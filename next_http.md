# Zexi HTTP servers: what is missing, and where it comes from

Status as of 2026-11-22, branch `arena/3c53e4fd-zexi` (PR #15), commit `0e9830d`.

Two servers ship:

- **Lean** — `examples/http_app.zx` on `lib/http.zx`. One request per call, blocking. Good for reading.
- **Full** — `examples/http_full.zx` on the embedded `http` module (`internal/driver/std/http.zx`, `net.zx`). A worker pool with a readiness poller.

This file lists what is still missing, and the source each item is mapped from:

- Go `net/http`: the local tree at `/tmp/gobin/x/go/src/net/http/server.go` (Go 1.27.4), line numbers as of that tree.
- Node.js `http`: https://nodejs.org/docs/latest-v22.x/api/http.html
- hyper (Rust): https://docs.rs/hyper/latest/hyper/server/conn/http1/struct.Builder.html
- nginx: https://nginx.org/en/docs/http/ngx_http_core_module.html (directive names). Defaults below come from secondary pages that quote nginx's docs.

---

## 1. Measured in this round

Machine: 2 cores, so the load client shares the CPU with the server. Client: `/tmp/loadgo` (Go, raw keep-alive sockets). Server: `http_full` with 4 workers.

| Scenario | Clients | Errors | Throughput | p50 | p99 |
| --- | --- | --- | --- | --- | --- |
| keep-alive `/health` | 40 | 0 | 25.8k req/s | 1.4 ms | 4.7 ms |
| keep-alive `/health` | 200 | 0 | 27.9k req/s | 6.6 ms | 19.5 ms |
| keep-alive `/health` | 1000 | 0 | 28.6k req/s | 30 ms | 86 ms |
| keep-alive `/health` | 2000 | 0 | 22.1k req/s | 75 ms | 176 ms |
| keep-alive `/api/notes` (real handler) | 1000 | 0 | 19.3k req/s | 48 ms | 125 ms |
| `Connection: close` churn | 1000 | 0 | 8.6k req/s | 24 ms | 107 ms |

Latency grows with the client count because requests queue behind the CPU. Nothing fails.

Fixed in this round (each has a regression test in `internal/driver/http_full_test.go`):

- **Pipelining, split blank line.** When `\r\n\r` arrived in one read and `\n` in the next, the head scan missed the blank line. The head then ran into the next pipelined request, which was read as this request's headers. Fixed by scanning 3 bytes back and using absolute offsets. `TestPipelinedRequestsAreAnswered`.
- **Slow-client denial of service.** `read_head` and `read_exact` restarted their timer on every `recv`. One byte every half second held a worker indefinitely. With two such clients on a 2-worker pool, a probe waited 10.5 s, the full length of the attack. Now the head and the body run under absolute deadlines, and the probe gets through in about 0 s. `TestTricklingHeadCannotHoldWorkers` (fails on the previous code).
- **`select()` → `poll()`.** `zxn_poll_read` used `select()`, which cannot watch a descriptor at or above `FD_SETSIZE` (1024). A server with more than about 1000 open connections would misbehave. Now `poll()`.
- **Monotonic clock.** Earlier round: `clock()` counted CPU time, so idle connections never aged out.

Developer note: `zexic` embeds the standard library at build time. After changing anything under `internal/driver/std/`, rebuild `/tmp/zexic` (`go build -o /tmp/zexic ./cmd/zexic`). A stale binary silently runs the old code. This happened once in this round.

---

## 2. Mapped from Go `net/http`

| Go construct (server.go) | What it does | Zexi today | Gap |
| --- | --- | --- | --- |
| `go c.serve(connCtx)` (~3608) | One goroutine per connection | Poller thread + fixed worker pool (8 by default, max 64) | Threads, not goroutines. Idle connections cost a poll slot (good). Active connections cost a thread (bad under slow handlers). |
| `conn.serve` loop (~1953) | Read, dispatch, write, repeat | `serve_count` / `serve_one` | Same shape. |
| `c.bufr.Peek(4)` before the header timer (~2197) | Waits for the next request to start, then starts `ReadHeaderTimeout` | Poller waits for readability, then the worker starts the head deadline | Same idea. Done. |
| `c.bufr` persists per connection (`newBufioReader`, ~2059) | Bytes after one request stay for the next one | Kernel buffer, read with `MSG_PEEK` up to the blank line | Works now, but only because nothing is buffered in user space. A head over 8 KiB, or a body read ahead, has nowhere to go. |
| `readRequest` (~1028) with `MaxHeaderBytes` (`DefaultMaxHeaderBytes = 1 MB`, ~935) | Head size limit, configurable | 8 KiB fixed in the code | Make it a router setting. |
| `chunkWriter.writeHeader` (~1309) decides keep-alive before writing headers | The response says `Connection: close` when the server will close | `serve_one` computes `keep` before dispatch (fixed earlier) | Done. |
| `ReadHeaderTimeout` (~3102) / `IdleTimeout` (~3115) / `ReadTimeout` (~3094) | Split deadlines | `header_timeout` (1 s), `KEEPALIVE_IDLE_MS` (5 s), `DEFAULT_TIMEOUT_MS` (30 s, body) | Same split. No `WriteTimeout`. |
| `Server.Shutdown(ctx)` (~3276) and `closeIdleConns` (~3330) | Stop accepting, close idle, wait for active requests up to `ctx` | `stop_serving()` sets a flag; workers finish their request and exit | No deadline. Idle connections are dropped, active ones are waited for without a limit. |
| `Request.Body` is an `io.Reader`, `MaxBytesReader` (`request.go`, ~1199) | Streaming request bodies with a cap | Whole body read into a `String` (`MAX_BODY` 16 MiB) | Needs traits or at least a reader type (see §5). |
| `ResponseWriter` streams and flushes | Chunked output, `Flush()` | Whole response built as a string, sent with Content-Length | No streamed or chunked responses. |
| `Expect: 100-continue` (`expectContinueReader`, ~984) | Sends `100 Continue` when the body is first read | Not handled | Clients that wait for it stall about one second, then send anyway. |
| `Hijacker` | Takes over the connection (WebSocket) | None | No upgrades. |
| `ErrAbortHandler`, `recover()` in `conn.serve` (~1960) | A handler panic closes one connection | Zexi panics abort the process | Needs a per-request boundary. |

## 3. Mapped from Node.js, hyper, and nginx

| Setting | Source default | Zexi today | Action |
| --- | --- | --- | --- |
| Node `headersTimeout` | min(60 s, `requestTimeout`); answers **408** | 1 s; answers **400** for an incomplete head | Consider 408 (RFC 9110 §15.5.9). Make the value configurable. |
| Node `requestTimeout` | 300 s, whole request including body | Head 1 s and body 30 s, separately | Add a total request deadline. |
| Node `keepAliveTimeout` | 5 s (Node 22 docs) | 5 s (`KEEPALIVE_IDLE_MS`) | Make it configurable on the router. |
| Node `keepAliveTimeoutBuffer` | 1 s added to the keep-alive timeout, to avoid ECONNRESET | None | Add if clients behind proxies hit resets. |
| Node `maxRequestsPerSocket` | 0 (no limit) | `MAX_REQUESTS_PER_CONN` 100 | Make it configurable. |
| Node `maxHeadersCount` | Limits header fields, not requests | None | Add a field-count limit. |
| Node `server.closeIdleConnections()` / `closeAllConnections()` | Explicit close helpers | Implicit on stop | Add both. |
| hyper `header_read_timeout` | 30 s; needs a timer | 1 s head budget | Make it configurable (already exists as `header_timeout`). |
| hyper `max_buf_size` | Minimum 8192, default ~400 KB | 8 KiB head, `MAX_BODY` 16 MiB | Configurable buffer sizes. |
| hyper `pipeline_flush` | Batches writes for pipelined responses | One `send` per response | Low priority. Pipelining is correct now, but not batched. |
| nginx `keepalive_timeout` | 75 s | 5 s idle | Reasonable for a small server. |
| nginx `keepalive_requests` | 1000 | 100 | Raise the default, or make it configurable. |
| nginx `keepalive_time` | 1 h total connection lifetime | 30 s (`CONNECTION_LIFETIME_MS`) | Make it configurable. |
| nginx `client_header_timeout` | Head deadline | Same as header budget | Done. |
| nginx `lingering_close` | Drains unread bytes before closing so the reset cannot eat the response | `c.drain(DRAIN_MS)` after error answers | Done. |
| nginx `worker_connections` | Caps connections per worker | No cap. The poller grows without limit (only `EMFILE` backoff) | Add a connection cap that answers 503 or closes. |

## 4. Missing, in priority order

### P0 — safety and correctness

1. **The lean server still has the slow-client flaw.** `lib/http.zx` `read_head` (around line 211) uses `zxhttp_recv` with a per-recv idle timeout, so trickling clients hold its thread. Fix: copy `read_head_until` from `net.zx` (absolute deadline).
2. **Partial heads and bodies hold a worker.** A head that trickles in still occupies a worker for up to `header_timeout`. Two trickling clients on a 2-worker pool make everyone else wait about 1 s. The fix belongs in the poller: keep bytes read so far in a per-connection buffer, hand the connection to a worker only when the head is complete, and let the worker read from that buffer first. This requires `read_head` to accept a prefix. Source: Go's `Peek(4)` then `ReadHeaderTimeout` (goroutines never block a thread on a partial head).
3. **`Expect: 100-continue`.** When the head has `Expect: 100-continue` and the body has not arrived, send `HTTP/1.1 100 Continue` first. Source: Go `readRequest` and `chunkWriter`.
4. **408 for a timed-out head**, to match Node and RFC 9110.

### P1 — scale

5. **The poller rebuilds its `poll()` set every 50 ms.** That is O(connections) per wake-up. It is fine at 2000 connections (measured above) and will not be fine at 20 000. Use `epoll` on Linux, with per-connection state in the event data.
6. **No connection cap.** Add `max_connections`; above it, answer 503 or close. Source: nginx `worker_connections`.
7. **Threads, not goroutines.** A slow handler holds a thread, so throughput is limited to `workers / handler_time`. Options: a larger default pool (cheap, limited), or non-blocking handlers, which needs the language work in §5.

### P2 — features

8. **Graceful shutdown with a deadline.** `shutdown(timeout_ms)`: stop accepting, close idle connections now, wait for active requests up to the timeout, then force-close. Sources: Go `Shutdown(ctx)`, Node `server.close()` with `closeIdleConnections()`.
9. **Chunked responses and streamed bodies.** Needed for large or generated responses, and for server-sent events. Source: Go `ResponseWriter` and `Flush`.
10. **Chunked request bodies with keep-alive.** Today a chunked request is answered and then closed (`serve_one`, `req.chunked()`). The body reader exists (`read_chunked`, used by the client). Use it, then keep the connection. Source: Go `transfer.go` (the chunked reader).
11. **Configurable limits** on the router: head size (Go default 1 MB), header count (Node `maxHeadersCount`), requests per connection (nginx 1000), connection lifetime (nginx 1 h), total request deadline (Node 300 s).
12. **TLS.** None. Go has `crypto/tls`, and the server would need it for anything public.
13. **HTTP/2, upgrades.** None. Hijack (WebSocket) needs a way to hand a raw socket to user code.
14. **Panic boundary.** A handler that fails should answer 500 and close its connection, not abort the process. Source: Go `conn.serve` recovers per connection and logs.

## 5. Language gaps (Zexi)

These are what the server design wants, and what Zexi lacks. From `docs/02-language-spec.md` and `docs/03-memory-model.md`:

- **Closures as handlers. — closed** Handlers can now be closures that capture configuration or shared state by value (`docs/15-closures.md`); Result-returning handlers are supported too. Go: `HandlerFunc` closures. hyper: `Service` values.
- **Traits or generics for a `Reader`/`Writer` abstraction. — traits added** Monomorphized generics plus trait declarations, `impl Trait for T`, and bounds (`T: Reader`) now exist (`docs/22-traits.md`), with static dispatch at monomorphization. A `Reader`/`Writer` trait can be declared, but Zexi still needs a real generic body/stream type for it to be useful: `dyn Trait` and associated types are not implemented. Go: `io.Reader`, `io.Writer`. hyper: `Body`.
- **Typed `Mutex<T>` and `RwLock<T>`. — closed in the library** The new `sync` module (`internal/driver/std/sync.zx`, `docs/21-typed-sync.md`) wraps a pthread lock and the value it protects; guards carry `T` and expose `get`/`get_mut`. Go: `sync.Mutex`, `sync.RWMutex`.
- **Typed channels and `select` with a timeout. — closed** `recv_timeout`/`send_timeout` (real `pthread_cond_timedwait`) and `chan_select2` cover the `i64` channel (`docs/21-typed-sync.md` §21.3). A typed `tchan_*` family now moves arbitrary `T` — including owning values like `String` — through an i64 handle; it needed two compiler changes: `sizeof::<T>()`/`align_of::<T>()` builtins and binding-aware generic bodies (docs/12-generics.md §12.15). Go: `chan T`, `select` with `time.After`.
- **Non-blocking I/O as a language construct. — closed** The `async` module runs tasks on fibers; the `await(fd)` compiler builtin suspends a task until a descriptor is readable, and one scheduler thread polls all waiting descriptors (`docs/23-async.md`). A waiting request costs a poll entry, not a thread. No `async fn` keyword, `Future<T>`, or `dyn`-style dispatch — awaitables are descriptors. Go: goroutines over a netpoller. hyper: futures on tokio. nginx: an event loop per worker.
- **A way to pass richer values to workers. — closed for owned values** The pool passes a small integer and pins the router into a fixed slot, because only `i64` crosses threads safely. Typed channels (`tchan_*`) and refcounted `shared_*` values now move/borrow owning `T` through such a handle (`docs/21-typed-sync.md` §21.4–21.5). Go shares pointers under race-free rules; Zexi still needs the handle plus a documented lifetime discipline.

Not gaps, because the docs list them: `Vec<T>`, `HashMap<K, V>`, `String`, slices, `Option`/`Result`, `if let`, `while let`, `defer`.

## 6. Suggested order

1. Lean server: absolute head deadline (P0.1). Small.
2. `Expect: 100-continue` and 408 (P0.3, P0.4). Small.
3. `shutdown(timeout_ms)` (P2.8). Small to medium.
4. Partial-head buffer in the poller (P0.2). Medium. Needs `read_head` to accept a prefix. This is the change that removes the last way for a slow client to hold a worker.
5. Connection cap and configurable limits (P1.6, P2.11). Small to medium.
6. `epoll` (P1.5). Medium. Only needed past a few thousand connections.
7. Chunked responses and chunked request bodies (P2.9, P2.10). Medium.
8. Language: closures, typed locks, typed channels with `select`, traits, non-blocking I/O (§5). All are in: typed locks/channels/shared values (`docs/21-typed-sync.md`), `sizeof` builtins and binding-aware generic bodies (`docs/12-generics.md` §12.15), traits with static dispatch (`docs/22-traits.md`), and fiber tasks with `await` plus a readiness scheduler (`docs/23-async.md`).

## 7. Not yet verified

- Go 1.22 (the CI pin) has not been tested with these changes. Only Go 1.27.4 has.
- The body-trickle deadline (30 s) has no test. The head one does.
- The 2000-client run shares 2 CPUs with the load client, so server-only throughput is higher than measured.
- The lean server (`lib/http.zx`) has no pipelining test. Its `Reader` type is meant to handle it.
- A gcc warning, `_DEFAULT_SOURCE redefined`, appears in the output of some driver tests. It comes from the generated C in other tests, not from these changes, and it does not fail anything.
