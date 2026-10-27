package ast_test

import (
	"reflect"
	"testing"

	"zexi/internal/ast"
	"zexi/internal/parser"
)

func TestCloneIsDeepAndFaithful(t *testing.T) {
	src := `const N: usize = 3;
struct Pair<A, B> { first: A, second: B }
enum Opt<T> { Some(T), None }
impl Pair<i64, i64> {
    fn sum(&self) -> i64 { self.first + self.second }
}
fn pick<T>(a: T, b: T, choose_a: bool) -> T {
    if choose_a { return a; }
    b
}
fn main() -> i32 {
    let mut p = Pair { first: 1, second: 2 };
    let xs: [i64; N] = [1, 2, 3];
    let o = Opt::Some(p.sum());
    match o {
        Opt::Some(v) if v > 0 => println(v),
        Opt::None => { println(0); }
        _ => {}
    }
    for i in 0..N { p.first += i as i64; }
    let f: fn(i64) -> i64 = pick::<i64>;
    let r = &mut p.first;
    let q = *r;
    println(q);
    println(f(1));
    0
}`
	file, diags := parser.Parse(0, 0, []byte(src))
	if len(diags) != 0 {
		t.Fatalf("parse diagnostics: %v", diags)
	}
	clone := ast.Clone(file)
	if !reflect.DeepEqual(file, clone) {
		t.Fatalf("Clone is not structurally faithful")
	}

	// Mutating the clone must not touch the original.
	origFn, ok := file.Items[len(file.Items)-1].(*ast.FnDecl)
	if !ok {
		t.Fatalf("last item is %T, want *ast.FnDecl", file.Items[len(file.Items)-1])
	}
	cloneFn, ok := clone.Items[len(clone.Items)-1].(*ast.FnDecl)
	if !ok {
		t.Fatalf("clone's last item is %T, want *ast.FnDecl", clone.Items[len(clone.Items)-1])
	}
	if cloneFn == origFn {
		t.Fatal("Clone returned the same node pointer")
	}
	cloneFn.Name = "renamed"
	cloneFn.Body.Stmts = nil
	if origFn.Name != "main" {
		t.Fatalf("original function was mutated: name = %q", origFn.Name)
	}
	if len(origFn.Body.Stmts) == 0 {
		t.Fatal("original statements were mutated")
	}

	// A generic declaration clone must not share its Generics slice.
	origStruct, ok := file.Items[1].(*ast.StructDecl)
	if !ok {
		t.Fatalf("item 1 is %T, want *ast.StructDecl", file.Items[1])
	}
	cloneStruct, ok := clone.Items[1].(*ast.StructDecl)
	if !ok {
		t.Fatalf("clone's item 1 is %T, want *ast.StructDecl", clone.Items[1])
	}
	cloneStruct.Generics[0].Name = "Changed"
	if origStruct.Generics[0].Name != "A" {
		t.Fatal("generic parameter slice is shared")
	}
}
