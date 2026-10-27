// Package mir defines Zexi's mid-level intermediate representation: a
// per-function control-flow graph of simple statements over numbered locals.
//
// MIR is the contract between the language front end and every backend
// (docs/04 §4.3, docs/06 §6.5). It exists so that adding a backend means
// writing a translator for roughly a dozen statement and terminator forms,
// instead of re-deriving the language from an AST walk. Nothing in this
// package knows about C, registers, or the standard library's implementation
// details; a backend consumes a Program plus the type information already
// carried on locals, places, and operands.
//
// # Shape of a body
//
// A Body is a list of Locals and a list of basic blocks. Local 0 is always the
// return place (`_0`), followed by the parameters (`_1` .. `_N`), then `let`
// bindings, then compiler temporaries, then drop flags. Every local has a type
// for its whole lifetime; there is no shadowing, and a `mut` binding is a
// property of the local, not of a place.
//
// Control flow is explicit: a block is a straight-line list of statements
// followed by exactly one terminator (goto, switch, call-then-continue, return,
// or unreachable). Expressions never nest — every intermediate value is
// written to a local, so a backend sees a flat instruction stream it can walk
// once.
//
// # Places
//
// A Place names storage: a local plus a path of projections (field, element,
// dereference, enum-variant downcast). Assignments write places; reads are
// Operands (a copy, a move, a constant, or a function reference).
//
// # Ownership
//
// MIR is where ownership becomes concrete. A `Move` operand transfers
// ownership out of a place, a `Drop` statement releases it, and drop flags
// make both conditional: `SetDropFlag(false)` accompanies every move, and a
// `Drop` with a flag does nothing when the value has already been moved away.
// This is what lets a backend lower Zexi's deterministic destruction without
// path analysis of its own — the front end has already decided *where* drops
// happen, and the flag only records whether a value is still there.
package mir

import (
	"fmt"
	"strings"

	"zexi/internal/sema"
	"zexi/internal/token"
	"zexi/internal/types"
)

// LocalID identifies a local within a body.
type LocalID int32

// BlockID identifies a basic block within a body.
type BlockID int32

// Valid reports whether the id refers to something. The zero LocalID is a real
// local (the return place), so "no local" is negative.
func (l LocalID) Valid() bool { return l >= 0 }

// Valid reports whether the id refers to a block.
func (b BlockID) Valid() bool { return b >= 0 }

func (l LocalID) String() string { return fmt.Sprintf("_%d", int32(l)) }
func (b BlockID) String() string { return fmt.Sprintf("bb%d", int32(b)) }

// LocalKind records why a local exists. Backends use it to decide which locals
// need a stack slot versus a register, and diagnostics use it to name a local
// the way the programmer wrote it.
type LocalKind uint8

const (
	// LocalReturn is local `_0`, the return place.
	LocalReturn LocalKind = iota
	// LocalArg is a function parameter.
	LocalArg
	// LocalUser is a `let` binding.
	LocalUser
	// LocalTemp is a compiler-generated temporary for an intermediate value.
	LocalTemp
	// LocalDropFlag is a `bool` recording whether an owning local still holds
	// a value that must be dropped.
	LocalDropFlag
)

func (k LocalKind) String() string {
	switch k {
	case LocalReturn:
		return "return"
	case LocalArg:
		return "arg"
	case LocalUser:
		return "user"
	case LocalTemp:
		return "temp"
	case LocalDropFlag:
		return "flag"
	}
	return "?"
}

// Local is one slot in a body's local table.
type Local struct {
	// Name is the source name for `let` bindings and parameters, and empty for
	// temporaries. It is diagnostic information only: backends address locals
	// by LocalID.
	Name string
	Ty   *types.Type
	Kind LocalKind
	// Mut records `let mut` / `mut` parameters.
	Mut bool
	// Sp is where the binding was written, when there is one.
	Sp token.Span
}

// Body is a lowered function.
type Body struct {
	Locals []*Local
	Blocks []*BasicBlock
	// Args lists the parameter locals in call order (never including the
	// return place).
	Args []LocalID
	Ret  *types.Type
	Sp   token.Span
}

// BasicBlock is a straight-line statement list with one exit.
type BasicBlock struct {
	Stmts []Stmt
	Term  Terminator
}

// --- places ---------------------------------------------------------------

// Place is storage: a local plus a projection path.
type Place struct {
	Local LocalID
	Proj  []Proj
}

