package driver_test

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	stdhttp "net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"zexi/internal/diag"
	"zexi/internal/driver"
)

// buildFullExample compiles examples/http_full.zx. It uses the embedded
// `http.server` module, so it needs no library file.
func buildFullExample(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping end-to-end network test in short mode")
	}
	bin := filepath.Join(t.TempDir(), "http_full")
	em := diag.NewEmitter()
	if _, ok := driver.BuildFiles([]string{"../../examples/http_full.zx"}, bin, driver.Options{}, em); !ok {
		em.Render(os.Stderr, em.FirstFileID())
		t.Fatal("failed to build examples/http_full.zx")
	}
	return bin
}

// startFullServer runs the full server on a free port with its store in a
// temporary file. extra holds the workers and request budget arguments.
func startFullServer(t *testing.T, bin, store string, extra ...string) *exampleServerProc {
	t.Helper()
	return startExampleServerEnv(t, bin, []string{"ZEXI_NOTES_FILE=" + store}, extra...)
}

func TestHTTPFullServer(t *testing.T) {
	bin := buildFullExample(t)
	store := filepath.Join(t.TempDir(), "notes.json")
	srv := startFullServer(t, bin, store, "2")
	base := "http://" + srv.addr
	client := newExampleClient()
	const key = "X-Api-Key"
	writeKey := map[string]string{key: "dev-key", "Content-Type": "application/json"}

	t.Run("seed data and listing", func(t *testing.T) {
		r, body := doReq(t, client, "GET", base+"/api/notes", "", nil)
		wantStatus(t, r, 200)
		var got struct {
			Count int `json:"count"`
			Notes []struct {
				ID    int      `json:"id"`
				Title string   `json:"title"`
				Tags  []string `json:"tags"`
			} `json:"notes"`
		}
		mustJSON(t, body, &got)
		if got.Count != 1 || got.Notes[0].Title != "Welcome to Zexi" || len(got.Notes[0].Tags) != 2 {
			t.Fatalf("seed listing = %s", body)
		}
	})

	t.Run("writes need the key", func(t *testing.T) {
		r, body := doReq(t, client, "POST", base+"/api/notes", `{"title":"x"}`, map[string]string{"Content-Type": "application/json"})
		wantStatus(t, r, 401)
		if r.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("401 without WWW-Authenticate; body %s", body)
		}
		r, _ = doReq(t, client, "DELETE", base+"/api/notes/1", "", map[string]string{key: "wrong"})
		wantStatus(t, r, 401)
		r, _ = doReq(t, client, "POST", base+"/api/admin/shutdown", "", nil)
		wantStatus(t, r, 401)
	})

	var createdID int
	t.Run("create, read, validate", func(t *testing.T) {
		r, body := doReq(t, client, "POST", base+"/api/notes", `{"title":"Ship it","body":"today","tags":["work"]}`, writeKey)
		wantStatus(t, r, 201)
		var note struct {
			ID int `json:"id"`
		}
		mustJSON(t, body, &note)
		createdID = note.ID
		if loc := r.Header.Get("Location"); loc != fmt.Sprintf("/api/notes/%d", createdID) {
			t.Errorf("Location = %q", loc)
		}
		r, body = doReq(t, client, "GET", fmt.Sprintf("%s/api/notes/%d", base, createdID), "", nil)
		wantStatus(t, r, 200)
		if !strings.Contains(string(body), `"title":"Ship it"`) {
			t.Errorf("read back = %s", body)
		}
		r, body = doReq(t, client, "POST", base+"/api/notes", `{"body":"no title"}`, writeKey)
		wantStatus(t, r, 422)
		if !strings.Contains(string(body), "title is required") {
			t.Errorf("422 body = %s", body)
		}
		r, _ = doReq(t, client, "POST", base+"/api/notes", `{bad json`, writeKey)
		wantStatus(t, r, 400)
		r, _ = doReq(t, client, "GET", base+"/api/notes/999999", "", nil)
		wantStatus(t, r, 404)
		r, _ = doReq(t, client, "GET", base+"/api/notes/abc", "", nil)
		wantStatus(t, r, 400)
	})

	t.Run("patch and put", func(t *testing.T) {
		url := fmt.Sprintf("%s/api/notes/%d", base, createdID)
		r, body := doReq(t, client, "PATCH", url, `{"body":"edited"}`, writeKey)
		wantStatus(t, r, 200)
		if !strings.Contains(string(body), `"body":"edited"`) || !strings.Contains(string(body), `"title":"Ship it"`) {
			t.Errorf("PATCH should change only body: %s", body)
		}
		r, body = doReq(t, client, "PUT", url, `{"title":"Replaced"}`, writeKey)
		wantStatus(t, r, 200)
		if !strings.Contains(string(body), `"body":""`) || !strings.Contains(string(body), `"tags":[]`) {
			t.Errorf("PUT should replace the whole note: %s", body)
		}
		r, _ = doReq(t, client, "PUT", url, `{"body":"no title"}`, writeKey)
		wantStatus(t, r, 422)
	})

	t.Run("filters", func(t *testing.T) {
		r, body := doReq(t, client, "GET", base+"/api/notes?q=WELCOME", "", nil)
		wantStatus(t, r, 200)
		if !strings.Contains(string(body), `"count":1`) {
			t.Errorf("q filter = %s", body)
		}
		r, body = doReq(t, client, "GET", base+"/api/notes?tag=zexi", "", nil)
		wantStatus(t, r, 200)
		if !strings.Contains(string(body), `"count":1`) {
			t.Errorf("tag filter = %s", body)
		}
		r, body = doReq(t, client, "GET", base+"/api/notes?tag=nothing", "", nil)
		wantStatus(t, r, 200)
		if !strings.Contains(string(body), `"count":0`) {
			t.Errorf("empty tag filter = %s", body)
		}
	})

	t.Run("html pages and forms", func(t *testing.T) {
		r, body := doReq(t, client, "GET", base+"/", "", nil)
		wantStatus(t, r, 200)
		if !strings.Contains(string(body), "Welcome to Zexi") {
			t.Errorf("index missing the seed note")
		}
		r, _ = doReq(t, client, "POST", base+"/ui/notes", "title=Form&body=x", nil)
		wantStatus(t, r, 401)
		r, _ = doReq(t, client, "POST", base+"/ui/notes", "title=%3Cscript%3Ex%3C%2Fscript%3E&key=dev-key", nil)
		wantStatus(t, r, 302)
		r, body = doReq(t, client, "GET", base+"/", "", nil)
		wantStatus(t, r, 200)
		if strings.Contains(string(body), "<script>x") || !strings.Contains(string(body), "&lt;script&gt;x&lt;/script&gt;") {
			t.Errorf("stored title was not escaped in the index")
		}
		r, body = doReq(t, client, "GET", base+"/notes/1", "", nil)
		wantStatus(t, r, 200)
		if !strings.Contains(string(body), "<h1>Welcome to Zexi</h1>") {
			t.Errorf("note page = %s", body)
		}
		r, _ = doReq(t, client, "GET", base+"/notes/999999", "", nil)
		wantStatus(t, r, 404)
	})

	t.Run("static files and the traversal guard", func(t *testing.T) {
		r, body := doReq(t, client, "GET", base+"/static/style.css", "", nil)
		wantStatus(t, r, 200)
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "text/css") || !strings.Contains(string(body), "font-family") {
			t.Errorf("style.css content type %q", r.Header.Get("Content-Type"))
		}
		r, _ = doReq(t, client, "GET", base+"/static/../../go.mod", "", nil)
		if r.StatusCode != 403 && r.StatusCode != 404 {
			t.Errorf("traversal answered %d, want 403 or 404", r.StatusCode)
		}
		r, _ = doReq(t, client, "GET", base+"/static/missing.txt", "", nil)
		wantStatus(t, r, 404)
	})

	t.Run("cors, methods, fallbacks", func(t *testing.T) {
		r, _ := doReq(t, client, "OPTIONS", base+"/api/notes", "", nil)
		wantStatus(t, r, 204)
		if r.Header.Get("Access-Control-Allow-Methods") == "" {
			t.Errorf("preflight without Access-Control-Allow-Methods")
		}
		r, _ = doReq(t, client, "DELETE", base+"/health", "", nil)
		wantStatus(t, r, 405)
		if r.Header.Get("Allow") == "" {
			t.Errorf("405 without Allow")
		}
		r, body := doReq(t, client, "GET", base+"/api/nope", "", nil)
		wantStatus(t, r, 404)
		if !strings.Contains(string(body), "no such endpoint") {
			t.Errorf("api 404 = %s", body)
		}
		r, body = doReq(t, client, "GET", base+"/nope", "", nil)
		wantStatus(t, r, 404)
		if !strings.Contains(string(body), "<h1>404</h1>") {
			t.Errorf("HTML 404 = %s", body)
		}
		r, body = doReq(t, client, "GET", base+"/hello/Ada", "", nil)
		wantStatus(t, r, 200)
		if string(body) != "Hello, Ada!" {
			t.Errorf("hello = %q", body)
		}
		r, _ = doReq(t, client, "GET", base+"/home", "", nil)
		wantStatus(t, r, 302)
		if r.Header.Get("Location") != "/" {
			t.Errorf("home Location = %q", r.Header.Get("Location"))
		}
		r, body = doReq(t, client, "GET", base+"/health", "", nil)
		wantStatus(t, r, 200)
		if !strings.Contains(string(body), `"status":"ok"`) {
			t.Errorf("health = %s", body)
		}
	})

	t.Run("concurrent writers keep every note", func(t *testing.T) {
		// Each writer asks to close after one request. A keep-alive connection
		// holds its worker for up to KEEPALIVE_IDLE_MS, so with 2 workers and
		// 30 pooled connections most writers would wait in the accept queue.
		const writers = 30
		closeKey := map[string]string{"X-Api-Key": "dev-key", "Content-Type": "application/json", "Connection": "close"}
		var wg sync.WaitGroup
		codes := make(chan int, writers)
		for i := 0; i < writers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				body := fmt.Sprintf(`{"title":"parallel %d"}`, i)
				r, _ := doReq(t, client, "POST", base+"/api/notes", body, closeKey)
				codes <- r.StatusCode
			}(i)
		}
		wg.Wait()
		close(codes)
		for code := range codes {
			if code != 201 {
				t.Errorf("concurrent create answered %d", code)
			}
		}
		r, body := doReq(t, client, "GET", base+"/api/stats", "", nil)
		wantStatus(t, r, 200)
		// Seed note + "Ship it" + the script-title note + 30 writers.
		if !strings.Contains(string(body), `"notes":33`) {
			t.Errorf("stats after %d concurrent creates = %s", writers, body)
		}
	})

	t.Run("delete", func(t *testing.T) {
		url := fmt.Sprintf("%s/api/notes/%d", base, createdID)
		r, _ := doReq(t, client, "DELETE", url, "", writeKey)
		wantStatus(t, r, 204)
		r, _ = doReq(t, client, "DELETE", url, "", writeKey)
		wantStatus(t, r, 404)
		r, _ = doReq(t, client, "GET", url, "", nil)
		wantStatus(t, r, 404)
	})

	t.Run("log lines", func(t *testing.T) {
		logged := srv.stderr.String()
		for _, want := range []string{"POST /api/notes -> 201", "-> 401", "GET /health -> 200"} {
			if !strings.Contains(logged, want) {
				t.Errorf("request log is missing %q:\n%s", want, logged)
			}
		}
	})

	t.Run("admin shutdown", func(t *testing.T) {
		r, _ := doReq(t, client, "POST", base+"/api/admin/shutdown", "", writeKey)
		wantStatus(t, r, 202)
		srv.waitExit(t, 5*time.Second)
		if !strings.Contains(srv.stdout.String(), "server stopped after") {
			t.Errorf("stdout = %q", srv.stdout.String())
		}
	})
}

