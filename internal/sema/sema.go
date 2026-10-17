// Package sema resolves names and infers types for a parsed Zexi file. It is
// the M0-alpha checker: it validates enough to generate correct C, and reports
// the language constructs that are designed but not yet implemented with a
// precise diagnostic rather than a crash.
//
// The output, Info, is a typed view of the program: function signatures, the
// value of each constant, and the inferred type of every expression. Codegen
// consumes Info; it never re-derives types.
package sema

import (
	"fmt"
	"strconv"
	"strings"

	"zexi/internal/ast"
	"zexi/internal/diag"
	"zexi/internal/token"
	"zexi/internal/types"
)

// Fn is a checked function.
type Fn struct {
	Decl     *ast.FnDecl
	Name     string
	QualName string // "Point::len_sq" for methods, else Name
	Params   []*types.Type
	Ret      *types.Type
	Self     *SelfParam
	Recv     *types.Struct // non-nil for methods
	Extern   bool
	ABI      string
}

// SelfParam describes a method receiver.
type SelfParam struct {
	ByRef bool
	Mut   bool
}

// Sig renders the function's signature for diagnostics.
func (f *Fn) Sig() string {
	parts := make([]string, len(f.Params))
	for i, p := range f.Params {
		parts[i] = p.String()
	}
	return fmt.Sprintf("%s(%s) -> %s", f.QualName, strings.Join(parts, ", "), f.Ret)
}

// Const is a compile-time constant.
type Const struct {
	Decl *ast.ConstDecl
	Name string
	Ty   *types.Type
	Val  ConstVal
}

// Info is the result of type checking.
type Info struct {
	File       *ast.File
	Fns        map[string]*Fn
	FnOrder    []*Fn
	Consts     map[string]*Const
	Structs    map[string]*types.Struct
	StructOrd  []*types.Struct
	Enums      map[string]*types.Enum
	EnumOrd    []*types.Enum
	Methods    map[string]map[string]*Fn // struct name -> method name -> fn
	Resolved   map[ast.Expr]*Fn          // resolved call / method target
	Ctors      map[ast.Expr]*Ctor        // resolved enum-variant construction
	PatVariant map[ast.Pattern]*types.Variant
	Types      map[ast.Expr]*types.Type
	DeclTypes  map[ast.Stmt]*types.Type
	Main       *Fn
}

// Ctor is a resolved enum-variant constructor.
type Ctor struct {
	Enum    *types.Enum
	Variant *types.Variant
}

// TypeOf returns the inferred type of an expression, or nil.
func (in *Info) TypeOf(e ast.Expr) *types.Type { return in.Types[e] }

// Check resolves names and infers types across the file.
func Check(file *ast.File) (*Info, []diag.Diagnostic) {
	c := &checker{
		info: &Info{
			File:       file,
			Fns:        map[string]*Fn{},
			Consts:     map[string]*Const{},
			Structs:    map[string]*types.Struct{},
			Enums:      map[string]*types.Enum{},
			Methods:    map[string]map[string]*Fn{},
			Resolved:   map[ast.Expr]*Fn{},
			Ctors:      map[ast.Expr]*Ctor{},
			PatVariant: map[ast.Pattern]*types.Variant{},
			Types:      map[ast.Expr]*types.Type{},
			DeclTypes:  map[ast.Stmt]*types.Type{},
		},
	}
	c.collect()
	c.checkBodies()
	c.checkOwnership()
	c.info.Main = c.info.Fns["main"]
	if c.info.Main == nil {
		c.errf(token.Span{}, "E0601", "no `main` function found")
	} else if len(c.info.Main.Params) != 0 {
		c.errf(c.info.Main.Decl.Sp, "E0601", "`main` must take no parameters, found %s", c.info.Main.Sig())
	} else if !c.info.Main.Ret.Kind.IsInt() && c.info.Main.Ret.Kind != types.Unit {
		c.errf(c.info.Main.Decl.Sp, "E0601", "`main` must return an integer or `()`, found %s", c.info.Main.Ret)
	}
	return c.info, c.errs
}

type checker struct {
	info *Info
	errs []diag.Diagnostic

	scope     []map[string]*types.Type // local bindings, innermost last
	cur       *Fn
	loopDepth int
}

func (c *checker) errf(sp token.Span, code, format string, args ...any) {
	c.errs = append(c.errs, diag.New(code, format, args...).WithSpan(sp))
}

func (c *checker) errAt(e ast.Expr, code, format string, args ...any) {
	c.errf(e.Span(), code, format, args...)
}

// --- collection pass ------------------------------------------------------

func (c *checker) collect() {
	// Pass 1: register struct and enum names so fields can refer to one
	// another.
	for _, it := range c.info.File.Items {
		switch d := it.(type) {
		case *ast.StructDecl:
			c.registerStruct(d)
		case *ast.EnumDecl:
			c.registerEnum(d)
		}
	}
	// Pass 2: fill in struct fields and enum variants.
	for _, it := range c.info.File.Items {
		switch d := it.(type) {
		case *ast.StructDecl:
			c.collectStructFields(d)
		case *ast.EnumDecl:
			c.collectEnumVariants(d)
		}
	}
	// Constants, functions, then impl blocks (methods need their struct).
	for _, it := range c.info.File.Items {
		switch d := it.(type) {
		case *ast.ConstDecl:
			c.collectConst(d)
		case *ast.FnDecl:
			c.collectFn(d, false, "")
		case *ast.ImplBlock:
			c.collectImpl(d)
		case *ast.TypeAlias:
			c.errf(d.Sp, "E0900", "`type` aliases are not supported by the compiler yet (roadmap M1)")
		case *ast.ModDecl:
			c.errf(d.Sp, "E0900", "`mod` is not supported by the compiler yet (roadmap M1)")
		case *ast.UseDecl:
			c.errf(d.Sp, "E0900", "`use` is not supported by the compiler yet (roadmap M1)")
		}
	}
}

func (c *checker) registerEnum(d *ast.EnumDecl) {
	if _, dup := c.info.Enums[d.Name]; dup {
		c.errf(d.Sp, "E0428", "the enum `%s` is defined multiple times", d.Name)
		return
	}
	en := &types.Enum{Name: d.Name, Copy: types.CopyAttr(d.Attrs)}
	c.info.Enums[d.Name] = en
	c.info.EnumOrd = append(c.info.EnumOrd, en)
}

func (c *checker) collectEnumVariants(d *ast.EnumDecl) {
	en, ok := c.info.Enums[d.Name]
	if !ok || len(en.Variants) > 0 {
		return
	}
	for i, v := range d.Variants {
		if _, dup := en.Variant(v.Name); dup {
			c.errf(v.Sp, "E0428", "the variant `%s` is defined multiple times", v.Name)
			continue
		}
		tv := types.Variant{Name: v.Name, Tuple: v.Tup, Index: i}
		for j, f := range v.Fields {
			name := f.Name
			if v.Tup {
				name = fmt.Sprintf("%d", j)
			}
			ft := c.resolveType(f.Ty)
			if ft == nil {
				ft = &types.Type{Kind: types.Invalid}
			}
			tv.Fields = append(tv.Fields, types.Field{Name: name, Ty: ft})
		}
		en.Variants = append(en.Variants, tv)
	}
	if en.Copy {
		for _, v := range en.Variants {
			for _, f := range v.Fields {
				if !f.Ty.IsCopy() {
					c.errf(d.Sp, "E0204", "`#[copy]` enum `%s` has non-Copy field of type `%s` in variant `%s`", en.Name, f.Ty, v.Name)
				}
			}
		}
	}
}

