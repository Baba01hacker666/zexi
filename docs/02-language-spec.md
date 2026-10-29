# 2. Language Specification (draft v0.1)

This is the *normative-shaped* spec for the M0 language. Features tagged
**M1/M2** are designed here; the lightweight source loader described in §2.8
supports qualified, `pub`-checked module paths on top of a still-flat merged
program namespace.

## 2.1 Lexical structure

- **Encoding:** UTF-8 source; identifiers are ASCII for M0.
- **Line comments:** `// ...` and `# ...` (a `#` that opens an attribute,
  `#[...]`, is not a comment; this also makes `#!/usr/bin/env zexi`
  shebang lines work).
- **Block comments:** `/* ... */`, nesting.
- **Doc comments:** `///` (attached to the following item).
- **Statement termination:** a statement ends at `;` *or* at a newline.
  Two statements on one line still need `;` between them; an expression
  continues across a newline wherever an operator or opener follows, as
  before. Semicolons are therefore always safe and never required at a
  line end.
- **Identifiers:** `[A-Za-z_][A-Za-z0-9_]*`.
- **Types** are identifiers; there is no separate type namespace.

### Literals

| Form | Examples |
| --- | --- |
| Decimal int | `0`, `42`, `1_000` |
| Hex / binary / octal | `0xFF`, `0b1010`, `0o755` |
| Float | `3.14`, `1e9`, `2.5e-3` |
| Byte | `b'A'`, `b'\n'` |
| Char (a Unicode scalar) | `'A'`, `'\u{1F600}'` |
| String (UTF-8, immutable) | `"hi\n"`, `"élan"` |
| Interpolated string (`String`) | `f"hi {name}, {n + 1} pts"` (`{{`/`}}` are literal braces) |
| Raw string (no escapes) | `r"C:\path"`, `r#"say "hi""#`, `r##"a #" b"##` |
| Triple-quoted raw string | `"""a "quoted" line\nand a second one"""` — the spelling to use for C, HTML, or SQL |
| Boolean | `true`, `false` |
| Unit/void | `()` |

Integer literals are **untyped** until inference; they default to `i32` and may
also adopt a float context. There are no implicit numeric conversions (see 2.4).

### Keywords

```
as      break   const   continue  defer   else    elif    enum    extern
false   fn      for     if        impl    import  in      let     loop
match   mod     mut     pub       return  struct  true    type
unsafe  until   use     while   i8 i16 i32 i64 isize u8 u16 u32 u64 usize f32 f64
bool    char    str     never
```

### Operators & punctuation

```
+ - * / %            == != < <= > >=      && || !
& | ^ << >>          = += -= *= /= %= &= |= ^= <<= >>=
( ) { } [ ]          . :: , ; : -> =>     .. ..=   ? @ _ 
```

Precedence (tightest → loosest): unary (`- ! & *`) → `as` → `* / %` →
`+ -` → `<< >>` → `&` → `^` → `|` → `.. ..=` → comparisons → `&&` → `||` →
`=` (right-assoc). `?` binds tighter than unary postfix on an expression.

## 2.2 Grammar (EBNF, selected)

