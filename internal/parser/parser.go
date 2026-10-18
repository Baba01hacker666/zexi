// Package parser is a hand-written recursive-descent parser with a
// precedence-climbing (Pratt) expression parser, implementing the grammar in
// docs/02. It is total on arbitrary token streams: on a syntax error it
// reports a diagnostic and resynchronizes rather than looping or panicking.
//
// Constructs the M0-alpha backend does not yet support (generics, struct
// literals, closures) are still rejected with a precise diagnostic here rather
// than silently mis-parsed.
package parser

import (
	"zexi/internal/ast"
	"zexi/internal/diag"
	"zexi/internal/lexer"
	"zexi/internal/token"
)

type parser struct {
	fileID int
	src    []byte
	toks   []token.Token
	pos    int
	errs   []diag.Diagnostic
	// noStructLit is >0 while parsing a context where `{` starts a block and
	// must not be taken as the start of a struct literal (if/while/for/match).
	noStructLit int
}

// Parse lexes and parses src, returning the AST and diagnostics. A partial AST
// is returned alongside errors so that tooling can keep working.
func Parse(fileID int, src []byte) (*ast.File, []diag.Diagnostic) {
	lx := lexer.New(fileID, src)
	p := &parser{fileID: fileID, src: src, toks: lx.All()}
	file := p.parseFile()
	return file, append(lx.Errors(), p.errs...)
}

// --- token cursor ---------------------------------------------------------

func (p *parser) cur() token.Token { return p.toks[p.pos] }

func (p *parser) at(k token.Kind) bool { return p.cur().Kind == k }

func (p *parser) prevEnd() token.Pos {
	if p.pos == 0 {
		return 0
	}
	return p.toks[p.pos-1].End
}

func (p *parser) bump() token.Token {
	t := p.toks[p.pos]
	if p.pos < len(p.toks)-1 {
		p.pos++
	}
	return t
}

// accept consumes and reports true if the current token has kind k.
func (p *parser) accept(k token.Kind) bool {
	if p.at(k) {
		p.bump()
		return true
	}
	return false
}

// expect consumes a token of kind k or reports a diagnostic.
func (p *parser) expect(k token.Kind, context string) (token.Token, bool) {
	if p.at(k) {
		return p.bump(), true
	}
	t := p.cur()
	p.errf(t, "E0201", "expected %s%s, found %s", k, context, t.Kind)
	return t, false
}

func (p *parser) errf(t token.Token, code, format string, args ...any) {
	span := token.Span{Start: t.Pos.Offset, End: t.End}
	p.errs = append(p.errs, diag.New(code, format, args...).WithSpan(span))
}

func (p *parser) spanFrom(start token.Pos) token.Span {
	return token.Span{Start: start, End: p.prevEnd()}
}

// --- items ----------------------------------------------------------------

func (p *parser) parseFile() *ast.File {
	start := p.cur().Pos.Offset
	var items []ast.Item
	for !p.at(token.KindEOF) {
		before := p.pos
		it := p.parseItem()
		if it != nil {
			items = append(items, it)
		}
		if p.pos == before { // guarantee progress on error
			p.bump()
		}
	}
	return &ast.File{Items: items, Sp: p.spanFrom(start)}
}

// parseItem parses a top-level declaration, including its attributes and
// visibility.
func (p *parser) parseItem() ast.Item {
	attrs := p.parseAttributes()

	vis := ast.Visibility(p.accept(token.KindPub))
	start := p.cur().Pos.Offset

	switch p.cur().Kind {
	case token.KindFn:
		return p.parseFn(vis, start)
	case token.KindUnsafe:
		p.bump()
		if p.at(token.KindFn) {
			d := p.parseFn(vis, start)
			if f, ok := d.(*ast.FnDecl); ok {
				f.Unsafe = true
			}
			return d
		}
		p.errf(p.cur(), "E0202", "expected `fn` after `unsafe`")
		return nil
	case token.KindExtern:
		return p.parseExternFn(vis, start)
	case token.KindStruct:
		return p.parseStruct(vis, attrs, start)
	case token.KindEnum:
		return p.parseEnum(vis, attrs, start)
	case token.KindConst:
		return p.parseConst(vis, start)
	case token.KindType:
		return p.parseTypeAlias(vis, start)
	case token.KindImpl:
		return p.parseImpl(start)
	case token.KindMod:
		return p.parseMod(vis, start)
	case token.KindUse:
		return p.parseUse(start)
	default:
		p.errf(p.cur(), "E0202", "expected an item (fn, struct, enum, const, type, impl, mod, use), found %s", p.cur().Kind)
		return nil
	}
}

// parseAttributes consumes `#[...]` attribute lists and returns the top-level
// attribute names (for example `copy` in `#[copy]`). Nested/complex attributes
// are consumed but their contents ignored.
func (p *parser) parseAttributes() []string {
	var attrs []string
	for p.at(token.KindHash) {
		p.bump()
		if _, ok := p.expect(token.KindLBracket, " after `#`"); !ok {
			return attrs
		}
		if p.at(token.KindIdentifier) {
			attrs = append(attrs, p.cur().Lit)
		}
		depth := 1
		for depth > 0 && !p.at(token.KindEOF) {
			switch p.cur().Kind {
			case token.KindLBracket:
				depth++
			case token.KindRBracket:
				depth--
			}
			p.bump()
		}
	}
	return attrs
}

