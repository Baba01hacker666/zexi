# 18. One C file, or many? (measured)

The question this document answers: *should the C that libraries ship all be
stuffed into one C file?* It is asked often enough, and the answer differs
depending on which of two things "one C file" means — so this separates them and
backs both with measurements taken on `gcc 12.2`, `-std=gnu11 -O2 -fwrapv
-ffunction-sections -fdata-sections` (the exact flags `driver.CompileC` uses),
2 cores.

**Short answer:**

| Reading | Verdict |
| --- | --- |
| **Merge the C *sources* into one `.c` in this repo** (one `zxrt.c` that all modules `extern` into) | **No.** It destroys the property the extension ladder exists for. |
| **Concatenate the per-module C into one *translation unit* at build time** | **Yes**, that is what we do now, and it is right — but for a narrower reason than §17.2 claims. |

## 18.1 What "one file" means today

Every module that needs C keeps it in its own `.zx` file, in a `c """..."""`
block. `collectNative` (`internal/driver/driver.go`) gathers those blocks in
source order, deduplicates identical text, and appends the result to the
generated C — so at build time there is exactly **one** translation unit
(`FrontEndFiles`, `driver.go` ≈ lines 186–200). Six standard modules ship C:

| module | C lines | C bytes | what it is |
| --- | ---: | ---: | --- |
| `thread` | 664 | 18.4 KB | pthreads, mutexes, channels, worker pool |
| `net` | 496 | 16.4 KB | sockets, HTTP plumbing, file I/O |
| `sys` | 227 | 7.1 KB | POSIX env, paths, clocks, byte I/O |
| `json` | 89 | 2.7 KB | `strtod`/`strtoll`, `%.17g`, UTF-8 |
| `http` | 78 | 2.5 KB | router slots, counters, stop flag |
| `regex` | 57 | 1.8 KB | POSIX ERE bridge |
| **total** | **1 611** | **47.7 KB** | all of it lands in the program's single `.c` |

A program that imports all six carries 47.7 KB of library C in the same file as
its own generated code. That file is written to a temp dir and thrown away, so
"one file" is a build-time artefact, not a source layout.

## 18.2 Why the single unit earns its place

Not because it is tidier. Because the C is compiled **with the generated
declarations in scope**, so a signature that disagrees is a `cc` error instead
of undefined behaviour. §17.4 records what that found: `sys` and `json` had
`char *` parameters where the language passes `struct zx_arrN_u8 *`, and `net`
declared `void *` returns for what the language types `*Router`. Separate C
files compiled fine and misbehaved; one unit refused.

The same property is what makes a **C function body** (rung 5 in
[`17-extending.md`](17-extending.md)) cheap: a body written in `c """..."""` is
compiled in the same unit as its callers, so `cc` checks it against the generated
typedefs and inlines it, with no FFI boundary and no second file to keep in sync.

There is a second, quieter benefit that showed up in measurement: a single unit
lets `cc` see the whole call graph, so it can inline across function
boundaries. On a synthetic 4 000-function program (~1 MB of C), one unit took
**7.65 s** to compile and eight units took **0.40 s** — the 19× difference is
almost entirely the optimizer working with global knowledge. Split builds have
no LTO, so they emit slower code. That is a real trade, not a rounding error.
(It also means `--jobs` silently changes optimization quality, which is worth
knowing when a benchmark number moves.)

## 18.3 What it costs

Measured on the same flags, the 47.7 KB shell is not free:

| file | source | compile |
| --- | ---: | ---: |
| empty translation unit (floor) | 1 KB | 0.018 s |
| `regex` block only | 1 KB | 0.030 s |
| `json` | 2 KB | 0.042 s |
| `http` | 2 KB | 0.035 s |
| `sys` | 6 KB | 0.084 s |
| `net` | 16 KB | 0.144 s |
| `thread` | 17 KB | 0.157 s |
| 300-function program, no library C | 72 KB | 0.031 s |
| the same program + all six blocks | 120 KB | 0.388 s |

