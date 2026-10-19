package driver

// Project-level entry points and root discovery. A project is a directory of
// `.zx` files compiled from one entry: the entry imports the rest with
// `use`/`import`/`mod`, and the loader pulls in everything transitively. There
// is no manifest — conventions only.

import (
	"fmt"
	"os"
	"path/filepath"
)

// Conventional entry points, in the order they are tried when a directory is
// given to the compiler. `main.zx` is the simple layout; `src/main.zx` is the
// common one for measurements that keep generated or auxiliary files out of the
// source root.
var entryCandidates = []string{
	"main.zx",
	filepath.Join("src", "main.zx"),
}

// ResolveEntry turns a CLI input into an entry file and a project root. The
// input may be a `.zx` file or a directory. For a directory, `main.zx` is tried
// first, then `src/main.zx`; the project root is the directory itself. For a
// file, the root is its directory — or that directory's parent when it is named
// `src`, so `zexic run src/main.zx` still reaches modules at the project root.
func ResolveEntry(input string) (entry, root string, err error) {
	st, statErr := os.Stat(input)
	if statErr == nil && st.IsDir() {
		for _, cand := range entryCandidates {
			p := filepath.Join(input, cand)
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				return p, ProjectRoot(p, ""), nil
			}
		}
		return "", "", fmt.Errorf("no entry point in %s (looked for %s and %s)", input, entryCandidates[0], entryCandidates[1])
	}
	if statErr != nil {
		return "", "", statErr
	}
	return input, ProjectRoot(input, ""), nil
}

// ProjectRoot returns the directory named imports and `mod` declarations are
// searched from. An explicit root (Options.Root, `--root=`) wins; otherwise it
// is the entry file's directory, or that directory's parent when it is `src`.
func ProjectRoot(entry, explicit string) string {
	if explicit != "" {
		abs, err := filepath.Abs(explicit)
		if err != nil {
			return filepath.Clean(explicit)
		}
		return abs
	}
	abs, err := filepath.Abs(entry)
	if err != nil {
		abs = filepath.Clean(entry)
	}
	dir := filepath.Dir(abs)
	if filepath.Base(dir) == "src" {
		return filepath.Dir(dir)
	}
	return dir
}