// TestHTTPFullServerPersists checks that notes written by one run are read by
// the next run from the same store file.
func TestHTTPFullServerPersists(t *testing.T) {
	bin := buildFullExample(t)
	store := filepath.Join(t.TempDir(), "notes.json")
	client := newExampleClient()
	writeKey := map[string]string{"X-Api-Key": "dev-key", "Content-Type": "application/json"}

	first := startFullServer(t, bin, store, "2")
	r, _ := doReq(t, client, "POST", "http://"+first.addr+"/api/notes", `{"title":"Kept across restarts","tags":["persist"]}`, writeKey)
	wantStatus(t, r, 201)
	r, _ = doReq(t, client, "POST", "http://"+first.addr+"/api/admin/shutdown", "", writeKey)
	wantStatus(t, r, 202)
	first.waitExit(t, 5*time.Second)

	raw, err := os.ReadFile(store)
	if err != nil {
		t.Fatalf("store file not written: %v", err)
	}
	if !bytes.Contains(raw, []byte("Kept across restarts")) {
		t.Fatalf("store file lacks the note:\n%s", raw)
	}

	second := startFullServer(t, bin, store, "2")
	r, body := doReq(t, client, "GET", "http://"+second.addr+"/api/notes?tag=persist", "", nil)
	wantStatus(t, r, 200)
	if !strings.Contains(string(body), "Kept across restarts") {
		t.Errorf("second run did not load the note: %s", body)
	}
}