```ebnf
program     = { item } ;

item        = fn_item | struct_item | enum_item | const_item
            | mod_item | use_item | import_item | impl_item | type_item ;

fn_item     = [ "pub" ] [ "unsafe" ] "fn" ident [ generics ]
              "(" [ params ] ")" [ "->" type ] ( block | ";" ) ;
params      = param { "," param } ;
param       = [ "mut" ] ident ":" type ;
generics    = "<" generic { "," generic } ">" ;
generic     = ident [ ":" bound { "+" bound } ] ;

struct_item = [ "pub" ] "struct" ident [ generics ] struct_body ;
struct_body = "{" [ field { "," field } ] "}" | "(" types ")" | ";" ;
field       = [ "pub" ] ident ":" type ;

enum_item   = [ "pub" ] "enum" ident [ generics ] "{" variant { "," variant } "}" ;
variant     = ident [ "(" types ")" ] | ident "{" fields "}" ;

const_item  = [ "pub" ] "const" ident [ ":" type ] "=" expr ";" ;
type_item   = [ "pub" ] "type" ident [ generics ] "=" type ";" ;
mod_item    = [ "pub" ] "mod" ident ( ";" | "{" { item } "}" ) ;
use_item    = "use" ( path | string ) [ "as" ident ] ";" ;
import_item = "import" ( import_path | string ) [ "as" ident ] ";" ;
import_path = ident { ( "." | "::" ) ident } ;
impl_item   = "impl" [ generics ] type [ "for" type ] "{" { fn_item } "}" ;

block       = "{" { stmt } [ expr ] "}" ;
stmt        = let_stmt | defer_stmt | item | expr [ ";" ] ;
let_stmt    = "let" "mut"? pattern [ ":" type ] [ "=" expr ] ";" ;
defer_stmt  = "defer" expr ";" ;

expr        = assign ;
assign      = logic_or [ ( "=" | op="=" ) expr ] ;
logic_or    = logic_and { "||" logic_and } ;
logic_and   = equality { "&&" equality } ;
equality    = compare { ( "==" | "!=" ) compare } ;
compare     = bitor  { ( "<" | "<=" | ">" | ">=" ) bitor } ;
bitor       = bitxor { "|" bitxor } ;
bitxor      = bitand { "^" bitand } ;
bitand      = shift  { "&" shift } ;
shift       = range  { ( "<<" | ">>" ) range } ;
range       = additive [ ( ".." | "..=" ) additive ] ;
additive    = multiplicative { ( "+" | "-" ) multiplicative } ;
multiplicative = cast { ( "*" | "/" | "%" ) cast } ;
cast        = unary { "as" type } ;
unary       = ( "-" | "!" | "&" | "&mut" | "*" ) unary | postfix ;
postfix     = primary { "." ident
                      | "." ident "(" [ args ] ")"
                      | "[" expr "]"
                      | "(" [ args ] ")"
                      | "?" } ;
primary     = literal | ident | path
            | "(" [ expr { "," expr } ] ")"
            | "[" [ expr { "," expr } ] "]"
            | "{" block } | "if" if_expr | "match" match_expr
            | "loop" block | "while" expr block | "while" "let" pattern "=" expr block
            | "until" expr block | "for" pattern "in" expr block
            | "return" [ expr ] | "break" [ expr ] | "continue"
            | "unsafe" block
            | path struct_lit ;
if_expr     = "if" expr block { "elif" expr block } [ "else" ( if_expr | block ) ]
            | "if" "let" pattern "=" expr block [ "else" ( if_expr | block ) ] ;
pattern     = single_pat { "|" single_pat } ;
single_pat  = "_" | "mut"? ident | path [ "(" patterns ")" | "{" field_pats "}" ]
            | literal | "-" number | literal ( ".." | "..=" ) literal ;
struct_lit  = "{" [ field_init { "," field_init } ] "}" ;
path        = ident { "::" ident } ;
```

Statements and the final expression of a block are distinguished the same way
as in Rust: a block's value is its trailing expression, and `;` discards it.

## 2.3 Types

### Primitives

```
bool                1 byte, values true/false (no implicit bool<->int)
i8 i16 i32 i64      signed integers
u8 u16 u32 u64      unsigned integers
isize usize         pointer-width integers
f32 f64             IEEE-754
char                one Unicode scalar value (32-bit), not a byte
never               the type of `return`/`panic`; coerces to anything
```

### Composites

<!-- fragment -->
```zexi
[T; N]        fixed array, N a compile-time constant
[T]           slice: (pointer, len) fat pointer, bounds-checked
&[T] &mut [T] borrowed slice
(T, U, ...)   tuple; () is the unit type
str           immutable UTF-8 string slice (pointer + len), not NUL-terminated
*T *mut T     raw pointer (only usable in `unsafe`)
&T &mut T     shared / exclusive references
fn(A, B) -> R function pointer
```

### User-defined

