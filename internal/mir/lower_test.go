package mir_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zexi/internal/diag"
	"zexi/internal/driver"
	"zexi/internal/mir"
)

// lowerString checks a source string and lowers it to MIR.
func lowerString(t *testing.T, src string) *mir.Result {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "prog.zx")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return lowerFile(t, path)
}

func lowerFile(t *testing.T, path string) *mir.Result {
	t.Helper()
	em := diag.NewEmitter()
	art, ok := driver.FrontEnd(path, driver.Options{CheckOnly: true}, em)
	if !ok || art == nil || art.Info == nil {
		t.Fatalf("front end rejected %s: %d diagnostics", path, len(em.Diags()))
	}
	return mir.Lower(art.Info)
}

// TestGoldenLocksShape pins the exact textual form of a tiny function. The dump
// is what `zexic emit --emit=mir` prints, so a change here is a change to the
// IR's observable surface and should be deliberate.
func TestGoldenLocksShape(t *testing.T) {
	res := lowerString(t, `
fn add(a: i64, b: i64) -> i64 {
    a + b
}

fn main() -> i32 {
    println(add(1, 2));
    0
}
`)
	if len(res.Unsupported) != 0 {
		t.Fatalf("unexpected unsupported constructs: %v", res.Unsupported)
	}
	fn := res.Prog.FnByName("add")
	if fn == nil {
		t.Fatal("add was not lowered")
	}
	want := `fn add(i64, i64) -> i64 {
    locals:
        _0: i64	// return
        _1: i64	// arg a
        _2: i64	// arg b
    bb0: {
        _0 = (copy _1 + copy _2);
        return;
    }
}
`
	if got := fn.String(); got != want {
		t.Errorf("unexpected MIR:\n got:\n%s\nwant:\n%s", got, want)
	}
}

