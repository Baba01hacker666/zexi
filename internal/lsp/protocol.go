// Package lsp implements the Language Server Protocol for zexi programs:
// diagnostics as you type, hover, go-to-definition, completion (including
// `.`-triggered member completion), and document symbols. The server speaks
// JSON-RPC 2.0 with Content-Length framing over any bidirectional stream
// (the `zexic lsp` command wires it to stdio).
package lsp

import "encoding/json"

// Position is a zero-based line and a zero-based UTF-16 code-unit column.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Range is a half-open interval between two positions.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Location is a range inside a document.
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// Diagnostic mirrors the server's diagnostics for the client.
type Diagnostic struct {
	Range    Range  `json:"range"`
	Severity int    `json:"severity"`
	Code     string `json:"code,omitempty"`
	Source   string `json:"source,omitempty"`
	Message  string `json:"message"`
}

// TextDocumentItem is an opened document.
type TextDocumentItem struct {
	URI        string `json:"uri"`
	LanguageID string `json:"languageId"`
	Version    int    `json:"version"`
	Text       string `json:"text"`
}

// TextDocumentIdentifier names a document.
type TextDocumentIdentifier struct {
	URI string `json:"uri"`
}

// VersionedTextDocumentIdentifier names a document at a version.
type VersionedTextDocumentIdentifier struct {
	URI     string `json:"uri"`
	Version int    `json:"version"`
}

// TextDocumentPositionParams locates a position inside a document.
type TextDocumentPositionParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
}

// DidOpenParams is the didOpen notification payload.
type DidOpenParams struct {
	TextDocument TextDocumentItem `json:"textDocument"`
}

// ContentChange is one edit. With full sync the whole Text replaces the file.
type ContentChange struct {
	Range *Range `json:"range,omitempty"`
	Text  string `json:"text"`
}

// DidChangeParams is the didChange notification payload.
type DidChangeParams struct {
	TextDocument   VersionedTextDocumentIdentifier `json:"textDocument"`
	ContentChanges []ContentChange                 `json:"contentChanges"`
}

// DidCloseParams is the didClose notification payload.
type DidCloseParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
}

// DidSaveParams is the didSave notification payload (ignored; we always
// analyze the in-memory buffer).
type DidSaveParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
}

// PublishDiagnosticsParams is sent to the client after every analysis.
type PublishDiagnosticsParams struct {
	URI         string       `json:"uri"`
	Version     int          `json:"version,omitempty"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// InitializeParams is the initialize request payload (fields beyond these
// are ignored).
type InitializeParams struct {
	ProcessID        *int              `json:"processId"`
	RootURI          *string           `json:"rootUri"`
	RootPath         *string           `json:"rootPath"`
	Capabilities     map[string]any    `json:"capabilities"`
	WorkspaceFolders []WorkspaceFolder `json:"workspaceFolders"`
}

// WorkspaceFolder is one workspace root.
type WorkspaceFolder struct {
	URI  string `json:"uri"`
	Name string `json:"name"`
}

// InitializeResult announces server capabilities.
type InitializeResult struct {
	Capabilities ServerCapabilities `json:"capabilities"`
	ServerInfo   *ServerInfo        `json:"serverInfo,omitempty"`
}

// ServerInfo identifies the server.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// ServerCapabilities lists what this server can do.
type ServerCapabilities struct {
	TextDocumentSync       TextDocumentSyncOptions `json:"textDocumentSync"`
	HoverProvider          bool                    `json:"hoverProvider"`
	DefinitionProvider     bool                    `json:"definitionProvider"`
	CompletionProvider     *CompletionOptions      `json:"completionProvider,omitempty"`
	DocumentSymbolProvider bool                    `json:"documentSymbolProvider"`
}

// TextDocumentSyncOptions selects full-document sync.
type TextDocumentSyncOptions struct {
	OpenClose bool `json:"openClose"`
	Change    int  `json:"change"` // 1 = full
	Save      bool `json:"save"`
}

// CompletionOptions advertises completion.
type CompletionOptions struct {
	TriggerCharacters []string `json:"triggerCharacters,omitempty"`
}

// Hover is a hover response.
type Hover struct {
	Contents MarkupContent `json:"contents"`
	Range    *Range        `json:"range,omitempty"`
}

// MarkupContent is markdown hover text.
type MarkupContent struct {
	Kind  string `json:"kind"` // "markdown"
	Value string `json:"value"`
}

// CompletionItem is one completion candidate.
type CompletionItem struct {
	Label         string `json:"label"`
	Kind          int    `json:"kind"`
	Detail        string `json:"detail,omitempty"`
	Documentation string `json:"documentation,omitempty"`
}

// CompletionList is a completion response.
type CompletionList struct {
	IsIncomplete bool             `json:"isIncomplete"`
	Items        []CompletionItem `json:"items"`
}

// Completion item kinds (the LSP enum).
const (
	CmpText       = 1
	CmpMethod     = 2
	CmpFunction   = 3
	CmpField      = 5
	CmpVariable   = 6
	CmpClass      = 7
	CmpInterface  = 8
	CmpModule     = 9
	CmpEnum       = 10
	CmpKeyword    = 14
	CmpStruct     = 23
	CmpEnumMember = 20
	CmpConstant   = 21
)

// DocumentSymbol is a symbol in the outline.
type DocumentSymbol struct {
	Name           string           `json:"name"`
	Detail         string           `json:"detail,omitempty"`
	Kind           int              `json:"kind"`
	Range          Range            `json:"range"`
	SelectionRange Range            `json:"selectionRange"`
	Children       []DocumentSymbol `json:"children,omitempty"`
}

// Symbol kinds (the LSP enum) used here.
const (
	SymFunction   = 12
	SymVariable   = 13
	SymConstant   = 14
	SymStruct     = 23
	SymEnum       = 10
	SymField      = 8
	SymEnumMember = 22
)

// jsonrpcError is a JSON-RPC error object.
type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// request is an incoming JSON-RPC message (request or notification).
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// response is an outgoing JSON-RPC reply.
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

// notification is an outgoing JSON-RPC notification.
type notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}