- `struct` — product type, C-compatible layout by default (`#[repr(C)]` in M1/M2).
- `enum` — **tagged union**; a discriminant plus the largest variant payload.
  Variants may own resources (`Some(String::from("x"))`). `Option<T>` and
  `Result<T,E>` are predeclared generic enums, so they need no import; they
  have no compiler magic beyond being always in scope.
- `impl` — inherent and trait methods. A function declared without a `self`
  parameter is an *associated function*, called as `Type::name(args)` (for
  example `Point::new(1, 2)`); one with `self` is called on a value
  (`p.len()`, `p.shift(1.0, 2.0)`).

### Heap types

The only heap type implemented today is **`String`**: an owning, growable UTF-8
buffer. It is *not* `Copy` — it owns heap memory and is freed by its destructor.

```zexi
let mut s = String::from("hello");
s.push_str(", world");
s.push('!');
println(s.len());
println(s.as_str());     // borrows a `str` (valid while `s` lives)
let t = s.clone();       // independent deep copy
```

| method | signature | notes |
| --- | --- | --- |
| `String::new()` | `-> String` | empty |
| `String::from(str)` | `-> String` | allocates and copies |
| `String::with_capacity(usize)` | `-> String` | pre-sized buffer |
| `len(&self)` | `-> usize` | byte length |
| `is_empty(&self)` | `-> bool` | |
| `as_str(&self)` | `-> str` | borrowed view |
| `clone(&self)` | `-> String` | deep copy |
| `clear(&mut self)` | | truncate to empty |
| `push(&mut self, char)` | | append one code point (UTF-8) |
| `push_str(&mut self, str)` | | append a string slice |

Owning values compose: a `struct`, `enum` variant, or array containing a
`String` is itself an owning type and gets a generated destructor, so dropping
it drops each field/element/variant payload. `Vec<T>` and `Box<T>` are not
implemented yet.

`+` concatenates any mix of `str` and `String`, allocating a fresh `String`. A
number, `bool`, or `char` operand is rendered as text first, so labels are easy
to build without a helper (`"count: " + n`, `s += n`). `println`/`print` accept
any number of printable values and separate them with a single space.

### Generics (M1)

Generics are **monomorphized**: each concrete instantiation becomes its own
function or type in the generated C; no vtable or boxing is involved. Type
arguments are inferred from arguments and the expected type, or written
explicitly with a turbofish.

```zexi
fn id<T>(x: T) -> T { x }

struct Pair<A, B> { first: A, second: B }

impl<A, B> Pair<A, B> {
    fn swap(&self) -> Pair<B, A> {
        Pair { first: self.second, second: self.first }
    }
}

enum Maybe<T> { Just(T), Nothing }

let p = Pair { first: 1, second: "one" };
let q = p.swap();               // Pair<str, i32>
let n = id::<i64>(7);           // explicit turbofish
let m: Maybe<i64> = Maybe::Just(3);
```

Type parameters are currently **unconstrained**: there are no trait bounds
(`fn max<T: Ord>(..)` reports `E0900`), no `dyn Trait`, and no associated
types or const generics. Explicit arguments may appear on calls and types
(`id::<i64>`, `Pair::<i32, str>`).

`Option<T>` and `Result<T, E>` are predeclared generic enums with variants
`Some(T)`/`None` and `Ok(T)`/`Err(E)`. The variant names may be written bare
(`Ok(n)`, `None`) when the expected type makes the enum unambiguous.

## 2.4 Type system rules

- **Static, strong, nominal** for structs/enums; `dyn Trait` is structural.
- **Inference:** bidirectional (like Rust). Only *local* inference; function
  signatures are always explicit. Un-annotated integer literals default `i32`,
  but wherever the expected type is known — a `let` annotation, a function's
  return type, an `if`/`match` branch in such a position, a call argument — an
  integer literal adopts it (`fn f() -> i64 { 5 }` and
  `let x: u8 = 1;` both work).
