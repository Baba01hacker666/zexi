# 8. Networking & HTTP

Zexi has a compact, opt-in standard library as well as a real **C ABI**. That
combination is enough to write a real HTTP client and server in ordinary Zexi
code. [`lib/http.zx`](../lib/http.zx) remains the explicit FFI walkthrough;
new applications should normally use the embedded `http` module described in
§8.6.

## 8.1 How it works: FFI via `extern "C"`

A body-less function declared `extern "C"` is emitted with its C name and links
against libc:

```zx
extern "C" fn getpid() -> i32;
extern "C" fn send(fd: i32, buf: str, n: usize, flags: i32) -> i64;

fn main() -> i32 {
    println(getpid());
    0
}
```

The type mapping is direct: integers map to their C fixed-width types, `bool`
to `bool`, `char` to `uint32_t`, `str` to `const char *`, `&T`/`*T` to `T *`,
and a fixed array `[u8; N]` is passed as a pointer to its backing storage. So
`str` arguments can go straight to `puts`, `write`, `send`, `strlen`, and so on.

## 8.2 Modelling `struct sockaddr_in`

Sockets need an address struct. Zexi has no `#include` and no way to cast
pointers, but a Zexi struct **is** a C struct with the same field order, so we
can model `sockaddr_in` with the exact same 16-byte layout:

```zx
struct SockAddrIn { family: u16, port: u16, addr: u32, pad: i64 }

extern "C" fn socket(domain: i32, typ: i32, protocol: i32) -> i32;
extern "C" fn connect(fd: i32, addr: &SockAddrIn, len: u32) -> i32;
```

Because the header is never included, our `connect` prototype is the only one
the C compiler sees, and at link time it resolves to libc's — the pointer is
just a pointer. `AF_INET` (2), `SOCK_STREAM` (1) and friends are ordinary
constants. The port and address are stored in network byte order with a tiny
`htons` byte-swap and `inet_addr`.

This is the same trick — but the honest version — that any FFI needs: describe
the ABI, then write the logic.

## 8.3 The library

```zx
// TCP
fn tcp_connect(host: str, port: i32) -> Conn     // Conn.fd < 0 on failure
fn http_listen(host: str, port: i32) -> Server    // Server.fd < 0 on failure

struct Conn  { fd: i32 }   // #[copy]
impl Conn {
    fn write(&self, data: str) -> i64
    fn read_all(&self) -> String     // until the peer closes (client)
    fn read_head(&self) -> String    // until "\r\n\r\n" (server)
    fn close(&self)
}

struct Server { fd: i32 }  // #[copy]
impl Server {
    fn accept(&self) -> Conn
    fn close(&self)
}

// HTTP
fn get_request(host: str, path: str) -> String
fn http_get(host: str, port: i32, path: str) -> String   // raw response
fn parse_response(raw: &String) -> Response              // { status, reason, body }
fn parse_request(raw: &String) -> Request                // { method, path, version, body }
fn response(status: i32, reason: str, body: str) -> String
fn reason_phrase(status: i32) -> str
fn push_int(s: &mut String, n: i64)                      // decimal formatting
```

A server:

<!-- fragment -->
```zx
fn route(req: &Request) -> Reply {
    if req.path.as_str() == "/" {
        return Reply { status: 200, body: String::from("hello\n") };
    }
    return Reply { status: 404, body: String::from("not found\n") };
}

fn main() -> i32 {
    let srv = http_listen("127.0.0.1", 9090);
    loop {
        let c = srv.accept();
        let req = parse_request(&c.read_head());
        let reply = route(&req);
        c.write(response(reply.status, reason_phrase(reply.status), reply.body.as_str()).as_str());
        c.close();
    }
    0
}
```

A client:

<!-- fragment -->
```zx
fn main() -> i32 {
    let raw = http_get("127.0.0.1", 9090, "/");
    let resp = parse_response(&raw);
    println(resp.status);   // 200
    println(resp.body);     // hello
    0
}
```

