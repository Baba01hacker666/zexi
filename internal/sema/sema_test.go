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

func TestStringUseAfterMove(t *testing.T) {
	src := `fn main() -> i32 {
    let a = String::from("x");
    let b = a;
    println(b.len());
    println(a.len());
    0
}`
	ok, codes := check(t, src)
	if ok || !has(codes, "E0382") {
		t.Fatalf("expected E0382, got %v", codes)
	}
}

func TestStringBorrowDoesNotMove(t *testing.T) {
	src := `fn main() -> i32 {
    let mut a = String::from("x");
    a.push_str("y");
    println(a.len());
    println(a.as_str());
    0
}`
	if ok, codes := check(t, src); !ok {
		t.Fatalf("expected clean, got %v", codes)
	}
}

func TestStringMethodArgType(t *testing.T) {
	src := `fn main() -> i32 {
    let mut s = String::from("x");
    s.push_str(5);
    0
}`
	ok, codes := check(t, src)
	if ok || !has(codes, "E0308") {
		t.Fatalf("expected E0308, got %v", codes)
	}
}

func TestStringNotCopyable(t *testing.T) {
	src := `#[copy]
struct S { name: String }
fn main() -> i32 {
    let s = S { name: String::from("x") };
    0
}`
	ok, codes := check(t, src)
	if ok || !has(codes, "E0204") {
		t.Fatalf("expected E0204, got %v", codes)
	}
}

func TestEnumOwningPayloadRejected(t *testing.T) {
	src := `enum E { S(String), A }
fn main() -> i32 {
    let e = E::S(String::from("x"));
    match e { E::A => println(0), E::S(s) => println(s.len()) }
    0
}`
	ok, codes := check(t, src)
	if ok || !has(codes, "E0900") {
		t.Fatalf("expected E0900, got %v", codes)
	}
}

func TestConstAdaptsToDeclaredIntType(t *testing.T) {
	src := `const N: usize = 4;
fn main() -> i32 {
    let xs: [i32; N] = [0; N];
    println(len(xs));
    0
}`
	if ok, codes := check(t, src); !ok {
		t.Fatalf("expected clean, got %v", codes)
	}
}

func TestConstMismatchedTypeRejected(t *testing.T) {
	src := `const N: i32 = true;
fn main() -> i32 { 0 }`
	ok, codes := check(t, src)
	if ok || !has(codes, "E0308") {
		t.Fatalf("expected E0308, got %v", codes)
	}
}

func TestArrayLitAdoptsExpectedElemType(t *testing.T) {
	src := `fn main() -> i32 {
    let xs: [i64; 3] = [0; 3];
    println(xs[0]);
    let ys: [i64; 2] = [1, 2];
    println(ys[1]);
    0
}`
	if ok, codes := check(t, src); !ok {
		t.Fatalf("expected clean, got %v", codes)
	}
}

// TestNewDiagnostics covers the checks added in the robustness pass.
func TestNewDiagnostics(t *testing.T) {
	cases := []struct {
		name string
		src  string
		code string
	}{
		{"return-value-in-unit-fn",
			"fn f() { return 5; }\nfn main() -> i32 { f(); 0 }", "E0308"},
		{"match-on-string",
			"fn main() -> i32 { let s = String::from(\"x\"); match s { _ => println(1) } 0 }", "E0308"},
		{"duplicate-catch-all",
			"enum E { A, B }\nfn main() -> i32 { match E::A { _ => println(1), _ => println(2) } 0 }", "E0001"},
		{"duplicate-variant-arm",
			"enum E { A, B }\nfn main() -> i32 { match E::A { E::A => println(1), E::A => println(2), _ => println(3) } 0 }", "E0001"},
		{"float-percent",
			"fn main() -> i32 { let x = 5.0 % 2.0; println(x); 0 }", "E0308"},
		{"negative-array-len",
			"fn main() -> i32 { let xs: [i32; -1] = [0; -1]; println(len(xs)); 0 }", "E0080"},
		{"method-without-self",
			"struct F { n: i32 }\nimpl F { fn make() -> i32 { 7 } }\nfn main() -> i32 { let f = F { n: 1 }; println(f.make()); 0 }", "E0599"},
		{"ref-to-temporary",
			"fn main() -> i32 { let r = &String::from(\"x\"); 0 }", "E0716"},
		{"recursive-type",
			"struct S { s: S }\nfn main() -> i32 { 0 }", "E0072"},
		{"array-repeat-non-copy",
			"fn main() -> i32 { let s = String::from(\"hi\"); let xs: [String; 3] = [s; 3]; println(xs[0]); 0 }", "E0204"},
		{"int-literal-out-of-range",
			"fn main() -> i32 { let x: u8 = 300; println(x); 0 }", "E0080"},
		{"nested-enum-pattern",
			"enum I { A, B }\nenum O { W(I), N }\nfn main() -> i32 { let o = O::W(I::B); match o { O::W(I::A) => println(1), O::N => println(2), _ => println(3) } 0 }", "E0900"},
		{"array-literal-moves-twice",
			"fn main() -> i32 { let a = String::from(\"x\"); let xs = [a, a]; println(xs[0]); 0 }", "E0382"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, codes := check(t, tc.src)
			if ok || !has(codes, tc.code) {
				t.Fatalf("expected %s, got %v", tc.code, codes)
			}
		})
	}
}

