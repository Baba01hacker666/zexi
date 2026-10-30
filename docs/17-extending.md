# 17. Adding to the language without touching the compiler

The rule this document exists to support: **a new capability should be written in
Zexi source, in the library that needs it** — not as a new C file the compiler has
to know about, and not as a special case in the driver.

There are five rungs on the ladder. Take the lowest one that works.

| Rung | Tool | Use when |
| --- | --- | --- |
| 1 | a plain `.zx` module | the job can be done in Zexi (`math`, `text`, `fmt`, `html`, `json` are all pure Zexi) |
| 2 | `extern "C"` declaration + `#[link("name")]` | the operating system or a system library already does it (`libm`, `libc`, `libsqlite3`) |
| 3 | `c """ ... """` | you need a few lines of C: a syscall wrapper, a struct the ABI fixes, a vendored helper |
| 4 | `#[no_mangle]` | C needs to call back into Zexi |
| 5 | `c """ ... """` as a **function body** | one function is the hot path, and you want it written in C without an FFI boundary at all |

Everything below compiles with plain `zexic run file.zx`. Nothing requires a flag,
a build file, or a change to this repository.

## 17.1 Rung 2: call a system library

Declare the function, name the library:

<!-- check-only -->
```zx
#[link("m")]
extern "C" fn sqrt(x: f64) -> f64;

fn main() -> i32 {
    println(sqrt(144.0));      // 12
    0
}
```

`#[link("m")]` adds `-lm` to the link step. The name is the library as the linker
spells it: `"m"`, `"pthread"`, `"sqlite3"`, `"ssl"`. `#[link("pthread")]` also
turns on `-pthread`, so the thread runtime links the same way a `use thread;`
program does.

Two more attributes extend the same idea:

- `#[link_dir("path")]` adds `-Lpath` to the link step, so a library that lives
  outside the system search path (a vendored `.a`, a build tree) is found
  without a custom build script.
- `#[include("header.h")]` puts `#include "header.h"` at the top of the
  generated C, before the program's own definitions. Use it to bring in a real
  header (types, macros, prototypes) instead of re-declaring them by hand. The
  header is searched along the usual `-I` paths, so pair it with the matching
  `#[link_dir]` or pass `-I` to the compiler.

An `extern "C"` declaration keeps its Zexi name as the C symbol, so the name you
declare is the name the linker looks for. Pointers, `str` (`const char *`), and
the fixed-width integers all map directly; `&mut [u8; N]` arrives as a pointer to
the generated array wrapper struct (see §17.3). A slice (`&[u8]`, `&mut [u8]`,
`[T]`) arrives as the two-word `zx_slice_T` value `{ T *ptr; size_t len; }`,
which is passed by value like any other struct — no length argument is needed.

## 17.2 Rung 3: carry your own C

A `c` item is a block of C compiled with the program:

<!-- check-only -->
```zx
extern "C" fn zx_gcd(a: i64, b: i64) -> i64;

c """
int64_t zx_gcd(int64_t a, int64_t b) {
    while (b != 0) { int64_t t = a % b; a = b; b = t; }
    return a;
}
"""

fn main() -> i32 {
    println(zx_gcd(84, 36));   // 12
    0
}
```

`c` and `c_source` are the same item. The body must be a raw string — `"""` or
`r#"..."#` — because C is full of quotes and backslashes that must stay as
written. `"""` is the convenient spelling: it may contain `"`, `\`, and newlines.

Three properties make this more useful than an opaque FFI boundary:

1. **Same translation unit.** The text is appended to the generated C, after the
   program's own definitions. Inline C therefore sees everything the compiler
   emits: `zx_String`, the `Vec`/`HashMap` helpers, every `#[no_mangle]`
   function, and every generated struct — including the ones a Zexi `struct`
   declares (`struct zx_RegexSpan`, `struct zx_arr4096_u8`, ...).
2. **It is checked.** Because the definitions and the `extern "C"` prototypes end
   up in one file, a signature that does not match is a C error at build time
   instead of undefined behaviour at run time. Converting the `sys` module to this
   form is exactly how a latent mismatch (`char *` versus the generated
   `struct zx_arr4096_u8 *`) was found.
3. **It travels with the module.** A library file that imports nothing can still
   bring native code, and every program that imports the library gets it — one
   copy, because identical blocks are deduplicated.

A library that ships its own C, in full:

```zx
// lib/fastmath.zx
pub extern "C" fn fm_cube(n: i64) -> i64;

c """
int64_t fm_cube(int64_t n) { return n * n * n; }
"""
```

