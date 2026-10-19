package ast

import "reflect"

// Clone returns a deep copy of an AST value. It is used to give every generic
// instantiation its own expression nodes: the checker keys inferred types by
// `ast.Expr` pointer, so two monomorphizations of one body must not share
// nodes.
//
// The copy is reflection-based (like codegen's tree walk), so new node kinds
// are covered automatically without a hand-written copier.
func Clone[T any](v T) T {
	out := deepCopy(reflect.ValueOf(v))
	if !out.IsValid() {
		return v
	}
	return out.Interface().(T)
}

func deepCopy(v reflect.Value) reflect.Value {
	if !v.IsValid() {
		return v
	}
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		inner := deepCopy(v.Elem())
		n := reflect.New(v.Type()).Elem()
		n.Set(inner)
		return n
	case reflect.Ptr:
		if v.IsNil() {
			return v
		}
		n := reflect.New(v.Type().Elem())
		n.Elem().Set(deepCopy(v.Elem()))
		return n
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		n := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			n.Index(i).Set(deepCopy(v.Index(i)))
		}
		return n
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		n := reflect.MakeMapWithSize(v.Type(), v.Len())
		iter := v.MapRange()
		for iter.Next() {
			n.SetMapIndex(deepCopy(iter.Key()), deepCopy(iter.Value()))
		}
		return n
	case reflect.Struct:
		n := reflect.New(v.Type()).Elem()
		for i := 0; i < v.NumField(); i++ {
			if n.Field(i).CanSet() {
				n.Field(i).Set(deepCopy(v.Field(i)))
			}
		}
		return n
	case reflect.Array:
		n := reflect.New(v.Type()).Elem()
		for i := 0; i < v.Len(); i++ {
			n.Index(i).Set(deepCopy(v.Index(i)))
		}
		return n
	default:
		return v
	}
}
