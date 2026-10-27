/* bench/strbuild/strbuild.c — the same benchmark in plain C. */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>

int main(void) {
    size_t cap = 64, len = 0;
    char *s = malloc(cap);
    char buf[32];
    for (int64_t i = 0; i < 800000; i++) {
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
    printf("strbuild %zu %lld\n", len, (long long)h);
    free(s);
    return 0;
}
