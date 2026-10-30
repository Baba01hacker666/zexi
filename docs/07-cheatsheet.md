# 7. Zexi Cheat-Sheet

A one-page tour of the language as it exists today (M0). Every snippet below is
a construct that compiles; the live, runnable version is
[`tests/syntax.zx`](../tests/syntax.zx).

## 7.1 A complete program

```zx
fn main() -> i32 {
    println("hello, zexi");
    0            // the trailing expression is the return value
}
```

`//` line comments, `/* ... */` block comments (nesting), `///` doc comments.

## 7.2 Declarations

```zx
const MAX: i32 = 100;
const GREETING = "hello";        // type inferred

type Meters = i64;               // a `type` alias is the same type everywhere
type Distance = Meters;          // aliases may chain
type Scores = [i32; 2];          // ... and may name compound types

struct Point { x: f64, y: f64 }  // move-only by default
struct Pair(i32, i32);           // tuple struct: `Pair(3, 4)`, fields `.0`/`.1`

#[copy]                          // opt into copy semantics
struct Vec2 { x: f64, y: f64 }

#[repr(C)]                       // C layout — accepted, and already the default
struct Handle { fd: i32 }

enum Shape { Circle(f64), Rect(f64, f64), Point }

impl Point {
    fn len_sq(&self) -> f64 { return self.x * self.x + self.y * self.y; }
    fn shift(&mut self, dx: f64, dy: f64) { self.x += dx; self.y += dy; }
    // No `self`: an associated function, called as `Point::new(x, y)`.
    fn new(x: f64, y: f64) -> Point { Point { x, y } }
}
```

### Attributes

```zx
#[copy]                          // struct/enum: copy instead of move
struct Vec2 { x: f64, y: f64 }

#[repr(C)]                       // struct: C layout — accepted, already the default
struct Handle { fd: i32 }

#[export]                        // fn: keep the Zexi name, external linkage
fn my_add(a: i64, b: i64) -> i64 { return a + b; }

#[no_mangle]                     // same as `#[export]`
fn also_visible(n: i64) -> i64 { return n; }

#[inline]                        // fn: hint to the C compiler
fn tiny(x: i64) -> i64 { return x + 1; }

#[inline(never)]                 // fn: do not inline
fn opaque(x: i64) -> i64 { return x; }
```

An attribute the compiler does not recognize is **E0539**, so a typo such as
`#[Copy]` is reported instead of silently doing nothing. `#[repr(...)]` accepts
only `C`.

`#[export]` and `#[no_mangle]` are the C-bridge entry points: the function keeps
its Zexi name as its C symbol with external linkage, and it is emitted even when
nothing in Zexi calls it. Paired with `extern "C"` declarations for the other
direction, C and Zexi can call each other both ways:

<!-- check-only -->
```zx
#[export]
fn zexi_add(a: i64, b: i64) -> i64 { return a + b; }

extern "C" fn run_c();           // defined in the .c file

fn main() -> i32 { run_c(); 0 }
```

```c
/* glue.c, built with `zexic build main.zx --csrc=glue.c` */
#include <stdint.h>
int64_t zexi_add(int64_t a, int64_t b);
void run_c(void) { zexi_add(20, 22); }
```

In multi-unit builds (`--jobs=N`) `#[inline]` is dropped rather than emitted,
because a bare C `inline` definition provides no external symbol for the other
translation units to link against.

## 7.3 Types

| | |
| --- | --- |
| integers | `i8 i16 i32 i64 isize` · `u8 u16 u32 u64 usize` |
| floats | `f32 f64` |
| other | `bool` · `char` · `str` (borrowed) · `String` (owning, heap) |
| compound | `[T; N]` fixed array · `&T` `&mut T` · `()` unit |

Integer literals are untyped until inference and default to `i32`; they also
adopt a float context (`let x: f64 = 1`, `1 + 2.5`). Otherwise there are no
implicit numeric conversions — use `as`. (`+` on a string renders a number,
`bool`, or `char` as text; that is a formatting convenience, not a numeric
conversion.)

## 7.4 Literals

