package lsp

// Analysis runs the zexi front end over a set of (possibly unsaved) files and
// answers language questions about the result: per-file diagnostics, the
// innermost AST node at a position, hover text, definitions, completions, and
// document symbols.

import (
	"path/filepath"
	"sort"
	"strings"

	"zexi/internal/ast"
	"zexi/internal/diag"
	"zexi/internal/driver"
	"zexi/internal/sema"
	"zexi/internal/token"
	"zexi/internal/types"
)

// analysis is one front-end run over one program entry.
type analysis struct {
	art *driver.Artifacts // Info may be nil when parsing failed
	em  *diag.Emitter
	// byPath maps cleaned absolute path -> file as registered in em.
	byPath map[string]diag.File
	// diags maps cleaned absolute path -> diagnostics rendered for it.
	diags map[string][]Diagnostic
}

// analyzeEntry runs the front end over path with overlay contents (unsaved
// buffers). suppressMain drops the "no main" error so libraries can be
// checked standalone in an editor.
func analyzeEntry(path string, overlay map[string][]byte, suppressMain bool) *analysis {
	em := diag.NewEmitter()
	art, _ := driver.FrontEnd(path, driver.Options{
		Overlay:   overlay,
		CheckOnly: true,
	}, em)
	a := &analysis{
		art:    art,
		em:     em,
		byPath: map[string]diag.File{},
		diags:  map[string][]Diagnostic{},
	}
	for _, f := range em.Files() {
		if p := cleanPath(f.Name); p != "" {
			a.byPath[p] = f
		}
	}
	for _, d := range em.Diags() {
		if suppressMain && d.Code == "E0601" && strings.Contains(d.Message, "no main") {
			continue
		}
		f, start, end, ok := em.Resolve(d.Span)
		if !ok {
			continue
		}
		p := cleanPath(f.Name)
		if p == "" {
			continue
		}
		// The analyzed source is the truth for offset conversion: the
		// overlay contents *are* what the front end saw.
		r := Range{
			Start: offsetToPosition(f.Src, start),
			End:   offsetToPosition(f.Src, end),
		}
		a.diags[p] = append(a.diags[p], Diagnostic{
			Range:    r,
			Severity: sevOf(d.Severity),
			Code:     d.Code,
			Source:   "zexi",
			Message:  d.Message,
		})
	}
	return a
}

// sevOf maps our severities to LSP ones (1=Error, 2=Warning).
func sevOf(s diag.Severity) int {
	switch s {
	case diag.Warning:
		return 2
	default:
		return 1
	}
}

