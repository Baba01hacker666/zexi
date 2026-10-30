package mir

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"zexi/internal/ast"
	"zexi/internal/sema"
	"zexi/internal/token"
	"zexi/internal/types"
)

// Unsupported records a construct this lowering pass does not handle yet. It is
// data rather than an error because `zexic emit --emit=mir` should still show
// everything it *can* lower, and because the count of unsupported constructs is
// the honest progress metric for the MIR migration.
type Unsupported struct {
	Fn      string
	Feature string
	Sp      token.Span
}

func (u Unsupported) String() string { return fmt.Sprintf("%s: %s", u.Fn, u.Feature) }

// Result is what Lower produces: the program plus every function it had to skip.
type Result struct {
	Prog        *Program
	Unsupported []Unsupported
	// Lowered and Total count reachable functions.
	Lowered int
	Total   int
}

func unsupportedf(sp token.Span, format string, args ...any) error {
	return &lowerErr{msg: fmt.Sprintf(format, args...), sp: sp}
}

type lowerErr struct {
	msg string
	sp  token.Span
}

func (e *lowerErr) Error() string { return e.msg }

// Lower turns a checked program into MIR.
//
// Only functions reachable from `main` (and from `#[no_mangle]` entry points)
// are lowered, matching what the C backend emits. A function that uses a
// construct with no lowering yet is skipped and reported in Result.Unsupported;
// the rest of the program still lowers, so coverage can grow construct by
// construct without blocking on any single feature.
func Lower(info *sema.Info) *Result {
	res := &Result{Prog: &Program{Structs: info.StructOrd, Enums: info.EnumOrd}}
	used := reachableFns(info)

	// Pass 1: create the Fn shells, so a call can reference a function that is
	// lowered later (mutual recursion needs this).
	bySema := make(map[*sema.Fn]*Fn, len(info.FnOrder))
	for _, src := range info.FnOrder {
		if !used[src] {
			continue
		}
		fn := &Fn{Name: src.Name, Qual: src.QualName, Src: src}
		if src.Decl != nil && src.Decl.Body == nil {
			fn.Extern = externSymbol(src)
		}
		bySema[src] = fn
		res.Prog.Fns = append(res.Prog.Fns, fn)
		if src == info.Main {
			res.Prog.Main = fn
		}
	}

	// Pass 2: lower bodies.
	failed := map[*Fn]string{}
	for _, src := range info.FnOrder {
		fn := bySema[src]
		if fn == nil || fn.Extern != "" {
			continue
		}
		res.Total++
		l := &lowerer{info: info, fn: fn, bySema: bySema}
		body, err := l.lowerFn(src)
		if err != nil {
			failed[fn] = err.Error()
			res.Unsupported = append(res.Unsupported, Unsupported{Fn: src.Name, Feature: err.Error(), Sp: spanOf(err, src)})
			continue
		}
		fn.Body = body
		res.Lowered++
	}

	// Pass 3: a function that calls something which failed cannot be lowered
	// either — propagate to a fixpoint so the report names the real cause and
	// nothing in the IR calls a bodyless function.
	for changed := true; changed; {
		changed = false
		for _, fn := range res.Prog.Fns {
			if fn.Body == nil || fn.Extern != "" {
				continue
			}
			if name, ok := firstUnloweredCallee(fn.Body, failed); ok {
				failed[fn] = fmt.Sprintf("calls `%s`, which is not lowered", name)
				res.Unsupported = append(res.Unsupported, Unsupported{Fn: fn.Name, Feature: failed[fn]})
				fn.Body = nil
				res.Lowered--
				changed = true
			}
		}
	}

	var kept []*Fn
	for _, fn := range res.Prog.Fns {
		if fn.Extern != "" {
			kept = append(kept, fn)
			continue
		}
		if fn.Body == nil {
			if res.Prog.Main == fn {
				res.Prog.Main = nil
			}
			continue
		}
		kept = append(kept, fn)
	}
	res.Prog.Fns = kept
	return res
}

// firstUnloweredCallee reports the first call in a body that targets a function
// that failed to lower.
func firstUnloweredCallee(b *Body, failed map[*Fn]string) (string, bool) {
	var check func(t Terminator) (string, bool)
	check = func(t Terminator) (string, bool) {
		c, ok := t.(*Call)
		if !ok || c.Callee.Fn == nil {
			return "", false
		}
		if _, bad := failed[c.Callee.Fn]; bad {
			return c.Callee.Fn.Name, true
		}
		return "", false
	}
	for _, blk := range b.Blocks {
		if name, ok := check(blk.Term); ok {
			return name, true
		}
	}
	return "", false
}

func spanOf(err error, src *sema.Fn) token.Span {
	if le, ok := err.(*lowerErr); ok {
		return le.sp
	}
	if src != nil && src.Decl != nil {
		return src.Decl.Sp
	}
	return token.Span{}
}

func externSymbol(src *sema.Fn) string {
	if src.Decl != nil && src.Decl.Name != "" {
		return src.Decl.Name
	}
	return src.Name
}

// --- reachability ---------------------------------------------------------

// reachableFns mirrors the C backend's usedFns: start from `main` and every
// `#[no_mangle]` function, then follow resolved calls, method calls, and names
// used as values. Without an entry point everything is kept, which is what a
// library-only module needs.
func reachableFns(info *sema.Info) map[*sema.Fn]bool {
	if info.Main == nil || info.Main.Decl == nil || info.Main.Decl.Body == nil {
		all := make(map[*sema.Fn]bool, len(info.FnOrder))
		for _, f := range info.FnOrder {
			all[f] = true
		}
		return all
	}
	used := map[*sema.Fn]bool{}
	var queue []*sema.Fn
	add := func(f *sema.Fn) {
		if f != nil && !used[f] {
			used[f] = true
			queue = append(queue, f)
		}
	}
	add(info.Main)
	for _, f := range info.FnOrder {
		if f.NoMangle {
			add(f)
		}
	}
	for len(queue) > 0 {
		f := queue[0]
		queue = queue[1:]
		if f.Decl == nil || f.Decl.Body == nil {
			continue
		}
		walkExprs(f.Decl.Body, func(e ast.Expr) {
			switch x := e.(type) {
			case *ast.CallExpr:
				add(info.Resolved[x])
			case *ast.MethodCallExpr:
				add(info.Resolved[x])
			case *ast.IdentExpr:
				if r := info.FnValues[x]; r != nil {
					add(r)
				} else {
					add(info.Fns[x.Name])
				}
			case *ast.PathExpr:
				if len(x.Segments) == 1 {
					add(info.Fns[x.Segments[0]])
				}
			}
		})
	}
	return used
}

// walkExprs visits every expression in a syntax subtree. Reflection keeps the
// walk correct as AST nodes are added, which matters because a missed call edge
// would silently drop a reachable function from the program.
func walkExprs(root any, fn func(ast.Expr)) {
	spanPkg := reflect.TypeOf(token.Span{}).PkgPath()
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		if !v.IsValid() {
			return
		}
		switch v.Kind() {
		case reflect.Pointer, reflect.Interface:
			if v.IsNil() {
				return
			}
			if e, ok := v.Interface().(ast.Expr); ok {
				fn(e)
			}
			walk(v.Elem())
		case reflect.Struct:
			if v.Type().PkgPath() == spanPkg {
				return // source positions carry no expressions
			}
			for i := 0; i < v.NumField(); i++ {
				walk(v.Field(i))
			}
		case reflect.Slice, reflect.Array:
			for i := 0; i < v.Len(); i++ {
				walk(v.Index(i))
			}
		}
	}
	walk(reflect.ValueOf(root))
}

// --- the lowerer ----------------------------------------------------------

type lowerer struct {
	info   *sema.Info
	fn     *Fn
	body   *Body
	bySema map[*sema.Fn]*Fn
	cur    BlockID
	scopes []*scope
	loops  []*loopFrame
}

type scope struct {
	names map[string]LocalID
	drops []dropEntry
}

type dropEntry struct {
	local LocalID
	flag  LocalID
}

type loopFrame struct {
	// cont is where `continue` goes: the body of a `loop`, the condition of a
	// `while`, or the increment block of a `for`.
	cont BlockID
	// exit is where `break` goes.
	exit BlockID
	// result is where `break <value>` writes, or -1 when the loop yields none.
	result LocalID
	// depth is len(scopes) at loop entry, so break/continue drop exactly the
	// scopes entered inside the loop.
	depth int
}

func (l *lowerer) lowerFn(src *sema.Fn) (*Body, error) {
	if src.Decl == nil || src.Decl.Body == nil {
		return nil, nil
	}
	l.body = &Body{Ret: src.Ret, Sp: src.Decl.Sp}
	l.cur = l.newBlock()
	l.pushScope()

	// Local 0 is the return place. It is never dropped here: returning an owning
	// value hands it to the caller, which drops it in its own scope.
	l.addLocal("", src.Ret, LocalReturn)

	for i, p := range src.Params {
		name := ""
		if i < len(src.ParamNames) {
			name = src.ParamNames[i]
		}
		if i == 0 && src.Self != nil {
			name = "self"
		}
		id := l.addLocal(name, p, LocalArg)
		l.body.Args = append(l.body.Args, id)
		if name != "" {
			l.declare(name, id)
		}
	}
	// By-value owning parameters arrive initialized and belong to the callee.
	// Register them after allocating all arguments so argument IDs stay
	// contiguous, and returning/moving one clears its flag just like a local.
	for _, id := range l.body.Args {
		if ty := l.body.Locals[id].Ty; ty.NeedsDrop() {
			l.registerDrop(id, ty)
		}
	}

	blk := src.Decl.Body
	for _, s := range blk.Stmts {
		if err := l.stmt(s); err != nil {
			return nil, err
		}
	}
	if blk.Tail != nil && !l.terminated() {
		if src.Ret != nil && src.Ret.Kind != types.Unit {
			if err := l.exprInto(blk.Tail, Place{Local: 0}); err != nil {
				return nil, err
			}
		} else if err := l.discard(blk.Tail); err != nil {
			return nil, err
		}
	}
	l.exitScope()
	if !l.terminated() {
		l.setTerm(&Return{Sp: blk.Sp})
	}
	return l.body, nil
}

// --- blocks and scopes ----------------------------------------------------

func (l *lowerer) newBlock() BlockID {
	l.body.Blocks = append(l.body.Blocks, &BasicBlock{})
	return BlockID(len(l.body.Blocks) - 1)
}

func (l *lowerer) terminated() bool { return l.body.Blocks[l.cur].Term != nil }

// setTerm installs the current block's terminator. Lowering keeps walking the
// AST after a `return` (the recursion has to unwind somehow); statements that
// follow are simply not emitted, because emit() is a no-op in a terminated
// block. Divergence is therefore implicit in the CFG rather than a mode the
// recursion has to track.
func (l *lowerer) setTerm(t Terminator) {
	if l.terminated() {
		return
	}
	l.body.Blocks[l.cur].Term = t
}

func (l *lowerer) emit(s Stmt) {
	if l.terminated() {
		return
	}
	l.body.Blocks[l.cur].Stmts = append(l.body.Blocks[l.cur].Stmts, s)
}

