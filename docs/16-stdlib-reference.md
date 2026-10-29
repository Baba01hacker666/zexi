# 16. Standard library reference

Every module below is built into the compiler: `use <module>;` needs no project
setup, no package download, and no extra C file. The modules whose work is done
in C (`json`, `net`, `thread`) link their runtime automatically when imported;
`sys` and `regex` carry their C inside their own `.zx` file as a `c """..."""`
block, which is the pattern any library can use — see
[`17-extending.md`](17-extending.md).

Signatures are written as they are declared in `internal/driver/std/<module>.zx`
— that file *is* the implementation, so it is also the place to look for the
exact behaviour of an edge case.

## 16.1 `web` — the short way to build a site

`web` is a thin convenience layer over `http`, `html`, and `json`. It is the
module to reach for unless you need something it does not wrap.

<!-- check-only -->
```zx
use web;
use html;
use json;

fn home(req: &Request) -> Response {
    let mut v = html::vars();
    v.set("title", "Home");
    return web::page("index", &v);        // templates/index.html
}

fn api(req: &Request) -> Response {
    let mut doc = json::object();
    doc.set_int("ok", 1);
    return web::json_of(&doc);
}

fn main() -> i32 {
    let mut app = web::new_app();
    app.get("/", home);
    app.get("/api", api);
    app.views("templates");               // where `page` reads templates
    app.pages("public");                  // /about -> public/about.html
    app.files("/static", "public/assets");
    app.workers(4);
    return app.serve(8000);
}
```

| function / method | description |
| --- | --- |
| `new_app() -> App` | empty app |
| `app.get/post/put/delete(path, handler)` | register a handler; `{name}` segments are captured |
| `app.route(method, path, handler)` | any method |
| `app.fallback(handler)` | replace the 404 handler |
| `app.before(mw)` / `app.after(mw)` | middleware around routing |
| `app.pages(root)` | serve a folder as the site (`/` → `index.html`, `/x` → `x.html` or `x/index.html`) |
| `http::project_path(path)` | resolve a relative folder against the entry directory; every folder argument in `web`/`http` goes through it, so `pages("public")` means *the project's* `public/`, not the shell's |
| `app.files(prefix, root)` | serve a folder under a URL prefix (longest prefix wins) |
| `app.views(dir)` | template folder used by `page` |
| `app.workers(n)` | threads `serve` uses (0 = two per core) |
| `app.dispatch(&mut req) -> Response` | route a parsed request without a socket |
| `app.serve_addr(addr = ":8000")` | serve on an address string |
| `app.serve(port = 8000, host = "0.0.0.0")` / `app.serve_local(port = 8000)` | run |
| `app.serve_n(port, max_requests)` | serve a request budget, then return (tests, one-shot tools) |
| `page(name, &vars) -> Response` | fill `templates/<name>.html` |
| `page_status(status, name, &vars)` | same, explicit status |
| `template_text(name)` / `template_path(name)` | the raw text / resolved path of a template |
| `html_page(body)` / `html_status(status, body)` | HTML responses |
| `text(body)` | plain-text response |
| `json_of(&value)` / `json_status(status, &value)` | JSON responses |
| `json_pair(key, value)` | `{"key": value}` |
| `json_ok()` | `{"ok": true}` |
| `error(status, message)` | `{"error": message}` with the given status |
| `bad_request(message)` / `not_found()` | `error(400, …)` / `error(404, …)` |
| `redirect(to)` | 302 with `Location` |
| `send_file(path)` | one file from disk with its MIME type |
| `params(req) -> Vars` | captured `{name}` segments |
| `query(req) -> Vars` | decoded query string |
| `form(req) -> Vars` | `application/x-www-form-urlencoded` body |
| `value(req, name)` | one value: query first, then the body |
| `json_body(req) -> json::Value` | parse the body (`null` when it is not JSON) |
| `views(dir)` / `views_dir(dir)` | template folder for the default mux |
| `get/post/put/delete(path, handler)`, `files`, `pages`, `serve*` | the same API on the process-wide default mux |

`App::serve_n` and `App::serve` return non-zero only when the address cannot be
bound.

## 16.2 `html` — escaping, widgets, templates

| function | description |
| --- | --- |
| `escape(s)` | `& < > " '` → entities |
| `attr(s)` | escape for an attribute value |
| `url_query(s)` | percent-encode a query component |
| `page(title, body)` / `page_with(title, head, body)` | a complete HTML document |
| `tag(name, text)` | `<name>text</name>` with the text escaped |
| `heading(level, text)` | `<h1>`…`<h6>` (level clamped to 1–6) |
| `link(href, text)` | anchor with both parts escaped |
| `list(items)` / `ordered_list(items)` | `<ul>` / `<ol>` from `Vec<String>` |
| `table(headers, rows)` | escaped table from a header row and rows |
| `json_script(id, json)` | `<script type="application/json">` that cannot be closed early |
| `script_safe(text)` | escape `<` so text can sit inside a script |
| `template(tpl, &vars)` | fill `{{name}}` / `{{name\|raw}}` from a `Vars` |
| `template_map(tpl, &map)` | the same, from a `HashMap<String, String>` |
| `template_pairs(tpl, &pairs)` | the same, from flattened key/value pairs |