func (c *checker) registerStruct(d *ast.StructDecl) {
	if _, dup := c.info.Structs[d.Name]; dup {
		c.errf(d.Sp, "E0428", "the struct `%s` is defined multiple times", d.Name)
		return
	}
	st := &types.Struct{Name: d.Name, Tuple: d.Tuple, Copy: types.CopyAttr(d.Attrs)}
	c.info.Structs[d.Name] = st
	c.info.StructOrd = append(c.info.StructOrd, st)
}

func (c *checker) collectStructFields(d *ast.StructDecl) {
	st, ok := c.info.Structs[d.Name]
	if !ok || len(st.Fields) > 0 {
		return // duplicate or already filled
	}
	for i, f := range d.Fields {
		name := f.Name
		if d.Tuple {
			name = fmt.Sprintf("%d", i)
		}
		ft := c.resolveType(f.Ty)
		if ft == nil {
			ft = &types.Type{Kind: types.Invalid}
		}
		st.Fields = append(st.Fields, types.Field{Name: name, Ty: ft, Pub: bool(f.Vis)})
	}
	if st.Copy {
		for _, f := range st.Fields {
			if !f.Ty.IsCopy() {
				c.errf(d.Sp, "E0204", "`#[copy]` struct `%s` has non-Copy field `%s` of type `%s`", st.Name, f.Name, f.Ty)
			}
		}
	}
}

func (c *checker) collectImpl(d *ast.ImplBlock) {
	if d.Trait != nil {
		c.errf(d.Sp, "E0900", "trait impls are not supported by the compiler yet (roadmap M1)")
		return
	}
	name := typePathName(d.SelfTy)
	st, ok := c.info.Structs[name]
	if !ok {
		c.errf(d.Sp, "E0118", "no struct named `%s` in this scope", name)
		return
	}
	if c.info.Methods[name] == nil {
		c.info.Methods[name] = map[string]*Fn{}
	}
	for _, m := range d.Fns {
		fn := c.makeFn(m, st)
		if fn == nil {
			continue
		}
		if _, dup := c.info.Methods[name][m.Name]; dup {
			c.errf(m.Sp, "E0592", "duplicate method `%s` on `%s`", m.Name, name)
			continue
		}
		c.info.Methods[name][m.Name] = fn
		c.info.FnOrder = append(c.info.FnOrder, fn)
	}
}

func (c *checker) collectFn(d *ast.FnDecl, extern bool, abi string) {
	if _, dup := c.info.Fns[d.Name]; dup {
		c.errf(d.Sp, "E0428", "the function `%s` is defined multiple times", d.Name)
		return
	}
	fn := c.makeFn(d, nil)
	if fn == nil {
		return
	}
	if extern || d.Extern != "" || d.Body == nil {
		fn.Extern = true
	}
	if abi != "" {
		fn.ABI = abi
	}
	c.info.Fns[d.Name] = fn
	c.info.FnOrder = append(c.info.FnOrder, fn)
}

// makeFn builds a checked function header, handling method receivers.
func (c *checker) makeFn(d *ast.FnDecl, recv *types.Struct) *Fn {
	fn := &Fn{Decl: d, Name: d.Name, Recv: recv}
	if recv != nil {
		fn.QualName = recv.Name + "::" + d.Name
	} else {
		fn.QualName = d.Name
	}
	for _, p := range d.Params {
		if p.Self {
			if recv == nil {
				c.errf(p.Sp, "E0424", "`self` can only be used in an `impl` block")
				continue
			}
			fn.Self = &SelfParam{ByRef: p.SelfRef, Mut: p.SelfMut}
			selfT := &types.Type{Kind: types.StructKind, St: recv}
			if p.SelfRef {
				selfT = types.RefTo(selfT, p.SelfMut)
			}
			fn.Params = append(fn.Params, selfT)
			continue
		}
		pt := c.resolveType(p.Ty)
		if pt == nil {
			pt = &types.Type{Kind: types.Invalid}
		}
		fn.Params = append(fn.Params, pt)
	}
	if d.Ret != nil {
		fn.Ret = c.resolveType(d.Ret)
	}
	if fn.Ret == nil {
		fn.Ret = types.UnitT
	}
	// A body-less function must be extern/FFI.
	if d.Body == nil && d.Extern == "" {
		c.errf(d.Sp, "E0204", "function `%s` has no body; only `extern` functions may omit it", d.Name)
	}
	return fn
}

// typePathName returns the single-segment name of a path type, or "<error>".
func typePathName(t ast.Type) string {
	if pt, ok := t.(*ast.PathType); ok && len(pt.Segments) > 0 {
		return pt.Segments[0]
	}
	return "<error>"
}

func (c *checker) collectConst(d *ast.ConstDecl) {
	if _, dup := c.info.Consts[d.Name]; dup {
		c.errf(d.Sp, "E0428", "the constant `%s` is defined multiple times", d.Name)
		return
	}
	var declared *types.Type
	if d.Ty != nil {
		declared = c.resolveType(d.Ty)
	}
	val, ok := c.evalConst(d.Val)
	if !ok {
		c.errf(d.Val.Span(), "E0602", "constant `%s` is not a compile-time constant expression", d.Name)
		return
	}
	if declared != nil {
		if !c.assignable(declared, val.Ty) {
			c.errf(d.Val.Span(), "E0308", "mismatched types: expected `%s`, found `%s`", declared, val.Ty)
			return
		}
		val.Ty = declared
	}
	c.info.Consts[d.Name] = &Const{Decl: d, Name: d.Name, Ty: val.Ty, Val: val}
}

// --- body checking --------------------------------------------------------

func (c *checker) checkBodies() {
	for _, fn := range c.info.FnOrder {
		if fn.Decl.Body == nil {
			continue
		}
		c.cur = fn
		c.scope = c.scope[:0]
		c.pushScope()
		for i, p := range fn.Decl.Params {
			if i >= len(fn.Params) {
				break
			}
			c.declare(p.Sp, p.Name, fn.Params[i])
		}
		bt := c.checkBlock(fn.Decl.Body)
		if fn.Ret.Kind != types.Unit {
			if !c.exprAlwaysReturns(fn.Decl.Body) && bt.Kind != types.Never {
				if !c.assignable(fn.Ret, bt) {
					c.errf(fn.Decl.Body.Sp, "E0308", "function `%s` returns `%s` but its body has type `%s`", fn.Name, fn.Ret, bt)
				}
			}
		}
		c.popScope()
	}
}

// exprAlwaysReturns reports whether evaluating e diverges via `return`.
func (c *checker) exprAlwaysReturns(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.JumpExpr:
		return x.Kind == token.KindReturn
	case *ast.Block:
		return c.blockAlwaysReturns(x)
	case *ast.IfExpr:
		if x.Else == nil {
			return false
		}
		return c.blockAlwaysReturns(x.Then) && c.exprAlwaysReturns(x.Else)
	}
	return false
}

