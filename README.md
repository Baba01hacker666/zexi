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
> `use` or Python-shaped `import`: `use "helpers.zx";` / `import "helpers.zx";`
> import a neighbouring file, while `use math;`, `import http.server;`, and
> `use io::fs;` select embedded standard-library helpers — see
> `internal/driver/std/`. Imports create qualifiers: `use math as m;` gives
> `m::maxi(1, 2)`, `import http.server;` gives `server::Router` (and the full
> `http::server::Router`), and `mod name;` / `mod name { ... }` give
> `name::item`. Qualified lookups are real — they resolve against the module's
> declarations and enforce `pub` (E0603 private, E0412 missing, E0433 module as
> a value) — while the merged program underneath is still flat, so unqualified
> names from a plain import keep working. Everything still
> compiles to a small, fast native binary through C.
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
> **monomorphized generics** for functions, structs, enums, and impls, with
> inference and turbofish (`id::<i64>`, `Pair::<i32, str>`); predeclared
> **`Option<T>`/`Result<T, E>`** and the **`?` operator** (lowered to `match`,
> with bare `Some`/`None`/`Ok`/`Err` from context); **owned enum payloads** with
> match move/drop glue;
> **first-class arrays** with bounds-checked indexing;
> a **use-after-move checker** for non-`Copy` values (opt into copying with
> `#[copy]`); the built-in owning heap type **`String`** with **deterministic
> destruction (`Drop`)**, string-parsing methods, and text-building sugar
> (`"n = " + n`, `s += n`); **`extern "C"` FFI** for
> calling libc; and compiling **several source files into one binary** — enough
> to ship an HTTP client/server in [`lib/http.zx`](lib/http.zx), plus embedded
> `net`/`http` modules (`import http.server;`) with URL parsing, redirects,
> headers, query/form decoding, chunked bodies, timeouts, response builders,
> cookies, static-file serving with MIME detection, downloads, and a
> fixed-capacity router built on function values. Go-style servers are a few
> lines: `handle_get("/users/{id}", user); listen(":8080")` registers on a
> default mux with `{name}` route parameters and `use_before`/`use_after`
> middleware. High-level `serve(&app, 8000)`, `serve_dir(root, 8000)`,
> `new_app()`, and `listen_n(addr, count)` cover explicit and bounded servers;
> `Client`, `get`, `do`, `fetch`, and `request` cover the client side (see
> [`docs/08-networking.md`](docs/08-networking.md)).
>
> **Many users at once:** `listen_workers(":8080", 8)` serves the default mux
> from a pool of OS threads that accept on the same socket, so one slow request
> no longer stalls everyone else — `listen_workers_n`,
> `Router::listen_workers*`, and `serve_workers*` are the bounded and
> explicit-router forms, and `requests_served()`/`stop_serving()` report on and
> wind down the pool. Under it, `net` adds UDP (`udp_bind`, `send_to`,
> `recv_from`), name resolution (`resolve`), readiness and bounded accept
> (`poll_read`, `accept_timeout`), `cpu_count`, and endpoint introspection
> (`Conn::peer`, `Server::port`).
>
> **Websites:** two dependency-free modules make server-rendered pages and JSON
> APIs easy. `json` (`parse`, `stringify`, `pretty`, `object`, `array`, `Value`)
> keeps integers exact, round-trips doubles exactly, and preserves member order;
> `html` (`escape`, `attr`, `tag`, `list`, `table`, `vars`, `template`) escapes
> by default, so a `{{name}}` taken from a query string cannot inject markup,
> and `web` ties the three together in the FastAPI shape: `web::get("/x", h)`,
> `web::page("index", &vars)` reading `templates/index.html`, `web::pages("public")`
> serving a folder of files, `web::params(req)`, `web::json_pair(k, v)`. See
> [`docs/14-json-and-html.md`](docs/14-json-and-html.md),
> [`docs/16-stdlib-reference.md`](docs/16-stdlib-reference.md) and
> [`examples/site/`](examples/site).
>
> **Closures:** `|x| x + 1` captures its surroundings by value, nests, escapes
> from the function that made it, and is what `Vec::each/count/any/all/fold`
> take. See [`docs/15-closures.md`](docs/15-closures.md).
>
> Embedded `math`, `text`,
> `io`, `fs`, `os`, `regex`, `fmt`, `codec`, and `thread` also provide dependency-free
> arithmetic, roots/logs/trigonometry/geometry, number theory, familiar string
> helpers, `input(prompt)`, capped binary-safe file helpers, local process/path
> conveniences, POSIX-ERE matching, number/value formatting (bases, grouping,
> fixed decimals, human units, alignment), hex/base64 encoding and checksums
> (CRC-32, FNV, Adler-32), and real OS **concurrency**: fork–join
> threads, blocking `i64` channels, lock-free `i64` atomics, and a task pool
> that spreads a function over a range across threads (`Thread::spawn`,
> `Chan::new`, `Atomic::new`, `Pool::new().map(f, lo, hi)`; see
> [`docs/11-concurrency.md`](docs/11-concurrency.md)).
>
> Not yet implemented: traits (and trait bounds, so generic parameters are
> unconstrained), `dyn Trait`, associated types, const generics, full module
> **isolation** (qualified access, `as` aliases, and `pub` checks work, but the
> merged program is still one flat namespace, so unqualified cross-module names
> remain visible and ignore `pub`), `Box`,
> `Send`/`Sync` value passing across threads (today only `i64` handles, arguments,
> and return values cross a thread boundary), match ergonomics (`ref` bindings,
> so a payload cannot be read out of a borrowed enum), user-defined `Drop`,
> borrow-lifetime analysis (bound `&`/`*` aliases are not yet tracked), nested
> patterns, and tuple values. See `docs/05-roadmap.md`.
>
> Recursive types work when the recursion goes through indirection (`Vec<T>`,
> `HashMap<K, V>`), so trees, comment threads, and JSON documents are ordinary
> declarations: `enum Tree { Leaf(i64), Node(Vec<Tree>) }`.

