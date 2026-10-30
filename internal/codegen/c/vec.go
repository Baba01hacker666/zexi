package c

import (
	"fmt"
	"strings"

	"zexi/internal/ast"
	"zexi/internal/types"
)

// This file lowers the builtin `Vec<T>` to C. A Vec is a small handle
//
//	struct zx_vec_<T> { T *ptr; size_t len; size_t cap; };
//
// with one set of generated helpers per element type, exactly like the array
// wrapper structs. The element type appears only behind a pointer in the
// handle, so a Vec struct definition never needs its element to be complete;
// the helpers, which do touch elements, are emitted after every destructor has
// been declared.

// vecTypeName is the deterministic C name of a Vec handle struct.
func vecTypeName(t *types.Type) string {
	if t == nil || t.Elem == nil {
		return "zx_vec_void"
	}
	return "zx_vec_" + typeMangle(t.Elem)
}

// collectVecTypes records every Vec type reachable from the program so its
// handle struct and helpers can be emitted.
func (g *gen) collectVecTypes() {
	var walk func(t *types.Type)
	walk = func(t *types.Type) {
		if t == nil {
			return
		}
		switch t.Kind {
		case types.VecKind:
			name := vecTypeName(t)
			if g.vecSeen[name] {
				return
			}
			g.vecSeen[name] = true
			walk(t.Elem)
			g.vecs[name] = t
			g.vecOrder = append(g.vecOrder, name)
		case types.Array, types.Ptr, types.Ref, types.Slice:
			walk(t.Elem)
		case types.Tuple:
			for _, e := range t.Elems {
				walk(e)
			}
		case types.StructKind:
			if t.St == nil || g.vecSeen["#"+t.St.Name] {
				return
			}
			g.vecSeen["#"+t.St.Name] = true
			for _, f := range t.St.Fields {
				walk(f.Ty)
			}
		case types.EnumKind:
			if t.En == nil || g.vecSeen["#"+t.En.Name] {
				return
			}
			g.vecSeen["#"+t.En.Name] = true
			for _, v := range t.En.Variants {
				for _, f := range v.Fields {
					walk(f.Ty)
				}
			}
		}
	}
	for _, t := range g.info.Types {
		walk(t)
	}
	for _, t := range g.info.DeclTypes {
		walk(t)
	}
	for _, f := range g.info.FnOrder {
		for _, p := range f.Params {
			walk(p)
		}
		walk(f.Ret)
	}
	for _, st := range g.info.StructOrd {
		for _, f := range st.Fields {
			walk(f.Ty)
		}
	}
	for _, en := range g.info.EnumOrd {
		for _, v := range en.Variants {
			for _, f := range v.Fields {
				walk(f.Ty)
			}
		}
	}
	if len(g.vecOrder) > 0 {
		// The helpers report allocation failures and bad indices.
		g.hasPanic = true
	}
}

// vecForwardDecls emits the incomplete typedefs. They must precede every struct
// definition, because a struct may hold a Vec by value.
func (g *gen) vecForwardDecls() {
	for _, name := range g.vecOrder {
		g.w.line("typedef struct %s %s;", name, name)
	}
}

// vecDefs emits the handle structs.
func (g *gen) vecDefs() {
	if len(g.vecOrder) == 0 {
		return
	}
	g.w.line("")
	for _, name := range g.vecOrder {
		t := g.vecs[name]
		g.w.line("// Vec<%s>", t.Elem)
		g.w.line("struct %s {", name)
		g.w.indent++
		g.w.line("%s *ptr;", ctype(t.Elem))
		g.w.line("size_t len;")
		g.w.line("size_t cap;")
		g.w.indent--
		g.w.line("};")
	}
	g.w.line("")
}

// vecRuntimeProtos declares the helpers; vecRuntime defines them.
func (g *gen) vecRuntimeProtos() {
	link := "static "
	if g.split {
		link = ""
	}
	for _, name := range g.vecOrder {
		t := g.vecs[name]
		et := ctype(t.Elem)
		g.w.line("%svoid zx_vec_reserve_%s(%s *v, size_t want);", link, name, name)
		g.w.line("%svoid zx_vec_push_%s(%s *v, %s x);", link, name, name, et)
		g.w.line("%s%s zx_vec_pop_%s(%s *v);", link, et, name, name)
		g.w.line("%s%s zx_vec_remove_%s(%s *v, size_t i);", link, et, name, name)
		g.w.line("%s%s zx_vec_swap_remove_%s(%s *v, size_t i);", link, et, name, name)
		g.w.line("%svoid zx_vec_insert_%s(%s *v, size_t i, %s x);", link, name, name, et)
		g.w.line("%svoid zx_vec_truncate_%s(%s *v, size_t n);", link, name, name)
		if t.Elem.IsCopy() {
			g.w.line("%s%s zx_vec_clone_%s(const %s *v);", link, name, name, name)
		}
	}
	g.w.line("")
}