// blockAlwaysReturns reports whether the block always diverges via `return`.
func (c *checker) blockAlwaysReturns(b *ast.Block) bool {
	if b.Tail != nil {
		return c.exprAlwaysReturns(b.Tail)
	}
	if len(b.Stmts) > 0 {
		if es, ok := b.Stmts[len(b.Stmts)-1].(*ast.ExprStmt); ok {
			return c.exprAlwaysReturns(es.X)
		}
	}
	return false
}

func (c *checker) checkBlock(b *ast.Block) *types.Type {
	c.pushScope()
	defer c.popScope()
	for _, s := range b.Stmts {
		c.checkStmt(s)
	}
	if b.Tail != nil {
		t := c.checkExpr(b.Tail, nil)
		c.info.Types[b] = t
		return t
	}
	// A block whose last statement diverges has type `never`.
	if len(b.Stmts) > 0 {
		if es, ok := b.Stmts[len(b.Stmts)-1].(*ast.ExprStmt); ok && c.exprAlwaysReturns(es.X) {
			c.info.Types[b] = types.NeverT
			return types.NeverT
		}
	}
	c.info.Types[b] = types.UnitT
	return types.UnitT
}

func (c *checker) checkStmt(s ast.Stmt) {
	switch st := s.(type) {
	case *ast.LetStmt:
		var declared *types.Type
		if st.Ty != nil {
			declared = c.resolveType(st.Ty)
		}
		var initT *types.Type
		if st.Val != nil {
			initT = c.checkExpr(st.Val, declared)
		}
		ty := declared
		if ty == nil {
			ty = initT
		}
		if ty == nil {
			c.errf(st.Sp, "E0282", "type annotations needed for this binding")
			ty = &types.Type{Kind: types.Invalid}
		}
		if ty.Kind == types.Unit {
			c.errf(st.Sp, "E0308", "cannot bind a value of type `()`")
		}
		if declared != nil && initT != nil && !c.assignable(declared, initT) {
			c.errf(st.Val.Span(), "E0308", "mismatched types: expected `%s`, found `%s`", declared, initT)
		}
		c.info.DeclTypes[st] = ty
		c.bindPattern(st.Pat, ty, true)
	case *ast.ExprStmt:
		c.checkExpr(st.X, nil)
	default:
		c.errf(s.Span(), "E0900", "unsupported statement")
	}
}

func (c *checker) bindPattern(p ast.Pattern, ty *types.Type, let bool) {
	switch pt := p.(type) {
	case *ast.BindPat:
		if let && pt.Name == "" {
			return
		}
		if pt.Name != "_" {
			c.declare(pt.Sp, pt.Name, ty)
		}
	case *ast.WildcardPat:
	case *ast.TuplePat:
		c.errf(pt.Sp, "E0900", "tuple patterns are not supported by the compiler yet")
	default:
		c.errf(p.Span(), "E0900", "this pattern is not supported by the compiler yet")
	}
}

// --- expression checking --------------------------------------------------

func (c *checker) checkExpr(e ast.Expr, expected *types.Type) *types.Type {
	t := c.infer(e, expected)
	if t == nil {
		t = &types.Type{Kind: types.Invalid}
	}
	c.info.Types[e] = t
	return t
}

func (c *checker) infer(e ast.Expr, expected *types.Type) *types.Type {
	switch x := e.(type) {
	case *ast.LiteralExpr:
		return c.inferLiteral(x, expected)

	case *ast.IdentExpr:
		return c.inferIdent(x.Name, x.Sp)

	case *ast.PathExpr:
		if len(x.Segments) == 1 {
			return c.inferIdent(x.Segments[0], x.Sp)
		}
		if len(x.Segments) == 2 {
			en, ok := c.info.Enums[x.Segments[0]]
			if !ok {
				c.errf(x.Sp, "E0412", "cannot find type `%s` in this scope", x.Segments[0])
				return nil
			}
			v, ok := en.Variant(x.Segments[1])
			if !ok {
				c.errf(x.Sp, "E0599", "no variant `%s` on enum `%s`", x.Segments[1], en.Name)
				return nil
			}
			if len(v.Fields) > 0 {
				c.errf(x.Sp, "E0061", "variant `%s::%s` requires %d argument(s)", en.Name, v.Name, len(v.Fields))
				return nil
			}
			c.info.Ctors[x] = &Ctor{Enum: en, Variant: v}
			return &types.Type{Kind: types.EnumKind, En: en}
		}
		c.errf(x.Sp, "E0900", "paths are not supported by the compiler yet (roadmap M1)")
		return nil

	case *ast.TupleExpr:
		if len(x.Elems) == 0 {
			return types.UnitT
		}
		c.errf(x.Sp, "E0900", "tuples are not supported by the compiler yet (roadmap M1)")
		return nil

	case *ast.Block:
		return c.checkBlock(x)

	case *ast.UnExpr:
		return c.inferUnary(x, expected)

	case *ast.BinExpr:
		return c.inferBinary(x)

	case *ast.AssignExpr:
		return c.inferAssign(x)

	case *ast.CastExpr:
		srcT := c.checkExpr(x.X, nil)
		dstT := c.resolveType(x.Ty)
		if dstT == nil {
			return nil
		}
		if !srcT.Kind.IsNumeric() && srcT.Kind != types.Char && srcT.Kind != types.Bool {
			c.errAt(x.X, "E0308", "cannot cast `%s` with `as`", srcT)
			return dstT
		}
		if !dstT.Kind.IsNumeric() && dstT.Kind != types.Char {
			c.errAt(x, "E0308", "cannot cast to `%s`", dstT)
		}
		return dstT

	case *ast.CallExpr:
		return c.inferCall(x)

	case *ast.IfExpr:
		return c.inferIf(x)

	case *ast.LoopExpr:
		return c.inferLoop(x)

	case *ast.JumpExpr:
		return c.inferJump(x)

	case *ast.MatchExpr:
		return c.inferMatch(x)
	case *ast.FieldExpr:
		return c.inferField(x)
	case *ast.MethodCallExpr:
		return c.inferMethodCall(x)
	case *ast.StructLitExpr:
		return c.inferStructLit(x)
	case *ast.IndexExpr:
		return c.inferIndex(x)
	case *ast.ArrayLitExpr:
		return c.inferArrayLit(x)
	case *ast.TryExpr:
		c.errf(x.Sp, "E0900", "the `?` operator is not supported by the compiler yet (roadmap M0)")
		return nil
	}
	c.errf(e.Span(), "E0900", "unsupported expression")
	return nil
}

func (c *checker) inferLiteral(x *ast.LiteralExpr, expected *types.Type) *types.Type {
	switch x.Kind {
	case token.KindInteger:
		if expected != nil && expected.Kind.IsInt() {
			return expected
		}
		return types.I32T
	case token.KindFloat:
		if expected != nil && expected.Kind.IsFloat() {
			return expected
		}
		return types.F64T
	case token.KindTrue, token.KindFalse:
		return types.BoolT
	case token.KindString, token.KindRawString:
		return types.StrT
	case token.KindChar:
		return types.CharT
	case token.KindByteChar:
		return &types.Type{Kind: types.U8}
	}
	return nil
}

