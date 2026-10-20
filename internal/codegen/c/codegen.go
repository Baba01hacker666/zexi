// Package c lowers a checked Zexi program to C11 and hands it to the system C
// compiler. This is the M0 backend described in docs/04 §4.1: C is a portable,
// AOT compile target with a mature optimizer, not an interpreter. It buys
// Rust-class code quality and C-class binary size before we invest in a native
// emitter (M2).
//
// The emitted code is deliberately conservative: every function is `static`
// except `main` and `extern` declarations so the linker's section GC can drop
// what is unused.
//
// Value-producing expressions that contain statements (blocks, if/else) are
// emitted with GNU statement expressions, `({ ... })`. We therefore require a
// GCC/Clang-compatible `cc`, which is the same toolchain the design targets.
package c

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"zexi/internal/ast"
	"zexi/internal/sema"
	"zexi/internal/token"
	"zexi/internal/types"
)

// Options configures C emission.
type Options struct {
	// NoStd drops the standard headers for freestanding targets. Unused in M0.
	NoStd bool
	// DebugChecks turns on the checks that release builds elide: signed and
	// unsigned integer overflow on `+ - * -` panics instead of wrapping.
	// Division and modulo by zero always panic, in every profile, because
	// they are not a performance choice — they are undefined behaviour.
	DebugChecks bool
}

type gen struct {
	info       *sema.Info
	w          *writer
	opts       Options
	tmp        int
	counts     map[string]int
	scopes     []map[string]string
	arrays     map[string]*types.Type // array type name -> array type
	arrSeen    map[string]bool
	arrayOrder []string
	vecs       map[string]*types.Type // Vec handle struct name -> Vec type
	vecSeen    map[string]bool
	vecOrder   []string
	dropFrames []*dropFrame
	dropTypes  []*dropType
	dropSeen   map[string]bool
	loopFrames []int // drop-frame depth at the start of each active loop body
	loopLabels []loopLabel
	trapDepth  int  // compiler-generated constructs (matches) since the innermost loop
	hasString  bool // whether the program uses the builtin `String`
	hasChar    bool // whether a `char` value is printed (needs the UTF-8 helper)
	// hasPanic marks that the program contains at least one run-time check
	// that can fail, so `zx_panicf` must be defined in the unit.
	hasPanic bool
	// externNames is the set of C-visible function names (`extern "C"`
	// declarations and `main`): locals mangle to `zx_<name>`, which must
	// never shadow one of these (a local `motto` would hide the C
	// function `zx_motto` it may need to call).
	externNames map[string]bool
	// used is the set of functions reachable from `main`; unreachable
	// functions are omitted from the output entirely.
	used map[*sema.Fn]bool
	// split marks multi-unit emission: user functions, drop helpers, and
	// constants get external linkage so cross-unit references link, and
	// their definitions are collected in core (emitted into unit 0).
	split bool
	core  *writer
	// pendingMoves tracks owned pattern bindings that were copied out for a
	// guard and must be taken (moved) once the guard succeeds.
	pendingMoves []pendingMove
}

// pendingMove is an owned pattern binding whose move out of the scrutinee is
// deferred until a guard has passed.
type pendingMove struct {
	name string
	ty   *types.Type
	expr string
}

// loopLabel are the jump targets of one source-level loop.
type loopLabel struct {
	brk, cont string
	outerTrap int    // trapDepth to restore when the loop body ends
	result    string // C temp receiving a `break <expr>` value, or "" (see loopResult)
}

// dropFrame is the set of locals declared in one lexical scope, in declaration
// order; destructors run in reverse. It also holds the scope's deferred
// expressions, which run (last-in-first-out) before the destructors.
type dropFrame struct {
	vars   []dropVar
	defers []ast.Expr
}

type dropVar struct {
	cname string
	ty    *types.Type
}

// dropType is a type that needs generated destructor/move helpers.
type dropType struct {
	name string // mangled, used as `zx_drop_<name>`
	t    *types.Type
}

type writer struct {
	b      strings.Builder
	indent int
}

func (w *writer) line(format string, a ...any) {
	for range w.indent {
		w.b.WriteString("    ")
	}
	fmt.Fprintf(&w.b, format, a...)
	w.b.WriteByte('\n')
}

func (w *writer) write(format string, a ...any) { fmt.Fprintf(&w.b, format, a...) }

// raw appends already-formatted text (including its own newlines/indentation).
func (w *writer) raw(s string) { w.b.WriteString(s) }

// Generate emits a complete C translation unit for the program.
func Generate(info *sema.Info, opts Options) (string, error) {
	g := newGen(info, opts)
	g.emitPrefix()
	g.w.raw("// Command-line argument storage (defined once per program).\nint zx_argc;\nchar **zx_argv;\n\n")
	for _, fn := range g.reachableFns() {
		g.funcDef(fn)
	}
	g.lateRuntime()
	return g.w.b.String(), nil
}

// GenerateSplit emits the program as one shared header plus n translation
// units (reachable function bodies dealt round-robin), so the driver can
// compile them in parallel. Every generated symbol is `static` except the
// argument globals, so each unit may include the full header without link
// conflicts and the linker's section GC drops what a unit does not use.
func GenerateSplit(info *sema.Info, opts Options, n int) (string, []string, error) {
	if n <= 1 {
		s, err := Generate(info, opts)
		return s, nil, err
	}
	g := newGen(info, opts)
	g.split = true
	g.core = &writer{}
	g.emitPrefix()
	header := g.w.b.String()

	fns := g.reachableFns()
	if len(fns) == 0 {
		return header, nil, nil
	}
	if len(fns) < n {
		n = len(fns)
	}
	writers := make([]*writer, n)
	for i := range writers {
		writers[i] = &writer{}
	}
	for i, fn := range fns {
		g.w = writers[i%n]
		g.funcDef(fn)
	}
	// The char printer is discovered while emitting bodies; each unit gets
	// its own static copy (unused ones are dropped by the linker).
	units := make([]string, n)
	for i, w := range writers {
		g.w = w
		g.lateRuntime()
		body := w.b.String()
		if i == 0 {
			body = "// Command-line argument storage (defined once per program).\nint zx_argc;\nchar **zx_argv;\n\n" + g.core.b.String() + body
		}
		units[i] = body
	}
	return header, units, nil
}

func newGen(info *sema.Info, opts Options) *gen {
	g := &gen{info: info, w: &writer{}, opts: opts, counts: map[string]int{}, arrays: map[string]*types.Type{}, arrSeen: map[string]bool{}, vecs: map[string]*types.Type{}, vecSeen: map[string]bool{}, dropSeen: map[string]bool{}, externNames: map[string]bool{}}
	g.used = usedFns(info)
	for _, fn := range info.FnOrder {
		if fn.Extern || fn.Name == "main" || fn.NoMangle {
			g.externNames[funcName(fn)] = true
		}
	}
	return g
}

// emitPrefix emits everything that is not a user function body: preamble,
// array/drop helpers, runtime, type definitions, constants, prototypes.
func (g *gen) emitPrefix() {
	g.preamble()
	g.collectArrayTypes()
	g.collectVecTypes()
	g.collectDropTypes()
	g.runtime()
	g.typeDefs()
	g.dropFuncs()
	// The Vec helpers call the destructors, so they come after dropFuncs has
	// declared them.
	g.vecRuntimeProtos()
	g.vecRuntime()
	g.constants()
	g.prototypes()
}

// reachableFns returns the emitted function bodies, in declaration order.
func (g *gen) reachableFns() []*sema.Fn {
	var out []*sema.Fn
	for _, fn := range g.info.FnOrder {
		if fn.Decl.Body != nil && g.used[fn] {
			out = append(out, fn)
		}
	}
	return out
}

// usedFns computes the set of functions reachable from `main` and from every
// `#[no_mangle]`/`#[export]` function, following direct calls, method calls,
// and names used as values (callbacks). Only reachable functions are emitted,
// so importing a large library costs nothing unless its functions are actually
// called — the C side stays small and fast to compile. When there is no entry
// point (defensive), everything is kept.
func usedFns(info *sema.Info) map[*sema.Fn]bool {
	if info.Main == nil || info.Main.Decl == nil || info.Main.Decl.Body == nil {
		all := map[*sema.Fn]bool{}
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
	// `#[no_mangle]`/`#[export]` functions are entry points for C callers, so
	// they are roots even when nothing in Zexi calls them.
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
				if r := info.Resolved[x]; r != nil {
					add(r)
				}
			case *ast.MethodCallExpr:
				if r := info.Resolved[x]; r != nil {
					add(r)
				}
			case *ast.IdentExpr:
				// A name in value position (callback argument, binding).
				if r := info.FnValues[x]; r != nil {
					add(r)
				} else if r := info.Fns[x.Name]; r != nil {
					add(r)
				}
			case *ast.PathExpr:
				if len(x.Segments) == 1 {
					if r := info.Fns[x.Segments[0]]; r != nil {
						add(r)
					}
				}
			}
		})
	}
	return used
}

// walkExprs visits every expression in a syntax subtree (via reflection, so
// new AST node kinds are covered automatically).
func walkExprs(root any, fn func(ast.Expr)) {
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
			for i := range v.NumField() {
				walk(v.Field(i))
			}
		case reflect.Slice, reflect.Array:
			for i := range v.Len() {
				walk(v.Index(i))
			}
		}
	}
	walk(reflect.ValueOf(root))
}

// charRuntime emits the UTF-8 `char` printer, but only for programs that print
// one, so that trivial programs keep producing minimal C.
func (g *gen) charRuntime() {
	if !g.hasChar {
		return
	}
	g.w.line("")
	if !g.hasString {
		// The `String` runtime defines the UTF-8 encoder; when a program only
		// prints a `char` (and never uses `String`), emit it here instead.
		g.w.line("// A `char` is a Unicode scalar value, so printing it writes its UTF-8")
		g.w.line("// encoding (not just the low byte).")
		g.w.line("static int zx_utf8_encode(unsigned int c, char *out) {")
		g.w.line("    if (c < 0x80) { out[0] = (char)c; return 1; }")
		g.w.line("    if (c < 0x800) { out[0] = (char)(0xC0 | (c >> 6)); out[1] = (char)(0x80 | (c & 0x3F)); return 2; }")
		g.w.line("    if (c < 0x10000) { out[0] = (char)(0xE0 | (c >> 12)); out[1] = (char)(0x80 | ((c >> 6) & 0x3F)); out[2] = (char)(0x80 | (c & 0x3F)); return 3; }")
		g.w.line("    out[0] = (char)(0xF0 | (c >> 18)); out[1] = (char)(0x80 | ((c >> 12) & 0x3F)); out[2] = (char)(0x80 | ((c >> 6) & 0x3F)); out[3] = (char)(0x80 | (c & 0x3F)); return 4;")
		g.w.line("}")
		g.w.line("")
	}
	g.w.line("static int zx_print_char(uint32_t c) {")
	g.w.line("    char buf[4];")
	g.w.line("    int n = zx_utf8_encode(c, buf);")
	g.w.line("    return (int)fwrite(buf, 1, (size_t)n, stdout);")
	g.w.line("}")
}

func (g *gen) preamble() {
	g.w.line("// Generated by zexic from Zexi source. Do not edit.")
	g.w.line("#include <stdint.h>")
	g.w.line("#include <stdbool.h>")
	g.w.line("#include <stddef.h>")
	g.w.line("#include <stdio.h>")
	g.w.line("#include <stdlib.h>")
	g.w.line("#include <string.h>")
	g.w.line("#include <stdarg.h>")
	g.w.line("")
	g.w.line("// Overflow detection uses the compiler builtins when they exist; without")
	g.w.line("// them the checked helpers fall back to plain (wrapping) arithmetic.")
	g.w.line("#if defined(__GNUC__) || defined(__clang__)")
	g.w.line("#define ZX_OVERFLOW_BUILTINS 1")
	g.w.line("#define ZX_NORETURN __attribute__((noreturn))")
	g.w.line("#else")
	g.w.line("#define ZX_NORETURN")
	g.w.line("#endif")
	g.w.line("")
	g.w.line("// Defined only when a `char` is printed.")
	g.w.line("static int zx_print_char(uint32_t c);")
	g.w.line("")
	g.w.line("// Defined only when the program can panic at run time.")
	g.w.line("static ZX_NORETURN void zx_panicf(const char *fmt, ...);")
	g.w.line("")
	g.w.line("// Command-line arguments, captured by `main` for arg_count()/arg_at().")
	g.w.line("extern int zx_argc;")
	g.w.line("extern char **zx_argv;")
	g.w.line("static int zx_arg_count(void) { return zx_argc; }")
	g.w.line("")
}

// runtime emits helpers that the generated code may call. Each is emitted only
// when the program actually uses the corresponding feature, so trivial programs
// produce minimal C.
func (g *gen) runtime() {
	if len(g.arrayOrder) > 0 || len(g.vecOrder) > 0 {
		g.hasPanic = true
		g.boundsHelper()
	}
	if g.hasString {
		// The allocator can fail, which is a panic path.
		g.hasPanic = true
		g.w.raw(zxStringRuntime)
	}
	// Which arithmetic needs checking is only known once bodies are lowered,
	// and C requires a declaration before the first use, so a debug build
	// emits the whole (tiny) family here. Unused helpers are dropped by the
	// linker's section GC.
	if g.opts.DebugChecks {
		for _, ct := range intCTypes {
			for _, op := range []string{"add", "sub", "mul"} {
				g.arithHelper(op, ct)
			}
			if strings.HasPrefix(ct, "int") {
				g.arithHelper("neg", ct)
			}
		}
	}
}

// lateRuntime emits the helpers whose need is only discovered while lowering
// bodies. Like the `char` printer, each unit that needs one gets its own
// static copy; the linker drops the copies it does not use.
func (g *gen) lateRuntime() {
	g.charRuntime()
	if g.hasPanic {
		g.panicHelper()
	}
}

// panicHelper defines `zx_panicf`, the single exit path for every run-time
// fault. stdout is flushed first: C buffers it when it is not a terminal, so
// without this a panic would silently swallow everything the program had
// already printed.
func (g *gen) panicHelper() {
	g.w.line("")
	g.w.line("// Report a run-time fault and stop. stdout is flushed so output")
	g.w.line("// produced before the panic is not lost in the C buffer.")
	g.w.line("static ZX_NORETURN void zx_panicf(const char *fmt, ...) {")
	g.w.line("    va_list ap;")
	g.w.line("    fflush(stdout);")
	g.w.line(`    fputs("zexi panic: ", stderr);`)
	g.w.line("    va_start(ap, fmt);")
	g.w.line("    vfprintf(stderr, fmt, ap);")
	g.w.line("    va_end(ap);")
	g.w.line(`    fputc('\n', stderr);`)
	g.w.line("    fflush(stderr);")
	g.w.line("    exit(101);")
	g.w.line("}")
	g.w.line("")
}

// arithOps names the overflow-checked arithmetic helpers.
var arithOps = map[string]string{"add": "add", "sub": "subtract", "mul": "multiply"}

// checkedArithName is the name of the overflow-checked helper for `op` on the
// C type `ct`.
func checkedArithName(op, ct string) string {
	return "zx_" + op + "_" + strings.ReplaceAll(ct, " ", "")
}

// intCTypes lists the C spelling of every Zexi integer type. On some platforms
// two of these are the same underlying type, which is fine: the helpers differ
// by name, and unused ones cost nothing.
var intCTypes = []string{
	"int8_t", "int16_t", "int32_t", "int64_t", "intptr_t",
	"uint8_t", "uint16_t", "uint32_t", "uint64_t", "size_t",
}

// wantCheckedArith returns the name of the checked helper for `op` on `ct`.
// Debug builds emit the whole family in the preamble, so a helper is never
// referenced before its declaration.
func (g *gen) wantCheckedArith(op, ct string) string {
	g.hasPanic = true
	return checkedArithName(op, ct)
}

