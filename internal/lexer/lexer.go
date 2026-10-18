// Package lexer implements a hand-written scanner for Zexi. The scanner is
// deliberately simple and total: given any byte sequence it terminates and
// never panics, reporting lexical errors as diagnostics. This property is
// asserted by the fuzz test in lexer_test.go.
package lexer

import (
	"zexi/internal/diag"
	"zexi/internal/token"
)

// Lexer turns a source buffer into a stream of tokens.
type Lexer struct {
	fileID int
	src    []byte
	off    int // byte offset of the next unread byte
	line   int // 1-based
	col    int // 1-based, byte-counted

	errs []diag.Diagnostic
}

// New returns a Lexer over src. fileID identifies the file for diagnostics.
func New(fileID int, src []byte) *Lexer {
	return &Lexer{fileID: fileID, src: src, line: 1, col: 1}
}

// Errors returns the lexical diagnostics collected so far.
func (l *Lexer) Errors() []diag.Diagnostic { return l.errs }

// All scans the whole input and returns the tokens, including a trailing EOF
// token. It is a convenience for tests and the `tokenize` driver command.
func (l *Lexer) All() []token.Token {
	var toks []token.Token
	for {
		t := l.Next()
		toks = append(toks, t)
		if t.Kind == token.KindEOF {
			return toks
		}
	}
}

// Next returns the next token. At end of input it repeatedly returns EOF.
func (l *Lexer) Next() token.Token {
	l.skipTrivia()

	startPos, startOff := l.pos(), l.off
	if l.atEnd() {
		return token.Token{Kind: token.KindEOF, Pos: startPos, End: token.Pos(startOff)}
	}

	c := l.peek()
	switch {
	// Prefix forms must be checked before the generic identifier case.
	case c == 'r' && l.peekAt(1) == '"':
		return l.scanRawString(startPos, startOff)
	case c == 'b' && l.peekAt(1) == '\'':
		return l.scanChar(startPos, startOff, true)
	case isIdentStart(c):
		return l.scanIdent(startPos, startOff)
	case isDigit(c):
		return l.scanNumber(startPos, startOff)
	case c == '"':
		return l.scanString(startPos, startOff)
	case c == '\'':
		return l.scanChar(startPos, startOff, false)
	default:
		return l.scanOperator(startPos, startOff)
	}
}

// --- trivia ---------------------------------------------------------------

func (l *Lexer) skipTrivia() {
	for !l.atEnd() {
		switch l.peek() {
		case ' ', '\t', '\r', '\n':
			l.advance()
		case '/':
			if l.peekAt(1) == '/' {
				for !l.atEnd() && l.peek() != '\n' {
					l.advance()
				}
			} else if l.peekAt(1) == '*' {
				l.skipBlockComment()
			} else {
				return
			}
		default:
			return
		}
	}
}

// skipBlockComment consumes a /* ... */ comment, supporting nesting. An
// unterminated comment is reported at its start.
func (l *Lexer) skipBlockComment() {
	startOff := l.off
	l.advance() // '/'
	l.advance() // '*'
	depth := 1
	for depth > 0 {
		if l.atEnd() {
			l.errs = append(l.errs, diag.New("E0003", "unterminated block comment").
				WithSpan(token.Span{Start: token.Pos(startOff), End: token.Pos(l.off)}))
			return
		}
		if l.peek() == '/' && l.peekAt(1) == '*' {
			depth++
			l.advance()
			l.advance()
			continue
		}
		if l.peek() == '*' && l.peekAt(1) == '/' {
			depth--
			l.advance()
			l.advance()
			continue
		}
		l.advance()
	}
}

// --- identifiers ----------------------------------------------------------

func (l *Lexer) scanIdent(startPos token.Position, startOff int) token.Token {
	for !l.atEnd() && isIdentContinue(l.peek()) {
		l.advance()
	}
	text := string(l.src[startOff:l.off])
	if text == "_" {
		return l.tok(token.KindUnderscore, "", startPos, startOff)
	}
	if k, ok := token.Lookup(text); ok {
		return l.tok(k, text, startPos, startOff)
	}
	return l.tok(token.KindIdentifier, text, startPos, startOff)
}

// --- numbers --------------------------------------------------------------

