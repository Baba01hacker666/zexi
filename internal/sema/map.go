package sema

import (
	"zexi/internal/ast"
	"zexi/internal/token"
	"zexi/internal/types"
)

// This file implements the builtin `HashMap<K, V>`: a growable, owning hash
// map. It is a compiler builtin rather than a library generic struct for the
// same reason `Vec<T>` is: it owns a heap table that must be reallocated, and
// its element destructors have to be generated per key/value type.
//
// Hashing is built in for a closed set of key types — the integer types,
// `bool`, `char`, `str`, and `String`. A real `Hash` trait would widen this;
// the seam is `isHashableKey` plus the generated `zx_hash_*` helpers.

// mapCtorSigs are the associated constructors.
var mapCtorSigs = map[string][]*types.Type{
	"new": nil,
}

// mapMethodSig describes a builtin method on `HashMap<K, V>`.
type mapMethodSig struct {
	// params is the parameter list after the receiver. `isKey` marks the
	// positions whose type is the key type `K`.
	params []*types.Type
	isKey  []bool
	ret    *types.Type
	// retOptionVal means the result is `Option<V>` (by value).
	retOptionVal bool
	// retOptionRef means the result is `Option<&V>`.
	retOptionRef bool
	// mut marks a `&mut self` receiver.
	mut bool
}

var mapMethods = map[string]mapMethodSig{
	"len":          {ret: types.UsizeT},
	"is_empty":     {ret: types.BoolT},
	"insert":       {params: []*types.Type{nil, nil}, isKey: []bool{true, false}, retOptionVal: true, mut: true},
	"get":          {params: []*types.Type{nil}, isKey: []bool{true}, retOptionRef: true},
	"remove":       {params: []*types.Type{nil}, isKey: []bool{true}, retOptionVal: true, mut: true},
	"contains_key": {params: []*types.Type{nil}, isKey: []bool{true}, ret: types.BoolT},
	"clear":        {ret: types.UnitT, mut: true},
}

// mapKeyVal returns the key and value types of a `HashMap<K, V>` type.
func mapKeyVal(t *types.Type) (*types.Type, *types.Type) {
	if t == nil || t.Kind != types.MapKind {
		return nil, nil
	}
	return t.Key, t.Val
}

// isHashableKey reports whether a key type has a built-in hash.
func isHashableKey(t *types.Type) bool {
	if t == nil {
		return false
	}
	switch t.Kind {
	case types.I8, types.I16, types.I32, types.I64, types.Isize,
		types.U8, types.U16, types.U32, types.U64, types.Usize,
		types.Bool, types.Char, types.Str, types.StringKind:
		return true
	}
	return false
}

// resolveMapType builds the semantic type for `HashMap<..>`.
func (c *checker) resolveMapType(sp token.Span, args []ast.Type) *types.Type {
	if len(args) != 2 {
		c.errf(sp, "E0107", "`HashMap` expects 2 type arguments but %d were supplied", len(args))
		return nil
	}
	key := c.resolveType(args[0])
	val := c.resolveType(args[1])
	if key == nil || val == nil {
		return nil
	}
	if key.Kind == types.Invalid || val.Kind == types.Invalid {
		return nil
	}
	if !isHashableKey(key) {
		c.errf(sp, "E0308", "`%s` cannot be used as a `HashMap` key; only integers, `bool`, `char`, `str`, and `String` have a built-in hash", key)
		return nil
	}
	return &types.Type{Kind: types.MapKind, Key: key, Val: val}
}

// optionOf instantiates `Option<T>` (or `Option<&T>` for byRef) through the
// ordinary generic machinery so codegen sees an ordinary enum.
func (c *checker) optionOf(sp token.Span, elem *types.Type, byRef bool) *types.Type {
	if byRef {
		elem = types.RefTo(elem, false)
	}
	if tmpl, ok := c.info.Enums["Option"]; ok {
		return c.instantiateEnum(sp, tmpl, []*types.Type{elem})
	}
	return &types.Type{Kind: types.Invalid}
}

// inferMapCtor checks `HashMap::new()`. The key and value types come from the
// turbofish, or from the expected type when the call appears where one is
// known (`let m: HashMap<i32, String> = HashMap::new();`).
func (c *checker) inferMapCtor(x *ast.CallExpr, p *ast.PathExpr, name string, expected *types.Type) *types.Type {
	want, ok := mapCtorSigs[name]
	if !ok {
		c.errf(p.Sp, "E0599", "no function `HashMap::%s`", name)
		for _, a := range x.Args {
			c.checkExpr(a, nil)
		}
		return &types.Type{Kind: types.Invalid}
	}
	var key, val *types.Type
	if len(p.TypeArgs) > 0 {
		mt := c.resolveMapType(p.Sp, p.TypeArgs)
		key, val = mapKeyVal(mt)
	} else if expected != nil && expected.Kind == types.MapKind {
		key, val = expected.Key, expected.Val
	}
	if key == nil || val == nil {
		c.errf(p.Sp, "E0282", "cannot infer the element type of `HashMap::%s`; annotate the binding or write `HashMap::<K, V>::%s()`", name, name)
		for _, a := range x.Args {
			c.checkExpr(a, nil)
		}
		return &types.Type{Kind: types.Invalid}
	}
	if len(x.Args) != len(want) {
		c.errAt(x, "E0061", "`HashMap::%s` takes %d argument(s) but %d were supplied", name, len(want), len(x.Args))
	}
	return &types.Type{Kind: types.MapKind, Key: key, Val: val}
}

// inferMapMethod type-checks a builtin method call on a `HashMap<K, V>`.
func (c *checker) inferMapMethod(x *ast.MethodCallExpr, recv *types.Type) *types.Type {
	key, val := mapKeyVal(recv)
	if key == nil || val == nil {
		key = &types.Type{Kind: types.Invalid}
		val = &types.Type{Kind: types.Invalid}
	}
	sig, ok := mapMethods[x.Name]
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
	want := make([]*types.Type, len(sig.params))
	for i, p := range sig.params {
		if i < len(sig.isKey) && sig.isKey[i] {
			want[i] = key
		} else {
			want[i] = p
		}
	}
	if len(x.Args) != len(want) {
		c.errAt(x, "E0061", "method `HashMap::%s` takes %d argument(s) but %d were supplied", x.Name, len(want), len(x.Args))
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
	switch {
	case sig.retOptionVal:
		return c.optionOf(x.Sp, val, false)
	case sig.retOptionRef:
		return c.optionOf(x.Sp, val, true)
	default:
		return sig.ret
	}
}

// IsMapMethod reports whether name is a builtin `HashMap` method.
func IsMapMethod(name string) bool {
	_, ok := mapMethods[name]
	return ok
}

// MapMethodMut reports whether the builtin `HashMap` method takes `&mut self`.
func MapMethodMut(name string) bool { return mapMethods[name].mut }
