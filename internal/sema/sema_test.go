package sema_test

import (
	"testing"

	"zexi/internal/diag"
	"zexi/internal/parser"
	"zexi/internal/sema"
	"zexi/internal/types"
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

func TestEnumOwningPayloadAccepted(t *testing.T) {
	// Enum variants may carry resource-owning payloads; a `match` moves the
	// payload into the binding (see the codegen move-glue tests).
	src := `enum E { S(String), A }
fn main() -> i32 {
    let e = E::S(String::from("x"));
    match e { E::A => println(0), E::S(s) => println(s.len()) }
    0
}`
	if ok, codes := check(t, src); !ok {
		t.Fatalf("expected clean, got %v", codes)
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

// TestMatchBoolCharAndDuplicates covers `match` on `bool`/`char` and the
// rejection of duplicate literal arms (which would be duplicate C `case`s).
func TestMatchBoolCharAndDuplicates(t *testing.T) {
	accepted := []struct{ name, src string }{
		{"bool-both-arms",
			"fn main() -> i32 { let b = true; match b { true => println(1), false => println(0) } 0 }"},
		{"bool-catch-all",
			"fn main() -> i32 { match 1 > 2 { true => println(1), _ => println(0) } 0 }"},
		{"bool-as-value",
			"fn main() -> i32 { let n = match 3 > 2 { true => 10, false => 20 }; println(n); 0 }"},
		{"char-match",
			`fn main() -> i32 { match (98 as char) { 'a' => println(1), 'b' => println(2), _ => println(0) } 0 }`},
		{"bool-guard-catch-all",
			"fn main() -> i32 { let b = false; match b { x if x => println(1), _ => println(0) } 0 }"},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			if ok, codes := check(t, tc.src); !ok {
				t.Fatalf("expected clean, got %v", codes)
			}
		})
	}

	rejected := []struct{ name, src, code string }{
		{"duplicate-int-arm",
			"fn main() -> i32 { match 1 { 1 => println(1), 1 => println(2), _ => println(3) } 0 }", "E0001"},
		{"duplicate-int-arm-different-spelling",
			"fn main() -> i32 { match 1 { 1 => println(1), 0x1 => println(2), _ => println(3) } 0 }", "E0001"},
		{"duplicate-bool-arm",
			"fn main() -> i32 { match true { true => println(1), true => println(2), _ => println(3) } 0 }", "E0001"},
		{"bool-not-exhaustive",
			"fn main() -> i32 { match true { true => println(1) } 0 }", "E0004"},
		{"char-not-exhaustive",
			`fn main() -> i32 { match 'a' { 'a' => println(1) } 0 }`, "E0004"},
		{"bool-pattern-on-int",
			"fn main() -> i32 { match 1 { true => println(1), _ => println(0) } 0 }", "E0308"},
		{"int-pattern-on-bool",
			"fn main() -> i32 { match true { 1 => println(1), _ => println(0) } 0 }", "E0308"},
		{"char-pattern-on-bool",
			`fn main() -> i32 { match true { 'a' => println(1), _ => println(0) } 0 }`, "E0308"},
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

// TestNegativePatterns covers negative integer patterns and range checking.
func TestNegativePatterns(t *testing.T) {
	accepted := []struct{ name, src string }{
		{"negative-literal",
			"fn main() -> i32 { match -1 { -1 => println(1), _ => println(0) } 0 }"},
		{"negative-smallest-i8",
			"fn f(n: i8) -> i32 { match n { -128 => 1, _ => 0 } }\nfn main() -> i32 { println(f(-128)); 0 }"},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			if ok, codes := check(t, tc.src); !ok {
				t.Fatalf("expected clean, got %v", codes)
			}
		})
	}

	rejected := []struct{ name, src, code string }{
		{"negative-on-unsigned",
			"fn f(n: u8) -> i32 { match n { -1 => 1, _ => 0 } }\nfn main() -> i32 { println(f(3)); 0 }", "E0080"},
		{"literal-too-large",
			"fn f(n: i8) -> i32 { match n { 300 => 1, _ => 0 } }\nfn main() -> i32 { println(f(3)); 0 }", "E0080"},
		{"duplicate-negative",
			"fn main() -> i32 { match -1 { -1 => println(1), -1 => println(2), _ => println(3) } 0 }", "E0001"},
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

// TestIntLiteralInFloatContext covers an untyped integer literal adopting a
// float type from its context.
func TestIntLiteralInFloatContext(t *testing.T) {
	accepted := []struct{ name, src string }{
		{"let-annotation", "fn main() -> i32 { let x: f64 = 1; println(x); 0 }"},
		{"let-annotation-neg", "fn main() -> i32 { let x: f64 = -1; println(x); 0 }"},
		{"return-type", "fn f() -> f64 { 1 }\nfn main() -> i32 { println(f()); 0 }"},
		{"call-arg", "fn f(x: f64) -> f64 { x }\nfn main() -> i32 { println(f(2)); 0 }"},
		{"struct-field", "struct P { x: f64 }\nfn main() -> i32 { let p = P { x: 1 }; println(p.x); 0 }"},
		{"arith-mixed", "fn main() -> i32 { println(1 + 2.5); 0 }"},
		{"compare-mixed", "fn main() -> i32 { let x = 2.0; println(x == 2); 0 }"},
		{"f32-context", "fn main() -> i32 { let x: f32 = 3; println(x); 0 }"},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			if ok, codes := check(t, tc.src); !ok {
				t.Fatalf("expected clean, got %v", codes)
			}
		})
	}

	if ok, codes := check(t, "fn main() -> i32 { let x: f64 = 99999999999999999999; println(x); 0 }"); ok || !has(codes, "E0080") {
		t.Fatalf("expected E0080 for an oversized literal, got %v", codes)
	}
}

func TestCharLiteralMustBeOneChar(t *testing.T) {
	for _, src := range []string{
		`fn main() -> i32 { let c = 'ab'; 0 }`,
		`fn main() -> i32 { let c = ''; 0 }`,
		"fn main() -> i32 { let b = b'\\u{1F600}'; 0 }",
		"fn main() -> i32 { let c = '\\u{D800}'; 0 }",
		`fn main() -> i32 { match 'x' { 'ab' => 1, _ => 0 } }`,
	} {
		ok, codes := check(t, src)
		if ok || !has(codes, "E0001") {
			t.Fatalf("%q: expected E0001, got ok=%v codes=%v", src, ok, codes)
		}
	}
	// Valid forms still check cleanly.
	for _, src := range []string{
		`fn main() -> i32 { let c = 'a'; let b = b'x'; let u = '\u{1F600}'; 0 }`,
	} {
		if ok, codes := check(t, src); !ok {
			t.Fatalf("%q: expected clean, got %v", src, codes)
		}
	}
}

func TestTypeAliases(t *testing.T) {
	src := `type Meters = i64;
type Distance = Meters;
type Scores = [i32; 2];
fn add(a: Meters, b: Meters) -> Meters { a + b }
fn main() -> i32 {
    let d: Distance = 40;
    let s: Scores = [5, 6];
    println(add(d, 2) + s[1] as i64);
    0
}`
	if ok, codes := check(t, src); !ok {
		t.Fatalf("expected clean, got %v", codes)
	}
}

func TestCyclicTypeAlias(t *testing.T) {
	src := `type A = B;
type B = A;
fn main() -> i32 { 0 }`
	ok, codes := check(t, src)
	if ok || !has(codes, "E0072") {
		t.Fatalf("expected E0072, got ok=%v codes=%v", ok, codes)
	}
}

func TestConstForwardReference(t *testing.T) {
	src := `const LIMIT = BASE + 2;
const BASE = 40;
fn main() -> i32 { println(LIMIT); 0 }`
	if ok, codes := check(t, src); !ok {
		t.Fatalf("expected clean, got %v", codes)
	}
	// A genuinely non-constant initializer is still rejected.
	ok, codes := check(t, `const X = f(1);
fn f(n: i32) -> i32 { n }
fn main() -> i32 { 0 }`)
	if ok || !has(codes, "E0602") {
		t.Fatalf("expected E0602, got ok=%v codes=%v", ok, codes)
	}
}

func TestTupleStructConstruction(t *testing.T) {
	src := `struct Pair(i32, i32);
fn main() -> i32 { let p = Pair(3, 4); println(p.0 + p.1); 0 }`
	if ok, codes := check(t, src); !ok {
		t.Fatalf("expected clean, got %v", codes)
	}
	ok, codes := check(t, `struct Pair(i32, i32);
fn main() -> i32 { let p = Pair(3); 0 }`)
	if ok || !has(codes, "E0061") {
		t.Fatalf("expected E0061 for arity, got ok=%v codes=%v", ok, codes)
	}
	ok, codes = check(t, `struct Named { x: i32 }
fn main() -> i32 { let p = Named(3); 0 }`)
	if ok || !has(codes, "E0061") {
		t.Fatalf("expected E0061 for named struct called as tuple, got ok=%v codes=%v", ok, codes)
	}
}

func TestEnumAssociatedFnViaDotDoesNotPanic(t *testing.T) {
	// Regression: this used to dereference a nil struct pointer and crash
	// the compiler when the receiver was an enum.
	src := `enum E { A, B }
impl E { fn make() -> E { E::A } }
fn main() -> i32 { let e = E::A; println(e.make()); 0 }`
	ok, codes := check(t, src)
	if ok || !has(codes, "E0599") {
		t.Fatalf("expected E0599, got ok=%v codes=%v", ok, codes)
	}
}

func TestOrAndRangePatterns(t *testing.T) {
	src := `enum E { A, B, C }
fn f(n: i32) -> i32 {
    match n {
        1 | 2 => 10,
        3..=5 => 30,
        6..9 => 60,
        _ => 0,
    }
}
fn g(e: E) -> i32 {
    match e {
        E::A | E::B => 1,
        E::C => 2,
    }
}
fn main() -> i32 { f(2) + g(E::B) }`
	if ok, codes := check(t, src); !ok {
		t.Fatalf("expected clean, got %v", codes)
	}
}

func TestOrPatternCannotBind(t *testing.T) {
	src := `fn main() -> i32 {
    match 1 {
        x | _ => x,
    }
}`
	ok, codes := check(t, src)
	if ok || !has(codes, "E0900") {
		t.Fatalf("expected E0900, got ok=%v codes=%v", ok, codes)
	}
}

func TestEmptyRangePattern(t *testing.T) {
	src := `fn main() -> i32 {
    match 1 {
        5..=1 => 1,
        _ => 0,
    }
}`
	ok, codes := check(t, src)
	if ok || !has(codes, "E0080") {
		t.Fatalf("expected E0080, got ok=%v codes=%v", ok, codes)
	}
}

func TestIfLetAndWhileLet(t *testing.T) {
	src := `enum E { V(i32), None }
fn make(i: i32) -> E { if i < 2 { E::V(i) } else { E::None } }
fn main() -> i32 {
    let mut total = 0;
    if let E::V(x) = make(1) {
        total += x;
    } else {
        total += 100;
    }
    let mut i = 0;
    while let E::V(x) = make(i) {
        total += x;
        i += 1;
    }
    total
}`
	if ok, codes := check(t, src); !ok {
		t.Fatalf("expected clean, got %v", codes)
	}
}

func TestUntilAndElif(t *testing.T) {
	src := `fn grade(n: i32) -> str {
    if n > 90 { "A" } elif n > 50 { "B" } else { "F" }
}
fn main() -> i32 {
    let mut i = 0;
    until i >= 3 { i += 1; }
    println(grade(95), i);
    0
}`
	if ok, codes := check(t, src); !ok {
		t.Fatalf("expected clean, got %v", codes)
	}
}

func TestDefer(t *testing.T) {
	src := `fn main() -> i32 {
    let mut s = String::from("x");
    defer println(s);
    defer println("later");
    s.push_str("y");
    0
}`
	if ok, codes := check(t, src); !ok {
		t.Fatalf("expected clean, got %v", codes)
	}
}

func TestDefaultAndNamedArgs(t *testing.T) {
	src := `fn area(w: i64, h: i64 = 10) -> i64 { w * h }
fn main() -> i32 { println(area(5), area(5, h = 3), area(h = 4, w = 2)); 0 }`
	if ok, codes := check(t, src); !ok {
		t.Fatalf("expected clean, got %v", codes)
	}
	// Computed constant defaults are fine.
	ok, codes := check(t, `fn f(a: i32, b: i32 = 20 + 22) -> i32 { a + b }
fn main() -> i32 { println(f(0)); 0 }`)
	if !ok {
		t.Fatalf("expected clean const default, got %v", codes)
	}
	// Unknown parameter name.
	ok, codes = check(t, `fn area(w: i64, h: i64 = 10) -> i64 { w * h }
fn main() -> i32 { println(area(5, z = 1)); 0 }`)
	if ok || !has(codes, "E0061") {
		t.Fatalf("expected E0061 for unknown name, got ok=%v codes=%v", ok, codes)
	}
	// Same parameter twice (positionally and by name).
	ok, codes = check(t, `fn area(w: i64, h: i64 = 10) -> i64 { w * h }
fn main() -> i32 { println(area(5, w = 2)); 0 }`)
	if ok || !has(codes, "E0062") {
		t.Fatalf("expected E0062 for duplicate, got ok=%v codes=%v", ok, codes)
	}
	// A required argument is still required.
	ok, codes = check(t, `fn area(w: i64, h: i64 = 10) -> i64 { w * h }
fn main() -> i32 { println(area()); 0 }`)
	if ok || !has(codes, "E0061") {
		t.Fatalf("expected E0061 for missing arg, got ok=%v codes=%v", ok, codes)
	}
	// Defaults must be constant expressions.
	ok, codes = check(t, `fn g() -> i32 { 1 }
fn f(a: i32, b: i32 = g()) -> i32 { a + b }
fn main() -> i32 { println(f(1)); 0 }`)
	if ok || !has(codes, "E0602") {
		t.Fatalf("expected E0602 for non-const default, got ok=%v codes=%v", ok, codes)
	}
	// A parameter without a default may not follow one with a default.
	ok, codes = check(t, `fn f(a: i32 = 1, b: i32) -> i32 { a + b }
fn main() -> i32 { println(f(1, 2)); 0 }`)
	if ok || !has(codes, "E0061") {
		t.Fatalf("expected E0061 for non-trailing default, got ok=%v codes=%v", ok, codes)
	}
	// Named arguments are not accepted by builtins.
	ok, codes = check(t, `fn main() -> i32 { println(x = 1); 0 }`)
	if ok || !has(codes, "E0061") {
		t.Fatalf("expected E0061 for named arg to println, got ok=%v codes=%v", ok, codes)
	}
}

func TestFStrings(t *testing.T) {
	src := `fn main() -> i32 {
    let name = "zexi";
    let s = f"hi {name}, {1 + 2} + {2.5} = fun? {true} {'!'}";
    println(s);
    0
}`
	if ok, codes := check(t, src); !ok {
		t.Fatalf("expected clean, got %v", codes)
	}
	// Interpolating a String borrows it: it stays usable afterwards.
	ok, codes := check(t, `fn main() -> i32 {
    let s = String::from("x");
    println(f"{s}");
    println(s);
    0
}`)
	if !ok {
		t.Fatalf("expected clean String interpolation, got %v", codes)
	}
	// Non-printable values cannot be interpolated.
	ok, codes = check(t, `struct P { x: i32 }
fn main() -> i32 { let p = P { x: 1 }; println(f"{p}"); 0 }`)
	if ok || !has(codes, "E0277") {
		t.Fatalf("expected E0277 for struct interpolation, got ok=%v codes=%v", ok, codes)
	}
}

func TestCBridgeBuiltins(t *testing.T) {
	// cstr borrows a String (it is not moved), from_cstr owns a copy.
	src := `extern "C" fn puts2(s: *u8) -> i32;
fn main() -> i32 {
    let s = String::from("hi");
    let n = puts2(cstr(s));
    println(s);
    let back = from_cstr(cstr(s));
    println(back, n);
    0
}`
	if ok, codes := check(t, src); !ok {
		t.Fatalf("expected clean, got %v", codes)
	}
	// cstr needs a string, from_cstr needs a pointer.
	ok, codes := check(t, `fn main() -> i32 { let p = cstr(42); 0 }`)
	if ok || !has(codes, "E0308") {
		t.Fatalf("expected E0308 for cstr(int), got ok=%v codes=%v", ok, codes)
	}
	ok, codes = check(t, `fn main() -> i32 { let s = from_cstr("literal"); println(s); 0 }`)
	if ok || !has(codes, "E0308") {
		t.Fatalf("expected E0308 for from_cstr(str), got ok=%v codes=%v", ok, codes)
	}
}

func TestStrMatch(t *testing.T) {
	src := `fn kind(cmd: str) -> i32 {
    match cmd {
        "quit" | "exit" => 0,
        "help" => 1,
        _ => 2,
    }
}
fn main() -> i32 { println(kind("help")); 0 }`
	if ok, codes := check(t, src); !ok {
		t.Fatalf("expected clean, got %v", codes)
	}
	// String matches need a catch-all.
	ok, codes := check(t, `fn main() -> i32 {
    let n = match "x" { "x" => 1 };
    println(n);
    0
}`)
	if ok || !has(codes, "E0004") {
		t.Fatalf("expected E0004, got ok=%v codes=%v", ok, codes)
	}
	// Non-string patterns do not fit a string scrutinee.
	ok, codes = check(t, `fn main() -> i32 {
    let n = match "x" { 1 => 1, _ => 0 };
    println(n);
    0
}`)
	if ok || !has(codes, "E0308") {
		t.Fatalf("expected E0308, got ok=%v codes=%v", ok, codes)
	}
}

func TestArgBuiltins(t *testing.T) {
	src := `fn main() -> i32 {
    if arg_count() > 1 { println(arg_at(1)); }
    0
}`
	if ok, codes := check(t, src); !ok {
		t.Fatalf("expected clean, got %v", codes)
	}
	ok, codes := check(t, `fn main() -> i32 { println(arg_at("x")); 0 }`)
	if ok || !has(codes, "E0308") {
		t.Fatalf("expected E0308, got ok=%v codes=%v", ok, codes)
	}
}

func TestDidYouMeanStillErrors(t *testing.T) {
	// A mistyped function name is E0425 (with a "did you mean" hint in
	// the message), not a panic or a silent pass.
	ok, codes := check(t, `fn main() -> i32 { pritnln("hi"); 0 }`)
	if ok || !has(codes, "E0425") {
		t.Fatalf("expected E0425, got ok=%v codes=%v", ok, codes)
	}
	ok, codes = check(t, `fn main() -> i32 { let total = 1; println(totla); 0 }`)
	if ok || !has(codes, "E0425") {
		t.Fatalf("expected E0425, got ok=%v codes=%v", ok, codes)
	}
}

func TestGenericTemplatesAreDeclaredNotEmitted(t *testing.T) {
	src := `struct Pair<A, B> { first: A, second: B }
enum Opt<T> { Some(T), None }
fn id<T>(x: T) -> T { x }
fn main() -> i32 {
    let p = Pair { first: 1, second: true };
    let o = Opt::Some(2);
    0
}`
	file, pd := parser.Parse(0, []byte(src))
	if len(pd) > 0 {
		t.Fatalf("unexpected parse diagnostics: %v", codesOf(pd))
	}
	info, ds := sema.Check(file)
	if len(ds) > 0 {
		t.Fatalf("unexpected diagnostics: %v", codesOf(ds))
	}
	id := info.Fns["id"]
	if id == nil || !id.Generic || len(id.Generics) != 1 || id.Generics[0] != "T" {
		t.Fatalf("generic fn not registered as a template: %#v", id)
	}
	if id.Params[0].Kind != types.ParamKind || id.Ret.Kind != types.ParamKind {
		t.Fatalf("template signature did not resolve to parameter types: %s", id.Sig())
	}
	st := info.Structs["Pair"]
	if st == nil || !st.Generic || len(st.Generics) != 2 {
		t.Fatalf("generic struct not registered as a template: %#v", st)
	}
	en := info.Enums["Opt"]
	if en == nil || !en.Generic || len(en.Generics) != 1 {
		t.Fatalf("generic enum not registered as a template: %#v", en)
	}
	if len(info.StructOrd) != 1 || len(info.EnumOrd) != 1 {
		t.Fatalf("expected exactly the two instantiations to be queued, got %d structs and %d enums", len(info.StructOrd), len(info.EnumOrd))
	}
	for _, s := range info.StructOrd {
		if s.Generic || s.Base != "Pair" || len(s.Args) != 2 || len(s.Fields) != 2 {
			t.Fatalf("struct instantiation not concrete: %#v", s)
		}
	}
	for _, e := range info.EnumOrd {
		if e.Generic || e.Base != "Opt" || len(e.Args) != 1 {
			t.Fatalf("enum instantiation not concrete: %#v", e)
		}
	}
	for _, fn := range info.FnOrder {
		if fn == id {
			t.Fatal("generic template must not be in FnOrder")
		}
	}
}

func TestGenericBoundsRejected(t *testing.T) {
	_, codes := check(t, `fn max<T: Ord>(a: T, b: T) -> T { a }
fn main() -> i32 { 0 }`)
	if !has(codes, "E0900") {
		t.Fatalf("expected E0900 for a bound, got %v", codes)
	}
}

func TestDuplicateTypeParameterRejected(t *testing.T) {
	_, codes := check(t, `fn f<T, T>(x: T) -> T { x }
fn main() -> i32 { 0 }`)
	if !has(codes, "E0428") {
		t.Fatalf("expected E0428 for a duplicate parameter, got %v", codes)
	}
}

func TestGenericImplMethodInstantiation(t *testing.T) {
	src := `struct Pair<T> { v: T }
impl<T> Pair<T> { fn get(&self) -> T { self.v } }
fn main() -> i32 {
    let p = Pair { v: 1 };
    println(p.get());
    0
}`
	file, pd := parser.Parse(0, []byte(src))
	if len(pd) > 0 {
		t.Fatalf("unexpected parse diagnostics: %v", codesOf(pd))
	}
	info, ds := sema.Check(file)
	if len(ds) > 0 {
		t.Fatalf("unexpected diagnostics: %v", codesOf(ds))
	}
	tmpl := info.Methods["Pair"]["get"]
	if tmpl == nil || !tmpl.Generic {
		t.Fatalf("method template not registered: %#v", tmpl)
	}
	found := false
	for _, fn := range info.FnOrder {
		if fn.Name == "get" && fn.Recv != nil && fn.Recv.St != nil && fn.Recv.St.Base == "Pair" && len(fn.Recv.St.Args) == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("no `Pair<i64>::get` instantiation in FnOrder")
	}
}

func TestGenericImplImplicitParams(t *testing.T) {
	// `impl Pair<T>` (parameters introduced by the self type) is accepted too.
	ok, codes := check(t, `struct Pair<T> { v: T }
impl Pair<T> { fn get(&self) -> T { self.v } }
fn main() -> i32 { let p = Pair { v: 1 }; println(p.get()); 0 }`)
	if !ok {
		t.Fatalf("expected clean, got %v", codes)
	}
}

func TestGenericFunctionInstantiation(t *testing.T) {
	src := `fn id<T>(x: T) -> T { return x; }
fn main() -> i32 {
    let a: i64 = id(1);
    let b = id::<i32>(2);
    let c = id(a);
    let d = id(b);
    println(a);
    println(b);
    println(c);
    println(d);
    0
}`
	file, pd := parser.Parse(0, []byte(src))
	if len(pd) > 0 {
		t.Fatalf("unexpected parse diagnostics: %v", codesOf(pd))
	}
	info, ds := sema.Check(file)
	if len(ds) > 0 {
		t.Fatalf("unexpected diagnostics: %v", codesOf(ds))
	}
	insts := 0
	for name, fn := range info.Fns {
		if len(name) > 3 && name[:3] == "id$" {
			insts++
			if fn.Generic {
				t.Fatalf("instantiation %s must not be a template", name)
			}
		}
	}
	// One for i64 (a and c share it) and one for i32 (b and d share it).
	if insts != 2 {
		t.Fatalf("expected 2 monomorphizations, got %d", insts)
	}
	if info.Fns["id"] == nil || !info.Fns["id"].Generic {
		t.Fatal("the generic template must still be registered")
	}
}

func TestGenericInferenceAndErrors(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "uninferable",
			src:  "fn make<T>() -> i64 { 0 }\nfn main() -> i32 { let x = make(); 0 }",
			want: "E0282",
		},
		{
			name: "too many type args",
			src:  "fn id<T>(x: T) -> T { return x; }\nfn main() -> i32 { println(id::<i32, i64>(1)); 0 }",
			want: "E0107",
		},
		{
			name: "type args on non-generic",
			src:  "fn f(x: i32) -> i32 { x }\nfn main() -> i32 { println(f::<i32>(1)); 0 }",
			want: "E0107",
		},
		{
			name: "argument type mismatch",
			src:  "fn pair<T>(a: T, b: T) -> T { return a; }\nfn main() -> i32 { println(pair(1, \"x\")); 0 }",
			want: "E0308",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, codes := check(t, tc.src)
			if ok {
				t.Fatalf("expected an error, got none")
			}
			if !has(codes, tc.want) {
				t.Fatalf("expected %s, got %v", tc.want, codes)
			}
		})
	}
}

