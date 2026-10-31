// Package driver orchestrates the Zexi pipeline: source -> tokens -> AST ->
// typed program -> C -> native binary. It owns process-level concerns
// (temporary directories, invoking the C compiler) so that the pure stages
// stay pure and testable.
package driver

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"zexi/internal/ast"
	zenc "zexi/internal/codegen/c"
	"zexi/internal/diag"
	"zexi/internal/lexer"
	"zexi/internal/sema"
	"zexi/internal/token"
	"zexi/internal/types"
)

// Options controls a compilation.
type Options struct {
	// OptLevel is passed to the C compiler: "0", "1", "2", "3", or "s".
	OptLevel string
	// CC is the C compiler to invoke. Empty means $CC or "cc".
	CC string
	// Root is the project root directory. Named imports (`use foo;`,
	// `use pkg::util;`, `mod foo;`) are searched here as well as next to the
	// importing file, so a `src/main.zx` entry can reach modules at the
	// project root. Empty means derive it from the entry file (see
	// ProjectRoot).
	Root string
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
	// Pthreads adds -pthread to the C compiler command line. The driver sets
	// it automatically when the program imports `thread`.
	Pthreads bool
	// Overlay supplies in-memory file contents (for unsaved editor buffers),
	// shadowing the disk for matching paths. Keys are cleaned to absolute,
	// symlink-resolved form when FrontEnd runs; values may be nil to mean
	// "deleted". The overlay is read-only from the driver's perspective.
	Overlay map[string][]byte
	// CheckOnly stops after type checking: no C is generated. The returned
	// Artifacts carry the semantic Info when checking produced one, even when
	// ok is false, so editor tooling can still answer queries over erroneous
	// programs. The regular Compile path is unaffected.
	CheckOnly bool
	// Backend picks the code generator: "" or "c" (the default, native C
	// backend), or "llvm" (MIR -> LLVM IR, linked with the backend's runtime
	// shim). The LLVM backend covers the mirrored MIR subset and rejects a
	// program that lowers to MIR outside it.
	Backend string
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
	// NeedsPthread is set when the program uses the `thread` module, so the
	// C compiler is invoked with -pthread.
	NeedsPthread bool
	// LinkLibs are libraries the program itself asked for with
	// `#[link("name")]`, in source order, appended to Options.LinkLibs.
	LinkLibs []string
	// LinkDirs are directories the program asked for with `#[link_dir("path")]`,
	// appended to Options.LibDirs.
	LinkDirs []string
}

// FrontEnd runs lexing, parsing, type checking, and C generation. It returns
// the artifacts and whether the program is error-free. Diagnostics are
// reported to em; callers render them.
// backendIsLLVM reports whether the options select the LLVM backend.
func backendIsLLVM(opts Options) bool {
	return opts.Backend == "llvm"
}

func FrontEnd(path string, opts Options, em *diag.Emitter) (*Artifacts, bool) {
	return FrontEndFiles([]string{path}, opts, em)
}

