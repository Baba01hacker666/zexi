package registry_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"zexi/internal/diag"
	"zexi/internal/driver"
	"zexi/internal/registry"
)

// TestCatalogIsComplete keeps the catalog honest: every entry has the metadata
// a listing needs, and every embedded source is the module it claims to be.
func TestCatalogIsComplete(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range registry.All() {
		if p.Name == "" || p.Summary == "" || p.Category == "" {
			t.Errorf("package %q has incomplete metadata: %+v", p.Name, p)
		}
		if seen[p.Name] {
			t.Errorf("package %q appears twice in the catalog", p.Name)
		}
		seen[p.Name] = true
		if len(p.Libs) == 0 {
			t.Errorf("package %q declares no library to link", p.Name)
		}
		if len(p.Headers) == 0 {
			t.Errorf("package %q declares no header, so its block cannot be self-contained", p.Name)
		}
		src := p.Source()
		if !strings.Contains(src, `#[link("`+p.Libs[0]+`")]`) {
			t.Errorf("package %q source does not ask for its library %q", p.Name, p.Libs[0])
		}
		for _, h := range p.Headers {
			if !strings.Contains(src, `#[include("`+h+`")]`) {
				t.Errorf("package %q source does not include its header %q", p.Name, h)
			}
		}
		if !strings.Contains(src, `c """`) {
			t.Errorf("package %q has no C block; every C library binding needs one", p.Name)
		}
	}
	if len(seen) < 4 {
		t.Errorf("catalog has only %d packages; the point is a useful starting set", len(seen))
	}
}

