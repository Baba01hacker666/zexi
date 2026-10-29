package layout_test

// The corpus check: every struct and enum the compiler actually builds while
// checking tests/, examples/ and bench/ is laid out by the table, emitted as C,
// and handed to `cc`. A hand-written matrix only covers the cases its author
// thought of; this covers the cases the language is actually used for.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"zexi/internal/diag"
	"zexi/internal/driver"
	"zexi/internal/layout"
	"zexi/internal/types"
)

// corpusFiles lists the .zx sources under the given roots.
func corpusFiles(t *testing.T, roots ...string) []string {
	t.Helper()
	var out []string
	for _, root := range roots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if !info.IsDir() && strings.HasSuffix(path, ".zx") {
				out = append(out, path)
			}
			return nil
		})
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("walking %s: %v", root, err)
		}
	}
	sort.Strings(out)
	return out
}

// zeroSized reports whether t contains a zero-sized component. C has no
// zero-sized object, so a struct with a `()` field has no C declaration to
// compare against.
func zeroSized(ty *types.Type) bool {
	if ty == nil {
		return true
	}
	switch ty.Kind {
	case types.Unit, types.Never:
		return true
	case types.Array:
		return zeroSized(ty.Elem) || ty.Len == 0
	case types.Tuple:
		for _, e := range ty.Elems {
			if zeroSized(e) {
				return true
			}
		}
	case types.StructKind:
		if ty.St == nil {
			return true
		}
		for _, f := range ty.St.Fields {
			if zeroSized(f.Ty) {
				return true
			}
		}
	case types.EnumKind:
		if ty.En == nil {
			return true
		}
		for _, v := range ty.En.Variants {
			for _, f := range v.Fields {
				if zeroSized(f.Ty) {
					return true
				}
			}
		}
	}
	return false
}

// corpusTypes collects the structs and enums declared anywhere in the corpus,
// skipping the ones that cannot be represented in C.
func corpusTypes(t *testing.T) []*types.Type {
	t.Helper()
	files := corpusFiles(t, "../../tests", "../../examples", "../../bench")
	if len(files) == 0 {
		t.Skip("no corpus sources found")
	}
	seen := map[string]bool{}
	var out []*types.Type
	for _, path := range files {
		em := diag.NewEmitter()
		art, ok := driver.FrontEnd(path, driver.Options{CheckOnly: true}, em)
		if !ok || art == nil || art.Info == nil {
			continue // a file that does not check is not our problem here
		}
		info := art.Info
		for _, s := range info.StructOrd {
			if s == nil || s.Generic {
				continue
			}
			ty := &types.Type{Kind: types.StructKind, St: s}
			if zeroSized(ty) {
				continue
			}
			key := "struct " + s.Name
			if _, err := layout.Default().Of(ty); err != nil {
				t.Errorf("corpus %s: layout error: %v", key, err)
				continue
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, ty)
		}
		for _, e := range info.EnumOrd {
			if e == nil || e.Generic {
				continue
			}
			ty := &types.Type{Kind: types.EnumKind, En: e}
			if zeroSized(ty) {
				continue
			}
			key := "enum " + e.Name
			if _, err := layout.Default().Of(ty); err != nil {
				t.Errorf("corpus %s: layout error: %v", key, err)
				continue
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, ty)
		}
	}
	return out
}