// vecRuntime emits the per-element-type helpers. It runs after dropFuncs so the
// destructors they call have been declared.
func (g *gen) vecRuntime() {
	if len(g.vecOrder) == 0 {
		return
	}
	if g.split {
		g.w, g.core = g.core, g.w
	}
	for _, name := range g.vecOrder {
		g.emitVecRuntime(name, g.vecs[name])
	}
	if g.split {
		g.w, g.core = g.core, g.w
	}
}

func (g *gen) emitVecRuntime(name string, t *types.Type) {
	et := ctype(t.Elem)
	lp := g.linkPrefix()
	dropElem := t.Elem.NeedsDrop()
	elemMangle := typeMangle(t.Elem)

	g.w.line("// Vec<%s> helpers", t.Elem)
	g.w.line("%svoid zx_vec_reserve_%s(%s *v, size_t want) {", lp, name, name)
	g.w.line("    if (want <= v->cap) return;")
	g.w.line(`    if (want > SIZE_MAX / sizeof(%s)) zx_panicf("capacity overflow");`, et)
	g.w.line("    size_t cap = v->cap ? v->cap : 4;")
	g.w.line("    while (cap < want) cap = (cap > SIZE_MAX / 2) ? want : cap * 2;")
	g.w.line("    %s *np = (%s *)realloc(v->ptr, cap * sizeof(%s));", et, et, et)
	g.w.line(`    if (!np) zx_panicf("out of memory");`)
	g.w.line("    v->ptr = np;")
	g.w.line("    v->cap = cap;")
	g.w.line("}")
	g.w.line("")

	g.w.line("%svoid zx_vec_push_%s(%s *v, %s x) {", lp, name, name, et)
	g.w.line(`    if (v->len == SIZE_MAX) zx_panicf("capacity overflow");`)
	g.w.line("    zx_vec_reserve_%s(v, v->len + 1);", name)
	g.w.line("    v->ptr[v->len++] = x;")
	g.w.line("}")
	g.w.line("")

	g.w.line("%s%s zx_vec_pop_%s(%s *v) {", lp, et, name, name)
	g.w.line(`    if (v->len == 0) zx_panicf("pop from an empty Vec");`)
	g.w.line("    %s x = v->ptr[--v->len];", et)
	if dropElem {
		// Zero the vacated slot: the buffer stays allocated, and a later push
		// must not leave a stale handle that could be dropped twice.
		g.w.line("    memset(&v->ptr[v->len], 0, sizeof(%s));", et)
	}
	g.w.line("    return x;")
	g.w.line("}")
	g.w.line("")

	g.w.line("%s%s zx_vec_remove_%s(%s *v, size_t i) {", lp, et, name, name)
	g.w.line(`    if (i >= v->len) zx_panicf("removal index %%zu out of bounds for length %%zu", i, v->len);`)
	g.w.line("    %s x = v->ptr[i];", et)
	g.w.line("    if (i + 1 < v->len) memmove(&v->ptr[i], &v->ptr[i + 1], (v->len - i - 1) * sizeof(%s));", et)
	g.w.line("    v->len--;")
	if dropElem {
		g.w.line("    memset(&v->ptr[v->len], 0, sizeof(%s));", et)
	}
	g.w.line("    return x;")
	g.w.line("}")
	g.w.line("")

	g.w.line("%s%s zx_vec_swap_remove_%s(%s *v, size_t i) {", lp, et, name, name)
	g.w.line(`    if (i >= v->len) zx_panicf("removal index %%zu out of bounds for length %%zu", i, v->len);`)
	g.w.line("    %s x = v->ptr[i];", et)
	g.w.line("    v->len--;")
	g.w.line("    if (i != v->len) v->ptr[i] = v->ptr[v->len];")
	if dropElem {
		g.w.line("    memset(&v->ptr[v->len], 0, sizeof(%s));", et)
	}
	g.w.line("    return x;")
	g.w.line("}")
	g.w.line("")

	g.w.line("%svoid zx_vec_insert_%s(%s *v, size_t i, %s x) {", lp, name, name, et)
	g.w.line(`    if (i > v->len) zx_panicf("insertion index %%zu out of bounds for length %%zu", i, v->len);`)
	g.w.line(`    if (v->len == SIZE_MAX) zx_panicf("capacity overflow");`)
	g.w.line("    zx_vec_reserve_%s(v, v->len + 1);", name)
	g.w.line("    if (i < v->len) memmove(&v->ptr[i + 1], &v->ptr[i], (v->len - i) * sizeof(%s));", et)
	g.w.line("    v->ptr[i] = x;")
	g.w.line("    v->len++;")
	g.w.line("}")
	g.w.line("")

	g.w.line("%svoid zx_vec_truncate_%s(%s *v, size_t n) {", lp, name, name)
	g.w.line("    if (n >= v->len) return;")
	if dropElem {
		g.w.line("    for (size_t i = n; i < v->len; i++) zx_drop_%s(&v->ptr[i]);", elemMangle)
	}
	g.w.line("    v->len = n;")
	g.w.line("}")

	if t.Elem.IsCopy() {
		g.w.line("")
		g.w.line("%s%s zx_vec_clone_%s(const %s *v) {", lp, name, name, name)
		g.w.line("    %s r = { 0, v->len, v->len };", name)
		g.w.line("    if (v->len) {")
		g.w.line("        r.ptr = (%s *)malloc(v->len * sizeof(%s));", et, et)
		g.w.line(`        if (!r.ptr) zx_panicf("out of memory");`)
		g.w.line("        memcpy(r.ptr, v->ptr, v->len * sizeof(%s));", et)
		g.w.line("    }")
		g.w.line("    return r;")
		g.w.line("}")
	}
	g.w.line("")
}

