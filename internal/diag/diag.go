// Package diag defines compiler diagnostics: a stable code, a severity, a
// primary span, and optional notes/help. The rendering lived here so that the
// CLI and a future language server share one implementation.
package diag

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"zexi/internal/token"
)

// Severity classifies a diagnostic.
type Severity int

const (
	Error Severity = iota
	Warning
	Note
)

func (s Severity) String() string {
	switch s {
	case Warning:
		return "warning"
	case Note:
		return "note"
	default:
		return "error"
	}
}

// Diagnostic is a single compiler message. Code is a stable identifier such as
// "E0308" so tooling can key off it; it may be empty for ad-hoc notes.
type Diagnostic struct {
	Severity Severity
	Code     string
	Message  string
	Span     token.Span
	Notes    []string
	Help     string
}

// New builds an error diagnostic.
func New(code, format string, args ...any) Diagnostic {
	return Diagnostic{
		Severity: Error,
		Code:     code,
		Message:  fmt.Sprintf(format, args...),
	}
}

// WithSpan attaches a span, returning the diagnostic for chaining.
func (d Diagnostic) WithSpan(s token.Span) Diagnostic {
	d.Span = s
	return d
}

// WithHelp attaches a help suggestion.
func (d Diagnostic) WithHelp(format string, args ...any) Diagnostic {
	d.Help = fmt.Sprintf(format, args...)
	return d
}

// WithNote appends a note.
func (d Diagnostic) WithNote(format string, args ...any) Diagnostic {
	d.Notes = append(d.Notes, fmt.Sprintf(format, args...))
	return d
}

// File is the minimal view of a source file the renderer needs. Base is the
// offset of the file's first byte in the shared span space: multi-file
// programs assign each file a disjoint range, so one span identifies both its
// file and the position inside it. Single-file programs use Base 0.
type File struct {
	Name string
	Src  []byte
	Base int
}

// Emitter collects diagnostics and renders them. Rendered output is a pure
// function of the collected diagnostics plus the file table, which keeps
// `--message-format` a view change and nothing more.
type Emitter struct {
	files  map[int]File
	diags  []Diagnostic
	sorted bool
}

// NewEmitter returns an Emitter with an empty file table.
func NewEmitter() *Emitter {
	return &Emitter{files: map[int]File{}, sorted: true}
}

// AddFile registers a source file at span offset 0 and returns its ID. It is
// a convenience for single-file compilation; multi-file programs use
// AddFileAt so every file gets its own span range.
func (e *Emitter) AddFile(name string, src []byte) int {
	return e.AddFileAt(name, src, 0)
}

// AddFileAt registers a source file whose first byte sits at base in the
// shared span space and returns its ID.
func (e *Emitter) AddFileAt(name string, src []byte, base int) int {
	id := len(e.files)
	e.files[id] = File{Name: name, Src: src, Base: base}
	return id
}

// Resolve maps a span to its file and to 0-based byte offsets inside that
// file's source. It reports ok=false when no registered file covers the span.
func (e *Emitter) Resolve(s token.Span) (f File, start, end int, ok bool) {
	off := int(s.Start)
	for _, cand := range e.files {
		if off >= cand.Base && off <= cand.Base+len(cand.Src) {
			start = off - cand.Base
			end = int(s.End) - cand.Base
			if end < start {
				end = start
			}
			return cand, start, end, true
		}
	}
	return File{}, 0, 0, false
}

// Files returns the registered files in registration order (id order).
func (e *Emitter) Files() []File {
	out := make([]File, 0, len(e.files))
	for i := 0; i < len(e.files); i++ {
		out = append(out, e.files[i])
	}
	return out
}

// FirstFileID returns the id of the first registered file, or -1 if none has
// been added. It is a convenience for single-file compilation.
func (e *Emitter) FirstFileID() int {
	if len(e.files) == 0 {
		return -1
	}
	return 0
}

// Report records a diagnostic. Passing zero Diagnostics is a no-op.
func (e *Emitter) Report(ds ...Diagnostic) {
	if len(ds) == 0 {
		return
	}
	e.diags = append(e.diags, ds...)
	e.sorted = false
}

// HasErrors reports whether any diagnostic has Error severity.
func (e *Emitter) HasErrors() bool {
	for _, d := range e.diags {
		if d.Severity == Error {
			return true
		}
	}
	return false
}

// Count returns the number of diagnostics of the given severity.
func (e *Emitter) Count(s Severity) int {
	n := 0
	for _, d := range e.diags {
		if d.Severity == s {
			n++
		}
	}
	return n
}

// Diags returns the collected diagnostics in render order. It lets tests and
// tooling (an LSP, a fixture runner) match on codes and messages without
// re-parsing rendered text.
func (e *Emitter) Diags() []Diagnostic {
	e.sortBySpan()
	out := make([]Diagnostic, len(e.diags))
	copy(out, e.diags)
	return out
}

