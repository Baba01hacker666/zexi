# 9. The C Bridge — using C libraries from Zexi

Zexi compiles to C, so the entire C ecosystem is available through a
small bridge with three parts: **declarations** (what the C functions
look like), **marshalling** (how values cross over), and **linking**
(how the C code joins your binary). `lib/http.zx` is a worked example:
BSD sockets are plain `extern "C"` calls under ordinary Zexi code.

## 1. Declarations

Declare C functions with `extern "C"`; the name is used verbatim:

```zx
extern "C" fn sqrt(x: f64) -> f64;
extern "C" fn puts(s: str) -> i32;
```

Or generate the declarations from the library's header:

```console
$ zexic bindgen /usr/include/math.h -o math_bindings.zx
```

`bindgen` translates the portable subset of C — scalars, sized integers,
`char *` strings, opaque pointers, array parameters — and lists anything
it cannot map (variadic functions, function pointers, by-value structs)
as comments at the top of the output, so you can finish those by hand.
Use the result like any library file: `use "math_bindings.zx";`.

## 2. Marshalling

| Zexi type | C type | Notes |
| --- | --- | --- |
| `i8`…`i64`, `u8`…`u64`, `isize`, `usize` | `int8_t`…`uint64_t`, `intptr_t`, `size_t` | exact width |
| `f32` / `f64` | `float` / `double` | |
| `bool` / `char` | `bool` / `uint32_t` | `char` is a Unicode scalar |
| `str` | `const char *` | string literals and `String` buffers are NUL-terminated |
| `String` | `zx_String` (internal) | pass to C as `str` or via `cstr(...)` |
| `*T` / `*mut T` | `T *` | raw pointers, for opaque C handles |
| `&T` / `&mut T` | `T *` | how `lib/http.zx` passes `sockaddr_in`; a reference may be passed wherever a raw pointer is expected (and vice versa), so `time(&mut slot)` just works |
| `struct` | C struct | field order matches; model C structs field-by-field like `SockAddrIn` in `lib/http.zx` |

Two builtins move strings across the boundary:

- `cstr(x: str | String) -> *u8` — **borrows** a NUL-terminated C string.
  The argument is never consumed. (If you pass a computed `String`
  temporary, its buffer is deliberately left allocated — like C's
  `strdup` — so the pointer stays valid; bind it to a variable first if
  that matters.)
- `from_cstr(p: *u8) -> String` — **copies** a C string into an owning
  Zexi `String`. You must guarantee `p` points at a valid NUL-terminated
  string.

## 3. Linking

The generated code is one C file; your C side joins at build time:

```console
$ zexic build app.zx --csrc=glue.c --csrc=more.c \
      --link=m --link=sqlite3 --lib-dir=/opt/lib --include=/opt/include
$ zexic run app.zx --csrc=glue.c --link=m
```

`--csrc` files are compiled and linked with the program (`--include`
adds header search paths for them), `--link` adds `-l` libraries, and
`--lib-dir` adds `-L` search paths. The same knobs exist in the driver
API as `driver.Options{CFiles, LinkLibs, LibDirs, IncludeDirs}`.

## Putting it together

See `examples/cbridge.zx` + `examples/cbridge_glue.c`:

```console
$ zexic run examples/cbridge.zx --csrc=examples/cbridge_glue.c --link=m
42
5
fast and small
3
1.5
```

## Safety

The bridge is deliberately thin: pointers are raw, `from_cstr` trusts
the pointer, and a Zexi struct only matches a C struct if you model the
fields (including padding) faithfully. The usual Zexi guarantees resume
on your side of the boundary — copy C strings into `String` early, keep
raw pointers close to the calls that need them, and wrap the result in
ordinary Zexi functions, exactly as `lib/http.zx` wraps sockets in a
`Conn` type.
