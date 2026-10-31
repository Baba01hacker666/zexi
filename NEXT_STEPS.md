# Next steps

Where the language is, what it does well, and the work that would move it
forward most. Ordered by value per unit of risk, not by ambition.

## Where things stand

- **C libraries install with one command.** `zexic add` writes a bound module
  (`sqlite`, `zlib`, `crypto`, `curl`, `microhttpd`) into the project's `lib/`,
  reporting whether the system half is installed by compiling and linking a
  probe. A package is one `.zx` file, so it is readable, editable, and
  committable; there is no manifest and no registry server. See
  `docs/19-modules.md`.
- **`eprint`/`eprintln` are builtins**, and `&[T; N]` / `&mut [T; N]` index like
  the array they point at, so a buffer a C function filled can be read back
  (previously `E0608`).
- **`Option`/`Result` have their everyday methods.** `unwrap`, `expect`,
  `unwrap_or`, and the tag tests `is_ok`/`is_err`/`is_some`/`is_none` are
  compiler builtins defined by the `match` they desugar to, so extractors
  consume their receiver (`E0382` on a second `unwrap`) and the tag tests only
  read it. `panic(msg)` is the `never`-typed counterpart they need. There is no
  `impl` block to write, so nothing in the prelude enums can drift.
- **A borrow can become a pointer.** `&buf as *u8`, `&mut n as *mut i64`,
  `str as *u8`, and `*T` ↔ integer are the one explicit way to make a raw
  pointer, which is what a C function that writes back through a borrow needs.
  The pointee must match (`E0308` otherwise), an array borrow decays to its
  first element, and there is still no pointer dereference in Zexi — the type is
  checked on the C side. This was the last known "cannot express it" gap.
- **A function body can be written in C.** `c """..."""` in body position
  compiles that body inside the program's one translation unit: parameters keep
  their source names and their Zexi C types, the C compiler checks them against
  the generated typedefs, and it inlines across the boundary for free. It is the
  fifth rung of the extension ladder (`docs/17-extending.md` §17.3b) and the one
  that most obviously spends the [single-unit design](docs/18-one-translation-unit.md).
- **A borrow reads like the value it points at.** `&T` resolves method calls on
  the pointee, comparisons read through it (`n > 7` for `n: &i64`, `s == "b"` for
  `s: &String`, including `&String` against `&String`), and `println` prints it.
  That is what makes `v.each(|s: &String| ...)` usable: `Vec`'s closure helpers
  (`each`, `count`, `any`, `all`) now accept a closure that *borrows* the
  element, so a `Vec<String>` can be visited without copying or moving.
- **An f-string hole holds a real expression.** Nested string and char
  literals, nested braces, and nested interpolations (`f"{"inner"}"`) no longer
  end the outer literal early — the scanner tracks hole depth and the quoting
  inside it.

- **Self-extension works.** A library adds native code with `c """..."""`,
  `#[link("name")]`, `#[link_dir("path")]`, `#[include("header.h")]`,
  `extern "C"` and `#[no_mangle]`. The compiler holds no C for the standard
  library: `sys`, `regex`, `json`, `net`, `http`, and `thread` each ship their
  C inside their own `.zx` file, and `TestStandardLibraryNeedsNoCompilerC`
  fails if any of it moves back into the driver. See `docs/17-extending.md`.
- **The server survives hostile input.** Oversized heads get `431`, malformed
  ones `400`, oversized bodies `413`; a connection that sends nothing is dropped
  after 250 ms and a slow head after 1 s; responses are drained out before the
  close so a reset cannot swallow them. See `docs/08-networking.md` §8.7.
- **Closures, worker pools, JSON, HTML, and a folder-based web layer** are in,
  with tests (`go test ./...`, the doc-example gate, and the examples folder).
  Closures may now *call* a captured handler (a `fn(..)` value read from the
  enclosing scope is carried in the environment), which is what makes
  `spawn(|| ...)` wrappers and Result-returning handlers possible.
- **Handlers can fail.** `get_result`/`post_result`/... and the `add_result`
  family accept `fn(&Request) -> Result<Response, String>`; `Err(e)` becomes a
  logged 500 and the message never reaches the client, and `?` propagates a
  lookup failure to that turn without a match at every step.