- **No implicit conversions**, including `i32 -> i64` and `bool -> int`. Use
  `as` for primitive casts; `as` between numeric types is the only implicit-ish
  operation and is explicit. An *untyped* integer literal, however, may be used
  where a float is expected (`let x: f64 = 1`, `f(2)` for an `f64` parameter) —
  literal typing, not a conversion. The other convenience is `+`/`+=` on a
  string, where a number, `bool`, or `char` operand is rendered as text
  (`"n = " + n`); this formats the value, it does not convert its type.
  Pointer/`unsafe` casts live in `unsafe`.
- **Coercions are limited to:** `&mut T -> &T`, `[T; N] -> [T]`,
  `T -> dyn Trait`, `never -> T`, and unsizing arrays to slices.
- **Exhaustiveness:** `match` over an `enum` must be exhaustive.
- **Mutability:** a `let` binding is immutable. Assigning to it, to a field or
  element reachable from it, taking `&mut` of it, or calling a `&mut self`
  method on it requires `let mut` (or a binding that is already a `&mut`/`*mut`
  handle). `&self` methods may not mutate the receiver.
- **Null:** there is no null pointer value in safe code. Absence is `Option<T>`,
  and `*T` is only dereferenceable in `unsafe`.

## 2.5 Control flow & expressions

Everything except declarations is an expression.

```zexi
fn classify(n: i64) -> str {
    if n < 0 { "negative" }
    else if n == 0 { "zero" }
    else { "positive" }
}

fn abs(n: i64) -> i64 {
    match n {
        0 => 0,
        x if x < 0 => -x,   // pattern + guard
        x => x,
    }
}
```

Loops: `loop { }` (infinite), `while cond { }`, `until cond { }` (loops while
the condition is false), and `for pat in iter { }` over an integer range
(`1..4`) or an array. `break`/`continue` behave as expected. A `loop` may yield
a value with `break <expr>`, and the loop expression then has the type of that
value (`while`/`until`/`for` always evaluate to `()`).

`elif` is sugar for `else if`. `if let pat = expr { .. } else { .. }` and
`while let pat = expr { .. }` are sugar over `match`: the block runs when the
pattern matches; `while let` re-evaluates `expr` each iteration and stops at
the first non-match. Patterns in `match` arms and `let`-patterns may be
combined with `|` (or-patterns, whose alternatives may not bind variables),
may be inclusive/exclusive ranges over integer or `char` literals
(`1..=9`, `'a'..'z'`; an empty range is an error), and a binding may be marked
`mut` (`Step::More(mut v)`).

`defer expr;` evaluates `expr` when the enclosing block scope exits — by
falling off the end or via `return`/`break`/`continue` — in LIFO order per
scope, before that scope's locals are dropped. The expression is evaluated at
exit time, so it observes the locals' final values; `return`/`break`/
`continue` inside a deferred expression are rejected.

`type Name = Ty;` declares a type alias. An alias is transparent — it is the
same type as `Ty` everywhere (including in `impl` targets and array lengths),
may name another alias, and may not be cyclic. A tuple struct
`struct Pair(i32, i32);` is constructed with call syntax, `Pair(1, 2)`, and
its fields are read as `.0`, `.1`, and so on. Constants may refer to constants
declared later in the same file. A `char` literal holds exactly one Unicode
scalar value; `''` and `'ab'` are errors.

Parameters may declare **default values** (`fn area(w: i64, h: i64 = 10)`):
only trailing parameters may have them, and the default must be a constant
expression. Calls may omit defaulted arguments and may pass any argument
**by name** (`area(h = 4, w = 2)`), in any order, for functions and methods;
an unknown name (E0061), a duplicated argument (E0062), or a missing
required argument (E0061) is an error. A function whose return type is
omitted returns `()`. An interpolated string `f"...{expr}..."` evaluates to
an owning `String`; interpolated values are borrowed (never moved) and must
be printable (numbers, `bool`, `char`, `str`, `String`).

<!-- fragment -->
```zexi
let first = loop {
    if xs[i] > 100 { break xs[i]; }
    i += 1;
    if i == len(xs) { break 0; }
};
```