## 8.4 Compiling against the low-level walkthrough library

The explicit FFI walkthrough can still be compiled **together with** the files
it needs; the first file owns `main`, and the rest are merged into it:

```sh
zexic run examples/http_server.zx lib/http.zx
zexic run examples/http_client.zx lib/http.zx
```

See [`examples/http_server.zx`](../examples/http_server.zx) and
[`examples/http_client.zx`](../examples/http_client.zx). The test suite builds
both and exercises the round trip over loopback
(`TestHTTPServerAndClient` in `internal/driver/driver_test.go`). The newer
loader also supports `use`/`import` and `mod`; see §8.6 for the zero-extra-file
high-level API.

## 8.5 Honest limitations (M0)

- **IPv4 only** (`AF_INET`), no IPv6.
- **No TLS** — plaintext HTTP; HTTPS needs a TLS stack or a syscall-level
  binding we don't have yet.
- **No `Content-Length` handling / chunked decoding** — the client reads until
  the server closes (`Connection: close`), which is why `http_get` requests it.
- **Single `send`** per write, and request heads are parsed with a budget
  rather than a full state machine.
- **No pipelining, no redirects** — but HTTP/1.1 keep-alive is served (see §8.7);
  the *client* still reads until close.
- Server sockets are written as head + body in two small sends; `TCP_NODELAY` is
  set on every accepted and connected socket so the second send is not held
  for a delayed ACK (without it a tiny response costs ~40 ms — see §8.7).
- **`Conn`/`Server` are not automatically closed** — there is no user-defined
  `Drop` yet, so `close()` is explicit.
- The status line parser assumes the standard `HTTP/1.x <code> <reason>` shape.

These are deliberate cut lines for M0; none of them is blocked by the language,
only deferred.

## 8.6 High-level stdlib networking (`import http.server;`)

The `net` and `http` modules embedded in the compiler are the higher-level
surface: URLs, redirects, headers, query/form decoding, response builders and
body framing are handled for you. `lib/http.zx` remains the worked example of
building the same thing from raw FFI declarations; new code should prefer
`use http;`, `import http.client;`, or `import http.server;`. These package-like
facades load the same module and make its declarations available directly while
full scoped module lookup is completed in M1.

A client in the urllib/requests style:

<!-- check-only -->
```zx
import http.client;

fn main() {
    let mut c = Client::new();
    c.set_header("Accept-Language", "en");

    let r = c.get("http://example.com:8080/search?q=zexi%20lang");
    if r.ok() {
        println(r.status);              // 200
        println(r.header("Content-Type"));
        println(r.body);
    } else {
        println(r.error);               // connection/parse/https failure
    }
}
```

| API | Purpose |
| --- | --- |
| `fetch(url)` / `get(url)` / `get_url(url)` / `get_status(url)` | one-call GET or status-only request |
| `request(method, url, body = "", content_type = "")` / `do(...)` | one-call configurable request |
| `post(url, body)`, `post_url`, `post_json`, `post_form_url`, `put_url`, `delete_url`, `download` | high-level URL helpers |
| `get_raw(host, port, path)`, `post_raw(host, port, path, body)` | raw-text request builders kept for hand-rolled calls |
| `Client::new()` / `Client.get/post/put/delete/send` | configurable reusable client |
| `Client.post_form`, `Client.download` | form posts, and fetch-to-file |
| `Client.timeout_ms`, `Client.follow_redirects`, `Client.max_redirects`, `Client.user_agent`, `Client.set_header` | client settings |
| `parse_url(str) -> Url` | scheme/host/port/path/query, with `Url.error` |
| `url_encode`, `url_decode`, `query_value` | escaping and parameter decoding |
| `Response.status/reason/body/error`, `Response.ok()`, `header()`, `content_length()` | response access |

Redirects are followed for 301/302/303 (switching to GET) and 307/308
(preserving the method and body), up to `max_redirects`. `https://` URLs
officially fail with `error` set, because the stack is plaintext HTTP only.