// arithHelper emits one overflow-checked arithmetic helper. The check uses the
// compiler builtins when they exist and degrades to wrapping arithmetic when
// they do not, so the generated C stays portable.
func (g *gen) arithHelper(op, ct string) {
	name := checkedArithName(op, ct)
	signed := strings.HasPrefix(ct, "int")
	if op == "neg" {
		g.w.line("static inline %s %s(%s a) {", ct, name, ct)
		g.w.line("#ifdef ZX_OVERFLOW_BUILTINS")
		if signed {
			g.w.line(`    if (a == (%s)((%s)1 << (sizeof(%s) * 8 - 1))) zx_panicf("attempt to negate with overflow");`, ct, ct, ct)
		}
		g.w.line("#endif")
		g.w.line("    return (%s)(-a);", ct)
		g.w.line("}")
		return
	}
	builtin := "__builtin_" + op + "_overflow"
	g.w.line("static inline %s %s(%s a, %s b) {", ct, name, ct, ct)
	g.w.line("#ifdef ZX_OVERFLOW_BUILTINS")
	g.w.line("    %s r;", ct)
	g.w.line(`    if (%s(a, b, &r)) zx_panicf("attempt to %s with overflow");`, builtin, arithOps[op])
	g.w.line("    return r;")
	g.w.line("#else")
	g.w.line("    return (%s)(a %s b);", ct, arithOps[op])
	g.w.line("#endif")
	g.w.line("}")
}

func (g *gen) boundsHelper() {
	g.w.line("// Bounds-checked array index: panics with a message instead of reading")
	g.w.line("// out of bounds. (M0-alpha: no backtrace yet.)")
	g.w.line("static inline int64_t zx_bounds(int64_t i, int64_t n) {")
	g.w.line("    if (i < 0) { i += n; }")
	g.w.line("    if (i < 0 || i >= n) {")
	g.w.line(`        zx_panicf("index %%lld out of bounds for length %%lld", (long long)i, (long long)n);`)
	g.w.line("    }")
	g.w.line("    return i;")
	g.w.line("}")
	g.w.line("")
}

func (g *gen) constants() { // Emit in source order for stable output.
	for _, it := range g.info.File.Items {
		d, ok := it.(*ast.ConstDecl)
		if !ok {
			continue
		}
		cn, ok := g.info.Consts[d.Name]
		if !ok {
			continue
		}
		if g.split {
			// Declaration in the header, definition in the core unit.
			g.w.line("extern const %s %s;", ctype(cn.Ty), cname(cn.Name))
			g.core.line("const %s %s = %s;", ctype(cn.Ty), cname(cn.Name), constLiteral(cn.Val, cn.Ty))
			continue
		}
		g.w.line("static const %s %s = %s;", ctype(cn.Ty), cname(cn.Name), constLiteral(cn.Val, cn.Ty))
	}
	g.w.line("")
}

// typeDefs emits C declarations for every Zexi struct and enum, in dependency
// order so that by-value fields refer to complete types. All names are
// forward-declared first, which also permits self-referential pointers.
func (g *gen) typeDefs() {
	if len(g.info.StructOrd) == 0 && len(g.info.EnumOrd) == 0 && len(g.arrayOrder) == 0 && len(g.vecOrder) == 0 {
		return
	}
	for _, s := range g.info.StructOrd {
		g.w.line("typedef struct %s %s;", typeName(s.Name), typeName(s.Name))
	}
	for _, e := range g.info.EnumOrd {
		g.w.line("typedef struct %s %s;", typeName(e.Name), typeName(e.Name))
	}
	for _, name := range g.arrayOrder {
		g.w.line("typedef struct %s %s;", name, name)
	}
	// A Vec handle holds its element behind a pointer, so its definition is
	// order-independent and can precede the structs that embed it by value.
	g.vecForwardDecls()
	g.vecDefs()
	g.w.line("")
	for _, name := range g.typeOrder() {
		switch {
		case g.info.Structs[name] != nil:
			g.emitStructDef(g.info.Structs[name])
		case g.info.Enums[name] != nil:
			g.emitEnumDef(g.info.Enums[name])
		case g.arrays[name] != nil:
			g.emitArrayDef(name, g.arrays[name])
		}
	}
	g.w.line("")
}

// emitArrayDef emits a struct wrapping a C array so that Zexi arrays are
// first-class values (assignable, returnable) rather than decaying pointers.
func (g *gen) emitArrayDef(name string, t *types.Type) {
	g.w.line("struct %s {", name)
	g.w.indent++
	if t.Len <= 0 {
		g.w.line("char e;")
	} else {
		g.w.line("%s e[%d];", ctype(t.Elem), t.Len)
	}
	g.w.indent--
	g.w.line("};")
}

// collectArrayTypes walks every type reachable from the program and records the
// array types that need a C wrapper struct.
func (g *gen) collectArrayTypes() {
	var walk func(t *types.Type)
	walk = func(t *types.Type) {
		if t == nil {
			return
		}
		switch t.Kind {
		case types.Array:
			name := arrayTypeName(t)
			if g.arrSeen[name] {
				return
			}
			g.arrSeen[name] = true
			walk(t.Elem)
			g.arrays[name] = t
			g.arrayOrder = append(g.arrayOrder, name)
		case types.Ptr, types.Ref, types.Slice:
			walk(t.Elem)
		case types.StructKind:
			if t.St == nil || g.arrSeen["#"+t.St.Name] {
				return
			}
			g.arrSeen["#"+t.St.Name] = true
			for _, f := range t.St.Fields {
				walk(f.Ty)
			}
		case types.EnumKind:
			if t.En == nil || g.arrSeen["#"+t.En.Name] {
				return
			}
			g.arrSeen["#"+t.En.Name] = true
			for _, v := range t.En.Variants {
				for _, f := range v.Fields {
					walk(f.Ty)
				}
			}
		case types.Tuple:
			for _, e := range t.Elems {
				walk(e)
			}
		case types.StringKind:
			g.hasString = true
		}
	}
	for _, f := range g.info.FnOrder {
		for _, p := range f.Params {
			walk(p)
		}
		walk(f.Ret)
	}
	for _, t := range g.info.Types {
		walk(t)
	}
	for _, t := range g.info.DeclTypes {
		walk(t)
	}
	// Also walk declared structs/enums directly: an otherwise-unused type still
	// has a C definition emitted, so a `String` field must pull in its typedef
	// (and any array wrappers it needs).
	for _, s := range g.info.Structs {
		for _, f := range s.Fields {
			walk(f.Ty)
		}
	}
	for _, e := range g.info.Enums {
		for _, v := range e.Variants {
			for _, f := range v.Fields {
				walk(f.Ty)
			}
		}
	}
}

// collectDropTypes records every type that owns a heap resource and therefore
// needs a generated destructor and move helper. `String` is built into the
// runtime; structs, enums, and arrays containing one are emitted here.
func (g *gen) collectDropTypes() {
	var walk func(t *types.Type)
	walk = func(t *types.Type) {
		if t == nil || !t.NeedsDrop() {
			return
		}
		if t.Kind == types.StringKind {
			return // provided by the runtime
		}
		if t.Kind == types.VecKind {
			// The handle's own destructor is generated below; its element
			// type still needs one when it owns resources.
			walk(t.Elem)
			name := typeMangle(t)
			if g.dropSeen[name] {
				return
			}
			g.dropSeen[name] = true
			g.dropTypes = append(g.dropTypes, &dropType{name: name, t: t})
			return
		}
		name := typeMangle(t)
		if g.dropSeen[name] {
			return
		}
		g.dropSeen[name] = true
		g.dropTypes = append(g.dropTypes, &dropType{name: name, t: t})
		switch t.Kind {
		case types.StructKind:
			for _, f := range t.St.Fields {
				walk(f.Ty)
			}
		case types.EnumKind:
			for _, v := range t.En.Variants {
				for _, f := range v.Fields {
					walk(f.Ty)
				}
			}
		case types.Array:
			walk(t.Elem)
		}
	}
	for _, f := range g.info.FnOrder {
		for _, p := range f.Params {
			walk(p)
		}
		walk(f.Ret)
	}
	for _, st := range g.info.StructOrd {
		walk(&types.Type{Kind: types.StructKind, St: st})
	}
	for _, en := range g.info.EnumOrd {
		walk(&types.Type{Kind: types.EnumKind, En: en})
	}
	for _, t := range g.info.Types {
		walk(t)
	}
	for _, t := range g.info.DeclTypes {
		walk(t)
	}
}

// dropFuncs emits forward declarations and definitions for the destructor and
// move helper of every resource-owning type.
func (g *gen) dropFuncs() {
	if len(g.dropTypes) == 0 {
		return
	}
	link := "static "
	if g.split {
		// Definitions land in the core unit; every other unit sees an
		// external prototype, so cross-unit drops link correctly.
		link = ""
	}
	for _, d := range g.dropTypes {
		ct := ctype(d.t)
		g.w.line("%svoid zx_drop_%s(%s *v);", link, d.name, ct)
		g.w.line("%s%s zx_move_%s(%s *v);", link, ct, d.name, ct)
	}
	g.w.line("")
	if g.split {
		g.w, g.core = g.core, g.w
	}
	for _, d := range g.dropTypes {
		g.emitDropFunc(d)
	}
	g.w.line("")
	if g.split {
		g.w, g.core = g.core, g.w
	}
}

func (g *gen) emitDropFunc(d *dropType) {
	t := d.t
	ct := ctype(t)
	g.w.line("%svoid zx_drop_%s(%s *v) {", g.linkPrefix(), d.name, ct)
	g.w.indent++
	switch t.Kind {
	case types.StructKind:
		for _, f := range t.St.Fields {
			if f.Ty.NeedsDrop() {
				g.w.line("zx_drop_%s(&v->%s);", typeMangle(f.Ty), fieldName(f.Name))
			}
		}
	case types.EnumKind:
		g.w.line("switch (v->tag) {")
		g.w.indent++
		for _, v := range t.En.Variants {
			any := false
			for _, f := range v.Fields {
				if f.Ty.NeedsDrop() {
					any = true
				}
			}
			if !any {
				continue
			}
			g.w.line("case %d: {", v.Index)
			g.w.indent++
			for _, f := range v.Fields {
				if f.Ty.NeedsDrop() {
					g.w.line("zx_drop_%s(&v->u.v%d.%s);", typeMangle(f.Ty), v.Index, fieldName(f.Name))
				}
			}
			g.w.line("break;")
			g.w.indent--
			g.w.line("}")
		}
		g.w.indent--
		g.w.line("}")
		g.w.line("v->tag = -1;")
	case types.VecKind:
		g.w.line("if (v->ptr) {")
		g.w.indent++
		if t.Elem.NeedsDrop() {
			g.w.line("for (size_t i = 0; i < v->len; i++) {")
			g.w.indent++
			g.w.line("zx_drop_%s(&v->ptr[i]);", typeMangle(t.Elem))
			g.w.indent--
			g.w.line("}")
		}
		g.w.line("free(v->ptr);")
		g.w.line("v->ptr = NULL;")
		g.w.indent--
		g.w.line("}")
		g.w.line("v->len = 0;")
		g.w.line("v->cap = 0;")
	case types.Array:
		if t.Len > 0 {
			g.w.line("for (int64_t i = 0; i < %d; i++) {", t.Len)
			g.w.indent++
			g.w.line("zx_drop_%s(&v->e[i]);", typeMangle(t.Elem))
			g.w.indent--
			g.w.line("}")
		}
	}
	g.w.indent--
	g.w.line("}")
	g.w.line("%s%s zx_move_%s(%s *v) { %s t = *v; memset(v, 0, sizeof *v); return t; }", g.linkPrefix(), ct, d.name, ct, ct)
}

// linkPrefix is the storage-class prefix for drop/move helpers (static, or
// external when emitting several translation units).
func (g *gen) linkPrefix() string {
	if g.split {
		return ""
	}
	return "static "
}

func (g *gen) emitStructDef(s *types.Struct) {
	g.w.line("struct %s {", typeName(s.Name))
	g.w.indent++
	if len(s.Fields) == 0 {
		g.w.line("char zx_empty;")
	}
	for _, f := range s.Fields {
		g.w.line("%s %s;", ctype(f.Ty), fieldName(f.Name))
	}
	g.w.indent--
	g.w.line("};")
}

// emitEnumDef emits a tagged union: an int32 tag plus a union of per-variant
// payload structs. Enums with no payload are just a tag.
func (g *gen) emitEnumDef(e *types.Enum) {
	g.w.line("struct %s {", typeName(e.Name))
	g.w.indent++
	g.w.line("int32_t tag;")
	if e.HasPayload() {
		g.w.line("union {")
		g.w.indent++
		for _, v := range e.Variants {
			if len(v.Fields) == 0 {
				continue
			}
			g.w.line("struct {")
			g.w.indent++
			for _, f := range v.Fields {
				g.w.line("%s %s;", ctype(f.Ty), fieldName(f.Name))
			}
			g.w.indent--
			g.w.line("} v%d;", v.Index)
		}
		g.w.indent--
		g.w.line("} u;")
	}
	g.w.indent--
	g.w.line("};")
}

// typeOrder returns struct and enum names sorted so by-value dependencies come
// before their dependents.
func (g *gen) typeOrder() []string {
	state := map[string]int{} // 0 unvisited, 1 visiting, 2 done
	var out []string
	var visit func(name string)
	visit = func(name string) {
		if state[name] != 0 {
			return
		}
		state[name] = 1
		for _, dep := range g.typeDeps(name) {
			visit(dep)
		}
		state[name] = 2
		out = append(out, name)
	}
	for _, s := range g.info.StructOrd {
		visit(s.Name)
	}
	for _, e := range g.info.EnumOrd {
		visit(e.Name)
	}
	for _, name := range g.arrayOrder {
		visit(name)
	}
	return out
}

// typeDeps lists struct/enum/array names that must be complete for `name`.
func (g *gen) typeDeps(name string) []string {
	var out []string
	if s, ok := g.info.Structs[name]; ok {
		for _, f := range s.Fields {
			out = append(out, valueTypeDeps(f.Ty)...)
		}
	}
	if e, ok := g.info.Enums[name]; ok {
		for _, v := range e.Variants {
			for _, f := range v.Fields {
				out = append(out, valueTypeDeps(f.Ty)...)
			}
		}
	}
	if a, ok := g.arrays[name]; ok {
		out = append(out, valueTypeDeps(a.Elem)...)
	}
	// Keep only names we actually define.
	kept := out[:0]
	for _, n := range out {
		if g.defined(n) {
			kept = append(kept, n)
		}
	}
	return kept
}

// defined reports whether a generated type name exists in this translation unit.
func (g *gen) defined(name string) bool {
	if _, ok := g.info.Structs[name]; ok {
		return true
	}
	if _, ok := g.info.Enums[name]; ok {
		return true
	}
	_, ok := g.arrays[name]
	return ok
}

// valueTypeDeps lists nominal type names that a by-value type t requires to be
// complete: the nominal type itself, or any reachable through aggregates.
func valueTypeDeps(t *types.Type) []string {
	if t == nil {
		return nil
	}
	switch t.Kind {
	case types.StructKind:
		if t.St != nil {
			return []string{t.St.Name}
		}
	case types.EnumKind:
		if t.En != nil {
			return []string{t.En.Name}
		}
	case types.Array:
		return []string{arrayTypeName(t)}
	case types.Tuple:
		var out []string
		for _, e := range t.Elems {
			out = append(out, valueTypeDeps(e)...)
		}
		return out
	}
	return nil
}

// arrayTypeName is the deterministic C name of an array wrapper struct.
func arrayTypeName(t *types.Type) string {
	return "zx_arr" + strconv.FormatInt(t.Len, 10) + "_" + typeMangle(t.Elem)
}

// typeMangle renders a type as an identifier fragment for generated names.
func typeMangle(t *types.Type) string {
	if t == nil {
		return "void"
	}
	switch t.Kind {
	case types.StructKind:
		if t.St != nil {
			return safeIdent(t.St.Name)
		}
	case types.EnumKind:
		if t.En != nil {
			return safeIdent(t.En.Name)
		}
	case types.Array:
		return arrayTypeName(t)
	case types.VecKind:
		return vecTypeName(t)
	case types.Ptr:
		return "p_" + typeMangle(t.Elem)
	case types.Ref:
		return "r_" + typeMangle(t.Elem)
	case types.Slice:
		return "s_" + typeMangle(t.Elem)
	case types.Tuple:
		parts := make([]string, len(t.Elems))
		for i, e := range t.Elems {
			parts[i] = typeMangle(e)
		}
		return "t_" + strings.Join(parts, "_")
	}
	return strings.Map(func(r rune) rune {
		if r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			return r
		}
		return '_'
	}, t.String())
}

func (g *gen) prototypes() {
	for _, fn := range g.info.FnOrder {
		if fn.Decl.Body != nil && !fn.Extern && fn.Name != "main" {
			if !g.used[fn] {
				continue // unreachable: never emitted, never declared
			}
			g.w.line("%s%s;", g.staticPrefix(fn), fnSignature(fn))
		} else if fn.Extern {
			g.w.line("%s;", fnSignature(fn))
		}
	}
	g.w.line("")
}

