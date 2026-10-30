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
	"slices"
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
	"codec":        "codec",
	"fmt":          "fmt",
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
	key string // canonical load key (file:<abs>, std:<name>, <parent>::<mod>)
	dir string // directory relative file imports resolve against
	// std marks a module embedded in the compiler; its own `use` declarations
	// name other standard-library modules, never files on disk.
	std    bool
	fileID int
	file   *ast.File

	imports []importBinding
	exports map[string]bool // top-level names, with whether each is `pub`

	// items is set for an inline `mod name { ... }` block; file modules use
	// file.Items instead. parent points at the enclosing module, and submods
	// maps an inline module's name to its own module.
	items   []ast.Item
	parent  *module
	submods map[string]*module

	// symbols holds this module's own top-level declarations. exportSyms is
	// the set reachable through a qualified path (public declarations plus
	// `pub use` re-exports); importSyms is the set an unqualified reference in
	// this module may resolve to (public items of plain/glob imports).
	symbols    map[string]*symbol
	exportSyms map[string]*symbol
	importSyms map[string]*symbol
	ambiguous  map[string][]string // unqualified names exported by >1 import

	// members are the file modules of a directory-package aggregate module.
	members []*module
	id      int

	// flattenPrivate marks modules whose private items keep their source
	// spelling. Only the files named directly on the command line need this:
	// they are one program, whereas `use`/`import`/`mod` modules isolate their
	// private names.
	flattenPrivate bool
}

// symbolKind classifies a module-level declaration. Methods and enum variants
// are not module symbols: they are reached through their type name.
type symbolKind int

const (
	symFn symbolKind = iota
	symStruct
	symEnum
	symConst
	symAlias
)

// symbol is one module-level declaration and the unique internal name it is
// lowered under when the flat namespace would otherwise collide or leak a
// private name across modules.
type symbol struct {
	name     string
	internal string
	mod      *module
	kind     symbolKind
	pub      bool
	decl     ast.Item
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

	entry      *module                  // the program module (first input path)
	useTargets map[*ast.UseDecl]*module // resolved target of each use/import
	nextModID  int
	nextBase   int // next free offset in the shared span space

	// overlay shadows on-disk contents for unsaved editor buffers, keyed by
	// cleaned absolute symlink-resolved paths (see normalizeOverlay).
	overlay map[string][]byte

	firstID int

	// loadingStd is set while a standard-library module (and its imports) are
	// being loaded.
	loadingStd bool
}

func newLoader(em *diag.Emitter) *loader {
	return &loader{
		em:         em,
		merged:     &ast.File{},
		mods:       map[string]*module{},
		useTargets: map[*ast.UseDecl]*module{},
	}
}

// loadDiskFile loads one file from disk and everything it imports, returning
// the file's id in the emitter's table.
func (l *loader) loadDiskFile(filePath string) int {
	m := l.loadDisk(filePath)
	if m == nil {
		return 0
	}
	if l.entry == nil {
		l.entry = m
	}
	// Files named directly on the command line are one program, exactly as
	// before module isolation.
	m.flattenPrivate = true
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
	if src, ok := l.overlay[abs]; ok {
		return l.loadModule(key, filePath, src, filepath.Dir(abs))
	}
	src, err := os.ReadFile(filePath)
	if err != nil {
		l.em.Report(diag.New("E9020", "cannot read %s: %v", filePath, err))
		return nil
	}
	return l.loadModule(key, filePath, src, filepath.Dir(abs))
}

// probeFile reports whether path names a loadable regular file: either it
// exists in the overlay (an unsaved editor buffer) or on disk. It is the
// overlay-aware version of the stat probes used to resolve imports.
func (l *loader) probeFile(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	if _, ok := l.overlay[abs]; ok {
		return true
	}
	if st, err := os.Stat(path); err == nil && !st.IsDir() {
		return true
	}
	return false
}

// normalizeOverlay cleans overlay keys the same way loadDisk cleans paths:
// absolute and, when the file resolves, symlink-resolved.
func normalizeOverlay(overlay map[string][]byte) map[string][]byte {
	if len(overlay) == 0 {
		return nil
	}
	out := make(map[string][]byte, len(overlay))
	for k, src := range overlay {
		abs, err := filepath.Abs(k)
		if err != nil {
			abs = filepath.Clean(k)
		}
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			abs = resolved
		}
		out[abs] = src
	}
	return out
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
	}
	display := "std/" + name + ".zx"
	src, err := fs.ReadFile(stdFS, display)
	if err != nil {
		l.em.Report(diag.New("E9022", "there is no standard library module named `%s`", name))
		return nil
	}
	// A standard-library module resolves its own imports inside the standard
	// library. Without this, `use http;` written in std/web.zx would pick up a
	// `lib/http.zx` sitting in the directory the compiler happened to be
	// started from, so the same program compiled or failed depending on the
	// working directory.
	l.loadingStd = true
	m := l.loadModule(key, display, src, "")
	l.loadingStd = false
	return m
}