func TestCorpusLayoutMatchesTheCCompiler(t *testing.T) {
	t.Parallel()
	cc := os.Getenv("CC")
	if cc == "" {
		cc = "cc"
	}
	if _, err := exec.LookPath(cc); err != nil {
		t.Skipf("no C compiler on PATH (%s)", err)
	}

	typesUnderTest := corpusTypes(t)
	if len(typesUnderTest) == 0 {
		t.Skip("the corpus declares no representable structs or enums")
	}

	g := newCgen()
	names := make([]string, len(typesUnderTest))
	for i, ty := range typesUnderTest {
		n := g.fresh("p")
		g.defs = append(g.defs, fmt.Sprintf("typedef %s %s;", g.name(ty), n))
		names[i] = n
	}

	var sb strings.Builder
	sb.WriteString("#include <stdio.h>\n#include <stdint.h>\n#include <stddef.h>\n#include <stdbool.h>\n")
	sb.WriteString("typedef struct { char *ptr; size_t len; size_t cap; } zx_String;\n")
	g.defs = append(g.defs, g.late...)
	for _, d := range g.defs {
		sb.WriteString(d + "\n")
	}
	sb.WriteString("int main(void) {\n")
	for i, ty := range typesUnderTest {
		key := names[i]
		fmt.Fprintf(&sb, "  printf(\"%%s %%zu\\n\", %q, sizeof(%s));\n", key+" size", key)
		fmt.Fprintf(&sb, "  printf(\"%%s %%zu\\n\", %q, _Alignof(%s));\n", key+" align", key)
		emitOffsetProbes(&sb, key, ty, g)
	}
	sb.WriteString("  return 0;\n}\n")

	dir := t.TempDir()
	csrc := filepath.Join(dir, "corpus.c")
	if err := os.WriteFile(csrc, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "corpus")
	if out, err := exec.Command(cc, "-std=gnu11", "-O0", "-o", bin, csrc).CombinedOutput(); err != nil {
		t.Fatalf("cc failed: %v\n%s", err, out)
	}
	raw, err := exec.Command(bin).Output()
	if err != nil {
		t.Fatalf("running the corpus probe failed: %v", err)
	}

	got := map[string]int64{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		// "<probe> <what> <value>", where <what> is "size", "align" or
		// "off.<field>".
		f := strings.Fields(line)
		if len(f) != 3 {
			t.Fatalf("unexpected probe output %q", line)
		}
		v, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil {
			t.Fatalf("unexpected probe value %q: %v", line, err)
		}
		got[f[0]+" "+f[1]] = v
	}

	tgt := layout.Default()
	checked := 0
	for i, ty := range typesUnderTest {
		key := names[i]
		want, err := tgt.Of(ty)
		if err != nil {
			t.Errorf("%s (%v): %v", key, ty, err)
			continue
		}
		name := typeLabel(ty)
		if c, ok := got[key+" size"]; !ok {
			t.Errorf("%s: cc reported no size", name)
		} else if c != want.Size {
			t.Errorf("%s: cc says size %d, table says %d", name, c, want.Size)
		}
		if c, ok := got[key+" align"]; !ok {
			t.Errorf("%s: cc reported no align", name)
		} else if c != want.Align {
			t.Errorf("%s: cc says align %d, table says %d", name, c, want.Align)
		}
		switch ty.Kind {
		case types.StructKind:
			sl, err := tgt.Struct(ty.St)
			if err != nil {
				t.Errorf("%s: %v", name, err)
				continue
			}
			for _, f := range sl.Fields {
				if c, ok := got[key+" off."+f.Name]; !ok {
					t.Errorf("%s field %s: cc reported no offset", name, f.Name)
				} else if c != f.Offset {
					t.Errorf("%s field %s: cc says offset %d, table says %d", name, f.Name, c, f.Offset)
				}
			}
		case types.EnumKind:
			el, err := tgt.Enum(ty.En)
			if err != nil {
				t.Errorf("%s: %v", name, err)
				continue
			}
			for _, v := range el.Variants {
				for _, f := range v.Fields {
					k := key + " off.v" + strconv.Itoa(v.Index) + "." + f.Name
					if c, ok := got[k]; !ok {
						t.Errorf("%s::%s field %s: cc reported no offset", name, v.Name, f.Name)
					} else if c != v.Offset+f.Offset {
						t.Errorf("%s::%s field %s: cc says offset %d, table says %d",
							name, v.Name, f.Name, c, v.Offset+f.Offset)
					}
				}
			}
		}
		checked++
		}
		if checked < 49 {
		t.Errorf("verified %d corpus types, want at least 49", checked)
		}
		t.Logf("verified %d corpus types against %s", checked, cc)
}

// typeLabel names a type for a test failure message.
func typeLabel(ty *types.Type) string {
	switch ty.Kind {
	case types.StructKind:
		return "struct " + ty.St.Name
	case types.EnumKind:
		return "enum " + ty.En.Name
	}
	return fmt.Sprint(ty)
}