// fnSignature renders a C function declarator without a body, e.g.
// `int32_t zx_add(int32_t a, int32_t b)`.
func fnSignature(fn *sema.Fn) string {
	params := make([]string, 0, len(fn.Decl.Params))
	for i, p := range fn.Decl.Params {
		pt := types.UnitT
		if i < len(fn.Params) {
			pt = fn.Params[i]
		}
		params = append(params, ctype(pt)+" "+localName(p.Name))
	}
	if len(params) == 0 {
		params = []string{"void"}
	}
	return fmt.Sprintf("%s %s(%s)", ctype(fn.Ret), funcName(fn), strings.Join(params, ", "))
}

func (g *gen) funcDef(fn *sema.Fn) {
	ret := fn.Ret
	retC := ctype(ret)
	if fn.Name == "main" {
		ret = types.I32T // C's main returns int
		retC = "int"
	}

	// Name counters are per function, and parameter names are bound before the
	// signature is written so the two always agree.
	g.counts = map[string]int{}
	g.pushScope()
	g.openDropFrame() // function frame: parameters
	params := make([]string, 0, len(fn.Decl.Params))
	for i, p := range fn.Decl.Params {
		pt := types.UnitT
		if i < len(fn.Params) {
			pt = fn.Params[i]
		}
		cn := g.bind(p.Name)
		params = append(params, ctype(pt)+" "+cn)
		g.registerDrop(cn, pt)
	}
	if len(params) == 0 {
		params = []string{"void"}
	}
	if fn.Name == "main" {
		// C's main receives the command line; capture it for the
		// arg_count()/arg_at() builtins.
		params = []string{"int zx_argc_in", "char **zx_argv_in"}
	}

	g.w.line("%s%s %s(%s) {", g.staticPrefix(fn), retC, funcName(fn), strings.Join(params, ", "))
	g.w.indent++
	g.openDropFrame() // body frame
	if fn.Name == "main" {
		g.w.line("zx_argc = zx_argc_in; zx_argv = zx_argv_in;")
	}
	g.blockStmts(fn.Decl.Body, ret)
	g.closeDropFrame()
	g.closeDropFrame()
	g.w.indent--
	g.popScope()
	g.w.line("}")
	g.w.line("")
}

func (g *gen) staticPrefix(fn *sema.Fn) string {
	if fn.Extern || fn.Name == "main" {
		return ""
	}
	if fn.NoMangle {
		// Exported for the C bridge: external linkage under the Zexi name.
		return ""
	}
	if g.split {
		// Multi-unit output: bodies live in different translation units, so
		// user functions need external linkage. A bare `inline` definition
		// would emit no external symbol and cross-unit calls would not link,
		// so the hint is dropped rather than paired with `extern inline`.
		return ""
	}
	switch {
	case fn.InlineNever:
		return "static __attribute__((noinline)) "
	case fn.Inline:
		return "static inline "
	}
	return "static "
}

// blockStmts emits a block's statements; if the function returns a value the
// trailing expression becomes a `return`.
func (g *gen) blockStmts(b *ast.Block, ret *types.Type) {
	for _, s := range b.Stmts {
		g.stmt(s)
	}
	if b.Tail != nil {
		tt := g.info.TypeOf(b.Tail)
		wantValue := ret != nil && ret.Kind != types.Unit &&
			tt != nil && tt.Kind != types.Unit && tt.Kind != types.Never
		if wantValue {
			g.emitReturn(b.Tail)
		} else {
			g.stmtExpr(b.Tail)
		}
		return
	}
	if ret != nil && ret.Kind != types.Unit {
		g.w.line("return %s;", zeroValue(ret))
	}
}

func (g *gen) stmt(s ast.Stmt) {
	switch st := s.(type) {
	case *ast.LetStmt:
		g.letStmt(st)
	case *ast.DeferStmt:
		// Register the expression on the innermost scope frame; it is
		// emitted when that frame exits (see emitFrameExit).
		if len(g.dropFrames) > 0 {
			f := g.dropFrames[len(g.dropFrames)-1]
			f.defers = append(f.defers, st.X)
		} else {
			g.discard(st.X)
		}
	case *ast.ExprStmt:
		g.stmtExpr(st.X)
	default:
		g.w.line("/* unsupported statement */")
	}
}

func (g *gen) letStmt(st *ast.LetStmt) {
	ty := g.info.DeclTypes[st]
	if ty == nil {
		ty = g.info.TypeOf(st.Val)
	}
	if ty != nil && ty.Kind == types.Never {
		// `let x = if c { return .. } else { .. };` never binds a value.
		if st.Val != nil {
			g.discard(st.Val)
		}
		return
	}
	name := ""
	switch p := st.Pat.(type) {
	case *ast.BindPat:
		name = g.bind(p.Name)
	case *ast.WildcardPat:
		name = "_"
	}
	if name == "" || name == "_" {
		if st.Val != nil {
			g.discard(st.Val)
		}
		return
	}
	ct := ctype(ty)
	if st.Val != nil {
		g.w.line("%s %s = %s;", ct, name, g.consume(st.Val))
	} else {
		// Zero-initialize so the end-of-scope destructor is safe.
		g.w.line("%s %s = %s;", ct, name, zeroValue(ty))
	}
	g.registerDrop(name, ty)
}

func (g *gen) stmtExpr(e ast.Expr) {
	switch x := e.(type) {
	case *ast.IfExpr:
		g.ifStmt(x)
	case *ast.LoopExpr:
		g.loopStmt(x)
	case *ast.JumpExpr:
		g.jumpStmt(x)
	case *ast.MatchExpr:
		g.matchStmt(x)
	case *ast.Block:
		g.w.line("{")
		g.w.indent++
		g.openDropFrame()
		g.blockStmts(x, nil)
		g.closeDropFrame()
		g.w.indent--
		g.w.line("}")
	default:
		g.discard(e)
	}
}

// discard evaluates e for its side effects. If the result is a temporary that
// owns a heap resource (e.g. an ignored `String` return value), it is freed.
func (g *gen) discard(e ast.Expr) {
	if t := g.info.TypeOf(e); t != nil && t.NeedsDrop() && !isPlaceExpr(e) {
		tm := g.tmpName()
		g.w.line("%s %s = %s;", ctype(t), tm, g.value(e))
		g.w.line("zx_drop_%s(&%s);", typeMangle(t), tm)
		return
	}
	if v := g.value(e); v != "" {
		g.w.line("%s;", v)
	}
}

func (g *gen) ifStmt(x *ast.IfExpr) {
	g.w.write("if (%s) {\n", g.value(x.Cond))
	g.w.indent++
	g.openDropFrame()
	g.blockStmts(x.Then, nil)
	g.closeDropFrame()
	g.w.indent--
	cur := x
	for cur.Else != nil {
		if elif, ok := cur.Else.(*ast.IfExpr); ok {
			g.w.write("} else if (%s) {\n", g.value(elif.Cond))
			g.w.indent++
			g.openDropFrame()
			g.blockStmts(elif.Then, nil)
			g.closeDropFrame()
			g.w.indent--
			cur = elif
			continue
		}
		g.w.write("} else {\n")
		g.w.indent++
		if blk, ok := cur.Else.(*ast.Block); ok {
			g.openDropFrame()
			g.blockStmts(blk, nil)
			g.closeDropFrame()
		} else {
			g.stmtExpr(cur.Else)
		}
		g.w.indent--
		g.w.write("}\n")
		return
	}
	g.w.write("}\n")
}

func (g *gen) loopStmt(x *ast.LoopExpr) string {
	switch x.Kind {
	case token.KindLoop:
		res := g.loopResult(x)
		brk, cont := g.openLoop(res)
		g.w.line("for (;;) {")
		g.w.indent++
		g.blockStmts(x.Body, nil)
		g.closeLoop()
		g.endLoopBody(cont)
		g.w.indent--
		g.w.line("}")
		g.w.line("%s: ;", brk)
		return res
	case token.KindWhile, token.KindUntil:
		cond := g.value(x.Cond)
		if x.Kind == token.KindUntil {
			cond = "!(" + cond + ")"
		}
		res := g.loopResult(x)
		brk, cont := g.openLoop(res)
		g.w.line("while (%s) {", cond)
		g.w.indent++
		g.blockStmts(x.Body, nil)
		g.closeLoop()
		g.endLoopBody(cont)
		g.w.indent--
		g.w.line("}")
		g.w.line("%s: ;", brk)
		return res
	case token.KindFor:
		return g.forStmt(x)
	}
	return ""
}

// loopValue emits a loop in value position: `loop { ... break v; }` evaluates to
// the `break` value, in the same way a block evaluates to its tail.
func (g *gen) loopValue(x *ast.LoopExpr) string {
	sub := &writer{indent: g.w.indent + 1}
	save := g.w
	g.w = sub
	res := g.loopStmt(x)
	g.w = save
	if res == "" {
		return "({ " + sub.b.String() + " 0; })"
	}
	sub.line("%s;", res)
	return "({ " + sub.b.String() + " })"
}

