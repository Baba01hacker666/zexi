package lexer_test

import (
	"testing"

	"zexi/internal/lexer"
	"zexi/internal/token"
)

// kinds lexes src and returns the token kinds, excluding the trailing EOF.
func kinds(t *testing.T, src string) []token.Kind {
	t.Helper()
	lx := lexer.New(0, []byte(src))
	var out []token.Kind
	for _, tok := range lx.All() {
		if tok.Kind == token.KindEOF {
			break
		}
		out = append(out, tok.Kind)
	}
	if errs := lx.Errors(); len(errs) != 0 {
		t.Fatalf("lex(%q): unexpected errors: %v", src, errs)
	}
	return out
}

func eq(t *testing.T, src string, want ...token.Kind) {
	t.Helper()
	got := kinds(t, src)
	if len(got) != len(want) {
		t.Fatalf("lex(%q): got %v, want %v", src, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("lex(%q): token %d = %v, want %v", src, i, got[i], want[i])
		}
	}
}

func TestKeywordsAndIdentifiers(t *testing.T) {
	eq(t, "let mut x = 1",
		token.KindLet, token.KindMut, token.KindIdentifier, token.KindEq, token.KindInteger)
	// Type names are identifiers, not keywords.
	eq(t, "i32 usize", token.KindIdentifier, token.KindIdentifier)
	// `_` is its own token; `_foo` is an identifier.
	eq(t, "_ _foo", token.KindUnderscore, token.KindIdentifier)
}

func TestOperatorLongestMatch(t *testing.T) {
	eq(t, "<<= >>= ..= => -> ::",
		token.KindShlEq, token.KindShrEq, token.KindDotDotEq,
		token.KindFatArrow, token.KindArrow, token.KindColonColon)
	eq(t, "a << b", token.KindIdentifier, token.KindShl, token.KindIdentifier)
	eq(t, "& &mut", token.KindAmp, token.KindAmp, token.KindMut)
}

func TestNumbers(t *testing.T) {
	eq(t, "0xFF 0b1010 0o755 1_000", token.KindInteger, token.KindInteger, token.KindInteger, token.KindInteger)
	// A digit after the dot makes a float; otherwise the dot stays separate.
	eq(t, "3.14 1e9 2.5e-3", token.KindFloat, token.KindFloat, token.KindFloat)
	eq(t, "1..2", token.KindInteger, token.KindDotDot, token.KindInteger)
}

func TestStringsAndChars(t *testing.T) {
	eq(t, `"hi" r"C:\p" b'x' 'A'`,
		token.KindString, token.KindRawString, token.KindByteChar, token.KindChar)
	// Raw strings do not treat backslashes specially.
	got := kinds(t, `r"a\nb"`)
	if len(got) != 1 || got[0] != token.KindRawString {
		t.Fatalf("raw string lexed as %v", got)
	}
}

func TestCommentsAreTrivia(t *testing.T) {
	eq(t, "1 // line\n2", token.KindInteger, token.KindInteger)
	eq(t, "1 /* a /* nested */ b */ 2", token.KindInteger, token.KindInteger)
	eq(t, "/* only a comment */")
}

func TestErrors(t *testing.T) {
	cases := []struct {
		src  string
		code string
	}{
		{`"unterminated`, "E0001"},
		{"/* open", "E0003"},
		{"`", "E0002"},
	}
	for _, c := range cases {
		lx := lexer.New(0, []byte(c.src))
		lx.All()
		errs := lx.Errors()
		if len(errs) == 0 {
			t.Fatalf("lex(%q): expected an error", c.src)
		}
		if errs[0].Code != c.code {
			t.Fatalf("lex(%q): code = %s, want %s", c.src, errs[0].Code, c.code)
		}
	}
}

// FuzzLexer asserts the scanner is total and well-behaved on arbitrary bytes:
// it terminates, always ends with EOF, and never emits an offset past the
// input. A panic here is a compiler bug (docs/04 §4.11).
func FuzzLexer(f *testing.F) {
	seeds := []string{
		"", "fn main() -> i32 { 0 }", "1..=9", `r"x"`, `b'q'`, "/*/*", "\"", "0x",
		"a::b::c", "<<=>>", "let x: [u8; 4] = [1,2,3,4];",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		lx := lexer.New(0, []byte(src))
		sawEOF := false
		for _, tok := range lx.All() {
			if int(tok.End) > len(src) {
				t.Fatalf("token %v ends past input (%d > %d)", tok, tok.End, len(src))
			}
			if tok.Kind == token.KindEOF {
				sawEOF = true
				break
			}
		}
		if !sawEOF {
			t.Fatalf("lexer did not terminate with EOF for %q", src)
		}
	})
}
