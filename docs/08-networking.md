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
- **No keep-alive**, no pipelining, no redirects.
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
| `fetch(url)` / `get_url(url)` / `get_status(url)` | one-call GET or status-only request |
| `request(method, url, body = "", content_type = "")` | one-call configurable request |
| `post_url`, `post_json`, `post_form_url`, `put_url`, `delete_url`, `download` | high-level URL helpers |
| `Client::new()` / `Client.get/post/put/delete/send` | configurable reusable client |
| `Client.post_form`, `Client.download` | form posts, and fetch-to-file |
| `Client.timeout_ms`, `Client.follow_redirects`, `Client.max_redirects`, `Client.user_agent`, `Client.set_header` | client settings |
| `parse_url(str) -> Url` | scheme/host/port/path/query, with `Url.error` |
| `url_encode`, `url_decode`, `query_value` | escaping and parameter decoding |
| `Response.status/reason/body/error`, `Response.ok()`, `header()`, `content_length()` | response access |

Redirects are followed for 301/302/303 (switching to GET) and 307/308
(preserving the method and body), up to `max_redirects`. `https://` URLs
officially fail with `error` set, because the stack is plaintext HTTP only.

A server can now be as small as a router plus `serve`:

```zx
import http.server;

fn hello(req: &Request) -> Response {
    return Response::json(200, "{\"hello\":true}");
}

fn main() -> i32 {
    let mut app = Router::new();
    app.get("/", hello);
    serve_local(&app, 8080)  // loopback only; runs until stopped
}
```

Use `serve(&app, port)` when intentionally exposing a service on all IPv4
interfaces, or pass an explicit host as its third argument. For a controlled
service or a test, `serve_n(&app, 10, 8080)` exits after ten valid requests;
`serve_local_n` is the loopback-only counterpart. [`examples/http_app.zx`](../examples/http_app.zx)
has a larger ready-to-run app. If you want the explicit accept loop, `listen`,
`listen_local`, `read_request`, and `send_response` remain public too.

| API | Purpose |
| --- | --- |
| `read_request(&Conn) -> Request` | head plus Content-Length body |
| `Request.query/param/cookie/path_only/raw_query/wants_keep_alive` | request access (`param` also reads form bodies) |
| `Response::text/html/json/empty/redirect`, `Response.set_header/set_cookie` | response builders |
| `send_response(&Conn, &Response) -> i64` | serialize and write a response |
| `Response.keep_alive` | advertise a persistent connection |
| `Router::new()`, `.get/.post/.put/.delete/.set_fallback`, `.dispatch/.serve` | Go-style handler table |
| `serve(&router, port)`, `serve_n(...)` | high-level accept loop (forever or request-bounded) |
| `serve_local(&router, port)`, `serve_local_n(...)` | loopback-only server helpers for local development/tests |
| `serve_static(root, path)`, `serve_dir(root, port)`, `serve_file(path)` | static files with MIME detection and traversal checks |
| `serve_dir_local(root, port)`, `serve_dir_local_n(...)` | loopback-only static-site helpers |
| `read_file`, `write_file_bytes`, `file_size` | binary-safe file helpers (also used by downloads) |

Handlers are ordinary functions (`fn(&Request) -> Response`) stored and invoked
through function values, so routing looks like Go:

```zx
fn hello(req: &Request) -> Response {
    return Response::text(200, "hi");
}

fn my_404(req: &Request) -> Response {
    return Response::empty(404);
}

fn main() -> i32 {
    let mut router = Router::new();
    router.get("/hello", hello);
    router.set_fallback(my_404);
    // ... accept loop:
    //     if router.serve(&c) { served += 1; }
    0
}
```

Routers hold up to 32 (method, path) pairs. A trailing `/*` makes a prefix
route (`/api/*` matches `/api/echo`) and `*` matches everything. Named path
parameters and middleware chains are not supported yet; use
`Request.query` for inputs. `Router.serve` also answers oversized bodies
with `413` before calling a handler.

A directory can be served in one call, much like Python's built-in file server:

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
NUL with `403`, returns `404` for missing files and `413` above 32 MiB. MIME
types follow the extension. Bodies are copied byte-for-byte: `send_response`
writes the head and body separately, so images and other binary files survive
(plain `render_response` is for text only). `Client.download(url, path)` is the
client-side counterpart; `read_file`/`write_file_bytes` are binary-safe.

Client connections get a 30 s default timeout and every body read is capped
(`MAX_BODY`, 16 MiB). Requests are framed by Content-Length; responses support
Content-Length, chunked encoding, and read-until-close.

Still deliberately out of scope: TLS/HTTPS, HTTP/2, connection pooling, a
client cookie jar (servers can read/set cookies, and clients can send them
with `set_header`), multipart uploads, range requests, path-pattern routing,
and middleware. Function values are supported, but the lack of dynamic
collections keeps route tables fixed at 32 entries.

## 8.7 What this demonstrates

- `extern "C"` FFI is real enough to bind the POSIX socket API with no compiler
  changes to the networking path.
- Struct layout, arrays-as-pointers, references, and the owning `String` (with
  `push_byte` for raw bytes and `find`/`substr`/`trim` for parsing) compose into
  a usable library.
- Multiple source files compile into one standalone binary.
