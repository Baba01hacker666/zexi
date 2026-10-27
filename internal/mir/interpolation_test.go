package mir_test

import (
	"os"
	"path/filepath"
	"testing"

	"zexi/internal/mir"
	"zexi/internal/types"
)

// Unlike the general differential suite, these tests must never skip on an
// unsupported operation: interpolation and this String subset are the feature
// under test. Expected output also keeps both engines from agreeing on the
// same mistake.
func TestInterpolationAgainstC(t *testing.T) {
	cases := []struct {
		name, source, want string
	}{
		{
			"literals",
			`fn main() -> i32 {
    let name = "world";
    println(f"");
    println(f"hello, {name}! {{ok}}\nnext\tline");
    println(f"{true} {false} {'A'} {'é'} {'😀'}");
    0
}`,
			"\nhello, world! {ok}\nnext\tline\ntrue false A é 😀\n",
		},
		{
			"numeric_widths_and_rounding",
			`fn main() -> i32 {
    let a: i8 = -7;
    let b: i16 = -32768;
    let c: i32 = -2147483647 - 1;
    let d: i64 = -9223372036854775807 - 1;
    let e: u8 = 255;
    let f: u16 = 65535;
    let g: u32 = 4294967295;
    let h: u64 = (-1) as u64;
    let i: f32 = 0.1234564999;
    let j: f64 = 1234567.89;
    println(f"{a},{b},{c},{d}");
    println(f"{e},{f},{g},{h}");
    println(f"{i},{j},{-0.0}");
    println(f"{255 as i8},{65535 as i16},{4294967295 as i32}");
    0
}`,
			"-7,-32768,-2147483648,-9223372036854775808\n" +
				"255,65535,4294967295,18446744073709551615\n" +
				"0.123457,1.23457e+06,-0\n-1,-1,-1\n",
		},
		{
			"narrow_arithmetic_wraps",
			`fn main() -> i32 {
    let a: i8 = -128;
    let b: i8 = 1;
    let c: u8 = 255;
    let d: u8 = 1;
    println(f"{a - b},{c + d},{(a - b) / 2},{-a},{(c + d) >> 1}");
    println(a - b, c + d, -a, (c + d) >> 1, (a - b) > b);
    let f: f32 = 16777216.0;
    println(f"{(f + (1.0 as f32)) - f}");
    0
}`,
			"127,0,63,-128,0\n127 0 -128 0 true\n0\n",
		},
		{
			"borrowed_strings_fields_and_temporaries",
			`struct Holder { name: String }
fn make() -> String { String::from("temporary") }
fn nested() -> String { let n = 2; f"nested:{n}" }
fn main() -> i32 {
    let mut owner = String::from("owned");
    let holder = Holder { name: String::from("field") };
    let names = [String::from("zero"), String::from("one")];
    let msg = f"[{owner}|{owner}|{holder.name}|{names[1]}|{make()}]";
    owner.push_str("!");
    println(msg);
    println(owner, holder.name, names[1]);
    println(f"({nested()}) {make().as_str()}");
    0
}`,
			"[owned|owned|field|one|temporary]\nowned! field one\n(nested:2) temporary\n",
		},
		{
			"evaluation_order",
			`fn next(n: &mut i32) -> i32 {
    *n += 1;
    println(*n);
    *n
}
fn main() -> i32 {
    let mut n: i32 = 0;
    let msg = f"{next(&mut n)}:{next(&mut n)}:{n}";
    println(msg);
    println(n);
    0
}`,
			"1\n2\n1:2:2\n2\n",
		},
		{
			"branches_and_early_return",
			`fn describe(b: bool) -> String {
    if b { f"yes:{b}" } else { f"no:{b}" }
}
fn early(b: bool) -> i32 {
    println(f"begin:{ { if b { return 7; } 9 } }:end");
    8
}
fn text() -> String { String::from("created") }
fn consume(s: String) -> i32 { len(s) as i32 }
fn early_arg(b: bool) -> i32 {
    consume(f"A{ { if b { return 19; } 5 } }")
}
fn early_string(b: bool) -> i32 {
    println(f"txt={ { if b { return 13; } text() } }");
    17
}
fn main() -> i32 {
    println(describe(true));
    println(describe(false));
    println(early(false));
    println(early(true));
    println(early_string(false));
    println(early_string(true));
    println(early_arg(false));
    println(early_arg(true));
    0
}`,
			"yes:true\nno:false\nbegin:9:end\n8\n7\ntxt=created\n17\n13\n2\n19\n",
		},
		{
			"string_operations_and_views",
			`fn main() -> i32 {
    let mut s = String::with_capacity(12);
    println(s.is_empty());
    s.push('é');
    s.push_str("abc");
    let mut c = s.clone();
    c.push('!');
    println(f"{s.as_str()}:{s.len()}:{c}:{len(c)}");
    c.clear();
    println(f"empty={c.is_empty()} len={c.len()}");
    println(s);
    let mut doubled = String::from("go");
    doubled.push_str(doubled.as_str());
    println(doubled);
    0
}`,
			"true\néabc:5:éabc!:6\nempty=true len=0\néabc\ngogo\n",
		},
		{
			"implicit_string_borrows",
			`struct Counter { marker: i32 }
impl Counter { fn measure(&self, s: str) -> usize { len(s) } }
fn length(s: str) -> usize { len(s) }
fn tail() -> String { String::from("-temp") }
fn identity(s: String) -> String { s }
fn main() -> i32 {
    let owner = String::from("zexi");
    let copy = String::from(owner);
    let saved = identity(tail());
    let mut dest = String::from(">");
    dest.push_str(owner);
    dest.push_str(tail());
    let r = &owner;
    let counter = Counter { marker: 0 };
    println(f"{length(owner)}:{counter.measure(owner)}:{length(tail())}:{r.len()}:{dest}:{copy}:{owner}:{saved}");
    0
}`,
			"4:4:5:4:>zexi-temp:zexi:zexi:-temp\n",
		},
		{
			"index_borrow_snapshots_index",
			`fn main() -> i32 {
    let mut a: [i32; 2] = [10, 20];
    let mut i: usize = 0;
    {
        let r = &mut a[i];
        i = 1;
        *r = 77;
    }
    println(f"{a[0]},{a[1]},{i}");
    0
}`,
			"77,20,1\n",
		},
		{
			"embedded_nul",
			`fn main() -> i32 {
    let mut s = String::from("a");
    s.push(0 as char);
    s.push('b');
    let msg = f"({s})";
    println(f"{msg}:{s.len()}");
    let embedded = f"u{0 as char}v";
    println(f"{embedded}:{embedded.len()}");
    0
}`,
			"(a):3\nu:3\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tc.name+".zx")
			if err := os.WriteFile(path, []byte(tc.source), 0o644); err != nil {
				t.Fatal(err)
			}
			native := runNative(t, path)
			interpreted, err := runMIR(t, path)
			if err != nil {
				t.Fatalf("MIR could not run: %v", err)
			}
			if native.stdout != tc.want || native.exit != 0 {
				t.Errorf("native: stdout %q, exit %d; want %q, exit 0", native.stdout, native.exit, tc.want)
			}
			if interpreted.stdout != native.stdout || interpreted.exit != native.exit {
				t.Errorf("MIR: stdout %q, exit %d; native stdout %q, exit %d", interpreted.stdout, interpreted.exit, native.stdout, native.exit)
			}
		})
	}
}

