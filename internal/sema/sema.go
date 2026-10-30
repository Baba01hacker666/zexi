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
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"zexi/internal/ast"
	"zexi/internal/diag"
	"zexi/internal/token"
	"zexi/internal/types"
)

// Fn is a checked function.
type Fn struct {
	Decl     *ast.FnDecl
	Name     string
	QualName string   // "Point::len_sq" for methods, else Name
	Generics []string // declared type-parameter names
	Generic  bool     // true for the un-instantiated template
	Params   []*types.Type
	// ParamNames and Defaults are aligned with Params (the receiver, when
	// present, occupies slot 0 with an empty name and no default).
	// Defaults[i] is the materialized literal for parameter i's default
	// value, or nil when the argument is required.
	ParamNames []string
	Defaults   []ast.Expr
	Ret        *types.Type
	Self       *SelfParam
	Recv       *types.Type // receiver type for methods (struct or enum), else nil
	Extern     bool
	ABI        string
	// Inline records `#[inline]`; InlineNever records `#[inline(never)]`.
	Inline      bool
	InlineNever bool
	// NoMangle keeps the Zexi name as the C symbol and gives it external
	// linkage, so C code can call it (`#[no_mangle]` or `#[export]`).
	NoMangle bool
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
	Methods    map[string]map[string]*Fn  // struct name -> method name -> fn
	Resolved   map[ast.Expr]*Fn           // resolved call / method target
	FnValues   map[ast.Expr]*Fn           // resolved function value (instantiated generic)
	Indirect   map[ast.Expr]*types.Type   // calls through a function value
	Ctors      map[ast.Expr]*Ctor         // resolved enum-variant construction
	TupleCtors map[ast.Expr]*types.Struct // resolved tuple-struct construction
	Aliases    map[string]*types.Type     // resolved `type` aliases
	PatVariant map[ast.Pattern]*types.Variant
	// TryMatches maps a `?` expression to the ordinary `match` it desugars to.
	TryMatches map[*ast.TryExpr]*ast.MatchExpr
	// CBody records functions whose entire body is written in C (`c r"..."`),
	// mapped to the body so codegen can emit it verbatim.
	CBody map[*ast.FnDecl]*ast.CBodyExpr

	// PreludeMatches maps `Option`/`Result` extractor calls (`unwrap`,
	// `expect`, `unwrap_or`) to the ordinary `match` they desugar to, so the
	// extractors reuse the move analysis and drop glue a written-out match
	// already gets.
	PreludeMatches map[*ast.MethodCallExpr]*ast.MatchExpr
	// PreludeTag records the tag test a `is_ok`/`is_err`/`is_some`/`is_none`
	// call lowers to: the variant name whose tag is compared.
	PreludeTag map[*ast.MethodCallExpr]string
	// Closures maps each lambda expression to its resolved signature and the
	// variables it captures from the enclosing function.
	Closures map[*ast.LambdaExpr]*Closure
	// ClosureOrd is every lambda in source order, so codegen can emit the
	// lifted functions deterministically.
	ClosureOrd []*ast.LambdaExpr
	Types      map[ast.Expr]*types.Type
	DeclTypes  map[ast.Stmt]*types.Type
	Main       *Fn
}

// Closure is the resolved shape of a lambda: its parameter and return types
// plus the enclosing bindings it captures (by value).
type Closure struct {
	Params   []*types.Type
	Ret      *types.Type
	Captures []Capture
}

// Capture is one variable a closure copies into its environment. Direct marks
// the closure whose body actually reads the variable; enclosing closures carry
// it too (they must hand the value down when the inner closure is built).
type Capture struct {
	Name   string
	Ty     *types.Type
	Direct bool
}

// Ctor is a resolved enum-variant constructor.
type Ctor struct {
	Enum    *types.Enum
	Variant *types.Variant
}

// TypeOf returns the inferred type of an expression, or nil.
func (in *Info) TypeOf(e ast.Expr) *types.Type { return in.Types[e] }

// PlaceThroughBorrow reports whether e denotes a value reached through a
// reference or pointer: an expression of reference/pointer type, a dereference
// of one, or a field/element reached by auto-dereferencing one. Such a place is
// borrowed, so it cannot be moved out of (and a match must not drop it).
func (in *Info) PlaceThroughBorrow(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.UnExpr:
		if x.Op == token.KindStar {
			if t := in.TypeOf(x.X); t != nil && (t.Kind == types.Ref || t.Kind == types.Ptr) {
				return true
			}
			return in.PlaceThroughBorrow(x.X)
		}
	case *ast.FieldExpr:
		if t := in.TypeOf(x.Recv); t != nil && (t.Kind == types.Ref || t.Kind == types.Ptr) {
			return true
		}
		return in.PlaceThroughBorrow(x.Recv)
	case *ast.IndexExpr:
		if t := in.TypeOf(x.Recv); t != nil && (t.Kind == types.Ref || t.Kind == types.Ptr) {
			return true
		}
		return in.PlaceThroughBorrow(x.Recv)
	}
	t := in.TypeOf(e)
	return t != nil && (t.Kind == types.Ref || t.Kind == types.Ptr)
}

// Check resolves names and infers types across the file.
func Check(file *ast.File) (*Info, []diag.Diagnostic) {
	c := &checker{
		info: &Info{
			File:           file,
			Fns:            map[string]*Fn{},
			Consts:         map[string]*Const{},
			Structs:        map[string]*types.Struct{},
			Enums:          map[string]*types.Enum{},
			Methods:        map[string]map[string]*Fn{},
			Resolved:       map[ast.Expr]*Fn{},
			FnValues:       map[ast.Expr]*Fn{},
			Indirect:       map[ast.Expr]*types.Type{},
			Ctors:          map[ast.Expr]*Ctor{},
			TupleCtors:     map[ast.Expr]*types.Struct{},
			Aliases:        map[string]*types.Type{},
			PatVariant:     map[ast.Pattern]*types.Variant{},
			TryMatches:     map[*ast.TryExpr]*ast.MatchExpr{},
			CBody:          map[*ast.FnDecl]*ast.CBodyExpr{},
			PreludeMatches: map[*ast.MethodCallExpr]*ast.MatchExpr{},
			PreludeTag:     map[*ast.MethodCallExpr]string{},
			Closures:       map[*ast.LambdaExpr]*Closure{},
			Types:          map[ast.Expr]*types.Type{},
			DeclTypes:      map[ast.Stmt]*types.Type{},
		},
		aliasDecls:  map[string]*ast.TypeAlias{},
		aliasTypes:  map[string]*types.Type{},
		aliasBusy:   map[string]bool{},
		structDecls: map[string]*ast.StructDecl{},
		enumDecls:   map[string]*ast.EnumDecl{},
		instBusy:    map[string]bool{},
	}
	c.registerPrelude()
	c.collect()
	c.checkBodies()
	c.checkOwnership()
	c.info.Main = c.info.Fns["main"]
	switch {
	case c.info.Main == nil:
		c.errf(token.Span{}, "E0601", "no `main` function found")
	case len(c.info.Main.Params) != 0:
		c.errf(c.info.Main.Decl.Sp, "E0601", "`main` must take no parameters, found %s", c.info.Main.Sig())
	case !c.info.Main.Ret.Kind.IsInt() && c.info.Main.Ret.Kind != types.Unit:
		c.errf(c.info.Main.Decl.Sp, "E0601", "`main` must return an integer or `()`, found %s", c.info.Main.Ret)
	}
	return c.info, dedupeDiags(c.errs)
}

// dedupeDiags drops exact duplicates. Instantiation re-checks a body once per
// concrete type, so a mistake can otherwise be reported several times.
func dedupeDiags(ds []diag.Diagnostic) []diag.Diagnostic {
	if len(ds) < 2 {
		return ds
	}
	seen := make(map[string]bool, len(ds))
	out := ds[:0]
	for _, d := range ds {
		k := fmt.Sprintf("%d:%d:%s:%s", d.Span.Start, d.Span.End, d.Code, d.Message)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, d)
	}
	return out
}

type checker struct {
	info *Info
	errs []diag.Diagnostic

	aliasDecls map[string]*ast.TypeAlias
	aliasTypes map[string]*types.Type
	aliasBusy  map[string]bool

	// structDecls/enumDecls keep the declaration AST for generic types so an
	// instantiation can clone it and re-resolve its fields with concrete
	// arguments (the same clone-and-recheck strategy as generic functions).
	structDecls map[string]*ast.StructDecl
	enumDecls   map[string]*ast.EnumDecl
	// instBusy guards against infinitely recursive generic instantiation.
	instBusy map[string]bool
	// cbodyOK is the one `c` body the function being checked may contain.
	cbodyOK ast.Expr
	// indirect counts enclosing references/pointers while resolving a type;
	// recursive instantiations behind one are finite and allowed.
	indirect int

	// pending records integer literals that defaulted to `i32` without an
	// expected type. The key is the fresh type instance stored in the binding
	// and in info.Types for the literal, so a later use can adopt a concrete
	// integer type in place (see adoptInt).
	pending map[*types.Type]*pendingInt

	scope     []map[string]*types.Type // local bindings, innermost last
	mutScope  []map[string]bool        // mutability of tracked bindings (else permissive)
	typeEnv   map[string]*types.Type   // in-scope type parameters (name -> type)
	cur       *Fn
	loopDepth int
	// lambdas is the stack of closures being checked, innermost last. Each
	// frame remembers the scope depth it opened at, so resolving a name below
	// that depth is a capture.
	lambdas []*lambdaFrame
	breaks  []*breakInfo // one entry per enclosing loop, innermost last
}

// lambdaFrame is one closure being checked.
type lambdaFrame struct {
	node     *ast.LambdaExpr
	base     int // scope depth of the enclosing function at the `|`
	captures []Capture
}

// pendingInt is a defaulted integer literal awaiting a concrete integer type.
type pendingInt struct{ text string }

// breakInfo tracks the `break` value of one enclosing loop. Only `loop` may
// carry one; `hasVal`/`val` record the first `break <expr>` seen so later
// breaks can be checked against it, and `hasBare` records a plain `break`.
type breakInfo struct {
	kind    token.Kind
	want    *types.Type // the type expected of the loop's value (may be nil)
	val     *types.Type
	hasVal  bool
	hasBare bool
}

func (c *checker) errf(sp token.Span, code, format string, args ...any) {
	d := diag.New(code, format, args...)
	// A message that mentions the invalid type is a cascade: some earlier
	// error (an unknown name, an unresolved type) already reported the root
	// cause and left `<invalid>` behind. Drop the follow-on so one mistake
	// yields one diagnostic, as rustc does for its error types.
	if strings.Contains(d.Message, "<invalid>") {
		return
	}
	c.errs = append(c.errs, d.WithSpan(sp))
}

func (c *checker) errAt(e ast.Expr, code, format string, args ...any) {
	c.errf(e.Span(), code, format, args...)
}

// checkGenerics validates a type-parameter list and returns its names.
// Duplicate names are rejected; written bounds are not supported yet.
func (c *checker) checkGenerics(gps []ast.GenericParam) []string {
	if len(gps) == 0 {
		return nil
	}
	var names []string
	seen := map[string]bool{}
	for _, g := range gps {
		if len(g.Bounds) > 0 {
			c.errf(g.Sp, "E0900", "trait bounds are not supported yet (roadmap M1)")
		}
		if seen[g.Name] {
			c.errf(g.Sp, "E0428", "the type parameter `%s` is declared more than once", g.Name)
			continue
		}
		seen[g.Name] = true
		names = append(names, g.Name)
	}
	return names
}

// withTypeParams runs fn with the named type parameters in scope, each bound to
// an opaque parameter type. It restores the previous environment afterwards.
func (c *checker) withTypeParams(names []string, fn func()) {
	saved := c.typeEnv
	if len(names) > 0 {
		env := make(map[string]*types.Type, len(names))
		for _, n := range names {
			env[n] = &types.Type{Kind: types.ParamKind, ParamName: n}
		}
		c.typeEnv = env
	}
	fn()
	c.typeEnv = saved
}

// --- collection pass ------------------------------------------------------

func (c *checker) collect() {
	// Pass 1: register struct, enum, and type-alias names so fields can refer
	// to one another.
	for _, it := range c.info.File.Items {
		switch d := it.(type) {
		case *ast.StructDecl:
			c.registerStruct(d)
		case *ast.EnumDecl:
			c.registerEnum(d)
		case *ast.TypeAlias:
			c.registerAlias(d)
		}
	}
	// Pass 2: constants, so their values are available to array lengths and
	// other constant positions in the declarations below. A constant may
	// refer to one declared later in the file, so collect to a fixpoint and
	// only then report the ones that never became constant.
	var pending []*ast.ConstDecl
	for _, it := range c.info.File.Items {
		if d, ok := it.(*ast.ConstDecl); ok {
			pending = append(pending, d)
		}
	}
	for len(pending) > 0 {
		progress := false
		var rest []*ast.ConstDecl
		for _, d := range pending {
			if c.tryCollectConst(d) {
				progress = true
			} else {
				rest = append(rest, d)
			}
		}
		if !progress {
			for _, d := range rest {
				c.errf(d.Val.Span(), "E0602", "constant `%s` is not a compile-time constant expression", d.Name)
			}
			break
		}
		pending = rest
	}
	// Pass 3: fill in struct fields and enum variants.
	for _, it := range c.info.File.Items {
		switch d := it.(type) {
		case *ast.StructDecl:
			c.collectStructFields(d)
		case *ast.EnumDecl:
			c.collectEnumVariants(d)
		}
	}
	// Functions, then impl blocks (methods need their struct).
	for _, it := range c.info.File.Items {
		switch d := it.(type) {
		case *ast.FnDecl:
			c.collectFn(d, false, "")
		case *ast.ImplBlock:
			c.collectImpl(d)
		case *ast.ModDecl:
			c.errf(d.Sp, "E0900", "`mod` is not supported by the compiler yet (roadmap M1)")
		case *ast.UseDecl:
			c.errf(d.Sp, "E0900", "`use` is not supported by the compiler yet (roadmap M1)")
		}
	}
	// Resolve every alias eagerly, so unknown or cyclic aliases are reported
	// even when nothing uses them.
	for _, it := range c.info.File.Items {
		if d, ok := it.(*ast.TypeAlias); ok {
			c.resolveAlias(d.Name, d.Sp)
		}
	}
	// Finally, reject types that contain themselves by value: they have
	// infinite size and cannot be emitted as a C struct.
	c.checkTypeCycles()
}

// registerAlias records a `type Name = Ty;` declaration. Aliases share the
// type namespace, so the name must not collide with a struct, enum, builtin,
// or another alias.
func (c *checker) registerAlias(d *ast.TypeAlias) {
	if _, dup := c.aliasDecls[d.Name]; dup {
		c.errf(d.Sp, "E0428", "the type alias `%s` is defined multiple times", d.Name)
		return
	}
	if _, dup := c.info.Structs[d.Name]; dup {
		c.errf(d.Sp, "E0428", "the type `%s` is defined multiple times", d.Name)
		return
	}
	if _, dup := c.info.Enums[d.Name]; dup {
		c.errf(d.Sp, "E0428", "the type `%s` is defined multiple times", d.Name)
		return
	}
	if types.IsBuiltinTypeName(d.Name) {
		c.errf(d.Sp, "E0428", "the type `%s` is defined multiple times", d.Name)
		return
	}
	c.aliasDecls[d.Name] = d
}

// resolveAlias resolves an alias name to the type it stands for, recursively
// (an alias may name another alias). Cyclic aliases are rejected.
func (c *checker) resolveAlias(name string, sp token.Span) *types.Type {
	if t, ok := c.aliasTypes[name]; ok {
		return t
	}
	d, ok := c.aliasDecls[name]
	if !ok {
		return nil
	}
	if c.aliasBusy[name] {
		c.errf(sp, "E0072", "type alias `%s` is cyclic", name)
		return &types.Type{Kind: types.Invalid}
	}
	c.aliasBusy[name] = true
	t := c.resolveType(d.Ty)
	delete(c.aliasBusy, name)
	if t == nil {
		t = &types.Type{Kind: types.Invalid}
	}
	c.aliasTypes[name] = t
	c.info.Aliases[name] = t
	return t
}

// checkTypeCycles rejects structs and enums that reach themselves through
// by-value fields (including enum payloads and array elements). A reference or
// pointer breaks the cycle and is allowed.
func (c *checker) checkTypeCycles() {
	edges := map[string][]string{}
	addType := func(from string, t *types.Type) {
		for t != nil {
			switch t.Kind {
			case types.Ref, types.Ptr, types.Slice:
				return
			case types.Array:
				t = t.Elem
				continue
			case types.StructKind:
				if t.St != nil {
					edges[from] = append(edges[from], t.St.Name)
				}
				return
			case types.EnumKind:
				if t.En != nil {
					edges[from] = append(edges[from], t.En.Name)
				}
				return
			default:
				return
			}
		}
	}
	for _, st := range c.info.Structs {
		for _, f := range st.Fields {
			addType(st.Name, f.Ty)
		}
	}
	for _, en := range c.info.Enums {
		for _, v := range en.Variants {
			for _, f := range v.Fields {
				addType(en.Name, f.Ty)
			}
		}
	}
	// reachesSelf reports whether `start` is reachable from itself.
	reachesSelf := func(start string) bool {
		stack := append([]string{}, edges[start]...)
		seen := map[string]bool{}
		for len(stack) > 0 {
			n := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if n == start {
				return true
			}
			if seen[n] {
				continue
			}
			seen[n] = true
			stack = append(stack, edges[n]...)
		}
		return false
	}
	for _, it := range c.info.File.Items {
		switch d := it.(type) {
		case *ast.StructDecl:
			if reachesSelf(d.Name) {
				c.errf(d.Sp, "E0072", "recursive type `%s` has infinite size; store it behind a pointer or reference", d.Name)
			}
		case *ast.EnumDecl:
			if reachesSelf(d.Name) {
				c.errf(d.Sp, "E0072", "recursive type `%s` has infinite size; store it behind a pointer or reference", d.Name)
			}
		}
	}
}

func (c *checker) registerEnum(d *ast.EnumDecl) {
	if _, dup := c.info.Enums[d.Name]; dup {
		c.errf(d.Sp, "E0428", "the enum `%s` is defined multiple times", d.Name)
		return
	}
	if _, dup := c.aliasDecls[d.Name]; dup {
		c.errf(d.Sp, "E0428", "the type `%s` is defined multiple times", d.Name)
		return
	}
	if types.IsBuiltinTypeName(d.Name) && d.Name != "Option" && d.Name != "Result" {
		c.errf(d.Sp, "E0428", "the name `%s` is reserved for the predeclared type", d.Name)
		return
	}
	names := c.checkGenerics(d.Generics)
	c.checkTypeAttrs(d.Attrs, d.Sp, "enum", d.Name)
	en := &types.Enum{Name: d.Name, Base: d.Name, Generics: names, Generic: len(names) > 0, Copy: types.CopyAttr(d.Attrs)}
	c.info.Enums[d.Name] = en
	c.enumDecls[d.Name] = d
	if !en.Generic {
		c.info.EnumOrd = append(c.info.EnumOrd, en)
	}
}

func (c *checker) collectEnumVariants(d *ast.EnumDecl) {
	en, ok := c.info.Enums[d.Name]
	if !ok || en.Generic || len(en.Variants) > 0 {
		return // duplicate, template, or already filled
	}
	c.collectEnumVariantsInner(d, en)
}

