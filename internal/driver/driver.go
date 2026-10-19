// Package driver orchestrates the Zexi pipeline: source -> tokens -> AST ->
// typed program -> C -> native binary. It owns process-level concerns
// (temporary directories, invoking the C compiler) so that the pure stages
// stay pure and testable.
package driver

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"zexi/internal/ast"
	zenc "zexi/internal/codegen/c"
	"zexi/internal/diag"
	"zexi/internal/lexer"
	"zexi/internal/parser"
	"zexi/internal/sema"
	"zexi/internal/token"
)

// Options controls a compilation.
type Options struct {
	// OptLevel is passed to the C compiler: "0", "1", "2", "3", or "s".
	OptLevel string
	// CC is the C compiler to invoke. Empty means $CC or "cc".
	CC string
	// CFiles are extra C source files compiled and linked together with
	// the generated code — the implementation side of the C bridge (your
	// own glue code, or a C library's sources).
	CFiles []string
	// LinkLibs are libraries to link, as `-l<name>` (for example "m").
	LinkLibs []string
	// LibDirs are extra library search directories, as `-L<dir>`.
	LibDirs []string
	// IncludeDirs are extra header search directories, as `-I<dir>`,
	// used when compiling CFiles.
	IncludeDirs []string
	// Jobs is how many translation units to compile in parallel (0 picks
	// automatically; 1 forces the single-file path).
	Jobs int
}

// Artifacts is the result of a successful front end + codegen run.
type Artifacts struct {
	Info *sema.Info
	CSrc string
	// FileID is the id of the source file in the emitter's table.
	FileID int
	// CExtra holds additional C source contents (as strings) to compile
	// and link into the binary, such as the networking runtime.
	CExtra []string
	// CHeader and CUnits hold the program split into several translation
	// units (a shared header plus function bodies) when compiling in
	// parallel; when empty, CSrc is the whole program.
	CHeader string
	CUnits  []string
}

// FrontEnd runs lexing, parsing, type checking, and C generation. It returns
// the artifacts and whether the program is error-free. Diagnostics are
// reported to em; callers render them.
func FrontEnd(path string, opts Options, em *diag.Emitter) (*Artifacts, bool) {
	return FrontEndFiles([]string{path}, opts, em)
}

// FrontEndFiles is FrontEnd for a program split across several source files.
// The files are parsed independently (each keeps its own identity for
// diagnostics) and their items are combined into one module. On top of that,
// `use` / `import` declarations import library files: `use "helpers.zx";`
// or `import "helpers.zx";` loads a relative file, while `use math;`,
// `import http.server;`, and `use std::math;` load a library by name — searched
// next to the importing file, in a `lib/` folder beside it, and finally in the
// standard library embedded in the compiler. `mod name;` loads `name.zx` or
// `name/mod.zx`. Imports are transitive, every file is loaded at most once, and
// cycles are harmless (a file being loaded counts as loaded). The first path is
// treated as the program.
func FrontEndFiles(paths []string, opts Options, em *diag.Emitter) (*Artifacts, bool) {
	if len(paths) == 0 {
		em.Report(diag.New("E9021", "no input files"))
		return nil, false
	}
	l := &loader{em: em, merged: &ast.File{}, loaded: map[string]bool{}}
	firstID := 0
	for i, path := range paths {
		id := l.loadDiskFile(path)
		if i == 0 {
			firstID = id
		}
	}
	if em.HasErrors() {
		return nil, false
	}
	merged := l.merged

	info, semaDiags := sema.Check(merged)
	em.Report(semaDiags...)
	if em.HasErrors() {
		return nil, false
	}

	art := &Artifacts{Info: info, FileID: firstID}
	if jobs := splitJobs(opts, len(info.FnOrder)); jobs > 1 {
		header, units, cerr := zenc.GenerateSplit(info, zenc.Options{}, jobs)
		if cerr != nil {
			em.Report(diag.New("E9001", "code generation failed: %v", cerr))
			return nil, false
		}
		if len(units) > 1 {
			art.CHeader, art.CUnits = header, units
		} else if len(units) == 1 {
			art.CSrc = header + units[0]
		} else {
			art.CSrc = header
		}
	} else {
		csrc, cerr := zenc.Generate(info, zenc.Options{})
		if cerr != nil {
			em.Report(diag.New("E9001", "code generation failed: %v", cerr))
			return nil, false
		}
		art.CSrc = csrc
	}
	if l.usesNet {
		art.CExtra = append(art.CExtra, string(netCSource))
	}
	if l.usesSys {
		art.CExtra = append(art.CExtra, string(sysCSource))
	}
	if l.usesRegex {
		art.CExtra = append(art.CExtra, string(regexCSource))
	}
	return art, true
}

