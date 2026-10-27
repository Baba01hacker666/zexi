package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Options configures the server.
type Options struct {
	// Root names the program entry to analyze (for example the project's
	// main.zx). When empty, each open document is analyzed as its own entry,
	// and the "no main" error is suppressed for documents that declare no
	// main function.
	Root string
}

// doc is one open editor buffer.
type doc struct {
	uri     string
	path    string // cleaned absolute path
	text    string
	version int
}

// server is the LSP state machine. It is not safe for concurrent use; Serve
// drives it from a single read loop.
type server struct {
	r   *bufio.Reader
	w   io.Writer
	opt Options

	docs map[string]*doc // keyed by cleaned path
	// analyses holds one front-end run per analyzed entry, newest first.
	analyses []*analysis
	// cached last-sent diagnostics per path, so closes clear stale ones.
	sent map[string]struct{}

	shutdown bool
	exited   bool
	exitCode int
}

// Serve runs the language server, reading requests from r and writing
// replies to w, until the client exits. It returns the process exit code
// (0 after a clean shutdown/exit sequence).
func Serve(r io.Reader, w io.Writer, opts Options) int {
	s := &server{
		r:    bufio.NewReader(r),
		w:    w,
		opt:  opts,
		docs: map[string]*doc{},
		sent: map[string]struct{}{},
	}
	return s.loop()
}

func (s *server) loop() int {
	for !s.exited {
		raw, err := readMessage(s.r)
		if err == io.EOF {
			break
		}
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "lsp: %v\n", err)
			return 1
		}
		var req request
		if err := json.Unmarshal(raw, &req); err != nil {
			s.replyError(nil, -32700, "parse error: "+err.Error())
			continue
		}
		s.dispatch(&req)
	}
	return s.exitCode
}

// dispatch routes one message.
func (s *server) dispatch(req *request) {
	isRequest := len(req.ID) > 0 && string(req.ID) != "null"
	switch req.Method {
	case "initialize":
		s.onInitialize(req, isRequest)
	case "initialized", "$/setTrace", "workspace/didChangeConfiguration", "workspace/didChangeWatchedFiles":
		// Notifications we accept and ignore.
		if isRequest {
			s.reply(req.ID, nil)
		}
	case "shutdown":
		s.shutdown = true
		if isRequest {
			s.reply(req.ID, nil)
		}
	case "exit":
		s.exited = true
		if !s.shutdown {
			s.exitCode = 1
		}
	case "textDocument/didOpen":
		s.onDidOpen(req)
	case "textDocument/didChange":
		s.onDidChange(req)
	case "textDocument/didClose":
		s.onDidClose(req)
	case "textDocument/didSave":
		// The buffer is already the source of truth; nothing to do.
	case "textDocument/hover":
		s.onHover(req, isRequest)
	case "textDocument/definition":
		s.onDefinition(req, isRequest)
	case "textDocument/completion":
		s.onCompletion(req, isRequest)
	case "textDocument/documentSymbol":
		s.onDocumentSymbol(req, isRequest)
	default:
		if isRequest {
			s.replyError(req.ID, -32601, fmt.Sprintf("method not found: %s", req.Method))
		}
	}
}

func (s *server) onInitialize(req *request, isRequest bool) {
	if !isRequest {
		return
	}
	var p InitializeParams
	_ = json.Unmarshal(req.Params, &p)
	if s.opt.Root == "" {
		if p.RootPath != nil && *p.RootPath != "" {
			s.opt.Root = findEntry(*p.RootPath)
		} else if p.RootURI != nil && *p.RootURI != "" {
			s.opt.Root = findEntry(uriToPath(*p.RootURI))
		} else if len(p.WorkspaceFolders) > 0 {
			s.opt.Root = findEntry(uriToPath(p.WorkspaceFolders[0].URI))
		}
	}
	s.reply(req.ID, InitializeResult{
		Capabilities: ServerCapabilities{
			TextDocumentSync: TextDocumentSyncOptions{
				OpenClose: true,
				Change:    1, // full
				Save:      true,
			},
			HoverProvider:      true,
			DefinitionProvider: true,
			CompletionProvider: &CompletionOptions{
				TriggerCharacters: []string{"."},
			},
			DocumentSymbolProvider: true,
		},
		ServerInfo: &ServerInfo{Name: "zexi-lsp", Version: "0.1"},
	})
}

// findEntry picks a program entry from a workspace root: main.zx if present,
// else any .zx at the top level, else "" (per-document analysis).
func findEntry(root string) string {
	if root == "" {
		return ""
	}
	if st, err := os.Stat(root); err == nil && !st.IsDir() {
		return root
	}
	for _, name := range []string{"main.zx", "src/main.zx"} {
		cand := filepath.Join(root, name)
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand
		}
	}
	if ents, err := os.ReadDir(root); err == nil {
		for _, e := range ents {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".zx") {
				return filepath.Join(root, e.Name())
			}
		}
	}
	return ""
}

// --- document sync ---------------------------------------------------------

