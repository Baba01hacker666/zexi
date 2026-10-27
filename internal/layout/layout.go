// Package layout computes the machine-level representation of Zexi types:
// sizes, alignments, field offsets, and enum tagging.
//
// Today the C back end never needs this — it writes types into C source and
// lets `cc` decide the layout with `sizeof` and `offsetof`. That works exactly
// as long as C is the only back end. As soon as something emits a lower-level
// target (LLVM IR, Cranelift, wasm) there is no `sizeof` to lean on, and
// every question the emitter asks — how big is this local, where does field 3
// start, does the tag live before or after the payload, is this enum niche-
// optimized — has to be answered by the compiler itself.
//
// The rules here are therefore not invented; they describe the layout `cc`
// produces for the C the current back end emits, so both back ends agree on
// an ABI and values can cross between them. The package is independent of any
// back end (it imports only internal/types) and allocation-free on the common
// path.
//
// # Rules
//
// The model is the LP64 System V layout that gcc and clang implement on
// x86-64 and AArch64:
//
//   - a scalar's alignment equals its size;
//   - a pointer is PointerSize wide and pointer-aligned;
//   - an array's alignment is its element's and its size is N times the
//     element's, with no trailing padding beyond the element stride;
//   - a struct's alignment is the maximum of its fields', each field starts
//     at the next offset that satisfies its own alignment, and the size is
//     rounded up to the struct's alignment;
//   - an enum is `int32_t tag` followed by a union of per-variant payload
//     structs, the union aligned to the strictest variant.
//
// A zero-sized type has alignment 1, matching C's rule that no object is
// required to have an alignment stricter than a character.
package layout

import (
	"fmt"

	"zexi/internal/types"
)

// Target describes the machine a layout is computed for. Only the pointer
// width matters today; a second target (wasm32) needs nothing else, because
// the alignment rules above are the same for every LP64/ILP32 platform Zexi
// targets.
type Target struct {
	// PointerSize is the width of a pointer in bytes. Zero means 8.
	PointerSize int64
}

// Default is the target of the machine running the compiler's code
// generator: LP64, 64-bit pointers.
func Default() Target { return Target{PointerSize: 8} }

// ptr returns the pointer width, defaulting to 8 for the zero Target.
func (t Target) ptr() int64 {
	if t.PointerSize <= 0 {
		return 8
	}
	return t.PointerSize
}

// Sized is the size and alignment of a type.
type Sized struct {
	Size  int64
	Align int64
}

// IsZeroSized reports whether the type occupies no storage. Zero-sized types
// still have alignment 1, so an array of them behaves like C's.
func (s Sized) IsZeroSized() bool { return s.Size == 0 }

// Field is one laid-out field of a struct, tuple, or variant.
type Field struct {
	Name   string
	Ty     *types.Type
	Offset int64
	Sized  Sized
}

// Struct is the layout of a product type. Fields are in declaration order;
// offsets are absolute within the struct.
type Struct struct {
	Name   string
	Size   int64
	Align  int64
	Fields []Field
}

// Sized returns the struct's size and alignment.
func (s Struct) Sized() Sized { return Sized{Size: s.Size, Align: s.Align} }

// Field looks a field up by name.
func (s Struct) Field(name string) (Field, bool) {
	for _, f := range s.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return Field{}, false
}

// Variant is one laid-out enum variant. Offset is where the variant's payload
// begins inside the enum value; a payload-less variant has no fields and
// Offset is meaningless.
type Variant struct {
	Name   string
	Index  int
	Offset int64
	Size   int64
	Align  int64
	Fields []Field
}

// Enum is the layout of a tagged union: an int32 tag at TagOffset, and the
// union of variant payloads at PayloadOffset.
type Enum struct {
	Name          string
	Size          int64
	Align         int64
	TagOffset     int64
	TagSize       int64
	PayloadOffset int64
	// PayloadSize is the size of the union, 0 when no variant carries data.
	PayloadSize int64
	Variants    []Variant
}

// Sized returns the enum's size and alignment.
func (e Enum) Sized() Sized { return Sized{Size: e.Size, Align: e.Align} }

// Variant looks a variant up by name.
func (e Enum) Variant(name string) (Variant, bool) {
	for _, v := range e.Variants {
		if v.Name == name {
			return v, true
		}
	}
	return Variant{}, false
}