## How it works

See [`workings.md`](workings.md) for a walkthrough of the compiler pipeline and
runtime.

Two lowerings consume the checked program. The shipped one emits C. The other is
**MIR**, a control-flow graph of numbered locals with explicit moves, drops, and
bounds-checked indexes ([`docs/13-mir.md`](docs/13-mir.md)): `--emit=mir` prints
it, it covers 93% of this repository's reachable functions today, and its
semantics are diffed against the C backend by a test that compiles each program
natively, runs it, interprets the MIR, and compares the output. It is not yet the
path the compiler takes — that is the next step toward a swappable native
backend.

## Benchmarks

Zexi compiles to C, so the honest expectation is near-C speed — and the
interesting question is what the language's guarantees cost. The
[`bench/`](bench/) suite runs each workload as a Zexi program and as the same
program in plain C (same compiler, same flags) and refuses to rank a pair
whose checksums differ. On the first run, pure computation matches C within
noise, and bounds-checked indexing and owning `String` growth cost about 10%
where they show up at all. Numbers and methodology:
[`bench/results.md`](bench/results.md), [`bench/README.md`](bench/README.md).

## Projects

A program can be one file or a whole folder. Point `zexic` at a project
directory and it finds `main.zx` (or `src/main.zx`), then pulls in everything
the entry links to — no manifest and no file list:

```sh
zexic run .                 # entry discovered, every import compiled in
zexic run src/main.zx       # the root above src/ is searched for modules
zexic build . -o app
```

Named imports are searched next to the importing file, in its `lib/`, at the
project root, and in the root's `lib/`, before the embedded standard library.
`use util;` where `util/` is a directory loads the **whole folder** (every `.zx`
inside it) as one package; `--root=dir` pins the root explicitly.

## Editor support (LSP)