// skipAttributes consumes and discards attribute lists.
func (p *parser) skipAttributes() { _ = p.parseAttributes() }

func (p *parser) parseFn(vis ast.Visibility, start token.Pos) ast.Item {
	p.bump() // fn
	name, ok := p.expectIdent("function name")
	if !ok {
		return nil
	}
	if p.at(token.KindLt) {
		p.errf(p.cur(), "E0900", "generics are not supported yet (design milestone M1)")
		p.skipGenericArgs()
	}
	fn := &ast.FnDecl{Vis: vis, Name: name, Sp: p.spanFrom(start)}
	fn.Params = p.parseParams()
	if p.accept(token.KindArrow) {
		fn.Ret = p.parseType()
	}
	if p.accept(token.KindSemicolon) {
		fn.Sp = p.spanFrom(start) // declaration without a body
		return fn
	}
	fn.Body = p.parseBlock()
	fn.Sp = p.spanFrom(start)
	return fn
}

func (p *parser) parseExternFn(vis ast.Visibility, start token.Pos) ast.Item {
	p.bump() // extern
	abi := ""
	if p.at(token.KindString) {
		abi = p.bump().Lit
	}
	if !p.at(token.KindFn) {
		p.errf(p.cur(), "E0202", "expected `fn` in `extern` declaration")
		return nil
	}
	d := p.parseFn(vis, start)
	if f, ok := d.(*ast.FnDecl); ok {
		f.Extern = abi
	}
	return d
}

func (p *parser) parseParams() []ast.Param {
	var params []ast.Param
	if _, ok := p.expect(token.KindLParen, " to begin parameter list"); !ok {
		return params
	}
	for !p.at(token.KindRParen) && !p.at(token.KindEOF) {
		start := p.cur().Pos.Offset
		if p.atSelfParam() {
			pr := ast.Param{Self: true, Name: "self"}
			if p.accept(token.KindAmp) {
				pr.SelfRef = true
				pr.SelfMut = p.accept(token.KindMut)
			}
			if _, ok := p.expectIdent("`self`"); !ok {
				p.skipTo(token.KindComma, token.KindRParen)
				if !p.accept(token.KindComma) {
					break
				}
				continue
			}
			pr.Sp = p.spanFrom(start)
			params = append(params, pr)
			if !p.accept(token.KindComma) {
				break
			}
			continue
		}
		mut := p.accept(token.KindMut)
		name, ok := p.expectIdent("parameter name")
		if !ok {
			p.skipTo(token.KindComma, token.KindRParen)
			if !p.accept(token.KindComma) {
				break
			}
			continue
		}
		p.expect(token.KindColon, " after parameter name")
		ty := p.parseType()
		params = append(params, ast.Param{Mut: mut, Name: name, Ty: ty, Sp: p.spanFrom(start)})
		if !p.accept(token.KindComma) {
			break
		}
	}
	p.expect(token.KindRParen, " to close parameter list")
	return params
}

func (p *parser) parseStruct(vis ast.Visibility, attrs []string, start token.Pos) ast.Item {
	p.bump() // struct
	name, ok := p.expectIdent("struct name")
	if !ok {
		return nil
	}
	if p.at(token.KindLt) {
		p.errf(p.cur(), "E0900", "generics are not supported yet (design milestone M1)")
		p.skipGenericArgs()
	}
	d := &ast.StructDecl{Vis: vis, Attrs: attrs, Name: name}
	switch {
	case p.accept(token.KindSemicolon):
	case p.at(token.KindLBrace):
		p.bump()
		for !p.at(token.KindRBrace) && !p.at(token.KindEOF) {
			fstart := p.cur().Pos.Offset
			fvis := ast.Visibility(p.accept(token.KindPub))
			fname, ok := p.expectIdent("field name")
			if !ok {
				p.skipTo(token.KindComma, token.KindRBrace)
				p.accept(token.KindComma)
				continue
			}
			p.expect(token.KindColon, " after field name")
			fty := p.parseType()
			d.Fields = append(d.Fields, ast.Field{Vis: fvis, Name: fname, Ty: fty, Sp: p.spanFrom(fstart)})
			if !p.accept(token.KindComma) {
				break
			}
		}
		p.expect(token.KindRBrace, " to close struct body")
	case p.accept(token.KindLParen):
		d.Tuple = true
		for !p.at(token.KindRParen) && !p.at(token.KindEOF) {
			fty := p.parseType()
			d.Fields = append(d.Fields, ast.Field{Ty: fty})
			if !p.accept(token.KindComma) {
				break
			}
		}
		p.expect(token.KindRParen, " to close tuple struct")
		p.expect(token.KindSemicolon, " after tuple struct")
	default:
		p.errf(p.cur(), "E0202", "expected `{`, `(`, or `;` in struct declaration")
	}
	d.Sp = p.spanFrom(start)
	return d
}

