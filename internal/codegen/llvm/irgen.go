// irgen.go emits one MIR function body as LLVM IR: one alloca per local in
// the entry block (so every MIR block reads a single, addressable slot),
// straight-line statement translation, and the CFG terminators MIR already
// materialized (goto / switchInt / call / return / unreachable).
//
// Semantics parity with the C backend at OptLevel 2 (-fwrapv): signed
// add/sub/mul wrap (LLVM's native integer semantics), division by zero panics
// with the C runtime's exact messages, INT_MIN / -1 panics while `% -1` is
// zero, negative indexes normalize with `i += n` like `zx_bounds`, comparisons
// are icmp/fcmp, and `>>` is arithmetic for signed types and logical for
// unsigned. With Options.DebugChecks, + - * and unary - panic on overflow via
// the llvm.s*with.overflow intrinsics, matching the checked C helpers.
package llvm

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"zexi/internal/layout"
	"zexi/internal/mir"
	"zexi/internal/token"
	"zexi/internal/types"
)

// llFnSym is the emitted symbol: `main` gets the literal symbol so the
// process entry point links; every other function keeps its unique MIR name
// (monomorphization already made names unique).
func (m *module) llFnSym(fn *mir.Fn) string {
	if fn == m.prog.Main {
		return "@main"
	}
	return llName(fn.Name)
}

// fnEmitter lowers one function body.
type fnEmitter struct {
	m    *module
	fn   *mir.Fn
	body *mir.Body
	v    int
	out  strings.Builder
	// tail holds shared panic blocks; LLVM blocks are order-free inside a
	// function body, so they are defined after the main flow.
	tail strings.Builder

	allocas map[mir.LocalID]string
	// panicLabels deduplicates panic tail blocks by reason.
	panicLabels map[string]string
	// curBlk is the LLVM block being emitted, for phi predecessor edges.
	curBlk  string
	joinSeq int
}

func (m *module) lineTo(w *strings.Builder, format string, args ...any) {
	fmt.Fprintf(w, "  "+format+"\n", args...)
}

func (e *fnEmitter) line(format string, args ...any) {
	e.m.lineTo(&e.out, format, args...)
}

func (e *fnEmitter) temp() string {
	e.v++
	return fmt.Sprintf("%%v%d", e.v)
}

// label names a new CFG block without opening it.
func (e *fnEmitter) label(kind string) string {
	e.joinSeq++
	return fmt.Sprintf("bb.%s%d", kind, e.joinSeq)
}

// startLabel opens a labeled block in the emission stream and records it as
// the block currently being emitted, so later phi nodes cite the right
// predecessor labels.
func (e *fnEmitter) startLabel(name string) {
	fmt.Fprintf(&e.out, "%s:\n", name)
	e.curBlk = name
}

func bbLabel(id mir.BlockID) string { return "bb" + strconv.Itoa(int(int32(id))) }

// allocaName returns the LLVM slot for local id, allocating storage once in
// the entry block. Alignment comes from the layout table so enum byte blobs,
// handles, and slice views land where the ABI says. Drop flags start false.
func (e *fnEmitter) allocaName(id mir.LocalID, l *mir.Local) (string, error) {
	if l == nil {
		return "", unsupportedf("local %d out of range", int32(id))
	}
	if name, ok := e.allocas[id]; ok {
		return name, nil
	}
	t, err := e.m.llTy(l.Ty)
	if err != nil {
		return "", fmt.Errorf("local %s: %w", l.Name, err)
	}
	align := int64(1)
	if sz, err := e.m.tgt.Of(l.Ty); err == nil && sz.Align > 0 {
		align = sz.Align
	}
	name := "%loc" + strconv.Itoa(int(int32(id)))
	e.m.lineTo(&e.out, "%s = alloca %s, align %d", name, t, align)
	if l.Kind == mir.LocalDropFlag {
		e.m.lineTo(&e.out, "store i1 false, ptr %s", name)
	}
	e.allocas[id] = name
	return name, nil
}

// emitFn lowers one body into the module text.
func (m *module) emitFn(fn *mir.Fn) error {
	body := fn.Body
	e := &fnEmitter{m: m, fn: fn, body: body, allocas: map[mir.LocalID]string{}, panicLabels: map[string]string{}}

	if fn == m.prog.Main && len(body.Args) > 0 {
		return unsupportedf("the LLVM backend's entry point takes no arguments yet")
	}

	retVoid := body.Ret == nil ||
		body.Ret.Kind == types.Unit || body.Ret.Kind == types.Never
	retTy := "void"
	if !retVoid {
		var err error
		if retTy, err = m.llTy(body.Ret); err != nil {
			return fmt.Errorf("%s return: %w", fn.Signature(), err)
		}
	}

	params := make([]string, 0, len(body.Args))
	for _, a := range body.Args {
		l := body.Locals[a]
		if l == nil || zeroSized(l.Ty) {
			continue // zero-sized arguments are Omitted from the ABI
		}
		t, err := m.llTy(l.Ty)
		if err != nil {
			return fmt.Errorf("%s argument %s: %w", fn.Qual, l.Name, err)
		}
		params = append(params, fmt.Sprintf("%s %%a%d", t, int(int32(a))))
	}
	fmt.Fprintf(&e.out, "\ndefine %s %s(%s) {\n", retTy, m.llFnSym(fn), strings.Join(params, ", "))
	e.startLabel(bbLabel(0))

	// Locals first, so every value has its storage before any block reads it.
	for i, l := range body.Locals {
		if l == nil || zeroSized(l.Ty) {
			continue
		}
		if _, err := e.allocaName(mir.LocalID(i), l); err != nil {
			return fmt.Errorf("%s: %w", fn.Signature(), err)
		}
	}
	for _, a := range body.Args {
		l := body.Locals[a]
		if l == nil || zeroSized(l.Ty) {
			continue
		}
		t, err := m.llTy(l.Ty)
		if err != nil {
			return err
		}
		if l.Name != "" {
			e.out.WriteString("  ; argument: " + l.Name + "\n")
		}
		// Parameter names carry the argument's local id (as in the `define`),
		// so skipped zero-sized arguments cannot shift the numbering.
		e.m.lineTo(&e.out, "store %s %%a%d, ptr %s", t, int(int32(a)), "%loc"+strconv.Itoa(int(int32(a))))
	}

	for i, blk := range body.Blocks {
		if i > 0 {
			e.startLabel(bbLabel(mir.BlockID(i)))
		}
		for _, s := range blk.Stmts {
			if err := e.stmt(s); err != nil {
				return err
			}
		}
		if err := e.term(blk.Term); err != nil {
			return err
		}
	}
	if e.tail.Len() > 0 {
		e.out.WriteString("\n")
		e.out.WriteString(e.tail.String())
	}
	e.out.WriteString("}\n")
	m.w.WriteString(e.out.String())
	return nil
}

// --- statements ------------------------------------------------------------

func (e *fnEmitter) stmt(s mir.Stmt) error {
	switch st := s.(type) {
	case *mir.Assign:
		return e.assign(st)
	case *mir.Drop:
		return e.drop(st)
	case *mir.SetDropFlag:
		return e.setFlag(st)
	default:
		return unsupportedf("statement %T", s)
	}
}

// tyOfPlace resolves the checked type of a place. Operand and dest types are
// pinned during MIR lowering, so nil is a lowering bug worth reporting.
func (e *fnEmitter) tyOfPlace(p mir.Place, fallback *types.Type) (*types.Type, error) {
	if t := e.body.Ty(p); t != nil {
		return t, nil
	}
	if fallback != nil {
		return fallback, nil
	}
	return nil, unsupportedf("place %s has no type", p.Local.String())
}