func (g *gen) forStmt(x *ast.LoopExpr) string {
	rng, ok := x.Iter.(*ast.BinExpr)
	if !ok || (rng.Op != token.KindDotDot && rng.Op != token.KindDotDotEq) {
		return g.forArray(x)
	}
	lt := g.info.TypeOf(rng.L)
	op := "<"
	if rng.Op == token.KindDotDotEq {
		op = "<="
	}
	// Evaluate both bounds once, before the loop, so a bound that is a call is
	// not re-run on every iteration.
	lo := g.tmpName()
	hi := g.tmpName()
	g.w.line("%s %s = %s;", ctype(lt), lo, g.value(rng.L))
	g.w.line("%s %s = %s;", ctype(lt), hi, g.value(rng.R))
	g.pushScope()
	// Bind the loop variable exactly once. Binding a placeholder first would
	// both burn a unique suffix and shadow an outer loop variable of the same
	// name in this scope, so references to the outer variable inside a nested
	// loop would resolve to a C name that is never declared.
	name := g.bindLoopVar(x.Pat)
	g.w.line("for (%s %s = %s; %s %s %s; %s++) {", ctype(lt), name, lo, name, op, hi, name)
	g.w.indent++
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

// forArray lowers `for x in arr`: the array is evaluated once into a temporary,
// then each element (a `Copy` value, enforced by sema) is bound in turn.
func (g *gen) forArray(x *ast.LoopExpr) string {
	it := g.info.TypeOf(x.Iter)
	if base := recvBase(it); base != nil && base.Kind == types.VecKind {
		return g.vecForLoop(x)
	}
	if it == nil || it.Kind != types.Array {
		return "" // sema already diagnosed a non-array iterable
	}
	arr := g.tmpName()
	g.w.line("%s %s = %s;", ctype(it), arr, g.value(x.Iter))
	g.pushScope()
	// See forStmt: bind the element name exactly once.
	name := g.bindLoopVar(x.Pat)
	// The iteration counter is a compiler temporary (`zx__tN`), so a user
	// local named `i` in the body cannot shadow it.
	ctr := g.tmpName()
	g.w.line("for (size_t %s = 0; %s < %d; %s++) {", ctr, ctr, it.Len, ctr)
	g.w.indent++
	g.w.line("%s %s = %s.e[%s];", ctype(it.Elem), name, arr, ctr)
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

func (g *gen) jumpStmt(x *ast.JumpExpr) {
	switch x.Kind {
	case token.KindReturn:
		g.emitReturn(x.Val)
	case token.KindBreak:
		res := ""
		if len(g.loopLabels) > 0 {
			res = g.loopLabels[len(g.loopLabels)-1].result
		}
		if x.Val != nil && res != "" {
			// Move the value out before the body's destructors run, so a break
			// value that is itself a body local is not freed first.
			g.w.line("%s = %s;", res, g.consume(x.Val))
		} else if x.Val != nil {
			g.w.line("%s;", g.value(x.Val))
		}
		if len(g.loopFrames) > 0 {
			g.emitDropsFrom(g.loopFrames[len(g.loopFrames)-1])
		}
		g.w.line("%s", g.loopJump("break", "brk"))
	case token.KindContinue:
		if len(g.loopFrames) > 0 {
			g.emitDropsFrom(g.loopFrames[len(g.loopFrames)-1])
		}
		g.w.line("%s", g.loopJump("continue", "cont"))
	}
}

// loopJump renders a `break`/`continue`. When a compiler-generated construct
// (a `match` lowered to a switch or do-while) sits between the jump and its
// loop, a bare `break`/`continue` would target the wrong construct, so jump to
// the loop's label instead.
func (g *gen) loopJump(kw, which string) string {
	if g.trapDepth == 0 || len(g.loopLabels) == 0 {
		return kw + ";"
	}
	l := g.loopLabels[len(g.loopLabels)-1]
	if which == "brk" {
		return "goto " + l.brk + ";"
	}
	return "goto " + l.cont + ";"
}

// --- expressions ----------------------------------------------------------

// value emits e as a C expression. Compound expressions that contain
// statements are wrapped in a GNU statement expression, `({ ... })`.
func (g *gen) value(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.LiteralExpr:
		return g.literal(x)

	case *ast.IdentExpr:
		if fn := g.info.FnValues[x]; fn != nil {
			return funcName(fn)
		}
		if ctor := g.info.Ctors[x]; ctor != nil {
			return g.enumConstruct(ctor, nil)
		}
		return g.identValue(x.Name, x.Sp)

	case *ast.PathExpr:
		if len(x.Segments) == 1 {
			return g.identValue(x.Segments[0], x.Sp)
		}
		if ctor := g.info.Ctors[x]; ctor != nil {
			return g.enumConstruct(ctor, nil)
		}
		return "0 /* unresolved path */"

	case *ast.UnExpr:
		switch x.Op {
		case token.KindMinus:
			if lit, ok := x.X.(*ast.LiteralExpr); ok && lit.Kind == token.KindInteger {
				if macro, ok := intMinMacro(lit.Text); ok {
					// `-9223372036854775808` is not a valid C constant; the
					// <stdint.h> macro is.
					return macro
				}
			}
			if g.opts.DebugChecks {
				// -INT_MIN overflows; the checked helper reports it instead of
				// silently producing the same negative value.
				if t := g.info.TypeOf(x); t != nil && t.Kind.IsInt() && t.Kind.IsSigned() {
					return "(" + g.wantCheckedArith("neg", ctype(t)) + "(" + g.value(x.X) + "))"
				}
			}
			return "(-" + g.value(x.X) + ")"
		case token.KindBang:
			return "(!" + g.value(x.X) + ")"
		case token.KindStar:
			return "(*" + g.value(x.X) + ")"
		case token.KindAmp:
			return "(&" + g.value(x.X) + ")"
		}
		return "0"

	case *ast.BinExpr:
		return g.binValue(x)

	case *ast.AssignExpr:
		return g.assignValue(x)

	case *ast.CastExpr:
		dst := g.info.TypeOf(x)
		return "((" + ctype(dst) + ")(" + g.value(x.X) + "))"

	case *ast.CallExpr:
		return g.callValue(x)

	case *ast.InterpStrExpr:
		return g.interpStrValue(x)

	case *ast.IfExpr:
		return g.ifValue(x)

	case *ast.Block:
		return g.blockValue(x)

	case *ast.TupleExpr:
		if len(x.Elems) == 0 {
			return "(void)0"
		}
		return "0"

	case *ast.FieldExpr:
		return g.fieldValue(x)

	case *ast.MethodCallExpr:
		return g.methodCallValue(x)

	case *ast.StructLitExpr:
		return g.structLitValue(x)

	case *ast.ArrayLitExpr:
		return g.arrayLitValue(x)

	case *ast.IndexExpr:
		return g.indexValue(x)

	case *ast.SliceExpr:
		return g.sliceValue(x)

	case *ast.MatchExpr:
		return g.matchValue(x)

	case *ast.TryExpr:
		if m := g.info.TryMatches[x]; m != nil {
			return g.matchValue(m)
		}
		return "0 /* unsupported try */"

	case *ast.LoopExpr:
		return g.loopValue(x)

	case *ast.JumpExpr:
		// A jump used in value position only occurs in a diverging tail; emit
		// it as a statement expression so it type-checks.
		sub := &writer{indent: g.w.indent + 1}
		save := g.w
		g.w = sub
		g.jumpStmt(x)
		g.w = save
		return "({ " + sub.b.String() + " 0; })"

	default:
		return "0 /* unsupported expression */"
	}
}

// isStrType reports whether t is the borrowed `str` or the owning `String`.
func isStrType(t *types.Type) bool {
	return t != nil && (t.Kind == types.Str || t.Kind == types.StringKind)
}

// withStrArgs evaluates each argument as a C `const char *`, binding temporary
// `String` operands to locals so their buffers outlive the generated call, then
// dropping those temporaries. `str` operands and `String` places are passed
// through directly (`String` places by address). When retType is "", the call is
// emitted as a statement (its value is discarded); otherwise its result is
// returned from a GNU statement expression.
func (g *gen) withStrArgs(args []ast.Expr, retType string, stringify bool, call func(ps []string) string) string {
	ps := make([]string, len(args))
	var decls, drops []string
	for i, e := range args {
		t := g.info.TypeOf(e)
		if t != nil && t.Kind == types.StringKind {
			if !isPlaceExpr(e) {
				tm := g.tmpName()
				decls = append(decls, "zx_String "+tm+" = "+g.value(e)+";")
				ps[i] = "zx_string_as_str(&" + tm + ")"
				drops = append(drops, "zx_drop_String(&"+tm+");")
				continue
			}
			ps[i] = "zx_string_as_str(&" + g.value(e) + ")"
			continue
		}
		if stringify && isTextable(t) {
			// A number, `bool`, or `char` is rendered into a local buffer so it
			// can be concatenated. The buffer outlives the call because the
			// whole expression is one statement expression.
			buf := g.tmpName()
			decls = append(decls, g.textDecl(buf, e))
			ps[i] = buf
			continue
		}
		ps[i] = g.value(e)
	}
	if len(decls) == 0 {
		return call(ps)
	}
	var b strings.Builder
	b.WriteString("({ ")
	for _, d := range decls {
		b.WriteString(d + " ")
	}
	if retType != "" {
		r := g.tmpName()
		b.WriteString(retType + " " + r + " = " + call(ps) + "; ")
		for _, d := range drops {
			b.WriteString(d + " ")
		}
		b.WriteString(r + "; })")
		return b.String()
	}
	b.WriteString(call(ps) + "; ")
	for _, d := range drops {
		b.WriteString(d + " ")
	}
	b.WriteString("(void)0; })")
	return b.String()
}

// isTextable reports whether t can be rendered as text for `"s" + value`.
func isTextable(t *types.Type) bool {
	if t == nil {
		return false
	}
	return t.Kind.IsNumeric() || t.Kind == types.Bool || t.Kind == types.Char
}

// textDecl declares a local text buffer holding the rendering of e, for use as
// a `str` argument.
func (g *gen) textDecl(buf string, e ast.Expr) string {
	t := g.info.TypeOf(e)
	v := g.value(e)
	if t != nil && t.Kind == types.Char {
		g.hasChar = true // emits the UTF-8 encoder
		return fmt.Sprintf("char %s[8]; %s[zx_utf8_encode((uint32_t)(%s), %s)] = 0;", buf, buf, v, buf)
	}
	format, cast := printFormat(t)
	return fmt.Sprintf(`char %s[32]; snprintf(%s, sizeof %s, "%s", %s);`, buf, buf, buf, format, cast(v))
}

// withArgs emits a call, coercing each argument whose parameter type is `str`
// from an owning `String`. Temporary `String` arguments are bound to locals
// inside a statement expression so their buffers outlive the call and are then
// freed. When retType is "", the call is emitted as a statement.
func (g *gen) withArgs(args []ast.Expr, wants []*types.Type, retType string, call func(ps []string) string) string {
	ps := make([]string, len(args))
	var decls, drops []string
	for i, e := range args {
		wantStr := i < len(wants) && wants[i] != nil && wants[i].Kind == types.Str
		t := g.info.TypeOf(e)
		if wantStr && t != nil && t.Kind == types.StringKind {
			if !isPlaceExpr(e) {
				tm := g.tmpName()
				decls = append(decls, "zx_String "+tm+" = "+g.value(e)+";")
				ps[i] = "zx_string_as_str(&" + tm + ")"
				drops = append(drops, "zx_drop_String(&"+tm+");")
				continue
			}
			ps[i] = "zx_string_as_str(&" + g.value(e) + ")"
			continue
		}
		ps[i] = g.consume(e)
	}
	if len(decls) == 0 {
		return call(ps)
	}
	var b strings.Builder
	b.WriteString("({ ")
	for _, d := range decls {
		b.WriteString(d + " ")
	}
	if retType != "" {
		r := g.tmpName()
		b.WriteString(retType + " " + r + " = " + call(ps) + "; ")
		for _, d := range drops {
			b.WriteString(d + " ")
		}
		b.WriteString(r + "; })")
		return b.String()
	}
	b.WriteString(call(ps) + "; ")
	for _, d := range drops {
		b.WriteString(d + " ")
	}
	b.WriteString("(void)0; })")
	return b.String()
}

// exprStmtType returns the C type used to capture an expression's value when it
// must be wrapped in a statement expression, or "" when it yields no value.
func (g *gen) exprStmtType(x ast.Expr) string {
	t := g.info.TypeOf(x)
	if t == nil || t.Kind == types.Unit || t.Kind == types.Never {
		return ""
	}
	return ctype(t)
}

func (g *gen) binValue(x *ast.BinExpr) string {
	if x.Op == token.KindDotDot || x.Op == token.KindDotDotEq {
		return "0 /* range outside of for */"
	}
	// String concatenation: `+` with a `str`/`String` operand allocates a fresh
	// `String`. Operands are borrowed (their bytes copied), so temporaries are
	// freed only after the result is built. A number, `bool`, or `char` on the
	// other side is rendered as text first.
	if x.Op == token.KindPlus && (isStrType(g.info.TypeOf(x.L)) || isStrType(g.info.TypeOf(x.R))) {
		return g.withStrArgs([]ast.Expr{x.L, x.R}, "zx_String", true, func(ps []string) string {
			return "zx_string_concat(" + ps[0] + ", " + ps[1] + ")"
		})
	}
	// String comparison compares bytes, not pointers, for any mix of `str` and
	// `String`. Ordering follows strcmp's byte order, which is the same order
	// Rust's Ord gives str.
	if isStrType(g.info.TypeOf(x.L)) && isStrType(g.info.TypeOf(x.R)) {
		switch x.Op {
		case token.KindEqEq, token.KindBangEq, token.KindLt, token.KindLe, token.KindGt, token.KindGe:
			return g.withStrArgs([]ast.Expr{x.L, x.R}, "bool", false, func(ps []string) string {
				return "(strcmp(" + ps[0] + ", " + ps[1] + ") " + x.Op.String() + " 0)"
			})
		}
	}
	if g.checkedInt(x) {
		return g.checkedBinOp(x)
	}
	return "(" + g.value(x.L) + " " + x.Op.String() + " " + g.value(x.R) + ")"
}

// checkedInt reports whether `x` is an integer operation that needs a run-time
// check: division and modulo by zero always do, and `+ - *` do in a debug
// build.
func (g *gen) checkedInt(x *ast.BinExpr) bool {
	switch x.Op {
	case token.KindSlash, token.KindPercent:
		t := g.info.TypeOf(x)
		return t != nil && t.Kind.IsInt()
	case token.KindPlus, token.KindMinus, token.KindStar:
		if !g.opts.DebugChecks {
			return false
		}
		t := g.info.TypeOf(x)
		return t != nil && t.Kind.IsInt()
	}
	return false
}

// checkedBinOp lowers a checked integer operation to a single statement
// expression: both operands are evaluated exactly once, in order, and the fault
// panics instead of trapping or wrapping silently.
func (g *gen) checkedBinOp(x *ast.BinExpr) string {
	g.hasPanic = true
	t := g.info.TypeOf(x)
	ct := ctype(t)
	// Operand statements (a nested match, say) are emitted to the enclosing
	// writer first; only the checks stay inside the statement expression.
	lv, rv := g.value(x.L), g.value(x.R)
	a, b, r := g.tmpName(), g.tmpName(), g.tmpName()
	var b2 strings.Builder
	fmt.Fprintf(&b2, "%s %s = %s; %s %s = %s; ", ct, a, lv, ct, b, rv)
	fmt.Fprintf(&b2, "%s %s; ", ct, r)
	if x.Op == token.KindSlash || x.Op == token.KindPercent {
		b2.WriteString(g.checkedDivMod(x.Op, t, a, b, r))
	} else {
		op := map[token.Kind]string{token.KindPlus: "add", token.KindMinus: "sub", token.KindStar: "mul"}[x.Op]
		fmt.Fprintf(&b2, "%s = %s(%s, %s); ", r, g.wantCheckedArith(op, ct), a, b)
	}
	fmt.Fprintf(&b2, "%s;", r)
	return "({ " + b2.String() + " })"
}

// checkedDivMod emits the zero-divisor and signed-overflow checks and stores the
// quotient or remainder in r. a and b are already-evaluated operands of type t.
func (g *gen) checkedDivMod(op token.Kind, t *types.Type, a, b, r string) string {
	g.hasPanic = true
	ct := ctype(t)
	var sb strings.Builder
	// A zero divisor traps on every target, so this check is not optional:
	// without it the process dies on SIGFPE with no message and takes the
	// buffered output with it.
	what := "divide"
	if op == token.KindPercent {
		what = "calculate the remainder of a division"
	}
	fmt.Fprintf(&sb, "if (%s == 0) zx_panicf(\"attempt to %s by zero\"); ", b, what)
	switch {
	case op == token.KindSlash && t.Kind.IsSigned():
		// x86 also traps on INT_MIN / -1, even under -fwrapv, so handle a
		// divisor of -1 without the divide instruction.
		fmt.Fprintf(&sb, "if (%s == (%s)(-1)) { if (%s == (%s)((%s)1 << (sizeof(%s) * 8 - 1))) zx_panicf(\"attempt to divide with overflow\"); %s = (%s)(-%s); } else %s = (%s)(%s / %s); ",
			b, ct, a, ct, ct, ct, r, ct, a, r, ct, a, b)
	case op == token.KindPercent && t.Kind.IsSigned():
		// The remainder of any value by -1 is zero; skipping the divide
		// also avoids the INT_MIN % -1 trap.
		fmt.Fprintf(&sb, "%s = (%s == (%s)(-1)) ? (%s)0 : (%s)(%s %% %s); ", r, b, ct, ct, ct, a, b)
	default:
		opStr := "/"
		if op == token.KindPercent {
			opStr = "%"
		}
		fmt.Fprintf(&sb, "%s = (%s)(%s %s %s); ", r, ct, a, opStr, b)
	}
	return sb.String()
}

// checkedCompoundDiv lowers `lhs /= rhs` and `lhs %= rhs` for integers through
// the same run-time checks as the binary forms. The left-hand place is bound
// once through a pointer, so a subscript with a side effect is evaluated
// exactly once.
func (g *gen) checkedCompoundDiv(x *ast.AssignExpr, t *types.Type) string {
	g.hasPanic = true
	ct := ctype(t)
	op := token.KindSlash
	if x.Op == token.KindPercentEq {
		op = token.KindPercent
	}
	p, a, b, r := g.tmpName(), g.tmpName(), g.tmpName(), g.tmpName()
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s *%s = &(%s); %s %s = *%s; %s %s = %s; %s %s; ",
		ct, p, g.value(x.Lhs), ct, a, p, ct, b, g.value(x.Rhs), ct, r)
	sb.WriteString(g.checkedDivMod(op, t, a, b, r))
	fmt.Fprintf(&sb, " *%s = %s; })", p, r)
	return "({ " + sb.String()
}

func (g *gen) ifValue(x *ast.IfExpr) string {
	t := g.info.TypeOf(x)
	if t == nil || t.Kind == types.Unit || t.Kind == types.Never {
		// Not a value; evaluate for effect.
		sub := &writer{indent: g.w.indent + 1}
		save := g.w
		g.w = sub
		g.ifStmt(x)
		g.w = save
		return "({ " + sub.b.String() + " 0; })"
	}
	if x.Else == nil {
		return "0"
	}
	tmp := g.tmpName()
	sub := &writer{indent: g.w.indent + 1}
	sub.line("%s %s;", ctype(t), tmp)
	sub.line("if (%s) {", g.value(x.Cond))
	sub.indent++
	sub.line("%s = %s;", tmp, g.branchValue(x.Then))
	sub.indent--
	var elseV string
	if blk, ok := x.Else.(*ast.Block); ok {
		elseV = g.branchValue(blk)
	} else {
		elseV = g.value(x.Else)
	}
	sub.line("} else {")
	sub.indent++
	sub.line("%s = %s;", tmp, elseV)
	sub.indent--
	sub.line("}")
	sub.line("%s;", tmp)
	return "({ " + sub.b.String() + " })"
}

// branchValue emits a block's value: statements followed by the tail as a
// value, wrapped as a statement expression.
func (g *gen) branchValue(b *ast.Block) string {
	if len(b.Stmts) == 0 && b.Tail != nil {
		// Consume, like blockValue does: a resource-owning place used as
		// a branch value must move, or both the place and the branch
		// result would own (and drop) the same buffer.
		return g.consume(b.Tail)
	}
	return g.blockValue(b)
}

func (g *gen) blockValue(b *ast.Block) string {
	sub := &writer{indent: g.w.indent + 1}
	save := g.w
	g.w = sub
	g.pushScope()
	g.openDropFrame()
	for _, s := range b.Stmts {
		g.stmt(s)
	}
	final := "(void)0"
	if b.Tail != nil {
		t := g.info.TypeOf(b.Tail)
		if t != nil && t.Kind != types.Unit && t.Kind != types.Never {
			// Materialize before the scope's destructors run: the tail may read
			// locals that the drops would otherwise free first.
			tm := g.tmpName()
			g.w.line("%s %s = %s;", ctype(t), tm, g.consume(b.Tail))
			final = tm
		} else {
			final = g.value(b.Tail)
		}
	}
	g.closeDropFrame()
	g.popScope()
	sub.line("%s;", final)
	g.w = save
	return "({ " + sub.b.String() + " })"
}

func (g *gen) identValue(name string, _ token.Span) string {
	if cn, ok := g.lookupLocal(name); ok {
		return cn
	}
	if _, ok := g.info.Consts[name]; ok {
		return cname(name)
	}
	if fn, ok := g.info.Fns[name]; ok {
		return funcName(fn)
	}
	return "0 /* unresolved " + name + " */"
}

func (g *gen) callValue(x *ast.CallExpr) string {
	if p, ok := x.Callee.(*ast.PathExpr); ok && len(p.Segments) == 2 && p.Segments[0] == "String" {
		return g.stringCtorValue(x, p.Segments[1])
	}
	if p, ok := x.Callee.(*ast.PathExpr); ok && len(p.Segments) == 2 && p.Segments[0] == "Vec" {
		return g.vecCtorValue(x, p.Segments[1])
	}
	// Calling a function value (a local, parameter, or field) needs an
	// explicit function-pointer cast: the value itself has C type `void *`.
	if ft := g.info.Indirect[x]; ft != nil {
		params := make([]string, len(ft.Params))
		for i, p := range ft.Params {
			params[i] = ctype(p)
		}
		sig := ctype(ft.Ret) + " (*)("
		if len(params) == 0 {
			sig += "void"
		} else {
			sig += strings.Join(params, ", ")
		}
		sig += ")"
		callee := g.value(x.Callee)
		return g.withArgs(x.Args, ft.Params, g.exprStmtType(x), func(ps []string) string {
			return "((" + sig + ")" + callee + ")(" + strings.Join(ps, ", ") + ")"
		})
	}
	if ctor := g.info.Ctors[x]; ctor != nil {
		return g.enumConstruct(ctor, x.Args)
	}
	if st := g.info.TupleCtors[x]; st != nil {
		// `Pair(1, 2)`: a compound literal in field order.
		if len(st.Fields) == 0 {
			return "((" + typeName(st.Name) + "){ 0 })"
		}
		var parts []string
		for i, f := range st.Fields {
			s := zeroValue(f.Ty)
			if i < len(x.Args) {
				s = g.consume(x.Args[i])
			}
			parts = append(parts, "."+fieldName(f.Name)+" = "+s)
		}
		return "((" + typeName(st.Name) + "){ " + strings.Join(parts, ", ") + " })"
	}
	if fn := g.info.Resolved[x]; fn != nil {
		return g.withArgs(x.Args, fn.Params, g.exprStmtType(x), func(ps []string) string {
			return funcName(fn) + "(" + strings.Join(ps, ", ") + ")"
		})
	}
	name := ""
	switch c := x.Callee.(type) {
	case *ast.IdentExpr:
		name = c.Name
	case *ast.PathExpr:
		if len(c.Segments) == 1 {
			name = c.Segments[0]
		}
	}
	if name == "print" || name == "println" {
		return g.printfCall(x, name)
	}
	if name == "len" {
		return g.lenValue(x)
	}
	if name == "cstr" {
		return g.cstrValue(x)
	}
	if name == "from_cstr" {
		if len(x.Args) != 1 {
			return "0"
		}
		// Copy the C string into an owning zx_String.
		return "zx_string_from((const char *)(" + g.value(x.Args[0]) + "))"
	}
	if name == "arg_count" {
		return "zx_arg_count()"
	}
	if name == "arg_at" {
		if len(x.Args) != 1 {
			return "zx_string_from(\"\")"
		}
		return "zx_arg_at((int)(" + g.value(x.Args[0]) + "))"
	}
	fn, ok := g.info.Fns[name]
	if !ok {
		return "0 /* call to unknown function */"
	}
	return g.withArgs(x.Args, fn.Params, g.exprStmtType(x), func(ps []string) string {
		return funcName(fn) + "(" + strings.Join(ps, ", ") + ")"
	})
}