// splitJobs decides how many translation units to compile in parallel: one
// for small programs (a second cc invocation would cost more than it saves),
// otherwise a capped number of CPUs.
func splitJobs(opts Options, fns int) int {
	if opts.Jobs == 1 {
		return 1
	}
	// Size-focused builds keep the single-file path: splitting duplicates
	// small runtime helpers across units, which costs a few KB of binary.
	if opts.OptLevel == "s" || opts.OptLevel == "z" {
		if opts.Jobs <= 0 {
			return 1
		}
	}
	n := opts.Jobs
	if n <= 0 {
		if fns < 64 {
			return 1
		}
		n = runtime.NumCPU()
		if n > 8 {
			n = 8
		}
	}
	if n < 1 {
		n = 1
	}
	return n
}

// stdFS holds the standard library: small Zexi source files embedded in
// the compiler, importable with `use math;`, `use std::math;`,
// `import http.server;`, and the other facade paths — no installation needed.
//
//go:embed std/*.zx
var stdFS embed.FS

// netCSource is the shared socket/file C runtime compiled into programs that
// select embedded net/http/fs helpers. Embedded so it needs no installation and
// stays in sync with the Zexi bindings.
//
//go:embed zxnet_c.txt
var netCSource []byte

// sysCSource is the C shim for the `sys` module (environment, local paths,
// clocks, and process helpers), embedded like the networking runtime.
//
//go:embed zxsys_c.txt
var sysCSource []byte

// regexCSource is the opt-in POSIX ERE bridge for the `regex` module.
//
//go:embed zxregex_c.txt
var regexCSource []byte

// loader reads source files (entry points plus everything they use/import/mod),
// parses them, and merges their items into one program.
type loader struct {
	em        *diag.Emitter
	merged    *ast.File
	loaded    map[string]bool // canonical file keys already loaded
	usesNet   bool            // program imports net/http/fs (needs the shared C runtime)
	usesSys   bool            // program imports sys (needs the C shim)
	usesRegex bool            // program imports regex (needs the POSIX ERE shim)
}

// loadDiskFile loads one file from disk and everything it imports.
func (l *loader) loadDiskFile(path string) int {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	// Resolve symbolic links so two imports that reach the same file through
	// a real path and a symlink share one key (and one load).
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	key := "file:" + abs
	if l.loaded[key] {
		return 0
	}
	src, err := os.ReadFile(path)
	if err != nil {
		l.em.Report(diag.New("E9020", "cannot read %s: %v", path, err))
		return 0
	}
	return l.loadSource(key, path, src, filepath.Dir(abs))
}

// stdModuleAliases gives the flat, embedded library a small package-shaped
// facade. The language currently merges imported declarations into one program
// namespace, but these paths make imports read naturally while real scoped
// module names are built out: `import http.server;`, `use io::fs;`, and
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

// loadStdFile loads one embedded standard-library file by base name
// (for example "math" for std/math.zx).
func (l *loader) loadStdFile(name string) int {
	key := "std:" + name
	if l.loaded[key] {
		return 0
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
	}
	display := "std/" + name + ".zx"
	src, err := fs.ReadFile(stdFS, display)
	if err != nil {
		l.em.Report(diag.New("E9022", "there is no standard library module named `%s`", name))
		return 0
	}
	return l.loadSource(key, display, src, "")
}