func (c *checker) inferIdent(name string, sp token.Span) *types.Type {
	if name == "_" {
		c.errf(sp, "E0425", "cannot use `_` as a value")
		return nil
	}
	for i := len(c.scope) - 1; i >= 0; i-- {
		if t, ok := c.scope[i][name]; ok {
			return t
		}
	}
	if cn, ok := c.info.Consts[name]; ok {
		return cn.Ty
	}
	if fn, ok := c.info.Fns[name]; ok {
		return &types.Type{Kind: types.Fn, Params: fn.Params, Ret: fn.Ret}
	}
	c.errf(sp, "E0425", "cannot find `%s` in this scope", name)
	return nil
}

func (c *checker) inferUnary(x *ast.UnExpr, expected *types.Type) *types.Type {
	switch x.Op {
	case token.KindMinus:
		t := c.checkExpr(x.X, expected)
		if !t.Kind.IsNumeric() {
			c.errAt(x, "E0308", "cannot apply unary `-` to `%s`", t)
		}
		return t
	case token.KindBang:
		t := c.checkExpr(x.X, nil)
		if t.Kind == types.Bool || t.Kind.IsInt() {
			return t
		}
		c.errAt(x, "E0308", "cannot apply unary `!` to `%s`", t)
		return t
	case token.KindStar:
		t := c.checkExpr(x.X, nil)
		if t.Kind == types.Ptr || t.Kind == types.Ref {
			return t.Elem
		}
		c.errAt(x, "E0308", "cannot dereference `%s`", t)
		return nil
	case token.KindAmp:
		t := c.checkExpr(x.X, nil)
		return types.RefTo(t, x.Mut)
	}
	return nil
}

func (c *checker) inferBinary(x *ast.BinExpr) *types.Type {
	if x.Op == token.KindDotDot || x.Op == token.KindDotDotEq {
		lt := c.checkExpr(x.L, nil)
		c.checkExpr(x.R, lt)
		if !lt.Kind.IsInt() {
			c.errAt(x, "E0308", "range bounds must be integers, found `%s`", lt)
		}
		return types.UnitT
	}

	lt := c.checkExpr(x.L, nil)
	rt := c.checkExpr(x.R, nil)
	// Integer literals adopt the type of the other operand.
	if !types.Equal(lt, rt) {
		switch {
		case isIntLiteral(x.L) && rt.Kind.IsNumeric():
			lt = c.checkExpr(x.L, rt)
		case isIntLiteral(x.R) && lt.Kind.IsNumeric():
			rt = c.checkExpr(x.R, lt)
		}
	}
	if !types.Equal(lt, rt) {
		c.errAt(x, "E0308", "mismatched types in binary expression: `%s` and `%s`", lt, rt)
		return nil
	}

	switch x.Op {
	case token.KindAmpAmp, token.KindPipePipe:
		if lt.Kind != types.Bool {
			c.errAt(x, "E0308", "logical operators require `bool`, found `%s`", lt)
		}
		return types.BoolT
	case token.KindEqEq, token.KindBangEq:
		if !lt.Kind.IsNumeric() && lt.Kind != types.Bool && lt.Kind != types.Char && lt.Kind != types.Str {
			c.errAt(x, "E0308", "cannot compare values of type `%s`", lt)
		}
		return types.BoolT
	case token.KindLt, token.KindLe, token.KindGt, token.KindGe:
		if !lt.Kind.IsNumeric() && lt.Kind != types.Char {
			c.errAt(x, "E0308", "cannot order values of type `%s`", lt)
		}
		return types.BoolT
	case token.KindShl, token.KindShr, token.KindAmp, token.KindPipe, token.KindCaret:
		if !lt.Kind.IsInt() {
			c.errAt(x, "E0308", "bitwise operators require integers, found `%s`", lt)
		}
		return lt
	case token.KindPlus, token.KindMinus, token.KindStar, token.KindSlash, token.KindPercent:
		if !lt.Kind.IsNumeric() {
			c.errAt(x, "E0308", "arithmetic operators require numbers, found `%s`", lt)
		}
		return lt
	}
	return lt
}

func (c *checker) inferAssign(x *ast.AssignExpr) *types.Type {
	if !c.isPlace(x.Lhs) {
		c.errAt(x.Lhs, "E0384", "cannot assign to this expression")
	}
	lt := c.checkExpr(x.Lhs, nil)
	rt := c.checkExpr(x.Rhs, lt)
	if x.Op == token.KindEq {
		if !c.assignable(lt, rt) {
			c.errAt(x.Rhs, "E0308", "mismatched types: expected `%s`, found `%s`", lt, rt)
		}
	} else {
		if !lt.Kind.IsNumeric() {
			c.errAt(x, "E0308", "compound assignment requires a numeric type, found `%s`", lt)
		}
	}
	return types.UnitT
}

// isPlace reports whether e is a valid assignment target.
func (c *checker) isPlace(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.IdentExpr:
		return x.Name != "_"
	case *ast.UnExpr:
		return x.Op == token.KindStar
	case *ast.FieldExpr, *ast.IndexExpr:
		return true
	}
	return false
}

func (c *checker) inferCall(x *ast.CallExpr) *types.Type {
	// `Enum::Variant(args)` is a variant constructor, not a function call.
	if p, ok := x.Callee.(*ast.PathExpr); ok && len(p.Segments) == 2 {
		return c.inferVariantCtor(x, p)
	}
	name, ok := calleeName(x.Callee)
	if !ok {
		c.errAt(x.Callee, "E0618", "expression is not callable")
		for _, a := range x.Args {
			c.checkExpr(a, nil)
		}
		return nil
	}
	if name == "print" || name == "println" {
		return c.inferPrint(x, name)
	}
	if name == "len" {
		return c.inferLen(x)
	}
	fn, ok := c.info.Fns[name]
	if !ok {
		c.errAt(x.Callee, "E0425", "cannot find function `%s` in this scope", name)
		for _, a := range x.Args {
			c.checkExpr(a, nil)
		}
		return nil
	}
	c.info.Resolved[x] = fn
	if len(x.Args) != len(fn.Params) {
		c.errAt(x, "E0061", "function `%s` takes %d argument(s) but %d were supplied", name, len(fn.Params), len(x.Args))
	}
	for i, a := range x.Args {
		var want *types.Type
		if i < len(fn.Params) {
			want = fn.Params[i]
		}
		at := c.checkExpr(a, want)
		if want != nil && !c.assignable(want, at) {
			c.errAt(a, "E0308", "mismatched types: expected `%s`, found `%s`", want, at)
		}
	}
	return fn.Ret
}

// inferLen checks the builtin `len(x)` for arrays and strings.
func (c *checker) inferLen(x *ast.CallExpr) *types.Type {
	if len(x.Args) != 1 {
		c.errAt(x, "E0061", "`len` takes exactly 1 argument but %d were supplied", len(x.Args))
		for _, a := range x.Args {
			c.checkExpr(a, nil)
		}
		return types.UsizeT
	}
	t := c.checkExpr(x.Args[0], nil)
	if t != nil && (t.Kind == types.Array || t.Kind == types.Str || t.Kind == types.Slice) {
		return types.UsizeT
	}
	c.errAt(x.Args[0], "E0308", "`len` expects an array, slice, or `str`, found `%s`", t)
	return types.UsizeT
}