// loadModule parses one already-read source into a module and follows its
// imports. The module is registered before imports are followed, so import
// cycles terminate: a file being loaded counts as loaded.
func (l *loader) loadModule(key, display string, src []byte, dir string) *module {
	if m, ok := l.mods[key]; ok {
		return m
	}
	m := &module{key: key, dir: dir, std: l.loadingStd}
	l.mods[key] = m
	// Each file gets a disjoint range of the shared span space, so any
	// diagnostic span identifies both its file and its position in it (see
	// diag.File.Base). The +1 leaves a gap so end-of-file spans are unique.
	base := l.nextBase
	l.nextBase += len(src) + 1
	m.fileID = l.em.AddFileAt(display, src, base)
	file, parseDiags := parser.Parse(m.fileID, base, src)
	l.em.Report(parseDiags...)
	if file == nil {
		file = &ast.File{}
	}
	m.file = file
	m.exports = computeExports(file.Items)
	l.order = append(l.order, m)
	l.collectImports(m, m, file.Items, nil)
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
// root is the file module whose flat namespace is being built; owner is the
// inline module that directly contains items, so imports declared inside an
// inline block bind in that block's scope.
func (l *loader) collectImports(root, owner *module, items []ast.Item, prefix []string) {
	for _, it := range items {
		switch d := it.(type) {
		case *ast.UseDecl:
			l.bindUse(owner, d)
		case *ast.ModDecl:
			if d.Inline {
				childPrefix := append(append([]string{}, prefix...), d.Name)
				childItems := d.Items
				if childItems == nil {
					childItems = []ast.Item{}
				}
				sub := &module{
					key:     root.key + "::" + strings.Join(childPrefix, "::"),
					dir:     owner.dir,
					file:    owner.file,
					items:   childItems,
					exports: computeExports(childItems),
				}
				sub.parent = owner
				l.mods[sub.key] = sub
				if owner.submods == nil {
					owner.submods = map[string]*module{}
				}
				owner.submods[d.Name] = sub
				l.bind(owner, []string{d.Name}, sub, d.Sp)
				if owner != root {
					l.bind(root, childPrefix, sub, d.Sp)
				}
				l.collectImports(root, sub, childItems, childPrefix)
				continue
			}
			l.bindMod(root, owner, d, prefix)
		}
	}
}

// bindMod resolves `mod name;` using the conventional `name.zx` and
// `name/mod.zx` layouts, and directory packages (`name/` with one or more .zx
// files), searching next to the declaring file and at the project root. It
// records the `name::` qualifier (respecting any enclosing inline-module
// prefix).
func (l *loader) bindMod(root, owner *module, d *ast.ModDecl, prefix []string) {
	qual := append(append([]string{}, prefix...), d.Name)
	bind := func(target *module) {
		if target == nil {
			return
		}
		l.bind(owner, []string{d.Name}, target, d.Sp)
		if owner != root {
			l.bind(root, qual, target, d.Sp)
		}
	}
	for _, dir := range l.searchDirs(owner.dir) {
		cand := filepath.Join(dir, d.Name+".zx")
		if l.probeFile(cand) {
			bind(l.loadDisk(cand))
			return
		}
	}
	for _, dir := range l.searchDirs(owner.dir) {
		pkgDir := filepath.Join(dir, d.Name)
		st, err := os.Stat(pkgDir)
		if err != nil || !st.IsDir() {
			continue
		}
		if modFile := filepath.Join(pkgDir, "mod.zx"); l.probeFile(modFile) {
			bind(l.loadDisk(modFile))
			return
		}
		bind(l.loadPackage(pkgDir))
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
		if target != nil {
			l.useTargets[d] = target
		}
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
	}

	target, parts, ok := l.resolveUse(m, d)
	if !ok {
		return
	}
	l.useTargets[d] = target
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
	dirs := l.searchDirs(m.dir)
	if m.std {
		dirs = nil // a standard-library module imports only other stdlib modules
	}
	for _, dir := range dirs {
		cand := filepath.Join(dir, name)
		if l.probeFile(cand) {
			if target := l.loadDisk(cand); target != nil {
				return target, parts, true
			}
			return nil, nil, false
		}
	}
	// A directory package: `use util;` loads every `.zx` file in `util/` (or
	// its `mod.zx` facade if present). This is the "many files, one import"
	// part of a convention-only project.
	for _, dir := range dirs {
		pkgDir := filepath.Join(dir, filepath.Join(parts...))
		st, err := os.Stat(pkgDir)
		if err != nil || !st.IsDir() {
			continue
		}
		if modFile := filepath.Join(pkgDir, "mod.zx"); l.probeFile(modFile) {
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
		if slices.Contains(dirs, d) {
			return
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
		pkg.members = append(pkg.members, member)
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
	l.buildSymbols()
	l.resolveSymbolImports()
	l.assignInternalNames()
	for _, m := range l.order {
		newRewriter(l, m).items(m.file.Items)
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

// rewriter resolves module-qualified paths and module-level names in one
// module. Local bindings are tracked so a parameter or `let` may shadow an
// item without being renamed.
type rewriter struct {
	l     *loader
	m     *module
	scope moduleScope

	scopes     []map[string]bool
	typeParams []map[string]bool
}

func newRewriter(l *loader, m *module) *rewriter {
	return &rewriter{l: l, m: m, scope: moduleScope{l: l, m: m}}
}

func (r *rewriter) items(items []ast.Item) {
	for _, it := range items {
		r.item(it)
	}
}

func (r *rewriter) item(it ast.Item) {
	switch d := it.(type) {
	case *ast.FnDecl:
		r.fn(d, false)
	case *ast.StructDecl:
		if sym := r.m.symbols[d.Name]; sym != nil {
			d.Name = sym.internal
		}
		r.pushTypeParams(genericNames(d.Generics))
		for i := range d.Fields {
			r.typ(d.Fields[i].Ty)
		}
		r.popTypeParams()
	case *ast.EnumDecl:
		if sym := r.m.symbols[d.Name]; sym != nil {
			d.Name = sym.internal
		}
		r.pushTypeParams(genericNames(d.Generics))
		for i := range d.Variants {
			for j := range d.Variants[i].Fields {
				r.typ(d.Variants[i].Fields[j].Ty)
			}
		}
		r.popTypeParams()
	case *ast.ConstDecl:
		if sym := r.m.symbols[d.Name]; sym != nil {
			d.Name = sym.internal
		}
		r.typ(d.Ty)
		if d.Val != nil {
			r.expr(d.Val)
		}
	case *ast.TypeAlias:
		if sym := r.m.symbols[d.Name]; sym != nil {
			d.Name = sym.internal
		}
		r.pushTypeParams(genericNames(d.Generics))
		r.typ(d.Ty)
		r.popTypeParams()
	case *ast.ImplBlock:
		r.pushTypeParams(genericNames(d.Generics))
		r.typ(d.SelfTy)
		if d.Trait != nil {
			r.typ(d.Trait)
		}
		for _, fn := range d.Fns {
			r.fn(fn, true)
		}
		r.popTypeParams()
	case *ast.ModDecl:
		if !d.Inline {
			return
		}
		if sub := r.m.submods[d.Name]; sub != nil {
			newRewriter(r.l, sub).items(d.Items)
		}
	}
}

// fn rewrites one function or method body. A method's name is not a module
// symbol; it is reached through its receiver type.
func (r *rewriter) fn(d *ast.FnDecl, method bool) {
	if !method {
		if sym := r.m.symbols[d.Name]; sym != nil {
			d.Name = sym.internal
		}
	}
	r.pushTypeParams(genericNames(d.Generics))
	r.pushScope()
	for i := range d.Params {
		p := &d.Params[i]
		if p.Ty != nil {
			r.typ(p.Ty)
		}
		if p.Default != nil {
			r.expr(p.Default)
		}
		if p.Self {
			r.bind("self")
		} else if p.Name != "" {
			r.bind(p.Name)
		}
	}
	r.typ(d.Ret)
	r.block(d.Body)
	r.popScope()
	r.popTypeParams()
}

func genericNames(gps []ast.GenericParam) []string {
	if len(gps) == 0 {
		return nil
	}
	out := make([]string, len(gps))
	for i, gp := range gps {
		out[i] = gp.Name
	}
	return out
}

func (r *rewriter) pushScope() {
	r.scopes = append(r.scopes, map[string]bool{})
}

func (r *rewriter) popScope() {
	r.scopes = r.scopes[:len(r.scopes)-1]
}

func (r *rewriter) bind(name string) {
	if name == "" || len(r.scopes) == 0 {
		return
	}
	r.scopes[len(r.scopes)-1][name] = true
}

func (r *rewriter) isLocal(name string) bool {
	for i := len(r.scopes) - 1; i >= 0; i-- {
		if r.scopes[i][name] {
			return true
		}
	}
	return false
}

func (r *rewriter) pushTypeParams(names []string) {
	r.typeParams = append(r.typeParams, map[string]bool{})
	if len(names) == 0 {
		return
	}
	top := r.typeParams[len(r.typeParams)-1]
	for _, name := range names {
		top[name] = true
	}
}

func (r *rewriter) popTypeParams() {
	r.typeParams = r.typeParams[:len(r.typeParams)-1]
}

func (r *rewriter) isTypeParam(name string) bool {
	for i := len(r.typeParams) - 1; i >= 0; i-- {
		if r.typeParams[i][name] {
			return true
		}
	}
	return false
}

func (r *rewriter) typ(t ast.Type) {
	switch x := t.(type) {
	case nil:
		return
	case *ast.PathType:
		if len(x.Segments) > 0 && !r.isTypeParam(x.Segments[0]) {
			if nx := r.scope.resolvePath(x.Segments, x); nx != nil {
				x.Segments = nx
			}
		}
		for _, a := range x.Args {
			r.typ(a)
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
		if x.Val != nil {
			r.expr(x.Val)
		}
		r.bindPattern(x.Pat)
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
	r.pushScope()
	for _, s := range b.Stmts {
		r.stmt(s)
	}
	if b.Tail != nil {
		r.expr(b.Tail)
	}
	r.popScope()
}

func (r *rewriter) expr(e ast.Expr) {
	switch x := e.(type) {
	case nil:
		return
	case *ast.Block:
		r.block(x)
	case *ast.IdentExpr:
		for _, a := range x.TypeArgs {
			r.typ(a)
		}
		if !r.isLocal(x.Name) {
			if internal, ok := r.scope.resolveIdent(x.Name, x); ok {
				x.Name = internal
			}
		}
	case *ast.LiteralExpr:
		// No names.
	case *ast.PathExpr:
		for _, a := range x.TypeArgs {
			r.typ(a)
		}
		if nx := r.scope.resolvePath(x.Segments, x); nx != nil {
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
			r.pushScope()
			r.pat(x.Arms[i].Pat)
			r.bindPattern(x.Arms[i].Pat)
			if x.Arms[i].Guard != nil {
				r.expr(x.Arms[i].Guard)
			}
			r.expr(x.Arms[i].Body)
			r.popScope()
		}
	case *ast.LoopExpr:
		if x.Cond != nil {
			r.expr(x.Cond)
		}
		if x.Iter != nil {
			r.expr(x.Iter)
		}
		r.pushScope()
		r.pat(x.Pat)
		r.bindPattern(x.Pat)
		r.block(x.Body)
		r.popScope()
	case *ast.JumpExpr:
		if x.Val != nil {
			r.expr(x.Val)
		}
	case *ast.StructLitExpr:
		for _, a := range x.TypeArgs {
			r.typ(a)
		}
		if nx := r.scope.resolvePath(x.Path, x); nx != nil {
			x.Path = nx
		}
		for i := range x.Fields {
			r.expr(x.Fields[i].Val)
		}
	case *ast.LambdaExpr:
		// A closure body names module items like any other expression, with
		// its own parameters shadowing outer bindings.
		r.pushScope()
		for i := range x.Params {
			p := &x.Params[i]
			if p.Ty != nil {
				r.typ(p.Ty)
			}
			if p.Default != nil {
				r.expr(p.Default)
			}
			if p.Name != "" {
				r.bind(p.Name)
			}
		}
		r.block2(x.Body)
		r.popScope()
	}
}

// block2 rewrites a body that is either a block or a single expression.
func (r *rewriter) block2(body ast.Expr) {
	if b, ok := body.(*ast.Block); ok {
		r.block(b)
		return
	}
	r.expr(body)
}

// pat rewrites the module-qualified paths inside a pattern. Binding names are
// handled by bindPattern.
func (r *rewriter) pat(p ast.Pattern) {
	switch x := p.(type) {
	case nil, *ast.BindPat, *ast.WildcardPat, *ast.LitPat, *ast.RangePat:
		return
	case *ast.TuplePat:
		for _, el := range x.Elems {
			r.pat(el)
		}
	case *ast.PathPat:
		if nx := r.scope.resolvePath(x.Path, x); nx != nil {
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
	}
}

// bindPattern introduces every name a pattern binds into the current scope.
func (r *rewriter) bindPattern(p ast.Pattern) {
	switch x := p.(type) {
	case nil, *ast.WildcardPat, *ast.LitPat, *ast.RangePat:
		return
	case *ast.BindPat:
		r.bind(x.Name)
	case *ast.TuplePat:
		for _, el := range x.Elems {
			r.bindPattern(el)
		}
	case *ast.PathPat:
		for _, s := range x.Sub {
			r.bindPattern(s)
		}
		for i := range x.Fields {
			r.bindPattern(x.Fields[i].Pat)
		}
	case *ast.OrPat:
		for _, alt := range x.Alts {
			r.bindPattern(alt)
		}
	}
}
