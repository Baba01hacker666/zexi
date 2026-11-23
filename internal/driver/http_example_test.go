package driver_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	stdhttp "net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"zexi/internal/diag"
	"zexi/internal/driver"
)

// TestHTTPExampleServer builds examples/http_server.zx with lib/http.zx, runs
// it on a loopback port, and checks its routes and its HTTP/1.1 behaviour end
// to end: routing and method rules, query, form and cookie handling, static
// files and their traversal guard, binary bodies, keep-alive and pipelining,
// bodies that arrive in several packets, protocol errors, a client that
// disconnects mid-response, and a clean shutdown. It is skipped in short mode
// and when no C compiler is available.
func TestHTTPExampleServer(t *testing.T) {
	bin := buildHTTPExample(t)
	srv := startExampleServer(t, bin)
	client := newExampleClient()
	base := "http://" + srv.addr

	t.Run("pages", func(t *testing.T) {
		r, body := doReq(t, client, "GET", base+"/", "", nil)
		wantStatus(t, r, 200)
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("index Content-Type = %q, want text/html", ct)
		}
		if !strings.Contains(string(body), "Zexi HTTP server") {
			t.Errorf("index body does not show the title: %q", body)
		}
		r, body = doReq(t, client, "GET", base+"/pi", "", nil)
		wantStatus(t, r, 200)
		if got := string(body); got != "3.141592653589793238462643383279\n" {
			t.Errorf("/pi body = %q", got)
		}
		r, body = doReq(t, client, "GET", base+"/form", "", nil)
		wantStatus(t, r, 200)
		if !strings.Contains(string(body), `action="/form"`) {
			t.Errorf("/form page has no form posting to /form: %q", body)
		}
	})

	t.Run("json", func(t *testing.T) {
		r, body := doReq(t, client, "GET", base+"/health", "", nil)
		wantStatus(t, r, 200)
		var h map[string]any
		mustJSON(t, body, &h)
		if h["status"] != "ok" {
			t.Errorf("/health status = %v, want ok", h["status"])
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
			t.Errorf("/health Content-Type = %q", ct)
		}

		r, body = doReq(t, client, "GET", base+"/greet/Grace%20Hopper", "", nil)
		wantStatus(t, r, 200)
		var g map[string]any
		mustJSON(t, body, &g)
		if g["greeting"] != "Hello, Grace Hopper!" {
			t.Errorf("/greet greeting = %v", g["greeting"])
		}

		r, body = doReq(t, client, "GET", base+"/greet/", "", nil)
		wantStatus(t, r, 400)
		var e map[string]any
		mustJSON(t, body, &e)
		if _, ok := e["error"]; !ok {
			t.Errorf("/greet/ error body has no error field: %s", body)
		}
	})

	t.Run("query and echo", func(t *testing.T) {
		r, body := doReq(t, client, "GET", base+"/query?name=Ada+L&x=%26&flag", "", nil)
		wantStatus(t, r, 200)
		var q map[string]any
		mustJSON(t, body, &q)
		if q["name"] != "Ada L" || q["x"] != "&" || q["flag"] != "" {
			t.Errorf("/query decoded to %v", q)
		}

		r, body = doReq(t, client, "POST", base+"/echo", "hello", nil)
		wantStatus(t, r, 200)
		if got := string(body); got != "you sent 5 byte(s): hello\n" {
			t.Errorf("/echo body = %q", got)
		}
	})

	t.Run("form", func(t *testing.T) {
		h := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
		r, body := doReq(t, client, "POST", base+"/form", "name=Ada&msg=a%26b%3Dc+d", h)
		wantStatus(t, r, 200)
		var f map[string]any
		mustJSON(t, body, &f)
		if f["name"] != "Ada" || f["msg"] != "a&b=c d" {
			t.Errorf("/form decoded to %v", f)
		}
		r, _ = doReq(t, client, "POST", base+"/form", "{}", map[string]string{"Content-Type": "application/json"})
		wantStatus(t, r, 415)
	})

	t.Run("headers and cookies", func(t *testing.T) {
		h := map[string]string{"X-Trace": "abc", "Cookie": "theme=dark; lang=en"}
		r, body := doReq(t, client, "GET", base+"/headers", "", h)
		wantStatus(t, r, 200)
		var hs map[string]any
		mustJSON(t, body, &hs)
		if hs["X-Trace"] != "abc" {
			t.Errorf("/headers did not echo X-Trace: %v", hs)
		}

		r, body = doReq(t, client, "GET", base+"/cookies", "", h)
		wantStatus(t, r, 200)
		var c map[string]any
		mustJSON(t, body, &c)
		if c["theme"] != "dark" || c["lang"] != "en" {
			t.Errorf("/cookies = %v", c)
		}

		r, _ = doReq(t, client, "GET", base+"/cookies/set?name=sid&value=abc123", "", nil)
		wantStatus(t, r, 200)
		if sc := r.Header.Get("Set-Cookie"); !strings.HasPrefix(sc, "sid=abc123;") || !strings.Contains(sc, "HttpOnly") {
			t.Errorf("Set-Cookie = %q", sc)
		}
		r, _ = doReq(t, client, "GET", base+"/cookies/set?name=bad%3Bname&value=x", "", nil)
		wantStatus(t, r, 400)
		r, _ = doReq(t, client, "GET", base+"/cookies/clear?name=sid", "", nil)
		wantStatus(t, r, 200)
		if sc := r.Header.Get("Set-Cookie"); !strings.Contains(sc, "Max-Age=0") {
			t.Errorf("clearing cookie: Set-Cookie = %q", sc)
		}
	})

	t.Run("status, redirect, time", func(t *testing.T) {
		r, body := doReq(t, client, "GET", base+"/status/418", "", nil)
		wantStatus(t, r, 418)
		if !strings.Contains(string(body), "I'm a teapot") {
			t.Errorf("/status/418 body = %q", body)
		}
		r, _ = doReq(t, client, "GET", base+"/status/999", "", nil)
		wantStatus(t, r, 400)
		r, body = doReq(t, client, "GET", base+"/status/204", "", nil)
		wantStatus(t, r, 204)
		if len(body) != 0 {
			t.Errorf("/status/204 sent a body: %q", body)
		}

		r, _ = doReq(t, client, "GET", base+"/redirect?to=/health", "", nil)
		wantStatus(t, r, 302)
		if loc := r.Header.Get("Location"); loc != "/health" {
			t.Errorf("Location = %q, want /health", loc)
		}
		r, _ = doReq(t, client, "GET", base+"/redirect?to=//evil.example", "", nil)
		wantStatus(t, r, 400)
		r, _ = doReq(t, client, "GET", base+"/redirect?to=/x&code=301", "", nil)
		wantStatus(t, r, 301)

		r, body = doReq(t, client, "GET", base+"/time", "", nil)
		wantStatus(t, r, 200)
		var tm map[string]any
		mustJSON(t, body, &tm)
		if iso, _ := tm["iso"].(string); len(iso) != 20 || !strings.HasSuffix(iso, "Z") {
			t.Errorf("/time iso = %v", tm["iso"])
		}
		if date := r.Header.Get("Date"); !strings.HasSuffix(date, "GMT") {
			t.Errorf("Date header = %q", date)
		}
		if sv := r.Header.Get("Server"); sv != "zexi-http/0.1" {
			t.Errorf("Server header = %q", sv)
		}
	})

	t.Run("bytes survive intact", func(t *testing.T) {
		r, body := doReq(t, client, "GET", base+"/bytes/1024", "", nil)
		wantStatus(t, r, 200)
		if ct := r.Header.Get("Content-Type"); ct != "application/octet-stream" {
			t.Errorf("/bytes Content-Type = %q", ct)
		}
		if len(body) != 1024 {
			t.Fatalf("/bytes/1024 returned %d bytes", len(body))
		}
		for i, b := range body {
			if b != byte(i%256) {
				t.Fatalf("byte %d = %d, want %d", i, b, i%256)
			}
		}
	})

	t.Run("static files", func(t *testing.T) {
		r, body := doReq(t, client, "GET", base+"/static/style.css", "", nil)
		wantStatus(t, r, 200)
		if ct := r.Header.Get("Content-Type"); ct != "text/css; charset=utf-8" {
			t.Errorf("style.css Content-Type = %q", ct)
		}
		if !strings.Contains(string(body), "font-family") {
			t.Errorf("style.css body = %q", body)
		}
		r, body = doReq(t, client, "GET", base+"/static/", "", nil)
		wantStatus(t, r, 200)
		if !strings.Contains(string(body), "Static files") {
			t.Errorf("/static/ did not serve index.html: %q", body)
		}
		r, _ = doReq(t, client, "GET", base+"/static/nope.txt", "", nil)
		wantStatus(t, r, 404)
		r, _ = doReq(t, client, "GET", base+"/static/%2e%2e/http_server.zx", "", nil)
		wantStatus(t, r, 403)
		r, _ = doReq(t, client, "GET", base+"/static/..%5Cx", "", nil)
		wantStatus(t, r, 403)
	})

	t.Run("methods and errors", func(t *testing.T) {
		r, _ := doReq(t, client, "DELETE", base+"/health", "", nil)
		wantStatus(t, r, 405)
		if allow := r.Header.Get("Allow"); !strings.Contains(allow, "GET") {
			t.Errorf("405 Allow = %q", allow)
		}
		r, _ = doReq(t, client, "OPTIONS", base+"/echo", "", nil)
		wantStatus(t, r, 204)
		if allow := r.Header.Get("Allow"); !strings.Contains(allow, "POST") {
			t.Errorf("OPTIONS Allow = %q", allow)
		}
		r, _ = doReq(t, client, "BREW", base+"/health", "", nil)
		wantStatus(t, r, 501)
		r, _ = doReq(t, client, "GET", base+"/no/such/route", "", nil)
		wantStatus(t, r, 404)
	})

	t.Run("head", func(t *testing.T) {
		r, body := doReq(t, client, "HEAD", base+"/time", "", nil)
		wantStatus(t, r, 200)
		if len(body) != 0 {
			t.Errorf("HEAD sent a body: %q", body)
		}
		if cl := r.Header.Get("Content-Length"); cl == "" || cl == "0" {
			t.Errorf("HEAD Content-Length = %q, want the GET length", cl)
		}
	})

	t.Run("pipelining", func(t *testing.T) {
		conn := dialExample(t, srv.addr)
		defer conn.Close()
		req := "GET /pi HTTP/1.1\r\nHost: x\r\n\r\n" +
			"GET /health HTTP/1.1\r\nHost: x\r\n\r\n" +
			"GET /nope HTTP/1.1\r\nHost: x\r\n\r\n"
		if _, err := conn.Write([]byte(req)); err != nil {
			t.Fatal(err)
		}
		br := bufio.NewReader(conn)
		want := []int{200, 200, 404}
		for i, status := range want {
			resp, _ := readRawResponse(t, br, "GET")
			if resp.StatusCode != status {
				t.Errorf("pipelined response %d: status %d, want %d", i, resp.StatusCode, status)
			}
		}
	})

	t.Run("body in later packets", func(t *testing.T) {
		conn := dialExample(t, srv.addr)
		defer conn.Close()
		if _, err := conn.Write([]byte("POST /echo HTTP/1.1\r\nHost: x\r\nContent-Length: 11\r\n\r\nhello")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(150 * time.Millisecond)
		if _, err := conn.Write([]byte(" world")); err != nil {
			t.Fatal(err)
		}
		resp, body := readRawResponse(t, bufio.NewReader(conn), "POST")
		if resp.StatusCode != 200 || string(body) != "you sent 11 byte(s): hello world\n" {
			t.Errorf("split body: %d %q", resp.StatusCode, body)
		}
	})

	t.Run("protocol errors", func(t *testing.T) {
		cases := []struct {
			name    string
			payload string
			status  int
		}{
			{"oversized body", "POST /echo HTTP/1.1\r\nHost: x\r\nContent-Length: 2000000\r\n\r\n", 413},
			{"chunked body", "POST /echo HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\n", 501},
			{"bad length", "POST /echo HTTP/1.1\r\nHost: x\r\nContent-Length: 12abc\r\n\r\n", 400},
			{"huge head", "GET / HTTP/1.1\r\nHost: x\r\nX-Big: " + strings.Repeat("a", 20000) + "\r\n\r\n", 431},
			{"garbage line", "GARBAGE\r\n\r\n", 400},
			{"unknown version", "GET /health HTTP/2.0\r\nHost: x\r\n\r\n", 505},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				conn := dialExample(t, srv.addr)
				defer conn.Close()
				if _, err := conn.Write([]byte(tc.payload)); err != nil {
					t.Fatal(err)
				}
				resp, _ := readRawResponse(t, bufio.NewReader(conn), "POST")
				if resp.StatusCode != tc.status {
					t.Errorf("status %d, want %d", resp.StatusCode, tc.status)
				}
				if !resp.Close {
					t.Errorf("connection stayed open after a protocol error")
				}
			})
		}
	})

	t.Run("slow head times out", func(t *testing.T) {
		conn := dialExample(t, srv.addr)
		defer conn.Close()
		if _, err := conn.Write([]byte("GET /health HTTP/1.1\r\nHost:")); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
		resp, _ := readRawResponse(t, bufio.NewReader(conn), "GET")
		if resp.StatusCode != 408 {
			t.Errorf("status %d, want 408", resp.StatusCode)
		}
	})

	t.Run("client leaves mid-response", func(t *testing.T) {
		conn := dialExample(t, srv.addr)
		if _, err := conn.Write([]byte("GET /bytes/1048576 HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 1000)
		_, _ = io.ReadFull(conn, buf)
		_ = conn.Close()
		time.Sleep(100 * time.Millisecond)
		r, _ := doReq(t, client, "GET", base+"/health", "", nil)
		wantStatus(t, r, 200)
	})

	t.Run("stats count requests", func(t *testing.T) {
		r, body := doReq(t, client, "GET", base+"/stats", "", nil)
		wantStatus(t, r, 200)
		var s map[string]any
		mustJSON(t, body, &s)
		if n, _ := s["requests"].(float64); n < 20 {
			t.Errorf("/stats requests = %v, want at least 20", s["requests"])
		}
	})

	t.Run("log lines", func(t *testing.T) {
		if got := srv.stderr.String(); !strings.Contains(got, "GET /health -> 200") {
			t.Errorf("request log is missing the /health line:\n%s", got)
		}
	})

	t.Run("shutdown", func(t *testing.T) {
		r, body := doReq(t, client, "POST", base+"/shutdown", "", nil)
		wantStatus(t, r, 200)
		if string(body) != "shutting down\n" {
			t.Errorf("/shutdown body = %q", body)
		}
		srv.waitExit(t, 5*time.Second)
		if !strings.Contains(srv.stdout.String(), "server stopped after") {
			t.Errorf("no shutdown message on stdout: %q", srv.stdout.String())
		}
	})
}

// TestHTTPExampleServerBudget runs the example with a request budget and
// checks that it exits by itself once the budget is spent.
func TestHTTPExampleServerBudget(t *testing.T) {
	bin := buildHTTPExample(t)
	srv := startExampleServer(t, bin, "2")
	client := newExampleClient()
	for i := 0; i < 2; i++ {
		r, _ := doReq(t, client, "GET", "http://"+srv.addr+"/health", "", nil)
		wantStatus(t, r, 200)
	}
	srv.waitExit(t, 5*time.Second)
	if !strings.Contains(srv.stdout.String(), "server stopped after 2 request(s)") {
		t.Errorf("stdout = %q, want the budget shutdown message", srv.stdout.String())
	}
}

// buildHTTPExample compiles examples/http_server.zx with lib/http.zx.
func buildHTTPExample(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping end-to-end network test in short mode")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no C compiler available")
	}
	bin := filepath.Join(t.TempDir(), "http_server")
	em := diag.NewEmitter()
	if _, ok := driver.BuildFiles([]string{"../../examples/http_server.zx", "../../lib/http.zx"}, bin, driver.Options{}, em); !ok {
		em.Render(os.Stderr, em.FirstFileID())
		t.Fatal("failed to build examples/http_server.zx")
	}
	return bin
}

