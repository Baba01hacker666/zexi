package driver

// This file implements the module layer that sits between parsing and semantic
// analysis. Every loaded source file (or inline `mod { ... }` block) becomes a
// module with a private export table. `use` / `import` declarations record what
// names that module is reachable under in the importing file, and a rewrite
// pass resolves path-qualified references (`alias::item`, `pkg::Type::new`,
// `mod::Enum::Variant`) before the flattened program reaches sema/codegen.
//
// The historical behavior is still honored: plain imports also expose their
// items unqualified, because the M0/M1-preview front end checked one flat
// namespace. What is new is that qualified access is real — it is checked
// against the target module's exports and honors `pub`.

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"zexi/internal/ast"
	"zexi/internal/diag"
	"zexi/internal/parser"
	"zexi/internal/token"
)

// stdModuleAliases gives the embedded library a package-shaped facade. The
// language resolves qualified paths through per-file import bindings, but these
// names make imports read naturally: `import http.server;`, `use io::fs;`, and
// `use std::http::client;` all select their backing module.
var stdModuleAliases = map[string]string{
	"fs":           "fs",
	"http":         "http",
	"http::client": "http",
	"http::files":  "http",
	"http::server": "http",
	"http::static": "http",
	"io":           "io",
	"io::console":  "io",
	"io::file":     "fs",
	"io::fs":       "fs",
	"math":         "math",
	"math::basic":  "math",
	"math::stats":  "math",
	"net":          "net",
	"net::tcp":     "net",
	"os":           "sys",
	"random":       "random",
	"random::rng":  "random",
	"regex":        "regex",
	"sys":          "sys",
	"sys::env":     "sys",
	"sys::time":    "sys",
	"text":         "text",
	"text::ascii":  "text",
	"text::regex":  "regex",
	"thread":       "thread",
	"thread::sync": "thread",
	"thread::task": "thread",
	"time":         "sys",
}

// stdModule resolves a public module path to an embedded source file. Keeping
// this table in the driver means aliases cost no runtime code and normal
// filesystem imports still take precedence over the standard-library facade.
func stdModule(parts []string) (string, bool) {
	if name, ok := stdModuleAliases[strings.Join(parts, "::")]; ok {
		return name, true
	}
	if len(parts) == 1 {
		name := parts[0]
		if _, err := fs.Stat(stdFS, path.Join("std", name+".zx")); err == nil {
			return name, true
		}
	}
	return "", false
}

// module is one source file or inline `mod { ... }` block: the unit of export
// tracking. Items are still flattened into the program for sema, but a module
// knows which of its names other modules may reach and under what paths.
type module struct {
	key    string // canonical load key (file:<abs>, std:<name>, <parent>::<mod>)
	dir    string // directory relative file imports resolve against
	fileID int
	file   *ast.File

	imports []importBinding
	exports map[string]bool // top-level names reachable through a qualified path
}

// importBinding maps a path prefix used in source to the module it names. A
// binding with prefix ["m"] makes `m::foo` resolve to module target; a
// multi-segment prefix (["http","server"]) supports dotted import paths.
type importBinding struct {
	prefix []string
	target *module
}

// loader reads source files (entry points plus everything they use/import/mod),
// parses them into modules, resolves qualified paths, and merges their items
// into the single program fed to sema.
type loader struct {
	em     *diag.Emitter
	merged *ast.File

	// root is the project root named imports and `mod` search from, after the
	// importing file's own directory (see ProjectRoot).
	root string

	mods  map[string]*module // load key -> module
	order []*module          // load order, for deterministic merging

	firstID int

	usesNet    bool // program imports net/http/fs (needs the shared C runtime)
	usesSys    bool // program imports sys (needs the C shim)
	usesRegex  bool // program imports regex (needs the POSIX ERE shim)
	usesThread bool // program imports thread (needs the pthread shim and -pthread)
}

func newLoader(em *diag.Emitter) *loader {
	return &loader{em: em, merged: &ast.File{}, mods: map[string]*module{}}
}

// loadDiskFile loads one file from disk and everything it imports, returning
// the file's id in the emitter's table.
func (l *loader) loadDiskFile(filePath string) int {
	m := l.loadDisk(filePath)
	if m == nil {
		return 0
	}
	return m.fileID
}