func (l *Lexer) scanNumber(startPos token.Position, startOff int) token.Token {
	kind := token.KindInteger

	if l.peek() == '0' {
		switch l.peekAt(1) {
		case 'x', 'X':
			l.advance()
			l.advance()
			if !isHexDigit(l.peek()) {
				l.errorf("E0005", startPos, "hexadecimal literal requires at least one digit")
			}
			l.consumeDigits(isHexDigit)
			return l.tok(kind, l.lit(startOff), startPos, startOff)
		case 'b', 'B':
			l.advance()
			l.advance()
			if !isBinDigit(l.peek()) {
				l.errorf("E0005", startPos, "binary literal requires at least one digit")
			}
			l.consumeDigits(isBinDigit)
			return l.tok(kind, l.lit(startOff), startPos, startOff)
		case 'o', 'O':
			l.advance()
			l.advance()
			if !isOctDigit(l.peek()) {
				l.errorf("E0005", startPos, "octal literal requires at least one digit")
			}
			l.consumeDigits(isOctDigit)
			return l.tok(kind, l.lit(startOff), startPos, startOff)
		}
	}

	l.consumeDigits(isDigit)

	// A fraction requires a digit after the dot, so `1..2` stays a range and
	// `1.foo` stays field access.
	if l.peek() == '.' && isDigit(l.peekAt(1)) {
		kind = token.KindFloat
		l.advance() // '.'
		l.consumeDigits(isDigit)
	}
	if c := l.peek(); c == 'e' || c == 'E' {
		if d := l.peekAt(1); isDigit(d) || ((d == '+' || d == '-') && isDigit(l.peekAt(2))) {
			kind = token.KindFloat
			l.advance() // e
			if l.peek() == '+' || l.peek() == '-' {
				l.advance()
			}
			l.consumeDigits(isDigit)
		}
	}
	return l.tok(kind, l.lit(startOff), startPos, startOff)
}

func (l *Lexer) consumeDigits(pred func(byte) bool) {
	for !l.atEnd() {
		c := l.peek()
		if pred(c) || c == '_' {
			l.advance()
			continue
		}
		return
	}
}

// --- strings and chars ----------------------------------------------------

func (l *Lexer) scanString(startPos token.Position, startOff int) token.Token {
	l.advance() // opening quote
	for {
		if l.atEnd() {
			l.errorf("E0001", startPos, "unterminated string literal")
			break
		}
		c := l.advance()
		if c == '\\' {
			l.advance() // skip the escaped byte; decoding happens later
			continue
		}
		if c == '"' {
			break
		}
		if c == '\n' {
			l.errorf("E0001", startPos, "unterminated string literal")
			break
		}
	}
	// Lit is the inner text (quotes excluded); escapes are decoded downstream.
	inner := l.innerLit(startOff)
	return l.tok(token.KindString, inner, startPos, startOff)
}

func (l *Lexer) scanRawString(startPos token.Position, startOff int) token.Token {
	l.advance() // 'r'
	l.advance() // opening quote
	for {
		if l.atEnd() {
			l.errorf("E0001", startPos, "unterminated raw string literal")
			break
		}
		if l.advance() == '"' {
			break
		}
	}
	inner := l.safeSlice(startOff+2, l.off-1)
	return l.tok(token.KindRawString, inner, startPos, startOff)
}

func (l *Lexer) scanChar(startPos token.Position, startOff int, isByte bool) token.Token {
	if isByte {
		l.advance() // 'b'
	}
	l.advance() // opening quote
	for {
		if l.atEnd() {
			l.errorf("E0001", startPos, "unterminated char literal")
			break
		}
		c := l.advance()
		if c == '\\' {
			l.advance()
			continue
		}
		if c == '\'' {
			break
		}
		if c == '\n' {
			l.errorf("E0001", startPos, "unterminated char literal")
			break
		}
	}
	kind := token.KindChar
	inner := l.innerLit(startOff)
	if isByte {
		kind = token.KindByteChar
		inner = l.safeSlice(startOff+2, l.off-1)
	}
	return l.tok(kind, inner, startPos, startOff)
}

// --- operators ------------------------------------------------------------