// syncBuffer is a bytes.Buffer that the process copier and the test can use
// at the same time.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// exampleServerProc is a running example server.
type exampleServerProc struct {
	addr   string
	stdout syncBuffer
	stderr syncBuffer
	exited chan struct{}
	err    error
}

// startExampleServer runs bin on a free loopback port (plus any extra
// arguments) and waits until it accepts connections.
func startExampleServer(t *testing.T, bin string, extra ...string) *exampleServerProc {
	t.Helper()
	return startExampleServerEnv(t, bin, nil, extra...)
}

// startExampleServerEnv is startExampleServer with extra environment entries
// (KEY=value) added to the inherited environment.
func startExampleServerEnv(t *testing.T, bin string, env []string, extra ...string) *exampleServerProc {
	t.Helper()
	port := freeLoopbackPort(t)
	srv := &exampleServerProc{addr: fmt.Sprintf("127.0.0.1:%d", port), exited: make(chan struct{})}
	cmd := exec.Command(bin, append([]string{strconv.Itoa(port)}, extra...)...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	cmd.Stdout = &srv.stdout
	cmd.Stderr = &srv.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting server: %v", err)
	}
	go func() {
		srv.err = cmd.Wait()
		close(srv.exited)
	}()
	t.Cleanup(func() {
		select {
		case <-srv.exited:
		default:
			_ = cmd.Process.Kill()
			<-srv.exited
		}
	})
	if !waitPort(srv.addr) {
		t.Fatalf("server never started listening; stderr:\n%s", srv.stderr.String())
	}
	return srv
}