- **Templates compose.** `web::page_text(name, values)` returns the rendered
  body as a `String`, and a `templates/_layout.html` whose `{{@@content@@}}`
  slot exists wraps the page in it — substitution only, no second language.
  `{{name}}` substitution is unchanged.
- **Buffers are borrowed, not copied.** `[T]`, `&[T]`, and `&mut [T]` are a
  two-word `(ptr, len)` value: `&[T; N]` unsizes, `a[lo..hi]` takes a sub-slice
  without copying, `for x in slice` yields elements by value, and indexing is
  bounds-checked. `String::push_bytes`/`as_bytes` and `str::as_bytes` bridge
  owned text and borrowed bytes.
- **I/O errors are values.** `sys` is the POSIX substrate (`errno`, `IoError`,
  byte I/O, files, environment, clocks); `io` and `fs` build on it and return
  `Result<_, IoError>`, so a missing file is `Err(NotFound)` rather than the
  same empty string as an empty file.
- **The standard library speaks bytes natively.** `text`, `codec`, and `html`
  are written over `&[u8]`: every encoder, checksum, escaper, and search has a
  `_bytes` twin, decoders return `Result` so bad input is not empty output, and
  no scan copies its input first — passing a `String` means lending its bytes.
  The buffer shims in `net`, `http`, `json`, and `sys` push whole slices instead
  of walking byte by byte. The move checker also understands a branch that
  returns: a value returned from an `if` or `match` arm is no longer reported as
  moved on the path that continues past it.
- **Borrowed map lookups.** `HashMap<String, V>` has `get_str`/`contains_str`
  that hash and compare over the borrowed bytes, so a template lookup no
  longer allocates a key for every `{{name}}`.
- **Verification in place:** unit tests, an end-to-end demo site, ASan/UBSan
  builds, a truncation fuzzer over every `.zx` file, and adversarial socket
  probes. The worker pool's `map` no longer loses a batch when the queue
  wraps: it banks its results at the real wrap point, measured against the
  queue's actual occupancy (including jobs an earlier caller left queued).

## 1. A connection engine that cannot be stalled — keep-alive landed, fairness open

Landed (2026-10-31, measured in `bench/http-results.md` on a 2-core box):
HTTP/1.1 keep-alive in every serve loop (≈2–4× close-mode throughput at
concurrency ≥ 8; 33k–54k req/s tiny-body across runs), `TCP_NODELAY` on
all sockets (fixed a 40 ms delayed-ACK stall: 23 req/s → 12k+ req/s at c=1),
prompt `400` + close for a stalled partial head (was: answer, then a 5 s
keep-alive idle hold), request-counted pool budgets with a non-blocking listener
(fixed a poll/accept race that hung budget shutdown with workers > 1), and a
repeatable harness (`bench/http_load.py` + `bench/http_bench.zx`).

Still open — one slow client holds one worker until its head deadline (1 s):
8 stalled heads delayed a normal request by ~1.1 s at 8 workers, ~4.4 s at
2 workers, i.e. about *n / workers × timeout*. Bounded, but not fair.

- Keep a per-worker list of half-read heads (fd, bytes so far, deadline) and
  `poll_read(fd, 0)` over it between accepts, so a connection that is not ready
  costs a poll instead of a thread.
- Add `zxn_recv_available` (MSG_DONTWAIT) to `net` and a `Conn` that owns its
  partial buffer, so a head can be resumed across calls.
- Expire pending entries at their deadline with a `408 Request Timeout`.
- Only then decide between this and a thread-per-connection mode; the numbers
  in `bench/http-results.md` should pick the answer, not taste.

Nothing in the language blocks this; it is `net`-side work.

## 2. Free what we allocate — open, measured

`scripts/leaks.sh <prog>` builds a program with AddressSanitizer and
LeakSanitizer and prints the summary, so the state is a number:

```sh
$ scripts/leaks.sh tests/vec.zx
tests/vec.zx: clean (no leaks, no sanitizer errors)

$ scripts/leaks.sh tests/closures.zx
tests/closures.zx: leaks detected
Direct leak of 184 byte(s) in 7 object(s) allocated from:
    #1 zx_clo_alloc
Direct leak of 6 byte(s) in 1 object(s) allocated from:
    #1 zx_string_reserve
    #2 zx_string_push_str
SUMMARY: AddressSanitizer: 190 byte(s) leaked in 8 allocation(s).
```