<!-- fragment -->
```zx
// main.zx
use fastmath;

fn main() -> i32 {
    println(fastmath::fm_cube(3));   // 27
    0
}
```

If you are binding a library someone has already bound, you do not have to write
any of this: `zexic add <name>` installs a finished module (`sqlite`, `zlib`,
`crypto`, `curl`, `microhttpd`) into your project's `lib/`. A package is exactly
the `.zx` file described above, so the catalog is a shortcut, not a different
mechanism — see [`19-modules.md`](19-modules.md).

## 17.3 Rung 4: let C call back into Zexi

`#[no_mangle]` gives a function external linkage under its Zexi name, so inline C
(or a C file you pass on the command line) can call it:

<!-- check-only -->
```zx
#[no_mangle]
fn zx_add_one(n: i64) -> i64 {
    return n + 1;
}

extern "C" fn zx_twice_plus(n: i64) -> i64;

c """
int64_t zx_twice_plus(int64_t n) { return zx_add_one(n) * 2; }
"""

fn main() -> i32 {
    println(zx_twice_plus(20));   // 42
    0
}
```

### Type mapping

| Zexi | C |
| --- | --- |
| `i8`/`u8`/`i16`/…/`u64` | `int8_t`/`uint8_t`/…/`uint64_t` |
| `i32`/`i64` (default ints) | `int32_t`/`int64_t` |
| `f32`/`f64` | `float`/`double` |
| `bool` | `bool` |
| `str` | `const char *` |
| `String` | `zx_String` (`{ char *ptr; size_t len; size_t cap; }`) |
| `&T` / `&mut T` | `zx_T *` |
| `&mut [u8; N]` | `struct zx_arrN_u8 *` (bytes are `buf->e`) |
| `&[u8]` / `&mut [u8]` / `[T]` | `zx_slice_u8` / `zx_slice_T` (`{ T *ptr; size_t len; }`, by value) |
| `struct S` | `struct zx_S` |
| returning `()` | `void` |
| `*T` | `T *` |

## 17.3b Rung 5: a whole function body in C

Rung 3 adds C *next to* Zexi: an `extern "C"` declaration on one side of a call
and a C definition on the other. Rung 5 removes the boundary. Write the body
itself in C, and the parameters appear in the body under their own names, with
the C types the language gives them:

```zx
fn dot(a: &[f32], b: &[f32]) -> f32 {
    c """
        float sum = 0.0f;
        for (size_t i = 0; i < a.len; i++) sum += a.ptr[i] * b.ptr[i];
        return sum;
    """
}
```

```
fn fib(n: i32) -> i64 {
    c """
        int64_t a = 0, b = 1;
        for (int32_t i = 0; i < n; i++) { int64_t t = a + b; a = b; b = t; }
        return a;
    """
}
```

```
fn bump(p: &mut i64) -> i64 {
    c """
        *p += 1;
        return *p;
    """
}

fn main() {
    let mut n: i64 = 41;
    println(bump(&mut n));   // 42
    println(n);              // 42: the C body wrote through the borrow
}
```

```
42
42
```

This is the rung that only works because of the [single translation
unit](18-one-translation-unit.md): the C is compiled in the same file as the
generated code, so

* **the ABI is checked, not assumed** — the C body is type-checked against the
  exact typedefs the compiler emits for those parameters, so a signature drift
  is a `cc` error rather than undefined behaviour;
* **there is no call** — `cc` sees caller and callee together and inlines the
  body, so a C body costs nothing at the boundary;
* **the parameter names are the source names** — no marshalling shim, no
  `extern` declaration to keep in sync, no second file;
* **the body can call back into Zexi** through a `#[no_mangle]` function, since
  both are in that same unit:

```zx
#[no_mangle]
fn zx_triple(n: i64) -> i64 {
    n * 3
}

fn triple_sum(n: i64) -> i64 {
    c """
        int64_t total = 0;
        for (int64_t i = 1; i <= n; i++) total += zx_triple(i);
        return total;
    """
}

fn main() {
    println(triple_sum(4));   // 3 + 6 + 9 + 12
}
```

```
30
```

A generic function is emitted once per instantiation, so `sizeof` inside the
body sees the concrete type:

```zx
fn bytes<T>(v: T) -> i64 {
    c """
        return (int64_t)sizeof(v);
    """
}

fn main() {
    println(bytes(1 as i32), bytes(1 as i64));   // 4 8
}
```

```
4 8
```

