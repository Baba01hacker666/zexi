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

## 1. A connection engine that cannot be stalled

Today one slow client holds one worker until its head deadline. That is bounded
but not fair: *n* half-sent heads delay a new request by about *n / workers ×
timeout*.

- Keep a per-worker list of half-read heads (fd, bytes so far, deadline) and
  `poll_read(fd, 0)` over it between accepts, so a connection that is not ready
  costs a poll instead of a thread.
- Add `zxn_recv_available` (MSG_DONTWAIT) to `net` and a `Conn` that owns its
  partial buffer, so a head can be resumed across calls.
- Expire pending entries at their deadline with a `408 Request Timeout`.
- Only then decide between this and a thread-per-connection mode; the numbers
  here should pick the answer, not taste.

## 2. Free what we allocate

LeakSanitizer reports every allocation the language makes: closure
environments, `String`, `Vec`, `HashMap`. Nothing crashes, but a long-running
server grows. The closure-record release path (`zx_clo_release` called from
`Drop` glue for `fn(..)` values) is the prerequisite; once it exists, captured
values free through the drop glue the compiler already emits.

- Give the closure record a real release path, then free captured values
  through the drop glue the compiler already emits.
- Make `Vec`/`HashMap` shrink on clear and free on scope exit.
- Keep the ASan run in the test loop so the count is a number, not a feeling.

## 3. Errors that a request handler can return

**Done:** `Result<Response, String>` returning handlers (`get_result` and
friends, plus `add_result`), with the framework turning `Err` into a logged
500; `?` already propagates inside any handler. Remaining:

- A compile-time complaint when a handler captures something non-`Send`-able
  (see §7).

## 4. Templates without a template language

**Done:** `page_text(path, values)` returns a `String` with `{{name}}`
substitution, and a `_layout.html` include wraps it through the
`{{@@content@@}}` slot. Remaining:

- Convenience helpers for the layout case (`page` returning a `Response` already
  exists; nothing further is planned).

## 5. Reach further out of the language

- `#[link]` names a library; `#[link_dir("path")]` and `#[include("header.h")]`
  are now supported. Still to do: ship `bindgen` output for two or three
  common libraries (SQLite, OpenSSL, zlib) as standard modules, each carrying
  its own C.
- Add `extern "C"` variadics so `printf`-shaped APIs are usable directly.

## 6. Language gaps worth closing

Collected while writing the library; each is small, each has bitten:

- `Vec` closure methods (`each`, `count`, `any`, `all`, `fold`) need `Copy`
  elements.
- `HashMap::get` takes an owned `String`, so every lookup allocates —
  `get_str`/`contains_str` now cover the `HashMap<String, V>` case.
- No variadic strings (`push_str` chains instead of one call).
- `while` conditions cannot allocate.
- Matching an enum through a borrow reports `E0507`.
- `io::read_line` does one `read` syscall per byte. Slices make the buffer easy
  to hold, but a line still needs a buffered reader type (buffer + consumed
  mark) before typing at a terminal costs one syscall per character.
- `str` is still C-ABI-shaped (NUL-terminated, no independent length) at the
  FFI boundary. Slices cover buffers and `str::as_bytes()` covers borrowing;
  length-prefixing `str` itself is still open.

## 7. Multi-threading, next layer

- `spawn` takes a function and integers; closures are now legal, so a
  `spawn(|| ...)` wrapper that packs an environment is a natural fit.
- A `Shared<T>` guard that makes "read-only after start" explicit, and a
  compile-time complaint when a handler captures something non-`Send`-able.
- Cancellation: a worker should be able to abandon a request that has run past
  a deadline.

## 8. Things that stay out of scope for now

- A renderer or template engine (the folder-and-`page` approach covers the
  cases we have).
- TLS. `#[link("ssl")]` plus a small binding is the intended route; doing it in
  the language today would mean implementing a protocol, not using one.
- Windows. The C blocks are POSIX; a Windows port means per-platform blocks,
  which the inline-C mechanism now makes possible without touching the driver.

## How to verify a change here

```sh
go build ./... && go vet ./... && go test ./...
python3 scripts/check_doc_examples.py --run     # every doc block compiles/runs
/tmp/zexic run tests/cinline.zx                 # inline C, #[link], #[no_mangle]
CC=/tmp/san/cc-san /tmp/zexic run tests/web_lib.zx   # sanitized build
```
