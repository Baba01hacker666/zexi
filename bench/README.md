# Zexi benchmarks

Small, honest benchmarks. Each one exists as a Zexi program and as the same
program in plain C, with a Rust twin where the toolchain is present. Every
implementation is built by its own compiler at the same optimization level and
must print the same checksum line at the same size. The harness
(`run_bench.py`) refuses to rank a row whose outputs differ — a faster program
that computes something else is not a result.

The rules are strict on purpose: whole-process wall time (startup included),
one untimed warmup run per binary before the timed rounds, per-process peak
RSS from `wait4`, checksum agreement between implementations, and the same
numbers for the Zexi and C rows.

## Benchmarks

| name | what it exercises |
| --- | --- |
| `fib` | recursive call overhead (naive fibonacci) |
| `loops` | integer loop arithmetic and modulo |
| `sort` | in-place quicksort over a large `i64` array (bounds-checked indexing) |
| `matmul` | f64 matrix multiply over flat arrays |
| `strbuild` | owning `String` growth: repeated formatted appends |
| `match` | `match` dispatch in a hot state machine |
| `bounds` | `for i in 0..len` indexing over a fixed array and a shared slice |

Each directory holds `<name>.zx` and `<name>.c`, plus `<name>.rs` when a Rust
twin exists, with the checksum rule written at the top of the sources.

`fib` deliberately has no Rust twin: LLVM rewrites that exponential recursion
into a linear loop, so the Rust build would print the same checksum while doing
a small fraction of the work. The C and Zexi builds keep the recursion.

## Sizes

Every program takes an optional first argument, the **scale factor**, and
repeats its workload that many times. The default is 1, so the checksums in the
source comments describe the default run; each size prints
`<name> <scale> <checksum>`. This keeps memory use fixed while time scales,
which is what the harness needs to compare rows at different sizes.

## Running

```sh
python3 bench/run_bench.py                     # sizes 1,2,4, three timed rounds
python3 bench/run_bench.py --only fib,sort     # a subset
python3 bench/run_bench.py --sizes 1,3         # chosen scale factors
python3 bench/run_bench.py --reps 5            # more timing rounds
python3 bench/run_bench.py --warmup 2          # more warmup runs
python3 bench/run_bench.py --exe ./zexic       # a prebuilt compiler
```

`zexic` is built once with `go build` unless `--exe` is given; the C twins
are built with `cc -std=gnu11 -O2 -fwrapv`, the flags `zexic` itself passes
to the C compiler. Rust twins are built with `rustc -O` when `rustc` is on
`PATH` (or `--rustc` names one); without it those columns stay empty. Results
land in [`results.md`](results.md) and [`results.json`](results.json), both
recording the machine's details — compare rows only on the machine they name.
`results.json` carries the same rows with per-implementation best/median
milliseconds, peak RSS, and checksum, for tooling.

## Bounds-check elimination

`bench/bounds` is the target of the conservative bounds-check elimination in
the C backend: the accumulation loop indexes a shared slice with
`for i in 0..s.len()`, which the compiler can prove in range without the C
optimizer's help. `zexic build bench/bounds/bounds.zx --report-bounds` reports
`2 elided, 0 kept`. At `-O0` this is a large win (the helper call disappears);
at `-O2` GCC already removes the check from this shape, so the row measures
the same work either way.

## HTTP server load test

`http_bench.zx` is a small dependency-free HTTP/1.1 server (GET `/`, `/json`,
`/big` 64 KiB, POST `/echo`) used to measure how much the stdlib server in
`internal/driver/std/http.zx` can handle at scale. It serves with a worker pool
and a request budget so every run exits deterministically:

```sh
zexic build bench/http_bench.zx -o build/bench/http_bench
./build/bench/http_bench 18200 4 100000   # <port> <workers> <request budget>
```

`http_load.py` drives it over raw sockets (no client-side keep-alive reuse
unless asked) and writes a markdown report:

```sh
python3 bench/http_load.py                                  # full sweep
python3 bench/http_load.py --quick --total 2000             # smoke run
python3 bench/http_load.py --concurrency 1,8,32,128 --workers 1,4 --total 8000 \
    --out bench/http-results.md
```

Phases: keep-alive throughput vs concurrency × workers (with per-request
p50/p95/p99, a server-side request-count cross-check, and the server process's
CPU cores + peak RSS sampled from `/proc` while the load runs), close-mode
comparison, 64 KiB GET, 1 KiB POST echo, a sustained 5 s resource measurement,
a slowloris probe (stalled half-sent heads vs one normal request), and abuse
probes (431/400/silent). Results land in [`http-results.md`](http-results.md);
compare rows only on the machine they name.

`http_bench.c` is a raw C twin of `http_bench.zx` — same routes, CLI, exit
line, keep-alive/deadline behaviour — so the two implementations can be
measured identically and compared (CPU/RSS included):

```sh
python3 bench/http_load.py --server c --total 8000 --concurrency 1,8,32,128 \
    --workers 1,4 --out bench/http-c-results.md
```

On the 2-core test box both servers land in the same throughput ballpark at
~1 MiB RSS, but the zexi server uses ~1.7× more CPU per request (see the
comparison table in `docs/08-networking.md` §8.7 and
[`http-c-results.md`](http-c-results.md)).

## What the numbers mean (and do not)

Zexi compiles to C, so the honest expectation is **near-C speed**: the
generated code passes through the same optimizer. The interesting questions
are what the language *costs* — bounds checks, move/destructor glue, `String`
growth, monomorphized generics — and whether that cost stays small.

These are microbenchmarks with fixed sizes and one machine. They are a
regression net and a sanity check on the "as fast as Rust" pitch, not a
published ranking of languages. When a Zexi row loses to C, the row stays.
