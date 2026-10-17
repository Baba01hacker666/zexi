package sema

import (
	"zexi/internal/ast"
	"zexi/internal/token"
)

// This file implements Zexi's move checker: a flow-sensitive pass that rejects
// use-after-move for non-Copy values. It is deliberately conservative at
// control-flow joins (a value moved on *any* path is considered moved
// afterwards), which can over-report but never accepts a real use-after-move.

type moveState struct {
	val bool // moved?
	had bool // was the name tracked before this scope?
}

type mover struct {
	c      *checker
	moved  map[string]bool
	frames []map[string]moveState
}

// checkOwnership runs the move checker over every function with a body.
func (c *checker) checkOwnership() {
	for _, fn := range c.info.FnOrder {
		if fn.Decl.Body == nil {
			continue
		}
		m := &mover{c: c, moved: map[string]bool{}}
		m.enter()
		for _, p := range fn.Decl.Params {
			if p.Name != "" {
				m.declare(p.Name)
			}
		}
		m.block(fn.Decl.Body)
		m.exit()
	}
}

// --- scope bookkeeping ----------------------------------------------------

func (m *mover) enter() { m.frames = append(m.frames, map[string]moveState{}) }

func (m *mover) exit() {
	f := m.frames[len(m.frames)-1]
	for name, st := range f {
		if st.had {
			m.moved[name] = st.val
		} else {
			delete(m.moved, name)
		}
	}
	m.frames = m.frames[:len(m.frames)-1]
}

func (m *mover) declare(name string) {
	if name == "" || name == "_" {
		return
	}
	f := m.frames[len(m.frames)-1]
	if _, ok := f[name]; !ok {
		val, had := m.moved[name]
		f[name] = moveState{val, had}
	}
	m.moved[name] = false
}

func (m *mover) snapshot() map[string]bool {
	cp := make(map[string]bool, len(m.moved))
	for k, v := range m.moved {
		cp[k] = v
	}
	return cp
}

func (m *mover) restore(s map[string]bool) { m.moved = s }

// union joins two paths: a value is moved if it was moved on either path.
func unionMoves(a, b map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k, v := range a {
		if v {
			out[k] = true
		}
	}
	for k, v := range b {
		if v {
			out[k] = true
		}
	}
	return out
}

// --- patterns -------------------------------------------------------------

func (m *mover) declarePat(p ast.Pattern) {
	switch pt := p.(type) {
	case *ast.BindPat:
		m.declare(pt.Name)
	case *ast.PathPat:
		for _, sub := range pt.Sub {
			m.declarePat(sub)
		}
		for _, fp := range pt.Fields {
			if fp.Pat != nil {
				m.declarePat(fp.Pat)
			} else {
				m.declare(fp.Name)
			}
		}
	}
}

// --- statements -----------------------------------------------------------

func (m *mover) block(b *ast.Block) {
	if b == nil {
		return
	}
	m.enter()
	for _, s := range b.Stmts {
		m.stmt(s)
	}
	if b.Tail != nil {
		m.consume(b.Tail)
	}
	m.exit()
}

func (m *mover) stmt(s ast.Stmt) {
	switch st := s.(type) {
	case *ast.LetStmt:
		if st.Val != nil {
			m.consume(st.Val)
		}
		m.declarePat(st.Pat)
	case *ast.ExprStmt:
		m.use(st.X)
	}
}

// --- expressions ----------------------------------------------------------

// use evaluates e as a read. Moved variables used here are errors.
func (m *mover) use(e ast.Expr) {
	switch x := e.(type) {
	case nil:
		return
	case *ast.IdentExpr:
		if m.moved[x.Name] {
			m.c.errf(x.Sp, "E0382", "use of moved value `%s`", x.Name)
			m.moved[x.Name] = false // suppress cascading errors
		}
	case *ast.PathExpr, *ast.LiteralExpr:
	case *ast.BinExpr:
		m.use(x.L)
		m.use(x.R)
	case *ast.UnExpr:
		m.use(x.X)
	case *ast.CastExpr:
		m.use(x.X)
	case *ast.FieldExpr:
		m.use(x.Recv)
	case *ast.IndexExpr:
		m.use(x.Recv)
		m.use(x.Index)
	case *ast.ArrayLitExpr:
		if x.Repeat {
			m.use(x.Count)
		}
		if x.Value != nil {
			m.use(x.Value)
		}
		for _, el := range x.Elems {
			m.use(el)
		}
	case *ast.StructLitExpr:
		for _, fi := range x.Fields {
			m.use(fi.Val)
		}
	case *ast.TupleExpr:
		for _, el := range x.Elems {
			m.use(el)
		}
	case *ast.CallExpr:
		m.call(x)
	case *ast.MethodCallExpr:
		m.methodCall(x)
	case *ast.AssignExpr:
		m.assign(x)
	case *ast.IfExpr:
		m.ifExpr(x)
	case *ast.MatchExpr:
		m.matchExpr(x)
	case *ast.LoopExpr:
		m.loop(x)
	case *ast.Block:
		m.block(x)
	case *ast.JumpExpr:
		if x.Val != nil {
			m.consume(x.Val)
		}
	case *ast.TryExpr:
		m.use(x.X)
	}
}