// Proj is one projection step. The zero value is not meaningful; use the
// constructors.
type Proj struct {
	Kind ProjKind
	// Idx is the field index for ProjField and the variant index for
	// ProjDowncast.
	Idx int
	// Name is the source field name for ProjField, for dumps.
	Name string
	// Op is the index operand for ProjIndex.
	Op Operand
	// Ty is the type of the place *after* this projection.
	Ty *types.Type
	// Checked marks a ProjIndex that needs a run-time bounds check. Lowering
	// sets it; an optimizer may clear it once it proves the index in range.
	Checked bool
}

// ProjKind enumerates projection steps.
type ProjKind uint8

const (
	// ProjField selects a struct field, tuple element, or (after a downcast)
	// an enum variant field.
	ProjField ProjKind = iota
	// ProjIndex selects an array element. Checked means the backend must
	// bounds-check and panic when out of range.
	ProjIndex
	// ProjDeref follows a reference or pointer.
	ProjDeref
	// ProjDowncast reinterprets an enum place as one of its variants, so that
	// ProjField can then reach a payload field.
	ProjDowncast
)

func (k ProjKind) String() string {
	switch k {
	case ProjField:
		return "field"
	case ProjIndex:
		return "index"
	case ProjDeref:
		return "deref"
	case ProjDowncast:
		return "downcast"
	}
	return "?"
}

// FieldProj projects a struct field by index.
func FieldProj(idx int, name string, ty *types.Type) Proj {
	return Proj{Kind: ProjField, Idx: idx, Name: name, Ty: ty}
}

// IndexProj projects an array element, bounds-checked unless checked is false.
func IndexProj(op Operand, elem *types.Type, checked bool) Proj {
	return Proj{Kind: ProjIndex, Op: op, Ty: elem, Checked: checked}
}

// DerefProj follows a reference.
func DerefProj(elem *types.Type) Proj { return Proj{Kind: ProjDeref, Ty: elem} }

// DowncastProj selects an enum variant.
func DowncastProj(variant int) Proj { return Proj{Kind: ProjDowncast, Idx: variant} }

// IsLocal reports whether the place names a whole local.
func (p Place) IsLocal() bool { return len(p.Proj) == 0 }

// Ty returns the type of the place, which is the type of its last projection
// or of the local when there are none. It returns nil for an unknown place.
func (b *Body) Ty(p Place) *types.Type {
	if int(p.Local) >= len(b.Locals) || !p.Local.Valid() {
		return nil
	}
	if len(p.Proj) == 0 {
		return b.Locals[p.Local].Ty
	}
	return p.Proj[len(p.Proj)-1].Ty
}

// String renders a place for dumps and diagnostics.
func (p Place) String() string {
	var sb strings.Builder
	sb.WriteString(p.Local.String())
	for _, pr := range p.Proj {
		switch pr.Kind {
		case ProjField:
			if pr.Name != "" {
				sb.WriteString("." + pr.Name)
			} else {
				fmt.Fprintf(&sb, ".%d", pr.Idx)
			}
		case ProjIndex:
			fmt.Fprintf(&sb, "[%s]", pr.Op.String())
		case ProjDeref:
			sb.WriteString(".(deref)")
		case ProjDowncast:
			fmt.Fprintf(&sb, " as variant#%d", pr.Idx)
		}
	}
	return sb.String()
}

// --- operands -------------------------------------------------------------

// OpKind enumerates operand forms.
type OpKind uint8

const (
	// OpCopy reads a place without taking ownership. Valid only for `Copy`
	// types; for an owning type the front end emits OpMove instead.
	OpCopy OpKind = iota
	// OpMove reads a place and takes ownership of its value.
	OpMove
	// OpConst is a literal.
	OpConst
	// OpFn is a reference to a function (a function value).
	OpFn
)

func (k OpKind) String() string {
	switch k {
	case OpCopy:
		return "copy"
	case OpMove:
		return "move"
	case OpConst:
		return "const"
	case OpFn:
		return "fn"
	}
	return "?"
}

// Operand is a value read by an rvalue, argument, or call.
type Operand struct {
	Kind OpKind
	// Place holds the source for OpCopy and OpMove.
	Place Place
	// Const holds the value for OpConst.
	Const Const
	// Fn holds the callee for OpFn.
	Fn *Fn
	// Ty is the operand's type.
	Ty *types.Type
}