// TestHTTPFullServerBudget checks the request budget: the server exits by
// itself after the requested number of requests.
func TestHTTPFullServerBudget(t *testing.T) {
	bin := buildFullExample(t)
	store := filepath.Join(t.TempDir(), "notes.json")
	srv := startFullServer(t, bin, store, "2", "3")
	client := newExampleClient()
	for i := 0; i < 3; i++ {
		r, _ := doReq(t, client, "GET", "http://"+srv.addr+"/health", "", nil)
		wantStatus(t, r, 200)
	}
	srv.waitExit(t, 5*time.Second)
	if !strings.Contains(srv.stdout.String(), "server stopped after 3 request(s)") {
		t.Errorf("stdout = %q, want the budget shutdown message", srv.stdout.String())
	}
}

// TestKeepAliveCapSignalsClose checks the per-connection request cap. The
// server must say `Connection: close` on the response that uses up a
// connection's last slot, so a client never sends another request down a
// socket the server is about to close. The client here is a raw connection
// with no retry logic, so a server that keeps advertising keep-alive fails.
func TestKeepAliveCapSignalsClose(t *testing.T) {
	bin := buildFullExample(t)
	srv := startFullServer(t, bin, filepath.Join(t.TempDir(), "notes.json"), "2")
	conn := dialExample(t, srv.addr)
	defer func() { conn.Close() }()
	br := bufio.NewReader(conn)
	req, err := stdhttp.NewRequest("GET", "http://"+srv.addr+"/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	const total = 205
	closes := 0
	for i := 1; i <= total; i++ {
		fmt.Fprintf(conn, "GET /health HTTP/1.1\r\nHost: %s\r\n\r\n", srv.addr)
		resp, err := stdhttp.ReadResponse(br, req)
		if err != nil {
			t.Fatalf("request %d on one connection failed: %v", i, err)
		}
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			t.Fatalf("request %d: reading body: %v", i, err)
		}
		resp.Body.Close()
		if resp.Close {
			closes++
			if i%100 != 0 {
				t.Errorf("server closed after request %d, want only after multiples of 100", i)
			}
			conn.Close()
			conn = dialExample(t, srv.addr)
			br = bufio.NewReader(conn)
		}
	}
	if closes != total/100 {
		t.Errorf("connection closed %d times in %d requests, want %d", closes, total, total/100)
	}
}

