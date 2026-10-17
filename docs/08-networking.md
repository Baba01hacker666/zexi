# 8. Networking & HTTP

Zexi has no standard library to speak of yet — but it does have the **C ABI**,
and that is enough to write a real HTTP client and server in ordinary Zexi code.
Everything here is implemented in [`lib/http.zx`](../lib/http.zx) and runs today.

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

## 8.4 Compiling against a library

There is no `use`/`mod` yet, so a program is compiled **together with** the
files it needs; the first file owns `main`, the rest are compiled into it:

```sh
zexic run examples/http_server.zx lib/http.zx
zexic run examples/http_client.zx lib/http.zx
```

See [`examples/http_server.zx`](../examples/http_server.zx) and
[`examples/http_client.zx`](../examples/http_client.zx). The test suite builds
both and exercises the round trip over loopback
(`TestHTTPServerAndClient` in `internal/driver/driver_test.go`).

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

## 8.6 What this demonstrates

- `extern "C"` FFI is real enough to bind the POSIX socket API with no compiler
  changes to the networking path.
- Struct layout, arrays-as-pointers, references, and the owning `String` (with
  `push_byte` for raw bytes and `find`/`substr`/`trim` for parsing) compose into
  a usable library.
- Multiple source files compile into one standalone binary.