A server can be as small as a few top-level registrations plus `listen` — the
same shape as Go's `http.HandleFunc` + `http.ListenAndServe`:

<!-- check-only -->
```zx
import http.server;

fn hello(req: &Request) -> Response {
    return Response::json(200, "{\"hello\":true}");
}

fn main() -> i32 {
    handle_get("/", hello);
    listen(":8080")            // runs until stopped
}
```

`handle_get/handle_post/handle_put/handle_delete` and `handle(method, path, fn)`
register on a process-wide default mux; `listen(addr)`/`listen_n(addr, max)`
take an address string (`":8080"`, `"8080"`, `"127.0.0.1:8080"`, or
`"http://host:8080"`). `use_before(fn)`/`use_after(fn)` attach middleware to the
default mux: a before hook may return a valid `Response` to short-circuit
(auth, redirects), an after hook can add headers. Prefer an explicit
`Router::new()`/`new_app()` when a program holds several independent servers.

If you want the classic `Router` value and accept loop, everything is still
there:

<!-- fragment -->
```zx
fn main() -> i32 {
    let mut app = new_app();
    app.get("/hello", hello);
    app.set_fallback(my_404);
    // ... per-connection: if app.serve(&c) { ... }
    0
}
```

Use `serve(&app, port)` when intentionally exposing a service on all IPv4
interfaces, or pass an explicit host as its third argument. For a controlled
service or a test, `serve_n(&app, 10, 8080)` exits after ten valid requests;
`serve_local_n` is the loopback-only counterpart. If you want the explicit
accept loop, the `net` helpers `listen_tcp`, `listen_local`, `read_request`,
and `send_response` remain public.

| API | Purpose |
| --- | --- |
| `read_request(&Conn) -> Request` | head plus Content-Length body |
| `Request.query/param/route_param/cookie/path_only/raw_query/wants_keep_alive` | request access (`param` reads query, route, then form) |
| `Response::text/html/json/empty/redirect`, `Response.set_header/set_cookie` | response builders |
| `send_response(&Conn, &Response) -> i64` | serialize and write a response |
| `Response.keep_alive` | advertise a persistent connection — every serve loop honours it (`Connection: keep-alive`, socket stays open) |
| `handle*` / `use_before` / `use_after` / `listen` / `listen_n` | top-level default-mux registration and serving |
| `new_app()`, `Router.get/.post/.put/.delete/.add`, `.use_before/.use_after`, `.set_fallback`, `.listen/.listen_n`, `.dispatch/.serve` | explicit router with the same features |
| `serve(&router, port)`, `serve_n(...)`, `serve_local(...)`, `serve_local_n(...)` | low-level accept loops (forever or request-bounded) |
| `parse_addr(str) -> Addr` | parse a listen address (`Addr.ok/host/port`) |
| `serve_static(root, path)`, `serve_dir(root, port)`, `serve_file(path)` | static files with MIME detection and traversal checks |
| `serve_dir_local(root, port)`, `serve_dir_local_n(...)` | loopback-only static-site helpers |
| `read_file`, `write_file_bytes`, `file_size` | binary-safe file helpers (also used by downloads) |

Handlers are ordinary functions (`fn(&Request) -> Response`) stored and invoked
through function values.

Routers hold up to 32 (method, path) pairs and 8 before/after middleware hooks.
A route segment written `{name}` matches exactly one path segment and its value
is available as `req.param("name")` (which checks the query string first, then
route parameters) or `req.route_param("name")`:

<!-- check-only -->
```zx
import http;

fn user(req: &Request) -> Response {
    return Response::text(200, req.param("id").as_str());
}

fn main() -> i32 {
    handle_get("/users/{id}", user);
    listen(":8080")
}
```

`/users/42` then yields `req.param("id") == "42"`. A trailing `/*` still makes a
prefix route (`/api/*` matches `/api/echo`), `*` matches everything, and a `*`
segment after named segments matches the remaining path. `Router.serve` answers
oversized bodies with `413` before calling a handler.

