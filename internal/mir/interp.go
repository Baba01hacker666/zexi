package mir

// The reference interpreter executes MIR directly.
//
// It exists for one reason: to check the lowering. A control-flow graph can be
// structurally valid (see Validate) and still compute the wrong thing — a
// swapped branch, an off-by-one in a loop increment, a move that reads the wrong
// local. Running the same program through this interpreter and through the real
// C backend and comparing their output catches exactly those mistakes, which is
// the "backend conformance" idea in docs/04 §4.11 applied before there is a
// second real backend.
//
// It is deliberately *not* an execution mode of the toolchain: Zexi's stated
// non-goals are no interpreter and no VM (docs/01 §1.2), and this does not
// change that. It lives in internal/mir, is used by tests only, and covers the
// pure-computation subset the C backend and this interpreter both understand.

import (
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"zexi/internal/token"
	"zexi/internal/types"
)

// Interp evaluates a program. Output written by `print`/`println` goes to Out.
type Interp struct {
	Prog *Program
	Out  io.Writer
	// StepLimit bounds execution so a lowering bug that produces an infinite
	// loop fails the test instead of hanging it.
	StepLimit int
	steps     int
	depth     int
}

// UnsupportedError reports a construct the interpreter does not model. It is
// distinct from a program bug: tests can skip constructs the oracle does not
// model instead of mistaking that limitation for a lowering failure.
type UnsupportedError struct{ What string }

func (e *UnsupportedError) Error() string { return "interp: unsupported: " + e.What }

// ErrExhausted reports that StepLimit was reached.
var ErrExhausted = fmt.Errorf("interp: step limit reached")

type valueKind uint8

const (
	vUnit valueKind = iota
	vInt
	vFloat
	vBool
	vStr
	vString
	vStruct
	vEnum
	vArray
	vRef
	vFn
)

// Value is an interpreter value. Aggregates hold their fields inline, which
// matches Zexi's value semantics: assigning an aggregate copies it.
type Value struct {
	kind   valueKind
	i      uint64
	f      float64
	b      bool
	s      string
	fields []Value // struct fields, enum payload
	elems  []Value // array elements
	tag    int     // enum variant index
	ref    *reference
	fn     *Fn
}

// A reference points to the resolved slot, not the expression that selected
// it. In particular, an array index is evaluated when the borrow is created.
type reference struct {
	target *Value
}

type frame struct {
	locals []Value
	fn     *Fn
}

func unitValue() Value { return Value{kind: vUnit} }

func intValue(v uint64) Value { return Value{kind: vInt, i: v} }

func floatValue(v float64, ty *types.Type) Value {
	if ty != nil && ty.Kind == types.F32 {
		v = float64(float32(v))
	}
	return Value{kind: vFloat, f: v}
}

// A typed MIR destination stores a value at its declared width. This matters
// for intermediate i8/u8 arithmetic and f32 rounding, not only explicit casts.
func normalizeValue(v Value, ty *types.Type) Value {
	if ty == nil {
		return v
	}
	switch {
	case v.kind == vInt && (ty.Kind.IsInt() || ty.Kind == types.Char):
		v.i = truncate(v.i, ty)
	case v.kind == vFloat && ty.Kind == types.F32:
		v.f = float64(float32(v.f))
	}
	return v
}

// boolValue keeps the integer bit pattern in step with the boolean, because a
// SwitchInt reads the integer field: `if c { .. }` branches on 1 and 0.
func boolValue(b bool) Value {
	v := Value{kind: vBool, b: b}
	if b {
		v.i = 1
	}
	return v
}

// copyValue implements value semantics: aggregates are duplicated, scalars are
// already values, and references are shared (they name storage).
func copyValue(v Value) Value {
	switch v.kind {
	case vStruct, vEnum:
		out := v
		out.fields = make([]Value, len(v.fields))
		for i := range v.fields {
			out.fields[i] = copyValue(v.fields[i])
		}
		return out
	case vArray:
		out := v
		out.elems = make([]Value, len(v.elems))
		for i := range v.elems {
			out.elems[i] = copyValue(v.elems[i])
		}
		return out
	}
	return v
}