// inferIndex checks `xs[i]` for arrays and slices.
func (c *checker) inferIndex(x *ast.IndexExpr) *types.Type {
	rt := c.checkExpr(x.Recv, nil)
	it := c.checkExpr(x.Index, nil)
	if rt == nil || (rt.Kind != types.Array && rt.Kind != types.Slice) {
		c.errAt(x, "E0608", "cannot index a value of type `%s`", rt)
		return nil
	}
	if it != nil && !it.Kind.IsInt() {
		c.errAt(x.Index, "E0308", "array index must be an integer, found `%s`", it)
	}
	// Constant indices are bounds-checked at compile time.
	if n, ok := c.evalConst(x.Index); ok && n.Ty != nil && n.Ty.Kind.IsInt() {
		if rt.Kind == types.Array && (n.Int < 0 || n.Int >= rt.Len) {
			c.errAt(x.Index, "E0502", "index %d is out of bounds for array of length %d", n.Int, rt.Len)
		}
	}
	return rt.Elem
}

// inferArrayLit checks `[a, b, c]` and `[value; count]`, using the expected
// type for the element type when available.
func (c *checker) inferArrayLit(x *ast.ArrayLitExpr) *types.Type {
	if x.Repeat {
		c.checkExpr(x.Count, types.UsizeT)
		n, ok := c.evalConst(x.Count)
		if !ok || n.Ty == nil || !n.Ty.Kind.IsInt() {
			c.errAt(x.Count, "E0308", "array repeat count must be an integer constant")
			c.checkExpr(x.Value, nil)
			return &types.Type{Kind: types.Array, Elem: types.I32T, Len: 0}
		}
		v := c.checkExpr(x.Value, nil)
		return &types.Type{Kind: types.Array, Elem: v, Len: n.Int}
	}
	if len(x.Elems) == 0 {
		c.errAt(x, "E0283", "cannot infer the element type of an empty array; annotate it")
		return &types.Type{Kind: types.Array, Elem: types.I32T, Len: 0}
	}
	var elemT *types.Type
	for _, e := range x.Elems {
		et := c.checkExpr(e, elemT)
		if elemT == nil {
			elemT = et
		} else if et != nil && !types.Equal(elemT, et) {
			c.errAt(e, "E0308", "array elements have incompatible types: `%s` and `%s`", elemT, et)
		}
	}
	return &types.Type{Kind: types.Array, Elem: elemT, Len: int64(len(x.Elems))}
}

// inferVariantCtor checks `Enum::Variant(args)`.
func (c *checker) inferVariantCtor(x *ast.CallExpr, p *ast.PathExpr) *types.Type {
	en, ok := c.info.Enums[p.Segments[0]]
	if !ok {
		c.errf(p.Sp, "E0412", "cannot find type `%s` in this scope", p.Segments[0])
		for _, a := range x.Args {
			c.checkExpr(a, nil)
		}
		return nil
	}
	v, ok := en.Variant(p.Segments[1])
	if !ok {
		c.errf(p.Sp, "E0599", "no variant `%s` on enum `%s`", p.Segments[1], en.Name)
		for _, a := range x.Args {
			c.checkExpr(a, nil)
		}
		return nil
	}
	c.info.Ctors[x] = &Ctor{Enum: en, Variant: v}
	if len(x.Args) != len(v.Fields) {
		c.errAt(x, "E0061", "variant `%s::%s` takes %d field(s) but %d were supplied", en.Name, v.Name, len(v.Fields), len(x.Args))
	}
	for i, a := range x.Args {
		var want *types.Type
		if i < len(v.Fields) {
			want = v.Fields[i].Ty
		}
		at := c.checkExpr(a, want)
		if want != nil && !c.assignable(want, at) {
			c.errAt(a, "E0308", "mismatched types: expected `%s`, found `%s`", want, at)
		}
	}
	return &types.Type{Kind: types.EnumKind, En: en}
}

// isCatchAll reports whether a pattern matches every value (a binding or `_`).
func isCatchAll(p ast.Pattern) bool {
	switch p.(type) {
	case *ast.WildcardPat, *ast.BindPat:
		return true
	}
	return false
}

func (c *checker) inferMatch(x *ast.MatchExpr) *types.Type {
	st := c.checkExpr(x.Scrutinee, nil)
	base := recvBase(st)
	var result *types.Type
	covered := map[string]bool{}
	catchAll := false
	for _, arm := range x.Arms {
		if isCatchAll(arm.Pat) {
			catchAll = true
		}
		c.pushScope()
		c.checkPattern(arm.Pat, st)
		if pp, ok := arm.Pat.(*ast.PathPat); ok {
			if v := c.info.PatVariant[pp]; v != nil {
				covered[v.Name] = true
			}
		}
		if arm.Guard != nil {
			gt := c.checkExpr(arm.Guard, types.BoolT)
			if gt == nil || gt.Kind != types.Bool {
				c.errAt(arm.Guard, "E0308", "match guard must be `bool`, found `%s`", gt)
			}
			c.errAt(arm.Guard, "E0900", "match guards are not supported by the compiler yet (roadmap M0)")
		}
		bt := c.checkExpr(arm.Body, result)
		switch {
		case result == nil:
			result = bt
		case result.Kind == types.Never:
			result = bt
		case bt != nil && bt.Kind == types.Never:
			// diverging arm does not constrain the result
		case bt != nil && !types.Equal(result, bt):
			c.errf(arm.Sp, "E0308", "match arms have incompatible types: `%s` and `%s`", result, bt)
		}
		c.popScope()
	}
	if base != nil && base.Kind == types.EnumKind && !catchAll {
		var missing []string
		for _, v := range base.En.Variants {
			if !covered[v.Name] {
				missing = append(missing, v.Name)
			}
		}
		if len(missing) > 0 {
			c.errAt(x, "E0004", "non-exhaustive match: missing `%s`", strings.Join(missing, "`, `"))
		}
	} else if base != nil && base.Kind.IsInt() && !catchAll {
		c.errAt(x, "E0004", "non-exhaustive match: add a `_` arm")
	}
	if result == nil {
		result = types.UnitT
	}
	return result
}

// checkPattern type-checks a pattern against the scrutinee type and declares its
// bindings in the current scope.
func (c *checker) checkPattern(p ast.Pattern, st *types.Type) {
	switch pt := p.(type) {
	case *ast.WildcardPat:
		return
	case *ast.BindPat:
		if pt.Name != "_" {
			c.declare(pt.Sp, pt.Name, st)
		}
		return
	case *ast.LitPat:
		if st == nil || !st.Kind.IsInt() {
			c.errf(pt.Sp, "E0308", "literal patterns require an integer scrutinee, found `%s`", st)
		}
		return
	case *ast.PathPat:
		base := recvBase(st)
		if base == nil || base.Kind != types.EnumKind {
			c.errf(pt.Sp, "E0308", "variant patterns require an enum scrutinee, found `%s`", st)
			return
		}
		name := pt.Path[len(pt.Path)-1]
		v, ok := base.En.Variant(name)
		if !ok {
			c.errf(pt.Sp, "E0599", "no variant `%s` on enum `%s`", name, base.En.Name)
			return
		}
		c.info.PatVariant[pt] = v
		if len(pt.Sub) > 0 && len(pt.Sub) != len(v.Fields) {
			c.errf(pt.Sp, "E0027", "pattern for `%s::%s` has %d field(s) but the variant has %d", base.En.Name, v.Name, len(pt.Sub), len(v.Fields))
		}
		for i, sub := range pt.Sub {
			if i >= len(v.Fields) {
				break
			}
			c.checkPattern(sub, v.Fields[i].Ty)
		}
		for _, fp := range pt.Fields {
			f, ok := variantField(v, fp.Name)
			if !ok {
				c.errf(fp.Sp, "E0026", "variant `%s` has no field named `%s`", v.Name, fp.Name)
				continue
			}
			sub := fp.Pat
			if sub == nil {
				sub = &ast.BindPat{Name: fp.Name, Sp: fp.Sp}
			}
			c.checkPattern(sub, f.Ty)
		}
		return
	default:
		c.errf(p.Span(), "E0900", "this pattern is not supported by the compiler yet (roadmap M0)")
	}
}