So the picture is narrower than "every allocation": `String`, `Vec`, `HashMap`,
struct fields, and the emitted drop glue are clean on the paths the corpus
exercises — `tests/vec.zx`, `tests/tour.zx`, `tests/strings.zx`,
`tests/std_bytes.zx`, `tests/prelude_methods.zx`, `examples/hello.zx`, and
`examples/result.zx` all report `clean`. What leaks is

- **closure environments** — `zx_clo_alloc` has no release path at all, so every
  function value that captures an owning value leaks its environment (184 bytes
  for `tests/closures.zx`);
- **a `String` borrowed out of a temporary.** The second allocation is a real,
  reproducible bug and it is not about closures:

  ```sh
  $ echo 'fn main() { println((String::from("a") + "bc").as_str()); }' > /tmp/l.zx
  $ scripts/leaks.sh /tmp/l.zx
  /tmp/l.zx: leaks detected
  Direct leak of 4 byte(s) in 1 object(s) ...
  ```

  `println(t.as_str())` with `t` a temporary `String` leaks `t`'s buffer:
  `as_str` hands out a borrow of a temporary that the enclosing statement owns,
  and no drop is emitted for it. `println(t)` and `let s = t; println(s.as_str())`
  are both clean, so the owner exists — it is the statement-scoped temporary
  list, not the drop glue, that is missing.

The work, in order:

1. Give the closure record a real release path (`zx_clo_release`, called from the
   `Drop` glue for `fn(..)` values), then let captured values free through the
   drop glue the compiler already emits for their types. This is the big one and
   the prerequisite for the rest.
2. Fix the borrowed-temporary case: a method returning a borrow of a temporary
   receiver (`as_str`, `as_bytes`, and anything like them) needs the receiver's
   drop scheduled at the end of the enclosing statement instead of being lost.
   That is a small, well-scoped change in `stringMethodCall` plus a test that
   `scripts/leaks.sh` runs over both shapes.
3. Only then tune `Vec`/`HashMap` (shrink on `clear`, free the table on scope
   exit) — they are not the current leak.
4. Keep `scripts/leaks.sh` in the loop, and add a corpus sweep so the number
   cannot grow unnoticed.

## 3. Errors that a request handler can return — done

`get_result`/`post_result`/... and the `add_result` family accept
`fn(&Request) -> Result<Response, String>`; `Err(e)` becomes a logged 500 whose
message never reaches the client, and `?` propagates inside any handler. The one
piece that belonged here — a compile-time complaint about a non-`Send`-able
capture — is now tracked in §7, because it needs the spawn machinery there.

## 4. Templates without a template language — done

`page_text(path, values)` renders `{{name}}` substitution into a `String`, and a
`templates/_layout.html` whose `{{@@content@@}}` slot exists wraps the page.
Substitution only, no second language. Layouts that need a value computed per
request go through `page` returning a `Response`; nothing further is planned.

## 5. Reach further out of the language — done, one gap

**Done:** `#[link("name")]`, `#[link_dir("path")]`, and `#[include("header.h")]`
work, and the bindings that used to be "still to do" now ship: `zexic add`
installs `sqlite`, `zlib`, `crypto` (OpenSSL digests/HMAC), `curl`, and
`microhttpd` into the project's `lib/`, each carrying its own C and its own
extern declarations (`docs/19-modules.md`). `sqlite`, `zlib`, `crypto`, and
`curl` are exercised against the real system libraries by
`internal/registry`'s end-to-end tests.

**Done:** `extern "C"` variadics. `extern "C" fn printf(fmt: str, ...) -> i32;`
declares a `printf`-shaped API directly: the parser accepts `...` as the last
parameter of an `extern` declaration (E0202 elsewhere), sema marks the `Fn`
variadic and requires only the fixed parameters (E0061 when missing) while
checking each extra argument against its own type, and codegen emits `...` in
the C prototype so the C compiler applies the default promotions (`f32` →
`double`, narrow ints → `int`). `zexic bindgen` emits the `...` tail for
variadic header functions too. Covered by `tests/c_variadic.zx` and the error
fixtures in `tests/errors/`.

## 6. Language gaps worth closing

