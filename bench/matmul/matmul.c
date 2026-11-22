/* bench/matmul/matmul.c — the same benchmark in plain C. argv[1] is the
   scale. Must print the same `matmul <scale> <checksum>` line as matmul.zx
   and matmul.rs. */
#include <stdio.h>
#include <stdlib.h>
#include <stdint.h>

#define N 256

int main(int argc, char **argv) {
    int64_t scale = argc > 1 ? atoll(argv[1]) : 1;
    if (scale < 1) scale = 1;
    static double a[N * N], b[N * N], c[N * N] = {0};
    for (size_t i = 0; i < N * N; i++) {
        a[i] = (double)(i % 7) * 0.5;
        b[i] = (double)(i % 13) * 0.25;
    }
    for (int64_t round = 0; round < 20 * scale; round++) {
        for (size_t r = 0; r < N; r++) {
            for (size_t k = 0; k < N; k++) {
                double av = a[r * N + k];
                for (size_t col = 0; col < N; col++) {
                    c[r * N + col] = c[r * N + col] + av * b[k * N + col];
                }
            }
        }
    }
    double sum = 0.0;
    for (size_t i = 0; i < N * N; i++) sum += c[i];
    printf("matmul %lld %lld\n", (long long)scale, (long long)(sum * 8.0));
    return 0;
}
