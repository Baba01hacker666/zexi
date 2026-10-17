# 1. Overview

## 1.1 The thesis

"High-level" and "small/fast/native" are usually opposites. GC'd languages (Go,
Java, C#) buy convenience with a runtime and a large binary. Rust buys safety and
speed with a steep learning curve and infamous complexity. C buys size and speed
with no safety at all.

Zexi's thesis: **memory safety can be paid for entirely at compile time**, and if
the language commits to that, it needs *no* runtime services for memory. That
removes the GC, the reflection metadata, and the service layer that make
Go/Rust binaries large — while keeping the expressiveness of a modern,
expression-oriented language.

Three hard goals, in priority order when they conflict:

1. **Memory safety** — no use-after-free, double-free, buffer overflow, or data
   race reachable from safe code. Undefined behavior is confined to `unsafe`.
2. **Execution speed** — Rust-class. Zero-cost abstractions: value types,
   monomorphized generics, inlining, no mandatory indirection.
3. **Small binaries** — C-class. A "hello world" should be tens of KB, and a
   freestanding target should link a runtime measured in KB.

Simplicity is the *method*: a small language is easier to learn, easier to
implement, and easier to keep safe than a large one.

## 1.2 Non-goals (for now)

- **No GC option.** We do not ship a tracing collector. Programs that need one
  can link a third-party allocator/collector through `unsafe` — the language
  makes no promise about it.
- **No macros in M0/M1.** Metaprogramming arrives only if generics + `comptime`
  cannot cover the need, and only in a hygienic, non-Turing-complete form.
- **No OOP inheritance.** Composition + `impl` methods + enums with data.
- **No exceptions.** Errors are `Result<T, E>` values (see 2.7).
- **No async/await in M0/M1.** Green threads are a library concern; the runtime
  has no scheduler.
- **No interpreter or bytecode VM.** See 4.1 — a VM conflicts with the size and
  startup goals and would make the memory model harder to reason about.

## 1.3 Why Go for the compiler?

Go is a good host language for a compiler: fast compile times (good for CI and
iteration), a decent standard library, easy concurrency for parallel module
compilation, simple cross-compilation, and a garbage collector *for the compiler*
(not for Zexi programs). The compiler's own runtime characteristics are
irrelevant to the language's; the host language choice optimizes for our
development speed, not the emitted code.

## 1.4 How Zexi compares

| | C | Rust | Go | **Zexi (M2)** |
| --- | --- | --- | --- | --- |
| Memory safety (safe subset) | ✗ | ✓ | ✓ | ✓ |
| GC required | no | no | yes | **no** |
| Hello-world size (static) | ~10 KB | ~300 KB+ | ~1.2 MB | **target < 50 KB** |
| Peacetime performance | high | high | high | high |
| Runtime | minimal | present | large | **minimal/optional** |
| Learning curve | low | high | low | **medium-low** |
| Generics | none | complex | interfaces | **simple, monomorphized** |
| Concurrency safety | none | borrow-checked | GC'd | **borrow-checked** |
| Compile speed | high | low | very high | **high** |

Design lesson taken from each:

- **From Rust:** ownership/borrowing, `Option`/`Result`, enums-as-sum-types,
  value semantics, zero-cost generics, explicit `unsafe`.
- **From C:** predictable layout, no hidden allocation, small runtime, direct
  compilation to machine code.
- **From Go:** the compiler is fast and simple; keep the language orthogonal;
  interfaces-by-structure (without GC) for dynamic dispatch when needed.

## 1.5 The simplicity budget

Simplicity is enforced as a *budget*: every feature must justify itself against
the three goals. Concrete rules:

- **One obvious way** to express a given construct.
- **No implicit control flow** except `?` (error propagation) and `defer`.

  *(Corrected: `defer` is explicit; `?` is the single implicit early-return and
  is syntactically visible.)*
- **No hidden allocations.** Allocation is always spelled (`Box::new`, an
  arena, a `Vec`).
- **Orthogonal**: `if` and `match` are expressions because everything is; there
  is no separate "ternary".
- **No overloading by arity/type.** One name, one meaning; generics carry the
  polymorphism.
