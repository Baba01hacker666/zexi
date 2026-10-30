// Command zexic is the Zexi compiler driver. It parses the command line and
// orchestrates the pipeline in internal/driver; every compiler stage lives
// under internal/.
//
// Usage:
//
//	zexic <command> [options] <file.zx>
//
// Commands: check, build, run, emit, tokenize, bindgen, lsp, version.
// See docs/04 §4.9 for the planned CLI surface.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"

	"zexi/internal/bindgen"
	"zexi/internal/diag"
	"zexi/internal/driver"
	"zexi/internal/lsp"
	"zexi/internal/mir"
	"zexi/internal/registry"
	"zexi/internal/token"
)

const version = "zexi 0.1.0 (M0-alpha + M1 foundations: modules, projects, stdlib, native C)"

// releaseVersion is stamped by a release build with
// -ldflags "-X main.releaseVersion=zexi 0.2.0 (...)" and replaces the version
// line; a source build instead appends the revision `go build` recorded.
var releaseVersion string

// versionString reports the compiler's version, from a release's build stamp
// or, in a source build, beside the VCS revision the toolchain embedded in
// the binary (`git describe` for a checkout, through `go build`'s VCS info).
func versionString() string {
	if releaseVersion != "" {
		return releaseVersion
	}
	if rev := buildRevision(); rev != "" {
		return version + " (built from " + rev + ")"
	}
	return version
}

// buildRevision reports the VCS revision `go build` recorded in the binary,
// shortened and marked when the build tree was dirty. Empty when unknown.
func buildRevision() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var rev string
	dirty := false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return ""
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		rev += "+dirty"
	}
	return rev
}

func main() { os.Exit(run(os.Args[1:])) }