func (e *fnEmitter) assign(s *mir.Assign) error {
	dstTy, err := e.tyOfPlace(s.Place, nil)
	if err != nil {
		return err
	}
	if zeroSized(dstTy) {
		return nil // unit values store nothing
	}
	dst, _, err := e.addrOf(s.Place, dstTy)
	if err != nil {
		return err
	}
	dstLL, err := e.m.llTy(dstTy)
	if err != nil {
		return err
	}
	switch rv := s.RV.(type) {
	case mir.Use:
		val, valTy, err := e.operandValue(rv.Op)
		if err != nil {
			return err
		}
		val, err = e.conv(val, valTy, dstTy, "assignment")
		if err != nil {
			return err
		}
		e.line("store %s %s, ptr %s", dstLL, val, dst)
		return nil
	case *mir.BinOp:
		val, err := e.binop(rv)
		if err != nil {
			return err
		}
		e.line("store %s %s, ptr %s", dstLL, val, dst)
		return nil
	case *mir.UnOp:
		val, err := e.unop(rv)
		if err != nil {
			return err
		}
		e.line("store %s %s, ptr %s", dstLL, val, dst)
		return nil
	case *mir.Cast:
		val, err := e.cast(rv)
		if err != nil {
			return err
		}
		e.line("store %s %s, ptr %s", dstLL, val, dst)
		return nil
	case *mir.Aggregate:
		return e.aggregate(rv, dst, dstTy)
	case *mir.Ref:
		src, _, err := e.addrOf(rv.Place, nil)
		if err != nil {
			return err
		}
		e.line("store ptr %s, ptr %s", src, dst)
		return nil
	case *mir.Discriminant:
		val, err := e.discriminant(rv)
		if err != nil {
			return err
		}
		val, err = e.conv(val, types.I32T, dstTy, "discriminant")
		if err != nil {
			return err
		}
		e.line("store %s %s, ptr %s", dstLL, val, dst)
		return nil
	case *mir.Len:
		val, err := e.len(rv)
		if err != nil {
			return err
		}
		val, err = e.conv(val, types.UsizeT, dstTy, "len")
		if err != nil {
			return err
		}
		e.line("store %s %s, ptr %s", dstLL, val, dst)
		return nil
	case *mir.Repeat:
		return e.repeat(rv, dst, dstTy)
	default:
		return unsupportedf("rvalue %T", s.RV)
	}
}

func (e *fnEmitter) setFlag(s *mir.SetDropFlag) error {
	name, ok := e.allocas[s.Flag]
	if !ok {
		return unsupportedf("unknown drop flag %s", s.Flag.String())
	}
	v := "false"
	if s.Val {
		v = "true"
	}
	e.line("store i1 %s, ptr %s", v, name)
	return nil
}

// drop releases the owning place's value when its drop flag still says the
// value is present. Only owning handles (`String`) are dropped in this
// backend's support set; nested ownership (structs/enums of String) is a
// front-end supported feature that MIR lowering must make explicit first.
func (e *fnEmitter) drop(s *mir.Drop) error {
	pTy, err := e.tyOfPlace(s.Place, nil)
	if err != nil {
		return err
	}
	if pTy.Kind != types.StringKind {
		switch pTy.Kind {
		case types.VecKind, types.MapKind, types.Slice, types.StructKind, types.EnumKind, types.Array:
			return unsupportedf("drop of %s", pTy)
		default:
			return nil // a non-owning value releases nothing
		}
	}
	dst, _, err := e.addrOf(s.Place, nil)
	if err != nil {
		return err
	}
	e.shim("void", "@zxl_string_drop", "ptr")
	if !s.HasFlag {
		e.line("call void @zxl_string_drop(ptr %s)", dst)
		return nil
	}
	name, ok := e.allocas[s.Flag]
	if !ok {
		return unsupportedf("drop of %s without its flag", s.Place.Local.String())
	}
	present := e.temp()
	e.line("%s = load i1, ptr %s", present, name)
	dropBlk := e.label("drop")
	joinBlk := e.label("afterdrop")
	e.line("br i1 %s, label %%%s, label %%%s", present, dropBlk, joinBlk)
	e.startLabel(dropBlk)
	e.line("call void @zxl_string_drop(ptr %s)", dst)
	e.line("br label %%%s", joinBlk)
	e.startLabel(joinBlk)
	return nil
}

// --- places ----------------------------------------------------------------

// addrOf resolves a MIR place to an LLVM pointer addressing the place's
// checked value, and returns that value's type.
//
// Approach: every local is a real, addressable slot, so a projection is
// resolved purely with getelementptr and dereference loads: structs through
// their precomputed member indexes, enums through the layout table's payload
// offsets, and arrays through bounds-checked indexes. `fallback` supplies the
// type for a place whose projections carry none (the lowering pins operand
// types, so callers pass the operand's).
func (e *fnEmitter) addrOf(p mir.Place, fallback *types.Type) (string, *types.Type, error) {
	if !p.Local.Valid() || int(p.Local) >= len(e.body.Locals) {
		return "", nil, unsupportedf("place %s out of range in %s", p.Local, e.fn.Qual)
	}
	l := e.body.Locals[p.Local]
	if l == nil || l.Ty == nil {
		return "", nil, unsupportedf("place %s without a checked type", p.Local.String())
	}
	if zeroSized(l.Ty) {
		return "", nil, unsupportedf("address of zero-sized local %s", l.Name)
	}
	addr, err := e.allocaName(p.Local, l)
	if err != nil {
		return "", nil, err
	}
	curTy := l.Ty
	var (
		row     *layout.Enum    // enum layout row, once a downcast begins
		variant *layout.Variant // selected variant of that row
	)
	for _, pr := range p.Proj {
		switch pr.Kind {
		case mir.ProjDeref:
			elemTy := pr.Ty
			if elemTy == nil && curTy.Elem != nil {
				elemTy = curTy.Elem
			}
			if elemTy == nil {
				return "", nil, unsupportedf("deref without an element type")
			}
			if zeroSized(elemTy) {
				return "", nil, unsupportedf("deref to a zero-sized type")
			}
			v := e.temp()
			e.line("%s = load ptr, ptr %s", v, addr)
			addr, curTy = v, elemTy
		case mir.ProjDowncast:
			if curTy.Kind != types.EnumKind || curTy.En == nil {
				return "", nil, unsupportedf("downcast of %s", curTy)
			}
			vrow, err := e.m.enumOf(curTy.En)
			if err != nil {
				return "", nil, err
			}
			if pr.Idx < 0 || pr.Idx >= len(vrow.Variants) {
				return "", nil, unsupportedf("variant #%d out of range in %s", pr.Idx, curTy.En.Name)
			}
			row, variant = &vrow, &vrow.Variants[pr.Idx]
			addr = e.gepBytes(addr, row.PayloadOffset)
		case mir.ProjField:
			switch {
			case curTy.Kind == types.StructKind && curTy.St != nil:
				idxs, ok := e.m.memberIdx[curTy.St.Name]
				if !ok || pr.Idx < 0 || pr.Idx >= len(idxs) {
					return "", nil, unsupportedf("field %d of %s is not a declared member", pr.Idx, curTy.St.Name)
				}
				if midx := idxs[pr.Idx]; midx < 0 {
					return "", nil, unsupportedf("field %s of %s is zero-sized", pr.Name, curTy.St.Name)
				} else {
					stTy, err := e.m.llTy(curTy)
					if err != nil {
						return "", nil, err
					}
					v := e.temp()
					e.line("%s = getelementptr inbounds %s, ptr %s, i64 0, i32 %d", v, stTy, addr, midx)
					addr = v
				}
			case curTy.Kind == types.EnumKind && row != nil && variant != nil:
				if pr.Idx < 0 || pr.Idx >= len(variant.Fields) {
					return "", nil, unsupportedf("field %d of variant %s out of range", pr.Idx, variant.Name)
				}
				addr = e.gepBytes(addr, variant.Fields[pr.Idx].Offset)
			default:
				return "", nil, unsupportedf("field access on %s", curTy)
			}
			if pr.Ty == nil {
				return "", nil, unsupportedf("field %s without a checked type", pr.Name)
			}
			curTy = pr.Ty
		case mir.ProjIndex:
			if curTy.Kind != types.Array || curTy.Elem == nil || curTy.Len < 0 {
				return "", nil, unsupportedf("index into %s", curTy)
			}
			idxV, idxTy, err := e.operandValue(pr.Op)
			if err != nil {
				return "", nil, err
			}
			// The C runtime bounds helper normalizes negative indexes
			// (`i += n`) before checking, so a signed out-of-range index is
			// reinterpreted, not rejected. Reproduce exactly that with LLVM
			// arithmetic: select wraps in select, and the check panics with
			// the runtime's exact message.
			w64, err := e.idxWiden(idxV, idxTy)
			if err != nil {
				return "", nil, err
			}
			norm := w64
			if pr.Checked {
				neg := e.temp()
				e.line("%s = icmp slt i64 %s, 0", neg, norm)
				adj := e.temp()
				e.line("%s = add i64 %s, %d", adj, norm, curTy.Len)
				norm = e.temp()
				e.line("%s = select i1 %s, i64 %s, i64 %s", norm, neg, adj, w64)
				outLo := e.temp()
				e.line("%s = icmp slt i64 %s, 0", outLo, norm)
				outHi := e.temp()
				e.line("%s = icmp sge i64 %s, %d", outHi, norm, curTy.Len)
				bad := e.temp()
				e.line("%s = or i1 %s, %s", bad, outLo, outHi)
				panicBlk, err := e.boundsPanic(norm, curTy.Len)
				if err != nil {
					return "", nil, err
				}
				okBlk := e.label("idxok")
				e.line("br i1 %s, label %%%s, label %%%s", bad, panicBlk, okBlk)
				e.startLabel(okBlk)
			}
			arrLL, err := e.m.llTy(curTy)
			if err != nil {
				return "", nil, err
			}
			v := e.temp()
			e.line("%s = getelementptr inbounds %s, ptr %s, i64 0, i64 %s", v, arrLL, addr, norm)
			addr = v
			if pr.Ty == nil {
				return "", nil, unsupportedf("array index without a checked element type")
			}
			curTy = pr.Ty
		default:
			return "", nil, unsupportedf("projection %s", pr.Kind)
		}
	}
	ty := e.body.Ty(p)
	if ty == nil {
		ty = fallback
	}
	return addr, ty, nil
}

