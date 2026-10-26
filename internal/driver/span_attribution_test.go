package driver_test

// Multi-file span attribution: a diagnostic raised inside an imported file
// must render against that file, not the entry. Every loaded file occupies a
// disjoint range of the shared span space (see diag.File.Base), so one span
// identifies both its file and its position in it.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zexi/internal/diag"
	"zexi/internal/driver"
)

func TestDiagnosticSpansPointAtTheirFile(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.zx")
	libPath := filepath.Join(dir, "lib.zx")
	src := "use \"lib.zx\";\nfn main() -> i32 {\n    let x: i64 = helper();\n    println(x);\n    0\n}\n"
	if err := os.WriteFile(mainPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	// The error lives in lib.zx: an i64 binding initialized with a str.
	lib := "pub fn helper() -> i64 {\n    let bad: i64 = \"oops\";\n    return bad;\n}\n"
	if err := os.WriteFile(libPath, []byte(lib), 0o644); err != nil {
		t.Fatal(err)
	}

	em := diag.NewEmitter()
	if _, ok := driver.FrontEnd(mainPath, driver.Options{}, em); ok {
		t.Fatal("expected a type error from lib.zx")
	}

	// The renderer resolves each span to its file; the entry file is passed
	// only as a legacy fallback for unlocated diagnostics.
	ds := em.Diags()
	if len(ds) == 0 {
		t.Fatal("no diagnostics reported")
	}
	var buf bytes.Buffer
	em.Render(&buf, em.FirstFileID())
	out := buf.String()

	if !strings.Contains(out, "lib.zx:2:20") {
		t.Fatalf("diagnostic should point into lib.zx line 2, got:\n%s", out)
	}
	if strings.Contains(out, "main.zx:3") {
		t.Fatalf("diagnostic leaked into main.zx, got:\n%s", out)
	}

	// JSON output resolves the same way.
	buf.Reset()
	em.RenderJSON(&buf, em.FirstFileID())
	if !strings.Contains(buf.String(), `"file":"`+libPath) {
		t.Fatalf("JSON diagnostic should name lib.zx, got:\n%s", buf.String())
	}
}
