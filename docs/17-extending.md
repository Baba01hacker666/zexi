# 17. Adding to the language without touching the compiler

The rule this document exists to support: **a new capability should be written in
Zexi source, in the library that needs it** — not as a new C file the compiler has
to know about, and not as a special case in the driver.

There are four rungs on the ladder. Take the lowest one that works.

| Rung | Tool | Use when |
| --- | --- | --- |
| 1 | a plain `.zx` module | the job can be done in Zexi (`math`, `text`, `fmt`, `html`, `json` are all pure Zexi) |
| 2 | `extern "C"` declaration + `#[link("name")]` | the operating system or a system library already does it (`libm`, `libc`, `libsqlite3`) |
| 3 | `c """ ... """` | you need a few lines of C: a syscall wrapper, a struct the ABI fixes, a vendored helper |
| 4 | `#[no_mangle]` | C needs to call back into Zexi |

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

An `extern "C"` declaration keeps its Zexi name as the C symbol, so the name you
declare is the name the linker looks for. Pointers, `str` (`const char *`), and
the fixed-width integers all map directly; `&mut [u8; N]` arrives as a pointer to
the generated array wrapper struct (see §17.3).

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
| `struct S` | `struct zx_S` |
| returning `()` | `void` |

## 17.4 What the standard library does

`sys` and `regex` carry their C in their own `.zx` files, so the compiler has no
embed for them any more:

```zx
// internal/driver/std/sys.zx
extern "C" fn zxs_time_ms() -> i64;

c """
int64_t zxs_time_ms(void) { ... }
"""
```

The larger runtimes (`net`, `thread`, `json`) are still separate C files because
they are thousands of lines and are shared by several modules; the mechanism
above is how they can move over, one module at a time, without a new compiler
feature.

## 17.5 Limits

* Inline C is C11 as the host `cc` accepts it; no C++, no Rust.
* The block has no type checking of its own — it is C. Errors come from `cc`,
  reported against the generated file.
* Static helpers are private to the translation unit; a symbol C needs from
  another C file must not be `static` (and the Zexi side must declare it
  `extern "C"` so a prototype exists).
* Inline C is emitted after the generated code, so it cannot be referenced by a
  `static` initialiser that runs earlier.
