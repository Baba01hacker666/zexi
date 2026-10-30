# 5. Roadmap & Minimal Viable Feature Set

> **Progress:** M0 is largely landed. Working today: lexer, parser, name
> resolution, bidirectional type inference, constant folding, a flow-sensitive
> move checker, and a C backend that produces a runnable native binary. A
> second lowering now sits beside the C backend: `internal/mir` flattens the
> checked program into a control-flow graph (`--emit=mir`), covering 93% of this
> repository's reachable functions and checked against the C backend by
> differential tests — the groundwork for a swappable backend
> ([`13-mir.md`](13-mir.md)). The
> first M1 usability foundations are also live: file `use`/`import`, dotted
> standard-library facades (`import http.server;`), `mod` source layouts,
> per-module `as` aliases, and path-qualified lookup with `pub` enforcement are
> resolved by the driver before checking. Project builds are convention-only:
> pass a directory (`zexic run .`), the loader finds `main.zx` or `src/main.zx`,
> searches the project root for named imports, and can load a whole folder for
> one `use pkg;`. Modules are isolated: private items stay private, only
> public items are imported unqualified, and same-named declarations in two
> modules are lowered under distinct internal names. Files named together on
> the command line remain one flat program.
> Implemented language surface: functions and recursion; `let`/`const`;
> integer/float/bool/char/`str`; arithmetic, bitwise, comparison, and logical
> operators; `as` casts; `if`/`else`; `while`/`loop`/`for i in a..b` and
> `for x in arr` (value iteration over `Copy` elements); `print`/`println` with
> any number of values; `len(x)` / `x.len()`; structs (named and tuple) with
> `impl` methods (`self`, `&self`, `&mut self`) and associated functions
> (`Point::new(..)`); value-yielding `loop` (`break <expr>`); enums
> (unit/tuple/struct variants) with
> `match` over enums, integers, `bool`, and `char`, guards (`pat if cond`), and
> exhaustiveness checking; **monomorphized generics** on functions, structs,
> enums, and impls (inference, turbofish, generic methods), predeclared
> `Option<T>`/`Result<T,E>` with the `?` operator, and **owned enum payloads**
> with match move/drop glue; first-class
> arrays with bounds-checked indexing; `mut` enforcement (a non-`mut` binding
> cannot be assigned through, `&mut`-borrowed, or mutated via a `&mut self`
> method); use-after-move errors for non-`Copy` values (`#[copy]` opts in);
> `String` concatenation with `+`/`+=` (a number/`bool`/`char` is rendered as
> text) and implicit `String`→`str` coercion in
> argument position; and the built-in owning heap type **`String`** with
> **deterministic destruction (`Drop`)** — buffers are freed exactly once at
> scope exit, on early `return`/`break`/`continue`, and on reassignment, while
> moves transfer ownership and leave the source empty. `extern "C"`
> declarations bind C functions (libc), and several `.zx` files compile together
> into one program; on top of that, `lib/http.zx` is a working HTTP/1.x client
> and server (see docs/08), now with a Go-style default mux: top-level
> `handle_get("/users/{id}", user)` plus `listen(":8080")`, `{name}` route
> parameters, and `use_before`/`use_after` middleware.
> Serving many users is a pool of OS threads: `listen_workers`/`serve_workers*`
> accept on one listening socket from N threads, pinning an explicit router
> into a fixed C slot so workers can reach it through an integer handle;
> `thread::Pool` spreads a function over a range off the request path. Under
> them, `net` gained UDP, name resolution, readiness polling, bounded accept,
> and endpoint introspection. Two new embedded modules serve websites: `json`
> (exact integers, round-tripping doubles, ordered objects) and `html`
> (escape-by-default templating and widgets).
> Not yet implemented: traits and trait bounds (generic parameters are
> unconstrained), `dyn Trait`, associated types, const generics, `Box`,
> user-defined `Drop`, borrow-lifetime analysis (so bound
> `&`/`*` aliases are not tracked), match ergonomics (`ref` bindings, so an
> owned payload cannot be read out of a borrowed enum), nested patterns, and
> tuple values.
> See `docs/06-backend-strategy.md` for the backend status.

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
| Modules | file imports + `mod`/`use`/`import` preview | scoped packages, isolation, `pub` enforcement | incremental + parallel builds |
| Runtime | panic+exit only | opt-in `alloc`/`fmt` | freestanding `no_std` tuned |
| Backend | C11 | C11 (+ PGO passthrough) | native x86-64 (or Cranelift) |
| Std | `core` skeleton | `alloc`, `Vec`, `String`, `Iterator` | `os`/`sys`, collections |