// loadDisk loads one on-disk file as a module (or returns the already-loaded
// one). Symbols are resolved so that two imports reaching the same file through
// a real path and a symlink share one module.
func (l *loader) loadDisk(filePath string) *module {
	abs, err := filepath.Abs(filePath)
	if err != nil {
		abs = filepath.Clean(filePath)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	key := "file:" + abs
	if m, ok := l.mods[key]; ok {
		return m
	}
	src, err := os.ReadFile(filePath)
	if err != nil {
		l.em.Report(diag.New("E9020", "cannot read %s: %v", filePath, err))
		return nil
	}
	return l.loadModule(key, filePath, src, filepath.Dir(abs))
}

// loadStdFile loads one embedded standard-library file by base name (for
// example "math" for std/math.zx).
func (l *loader) loadStdFile(name string) *module {
	key := "std:" + name
	if m, ok := l.mods[key]; ok {
		return m
	}
	// The socket/file C shim is intentionally shared. `fs` only uses its file
	// functions, but linking it is still opt-in and dead-code elimination keeps
	// unused socket helpers out of the binary.
	switch name {
	case "net", "http", "fs":
		l.usesNet = true
	case "sys":
		l.usesSys = true
	case "regex":
		l.usesRegex = true
	case "thread":
		l.usesThread = true
	}
	display := "std/" + name + ".zx"
	src, err := fs.ReadFile(stdFS, display)
	if err != nil {
		l.em.Report(diag.New("E9022", "there is no standard library module named `%s`", name))
		return nil
	}
	return l.loadModule(key, display, src, "")
}

// loadModule parses one already-read source into a module and follows its
// imports. The module is registered before imports are followed, so import
// cycles terminate: a file being loaded counts as loaded.
func (l *loader) loadModule(key, display string, src []byte, dir string) *module {
	if m, ok := l.mods[key]; ok {
		return m
	}
	m := &module{key: key, dir: dir}
	l.mods[key] = m
	m.fileID = l.em.AddFile(display, src)
	file, parseDiags := parser.Parse(m.fileID, src)
	l.em.Report(parseDiags...)
	if file == nil {
		file = &ast.File{}
	}
	m.file = file
	m.exports = computeExports(file.Items)
	l.order = append(l.order, m)
	l.collectImports(m, file.Items, nil)
	return m
}

// computeExports builds a module's public name table from its top-level items.
// Only `pub` declarations are reachable through a qualified path.
func computeExports(items []ast.Item) map[string]bool {
	out := map[string]bool{}
	add := func(name string, pub bool) {
		if name == "" {
			return
		}
		out[name] = out[name] || pub
	}
	for _, it := range items {
		switch d := it.(type) {
		case *ast.FnDecl:
			add(d.Name, bool(d.Vis))
		case *ast.StructDecl:
			add(d.Name, bool(d.Vis))
		case *ast.EnumDecl:
			add(d.Name, bool(d.Vis))
		case *ast.ConstDecl:
			add(d.Name, bool(d.Vis))
		case *ast.TypeAlias:
			add(d.Name, bool(d.Vis))
		}
	}
	return out
}

// collectImports walks a module's items, loading what its `use`/`import`/`mod`
// declarations name and recording the bindings that make qualified access work.
// Inline `mod { ... }` blocks become synthetic modules so `mod::item` is
// checked against their exports too; prefix carries the enclosing inline-module
// path so nested `mod outer { mod inner { .. } }` binds `outer::inner::item`.
func (l *loader) collectImports(m *module, items []ast.Item, prefix []string) {
	for _, it := range items {
		switch d := it.(type) {
		case *ast.UseDecl:
			l.bindUse(m, d)
		case *ast.ModDecl:
			if d.Inline {
				childPrefix := append(append([]string{}, prefix...), d.Name)
				sub := &module{
					key:     m.key + "::" + strings.Join(childPrefix, "::"),
					dir:     m.dir,
					file:    m.file,
					exports: computeExports(d.Items),
				}
				l.mods[sub.key] = sub
				m.imports = append(m.imports, importBinding{prefix: childPrefix, target: sub})
				l.collectImports(m, d.Items, childPrefix)
				continue
			}
			l.bindMod(m, d, prefix)
		}
	}
}

// bindMod resolves `mod name;` using the conventional `name.zx` and
// `name/mod.zx` layouts, and directory packages (`name/` with one or more .zx
// files), searching next to the declaring file and at the project root. It
// records the `name::` qualifier (respecting any enclosing inline-module
// prefix).
func (l *loader) bindMod(m *module, d *ast.ModDecl, prefix []string) {
	qual := append(append([]string{}, prefix...), d.Name)
	for _, dir := range l.searchDirs(m.dir) {
		cand := filepath.Join(dir, d.Name+".zx")
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			if target := l.loadDisk(cand); target != nil {
				l.bind(m, qual, target, d.Sp)
			}
			return
		}
	}
	for _, dir := range l.searchDirs(m.dir) {
		pkgDir := filepath.Join(dir, d.Name)
		st, err := os.Stat(pkgDir)
		if err != nil || !st.IsDir() {
			continue
		}
		if modFile := filepath.Join(pkgDir, "mod.zx"); fileExists(modFile) {
			if target := l.loadDisk(modFile); target != nil {
				l.bind(m, qual, target, d.Sp)
			}
			return
		}
		if pkg := l.loadPackage(pkgDir); pkg != nil {
			l.bind(m, qual, pkg, d.Sp)
		}
		return
	}
	l.em.Report(diag.New("E9023", "cannot find module `%s` (looked for %s.zx and %s/mod.zx)", d.Name, d.Name, d.Name).WithSpan(d.Sp))
}