// cli holds parsed command-line options.
type cli struct {
	positional []string
	progArgs   []string // program arguments after `--` (run only)
	out        string
	emit       string
	optLevel   string
	cc         string
	// root overrides the project root for named imports and `mod` (--root).
	root string
	// C bridge options.
	cFiles      []string
	linkLibs    []string
	libDirs     []string
	includeDirs []string
	// jobs is how many C translation units to compile in parallel
	// (0 = automatic, 1 = single file).
	jobs int
	// msgFormat selects how diagnostics are printed: "human" (default) or
	// "json" (one JSON object per line).
	msgFormat string
	// add-only options: where to install a package, and whether to replace an
	// existing module file.
	addDir string
	force  bool
	list   bool
	// unknown collects flags this version does not know, so they are reported
	// instead of silently changing nothing.
	unknown []string
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
		case strings.HasPrefix(a, "--message-format="):
			c.msgFormat = strings.TrimPrefix(a, "--message-format=")
		case strings.HasPrefix(a, "--cc="):
			c.cc = strings.TrimPrefix(a, "--cc=")
		case strings.HasPrefix(a, "--root="):
			c.root = strings.TrimPrefix(a, "--root=")
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
		case a == "--jobs" && i+1 < len(args):
			i++
			if n, err := strconv.Atoi(args[i]); err == nil && n >= 0 {
				c.jobs = n
			}
		case a == "-O0" || a == "-O1" || a == "-O2" || a == "-O3" || a == "-Os" || a == "-Oz":
			// The classic spellings, so `zexic build -O2 x.zx` does not need
			// --opt-level=.
			c.optLevel = a[2:]
		case a == "--force":
			c.force = true
		case a == "--list":
			c.list = true
		case strings.HasPrefix(a, "--dir="):
			c.addDir = strings.TrimPrefix(a, "--dir=")
		case strings.HasPrefix(a, "-"):
			// Reported rather than ignored: a mistyped flag used to build a
			// program with silently different settings.
			c.unknown = append(c.unknown, a)
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
		Root:        c.root,
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
	switch c.msgFormat {
	case "", "human", "json":
	default:
		fmt.Fprintf(os.Stderr, "zexic: unknown --message-format=%q (want human or json)\n", c.msgFormat)
		return 2
	}

	if len(c.unknown) > 0 {
		fmt.Fprintf(os.Stderr, "zexic %s: unknown option %s\n", cmd, strings.Join(c.unknown, ", "))
		fmt.Fprintf(os.Stderr, "run `zexic help` for the options this build understands\n")
		return 2
	}

	switch cmd {
	case "version", "--version", "-V":
		fmt.Println(versionString())
		return 0
	case "help", "--help", "-h":
		usage(os.Stdout)
		return 0
	case "tokenize":
		return cmdTokenize(c)
	case "bindgen":
		return cmdBindgen(c)
	case "add", "install":
		return cmdAdd(c)
	case "check":
		return cmdCheck(c)
	case "build":
		return cmdBuild(c)
	case "run":
		return cmdRun(c)
	case "emit":
		return cmdEmit(c)
	case "lsp":
		return cmdLsp(c)
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

// renderDiags prints the collected diagnostics in the selected
// --message-format: the human rustc-like form (default) or JSONL for tools.
func renderDiags(c cli, em *diag.Emitter, fileID int) {
	if c.msgFormat == "json" {
		em.RenderJSON(os.Stderr, fileID)
		return
	}
	em.Render(os.Stderr, fileID)
}

// resolveInputs resolves the first positional argument as a project entry: a
// `.zx` file or a directory containing `main.zx`/`src/main.zx`. It returns the
// entry plus any extra files and the project root to pass through Options.
func resolveInputs(c cli, cmd string) ([]string, string, bool) {
	if len(c.positional) == 0 {
		fmt.Fprintf(os.Stderr, "zexic %s: expected a .zx file or a project directory\n", cmd)
		return nil, "", false
	}
	entry, root, err := driver.ResolveEntry(c.positional[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "zexic %s: %v\n", cmd, err)
		return nil, "", false
	}
	paths := append([]string{entry}, c.positional[1:]...)
	return paths, root, true
}

func cmdTokenize(c cli) int {
	path, ok := requireFile(c, "tokenize")
	if !ok {
		return 2
	}
	em := diag.NewEmitter()
	toks, fileID, ok := driver.Tokens(path, em)
	if !ok {
		renderDiags(c, em, fileID)
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
	paths, root, ok := resolveInputs(c, "check")
	if !ok {
		return 2
	}
	c.root = root
	em := diag.NewEmitter()
	art, ok := driver.FrontEndFiles(paths, c.opts(), em)
	if !ok {
		renderDiags(c, em, emFileID(em))
		return 1
	}
	fmt.Printf("%s: ok (%d function(s), %d constant(s))\n", filepath.Base(paths[0]), len(art.Info.FnOrder), len(art.Info.Consts))
	return 0
}

func cmdBuild(c cli) int {
	paths, root, ok := resolveInputs(c, "build")
	if !ok {
		return 2
	}
	c.root = root
	em := diag.NewEmitter()
	out, ok := driver.BuildFiles(paths, c.out, c.opts(), em)
	if !ok {
		renderDiags(c, em, emFileID(em))
		return 1
	}
	fmt.Printf("built %s\n", out)
	return 0
}

func cmdRun(c cli) int {
	paths, root, ok := resolveInputs(c, "run")
	if !ok {
		return 2
	}
	c.root = root
	// Arguments after a `--` separator belong to the program, not to the
	// compiler: `zexic run app.zx -- arg1 arg2` runs app with arg1 arg2.
	progArgs := c.progArgs
	em := diag.NewEmitter()
	dir, err := os.MkdirTemp("", "zexic-run-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "zexic run: %v\n", err)
		return 2
	}
	defer func() { _ = os.RemoveAll(dir) }()

	out := filepath.Join(dir, driver.DefaultOutput(paths[0]))
	if _, ok := driver.BuildFiles(paths, out, c.opts(), em); !ok {
		renderDiags(c, em, emFileID(em))
		return 1
	}

	child := exec.Command(out, progArgs...)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if err := child.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "zexic run: %v\n", err)
		return 2
	}
	return 0
}

func cmdEmit(c cli) int {
	paths, root, ok := resolveInputs(c, "emit")
	if !ok {
		return 2
	}
	c.root = root
	if c.emit == "" {
		c.emit = "c"
	}
	if c.emit != "c" && c.emit != "mir" {
		fmt.Fprintf(os.Stderr, "zexic emit: --emit=%s is not supported yet (try `c` or `mir`)\n", c.emit)
		return 2
	}
	em := diag.NewEmitter()
	if c.emit == "mir" {
		return emitMIR(paths, c, em)
	}
	opts := c.opts()
	opts.Jobs = 1 // emit prints one translation unit
	art, ok := driver.FrontEndFiles(paths, opts, em)
	if !ok {
		renderDiags(c, em, emFileID(em))
		return 1
	}
	fmt.Print(art.CSrc)
	return 0
}

// emitMIR prints the program's mid-level IR: a control-flow graph per function,
// with every value in a numbered local. MIR is the contract each backend
// consumes (docs/04 §4.3), so this dump is the supported way to see what a
// backend is handed before any of them exist.
func emitMIR(paths []string, c cli, em *diag.Emitter) int {
	opts := c.opts()
	opts.CheckOnly = true // no C is generated
	art, ok := driver.FrontEndFiles(paths, opts, em)
	if !ok {
		renderDiags(c, em, emFileID(em))
		return 1
	}
	res := mir.Lower(art.Info)
	// Validate before printing: a backend is entitled to assume these
	// invariants, so a dump that violates them is a compiler bug worth
	// reporting rather than handing to one.
	if err := res.Prog.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "zexic emit: internal error: %v\n", err)
		fmt.Fprint(os.Stderr, res.Prog.String())
		return 3
	}
	fmt.Print(res.Prog.String())
	if n := len(res.Unsupported); n > 0 {
		fmt.Fprintf(os.Stderr, "zexic emit: MIR lowered %d/%d reachable functions; %d not lowered:\n",
			res.Lowered, res.Total, n)
		for i, u := range res.Unsupported {
			if i == 10 {
				fmt.Fprintf(os.Stderr, "  ... and %d more\n", n-10)
				break
			}
			fmt.Fprintf(os.Stderr, "  %s: %s\n", u.Fn, u.Feature)
		}
	}
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

