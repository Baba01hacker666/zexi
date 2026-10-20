package driver_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"zexi/internal/diag"
	"zexi/internal/driver"
)

// buildAndRun compiles src to a binary in a temp dir, runs it, and returns its
// combined stdout/stderr plus the process state.
func buildAndRun(t *testing.T, name, src string, opts driver.Options, args ...string) (string, *exec.ExitError) {
	t.Helper()
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no C compiler available")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, name+".zx")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, name)
	em := diag.NewEmitter()
	if _, ok := driver.Build(path, out, opts, em); !ok {
		var sb strings.Builder
		em.Render(&sb, em.FirstFileID())
		t.Fatalf("failed to build %s:\n%s", name, sb.String())
	}
	cmd := exec.Command(out, args...)
	raw, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if err != nil {
		ee, _ = err.(*exec.ExitError)
		if ee == nil {
			t.Fatalf("running %s: %v", name, err)
		}
	}
	return string(raw), ee
}

func wantOutput(t *testing.T, name, src, want string) {
	t.Helper()
	got, ee := buildAndRun(t, name, src, driver.Options{})
	if ee != nil {
		t.Fatalf("%s exited with %v\noutput: %q", name, ee, got)
	}
	if got != want {
		t.Fatalf("%s output mismatch\n got: %q\nwant: %q", name, got, want)
	}
}

// TestNestedLoopVariablesKeepTheirScope is a regression test for a
// miscompilation: `for` lowering bound a placeholder name before the real loop
// variable, which recorded that placeholder in the loop's scope. A nested loop
// whose variable had a different name therefore shadowed the outer variable,
// and every reference to it inside the inner loop named a C identifier that was
// never declared.
func TestNestedLoopVariablesKeepTheirScope(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end build in short mode")
	}
	src := `fn main() -> i32 {
    let mut total = 0;
    for i in 0..3 {
        for j in 0..3 {
            if i == 2 { break; }
            total += i * 10 + j;
        }
    }
    println(total);

    let xs: [i32; 3] = [10, 20, 30];
    let mut prod = 0;
    for x in xs {
        for y in xs {
            prod += x * y;
        }
    }
    println(prod);

    // Three levels deep, with the outermost variable read in the innermost.
    let mut deep = 0;
    for a in 0..2 {
        for b in 0..2 {
            for c in 0..2 {
                deep += a * 100 + b * 10 + c;
            }
        }
    }
    println(deep);
    0
}
`
	// i=0: 0+1+2=3; i=1: 10+11+12=33; i=2 breaks immediately. 3+33=36.
	// (10+20+30)^2 = 3600.
	// a,b,c each in 0..2: (0+1)+(10+11)+(100+101)+(110+111) = 444.
	wantOutput(t, "nested_loops", src, "36\n3600\n444\n")
}

// TestMatchTemporariesDoNotShadowLocals is a regression test for a silent
// wrong-answer miscompilation: the scrutinee and result temporaries of a
// `match` were allocated from the user-name pool, so a user local named `m` or
// `r` was rebound to the compiler temporary inside the arms.
func TestMatchTemporariesDoNotShadowLocals(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end build in short mode")
	}
	src := `enum E { A(i64), B }

fn main() -> i32 {
    // ` + "`r`" + ` is the name the match-result temporary used to take.
    let r: i64 = 99;
    let y = match r { 99 => r + 1, _ => 0 };
    println(y, r);

    // ` + "`m`" + ` is the name the scrutinee temporary used to take.
    let m: i64 = 5;
    let e = E::A(1);
    match e {
        E::A(v) => println(m, v),
        E::B => println(m, -1),
    }
    println(m);
    0
}
`
	wantOutput(t, "match_shadow", src, "100 99\n5 1\n5\n")
}

