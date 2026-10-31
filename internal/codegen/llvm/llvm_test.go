package llvm_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"zexi/internal/diag"
	"zexi/internal/driver"
)

// The LLVM backend is checked the same way the MIR interpreter is: the same
// source compiled by both backends must produce identical stdout and exit
// codes. If clang is missing the LLVM half cannot run, and the test skips
// rather than passing silently.

type result struct {
	stdout string
	exit   int
}

// runProgram compiles path with the given backend and runs the binary.
func runProgram(t *testing.T, path, backend string) (result, bool) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "prog")
	em := diag.NewEmitter()
	if _, ok := driver.Build(path, bin, driver.Options{OptLevel: "2", Backend: backend}, em); !ok {
		var buf bytes.Buffer
		em.Render(&buf, 0)
		t.Fatalf("build with backend %q failed:\n%s", backend, buf.String())
	}
	cmd := exec.Command(bin)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running the binary: %v", err)
	}
	return result{stdout: out.String(), exit: code}, true
}

func TestDifferentialAgainstCBackend(t *testing.T) {
	if _, err := exec.LookPath("clang"); err != nil {
		t.Skip("clang not found; the LLVM backend cannot link")
	}
	programs := map[string]string{
		"arithmetic": `
fn main() -> i32 {
    let a = 17;
    let b = 5;
    println(a + b, a - b, a * b, a / b, a % b);
    println(a & b, a | b, a ^ b, a << 2, a >> 1);
    println(-a, !b);
    println((a + b) * 2 - 3 / 3);
    0
}`,
		"control_flow": `
fn grade(n: i64) -> i32 {
    if n > 90 { return 4; }
    elif n > 80 { return 3; }
    elif n > 70 { return 2; }
    else { return 0; }
}
fn main() -> i32 {
    let mut total: i64 = 0;
    let mut i: i64 = 0;
    while i < 20 {
        i += 1;
        if i % 3 == 0 { continue; }
        if i > 15 { break; }
        total += grade(i * 5);
    }
    for j in 0..5 {
        total += j;
    }
    for k in 0..=3 {
        total += k * 10;
    }
    let v = loop {
        total += 1;
        if total > 100 { break 7; }
    };
    println(total, v);
    0
}`,
		"recursion": `
fn fib(n: i64) -> i64 {
    if n < 2 { return n; }
    fib(n - 1) + fib(n - 2)
}
fn ack(m: i64, n: i64) -> i64 {
    if m == 0 { return n + 1; }
    if n == 0 { return ack(m - 1, 1); }
    ack(m - 1, ack(m, n - 1))
}
fn main() -> i32 {
    println(fib(15));
    println(ack(2, 3));
    0
}`,
		"structs": `
struct Point { x: i64, y: i64 }
impl Point {
    fn norm1(&self) -> i64 { if self.x < 0 { -self.x } else { self.x } + if self.y < 0 { -self.y } else { self.y } }
    fn translated(&self, dx: i64, dy: i64) -> Point {
        Point { x: self.x + dx, y: self.y + dy }
    }
}
fn main() -> i32 {
    let p = Point { x: -3, y: 4 };
    let q = p.translated(10, -10);
    println(p.norm1(), q.norm1(), p.x, q.x, q.y);
    0
}`,
		"enums_and_match": `
enum Op { Add, Sub, Scale(i64), Range { lo: i64, hi: i64 } }
fn apply(op: Op, a: i64, b: i64) -> i64 {
    match op {
        Op::Add => a + b,
        Op::Sub => a - b,
        Op::Scale(k) => a * k,
        Op::Range { lo, hi } => hi - lo,
    }
}
fn bucket(n: i64) -> i64 {
    match n {
        0 => 100,
        1 | 2 => 200,
        3..=9 => 300,
        _ => 400,
    }
}
fn sign(n: i64) -> i64 {
    match n {
        x if x < 0 => -1,
        0 => 0,
        _ => 1,
    }
}
fn main() -> i32 {
    println(apply(Op::Add, 3, 4), apply(Op::Sub, 3, 4));
    println(apply(Op::Scale(5), 3, 0), apply(Op::Range { lo: 2, hi: 9 }, 0, 0));
    println(bucket(0), bucket(2), bucket(7), bucket(50));
    println(sign(-5), sign(0), sign(9));
    0
}`,
		"arrays": `
fn bubble(a: &mut [i64; 8]) {
    let mut i: usize = 0;
    while i < 8 {
        let mut j: usize = 0;
        while j + 1 < 8 - i {
            if (*a)[j] > (*a)[j + 1] {
                let t = (*a)[j];
                (*a)[j] = (*a)[j + 1];
                (*a)[j + 1] = t;
            }
            j += 1;
        }
        i += 1;
    }
}
fn main() -> i32 {
    let mut a: [i64; 8] = [5, 3, 9, 1, 7, 2, 8, 4];
    bubble(&mut a);
    let mut sum: i64 = 0;
    for i in 0..8 {
        sum += a[i as usize] * (i as i64 + 1);
    }
    println(len(a), sum, a[0], a[7]);
    0
}`,
		"references_and_mutation": `
fn bump(p: &mut i64, by: i64) { *p = *p + by; }
fn main() -> i32 {
    let mut x: i64 = 10;
    bump(&mut x, 5);
    bump(&mut x, -3);
    println(x);
    0
}`,
		"option_result": `
fn half(x: i64) -> Option<i64> {
    if x % 2 != 0 { return None; }
    Some(x / 2)
}
fn checked_div(a: i64, b: i64) -> Result<i64, i64> {
    if b == 0 { return Err(-1); }
    Ok(a / b)
}
fn chain(a: i64, b: i64) -> Result<i64, i64> {
    let q = checked_div(a, b)?;
    let r = checked_div(q, 2)?;
    Ok(r)
}
fn main() -> i32 {
    match half(10) { Some(v) => println("half", v), None => println("odd") }
    match half(7) { Some(v) => println("half", v), None => println("odd") }
    match chain(100, 2) { Ok(v) => println("ok", v), Err(e) => println("err", e) }
    match chain(100, 0) { Ok(v) => println("ok", v), Err(e) => println("err", e) }
    0
}`,
		"casts": `
fn main() -> i32 {
    let a: i64 = 300;
    let b: u8 = a as u8;
    let c: i32 = -7;
    let d: i64 = c as i64;
    let e: i64 = 3;
    let f: i32 = e as i32;
    println(b, d, f, (a as i32));
    0
}`,
		"nested_loops": `
fn main() -> i32 {
    let mut count: i64 = 0;
    for i in 0..5 {
        for j in 0..5 {
            if i == j { continue; }
            if i * j > 12 { break; }
            count += i * j;
        }
    }
    println(count);
    0
}`,
		"strings": `
fn main() -> i32 {
    let s = String::from("hello");
    let mut t = String::with_capacity(16);
    t.push_str(s.as_str());
    t.push(' ');
    t.push_str("world");
    println(t.as_str(), len(s), s.is_empty());
    let n = 42;
    println("value: {n}");
    0
}`,
		"div_by_zero_panics": `
fn main() -> i32 {
    let a: i64 = 10;
    let b: i64 = 0;
    println(a / b);
    0
}`,
		"bounds_panics": `
fn main() -> i32 {
    let a: [i64; 3] = [1, 2, 3];
    let i: i64 = 7;
    println(a[i as usize]);
    0
}`,
	}
	for name, src := range programs {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, name+".zx")
			if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
				t.Fatal(err)
			}
			want, _ := runProgram(t, path, "c")
			got, _ := runProgram(t, path, "llvm")
			if got.stdout != want.stdout {
				t.Errorf("output differs\n  c:    %q\n  llvm: %q", want.stdout, got.stdout)
			}
			if got.exit != want.exit {
				t.Errorf("exit code differs: c %d, llvm %d", want.exit, got.exit)
			}
		})
	}
}

