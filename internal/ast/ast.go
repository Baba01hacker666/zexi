// Package ast defines Zexi's abstract syntax tree. Nodes are intentionally
// faithful to source: no desugaring happens here. Every node carries a span so
// diagnostics, formatting, and a future language server can point at source.
//
// M0 scope: the full item grammar and the expression/statement node set are
// declared so the parser, checker, and lowerings can be filled in
// incrementally. See docs/02 for the grammar this mirrors.
package ast

import "zexi/internal/token"

// Node is implemented by every AST node.
type Node interface {
	Span() token.Span
}

// File is one source module.
type File struct {
	Items []Item
	Sp    token.Span
}

func (f *File) Span() token.Span { return f.Sp }

// --- items ----------------------------------------------------------------

// Item is a top-level declaration.
type Item interface {
	Node
	itemNode()
}

// Visibility records `pub`, `pub(crate)`, etc. M0 only distinguishes pub/not.
type Visibility bool

// FnDecl is a function or method declaration.
type FnDecl struct {
	Vis      Visibility
	Unsafe   bool
	Extern   string // "" unless `extern "C"`, which is stored here
	Name     string
	Generics []GenericParam
	Params   []Param
	Ret      Type
	Body     *Block // nil for `extern` declarations (no body)
	Sp       token.Span
}

func (*FnDecl) itemNode()          {}
func (d *FnDecl) Span() token.Span { return d.Sp }

// Param is a function parameter.
type Param struct {
	Mut  bool
	Name string
	Ty   Type
	// Default is the optional default value (`name: i32 = 42`). It must be
	// a constant expression; calls may omit trailing defaulted arguments.
	Default Expr
	// Self marks the receiver parameter of a method. When set, Ty is nil for
	// `self`, or a reference type for `&self` / `&mut self`.
	Self    bool
	SelfRef bool
	SelfMut bool
	Sp      token.Span
}

// GenericParam is a type parameter with optional trait bounds.
type GenericParam struct {
	Name   string
	Bounds []Type // trait bounds; empty means unconstrained
	Sp     token.Span
}

// StructDecl is a struct definition.
type StructDecl struct {
	Vis    Visibility
	Attrs  []string
	Name   string
	Fields []Field
	Tuple  bool // true for `struct S(T, U);`
	Sp     token.Span
}

func (*StructDecl) itemNode()          {}
func (d *StructDecl) Span() token.Span { return d.Sp }

// Field is a named struct field.
type Field struct {
	Vis  Visibility
	Name string
	Ty   Type
	Sp   token.Span
}

// EnumDecl is a tagged-union definition.
type EnumDecl struct {
	Vis      Visibility
	Attrs    []string
	Name     string
	Variants []Variant
	Sp       token.Span
}

func (*EnumDecl) itemNode()          {}
func (d *EnumDecl) Span() token.Span { return d.Sp }

// Variant is one enum case: unit, tuple, or struct-like.
type Variant struct {
	Name   string
	Fields []Field // used for tuple and struct variants
	Tup    bool
	Sp     token.Span
}

// ConstDecl is a named constant.
type ConstDecl struct {
	Vis  Visibility
	Name string
	Ty   Type // may be nil; inferred from the value
	Val  Expr
	Sp   token.Span
}

func (*ConstDecl) itemNode()          {}
func (d *ConstDecl) Span() token.Span { return d.Sp }

// TypeAlias is `type Name = Ty;`.
type TypeAlias struct {
	Vis  Visibility
	Name string
	Ty   Type
	Sp   token.Span
}

func (*TypeAlias) itemNode()          {}
func (d *TypeAlias) Span() token.Span { return d.Sp }

// ModDecl is `mod name;` or `mod name { ... }`.
type ModDecl struct {
	Vis   Visibility
	Name  string
	Items []Item // nil when the module is a separate file
	Sp    token.Span
}

func (*ModDecl) itemNode()          {}
func (d *ModDecl) Span() token.Span { return d.Sp }

// UseDecl is `use path;` or `use "file.zx";`. The driver resolves it to
// source files (a library import) before type checking.
type UseDecl struct {
	Path []string
	// File is the literal path of `use "file.zx";`, resolved relative to
	// the importing file. Empty for the `use a::b;` form.
	File string
	As   string
	Sp   token.Span
}

