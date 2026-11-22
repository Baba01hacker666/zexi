/* bench/loops/loops.c — the same benchmark in plain C. argv[1] is the
   scale. Must print the same `loops <scale> <checksum>` line as loops.zx
   and loops.rs. */
#include <stdio.h>
#include <stdlib.h>
#include <stdint.h>

int main(int argc, char **argv) {
    int64_t scale = argc > 1 ? atoll(argv[1]) : 1;
    if (scale < 1) scale = 1;
    int64_t acc = 0;
    for (int64_t i = 0; i < 5000 * scale; i++) {
        for (int64_t j = 0; j < 5000; j++) {
            acc = (acc + (i * 31 + j) % 1000003) % 1000003;
        }
    }
    printf("loops %lld %lld\n", (long long)scale, (long long)acc);
    return 0;
}