// cmdLsp runs the language server over stdio. An optional argument names the
// program entry (for example the project's main.zx); without one, each open
// document is analyzed on its own and the "no main" error is suppressed for
// documents that declare no main function.
func cmdLsp(c cli) int {
	root := ""
	if len(c.positional) > 0 {
		root = c.positional[0]
	}
	return lsp.Serve(os.Stdin, os.Stdout, lsp.Options{Root: root})
}

func usage(w *os.File) {
	_, _ = fmt.Fprint(w, `zexi - the Zexi programming language compiler

usage:
  zexic <command> [options] <file.zx> [lib.zx ...]

commands:
  check      parse, typecheck, and validate (no code generation)
  build      compile to a standalone native binary
  run        build to a temporary path and execute
  emit       print an intermediate form (--emit=c)
  tokenize   print the token stream
  bindgen    generate Zexi extern declarations from a C header
  add        install a C-library module into lib/ (no argument: list them)
  lsp        run the language server on stdio (optional argument: program entry)
  version    print the compiler version

Project builds: the first argument may be a project directory. main.zx is
tried first, then src/main.zx. Named imports (use foo;, mod foo;) are
resolved next to the importing file, in its lib/ folder, at the project root,
and finally in the standard library. A directory package (use util; with a
util/ folder holding .zx files) loads the whole folder. Extra .zx files may
still be passed explicitly and are compiled together with the entry.

C libraries: "zexic add sqlite" installs a bound module (sqlite, zlib, crypto,
curl, microhttpd) into the project's lib/ folder; "zexic add" alone lists the
catalog with this machine's dependency status.

An option this build does not know is an error, not a silent no-op.

options:
  -o <path>                  output path for build (or for bindgen)
  --opt-level=<0|1|2|3|s|z>  optimization profile (default 2; also -O2, -Os)
  --cc=<path>                C compiler to use (default $CC or cc)
  --emit=<c>                 intermediate form to print
  --root=<dir>               project root for named imports and mod
  --message-format=<human|json>
                             diagnostic output: rustc-like (default) or one
                             JSON object per line for tools

C bridge options (build/run):
  --csrc=<file.c>            extra C source compiled and linked in (repeatable)
  --link=<lib>               library to link, like -l<lib> (repeatable)
  --lib-dir=<dir>            library search directory (repeatable)
  --include=<dir>            header search directory for --csrc files (repeatable)
  --jobs=<n>                 parallel C compile units (0 = auto, 1 = single file)
`)
}

