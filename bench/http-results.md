# HTTP server load test (zexi) — 2026-10-31

Machine: x86_64, 2 cpu(s), Python 3.11.2. Server: `bench/http_bench.zx` built by zexic (C backend, -O2), worker pool via `listen_workers_n`, HTTP/1.1 keep-alive. Load: `bench/http_load.py` raw sockets, 8000 reqs per throughput cell. cpu cores = /proc jiffies over wall time; rss MiB = server peak VmHWM.

## resources (sustained 5 s load; cpu from /proc jiffies, rss = VmHWM)

| workers | concurrency | req/s | ok | errors | cpu cores | cpu % | rss end MiB | rss peak MiB |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 4 | 128 | 42036 | 210480 | 0 | 0.76 | 38 | 0.9 | 0.9 |

## slowloris probe (stalled half-sent heads)

| workers | stalled heads | normal request status | latency ms |
| ---: | ---: | ---: | ---: |
| 2 | 8 | 200 | 4430 |
| 8 | 8 | 200 | 1082 |

## results

| load | requests | ok | errors | req/s | p50 ms | p95 ms | p99 ms | wall s | cpu cores | rss MiB |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| GET / keep-alive workers=1 c=1 | 8000 | 8000 | 0 | 17304 | 0.06 | 0.09 | 0.12 | 0.46 | 0.54 | 0.0 |
| GET / keep-alive workers=1 c=8 | 8000 | 8000 | 0 | 12960 | 0.07 | 0.10 | 0.13 | 0.62 | 0.50 | 0.0 |
| GET / keep-alive workers=1 c=32 | 8000 | 8000 | 0 | 17502 | 0.05 | 0.08 | 0.13 | 0.46 | 0.50 | 0.0 |
| GET / keep-alive workers=1 c=128 | 8000 | 8000 | 0 | 16426 | 0.05 | 0.08 | 186.16 | 0.49 | 0.51 | 0.0 |
| GET / keep-alive workers=4 c=1 | 8000 | 8000 | 0 | 15646 | 0.07 | 0.09 | 0.12 | 0.51 | 0.55 | 0.9 |
| GET / keep-alive workers=4 c=8 | 8000 | 8000 | 0 | 41904 | 0.07 | 0.21 | 0.40 | 0.19 | 0.78 | 0.9 |
| GET / keep-alive workers=4 c=32 | 8000 | 8000 | 0 | 35739 | 0.06 | 0.30 | 0.93 | 0.22 | 0.67 | 0.9 |
| GET / keep-alive workers=4 c=128 | 8000 | 8000 | 0 | 33160 | 0.08 | 0.40 | 80.58 | 0.24 | 0.66 | 1.0 |
| GET / close-mode workers=1 c=32 | 8000 | 8000 | 0 | 11673 | 2.50 | 4.75 | 7.48 | 0.69 | 0.28 | 0.0 |
| GET / close-mode workers=4 c=32 | 8000 | 8000 | 0 | 11384 | 2.48 | 5.11 | 7.66 | 0.70 | 0.28 | 0.9 |
| GET /big keep-alive workers=4 c=16 | 1600 | 1600 | 0 | 19845 | 0.14 | 0.50 | 1.07 | 0.08 | 0.86 | 1.1 |
| POST /echo workers=4 c=16 | 1600 | 1600 | 0 | 41920 | 0.05 | 0.23 | 0.99 | 0.04 | 0.52 | 1.0 |

