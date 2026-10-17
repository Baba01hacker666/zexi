package sema_test

import (
	"testing"

	"zexi/internal/diag"
	"zexi/internal/parser"
	"zexi/internal/sema"
)

// check parses and type-checks src, returning whether it is clean and the
// diagnostic codes that were produced.
func check(t *testing.T, src string) (ok bool, codes []string) {
	t.Helper()
	file, pd := parser.Parse(0, []byte(src))
	if file == nil {
		t.Fatalf("parser returned nil for %q", src)
	}
	if len(pd) > 0 {
		t.Fatalf("unexpected parse diagnostics for %q: %v", src, codesOf(pd))
	}
	_, ds := sema.Check(file)
	return len(ds) == 0, codesOf(ds)
}

func codesOf(ds []diag.Diagnostic) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Code)
	}
	return out
}

func has(codes []string, want string) bool {
	for _, c := range codes {
		if c == want {
			return true
		}
	}
	return false
}

func TestUseAfterMove(t *testing.T) {
	src := `struct S { x: i64 }
fn main() -> i32 {
    let a = S { x: 1 };
    let b = a;
    println(b.x);
    println(a.x);
    0
}`
	ok, codes := check(t, src)
	if ok {
		t.Fatal("expected a use-after-move error")
	}
	if !has(codes, "E0382") {
		t.Fatalf("expected E0382, got %v", codes)
	}
}

func TestMoveReinitAndBorrow(t *testing.T) {
	// Reassignment reinitializes; `&self` methods borrow rather than move.
	src := `struct S { x: i64 }
impl S { fn get(&self) -> i64 { return self.x; } }
fn main() -> i32 {
    let mut a = S { x: 1 };
    let b = a;
    a = S { x: 2 };
    println(a.get());
    println(b.get());
    0
}`
	if ok, codes := check(t, src); !ok {
		t.Fatalf("expected clean, got %v", codes)
	}
}

func TestCopyOptIn(t *testing.T) {
	src := `#[copy]
struct P { x: i64 }
fn main() -> i32 {
    let a = P { x: 1 };
    let b = a;
    println(a.x + b.x);
    0
}`
	if ok, codes := check(t, src); !ok {
		t.Fatalf("expected clean, got %v", codes)
	}
}

func TestNonExhaustiveMatch(t *testing.T) {
	src := `enum E { A, B }
fn f(e: E) -> i32 { match e { E::A => 1 } }
fn main() -> i32 { f(E::A) }`
	ok, codes := check(t, src)
	if ok || !has(codes, "E0004") {
		t.Fatalf("expected E0004, got %v", codes)
	}
}

func TestConstantOutOfBounds(t *testing.T) {
	src := `fn main() -> i32 {
    let xs: [i32; 3] = [1, 2, 3];
    println(xs[5]);
    0
}`
	ok, codes := check(t, src)
	if ok || !has(codes, "E0502") {
		t.Fatalf("expected E0502, got %v", codes)
	}
}

func TestUnknownMethod(t *testing.T) {
	src := `struct S { x: i64 }
fn main() -> i32 { let s = S { x: 1 }; println(s.nope()); 0 }`
	ok, codes := check(t, src)
	if ok || !has(codes, "E0599") {
		t.Fatalf("expected E0599, got %v", codes)
	}
}
