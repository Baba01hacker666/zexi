# 10. Everyday standard-library recipes

Zexi's embedded standard library is intentionally small, but it now covers the
common *script and service* tasks that otherwise make a language feel harder
than it needs to be. No package installation is required: import the facade,
then call its declarations directly.

> The loader exposes imports both unqualified and through a
> qualifier. `import http.server;` makes `Router`, `Response`, and `serve_local`
> available directly *and* as `server::Router` / `server::serve_local`; the full
> path `http::server::Router` works too. Qualified lookups enforce `pub`;
> unqualified access is still backed by the flat merged namespace.

## 10.1 A local-only HTTP service

Use `serve_local` during development when a server should be reachable only
from the same machine. It binds `127.0.0.1`, unlike `serve`, whose default host
is `0.0.0.0`.

<!-- check-only -->
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

<!-- check-only -->
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

For file contents, combine this with `import io.fs;`. `fs::read`,
`fs::write(path, &[u8])`, and `fs::copy` preserve embedded NUL bytes and cap
individual reads/writes/copies at `fs::MAX_BYTES` (32 MiB). They return
`Result<_, IoError>` (`IoError` comes from `sys`), so an empty file is
`Ok("")` and a missing one is `Err(IoError::NotFound)` instead of the same
empty string.

```zx
use fs;
use sys;

fn main() {
    let path = "/tmp/zx_fs_recipe.txt";
    let written = fs::write(path, "hello".as_bytes());
    match fs::read(path) {
        Ok(text) => println(text.len()),          // 5
        Err(e) => println(sys::message(e)),
    }
    let removed = fs::remove(path);
}
```

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

## 10.6 Concurrency: threads, channels, and atomics

`use thread;` adds real OS threads with fork–join, blocking `i64` channels, and
lock-free `i64` atomics. It is the one stdlib module that links a thread shim
(and `-pthread`), and only when imported:

```zx
use thread;

fn worker(ch: i64) -> i64 {
    let mut i: i64 = 1;
    while i <= 5 {
        chan_send(ch, i * i);
        i += 1;
    }
    chan_close(ch);
    return 0;
}

fn main() -> i32 {
    let ch = Chan::new(4);
    let t = Thread::spawn(worker, ch.id());
    let mut sum: i64 = 0;
    let mut v: i64 = 0;
    while ch.recv(&mut v) {
        sum += v;
    }
    t.join();
    println(sum);        // 55
    0
}
```

Only `i64` values cross a thread boundary, which keeps the subset data-race-free.
The full guide — `spawn`/`spawn2`/`spawn_void`, the free-function forms a worker
uses, atomics, limits, and the `Send`/`Sync` roadmap — is in
[`11-concurrency.md`](11-concurrency.md), and
[`tests/threads.zx`](../tests/threads.zx) is a runnable tour.

## 10.7 Formatting numbers and units

`use fmt;` is a pure-Zexi (no C shim) toolbox for the shapes a program prints:
bases, grouping, fixed decimals, human units, ordinals, and alignment.

```zx
use fmt;

fn main() -> i32 {
    println(fmt::hex(255, true));          // FF
    println(fmt::hex_pad(255, 4));         // 00ff
    println(fmt::thousands(1234567));      // 1,234,567
    println(fmt::fixed(3.14159, 2));       // 3.14
    println(fmt::sci(12345.0, 2));         // 1.23e4
    println(fmt::percent(0.256, 1));       // 25.6%
    println(fmt::bytes(1536));             // 1.5 KB
    println(fmt::duration(3661000));       // 1h 1m 1s
    println(fmt::ordinal(21));             // 21st
    println("[" + fmt::pad_left("42", 6) + "]");   // [    42]
    println("[" + fmt::pad_center("ab", 6, '-') + "]"); // [--ab--]
    0
}
```

