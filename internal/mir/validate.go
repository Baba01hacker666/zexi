package mir

import (
	"fmt"
	"strings"

	"zexi/internal/types"
)

// Validate checks the structural invariants a backend is allowed to assume:
// every block ends in exactly one terminator, every branch target exists, every
// place and local index is in range, places and operands carry types, drop flags
// name flag locals, and — the interesting one — no local is read before it must
// have been written on every path that reaches the read.
//
// Lowering is the only producer of MIR today, so a failure here is a compiler
// bug rather than a user error. That is exactly why it is worth checking: the
// tests call Validate on every lowered body, so a lowering mistake shows up as a
// precise "bb3 reads _7 before it is written" instead of a wrong answer at run
// time or a crash inside the C compiler.
func (p *Program) Validate() error {
	for _, fn := range p.Fns {
		if fn.Body == nil {
			continue
		}
		if err := fn.Body.validate(fn.Name); err != nil {
			return err
		}
	}
	return nil
}

func (b *Body) validate(name string) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("mir: %s: %s", name, fmt.Sprintf(format, args...))
	}
	if len(b.Blocks) == 0 {
		return fail("body has no blocks")
	}
	if len(b.Locals) == 0 {
		return fail("body has no locals (the return place is local 0)")
	}
	for i, l := range b.Locals {
		if l == nil || l.Ty == nil {
			return fail("local %s has no type", LocalID(i))
		}
	}
	for _, a := range b.Args {
		if !a.Valid() || int(a) >= len(b.Locals) {
			return fail("argument local %s is out of range", a)
		}
	}
	for bi, blk := range b.Blocks {
		if blk.Term == nil {
			return fail("%s has no terminator", BlockID(bi))
		}
		for _, s := range blk.Stmts {
			if err := b.checkStmt(name, s); err != nil {
				return err
			}
		}
		if err := b.checkTerm(name, blk.Term); err != nil {
			return err
		}
	}
	return b.checkInit(name)
}

func (b *Body) checkPlace(where string, p Place) error {
	if !p.Local.Valid() || int(p.Local) >= len(b.Locals) {
		return fmt.Errorf("mir: %s: place %s names a local out of range", where, p)
	}
	for _, pr := range p.Proj {
		switch pr.Kind {
		case ProjField, ProjDeref:
			if pr.Ty == nil {
				return fmt.Errorf("mir: %s: %s projection of %s has no type", where, pr.Kind, p)
			}
		case ProjIndex:
			if pr.Ty == nil {
				return fmt.Errorf("mir: %s: index projection of %s has no type", where, p)
			}
			if pr.Op.Kind == OpConst && pr.Op.Const.Kind != ConstInt {
				return fmt.Errorf("mir: %s: place %s indexes with a non-integer constant", where, p)
			}
			if pr.Op.Ty != nil && !pr.Op.Ty.Kind.IsInt() {
				return fmt.Errorf("mir: %s: place %s indexes with `%s`, which is not an integer", where, p, pr.Op.Ty)
			}
		case ProjDowncast:
			// A downcast only narrows an enum to one variant; the following
			// field projection supplies the type.
		default:
			return fmt.Errorf("mir: %s: place %s has an unknown projection kind", where, p)
		}
	}
	return nil
}

func (b *Body) checkOperand(where string, o Operand) error {
	if o.Ty == nil {
		return fmt.Errorf("mir: %s: %s operand `%s` has no type", where, o.Kind, o)
	}
	if o.Kind == OpCopy || o.Kind == OpMove {
		if err := b.checkPlace(where, o.Place); err != nil {
			return err
		}
	}
	if o.Kind == OpFn && o.Fn == nil {
		return fmt.Errorf("mir: %s: function operand has no callee", where)
	}
	return nil
}

func (b *Body) checkStmt(where string, s Stmt) error {
	switch st := s.(type) {
	case *Assign:
		if err := b.checkPlace(where, st.Place); err != nil {
			return err
		}
		return b.checkRvalue(where, st.RV)
	case *Drop:
		if err := b.checkPlace(where, st.Place); err != nil {
			return err
		}
		if st.HasFlag {
			return b.checkFlag(where, st.Flag)
		}
		return nil
	case *SetDropFlag:
		return b.checkFlag(where, st.Flag)
	}
	return fmt.Errorf("mir: %s: unknown statement", where)
}