// cleanPath canonicalizes a file name the way the overlay is keyed: absolute,
// symlink-resolved when possible.
func cleanPath(p string) string {
	if p == "" {
		return ""
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = filepath.Clean(p)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return abs
}

// --- AST traversal ---------------------------------------------------------

// inspect calls fn on every expression and pattern in the item tree, parents
// before children. fn returns false to skip an expression's children.
func inspect(node ast.Node, fn func(ast.Node) bool) {
	if node == nil {
		return
	}
	switch n := node.(type) {
	case *ast.File:
		for _, it := range n.Items {
			inspect(it, fn)
		}
	case *ast.FnDecl:
		if !fn(n) {
			return
		}
		for i := range n.Params {
			if d := n.Params[i].Default; d != nil {
				inspect(d, fn)
			}
		}
		inspect(n.Body, fn)
	case *ast.ImplBlock:
		if !fn(n) {
			return
		}
		for _, f := range n.Fns {
			inspect(f, fn)
		}
	case *ast.StructDecl:
		fn(n)
	case *ast.EnumDecl:
		fn(n)
	case *ast.ConstDecl:
		if !fn(n) {
			return
		}
		inspect(n.Val, fn)
	case *ast.TypeAlias:
		fn(n)
	case *ast.UseDecl:
		fn(n)
	case *ast.ModDecl:
		fn(n)
	case *ast.Block:
		fn(n)
		for _, s := range n.Stmts {
			inspect(s, fn)
		}
	case *ast.LetStmt:
		if !fn(n) {
			return
		}
		inspect(n.Pat, fn)
		inspect(n.Val, fn)
	case *ast.ExprStmt:
		if !fn(n) {
			return
		}
		inspect(n.X, fn)
	case *ast.DeferStmt:
		if !fn(n) {
			return
		}
		inspect(n.X, fn)
	// Expressions.
	case *ast.IdentExpr:
		fn(n)
	case *ast.PathExpr:
		fn(n)
	case *ast.LiteralExpr:
		fn(n)
	case *ast.BinExpr:
		if !fn(n) {
			return
		}
		inspect(n.L, fn)
		inspect(n.R, fn)
	case *ast.UnExpr:
		if !fn(n) {
			return
		}
		inspect(n.X, fn)
	case *ast.AssignExpr:
		if !fn(n) {
			return
		}
		inspect(n.Lhs, fn)
		inspect(n.Rhs, fn)
	case *ast.CastExpr:
		if !fn(n) {
			return
		}
		inspect(n.X, fn)
	case *ast.TupleExpr:
		if !fn(n) {
			return
		}
		for _, e := range n.Elems {
			inspect(e, fn)
		}
	case *ast.ArrayLitExpr:
		if !fn(n) {
			return
		}
		for _, e := range n.Elems {
			inspect(e, fn)
		}
		inspect(n.Value, fn)
		inspect(n.Count, fn)
	case *ast.NamedArgExpr:
		if !fn(n) {
			return
		}
		inspect(n.Val, fn)
	case *ast.InterpStrExpr:
		if !fn(n) {
			return
		}
		for _, e := range n.Parts {
			inspect(e, fn)
		}
	case *ast.CallExpr:
		if !fn(n) {
			return
		}
		inspect(n.Callee, fn)
		for _, e := range n.Args {
			inspect(e, fn)
		}
	case *ast.MethodCallExpr:
		if !fn(n) {
			return
		}
		inspect(n.Recv, fn)
		for _, e := range n.Args {
			inspect(e, fn)
		}
	case *ast.FieldExpr:
		if !fn(n) {
			return
		}
		inspect(n.Recv, fn)
	case *ast.IndexExpr:
		if !fn(n) {
			return
		}
		inspect(n.Recv, fn)
		inspect(n.Index, fn)
	case *ast.SliceExpr:
		if !fn(n) {
			return
		}
		inspect(n.Recv, fn)
		inspect(n.Low, fn)
		inspect(n.High, fn)
	case *ast.TryExpr:
		if !fn(n) {
			return
		}
		inspect(n.X, fn)
	case *ast.IfExpr:
		if !fn(n) {
			return
		}
		inspect(n.Cond, fn)
		inspect(n.Then, fn)
		inspect(n.Else, fn)
	case *ast.MatchExpr:
		if !fn(n) {
			return
		}
		inspect(n.Scrutinee, fn)
		for i := range n.Arms {
			arm := &n.Arms[i]
			inspect(arm.Pat, fn)
			inspect(arm.Guard, fn)
			inspect(arm.Body, fn)
		}
	case *ast.LoopExpr:
		if !fn(n) {
			return
		}
		inspect(n.Cond, fn)
		inspect(n.Pat, fn)
		inspect(n.Iter, fn)
		inspect(n.Body, fn)
	case *ast.JumpExpr:
		if !fn(n) {
			return
		}
		inspect(n.Val, fn)
	case *ast.StructLitExpr:
		if !fn(n) {
			return
		}
		for i := range n.Fields {
			inspect(n.Fields[i].Val, fn)
		}
	// Patterns.
	case *ast.BindPat:
		fn(n)
	case *ast.WildcardPat:
		fn(n)
	case *ast.TuplePat:
		if !fn(n) {
			return
		}
		for _, e := range n.Elems {
			inspect(e, fn)
		}
	case *ast.PathPat:
		if !fn(n) {
			return
		}
		for _, e := range n.Sub {
			inspect(e, fn)
		}
		for i := range n.Fields {
			inspect(n.Fields[i].Pat, fn)
		}
	case *ast.LitPat:
		fn(n)
	case *ast.OrPat:
		if !fn(n) {
			return
		}
		for _, e := range n.Alts {
			inspect(e, fn)
		}
	case *ast.RangePat:
		fn(n)
	}
}

// nodeSpan returns the node's span.
func nodeSpan(n ast.Node) token.Span { return n.Span() }

// nodeAtOffset returns the innermost node whose span contains offset.
func nodeAtOffset(root ast.Node, offset int) ast.Node {
	var best ast.Node
	inspect(root, func(n ast.Node) bool {
		sp := nodeSpan(n)
		if offset >= int(sp.Start) && offset <= int(sp.End) {
			if best == nil {
				best = n
			}
			// Prefer the smallest containing node (latest start, earliest end).
			bestSp := nodeSpan(best)
			if sp.Start > bestSp.Start || (sp.Start == bestSp.Start && sp.End < bestSp.End) {
				if offset >= int(sp.Start) && offset <= int(sp.End) {
					best = n
				}
			}
		}
		return true
	})
	return best
}

// nodeEndingAt returns the innermost node whose span ends exactly at offset
// (used for `.`-completion: the receiver of the dot).
func nodeEndingAt(root ast.Node, offset int) ast.Node {
	var best ast.Node
	inspect(root, func(n ast.Node) bool {
		if int(nodeSpan(n).End) == offset {
			if best == nil {
				best = n
			} else {
				bestSp := nodeSpan(best)
				sp := nodeSpan(n)
				if sp.Start > bestSp.Start {
					best = n
				}
			}
		}
		return true
	})
	return best
}

// --- query helpers ---------------------------------------------------------

// globalOffset converts a position in the file at path to an offset in the
// shared span space.
func (a *analysis) globalOffset(path string, off int) int {
	if f, ok := a.byPath[path]; ok {
		return f.Base + off
	}
	return off
}

// fileSpan converts a global span to a range in its file.
func (a *analysis) fileSpan(sp token.Span) (Range, string, bool) {
	f, start, end, ok := a.em.Resolve(sp)
	if !ok {
		return Range{}, "", false
	}
	p := cleanPath(f.Name)
	r := Range{
		Start: offsetToPosition(f.Src, start),
		End:   offsetToPosition(f.Src, end),
	}
	return r, p, true
}

// deref strips reference/pointer wrappers so member access works on `&mut s`.
func deref(t *types.Type) *types.Type {
	for t != nil && (t.Kind == types.Ref || t.Kind == types.Ptr) {
		t = t.Elem
	}
	return t
}

// hoverText renders the markdown hover body for a node.
func (a *analysis) hoverText(n ast.Node) string {
	if a.art == nil || a.art.Info == nil || n == nil {
		return ""
	}
	info := a.art.Info
	switch x := n.(type) {
	case *ast.IdentExpr:
		if fn := info.Fns[x.Name]; fn != nil {
			return mdCode(sigOfFn(fn))
		}
		if c := info.Consts[x.Name]; c != nil {
			return mdCode("const " + c.Name + ": " + c.Ty.String())
		}
		if t := info.Structs[x.Name]; t != nil {
			return mdCode("struct " + t.Name)
		}
		if t := info.Enums[x.Name]; t != nil {
			return mdCode("enum " + t.Name)
		}
		if t := info.TypeOf(x); t != nil {
			return mdCode(t.String())
		}
	case *ast.PathExpr:
		if fn := info.Resolved[x]; fn != nil {
			return mdCode(sigOfFn(fn))
		}
		if len(x.Segments) > 0 {
			last := x.Segments[len(x.Segments)-1]
			if fn := info.Fns[last]; fn != nil {
				return mdCode(sigOfFn(fn))
			}
			if t := info.Structs[last]; t != nil {
				return mdCode("struct " + t.Name)
			}
			if t := info.Enums[last]; t != nil {
				return mdCode("enum " + t.Name)
			}
		}
		if t := info.TypeOf(x); t != nil {
			return mdCode(t.String())
		}
	case *ast.FieldExpr:
		if t := deref(info.TypeOf(x.Recv)); t != nil && t.St != nil {
			if f, ok := t.St.Field(x.Field); ok {
				return mdCode(x.Field + ": " + f.Ty.String())
			}
		}
		if t := info.TypeOf(x); t != nil {
			return mdCode(x.Field + ": " + t.String())
		}
	case *ast.MethodCallExpr:
		if fn := info.Resolved[x]; fn != nil {
			return mdCode(sigOfFn(fn))
		}
		if t := info.TypeOf(x); t != nil {
			return mdCode(x.Name + "(): " + t.String())
		}
	case *ast.CallExpr:
		if fn := info.Resolved[x]; fn != nil {
			return mdCode(sigOfFn(fn))
		}
		if fn := info.FnValues[x.Callee]; fn != nil {
			return mdCode(sigOfFn(fn))
		}
		if t := info.TypeOf(x); t != nil {
			return mdCode(t.String())
		}
	case *ast.StructLitExpr:
		if len(x.Path) > 0 {
			name := x.Path[len(x.Path)-1]
			if t := info.Structs[name]; t != nil {
				return mdCode("struct " + t.Name)
			}
			if t := info.Enums[name]; t != nil {
				return mdCode("enum " + t.Name)
			}
		}
	case *ast.BindPat:
		if t := info.DeclTypes[bindOwner(info, x)]; t != nil {
			return mdCode(x.Name + ": " + t.String())
		}
		return mdCode("let " + x.Name)
	}
	// Expression fallback: show the inferred type.
	if e, ok := n.(ast.Expr); ok {
		if t := info.TypeOf(e); t != nil {
			return mdCode(t.String())
		}
	}
	return ""
}

// bindOwner finds the let statement that introduces a binding pattern (so
// DeclTypes can give it a type).
func bindOwner(info *sema.Info, p ast.Pattern) ast.Stmt {
	var owner ast.Stmt
	inspect(info.File, func(n ast.Node) bool {
		if l, ok := n.(*ast.LetStmt); ok {
			found := false
			inspect(l.Pat, func(c ast.Node) bool {
				if c == p {
					found = true
				}
				return true
			})
			if found {
				owner = l
			}
		}
		return owner == nil
	})
	return owner
}

// mdCode wraps text in a zexi code fence for hover.
func mdCode(s string) string { return "```zexi\n" + s + "\n```" }

// sigOfFn renders a function signature.
func sigOfFn(fn *sema.Fn) string {
	var b strings.Builder
	b.WriteString("fn ")
	b.WriteString(fn.QualName)
	b.WriteByte('(')
	first := true
	for i, p := range fn.Params {
		name := ""
		if i < len(fn.ParamNames) {
			name = fn.ParamNames[i]
		}
		if i == 0 && name == "" {
			b.WriteString(p.String())
			first = false
			continue
		}
		if !first {
			b.WriteString(", ")
		}
		first = false
		if name != "" {
			b.WriteString(name)
			b.WriteString(": ")
		}
		b.WriteString(p.String())
	}
	b.WriteByte(')')
	if fn.Decl != nil && fn.Decl.Ret != nil && fn.Ret != nil && fn.Ret.String() != "()" {
		b.WriteString(" -> ")
		b.WriteString(fn.Ret.String())
	}
	return b.String()
}

// definitionLoc resolves the definition of the node at the offset.
func (a *analysis) definitionLoc(path string, offset int) (Location, bool) {
	if a.art == nil || a.art.Info == nil {
		return Location{}, false
	}
	info := a.art.Info
	n := nodeAtOffset(info.File, a.globalOffset(path, offset))
	if n == nil {
		return Location{}, false
	}
	target, ok := a.declSpan(n, path, offset)
	if !ok {
		return Location{}, false
	}
	r, p, ok := a.fileSpan(target)
	if !ok {
		return Location{}, false
	}
	return Location{URI: pathToURI(p), Range: r}, true
}

// declSpan resolves a node to the span of its declaration.
func (a *analysis) declSpan(n ast.Node, path string, offset int) (token.Span, bool) {
	info := a.art.Info
	switch x := n.(type) {
	case *ast.IdentExpr:
		return a.identDecl(x, path, offset, info)
	case *ast.PathExpr:
		if fn := info.Resolved[x]; fn != nil && fn.Decl != nil {
			return nodeSpan(fn.Decl), true
		}
		if fn := info.FnValues[x]; fn != nil && fn.Decl != nil {
			return nodeSpan(fn.Decl), true
		}
		if len(x.Segments) > 0 {
			return nameDecl(x.Segments[len(x.Segments)-1], info)
		}
	case *ast.FieldExpr:
		if t := deref(info.TypeOf(x.Recv)); t != nil && t.St != nil {
			return fieldDecl(info, t.St.Name, x.Field)
		}
	case *ast.MethodCallExpr:
		if fn := info.Resolved[x]; fn != nil && fn.Decl != nil {
			return nodeSpan(fn.Decl), true
		}
	case *ast.CallExpr:
		if fn := info.Resolved[x]; fn != nil && fn.Decl != nil {
			return nodeSpan(fn.Decl), true
		}
		if fn := info.FnValues[x.Callee]; fn != nil && fn.Decl != nil {
			return nodeSpan(fn.Decl), true
		}
	case *ast.StructLitExpr:
		if len(x.Path) > 0 {
			return nameDecl(x.Path[len(x.Path)-1], info)
		}
	case *ast.BindPat:
		// A binding refers to itself; jump to the let statement instead.
		if owner := bindOwner(info, x); owner != nil {
			return nodeSpan(owner), true
		}
	}
	return token.Span{}, false
}

// identDecl resolves a bare name to its declaration: a local binding (the
// nearest preceding one), a parameter, or a global.
func (a *analysis) identDecl(x *ast.IdentExpr, path string, offset int, info *sema.Info) (token.Span, bool) {
	goff := a.globalOffset(path, offset)
	// Locals: the closest binding with this name that starts before us and is
	// in the same file.
	var local token.Span
	localStart, found := -1, false
	inspect(info.File, func(n ast.Node) bool {
		b, ok := n.(*ast.BindPat)
		if !ok || b.Name != x.Name {
			return true
		}
		sp := nodeSpan(b)
		if int(sp.Start) <= goff && int(sp.Start) >= localStart {
			if _, p, ok := a.fileSpan(sp); ok && p == path {
				local = sp
				localStart = int(sp.Start)
				found = true
			}
		}
		return true
	})
	if found {
		return local, true
	}
	// Parameters.
	for _, fn := range info.FnOrder {
		if fn.Decl == nil {
			continue
		}
		for j := range fn.Decl.Params {
			if fn.Decl.Params[j].Name == x.Name {
				return fn.Decl.Params[j].Sp, true
			}
		}
	}
	if fn := info.Resolved[x]; fn != nil && fn.Decl != nil {
		return nodeSpan(fn.Decl), true
	}
	return nameDecl(x.Name, info)
}

// nameDecl resolves a global name to its declaration span.
func nameDecl(name string, info *sema.Info) (token.Span, bool) {
	if fn := info.Fns[name]; fn != nil && fn.Decl != nil {
		return nodeSpan(fn.Decl), true
	}
	if c := info.Consts[name]; c != nil && c.Decl != nil {
		return nodeSpan(c.Decl), true
	}
	if d := structDecl(info, name); d != nil {
		return nodeSpan(d), true
	}
	if d := enumDecl(info, name); d != nil {
		return nodeSpan(d), true
	}
	return token.Span{}, false
}

// structDecl / enumDecl find the AST declaration for a type name.
func structDecl(info *sema.Info, name string) *ast.StructDecl {
	var found *ast.StructDecl
	inspect(info.File, func(n ast.Node) bool {
		if d, ok := n.(*ast.StructDecl); ok && d.Name == name {
			found = d
		}
		return found == nil
	})
	return found
}

func enumDecl(info *sema.Info, name string) *ast.EnumDecl {
	var found *ast.EnumDecl
	inspect(info.File, func(n ast.Node) bool {
		if d, ok := n.(*ast.EnumDecl); ok && d.Name == name {
			found = d
		}
		return found == nil
	})
	return found
}

// fieldDecl finds the span of one struct field's declaration.
func fieldDecl(info *sema.Info, structName, fieldName string) (token.Span, bool) {
	d := structDecl(info, structName)
	if d == nil {
		return token.Span{}, false
	}
	for i := range d.Fields {
		if d.Fields[i].Name == fieldName {
			return d.Fields[i].Sp, true
		}
	}
	return token.Span{}, false
}

// completionItems lists candidates at the offset. When dot is true the offset
// sits after `.` and the candidates are members of the receiver's type.
func (a *analysis) completionItems(path string, offset int, dot bool) []CompletionItem {
	var items []CompletionItem
	if a.art == nil || a.art.Info == nil {
		return items
	}
	info := a.art.Info
	if dot {
		goff := a.globalOffset(path, offset)
		recv, ok := nodeEndingAt(info.File, goff).(ast.Expr)
		if !ok {
			return items
		}
		t := deref(info.TypeOf(recv))
		if t == nil {
			return items
		}
		if t.St != nil {
			for i := range t.St.Fields {
				f := &t.St.Fields[i]
				items = append(items, CompletionItem{
					Label:  f.Name,
					Kind:   CmpField,
					Detail: f.Ty.String(),
				})
			}
			for _, m := range info.Methods[t.St.Name] {
				items = append(items, CompletionItem{
					Label:  m.Name,
					Kind:   CmpMethod,
					Detail: sigOfFn(m),
				})
			}
			return items
		}
		if t.En != nil {
			for i := range t.En.Variants {
				v := &t.En.Variants[i]
				items = append(items, CompletionItem{
					Label:  v.Name,
					Kind:   CmpEnumMember,
					Detail: variantDetail(v),
				})
			}
			for _, m := range info.Methods[t.En.Name] {
				items = append(items, CompletionItem{
					Label:  m.Name,
					Kind:   CmpMethod,
					Detail: sigOfFn(m),
				})
			}
			return items
		}
		items = append(items, builtinMembers(t)...)
		return items
	}
	// General completion: keywords + globals.
	for _, kw := range zexiKeywords {
		items = append(items, CompletionItem{Label: kw, Kind: CmpKeyword})
	}
	names := make([]string, 0, len(info.Fns))
	for n := range info.Fns {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		items = append(items, CompletionItem{Label: n, Kind: CmpFunction, Detail: sigOfFn(info.Fns[n])})
	}
	names = names[:0]
	for n := range info.Consts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		c := info.Consts[n]
		detail := "const"
		if c.Ty != nil {
			detail = "const " + c.Ty.String()
		}
		items = append(items, CompletionItem{Label: n, Kind: CmpConstant, Detail: detail})
	}
	for _, st := range info.StructOrd {
		items = append(items, CompletionItem{Label: st.Name, Kind: CmpStruct, Detail: "struct"})
	}
	for _, en := range info.EnumOrd {
		items = append(items, CompletionItem{Label: en.Name, Kind: CmpEnum, Detail: "enum"})
		for i := range en.Variants {
			items = append(items, CompletionItem{
				Label:  en.Variants[i].Name,
				Kind:   CmpEnumMember,
				Detail: en.Name + "::" + en.Variants[i].Name,
			})
		}
	}
	return items
}