// waitExit waits for the server process to exit on its own and checks that it
// exited cleanly.
func (s *exampleServerProc) waitExit(t *testing.T, limit time.Duration) {
	t.Helper()
	select {
	case <-s.exited:
	case <-time.After(limit):
		t.Fatalf("server did not exit within %v; stderr:\n%s", limit, s.stderr.String())
	}
	if s.err != nil {
		t.Fatalf("server exited with %v; stderr:\n%s", s.err, s.stderr.String())
	}
}

// freeLoopbackPort asks the kernel for an unused port on 127.0.0.1.
func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return tcpPort(t, ln)
}

// newExampleClient is a client that never follows redirects, so tests can
// check the 3xx responses the server sends.
func newExampleClient() *stdhttp.Client {
	return &stdhttp.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(*stdhttp.Request, []*stdhttp.Request) error {
			return stdhttp.ErrUseLastResponse
		},
	}
}

// doReq sends one request and returns the response and its full body.
func doReq(t *testing.T, c *stdhttp.Client, method, url, body string, headers map[string]string) (*stdhttp.Response, []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := stdhttp.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: reading body: %v", method, url, err)
	}
	return resp, b
}

func wantStatus(t *testing.T, r *stdhttp.Response, want int) {
	t.Helper()
	if r.StatusCode != want {
		t.Fatalf("%s %s: status %d, want %d", r.Request.Method, r.Request.URL.Path, r.StatusCode, want)
	}
}

func mustJSON(t *testing.T, body []byte, into any) {
	t.Helper()
	if err := json.Unmarshal(body, into); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, body)
	}
}

// dialExample opens a raw TCP connection to the example server.
func dialExample(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	return conn
}

// readRawResponse reads one response with net/http's parser, so framing is
// checked by the same rules a real client uses. It returns the response and
// its body.
func readRawResponse(t *testing.T, br *bufio.Reader, method string) (*stdhttp.Response, []byte) {
	t.Helper()
	req, err := stdhttp.NewRequest(method, "http://127.0.0.1/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := stdhttp.ReadResponse(br, req)
	if err != nil {
		t.Fatalf("reading response: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response body: %v", err)
	}
	return resp, body
}