// vecCtorValue lowers `Vec::new()` and `Vec::with_capacity(n)`.
func (g *gen) vecCtorValue(x *ast.CallExpr, name string) string {
	t := g.info.TypeOf(x)
	if t == nil || t.Kind != types.VecKind {
		return "0 /* unresolved Vec */"
	}
	if name == "new" || len(x.Args) == 0 {
		return "((" + ctype(t) + "){ 0 })"
	}
	tm := g.tmpName()
	return "({ " + ctype(t) + " " + tm + " = { 0 }; zx_vec_reserve_" + vecTypeName(t) +
		"(&" + tm + ", (size_t)(" + g.value(x.Args[0]) + ")); " + tm + "; })"
}

// vecReceiverValue lowers a Vec receiver to the handle itself. A borrowed
// receiver (`&Vec<T>` or `&mut Vec<T>`, as in a parameter) is a `Vec<T> *` in
// C, so it is dereferenced once per reference level.
func (g *gen) vecReceiverValue(e ast.Expr) string {
	recv := g.value(e)
	for t := g.info.TypeOf(e); t != nil && (t.Kind == types.Ref || t.Kind == types.Ptr); t = t.Elem {
		recv = "(*" + recv + ")"
	}
	return recv
}

// vecBorrowedReceiver reports whether a receiver is a reference rather than a
// Vec value. Borrowed receivers are not temporaries: they must not be
// materialized and dropped.
func vecBorrowedReceiver(t *types.Type) bool {
	return t != nil && (t.Kind == types.Ref || t.Kind == types.Ptr)
}