// idxWiden reinterprets an index operand as a signed i64, matching the C
// runtime's `(int64_t)` conversion: unsigned operands keep their bit pattern,
// signed ones are sign-extended.
func (e *fnEmitter) idxWiden(v string, ty *types.Type) (string, error) {
	if ty == nil {
		return v, nil // already i64-shaped; trust the lowering
	}
	w := intWidth(ty)
	if w == 64 {
		return v, nil
	}
	tyLL, err := e.m.llTy(ty)
	if err != nil {
		return "", err
	}
	ext := e.temp()
	if ty.Kind.IsUnsigned() {
		e.line("%s = zext %s %s to i64", ext, tyLL, v)
	} else {
		e.line("%s = sext %s %s to i64", ext, tyLL, v)
	}
	return ext, nil
}

// gepBytes addresses `off` bytes into a value through an i8-typed GEP.
func (e *fnEmitter) gepBytes(addr string, off int64) string {
	if off == 0 {
		return addr
	}
	v := e.temp()
	e.line("%s = getelementptr inbounds i8, ptr %s, i64 %d", v, addr, off)
	return v
}

// boundsPanic adds (once) a tail block panicking with the runtime's exact
// bounds message and returns its label.
func (e *fnEmitter) boundsPanic(idxV string, n int64) (string, error) {
	const key = "bounds"
	if lbl, ok := e.panicLabels[key]; ok {
		return lbl, nil
	}
	lbl := e.label("panic")
	e.shim("void", "@zxl_panic_bounds", "i64", "i64")
	e.m.lineTo(&e.tail, "%s:", lbl)
	e.m.lineTo(&e.tail, "call void @zxl_panic_bounds(i64 %s, i64 %d)", idxV, n)
	fmt.Fprintln(&e.tail, "  unreachable")
	e.panicLabels[key] = lbl
	return lbl, nil
}

// panicMsgPanic adds a tail block panicking with a fixed message.
func (e *fnEmitter) panicMsgPanic(msg string) (string, error) {
	if lbl, ok := e.panicLabels[msg]; ok {
		return lbl, nil
	}
	lbl := e.label("panic")
	e.shim("void", "@zxl_panic_msg", "ptr")
	e.m.lineTo(&e.tail, "%s:", lbl)
	e.m.lineTo(&e.tail, "call void @zxl_panic_msg(ptr %s)", e.m.strCon(msg))
	fmt.Fprintln(&e.tail, "  unreachable")
	e.panicLabels[msg] = lbl
	return lbl, nil
}

// --- operands and conversions ---------------------------------------------

// operandValue materializes an operand as an SSA value plus its checked type.
// Constants become literals or string globals; places are loaded via addrOf.
func (e *fnEmitter) operandValue(o mir.Operand) (string, *types.Type, error) {
	switch o.Kind {
	case mir.OpConst:
		c := o.Const
		switch c.Kind {
		case mir.ConstInt, mir.ConstChar:
			if o.Ty == nil {
				return "", nil, unsupportedf("integer constant without a type")
			}
			if _, err := e.m.llTy(o.Ty); err != nil {
				return "", nil, err
			}
			return fmt.Sprintf("%d", int64(c.Bits)), o.Ty, nil
		case mir.ConstFloat:
			if o.Ty == nil {
				return "", nil, unsupportedf("float constant without a type")
			}
			if _, err := e.m.llTy(o.Ty); err != nil {
				return "", nil, err
			}
			return floatBits(c.F, o.Ty), o.Ty, nil
		case mir.ConstBool:
			if o.Ty == nil {
				return "", nil, unsupportedf("bool constant without a type")
			}
			if _, err := e.m.llTy(o.Ty); err != nil {
				return "", nil, err
			}
			if c.Bits != 0 {
				return "true", o.Ty, nil
			}
			return "false", o.Ty, nil
		case mir.ConstStr:
			return e.m.strCon(c.S), o.Ty, nil
		case mir.ConstUnit:
			return "", o.Ty, nil
		default:
			return "", nil, unsupportedf("constant of kind %s", c.Kind)
		}
	case mir.OpCopy, mir.OpMove:
		addr, ty, err := e.addrOf(o.Place, o.Ty)
		if err != nil {
			return "", nil, err
		}
		if ty == nil {
			return "", nil, unsupportedf("copy of %s without a checked type", o.Place.String())
		}
		valLL, err := e.m.llTy(ty)
		if err != nil {
			return "", nil, err
		}
		v := e.temp()
		e.line("%s = load %s, ptr %s", v, valLL, addr)
		return v, ty, nil
	case mir.OpFn:
		if o.Fn == nil {
			return "", nil, unsupportedf("function operand without a callee")
		}
		return e.m.llFnSym(o.Fn), o.Ty, nil
	}
	return "", nil, unsupportedf("operand kind %d", o.Kind)
}

// floatBits renders a floating literal in LLVM's bit-pattern form, which
// round-trips exactly without depending on the emitter's decimal formatting.
func floatBits(f float64, ty2 *types.Type) string {
	return fmt.Sprintf("0x%016X", math.Float64bits(f))
}

// conv adapts an SSA value `val` of checked type `from` to checked type `to`,
// used where the checker's coercions are implicit (call arguments, aggregate
// fields, return stores). Pointer families and aggregates pass through.
func (e *fnEmitter) conv(val string, from, to *types.Type, what string) (string, error) {
	if from == nil || to == nil {
		return val, nil // an untyped operand is the caller's business
	}
	fx, err := e.m.llTy(from)
	if err != nil {
		return "", err
	}
	tx, err := e.m.llTy(to)
	if err != nil {
		return "", err
	}
	if fx == tx {
		return val, nil
	}
	switch {
	case from.Kind.IsInt() && to.Kind.IsInt():
		fw, tw := intWidth(from), intWidth(to)
		if fw > tw {
			v := e.temp()
			e.line("%s = trunc %s %s to %s", v, fx, val, tx)
			return v, nil
		}
		if fw < tw {
			v := e.temp()
			if from.Kind.IsUnsigned() {
				e.line("%s = zext %s %s to %s", v, fx, val, tx)
			} else {
				e.line("%s = sext %s %s to %s", v, fx, val, tx)
			}
			return v, nil
		}
		return val, nil // same width, different signedness spelling
	case from.Kind.IsInt() && to.Kind.IsFloat():
		v := e.temp()
		if from.Kind.IsUnsigned() {
			e.line("%s = uitofp %s %s to %s", v, fx, val, tx)
		} else {
			e.line("%s = sitofp %s %s to %s", v, fx, val, tx)
		}
		return v, nil
	case from.Kind.IsFloat() && to.Kind.IsInt():
		v := e.temp()
		if to.Kind.IsUnsigned() {
			e.line("%s = fptoui %s %s to %s", v, fx, val, tx)
		} else {
			e.line("%s = fptosi %s %s to %s", v, fx, val, tx)
		}
		return v, nil
	case from.Kind.IsFloat() && to.Kind.IsFloat():
		v := e.temp()
		if intWidth(from) < intWidth(to) {
			e.line("%s = fpext %s %s to %s", v, fx, val, tx)
		} else {
			e.line("%s = fptrunc %s %s to %s", v, fx, val, tx)
		}
		return v, nil
	case from.Kind == types.Bool && to.Kind.IsInt():
		v := e.temp()
		e.line("%s = zext i1 %s to %s", v, val, tx)
		return v, nil
	case from.Kind.IsInt() && to.Kind == types.Bool:
		v := e.temp()
		e.line("%s = trunc %s %s to i1", v, fx, val)
		return v, nil
	case from.Kind == types.StringKind && to.Kind == types.Str:
		v := e.temp()
		e.line("%s = extractvalue %s %s, 0", v, fx, val)
		return v, nil
	default:
		return "", unsupportedf("%s: no coercion from %s to %s", what, from, to)
	}
}