func (p *parser) parseEnum(vis ast.Visibility, attrs []string, start token.Pos) ast.Item {
	p.bump() // enum
	name, ok := p.expectIdent("enum name")
	if !ok {
		return nil
	}
	if p.at(token.KindLt) {
		p.errf(p.cur(), "E0900", "generics are not supported yet (design milestone M1)")
		p.skipGenericArgs()
	}
	d := &ast.EnumDecl{Vis: vis, Attrs: attrs, Name: name}
	if _, ok := p.expect(token.KindLBrace, " to begin enum body"); !ok {
		return d
	}
	for !p.at(token.KindRBrace) && !p.at(token.KindEOF) {
		vstart := p.cur().Pos.Offset
		vname, ok := p.expectIdent("variant name")
		if !ok {
			p.skipTo(token.KindComma, token.KindRBrace)
			p.accept(token.KindComma)
			continue
		}
		v := ast.Variant{Name: vname}
		switch {
		case p.accept(token.KindLParen):
			v.Tup = true
			for !p.at(token.KindRParen) && !p.at(token.KindEOF) {
				fty := p.parseType()
				v.Fields = append(v.Fields, ast.Field{Ty: fty})
				if !p.accept(token.KindComma) {
					break
				}
			}
			p.expect(token.KindRParen, " to close variant fields")
		case p.accept(token.KindLBrace):
			for !p.at(token.KindRBrace) && !p.at(token.KindEOF) {
				fstart := p.cur().Pos.Offset
				fname, ok := p.expectIdent("field name")
				if !ok {
					p.skipTo(token.KindComma, token.KindRBrace)
					p.accept(token.KindComma)
					continue
				}
				p.expect(token.KindColon, " after field name")
				fty := p.parseType()
				v.Fields = append(v.Fields, ast.Field{Name: fname, Ty: fty, Sp: p.spanFrom(fstart)})
				if !p.accept(token.KindComma) {
					break
				}
			}
			p.expect(token.KindRBrace, " to close variant fields")
		}
		v.Sp = p.spanFrom(vstart)
		d.Variants = append(d.Variants, v)
		if !p.accept(token.KindComma) {
			break
		}
	}
	p.expect(token.KindRBrace, " to close enum body")
	d.Sp = p.spanFrom(start)
	return d
}

func (p *parser) parseConst(vis ast.Visibility, start token.Pos) ast.Item {
	p.bump() // const
	name, ok := p.expectIdent("constant name")
	if !ok {
		return nil
	}
	d := &ast.ConstDecl{Vis: vis, Name: name}
	if p.accept(token.KindColon) {
		d.Ty = p.parseType()
	}
	p.expect(token.KindEq, " in constant declaration")
	d.Val = p.parseExpr()
	p.expectSemicolon()
	d.Sp = p.spanFrom(start)
	return d
}

func (p *parser) parseTypeAlias(vis ast.Visibility, start token.Pos) ast.Item {
	p.bump() // type
	name, ok := p.expectIdent("type alias name")
	if !ok {
		return nil
	}
	d := &ast.TypeAlias{Vis: vis, Name: name}
	if p.at(token.KindLt) {
		p.errf(p.cur(), "E0900", "generics are not supported yet (design milestone M1)")
		p.skipGenericArgs()
	}
	p.expect(token.KindEq, " in type alias")
	d.Ty = p.parseType()
	p.expectSemicolon()
	d.Sp = p.spanFrom(start)
	return d
}

func (p *parser) parseImpl(start token.Pos) ast.Item {
	p.bump() // impl
	if p.at(token.KindLt) {
		p.errf(p.cur(), "E0900", "generics are not supported yet (design milestone M1)")
		p.skipGenericArgs()
	}
	d := &ast.ImplBlock{}
	d.SelfTy = p.parseType()
	if p.accept(token.KindFor) {
		// `impl Trait for Type`: what we parsed as SelfTy is actually the trait.
		d.Trait = d.SelfTy
		d.SelfTy = p.parseType()
	}
	if _, ok := p.expect(token.KindLBrace, " to begin impl body"); !ok {
		return d
	}
	for !p.at(token.KindRBrace) && !p.at(token.KindEOF) {
		before := p.pos
		p.skipAttributes()
		vis := ast.Visibility(p.accept(token.KindPub))
		if p.at(token.KindFn) {
			if f, ok := p.parseFn(vis, p.cur().Pos.Offset).(*ast.FnDecl); ok {
				d.Fns = append(d.Fns, f)
			}
		} else {
			p.errf(p.cur(), "E0202", "expected `fn` in impl body, found %s", p.cur().Kind)
			p.skipTo(token.KindRBrace)
		}
		if p.pos == before {
			p.bump()
		}
	}
	p.expect(token.KindRBrace, " to close impl body")
	d.Sp = p.spanFrom(start)
	return d
}

