# 10. Everyday standard-library recipes

Zexi's embedded standard library is intentionally small, but it now covers the
common *script and service* tasks that otherwise make a language feel harder
than it needs to be. No package installation is required: import the facade,
then call its declarations directly.

> The current M1-preview loader is deliberately flat. For example,
> `import http.server;` makes `Router`, `Response`, and `serve_local` available
> directly; it does **not** create an `http.server.*` namespace yet.

## 10.1 A local-only HTTP service

Use `serve_local` during development when a server should be reachable only
from the same machine. It binds `127.0.0.1`, unlike `serve`, whose default host
is `0.0.0.0`.

```zx
import http.server;

fn hello(req: &Request) -> Response {
    return Response::json(200, "{\"ok\":true}");
}

fn main() -> i32 {
    let mut app = Router::new();
    app.get("/health", hello);
    serve_local(&app, 8000)
}
```

For a deterministic tool or test, use `serve_local_n(&app, count, port)`. It
exits after `count` valid HTTP requests; empty TCP readiness probes do not count.

To serve a local directory without writing an accept loop:

```zx
import http.server;

fn main() -> i32 {
    let root = arg_at(1);
    serve_dir_local(root.as_str(), 8000)
}
```

`serve_dir_local_n` is the bounded counterpart. Both static helpers preserve
binary files, map `/` to `index.html`, and reject traversal attempts. Use
`serve` or `serve_dir` with an explicit host only when exposure beyond loopback
is intentional.

## 10.2 Quick HTTP client calls

The HTTP facade has a one-call API for the common cases:

```zx
import http.client;

fn main() -> i32 {
    let status = get_status("http://127.0.0.1:8000/health");
    println("health:", status);

    let created = post_json("http://127.0.0.1:8000/items", "{\"name\":\"zexi\"}");
    if created.ok() {
        println(created.body);
    } else {
        println("request failed:", created.error);
    }

    let form = post_form_url("http://127.0.0.1:8000/login", "name=Ada&role=admin");
    println(form.status);
    0
}
```

Use `request(method, url, body = "", content_type = "")` for another verb,
or create a `Client` when you need custom headers, timeout settings, redirect
control, or several requests with the same configuration. HTTP is plaintext
only today: an `https://` URL returns a `Response` with `error` set rather than
silently downgrading the connection.

At the TCP level, `connect_local(port)` and `listen_local(port)` are short forms
for loopback connections. They remain useful for small protocols that are not
HTTP.

## 10.3 Local process and filesystem work

`import os;` selects the `sys` module. It offers process-local, POSIX-backed
helpers without requiring handwritten C declarations:

```zx
import os;

fn main() -> i32 {
    println("cwd:", cwd());
    println("host:", hostname());
    println("pid:", pid(), "cpus:", cpu_count());

    let mode = env_or("APP_MODE", "development");
    println("mode:", mode);

    // These change only this process and children it later starts.
    env_set("APP_MODE", "test");
    println(env_get("APP_MODE"));
    env_unset("APP_MODE");
    0
}
```

Useful path/process calls are:

| API | Behavior |
| --- | --- |
| `cwd()`, `hostname()` | Return a local string, or `""` if the OS call fails. |
| `home_dir()`, `temp_dir()` | Read conventional environment locations (`/tmp` is the fallback temp directory). |
| `env_has`, `env_get`, `env_or` | Inspect environment variables; `env_has` distinguishes an empty value from an unset variable. |
| `env_set`, `env_unset` | Change only the running process environment; return `0` on success, `-1` on failure. |
| `time_ms`, `mono_ms`, `millis_since`, `sleep_ms` | Wall clock and monotonic timing helpers. |
| `mkdir(path)` | Create one directory level; an existing directory is success; missing parents are **not** created. |
| `chdir`, `rename_path`, `remove_path` | Explicit local filesystem operations; return `0` or `-1`. `remove_path` is never recursive. |

For file contents, combine this with `import io.fs;`. `read_bytes`,
`write_bytes(path, &String)`, and `copy_file` preserve embedded NUL bytes and
cap individual reads/writes/copies at 32 MiB. A missing read is `""`; use
`path_exists` or `file_len` when an empty file must be distinguished from an
error.

## 10.4 Regular expressions

`import text.regex;` adds a compact, opt-in regular-expression helper set.
It uses the platform's **POSIX extended regular expression (ERE)** engine, so
it is suitable for validation and lightweight parsing without adding a large
runtime:

```zx
import text.regex;

fn main() -> i32 {
    let label = "build-42";
    println(is_valid("^[a-z]+-[0-9]+$"));
    println(is_match("[0-9]+", label));       // search anywhere
    println(full_match("[0-9]+", "42"));     // whole string only
    println(find("[0-9]+", label));
    println(replace_first("[0-9]+", label, "XX"));
    0
}
```

| API | Behavior |
| --- | --- |
| `is_valid(pattern)` | Check whether a POSIX ERE pattern compiles. |
| `is_match(pattern, text)` | Search for any matching substring. |
| `full_match(pattern, text)` | Require the entire text to match. |
| `match_span(pattern, text)` | Return a `RegexSpan { start, end }` byte range; both fields are `-1` when absent or invalid. |
| `find_index`, `find` | Return the first match position or first matching string. |
| `replace_first` | Replace the first matching substring; invalid/missing patterns leave text unchanged. |

This is **not** PCRE or Python `re`: use `[0-9]` or `[[:digit:]]` instead of
`\d`, and do not expect look-around, named captures, Unicode properties, or a
collection-returning `find_all` yet. Inputs are C-style text strings, so
embedded NUL bytes are unsupported. Matching runs synchronously with no
per-call timeout, so do not accept untrusted patterns in a latency-critical
service. The small API is intentional until dynamic collections and richer
result types land.

Ready-to-run examples: [`examples/http_app.zx`](../examples/http_app.zx),
[`examples/static_server.zx`](../examples/static_server.zx),
[`examples/local_tools.zx`](../examples/local_tools.zx), and
[`examples/regex.zx`](../examples/regex.zx).
