# Next steps

Where the language is, what it does well, and the work that would move it
forward most. Ordered by value per unit of risk, not by ambition.

## Where things stand

- **Self-extension works.** A library adds native code with `c """..."""`,
  `#[link("name")]`, `extern "C"` and `#[no_mangle]`. The compiler holds no C for
  the standard library: `sys`, `regex`, `json`, `net`, `http`, and `thread` each
  ship their C inside their own `.zx` file, and
  `TestStandardLibraryNeedsNoCompilerC` fails if any of it moves back into the
  driver. See `docs/17-extending.md`.
- **The server survives hostile input.** Oversized heads get `431`, malformed
  ones `400`, oversized bodies `413`; a connection that sends nothing is dropped
  after 250 ms and a slow head after 1 s; responses are drained out before the
  close so a reset cannot swallow them. See `docs/08-networking.md` §8.7.
- **Closures, worker pools, JSON, HTML, and a folder-based web layer** are in,
  with tests (`go test ./...`, the doc-example gate, and the examples folder).
- Verification in place: unit tests, an end-to-end demo site, ASan/UBSan builds,
  a truncation fuzzer over every `.zx` file, and adversarial socket probes.

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
server grows.

- Give the closure record a real release path (`zx_clo_release` called from
  `Drop` glue for `fn(..)` values), then free captured values through the drop
  glue the compiler already emits.
- Make `Vec`/`HashMap` shrink on clear and free on scope exit.
- Keep the ASan run in the test loop so the count is a number, not a feeling.

## 3. Errors that a request handler can return

Handlers return `Response`; a failure inside one has nowhere to go.

- Add `Result<Response, String>` returning handlers, with the framework turning
  `Err` into a 500 and logging the message.
- Add `?` propagation for handlers, so a database or file error does not need a
  match at every step.

## 4. Templates without a template language

The current answer is deliberately small: serve a folder, read a file, fill
placeholders. The next step is composition rather than a syntax:

- `page(path, values)` returning a `String`, with `{{name}}` substitution and a
  layout include.
- Nothing to parse, nothing to compile, no second language in the project.

## 5. Reach further out of the language

- `#[link]` today names a library; add `#[link_dir("path")]` and
  `#[include("header.h")]` so a binding can be written against a real header.
- Ship `bindgen` output for two or three common libraries (SQLite, OpenSSL,
  zlib) as standard modules, each carrying its own C.
- Add `extern "C"` variadics so `printf`-shaped APIs are usable directly.

## 6. Language gaps worth closing

Collected while writing the library; each is small, each has bitten:

- `Vec` closure methods (`each`, `count`, `any`, `all`, `fold`) need `Copy`
  elements.
- `HashMap::get` takes an owned `String`, so every lookup allocates.
- No variadic strings (`push_str` chains instead of one call).
- `while` conditions cannot allocate.
- Matching an enum through a borrow reports `E0507`.

## 7. Multi-threading, next layer

- `spawn` takes a function and integers; closures are now legal, so a
  `spawn(|| ...)` wrapper that packs an environment is a natural fit.
- A `Shared<T>` guard that makes "read-only after start" explicit, and a
  compile-time complaint when a handler captures something non-`Send`-able.
- Cancellation: a worker should be able to abandon a request that has run past a
  deadline.

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