func (l *lowerer) gotoBlock(b BlockID, sp token.Span) { l.setTerm(&Goto{Target: b, Sp: sp}) }

func (l *lowerer) pushScope() {
	l.scopes = append(l.scopes, &scope{names: map[string]LocalID{}})
}

// exitScope drops the scope's owning locals in reverse declaration order and
// pops it. The drops are skipped when the block is already terminated: control
// never reaches them, and a `return` has already emitted equivalent drops.
func (l *lowerer) exitScope() { l.unwindTo(len(l.scopes) - 1); l.scopes = l.scopes[:len(l.scopes)-1] }

// unwindTo emits drops for every scope at or above `depth`, innermost first,
// without popping. Callers that continue lowering then pop once, so a path that
// leaves a scope early (a guard failure, a `break`) gets its own drops without
// disturbing the scope stack the rest of the recursion relies on.
func (l *lowerer) unwindTo(depth int) {
	for i := len(l.scopes) - 1; i >= depth; i-- {
		sc := l.scopes[i]
		for j := len(sc.drops) - 1; j >= 0; j-- {
			d := sc.drops[j]
			l.emit(&Drop{Place: Place{Local: d.local}, Flag: d.flag, HasFlag: true})
		}
	}
}

func (l *lowerer) declare(name string, id LocalID) {
	l.scopes[len(l.scopes)-1].names[name] = id
}

func (l *lowerer) lookup(name string) (LocalID, bool) {
	for i := len(l.scopes) - 1; i >= 0; i-- {
		if id, ok := l.scopes[i].names[name]; ok {
			return id, true
		}
	}
	return 0, false
}

// --- locals ---------------------------------------------------------------

func (l *lowerer) addLocal(name string, ty *types.Type, kind LocalKind) LocalID {
	if ty == nil {
		ty = types.UnitT
	}
	l.body.Locals = append(l.body.Locals, &Local{Name: name, Ty: ty, Kind: kind})
	return LocalID(len(l.body.Locals) - 1)
}

// temp allocates a compiler temporary. Owning temporaries are registered for
// dropping in the current scope exactly like `let` bindings, so a temporary can
// never leak: the scope's drop list is the single place that decides what is
// released.
func (l *lowerer) temp(ty *types.Type) LocalID {
	id := l.addLocal("", ty, LocalTemp)
	if ty != nil && ty.NeedsDrop() {
		l.registerDrop(id, ty)
	}
	return id
}

// registerDrop gives an owning local a drop flag (true now, false after a move)
// and records it for scope exit.
func (l *lowerer) registerDrop(id LocalID, ty *types.Type) LocalID {
	flag := l.addLocal("", types.BoolT, LocalDropFlag)
	l.emit(&SetDropFlag{Flag: flag, Val: true, Sp: token.Span{}})
	sc := l.scopes[len(l.scopes)-1]
	sc.drops = append(sc.drops, dropEntry{local: id, flag: flag})
	return flag
}

// flagOf reports the drop flag registered for a whole-local place, if any.
func (l *lowerer) flagOf(p Place) (LocalID, bool) {
	if !p.IsLocal() {
		return 0, false
	}
	for i := len(l.scopes) - 1; i >= 0; i-- {
		for _, d := range l.scopes[i].drops {
			if d.local == p.Local {
				return d.flag, true
			}
		}
	}
	return 0, false
}

// --- types ----------------------------------------------------------------

func (l *lowerer) ty(e ast.Expr) *types.Type {
	if e == nil {
		return nil
	}
	return l.info.TypeOf(e)
}

func isUnit(t *types.Type) bool { return t != nil && t.Kind == types.Unit }

// isNoValue reports whether an expression produces nothing to store: `()` and
// `never` (a branch that always diverges) both need a destination local only
// because lowering is uniform, not because anything is read back.
func isNoValue(t *types.Type) bool {
	return t != nil && (t.Kind == types.Unit || t.Kind == types.Never)
}

func (l *lowerer) unitOperand() Operand {
	return Operand{Kind: OpConst, Const: UnitConst(), Ty: types.UnitT}
}

// --- places ---------------------------------------------------------------

// placeOf reports whether e denotes storage. It returns ok=false for value
// expressions (calls, literals, operators), which callers materialize into a
// temporary instead.
func (l *lowerer) placeOf(e ast.Expr) (Place, bool, error) {
	switch x := e.(type) {
	case *ast.IdentExpr:
		if id, ok := l.lookup(x.Name); ok {
			return Place{Local: id}, true, nil
		}
		return Place{}, false, nil
	case *ast.UnExpr:
		if x.Op != token.KindStar {
			return Place{}, false, nil
		}
		base, ok, err := l.placeOf(x.X)
		if err != nil || !ok {
			return Place{}, false, err
		}
		bt := l.body.Ty(base)
		if bt == nil || (bt.Kind != types.Ref && bt.Kind != types.Ptr) {
			return Place{}, false, unsupportedf(x.Sp, "dereference of a non-reference value")
		}
		base.Proj = append(base.Proj, DerefProj(bt.Elem))
		return base, true, nil
	case *ast.FieldExpr:
		base, ok, err := l.placeOf(x.Recv)
		if err != nil || !ok {
			return Place{}, false, err
		}
		base, st, err := l.autoDeref(base, x.Sp)
		if err != nil {
			return Place{}, false, err
		}
		if st == nil || st.Kind != types.StructKind || st.St == nil {
			return Place{}, false, unsupportedf(x.Sp, "field access on `%s`", st)
		}
		idx, fty, ok := fieldIndex(st.St, x.Field)
		if !ok {
			return Place{}, false, unsupportedf(x.Sp, "field `%s` of `%s`", x.Field, st)
		}
		base.Proj = append(base.Proj, FieldProj(idx, x.Field, fty))
		return base, true, nil
	case *ast.IndexExpr:
		base, ok, err := l.placeOf(x.Recv)
		if err != nil || !ok {
			return Place{}, false, err
		}
		base, rt, err := l.autoDeref(base, x.Sp)
		if err != nil {
			return Place{}, false, err
		}
		if rt == nil || rt.Kind != types.Array {
			return Place{}, false, unsupportedf(x.Sp, "indexing `%s` (only fixed arrays index in MIR so far)", rt)
		}
		idxOp, err := l.expr(x.Index)
		if err != nil {
			return Place{}, false, err
		}
		base.Proj = append(base.Proj, IndexProj(idxOp, rt.Elem, true))
		return base, true, nil
	}
	return Place{}, false, nil
}

// autoDeref peels references off a place, which is what field and index access
// do implicitly (`self.field`, `a[i]` where `a: &mut [T; N]`).
func (l *lowerer) autoDeref(p Place, sp token.Span) (Place, *types.Type, error) {
	t := l.body.Ty(p)
	for t != nil && (t.Kind == types.Ref || t.Kind == types.Ptr) {
		if t.Elem == nil {
			return p, nil, unsupportedf(sp, "reference without an element type")
		}
		p.Proj = append(p.Proj, DerefProj(t.Elem))
		t = t.Elem
	}
	return p, t, nil
}

func fieldIndex(st *types.Struct, name string) (int, *types.Type, bool) {
	for i := range st.Fields {
		if st.Fields[i].Name == name {
			return i, st.Fields[i].Ty, true
		}
	}
	if st.Tuple {
		// Tuple-struct fields are spelled by position: `p.0`.
		if n, err := strconv.Atoi(name); err == nil && n >= 0 && n < len(st.Fields) {
			return n, st.Fields[n].Ty, true
		}
	}
	return 0, nil, false
}

// read turns a place into an operand: a copy for `Copy` types, a move for owning
// types. A move clears the place's drop flag on the spot, so the scope-exit drop
// for that local becomes a no-op.
func (l *lowerer) read(p Place, sp token.Span) (Operand, error) {
	t := l.body.Ty(p)
	if t == nil {
		return Operand{}, unsupportedf(sp, "value without a type")
	}
	if t.IsCopy() {
		return Copy(p, t), nil
	}
	if !p.IsLocal() {
		// A projected owning value needs a per-field drop flag (or explicit
		// drop-safe clearing). Moving it while retaining the parent's drop flag
		// would release the same resource twice. Pattern bindings have their own
		// narrow exception for a variant's sole owning field (moveBinding).
		if t.NeedsDrop() {
			return Operand{}, unsupportedf(sp, "partial move from `%s` requires per-field drop flags", p)
		}
		return Move(p, t), nil
	}
	if flag, ok := l.flagOf(p); ok {
		l.emit(&SetDropFlag{Flag: flag, Val: false, Sp: sp})
	}
	return Move(p, t), nil
}

// --- expressions ----------------------------------------------------------

// expr lowers e to an operand holding its value. Place expressions stay places;
// everything else is materialized into a temporary first.
func (l *lowerer) expr(e ast.Expr) (Operand, error) {
	if l.terminated() {
		return l.unitOperand(), nil
	}
	t := l.ty(e)
	if isNoValue(t) {
		tmp := l.addLocal("", types.UnitT, LocalTemp)
		if err := l.exprInto(e, Place{Local: tmp}); err != nil {
			return Operand{}, err
		}
		return l.unitOperand(), nil
	}
	if p, ok, err := l.placeOf(e); err != nil {
		return Operand{}, err
	} else if ok {
		return l.read(p, e.Span())
	}
	tmp := l.temp(t)
	if err := l.exprInto(e, Place{Local: tmp}); err != nil {
		return Operand{}, err
	}
	return l.read(Place{Local: tmp}, e.Span())
}

// tempFor materializes e into a fresh temporary and returns its place (never a
// move), which is what borrowing needs.
func (l *lowerer) tempFor(e ast.Expr) (Place, error) {
	t := l.ty(e)
	if t == nil {
		return Place{}, unsupportedf(e.Span(), "value without a type")
	}
	tmp := l.temp(t)
	place := Place{Local: tmp}
	// A borrowed temporary is not initialized until its expression finishes.
	// If that expression returns or breaks, scope exit must not drop garbage.
	flag, owning := l.flagOf(place)
	if owning {
		l.emit(&SetDropFlag{Flag: flag, Val: false, Sp: e.Span()})
	}
	if err := l.exprInto(e, place); err != nil {
		return Place{}, err
	}
	if owning && !l.terminated() {
		l.emit(&SetDropFlag{Flag: flag, Val: true, Sp: e.Span()})
	}
	return place, nil
}

// discard evaluates e and drops its value at the end of the statement.
func (l *lowerer) discard(e ast.Expr) error {
	l.pushScope()
	ty := l.ty(e)
	if isNoValue(ty) {
		ty = types.UnitT
	}
	tmp := l.temp(ty)
	if err := l.exprInto(e, Place{Local: tmp}); err != nil {
		return err
	}
	l.exitScope()
	return nil
}

func (l *lowerer) assign(dest Place, op Operand, sp token.Span) {
	if op.Ty == nil {
		// The checker does not record a type for every literal in every
		// position; the destination's type is the answer by construction.
		op.Ty = l.body.Ty(dest)
	}
	l.emit(&Assign{Place: dest, RV: Use{Op: op}, Sp: sp})
}

