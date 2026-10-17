# 5. Roadmap & Minimal Viable Feature Set

The MVP is milestone **M0**: enough language to compile real, safe, small
programs to standalone binaries, with every architectural seam in place so
M1/M2 are additive.

## 5.1 Milestone overview

| | M0 — "it compiles" | M1 — "it's usable" | M2 — "it's fast & small" |
| --- | --- | --- | --- |
| Goal | Compile a safe subset → C → binary | Real programs; modules; NLL | Native backend + size/speed tuning |
| Types | primitives, tuples, arrays, slices, structs, enums | + generics, traits, `dyn`, `str`/`String` | + const eval, SIMD, niche opt |
| Ownership | lexical scope, moves, explicit `drop` | full NLL borrow checker, lifetimes | escape analysis, drop elision |
| Errors | `Result` + `?`, panic abort | + `Option` ergonomics, `?` in `main` | zero-cost error paths |
| Modules | single file | packages (`mod`/`use`/`pub`) | incremental + parallel builds |
| Runtime | panic+exit only | opt-in `alloc`/`fmt` | freestanding `no_std` tuned |
| Backend | C11 | C11 (+ PGO passthrough) | native x86-64 (or Cranelift) |
| Std | `core` skeleton | `alloc`, `Vec`, `String`, `Iterator` | `os`/`sys`, collections |

## 5.2 M0 — the minimal viable feature set (the "must ship")

**Lexical/parse**
- Full lexer (done) and recursive-descent parser for the grammar in docs/02.
- Single-file programs, no `use`/`mod` (those are M1), but the AST/parser already
  model them so M1 is a lowering change, not a rewrite.

**Types (M0)**
- Primitives: `bool i8 i16 i32 i64 u8 u16 u32 u64 isize usize f32 f64 char`.
- `()`, tuples, `[T; N]`, `[T]` + `&[T]`, `&T`, `&mut T`, `*T` (unsafe), `str`.
- `struct`, `enum` (tagged union) with `match`.
- `Option<T>`, `Result<T,E>` as library enums.
- No generics yet — but built-in `Option`/`Result`/slices are hard-coded in the
  type checker so user programs are already useful.

**Declarations & statements**
- `fn`, `struct`, `enum`, `const`, `type` aliases.
- `let` / `let mut`, `const`, `impl` blocks with inherent methods.
- `if/else`, `match` (exhaustive), `while`, `loop`, `for x in a..b`.
- `return`, `break`/`continue`, `defer`, `unsafe {}`.
- Expression-oriented blocks.

**Ownership (M0, conservative)**
- Move semantics for non-`Copy` types; use-after-move = error.
- Borrows by lexical scope (a borrow lives until end of block) — stricter than
  needed, deliberately simple.
- `Drop`/destructor calls inserted at scope exit.
- Explicit heap only via a built-in `Box<T>` in the `alloc` feature.

**Codegen**
- MIR → C11 for x86-64 and AArch64 via the system `cc`.
- Bounds checks on slice/array indexing (always on in M0).
- Debug overflow checks; release wraps.

**Driver**
- `zexi check|build|run` with `--emit=ast|hir|mir|c|bin`, `-o`, `--opt-level`.
- Diagnostics with spans and stable codes; golden tests.

**The M0 acceptance test:** compile a program that (a) defines a struct and an
enum, (b) uses `match`, loops, and slices with bounds checks, (c) allocates and
frees a heap value through `Box`, (d) returns a `Result` propagated with `?`,
(e) prints via a tiny `write(2)` wrapper — to a **statically linked binary under
64 KB** that passes its tests. That single program exercises every architectural
seam.

## 5.3 M1 — usability (the "worth using" set)

- **Modules & packages:** `mod`, `use`, `pub`, directory packages, prelude.
- **Generics + traits:** monomorphized user generics, operator traits,
  `Iterator`, `Clone`/`Copy`/`Drop`, `From`/`Into`, `dyn Trait` (M1 late).
- **Real ownership checker:** NLL, lifetime elision + explicit lifetimes,
  conditional moves, drop flags.
- **Strings & collections:** `String`, `Vec<T>`, `HashMap` (in `alloc`).
- **Formatting:** `println!`-equivalent without macros — `println(fmt, args)` with
  a monomorphized formatting trait (no reflection).
- **Attributes:** `#[repr(C)]`, `#[inline]`, `#[no_mangle]`, `#[export]`.
- **FFI:** `extern "C"` functions and structs, for libc and C libraries.
- **Incremental + parallel builds**; LSP/`zexi fmt` usable.

## 5.4 M2 — the performance/size endgame

- **Native backend:** hand-written x86-64 and AArch64 emitters, or Cranelift
  integrated behind the same MIR contract. Goal: drop the C toolchain dependency
  and control layout/size fully.
- **Optimizations:** MIR inlining, escape analysis → stack promotion,
  bounds/overflow-check elision, drop elision, const evaluation/`comptime`,
  niche optimization for `Option<&T>`/`Option<Box<T>>`, PGO.
- **Freestanding target:** `no_std` + direct syscalls, no libc; sub-20 KB
  stripped binaries for computational programs.
- **Concurrency:** `Send`/`Sync`, threads, `Mutex`/`Channel`, data-race freedom
  checked by the borrow checker.
- **Self-hosting (stretch):** rewrite `zexic` in Zexi once `String`/`Vec`/`Map`/IO
  are mature.

## 5.5 What we will *not* add without strong evidence

- Macros (use generics + `comptime` first).
- Exceptions / stack unwinding.
- A GC or any interpreter/VM.
- Operator overloading beyond trait-implemented operators.
- Implicit conversions or coercion soup.
- Inheritance.
- Reflection/RTTI.

## 5.6 Risks & mitigations

| Risk | Mitigation |
| --- | --- |
| Borrow checker too strict (M0 lexical) frustrates users | Ship M1 NLL fast; keep M0 explicitly labeled conservative |
| C backend leaks implementation details into semantics | Backend-conformance tests; MIR is the contract |
| Monomorphization code bloat | DCE + `opt-level=s` + "instantiate only used" |
| Panic-abort limits libraries | Provide `catch`-style boundaries only where an ABI allows (M2) |
| Scope creep destroys simplicity | The simplicity budget in docs/01 §1.5 is a gate on every RFC |
| Self-hosting never happens | It is explicitly a stretch goal, not a credibility dependency |
