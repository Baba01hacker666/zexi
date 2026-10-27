package lsp_test

// End-to-end tests for the language server: a client thread speaks the
// protocol over in-memory pipes while the server runs in a goroutine.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"zexi/internal/lsp"
)

// client is a minimal LSP client for tests.
type client struct {
	t      *testing.T
	w      io.WriteCloser // to server
	r      io.ReadCloser  // from server
	nextID int
	// notify holds publishDiagnostics notifications seen while waiting for
	// replies.
	notify map[string][]lsp.Diagnostic
}

func newClient(t *testing.T) *client {
	t.Helper()
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()
	go lsp.Serve(serverIn, serverOut, lsp.Options{})
	return &client{t: t, w: clientOut, r: clientIn, notify: map[string][]lsp.Diagnostic{}}
}

func (c *client) send(v any) {
	c.t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := fmt.Fprintf(c.w, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		c.t.Fatal(err)
	}
	if _, err := c.w.Write(body); err != nil {
		c.t.Fatal(err)
	}
}

// readMsg reads one framed message and decodes it.
func (c *client) readMsg() map[string]any {
	c.t.Helper()
	buf := make([]byte, 0)
	// Read headers byte-wise off the pipe via a tiny buffered dance: pipes
	// don't implement bufio directly, so read in small chunks and split.
	accum := make([]byte, 0, 1024)
	tmp := make([]byte, 256)
	for {
		if i := strings.Index(string(accum), "\r\n\r\n"); i >= 0 {
			headers := string(accum[:i])
			for _, line := range strings.Split(headers, "\r\n") {
				if v, ok := strings.CutPrefix(line, "Content-Length: "); ok {
					length, err := strconv.Atoi(strings.TrimSpace(v))
					if err != nil {
						c.t.Fatal(err)
					}
					rest := accum[i+4:]
					for len(rest) < length {
						n, err := c.r.Read(tmp)
						if err != nil {
							c.t.Fatal(err)
						}
						rest = append(rest, tmp[:n]...)
					}
					buf = rest[:length]
					var msg map[string]any
					if err := json.Unmarshal(buf, &msg); err != nil {
						c.t.Fatalf("decode %q: %v", buf, err)
					}
					return msg
				}
			}
			c.t.Fatalf("no Content-Length in %q", headers)
		}
		n, err := c.r.Read(tmp)
		if err != nil {
			c.t.Fatal(err)
		}
		accum = append(accum, tmp[:n]...)
	}
}

// request sends a request and returns its result field, failing on error
// replies. Notifications received along the way are recorded.
func (c *client) request(method string, params any) any {
	c.t.Helper()
	c.nextID++
	id := c.nextID
	c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	for {
		msg := c.readMsg()
		if _, ok := msg["method"]; ok {
			c.recordNotify(msg)
			continue
		}
		gotID, _ := msg["id"].(float64)
		if int(gotID) != id {
			c.t.Fatalf("wanted reply id %d, got %v", id, msg)
		}
		if e, ok := msg["error"]; ok && e != nil {
			c.t.Fatalf("request %s failed: %v", method, e)
		}
		return msg["result"]
	}
}

