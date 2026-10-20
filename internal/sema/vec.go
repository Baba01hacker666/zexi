package sema

import (
	"zexi/internal/ast"
	"zexi/internal/token"
	"zexi/internal/types"
)

// This file implements the builtin `Vec<T>`: a growable, owning array. It is a
// compiler builtin rather than a library generic struct because growing one
// needs `realloc`, and the element destructor has to be generated per element
// type. Everything else about it is ordinary: a `Vec<T>` value is a move-only
// owning handle, `Vec<String>` drops its elements, and indexing is bounds
// checked at run time.

// vecCtorSigs are the associated constructors, `Vec::new()` and friends.
var vecCtorSigs = map[string][]*types.Type{
	"new":           nil,
	"with_capacity": {types.UsizeT},
}

// vecMethodSig describes a builtin method on `Vec<T>`.
type vecMethodSig struct {
	// params is the parameter list after the receiver. `elem` marks the
	// positions whose type is the element type `T`.
	params []*types.Type
	elem   []bool
	ret    *types.Type
	// retElem means the result type is `T` itself.
	retElem bool
	// retOptionElem means the result type is `Option<T>`.
	retOptionElem bool
	// mut marks a `&mut self` receiver.
	mut bool
}

var vecMethods = map[string]vecMethodSig{
	"len":         {ret: types.UsizeT},
	"is_empty":    {ret: types.BoolT},
	"capacity":    {ret: types.UsizeT},
	"clone":       {retElem: false, ret: nil}, // filled in per instantiation
	"push":        {params: []*types.Type{nil}, elem: []bool{true}, ret: types.UnitT, mut: true},
	"reserve":     {params: []*types.Type{types.UsizeT}, ret: types.UnitT, mut: true},
	"clear":       {ret: types.UnitT, mut: true},
	"truncate":    {params: []*types.Type{types.UsizeT}, ret: types.UnitT, mut: true},
	"pop":         {retOptionElem: true, mut: true},
	"remove":      {params: []*types.Type{types.UsizeT}, retElem: true, mut: true},
	"swap_remove": {params: []*types.Type{types.UsizeT}, retElem: true, mut: true},
	"insert":      {params: []*types.Type{types.UsizeT, nil}, elem: []bool{false, true}, ret: types.UnitT, mut: true},
}

// vecElem returns the element type of a `Vec<T>` type, or nil.
func vecElem(t *types.Type) *types.Type {
	if t == nil || t.Kind != types.VecKind {
		return nil
	}
	return t.Elem
}

// resolveVecType builds the semantic type for `Vec<..>`.
func (c *checker) resolveVecType(sp token.Span, args []ast.Type) *types.Type {
	if len(args) != 1 {
		c.errf(sp, "E0107", "`Vec` expects 1 type argument but %d were supplied", len(args))
		return nil
	}
	elem := c.resolveType(args[0])
	if elem == nil {
		return nil
	}
	if elem.Kind == types.Invalid {
		return nil
	}
	return &types.Type{Kind: types.VecKind, Elem: elem}
}

// vecResultType fills in the signatures whose result depends on `T`.
func (c *checker) vecResultType(sp token.Span, sig vecMethodSig, elem *types.Type) *types.Type {
	switch {
	case sig.retOptionElem:
		// `pop` yields `Option<T>`, instantiated through the ordinary generic
		// machinery so codegen sees an ordinary enum.
		if tmpl, ok := c.info.Enums["Option"]; ok {
			return c.instantiateEnum(sp, tmpl, []*types.Type{elem})
		}
		return &types.Type{Kind: types.Invalid}
	case sig.retElem:
		return elem
	case sig.ret != nil:
		return sig.ret
	default: // `clone`
		return &types.Type{Kind: types.VecKind, Elem: elem}
	}
}