// tagSize is the width of the discriminant field, matching the `int32_t tag`
// the C back end emits.
const tagSize int64 = 4

// Of returns the size and alignment of ty.
//
// It fails for types that have no machine representation: type parameters
// that were never monomorphized, invalid types, and generic templates. Those
// are compiler bugs by the time a back end runs, so the error names the type
// rather than returning a plausible-looking size.
func (t Target) Of(ty *types.Type) (Sized, error) {
	return t.of(ty, nil)
}

// of is Of with a recursion guard. A struct that contains itself by value has
// no finite size; the guard turns that into an error instead of a stack
// overflow.
func (t Target) of(ty *types.Type, visiting []*types.Type) (Sized, error) {
	if ty == nil {
		return Sized{}, fmt.Errorf("layout: nil type")
	}
	switch ty.Kind {
	case types.Unit, types.Never:
		return Sized{Size: 0, Align: 1}, nil
	case types.Bool, types.I8, types.U8:
		return Sized{Size: 1, Align: 1}, nil
	case types.I16, types.U16:
		return Sized{Size: 2, Align: 2}, nil
	case types.I32, types.U32, types.F32, types.Char:
		// `char` is a Unicode scalar value and the C back end types it as
		// uint32_t, so it is four bytes, not one.
		return Sized{Size: 4, Align: 4}, nil
	case types.I64, types.U64, types.F64, types.Isize, types.Usize:
		return Sized{Size: 8, Align: 8}, nil
	case types.Str, types.Ptr, types.Ref, types.Fn:
		p := t.ptr()
		return Sized{Size: p, Align: p}, nil
	case types.StringKind, types.VecKind, types.MapKind:
		// All three are owning handles of the shape
		// `{ pointer, size_t len, size_t cap }`.
		p := t.ptr()
		return Sized{Size: 3 * p, Align: p}, nil
	case types.Slice:
		// A slice is a fat pointer `(ptr, len)`.
		p := t.ptr()
		return Sized{Size: 2 * p, Align: p}, nil
	case types.Array:
		if ty.Len < 0 {
			return Sized{}, fmt.Errorf("layout: array with negative length %d", ty.Len)
		}
		elem, err := t.of(ty.Elem, visiting)
		if err != nil {
			return Sized{}, err
		}
		return Sized{Size: elem.Size * ty.Len, Align: elem.Align}, nil
	case types.Tuple:
		fields := make([]Field, 0, len(ty.Elems))
		for i, e := range ty.Elems {
			fields = append(fields, Field{Name: fmt.Sprintf("%d", i), Ty: e})
		}
		return t.aggregate(fields, visiting)
	case types.StructKind:
		if ty.St == nil {
			return Sized{}, fmt.Errorf("layout: struct type without a definition")
		}
		if err := enter(visiting, ty); err != nil {
			return Sized{}, err
		}
		s, err := t.structOf(ty.St, append(visiting, ty))
		if err != nil {
			return Sized{}, err
		}
		return s.Sized(), nil
	case types.EnumKind:
		if ty.En == nil {
			return Sized{}, fmt.Errorf("layout: enum type without a definition")
		}
		if err := enter(visiting, ty); err != nil {
			return Sized{}, err
		}
		// Variants are laid out below; they cannot recurse through this
		// enum's own name because a variant field of the enum's type would
		// already have been rejected by the checker.
		e, err := t.enumOf(ty.En, append(visiting, ty))
		if err != nil {
			return Sized{}, err
		}
		return e.Sized(), nil
	case types.ParamKind:
		return Sized{}, fmt.Errorf("layout: type parameter %q was not monomorphized", ty.ParamName)
	default:
		return Sized{}, fmt.Errorf("layout: type %v has no machine representation", ty)
	}
}

// Struct returns the full layout of a struct, including field offsets.
func (t Target) Struct(s *types.Struct) (Struct, error) {
	return t.structOf(s, nil)
}

func (t Target) structOf(s *types.Struct, visiting []*types.Type) (Struct, error) {
	if s == nil {
		return Struct{}, fmt.Errorf("layout: nil struct")
	}
	if s.Generic {
		return Struct{}, fmt.Errorf("layout: generic struct %q was not instantiated", s.Name)
	}
	fields := make([]Field, len(s.Fields))
	for i, f := range s.Fields {
		fields[i] = Field{Name: f.Name, Ty: f.Ty}
	}
	out, err := t.aggregateNamed(s.Name, fields, visiting)
	if err != nil {
		return Struct{}, err
	}
	return out, nil
}

