# 12. Generics and `Option`/`Result` — design note

> **Status:** implemented on branch `m1-generics` (stages 1–5), landed as
> commits `b531034`, `8e53534`, `c679e9c`, `936493a`, and `14b8aad`. Stage 6
> folds the result back into the user-facing docs. The surface and model below
> are what shipped; §12.14 records the remaining limits.

This closes the largest remaining M1 gap. Today the parser rejects generic
syntax outright (`E0900: generics are not supported yet`), `PathType.Args` is
parsed-then-dropped, and the M0 acceptance item "returns a `Result` propagated
with `?`" is unmet. After this pass:

```zx
enum ParseError { Empty, BadDigit { pos: usize } }

fn parse_u32(s: str) -> Result<u32, ParseError> {
    let v = String::from(s);            // `byte_at` is a String method
    if v.len() == 0 { return Err(ParseError::Empty); }
    let mut acc: u32 = 0;
    let mut i: usize = 0;
    while i < v.len() {
        let c = v.byte_at(i);
        if c < 48 || c > 57 { return Err(ParseError::BadDigit { pos: i }); }
        acc = acc * 10 + ((c - 48) as u32);
        i += 1;
    }
    return Ok(acc);
}

fn double(s: str) -> Result<u32, ParseError> {
    let n = parse_u32(s)?;   // early-return on Err, unwrap the Ok
    return Ok(n * 2);
}
```

## 12.1 Scope (locked)

1. **Full monomorphization** of user generics: functions, structs, enums, and
   `impl` blocks.
2. **Owned type arguments are allowed** (`Option<String>`, `Result<String, E>`).
   This requires fixing the current "enum variants cannot carry owning
   payloads" restriction (§12.7).
3. **Inference and turbofish**: type arguments are inferred from the expected
   type and arguments where possible, and written explicitly as `f::<T>(..)` /
   `Type::<T, U>::new()` when not.
4. **Built-in `Option<T>` / `Result<T, E>`** and the **`?` operator** (desugared
   to `match`, §12.6).
5. Delivered as **one branch/PR, staged by commit** (§12.13).

## 12.2 Non-goals (explicit)

- **Traits and bounds.** `T: Ord`, `dyn Trait`, associated types, operator
  overloading, and blanket impls are a separate feature. This pass ships
  **unconstrained** type parameters only. A written bound is a clean `E0900`
  ("trait bounds are not supported yet"). Consequently a generic body may treat
  `T` only as an opaque value: bind, move, pass, store, return, match, and drop
  it — but not index, add, compare, or call methods on it.
- **Const generics** (`[T; N]` with `N` a parameter).
- **`Vec<T>`/`Box<T>`/`HashMap`.** Those are library types that *use* this
  machinery; adding them is follow-up work (the allocator seam is already
  sketched in docs/03 §3.5).
- **`Send`/`Sync`, lifetimes, NLL.** Orthogonal.
- **Higher-kinded/associated types, defaults, variadics.**

## 12.3 Surface grammar

The EBNF in docs/02 §2.2 already reserves the syntax; the parser simply errors
today. Additions:

```ebnf
fn_item     = [ "pub" ] [ "unsafe" ] "fn" ident [ generics ] "(" ... ) ;
struct_item = [ "pub" ] "struct" ident [ generics ] ... ;
enum_item   = [ "pub" ] "enum" ident [ generics ] ... ;
type_item   = [ "pub" ] "type" ident [ generics ] "=" type ";" ;
impl_item   = "impl" [ generics ] type "{" { fn_item } "}" ;

type        = ... | path_type | ... ;
path_type   = ident [ "::" ident ] [ "<" type { "," type } ">" ] ;   (* Foo / Option<i64> *)

postfix     = ... | "::" "<" type { "," type } ">" | "." ident ... ;  (* turbofish *)
```

- Type arguments use the normal `<...>` form: `Option<i64>`, `Pair<A, B>`.
- Expression turbofish is `::<...>` before a call or field access:
  `Vec::<i64>::new()`, `make::<u8>(1)`.
- `> >` is not split; the lexer hands the parser `>>` as one token, so the type
  argument parser must treat a `>>` token as two closing angle brackets.

### AST additions

| Node | Field to add |
| --- | --- |
| `StructDecl` | `Generics []GenericParam` |
| `EnumDecl` | `Generics []GenericParam` |
| `TypeAlias` | `Generics []GenericParam` |
| `ImplBlock` | `Generics []GenericParam` |
| `FnDecl` | already has `Generics` |
| `PathExpr` | `TypeArgs []Type` (turbofish on an expression path) |
| `PathType.Args` | already exists; start populating it |