func TestGenericStructInference(t *testing.T) {
	src := `struct Pair<A, B> { first: A, second: B }
impl<A, B> Pair<A, B> {
    fn swap(&self) -> Pair<B, A> { Pair { first: self.second, second: self.first } }
}
fn main() -> i32 {
    let p = Pair { first: 1, second: true };
    let q: Pair<bool, i32> = p.swap();
    let r = Pair::<str, i64> { first: "x", second: 2 };
    println(q.first);
    println(r.second);
    0
}`
	if ok, codes := check(t, src); !ok {
		t.Fatalf("expected clean, got %v", codes)
	}
}

func TestGenericStructNeedsAnnotation(t *testing.T) {
	_, codes := check(t, `struct Box<T> { v: T }
fn main() -> i32 { let b = Box { }; 0 }`)
	if !has(codes, "E0282") {
		t.Fatalf("expected E0282, got %v", codes)
	}
}

func TestGenericTypeArity(t *testing.T) {
	_, codes := check(t, `struct Pair<A, B> { first: A, second: B }
fn main() -> i32 { let p: Pair<i32> = Pair { first: 1, second: 2 }; 0 }`)
	if !has(codes, "E0107") {
		t.Fatalf("expected E0107, got %v", codes)
	}
}

func TestRecursiveGenericTypeRejected(t *testing.T) {
	_, codes := check(t, `struct N<T> { next: N<T> }
fn f(n: N<i32>) -> i32 { 0 }
fn main() -> i32 { 0 }`)
	if !has(codes, "E0072") {
		t.Fatalf("expected E0072, got %v", codes)
	}
}