`Vars` is the builder `page` takes:

<!-- fragment -->
```zx
let mut v = html::vars();
v.set("title", "Home");          // str
v.set_int("count", 3);           // i64
v.set_bool("admin", true);
v.get("title")                   // String ("" when unset)
v.has("title")                   // bool
v.len()                          // usize
html::Vars::from_map(map)        // take an existing map
```

Template rules: `{{name}}` is escaped, `{{name|raw}}` is inserted verbatim,
`{{{{` writes a literal `{{`, and an unknown name inserts nothing.

## 16.3 `http` — server and client

**Client** (urllib-shaped):

| function | description |
| --- | --- |
| `fetch(url) -> Response` / `get(url)` | GET |
| `post(url, body)`, `post_json(url, json)`, `post_form_url(url, form)` | POST with the right content type |
| `put_url(url, body)`, `delete_url(url)` | other methods |
| `request(method, url, body = "", content_type = "")` | any method |
| `get_status(url) -> i32` | status code only |
| `download(url, dest) -> Response` | write the body to a file |
| `Client::new()`, `.set_header(k, v)`, `.get/post/...` | a reusable client with settings (`timeout_ms`, `follow_redirects`, `max_redirects`, `user_agent` are fields) |
| `get_raw(host, port, path)`, `post_raw(...)` | talk to a host:port directly |
| `parse_url(raw) -> Url`, `url_encode`, `url_decode`, `query_value` | URL helpers |
| `parse_response(raw) -> Response` | parse a raw HTTP response |

`Response` fields: `status`, `content_type`, `headers`, `body`; methods
`ok()`, `header(name)`, `content_length()`, `set_header(name, value)`,
`set_cookie(name, value)`.

**Server**:

| function | description |
| --- | --- |
| `new_app() -> Router` | router |
| `router.get/post/put/delete/add(method, …)`, `set_fallback`, `use_before`, `use_after` | registration |
| `router.files(prefix, root)`, `router.pages(root)`, `router.views(dir)` | folder mounts |
| `serve(router, port, host)` / `serve_local(router, port)` / `serve_n(router, max_requests, …)` | single-threaded serving |
| `serve_workers(router, workers = 0, port, host)` | pooled serving (0 = two threads per core) |
| `serve_workers_n(router, workers, max_requests, …)` | pooled with a budget |
| `listen(addr)`, `listen_n(addr, max_requests)`, `listen_workers(addr, workers)` | default-mux entry points |
| `handle_get/handle_post/handle_put/handle_delete(path, fn)` | default-mux routes |
| `serve_file(path)`, `serve_static(root, url_path)`, `serve_page(root, url_path)` | file responses |
| `serve_dir(root, port)`, `serve_dir_n`, `serve_dir_local` | serve a folder with no code |
| `project_path(path)` | resolve a relative folder against the entry directory |
| `parse_request(raw) -> Request`, `read_request(&conn)`, `send_response(&conn, &resp)` | low level |
| `requests_served()`, `connections_served()`, `stop_serving()`, `serving_stopped()`, `reset_stats()` | pool control |

`Request` exposes `method`, `path`, `body`, `headers`, `query`, and methods
`param(name)`, `raw_query()`, `collect_params(&mut map)`, `collect_query`,
`collect_form`, `header(name)`, `wants_keep_alive()`.

## 16.4 `json`

| function | description |
| --- | --- |
| `object()` / `array()` | empty containers (`Value`) |
| `null()`, `of_bool`, `of_int`, `of_num`, `of_str`, `of_string` | constructors |
| `Value::set(key, value)`, `set_int`, `set_num`, `set_bool`, `set_str`, `push(value)`, `push_int`, `push_str` | builders |
| `Value::kind()` | 0 null, 1 bool, 2 int, 3 num, 4 str, 5 array, 6 object |
| `Value::as_int()` / `as_num()` / `as_str()` / `as_bool()` / `is_null()` | readers |
| `Value::get(key)`, `at(index)`, `len()`, `has(key)` | lookup |
| `stringify(&v)` / `pretty(&v, indent = 2)` | to text |
| `parse(raw) -> Result<Value, String>` | from text |
| `valid(raw) -> bool` | syntax check only |
| `quote(s)` | a JSON string literal |

Integers stay exact `i64`, doubles print in the shortest form that reads back,
and object key order is preserved.

## 16.5 `net`