func (p *parser) parseMod(vis ast.Visibility, start token.Pos) ast.Item {
	p.bump() // mod
	name, ok := p.expectIdent("module name")
	if !ok {
		return nil
	}
	d := &ast.ModDecl{Vis: vis, Name: name}
	if p.accept(token.KindSemicolon) {
		d.Sp = p.spanFrom(start)
		return d
	}
	if _, ok := p.expect(token.KindLBrace, " to begin module body"); !ok {
		return d
	}
	for !p.at(token.KindRBrace) && !p.at(token.KindEOF) {
		before := p.pos
		if it := p.parseItem(); it != nil {
			d.Items = append(d.Items, it)
		}
		if p.pos == before {
			p.bump()
		}
	}
	p.expect(token.KindRBrace, " to close module body")
	d.Sp = p.spanFrom(start)
	return d
}

func (p *parser) parseUse(start token.Pos) ast.Item {
	p.bump() // use
	d := &ast.UseDecl{}
	d.Path = append(d.Path, p.expectIdentPath("use path"))
	for p.accept(token.KindColonColon) {
		d.Path = append(d.Path, p.expectIdentPath("use path segment"))
	}
	if p.accept(token.KindAs) {
		d.As = p.expectIdentPath("use alias")
	}
	p.expectSemicolon()
	d.Sp = p.spanFrom(start)
	return d
}

// --- statements -----------------------------------------------------------

func (p *parser) parseBlock() *ast.Block {
	start := p.cur().Pos.Offset
	b := &ast.Block{}
	if _, ok := p.expect(token.KindLBrace, " to begin block"); !ok {
		b.Sp = p.spanFrom(start)
		return b
	}
	for !p.at(token.KindRBrace) && !p.at(token.KindEOF) {
		before := p.pos
		if p.at(token.KindLet) {
			b.Stmts = append(b.Stmts, p.parseLet())
		} else if p.at(token.KindSemicolon) {
			p.bump() // stray semicolon
		} else {
			e := p.parseExpr()
			switch {
			case p.accept(token.KindSemicolon):
				b.Stmts = append(b.Stmts, &ast.ExprStmt{X: e, Sp: e.Span()})
			case isBlockLike(e):
				// `if`/`while`/`for`/`loop`/`match`/block statements do not
				// need a trailing semicolon. If one is last in the block it is
				// the block's value.
				if p.at(token.KindRBrace) {
					b.Tail = e
					break
				}
				b.Stmts = append(b.Stmts, &ast.ExprStmt{X: e, Sp: e.Span()})
			case p.at(token.KindRBrace):
				b.Tail = e
			default:
				p.errf(p.cur(), "E0203", "expected `;` after expression statement")
				p.skipTo(token.KindSemicolon, token.KindRBrace)
				p.accept(token.KindSemicolon)
				b.Stmts = append(b.Stmts, &ast.ExprStmt{X: e, Sp: e.Span()})
			}
		}
		if p.pos == before {
			p.bump()
		}
	}
	p.expect(token.KindRBrace, " to close block")
	b.Sp = p.spanFrom(start)
	return b
}

// isBlockLike reports whether an expression is a control-flow form that may
// appear as a statement without a terminating semicolon.
func isBlockLike(e ast.Expr) bool {
	switch e.(type) {
	case *ast.IfExpr, *ast.LoopExpr, *ast.MatchExpr, *ast.Block:
		return true
	}
	return false
}

// atSelfParam reports whether the cursor is at a method receiver: `self`,
// `&self`, or `&mut self`.
func (p *parser) atSelfParam() bool {
	c := p.cur()
	if c.Kind == token.KindIdentifier && c.Lit == "self" {
		return true
	}
	if c.Kind != token.KindAmp {
		return false
	}
	switch n := p.tokAt(1); n.Kind {
	case token.KindIdentifier:
		return n.Lit == "self"
	case token.KindMut:
		m := p.tokAt(2)
		return m.Kind == token.KindIdentifier && m.Lit == "self"
	}
	return false
}

// tokAt returns the token n positions ahead of the cursor, clamped to EOF.
func (p *parser) tokAt(n int) token.Token {
	i := p.pos + n
	if i >= len(p.toks) {
		return p.toks[len(p.toks)-1] // EOF
	}
	return p.toks[i]
}

// parseStructLit parses `Path { field: value, .. }` or `Path { field, .. }`.
func (p *parser) parseStructLit(path []string, start token.Pos) ast.Expr {
	lit := &ast.StructLitExpr{Path: path}
	p.bump() // {
	for !p.at(token.KindRBrace) && !p.at(token.KindEOF) {
		fstart := p.cur().Pos.Offset
		if p.at(token.KindDotDot) {
			p.bump()
			break
		}
		name := p.expectIdentPath("field name")
		var val ast.Expr
		if p.accept(token.KindColon) {
			val = p.parseExpr()
		} else {
			val = &ast.IdentExpr{Name: name, Sp: p.spanFrom(fstart)}
		}
		lit.Fields = append(lit.Fields, ast.FieldInit{Name: name, Val: val, Sp: p.spanFrom(fstart)})
		if !p.accept(token.KindComma) {
			break
		}
	}
	p.expect(token.KindRBrace, " to close struct literal")
	lit.Sp = p.spanFrom(start)
	return lit
}

