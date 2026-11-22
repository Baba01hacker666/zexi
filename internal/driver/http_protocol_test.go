package driver_test

import (
	"bytes"
	"io"
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

// TestHTTPMethodSemantics checks the router rules without a socket: HEAD uses
// the GET handler, a path that exists under another method is 405 with Allow
// (not 404), and an unknown path stays 404.
func TestHTTPMethodSemantics(t *testing.T) {
	wantOutput(t, "http_methods", `use http;

fn hello(req: &Request) -> Response { return Response::text(200, "hello body"); }
fn created(req: &Request) -> Response { return Response::text(201, "made"); }

fn main() -> i32 {
    let mut app = new_app();
    app.get("/items", hello);
    app.post("/items", created);
    app.add("PROPFIND", "/items", hello);

    let raw_head = String::from("HEAD /items HTTP/1.1\r\nHost: x\r\n\r\n");
    let mut h = parse_request(&raw_head);
    let hr = app.dispatch(&mut h);
    println(hr.status, hr.body);

    let raw_put = String::from("PUT /items HTTP/1.1\r\nHost: x\r\n\r\n");
    let mut p = parse_request(&raw_put);
    let pr = app.dispatch(&mut p);
    println(pr.status, pr.body.len(), pr.header("Allow"));

    let raw_missing = String::from("HEAD /missing HTTP/1.1\r\nHost: x\r\n\r\n");
    let mut m = parse_request(&raw_missing);
    let mr = app.dispatch(&mut m);
    println(mr.status, mr.header("Allow").len());
    0
}
`, "200 hello body\n405 0 GET, HEAD, POST, PROPFIND\n404 0\n")
}

// TestHEADSuppressesBody drives a real HEAD request through the server: the
// response must carry the GET Content-Length but no body bytes, and the
// one-request budget must shut the server down.
func TestHEADSuppressesBody(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end network test in short mode")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no C compiler available")
	}

	dir := t.TempDir()
	src := `use http;

fn hello(req: &Request) -> Response { return Response::text(200, "hi"); }

fn main() -> i32 {
    let mut app = new_app();
    app.get("/", hello);
    let rc = serve_local_n(&app, 1, 18114);
    println("rc", rc, "requests", requests_served());
    0
}
`
	mainPath := filepath.Join(dir, "main.zx")
	if err := os.WriteFile(mainPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "srv")
	em := diag.NewEmitter()
	if _, ok := driver.BuildFiles([]string{mainPath}, bin, driver.Options{}, em); !ok {
		em.Render(os.Stderr, em.FirstFileID())
		t.Fatal("failed to build the server")
	}

	var srvOut bytes.Buffer
	server := exec.Command(bin)
	server.Stdout = &srvOut
	server.Stderr = &srvOut
	if err := server.Start(); err != nil {
		t.Fatalf("starting server: %v", err)
	}
	defer func() { _ = server.Process.Kill() }()
	if !waitPort("127.0.0.1:18114") {
		t.Fatalf("server never started listening; output:\n%s", srvOut.String())
	}

	c, err := net.DialTimeout("tcp", "127.0.0.1:18114", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("HEAD / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(4 * time.Second))
	raw, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("reading HEAD response: %v (got %q)", err, raw)
	}
	_ = c.Close()

	parts := bytes.SplitN(raw, []byte("\r\n\r\n"), 2)
	if len(parts) != 2 {
		t.Fatalf("response has no header terminator: %q", raw)
	}
	head, body := string(parts[0]), parts[1]
	if !strings.HasPrefix(head, "HTTP/1.1 200 ") {
		t.Fatalf("status line = %q, want 200", strings.SplitN(head, "\r\n", 2)[0])
	}
	if !strings.Contains(head, "Content-Length: 2\r\n") {
		t.Fatalf("HEAD response should advertise the GET length, head = %q", head)
	}
	if len(body) != 0 {
		t.Fatalf("HEAD response carried %d body byte(s): %q", len(body), body)
	}

	done := make(chan struct{})
	go func() { _ = server.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not exit after its 1-request budget")
	}
	if got := srvOut.String(); !strings.Contains(got, "requests 1") {
		t.Fatalf("server log = %q, want it to report 1 request", got)
	}
}