// exprInto lowers e so that its value ends up in dest.
func (l *lowerer) exprInto(e ast.Expr, dest Place) error {
	if l.terminated() {
		return nil
	}
	switch x := e.(type) {
	case *ast.LiteralExpr:
		c, err := l.literal(x)
		if err != nil {
			return err
		}
		l.assign(dest, Operand{Kind: OpConst, Const: c, Ty: l.ty(e)}, x.Sp)
		return nil

	case *ast.TupleExpr:
		if len(x.Elems) == 0 {
			return nil // `()`
		}
		return unsupportedf(x.Sp, "tuple values")

	case *ast.IdentExpr:
		return l.identInto(x, dest)

	case *ast.PathExpr:
		return l.pathInto(x, dest)

	case *ast.UnExpr:
		return l.unaryInto(x, dest)

	case *ast.BinExpr:
		return l.binaryInto(x, dest)

	case *ast.AssignExpr:
		return l.assignInto(x)

	case *ast.CastExpr:
		op, err := l.expr(x.X)
		if err != nil {
			return err
		}
		l.emit(&Assign{Place: dest, RV: &Cast{
			Kind: castKind(l.ty(x.X), l.ty(x)),
			X:    op,
			From: l.ty(x.X),
			To:   l.ty(x),
		}, Sp: x.Sp})
		return nil

	case *ast.IfExpr:
		return l.ifInto(x, dest)

	case *ast.Block:
		return l.blockInto(x, dest)

	case *ast.LoopExpr:
		return l.loopInto(x, dest)

	case *ast.CallExpr:
		return l.callInto(x, dest)

	case *ast.MethodCallExpr:
		// The builtin `Option`/`Result` methods were already lowered by the
		// checker: the extractors are an ordinary match, and a tag test is a
		// discriminant comparison.
		if m := l.info.PreludeMatches[x]; m != nil {
			return l.matchInto(m, dest)
		}
		if variant, ok := l.info.PreludeTag[x]; ok {
			return l.preludeTagInto(x, variant, dest)
		}
		return l.methodInto(x, dest)

	case *ast.FieldExpr, *ast.IndexExpr:
		p, ok, err := l.placeOf(e)
		if err != nil {
			return err
		}
		if !ok {
			return unsupportedf(e.Span(), "expression is not a place")
		}
		op, err := l.read(p, e.Span())
		if err != nil {
			return err
		}
		l.assign(dest, op, e.Span())
		return nil

	case *ast.StructLitExpr:
		return l.structLitInto(x, dest)

	case *ast.ArrayLitExpr:
		return l.arrayLitInto(x, dest)

	case *ast.MatchExpr:
		return l.matchInto(x, dest)

	case *ast.TryExpr:
		m := l.info.TryMatches[x]
		if m == nil {
			return unsupportedf(x.Sp, "`?` without a desugared match")
		}
		return l.matchInto(m, dest)

	case *ast.JumpExpr:
		return l.jump(x)

	case *ast.InterpStrExpr:
		return l.interpStrInto(x, dest)

	case *ast.SliceExpr:
		return unsupportedf(x.Sp, "slice expressions")

	case *ast.NamedArgExpr:
		return unsupportedf(x.Sp, "named arguments (the checker rewrites these before MIR)")
	}
	return unsupportedf(e.Span(), "expression %T", e)
}

// --- statements -----------------------------------------------------------

func (l *lowerer) stmt(s ast.Stmt) error {
	if l.terminated() {
		return nil
	}
	switch st := s.(type) {
	case *ast.LetStmt:
		return l.letStmt(st)
	case *ast.ExprStmt:
		return l.discard(st.X)
	case *ast.DeferStmt:
		return unsupportedf(st.Sp, "`defer`")
	}
	return unsupportedf(s.Span(), "statement %T", s)
}

func (l *lowerer) letStmt(st *ast.LetStmt) error {
	ty := l.info.DeclTypes[st]
	if ty == nil {
		ty = l.ty(st.Val)
	}
	if ty != nil && ty.Kind == types.Never {
		if st.Val != nil {
			return l.discard(st.Val)
		}
		return nil
	}
	switch p := st.Pat.(type) {
	case *ast.WildcardPat:
		if st.Val == nil {
			return nil
		}
		return l.discard(st.Val)
	case *ast.BindPat:
		if p.Name == "_" {
			if st.Val == nil {
				return nil
			}
			return l.discard(st.Val)
		}
		id := l.addLocal(p.Name, ty, LocalUser)
		l.body.Locals[id].Mut = st.Mut
		l.declare(p.Name, id)
		if st.Val != nil {
			if err := l.exprInto(st.Val, Place{Local: id}); err != nil {
				return err
			}
		}
		if ty != nil && ty.NeedsDrop() {
			l.registerDrop(id, ty)
		}
		return nil
	}
	return unsupportedf(st.Pat.Span(), "binding pattern %T", st.Pat)
}

// --- identifiers and literals --------------------------------------------

func (l *lowerer) identInto(x *ast.IdentExpr, dest Place) error {
	if id, ok := l.lookup(x.Name); ok {
		op, err := l.read(Place{Local: id}, x.Sp)
		if err != nil {
			return err
		}
		l.assign(dest, op, x.Sp)
		return nil
	}
	if c := l.info.Consts[x.Name]; c != nil {
		op, err := constOperand(c.Val, c.Ty, x.Sp)
		if err != nil {
			return err
		}
		l.assign(dest, op, x.Sp)
		return nil
	}
	if ctor := l.info.Ctors[x]; ctor != nil {
		// A bare unit variant name, such as `None` from the predeclared Option.
		l.emit(&Assign{Place: dest, RV: &Aggregate{Kind: AggEnum, Variant: ctor.Variant.Index, Ty: l.ty(x)}, Sp: x.Sp})
		return nil
	}
	if f := l.info.FnValues[x]; f != nil {
		return l.fnValueInto(f, dest, x.Sp)
	}
	if f := l.info.Fns[x.Name]; f != nil {
		return l.fnValueInto(f, dest, x.Sp)
	}
	return unsupportedf(x.Sp, "name `%s`", x.Name)
}

func (l *lowerer) fnValueInto(src *sema.Fn, dest Place, sp token.Span) error {
	fn := l.bySema[src]
	if fn == nil {
		return unsupportedf(sp, "function value `%s` (not reachable)", src.Name)
	}
	if isUnit(l.body.Ty(dest)) {
		return nil
	}
	l.emit(&Assign{Place: dest, RV: Use{Op: Operand{Kind: OpFn, Fn: fn, Ty: l.body.Ty(dest)}}, Sp: sp})
	return nil
}

func (l *lowerer) pathInto(x *ast.PathExpr, dest Place) error {
	if ctor := l.info.Ctors[x]; ctor != nil {
		// A unit variant: the aggregate sets the tag and carries no payload.
		l.emit(&Assign{Place: dest, RV: &Aggregate{Kind: AggEnum, Variant: ctor.Variant.Index, Ty: l.ty(x)}, Sp: x.Sp})
		return nil
	}
	if len(x.Segments) == 1 {
		if f := l.info.Fns[x.Segments[0]]; f != nil {
			return l.fnValueInto(f, dest, x.Sp)
		}
	}
	return unsupportedf(x.Sp, "path `%s`", strings.Join(x.Segments, "::"))
}

func (l *lowerer) literal(x *ast.LiteralExpr) (Const, error) {
	t := l.ty(x)
	switch x.Kind {
	case token.KindInteger:
		s := strings.ReplaceAll(x.Text, "_", "")
		if t != nil && t.Kind.IsFloat() {
			f, err := strconv.ParseFloat(s, 64)
			if err != nil {
				return Const{}, unsupportedf(x.Sp, "float literal `%s`", x.Text)
			}
			return Const{Kind: ConstFloat, F: f}, nil
		}
		neg := strings.HasPrefix(s, "-")
		if neg {
			s = s[1:]
		}
		base, digits := 10, s
		if len(s) > 2 && s[0] == '0' {
			switch s[1] {
			case 'x', 'X':
				base, digits = 16, s[2:]
			case 'o', 'O':
				base, digits = 8, s[2:]
			case 'b', 'B':
				base, digits = 2, s[2:]
			}
		}
		v, err := strconv.ParseUint(digits, base, 64)
		if err != nil {
			return Const{}, unsupportedf(x.Sp, "integer literal `%s`", x.Text)
		}
		if neg {
			v = uint64(-int64(v))
		}
		return Const{Kind: ConstInt, Bits: v}, nil
	case token.KindFloat:
		s := strings.ReplaceAll(x.Text, "_", "")
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return Const{}, unsupportedf(x.Sp, "float literal `%s`", x.Text)
		}
		return Const{Kind: ConstFloat, F: f}, nil
	case token.KindString, token.KindRawString:
		s := x.Text
		if x.Kind == token.KindString {
			if dec, ok := sema.DecodeString(s); ok {
				s = dec
			}
		}
		return StrConst(s), nil
	case token.KindChar, token.KindByteChar:
		dec, ok := sema.DecodeString(x.Text)
		if !ok || dec == "" {
			return Const{}, unsupportedf(x.Sp, "char literal")
		}
		for _, r := range dec {
			return CharConst(r), nil
		}
		return CharConst(0), nil
	case token.KindTrue:
		return BoolConst(true), nil
	case token.KindFalse:
		return BoolConst(false), nil
	}
	return Const{}, unsupportedf(x.Sp, "literal %s", x.Kind)
}

// constOperand lifts a checked constant into an operand.
func constOperand(v sema.ConstVal, ty *types.Type, sp token.Span) (Operand, error) {
	if ty == nil {
		return Operand{}, unsupportedf(sp, "constant without a type")
	}
	switch {
	case ty.Kind == types.Unit:
		return Operand{Kind: OpConst, Const: UnitConst(), Ty: ty}, nil
	case ty.Kind == types.Bool:
		return Operand{Kind: OpConst, Const: BoolConst(v.Bool), Ty: ty}, nil
	case ty.Kind == types.Char:
		return Operand{Kind: OpConst, Const: CharConst(rune(v.Int)), Ty: ty}, nil
	case ty.Kind == types.Str:
		return Operand{Kind: OpConst, Const: StrConst(v.Str), Ty: ty}, nil
	case ty.Kind.IsFloat():
		return Operand{Kind: OpConst, Const: Const{Kind: ConstFloat, F: v.Float}, Ty: ty}, nil
	case ty.Kind.IsInt():
		if v.Uint != 0 {
			return Operand{Kind: OpConst, Const: UintConst(v.Uint), Ty: ty}, nil
		}
		return Operand{Kind: OpConst, Const: IntConst(v.Int), Ty: ty}, nil
	}
	return Operand{}, unsupportedf(sp, "constant of type `%s`", ty)
}

// --- operators ------------------------------------------------------------