func (p *parser) parseLet() ast.Stmt {
	start := p.cur().Pos.Offset
	p.bump() // let
	mut := p.accept(token.KindMut)
	pat := p.parsePattern()
	s := &ast.LetStmt{Mut: mut, Pat: pat}
	if p.accept(token.KindColon) {
		s.Ty = p.parseType()
	}
	if p.accept(token.KindEq) {
		s.Val = p.parseExpr()
	}
	p.expectSemicolon()
	s.Sp = p.spanFrom(start)
	return s
}

func (p *parser) expectSemicolon() {
	if p.accept(token.KindSemicolon) {
		return
	}
	// Allow `}` to stand in for a missing semicolon, like Rust, so a trailing
	// statement before a block close does not cascade errors.
	if p.at(token.KindRBrace) || p.at(token.KindEOF) {
		return
	}
	p.errf(p.cur(), "E0203", "expected `;`, found %s", p.cur().Kind)
	p.skipTo(token.KindSemicolon, token.KindRBrace)
	p.accept(token.KindSemicolon)
}

// --- patterns -------------------------------------------------------------

func (p *parser) parsePattern() ast.Pattern {
	start := p.cur().Pos.Offset
	switch p.cur().Kind {
	case token.KindUnderscore:
		p.bump()
		return &ast.WildcardPat{Sp: p.spanFrom(start)}
	case token.KindLParen:
		p.bump()
		var elems []ast.Pattern
		for !p.at(token.KindRParen) && !p.at(token.KindEOF) {
			elems = append(elems, p.parsePattern())
			if !p.accept(token.KindComma) {
				break
			}
		}
		p.expect(token.KindRParen, " to close tuple pattern")
		return &ast.TuplePat{Elems: elems, Sp: p.spanFrom(start)}
	case token.KindInteger, token.KindFloat, token.KindString, token.KindRawString,
		token.KindChar, token.KindByteChar, token.KindTrue, token.KindFalse:
		t := p.bump()
		return &ast.LitPat{Kind: t.Kind, Text: t.Lit, Sp: p.spanFrom(start)}
	case token.KindIdentifier:
		mut := p.accept(token.KindMut)
		if p.at(token.KindIdentifier) {
			name := p.bump().Lit
			if p.at(token.KindColonColon) || p.at(token.KindLParen) || p.at(token.KindLBrace) {
				return p.finishPathPattern(mut, name, start)
			}
			return &ast.BindPat{Mut: mut, Name: name, Sp: p.spanFrom(start)}
		}
		return p.finishPathPattern(mut, "", start)
	default:
		p.errf(p.cur(), "E0202", "expected a pattern, found %s", p.cur().Kind)
		p.bump()
		return &ast.WildcardPat{Sp: p.spanFrom(start)}
	}
}

func (p *parser) finishPathPattern(_ bool, first string, start token.Pos) ast.Pattern {
	pp := &ast.PathPat{}
	if first != "" {
		pp.Path = append(pp.Path, first)
	}
	for p.accept(token.KindColonColon) {
		pp.Path = append(pp.Path, p.expectIdentPath("path segment"))
	}
	switch {
	case p.accept(token.KindLParen):
		for !p.at(token.KindRParen) && !p.at(token.KindEOF) {
			pp.Sub = append(pp.Sub, p.parsePattern())
			if !p.accept(token.KindComma) {
				break
			}
		}
		p.expect(token.KindRParen, " to close variant pattern")
	case p.accept(token.KindLBrace):
		for !p.at(token.KindRBrace) && !p.at(token.KindEOF) {
			if p.at(token.KindDotDot) {
				p.bump()
				pp.HasRest = true
				break
			}
			fstart := p.cur().Pos.Offset
			fname := p.expectIdentPath("field pattern")
			var sub ast.Pattern
			if p.accept(token.KindColon) {
				sub = p.parsePattern()
			}
			pp.Fields = append(pp.Fields, ast.FieldPat{Name: fname, Pat: sub, Sp: p.spanFrom(fstart)})
			if !p.accept(token.KindComma) {
				break
			}
		}
		p.expect(token.KindRBrace, " to close struct pattern")
	}
	pp.Sp = p.spanFrom(start)
	return pp
}

// --- expressions ----------------------------------------------------------

// infixPrec is the binding power of binary operators, tightest = highest. It
// encodes the precedence table from docs/02 §2.1.
var infixPrec = map[token.Kind]int{
	token.KindPipePipe: 1,
	token.KindAmpAmp:   2,
	token.KindEqEq:     3,
	token.KindBangEq:   3,
	token.KindLt:       3,
	token.KindLe:       3,
	token.KindGt:       3,
	token.KindGe:       3,
	token.KindPipe:     4,
	token.KindCaret:    5,
	token.KindAmp:      6,
	token.KindShl:      7,
	token.KindShr:      7,
	token.KindDotDot:   8,
	token.KindDotDotEq: 8,
	token.KindPlus:     9,
	token.KindMinus:    9,
	token.KindStar:     10,
	token.KindSlash:    10,
	token.KindPercent:  10,
}