// loadSource parses one already-read file, records ordinary declarations, and
// follows import/module directives. dir is the directory relative imports
// resolve against (empty for embedded files).
func (l *loader) loadSource(key, display string, src []byte, dir string) int {
	l.loaded[key] = true // mark first: import cycles load each file once
	fileID := l.em.AddFile(display, src)
	file, parseDiags := parser.Parse(fileID, src)
	l.em.Report(parseDiags...)
	if file == nil {
		return fileID
	}
	l.mergeItems(file.Items, dir)
	return fileID
}

// mergeItems flattens the current M1-preview module surface into the program
// fed to sema. This is intentionally the same one-namespace behavior as
// `use`: file layout is useful today, while path-qualified item lookup and
// visibility checking remain future module work.
func (l *loader) mergeItems(items []ast.Item, dir string) {
	for _, it := range items {
		switch d := it.(type) {
		case *ast.UseDecl:
			l.loadUse(d, dir)
		case *ast.ModDecl:
			l.loadMod(d, dir)
		default:
			l.merged.Items = append(l.merged.Items, it)
		}
	}
}

// loadMod resolves `mod name;` using the conventional `name.zx` and
// `name/mod.zx` layouts. Inline `mod name { ... }` blocks are flattened in the
// same way, which makes them useful for organization before namespaces land.
func (l *loader) loadMod(d *ast.ModDecl, dir string) {
	if d.Inline {
		l.mergeItems(d.Items, dir)
		return
	}
	for _, cand := range []string{
		filepath.Join(dir, d.Name+".zx"),
		filepath.Join(dir, d.Name, "mod.zx"),
	} {
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			l.loadDiskFile(cand)
			return
		}
	}
	l.em.Report(diag.New("E9023", "cannot find module `%s` (looked for %s.zx and %s/mod.zx)", d.Name, d.Name, d.Name).WithSpan(d.Sp))
}

// loadUse resolves and loads one `use` / `import` declaration.
func (l *loader) loadUse(d *ast.UseDecl, dir string) {
	// Parsing aliases now keeps source forward-compatible, but aliases would
	// promise a namespace that the deliberately flat M1-preview loader cannot
	// provide. Reject them explicitly rather than silently ignoring `as name`.
	if d.As != "" {
		l.em.Report(diag.New("E9024", "import aliases are not supported by the current flat module loader; remove `as %s` and use imported items unqualified", d.As).WithSpan(d.Sp))
		return
	}
	if d.File != "" {
		// `use "helpers.zx";` / `import "helpers.zx";` — relative to the
		// importing file.
		l.loadDiskFile(filepath.Join(dir, filepath.FromSlash(d.File)))
		return
	}
	if len(d.Path) == 0 {
		return
	}

	// Preserve the historical convenience for local modules named net/http/sys
	// (and regex): their ABI calls can use the embedded runtime too. Embedded
	// modules also mark themselves in loadStdFile, which covers facade paths
	// like `import http.server;` whose last segment is not `http`.
	switch d.Path[len(d.Path)-1] {
	case "net", "http", "fs":
		l.usesNet = true
	case "sys":
		l.usesSys = true
	case "regex":
		l.usesRegex = true
	}

	parts := d.Path
	if parts[0] == "std" {
		if len(parts) < 2 {
			l.em.Report(diag.New("E9022", "`use std;` needs a module name, such as `use std::math;`").WithSpan(d.Sp))
			return
		}
		if module, ok := stdModule(parts[1:]); ok {
			l.loadStdFile(module)
			return
		}
		l.em.Report(diag.New("E9022", "there is no standard library module named `%s`", strings.Join(parts[1:], "::")).WithSpan(d.Sp))
		return
	}

	// `use a::b;` / `import a.b;` names a/b.zx: local modules win over
	// embedded ones so applications can provide their own package trees.
	name := filepath.Join(parts...) + ".zx"
	for _, cand := range []string{filepath.Join(dir, name), filepath.Join(dir, "lib", name)} {
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			l.loadDiskFile(cand)
			return
		}
	}
	if module, ok := stdModule(parts); ok {
		l.loadStdFile(module)
		return
	}
	l.em.Report(diag.New("E9022", "cannot find a library named `%s` (looked for %s next to the importing file, in its lib/ folder, and in the standard library)", strings.Join(parts, "::"), name).WithSpan(d.Sp))
}

