package parser_test

import (
	"testing"

	"zexi/internal/ast"
	"zexi/internal/parser"
)

func parse(t *testing.T, src string) (*ast.File, int) {
	t.Helper()
	file, diags := parser.Parse(0, []byte(src))
	if file == nil {
		t.Fatalf("Parse(%q) returned nil file", src)
	}
	return file, len(diags)
}

func TestParsesFunction(t *testing.T) {
	file, n := parse(t, "fn main() -> i32 { 0 }")
	if n != 0 {
		t.Fatalf("unexpected diagnostics: %d", n)
	}
	if len(file.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(file.Items))
	}
	fn, ok := file.Items[0].(*ast.FnDecl)
	if !ok || fn.Name != "main" {
		t.Fatalf("expected fn main, got %#v", file.Items[0])
	}
	if fn.Body == nil || fn.Body.Tail == nil {
		t.Fatalf("expected a block with a tail expression")
	}
}

func TestBlockLikeStatementNeedsNoSemicolon(t *testing.T) {
	src := `fn main() -> i32 {
    if true { println("a"); }
    let mut i = 0;
    while i < 3 { i += 1; }
    0
}`
	_, n := parse(t, src)
	if n != 0 {
		t.Fatalf("expected no diagnostics, got %d", n)
	}
}

func TestReportsSyntaxError(t *testing.T) {
	if _, n := parse(t, "fn 123 {}"); n == 0 {
		t.Fatal("expected a syntax error")
	}
}

// FuzzParse asserts the parser is total: any byte sequence either parses or
// produces diagnostics, but never panics and always terminates.
func FuzzParse(f *testing.F) {
	seeds := []string{
		"",
		"fn main() -> i32 { 0 }",
		"fn f(a: i32, b: i32) -> i32 { a + b }",
		"struct S { x: i32, y: i32 }",
		"enum E { A, B(i32), C { v: bool } }",
		"impl S { fn m(self) -> i32 { 0 } }",
		"mod m { fn f() {} }",
		"use a::b::c as d;",
		"const X: i32 = 1 + 2 * 3;",
		"type T = [u8; 4];",
		"fn f() { for i in 0..10 { if i > 5 { break; } } }",
		"fn f() { match x { 0 => 1, _ => 2, } }",
		"fn f() { let (a, b) = (1, 2); }",
		"fn f() { unsafe { let p = &x; } }",
		"fn f() -> i32 { {{ 1 }} }",
		"\x00\xff\xfe",
		"fn",
		"{{{{{{",
		"]]]]]]",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		file, _ := parser.Parse(0, []byte(src))
		if file == nil {
			t.Fatalf("Parse(%q) returned nil file", src)
		}
	})
}

// TestMatchGuardsAndJumpsInArmBodies covers `pat if cond => body` arms and bare
// `break`/`continue`/`return` as arm bodies.
func TestMatchGuardsAndJumpsInArmBodies(t *testing.T) {
	src := `fn main() -> i32 {
    for i in 0..3 {
        match i {
            0 => continue,
            1 if i == 1 => break,
            _ => println(i),
        }
    }
    match 1 {
        x if x > 0 => return 0,
        _ => return 1,
    }
}`
	if _, n := parse(t, src); n != 0 {
		t.Fatalf("unexpected diagnostics: %d", n)
	}
}
