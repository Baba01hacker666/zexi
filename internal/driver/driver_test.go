package driver_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"zexi/internal/diag"
	"zexi/internal/driver"
)

// TestTypeCheckExamples ensures every shipped example that targets the
// M0-alpha subset type-checks cleanly.
func TestTypeCheckExamples(t *testing.T) {
	files := []string{
		"../../examples/hello.zx",
		"../../tests/tour.zx",
	}
	for _, f := range files {
		em := diag.NewEmitter()
		if _, ok := driver.FrontEnd(f, driver.Options{}, em); !ok {
			t.Errorf("%s: front end reported errors", f)
		}
	}
}

// TestTourEndToEnd compiles and runs tests/tour.zx, asserting its output. It is
// skipped when no C compiler is available or in short mode.
func TestTourEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end build in short mode")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no C compiler available")
	}

	out := filepath.Join(t.TempDir(), "tour")
	em := diag.NewEmitter()
	if _, ok := driver.Build("../../tests/tour.zx", out, driver.Options{}, em); !ok {
		t.Fatal("build failed")
	}

	cmd := exec.Command(out)
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("running %s: %v", out, err)
	}
	got := string(raw)
	want := strings.Join([]string{
		"hello from zexi",
		"0 1 1 2 3 5 8 13 21 34 ",
		"55",
		"zero",
		"9000000000",
		"7",
		"true",
		"Z",
		"410065408",
		"",
	}, "\n")
	if got != want {
		t.Fatalf("output mismatch\n got: %q\nwant: %q", got, want)
	}
}