// TestErgonomicsAccepted covers the additive syntax changes.
func TestErgonomicsAccepted(t *testing.T) {
	cases := []struct{ name, src string }{
		{"for-in-array",
			"fn main() -> i32 { let xs: [i32; 3] = [1, 2, 3]; let mut t = 0; for x in xs { t += x; } println(t); 0 }"},
		{"for-in-array-non-copy-rejected",
			"fn main() -> i32 { let xs = [String::from(\"a\")]; for x in xs { println(x); } 0 }"},
		{"string-concat",
			"fn f(a: str) -> String { return \"hi \" + a + \"!\"; }\nfn main() -> i32 { let s = String::from(\"bob\"); println(f(s)); 0 }"},
		{"string-pluseq",
			"fn main() -> i32 { let mut s = String::from(\"a\"); s += \"b\"; println(s); 0 }"},
		{"string-as-str-arg",
			"fn f(a: str) -> usize { return len(a); }\nfn main() -> i32 { let s = String::from(\"abcd\"); println(f(s)); 0 }"},
		{"string-eq-str",
			"fn main() -> i32 { let s = String::from(\"x\"); if s == \"x\" { println(1); } 0 }"},
		{"len-sugar",
			"fn main() -> i32 { let xs: [i32; 4] = [0; 4]; let s = String::from(\"hi\"); println(xs.len()); println(len(s)); 0 }"},
		{"match-self-ref",
			"enum E { A, B }\nimpl E { fn v(&self) -> i32 { match self { E::A => 1, E::B => 2 } } }\nfn main() -> i32 { let e = E::B; println(e.v()); 0 }"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, codes := check(t, tc.src)
			rejected := has(codes, "E0507")
			if tc.name == "for-in-array-non-copy-rejected" {
				if ok || !rejected {
					t.Fatalf("expected E0507, got %v", codes)
				}
				return
			}
			if !ok {
				t.Fatalf("expected clean, got %v", codes)
			}
		})
	}
}