// vecMethodCall lowers a builtin method call on a `Vec<T>`. A call on a
// temporary receiver borrows it for the duration and drops it afterwards.
func (g *gen) vecMethodCall(x *ast.MethodCallExpr) string {
	t := recvBase(g.info.TypeOf(x.Recv))
	if t == nil || t.Kind != types.VecKind {
		return "0 /* unresolved Vec method */"
	}
	vn := vecTypeName(t)
	recv := g.vecReceiverValue(x.Recv)
	var prologue, epilogue string
	if !isPlaceExpr(x.Recv) && !vecBorrowedReceiver(g.info.TypeOf(x.Recv)) {
		tm := g.tmpName()
		prologue = ctype(t) + " " + tm + " = " + recv + "; "
		recv = tm
		epilogue = " zx_drop_" + typeMangle(t) + "(&" + tm + ");"
	}
	// expr is the call's value; a void method uses "(void)0".
	var expr string
	switch x.Name {
	case "len":
		expr = "(" + recv + ").len"
	case "is_empty":
		expr = "((" + recv + ").len == 0)"
	case "capacity":
		expr = "(" + recv + ").cap"
	case "clear":
		expr = "zx_vec_truncate_" + vn + "(&" + recv + ", 0)"
	case "reserve":
		expr = "zx_vec_reserve_" + vn + "(&" + recv + ", (size_t)(" + g.value(x.Args[0]) + "))"
	case "truncate":
		expr = "zx_vec_truncate_" + vn + "(&" + recv + ", (size_t)(" + g.value(x.Args[0]) + "))"
	case "push":
		// A non-`Copy` element moves into the buffer, leaving the caller's
		// copy empty so the buffer is not freed twice.
		expr = "zx_vec_push_" + vn + "(&" + recv + ", " + g.consume(x.Args[0]) + ")"
	case "insert":
		expr = "zx_vec_insert_" + vn + "(&" + recv + ", (size_t)(" + g.value(x.Args[0]) + "), " +
			g.consume(x.Args[1]) + ")"
	case "clone":
		expr = "zx_vec_clone_" + vn + "(&" + recv + ")"
	case "pop":
		// `pop` yields `Option<T>`, so the empty case builds `None` instead of
		// faulting. The runtime helper's own empty check stays as a backstop.
		return "({ " + prologue + popOption(recv, vn, g.info.TypeOf(x), t.Elem) + epilogue + " })"
	case "each", "count", "any", "all", "fold":
		return g.vecClosureLoop(x, recv, t)

	case "remove", "swap_remove":
		rt := g.info.TypeOf(x)
		res := g.tmpName()
		fn := "zx_vec_remove_"
		if x.Name == "swap_remove" {
			fn = "zx_vec_swap_remove_"
		}
		call := fn + vn + "(&" + recv + ", (size_t)(" + g.value(x.Args[0]) + "))"
		return "({ " + prologue + ctype(rt) + " " + res + " = " + call + ";" + epilogue + " " + res + "; })"
	default:
		return "0 /* unhandled Vec method */"
	}
	return "({ " + prologue + expr + ";" + epilogue + " })"
}

// vecClosureLoop lowers the closure-taking Vec methods (`each`, `count`, `any`,
// `all`) to an inline loop over the buffer. The element is copied per
// iteration, which is why sema requires `Copy` elements.
func (g *gen) vecClosureLoop(x *ast.MethodCallExpr, recv string, t *types.Type) string {
	elem := t.Elem
	argIdx := 0
	if x.Name == "fold" {
		argIdx = 1
	}
	fnTy := g.info.TypeOf(x.Args[argIdx])
	if fnTy == nil || fnTy.Kind != types.Fn {
		return "0 /* closure type missing */"
	}
	f := g.tmpName()
	ctr := g.tmpName()
	el := g.tmpName()
	vec := g.tmpName()
	call := g.callClosure(f, fnTy, []string{el})
	acc := g.tmpName()
	var head, tail string
	switch x.Name {
	case "each":
		head, tail = "", ""
	case "count":
		head, tail = "size_t zx_n = 0;", "zx_n"
	case "any":
		head, tail = "bool zx_ok = false;", "zx_ok"
	case "all":
		head, tail = "bool zx_ok = true;", "zx_ok"
	case "fold":
		head, tail = ctype(elem)+" "+acc+" = "+g.consume(x.Args[0])+";", acc
	}
	var body string
	switch x.Name {
	case "each":
		body = call + ";"
	case "count":
		body = "if (" + call + ") zx_n++;"
	case "any":
		body = "if (" + call + ") { zx_ok = true; break; }"
	case "all":
		body = "if (!(" + call + ")) { zx_ok = false; break; }"
	case "fold":
		body = acc + " = " + g.callClosure(f, fnTy, []string{acc, el}) + ";"
	}
	out := "({ "
	out += ctype(t) + " " + vec + " = " + recv + "; "
	out += "void *" + f + " = " + g.value(x.Args[argIdx]) + "; "
	out += head + " "
	out += "size_t " + ctr + "; "
	out += "for (" + ctr + " = 0; " + ctr + " < (size_t)(" + vec + ").len; " + ctr + "++) { "
	out += ctype(elem) + " " + el + " = (" + vec + ").ptr[" + ctr + "]; "
	out += body + " } "
	if tail != "" {
		out += tail + "; "
	}
	out += "})"
	return out
}