// FrontEndFiles is FrontEnd for a program split across several source files.
// The files are parsed independently (each keeps its own identity for
// diagnostics) and loaded as modules (see modules.go). `use` / `import`
// declarations import library files: `use "helpers.zx";` or
// `import "helpers.zx";` loads a relative file, while `use math;`,
// `import http.server;`, and `use std::math;` load a library by name — searched
// next to the importing file, in a `lib/` folder beside it, and finally in the
// standard library embedded in the compiler. `mod name;` loads `name.zx` or
// `name/mod.zx`. An import binds the module to a qualifier (`as alias`, else the
// full path and its last segment) so qualified paths are resolved and their
// `pub` visibility checked before checking. Imports are transitive, every file
// is loaded at most once, and cycles are harmless (a file being loaded counts as
// loaded). The first path is treated as the program.
func FrontEndFiles(paths []string, opts Options, em *diag.Emitter) (*Artifacts, bool) {
	if len(paths) == 0 {
		em.Report(diag.New("E9021", "no input files"))
		return nil, false
	}
	l := newLoader(em)
	l.overlay = normalizeOverlay(opts.Overlay)
	l.root = ProjectRoot(paths[0], opts.Root)
	for i, path := range paths {
		id := l.loadDiskFile(path)
		if i == 0 {
			l.firstID = id
		}
	}
	if em.HasErrors() {
		if opts.CheckOnly {
			return &Artifacts{FileID: l.firstID}, false
		}
		return nil, false
	}
	l.finish()
	if em.HasErrors() {
		if opts.CheckOnly {
			return &Artifacts{FileID: l.firstID}, false
		}
		return nil, false
	}
	merged := l.merged

	info, semaDiags := sema.Check(merged)
	em.Report(semaDiags...)
	if em.HasErrors() {
		if opts.CheckOnly {
			return &Artifacts{Info: info, FileID: l.firstID}, false
		}
		return nil, false
	}
	if opts.CheckOnly {
		return &Artifacts{Info: info, FileID: l.firstID}, true
	}

	art := &Artifacts{Info: info, FileID: l.firstID}
	// Debug builds check integer overflow; release builds wrap (see
	// docs/03-memory-model.md). Division by zero panics in both.
	cgOpts := zenc.Options{DebugChecks: opts.OptLevel == "0", ProjectDir: l.root}
	if jobs := splitJobs(opts, len(info.FnOrder)); jobs > 1 {
		header, units, cerr := zenc.GenerateSplit(info, cgOpts, jobs)
		if cerr != nil {
			em.Report(diag.New("E9001", "code generation failed: %v", cerr))
			return nil, false
		}
		switch {
		case len(units) > 1:
			art.CHeader, art.CUnits = header, units
		case len(units) == 1:
			art.CSrc = header + units[0]
		default:
			art.CSrc = header
		}
	} else {
		csrc, cerr := zenc.Generate(info, cgOpts)
		if cerr != nil {
			em.Report(diag.New("E9001", "code generation failed: %v", cerr))
			return nil, false
		}
		art.CSrc = csrc
	}

	// Native code and libraries the program names in its own source. Appending
	// the text to the generated translation unit (rather than compiling it as a
	// separate file) means inline C can use the runtime helpers the generated
	// code already defines: `zx_String`, the Vec/hash helpers, and anything a
	// `#[no_mangle]` function exposes.
	blocks, libs, linkDirs, includes, pthread := collectNative(merged.Items)
	if len(blocks) > 0 || len(includes) > 0 {
		var parts []string
		for _, h := range includes {
			parts = append(parts, `#include "`+h+`"`)
		}
		if len(blocks) > 0 {
			parts = append(parts, strings.Join(blocks, "\n"))
		}
		text := "\n// --- C source included from Zexi (`c \"\"\"...\"\"\"` blocks) ---\n" + strings.Join(parts, "\n")
		if len(art.CUnits) > 1 {
			art.CUnits[0] += text
		} else {
			art.CSrc += text
		}
	}
	art.LinkLibs = libs
	art.LinkDirs = linkDirs
	if pthread {
		art.NeedsPthread = true
	}
	return art, true
}