func TestRecursiveGenericTypeBehindReferenceAllowed(t *testing.T) {
	ok, codes := check(t, `struct N<T> { v: T, next: &N<T> }
fn f(n: N<i32>) -> i32 { return n.v; }
fn main() -> i32 { 0 }`)
	if !ok {
		t.Fatalf("expected clean, got %v", codes)
	}
}

func TestOwnedPayloadMatchByBorrowRejected(t *testing.T) {
	_, codes := check(t, `enum E { S(String), A }
fn peek(e: &E) -> i32 { match e { E::S(s) => s.len() as i32, E::A => 0 } }
fn main() -> i32 { 0 }`)
	if !has(codes, "E0507") {
		t.Fatalf("expected E0507, got %v", codes)
	}
}

func TestGenericEnumStructVariant(t *testing.T) {
	src := `enum R<T, E> { Ok(T), Err { e: E, code: i64 } }
fn code(r: R<String, str>) -> i64 {
    match r { R::Ok(s) => s.len() as i64, R::Err { code, .. } => code }
}
fn main() -> i32 {
    let a: R<String, str> = R::Ok(String::from("abc"));
    let b: R<String, str> = R::Err { e: "bad", code: 7 };
    println(code(a) + code(b));
    0
}`
	if ok, codes := check(t, src); !ok {
		t.Fatalf("expected clean, got %v", codes)
	}
}

