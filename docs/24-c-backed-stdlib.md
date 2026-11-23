# 24. C-backed standard library: filling the gaps Go-style

Zexi deliberately keeps small: several conveniences a bigger language would
put in the compiler are simply absent. They do not have to be absent from the
*standard library*. Each module in this chapter is ordinary Zexi source whose
own `c """..."""` block does the work, so `use <module>;` links it with no
driver changes and no compiler features. APIs are modeled on Go's standard
library, which has the same "small core, useful packages" shape.

| Module | Go reference | What it closes |
| --- | --- | --- |
| [`bufio`](../internal/driver/std/bufio.zx) | `bufio.Reader` | `io::read_line` issued one `read(2)` per byte (NEXT_STEPS.md §6) |
| [`process`](../internal/driver/std/process.zx) | `os/exec` | no way to run another program and capture its output |
| [`path`](../internal/driver/std/path.zx) | `path/filepath` | no path manipulation or `stat` queries |
| [`time`](../internal/driver/std/time.zx) | `time` | `sys` has clocks but no formatting |
| [`sort`](../internal/driver/std/sort.zx) | `sort` / `slices` | no sorting of arrays or slices |
| [`log`](../internal/driver/std/log.zx) | `log` / `log/slog` | no leveled logging |
| [`flag`](../internal/driver/std/flag.zx) | `flag` | only raw `arg_at` / `arg_count` |
| [`strings`](../internal/driver/std/strings.zx) | `strings` | no split / trim / case conversion helpers |
| `path::walk` (in [`path`](../internal/driver/std/path.zx)) | `filepath.Walk` | no recursive directory traversal |
| [`csv`](../internal/driver/std/csv.zx) | `encoding/csv` | no CSV parsing or quoting |
| [`strconv`](../internal/driver/std/strconv.zx) | `strconv` | no checked number parsing / formatting |
| [`glob`](../internal/driver/std/glob.zx) | `filepath.Glob` / `path.Match` | no filename matching |
| [`url`](../internal/driver/std/url.zx) | `net/url` | no URL parsing or percent decoding |
| [`container`](../internal/driver/std/container.zx) | `container/list` | no linked list |

## 24.1 `bufio`

<!-- check-only -->
```zx
use bufio;
use sys;

fn main() -> i32 {
    let fd = match sys::open_read("names.txt") {
        Ok(f) => f,
        Err(_) => return 1,
    };
    let r = Reader::from_fd(fd);
    loop {
        let line = r.read_line();
        if line.len() == 0 { break; }
        println(line);
    }
    r.close();
    sys::close_fd(fd);
    0
}
```

`Reader` keeps an 8 KiB window in C. `read_line` returns the next line without
`\n` (and without a preceding `\r`), an empty `String` only at EOF.
`read_bytes(buf, cap)` reads through the buffer first, and `buffered()` reports
what is still cached. The reader does not own the descriptor — close both.

## 24.2 `process`

```zx
use process;

fn main() -> i32 {
    let c = Command::new("/bin/echo");
    c.arg("hello");
    let out = c.output();
    println(out.status, out.stdout.trim(), out.stderr);
    c.free();
    0
}
```

C builds the `argv`, forks, wires two pipes, `execvp`s, drains stdout and
stderr, and `waitpid`s. `output()` returns an `Output` with `status`, `stdout`
and `stderr`. Streams are capped at 8192 bytes each; at most 63 arguments are
kept; status `-1` means the program could not be started. This is the C bridge
used the other way around — C is now the implementation layer, not the API.

## 24.3 `path`

`join`, `base`, `dir`, `ext`, `clean`, `realpath`, `exists`, `is_dir` and
`is_file`, all C-backed. `clean` is lexical (Go `filepath.Clean` semantics:
collapse `//`, drop `.`, resolve `..`). Results are capped at 8192 bytes.

```zx
use path;

fn main() -> i32 {
    println(path::join("/tmp", "a.txt"));        // /tmp/a.txt
    println(path::base("/tmp/a.txt"));           // a.txt
    println(path::ext("/tmp/a.txt"));            // .txt
    println(path::exists("/tmp"));               // true
    0
}
```

## 24.4 `time`

`now_s`, `now_ms` and `format(epoch, strftime_layout)`. The layout syntax is
POSIX `strftime` (`%Y-%m-%d %H:%M:%S`), not Go's reference-time layout, because
that is what C already implements. `sys::time_ms`/`mono_ms`/`sleep_ms` remain
the primitives for measurement.

```zx
use time;

fn main() -> i32 {
    println(time::format(0, "%F"));              // 1970-01-01
    0
}
```

## 24.5 `sort`, `log`, `flag`

`sort` uses C `qsort` on a `(ptr, len)` slice passed by value, so it handles any
length. `sort::i64` / `sort::f64` need no callback; `sort::by_i64` passes a Zexi
closure to C and calls it back through a trampoline (the closure returns C
`bool`, which the trampoline respects). `reverse_i64` is a plain C loop.