func isAssignOp(k token.Kind) bool {
	switch k {
	case token.KindEq, token.KindPlusEq, token.KindMinusEq, token.KindStarEq,
		token.KindSlashEq, token.KindPercentEq, token.KindAmpEq, token.KindPipeEq,
		token.KindCaretEq, token.KindShlEq, token.KindShrEq:
		return true
	}
	return false
}

func (p *parser) parseExpr() ast.Expr { return p.parseAssign() }

// parseAssign handles right-associative assignment.
func (p *parser) parseAssign() ast.Expr {
	lhs := p.parseBinary(1)
	if isAssignOp(p.cur().Kind) {
		op := p.bump()
		rhs := p.parseAssign()
		return &ast.AssignExpr{Op: op.Kind, Lhs: lhs, Rhs: rhs, Sp: token.Span{Start: lhs.Span().Start, End: rhs.Span().End}}
	}
	return lhs
}

func (p *parser) parseBinary(minPrec int) ast.Expr {
	lhs := p.parseCast()
	for {
		op := p.cur().Kind
		prec, ok := infixPrec[op]
		if !ok || prec < minPrec {
			return lhs
		}
		p.bump()
		rhs := p.parseBinary(prec + 1) // left-associative
		lhs = &ast.BinExpr{Op: op, L: lhs, R: rhs, Sp: token.Span{Start: lhs.Span().Start, End: rhs.Span().End}}
	}
}

func (p *parser) parseCast() ast.Expr {
	e := p.parseUnary()
	for p.at(token.KindAs) {
		p.bump()
		ty := p.parseType()
		e = &ast.CastExpr{X: e, Ty: ty, Sp: token.Span{Start: e.Span().Start, End: p.prevEnd()}}
	}
	return e
}

func (p *parser) parseUnary() ast.Expr {
	start := p.cur().Pos.Offset
	switch p.cur().Kind {
	case token.KindMinus, token.KindBang, token.KindStar:
		op := p.bump().Kind
		x := p.parseUnary()
		return &ast.UnExpr{Op: op, X: x, Sp: p.spanFrom(start)}
	case token.KindAmp:
		p.bump()
		mut := p.accept(token.KindMut)
		x := p.parseUnary()
		return &ast.UnExpr{Op: token.KindAmp, Mut: mut, X: x, Sp: p.spanFrom(start)}
	default:
		return p.parsePostfix()
	}
}

func (p *parser) parsePostfix() ast.Expr {
	e := p.parsePrimary()
	for {
		switch p.cur().Kind {
		case token.KindDot:
			p.bump()
			name := ""
			if p.at(token.KindInteger) {
				name = p.bump().Lit // tuple-struct field: `.0`
			} else {
				name = p.expectIdentPath("field or method name")
			}
			if p.at(token.KindLParen) {
				args := p.parseArgs()
				e = &ast.MethodCallExpr{Recv: e, Name: name, Args: args, Sp: token.Span{Start: e.Span().Start, End: p.prevEnd()}}
			} else {
				e = &ast.FieldExpr{Recv: e, Field: name, Sp: token.Span{Start: e.Span().Start, End: p.prevEnd()}}
			}
		case token.KindLBracket:
			p.bump()
			idx := p.parseExpr()
			p.expect(token.KindRBracket, " to close index expression")
			e = &ast.IndexExpr{Recv: e, Index: idx, Sp: token.Span{Start: e.Span().Start, End: p.prevEnd()}}
		case token.KindLParen:
			args := p.parseArgs()
			e = &ast.CallExpr{Callee: e, Args: args, Sp: token.Span{Start: e.Span().Start, End: p.prevEnd()}}
		case token.KindQuestion:
			p.bump()
			e = &ast.TryExpr{X: e, Sp: token.Span{Start: e.Span().Start, End: p.prevEnd()}}
		default:
			return e
		}
	}
}

// parseArrayLit parses `[a, b, c]` and the repeat form `[value; count]`.
func (p *parser) parseArrayLit() ast.Expr {
	start := p.cur().Pos.Offset
	p.bump() // [
	lit := &ast.ArrayLitExpr{}
	if p.at(token.KindRBracket) {
		p.bump()
		lit.Sp = p.spanFrom(start)
		return lit
	}
	first := p.parseExpr()
	if p.accept(token.KindSemicolon) {
		lit.Repeat = true
		lit.Value = first
		lit.Count = p.parseExpr()
	} else {
		lit.Elems = append(lit.Elems, first)
		for p.accept(token.KindComma) {
			if p.at(token.KindRBracket) {
				break
			}
			lit.Elems = append(lit.Elems, p.parseExpr())
		}
	}
	p.expect(token.KindRBracket, " to close array literal")
	lit.Sp = p.spanFrom(start)
	return lit
}

func (p *parser) parseArgs() []ast.Expr {
	var args []ast.Expr
	p.bump() // (
	for !p.at(token.KindRParen) && !p.at(token.KindEOF) {
		args = append(args, p.parseExpr())
		if !p.accept(token.KindComma) {
			break
		}
	}
	p.expect(token.KindRParen, " to close argument list")
	return args
}