// Enum returns the full layout of an enum, including tag and variant offsets.
func (t Target) Enum(e *types.Enum) (Enum, error) {
	return t.enumOf(e, nil)
}

func (t Target) enumOf(e *types.Enum, visiting []*types.Type) (Enum, error) {
	if e == nil {
		return Enum{}, fmt.Errorf("layout: nil enum")
	}
	if e.Generic {
		return Enum{}, fmt.Errorf("layout: generic enum %q was not instantiated", e.Name)
	}

	// The tag sits first, exactly like the C back end's `int32_t tag`.
	out := Enum{Name: e.Name, TagOffset: 0, TagSize: tagSize, PayloadOffset: tagSize}

	// Each variant's payload is a struct; the union takes the strictest
	// alignment and the largest size.
	maxAlign := int64(1)
	maxSize := int64(0)
	variants := make([]Variant, len(e.Variants))
	for i, v := range e.Variants {
		fields := make([]Field, len(v.Fields))
		for j, f := range v.Fields {
			fields[j] = Field{Name: f.Name, Ty: f.Ty}
		}
		agg, err := t.aggregateNamed(v.Name, fields, visiting)
		if err != nil {
			return Enum{}, fmt.Errorf("layout: enum %s variant %s: %w", e.Name, v.Name, err)
		}
		variants[i] = Variant{
			Name:   v.Name,
			Index:  v.Index,
			Size:   agg.Size,
			Align:  agg.Align,
			Fields: agg.Fields,
		}
		if agg.Align > maxAlign {
			maxAlign = agg.Align
		}
		if agg.Size > maxSize {
			maxSize = agg.Size
		}
	}

	out.Align = maxAlign
	if out.Align < tagSize {
		out.Align = tagSize
	}
	if e.HasPayload() {
		// The union starts after the tag, at the union's own alignment.
		out.PayloadOffset = alignUp(tagSize, maxAlign)
		out.PayloadSize = alignUp(maxSize, maxAlign)
	}
	for i := range variants {
		variants[i].Offset = out.PayloadOffset
	}
	out.Variants = variants
	out.Size = alignUp(out.PayloadOffset+out.PayloadSize, out.Align)
	return out, nil
}

// aggregate lays out anonymous structural fields (a tuple's elements).
func (t Target) aggregate(fields []Field, visiting []*types.Type) (Sized, error) {
	s, err := t.aggregateNamed("", fields, visiting)
	if err != nil {
		return Sized{}, err
	}
	return s.Sized(), nil
}

// aggregateNamed assigns offsets to fields in declaration order and rounds the
// total up to the struct's alignment.
func (t Target) aggregateNamed(name string, fields []Field, visiting []*types.Type) (Struct, error) {
	out := Struct{Name: name, Align: 1, Fields: fields}
	var off int64
	for i := range out.Fields {
		sz, err := t.of(out.Fields[i].Ty, visiting)
		if err != nil {
			return Struct{}, err
		}
		off = alignUp(off, sz.Align)
		out.Fields[i].Offset = off
		out.Fields[i].Sized = sz
		off += sz.Size
		if sz.Align > out.Align {
			out.Align = sz.Align
		}
	}
	out.Size = alignUp(off, out.Align)
	return out, nil
}

// enter rejects a type that is already being laid out, which means the type
// contains itself by value and therefore has no finite size.
func enter(visiting []*types.Type, ty *types.Type) error {
	for _, v := range visiting {
		if v == ty {
			return fmt.Errorf("layout: type %v contains itself by value", ty)
		}
	}
	return nil
}

// alignUp rounds n up to the next multiple of align. align must be positive.
func alignUp(n, align int64) int64 {
	if align <= 1 {
		return n
	}
	rem := n % align
	if rem == 0 {
		return n
	}
	return n + align - rem
}

// AlignUp is alignUp for callers outside the package: an offset rounded up to
// the next multiple of align.
func AlignUp(n, align int64) int64 { return alignUp(n, align) }
