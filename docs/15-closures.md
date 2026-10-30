# 15. Closures

A closure is an anonymous function written where a `fn(..)` value is expected.
It can read the bindings around it, which is the reason to have one at all:

```zx
fn main() -> i32 {
    let limit = 10;
    let under: fn(i32) -> bool = |n| n < limit;
    println(under(7));          // true
    println(under(70));         // false
    0
}
```

## 15.1 Syntax

<!-- fragment -->
```zx
|x| x + 1                 // one parameter, expression body
|a, b| a * b              // several parameters
|| 42                     // no parameters
|x: i64| x * 2            // annotated parameter
|n| {                     // block body: the trailing expression is the result
    let doubled = n * 2;
    doubled
}
```

The parameter list opens with `|` and closes with `|`; `||` is an empty list.
The body is either an expression or a block. A struct literal is allowed in the
body even where `{` would otherwise start a block, so `|x| Point { .. }` parses
as a struct literal.

## 15.2 The target type supplies the signature

A closure takes its parameter and return types from the position it is written
in, so one of these has to be known:

* a `let` annotation — `let f: fn(i32) -> i32 = |n| n + 1;`
* a function parameter — `apply(|n| n + 1, 41)` where `apply` takes
  `fn(i32) -> i32`
* a container method — `v.each(|x| println(x))` where the element type is `T`
* a struct field initialiser whose field type is a `fn`

Without one, the checker reports `E0282` and tells you to annotate. This is
deliberate: guessing would make inferred parameter types depend on the order
the checker happens to visit expressions.

Parameters may be annotated explicitly. If an annotation disagrees with the
target type, that is `E0308`; a different parameter count is `E0057`.

## 15.3 Captures are by value

A closure copies what it reads into its environment. That has one consequence
worth knowing:

```zx
fn main() -> i32 {
    let mut base = 100;
    let f: fn(i32) -> i32 = |n| n + base;
    base = 5;                // does not change `f`: it captured a copy
    println(f(1));           // 101
    println(base);           // 5
    0
}
```

`Copy` values (`i32`, `f64`, `bool`, `char`, structs of those, ...) are copied,
so the original stays usable. Owning values (`String`, `Vec<T>`, `HashMap`, and
structs holding one) **move** into the environment, exactly as `let g = s;`
would: the enclosing binding is moved-from, and using it afterwards is `E0382`.

```zx
fn main() -> i32 {
    let suffix = String::from("!");
    let shout: fn(i32) -> String = |n| {
        let mut s = String::from("n=");
        s += n;
        s += suffix.as_str();     // suffix moved into the closure
        s
    };
    println(shout(9).as_str());   // n=9!
    // println(suffix.as_str());  // E0382: use of moved value
    0
}
```

Closures nest. An inner closure that reads a name from further out makes every
enclosing closure carry that value too, so the chain of environments is always
complete:

```zx
fn main() -> i32 {
    let step = 2;
    let outer: fn(i32) -> i32 = |n| {
        let inner: fn(i32) -> i32 = |m| m * step + n;
        inner(10)
    };
    println(outer(3));       // 23
    0
}
```

## 15.4 Closures outlive the call

An environment is heap-allocated when a closure captures something, so a
closure can be returned:

```zx
fn adder(n: i32) -> fn(i32) -> i32 {
    return |x| x + n;
}

fn main() -> i32 {
    let add7 = adder(7);
    println(add7(35));       // 42
    0
}
```

A closure that captures nothing needs no allocation at all: every value of it
points at one shared record.

## 15.5 What a function value is

`fn(A) -> B` is a single pointer to a closure record:

```c
typedef struct { long rc; void *fn; } zx_Closure;   // followed by the captures
```

* A named function used as a value points at a static record: `passing
  `double` allocates nothing and costs one indirection at the call.
* A capturing closure points at a heap record holding its copies.
* Calling through the value passes the record itself as an extra first
  argument, so the callee always has its environment.

Two consequences:

1. Function values keep working where they always did — `let g = pick(true);`
   and `apply(double, 21)` behave exactly as before.
2. Environments are reference-counted but not yet released: a closure created
   in a hot loop allocates and the memory is reclaimed at exit. Releasing them
   needs `Drop` for function values, which is the next step (see
   [§5 Roadmap](05-roadmap.md)).

## 15.6 Container methods that take closures

`Vec<T>` has four, all of which pass each element by copy (so `T` must be
`Copy`; iterating owning elements needs borrows, which the language does not
have yet):

```zx
fn main() -> i32 {
    let mut v: Vec<i64> = Vec::new();
    v.push(1);
    v.push(4);
    v.push(9);

    v.each(|x| println(x));                  // 1 4 9
    println(v.count(|x| x % 2 == 0));        // 1
    println(v.any(|x| x > 8));               // true
    println(v.all(|x| x >= 0));              // true
    println(v.fold(0, |acc, x| acc + x));    // 14
    0
}
```

| method | signature | result |
| --- | --- | --- |
| `each` | `fn(T) -> ()` | `()` |
| `count` | `fn(T) -> bool` | `usize`, how many matched |
| `any` | `fn(T) -> bool` | `bool`, true if one matched |
| `all` | `fn(T) -> bool` | `bool`, true if every element matched |
| `fold` | `T`, `fn(T, T) -> T` | `T`, the accumulator |

## 15.7 Limits (today)

* No borrows in a closure: captures are copies, never `&T`/`&mut T`.
* No mutation of the enclosing scope. Assigning to a captured name writes the
  closure's own copy, which is why `let` bindings stay immutable there.
* Closures cannot `break` or `continue` a loop outside themselves; the body is
  its own function.
* There is no `move` keyword: owning captures always move.
* Environments are not yet freed (see §15.5).
