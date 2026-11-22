/* bench/match/match.c — the same benchmark in plain C. argv[1] is the scale.
   Must print the same `match <scale> <checksum>` line as match.zx and
   match.rs. Signed overflow wraps (`-fwrapv`), matching the other twins. */
#include <stdio.h>
#include <stdlib.h>
#include <stdint.h>

int main(int argc, char **argv) {
    int64_t scale = argc > 1 ? atoll(argv[1]) : 1;
    if (scale < 1) scale = 1;
    int64_t acc = 1;
    for (int64_t k = 0; k < 50000000 * scale; k++) {
        int64_t v = (k * 7 + 13) % 101;
        switch (k & 7) {
            case 0: acc = acc + v; break;
            case 1: acc = acc - v; break;
            case 2: acc = acc * 3 + v; break;
            case 3: acc = acc ^ v; break;
            case 4: acc = (acc * 2) ^ v; break;
            case 5: acc = -acc + v; break;
            case 6: acc = acc + v * 2; break;
            default: acc = acc - v * 3; break;
        }
    }
    printf("match %lld %lld\n", (long long)scale, (long long)acc);
    return 0;
}
