# 13. MIR — the mid-level IR

This chapter describes `internal/mir`, the control-flow IR that the language
front end produces and that backends are meant to consume (`docs/04 §4.3`,
`docs/06 §6.5`).

**Status: landed for the computational core; the default backend is still C.**
`zexic emit --emit=mir` lowers and prints MIR for 93% of the reachable functions
in `tests/`, `examples/`, and `bench/`; every body it produces passes the
invariants in `Validate`, and the subset it covers is checked for *semantic*
equivalence against the C backend by a differential test. The C backend still
walks the typed AST directly, so MIR is a second, verified description of the
same program — and the opt-in **LLVM backend** (`zexic --backend=llvm`,
`--emit=llvm`) is its first real consumer: it lowers the same MIR to LLVM IR
and links through `clang`.

---

## 13.1 Why a second IR at all

The C backend lowers a tree: expressions nest, and value-producing `if`/blocks
are emitted with GNU statement expressions `({ ... })`. That works, and it is how
Zexi got a working compiler quickly, but it has three consequences:

1. **Every backend re-implements the language.** Adding an LLVM or x86-64
   backend today means a second AST walker, and then keeping two walkers in step
   forever.
2. **Ownership is emergent.** `codegen` tracks `dropFrames`, `dropTypes`,
   `loopFrames` and `trapDepth` while it prints, so correctness depends on
   interactions between textual scopes rather than on a data structure.
3. **Checks cannot be reasoned about.** A bounds check is buried inside an
   emitted expression, so no pass can prove it away.

MIR addresses all three by flattening each function into numbered locals and
basic blocks, and by making drops, moves, and checks explicit instructions.

## 13.2 Shape

```
Program ── Fn* ── Body ── BasicBlock* ── Stmt* + Terminator
                     └── Local*
```

* **Locals** are typed for their whole lifetime. Local `_0` is the return place,
  `_1.._N` are the parameters, then `let` bindings, then temporaries, then drop
  flags. There is no shadowing.
* **Blocks** are straight-line statement lists ending in exactly one terminator.
* **Places** name storage: a local plus a projection path (`field`, `index`,
  `deref`, `downcast`).
* **Operands** are a copy of a place, a move of a place, a constant, or a
  function reference.

A whole function, as `zexic emit --emit=mir` prints it:

```
fn add(i64, i64) -> i64 {
    locals:
        _0: i64	// return
        _1: i64	// arg a
        _2: i64	// arg b
    bb0: {
        _0 = (copy _1 + copy _2);
        return;
    }
}
```

An enum match becomes a switch on the tag, with payload binding in the arm:

```
    bb0: {
        _2 = move _1;
        _3 = discriminant(_2);
        switchInt(copy _3) -> [0: bb2, 1: bb3, 2: bb4, otherwise: bb5];
    }
    bb3: {
        _4 = move _2 as variant#1.0;
        _5 = &_4;
        _0 = call norm1(copy _5) -> [return: bb6];
    }
```

## 13.3 The instruction set

Statements:

| Statement | Meaning |
| --- | --- |
| `p = rvalue` | write a value into a place |
| `drop(p) with flag f` | release an owned value, if the flag says it is still there |
| `dropflag(f) = true/false` | record that a value is present or was moved away |

Rvalues:

| Rvalue | Meaning |
| --- | --- |
| `copy p` / `move p` | read a place without / with taking ownership |
| `a <op> b` | arithmetic, comparison, bitwise. `&&`/`||` appear only when both sides are already evaluated; short-circuit evaluation of source expressions becomes control flow |
| `-x`, `!x` | prefix operators |
| `cast(x as T, kind)` | integer/float/bool/char/pointer conversions |
| `struct { .. }`, `enum { variant#n: [..] }`, `array { .. }`, `[x; N]` | aggregates |
| `&p`, `&mut p` | borrow |
| `discriminant(p)` | an enum's tag |
| `len(p)` | length of an array or `str` |

Terminators:

| Terminator | Meaning |
| --- | --- |
| `goto -> bbN` | unconditional branch |
| `switchInt(v) -> [c: bbX, otherwise: bbY]` | branch on an integer or bool |
| `_d = call f(args) -> [return: bbN]` | call, then continue |
| `return` | return the value in `_0` |
| `unreachable` | control cannot get here |

Calls name their callee as one of three things: a lowered Zexi function, an
`extern "C"` symbol, or a **runtime intrinsic** — a string such as
`String::push_str`, `Vec::push`, or `println`. MIR deliberately does not model
the built-in owning types; it names the operation and lets each backend map the
name onto its runtime.

Interpolation `f"..."` builds an owning `String` with these intrinsics, in
source order:

| Intrinsic | Contract |
| --- | --- |
| `String::new()` | Create an empty owning `String` |
| `String::as_str(&String)` | Borrow a `str` view |
| `String::push_str(&mut String, str)` | Append a string view |
| `String::push(&mut String, char)` | Append a UTF-8 character |
| `String::push_i64` / `push_u64` | Append a decimal `i64` / `u64` |
| `String::push_f64` | Append an `f64` with C `printf`'s default `%g` precision |