// inferVecCtor checks `Vec::new()` and `Vec::with_capacity(n)`. The element
// type comes from the turbofish, or from the expected type when the call
// appears where one is known (`let v: Vec<i64> = Vec::new();`).
func (c *checker) inferVecCtor(x *ast.CallExpr, p *ast.PathExpr, name string, expected *types.Type) *types.Type {
	want, ok := vecCtorSigs[name]
	if !ok {
		c.errf(p.Sp, "E0599", "no function `Vec::%s`", name)
		for _, a := range x.Args {
			c.checkExpr(a, nil)
		}
		return &types.Type{Kind: types.Invalid}
	}
	var elem *types.Type
	if len(p.TypeArgs) > 0 {
		vt := c.resolveVecType(p.Sp, p.TypeArgs)
		elem = vecElem(vt)
	} else if expected != nil && expected.Kind == types.VecKind {
		elem = expected.Elem
	}
	if elem == nil {
		c.errf(p.Sp, "E0282", "cannot infer the element type of `Vec::%s`; annotate the binding or write `Vec::<T>::%s()`", name, name)
		for _, a := range x.Args {
			c.checkExpr(a, nil)
		}
		return &types.Type{Kind: types.Invalid}
	}
	if len(x.Args) != len(want) {
		c.errAt(x, "E0061", "`Vec::%s` takes %d argument(s) but %d were supplied", name, len(want), len(x.Args))
	}
	for i, a := range x.Args {
		var wt *types.Type
		if i < len(want) {
			wt = want[i]
		}
		at := c.checkExpr(a, wt)
		if wt != nil && !c.argAssignable(wt, at, false) {
			c.errAt(a, "E0308", "mismatched types: expected `%s`, found `%s`", wt, at)
		}
	}
	return &types.Type{Kind: types.VecKind, Elem: elem}
}

// inferVecMethod type-checks a builtin method call on a `Vec<T>`.
func (c *checker) inferVecMethod(x *ast.MethodCallExpr, recv *types.Type) *types.Type {
	elem := vecElem(recv)
	if elem == nil {
		elem = &types.Type{Kind: types.Invalid}
	}
	sig, ok := vecMethods[x.Name]
	if !ok {
		c.errAt(x, "E0599", "no method `%s` on type `%s`", x.Name, recv)
		for _, a := range x.Args {
			c.checkExpr(a, nil)
		}
		return nil
	}
	if sig.mut {
		c.checkMutable(x.Recv, x.Recv.Span(), "cannot borrow `%s` as mutable, as it is not declared `mut`")
	}
	if x.Name == "clone" && !elem.IsCopy() {
		// A deep copy would need a per-type element clone, which the language
		// has no trait for yet. Copy the elements by hand instead.
		c.errAt(x, "E0277", "`Vec<%s>` cannot be cloned because `%s` is not `Copy`; build a new `Vec` and push each element", elem, elem)
		return nil
	}
	want := make([]*types.Type, len(sig.params))
	for i, p := range sig.params {
		if i < len(sig.elem) && sig.elem[i] {
			want[i] = elem
		} else {
			want[i] = p
		}
	}
	if len(x.Args) != len(want) {
		c.errAt(x, "E0061", "method `Vec::%s` takes %d argument(s) but %d were supplied", x.Name, len(want), len(x.Args))
	}
	for i, a := range x.Args {
		var wt *types.Type
		if i < len(want) {
			wt = want[i]
		}
		at := c.checkExpr(a, wt)
		if wt != nil && !c.argAssignable(wt, at, false) {
			c.errAt(a, "E0308", "mismatched types: expected `%s`, found `%s`", wt, at)
		}
	}
	return c.vecResultType(x.Sp, sig, elem)
}

// VecMethodMut reports whether the builtin `Vec` method takes `&mut self`.
// Codegen uses it to decide whether the receiver is written through.
func VecMethodMut(name string) bool { return vecMethods[name].mut }

// IsVecMethod reports whether name is a builtin `Vec` method.
func IsVecMethod(name string) bool {
	_, ok := vecMethods[name]
	return ok
}