// stringCtorValue emits the builtin `String` constructors.
func (g *gen) stringCtorValue(x *ast.CallExpr, name string) string {
	var wants []*types.Type
	if name == "from" {
		wants = []*types.Type{types.StrT}
	}
	return g.withArgs(x.Args, wants, "zx_String", func(ps []string) string {
		return "zx_string_" + name + "(" + strings.Join(ps, ", ") + ")"
	})
}

func (g *gen) printfCall(x *ast.CallExpr, name string) string {
	nl := ""
	if name == "println" {
		nl = `\n`
	}
	if len(x.Args) == 0 {
		return fmt.Sprintf(`printf("%s")`, nl)
	}
	if len(x.Args) == 1 {
		return g.printOne(x.Args[0], nl)
	}
	// Several values are printed separated by a single space.
	parts := make([]string, 0, len(x.Args)+1)
	for i, arg := range x.Args {
		if i > 0 {
			parts = append(parts, `printf(" ")`)
		}
		parts = append(parts, g.printOne(arg, ""))
	}
	if nl != "" {
		parts = append(parts, fmt.Sprintf(`printf("%s")`, nl))
	}
	return "({ " + strings.Join(parts, "; ") + "; (void)0; })"
}

// printOne emits the printing of a single value, optionally followed by `nl`.
func (g *gen) printOne(arg ast.Expr, nl string) string {
	t := g.info.TypeOf(arg)
	if t != nil && t.Kind == types.StringKind {
		if isPlaceExpr(arg) {
			return fmt.Sprintf(`printf("%%s%s", zx_string_as_str(&%s))`, nl, g.value(arg))
		}
		// A temporary owner is released as soon as it has been printed.
		tm := g.tmpName()
		return fmt.Sprintf(`({ zx_String %s = %s; printf("%%s%s", zx_string_as_str(&%s)); zx_drop_String(&%s); (void)0; })`,
			tm, g.value(arg), nl, tm, tm)
	}
	v := g.value(arg)
	if t != nil && t.Kind == types.Char {
		// A `char` is a Unicode scalar value, not a byte: encode it as UTF-8
		// rather than printing the low byte with `%c`.
		g.hasChar = true
		if nl == "" {
			return fmt.Sprintf(`zx_print_char(%s)`, v)
		}
		return fmt.Sprintf(`({ zx_print_char(%s); printf("%s"); })`, v, nl)
	}
	format, cast := printFormat(t)
	return fmt.Sprintf(`printf("%s%s", %s)`, format, nl, cast(v))
}

func printFormat(t *types.Type) (format string, cast func(string) string) {
	if t == nil {
		return "%p", func(v string) string { return "(void *)" + v }
	}
	switch {
	case t.Kind == types.Str:
		return "%s", func(v string) string { return v }
	case t.Kind == types.Char:
		return "%c", func(v string) string { return "(int)(" + v + ")" }
	case t.Kind == types.Bool:
		return "%s", func(v string) string { return "(" + v + ` ? "true" : "false")` }
	case t.Kind.IsFloat():
		return "%g", func(v string) string { return "(double)(" + v + ")" }
	case t.Kind.IsUnsigned():
		return "%llu", func(v string) string { return "(unsigned long long)(" + v + ")" }
	case t.Kind.IsInt():
		return "%lld", func(v string) string { return "(long long)(" + v + ")" }
	default:
		return "%p", func(v string) string { return "(void *)" + v }
	}
}

// fieldValue emits a field access, auto-dereferencing a reference or pointer
// receiver so that `p.x` and `r.x` (where `r: &Point`) both work.
func (g *gen) fieldValue(x *ast.FieldExpr) string {
	rt := g.info.TypeOf(x.Recv)
	recv := g.value(x.Recv)
	if rt != nil && (rt.Kind == types.Ref || rt.Kind == types.Ptr) {
		return "(" + recv + ")->" + fieldName(x.Field)
	}
	return "(" + recv + ")." + fieldName(x.Field)
}

// methodCallValue emits a resolved method call, passing the receiver by value
// or by address according to the method's `self` parameter.
func (g *gen) methodCallValue(x *ast.MethodCallExpr) string {
	if base := recvBase(g.info.TypeOf(x.Recv)); base != nil && base.Kind == types.StringKind {
		return g.stringMethodCall(x)
	}
	if base := recvBase(g.info.TypeOf(x.Recv)); base != nil && base.Kind == types.VecKind {
		return g.vecMethodCall(x)
	}
	// `arr.len()` / `s.len()` mirror the builtin `len`.
	if x.Name == "len" && len(x.Args) == 0 {
		if base := recvBase(g.info.TypeOf(x.Recv)); base != nil {
			if base.Kind == types.Array {
				return strconv.FormatInt(base.Len, 10)
			}
			if base.Kind == types.Str {
				return "strlen(" + g.value(x.Recv) + ")"
			}
		}
	}
	m := g.info.Resolved[x]
	if m == nil {
		return "0 /* unresolved method */"
	}
	var wants []*types.Type
	if m.Self != nil && len(m.Params) > 0 {
		wants = m.Params[1:]
	} else {
		wants = m.Params
	}
	rt := g.info.TypeOf(x.Recv)
	return g.withArgs(x.Args, wants, g.exprStmtType(x), func(ps []string) string {
		var args []string
		if m.Self != nil {
			isPtr := rt != nil && (rt.Kind == types.Ref || rt.Kind == types.Ptr)
			if m.Self.ByRef {
				switch {
				case isPtr:
					args = append(args, g.value(x.Recv))
				case isPlaceExpr(x.Recv):
					args = append(args, "(&"+g.value(x.Recv)+")")
				default:
					// A temporary receiver: bind it to a local so its address
					// can be taken.
					args = append(args, "(&"+g.materialize(x.Recv)+")")
				}
			} else {
				if isPtr {
					if t := g.info.TypeOf(x.Recv); t != nil && t.Elem != nil && t.Elem.NeedsDrop() {
						args = append(args, "zx_move_"+typeMangle(t.Elem)+"("+g.value(x.Recv)+")")
					} else {
						args = append(args, "(*"+g.value(x.Recv)+")")
					}
				} else {
					args = append(args, g.consume(x.Recv))
				}
			}
		}
		args = append(args, ps...)
		return funcName(m) + "(" + strings.Join(args, ", ") + ")"
	})
}

// stringMethodCall emits a builtin `String` method. Borrowing a place passes its
// address (so `&mut self` methods observe the original); a temporary receiver is
// materialized inside a statement expression.
func (g *gen) stringMethodCall(x *ast.MethodCallExpr) string {
	fn := "zx_string_" + x.Name

	// Coerce `String` arguments passed to `str` parameters, binding temporaries
	// so their buffers outlive the call.
	wants := sema.StringMethodParams(x.Name)
	var specs, decls, drops []string
	for i, a := range x.Args {
		wantStr := i < len(wants) && wants[i] != nil && wants[i].Kind == types.Str
		t := g.info.TypeOf(a)
		if wantStr && t != nil && t.Kind == types.StringKind {
			if !isPlaceExpr(a) {
				tm := g.tmpName()
				decls = append(decls, "zx_String "+tm+" = "+g.value(a)+";")
				specs = append(specs, "zx_string_as_str(&"+tm+")")
				drops = append(drops, "zx_drop_String(&"+tm+");")
				continue
			}
			specs = append(specs, "zx_string_as_str(&"+g.value(a)+")")
			continue
		}
		specs = append(specs, g.value(a))
	}
	var restB strings.Builder
	for _, s := range specs {
		restB.WriteString(", ")
		restB.WriteString(s)
	}
	rest := restB.String()

	rt := g.info.TypeOf(x)
	recv := g.info.TypeOf(x.Recv)
	recvIsPtr := recv != nil && (recv.Kind == types.Ref || recv.Kind == types.Ptr)

	var core string
	switch {
	case recvIsPtr:
		// A `&String` / `*String` receiver is already a pointer to the String.
		core = fn + "(" + g.value(x.Recv) + rest + ")"
	case isPlaceExpr(x.Recv):
		core = fn + "(&" + g.value(x.Recv) + rest + ")"
	default:
		t := g.tmpName()
		call := fn + "(&" + t + rest + ")"
		prologue := "zx_String " + t + " = " + g.value(x.Recv) + "; "
		switch {
		case rt == nil || rt.Kind == types.Unit || rt.Kind == types.Never:
			core = "({ " + prologue + call + "; zx_drop_String(&" + t + "); (void)0; })"
		case x.Name == "as_str":
			// The result borrows the temporary's buffer, so the buffer cannot be
			// released until the enclosing statement finishes (M0 limitation).
			core = "({ " + prologue + call + "; })"
		default:
			r := g.tmpName()
			core = "({ " + prologue + ctype(rt) + " " + r + " = " + call + "; zx_drop_String(&" + t + "); " + r + "; })"
		}
	}

	if len(decls) == 0 {
		return core
	}
	var b strings.Builder
	b.WriteString("({ ")
	for _, d := range decls {
		b.WriteString(d + " ")
	}
	if rt == nil || rt.Kind == types.Unit || rt.Kind == types.Never {
		b.WriteString(core + "; ")
		for _, d := range drops {
			b.WriteString(d + " ")
		}
		b.WriteString("(void)0; })")
		return b.String()
	}
	r := g.tmpName()
	b.WriteString(ctype(rt) + " " + r + " = " + core + "; ")
	for _, d := range drops {
		b.WriteString(d + " ")
	}
	b.WriteString(r + "; })")
	return b.String()
}

// structLitValue emits a C compound literal, using designated initializers so
// field order in source does not matter.
func (g *gen) structLitValue(x *ast.StructLitExpr) string {
	if len(x.Path) == 2 {
		if ctor := g.info.Ctors[x]; ctor != nil {
			return g.enumStructValue(ctor, x.Fields)
		}
		return "0 /* unknown variant */"
	}
	if len(x.Path) != 1 {
		return "0 /* invalid struct literal */"
	}
	// A generic struct's literal is an instantiation, so prefer the checked
	// type of the expression over the (field-less) template declaration.
	var st *types.Struct
	if t := g.info.TypeOf(x); t != nil && t.Kind == types.StructKind && t.St != nil {
		st = t.St
	} else if s, ok := g.info.Structs[x.Path[0]]; ok {
		st = s
	}
	if st == nil {
		return "0 /* unknown struct */"
	}
	inits := map[string]ast.Expr{}
	for _, fi := range x.Fields {
		inits[fi.Name] = fi.Val
	}
	if len(st.Fields) == 0 {
		return "((" + typeName(st.Name) + "){ 0 })"
	}
	var parts []string
	for _, f := range st.Fields {
		v, ok := inits[f.Name]
		if !ok {
			continue
		}
		parts = append(parts, fmt.Sprintf(".%s = %s", fieldName(f.Name), g.consume(v)))
	}
	return "((" + typeName(st.Name) + "){ " + strings.Join(parts, ", ") + " })"
}

// enumStructValue emits a struct-variant construction using designated fields.
func (g *gen) enumStructValue(ctor *sema.Ctor, fields []ast.FieldInit) string {
	en, v := ctor.Enum, ctor.Variant
	tag := strconv.Itoa(v.Index)
	if len(v.Fields) == 0 {
		return "((" + typeName(en.Name) + "){ .tag = " + tag + " })"
	}
	inits := map[string]ast.Expr{}
	for _, fi := range fields {
		inits[fi.Name] = fi.Val
	}
	var parts []string
	for _, f := range v.Fields {
		e, ok := inits[f.Name]
		if !ok {
			continue
		}
		parts = append(parts, "."+fieldName(f.Name)+" = "+g.consume(e))
	}
	return "((" + typeName(en.Name) + "){ .tag = " + tag +
		", .u.v" + tag + " = { " + strings.Join(parts, ", ") + " } })"
}

// enumConstruct emits a tagged-union value for an enum variant.
func (g *gen) enumConstruct(ctor *sema.Ctor, args []ast.Expr) string {
	en, v := ctor.Enum, ctor.Variant
	tag := strconv.Itoa(v.Index)
	if len(v.Fields) == 0 {
		return "((" + typeName(en.Name) + "){ .tag = " + tag + " })"
	}
	var inits []string
	for i, f := range v.Fields {
		s := zeroValue(f.Ty)
		if i < len(args) {
			s = g.consume(args[i])
		}
		inits = append(inits, "."+fieldName(f.Name)+" = "+s)
	}
	return "((" + typeName(en.Name) + "){ .tag = " + tag +
		", .u.v" + tag + " = { " + strings.Join(inits, ", ") + " } })"
}

// arrayLitValue emits a compound literal for an array wrapper struct.
func (g *gen) arrayLitValue(x *ast.ArrayLitExpr) string {
	t := g.info.TypeOf(x)
	if t == nil || t.Kind != types.Array {
		return "0 /* invalid array literal */"
	}
	name := arrayTypeName(t)
	if t.Len <= 0 {
		return "((" + name + "){ 0 })"
	}
	if x.Repeat {
		v := g.value(x.Value)
		parts := make([]string, t.Len)
		for i := range parts {
			parts[i] = v
		}
		return "((" + name + "){ .e = { " + strings.Join(parts, ", ") + " } })"
	}
	var vals []string
	for _, e := range x.Elems {
		vals = append(vals, g.consume(e))
	}
	return "((" + name + "){ .e = { " + strings.Join(vals, ", ") + " } })"
}

// indexValue emits a bounds-checked array index.
func (g *gen) indexValue(x *ast.IndexExpr) string {
	rt := g.info.TypeOf(x.Recv)
	recv := g.value(x.Recv)
	idx := g.value(x.Index)
	if rt != nil && rt.Kind == types.Array {
		return "(" + recv + ").e[zx_bounds(" + idx + ", " + strconv.FormatInt(rt.Len, 10) + ")]"
	}
	if base := recvBase(rt); base != nil && base.Kind == types.VecKind {
		return vecIndex("("+g.vecReceiverValue(x.Recv)+")", idx)
	}
	return "(" + recv + ")[" + idx + "]"
}

