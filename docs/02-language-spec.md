# 2. Language Specification (draft v0.1)

This is the *normative-shaped* spec for the M0 language. Features tagged
**M1/M2** are designed here but not implemented initially.

## 2.1 Lexical structure

- **Encoding:** UTF-8 source; identifiers are ASCII for M0.
- **Line comments:** `// ...`
- **Block comments:** `/* ... */`, nesting.
- **Doc comments:** `///` (attached to the following item).
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
| Raw string | `r"C:\path"` |
| Boolean | `true`, `false` |
| Unit/void | `()` |

Integer literals are **untyped** until inference; they default to `i32`. There
are no implicit numeric conversions (see 2.4).

### Keywords

```
as      break   const   continue  defer   else    enum    extern
false   fn      for     if        impl    in      let     loop
match   mod     mut     pub       return  struct  true    type
unsafe  use     while   i8 i16 i32 i64 isize u8 u16 u32 u64 usize f32 f64
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
            | mod_item | use_item | impl_item | type_item ;

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
use_item    = "use" path [ "as" ident ] ";" ;
impl_item   = "impl" [ generics ] type [ "for" type ] "{" { fn_item } "}" ;

block       = "{" { stmt } [ expr ] "}" ;
stmt        = let_stmt | item | expr [ ";" ] ;
let_stmt    = "let" "mut"? pattern [ ":" type ] [ "=" expr ] ";" ;

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
            | "loop" block | "while" expr block | "for" pattern "in" expr block
            | "return" [ expr ] | "break" [ expr ] | "continue"
            | "defer" expr | "unsafe" block
            | path struct_lit ;
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
  `Option<T>` and `Result<T,E>` are ordinary library enums, no compiler magic
  except niche optimizations in M2.
- `impl` — inherent and trait methods.

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

Owning values compose: a `struct` (or array) containing a `String` is itself an
owning type and gets a generated destructor, so dropping it drops each
field/element. `Vec<T>` and `Box<T>` arrive with generics (M1).

### Generics (M1)

Generics are **monomorphized**: each concrete instantiation is a separate
function/type. No vtable is required unless the generic parameter is used as a
`dyn` trait object.

```zexi
fn max<T: Ord>(a: T, b: T) -> T { if a > b { a } else { b } }
```

Traits provide bounds; `dyn Trait` gives type-erased dynamic dispatch (M2).

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
  operation and is explicit. Pointer/`unsafe` casts live in `unsafe`.
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

Loops: `loop { }` (infinite), `while cond { }`, `for pat in iter { }`, where
`for` desugars to the `Iterator` trait protocol. `break`/`continue` as expected;
`break value` yields from a `loop`.

## 2.6 Ownership (summary — see docs/03)

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

2. **`panic`** for unrecoverable bugs; unwinds are **not** supported in M0 — a
   panic aborts (or calls a user hook), keeping the binary small and behavior
   predictable. Bounds-check failures and integer-overflow checks (debug) panic.

## 2.8 Modules & packages

- A **file** is a module; a **directory** with `mod.zx` is a package root.
- `mod foo;` declares a child module (file `foo.zx` or `foo/mod.zx`).
- `use` brings paths into scope; `pub` controls visibility.
- **M0 compiles a single file** for the first milestone; the module system
  lands in M1.

## 2.9 Attributes (M1/M2)

`#[...]` annotate items: `#[repr(C)]`, `#[inline]`, `#[inline(never)]`,
`#[no_mangle]`, `#[cold]`, `#[packed]`, `#[export]`, `#[deprecated]`,
`#[align(N)]`. Attributes are declarations to the compiler, not user-extensible
macros.

## 2.10 Standard library scope (M0)

Deliberately tiny — a "core" that is largely in-language:

- `core`: primitive ops, `Option`, `Result`, `Iterator`, `Clone`, `Copy`,
  `Drop`, `Ord`/`PartialOrd`, `Eq`, formatting traits.
- `alloc` (opt-in): `Box`, `Vec`, `String`, arenas, `GlobalAlloc`.
- `os`/`sys` (opt-in): file, socket, process, time — thin libc or syscall
  wrappers, only linked when used.

No `std` is linked by default; `#![no_std]` is the default rather than an
opt-in, which is a key part of hitting the binary-size target.