<!-- fragment -->
```zx
42  1_000  0xFF  0b1010  0o755     // integers
3.14  1e9  2.5e-3                  // floats
true  false                        // bool
'Z'  '\n'  '\u{1F600}'             // char (exactly one character)
"hi\n"  "élan"  r"C:\path"         // str; raw strings take backslashes literally
r#"say "hi""#  r##"a #" b"##       // raw strings with `#`s may contain quotes
```

## 7.5 Bindings

```zx
let a = 40;          // immutable, type inferred
let mut b: i64 = 2;  // mutable, annotated
b += 3;
```

A non-`mut` binding cannot be assigned to — not directly, not through a field
or index (`a.x = 1`, `xs[0] = 1`), not by taking `&mut`, and not by calling a
`&mut self` method (E0596).

## 7.6 Operators

```
+ - * / %          == != < <= > >=      && || !
& | ^ << >>        = += -= *= /= %= &= |= ^= <<= >>=
```

Precedence (tightest first): unary `- ! & *` → `as` → `* / %` → `+ -` →
`<< >>` → `&` → `^` → `|` → `.. ..=` → comparisons → `&&` → `||` → `=`.

## 7.7 Control flow

<!-- fragment -->
```zx
// if / else if / else — as a statement ...
if n < 0 {
    return "negative";
} else if n == 0 {
    return "zero";
} else {
    return "positive";
}

// ... or as an expression that yields a value
let s = if n < 0 { -1 } else if n == 0 { 0 } else { 1 };

// `elif` is `else if` without the extra keyword
if n < 0 {
    return "negative";
} elif n == 0 {
    return "zero";
} else {
    return "positive";
}

// loops
while i < 5 { sum += i; i += 1; }
until i >= 5 { sum += i; i += 1; }   // `until` loops while the condition is false

let mut n = 0;
loop { n += 1; if n == 3 { break; } }

// a `loop` may produce a value with `break <expr>`
let found = loop {
    n += 1;
    if n * n > 50 { break n; }
};

for k in 1..=4 { acc += k; }   // inclusive range; 1..4 is exclusive
```

`return`, `break`, and `continue` work in every loop. Only `loop` yields a value
from `break`; `while`/`until`/`for` always evaluate to `()`. A `loop` with no
`break` at all has the never type, so `fn f() -> i32 { loop { return 1; } }` is
fine. A block's value is its trailing expression (no `;`).

`if let` runs a block when a pattern matches, and `while let` loops while it
keeps matching (the scrutinee is re-evaluated each iteration). Both are sugar
over `match`, and take the same patterns:

<!-- fragment -->
```zx
if let Shape::Circle(r) = shape {
    println(r);
} else {
    println("not a circle");
}