// Check the actual ownership and intrinsic contract as well as the output. The
// interpreter deliberately does not allocate or free, so output alone cannot
// detect a double-drop, a drop of uninitialized storage, or a moved owner.
func TestInterpolationMIRContracts(t *testing.T) {
	res := lowerString(t, `
fn make() -> String { String::from("made") }
fn consume(s: String) -> i32 { len(s) as i32 }
fn identity(s: String) -> String { s }
fn early_arg(b: bool) -> i32 {
    consume(f"A{ { if b { return 19; } 5 } }")
}
fn early(b: bool) -> i32 {
    let owner = String::from("own");
    println(f"start:{owner}:{make()}:{ { if b { return 7; } make() } }:{b}:{-3}:{5 as u64}:{0.5}:{'é'}");
    println(owner);
    8
}
fn main() -> i32 { println(early(false)); println(early(true)); println(early_arg(false)); println(early_arg(true)); println(identity(make())); 0 }
`)
	if len(res.Unsupported) != 0 {
		t.Fatalf("interpolation did not lower: %v", res.Unsupported)
	}
	if err := res.Prog.Validate(); err != nil {
		t.Fatal(err)
	}
	body := res.Prog.FnByName("early").Body
	var owner mir.LocalID = -1
	for i, local := range body.Locals {
		if local.Name == "owner" {
			owner = mir.LocalID(i)
		}
	}
	if !owner.Valid() {
		t.Fatal("owner local not found")
	}

	seen := map[string]int{}
	var firstMake *mir.Call
	for _, block := range body.Blocks {
		for _, stmt := range block.Stmts {
			if a, ok := stmt.(*mir.Assign); ok {
				if use, ok := a.RV.(mir.Use); ok && use.Op.Kind == mir.OpMove && use.Op.Place.Local == owner {
					t.Error("interpolation moved the original owning String")
				}
			}
		}
		call, ok := block.Term.(*mir.Call)
		if !ok {
			continue
		}
		for _, arg := range call.Args {
			if arg.Kind == mir.OpMove && arg.Place.Local == owner {
				t.Error("interpolation passed the original owning String by move")
			}
		}
		if call.Callee.Fn != nil && call.Callee.Fn.Name == "make" && firstMake == nil {
			firstMake = call
		}
		name := call.Callee.Builtin
		switch name {
		case mir.BnStringAsStr:
			seen[name]++
			checkStringReceiver(t, call, false)
			if call.Dest == nil || body.Ty(*call.Dest).Kind != types.Str {
				t.Errorf("%s should return str", name)
			}
		case mir.BnStringPushStr, mir.BnStringPush, mir.BnStringPushI64, mir.BnStringPushU64, mir.BnStringPushF64:
			seen[name]++
			checkStringReceiver(t, call, true)
			if call.Dest != nil || len(call.Args) != 2 {
				t.Errorf("%s should have two arguments and return unit", name)
				continue
			}
			want := map[string]types.Kind{
				mir.BnStringPushStr: types.Str, mir.BnStringPush: types.Char,
				mir.BnStringPushI64: types.I64, mir.BnStringPushU64: types.U64,
				mir.BnStringPushF64: types.F64,
			}[name]
			if call.Args[1].Ty == nil || call.Args[1].Ty.Kind != want {
				t.Errorf("%s argument has type %v, want %s", name, call.Args[1].Ty, (&types.Type{Kind: want}).String())
			}
		}
	}
	for _, name := range []string{mir.BnStringAsStr, mir.BnStringPushStr, mir.BnStringPush, mir.BnStringPushI64, mir.BnStringPushU64, mir.BnStringPushF64} {
		if seen[name] == 0 {
			t.Errorf("missing intrinsic %s", name)
		}
	}

	// The result of make() is borrowed and dropped immediately after its bytes
	// are appended, before evaluating the following interpolation part.
	if firstMake == nil || firstMake.Dest == nil {
		t.Fatal("temporary-producing call not found")
	}
	temp := firstMake.Dest.Local
	asStr, ok := body.Blocks[firstMake.Target].Term.(*mir.Call)
	if !ok || asStr.Callee.Builtin != mir.BnStringAsStr {
		t.Fatal("temporary is not immediately borrowed as str")
	}
	appendCall, ok := body.Blocks[asStr.Target].Term.(*mir.Call)
	if !ok || appendCall.Callee.Builtin != mir.BnStringPushStr {
		t.Fatal("borrowed temporary is not immediately appended")
	}
	dropped := false
	for _, stmt := range body.Blocks[appendCall.Target].Stmts {
		if d, ok := stmt.(*mir.Drop); ok && d.Place.Local == temp && d.HasFlag {
			dropped = true
		}
	}
	if !dropped {
		t.Error("temporary String was not dropped after appending")
	}

	checkStringDropPaths(t, body)
	checkStringDropPaths(t, res.Prog.FnByName("early_arg").Body)
	checkStringDropPaths(t, res.Prog.FnByName("consume").Body)
	checkStringDropPaths(t, res.Prog.FnByName("identity").Body)
}