// intWidth returns a checked integer type's bit width.
func intWidth(ty *types.Type) int {
	switch ty.Kind {
	case types.I8, types.U8:
		return 8
	case types.I16, types.U16:
		return 16
	case types.I32, types.U32, types.Char:
		return 32
	default:
		// i64, u64, isize, usize (callers checked the family first).
		return 64
	}
}

// zeroSized reports whether a local or member occupies no storage.
func zeroSized(ty *types.Type) bool {
	if ty == nil {
		return false
	}
	switch ty.Kind {
	case types.Unit, types.Never:
		return true
	default:
		return false
	}
}

// --- rvalues ---------------------------------------------------------------

func (e *fnEmitter) binop(b *mir.BinOp) (string, error) {
	lv, lty, err := e.operandValue(b.L)
	if err != nil {
		return "", err
	}
	rv, rty, err := e.operandValue(b.R)
	if err != nil {
		return "", err
	}
	if b.Ty == nil {
		return "", unsupportedf("operator without a result type")
	}
	if lty != nil && (lty.Kind == types.StringKind || lty.Kind == types.VecKind || lty.Kind == types.MapKind) {
		return "", unsupportedf("operator on %s", lty)
	}
	lLL, err := e.m.llTy(b.Ty)
	if err != nil {
		return "", err
	}
	switch b.Op {
	case token.KindPlus, token.KindMinus, token.KindStar:
		// Matching convention: operands share the operator's checked type,
		// and the checker pins both operand types to the result's.
		if b.Ty.Kind.IsFloat() {
			lv, err = e.conv(lv, lty, b.Ty, "arithmetic")
			if err != nil {
				return "", err
			}
			rv, err = e.conv(rv, rty, b.Ty, "arithmetic")
			if err != nil {
				return "", err
			}
			op := map[token.Kind]string{token.KindPlus: "fadd", token.KindMinus: "fsub", token.KindStar: "fmul"}[b.Op]
			res := e.temp()
			e.line("%s = %s %s %s, %s", res, op, lLL, lv, rv)
			return res, nil
		}
		lv, err = e.conv(lv, lty, b.Ty, "arithmetic")
		if err != nil {
			return "", err
		}
		rv, err = e.conv(rv, rty, b.Ty, "arithmetic")
		if err != nil {
			return "", err
		}
		arith := map[token.Kind]string{token.KindPlus: "add", token.KindMinus: "sub", token.KindStar: "mul"}[b.Op]
		if e.m.opts.DebugChecks {
			return e.checkedArith(arith, lLL, lv, rv, b.Ty)
		}
		res := e.temp()
		e.line("%s = %s %s %s, %s", res, arith, lLL, lv, rv)
		return res, nil

	case token.KindSlash, token.KindPercent:
		lv, err = e.conv(lv, lty, b.Ty, "arithmetic")
		if err != nil {
			return "", err
		}
		rv, err = e.conv(rv, rty, b.Ty, "arithmetic")
		if err != nil {
			return "", err
		}
		if b.Ty.Kind.IsFloat() {
			if b.Op == token.KindPercent {
				return "", unsupportedf("float remainder")
			}
			// The C runtime computes IEEE semantics: /0 yields inf/nan.
			res := e.temp()
			e.line("%s = fdiv %s %s, %s", res, lLL, lv, rv)
			return res, nil
		}
		return e.intDivMod(b, lLL, lv, rv)

	case token.KindLt, token.KindLe, token.KindGt, token.KindGe, token.KindEqEq, token.KindBangEq:
		// Comparisons happen over the operands' own type (the checker pins
		// both to a common type); the comparison's result type is bool, so
		// conv'ing operands to b.Ty here would be wrong.
		clv, crv, cmpTy, err := e.compareOperands(lv, lty, rv, rty, "comparison")
		if err != nil {
			return "", err
		}
		cmpLL, err := e.m.llTy(cmpTy)
		if err != nil {
			return "", err
		}
		res := e.temp()
		if cmpTy.Kind.IsFloat() {
			// IEEE comparators match C: olt/ole/ogt/oge are false for NaN,
			// `une` is true (so NaN != NaN prints correctly).
			op := map[token.Kind]string{
				token.KindLt: "olt", token.KindLe: "ole", token.KindGt: "ogt",
				token.KindGe: "oge", token.KindEqEq: "oeq", token.KindBangEq: "une",
			}[b.Op]
			e.line("%s = fcmp %s %s %s, %s", res, op, cmpLL, clv, crv)
			return res, nil
		}
		op := map[token.Kind]string{
			token.KindLt: "slt", token.KindLe: "sle", token.KindGt: "sgt",
			token.KindGe: "sge", token.KindEqEq: "eq", token.KindBangEq: "ne",
		}[b.Op]
		if cmpTy.Kind.IsUnsigned() {
			op = map[token.Kind]string{
				token.KindLt: "ult", token.KindLe: "ule", token.KindGt: "ugt",
				token.KindGe: "uge", token.KindEqEq: "eq", token.KindBangEq: "ne",
			}[b.Op]
		}
		e.line("%s = icmp %s %s %s, %s", res, op, cmpLL, clv, crv)
		return res, nil

	case token.KindAmpAmp, token.KindPipePipe:
		// The lowering only builds these when both sides are already
		// evaluated, so a plain boolean op matches the interpreter.
		if b.Ty.Kind != types.Bool {
			return "", unsupportedf("%s on %s", b.Op, b.Ty)
		}
		op := "and"
		if b.Op == token.KindPipePipe {
			op = "or"
		}
		res := e.temp()
		e.line("%s = %s i1 %s, %s", res, op, lv, rv)
		return res, nil

	case token.KindAmp, token.KindPipe, token.KindCaret:
		lv, err = e.conv(lv, lty, b.Ty, "bitwise")
		if err != nil {
			return "", err
		}
		rv, err = e.conv(rv, rty, b.Ty, "bitwise")
		if err != nil {
			return "", err
		}
		op := map[token.Kind]string{token.KindAmp: "and", token.KindPipe: "or", token.KindCaret: "xor"}[b.Op]
		res := e.temp()
		e.line("%s = %s %s %s, %s", res, op, lLL, lv, rv)
		return res, nil

	case token.KindShl, token.KindShr:
		lv, err = e.conv(lv, lty, b.Ty, "shift")
		if err != nil {
			return "", err
		}
		// Shift counts live in their own type; the checker guarantees the
		// count fits the value's width, so widen rather than truncate.
		cnt, err := e.conv(rv, rty, b.Ty, "shift count")
		if err != nil {
			return "", err
		}
		res := e.temp()
		switch {
		case b.Op == token.KindShl:
			e.line("%s = shl %s %s, %s", res, lLL, lv, cnt)
		case b.Ty.Kind.IsSigned():
			e.line("%s = ashr %s %s, %s", res, lLL, lv, cnt)
		default:
			e.line("%s = lshr %s %s, %s", res, lLL, lv, cnt)
		}
		return res, nil
	}
	return "", unsupportedf("operator %s", b.Op)
}

// compareOperands materializes two comparison operands over a common checked
// type, returning their values and that type. The checker normally pins both
// operands to the same type; widening to the wider one is a defense.
func (e *fnEmitter) compareOperands(lv string, lty *types.Type, rv string, rty *types.Type, what string) (string, string, *types.Type, error) {
	if lty == nil || rty == nil {
		return "", "", nil, unsupportedf("%s: untyped comparison operands", what)
	}
	cmpTy := lty
	if intWidth(rty) > intWidth(lty) {
		cmpTy = rty
	}
	clv, err := e.conv(lv, lty, cmpTy, what)
	if err != nil {
		return "", "", nil, err
	}
	crv, err := e.conv(rv, rty, cmpTy, what)
	if err != nil {
		return "", "", nil, err
	}
	return clv, crv, cmpTy, nil
}