// Tokens returns the token stream for a file, for the `tokenize` command.
func Tokens(path string, em *diag.Emitter) ([]token.Token, int, bool) {
	src, err := os.ReadFile(path)
	if err != nil {
		em.Report(diag.New("E9020", "cannot read %s: %v", path, err))
		return nil, 0, false
	}
	fileID := em.AddFile(path, src)
	lx := lexer.New(fileID, src)
	toks := lx.All()
	em.Report(lx.Errors()...)
	return toks, fileID, !em.HasErrors()
}

// Build compiles a source file to a native binary at outPath, returning the
// path of the emitted binary.
func Build(path, outPath string, opts Options, em *diag.Emitter) (string, bool) {
	return BuildFiles([]string{path}, outPath, opts, em)
}

// BuildFiles compiles a program made of several source files (see
// FrontEndFiles) to a native binary at outPath.
func BuildFiles(paths []string, outPath string, opts Options, em *diag.Emitter) (string, bool) {
	art, ok := FrontEndFiles(paths, opts, em)
	if !ok {
		return "", false
	}
	// When the program uses net/http, append the C networking runtime
	// (written to a temp file, like the generated C).
	if len(art.CExtra) > 0 {
		for _, extra := range art.CExtra {
			f, err := os.CreateTemp("", "zexic-extra-*.c")
			if err != nil {
				em.Report(diag.New("E9002", "%v", err))
				return "", false
			}
			defer os.Remove(f.Name())
			if _, err := f.WriteString(extra); err != nil {
				f.Close()
				em.Report(diag.New("E9002", "%v", err))
				return "", false
			}
			f.Close()
			opts.CFiles = append(opts.CFiles, f.Name())
		}
	}
	if outPath == "" {
		outPath = DefaultOutput(paths[0])
	}
	if len(art.CUnits) > 1 {
		if err := CompileUnits(art.CHeader, art.CUnits, outPath, opts); err != nil {
			em.Report(diag.New("E9002", "%v", err))
			return "", false
		}
	} else if err := CompileC(art.CSrc, outPath, opts); err != nil {
		em.Report(diag.New("E9002", "%v", err))
		return "", false
	}
	return outPath, true
}

// CompileC writes csrc to a temporary file and invokes the C compiler to
// produce a native binary at outPath.
func CompileC(csrc, outPath string, opts Options) error {
	dir, err := os.MkdirTemp("", "zexic-")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	cfile := filepath.Join(dir, "out.c")
	if err := os.WriteFile(cfile, []byte(csrc), 0o644); err != nil {
		return fmt.Errorf("writing C source: %w", err)
	}

	cc := opts.CC
	if cc == "" {
		cc = os.Getenv("CC")
	}
	if cc == "" {
		cc = "cc"
	}
	if _, err := exec.LookPath(cc); err != nil {
		return fmt.Errorf("C compiler %q not found; install gcc or clang, or set CC", cc)
	}

	args := []string{"-std=gnu11", optFlag(opts.OptLevel), "-fwrapv"}
	// Tree-shaking: each function/data gets its own section so the linker
	// can drop what the program never calls (only include what is used).
	args = append(args, "-ffunction-sections", "-fdata-sections")
	for _, dir := range opts.IncludeDirs {
		args = append(args, "-I"+dir)
	}
	args = append(args, "-o", outPath, cfile)
	// Extra C sources (the C side of the bridge) compile and link with
	// the generated code; libraries come last, as linkers require.
	args = append(args, opts.CFiles...)
	for _, dir := range opts.LibDirs {
		args = append(args, "-L"+dir)
	}
	for _, lib := range opts.LinkLibs {
		args = append(args, "-l"+lib)
	}
	// Drop sections no referenced symbol keeps alive (tree-shaking).
	if runtime.GOOS == "darwin" {
		args = append(args, "-Wl,-dead_strip")
	} else {
		args = append(args, "-Wl,--gc-sections")
	}
	cmd := exec.Command(cc, args...)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("C compiler (%s %s) failed: %w", cc, strings.Join(args, " "), err)
	}
	return nil
}

