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
