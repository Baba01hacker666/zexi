// Command zexic is the Zexi compiler driver. It parses the command line and
// orchestrates the pipeline in internal/driver; every compiler stage lives
// under internal/.
//
// Usage:
//
//	zexic <command> [options] <file.zx>
//
// Commands: check, build, run, emit, tokenize, version.
// See docs/04 §4.9 for the planned CLI surface.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"zexi/internal/diag"
	"zexi/internal/driver"
	"zexi/internal/token"
)

const version = "zexi 0.1.0 (M0-alpha: functions, control flow, printing)"

func main() { os.Exit(run(os.Args[1:])) }

// cli holds parsed command-line options.
type cli struct {
	positional []string
	out        string
	emit       string
	optLevel   string
	cc         string
}

func parseArgs(args []string) cli {
	var c cli
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-o" && i+1 < len(args):
			i++
			c.out = args[i]
		case strings.HasPrefix(a, "--output="):
			c.out = strings.TrimPrefix(a, "--output=")
		case strings.HasPrefix(a, "--emit="):
			c.emit = strings.TrimPrefix(a, "--emit=")
		case strings.HasPrefix(a, "--opt-level="):
			c.optLevel = strings.TrimPrefix(a, "--opt-level=")
		case strings.HasPrefix(a, "--cc="):
			c.cc = strings.TrimPrefix(a, "--cc=")
		case strings.HasPrefix(a, "-"):
			// Unknown flag: ignore for forward compatibility.
		default:
			c.positional = append(c.positional, a)
		}
	}
	return c
}

func (c cli) opts() driver.Options {
	return driver.Options{OptLevel: c.optLevel, CC: c.cc}
}

func run(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return 2
	}
	cmd := args[0]
	c := parseArgs(args[1:])

	switch cmd {
	case "version", "--version", "-V":
		fmt.Println(version)
		return 0
	case "help", "--help", "-h":
		usage(os.Stdout)
		return 0
	case "tokenize":
		return cmdTokenize(c)
	case "check":
		return cmdCheck(c)
	case "build":
		return cmdBuild(c)
	case "run":
		return cmdRun(c)
	case "emit":
		return cmdEmit(c)
	default:
		fmt.Fprintf(os.Stderr, "zexic: unknown command %q\n", cmd)
		usage(os.Stderr)
		return 2
	}
}

func requireFile(c cli, cmd string) (string, bool) {
	if len(c.positional) == 0 {
		fmt.Fprintf(os.Stderr, "zexic %s: expected a .zx file argument\n", cmd)
		return "", false
	}
	return c.positional[0], true
}

// requireFiles returns all positional inputs. The first is the program; the
// rest are usually library files compiled into it.
func requireFiles(c cli, cmd string) ([]string, bool) {
	if len(c.positional) == 0 {
		fmt.Fprintf(os.Stderr, "zexic %s: expected a .zx file argument\n", cmd)
		return nil, false
	}
	return c.positional, true
}

func cmdTokenize(c cli) int {
	path, ok := requireFile(c, "tokenize")
	if !ok {
		return 2
	}
	em := diag.NewEmitter()
	toks, fileID, ok := driver.Tokens(path, em)
	if !ok {
		em.Render(os.Stderr, fileID)
		return 1
	}
	for _, t := range toks {
		if t.Kind == token.KindEOF {
			fmt.Printf("%d:%-3d  %s\n", t.Pos.Line, t.Pos.Col, t.Kind)
			break
		}
		fmt.Printf("%d:%-3d  %s\n", t.Pos.Line, t.Pos.Col, t)
	}
	return 0
}

func cmdCheck(c cli) int {
	paths, ok := requireFiles(c, "check")
	if !ok {
		return 2
	}
	em := diag.NewEmitter()
	art, ok := driver.FrontEndFiles(paths, c.opts(), em)
	if !ok {
		em.Render(os.Stderr, emFileID(em))
		return 1
	}
	fmt.Printf("%s: ok (%d function(s), %d constant(s))\n", filepath.Base(paths[0]), len(art.Info.FnOrder), len(art.Info.Consts))
	return 0
}

func cmdBuild(c cli) int {
	paths, ok := requireFiles(c, "build")
	if !ok {
		return 2
	}
	em := diag.NewEmitter()
	out, ok := driver.BuildFiles(paths, c.out, c.opts(), em)
	if !ok {
		em.Render(os.Stderr, emFileID(em))
		return 1
	}
	fmt.Printf("built %s\n", out)
	return 0
}

func cmdRun(c cli) int {
	paths, ok := requireFiles(c, "run")
	if !ok {
		return 2
	}
	em := diag.NewEmitter()
	dir, err := os.MkdirTemp("", "zexic-run-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "zexic run: %v\n", err)
		return 2
	}
	defer os.RemoveAll(dir)

	out := filepath.Join(dir, driver.DefaultOutput(paths[0]))
	if _, ok := driver.BuildFiles(paths, out, c.opts(), em); !ok {
		em.Render(os.Stderr, emFileID(em))
		return 1
	}

	child := exec.Command(out)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if err := child.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "zexic run: %v\n", err)
		return 2
	}
	return 0
}

func cmdEmit(c cli) int {
	paths, ok := requireFiles(c, "emit")
	if !ok {
		return 2
	}
	if c.emit == "" {
		c.emit = "c"
	}
	if c.emit != "c" {
		fmt.Fprintf(os.Stderr, "zexic emit: --emit=%s is not supported yet (try `c`)\n", c.emit)
		return 2
	}
	em := diag.NewEmitter()
	art, ok := driver.FrontEndFiles(paths, c.opts(), em)
	if !ok {
		em.Render(os.Stderr, emFileID(em))
		return 1
	}
	fmt.Print(art.CSrc)
	return 0
}

// emFileID returns the id of the single source file registered with em, or 0.
func emFileID(em *diag.Emitter) int { return em.FirstFileID() }

func usage(w *os.File) {
	fmt.Fprint(w, `zexi - the Zexi programming language compiler

usage:
  zexic <command> [options] <file.zx> [lib.zx ...]

commands:
  check      parse, typecheck, and validate (no code generation)
  build      compile to a standalone native binary
  run        build to a temporary path and execute
  emit       print an intermediate form (--emit=c)
  tokenize   print the token stream
  version    print the compiler version

Multiple .zx files are compiled together into one program (the first is the
entry file; the rest are usually library files).

options:
  -o <path>                  output path for build
  --opt-level=<0|1|2|3|s>    optimization profile (default 2, 's' for size)
  --cc=<path>                C compiler to use (default $CC or cc)
  --emit=<c>                 intermediate form to print
`)
}