The mapping is the same table as rung 3 (see "Type mapping" above): `&[T]` is
`{ T *ptr; size_t len; }` passed by value, `&mut T` is `T *`, `str` is
`const char *`, `char` is `uint32_t`, and a method's receiver is `self` — a
pointer to the struct for `&self`, so `self->x` is how you read a field.

Four rules keep this safe, and each is a diagnostic rather than a surprise:

| Rule | Why |
| --- | --- |
| It must be the **entire** body (E0540) | nothing else in the function can run, so allowing statements around it would silently drop them |
| Parameters and the result **may not need a destructor** (E0540) | `String`, `Vec<T>`, and owning structs would leak: the generated drop glue is what normally runs |
| Parameter names must be **C identifiers** (E0540) | the body refers to them by those names, so `int` or `total count` cannot survive into C |
| `main` cannot have one (E0540) | the runtime sets up `argc`/`argv` and the exit code there |

`&String`, `&[u8]`, `&mut [u8; N]`, `str`, and scalars are all fine as
parameters — they are borrowed or `Copy`, so there is nothing to drop. The
`--emit=mir` dump reports the function as not lowered (`a function body written
in C`), which is honest: the MIR interpreter cannot run C.

A `*T` argument is how a C function writes back through a borrow. Zexi has no
`unsafe` block and no pointer dereference, so a pointer is made explicitly with
`as` and only ever handed to C:

```zx
extern "C" fn zx_bzero(p: *u8, n: usize);

c """
void zx_bzero(uint8_t *p, size_t n) { for (size_t i = 0; i < n; i++) p[i] = 0; }
"""

fn main() {
    let mut buf: [u8; 8] = [65; 8];
    zx_bzero(&mut buf as *u8, 4);    // an array borrow decays to its first byte
    println(buf[0] as i32, buf[3] as i32, buf[4] as char);
}
```

```
0 0 A
```

The pointee must match, so the C side receives the type the language promised:
`&mut buf as *u8` is allowed, `&n as *u8` (where `n: i64`) is `E0308`, and a
slice borrow is refused with a message pointing at `&xs[i] as *T`. A pointer can
also be observed as an integer and converted back, which is how a C library's
opaque handle (returned as `usize`/`i64`) becomes a typed pointer again.

## 17.4 What the standard library does

Every standard module that needs C now carries it in its own `.zx` file, so the
compiler holds no C for the library at all:

| module | C it ships | why it needs C |
| --- | --- | --- |
| `sys` | POSIX environment, paths, clocks | `getenv`, `getcwd`, `clock_gettime` |
| `regex` | a small backtracking matcher | pointer-level scanning |
| `json` | `strtod`/`strtoll`, `%.17g` printing, UTF-8 encoding | exact number round-trips |
| `net` | sockets, HTTP plumbing, file I/O | the socket API |
| `http` | router slots, request counters, stop flag | typed `Router` storage and C11 atomics |
| `thread` | threads, mutexes, channels, worker pool | `pthread` |

```zx
// internal/driver/std/sys.zx
extern "C" fn zxs_time_ms() -> i64;

c """
int64_t zxs_time_ms(void) { ... }
"""
```

Because the block is compiled with the generated code, `http` stores real
`zx_Router` values in its pinned slots — no `void *`, no guessed byte size:

```c
_Alignas(64) static zx_Router zxh_mux_extra[ZXH_MUX_SLOTS];
zx_Router *zxh_mux_slot(int32_t slot) { return &zxh_mux_extra[slot]; }
```

Two rules follow from that shared translation unit:

* a block must be self-contained — include the headers it uses, because nothing
  in front of it provides them;
* a block is compiled after the generated code, so it can call it freely but not
  the other way round, except through `#[no_mangle]` functions declared
  `extern "C"` on the Zexi side.

Moving C out of the driver also *fixed* real bugs: `sys` and `json` had `char *`
parameters where the language passes `struct zx_arrN_u8 *`, and `net` declared
`void *` returns for what the language types as `*Router`. A separate C file
never saw the prototypes and compiled anyway; a block in the generated unit sees
them and refuses.

## 17.5 Limits

* Inline C is C11 as the host `cc` accepts it; no C++, no Rust.
* The block has no type checking of its own — it is C. Errors come from `cc`,
  reported against the generated file.
* Static helpers are private to the translation unit; a symbol C needs from
  another C file must not be `static` (and the Zexi side must declare it
  `extern "C"` so a prototype exists).
* Inline C is emitted after the generated code, so it cannot be referenced by a
  `static` initialiser that runs earlier.