The default mux is one per process (it is not a user-visible global; the storage
lives behind the `http` shim). Programs that need isolated registries should use
explicit routers.

A directory can be served in one call, much like Python's built-in file server:

<!-- check-only -->
```zx
import http.server;

fn main() -> i32 {
    let root = arg_at(1);
    serve_dir_local(root.as_str(), 8000)
}
```

Use `serve_dir` when LAN exposure is intentional, or `serve_dir_local_n` for a
request-bounded local tool. Use `serve_static(root, req.path_only().as_str())`
when it is one route in a larger app. `serve_static` maps `/` to `index.html`, rejects decoded `..`, backslashes and
NUL with `403`, resolves symlinks and refuses any target that escapes the
served root with `403`, returns `404` for missing files and `413` above 32 MiB. MIME
types follow the extension. Bodies are copied byte-for-byte: `send_response`
writes the head and body separately, so images and other binary files survive
(plain `render_response` is for text only). `Client.download(url, path)` is the
client-side counterpart; `read_file`/`write_file_bytes` are binary-safe.

Client connections get a 30 s default timeout and every body read is capped
(`MAX_BODY`, 16 MiB). Requests are framed by Content-Length; responses support
Content-Length, chunked encoding, and read-until-close.

Still deliberately out of scope: TLS/HTTPS, HTTP/2, a client cookie jar
(servers can read/set cookies, and clients can send them with `set_header`),
multipart uploads, range requests, and a client connection pool. Server-side
routing does support `{name}` path patterns, `*` prefixes, and
`use_before`/`use_after` middleware; the route table and middleware lists are
fixed-size arrays (32 routes, 8 hooks each), so a router that needs more must be
split across routers (up to four can be served at once, counting the default
mux).

## 8.7 Serving many users at once

Everything above serves one connection at a time: `listen` accepts, reads,
dispatches, writes, closes, and only then looks at the next connection. That is
fine for a laptop demo and wrong for anything with two visitors — a single slow
request, or a client that connects and sends nothing, stalls everyone behind it.

A **worker pool** fixes that by letting several OS threads accept on the *same*
listening socket. The kernel hands each new connection to whichever thread is
idle, so a slow request only occupies its own worker:

<!-- check-only -->
```zx
import http.server;

fn home(req: &Request) -> Response {
    Response::html(200, "<h1>home</h1>")
}

fn main() -> i32 {
    handle_get("/", home);
    listen_workers(":8080", 8)      // eight threads accepting together
}
```

| Call | What it does |
| --- | --- |
| `listen_workers(addr = "0.0.0.0:8000", workers = 0)` | serve the default mux with a pool (`workers` 0 = two per core) |
| `listen_workers_n(addr, workers, max_requests)` | the same, stopping after a pool-wide request budget |
| `Router::listen_workers(addr, workers)`, `Router::listen_workers_n(addr, workers, max_requests)` | the pool serving an explicit router |
| `serve_workers(router, workers, port, host)` / `serve_workers_n(...)` | function forms of the `Router` methods |
| `serve_local_workers(router, ...)` / `serve_local_workers_n(...)` | the same bound to loopback |
| `requests_served()`, `connections_served()` | pool-wide counters (C11 atomics, no lock) |
| `stop_serving()`, `serving_stopped()`, `reset_stats()` | cooperative shutdown and counter reset |

How a `Router` reaches a thread: the pool's C worker loop is handed a small
integer, not a Zexi value, so the pool **pins** the router — it copies
the route table into a fixed, over-aligned slot in the C runtime and passes the
slot number. The default mux already lives in slot 0, so it needs no copy. Three
slots are available for explicit routers; if all are busy, `serve_workers*`
falls back to serving on the calling thread instead of failing.