// RunMain executes `main` and returns its exit code.
func (in *Interp) RunMain() (int64, error) {
	if in.Prog.Main == nil {
		return 0, fmt.Errorf("interp: program has no main")
	}
	if in.Out == nil {
		in.Out = io.Discard
	}
	if in.StepLimit == 0 {
		in.StepLimit = 20_000_000
	}
	v, err := in.call(in.Prog.Main, nil)
	if err != nil {
		return 0, err
	}
	if v.kind == vInt {
		return int64(v.i), nil
	}
	return 0, nil
}

func (in *Interp) call(fn *Fn, args []Value) (Value, error) {
	if fn.Body == nil {
		return Value{}, &UnsupportedError{What: "extern function " + fn.Name}
	}
	if in.depth++; in.depth > 10_000 {
		return Value{}, fmt.Errorf("interp: recursion too deep (stack overflow in the real program)")
	}
	defer func() { in.depth-- }()

	fr := &frame{locals: make([]Value, len(fn.Body.Locals)), fn: fn}
	for i, id := range fn.Body.Args {
		if i >= len(args) {
			break
		}
		fr.locals[id] = normalizeValue(args[i], fn.Body.Locals[id].Ty)
	}
	cur := BlockID(0)
	for {
		if in.steps++; in.steps > in.StepLimit {
			return Value{}, ErrExhausted
		}
		if int(cur) >= len(fn.Body.Blocks) {
			return Value{}, fmt.Errorf("interp: %s: block %s out of range", fn.Name, cur)
		}
		blk := fn.Body.Blocks[cur]
		for _, s := range blk.Stmts {
			if err := in.execStmt(fr, s); err != nil {
				return Value{}, err
			}
		}
		switch t := blk.Term.(type) {
		case *Goto:
			cur = t.Target
		case *SwitchInt:
			v, err := in.operand(fr, t.Discr)
			if err != nil {
				return Value{}, err
			}
			cur = t.Otherwise
			for _, c := range t.Cases {
				if v.i == c.Val {
					cur = c.Target
					break
				}
			}
		case *Call:
			var args []Value
			for _, a := range t.Args {
				v, err := in.operand(fr, a)
				if err != nil {
					return Value{}, err
				}
				args = append(args, v)
			}
			res, err := in.invoke(t.Callee, args, t.Args)
			if err != nil {
				return Value{}, err
			}
			if t.Dest != nil {
				slot, err := in.place(fr, *t.Dest)
				if err != nil {
					return Value{}, err
				}
				*slot = normalizeValue(copyValue(res), fr.fn.Body.Ty(*t.Dest))
			}
			cur = t.Target
		case *Return:
			return copyValue(fr.locals[0]), nil
		case *Unreachable:
			return Value{}, fmt.Errorf("interp: %s: reached unreachable block (the pattern was not exhaustive?)", fn.Name)
		default:
			return Value{}, fmt.Errorf("interp: %s: unknown terminator %T", fn.Name, t)
		}
	}
}

func (in *Interp) invoke(ref FuncRef, args []Value, ops []Operand) (Value, error) {
	switch {
	case ref.Fn != nil:
		return in.call(ref.Fn, args)
	case ref.Builtin == BnPrint, ref.Builtin == BnPrintln:
		for i, a := range args {
			if i > 0 {
				fmt.Fprint(in.Out, " ")
			}
			text, err := printable(a, ops[i].Ty)
			if err != nil {
				return Value{}, err
			}
			fmt.Fprint(in.Out, text)
		}
		if ref.Builtin == BnPrintln {
			fmt.Fprintln(in.Out)
		}
		return unitValue(), nil
	case ref.Builtin == BnPanic:
		msg := "panic"
		if len(args) > 0 {
			var err error
			msg, err = printable(args[0], ops[0].Ty)
			if err != nil {
				return Value{}, err
			}
		}
		return Value{}, fmt.Errorf("interp: panic: %s", msg)
	}
	return stringBuiltin(ref.Builtin, args)
}

