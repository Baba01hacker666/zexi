# 10. Everyday standard-library recipes

Zexi's embedded standard library is intentionally small, but it now covers the
common *script and service* tasks that otherwise make a language feel harder
than it needs to be. No package installation is required: import the facade,
then call its declarations directly.

> The M1-preview loader exposes imports both unqualified and through a
> qualifier. `import http.server;` makes `Router`, `Response`, and `serve_local`
> available directly *and* as `server::Router` / `server::serve_local`; the full
> path `http::server::Router` works too. Qualified lookups enforce `pub`;
> unqualified access is still backed by the flat merged namespace.

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
    handle_get("/health", hello);
    listen("127.0.0.1:8000")
}
```

The top-level form registers on the default mux and `listen` takes an address
string, so it reads like Go's `http.HandleFunc` + `http.ListenAndServe`. For an
explicit router, `let mut app = new_app(); app.get("/health", hello);` then
`serve_local(&app, 8000)` behaves as before.

For a deterministic tool or test, use `serve_local_n(&app, count, port)` (or
`listen_n(addr, count)`) to exit after `count` valid HTTP requests; empty TCP
readiness probes do not count.

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
[`examples/local_tools.zx`](../examples/local_tools.zx),
[`examples/regex.zx`](../examples/regex.zx), and
[`examples/math_tools.zx`](../examples/math_tools.zx).

## 10.5 Everyday numerical work

`import math;` is a dependency-free, pure-Zexi toolbox for the common numeric
jobs that should not require an external package or a `-lm` linker flag:

```zx
import math;

fn main() -> i32 {
    let angle = atan2(4.0, 3.0);       // atan2(y, x), in radians
    println(degrees(angle));
    println(sin(radians(30.0)));

    println(exp(1.0), ln(E));
    println(powf(9.0, 0.5), cbrt(-125.0));
    println(smoothstep(0.0, 100.0, 50.0));
    println(distance(0.0, 0.0, 3.0, 4.0));
    0
}
```

| Group | APIs |
| --- | --- |
| Constants and conversion | `PI`, `TAU`, `E`, `FRAC_PI_2`, `FRAC_PI_4`, `SQRT_2`, `LN_2`, `LN_10`, `radians`, `degrees` |
| Basic float work | `min`, `max`, `clamp`, `abs`, `sign`, `copysign`, `floor`, `ceil`, `round`, `trunc`, `fract`, `saturate`, `midpoint` |
| Exponentials and logs | `exp`, `exp2`, `ln`/`log`, `log2`, `log10`, `log_base`, `powf` |
| Trigonometry | `sin`, `cos`, `tan`, `atan`, `atan2`, `asin`, `acos`, `sinh`, `cosh`, `tanh` |
| Roots and geometry | `sqrt`, `cbrt`, `root`, `hypot`, `distance`, `distance_sq` |
| Ranges and animation | `lerp`, `inverse_lerp`, `remap`, `remap_clamped`, `smoothstep`, `smootherstep`, `approx_eq` |
| Integers and number theory | `gcd`, `lcm`, `isqrt`, `powi`, `pow_mod`, bit helpers, `factorial`, `choose`, `is_prime`, `next_prime` |

`pow(base, exponent)` retains its efficient **integer** exponent API; use
`powf(base, exponent)` for real exponents. `powf` accepts any real exponent for
a positive base and also accepts whole `i32` exponents for a negative base;
other negative-base real powers return `0.0` because they have no real result.

The approximation-based transcendental functions are designed for normal app,
visualization, game, and scripting calculations—not correctly rounded,
scientific-edge-case work. `exp` saturates positive overflow at `MAX_F64` and
underflows extreme negative input to `0.0`; `ln`, logarithms, and real roots
with no real value return `0.0`. `is_nan`, `is_finite`, and `is_infinite` let a
caller explicitly inspect exceptional floating-point values. For stricter
platform-libm semantics, bind the required C function explicitly as described
in [`09-c-bridge.md`](09-c-bridge.md).

Run [`examples/math_tools.zx`](../examples/math_tools.zx) for a compact tour.