// TestRuntimePanics checks that a run-time fault reports itself, keeps the
// output already written to stdout, and exits with a status a caller can test
// for. Before the panic runtime, every one of these killed the process with a
// signal or a bare abort(), and the C stdout buffer went with it.
func TestRuntimePanics(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end build in short mode")
	}
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "divide_by_zero",
			src: `fn main() -> i32 {
    let d = arg_count() - 1;
    println("before");
    println(5 / d);
    0
}
`,
			want: "before\nzexi panic: attempt to divide by zero\n",
		},
		{
			name: "remainder_by_zero",
			src: `fn main() -> i32 {
    let d = arg_count() - 1;
    println("before");
    println(5 % d);
    0
}
`,
			want: "before\nzexi panic: attempt to calculate the remainder of a division by zero\n",
		},
		{
			name: "index_out_of_bounds",
			src: `fn main() -> i32 {
    let xs: [i32; 3] = [1, 2, 3];
    let i = arg_count() as i64 + 2;
    println("before");
    println(xs[i]);
    0
}
`,
			want: "before\nzexi panic: index 3 out of bounds for length 3\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ee := buildAndRun(t, tc.name, tc.src, driver.Options{})
			if ee == nil {
				t.Fatalf("%s should have panicked, got output %q", tc.name, got)
			}
			if code := ee.ExitCode(); code != 101 {
				t.Fatalf("%s exit code = %d, want 101", tc.name, code)
			}
			if got != tc.want {
				t.Fatalf("%s output mismatch\n got: %q\nwant: %q", tc.name, got, tc.want)
			}
		})
	}
}

// TestOverflowChecksFollowsOptLevel checks the documented contract: debug
// builds panic on integer overflow, release builds wrap.
func TestOverflowChecksFollowsOptLevel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end build in short mode")
	}
	src := `fn main() -> i32 {
    let big: i64 = 9223372036854775807;
    println(big + 1);
    0
}
`
	// Release wraps.
	wantOutput(t, "overflow_release", src, "-9223372036854775808\n")

	// Debug panics, and keeps the exit status a caller can test for.
	got, ee := buildAndRun(t, "overflow_debug", src, driver.Options{OptLevel: "0"})
	if ee == nil {
		t.Fatalf("debug build should have panicked on overflow, got %q", got)
	}
	if code := ee.ExitCode(); code != 101 {
		t.Fatalf("debug overflow exit code = %d, want 101", code)
	}
	if want := "zexi panic: attempt to add with overflow\n"; got != want {
		t.Fatalf("debug overflow output mismatch\n got: %q\nwant: %q", got, want)
	}
}

// TestCheckedArithmeticStaysCorrect checks that inserting the checks did not
// change the value of any arithmetic, including the cases the checks rewrite
// (a divisor of -1, and compound assignment through a subscript).
func TestCheckedArithmeticStaysCorrect(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end build in short mode")
	}
	src := `fn main() -> i32 {
    println(7 / 2, -7 / 2, 7 / -2, -7 / -2);
    println(7 % 3, -7 % 3, 7 % -3, -7 % -3);
    let mut xs: [i64; 3] = [1, 2, 3];
    let mut i: i64 = 0;
    while i < 3 {
        xs[i] += 10;
        xs[i] *= 2;
        xs[i] -= 1;
        i += 1;
    }
    println(xs[0], xs[1], xs[2]);
    let m: i8 = -128;
    println(m / 2, m % 7);
    0
}
`
	want := "3 -3 -3 3\n" +
		"1 -1 1 -1\n" +
		"21 23 25\n" +
		"-64 -2\n"
	// Both profiles must agree: the checks report faults, they never change a
	// value that does not overflow.
	for _, level := range []string{"", "0"} {
		label := "release"
		if level == "0" {
			label = "debug"
		}
		got, ee := buildAndRun(t, "arith_"+label, src, driver.Options{OptLevel: level})
		if ee != nil {
			t.Fatalf("%s exited with %v\noutput: %q", label, ee, got)
		}
		if got != want {
			t.Fatalf("%s output mismatch\n got: %q\nwant: %q", label, got, want)
		}
	}
}

// TestStringBuilderGrowth covers the String append path now that the buffer
// carries a capacity and grows geometrically instead of reallocating to the
// exact size on every byte. The interesting cases are the ones where growth
// could go wrong: appending a slice of the string's own buffer (the source may
// be invalidated by the realloc), refilling after clear, and starting from a
// preallocated capacity.
func TestStringBuilderGrowth(t *testing.T) {
	wantOutput(t, "strbuild", `fn main() -> i32 {
    let mut b = String::new();
    let mut i = 0;
    while i < 300 { b.push_byte(97); i = i + 1; }
    println(b.len());

    let mut c = String::new();
    c.push_str("abc");
    c.push_str(c.as_str());
    println(c);

    let mut d = String::with_capacity(64);
    let mut j = 0;
    while j < 10 { d.push_str("xy"); j = j + 1; }
    println(d, d.len());
    d.clear();
    println(d.len());
    d.push_str("refilled");
    println(d);

    let mut e = String::new();
    e.push('é');
    e.push_byte(33);
    println(e, e.len());
    0
}
`, "300\nabcabc\nxyxyxyxyxyxyxyxyxyxy 20\n0\nrefilled\né! 3\n")
}