Shutdown is cooperative. Workers accept with `accept_timeout` (see §8.8) so each
one notices the stop flag within `WORKER_POLL_MS` (100 ms) instead of blocking
forever. Every pool entry point puts the listener in non-blocking mode first: two
workers can wake for the same connection, and without `O_NONBLOCK` the loser
blocks in `accept()` forever, which used to hang budget shutdown entirely. A
request budget (`*_n` calls) is how tests and one-shot tools exit
deterministically: the budget counts **requests**, not connections, so a
keep-alive connection that carries the whole budget still triggers it — the
worker that reaches the budget answers that request, sets the stop flag
immediately (no keep-alive idle wait), and the rest wind down within one poll
interval.

Handlers stay ordinary functions and need no locking: a request and its
response are per-thread values, and the router is read-only once the pool
starts. Anything shared between requests must be shared explicitly (an
`Atomic` from `thread`, or a channel).

### What a hostile client costs

A worker pool bounds, but does not remove, the cost of a client that misbehaves,
so every accepted connection carries deadlines:

| Deadline | Default | What it stops |
| --- | --- | --- |
| first byte | 250 ms (`FIRST_BYTE_MS`) | a socket that connects and sends nothing |
| request head | 1 s (`HEAD_TIMEOUT_MS`), `app.header_timeout(ms)` | a head sent a byte at a time |
| body and I/O | 30 s (`DEFAULT_TIMEOUT_MS`) | a client that stalls mid-body or stops reading |

| Bad input | Answer |
| --- | --- |
| head larger than 8 KiB without its blank line | `431 Request Header Fields Too Large` |
| bytes but no parseable request line | `400 Bad Request` |
| a complete request line but no terminating blank line | `400 Bad Request` after the 1 s head deadline, then close (no keep-alive idle) |
| `Content-Length` above `MAX_BODY` | `413 Payload Too Large` |
| chunked body that does not parse | the normal `404`/`400`, drained first so the reset cannot eat it |

The last column matters for the same reason the timeouts do: closing a socket
that still has unread bytes makes the kernel send RST, which can destroy the
response the client has not read yet. The server drains before it closes.

One limit remains honest: a client that sends *part* of a head occupies a worker
until the head deadline expires, then gets a prompt `400` and a close. Measured
(`bench/http-results.md`): 8 stalled heads against 8 workers delayed one normal
request by ~1.1 s; against 2 workers, ~4.4 s — roughly `stalled / workers × 1 s`.
Raise `workers`, lower `header_timeout`, or put a real edge in front when that
matters. A per-worker list of pending half-read heads (poll instead of park) is
the next step, and the pieces it needs (`poll_read`, `wait_readable`,
per-connection buffers) are already in `net`.

### Measured scale (2-core x86_64, 2026-10-31)

The stdlib server is dependency-free (own HTTP/1.1 implementation over BSD
sockets — no libmicrohttpd), so `bench/http_bench.zx` + `bench/http_load.py`
measure exactly what ships. Latest committed run — every cell server-count
verified, 8000 requests per throughput cell, raw-socket client — is
[`bench/http-results.md`](../bench/http-results.md):

| load | workers | concurrency | req/s | p50 ms | p99 ms |
| --- | ---: | ---: | ---: | ---: | ---: |
| GET / keep-alive (tiny body) | 1 | 1 / 8 / 32 / 128 | 33,233 / 31,323 / 24,663 / 16,633 | 0.02 | 0.06–0.15 |
| GET / keep-alive | 4 | 1 / 8 / 32 / 128 | 25,995 / 28,312 / 41,790 / 27,652 | 0.02–0.06 | 0.10–1.43 |
| GET / close-mode (one conn per request) | 1 / 4 | 32 | 12,597 / 12,690 | 2.01 / 1.64 | 4.64 / 7.72 |
| GET /big (64 KiB body) keep-alive | 4 | 16 | 12,078 (≈0.8 GB/s) | 0.05 | 0.60 |
| POST /echo (1 KiB body) keep-alive | 4 | 16 | 13,258 | 0.02 | 0.13 |