// collectNative returns the `c r"..."` blocks, the libraries named by
// `#[link("name")]`, and whether pthread was asked for. Both are declarations
// in source, so a library can extend the program (a new syscall wrapper, a
// vendored C helper, a system library) without the compiler knowing about it.
func collectNative(items []ast.Item) (blocks, libs, linkDirs, includes []string, pthread bool) {
	seenLib := map[string]bool{}
	seenBlock := map[string]bool{}
	seenLinkDir := map[string]bool{}
	seenInclude := map[string]bool{}
	var collect func(attrs []string)
	collect = func(attrs []string) {
		for _, a := range attrs {
			switch types.AttrName(a) {
			case "link":
				name := strings.Trim(types.AttrArg(a), " \t\"'")
				if name == "" {
					continue
				}
				if name == "pthread" {
					pthread = true
					continue
				}
				if seenLib[name] {
					continue
				}
				seenLib[name] = true
				libs = append(libs, name)
			case "link_dir":
				dir := strings.Trim(types.AttrArg(a), " \t\"'")
				if dir == "" {
					continue
				}
				if seenLinkDir[dir] {
					continue
				}
				seenLinkDir[dir] = true
				linkDirs = append(linkDirs, dir)
			case "include":
				hdr := strings.Trim(types.AttrArg(a), " \t\"'")
				if hdr == "" {
					continue
				}
				if seenInclude[hdr] {
					continue
				}
				seenInclude[hdr] = true
				includes = append(includes, hdr)
			}
		}
	}
	for _, it := range items {
		switch d := it.(type) {
		case *ast.CBlock:
			collect(d.Attrs)
			body := strings.TrimSpace(d.Body)
			if body == "" || seenBlock[body] {
				continue
			}
			seenBlock[body] = true
			blocks = append(blocks, body)
		case *ast.FnDecl:
			collect(d.Attrs)
		case *ast.StructDecl:
			collect(d.Attrs)
		case *ast.EnumDecl:
			collect(d.Attrs)
		case *ast.ImplBlock:
			for _, f := range d.Fns {
				collect(f.Attrs)
			}
		}
	}
	return blocks, libs, linkDirs, includes, pthread
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
		n = min(runtime.NumCPU(), 8)
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

// Tokens returns the token stream for a file, for the `tokenize` command.
func Tokens(path string, em *diag.Emitter) ([]token.Token, int, bool) {
	src, err := os.ReadFile(path)
	if err != nil {
		em.Report(diag.New("E9020", "cannot read %s: %v", path, err))
		return nil, 0, false
	}
	fileID := em.AddFile(path, src)
	lx := lexer.New(fileID, 0, src)
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
			defer func() { _ = os.Remove(f.Name()) }()
			if _, err := f.WriteString(extra); err != nil {
				_ = f.Close()
				em.Report(diag.New("E9002", "%v", err))
				return "", false
			}
			_ = f.Close()
			opts.CFiles = append(opts.CFiles, f.Name())
		}
	}
	if outPath == "" {
		outPath = DefaultOutput(paths[0])
	}
	if art.NeedsPthread {
		opts.Pthreads = true
	}
	if len(art.LinkLibs) > 0 {
		opts.LinkLibs = append(opts.LinkLibs, art.LinkLibs...)
	}
	if len(art.LinkDirs) > 0 {
		opts.LibDirs = append(opts.LibDirs, art.LinkDirs...)
	}
	if backendIsLLVM(opts) {
		_, err := CompileLLVM(art.Info, outPath, opts, false)
		if err != nil {
			em.Report(diag.New("E9003", "%v", err))
			return "", false
		}
		return outPath, true
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
	defer func() { _ = os.RemoveAll(dir) }()

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
		return fmt.Errorf("c compiler %q not found; install gcc or clang, or set CC", cc)
	}

	args := []string{"-std=gnu11", optFlag(opts.OptLevel), "-fwrapv"}
	// Tree-shaking: each function/data gets its own section so the linker
	// can drop what the program never calls (only include what is used).
	args = append(args, "-ffunction-sections", "-fdata-sections")
	if opts.Pthreads {
		args = append(args, "-pthread")
	}
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
		return fmt.Errorf("c compiler (%s %s) failed: %w", cc, strings.Join(args, " "), err)
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
	defer func() { _ = os.RemoveAll(dir) }()

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
		return fmt.Errorf("c compiler %q not found; install gcc or clang, or set CC", cc)
	}

	base := []string{"-std=gnu11", optFlag(opts.OptLevel), "-fwrapv", "-ffunction-sections", "-fdata-sections", "-I" + dir}
	if opts.Pthreads {
		base = append(base, "-pthread")
	}
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
	if opts.Pthreads {
		args = append(args, "-pthread")
	}
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
		return fmt.Errorf("linker (%s) failed: %w\n%s", cc, err, stderr.String())
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