func (b *Body) checkFlag(where string, id LocalID) error {
	if !id.Valid() || int(id) >= len(b.Locals) {
		return fmt.Errorf("mir: %s: drop flag %s is out of range", where, id)
	}
	if b.Locals[id].Kind != LocalDropFlag {
		return fmt.Errorf("mir: %s: drop flag %s is a %s local, not a flag", where, id, b.Locals[id].Kind)
	}
	return nil
}

func (b *Body) checkRvalue(where string, r Rvalue) error {
	switch rv := r.(type) {
	case Use:
		return b.checkOperand(where, rv.Op)
	case *BinOp:
		if err := b.checkOperand(where, rv.L); err != nil {
			return err
		}
		return b.checkOperand(where, rv.R)
	case *UnOp:
		return b.checkOperand(where, rv.X)
	case *Cast:
		return b.checkOperand(where, rv.X)
	case *Aggregate:
		for _, o := range rv.Ops {
			if err := b.checkOperand(where, o); err != nil {
				return err
			}
		}
		if rv.Ty == nil {
			return fmt.Errorf("mir: %s: aggregate has no type", where)
		}
		if rv.Kind == AggEnum && rv.Variant < 0 {
			return fmt.Errorf("mir: %s: enum aggregate has no variant", where)
		}
		return nil
	case *Ref:
		return b.checkPlace(where, rv.Place)
	case *Discriminant:
		return b.checkPlace(where, rv.Place)
	case *Len:
		return b.checkPlace(where, rv.Place)
	case *Repeat:
		if err := b.checkOperand(where, rv.Op); err != nil {
			return err
		}
		if rv.Ty == nil {
			return fmt.Errorf("mir: %s: repeat aggregate has no type", where)
		}
		return nil
	}
	return fmt.Errorf("mir: %s: unknown rvalue", where)
}

func (b *Body) checkTerm(where string, t Terminator) error {
	switch tt := t.(type) {
	case *Goto:
		return b.checkTarget(where, tt.Target)
	case *SwitchInt:
		if err := b.checkOperand(where, tt.Discr); err != nil {
			return err
		}
		if tt.Discr.Ty != nil && !tt.Discr.Ty.Kind.IsInt() &&
			tt.Discr.Ty.Kind != types.Bool && tt.Discr.Ty.Kind != types.Char {
			return fmt.Errorf("mir: %s: switchInt discriminant is `%s`, not an integer", where, tt.Discr.Ty)
		}
		if len(tt.Cases) == 0 {
			return fmt.Errorf("mir: %s: switchInt has no cases and no use", where)
		}
		for _, c := range tt.Cases {
			if err := b.checkTarget(where, c.Target); err != nil {
				return err
			}
		}
		return b.checkTarget(where, tt.Otherwise)
	case *Call:
		for _, a := range tt.Args {
			if err := b.checkOperand(where, a); err != nil {
				return err
			}
		}
		if tt.Dest != nil {
			if err := b.checkPlace(where, *tt.Dest); err != nil {
				return err
			}
		}
		if tt.Callee.Fn == nil && tt.Callee.Extern == "" && tt.Callee.Builtin == "" {
			return fmt.Errorf("mir: %s: call has no callee", where)
		}
		return b.checkTarget(where, tt.Target)
	case *Return, *Unreachable:
		return nil
	}
	return fmt.Errorf("mir: %s: unknown terminator", where)
}

func (b *Body) checkTarget(where string, target BlockID) error {
	if !target.Valid() || int(target) >= len(b.Blocks) {
		return fmt.Errorf("mir: %s: branch target %s is out of range", where, target)
	}
	return nil
}