func variantField(v *types.Variant, name string) (*types.Field, bool) {
	for i := range v.Fields {
		if v.Fields[i].Name == name {
			return &v.Fields[i], true
		}
	}
	return nil, false
}

// recvBase strips references and pointers to find the underlying type, which is
// how `p.x` and `p.method()` auto-deref.
func recvBase(t *types.Type) *types.Type {
	for t != nil && (t.Kind == types.Ref || t.Kind == types.Ptr) {
		t = t.Elem
	}
	return t
}

func (c *checker) inferField(x *ast.FieldExpr) *types.Type {
	rt := c.checkExpr(x.Recv, nil)
	base := recvBase(rt)
	if base == nil || base.Kind != types.StructKind {
		c.errAt(x, "E0609", "no field `%s` on type `%s`", x.Field, rt)
		return nil
	}
	f, ok := base.St.Field(x.Field)
	if !ok {
		c.errAt(x, "E0609", "no field `%s` on type `%s`", x.Field, base)
		return nil
	}
	return f.Ty
}

func (c *checker) inferMethodCall(x *ast.MethodCallExpr) *types.Type {
	rt := c.checkExpr(x.Recv, nil)
	base := recvBase(rt)
	if base == nil || base.Kind != types.StructKind {
		c.errAt(x, "E0599", "no method `%s` on type `%s`", x.Name, rt)
		for _, a := range x.Args {
			c.checkExpr(a, nil)
		}
		return nil
	}
	m, ok := c.info.Methods[base.St.Name][x.Name]
	if !ok {
		c.errAt(x, "E0599", "no method `%s` on type `%s`", x.Name, base)
		for _, a := range x.Args {
			c.checkExpr(a, nil)
		}
		return nil
	}
	if m.Self != nil && m.Self.ByRef && rt.Kind != types.Ref && rt.Kind != types.Ptr {
		// Taking `&self` from a value receiver is allowed (a temporary is
		// addressed); nothing to check here.
	}
	c.info.Resolved[x] = m
	want := m.Params
	if m.Self != nil && len(want) > 0 {
		want = want[1:] // skip the receiver
	}
	if len(x.Args) != len(want) {
		c.errAt(x, "E0061", "method `%s::%s` takes %d argument(s) but %d were supplied", base.St.Name, x.Name, len(want), len(x.Args))
	}
	for i, a := range x.Args {
		var wt *types.Type
		if i < len(want) {
			wt = want[i]
		}
		at := c.checkExpr(a, wt)
		if wt != nil && !c.assignable(wt, at) {
			c.errAt(a, "E0308", "mismatched types: expected `%s`, found `%s`", wt, at)
		}
	}
	return m.Ret
}

func (c *checker) inferStructLit(x *ast.StructLitExpr) *types.Type {
	if len(x.Path) == 2 {
		return c.inferEnumStructLit(x)
	}
	if len(x.Path) != 1 {
		c.errAt(x, "E0900", "path-qualified struct literals are not supported yet (roadmap M1)")
		return nil
	}
	st, ok := c.info.Structs[x.Path[0]]
	if !ok {
		c.errAt(x, "E0412", "cannot find type `%s` in this scope", x.Path[0])
		return nil
	}
	seen := map[string]bool{}
	for _, fi := range x.Fields {
		f, ok := st.Field(fi.Name)
		if !ok {
			c.errf(fi.Sp, "E0560", "struct `%s` has no field named `%s`", st.Name, fi.Name)
			c.checkExpr(fi.Val, nil)
			continue
		}
		if seen[fi.Name] {
			c.errf(fi.Sp, "E0062", "field `%s` specified more than once", fi.Name)
		}
		seen[fi.Name] = true
		vt := c.checkExpr(fi.Val, f.Ty)
		if !c.assignable(f.Ty, vt) {
			c.errAt(fi.Val, "E0308", "mismatched types in field `%s`: expected `%s`, found `%s`", fi.Name, f.Ty, vt)
		}
	}
	for _, f := range st.Fields {
		if !seen[f.Name] {
			c.errf(x.Sp, "E0063", "missing field `%s` in initializer of `%s`", f.Name, st.Name)
		}
	}
	return &types.Type{Kind: types.StructKind, St: st}
}

// inferEnumStructLit checks `Enum::Variant { field: value, .. }`.
func (c *checker) inferEnumStructLit(x *ast.StructLitExpr) *types.Type {
	en, ok := c.info.Enums[x.Path[0]]
	if !ok {
		c.errAt(x, "E0412", "cannot find type `%s` in this scope", x.Path[0])
		return nil
	}
	v, ok := en.Variant(x.Path[1])
	if !ok {
		c.errAt(x, "E0599", "no variant `%s` on enum `%s`", x.Path[1], en.Name)
		return nil
	}
	c.info.Ctors[x] = &Ctor{Enum: en, Variant: v}
	seen := map[string]bool{}
	for _, fi := range x.Fields {
		f, ok := variantField(v, fi.Name)
		if !ok {
			c.errf(fi.Sp, "E0560", "variant `%s::%s` has no field named `%s`", en.Name, v.Name, fi.Name)
			c.checkExpr(fi.Val, nil)
			continue
		}
		if seen[fi.Name] {
			c.errf(fi.Sp, "E0062", "field `%s` specified more than once", fi.Name)
		}
		seen[fi.Name] = true
		vt := c.checkExpr(fi.Val, f.Ty)
		if !c.assignable(f.Ty, vt) {
			c.errAt(fi.Val, "E0308", "mismatched types in field `%s`: expected `%s`, found `%s`", fi.Name, f.Ty, vt)
		}
	}
	for _, f := range v.Fields {
		if !seen[f.Name] {
			c.errf(x.Sp, "E0063", "missing field `%s` in initializer of `%s::%s`", f.Name, en.Name, v.Name)
		}
	}
	return &types.Type{Kind: types.EnumKind, En: en}
}

func (c *checker) inferPrint(x *ast.CallExpr, name string) *types.Type {
	if len(x.Args) > 1 {
		c.errAt(x, "E0061", "`%s` takes at most 1 argument but %d were supplied", name, len(x.Args))
	}
	for _, a := range x.Args {
		at := c.checkExpr(a, nil)
		if !isPrintable(at) {
			c.errAt(a, "E0277", "`%s` cannot be printed", at)
		}
	}
	return types.UnitT
}