// sliceValue emits a Python-style slice `s[lo:hi]` as an owning String.
func (g *gen) sliceValue(x *ast.SliceExpr) string {
	recv := g.value(x.Recv)
	rt := g.info.TypeOf(x.Recv)
	low := "0"
	if x.Low != nil {
		low = g.value(x.Low)
	}
	high := ""
	if x.High != nil {
		high = g.value(x.High)
	}
	if rt != nil && rt.Kind == types.StringKind {
		if isPlaceExpr(x.Recv) {
			p := g.tmpName()
			r := g.tmpName()
			expr := "({ const char *" + p + " = zx_string_as_str(&(" + recv + ")); "
			if high != "" {
				expr += "zx_String " + r + " = zx_string_slice(" + p + ", " + low + ", " + high + "); "
			} else {
				expr += "zx_String " + r + " = zx_string_slice(" + p + ", " + low + ", strlen(" + p + ")); "
			}
			expr += r + "; })"
			return expr
		}
		tm := g.tmpName()
		p := g.tmpName()
		r := g.tmpName()
		expr := "({ zx_String " + tm + " = " + recv + "; const char *" + p + " = zx_string_as_str(&" + tm + "); "
		if high != "" {
			expr += "zx_String " + r + " = zx_string_slice(" + p + ", " + low + ", " + high + "); "
		} else {
			expr += "zx_String " + r + " = zx_string_slice(" + p + ", " + low + ", strlen(" + p + ")); "
		}
		expr += "zx_drop_String(&" + tm + "); " + r + "; })"
		return expr
	}
	// str (const char *)
	if isPlaceExpr(x.Recv) {
		if high != "" {
			return "zx_string_slice(" + recv + ", " + low + ", " + high + ")"
		}
		return "zx_string_slice(" + recv + ", " + low + ", strlen(" + recv + "))"
	}
	tm := g.tmpName()
	if high != "" {
		return "({ const char *" + tm + " = " + recv + "; zx_string_slice(" + tm + ", " + low + ", " + high + "); })"
	}
	return "({ const char *" + tm + " = " + recv + "; zx_string_slice(" + tm + ", " + low + ", strlen(" + tm + ")); })"
}

// cstrValue emits the C-bridge builtin `cstr(x)`: a borrowed `*u8`
// pointing at NUL-terminated text. A `str` already is a C string; a
// `String` place lends its buffer. A computed `String` temporary is
// materialized and intentionally not dropped — its buffer must outlive
// the pointer, mirroring how C code treats such hand-offs (bind the
// String to a variable first if that is not acceptable).
func (g *gen) cstrValue(x *ast.CallExpr) string {
	if len(x.Args) != 1 {
		return "0"
	}
	arg := x.Args[0]
	t := g.info.TypeOf(arg)
	if t != nil && t.Kind == types.StringKind {
		if isPlaceExpr(arg) {
			return "((uint8_t *)zx_string_as_str(&(" + g.value(arg) + ")))"
		}
		tm := g.tmpName()
		return "({ zx_String " + tm + " = " + g.value(arg) +
			"; (uint8_t *)zx_string_as_str(&" + tm + "); })"
	}
	return "((uint8_t *)(" + g.value(arg) + "))"
}

// lenValue emits the builtin `len(x)`.
func (g *gen) lenValue(x *ast.CallExpr) string {
	if len(x.Args) != 1 {
		return "0"
	}
	t := g.info.TypeOf(x.Args[0])
	switch {
	case t != nil && t.Kind == types.Array:
		return strconv.FormatInt(t.Len, 10)
	case t != nil && t.Kind == types.Str:
		return "strlen(" + g.value(x.Args[0]) + ")"
	case t != nil && t.Kind == types.VecKind:
		return g.vecLenValue(x)
	case t != nil && t.Kind == types.StringKind:
		if isPlaceExpr(x.Args[0]) {
			return "zx_string_len(&" + g.value(x.Args[0]) + ")"
		}
		tm := g.tmpName()
		return "({ zx_String " + tm + " = " + g.value(x.Args[0]) +
			"; size_t zx_n = zx_string_len(&" + tm + "); zx_drop_String(&" + tm + "); zx_n; })"
	}
	if base := recvBase(t); base != nil && base.Kind == types.VecKind {
		return g.vecLenValue(x)
	}
	return "0"
}

// matchValue emits a `match` in expression position as a statement expression.
func (g *gen) matchValue(x *ast.MatchExpr) string {
	t := g.info.TypeOf(x)
	sub := &writer{indent: g.w.indent + 1}
	save := g.w
	g.w = sub
	g.pushScope()
	orig := g.info.TypeOf(x.Scrutinee)
	st, scrut := g.matchScrutinee(orig, g.value(x.Scrutinee))
	// A by-value match consumes its scrutinee: owned payloads are moved into
	// the bindings, and the remaining value is dropped once at the match end.
	owned := g.matchOwnsScrutinee(x.Scrutinee, st)
	if owned {
		scrut = g.consume(x.Scrutinee)
	}
	m := g.tmpName()
	sub.line("%s %s = %s;", ctype(st), m, scrut)
	if owned {
		g.openDropFrame()
		g.registerDrop(m, st)
	}
	result := ""
	if t != nil && t.Kind != types.Unit && t.Kind != types.Never {
		result = g.tmpName()
		sub.line("%s %s;", ctype(t), result)
	}
	g.emitMatchArms(x, st, m, result, sub)
	if owned {
		g.closeDropFrame()
	}
	if result != "" {
		sub.line("%s;", result)
	}
	g.popScope()
	g.w = save
	return "({ " + sub.b.String() + " })"
}

// exprDiverges reports whether evaluating e always leaves the current function
// or loop (its type is `never`), so a match arm cannot assign a result.
func (g *gen) exprDiverges(e ast.Expr) bool {
	t := g.info.TypeOf(e)
	return t != nil && t.Kind == types.Never
}

// matchOwnsScrutinee reports whether the match takes ownership of its
// scrutinee. A scrutinee reached through a borrow (a reference/pointer, a
// dereference, or a field/element of one) is matched through the borrow and is
// never moved or dropped by the match.
func (g *gen) matchOwnsScrutinee(e ast.Expr, st *types.Type) bool {
	if g.info.PlaceThroughBorrow(e) {
		return false
	}
	return st != nil && st.NeedsDrop()
}

// matchHasGuard reports whether any arm of the match carries a guard.
func matchHasGuard(x *ast.MatchExpr) bool {
	for _, a := range x.Arms {
		if a.Guard != nil {
			return true
		}
	}
	return false
}

// matchScrutinee dereferences a `&T`/`*T` scrutinee so the match tests the value
// itself (`match self`).
func (g *gen) matchScrutinee(st *types.Type, scrut string) (*types.Type, string) {
	if st != nil && (st.Kind == types.Ref || st.Kind == types.Ptr) {
		return st.Elem, "(*" + scrut + ")"
	}
	return st, scrut
}

// emitMatchArms lowers the arms. A `switch` is used when no arm has a guard
// or a complex (or/range) pattern; those need fall-through on guard failure
// or tests a C `switch` cannot express, so they are lowered to an if-chain
// inside a `do { } while (0)`.
func (g *gen) emitMatchArms(x *ast.MatchExpr, st *types.Type, m, result string, w *writer) {
	// String matches also take the guarded path: they test with strcmp,
	// which a C `switch` cannot express.
	if matchHasGuard(x) || matchHasComplexPat(x) || (st != nil && st.Kind == types.Str) {
		g.emitGuardedArms(x, st, m, result, w)
		return
	}
	g.emitSwitch(x, st, m, result, w)
}

// matchHasComplexPat reports whether any arm uses an or-pattern or a range
// pattern, which a C `switch` cannot express directly.
func matchHasComplexPat(x *ast.MatchExpr) bool {
	for _, a := range x.Arms {
		switch a.Pat.(type) {
		case *ast.OrPat, *ast.RangePat:
			return true
		}
	}
	return false
}

// patCond renders the C test for a pattern against disc (the scrutinee value,
// or its `.tag` for enums), or "" when the pattern always matches.
func (g *gen) patCond(p ast.Pattern, disc string, st *types.Type) string {
	isEnum := st != nil && st.Kind == types.EnumKind
	switch pt := p.(type) {
	case *ast.WildcardPat, *ast.BindPat:
		return ""
	case *ast.PathPat:
		if v := g.info.PatVariant[pt]; v != nil && isEnum {
			return disc + " == " + strconv.Itoa(v.Index)
		}
		return ""
	case *ast.LitPat:
		if st != nil && st.Kind == types.Str {
			return "strcmp(" + disc + ", " + strPatLabel(pt) + ") == 0"
		}
		return disc + " == " + litPatLabel(pt)
	case *ast.OrPat:
		var parts []string
		for _, alt := range pt.Alts {
			c := g.patCond(alt, disc, st)
			if c == "" {
				return "" // a catch-all alternative matches everything
			}
			parts = append(parts, "("+c+")")
		}
		return strings.Join(parts, " || ")
	case *ast.RangePat:
		op := "<"
		if pt.Inclusive {
			op = "<="
		}
		return disc + " >= " + litPatLabel(pt.Lo) + " && " + disc + " " + op + " " + litPatLabel(pt.Hi)
	}
	return ""
}

// emitGuardedArms emits one block per arm. When an arm's pattern matches but its
// guard fails, control simply falls through to the next arm; when it matches and
// the guard holds, the body runs and `break` leaves the enclosing do-while(0).
func (g *gen) emitGuardedArms(x *ast.MatchExpr, st *types.Type, m, result string, w *writer) {
	isEnum := st != nil && st.Kind == types.EnumKind
	disc := m
	if isEnum {
		disc = m + ".tag"
	}
	w.line("do {")
	w.indent++
	for _, arm := range x.Arms {
		cond := g.patCond(arm.Pat, disc, st)
		w.line("{")
		w.indent++
		if cond != "" {
			w.line("if (%s) {", cond)
			w.indent++
		}
		g.pushScope()
		g.openDropFrame()
		armFrame := g.dropFrames[len(g.dropFrames)-1]
		g.pendingMoves = nil
		g.bindArm(w, arm.Pat, st, m, isEnum, arm.Guard != nil)
		pend := g.pendingMoves
		g.pendingMoves = nil
		g.trapDepth++
		if arm.Guard != nil {
			w.line("if (%s) {", g.value(arm.Guard))
			w.indent++
		}
		// The guard passed: take ownership of any payloads copied for it.
		for _, pm := range pend {
			w.line("%s = zx_move_%s(&(%s));", pm.name, typeMangle(pm.ty), pm.expr)
			g.registerDrop(pm.name, pm.ty)
		}
		if result != "" && !g.exprDiverges(arm.Body) {
			w.line("%s = %s;", result, g.consume(arm.Body))
		} else {
			save := g.w
			g.w = w
			g.stmtExpr(arm.Body)
			g.w = save
		}
		// On success the arm's payload bindings are released before `break`;
		// when a guard fails they are released on the fall-through path below.
		g.emitFrameExit(armFrame)
		w.line("break;")
		if arm.Guard != nil {
			w.indent--
			w.line("}")
		}
		g.dropFrames = g.dropFrames[:len(g.dropFrames)-1]
		g.trapDepth--
		g.popScope()
		if cond != "" {
			w.indent--
			w.line("}")
		}
		w.indent--
		w.line("}")
	}
	w.indent--
	w.line("} while (0);")
}

// matchStmt emits a `match` in statement position.
func (g *gen) matchStmt(x *ast.MatchExpr) {
	g.pushScope()
	orig := g.info.TypeOf(x.Scrutinee)
	st, scrut := g.matchScrutinee(orig, g.value(x.Scrutinee))
	owned := g.matchOwnsScrutinee(x.Scrutinee, st)
	if owned {
		scrut = g.consume(x.Scrutinee)
	}
	m := g.tmpName()
	g.w.line("%s %s = %s;", ctype(st), m, scrut)
	if owned {
		g.openDropFrame()
		g.registerDrop(m, st)
	}
	g.emitMatchArms(x, st, m, "", g.w)
	if owned {
		g.closeDropFrame()
	}
	g.popScope()
}

// emitSwitch lowers match arms to a C switch over the tag (enums) or the value
// (integers). Arm bodies are wrapped in braces so their bindings are scoped.
func (g *gen) emitSwitch(x *ast.MatchExpr, st *types.Type, m, result string, w *writer) {
	isEnum := st != nil && st.Kind == types.EnumKind
	disc := m
	if isEnum {
		disc = m + ".tag"
	}
	w.line("switch (%s) {", disc)
	w.indent++
	for _, arm := range x.Arms {
		label, isDefault := g.armLabel(arm.Pat, isEnum)
		if isDefault {
			w.line("default: {")
		} else {
			w.line("case %s: {", label)
		}
		w.indent++
		g.pushScope()
		g.openDropFrame()
		g.bindArm(w, arm.Pat, st, m, isEnum, false)
		g.trapDepth++
		if result != "" && !g.exprDiverges(arm.Body) {
			w.line("%s = %s;", result, g.consume(arm.Body))
		} else {
			save := g.w
			g.w = w
			g.stmtExpr(arm.Body)
			g.w = save
		}
		g.closeDropFrame()
		g.trapDepth--
		g.popScope()
		w.line("break;")
		w.indent--
		w.line("}")
	}
	w.indent--
	w.line("}")
}

// armLabel returns the C case label for a match arm, and whether it is the
// default arm.
func (g *gen) armLabel(p ast.Pattern, isEnum bool) (string, bool) {
	switch pt := p.(type) {
	case *ast.WildcardPat, *ast.BindPat:
		return "", true
	case *ast.PathPat:
		if v := g.info.PatVariant[pt]; v != nil && isEnum {
			return strconv.Itoa(v.Index), false
		}
		return "", true
	case *ast.LitPat:
		return litPatLabel(pt), false
	}
	return "", true
}

// bindArm declares the variables a pattern introduces, hoisting payload fields
// out of the tagged union into locals.
func (g *gen) bindArm(w *writer, p ast.Pattern, st *types.Type, m string, isEnum, deferMove bool) {
	switch pt := p.(type) {
	case *ast.BindPat:
		if pt.Name != "_" {
			name := g.bind(pt.Name)
			if st != nil && st.NeedsDrop() {
				if deferMove {
					// A guard may fail, so bind a shallow copy for it; ownership
					// transfers only once the guard has passed.
					w.line("%s %s = %s;", ctype(st), name, m)
					g.pendingMoves = append(g.pendingMoves, pendingMove{name: name, ty: st, expr: m})
				} else {
					// The whole (consumed) scrutinee moves into the binding, so
					// the match-level destructor is left empty.
					w.line("%s %s = zx_move_%s(&%s);", ctype(st), name, typeMangle(st), m)
					g.registerDrop(name, st)
				}
			} else {
				w.line("%s %s = %s;", ctype(st), name, m)
			}
		}
	case *ast.PathPat:
		v := g.info.PatVariant[pt]
		if v == nil || !isEnum || len(v.Fields) == 0 {
			return
		}
		base := m + ".u.v" + strconv.Itoa(v.Index)
		for i, sub := range pt.Sub {
			if i >= len(v.Fields) {
				break
			}
			f := v.Fields[i]
			g.bindPatTo(w, sub, base+"."+fieldName(f.Name), f.Ty, deferMove)
		}
		for _, fp := range pt.Fields {
			f, ok := variantFieldOf(v, fp.Name)
			if !ok {
				continue
			}
			sub := fp.Pat
			if sub == nil {
				sub = &ast.BindPat{Name: fp.Name}
			}
			g.bindPatTo(w, sub, base+"."+fieldName(f.Name), f.Ty, deferMove)
		}
	}
}

// bindPatTo recursively declares bindings for a sub-pattern.
func (g *gen) bindPatTo(w *writer, p ast.Pattern, cExpr string, ty *types.Type, deferMove bool) {
	switch pt := p.(type) {
	case *ast.BindPat:
		if pt.Name != "_" {
			name := g.bind(pt.Name)
			if ty != nil && ty.NeedsDrop() {
				if deferMove {
					w.line("%s %s = %s;", ctype(ty), name, cExpr)
					g.pendingMoves = append(g.pendingMoves, pendingMove{name: name, ty: ty, expr: cExpr})
				} else {
					// Move the payload out of the consumed scrutinee so the match
					// destructor cannot free it a second time.
					w.line("%s %s = zx_move_%s(&(%s));", ctype(ty), name, typeMangle(ty), cExpr)
					g.registerDrop(name, ty)
				}
			} else {
				w.line("%s %s = %s;", ctype(ty), name, cExpr)
			}
		}
	case *ast.PathPat:
		v := g.info.PatVariant[pt]
		if v == nil || ty == nil || ty.Kind != types.EnumKind || len(v.Fields) == 0 {
			return
		}
		base := cExpr + ".u.v" + strconv.Itoa(v.Index)
		for i, sub := range pt.Sub {
			if i >= len(v.Fields) {
				break
			}
			f := v.Fields[i]
			g.bindPatTo(w, sub, base+"."+fieldName(f.Name), f.Ty, deferMove)
		}
		for _, fp := range pt.Fields {
			f, ok := variantFieldOf(v, fp.Name)
			if !ok {
				continue
			}
			sub := fp.Pat
			if sub == nil {
				sub = &ast.BindPat{Name: fp.Name}
			}
			g.bindPatTo(w, sub, base+"."+fieldName(f.Name), f.Ty, deferMove)
		}
	}
}

