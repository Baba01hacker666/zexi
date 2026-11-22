# Zexi benchmarks

generated 2026-11-22 12:43 · Linux x86_64 · 4.15.0-213-generic
cc: cc (Ubuntu 13.3.0-6ubuntu2~24.04.1) 13.3.0
rustc: rustc 1.100.0 (0ef1bc386 2026-11-09)

Whole-process wall time and peak RSS (VmHWM sampled while the process runs), best of 3 timed runs after 1 warmup run(s). Sizes are scale factors passed as argv[1]. `C/zexi` and `Rust/zexi` above 1.0 mean Zexi was faster. Each row's implementations print the same checksum; a row that does not is marked, not ranked.

| benchmark | size | zexi | C | Rust | C/zexi | Rust/zexi | zexi RSS | C RSS | Rust RSS | note |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| fib | 1 | 297.4 ms | 276.7 ms | — | 1.07 | — | 1064 KiB | 1060 KiB | — |  |
| fib | 2 | 402.9 ms | 376.7 ms | — | 1.07 | — | 1076 KiB | 1060 KiB | — |  |
| fib | 4 | 814.0 ms | 748.5 ms | — | 1.09 | — | 1076 KiB | 1084 KiB | — |  |
| loops | 1 | 81.7 ms | 81.8 ms | 85.3 ms | 1.00 | 0.96 | 1100 KiB | 1168 KiB | 1172 KiB |  |
| loops | 2 | 162.9 ms | 167.4 ms | 170.2 ms | 0.97 | 0.96 | 1084 KiB | 1088 KiB | 1260 KiB |  |
| loops | 4 | 330.4 ms | 327.2 ms | 337.2 ms | 1.01 | 0.98 | 1168 KiB | 1104 KiB | 1196 KiB |  |
| sort | 1 | 149.0 ms | 134.7 ms | 126.8 ms | 1.11 | 1.17 | 4072 KiB | 4240 KiB | 5116 KiB |  |
| sort | 2 | 298.2 ms | 268.8 ms | 251.9 ms | 1.11 | 1.18 | 4092 KiB | 4204 KiB | 5120 KiB |  |
| sort | 4 | 595.7 ms | 543.2 ms | 500.0 ms | 1.10 | 1.19 | 4124 KiB | 4180 KiB | 5196 KiB |  |
| matmul | 1 | 101.0 ms | 99.5 ms | 90.7 ms | 1.01 | 1.11 | 2524 KiB | 2752 KiB | 3628 KiB |  |
| matmul | 2 | 199.9 ms | 201.3 ms | 181.6 ms | 0.99 | 1.10 | 3056 KiB | 2756 KiB | 3624 KiB |  |
| matmul | 4 | 407.8 ms | 401.4 ms | 367.3 ms | 1.02 | 1.11 | 2520 KiB | 2760 KiB | 3608 KiB |  |
| strbuild | 1 | 194.6 ms | 160.9 ms | 112.7 ms | 1.21 | 1.73 | 10728 KiB | 10904 KiB | 11492 KiB |  |
| strbuild | 2 | 385.0 ms | 343.9 ms | 228.9 ms | 1.12 | 1.68 | 20808 KiB | 20692 KiB | 21260 KiB |  |
| strbuild | 4 | 682.9 ms | 663.0 ms | 414.7 ms | 1.03 | 1.65 | 40988 KiB | 41036 KiB | 41564 KiB |  |
| match | 1 | 399.3 ms | 410.3 ms | 389.6 ms | 0.97 | 1.02 | 1084 KiB | 1072 KiB | 1176 KiB |  |
| match | 2 | 671.4 ms | 805.1 ms | 725.9 ms | 0.83 | 0.92 | 1096 KiB | 1160 KiB | 1180 KiB |  |
| match | 4 | 1461.3 ms | 1410.4 ms | 1611.2 ms | 1.04 | 0.91 | 1160 KiB | 1080 KiB | 1196 KiB |  |
| bounds | 1 | 12.1 ms | 9.9 ms | — | 1.22 | — | 5132 KiB | 5276 KiB | — |  |
| bounds | 2 | 22.1 ms | 17.9 ms | — | 1.23 | — | 5176 KiB | 5268 KiB | — |  |
| bounds | 4 | 41.4 ms | 32.0 ms | — | 1.30 | — | 5156 KiB | 5252 KiB | — |  |