func (c *checker) inferIf(x *ast.IfExpr) *types.Type {
	ct := c.checkExpr(x.Cond, types.BoolT)
	if ct.Kind != types.Bool {
		c.errAt(x.Cond, "E0308", "`if` condition must be `bool`, found `%s`", ct)
	}
	thenT := c.checkBlock(x.Then)
	if x.Else == nil {
		if thenT.Kind != types.Unit && thenT.Kind != types.Never {
			c.errf(x.Sp, "E0317", "`if` without `else` must have type `()`; found `%s`", thenT)
		}
		return types.UnitT
	}
	elseT := c.checkExpr(x.Else, thenT)
	// A diverging branch takes on the type of the other branch.
	if thenT.Kind == types.Never && elseT.Kind == types.Never {
		return types.NeverT
	}
	if thenT.Kind == types.Never {
		return elseT
	}
	if elseT.Kind == types.Never {
		return thenT
	}
	if !types.Equal(thenT, elseT) {
		c.errf(x.Sp, "E0308", "`if` and `else` have incompatible types: `%s` and `%s`", thenT, elseT)
		return nil
	}
	return thenT
}

func (c *checker) inferLoop(x *ast.LoopExpr) *types.Type {
	switch x.Kind {
	case token.KindLoop:
		c.loopDepth++
		c.checkBlock(x.Body)
		c.loopDepth--
		return types.UnitT
	case token.KindWhile:
		ct := c.checkExpr(x.Cond, types.BoolT)
		if ct.Kind != types.Bool {
			c.errAt(x.Cond, "E0308", "`while` condition must be `bool`, found `%s`", ct)
		}
		c.loopDepth++
		c.checkBlock(x.Body)
		c.loopDepth--
		return types.UnitT
	case token.KindFor:
		return c.inferFor(x)
	}
	return types.UnitT
}

func (c *checker) inferFor(x *ast.LoopExpr) *types.Type {
	rng, ok := x.Iter.(*ast.BinExpr)
	if !ok || (rng.Op != token.KindDotDot && rng.Op != token.KindDotDotEq) {
		c.errAt(x.Iter, "E0900", "only `for i in a..b` is supported by the compiler yet (roadmap M0)")
		c.loopDepth++
		c.checkBlock(x.Body)
		c.loopDepth--
		return types.UnitT
	}
	lt := c.checkExpr(rng.L, nil)
	c.checkExpr(rng.R, lt)
	if !lt.Kind.IsInt() {
		c.errAt(x.Iter, "E0308", "range bounds must be integers, found `%s`", lt)
		lt = types.I32T
	}
	c.pushScope()
	c.bindPattern(x.Pat, lt, true)
	c.loopDepth++
	c.checkBlock(x.Body)
	c.loopDepth--
	c.popScope()
	return types.UnitT
}

func (c *checker) inferJump(x *ast.JumpExpr) *types.Type {
	switch x.Kind {
	case token.KindReturn:
		if c.cur == nil {
			return types.NeverT
		}
		if x.Val == nil {
			if c.cur.Ret.Kind != types.Unit {
				c.errf(x.Sp, "E0308", "this function must return a value of type `%s`", c.cur.Ret)
			}
			return types.NeverT
		}
		vt := c.checkExpr(x.Val, c.cur.Ret)
		if c.cur.Ret.Kind != types.Unit && !c.assignable(c.cur.Ret, vt) {
			c.errAt(x.Val, "E0308", "mismatched types: expected `%s`, found `%s`", c.cur.Ret, vt)
		}
		return types.NeverT
	case token.KindBreak:
		if c.loopDepth == 0 {
			c.errf(x.Sp, "E0268", "`break` outside of a loop")
		}
		if x.Val != nil {
			c.errAt(x.Val, "E0900", "`break` with a value is not supported by the compiler yet")
		}
		return types.NeverT
	case token.KindContinue:
		if c.loopDepth == 0 {
			c.errf(x.Sp, "E0268", "`continue` outside of a loop")
		}
		return types.NeverT
	}
	return types.NeverT
}

// --- type resolution ------------------------------------------------------

func (c *checker) resolveType(t ast.Type) *types.Type {
	switch x := t.(type) {
	case *ast.PathType:
		if len(x.Segments) == 0 {
			return nil
		}
		if x.Segments[0] == "_" {
			c.errf(x.Sp, "E0121", "the type placeholder `_` is not allowed here")
			return nil
		}
		if len(x.Segments) > 1 {
			c.errf(x.Sp, "E0900", "type paths are not supported by the compiler yet (roadmap M1)")
			return nil
		}
		if bt, ok := types.Builtins[x.Segments[0]]; ok {
			return bt
		}
		if st, ok := c.info.Structs[x.Segments[0]]; ok {
			return &types.Type{Kind: types.StructKind, St: st}
		}
		if en, ok := c.info.Enums[x.Segments[0]]; ok {
			return &types.Type{Kind: types.EnumKind, En: en}
		}
		c.errf(x.Sp, "E0412", "cannot find type `%s` in this scope", x.Segments[0])
		return nil
	case *ast.RefType:
		return types.RefTo(c.resolveType(x.Elem), x.Mut)
	case *ast.PtrType:
		return types.PtrTo(c.resolveType(x.Elem))
	case *ast.TupleType:
		if len(x.Elems) == 0 {
			return types.UnitT
		}
		c.errf(x.Sp, "E0900", "tuple types are not supported by the compiler yet (roadmap M1)")
		return nil
	case *ast.SliceType:
		return &types.Type{Kind: types.Slice, Elem: c.resolveType(x.Elem)}
	case *ast.ArrayType:
		elem := c.resolveType(x.Elem)
		n, ok := c.evalConst(x.Len)
		if !ok || n.Ty == nil || !n.Ty.Kind.IsInt() {
			c.errf(x.Sp, "E0308", "array length must be an integer constant")
			return &types.Type{Kind: types.Array, Elem: elem}
		}
		return &types.Type{Kind: types.Array, Elem: elem, Len: n.Int}
	case *ast.FnType:
		f := &types.Type{Kind: types.Fn}
		for _, p := range x.Params {
			f.Params = append(f.Params, c.resolveType(p))
		}
		if x.Ret != nil {
			f.Ret = c.resolveType(x.Ret)
		} else {
			f.Ret = types.UnitT
		}
		return f
	}
	return nil
}

// assignable reports whether a value of type src may be used where dst is
// expected. Zexi has no implicit conversions, so this is equality plus the
// `never`-coerces-to-anything rule and literal-driven types already unified by
// the caller.
func (c *checker) assignable(dst, src *types.Type) bool {
	if dst == nil || src == nil {
		return true
	}
	if src.Kind == types.Never || types.Equal(dst, src) {
		return true
	}
	// `&mut T` coerces to `&T`.
	if dst.Kind == types.Ref && src.Kind == types.Ref && dst.Mut == false && src.Mut == true {
		return types.Equal(dst.Elem, src.Elem)
	}
	return false
}

// --- scope helpers --------------------------------------------------------

func (c *checker) pushScope() { c.scope = append(c.scope, map[string]*types.Type{}) }
func (c *checker) popScope() {
	if len(c.scope) > 0 {
		c.scope = c.scope[:len(c.scope)-1]
	}
}

