# HTTP server load test — 2026-10-31

Machine: x86_64, 2 cpu(s), Python 3.11.2. Server: `bench/http_bench.zx` built by zexic (C backend, -O2), worker pool via `listen_workers_n`, HTTP/1.1 keep-alive. Load: `bench/http_load.py` raw sockets, 8000 reqs per throughput cell.

## slowloris probe (stalled half-sent heads)

| workers | stalled heads | normal request status | latency ms |
| ---: | ---: | ---: | ---: |
| 2 | 8 | 200 | 4423 |
| 8 | 8 | 200 | 1076 |

## results

| load | requests | ok | errors | req/s | p50 ms | p95 ms | p99 ms | wall s |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| GET / keep-alive workers=1 c=1 | 8000 | 8000 | 0 | 33233 | 0.02 | 0.03 | 0.06 | 0.24 |
| GET / keep-alive workers=1 c=8 | 8000 | 8000 | 0 | 31323 | 0.02 | 0.05 | 0.08 | 0.26 |
| GET / keep-alive workers=1 c=32 | 8000 | 8000 | 0 | 24663 | 0.02 | 0.07 | 0.12 | 0.32 |
| GET / keep-alive workers=1 c=128 | 8000 | 8000 | 0 | 16633 | 0.02 | 0.05 | 0.15 | 0.48 |
| GET / keep-alive workers=4 c=1 | 8000 | 8000 | 0 | 25995 | 0.02 | 0.05 | 0.15 | 0.31 |
| GET / keep-alive workers=4 c=8 | 8000 | 8000 | 0 | 28312 | 0.06 | 0.24 | 1.43 | 0.28 |
| GET / keep-alive workers=4 c=32 | 8000 | 8000 | 0 | 41790 | 0.02 | 0.06 | 0.16 | 0.19 |
| GET / keep-alive workers=4 c=128 | 8000 | 8000 | 0 | 27652 | 0.02 | 0.04 | 0.10 | 0.29 |
| GET / close-mode workers=1 c=32 | 8000 | 8000 | 0 | 12597 | 2.01 | 2.83 | 4.64 | 0.64 |
| GET / close-mode workers=4 c=32 | 8000 | 8000 | 0 | 12690 | 1.64 | 3.86 | 7.72 | 0.63 |
| GET /big keep-alive workers=4 c=16 | 1600 | 1600 | 0 | 12078 | 0.05 | 0.13 | 0.60 | 0.13 |
| POST /echo workers=4 c=16 | 1600 | 1600 | 0 | 13258 | 0.02 | 0.06 | 0.13 | 0.12 |