func (l *lowerer) unaryInto(x *ast.UnExpr, dest Place) error {
	switch x.Op {
	case token.KindMinus, token.KindBang:
		op, err := l.expr(x.X)
		if err != nil {
			return err
		}
		l.emit(&Assign{Place: dest, RV: &UnOp{Op: x.Op, X: op, Ty: l.ty(x)}, Sp: x.Sp})
		return nil
	case token.KindAmp:
		p, err := l.placeForRef(x.X)
		if err != nil {
			return err
		}
		rt := types.RefTo(l.body.Ty(p), x.Mut)
		l.emit(&Assign{Place: dest, RV: &Ref{Mut: x.Mut, Place: p, Ty: rt}, Sp: x.Sp})
		return nil
	case token.KindStar:
		p, ok, err := l.placeOf(x)
		if err != nil {
			return err
		}
		if !ok {
			return unsupportedf(x.Sp, "dereference in value position")
		}
		op, err := l.read(p, x.Sp)
		if err != nil {
			return err
		}
		l.assign(dest, op, x.Sp)
		return nil
	}
	return unsupportedf(x.Sp, "unary operator %s", x.Op)
}

// placeForRef returns a place to borrow: the place itself when the expression is
// one, otherwise a temporary holding its value.
func (l *lowerer) placeForRef(e ast.Expr) (Place, error) {
	if p, ok, err := l.placeOf(e); err != nil {
		return Place{}, err
	} else if ok {
		return p, nil
	}
	return l.tempFor(e)
}

func (l *lowerer) binaryInto(x *ast.BinExpr, dest Place) error {
	switch x.Op {
	case token.KindAmpAmp:
		return l.shortCircuit(x, dest, true)
	case token.KindPipePipe:
		return l.shortCircuit(x, dest, false)
	}
	lt, rt := l.ty(x.L), l.ty(x.R)
	if (lt != nil && lt.Kind == types.StringKind) || (rt != nil && rt.Kind == types.StringKind) {
		return l.stringConcatInto(x, dest)
	}
	if (lt != nil && (lt.Kind == types.VecKind || lt.Kind == types.MapKind)) ||
		(rt != nil && (rt.Kind == types.VecKind || rt.Kind == types.MapKind)) {
		return unsupportedf(x.Sp, "operator %s on collections", x.Op)
	}
	lop, err := l.expr(x.L)
	if err != nil {
		return err
	}
	rop, err := l.expr(x.R)
	if err != nil {
		return err
	}
	l.emit(&Assign{Place: dest, RV: &BinOp{Op: x.Op, L: lop, R: rop, Ty: l.ty(x)}, Sp: x.Sp})
	return nil
}

// shortCircuit lowers `a && b` / `a || b` to control flow: the right operand
// lives in a block only reachable when the left operand did not already decide
// the answer.
func (l *lowerer) shortCircuit(x *ast.BinExpr, dest Place, isAnd bool) error {
	lop, err := l.expr(x.L)
	if err != nil {
		return err
	}
	shortBlk := l.newBlock()
	rhsBlk := l.newBlock()
	join := l.newBlock()
	// For `&&` a false left operand decides the answer; for `||` a true one.
	shortVal := uint64(0)
	if !isAnd {
		shortVal = 1
	}
	l.setTerm(&SwitchInt{Discr: lop, Cases: []Case{{Val: shortVal, Target: shortBlk}}, Otherwise: rhsBlk, Sp: x.Sp})

	l.cur = shortBlk
	l.assign(dest, Operand{Kind: OpConst, Const: BoolConst(shortVal == 1), Ty: types.BoolT}, x.Sp)
	l.gotoBlock(join, x.Sp)

	l.cur = rhsBlk
	rop, err := l.expr(x.R)
	if err != nil {
		return err
	}
	l.assign(dest, rop, x.Sp)
	l.gotoBlock(join, x.Sp)

	l.cur = join
	return nil
}

// stringConcatInto models `s + x` / `s += x` as an operation over operands the
// backend lowers with its string runtime. MIR records only the operands and the
// result type; how a non-string side is rendered as text is a backend decision.
func (l *lowerer) stringConcatInto(x *ast.BinExpr, dest Place) error {
	lop, err := l.borrowOrCopy(x.L)
	if err != nil {
		return err
	}
	rop, err := l.borrowOrCopy(x.R)
	if err != nil {
		return err
	}
	l.emit(&Assign{Place: dest, RV: &BinOp{Op: token.KindPlus, L: lop, R: rop, Ty: l.ty(x)}, Sp: x.Sp})
	return nil
}

// borrowOrCopy reads an operand for printing or text building: values that own
// resources are borrowed so the caller keeps them.
func (l *lowerer) borrowOrCopy(e ast.Expr) (Operand, error) {
	t := l.ty(e)
	if t != nil && t.NeedsDrop() {
		p, err := l.placeForRef(e)
		if err != nil {
			return Operand{}, err
		}
		rt := types.RefTo(l.body.Ty(p), false)
		tmp := l.temp(rt)
		l.emit(&Assign{Place: Place{Local: tmp}, RV: &Ref{Place: p, Ty: rt}, Sp: e.Span()})
		return Copy(Place{Local: tmp}, rt), nil
	}
	return l.expr(e)
}

func castKind(from, to *types.Type) CastKind {
	if from == nil || to == nil {
		return CastNoop
	}
	if types.Equal(from, to) {
		return CastNoop
	}
	switch {
	case from.Kind == types.StringKind && to.Kind == types.Str:
		return CastString
	case from.Kind == types.Bool && to.Kind.IsInt(), from.Kind.IsInt() && to.Kind == types.Bool:
		return CastBoolInt
	case from.Kind == types.Char && to.Kind.IsInt(), from.Kind.IsInt() && to.Kind == types.Char:
		return CastCharInt
	case from.Kind.IsInt() && to.Kind.IsInt():
		return CastInt
	case from.Kind.IsFloat() && to.Kind.IsFloat():
		return CastFloat
	case from.Kind.IsInt() && to.Kind.IsFloat():
		return CastIntToFloat
	case from.Kind.IsFloat() && to.Kind.IsInt():
		return CastFloatToInt
	case from.Kind == types.Ptr || from.Kind == types.Ref || to.Kind == types.Ptr || to.Kind == types.Ref:
		return CastPtr
	}
	return CastNoop
}

func (l *lowerer) assignInto(x *ast.AssignExpr) error {
	place, ok, err := l.placeOf(x.Lhs)
	if err != nil {
		return err
	}
	if !ok {
		return unsupportedf(x.Lhs.Span(), "assignment to a non-place")
	}
	if x.Op == token.KindEq {
		// Evaluate the right side first, so `a[i] = a[i] + 1` reads before it
		// writes and `x = f(x)` sees the old value.
		tmp, err := l.tempFor(x.Rhs)
		if err != nil {
			return err
		}
		op, err := l.read(tmp, x.Rhs.Span())
		if err != nil {
			return err
		}
		l.assign(place, op, x.Sp)
		return nil
	}
	op := compoundBase(x.Op)
	if op == 0 {
		return unsupportedf(x.Sp, "assignment operator %s", x.Op)
	}
	pt := l.body.Ty(place)
	if pt != nil && pt.Kind == types.StringKind {
		cur, err := l.borrowOrCopy(x.Lhs)
		if err != nil {
			return err
		}
		rhs, err := l.borrowOrCopy(x.Rhs)
		if err != nil {
			return err
		}
		tmp := l.temp(pt)
		l.emit(&Assign{Place: Place{Local: tmp}, RV: &BinOp{Op: token.KindPlus, L: cur, R: rhs, Ty: pt}, Sp: x.Sp})
		res, err := l.read(Place{Local: tmp}, x.Sp)
		if err != nil {
			return err
		}
		l.assign(place, res, x.Sp)
		return nil
	}
	cur, err := l.read(place, x.Sp)
	if err != nil {
		return err
	}
	rhs, err := l.expr(x.Rhs)
	if err != nil {
		return err
	}
	tmp := l.temp(pt)
	l.emit(&Assign{Place: Place{Local: tmp}, RV: &BinOp{Op: op, L: cur, R: rhs, Ty: pt}, Sp: x.Sp})
	res, err := l.read(Place{Local: tmp}, x.Sp)
	if err != nil {
		return err
	}
	l.assign(place, res, x.Sp)
	return nil
}

func compoundBase(op token.Kind) token.Kind {
	switch op {
	case token.KindPlusEq:
		return token.KindPlus
	case token.KindMinusEq:
		return token.KindMinus
	case token.KindStarEq:
		return token.KindStar
	case token.KindSlashEq:
		return token.KindSlash
	case token.KindPercentEq:
		return token.KindPercent
	case token.KindAmpEq:
		return token.KindAmp
	case token.KindPipeEq:
		return token.KindPipe
	case token.KindCaretEq:
		return token.KindCaret
	case token.KindShlEq:
		return token.KindShl
	case token.KindShrEq:
		return token.KindShr
	}
	return 0
}

// --- control flow ---------------------------------------------------------

func (l *lowerer) ifInto(x *ast.IfExpr, dest Place) error {
	cond, err := l.expr(x.Cond)
	if err != nil {
		return err
	}
	thenB := l.newBlock()
	join := l.newBlock()
	// With no `else`, the false edge goes straight to the join and nothing is
	// written: a missing else branch is `()`.
	elseB := join
	if x.Else != nil {
		elseB = l.newBlock()
	}
	l.setTerm(&SwitchInt{Discr: cond, Cases: []Case{{Val: 1, Target: thenB}}, Otherwise: elseB, Sp: x.Sp})

	l.cur = thenB
	if err := l.blockInto(x.Then, dest); err != nil {
		return err
	}
	l.gotoBlock(join, x.Sp)

	if x.Else != nil {
		l.cur = elseB
		if err := l.exprInto(x.Else, dest); err != nil {
			return err
		}
		l.gotoBlock(join, x.Sp)
	}

	l.cur = join
	return nil
}

// writeUnit stores `()` into dest unless dest is itself unit.
func (l *lowerer) writeUnit(dest Place, sp token.Span) {
	if isUnit(l.body.Ty(dest)) {
		return
	}
	l.assign(dest, l.unitOperand(), sp)
}

func (l *lowerer) blockInto(b *ast.Block, dest Place) error {
	l.pushScope()
	for _, s := range b.Stmts {
		if err := l.stmt(s); err != nil {
			return err
		}
	}
	if b.Tail != nil {
		if err := l.exprInto(b.Tail, dest); err != nil {
			return err
		}
	} else {
		l.writeUnit(dest, b.Sp)
	}
	l.exitScope()
	return nil
}

func (l *lowerer) jump(x *ast.JumpExpr) error {
	switch x.Kind {
	case token.KindReturn:
		if x.Val != nil && !l.terminated() {
			if err := l.exprInto(x.Val, Place{Local: 0}); err != nil {
				return err
			}
		}
		l.unwindTo(0)
		l.setTerm(&Return{Sp: x.Sp})
		return nil
	case token.KindBreak:
		if len(l.loops) == 0 {
			return unsupportedf(x.Sp, "`break` outside a loop")
		}
		frame := l.loops[len(l.loops)-1]
		if x.Val != nil {
			if frame.result < 0 {
				return unsupportedf(x.Sp, "`break` with a value in a loop that yields none")
			}
			if err := l.exprInto(x.Val, Place{Local: frame.result}); err != nil {
				return err
			}
		}
		l.unwindTo(frame.depth)
		l.gotoBlock(frame.exit, x.Sp)
		return nil
	case token.KindContinue:
		if len(l.loops) == 0 {
			return unsupportedf(x.Sp, "`continue` outside a loop")
		}
		frame := l.loops[len(l.loops)-1]
		l.unwindTo(frame.depth)
		l.gotoBlock(frame.cont, x.Sp)
		return nil
	}
	return unsupportedf(x.Sp, "jump %s", x.Kind)
}

