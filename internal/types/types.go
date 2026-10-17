// Package types defines Zexi's semantic types. A Type is a small value struct
// with pointer children, so equality is a structural comparison. This is
// deliberately backend-agnostic: mapping a Type to a C type or a machine layout
// happens in codegen and layout, never here.
package types

import "fmt"

// Kind enumerates the type constructors.
type Kind uint8

const (
	Invalid Kind = iota
	Unit
	Bool
	Char
	Str
	Never

	I8
	I16
	I32
	I64
	Isize

	U8
	U16
	U32
	U64
	Usize

	F32
	F64

	Ptr   // *T
	Ref   // &T, &mut T
	Array // [T; N]
	Slice // [T]
	Tuple // (A, B, ...)
	Fn    // fn(A, B) -> R
)

// Type is a semantic type. Only the fields relevant to Kind are set.
type Type struct {
	Kind  Kind
	Elem  *Type   // Ptr, Ref, Array, Slice
	Mut   bool    // Ref/Ptr mutability
	Len   int64   // Array
	Elems []*Type // Tuple
	// Fn
	Params []*Type
	Ret    *Type
}

// Shorthands for the primitive types.
var (
	UnitT  = &Type{Kind: Unit}
	BoolT  = &Type{Kind: Bool}
	CharT  = &Type{Kind: Char}
	StrT   = &Type{Kind: Str}
	NeverT = &Type{Kind: Never}
	I32T   = &Type{Kind: I32}
	I64T   = &Type{Kind: I64}
	F64T   = &Type{Kind: F64}
	U32T   = &Type{Kind: U32}
	UsizeT = &Type{Kind: Usize}
)

// Builtins maps predeclared type names to types.
var Builtins = map[string]*Type{
	"i8":    {Kind: I8},
	"i16":   {Kind: I16},
	"i32":   I32T,
	"i64":   I64T,
	"isize": {Kind: Isize},
	"u8":    {Kind: U8},
	"u16":   {Kind: U16},
	"u32":   U32T,
	"u64":   {Kind: U64},
	"usize": UsizeT,
	"f32":   {Kind: F32},
	"f64":   F64T,
	"bool":  BoolT,
	"char":  CharT,
	"str":   StrT,
	"never": NeverT,
}

// PtrTo returns *T.
func PtrTo(elem *Type) *Type { return &Type{Kind: Ptr, Elem: elem} }

// RefTo returns &T or &mut T.
func RefTo(elem *Type, mut bool) *Type { return &Type{Kind: Ref, Elem: elem, Mut: mut} }

// Numeric-kind predicates.

func (k Kind) IsInt() bool {
	switch k {
	case I8, I16, I32, I64, Isize, U8, U16, U32, U64, Usize:
		return true
	}
	return false
}

func (k Kind) IsFloat() bool   { return k == F32 || k == F64 }
func (k Kind) IsNumeric() bool { return k.IsInt() || k.IsFloat() }

func (k Kind) IsSigned() bool {
	switch k {
	case I8, I16, I32, I64, Isize:
		return true
	}
	return false
}

func (k Kind) IsUnsigned() bool {
	switch k {
	case U8, U16, U32, U64, Usize:
		return true
	}
	return false
}

// Equal reports structural equality of two types. A nil type equals only nil.
func Equal(a, b *Type) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Kind != b.Kind || a.Mut != b.Mut || a.Len != b.Len {
		return false
	}
	if !Equal(a.Elem, b.Elem) || !Equal(a.Ret, b.Ret) {
		return false
	}
	if len(a.Elems) != len(b.Elems) || len(a.Params) != len(b.Params) {
		return false
	}
	for i := range a.Elems {
		if !Equal(a.Elems[i], b.Elems[i]) {
			return false
		}
	}
	for i := range a.Params {
		if !Equal(a.Params[i], b.Params[i]) {
			return false
		}
	}
	return true
}

// String renders a type for diagnostics.
func (t *Type) String() string {
	if t == nil {
		return "<unknown>"
	}
	switch t.Kind {
	case Invalid:
		return "<invalid>"
	case Unit:
		return "()"
	case Bool:
		return "bool"
	case Char:
		return "char"
	case Str:
		return "str"
	case Never:
		return "never"
	case I8:
		return "i8"
	case I16:
		return "i16"
	case I32:
		return "i32"
	case I64:
		return "i64"
	case Isize:
		return "isize"
	case U8:
		return "u8"
	case U16:
		return "u16"
	case U32:
		return "u32"
	case U64:
		return "u64"
	case Usize:
		return "usize"
	case F32:
		return "f32"
	case F64:
		return "f64"
	case Ptr:
		return "*" + t.Elem.String()
	case Ref:
		if t.Mut {
			return "&mut " + t.Elem.String()
		}
		return "&" + t.Elem.String()
	case Array:
		return fmt.Sprintf("[%s; %d]", t.Elem, t.Len)
	case Slice:
		return "[" + t.Elem.String() + "]"
	case Tuple:
		s := "("
		for i, e := range t.Elems {
			if i > 0 {
				s += ", "
			}
			s += e.String()
		}
		return s + ")"
	case Fn:
		s := "fn("
		for i, e := range t.Params {
			if i > 0 {
				s += ", "
			}
			s += e.String()
		}
		s += ")"
		if t.Ret != nil && t.Ret.Kind != Unit {
			s += " -> " + t.Ret.String()
		}
		return s
	}
	return "<type>"
}