// Copy reads a place copy-wise.
func Copy(p Place, ty *types.Type) Operand { return Operand{Kind: OpCopy, Place: p, Ty: ty} }

// Move reads a place move-wise.
func Move(p Place, ty *types.Type) Operand { return Operand{Kind: OpMove, Place: p, Ty: ty} }

// String renders an operand for dumps.
func (o Operand) String() string {
	switch o.Kind {
	case OpCopy:
		return "copy " + o.Place.String()
	case OpMove:
		return "move " + o.Place.String()
	case OpConst:
		return "const " + o.Const.String(o.Ty)
	case OpFn:
		if o.Fn != nil {
			return "fn " + o.Fn.Name
		}
		return "fn ?"
	}
	return "?"
}

// ConstKind enumerates literal forms.
type ConstKind uint8

const (
	// ConstInt is an integer of any width. Bits holds the two's-complement bit
	// pattern; the operand's type says how to interpret it.
	ConstInt ConstKind = iota
	ConstFloat
	ConstBool
	ConstChar
	ConstStr
	ConstUnit
)

func (k ConstKind) String() string {
	switch k {
	case ConstInt:
		return "int"
	case ConstFloat:
		return "float"
	case ConstBool:
		return "bool"
	case ConstChar:
		return "char"
	case ConstStr:
		return "str"
	case ConstUnit:
		return "unit"
	}
	return "?"
}

// Const is a literal value.
type Const struct {
	Kind ConstKind
	// Bits holds integers, chars, and bools (0 or 1).
	Bits uint64
	// F holds floats.
	F float64
	// S holds `str` literals (already unescaped).
	S string
}

// IntConst builds a signed integer constant.
func IntConst(v int64) Const { return Const{Kind: ConstInt, Bits: uint64(v)} }

// UintConst builds an unsigned integer constant.
func UintConst(v uint64) Const { return Const{Kind: ConstInt, Bits: v} }

// BoolConst builds a bool constant.
func BoolConst(v bool) Const {
	if v {
		return Const{Kind: ConstBool, Bits: 1}
	}
	return Const{Kind: ConstBool}
}

// CharConst builds a char constant.
func CharConst(r rune) Const { return Const{Kind: ConstChar, Bits: uint64(r)} }

// StrConst builds a `str` constant.
func StrConst(s string) Const { return Const{Kind: ConstStr, S: s} }

// UnitConst is the unit value.
func UnitConst() Const { return Const{Kind: ConstUnit} }

// AsInt interprets the constant as a signed integer.
func (c Const) AsInt() int64 { return int64(c.Bits) }

// AsUint interprets the constant as an unsigned integer.
func (c Const) AsUint() uint64 { return c.Bits }

// String renders a constant using the operand type for width and signedness.
func (c Const) String(ty *types.Type) string {
	switch c.Kind {
	case ConstInt:
		switch {
		case ty != nil && ty.Kind.IsSigned():
			return fmt.Sprintf("%d", int64(c.Bits))
		case ty != nil && ty.Kind.IsUnsigned():
			return fmt.Sprintf("%d", c.Bits)
		default:
			return fmt.Sprintf("%d", int64(c.Bits))
		}
	case ConstFloat:
		return fmt.Sprintf("%v", c.F)
	case ConstBool:
		return fmt.Sprintf("%v", c.Bits != 0)
	case ConstChar:
		return fmt.Sprintf("'%c'", rune(c.Bits))
	case ConstStr:
		return fmt.Sprintf("%q", c.S)
	case ConstUnit:
		return "()"
	}
	return "?"
}

// --- rvalues --------------------------------------------------------------

// Rvalue computes a value written by an Assign statement.
type Rvalue interface {
	rvalue()
}

// Use copies or moves an operand, which is how a place, constant, or function
// reference becomes the right-hand side of an assignment.
type Use struct{ Op Operand }

func (Use) rvalue() {}

// BinOp applies a binary operator. Op is a token.Kind such as KindPlus or
// KindLt; the operand types determine width, signedness, and float-vs-int.
type BinOp struct {
	Op   token.Kind
	L, R Operand
	Ty   *types.Type
}

func (*BinOp) rvalue() {}

// UnOp applies a prefix operator: KindMinus or KindBang.
type UnOp struct {
	Op token.Kind
	X  Operand
	Ty *types.Type
}

func (*UnOp) rvalue() {}

