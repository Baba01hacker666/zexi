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

// TestMatchArmCommaOptionalAfterBlock covers the Rust-like rule that a match arm
// whose body ends in `}` may omit its trailing comma.
func TestMatchArmCommaOptionalAfterBlock(t *testing.T) {
	src := `fn main() -> i32 {
    match 1 {
        0 => { println(0) }
        x if x == 1 => if true { println(1) } else { println(2) }
        _ => { println(3) },
    }
    0
}`
	if _, n := parse(t, src); n != 0 {
		t.Fatalf("unexpected diagnostics: %d", n)
	}
	// A non-block body still requires a comma before another arm.
	if _, n := parse(t, "fn main() -> i32 { match 1 { 0 => 1 1 => 2 }; 0 }"); n == 0 {
		t.Fatalf("expected a diagnostic for a missing comma")
	}
}

// TestNegativeLiteralPattern covers `-N` as a match pattern.
func TestNegativeLiteralPattern(t *testing.T) {
	src := `fn f(n: i32) -> i32 {
    match n {
        -128 => 1,
        -1 => 2,
        0 => 3,
        _ => 4,
    }
}`
	if _, n := parse(t, src); n != 0 {
		t.Fatalf("unexpected diagnostics: %d", n)
	}
	// `-` must be followed by a number.
	if _, n := parse(t, "fn main() -> i32 { match 1 { -x => 0, _ => 1 }; 0 }"); n == 0 {
		t.Fatalf("expected a diagnostic for `-x` in a pattern")
	}
}

// TestNewKeywordsAndSyntax covers `until`, `elif`, `defer`, `if let` /
// `while let`, or-patterns, range patterns, `mut` bindings in patterns, and
// tuple-struct construction.
func TestNewKeywordsAndSyntax(t *testing.T) {
	src := `struct Pair(i32, i32);
enum E { V(i32), None }
fn make(i: i32) -> E { if i < 2 { E::V(i) } else { E::None } }
fn grade(n: i32) -> str {
    if n > 90 { "A" } elif n > 50 { "B" } else { "F" }
}
fn f(n: i32) -> i32 {
    match n {
        1 | 2 => 10,
        3..=5 => 30,
        6..9 => 60,
        -5..-1 => 40,
        _ => 0,
    }
}
fn g(e: E) -> i32 {
    match e {
        E::V(mut x) => x,
        E::None => 0,
    }
}
fn main() -> i32 {
    let p = Pair(1, 2);
    let mut i = 0;
    until i >= 3 { i += 1; }
    defer println("done", p.0);
    if let E::V(x) = make(1) {
        println(x);
    } else {
        println("none");
    }
    while let E::V(x) = make(i) {
        i += 1;
        if x > 10 { break; }
    }
    let c = match 'q' {
        'a'..='z' => 1,
        _ => 0,
    };
    println(grade(95), f(4), c);
    0
}`
	if _, n := parse(t, src); n != 0 {
		t.Fatalf("unexpected diagnostics: %d", n)
	}
	// `defer` used as an expression is a syntax error.
	if _, n := parse(t, "fn main() -> i32 { let x = defer println(1); 0 }"); n == 0 {
		t.Fatalf("expected a diagnostic for `defer` in expression position")
	}
}

// TestEasySyntax covers the Python-flavoured surface: statements ended
// by newlines instead of semicolons, default parameter values, named
// arguments, and f-string interpolation.
func TestEasySyntax(t *testing.T) {
	src := `fn greet(name: str, punct: str = "!", times: i64 = 1) -> String {
    f"hi {name}{punct}"
}
fn main() {
    let a = greet("bob")
    let b = greet(name = "ada", times = 2)
    println(a, b)
    let c = 40 +
        2
    println(c)
}`
	if _, n := parse(t, src); n != 0 {
		t.Fatalf("unexpected diagnostics: %d", n)
	}
	// Two statements on one line still need a semicolon between them.
	if _, n := parse(t, "fn main() -> i32 { let a = 1 let b = 2; 0 }"); n == 0 {
		t.Fatal("expected a diagnostic for two statements on one line without `;`")
	}
	// An unterminated interpolation hole is a syntax error.
	if _, n := parse(t, `fn main() -> i32 { println(f"oops {"); 0 }`); n == 0 {
		t.Fatal("expected a diagnostic for unterminated `{` in f-string")
	}
}

func TestImportAndInlineModuleSyntax(t *testing.T) {
	src := `import http.server as server;
import std::http::client;
import "helper.zx" as helper;
mod inline { fn answer() -> i32 { 42 } }
mod external;`
	file, n := parse(t, src)
	if n != 0 {
		t.Fatalf("unexpected diagnostics: %d", n)
	}
	if len(file.Items) != 5 {
		t.Fatalf("expected 5 items, got %d", len(file.Items))
	}
	first, ok := file.Items[0].(*ast.UseDecl)
	if !ok || len(first.Path) != 2 || first.Path[0] != "http" || first.Path[1] != "server" || first.As != "server" {
		t.Fatalf("unexpected dotted import: %#v", file.Items[0])
	}
	second, ok := file.Items[1].(*ast.UseDecl)
	if !ok || len(second.Path) != 3 || second.Path[0] != "std" || second.Path[2] != "client" {
		t.Fatalf("unexpected qualified import: %#v", file.Items[1])
	}
	third, ok := file.Items[2].(*ast.UseDecl)
	if !ok || third.File != "helper.zx" || third.As != "helper" {
		t.Fatalf("unexpected file import: %#v", file.Items[2])
	}
	inline, ok := file.Items[3].(*ast.ModDecl)
	if !ok || !inline.Inline || inline.Name != "inline" || len(inline.Items) != 1 {
		t.Fatalf("unexpected inline module: %#v", file.Items[3])
	}
	external, ok := file.Items[4].(*ast.ModDecl)
	if !ok || external.Inline || external.Name != "external" {
		t.Fatalf("unexpected external module: %#v", file.Items[4])
	}
}