// notify sends a notification.
func (c *client) notifySend(method string, params any) {
	c.t.Helper()
	c.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// recordNotify stores publishDiagnostics payloads.
func (c *client) recordNotify(msg map[string]any) {
	if msg["method"] != "textDocument/publishDiagnostics" {
		return
	}
	params, _ := msg["params"].(map[string]any)
	uri, _ := params["uri"].(string)
	var ds []lsp.Diagnostic
	raw, _ := json.Marshal(params["diagnostics"])
	_ = json.Unmarshal(raw, &ds)
	c.notify[uri] = ds
}

// drainUntilDiags reads messages until uri has a recorded diagnostic list,
// optionally matching wantErr ("" means accept any, including none).
func (c *client) waitForDiags(uri string, timeoutMsg string) {
	c.t.Helper()
	for i := 0; i < 200; i++ {
		if _, ok := c.notify[uri]; ok {
			return
		}
		// Read one more message (blocking); it may be the one we want.
		done := make(chan struct{})
		go func() { close(done) }()
		<-done
		msg := c.readMsg()
		if _, ok := msg["method"]; ok {
			c.recordNotify(msg)
			continue
		}
	}
	c.t.Fatalf("timeout waiting for diagnostics on %s (%s)", uri, timeoutMsg)
}

func (c *client) initialize(rootURI string) {
	c.t.Helper()
	params := map[string]any{"capabilities": map[string]any{}}
	if rootURI != "" {
		params["rootUri"] = rootURI
	}
	res := c.request("initialize", params)
	m, _ := res.(map[string]any)
	if m == nil {
		c.t.Fatal("initialize returned no result")
	}
	caps, _ := m["capabilities"].(map[string]any)
	if caps == nil {
		c.t.Fatal("initialize returned no capabilities")
	}
	if h, _ := caps["hoverProvider"].(bool); !h {
		c.t.Fatal("hoverProvider not advertised")
	}
	c.notifySend("initialized", map[string]any{})
}

func pathURI(p string) string {
	return "file://" + filepath.ToSlash(p)
}

// openDoc opens a document and returns after its diagnostics are published.
func (c *client) openDoc(path, text string) string {
	c.t.Helper()
	uri := pathURI(path)
	c.notifySend("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri": uri, "languageId": "zexi", "version": 1, "text": text,
		},
	})
	// Wait for the first publish (even an empty one) so analysis is done.
	for i := 0; i < 400; i++ {
		if _, ok := c.notify[uri]; ok {
			return uri
		}
		msg := c.readMsg()
		if _, ok := msg["method"]; ok {
			c.recordNotify(msg)
		}
	}
	c.t.Fatalf("no diagnostics published for %s", uri)
	return ""
}

const mainSrc = `struct Point {
    x: i64,
    y: i64,
}

fn add(a: i64, b: i64) -> i64 {
    a + b
}

fn main() -> i32 {
    let p = Point { x: 1, y: 2 };
    let s = add(p.x, p.y);
    println(s);
    0
}
`

func TestInitializeCapabilities(t *testing.T) {
	c := newClient(t)
	defer c.w.Close()
	c.initialize("")
}

func TestDiagnosticsLifecycle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.zx")
	// Type error: `let s = add(p.x, "two");`
	bad := `fn add(a: i64, b: i64) -> i64 {
    a + b
}

fn main() -> i32 {
    let s = add(1, "two");
    println(s);
    0
}
`
	c := newClient(t)
	defer c.w.Close()
	c.initialize("")
	uri := c.openDoc(path, bad)
	ds := c.notify[uri]
	if len(ds) == 0 {
		t.Fatalf("expected a type error, got none")
	}
	found := false
	for _, d := range ds {
		if d.Code == "E0308" && strings.Contains(d.Message, "i64") {
			found = true
			if d.Range.Start.Line != 5 { // 0-based line 5: `let s = ...`
				t.Fatalf("error should be on line 6 (0-based 5), got %+v", d.Range)
			}
			if d.Severity != 1 {
				t.Fatalf("severity should be 1 (Error), got %d", d.Severity)
			}
		}
	}
	if !found {
		t.Fatalf("no E0308 in %v", ds)
	}

	// Fix the buffer; diagnostics should clear without saving to disk.
	good := strings.Replace(bad, `"two"`, "2", 1)
	delete(c.notify, uri)
	c.notifySend("textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": uri, "version": 2},
		"contentChanges": []map[string]any{{"text": good}},
	})
	c.waitForDiags(uri, "after fix")
	if ds := c.notify[uri]; len(ds) != 0 {
		t.Fatalf("expected clean diagnostics after fix, got %v", ds)
	}
}