Numeric arguments are explicitly widened to the intrinsic's type, and booleans
branch to append `"true"` or `"false"`. Interpolated `String` values are
borrowed, not moved; a temporary source lives until its part is appended and is
then dropped. The builder moves into the result after the last part. Normal
`String`-to-`str` call arguments use the same borrowing operation.

## 13.4 Ownership and drops

Every owning local and by-value parameter gets a **drop flag**. It is `true`
when the value is initialized and is set to `false` at the point the value is
moved out. A borrowed temporary under construction has a false flag until its
initializer completes, so an early exit does not drop uninitialized storage.
At scope exit the body emits `drop(local) with flag`, which does nothing when
the value has already moved away. This is what makes conditional moves safe
without path analysis in the backend:

```
    dropflag(_f1) = false;     // _1 will not be dropped
    _3 = move _1;              // transfers ownership of the argument
```

Scope exits on `return`, `break`, and `continue` emit their own drops before
jumping, so a value is released exactly once on every path. The move checker in
`internal/sema` has already proven that no place is read after a move; MIR only
has to place the drops.

**Known gap: partial moves.** Moving one field out of a struct or one payload out
of a variant needs a flag per *path*, not per local. Since drop flags are per
local today, lowering reports ordinary moves out of owning fields or elements
as unsupported rather than risking a double free. Pattern binding has a narrow
exception for a variant's single owning field, which transfers the whole
drop obligation. This is the top item to fix when drop elaboration grows
per-field flags.

## 13.5 Lowering

`mir.Lower(info)` walks the checked program:

* **Reachability first.** Only functions reachable from `main` (and from
  `#[no_mangle]` entry points) are lowered, matching what the C backend emits.
* **Everything that already desugared, stays desugared.** `if let` and
  `while let` are two-arm matches from the parser; `?` is a match recorded in
  `Info.TryMatches`; generics are monomorphized before MIR. Lowering sees only
  the ordinary forms.
* **Failure is data.** A construct with no lowering is reported as
  `Unsupported{fn, feature, span}` and that function is skipped — the rest of the
  program still lowers. Callers of a skipped function are skipped too, to a
  fixpoint, so nothing in the IR calls a bodyless function.
* **Divergence is implicit.** After a `return`, lowering keeps walking the AST to
  unwind its own recursion, but `emit` is a no-op in a terminated block, so no
  instructions land after a terminator.

Nothing has to be handled in one go: coverage grows construct by construct, and
the unsupported list is the progress metric.

## 13.6 Verification

Three layers, in increasing strength:

1. **`Validate`** — structural invariants (one terminator per block, targets in
   range, typed places and operands, drop flags are flags) plus a
   definite-initialization dataflow over the CFG that reports a local read on a
   path where it may not have been written.
2. **`TestValidateEveryBody` and the corpus test** — every body lowered from the
   repository's own programs is validated, so a lowering regression fails CI.
3. **A reference interpreter, used as an oracle.** `interp.go` executes MIR for
   the computational subset and selected `String` operations. Differential
   tests compile each program with the real C backend, run it, interpret the
   MIR, and compare stdout and exit codes. The interpolation cases are
   mandatory (they do not skip unsupported operations) and cover formatting,
   borrowing, evaluation order, early exits, and numeric widths. The interpreter
   does not model allocation or destruction; separate MIR path tests check
   ownership flags, temporary drops, and drops on early returns.

The interpreter is **not** an execution mode: Zexi's non-goals include no
interpreter and no VM (`docs/01 §1.2`). It exists inside `internal/mir` for tests
only.

## 13.7 What is not lowered yet

As of this writing, `tests/`+`examples/`+`bench/` lower 413 of 444 reachable
functions. Interpolation removed eight previous blockers; four functions that
previously lowered are now reported because their partial moves could otherwise
double-drop. The remaining causes include:

| Cause | Notes |
| --- | --- |
| Indirect calls through function values | needs a callee *operand* in `Call`, not just a `FuncRef` |
| `defer` | needs scope-exit action lists ordered before drops |
| `for x in collection` | needs an iterator protocol (or an index loop over `len`) |
| Indexing `Vec`/`HashMap` | currently array-only; would become a runtime intrinsic |
| Tuple values | tuples are not implemented in the language yet |
| Partial moves | see §13.4 |
| Qualified constants | e.g. a module-level `const` reached through a path |

## 13.8 Adding a backend

A backend needs:

1. **Layout.** `internal/layout` provides sizes, alignments, field offsets,
   and enum tags; its ABI rules are checked against the C compiler.
2. **A translation for each statement, terminator, and rvalue** in §13.3 — this
   is a few hundred lines, not a language re-implementation.
3. **Drop glue.** `drop(p)` for a struct or enum is a call to a generated helper
   that drops each owning field, guarded per field by its own flag if partial
   moves are supported.
4. **Runtime intrinsics.** `println`, `String::*`, `Vec::*`, `HashMap::*`. The C
   runtime in `internal/codegen/c` already implements them; a native backend
   would need its own.
5. **A conformance run.** The differential test harness is the template: same
   program, same expected output, both backends.

The endgame in `docs/06 §6.4` is unchanged: keep C through M1, then put the
native emitter (or Cranelift) behind this contract, with C as the reference
implementation and portable fallback.