// printable mirrors the C runtime's printing of the scalar and string types.
// String arguments to print are borrowed, so follow their reference first.
func printable(v Value, ty *types.Type) (string, error) {
	for v.kind == vRef {
		if v.ref == nil || v.ref.target == nil {
			return "", fmt.Errorf("interp: invalid printable reference")
		}
		v = *v.ref.target
		if ty != nil && (ty.Kind == types.Ref || ty.Kind == types.Ptr) {
			ty = ty.Elem
		}
	}
	if ty != nil {
		switch {
		case ty.Kind == types.Char:
			return encodeChar(uint32(v.i)), nil
		case ty.Kind.IsSigned():
			return strconv.FormatInt(int64(truncate(v.i, ty)), 10), nil
		case ty.Kind.IsUnsigned():
			return strconv.FormatUint(truncate(v.i, ty), 10), nil
		case ty.Kind.IsFloat():
			return formatFloat(floatValue(v.f, ty).f), nil
		}
	}
	if v.kind == vStr || v.kind == vString {
		return cString(v.s), nil
	}
	return formatValue(v), nil
}

// cString models the current C runtime's NUL-terminated str representation.
func cString(s string) string {
	if i := strings.IndexByte(s, 0); i >= 0 {
		return s[:i]
	}
	return s
}

// formatFloat is the runtime's snprintf("%g", value), whose default precision
// is six significant digits. Go's default %g uses a different precision.
func formatFloat(f float64) string {
	if math.IsNaN(f) {
		if math.Signbit(f) {
			return "-nan"
		}
		return "nan"
	}
	if math.IsInf(f, 1) {
		return "inf"
	}
	if math.IsInf(f, -1) {
		return "-inf"
	}
	return strconv.FormatFloat(f, 'g', 6, 64)
}

// encodeChar matches zx_utf8_encode, including the C backend's handling of
// integer-to-char casts outside the Unicode scalar range.
func encodeChar(c uint32) string {
	switch {
	case c < 0x80:
		return string([]byte{byte(c)})
	case c < 0x800:
		return string([]byte{byte(0xc0 | (c >> 6)), byte(0x80 | (c & 0x3f))})
	case c < 0x10000:
		return string([]byte{byte(0xe0 | (c >> 12)), byte(0x80 | ((c >> 6) & 0x3f)), byte(0x80 | (c & 0x3f))})
	default:
		return string([]byte{byte(0xf0 | (c >> 18)), byte(0x80 | ((c >> 12) & 0x3f)), byte(0x80 | ((c >> 6) & 0x3f)), byte(0x80 | (c & 0x3f))})
	}
}