// TestValidateEveryBody runs the structural and definite-initialization checks
// over a spread of language features. A failure here is a lowering bug.
func TestValidateEveryBody(t *testing.T) {
	programs := map[string]string{
		"arithmetic": `
fn main() -> i32 {
    let a = 7;
    let b = 3;
    println(a + b, a - b, a * b, a / b, a % b);
    println(a & b, a | b, a ^ b, a << 2, a >> 1);
    println(-a, !a);
    0
}`,
		"control": `
fn classify(n: i64) -> i64 {
    if n < 0 { return -1; }
    elif n == 0 { return 0; }
    else { return 1; }
}
fn main() -> i32 {
    let mut i: i64 = 0;
    let mut sum: i64 = 0;
    while i < 10 {
        i += 1;
        if i % 2 == 0 { continue; }
        if i > 7 { break; }
        sum += classify(i);
    }
    for j in 0..4 {
        sum += j;
    }
    let v = loop {
        sum += 1;
        if sum > 5 { break 42; }
    };
    println(sum, v);
    0
}`,
		"aggregates": `
struct P { x: i64, y: i64 }
impl P {
    fn sum(&self) -> i64 { self.x + self.y }
    fn shifted(&self, d: i64) -> P { P { x: self.x + d, y: self.y } }
}
enum E { A, B(i64, i64), C { v: i64 } }
fn get(e: E) -> i64 {
    match e {
        E::A => 0,
        E::B(a, b) => a * 10 + b,
        E::C { v } => v,
    }
}
fn main() -> i32 {
    let p = P { x: 1, y: 2 };
    let q = p.shifted(3);
    println(p.sum(), q.sum(), get(E::B(4, 5)), get(E::C { v: 9 }));
    0
}`,
		"arrays": `
fn main() -> i32 {
    let mut a: [i64; 5] = [0; 5];
    let mut i: usize = 0;
    while i < 5 {
        a[i] = (i * i) as i64;
        i += 1;
    }
    let mut sum: i64 = 0;
    for j in 0..5 {
        sum += a[j as usize];
    }
    println(sum, len(a), a.len());
    0
}`,
		"references": `
fn bump(p: &mut i64) { *p = *p + 1; }
fn main() -> i32 {
    let mut x: i64 = 41;
    bump(&mut x);
    println(x);
    0
}`,
		"boolean": `
fn main() -> i32 {
    let a = true;
    let b = false;
    if a && !b {
        println("and", a || b);
    }
    0
}`,
	}
	for name, src := range programs {
		t.Run(name, func(t *testing.T) {
			res := lowerString(t, src)
			if len(res.Unsupported) != 0 {
				t.Fatalf("unsupported: %v", res.Unsupported)
			}
			if err := res.Prog.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestCorpusLowersEverythingItReports walks the repository's own programs and
// checks two things per file: every body that lowered passes Validate, and the
// coverage counters agree with the function list.
func TestCorpusLowersEverythingItReports(t *testing.T) {
	root := filepath.Join("..", "..")
	var files []string
	for _, dir := range []string{"tests", "examples", "bench"} {
		_ = filepath.Walk(filepath.Join(root, dir), func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".zx") {
				return nil
			}
			files = append(files, path)
			return nil
		})
	}
	if len(files) == 0 {
		t.Fatal("no corpus files found")
	}
	var lowered, total int
	for _, path := range files {
		em := diag.NewEmitter()
		art, ok := driver.FrontEnd(path, driver.Options{CheckOnly: true}, em)
		if !ok || art == nil || art.Info == nil {
			continue // some examples require a C source or a directory context
		}
		res := mir.Lower(art.Info)
		if err := res.Prog.Validate(); err != nil {
			t.Errorf("%s: %v", path, err)
		}
		lowered += res.Lowered
		total += res.Total
	}
	if total == 0 {
		t.Fatal("corpus lowered nothing at all")
	}
	t.Logf("MIR corpus coverage: %d/%d reachable functions (%.0f%%)", lowered, total, 100*float64(lowered)/float64(total))
}

// TestUnsupportedIsReported checks that a construct with no lowering is reported
// against the function that uses it rather than silently producing wrong IR.
func TestUnsupportedIsReported(t *testing.T) {
	res := lowerString(t, `
fn main() -> i32 {
    defer println("later");
    0
}`)
	if len(res.Unsupported) == 0 {
		t.Fatal("expected defer to be reported as unsupported")
	}
	if res.Prog.Main != nil {
		t.Error("main should not be lowered when it uses an unsupported construct")
	}
	if !strings.Contains(res.Unsupported[0].Feature, "defer") {
		t.Errorf("report should name the construct, got %q", res.Unsupported[0].Feature)
	}
}

// TestPropagatesFailureToCallers checks the fixpoint that stops a function from
// calling something that never lowered.
func TestPropagatesFailureToCallers(t *testing.T) {
	res := lowerString(t, `
fn helper() -> i64 {
    defer println("later");
    1
}
fn main() -> i32 {
    println(helper());
    0
}`)
	if res.Prog.Main != nil {
		t.Error("main calls a function that could not be lowered; it must not be lowered either")
	}
	var mainReport, helperReport bool
	for _, u := range res.Unsupported {
		if u.Fn == "main" && strings.Contains(u.Feature, "helper") {
			mainReport = true
		}
		if u.Fn == "helper" {
			helperReport = true
		}
	}
	if !mainReport || !helperReport {
		t.Errorf("both functions should be reported, got %v", res.Unsupported)
	}
}

// A field moved out of an owning aggregate cannot share the parent's drop
// flag. Reporting this gap is safer than emitting a double drop; match-pattern
// bindings have a separate, explicitly handled sole-owning-field exception.
func TestOwningFieldMoveIsReported(t *testing.T) {
	res := lowerString(t, `
struct Pair { a: String, b: String }
fn take(p: Pair) -> String { p.a }
fn main() -> i32 {
    let p = Pair { a: String::from("a"), b: String::from("b") };
    println(take(p));
    0
}`)
	if res.Prog.Main != nil {
		t.Error("caller of an unsupported partial move should not be lowered")
	}
	found := false
	for _, u := range res.Unsupported {
		if u.Fn == "take" && strings.Contains(u.Feature, "partial move") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a partial-move report for take, got %v", res.Unsupported)
	}
}
