package layout_test

// This file checks the layout table against the one authority that cannot be
// argued with: the C compiler the back end actually uses. For every type in a
// matrix we emit the same C declaration the back end would emit, ask `cc` for
// sizeof, _Alignof and offsetof, run the program, and require the table to
// agree field for field.
//
// A layout table that is merely self-consistent is worthless — the whole point
// is agreeing with what the other back end produces, and the only way to know
// that is to ask.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"zexi/internal/layout"
	"zexi/internal/types"
)

// cgen accumulates C declarations and hands out a name for each type.
//
// Types are keyed structurally (by their label) rather than by pointer: a
// recursive type such as `enum Tree { Leaf(i64), Node(Vec<Tree>) }` reaches the
// same declaration through freshly-built `*types.Type` values, and keying by
// pointer would recurse forever. Struct and enum names are registered *before*
// their fields are emitted and forward-declared as `typedef struct n n;`, which
// is what makes a cycle through `Vec`/`HashMap` terminate — a by-value cycle is
// still impossible, exactly as the C back end requires.
type cgen struct {
	defs []string
	// late holds declarations that must follow every named type: a hash map
	// slot embeds its key and value by value, so its body can only be emitted
	// once those types are complete (the same rule the C back end follows).
	late []string
	seen map[string]string
	n    int
}

func newCgen() *cgen { return &cgen{seen: map[string]string{}} }

