package driver_test

import (
	"bytes"
	"io"
	"net"
	stdhttp "net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
		"../../examples/guess.zx",
		"../../examples/args.zx",
		"../../examples/http_app.zx",
		"../../examples/static_server.zx",
		"../../examples/regex.zx",
		"../../examples/local_tools.zx",
		"../../examples/math_tools.zx",
		"../../examples/result.zx",
		"../../examples/enum_match.zx",
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
		"../../tests/function_values.zx",
		"../../tests/keywords.zx",
		"../../tests/easy.zx",
		"../../tests/modules.zx",
		"../../tests/threads.zx",
		"../../tests/formatting.zx",
		"../../tests/encoding.zx",
		"../../tests/generics.zx",
		"../../tests/option_result.zx",
		"../../tests/vec.zx",
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
				"false", "true", "12.5664", "12", "0", "zero", "one", "many", "0", "7", "-1", "10",
				"1", "1", "kept", "true", "field", "red", "again", "",
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
			file: "../../tests/vec.zx",
			want: strings.Join([]string{
				"0 true 0", "3 false", "true", "10 20 30", "99 3",
				"10", "99", "30", "30 2", "10 1", "99 3 1 3", "100 4",
				"2", "true", "0 true", "6", "3 3", "4",
				"3 alpha gamma", "gamma 2", "alpha 1", "1 2 8", "",
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
			file: "../../tests/function_values.zx",
			want: strings.Join([]string{"42", "21", "15", ""}, "\n"),
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
		{
			file: "../../tests/modules.zx",
			want: strings.Join([]string{
				"14 10 9 42",
				"25",
				"",
			}, "\n"),
		},
		{
			file: "../../tests/threads.zx",
			want: strings.Join([]string{
				"square: 81",
				"sum: 19900",
				"total: 15",
				"counter: 4000",
				"",
			}, "\n"),
		},
		{
			file: "../../tests/formatting.zx",
			want: strings.Join([]string{
				"ff",
				"FF",
				"00ff",
				"-00ff",
				"1010",
				"100",
				"qglj",
				"11111111",
				"-8000000000000000",
				"1,234,567",
				"-1,000,000",
				"0",
				"1 234 567",
				"3.14",
				"3",
				"0.00",
				"0.000",
				"1.23e4",
				"4.200e-4",
				"1.0e1",
				"25.6%",
				"999 B",
				"1.5 KB",
				"123.5 MB",
				"250ms",
				"1m 30s",
				"1s 500ms",
				"1h 1m 1s",
				"1st 2nd 3rd 11th 21st 0th",
				"[ab   ]",
				"[   ab]",
				"[ ab  ]",
				"[abcd]",
				"",
			}, "\n"),
		},
		{
			file: "../../tests/encoding.zx",
			want: strings.Join([]string{
				"68656c6c6f",
				"68656C6C6F",
				"hello",
				"[]",
				"[]",
				"aGVsbG8=",
				"",
				"VGhlIHF1aWNrIGJyb3duIGZveCBqdW1wcyBvdmVyIHRoZSBsYXp5IGRvZw",
				"hello",
				"hello",
				"123456789",
				"00000000",
				"cbf43926",
				"414fa339",
				"00000001",
				"091e01de",
				"4f9f2cab",
				"a430d84680aabd0b",
				"06d5573923c6cdfc",
				"000000310f923099",
				"",
			}, "\n"),
		},
		{
			file: "../../tests/generics.zx",
			want: strings.Join([]string{
				"7",
				"7",
				"hey",
				"21",
				"1",
				"b",
				"42",
				"9",
				"7",
				"10",
				"true",
				"10",
				"5",
				"6",
				"4",
				"0",
				"12",
				"0",
				"",
			}, "\n"),
		},
		{
			file: "../../tests/option_result.zx",
			want: strings.Join([]string{
				"42",
				"oops",
				"1",
				"3",
				"oops",
				"6",
				"oops",
				"10",
				"-2",
				"0",
				"3",
				"",
			}, "\n"),
		},
		{
			file: "../../examples/result.zx",
			want: "42\n",
		},
		{
			file: "../../examples/enum_match.zx",
			want: "9.14159\n7\n",
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
	defer func() { _ = server.Process.Kill() }()

	if !waitPort("127.0.0.1:18081") {
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
	go func() { _ = server.Wait(); close(done) }()
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

// TestStdNetHTTPRoundTrip builds a server and a client against the embedded
// standard-library modules (`use http;` / `use std::http;`). Neither program
// names a C source file: the driver must recognise the import and link the
// embedded socket runtime (zxnet) by itself. The client also checks that a
// server can read a request head with Conn::read_head.
func TestStdNetHTTPRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end network test in short mode")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no C compiler available")
	}

	dir := t.TempDir()
	serverSrc := `use http;

fn main() -> i32 {
    let mut srv = listen_tcp("127.0.0.1", 18082);
    if srv.fd < 0 {
        println("bind failed");
        return 1;
    }
    let mut served: i32 = 0;
    while served < 3 {
        let mut c = srv.accept();
        if c.fd < 0 {
            continue;
        }
        let req = read_request(&c);
        if !req.valid() {
            c.close();
            continue;
        }
        print(req.method);
        print(" ");
        print(req.path);
        print(" ");
        println(req.body.len());
        let body = if req.path == "/pi" { "3.141592653589793238462643383279" } else { "index" };
        let resp = response(200, reason_phrase(200), body);
        c.write(resp);
        c.close();
        served += 1;
    }
    srv.close();
    0
}
`
	clientSrc := `use std::http;

fn main() -> i32 {
    let a = get_raw("127.0.0.1", 18082, "/");
    let ra = parse_response(&a);
    print(ra.status);
    print(" ");
    print(ra.body.trim());
    print(" ");
    println(ra.header("content-length"));

    let b = get_raw("127.0.0.1", 18082, "/pi");
    let rb = parse_response(&b);
    print(rb.status);
    print(" ");
    println(rb.body.trim());

    let p = post_raw("127.0.0.1", 18082, "/echo", "hello body");
    let rp = parse_response(&p);
    print(rp.status);
    print(" ");
    println(rp.body.trim());
    0
}
`
	serverPath := filepath.Join(dir, "server.zx")
	clientPath := filepath.Join(dir, "client.zx")
	if err := os.WriteFile(serverPath, []byte(serverSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(clientPath, []byte(clientSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	srvBin := filepath.Join(dir, "server")
	cliBin := filepath.Join(dir, "client")
	em := diag.NewEmitter()
	if _, ok := driver.Build(serverPath, srvBin, driver.Options{}, em); !ok {
		em.Render(os.Stderr, em.FirstFileID())
		t.Fatal("failed to build the stdlib HTTP server")
	}
	em = diag.NewEmitter()
	if _, ok := driver.Build(clientPath, cliBin, driver.Options{}, em); !ok {
		em.Render(os.Stderr, em.FirstFileID())
		t.Fatal("failed to build the stdlib HTTP client")
	}

	var srvOut bytes.Buffer
	server := exec.Command(srvBin)
	server.Stdout = &srvOut
	server.Stderr = &srvOut
	if err := server.Start(); err != nil {
		t.Fatalf("starting server: %v", err)
	}
	defer func() { _ = server.Process.Kill() }()

	if !waitPort("127.0.0.1:18082") {
		t.Fatalf("server never started listening; output:\n%s", srvOut.String())
	}

	out, err := exec.Command(cliBin).Output()
	if err != nil {
		t.Fatalf("running client: %v", err)
	}
	wantClient := "200 index 5\n200 3.141592653589793238462643383279\n200 index\n"
	if string(out) != wantClient {
		t.Fatalf("client output mismatch\n got: %q\nwant: %q", string(out), wantClient)
	}

	done := make(chan struct{})
	go func() { _ = server.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not exit after serving three requests")
	}
	if wantLog := "GET / 0\nGET /pi 0\nPOST /echo 10\n"; srvOut.String() != wantLog {
		t.Fatalf("server log mismatch\n got: %q\nwant: %q", srvOut.String(), wantLog)
	}
}

// TestStdHTTPClientReadsBodies checks that the client stops at the declared
// end of a body instead of waiting for the peer to close: one response is
// chunked, the other uses Content-Length. The raw server keeps each
// connection open for three seconds, so finishing quickly proves the client
// understood the body framing.
func TestStdHTTPClientReadsBodies(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end network test in short mode")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no C compiler available")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	payloads := []string{
		"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n6\r\n world\r\n0\r\n\r\n",
		"HTTP/1.1 200 OK\r\nContent-Length: 11\r\nContent-Type: text/plain\r\n\r\nhello world",
	}
	go func() {
		for _, payload := range payloads {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Serve concurrently: the connections stay open for a while, and
			// the client must open the second one without waiting for the first
			// to close.
			go func(conn net.Conn, payload string) {
				buf := make([]byte, 4096)
				total := 0
				for total < len(buf) {
					n, err := conn.Read(buf[total:])
					if err != nil {
						break
					}
					total += n
					if strings.Contains(string(buf[:total]), "\r\n\r\n") {
						break
					}
				}
				_, _ = conn.Write([]byte(payload))
				time.Sleep(3 * time.Second)
				_ = conn.Close()
			}(conn, payload)
		}
	}()

	dir := t.TempDir()
	clientPath := filepath.Join(dir, "client.zx")
	clientSrc := `use http;

fn main() -> i32 {
    let port = String::from(arg_at(1)).parse_i64() as i32;
    let a = get_raw("127.0.0.1", port, "/");
    let ra = parse_response(&a);
    print(ra.status);
    print(" [");
    print(ra.body);
    println("]");

    let b = get_raw("127.0.0.1", port, "/");
    let rb = parse_response(&b);
    print(rb.status);
    print(" [");
    print(rb.body);
    println("]");
    0
}
`
	if err := os.WriteFile(clientPath, []byte(clientSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	cliBin := filepath.Join(dir, "client")
	em := diag.NewEmitter()
	if _, ok := driver.Build(clientPath, cliBin, driver.Options{}, em); !ok {
		em.Render(os.Stderr, em.FirstFileID())
		t.Fatal("failed to build the HTTP client")
	}

	port := tcpPort(t, ln)
	start := time.Now()
	out, err := exec.Command(cliBin, strconv.Itoa(port)).Output()
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("running client: %v", err)
	}
	want := "200 [hello world]\n200 [hello world]\n"
	if string(out) != want {
		t.Fatalf("client output mismatch\n got: %q\nwant: %q", string(out), want)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("client waited %v; it should stop at the end of the body, not at EOF", elapsed)
	}
}

// TestStdHTTPHighLevelAPI exercises the urllib-style client (URL parsing,
// headers, redirects, query decoding, POST bodies, https rejection) against a
// server built from the Response builders and send_response.
func TestStdHTTPHighLevelAPI(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end network test in short mode")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no C compiler available")
	}

	dir := t.TempDir()
	serverSrc := `use http;

fn hello_handler(req: &Request) -> Response {
    println(req.path_only());
    let mut r = Response::html(200, "<h1>hello</h1>");
    r.set_header("X-Route", "hello");
    r.set_cookie("sid", "abc");
    return r;
}

fn old_handler(req: &Request) -> Response {
    println(req.path_only());
    return Response::redirect("/hello");
}

fn echo_handler(req: &Request) -> Response {
    println(req.path_only());
    let mut body = req.query("x");
    body.push_str(":");
    body.push_str(req.body.as_str());
    return Response::text(200, body.as_str());
}

fn cookie_handler(req: &Request) -> Response {
    println(req.path_only());
    let v = req.cookie("sid");
    return Response::text(200, v.as_str());
}

fn form_handler(req: &Request) -> Response {
    println(req.path_only());
    let v = req.param("name");
    return Response::text(200, v.as_str());
}

fn main() -> i32 {
    let mut srv = listen_local(18088);
    if !srv.ok() {
        println("bind failed");
        return 1;
    }
    let mut router = Router::new();
    router.get("/hello", hello_handler);
    router.get("/old", old_handler);
    router.post("/api/*", echo_handler);
    router.get("/cookie", cookie_handler);
    router.post("/form", form_handler);

    let mut served: i32 = 0;
    while served < 8 {
        let mut c = srv.accept();
        if !c.ok() {
            continue;
        }
        if router.serve(&c) {
            served += 1;
        }
        c.close();
    }
    srv.close();
    0
}
`
	clientSrc := `use http;

fn main() -> i32 {
    let mut c = Client::new();
    c.set_header("Cookie", "sid=abc");
    let a = c.get("http://127.0.0.1:18088/hello");
    print(a.status);
    print(" ");
    print(a.header("content-type"));
    print(" ");
    print(a.header("x-route"));
    print(" ");
    print(a.header("set-cookie"));
    print(" ");
    println(a.body);

    let b = fetch("http://127.0.0.1:18088/old");
    print(b.status);
    print(" ");
    println(b.body);

    let d = c.post("http://127.0.0.1:18088/api/echo?x=hi%20there", "body text");
    print(d.status);
    print(" ");
    println(d.body);

    let e = c.get("https://127.0.0.1:18088/hello");
    print(e.status);
    print(" ");
    println(e.error);

    let f = c.get("http://127.0.0.1:18088/cookie");
    print(f.status);
    print(" ");
    println(f.body);

    let g = c.post_form("http://127.0.0.1:18088/form", "name=zexi%20lang");
    print(g.status);
    print(" ");
    println(g.body);

    println(get_status("http://127.0.0.1:18088/hello"));
    let h = post_form_url("http://127.0.0.1:18088/form", "name=quick");
    print(h.status);
    print(" ");
    println(h.body);
    0
}
`
	serverPath := filepath.Join(dir, "server.zx")
	clientPath := filepath.Join(dir, "client.zx")
	if err := os.WriteFile(serverPath, []byte(serverSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(clientPath, []byte(clientSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	srvBin := filepath.Join(dir, "server")
	cliBin := filepath.Join(dir, "client")
	em := diag.NewEmitter()
	if _, ok := driver.Build(serverPath, srvBin, driver.Options{}, em); !ok {
		em.Render(os.Stderr, em.FirstFileID())
		t.Fatal("failed to build the high-level server")
	}
	em = diag.NewEmitter()
	if _, ok := driver.Build(clientPath, cliBin, driver.Options{}, em); !ok {
		em.Render(os.Stderr, em.FirstFileID())
		t.Fatal("failed to build the high-level client")
	}

	var srvOut bytes.Buffer
	server := exec.Command(srvBin)
	server.Stdout = &srvOut
	server.Stderr = &srvOut
	if err := server.Start(); err != nil {
		t.Fatalf("starting server: %v", err)
	}
	defer func() { _ = server.Process.Kill() }()

	if !waitPort("127.0.0.1:18088") {
		t.Fatalf("server never started listening; output:\n%s", srvOut.String())
	}

	out, err := exec.Command(cliBin).Output()
	if err != nil {
		t.Fatalf("running client: %v", err)
	}
	want := "200 text/html; charset=utf-8 hello sid=abc <h1>hello</h1>\n" +
		"200 <h1>hello</h1>\n" +
		"200 hi there:body text\n" +
		"0 https is not supported (this client is plaintext HTTP only)\n" +
		"200 abc\n" +
		"200 zexi lang\n" +
		"200\n" +
		"200 quick\n"
	if string(out) != want {
		t.Fatalf("client output mismatch\n got: %q\nwant: %q", string(out), want)
	}

	done := make(chan struct{})
	go func() { _ = server.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not exit after serving eight requests")
	}
	if wantLog := "/hello\n/old\n/hello\n/api/echo\n/cookie\n/form\n/hello\n/form\n"; srvOut.String() != wantLog {
		t.Fatalf("server log mismatch\n got: %q\nwant: %q", srvOut.String(), wantLog)
	}
}

// TestStdHTTPStaticFiles serves a directory with serve_static and checks it
// with Go's HTTP client: MIME types, exact binary bytes, 404 fallback, and
// rejection of encoded path traversal.
func TestStdHTTPStaticFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end network test in short mode")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no C compiler available")
	}

	dir := t.TempDir()
	web := filepath.Join(dir, "web")
	if err := os.MkdirAll(web, 0o755); err != nil {
		t.Fatal(err)
	}
	index := []byte("<h1>index</h1>")
	blob := make([]byte, 256)
	for i := range blob {
		blob[i] = byte(i)
	}
	if err := os.WriteFile(filepath.Join(web, "index.html"), index, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(web, "blob.bin"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A symlink inside the served tree must not expose the file it points to
	// outside the tree.
	if err := os.Symlink(filepath.Join(dir, "secret.txt"), filepath.Join(web, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	serverSrc := `use http;

fn main() -> i32 {
    let root = String::from(arg_at(1));
    let mut srv = listen_tcp("127.0.0.1", 18089);
    if !srv.ok() {
        println("bind failed");
        return 1;
    }
    let mut served: i32 = 0;
    while served < 5 {
        let mut c = srv.accept();
        if !c.ok() {
            continue;
        }
        let req = read_request(&c);
        if !req.valid() {
            c.close();
            continue;
        }
        let r = serve_static(root.as_str(), req.path_only().as_str());
        send_response(&c, &r);
        c.close();
        served += 1;
    }
    srv.close();
    0
}
`
	serverPath := filepath.Join(dir, "server.zx")
	if err := os.WriteFile(serverPath, []byte(serverSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	srvBin := filepath.Join(dir, "server")
	em := diag.NewEmitter()
	if _, ok := driver.Build(serverPath, srvBin, driver.Options{}, em); !ok {
		em.Render(os.Stderr, em.FirstFileID())
		t.Fatal("failed to build the static-file server")
	}

	var srvOut bytes.Buffer
	server := exec.Command(srvBin, web)
	server.Stdout = &srvOut
	server.Stderr = &srvOut
	if err := server.Start(); err != nil {
		t.Fatalf("starting server: %v", err)
	}
	defer func() { _ = server.Process.Kill() }()

	if !waitPort("127.0.0.1:18089") {
		t.Fatalf("server never started listening; output:\n%s", srvOut.String())
	}

	base := "http://127.0.0.1:18089"
	get := func(path string) (int, string, []byte) {
		resp, err := stdhttp.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		return resp.StatusCode, resp.Header.Get("Content-Type"), body
	}

	if status, ctype, body := get("/index.html"); status != 200 || ctype != "text/html; charset=utf-8" || !bytes.Equal(body, index) {
		t.Fatalf("index: status=%d type=%q body=%q", status, ctype, body)
	}
	if status, ctype, body := get("/blob.bin"); status != 200 || ctype != "application/octet-stream" || !bytes.Equal(body, blob) {
		t.Fatalf("blob: status=%d type=%q len=%d identical=%v", status, ctype, len(body), bytes.Equal(body, blob))
	}
	if status, _, _ := get("/missing.txt"); status != 404 {
		t.Fatalf("missing file: status=%d, want 404", status)
	}
	if status, _, _ := get("/%2e%2e/secret.txt"); status != 403 {
		t.Fatalf("traversal: status=%d, want 403", status)
	}
	if status, _, body := get("/link.txt"); status != 403 {
		t.Fatalf("symlink escape: status=%d body=%q, want 403", status, body)
	}

	done := make(chan struct{})
	go func() { _ = server.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not exit after serving five requests")
	}
}

// TestParallelCUnitsMatchSingleFile checks that the multi-unit (parallel cc)
// build path produces byte-identical program output to the single-file path.
func TestParallelCUnitsMatchSingleFile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end build in short mode")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no C compiler available")
	}

	for _, file := range []string{"../../tests/pi.zx", "../../tests/strings.zx", "../../tests/threads.zx"} {
		t.Run(filepath.Base(file), func(t *testing.T) {
			dir := t.TempDir()
			outputs := make([][]byte, 2)
			for i, jobs := range []int{1, 4} {
				out := filepath.Join(dir, "prog"+strconv.Itoa(jobs))
				em := diag.NewEmitter()
				if _, ok := driver.Build(file, out, driver.Options{Jobs: jobs}, em); !ok {
					em.Render(os.Stderr, em.FirstFileID())
					t.Fatalf("build with jobs=%d failed", jobs)
				}
				raw, err := exec.Command(out).Output()
				if err != nil {
					t.Fatalf("running jobs=%d binary: %v", jobs, err)
				}
				outputs[i] = raw
			}
			if string(outputs[0]) != string(outputs[1]) {
				t.Fatalf("outputs differ\njobs=1: %q\njobs=4: %q", outputs[0], outputs[1])
			}
		})
	}
}

// tcpPort returns the TCP port a listener is bound to.
func tcpPort(t *testing.T, ln net.Listener) int {
	t.Helper()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address is %T, want *net.TCPAddr", ln.Addr())
	}
	return addr.Port
}

// waitPort reports whether addr accepts a TCP connection within the startup
// timeout used by the integration tests.
func waitPort(addr string) bool {
	const timeout = 5 * time.Second
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
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

// TestProgramArgs builds a program that echoes its command line through
// arg_count()/arg_at(i) and checks the output.
func TestProgramArgs(t *testing.T) {
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no C compiler available")
	}
	out := filepath.Join(t.TempDir(), "prog")
	em := diag.NewEmitter()
	if _, ok := driver.Build("../../examples/args.zx", out, driver.Options{}, em); !ok {
		em.Render(os.Stderr, em.FirstFileID())
		t.Fatal("args build failed")
	}
	raw, err := exec.Command(out, "hello", "world").Output()
	if err != nil {
		t.Fatalf("running args program: %v", err)
	}
	want := "program: " + out + "\narg 1: hello\narg 2: world\n"
	if got := string(raw); got != want {
		t.Fatalf("output mismatch\n got: %q\nwant: %q", got, want)
	}
}

// TestReadLine builds a program against the stdlib `io` module and feeds
// it a line on stdin.
func TestReadLine(t *testing.T) {
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no C compiler available")
	}
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.zx")
	src := `use io;
fn main() -> i32 {
    let line = input("prompt> ");
    println(f"got: {line} ({len(line)})");
    0
}
`
	if err := os.WriteFile(mainPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "prog")
	em := diag.NewEmitter()
	if _, ok := driver.Build(mainPath, out, driver.Options{}, em); !ok {
		em.Render(os.Stderr, em.FirstFileID())
		t.Fatal("read_line build failed")
	}
	cmd := exec.Command(out)
	cmd.Stdin = strings.NewReader("hello zexi\nignored\n")
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("running read_line program: %v", err)
	}
	if want := "prompt> got: hello zexi (10)\n"; string(raw) != want {
		t.Fatalf("output mismatch\n got: %q\nwant: %q", string(raw), want)
	}
}

// TestImportFacadesAndModLayout exercises the M1-oriented source organization
// layer: package-shaped imports, a local module file, a directory module, and
// an inline module all flatten into the current program namespace.
func TestImportFacadesAndModLayout(t *testing.T) {
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no C compiler available")
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"helper.zx": `fn triple(n: i32) -> i32 { n * 3 }
`,
		"nested/mod.zx": `fn seven() -> i32 { 7 }
`,
		"main.zx": `import math.basic;
import text.ascii;
import text.regex;
import io.fs;
import os;
import random;
import net.tcp;
import http.client;
mod helper;
mod nested;
mod inline { fn add(a: i32, b: i32) -> i32 { a + b } }

fn main() -> i32 {
    println(triple(14), seven(), add(2, 3));
    println(abs(sin(PI / 2.0) - 1.0) < 0.00001);
    println(to_upper("zexi"));
    println(upper("hi"), lower("BYE"), strip("  zexi  "), contains("zexi", "ex"), is_blank(" \t"), is_match("[0-9]+", "v42"));
    0
}
`,
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mainPath := filepath.Join(dir, "main.zx")
	out := filepath.Join(dir, "prog")
	em := diag.NewEmitter()
	if _, ok := driver.Build(mainPath, out, driver.Options{}, em); !ok {
		em.Render(os.Stderr, em.FirstFileID())
		t.Fatal("failed to build import/module program")
	}
	raw, err := exec.Command(out).Output()
	if err != nil {
		t.Fatalf("running import/module program: %v", err)
	}
	if want := "42 7 5\ntrue\nZEXI\nHI bye zexi true true true\n"; string(raw) != want {
		t.Fatalf("output mismatch\n got: %q\nwant: %q", string(raw), want)
	}
}

// TestStdFSBinaryRoundTrip proves that the small filesystem layer brings in
// the shared C shim automatically and preserves bytes after an embedded NUL.
func TestStdFSBinaryRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end filesystem test in short mode")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no C compiler available")
	}
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.zx")
	src := `import io.fs;

fn main() -> i32 {
    let source = arg_at(1);
    let target = arg_at(2);
    let mut data = String::from("hi");
    data.push_byte(0);
    data.push_str("there");
    println(write_bytes(source.as_str(), &data));
    println(copy_file(source.as_str(), target.as_str()));
    let got = read_bytes(target.as_str());
    println(got.len(), got.byte_at(2), got.byte_at(7));
    0
}
`
	if err := os.WriteFile(mainPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "prog")
	em := diag.NewEmitter()
	if _, ok := driver.Build(mainPath, out, driver.Options{}, em); !ok {
		em.Render(os.Stderr, em.FirstFileID())
		t.Fatal("failed to build filesystem program")
	}
	source := filepath.Join(dir, "source.bin")
	target := filepath.Join(dir, "target.bin")
	raw, err := exec.Command(out, source, target).Output()
	if err != nil {
		t.Fatalf("running filesystem program: %v", err)
	}
	if want := "8\n8\n8 0 101\n"; string(raw) != want {
		t.Fatalf("output mismatch\n got: %q\nwant: %q", string(raw), want)
	}
	wantBytes := []byte{'h', 'i', 0, 't', 'h', 'e', 'r', 'e'}
	gotBytes, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotBytes, wantBytes) {
		t.Fatalf("copied bytes mismatch\n got: %v\nwant: %v", gotBytes, wantBytes)
	}
}

// TestHighLevelHTTPServeN validates the compact loopback-only
// `import http.server` surface. A readiness TCP probe is deliberately sent
// before the two HTTP requests; it must not consume serve_local_n's budget.
func TestHighLevelHTTPServeN(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end network test in short mode")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no C compiler available")
	}
	reserve, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := tcpPort(t, reserve)
	if err := reserve.Close(); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	mainPath := filepath.Join(dir, "server.zx")
	src := `import http.server;

fn hello(req: &Request) -> Response {
    return Response::text(200, "hello from serve_n");
}

fn main() -> i32 {
    let port = arg_at(1).parse_i64() as i32;
    let mut app = Router::new();
    app.get("/", hello);
    serve_local_n(&app, 2, port)
}
`
	if err := os.WriteFile(mainPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	serverBin := filepath.Join(dir, "server")
	em := diag.NewEmitter()
	if _, ok := driver.Build(mainPath, serverBin, driver.Options{}, em); !ok {
		em.Render(os.Stderr, em.FirstFileID())
		t.Fatal("failed to build high-level server")
	}

	server := exec.Command(serverBin, strconv.Itoa(port))
	var serverOut bytes.Buffer
	server.Stdout = &serverOut
	server.Stderr = &serverOut
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Process.Kill() }()
	addr := "127.0.0.1:" + strconv.Itoa(port)
	if !waitPort(addr) {
		t.Fatalf("server never started listening; output:\n%s", serverOut.String())
	}

	for i := range 2 {
		resp, err := stdhttp.Get("http://" + addr + "/")
		if err != nil {
			t.Fatalf("GET %d: %v", i, err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("reading GET %d: %v", i, err)
		}
		if resp.StatusCode != stdhttp.StatusOK || string(body) != "hello from serve_n" {
			t.Fatalf("GET %d: status=%d body=%q", i, resp.StatusCode, body)
		}
	}

	done := make(chan struct{})
	go func() { _ = server.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("serve_n did not exit after its request budget; output:\n%s", serverOut.String())
	}
	if got := serverOut.String(); got != "" {
		t.Fatalf("unexpected server output: %q", got)
	}
}