Across repeated runs the pool peaks at roughly 38k–54k tiny requests/s; the
p99 outliers at high concurrency are accept-queue waits, not handler time.
Takeaways: keep-alive is ≈2–4× close-mode at concurrency ≥ 8 (a pooled
accept+close per request costs ~2 ms p50 on this box); past a handful of
connections the two cores — not the pool — are the limit. Two fixes came out
of these numbers: `TCP_NODELAY` on all sockets (head+body are two small
writes; without it the second waits for a delayed ACK — 23 req/s, p50 ≈ 44 ms
at c=1 became 11k+), and a non-blocking listener in every pool entry point (a
poll/accept race let one worker block in `accept()` forever and hang budget
shutdown).

Reproduce: `python3 bench/http_load.py --total 8000 --concurrency 1,8,32,128 --workers 1,4 --out bench/http-results.md`
(see `bench/README.md`).

## 8.8 UDP, name resolution, and readiness

The same module covers the rest of a small server's needs.

<!-- check-only -->
```zx
use net;

fn main() -> i32 {
    // Never leave the machine: port 0 asks the kernel for a free port.
    let mut server = listen_local(0);
    println("listening on", server.port());

    // A bounded accept lets a loop check for shutdown instead of hanging.
    let conn = server.accept_timeout(100);
    if !conn.is_open() {
        println("no connection within 100 ms");
    }

    // UDP: connectionless datagrams, each received with its sender.
    let mut udp = udp_bind_local(0);
    udp.send_to("127.0.0.1", udp.port(), "ping");
    let packet = udp.recv_from();
    println(packet.data, packet.host, packet.port);
    udp.close();
    server.close();
    0
}
```

| API | Purpose |
| --- | --- |
| `resolve(host) -> String` | first IPv4 address for a name ("" when it does not resolve) |
| `cpu_count() -> i32` | processors available, for sizing pools and splitting work |
| `poll_read(fd, ms) -> i32` | wait for readability: 1 ready, 0 timeout, -1 error |
| `Conn::wait_readable(ms)`, `Server::wait_readable(ms)` | the same over a handle |
| `Server::accept_timeout(ms)` | bounded accept, returning a closed `Conn` on timeout |
| `Server::port()`, `Server::set_nonblocking(on)`, `Server::from_fd(fd)` | bound port, `O_NONBLOCK`, adopt a descriptor |
| `Conn::from_fd(fd)`, `Conn::fd()`, `Conn::is_open()`, `Conn::peer()` | adopt/introspect a connection |
| `Conn::local_port()`, `Conn::read_some(max)`, `Conn::shutdown_write()` | own endpoint, one bounded read, half-close |
| `udp_bind(host, port)`, `udp_bind_local(port)` | open a datagram endpoint |
| `Udp::send_to(host, port, data)`, `Udp::recv_from() -> Datagram`, `Udp::recv_text()`, `Udp::set_timeout(ms)` | datagram I/O |
| `last_error()`, `error_string(code)` | the last `errno`, as text |

`Datagram` carries `data`, `host`, and `port`, so a datagram server answers with
`udp.send_to(packet.host.as_str(), packet.port, reply)`.

Limits worth knowing: everything is IPv4, plaintext, and single-address-per-name
(`resolve` returns the first answer). `MAX_READ` (64 MiB), `MAX_LINE` (64 KiB),
and the HTTP layer's `MAX_BODY` (16 MiB) bound what a peer can make you buffer.
A pool is capped at `MAX_WORKERS` (64) threads, and the router slot table holds
one default mux plus three explicitly served routers at a time.

## 8.9 What this demonstrates

- `extern "C"` FFI is real enough to bind the POSIX socket API with no compiler
  changes to the networking path.
- Struct layout, arrays-as-pointers, references, and the owning `String` (with
  `push_byte` for raw bytes and `find`/`substr`/`trim` for parsing) compose into
  a usable library.
- Multiple source files compile into one standalone binary.