// TestImmutableBindings covers the `mut` enforcement pass.
func TestImmutableBindings(t *testing.T) {
	rejected := []struct{ name, src string }{
		{"assign-to-let",
			"fn main() -> i32 { let x = 1; x = 2; println(x); 0 }"},
		{"compound-assign-to-let",
			"fn main() -> i32 { let x = 1; x += 2; println(x); 0 }"},
		{"field-assign-through-let",
			"struct S { n: i32 }\nfn main() -> i32 { let s = S { n: 1 }; s.n = 2; println(s.n); 0 }"},
		{"index-assign-through-let",
			"fn main() -> i32 { let xs: [i32; 2] = [1, 2]; xs[0] = 9; println(xs[0]); 0 }"},
		{"ref-mut-of-let",
			"fn f(p: &mut i32) { *p = 1; }\nfn main() -> i32 { let x = 0; f(&mut x); println(x); 0 }"},
		{"str-method-on-let",
			"fn main() -> i32 { let s = String::from(\"a\"); s.push(98); println(s); 0 }"},
		{"str-pluseq-on-let",
			"fn main() -> i32 { let s = String::from(\"a\"); s += \"b\"; println(s); 0 }"},
		{"assign-field-in-ref-self",
			"struct C { n: i32 }\nimpl C { fn bad(&self) { self.n += 1; } }\nfn main() -> i32 { let c = C { n: 0 }; c.bad(); 0 }"},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			ok, codes := check(t, tc.src)
			if ok || !has(codes, "E0596") {
				t.Fatalf("expected E0596, got %v", codes)
			}
		})
	}

	accepted := []struct{ name, src string }{
		{"assign-to-mut-let",
			"fn main() -> i32 { let mut x = 1; x = 2; x += 3; println(x); 0 }"},
		{"field-assign-through-mut",
			"struct S { n: i32 }\nfn main() -> i32 { let mut s = S { n: 1 }; s.n = 2; println(s.n); 0 }"},
		{"ref-mut-of-mut-let",
			"fn f(p: &mut i32) { *p = 1; }\nfn main() -> i32 { let mut x = 0; f(&mut x); println(x); 0 }"},
		{"assign-through-mut-handle",
			"struct S { n: i32 }\nfn main() -> i32 { let mut s = S { n: 1 }; let r = &mut s; r.n = 5; println(s.n); 0 }"},
		{"mutate-self-in-ref-mut-self",
			"struct C { n: i32 }\nimpl C { fn inc(&mut self) { self.n += 1; } }\nfn main() -> i32 { let mut c = C { n: 0 }; c.inc(); println(c.n); 0 }"},
		{"param-mutation-permissive",
			"fn f(mut n: i32) -> i32 { n += 1; n }\nfn main() -> i32 { println(f(1)); 0 }"},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			if ok, codes := check(t, tc.src); !ok {
				t.Fatalf("expected clean, got %v", codes)
			}
		})
	}
}

// TestMatchGuards covers exhaustiveness and typing of `pat if cond` arms.
func TestMatchGuards(t *testing.T) {
	rejected := []struct{ name, src, code string }{
		{"guarded-arm-not-exhaustive",
			"enum E { A, B }\nfn main() -> i32 { match E::A { _ if true => println(1) } 0 }", "E0004"},
		{"guarded-variant-not-exhaustive",
			"enum E { A, B }\nfn main() -> i32 { match E::A { E::A if true => println(1), E::B => println(2) } 0 }", "E0004"},
		{"int-guard-needs-catch-all",
			"fn main() -> i32 { match 1 { x if x > 0 => println(1) } 0 }", "E0004"},
		{"non-bool-guard",
			"fn main() -> i32 { match 1 { x if x => println(1), _ => println(2) } 0 }", "E0308"},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			ok, codes := check(t, tc.src)
			if ok || !has(codes, tc.code) {
				t.Fatalf("expected %s, got %v", tc.code, codes)
			}
		})
	}

	accepted := []struct{ name, src string }{
		{"guard-with-fallthrough-and-catch-all",
			"fn main() -> i32 { match 5 { x if x > 10 => println(1), x if x > 3 => println(2), _ => println(3) } 0 }"},
		{"same-variant-twice-when-guarded",
			"enum E { A(i32), B }\nfn main() -> i32 { match E::A(1) { E::A(n) if n > 0 => println(1), E::A(n) => println(n), E::B => println(0) } 0 }"},
		{"guarded-wildcard-then-uncovered",
			"enum E { A, B }\nfn main() -> i32 { match E::A { _ if true => println(1), E::A => println(2), E::B => println(3) } 0 }"},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			if ok, codes := check(t, tc.src); !ok {
				t.Fatalf("expected clean, got %v", codes)
			}
		})
	}
}