| function | description |
| --- | --- |
| `connect(host, port, timeout_ms = 30000) -> Conn` | TCP client |
| `connect_local(port, timeout_ms)` | loopback TCP client |
| `listen_tcp(host, port) -> Server` / `listen_local(port)` | TCP listener |
| `resolve(host) -> String` | first IPv4 address, `""` when it does not resolve |
| `cpu_count() -> i32` | available cores |
| `poll_read(fd, ms)` | 1 readable, 0 timeout, −1 error |
| `udp_bind(host, port) -> Udp` / `udp_bind_local(port)` | UDP socket |
| `Udp::send_to(host, port, data)`, `recv_from() -> Datagram`, `close()` | datagrams |
| `last_error()`, `error_string(code)` | error text |

`Conn`: `read_some(buf)`, `write(bytes)`, `read_line()`, `read_head()`,
`peer()`, `local_port()`, `timeout(ms)`, `shutdown_write()`, `close()`, `fd()`.
`Server`: `accept()`, `accept_timeout(ms)`, `port()`, `fd()`, `set_nonblocking()`,
`close()`. `Datagram` carries `data`, `host`, `port`.

## 16.6 `thread`

| function | description |
| --- | --- |
| `spawn(f: fn(i64) -> i64, arg)` / `spawn2` / `spawn_void` | start a thread |
| `join(handle) -> i64` | wait and take the result |
| `Thread::new/spawn/join` | the handle form |
| `chan_new(cap)` → `Chan` | bounded channel |
| `Chan::send/recv/try_recv/close/len/closed` | channel operations |
| `atomic_new`, `Atomic::add/get/set` | atomics |
| `Pool::new(workers)`, `.submit(f, arg)`, `.wait()`, `.result(seq)`, `.map(f, lo, hi)`, `.free()` | fixed-size worker pool |
| `cpu_count()` | available cores |

Closures work as task bodies: `pool.map(|x| x * x, 0, 100)`.

## 16.7 `fs`, `io`, `sys`

| module | functions |
| --- | --- |
| `fs` | `read_text`, `read_bytes`, `write_text`, `write_bytes`, `copy_file`, `path_exists`, `file_len` |
| `io` | `read_line`, `prompt_line`, `read_all`, `input(prompt = "")`, `print_err`, `eprintln_err` |
| `sys` | `env_get`, `env_has`, `env_or`, `env_set`, `env_unset`, `time_ms`, `time_s`, `mono_ms`, `sleep_ms`, `pid`, `millis_since`, `cwd`, `hostname`, `home_dir`, `temp_dir`, `chdir`, `mkdir`, `remove_path`, `rename_path`, `cpu_count` |

## 16.8 `fmt`, `text`, `codec`, `math`, `random`, `regex`

| module | functions |
| --- | --- |
| `fmt` | `hex_lower`, `hex_upper`, `hex_pad`, `bin_str`, `oct_str`, `radix_str`, `thousands`, `thousands_sep`, `fixed_point`, `sci_notation`, `percent_str`, `byte_size`, `duration_ms`, `ordinal`, `align_left/right/center` |
| `text` | `to_upper`, `to_lower`, `trim_start/end/strip`, `replace_all`, `replace`, `index_of`, `last_index_of`, `count_occurrences`, `contains`, `starts_with`, `ends_with`, `pad_start`, `pad_end`, `slice_text`, `compare`, `repeat_text`, `join_with`, `is_blank`, `is_digits_only`, `is_digit/alpha/alnum/space`, `format_bool` |
| `codec` | `hex_encode`, `hex_encode_upper`, `hex_decode`, `base64_encode`, `base64_encode_url`, `base64_decode`, `crc32`, `adler32`, `fnv1a32`, `fnv1a64`, `djb2` (each with a `_hex` form where useful) |
| `math` | `min(max, clamp, abs, sign, gcd, lcm, pow, sqrt, isqrt, log2_floor, popcount, trailing_zeros, leading_zeros, is_pow2, div_floor, mod_floor, div_ceil, sin, cos, tan, hypot, lerp, inverse_lerp, remap, approx_eq, floor, ceil, round, trunc, mean2)` — integer forms take an `i`/`_i` suffix or plain names (`mini`/`min_i`), float forms are unprefixed |
| `random` | `seed_random`, `seed_from_time`, `rand_int(lo, hi)`, `rand_bool`, `rand_float`, `Rng::new` |
| `regex` | `is_valid`, `is_match`, `full_match`, `find`, `find_index`, `match_span`, `replace_first` |

## 16.9 Builtins that are not in a module

| builtin | description |
| --- | --- |
| `print` / `println` | write a value and a newline |
| `len(x)` | length of a string, array, `Vec`, or `HashMap` |
| `arg_count()` / `arg_at(i)` | command-line arguments (`arg_at` returns `String`) |
| `project_dir()` | directory of the entry file, baked into the binary |
| `String::from`, `Vec::new`, `HashMap::new` | container constructors |
| closures (`\|x\| …`) | see [§15](15-closures.md) |
