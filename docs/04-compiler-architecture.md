# 4. Compiler Architecture

The compiler `zexic` is written in Go. It is a **batch, whole-program, ahead-of-time
compiler**: it reads Zexi source and produces a standalone native executable.
There is no interpreter and no bytecode VM in the shipped toolchain.

## 4.1 Why AOT → native, and why C as the M0 backend

**No VM/interpreter** is a hard requirement. A VM would add a runtime image,
startup cost, and a JIT — all hostile to the size goal, and it would make the
memory model's "static drop" story harder (objects would need runtime type info).

For the **M0 backend** we emit **C11** and invoke the system toolchain
(`cc`/`clang`) rather than writing a machine-code emitter immediately. Rationale:

- C is *itself* compiled AOT to native binaries; it is a compile target, not an
  interpreter — the constraint is satisfied.
- We inherit a mature, heavily-tuned optimizer → "Rust-like speed" without
  writing a register allocator first.
- We inherit linkers, section GC (`-ffunction-sections -fdata-sections
  -Wl,--gc-sections`), LTO, and cross-compilation → "C-like size" quickly.
- Emitted C is debuggable (gdb/`-g`) and auditable, which is invaluable while
  the ownership checker matures.

Trade-off: the C backend cannot express everything precisely (e.g. it relies on
compiler-inserted `free` calls and `__attribute__` for `never`/noreturn,
alignment). We accept this for M0 and reserve **M2** for a native backend
(hand-written x86-64/AArch64 emitter *or* Cranelift) once the IR is stable. The
IR is designed so the C backend is one of several backends, not the architecture.

Emitted C is never shown to users as the product, and we ship `--emit=c` only as
a debugging/testing aid (the golden-file test corpus uses it).

## 4.2 Pipeline

```
            ┌──────────┐
 .zx source │  lexer   │ tokens(+spans)
            └────┬─────┘
                 ▼
            ┌──────────┐
            │  parser  │ AST
            └────┬─────┘
                 ▼
            ┌──────────────┐   name resolution, macro-free
            │   resolve    │   module tree, scope graph
            └──────┬───────┘
                   ▼
            ┌──────────────┐   bidirectional type inference,
            │  typecheck   │   trait/bound resolution, coercion
            └──────┬───────┘
                   ▼
            ┌──────────────┐   ownership, moves, borrows (NLL),
            │  borrowck    │   lifetime inference, drop elaboration
            └──────┬───────┘
                   ▼
            ┌──────────────┐   typed, desugared, monomorphized,
            │     HIR      │   drop points explicit
            └──────┬───────┘
                   ▼
            ┌──────────────┐   SSA-ish CFG: locals lowered to
            │     MIR      │   temps, control flow explicit
            └──────┬───────┘
                   ▼            passes: inline, DCE, fold,
            ┌──────────────┐   bounds-check elision, escape→stack
            │  mir-opt     │
            └──────┬───────┘
                   ▼
        ┌──────────┴───────────┐
        ▼                      ▼
  ┌───────────┐         ┌────────────┐
  │ codegen/c │         │ codegen/   │   (M2) codegen/llvm (optional)
  └─────┬─────┘         │ x86_64     │
        │               └─────┬──────┘
        ▼                     ▼
   foo.c ──cc──▶ a.out     object files ──ld──▶ a.out
                (standalone native binary)
```

Key invariant: **every stage is independently testable and dumpable**
(`--emit=ast|hir|mir|c|asm|bin`).

## 4.3 Intermediate representations

### AST
Faithful to source: spans on every node, no desugaring. Used for diagnostics,
formatting (`zexi fmt`), and the LSP.

### HIR (High-level IR)
- Names are **resolved** to IDs (no strings).
- Types are **fully inferred and explicit** on every expression.
- Generics are **monomorphized** (an instantiation table maps `foo::<i32>` →
  concrete item).
- Desugaring done: `for` → iterator calls, `?` → `match`, `defer` → scoped drop,
  operators → trait methods where user-defined.
- **Drop points are explicit** as `Drop(x)` terminators.
- **Ownership annotations** (moved/borrowed/borrowed-mut) are materialized here;
  HIR is the first "provably safe" form.