// bindUse resolves and loads one `use` / `import` declaration and records the
// path prefixes under which the target module's items may be reached.
func (l *loader) bindUse(m *module, d *ast.UseDecl) {
	if d.File != "" {
		// `use "helpers.zx";` / `import "helpers.zx";` — relative to the
		// importing file.
		target := l.loadDisk(filepath.Join(m.dir, filepath.FromSlash(d.File)))
		qual := d.As
		if qual == "" {
			base := filepath.Base(d.File)
			qual = strings.TrimSuffix(base, filepath.Ext(base))
		}
		if target != nil && qual != "" {
			l.bind(m, []string{qual}, target, d.Sp)
		}
		return
	}
	if len(d.Path) == 0 {
		return
	}

	// Preserve the historical convenience for local modules named net/http/sys
	// (and regex): their ABI calls can use the embedded runtime too.
	switch d.Path[len(d.Path)-1] {
	case "net", "http", "fs":
		l.usesNet = true
	case "sys":
		l.usesSys = true
	case "regex":
		l.usesRegex = true
	case "thread":
		l.usesThread = true
	}

	target, parts, ok := l.resolveUse(m, d)
	if !ok {
		return
	}
	if d.As != "" {
		l.bind(m, []string{d.As}, target, d.Sp)
		return
	}
	// Bind the full path (`http::server::Router`) and its last segment
	// (`server::Router`) so both dotted Python-style imports and `::` paths
	// read naturally. The leading package is deliberately not bound: two
	// facades can share it (text.ascii and text.regex), which would make the
	// qualifier ambiguous.
	l.bind(m, parts, target, d.Sp)
	if len(parts) > 0 {
		l.bind(m, parts[len(parts)-1:], target, d.Sp)
	}
}

