package driver_test

// Error-fixture tests: every file in tests/errors/ is compiled as written and
// must fail with exactly the diagnostics its markers declare.
//
// A marker is a comment on any source line:
//
//	// error: E0308 mismatched types
//	// warning: E0001 unreachable pattern
//
// The code is required; the rest of the marker is a substring the diagnostic's
// message must contain (optional, but keep it: matching codes alone can hide
// the wrong message). Markers and diagnostics match one-to-one, in any order,
// so a fixture fails if a declared diagnostic is missing, if the compiler
// produces an undeclared one (of any severity), or if the file compiles.
//
// To grow diagnostic coverage, drop a new .zx file into tests/errors/ and
// declare what it should say — no new Go code needed.

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"zexi/internal/diag"
	"zexi/internal/driver"
)

const errorFixturesDir = "../../tests/errors"

// marker is one expected diagnostic declared in a fixture.
type marker struct {
	sev  diag.Severity
	code string
	sub  string // message substring; may be empty
	line int
}

// parseMarkers scans fixture source lines for `// error:` / `// warning:`
// (or `# error:` / `# warning:`) comments. Comment markers inside string
// literals are ignored.
func parseMarkers(src string) []marker {
	var out []marker
	for i, line := range strings.Split(src, "\n") {
		code := stripStrings(line)
		for _, sevName := range []struct {
			name string
			sev  diag.Severity
		}{{"error", diag.Error}, {"warning", diag.Warning}} {
			for _, prefix := range []string{"// " + sevName.name + ": ", "# " + sevName.name + ": "} {
				if idx := strings.Index(code, prefix); idx >= 0 {
					rest := strings.TrimSpace(code[idx+len(prefix):])
					fields := strings.SplitN(rest, " ", 2)
					m := marker{sev: sevName.sev, code: fields[0], line: i + 1}
					if len(fields) == 2 {
						m.sub = strings.TrimSpace(fields[1])
					}
					out = append(out, m)
				}
			}
		}
	}
	return out
}

// stripStrings blanks out the contents of double-quoted string literals so
// comment markers inside them are not picked up.
func stripStrings(line string) string {
	var b strings.Builder
	inStr := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inStr && c == '\\' && i+1 < len(line):
			i++
			b.WriteString("  ")
		case c == '"':
			inStr = !inStr
			b.WriteByte(' ')
		case inStr:
			b.WriteByte(' ')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// TestErrorFixtures compiles every tests/errors/*.zx and requires that its
// declared markers match the compiler's diagnostics one-to-one.
func TestErrorFixtures(t *testing.T) {
	entries, err := os.ReadDir(errorFixturesDir)
	if err != nil {
		t.Fatalf("read %s: %v", errorFixturesDir, err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".zx") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatalf("no .zx fixtures in %s", errorFixturesDir)
	}

	for _, name := range names {
		t.Run(strings.TrimSuffix(name, ".zx"), func(t *testing.T) {
			path := filepath.Join(errorFixturesDir, name)
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			marks := parseMarkers(string(src))
			if len(marks) == 0 {
				t.Fatalf("fixture declares no `// error:` markers; tests/errors/ holds failing programs only")
			}
			hasError := false
			for _, m := range marks {
				if m.sev == diag.Error {
					hasError = true
				}
			}
			if !hasError {
				t.Fatalf("fixture declares only warnings; tests/errors/ holds failing programs only")
			}

			em := diag.NewEmitter()
			_, ok := driver.FrontEndFiles([]string{path}, driver.Options{}, em)
			if ok {
				t.Fatalf("fixture compiled cleanly, but it declares %d diagnostic(s)", len(marks))
			}

			// Match markers to diagnostics one-to-one, in any order.
			ds := em.Diags()
			used := make([]bool, len(ds))
			for _, m := range marks {
				found := false
				for i, d := range ds {
					if used[i] || d.Severity != m.sev || d.Code != m.code {
						continue
					}
					if m.sub != "" && !strings.Contains(d.Message, m.sub) {
						continue
					}
					used[i] = true
					found = true
					break
				}
				if !found {
					t.Errorf("line %d: expected %s[%s] %q, which no diagnostic matched\n%s",
						m.line, m.sev, m.code, m.sub, renderAll(ds))
				}
			}
			for i, d := range ds {
				if !used[i] {
					t.Errorf("undeclared diagnostic %s[%s]: %s\n(expected it with a `// %s: %s ...` marker on its line)",
						d.Severity, d.Code, d.Message, strings.ToLower(d.Severity.String()), d.Code)
				}
			}
		})
	}
}

// renderAll formats diagnostics the way the CLI does, for failure messages.
func renderAll(ds []diag.Diagnostic) string {
	var b strings.Builder
	for _, d := range ds {
		head := d.Severity.String()
		if d.Code != "" {
			head += "[" + d.Code + "]"
		}
		b.WriteString("  " + head + ": " + d.Message + "\n")
	}
	return b.String()
}