`GenericParam` (with `Name`, and `Bounds []Type`) already exists in `ast`.

## 12.4 Semantic model

### 12.4.1 Type parameters

Add one semantic kind:

```go
// types.Kind
Param // a generic type parameter, alive only while checking a template

// types.Type
Param string // the parameter's name, when Kind == Param
```

A generic template is checked **once** with each `T` represented as
`&types.Type{Kind: types.Param, Param: "T"}`. The checker records the template's
`Fn` (and `types.Struct`/`types.Enum`) with a `Generics []string` list, and marks
it `Generic = true`.

`IsCopy`/`NeedsDrop` on a `Param` are **unknown** and return a conservative
`false`. Because a `Param` value is treated as non-`Copy`, the existing move
checker (`internal/sema/own.go`) already moves it on assignment/pass — which is
the correct conservative behavior for both owned and `Copy` instantiations once
monomorphized.

### 12.4.2 Instantiation keys and names

Every concrete instantiation gets a **canonical key** that is an injective
encoding of the base name plus its type arguments:

```
instName("Pair", [i64, str]) == "Pair$i64$str"
instName("Result", [u32, ParseError]) == "Result$u32$ParseError"
```

- The `$` separator cannot occur in a Zexi identifier, and codegen's existing
  `safeIdent` hex-escapes it (`$` → `_24_`) while escaping `_` as `_5f`, so
  downstream C names (`zx_Pair_24i64_24str`) stay unique.
- `typeKey(t)` is an injective rendering of a type; nested types join with `$`
  (`&Pair$i64$str` → `r$Pair$i64$str`). A unit test asserts injectivity over a
  representative set.

`types.Struct`/`types.Enum` gain display fields so diagnostics stay readable
while the map key stays mangled:

```go
type Struct struct {
    Name  string      // canonical key, e.g. "Pair$i64$str"
    Base  string      // source name, e.g. "Pair" (== Name for non-generic types)
    Args  []*Type     // instantiation arguments, nil for non-generic types
    ...
}
```

`types.Type.String()` prints `Base<Args…>` when `Args != nil`, else `Name`. This
keeps every existing `String()`-based diagnostic correct and human-readable.

### 12.4.3 Templates vs. instantiations

- `Info.Fns` holds only **concrete** functions the backend may emit, plus
  templates flagged `Generic`.
- Templates are **excluded** from `Info.FnOrder` (which drives codegen's
  `reachableFns`), so nothing generic is ever emitted.
- Instantiations are ordinary `*sema.Fn` values, registered in `Info.Fns`,
  `Info.FnOrder`, and `Info.Methods` exactly like hand-written functions. The
  backend therefore needs **almost no generic awareness** (§12.8).

## 12.5 Checking strategy

The hard constraint is that `sema.Info` keys expression types by `ast.Expr`
**pointer**, and codegen walks the AST. If a generic body were checked once and
instantiated twice, both instantiations would share the same expression nodes
and therefore the same recorded types — codegen could not tell them apart.

**Chosen approach: instantiate by cloning the body and re-checking it.**

1. **Declare** each generic item once: resolve its signature with parameters
   represented as opaque `Param` types, and register the template name, parameter
   list, and (for types) fields/variants containing `Param`s.
2. **Defer body checking.** A generic body is checked only when an instantiation
   is requested. This keeps diagnostics concrete (the checker sees real types)
   and avoids reporting the same error once per instantiation; identical
   diagnostics produced by several instantiations are deduplicated by
   (span, code, message). An unused generic body is therefore not checked
   (documented, C++-template-like).
3. **On first use** of a concrete instantiation, `instantiate(template, args)`:
   - compute the canonical key; return the cached `*Fn`/`*Struct`/`*Enum` if present;
   - deep-**clone** the template's AST body (`ast.Clone`, below);
   - re-run the checker on the clone with a type-parameter environment
     `{T ↦ arg, …}` so `resolveType`/`checkExpr` record concrete types on the
     cloned nodes;
   - register the resulting concrete function/type under the key.
4. Recursive/nested instantiation (`fn f<T>() { g::<T>() }`) happens naturally:
   re-checking the clone resolves `g::<T>` with `T` already bound to the concrete
   argument.

