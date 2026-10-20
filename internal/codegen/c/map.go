package c

import (
	"fmt"
	"strings"

	"zexi/internal/ast"
	"zexi/internal/types"
)

// This file lowers the builtin `HashMap<K, V>` to C. A map is a small handle
//
//	struct zx_hashmap_<K>_<V> {
//	    zx_hashpair_<K>_<V> *entries; // { K key; V value; int used; }
//	    size_t len;
//	    size_t cap;
//	};
//
// with open addressing (linear probing). Hashing is built in for the key types
// `isHashableKey` accepts; the generated `zx_hash_*`/`zx_mkey_eq_*` helpers
// mirror that closed set.

// mapTypeName is the deterministic C name of a HashMap handle struct.
func mapTypeName(t *types.Type) string {
	if t == nil || t.Key == nil || t.Val == nil {
		return "zx_hashmap_void"
	}
	return "zx_hashmap_" + typeMangle(t.Key) + "_" + typeMangle(t.Val)
}

// hashPairName is the C name of one open-addressing slot.
func hashPairName(t *types.Type) string {
	return "zx_hashpair_" + typeMangle(t.Key) + "_" + typeMangle(t.Val)
}

// hashKeyName is the C name of the key hasher for a map's key type.
func hashKeyName(t *types.Type) string {
	return "zx_hash_" + typeMangle(t.Key)
}

// mapEqName is the C name of the key equality predicate.
func mapEqName(t *types.Type) string {
	return "zx_mkey_eq_" + typeMangle(t.Key)
}