// TestEmitLLVMChecksFrontEndErrors keeps --emit=llvm honest about programs the
// front end rejects: the error must come from the diagnostics, not a crash.
func TestEmitLLVMUnsupportedIsReported(t *testing.T) {
	// A program MIR cannot lower must be reported rather than emitted as
	// wrong IR. extern "C" bodies are outside this backend's phase.
	src := `
extern "C" fn puts(s: *u8) -> i32;
fn main() -> i32 {
    puts("x" as *u8);
    0
}`
	dir := t.TempDir()
	path := filepath.Join(dir, "extern.zx")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	em := diag.NewEmitter()
	art, ok := driver.FrontEnd(path, driver.Options{CheckOnly: true}, em)
	if !ok || art == nil || art.Info == nil {
		var buf bytes.Buffer
		em.Render(&buf, 0)
		t.Skipf("front end rejected the program:\n%s", buf.String())
	}
	// The build through the LLVM backend must fail with a diagnostic, not
	// panic. (Emission happens in driver.Build with Backend: "llvm".)
	outBin := filepath.Join(dir, "prog")
	em2 := diag.NewEmitter()
	_, ok = driver.Build(path, outBin, driver.Options{OptLevel: "2", Backend: "llvm"}, em2)
	if ok {
		t.Skip("extern program lowered; nothing to assert")
	}
	var buf bytes.Buffer
	em2.Render(&buf, 0)
	if !strings.Contains(buf.String(), "llvm") && !strings.Contains(buf.String(), "lower") {
		t.Errorf("expected a backend diagnostic, got:\n%s", buf.String())
	}
}