func (l *lowerer) loopInto(x *ast.LoopExpr, dest Place) error {
	var result LocalID = -1
	if t := l.ty(x); t != nil && !isUnit(t) {
		result = l.temp(t)
	}
	exit := l.newBlock()
	depth := len(l.scopes)

	var frame *loopFrame
	switch x.Kind {
	case token.KindLoop:
		body := l.newBlock()
		frame = &loopFrame{cont: body, exit: exit, result: result, depth: depth}
		l.gotoBlock(body, x.Sp)
		l.cur = body
		l.loops = append(l.loops, frame)
		if err := l.loopBody(x.Body, dest); err != nil {
			return err
		}
		l.gotoBlock(body, x.Sp) // back edge
		l.cur = exit

	case token.KindWhile, token.KindUntil:
		head := l.newBlock()
		body := l.newBlock()
		frame = &loopFrame{cont: head, exit: exit, result: result, depth: depth}
		l.gotoBlock(head, x.Sp)
		l.cur = head
		l.loops = append(l.loops, frame)
		cond, err := l.expr(x.Cond)
		if err != nil {
			return err
		}
		onTrue, onFalse := body, exit
		if x.Kind == token.KindUntil {
			onTrue, onFalse = exit, body
		}
		l.setTerm(&SwitchInt{Discr: cond, Cases: []Case{{Val: 1, Target: onTrue}}, Otherwise: onFalse, Sp: x.Sp})
		l.cur = body
		if err := l.loopBody(x.Body, dest); err != nil {
			return err
		}
		l.gotoBlock(head, x.Sp)
		l.cur = exit

	case token.KindFor:
		iter, ok := x.Iter.(*ast.BinExpr)
		if !ok || (iter.Op != token.KindDotDot && iter.Op != token.KindDotDotEq) {
			return unsupportedf(x.Sp, "`for` over a collection (only `for i in a..b` lowers so far)")
		}
		it := l.ty(iter.L)
		if it == nil {
			return unsupportedf(x.Sp, "range with no element type")
		}
		// Both bounds are evaluated once, before the loop.
		loOp, err := l.expr(iter.L)
		if err != nil {
			return err
		}
		hiOp, err := l.expr(iter.R)
		if err != nil {
			return err
		}
		idx := l.addLocal("", it, LocalTemp)
		l.assign(Place{Local: idx}, loOp, x.Sp)
		hi := l.temp(it)
		l.assign(Place{Local: hi}, hiOp, x.Sp)

		head := l.newBlock()
		body := l.newBlock()
		incr := l.newBlock()
		frame = &loopFrame{cont: incr, exit: exit, result: result, depth: depth}
		l.gotoBlock(head, x.Sp)
		l.cur = head
		l.loops = append(l.loops, frame)
		cmpOp := token.KindLt
		if iter.Op == token.KindDotDotEq {
			cmpOp = token.KindLe
		}
		cnd := l.temp(types.BoolT)
		l.emit(&Assign{Place: Place{Local: cnd}, RV: &BinOp{
			Op: cmpOp,
			L:  Copy(Place{Local: idx}, it),
			R:  Copy(Place{Local: hi}, it),
			Ty: types.BoolT,
		}, Sp: x.Sp})
		l.setTerm(&SwitchInt{Discr: Copy(Place{Local: cnd}, types.BoolT), Cases: []Case{{Val: 1, Target: body}}, Otherwise: exit, Sp: x.Sp})

		l.cur = body
		l.pushScope()
		if err := l.bindPattern(x.Pat, Place{Local: idx}, it, x.Sp); err != nil {
			return err
		}
		if err := l.blockInto(x.Body, dest); err != nil {
			return err
		}
		l.exitScope()
		l.gotoBlock(incr, x.Sp)

		l.cur = incr
		next := l.temp(it)
		l.emit(&Assign{Place: Place{Local: next}, RV: &BinOp{
			Op: token.KindPlus,
			L:  Copy(Place{Local: idx}, it),
			R:  Operand{Kind: OpConst, Const: IntConst(1), Ty: it},
			Ty: it,
		}, Sp: x.Sp})
		l.assign(Place{Local: idx}, Copy(Place{Local: next}, it), x.Sp)
		l.gotoBlock(head, x.Sp)
		l.cur = exit

	default:
		return unsupportedf(x.Sp, "loop kind %s", x.Kind)
	}
	l.loops = l.loops[:len(l.loops)-1]

	if result >= 0 {
		op, err := l.read(Place{Local: result}, x.Sp)
		if err != nil {
			return err
		}
		l.assign(dest, op, x.Sp)
	}
	return nil
}

// loopBody lowers a loop body in its own scope so per-iteration locals (and
// dropped temporaries) are released on every pass.
func (l *lowerer) loopBody(b *ast.Block, dest Place) error {
	return l.blockInto(b, dest)
}

// --- calls ----------------------------------------------------------------

// panicInto lowers the `panic(msg)` builtin. Control never returns, so the
// call's successor block is unreachable — that is what lets a `panic` stand in
// as an arm body of the match `unwrap`/`expect` desugar to.
func (l *lowerer) panicInto(x *ast.CallExpr) error {
	ops := make([]Operand, 0, len(x.Args))
	for _, a := range x.Args {
		op, err := l.expr(a)
		if err != nil {
			return err
		}
		ops = append(ops, op)
	}
	dead := l.newBlock()
	l.setTerm(&Call{Callee: FuncRef{Builtin: BnPanic}, Args: ops, Target: dead, Sp: x.Sp})
	l.cur = dead
	l.setTerm(&Unreachable{Sp: x.Sp})
	return nil
}

func (l *lowerer) callInto(x *ast.CallExpr, dest Place) error {
	if name, ok := calleeName(x.Callee); ok {
		switch name {
		case "print", "println":
			return l.printInto(x, name == "println")
		case "panic":
			return l.panicInto(x)
		case "len":
			return l.lenInto(x, dest)
		case "cstr", "from_cstr", "arg_count", "arg_at":
			return unsupportedf(x.Sp, "builtin `%s`", name)
		}
	}
	if ctor := l.info.Ctors[x]; ctor != nil {
		return l.enumCtorInto(x, ctor, l.ty(x), dest)
	}
	if st := l.info.TupleCtors[x]; st != nil {
		return l.structCtorInto(x.Args, st, l.ty(x), dest, x.Sp)
	}
	if p, ok := x.Callee.(*ast.PathExpr); ok && len(p.Segments) == 2 {
		switch p.Segments[0] {
		case "String", "Vec", "HashMap":
			// `String::from(..)`, `Vec::new()`, `HashMap::new()`: a runtime
			// intrinsic of the built-in owning type, not a Zexi function.
			ops := make([]Operand, 0, len(x.Args))
			for i, a := range x.Args {
				var op Operand
				var err error
				if p.Segments[0] == "String" && p.Segments[1] == "from" && i == 0 {
					op, err = l.stringViewOperand(a)
				} else {
					op, err = l.expr(a)
				}
				if err != nil || l.terminated() {
					return err
				}
				ops = append(ops, op)
			}
			return l.emitBuiltin(p.Segments[0]+"::"+p.Segments[1], ops, dest, l.ty(x), x.Sp)
		}
	}
	fn := l.info.Resolved[x]
	if fn == nil {
		return unsupportedf(x.Sp, "indirect call through a function value")
	}
	return l.directCallInto(fn, x.Args, dest, x.Sp, nil)
}

func calleeName(e ast.Expr) (string, bool) {
	switch x := e.(type) {
	case *ast.IdentExpr:
		return x.Name, true
	case *ast.PathExpr:
		if len(x.Segments) == 1 {
			return x.Segments[0], true
		}
	}
	return "", false
}

// directCallInto emits a Call terminator. recv, when non-nil, is the already
// evaluated receiver that occupies parameter slot 0 of a method.
func (l *lowerer) directCallInto(src *sema.Fn, args []ast.Expr, dest Place, sp token.Span, recv *Operand) error {
	fn := l.bySema[src]
	if fn == nil {
		return unsupportedf(sp, "call to `%s`, which is not part of the program", src.Name)
	}
	ops := make([]Operand, 0, len(args)+1)
	if recv != nil {
		ops = append(ops, *recv)
	}
	for _, a := range args {
		var op Operand
		var err error
		// The checker permits a String in a str argument position; this is a
		// borrow, not a move of the String into a differently typed parameter.
		i := len(ops)
		if i < len(src.Params) && src.Params[i] != nil && src.Params[i].Kind == types.Str {
			op, err = l.stringViewOperand(a)
		} else {
			op, err = l.expr(a)
		}
		if err != nil || l.terminated() {
			return err
		}
		ops = append(ops, op)
	}
	var destPtr *Place
	if !isUnit(src.Ret) && !isUnit(l.body.Ty(dest)) {
		d := dest
		destPtr = &d
	}
	next := l.newBlock()
	l.setTerm(&Call{Callee: FuncRef{Fn: fn}, Args: ops, Dest: destPtr, Target: next, Sp: sp})
	l.cur = next
	return nil
}

// builtinTypeName is the name a runtime intrinsic is spelled under. Generic
// arguments are dropped, because the intrinsic's operand types already say
// which instantiation it is.
func builtinTypeName(t *types.Type) string {
	switch t.Kind {
	case types.StringKind:
		return "String"
	case types.VecKind:
		return "Vec"
	case types.MapKind:
		return "HashMap"
	}
	return t.String()
}

// builtinMethodInto lowers a method on a built-in owning type to a runtime
// intrinsic call. MIR does not model `String`/`Vec`/`HashMap` internals: it names
// the operation (`String::push_str`, `Vec::push`, `HashMap::get`) and hands the
// receiver over as a reference, and each backend maps that name onto its
// runtime. That keeps the IR free of any one backend's implementation.
func (l *lowerer) builtinMethodInto(x *ast.MethodCallExpr, rt *types.Type, dest Place) error {
	recv, err := l.placeForRef(x.Recv)
	if err != nil || l.terminated() {
		return err
	}
	mut := true
	if rt.Kind == types.StringKind {
		// A source receiver may itself be &String; the intrinsic always takes
		// a reference to the String, not a reference to that reference.
		recv, _, err = l.autoDeref(recv, x.Sp)
		if err != nil {
			return err
		}
		switch x.Name {
		case "clear", "push", "push_str", "push_byte":
			mut = true
		default:
			mut = false
		}
	}
	ops := []Operand{l.refOperand(recv, mut, x.Sp)}
	wants := sema.StringMethodParams(x.Name)
	for i, a := range x.Args {
		var op Operand
		if rt.Kind == types.StringKind && i < len(wants) && wants[i] != nil && wants[i].Kind == types.Str {
			op, err = l.stringViewOperand(a)
		} else {
			op, err = l.expr(a)
		}
		if err != nil || l.terminated() {
			return err
		}
		ops = append(ops, op)
	}
	return l.emitBuiltin(builtinTypeName(rt)+"::"+x.Name, ops, dest, l.ty(x), x.Sp)
}