// checkedArith emits add/sub/mul via the with.overflow intrinsics and branches
// to the C runtime's exact overflow panic. Returns the wrapping result value.
func (e *fnEmitter) checkedArith(arith, tyLL, lv, rv string, ty *types.Type) (string, error) {
	wait := "s"
	if ty != nil && ty.Kind.IsUnsigned() {
		wait = "u"
	}
	intr := fmt.Sprintf("@llvm.%s%s.with.overflow.%s", wait, arith, tyLL)
	e.m.declareIntrinsic(fmt.Sprintf("declare { %s, i1 } %s(%s, %s)", tyLL, intr, tyLL, tyLL))
	pair := e.temp()
	e.line("%s = call { %s, i1 } %s(%s, %s)", pair, tyLL, intr, lv, rv)
	sum := e.temp()
	flag := e.temp()
	e.line("%s = extractvalue { %s, i1 } %s, 0", sum, tyLL, pair)
	e.line("%s = extractvalue { %s, i1 } %s, 1", flag, tyLL, pair)
	ovBlk, err := e.panicMsgPanic("attempt to " + overflowWord(arith) + " with overflow")
	if err != nil {
		return "", err
	}
	join := e.label("ovok")
	e.line("br i1 %s, label %%%s, label %%%s", flag, ovBlk, join)
	e.startLabel(join)
	return sum, nil
}

func overflowWord(arith string) string {
	switch arith {
	case "add":
		return "add"
	case "sub":
		return "subtract"
	default:
		return "multiply"
	}
}

// minSigned is INT_MIN for width w.
func minSigned(w int) int64 {
	return int64(uint64(1) << uint(w-1))
}

// intDivMod emits integer division and remainder with the C runtime's exact
// behavior: /0 and %0 panic in every profile with per-operator messages,
// INT_MIN / -1 panics instead of trapping, and `% -1` is zero.
func (e *fnEmitter) intDivMod(b *mir.BinOp, tyLL, lv, rv string) (string, error) {
	signed := b.Ty.Kind.IsSigned()
	what := "divide"
	if b.Op == token.KindPercent {
		what = "calculate the remainder of a division"
	}
	divPanic, err := e.panicMsgPanic("attempt to " + what + " by zero")
	if err != nil {
		return "", err
	}
	zero := e.temp()
	e.line("%s = icmp eq %s %s, 0", zero, tyLL, rv)
	begin := e.label("divok")
	e.line("br i1 %s, label %%%s, label %%%s", zero, divPanic, begin)
	e.startLabel(begin)

	if !signed {
		op := "udiv"
		if b.Op == token.KindPercent {
			op = "urem"
		}
		res := e.temp()
		e.line("%s = %s %s %s, %s", res, op, tyLL, lv, rv)
		return res, nil
	}

	neg1 := e.temp()
	e.line("%s = icmp eq %s %s, -1", neg1, tyLL, rv)
	negBlk := e.label("divneg")
	normBlk := e.label("divnorm")
	join := e.label("divjoin")
	e.line("br i1 %s, label %%%s, label %%%s", neg1, negBlk, normBlk)
	e.startLabel(negBlk)
	special := lv
	if b.Op == token.KindSlash {
		// The -1 path computes (-a) instead of dividing; only INT_MIN over
		// -1 overflows, checked here so x86's idiv never traps.
		absMin := e.temp()
		e.line("%s = icmp eq %s %s, %d", absMin, tyLL, lv, minSigned(intWidth(b.Ty)))
		ovPanic, err := e.panicMsgPanic("attempt to divide with overflow")
		if err != nil {
			return "", err
		}
		safeBlk := e.label("divsafe")
		e.line("br i1 %s, label %%%s, label %%%s", absMin, ovPanic, safeBlk)
		e.startLabel(safeBlk)
		neg := e.temp()
		e.line("%s = sub %s 0, %s", neg, tyLL, lv)
		special = neg
	} else {
		special = "0" // the remainder of any value by -1 is zero
	}
	e.line("br label %%%s", join)
	negPred := e.curBlk
	e.startLabel(normBlk)
	op := "sdiv"
	if b.Op == token.KindPercent {
		op = "srem"
	}
	normal := e.temp()
	e.line("%s = %s %s %s, %s", normal, op, tyLL, lv, rv)
	e.line("br label %%%s", join)
	normPred := e.curBlk
	e.startLabel(join)
	res := e.temp()
	e.line("%s = phi %s [ %s, %%%s ], [ %s, %%%s ]", res, tyLL, special, negPred, normal, normPred)
	return res, nil
}

func (e *fnEmitter) unop(u *mir.UnOp) (string, error) {
	xv, xty, err := e.operandValue(u.X)
	if err != nil {
		return "", err
	}
	if u.Ty == nil {
		return "", unsupportedf("unary without a result type")
	}
	xLL, err := e.m.llTy(u.Ty)
	if err != nil {
		return "", err
	}
	xv, err = e.conv(xv, xty, u.Ty, "unary")
	if err != nil {
		return "", err
	}
	res := e.temp()
	switch u.Op {
	case token.KindMinus:
		if u.Ty.Kind.IsFloat() {
			e.line("%s = fneg %s %s", res, xLL, xv)
			return res, nil
		}
		if e.m.opts.DebugChecks && u.Ty.Kind.IsSigned() {
			// Mirrors the C helper zx_neg_<T>: INT_MIN panics, others wrap.
			wait := "s"
			intr := fmt.Sprintf("@llvm.%ssub.with.overflow.%s", wait, xLL)
			e.m.declareIntrinsic(fmt.Sprintf("declare { %s, i1 } %s(%s, %s)", xLL, intr, xLL, xLL))
			pair := e.temp()
			e.line("%s = call { %s, i1 } %s(%s 0, %s %s)", pair, xLL, intr, xLL, xLL, xv)
			flag := e.temp()
			e.line("%s = extractvalue { %s, i1 } %s, 1", flag, xLL, pair)
			ovBlk, err := e.panicMsgPanic("attempt to negate with overflow")
			if err != nil {
				return "", err
			}
			join := e.label("negok")
			e.line("br i1 %s, label %%%s, label %%%s", flag, ovBlk, join)
			e.startLabel(join)
			val := e.temp()
			e.line("%s = extractvalue { %s, i1 } %s, 0", val, xLL, pair)
			return val, nil
		}
		e.line("%s = sub %s 0, %s", res, xLL, xv)
		return res, nil
	case token.KindBang:
		if u.Ty.Kind == types.Bool {
			e.line("%s = xor i1 %s, true", res, xv)
			return res, nil
		}
		// `!x` on an integer is logical negation typed as the operand:
		// `!0 == 1`, `!5 == 0` (sema returns the operand's type for `!`).
		if u.Ty.Kind.IsSigned() || u.Ty.Kind.IsUnsigned() {
			zero := e.temp()
			zv := e.temp()
			e.line("%s = icmp eq %s %s, 0", zero, xLL, xv)
			e.line("%s = zext i1 %s to %s", zv, zero, xLL)
			return zv, nil
		}
		return "", unsupportedf("! of %s", u.Ty)
	}
	return "", unsupportedf("unary operator %s", u.Op)
}

func (e *fnEmitter) cast(c *mir.Cast) (string, error) {
	xv, xty, err := e.operandValue(c.X)
	if err != nil {
		return "", err
	}
	switch c.Kind {
	case mir.CastNoop:
		return e.conv(xv, xty, c.To, "cast")
	case mir.CastInt, mir.CastFloat, mir.CastIntToFloat, mir.CastFloatToInt,
		mir.CastBoolInt, mir.CastCharInt:
		return e.conv(xv, xty, c.To, "cast")
	case mir.CastPtr:
		// References, pointers, and fn values share the opaque ptr type.
		return xv, nil
	case mir.CastString:
		// Borrow an owned String: the data pointer, matching the C runtime's
		// borrowed `const char*`.
		fLL, err := e.m.llTy(c.From)
		if err != nil {
			return "", err
		}
		v := e.temp()
		e.line("%s = extractvalue %s %s, 0", v, fLL, xv)
		return v, nil
	default:
		return "", unsupportedf("cast %s", c.Kind)
	}
}

// discriminant reads an enum's tag as i32, then converts it to the rvalue's
// checked type (typically the same i32; a match destination widens).
func (e *fnEmitter) discriminant(d *mir.Discriminant) (string, error) {
	addr, _, err := e.addrOf(d.Place, d.Ty)
	if err != nil {
		return "", err
	}
	// The tag is an i32 at byte 0, exactly like the C backend's `int32_t tag`.
	v := e.temp()
	e.line("%s = load i32, ptr %s", v, addr)
	return v, nil
}