// callClosure emits a call through a function value: the record carries both
// the function and the environment it runs with.
func (g *gen) callClosure(callee string, fnTy *types.Type, args []string) string {
	params := make([]string, 0, len(fnTy.Params)+1)
	for _, p := range fnTy.Params {
		params = append(params, ctype(p))
	}
	sig := ctype(fnTy.Ret) + " (*)(void *"
	if len(params) > 0 {
		sig += ", " + strings.Join(params, ", ")
	}
	sig += ")"
	all := append([]string{"(void *)" + callee}, args...)
	return "((" + sig + ")(((zx_Closure *)" + callee + ")->fn))(" + strings.Join(all, ", ") + ")"
}

// popOption builds the `Option<T>` result of `pop`: `None` when the Vec is
// empty, `Some(x)` holding the removed element otherwise. opt is the semantic
// `Option<T>` type and elem the element type.
func popOption(recv, vn string, opt, elem *types.Type) string {
	res, el := "zx__o", "zx__e"
	someTag, someField, noneTag := 0, "f0", 1
	if opt != nil && opt.Kind == types.EnumKind && opt.En != nil {
		for _, v := range opt.En.Variants {
			switch v.Name {
			case "Some":
				someTag = v.Index
				if len(v.Fields) > 0 {
					someField = fieldName(v.Fields[0].Name)
				}
			case "None":
				noneTag = v.Index
			}
		}
	}
	rt, ot := ctype(opt), ctype(elem)
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s; ", rt, res)
	fmt.Fprintf(&b, "if ((%s).len == 0) { %s = ((%s){ .tag = %d }); } ", recv, res, rt, noneTag)
	fmt.Fprintf(&b, "else { %s %s = zx_vec_pop_%s(&%s); ", ot, el, vn, recv)
	fmt.Fprintf(&b, "%s = ((%s){ .tag = %d, .u.v%d = { .%s = %s } }); } ",
		res, rt, someTag, someTag, someField, el)
	fmt.Fprintf(&b, "%s; ", res)
	return b.String()
}

// vecIndex lowers `v[i]`, bounds-checked against the live length. The receiver
// is already parenthesized by the caller.
func vecIndex(recv, idx string) string {
	return recv + ".ptr[zx_bounds((int64_t)(" + idx + "), (int64_t)" + recv + ".len)]"
}

// vecForLoop lowers `for x in v` over a Vec of `Copy` elements. The handle is
// read in place rather than copied: copying it would alias the buffer, and the
// copy's destructor would then free the caller's memory.
func (g *gen) vecForLoop(x *ast.LoopExpr) string {
	it := recvBase(g.info.TypeOf(x.Iter))
	if it == nil || it.Kind != types.VecKind {
		return ""
	}
	recv := g.vecReceiverValue(x.Iter)
	if !isPlaceExpr(x.Iter) && !vecBorrowedReceiver(g.info.TypeOf(x.Iter)) {
		// Materialize the temporary and register it in the *enclosing* drop
		// frame, so a `break` out of the loop still frees it at scope exit.
		tm := g.tmpName()
		g.w.line("%s %s = %s;", ctype(it), tm, recv)
		g.registerDrop(tm, it)
		recv = tm
	}
	g.pushScope()
	name := g.bindLoopVar(x.Pat)
	ctr := g.tmpName()
	g.w.line("for (size_t %s = 0; %s < (size_t)(%s).len; %s++) {", ctr, ctr, recv, ctr)
	g.w.indent++
	g.w.line("%s %s = (%s).ptr[%s];", ctype(it.Elem), name, recv, ctr)
	res := g.loopResult(x)
	brk, cont := g.openLoop(res)
	g.blockStmts(x.Body, nil)
	g.closeLoop()
	g.endLoopBody(cont)
	g.w.indent--
	g.w.line("}")
	g.w.line("%s: ;", brk)
	g.popScope()
	return res
}

// vecLenValue lowers `len(v)`.
func (g *gen) vecLenValue(x *ast.CallExpr) string {
	if len(x.Args) != 1 {
		return "0"
	}
	if isPlaceExpr(x.Args[0]) || vecBorrowedReceiver(g.info.TypeOf(x.Args[0])) {
		return "(" + g.vecReceiverValue(x.Args[0]) + ").len"
	}
	t := g.info.TypeOf(x.Args[0])
	tm := g.tmpName()
	return "({ " + ctype(t) + " " + tm + " = " + g.value(x.Args[0]) +
		"; size_t zx_n = " + tm + ".len; zx_drop_" + typeMangle(t) + "(&" + tm + "); zx_n; })"
}