// resolveUse maps a `use` path to a module, preferring local files over the
// embedded standard library. It returns the path segments used for binding.
func (l *loader) resolveUse(m *module, d *ast.UseDecl) (*module, []string, bool) {
	parts := d.Path
	if parts[0] == "std" {
		if len(parts) < 2 {
			l.em.Report(diag.New("E9022", "`use std;` needs a module name, such as `use std::math;`").WithSpan(d.Sp))
			return nil, nil, false
		}
		parts = parts[1:]
		if module, ok := stdModule(parts); ok {
			if target := l.loadStdFile(module); target != nil {
				return target, parts, true
			}
			return nil, nil, false
		}
		l.em.Report(diag.New("E9022", "there is no standard library module named `%s`", strings.Join(parts, "::")).WithSpan(d.Sp))
		return nil, nil, false
	}

	// `use a::b;` / `import a.b;` names a/b.zx: local modules win over
	// embedded ones so applications can provide their own package trees.
	name := filepath.Join(parts...) + ".zx"
	for _, dir := range l.searchDirs(m.dir) {
		cand := filepath.Join(dir, name)
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			if target := l.loadDisk(cand); target != nil {
				return target, parts, true
			}
			return nil, nil, false
		}
	}
	// A directory package: `use util;` loads every `.zx` file in `util/` (or
	// its `mod.zx` facade if present). This is the "many files, one import"
	// part of a convention-only project.
	for _, dir := range l.searchDirs(m.dir) {
		pkgDir := filepath.Join(dir, filepath.Join(parts...))
		st, err := os.Stat(pkgDir)
		if err != nil || !st.IsDir() {
			continue
		}
		if modFile := filepath.Join(pkgDir, "mod.zx"); fileExists(modFile) {
			target := l.loadDisk(modFile)
			return target, parts, target != nil
		}
		if pkg := l.loadPackage(pkgDir); pkg != nil {
			return pkg, parts, true
		}
		return nil, nil, false
	}
	if module, ok := stdModule(parts); ok {
		if target := l.loadStdFile(module); target != nil {
			return target, parts, true
		}
		return nil, nil, false
	}
	l.em.Report(diag.New("E9022", "cannot find a library named `%s` (looked for %s next to the importing file, in its lib/ folder, at the project root, and in the standard library)", strings.Join(parts, "::"), name).WithSpan(d.Sp))
	return nil, nil, false
}

// searchDirs returns the directories a named module is looked up in, most
// specific first: the importing file's directory, its `lib/` folder, the
// project root, and the root's `lib/` folder. Duplicates are collapsed, which
// matters in the common flat layout where the importer is the root.
func (l *loader) searchDirs(importerDir string) []string {
	var dirs []string
	add := func(d string) {
		if d == "" {
			return
		}
		for _, x := range dirs {
			if x == d {
				return
			}
		}
		dirs = append(dirs, d)
	}
	add(importerDir)
	add(filepath.Join(importerDir, "lib"))
	if l.root != "" && l.root != importerDir {
		add(l.root)
		add(filepath.Join(l.root, "lib"))
	}
	return dirs
}

// loadPackage loads every `.zx` file in a directory as one aggregate module:
// the files each keep their own imports and are merged into the program, while
// qualified access through the package qualifier is checked against the union
// of their exports. `main.zx` and `_`-prefixed files are skipped, so a package
// cannot pull in a second entry point or private scratch source.
func (l *loader) loadPackage(dir string) *module {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = filepath.Clean(dir)
	}
	key := "pkg:" + abs
	if m, ok := l.mods[key]; ok {
		return m
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		l.em.Report(diag.New("E9022", "cannot read package directory %s: %v", dir, err))
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasSuffix(n, ".zx") || n == "main.zx" || strings.HasPrefix(n, "_") {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) == 0 {
		l.em.Report(diag.New("E9022", "package directory %s has no .zx files", dir))
		return nil
	}
	pkg := &module{key: key, dir: dir, exports: map[string]bool{}}
	l.mods[key] = pkg // register before loading members, so cycles terminate
	for _, n := range names {
		member := l.loadDisk(filepath.Join(dir, n))
		if member == nil {
			continue
		}
		for name, pub := range member.exports {
			pkg.exports[name] = pkg.exports[name] || pub
		}
	}
	return pkg
}

// fileExists reports whether path is an existing regular file.
func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// bind records one import binding, skipping exact duplicates and reporting a
// conflict when the same qualifier names two different modules.
func (l *loader) bind(m *module, prefix []string, target *module, sp token.Span) {
	if len(prefix) == 0 || target == nil {
		return
	}
	for _, b := range m.imports {
		if !equalPath(b.prefix, prefix) {
			continue
		}
		if b.target == target {
			return
		}
		l.em.Report(diag.New("E0255", "the name `%s` is imported from two different modules", strings.Join(prefix, "::")).WithSpan(sp))
		return
	}
	m.imports = append(m.imports, importBinding{prefix: prefix, target: target})
}

