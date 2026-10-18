package bindgen_test

import (
	"strings"
	"testing"

	"zexi/internal/bindgen"
)

func TestGenerateBasicHeader(t *testing.T) {
	src := `#include <stddef.h>
/* Adds two numbers. */
int add(int a, int b);
double sqrt(double x);
const char *greet(const char *name);
char *dupstr(char *s);
unsigned long hash(const void *data, size_t len);
long random(void);
size_t strlen(const char *s);
int puts(const char *);
void logmsg(const char *fmt, ...);
int frobnicate(widget_t *w);
struct Point { int x; int y; };
typedef int myint;
`
	res := bindgen.Generate([]byte(src), "sample.h")
	wantDecls := []string{
		`extern "C" fn add(a: i32, b: i32) -> i32;`,
		`extern "C" fn sqrt(x: f64) -> f64;`,
		`extern "C" fn greet(name: str) -> str;`,
		`extern "C" fn dupstr(s: *mut u8) -> *mut u8;`,
		`extern "C" fn hash(data: *u8, len: usize) -> u64;`,
		`extern "C" fn random() -> i64;`,
		`extern "C" fn strlen(s: str) -> usize;`,
		`extern "C" fn puts(a0: str) -> i32;`,
	}
	for _, want := range wantDecls {
		if !strings.Contains(res.Source, want) {
			t.Errorf("generated source is missing %q\n---\n%s", want, res.Source)
		}
	}
	if len(res.Bound) != len(wantDecls) {
		t.Errorf("bound %d functions, want %d: %v", len(res.Bound), len(wantDecls), res.Bound)
	}
	// Variadic and unknown-type functions are skipped, with reasons.
	if len(res.Skipped) != 2 {
		t.Fatalf("skipped = %v, want 2 entries", res.Skipped)
	}
	joined := strings.Join(res.Skipped, "\n")
	if !strings.Contains(joined, "logmsg (variadic)") {
		t.Errorf("expected a variadic skip note, got %v", res.Skipped)
	}
	if !strings.Contains(joined, "frobnicate") {
		t.Errorf("expected a skip note for the unknown type, got %v", res.Skipped)
	}
}

func TestGenerateArrayAndPointerParams(t *testing.T) {
	src := "void fill(char buf[64], int n);\nint sum(const int *xs, size_t n);\n"
	res := bindgen.Generate([]byte(src), "arr.h")
	if !strings.Contains(res.Source, `extern "C" fn fill(buf: *mut u8, n: i32);`) {
		t.Errorf("array param not decayed to a pointer:\n%s", res.Source)
	}
	if !strings.Contains(res.Source, `extern "C" fn sum(xs: *i32, n: usize) -> i32;`) {
		t.Errorf("const pointer param mistranslated:\n%s", res.Source)
	}
}