func checkStringReceiver(t *testing.T, call *mir.Call, mut bool) {
	t.Helper()
	if len(call.Args) == 0 || call.Args[0].Ty == nil {
		t.Errorf("%s missing its receiver", call.Callee.Builtin)
		return
	}
	typ := call.Args[0].Ty
	if typ.Kind != types.Ref || typ.Mut != mut || typ.Elem == nil || typ.Elem.Kind != types.StringKind {
		t.Errorf("%s has receiver %s, want &%sString", call.Callee.Builtin, typ, map[bool]string{true: "mut "}[mut])
	}
}

// Explore the acyclic fixture's paths, interpreting only String initialization,
// moves, and drop flags. Both sides of a switch are visited. An active drop must
// name a live, initialized value, including on the return from inside a part.
func checkStringDropPaths(t *testing.T, body *mir.Body) {
	t.Helper()
	type state struct {
		live  []bool
		flags []bool
	}
	clone := func(s state) state {
		return state{live: append([]bool(nil), s.live...), flags: append([]bool(nil), s.flags...)}
	}
	initial := state{live: make([]bool, len(body.Locals)), flags: make([]bool, len(body.Locals))}
	for _, arg := range body.Args {
		initial.live[arg] = true
	}
	var walk func(mir.BlockID, state, int)
	walk = func(id mir.BlockID, s state, depth int) {
		if depth > len(body.Blocks) {
			t.Fatal("fixture unexpectedly contains a loop")
		}
		block := body.Blocks[id]
		move := func(op mir.Operand) {
			if op.Kind == mir.OpMove && op.Place.IsLocal() && body.Locals[op.Place.Local].Ty.Kind == types.StringKind {
				if !s.live[op.Place.Local] {
					t.Errorf("%s moves an uninitialized String %s", id, op.Place.Local)
				}
				s.live[op.Place.Local] = false
			}
		}
		for _, stmt := range block.Stmts {
			switch st := stmt.(type) {
			case *mir.SetDropFlag:
				s.flags[st.Flag] = st.Val
			case *mir.Assign:
				if use, ok := st.RV.(mir.Use); ok {
					move(use.Op)
				}
				if ref, ok := st.RV.(*mir.Ref); ok && ref.Place.IsLocal() && body.Locals[ref.Place.Local].Ty.Kind == types.StringKind && !s.live[ref.Place.Local] {
					t.Errorf("%s borrows an uninitialized String %s", id, ref.Place.Local)
				}
				if st.Place.IsLocal() && body.Locals[st.Place.Local].Ty.Kind == types.StringKind {
					s.live[st.Place.Local] = true
				}
			case *mir.Drop:
				if body.Locals[st.Place.Local].Ty.Kind == types.StringKind && (!st.HasFlag || s.flags[st.Flag]) {
					if !s.live[st.Place.Local] {
						t.Errorf("%s drops an uninitialized or already-dropped String %s", id, st.Place.Local)
					}
					s.live[st.Place.Local] = false
				}
			}
		}
		switch term := block.Term.(type) {
		case *mir.Goto:
			walk(term.Target, s, depth+1)
		case *mir.Call:
			for _, arg := range term.Args {
				move(arg)
			}
			if term.Dest != nil && term.Dest.IsLocal() && body.Locals[term.Dest.Local].Ty.Kind == types.StringKind {
				s.live[term.Dest.Local] = true
			}
			walk(term.Target, s, depth+1)
		case *mir.SwitchInt:
			for _, c := range term.Cases {
				walk(c.Target, clone(s), depth+1)
			}
			walk(term.Otherwise, clone(s), depth+1)
		case *mir.Return:
			for i, local := range body.Locals {
				if local.Kind != mir.LocalReturn && local.Ty.Kind == types.StringKind && s.live[i] {
					t.Errorf("%s returns while String %s is still live", id, mir.LocalID(i))
				}
			}
		case *mir.Unreachable:
		default:
			t.Errorf("unexpected terminator %T", term)
		}
	}
	walk(0, initial, 0)
}