// CastKind distinguishes the cast families a backend must lower differently.
type CastKind uint8

const (
	// CastInt is an integer-to-integer conversion (truncate or extend).
	CastInt CastKind = iota
	// CastFloat is a float-to-float conversion.
	CastFloat
	// CastIntToFloat widens an integer into a float.
	CastIntToFloat
	// CastFloatToInt truncates a float toward zero.
	CastFloatToInt
	// CastBoolInt converts between bool and an integer.
	CastBoolInt
	// CastCharInt converts between char and an integer.
	CastCharInt
	// CastPtr is a pointer/reference-to-pointer conversion.
	CastPtr
	// CastString borrows an owned `String` as `str`.
	CastString
	// CastNoop is a cast between identical types; the language allows it but a
	// backend can drop it.
	CastNoop
)

func (k CastKind) String() string {
	switch k {
	case CastInt:
		return "int"
	case CastFloat:
		return "float"
	case CastIntToFloat:
		return "int->float"
	case CastFloatToInt:
		return "float->int"
	case CastBoolInt:
		return "bool<->int"
	case CastCharInt:
		return "char<->int"
	case CastPtr:
		return "ptr"
	case CastString:
		return "String->str"
	case CastNoop:
		return "noop"
	}
	return "?"
}

// Cast converts an operand to another type.
type Cast struct {
	Kind CastKind
	X    Operand
	From *types.Type
	To   *types.Type
}

func (*Cast) rvalue() {}

// AggKind enumerates aggregate constructions.
type AggKind uint8

const (
	// AggStruct builds a struct value (tuple structs included) from field
	// values in declaration order.
	AggStruct AggKind = iota
	// AggEnum builds an enum value of Variant from payload values.
	AggEnum
	// AggArray builds an array value from element values.
	AggArray
)

func (k AggKind) String() string {
	switch k {
	case AggStruct:
		return "struct"
	case AggEnum:
		return "enum"
	case AggArray:
		return "array"
	}
	return "?"
}

// Aggregate builds a struct, enum variant, or array from operands.
type Aggregate struct {
	Kind AggKind
	// Variant is the variant index for AggEnum.
	Variant int
	Ops     []Operand
	Ty      *types.Type
}

func (*Aggregate) rvalue() {}

// Ref takes a reference to a place. Taking a reference never moves.
type Ref struct {
	Mut   bool
	Place Place
	Ty    *types.Type
}

func (*Ref) rvalue() {}

// Discriminant reads an enum's tag as an integer.
type Discriminant struct {
	Place Place
	Ty    *types.Type
}

func (*Discriminant) rvalue() {}

// Len reads the length of an array, `str`, `Vec`, or `HashMap`.
type Len struct {
	Place Place
	Ty    *types.Type
}

func (*Len) rvalue() {}

// Repeat builds `[value; count]`.
type Repeat struct {
	Op    Operand
	Count uint64
	Ty    *types.Type
}

func (*Repeat) rvalue() {}

// --- statements -----------------------------------------------------------

// Stmt is a statement: something that happens without transferring control.
type Stmt interface {
	stmt()
	// Span returns the source range that produced the statement.
	Span() token.Span
}

// Assign writes an rvalue into a place.
type Assign struct {
	Place Place
	RV    Rvalue
	Sp    token.Span
}

func (*Assign) stmt()              {}
func (s *Assign) Span() token.Span { return s.Sp }

// Drop releases the value in a place. When HasFlag is set, the drop is
// conditional on the named drop flag still being true, which is how a value
// that was moved away on some paths is not freed twice.
type Drop struct {
	Place   Place
	Flag    LocalID
	HasFlag bool
	Sp      token.Span
}

func (*Drop) stmt()              {}
func (s *Drop) Span() token.Span { return s.Sp }

// SetDropFlag records that a value is present (val true, at initialization) or
// gone (val false, after a move).
type SetDropFlag struct {
	Flag LocalID
	Val  bool
	Sp   token.Span
}

func (*SetDropFlag) stmt()              {}
func (s *SetDropFlag) Span() token.Span { return s.Sp }

// --- terminators ----------------------------------------------------------

// Terminator ends a basic block. Exactly one terminator per block.
type Terminator interface {
	terminator()
	// Span returns the source range that produced the terminator.
	Span() token.Span
}

// Goto jumps unconditionally.
type Goto struct {
	Target BlockID
	Sp     token.Span
}