// stringViewOperand implements the checker's implicit String-to-str argument
// coercion. A String place stays owned by its caller; a temporary is kept alive
// for the call (or interpolation part) and is dropped in its enclosing scope.
func (l *lowerer) stringViewOperand(e ast.Expr) (Operand, error) {
	if t := l.ty(e); t == nil || t.Kind != types.StringKind {
		return l.expr(e)
	}
	recv, err := l.borrowOrCopy(e)
	if err != nil || l.terminated() {
		return Operand{}, err
	}
	view := Place{Local: l.temp(types.StrT)}
	if err := l.emitBuiltin(BnStringAsStr, []Operand{recv}, view, types.StrT, e.Span()); err != nil {
		return Operand{}, err
	}
	return Copy(view, types.StrT), nil
}

// refOperand materializes a reference to a place in a non-owning temporary.
func (l *lowerer) refOperand(place Place, mut bool, sp token.Span) Operand {
	ty := types.RefTo(l.body.Ty(place), mut)
	tmp := l.temp(ty)
	l.emit(&Assign{Place: Place{Local: tmp}, RV: &Ref{Mut: mut, Place: place, Ty: ty}, Sp: sp})
	return Copy(Place{Local: tmp}, ty)
}

// interpStrInto builds a String in evaluation order. The buffer has its own
// drop flag so an early return in a part releases it; on success it is moved to
// dest. Each part gets a short scope: a temporary String borrowed by that part
// is dropped after its bytes have been appended, not after the enclosing block.
func (l *lowerer) interpStrInto(x *ast.InterpStrExpr, dest Place) error {
	// A temporary destination is not initialized until the result is ready.
	// In particular, do not drop it if a part returns or breaks early.
	destFlag, hasDestFlag := l.flagOf(dest)
	if hasDestFlag {
		l.emit(&SetDropFlag{Flag: destFlag, Val: false, Sp: x.Sp})
	}
	buf := Place{Local: l.temp(types.StringT)}
	if err := l.emitBuiltin(BnStringNew, nil, buf, types.StringT, x.Sp); err != nil {
		return err
	}
	for _, part := range x.Parts {
		l.pushScope()
		if err := l.interpPartInto(part, buf); err != nil {
			return err
		}
		l.exitScope()
		if l.terminated() {
			return nil
		}
	}
	op, err := l.read(buf, x.Sp)
	if err != nil {
		return err
	}
	l.assign(dest, op, x.Sp)
	if hasDestFlag {
		l.emit(&SetDropFlag{Flag: destFlag, Val: true, Sp: x.Sp})
	}
	return nil
}

// interpPartInto appends one literal or printable expression, borrowing owning
// String operands instead of moving them out of their source places.
func (l *lowerer) interpPartInto(part ast.Expr, buf Place) error {
	if lit, ok := part.(*ast.LiteralExpr); ok && lit.Kind == token.KindString {
		c, err := l.literal(lit)
		if err != nil {
			return err
		}
		return l.appendTo(buf, BnStringPushStr, Operand{Kind: OpConst, Const: c, Ty: types.StrT}, lit.Sp)
	}

	t := l.ty(part)
	if t == nil || t.Kind == types.Invalid {
		return unsupportedf(part.Span(), "interpolated value without a type")
	}
	if t.Kind == types.StringKind {
		view, err := l.stringViewOperand(part)
		if err != nil || l.terminated() {
			return err
		}
		return l.appendTo(buf, BnStringPushStr, view, part.Span())
	}
	op, err := l.expr(part)
	if err != nil || l.terminated() {
		return err
	}
	switch {
	case t.Kind == types.Str:
		return l.appendTo(buf, BnStringPushStr, op, part.Span())
	case t.Kind == types.Char:
		return l.appendTo(buf, BnStringPush, op, part.Span())
	case t.Kind == types.Bool:
		return l.appendBool(buf, op, part.Span())
	case t.Kind.IsInt() && t.Kind.IsSigned():
		return l.appendNumber(buf, BnStringPushI64, op, types.I64T, part.Span())
	case t.Kind.IsInt():
		return l.appendNumber(buf, BnStringPushU64, op, types.Builtins["u64"], part.Span())
	case t.Kind.IsFloat():
		return l.appendNumber(buf, BnStringPushF64, op, types.F64T, part.Span())
	}
	return unsupportedf(part.Span(), "interpolating a value of type %s", t)
}

// appendNumber records the widening cast explicitly: changing an operand's
// type alone would lose signed extension and f32-to-f64 conversion.
func (l *lowerer) appendNumber(buf Place, builtin string, op Operand, to *types.Type, sp token.Span) error {
	if !types.Equal(op.Ty, to) {
		tmp := Place{Local: l.temp(to)}
		l.emit(&Assign{Place: tmp, RV: &Cast{Kind: castKind(op.Ty, to), X: op, From: op.Ty, To: to}, Sp: sp})
		op = Copy(tmp, to)
	}
	return l.appendTo(buf, builtin, op, sp)
}

func (l *lowerer) appendBool(buf Place, op Operand, sp token.Span) error {
	yes, no, join := l.newBlock(), l.newBlock(), l.newBlock()
	l.setTerm(&SwitchInt{Discr: op, Cases: []Case{{Val: 1, Target: yes}}, Otherwise: no, Sp: sp})
	l.cur = yes
	if err := l.appendTo(buf, BnStringPushStr, Operand{Kind: OpConst, Const: StrConst("true"), Ty: types.StrT}, sp); err != nil {
		return err
	}
	l.gotoBlock(join, sp)
	l.cur = no
	if err := l.appendTo(buf, BnStringPushStr, Operand{Kind: OpConst, Const: StrConst("false"), Ty: types.StrT}, sp); err != nil {
		return err
	}
	l.gotoBlock(join, sp)
	l.cur = join
	return nil
}

func (l *lowerer) appendTo(buf Place, builtin string, arg Operand, sp token.Span) error {
	recv := l.refOperand(buf, true, sp)
	return l.emitBuiltin(builtin, []Operand{recv, arg}, Place{}, types.UnitT, sp)
}

// emitBuiltin emits a call to a runtime intrinsic, continuing at a fresh block.
func (l *lowerer) emitBuiltin(name string, ops []Operand, dest Place, retTy *types.Type, sp token.Span) error {
	if l.terminated() {
		return nil
	}
	var destPtr *Place
	if !isUnit(retTy) && !isUnit(l.body.Ty(dest)) {
		d := dest
		destPtr = &d
	}
	next := l.newBlock()
	l.setTerm(&Call{Callee: FuncRef{Builtin: name}, Args: ops, Dest: destPtr, Target: next, Sp: sp})
	l.cur = next
	return nil
}

// printInto lowers `print`/`println`. Arguments are read, never consumed: an
// owning value is borrowed, so `println(s)` leaves `s` usable.
func (l *lowerer) printInto(x *ast.CallExpr, newline bool) error {
	ops := make([]Operand, 0, len(x.Args))
	for _, a := range x.Args {
		op, err := l.borrowOrCopy(a)
		if err != nil || l.terminated() {
			return err
		}
		ops = append(ops, op)
	}
	builtin := BnPrint
	if newline {
		builtin = BnPrintln
	}
	next := l.newBlock()
	l.setTerm(&Call{Callee: FuncRef{Builtin: builtin}, Args: ops, Target: next, Sp: x.Sp})
	l.cur = next
	return nil
}

func (l *lowerer) lenInto(x *ast.CallExpr, dest Place) error {
	if len(x.Args) != 1 {
		return unsupportedf(x.Sp, "`len` with %d arguments", len(x.Args))
	}
	p, err := l.placeForRef(x.Args[0])
	if err != nil {
		return err
	}
	l.emit(&Assign{Place: dest, RV: &Len{Place: p, Ty: l.ty(x)}, Sp: x.Sp})
	return nil
}

// preludeTagInto lowers `x.is_ok()` and friends to a comparison of the enum's
// tag against the variant index.
func (l *lowerer) preludeTagInto(x *ast.MethodCallExpr, variant string, dest Place) error {
	rt := l.info.TypeOf(x.Recv)
	base := rt
	for base != nil && (base.Kind == types.Ref || base.Kind == types.Ptr) {
		base = base.Elem
	}
	if base == nil || base.Kind != types.EnumKind || base.En == nil {
		return unsupportedf(x.Sp, "tag test on a non-enum receiver")
	}
	v, ok := base.En.Variant(variant)
	if !ok {
		return unsupportedf(x.Sp, "tag test for variant `%s`", variant)
	}
	place, ok, err := l.placeOf(x.Recv)
	if err != nil {
		return err
	}
	if !ok {
		// A computed receiver still needs a place to read the tag from.
		tmp := l.temp(rt)
		if err := l.exprInto(x.Recv, Place{Local: tmp}); err != nil {
			return err
		}
		place = Place{Local: tmp}
	}
	disc := l.temp(types.UsizeT)
	l.emit(&Assign{Place: Place{Local: disc}, RV: &Discriminant{Place: place, Ty: types.UsizeT}, Sp: x.Sp})
	l.emit(&Assign{Place: dest, RV: &BinOp{
		Op: token.KindEqEq,
		L:  Copy(Place{Local: disc}, types.UsizeT),
		R:  Operand{Kind: OpConst, Const: IntConst(int64(v.Index)), Ty: types.UsizeT},
		Ty: types.BoolT,
	}, Sp: x.Sp})
	return nil
}

// methodInto lowers `recv.name(args)`. The receiver is argument 0; a `&self` or
// `&mut self` receiver is passed as a reference, borrowing the place when there
// is one and a temporary holding the value otherwise.
func (l *lowerer) methodInto(x *ast.MethodCallExpr, dest Place) error {
	fn := l.info.Resolved[x]
	if fn == nil {
		if rt := l.ty(x.Recv); rt != nil {
			// Methods are found through `&T` as well as `T`.
			for rt.Kind == types.Ref || rt.Kind == types.Ptr {
				rt = rt.Elem
			}
			if rt != nil {
				switch rt.Kind {
				case types.StringKind, types.VecKind, types.MapKind:
					return l.builtinMethodInto(x, rt, dest)
				}
			}
		}
		// `len()` is a builtin on arrays and `str`, not a declared method, so
		// the checker leaves it unresolved.
		if x.Name == "len" && len(x.Args) == 0 {
			return l.lenInto(&ast.CallExpr{Callee: x.Recv, Args: []ast.Expr{x.Recv}, Sp: x.Sp}, dest)
		}
		return unsupportedf(x.Sp, "unresolved method call `%s`", x.Name)
	}
	if fn.Self == nil {
		return unsupportedf(x.Sp, "method `%s` without a receiver", x.Name)
	}
	var recv Operand
	if !fn.Self.ByRef {
		op, err := l.expr(x.Recv)
		if err != nil {
			return err
		}
		recv = op
	} else {
		p, err := l.placeForRef(x.Recv)
		if err != nil {
			return err
		}
		rt := types.RefTo(l.body.Ty(p), fn.Self.Mut)
		tmp := l.temp(rt)
		l.emit(&Assign{Place: Place{Local: tmp}, RV: &Ref{Mut: fn.Self.Mut, Place: p, Ty: rt}, Sp: x.Sp})
		recv = Copy(Place{Local: tmp}, rt)
	}
	return l.directCallInto(fn, x.Args, dest, x.Sp, &recv)
}

