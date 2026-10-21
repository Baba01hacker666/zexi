package driver

// This file implements per-module name isolation on top of the loader's flat
// merge. Each top-level declaration gets a symbol; a private declaration, or a
// public one whose name is also declared by another module, is lowered under a
// unique internal name. Every reference is then rewritten against the module's
// own scope, so same-named private helpers can coexist and a private name is
// not accidentally visible from another module. Public unique names keep their
// spelling, preserving the historical convenience of unqualified imports.

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"zexi/internal/ast"
	"zexi/internal/diag"
)

// itemsOf returns the top-level items owned by m. Inline modules carry their
// own item slice; ordinary file modules use the parsed file.
func itemsOf(m *module) []ast.Item {
	if m == nil {
		return nil
	}
	if m.items != nil {
		return m.items
	}
	if m.file != nil {
		return m.file.Items
	}
	return nil
}

// buildSymbols creates the declaration table for every module, including
// inline modules and directory packages, and assigns each module a stable id.
func (l *loader) buildSymbols() {
	seen := map[*module]bool{}
	var add func(*module)
	add = func(m *module) {
		if m == nil || seen[m] {
			return
		}
		seen[m] = true
		l.nextModID++
		m.id = l.nextModID
		l.buildModuleSymbols(m)
		for _, name := range sortedSubmodNames(m) {
			add(m.submods[name])
		}
	}
	for _, m := range l.order {
		add(m)
	}
	for _, key := range sortedModuleKeys(l.mods) {
		add(l.mods[key])
	}
}

