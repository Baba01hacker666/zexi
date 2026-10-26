/* bench/fib/fib.c — the same benchmark in plain C. Must print `fib 17108661`. */
#include <stdio.h>
#include <stdint.h>

static int64_t fib(int64_t n) {
    if (n < 2) return n;
    return fib(n - 1) + fib(n - 2);
}

int main(void) {
    int64_t acc = 0;
    for (int64_t i = 0; i < 12; i++) acc += fib(34);
    printf("fib %lld\n", (long long)acc);
    return 0;
}