// TestLiteralAndArrayBounds covers range checking of (negative) integer
// literals and array lengths.
func TestLiteralAndArrayBounds(t *testing.T) {
	rejected := []struct{ name, src, code string }{
		{"neg-repeat-count",
			"fn main() -> i32 { let a = [0; -1]; println(len(a)); 0 }", "E0080"},
		{"huge-repeat-count",
			"fn main() -> i32 { let a = [0; 99999999999]; println(len(a)); 0 }", "E0080"},
		{"huge-array-type",
			"fn main() -> i32 { let a: [i32; 99999999999] = [0; 3]; println(len(a)); 0 }", "E0080"},
		{"neg-array-type",
			"fn main() -> i32 { let a: [i8; -3] = [0; 3]; println(len(a)); 0 }", "E0080"},
		{"u8-underflow",
			"fn main() -> i32 { let x: u8 = -1; println(x); 0 }", "E0080"},
		{"i8-underflow",
			"fn main() -> i32 { let x: i8 = -129; println(x); 0 }", "E0080"},
		{"i8-overflow",
			"fn main() -> i32 { let x: i8 = 128; println(x); 0 }", "E0080"},
		{"ref-to-temporary-field",
			"struct P { a: i32 }\nfn mk() -> P { return P { a: 1 }; }\nfn main() -> i32 { let r = &mk().a; println(*r); 0 }", "E0716"},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			ok, codes := check(t, tc.src)
			if ok || !has(codes, tc.code) {
				t.Fatalf("expected %s, got %v", tc.code, codes)
			}
		})
	}

	accepted := []struct{ name, src string }{
		{"i8-min-literal",
			"fn main() -> i32 { let x: i8 = -128; println(x); 0 }"},
		{"i32-min-literal",
			"fn main() -> i32 { let x: i32 = -2147483648; println(x); 0 }"},
		{"i64-min-literal",
			"fn main() -> i32 { let x: i64 = -9223372036854775808; println(x); 0 }"},
		{"inferred-negative",
			"fn main() -> i32 { let x = -5; println(x); 0 }"},
		{"field-of-temporary",
			"struct P { a: i32 }\nfn mk() -> P { return P { a: 1 }; }\nfn main() -> i32 { println(mk().a); 0 }"},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			if ok, codes := check(t, tc.src); !ok {
				t.Fatalf("expected clean, got %v", codes)
			}
		})
	}
}

// TestConstRange covers range checking of folded integer constants.
func TestConstRange(t *testing.T) {
	rejected := []struct{ name, src, code string }{
		{"u8-overflow", "const X: u8 = 300; fn main() -> i32 { println(X); 0 }", "E0080"},
		{"u8-sum-overflow", "const X: u8 = 200 + 100; fn main() -> i32 { println(X); 0 }", "E0080"},
		{"i8-overflow", "const X: i8 = 200; fn main() -> i32 { println(X); 0 }", "E0080"},
		{"u8-negative", "const X: u8 = -1; fn main() -> i32 { println(X); 0 }", "E0080"},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			ok, codes := check(t, tc.src)
			if ok || !has(codes, tc.code) {
				t.Fatalf("expected %s, got %v", tc.code, codes)
			}
		})
	}

	accepted := []struct{ name, src string }{
		{"u8-max", "const X: u8 = 255; fn main() -> i32 { println(X); 0 }"},
		{"i8-min", "const X: i8 = -128; fn main() -> i32 { println(X); 0 }"},
		{"i64-min", "const X: i64 = -9223372036854775808; fn main() -> i32 { println(X); 0 }"},
		{"u64-max", "const X: u64 = 18446744073709551615; fn main() -> i32 { println(X); 0 }"},
		{"inferred", "const X = -5000000000; fn main() -> i32 { println(X); 0 }"},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			if ok, codes := check(t, tc.src); !ok {
				t.Fatalf("expected clean, got %v", codes)
			}
		})
	}
}

// TestReturnTypePropagation covers bidirectional inference for a function body's
// trailing expression and the branches of a trailing `if`/`match`.
func TestReturnTypePropagation(t *testing.T) {
	accepted := []struct{ name, src string }{
		{"int-literal-tail", "fn f() -> i64 { 5 }\nfn main() -> i32 { println(f()); 0 }"},
		{"u8-literal-tail", "fn f() -> u8 { 200 }\nfn main() -> i32 { println(f() as i32); 0 }"},
		{"if-branches", "fn f(c: bool) -> i64 { if c { 1 } else { 2 } }\nfn main() -> i32 { println(f(true)); 0 }"},
		{"match-arms", "fn f(n: i32) -> i64 { match n { 0 => 10, _ => 20 } }\nfn main() -> i32 { println(f(0)); 0 }"},
		{"annotated-let-if", "fn main() -> i32 { let x: i64 = if true { 1 } else { 2 }; println(x); 0 }"},
		{"annotated-let-match", "fn main() -> i32 { let x: u8 = match 0 { 0 => 1, _ => 2 }; println(x as i32); 0 }"},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			if ok, codes := check(t, tc.src); !ok {
				t.Fatalf("expected clean, got %v", codes)
			}
		})
	}

	rejected := []struct{ name, src, code string }{
		{"literal-too-big-for-tail", "fn f() -> u8 { 300 }\nfn main() -> i32 { println(f() as i32); 0 }", "E0080"},
		{"bad-branch", "fn f(c: bool) -> i64 { if c { 1 } else { \"s\" } }\nfn main() -> i32 { println(f(true)); 0 }", "E0308"},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			ok, codes := check(t, tc.src)
			if ok || !has(codes, tc.code) {
				t.Fatalf("expected %s, got %v", tc.code, codes)
			}
		})
	}
}

