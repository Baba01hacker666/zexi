/* bench/loops/loops.c — the same benchmark in plain C. */
#include <stdio.h>
#include <stdint.h>

int main(void) {
    int64_t acc = 0;
    for (int64_t i = 0; i < 5000; i++) {
        for (int64_t j = 0; j < 5000; j++) {
            acc = (acc + (i * 31 + j) % 1000003) % 1000003;
        }
    }
    printf("loops %lld\n", (long long)acc);
    return 0;
}
