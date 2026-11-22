/* bench/fib/fib.c — the same benchmark in plain C. argv[1] is the scale.
   Must print the same `fib <scale> <checksum>` line as fib.zx and fib.rs. */
#include <stdio.h>
#include <stdlib.h>
#include <stdint.h>

static int64_t fib(int64_t n) {
    if (n < 2) return n;
    return fib(n - 1) + fib(n - 2);
}

int main(int argc, char **argv) {
    int64_t scale = argc > 1 ? atoll(argv[1]) : 1;
    if (scale < 1) scale = 1;
    int64_t n = 34 + scale % 2;
    int64_t acc = 0;
    for (int64_t i = 0; i < 12 * scale; i++) acc += fib(n);
    printf("fib %lld %lld\n", (long long)scale, (long long)acc);
    return 0;
}