// TestEscapeDiagnostics covers malformed escapes in string/char/byte literals.
func TestEscapeDiagnostics(t *testing.T) {
	bad := []string{
		`fn main() -> i32 { println("a\q"); 0 }`,
		`fn main() -> i32 { println("a\xZZ"); 0 }`,
		`fn main() -> i32 { println("a\u{zz}"); 0 }`,
	}
	for _, src := range bad {
		ok, codes := check(t, src)
		if ok || !has(codes, "E0001") {
			t.Fatalf("expected E0001 for %q, got %v", src, codes)
		}
	}
	good := []string{
		`fn main() -> i32 { println("a\tb\nc\\d\"e\x41\u{1F600}"); 0 }`,
		`fn main() -> i32 { println(r"a\qb"); 0 }`,
		`fn main() -> i32 { let b = b'\n'; println(b as i32); 0 }`,
		`fn main() -> i32 { let c = 'é'; println(c); 0 }`,
	}
	for _, src := range good {
		if ok, codes := check(t, src); !ok {
			t.Fatalf("expected clean for %q, got %v", src, codes)
		}
	}
}

// TestAssociatedFunctions covers `Type::name(args)` calls that resolve to an
// `impl` function declared without `self`.
func TestAssociatedFunctions(t *testing.T) {
	accepted := []struct{ name, src string }{
		{"struct-ctor",
			"struct P { x: i32, y: i32 }\nimpl P { fn new(x: i32, y: i32) -> P { P { x, y } } }\nfn main() -> i32 { let p = P::new(1, 2); println(p.x + p.y); 0 }"},
		{"static-calls-static",
			"struct P { x: i32 }\nimpl P { fn zero() -> P { P { x: 0 } } fn one() -> P { let p = P::zero(); P { x: p.x + 1 } } }\nfn main() -> i32 { println(P::one().x); 0 }"},
		{"enum-associated-fn",
			"enum E { A, B }\nimpl E { fn make() -> E { E::A } }\nfn main() -> i32 { match E::make() { E::A => println(1), E::B => println(2) } 0 }"},
		{"string-arg-coercion",
			"struct S { s: String }\nimpl S { fn of(name: str) -> S { S { s: String::from(name) } } }\nfn main() -> i32 { let s = S::of(\"hi\"); println(s.s); 0 }"},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			if ok, codes := check(t, tc.src); !ok {
				t.Fatalf("expected clean, got %v", codes)
			}
		})
	}

	rejected := []struct{ name, src, code string }{
		{"self-method-via-colons",
			"struct P { x: i32 }\nimpl P { fn get(&self) -> i32 { self.x } }\nfn main() -> i32 { P::get(); 0 }", "E0599"},
		{"wrong-arg-count",
			"struct P { x: i32 }\nimpl P { fn new(x: i32) -> P { P { x } } }\nfn main() -> i32 { println(P::new(1, 2).x); 0 }", "E0061"},
		{"wrong-arg-type",
			"struct P { x: i32 }\nimpl P { fn new(x: i32) -> P { P { x } } }\nfn main() -> i32 { println(P::new(\"s\").x); 0 }", "E0308"},
		{"unknown-type-still-reported",
			"fn main() -> i32 { Nope::f(); 0 }", "E0412"},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			ok, codes := check(t, tc.src)
			if ok || !has(codes, tc.code) {
				t.Fatalf("expected %s, got %v", tc.code, codes)
			}
		})
	}
}

