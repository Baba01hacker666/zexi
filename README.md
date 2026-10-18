# Zexi

Zexi is an experimental systems programming language: **memory-safe like Rust, as small as C, as fast as Rust, with a minimalist surface like Rust — without the runtime baggage of Go.**

This repository contains the **initial design and specification** of Zexi: the viable feature set (M0–M2) and the compiler architecture. The compiler is written in **Go**.

> Status: **M0-alpha, working end to end.** `zexic` parses, type-checks, runs a
> flow-sensitive move checker, lowers to C, and links a **native binary** you can
> run.
>
> **C bridge:** since Zexi compiles to C, C libraries are first-class —
> `extern "C"` declarations (generated from headers by `zexic bindgen`),
> `cstr`/`from_cstr` string marshalling, and `--csrc`/`--link` to compile
> and link C code in (see `docs/09-c-bridge.md` and `examples/cbridge.zx`;
> `lib/http.zx` is a full worked example over BSD sockets).
>
> Easy to write, like Python: `#` or `//` comments; semicolons optional at
> line ends; `f"hello, {name}!"` string interpolation; default arguments
> (`fn area(w: i64, h: i64 = 10)`) and named arguments (`area(h = 4, w = 2)`);
> functions may omit a return type when they return nothing. Libraries via
> `use`: `use "helpers.zx";` imports a neighbouring file, and `use math;` /
> `use text;` (or `use std::math;`) import the standard library embedded in
> the compiler — see `internal/driver/std/`. Still compiled to a small,
> fast native binary through C.
>
> Implemented: functions and recursion; `let`/`const`; `type` aliases; integer/
> float/bool/char/`str` values; arithmetic, bitwise, comparison, and logical
> operators; `as` casts; `if`/`elif`/`else`; `while`/`until`/`loop`/`for i in a..b`
> (a `loop` can yield a value via `break <expr>`); `if let`/`while let`;
> `defer` (scope-exit actions, run in LIFO order before destructors);
> `print`/`println` of any number of values; `len`;
> **structs** (named and tuple) with **`impl` methods** (`self`, `&self`,
> `&mut self`) and **associated functions** (`Point::new(1, 2)`); **enums** with
> tuple/struct/unit variants and **`match`** with exhaustiveness checking,
> or-patterns (`A | B`), and range patterns (`1..=9`, `'a'..='z'`);
> **first-class arrays** with bounds-checked indexing;
> a **use-after-move checker** for non-`Copy` values (opt into copying with
> `#[copy]`); the built-in owning heap type **`String`** with **deterministic
> destruction (`Drop`)**, string-parsing methods, and text-building sugar
> (`"n = " + n`, `s += n`); **`extern "C"` FFI** for
> calling libc; and compiling **several source files into one binary** — enough
> to ship an HTTP client/server in [`lib/http.zx`](lib/http.zx) (see
> [`docs/08-networking.md`](docs/08-networking.md)).
>
> Not yet implemented: generics, traits/`impl Trait`, modules/`use`, `Box`/`Vec`,
> user-defined `Drop`, borrow-lifetime analysis (bound `&`/`*` aliases are not yet
> tracked), and `Result`/`?`. Enum variants cannot carry owning payloads yet. See
> `docs/05-roadmap.md`.

## Try it

```sh
go build ./...
go run ./cmd/zexic run tests/tour.zx      # compile + run a real program
go run ./cmd/zexic run tests/structs.zx   # structs, methods, #[copy]
go run ./cmd/zexic run tests/enums.zx     # enums + match
go run ./cmd/zexic run tests/arrays.zx    # arrays + bounds checks
go run ./cmd/zexic run tests/strings.zx   # owning heap String + methods
go run ./cmd/zexic run tests/drop.zx      # deterministic destruction across control flow
go run ./cmd/zexic run tests/syntax.zx    # a runnable tour of the whole syntax
go run ./cmd/zexic run tests/keywords.zx  # until/elif/defer, or+range patterns, if let, type aliases
go run ./cmd/zexic run tests/easy.zx      # the easy way: no semicolons, f-strings, defaults, libraries
go run ./cmd/zexic run examples/easy.zx   # same ideas, as a small standalone demo
go run ./cmd/zexic run tests/sugar.zx     # break-with-value, Type::new(..), print/format sugar
go run ./cmd/zexic run tests/pi.zx        # 30 digits of π: big-int + Machin's formula
go run ./cmd/zexic run examples/http_server.zx lib/http.zx   # an HTTP server, in Zexi
go run ./cmd/zexic run examples/http_client.zx lib/http.zx   # an HTTP client (needs the server)
go run ./cmd/zexic run examples/hello.zx
go run ./cmd/zexic emit --emit=c tests/tour.zx   # inspect the generated C
```