**`ast.Clone`.** A pointer-preserving deep copy of the AST is required so each
instantiation owns distinct expression nodes. Rather than hand-writing ~60 node
copiers, implement `ast.Clone` with the same reflection technique as codegen's
`walkExprs` (docs/04-style "new node kinds are covered automatically"),
allocating fresh structs/slices/pointers and recursing into interface values.
A round-trip test (`Clone` then structural compare) guards it. If reflection
proves awkward for a node, that node gets an explicit case.

### 12.5.1 Inferring type arguments

At a generic call site, unify the callee's parameter patterns against the
argument types:

- `let x = f(1)` with `fn f<T>(x: T)` ⇒ `T = i32` (literal defaulting applies).
- `let x: Option<String> = make();` ⇒ `T = String` from the expected type.
- Mismatch between two inference sources is `E0308`; an unresolved parameter is
  `E0282` ("type annotations needed").

Unification is structural over the parameter type: `Param` binds; `&`, `*`,
`[]`, `[T; N]`, tuple, struct, and enum instantiations recurse. A parameter that
appears only in the return type cannot be inferred without turbofish (`E0282`).

### 12.5.2 Turbofish

`f::<i64>(x)` and `Type::<i64, str>::new()` supply arguments explicitly.
Explicit arguments are checked for arity (`E0107`) and then used directly,
skipping inference for those slots.

### 12.5.3 Generic types in literals and patterns

- **Construction:** `Pair { a: 1, b: "x" }` infers arguments from field
  initializers and/or the expected type; `Pair::<i64, str> { .. }` is explicit.
  Tuple-struct form `Pair::<i64>(1)` is supported the same way. Variant
  construction (`Result::Err(e)`) resolves the **base** enum name and then infers
  the instantiation from the expected type and the payload arguments — which is
  why `?` works: its `return Result::Err(err)` is in return position, where the
  expected type is the function's `Result<T, E>` and both parameters are known.
  Turbofish can always pin the arguments explicitly when inference is
  insufficient.
- **Patterns:** `match opt { Option::Some(x) => …, Option::None => … }` already
  resolves variants from the scrutinee's enum type; with instantiation that
  scrutinee type is `Option$i64` and the payload type is concrete.
- **`let` with a generic annotation:** `let o: Option<i64> = …;` resolves
  `Option<i64>` into the instantiated enum.

### 12.5.4 Methods on generic types

`impl Pair<T> { fn get(&self) -> T { … } }` stores its methods under the
**base** name `Pair` keyed by method name, with the impl's `Generics` recorded.
Looking up a method on a receiver of type `Pair$i64$str` finds the template and
instantiates the method with the receiver's arguments (`T ↦ i64`, second param
from the type args). Associated functions (`Pair::<i64>::new(..)`) resolve the
same way.

## 12.6 `Option`, `Result`, and `?`

### 12.6.1 Predeclared enums

`Option<T>` and `Result<T, E>` are ordinary generic enums, but they are
**predeclared** by the checker (registered exactly like a two-variant generic
enum in a built-in `core` scope), so they are usable without an import — matching
`examples/result.zx`. They carry no compiler magic beyond being always in scope.
`String`/`String::from` receive the same treatment today, so this is consistent.

```zx
enum Option<T> { Some(T), None }
enum Result<T, E> { Ok(T), Err(E) }
```

### 12.6.2 Desugaring `?`

`e?` is lowered to a `match` **before** type checking, so the checker and
codegen only ever see ordinary `match`:

```zx
// in a function returning Result<..,..>
let n = parse_u32(s)?;
// becomes
let n = match parse_u32(s) {
    Result::Ok(v) => v,
    Result::Err(err) => return Result::Err(err),
};

// in a function returning Option<..>
// becomes
match e { Option::Some(v) => v, Option::None => return Option::None }
```

- The desugar runs as a small AST pass over each function body, using the
  **base name of the function's declared return type** to choose the
  `Result`/`Ok`/`Err` or `Option`/`Some`/`None` shape.
- Anything else is `E0277`: "the `?` operator requires the enclosing function to
  return `Result` or `Option`".
- Constructors are emitted **qualified** (`Result::Err(..)`), which the checker
  already supports; no bare-variant resolution is required.
- The payload binding is a normal move (§12.7), so `Result<String, E>?` is sound.

### 12.6.3 Bare-variant ergonomics (small addition)