while let Step::More(v) = next(i) {
    total += v;
    i += 1;
}
```

`defer expr;` schedules an expression to run when the enclosing *block* scope
exits — by falling off the end or via `return`/`break`/`continue`. Defers in
one scope run last-in-first-out, are evaluated at exit time (so they see the
locals' final values), and run before that scope's destructors:

```zx
let mut s = String::from("x");
defer println(s);   // prints "xy": it runs at scope exit
s.push_str("y");
```

## 7.8 `match`

<!-- fragment -->
```zx
match d {
    Direction::North => Direction::East,
    Direction::South => "S",
    Message::Move { x, y } => x + y,   // struct variant: bind fields
    Shape::Circle(r) => r,             // tuple variant: bind payload
    _ => 0,                            // wildcard; matches must be exhaustive
}
```

An arm may carry a guard, `pat if cond => body`. When the pattern matches but
the guard fails, the following arms are tried, so a variant may appear more than
once. A guarded arm does not count towards exhaustiveness.

<!-- fragment -->
```zx
match n {
    0 => "zero",
    x if x < 0 => "negative",
    x if x > 100 => "big",
    _ => "small",
}
```

The scrutinee may be an `enum`, an integer, a `bool`, a `char`, or a `str`. Integers and
`char`s need a `_` arm; a `bool` may instead cover both `true` and `false`.
Negative literal patterns are allowed (`-1 => ..`), out-of-range patterns are
rejected (`E0080`), and duplicate literal arms are rejected (`E0001`).

Alternatives may be combined with `|` (or-patterns) and ranges. Or-pattern
alternatives may not bind variables; a range must not be empty (`5..=1` is
`E0080`), and ranges do not by themselves make a match exhaustive:

<!-- fragment -->
```zx
match n {
    0 => "zero",
    1 | 2 | 3 => "small",
    4..=6 => "mid",
    7..10 => "high",
    _ => "huge",
}
match c {
    'a'..='z' => "lower",
    'A'..='Z' => "upper",
    _ => "other",
}
```

A binding may be declared mutable inside a pattern: `Shape::Circle(mut r)`.
Pattern bindings in a `let` use `let mut` as usual.

Strings match too (compared by value, needs a `_` arm) — handy for
command dispatch; match a `String` via `.as_str()`:

```zx
match arg_at(1).as_str() {
    "quit" | "exit" => 0,
    "help" => 1,
    _ => 2,
}
```

`break`, `continue`, and `return` may be used directly as an arm body
(`_ => continue`). The comma after an arm is optional when the body ends in `}`
(`0 => { a() } _ => { b() }`).

## 7.9 Arrays

<!-- fragment -->
```zx
let xs: [i32; 3] = [10, 20, 30];
let zeros = [0; 4];        // repeat form
println(len(xs));          // 3
println(xs.len());         // 3    (method form)
println(xs[1]);            // 20, bounds-checked at run time
xs[0] = 99;                // needs `let mut xs`
for x in xs {              // iterate the elements (element type must be `Copy`)
    println(x);
}
```

## 7.10 `String` and ownership

```zx
let mut s = String::from("zexi");  // owning, heap-allocated, not `Copy`
s.push_str("!");
s.push('?');
s += " done";          // `+=` appends
println(s);            // zexi!? done
println(s.len());      // 11
let greeting = "hi, " + s + "!";   // `+` concatenates any mix of `str`/`String`
let report = "lines: " + 12 + " (" + true + ")";  // numbers/bool/char are rendered
s += 3;                // `+=` appends; a number is rendered as text
let t = s.clone();     // deep copy
let u = s;             // move: `s` is unusable afterwards (E0382)
```

A `String` may be passed wherever a borrowed `str` is expected (for example a
function parameter, `push_str`, or `==`), so `.as_str()` is usually unnecessary.
When a number, `bool`, or `char` is concatenated with `+` (or `+=`) it is
formatted into text first, which removes the need for a manual `int_to_str`.

All six comparison operators work on any mix of `str` and `String` and compare
**bytes**, so `"Z" < "a"` is `true` — the same order Rust's `Ord` gives `str`.
`len()` and the string operations are byte-based, not character-based.
Appends (`push`, `push_byte`, `push_str`) grow the buffer geometrically, so
building a string in a loop is amortized O(1) per byte, and `clear()` keeps the
buffer for reuse.

Values move by default; a moved-out value is a compile error if used again.
Buffers are freed exactly once at scope exit (including on early `return` /
`break` / `continue` and on reassignment) — no GC, no leaks. `#[copy]` types
copy instead of moving.

### `Vec<T>`: the growable array

```zx
let mut v: Vec<i64> = Vec::new();     // or Vec::with_capacity(100)
v.push(10);
v.push(20);
v.insert(1, 15);                      // shift-insert at an index
println(v.len(), v.is_empty());       // 3 false
println(v[0], v[1], v[2]);            // 10 15 20

let last: Option<i64> = v.pop();      // Some(20), or None when empty
let mid: i64 = v.remove(0);           // shift-remove, returns the element
v.swap_remove(0);                     // O(1) remove, order not preserved
v.truncate(1);                        // drop everything past index 1
v.reserve(64);                        // grow the capacity up front
v.clear();
```

`Vec<T>` owns its buffer and is move-only, like `String`: elements are dropped
when the `Vec` goes out of scope, is reassigned, or is truncated. Indexing is
bounds-checked and panics rather than reading out of bounds.

Two operations need a `Copy` element type, because there is no element-wise
clone trait yet:

- `clone` on a `Vec<String>` is E0277 — build a new `Vec` and push each element.
- `for x in v` borrows the elements, so it needs `Copy` too; iterating a
  `Vec<String>` is E0507. Index with `v[i]` instead.

Nested (`Vec<Vec<i64>>`) and struct-holding (`Vec<Item>`) vectors work, and a
struct may hold a `Vec` field. See `tests/vec.zx` for a full tour.

## 7.11 Printing

<!-- fragment -->
```zx
print("no newline");
println("with newline");
println(42);          // any printable value: ints, floats, bool, char, str, String
println("point:", p.x, p.y);   // several values, separated by a space
println();            // just a newline
```

## 7.12 Casts