func (p *parser) parsePrimary() ast.Expr {
	t := p.cur()
	start := t.Pos.Offset
	switch t.Kind {
	case token.KindInteger, token.KindFloat, token.KindString, token.KindRawString,
		token.KindChar, token.KindByteChar, token.KindTrue, token.KindFalse:
		p.bump()
		return &ast.LiteralExpr{Kind: t.Kind, Text: t.Lit, Sp: p.spanFrom(start)}

	case token.KindIdentifier:
		p.bump()
		path := []string{t.Lit}
		for p.accept(token.KindColonColon) {
			path = append(path, p.expectIdentPath("path segment"))
		}
		if p.at(token.KindLBrace) && p.noStructLit == 0 {
			return p.parseStructLit(path, start)
		}
		if len(path) == 1 {
			return &ast.IdentExpr{Name: path[0], Sp: p.spanFrom(start)}
		}
		return &ast.PathExpr{Segments: path, Sp: p.spanFrom(start)}

	case token.KindLParen:
		p.bump()
		if p.accept(token.KindRParen) {
			return &ast.TupleExpr{Sp: p.spanFrom(start)} // unit
		}
		first := p.parseExpr()
		if p.at(token.KindComma) {
			elems := []ast.Expr{first}
			for p.accept(token.KindComma) {
				if p.at(token.KindRParen) {
					break
				}
				elems = append(elems, p.parseExpr())
			}
			p.expect(token.KindRParen, " to close tuple")
			return &ast.TupleExpr{Elems: elems, Sp: p.spanFrom(start)}
		}
		p.expect(token.KindRParen, " to close parenthesized expression")
		return first

	case token.KindLBrace:
		return p.parseBlock()

	case token.KindLBracket:
		return p.parseArrayLit()

	case token.KindIf:
		return p.parseIf()

	case token.KindMatch:
		return p.parseMatch()

	case token.KindLoop:
		p.bump()
		body := p.parseBlock()
		return &ast.LoopExpr{Kind: token.KindLoop, Body: body, Sp: p.spanFrom(start)}

	case token.KindWhile:
		p.bump()
		p.noStructLit++
		cond := p.parseExpr()
		p.noStructLit--
		body := p.parseBlock()
		return &ast.LoopExpr{Kind: token.KindWhile, Cond: cond, Body: body, Sp: p.spanFrom(start)}

	case token.KindFor:
		p.bump()
		pat := p.parsePattern()
		p.expect(token.KindIn, " in `for` loop")
		p.noStructLit++
		iter := p.parseExpr()
		p.noStructLit--
		body := p.parseBlock()
		return &ast.LoopExpr{Kind: token.KindFor, Pat: pat, Iter: iter, Body: body, Sp: p.spanFrom(start)}

	case token.KindReturn, token.KindBreak, token.KindContinue:
		p.bump()
		j := &ast.JumpExpr{Kind: t.Kind}
		// `return <expr>` and `break <expr>` may take a value; `continue` never
		// does. A `,` (match arm / argument list) also ends the jump, so that
		// `_ => continue,` and `_ => break,` parse.
		if t.Kind != token.KindContinue &&
			!p.at(token.KindSemicolon) && !p.at(token.KindRBrace) &&
			!p.at(token.KindEOF) && !p.at(token.KindComma) {
			j.Val = p.parseExpr()
		}
		j.Sp = p.spanFrom(start)
		return j

	case token.KindUnsafe:
		p.bump()
		return p.parseBlock()

	case token.KindDefer:
		p.bump()
		x := p.parseExpr()
		p.errf(t, "E0900", "`defer` is not supported yet (design milestone M1)")
		return x

	default:
		p.errf(t, "E0202", "expected an expression, found %s", t.Kind)
		p.bump()
		return &ast.TupleExpr{Sp: p.spanFrom(start)} // unit placeholder
	}
}

func (p *parser) parseIf() ast.Expr {
	start := p.cur().Pos.Offset
	p.bump() // if
	p.noStructLit++
	cond := p.parseExpr()
	p.noStructLit--
	then := p.parseBlock()
	e := &ast.IfExpr{Cond: cond, Then: then}
	if p.accept(token.KindElse) {
		if p.at(token.KindIf) {
			e.Else = p.parseIf()
		} else {
			e.Else = p.parseBlock()
		}
	}
	e.Sp = p.spanFrom(start)
	return e
}

// isBlockLikeExpr reports whether e ends in `}`, so that a match arm using it as
// its body may omit the trailing comma.
func isBlockLikeExpr(e ast.Expr) bool {
	switch e.(type) {
	case *ast.Block, *ast.IfExpr, *ast.MatchExpr, *ast.LoopExpr:
		return true
	}
	return false
}