func sortedSubmodNames(m *module) []string {
	names := make([]string, 0, len(m.submods))
	for name := range m.submods {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedModuleKeys(mods map[string]*module) []string {
	keys := make([]string, 0, len(mods))
	for key := range mods {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// buildModuleSymbols records the declarations directly owned by m. Methods and
// enum variants are deliberately not symbols: they are resolved through their
// type name and never need a module-qualified spelling.
func (l *loader) buildModuleSymbols(m *module) {
	if m.symbols == nil {
		m.symbols = map[string]*symbol{}
	}
	for _, it := range itemsOf(m) {
		var sym *symbol
		switch d := it.(type) {
		case *ast.FnDecl:
			sym = &symbol{name: d.Name, mod: m, kind: symFn, pub: bool(d.Vis), decl: d}
		case *ast.StructDecl:
			sym = &symbol{name: d.Name, mod: m, kind: symStruct, pub: bool(d.Vis), decl: d}
		case *ast.EnumDecl:
			sym = &symbol{name: d.Name, mod: m, kind: symEnum, pub: bool(d.Vis), decl: d}
		case *ast.ConstDecl:
			sym = &symbol{name: d.Name, mod: m, kind: symConst, pub: bool(d.Vis), decl: d}
		case *ast.TypeAlias:
			sym = &symbol{name: d.Name, mod: m, kind: symAlias, pub: bool(d.Vis), decl: d}
		}
		if sym == nil {
			continue
		}
		// A duplicate in one module is left for sema's E0428. Keep the first
		// symbol so references still resolve while the diagnostic is reported.
		if _, dup := m.symbols[sym.name]; !dup {
			m.symbols[sym.name] = sym
		}
	}
}

// initExports prepares every module's qualified-export table from its own
// public declarations.
func (l *loader) initExports() {
	for _, m := range l.allModules() {
		if m.exportSyms == nil {
			m.exportSyms = map[string]*symbol{}
		}
		for name, sym := range m.symbols {
			if sym.pub {
				m.exportSyms[name] = sym
				if m.exports == nil {
					m.exports = map[string]bool{}
				}
				m.exports[name] = true
			}
		}
	}
}

// resolveSymbolImports computes re-exports and the unqualified import scope.
// Re-exports are iterated to a fixpoint because a facade may re-export another
// facade.
func (l *loader) resolveSymbolImports() {
	l.initExports()
	mods := l.allModules()

	for changed := true; changed; {
		changed = false
		for _, m := range mods {
			// A directory package exports the union of its member modules.
			for _, member := range m.members {
				for name, sym := range member.exportSyms {
					if !sym.pub {
						continue
					}
					if m.exportSyms[name] != sym {
						m.exportSyms[name] = sym
						if m.exports == nil {
							m.exports = map[string]bool{}
						}
						m.exports[name] = true
						changed = true
					}
				}
			}
			for _, it := range itemsOf(m) {
				d, ok := it.(*ast.UseDecl)
				if !ok || !bool(d.Vis) {
					continue
				}
				target := l.useTargets[d]
				if target == nil {
					continue
				}
				for name, sym := range target.exportSyms {
					if !sym.pub {
						continue
					}
					if m.exportSyms[name] != sym {
						m.exportSyms[name] = sym
						if m.exports == nil {
							m.exports = map[string]bool{}
						}
						m.exports[name] = true
						changed = true
					}
				}
			}
		}
	}

	// Unqualified imports: a plain `use foo;` (or an explicit `use foo::*;`)
	// brings foo's public names into this module's scope. An aliased import
	// stays qualified, so two modules with the same public name can be used
	// side by side as `a::helper()` and `b::helper()`.
	for _, m := range mods {
		for _, it := range itemsOf(m) {
			d, ok := it.(*ast.UseDecl)
			if !ok || !importsUnqualified(d) {
				continue
			}
			target := l.useTargets[d]
			if target == nil {
				continue
			}
			for name, sym := range target.exportSyms {
				if !sym.pub {
					continue
				}
				if _, local := m.symbols[name]; local {
					continue
				}
				if prev, ok := m.importSyms[name]; ok {
					if prev != sym {
						if m.ambiguous == nil {
							m.ambiguous = map[string][]string{}
						}
						m.ambiguous[name] = appendUniqueString(m.ambiguous[name], describeSymbol(prev))
						m.ambiguous[name] = appendUniqueString(m.ambiguous[name], describeSymbol(sym))
					}
					continue
				}
				if m.importSyms == nil {
					m.importSyms = map[string]*symbol{}
				}
				m.importSyms[name] = sym
			}
		}
	}
}

func importsUnqualified(d *ast.UseDecl) bool {
	return d.As == "" || d.Glob
}

func appendUniqueString(xs []string, s string) []string {
	if slices.Contains(xs, s) {
		return xs
	}
	return append(xs, s)
}

func describeSymbol(sym *symbol) string {
	name := "?"
	if sym != nil {
		name = sym.name
	}
	mod := "?"
	if sym != nil && sym.mod != nil {
		mod = sym.mod.key
	}
	return mod + "::" + name
}

// allModules returns every module in a deterministic order: load order first,
// then inline/package modules by key.
func (l *loader) allModules() []*module {
	seen := map[*module]bool{}
	var out []*module
	var add func(*module)
	add = func(m *module) {
		if m == nil || seen[m] {
			return
		}
		seen[m] = true
		out = append(out, m)
		for _, name := range sortedSubmodNames(m) {
			add(m.submods[name])
		}
	}
	for _, m := range l.order {
		add(m)
	}
	for _, key := range sortedModuleKeys(l.mods) {
		add(l.mods[key])
	}
	return out
}

// assignInternalNames decides which declarations keep their source spelling
// and gives every other declaration a collision-free internal name.
func (l *loader) assignInternalNames() {
	count := map[string]int{}
	for _, m := range l.allModules() {
		for name := range m.symbols {
			count[name]++
		}
	}
	for _, m := range l.allModules() {
		for name, sym := range m.symbols {
			switch {
			case sym.kind == symFn && name == "main" && m == l.entry:
				sym.internal = "main"
			case keepsExternalName(sym):
				sym.internal = name
			case (sym.pub || m.flattenPrivate) && count[name] == 1:
				sym.internal = name
			default:
				sym.internal = fmt.Sprintf("__zx$%d$%s", m.id, name)
			}
		}
	}
}

// keepsExternalName reports declarations whose C symbol is part of an external
// ABI and therefore cannot be renamed.
func keepsExternalName(sym *symbol) bool {
	fn, ok := sym.decl.(*ast.FnDecl)
	if !ok {
		return false
	}
	if fn.Extern != "" || fn.Body == nil {
		return true
	}
	for _, a := range fn.Attrs {
		name := strings.TrimSpace(a)
		if name == "no_mangle" || name == "export" || strings.HasPrefix(name, "export(") || strings.HasPrefix(name, "no_mangle(") {
			return true
		}
	}
	return false
}

// moduleScope is the name-resolution view of one module while its AST is being
// rewritten.
type moduleScope struct {
	l *loader
	m *module
}

func (s moduleScope) item(name string) *symbol {
	if s.m != nil {
		if sym, ok := s.m.symbols[name]; ok {
			return sym
		}
		if mods, ok := s.m.ambiguous[name]; ok && len(mods) > 1 {
			return nil
		}
		if sym, ok := s.m.importSyms[name]; ok {
			return sym
		}
	}
	return nil
}

// describeAmbiguity reports the modules that export an ambiguous imported name.
func (s moduleScope) describeAmbiguity(name string) []string {
	if s.m == nil {
		return nil
	}
	return s.m.ambiguous[name]
}

// moduleForPrefix returns the target module for the longest matching import
// prefix, if any.
func (s moduleScope) moduleForPrefix(segs []string) (*importBinding, int) {
	var bind *importBinding
	best := 0
	if s.m == nil {
		return nil, 0
	}
	for i := range s.m.imports {
		b := &s.m.imports[i]
		if len(b.prefix) > best && len(segs) >= len(b.prefix) && equalPath(segs[:len(b.prefix)], b.prefix) {
			best = len(b.prefix)
			bind = b
		}
	}
	return bind, best
}

// resolvePath rewrites a path to the internal name of the symbol it names.
// It returns nil when the path names no module symbol (a builtin, a local, or
// an enum variant segment), in which case the caller leaves it untouched.
func (s moduleScope) resolvePath(segs []string, sp ast.Node) []string {
	if len(segs) == 0 {
		return nil
	}
	if bind, n := s.moduleForPrefix(segs); bind != nil {
		rest := segs[n:]
		modName := strings.Join(bind.prefix, "::")
		if len(rest) == 0 {
			s.l.em.Report(diag.New("E0433", "`%s` is a module and cannot be used as a value or type", modName).WithSpan(sp.Span()))
			return nil
		}
		pub, known := bind.target.exports[rest[0]]
		if !known {
			s.l.em.Report(diag.New("E0412", "module `%s` has no item named `%s`", modName, rest[0]).WithSpan(sp.Span()))
			return nil
		}
		if !pub {
			s.l.em.Report(diag.New("E0603", "item `%s::%s` is private", modName, rest[0]).WithSpan(sp.Span()))
			return nil
		}
		sym := bind.target.exportSyms[rest[0]]
		if sym == nil {
			s.l.em.Report(diag.New("E0412", "module `%s` has no item named `%s`", modName, rest[0]).WithSpan(sp.Span()))
			return nil
		}
		out := make([]string, 0, len(rest))
		out = append(out, sym.internal)
		out = append(out, rest[1:]...)
		return out
	}
	if sym := s.item(segs[0]); sym != nil {
		out := make([]string, 0, len(segs))
		out = append(out, sym.internal)
		out = append(out, segs[1:]...)
		return out
	}
	if mods := s.describeAmbiguity(segs[0]); len(mods) > 1 {
		s.l.em.Report(diag.New("E0659", "`%s` is ambiguous; it is exported by %s", segs[0], strings.Join(mods, " and ")).WithSpan(sp.Span()))
	}
	return nil
}

// resolveIdent rewrites a bare identifier that names a module-level item. A
// local binding always wins, so a parameter may shadow a function.
func (s moduleScope) resolveIdent(name string, sp ast.Node) (string, bool) {
	if s.m != nil {
		if sym, ok := s.m.symbols[name]; ok {
			return sym.internal, true
		}
	}
	if mods := s.describeAmbiguity(name); len(mods) > 1 {
		s.l.em.Report(diag.New("E0659", "`%s` is ambiguous; it is exported by %s", name, strings.Join(mods, " and ")).WithSpan(sp.Span()))
		return "", false
	}
	if s.m != nil {
		if sym, ok := s.m.importSyms[name]; ok {
			return sym.internal, true
		}
	}
	return "", false
}