<!-- fragment -->
```zx
let n = 65 as char;    // 'A'
let m = big as i32;    // truncating integer cast
let f = 3 as f64;
```

## 7.13 Generics, `Option`, and `Result`

Generic functions and types are monomorphized: each distinct set of type
arguments becomes its own function/type. Parameters are inferred from the
arguments and the expected type, or given explicitly with a turbofish. Type
parameters are unconstrained for now (no trait bounds).

```zx
fn id<T>(x: T) -> T { x }

struct Pair<A, B> { first: A, second: B }

impl<A, B> Pair<A, B> {
    fn swap(&self) -> Pair<B, A> {
        Pair { first: self.second, second: self.first }
    }
}

enum Maybe<T> { Just(T), Nothing }

fn main() -> i32 {
    let p = Pair { first: 1, second: "one" };
    let q = p.swap();               // Pair<str, i32>
    println(q.first);               // one
    println(id::<i64>(7));
    0
}
```

`Option<T>` and `Result<T, E>` are predeclared generic enums
(`Some(T)`/`None`, `Ok(T)`/`Err(E)`), so they need no import. Bare variant
names take their enum from context:

```zx
fn half(x: i64) -> Option<i64> {
    if x % 2 == 0 { return Some(x / 2); }
    return None;
}
```

`?` is ordinary match-based control flow: it yields the `Ok`/`Some` payload and
returns the `Err`/`None` case from the enclosing function. That function must
return the matching enum, and the error type must match.

```zx
fn parse(s: str) -> Result<i64, str> {
    if s == "" { return Err("empty"); }
    return Ok(42);
}

fn double(s: str) -> Result<i64, str> {
    let n = parse(s)?;              // early-return on Err
    return Ok(n * 2);
}

fn main() -> i32 {
    match double("x") {
        Ok(n) => println(n),
        Err(e) => println(e),
    }
    0
}
```

A by-value `match` over an enum that owns memory consumes the scrutinee; bound
payloads move into the arm and are dropped once. A guarded arm copies for the
guard and takes ownership only after it passes. Binding an owned payload out of
a borrow is `E0507`.

## 7.14 Closures

```zx
fn adder(n: i32) -> fn(i32) -> i32 { return |x| x + n; }   // escapes

fn main() -> i32 {
    let base = 100;
    let shifted: fn(i32) -> i32 = |n| n + base; // captures `base` by copy
    println(shifted(1));                        // 101
    let add7 = adder(7);
    println(add7(35));                          // 42

    let mut v: Vec<i64> = Vec::new();
    v.push(4);
    v.push(9);
    v.each(|x| println(x));                     // 4 9
    println(v.count(|x| x > 5));                // 2
    println(v.fold(0, |acc, x| acc + x));       // 13
    0
}
```

The target type supplies the signature, so annotate the binding when nothing
else fixes it. `Copy` values are captured by copy; `String`/`Vec`/`HashMap`
move into the closure (using the original afterwards is `E0382`). See
[`15-closures.md`](15-closures.md).

## 7.15 Writing it the easy way

Zexi borrows Python's ergonomics where they cost nothing at run time:

```zx
#!/usr/bin/env zexi          # `#` comments work (so do `//` and `/* */`);
// Semicolons are optional: a newline ends a statement. Keep them if you like.
let a = 1
let b = 2;

let name = "world"
println(f"hello, {name}! 1 + 2 = {1 + 2}")   // interpolation, type String

fn area(w: i64, h: i64 = 10) -> i64 { w * h }  // default argument
area(5)              // 50
area(5, h = 3)       // 15 — named (keyword) arguments, any order
area(h = 4, w = 2)   // 8

