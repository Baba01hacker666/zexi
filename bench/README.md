# Zexi benchmarks

Small, honest benchmarks: each one exists as a Zexi program and as the same
program in plain C, both compiled by the same C compiler at `-O2`, both
required to print the same checksum. The harness (`run_bench.py`) refuses to
rank a pair whose outputs differ — a faster program that computes something
else is not a result.

The rules are strict on purpose: whole-process wall time (startup included;
the programs are sized so it is a rounding error), checksum agreement between
implementations, and the same toolchain for every row.

## Benchmarks

| name | what it exercises |
| --- | --- |
| `fib` | recursive call overhead (naive fibonacci) |
| `loops` | integer loop arithmetic and modulo |
| `sort` | in-place quicksort over a large `i64` array (bounds-checked indexing) |
| `matmul` | f64 matrix multiply over flat arrays |
| `strbuild` | owning `String` growth: repeated formatted appends |
| `match` | `match` dispatch in a hot state machine |

Each directory holds `<name>.zx` and `<name>.c`, with the checksum rule
written at the top of both.

## Running

```sh
python3 bench/run_bench.py                # build, verify, time, write results.md
python3 bench/run_bench.py --only fib     # one benchmark
python3 bench/run_bench.py --reps 5       # more timing rounds
python3 bench/run_bench.py --exe ./zexic  # a prebuilt compiler
```

`zexic` is built once with `go build` unless `--exe` is given; the C twins
are built with `cc -std=gnu11 -O2 -fwrapv`, the flags `zexic` itself passes
to the C compiler. Results land in [`results.md`](results.md) with the
machine's details — compare rows only on the machine they name.

## What the numbers mean (and do not)

Zexi compiles to C, so the honest expectation is **near-C speed**: the
generated code passes through the same optimizer. The interesting questions
are what the language *costs* — bounds checks, move/destructor glue, `String`
growth, monomorphized generics — and whether that cost stays small.

These are microbenchmarks with fixed sizes and one machine. They are a
regression net and a sanity check on the "as fast as Rust" pitch, not a
published ranking of languages. When a Zexi row loses to C, the row stays.