func (s *server) onDidOpen(req *request) {
	var p DidOpenParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return
	}
	path := cleanPath(uriToPath(p.TextDocument.URI))
	s.docs[path] = &doc{
		uri:     p.TextDocument.URI,
		path:    path,
		text:    p.TextDocument.Text,
		version: p.TextDocument.Version,
	}
	s.recheck()
}

func (s *server) onDidChange(req *request) {
	var p DidChangeParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return
	}
	path := cleanPath(uriToPath(p.TextDocument.URI))
	d, ok := s.docs[path]
	if !ok {
		return
	}
	for _, ch := range p.ContentChanges {
		if ch.Range == nil {
			d.text = ch.Text
		} else {
			// Partial edits are accepted for robustness even though we
			// advertise full sync.
			start := positionToOffset([]byte(d.text), ch.Range.Start)
			end := positionToOffset([]byte(d.text), ch.Range.End)
			if start > end {
				start, end = end, start
			}
			d.text = d.text[:start] + ch.Text + d.text[end:]
		}
	}
	d.version = p.TextDocument.Version
	s.recheck()
}

func (s *server) onDidClose(req *request) {
	var p DidCloseParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return
	}
	path := cleanPath(uriToPath(p.TextDocument.URI))
	delete(s.docs, path)
	s.recheck()
}

// --- analysis & diagnostics ------------------------------------------------

// overlaySnapshot captures every open buffer for the front end.
func (s *server) overlaySnapshot() map[string][]byte {
	overlay := make(map[string][]byte, len(s.docs))
	for p, d := range s.docs {
		overlay[p] = []byte(d.text)
	}
	return overlay
}

// recheck re-runs the front end and publishes fresh diagnostics.
func (s *server) recheck() {
	overlay := s.overlaySnapshot()
	s.analyses = nil

	var entries []string
	if s.opt.Root != "" {
		// A named root is the program entry; analyze just it so program-level
		// diagnostics (like "no main") stay meaningful.
		entries = append(entries, cleanPath(s.opt.Root))
	} else {
		for p := range s.docs {
			entries = append(entries, p)
		}
	}

	merged := map[string][]Diagnostic{}
	for _, p := range entries {
		suppress := s.opt.Root == "" && !hasMain(p, overlay)
		a := analyzeEntry(p, overlay, suppress)
		s.analyses = append(s.analyses, a)
		for fp, ds := range a.diags {
			merged[fp] = append(merged[fp], ds...)
		}
	}

	// Publish to every open document and every file that has diagnostics.
	targets := map[string]bool{}
	for p := range s.docs {
		targets[p] = true
	}
	for p := range merged {
		targets[p] = true
	}
	for p := range s.sent {
		targets[p] = true
	}
	s.sent = map[string]struct{}{}
	for p := range targets {
		uri := s.uriFor(p)
		ds := dedupeDiags(merged[p])
		if ds == nil {
			ds = []Diagnostic{}
		}
		s.send(notification{
			JSONRPC: "2.0",
			Method:  "textDocument/publishDiagnostics",
			Params: PublishDiagnosticsParams{
				URI:         uri,
				Version:     s.versionFor(p),
				Diagnostics: ds,
			},
		})
		if ds != nil {
			s.sent[p] = struct{}{}
		}
	}
}

// hasMain reports whether the file text declares fn main (a cheap proxy for
// "this is a program entry", used to suppress the E0601 error on libraries).
func hasMain(path string, overlay map[string][]byte) bool {
	src, ok := overlay[path]
	if !ok {
		b, err := os.ReadFile(path)
		if err != nil {
			return true
		}
		src = b
	}
	for _, line := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(line, "fn main") || strings.HasPrefix(line, "pub fn main") {
			return true
		}
	}
	return false
}

// dedupeDiags drops duplicates produced by analyzing several entries over the
// same file.
func dedupeDiags(ds []Diagnostic) []Diagnostic {
	seen := map[string]bool{}
	out := ds[:0]
	for _, d := range ds {
		key := fmt.Sprintf("%d:%d:%s:%s", d.Range.Start.Line, d.Range.Start.Character, d.Code, d.Message)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, d)
	}
	return out
}

// uriFor maps a cleaned path to the URI to publish under, preferring the URI
// of the open document.
func (s *server) uriFor(path string) string {
	if d, ok := s.docs[path]; ok {
		return d.uri
	}
	return pathToURI(path)
}

func (s *server) versionFor(path string) int {
	if d, ok := s.docs[path]; ok {
		return d.version
	}
	return 0
}

// analysisFor finds an analysis that covers path.
func (s *server) analysisFor(path string) *analysis {
	for _, a := range s.analyses {
		if _, ok := a.byPath[path]; ok {
			return a
		}
	}
	return nil
}

// --- language features -----------------------------------------------------