// --- aggregates -----------------------------------------------------------

func (l *lowerer) structLitInto(x *ast.StructLitExpr, dest Place) error {
	if ctor := l.info.Ctors[x]; ctor != nil {
		ops, err := l.fieldOperands(x.Fields, ctor.Variant.Fields, x.Sp)
		if err != nil {
			return err
		}
		l.emit(&Assign{Place: dest, RV: &Aggregate{Kind: AggEnum, Variant: ctor.Variant.Index, Ops: ops, Ty: l.ty(x)}, Sp: x.Sp})
		return nil
	}
	t := l.ty(x)
	if t == nil || t.Kind != types.StructKind || t.St == nil {
		return unsupportedf(x.Sp, "struct literal of `%s`", t)
	}
	ops, err := l.fieldOperands(x.Fields, t.St.Fields, x.Sp)
	if err != nil {
		return err
	}
	l.emit(&Assign{Place: dest, RV: &Aggregate{Kind: AggStruct, Ops: ops, Ty: t}, Sp: x.Sp})
	return nil
}

func (l *lowerer) structCtorInto(args []ast.Expr, st *types.Struct, ty *types.Type, dest Place, sp token.Span) error {
	ops := make([]Operand, 0, len(args))
	for i, a := range args {
		op, err := l.expr(a)
		if err != nil {
			return err
		}
		if i < len(st.Fields) {
			op = retype(op, st.Fields[i].Ty)
		}
		ops = append(ops, op)
	}
	l.emit(&Assign{Place: dest, RV: &Aggregate{Kind: AggStruct, Ops: ops, Ty: ty}, Sp: sp})
	return nil
}

func retype(op Operand, ty *types.Type) Operand {
	if op.Ty == nil && ty != nil {
		op.Ty = ty
	}
	return op
}

// fieldOperands orders struct-literal initializers by declaration order, since
// the source may write them in any order.
func (l *lowerer) fieldOperands(inits []ast.FieldInit, fields []types.Field, sp token.Span) ([]Operand, error) {
	byName := make(map[string]ast.Expr, len(inits))
	for _, fi := range inits {
		byName[fi.Name] = fi.Val
	}
	ops := make([]Operand, 0, len(fields))
	for _, f := range fields {
		v, ok := byName[f.Name]
		if !ok {
			return nil, unsupportedf(sp, "struct literal missing field `%s`", f.Name)
		}
		op, err := l.expr(v)
		if err != nil {
			return nil, err
		}
		ops = append(ops, retype(op, f.Ty))
	}
	return ops, nil
}

func (l *lowerer) enumCtorInto(x *ast.CallExpr, ctor *sema.Ctor, ty *types.Type, dest Place) error {
	v := ctor.Variant
	ops := make([]Operand, 0, len(x.Args))
	for i, a := range x.Args {
		op, err := l.expr(a)
		if err != nil {
			return err
		}
		if i < len(v.Fields) {
			op = retype(op, v.Fields[i].Ty)
		}
		ops = append(ops, op)
	}
	l.emit(&Assign{Place: dest, RV: &Aggregate{Kind: AggEnum, Variant: v.Index, Ops: ops, Ty: ty}, Sp: x.Sp})
	return nil
}

func (l *lowerer) arrayLitInto(x *ast.ArrayLitExpr, dest Place) error {
	t := l.ty(x)
	if t == nil || t.Kind != types.Array {
		return unsupportedf(x.Sp, "array literal of `%s`", t)
	}
	if x.Repeat {
		count, err := l.constUint(x.Count)
		if err != nil {
			return err
		}
		op, err := l.expr(x.Value)
		if err != nil {
			return err
		}
		l.emit(&Assign{Place: dest, RV: &Repeat{Op: op, Count: count, Ty: t}, Sp: x.Sp})
		return nil
	}
	if int64(len(x.Elems)) != t.Len {
		return unsupportedf(x.Sp, "array literal length mismatch")
	}
	ops := make([]Operand, 0, len(x.Elems))
	for _, e := range x.Elems {
		op, err := l.expr(e)
		if err != nil {
			return err
		}
		ops = append(ops, retype(op, t.Elem))
	}
	l.emit(&Assign{Place: dest, RV: &Aggregate{Kind: AggArray, Ops: ops, Ty: t}, Sp: x.Sp})
	return nil
}

// constUint reads a constant integer expression: a literal, or a `const` name.
func (l *lowerer) constUint(e ast.Expr) (uint64, error) {
	if lit, ok := e.(*ast.LiteralExpr); ok {
		c, err := l.literal(lit)
		if err != nil {
			return 0, err
		}
		return c.Bits, nil
	}
	if id, ok := e.(*ast.IdentExpr); ok {
		if c := l.info.Consts[id.Name]; c != nil {
			return uint64(c.Val.Int), nil
		}
	}
	return 0, unsupportedf(e.Span(), "non-constant array repeat count")
}

// --- match ----------------------------------------------------------------

// matchInto lowers a match over a scrutinee temporary.
//
// When every arm is a plain variant pattern (with no payload bindings) and no
// arm has a guard, the whole match becomes one SwitchInt on the discriminant,
// which is the shape a backend wants. Otherwise each arm gets a test block that
// branches to its body or to the next arm's test.
func (l *lowerer) matchInto(x *ast.MatchExpr, dest Place) error {
	scrutTy := l.ty(x.Scrutinee)
	// The scrutinee gets its own local without the automatic drop registration
	// of temp(), because an arm may move a payload out of it and the residual
	// must then not be dropped again.
	scrutLocal := l.addLocal("", scrutTy, LocalTemp)
	if err := l.exprInto(x.Scrutinee, Place{Local: scrutLocal}); err != nil {
		return err
	}
	if scrutTy != nil && scrutTy.NeedsDrop() {
		l.registerDrop(scrutLocal, scrutTy)
	}
	scrut := Place{Local: scrutLocal}
	exit := l.newBlock()
	if switchable(x, scrutTy) {
		if err := l.matchSwitch(x, scrut, scrutTy, dest, exit); err != nil {
			return err
		}
	} else if err := l.matchChain(x, scrut, scrutTy, dest, exit); err != nil {
		return err
	}
	l.cur = exit
	return nil
}

// switchable reports whether the arms are a pure variant dispatch.
func switchable(x *ast.MatchExpr, scrutTy *types.Type) bool {
	if scrutTy == nil || scrutTy.Kind != types.EnumKind {
		return false
	}
	for _, arm := range x.Arms {
		if arm.Guard != nil {
			return false
		}
		switch p := arm.Pat.(type) {
		case *ast.WildcardPat:
		case *ast.BindPat:
			// A catch-all binding arm is the switch's default target.
		case *ast.PathPat:
			// Payload bindings are fine: the switch picks the arm and the
			// binding happens inside it.
		case *ast.OrPat:
			for _, alt := range p.Alts {
				if _, ok := alt.(*ast.PathPat); !ok {
					return false
				}
			}
		default:
			return false
		}
	}
	return true
}

func (l *lowerer) matchSwitch(x *ast.MatchExpr, scrut Place, scrutTy *types.Type, dest Place, exit BlockID) error {
	bodies := make([]BlockID, len(x.Arms))
	for i := range x.Arms {
		bodies[i] = l.newBlock()
	}
	// The default target: a catch-all arm if there is one, otherwise a block
	// the checker proved unreachable (the match is exhaustive). It is only
	// allocated when no arm supplies one, so no block is left dangling.
	def := BlockID(-1)
	hasDefault := false
	var cases []Case
	seen := map[int]bool{}
	for i, arm := range x.Arms {
		switch p := arm.Pat.(type) {
		case *ast.WildcardPat:
			def, hasDefault = bodies[i], true
		case *ast.BindPat:
			def, hasDefault = bodies[i], true
		case *ast.OrPat:
			for _, alt := range p.Alts {
				pp, ok := alt.(*ast.PathPat)
				if !ok {
					return unsupportedf(alt.Span(), "or-pattern alternative in a variant switch")
				}
				v, ok := variantIndex(scrutTy, pp)
				if !ok {
					return unsupportedf(pp.Sp, "variant `%s`", strings.Join(pp.Path, "::"))
				}
				if !seen[v] {
					seen[v] = true
					cases = append(cases, Case{Val: uint64(v), Target: bodies[i]})
				}
			}
		case *ast.PathPat:
			v, ok := variantIndex(scrutTy, p)
			if !ok {
				return unsupportedf(p.Sp, "variant `%s`", strings.Join(p.Path, "::"))
			}
			if !seen[v] {
				seen[v] = true
				cases = append(cases, Case{Val: uint64(v), Target: bodies[i]})
			}
		default:
			return unsupportedf(arm.Pat.Span(), "pattern %T in a variant switch", arm.Pat)
		}
	}
	sort.SliceStable(cases, func(i, j int) bool { return cases[i].Val < cases[j].Val })
	if !hasDefault {
		def = l.newBlock()
	}
	disc := l.temp(types.UsizeT)
	l.emit(&Assign{Place: Place{Local: disc}, RV: &Discriminant{Place: scrut, Ty: types.UsizeT}, Sp: x.Sp})
	l.setTerm(&SwitchInt{Discr: Copy(Place{Local: disc}, types.UsizeT), Cases: cases, Otherwise: def, Sp: x.Sp})

	for i, arm := range x.Arms {
		l.cur = bodies[i]
		depth := len(l.scopes)
		l.pushScope()
		if err := l.bindOrCheck(arm.Pat, scrut, scrutTy, arm.Sp); err != nil {
			return err
		}
		if err := l.exprInto(arm.Body, dest); err != nil {
			return err
		}
		l.unwindTo(depth)
		l.scopes = l.scopes[:depth]
		l.gotoBlock(exit, arm.Sp)
	}
	if !hasDefault {
		l.cur = def
		l.setTerm(&Unreachable{Sp: x.Sp})
	}
	return nil
}

func variantIndex(scrutTy *types.Type, p *ast.PathPat) (int, bool) {
	if scrutTy == nil || scrutTy.En == nil || len(p.Path) == 0 {
		return 0, false
	}
	v, ok := scrutTy.En.Variant(p.Path[len(p.Path)-1])
	if !ok {
		return 0, false
	}
	return v.Index, true
}