// Implicit String-to-str coercions and method calls must use the same typed
// intrinsic contract as interpolation, including through a &String receiver.
func TestStringCallOperandTypes(t *testing.T) {
	res := lowerString(t, `
fn length(s: str) -> usize { len(s) }
fn main() -> i32 {
    let owner = String::from("zexi");
    let copy = String::from(owner);
    let mut dest = String::new();
    dest.push_str(owner);
    let r = &owner;
    println(f"{length(owner)}:{r.len()}:{copy}:{dest}");
    0
}`)
	if len(res.Unsupported) != 0 {
		t.Fatalf("unexpected unsupported: %v", res.Unsupported)
	}
	if err := res.Prog.Validate(); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, block := range res.Prog.Main.Body.Blocks {
		call, ok := block.Term.(*mir.Call)
		if !ok {
			continue
		}
		name := call.Callee.Builtin
		if call.Callee.Fn != nil {
			name = call.Callee.Fn.Name
		}
		switch name {
		case "length", "String::from":
			seen[name]++
			if len(call.Args) != 1 || call.Args[0].Ty == nil || call.Args[0].Ty.Kind != types.Str {
				t.Errorf("%s must take one str, got %v", name, call.Args)
			}
		case "String::len":
			seen[name]++
			checkStringReceiver(t, call, false)
		case mir.BnStringPushStr:
			seen[name]++
			checkStringReceiver(t, call, true)
			if len(call.Args) != 2 || call.Args[1].Ty == nil || call.Args[1].Ty.Kind != types.Str {
				t.Errorf("%s must take a str, got %v", name, call.Args)
			}
		}
	}
	for _, name := range []string{"length", "String::from", "String::len", mir.BnStringPushStr} {
		if seen[name] == 0 {
			t.Errorf("missing call to %s", name)
		}
	}
}