func (*Goto) terminator()        {}
func (t *Goto) Span() token.Span { return t.Sp }

// Case is one value of a SwitchInt.
type Case struct {
	Val    uint64
	Target BlockID
}

// SwitchInt branches on an integer discriminant, which is how `match`, `if`,
// and short-circuit operators become CFG edges.
type SwitchInt struct {
	Discr     Operand
	Cases     []Case
	Otherwise BlockID
	Sp        token.Span
}

func (*SwitchInt) terminator()        {}
func (t *SwitchInt) Span() token.Span { return t.Sp }

// Call invokes a function or runtime intrinsic and continues at Target with the
// result in Dest. Dest is nil for calls that produce no value.
type Call struct {
	Callee FuncRef
	Args   []Operand
	Dest   *Place
	Target BlockID
	Sp     token.Span
}

func (*Call) terminator()        {}
func (t *Call) Span() token.Span { return t.Sp }

// Return leaves the function with the value in local `_0`.
type Return struct{ Sp token.Span }

func (*Return) terminator()        {}
func (t *Return) Span() token.Span { return t.Sp }

// Unreachable marks a block that control cannot reach, such as the tail of a
// block that always returns. It exists so every block has a terminator and the
// IR stays structurally total.
type Unreachable struct{ Sp token.Span }

func (*Unreachable) terminator()        {}
func (t *Unreachable) Span() token.Span { return t.Sp }

// --- functions and programs ----------------------------------------------

// FuncRef names what a Call invokes: a lowered Zexi function, an `extern "C"`
// symbol, or a runtime intrinsic.
type FuncRef struct {
	// Fn is set for a Zexi function in this program.
	Fn *Fn
	// Extern is the C symbol for an `extern "C"` declaration.
	Extern string
	// Builtin names a runtime intrinsic (`println`, `panic`, ...).
	Builtin string
}

// String renders the callee for dumps.
func (f FuncRef) String() string {
	switch {
	case f.Fn != nil:
		return f.Fn.Name
	case f.Extern != "":
		return "extern " + f.Extern
	case f.Builtin != "":
		return "builtin " + f.Builtin
	}
	return "?"
}

// IsBuiltin reports whether the callee is a runtime intrinsic.
func (f FuncRef) IsBuiltin() bool { return f.Builtin != "" }

// Builtin intrinsic names the IR can call. They are strings rather than an enum
// so that adding one (a `String` method, a `Vec` operation) does not change the
// IR's shape; each backend decides what it supports and reports the rest.
const (
	// BnPrint writes its arguments to standard output with no trailing
	// newline; BnPrintln does the same and then writes one.
	BnPrint   = "print"
	BnPrintln = "println"
	// BnPanic aborts with a message.
	BnPanic = "panic"
)

// Fn is one lowered function.
type Fn struct {
	// Name is the unique IR name (`main`, `Point::len_sq`, `id$i64`).
	Name string
	// Qual is the source-qualified name for diagnostics.
	Qual string
	// Body is nil for `extern "C"` declarations, which have no Zexi code.
	Body *Body
	// Extern is the C symbol for an extern declaration.
	Extern string
	// Src is the checked declaration this function was lowered from.
	Src *sema.Fn
}

// Signature renders the function's parameter and return types.
func (f *Fn) Signature() string {
	params := ""
	if f.Body != nil {
		parts := make([]string, 0, len(f.Body.Args))
		for _, a := range f.Body.Args {
			parts = append(parts, f.Body.Locals[a].Ty.String())
		}
		params = strings.Join(parts, ", ")
	}
	name := f.Qual
	if name == "" {
		name = f.Name
	}
	if f.Body == nil {
		return fmt.Sprintf("%s(%s) [extern]", name, params)
	}
	return fmt.Sprintf("%s(%s) -> %s", name, params, f.Body.Ret)
}

// Program is a whole lowered program.
type Program struct {
	// Fns holds every reachable function in declaration order.
	Fns []*Fn
	// Structs and Enums are the nominal types the program defines or uses, in
	// a deterministic order, so a backend can emit layout definitions once.
	Structs []*types.Struct
	Enums   []*types.Enum
	// Main is the entry point, or nil when the module has none.
	Main *Fn
}

// FnByName finds a lowered function by IR name.
func (p *Program) FnByName(name string) *Fn {
	for _, f := range p.Fns {
		if f.Name == name {
			return f
		}
	}
	return nil
}