// variantDetail renders `Name(a: T, b: U)` for a variant.
func variantDetail(v *types.Variant) string {
	var b strings.Builder
	b.WriteString(v.Name)
	b.WriteByte('(')
	for i := range v.Fields {
		if i > 0 {
			b.WriteString(", ")
		}
		f := &v.Fields[i]
		if f.Name != "" {
			b.WriteString(f.Name)
			b.WriteString(": ")
		}
		if f.Ty != nil {
			b.WriteString(f.Ty.String())
		}
	}
	b.WriteByte(')')
	return b.String()
}

// zexiKeywords are bare-language keywords offered in general completion.
var zexiKeywords = []string{
	"as", "async", "break", "const", "continue", "defer", "else", "enum",
	"extern", "fn", "for", "if", "impl", "import", "in", "let", "loop",
	"match", "mod", "mut", "pub", "return", "struct", "type", "unsafe",
	"use", "while",
}

// builtinMembers offers method completion on builtin types. The checker
// owns the authoritative list; this covers the common string/array surface.
func builtinMembers(t *types.Type) []CompletionItem {
	switch t.Kind {
	case types.Str:
		return []CompletionItem{
			{Label: "len", Kind: CmpMethod, Detail: "fn len(&self) -> i64"},
			{Label: "byte_at", Kind: CmpMethod, Detail: "fn byte_at(&self, i: i64) -> u8"},
			{Label: "push_str", Kind: CmpMethod, Detail: "fn push_str(&mut self, s: str)"},
			{Label: "push", Kind: CmpMethod, Detail: "fn push(&mut self, b: u8)"},
		}
	}
	return nil
}

