# How Zexi Works

This document explains the Zexi compiler and runtime in plain terms: what
happens from a `.zx` source file to a running native binary, how the pieces
fit together, and where to look in the codebase.

---

## 1. Big picture

Zexi is a small systems language that **compiles to C** and then to a native
binary. There is no VM and no garbage collector. The compiler is written in
Go and lives under `internal/` and `cmd/zexic/`.

```
.zx source ──► lexer ──► parser ──► AST ──► sema/type-check ──► codegen (C) ──► cc ──► native binary
```

The only runtime support is a tiny C helper library (`zx_*` functions) that
handles strings, printing, arrays, and the standard library's C shims.

---

## 2. Pipeline stages

### 2.1 Lexer (`internal/lexer`)
- Turns source text into tokens.
- Supports `//` and `#` comments, optional semicolons, f-strings, etc.

### 2.2 Parser (`internal/parser`)
- Builds an AST (`internal/ast`).
- Handles expressions, statements, items (functions, structs, enums, impls,
  use declarations).

### 2.3 Loader / driver (`internal/driver`)
- `FrontEndFiles` (in `driver.go`) loads one or more source files.
- Resolves `use` declarations:
  - `use "helpers.zx";` → relative file
  - `use math;` / `use std::math;` → embedded standard library
    (`internal/driver/std/*.zx`)
- Merges all items into one module.

### 2.4 Semantic analysis (`internal/sema`)
- Type checking, name resolution.
- Move checker (`own.go`): non-`#[copy]` values are move-only; use-after-move
  is an error.
- Produces `sema.Info` with `FnOrder` (reachable functions in order).

### 2.5 Code generation (`internal/codegen/c`)
- Lowers the typed AST to C.
- `Generate` → single translation unit (`CSrc`).
- `GenerateSplit` → shared header + N units (`CHeader` + `CUnits`) for
  parallel compilation of large programs.

### 2.6 Compilation / linking (`driver.CompileC` / `CompileUnits`)
- Writes generated C to temp files.
- Invokes `cc` with `-std=gnu11`, optimization, `-ffunction-sections`,
  `-fdata-sections`, and include dirs.
- Links extra C sources (`--csrc`), embedded runtimes (net/sys), and libs.
- `--gc-sections` / `-dead_strip` prunes unused functions (tree-shaking).

---

## 3. Memory model

- **Value types**: integers, floats, bool, char, arrays, structs/enums are
  stored inline (like C).
- **`String`**: owning heap buffer, reference-counted-ish deterministic
  destruction (`Drop`). Move-only by default.
- **`str`**: borrowed C string (NUL-terminated) for FFI and literals.
- **`#[copy]`**: opts a struct/enum into copy semantics (default is move).
- **`defer`**: runs scope-exit actions in LIFO order before destructors.
- No borrow checker beyond move checking; `&`/`&mut` are tracked loosely.

See `docs/03-memory-model.md`.

---

## 4. Standard library

Embedded in the compiler under `internal/driver/std/`:

| Module | File | Purpose |
|--------|------|---------|
| io     | `io.zx` | stdin/stdout, files, read_all |
| math   | `math.zx` | arithmetic helpers, isqrt, pow_mod |
| text   | `text.zx` | string helpers |
| random | `random.zx` | RNG |
| sys    | `sys.zx` | env, clocks, sleep, pid (C shim `zxsys_c.txt`) |
| net    | `net.zx` | TCP sockets (C shim `zxnet_c.txt`) |
| http   | `http.zx` | HTTP client/server on top of net |

Import with `use io;`, `use std::net;`, `use http;`, etc. The linker prunes
anything not called, so importing costs nothing unless used.

Each module may pull in a C shim (`zxnet_c.txt`, `zxsys_c.txt`) which is
appended to the build only when the module is used.

---

## 5. Networking & HTTP

- `net.zx` wraps POSIX sockets via `extern "C"` functions (`zxn_*`).
- `Conn` and `Server` own file descriptors; `close` invalidates the fd.
- `http.zx` implements:
  - URL parsing, redirects (with header stripping on cross-origin)
  - Request/response parsing, chunked encoding
  - Client with timeouts, headers, cookies
  - Server with routing (`Router`), static files, MIME detection
  - `read_file` (binary-safe, capped at MAX_FILE)
- Connection establishment has a configurable timeout (`connect(..., ms)`).

See `docs/08-networking.md`.

---

## 6. C bridge

- `extern "C" fn ...` declares FFI functions.
- `zexic bindgen` generates declarations from C headers.
- `--csrc` compiles extra C files; `--include`/`--libdir`/`--link` for
  headers/libs.
- String marshalling: `cstr`/`from_cstr`, `str` (borrowed) vs `String`
  (owning).

See `docs/09-c-bridge.md` and `examples/cbridge.zx`.

---

## 7. CLI (`cmd/zexic`)

Common commands:

- `zexic run file.zx` — compile and run.
- `zexic build file.zx -o out` — compile to binary.
- `zexic emit file.zx` — print generated C (single unit).
- `zexic bindgen header.h` — generate extern declarations.

Key flags: `--jobs N` (parallel units), `--opt-level`, `--include`, `--csrc`,
`--link`, `--cc`.

---

## 8. Where to look

| Concern | Location |
|---------|----------|
| Lexer | `internal/lexer/` |
| Parser | `internal/parser/` |
| AST | `internal/ast/` |
| Types | `internal/types/` |
| Sema / move check | `internal/sema/` |
| Codegen | `internal/codegen/c/` |
| Driver / build | `internal/driver/driver.go` |
| Stdlib | `internal/driver/std/` |
| C shims | `internal/driver/zxnet_c.txt`, `zxsys_c.txt` |
| CLI | `cmd/zexic/main.go` |
| Docs | `docs/` |
| Examples | `examples/` |
| Tests | `tests/`, `internal/driver/driver_test.go` |

---

## 9. Quick mental model

1. Source is parsed into an AST.
2. `use` declarations pull in more files / embedded stdlib.
3. Sema type-checks and determines reachable functions.
4. Codegen emits C (one or many units).
5. `cc` compiles + links with runtime shims.
6. `--gc-sections` drops unused code, keeping binaries small.

That's it — Zexi is "Rust-like safety, C-sized output, Go-written compiler."