func (*UseDecl) itemNode()          {}
func (d *UseDecl) Span() token.Span { return d.Sp }

// ImplBlock is `impl Type { .. }` or `impl Trait for Type { .. }`.
type ImplBlock struct {
	SelfTy Type
	Trait  Type // nil for inherent impls
	Fns    []*FnDecl
	Sp     token.Span
}

func (*ImplBlock) itemNode()          {}
func (d *ImplBlock) Span() token.Span { return d.Sp }

// --- types ----------------------------------------------------------------

// Type is a syntactic type. Distinct from the semantic type in package types.
type Type interface {
	Node
	typeNode()
}

// PathType is a named type, possibly with generic arguments: `Vec<i32>`.
type PathType struct {
	Segments []string
	Args     []Type
	Sp       token.Span
}

func (*PathType) typeNode()          {}
func (t *PathType) Span() token.Span { return t.Sp }

// RefType is `&T` or `&mut T`.
type RefType struct {
	Mut  bool
	Elem Type
	Sp   token.Span
}

func (*RefType) typeNode()          {}
func (t *RefType) Span() token.Span { return t.Sp }

// PtrType is `*T` or `*mut T` (unsafe-only).
type PtrType struct {
	Mut  bool
	Elem Type
	Sp   token.Span
}

func (*PtrType) typeNode()          {}
func (t *PtrType) Span() token.Span { return t.Sp }

// ArrayType is `[T; N]`.
type ArrayType struct {
	Elem Type
	Len  Expr
	Sp   token.Span
}

func (*ArrayType) typeNode()          {}
func (t *ArrayType) Span() token.Span { return t.Sp }

// SliceType is `[T]`.
type SliceType struct {
	Elem Type
	Sp   token.Span
}

func (*SliceType) typeNode()          {}
func (t *SliceType) Span() token.Span { return t.Sp }

// TupleType is `(A, B, ...)`.
type TupleType struct {
	Elems []Type
	Sp    token.Span
}

func (*TupleType) typeNode()          {}
func (t *TupleType) Span() token.Span { return t.Sp }

// FnType is `fn(A, B) -> R`.
type FnType struct {
	Params []Type
	Ret    Type
	Sp     token.Span
}

func (*FnType) typeNode()          {}
func (t *FnType) Span() token.Span { return t.Sp }

// --- statements -----------------------------------------------------------

// Stmt is a statement.
type Stmt interface {
	Node
	stmtNode()
}

// LetStmt is `let pat: T = expr;`.
type LetStmt struct {
	Mut bool
	Pat Pattern
	Ty  Type // may be nil
	Val Expr // may be nil (uninitialized binding)
	Sp  token.Span
}

func (*LetStmt) stmtNode()          {}
func (s *LetStmt) Span() token.Span { return s.Sp }

// ExprStmt wraps an expression used as a statement. A trailing expression in a
// block is not wrapped; it is the block's value.
type ExprStmt struct {
	X  Expr
	Sp token.Span
}

func (*ExprStmt) stmtNode()          {}
func (s *ExprStmt) Span() token.Span { return s.Sp }

// DeferStmt is `defer expr;`: the expression is evaluated when the enclosing
// block scope exits — by falling off the end, or via `return`, `break`, or
// `continue` — after the deferred expressions registered later in the same
// scope (LIFO) and before the scope's locals are dropped.
type DeferStmt struct {
	X  Expr
	Sp token.Span
}

func (*DeferStmt) stmtNode()          {}
func (s *DeferStmt) Span() token.Span { return s.Sp }

// --- expressions ----------------------------------------------------------

// Expr is an expression.
type Expr interface {
	Node
	exprNode()
}

// Block is `{ stmts; tail }`, itself an expression.
type Block struct {
	Stmts []Stmt
	Tail  Expr // may be nil (block evaluates to `()`)
	Sp    token.Span
}

func (*Block) exprNode()          {}
func (b *Block) Span() token.Span { return b.Sp }

// IdentExpr is a bare name.
type IdentExpr struct {
	Name string
	Sp   token.Span
}

func (*IdentExpr) exprNode()          {}
func (e *IdentExpr) Span() token.Span { return e.Sp }

// PathExpr is `a::b::c`.
type PathExpr struct {
	Segments []string
	Sp       token.Span
}