// checkInit runs a forward "must be initialized" dataflow over the CFG.
//
// A local counts as initialized on entry to a block only when every predecessor
// initializes it; a block's own statements are then simulated in order. Drop
// flags count as initialized from the start: a flag is a plain bool that
// lowering sets before any drop can read it, and treating them as bookkeeping
// keeps the analysis about values rather than about flags.
//
// The fixpoint computes only the `in` sets. Errors are reported in a second,
// single pass, so a transient state during iteration can never produce a false
// diagnostic.
func (b *Body) checkInit(name string) error {
	n := len(b.Locals)
	nblocks := len(b.Blocks)

	entry := make([]bool, n)
	entry[0] = true // return place
	for _, a := range b.Args {
		if int(a) < n {
			entry[a] = true
		}
	}
	for i, l := range b.Locals {
		if l.Kind == LocalDropFlag {
			entry[i] = true
		}
	}

	// Reachability and predecessor edges, both from the entry block only.
	reachable := make([]bool, nblocks)
	preds := make([][]int, nblocks)
	reachable[0] = true
	for queue := []int{0}; len(queue) > 0; {
		bi := queue[0]
		queue = queue[1:]
		for _, s := range successors(b.Blocks[bi].Term) {
			si := int(s)
			if si < 0 || si >= nblocks {
				continue
			}
			preds[si] = append(preds[si], bi)
			if !reachable[si] {
				reachable[si] = true
				queue = append(queue, si)
			}
		}
	}

	in := make([][]bool, nblocks)
	for changed := true; changed; {
		changed = false
		for bi := 0; bi < nblocks; bi++ {
			if !reachable[bi] {
				continue
			}
			var cur []bool
			if bi == 0 {
				cur = entry
			} else {
				for _, p := range preds[bi] {
					if in[p] == nil {
						continue // predecessor not yet computed
					}
					out := b.simulate(p, in[p]).out
					if cur == nil {
						cur = append([]bool(nil), out...)
						continue
					}
					for i := range cur {
						cur[i] = cur[i] && out[i]
					}
				}
				if cur == nil {
					continue // no predecessor has produced a state yet
				}
			}
			if in[bi] == nil || !equalBools(in[bi], cur) {
				in[bi] = cur
				changed = true
			}
		}
	}

	for bi := 0; bi < nblocks; bi++ {
		if !reachable[bi] || in[bi] == nil {
			continue
		}
		sim := b.simulate(bi, in[bi])
		if sim.err != "" {
			return fmt.Errorf("mir: %s: %s: %s", name, BlockID(bi), sim.err)
		}
	}
	return nil
}

type simResult struct {
	out []bool
	err string
}

// simulate walks one block's statements over an initialized-locals set, marking
// definitions and (when reporting) recording the first uninitialized read.
func (b *Body) simulate(bi int, in []bool) simResult {
	cur := append([]bool(nil), in...)
	var firstErr string
	report := func(format string, args ...any) {
		if firstErr == "" {
			firstErr = fmt.Sprintf(format, args...)
		}
	}
	read := func(o Operand) {
		if o.Kind != OpCopy && o.Kind != OpMove {
			return
		}
		if !o.Place.Local.Valid() || int(o.Place.Local) >= len(cur) {
			return
		}
		if !cur[o.Place.Local] {
			report("reads %s before it is written", o.Place.Local)
		}
	}
	blk := b.Blocks[bi]
	for _, s := range blk.Stmts {
		switch st := s.(type) {
		case *Assign:
			checkRvalueReads(st.RV, read)
			if int(st.Place.Local) < len(cur) {
				cur[st.Place.Local] = true
			}
		case *Drop:
			if len(st.Place.Proj) == 0 && int(st.Place.Local) < len(cur) {
				cur[st.Place.Local] = false
			}
		case *SetDropFlag:
			if int(st.Flag) < len(cur) {
				cur[st.Flag] = true
			}
		}
	}
	switch t := blk.Term.(type) {
	case *SwitchInt:
		read(t.Discr)
	case *Call:
		for _, a := range t.Args {
			read(a)
		}
		if t.Dest != nil && int(t.Dest.Local) < len(cur) {
			cur[t.Dest.Local] = true
		}
	}
	return simResult{out: cur, err: firstErr}
}

func checkRvalueReads(r Rvalue, read func(Operand)) {
	switch rv := r.(type) {
	case Use:
		read(rv.Op)
	case *BinOp:
		read(rv.L)
		read(rv.R)
	case *UnOp:
		read(rv.X)
	case *Cast:
		read(rv.X)
	case *Aggregate:
		for _, o := range rv.Ops {
			read(o)
		}
	case *Repeat:
		read(rv.Op)
	}
}

func successors(t Terminator) []BlockID {
	switch tt := t.(type) {
	case *Goto:
		return []BlockID{tt.Target}
	case *SwitchInt:
		out := make([]BlockID, 0, len(tt.Cases)+1)
		for _, c := range tt.Cases {
			out = append(out, c.Target)
		}
		return append(out, tt.Otherwise)
	case *Call:
		return []BlockID{tt.Target}
	}
	return nil
}

func equalBools(a, b []bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// DumpLocalNames renders a `local = source name` table, for diagnostics that
// want to point at the binding a temporary came from.
func (b *Body) DumpLocalNames() string {
	var sb strings.Builder
	for i, l := range b.Locals {
		if l.Name == "" {
			continue
		}
		fmt.Fprintf(&sb, "%s = %s, ", LocalID(i), l.Name)
	}
	return strings.TrimSuffix(sb.String(), ", ")
}