| Group | APIs |
| --- | --- |
| Bases | `hex(n, upper)`, `hex_pad`, `radix(n, base)` (2..36) |
| Decimal grouping | `thousands`, `thousands_sep` |
| Decimals and exponents | `fixed`, `sci`, `percent` |
| Human units | `bytes`, `duration` |
| Ordinals | `ordinal` |
| Padding | `pad_left`, `pad_right`, `pad_center` (each takes an optional fill character) |

Negative integers keep a leading `-` in front of the digits (`-ff`, not a
two's-complement form). `fixed` rounds half away from zero, prints `NaN` for
NaN, and keeps the sign for infinities (`-inf`/`inf`); it falls back to the
built-in float formatting for values too large to scale exactly. `bytes`
rounds before promoting units, so `fmt::bytes(999_999)` is `"1.0 MB"`, not
`"1000.0 KB"`. `radix` clamps its base to 2..36 and `fixed`/`sci` clamp their
digit count (0..18). The old names (`hex_lower`, `fixed_point`, `byte_size`,
`align_right`, and friends) remain as aliases. See
[`tests/formatting.zx`](../tests/formatting.zx) for every function.

## 10.8 Encoders and checksums

`use codec;` is a pure-Zexi (no C shim) module for the conversions around the
network edge: hex and base64 for tokens, Basic auth, and data URIs, plus the
checksums used for ETags and integrity checks.

```zx
use codec;

fn main() -> i32 {
    println(codec::hex_encode("hi"));          // 6869
    println(codec::hex_decode("68656c6c6f"));  // hello
    println(codec::base64_encode("hello"));    // aGVsbG8=
    println(codec::base64_encode_url("a?b"));  // YT9i
    println(codec::base64_decode("aGVsbG8=")); // hello
    println(codec::crc32_hex("123456789"));    // cbf43926
    println(codec::fnv1a64_hex("hello"));      // a430d84680aabd0b
    0
}
```

| Group | APIs |
| --- | --- |
| Hex | `hex_encode`, `hex_encode_upper`, `hex_decode`, `hex_encode_bytes`, `hex_encode_bytes_upper`, `hex_decode_bytes` |
| Base64 | `base64_encode`, `base64_encode_url`, `base64_decode`, `base64_encode_bytes`, `base64_encode_url_bytes`, `base64_decode_bytes` |
| Checksums | `crc32`, `crc32_hex`, `adler32`, `adler32_hex`, `fnv1a32`, `fnv1a32_hex`, `fnv1a64`, `fnv1a64_hex`, `djb2`, `djb2_hex` (each with a `_bytes` twin) |

`crc32` is the zlib/IEEE 802.3 CRC-32; `adler32` matches zlib; `fnv1a32`/`fnv1a64`
are 32-/64-bit FNV-1a; `djb2` is Bernstein's hash. The numeric forms return an
`i64` (`u64` for the 64-bit hashes) and the `*_hex` forms return a zero-padded
lowercase digest. `hex_decode` and `base64_decode` return an empty string on
malformed input rather than guessing; the `_bytes` decoders return
`Result<String, String>` instead, which is what you want for a token that came
off the wire:

```zx
use codec;
use fs;

fn main() -> i32 {
    // Binary data goes in and comes back byte for byte, NUL included.
    let mut data = String::new();
    data.push_byte(0);
    data.push_byte(255);
    match fs::write("/tmp/zx_recipe.bin", data.as_bytes()) {
        Ok(_) => {}
        Err(_) => {}
    }
    match fs::read("/tmp/zx_recipe.bin") {
        Ok(raw) => println(codec::hex_encode_bytes(raw.as_bytes())), // 00ff
        Err(_) => println("read failed"),
    }
    match codec::base64_decode_bytes("aGVsbG8=".as_bytes()) {
        Ok(text) => println(text.as_str()),  // hello
        Err(_) => println("bad token"),
    }
    match fs::remove("/tmp/zx_recipe.bin") {
        Ok(_) => {}
        Err(_) => {}
    }
    0
}
```

See [`tests/encoding.zx`](../tests/encoding.zx) and
[`tests/std_bytes.zx`](../tests/std_bytes.zx) for the standard test vectors.