// TestDefaultMuxAndGoClient exercises the Go-flavoured surface: top-level
// handle_get/listen on a process-global mux, `{name}` route parameters, after
// middleware, and the one-call client (`get`, `do`).
func TestDefaultMuxAndGoClient(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end network test in short mode")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no C compiler available")
	}
	reserve, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := tcpPort(t, reserve)
	if err := reserve.Close(); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	serverSrc := `use http;

fn home(req: &Request) -> Response {
    return Response::text(200, "home");
}

fn user(req: &Request) -> Response {
    return Response::text(200, req.param("id").as_str());
}

fn cors(req: &Request, res: &mut Response) {
    res.set_header("X-Mux", "zexi");
}

fn main() -> i32 {
    let mut addr = String::from("127.0.0.1:");
    addr.push_str(arg_at(1).as_str());
    http::handle_get("/", home);
    http::handle_get("/users/{id}", user);
    http::use_after(cors);
    http::listen_n(addr.as_str(), 2)
}
`
	clientSrc := `use http;

fn main() -> i32 {
    let mut base = String::from("http://127.0.0.1:");
    base.push_str(arg_at(1).as_str());
    let mut one = base.clone();
    one.push_str("/users/42");
    let a = http::get(one.as_str());
    println(a.status, a.body.as_str(), a.header("X-Mux").as_str());
    let mut two = base.clone();
    two.push_str("/");
    let b = http::do("GET", two.as_str());
    println(b.status, b.body.as_str());
    0
}
`
	serverPath := filepath.Join(dir, "server.zx")
	clientPath := filepath.Join(dir, "client.zx")
	if err := os.WriteFile(serverPath, []byte(serverSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(clientPath, []byte(clientSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	serverBin := filepath.Join(dir, "server")
	clientBin := filepath.Join(dir, "client")
	em := diag.NewEmitter()
	if _, ok := driver.Build(serverPath, serverBin, driver.Options{}, em); !ok {
		em.Render(os.Stderr, em.FirstFileID())
		t.Fatal("failed to build default-mux server")
	}
	em = diag.NewEmitter()
	if _, ok := driver.Build(clientPath, clientBin, driver.Options{}, em); !ok {
		em.Render(os.Stderr, em.FirstFileID())
		t.Fatal("failed to build Zexi HTTP client")
	}

	server := exec.Command(serverBin, strconv.Itoa(port))
	var serverOut bytes.Buffer
	server.Stdout = &serverOut
	server.Stderr = &serverOut
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Process.Kill() }()
	if !waitPort("127.0.0.1:" + strconv.Itoa(port)) {
		t.Fatalf("server never started listening; output:\n%s", serverOut.String())
	}

	clientOut, err := exec.Command(clientBin, strconv.Itoa(port)).CombinedOutput()
	if err != nil {
		t.Fatalf("client failed: %v\n%s", err, clientOut)
	}
	want := "200 42 zexi\n200 home\n"
	if string(clientOut) != want {
		t.Fatalf("client output mismatch:\n got %q\nwant %q", clientOut, want)
	}
}

// TestThreadEdgeCases pins the deterministic corners of the concurrency
// primitives: non-blocking receive, close waking a blocked receiver, and
// stale/invalid handles failing cleanly.
func TestThreadEdgeCases(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end build in short mode")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no C compiler available")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "edge.zx")
	src := `use thread;

fn waiter(ch: i64) -> i64 {
    let mut v: i64 = 0;
    if chan_recv(ch, &mut v) {
        return v;
    }
    return -1;
}

fn main() -> i32 {
    let ch = Chan::new(1);
    let mut v: i64 = 7;
    println(chan_try_recv(ch.id(), &mut v), v);
    chan_send(ch.id(), 99);
    println(chan_try_recv(ch.id(), &mut v), v);
    println(chan_len(ch.id()));

    let t = Thread::spawn(waiter, ch.id());
    chan_close(ch.id());
    println(t.join());

    println(atomic_get(0), join(0));
    0
}
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "edge")
	em := diag.NewEmitter()
	if _, ok := driver.Build(path, out, driver.Options{}, em); !ok {
		em.Render(os.Stderr, em.FirstFileID())
		t.Fatal("failed to build concurrency edge-case program")
	}
	raw, err := exec.Command(out).Output()
	if err != nil {
		t.Fatalf("running edge-case program: %v", err)
	}
	want := "false 7\ntrue 99\n0\n-1\n0 0\n"
	if got := string(raw); got != want {
		t.Fatalf("output mismatch\n got: %q\nwant: %q", got, want)
	}
}

// TestStdMathConvenience exercises the high-level dependency-free numerical
// surface: conventional aliases, transcendental helpers, geometry, domain
// fallbacks, and floating-point classification.
func TestStdMathConvenience(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end math test in short mode")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no C compiler available")
	}
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.zx")
	src := `import math;

fn main() -> i32 {
    println(sqrt(81.0) == 9.0);
    println(approx_eq(hypot(3.0, 4.0), 5.0, 0.0000001));
    println(pow(2.0, 10) == 1024.0);
    println(approx_eq(sin(PI / 2.0), 1.0, 0.0000001));
    println(approx_eq(exp(1.0), E, 0.000000000001), approx_eq(log(E), 1.0, 0.000000000001), approx_eq(log2(8.0), 3.0, 0.000000000001), approx_eq(log10(1000.0), 3.0, 0.000000000001), approx_eq(log_base(81.0, 3.0), 4.0, 0.000000000001), approx_eq(exp(ln(0.5)), 0.5, 0.000000000001));
    println(approx_eq(powf(9.0, 0.5), 3.0, 0.000000000001), approx_eq(powf(-2.0, 3.0), -8.0, 0.000000000001), powf(-2.0, 0.5) == 0.0, powf(MAX_F64, 0.0) == 1.0);
    println(approx_eq(atan(1.0), FRAC_PI_4, 0.000000000001), approx_eq(atan2(1.0, 1.0), FRAC_PI_4, 0.000000000001), approx_eq(atan2(-1.0, -1.0), 0.0 - 3.0 * FRAC_PI_4, 0.000000000001), approx_eq(asin(1.0), FRAC_PI_2, 0.000000000001), approx_eq(asin(0.5), PI / 6.0, 0.000000000001), approx_eq(acos(-1.0), PI, 0.000000000001));
    println(approx_eq(radians(180.0), PI, 0.000000000001), approx_eq(degrees(PI), 180.0, 0.000000000001), approx_eq(sinh(0.0), 0.0, 0.000000000001), approx_eq(cosh(0.0), 1.0, 0.000000000001), approx_eq(tanh(1.0), 0.7615941559557649, 0.000000000001), sinh(0.0 - 1000.0) == 0.0 - sinh(1000.0), cosh(0.0 - 1000.0) == cosh(1000.0));
    println(approx_eq(cbrt(-125.0), -5.0, 0.000000000001), approx_eq(root(81.0, 4), 3.0, 0.000000000001), smoothstep(0.0, 10.0, 5.0) == 0.5, smootherstep(0.0, 10.0, 5.0) == 0.5);
    println(approx_eq(distance(0.0, 0.0, 3.0, 4.0), 5.0, 0.000000000001), distance_sq(0.0, 0.0, 3.0, 4.0) == 25.0, fract(-2.75) == -0.75, copysign(2.0, -1.0) == -2.0, sign(-0.5) == -1.0, saturate(3.0) == 1.0);
    let inf = MAX_F64 * 2.0;
    println(is_nan(1.0), is_finite(MAX_F64), is_infinite(inf), is_finite(inf));
    println(div_floor(1, 0), mod_floor(1, 0), div_ceil(1, 0), powi(2, -1), pow_mod(2, -1, 7));
    println(floor(9223372036854775808.0) == 9223372036854775808.0);
    println(trunc(0.0 - 9223372036854775808.0) == 0.0 - 9223372036854775808.0);
    println(factorial(10), choose(10, 3), is_prime(97), next_prime(100), next_prime(9223372036854775807));
    0
}
`
	if err := os.WriteFile(mainPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "prog")
	em := diag.NewEmitter()
	if _, ok := driver.Build(mainPath, out, driver.Options{}, em); !ok {
		em.Render(os.Stderr, em.FirstFileID())
		t.Fatal("failed to build high-level math program")
	}
	raw, err := exec.Command(out).Output()
	if err != nil {
		t.Fatalf("running high-level math program: %v", err)
	}
	if want := "true\ntrue\ntrue\ntrue\ntrue true true true true true\ntrue true true true\ntrue true true true true true\ntrue true true true true true true\ntrue true true true\ntrue true true true true true\nfalse true true false\n0 0 0 0 0\ntrue\ntrue\n3628800 120 true 101 0\n"; string(raw) != want {
		t.Fatalf("output mismatch\n got: %q\nwant: %q", string(raw), want)
	}
}

// TestMissingModIsDiagnostic makes sure the M1-preview module loader reports a
// useful error instead of passing an unresolved ModDecl to semantic analysis.
func TestMissingModIsDiagnostic(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.zx")
	if err := os.WriteFile(mainPath, []byte("mod missing;\nfn main() -> i32 { 0 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	em := diag.NewEmitter()
	if _, ok := driver.FrontEnd(mainPath, driver.Options{}, em); ok {
		t.Fatal("expected a diagnostic for a missing module")
	}
	var buf bytes.Buffer
	em.Render(&buf, em.FirstFileID())
	if !strings.Contains(buf.String(), "E9023") {
		t.Fatalf("expected E9023, got:\n%s", buf.String())
	}
}

// TestHighLevelStaticServeDirN covers the high-level loopback static-server
// loop rather than only its lower-level serve_static building block.
func TestHighLevelStaticServeDirN(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end network test in short mode")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no C compiler available")
	}
	reserve, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := tcpPort(t, reserve)
	if err := reserve.Close(); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	web := filepath.Join(dir, "web")
	if err := os.Mkdir(web, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(web, "index.html"), []byte("index via serve_dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(web, "hello.txt"), []byte("hello static"), 0o644); err != nil {
		t.Fatal(err)
	}
	mainPath := filepath.Join(dir, "server.zx")
	src := `import http.server;

fn main() -> i32 {
    let root = arg_at(1);
    let port = arg_at(2).parse_i64() as i32;
    serve_dir_local_n(root.as_str(), 2, port)
}
`
	if err := os.WriteFile(mainPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	serverBin := filepath.Join(dir, "server")
	em := diag.NewEmitter()
	if _, ok := driver.Build(mainPath, serverBin, driver.Options{}, em); !ok {
		em.Render(os.Stderr, em.FirstFileID())
		t.Fatal("failed to build high-level static server")
	}

	server := exec.Command(serverBin, web, strconv.Itoa(port))
	var serverOut bytes.Buffer
	server.Stdout = &serverOut
	server.Stderr = &serverOut
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Process.Kill() }()
	addr := "127.0.0.1:" + strconv.Itoa(port)
	if !waitPort(addr) {
		t.Fatalf("server never started listening; output:\n%s", serverOut.String())
	}
	for _, tc := range []struct {
		path string
		want string
	}{
		{"/", "index via serve_dir"},
		{"/hello.txt", "hello static"},
	} {
		resp, err := stdhttp.Get("http://" + addr + tc.path)
		if err != nil {
			t.Fatalf("GET %s: %v", tc.path, err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("reading %s: %v", tc.path, err)
		}
		if resp.StatusCode != stdhttp.StatusOK || string(body) != tc.want {
			t.Fatalf("GET %s: status=%d body=%q", tc.path, resp.StatusCode, body)
		}
	}

	done := make(chan struct{})
	go func() { _ = server.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("serve_dir_n did not exit after its request budget; output:\n%s", serverOut.String())
	}
	if got := serverOut.String(); got != "" {
		t.Fatalf("unexpected server output: %q", got)
	}
}

// TestProjectDirectoryEntry covers convention-only project builds: a directory
// entry (`main.zx` or `src/main.zx`), a project root derived from it, and named
// imports that reach modules at the root from a file in `src/`.
func TestProjectDirectoryEntry(t *testing.T) {
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no C compiler available")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"util.zx":     "pub fn double(n: i32) -> i32 { n * 2 }\n",
		"src/main.zx": "use util;\nfn main() -> i32 { println(util::double(21)); 0 }\n",
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	entry, root, err := driver.ResolveEntry(dir)
	if err != nil {
		t.Fatalf("ResolveEntry(%s): %v", dir, err)
	}
	if want := filepath.Join(dir, "src", "main.zx"); entry != want {
		t.Fatalf("entry = %q, want %q", entry, want)
	}
	if root != dir {
		t.Fatalf("root = %q, want %q", root, dir)
	}

	// The same root is derived whether the directory, the entry file, or a
	// file inside `src` is named on the command line.
	for _, in := range []string{dir, entry, filepath.Join(dir, "src")} {
		_, r, err := driver.ResolveEntry(in)
		if err != nil {
			t.Fatalf("ResolveEntry(%s): %v", in, err)
		}
		if r != dir {
			t.Fatalf("ResolveEntry(%s) root = %q, want %q", in, r, dir)
		}
	}

	out := filepath.Join(dir, "prog")
	em := diag.NewEmitter()
	if _, ok := driver.Build(entry, out, driver.Options{Root: root}, em); !ok {
		em.Render(os.Stderr, em.FirstFileID())
		t.Fatal("project build failed")
	}
	raw, err := exec.Command(out).Output()
	if err != nil {
		t.Fatalf("running project program: %v", err)
	}
	if got := string(raw); got != "42\n" {
		t.Fatalf("output mismatch: got %q want %q", got, "42\n")
	}
}

// TestDirectoryPackage loads a whole folder for one import: `use util;` pulls
// every .zx file in util/, whose public names are reachable through the
// package qualifier while private ones are not.
func TestDirectoryPackage(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "util"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"util/a.zx":  "pub fn a() -> i32 { 1 }\n",
		"util/b.zx":  "pub fn b() -> i32 { 2 }\nfn hidden() -> i32 { 9 }\n",
		"main.zx":    "use util;\nfn main() -> i32 { println(util::a(), util::b()); 0 }\n",
		"badmain.zx": "use util;\nfn main() -> i32 { util::hidden() }\n",
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	em := diag.NewEmitter()
	if _, ok := driver.FrontEnd(filepath.Join(dir, "main.zx"), driver.Options{}, em); !ok {
		em.Render(os.Stderr, em.FirstFileID())
		t.Fatal("directory package failed to type-check")
	}

	em = diag.NewEmitter()
	if _, ok := driver.FrontEnd(filepath.Join(dir, "badmain.zx"), driver.Options{}, em); ok {
		t.Fatal("expected a private package item to be rejected")
	}
	var buf bytes.Buffer
	em.Render(&buf, em.FirstFileID())
	if !strings.Contains(buf.String(), "E0603") {
		t.Fatalf("expected E0603 for package-private item, got:\n%s", buf.String())
	}
}

// TestProjectRootReachability checks that a module in a subdirectory can import
// a module at the project root by name.
func TestProjectRootReachability(t *testing.T) {
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no C compiler available")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"util.zx":          "pub fn base() -> i32 { 40 }\n",
		"nested/helper.zx": "use util;\npub fn seven() -> i32 { util::base() + 7 }\n",
		"main.zx":          "use nested::helper;\nfn main() -> i32 { println(helper::seven()); 0 }\n",
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entry, root, err := driver.ResolveEntry(filepath.Join(dir, "main.zx"))
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "prog")
	em := diag.NewEmitter()
	if _, ok := driver.Build(entry, out, driver.Options{Root: root}, em); !ok {
		em.Render(os.Stderr, em.FirstFileID())
		t.Fatal("project-root build failed")
	}
	raw, err := exec.Command(out).Output()
	if err != nil {
		t.Fatalf("running project program: %v", err)
	}
	if got := string(raw); got != "47\n" {
		t.Fatalf("output mismatch: got %q want %q", got, "47\n")
	}
}

// TestProjectNoEntry checks the diagnostic for a directory that has no entry
// point.
func TestProjectNoEntry(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := driver.ResolveEntry(dir); err == nil {
		t.Fatal("expected an error for a directory with no main.zx")
	}
}

// TestModuleQualifiedAccess exercises scoped modules end to end: an aliased
// file import, qualified functions/constants/types/associated functions/enum
// variants, a `mod name;` qualifier, and an inline `mod { ... }` qualifier.
func TestModuleQualifiedAccess(t *testing.T) {
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no C compiler available")
	}
	dir := t.TempDir()
	files := map[string]string{
		"lib.zx": `pub fn triple(n: i32) -> i32 { n * 3 }
pub const BASE: i32 = 10;
pub struct Point { pub x: i32, pub y: i32 }
pub enum Mode { Idle, Busy(i32) }
impl Point {
    pub fn new(x: i32, y: i32) -> Point { Point { x: x, y: y } }
    pub fn sum(self) -> i32 { self.x + self.y }
}
`,
		"tool.zx": `pub fn double(n: i32) -> i32 { n * 2 }
`,
		"main.zx": `use "lib.zx" as lib;
use math as m;
mod tool;
mod inline { pub fn add(a: i32, b: i32) -> i32 { a + b } }

fn main() -> i32 {
    let p: lib::Point = lib::Point::new(2, 3);
    println(lib::triple(7), p.sum(), lib::BASE, m::maxi(1, 5), tool::double(21), inline::add(2, 3));
    let mode = lib::Mode::Busy(9);
    match mode {
        lib::Mode::Busy(n) => { println(n); }
        lib::Mode::Idle => {}
    }
    0
}
`,
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mainPath := filepath.Join(dir, "main.zx")
	out := filepath.Join(dir, "prog")
	em := diag.NewEmitter()
	if _, ok := driver.Build(mainPath, out, driver.Options{}, em); !ok {
		em.Render(os.Stderr, em.FirstFileID())
		t.Fatal("failed to build qualified-module program")
	}
	raw, err := exec.Command(out).Output()
	if err != nil {
		t.Fatalf("running qualified-module program: %v", err)
	}
	if want := "21 5 10 5 42 5\n9\n"; string(raw) != want {
		t.Fatalf("output mismatch\n got: %q\nwant: %q", string(raw), want)
	}
}

// TestModulePrivacy checks that qualified access to a non-`pub` item is a
// diagnostic rather than silently working through the flattened namespace.
func TestModulePrivacy(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"lib.zx":  "fn secret() -> i32 { 1 }\npub fn open() -> i32 { secret() }\n",
		"main.zx": "use \"lib.zx\" as lib;\nfn main() -> i32 { lib::secret() }\n",
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	em := diag.NewEmitter()
	if _, ok := driver.FrontEnd(filepath.Join(dir, "main.zx"), driver.Options{}, em); ok {
		t.Fatal("expected private qualified access to be rejected")
	}
	var buf bytes.Buffer
	em.Render(&buf, em.FirstFileID())
	if !strings.Contains(buf.String(), "E0603") {
		t.Fatalf("expected E0603 for private item, got:\n%s", buf.String())
	}
}

// TestModuleUnknownItem and TestModuleUsedAsValue cover the two other
// qualified-path mistakes: a missing export and treating a module as a value.
func TestModuleQualifiedDiagnostics(t *testing.T) {
	cases := []struct {
		name, src, code string
	}{
		{"unknown", "use math as m;\nfn main() -> i32 { m::nope() }\n", "E0412"},
		{"module-as-type", "use math as m;\nfn main() -> i32 { let x: m = 0; 0 }\n", "E0433"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			mainPath := filepath.Join(dir, "main.zx")
			if err := os.WriteFile(mainPath, []byte(tc.src), 0o644); err != nil {
				t.Fatal(err)
			}
			em := diag.NewEmitter()
			if _, ok := driver.FrontEnd(mainPath, driver.Options{}, em); ok {
				t.Fatalf("expected %s for:\n%s", tc.code, tc.src)
			}
			var buf bytes.Buffer
			em.Render(&buf, em.FirstFileID())
			if !strings.Contains(buf.String(), tc.code) {
				t.Fatalf("expected %s, got:\n%s", tc.code, buf.String())
			}
		})
	}
}

// TestStdRegexHelpers exercises the opt-in POSIX ERE facade, including its
// compatible span ABI, invalid-pattern behavior, and string replacement.
func TestStdRegexHelpers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end regex test in short mode")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no C compiler available")
	}
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.zx")
	src := `import text.regex;

fn main() -> i32 {
    println(is_valid("[A-Z]+-[0-9]+"));
    println(is_match("[0-9]+", "item=42"), is_match("[0-9]+", "none"));
    println(full_match("[0-9]+", "42"), full_match("[0-9]+", "x42"));
    let span = match_span("beta[0-9]+", "alpha beta42 gamma");
    println(span.start, span.end);
    println(find("beta[0-9]+", "alpha beta42 gamma"));
    println(find_index("beta[0-9]+", "alpha beta42 gamma"));
    println(replace_first("beta[0-9]+", "alpha beta42 gamma", "ID"));
    println(is_valid("["), find_index("[", "text"));
    println(replace_first("^", "x", ">"));
    0
}
`
	if err := os.WriteFile(mainPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "prog")
	em := diag.NewEmitter()
	if _, ok := driver.Build(mainPath, out, driver.Options{}, em); !ok {
		em.Render(os.Stderr, em.FirstFileID())
		t.Fatal("failed to build regex program")
	}
	raw, err := exec.Command(out).Output()
	if err != nil {
		t.Fatalf("running regex program: %v", err)
	}
	if want := "true\ntrue false\ntrue false\n6 12\nbeta42\n6\nalpha ID gamma\nfalse -1\n>x\n"; string(raw) != want {
		t.Fatalf("output mismatch\n got: %q\nwant: %q", string(raw), want)
	}
}

// TestStdSysLocalHelpers covers local process state and deliberately
// non-recursive filesystem operations through the friendly os facade.
func TestStdSysLocalHelpers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end local-system test in short mode")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no C compiler available")
	}
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.zx")
	src := `import os;
import io.fs;

fn main() -> i32 {
    let work = arg_at(1);
    let from = arg_at(2);
    let to = arg_at(3);
    println(cwd().len() > 0, hostname().len() > 0, cpu_count() > 0);
    println(env_has("ZEXI_ARENA_MISSING_VALUE"), env_or("ZEXI_ARENA_MISSING_VALUE", "fallback"));
    println(env_set("ZEXI_ARENA_LOCAL_TEST", "set"));
    println(env_get("ZEXI_ARENA_LOCAL_TEST"));
    println(env_unset("ZEXI_ARENA_LOCAL_TEST"));
    println(env_has("ZEXI_ARENA_LOCAL_TEST"));
    println(mkdir(work.as_str()), mkdir(work.as_str()));
    println(chdir(work.as_str()));
    println(cwd().len() > 0);
    println(write_text(from.as_str(), "local"));
    println(rename_path(from.as_str(), to.as_str()));
    println(path_exists(to.as_str()));
    println(remove_path(to.as_str()));
    println(path_exists(to.as_str()));
    0
}
`
	if err := os.WriteFile(mainPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "prog")
	em := diag.NewEmitter()
	if _, ok := driver.Build(mainPath, out, driver.Options{}, em); !ok {
		em.Render(os.Stderr, em.FirstFileID())
		t.Fatal("failed to build local-system program")
	}
	work := filepath.Join(dir, "workspace")
	from := filepath.Join(dir, "from.txt")
	to := filepath.Join(dir, "to.txt")
	raw, err := exec.Command(out, work, from, to).Output()
	if err != nil {
		t.Fatalf("running local-system program: %v", err)
	}
	if want := "true true true\nfalse fallback\n0\nset\n0\nfalse\n0 0\n0\ntrue\n5\n0\ntrue\n0\nfalse\n"; string(raw) != want {
		t.Fatalf("output mismatch\n got: %q\nwant: %q", string(raw), want)
	}
}

// TestExportedAttributes checks the M1 attribute surface: `#[export]` and
// `#[no_mangle]` give a function its plain Zexi name with external linkage so
// C code can call it, and such a function is emitted even when nothing in Zexi
// calls it. `#[inline]` and `#[inline(never)]` are passed through to the C
// compiler, and `#[repr(C)]` names the layout the backend already uses.
func TestExportedAttributes(t *testing.T) {
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no C compiler available")
	}
	dir := t.TempDir()
	glue := `#include <stdio.h>
#include <stdint.h>
int64_t zexi_add(int64_t a, int64_t b);
int64_t public_name(int64_t n);
void run_c(void) {
    printf("c sees add=%lld public=%lld\n",
           (long long)zexi_add(20, 22), (long long)public_name(3));
}
`
	main := `#[export]
fn zexi_add(a: i64, b: i64) -> i64 { return a + b; }

#[no_mangle]
fn public_name(n: i64) -> i64 { return n * 2; }

#[inline]
fn fast(x: i64) -> i64 { return x + 1; }

#[inline(never)]
fn slow(x: i64) -> i64 { return x - 1; }

#[repr(C)]
#[copy]
struct Pair { x: i32, y: i32 }

extern "C" fn run_c();

fn main() -> i32 {
    println(fast(1), slow(1));
    let p = Pair { x: 1, y: 2 };
    let q = p;
    println(p.x, q.y);
    run_c();
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
	if _, ok := driver.Build(mainPath, out, driver.Options{CFiles: []string{gluePath}}, em); !ok {
		em.Render(os.Stderr, em.FirstFileID())
		t.Fatal("attribute bridge build failed")
	}
	raw, err := exec.Command(out).Output()
	if err != nil {
		t.Fatalf("running attribute program: %v", err)
	}
	want := "2 0\n1 2\nc sees add=42 public=6\n"
	if got := string(raw); got != want {
		t.Fatalf("output mismatch\n got: %q\nwant: %q", got, want)
	}
}

// TestUnknownAttributeIsRejected checks that a misspelled attribute is a
// diagnostic rather than a silent no-op.
func TestUnknownAttributeIsRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.zx")
	src := `#[Copy]
struct S { x: i32 }

#[bogus]
fn f() -> i32 { 0 }

fn main() -> i32 { 0 }
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	em := diag.NewEmitter()
	if _, ok := driver.FrontEnd(path, driver.Options{}, em); ok {
		t.Fatal("expected the unknown attributes to be rejected")
	}
	var buf strings.Builder
	em.Render(&buf, em.FirstFileID())
	got := buf.String()
	for _, want := range []string{"E0539", "#[Copy]", "#[bogus]"} {
		if !strings.Contains(got, want) {
			t.Fatalf("diagnostics missing %q:\n%s", want, got)
		}
	}
}