func (c *checker) collectEnumVariantsInner(d *ast.EnumDecl, en *types.Enum) {
	for i, v := range d.Variants {
		if _, dup := en.Variant(v.Name); dup {
			c.errf(v.Sp, "E0428", "the variant `%s` is defined multiple times", v.Name)
			continue
		}
		tv := types.Variant{Name: v.Name, Tuple: v.Tup, Index: i}
		for j, f := range v.Fields {
			name := f.Name
			if v.Tup {
				name = strconv.Itoa(j)
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

// registerPrelude declares the generic enums that are always in scope, exactly
// like ordinary generic enums (they are instantiated on demand).
func (c *checker) registerPrelude() {
	c.addPreludeEnum("Option", []string{"T"}, []preludeVariant{
		{name: "Some", fields: []string{"T"}},
		{name: "None"},
	})
	c.addPreludeEnum("Result", []string{"T", "E"}, []preludeVariant{
		{name: "Ok", fields: []string{"T"}},
		{name: "Err", fields: []string{"E"}},
	})
}

type preludeVariant struct {
	name   string
	fields []string
}

func (c *checker) addPreludeEnum(name string, params []string, variants []preludeVariant) {
	d := &ast.EnumDecl{Name: name}
	for _, p := range params {
		d.Generics = append(d.Generics, ast.GenericParam{Name: p})
	}
	for _, v := range variants {
		av := ast.Variant{Name: v.name, Tup: len(v.fields) > 0}
		for _, f := range v.fields {
			av.Fields = append(av.Fields, ast.Field{Ty: &ast.PathType{Segments: []string{f}}})
		}
		d.Variants = append(d.Variants, av)
	}
	c.info.Enums[name] = &types.Enum{Name: name, Base: name, Generics: params, Generic: true}
	c.enumDecls[name] = d
}

// checkTypeAttrs validates the attributes on a struct or enum. `#[copy]` opts
// into copy semantics and `#[repr(C)]` names the layout the backend already
// uses; anything else is a typo the user should hear about.
func (c *checker) checkTypeAttrs(attrs []string, sp token.Span, what, name string) {
	for _, a := range attrs {
		nm := types.AttrName(a)
		switch nm {
		case "copy", "allow":
		case "repr":
			if arg := types.AttrArg(a); arg != "" && arg != "C" && arg != "c" {
				c.errf(sp, "E0539", "`#[repr(%s)]` is not supported; the only layout is `#[repr(C)]`, which is already the default", arg)
			}
		default:
			if !types.KnownAttrs[nm] {
				c.errf(sp, "E0539", "unknown attribute `#[%s]` on %s `%s`", nm, what, name)
			}
		}
	}
}

func (c *checker) registerStruct(d *ast.StructDecl) {
	if _, dup := c.info.Structs[d.Name]; dup {
		c.errf(d.Sp, "E0428", "the struct `%s` is defined multiple times", d.Name)
		return
	}
	if types.IsBuiltinTypeName(d.Name) {
		c.errf(d.Sp, "E0428", "the name `%s` is reserved for the predeclared type", d.Name)
		return
	}
	if _, dup := c.aliasDecls[d.Name]; dup {
		c.errf(d.Sp, "E0428", "the type `%s` is defined multiple times", d.Name)
		return
	}
	names := c.checkGenerics(d.Generics)
	c.checkTypeAttrs(d.Attrs, d.Sp, "struct", d.Name)
	st := &types.Struct{Name: d.Name, Base: d.Name, Generics: names, Generic: len(names) > 0, Tuple: d.Tuple, Copy: types.CopyAttr(d.Attrs)}
	c.info.Structs[d.Name] = st
	c.structDecls[d.Name] = d
	if !st.Generic {
		c.info.StructOrd = append(c.info.StructOrd, st)
	}
}

// collectStructFields fills a non-generic struct's fields. A generic template's
// fields are resolved per instantiation instead (see instantiateStruct).
func (c *checker) collectStructFields(d *ast.StructDecl) {
	st, ok := c.info.Structs[d.Name]
	if !ok || st.Generic || len(st.Fields) > 0 {
		return // duplicate, template, or already filled
	}
	c.collectStructFieldsInner(d, st)
}

func (c *checker) collectStructFieldsInner(d *ast.StructDecl, st *types.Struct) {
	for i, f := range d.Fields {
		name := f.Name
		if d.Tuple {
			name = strconv.Itoa(i)
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
	baseSt, hasStruct := c.info.Structs[name]
	baseEn, hasEnum := c.info.Enums[name]
	if !hasStruct && !hasEnum {
		c.errf(d.Sp, "E0118", "no type named `%s` in this scope", name)
		return
	}
	generic := (hasStruct && baseSt.Generic) || (hasEnum && baseEn.Generic)
	params := c.checkGenerics(d.Generics)
	pt, isPath := d.SelfTy.(*ast.PathType)

	// `impl Pair<T> { .. }` may introduce its parameters implicitly through the
	// self type. Accept that spelling when the arguments are unknown names.
	if generic && len(params) == 0 && isPath && len(pt.Args) == len(typeGenerics(baseSt, baseEn)) {
		var implicit []string
		ok := true
		for _, a := range pt.Args {
			at, isArgPath := a.(*ast.PathType)
			if !isArgPath || len(at.Segments) != 1 || len(at.Args) != 0 || isKnownTypeName(c, at.Segments[0]) {
				ok = false
				break
			}
			implicit = append(implicit, at.Segments[0])
		}
		if ok {
			params = implicit
		}
	}

	if len(params) == 0 {
		if generic {
			c.errf(d.Sp, "E0107", "`%s` expects %d type argument(s); write `impl<T, ..> %s<T, ..>`", name, len(typeGenerics(baseSt, baseEn)), name)
			return
		}
		if isPath && len(pt.Args) > 0 {
			c.errf(d.Sp, "E0107", "`%s` is not generic", name)
			return
		}
		recvT := c.nominalType(name)
		if c.info.Methods[name] == nil {
			c.info.Methods[name] = map[string]*Fn{}
		}
		for _, m := range d.Fns {
			fn := c.makeFn(m, recvT)
			if fn == nil {
				continue
			}
			if _, dup := c.info.Methods[name][m.Name]; dup {
				c.errf(m.Sp, "E0592", "duplicate method `%s` on `%s`", m.Name, name)
				continue
			}
			c.info.Methods[name][m.Name] = fn
			if !fn.Generic {
				c.info.FnOrder = append(c.info.FnOrder, fn)
			}
		}
		return
	}

	// Generic impl: require the receiver to apply the impl's parameters in
	// declaration order (`impl<T, U> Pair<T, U>`), which keeps the mapping from
	// a concrete receiver's arguments back to the template positional.
	if !generic {
		c.errf(d.Sp, "E0107", "`%s` is not generic", name)
		return
	}
	want := typeGenerics(baseSt, baseEn)
	if !isPath || len(pt.Segments) != 1 || len(pt.Args) != len(params) || len(params) != len(want) {
		c.errf(d.Sp, "E0900", "a generic impl must be written `impl<T, ..> %s<T, ..>` with one parameter per type argument", name)
		return
	}
	for i, a := range pt.Args {
		at, isArgPath := a.(*ast.PathType)
		if !isArgPath || len(at.Segments) != 1 || len(at.Args) != 0 || at.Segments[0] != params[i] {
			c.errf(d.Sp, "E0900", "a generic impl must apply its type parameters in declaration order")
			return
		}
	}
	recvT := c.nominalType(name)
	if c.info.Methods[name] == nil {
		c.info.Methods[name] = map[string]*Fn{}
	}
	for _, m := range d.Fns {
		if len(m.Generics) > 0 {
			c.errf(m.Sp, "E0900", "type parameters on methods are not supported yet (roadmap M1)")
			continue
		}
		var fn *Fn
		c.withTypeParams(params, func() {
			fn = c.makeFnInner(m, recvT)
		})
		fn.Generics = params
		fn.Generic = true
		if _, dup := c.info.Methods[name][m.Name]; dup {
			c.errf(m.Sp, "E0592", "duplicate method `%s` on `%s`", m.Name, name)
			continue
		}
		c.info.Methods[name][m.Name] = fn
		// A method template is not emitted; instantiations go to FnOrder.
	}
}

// typeGenerics returns the declared type-parameter names of a nominal template.
func typeGenerics(st *types.Struct, en *types.Enum) []string {
	if st != nil {
		return st.Generics
	}
	if en != nil {
		return en.Generics
	}
	return nil
}

func isKnownTypeName(c *checker, name string) bool {
	if _, ok := types.Builtins[name]; ok {
		return true
	}
	if _, ok := c.info.Structs[name]; ok {
		return true
	}
	if _, ok := c.info.Enums[name]; ok {
		return true
	}
	_, ok := c.aliasDecls[name]
	return ok
}

// instantiateMethod specializes a generic-impl method for a concrete receiver.
func (c *checker) instantiateMethod(tmpl *Fn, recv *types.Type) *Fn {
	key := typeKey(recv) + "::" + tmpl.Name
	if fn, ok := c.info.Fns[key]; ok {
		return fn
	}
	clone := ast.Clone(tmpl.Decl)
	clone.Generics = nil
	bind := paramBindings(tmpl.Generics, nominalArgs(recv))
	var fn *Fn
	c.withBindings(bind, func() {
		fn = c.makeFnInner(clone, recv)
	})
	fn.Name = tmpl.Name
	fn.QualName = nominalName(recv) + "::" + tmpl.Name
	fn.Generics = nil
	fn.Generic = false
	c.info.Fns[key] = fn
	c.info.FnOrder = append(c.info.FnOrder, fn)
	return fn
}

// instantiateAssociatedMethod specializes a generic-impl associated function
// (one without a `self` receiver). Because there is no receiver value to
// inspect, its arguments are inferred from the turbofish and the expected type.
func (c *checker) instantiateAssociatedMethod(tmpl *Fn, p *ast.PathExpr, expected *types.Type) *Fn {
	base := p.Segments[0]
	bindings := map[string]*types.Type{}
	if len(p.TypeArgs) > len(tmpl.Generics) {
		c.errf(p.Sp, "E0107", "`%s` takes %d type argument(s) but %d were supplied", base, len(tmpl.Generics), len(p.TypeArgs))
	}
	for i, name := range tmpl.Generics {
		if i < len(p.TypeArgs) {
			if t := c.resolveType(p.TypeArgs[i]); t != nil {
				bindings[name] = t
			}
		}
	}
	if expected != nil {
		switch expected.Kind {
		case types.StructKind:
			if expected.St != nil && nominalBaseName(expected) == base {
				for i, name := range tmpl.Generics {
					if _, ok := bindings[name]; !ok && i < len(expected.St.Args) {
						bindConcrete(bindings, name, expected.St.Args[i])
					}
				}
			}
		case types.EnumKind:
			if expected.En != nil && nominalBaseName(expected) == base {
				for i, name := range tmpl.Generics {
					if _, ok := bindings[name]; !ok && i < len(expected.En.Args) {
						bindConcrete(bindings, name, expected.En.Args[i])
					}
				}
			}
		}
	}
	args := c.bindGenericArgs(p.Sp, tmpl.Generics, bindings)
	if args == nil {
		return nil
	}
	var recv *types.Type
	if st, ok := c.info.Structs[base]; ok {
		recv = c.instantiateStruct(p.Sp, st, args)
	} else if en, ok := c.info.Enums[base]; ok {
		recv = c.instantiateEnum(p.Sp, en, args)
	} else {
		return nil
	}
	if recv == nil || recv.Kind == types.Invalid {
		return nil
	}
	return c.instantiateMethod(tmpl, recv)
}

// nominalType resolves a bare type name to a struct or enum type, or nil.
func (c *checker) nominalType(name string) *types.Type {
	if st, ok := c.info.Structs[name]; ok {
		return &types.Type{Kind: types.StructKind, St: st}
	}
	if en, ok := c.info.Enums[name]; ok {
		return &types.Type{Kind: types.EnumKind, En: en}
	}
	return nil
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
	if !fn.Generic {
		c.info.FnOrder = append(c.info.FnOrder, fn)
	}
}

// makeFn builds a checked function header, handling method receivers and
// generic type parameters.
func (c *checker) makeFn(d *ast.FnDecl, recv *types.Type) *Fn {
	names := c.checkGenerics(d.Generics)
	var fn *Fn
	c.withTypeParams(names, func() {
		fn = c.makeFnInner(d, recv)
	})
	fn.Generics = names
	fn.Generic = len(names) > 0
	return fn
}

// applyFnAttrs reads a function's `#[...]` attributes, recording the ones the
// backend acts on and rejecting the ones the compiler does not know, so a typo
// cannot silently do nothing.
func (c *checker) applyFnAttrs(fn *Fn, d *ast.FnDecl) {
	for _, a := range d.Attrs {
		switch types.AttrName(a) {
		case "inline":
			if types.AttrArg(a) == "never" {
				fn.InlineNever = true
			} else {
				fn.Inline = true
			}
		case "no_mangle", "export":
			if recv := fn.Recv; recv != nil {
				c.errf(d.Sp, "E0599", "`#[%s]` is not allowed on a method; put it on a plain function", types.AttrName(a))
				continue
			}
			if len(d.Generics) > 0 {
				c.errf(d.Sp, "E0599", "`#[%s]` is not allowed on a generic function; C needs one concrete symbol", types.AttrName(a))
				continue
			}
			fn.NoMangle = true
		case "allow":
			// Accepted and ignored.
		default:
			if !types.KnownAttrs[types.AttrName(a)] {
				c.errf(d.Sp, "E0539", "unknown attribute `#[%s]`", types.AttrName(a))
			}
		}
	}
}

func (c *checker) makeFnInner(d *ast.FnDecl, recv *types.Type) *Fn {
	fn := &Fn{Decl: d, Name: d.Name, Recv: recv}
	c.applyFnAttrs(fn, d)
	if recv != nil {
		fn.QualName = nominalName(recv) + "::" + d.Name
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
			selfT := recv
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
	// Parameter names and default values, aligned with Params. A default
	// must be a constant expression (a literal, a `const`, or arithmetic
	// over those); it is materialized once, here, as a literal that call
	// sites can substitute for an omitted argument.
	seenDefault := false
	for _, p := range d.Params {
		if p.Self {
			// Match the loop above: an invalid `self` (a top-level function)
			// reported E0424 and appended no type, so it must not append a
			// name/default slot either — Params and ParamNames would skew.
			if recv == nil {
				continue
			}
			fn.ParamNames = append(fn.ParamNames, "")
			fn.Defaults = append(fn.Defaults, nil)
			continue
		}
		fn.ParamNames = append(fn.ParamNames, p.Name)
		fn.Defaults = append(fn.Defaults, nil)
		if p.Default == nil {
			if seenDefault {
				c.errf(p.Sp, "E0061", "parameter `%s` without a default value follows a parameter with one", p.Name)
			}
			continue
		}
		seenDefault = true
		pt := fn.Params[len(fn.ParamNames)-1]
		if lit, ok := p.Default.(*ast.LiteralExpr); ok {
			// A literal default stands as-is; check it against the
			// parameter type once, here, so a bad default is reported
			// even if the function is never called.
			c.checkExpr(lit, pt)
			fn.Defaults[len(fn.Defaults)-1] = lit
			continue
		}
		cv, ok := c.evalConst(p.Default)
		if !ok {
			c.errf(p.Default.Span(), "E0602", "default value for parameter `%s` is not a constant expression", p.Name)
			continue
		}
		lit := constLiteralFor(cv, pt, p.Default.Span())
		if lit == nil {
			c.errf(p.Default.Span(), "E0602", "default value for parameter `%s` cannot be used as `%s`", p.Name, pt)
			continue
		}
		fn.Defaults[len(fn.Defaults)-1] = lit
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

// nominalName returns the name of a struct or enum type, or "".
func nominalName(t *types.Type) string {
	switch {
	case t == nil:
		return ""
	case t.Kind == types.StructKind && t.St != nil:
		return t.St.Name
	case t.Kind == types.EnumKind && t.En != nil:
		return t.En.Name
	}
	return ""
}

// typePathName returns the single-segment name of a path type, or "<error>".
func typePathName(t ast.Type) string {
	if pt, ok := t.(*ast.PathType); ok && len(pt.Segments) > 0 {
		return pt.Segments[0]
	}
	return "<error>"
}

// tryCollectConst folds and registers one constant. It returns false only
// when the value is not (yet) a compile-time constant — for example because
// it refers to a constant declared later, which the fixpoint loop in collect
// retries. All other outcomes (success and hard errors) return true.
func (c *checker) tryCollectConst(d *ast.ConstDecl) bool {
	if _, dup := c.info.Consts[d.Name]; dup {
		c.errf(d.Sp, "E0428", "the constant `%s` is defined multiple times", d.Name)
		return true
	}
	var declared *types.Type
	if d.Ty != nil {
		declared = c.resolveType(d.Ty)
	}
	val, ok := c.evalConst(d.Val)
	if !ok {
		return false
	}
	if declared != nil {
		switch {
		case val.Untyped && declared.Kind.IsInt() && declared.Kind != types.Invalid:
			if !intValFits(val, declared) {
				c.errf(d.Val.Span(), "E0080", "constant `%s` does not fit in `%s`", d.Name, declared)
				return true
			}
			val.Ty = declared
		case !c.assignable(declared, val.Ty):
			c.errf(d.Val.Span(), "E0308", "mismatched types: expected `%s`, found `%s`", declared, val.Ty)
			return true
		default:
			val.Ty = declared
		}
	}
	c.info.Consts[d.Name] = &Const{Decl: d, Name: d.Name, Ty: val.Ty, Val: val}
	return true
}

// --- body checking --------------------------------------------------------

// cbodyOf returns the `c` body of a function body, when the block is nothing
// but that body. Anything else (a body with other statements, or a C block in
// the middle of an expression) is rejected where the block is checked.
func cbodyOf(b *ast.Block) *ast.CBodyExpr {
	if b == nil {
		return nil
	}
	if cb, ok := b.Tail.(*ast.CBodyExpr); ok && len(b.Stmts) == 0 {
		return cb
	}
	if len(b.Stmts) == 1 {
		if es, ok := b.Stmts[0].(*ast.ExprStmt); ok {
			if cb, ok := es.X.(*ast.CBodyExpr); ok {
				return cb
			}
		}
	}
	return nil
}

// checkCBody validates a function whose body is written in C and records it for
// codegen. The C body runs instead of generated code, so nothing would emit
// drop glue for it: parameters and the result must therefore be values that
// have no destructor to run (scalars, `str`, borrows, pointers, `#[copy]`
// structs).
func (c *checker) checkCBody(fn *Fn, cb *ast.CBodyExpr) {
	if fn.Name == "main" {
		c.errf(cb.Sp, "E0540", "`main` cannot have a `c` body: the runtime sets up the argument list there")
		return
	}
	for i, p := range fn.Decl.Params {
		pt := types.UnitT
		if i < len(fn.Params) {
			pt = fn.Params[i]
		}
		if !token.IsCIdentifier(p.Name) {
			c.errf(p.Sp, "E0540", "parameter `%s` cannot be used in a `c` body: it is not a C identifier (rename it)", p.Name)
			return
		}
		if pt.NeedsDrop() {
			c.errf(p.Sp, "E0540", "parameter `%s` has type `%s`, which needs its destructor run; a `c` body cannot drop it, so take `&%s` instead", p.Name, pt, pt)
			return
		}
	}
	if fn.Ret.NeedsDrop() {
		c.errf(cb.Sp, "E0540", "a `c` body cannot return `%s`: the value would never be dropped; return a scalar or a borrow", fn.Ret)
		return
	}
	c.info.CBody[fn.Decl] = cb
}

func (c *checker) checkBodies() {
	// A worklist: instantiating a generic call appends the concrete function to
	// FnOrder, and it must be checked in turn. `len` is re-evaluated on every
	// iteration on purpose, so the bound must not be hoisted (intrange would
	// range over a snapshot of the original length).
	for i := 0; i < len(c.info.FnOrder); i++ { //nolint:intrange // FnOrder grows during the loop
		fn := c.info.FnOrder[i]
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
			c.declare(p.Name, fn.Params[i])
		}
		if fn.Self != nil && fn.Self.ByRef && !fn.Self.Mut {
			// `&self` methods may not mutate the receiver.
			c.markMut("self", false)
		}
		var want *types.Type
		if fn.Ret.Kind != types.Unit {
			want = fn.Ret
		}
		c.cbodyOK = cbodyOf(fn.Decl.Body)
		bt := c.checkBlockWith(fn.Decl.Body, want)
		if cb := cbodyOf(fn.Decl.Body); cb != nil && c.info.Types[cb] != nil && c.info.Types[cb].Kind != types.Invalid {
			c.checkCBody(fn, cb)
		}
		c.cbodyOK = nil
		if fn.Ret.Kind != types.Unit {
			// An invalid body type has already been diagnosed; don't pile on.
			if !c.exprAlwaysReturns(fn.Decl.Body) && bt.Kind != types.Never && bt.Kind != types.Invalid {
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

func (c *checker) checkBlock(b *ast.Block) { c.checkBlockWith(b, nil) }

// checkBlockWith type-checks a block, giving its trailing expression `want` as
// the expected type. This is how a function body's tail adopts the declared
// return type (`fn f() -> i64 { 5 }`), and how the branches of a tail `if`/
// `match` learn it too.
func (c *checker) checkBlockWith(b *ast.Block, want *types.Type) *types.Type {
	c.pushScope()
	defer c.popScope()
	for _, s := range b.Stmts {
		c.checkStmt(s)
	}
	if b.Tail != nil {
		t := c.checkExpr(b.Tail, want)
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
		c.bindPattern(st.Pat, ty)
		if bp, ok := st.Pat.(*ast.BindPat); ok {
			c.markMut(bp.Name, st.Mut)
		}
	case *ast.DeferStmt:
		if exprHasJump(st.X) {
			c.errf(st.Sp, "E0268", "`return`, `break`, and `continue` cannot be used inside a `defer` expression")
		}
		c.checkExpr(st.X, nil)
	case *ast.ExprStmt:
		c.checkExpr(st.X, nil)
	default:
		c.errf(s.Span(), "E0900", "unsupported statement")
	}
}

// exprHasJump reports whether e contains a `return`, `break`, or `continue`
// anywhere inside it. Such jumps cannot be deferred: the deferred expression
// runs at scope exit, where the jump's target no longer exists.
func exprHasJump(e ast.Expr) bool {
	switch x := e.(type) {
	case nil:
		return false
	case *ast.JumpExpr:
		return true
	case *ast.Block:
		for _, s := range x.Stmts {
			switch st := s.(type) {
			case *ast.LetStmt:
				if exprHasJump(st.Val) {
					return true
				}
			case *ast.DeferStmt:
				if exprHasJump(st.X) {
					return true
				}
			case *ast.ExprStmt:
				if exprHasJump(st.X) {
					return true
				}
			}
		}
		return exprHasJump(x.Tail)
	case *ast.BinExpr:
		return exprHasJump(x.L) || exprHasJump(x.R)
	case *ast.UnExpr:
		return exprHasJump(x.X)
	case *ast.CastExpr:
		return exprHasJump(x.X)
	case *ast.TryExpr:
		return exprHasJump(x.X)
	case *ast.AssignExpr:
		return exprHasJump(x.Lhs) || exprHasJump(x.Rhs)
	case *ast.FieldExpr:
		return exprHasJump(x.Recv)
	case *ast.IndexExpr:
		return exprHasJump(x.Recv) || exprHasJump(x.Index)
	case *ast.SliceExpr:
		return exprHasJump(x.Recv) || exprHasJump(x.Low) || exprHasJump(x.High)
	case *ast.CallExpr:
		if exprHasJump(x.Callee) {
			return true
		}
		if slices.ContainsFunc(x.Args, exprHasJump) {
			return true
		}
	case *ast.MethodCallExpr:
		if exprHasJump(x.Recv) {
			return true
		}
		if slices.ContainsFunc(x.Args, exprHasJump) {
			return true
		}
	case *ast.StructLitExpr:
		for _, f := range x.Fields {
			if exprHasJump(f.Val) {
				return true
			}
		}
	case *ast.ArrayLitExpr:
		if exprHasJump(x.Value) || exprHasJump(x.Count) {
			return true
		}
		if slices.ContainsFunc(x.Elems, exprHasJump) {
			return true
		}
	case *ast.TupleExpr:
		if slices.ContainsFunc(x.Elems, exprHasJump) {
			return true
		}
	case *ast.IfExpr:
		return exprHasJump(x.Cond) || exprHasJump(x.Then) || exprHasJump(x.Else)
	case *ast.LoopExpr:
		return exprHasJump(x.Cond) || exprHasJump(x.Iter) || exprHasJump(x.Body)
	case *ast.MatchExpr:
		if exprHasJump(x.Scrutinee) {
			return true
		}
		for _, a := range x.Arms {
			if exprHasJump(a.Guard) || exprHasJump(a.Body) {
				return true
			}
		}
	}
	return false
}

func (c *checker) bindPattern(p ast.Pattern, ty *types.Type) {
	switch pt := p.(type) {
	case *ast.BindPat:
		if pt.Name == "" {
			return
		}
		if pt.Name != "_" {
			c.declare(pt.Name, ty)
		}
	case *ast.WildcardPat:
	case *ast.TuplePat:
		c.errf(pt.Sp, "E0900", "tuple patterns are not supported by the compiler yet")
	default:
		c.errf(p.Span(), "E0900", "this pattern is not supported by the compiler yet")
	}
}

// --- expression checking --------------------------------------------------

// inferCBody accepts `c r"...\"` only where a C body is allowed — as the whole
// body of the function being checked — and gives it type `never`, since a C
// `return` leaves the function rather than producing a value here.
func (c *checker) inferCBody(x *ast.CBodyExpr) *types.Type {
	if c.cbodyOK != x {
		c.errf(x.Sp, "E0540", "a `c` body must be the entire body of a function, not part of an expression")
		return &types.Type{Kind: types.Invalid}
	}
	c.info.Types[x] = types.NeverT
	return types.NeverT
}

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

	case *ast.CBodyExpr:
		return c.inferCBody(x)

	case *ast.IdentExpr:
		return c.inferIdentExpr(x, expected)

	case *ast.PathExpr:
		if len(x.Segments) == 1 {
			if t := c.lookupScope(x.Segments[0]); t != nil {
				c.noteCapture(x.Segments[0], t)
				c.adoptInt(t, expected)
			}
			return c.inferIdent(x.Segments[0], x.Sp)
		}
		if len(x.Segments) == 2 {
			en, ok := c.info.Enums[x.Segments[0]]
			if !ok {
				c.errf(x.Sp, "E0412", "cannot find type `%s` in this scope", x.Segments[0])
				return nil
			}
			if en.Generic {
				args, ok := c.inferEnumArgsFrom(x.Sp, en, x.TypeArgs, expected)
				if !ok {
					return &types.Type{Kind: types.Invalid}
				}
				t := c.instantiateEnum(x.Sp, en, args)
				if t.Kind != types.EnumKind || t.En == nil {
					return &types.Type{Kind: types.Invalid}
				}
				en = t.En
			} else if len(x.TypeArgs) > 0 {
				c.errf(x.Sp, "E0107", "`%s` is not generic", en.Base)
			}
			v, ok := en.Variant(x.Segments[1])
			if !ok {
				c.errf(x.Sp, "E0599", "no variant `%s` on enum `%s`", x.Segments[1], en.Name)
				return nil
			}
			if len(v.Fields) > 0 {
				c.errf(x.Sp, "E0061", "variant `%s::%s` requires %d argument(s)", en.Base, v.Name, len(v.Fields))
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
		return c.checkBlockWith(x, expected)

	case *ast.LambdaExpr:
		return c.inferLambda(x, expected)

	case *ast.UnExpr:
		return c.inferUnary(x, expected)

	case *ast.BinExpr:
		return c.inferBinary(x, expected)

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
		return c.inferCall(x, expected)

	case *ast.IfExpr:
		return c.inferIf(x, expected)

	case *ast.LoopExpr:
		return c.inferLoop(x, expected)

	case *ast.JumpExpr:
		return c.inferJump(x)

	case *ast.MatchExpr:
		return c.inferMatch(x, expected)
	case *ast.FieldExpr:
		return c.inferField(x)
	case *ast.MethodCallExpr:
		return c.inferMethodCall(x)
	case *ast.InterpStrExpr:
		// `f"...{expr}..."`: literal chunks are plain strings; every
		// interpolated expression must have a printable type. The result
		// is an owning `String`.
		for _, part := range x.Parts {
			at := c.checkExpr(part, nil)
			if _, isLit := part.(*ast.LiteralExpr); isLit {
				continue
			}
			if at != nil && at.Kind != types.Invalid && !isPrintable(at) {
				c.errAt(part, "E0277", "value of type `%s` cannot be interpolated into a string", at)
			}
		}
		return types.StringT
	case *ast.NamedArgExpr:
		// Named arguments are rewritten by resolveArgs before their
		// values are checked; reaching here means the call target does
		// not accept them (a builtin, constructor, or variant).
		c.errAt(x, "E0061", "named arguments are only supported for function and method calls")
		return c.checkExpr(x.Val, nil)
	case *ast.StructLitExpr:
		return c.inferStructLit(x, expected)
	case *ast.IndexExpr:
		return c.inferIndex(x)
	case *ast.SliceExpr:
		return c.inferSlice(x)
	case *ast.ArrayLitExpr:
		return c.inferArrayLit(x, expected)
	case *ast.TryExpr:
		return c.inferTry(x, expected)
	}
	c.errf(e.Span(), "E0900", "unsupported expression")
	return nil
}

// checkEscapes reports an unknown or malformed escape in a string or char
// literal. The text is decoded later (in codegen), where an undecodable escape
// would otherwise be silently passed through verbatim.
func (c *checker) checkEscapes(x *ast.LiteralExpr) {
	if _, ok := DecodeString(x.Text); !ok {
		c.errf(x.Sp, "E0001", "unknown escape sequence in literal: %s", x.Text)
	}
}

func (c *checker) inferLiteral(x *ast.LiteralExpr, expected *types.Type) *types.Type {
	switch x.Kind {
	case token.KindInteger:
		if expected != nil && expected.Kind.IsFloat() {
			// An integer literal may initialize a float directly, so
			// `let x: f64 = 1` and `f(2)` for an `f64` parameter both work.
			// (This is literal typing, not a conversion: the literal is untyped
			// until its context fixes it.)
			s := strings.ReplaceAll(x.Text, "_", "")
			if _, err := strconv.ParseInt(s, 0, 64); err != nil {
				if _, err := strconv.ParseUint(s, 0, 64); err != nil {
					c.errf(x.Sp, "E0080", "integer literal `%s` does not fit in any integer type", x.Text)
				}
			}
			return expected
		}
		if expected != nil && expected.Kind.IsInt() {
			if !intLitFits(x.Text, expected) {
				c.errf(x.Sp, "E0080", "integer literal `%s` does not fit in `%s`", x.Text, expected)
			}
			return expected
		}
		// Un-annotated: prefer i32, widening to i64/u64 when the value is bigger
		// rather than silently truncating.
		if !intLitFits(x.Text, types.I32T) {
			s := strings.ReplaceAll(x.Text, "_", "")
			if _, err := strconv.ParseInt(s, 0, 64); err == nil {
				return types.I64T
			}
			if _, err := strconv.ParseUint(s, 0, 64); err == nil {
				return &types.Type{Kind: types.U64}
			}
			c.errf(x.Sp, "E0080", "integer literal `%s` does not fit in any integer type", x.Text)
		}
		// A fresh instance, never the shared types.I32T: a binding may adopt a
		// concrete integer type later without changing every other i32.
		t := &types.Type{Kind: types.I32}
		if c.pending == nil {
			c.pending = map[*types.Type]*pendingInt{}
		}
		c.pending[t] = &pendingInt{text: x.Text}
		return t
	case token.KindFloat:
		if expected != nil && expected.Kind.IsFloat() {
			return expected
		}
		return types.F64T
	case token.KindTrue, token.KindFalse:
		return types.BoolT
	case token.KindString:
		c.checkEscapes(x)
		return types.StrT
	case token.KindRawString:
		// Raw strings are not decoded, so escapes are literal text.
		return types.StrT
	case token.KindChar:
		if _, ok := decodeCharLiteral(x.Text); !ok {
			c.errf(x.Sp, "E0001", "char literal must contain exactly one character")
		}
		return types.CharT
	case token.KindByteChar:
		if r, ok := decodeCharLiteral(x.Text); !ok || r > 0xFF {
			c.errf(x.Sp, "E0001", "byte literal must contain exactly one byte")
		}
		return &types.Type{Kind: types.U8}
	}
	return nil
}

// editDistance is the Levenshtein distance between two names, for
// "did you mean" suggestions.
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 0
			if ra[i-1] != rb[j-1] {
				cost = 1
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}

func min3(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}

// suggestName returns " (did you mean `x`?)" when one of the candidates
// is within edit distance 2 of name, and "" otherwise. Candidates are
// sorted first so a tie between two equally-distant suggestions resolves
// to the same one every run (callers build the list by ranging over Go
// maps, whose iteration order is randomized).
func suggestName(name string, candidates []string) string {
	sorted := make([]string, len(candidates))
	copy(sorted, candidates)
	sort.Strings(sorted)
	best, bestD := "", 3
	for _, cand := range sorted {
		if cand == name {
			continue
		}
		if d := editDistance(name, cand); d < bestD {
			best, bestD = cand, d
		}
	}
	if best == "" {
		return ""
	}
	return fmt.Sprintf(" (did you mean `%s`?)", best)
}

// adoptInt retypes a binding that was defaulted from a bare integer literal
// when a use demands a concrete integer type. It mutates the type instance in
// place, so every use of the binding (including aliases created by
// `let y = x;`, which share the pointer) agrees on the adopted type.
//
// It returns nil when the binding is not pending, the demand is not an integer
// type, or the literal does not fit. In the last case the normal E0080/E0308
// path produces the diagnostic; adopting would hide the reason.
func (c *checker) adoptInt(t, want *types.Type) *types.Type {
	p, ok := c.pending[t]
	if !ok || want == nil || !want.Kind.IsInt() {
		return nil
	}
	if !intLitFits(p.text, want) {
		return nil
	}
	t.Kind = want.Kind
	delete(c.pending, t)
	return t
}

// inferIdentExpr types a bare name used as a value. A generic function is
// instantiated when explicit type arguments (`f::<i64>`) or an expected
// function type determine its parameters.
// inferLambda checks a closure against the type it is expected to have. The
// target type supplies any parameter or return type the closure leaves out,
// which is why a closure has to appear where its signature is known:
//
//	let f: fn(i32) -> i32 = |n| n * 2;
//	vec.each(|x| println(x));            // the parameter type comes from `each`
func (c *checker) inferLambda(x *ast.LambdaExpr, expected *types.Type) *types.Type {
	if expected == nil || expected.Kind != types.Fn {
		c.errf(x.Sp, "E0282", "cannot infer the type of this closure; give it a target type, as in `let f: fn(i32) -> i32 = |n| n;`")
		return &types.Type{Kind: types.Invalid}
	}
	if len(expected.Params) != len(x.Params) {
		c.errf(x.Sp, "E0057", "this closure takes %d parameter(s) but `%s` expects %d", len(x.Params), expected.String(), len(expected.Params))
		return &types.Type{Kind: types.Invalid}
	}
	params := make([]*types.Type, len(expected.Params))
	copy(params, expected.Params)
	for i, p := range x.Params {
		if p.Ty == nil {
			continue
		}
		want := c.resolveType(p.Ty)
		if want == nil {
			return &types.Type{Kind: types.Invalid}
		}
		if !c.assignable(want, params[i]) {
			c.errf(p.Sp, "E0308", "closure parameter `%s` has type `%s`, but `%s` expects `%s`", p.Name, want.String(), expected.String(), params[i].String())
			return &types.Type{Kind: types.Invalid}
		}
	}

	cl := &Closure{Params: params, Ret: expected.Ret}
	frame := &lambdaFrame{node: x, base: len(c.scope)}

	// Parameters are bindings like any other, so the body resolves them
	// normally; `base` is above them, which keeps them out of the captures.
	c.pushScope()
	for i, p := range x.Params {
		c.declare(p.Name, params[i])
	}
	c.lambdas = append(c.lambdas, frame)
	body := c.checkExpr(x.Body, expected.Ret)
	c.lambdas = c.lambdas[:len(c.lambdas)-1]
	c.popScope()

	switch body.Kind {
	case types.Unit, types.Never, types.Invalid:
	default:
		if !c.assignable(body, expected.Ret) {
			c.errf(x.Body.Span(), "E0308", "this closure returns `%s`, but `%s` expects `%s`", body.String(), expected.String(), expected.Ret.String())
		}
	}

	cl.Captures = frame.captures
	c.info.Closures[x] = cl
	c.info.ClosureOrd = append(c.info.ClosureOrd, x)
	return expected
}

// noteCapture records that the innermost closure reads a binding from the
// enclosing function. Variables declared inside the closure (parameters and
// locals) are not captures.
func (c *checker) noteCapture(name string, ty *types.Type) {
	if len(c.lambdas) == 0 || ty == nil {
		return
	}
	depth := c.lookupScopeDepth(name)
	if depth < 0 {
		return
	}
	// The innermost closure reads the value and owns the move; each enclosing
	// closure only carries it so it can build the inner one.
	for i := len(c.lambdas) - 1; i >= 0; i-- {
		frame := c.lambdas[i]
		if depth >= frame.base {
			break // bound inside this closure, so not a capture of it or of any outer one
		}
		found := false
		for _, cap := range frame.captures {
			if cap.Name == name {
				found = true
				break
			}
		}
		if !found {
			frame.captures = append(frame.captures, Capture{Name: name, Ty: ty, Direct: i == len(c.lambdas)-1})
		}
	}
}

// lookupScopeDepth is lookupScope, but it also reports the scope the binding
// was found in (-1 when it is not a local).
func (c *checker) lookupScopeDepth(name string) int {
	for i := len(c.scope) - 1; i >= 0; i-- {
		if _, ok := c.scope[i][name]; ok {
			return i
		}
	}
	return -1
}

func (c *checker) inferIdentExpr(x *ast.IdentExpr, expected *types.Type) *types.Type {
	// A local binding or constant shadows a function of the same name.
	if t := c.lookupScope(x.Name); t != nil {
		c.noteCapture(x.Name, t)
		c.adoptInt(t, expected)
		return c.inferIdent(x.Name, x.Sp)
	}
	if _, ok := c.info.Consts[x.Name]; ok {
		return c.inferIdent(x.Name, x.Sp)
	}
	fn, ok := c.info.Fns[x.Name]
	if !ok {
		if t := c.inferBareUnitVariant(x, expected); t != nil {
			return t
		}
		return c.inferIdent(x.Name, x.Sp)
	}
	if !fn.Generic {
		if len(x.TypeArgs) > 0 {
			c.errf(x.Sp, "E0107", "`%s` is not generic", x.Name)
		}
		return c.inferIdent(x.Name, x.Sp)
	}
	bindings := map[string]*types.Type{}
	if len(x.TypeArgs) > len(fn.Generics) {
		c.errf(x.Sp, "E0107", "`%s` takes %d type argument(s) but %d were supplied", x.Name, len(fn.Generics), len(x.TypeArgs))
	}
	for i, name := range fn.Generics {
		if i < len(x.TypeArgs) {
			if t := c.resolveType(x.TypeArgs[i]); t != nil {
				bindings[name] = t
			}
		}
	}
	if expected != nil && expected.Kind == types.Fn {
		c.unify(&types.Type{Kind: types.Fn, Params: fn.Params, Ret: fn.Ret}, expected, bindings)
	}
	args := c.bindGenericArgs(x.Sp, fn.Generics, bindings)
	if args == nil {
		return &types.Type{Kind: types.Invalid}
	}
	inst := c.instantiateFn(fn, args)
	c.info.FnValues[x] = inst
	return &types.Type{Kind: types.Fn, Params: inst.Params, Ret: inst.Ret}
}

// inferIdent resolves a bare value name without generic instantiation.
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
	var cands []string
	for i := len(c.scope) - 1; i >= 0; i-- {
		for n := range c.scope[i] {
			cands = append(cands, n)
		}
	}
	for n := range c.info.Consts {
		cands = append(cands, n)
	}
	for n := range c.info.Fns {
		cands = append(cands, n)
	}
	c.errf(sp, "E0425", "cannot find `%s` in this scope%s", name, suggestName(name, cands))
	return nil
}

// inferNegIntLit types `-<integer literal>`. The sign and the digits are one
// constant, so the range check must see the negative value (otherwise
// `let x: i8 = -128` is rejected and `let x: u8 = -1` is accepted).
func (c *checker) inferNegIntLit(x *ast.UnExpr, lit *ast.LiteralExpr, expected *types.Type) *types.Type {
	text := "-" + lit.Text
	if expected != nil && expected.Kind.IsFloat() {
		// `-1` in a float context, e.g. `let x: f64 = -1`. Codegen emits the
		// literal, which C converts to the float type.
		c.info.Types[lit] = expected
		return expected
	}
	t := expected
	if t == nil || !t.Kind.IsInt() {
		if intLitFits(text, types.I32T) {
			t = types.I32T
		} else {
			t = types.I64T
		}
	}
	if !intLitFits(text, t) {
		c.errf(x.Sp, "E0080", "integer literal `%s` does not fit in `%s`", text, t)
	}
	c.info.Types[lit] = t
	return t
}

func (c *checker) inferUnary(x *ast.UnExpr, expected *types.Type) *types.Type {
	switch x.Op {
	case token.KindMinus:
		if lit, ok := x.X.(*ast.LiteralExpr); ok && lit.Kind == token.KindInteger {
			return c.inferNegIntLit(x, lit, expected)
		}
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
		if !c.isPlace(x.X) {
			c.errAt(x.X, "E0716", "cannot take a reference to a temporary value; bind it to a variable first")
		}
		if x.Mut {
			c.checkMutable(x.X, x.X.Span(), "cannot borrow `%s` as mutable, as it is not declared `mut`")
		}
		return types.RefTo(t, x.Mut)
	}
	return nil
}

// isStrLike reports whether t is one of the string types: the borrowed `str`
// or the owning `String`.
func isStrLike(t *types.Type) bool {
	return t != nil && (t.Kind == types.Str || t.Kind == types.StringKind)
}

// isScalarText reports whether t is a value that can be rendered as text and
// concatenated onto a string (`"count: " + n`).
func isScalarText(t *types.Type) bool {
	if t == nil {
		return false
	}
	return t.Kind.IsNumeric() || t.Kind == types.Bool || t.Kind == types.Char
}

// isComparison reports whether op is one of the six comparison operators.
func isComparison(op token.Kind) bool {
	switch op {
	case token.KindEqEq, token.KindBangEq, token.KindLt, token.KindLe, token.KindGt, token.KindGe:
		return true
	}
	return false
}

func (c *checker) inferBinary(x *ast.BinExpr, expected *types.Type) *types.Type {
	if x.Op == token.KindDotDot || x.Op == token.KindDotDotEq {
		lt := c.checkExpr(x.L, nil)
		rt := c.checkExpr(x.R, nil)
		if c.adoptInt(lt, rt) == nil {
			// Keep today's literal adoption and its error when the pending
			// bound cannot take the other side's type.
			c.checkExpr(x.R, lt)
		}
		if !lt.Kind.IsInt() {
			c.errAt(x, "E0308", "range bounds must be integers, found `%s`", lt)
		}
		return types.UnitT
	}

	// A tree of integer literals adopts a numeric type from context, so
	// `let x: i64 = 5 ^ 3` is `i64`, not the `i32` bare literals default to.
	var lt, rt *types.Type
	if expected != nil && expected.Kind.IsNumeric() && intLiteralExpr(x.L) && intLiteralExpr(x.R) {
		lt = c.checkExpr(x.L, expected)
		rt = c.checkExpr(x.R, expected)
	} else {
		lt = c.checkExpr(x.L, nil)
		rt = c.checkExpr(x.R, nil)
	}
	// String concatenation: `+` over any mix of `str`/`String` produces a
	// fresh `String`. Both operands are borrowed (their bytes are copied), so
	// they remain usable afterwards. A number, `bool`, or `char` on one side is
	// rendered as text, so `"count: " + n` reads naturally.
	if x.Op == token.KindPlus {
		switch {
		case isStrLike(lt) && (isStrLike(rt) || isScalarText(rt)),
			isStrLike(rt) && isScalarText(lt):
			return types.StringT
		}
	}
	// Comparison over any two string-like operands compares bytes, so `"a" < b`
	// works for every mix of `str` and `String`. Ordering follows byte value,
	// the same order Rust's Ord gives str.
	if isComparison(x.Op) && isStrLike(lt) && isStrLike(rt) {
		return types.BoolT
	}
	// Integer literals adopt the type of the other operand. A literal
	// expression (`-7`, `1 + 2`) adopts only from a side that is not itself
	// one, so `-9223372036854775807 - 1` keeps its natural `i64` and lets the
	// bare `1` follow it, while `-7 + x` follows `x`'s type.
	if !types.Equal(lt, rt) {
		switch {
		case isIntLiteral(x.L) && rt.Kind.IsNumeric():
			lt = c.checkExpr(x.L, rt)
		case isIntLiteral(x.R) && lt.Kind.IsNumeric():
			rt = c.checkExpr(x.R, lt)
		case intLiteralExpr(x.L) && !intLiteralExpr(x.R) && rt.Kind.IsNumeric():
			lt = c.checkExpr(x.L, rt)
		case intLiteralExpr(x.R) && !intLiteralExpr(x.L) && lt.Kind.IsNumeric():
			rt = c.checkExpr(x.R, lt)
		case c.adoptInt(lt, rt) != nil:
			// A variable defaulted from a bare literal adopts the other
			// operand's concrete integer type in place.
		case c.adoptInt(rt, lt) != nil:
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
	case token.KindPlus, token.KindMinus, token.KindStar, token.KindSlash:
		if !lt.Kind.IsNumeric() {
			c.errAt(x, "E0308", "arithmetic operators require numbers, found `%s`", lt)
		}
		return lt
	case token.KindPercent:
		if !lt.Kind.IsInt() {
			c.errAt(x, "E0308", "`%%` requires integers, found `%s`", lt)
		}
		return lt
	}
	return lt
}

func (c *checker) inferAssign(x *ast.AssignExpr) *types.Type {
	if !c.isPlace(x.Lhs) {
		c.errAt(x.Lhs, "E0384", "cannot assign to this expression")
	}
	c.checkMutable(x.Lhs, x.Lhs.Span(), "cannot assign to `%s`, as it is not declared `mut`")
	lt := c.checkExpr(x.Lhs, nil)
	rt := c.checkExpr(x.Rhs, lt)
	if x.Op == token.KindEq {
		if !c.assignable(lt, rt) {
			c.errAt(x.Rhs, "E0308", "mismatched types: expected `%s`, found `%s`", lt, rt)
		}
	} else {
		switch {
		case lt.Kind == types.StringKind && x.Op == token.KindPlusEq:
			// `s += t` appends to an owning `String`; a scalar `t` is formatted
			// as text first.
			if !isStrLike(rt) && !isScalarText(rt) {
				c.errAt(x.Rhs, "E0308", "cannot append `%s` to a `String`", rt)
			}
		case x.Op == token.KindPercentEq && !lt.Kind.IsInt():
			c.errAt(x, "E0308", "`%%=` requires integers, found `%s`", lt)
		case !lt.Kind.IsNumeric():
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
	case *ast.FieldExpr:
		return c.isPlace(x.Recv)
	case *ast.IndexExpr:
		return c.isPlace(x.Recv)
	case *ast.SliceExpr:
		return c.isPlace(x.Recv)
	}
	return false
}

// isCallableName reports whether the callee is a plain name (identifier or
// path), which the other branches of inferCall handle.
func isCallableName(e ast.Expr) bool {
	switch e.(type) {
	case *ast.IdentExpr, *ast.PathExpr:
		return true
	}
	return false
}

// exprOfFnType checks a callee expression that is not a name and returns its
// `fn(..)` type, or nil when it is not a function value.
func (c *checker) exprOfFnType(e ast.Expr) *types.Type {
	t := c.checkExpr(e, nil)
	if t != nil && t.Kind == types.Fn {
		return t
	}
	return nil
}

// checkIndirectCall type-checks a call through a computed function value.
func (c *checker) checkIndirectCall(x *ast.CallExpr, ft *types.Type) *types.Type {
	c.info.Indirect[x] = ft
	if len(x.Args) != len(ft.Params) {
		c.errAt(x, "E0061", "function value takes %d argument(s) but %d were supplied", len(ft.Params), len(x.Args))
	}
	for i, a := range x.Args {
		var want *types.Type
		if i < len(ft.Params) {
			want = ft.Params[i]
		}
		at := c.checkExpr(a, want)
		if want != nil && !c.argAssignable(want, at, false) {
			c.errAt(a, "E0308", "mismatched types: expected `%s`, found `%s`", want, at)
		}
	}
	return ft.Ret
}

func (c *checker) inferCall(x *ast.CallExpr, expected *types.Type) *types.Type {
	// `Enum::Variant(args)` is a variant constructor, and `String::new()` /
	// `String::from(..)` are builtin constructors.
	if p, ok := x.Callee.(*ast.PathExpr); ok && len(p.Segments) == 2 {
		if p.Segments[0] == "String" {
			return c.inferStringCtor(x, p.Segments[1])
		}
		if p.Segments[0] == "Vec" {
			return c.inferVecCtor(x, p, p.Segments[1], expected)
		}
		if p.Segments[0] == "HashMap" {
			return c.inferMapCtor(x, p, p.Segments[1], expected)
		}
		// `Type::name(args)` may also name an associated function: one declared
		// in an `impl` block without a `self` parameter.
		if m, ok := c.info.Methods[p.Segments[0]][p.Segments[1]]; ok {
			if m.Generic {
				m = c.instantiateAssociatedMethod(m, p, expected)
				if m == nil {
					for _, a := range x.Args {
						c.checkExpr(a, nil)
					}
					return &types.Type{Kind: types.Invalid}
				}
			}
			if m.Self != nil {
				c.errf(p.Sp, "E0599", "`%s::%s` takes `self`; call it with `.` on a value", p.Segments[0], p.Segments[1])
				for _, a := range x.Args {
					c.checkExpr(a, nil)
				}
				return m.Ret
			}
			return c.checkCallArgs(x, m, fmt.Sprintf("function `%s::%s`", p.Segments[0], p.Segments[1]))
		}
		return c.inferVariantCtor(x, p, expected)
	}
	// Any expression of function type is callable, not just a name: the result
	// of a call that returns a `fn(..)` (`make_adder(3)(x)`), an indexed slot
	// (`handlers[i](req)`), or a field (`self.on_click(req)`).
	if !isCallableName(x.Callee) {
		if ft := c.exprOfFnType(x.Callee); ft != nil {
			return c.checkIndirectCall(x, ft)
		}
		c.errAt(x.Callee, "E0618", "expression is not callable")
		for _, a := range x.Args {
			c.checkExpr(a, nil)
		}
		return nil
	}
	name, ok := calleeName(x.Callee)
	if !ok {
		c.errAt(x.Callee, "E0618", "expression is not callable")
		for _, a := range x.Args {
			c.checkExpr(a, nil)
		}
		return nil
	}
	if name == "print" || name == "println" || name == "eprint" || name == "eprintln" {
		return c.inferPrint(x)
	}
	if name == "panic" {
		return c.inferPanic(x)
	}
	if name == "len" {
		return c.inferLen(x)
	}
	if name == "cstr" {
		return c.inferCStr(x)
	}
	if name == "from_cstr" {
		return c.inferFromCStr(x)
	}
	if name == "arg_count" {
		return c.inferArgCount(x)
	}
	if name == "project_dir" {
		if len(x.Args) != 0 {
			c.errAt(x, "E0061", "`project_dir` takes no arguments but %d were supplied", len(x.Args))
		}
		// An owning String, like `arg_at`: the directory is baked into the
		// binary and handed to the program, so it needs a real value.
		return types.StringT
	}
	if name == "arg_at" {
		return c.inferArgAt(x)
	}
	// `Pair(1, 2)` constructs a tuple struct; a named struct used this way
	// gets a pointed error rather than "cannot find function".
	if st, ok := c.info.Structs[name]; ok {
		if !st.Tuple {
			c.errAt(x.Callee, "E0061", "struct `%s` is not a tuple struct; construct it as `%s { field: value }`", name, name)
			for _, a := range x.Args {
				c.checkExpr(a, nil)
			}
			return &types.Type{Kind: types.StructKind, St: st}
		}
		concrete := st
		if st.Generic {
			bindings := map[string]*types.Type{}
			explicit := calleeTypeArgs(x.Callee)
			if len(explicit) > len(st.Generics) {
				c.errAt(x.Callee, "E0107", "`%s` takes %d type argument(s) but %d were supplied", st.Base, len(st.Generics), len(explicit))
			}
			for i, gname := range st.Generics {
				if i < len(explicit) {
					if t := c.resolveType(explicit[i]); t != nil {
						bindings[gname] = t
					}
				}
			}
			if expected != nil && expected.Kind == types.StructKind && expected.St != nil && nominalBaseName(expected) == st.Base {
				for i, gname := range st.Generics {
					if _, ok := bindings[gname]; !ok && i < len(expected.St.Args) {
						bindConcrete(bindings, gname, expected.St.Args[i])
					}
				}
			}
			if !allBound(st.Generics, bindings) {
				for i, a := range x.Args {
					pat := c.structFieldPattern(st, strconv.Itoa(i))
					if pat == nil {
						continue
					}
					at := c.checkExpr(a, hintType(pat))
					c.unify(pat, at, bindings)
				}
			}
			args := c.bindGenericArgs(x.Callee.Span(), st.Generics, bindings)
			if args == nil {
				for _, a := range x.Args {
					c.checkExpr(a, nil)
				}
				return &types.Type{Kind: types.Invalid}
			}
			t := c.instantiateStruct(x.Callee.Span(), st, args)
			if t.Kind != types.StructKind || t.St == nil {
				return &types.Type{Kind: types.Invalid}
			}
			concrete = t.St
		} else if len(calleeTypeArgs(x.Callee)) > 0 {
			c.errAt(x.Callee, "E0107", "`%s` is not generic", st.Base)
		}
		c.info.TupleCtors[x] = concrete
		if len(x.Args) != len(concrete.Fields) {
			c.errAt(x, "E0061", "tuple struct `%s` takes %d field(s) but %d were supplied", name, len(concrete.Fields), len(x.Args))
		}
		for i, a := range x.Args {
			var want *types.Type
			if i < len(concrete.Fields) {
				want = concrete.Fields[i].Ty
			}
			at := c.checkExpr(a, want)
			if want != nil && !c.argAssignable(want, at, false) {
				c.errAt(a, "E0308", "mismatched types: expected `%s`, found `%s`", want, at)
			}
		}
		return &types.Type{Kind: types.StructKind, St: concrete}
	}
	// A local binding of function type can be called directly: this is the
	// indirect call the router/handler APIs are built on. Reading the binding
	// from the enclosing scope is also a capture, like any other read — a
	// closure that calls `handler(x)` must carry `handler` in its environment.
	if ft := c.lookupScope(name); ft != nil && ft.Kind == types.Fn {
		c.noteCapture(name, ft)
		c.info.Indirect[x] = ft
		if len(x.Args) != len(ft.Params) {
			c.errAt(x, "E0061", "function value takes %d argument(s) but %d were supplied", len(ft.Params), len(x.Args))
		}
		for i, a := range x.Args {
			var want *types.Type
			if i < len(ft.Params) {
				want = ft.Params[i]
			}
			at := c.checkExpr(a, want)
			if want != nil && !c.argAssignable(want, at, false) {
				c.errAt(a, "E0308", "mismatched types: expected `%s`, found `%s`", want, at)
			}
		}
		return ft.Ret
	}
	fn, ok := c.info.Fns[name]
	if !ok {
		// A bare variant name (`Ok(x)`, `Some(x)`) resolves against the
		// expected enum type, so the examples read naturally.
		if t := c.inferBareVariantCtor(x, name, expected); t != nil {
			return t
		}
		cands := []string{"print", "println", "eprint", "eprintln", "panic", "len", "cstr", "from_cstr", "arg_count", "arg_at", "project_dir"}
		for n := range c.info.Fns {
			cands = append(cands, n)
		}
		c.errAt(x.Callee, "E0425", "cannot find function `%s` in this scope%s", name, suggestName(name, cands))
		for _, a := range x.Args {
			c.checkExpr(a, nil)
		}
		return nil
	}
	if fn.Generic {
		return c.checkGenericCall(x, fn, calleeTypeArgs(x.Callee), expected, fmt.Sprintf("function `%s`", name))
	}
	if len(calleeTypeArgs(x.Callee)) > 0 {
		c.errAt(x.Callee, "E0107", "`%s` is not generic", name)
	}
	return c.checkCallArgs(x, fn, fmt.Sprintf("function `%s`", name))
}

// resolveArgs rewrites a call's argument list into plain positional
// order: named arguments (`name = value`) are matched to parameters by
// name, and omitted trailing parameters that have defaults are filled
// with their default literals. names/defaults are aligned with the
// callee's value parameters. When the call is already plain positional
// with the right arity it is returned unchanged, so the caller's arity
// diagnostic still fires for malformed calls.
func (c *checker) resolveArgs(sp token.Span, args []ast.Expr, names []string, defaults []ast.Expr, label string) []ast.Expr {
	hasNamed := false
	for _, a := range args {
		if _, ok := a.(*ast.NamedArgExpr); ok {
			hasNamed = true
			break
		}
	}
	if !hasNamed {
		if len(args) == len(names) {
			return args
		}
		if len(args) < len(names) {
			filled := make([]ast.Expr, len(names))
			copy(filled, args)
			ok := true
			for i := len(args); i < len(names); i++ {
				if defaults[i] == nil {
					ok = false
					break
				}
				filled[i] = ast.Clone(defaults[i])
			}
			if ok {
				return filled
			}
		}
		return args
	}
	out := make([]ast.Expr, len(names))
	seen := make([]bool, len(names))
	pos := 0
	for _, a := range args {
		if na, ok := a.(*ast.NamedArgExpr); ok {
			idx := -1
			for i, n := range names {
				if n == na.Name {
					idx = i
					break
				}
			}
			if idx < 0 {
				c.errf(na.Sp, "E0061", "%s has no parameter named `%s`", label, na.Name)
				continue
			}
			if seen[idx] {
				c.errf(na.Sp, "E0062", "argument `%s` was specified more than once", na.Name)
				continue
			}
			seen[idx] = true
			out[idx] = na.Val
			continue
		}
		if pos >= len(names) {
			// `out` always has len(names) entries, so the caller's arity
			// check never sees a surplus — report it here.
			c.errf(a.Span(), "E0061", "%s takes %d argument(s) but more were supplied", label, len(names))
			break
		}
		// Find the next unfilled slot for this positional argument; a
		// positional argument may not follow a named one for a slot
		// that a named argument already claimed out of order.
		for pos < len(names) && seen[pos] {
			pos++
		}
		if pos >= len(names) {
			c.errf(a.Span(), "E0061", "%s takes %d argument(s) but more were supplied", label, len(names))
			break
		}
		seen[pos] = true
		out[pos] = a
		pos++
	}
	for i := range out {
		if out[i] == nil && defaults != nil && defaults[i] != nil {
			out[i] = ast.Clone(defaults[i])
			seen[i] = true
		}
	}
	for i := range out {
		if out[i] == nil {
			c.errf(sp, "E0061", "%s is missing an argument for parameter `%s`", label, names[i])
			out[i] = &ast.LiteralExpr{Kind: token.KindInteger, Text: "0", Sp: sp}
		}
	}
	return out
}

// checkCallArgs records the resolved callee, checks a call's arguments against
// its parameter list, and returns the callee's return type.
func (c *checker) checkCallArgs(x *ast.CallExpr, fn *Fn, label string) *types.Type {
	c.info.Resolved[x] = fn
	x.Args = c.resolveArgs(x.Sp, x.Args, fn.ParamNames, fn.Defaults, label)
	if len(x.Args) != len(fn.Params) {
		c.errAt(x, "E0061", "%s takes %d argument(s) but %d were supplied", label, len(fn.Params), len(x.Args))
	}
	for i, a := range x.Args {
		var want *types.Type
		if i < len(fn.Params) {
			want = fn.Params[i]
		}
		at := c.checkExpr(a, want)
		if want != nil && !c.argAssignable(want, at, fn.Extern) {
			c.errAt(a, "E0308", "mismatched types: expected `%s`, found `%s`", want, at)
		}
	}
	return fn.Ret
}

// calleeTypeArgs returns the explicit turbofish arguments on a callee.
func calleeTypeArgs(e ast.Expr) []ast.Type {
	switch x := e.(type) {
	case *ast.IdentExpr:
		return x.TypeArgs
	case *ast.PathExpr:
		return x.TypeArgs
	}
	return nil
}

// checkGenericCall infers a generic function's type arguments (from explicit
// turbofish, the expected result, and the argument types), instantiates it, and
// checks the call against the concrete signature.
func (c *checker) checkGenericCall(x *ast.CallExpr, tmpl *Fn, explicit []ast.Type, expected *types.Type, label string) *types.Type {
	bindings := map[string]*types.Type{}
	if len(explicit) > len(tmpl.Generics) {
		c.errAt(x, "E0107", "`%s` takes %d type argument(s) but %d were supplied", tmpl.Name, len(tmpl.Generics), len(explicit))
	}
	for i, name := range tmpl.Generics {
		if i < len(explicit) {
			if t := c.resolveType(explicit[i]); t != nil {
				bindings[name] = t
			}
		}
	}
	// A known result type can pin parameters that the arguments cannot.
	if expected != nil {
		c.unify(tmpl.Ret, expected, bindings)
	}
	x.Args = c.resolveArgs(x.Sp, x.Args, tmpl.ParamNames, tmpl.Defaults, label)
	if !allBound(tmpl.Generics, bindings) {
		for i, a := range x.Args {
			var hint *types.Type
			if i < len(tmpl.Params) {
				hint = hintType(tmpl.Params[i])
			}
			at := c.checkExpr(a, hint)
			if i < len(tmpl.Params) {
				c.unify(tmpl.Params[i], at, bindings)
			}
		}
	}
	args := c.bindGenericArgs(x.Callee.Span(), tmpl.Generics, bindings)
	if args == nil {
		return &types.Type{Kind: types.Invalid}
	}
	inst := c.instantiateFn(tmpl, args)
	// Re-check the arguments against the concrete signature so that integer
	// literals adopt the instantiated type (e.g. `id::<i64>(1)`).
	return c.checkCallArgs(x, inst, label)
}

// bindGenericArgs materializes the concrete arguments for a generic item,
// reporting E0282 for any parameter inference left unbound.
func (c *checker) bindGenericArgs(sp token.Span, generics []string, bindings map[string]*types.Type) []*types.Type {
	args := make([]*types.Type, len(generics))
	ok := true
	for i, name := range generics {
		if t, found := bindings[name]; found && t != nil && t.Kind != types.Invalid {
			args[i] = t
			continue
		}
		c.errf(sp, "E0282", "type annotations needed: cannot infer type parameter `%s`", name)
		args[i] = &types.Type{Kind: types.Invalid}
		ok = false
	}
	if !ok {
		return nil
	}
	return args
}

// allBound reports whether every type parameter already has a concrete binding.
// When it does, the untyped inference pre-pass is unnecessary (and would check
// arguments such as bare `Some`/`None` without the context they need).
func allBound(generics []string, bindings map[string]*types.Type) bool {
	for _, name := range generics {
		if t, ok := bindings[name]; !ok || t == nil || t.Kind == types.Invalid || containsParam(t) {
			return false
		}
	}
	return true
}

// bindConcrete records a concrete type for a parameter, ignoring parametric
// (template-only) candidates; those cannot become instantiation arguments.
func bindConcrete(b map[string]*types.Type, name string, t *types.Type) {
	if t == nil || t.Kind == types.Invalid || containsParam(t) {
		return
	}
	if _, ok := b[name]; !ok {
		b[name] = t
	}
}

// unify structurally matches a (possibly generic) parameter type against a
// concrete type, recording parameter bindings. It deliberately does not report
// diagnostics: the caller checks assignability against the instantiated
// signature, which produces the precise error.
func (c *checker) unify(pat, got *types.Type, b map[string]*types.Type) bool {
	if pat == nil || got == nil || got.Kind == types.Invalid || pat.Kind == types.Invalid {
		return true
	}
	// A parametric type is never a concrete binding; binding it would create a
	// recursive/partly-generic instantiation. Leave the parameter unbound so
	// the caller reports E0282.
	if got.Kind == types.ParamKind || containsParam(got) {
		return true
	}
	if pat.Kind == types.ParamKind {
		if prev, ok := b[pat.ParamName]; ok {
			return types.Equal(prev, got)
		}
		b[pat.ParamName] = got
		return true
	}
	if pat.Kind != got.Kind {
		return false
	}
	switch pat.Kind {
	case types.Ref, types.Ptr, types.Slice:
		return c.unify(pat.Elem, got.Elem, b)
	case types.Array:
		if pat.Len != got.Len {
			return false
		}
		return c.unify(pat.Elem, got.Elem, b)
	case types.Tuple:
		if len(pat.Elems) != len(got.Elems) {
			return false
		}
		ok := true
		for i := range pat.Elems {
			if !c.unify(pat.Elems[i], got.Elems[i], b) {
				ok = false
			}
		}
		return ok
	case types.Fn:
		if len(pat.Params) != len(got.Params) {
			return false
		}
		ok := true
		for i := range pat.Params {
			if !c.unify(pat.Params[i], got.Params[i], b) {
				ok = false
			}
		}
		if !c.unify(pat.Ret, got.Ret, b) {
			ok = false
		}
		return ok
	case types.StructKind:
		if pat.St == nil || got.St == nil {
			return true
		}
		return c.unifyNominal(nominalBaseName(pat), pat.St.Args, nominalBaseName(got), got.St.Args, b)
	case types.EnumKind:
		if pat.En == nil || got.En == nil {
			return true
		}
		return c.unifyNominal(nominalBaseName(pat), pat.En.Args, nominalBaseName(got), got.En.Args, b)
	}
	return true
}

// unifyNominal matches two applications of the same nominal type, binding
// parameters from their argument lists (an empty argument list means a bare
// template or a non-generic type, which has nothing to bind).
func (c *checker) unifyNominal(patName string, patArgs []*types.Type, gotName string, gotArgs []*types.Type, b map[string]*types.Type) bool {
	if patName != gotName {
		return false
	}
	if len(patArgs) == 0 || len(gotArgs) == 0 || len(patArgs) != len(gotArgs) {
		return len(patArgs) == 0 || len(gotArgs) == 0
	}
	ok := true
	for i := range patArgs {
		if !c.unify(patArgs[i], gotArgs[i], b) {
			ok = false
		}
	}
	return ok
}

// withBindings runs fn with additional type-parameter bindings layered over the
// current environment (used while re-checking an instantiated body).
func (c *checker) withBindings(bind map[string]*types.Type, fn func()) {
	saved := c.typeEnv
	merged := make(map[string]*types.Type, len(saved)+len(bind))
	for k, v := range saved {
		merged[k] = v
	}
	for k, v := range bind {
		merged[k] = v
	}
	c.typeEnv = merged
	fn()
	c.typeEnv = saved
}

// instantiateFn clones a generic function's body, re-checks its signature with
// the concrete arguments, and registers it as an ordinary function. Its body is
// checked by the checkBodies worklist.
func (c *checker) instantiateFn(tmpl *Fn, args []*types.Type) *Fn {
	key := instKey(tmpl.Name, args)
	if fn, ok := c.info.Fns[key]; ok {
		return fn
	}
	clone := ast.Clone(tmpl.Decl)
	clone.Generics = nil
	bind := make(map[string]*types.Type, len(tmpl.Generics))
	for i, name := range tmpl.Generics {
		if i < len(args) && args[i] != nil {
			bind[name] = args[i]
		}
	}
	var fn *Fn
	c.withBindings(bind, func() {
		fn = c.makeFnInner(clone, tmpl.Recv)
	})
	fn.Name = key
	fn.QualName = key
	fn.Generics = nil
	fn.Generic = false
	fn.Extern = tmpl.Extern
	fn.ABI = tmpl.ABI
	c.info.Fns[key] = fn
	c.info.FnOrder = append(c.info.FnOrder, fn)
	return fn
}

// instKey is the canonical, injective name of a generic instantiation:
// `id$3:i64`. Length-prefixed components keep names containing `$` (nested
// instantiations) unambiguous.
func instKey(base string, args []*types.Type) string {
	var b strings.Builder
	b.WriteString(base)
	for _, a := range args {
		k := typeKey(a)
		b.WriteString("$")
		b.WriteString(strconv.Itoa(len(k)))
		b.WriteString(":")
		b.WriteString(k)
	}
	return b.String()
}

// typeKey is an injective textual key for a semantic type, used to name
// instantiations. Compound constructors start with a `$` tag, which cannot
// occur in a source identifier (and is escaped by codegen's safeIdent).
func typeKey(t *types.Type) string {
	if t == nil {
		return "void"
	}
	switch t.Kind {
	case types.ParamKind:
		return compound("p", t.ParamName)
	case types.StructKind:
		if t.St != nil {
			return t.St.Name
		}
		return "struct"
	case types.EnumKind:
		if t.En != nil {
			return t.En.Name
		}
		return "enum"
	case types.Ref:
		m := "0"
		if t.Mut {
			m = "1"
		}
		return compound("r", m, typeKey(t.Elem))
	case types.Ptr:
		return compound("x", typeKey(t.Elem))
	case types.Slice:
		return compound("s", typeKey(t.Elem))
	case types.Array:
		return compound("a", strconv.FormatInt(t.Len, 10), typeKey(t.Elem))
	case types.Tuple:
		parts := make([]string, len(t.Elems))
		for i, e := range t.Elems {
			parts[i] = typeKey(e)
		}
		return compound("t", parts...)
	case types.Fn:
		parts := make([]string, 0, len(t.Params)+2)
		parts = append(parts, strconv.Itoa(len(t.Params)))
		for _, p := range t.Params {
			parts = append(parts, typeKey(p))
		}
		parts = append(parts, typeKey(t.Ret))
		return compound("f", parts...)
	default:
		return t.String()
	}
}

func compound(tag string, parts ...string) string {
	var b strings.Builder
	b.WriteString("$")
	b.WriteString(tag)
	for _, p := range parts {
		b.WriteString(strconv.Itoa(len(p)))
		b.WriteString(":")
		b.WriteString(p)
	}
	return b.String()
}

// paramBindings maps a template's parameter names to concrete arguments.
func paramBindings(names []string, args []*types.Type) map[string]*types.Type {
	b := make(map[string]*types.Type, len(names))
	for i, n := range names {
		if i < len(args) && args[i] != nil {
			b[n] = args[i]
		}
	}
	return b
}

// containsParam reports whether t mentions a type parameter anywhere.
func containsParam(t *types.Type) bool {
	if t == nil {
		return false
	}
	switch t.Kind {
	case types.ParamKind:
		return true
	case types.Ref, types.Ptr, types.Slice, types.Array:
		return containsParam(t.Elem)
	case types.Tuple:
		if slices.ContainsFunc(t.Elems, containsParam) {
			return true
		}
	case types.Fn:
		if slices.ContainsFunc(t.Params, containsParam) {
			return true
		}
		return containsParam(t.Ret)
	case types.StructKind:
		if t.St != nil {
			if slices.ContainsFunc(t.St.Args, containsParam) {
				return true
			}
		}
	case types.EnumKind:
		if t.En != nil {
			if slices.ContainsFunc(t.En.Args, containsParam) {
				return true
			}
		}
	}
	return false
}

// applyStructArgs resolves a written application `Foo<..>` of a nominal type.
func (c *checker) applyStructArgs(sp token.Span, st *types.Struct, args []*types.Type) *types.Type {
	if !st.Generic {
		c.errf(sp, "E0107", "`%s` is not generic", st.Base)
		return &types.Type{Kind: types.StructKind, St: st}
	}
	args, ok := c.checkTypeArgArity(sp, st.Base, st.Generics, args)
	if !ok {
		return &types.Type{Kind: types.Invalid}
	}
	if anyContainsParam(args) {
		// A parametric application inside a template: keep the arguments so
		// that inference can unify them with a concrete instantiation. This
		// descriptor is never emitted; the concrete instantiation re-resolves
		// from the AST.
		return &types.Type{Kind: types.StructKind, St: &types.Struct{
			Name: st.Name, Base: st.Base, Args: args, Generics: st.Generics,
			Tuple: st.Tuple, Copy: st.Copy, Generic: true,
		}}
	}
	return c.instantiateStruct(sp, st, args)
}

// applyEnumArgs resolves a written application `E<..>` of a nominal type.
func (c *checker) applyEnumArgs(sp token.Span, en *types.Enum, args []*types.Type) *types.Type {
	if !en.Generic {
		c.errf(sp, "E0107", "`%s` is not generic", en.Base)
		return &types.Type{Kind: types.EnumKind, En: en}
	}
	args, ok := c.checkTypeArgArity(sp, en.Base, en.Generics, args)
	if !ok {
		return &types.Type{Kind: types.Invalid}
	}
	if anyContainsParam(args) {
		return &types.Type{Kind: types.EnumKind, En: &types.Enum{
			Name: en.Name, Base: en.Base, Args: args, Generics: en.Generics,
			Copy: en.Copy, Generic: true,
		}}
	}
	return c.instantiateEnum(sp, en, args)
}

func anyContainsParam(ts []*types.Type) bool {
	return slices.ContainsFunc(ts, containsParam)
}

// checkTypeArgArity validates the number of type arguments against a template's
// parameters, padding or truncating so callers never index out of range.
func (c *checker) checkTypeArgArity(sp token.Span, base string, params []string, args []*types.Type) ([]*types.Type, bool) {
	if len(args) == len(params) {
		return args, true
	}
	c.errf(sp, "E0107", "`%s` expects %d type argument(s) but %d were supplied", base, len(params), len(args))
	for len(args) < len(params) {
		args = append(args, &types.Type{Kind: types.Invalid})
	}
	return args[:len(params)], false
}

// instantiateStruct builds the concrete struct for a type argument list by
// cloning the declaration and re-resolving its fields with the arguments bound.
func (c *checker) instantiateStruct(sp token.Span, tmpl *types.Struct, args []*types.Type) *types.Type {
	key := instKey(tmpl.Name, args)
	if c.instBusy[key] {
		// Recursion through a reference or pointer is fine: the C definition
		// only needs a forward declaration. Direct recursion has no finite C
		// representation.
		if c.indirect > 0 {
			if st, ok := c.info.Structs[key]; ok {
				return &types.Type{Kind: types.StructKind, St: st}
			}
			return &types.Type{Kind: types.Invalid}
		}
		c.errf(sp, "E0072", "recursive generic type `%s` has infinite size; store it behind a pointer or reference", tmpl.Base)
		return &types.Type{Kind: types.Invalid}
	}
	if st, ok := c.info.Structs[key]; ok {
		return &types.Type{Kind: types.StructKind, St: st}
	}
	c.instBusy[key] = true
	defer delete(c.instBusy, key)
	d := ast.Clone(c.structDecls[tmpl.Name])
	if d == nil {
		return &types.Type{Kind: types.Invalid}
	}
	st := &types.Struct{Name: key, Base: tmpl.Base, Args: args, Tuple: tmpl.Tuple, Copy: tmpl.Copy}
	c.info.Structs[key] = st
	c.info.StructOrd = append(c.info.StructOrd, st)
	c.withBindings(paramBindings(tmpl.Generics, args), func() {
		c.collectStructFieldsInner(d, st)
	})
	return &types.Type{Kind: types.StructKind, St: st}
}

// instantiateEnum is the enum counterpart of instantiateStruct.
func (c *checker) instantiateEnum(sp token.Span, tmpl *types.Enum, args []*types.Type) *types.Type {
	key := instKey(tmpl.Name, args)
	if c.instBusy[key] {
		if c.indirect > 0 {
			if en, ok := c.info.Enums[key]; ok {
				return &types.Type{Kind: types.EnumKind, En: en}
			}
			return &types.Type{Kind: types.Invalid}
		}
		c.errf(sp, "E0072", "recursive generic type `%s` has infinite size; store it behind a pointer or reference", tmpl.Base)
		return &types.Type{Kind: types.Invalid}
	}
	if en, ok := c.info.Enums[key]; ok {
		return &types.Type{Kind: types.EnumKind, En: en}
	}
	c.instBusy[key] = true
	defer delete(c.instBusy, key)
	d := ast.Clone(c.enumDecls[tmpl.Name])
	if d == nil {
		return &types.Type{Kind: types.Invalid}
	}
	en := &types.Enum{Name: key, Base: tmpl.Base, Args: args, Copy: tmpl.Copy}
	c.info.Enums[key] = en
	c.info.EnumOrd = append(c.info.EnumOrd, en)
	c.withBindings(paramBindings(tmpl.Generics, args), func() {
		c.collectEnumVariantsInner(d, en)
	})
	return &types.Type{Kind: types.EnumKind, En: en}
}

// nominalBaseName returns the source (base) name of a nominal type, so method
// lookup can find the template shared by all instantiations.
func nominalBaseName(t *types.Type) string {
	switch {
	case t == nil:
		return ""
	case t.Kind == types.StructKind && t.St != nil:
		if t.St.Base != "" {
			return t.St.Base
		}
		return t.St.Name
	case t.Kind == types.EnumKind && t.En != nil:
		if t.En.Base != "" {
			return t.En.Base
		}
		return t.En.Name
	}
	return ""
}

// nominalArgs returns a nominal type's instantiation arguments (nil if it is
// not generic).
func nominalArgs(t *types.Type) []*types.Type {
	switch {
	case t == nil:
		return nil
	case t.Kind == types.StructKind && t.St != nil:
		return t.St.Args
	case t.Kind == types.EnumKind && t.En != nil:
		return t.En.Args
	}
	return nil
}

// stringCtorSig describes a builtin `String` constructor.
var stringCtorSig = map[string][]*types.Type{
	"new":           nil,
	"from":          {types.StrT},
	"with_capacity": {types.UsizeT},
}

// inferStringCtor checks `String::new()`, `String::from(str)`, and
// `String::with_capacity(usize)`.
func (c *checker) inferStringCtor(x *ast.CallExpr, name string) *types.Type {
	want, ok := stringCtorSig[name]
	if !ok {
		c.errf(x.Callee.Span(), "E0599", "no function `String::%s`", name)
		for _, a := range x.Args {
			c.checkExpr(a, nil)
		}
		return types.StringT
	}
	if len(x.Args) != len(want) {
		c.errAt(x, "E0061", "`String::%s` takes %d argument(s) but %d were supplied", name, len(want), len(x.Args))
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
	return types.StringT
}

// stringMethodSig describes a builtin method on `String`.
// StringMethodParams returns the parameter types of the builtin `String` method
// name, or nil if there is no such method. Codegen uses it to coerce `String`
// arguments to `str` parameters.
func StringMethodParams(name string) []*types.Type {
	m, ok := stringMethods[name]
	if !ok {
		return nil
	}
	return m.params
}

type stringMethodSig struct {
	params []*types.Type
	ret    *types.Type
	mut    bool // takes `&mut self`
}

var stringMethods = map[string]stringMethodSig{
	"len":         {nil, types.UsizeT, false},
	"is_empty":    {nil, types.BoolT, false},
	"as_str":      {nil, types.StrT, false},
	"clone":       {nil, types.StringT, false},
	"clear":       {nil, types.UnitT, true},
	"push":        {[]*types.Type{types.CharT}, types.UnitT, true},
	"push_str":    {[]*types.Type{types.StrT}, types.UnitT, true},
	"push_byte":   {[]*types.Type{types.U8T}, types.UnitT, true},
	"push_bytes":  {[]*types.Type{{Kind: types.Ref, Elem: &types.Type{Kind: types.Slice, Elem: types.U8T}}}, types.UnitT, true},
	"as_bytes":    {nil, &types.Type{Kind: types.Ref, Elem: &types.Type{Kind: types.Slice, Elem: types.U8T}}, false},
	"byte_at":     {[]*types.Type{types.UsizeT}, types.I64T, false},
	"find":        {[]*types.Type{types.StrT}, types.I64T, false},
	"contains":    {[]*types.Type{types.StrT}, types.BoolT, false},
	"starts_with": {[]*types.Type{types.StrT}, types.BoolT, false},
	"ends_with":   {[]*types.Type{types.StrT}, types.BoolT, false},
	"substr":      {[]*types.Type{types.UsizeT, types.UsizeT}, types.StringT, false},
	"trim":        {nil, types.StringT, false},
	"parse_i64":   {nil, types.I64T, false},
}

// inferStringMethod type-checks a builtin method call on a `String`.
func (c *checker) inferStringMethod(x *ast.MethodCallExpr) *types.Type {
	m, ok := stringMethods[x.Name]
	if !ok {
		c.errAt(x, "E0599", "no method `%s` on type `String`", x.Name)
		for _, a := range x.Args {
			c.checkExpr(a, nil)
		}
		return nil
	}
	if len(x.Args) != len(m.params) {
		c.errAt(x, "E0061", "method `String::%s` takes %d argument(s) but %d were supplied", x.Name, len(m.params), len(x.Args))
	}
	if m.mut {
		c.checkMutable(x.Recv, x.Recv.Span(), "cannot borrow `%s` as mutable, as it is not declared `mut`")
	}
	for i, a := range x.Args {
		var wt *types.Type
		if i < len(m.params) {
			wt = m.params[i]
		}
		at := c.checkExpr(a, wt)
		if wt != nil && !c.argAssignable(wt, at, false) {
			c.errAt(a, "E0308", "mismatched types: expected `%s`, found `%s`", wt, at)
		}
	}
	return m.ret
}

// inferCStr checks the C-bridge builtin `cstr(x)`: it borrows a
// NUL-terminated C string (`*u8`) from a `str` or a `String`, for
// passing to `extern "C"` functions that take `const char *`. The value
// is only borrowed; the argument is never consumed.
func (c *checker) inferCStr(x *ast.CallExpr) *types.Type {
	if len(x.Args) != 1 {
		c.errAt(x, "E0061", "`cstr` takes 1 argument but %d were supplied", len(x.Args))
		for _, a := range x.Args {
			c.checkExpr(a, nil)
		}
		return types.PtrTo(types.U8T)
	}
	at := c.checkExpr(x.Args[0], nil)
	if at != nil && at.Kind != types.Invalid && at.Kind != types.Str && at.Kind != types.StringKind {
		c.errAt(x.Args[0], "E0308", "`cstr` expects a `str` or `String`, found `%s`", at)
	}
	return types.PtrTo(types.U8T)
}

// inferFromCStr checks the C-bridge builtin `from_cstr(p)`: it copies a
// NUL-terminated C string into an owning Zexi `String`. The caller must
// guarantee that p points to a valid C string.
func (c *checker) inferFromCStr(x *ast.CallExpr) *types.Type {
	if len(x.Args) != 1 {
		c.errAt(x, "E0061", "`from_cstr` takes 1 argument but %d were supplied", len(x.Args))
		for _, a := range x.Args {
			c.checkExpr(a, nil)
		}
		return types.StringT
	}
	at := c.checkExpr(x.Args[0], nil)
	if at != nil && at.Kind != types.Invalid && at.Kind != types.Ptr {
		c.errAt(x.Args[0], "E0308", "`from_cstr` expects a `*u8` C string pointer, found `%s`", at)
	}
	return types.StringT
}

// inferArgCount checks the builtin `arg_count()`: the number of
// command-line arguments the program was started with, including the
// program name at index 0.
func (c *checker) inferArgCount(x *ast.CallExpr) *types.Type {
	if len(x.Args) != 0 {
		c.errAt(x, "E0061", "`arg_count` takes no arguments but %d were supplied", len(x.Args))
		for _, a := range x.Args {
			c.checkExpr(a, nil)
		}
	}
	return types.I32T
}

// inferArgAt checks the builtin `arg_at(i)`: the i-th command-line
// argument as an owning `String` ("" when out of range).
func (c *checker) inferArgAt(x *ast.CallExpr) *types.Type {
	if len(x.Args) != 1 {
		c.errAt(x, "E0061", "`arg_at` takes 1 argument but %d were supplied", len(x.Args))
		for _, a := range x.Args {
			c.checkExpr(a, nil)
		}
		return types.StringT
	}
	at := c.checkExpr(x.Args[0], nil)
	if at != nil && at.Kind != types.Invalid && !at.Kind.IsInt() {
		c.errAt(x.Args[0], "E0308", "`arg_at` expects an integer index, found `%s`", at)
	}
	return types.StringT
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
	if base := recvBase(t); base != nil && base.Kind == types.VecKind {
		return types.UsizeT
	}
	if t != nil && (t.Kind == types.Array || t.Kind == types.Str || t.Kind == types.Slice ||
		t.Kind == types.StringKind || t.Kind == types.VecKind) {
		return types.UsizeT
	}
	if t != nil && t.Kind == types.Ref && t.Elem != nil && t.Elem.Kind == types.Slice {
		return types.UsizeT
	}
	c.errAt(x.Args[0], "E0308", "`len` expects an array, slice, `str`, `String`, or `Vec`, found `%s`", t)
	return types.UsizeT
}

// inferIndex checks `xs[i]` for arrays, slices, and Vecs, including a Vec
// reached through a borrow (`&Vec<T>`).
func (c *checker) inferIndex(x *ast.IndexExpr) *types.Type {
	rt := c.checkExpr(x.Recv, nil)
	it := c.checkExpr(x.Index, nil)
	if base := recvBase(rt); base != nil && base.Kind == types.VecKind {
		rt = base
	}
	if rt != nil && rt.Kind == types.Ref && rt.Elem != nil && rt.Elem.Kind == types.Slice {
		rt = rt.Elem
	}
	// `&[T; N]` and `&mut [T; N]` index like the array they point at, so a
	// buffer a C function filled can be read (and written) through the
	// reference a signature declares. Without this, `out[i] = v` on an
	// `&mut [u8; N]` parameter was E0608 — the one shape an FFI out-parameter
	// arrives in.
	if rt != nil && (rt.Kind == types.Ref || rt.Kind == types.Ptr) && rt.Elem != nil && rt.Elem.Kind == types.Array {
		rt = rt.Elem
	}
	if rt == nil || (rt.Kind != types.Array && rt.Kind != types.Slice && rt.Kind != types.VecKind) {
		c.errAt(x, "E0608", "cannot index a value of type `%s`", rt)
		return nil
	}
	if it != nil && !it.Kind.IsInt() {
		c.errAt(x.Index, "E0308", "array index must be an integer, found `%s`", it)
	}
	// Constant indices are bounds-checked at compile time.
	if n, ok := c.evalConst(x.Index); ok && n.Ty != nil && n.Ty.Kind.IsInt() {
		if rt.Kind == types.Array {
			if n.Int >= rt.Len || n.Int < -rt.Len {
				c.errAt(x.Index, "E0502", "index %d is out of bounds for array of length %d", n.Int, rt.Len)
			}
		}
	}
	return rt.Elem
}

// inferSlice checks `s[lo:hi]` string slicing.
func (c *checker) inferSlice(x *ast.SliceExpr) *types.Type {
	rt := c.checkExpr(x.Recv, nil)
	isStr := rt != nil && (rt.Kind == types.Str || rt.Kind == types.StringKind)
	// Arrays and slices slice into a borrowed view without copying. A slice of
	// a borrowed slice keeps the same element type.
	base := rt
	if base != nil && base.Kind == types.Ref && base.Elem != nil {
		base = base.Elem
	}
	if base != nil && (base.Kind == types.Array || base.Kind == types.Slice) {
		if x.Low != nil {
			it := c.checkExpr(x.Low, nil)
			if it != nil && !it.Kind.IsInt() {
				c.errAt(x.Low, "E0308", "slice bound must be an integer, found `%s`", it)
			}
		}
		if x.High != nil {
			it := c.checkExpr(x.High, nil)
			if it != nil && !it.Kind.IsInt() {
				c.errAt(x.High, "E0308", "slice bound must be an integer, found `%s`", it)
			}
		}
		if base.Kind == types.Slice {
			return base
		}
		return &types.Type{Kind: types.Slice, Elem: base.Elem}
	}
	if !isStr {
		c.errAt(x, "E0608", "cannot slice a value of type `%s`", rt)
		if x.Low != nil {
			c.checkExpr(x.Low, nil)
		}
		if x.High != nil {
			c.checkExpr(x.High, nil)
		}
		return nil
	}
	if x.Low != nil {
		it := c.checkExpr(x.Low, nil)
		if it != nil && !it.Kind.IsInt() {
			c.errAt(x.Low, "E0308", "slice bound must be an integer, found `%s`", it)
		}
	}
	if x.High != nil {
		it := c.checkExpr(x.High, nil)
		if it != nil && !it.Kind.IsInt() {
			c.errAt(x.High, "E0308", "slice bound must be an integer, found `%s`", it)
		}
	}
	return types.StringT
}

// inferArrayLit checks `[a, b, c]` and `[value; count]`, using the expected
// type for the element type when available.
func (c *checker) inferArrayLit(x *ast.ArrayLitExpr, expected *types.Type) *types.Type {
	var wantElem *types.Type
	if expected != nil && expected.Kind == types.Array {
		wantElem = expected.Elem
	}
	if x.Repeat {
		c.checkExpr(x.Count, types.UsizeT)
		n, ok := c.evalConst(x.Count)
		if !ok || n.Ty == nil || !n.Ty.Kind.IsInt() {
			c.errAt(x.Count, "E0308", "array repeat count must be an integer constant")
			c.checkExpr(x.Value, wantElem)
			return &types.Type{Kind: types.Array, Elem: types.I32T, Len: 0}
		}
		v := c.checkExpr(x.Value, wantElem)
		if n.Int < 0 {
			c.errAt(x.Count, "E0080", "array length must be non-negative, found `%d`", n.Int)
			return &types.Type{Kind: types.Array, Elem: v, Len: 0}
		}
		if n.Int > maxArrayLen {
			c.errAt(x.Count, "E0080", "array length `%d` is too large (maximum %d)", n.Int, maxArrayLen)
			return &types.Type{Kind: types.Array, Elem: v, Len: 0}
		}
		if v != nil && !v.IsCopy() {
			c.errAt(x.Value, "E0204", "array repeat requires a `Copy` element type, found `%s`; the value would be duplicated into every slot", v)
		}
		return &types.Type{Kind: types.Array, Elem: v, Len: n.Int}
	}
	if len(x.Elems) == 0 {
		c.errAt(x, "E0283", "cannot infer the element type of an empty array; annotate it")
		return &types.Type{Kind: types.Array, Elem: types.I32T, Len: 0}
	}
	elemT := wantElem
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

// inferEnumArgsFrom resolves an enum's arguments from turbofish and the
// expected type, for a construction site with no payload to infer from.
func (c *checker) inferEnumArgsFrom(sp token.Span, tmpl *types.Enum, explicit []ast.Type, expected *types.Type) ([]*types.Type, bool) {
	bindings := map[string]*types.Type{}
	if len(explicit) > len(tmpl.Generics) {
		c.errf(sp, "E0107", "`%s` takes %d type argument(s) but %d were supplied", tmpl.Base, len(tmpl.Generics), len(explicit))
	}
	for i, name := range tmpl.Generics {
		if i < len(explicit) {
			if t := c.resolveType(explicit[i]); t != nil {
				bindings[name] = t
			}
		}
	}
	if expected != nil && expected.Kind == types.EnumKind {
		var got []*types.Type
		if expected.En != nil && nominalBaseName(expected) == tmpl.Base {
			got = expected.En.Args
		}
		for i, name := range tmpl.Generics {
			if _, ok := bindings[name]; !ok && i < len(got) {
				bindConcrete(bindings, name, got[i])
			}
		}
	}
	args := c.bindGenericArgs(sp, tmpl.Generics, bindings)
	return args, args != nil
}

// hintType returns a nominal pattern type that can give a bare variant or
// struct literal the context it needs, or nil for other patterns.
func hintType(t *types.Type) *types.Type {
	if t == nil {
		return nil
	}
	if (t.Kind == types.EnumKind && t.En != nil) || (t.Kind == types.StructKind && t.St != nil) {
		return t
	}
	return nil
}

// templateHasVariant reports whether a template-only enum descriptor declares
// the named variant, so a bare variant is recognised (rather than reported as
// an unknown function) even when its type arguments are not inferable yet.
func (c *checker) templateHasVariant(en *types.Enum, name string) bool {
	if en == nil || !en.Generic || en.Base == "" || len(en.Variants) > 0 {
		return false
	}
	d := c.enumDecls[en.Base]
	if d == nil {
		return false
	}
	for _, v := range d.Variants {
		if v.Name == name {
			return true
		}
	}
	return false
}

// inferBareVariantCtor resolves `Ok(x)` / `Err(e)` / `Some(x)` when the callee
// is a bare identifier and the expected type is an enum with that variant.
// returns nil when the name is not a variant of the expected enum.
func (c *checker) inferBareVariantCtor(x *ast.CallExpr, name string, expected *types.Type) *types.Type {
	if expected == nil || expected.Kind != types.EnumKind || expected.En == nil {
		return nil
	}
	v, ok := expected.En.Variant(name)
	if !ok {
		if c.templateHasVariant(expected.En, name) {
			for _, a := range x.Args {
				c.checkExpr(a, nil)
			}
			return &types.Type{Kind: types.EnumKind, En: expected.En}
		}
		return nil
	}
	c.info.Ctors[x] = &Ctor{Enum: expected.En, Variant: v}
	if len(x.Args) != len(v.Fields) {
		c.errAt(x, "E0061", "variant `%s::%s` takes %d field(s) but %d were supplied", expected.En.Base, v.Name, len(v.Fields), len(x.Args))
	}
	for i, a := range x.Args {
		var want *types.Type
		if i < len(v.Fields) {
			want = v.Fields[i].Ty
		}
		at := c.checkExpr(a, want)
		if want != nil && !c.argAssignable(want, at, false) {
			c.errAt(a, "E0308", "mismatched types: expected `%s`, found `%s`", want, at)
		}
	}
	return &types.Type{Kind: types.EnumKind, En: expected.En}
}

// inferBareUnitVariant resolves a bare unit-variant name (`None`) in value
// position from the expected enum type.
func (c *checker) inferBareUnitVariant(x *ast.IdentExpr, expected *types.Type) *types.Type {
	if expected == nil || expected.Kind != types.EnumKind || expected.En == nil {
		return nil
	}
	v, ok := expected.En.Variant(x.Name)
	if !ok {
		if c.templateHasVariant(expected.En, x.Name) {
			return &types.Type{Kind: types.EnumKind, En: expected.En}
		}
		return nil
	}
	if len(v.Fields) != 0 {
		return nil
	}
	c.info.Ctors[x] = &Ctor{Enum: expected.En, Variant: v}
	return &types.Type{Kind: types.EnumKind, En: expected.En}
}

// inferVariantCtor checks `Enum::Variant(args)`.
func (c *checker) inferVariantCtor(x *ast.CallExpr, p *ast.PathExpr, expected *types.Type) *types.Type {
	tmpl, ok := c.info.Enums[p.Segments[0]]
	if !ok {
		c.errf(p.Sp, "E0412", "cannot find type `%s` in this scope", p.Segments[0])
		for _, a := range x.Args {
			c.checkExpr(a, nil)
		}
		return nil
	}
	en := tmpl
	if tmpl.Generic {
		var okArgs bool
		en, okArgs = c.instantiateEnumForVariant(x, p, tmpl, expected)
		if !okArgs {
			for _, a := range x.Args {
				c.checkExpr(a, nil)
			}
			return &types.Type{Kind: types.Invalid}
		}
	} else if len(p.TypeArgs) > 0 {
		c.errf(p.Sp, "E0107", "`%s` is not generic", tmpl.Base)
	}
	return c.checkVariantCtor(x, p, en)
}

// checkVariantCtor checks a variant constructor against a concrete enum.
func (c *checker) checkVariantCtor(x *ast.CallExpr, p *ast.PathExpr, en *types.Enum) *types.Type {
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
		if want != nil && !c.argAssignable(want, at, false) {
			c.errAt(a, "E0308", "mismatched types: expected `%s`, found `%s`", want, at)
		}
	}
	return &types.Type{Kind: types.EnumKind, En: en}
}

// instantiateEnumForVariant infers an enum's arguments for a variant
// construction from turbofish, the expected type, and the payload arguments.
func (c *checker) instantiateEnumForVariant(x *ast.CallExpr, p *ast.PathExpr, tmpl *types.Enum, expected *types.Type) (*types.Enum, bool) {
	bindings := map[string]*types.Type{}
	if len(p.TypeArgs) > len(tmpl.Generics) {
		c.errf(p.Sp, "E0107", "`%s` takes %d type argument(s) but %d were supplied", tmpl.Base, len(tmpl.Generics), len(p.TypeArgs))
	}
	for i, name := range tmpl.Generics {
		if i < len(p.TypeArgs) {
			if t := c.resolveType(p.TypeArgs[i]); t != nil {
				bindings[name] = t
			}
		}
	}
	if expected != nil && expected.Kind == types.EnumKind && expected.En != nil && nominalBaseName(expected) == tmpl.Base {
		for i, name := range tmpl.Generics {
			if _, ok := bindings[name]; !ok && i < len(expected.En.Args) {
				bindConcrete(bindings, name, expected.En.Args[i])
			}
		}
	}
	patterns := c.enumVariantPatterns(tmpl, p.Segments[1])
	if !allBound(tmpl.Generics, bindings) {
		for i, a := range x.Args {
			if i >= len(patterns) {
				break
			}
			at := c.checkExpr(a, hintType(patterns[i]))
			c.unify(patterns[i], at, bindings)
		}
	}
	args := c.bindGenericArgs(p.Sp, tmpl.Generics, bindings)
	if args == nil {
		return nil, false
	}
	t := c.instantiateEnum(p.Sp, tmpl, args)
	if t.Kind != types.EnumKind || t.En == nil {
		return nil, false
	}
	return t.En, true
}

// enumFieldPattern resolves one named field of a variant with the enum's type
// parameters in scope, for inference at a struct-variant literal.
func (c *checker) enumFieldPattern(tmpl *types.Enum, variantName, fieldName string) *types.Type {
	d := c.enumDecls[tmpl.Name]
	if d == nil {
		return nil
	}
	var out *types.Type
	c.withTypeParams(tmpl.Generics, func() {
		for _, v := range d.Variants {
			if v.Name != variantName {
				continue
			}
			for i, f := range v.Fields {
				name := f.Name
				if v.Tup {
					name = strconv.Itoa(i)
				}
				if name == fieldName {
					out = c.resolveType(f.Ty)
					return
				}
			}
			return
		}
	})
	return out
}

// enumVariantPatterns resolves a variant's field types with the enum's type
// parameters in scope, for inference at a construction site.
func (c *checker) enumVariantPatterns(tmpl *types.Enum, variantName string) []*types.Type {
	d := c.enumDecls[tmpl.Name]
	if d == nil {
		return nil
	}
	var out []*types.Type
	c.withTypeParams(tmpl.Generics, func() {
		for _, v := range d.Variants {
			if v.Name != variantName {
				continue
			}
			for _, f := range v.Fields {
				out = append(out, c.resolveType(f.Ty))
			}
			return
		}
	})
	return out
}

// isCatchAll reports whether a pattern matches every value (a binding or `_`).
func isCatchAll(p ast.Pattern) bool {
	switch p.(type) {
	case *ast.WildcardPat, *ast.BindPat:
		return true
	}
	return false
}

// coversAllVariants reports whether earlier arms cover every variant of an
// enum scrutinee, so a later catch-all cannot match anything new. Coverage is
// tracked per variant name: a payload pattern like `E::A(x)` matches the whole
// variant, since nested patterns are not part of the language.
func coversAllVariants(base *types.Type, covered map[string]bool) bool {
	if base == nil || base.Kind != types.EnumKind || base.En == nil || len(base.En.Variants) == 0 {
		return false
	}
	for _, v := range base.En.Variants {
		if !covered[v.Name] {
			return false
		}
	}
	return true
}

// coversBothBools reports whether earlier literal arms match both `true` and
// `false`, so a later catch-all cannot match anything new.
func coversBothBools(base *types.Type, boolsCovered map[string]bool) bool {
	return base != nil && base.Kind == types.Bool && boolsCovered["true"] && boolsCovered["false"]
}

// litPatKey canonicalises a literal pattern so duplicates can be rejected before
// they become duplicate C `case` labels.
func litPatKey(p *ast.LitPat) string {
	switch p.Kind {
	case token.KindTrue:
		return "bool:true"
	case token.KindFalse:
		return "bool:false"
	case token.KindInteger:
		return "int:" + normalizeIntText(p.Text)
	case token.KindChar, token.KindByteChar:
		return litPatKindName(p.Kind) + ":" + p.Text
	}
	return litPatKindName(p.Kind) + ":" + p.Text
}

// normalizeIntText folds the various integer literal spellings of one value onto
// a single key (so `1`, `0x1`, and `1_0` style duplicates are seen as one).
func normalizeIntText(s string) string {
	t := strings.ReplaceAll(s, "_", "")
	if v, err := strconv.ParseInt(t, 0, 64); err == nil {
		return strconv.FormatInt(v, 10)
	}
	if v, err := strconv.ParseUint(t, 0, 64); err == nil {
		return strconv.FormatUint(v, 10)
	}
	return t
}

// litPatKindName names the kind of a literal for diagnostics.
func litPatKindName(k token.Kind) string {
	switch k {
	case token.KindInteger:
		return "an integer literal"
	case token.KindFloat:
		return "a float literal"
	case token.KindString, token.KindRawString:
		return "a string literal"
	case token.KindChar:
		return "a char literal"
	case token.KindByteChar:
		return "a byte literal"
	case token.KindTrue, token.KindFalse:
		return "a bool literal"
	}
	return "a " + k.String() + " literal"
}

func (c *checker) inferMatch(x *ast.MatchExpr, expected *types.Type) *types.Type {
	st := c.checkExpr(x.Scrutinee, nil)
	base := recvBase(st)
	if base == nil || (base.Kind != types.EnumKind && !base.Kind.IsInt() && base.Kind != types.Bool && base.Kind != types.Char && base.Kind != types.Str) {
		c.errAt(x.Scrutinee, "E0308", "`match` requires an enum, integer, `bool`, `char`, or `str` scrutinee, found `%s`", st)
	}
	var result *types.Type
	if expected != nil && expected.Kind != types.Unit && expected.Kind != types.Never && expected.Kind != types.Invalid {
		// The context may fix the result type (e.g. `fn f() -> i64 { match .. }`),
		// so that literal arms adopt it.
		result = expected
	}
	covered := map[string]bool{}
	litCovered := map[string]bool{}
	boolsCovered := map[string]bool{}
	catchAll := false
	for _, arm := range x.Arms {
		guarded := arm.Guard != nil
		// dead marks an arm that can never match: an earlier catch-all
		// already matches everything, or (for an enum/bool scrutinee) the
		// earlier arms cover every case. Report it once at the top; the
		// per-pattern checks below stay quiet for the same arm.
		dead := false
		if !guarded && catchAll {
			dead = true
			c.errf(arm.Pat.Span(), "E0001", "unreachable pattern: an earlier arm already matches everything")
		}
		if !guarded && isCatchAll(arm.Pat) {
			if !dead && (coversAllVariants(base, covered) || coversBothBools(base, boolsCovered)) {
				dead = true
				c.errf(arm.Pat.Span(), "E0001", "unreachable pattern: an earlier arm already matches everything")
			}
			catchAll = true
		}
		noteLit := func(lp *ast.LitPat) {
			// Each literal becomes one C `case`, and duplicate cases are invalid
			// C, so catch them here with a real diagnostic.
			key := litPatKey(lp)
			if !dead && litCovered[key] {
				c.errf(lp.Sp, "E0001", "unreachable pattern: this value is already covered")
			}
			litCovered[key] = true
			if lp.Kind == token.KindTrue {
				boolsCovered["true"] = true
			}
			if lp.Kind == token.KindFalse {
				boolsCovered["false"] = true
			}
		}
		if lp, ok := arm.Pat.(*ast.LitPat); ok && !guarded {
			noteLit(lp)
		}
		if op, ok := arm.Pat.(*ast.OrPat); ok && !guarded {
			// An or-pattern covers the union of its alternatives; a catch-all
			// alternative makes the whole arm a catch-all.
			for _, alt := range op.Alts {
				if isCatchAll(alt) {
					if !dead && catchAll {
						c.errf(alt.Span(), "E0001", "unreachable pattern: an earlier arm already matches everything")
					}
					catchAll = true
				}
				if lp, ok := alt.(*ast.LitPat); ok {
					noteLit(lp)
				}
			}
		}
		c.pushScope()
		borrowed := c.info.PlaceThroughBorrow(x.Scrutinee)
		c.checkPattern(arm.Pat, st, borrowed)
		if pp, ok := arm.Pat.(*ast.PathPat); ok && !guarded {
			if v := c.info.PatVariant[pp]; v != nil {
				if !dead && covered[v.Name] {
					c.errf(arm.Pat.Span(), "E0001", "unreachable pattern: `%s` is already covered", v.Name)
				}
				covered[v.Name] = true
			}
		}
		if op, ok := arm.Pat.(*ast.OrPat); ok && !guarded {
			seenHere := map[string]bool{}
			for _, alt := range op.Alts {
				pp, ok := alt.(*ast.PathPat)
				if !ok {
					continue
				}
				if v := c.info.PatVariant[pp]; v != nil && !seenHere[v.Name] {
					seenHere[v.Name] = true
					if !dead && covered[v.Name] {
						c.errf(alt.Span(), "E0001", "unreachable pattern: `%s` is already covered", v.Name)
					}
					covered[v.Name] = true
				}
			}
		}
		if arm.Guard != nil {
			gt := c.checkExpr(arm.Guard, types.BoolT)
			if gt == nil || gt.Kind != types.Bool {
				c.errAt(arm.Guard, "E0308", "match guard must be `bool`, found `%s`", gt)
			}
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
	switch {
	case base != nil && base.Kind == types.EnumKind && !catchAll:
		var missing []string
		for _, v := range base.En.Variants {
			if !covered[v.Name] {
				missing = append(missing, v.Name)
			}
		}
		if len(missing) > 0 {
			c.errAt(x, "E0004", "non-exhaustive match: missing `%s`", strings.Join(missing, "`, `"))
		}
	case base != nil && base.Kind == types.Bool && !catchAll && (!boolsCovered["true"] || !boolsCovered["false"]):
		c.errAt(x, "E0004", "non-exhaustive match: cover both `true` and `false`, or add a `_` arm")
	case base != nil && (base.Kind.IsInt() || base.Kind == types.Char || base.Kind == types.Str) && !catchAll:
		c.errAt(x, "E0004", "non-exhaustive match: add a `_` arm")
	}
	if result == nil {
		result = types.UnitT
	}
	return result
}

// inferTry lowers `e?` to an ordinary match over the operand. The desugared
// match is kept in Info.TryMatches so codegen (which never sees `?`) can emit
// it.
// inferPreludeMethod types the builtin methods of `Option` and `Result`, and
// reports whether it handled the call. The tag tests lower to a single tag
// comparison (so they borrow the receiver and never move it); the extractors
// lower to the same ordinary `match` that `?` desugars to, which is what gives
// them the move analysis and the once-only drop of the remainder for free —
// `r.unwrap()` moves the payload out exactly like
// `match r { Ok(v) => v, Err(_) => panic("...") }`.
func (c *checker) inferPreludeMethod(x *ast.MethodCallExpr, base *types.Type) (*types.Type, bool) {
	baseName := nominalBaseName(base)
	isResult := baseName == "Result"
	okName, errName := "Some", "None"
	if isResult {
		okName, errName = "Ok", "Err"
	}
	var payload *types.Type
	if len(base.En.Args) > 0 {
		payload = base.En.Args[0]
	}
	unitPayload := payload == nil || payload.Kind == types.Unit

	// Tag tests read the discriminant, so they take the receiver by borrow.
	tags := map[string]string{"is_some": "Some", "is_none": "None"}
	if isResult {
		tags = map[string]string{"is_ok": "Ok", "is_err": "Err"}
	}
	if variant, ok := tags[x.Name]; ok {
		if len(x.Args) != 0 {
			c.errAt(x, "E0061", "`%s` takes no arguments but %d were supplied", x.Name, len(x.Args))
			for _, a := range x.Args {
				c.checkExpr(a, nil)
			}
		}
		if _, ok := base.En.Variant(variant); !ok {
			c.errAt(x, "E0277", "`%s` needs a `%s::%s` variant", x.Name, baseName, variant)
			return types.BoolT, true
		}
		c.info.PreludeTag[x] = variant
		return types.BoolT, true
	}

	switch x.Name {
	case "unwrap", "expect", "unwrap_or":
	default:
		return nil, false
	}

	sp := x.Sp
	// The failing arm: `Err(_)` for a Result, bare `None` for an Option.
	failPat := &ast.PathPat{Path: []string{baseName, errName}, Sp: sp}
	if isResult {
		// The error value is not needed, and binding it would move an owned
		// error into an unused local; match it with a wildcard instead.
		failPat.Sub = []ast.Pattern{&ast.WildcardPat{Sp: sp}}
	}
	var failBody ast.Expr
	switch x.Name {
	case "unwrap":
		if len(x.Args) != 0 {
			c.errAt(x, "E0061", "`unwrap` takes no arguments but %d were supplied", len(x.Args))
			for _, a := range x.Args {
				c.checkExpr(a, nil)
			}
		}
		failBody = &ast.CallExpr{
			Callee: &ast.IdentExpr{Name: "panic", Sp: sp},
			Args:   []ast.Expr{&ast.LiteralExpr{Kind: token.KindString, Text: "called `unwrap` on a `" + errName + "` value", Sp: sp}},
			Sp:     sp,
		}
	case "expect":
		if len(x.Args) != 1 {
			c.errAt(x, "E0061", "`expect` takes 1 argument but %d were supplied", len(x.Args))
			for _, a := range x.Args {
				c.checkExpr(a, nil)
			}
			return payload, true
		}
		at := c.checkExpr(x.Args[0], nil)
		if !isPrintable(at) {
			c.errAt(x.Args[0], "E0277", "`%s` cannot be printed", at)
		}
		failBody = &ast.CallExpr{
			Callee: &ast.IdentExpr{Name: "panic", Sp: sp},
			Args:   []ast.Expr{x.Args[0]},
			Sp:     sp,
		}
	case "unwrap_or":
		if len(x.Args) != 1 {
			c.errAt(x, "E0061", "`unwrap_or` takes 1 argument but %d were supplied", len(x.Args))
			for _, a := range x.Args {
				c.checkExpr(a, nil)
			}
			return payload, true
		}
		at := c.checkExpr(x.Args[0], payload)
		if payload != nil && !c.argAssignable(payload, at, false) {
			// Already reported: building the match now would only add a second,
			// less specific diagnostic about its arms.
			c.errAt(x.Args[0], "E0308", "mismatched types: expected `%s`, found `%s`", payload, at)
			return payload, true
		}
		failBody = x.Args[0]
	}
	if _, ok := base.En.Variant(errName); !ok {
		c.errAt(x, "E0277", "`%s` needs a `%s::%s` variant", x.Name, baseName, errName)
		return payload, true
	}

	// The succeeding arm yields the payload, or `()` when there is nothing to
	// extract (a wildcard stands in for `Ok(())`, so no void local is declared).
	var okPat ast.Pattern
	var okBody ast.Expr
	if unitPayload {
		okPat = &ast.WildcardPat{Sp: sp}
		okBody = &ast.TupleExpr{Sp: sp}
	} else {
		const bind = "__prelude_value"
		okPat = &ast.PathPat{
			Path: []string{baseName, okName},
			Sub:  []ast.Pattern{&ast.BindPat{Name: bind, Sp: sp}},
			Sp:   sp,
		}
		okBody = &ast.IdentExpr{Name: bind, Sp: sp}
	}

	m := &ast.MatchExpr{
		Scrutinee: x.Recv,
		// The failing arm comes first: a unit success arm is a wildcard, which
		// would otherwise swallow the failure.
		Arms: []ast.MatchArm{
			{Pat: failPat, Body: failBody, Sp: sp},
			{Pat: okPat, Body: okBody, Sp: sp},
		},
		Sp: sp,
	}
	c.info.PreludeMatches[x] = m
	var want *types.Type
	if x.Name != "unwrap_or" {
		want = payload
	}
	ret := c.checkExpr(m, want)
	c.info.Types[x] = ret
	return ret, true
}

func (c *checker) inferTry(x *ast.TryExpr, expected *types.Type) *types.Type {
	ret := c.cur.Ret
	retBase := ""
	if ret != nil && ret.Kind == types.EnumKind && ret.En != nil {
		retBase = nominalBaseName(ret)
	}
	if retBase != "Result" && retBase != "Option" {
		c.errAt(x, "E0277", "the `?` operator requires the enclosing function to return `Result` or `Option`")
		c.checkExpr(x.X, nil)
		return nil
	}
	src := c.checkExpr(x.X, nil)
	srcBase := nominalBaseName(src)
	if src == nil || src.Kind != types.EnumKind || (srcBase != "Result" && srcBase != "Option") {
		c.errAt(x, "E0308", "the `?` operator requires a `Result` or `Option` operand, found `%s`", src)
		return nil
	}
	if srcBase == "Result" && retBase != "Result" {
		c.errAt(x, "E0277", "`?` on a `Result` requires the enclosing function to return a `Result`")
		return nil
	}
	if srcBase == "Option" && retBase != "Option" {
		c.errAt(x, "E0277", "`?` on an `Option` requires the enclosing function to return an `Option`")
		return nil
	}
	needOk, needErr := "Ok", "Err"
	if srcBase == "Option" {
		needOk, needErr = "Some", "None"
	}
	if _, ok := src.En.Variant(needOk); !ok {
		c.errAt(x, "E0277", "`?` requires a `%s` operand with a `%s` variant", srcBase, needOk)
		return nil
	}
	if _, ok := src.En.Variant(needErr); !ok {
		c.errAt(x, "E0277", "`?` requires a `%s` operand with a `%s` variant", srcBase, needErr)
		return nil
	}
	// The enclosing return type must be able to carry the early return.
	retErr := "Err"
	if srcBase == "Option" {
		retErr = "None"
	}
	if _, ok := ret.En.Variant(retErr); !ok {
		c.errAt(x, "E0277", "`?` requires the enclosing return type to have a `%s` variant", retErr)
		return nil
	}
	if srcBase == "Result" && len(src.En.Args) == 2 && len(ret.En.Args) == 2 {
		if !c.assignable(ret.En.Args[1], src.En.Args[1]) {
			c.errAt(x, "E0308", "`?` cannot convert the error type `%s` to `%s`", src.En.Args[1], ret.En.Args[1])
		}
	}

	const okName, errName = "__try_ok", "__try_err"
	// `Ok(())` has no value to bind: use a wildcard instead, so codegen never
	// declares a `void` local for the payload.
	unitOK := len(src.En.Args) >= 1 && src.En.Args[0] != nil && src.En.Args[0].Kind == types.Unit
	var okPat ast.Pattern
	var okBody ast.Expr
	if unitOK {
		okPat = &ast.WildcardPat{Sp: x.Sp}
		okBody = &ast.TupleExpr{Sp: x.Sp}
	} else {
		okPat = &ast.PathPat{
			Path: []string{srcBase, "Ok"},
			Sub:  []ast.Pattern{&ast.BindPat{Name: okName, Sp: x.Sp}},
			Sp:   x.Sp,
		}
		okBody = &ast.IdentExpr{Name: okName, Sp: x.Sp}
	}
	var errPat ast.Pattern
	var errBody ast.Expr
	if srcBase == "Result" {
		if pp, isPath := okPat.(*ast.PathPat); isPath {
			pp.Path[1] = "Ok"
		}
		errPat = &ast.PathPat{
			Path: []string{"Result", "Err"},
			Sub:  []ast.Pattern{&ast.BindPat{Name: errName, Sp: x.Sp}},
			Sp:   x.Sp,
		}
		errBody = &ast.JumpExpr{Kind: token.KindReturn, Sp: x.Sp, Val: &ast.CallExpr{
			Callee: &ast.PathExpr{Segments: []string{"Result", "Err"}, Sp: x.Sp},
			Args:   []ast.Expr{&ast.IdentExpr{Name: errName, Sp: x.Sp}},
			Sp:     x.Sp,
		}}
	} else {
		if pp, isPath := okPat.(*ast.PathPat); isPath {
			pp.Path[1] = "Some"
		}
		errPat = &ast.PathPat{Path: []string{"Option", "None"}, Sp: x.Sp}
		errBody = &ast.JumpExpr{Kind: token.KindReturn, Sp: x.Sp, Val: &ast.PathExpr{Segments: []string{"Option", "None"}, Sp: x.Sp}}
	}
	m := &ast.MatchExpr{
		Scrutinee: x.X,
		// The error arm comes first: a unit `Ok` desugars to a wildcard, which
		// would make a later arm unreachable.
		Arms: []ast.MatchArm{
			{Pat: errPat, Body: errBody, Sp: x.Sp},
			{Pat: okPat, Body: okBody, Sp: x.Sp},
		},
		Sp: x.Sp,
	}
	c.info.TryMatches[x] = m
	return c.checkExpr(m, expected)
}

// checkPattern type-checks a pattern against the scrutinee type and declares its
// bindings in the current scope.
func (c *checker) checkPattern(p ast.Pattern, st *types.Type, borrowed bool) {
	switch pt := p.(type) {
	case *ast.WildcardPat:
		return
	case *ast.BindPat:
		if pt.Name != "_" {
			if borrowed && st != nil && st.NeedsDrop() {
				c.errf(pt.Sp, "E0507", "cannot move the owned value `%s` out of a borrowed scrutinee", pt.Name)
			}
			c.declare(pt.Name, st)
		}
		return
	case *ast.OrPat:
		for _, alt := range pt.Alts {
			if patternBinds(alt) {
				// Which alternative matched would be ambiguous, and codegen
				// binds nothing for an or-pattern arm.
				c.errf(alt.Span(), "E0900", "or-pattern alternatives cannot bind variables; match on the whole value and destructure in the arm body instead")
			}
			c.checkPattern(alt, st, borrowed)
		}
		return
	case *ast.RangePat:
		c.checkRangePat(pt, st)
		return
	case *ast.LitPat:
		if pt.Kind == token.KindChar || pt.Kind == token.KindByteChar {
			if r, ok := decodeCharLiteral(pt.Text); !ok || (pt.Kind == token.KindByteChar && r > 0xFF) {
				c.errf(pt.Sp, "E0001", "invalid char literal in pattern")
			}
		}
		switch {
		case st != nil && st.Kind.IsInt():
			if pt.Kind != token.KindInteger && pt.Kind != token.KindByteChar {
				c.errf(pt.Sp, "E0308", "mismatched types: expected `%s` in pattern, found %s", st, litPatKindName(pt.Kind))
			} else if pt.Kind == token.KindInteger && !intLitFits(pt.Text, st) {
				c.errf(pt.Sp, "E0080", "pattern `%s` does not fit in `%s`", pt.Text, st)
			}
		case st != nil && st.Kind == types.Bool:
			if pt.Kind != token.KindTrue && pt.Kind != token.KindFalse {
				c.errf(pt.Sp, "E0308", "expected a `bool` pattern (`true`/`false`), found %s", litPatKindName(pt.Kind))
			}
		case st != nil && st.Kind == types.Char:
			if pt.Kind != token.KindChar {
				c.errf(pt.Sp, "E0308", "expected a `char` pattern, found %s", litPatKindName(pt.Kind))
			}
		case st != nil && st.Kind == types.Str:
			if pt.Kind != token.KindString && pt.Kind != token.KindRawString {
				c.errf(pt.Sp, "E0308", "expected a string pattern, found %s", litPatKindName(pt.Kind))
			} else if pt.Kind == token.KindString {
				if _, ok := DecodeString(pt.Text); !ok {
					c.errf(pt.Sp, "E0001", "invalid string literal in pattern")
				}
			}
		default:
			c.errf(pt.Sp, "E0308", "literal patterns require an integer, `bool`, `char`, or `str` scrutinee, found `%s`", st)
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
			c.rejectNestedPat(sub)
			c.checkPattern(sub, v.Fields[i].Ty, borrowed)
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
			c.rejectNestedPat(sub)
			c.checkPattern(sub, f.Ty, borrowed)
		}
		return
	default:
		c.errf(p.Span(), "E0900", "this pattern is not supported by the compiler yet (roadmap M0)")
	}
}

// patternBinds reports whether p introduces a variable binding anywhere
// inside it (including struct-pattern field shorthand).
func patternBinds(p ast.Pattern) bool {
	switch pt := p.(type) {
	case *ast.BindPat:
		return true
	case *ast.TuplePat:
		if slices.ContainsFunc(pt.Elems, patternBinds) {
			return true
		}
	case *ast.OrPat:
		if slices.ContainsFunc(pt.Alts, patternBinds) {
			return true
		}
	case *ast.PathPat:
		if slices.ContainsFunc(pt.Sub, patternBinds) {
			return true
		}
		for _, fp := range pt.Fields {
			if fp.Pat == nil || patternBinds(fp.Pat) {
				return true
			}
		}
	}
	return false
}

// checkRangePat validates a `lo..hi` / `lo..=hi` pattern against the
// scrutinee type: endpoints must be literals of the matching kind, must fit,
// and must not describe an empty range.
func (c *checker) checkRangePat(pt *ast.RangePat, st *types.Type) {
	switch {
	case st != nil && st.Kind.IsInt():
		if pt.Lo.Kind != token.KindInteger || pt.Hi.Kind != token.KindInteger {
			c.errf(pt.Sp, "E0308", "range pattern endpoints must be integer literals for an integer scrutinee")
			return
		}
		for _, ep := range []*ast.LitPat{pt.Lo, pt.Hi} {
			if !intLitFits(ep.Text, st) {
				c.errf(ep.Sp, "E0080", "pattern `%s` does not fit in `%s`", ep.Text, st)
			}
		}
		if lo, ok1 := intLitValue(pt.Lo.Text); ok1 {
			if hi, ok2 := intLitValue(pt.Hi.Text); ok2 && rangeIsEmpty(lo, hi, pt.Inclusive) {
				c.errf(pt.Sp, "E0080", "range pattern `%s` is empty and can never match", rangePatText(pt))
			}
		}
	case st != nil && st.Kind == types.Char:
		if pt.Lo.Kind != token.KindChar || pt.Hi.Kind != token.KindChar {
			c.errf(pt.Sp, "E0308", "range pattern endpoints must be char literals for a `char` scrutinee")
			return
		}
		lo, hi := firstRune(pt.Lo.Text), firstRune(pt.Hi.Text)
		if rangeIsEmpty(int64(lo), int64(hi), pt.Inclusive) {
			c.errf(pt.Sp, "E0080", "range pattern `%s` is empty and can never match", rangePatText(pt))
		}
	default:
		c.errf(pt.Sp, "E0308", "range patterns require an integer or `char` scrutinee, found `%s`", st)
	}
}

// intLitValue parses an integer literal pattern's text, if it fits in an i64.
func intLitValue(text string) (int64, bool) {
	s := strings.ReplaceAll(text, "_", "")
	v, err := strconv.ParseInt(s, 0, 64)
	return v, err == nil
}

// rangeIsEmpty reports whether lo..hi (or lo..=hi) matches no value.
func rangeIsEmpty(lo, hi int64, inclusive bool) bool {
	if inclusive {
		return lo > hi
	}
	return lo >= hi
}

// rangePatText renders a range pattern for diagnostics.
func rangePatText(pt *ast.RangePat) string {
	op := ".."
	if pt.Inclusive {
		op = "..="
	}
	return pt.Lo.Text + op + pt.Hi.Text
}

// rejectNestedPat diagnoses refutable sub-patterns. Codegen discriminates only
// the outer variant, so a nested variant or literal pattern would silently pick
// the wrong arm (or emit a duplicate `case`), so we reject it for now.
func (c *checker) rejectNestedPat(p ast.Pattern) {
	switch p.(type) {
	case *ast.PathPat, *ast.LitPat, *ast.OrPat, *ast.RangePat:
		c.errf(p.Span(), "E0900", "nested patterns are not supported by the compiler yet (roadmap M0)")
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

// sliceElem returns the element type when t is a slice or a reference to one,
// and nil otherwise.
func sliceElem(t *types.Type) *types.Type {
	if t == nil {
		return nil
	}
	if t.Kind == types.Slice {
		return t.Elem
	}
	if t.Kind == types.Ref && t.Elem != nil && t.Elem.Kind == types.Slice {
		return t.Elem.Elem
	}
	return nil
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
	if base != nil && base.Kind == types.StringKind {
		return c.inferStringMethod(x)
	}
	if base != nil && base.Kind == types.VecKind {
		return c.inferVecMethod(x, base)
	}
	if base != nil && base.Kind == types.MapKind {
		if x.Name == "get" && !c.isPlace(x.Recv) && (rt == nil || (rt.Kind != types.Ref && rt.Kind != types.Ptr)) {
			c.errAt(x.Recv, "E0716", "temporary `HashMap` value dropped while borrowed; bind it to a variable first")
		}
		return c.inferMapMethod(x, base)
	}
	// `Option` and `Result` carry builtin methods: the tag tests and the
	// extractors. They cannot be `impl` blocks, because the prelude enums are
	// templates the checker builds itself.
	if base != nil && base.Kind == types.EnumKind && base.En != nil {
		switch nominalBaseName(base) {
		case "Result", "Option":
			if t, handled := c.inferPreludeMethod(x, base); handled {
				return t
			}
		}
	}
	// `len()` is also available directly on arrays, slices, and `str` values.
	if x.Name == "len" && len(x.Args) == 0 && base != nil && (base.Kind == types.Array || base.Kind == types.Str || base.Kind == types.Slice) {
		return types.UsizeT
	}
	// `str::as_bytes()` borrows the text as a byte slice, without copying.
	if x.Name == "as_bytes" && len(x.Args) == 0 && base != nil && base.Kind == types.Str {
		return &types.Type{Kind: types.Ref, Elem: &types.Type{Kind: types.Slice, Elem: types.U8T}}
	}
	if base == nil || (base.Kind != types.StructKind && base.Kind != types.EnumKind) {
		c.errAt(x, "E0599", "no method `%s` on type `%s`", x.Name, rt)
		for _, a := range x.Args {
			c.checkExpr(a, nil)
		}
		return nil
	}
	m, ok := c.info.Methods[nominalBaseName(base)][x.Name]
	if !ok {
		c.errAt(x, "E0599", "no method `%s` on type `%s`", x.Name, base)
		for _, a := range x.Args {
			c.checkExpr(a, nil)
		}
		return nil
	}
	if m.Generic {
		m = c.instantiateMethod(m, base)
	}
	if m.Self == nil {
		c.errAt(x, "E0599", "`%s::%s` takes no `self`, so it cannot be called with `.`", base, x.Name)
		for _, a := range x.Args {
			c.checkExpr(a, nil)
		}
		return m.Ret
	}
	if m.Self != nil && m.Self.ByRef && m.Self.Mut {
		c.checkMutable(x.Recv, x.Recv.Span(), "cannot borrow `%s` as mutable, as it is not declared `mut`")
	}
	// Taking `&self` from a value receiver is allowed (a temporary is
	// addressed); nothing to check here.
	c.info.Resolved[x] = m
	want := m.Params
	names, defaults := m.ParamNames, m.Defaults
	if m.Self != nil && len(want) > 0 {
		want = want[1:] // skip the receiver
		if len(names) > 0 {
			names, defaults = names[1:], defaults[1:]
		}
	}
	x.Args = c.resolveArgs(x.Sp, x.Args, names, defaults, fmt.Sprintf("method `%s::%s`", base, x.Name))
	if len(x.Args) != len(want) {
		c.errAt(x, "E0061", "method `%s::%s` takes %d argument(s) but %d were supplied", base, x.Name, len(want), len(x.Args))
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
	return m.Ret
}

func (c *checker) inferStructLit(x *ast.StructLitExpr, expected *types.Type) *types.Type {
	if len(x.Path) == 2 {
		return c.inferEnumStructLit(x, expected)
	}
	if len(x.Path) != 1 {
		c.errAt(x, "E0900", "path-qualified struct literals are not supported yet (roadmap M1)")
		return nil
	}
	tmpl, ok := c.info.Structs[x.Path[0]]
	if !ok {
		c.errAt(x, "E0412", "cannot find type `%s` in this scope", x.Path[0])
		for _, fi := range x.Fields {
			c.checkExpr(fi.Val, nil)
		}
		return nil
	}
	st := tmpl
	if tmpl.Generic {
		args, ok := c.inferStructArgs(x, tmpl, expected)
		if !ok {
			for _, fi := range x.Fields {
				c.checkExpr(fi.Val, nil)
			}
			return &types.Type{Kind: types.Invalid}
		}
		t := c.instantiateStruct(x.Sp, tmpl, args)
		if t.Kind != types.StructKind || t.St == nil {
			return &types.Type{Kind: types.Invalid}
		}
		st = t.St
	} else if len(x.TypeArgs) > 0 {
		c.errAt(x, "E0107", "`%s` is not generic", tmpl.Base)
	}
	return c.checkStructLitFields(x, st)
}

// checkStructLitFields checks a struct literal against a concrete struct.
func (c *checker) checkStructLitFields(x *ast.StructLitExpr, st *types.Struct) *types.Type {
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

// inferStructArgs infers a generic struct's arguments from turbofish, the
// expected type, and field initializers.
func (c *checker) inferStructArgs(x *ast.StructLitExpr, tmpl *types.Struct, expected *types.Type) ([]*types.Type, bool) {
	bindings := map[string]*types.Type{}
	if len(x.TypeArgs) > len(tmpl.Generics) {
		c.errAt(x, "E0107", "`%s` takes %d type argument(s) but %d were supplied", tmpl.Base, len(tmpl.Generics), len(x.TypeArgs))
	}
	for i, name := range tmpl.Generics {
		if i < len(x.TypeArgs) {
			if t := c.resolveType(x.TypeArgs[i]); t != nil {
				bindings[name] = t
			}
		}
	}
	if expected != nil && expected.Kind == types.StructKind && expected.St != nil && nominalBaseName(expected) == tmpl.Base {
		for i, name := range tmpl.Generics {
			if _, ok := bindings[name]; !ok && i < len(expected.St.Args) {
				bindConcrete(bindings, name, expected.St.Args[i])
			}
		}
	}
	if !allBound(tmpl.Generics, bindings) {
		for _, fi := range x.Fields {
			pattern := c.structFieldPattern(tmpl, fi.Name)
			if pattern == nil {
				continue
			}
			at := c.checkExpr(fi.Val, hintType(pattern))
			c.unify(pattern, at, bindings)
		}
	}
	args := c.bindGenericArgs(x.Sp, tmpl.Generics, bindings)
	return args, args != nil
}

// structFieldPattern resolves a template field's type with the template's type
// parameters in scope, for inference at a literal.
func (c *checker) structFieldPattern(tmpl *types.Struct, fieldName string) *types.Type {
	d := c.structDecls[tmpl.Name]
	if d == nil {
		return nil
	}
	var out *types.Type
	c.withTypeParams(tmpl.Generics, func() {
		for i, f := range d.Fields {
			name := f.Name
			if d.Tuple {
				name = strconv.Itoa(i)
			}
			if name == fieldName {
				out = c.resolveType(f.Ty)
				return
			}
		}
	})
	return out
}

// inferEnumStructLit checks `Enum::Variant { field: value, .. }`.
func (c *checker) inferEnumStructLit(x *ast.StructLitExpr, expected *types.Type) *types.Type {
	tmpl, ok := c.info.Enums[x.Path[0]]
	if !ok {
		c.errAt(x, "E0412", "cannot find type `%s` in this scope", x.Path[0])
		for _, fi := range x.Fields {
			c.checkExpr(fi.Val, nil)
		}
		return nil
	}
	en := tmpl
	if tmpl.Generic {
		bindings := map[string]*types.Type{}
		if len(x.TypeArgs) > len(tmpl.Generics) {
			c.errAt(x, "E0107", "`%s` takes %d type argument(s) but %d were supplied", tmpl.Base, len(tmpl.Generics), len(x.TypeArgs))
		}
		for i, name := range tmpl.Generics {
			if i < len(x.TypeArgs) {
				if t := c.resolveType(x.TypeArgs[i]); t != nil {
					bindings[name] = t
				}
			}
		}
		if expected != nil && expected.Kind == types.EnumKind && expected.En != nil && nominalBaseName(expected) == tmpl.Base {
			for i, name := range tmpl.Generics {
				if _, ok := bindings[name]; !ok && i < len(expected.En.Args) {
					bindConcrete(bindings, name, expected.En.Args[i])
				}
			}
		}
		if !allBound(tmpl.Generics, bindings) {
			for _, fi := range x.Fields {
				pattern := c.enumFieldPattern(tmpl, x.Path[1], fi.Name)
				if pattern == nil {
					continue
				}
				at := c.checkExpr(fi.Val, hintType(pattern))
				c.unify(pattern, at, bindings)
			}
		}
		args := c.bindGenericArgs(x.Sp, tmpl.Generics, bindings)
		if args == nil {
			for _, fi := range x.Fields {
				c.checkExpr(fi.Val, nil)
			}
			return &types.Type{Kind: types.Invalid}
		}
		t := c.instantiateEnum(x.Sp, tmpl, args)
		if t.Kind != types.EnumKind || t.En == nil {
			return &types.Type{Kind: types.Invalid}
		}
		en = t.En
	} else if len(x.TypeArgs) > 0 {
		c.errAt(x, "E0107", "`%s` is not generic", tmpl.Base)
	}
	return c.checkEnumStructLitFields(x, en)
}

// checkEnumStructLitFields checks an enum struct-variant literal.
func (c *checker) checkEnumStructLitFields(x *ast.StructLitExpr, en *types.Enum) *types.Type {
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

// formatLiteral returns the decoded format string when a call's first argument
// is a string literal containing at least one `{}` placeholder. The remaining
// arguments are then the values to interpolate.
func formatLiteral(args []ast.Expr) (string, bool) {
	if len(args) == 0 {
		return "", false
	}
	lit, ok := args[0].(*ast.LiteralExpr)
	if !ok {
		return "", false
	}
	switch lit.Kind {
	case token.KindString:
		decoded, ok := DecodeString(lit.Text)
		if !ok {
			return "", false
		}
		return decoded, strings.Contains(decoded, "{}")
	case token.KindRawString:
		return lit.Text, strings.Contains(lit.Text, "{}")
	}
	return "", false
}

// countFormatArgs returns the number of `{}` placeholders in a format string.
func countFormatArgs(format string) int {
	return strings.Count(format, "{}")
}

// inferPanic types `panic(msg)`: it never returns, so its type is `never` and it
// is usable anywhere a value is expected — the reason it can stand in for an
// arm body in the match that `unwrap`/`expect` desugar to.
func (c *checker) inferPanic(x *ast.CallExpr) *types.Type {
	switch len(x.Args) {
	case 0:
		return types.NeverT
	case 1:
		at := c.checkExpr(x.Args[0], nil)
		if !isPrintable(at) {
			c.errAt(x.Args[0], "E0277", "`%s` cannot be printed", at)
		}
		return types.NeverT
	default:
		c.errAt(x, "E0061", "`panic` takes at most one argument but %d were supplied", len(x.Args))
		for _, a := range x.Args {
			c.checkExpr(a, nil)
		}
		return types.NeverT
	}
}

func (c *checker) inferPrint(x *ast.CallExpr) *types.Type {
	if format, ok := formatLiteral(x.Args); ok {
		want := countFormatArgs(format)
		if want != len(x.Args)-1 {
			c.errAt(x, "E0061", "format string has %d placeholder(s) but %d argument(s) were supplied", want, len(x.Args)-1)
		}
		for _, a := range x.Args[1:] {
			at := c.checkExpr(a, nil)
			if !isPrintable(at) {
				c.errAt(a, "E0277", "`%s` cannot be printed", at)
			}
		}
		return types.UnitT
	}
	for _, a := range x.Args {
		at := c.checkExpr(a, nil)
		if !isPrintable(at) {
			c.errAt(a, "E0277", "`%s` cannot be printed", at)
		}
	}
	return types.UnitT
}

func (c *checker) inferIf(x *ast.IfExpr, expected *types.Type) *types.Type {
	ct := c.checkExpr(x.Cond, types.BoolT)
	if ct.Kind != types.Bool {
		c.errAt(x.Cond, "E0308", "`if` condition must be `bool`, found `%s`", ct)
	}
	thenT := c.checkBlockWith(x.Then, expected)
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

func (c *checker) inferLoop(x *ast.LoopExpr, expected *types.Type) *types.Type {
	switch x.Kind {
	case token.KindLoop:
		bi := c.pushBreak(token.KindLoop, expected)
		c.loopDepth++
		c.checkBlock(x.Body)
		c.loopDepth--
		c.popBreak()
		if bi.hasVal {
			return bi.val
		}
		if bi.hasBare {
			return types.UnitT
		}
		// No `break` at all: the loop can only be left by `return` (or by
		// running forever), so its type is the never type.
		return types.NeverT
	case token.KindWhile, token.KindUntil:
		c.pushBreak(x.Kind, nil)
		ct := c.checkExpr(x.Cond, types.BoolT)
		if ct.Kind != types.Bool {
			c.errAt(x.Cond, "E0308", "`%s` condition must be `bool`, found `%s`", loopKindName(x.Kind), ct)
		}
		c.loopDepth++
		c.checkBlock(x.Body)
		c.loopDepth--
		c.popBreak()
		return types.UnitT
	case token.KindFor:
		return c.inferFor(x)
	}
	return types.UnitT
}

// pushBreak enters a loop body for the purpose of `break` value checking.
func (c *checker) pushBreak(kind token.Kind, want *types.Type) *breakInfo {
	bi := &breakInfo{kind: kind, want: want}
	c.breaks = append(c.breaks, bi)
	return bi
}

func (c *checker) popBreak() {
	if len(c.breaks) > 0 {
		c.breaks = c.breaks[:len(c.breaks)-1]
	}
}

func (c *checker) inferFor(x *ast.LoopExpr) *types.Type {
	c.pushBreak(token.KindFor, nil)
	defer c.popBreak()
	rng, ok := x.Iter.(*ast.BinExpr)
	if !ok || (rng.Op != token.KindDotDot && rng.Op != token.KindDotDotEq) {
		// `for x in arr` iterates an array by value. Elements must be `Copy`,
		// otherwise the loop binding would alias (and possibly double-free) an
		// owned element.
		it := c.checkExpr(x.Iter, nil)
		if it != nil && it.Kind == types.Array {
			if !it.Elem.IsCopy() {
				c.errAt(x.Iter, "E0507", "cannot iterate an array of non-`Copy` element type `%s` by value", it.Elem)
			}
			c.pushScope()
			c.bindPattern(x.Pat, it.Elem)
			c.loopDepth++
			c.checkBlock(x.Body)
			c.loopDepth--
			c.popScope()
			return types.UnitT
		}
		if elem := sliceElem(it); elem != nil {
			// A slice yields `Copy` elements by value, like an array.
			if !elem.IsCopy() {
				c.errAt(x.Iter, "E0507", "cannot iterate a slice of non-`Copy` element type `%s` by value", elem)
			}
			c.pushScope()
			c.bindPattern(x.Pat, elem)
			c.loopDepth++
			c.checkBlock(x.Body)
			c.loopDepth--
			c.popScope()
			return types.UnitT
		}
		if base := recvBase(it); base != nil && base.Kind == types.VecKind {
			// As with arrays, elements must be `Copy`: the binding is a copy
			// of each element, and an owned element would be dropped twice.
			if !base.Elem.IsCopy() {
				c.errAt(x.Iter, "E0507", "cannot iterate a `Vec` of non-`Copy` element type `%s` by value", base.Elem)
			}
			c.pushScope()
			c.bindPattern(x.Pat, base.Elem)
			c.loopDepth++
			c.checkBlock(x.Body)
			c.loopDepth--
			c.popScope()
			return types.UnitT
		}
		c.errAt(x.Iter, "E0900", "`for` iterates an integer range (`a..b`), an array, or a `Vec`")
		c.loopDepth++
		c.checkBlock(x.Body)
		c.loopDepth--
		return types.UnitT
	}
	lt := c.checkExpr(rng.L, nil)
	rt := c.checkExpr(rng.R, nil)
	if c.adoptInt(lt, rt) == nil {
		c.checkExpr(rng.R, lt)
	}
	if !lt.Kind.IsInt() {
		c.errAt(x.Iter, "E0308", "range bounds must be integers, found `%s`", lt)
		lt = types.I32T
	}
	c.pushScope()
	c.bindPattern(x.Pat, lt)
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
		if c.cur.Ret.Kind == types.Unit {
			if vt != nil && vt.Kind != types.Unit && vt.Kind != types.Never {
				c.errAt(x.Val, "E0308", "this function returns `()`, so `return` cannot take a value")
			}
		} else if !c.assignable(c.cur.Ret, vt) {
			c.errAt(x.Val, "E0308", "mismatched types: expected `%s`, found `%s`", c.cur.Ret, vt)
		}
		return types.NeverT
	case token.KindBreak:
		if c.loopDepth == 0 {
			c.errf(x.Sp, "E0268", "`break` outside of a loop")
			if x.Val != nil {
				c.checkExpr(x.Val, nil)
			}
			return types.NeverT
		}
		if len(c.breaks) == 0 {
			if x.Val != nil {
				c.checkExpr(x.Val, nil)
			}
			return types.NeverT
		}
		bi := c.breaks[len(c.breaks)-1]
		if bi.kind != token.KindLoop {
			if x.Val != nil {
				c.errAt(x.Val, "E0308", "`%s` loops do not produce a value; use `break` without a value", loopKindName(bi.kind))
				c.checkExpr(x.Val, nil)
			}
			return types.NeverT
		}
		var want *types.Type
		if bi.hasVal {
			want = bi.val
		} else {
			want = bi.want
		}
		if x.Val == nil {
			if bi.hasVal {
				c.errf(x.Sp, "E0308", "`break` must carry a value of type `%s`", bi.val)
			} else {
				bi.hasBare = true
			}
			return types.NeverT
		}
		vt := c.checkExpr(x.Val, want)
		if bi.hasVal {
			if vt != nil && !c.assignable(bi.val, vt) {
				c.errAt(x.Val, "E0308", "mismatched types in `break`: expected `%s`, found `%s`", bi.val, vt)
			}
		} else {
			bi.val = vt
			bi.hasVal = true
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

// loopKindName names a loop keyword for diagnostics.
func loopKindName(k token.Kind) string {
	switch k {
	case token.KindWhile:
		return "while"
	case token.KindUntil:
		return "until"
	case token.KindFor:
		return "for"
	}
	return "loop"
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
		if len(x.Segments) == 1 {
			if t, ok := c.typeEnv[x.Segments[0]]; ok {
				if len(x.Args) > 0 {
					c.errf(x.Sp, "E0107", "type parameter `%s` cannot take type arguments", x.Segments[0])
				}
				return t
			}
		}
		if len(x.Segments) > 1 {
			c.errf(x.Sp, "E0900", "type paths are not supported by the compiler yet (roadmap M1)")
			return nil
		}
		name := x.Segments[0]
		if name == "Vec" {
			return c.resolveVecType(x.Sp, x.Args)
		}
		if name == "HashMap" {
			return c.resolveMapType(x.Sp, x.Args)
		}
		if len(x.Args) > 0 {
			args := make([]*types.Type, len(x.Args))
			for i, a := range x.Args {
				args[i] = c.resolveType(a)
			}
			if st, ok := c.info.Structs[name]; ok {
				return c.applyStructArgs(x.Sp, st, args)
			}
			if en, ok := c.info.Enums[name]; ok {
				return c.applyEnumArgs(x.Sp, en, args)
			}
			if _, ok := c.aliasDecls[name]; ok {
				c.errf(x.Sp, "E0107", "type alias `%s` is not generic", name)
				return c.resolveAlias(name, x.Sp)
			}
			c.errf(x.Sp, "E0412", "cannot find type `%s` in this scope", name)
			return nil
		}
		if bt, ok := types.Builtins[name]; ok {
			return bt
		}
		if st, ok := c.info.Structs[name]; ok {
			if st.Generic {
				c.errf(x.Sp, "E0107", "`%s` expects %d type argument(s)", st.Base, len(st.Generics))
			}
			return &types.Type{Kind: types.StructKind, St: st}
		}
		if en, ok := c.info.Enums[name]; ok {
			if en.Generic {
				c.errf(x.Sp, "E0107", "`%s` expects %d type argument(s)", en.Base, len(en.Generics))
			}
			return &types.Type{Kind: types.EnumKind, En: en}
		}
		if _, ok := c.aliasDecls[name]; ok {
			return c.resolveAlias(name, x.Sp)
		}
		c.errf(x.Sp, "E0412", "cannot find type `%s` in this scope", name)
		return nil
	case *ast.RefType:
		c.indirect++
		elem := c.resolveType(x.Elem)
		c.indirect--
		return types.RefTo(elem, x.Mut)
	case *ast.PtrType:
		c.indirect++
		elem := c.resolveType(x.Elem)
		c.indirect--
		return types.PtrTo(elem)
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
		if n.Int < 0 {
			c.errf(x.Sp, "E0080", "array length must be non-negative, found `%d`", n.Int)
			return &types.Type{Kind: types.Array, Elem: elem}
		}
		if n.Int > maxArrayLen {
			c.errf(x.Sp, "E0080", "array length `%d` is too large (maximum %d)", n.Int, maxArrayLen)
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
	if dst.Kind == types.Ref && src.Kind == types.Ref && !dst.Mut && src.Mut {
		return types.Equal(dst.Elem, src.Elem)
	}
	// Unsizing: a borrow of a fixed array is a slice of the same elements.
	// `&mut [T; N]` also satisfies `&[T]`; the other direction does not hold.
	if dst.Kind == types.Ref && src.Kind == types.Ref && dst.Elem != nil && src.Elem != nil &&
		dst.Elem.Kind == types.Slice && src.Elem.Kind == types.Array &&
		types.Equal(dst.Elem.Elem, src.Elem.Elem) {
		return !dst.Mut || src.Mut
	}
	// A slice value may be passed where a borrowed slice is expected: both are
	// a non-owning view, so taking the borrow is a no-op.
	if dst.Kind == types.Ref && dst.Elem != nil && dst.Elem.Kind == types.Slice &&
		src.Kind == types.Slice && types.Equal(dst.Elem, src) {
		return !dst.Mut
	}
	return false
}

// argAssignable is `assignable` plus the borrowed-`str` coercion: an owning
// `String` may be passed where a `str` is expected, because the callee only
// borrows it for the duration of the call. It is deliberately *not* used for
// `let`/field/return positions, where a longer-lived borrow from a temporary
// could dangle.
//
// ptrRefOK enables the pointer/reference interchange: at the C boundary a
// reference and a raw pointer are the same machine pointer, so either may
// be passed where the other is expected (with matching mutability). This
// keeps `extern "C"` declarations pleasant — `time(&mut slot)` just works —
// but it must stay confined to extern call sites: a raw `*T` (which an
// extern function may return null) would otherwise become a `&T` with no
// `unsafe`, removing the guarantee that a reference is valid.
func (c *checker) argAssignable(want, got *types.Type, ptrRefOK bool) bool {
	if c.assignable(want, got) {
		return true
	}
	if want != nil && want.Kind == types.Str && got != nil && got.Kind == types.StringKind {
		return true
	}
	if ptrRefOK && want != nil && got != nil &&
		(want.Kind == types.Ptr || want.Kind == types.Ref) &&
		(got.Kind == types.Ptr || got.Kind == types.Ref) {
		if want.Mut && !got.Mut {
			return false
		}
		return types.Equal(want.Elem, got.Elem)
	}
	return false
}

// --- scope helpers --------------------------------------------------------

func (c *checker) pushScope() {
	c.scope = append(c.scope, map[string]*types.Type{})
	c.mutScope = append(c.mutScope, map[string]bool{})
}
func (c *checker) popScope() {
	if len(c.scope) > 0 {
		c.scope = c.scope[:len(c.scope)-1]
	}
	if len(c.mutScope) > 0 {
		c.mutScope = c.mutScope[:len(c.mutScope)-1]
	}
}

// markMut records whether a `let`/`for` binding is mutable. Names that are
// never marked (parameters, `self`, match bindings) are treated as mutable so
// the check only ever rejects code we are certain about.
func (c *checker) markMut(name string, mut bool) {
	if name == "" || name == "_" || len(c.mutScope) == 0 {
		return
	}
	c.mutScope[len(c.mutScope)-1][name] = mut
}

// isMut reports whether binding `name` may be assigned to or mutably borrowed.
func (c *checker) isMut(name string) bool {
	for i := len(c.mutScope) - 1; i >= 0; i-- {
		if m, ok := c.mutScope[i][name]; ok {
			return m
		}
	}
	return true
}

// lookupType resolves the declared type of a local binding, if any.
func (c *checker) lookupType(name string) *types.Type {
	for i := len(c.scope) - 1; i >= 0; i-- {
		if t, ok := c.scope[i][name]; ok {
			return t
		}
	}
	return nil
}

// mutRoot returns the identifier at the root of a field/index access chain, so
// `a.b[i].c` yields "a". A dereference stops the walk (the mutability of a
// pointee is governed by the reference, not by the binding), in which case it
// returns "".
func mutRoot(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.IdentExpr:
		if x.Name == "_" {
			return ""
		}
		return x.Name
	case *ast.FieldExpr:
		return mutRoot(x.Recv)
	case *ast.IndexExpr:
		return mutRoot(x.Recv)
	case *ast.SliceExpr:
		return mutRoot(x.Recv)
	}
	return ""
}

// checkMutable reports an error if `e` is rooted at a binding that is not
// `mut` and is not already a mutable reference or pointer. `format` is the
// diagnostic message, with one `%s` for the binding name.
func (c *checker) checkMutable(e ast.Expr, sp token.Span, format string) {
	name := mutRoot(e)
	if name == "" || c.isMut(name) {
		return
	}
	if t := c.lookupType(name); t != nil && (t.Kind == types.Ptr || (t.Kind == types.Ref && t.Mut)) {
		return // the binding is an immutable handle to mutable data
	}
	c.errf(sp, "E0596", format, name)
}

func (c *checker) declare(name string, ty *types.Type) {
	if len(c.scope) == 0 {
		return
	}
	// Shadowing and re-declaration are both allowed: a new binding simply
	// replaces the previous one in the current scope.
	c.scope[len(c.scope)-1][name] = ty
}

// --- helpers --------------------------------------------------------------

// maxArrayLen bounds array lengths. Array literals are expanded element by
// element into C, so an enormous length would exhaust the compiler's memory
// long before the C compiler saw it.
const maxArrayLen = 1 << 20

func isIntLiteral(e ast.Expr) bool {
	l, ok := e.(*ast.LiteralExpr)
	return ok && l.Kind == token.KindInteger
}

// intLiteralExpr reports whether e is a pure integer-literal expression: an
// integer literal, a unary minus on one, or two of them combined with a binary
// operator. Only the value of such an expression is fixed; its type is still
// pending, so it can adopt a numeric type from the surrounding context.
func intLiteralExpr(e ast.Expr) bool {
	switch n := e.(type) {
	case *ast.LiteralExpr:
		return n.Kind == token.KindInteger
	case *ast.UnExpr:
		return n.Op == token.KindMinus && intLiteralExpr(n.X)
	case *ast.BinExpr:
		return intLiteralExpr(n.L) && intLiteralExpr(n.R)
	}
	return false
}

// intValFits reports whether a folded integer constant fits in the integer type
// t. Unlike intLitFits it works on a computed value, so it also covers negated
// and combined constants (`const X: u8 = 200 + 100`).
func intValFits(v ConstVal, t *types.Type) bool {
	var bits uint
	switch t.Kind {
	case types.I8, types.U8:
		bits = 8
	case types.I16, types.U16:
		bits = 16
	case types.I32, types.U32:
		bits = 32
	case types.I64, types.Isize, types.U64, types.Usize:
		bits = 64
	default:
		return true
	}
	if t.Kind.IsUnsigned() {
		if v.Int < 0 {
			return false
		}
		return bits == 64 || v.Uint <= (uint64(1)<<bits)-1
	}
	if bits == 64 {
		return true
	}
	min := -(int64(1) << (bits - 1))
	max := (int64(1) << (bits - 1)) - 1
	return v.Int >= min && v.Int <= max
}

// intLitFits reports whether the integer literal text fits in the integer type
// t. It ignores underscores and accepts decimal/hex/binary/octal forms.
func intLitFits(text string, t *types.Type) bool {
	if t == nil || !t.Kind.IsInt() {
		return true
	}
	s := strings.ReplaceAll(text, "_", "")
	var bits uint
	switch t.Kind {
	case types.I8, types.U8:
		bits = 8
	case types.I16, types.U16:
		bits = 16
	case types.I32, types.U32:
		bits = 32
	case types.I64, types.Isize, types.U64, types.Usize:
		bits = 64
	default:
		return true
	}
	if t.Kind.IsUnsigned() {
		if u, err := strconv.ParseUint(s, 0, 64); err == nil {
			return bits == 64 || u <= (uint64(1)<<bits)-1
		}
		return false
	}
	i, err := strconv.ParseInt(s, 0, 64)
	if err != nil {
		return false
	}
	if bits == 64 {
		return true
	}
	min := -(int64(1) << (bits - 1))
	max := (int64(1) << (bits - 1)) - 1
	return i >= min && i <= max
}

func isPrintable(t *types.Type) bool {
	if t == nil {
		return false
	}
	switch t.Kind {
	case types.Str, types.Char, types.Bool, types.StringKind:
		return true
	}
	return t.Kind.IsNumeric()
}

// lookupScope returns the type of a local binding, or nil.
func (c *checker) lookupScope(name string) *types.Type {
	for i := len(c.scope) - 1; i >= 0; i-- {
		if t, ok := c.scope[i][name]; ok {
			return t
		}
	}
	return nil
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
	Raw     bool // a raw string literal: Str holds undecoded source text
	IsBool  bool
	// Untyped marks an integer value that came from a bare literal (defaulting
	// to `i32`) and may still adopt an explicit integer type.
	Untyped bool
}

// constLiteralFor materializes a folded constant as a literal expression
// of the given parameter type, for use as a call-site default argument.
// It returns nil when the value cannot be a literal of that type.
func constLiteralFor(cv ConstVal, pt *types.Type, sp token.Span) ast.Expr {
	invalid := pt == nil || pt.Kind == types.Invalid
	switch {
	case cv.IsStr:
		if !invalid && pt.Kind != types.Str && pt.Kind != types.StringKind {
			return nil
		}
		text := cv.Str
		if cv.Raw {
			text = escapeStringLit(cv.Str)
		}
		return &ast.LiteralExpr{Kind: token.KindString, Text: text, Sp: sp}
	case cv.IsBool:
		if !invalid && pt.Kind != types.Bool {
			return nil
		}
		kind := token.KindFalse
		if cv.Bool {
			kind = token.KindTrue
		}
		return &ast.LiteralExpr{Kind: kind, Sp: sp}
	case cv.IsFloat:
		if !invalid && !pt.Kind.IsFloat() {
			return nil
		}
		return &ast.LiteralExpr{Kind: token.KindFloat, Text: strconv.FormatFloat(cv.Float, 'g', -1, 64), Sp: sp}
	default:
		if !invalid && pt.Kind == types.Char {
			// Check the wide value before narrowing to rune: converting first
			// would truncate, and a truncated value could look valid.
			if cv.Int < 0 || cv.Int > utf8.MaxRune {
				return nil
			}
			r := rune(cv.Int)
			if !utf8.ValidRune(r) {
				return nil
			}
			return &ast.LiteralExpr{Kind: token.KindChar, Text: escapeCharLit(r), Sp: sp}
		}
		if !invalid && !pt.Kind.IsInt() {
			return nil
		}
		text := strconv.FormatInt(cv.Int, 10)
		if !invalid && pt.Kind.IsUnsigned() {
			text = strconv.FormatUint(cv.Uint, 10)
		}
		return &ast.LiteralExpr{Kind: token.KindInteger, Text: text, Sp: sp}
	}
}

// escapeStringLit renders a decoded string as the inner text of a string
// literal (without the surrounding quotes).
func escapeStringLit(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString("\\\"")
		case '\\':
			b.WriteString("\\\\")
		case '\n':
			b.WriteString("\\n")
		case '\t':
			b.WriteString("\\t")
		case '\r':
			b.WriteString("\\r")
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, "\\u{%X}", r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// escapeCharLit renders a rune as the inner text of a char literal.
func escapeCharLit(r rune) string {
	switch r {
	case '\'':
		return "\\'"
	case '\\':
		return "\\\\"
	case '\n':
		return "\\n"
	case '\t':
		return "\\t"
	case '\r':
		return "\\r"
	}
	if r < 0x20 {
		return fmt.Sprintf("\\u{%X}", r)
	}
	return string(r)
}

// evalConst folds a constant expression, supporting literals, unary minus, and
// the arithmetic and bitwise binary operators over integer and float values.
func (c *checker) evalConst(e ast.Expr) (ConstVal, bool) {
	switch x := e.(type) {
	case *ast.LiteralExpr:
		return c.evalConstLiteral(x)
	case *ast.IdentExpr:
		if cn, ok := c.info.Consts[x.Name]; ok {
			return cn.Val, true
		}
		return ConstVal{}, false
	case *ast.PathExpr:
		if len(x.Segments) == 1 {
			if cn, ok := c.info.Consts[x.Segments[0]]; ok {
				return cn.Val, true
			}
		}
		return ConstVal{}, false
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
			// A magnitude that becomes representable after negation (e.g.
			// `-9223372036854775808`) is a plain integer again, so it can adopt
			// a declared type.
			if v.Int >= -2147483648 && v.Int <= 2147483647 {
				v.Ty = types.I32T
			} else {
				v.Ty = types.I64T
			}
			v.Untyped = true
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
			return ConstVal{Ty: types.I32T, Int: i, Uint: uint64(i), Untyped: true}, true
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
	case token.KindString:
		return ConstVal{Ty: types.StrT, Str: x.Text, IsStr: true}, true
	case token.KindRawString:
		return ConstVal{Ty: types.StrT, Str: x.Text, IsStr: true, Raw: true}, true
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
	return ConstVal{Ty: types.I32T, Int: out, Uint: uint64(out), Untyped: l.Untyped && r.Untyped}, true
}

// firstRune decodes the first decoded rune of a char literal's inner text.
func firstRune(s string) rune {
	r, _ := decodeCharInner(s)
	return r
}

// decodeCharLiteral decodes the inner text of a char or byte literal,
// requiring exactly one character: a single rune, or one escape sequence.
// (A multi-character or empty literal used to be silently truncated to
// its first byte.)
func decodeCharLiteral(s string) (rune, bool) {
	if s == "" {
		return 0, false
	}
	if s[0] != '\\' {
		r, size := utf8.DecodeRuneInString(s)
		return r, size == len(s)
	}
	r, ok := decodeEscape(s[1:])
	if !ok {
		return 0, false
	}
	return r, 1+escapeLen(s[1:]) == len(s)
}

// escapeLen returns the length of the escape sequence at the start of s,
// where s is the text following the backslash.
func escapeLen(s string) int {
	if s == "" {
		return 0
	}
	switch s[0] {
	case 'x':
		if len(s) >= 3 {
			return 3
		}
		return len(s)
	case 'u':
		if i := strings.IndexByte(s, '}'); i >= 0 {
			return i + 1
		}
		return len(s)
	}
	return 1
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
					// Only Unicode scalar values are valid `char`s.
					if v <= 0x10FFFF && (v < 0xD800 || v > 0xDFFF) {
						return rune(v), true
					}
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
