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
// `use` declarations import library files: `use "helpers.zx";` loads a file
// relative to the importing file, and `use math;` / `use std::math;` load a
// library by name — searched next to the importing file, in a `lib/` folder
// beside it, and finally in the standard library embedded in the compiler.
// Imports are transitive, every file is loaded at most once, and import
// cycles are harmless (a file being loaded counts as loaded). The first
// path is treated as the program.
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
// the compiler, importable from any program with `use math;`,
// `use text;`, or `use std::math;` — no installation or paths needed.
//
//go:embed std/*.zx
var stdFS embed.FS

// netCSource is the C networking runtime compiled into programs that
// `use net;` or `use http;`. Embedded so it needs no installation and
// stays in sync with the Zexi bindings.
//
//go:embed zxnet_c.txt
var netCSource []byte

// sysCSource is the C shim for the `sys` module (env, clocks, sleep, pid),
// embedded like the networking runtime.
//
//go:embed zxsys_c.txt
var sysCSource []byte

// loader reads source files (entry points plus everything they `use`),
// parses them, and merges their items into one program.
type loader struct {
	em      *diag.Emitter
	merged  *ast.File
	loaded  map[string]bool // canonical file keys already loaded
	usesNet bool            // program imports net/http (needs the C runtime)
	usesSys bool            // program imports sys/random (needs the C shim)
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

// loadStdFile loads one embedded standard-library file by base name
// (for example "math" for std/math.zx).
func (l *loader) loadStdFile(name string) int {
	key := "std:" + name
	if l.loaded[key] {
		return 0
	}
	display := "std/" + name + ".zx"
	src, err := fs.ReadFile(stdFS, display)
	if err != nil {
		l.em.Report(diag.New("E9022", "there is no standard library module named `%s`", name))
		return 0
	}
	return l.loadSource(key, display, src, "")
}

// loadSource parses one already-read file, records its items, and follows
// its `use` declarations. dir is the directory relative imports resolve
// against (empty for embedded files). It reports whether the program
// uses the C networking runtime (`use net;` / `use http;`).
func (l *loader) loadSource(key, display string, src []byte, dir string) int {
	l.loaded[key] = true // mark first: import cycles load each file once
	fileID := l.em.AddFile(display, src)
	file, parseDiags := parser.Parse(fileID, src)
	l.em.Report(parseDiags...)
	if file == nil {
		return fileID
	}
	for _, it := range file.Items {
		if d, ok := it.(*ast.UseDecl); ok {
			l.loadUse(d, dir)
			if d.File == "" && len(d.Path) > 0 {
				name := d.Path[len(d.Path)-1]
				if name == "net" || name == "http" {
					l.usesNet = true
				}
				if name == "sys" {
					l.usesSys = true
				}
			}
			continue // imports contribute their own items; never checked here
		}
		l.merged.Items = append(l.merged.Items, it)
	}
	return fileID
}

// loadUse resolves and loads one `use` declaration.
func (l *loader) loadUse(d *ast.UseDecl, dir string) {
	if d.File != "" {
		// `use "helpers.zx";` — a path relative to the importing file.
		l.loadDiskFile(filepath.Join(dir, filepath.FromSlash(d.File)))
		return
	}
	if len(d.Path) == 0 {
		return
	}
	if d.Path[0] == "std" {
		if len(d.Path) < 2 {
			l.em.Report(diag.New("E9022", "`use std;` needs a module name, such as `use std::math;`").WithSpan(d.Sp))
			return
		}
		l.loadStdFile(d.Path[len(d.Path)-1])
		return
	}
	// `use a::b;` names the file a/b.zx: look beside the importing file,
	// then in a lib/ folder beside it, then in the embedded stdlib.
	name := filepath.Join(d.Path...) + ".zx"
	for _, cand := range []string{filepath.Join(dir, name), filepath.Join(dir, "lib", name)} {
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			l.loadDiskFile(cand)
			return
		}
	}
	if _, err := fs.Stat(stdFS, path.Join("std", filepath.ToSlash(name))); err == nil {
		l.loadStdFile(d.Path[len(d.Path)-1])
		return
	}
	l.em.Report(diag.New("E9022", "cannot find a library named `%s` (looked for %s next to the importing file, in its lib/ folder, and in the standard library)", strings.Join(d.Path, "::"), name).WithSpan(d.Sp))
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

	args := []string{"-std=gnu11", optFlag(opts.OptLevel)}
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

	base := []string{"-std=gnu11", optFlag(opts.OptLevel), "-ffunction-sections", "-fdata-sections", "-I" + dir}
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

	args := []string{"-o", outPath}
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