// TestBreakValues covers `break <expr>` as the value of a `loop`.
func TestBreakValues(t *testing.T) {
	accepted := []struct{ name, src string }{
		{"simple",
			"fn main() -> i32 { let x = loop { break 5; }; println(x); 0 }"},
		{"typed-by-let",
			"fn main() -> i32 { let x: i64 = loop { break 5; }; println(x); 0 }"},
		{"typed-by-return",
			"fn f() -> i64 { loop { break 5; } }\nfn main() -> i32 { println(f()); 0 }"},
		{"conditional",
			"fn main() -> i32 { let x = loop { if true { break 5; } }; println(x); 0 }"},
		{"owned-value",
			"fn main() -> i32 { let s: String = loop { break String::from(\"a\"); }; println(s); 0 }"},
		{"str-value",
			"fn main() -> i32 { let s = loop { break \"a\"; }; println(s); 0 }"},
		{"nested-loop",
			"fn main() -> i32 { let r = loop { loop { break; } break 3; }; println(r); 0 }"},
		{"break-in-match-arm",
			"fn main() -> i32 { let mut n = 0; let x = loop { n += 1; match n { 2 => break n * 10, _ => {} } }; println(x); 0 }"},
		{"plain-loop-still-unit",
			"fn main() -> i32 { loop { break; } println(1); 0 }"},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			if ok, codes := check(t, tc.src); !ok {
				t.Fatalf("expected clean, got %v", codes)
			}
		})
	}

	rejected := []struct{ name, src, code string }{
		{"while-does-not-yield",
			"fn main() -> i32 { while true { break 5; } 0 }", "E0308"},
		{"for-does-not-yield",
			"fn main() -> i32 { for i in 0..3 { break i; } 0 }", "E0308"},
		{"mismatched-break-types",
			"fn main() -> i32 { let x = loop { if true { break 1; } break 2.0; }; println(x); 0 }", "E0308"},
		{"bare-break-in-valued-loop",
			"fn main() -> i32 { let x = loop { if true { break 1; } break; }; println(x); 0 }", "E0308"},
		{"value-outside-loop",
			"fn main() -> i32 { break 5; 0 }", "E0268"},
		{"break-type-mismatch",
			"fn main() -> i32 { let x: i32 = loop { break \"no\"; }; 0 }", "E0308"},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			ok, codes := check(t, tc.src)
			if ok || !has(codes, tc.code) {
				t.Fatalf("expected %s, got %v", tc.code, codes)
			}
		})
	}
}

// TestPrintAndStringSugar covers multi-value `println` and concatenating a
// number, `bool`, or `char` onto a string.
func TestPrintAndStringSugar(t *testing.T) {
	accepted := []struct{ name, src string }{
		{"println-many",
			`fn main() -> i32 { println("a", 1, 2.5, true, 'c'); 0 }`},
		{"print-many",
			`fn main() -> i32 { print("a", 1); println(); 0 }`},
		{"println-many-strings",
			`fn main() -> i32 { println(String::from("a"), "b"); 0 }`},
		{"println-none",
			`fn main() -> i32 { println(); 0 }`},
		{"str-plus-int",
			`fn main() -> i32 { println("n = " + 1); 0 }`},
		{"int-plus-str",
			`fn main() -> i32 { println(1 + " items"); 0 }`},
		{"str-plus-float-bool-char",
			`fn main() -> i32 { println("x" + 1.5 + true + 'c'); 0 }`},
		{"string-plus-int",
			`fn main() -> i32 { println(String::from("n = ") + 1); 0 }`},
		{"push-eq-scalar",
			`fn main() -> i32 { let mut s = String::from("v: "); s += 7; println(s); 0 }`},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			if ok, codes := check(t, tc.src); !ok {
				t.Fatalf("expected clean, got %v", codes)
			}
		})
	}

	rejected := []struct{ name, src, code string }{
		{"unprintable-struct",
			`struct P { x: i32 } fn main() -> i32 { println("p", P { x: 1 }); 0 }`, "E0277"},
		{"str-plus-struct",
			`struct P { x: i32 } fn main() -> i32 { println("p" + P { x: 1 }); 0 }`, "E0308"},
		{"push-eq-struct",
			`struct P { x: i32 } fn main() -> i32 { let mut s = String::from("a"); s += P { x: 1 }; 0 }`, "E0308"},
		{"int-plus-int",
			`fn main() -> i32 { println(1 + 2); 0 }`, ""},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			ok, codes := check(t, tc.src)
			if tc.code == "" {
				if !ok {
					t.Fatalf("expected clean, got %v", codes)
				}
				return
			}
			if ok || !has(codes, tc.code) {
				t.Fatalf("expected %s, got %v", tc.code, codes)
			}
		})
	}
}
