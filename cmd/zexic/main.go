// Command zexic is the Zexi compiler driver. It is deliberately thin: argument
// parsing, pipeline orchestration, and diagnostic rendering live here; every
// stage lives in internal/.
//
// Usage:
//
//	zexic <command> [options] <file.zx>
//
// Commands: check, build, run, tokenize, version.
// See docs/04 §4.9 for the full planned CLI.
package main

import (
	"fmt"
	"os"
	"strings"

	"zexi/internal/diag"
	"zexi/internal/lexer"
	"zexi/internal/parser"
	"zexi/internal/token"
)

const version = "zexi 0.0.1 (design preview)"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return 2
	}

	cmd := args[0]
	rest := args[1:]

	switch cmd {
	case "version", "--version", "-V":
		fmt.Println(version)
		return 0
	case "help", "--help", "-h":
		usage(os.Stdout)
		return 0
	case "tokenize":
		return cmdTokenize(rest)
	case "check", "build", "run":
		return cmdPipeline(cmd, rest)
	default:
		fmt.Fprintf(os.Stderr, "zexic: unknown command %q\n", cmd)
		usage(os.Stderr)
		return 2
	}
}

// cmdTokenize prints the token stream. It is the one end-to-end command that
// works today and doubles as a debugging aid for the lexer.
func cmdTokenize(args []string) int {
	path, ok := firstFileArg(args)
	if !ok {
		fmt.Fprintln(os.Stderr, "zexic tokenize: expected a .zx file argument")
		return 2
	}
	src, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zexic: cannot read %s: %v\n", path, err)
		return 2
	}

	em := diag.NewEmitter()
	fileID := em.AddFile(path, src)
	lx := lexer.New(fileID, src)
	for _, t := range lx.All() {
		if t.Kind == token.KindEOF {
			fmt.Printf("%d:%-3d  %s\n", t.Pos.Line, t.Pos.Col, t.Kind)
			break
		}
		fmt.Printf("%d:%-3d  %s\n", t.Pos.Line, t.Pos.Col, t)
	}
	em.Report(lx.Errors()...)
	if em.HasErrors() {
		em.Render(os.Stderr, fileID)
		return 1
	}
	return 0
}

// cmdPipeline runs the shared front end for check/build/run. Build and run are
// gated on the parser/backends landing (docs/05).
func cmdPipeline(cmd string, args []string) int {
	path, ok := firstFileArg(args)
	if !ok {
		fmt.Fprintf(os.Stderr, "zexic %s: expected a .zx file argument\n", cmd)
		return 2
	}
	src, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zexic: cannot read %s: %v\n", path, err)
		return 2
	}

	em := diag.NewEmitter()
	fileID := em.AddFile(path, src)

	// Front end: lex -> parse. Later stages (resolve, check, borrowck, mir,
	// codegen) slot in here in pipeline order.
	_, parseDiags := parser.Parse(fileID, src)
	em.Report(parseDiags...)

	if cmd != "check" && !em.HasErrors() {
		fmt.Fprintf(os.Stderr, "zexic %s: code generation is not implemented yet; see docs/05 (M0 roadmap)\n", cmd)
	}

	em.Render(os.Stderr, fileID)
	if em.HasErrors() {
		return 1
	}
	return 0
}

// firstFileArg returns the first non-flag argument, i.e. the source path.
func firstFileArg(args []string) (string, bool) {
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		return a, true
	}
	return "", false
}

func usage(w *os.File) {
	fmt.Fprint(w, `zexi - the Zexi programming language compiler

usage:
  zexic <command> [options] <file.zx>

commands:
  check      parse, typecheck, and borrow-check only
  build      compile to a standalone native binary
  run        build to a temporary path and execute
  tokenize   print the token stream (front end demo)
  version    print the compiler version
  help       show this message

options (planned, see docs/04):
  -o <path>                  output path
  --opt-level=<0|1|2|3|s>    optimization profile (default 2, 's' for size)
  --target=<triple>          cross-compile
  --static / --dynamic       link mode
  --emit=<ast|hir|mir|c|asm|bin>
`)
}