func (s *server) onHover(req *request, isRequest bool) {
	if !isRequest {
		return
	}
	var p TextDocumentPositionParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		s.reply(req.ID, nil)
		return
	}
	path := cleanPath(uriToPath(p.TextDocument.URI))
	d, ok := s.docs[path]
	if !ok {
		s.reply(req.ID, nil)
		return
	}
	a := s.analysisFor(path)
	if a == nil || a.art == nil || a.art.Info == nil {
		s.reply(req.ID, nil)
		return
	}
	off := positionToOffset([]byte(d.text), p.Position)
	n := nodeAtOffset(a.art.Info.File, a.globalOffset(path, off))
	text := a.hoverText(n)
	if text == "" {
		s.reply(req.ID, nil)
		return
	}
	h := Hover{Contents: MarkupContent{Kind: "markdown", Value: text}}
	if n != nil {
		if r, _, ok := a.fileSpan(nodeSpan(n)); ok {
			h.Range = &r
		}
	}
	s.reply(req.ID, h)
}

func (s *server) onDefinition(req *request, isRequest bool) {
	if !isRequest {
		return
	}
	var p TextDocumentPositionParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		s.reply(req.ID, nil)
		return
	}
	path := cleanPath(uriToPath(p.TextDocument.URI))
	d, ok := s.docs[path]
	if !ok {
		s.reply(req.ID, nil)
		return
	}
	a := s.analysisFor(path)
	if a == nil {
		s.reply(req.ID, nil)
		return
	}
	off := positionToOffset([]byte(d.text), p.Position)
	if loc, ok := a.definitionLoc(path, off); ok {
		s.reply(req.ID, []Location{loc})
		return
	}
	s.reply(req.ID, nil)
}

func (s *server) onCompletion(req *request, isRequest bool) {
	if !isRequest {
		return
	}
	var p TextDocumentPositionParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		s.reply(req.ID, CompletionList{})
		return
	}
	path := cleanPath(uriToPath(p.TextDocument.URI))
	d, ok := s.docs[path]
	if !ok {
		s.reply(req.ID, CompletionList{})
		return
	}
	a := s.analysisFor(path)
	if a == nil {
		s.reply(req.ID, CompletionList{})
		return
	}
	off := positionToOffset([]byte(d.text), p.Position)
	dot := false
	text := d.text
	// Member completion when the character before the cursor is `.` (after
	// skipping the partial word being typed).
	i := off
	for i > 0 && isIdentByte(text[i-1]) {
		i--
	}
	if i > 0 && text[i-1] == '.' {
		dot = true
		off = i - 1 // the receiver ends at the dot
		// A bare `p.` does not parse, so analyze a patched buffer with a
		// dummy member name when nothing (or a non-identifier) follows the
		// dot. The receiver keeps its span, so member lookup still works.
		if i == len(text) || !isIdentByte(text[i]) {
			patched := text[:off+1] + "zq" + text[off+1:]
			overlay := s.overlaySnapshot()
			overlay[path] = []byte(patched)
			suppress := s.opt.Root == "" && !hasMain(path, overlay)
			pa := analyzeEntry(path, overlay, suppress)
			if pa.art != nil && pa.art.Info != nil {
				items := pa.completionItems(path, off, true)
				if items == nil {
					items = []CompletionItem{}
				}
				s.reply(req.ID, CompletionList{Items: items})
				return
			}
		}
	}
	items := a.completionItems(path, off, dot)
	if items == nil {
		items = []CompletionItem{}
	}
	s.reply(req.ID, CompletionList{Items: items})
}

func isIdentByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func (s *server) onDocumentSymbol(req *request, isRequest bool) {
	if !isRequest {
		return
	}
	var p struct {
		TextDocument TextDocumentIdentifier `json:"textDocument"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		s.reply(req.ID, nil)
		return
	}
	path := cleanPath(uriToPath(p.TextDocument.URI))
	a := s.analysisFor(path)
	if a == nil {
		s.reply(req.ID, []DocumentSymbol{})
		return
	}
	syms := a.documentSymbols(path)
	if syms == nil {
		syms = []DocumentSymbol{}
	}
	s.reply(req.ID, syms)
}

// --- JSON-RPC plumbing -----------------------------------------------------

func (s *server) reply(id json.RawMessage, result any) {
	// A map keeps an explicit null result in the payload (encoding/json's
	// omitempty would drop it, which breaks JSON-RPC's reply shape).
	s.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (s *server) replyError(id json.RawMessage, code int, msg string) {
	s.send(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": msg},
	})
}

func (s *server) send(v any) {
	body, err := json.Marshal(v)
	if err != nil {
		return
	}
	_ = writeMessage(s.w, body)
}

// --- URI helpers -----------------------------------------------------------

// uriToPath converts a file:// URI to a filesystem path.
func uriToPath(uri string) string {
	if uri == "" {
		return ""
	}
	if !strings.HasPrefix(uri, "file://") {
		return uri
	}
	u, err := url.Parse(uri)
	if err != nil {
		return strings.TrimPrefix(uri, "file://")
	}
	return u.Path
}

// pathToURI converts a filesystem path to a file:// URI with proper escaping.
func pathToURI(path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	return u.String()
}