// collectMapTypes records every HashMap type reachable from the program so
// its handle struct and helpers can be emitted.
func (g *gen) collectMapTypes() {
	var walk func(t *types.Type)
	walk = func(t *types.Type) {
		if t == nil {
			return
		}
		switch t.Kind {
		case types.MapKind:
			name := mapTypeName(t)
			if g.mapSeen[name] {
				return
			}
			g.mapSeen[name] = true
			walk(t.Key)
			walk(t.Val)
			g.maps[name] = t
			g.mapOrder = append(g.mapOrder, name)
		case types.Array, types.Ptr, types.Ref, types.Slice:
			walk(t.Elem)
		case types.Tuple:
			for _, e := range t.Elems {
				walk(e)
			}
		case types.StructKind:
			if t.St == nil || g.mapSeen["#"+t.St.Name] {
				return
			}
			g.mapSeen["#"+t.St.Name] = true
			for _, f := range t.St.Fields {
				walk(f.Ty)
			}
		case types.EnumKind:
			if t.En == nil || g.mapSeen["#"+t.En.Name] {
				return
			}
			g.mapSeen["#"+t.En.Name] = true
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
}

// mapForwardDecls emits the incomplete typedefs. They must precede every
// struct definition, because a struct may hold a HashMap by value.
func (g *gen) mapForwardDecls() {
	for _, name := range g.mapOrder {
		t := g.maps[name]
		g.w.line("typedef struct %s %s;", hashPairName(t), hashPairName(t))
		g.w.line("typedef struct %s %s;", name, name)
	}
}

// mapDefs emits the slot and handle structs.
func (g *gen) mapDefs() {
	if len(g.mapOrder) == 0 {
		return
	}
	g.w.line("")
	for _, name := range g.mapOrder {
		t := g.maps[name]
		pn, mn := hashPairName(t), mapTypeName(t)
		g.w.line("struct %s {", pn)
		g.w.indent++
		g.w.line("%s key;", ctype(t.Key))
		g.w.line("%s value;", ctype(t.Val))
		g.w.line("int used;")
		g.w.indent--
		g.w.line("};")
		g.w.line("struct %s {", mn)
		g.w.indent++
		g.w.line("%s *entries;", pn)
		g.w.line("size_t len;")
		g.w.line("size_t cap;")
		g.w.indent--
		g.w.line("};")
	}
}

// mapRuntime emits the hash and map helpers. It runs after dropFuncs so the
// element destructors they call have been declared.
func (g *gen) mapRuntime() {
	if len(g.mapOrder) == 0 {
		return
	}
	hasStrKey := false
	hasStringKey := false
	for _, name := range g.mapOrder {
		switch g.maps[name].Key.Kind {
		case types.Str:
			hasStrKey = true
		case types.StringKind:
			hasStringKey = true
		}
	}
	g.w.line("")
	g.w.line("// HashMap helpers")
	g.w.line("static uint64_t zx_hash_mix(uint64_t x) {")
	g.w.indent++
	g.w.line("x ^= x >> 30; x *= 0xBF58476D1CE4E5B9ULL;")
	g.w.line("x ^= x >> 27; x *= 0x94D049BB133111EBULL;")
	g.w.line("x ^= x >> 31;")
	g.w.line("return x;")
	g.w.indent--
	g.w.line("}")
	if hasStrKey || hasStringKey {
		g.w.line("static uint64_t zx_hash_fnv(const unsigned char *p, size_t n) {")
		g.w.indent++
		g.w.line("uint64_t h = 1469598103934665603ULL;")
		g.w.line("for (size_t i = 0; i < n; i++) { h ^= p[i]; h *= 1099511628211ULL; }")
		g.w.line("return h;")
		g.w.indent--
		g.w.line("}")
	}
	for _, name := range g.mapOrder {
		g.emitMapRuntime(g.maps[name])
	}
}

func (g *gen) emitMapRuntime(t *types.Type) {
	kt, vt := ctype(t.Key), ctype(t.Val)
	pn, mn := hashPairName(t), mapTypeName(t)
	hk, eq := hashKeyName(t), mapEqName(t)
	keyCopy, valCopy := t.Key.IsCopy(), t.Val.IsCopy()

	g.w.line("")
	g.w.line("// HashMap<%s, %s> helpers", t.Key, t.Val)
	if !g.hashSeen[hashKeyName(t)] {
		// One hasher per key type, however many maps share it.
		g.hashSeen[hashKeyName(t)] = true
		g.w.line("static size_t %s(%s key) {", hk, kt)
		g.w.indent++
		switch t.Key.Kind {
		case types.Str:
			g.w.line(`return (size_t)zx_hash_fnv((const unsigned char *)key, strlen(key));`)
		case types.StringKind:
			g.w.line("const char *s = zx_string_as_str(&key);")
			g.w.line(`return (size_t)zx_hash_fnv((const unsigned char *)s, strlen(s));`)
		default:
			g.w.line("return (size_t)zx_hash_mix((uint64_t)key);")
		}
		g.w.indent--
		g.w.line("}")
		g.w.line("static int %s(%s a, %s b) {", eq, kt, kt)
		g.w.indent++
		switch t.Key.Kind {
		case types.Str:
			g.w.line(`return strcmp(a, b) == 0;`)
		case types.StringKind:
			g.w.line(`return strcmp(zx_string_as_str(&a), zx_string_as_str(&b)) == 0;`)
		default:
			g.w.line("return a == b;")
		}
		g.w.indent--
		g.w.line("}")
	}
	g.w.line("static void zx_hashmap_reserve_%s(%s *m, size_t want) {", mn, mn)
	g.w.line("    if (want <= m->cap) return;")
	g.w.line("    if (want > SIZE_MAX / sizeof(%s)) zx_panicf(\"capacity overflow\");", pn)
	g.w.line("    size_t cap = m->cap ? m->cap : 4;")
	g.w.line("    while (cap < want) cap = (cap > SIZE_MAX / 2) ? want : cap * 2;")
	g.w.line("    %s *np = (%s *)malloc(cap * sizeof(%s));", pn, pn, pn)
	g.w.line(`    if (!np) zx_panicf("out of memory");`)
	g.w.line("    for (size_t i = 0; i < cap; i++) memset(&np[i], 0, sizeof(np[i]));")
	g.w.line("    if (m->entries) {")
	g.w.indent++
	g.w.line("for (size_t i = 0; i < m->cap; i++) {")
	g.w.indent++
	g.w.line("if (m->entries[i].used) {")
	g.w.indent++
	g.w.line("size_t idx = %s(m->entries[i].key) %% cap;", hk)
	g.w.line("while (np[idx].used) idx = (idx + 1) %% cap;")
	g.w.line("np[idx] = m->entries[i];")
	g.w.indent--
	g.w.line("}")
	g.w.indent--
	g.w.line("}")
	g.w.line("free(m->entries);")
	g.w.indent--
	g.w.line("    }")
	g.w.line("    m->entries = np;")
	g.w.line("    m->cap = cap;")
	g.w.line("}")

	g.w.line("static int zx_hashmap_insert_%s(%s *m, %s key, %s value, %s *out) {", mn, mn, kt, vt, vt)
	g.w.line("    if (m->cap == 0) {")
	g.w.line("        zx_hashmap_reserve_%s(m, 4);", mn)
	g.w.line("    } else if (m->len * 10 >= m->cap * 7) {")
	g.w.line("        zx_hashmap_reserve_%s(m, m->cap * 2);", mn)
	g.w.line("    }")
	g.w.line("    size_t idx = %s(key) %% m->cap;", hk)
	g.w.line("    while (m->entries[idx].used && !%s(m->entries[idx].key, key)) idx = (idx + 1) %% m->cap;", eq)
	g.w.line("    if (m->entries[idx].used) {")
	g.w.indent++
	g.w.line("%s zx_old = m->entries[idx].value;", vt)
	g.w.line("m->entries[idx].value = value;")
	if !valCopy {
		g.w.line("memset(&value, 0, sizeof(value));")
	}
	g.w.line("*out = zx_old;")
	if !valCopy {
		g.w.line("memset(&zx_old, 0, sizeof(zx_old));")
	}
	g.w.line("return 1;")
	g.w.indent--
	g.w.line("    }")
	g.w.line("    m->entries[idx].used = 1;")
	g.w.line("    m->entries[idx].key = key;")
	if !keyCopy {
		g.w.line("    memset(&key, 0, sizeof(key));")
	}
	g.w.line("    m->entries[idx].value = value;")
	if !valCopy {
		g.w.line("    memset(&value, 0, sizeof(value));")
	}
	g.w.line("    m->len++;")
	g.w.line("    return 0;")
	g.w.line("}")

	g.w.line("static int zx_hashmap_get_%s(%s *m, %s key, %s **out) {", mn, mn, kt, vt)
	g.w.line("    if (m->cap == 0 || m->entries == NULL) return 0;")
	g.w.line("    size_t idx = %s(key) %% m->cap;", hk)
	g.w.line("    while (m->entries[idx].used && !%s(m->entries[idx].key, key)) idx = (idx + 1) %% m->cap;", eq)
	g.w.line("    if (!m->entries[idx].used) return 0;")
	g.w.line("    *out = &m->entries[idx].value;")
	g.w.line("    return 1;")
	g.w.line("}")

	g.w.line("static int zx_hashmap_remove_%s(%s *m, %s key, %s *out) {", mn, mn, kt, vt)
	g.w.line("    if (m->cap == 0 || m->entries == NULL) return 0;")
	g.w.line("    size_t idx = %s(key) %% m->cap;", hk)
	g.w.line("    while (m->entries[idx].used && !%s(m->entries[idx].key, key)) idx = (idx + 1) %% m->cap;", eq)
	g.w.line("    if (!m->entries[idx].used) return 0;")
	g.w.line("    *out = m->entries[idx].value;")
	g.w.line("    memset(&m->entries[idx].value, 0, sizeof(%s));", vt)
	g.w.line("    memset(&m->entries[idx].key, 0, sizeof(%s));", kt)
	g.w.line("    m->entries[idx].used = 0;")
	g.w.line("    m->len--;")
	g.w.line("    return 1;")
	g.w.line("}")

	g.w.line("static int zx_hashmap_contains_%s(%s *m, %s key) {", mn, mn, kt)
	g.w.line("    if (m->cap == 0 || m->entries == NULL) return 0;")
	g.w.line("    size_t idx = %s(key) %% m->cap;", hk)
	g.w.line("    while (m->entries[idx].used && !%s(m->entries[idx].key, key)) idx = (idx + 1) %% m->cap;", eq)
	g.w.line("    return m->entries[idx].used;")
	g.w.line("}")

	g.w.line("static void zx_hashmap_clear_%s(%s *m) {", mn, mn)
	g.w.line("    if (m->entries) {")
	g.w.indent++
	g.w.line("        for (size_t i = 0; i < m->cap; i++) {")
	g.w.indent++
	g.w.line("            if (m->entries[i].used) {")
	g.w.indent++
	if t.Key.NeedsDrop() {
		g.w.line("zx_drop_%s(&m->entries[i].key);", typeMangle(t.Key))
	}
	if t.Val.NeedsDrop() {
		g.w.line("zx_drop_%s(&m->entries[i].value);", typeMangle(t.Val))
	}
	g.w.line("memset(&m->entries[i].key, 0, sizeof(%s));", kt)
	g.w.line("memset(&m->entries[i].value, 0, sizeof(%s));", vt)
	g.w.indent--
	g.w.line("            }")
	g.w.indent--
	g.w.line("        }")
	g.w.line("        free(m->entries);")
	g.w.line("        m->entries = NULL;")
	g.w.indent--
	g.w.line("    }")
	g.w.line("    m->len = 0;")
	g.w.line("    m->cap = 0;")
	g.w.line("}")
}

// mapCtorValue lowers `HashMap::new()`.
func (g *gen) mapCtorValue(x *ast.CallExpr, name string) string {
	t := g.info.TypeOf(x)
	if t == nil || t.Kind != types.MapKind {
		return "0 /* unresolved HashMap */"
	}
	if name == "new" || len(x.Args) == 0 {
		return "((" + ctype(t) + "){ 0 })"
	}
	return "0 /* unsupported HashMap constructor */"
}

// optionExpr builds an `Option<elem>` from a hit condition and a value
// expression: `Some(value)` when cond holds, `None` otherwise. It mirrors
// popOption for Vec but is generic over the payload type, so it also serves
// the reference payload of `HashMap::get`.
func optionExpr(opt, elem *types.Type, cond, val string) string {
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
	fmt.Fprintf(&b, "%s %s; if (%s) { %s %s = %s; %s = ((%s){ .tag = %d, .u.v%d = { .%s = %s } }); } else { %s = ((%s){ .tag = %d }); } %s;",
		rt, res, cond, ot, el, val, res, rt, someTag, someTag, someField, el, res, rt, noneTag, res)
	return b.String()
}

// mapMethodCall lowers a builtin method call on a `HashMap<K, V>`.
func (g *gen) mapMethodCall(x *ast.MethodCallExpr) string {
	t := recvBase(g.info.TypeOf(x.Recv))
	if t == nil || t.Kind != types.MapKind {
		return "0 /* unresolved HashMap method */"
	}
	mn := mapTypeName(t)
	recv := g.vecReceiverValue(x.Recv)
	borrowed := vecBorrowedReceiver(g.info.TypeOf(x.Recv))
	var prologue, epilogue string
	if !isPlaceExpr(x.Recv) && !borrowed {
		tm := g.tmpName()
		prologue = ctype(t) + " " + tm + " = " + recv + "; "
		recv = tm
		epilogue = " zx_drop_" + typeMangle(t) + "(&" + tm + ");"
	}
	switch x.Name {
	case "len":
		return "({ " + prologue + "(" + recv + ").len;" + epilogue + " })"
	case "is_empty":
		return "({ " + prologue + "((" + recv + ").len == 0);" + epilogue + " })"
	case "clear":
		return "({ " + prologue + "zx_hashmap_clear_" + mn + "(&" + recv + ");" + epilogue + " })"
	case "insert":
		rt := g.info.TypeOf(x)
		res := g.tmpName()
		expr := "({ " + prologue + ctype(rt) + " " + res + "; " + ctype(t.Val) + " zx__old;" +
			" int zx__hit = zx_hashmap_insert_" + mn + "(&" + recv + ", " +
			g.consume(x.Args[0]) + ", " + g.consume(x.Args[1]) + ", &zx__old);" + epilogue +
			" " + optionExpr(rt, t.Val, "zx__hit", "zx__old") + "; })"
		return expr
	case "get":
		rt := g.info.TypeOf(x)
		res := g.tmpName()
		expr := "({ " + prologue + ctype(rt) + " " + res + "; " + ctype(t.Val) + " *zx__p;" +
			" int zx__hit = zx_hashmap_get_" + mn + "(&" + recv + ", " +
			g.consume(x.Args[0]) + ", &zx__p);" + epilogue +
			" " + optionExpr(rt, types.RefTo(t.Val, false), "zx__hit", "zx__p") + "; })"
		return expr
	case "remove":
		rt := g.info.TypeOf(x)
		res := g.tmpName()
		expr := "({ " + prologue + ctype(rt) + " " + res + "; " + ctype(t.Val) + " zx__old;" +
			" int zx__hit = zx_hashmap_remove_" + mn + "(&" + recv + ", " +
			g.consume(x.Args[0]) + ", &zx__old);" + epilogue +
			" " + optionExpr(rt, t.Val, "zx__hit", "zx__old") + "; })"
		return expr
	case "contains_key":
		return "({ " + prologue + "(bool)zx_hashmap_contains_" + mn + "(&" + recv + ", " +
			g.consume(x.Args[0]) + ");" + epilogue + " })"
	}
	return "0 /* unhandled HashMap method */"
}