// stringBuiltin models just the String runtime operations that the interpreter
// understands. Strings themselves are Go values; a reference mutates its
// target slot. No allocation or destructor behavior is claimed by the oracle.
func stringBuiltin(name string, args []Value) (Value, error) {
	arity := -1
	switch name {
	case BnStringNew:
		arity = 0
	case "String::from", "String::with_capacity", BnStringAsStr,
		"String::len", "String::is_empty", "String::clone", "String::clear":
		arity = 1
	case BnStringPushStr, BnStringPush, BnStringPushI64, BnStringPushU64, BnStringPushF64:
		arity = 2
	default:
		return Value{}, &UnsupportedError{What: "call to " + name}
	}
	if len(args) != arity {
		return Value{}, fmt.Errorf("interp: %s expects %d arguments, got %d", name, arity, len(args))
	}
	switch name {
	case BnStringNew, "String::with_capacity":
		return Value{kind: vString}, nil
	case "String::from":
		if args[0].kind != vStr {
			return Value{}, fmt.Errorf("interp: String::from requires a str")
		}
		return Value{kind: vString, s: cString(args[0].s)}, nil
	}
	// Source methods can have a reference-to-reference receiver; peel all
	// references, retaining a pointer to the owning String slot.
	recv := &args[0]
	for recv.kind == vRef && recv.ref != nil && recv.ref.target != nil {
		recv = recv.ref.target
	}
	if recv.kind != vString {
		return Value{}, fmt.Errorf("interp: %s requires a String receiver", name)
	}
	switch name {
	case BnStringAsStr:
		return Value{kind: vStr, s: cString(recv.s)}, nil
	case "String::len":
		return intValue(uint64(len(recv.s))), nil
	case "String::is_empty":
		return boolValue(len(recv.s) == 0), nil
	case "String::clone":
		return Value{kind: vString, s: recv.s}, nil
	case "String::clear":
		recv.s = ""
	case BnStringPushStr:
		if args[1].kind != vStr {
			return Value{}, fmt.Errorf("interp: String::push_str requires a str")
		}
		recv.s += cString(args[1].s)
	case BnStringPush:
		recv.s += encodeChar(uint32(args[1].i))
	case BnStringPushI64:
		recv.s += strconv.FormatInt(int64(args[1].i), 10)
	case BnStringPushU64:
		recv.s += strconv.FormatUint(args[1].i, 10)
	case BnStringPushF64:
		recv.s += formatFloat(args[1].f)
	}
	return unitValue(), nil
}

func formatValue(v Value) string {
	switch v.kind {
	case vUnit:
		return "()"
	case vInt:
		return strconv.FormatInt(int64(v.i), 10)
	case vFloat:
		return strconv.FormatFloat(v.f, 'g', -1, 64)
	case vBool:
		if v.b {
			return "true"
		}
		return "false"
	case vStr, vString:
		return v.s
	}
	return "<aggregate>"
}

func (in *Interp) execStmt(fr *frame, s Stmt) error {
	switch st := s.(type) {
	case *Assign:
		v, err := in.rvalue(fr, st.RV)
		if err != nil {
			return err
		}
		slot, err := in.place(fr, st.Place)
		if err != nil {
			return err
		}
		*slot = normalizeValue(copyValue(v), fr.fn.Body.Ty(st.Place))
	case *Drop:
		// Nothing to release: the interpreter owns no heap.
		if st.HasFlag {
			if _, err := in.place(fr, Place{Local: st.Flag}); err != nil {
				return err
			}
		}
	case *SetDropFlag:
		slot, err := in.place(fr, Place{Local: st.Flag})
		if err != nil {
			return err
		}
		*slot = boolValue(st.Val)
	default:
		return fmt.Errorf("interp: unknown statement %T", s)
	}
	return nil
}