So that `Ok(x)` / `Err(e)` / `Some(x)` / `None` read naturally (as the examples
write them), the checker also resolves a **bare** variant name from the expected
type: in `checkExpr(e, expected)`, a call whose callee is a bare identifier, or a
bare identifier/path in value position, resolves to a variant of `expected`'s
enum when the name matches. This is a small, self-contained addition to
`inferCall`/`inferExpr` and is independent of the rest of the pass.

## 12.7 Owned payloads in enum variants (the match fix)

The current restriction (`E0900`, sema.go ~line 399) exists because a `match`
**binds a payload by shallow copy**:

```c
/* bindArm / bindPatTo today */
zx_String x = m.u.v0.field;   /* shallow copy of an owned value */
```

and the match scrutinee is never dropped, so owned payloads either double-free
(if bindings were dropped) or leak (as emitted today). The fix has three parts:

1. **Move bound owned payloads.** For a non-`Copy` payload field, emit
   `T x = zx_move_<T>(&m.u.vN.field);` instead of a plain copy. Codegen already
   generates `zx_move_<name>` for every drop type (`emitDropFunc`), so the
   helper exists. This nulls the source, so a later drop of the scrutinee is
   safe.
2. **Register owned bindings for drop.** `bindArm`/`bindPatTo` call
   `registerDrop` for bindings whose type `NeedsDrop`, so an unused binding frees
   at arm scope exit, with early `return`/`break` handled by the existing drop
   frames.
3. **Drop the scrutinee once, at the end of the match**, when its type
   `NeedsDrop`. Bindings were moved out (source nulled), so this frees only the
   payloads no arm bound (e.g. the body of `Err(_)`), with no double free.
   Registering the scrutinee in a drop frame makes early `return` correct too.

With those three changes the `E0900` guard is removed and `collectDropTypes`
(which already recurses through enum variants) automatically emits drop glue for
every owning instantiation.

## 12.8 Codegen changes

Monomorphization lives entirely in sema, so codegen stays dumb:

- `reachableFns` already iterates `Info.FnOrder` and follows `Info.Resolved`; as
  long as instantiations are registered there (they are, §12.4.3), prototypes and
  definitions are emitted with no codegen change.
- `typeMangle`, `typeName`, `emitStructDef`, `emitEnumDef`, `emitDropFunc`, and
  `collectDropTypes` already recurse through nested struct/enum/array types and
  are keyed by `Name`, so instantiations "just work" once their `types.*` entries
  exist with correct fields.
- The only codegen edits are the §12.7 match binding/scrutinee changes and using
  the display form for any type names it prints.

## 12.9 Diagnostics

| Code | Meaning |
| --- | --- |
| `E0107` | wrong number of type arguments (`Foo<T>` given 0 or 2) |
| `E0282` | type annotations needed (a parameter could not be inferred) |
| `E0283` | ambiguous instantiation (two candidate arguments) |
| `E0118` | unknown type parameter in a signature/body |
| `E0277` | the `?` operator used where the function does not return `Result`/`Option` |
| `E0900` | (kept) `T: Bound` / `dyn` / trait impls are not supported yet |
| `E0308` | instantiation-argument mismatch (reuses the existing mismatch error) |

Codes are chosen to match Rust's where the meaning matches, so the docs and the
(stable-code) diagnostic list stay conventional.

## 12.10 Driver and stdlib

- No driver changes are needed for the core feature; the loader already merges
  modules before checking.
- `Option`/`Result` are predeclared (§12.6.1), so `examples/result.zx` and
  `examples/enum_match.zx` work without imports.
- The aspirational examples need small fixes to match the supported surface:
  struct-variant syntax (`BadDigit { pos: usize }`, not `BadDigit(pos: usize)`)
  and `str`/`&[u8]` arguments (`parse_u32(s: str)` using `byte_at`, since slices
  are still a placeholder type). They become registered acceptance tests.

## 12.11 Testing

- **New runnable programs** (type-check + run + parallel-vs-single parity, like
  the existing suites):
  - `tests/generics.zx` — generic functions (inference + turbofish), generic
    structs/enums, generic `impl` methods, nested instantiation, a struct and an
    enum containing `String` (owned payloads), and a `Copy` instantiation.
  - `tests/option_result.zx` — `Option`/`Result` construction, `match`, `?`
    propagation of both `Ok` and `Err`, `?` inside nested calls, and an owned
    `Result<String, _>`.
- **Negative tests** (type-check only, expect diagnostics): wrong arity
  (`E0107`), uninferable parameter (`E0282`), `T: Bound` (`E0900`), and `?` in a
  non-`Result`/`Option` function (`E0277`).
