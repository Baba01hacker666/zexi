/* cbridge_glue.c — the C side of examples/cbridge.zx.
 *
 * Compile it into the program with:
 *   zexic run examples/cbridge.zx --csrc=examples/cbridge_glue.c --link=m
 */
#include <string.h>

int zx_add(int a, int b) { return a + b; }

/* Length of s plus room for one '!'. */
int zx_exclaim_len(const char *s) { return (int)strlen(s) + 1; }

int zx_count_a(const char *s) {
    int n = 0;
    for (; *s; s++) {
        if (*s == 'a') n++;
    }
    return n;
}

const char *zx_motto(void) { return "fast and small"; }
