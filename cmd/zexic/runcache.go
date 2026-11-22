package main

// Warm-run cache for `zexic run`.
//
// `run` used to build a fresh binary in a temporary directory on every
// invocation, paying full C compilation even when nothing had changed. The
// cache here is content-addressed: the key hashes the compiler version, the
// effective options, every source file the loader actually read (entry points
// plus imports and `mod` files), and the C compiler's identity. A hit executes
// the stored binary directly; a miss builds once and atomically publishes the
// result.
//
// Program arguments are deliberately not part of the key: they change what the
// program does, not what it is.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"zexi/internal/driver"
)

// cacheKeep is how many run binaries are retained. The cache is a convenience,
// not a build artifact store, so it stays small.
const cacheKeep = 32

// runCacheDir returns the directory run binaries live in. ZEXI_CACHE_DIR
// overrides the OS cache location (useful for tests and for keeping a project's
// cache on a faster disk).
func runCacheDir() (string, bool) {
	if d := os.Getenv("ZEXI_CACHE_DIR"); d != "" {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", false
		}
		return d, true
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", false
	}
	d := filepath.Join(base, "zexi", "run")
	if err := os.MkdirAll(d, 0o755); err != nil {
		return "", false
	}
	return d, true
}

// runCacheKey hashes everything that determines the built binary. It returns
// false when an input cannot be read, in which case the caller builds normally
// instead of caching a guess.
func runCacheKey(files []string, opts driver.Options) (string, bool) {
	h := sha256.New()
	io.WriteString(h, "zexi-run-v1\x00")
	io.WriteString(h, versionString())
	fmt.Fprintf(h, "\x00opt=%s\x00cc=%s\x00backend=%s\x00root=%s\x00jobs=%d\x00",
		opts.OptLevel, opts.CC, opts.Backend, opts.Root, opts.Jobs)
	for _, list := range [][]string{opts.LinkLibs, opts.LibDirs, opts.IncludeDirs, opts.CFiles} {
		for _, v := range list {
			io.WriteString(h, v)
			io.WriteString(h, "\x00")
		}
		io.WriteString(h, "\x01")
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return "", false
		}
		fmt.Fprintf(h, "file=%s\x00len=%d\x00", f, len(b))
		h.Write(b)
	}
	// Extra C sources compiled with the program are inputs too, and their
	// path alone is not enough (editing one must invalidate the entry).
	for _, f := range opts.CFiles {
		b, err := os.ReadFile(f)
		if err != nil {
			return "", false
		}
		fmt.Fprintf(h, "csrc=%s\x00len=%d\x00", f, len(b))
		h.Write(b)
	}
	// The compiler binary's own identity: a source build's version string
	// records the commit and dirty flag, not the bytes of an uncommitted
	// rebuild, so two different compilers could otherwise share a cache.
	if exe, err := os.Executable(); err == nil {
		io.WriteString(h, "exe="+exe+"\x00")
		if st, err := os.Stat(exe); err == nil {
			fmt.Fprintf(h, "exestat=%d\x00%d\x00", st.Size(), st.ModTime().UnixNano())
		}
	}
	// The system C compiler changing underneath the cache would otherwise
	// leave a binary compiled by a different toolchain. Include its path and
	// stat identity, not just the user-supplied name.
	cc := opts.CC
	if cc == "" {
		cc = os.Getenv("CC")
	}
	if cc == "" {
		cc = "cc"
	}
	if p, err := exec.LookPath(cc); err == nil {
		io.WriteString(h, "ccpath="+p+"\x00")
		if st, err := os.Stat(p); err == nil {
			fmt.Fprintf(h, "ccstat=%d\x00%d\x00", st.Size(), st.ModTime().UnixNano())
		}
	} else {
		io.WriteString(h, "ccname="+cc+"\x00")
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

// cacheLookup returns the cached binary for key, if it is still there.
func cacheLookup(dir, key string) (string, bool) {
	p := filepath.Join(dir, key)
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() {
		return "", false
	}
	return p, true
}

// cacheStore publishes a just-built binary under key. The build already
// happened in a temporary directory on the same filesystem, so a rename makes
// the entry visible atomically; a concurrent process that published first wins
// harmlessly.
func cacheStore(dir, key, built string) (string, bool) {
	dst := filepath.Join(dir, key)
	if _, err := os.Stat(dst); err == nil {
		return dst, true
	}
	if err := os.Rename(built, dst); err != nil {
		if _, err := os.Stat(dst); err == nil {
			return dst, true
		}
		return "", false
	}
	pruneCache(dir)
	return dst, true
}

// pruneCache removes all but the newest cacheKeep binaries.
func pruneCache(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type entry struct {
		path string
		mod  time.Time
	}
	var files []entry
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, entry{filepath.Join(dir, e.Name()), info.ModTime()})
	}
	if len(files) <= cacheKeep {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	for _, f := range files[cacheKeep:] {
		_ = os.Remove(f.path)
	}
}
