/* bench/strbuild/strbuild.c — the same benchmark in plain C. argv[1] is the
   scale. Must print the same `strbuild <scale> <len> <hash>` line as
   strbuild.zx and strbuild.rs. */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>

int main(int argc, char **argv) {
    int64_t scale = argc > 1 ? atoll(argv[1]) : 1;
    if (scale < 1) scale = 1;
    size_t cap = 64, len = 0;
    char *s = malloc(cap);
    char buf[32];
    for (int64_t i = 0; i < 800000 * scale; i++) {
        int n = snprintf(buf, sizeof buf, "item %lld\n", (long long)i);
        if (len + (size_t)n > cap) {
            while (len + (size_t)n > cap) cap *= 2;
            s = realloc(s, cap);
        }
        memcpy(s + len, buf, (size_t)n);
        len += (size_t)n;
    }
    int64_t h = 0;
    for (size_t k = 0; k < len; k++) h = (h * 131 + (unsigned char)s[k]) % 2147483647;
    printf("strbuild %lld %zu %lld\n", (long long)scale, len, (long long)h);
    free(s);
    return 0;
}