// len reads the length of an array (statically known here), an owning handle
// (String/Vec/HashMap, member 1 of the shared `{ ptr, len, cap }` shape), a
// slice view (member 1 of `{ ptr, len }`), or a borrowed `str` (strlen, like
// the C runtime).
func (e *fnEmitter) len(l *mir.Len) (string, error) {
	tyOf, err := e.tyOfPlace(l.Place, l.Ty)
	if err != nil {
		return "", err
	}
	switch tyOf.Kind {
	case types.Array:
		if tyOf.Len < 0 {
			return "", unsupportedf("len of %s", tyOf)
		}
		return strconv.FormatInt(tyOf.Len, 10), nil
	case types.StringKind, types.VecKind, types.MapKind, types.Slice:
		addr, _, err := e.addrOf(l.Place, tyOf)
		if err != nil {
			return "", err
		}
		hLL, err := e.m.llTy(tyOf)
		if err != nil {
			return "", err
		}
		v := e.temp()
		e.line("%s = load %s, ptr %s", v, hLL, addr)
		res := e.temp()
		e.line("%s = extractvalue %s %s, 1", res, hLL, v)
		return res, nil
	case types.Str:
		addr, _, err := e.addrOf(l.Place, tyOf)
		if err != nil {
			return "", err
		}
		v := e.temp()
		e.line("%s = load ptr, ptr %s", v, addr)
		e.shim("i64", "@zxl_str_len", "ptr")
		res := e.temp()
		e.line("%s = call i64 @zxl_str_len(ptr %s)", res, v)
		return res, nil
	default:
		return "", unsupportedf("len of %s", tyOf)
	}
}

// aggregate writes a struct, enum variant, or array value into the
// destination place with per-field stores. Values of zero-sized fields are
// skipped (there is nothing to store); every other field is stored at its
// table offset via a GEP of the matching shape.
func (e *fnEmitter) aggregate(a *mir.Aggregate, dst string, dstTy *types.Type) error {
	switch a.Kind {
	case mir.AggStruct:
		if dstTy.St == nil {
			return unsupportedf("struct aggregate without a definition")
		}
		lo, ok := e.m.structLayout[dstTy.St.Name]
		if !ok {
			var err error
			if lo, err = e.m.tgt.Struct(dstTy.St); err != nil {
				return err
			}
			e.m.structLayout[dstTy.St.Name] = lo
		}
		idxs, ok := e.m.memberIdx[dstTy.St.Name]
		if !ok {
			// Anonymous tuple values are declared lazily by caller paths; if
			// the member table is missing, the type was not pre-declared.
			if err := e.m.declareStructType(dstTy.St); err != nil {
				return err
			}
			idxs = e.m.memberIdx[dstTy.St.Name]
		}
		for j, op := range a.Ops {
			f := lo.Fields[j]
			if zeroSized(f.Ty) {
				continue
			}
			midx := idxs[j]
			if midx < 0 {
				return unsupportedf("aggregate field %d of %s is zero-sized", j, dstTy.St.Name)
			}
			if j >= len(idxs) {
				return unsupportedf("aggregate %s has more operands than fields", dstTy.St.Name)
			}
			stTy, err := e.m.llTy(dstTy)
			if err != nil {
				return err
			}
			fLL, err := e.m.llTy(f.Ty)
			if err != nil {
				return err
			}
			p := e.temp()
			e.line("%s = getelementptr inbounds %s, ptr %s, i64 0, i32 %d", p, stTy, dst, midx)
			val, valTy, err := e.operandValue(op)
			if err != nil {
				return err
			}
			val, err = e.conv(val, valTy, f.Ty, "aggregate field")
			if err != nil {
				return err
			}
			e.line("store %s %s, ptr %s", fLL, val, p)
		}
		return nil

	case mir.AggEnum:
		if dstTy.En == nil {
			return unsupportedf("enum aggregate without a definition")
		}
		row, err := e.m.enumOf(dstTy.En)
		if err != nil {
			return err
		}
		if a.Variant < 0 || a.Variant >= len(row.Variants) {
			return unsupportedf("variant #%d of %s", a.Variant, dstTy.En.Name)
		}
		variant := row.Variants[a.Variant]
		// The tag is an i32 at TagOffset.
		tagPtr := e.gepBytes(dst, row.TagOffset)
		e.line("store i32 %d, ptr %s", variant.Index, tagPtr)
		// Payload fields live at PayloadOffset + their offset within the
		// variant's own layout.
		for j, op := range a.Ops {
			f := variant.Fields[j]
			if zeroSized(f.Ty) {
				continue
			}
			p := e.gepBytes(dst, row.PayloadOffset+f.Offset)
			fLL, err := e.m.llTy(f.Ty)
			if err != nil {
				return err
			}
			val, valTy, err := e.operandValue(op)
			if err != nil {
				return err
			}
			val, err = e.conv(val, valTy, f.Ty, "variant field")
			if err != nil {
				return err
			}
			e.line("store %s %s, ptr %s", fLL, val, p)
		}
		return nil

	case mir.AggArray:
		if dstTy.Kind != types.Array {
			return unsupportedf("array aggregate of %s", dstTy)
		}
		arrLL, err := e.m.llTy(dstTy)
		if err != nil {
			return err
		}
		for j, op := range a.Ops {
			val, valTy, err := e.operandValue(op)
			if err != nil {
				return err
			}
			val, err = e.conv(val, valTy, dstTy.Elem, "array element")
			if err != nil {
				return err
			}
			p := e.temp()
			e.line("%s = getelementptr inbounds %s, ptr %s, i64 0, i64 %d", p, arrLL, dst, j)
			elemLL, err := e.m.llTy(dstTy.Elem)
			if err != nil {
				return err
			}
			e.line("store %s %s, ptr %s", elemLL, val, p)
		}
		return nil
	}
	return unsupportedf("aggregate of kind %s", a.Kind)
}

// repeat writes `[value; count]` with a store per element, matching the
// interpreter's array construction.
func (e *fnEmitter) repeat(r *mir.Repeat, dst string, dstTy *types.Type) error {
	if dstTy.Kind != types.Array {
		return unsupportedf("repeat of %s", dstTy)
	}
	if r.Count != uint64(dstTy.Len) {
		return unsupportedf("repeat of %d elements into %s", r.Count, dstTy)
	}
	arrLL, err := e.m.llTy(dstTy)
	if err != nil {
		return err
	}
	val, valTy, err := e.operandValue(r.Op)
	if err != nil {
		return err
	}
	val, err = e.conv(val, valTy, dstTy.Elem, "repeat element")
	if err != nil {
		return err
	}
	elemLL, err := e.m.llTy(dstTy.Elem)
	if err != nil {
		return err
	}
	for i := uint64(0); i < r.Count; i++ {
		p := e.temp()
		e.line("%s = getelementptr inbounds %s, ptr %s, i64 0, i64 %d", p, arrLL, dst, i)
		e.line("store %s %s, ptr %s", elemLL, val, p)
	}
	return nil
}

// --- terminators -----------------------------------------------------------

func (e *fnEmitter) term(t mir.Terminator) error {
	switch tm := t.(type) {
	case *mir.Goto:
		e.line("br label %%%s", bbLabel(tm.Target))
		return nil
	case *mir.SwitchInt:
		dv, dty, err := e.operandValue(tm.Discr)
		if err != nil {
			return err
		}
		if dty == nil {
			return unsupportedf("switch without a discriminant type")
		}
		dLL, err := e.m.llTy(dty)
		if err != nil {
			return err
		}
		// Case values are raw bit patterns and compare equal to the
		// discriminant's own bits, exactly as the MIR switch was built.
		e.line("switch %s %s, label %%%s [", dLL, dv, bbLabel(tm.Otherwise))
		for _, cs := range tm.Cases {
			e.line("%s %d, label %%%s", dLL, cs.Val, bbLabel(cs.Target))
		}
		e.line("]")
		return nil
	case *mir.Call:
		return e.emitCall(tm)
	case *mir.Return:
		if e.body.Ret == nil || zeroSized(e.body.Ret) {
			e.line("ret void")
			return nil
		}
		retLL, err := e.m.llTy(e.body.Ret)
		if err != nil {
			return err
		}
		v := e.temp()
		e.line("%s = load %s, ptr %%loc0", v, retLL)
		e.line("ret %s %s", retLL, v)
		return nil
	case *mir.Unreachable:
		e.line("unreachable")
		return nil
	}
	return unsupportedf("terminator %T", t)
}

