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
// supported subset type-checks cleanly.
func TestTypeCheckExamples(t *testing.T) {
	files := []string{
		"../../examples/hello.zx",
		"../../tests/tour.zx",
		"../../tests/structs.zx",
		"../../tests/enums.zx",
		"../../tests/arrays.zx",
	}
	for _, f := range files {
		em := diag.NewEmitter()
		if _, ok := driver.FrontEnd(f, driver.Options{}, em); !ok {
			t.Errorf("%s: front end reported errors", f)
		}
	}
}

// TestRunPrograms compiles and runs each program, asserting its output. It is
// skipped when no C compiler is available or in short mode.
func TestRunPrograms(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end build in short mode")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no C compiler available")
	}

	cases := []struct {
		file string
		want string
	}{
		{
			file: "../../tests/tour.zx",
			want: strings.Join([]string{
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
			}, "\n"),
		},
		{
			file: "../../tests/structs.zx",
			want: strings.Join([]string{
				"25", "4", "6", "2.5", "3.5", "15", "0", "30", "",
			}, "\n"),
		},
		{
			file: "../../tests/enums.zx",
			want: strings.Join([]string{
				"12.5664", "12", "0", "zero", "one", "many", "0", "7", "-1", "10", "",
			}, "\n"),
		},
		{
			file: "../../tests/arrays.zx",
			want: strings.Join([]string{
				"5", "15", "4", "0", "1", "100", "5", "32", "9", "",
			}, "\n"),
		},
	}

	for _, tc := range cases {
		t.Run(filepath.Base(tc.file), func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "prog")
			em := diag.NewEmitter()
			if _, ok := driver.Build(tc.file, out, driver.Options{}, em); !ok {
				t.Fatal("build failed")
			}
			raw, err := exec.Command(out).Output()
			if err != nil {
				t.Fatalf("running %s: %v", out, err)
			}
			if got := string(raw); got != tc.want {
				t.Fatalf("output mismatch\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}
