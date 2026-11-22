/* bench/bounds/bounds.c — the same indexing benchmark in plain C. The length
   is derived from argc so the optimizer treats it as unknown, like a slice
   length at run time. argv[1] is the scale. Must print the same
   `bounds <scale> <checksum>` line as bounds.zx. */
#include <stdio.h>
#include <stdlib.h>
#include <stdint.h>

#define N 524288

int main(int argc, char **argv) {
    int64_t scale = argc > 1 ? atoll(argv[1]) : 1;
    if (scale < 1) scale = 1;
    static int64_t a[N];
    for (size_t i = 0; i < N; i++) a[i] = (int64_t)i % 97;
    const int64_t *s = a;
    /* A runtime length the optimizer cannot fold, like a slice length. */
    size_t n = (size_t)N - (size_t)(argc < 0);
    int64_t acc = 0;
    for (int64_t pass = 0; pass < 64 * scale; pass++) {
        for (size_t i = 0; i < n; i++) acc += s[i];
    }
    printf("bounds %lld %lld\n", (long long)scale, (long long)acc);
    return 0;
}
