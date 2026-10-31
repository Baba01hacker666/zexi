// Package registry is Zexi's module catalog: the set of C-backed packages a
// program can add with `zexic add <name>`.
//
// A package is one `.zx` file that carries everything the module needs — the
// `extern "C"` declarations, the `c """..."""` glue, and a Zexi-shaped API on
// top. Installing it copies that file into the project's `lib/` folder, which
// the loader already searches for named imports, so `use sqlite;` works with no
// manifest, no lockfile, and no compiler change. That is the same property
// docs/17-extending.md is built on: a capability is a library, not a compiler
// feature.
package registry

import (
	"embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

//go:embed pkgs/*.zx
var pkgFS embed.FS

// Package is one entry in the catalog.
type Package struct {
	// Name is what `zexic add` takes and what `use` imports.
	Name string
	// Summary is a one-line description for `zexic add` with no arguments.
	Summary string
	// Category groups related packages in the listing.
	Category string
	// Libs are the linker names (`-l<name>`) the module asks for. They are what
	// a missing development package shows up as at build time.
	Libs []string
	// Headers are the C headers the module's `#[include]` attributes need.
	Headers []string
	// Hint is the install command printed when a dependency is missing.
	Hint string

	source string
}

// Source returns the module's Zexi source.
func (p Package) Source() string { return p.source }

// catalog is every package `zexic add` can install. Keeping the list here (with
// the source embedded beside it) means the catalog ships with the compiler
// binary: `zexic add` works offline and never depends on a registry server.
var catalog = []Package{
	{
		Name:     "sqlite",
		Summary:  "embedded SQL database (libsqlite3)",
		Category: "storage",
		Libs:     []string{"sqlite3"},
		Headers:  []string{"sqlite3.h"},
		Hint:     "apt install libsqlite3-dev  |  dnf install sqlite-devel  |  brew install sqlite",
	},
	{
		Name:     "zlib",
		Summary:  "DEFLATE compress/inflate and crc32 (zlib)",
		Category: "data",
		Libs:     []string{"z"},
		Headers:  []string{"zlib.h"},
		Hint:     "apt install zlib1g-dev  |  dnf install zlib-devel  |  brew install zlib",
	},
	{
		Name:     "crypto",
		Summary:  "SHA-256/512/1, MD5, HMAC-SHA256 digests (libcrypto)",
		Category: "security",
		Libs:     []string{"crypto"},
		Headers:  []string{"openssl/evp.h", "openssl/hmac.h"},
		Hint:     "apt install libssl-dev  |  dnf install openssl-devel  |  brew install openssl",
	},
	{
		Name:     "curl",
		Summary:  "HTTP(S) client with TLS, redirects, and timeouts (libcurl)",
		Category: "network",
		Libs:     []string{"curl"},
		Headers:  []string{"curl/curl.h"},
		Hint:     "apt install libcurl4-openssl-dev  |  dnf install libcurl-devel  |  brew install curl",
	},
	{
		Name:     "microhttpd",
		Summary:  "native HTTP server daemon (libmicrohttpd)",
		Category: "network",
		Libs:     []string{"microhttpd"},
		Headers:  []string{"microhttpd.h"},
		Hint:     "apt install libmicrohttpd-dev  |  dnf install libmicrohttpd-devel  |  brew install libmicrohttpd",
	},
}

func init() {
	for i := range catalog {
		data, err := pkgFS.ReadFile("pkgs/" + catalog[i].Name + ".zx")
		if err != nil {
			// A missing embedded source is a build-time packaging bug, and the
			// catalog would lie about what it can install.
			panic(fmt.Sprintf("registry: embedded source for %q: %v", catalog[i].Name, err))
		}
		catalog[i].source = string(data)
	}
}

// All returns the catalog, sorted by category then name for a stable listing.
func All() []Package {
	out := append([]Package(nil), catalog...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Category != out[j].Category {
			return out[i].Category < out[j].Category
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Find returns the named package.
func Find(name string) (Package, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, p := range catalog {
		if p.Name == name {
			return p, true
		}
	}
	return Package{}, false
}

// ErrUnknown is returned when a name is not in the catalog.
var ErrUnknown = errors.New("unknown package")

// ErrExists is returned when installing would overwrite an existing file and
// force was not requested.
var ErrExists = errors.New("already installed")

// Install writes the package's module source into dir, which is normally the
// project's `lib/` folder. It creates the directory when needed and refuses to
// overwrite an existing file unless force is set, because a local copy is meant
// to be editable: silently replacing it would throw away the user's changes.
func Install(dir, name string, force bool) (string, Package, error) {
	p, ok := Find(name)
	if !ok {
		return "", Package{}, fmt.Errorf("%w: %q", ErrUnknown, name)
	}
	if dir == "" {
		return "", p, errors.New("no destination directory")
	}
	path := filepath.Join(dir, p.Name+".zx")
	if _, err := os.Stat(path); err == nil && !force {
		return path, p, fmt.Errorf("%w: %s (use --force to replace it)", ErrExists, path)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", p, err
	}
	if err := os.WriteFile(path, []byte(p.Source()), 0o644); err != nil {
		return "", p, err
	}
	return path, p, nil
}

// DefaultDir is where `zexic add` installs when no directory is given: the
// `lib/` folder of the project root, which the loader searches for named
// imports (see internal/driver/modules.go).
func DefaultDir(root string) string {
	if root == "" {
		root = "."
	}
	return filepath.Join(root, "lib")
}

// Deps reports whether the system has what a package needs.
type Deps struct {
	// Ready is true when every header and library was found.
	Ready bool
	// Checked is false when no C compiler was available to ask.
	Checked bool
	// MissingHeaders lists the headers the compiler could not find.
	MissingHeaders []string
	// MissingLibs lists the libraries the linker (or compiler) rejected.
	MissingLibs []string
	// Detail is the compiler's own first line of complaint, for -v style output.
	Detail string
}

// String renders a short status for a listing line.
func (d Deps) String() string {
	switch {
	case !d.Checked:
		return "unknown (no C compiler)"
	case d.Ready:
		return "ready"
	case len(d.MissingHeaders) > 0:
		return "needs " + strings.Join(append(d.MissingHeaders, prefixAll(d.MissingLibs, "lib")...), ", ")
	default:
		return "needs " + strings.Join(prefixAll(d.MissingLibs, "lib"), ", ")
	}
}

func prefixAll(items []string, prefix string) []string {
	if len(items) == 0 {
		return nil
	}
	out := make([]string, len(items))
	for i, s := range items {
		out[i] = prefix + s
	}
	return out
}

// probeCache memoises dependency probes, which shell out to the C compiler.
var (
	probeMu    sync.Mutex
	probeCache = map[string]Deps{}
)

// Probe asks the C compiler whether a package's headers and libraries are
// present, by compiling and linking a program that includes every header and
// names every library. Asking the toolchain is the only answer that matches what
// a real build will do — a header may exist in a path `cc` does not search, and
// a `.so` may exist without its development symlink.
func Probe(cc string, p Package) Deps {
	if cc == "" {
		cc = "cc"
	}
	key := cc + "\x00" + p.Name
	probeMu.Lock()
	if d, ok := probeCache[key]; ok {
		probeMu.Unlock()
		return d
	}
	probeMu.Unlock()

	d := probe(cc, p)

	probeMu.Lock()
	probeCache[key] = d
	probeMu.Unlock()
	return d
}

func probe(cc string, p Package) Deps {
	if _, err := exec.LookPath(cc); err != nil {
		return Deps{Checked: false}
	}
	dir, err := os.MkdirTemp("", "zexi-probe-")
	if err != nil {
		return Deps{Checked: false}
	}
	defer func() { _ = os.RemoveAll(dir) }()

	var src strings.Builder
	src.WriteString("#include <stddef.h>\n")
	for _, h := range p.Headers {
		fmt.Fprintf(&src, "#include <%s>\n", h)
	}
	src.WriteString("int main(void) { return 0; }\n")
	cfile := filepath.Join(dir, "probe.c")
	if err := os.WriteFile(cfile, []byte(src.String()), 0o644); err != nil {
		return Deps{Checked: false}
	}

	// Headers first: a missing header is the usual failure, and its message
	// names the file, which is friendlier than a link error about a library.
	compile := exec.Command(cc, "-c", cfile, "-o", filepath.Join(dir, "probe.o"))
	if out, err := compile.CombinedOutput(); err != nil {
		d := Deps{Checked: true, Detail: firstLine(out)}
		for _, h := range p.Headers {
			if strings.Contains(string(out), h) {
				d.MissingHeaders = append(d.MissingHeaders, h)
			}
		}
		if len(d.MissingHeaders) == 0 {
			// The compiler failed for some other reason; do not blame a header.
			d.MissingHeaders = append(d.MissingHeaders, p.Headers...)
		}
		d.MissingLibs = append([]string(nil), p.Libs...)
		return d
	}

	// Then link, so a library without its development symlink is reported as a
	// library problem rather than silently passing.
	args := []string{cfile, "-o", filepath.Join(dir, "probe")}
	for _, l := range p.Libs {
		args = append(args, "-l"+l)
	}
	link := exec.Command(cc, args...)
	if out, err := link.CombinedOutput(); err != nil {
		d := Deps{Checked: true, Detail: firstLine(out)}
		for _, l := range p.Libs {
			if strings.Contains(string(out), "l"+l) || strings.Contains(string(out), l) {
				d.MissingLibs = append(d.MissingLibs, l)
			}
		}
		if len(d.MissingLibs) == 0 {
			d.MissingLibs = append([]string(nil), p.Libs...)
		}
		return d
	}
	return Deps{Checked: true, Ready: true}
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// Suggest returns catalog names close to an unknown one, so a typo gets a hint
// (`zexic add sqlite3` → "did you mean sqlite?").
func Suggest(name string) []string {
	name = strings.ToLower(strings.TrimSpace(name))
	var out []string
	for _, p := range catalog {
		if strings.Contains(p.Name, name) || strings.Contains(name, p.Name) ||
			editDistance(name, p.Name) <= 2 {
			out = append(out, p.Name)
		}
	}
	return out
}

// editDistance is the Levenshtein distance, small enough to keep inline.
func editDistance(a, b string) int {
	if a == b {
		return 0
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, min(cur[j-1]+1, prev[j-1]+cost))
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