Collected while writing the library, each small, each has bitten. Each row was
re-checked against the current compiler rather than trusted: one was fixed this
round, two turned out to be stale claims (they already work), and the rest are
still open.

| Gap | State |
| --- | --- |
| `Vec` closure helpers (`each`, `count`, `any`, `all`) needed `Copy` elements | **fixed** — a closure may take `&T` and borrow each element in place |
| `while` conditions could not allocate | **stale claim** — a temporary owner in the condition works (`while (String::from("ab") + "c").len() > 0 && n < 2 {}`); the generated loop drops it per iteration |
| Matching an enum through a borrow reported `E0507` | **stale claim** for `Copy` payloads — `match s { Shape::Circle(r) => … }` on `s: &Shape` binds by value and works. The real restriction is binding an *owning* payload, listed below |
| `HashMap::get` allocated a key per lookup | **fixed earlier** — `get_str`/`contains_str` hash the borrowed bytes |
| `Vec::fold` over owning elements | **open** — its accumulator is an element value and the generated loop cannot drop the values it replaces, so it stays `Copy`-only; fold into an owned accumulator with an index instead |
| `for x in v` copies each element | **open** — iterating a `Vec<String>` is `E0507`; `v.each(|s: &String| …)` is the way today. A `for s in &v` form is the fix, and it needs the borrow-checked iterator idea below |
| No `ref`-style pattern | **open** — an owning payload cannot be bound by reference out of a borrowed scrutinee (`Msg::Text(s) => …` on `&Msg` is `E0507`). A `ref s` pattern is the smallest fix; `_` is the workaround |
| Variadic string building | **fixed** — `push_str` takes one or more arguments (`s.push_str("a", "b", "c")`); each is appended in order and a `String` argument is borrowed, not moved |
| `io::read_line` does one `read` syscall per byte | **open** — a line needs a buffered-reader type (buffer + consumed mark); the type is the work, not the syscalls |
| `str` is C-ABI-shaped at the boundary | **open, deliberate** — NUL-terminated with no independent length. Slices cover buffers and `str::as_bytes()` covers borrowing; length-prefixing `str` is an ABI change with no forcing need yet |
| Pointer identity comparison | **not a gap by choice** — a comparison through a borrow compares values, never addresses; two `*T` cannot be compared at all. Use integer addresses if that is what you mean |

## 7. Multi-threading, next layer — open

- `spawn` takes a function and integers; closures are legal, so a `spawn(|| ...)`
  wrapper that packs an environment is a natural fit.
- A `Shared<T>` guard that makes "read-only after start" explicit, plus the
  compile-time complaint about a non-`Send`-able capture that §3 used to track.
  That check needs the type to be visible across the spawn boundary, which is
  the same missing piece as the `Shared<T>` guard.
- Cancellation: a worker should be able to abandon a request that has run past a
  deadline.

## 8. Things that stay out of scope for now

- A renderer or template engine (the folder-and-`page` approach covers the
  cases we have).
- TLS. `#[link("ssl")]` plus a small binding is the intended route; doing it in
  the language today would mean implementing a protocol, not using one.
- Windows. The C blocks are POSIX; a Windows port means per-platform blocks,
  which the inline-C mechanism now makes possible without touching the driver.
- A borrow-checked iterator protocol (`for s in &v`). Every other item in §6 is
  a widening of an existing builtin; this one needs a trait, or an equivalent
  hand-rolled protocol the compiler knows about, and it is the only item here
  that would grow the type system.

## How to verify a change here

```sh
go build ./... && go vet ./... && go test ./...
python3 scripts/check_doc_examples.py --run     # every doc block compiles/runs

zexic run tests/cinline.zx        # inline C, #[link], #[no_mangle]
zexic run tests/c_body.zx         # a function body written in C
zexic run tests/pointer_cast.zx   # &t as *T
zexic run tests/vec_visit.zx      # visiting a Vec<String> by borrow
zexic run tests/prelude_methods.zx  # unwrap / is_ok / panic

scripts/leaks.sh tests/closures.zx   # ASan + LeakSanitizer summary
scripts/leaks.sh tests/vec.zx        # ... and a clean control
```

`scripts/leaks.sh` builds the program as one translation unit with
`-fsanitize=address` and prints either `clean` or the leak summary, so §2 has a
number to move rather than a feeling to argue about.