// TestParsesGenericSyntax covers generics on every item, type arguments on
// types, expression turbofish, and the `>>` splitting needed by nested
// instantiations.
func TestParsesGenericSyntax(t *testing.T) {
	src := `struct Pair<A, B> { first: A, second: B }
enum Option<T> { Some(T), None }
type PairI64<B> = Pair<i64, B>;
impl<A, B> Pair<A, B> {
    fn swap(self) -> Pair<B, A> { Pair { first: self.second, second: self.first } }
}
fn id<T>(x: T) -> T { x }
fn nested(x: Vec<Vec<i64>>) -> i64 { 0 }
fn main() -> i32 {
    let a = id::<i64>(1);
    let b = Pair::<i64, i64> { first: 1, second: 2 };
    let c = Vec::<i64>::new();
    0
}`
	file, n := parse(t, src)
	if n != 0 {
		t.Fatalf("unexpected diagnostics: %d", n)
	}
	if len(file.Items) != 7 {
		t.Fatalf("expected 7 items, got %d", len(file.Items))
	}
	st, ok := file.Items[0].(*ast.StructDecl)
	if !ok || len(st.Generics) != 2 || st.Generics[0].Name != "A" || st.Generics[1].Name != "B" {
		t.Fatalf("unexpected struct generics: %#v", file.Items[0])
	}
	en, ok := file.Items[1].(*ast.EnumDecl)
	if !ok || len(en.Generics) != 1 || en.Generics[0].Name != "T" {
		t.Fatalf("unexpected enum generics: %#v", file.Items[1])
	}
	al, ok := file.Items[2].(*ast.TypeAlias)
	if !ok || len(al.Generics) != 1 {
		t.Fatalf("unexpected alias generics: %#v", file.Items[2])
	}
	pt, ok := al.Ty.(*ast.PathType)
	if !ok || len(pt.Args) != 2 {
		t.Fatalf("unexpected alias target: %#v", al.Ty)
	}
	im, ok := file.Items[3].(*ast.ImplBlock)
	if !ok || len(im.Generics) != 2 {
		t.Fatalf("unexpected impl generics: %#v", file.Items[3])
	}
	fn, ok := file.Items[4].(*ast.FnDecl)
	if !ok || len(fn.Generics) != 1 || fn.Generics[0].Name != "T" {
		t.Fatalf("unexpected fn generics: %#v", file.Items[4])
	}
	nested, ok := file.Items[5].(*ast.FnDecl)
	if !ok || len(nested.Params) != 1 {
		t.Fatalf("unexpected nested generic fn: %#v", file.Items[5])
	}
	outer, ok := nested.Params[0].Ty.(*ast.PathType)
	if !ok || len(outer.Args) != 1 {
		t.Fatalf("expected Vec<Vec<i64>>, got %#v", nested.Params[0].Ty)
	}
	inner, ok := outer.Args[0].(*ast.PathType)
	if !ok || len(inner.Args) != 1 {
		t.Fatalf("expected inner Vec<i64>, got %#v", outer.Args[0])
	}

	main, ok := file.Items[6].(*ast.FnDecl)
	if !ok || len(main.Body.Stmts) != 3 {
		t.Fatalf("unexpected main: %#v", file.Items[6])
	}
	// `id::<i64>(1)`: turbofish on a bare name.
	firstLit, ok := main.Body.Stmts[0].(*ast.LetStmt)
	if !ok {
		t.Fatalf("unexpected stmt: %#v", main.Body.Stmts[0])
	}
	call, ok := firstLit.Val.(*ast.CallExpr)
	if !ok {
		t.Fatalf("unexpected call: %#v", firstLit.Val)
	}
	idExpr, ok := call.Callee.(*ast.IdentExpr)
	if !ok || idExpr.Name != "id" || len(idExpr.TypeArgs) != 1 {
		t.Fatalf("unexpected turbofish callee: %#v", call.Callee)
	}
	// `Pair::<i64, i64> { .. }`: turbofish on a struct literal.
	secondLit, ok := main.Body.Stmts[1].(*ast.LetStmt)
	if !ok {
		t.Fatalf("unexpected stmt: %#v", main.Body.Stmts[1])
	}
	lit, ok := secondLit.Val.(*ast.StructLitExpr)
	if !ok || len(lit.TypeArgs) != 2 || len(lit.Fields) != 2 {
		t.Fatalf("unexpected struct literal: %#v", secondLit.Val)
	}
	// `Vec::<i64>::new()`: turbofish on a type-qualified call.
	thirdLit, ok := main.Body.Stmts[2].(*ast.LetStmt)
	if !ok {
		t.Fatalf("unexpected stmt: %#v", main.Body.Stmts[2])
	}
	ctor, ok := thirdLit.Val.(*ast.CallExpr)
	if !ok {
		t.Fatalf("unexpected call: %#v", thirdLit.Val)
	}
	path, ok := ctor.Callee.(*ast.PathExpr)
	if !ok || len(path.Segments) != 2 || path.Segments[1] != "new" || len(path.TypeArgs) != 1 {
		t.Fatalf("unexpected qualified ctor: %#v", ctor.Callee)
	}
}
