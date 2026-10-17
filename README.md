# Zexi

Zexi is an experimental systems programming language: **memory-safe like Rust, as small as C, as fast as Rust, with a minimalist surface like Rust — without the runtime baggage of Go.**

This repository contains the **initial design and specification** of Zexi: the viable feature set (M0–M2) and the compiler architecture. The compiler is written in **Go**.

> Status: **M0-alpha, working end to end.** `zexic` parses, type-checks, lowers
> to C, and links a **native binary** you can run. The supported subset is
> functions, `let`, `const`, arithmetic, comparisons, casts, `if`/`else`,
> `while`, `loop`, `for i in a..b`, and `print`/`println`. Structs, enums,
> `match`, `impl`, generics, and the ownership checker are designed but not yet
> implemented — see `docs/05-roadmap.md`.

## Try it

```sh
go build ./...
go run ./cmd/zexic run tests/tour.zx      # compile + run a real program
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
- [`examples/`](examples/) — illustrative Zexi programs (aspirational; only the M0-alpha subset compiles today).
- [`tests/tour.zx`](tests/tour.zx) — the end-to-end program the test suite compiles and runs.

## Repository layout

```
cmd/zexic/            compiler driver (CLI entry point)
internal/
  token/              token kinds + positions
  lexer/              hand-written lexer (working, fuzz-tested)
  ast/                syntax tree
  parser/             recursive-descent + Pratt parser (working, fuzz-tested)
  types/              semantic type representation
  sema/               name resolution, type inference, constant folding
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
