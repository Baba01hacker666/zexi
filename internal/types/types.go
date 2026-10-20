// Package types defines Zexi's semantic types. A Type is a small value struct
// with pointer children, so equality is a structural comparison. This is
// deliberately backend-agnostic: mapping a Type to a C type or a machine layout
// happens in codegen and layout, never here.
package types

import (
	"fmt"
	"slices"
	"strings"
)

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

	Ptr        // *T
	Ref        // &T, &mut T
	Array      // [T; N]
	Slice      // [T]
	Tuple      // (A, B, ...)
	Fn         // fn(A, B) -> R
	StructKind // nominal product type
	EnumKind   // nominal tagged union
	StringKind // builtin owning heap string
	VecKind    // builtin growable owning array, Vec<T>
	MapKind    // builtin hash map, HashMap<K, V>
	ParamKind  // a generic type parameter (only while checking a template)
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
	// Struct
	St *Struct
	// Enum
	En *Enum
	// MapKind: the key and value types of a `HashMap<K, V>`.
	Key *Type
	Val *Type
	// ParamKind: the parameter's name, e.g. the `T` in `Option<T>`.
	ParamName string
}

// Struct is a nominal product type. For a generic instantiation, Name is the
// canonical instantiation key (e.g. `Pair$i64$str`), Base is the source name
// (`Pair`), and Args are the type arguments.
type Struct struct {
	Name     string
	Base     string
	Args     []*Type
	Generics []string // declared type-parameter names (template only)
	Generic  bool     // true for the un-instantiated template
	Fields   []Field
	Tuple    bool
	Copy     bool // opt-in via `#[copy]`
}

// Field is one struct field.
type Field struct {
	Name string
	Ty   *Type
	Pub  bool
}

// Field returns the named field, if present.
func (s *Struct) Field(name string) (*Field, bool) {
	for i := range s.Fields {
		if s.Fields[i].Name == name {
			return &s.Fields[i], true
		}
	}
	return nil, false
}

// Enum is a nominal tagged union. For a generic instantiation, the Name/Base/
// Args convention matches Struct.
type Enum struct {
	Name     string
	Base     string
	Args     []*Type
	Generics []string // declared type-parameter names (template only)
	Generic  bool     // true for the un-instantiated template
	Variants []Variant
	Copy     bool // opt-in via `#[copy]`
}

// Variant is one case of an enum. Variants with no fields are unit variants.
type Variant struct {
	Name   string
	Fields []Field
	Tuple  bool
	Index  int
}

// Variant returns the named variant, if present.
func (e *Enum) Variant(name string) (*Variant, bool) {
	for i := range e.Variants {
		if e.Variants[i].Name == name {
			return &e.Variants[i], true
		}
	}
	return nil, false
}

// HasPayload reports whether any variant carries data.
func (e *Enum) HasPayload() bool {
	for _, v := range e.Variants {
		if len(v.Fields) > 0 {
			return true
		}
	}
	return false
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
	U8T    = &Type{Kind: U8}
	U32T   = &Type{Kind: U32}
	UsizeT = &Type{Kind: Usize}

	// StringT is the builtin owning heap string. It is not `Copy`: it owns a
	// heap buffer that is freed by its destructor.
	StringT = &Type{Kind: StringKind}
)

// Builtins maps predeclared type names to types.
var Builtins = map[string]*Type{
	"i8":     {Kind: I8},
	"i16":    {Kind: I16},
	"i32":    I32T,
	"i64":    I64T,
	"isize":  {Kind: Isize},
	"u8":     {Kind: U8},
	"u16":    {Kind: U16},
	"u32":    U32T,
	"u64":    {Kind: U64},
	"usize":  UsizeT,
	"f32":    {Kind: F32},
	"f64":    F64T,
	"bool":   BoolT,
	"char":   CharT,
	"str":    StrT,
	"never":  NeverT,
	"String": StringT,
}

// ReservedTypeNames are the predeclared generic types. Like the primitive
// builtins they occupy the type namespace and cannot be redefined.
var ReservedTypeNames = map[string]bool{
	"Vec":    true,
	"Option": true,
	"Result": true,
}