func (in *Interp) rvalue(fr *frame, r Rvalue) (Value, error) {
	switch rv := r.(type) {
	case Use:
		return in.operand(fr, rv.Op)
	case *BinOp:
		l, err := in.operand(fr, rv.L)
		if err != nil {
			return Value{}, err
		}
		r, err := in.operand(fr, rv.R)
		if err != nil {
			return Value{}, err
		}
		return binop(rv.Op, l, r, rv.Ty, rv.L.Ty)
	case *UnOp:
		x, err := in.operand(fr, rv.X)
		if err != nil {
			return Value{}, err
		}
		switch rv.Op {
		case token.KindMinus:
			if rv.Ty != nil && rv.Ty.Kind.IsFloat() {
				return floatValue(-x.f, rv.Ty), nil
			}
			return intValue(-x.i), nil
		case token.KindBang:
			if rv.Ty != nil && rv.Ty.Kind == types.Bool {
				return boolValue(!x.b), nil
			}
			// Zexi's `!` is logical negation, as in C: `!0 == 1`, `!5 == 0`.
			if x.i == 0 {
				return intValue(1), nil
			}
			return intValue(0), nil
		}
		return Value{}, &UnsupportedError{What: "unary " + rv.Op.String()}
	case *Cast:
		x, err := in.operand(fr, rv.X)
		if err != nil {
			return Value{}, err
		}
		return castValue(rv, x)
	case *Aggregate:
		var vals []Value
		for _, o := range rv.Ops {
			v, err := in.operand(fr, o)
			if err != nil {
				return Value{}, err
			}
			vals = append(vals, copyValue(v))
		}
		switch rv.Kind {
		case AggStruct:
			return Value{kind: vStruct, fields: vals}, nil
		case AggEnum:
			return Value{kind: vEnum, tag: rv.Variant, fields: vals}, nil
		case AggArray:
			return Value{kind: vArray, elems: vals}, nil
		}
		return Value{}, fmt.Errorf("interp: unknown aggregate kind")
	case *Ref:
		slot, err := in.place(fr, rv.Place)
		if err != nil {
			return Value{}, err
		}
		return Value{kind: vRef, ref: &reference{target: slot}}, nil
	case *Discriminant:
		v, err := in.place(fr, rv.Place)
		if err != nil {
			return Value{}, err
		}
		if v.kind != vEnum {
			return Value{}, fmt.Errorf("interp: discriminant of a non-enum")
		}
		return intValue(uint64(v.tag)), nil
	case *Len:
		v, err := in.place(fr, rv.Place)
		if err != nil {
			return Value{}, err
		}
		switch v.kind {
		case vArray:
			return intValue(uint64(len(v.elems))), nil
		case vStr:
			return intValue(uint64(len(cString(v.s)))), nil
		case vString:
			return intValue(uint64(len(v.s))), nil
		}
		return Value{}, &UnsupportedError{What: "len of a non-array"}
	case *Repeat:
		v, err := in.operand(fr, rv.Op)
		if err != nil {
			return Value{}, err
		}
		elems := make([]Value, rv.Count)
		for i := range elems {
			elems[i] = copyValue(v)
		}
		return Value{kind: vArray, elems: elems}, nil
	}
	return Value{}, fmt.Errorf("interp: unknown rvalue %T", r)
}

func (in *Interp) operand(fr *frame, o Operand) (Value, error) {
	switch o.Kind {
	case OpConst:
		return constValue(o), nil
	case OpCopy, OpMove:
		v, err := in.place(fr, o.Place)
		if err != nil {
			return Value{}, err
		}
		if o.Kind == OpMove {
			return *v, nil
		}
		return copyValue(*v), nil
	case OpFn:
		return Value{kind: vFn, fn: o.Fn}, nil
	}
	return Value{}, fmt.Errorf("interp: unknown operand kind")
}

func constValue(o Operand) Value {
	switch o.Const.Kind {
	case ConstInt:
		return intValue(o.Const.Bits)
	case ConstFloat:
		return floatValue(o.Const.F, o.Ty)
	case ConstBool:
		return boolValue(o.Const.Bits != 0)
	case ConstChar:
		return intValue(o.Const.Bits)
	case ConstStr:
		return Value{kind: vStr, s: o.Const.S}
	}
	return unitValue()
}

// place resolves a place to the storage it names, following references.
func (in *Interp) place(fr *frame, p Place) (*Value, error) {
	if !p.Local.Valid() || int(p.Local) >= len(fr.locals) {
		return nil, fmt.Errorf("interp: local %s out of range in %s", p.Local, fr.fn.Name)
	}
	idx := make([]uint64, len(p.Proj))
	for i, pr := range p.Proj {
		if pr.Kind != ProjIndex {
			continue
		}
		v, err := in.operand(fr, pr.Op)
		if err != nil {
			return nil, err
		}
		idx[i] = v.i
	}
	return resolveProj(&fr.locals[p.Local], p.Proj, idx)
}