### MIR (Mid-level IR)
- Per-function **control-flow graph** of basic blocks.
- SSA-like: unlimited virtual registers; `let` bindings become places.
- Explicit `Drop`, `Call`, `Ref`, `Discriminant`, and bounds-checked `Index`
  projections; every `move` transfers ownership and clears a drop flag.
- All addresses/aggregates explicit; no method calls remain.
- **This is where checks are optimized:** bounds-check elision, overflow-check
  removal, drop-elision after move, escape analysis for stack promotion.

**Implemented** in `internal/mir` (`docs/13-mir.md`): the IR, a lowering from
`sema.Info`, a validator, a textual dump (`--emit=mir`), and a reference
interpreter used as a test oracle. It covers the computational core — 92% of the
reachable functions in this repository lower today. Not yet on the production
path: the C backend still walks the typed AST, so nothing consumes MIR outside
the MIR tests. Deliberate deviations from the plan above, recorded so they are
choices rather than drift:

- **No separate HIR.** The checker's `sema.Info` is the typed, monomorphized,
  desugared program (`?` and `if let` are already lowered; generics are already
  instantiated), so MIR lowers from it directly.
- **No `StorageLive`/`StorageDead` yet.** Locals are declared in the body's local
  table and initialized by assignment; stack-slot lifetimes become a backend
  concern when the native emitter lands, which is also when these statements earn
  their place.
- **The pass pipeline (`mir/opt`) does not exist yet.** Checks are represented so
  they *can* be elided — a `ProjIndex` carries a `Checked` bit — but no pass
  clears it yet.

### Backend input contract
A backend consumes MIR + a type/layout table and must be able to emit: functions,
statics, the drop glue, the panic entry, and (if used) allocator calls. Adding a
backend must not require touching the front end — enforced by an interface and a
MIR-conformance test suite that runs N backends against the same golden output.

## 4.4 Go package layout

```
cmd/zexic/                 CLI driver
internal/
  token/                   TokenKind, Token, Position, Span
  lexer/                   hand-written scanner (done)
  ast/                     AST node types + walkers
  parser/                  recursive-descent + Pratt for exprs
  diag/                    Diagnostic, Emitter, error codes (E0xxx)
  resolve/                 module tree, name/scope resolution
  types/                   type repr, inference (HM-lite + bidirectional)
  check/                   typecheck + trait/bound solving
  borrowck/                ownership, move/borrow/lifetime checking
  hir/                     HIR nodes + lowering from AST
  mir/                     MIR CFG + lowering + validator + test interpreter (done)
  mir/opt/                 inlining, DCE, const-fold, bounds elision (future)
  layout/                  size/align/field offsets, ABI (done, verified vs cc)
  codegen/c/               MIR → C11 (M0 backend)
  codegen/x86_64/          native emitter (M2)
  link/                    toolchain invocation, LTO, static/dynamic
  driver/                  pipeline orchestration, incremental cache
testdata/                  golden tests: .zx → expected diagnostics/exit
```

Parallelism: modules type-check and lower concurrently (worker pool); MIR
optimization is per-function parallel. The incremental cache keys on a
content hash of (source, flags) per module.

## 4.5 Type checking strategy

- **Bidirectional inference** (`check` vs `infer` modes) rather than full
  Hindley–Milner: function signatures are always written, so we avoid
  let-polymorphism complexity while getting local inference.
- Integer literal defaulting to `i32` at unconstrained sites.
- **Traits** are solved with a small, terminating solver: supertraits, blanket
  impls, associated types (M2). Coherence is enforced (no overlapping impls).
- Errors are collected, not fatal: report multiple diagnostics per compile where
  the AST permits, always with a `Span` and a stable code.

## 4.6 Ownership/borrow checking strategy