func TestMultiFileDiagnosticsGoToTheirFile(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.zx")
	libPath := filepath.Join(dir, "lib.zx")
	mainSrc := `use "lib.zx";

fn main() -> i32 {
    let x: i64 = helper();
    println(x);
    0
}
`
	libSrc := `pub fn helper() -> i64 {
    let bad: i64 = "oops";
    return bad;
}
`
	if err := os.WriteFile(libPath, []byte(libSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	c := newClient(t)
	defer c.w.Close()
	c.initialize("")
	uri := c.openDoc(mainPath, mainSrc)

	// The error must land on lib.zx, not the open document.
	libURI := pathURI(libPath)
	for i := 0; i < 400; i++ {
		if ds, ok := c.notify[libURI]; ok && len(ds) > 0 {
			d := ds[0]
			if d.Range.Start.Line != 1 {
				t.Fatalf("error should be on lib.zx line 2 (0-based 1), got %+v", d.Range)
			}
			if len(c.notify[uri]) != 0 {
				t.Fatalf("error leaked to main.zx: %v", c.notify[uri])
			}
			return
		}
		msg := c.readMsg()
		if _, ok := msg["method"]; ok {
			c.recordNotify(msg)
		}
	}
	t.Fatalf("no diagnostics published for imported %s (got %v)", libURI, c.notify)
}

func TestHoverDefinitionSymbols(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.zx")
	c := newClient(t)
	defer c.w.Close()
	c.initialize("")
	uri := c.openDoc(path, mainSrc)
	if ds := c.notify[uri]; len(ds) > 0 {
		t.Fatalf("fixture should be clean, got %v", ds)
	}

	// Hover on `add` in `let s = add(...)` -> line 11 (0-based), col 12.
	res := c.request("textDocument/hover", map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": 11, "character": 12},
	})
	h, _ := res.(map[string]any)
	if h == nil {
		t.Fatal("hover over add returned nothing")
	}
	contents, _ := h["contents"].(map[string]any)
	value, _ := contents["value"].(string)
	if !strings.Contains(value, "fn add") || !strings.Contains(value, "i64") {
		t.Fatalf("hover should show add's signature, got %q", value)
	}

	// Hover on field x in `p.x` -> line 11, char 18.
	res = c.request("textDocument/hover", map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": 11, "character": 18},
	})
	h, _ = res.(map[string]any)
	if h == nil {
		t.Fatal("hover over p.x field returned nothing")
	}
	contents, _ = h["contents"].(map[string]any)
	if v, _ := contents["value"].(string); !strings.Contains(v, "x: i64") {
		t.Fatalf("hover should show field type, got %q", v)
	}

	// Definition of `add` -> its declaration on line 5 (0-based).
	res = c.request("textDocument/definition", map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": 11, "character": 12},
	})
	locs, _ := res.([]any)
	if len(locs) == 0 {
		t.Fatal("definition of add returned nothing")
	}
	loc, _ := locs[0].(map[string]any)
	if u, _ := loc["uri"].(string); u != uri {
		t.Fatalf("definition uri = %q, want %q", u, uri)
	}
	lr, _ := loc["range"].(map[string]any)
	start, _ := lr["start"].(map[string]any)
	if line, _ := start["line"].(float64); int(line) != 5 {
		t.Fatalf("definition should land on fn add (line 5), got %v", start)
	}

	// Document symbols: fn add, fn main, struct Point.
	res = c.request("textDocument/documentSymbol", map[string]any{
		"textDocument": map[string]any{"uri": uri},
	})
	syms, _ := res.([]any)
	names := map[string]bool{}
	for _, s := range syms {
		m, _ := s.(map[string]any)
		n, _ := m["name"].(string)
		names[n] = true
	}
	for _, want := range []string{"add", "main", "Point"} {
		if !names[want] {
			t.Fatalf("missing document symbol %q (got %v)", want, names)
		}
	}
}

func TestMemberCompletion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.zx")
	src := `struct Point {
    x: i64,
    y: i64,
}

fn main() -> i32 {
    let p = Point { x: 1, y: 2 };
    let s = p.;
    0
}
`
	c := newClient(t)
	defer c.w.Close()
	c.initialize("")
	uri := c.openDoc(path, src)

	// Completion right after `p.` on line 7 (`    let s = p.;`).
	res := c.request("textDocument/completion", map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": 7, "character": 14},
	})
	list, _ := res.(map[string]any)
	items, _ := list["items"].([]any)
	labels := map[string]bool{}
	for _, it := range items {
		m, _ := it.(map[string]any)
		l, _ := m["label"].(string)
		labels[l] = true
	}
	if !labels["x"] || !labels["y"] {
		t.Fatalf("member completion should offer x and y, got %v", labels)
	}
}