func TestGenericAssociatedFunction(t *testing.T) {
	src := `struct P<T> { v: T }
impl<T> P<T> {
    fn make(v: T) -> P<T> { P { v: v } }
    fn get(&self) -> T { self.v }
}
fn main() -> i32 {
    let p = P::<i64>::make(5);
    let q: P<str> = P::make("hi");
    println(p.get());
    println(q.get());
    0
}`
	if ok, codes := check(t, src); !ok {
		t.Fatalf("expected clean, got %v", codes)
	}
}

func TestQuestionOperatorDesugars(t *testing.T) {
	src := `enum E { No }
fn parse(s: str) -> Result<i64, E> {
    if s == "" { return Err(E::No); }
    return Ok(1);
}
fn twice(s: str) -> Result<i64, E> {
    let n = parse(s)?;
    return Ok(n + n);
}
fn maybe(x: i64) -> Option<i64> {
    if x > 0 { return Some(x); }
    return None;
}
fn call(x: i64) -> Option<i64> {
    let v = maybe(x)?;
    return Some(v + 1);
}
fn main() -> i32 {
    match twice("a") { Ok(v) => println(v), Err(_) => println(0) }
    match call(1) { Some(v) => println(v), None => println(0) }
    0
}`
	if ok, codes := check(t, src); !ok {
		t.Fatalf("expected clean, got %v", codes)
	}
}

func TestQuestionRequiresResultReturn(t *testing.T) {
	_, codes := check(t, `fn f() -> i64 { let x = g()?; x }
fn g() -> Result<i64, str> { Ok(1) }
fn main() -> i32 { 0 }`)
	if !has(codes, "E0277") {
		t.Fatalf("expected E0277, got %v", codes)
	}
}

func TestQuestionCanonicalErrorMismatch(t *testing.T) {
	_, codes := check(t, `enum A { X }
enum B { Y }
fn one() -> Result<i64, A> { Ok(1) }
fn two() -> Result<i64, B> {
    let n = one()?;
    Ok(n)
}
fn main() -> i32 { 0 }`)
	if !has(codes, "E0308") {
		t.Fatalf("expected E0308, got %v", codes)
	}
}

func TestBareVariantsNeedContext(t *testing.T) {
	_, codes := check(t, `fn main() -> i32 { let x = Some(1); 0 }`)
	if !has(codes, "E0425") {
		t.Fatalf("expected E0425 for an uninferable bare variant, got %v", codes)
	}
}