```zx
use sort;

fn main() -> i32 {
    let mut v: [i64; 5] = [3, 1, 4, 1, 5];
    sort::i64(&mut v[0..5]);              // 1 1 3 4 5
    sort::by_i64(&mut v[0..5], |a, b| a > b);
    0
}
```

`log` writes timestamped lines to stderr with `log::debug/info/warn/error(msg)`
and a minimum level (`DEBUG=0` … `ERROR=3`, default INFO). Build messages with
f-strings: `log::warn(f"slow: {ms}ms")`.

`flag` registers flags before `parse()` and reads them back afterward:
`flag::int`, `flag::str`, `flag::bool`, then `get_i64`, `get_str`, `get_bool`.
It accepts `-n 3`, `--n 3`, `-n=3`, `--n=3`, bare boolean flags and `-h`.

```zx
use flag;

fn main() -> i32 {
    flag::int("n", 10, "how many");
    flag::parse();
    println(flag::get_i64("n"));
    0
}
```

## 24.6 `strings` and `path::walk`

`strings` follows Go's `strings` package: `contains`, `has_prefix`,
`has_suffix`, `index`, `to_upper`, `to_lower`, `trim`, `replace`, `split` and
`join`. C does the searching/case work; `split` is a loop over a C cursor that
returns field boundaries, so Zexi slices the original bytes and builds owned
`String`s without copying through a C buffer. Consecutive separators produce
empty fields, like Go.

```zx
use strings;

fn main() -> i32 {
    let parts = strings::split("a,b,c", ",");
    println(parts.len(), strings::join(&parts, ":"));  // 3 a:b:c
    0
}
```

`path::walk(root, cb)` is Go's `filepath.Walk` shape: C walks with
`opendir`/`readdir` and calls a Zexi closure `cb(path, is_dir)` for every entry,
including the root. A nonzero return stops the walk.

<!-- check-only -->
```zx
use path;

fn visit(p: str, is_dir: i32) -> i32 {
    println(path::base(p), is_dir);
    0
}

fn main() -> i32 {
    println(path::walk("/tmp", visit));
    0
}
```

## 24.7 `csv` and `strconv`

`csv::parse_line` returns a `Vec<String>` of unescaped fields (doubled quotes
collapse), and `csv::write(&fields)` quotes fields containing a comma, quote,
newline or surrounding space. Both stream one record at a time, so a CSV file
can be read through `bufio::Reader` line by line.

```zx
use csv;

fn main() -> i32 {
    let f = csv::parse_line("a,\"b,c\",d");
    println(f.len(), f[1]);   // 3 b,c
    println(csv::write(&f));  // a,"b,c",d
    0
}
```

`strconv::parse_i64` / `parse_f64` return `Option` (no silent zero on bad
input); `format_i64`, `format_u64`, `format_f64(v, precision)` and `itoa`
return owned `String`s built by C's `snprintf`.

## 24.8 `glob`, `url`, `container`

`glob::match_(pattern, name)` uses C `fnmatch` (`*`, `?`, `[...]`);
`glob::files(pattern)` enumerates one directory with `readdir` and returns the
matching names, including the directory prefix. At most 64 names and 8192 bytes
are returned, and the order is readdir order. The function is spelled `match_`
because `match` is a Zexi keyword.

```zx
use glob;

fn main() -> i32 {
    println(glob::match_("*.txt", "notes.txt"));  // true
    0
}
```

`url::parse` returns a `Url` with `scheme`, `host`, `port`, `path`, `query` and
`fragment` fields. `query_get`, `encode` and `decode` handle percent escapes
(plus decodes to a space, like HTML form values). IPv6 literals are not split
out of the host.

```zx
use url;

fn main() -> i32 {
    let u = url::parse("https://example.com:8080/a/b?x=1#top");
    println(u.host, u.port, u.path, url::query_get(u.query.as_str(), "x"));
    0
}
```

`container::List` is a C doubly linked list of i64 values with `push_front`,
`push_back`, `pop_front`, `pop_back`, `front`, `back`, `len`, `contains`,
`remove` and `free`. `container::Heap` is a C min-heap with `push`, `peek`,
`pop`, `len` and `free` (Go's `container/heap` shape). Like the other handle
types, neither is `Copy` or thread-safe on its own — guard a shared handle with
`sync::Mutex`.

## 24.9 Why C, and what the limits are

- Every module carries its own C; none adds a driver-side C blob or a linker
  flag. This keeps the self-extension promise in docs/17 intact.
- A C-backed module is bounded by its fixed buffers and by the C ABI: paths and
  captured output are capped at 8192 bytes, formatted time at 256.
- Errors are reported as empty strings, `false`, or `-1` rather than Zexi
  `Result` values. Wrapping them in `Result` is a small Zexi-level change when a
  program wants richer errors; the C stays simple.
- `use time;` now loads this module. `sys` is still reached with `use sys;` or
  `import os;`.