## 2.6 Ownership (summary — see docs/03)

<!-- fragment -->
```zexi
fn main() {
    let s = String::from("hello");   // s owns heap memory
    consume(s);                      // move; s is now unusable
    // println(s) would be a compile error: use of moved value

    let mut v = Vec::new();          // (M1; today the owning type is String)
    v.push(1);
    let r = &v[0];                   // shared borrow
    // v.push(2) would be a compile error while r is live
    println(r);
}
```

Rules:

- Values have a single owner. Assignment/parameter-passing **moves** non-`Copy`
  values.
- `Copy` types are copied, not moved. Primitives, references, and pointers are
  `Copy`; arrays/tuples are `Copy` when their components are. **Structs and
  enums are move-only by default** and opt into copying with `#[copy]` (all
  fields must then be `Copy`), mirroring Rust's `derive(Copy)`.
- **Destruction (`Drop`) is deterministic.** A destructor runs for every live
  local at the end of its scope, on early `return`/`break`/`continue`, and
  before an overwritten value. A move transfers ownership and empties the
  source, so the moved-out value is never freed twice. Destructors for a
  `struct`/array recurse into their fields/elements.
- `&T` / `&mut T` borrows must satisfy: any number of `&T`, **or** exactly one
  `&mut T`; a borrow cannot outlive its referent. *(Target rule; today the
  checker validates moves/use-after-move but does not yet track borrow
  lifetimes — bound aliases are not checked, and non-lexical lifetimes arrive in
  M1.)*

### Implemented today vs. target

| Aspect | Implemented (M0) | Target (M1/M2) |
| --- | --- | --- |
| Moves | yes; use-after-move = `E0382` | + conditional/partial moves tracked precisely |
| Drop | yes for `String`, and for `struct`/array values that contain one | user-defined `Drop`, drop flags, drop elision |
| Heap types | `String` | `Box<T>`, `Vec<T>`, collections |
| Enum payloads | `Copy` only (owning payloads rejected: `E0900`) | owning payloads once `match` borrows instead of aliasing |
| Borrows | `&`/`&mut self` param passing; no lifetime tracking | full NLL borrow checker, lifetimes |

## 2.7 Error handling

No exceptions. Two mechanisms:

1. **`Result<T, E>` + the `?` operator** for recoverable errors:

   ```zexi
   fn read_cfg() -> Result<Config, IoError> {
       let text = fs::read_to_string("cfg.toml")?;   // early-returns Err
       Ok(parse(text)?)
   }
   ```

   `e?` is ordinary match-based control flow: it yields the `Ok` payload and
   returns the `Err` case from the enclosing function. The function must return
   a `Result` whose error type matches (`E0277`/`E0308` otherwise). The same
   operator works on `Option<T>` (yielding `Some`'s payload and returning
   `None`) inside a function that returns `Option`.

2. **`panic`** for unrecoverable bugs; unwinds are **not** supported in M0 — a
   panic aborts (or calls a user hook), keeping the binary small and behavior
   predictable. Bounds-check failures and integer-overflow checks (debug) panic.

## 2.8 Modules & packages

The loader supports **scoped, path-qualified access** on top of the flat M1
preview:

- A **file** can be loaded as a module. `mod foo;` finds `foo.zx` or
  `foo/mod.zx` relative to its declaring file. `mod foo { ... }` is accepted
  for inline organization. Both create a `foo::` qualifier.
- `use` imports a whole library file at compile time. `use "path.zx";` loads a
  relative file; `use a::b;` loads `a/b.zx`, searched next to the importing
  file, then in a `lib/` folder beside it, then in the embedded standard
  library (`use std::math;` also works).
- `import` is an equivalent Python-shaped spelling. It additionally accepts
  dots: `import http.server;` selects the high-level HTTP module and
  `import io.fs;` selects the file helpers. `::` is accepted there too.
- **`as` binds the module to a qualifier**: `import math as m;` makes
  `m::sqrt(2.0)` valid. Without `as`, the last path segment is the qualifier
  (`import http.server;` → `server::Router`), and the full path is accepted too
  (`http::server::Router`).