// --- calls -----------------------------------------------------------------

// shim records a shim helper's declaration for the module, once.
func (e *fnEmitter) shim(ret, name string, argTys ...string) {
	e.m.declareIntrinsic(fmt.Sprintf("declare %s %s(%s)", ret, name, strings.Join(argTys, ", ")))
}

// handleOut resolves where an out-parameter String constructor writes its
// result: the call's destination place, or a scratch slot when the value is
// discarded. (The constructors write through a pointer rather than returning
// the 24-byte struct, so the shim and the IR share one calling convention.)
func (e *fnEmitter) handleOut(c *mir.Call) (string, error) {
	if c.Dest != nil {
		dstTy, err := e.tyOfPlace(*c.Dest, types.StringT)
		if err != nil {
			return "", err
		}
		if !zeroSized(dstTy) {
			dst, _, err := e.addrOf(*c.Dest, dstTy)
			if err != nil {
				return "", err
			}
			return dst, nil
		}
	}
	slot := e.temp()
	e.line("%s = alloca %s, align 8", slot, handleType)
	return slot, nil
}

// storeResult writes a call's value into the terminator's Dest place.
func (e *fnEmitter) storeResult(c *mir.Call, val string, valTy *types.Type) error {
	if c.Dest == nil {
		return nil // the value is discarded
	}
	dstTy, err := e.tyOfPlace(*c.Dest, valTy)
	if err != nil {
		return err
	}
	if zeroSized(dstTy) {
		return nil
	}
	dst, _, err := e.addrOf(*c.Dest, dstTy)
	if err != nil {
		return err
	}
	val, err = e.conv(val, valTy, dstTy, "call result")
	if err != nil {
		return err
	}
	dstLL, err := e.m.llTy(dstTy)
	if err != nil {
		return err
	}
	e.line("store %s %s, ptr %s", dstLL, val, dst)
	return nil
}

// emitCall lowers one MIR call terminator: a lowered Zexi function, an
// `extern "C"` symbol, or a runtime builtin.
func (e *fnEmitter) emitCall(c *mir.Call) error {
	if c.Callee.IsBuiltin() {
		return e.emitBuiltin(c)
	}
	callee := c.Callee.Fn
	if callee == nil {
		return unsupportedf("call to %s", c.Callee)
	}
	if callee.Body == nil {
		return unsupportedf("call to extern symbol %q (extern \"C\" linking waits for its LLVM phase)", c.Callee.Extern)
	}
	hasRet := callee.Body.Ret != nil && !zeroSized(callee.Body.Ret)
	retLL := ""
	if hasRet {
		var err error
		if retLL, err = e.m.llTy(callee.Body.Ret); err != nil {
			return err
		}
	}
	argText := make([]string, 0, len(c.Args))
	for i, op := range c.Args {
		if i >= len(callee.Body.Args) {
			return unsupportedf("call to %s with more arguments than parameters", callee.Qual)
		}
		val, valTy, err := e.operandValue(op)
		if err != nil {
			return err
		}
		paramTy := callee.Body.Locals[callee.Body.Args[i]].Ty
		if zeroSized(valTy) || zeroSized(paramTy) {
			if !zeroSized(valTy) || !zeroSized(paramTy) {
				return unsupportedf("call to %s argument %d pairs zero-sized with %s",
					callee.Qual, i, paramTy)
			}
			continue // zero-sized arguments are omitted from the ABI
		}
		val, err = e.conv(val, valTy, paramTy, "call argument")
		if err != nil {
			return err
		}
		paramLL, err := e.m.llTy(paramTy)
		if err != nil {
			return err
		}
		argText = append(argText, fmt.Sprintf("%s %s", paramLL, val))
	}
	sym := e.m.llFnSym(callee)
	if hasRet {
		ret := e.temp()
		e.line("%s = call %s %s(%s)", ret, retLL, sym, strings.Join(argText, ", "))
		if err := e.storeResult(c, ret, callee.Body.Ret); err != nil {
			return err
		}
	} else {
		e.line("call void %s(%s)", sym, strings.Join(argText, ", "))
		if c.Dest != nil {
			return unsupportedf("call to %s produces no value for its destination", callee.Qual)
		}
	}
	e.line("br label %%%s", bbLabel(c.Target))
	return nil
}

// emitBuiltin lowers a runtime builtin call. Print, panic, and the String
// intrinsics are the supported set, each with a `zxl_*` shim helper whose ABI
// the shim file in this package defines.
func (e *fnEmitter) emitBuiltin(c *mir.Call) error {
	name := c.Callee.Builtin
	switch name {
	case mir.BnPrint, mir.BnPrintln:
		if err := e.printCall(c, name == mir.BnPrintln); err != nil {
			return err
		}
	case mir.BnPanic:
		return e.panicCall(c)
	case mir.BnStringNew:
		// An empty handle is the zero value: no allocation needed, matching
		// the C runtime's zero-initialized String.
		if err := e.storeResult(c, "zeroinitializer", nil); err != nil {
			return err
		}
	case "String::from":
		if len(c.Args) != 1 {
			return unsupportedf("String::from expects one argument")
		}
		val, valTy, err := e.operandValue(c.Args[0])
		if err != nil {
			return err
		}
		if valTy == nil || valTy.Kind != types.Str {
			return unsupportedf("String::from requires a str")
		}
		out, err := e.handleOut(c)
		if err != nil {
			return err
		}
		// The constructors write through an out pointer: the shim's C ABI
		// and the IR then agree on the machine-level signature.
		e.shim("void", "@zxl_string_from", "ptr", "ptr")
		e.line("call void @zxl_string_from(ptr %s, ptr %s)", val, out)
	case "String::with_capacity":
		if len(c.Args) != 1 {
			return unsupportedf("String::with_capacity expects one argument")
		}
		val, _, err := e.operandValue(c.Args[0])
		if err != nil {
			return err
		}
		out, err := e.handleOut(c)
		if err != nil {
			return err
		}
		e.shim("void", "@zxl_string_with_cap", "i64", "ptr")
		e.line("call void @zxl_string_with_cap(i64 %s, ptr %s)", val, out)
	case mir.BnStringAsStr:
		recv, err := e.stringAddr(c.Args[0], "String::as_str receiver")
		if err != nil {
			return err
		}
		e.shim("ptr", "@zxl_string_as_str", "ptr")
		v := e.temp()
		e.line("%s = call ptr @zxl_string_as_str(ptr %s)", v, recv)
		if err := e.storeResult(c, v, nil); err != nil {
			return err
		}
	case "String::len":
		recv, err := e.stringAddr(c.Args[0], "String::len receiver")
		if err != nil {
			return err
		}
		v := e.temp()
		e.line("%s = load i64, ptr %s", v, e.gepBytes(recv, e.handleFieldOff()))
		if err := e.storeResult(c, v, nil); err != nil {
			return err
		}
	case "String::is_empty":
		recv, err := e.stringAddr(c.Args[0], "String::is_empty receiver")
		if err != nil {
			return err
		}
		v := e.temp()
		e.line("%s = load i64, ptr %s", v, e.gepBytes(recv, e.handleFieldOff()))
		res := e.temp()
		e.line("%s = icmp eq i64 %s, 0", res, v)
		if err := e.storeResult(c, res, nil); err != nil {
			return err
		}
	case "String::clone":
		recv, err := e.stringAddr(c.Args[0], "String::clone receiver")
		if err != nil {
			return err
		}
		out, err := e.handleOut(c)
		if err != nil {
			return err
		}
		e.shim("void", "@zxl_string_clone", "ptr", "ptr")
		e.line("call void @zxl_string_clone(ptr %s, ptr %s)", recv, out)
	case "String::clear":
		recv, err := e.stringAddr(c.Args[0], "String::clear receiver")
		if err != nil {
			return err
		}
		e.shim("void", "@zxl_string_clear", "ptr")
		e.line("call void @zxl_string_clear(ptr %s)", recv)
	case mir.BnStringPushStr:
		recv, arg, err := e.stringMutationArgs(c, "String::push_str")
		if err != nil {
			return err
		}
		e.shim("void", "@zxl_string_push_str", "ptr", "ptr")
		e.line("call void @zxl_string_push_str(ptr %s, ptr %s)", recv, arg)
	case mir.BnStringPush:
		recv, err := e.stringAddr(c.Args[0], "String::push receiver")
		if err != nil {
			return err
		}
		if len(c.Args) != 2 {
			return unsupportedf("String::push expects two arguments")
		}
		val, valTy, err := e.operandValue(c.Args[1])
		if err != nil {
			return err
		}
		if valTy == nil || valTy.Kind != types.Char && valTy.Kind != types.U32 {
			return unsupportedf("String::push requires a char")
		}
		val, err = e.conv(val, valTy, types.U32T, "String::push char")
		if err != nil {
			return err
		}
		e.shim("void", "@zxl_string_push", "ptr", "i32")
		e.line("call void @zxl_string_push(ptr %s, i32 %s)", recv, val)
	case mir.BnStringPushI64, mir.BnStringPushU64, mir.BnStringPushF64:
		recv, err := e.stringAddr(c.Args[0], name+" receiver")
		if err != nil {
			return err
		}
		if len(c.Args) != 2 {
			return unsupportedf("%s expects two arguments", name)
		}
		val, valTy, err := e.operandValue(c.Args[1])
		if err != nil {
			return err
		}
		var want *types.Type
		var fn string
		switch name {
		case mir.BnStringPushI64:
			want, fn = types.I64T, "@zxl_string_push_i64"
		case mir.BnStringPushU64:
			want, fn = types.Builtins["u64"], "@zxl_string_push_u64"
		default:
			want, fn = types.F64T, "@zxl_string_push_f64"
		}
		val, err = e.conv(val, valTy, want, name)
		if err != nil {
			return err
		}
		wantLL, err := e.m.llTy(want)
		if err != nil {
			return err
		}
		e.shim("void", fn, "ptr", wantLL)
		e.line("call void %s(ptr %s, %s %s)", fn, recv, wantLL, val)
	default:
		return unsupportedf("builtin %q", name)
	}
	e.line("br label %%%s", bbLabel(c.Target))
	return nil
}