`hello.zx` links to a ~18 KB native ELF. The backend emits C and hands it to the
system C compiler; see [`docs/06-backend-strategy.md`](docs/06-backend-strategy.md)
for why, its real costs, and the plan to replace it with a native backend.

## TL;DR of the design

| Concern | Decision |
| --- | --- |
| Source form | Expression-oriented, C/Rust-flavored, semicolon-terminated statements |
| Memory safety | **Compile-time ownership + borrow checking**, no garbage collector |
| Runtime | Freestanding, opt-in. Hello-world links a runtime of a few KB |
| Compiler host lang | Go |
| Backend (M0) | Emit **C11**, compile with the system `cc` / `clang` (portable, mature optimizer) |
| Backend (M2+) | Native x86-64/ARM64 (or Cranelift) for full control of size & speed |
| Output | A **standalone native binary**, direct syscalls or libc, static by default |
| Generics | Monomorphized (zero-cost), no boxing, no reflection |
| Binary size | Dead-code elimination at MIR, section GC, no std by default, `opt-level=size` |

## Documents

- [`docs/01-overview.md`](docs/01-overview.md) — vision, goals, non-goals, comparisons.
- [`docs/02-language-spec.md`](docs/02-language-spec.md) — lexical & syntactic spec, type system, EBNF.
- [`docs/03-memory-model.md`](docs/03-memory-model.md) — ownership, borrows, lifetimes, `unsafe`.
- [`docs/04-compiler-architecture.md`](docs/04-compiler-architecture.md) — Go pipeline, IRs, passes, backends.
- [`docs/05-roadmap.md`](docs/05-roadmap.md) — M0/M1/M2 milestones and the minimal viable feature set.
- [`docs/06-backend-strategy.md`](docs/06-backend-strategy.md) — the C backend, its costs, and the road to a native backend.
- [`docs/07-cheatsheet.md`](docs/07-cheatsheet.md) — a one-page tour of the language as it exists today.
- [`docs/08-networking.md`](docs/08-networking.md) — FFI via `extern "C"`, and the HTTP library in `lib/http.zx`.
- [`examples/`](examples/) — illustrative Zexi programs (aspirational; only the M0-alpha subset compiles today).
- [`tests/tour.zx`](tests/tour.zx) — the end-to-end program the test suite compiles and runs.
- [`tests/syntax.zx`](tests/syntax.zx) — a runnable syntax tour; [`tests/pi.zx`](tests/pi.zx) — 30 digits of π.

## Repository layout

```
cmd/zexic/            compiler driver (CLI entry point)
internal/
  token/              token kinds + positions
  lexer/              hand-written lexer (working, fuzz-tested)
  ast/                syntax tree
  parser/             recursive-descent + Pratt parser (working, fuzz-tested)
  types/              semantic type representation
  sema/               name resolution, type inference, constant folding,
                      and the flow-sensitive move checker (own.go)
  codegen/c/          M0 backend: typed program -> C11
  driver/             pipeline orchestration + C compiler invocation
  diag/               diagnostics + source spans
docs/                 the specification
examples/             sample Zexi source
tests/                programs exercised by the test suite
```

## Test

```sh
go test ./...                 # unit, fuzz-corpus, and end-to-end run tests
go test -run xxx -fuzz FuzzParse ./internal/parser   # property-test the parser
go test -run xxx -fuzz FuzzLexer ./internal/lexer    # property-test the lexer
```