- **Drop correctness:** an owned `Option<String>`/`Result<String,_>` test, plus a
  Go test that builds one such program with `-fsanitize=address` when the
  toolchain supports it (skipped otherwise), so double-free/leak regressions are
  caught.
- **Unit tests:** `instName` injectivity over a representative type set; `ast.Clone`
  round-trip.
- Existing `tests/enums.zx` and the struct/enum/drop tests must keep passing
  (they exercise the touched `bindArm`/scrutinee paths).

## 12.12 Risks and limitations

| Risk | Mitigation |
| --- | --- |
| AST cloning via reflection misses a node shape | Round-trip test; codegen already trusts reflection over the same structs |
| Re-checking bodies multiplies diagnostics | Instantiation-time checking with span+code+message dedupe; unused templates are unchecked |
| Box bloat from eager instantiation | Instantiate only what is reachable (driven by `usedFns`); DCE at MIR is future work |
| No bounds makes some generic bodies unexpressible | Documented; traits are the next feature after this |
| Match-drop interaction with early `return` | Reuse existing drop frames; ASan test |
| `?` tied to `Result`/`Option` by base name | Documented limitation; user enums can opt in by using `Ok`/`Err` variants |

## 12.13 Delivery plan (one branch, staged commits)

1. **Parser + AST:** remove the `E0900` generics errors; populate `Generics` on
   all items; parse `PathType.Args` and expression turbofish (including `>>`);
   add the missing AST fields. (Type-check-only; no behavior change yet.)
2. **`ast.Clone` + semantic `Param` kind + template declaration/checking:**
   `types.Param`, `Generics`/`Generic` on `Fn`, template registration, and the
   opaque-`T` body check with `E0900` for bounds.
3. **Generic functions:** instantiation, inference, turbofish, `E0107`/`E0282`,
   registration in `Info`/`FnOrder`; `tests/generics.zx` (functions only).
4. **Generic structs, enums, and `impl` methods**, including construction,
   patterns, and owned payloads; the §12.7 match-move fix; expand
   `tests/generics.zx`.
5. **`Option`/`Result`/`?`:** predeclared enums, `?` desugar, bare-variant
   ergonomics; `tests/option_result.zx`; update `examples/result.zx` and
   `examples/enum_match.zx`; register them.
6. **Docs + polish:** this note is finalized as shipped; update docs/02
   (§2.3/§2.10), docs/05 (roadmap checkmarks), docs/07 (cheatsheet), docs/03 (the
   enum-payload gap is closed), README, and `workings.md`; ASan drop test.

Each commit keeps `gofmt`, `go vet ./...`, and `go test ./...` green. If Stage 3
reveals the clone/re-check approach is too costly, the fallback is to key
`sema.Info` by `(instantiation, expr)` instead of cloning; that decision point is
deliberately placed after generic functions work, since functions are the
simplest case.

## 12.14 Status and known limitations

All six stages landed. Shipped today:

- Generic functions, structs, enums, tuple structs, and inherent impls
  (methods and associated functions), with inference from arguments, fields,
  payloads, and the expected type, plus turbofish on calls and types.
- `Option<T>` / `Result<T, E>` as predeclared templates, bare `Some`/`None`/
  `Ok`/`Err` from context, and `?` lowered to an ordinary match for both enums.
- Owned payloads: constructing, moving into and out of an enum, dropping the
  active variant once, and match-move glue (including guarded arms).
- Diagnostics: `E0107` arity, `E0282` inference, `E0072` direct recursive
  instantiation, `E0428` duplicate parameters, `E0507` moving an owned payload
  out of a borrow, `E0277`/`E0308` for `?` placement and error type, and
  `E0900` for trait bounds.

Known limits (deliberate for M1):

- Inference through *nested* generic fields (e.g. a field of type
  `Inner<T>`) is conservative: annotate the literal or use a turbofish.
  Direct payload/argument inference, which covers the common cases, works.
- Nested patterns are still rejected (`Err(MyErr::Bad { pos })`); bind the
  payload and match again, or match the inner enum directly.
- `?` cannot convert between error types; the error type must match the
  enclosing function exactly.
- Borrowed payloads cannot be moved (`E0507`); bind by value or ignore them.
- The ASan leak/double-free check could not run in the Alpine/musl CI image
  (no libasan/clang). Drop behavior is covered by the runtime tests in
  `tests/generics.zx`, `tests/option_result.zx`, and `tests/drop.zx`.