// cmdAdd installs a catalog module into the project's lib/ folder, or lists the
// catalog when no name is given. The module is one .zx file carrying its own
// extern declarations and C glue (docs/17-extending.md), so installing is a file
// copy: no manifest, no lockfile, no network, and nothing for the compiler to
// learn. `use <name>;` then resolves through the loader's existing lib/ search.
func cmdAdd(c cli) int {
	if len(c.positional) == 0 || c.list {
		return listPackages(c)
	}
	name := c.positional[0]
	if _, ok := registry.Find(name); !ok {
		fmt.Fprintf(os.Stderr, "zexic add: no package named %q\n", name)
		if hints := registry.Suggest(name); len(hints) > 0 {
			fmt.Fprintf(os.Stderr, "did you mean %s?\n", strings.Join(hints, " or "))
		}
		fmt.Fprintf(os.Stderr, "run `zexic add` with no arguments to list the catalog\n")
		return 1
	}
	dir := c.addDir
	if dir == "" {
		root := c.root
		if root == "" {
			root = "."
		}
		dir = registry.DefaultDir(root)
	}
	path, pkg, err := registry.Install(dir, name, c.force)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zexic add: %v\n", err)
		return 1
	}
	fmt.Printf("installed %s — %s\n", path, pkg.Summary)
	fmt.Printf("  use %s;\n", pkg.Name)

	// Ask the toolchain whether the system half is there. This is advisory: the
	// program will not build without it, but failing `add` would leave the user
	// with a module they cannot even keep.
	deps := registry.Probe(c.cc, pkg)
	switch {
	case deps.Ready:
		fmt.Printf("  system check: ready (%s)\n", strings.Join(pkg.Headers, ", "))
	case !deps.Checked:
		fmt.Printf("  system check: skipped (no C compiler on PATH)\n")
		fmt.Printf("  %s needs %s\n", pkg.Name, strings.Join(pkg.Libs, ", "))
	default:
		fmt.Printf("  system check: missing %s\n", strings.Join(append(deps.MissingHeaders, prefixLibs(deps.MissingLibs)...), ", "))
		fmt.Printf("  install it with: %s\n", pkg.Hint)
		if deps.Detail != "" {
			fmt.Printf("  compiler said: %s\n", deps.Detail)
		}
	}
	return 0
}

func prefixLibs(libs []string) []string {
	out := make([]string, len(libs))
	for i, l := range libs {
		out[i] = "lib" + l
	}
	return out
}

// listPackages prints the catalog with live dependency status, so `zexic add`
// on its own answers "what can I add, and what would I need to install first?".
func listPackages(c cli) int {
	pkgs := registry.All()
	fmt.Printf("%-12s %-10s %-52s %s\n", "package", "category", "what it gives you", "system")
	for _, p := range pkgs {
		status := registry.Probe(c.cc, p).String()
		fmt.Printf("%-12s %-10s %-52s %s\n", p.Name, p.Category, p.Summary, status)
	}
	fmt.Printf("\ninstall one with `zexic add <package>` (writes lib/<package>.zx),\nthen import it with `use <package>;`.\n")
	for _, p := range pkgs {
		if d := registry.Probe(c.cc, p); d.Checked && !d.Ready {
			fmt.Printf("  %-12s %s\n", p.Name, p.Hint)
		}
	}
	fmt.Printf("\nfor a dependency-free server and HTTP client, use the standard library's\n`http` and `net` modules — they need no C library at all.\n")
	return 0
}