- **Qualified paths are real lookups**: `alias::fn(..)`,
  `mod::Type { .. }`, `pkg::Type::new(..)`, `mod::Enum::Variant`, and
  `mod::Type` in type position all resolve against the target module's
  declarations, and `pub` is enforced (**E0603** for a private item, **E0412**
  for a missing one, **E0433** for using a module as a value).
- Imports are transitive, every file is loaded once, and cycles are harmless.

**Projects (convention only, no manifest).** The compiler accepts a directory as
well as a file. For a directory, `main.zx` is tried first, then `src/main.zx`;
the **project root** is that directory (for `zexic run src/main.zx`, the root is
the directory above `src`). Named imports (`use foo;`, `use a::b;`, `mod foo;`)
are searched in this order: the importing file's directory, its `lib/` folder,
the project root, the root's `lib/` folder, then the embedded standard library.
If a lookup names a **directory** instead of a file, the whole folder is loaded
as one package: every `.zx` file in it (except `main.zx` and `_`-prefixed files)
is compiled in, and its public names are reachable through the package
qualifier. So a project can be `main.zx` plus a handful of files, or a tree of
folders, and `zexic run .` compiles the entry and everything it links to.
`--root=<dir>` overrides the discovered root.

The preview remains **flat underneath**: items are still merged into one
program namespace, so plain imports continue to expose their names unqualified
(this keeps existing code working and means `pub` is not yet enforced for
*unqualified* access). Name collisions between two loaded modules are still
errors, and there are no re-exports, glob imports, or package metadata yet.
Full isolation — per-module namespaces that make unqualified cross-module
access respect `pub` — is the remaining M1 work. An unresolvable
import/module is E9022/E9023.

Files may also be passed directly on the command line, as before.

## 2.9 Attributes and native code

`#[...]` annotate items: `#[repr(C)]`, `#[inline]`, `#[inline(never)]`,
`#[no_mangle]`, `#[cold]`, `#[packed]`, `#[export]`, `#[deprecated]`,
`#[align(N)]`, `#[link("name")]`. Attributes are declarations to the compiler,
not user-extensible macros.

Two items let a module reach outside the language:

- `extern "C" fn name(..) -> T;` declares a C function to call.
- `c """ ... """` compiles a block of C with the program. The block is appended to
  the generated translation unit, so it can use the runtime types and call
  `#[no_mangle]` functions.

`#[link("m")]` on either item adds `-lname` to the link step (`"pthread"` also
adds `-pthread`). Together they are how a library adds native code without a
change to the compiler; the standard library's C is shipped entirely this way — see [`17-extending.md`](17-extending.md).

## 2.10 Standard library scope (M0)

Deliberately tiny — a "core" that is largely in-language:

- `core`: primitive ops, `Option`, `Result`, `Iterator`, `Clone`, `Copy`,
  `Drop`, `Ord`/`PartialOrd`, `Eq`, formatting traits.
- `alloc` (opt-in): `Box`, `Vec`, `String`, arenas, `GlobalAlloc`.
- `os`/`sys` (opt-in): file, socket, process, time — thin libc or syscall
  wrappers, only linked when used.

The shipped M1-preview helpers are embedded source modules: `io`, `fs`, `fmt`,
`codec`, `math`, `text`, `regex`, `random`, `sys`/`os`, `net`, `http`, and
`thread`. For example, `import text.regex;` selects the small POSIX-ERE facade,
`import os;` selects local environment/path/process helpers, `use fmt;` selects
pure-Zexi number/value formatting, `use codec;` selects hex/base64 and
checksums, and `use thread;` selects the pthread concurrency preview (threads,
`i64` channels, `i64` atomics; see [`11-concurrency.md`](11-concurrency.md)).
Their C shims are linked only if that module is selected.

No `std` is linked by default; `#![no_std]` is the default rather than an
opt-in, which is a key part of hitting the binary-size target.