- Build a **move/borrow dataflow** over MIR-lite *before* MIR proper (on HIR),
  tracking each place's state: `Uninit | Owned | Moved | Borrowed(sh) |
  Borrowed(mut)`.
- Compute **NLL** by liveness on the CFG: a borrow's region is the set of program
  points from the borrow to its last use.
- **Drop elaboration:** insert `Drop` at scope exit and after conditional moves;
  flag use-after-move and double-drop.
- M0 ships a conservative **lexical** version (borrow lasts to end of block) to
  get programs running; M1 switches the same dataflow to NLL. The interface is
  identical, so the backend never changes.

## 4.7 Binary size strategy (first-class, not an afterthought)

1. **No default std/runtime.** `#![no_std]` semantics are the default.
2. **Dead-code elimination at MIR** before codegen (unused monomorphizations,
   unused traits, unused drop glue).
3. **Monomorphize only used instantiations.**
4. **Section GC** at link: `-ffunction-sections -fdata-sections
   -Wl,--gc-sections -Wl,--strip-all`.
5. **`-Os`** in the size profile; `opt-level` = `debug|size|speed`.
6. **No RTTI/reflection/exception tables**; panics abort instead of unwinding.
7. **Static linking** by default (`-static` where the target supports it).

## 4.8 Speed strategy

1. **Monomorphized generics** → direct calls, inlinable.
2. **Value semantics** + escape analysis → stack allocation.
3. **MIR inlining** for small/`#[inline]` functions before codegen; the C
   backend then sees pre-inlined code and tunes further with `-O2`.
4. **Bounds/overflow check elision** in provable cases.
5. **No GC pauses**; deterministic memory.
6. **Profile-guided optimization** passthrough (`--pgo`) in M2.

## 4.9 CLI

```
zexi build [file.zx] [flags]      compile to a native binary
zexi run   [file.zx] [flags]      build to a temp path and execute
zexi check [file.zx]              parse + typecheck + borrowck only
zexi fmt   [file.zx]              format source
zexi doc   ...                    generate docs (M2)
zexi emit  --emit=<mir|c> file.zx     (ast/hir/asm/bin are planned)
zexi lsp   [entry.zx]             language server on stdio (LSP)

Options:
  -o <path>            output path
  --opt-level=<0|1|2|3|s>   default: 2 (speed); 's' for size profile
  --target=<triple>    cross-compile (via backend toolchain)
  --static / --dynamic
  --no-std             freestanding (default for libs)
  --edition=2026
  --emit               dump an intermediate form
```

Exit codes: `0` ok, `1` diagnostics, `2` I/O or toolchain error. Diagnostics are
machine-readable with `--message-format=json` for editors.

## 4.10 Diagnostics

Every token carries a `Span` (file id + byte range + line/col) from the lexer
onward. Diagnostics have a severity, a stable code (`E0102`), a primary span,
optional notes and a suggested fix. Example:

```
error[E0308]: mismatched types
  --> examples/hello.zx:4:18
   |
 4 |     let n: i32 = "hi";
   |                  ^^^^ expected `i32`, found `str`
   |
   = help: use `let n: i32 = 42;` or convert explicitly with `parse::<i32>()`
```

The rendering is shared by CLI and the language server (`zexic lsp`); the
emitter is the only place that formats, so `--message-format` is a pure view
change. Spans live in one shared offset space across every file a program
loads, so each diagnostic resolves to the file and position it came from even
when the error is in an imported file.

## 4.11 Testing strategy

- **Golden tests:** `testdata/**/*.zx` with expected `.stderr`/`.stdout`/exit
  codes; run at the `check` level and at the `run` level.
- **Backend conformance:** same MIR → assert equivalent observable behavior
  across C and native backends.
- **Fuzzing:** `go test -fuzz` on the lexer and parser (must never panic; must
  always terminate). A parsing panic is a bug even on invalid input.
- **Self-hosting path (long term):** because the compiler is in Go, self-hosting
  is *not* required for credibility; it is a stretch goal after M2, once the
  language has a string/collection/io story.

## 4.12 Bootstrapping reality check

The compiler is free to use Go's GC, maps, and strings — Zexi's size/speed goals
constrain *Zexi programs*, not the compiler's own memory. This separation is
deliberate: it keeps the compiler simple and lets us move fast, which matters
more than dogfooding early.
