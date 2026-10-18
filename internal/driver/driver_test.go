package driver_test

import (
	"bytes"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zexi/internal/diag"
	"zexi/internal/driver"
)

// TestTypeCheckExamples ensures every shipped example that targets the
// supported subset type-checks cleanly.
func TestTypeCheckExamples(t *testing.T) {
	files := []string{
		"../../examples/hello.zx",
		"../../examples/easy.zx",
		"../../examples/cbridge.zx",
		"../../tests/tour.zx",
		"../../tests/structs.zx",
		"../../tests/enums.zx",
		"../../tests/arrays.zx",
		"../../tests/ownership.zx",
		"../../tests/strings.zx",
		"../../tests/drop.zx",
		"../../tests/string_methods.zx",
		"../../tests/ergonomics.zx",
		"../../tests/syntax.zx",
		"../../tests/pi.zx",
		"../../tests/match_guards.zx",
		"../../tests/robustness.zx",
		"../../tests/sugar.zx",
		"../../tests/keywords.zx",
		"../../tests/easy.zx",
	}
	for _, f := range files {
		em := diag.NewEmitter()
		if _, ok := driver.FrontEnd(f, driver.Options{}, em); !ok {
			t.Errorf("%s: front end reported errors", f)
		}
	}
}

// TestRunPrograms compiles and runs each program, asserting its output. It is
// skipped when no C compiler is available or in short mode.
func TestRunPrograms(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end build in short mode")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no C compiler available")
	}

	cases := []struct {
		file string
		want string
	}{
		{
			file: "../../tests/tour.zx",
			want: strings.Join([]string{
				"hello from zexi",
				"0 1 1 2 3 5 8 13 21 34 ",
				"55",
				"zero",
				"9000000000",
				"7",
				"true",
				"Z",
				"410065408",
				"",
			}, "\n"),
		},
		{
			file: "../../tests/structs.zx",
			want: strings.Join([]string{
				"25", "4", "6", "2.5", "3.5", "15", "0", "30", "2", "",
			}, "\n"),
		},
		{
			file: "../../tests/enums.zx",
			want: strings.Join([]string{
				"false", "true", "12.5664", "12", "0", "zero", "one", "many", "0", "7", "-1", "10", "",
			}, "\n"),
		},
		{
			file: "../../tests/arrays.zx",
			want: strings.Join([]string{
				"5", "15", "4", "0", "1", "100", "5", "32", "9", "",
			}, "\n"),
		},
		{
			file: "../../tests/ownership.zx",
			want: strings.Join([]string{
				"7", "7", "84", "",
			}, "\n"),
		},
		{
			file: "../../tests/strings.zx",
			want: strings.Join([]string{
				"4", "false", "zexi lang!", "zexi lang!", "Ada", "36",
				"Hello, world!", "zexi lang!!!", "zexi lang!", "alpha!", "beta",
				"second", "",
			}, "\n"),
		},
		{
			file: "../../tests/drop.zx",
			want: strings.Join([]string{
				"first", "second", "16", "left", "right", "7", "7", "moved", "",
			}, "\n"),
		},
		{
			file: "../../tests/string_methods.zx",
			want: strings.Join([]string{
				"3", "71", "-1", "true", "true", "true", "false", "/index.html",
				"/index.html HTTP/1.1", "Content-Length: 1527", "1527", "1528",
				"16", "",
			}, "\n"),
		},
		{
			file: "../../tests/ergonomics.zx",
			want: strings.Join([]string{
				"30", "5", "5", "total=30", "sum is total=30",
				"zexi: yes", "4", "4", "5", "",
			}, "\n"),
		},
		{
			file: "../../tests/syntax.zx",
			want: strings.Join([]string{
				"42", "14", "20", "3", "2", "16", "8", "14", "false", "true",
				"false", "negative", "zero", "positive", "-1", "120", "10",
				"3", "10", "300", "A", "Z", "true", "1", "2", "11", "N", "E",
				"3", "20", "4", "zexi!", "5", "no newline; hello, zexi", "100", "",
			}, "\n"),
		},
		{
			file: "../../tests/pi.zx",
			want: strings.Join([]string{
				"3.141592653589793238462643383279", "",
			}, "\n"),
		},
		{
			file: "../../tests/match_guards.zx",
			want: strings.Join([]string{
				"27", "0", "16", "15", "0",
				"zero", "negative", "big", "small", "",
			}, "\n"),
		},
		{
			file: "../../tests/robustness.zx",
			want: strings.Join([]string{
				"4", "5", "6", "xy", "xyxy", "box", "3", "boxed",
				"38", "-128", "-300", "é", "😀", "A", "",
			}, "\n"),
		},
		{
			file: "../../tests/sugar.zx",
			want: strings.Join([]string{
				"7", "0", "-1", "-1", "40", "kept 4",
				"point: 3 4", "flags true false 2.5",
				"n = 42", "42 items", "answer: true!",
				"count: 42 / 1.5", "empty", "yes", "z", "cold",
				"ratio: 1 3.5", "",
			}, "\n"),
		},
		{
			file: "../../tests/keywords.zx",
			want: strings.Join([]string{
				"A", "B", "C", "F", "4", "4",
				"block body", "deferred-1", "deferred-2", "xy",
				"early cleanup", "1", "no return", "early cleanup", "2",
				"body 1", "iter 1", "iter 2", "body 3", "iter 3",
				"42", "6", "3 4", "7", "bob 7",
				"zero", "small", "mid", "high", "huge", "10", "20",
				"deep freeze", "freezing", "ok",
				"lower", "upper", "digit", "other",
				"horizontal", "vertical", "5", "-1",
				"got 10", "none", "done", "30",
				`C:\new\test`, `say "hi" \n`, `a\tb`, "42", "",
			}, "\n"),
		},
		{
			file: "../../tests/easy.zx",
			want: strings.Join([]string{
				"hello, bob!", "hello, ada? x2", "hello, cy! x3",
				"50", "15", "8",
				"21", "welcome, zexi!",
				"10", "6", "1024", "ababab", "00042",
				"world #3 {ok}", "pi~3.5 ok=true grade=A",
				"42", "6", "3", "",
			}, "\n"),
		},
	}

	for _, tc := range cases {
		t.Run(filepath.Base(tc.file), func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "prog")
			em := diag.NewEmitter()
			if _, ok := driver.Build(tc.file, out, driver.Options{}, em); !ok {
				t.Fatal("build failed")
			}
			raw, err := exec.Command(out).Output()
			if err != nil {
				t.Fatalf("running %s: %v", out, err)
			}
			if got := string(raw); got != tc.want {
				t.Fatalf("output mismatch\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

// TestHTTPServerAndClient builds the Zexi HTTP library (lib/http.zx) into both
// a server and a client, runs the server on a loopback port, and checks that
// the client's parsed responses and the server's request log agree. It is
// skipped when no C compiler is available or in short mode.
func TestHTTPServerAndClient(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end network test in short mode")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no C compiler available")
	}

	dir := t.TempDir()
	lib := "../../lib/http.zx"
	srvBin := filepath.Join(dir, "http_server")
	cliBin := filepath.Join(dir, "http_client")

	em := diag.NewEmitter()
	if _, ok := driver.BuildFiles([]string{"../../tests/http_server.zx", lib}, srvBin, driver.Options{}, em); !ok {
		em.Render(os.Stderr, em.FirstFileID())
		t.Fatal("failed to build the HTTP server")
	}
	em = diag.NewEmitter()
	if _, ok := driver.BuildFiles([]string{"../../tests/http_client.zx", lib}, cliBin, driver.Options{}, em); !ok {
		em.Render(os.Stderr, em.FirstFileID())
		t.Fatal("failed to build the HTTP client")
	}

	var srvOut bytes.Buffer
	server := exec.Command(srvBin)
	server.Stdout = &srvOut
	server.Stderr = &srvOut
	if err := server.Start(); err != nil {
		t.Fatalf("starting server: %v", err)
	}
	defer server.Process.Kill()

	if !waitPort("127.0.0.1:18081", 5*time.Second) {
		t.Fatalf("server never started listening; output:\n%s", srvOut.String())
	}

	out, err := exec.Command(cliBin).Output()
	if err != nil {
		t.Fatalf("running client: %v", err)
	}
	wantClient := "200 index\n200 3.141592653589793238462643383279\n"
	if string(out) != wantClient {
		t.Fatalf("client output mismatch\n got: %q\nwant: %q", string(out), wantClient)
	}

	done := make(chan struct{})
	go func() { server.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not exit after serving two requests")
	}
	wantLog := "GET /\nGET /pi\n"
	if got := srvOut.String(); got != wantLog {
		t.Fatalf("server log mismatch\n got: %q\nwant: %q", got, wantLog)
	}
}

// TestHTTPExamplesTypeCheck ensures the HTTP examples compile against the
// library (no C compiler needed).
func TestHTTPExamplesTypeCheck(t *testing.T) {
	for _, f := range []string{
		"../../examples/http_server.zx",
		"../../examples/http_client.zx",
	} {
		em := diag.NewEmitter()
		if _, ok := driver.FrontEndFiles([]string{f, "../../lib/http.zx"}, driver.Options{}, em); !ok {
			em.Render(os.Stderr, em.FirstFileID())
			t.Errorf("%s + lib/http.zx: front end reported errors", f)
		}
	}
}

// waitPort reports whether addr accepts a TCP connection within d.
func waitPort(addr string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

// TestUseLoadsRelativeFile checks that `use "helper.zx";` pulls a sibling
// file into the program.
func TestUseLoadsRelativeFile(t *testing.T) {
	dir := t.TempDir()
	helper := "pub fn triple(n: i32) -> i32 { n * 3 }\n"
	main := "use \"helper.zx\";\nfn main() -> i32 { println(triple(7)); 0 }\n"
	if err := os.WriteFile(filepath.Join(dir, "helper.zx"), []byte(helper), 0o644); err != nil {
		t.Fatal(err)
	}
	mainPath := filepath.Join(dir, "main.zx")
	if err := os.WriteFile(mainPath, []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}
	em := diag.NewEmitter()
	if _, ok := driver.FrontEnd(mainPath, driver.Options{}, em); !ok {
		em.Render(os.Stderr, em.FirstFileID())
		t.Fatal("expected `use \"helper.zx\"` to load the library")
	}
}

// TestUseEmbeddedStdlib checks that `use math;` finds the standard library
// embedded in the compiler, from any directory.
func TestUseEmbeddedStdlib(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.zx")
	src := "use math;\nfn main() -> i32 { println(clampi(15, 0, 10), gcdi(12, 18)); 0 }\n"
	if err := os.WriteFile(mainPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	em := diag.NewEmitter()
	if _, ok := driver.FrontEnd(mainPath, driver.Options{}, em); !ok {
		em.Render(os.Stderr, em.FirstFileID())
		t.Fatal("expected `use math;` to load the embedded stdlib")
	}
}

// TestUseMissingLibrary checks that an unresolvable `use` is a clean
// diagnostic (E9022), not a crash.
func TestUseMissingLibrary(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.zx")
	src := "use nosuchlib;\nfn main() -> i32 { 0 }\n"
	if err := os.WriteFile(mainPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	em := diag.NewEmitter()
	if _, ok := driver.FrontEnd(mainPath, driver.Options{}, em); ok {
		t.Fatal("expected a diagnostic for a missing library")
	}
	var buf bytes.Buffer
	em.Render(&buf, em.FirstFileID())
	if !strings.Contains(buf.String(), "E9022") {
		t.Fatalf("expected E9022, got:\n%s", buf.String())
	}
}

// TestCBridge builds a Zexi program that calls C code: a glue .c file
// compiled in with Options.CFiles and libm linked with Options.LinkLibs.
// It exercises the whole bridge — extern declarations, `str` pass-through,
// `cstr`/`from_cstr` marshalling, and the extra link inputs.
func TestCBridge(t *testing.T) {
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no C compiler available")
	}
	dir := t.TempDir()
	glue := `#include <string.h>
int zx_add(int a, int b) { return a + b; }
int zx_exclaim_len(const char *s) { return (int)strlen(s) + 1; }
int zx_count_a(const char *s) { int n = 0; for (; *s; s++) { if (*s == 'a') n++; } return n; }
const char *zx_motto(void) { return "fast and small"; }
`
	main := `extern "C" fn zx_add(a: i32, b: i32) -> i32;
extern "C" fn zx_exclaim_len(s: str) -> i32;
extern "C" fn zx_count_a(s: *u8) -> i32;
extern "C" fn zx_motto() -> *u8;
extern "C" fn sqrt(x: f64) -> f64;
fn main() -> i32 {
    println(zx_add(20, 22));
    println(zx_exclaim_len("zexi"));
    let motto = from_cstr(zx_motto());
    println(motto);
    println(zx_count_a(cstr(motto)));
    println(sqrt(2.25));
    0
}
`
	gluePath := filepath.Join(dir, "glue.c")
	mainPath := filepath.Join(dir, "main.zx")
	if err := os.WriteFile(gluePath, []byte(glue), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mainPath, []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "prog")
	em := diag.NewEmitter()
	opts := driver.Options{CFiles: []string{gluePath}, LinkLibs: []string{"m"}}
	if _, ok := driver.Build(mainPath, out, opts, em); !ok {
		em.Render(os.Stderr, em.FirstFileID())
		t.Fatal("C bridge build failed")
	}
	raw, err := exec.Command(out).Output()
	if err != nil {
		t.Fatalf("running bridged program: %v", err)
	}
	want := "42\n5\nfast and small\n3\n1.5\n"
	if got := string(raw); got != want {
		t.Fatalf("output mismatch\n got: %q\nwant: %q", got, want)
	}
}
