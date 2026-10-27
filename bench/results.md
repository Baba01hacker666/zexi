# Zexi benchmarks

generated 2026-10-26 · Linux e2b.local 6.1.158+ · cc (Debian 12.2.0-14+deb12u1) 12.2.0

Wall time of the whole process, best and median of 3 runs. `C/zexi` above 1.0 means the Zexi build was faster. Each row's two implementations print the same checksum; a row that does not is marked, not ranked.

| benchmark | zexi best | zexi med | C best | C med | C/zexi | note |
| --- | --- | --- | --- | --- | --- | --- |
| fib | 131.4 ms | 132.9 ms | 131.2 ms | 135.7 ms | 1.00 |  |
| loops | 92.9 ms | 93.3 ms | 92.8 ms | 93.4 ms | 1.00 |  |
| sort | 148.2 ms | 149.2 ms | 133.6 ms | 134.6 ms | 0.90 |  |
| matmul | 121.3 ms | 148.1 ms | 108.8 ms | 125.2 ms | 0.90 |  |
| strbuild | 104.6 ms | 116.2 ms | 95.5 ms | 96.3 ms | 0.91 |  |
| match | 97.9 ms | 98.2 ms | 99.9 ms | 103.0 ms | 1.02 |  |
