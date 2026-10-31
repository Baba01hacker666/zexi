// bench/http_bench.c — raw C twin of bench/http_bench.zx, for the head-to-head
// in bench/http_load.py --server c. Same routes, same keep-alive semantics,
// same CLI, same exit line, so the harness can measure both identically:
//
//     cc -std=gnu11 -O2 -pthread bench/http_bench.c -o build/bench/http_bench_c
//     ./build/bench/http_bench_c <port> <workers> <budget>
//
// `workers` 0 picks two threads per core; `budget` 0 serves until killed.
// Prints "rc 0 requests N conns K" on a budget shutdown.
//
// Deliberately the same shape as the zexi server: a worker pool accepting on
// one non-blocking listener, TCP_NODELAY on accepted sockets, head+body sent
// as two writes, a 1 s head deadline (stalled partial head -> 400 + close),
// 8 KiB head cap (-> 431), 5 s keep-alive idle, and a request-counted budget.

#define _GNU_SOURCE
#include <arpa/inet.h>
#include <errno.h>
#include <fcntl.h>
#include <netinet/in.h>
#include <netinet/tcp.h>
#include <poll.h>
#include <pthread.h>
#include <signal.h>
#include <stdatomic.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>

#define HEAD_MAX 8192
#define BODY_MAX (16 * 1024 * 1024)
#define FIRST_BYTE_MS 250
#define HEAD_TIMEOUT_MS 1000
#define KEEPALIVE_IDLE_MS 5000
#define POLL_MS 100

static atomic_long g_requests, g_conns;
static atomic_int g_stop;
static long g_budget;
static char g_big[65536];

static long now_ms(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return ts.tv_sec * 1000L + ts.tv_nsec / 1000000L;
}

static int write_all(int fd, const char *buf, size_t len) {
    while (len > 0) {
        ssize_t n = write(fd, buf, len);
        if (n < 0) {
            if (errno == EINTR) continue;
            return -1;
        }
        buf += n;
        len -= (size_t)n;
    }
    return 0;
}

static void send_simple(int fd, int code, const char *reason, const char *body, int keep) {
    char head[256];
    int hl = snprintf(head, sizeof head,
                      "HTTP/1.1 %d %s\r\nContent-Type: text/plain\r\n"
                      "Content-Length: %zu\r\nConnection: %s\r\n\r\n",
                      code, reason, strlen(body), keep ? "keep-alive" : "close");
    write_all(fd, head, (size_t)hl);
    write_all(fd, body, strlen(body));
}

// Find "\r\n\r\n" in buf[0..have); returns the offset of the first byte after
// the blank line, or -1.
static int head_end(const char *buf, int have) {
    if (have < 4) return -1;
    for (int i = 0; i + 3 < have; i++)
        if (buf[i] == '\r' && buf[i + 1] == '\n' && buf[i + 2] == '\r' && buf[i + 3] == '\n')
            return i + 4;
    return -1;
}

