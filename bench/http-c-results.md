# HTTP server load test (raw C) — 2026-10-31

Machine: x86_64, 2 cpu(s), Python 3.11.2. Server: `bench/http_bench.c` (raw C twin), `cc -std=gnu11 -O2 -pthread`, worker pool, HTTP/1.1 keep-alive. Load: `bench/http_load.py` raw sockets, 8000 reqs per throughput cell. cpu cores = /proc jiffies over wall time; rss MiB = server peak VmHWM.

## resources (sustained 5 s load; cpu from /proc jiffies, rss = VmHWM)

| workers | concurrency | req/s | ok | errors | cpu cores | cpu % | rss end MiB | rss peak MiB |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 4 | 128 | 39217 | 196439 | 0 | 0.42 | 21 | 0.9 | 0.9 |

## slowloris probe (stalled half-sent heads)

| workers | stalled heads | normal request status | latency ms |
| ---: | ---: | ---: | ---: |
| 2 | 8 | 200 | 3955 |
| 8 | 8 | 200 | 951 |

## results

| load | requests | ok | errors | req/s | p50 ms | p95 ms | p99 ms | wall s | cpu cores | rss MiB |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| GET / keep-alive workers=1 c=1 | 8000 | 8000 | 0 | 18435 | 0.06 | 0.08 | 0.09 | 0.43 | 0.44 | 0.0 |
| GET / keep-alive workers=1 c=8 | 8000 | 8000 | 0 | 21592 | 0.04 | 0.09 | 0.13 | 0.37 | 0.38 | 0.0 |
| GET / keep-alive workers=1 c=32 | 8000 | 8000 | 0 | 20787 | 0.04 | 0.06 | 0.10 | 0.38 | 0.42 | 0.0 |
| GET / keep-alive workers=1 c=128 | 8000 | 8000 | 0 | 25324 | 0.04 | 0.07 | 134.31 | 0.32 | 0.32 | 0.0 |
| GET / keep-alive workers=4 c=1 | 8000 | 8000 | 0 | 15770 | 0.07 | 0.08 | 0.10 | 0.51 | 0.45 | 1.2 |
| GET / keep-alive workers=4 c=8 | 8000 | 8000 | 0 | 31719 | 0.09 | 0.33 | 0.53 | 0.25 | 0.40 | 1.2 |
| GET / keep-alive workers=4 c=32 | 8000 | 8000 | 0 | 30743 | 0.08 | 0.32 | 1.00 | 0.26 | 0.35 | 1.1 |
| GET / keep-alive workers=4 c=128 | 8000 | 8000 | 0 | 29761 | 0.07 | 0.48 | 58.90 | 0.27 | 0.30 | 1.2 |
| GET / close-mode workers=1 c=32 | 8000 | 8000 | 0 | 16115 | 1.59 | 3.52 | 5.17 | 0.50 | 0.22 | 0.0 |
| GET / close-mode workers=4 c=32 | 8000 | 8000 | 0 | 10415 | 2.33 | 6.41 | 9.35 | 0.77 | 0.18 | 1.1 |
| GET /big keep-alive workers=4 c=16 | 1600 | 1600 | 0 | 26880 | 0.05 | 0.32 | 1.08 | 0.06 | 0.17 | 1.1 |
| POST /echo workers=4 c=16 | 1600 | 1600 | 0 | 24608 | 0.09 | 0.48 | 2.59 | 0.07 | 0.15 | 1.1 |

