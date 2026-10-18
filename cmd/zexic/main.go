// Command zexic is the Zexi compiler driver. It parses the command line and
// orchestrates the pipeline in internal/driver; every compiler stage lives
// under internal/.
//
// Usage:
//
//	zexic <command> [options] <file.zx>
//
// Commands: check, build, run, emit, tokenize, bindgen, version.
// See docs/04 §4.9 for the planned CLI surface.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"zexi/internal/bindgen"
	"zexi/internal/diag"
	"zexi/internal/driver"
	"zexi/internal/token"
)

const version = "zexi 0.1.0 (M0-alpha + M1 foundations: imports, stdlib, native C)"

func main() { os.Exit(run(os.Args[1:])) }

// cli holds parsed command-line options.
type cli struct {
	positional []string
	progArgs   []string // program arguments after `--` (run only)
	out        string
	emit       string
	optLevel   string
	cc         string
	// C bridge options.
	cFiles      []string
	linkLibs    []string
	libDirs     []string
	includeDirs []string
	// jobs is how many C translation units to compile in parallel
	// (0 = automatic, 1 = single file).
	jobs int
}

func parseArgs(args []string) cli {
	var c cli
	afterSep := false // everything after `--` is a program argument
	for i := 0; i < len(args); i++ {
		a := args[i]
		if afterSep {
			c.progArgs = append(c.progArgs, a)
			continue
		}
		switch {
		case a == "--":
			afterSep = true
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
		case strings.HasPrefix(a, "--csrc="):
			c.cFiles = append(c.cFiles, strings.TrimPrefix(a, "--csrc="))
		case strings.HasPrefix(a, "--link="):
			c.linkLibs = append(c.linkLibs, strings.TrimPrefix(a, "--link="))
		case strings.HasPrefix(a, "--lib-dir="):
			c.libDirs = append(c.libDirs, strings.TrimPrefix(a, "--lib-dir="))
		case strings.HasPrefix(a, "--include="):
			c.includeDirs = append(c.includeDirs, strings.TrimPrefix(a, "--include="))
		case strings.HasPrefix(a, "--jobs="):
			if n, err := strconv.Atoi(strings.TrimPrefix(a, "--jobs=")); err == nil && n >= 0 {
				c.jobs = n
			}
		case strings.HasPrefix(a, "-"):
			// Unknown flag: ignore for forward compatibility.
		default:
			c.positional = append(c.positional, a)
		}
	}
	return c
}

func (c cli) opts() driver.Options {
	return driver.Options{
		OptLevel:    c.optLevel,
		CC:          c.cc,
		CFiles:      c.cFiles,
		LinkLibs:    c.linkLibs,
		LibDirs:     c.libDirs,
		IncludeDirs: c.includeDirs,
		Jobs:        c.jobs,
	}
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
	case "bindgen":
		return cmdBindgen(c)
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
	// Arguments after a `--` separator belong to the program, not to the
	// compiler: `zexic run app.zx -- arg1 arg2` runs app with arg1 arg2.
	progArgs := c.progArgs
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

	child := exec.Command(out, progArgs...)
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
	opts := c.opts()
	opts.Jobs = 1 // emit prints one translation unit
	art, ok := driver.FrontEndFiles(paths, opts, em)
	if !ok {
		em.Render(os.Stderr, emFileID(em))
		return 1
	}
	fmt.Print(art.CSrc)
	return 0
}

// cmdBindgen translates a C header into Zexi `extern "C"` declarations,
// printed to stdout or written to the -o file. Declarations bindgen cannot
// map are listed in the output as comments.
func cmdBindgen(c cli) int {
	path, ok := requireFile(c, "bindgen")
	if !ok {
		return 2
	}
	src, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zexic bindgen: %v\n", err)
		return 1
	}
	res := bindgen.Generate(src, filepath.Base(path))
	if c.out != "" {
		if err := os.WriteFile(c.out, []byte(res.Source), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "zexic bindgen: %v\n", err)
			return 1
		}
		fmt.Printf("wrote %s (%d declaration(s), %d skipped)\n", c.out, len(res.Bound), len(res.Skipped))
		return 0
	}
	fmt.Print(res.Source)
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
  bindgen    generate Zexi extern declarations from a C header
  version    print the compiler version

Multiple .zx files are compiled together into one program (the first is the
entry file; the rest are usually library files).

options:
  -o <path>                  output path for build (or for bindgen)
  --opt-level=<0|1|2|3|s>    optimization profile (default 2, 's' for size)
  --cc=<path>                C compiler to use (default $CC or cc)
  --emit=<c>                 intermediate form to print

C bridge options (build/run):
  --csrc=<file.c>            extra C source compiled and linked in (repeatable)
  --link=<lib>               library to link, like -l<lib> (repeatable)
  --lib-dir=<dir>            library search directory (repeatable)
  --include=<dir>            header search directory for --csrc files (repeatable)
  --jobs=<n>                 parallel C compile units (0 = auto, 1 = single file)
`)
}