// handleFieldOff is the byte offset of a handle's second member (the length)
// within the shared `{ ptr, len, cap }` shape.
func (e *fnEmitter) handleFieldOff() int64 { return 8 }

// stringAddr resolves a String builtin's receiver to the address of the
// handle's storage: a reference operand loads its pointer, a place operand of
// String type is the slot itself.
func (e *fnEmitter) stringAddr(op mir.Operand, what string) (string, error) {
	ty := op.Ty
	if ty == nil {
		var err error
		if ty, err = e.tyOfPlace(op.Place, nil); err != nil {
			return "", err
		}
	}
	if ty == nil {
		return "", unsupportedf("%s without a checked type", what)
	}
	switch ty.Kind {
	case types.Ref:
		v, _, err := e.operandValue(op)
		if err != nil {
			return "", err
		}
		if ty.Elem != nil && ty.Elem.Kind == types.Ref {
			// A reference-to-reference receiver keeps peeling: one extra
			// load per indirection.
			for inner := ty.Elem; inner != nil && inner.Kind == types.Ref; inner = inner.Elem {
				d := e.temp()
				e.line("%s = load ptr, ptr %s", d, v)
				v = d
			}
		}
		return v, nil
	case types.StringKind:
		addr, _, err := e.addrOf(op.Place, ty)
		if err != nil {
			return "", err
		}
		return addr, nil
	default:
		return "", unsupportedf("%s of %s", what, ty)
	}
}

// stringMutationArgs resolves a mutating String builtin's receiver and its
// text argument, checking the shapes the checker has already pinned.
func (e *fnEmitter) stringMutationArgs(c *mir.Call, name string) (string, string, error) {
	if len(c.Args) != 2 {
		return "", "", unsupportedf("%s expects two arguments", name)
	}
	recv, err := e.stringAddr(c.Args[0], name+" receiver")
	if err != nil {
		return "", "", err
	}
	text, textTy, err := e.operandValue(c.Args[1])
	if err != nil {
		return "", "", err
	}
	if textTy == nil || textTy.Kind != types.Str {
		return "", "", unsupportedf("%s requires a str", name)
	}
	return recv, text, nil
}

// printCall lowers `print`/`println`: each argument prints on its own, with
// single spaces between them and a newline last for println, exactly like the
// interp's print builtin (which mirrors the C backend's per-argument print).
func (e *fnEmitter) printCall(c *mir.Call, nl bool) error {
	for i, op := range c.Args {
		if i > 0 {
			e.shim("void", "@zxl_print_sep")
			e.line("call void @zxl_print_sep()")
		}
		if err := e.printOperand(op); err != nil {
			return err
		}
	}
	if nl {
		e.shim("void", "@zxl_print_nl")
		e.line("call void @zxl_print_nl()")
	}
	return nil
}

// printOperand prints one value with the same text the C runtime would produce:
// decimal integers, %g floats, true/false, UTF-8 chars, and string contents.
func (e *fnEmitter) printOperand(op mir.Operand) error {
	val, ty, err := e.operandValue(op)
	if err != nil {
		return err
	}
	// Borrowed values print through to the value behind the borrow.
	for ty != nil && ty.Kind == types.Ref && ty.Elem != nil {
		v := e.temp()
		e.line("%s = load ptr, ptr %s", v, val)
		val = v
		ty = ty.Elem
	}
	if ty == nil {
		return unsupportedf("print of an untyped operand")
	}
	switch {
	case ty.Kind == types.StringKind:
		tyLL, err := e.m.llTy(ty)
		if err != nil {
			return err
		}
		v := e.temp()
		e.line("%s = extractvalue %s %s, 0", v, tyLL, val)
		e.shim("void", "@zxl_print_str", "ptr")
		e.line("call void @zxl_print_str(ptr %s)", v)
		return nil
	case ty.Kind == types.Str:
		e.shim("void", "@zxl_print_str", "ptr")
		e.line("call void @zxl_print_str(ptr %s)", val)
		return nil
	case ty.Kind == types.Char:
		e.shim("void", "@zxl_print_char", "i32")
		e.line("call void @zxl_print_char(i32 %s)", val)
		return nil
	case ty.Kind == types.Bool:
		v, err := e.conv(val, ty, types.U8T, "print bool")
		if err != nil {
			return err
		}
		e.shim("void", "@zxl_print_bool", "i8")
		e.line("call void @zxl_print_bool(i8 %s)", v)
		return nil
	case ty.Kind.IsSigned():
		v, err := e.conv(val, ty, types.I64T, "print int")
		if err != nil {
			return err
		}
		e.shim("void", "@zxl_print_i64", "i64")
		e.line("call void @zxl_print_i64(i64 %s)", v)
		return nil
	case ty.Kind.IsUnsigned():
		v, err := e.conv(val, ty, types.Builtins["u64"], "print uint")
		if err != nil {
			return err
		}
		e.shim("void", "@zxl_print_u64", "i64")
		e.line("call void @zxl_print_u64(i64 %s)", v)
		return nil
	case ty.Kind.IsFloat():
		v, err := e.conv(val, ty, types.F64T, "print float")
		if err != nil {
			return err
		}
		e.shim("void", "@zxl_print_f64", "double")
		e.line("call void @zxl_print_f64(double %s)", v)
		return nil
	}
	return unsupportedf("print of %s", ty)
}

// panicCall lowers the `panic` builtin: report the message (or the default
// text when none is given) and end the block without a successor, exactly
// like the C runtime's panic exit path.
func (e *fnEmitter) panicCall(c *mir.Call) error {
	if len(c.Args) > 1 {
		return unsupportedf("panic with %d arguments", len(c.Args))
	}
	msg := e.m.strCon("panic")
	if len(c.Args) == 1 {
		val, ty, err := e.operandValue(c.Args[0])
		if err != nil {
			return err
		}
		for ty != nil && ty.Kind == types.Ref && ty.Elem != nil {
			v := e.temp()
			e.line("%s = load ptr, ptr %s", v, val)
			val = v
			ty = ty.Elem
		}
		if ty == nil {
			return unsupportedf("panic argument without a checked type")
		}
		switch ty.Kind {
		case types.Str:
			msg = val
		case types.StringKind:
			tyLL, err := e.m.llTy(ty)
			if err != nil {
				return err
			}
			v := e.temp()
			e.line("%s = extractvalue %s %s, 0", v, tyLL, val)
			msg = v
		default:
			return unsupportedf("panic argument of %s (the checker must interpolate it into a string first)", ty)
		}
	}
	e.shim("void", "@zxl_panic_msg", "ptr")
	e.line("call void @zxl_panic_msg(ptr %s)", msg)
	e.line("unreachable")
	return nil
}