fn greet() { println("hi") }   // no return type needed when returning ()
```

- Defaults must be constant expressions (`= 10`, `= 20 + 22`, `= SOME_CONST`),
  and only trailing parameters may have them.
- In `f"..."`, `{{` and `}}` are literal braces. Interpolated values are
  borrowed, never moved, and must be printable (numbers, `bool`, `char`,
  `str`, `String`).

Libraries: `use` and `import` load whole files into your program. Loaded
declarations are available unqualified (the preview keeps a flat namespace), and
each import also creates a qualifier so you can write path-qualified lookups.
`import` accepts Python-shaped dots as a friendly alternative to `use` paths
with `::`.

```zx
use "helpers.zx";           // a file next to yours (path relative to your file)
import "helpers.zx" as h;   // bind the module to a qualifier: h::triple(2)
use math;                   // standard library: local files win, then embedded std
use math as m;              // qualifier alias: m::maxi(1, 2)
use std::text;
import http.server;         // package-shaped facade; server::Router, server::serve(&r, p)
import http.server as srv;  // or srv::Router
import io.fs;               // filesystem facade: fs::read("f")
import os;                  // environment, local paths, clocks, process helpers
import text.regex;          // POSIX ERE helpers: regex::is_match(pat, s)
```

`mod name;` loads `name.zx` or `name/mod.zx`; `mod name { ... }` works too.
Both create a `name::` qualifier, so `util::double(21)` and
`helper::add(1, 2)` resolve against that module's public declarations. Qualified
access checks `pub`: a private item is E0603, a missing one E0412, and using a
module as a value/type is E0433. Every file is loaded at most once and cycles
are harmless.

### Projects: compile just the entry

There is no manifest. Point the compiler at a file or a **directory**; for a
directory it looks for `main.zx`, then `src/main.zx`, and takes that directory
as the project root. Named imports are then searched next to the importing
file, in its `lib/`, at the project root, and in the root's `lib/` before the
standard library — so `src/main.zx` can `use util;` and `mod pkg;` from the
project root.

```zx
// project/util.zx          ->  use util;            ... util::double(21)
// project/pkg/a.zx         ->  use pkg;             ... pkg::a()
// project/pkg/b.zx             (pkg/ is loaded whole: every .zx file in it)
// project/src/main.zx      ->  zexic run .   (entry discovered automatically)
```

A directory used as a module loads **all** its `.zx` files as one package
(`main.zx` and `_`-prefixed files are skipped); its public names are reachable
through the package qualifier, and a private one is E0603. `--root=dir` pins
the project root explicitly.

Under the hood the merged program is still **flat**: unqualified names from a
plain import keep working, and `pub` is enforced only for *qualified* paths, so
the preview does not yet isolate same-named items from two modules. Passing
extra `.zx` files on the command line (`zexic run main.zx lib.zx`) still works
too.

The embedded standard library (sources in `internal/driver/std/`) includes:

- `math`: integer/bit/prime helpers plus `PI`, angle conversion, roots,
  exponentials/logarithms, `sin`/`cos`/inverse and hyperbolic trig, `pow`/
  `powf`, geometry, interpolation/smoothing, and conventional `min`/`max`/
  `abs` aliases. See [`10-stdlib-recipes.md`](10-stdlib-recipes.md) for
  domain and approximation behavior.
- `text`: ASCII casing, trim/pad/replace/search, comparisons, and simple
  formatting helpers; familiar `upper`, `lower`, `strip`, `contains`,
  `starts_with`, `ends_with`, `replace`, and `is_blank` aliases are included
  for small scripts.
- `regex`: opt-in POSIX-ERE validation/search/full-match/find/replace helpers
  via `import text.regex;`. It is intentionally not a PCRE engine.
- `fmt`: pure-Zexi formatting of numbers and values. Bases (`hex(n, upper)`,
  `radix(n, base)`, `hex_pad`), decimal grouping (`thousands`,
  `thousands_sep`), decimals and exponents (`fixed`, `sci`, `percent`), human
  units (`bytes`, `duration`), ordinals (`ordinal`), and padding (`pad_left`,
  `pad_right`, `pad_center`, each with an optional fill character). The older
  spellings (`hex_lower`, `fixed_point`, `byte_size`, `align_right`, …) remain
  as aliases. It has no C shim, so it costs nothing to import.
- `codec`: pure-Zexi encoders and checksums — hex (`hex_encode`,
  `hex_encode_upper`, `hex_decode`), base64 (`base64_encode`,
  `base64_encode_url`, `base64_decode`), and CRC-32/Adler-32/FNV-1a/djb2 with
  fixed-width `*_hex` forms.
- `sys`, `io`, `fs`, and `random`: `sys` is the POSIX substrate (errno and
  `IoError`, byte I/O, files, environment, clocks); `io` provides console I/O
  (`input(prompt = "")`, `read_line() -> Result<String, IoError>`, `eprintln`);
  `fs` provides capped, binary-safe `read`/`write(path, &[u8])`/`copy`
  (32 MiB cap) plus `exists`, `metadata`, and `remove`. Random numbers are
  deterministic or libc-backed.
- `net` and `http`: TCP and UDP sockets, name resolution, readiness polling, and
  a high-level HTTP client/server. `Client`, `get`, `do`, `fetch`, `request`,
  `post_form_url`, `Router`, `new_app`, the Go-style
  `handle_get`/`handle_post`/`use_before`/`use_after` + `listen(":8080")`
  default mux with `{name}` route parameters, `serve_local`, `serve_n`,
  `serve_dir`, and static-file helpers; `listen_workers`/`serve_workers*` serve
  with a pool of OS threads, and `requests_served()`/`stop_serving()` report and
  stop it; `udp_bind`/`send_to`/`recv_from`, `resolve`, `poll_read`,
  `accept_timeout`, and `cpu_count` sit underneath. Documented in
  [`08-networking.md`](08-networking.md).
- `thread`: OS threads (`Thread::spawn`/`spawn2`/`spawn_void`, `join`), blocking
  `i64` channels (`Chan`, `chan_send`/`chan_recv`/`chan_close`), lock-free
  `i64` atomics (`Atomic`, `atomic_add`/`get`/`set`), and a task pool
  (`Pool::new`, `submit`/`wait`/`result`, `map` over a range, `free`). Only
  `i64` values cross a thread boundary; see
  [`11-concurrency.md`](11-concurrency.md).
- `json`: parse, build, and print JSON with exact integers, round-tripping
  doubles, insertion-ordered objects, and errors that name a byte offset
  (`parse`, `stringify`, `pretty`, `Value`, `object`, `array`, `of_*`,
  `quote`, `valid`). See [`14-json-and-html.md`](14-json-and-html.md).
- `html`: safe server-rendered markup — `escape`/`attr`/`url_query`,
  `page`/`page_with`, `tag`/`heading`/`link`/`list`/`table`, `json_script`, and
  `template` with `{{name}}` (escaped) and `{{name|raw}}`. See
  [`14-json-and-html.md`](14-json-and-html.md).

See [`10-stdlib-recipes.md`](10-stdlib-recipes.md) for runnable local HTTP,
filesystem, OS, and regex patterns.

Command-line arguments are builtins: `arg_count()` counts them (index 0
is the program name) and `arg_at(i)` returns argument `i` as a `String`
(`""` when out of range) — see `examples/args.zx`. For a complete small
program that ties it all together, see `examples/guess.zx`.

## 7.16 Adding native code from a library

<!-- check-only -->
```zx
#[link("m")]
extern "C" fn sqrt(x: f64) -> f64;        // call into a system library