## 5.2 M0 — the minimal viable feature set (the "must ship")

**Lexical/parse**
- Full lexer (done) and recursive-descent parser for the grammar in docs/02.
- A small practical extension is already shipped: `use`/`import` load files and
  embedded stdlib modules, and `mod name;` loads `name.zx` or `name/mod.zx`.
  Imports create qualifiers (`import math as m;` → `m::sqrt(2.0)`,
  `mod util;` → `util::f`) and qualified lookup enforces `pub` (E0603/E0412/
  E0433). Per-module isolation is live: private items are module-local, plain
  imports bring only public names into unqualified scope, and same-named
  declarations in separate modules coexist under distinct internal names.

**Types (M0)**
- Primitives: `bool i8 i16 i32 i64 u8 u16 u32 u64 isize usize f32 f64 char`.
- `()`, tuples, `[T; N]`, `[T]` + `&[T]`, `&T`, `&mut T`, `*T` (unsafe), `str`.
- `struct`, `enum` (tagged union) with `match`.
- Predeclared generic `Option<T>`/`Result<T,E>` with `?` propagation (lowered
  to `match`); the error type must match exactly (no `From` conversion yet).

**Declarations & statements**
- `fn`, `struct`, `enum`, `const`, `type` aliases.
- `let` / `let mut`, `const`, `impl` blocks with inherent methods.
- `if/else`, `match` (exhaustive), `while`, `loop`, `for x in a..b`.
- `return`, `break`/`continue`, `defer`, `unsafe {}`.
- Expression-oriented blocks.

**Ownership (M0, conservative)**
- Move semantics for non-`Copy` types; use-after-move = error.
- Deterministic destruction (`Drop`): a destructor runs for every live local at
  scope exit, on early `return`/`break`/`continue`, and before an overwritten
  value. Moves transfer ownership and null the source, so a moved-out value is
  never freed twice.
- The one heap type is the built-in `String` (an owning UTF-8 buffer). A
  `struct`, `enum` variant, or array containing a `String` is itself an owning
  value and gets a generated destructor; dropping it drops each
  field/element/payload.
- **Known M0 gaps (documented, not yet closed):** a `str` borrowed from a
  temporary `String` prolongs that
  temporary until the end of the statement; bound `&`/`*` aliases are not tracked
  across moves (no lifetime analysis). Full handling arrives with M1/M2.
- Explicit heap only via built-in heap types (`String` now, `Box<T>`/`Vec<T>` at
  M1).

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

- **Modules & packages:** scoped packages are live. Qualified lookup, `as`
  aliases, `pub` checks, directory entry points, whole-folder packages,
  per-module isolation, re-exports, and glob imports are implemented. A
  prelude module remains, as do manifests/package metadata (not planned for the
  convention-only project model).
- **Generics + traits:** monomorphized user generics, operator traits,
  `Iterator`, `Clone`/`Copy`/`Drop`, `From`/`Into`, `dyn Trait` (M1 late).
- **Real ownership checker:** NLL, lifetime elision + explicit lifetimes,
  conditional moves, drop flags.
- **Strings & collections:** `String` and `Vec<T>` are live; `HashMap` (in
  `alloc`) is the remaining piece, and needs a `Hash` trait to key on.
- **Formatting:** `println`/`print` accept any number of values (space separated)
  today; a full `println(fmt, args)` with a monomorphized formatting trait (no
  reflection) arrives with generics.
- **Attributes:** `#[repr(C)]`, `#[inline]`, `#[no_mangle]`, `#[export]` are
  live, as is `#[copy]`; an attribute the compiler does not know is E0539.
  `#[repr(C)]` is accepted and is already the default layout.
- **FFI:** `extern "C"` functions are live, in both directions — C calls into
  `#[export]`/`#[no_mangle]` functions and Zexi calls out through `extern "C"`
  declarations. `extern "C"` structs remain.
- **Concurrency (preview):** `thread` gives OS threads, `i64` channels, and
  `i64` atomics over a pthread shim, linked only when imported. Only `i64`
  values cross a thread boundary, which keeps the subset data-race-free; full
  `Send`/`Sync` value passing is the M2 item below. See
  [`11-concurrency.md`](11-concurrency.md).
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
- **Concurrency:** extend today's `i64`-only preview with `Send`/`Sync`, owning
  values and shared references crossing threads, `Mutex`/typed `Channel`, and
  data-race freedom checked by the borrow checker.
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