// documentSymbols collects the outline of one file: top-level items whose
// spans live in it.
func (a *analysis) documentSymbols(path string) []DocumentSymbol {
	var out []DocumentSymbol
	if a.art == nil || a.art.Info == nil {
		return out
	}
	info := a.art.Info
	for _, it := range info.File.Items {
		sp := nodeSpan(it)
		_, p, ok := a.fileSpan(sp)
		if !ok || p != path {
			continue
		}
		r := Range{}
		if rr, _, ok := a.fileSpan(sp); ok {
			r = rr
		}
		switch d := it.(type) {
		case *ast.FnDecl:
			out = append(out, DocumentSymbol{
				Name: d.Name, Kind: SymFunction,
				Range: r, SelectionRange: r,
				Detail: "fn",
			})
		case *ast.StructDecl:
			sym := DocumentSymbol{
				Name: d.Name, Kind: SymStruct,
				Range: r, SelectionRange: r,
				Detail: "struct",
			}
			for i := range d.Fields {
				fr, _, ok := a.fileSpan(d.Fields[i].Sp)
				if !ok {
					continue
				}
				sym.Children = append(sym.Children, DocumentSymbol{
					Name: d.Fields[i].Name, Kind: SymField,
					Range: fr, SelectionRange: fr,
				})
			}
			out = append(out, sym)
		case *ast.EnumDecl:
			sym := DocumentSymbol{
				Name: d.Name, Kind: SymEnum,
				Range: r, SelectionRange: r,
				Detail: "enum",
			}
			for i := range d.Variants {
				vr, _, ok := a.fileSpan(d.Variants[i].Sp)
				if !ok {
					continue
				}
				sym.Children = append(sym.Children, DocumentSymbol{
					Name: d.Variants[i].Name, Kind: SymEnumMember,
					Range: vr, SelectionRange: vr,
				})
			}
			out = append(out, sym)
		case *ast.ConstDecl:
			out = append(out, DocumentSymbol{
				Name: d.Name, Kind: SymConstant,
				Range: r, SelectionRange: r,
				Detail: "const",
			})
		case *ast.ImplBlock:
			for _, f := range d.Fns {
				fr, _, ok := a.fileSpan(f.Sp)
				if !ok {
					continue
				}
				out = append(out, DocumentSymbol{
					Name: f.Name, Kind: SymFunction,
					Range: fr, SelectionRange: fr,
					Detail: "fn",
				})
			}
		}
	}
	return out
}
