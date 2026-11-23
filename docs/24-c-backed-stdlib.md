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

## 24.5 Why C, and what the limits are

- Every module carries its own C; none adds a driver-side C blob or a linker
  flag. This keeps the self-extension promise in docs/17 intact.
- A C-backed module is bounded by its fixed buffers and by the C ABI: paths and
  captured output are capped at 8192 bytes, formatted time at 256.
- Errors are reported as empty strings, `false`, or `-1` rather than Zexi
  `Result` values. Wrapping them in `Result` is a small Zexi-level change when a
  program wants richer errors; the C stays simple.
- `use time;` now loads this module. `sys` is still reached with `use sys;` or
  `import os;`.