func (*PathExpr) exprNode()          {}
func (e *PathExpr) Span() token.Span { return e.Sp }

// LiteralExpr is any literal.
type LiteralExpr struct {
	Kind token.Kind
	Text string
	Sp   token.Span
}

func (*LiteralExpr) exprNode()          {}
func (e *LiteralExpr) Span() token.Span { return e.Sp }

// BinExpr is a binary operation.
type BinExpr struct {
	Op   token.Kind
	L, R Expr
	Sp   token.Span
}

func (*BinExpr) exprNode()          {}
func (e *BinExpr) Span() token.Span { return e.Sp }

// UnExpr is a prefix unary operation (`- ! & &mut *`).
type UnExpr struct {
	Op  token.Kind
	Mut bool // for `&mut`
	X   Expr
	Sp  token.Span
}

func (*UnExpr) exprNode()          {}
func (e *UnExpr) Span() token.Span { return e.Sp }

// AssignExpr is `place = value` or a compound assignment such as `place += v`.
type AssignExpr struct {
	Op  token.Kind // KindEq or a compound operator
	Lhs Expr
	Rhs Expr
	Sp  token.Span
}

func (*AssignExpr) exprNode()          {}
func (e *AssignExpr) Span() token.Span { return e.Sp }

// CastExpr is `expr as Ty`.
type CastExpr struct {
	X  Expr
	Ty Type
	Sp token.Span
}

func (*CastExpr) exprNode()          {}
func (e *CastExpr) Span() token.Span { return e.Sp }

// TupleExpr represents `(e)`, `(a, b)` and the unit literal `()`. A
// parenthesized single expression is represented by that expression itself.
type TupleExpr struct {
	Elems []Expr
	Sp    token.Span
}

func (*TupleExpr) exprNode()          {}
func (e *TupleExpr) Span() token.Span { return e.Sp }

// ArrayLitExpr is `[e0, e1, ...]` or the repeat form `[value; count]`.
type ArrayLitExpr struct {
	Elems  []Expr
	Value  Expr
	Count  Expr
	Repeat bool
	Sp     token.Span
}

func (*ArrayLitExpr) exprNode()          {}
func (e *ArrayLitExpr) Span() token.Span { return e.Sp }

// NamedArgExpr is one `name = value` argument in a call. The checker
// rewrites calls that use named arguments (or omit defaulted ones) into
// plain positional argument lists, so later stages never see this node in
// a resolved call.
type NamedArgExpr struct {
	Name string
	Val  Expr
	Sp   token.Span
}

func (*NamedArgExpr) exprNode()          {}
func (e *NamedArgExpr) Span() token.Span { return e.Sp }

// InterpStrExpr is an interpolated string `f"...{expr}..."`. Parts
// alternates literal chunks (*LiteralExpr of kind string) and arbitrary
// expressions whose values are converted to text. The whole expression
// has type `String`.
type InterpStrExpr struct {
	Parts []Expr
	Sp    token.Span
}

func (*InterpStrExpr) exprNode()          {}
func (e *InterpStrExpr) Span() token.Span { return e.Sp }

// CallExpr is `f(args)`.
type CallExpr struct {
	Callee Expr
	Args   []Expr
	Sp     token.Span
}

func (*CallExpr) exprNode()          {}
func (e *CallExpr) Span() token.Span { return e.Sp }

// MethodCallExpr is `recv.name(args)`.
type MethodCallExpr struct {
	Recv Expr
	Name string
	Args []Expr
	Sp   token.Span
}

func (*MethodCallExpr) exprNode()          {}
func (e *MethodCallExpr) Span() token.Span { return e.Sp }

// FieldExpr is `recv.field`.
type FieldExpr struct {
	Recv  Expr
	Field string
	Sp    token.Span
}

func (*FieldExpr) exprNode()          {}
func (e *FieldExpr) Span() token.Span { return e.Sp }

// IndexExpr is `xs[i]`.
type IndexExpr struct {
	Recv  Expr
	Index Expr
	Sp    token.Span
}

func (*IndexExpr) exprNode()          {}
func (e *IndexExpr) Span() token.Span { return e.Sp }

// TryExpr is postfix `?`.
type TryExpr struct {
	X  Expr
	Sp token.Span
}

func (*TryExpr) exprNode()          {}
func (e *TryExpr) Span() token.Span { return e.Sp }

