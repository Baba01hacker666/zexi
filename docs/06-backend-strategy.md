# 6. Backend Strategy: why Zexi emits C, and the road off it

This document exists because the M0 backend is easy to misread. Zexi's compiler
currently **translates a Zexi program to C, then invokes the system C compiler to
produce the native binary**. This chapter states exactly what that means, why it
is a defensible milestone rather than a shortcut, its real costs, and the plan
to replace it.

## 6.1 What actually happens today

```
hello.zx ──zexic──▶ hello.c ──cc (gcc/clang)──▶ hello   (native ELF, no VM)
```

The C compiler is used as a **code generator and assembler/linker**, the way a
compiler normally uses LLVM. The result is a real, standalone native executable:
there is no interpreter, no bytecode, and no runtime image. Verify it:

```sh
zexic emit --emit=c examples/hello.zx    # see the generated C
zexic build examples/hello.zx -o hello   # compile it
file hello                               # ELF 64-bit ... executable
```

What is *not* true: Zexi is not "a C preprocessor" in the sense of textually
pasting code. It parses and type-checks Zexi, then lowers a resolved, typed
program into C. C is an intermediate representation that happens to have a
mature compiler attached.

## 6.2 Why we started here

| Benefit | Detail |
| --- | --- |
| Instant good codegen | We inherit register allocation, instruction selection, and decades of optimization. "Rust-like speed" without writing a register allocator first. |
| Small binaries early | `-O2`/`-Os`, section GC, and LTO come for free; `hello.zx` links in **~18 KB**. |
| Portable target | x86-64, AArch64, RISC-V, Windows, wasm — all reachable before we write a single instruction encoder. |
| Debuggable | Emitted C is readable; you can step through it with gdb and inspect the lowering. |
| Focused effort | We spend our time on the parts only Zexi can have: the ownership/borrow checker and the language design. |

This is the same route **Nim**, **Vala**, and **V** took: compile to C, benefit
from its toolchain, then progressively own more of the backend.

## 6.3 The honest costs

1. **Toolchain dependency.** Users need a C compiler installed. A language that
   claims to be self-contained should not require one forever.
2. **Semantic impedance.** C has undefined behavior and a coercing type system.
   Zexi guarantees (no UB in safe code, exact layout, deterministic drops) must
   be *encoded* in C and can be violated by a bad lowering. This is a soundness
   risk, mitigated by keeping all `unsafe`/UB-prone lowering in one backend and
   testing it against a conformance suite.
3. **GNU extensions.** Value-producing `if`/block expressions lower to GNU
   statement expressions `({ ... })`. They work on gcc/clang but **not MSVC**,
   and they are not standard C11. This is the biggest wart.
4. **Opaque failure modes.** When the C compiler rejects generated code, the user
   sees a C error, not a Zexi diagnostic. (We translate and re-word where we can.)
5. **Two IRs to reason about.** C's optimizer can undo layout assumptions, and
   profiling/debug info describes C, not Zexi.

None of these are acceptable as *permanent* properties. They are acceptable as
*M0* properties.

## 6.4 The options at M2

| Option | Effort | Control | Notes |
| --- | --- | --- | --- |
| **Stay on C** | low | low | Fastest iteration; keeps the toolchain dependency and GNU-extension wart forever. |
| **LLVM backend** | medium | high | Emit LLVM IR behind the MIR contract. Removes the C wart, keeps a large external dependency (and `llvm-dev`). |
| **Native emitter** | high | total | We emit x86-64 / AArch64 ourselves (or use Cranelift). No external codegen dependency, full control of layout and size. This is the endgame for a self-respecting systems language. |
| **Cranelift** | medium | high | A library backend; Rust-native, fast compiles, no C. A good middle path. |

**Decision:** keep the C backend through M0 and M1 to move fast on the front end,
then build the **native backend at M2** behind the same backend contract, with
**Cranelift as the preferred intermediate step** if the hand-written emitter is
too slow to land. The C backend then becomes an optional, portable fallback for
platforms we have not handwritten yet — never the only path.

## 6.5 What makes this a real plan and not a hope

The architecture already isolates the choice. `docs/04 §4.3` fixes a backend
contract: a backend consumes MIR plus a layout table and must be able to emit
functions, statics, drop glue, and the panic entry. The C backend is one
implementation of that contract.

Work required to honor the plan:

1. **Land MIR.** Today the C backend walks the typed AST directly (documented as
   an M0 shortcut). M1 introduces MIR so backends are swappable and
   ownership-driven optimizations (drop elision, bounds-check elision,
   escape-to-stack) run *before* any backend.
2. **Add the backend interface** (`internal/codegen.Backend`) and a
   conformance suite that runs every backend against the same programs and
   compares observable behavior. The C backend is tested by it first.
3. **Grow the native emitter** target by target (start x86-64 SysV), keeping the
   C backend green as the reference.

Until step 3 ships, `--cc` selects the C compiler and `--emit=c` remains the
supported way to inspect lowering. After it ships, `--backend=native|c` selects
between them and the default flips to `native`.
