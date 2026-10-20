# 3. Memory Model

Zexi's memory-safety story is the reason it exists. This chapter is the design
contract; docs/02 has the surface syntax.

## 3.1 Design choice: static ownership, no collector

We choose **compile-time ownership with borrow checking** (Rust-like) over:

| Alternative | Why rejected |
| --- | --- |
| Tracing GC | Needs a runtime + metadata → violates size & latency goals |
| Reference counting | Runtime cost on every assignment; cycles leak |
| Region inference alone | Too restrictive/opaque; poor for general code |
| Manual `free` | Not memory-safe |

Static ownership gives **deterministic destruction**, a near-zero runtime, and
no pauses. The cost is compiler complexity — paid once, in Go, by us.

## 3.2 The model in four rules

1. **Every value has exactly one owner.** The owner is a stack slot, a struct
   field, an array element, or a heap allocation reachable from one.
2. **Moves transfer ownership.** Assigning or passing a non-`Copy` value moves
   it; the source becomes *uninitialized* and using it is a compile error.
3. **Borrows are temporary, checked accesses.** `&T` is a shared borrow, `&mut T`
   is exclusive. Aliasing rules (below) are enforced by the checker.
4. **Drop is static.** When an owner goes out of scope (or is moved into a
   `drop`), the compiler inserts the destructor at a statically known point. No
   runtime bookkeeping decides when to free.

## 3.3 Aliasing rules (the "one mutable XOR many shared")

At any program point, for a given memory location:

- Any number of `&T` borrows are allowed **if no `&mut T` is live**, or
- Exactly one `&mut T` borrow **and no `&T`**.

"Live" is computed by **non-lexical lifetimes (NLL)**: a borrow ends at its last
use, not at the closing brace. M0 ships a conservative lexical approximation;
M1 ships real NLL.

Borrows may not outlive their referent:

```zexi
fn dangling() -> &i32 {
    let x = 5;
    &x            // ERROR: `x` does not live long enough
}
```

## 3.4 Lifetimes

M1 introduces explicit lifetime parameters only when elision fails:

```zexi
fn longest<'a>(a: &'a str, b: &'a str) -> &'a str {
    if a.len() > b.len() { a } else { b }
}
```

Most signatures need no annotation: borrowing a single input, or `self`, elides
automatically. Lifetimes are erased before codegen.

## 3.5 Stack vs heap

- **Stack by default.** Locals live on the stack; escape analysis + inlining
  keep many `Vec`s/`Box`es off the heap entirely (M2 optimization).
- **Heap is explicit.** `Box::new`, `Vec`, `String`, arenas. There is no hidden
  allocation anywhere in the language — no boxing from interfaces, no reification
  of generics, no closures capturing by heap unless captured by `move` into a
  `Box`.
- **Allocators:** the `GlobalAlloc` trait is the single seam. The default global
  allocator is a small `mmap`-backed bump/free-list allocator; programs may
  install their own or use scoped **arenas**:

  ```zexi
  let arena = Arena::new();
  let s = arena.str("temporary");   // freed when `arena` drops
  ```

## 3.6 Slices, bounds checks, and safety

- `[T]` is `(ptr, len)`; indexing `xs[i]` **bounds-checks** and panics on
  failure.
- Checks are **elided** when the compiler proves `i < len` (loop-range
  reasoning, M2). Elision is an optimization; soundness never depends on it.
- Integer arithmetic **checks overflow in debug** (panic) and wraps in release;
  `+%`, `+%`-family or `Wrapping<T>` opt into wrap everywhere. (Div-by-zero and
  `isize::MIN / -1` always trap.)
- `char` is a valid Unicode scalar; `str` is guaranteed valid UTF-8 at the type
  boundary.

## 3.7 Concurrency safety

**Today (preview).** The `thread` module ships real OS threads, `i64` channels,
and lock-free `i64` atomics (see [`11-concurrency.md`](11-concurrency.md)). Only
`i64` values cross a thread boundary — arguments, return values, channel
payloads, atomic cells, and the integer handles to the primitives themselves. No
`String`, struct, enum, or reference can be sent to or shared with another
thread, so there is no shared Zexi memory to race on; the only shared state is
the C-side table of channels/atomics, which is mutex- or atomic-guarded. That
makes this subset data-race-free without a `Send`/`Sync` checker.

**Target (M2).** The full model lets owned values and shared references cross
threads, gated by the same aliasing rules:

- A value can be sent to another thread only if it is `Send`.
- A value can be shared by `&T` across threads only if `T: Sync`.
- Interior mutability (`Cell`, `RefCell`, `Mutex`) routes unsafe mutation through
  checked APIs; `RefCell` can panic at runtime, `Mutex` uses OS primitives.

## 3.8 `unsafe` — the escape hatch

`unsafe { }` unlocks: dereferencing `*T`, calling `unsafe fn`, accessing
`union` fields, implementing `unsafe trait`, and `extern` FFI. Everything else
is unchanged. Safety invariants that `unsafe` code must uphold are documented per
API ("Safety" doc section, and flagged by a lint if missing). The soundness
argument for Zexi is: **all memory unsafety in a program is syntactically
confined to `unsafe` blocks.**

## 3.9 Owned enum payloads and `match`

Enum variants may own resources. Constructing a variant moves its arguments
into the enum; dropping the enum drops the active variant's payload.

A by-value `match` over an enum that owns memory **consumes** the scrutinee.
An arm that binds an owned payload moves it out of the scrutinee and drops it
at the end of the arm; the scrutinee's own destructor then runs once and frees
only what the arm did not take. A guarded arm binds a shallow copy for the
guard and takes ownership only once the guard passes, so a failing guard
leaves the scrutinee intact for the next arm.

```zexi
enum Msg { Text(String), Ping }

match m {
    Msg::Text(s) => println(s),   // `s` owns the buffer here
    Msg::Ping => {}
}                                  // `m` is dropped once, after the arm
```

A payload cannot be moved out through a borrow: matching a `&Enum` and binding
an owned payload reports `E0507`. Match by value, or use a non-owning pattern.

## 3.10 The runtime (or lack of one)

The M0/M1 runtime is what the language cannot lower to inline code:

| Component | Size | Notes |
| --- | --- | --- |
| `_start` / entry trampoline | ~200 B | calls `main`, then `exit` |
| panic handler + `abort` | ~300 B | message fmt optional, gated by feature |
| global allocator (opt-in) | ~1 KB | bump + free list over `mmap` |
| formatting machinery (opt-in) | ~2 KB | no reflection; monomorphized |
| libc/syscall shim (opt-in) | ~0.5 KB | direct `syscall` when freestanding |

A `#![no_std]` program that only computes and writes bytes via syscalls should
link a runtime under **2 KB** and a stripped binary under **20 KB** on x86-64.
That is the concrete size target (see docs/05).