// consume evaluates e and moves out of it if it names a non-Copy place.
func (m *mover) consume(e ast.Expr) {
	switch x := e.(type) {
	case *ast.IdentExpr:
		t := m.c.info.TypeOf(x)
		if t != nil && !t.IsCopy() {
			if m.moved[x.Name] {
				m.c.errf(x.Sp, "E0382", "use of moved value `%s`", x.Name)
				return
			}
			m.moved[x.Name] = true
			return
		}
		m.use(e)
	case *ast.FieldExpr:
		m.use(x.Recv)
		if t := m.c.info.TypeOf(x); t != nil && !t.IsCopy() {
			if base, ok := x.Recv.(*ast.IdentExpr); ok {
				m.moved[base.Name] = true
			}
		}
	case *ast.IndexExpr:
		m.use(x.Recv)
		m.use(x.Index)
		if t := m.c.info.TypeOf(x); t != nil && !t.IsCopy() {
			if base, ok := x.Recv.(*ast.IdentExpr); ok {
				m.moved[base.Name] = true
			}
		}
	default:
		m.use(e)
	}
}

func (m *mover) call(x *ast.CallExpr) {
	fn := m.c.info.Resolved[x]
	for i, a := range x.Args {
		if fn != nil && i < len(fn.Params) && !fn.Params[i].IsCopy() {
			m.consume(a)
		} else {
			m.use(a)
		}
	}
}

func (m *mover) methodCall(x *ast.MethodCallExpr) {
	fn := m.c.info.Resolved[x]
	if fn != nil && fn.Self != nil && !fn.Self.ByRef {
		m.consume(x.Recv) // by-value receiver: moves
	} else {
		m.use(x.Recv) // `&self` / `&mut self`, or unresolved
	}
	for i, a := range x.Args {
		pi := i
		if fn != nil && fn.Self != nil {
			pi = i + 1
		}
		if fn != nil && pi < len(fn.Params) && !fn.Params[pi].IsCopy() {
			m.consume(a)
		} else {
			m.use(a)
		}
	}
}

func (m *mover) assign(x *ast.AssignExpr) {
	if x.Op != token.KindEq {
		// Read-modify-write: both sides are read.
		m.use(x.Lhs)
		m.use(x.Rhs)
		return
	}
	m.consume(x.Rhs)
	switch l := x.Lhs.(type) {
	case *ast.IdentExpr:
		m.moved[l.Name] = false // reinitialized
	case *ast.FieldExpr:
		m.use(l.Recv)
	case *ast.IndexExpr:
		m.use(l.Recv)
		m.use(l.Index)
	default:
		m.use(x.Lhs)
	}
}

func (m *mover) ifExpr(x *ast.IfExpr) {
	m.use(x.Cond)
	before := m.snapshot()

	m.restore(cloneMap(before))
	m.block(x.Then)
	thenState := m.snapshot()

	elseState := before
	if x.Else != nil {
		m.restore(cloneMap(before))
		m.use(x.Else)
		elseState = m.snapshot()
	}
	m.restore(unionMoves(thenState, elseState))
}

func (m *mover) matchExpr(x *ast.MatchExpr) {
	m.use(x.Scrutinee)
	before := m.snapshot()
	result := before
	for _, arm := range x.Arms {
		m.restore(cloneMap(before))
		m.enter()
		m.declarePat(arm.Pat)
		if arm.Guard != nil {
			m.use(arm.Guard)
		}
		m.use(arm.Body)
		result = unionMoves(result, m.snapshot())
		m.exit()
	}
	m.restore(result)
}

func (m *mover) loop(x *ast.LoopExpr) {
	before := m.snapshot()
	switch x.Kind {
	case token.KindFor:
		m.use(x.Iter)
		before = m.snapshot()
		m.enter()
		m.declarePat(x.Pat)
		m.block(x.Body)
		after := m.snapshot()
		m.exit()
		m.restore(unionMoves(before, after))
	default:
		if x.Cond != nil {
			m.use(x.Cond)
			before = m.snapshot()
		}
		m.block(x.Body)
		after := m.snapshot()
		m.restore(unionMoves(before, after))
	}
}

func cloneMap(s map[string]bool) map[string]bool {
	cp := make(map[string]bool, len(s))
	for k, v := range s {
		cp[k] = v
	}
	return cp
}