// TestCatalogSourcesTypeCheck compiles each embedded module as part of a small
// program. This is the guarantee that `zexic add` never installs a module that
// does not even parse: the embedded source is checked on every `go test`.
func TestCatalogSourcesTypeCheck(t *testing.T) {
	for _, p := range registry.All() {
		t.Run(p.Name, func(t *testing.T) {
			dir := t.TempDir()
			libDir := filepath.Join(dir, "lib")
			path, _, err := registry.Install(libDir, p.Name, false)
			if err != nil {
				t.Fatalf("install: %v", err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("installed module missing: %v", err)
			}
			main := "use " + p.Name + ";\n\n" + dispatchStub(p.Name) + "\nfn main() -> i32 { 0 }\n"
			mainPath := filepath.Join(dir, "main.zx")
			if err := os.WriteFile(mainPath, []byte(main), 0o644); err != nil {
				t.Fatal(err)
			}
			em := diag.NewEmitter()
			if _, ok := driver.FrontEnd(mainPath, driver.Options{CheckOnly: true}, em); !ok {
				var b strings.Builder
				em.Render(&b, em.FirstFileID())
				t.Fatalf("package %q does not type-check:\n%s", p.Name, b.String())
			}
		})
	}
}

// dispatchStub supplies the callback that a package expects the program to
// define. Only microhttpd needs one today; the switch keeps the test honest if
// another package adds a required hook.
func dispatchStub(name string) string {
	if name != "microhttpd" {
		return ""
	}
	return `#[no_mangle]
fn zxmhd_dispatch(method: str, path: str, body: str,
                  out: &mut [u8; 65536], cap: i32) -> i32 {
    out[0] = 111;
    out[1] = 107;
    out[2] = 0;
    200
}
`
}

// TestInstallRefusesOverwrite checks that a local copy is treated as the user's
// file: installing over it silently would discard their edits.
func TestInstallRefusesOverwrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "lib")
	path, _, err := registry.Install(dir, "sqlite", false)
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	if err := os.WriteFile(path, []byte("// edited by the user\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.Install(dir, "sqlite", false); err == nil {
		t.Fatal("second install succeeded; it must refuse to overwrite")
	} else if !strings.Contains(err.Error(), "already installed") {
		t.Fatalf("unhelpful error: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "edited by the user") {
		t.Fatal("the refused install still replaced the file")
	}
	if _, _, err := registry.Install(dir, "sqlite", true); err != nil {
		t.Fatalf("forced install: %v", err)
	}
	raw, _ = os.ReadFile(path)
	if strings.Contains(string(raw), "edited by the user") {
		t.Fatal("--force did not replace the file")
	}
}

// TestInstallUnknownPackage checks the typo path, which is the most common way
// a package manager is asked for something it does not have.
func TestInstallUnknownPackage(t *testing.T) {
	if _, _, err := registry.Install(t.TempDir(), "sqllite", false); err == nil {
		t.Fatal("installing a misspelling succeeded")
	}
	hints := registry.Suggest("sqlite3")
	if len(hints) == 0 || hints[0] != "sqlite" {
		t.Fatalf("Suggest(%q) = %v, want sqlite first", "sqlite3", hints)
	}
}

// TestProbeReadyWithRealToolchain builds a tiny library and header of its own
// and checks that Probe reports a ready package. This is the positive case the
// catalog cannot exercise on a machine without the development packages.
func TestProbeReadyWithRealToolchain(t *testing.T) {
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no C compiler available")
	}
	dir := t.TempDir()
	inc := filepath.Join(dir, "include")
	lib := filepath.Join(dir, "lib")
	for _, d := range []string{inc, lib} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(inc, "zxprobe.h"), []byte("int zxprobe(void);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "zxprobe.c")
	if err := os.WriteFile(src, []byte("int zxprobe(void) { return 0; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	so := filepath.Join(lib, "libzxprobe.so")
	out, err := exec.Command("cc", "-shared", "-fPIC", "-o", so, src).CombinedOutput()
	if err != nil {
		t.Skipf("cannot build a shared library here: %v\n%s", err, out)
	}

	t.Setenv("CPATH", inc)
	t.Setenv("LIBRARY_PATH", lib)

	pkg := registry.Package{
		Name:    "zxprobe",
		Libs:    []string{"zxprobe"},
		Headers: []string{"zxprobe.h"},
	}
	deps := registry.Probe("cc", pkg)
	if !deps.Checked {
		t.Fatal("Probe reported no compiler although cc is on PATH")
	}
	if !deps.Ready {
		t.Fatalf("Probe did not find the library and header it was given: %+v", deps)
	}

	// And a package whose header does not exist must be reported as missing
	// rather than assumed present.
	missing := registry.Probe("cc", registry.Package{
		Name:    "zxnotthere",
		Libs:    []string{"zxnotthere"},
		Headers: []string{"zxnotthere.h"},
	})
	if missing.Ready {
		t.Fatal("Probe claimed ready for a header and library that do not exist")
	}
	if len(missing.MissingHeaders) == 0 {
		t.Fatal("Probe did not name the missing header")
	}
}

// TestInstalledModulesBuildEndToEnd is the package manager's real contract: what
// `zexic add` writes must compile and run against the system library. Each case
// is skipped when that library's development files are absent, which is the
// usual state of a minimal container; CI installs them so these are gates there.
// `microhttpd` is not in the table because binding a TCP port in a test suite is
// a shared resource; its Zexi side is still type-checked by
// TestCatalogSourcesTypeCheck.
func TestInstalledModulesBuildEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end build in short mode")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no C compiler available")
	}

	cases := []struct {
		pkg  string
		main string
		want string
	}{
		{
			pkg: "sqlite",
			main: `use sqlite;

fn main() -> i32 {
    let mut db = match sqlite::memory() {
        Ok(d) => d,
        Err(e) => { eprintln(e); return 1; }
    };
    match db.exec("create table t(a text)") {
        Ok(_) => {},
        Err(e) => { eprintln(e); return 1; }
    };
    db.exec("insert into t values('hello')");
    match db.query("select a from t") {
        Ok(rows) => println(rows),
        Err(e) => eprintln(e),
    }
    db.close();
    0
}
`,
			want: "hello",
		},
		{
			pkg: "zlib",
			main: `use zlib;

fn main() -> i32 {
    let text = "zexi compresses this text";
    let mut packed: [u8; 128] = [0; 128];
    let n = match zlib::compress_at(text.as_bytes(), &mut packed) {
        Ok(n) => n,
        Err(e) => { eprintln(e); return 1; }
    };
    let mut plain: [u8; 128] = [0; 128];
    let m = match zlib::decompress(&packed[0..n], &mut plain) {
        Ok(m) => m,
        Err(e) => { eprintln(e); return 1; }
    };
    let mut back = String::new();
    back.push_bytes(&plain[0..m]);
    println(back == text);
    0
}
`,
			want: "true",
		},
		{
			pkg: "crypto",
			main: `use crypto;

fn main() -> i32 {
    println(crypto::sha256_hex("abc".as_bytes()));
    0
}
`,
			want: "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
		},
		{
			pkg: "curl",
			main: `use curl;

fn main() -> i32 {
    // file:// exercises the transfer path with no network and no server.
    let r = match curl::get_limit("file:///etc/hostname", 4096, 5000) {
        Ok(r) => r,
        Err(e) => { eprintln(e); return 1; }
    };
    println(len(r.body) > 0);
    0
}
`,
			want: "true",
		},
	}

	for _, tc := range cases {
		t.Run(tc.pkg, func(t *testing.T) {
			pkg, ok := registry.Find(tc.pkg)
			if !ok {
				t.Fatalf("%q is not in the catalog", tc.pkg)
			}
			if deps := registry.Probe("cc", pkg); !deps.Ready {
				t.Skipf("%s development files not installed (%s)", tc.pkg, deps.String())
			}

			dir := t.TempDir()
			if _, _, err := registry.Install(filepath.Join(dir, "lib"), tc.pkg, false); err != nil {
				t.Fatal(err)
			}
			mainPath := filepath.Join(dir, "main.zx")
			if err := os.WriteFile(mainPath, []byte(tc.main), 0o644); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(dir, "prog")
			em := diag.NewEmitter()
			if _, ok := driver.Build(mainPath, out, driver.Options{Root: dir}, em); !ok {
				var b strings.Builder
				em.Render(&b, em.FirstFileID())
				t.Fatalf("build failed:\n%s", b.String())
			}
			got, err := exec.Command(out).Output()
			if err != nil {
				t.Fatalf("running the program: %v", err)
			}
			if strings.TrimSpace(string(got)) != tc.want {
				t.Fatalf("program printed %q, want %q", got, tc.want)
			}
		})
	}
}
