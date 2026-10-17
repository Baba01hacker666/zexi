// Package parser turns a token stream into an AST. It is a recursive-descent
// parser with a Pratt (precedence-climbing) expression parser, matching the
// grammar in docs/02.
//
// Status (M0): not yet implemented. Parse records a single "unimplemented"
// diagnostic and returns an empty file so that the driver, diagnostics, and
// golden-test harness can be exercised end to end before the grammar lands.
package parser

import (
	"zexi/internal/ast"
	"zexi/internal/diag"
	"zexi/internal/lexer"
	"zexi/internal/token"
)

// Parse lexes and parses src into an AST, returning any diagnostics. A non-empty
// diagnostic slice may still be accompanied by a partial AST for tooling.
func Parse(fileID int, src []byte) (*ast.File, []diag.Diagnostic) {
	lx := lexer.New(fileID, src)
	toks := lx.All()

	file := &ast.File{Sp: token.Span{Start: 0, End: token.Pos(len(src))}}
	diags := append([]diag.Diagnostic{}, lx.Errors()...)

	// TODO(M0): recursive-descent over toks -> items; Pratt for expressions.
	_ = toks
	diags = append(diags, diag.New("E0900",
		"the parser is not implemented yet (design milestone M0)").
		WithSpan(file.Sp).
		WithNote("this repository currently ships the specification and a working lexer").
		WithHelp("run `zexi tokenize <file>` to inspect the front end today"))
	return file, diags
}