// Serve one connection with keep-alive. Returns after close/EOF/budget.
static void serve_conn(int fd) {
    char *buf = malloc(HEAD_MAX + BODY_MAX + 1);
    if (!buf) return;
    int have = 0;
    int first = 1;
    for (;;) {
        // --- read the head ---
        int hend = head_end(buf, have);
        long first_deadline = now_ms() + (first ? FIRST_BYTE_MS : KEEPALIVE_IDLE_MS);
        long head_deadline = 0;
        while (hend < 0) {
            if (have >= HEAD_MAX) {
                send_simple(fd, 431, "Request Header Fields Too Large", "", 0);
                goto out;
            }
            long now = now_ms();
            long wait;
            if (have == 0) {
                wait = first_deadline - now;
            } else {
                if (head_deadline == 0) {
                    head_deadline = now + HEAD_TIMEOUT_MS;
                }
                wait = head_deadline - now;
            }
            if (wait <= 0) {
                if (have == 0) goto out;              // idle: close silently
                send_simple(fd, 400, "Bad Request", "", 0);  // partial head
                goto out;
            }
            struct pollfd p = {fd, POLLIN, 0};
            int r = poll(&p, 1, (int)(wait > 1000 ? 1000 : wait));
            if (r < 0) { if (errno == EINTR) continue; goto out; }
            if (r == 0) continue;
            ssize_t n = recv(fd, buf + have, (size_t)(HEAD_MAX - have), 0);
            if (n == 0) goto out;                     // EOF
            if (n < 0) { if (errno == EINTR || errno == EAGAIN) continue; goto out; }
            have += (int)n;
            hend = head_end(buf, have);
        }
        // --- parse request line + the headers we care about ---
        char method[16] = {0}, path[1024] = {0}, version[16] = {0};
        long clen = 0;
        int keep = 1;
        {
            char *line = buf;
            char *eol = memchr(buf, '\r', (size_t)hend);
            if (!eol) { send_simple(fd, 400, "Bad Request", "", 0); goto out; }
            *eol = 0;
            if (sscanf(line, "%15s %1023s %15s", method, path, version) != 3) {
                send_simple(fd, 400, "Bad Request", "", 0); goto out;
            }
            if (strcmp(version, "HTTP/1.0") == 0) keep = 0;
            char *p = eol + 2;
            while (p < buf + hend - 2) {
                char *le = strstr(p, "\r\n");
                if (!le || le >= buf + hend) break;
                *le = 0;
                if (strncasecmp(p, "Content-Length:", 15) == 0) clen = strtol(p + 15, NULL, 10);
                else if (strncasecmp(p, "Connection:", 11) == 0) {
                    if (strcasestr(p + 11, "close")) keep = 0;
                }
                p = le + 2;
            }
        }
        if (clen < 0) { send_simple(fd, 400, "Bad Request", "", 0); goto out; }
        if (clen > BODY_MAX) { send_simple(fd, 413, "Payload Too Large", "", 0); goto out; }
        // --- read the body (may already be buffered past the head) ---
        int body_have = have - hend;
        if (body_have > clen) body_have = (int)clen;
        long body_deadline = now_ms() + 30000;
        while (body_have < clen) {
            long now = now_ms();
            long wait = body_deadline - now;
            if (wait <= 0) goto out;
            struct pollfd p = {fd, POLLIN, 0};
            int r = poll(&p, 1, (int)(wait > 1000 ? 1000 : wait));
            if (r <= 0) { if (r < 0 && errno == EINTR) continue; goto out; }
            ssize_t n = recv(fd, buf + hend + body_have, (size_t)(clen - body_have), 0);
            if (n <= 0) { if (n < 0 && (errno == EINTR || errno == EAGAIN)) continue; goto out; }
            body_have += (int)n;
        }
        // --- dispatch ---
        const char *body = "";
        size_t blen = 0;
        const char *ctype = "text/plain";
        int code = 200;
        if (strcmp(method, "GET") == 0 && strcmp(path, "/") == 0) {
            body = "hello from c\n"; blen = strlen(body);
        } else if (strcmp(method, "GET") == 0 && strcmp(path, "/json") == 0) {
            body = "{\"ok\":true,\"name\":\"c\",\"n\":42}"; blen = strlen(body); ctype = "application/json";
        } else if (strcmp(method, "GET") == 0 && strcmp(path, "/big") == 0) {
            body = g_big; blen = sizeof g_big;
        } else if (strcmp(method, "POST") == 0 && strcmp(path, "/echo") == 0) {
            body = buf + hend; blen = (size_t)clen;
        } else {
            code = 404; body = "not found\n"; blen = strlen(body);
        }
        // --- respond: head and body as two writes, like the zexi server ---
        char head[256];
        int hl = snprintf(head, sizeof head,
                          "HTTP/1.1 %d %s\r\nContent-Type: %s\r\n"
                          "Content-Length: %zu\r\nConnection: %s\r\n\r\n",
                          code, code == 200 ? "OK" : (code == 404 ? "Not Found" : "Error"),
                          ctype, blen, keep ? "keep-alive" : "close");
        write_all(fd, head, (size_t)hl);
        if (blen) write_all(fd, body, blen);
        atomic_fetch_add(&g_requests, 1);
        first = 0;
        if (g_budget > 0 && atomic_load(&g_requests) >= g_budget) {
            atomic_store(&g_stop, 1);
            goto out;
        }
        if (!keep) goto out;
        // keep-alive: carry any pipelined bytes to the front of the buffer
        int leftover = have - hend - (int)clen;
        if (leftover > 0) memmove(buf, buf + hend + clen, (size_t)leftover);
        have = leftover;
    }
out:
    free(buf);
}

static void *worker(void *arg) {
    int lfd = *(int *)arg;
    for (;;) {
        if (atomic_load(&g_stop)) break;
        struct pollfd p = {lfd, POLLIN, 0};
        int r = poll(&p, 1, POLL_MS);
        if (r < 0) { if (errno == EINTR) continue; break; }
        if (r == 0) continue;
        int c = accept(lfd, NULL, NULL);
        if (c < 0) { if (errno == EINTR || errno == EAGAIN || errno == EWOULDBLOCK) continue; break; }
        int one = 1;
        setsockopt(c, IPPROTO_TCP, TCP_NODELAY, &one, sizeof one);
        atomic_fetch_add(&g_conns, 1);
        serve_conn(c);
        close(c);
    }
    return NULL;
}

int main(int argc, char **argv) {
    signal(SIGPIPE, SIG_IGN);
    int port = argc > 1 ? atoi(argv[1]) : 18090;
    int workers = argc > 2 ? atoi(argv[2]) : 0;
    g_budget = argc > 3 ? atol(argv[3]) : 0;
    if (workers <= 0) {
        long n = sysconf(_SC_NPROCESSORS_ONLN);
        workers = (int)(n * 2);
        if (workers < 2) workers = 2;
    }
    if (workers > 64) workers = 64;

    for (int i = 0; i < (int)sizeof g_big / 16; i++)
        memcpy(g_big + i * 16, "0123456789abcdef", 16);

    int lfd = socket(AF_INET, SOCK_STREAM, 0);
    int one = 1;
    setsockopt(lfd, SOL_SOCKET, SO_REUSEADDR, &one, sizeof one);
    struct sockaddr_in a = {0};
    a.sin_family = AF_INET;
    a.sin_port = htons((unsigned short)port);
    a.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    if (bind(lfd, (struct sockaddr *)&a, sizeof a) < 0 || listen(lfd, 512) < 0) {
        perror("bind/listen");
        return 1;
    }
    fcntl(lfd, F_SETFL, fcntl(lfd, F_GETFL, 0) | O_NONBLOCK);

    pthread_t th[64];
    int started = 0;
    for (int i = 0; i < workers; i++) {
        if (pthread_create(&th[i], NULL, worker, &lfd) != 0) break;
        started++;
    }
    for (int i = 0; i < started; i++) pthread_join(th[i], NULL);
    close(lfd);
    printf("rc 0 requests %ld conns %ld\n",
           atomic_load(&g_requests), atomic_load(&g_conns));
    return 0;
}