// IsBuiltinTypeName reports whether a name is taken by a predeclared type, so
// a `struct`, `enum`, or `type` may not claim it.
func IsBuiltinTypeName(name string) bool {
	if _, ok := Builtins[name]; ok {
		return true
	}
	return ReservedTypeNames[name]
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
	if a.Kind == ParamKind {
		return a.ParamName == b.ParamName
	}
	if a.Kind == StructKind {
		return a.St != nil && b.St != nil && a.St.Name == b.St.Name
	}
	if a.Kind == EnumKind {
		return a.En != nil && b.En != nil && a.En.Name == b.En.Name
	}
	if a.Kind == MapKind {
		return a.Key != nil && b.Key != nil && a.Val != nil && b.Val != nil &&
			Equal(a.Key, b.Key) && Equal(a.Val, b.Val)
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

// IsCopy reports whether values of this type are copied on assignment rather
// than moved. Primitives, references, pointers, and function pointers are
// copyable. Structs and enums are *not* copyable unless declared `#[copy]`
// (with all fields copyable). Arrays and tuples copy iff their components do.
func (t *Type) IsCopy() bool {
	if t == nil {
		return true
	}
	switch t.Kind {
	case StructKind:
		if t.St == nil || !t.St.Copy {
			return false
		}
		for _, f := range t.St.Fields {
			if !f.Ty.IsCopy() {
				return false
			}
		}
		return true
	case EnumKind:
		if t.En == nil || !t.En.Copy {
			return false
		}
		for _, v := range t.En.Variants {
			for _, f := range v.Fields {
				if !f.Ty.IsCopy() {
					return false
				}
			}
		}
		return true
	case Array:
		return t.Elem.IsCopy()
	case VecKind:
		// A Vec owns a heap buffer; copying the handle would double-free it.
		return false
	case MapKind:
		// A HashMap owns a heap table; copying the handle would double-free it.
		return false
	case Tuple:
		for _, e := range t.Elems {
			if !e.IsCopy() {
				return false
			}
		}
		return true
	case StringKind:
		return false
	case ParamKind:
		// A type parameter is treated as non-`Copy`: the move checker then
		// moves it, which is the sound conservative choice for any concrete
		// instantiation (a `Copy` instantiation can still be used freely).
		return false
	case Invalid:
		return true
	default:
		return true
	}
}

// NeedsDrop reports whether values of this type own a heap resource that must
// be released by a destructor. It is true for `String` and for any aggregate
// that transitively contains one. Types that need dropping are never `Copy`.
func (t *Type) NeedsDrop() bool {
	if t == nil {
		return false
	}
	switch t.Kind {
	case StringKind:
		return true
	case StructKind:
		if t.St == nil {
			return false
		}
		for _, f := range t.St.Fields {
			if f.Ty.NeedsDrop() {
				return true
			}
		}
	case EnumKind:
		if t.En == nil {
			return false
		}
		for _, v := range t.En.Variants {
			for _, f := range v.Fields {
				if f.Ty.NeedsDrop() {
					return true
				}
			}
		}
	case VecKind:
		// Always: the buffer itself is heap-allocated, whatever it holds.
		return true
	case MapKind:
		// Always: the table itself is heap-allocated, whatever it holds.
		return true
	case Array:
		return t.Elem.NeedsDrop()
	case Tuple:
		for _, e := range t.Elems {
			if e.NeedsDrop() {
				return true
			}
		}
	}
	return false
}

// AttrName is the identifier part of an attribute: `repr` for `repr(C)`.
func AttrName(attr string) string {
	name, _, _ := strings.Cut(attr, "(")
	return name
}

// AttrArg is the parenthesized part of an attribute: `C` for `repr(C)`,
// `never` for `inline(never)`, and "" when there is none.
func AttrArg(attr string) string {
	i := strings.IndexByte(attr, '(')
	if i < 0 || !strings.HasSuffix(attr, ")") {
		return ""
	}
	return attr[i+1 : len(attr)-1]
}

// HasAttr reports whether an attribute list contains the named attribute,
// whatever its argument.
func HasAttr(attrs []string, name string) bool {
	for _, a := range attrs {
		if AttrName(a) == name {
			return true
		}
	}
	return false
}

// AttrValue returns the argument of the named attribute, and whether it was
// present.
func AttrValue(attrs []string, name string) (string, bool) {
	for _, a := range attrs {
		if AttrName(a) == name {
			return AttrArg(a), true
		}
	}
	return "", false
}

// KnownAttrs are the attributes the compiler understands. Anything else is a
// diagnostic, so a typo like `#[Copy]` cannot silently do nothing.
var KnownAttrs = map[string]bool{
	"copy":      true, // opt a struct or enum into copy semantics
	"repr":      true, // `repr(C)`: C layout, which is already the default
	"inline":    true, // hint (or `inline(never)`) to the C compiler
	"no_mangle": true, // keep the Zexi name as the C symbol, with external linkage
	"export":    true, // alias for `no_mangle`, for the C bridge
	"allow":     true, // accepted and ignored, so lints can be named locally
}

// CopyAttr reports whether an attribute list opts a type into `Copy`.
func CopyAttr(attrs []string) bool {
	return slices.Contains(attrs, "copy")
}

// stDisplay renders a nominal type for diagnostics: `Base<Args...>` for an
// instantiation, else the plain name.
func stDisplay(name, base string, args []*Type) string {
	if len(args) == 0 {
		return name
	}
	if base == "" {
		base = name
	}
	var sb strings.Builder
	sb.WriteString(base)
	sb.WriteString("<")
	for i, a := range args {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(a.String())
	}
	sb.WriteString(">")
	return sb.String()
}

// String renders a type for diagnostics.
func (t *Type) String() string {
	if t == nil {
		return "<unknown>"
	}
	switch t.Kind {
	case Invalid:
		return "<invalid>"
	case ParamKind:
		if t.ParamName != "" {
			return t.ParamName
		}
		return "<param>"
	case Unit:
		return "()"
	case Bool:
		return "bool"
	case Char:
		return "char"
	case Str:
		return "str"
	case StringKind:
		return "String"
	case VecKind:
		return "Vec<" + t.Elem.String() + ">"
	case MapKind:
		return "HashMap<" + t.Key.String() + ", " + t.Val.String() + ">"
	case Never:
		return "never"
	case StructKind:
		if t.St != nil {
			return stDisplay(t.St.Name, t.St.Base, t.St.Args)
		}
		return "<struct>"
	case EnumKind:
		if t.En != nil {
			return stDisplay(t.En.Name, t.En.Base, t.En.Args)
		}
		return "<enum>"
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
		var sb strings.Builder
		sb.WriteString("(")
		for i, e := range t.Elems {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(e.String())
		}
		sb.WriteString(")")
		return sb.String()
	case Fn:
		var sb strings.Builder
		sb.WriteString("fn(")
		for i, e := range t.Params {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(e.String())
		}
		sb.WriteString(")")
		if t.Ret != nil && t.Ret.Kind != Unit {
			sb.WriteString(" -> ")
			sb.WriteString(t.Ret.String())
		}
		return sb.String()
	}
	return "<type>"
}