// TestSlowFirstByteIsAnswered checks that a client which connects and takes
// half a second to send its request still gets an answer. Only a socket that
// stays silent for the whole header budget may be dropped without a reply.
func TestSlowFirstByteIsAnswered(t *testing.T) {
	bin := buildFullExample(t)
	srv := startFullServer(t, bin, filepath.Join(t.TempDir(), "notes.json"), "2")
	conn := dialExample(t, srv.addr)
	defer conn.Close()
	time.Sleep(500 * time.Millisecond)
	fmt.Fprintf(conn, "GET /health HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", srv.addr)
	req, err := stdhttp.NewRequest("GET", "http://"+srv.addr+"/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := stdhttp.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatalf("slow client got no answer: %v", err)
	}
	resp.Body.Close()
	wantStatus(t, resp, 200)
}

// keepAliveLoop sends n requests on one keep-alive connection and reconnects
// only after the server says `Connection: close`. A connection the server drops
// early, or a status other than 200, is an error. The close may come only on
// the request that uses up the connection's last slot (every 100th request).
func keepAliveLoop(addr string, n int) error {
	dial := func() (net.Conn, *bufio.Reader, error) {
		c, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			return nil, nil, err
		}
		_ = c.SetDeadline(time.Now().Add(60 * time.Second))
		return c, bufio.NewReader(c), nil
	}
	conn, br, err := dial()
	if err != nil {
		return err
	}
	defer func() { conn.Close() }()
	for i := 1; i <= n; i++ {
		if _, err := fmt.Fprintf(conn, "GET /health HTTP/1.1\r\nHost: %s\r\n\r\n", addr); err != nil {
			return fmt.Errorf("request %d: write: %w", i, err)
		}
		resp, err := stdhttp.ReadResponse(br, nil)
		if err != nil {
			return fmt.Errorf("request %d: %w", i, err)
		}
		_, err = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("request %d: body: %w", i, err)
		}
		if resp.StatusCode != 200 {
			return fmt.Errorf("request %d: status %d", i, resp.StatusCode)
		}
		if resp.Close {
			if i%100 != 0 {
				return fmt.Errorf("server closed after request %d, want only after multiples of 100", i)
			}
			conn.Close()
			if conn, br, err = dial(); err != nil {
				return err
			}
		}
	}
	return nil
}

