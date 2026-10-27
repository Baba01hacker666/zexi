package mir

import (
	"fmt"
	"strings"
)

// String renders the whole program: nominal type definitions followed by every
// function body. The output is deterministic (declaration order, no map
// iteration), so it can be compared in golden tests and diffed between
// compiler versions — which is the point of `zexic emit --emit=mir`.
func (p *Program) String() string {
	var sb strings.Builder
	for _, st := range p.Structs {
		fmt.Fprintf(&sb, "struct %s {\n", st.Name)
		for i, f := range st.Fields {
			fmt.Fprintf(&sb, "    %d: %s: %s,\n", i, f.Name, f.Ty)
		}
		sb.WriteString("}\n\n")
	}
	for _, en := range p.Enums {
		fmt.Fprintf(&sb, "enum %s {\n", en.Name)
		for _, v := range en.Variants {
			if len(v.Fields) == 0 {
				fmt.Fprintf(&sb, "    %d: %s,\n", v.Index, v.Name)
				continue
			}
			parts := make([]string, 0, len(v.Fields))
			for _, f := range v.Fields {
				parts = append(parts, f.Ty.String())
			}
			fmt.Fprintf(&sb, "    %d: %s(%s),\n", v.Index, v.Name, strings.Join(parts, ", "))
		}
		sb.WriteString("}\n\n")
	}
	for i, fn := range p.Fns {
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(fn.String())
	}
	return sb.String()
}

// String renders one function: its signature, its local table, and its blocks.
func (f *Fn) String() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "fn %s {\n", f.Signature())
	if f.Body == nil {
		sb.WriteString("    // extern declaration: no body\n}\n")
		return sb.String()
	}
	sb.WriteString("    locals:\n")
	for i, l := range f.Body.Locals {
		fmt.Fprintf(&sb, "        %s: %s\t// %s", LocalID(i), l.Ty, l.Kind)
		if l.Name != "" {
			fmt.Fprintf(&sb, " %s", l.Name)
		}
		if l.Mut {
			sb.WriteString(" (mut)")
		}
		sb.WriteString("\n")
	}
	for i, b := range f.Body.Blocks {
		fmt.Fprintf(&sb, "    %s: {\n", BlockID(i))
		for _, s := range b.Stmts {
			fmt.Fprintf(&sb, "        %s\n", stmtString(s))
		}
		if b.Term != nil {
			fmt.Fprintf(&sb, "        %s\n", termString(b.Term))
		}
		sb.WriteString("    }\n")
	}
	sb.WriteString("}\n")
	return sb.String()
}

func stmtString(s Stmt) string {
	switch st := s.(type) {
	case *Assign:
		return fmt.Sprintf("%s = %s;", st.Place, rvalueString(st.RV))
	case *Drop:
		if st.HasFlag {
			return fmt.Sprintf("drop(%s) with flag %s;", st.Place, st.Flag)
		}
		return fmt.Sprintf("drop(%s);", st.Place)
	case *SetDropFlag:
		return fmt.Sprintf("dropflag(%s) = %v;", st.Flag, st.Val)
	}
	return "<?>"
}

func rvalueString(r Rvalue) string {
	switch rv := r.(type) {
	case Use:
		return rv.Op.String()
	case *BinOp:
		return fmt.Sprintf("(%s %s %s)", rv.L, rv.Op, rv.R)
	case *UnOp:
		return fmt.Sprintf("%s%s", rv.Op, rv.X)
	case *Cast:
		return fmt.Sprintf("cast(%s as %s, %s)", rv.X, rv.To, rv.Kind)
	case *Aggregate:
		parts := make([]string, 0, len(rv.Ops))
		for _, o := range rv.Ops {
			parts = append(parts, o.String())
		}
		switch rv.Kind {
		case AggEnum:
			return fmt.Sprintf("%s { variant#%d: [%s] }", rv.Ty, rv.Variant, strings.Join(parts, ", "))
		default:
			return fmt.Sprintf("%s { %s }", rv.Kind, strings.Join(parts, ", "))
		}
	case *Ref:
		kw := "&"
		if rv.Mut {
			kw = "&mut "
		}
		return kw + rv.Place.String()
	case *Discriminant:
		return fmt.Sprintf("discriminant(%s)", rv.Place)
	case *Len:
		return fmt.Sprintf("len(%s)", rv.Place)
	case *Repeat:
		return fmt.Sprintf("[%s; %d]", rv.Op, rv.Count)
	}
	return "<?>"
}

func termString(t Terminator) string {
	switch tt := t.(type) {
	case *Goto:
		return fmt.Sprintf("goto -> %s;", tt.Target)
	case *SwitchInt:
		parts := make([]string, 0, len(tt.Cases)+1)
		for _, c := range tt.Cases {
			parts = append(parts, fmt.Sprintf("%d: %s", c.Val, c.Target))
		}
		parts = append(parts, "otherwise: "+tt.Otherwise.String())
		return fmt.Sprintf("switchInt(%s) -> [%s];", tt.Discr, strings.Join(parts, ", "))
	case *Call:
		args := make([]string, 0, len(tt.Args))
		for _, a := range tt.Args {
			args = append(args, a.String())
		}
		call := fmt.Sprintf("call %s(%s)", tt.Callee, strings.Join(args, ", "))
		if tt.Dest != nil {
			call = tt.Dest.String() + " = " + call
		}
		return fmt.Sprintf("%s -> [return: %s];", call, tt.Target)
	case *Return:
		return "return;"
	case *Unreachable:
		return "unreachable;"
	}
	return "<?>;"
}
