package layout

import (
	"testing"

	"zexi/internal/types"
)

// structOf is a test helper: an instantiated struct with the given fields.
func structOf(name string, fields ...types.Field) *types.Type {
	return &types.Type{Kind: types.StructKind, St: &types.Struct{Name: name, Base: name, Fields: fields}}
}

func field(name string, ty *types.Type) types.Field { return types.Field{Name: name, Ty: ty} }

func arr(elem *types.Type, n int64) *types.Type {
	return &types.Type{Kind: types.Array, Elem: elem, Len: n}
}

func vec(elem *types.Type) *types.Type { return &types.Type{Kind: types.VecKind, Elem: elem} }

func mapOf(k, v *types.Type) *types.Type {
	return &types.Type{Kind: types.MapKind, Key: k, Val: v}
}

func TestScalarSizes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ty   *types.Type
		want Sized
	}{
		{"unit", types.UnitT, Sized{0, 1}},
		{"never", types.NeverT, Sized{0, 1}},
		{"bool", types.BoolT, Sized{1, 1}},
		{"i8", &types.Type{Kind: types.I8}, Sized{1, 1}},
		{"u8", types.U8T, Sized{1, 1}},
		{"i16", &types.Type{Kind: types.I16}, Sized{2, 2}},
		{"i32", types.I32T, Sized{4, 4}},
		{"u32", types.U32T, Sized{4, 4}},
		{"i64", types.I64T, Sized{8, 8}},
		{"f32", &types.Type{Kind: types.F32}, Sized{4, 4}},
		{"f64", types.F64T, Sized{8, 8}},
		{"char", types.CharT, Sized{4, 4}},
		{"usize", types.UsizeT, Sized{8, 8}},
		{"str", types.StrT, Sized{8, 8}},
		{"string", types.StringT, Sized{24, 8}},
		{"&i32", types.RefTo(types.I32T, false), Sized{8, 8}},
		{"*i64", types.PtrTo(types.I64T), Sized{8, 8}},
		{"fn", &types.Type{Kind: types.Fn}, Sized{8, 8}},
		{"[bool]", &types.Type{Kind: types.Slice, Elem: types.BoolT}, Sized{16, 8}},
	}
	for _, c := range cases {
		got, err := Default().Of(c.ty)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestOwnershipHandlesAreThreeWords(t *testing.T) {
	t.Parallel()
	// String, Vec<T> and HashMap<K,V> are all `{ pointer, len, cap }`. This
	// is the promise the runtime intrinsics in the MIR rely on.
	for _, ty := range []*types.Type{
		types.StringT,
		vec(types.I32T),
		vec(types.StringT),
		mapOf(types.StrT, types.I32T),
	} {
		got, err := Default().Of(ty)
		if err != nil {
			t.Fatalf("%v: %v", ty, err)
		}
		if got != (Sized{24, 8}) {
			t.Errorf("%v: got %+v, want {24 8}", ty, got)
		}
	}
}

func TestArrayStride(t *testing.T) {
	t.Parallel()
	// An array of a 3-byte struct strides by the element's *size*, which is
	// rounded up to its alignment, not by 3.
	elem := structOf("Three", field("a", types.U8T), field("b", types.U8T), field("c", types.U8T))
	es, err := Default().Of(elem)
	if err != nil {
		t.Fatal(err)
	}
	if es != (Sized{3, 1}) {
		t.Fatalf("element: got %+v, want {3 1}", es)
	}

	got, err := Default().Of(arr(types.I32T, 4))
	if err != nil {
		t.Fatal(err)
	}
	if got != (Sized{16, 4}) {
		t.Errorf("[i32; 4]: got %+v, want {16 4}", got)
	}

	zero, err := Default().Of(arr(types.UnitT, 10))
	if err != nil {
		t.Fatal(err)
	}
	if zero != (Sized{0, 1}) {
		t.Errorf("[(); 10]: got %+v, want {0 1}", zero)
	}
}

func TestStructOffsets(t *testing.T) {
	t.Parallel()
	// u8, i32, u8 pads to: a@0, b@4, c@8, size 12.
	ty := structOf("Pad",
		field("a", types.U8T),
		field("b", types.I32T),
		field("c", types.U8T),
	)
	got, err := Default().Struct(ty.St)
	if err != nil {
		t.Fatal(err)
	}
	if got.Size != 12 || got.Align != 4 {
		t.Fatalf("size/align: got %d/%d, want 12/4", got.Size, got.Align)
	}
	want := map[string]int64{"a": 0, "b": 4, "c": 8}
	for name, off := range want {
		f, ok := got.Field(name)
		if !ok {
			t.Fatalf("field %s missing", name)
		}
		if f.Offset != off {
			t.Errorf("field %s: offset %d, want %d", name, f.Offset, off)
		}
	}
}

func TestStructAlignmentRoundsSize(t *testing.T) {
	t.Parallel()
	// A 3-byte struct followed by an i64 must start the i64 at 8, so the
	// whole thing is 16 bytes and 8-aligned.
	three := structOf("Three", field("a", types.U8T), field("b", types.U8T), field("c", types.U8T))
	ty := structOf("Outer", field("head", three), field("tail", types.I64T))
	got, err := Default().Struct(ty.St)
	if err != nil {
		t.Fatal(err)
	}
	if got.Align != 8 || got.Size != 16 {
		t.Fatalf("got size/align %d/%d, want 16/8", got.Size, got.Align)
	}
	if f, _ := got.Field("tail"); f.Offset != 8 {
		t.Errorf("tail offset %d, want 8", f.Offset)
	}
}

func TestNestedStructOffsets(t *testing.T) {
	t.Parallel()
	inner := structOf("Inner", field("x", types.U8T), field("y", types.I64T))
	outer := structOf("Outer", field("a", types.U8T), field("in", inner))
	got, err := Default().Struct(outer.St)
	if err != nil {
		t.Fatal(err)
	}
	// Outer.a@0; Inner is 8-aligned, so it starts at 8. Inner itself is
	// 16 bytes (x@0, y@8), giving a 24-byte total rounded to 8.
	if got.Size != 24 || got.Align != 8 {
		t.Fatalf("got size/align %d/%d, want 24/8", got.Size, got.Align)
	}
	if f, _ := got.Field("in"); f.Offset != 8 {
		t.Errorf("in offset %d, want 8", f.Offset)
	}
}

func TestTupleLayout(t *testing.T) {
	t.Parallel()
	ty := &types.Type{Kind: types.Tuple, Elems: []*types.Type{types.U8T, types.I32T}}
	got, err := Default().Of(ty)
	if err != nil {
		t.Fatal(err)
	}
	if got != (Sized{8, 4}) {
		t.Errorf("(u8, i32): got %+v, want {8 4}", got)
	}
}

func TestEnumTagAndPayload(t *testing.T) {
	t.Parallel()
	// enum E { A, B(i64), C(u8, u8) } — the union aligns to 8, so the
	// payload starts at 8 and the whole enum is 16 bytes.
	en := &types.Enum{
		Name: "E",
		Variants: []types.Variant{
			{Name: "A", Index: 0},
			{Name: "B", Index: 1, Fields: []types.Field{field("0", types.I64T)}},
			{Name: "C", Index: 2, Fields: []types.Field{field("0", types.U8T), field("1", types.U8T)}},
		},
	}
	got, err := Default().Enum(en)
	if err != nil {
		t.Fatal(err)
	}
	if got.TagOffset != 0 || got.TagSize != 4 {
		t.Errorf("tag at %d size %d, want 0/4", got.TagOffset, got.TagSize)
	}
	if got.PayloadOffset != 8 {
		t.Errorf("payload at %d, want 8", got.PayloadOffset)
	}
	if got.Align != 8 || got.Size != 16 {
		t.Errorf("got size/align %d/%d, want 16/8", got.Size, got.Align)
	}
	if v, _ := got.Variant("B"); v.Offset != 8 || v.Size != 8 {
		t.Errorf("variant B: offset %d size %d, want 8/8", v.Offset, v.Size)
	}
	if v, ok := got.Variant("C"); !ok || len(v.Fields) != 2 {
		t.Errorf("variant C missing its fields")
	}
}

func TestEnumsAreFourBytesWithoutPayload(t *testing.T) {
	t.Parallel()
	en := &types.Enum{
		Name: "Unit",
		Variants: []types.Variant{
			{Name: "A", Index: 0},
			{Name: "B", Index: 1},
		},
	}
	got, err := Default().Enum(en)
	if err != nil {
		t.Fatal(err)
	}
	if got.Size != 4 || got.Align != 4 {
		t.Errorf("got size/align %d/%d, want 4/4", got.Size, got.Align)
	}
	if got.PayloadSize != 0 {
		t.Errorf("payload size %d, want 0", got.PayloadSize)
	}
}

func TestEnumWithPointerPayload(t *testing.T) {
	t.Parallel()
	// The union aligns to 8 even though the tag is only 4 bytes wide.
	en := &types.Enum{
		Name: "Maybe",
		Variants: []types.Variant{
			{Name: "Nothing", Index: 0},
			{Name: "Some", Index: 1, Fields: []types.Field{field("0", types.StrT)}},
		},
	}
	got, err := Default().Enum(en)
	if err != nil {
		t.Fatal(err)
	}
	if got.Size != 16 || got.Align != 8 || got.PayloadOffset != 8 {
		t.Errorf("got size/align/payload %d/%d/%d, want 16/8/8", got.Size, got.Align, got.PayloadOffset)
	}
}

func TestEnumWithThreeFieldVariantAlignsToItsStrictestField(t *testing.T) {
	t.Parallel()
	// A variant containing an f64 forces the union to 8-alignment even
	// though every other variant is byte-sized.
	en := &types.Enum{
		Name: "Mixed",
		Variants: []types.Variant{
			{Name: "Small", Index: 0, Fields: []types.Field{field("0", types.U8T)}},
			{Name: "Big", Index: 1, Fields: []types.Field{field("0", types.U8T), field("1", types.F64T)}},
		},
	}
	got, err := Default().Enum(en)
	if err != nil {
		t.Fatal(err)
	}
	if got.Align != 8 || got.PayloadOffset != 8 {
		t.Errorf("got align/payload %d/%d, want 8/8", got.Align, got.PayloadOffset)
	}
	// Big is u8@0, pad, f64@8 = 16 bytes.
	if v, _ := got.Variant("Big"); v.Size != 16 {
		t.Errorf("variant Big size %d, want 16", v.Size)
	}
}

func TestPointerWidthIsTargetDependent(t *testing.T) {
	t.Parallel()
	w32 := Target{PointerSize: 4}
	got, err := w32.Of(types.StringT)
	if err != nil {
		t.Fatal(err)
	}
	if got != (Sized{12, 4}) {
		t.Errorf("32-bit String: got %+v, want {12 4}", got)
	}
	got, err = w32.Of(types.RefTo(types.I64T, true))
	if err != nil {
		t.Fatal(err)
	}
	if got != (Sized{4, 4}) {
		t.Errorf("32-bit pointer: got %+v, want {4 4}", got)
	}
}

func TestZeroTargetDefaultsToEightBytePointers(t *testing.T) {
	t.Parallel()
	got, err := (Target{}).Of(types.StrT)
	if err != nil {
		t.Fatal(err)
	}
	if got != (Sized{8, 8}) {
		t.Errorf("got %+v, want {8 8}", got)
	}
}

func TestUnrepresentableTypesError(t *testing.T) {
	t.Parallel()
	cases := map[string]*types.Type{
		"nil":        nil,
		"param":      {Kind: types.ParamKind, ParamName: "T"},
		"invalid":    {Kind: types.Invalid},
		"generic":    {Kind: types.StructKind, St: &types.Struct{Name: "Pair", Generic: true}},
		"no-struct":  {Kind: types.StructKind},
		"negative-n": arr(types.U8T, -1),
	}
	for name, ty := range cases {
		if _, err := Default().Of(ty); err == nil {
			t.Errorf("%s: expected an error, got none", name)
		}
	}
}

func TestRecursiveTypeIsRejected(t *testing.T) {
	t.Parallel()
	// A struct holding itself by value has no finite size; laying it out
	// must fail rather than recurse forever.
	self := &types.Struct{Name: "Self", Base: "Self"}
	ty := &types.Type{Kind: types.StructKind, St: self}
	self.Fields = []types.Field{field("me", ty)}
	if _, err := Default().Of(ty); err == nil {
		t.Fatal("expected an error for a struct containing itself by value")
	}
}

func TestAlignUp(t *testing.T) {
	t.Parallel()
	cases := []struct{ n, align, want int64 }{
		{0, 1, 0}, {0, 8, 0}, {1, 8, 8}, {8, 8, 8}, {9, 8, 16},
		{4, 4, 4}, {5, 4, 8}, {3, 1, 3}, {7, 2, 8},
	}
	for _, c := range cases {
		if got := AlignUp(c.n, c.align); got != c.want {
			t.Errorf("AlignUp(%d, %d) = %d, want %d", c.n, c.align, got, c.want)
		}
	}
}

func TestZeroSizedStruct(t *testing.T) {
	t.Parallel()
	empty := structOf("Empty")
	got, err := Default().Of(empty)
	if err != nil {
		t.Fatal(err)
	}
	if got != (Sized{0, 1}) {
		t.Errorf("empty struct: got %+v, want {0 1}", got)
	}
}