// TestHTTPFullStressKeepAlive runs more keep-alive clients than the pool has
// workers. Every request must get an answer: no connection may be dropped
// early and no request may time out.
func TestHTTPFullStressKeepAlive(t *testing.T) {
	bin := buildFullExample(t)
	srv := startFullServer(t, bin, filepath.Join(t.TempDir(), "notes.json"), "4")
	const clients = 40
	const perClient = 150 // crosses the 100-request connection cap once per client
	var wg sync.WaitGroup
	errs := make(chan error, clients)
	for c := 0; c < clients; c++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			if err := keepAliveLoop(srv.addr, perClient); err != nil {
				errs <- fmt.Errorf("client %d: %w", id, err)
			}
		}(c)
	}
	wg.Wait()
	close(errs)
	failed := 0
	for err := range errs {
		failed++
		if failed <= 5 {
			t.Error(err)
		}
	}
	if failed > 0 {
		t.Errorf("%d of %d clients failed", failed, clients)
	}
}

// TestIdleKeepAliveDoesNotStarveNewClients holds more idle keep-alive
// connections than the pool has workers. An idle connection must not hold a
// worker, so a new client is still answered at once, and the idle connections
// still work afterwards.
func TestIdleKeepAliveDoesNotStarveNewClients(t *testing.T) {
	bin := buildFullExample(t)
	srv := startFullServer(t, bin, filepath.Join(t.TempDir(), "notes.json"), "2")
	req, err := stdhttp.NewRequest("GET", "http://"+srv.addr+"/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	const idleCount = 6
	idle := make([]net.Conn, 0, idleCount)
	readers := make([]*bufio.Reader, 0, idleCount)
	defer func() {
		for _, c := range idle {
			c.Close()
		}
	}()
	for i := 0; i < idleCount; i++ {
		conn := dialExample(t, srv.addr)
		fmt.Fprintf(conn, "GET /health HTTP/1.1\r\nHost: %s\r\n\r\n", srv.addr)
		br := bufio.NewReader(conn)
		resp, err := stdhttp.ReadResponse(br, req)
		if err != nil {
			t.Fatalf("idle client %d: %v", i, err)
		}
		resp.Body.Close()
		wantStatus(t, resp, 200)
		idle = append(idle, conn)
		readers = append(readers, br)
	}

	start := time.Now()
	probe := dialExample(t, srv.addr)
	defer probe.Close()
	fmt.Fprintf(probe, "GET /health HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", srv.addr)
	resp, err := stdhttp.ReadResponse(bufio.NewReader(probe), req)
	waited := time.Since(start)
	if err != nil {
		t.Fatalf("new client got no answer behind %d idle connections: %v", idleCount, err)
	}
	resp.Body.Close()
	wantStatus(t, resp, 200)
	if waited > 2*time.Second {
		t.Errorf("new client waited %v behind idle keep-alive connections", waited)
	}

	// The idle connections are still usable after the probe.
	fmt.Fprintf(idle[0], "GET /health HTTP/1.1\r\nHost: %s\r\n\r\n", srv.addr)
	resp, err = stdhttp.ReadResponse(readers[0], req)
	if err != nil {
		t.Fatalf("idle connection dead after the probe: %v", err)
	}
	resp.Body.Close()
	wantStatus(t, resp, 200)
}

// TestHTTPFullTraversalIsBlocked sends raw request lines that try to leave the
// static folder, in plain, percent-encoded, double-encoded, backslash, NUL,
// and Unicode-dot forms. The client does not normalize them (Go's and curl's
// clients would), so the server sees exactly these bytes. Each must get 403 or
// 404, and no response may contain a file from outside the folder.
func TestHTTPFullTraversalIsBlocked(t *testing.T) {
	bin := buildFullExample(t)
	srv := startFullServer(t, bin, filepath.Join(t.TempDir(), "notes.json"), "2")
	cases := []string{
		"/static/../../etc/passwd",
		"/static/..%2f..%2fetc%2fpasswd",
		"/static/%2e%2e/%2e%2e/etc/passwd",
		"/static/%2e%2e%2f%2e%2e%2fetc%2fpasswd",
		"/static/..%5c..%5cetc%5cpasswd",
		"/static/..\\..\\etc\\passwd",
		"/static/%252e%252e/%252e%252e/etc/passwd",
		"/static/%00/../../etc/passwd",
		"/static/x%00.txt",
		"/static/....//....//etc/passwd",
		"/static//etc/passwd",
		"/static/%2fetc%2fpasswd",
		"/static/./../go.mod",
		"/static/style.css/..",
		"/static/%ef%bc%8e%ef%bc%8e/etc/passwd",
		"/static/style.css%00",
	}
	for _, path := range cases {
		t.Run(path, func(t *testing.T) {
			conn := dialExample(t, srv.addr)
			defer conn.Close()
			fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n", path)
			data, _ := io.ReadAll(conn)
			status, _, _ := strings.Cut(string(data), "\r\n")
			if !strings.HasPrefix(status, "HTTP/1.1 403 ") && !strings.HasPrefix(status, "HTTP/1.1 404 ") {
				t.Errorf("status %q, want 403 or 404", status)
			}
			if bytes.Contains(data, []byte("root:")) || bytes.Contains(data, []byte("module ")) {
				t.Errorf("response leaks a file outside the static folder: %q", data)
			}
		})
	}
}