func (c *checker) declare(sp token.Span, name string, ty *types.Type) {
	if len(c.scope) == 0 {
		return
	}
	if _, dup := c.scope[len(c.scope)-1][name]; dup {
		// Shadowing is allowed in Zexi; this only guards against a completely
		// identical re-declaration, which is still legal but noisy. Do nothing.
	}
	c.scope[len(c.scope)-1][name] = ty
}

// --- helpers --------------------------------------------------------------

func isIntLiteral(e ast.Expr) bool {
	l, ok := e.(*ast.LiteralExpr)
	return ok && l.Kind == token.KindInteger
}

func isPrintable(t *types.Type) bool {
	if t == nil {
		return false
	}
	switch t.Kind {
	case types.Str, types.Char, types.Bool:
		return true
	}
	return t.Kind.IsNumeric()
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

// --- constant evaluation --------------------------------------------------

// ConstVal is a compile-time constant value.
type ConstVal struct {
	Ty      *types.Type
	Int     int64
	Uint    uint64
	Float   float64
	Str     string
	Bool    bool
	IsFloat bool
	IsStr   bool
	IsBool  bool
}

// evalConst folds a constant expression, supporting literals, unary minus, and
// the arithmetic and bitwise binary operators over integer and float values.
func (c *checker) evalConst(e ast.Expr) (ConstVal, bool) {
	switch x := e.(type) {
	case *ast.LiteralExpr:
		return c.evalConstLiteral(x)
	case *ast.UnExpr:
		if x.Op != token.KindMinus && x.Op != token.KindBang {
			return ConstVal{}, false
		}
		v, ok := c.evalConst(x.X)
		if !ok {
			return ConstVal{}, false
		}
		if v.IsFloat {
			if x.Op == token.KindMinus {
				v.Float = -v.Float
			}
			return v, true
		}
		if v.IsBool {
			if x.Op == token.KindBang {
				v.Bool = !v.Bool
			}
			return v, true
		}
		if x.Op == token.KindMinus {
			v.Int = -v.Int
			v.Uint = uint64(v.Int)
		} else {
			v.Int = ^v.Int
			v.Uint = ^v.Uint
		}
		return v, true
	case *ast.BinExpr:
		return c.evalConstBinary(x)
	}
	return ConstVal{}, false
}

func (c *checker) evalConstLiteral(x *ast.LiteralExpr) (ConstVal, bool) {
	switch x.Kind {
	case token.KindInteger:
		s := strings.ReplaceAll(x.Text, "_", "")
		if i, err := strconv.ParseInt(s, 0, 64); err == nil {
			return ConstVal{Ty: types.I32T, Int: i, Uint: uint64(i)}, true
		}
		if u, err := strconv.ParseUint(s, 0, 64); err == nil {
			v := ConstVal{Ty: &types.Type{Kind: types.U64}, Uint: u, Int: int64(u)}
			return v, true
		}
		return ConstVal{}, false
	case token.KindFloat:
		s := strings.ReplaceAll(x.Text, "_", "")
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return ConstVal{}, false
		}
		return ConstVal{Ty: types.F64T, Float: f, IsFloat: true}, true
	case token.KindTrue:
		return ConstVal{Ty: types.BoolT, Bool: true, IsBool: true}, true
	case token.KindFalse:
		return ConstVal{Ty: types.BoolT, Bool: false, IsBool: true}, true
	case token.KindString, token.KindRawString:
		return ConstVal{Ty: types.StrT, Str: x.Text, IsStr: true}, true
	case token.KindChar:
		r := firstRune(x.Text)
		return ConstVal{Ty: types.CharT, Int: int64(r), Uint: uint64(r)}, true
	case token.KindByteChar:
		r := firstRune(x.Text)
		return ConstVal{Ty: &types.Type{Kind: types.U8}, Int: int64(r), Uint: uint64(r)}, true
	}
	return ConstVal{}, false
}

func (c *checker) evalConstBinary(x *ast.BinExpr) (ConstVal, bool) {
	l, ok := c.evalConst(x.L)
	if !ok {
		return ConstVal{}, false
	}
	r, ok := c.evalConst(x.R)
	if !ok {
		return ConstVal{}, false
	}
	if l.IsFloat || r.IsFloat {
		lf, rf := l.Float, r.Float
		if !l.IsFloat {
			lf = float64(l.Int)
		}
		if !r.IsFloat {
			rf = float64(r.Int)
		}
		var f float64
		switch x.Op {
		case token.KindPlus:
			f = lf + rf
		case token.KindMinus:
			f = lf - rf
		case token.KindStar:
			f = lf * rf
		case token.KindSlash:
			f = lf / rf
		default:
			return ConstVal{}, false
		}
		return ConstVal{Ty: types.F64T, Float: f, IsFloat: true}, true
	}
	a, b := l.Int, r.Int
	var out int64
	switch x.Op {
	case token.KindPlus:
		out = a + b
	case token.KindMinus:
		out = a - b
	case token.KindStar:
		out = a * b
	case token.KindSlash:
		if b == 0 {
			return ConstVal{}, false
		}
		out = a / b
	case token.KindPercent:
		if b == 0 {
			return ConstVal{}, false
		}
		out = a % b
	case token.KindAmp:
		out = a & b
	case token.KindPipe:
		out = a | b
	case token.KindCaret:
		out = a ^ b
	case token.KindShl:
		out = a << uint(b)
	case token.KindShr:
		out = a >> uint(b)
	default:
		return ConstVal{}, false
	}
	return ConstVal{Ty: types.I32T, Int: out, Uint: uint64(out)}, true
}

// firstRune decodes the first decoded rune of a char literal's inner text.
func firstRune(s string) rune {
	r, _ := decodeCharInner(s)
	return r
}

// decodeCharInner decodes one character (with escapes) from a char literal's
// inner text.
func decodeCharInner(s string) (rune, bool) {
	if s == "" {
		return 0, false
	}
	if s[0] != '\\' {
		for _, r := range s {
			return r, true
		}
		return 0, false
	}
	return decodeEscape(s[1:])
}

func decodeEscape(s string) (rune, bool) {
	if s == "" {
		return 0, false
	}
	switch s[0] {
	case 'n':
		return '\n', true
	case 't':
		return '\t', true
	case 'r':
		return '\r', true
	case '0':
		return 0, true
	case '\\':
		return '\\', true
	case '\'':
		return '\'', true
	case '"':
		return '"', true
	case 'x':
		if len(s) >= 3 {
			if v, err := strconv.ParseUint(s[1:3], 16, 8); err == nil {
				return rune(v), true
			}
		}
	case 'u':
		if len(s) >= 2 && s[1] == '{' {
			if i := strings.IndexByte(s, '}'); i > 2 {
				if v, err := strconv.ParseUint(s[2:i], 16, 32); err == nil {
					return rune(v), true
				}
			}
		}
	}
	return 0, false
}

// DecodeString decodes the inner text of a string literal into its runtime
// bytes, resolving the standard escapes.
func DecodeString(s string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			i++
			continue
		}
		r, ok := decodeEscape(s[i+1:])
		if !ok {
			return "", false
		}
		b.WriteRune(r)
		// Advance past the escape sequence.
		i += 2
		switch s[i-1] {
		case 'x':
			i += 2
		case 'u':
			if i < len(s) && s[i] == '{' {
				for i < len(s) && s[i] != '}' {
					i++
				}
				i++ // consume '}'
			}
		}
	}
	return b.String(), true
}