func resolveProj(v *Value, projs []Proj, idx []uint64) (*Value, error) {
	for i, pr := range projs {
		switch pr.Kind {
		case ProjDeref:
			if v.kind != vRef || v.ref == nil || v.ref.target == nil {
				return nil, fmt.Errorf("interp: deref of a non-reference")
			}
			v = v.ref.target
		case ProjField:
			if v.kind != vStruct && v.kind != vEnum {
				return nil, fmt.Errorf("interp: field access on a non-aggregate")
			}
			if pr.Idx < 0 || pr.Idx >= len(v.fields) {
				return nil, fmt.Errorf("interp: field index %d out of range", pr.Idx)
			}
			v = &v.fields[pr.Idx]
		case ProjIndex:
			if v.kind != vArray {
				return nil, fmt.Errorf("interp: index into a non-array")
			}
			n := idx[i]
			if n >= uint64(len(v.elems)) {
				return nil, fmt.Errorf("interp: index %d out of bounds (len %d)", n, len(v.elems))
			}
			v = &v.elems[n]
		case ProjDowncast:
			// The tag was checked by the switch that led here.
		default:
			return nil, fmt.Errorf("interp: unknown projection")
		}
	}
	return v, nil
}

func binop(op token.Kind, l, r Value, resTy, opTy *types.Type) (Value, error) {
	isFloat := (opTy != nil && opTy.Kind.IsFloat()) || l.kind == vFloat
	if l.kind == vBool || (opTy != nil && opTy.Kind == types.Bool) {
		// `&&`/`||` on already-evaluated bools.
		switch op {
		case token.KindAmpAmp:
			return boolValue(l.b && r.b), nil
		case token.KindPipePipe:
			return boolValue(l.b || r.b), nil
		case token.KindEqEq:
			return boolValue(l.b == r.b), nil
		case token.KindBangEq:
			return boolValue(l.b != r.b), nil
		}
	}
	if isFloat {
		a, b := l.f, r.f
		switch op {
		case token.KindPlus:
			return floatValue(a+b, resTy), nil
		case token.KindMinus:
			return floatValue(a-b, resTy), nil
		case token.KindStar:
			return floatValue(a*b, resTy), nil
		case token.KindSlash:
			if b == 0 {
				return Value{}, fmt.Errorf("interp: float division by zero")
			}
			return floatValue(a/b, resTy), nil
		case token.KindLt:
			return boolValue(a < b), nil
		case token.KindLe:
			return boolValue(a <= b), nil
		case token.KindGt:
			return boolValue(a > b), nil
		case token.KindGe:
			return boolValue(a >= b), nil
		case token.KindEqEq:
			return boolValue(a == b), nil
		case token.KindBangEq:
			return boolValue(a != b), nil
		}
		return Value{}, &UnsupportedError{What: "float operator " + op.String()}
	}
	// Integers are stored at their declared width when assigned. Signedness
	// determines comparison, division and right-shift semantics.
	signed := opTy == nil || opTy.Kind.IsSigned() || opTy.Kind == types.Char
	if opTy != nil && opTy.Kind == types.Char {
		signed = false
	}
	a, b := l.i, r.i
	sa, sb := int64(a), int64(b)
	switch op {
	case token.KindPlus:
		return intValue(a + b), nil
	case token.KindMinus:
		return intValue(a - b), nil
	case token.KindStar:
		return intValue(a * b), nil
	case token.KindSlash:
		if b == 0 {
			return Value{}, fmt.Errorf("interp: division by zero")
		}
		if signed {
			return intValue(uint64(sa / sb)), nil
		}
		return intValue(a / b), nil
	case token.KindPercent:
		if b == 0 {
			return Value{}, fmt.Errorf("interp: modulo by zero")
		}
		if signed {
			return intValue(uint64(sa % sb)), nil
		}
		return intValue(a % b), nil
	case token.KindAmp:
		return intValue(a & b), nil
	case token.KindPipe:
		return intValue(a | b), nil
	case token.KindCaret:
		return intValue(a ^ b), nil
	case token.KindShl:
		return intValue(a << (b & 63)), nil
	case token.KindShr:
		if signed {
			return intValue(uint64(sa >> (b & 63))), nil
		}
		return intValue(a >> (b & 63)), nil
	case token.KindEqEq:
		return boolValue(a == b), nil
	case token.KindBangEq:
		return boolValue(a != b), nil
	case token.KindLt:
		if signed {
			return boolValue(sa < sb), nil
		}
		return boolValue(a < b), nil
	case token.KindLe:
		if signed {
			return boolValue(sa <= sb), nil
		}
		return boolValue(a <= b), nil
	case token.KindGt:
		if signed {
			return boolValue(sa > sb), nil
		}
		return boolValue(a > b), nil
	case token.KindGe:
		if signed {
			return boolValue(sa >= sb), nil
		}
		return boolValue(a >= b), nil
	}
	return Value{}, &UnsupportedError{What: "operator " + op.String()}
}