// IfExpr is `if cond { .. } else { .. }` / `else if`.
type IfExpr struct {
	Cond Expr
	Then *Block
	Else Expr // *Block, *IfExpr, or nil
	Sp   token.Span
}

func (*IfExpr) exprNode()          {}
func (e *IfExpr) Span() token.Span { return e.Sp }

// MatchExpr is `match scrutinee { arms }`.
type MatchExpr struct {
	Scrutinee Expr
	Arms      []MatchArm
	Sp        token.Span
}

func (*MatchExpr) exprNode()          {}
func (e *MatchExpr) Span() token.Span { return e.Sp }

// MatchArm is `pat if guard => body`.
type MatchArm struct {
	Pat   Pattern
	Guard Expr // may be nil
	Body  Expr
	Sp    token.Span
}

// LoopExpr covers `loop`, `while`, `until`, and `for`.
type LoopExpr struct {
	Kind token.Kind // KindLoop, KindWhile, KindUntil, or KindFor
	Cond Expr       // while / until
	Pat  Pattern    // for
	Iter Expr       // for
	Body *Block
	Sp   token.Span
}

func (*LoopExpr) exprNode()          {}
func (e *LoopExpr) Span() token.Span { return e.Sp }

// ReturnExpr, BreakExpr, ContinueExpr share this shape.
type JumpExpr struct {
	Kind token.Kind // KindReturn, KindBreak, KindContinue
	Val  Expr       // may be nil
	Sp   token.Span
}

func (*JumpExpr) exprNode()          {}
func (e *JumpExpr) Span() token.Span { return e.Sp }

// StructLitExpr is `Path { field: value, .. }`.
type StructLitExpr struct {
	Path   []string
	Fields []FieldInit
	Sp     token.Span
}

func (*StructLitExpr) exprNode()          {}
func (e *StructLitExpr) Span() token.Span { return e.Sp }

// FieldInit is one `name: value` in a struct literal.
type FieldInit struct {
	Name string
	Val  Expr
	Sp   token.Span
}

// --- patterns -------------------------------------------------------------

// Pattern is a binding or match pattern.
type Pattern interface {
	Node
	patternNode()
}

// BindPat is `name` or `mut name`.
type BindPat struct {
	Mut  bool
	Name string
	Sp   token.Span
}

func (*BindPat) patternNode()       {}
func (p *BindPat) Span() token.Span { return p.Sp }

// WildcardPat is `_`.
type WildcardPat struct {
	Sp token.Span
}

func (*WildcardPat) patternNode()       {}
func (p *WildcardPat) Span() token.Span { return p.Sp }

// TuplePat is `(a, b, ..)`.
type TuplePat struct {
	Elems []Pattern
	Sp    token.Span
}

func (*TuplePat) patternNode()       {}
func (p *TuplePat) Span() token.Span { return p.Sp }

// PathPat is `Enum::Variant`, optionally with sub-patterns or fields.
type PathPat struct {
	Path    []string
	Sub     []Pattern
	Fields  []FieldPat
	HasRest bool
	Sp      token.Span
}

func (*PathPat) patternNode()       {}
func (p *PathPat) Span() token.Span { return p.Sp }

// LitPat is a literal pattern such as `42`, `"s"`, or `true`.
type LitPat struct {
	Kind token.Kind
	Text string
	Sp   token.Span
}

func (*LitPat) patternNode()       {}
func (p *LitPat) Span() token.Span { return p.Sp }

// FieldPat is `name: pat` inside a struct pattern.
type FieldPat struct {
	Name string
	Pat  Pattern
	Sp   token.Span
}

// OrPat is `pat | pat | ...` in a `match` arm or `if let`/`while let`: the arm
// is taken when any alternative matches. Alternatives may not bind variables
// (which alternative bound the value would be ambiguous).
type OrPat struct {
	Alts []Pattern
	Sp   token.Span
}

func (*OrPat) patternNode()       {}
func (p *OrPat) Span() token.Span { return p.Sp }

// RangePat is `lo..hi` or `lo..=hi` over integer or `char` literal endpoints.
type RangePat struct {
	Lo, Hi    *LitPat
	Inclusive bool // true for `..=`
	Sp        token.Span
}

func (*RangePat) patternNode()       {}
func (p *RangePat) Span() token.Span { return p.Sp }