func (l *lowerer) matchChain(x *ast.MatchExpr, scrut Place, scrutTy *types.Type, dest Place, exit BlockID) error {
	next := l.newBlock()
	l.gotoBlock(next, x.Sp)
	for _, arm := range x.Arms {
		l.cur = next
		body := l.newBlock()
		fail := l.newBlock()
		cond, err := l.patternCond(arm.Pat, scrut, scrutTy)
		if err != nil {
			return err
		}
		l.setTerm(&SwitchInt{Discr: cond, Cases: []Case{{Val: 1, Target: body}}, Otherwise: fail, Sp: arm.Sp})

		l.cur = body
		depth := len(l.scopes)
		l.pushScope()
		if err := l.bindOrCheck(arm.Pat, scrut, scrutTy, arm.Sp); err != nil {
			return err
		}
		if arm.Guard != nil {
			g, err := l.expr(arm.Guard)
			if err != nil {
				return err
			}
			okBlk := l.newBlock()
			guardFail := l.newBlock()
			l.setTerm(&SwitchInt{Discr: g, Cases: []Case{{Val: 1, Target: okBlk}}, Otherwise: guardFail, Sp: arm.Sp})
			// The guard failed: release the arm's bindings, then try the next arm.
			l.cur = guardFail
			l.unwindTo(depth)
			l.scopes = l.scopes[:depth]
			l.gotoBlock(fail, arm.Sp)
			// Re-enter the arm's scope for the body.
			l.scopes = append(l.scopes, &scope{names: map[string]LocalID{}})
			l.cur = okBlk
			if err := l.bindOrCheck(arm.Pat, scrut, scrutTy, arm.Sp); err != nil {
				return err
			}
		}
		if err := l.exprInto(arm.Body, dest); err != nil {
			return err
		}
		l.unwindTo(depth)
		l.scopes = l.scopes[:depth]
		l.gotoBlock(exit, arm.Sp)
		next = fail
	}
	l.cur = next
	l.setTerm(&Unreachable{Sp: x.Sp})
	return nil
}

// patternCond builds a bool operand that is true when p matches the scrutinee.
func (l *lowerer) patternCond(p ast.Pattern, scrut Place, scrutTy *types.Type) (Operand, error) {
	sp := p.Span()
	switch pt := p.(type) {
	case *ast.WildcardPat:
		return Operand{Kind: OpConst, Const: BoolConst(true), Ty: types.BoolT}, nil
	case *ast.BindPat:
		return Operand{Kind: OpConst, Const: BoolConst(true), Ty: types.BoolT}, nil
	case *ast.LitPat:
		lit := &ast.LiteralExpr{Kind: pt.Kind, Text: pt.Text, Sp: pt.Sp}
		c, err := l.literal(lit)
		if err != nil {
			return Operand{}, err
		}
		val, err := l.read(scrut, sp)
		if err != nil {
			return Operand{}, err
		}
		return l.binOp(token.KindEqEq, val, Operand{Kind: OpConst, Const: c, Ty: scrutTy}, types.BoolT, sp), nil
	case *ast.RangePat:
		val, err := l.read(scrut, sp)
		if err != nil {
			return Operand{}, err
		}
		lo, err := l.literal(&ast.LiteralExpr{Kind: pt.Lo.Kind, Text: pt.Lo.Text, Sp: pt.Lo.Sp})
		if err != nil {
			return Operand{}, err
		}
		hi, err := l.literal(&ast.LiteralExpr{Kind: pt.Hi.Kind, Text: pt.Hi.Text, Sp: pt.Hi.Sp})
		if err != nil {
			return Operand{}, err
		}
		lower := l.binOp(token.KindGe, val, Operand{Kind: OpConst, Const: lo, Ty: scrutTy}, types.BoolT, sp)
		upperOp := token.KindLt
		if pt.Inclusive {
			upperOp = token.KindLe
		}
		upper := l.binOp(upperOp, val, Operand{Kind: OpConst, Const: hi, Ty: scrutTy}, types.BoolT, sp)
		// Both sides are already evaluated, so this is a plain logical and.
		return l.binOp(token.KindAmpAmp, lower, upper, types.BoolT, sp), nil
	case *ast.PathPat:
		if scrutTy != nil && scrutTy.Kind == types.EnumKind {
			v, ok := variantIndex(scrutTy, pt)
			if !ok {
				return Operand{}, unsupportedf(pt.Sp, "variant `%s`", strings.Join(pt.Path, "::"))
			}
			disc := l.temp(types.UsizeT)
			l.emit(&Assign{Place: Place{Local: disc}, RV: &Discriminant{Place: scrut, Ty: types.UsizeT}, Sp: sp})
			return l.binOp(token.KindEqEq,
				Copy(Place{Local: disc}, types.UsizeT),
				Operand{Kind: OpConst, Const: IntConst(int64(v)), Ty: types.UsizeT},
				types.BoolT, sp), nil
		}
		if len(pt.Path) == 1 && (pt.Path[0] == "true" || pt.Path[0] == "false") {
			val, err := l.read(scrut, sp)
			if err != nil {
				return Operand{}, err
			}
			return l.binOp(token.KindEqEq, val,
				Operand{Kind: OpConst, Const: BoolConst(pt.Path[0] == "true"), Ty: types.BoolT},
				types.BoolT, sp), nil
		}
		return Operand{}, unsupportedf(sp, "pattern path `%s`", strings.Join(pt.Path, "::"))
	case *ast.OrPat:
		var acc Operand
		have := false
		for _, alt := range pt.Alts {
			c, err := l.patternCond(alt, scrut, scrutTy)
			if err != nil {
				return Operand{}, err
			}
			if !have {
				acc, have = c, true
				continue
			}
			acc = l.binOp(token.KindPipePipe, acc, c, types.BoolT, sp)
		}
		if !have {
			return Operand{Kind: OpConst, Const: BoolConst(false), Ty: types.BoolT}, nil
		}
		return acc, nil
	}
	return Operand{}, unsupportedf(sp, "pattern %T", p)
}

// binOp emits a binary operation into a fresh temporary and returns it.
func (l *lowerer) binOp(op token.Kind, a, b Operand, ty *types.Type, sp token.Span) Operand {
	tmp := l.temp(ty)
	l.emit(&Assign{Place: Place{Local: tmp}, RV: &BinOp{Op: op, L: a, R: b, Ty: ty}, Sp: sp})
	return Copy(Place{Local: tmp}, ty)
}

// bindOrCheck binds a pattern's variables from a scrutinee known to match.
func (l *lowerer) bindOrCheck(p ast.Pattern, scrut Place, scrutTy *types.Type, sp token.Span) error {
	switch pt := p.(type) {
	case *ast.WildcardPat:
		return nil
	case *ast.BindPat:
		return l.bindPattern(pt, scrut, l.body.Ty(scrut), sp)
	case *ast.PathPat:
		if scrutTy == nil || scrutTy.Kind != types.EnumKind {
			return nil
		}
		v, ok := variantIndex(scrutTy, pt)
		if !ok {
			return nil
		}
		variant, _ := scrutTy.En.Variant(variantName(scrutTy.En, v))
		for i, sub := range pt.Sub {
			if i >= len(variant.Fields) {
				break
			}
			field := Place{Local: scrut.Local, Proj: append(append([]Proj(nil), scrut.Proj...),
				DowncastProj(v), FieldProj(i, variant.Fields[i].Name, variant.Fields[i].Ty))}
			if err := l.bindOrCheck(sub, field, variant.Fields[i].Ty, sp); err != nil {
				return err
			}
		}
		for _, fp := range pt.Fields {
			idx, fty, ok := fieldIndex(&types.Struct{Fields: variant.Fields, Tuple: variant.Tuple}, fp.Name)
			if !ok {
				continue
			}
			field := Place{Local: scrut.Local, Proj: append(append([]Proj(nil), scrut.Proj...),
				DowncastProj(v), FieldProj(idx, fp.Name, fty))}
			// `Variant { field }` is shorthand for `Variant { field: field }`;
			// the parser leaves Pat nil in that form.
			pat := fp.Pat
			if pat == nil {
				pat = &ast.BindPat{Name: fp.Name, Sp: fp.Sp}
			}
			if err := l.bindOrCheck(pat, field, fty, sp); err != nil {
				return err
			}
		}
		return nil
	case *ast.LitPat, *ast.RangePat:
		// A literal or range pattern binds nothing; its test already ran.
		return nil
	case *ast.OrPat:
		// Alternatives cannot bind variables (the checker rejects that).
		return nil
	case *ast.TuplePat:
		return nil
	}
	return unsupportedf(sp, "binding pattern %T", p)
}

func variantName(e *types.Enum, idx int) string {
	for _, v := range e.Variants {
		if v.Index == idx {
			return v.Name
		}
	}
	return ""
}

// moveBinding reads a pattern binding out of a place. Moving an owning value out
// of a field of the match scrutinee is a partial move: the remaining payload
// must still be dropped, so it is only allowed when this field is the variant's
// *only* owning field, in which case taking it hands over the whole obligation
// and the scrutinee's own drop is switched off.
func (l *lowerer) moveBinding(src Place, ty *types.Type, sp token.Span) (Operand, error) {
	if ty == nil {
		return Operand{}, unsupportedf(sp, "binding without a type")
	}
	if src.IsLocal() || !ty.NeedsDrop() {
		return l.read(src, sp)
	}
	if !l.soleOwningField(src) {
		return Operand{}, unsupportedf(sp, "binding by value out of a variant with more than one owning field")
	}
	if flag, ok := l.flagOf(Place{Local: src.Local}); ok {
		l.emit(&SetDropFlag{Flag: flag, Val: false, Sp: sp})
	}
	return Move(src, ty), nil
}

// soleOwningField reports whether the projected place is the only field of its
// variant that needs dropping.
func (l *lowerer) soleOwningField(src Place) bool {
	if len(src.Proj) < 2 {
		return false
	}
	base := l.body.Locals[src.Local].Ty
	if base == nil || base.Kind != types.EnumKind || base.En == nil {
		return false
	}
	// Find the variant this projection descends into.
	variant := -1
	for i := len(src.Proj) - 1; i >= 0; i-- {
		if src.Proj[i].Kind == ProjDowncast {
			variant = src.Proj[i].Idx
			break
		}
	}
	if variant < 0 {
		return false
	}
	owning := 0
	for _, v := range base.En.Variants {
		if v.Index != variant {
			continue
		}
		for _, f := range v.Fields {
			if f.Ty.NeedsDrop() {
				owning++
			}
		}
	}
	return owning == 1
}

// bindPattern binds a single name (a `let`, a `for` variable, or a match
// binding).
func (l *lowerer) bindPattern(p ast.Pattern, src Place, ty *types.Type, sp token.Span) error {
	switch pt := p.(type) {
	case *ast.WildcardPat:
		return nil
	case *ast.BindPat:
		if pt.Name == "_" {
			return nil
		}
		id := l.addLocal(pt.Name, ty, LocalUser)
		l.body.Locals[id].Mut = pt.Mut
		op, err := l.moveBinding(src, ty, sp)
		if err != nil {
			return err
		}
		l.assign(Place{Local: id}, op, sp)
		if ty != nil && ty.NeedsDrop() {
			l.registerDrop(id, ty)
		}
		l.declare(pt.Name, id)
		return nil
	}
	return unsupportedf(sp, "binding pattern %T", p)
}