So the library C costs roughly **0.31–0.36 s** of `cc` time at `-O2`, dominated
by `net` and `thread`, and it is a fixed floor for any networked program. A
program that imports one small module pays 0.02–0.05 s and does not notice.

## 18.4 The real risks of pooling, and which are live

Pooling independent C into one unit removes the isolation the compiler would
otherwise give you. Three things can leak:

1. **`static` names.** Two blocks may each define `static int parse_number(...)`.
   In separate files that is fine; in one unit it is a hard error in every
   program that imports both. *Current state: 0 collisions across all six
   blocks* (checked). Nothing in the compiler enforces it — the guard is `cc`.
2. **Macros.** Macros defined by an earlier block are live for every later one,
   so a user's `c """#define min(a,b) ..."""` could rewrite library C below it.
   *Current state: safe by discipline* — the blocks only define `ZXS_*`,
   `ZXN_*`, `ZXT_*`, `ZXH_*`. Only `_POSIX_C_SOURCE` / `_DEFAULT_SOURCE` are
   shared, and they turn out to be **inert**: the generated preamble
   (`codegen.go` `preamble()`) already includes `<stdio.h>` and `<stdlib.h>`
   before any block, so feature-test macros declared inside a block come too
   late to matter — and `_DEFAULT_SOURCE` actually produces a
   `"_DEFAULT_SOURCE" redefined` warning because `-std=gnu11` predefines it.
   "A block must be self-contained" is therefore true for *headers* only.
3. **Compile-unit weight.** All inline C is appended to unit 0 alone
   (`driver.go` ≈ line 197), so in a `--jobs=8` build the 47.7 KB blob — plus
   whatever `net`'s 16 headers pull in — always sits on one worker while the
   other seven finish early. The critical path is a single core.

## 18.5 The part of §17.2 that is stronger than it needs to be

§17.2 says inline C is in the same translation unit *so that it can see the
runtime helpers and every generated struct*. For the standard library, only the
second half is load-bearing:

* every one of the six blocks references generated **types**
  (`zx_Router`, `zx_arrN_u8`, `zx_slice_u8`, `zx_RegexSpan`);
* **none** of them calls a generated function.

Generated types and prototypes are emitted by `emitPrefix`, which is the part
`GenerateSplit` puts in **`zexi_gen.h`**, a header every unit includes. So a
block could be its own translation unit — `#include "zexi_gen.h"` plus the
block — and keep the entire type-checking benefit that caught the ABI bugs.
Same-TU is required only for a block that *calls* generated `static` helpers,
which is a property of that block, not of the design.

## 18.6 Recommendation

1. **Keep the sources modular.** One `c """..."""` block per module, next to the
   `extern "C"` declarations it serves. A single `zxrt.c` would make `sys`'s C
   compile into every program that imports anything, break the dedup in
   `collectNative`, and require deleting `TestStandardLibraryNeedsNoCompilerC` —
   the test that keeps §17 honest. This is the change not to make.
2. **Keep concatenating at build time.** It is the cheapest way to get
   compile-time ABI agreement, and the cost above is a third of a second at
   worst.
3. **Guard the two leaks that are currently only guarded by luck.** A small
   test that assembles the blocks for a program importing every module and
   fails on (a) a `static` name defined twice and (b) a `#define` whose name is
   already defined by an earlier block, would turn §18.4 from discipline into a
   build error. This is a ~40-line test and the highest value-per-risk change
   available here.
4. **If compile time becomes the problem, do not answer it with "one file vs
   many".** The fix is to give each block its own unit that includes
   `zexi_gen.h` — which keeps the checking and gives `cc` eight independent
   files — and to stop pinning the blob to unit 0. Reserve the single-TU path
   for `zexic emit`, where a human reads it.

The invariant worth writing down: **library C must name its types from the
generated header, must not need generated function bodies, and must prefix every
symbol and macro it defines.** Given that, "one file or many" stops being a
correctness question and becomes only a compile-time-versus-optimization dial.