#[no_mangle]
fn zx_half(n: i64) -> i64 { return n / 2; }   // ... and let C call back

extern "C" fn zx_quarter(n: i64) -> i64;

c """
int64_t zx_quarter(int64_t n) { return zx_half(n); }
"""

fn main() -> i32 {
    println(sqrt(256.0));      // 16
    println(zx_quarter(10));   // 5
    0
}
```

`c """..."""` compiles C with the program, in the same translation unit, so it
can use `String`, the `Vec`/`HashMap` helpers, generated structs, and any
`#[no_mangle]` function. That is how a library adds a syscall wrapper or a
vendored helper without a compiler change: `sys` and `regex` ship their C this
way. See [`17-extending.md`](17-extending.md).

`#[link_dir("path")]` adds `-Lpath` and `#[include("header.h")]` emits
`#include "header.h"` at the top of the generated C, so a binding can be
written against a real header instead of re-declaring its types.

## 7.17 Not implemented yet (roadmap M1+)

Traits (and trait bounds), `dyn Trait`, associated types, const generics,
`Box`, user-defined `Drop`, closure environments are not freed yet, tuple values (tuple *structs* exist; `(a, b)` values
and destructuring do not yet), borrow-lifetime (NLL) analysis, bindings inside
or-patterns, and nested patterns. Pattern bindings in a `match` on a *borrowed*
enum still cannot move an owned payload out (E0507), which is why `json::Value`
is a tagged struct rather than an enum; `ref` bindings / match ergonomics are
the missing piece. Scoped modules have landed for
*qualified* access (imports, `as` aliases, `mod::item`, `pub` checks), but the
merged program is still one flat namespace: unqualified cross-module names
remain visible and ignore `pub`, so full isolation and package metadata are
still M1 work. See
[`05-roadmap.md`](05-roadmap.md) for the plan.