func (l *Lexer) scanOperator(startPos token.Position, startOff int) token.Token {
	type entry struct {
		text string
		kind token.Kind
	}
	// Longest-match first.
	table := []entry{
		{"<<=", token.KindShlEq}, {">>=", token.KindShrEq}, {"..=", token.KindDotDotEq},
		{"==", token.KindEqEq}, {"!=", token.KindBangEq}, {"<=", token.KindLe},
		{">=", token.KindGe}, {"&&", token.KindAmpAmp}, {"||", token.KindPipePipe},
		{"+=", token.KindPlusEq}, {"-=", token.KindMinusEq}, {"*=", token.KindStarEq},
		{"/=", token.KindSlashEq}, {"%=", token.KindPercentEq}, {"&=", token.KindAmpEq},
		{"|=", token.KindPipeEq}, {"^=", token.KindCaretEq}, {"<<", token.KindShl},
		{">>", token.KindShr}, {"->", token.KindArrow}, {"=>", token.KindFatArrow},
		{"..", token.KindDotDot}, {"::", token.KindColonColon},
		{"+", token.KindPlus}, {"-", token.KindMinus}, {"*", token.KindStar},
		{"/", token.KindSlash}, {"%", token.KindPercent}, {"&", token.KindAmp},
		{"|", token.KindPipe}, {"^", token.KindCaret}, {"!", token.KindBang},
		{"~", token.KindTilde}, {"=", token.KindEq}, {"<", token.KindLt},
		{">", token.KindGt}, {".", token.KindDot}, {":", token.KindColon},
		{",", token.KindComma}, {";", token.KindSemicolon}, {"?", token.KindQuestion},
		{"@", token.KindAt}, {"#", token.KindHash},
		{"(", token.KindLParen}, {")", token.KindRParen},
		{"{", token.KindLBrace}, {"}", token.KindRBrace},
		{"[", token.KindLBracket}, {"]", token.KindRBracket},
	}
	for _, e := range table {
		if l.hasPrefix(e.text) {
			for range e.text {
				l.advance()
			}
			return l.tok(e.kind, e.text, startPos, startOff)
		}
	}

	bad := l.advance()
	l.errs = append(l.errs, diag.New("E0002", "unexpected character %q", string(bad)).
		WithSpan(token.Span{Start: token.Pos(startOff), End: token.Pos(l.off)}))
	return l.tok(token.KindInvalid, string(bad), startPos, startOff)
}

// --- helpers --------------------------------------------------------------

func (l *Lexer) atEnd() bool { return l.off >= len(l.src) }

func (l *Lexer) peek() byte {
	if l.atEnd() {
		return 0
	}
	return l.src[l.off]
}

func (l *Lexer) peekAt(n int) byte {
	if l.off+n >= len(l.src) {
		return 0
	}
	return l.src[l.off+n]
}

func (l *Lexer) hasPrefix(s string) bool {
	if l.off+len(s) > len(l.src) {
		return false
	}
	return string(l.src[l.off:l.off+len(s)]) == s
}

// advance consumes and returns one byte, maintaining line/column. Newlines
// reset the column.
func (l *Lexer) advance() byte {
	if l.atEnd() {
		return 0
	}
	c := l.src[l.off]
	l.off++
	if c == '\n' {
		l.line++
		l.col = 1
	} else {
		l.col++
	}
	return c
}

func (l *Lexer) pos() token.Position {
	return token.Position{FileID: l.fileID, Line: l.line, Col: l.col, Offset: token.Pos(l.off)}
}

func (l *Lexer) tok(k token.Kind, lit string, start token.Position, startOff int) token.Token {
	return token.Token{Kind: k, Lit: lit, Pos: start, End: token.Pos(l.off)}
}

func (l *Lexer) lit(startOff int) string { return string(l.src[startOff:l.off]) }

// innerLit returns the text between the first and last byte of the token,
// used for quoted literals.
func (l *Lexer) innerLit(startOff int) string {
	return l.safeSlice(startOff+1, l.off-1)
}

// safeSlice returns src[lo:hi] as a string, clamping to the buffer and
// returning "" for an inverted or out-of-range range. Unterminated literals
// make lo > hi, so callers must never index raw here.
func (l *Lexer) safeSlice(lo, hi int) string {
	if lo < 0 {
		lo = 0
	}
	if hi > len(l.src) {
		hi = len(l.src)
	}
	if lo >= hi {
		return ""
	}
	return string(l.src[lo:hi])
}

func (l *Lexer) errorf(code string, pos token.Position, format string, args ...any) {
	span := token.Span{Start: pos.Offset, End: token.Pos(l.off)}
	l.errs = append(l.errs, diag.New(code, format, args...).WithSpan(span))
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentContinue(c byte) bool { return isIdentStart(c) || isDigit(c) }

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isHexDigit(c byte) bool {
	return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func isBinDigit(c byte) bool { return c == '0' || c == '1' }
func isOctDigit(c byte) bool { return c >= '0' && c <= '7' }