func variantFieldOf(v *types.Variant, name string) (*types.Field, bool) {
	for i := range v.Fields {
		if v.Fields[i].Name == name {
			return &v.Fields[i], true
		}
	}
	return nil, false
}

// litPatLabel renders a literal pattern as a C case label.
func litPatLabel(p *ast.LitPat) string {
	switch p.Kind {
	case token.KindInteger:
		return strings.ReplaceAll(p.Text, "_", "")
	case token.KindTrue:
		return "1"
	case token.KindFalse:
		return "0"
	case token.KindChar, token.KindByteChar:
		return strconv.FormatInt(int64(firstRune(p.Text)), 10)
	}
	return "0"
}

// strPatLabel renders a string literal pattern as a C string literal.
func strPatLabel(p *ast.LitPat) string {
	if p.Kind == token.KindRawString {
		return cString(p.Text)
	}
	decoded, ok := sema.DecodeString(p.Text)
	if !ok {
		decoded = p.Text
	}
	return cString(decoded)
}

// intMinMacro maps a bare magnitude that is exactly the minimum of a signed
// type to the matching <stdint.h> macro. Writing the positive magnitude in C
// would either overflow a signed type or be silently treated as unsigned.
func intMinMacro(text string) (string, bool) {
	switch strings.ReplaceAll(text, "_", "") {
	case "9223372036854775808":
		return "INT64_MIN", true
	case "2147483648":
		return "INT32_MIN", true
	case "32768":
		return "INT16_MIN", true
	case "128":
		return "INT8_MIN", true
	}
	return "", false
}

func (g *gen) literal(x *ast.LiteralExpr) string {
	switch x.Kind {
	case token.KindInteger:
		s := strings.ReplaceAll(x.Text, "_", "")
		// C has no `0o` prefix, and `0b` is only a GNU extension: emit those
		// forms in decimal.
		if len(s) > 2 && s[0] == '0' {
			if v, err := strconv.ParseUint(s[2:], 8, 64); err == nil && (s[1] == 'o' || s[1] == 'O') {
				s = strconv.FormatUint(v, 10)
			} else if v, err := strconv.ParseUint(s[2:], 2, 64); err == nil && (s[1] == 'b' || s[1] == 'B') {
				s = strconv.FormatUint(v, 10)
			}
		}
		if t := g.info.TypeOf(x); t != nil && t.Kind.IsFloat() {
			return s + ".0"
		}
		return s
	case token.KindFloat:
		s := strings.ReplaceAll(x.Text, "_", "")
		if !strings.ContainsAny(s, ".eE") {
			s += ".0"
		}
		return s
	case token.KindString:
		decoded, ok := sema.DecodeString(x.Text)
		if !ok {
			decoded = x.Text
		}
		return cString(decoded)
	case token.KindRawString:
		// Raw strings are literal text: backslashes are not escapes.
		return cString(x.Text)
	case token.KindChar:
		r := firstRune(x.Text)
		return "(uint32_t)" + strconv.FormatInt(int64(r), 10)
	case token.KindByteChar:
		r := firstRune(x.Text)
		return "(uint8_t)" + strconv.FormatInt(int64(r), 10)
	case token.KindTrue:
		return "true"
	case token.KindFalse:
		return "false"
	}
	return "0"
}

// interpStrValue lowers `f"...{expr}..."` to a GNU statement expression
// that builds a fresh `String` part by part. Interpolated values are only
// read, never moved: a `String` place is borrowed through its `str` view,
// and a computed `String` is dropped after its text is copied out.
func (g *gen) interpStrValue(x *ast.InterpStrExpr) string {
	s := g.tmpName()
	var b strings.Builder
	b.WriteString("({ zx_String " + s + " = zx_string_new();")
	for _, part := range x.Parts {
		// Literal chunks are always string literals; an interpolated
		// literal (`{3.5}`, `{true}`) goes through the typed switch.
		if lit, ok := part.(*ast.LiteralExpr); ok && lit.Kind == token.KindString {
			b.WriteString(" zx_string_push_str(&" + s + ", " + g.literal(lit) + ");")
			continue
		}
		t := g.info.TypeOf(part)
		v := g.value(part)
		switch {
		case t == nil || t.Kind == types.Invalid:
			// Already diagnosed by the checker.
		case t.Kind == types.Str:
			b.WriteString(" zx_string_push_str(&" + s + ", " + v + ");")
		case t.Kind == types.StringKind:
			if isPlaceExpr(part) {
				b.WriteString(" zx_string_push_str(&" + s + ", zx_string_as_str(&(" + v + ")));")
			} else {
				tm := g.tmpName()
				b.WriteString(" { zx_String " + tm + " = " + v + "; zx_string_push_str(&" + s + ", zx_string_as_str(&" + tm + ")); zx_drop_String(&" + tm + "); }")
			}
		case t.Kind.IsInt() && t.Kind.IsSigned():
			b.WriteString(" zx_string_push_i64(&" + s + ", (int64_t)(" + v + "));")
		case t.Kind.IsInt():
			b.WriteString(" zx_string_push_u64(&" + s + ", (uint64_t)(" + v + "));")
		case t.Kind.IsFloat():
			b.WriteString(" zx_string_push_f64(&" + s + ", (double)(" + v + "));")
		case t.Kind == types.Bool:
			b.WriteString(" zx_string_push_str(&" + s + ", (" + v + ") ? \"true\" : \"false\");")
		case t.Kind == types.Char:
			b.WriteString(" zx_string_push(&" + s + ", (uint32_t)(" + v + "));")
		}
	}
	b.WriteString(" " + s + "; })")
	return b.String()
}

// --- naming ---------------------------------------------------------------

var cKeywords = map[string]bool{
	"auto": true, "break": true, "case": true, "char": true, "const": true,
	"continue": true, "default": true, "do": true, "double": true, "else": true,
	"enum": true, "extern": true, "float": true, "for": true, "goto": true,
	"if": true, "inline": true, "int": true, "long": true, "register": true,
	"restrict": true, "return": true, "short": true, "signed": true,
	"sizeof": true, "static": true, "struct": true, "switch": true,
	"typedef": true, "union": true, "unsigned": true, "void": true,
	"volatile": true, "while": true, "bool": true, "_Bool": true,
}

// funcName mangles a Zexi function name into C. `main` and `extern` functions
// keep their names (the latter must, to match the C ABI); every other function
// is prefixed to avoid clashing with libc. Methods are prefixed with their
// struct name.
func funcName(fn *sema.Fn) string {
	if fn.Recv != nil {
		return "zx_" + safeIdent(fn.Recv.String()) + "_" + safeIdent(fn.Name)
	}
	if fn.Name == "main" || fn.Extern || fn.NoMangle {
		// `main`, `extern "C"` functions, and `#[no_mangle]`/`#[export]`
		// functions keep their exact Zexi name as the C symbol.
		return fn.Name
	}
	return "zx_" + safeIdent(fn.Name)
}

// typeName is the C name of a Zexi struct.
func typeName(name string) string { return "zx_" + safeIdent(name) }

// fieldName is the C name of a struct field. Tuple-struct fields (named "0",
// "1", ...) become f0, f1, ... so they are valid C identifiers.
func fieldName(name string) string {
	if name != "" && name[0] >= '0' && name[0] <= '9' {
		return "f" + name
	}
	return safeIdent(name)
}

func cname(name string) string { return "ZX_" + safeIdent(name) }

func safeIdent(name string) string {
	if name == "" {
		return "zx_anon"
	}
	// Mangle injectively: escape `_` as `_5f`, and hex-encode anything that is
	// not an identifier character. This keeps distinct Zexi names distinct in C
	// (e.g. `int` -> `z_int`, `int_` -> `int_5f`).
	var b strings.Builder
	for i := range len(name) {
		ch := name[i]
		switch {
		case ch == '_':
			b.WriteString("_5f")
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9':
			b.WriteByte(ch)
		default:
			b.WriteByte('_')
			b.WriteString(strconv.FormatInt(int64(ch), 16))
			b.WriteByte('_')
		}
	}
	s := b.String()
	if cKeywords[s] {
		s = "z_" + s
	}
	return s
}

// localName renders a parameter/local name for the *signature*; binding uses
// the same unique scheme so the two agree.
func localName(name string) string { return "zx_" + safeIdent(name) }

// bindLoopVar binds a `for` loop's pattern variable to its C name. It must be
// called exactly once per loop: binding a placeholder name first would burn a
// unique suffix *and* record that name in the loop's scope, so a reference to
// an outer loop variable of the same name would resolve to a C identifier that
// is never declared.
func (g *gen) bindLoopVar(p ast.Pattern) string {
	if bp, ok := p.(*ast.BindPat); ok {
		return g.bind(bp.Name)
	}
	// A wildcard (or any other non-binding pattern) still needs a C name for
	// the element; use a compiler temporary so it cannot collide with a user
	// binding of the same name.
	return g.tmpName()
}

func (g *gen) bind(name string) string {
	c := "zx_" + safeIdent(name)
	if g.counts[name] > 0 {
		c = fmt.Sprintf("%s_%d", c, g.counts[name])
	}
	// Never shadow a C-visible function: a local whose mangled name
	// would hide an `extern "C"` function it may call gets a suffix.
	for g.externNames[c] {
		c += "_"
	}
	g.counts[name]++
	if len(g.scopes) > 0 {
		g.scopes[len(g.scopes)-1][name] = c
	}
	return c
}

func (g *gen) lookupLocal(name string) (string, bool) {
	for i := len(g.scopes) - 1; i >= 0; i-- {
		if c, ok := g.scopes[i][name]; ok {
			return c, true
		}
	}
	return "", false
}

func (g *gen) pushScope() { g.scopes = append(g.scopes, map[string]string{}) }
func (g *gen) popScope() {
	if len(g.scopes) > 0 {
		g.scopes = g.scopes[:len(g.scopes)-1]
	}
}

// tmpName returns a fresh compiler-generated C identifier. The `__` can never
// appear in a mangled user name (safeIdent escapes every `_`), so temporaries
// cannot collide with locals.
func (g *gen) tmpName() string {
	g.tmp++
	return fmt.Sprintf("zx__t%d", g.tmp)
}

// --- drop frames ----------------------------------------------------------

func (g *gen) openDropFrame() { g.dropFrames = append(g.dropFrames, &dropFrame{}) }

// openLoop enters a loop body: it opens the body's drop frame and records jump
// labels for `break`/`continue`. `break`/`continue` inside a `match` lowered to
// a C `switch` or `do { } while (0)` would otherwise target that construct, so
// they are instead emitted as `goto` to these labels (see trapDepth).
func (g *gen) openLoop(result string) (brk, cont string) {
	g.openDropFrame()
	g.loopFrames = append(g.loopFrames, len(g.dropFrames)-1)
	brk, cont = g.tmpName(), g.tmpName()
	g.loopLabels = append(g.loopLabels, loopLabel{brk: brk, cont: cont, outerTrap: g.trapDepth, result: result})
	g.trapDepth = 0
	return brk, cont
}

// loopResult declares the storage that carries a `loop`'s `break` value and
// returns its C name, or "" when the loop produces no value. The slot lives
// outside the loop body so the value survives the body's destructors; it is
// not registered for dropping (ownership is handed to the loop expression,
// exactly like the value of a block).
func (g *gen) loopResult(x *ast.LoopExpr) string {
	t := g.info.TypeOf(x)
	if t == nil || t.Kind == types.Unit || t.Kind == types.Never {
		return ""
	}
	res := g.tmpName()
	g.w.line("%s %s = %s;", ctype(t), res, zeroValue(t))
	return res
}

// endLoopBody emits the `continue` target at the tail of a loop body. It must
// be emitted *after* closeLoop, so that a `goto` to it lands past the
// end-of-iteration destructors (which the jump ran itself).
func (g *gen) endLoopBody(cont string) { g.w.line("%s: ;", cont) }

func (g *gen) closeLoop() {
	if len(g.loopFrames) > 0 {
		g.loopFrames = g.loopFrames[:len(g.loopFrames)-1]
	}
	if len(g.loopLabels) > 0 {
		g.trapDepth = g.loopLabels[len(g.loopLabels)-1].outerTrap
		g.loopLabels = g.loopLabels[:len(g.loopLabels)-1]
	}
	g.closeDropFrame()
}

func (g *gen) closeDropFrame() {
	if len(g.dropFrames) == 0 {
		return
	}
	f := g.dropFrames[len(g.dropFrames)-1]
	g.emitFrameExit(f)
	g.dropFrames = g.dropFrames[:len(g.dropFrames)-1]
}

// emitFrameExit emits everything that must happen when a scope is left: its
// deferred expressions first, in reverse order of their `defer` statements
// (so they can still read the locals), then the destructors of the locals
// declared in the scope.
func (g *gen) emitFrameExit(f *dropFrame) {
	for i := len(f.defers) - 1; i >= 0; i-- {
		g.discard(f.defers[i])
	}
	g.emitDrops(f)
}

func (g *gen) emitDrops(f *dropFrame) {
	for i := len(f.vars) - 1; i >= 0; i-- {
		v := f.vars[i]
		g.w.line("zx_drop_%s(&%s);", typeMangle(v.ty), v.cname)
	}
}

// emitDropsFrom runs deferred expressions and destructors for every frame
// from the innermost down to and including frame `start`. Used on `return`,
// `break`, and `continue`.
func (g *gen) emitDropsFrom(start int) {
	for i := len(g.dropFrames) - 1; i >= start; i-- {
		g.emitFrameExit(g.dropFrames[i])
	}
}

// materialize binds an rvalue expression to a temporary in the current block so
// that its address can be taken, and schedules it for dropping at scope exit.
func (g *gen) materialize(e ast.Expr) string {
	t := g.info.TypeOf(e)
	tm := g.tmpName()
	g.w.line("%s %s = %s;", ctype(t), tm, g.consume(e))
	g.registerDrop(tm, t)
	return tm
}

func (g *gen) registerDrop(cname string, t *types.Type) {
	if t == nil || !t.NeedsDrop() || len(g.dropFrames) == 0 {
		return
	}
	f := g.dropFrames[len(g.dropFrames)-1]
	f.vars = append(f.vars, dropVar{cname: cname, ty: t})
}

// --- moves ----------------------------------------------------------------

// isPlaceExpr reports whether e denotes an addressable location (an lvalue).
// A field or index is only a place if its base is: `f().x` is an rvalue.
func isPlaceExpr(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.IdentExpr:
		return true
	case *ast.FieldExpr:
		return isPlaceExpr(x.Recv)
	case *ast.IndexExpr:
		return isPlaceExpr(x.Recv)
	case *ast.UnExpr:
		// A dereference `*p` denotes the pointee, which is a place.
		return x.Op == token.KindStar
	}
	return false
}

// recvBase strips references and pointers to find the underlying nominal type.
func recvBase(t *types.Type) *types.Type {
	for t != nil && (t.Kind == types.Ref || t.Kind == types.Ptr) {
		t = t.Elem
	}
	return t
}

// consume emits e in a by-value context: a resource-owning place is moved (the
// buffer transfers to the new owner and the source is left empty, so its later
// destructor is a no-op); everything else is simply evaluated.
func (g *gen) consume(e ast.Expr) string {
	if t := g.info.TypeOf(e); t != nil && t.NeedsDrop() && isPlaceExpr(e) {
		return "zx_move_" + typeMangle(t) + "(&" + g.value(e) + ")"
	}
	return g.value(e)
}

// emitReturn emits a `return`, first moving the value out (so it is not dropped
// with the locals) and then running the destructors of every live local.
func (g *gen) emitReturn(val ast.Expr) {
	if val == nil {
		g.emitDropsFrom(0)
		g.w.line("return;")
		return
	}
	t := g.info.TypeOf(val)
	if t != nil && t.Kind == types.Never {
		// The expression diverges, so there is nothing to return.
		g.w.line("%s;", g.value(val))
		return
	}
	if t == nil || t.Kind == types.Unit {
		if v := g.value(val); v != "" && v != "0" {
			g.w.line("%s;", v)
		}
		g.emitDropsFrom(0)
		g.w.line("return;")
		return
	}
	// Materialize the value *before* running destructors: the expression may
	// read locals (or by-value parameters) that the drops are about to free.
	tmp := g.tmpName()
	g.w.line("%s %s = %s;", ctype(t), tmp, g.consume(val))
	g.emitDropsFrom(0)
	g.w.line("return %s;", tmp)
}

