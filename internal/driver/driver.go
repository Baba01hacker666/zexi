// Package driver orchestrates the Zexi pipeline: source -> tokens -> AST ->
// typed program -> C -> native binary. It owns process-level concerns
// (temporary directories, invoking the C compiler) so that the pure stages
// stay pure and testable.
package driver

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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
}

// Artifacts is the result of a successful front end + codegen run.
type Artifacts struct {
	Info *sema.Info
	CSrc string
	// FileID is the id of the source file in the emitter's table.
	FileID int
}

// FrontEnd runs lexing, parsing, type checking, and C generation. It returns
// the artifacts and whether the program is error-free. Diagnostics are
// reported to em; callers render them.
func FrontEnd(path string, opts Options, em *diag.Emitter) (*Artifacts, bool) {
	src, err := os.ReadFile(path)
	if err != nil {
		em.Report(diag.New("E9020", "cannot read %s: %v", path, err))
		return nil, false
	}
	fileID := em.AddFile(path, src)

	file, parseDiags := parser.Parse(fileID, src)
	em.Report(parseDiags...)
	if em.HasErrors() {
		return nil, false
	}

	info, semaDiags := sema.Check(file)
	em.Report(semaDiags...)
	if em.HasErrors() {
		return nil, false
	}

	csrc, cerr := zenc.Generate(info, zenc.Options{})
	if cerr != nil {
		em.Report(diag.New("E9001", "code generation failed: %v", cerr))
		return nil, false
	}
	return &Artifacts{Info: info, CSrc: csrc, FileID: fileID}, true
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
	art, ok := FrontEnd(path, opts, em)
	if !ok {
		return "", false
	}
	if outPath == "" {
		outPath = DefaultOutput(path)
	}
	if err := CompileC(art.CSrc, outPath, opts); err != nil {
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

	args := []string{"-std=gnu11", optFlag(opts.OptLevel), "-o", outPath, cfile}
	cmd := exec.Command(cc, args...)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("C compiler (%s %s) failed: %w", cc, strings.Join(args, " "), err)
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
