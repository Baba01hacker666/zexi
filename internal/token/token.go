// Package token defines the lexical tokens of Zexi: their kinds, the source
// positions they occupy, and the keyword table. It is dependency-free so that
// both the lexer and the parser can share it.
package token

import "fmt"

// Kind enumerates every token kind. The zero value is KindInvalid so that an
// uninitialized Token is obviously wrong.
type Kind int

const (
	KindInvalid Kind = iota

	KindEOF
	KindIdentifier

	// Literals.
	KindInteger
	KindFloat
	KindString
	KindRawString
	KindChar
	KindByteChar

	// Keywords.
	KindAs
	KindBreak
	KindConst
	KindContinue
	KindDefer
	KindElse
	KindEnum
	KindExtern
	KindFalse
	KindFn
	KindFor
	KindIf
	KindImpl
	KindIn
	KindLet
	KindLoop
	KindMatch
	KindMod
	KindMut
	KindPub
	KindReturn
	KindStruct
	KindTrue
	KindType
	KindUnsafe
	KindUse
	KindWhile

	// Operators and punctuation.
	KindPlus
	KindMinus
	KindStar
	KindSlash
	KindPercent
	KindAmp
	KindPipe
	KindCaret
	KindShl
	KindShr
	KindBang
	KindTilde
	KindEq
	KindEqEq
	KindBangEq
	KindLt
	KindLe
	KindGt
	KindGe
	KindAmpAmp
	KindPipePipe
	KindPlusEq
	KindMinusEq
	KindStarEq
	KindSlashEq
	KindPercentEq
	KindAmpEq
	KindPipeEq
	KindCaretEq
	KindShlEq
	KindShrEq
	KindArrow
	KindFatArrow
	KindDot
	KindDotDot
	KindDotDotEq
	KindColonColon
	KindColon
	KindComma
	KindSemicolon
	KindQuestion
	KindAt
	KindHash
	KindUnderscore

	KindLParen
	KindRParen
	KindLBrace
	KindRBrace
	KindLBracket
	KindRBracket
)

// keywords maps reserved words to their kinds. Type names such as i32 are NOT
// keywords: they are predeclared identifiers resolved in the type namespace.
var keywords = map[string]Kind{
	"as":       KindAs,
	"break":    KindBreak,
	"const":    KindConst,
	"continue": KindContinue,
	"defer":    KindDefer,
	"else":     KindElse,
	"enum":     KindEnum,
	"extern":   KindExtern,
	"false":    KindFalse,
	"fn":       KindFn,
	"for":      KindFor,
	"if":       KindIf,
	"impl":     KindImpl,
	"in":       KindIn,
	"let":      KindLet,
	"loop":     KindLoop,
	"match":    KindMatch,
	"mod":      KindMod,
	"mut":      KindMut,
	"pub":      KindPub,
	"return":   KindReturn,
	"struct":   KindStruct,
	"true":     KindTrue,
	"type":     KindType,
	"unsafe":   KindUnsafe,
	"use":      KindUse,
	"while":    KindWhile,
}

// Lookup reports the keyword kind for an identifier, if any. It returns
// (KindIdentifier, false) for an ordinary identifier.
func Lookup(ident string) (Kind, bool) {
	k, ok := keywords[ident]
	if !ok {
		return KindIdentifier, false
	}
	return k, true
}

// String returns a human-readable name for the kind, used in diagnostics and
// in the `tokenize` dump.
func (k Kind) String() string {
	if s, ok := kindNames[k]; ok {
		return s
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}

// IsKeyword reports whether the kind is one of the reserved words.
func (k Kind) IsKeyword() bool {
	return k >= KindAs && k <= KindWhile
}

// IsLiteral reports whether the kind is a literal token.
func (k Kind) IsLiteral() bool {
	return k >= KindInteger && k <= KindByteChar
}

var kindNames = map[Kind]string{
	KindInvalid:    "invalid",
	KindEOF:        "end of file",
	KindIdentifier: "identifier",
	KindInteger:    "integer literal",
	KindFloat:      "float literal",
	KindString:     "string literal",
	KindRawString:  "raw string literal",
	KindChar:       "char literal",
	KindByteChar:   "byte literal",
	KindAs:         "as",
	KindBreak:      "break",
	KindConst:      "const",
	KindContinue:   "continue",
	KindDefer:      "defer",
	KindElse:       "else",
	KindEnum:       "enum",
	KindExtern:     "extern",
	KindFalse:      "false",
	KindFn:         "fn",
	KindFor:        "for",
	KindIf:         "if",
	KindImpl:       "impl",
	KindIn:         "in",
	KindLet:        "let",
	KindLoop:       "loop",
	KindMatch:      "match",
	KindMod:        "mod",
	KindMut:        "mut",
	KindPub:        "pub",
	KindReturn:     "return",
	KindStruct:     "struct",
	KindTrue:       "true",
	KindType:       "type",
	KindUnsafe:     "unsafe",
	KindUse:        "use",
	KindWhile:      "while",
	KindPlus:       "+",
	KindMinus:      "-",
	KindStar:       "*",
	KindSlash:      "/",
	KindPercent:    "%",
	KindAmp:        "&",
	KindPipe:       "|",
	KindCaret:      "^",
	KindShl:        "<<",
	KindShr:        ">>",
	KindBang:       "!",
	KindTilde:      "~",
	KindEq:         "=",
	KindEqEq:       "==",
	KindBangEq:     "!=",
	KindLt:         "<",
	KindLe:         "<=",
	KindGt:         ">",
	KindGe:         ">=",
	KindAmpAmp:     "&&",
	KindPipePipe:   "||",
	KindPlusEq:     "+=",
	KindMinusEq:    "-=",
	KindStarEq:     "*=",
	KindSlashEq:    "/=",
	KindPercentEq:  "%=",
	KindAmpEq:      "&=",
	KindPipeEq:     "|=",
	KindCaretEq:    "^=",
	KindShlEq:      "<<=",
	KindShrEq:      ">>=",
	KindArrow:      "->",
	KindFatArrow:   "=>",
	KindDot:        ".",
	KindDotDot:     "..",
	KindDotDotEq:   "..=",
	KindColonColon: "::",
	KindColon:      ":",
	KindComma:      ",",
	KindSemicolon:  ";",
	KindQuestion:   "?",
	KindAt:         "@",
	KindHash:       "#",
	KindUnderscore: "_",
	KindLParen:     "(",
	KindRParen:     ")",
	KindLBrace:     "{",
	KindRBrace:     "}",
	KindLBracket:   "[",
	KindRBracket:   "]",
}

// Pos is a byte offset into a source file (0-based). It is the canonical way to
// refer to a location; line/column are derived for display only.
type Pos int

// Position is a human-readable location, used in diagnostics.
type Position struct {
	FileID int // index into the driver's file table
	Line   int // 1-based
	Col    int // 1-based, counted in bytes
	Offset Pos // 0-based byte offset
}

func (p Position) String() string {
	return fmt.Sprintf("%d:%d", p.Line, p.Col)
}

// Span is a half-open byte range [Start, End) in one file.
type Span struct {
	Start Pos
	End   Pos
}

// Token is one lexical unit. Lit holds the raw source text of a literal and,
// for identifiers, the identifier text (without unescaping).
type Token struct {
	Kind Kind
	Lit  string
	Pos  Position
	// End offset of the token, so callers can slice the source.
	End Pos
}

func (t Token) String() string {
	switch {
	case t.Kind.IsLiteral() || t.Kind == KindIdentifier:
		return fmt.Sprintf("%s(%q)", t.Kind, t.Lit)
	default:
		return t.Kind.String()
	}
}