`zexic lsp` is a language server speaking JSON-RPC over stdio. It gives you
diagnostics as you type (unsaved buffers included, multi-file errors landing on
the file that actually has them), hover types and signatures, go-to-definition,
completion — including `.`-triggered field/method/variant completion — and
document symbols. Point your editor's LSP client at `zexic lsp` for the
`zexi` language; an optional argument names the program entry (`zexic lsp
main.zx`) so program-level diagnostics are exact, and without one each open
document is analyzed on its own (libraries included — the "no main" error is
suppressed for files that don't declare one).

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
go run ./cmd/zexic run tests/modules.zx   # scoped imports: aliases, mod::item, pub checks
go run ./cmd/zexic run tests/threads.zx   # threads, channels, and atomics
go run ./cmd/zexic run tests/pool.zx      # task pool: spread a range over threads
go run ./cmd/zexic run tests/json_lib.zx  # parse, build, and print JSON
go run ./cmd/zexic run tests/html_lib.zx  # escaping, widgets, and templates
go run ./cmd/zexic run tests/net_local.zx # loopback TCP/UDP, resolve, poll
go run ./cmd/zexic run tests/recursive.zx # trees through Vec and HashMap
go run ./cmd/zexic run tests/formatting.zx # bases, grouping, units, alignment
go run ./cmd/zexic run tests/encoding.zx   # hex, base64, and checksums
go run ./cmd/zexic run tests/easy.zx      # the easy way: no semicolons, f-strings, defaults, libraries
go run ./cmd/zexic run examples/easy.zx   # same ideas, as a small standalone demo
go run ./cmd/zexic run tests/sugar.zx     # break-with-value, Type::new(..), print/format sugar
go run ./cmd/zexic run tests/pi.zx        # 30 digits of π: big-int + Machin's formula
go run ./cmd/zexic run examples/site                        # a pooled site: templates, folders, JSON API
go run ./cmd/zexic run examples/http_app.zx                  # Go-style default mux: handle_get + listen
go run ./cmd/zexic run examples/static_server.zx -- .         # one-call static server
go run ./cmd/zexic run examples/local_tools.zx -- release-42  # local os + regex helpers
go run ./cmd/zexic run examples/math_tools.zx                 # roots, logs, angles, and geometry
go run ./cmd/zexic run examples/regex.zx                      # POSIX-ERE matching
# The low-level, explicit FFI example remains available too:
go run ./cmd/zexic run examples/http_server.zx lib/http.zx
go run ./cmd/zexic run examples/http_client.zx lib/http.zx   # an HTTP client (needs the server)
go run ./cmd/zexic run examples/hello.zx
go run ./cmd/zexic emit --emit=c tests/tour.zx   # inspect the generated C
go run ./cmd/zexic emit --emit=mir tests/tour.zx # inspect the control-flow IR
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
- [`docs/08-networking.md`](docs/08-networking.md) — the embedded HTTP client/server and networking helpers.
- [`docs/09-c-bridge.md`](docs/09-c-bridge.md) — calling C libraries through `extern "C"` and bindgen.
- [`docs/10-stdlib-recipes.md`](docs/10-stdlib-recipes.md) — local networking, OS, filesystem, and regex recipes.
- [`docs/11-concurrency.md`](docs/11-concurrency.md) — threads, `i64` channels, and atomics.
- [`docs/12-generics.md`](docs/12-generics.md) — monomorphized generics, `Option`/`Result`, and `?`.
- [`docs/13-mir.md`](docs/13-mir.md) — the mid-level IR: shape, instruction set, drops, lowering, and verification.
- [`docs/14-json-and-html.md`](docs/14-json-and-html.md) — building websites: JSON values and safe HTML templating.
- [`examples/`](examples/) — ready-to-run Zexi programs plus focused feature examples.
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
  mir/                mid-level IR: CFG lowering, validator, dump, test oracle
  layout/             ABI table: sizes, alignments, field offsets, enum tags
  driver/             pipeline orchestration, imports/modules + C compiler invocation
  diag/               diagnostics + source spans
docs/                 the specification
examples/             sample Zexi source
tests/                programs exercised by the test suite
  errors/             rejected programs with their expected diagnostics
scripts/              doc-example checker and other repo tooling
bench/                benchmarks against plain C (see bench/README.md)
```

## Test

```sh
go test ./...                 # unit, fuzz-corpus, and end-to-end run tests
go test -run xxx -fuzz FuzzParse ./internal/parser   # property-test the parser
go test -run xxx -fuzz FuzzLexer ./internal/lexer    # property-test the lexer
python3 scripts/check_doc_examples.py --run  # compile and run every doc example
python3 bench/run_bench.py                  # benchmark against plain C
```

Diagnostics are pinned by fixture programs in [`tests/errors/`](tests/errors/):
each file declares the exact errors it must produce (`// error: E0382 ...`),
and a mismatch — a missing, extra, or changed diagnostic — fails the suite.
The documentation's fenced `zx` blocks are checked the same way the code is:
`check_doc_examples.py` compiles each one (wrapping fragments as needed) so
the docs cannot drift away from the compiler.