// cField mirrors the C back end's fieldName: a field the language allows but
// C does not (a tuple element named "0") is prefixed, and anything else that
// is not a C identifier is replaced. The layout must be identical either way,
// so sanitising the name cannot mask a bug.
func cField(name string) string {
	if name == "" {
		return "zx_anon"
	}
	if name[0] >= '0' && name[0] <= '9' {
		return "f" + name
	}
	var b strings.Builder
	for i, r := range name {
		ok := r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok || (i == 0 && r >= '0' && r <= '9') {
			b.WriteByte('_')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (c *cgen) fresh(prefix string) string {
	c.n++
	return fmt.Sprintf("%s%d", prefix, c.n)
}

// name returns a C type name usable in a typedef for ty, emitting whatever
// supporting declarations it needs first.
func (c *cgen) name(ty *types.Type) string {
	key := typeLabel(ty)
	if got, ok := c.seen[key]; ok {
		return got
	}
	var out string
	switch ty.Kind {
	case types.Bool:
		out = "bool"
	case types.Char:
		out = "uint32_t"
	case types.I8:
		out = "int8_t"
	case types.I16:
		out = "int16_t"
	case types.I32:
		out = "int32_t"
	case types.I64:
		out = "int64_t"
	case types.Isize:
		out = "intptr_t"
	case types.U8:
		out = "uint8_t"
	case types.U16:
		out = "uint16_t"
	case types.U32:
		out = "uint32_t"
	case types.U64:
		out = "uint64_t"
	case types.Usize:
		out = "size_t"
	case types.F32:
		out = "float"
	case types.F64:
		out = "double"
	case types.Str:
		out = "const char *"
	case types.StringKind:
		out = "zx_String"
	case types.Unit:
		out = "zx_unit"
	case types.Ptr, types.Ref:
		elem := c.name(ty.Elem)
		id := c.fresh("t_ptr")
		c.defs = append(c.defs, fmt.Sprintf("typedef %s *%s;", elem, id))
		out = id
	case types.Fn:
		id := c.fresh("t_fn")
		c.defs = append(c.defs, fmt.Sprintf("typedef void *%s;", id))
		out = id
	case types.Slice:
		id := c.fresh("t_slice")
		c.defs = append(c.defs, fmt.Sprintf("typedef struct { const void *ptr; size_t len; } %s;", id))
		out = id
	case types.Array:
		elem := c.name(ty.Elem)
		id := c.fresh("t_arr")
		c.defs = append(c.defs, fmt.Sprintf("typedef %s %s[%d];", elem, id, ty.Len))
		out = id
	case types.VecKind:
		elem := c.name(ty.Elem)
		id := c.fresh("t_vec")
		c.defs = append(c.defs, fmt.Sprintf("typedef struct { %s *ptr; size_t len; size_t cap; } %s;", elem, id))
		out = id
	case types.MapKind:
		key, val := c.name(ty.Key), c.name(ty.Val)
		id := c.fresh("t_map")
		pair := id + "_pair"
		c.defs = append(c.defs, fmt.Sprintf("typedef struct %s %s;", pair, pair))
		c.defs = append(c.defs, fmt.Sprintf("typedef struct { %s *entries; size_t len; size_t cap; } %s;", pair, id))
		c.late = append(c.late, fmt.Sprintf("struct %s { %s key; %s value; int used; };", pair, key, val))
		out = id
	case types.Tuple:
		fields := make([]string, len(ty.Elems))
		for i, e := range ty.Elems {
			fields[i] = fmt.Sprintf("%s f%d;", c.name(e), i)
		}
		id := c.fresh("t_tuple")
		c.defs = append(c.defs, fmt.Sprintf("typedef struct { %s } %s;", strings.Join(fields, " "), id))
		out = id
	case types.StructKind:
		id := c.fresh("t_struct")
		// Register the name (and forward-declare it) before the fields are
		// named: a field may reach back to this struct through a Vec or
		// HashMap, and that reference is legal only once the name exists.
		c.seen[key] = id
		c.defs = append(c.defs, fmt.Sprintf("typedef struct %s %s;", id, id))
		fields := make([]string, len(ty.St.Fields))
		for i, f := range ty.St.Fields {
			fields[i] = fmt.Sprintf("%s %s;", c.name(f.Ty), cField(f.Name))
		}
		c.defs = append(c.defs, fmt.Sprintf("struct %s { %s };", id, strings.Join(fields, " ")))
		return id
	case types.EnumKind:
		id := c.fresh("t_enum")
		c.seen[key] = id
		c.defs = append(c.defs, fmt.Sprintf("typedef struct %s %s;", id, id))
		var members []string
		for _, v := range ty.En.Variants {
			if len(v.Fields) == 0 {
				continue
			}
			fields := make([]string, len(v.Fields))
			for i, f := range v.Fields {
				fields[i] = fmt.Sprintf("%s %s;", c.name(f.Ty), cField(f.Name))
			}
			members = append(members, fmt.Sprintf("struct { %s } v%d;", strings.Join(fields, " "), v.Index))
		}
		def := "typedef struct { int32_t tag;"
		if len(members) > 0 {
			def += " union { " + strings.Join(members, " ") + " } u;"
		}
		def += " };"
		c.defs = append(c.defs, fmt.Sprintf("struct %s {%s", id, strings.TrimPrefix(def, "typedef struct {")))
		return id
	default:
		panic("cgen: unsupported kind " + fmt.Sprint(ty.Kind))
	}
	c.seen[key] = out
	return out
}

func fld(name string, ty *types.Type) types.Field { return types.Field{Name: name, Ty: ty} }

func ptr(t *types.Type) *types.Type         { return types.PtrTo(t) }
func ref(t *types.Type, m bool) *types.Type { return types.RefTo(t, m) }

// probeTypes builds the matrix. Each entry becomes a C typedef plus a row of
// sizeof/_Alignof assertions.
func probeTypes() []*types.Type {
	three := &types.Type{Kind: types.StructKind, St: &types.Struct{
		Name: "Three", Base: "Three",
		Fields: []types.Field{fld("a", types.U8T), fld("b", types.U8T), fld("c", types.U8T)},
	}}
	pad := &types.Type{Kind: types.StructKind, St: &types.Struct{
		Name: "Pad", Base: "Pad",
		Fields: []types.Field{fld("a", types.U8T), fld("b", types.I32T), fld("c", types.U8T)},
	}}
	inner := &types.Type{Kind: types.StructKind, St: &types.Struct{
		Name: "Inner", Base: "Inner",
		Fields: []types.Field{fld("x", types.U8T), fld("y", types.I64T)},
	}}
	outer := &types.Type{Kind: types.StructKind, St: &types.Struct{
		Name: "Outer", Base: "Outer",
		Fields: []types.Field{fld("a", types.U8T), fld("in", inner), fld("z", types.BoolT)},
	}}
	unitEnum := &types.Type{Kind: types.EnumKind, En: &types.Enum{
		Name: "UnitEnum",
		Variants: []types.Variant{
			{Name: "A", Index: 0}, {Name: "B", Index: 1}, {Name: "C", Index: 2},
		},
	}}
	payloadEnum := &types.Type{Kind: types.EnumKind, En: &types.Enum{
		Name: "PayloadEnum",
		Variants: []types.Variant{
			{Name: "A", Index: 0},
			{Name: "B", Index: 1, Fields: []types.Field{fld("f0", types.I64T)}},
			{Name: "C", Index: 2, Fields: []types.Field{fld("f0", types.U8T), fld("f1", types.U8T)}},
		},
	}}
	wideEnum := &types.Type{Kind: types.EnumKind, En: &types.Enum{
		Name: "WideEnum",
		Variants: []types.Variant{
			{Name: "Small", Index: 0, Fields: []types.Field{fld("f0", types.U8T)}},
			{Name: "Big", Index: 1, Fields: []types.Field{fld("f0", types.U8T), fld("f1", types.F64T)}},
		},
	}}

	// `()` and `!` are deliberately absent: C has no zero-sized object, so
	// there is no C declaration for `cc` to size. The back end spells unit
	// as `void` and never takes its sizeof.
	return []*types.Type{
		types.BoolT, types.CharT, types.StrT, types.StringT,
		&types.Type{Kind: types.I8}, &types.Type{Kind: types.I16}, types.I32T, types.I64T,
		types.U8T, &types.Type{Kind: types.U16}, types.U32T, &types.Type{Kind: types.U64},
		types.UsizeT, &types.Type{Kind: types.F32}, types.F64T,
		ptr(types.I32T), ref(types.I64T, true), &types.Type{Kind: types.Fn},
		&types.Type{Kind: types.Slice, Elem: types.I32T},
		&types.Type{Kind: types.Array, Elem: types.I32T, Len: 4},
		&types.Type{Kind: types.Array, Elem: three, Len: 3},
		&types.Type{Kind: types.VecKind, Elem: types.U8T},
		&types.Type{Kind: types.VecKind, Elem: types.StringT},
		&types.Type{Kind: types.MapKind, Key: types.StrT, Val: types.I32T},
		&types.Type{Kind: types.Tuple, Elems: []*types.Type{types.U8T, types.I32T}},
		&types.Type{Kind: types.Tuple, Elems: []*types.Type{types.StrT, types.BoolT, &types.Type{Kind: types.U64}}},
		three, pad, inner, outer,
		unitEnum, payloadEnum, wideEnum,
		&types.Type{Kind: types.StructKind, St: &types.Struct{
			Name: "WithEnum", Base: "WithEnum",
			Fields: []types.Field{fld("tag", types.U8T), fld("e", payloadEnum)},
		}},
	}
}

func TestLayoutMatchesTheCCompiler(t *testing.T) {
	t.Parallel()
	cc := os.Getenv("CC")
	if cc == "" {
		cc = "cc"
	}
	if _, err := exec.LookPath(cc); err != nil {
		t.Skipf("no C compiler on PATH (%s): layout is verified against cc, skipping", err)
	}

	g := newCgen()
	probes := probeTypes()

	// Emit one typedef per probe so sizeof can be taken without repeating
	// the declarator.
	probeNames := make([]string, len(probes))
	for i, ty := range probes {
		n := g.fresh("p")
		g.defs = append(g.defs, fmt.Sprintf("typedef %s %s;", g.name(ty), n))
		probeNames[i] = n
	}

	var sb strings.Builder
	sb.WriteString("#include <stdio.h>\n#include <stdint.h>\n#include <stddef.h>\n#include <stdbool.h>\n")
	sb.WriteString("typedef struct { char _unused; } zx_unit;\n")
	sb.WriteString("typedef struct { char *ptr; size_t len; size_t cap; } zx_String;\n")
	g.defs = append(g.defs, g.late...)
	for _, d := range g.defs {
		sb.WriteString(d + "\n")
	}
	sb.WriteString("int main(void) {\n")
	for i, ty := range probes {
		n := probeNames[i]
		fmt.Fprintf(&sb, "  printf(\"%%s size %%zu\\n\", %q, sizeof(%s));\n", "probe"+strconv.Itoa(i), n)
		fmt.Fprintf(&sb, "  printf(\"%%s align %%zu\\n\", %q, _Alignof(%s));\n", "probe"+strconv.Itoa(i), n)
		emitOffsetProbes(&sb, "probe"+strconv.Itoa(i), ty, g)
	}
	sb.WriteString("  return 0;\n}\n")

	dir := t.TempDir()
	csrc := filepath.Join(dir, "probe.c")
	if err := os.WriteFile(csrc, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "probe")
	build := exec.Command(cc, "-std=gnu11", "-O0", "-o", bin, csrc)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("cc failed: %v\n%s\n--- source ---\n%s", err, out, sb.String())
	}
	run := exec.Command(bin)
	raw, err := run.Output()
	if err != nil {
		t.Fatalf("running the probe failed: %v", err)
	}

	// got["probe3 size"] = 8
	got := map[string]int64{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
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
	for i, ty := range probes {
		key := "probe" + strconv.Itoa(i)
		want, err := tgt.Of(ty)
		if err != nil {
			t.Errorf("%s: Of(%v): %v", key, ty, err)
			continue
		}
		if c, ok := got[key+" size"]; !ok {
			t.Errorf("%s: cc reported no size", key)
		} else if c != want.Size {
			t.Errorf("%s (%v): size %d, table says %d", key, ty, c, want.Size)
		}
		if c, ok := got[key+" align"]; !ok {
			t.Errorf("%s: cc reported no alignment", key)
		} else if c != want.Align {
			t.Errorf("%s (%v): align %d, table says %d", key, ty, c, want.Align)
		}
		// Field offsets, recorded by emitOffsetProbes.
		switch ty.Kind {
		case types.StructKind:
			sl, err := tgt.Struct(ty.St)
			if err != nil {
				t.Errorf("%s: Struct: %v", key, err)
				continue
			}
			for _, f := range sl.Fields {
				k := key + " off." + f.Name
				if c, ok := got[k]; !ok {
					t.Errorf("%s field %s: cc reported no offset", key, f.Name)
				} else if c != f.Offset {
					t.Errorf("%s field %s: cc says offset %d, table says %d", key, f.Name, c, f.Offset)
				}
			}
		case types.EnumKind:
			el, err := tgt.Enum(ty.En)
			if err != nil {
				t.Errorf("%s: Enum: %v", key, err)
				continue
			}
			for _, v := range el.Variants {
				for _, f := range v.Fields {
					k := key + " off.v" + strconv.Itoa(v.Index) + "." + f.Name
					wantOff := v.Offset + f.Offset
					if c, ok := got[k]; !ok {
						t.Errorf("%s variant %s field %s: cc reported no offset", key, v.Name, f.Name)
					} else if c != wantOff {
						t.Errorf("%s variant %s field %s: cc says offset %d, table says %d",
							key, v.Name, f.Name, c, wantOff)
					}
				}
			}
		}
	}
}

// emitOffsetProbes prints offsetof for every addressable field of a struct or
// enum probe. Unions share offset 0 across variants, so enum offsets are taken
// through each variant's own payload struct.
func emitOffsetProbes(sb *strings.Builder, key string, ty *types.Type, g *cgen) {
	switch ty.Kind {
	case types.StructKind:
		for _, f := range ty.St.Fields {
			fmt.Fprintf(sb, "  printf(\"%%s %%d\\n\", %q, (int)offsetof(%s, %s));\n",
				key+" off."+f.Name, g.name(ty), cField(f.Name))
		}
	case types.EnumKind:
		if !ty.En.HasPayload() {
			return
		}
		for _, v := range ty.En.Variants {
			for _, f := range v.Fields {
				fmt.Fprintf(sb, "  printf(\"%%s %%d\\n\", %q, (int)offsetof(%s, u.v%d.%s));\n",
					key+" off.v"+strconv.Itoa(v.Index)+"."+f.Name, g.name(ty), v.Index, cField(f.Name))
			}
		}
	}
}