func equalPath(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// finish rewrites qualified paths in every module and merges the resulting
// items into the one program sema checks.
func (l *loader) finish() {
	for _, m := range l.order {
		r := &rewriter{l: l, m: m}
		r.items(m.file.Items)
	}
	for _, m := range l.order {
		l.appendItems(m.file.Items)
	}
}

// appendItems copies a module's items into the merged program, flattening
// inline modules and dropping the directives sema never sees.
func (l *loader) appendItems(items []ast.Item) {
	for _, it := range items {
		switch d := it.(type) {
		case *ast.UseDecl:
			// Resolved by the loader.
		case *ast.ModDecl:
			if d.Inline {
				l.appendItems(d.Items)
			}
		default:
			l.merged.Items = append(l.merged.Items, it)
		}
	}
}

// --- qualified-path rewriting ---------------------------------------------

// rewriter resolves module-qualified paths in one module. It leaves unqualified
// names alone (sema still resolves those in the flat program), but rewrites
// `prefix::rest` to `rest` once prefix is verified to name an imported module
// that exports `rest`.
type rewriter struct {
	l *loader
	m *module
}

func (r *rewriter) items(items []ast.Item) {
	for _, it := range items {
		r.item(it)
	}
}

func (r *rewriter) item(it ast.Item) {
	switch d := it.(type) {
	case *ast.FnDecl:
		for i := range d.Params {
			r.typ(d.Params[i].Ty)
			if d.Params[i].Default != nil {
				r.expr(d.Params[i].Default)
			}
		}
		r.typ(d.Ret)
		if d.Body != nil {
			r.block(d.Body)
		}
	case *ast.StructDecl:
		for i := range d.Fields {
			r.typ(d.Fields[i].Ty)
		}
	case *ast.EnumDecl:
		for i := range d.Variants {
			for j := range d.Variants[i].Fields {
				r.typ(d.Variants[i].Fields[j].Ty)
			}
		}
	case *ast.ConstDecl:
		r.typ(d.Ty)
		if d.Val != nil {
			r.expr(d.Val)
		}
	case *ast.TypeAlias:
		r.typ(d.Ty)
	case *ast.ImplBlock:
		r.typ(d.SelfTy)
		for _, fn := range d.Fns {
			r.item(fn)
		}
	case *ast.ModDecl:
		if d.Inline {
			r.items(d.Items)
		}
	}
}

func (r *rewriter) typ(t ast.Type) {
	switch x := t.(type) {
	case nil:
		return
	case *ast.PathType:
		nx := r.path(x.Segments, x.Sp)
		if nx != nil {
			x.Segments = nx
		}
	case *ast.RefType:
		r.typ(x.Elem)
	case *ast.PtrType:
		r.typ(x.Elem)
	case *ast.ArrayType:
		r.typ(x.Elem)
		if x.Len != nil {
			r.expr(x.Len)
		}
	case *ast.SliceType:
		r.typ(x.Elem)
	case *ast.TupleType:
		for _, e := range x.Elems {
			r.typ(e)
		}
	case *ast.FnType:
		for _, p := range x.Params {
			r.typ(p)
		}
		r.typ(x.Ret)
	}
}

func (r *rewriter) stmt(s ast.Stmt) {
	switch x := s.(type) {
	case *ast.LetStmt:
		r.typ(x.Ty)
		r.pat(x.Pat)
		if x.Val != nil {
			r.expr(x.Val)
		}
	case *ast.ExprStmt:
		r.expr(x.X)
	case *ast.DeferStmt:
		r.expr(x.X)
	}
}

func (r *rewriter) block(b *ast.Block) {
	if b == nil {
		return
	}
	for _, s := range b.Stmts {
		r.stmt(s)
	}
	if b.Tail != nil {
		r.expr(b.Tail)
	}
}

func (r *rewriter) expr(e ast.Expr) {
	switch x := e.(type) {
	case nil:
		return
	case *ast.Block:
		r.block(x)
	case *ast.IdentExpr, *ast.LiteralExpr:
		// Bare names and literals carry no module qualification.
	case *ast.PathExpr:
		if nx := r.path(x.Segments, x.Sp); nx != nil {
			x.Segments = nx
		}
	case *ast.BinExpr:
		r.expr(x.L)
		r.expr(x.R)
	case *ast.UnExpr:
		r.expr(x.X)
	case *ast.AssignExpr:
		r.expr(x.Lhs)
		r.expr(x.Rhs)
	case *ast.CastExpr:
		r.expr(x.X)
		r.typ(x.Ty)
	case *ast.TupleExpr:
		for _, el := range x.Elems {
			r.expr(el)
		}
	case *ast.ArrayLitExpr:
		if x.Repeat {
			r.expr(x.Value)
			r.expr(x.Count)
			return
		}
		for _, el := range x.Elems {
			r.expr(el)
		}
	case *ast.NamedArgExpr:
		r.expr(x.Val)
	case *ast.InterpStrExpr:
		for _, p := range x.Parts {
			r.expr(p)
		}
	case *ast.CallExpr:
		r.expr(x.Callee)
		for _, a := range x.Args {
			r.expr(a)
		}
	case *ast.MethodCallExpr:
		r.expr(x.Recv)
		for _, a := range x.Args {
			r.expr(a)
		}
	case *ast.FieldExpr:
		r.expr(x.Recv)
	case *ast.IndexExpr:
		r.expr(x.Recv)
		r.expr(x.Index)
	case *ast.SliceExpr:
		r.expr(x.Recv)
		r.expr(x.Low)
		r.expr(x.High)
	case *ast.TryExpr:
		r.expr(x.X)
	case *ast.IfExpr:
		r.expr(x.Cond)
		r.block(x.Then)
		r.expr(x.Else)
	case *ast.MatchExpr:
		r.expr(x.Scrutinee)
		for i := range x.Arms {
			r.pat(x.Arms[i].Pat)
			if x.Arms[i].Guard != nil {
				r.expr(x.Arms[i].Guard)
			}
			r.expr(x.Arms[i].Body)
		}
	case *ast.LoopExpr:
		if x.Cond != nil {
			r.expr(x.Cond)
		}
		r.pat(x.Pat)
		if x.Iter != nil {
			r.expr(x.Iter)
		}
		r.block(x.Body)
	case *ast.JumpExpr:
		if x.Val != nil {
			r.expr(x.Val)
		}
	case *ast.StructLitExpr:
		if nx := r.path(x.Path, x.Sp); nx != nil {
			x.Path = nx
		}
		for i := range x.Fields {
			r.expr(x.Fields[i].Val)
		}
	}
}

func (r *rewriter) pat(p ast.Pattern) {
	switch x := p.(type) {
	case nil:
		return
	case *ast.BindPat:
	case *ast.WildcardPat:
	case *ast.LitPat:
	case *ast.TuplePat:
		for _, el := range x.Elems {
			r.pat(el)
		}
	case *ast.PathPat:
		if nx := r.path(x.Path, x.Sp); nx != nil {
			x.Path = nx
		}
		for _, s := range x.Sub {
			r.pat(s)
		}
		for i := range x.Fields {
			r.pat(x.Fields[i].Pat)
		}
	case *ast.OrPat:
		for _, alt := range x.Alts {
			r.pat(alt)
		}
	case *ast.RangePat:
	}
}

// path resolves a possibly module-qualified path. It returns nil when the path
// was already rewritten in place or is unqualified; callers only assign a
// non-nil result. It reports a diagnostic when a path names a module but not an
// exported item.
func (r *rewriter) path(segs []string, sp token.Span) []string {
	if len(segs) == 0 {
		return nil
	}
	var bind *importBinding
	best := 0
	for i := range r.m.imports {
		b := &r.m.imports[i]
		if len(b.prefix) > best && len(segs) >= len(b.prefix) && equalPath(segs[:len(b.prefix)], b.prefix) {
			best = len(b.prefix)
			bind = b
		}
	}
	if bind == nil {
		return nil // not module-qualified: an enum path or a local name
	}
	rest := segs[best:]
	modName := strings.Join(bind.prefix, "::")
	if len(rest) == 0 {
		r.l.em.Report(diag.New("E0433", "`%s` is a module and cannot be used as a value or type", modName).WithSpan(sp))
		return nil
	}
	pub, ok := bind.target.exports[rest[0]]
	if !ok {
		r.l.em.Report(diag.New("E0412", "module `%s` has no item named `%s`", modName, rest[0]).WithSpan(sp))
		return nil
	}
	if !pub {
		r.l.em.Report(diag.New("E0603", "item `%s::%s` is private", modName, rest[0]).WithSpan(sp))
		return nil
	}
	return rest
}
