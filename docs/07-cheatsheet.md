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

struct Point { x: f64, y: f64 }  // move-only by default

#[copy]                          // opt into copy semantics
struct Vec2 { x: f64, y: f64 }

enum Shape { Circle(f64), Rect(f64, f64), Point }

impl Point {
    fn len_sq(&self) -> f64 { return self.x * self.x + self.y * self.y; }
    fn shift(&mut self, dx: f64, dy: f64) { self.x += dx; self.y += dy; }
    // No `self`: an associated function, called as `Point::new(x, y)`.
    fn new(x: f64, y: f64) -> Point { Point { x, y } }
}
```

## 7.3 Types

| | |
| --- | --- |
| integers | `i8 i16 i32 i64 isize` · `u8 u16 u32 u64 usize` |
| floats | `f32 f64` |
| other | `bool` · `char` · `str` (borrowed) · `String` (owning, heap) |
| compound | `[T; N]` fixed array · `&T` `&mut T` · `()` unit |

Integer literals are untyped until inference and default to `i32`. There are no
implicit numeric conversions — use `as`. (`+` on a string renders a number,
`bool`, or `char` as text; that is a formatting convenience, not a numeric
conversion.)

## 7.4 Literals

```zx
42  1_000  0xFF  0b1010  0o755     // integers
3.14  1e9  2.5e-3                  // floats
true  false                        // bool
'Z'  '\n'  '\u{1F600}'             // char
"hi\n"  "élan"  r"C:\path"         // str
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

// loops
while i < 5 { sum += i; i += 1; }

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
from `break`; `while`/`for` always evaluate to `()`. A block's value is its
trailing expression (no `;`).

## 7.8 `match`

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

```zx
match n {
    0 => "zero",
    x if x < 0 => "negative",
    x if x > 100 => "big",
    _ => "small",
}
```

The scrutinee may be an `enum`, an integer, a `bool`, or a `char`. Integers and
`char`s need a `_` arm; a `bool` may instead cover both `true` and `false`.
Duplicate literal arms are rejected (`E0001`).

`break`, `continue`, and `return` may be used directly as an arm body
(`_ => continue`). The comma after an arm is optional when the body ends in `}`
(`0 => { a() } _ => { b() }`).

## 7.9 Arrays

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

Values move by default; a moved-out value is a compile error if used again.
Buffers are freed exactly once at scope exit (including on early `return` /
`break` / `continue` and on reassignment) — no GC, no leaks. `#[copy]` types
copy instead of moving.

## 7.11 Printing

```zx
print("no newline");
println("with newline");
println(42);          // any printable value: ints, floats, bool, char, str, String
println("point:", p.x, p.y);   // several values, separated by a space
println();            // just a newline
```

## 7.12 Casts

```zx
let n = 65 as char;    // 'A'
let m = big as i32;    // truncating integer cast
let f = 3 as f64;
```

## 7.13 Not implemented yet (roadmap M1+)

Generics, traits, `mod`/`use`, `Vec`/`Box`, user-defined `Drop`, tuples,
borrow-lifetime (NLL) analysis, `Result`/`?`, and enum variants that own
resources. See [`05-roadmap.md`](05-roadmap.md) for the plan.