func (p *parser) parseMatch() ast.Expr {
	start := p.cur().Pos.Offset
	p.bump() // match
	p.noStructLit++
	scrut := p.parseExpr()
	p.noStructLit--
	m := &ast.MatchExpr{Scrutinee: scrut}
	if _, ok := p.expect(token.KindLBrace, " to begin match arms"); !ok {
		m.Sp = p.spanFrom(start)
		return m
	}
	for !p.at(token.KindRBrace) && !p.at(token.KindEOF) {
		astart := p.cur().Pos.Offset
		arm := ast.MatchArm{Pat: p.parsePattern()}
		if p.accept(token.KindIf) {
			arm.Guard = p.parseExpr()
		}
		p.expect(token.KindFatArrow, " in match arm")
		arm.Body = p.parseExpr()
		arm.Sp = p.spanFrom(astart)
		m.Arms = append(m.Arms, arm)
		if !p.accept(token.KindComma) {
			// A block-like body ends with `}`, so the comma is optional (as in
			// Rust): `0 => { a() } _ => { b() }`.
			if !p.at(token.KindRBrace) && !isBlockLikeExpr(arm.Body) {
				break
			}
		}
	}
	p.expect(token.KindRBrace, " to close match")
	m.Sp = p.spanFrom(start)
	return m
}

// --- types ----------------------------------------------------------------

func (p *parser) parseType() ast.Type {
	t := p.cur()
	start := t.Pos.Offset
	switch t.Kind {
	case token.KindAmp:
		p.bump()
		mut := p.accept(token.KindMut)
		return &ast.RefType{Mut: mut, Elem: p.parseType(), Sp: p.spanFrom(start)}
	case token.KindStar:
		p.bump()
		mut := p.accept(token.KindMut)
		return &ast.PtrType{Mut: mut, Elem: p.parseType(), Sp: p.spanFrom(start)}
	case token.KindLParen:
		p.bump()
		var elems []ast.Type
		for !p.at(token.KindRParen) && !p.at(token.KindEOF) {
			elems = append(elems, p.parseType())
			if !p.accept(token.KindComma) {
				break
			}
		}
		p.expect(token.KindRParen, " to close tuple type")
		return &ast.TupleType{Elems: elems, Sp: p.spanFrom(start)}
	case token.KindLBracket:
		p.bump()
		elem := p.parseType()
		if p.accept(token.KindSemicolon) {
			n := p.parseExpr()
			p.expect(token.KindRBracket, " to close array type")
			return &ast.ArrayType{Elem: elem, Len: n, Sp: p.spanFrom(start)}
		}
		p.expect(token.KindRBracket, " to close slice type")
		return &ast.SliceType{Elem: elem, Sp: p.spanFrom(start)}
	case token.KindFn:
		p.bump()
		var params []ast.Type
		p.expect(token.KindLParen, " in function type")
		for !p.at(token.KindRParen) && !p.at(token.KindEOF) {
			params = append(params, p.parseType())
			if !p.accept(token.KindComma) {
				break
			}
		}
		p.expect(token.KindRParen, " to close function type")
		f := &ast.FnType{Params: params}
		if p.accept(token.KindArrow) {
			f.Ret = p.parseType()
		}
		f.Sp = p.spanFrom(start)
		return f
	case token.KindIdentifier:
		p.bump()
		path := []string{t.Lit}
		for p.accept(token.KindColonColon) {
			path = append(path, p.expectIdentPath("type path segment"))
		}
		pt := &ast.PathType{Segments: path}
		if p.at(token.KindLt) {
			p.errf(p.cur(), "E0900", "generics are not supported yet (design milestone M1)")
			p.skipGenericArgs()
		}
		pt.Sp = p.spanFrom(start)
		return pt
	case token.KindUnderscore:
		p.bump()
		// `_` as an inferred type; represented as a single-segment path so the
		// checker can fill it in.
		return &ast.PathType{Segments: []string{"_"}, Sp: p.spanFrom(start)}
	default:
		p.errf(t, "E0202", "expected a type, found %s", t.Kind)
		p.bump()
		return &ast.PathType{Segments: []string{"<error>"}, Sp: p.spanFrom(start)}
	}
}

// --- small helpers --------------------------------------------------------

func (p *parser) expectIdent(context string) (string, bool) {
	if p.at(token.KindIdentifier) {
		return p.bump().Lit, true
	}
	p.errf(p.cur(), "E0202", "expected %s, found %s", context, p.cur().Kind)
	return "", false
}

// expectIdentPath is like expectIdent but returns a placeholder on error so
// callers can keep building a partial AST.
func (p *parser) expectIdentPath(_ string) string {
	if p.at(token.KindIdentifier) {
		return p.bump().Lit
	}
	p.errf(p.cur(), "E0202", "expected an identifier, found %s", p.cur().Kind)
	return "<error>"
}

// skipTo consumes tokens until one of stop is current (or EOF). It does not
// consume the stop token.
func (p *parser) skipTo(stop ...token.Kind) {
	for !p.at(token.KindEOF) {
		for _, k := range stop {
			if p.at(k) {
				return
			}
		}
		p.bump()
	}
}

// skipGenericArgs consumes a balanced `<...>` after a generics error.
func (p *parser) skipGenericArgs() {
	if !p.at(token.KindLt) {
		return
	}
	depth := 0
	for !p.at(token.KindEOF) {
		switch p.cur().Kind {
		case token.KindLt:
			depth++
		case token.KindGt:
			depth--
			if depth == 0 {
				p.bump()
				return
			}
		case token.KindSemicolon, token.KindLBrace:
			return
		}
		p.bump()
	}
}