// assignValue emits an assignment; for resource-owning targets the old value is
// dropped first, and resource-owning sources are moved.
func (g *gen) assignValue(x *ast.AssignExpr) string {
	if x.Op == token.KindPlusEq {
		if lt := g.info.TypeOf(x.Lhs); lt != nil && lt.Kind == types.StringKind {
			lhs := g.value(x.Lhs)
			return g.withStrArgs([]ast.Expr{x.Rhs}, "", true, func(ps []string) string {
				return "zx_string_push_str(&" + lhs + ", " + ps[0] + ")"
			})
		}
	}
	if x.Op != token.KindEq {
		if op, ok := compoundArithOp[x.Op]; ok && g.opts.DebugChecks {
			if t := g.info.TypeOf(x.Lhs); t != nil && t.Kind.IsInt() {
				// Bind the place once through a pointer, so a subscript with a
				// side effect is still evaluated exactly once.
				lhs := g.value(x.Lhs)
				rhs := g.value(x.Rhs)
				p, res := g.tmpName(), g.tmpName()
				return fmt.Sprintf("({ %s *%s = &(%s); %s %s = %s(*%s, %s); *%s = %s; })",
					ctype(t), p, lhs, ctype(t), res, g.wantCheckedArith(op, ctype(t)), p, rhs, p, res)
			}
		}
		if x.Op == token.KindSlashEq || x.Op == token.KindPercentEq {
			if t := g.info.TypeOf(x.Lhs); t != nil && t.Kind.IsInt() {
				// Division and modulo are checked in every profile: a zero
				// divisor (and INT_MIN / -1) is undefined behaviour, not a
				// performance choice. The place is bound once through a
				// pointer, like the debug arithmetic path above.
				return g.checkedCompoundDiv(x, t)
			}
		}
		return "(" + g.value(x.Lhs) + " " + x.Op.String() + " " + g.value(x.Rhs) + ")"
	}
	lt := g.info.TypeOf(x.Lhs)
	rhs := g.consume(x.Rhs)
	if lt != nil && lt.NeedsDrop() && isPlaceExpr(x.Lhs) {
		lhs := g.value(x.Lhs)
		// Materialize the RHS first: it may read the value being overwritten
		// (e.g. `s = s + "x"`), which the drop would free.
		tmp := g.tmpName()
		return "({ " + ctype(lt) + " " + tmp + " = " + rhs + "; zx_drop_" + typeMangle(lt) + "(&" + lhs + "); " + lhs + " = " + tmp + "; })"
	}
	return "(" + g.value(x.Lhs) + " = " + rhs + ")"
}

// --- types and literals ---------------------------------------------------

// compoundArithOp maps a compound assignment operator to the checked helper
// that implements its arithmetic.
var compoundArithOp = map[token.Kind]string{
	token.KindPlusEq:  "add",
	token.KindMinusEq: "sub",
	token.KindStarEq:  "mul",
}

func ctype(t *types.Type) string {
	if t == nil {
		return "void"
	}
	switch t.Kind {
	case types.Unit, types.Never, types.Invalid:
		return "void"
	case types.Bool:
		return "bool"
	case types.Char:
		return "uint32_t"
	case types.Str:
		return "const char *"
	case types.StringKind:
		return "zx_String"
	case types.VecKind:
		return vecTypeName(t)
	case types.I8:
		return "int8_t"
	case types.I16:
		return "int16_t"
	case types.I32:
		return "int32_t"
	case types.I64:
		return "int64_t"
	case types.Isize:
		return "intptr_t"
	case types.U8:
		return "uint8_t"
	case types.U16:
		return "uint16_t"
	case types.U32:
		return "uint32_t"
	case types.U64:
		return "uint64_t"
	case types.Usize:
		return "size_t"
	case types.F32:
		return "float"
	case types.F64:
		return "double"
	case types.Ptr, types.Ref:
		return ctype(t.Elem) + " *"
	case types.Array:
		return arrayTypeName(t)
	case types.Slice:
		return "const void *"
	case types.StructKind:
		if t.St != nil {
			return typeName(t.St.Name)
		}
		return "void"
	case types.EnumKind:
		if t.En != nil {
			return typeName(t.En.Name)
		}
		return "void"
	case types.Fn:
		return "void *"
	}
	return "void"
}

func zeroValue(t *types.Type) string {
	if t == nil {
		return "0"
	}
	switch {
	case t.Kind == types.Bool:
		return "false"
	case t.Kind.IsFloat():
		return "0.0"
	case t.Kind == types.Str, t.Kind == types.Ptr, t.Kind == types.Ref:
		return "0"
	case t.Kind == types.StringKind:
		return "((zx_String){ 0 })"
	case t.Kind == types.VecKind:
		return "((" + vecTypeName(t) + "){ 0 })"
	case t.Kind == types.StructKind && t.St != nil:
		return "((" + typeName(t.St.Name) + "){ 0 })"
	case t.Kind == types.EnumKind && t.En != nil:
		return "((" + typeName(t.En.Name) + "){ 0 })"
	case t.Kind == types.Array:
		return "((" + arrayTypeName(t) + "){ 0 })"
	default:
		return "0"
	}
}

func constLiteral(v sema.ConstVal, t *types.Type) string {
	switch {
	case v.IsStr:
		s := v.Str
		if !v.Raw {
			if decoded, ok := sema.DecodeString(v.Str); ok {
				s = decoded
			}
		}
		return cString(s)
	case v.IsBool:
		if v.Bool {
			return "true"
		}
		return "false"
	case v.IsFloat:
		s := strconv.FormatFloat(v.Float, 'g', -1, 64)
		if !strings.ContainsAny(s, ".eE") {
			s += ".0"
		}
		return s
	default:
		if t != nil && t.Kind.IsUnsigned() {
			s := strconv.FormatUint(v.Uint, 10)
			if (t.Kind == types.U64 || t.Kind == types.Usize) && v.Uint > 9223372036854775807 {
				// Otherwise C treats the constant as unsigned with a warning.
				s += "ULL"
			}
			return s
		}
		if t != nil && (t.Kind == types.I64 || t.Kind == types.Isize) && v.Int == -9223372036854775808 {
			// `-9223372036854775808` is not a valid C constant.
			return "INT64_MIN"
		}
		return strconv.FormatInt(v.Int, 10)
	}
}

// cString renders a Go string as a C string literal, escaping bytes that would
// otherwise terminate or corrupt the literal.
func cString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := range len(s) {
		c := s[i]
		switch c {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if c < 0x20 || c == 0x7f {
				fmt.Fprintf(&b, `\%03o`, c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func firstRune(inner string) rune {
	decoded, ok := sema.DecodeString(inner)
	if !ok || decoded == "" {
		return 0
	}
	for _, r := range decoded {
		return r
	}
	return 0
}

// zxStringRuntime is the built-in owning heap string. `String` values own a
// malloc'd buffer; the generated move helper transfers ownership and clears the
// source, and the destructor frees the buffer (nulling the fields so a second
// destructor run is a no-op). This is emitted once at the top of every program.
const zxStringRuntime = `
// --- builtin String: an owning, UTF-8 heap string -------------------------
typedef struct { char *ptr; size_t len; size_t cap; } zx_String;

/* Grow so that len + extra + 1 bytes fit, doubling the capacity so repeated
   appends are amortized O(1). The cap field is a lower bound on the
   allocation; it may be stale on a value built by a compound literal, so it is
   only trusted as a hint and never used to shrink an allocation. */
static void zx_string_reserve(zx_String *s, size_t extra) {
    size_t need = s->len + extra + 1;
    if (s->cap >= need) return;
    size_t nc = s->cap >= s->len ? s->cap * 2 : s->len + 1;
    if (nc < need) nc = need;
    char *np = (char *)realloc(s->ptr, nc);
    if (!np) zx_panicf("out of memory");
    s->ptr = np;
    s->cap = nc;
}

static zx_String zx_string_new(void) {
    zx_String s; s.ptr = NULL; s.len = 0; s.cap = 0; return s;
}

static zx_String zx_string_from(const char *p) {
    zx_String s;
    if (!p) p = "";
    s.len = strlen(p);
    s.ptr = (char *)malloc(s.len + 1);
    if (!s.ptr) zx_panicf("out of memory");
    memcpy(s.ptr, p, s.len + 1);
    s.cap = s.len + 1;
    return s;
}

static zx_String zx_arg_at(int i) {
    if (i < 0 || i >= zx_argc || !zx_argv) return zx_string_from("");
    return zx_string_from(zx_argv[i]);
}

static zx_String zx_string_with_capacity(size_t n) {
    zx_String s;
    s.len = 0;
    s.ptr = (char *)malloc(n + 1);
    if (!s.ptr) zx_panicf("out of memory");
    s.ptr[0] = 0;
    s.cap = n + 1;
    return s;
}

// Concatenate two C strings into a fresh owning String (a + b). Both inputs
// are borrowed, so the caller's values remain valid.
static zx_String zx_string_concat(const char *a, const char *b) {
    size_t la = a ? strlen(a) : 0;
    size_t lb = b ? strlen(b) : 0;
    zx_String r;
    r.len = la + lb;
    r.ptr = (char *)malloc(la + lb + 1);
    if (!r.ptr) zx_panicf("out of memory");
    r.cap = la + lb + 1;
    if (la) memcpy(r.ptr, a, la);
    if (lb) memcpy(r.ptr + la, b, lb);
    r.ptr[la + lb] = 0;
    return r;
}

static size_t zx_string_len(const zx_String *s) { return s->len; }

static zx_String zx_string_slice(const char *s, int64_t lo, int64_t hi) {
    if (!s) s = "";
    int64_t n = (int64_t)strlen(s);
    if (lo < 0) lo += n;
    if (hi < 0) hi += n;
    if (lo < 0) lo = 0;
    if (hi < 0) hi = 0;
    if (lo > n) lo = n;
    if (hi > n) hi = n;
    if (hi < lo) { lo = hi; }
    size_t len = (size_t)(hi - lo);
    zx_String r;
    r.len = len;
    r.ptr = (char *)malloc(len + 1);
    if (!r.ptr) zx_panicf("out of memory");
    r.cap = len + 1;
    if (len) memcpy(r.ptr, s + lo, len);
    r.ptr[len] = 0;
    return r;
}
static bool zx_string_is_empty(const zx_String *s) { return s->len == 0; }
static const char *zx_string_as_str(const zx_String *s) { return s->ptr ? s->ptr : ""; }
/* Clear keeps the buffer, so a cleared string can be refilled for free. */
static void zx_string_clear(zx_String *s) { if (s->ptr) s->ptr[0] = 0; s->len = 0; }

static void zx_string_push_str(zx_String *s, const char *p) {
    if (!p) return;
    size_t n = strlen(p);
    /* p may point into this string's own buffer (e.g. s.push_str(s.as_str())
       or s += s); remember its offset and re-derive it after realloc, which
       may move the buffer. */
    const char *src = p;
    long off = -1;
    if (s->ptr) {
        uintptr_t base = (uintptr_t)s->ptr, here = (uintptr_t)p;
        if (here >= base && here <= base + (uintptr_t)s->len) off = (long)(here - base);
    }
    zx_string_reserve(s, n);
    if (off >= 0) src = s->ptr + off;
    /* memmove: the source may overlap the destination (self-append). */
    memmove(s->ptr + s->len, src, n + 1);
    s->len += n;
}

static void zx_string_push_i64(zx_String *s, int64_t v) {
    char buf[32];
    snprintf(buf, sizeof buf, "%lld", (long long)v);
    zx_string_push_str(s, buf);
}
static void zx_string_push_u64(zx_String *s, uint64_t v) {
    char buf[32];
    snprintf(buf, sizeof buf, "%llu", (unsigned long long)v);
    zx_string_push_str(s, buf);
}
static void zx_string_push_f64(zx_String *s, double v) {
    char buf[64];
    snprintf(buf, sizeof buf, "%g", v);
    zx_string_push_str(s, buf);
}

static int zx_utf8_encode(unsigned int c, char *out) {
    if (c < 0x80) { out[0] = (char)c; return 1; }
    if (c < 0x800) { out[0] = (char)(0xC0 | (c >> 6)); out[1] = (char)(0x80 | (c & 0x3F)); return 2; }
    if (c < 0x10000) { out[0] = (char)(0xE0 | (c >> 12)); out[1] = (char)(0x80 | ((c >> 6) & 0x3F)); out[2] = (char)(0x80 | (c & 0x3F)); return 3; }
    out[0] = (char)(0xF0 | (c >> 18)); out[1] = (char)(0x80 | ((c >> 12) & 0x3F)); out[2] = (char)(0x80 | ((c >> 6) & 0x3F)); out[3] = (char)(0x80 | (c & 0x3F)); return 4;
}

static void zx_string_push(zx_String *s, uint32_t c) {
    char buf[4];
    int n = zx_utf8_encode(c, buf);
    zx_string_reserve(s, (size_t)n);
    memcpy(s->ptr + s->len, buf, (size_t)n);
    s->len += (size_t)n;
    s->ptr[s->len] = 0;
}

// Append one raw byte (no UTF-8 encoding), used when copying binary or
// already-encoded data into a String (e.g. a socket read).
static void zx_string_push_byte(zx_String *s, uint8_t b) {
    zx_string_reserve(s, 1);
    s->ptr[s->len] = (char)b;
    s->len += 1;
    s->ptr[s->len] = 0;
}

static zx_String zx_string_clone(const zx_String *s) {
    zx_String r;
    r.len = s->len;
    r.ptr = (char *)malloc(r.len + 1);
    if (!r.ptr) zx_panicf("out of memory");
    r.cap = r.len + 1;
    if (s->ptr) memcpy(r.ptr, s->ptr, r.len + 1); else r.ptr[0] = 0;
    return r;
}

static int64_t zx_string_byte_at(const zx_String *s, size_t i) {
    if (i >= s->len || !s->ptr) return -1;
    return (int64_t)(unsigned char)s->ptr[i];
}

static int64_t zx_string_find(const zx_String *s, const char *needle) {
    if (!s->ptr || !needle) return -1;
    size_t nl = strlen(needle);
    if (nl == 0) return 0;
    if (nl > s->len) return -1;
    for (size_t i = 0; i + nl <= s->len; i++) {
        if (memcmp(s->ptr + i, needle, nl) == 0) return (int64_t)i;
    }
    return -1;
}

static bool zx_string_contains(const zx_String *s, const char *needle) {
    return zx_string_find(s, needle) >= 0;
}

static bool zx_string_starts_with(const zx_String *s, const char *prefix) {
    if (!s->ptr || !prefix) return false;
    size_t pl = strlen(prefix);
    if (pl > s->len) return false;
    return memcmp(s->ptr, prefix, pl) == 0;
}

static bool zx_string_ends_with(const zx_String *s, const char *suffix) {
    if (!s->ptr || !suffix) return false;
    size_t sl = strlen(suffix);
    if (sl > s->len) return false;
    return memcmp(s->ptr + (s->len - sl), suffix, sl) == 0;
}

static zx_String zx_string_substr(const zx_String *s, size_t start, size_t n) {
    if (start > s->len) start = s->len;
    if (n > s->len - start) n = s->len - start;
    zx_String r;
    r.len = n;
    r.ptr = (char *)malloc(n + 1);
    if (!r.ptr) zx_panicf("out of memory");
    r.cap = n + 1;
    if (n && s->ptr) memcpy(r.ptr, s->ptr + start, n);
    r.ptr[n] = 0;
    return r;
}

static zx_String zx_string_trim(const zx_String *s) {
    size_t a = 0, b = s->ptr ? s->len : 0;
    while (a < b && (unsigned char)s->ptr[a] <= ' ') a++;
    while (b > a && (unsigned char)s->ptr[b - 1] <= ' ') b--;
    zx_String r;
    r.len = b - a;
    r.ptr = (char *)malloc(r.len + 1);
    if (!r.ptr) zx_panicf("out of memory");
    r.cap = r.len + 1;
    if (r.len) memcpy(r.ptr, s->ptr + a, r.len);
    r.ptr[r.len] = 0;
    return r;
}

static int64_t zx_string_parse_i64(const zx_String *s) {
    if (!s->ptr) return 0;
    return (int64_t)strtoll(s->ptr, NULL, 10);
}

static void zx_drop_String(zx_String *s) { free(s->ptr); s->ptr = NULL; s->len = 0; s->cap = 0; }
static zx_String zx_move_String(zx_String *v) { zx_String t = *v; v->ptr = NULL; v->len = 0; v->cap = 0; return t; }
`