func (e *Emitter) sortBySpan() {
	if e.sorted {
		return
	}
	sort.SliceStable(e.diags, func(i, j int) bool {
		a, b := e.diags[i], e.diags[j]
		if a.Span.Start != b.Span.Start {
			return a.Span.Start < b.Span.Start
		}
		return a.Span.End < b.Span.End
	})
	e.sorted = true
}

// Render writes all diagnostics in rustc-like form to w.
func (e *Emitter) Render(w io.Writer, fileID int) {
	e.sortBySpan()
	for _, d := range e.diags {
		e.renderOne(w, d, fileID)
	}
}

// RenderJSON writes each diagnostic as one JSON object per line (JSONL) with
// its code, severity, message, file/line/col, and notes/help. It is a view of
// the same data Render prints — `--message-format=json` is a view change and
// nothing more — for editors, CI, and other tools.
func (e *Emitter) RenderJSON(w io.Writer, fileID int) {
	e.sortBySpan()
	enc := json.NewEncoder(w)
	for _, d := range e.diags {
		file, start, _, ok := e.fileFor(d.Span, fileID)
		if !ok {
			continue
		}
		line, col, _, _ := locate(file, start, start)
		out := map[string]any{
			"severity": d.Severity.String(),
			"code":     d.Code,
			"message":  d.Message,
			"file":     file.Name,
			"line":     line,
			"col":      col,
		}
		if len(d.Notes) > 0 {
			out["notes"] = d.Notes
		}
		if d.Help != "" {
			out["help"] = d.Help
		}
		_ = enc.Encode(out) // Encode appends the newline
	}
}

// fileFor resolves the file a span points into — the one whose base range
// covers it — and returns the span's byte offsets inside that file. When the
// span is unclaimed (an ad-hoc diagnostic with no location), it falls back to
// the given file ID and treats the span as local to it.
func (e *Emitter) fileFor(s token.Span, fileID int) (f File, start, end int, ok bool) {
	if f, start, end, ok := e.Resolve(s); ok {
		return f, start, end, true
	}
	if f, found := e.files[fileID]; found {
		start = min(max(int(s.Start), 0), len(f.Src))
		end = min(max(int(s.End), start), len(f.Src))
		return f, start, end, true
	}
	return File{}, 0, 0, false
}

func (e *Emitter) renderOne(w io.Writer, d Diagnostic, fileID int) {
	head := d.Severity.String()
	if d.Code != "" {
		_, _ = fmt.Fprintf(w, "%s[%s]: %s\n", head, d.Code, d.Message)
	} else {
		_, _ = fmt.Fprintf(w, "%s: %s\n", head, d.Message)
	}

	file, start, end, ok := e.fileFor(d.Span, fileID)
	if ok {
		line, col, text, caret := locate(file, start, end)
		if text != "" {
			_, _ = fmt.Fprintf(w, "  --> %s:%d:%d\n", file.Name, line, col)
			_, _ = fmt.Fprintf(w, "   |\n")
			_, _ = fmt.Fprintf(w, "%2d | %s\n", line, text)
			_, _ = fmt.Fprintf(w, "   | %s%s\n", strings.Repeat(" ", col-1), caret)
		}
	}

	for _, n := range d.Notes {
		_, _ = fmt.Fprintf(w, "   = note: %s\n", n)
	}
	if d.Help != "" {
		_, _ = fmt.Fprintf(w, "   = help: %s\n", d.Help)
	}
	_, _ = fmt.Fprintln(w)
}

// locate converts local byte offsets into a 1-based line/col plus the source
// line text and a caret run. It is deliberately forgiving: a zero span yields
// no snippet.
func locate(f File, start, end int) (line, col int, text, caret string) {
	if len(f.Src) == 0 || start < 0 {
		return 0, 0, "", ""
	}
	start = min(start, len(f.Src))
	line, col = 1, 1
	lineStart := 0
	for i := range start {
		if f.Src[i] == '\n' {
			line++
			lineStart = i + 1
			col = 1
		} else {
			col++
		}
	}
	lineEnd := lineStart
	for lineEnd < len(f.Src) && f.Src[lineEnd] != '\n' {
		lineEnd++
	}
	text = string(f.Src[lineStart:lineEnd])

	n := max(end-start, 1)
	// Do not run the caret past the end of the line (spans may cover a whole
	// file), and cap absurdly long runs so output stays readable.
	if avail := lineEnd - (lineStart + col - 1); avail > 0 && n > avail {
		n = avail
	}
	const maxCaret = 120
	if n > maxCaret {
		n = maxCaret
	}
	caret = strings.Repeat("^", n)
	return line, col, text, caret
}