func castValue(c *Cast, x Value) (Value, error) {
	switch c.Kind {
	case CastInt:
		// Normalize the source first, so widening a signed narrow value
		// sign-extends instead of treating its bits as an unsigned number.
		return intValue(truncate(truncate(x.i, c.From), c.To)), nil
	case CastFloat:
		return floatValue(floatValue(x.f, c.From).f, c.To), nil
	case CastIntToFloat:
		if c.From != nil && c.From.Kind.IsSigned() {
			return floatValue(float64(int64(truncate(x.i, c.From))), c.To), nil
		}
		return floatValue(float64(truncate(x.i, c.From)), c.To), nil
	case CastFloatToInt:
		return intValue(uint64(int64(x.f))), nil
	case CastBoolInt:
		if x.kind == vBool {
			if x.b {
				return intValue(1), nil
			}
			return intValue(0), nil
		}
		return boolValue(x.i != 0), nil
	case CastCharInt:
		if c.To != nil && c.To.Kind == types.Char {
			return intValue(x.i), nil
		}
		return intValue(truncate(x.i, c.To)), nil
	case CastNoop, CastPtr, CastString:
		return x, nil
	}
	return Value{}, &UnsupportedError{What: "cast " + c.Kind.String()}
}

// truncate narrows an integer to the width of t, which is what makes
// `let x: u8 = 300 as u8` behave like the C backend.
func truncate(v uint64, t *types.Type) uint64 {
	if t == nil {
		return v
	}
	switch t.Kind {
	case types.I8:
		return uint64(int64(int8(v)))
	case types.U8:
		return uint64(uint8(v))
	case types.I16:
		return uint64(int64(int16(v)))
	case types.U16:
		return uint64(uint16(v))
	case types.I32:
		return uint64(int64(int32(v)))
	case types.U32, types.Char:
		return uint64(uint32(v))
	case types.Bool:
		if v != 0 {
			return 1
		}
		return 0
	}
	return v
}

// Supported reports whether every function in the program uses only the parts of
// the language the interpreter models, so differential tests can skip a program
// instead of failing it for the wrong reason.
func (in *Interp) Supported() (bool, string) {
	for _, fn := range in.Prog.Fns {
		if fn.Body == nil {
			continue
		}
		for _, blk := range fn.Body.Blocks {
			if c, ok := blk.Term.(*Call); ok && c.Callee.Builtin != "" {
				switch c.Callee.Builtin {
				case BnPrint, BnPrintln, BnPanic,
					BnStringNew, "String::from", "String::with_capacity", BnStringAsStr,
					"String::len", "String::is_empty", "String::clone", "String::clear",
					BnStringPushStr, BnStringPush, BnStringPushI64, BnStringPushU64, BnStringPushF64:
				default:
					return false, "builtin " + c.Callee.Builtin
				}
			}
			if c, ok := blk.Term.(*Call); ok && c.Callee.Extern != "" {
				return false, "extern " + c.Callee.Extern
			}
			for _, s := range blk.Stmts {
				a, ok := s.(*Assign)
				if !ok {
					continue
				}
				if b, ok := a.RV.(*BinOp); ok {
					if t := b.L.Ty; t != nil && (t.Kind == types.StringKind || t.Kind == types.VecKind || t.Kind == types.MapKind) {
						return false, "operator on " + t.String()
					}
				}
			}
		}
	}
	return true, ""
}

// Format renders a value for diagnostics (not for program output).
func (v Value) Format() string { return formatValue(v) }
