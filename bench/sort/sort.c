/* bench/sort/sort.c — the same benchmark in plain C. */
#include <stdio.h>
#include <stdint.h>

#define N 400000

static void quicksort(int64_t *a, int64_t lo, int64_t hi) {
    if (lo >= hi) return;
    int64_t pivot = a[hi];
    int64_t i = lo, j = hi - 1;
    while (i <= j) {
        if (a[i] < pivot) {
            i++;
        } else {
            int64_t t = a[i];
            a[i] = a[j];
            a[j] = t;
            j--;
        }
    }
    int64_t t = a[i];
    a[i] = a[hi];
    a[hi] = t;
    quicksort(a, lo, i - 1);
    quicksort(a, i + 1, hi);
}

int main(void) {
    static int64_t a[N];
    int64_t checksum = 0;
    for (int64_t round = 0; round < 4; round++) {
        int64_t x = 12345 + round;
        for (int64_t i = 0; i < N; i++) {
            x = x * INT64_C(6364136223846793005) + INT64_C(1442695040888963407);
            a[i] = (x & 0x7fffffffffffffff) % 1000000;
        }
        quicksort(a, 0, N - 1);
        checksum += a[0] + a[N / 2] + a[N - 1];
        for (int64_t i = 0; i < N; i++) checksum += a[i] % 97;
    }
    printf("sort %lld\n", (long long)checksum);
    return 0;
}