// CompileUnits compiles several translation units in parallel (each is the
// shared header plus one slice of function bodies) and links them with the
// extra C sources into a native binary at outPath. The units are the modular
// form of the generated C: the compiler front end never changes, only how
// the back end hands work to `cc`.
func CompileUnits(header string, units []string, outPath string, opts Options) error {
	dir, err := os.MkdirTemp("", "zexic-mod-")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	if err := os.WriteFile(filepath.Join(dir, "zexi_gen.h"), []byte(header), 0o644); err != nil {
		return fmt.Errorf("writing generated header: %w", err)
	}

	cc := opts.CC
	if cc == "" {
		cc = os.Getenv("CC")
	}
	if cc == "" {
		cc = "cc"
	}
	if _, err := exec.LookPath(cc); err != nil {
		return fmt.Errorf("C compiler %q not found; install gcc or clang, or set CC", cc)
	}

	base := []string{"-std=gnu11", optFlag(opts.OptLevel), "-fwrapv", "-ffunction-sections", "-fdata-sections", "-I" + dir}
	objs := make([]string, len(units))
	errs := make([]string, len(units))
	var wg sync.WaitGroup
	for i, src := range units {
		cfile := filepath.Join(dir, fmt.Sprintf("unit%d.c", i))
		if err := os.WriteFile(cfile, []byte("#include \"zexi_gen.h\"\n"+src), 0o644); err != nil {
			return fmt.Errorf("writing unit: %w", err)
		}
		objs[i] = filepath.Join(dir, fmt.Sprintf("unit%d.o", i))
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			args := append(append([]string{}, base...), "-c", cfile, "-o", objs[i])
			var stderr bytes.Buffer
			cmd := exec.Command(cc, args...)
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				errs[i] = fmt.Sprintf("compiling unit %d (%s): %v\n%s", i, cfile, err, stderr.String())
			}
		}(i)
	}
	wg.Wait()
	for _, e := range errs {
		if e != "" {
			return fmt.Errorf("%s", e)
		}
	}

	// Extra C bridge sources are compiled during this final link invocation, so
	// carry the same signed-overflow semantics as generated translation units.
	args := []string{"-std=gnu11", optFlag(opts.OptLevel), "-fwrapv", "-ffunction-sections", "-fdata-sections"}
	for _, inc := range opts.IncludeDirs {
		args = append(args, "-I"+inc)
	}
	args = append(args, "-o", outPath)
	args = append(args, objs...)
	args = append(args, opts.CFiles...)
	for _, libDir := range opts.LibDirs {
		args = append(args, "-L"+libDir)
	}
	for _, lib := range opts.LinkLibs {
		args = append(args, "-l"+lib)
	}
	if runtime.GOOS == "darwin" {
		args = append(args, "-Wl,-dead_strip")
	} else {
		args = append(args, "-Wl,--gc-sections")
	}
	cmd := exec.Command(cc, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("linker (%s) failed: %v\n%s", cc, err, stderr.String())
	}
	return nil
}

// DefaultOutput derives an output path from a source path: "a/b/hello.zx"
// becomes "hello" in the current directory.
func DefaultOutput(srcPath string) string {
	base := filepath.Base(srcPath)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	if base == "" {
		base = "a.out"
	}
	return base
}

func optFlag(level string) string {
	switch level {
	case "", "2":
		return "-O2"
	case "s", "z":
		return "-Os"
	case "0", "1", "3":
		return "-O" + level
	default:
		return "-O2"
	}
}
