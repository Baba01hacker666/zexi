#!/usr/bin/env python3
"""Zexi HTTP server load test — how much can the built-in server handle?

Builds bench/http_bench.zx with zexic (C backend) into a native binary, then
drives it with a raw-socket load generator (no external tools) and reports
requests/sec, latency percentiles, error counts, and the server-side request
count, across a sweep of client concurrency and worker-pool sizes.

Two servers can be measured with the same load: the default is the Zexi
stdlib server (bench/http_bench.zx built by zexic); --server c measures the
raw C twin (bench/http_bench.c, same routes/CLI/exit line) built with cc.
Every throughput row also reports the server process's CPU (cores used, from
/proc/<pid>/stat jiffies) and peak RSS (VmHWM from /proc/<pid>/status),
sampled while the load runs.

Phases:
  throughput   tiny GET / with HTTP/1.1 keep-alive, concurrency x workers
  close-mode   same load with Connection: close (one TCP connection per
               request) — the pre-keep-alive behaviour — for comparison
  big-body     GET /big (64 KiB) with keep-alive — raw throughput
  post-echo    POST /echo with a 1 KiB body
  slowloris    K stalled half-sent heads against W workers; measures how much
               one stalled client set delays a normal request (fairness bound)
  abuse        oversized head -> 431, garbage -> 400/404, silent probe -> no reply

    python3 bench/http_load.py                  # full sweep (zexi server)
    python3 bench/http_load.py --server c --out bench/http-c-results.md
    python3 bench/http_load.py --quick          # small request counts
    python3 bench/http_load.py --exe path/to/zexic --total 2000 --timeout 120
    python3 bench/http_load.py --out bench/http-results.md

Results print to stdout as a markdown table and are written to --out
(overwriting the previous run), like bench/results.md. Compare rows only on the
machine they name. Ports are bound on 127.0.0.1 only. --timeout bounds the
whole run: phases that would start with too little time left are skipped and
noted.
"""
import argparse
import os
import platform
import shutil
import socket
import subprocess
import sys
import threading
import time
from datetime import date

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
PORT_BASE = 18200


def sh(cmd, **kw):
    return subprocess.run(cmd, capture_output=True, text=True,
                          encoding="utf-8", errors="replace", **kw)


def find_compiler(exe):
    if exe:
        return exe
    go = shutil.which("go")
    if not go:
        raise SystemExit("no --exe given and no `go` on PATH to build zexic with")
    out = os.path.join(ROOT, "build", "bench", "zexic")
    os.makedirs(os.path.dirname(out), exist_ok=True)
    p = sh([go, "build", "-o", out, "./cmd/zexic"], cwd=ROOT)
    if p.returncode != 0:
        raise SystemExit("go build ./cmd/zexic failed:\n" + p.stderr)
    return out


def build_server(zexic, kind):
    os.makedirs(os.path.join(ROOT, "build", "bench"), exist_ok=True)
    if kind == "c":
        cc = shutil.which("cc") or shutil.which("gcc") or shutil.which("clang")
        if not cc:
            raise SystemExit("--server c needs a C compiler (cc/gcc/clang) on PATH")
        out = os.path.join(ROOT, "build", "bench", "http_bench_c")
        p = sh([cc, "-std=gnu11", "-O2", "-pthread",
                "bench/http_bench.c", "-o", out], cwd=ROOT)
        if p.returncode != 0:
            raise SystemExit("building bench/http_bench.c failed:\n" + p.stderr)
        return out
    out = os.path.join(ROOT, "build", "bench", "http_bench")
    p = sh([zexic, "build", "bench/http_bench.zx", "-o", out], cwd=ROOT)
    if p.returncode != 0:
        raise SystemExit("building bench/http_bench.zx failed:\n" + p.stdout + p.stderr)
    return out


CLK_TCK = os.sysconf("SC_CLK_TCK") if hasattr(os, "sysconf") else 100


def _proc_jiffies(pid):
    with open(f"/proc/{pid}/stat", encoding="utf-8") as f:
        data = f.read()
    rest = data[data.rindex(")") + 2:].split()  # fields from `state` on
    return int(rest[11]) + int(rest[12])         # utime + stime


def _proc_rss(pid):
    rss = hwm = 0
    with open(f"/proc/{pid}/status", encoding="utf-8") as f:
        for line in f:
            if line.startswith("VmRSS:"):
                rss = int(line.split()[1])
            elif line.startswith("VmHWM:"):
                hwm = int(line.split()[1])
    return rss, hwm  # KiB


class Server:
    """One http_bench process with a request budget (0 = run until killed)."""

    def __init__(self, binary, port, workers, budget):
        self.port = port
        self.proc = subprocess.Popen(
            [binary, str(port), str(workers), str(budget)],
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        self.pid = self.proc.pid
        self._samp = []
        self._samp_stop = threading.Event()
        self._samp_thread = None
        self._wait_ready()

    def _sample_once(self):
        try:
            rss, hwm = _proc_rss(self.pid)
            if hwm == 0:
                return  # zombie / exiting: skip so the last valid RSS sample sticks
            self._samp.append((time.perf_counter(), _proc_jiffies(self.pid),
                               rss, hwm))
        except (OSError, ValueError, IndexError):
            pass  # process gone

    def start_sampling(self):
        """Sample /proc CPU + RSS every 50 ms until stop_sampling()."""
        self._samp = []
        self._samp_stop.clear()
        self._sample_once()

        def loop():
            while not self._samp_stop.is_set():
                self._sample_once()
                self._samp_stop.wait(0.05)

        self._samp_thread = threading.Thread(target=loop, daemon=True)
        self._samp_thread.start()

    def stop_sampling(self):
        """Returns (cpu cores used on average, RSS at end MiB, peak RSS MiB)."""
        self._samp_stop.set()
        if self._samp_thread is not None:
            self._samp_thread.join(timeout=2)
        self._sample_once()  # final sample if the process is still alive
        if len(self._samp) >= 2:
            t0, j0 = self._samp[0][0], self._samp[0][1]
            t1, j1, rss_end, hwm = self._samp[-1]
            wall = t1 - t0
            cores = (j1 - j0) / CLK_TCK / wall if wall > 0 else 0.0
            return cores, rss_end / 1024.0, hwm / 1024.0
        return 0.0, 0.0, 0.0

    def _wait_ready(self, timeout=10.0):
        deadline = time.time() + timeout
        while time.time() < deadline:
            if self.proc.poll() is not None:
                raise SystemExit("server exited early:\n" + self.proc.stdout.read())
            try:
                s = socket.create_connection(("127.0.0.1", self.port), timeout=0.2)
                s.close()
                return
            except OSError:
                time.sleep(0.05)
        raise SystemExit("server never started listening")

    def wait_done(self, timeout=15.0):
        try:
            out, _ = self.proc.communicate(timeout=timeout)
        except subprocess.TimeoutExpired:
            self.proc.kill()
            out, _ = self.proc.communicate()
        return out.strip()

    def stop(self):
        if self.proc.poll() is None:
            self.proc.terminate()
            try:
                self.proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.proc.kill()


def raw_request(sock, method, path, body=b"", keep_alive=True, host="127.0.0.1"):
    """Send one request on sock, read one full response. Returns (status, head, body)."""
    conn_hdr = b"keep-alive" if keep_alive else b"close"
    req = (f"{method} {path} HTTP/1.1\r\nHost: {host}\r\n"
           f"Connection: {conn_hdr.decode()}\r\n"
           f"Content-Length: {len(body)}\r\n\r\n").encode() + body
    sock.sendall(req)
    buf = b""
    while b"\r\n\r\n" not in buf:
        chunk = sock.recv(65536)
        if not chunk:
            raise ConnectionError("EOF in head")
        buf += chunk
    head, rest = buf.split(b"\r\n\r\n", 1)
    status = int(head.split(b" ", 2)[1])
    clen = 0
    for line in head.split(b"\r\n")[1:]:
        if line.lower().startswith(b"content-length:"):
            clen = int(line.split(b":", 1)[1].strip())
    body_out = rest
    while len(body_out) < clen:
        chunk = sock.recv(65536)
        if not chunk:
            break
        body_out += chunk
    return status, head, body_out[:clen]


class LoadResult:
    def __init__(self):
        self.ok = 0
        self.errors = 0
        self.status = {}
        self.lats = []
        self.lock = threading.Lock()
        self.wall = 0.0

    def record(self, status, lat, err=False):
        with self.lock:
            if err:
                self.errors += 1
            else:
                self.ok += 1
                self.status[status] = self.status.get(status, 0) + 1
                self.lats.append(lat)


def run_load(port, total, concurrency, path="/", method="GET", body=b"", keep_alive=True):
    """total requests spread over `concurrency` threads; each thread uses one
    keep-alive socket (or a fresh socket per request when keep_alive=False)."""
    res = LoadResult()
    per = total // concurrency
    extra = total - per * concurrency

    def worker(idx):
        n = per + (1 if idx < extra else 0)
        sock = None
        try:
            for _ in range(n):
                t0 = time.perf_counter()
                try:
                    if keep_alive:
                        if sock is None:
                            sock = socket.create_connection(("127.0.0.1", port), timeout=10)
                        status, _head, _b = raw_request(sock, method, path, body, keep_alive=True)
                    else:
                        sock = socket.create_connection(("127.0.0.1", port), timeout=10)
                        status, _head, _b = raw_request(sock, method, path, body, keep_alive=False)
                        sock.close()
                        sock = None
                except (OSError, ConnectionError):
                    res.record(0, 0.0, err=True)
                    if sock is not None:
                        try:
                            sock.close()
                        except OSError:
                            pass
                        sock = None
                    continue
                lat = time.perf_counter() - t0
                if status == 200:
                    res.record(status, lat)
                else:
                    res.record(0, 0.0, err=True)
                    res.status[status] = res.status.get(status, 0) + 1
        finally:
            if sock is not None:
                try:
                    sock.close()
                except OSError:
                    pass

    threads = [threading.Thread(target=worker, args=(i,)) for i in range(concurrency)]
    t0 = time.perf_counter()
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    res.wall = time.perf_counter() - t0
    return res


def run_load_for(port, duration, concurrency, path="/", method="GET", body=b"", keep_alive=True):
    """Like run_load, but workers send requests until `duration` seconds have
    passed — used for steady-state CPU/RSS sampling."""
    res = LoadResult()
    deadline = time.perf_counter() + duration

    def worker(_idx):
        sock = None
        try:
            while time.perf_counter() < deadline:
                t0 = time.perf_counter()
                try:
                    if keep_alive:
                        if sock is None:
                            sock = socket.create_connection(("127.0.0.1", port), timeout=10)
                        status, _h, _b = raw_request(sock, method, path, body, keep_alive=True)
                    else:
                        sock = socket.create_connection(("127.0.0.1", port), timeout=10)
                        status, _h, _b = raw_request(sock, method, path, body, keep_alive=False)
                        sock.close()
                        sock = None
                except (OSError, ConnectionError):
                    res.record(0, 0.0, err=True)
                    if sock is not None:
                        try:
                            sock.close()
                        except OSError:
                            pass
                        sock = None
                    continue
                if status == 200:
                    res.record(status, time.perf_counter() - t0)
                else:
                    res.record(0, 0.0, err=True)
                    res.status[status] = res.status.get(status, 0) + 1
        finally:
            if sock is not None:
                try:
                    sock.close()
                except OSError:
                    pass

    threads = [threading.Thread(target=worker, args=(i,)) for i in range(concurrency)]
    t0 = time.perf_counter()
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    res.wall = time.perf_counter() - t0
    return res


def pct(lats, p):
    if not lats:
        return 0.0
    lats = sorted(lats)
    k = min(len(lats) - 1, max(0, int(round(p / 100.0 * len(lats))) - 1))
    return lats[k] * 1000.0


def fmt_row(label, res, total, res_src=None, note=""):
    rps = res.ok / res.wall if res.wall else 0
    if res_src is not None:
        cores, _rss_end, hwm = res_src
        cpu_cell, rss_cell = f"{cores:.2f}", f"{hwm:.1f}"
    else:
        cpu_cell, rss_cell = "–", "–"
    return (f"| {label}{note} | {total} | {res.ok} | {res.errors} | {rps:.0f} | "
            f"{pct(res.lats, 50):.2f} | {pct(res.lats, 95):.2f} | {pct(res.lats, 99):.2f} | "
            f"{res.wall:.2f} | {cpu_cell} | {rss_cell} |")


TABLE_HEAD = ("| load | requests | ok | errors | req/s | p50 ms | p95 ms | p99 ms | wall s | cpu cores | rss MiB |\n"
              "| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |")


def phase(binary, port, workers, budget, total, concurrency, label, **kw):
    srv = Server(binary, port, workers, budget)
    try:
        srv.start_sampling()
        res = run_load(port, total, concurrency, **kw)
        res_src = srv.stop_sampling()
    finally:
        out = srv.wait_done()
    parts = out.split()
    served = int(parts[parts.index("requests") + 1]) if "requests" in parts else None
    match = "match" if served == res.ok else f"MISMATCH(server={served})"
    print(f"  {label}: {res.ok / res.wall:.0f} req/s, ok={res.ok} err={res.errors} "
          f"server_requests={served} {match} cpu={res_src[0]:.2f} cores rss_peak={res_src[2]:.1f} MiB",
          flush=True)
    return (fmt_row(label, res, total, res_src=res_src,
                    note="" if served == res.ok else f" **{match}**"),
            served, res.ok)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--exe", help="prebuilt zexic binary")
    ap.add_argument("--server", choices=["zx", "c"], default="zx",
                    help="which bench server to measure: zx = bench/http_bench.zx via zexic, "
                         "c = raw C twin bench/http_bench.c via cc")
    ap.add_argument("--out", help="write the report to this markdown file (overwrites)")
    ap.add_argument("--quick", action="store_true", help="small request counts")
    ap.add_argument("--concurrency", default="1,8,32,128",
                    help="comma-separated client concurrency sweep")
    ap.add_argument("--workers", default="1,4", help="comma-separated worker-pool sizes")
    ap.add_argument("--total", type=int, default=0,
                    help="requests per throughput cell (default: 4000 quick / 20000 full)")
    ap.add_argument("--timeout", type=float, default=240.0,
                    help="global wall-clock budget in seconds; phases that would start with "
                         "less than 20s left are skipped and noted")
    args = ap.parse_args()

    zexic = find_compiler(args.exe) if args.server == "zx" else None
    binary = build_server(zexic, args.server)
    total = args.total or (4000 if args.quick else 20000)
    big_total = max(200, total // 5)
    concs = [int(x) for x in args.concurrency.split(",")]
    worker_list = [int(x) for x in args.workers.split(",")]
    nproc = os.cpu_count()
    deadline = time.time() + args.timeout

    def left():
        return deadline - time.time()

    def ok_to_start():
        if left() < 20:
            print(f"  (phase skipped: only {left():.0f}s of the {args.timeout:.0f}s budget left)", flush=True)
            return False
        return True

    port = PORT_BASE
    rows = [TABLE_HEAD]
    sl_rows = []

    if args.server == "c":
        server_desc = ("server = bench/http_bench.c (raw C twin), built with "
                       "cc -std=gnu11 -O2 -pthread, same routes/CLI as the zexi bench")
    else:
        server_desc = "server = bench/http_bench.zx via zexic (C backend)"
    print(f"# HTTP load test — {date.today().isoformat()}", flush=True)
    print(f"machine: {platform.machine()}, {nproc} cpu(s), python {platform.python_version()}, "
          f"{server_desc}", flush=True)

    print("\n## throughput: GET / (tiny body), HTTP/1.1 keep-alive", flush=True)
    if ok_to_start():
        for w in worker_list:
            for c in concs:
                if not ok_to_start():
                    break
                r, _served, _ok = phase(binary, port, w, total, total, c,
                                        f"GET / keep-alive workers={w} c={c}",
                                        path="/", keep_alive=True)
                port += 1
                rows.append(r)

    print("\n## connection-per-request (Connection: close), for comparison", flush=True)
    if ok_to_start():
        for w in worker_list:
            c = max(1, max(concs) // 4)
            r, _s, _o = phase(binary, port, w, total, total, c,
                              f"GET / close-mode workers={w} c={c}",
                              path="/", keep_alive=False)
            port += 1
            rows.append(r)

    print("\n## big body: GET /big (64 KiB), keep-alive", flush=True)
    if ok_to_start():
        w = worker_list[-1]
        c = 16
        r, _s, _o = phase(binary, port, w, big_total, big_total, c,
                          f"GET /big keep-alive workers={w} c={c}",
                          path="/big", keep_alive=True)
        port += 1
        rows.append(r)

    print("\n## POST /echo (1 KiB body), keep-alive", flush=True)
    if ok_to_start():
        w = worker_list[-1]
        r, _s, _o = phase(binary, port, w, big_total, big_total, 16,
                          f"POST /echo workers={w} c=16",
                          path="/echo", method="POST", body=b"x" * 1024, keep_alive=True)
        port += 1
        rows.append(r)

    print("\n## resources: sustained 5 s load at max concurrency (server cpu/rss sampled throughout)", flush=True)
    res_rows = []
    if ok_to_start():
        w = worker_list[-1]
        c = max(concs)
        srv = Server(binary, port, w, 0)  # budget 0: killed after the measurement
        try:
            srv.start_sampling()
            res = run_load_for(port, 5.0, c, path="/", keep_alive=True)
            cores, rss_end, hwm = srv.stop_sampling()
        finally:
            srv.stop()
        pct_cpu = cores / nproc * 100 if nproc else 0
        res_rows.append((w, c, res, cores, pct_cpu, rss_end, hwm))
        print(f"  sustained workers={w} c={c} for 5 s: {res.ok / res.wall:.0f} req/s, "
              f"ok={res.ok} err={res.errors}, server cpu {cores:.2f} cores ({pct_cpu:.0f}%), "
              f"rss {rss_end:.1f} MiB end / {hwm:.1f} MiB peak", flush=True)
        port += 1

    print("\n## slowloris probe: stalled half-sent heads vs one normal request", flush=True)
    if ok_to_start():
        for w in (2, 8):
            if not ok_to_start():
                break
            stalled = 8
            srv = Server(binary, port, w, 0)  # budget 0: run until killed
            try:
                socks = []
                for _i in range(stalled):
                    s = socket.create_connection(("127.0.0.1", port), timeout=5)
                    s.sendall(b"GET / HTTP/1.1\r\nHost: x\r\n")  # no final CRLF: head never ends
                    socks.append(s)
                time.sleep(0.05)
                t0 = time.perf_counter()
                s = socket.create_connection(("127.0.0.1", port), timeout=40)
                try:
                    status, _h, _b = raw_request(s, "GET", "/", keep_alive=False)
                    lat_ms = (time.perf_counter() - t0) * 1000
                except (OSError, ConnectionError) as e:
                    status, lat_ms = f"TIMEOUT({type(e).__name__})", 40000.0
                s.close()
                for s2 in socks:
                    s2.close()
                sl_rows.append((w, stalled, status, lat_ms))
                print(f"  workers={w}: {stalled} stalled heads -> normal request "
                      f"status={status} latency={lat_ms:.0f} ms", flush=True)
            finally:
                srv.stop()
            port += 1

    print("\n## abuse probes", flush=True)
    if ok_to_start():
        srv = Server(binary, port, 2, 0)
        try:
            s = socket.create_connection(("127.0.0.1", port), timeout=5)
            s.sendall(b"GET /" + b"a" * 20000 + b" HTTP/1.1\r\n\r\n")  # oversized head
            r = s.recv(4096)
            print("  oversized head ->", r.split(b"\r\n")[0].decode(), flush=True)
            s.close()
            s = socket.create_connection(("127.0.0.1", port), timeout=5)
            s.sendall(b"GARBAGE\r\n\r\n")
            r = s.recv(4096)
            print("  garbage head   ->", r.split(b"\r\n")[0].decode() if r else "(no reply)", flush=True)
            s.close()
            s = socket.create_connection(("127.0.0.1", port), timeout=5)
            s.close()  # silent probe: connect and close without sending anything
            s = socket.create_connection(("127.0.0.1", port), timeout=5)
            status, _h, _b = raw_request(s, "GET", "/json", keep_alive=False)
            print("  silent probe   -> ignored; server still up, GET /json ->", status, flush=True)
            s.close()
        finally:
            srv.stop()
        port += 1

    print("\n" + "\n".join(rows), flush=True)
    if args.server == "c":
        srv_line = (f"Server: `bench/http_bench.c` (raw C twin), `cc -std=gnu11 -O2 -pthread`, "
                    f"worker pool, HTTP/1.1 keep-alive. ")
    else:
        srv_line = (f"Server: `bench/http_bench.zx` built by zexic (C backend, -O2), worker pool via "
                    f"`listen_workers_n`, HTTP/1.1 keep-alive. ")
    report = [f"# HTTP server load test ({'raw C' if args.server == 'c' else 'zexi'}) — {date.today().isoformat()}", "",
              f"Machine: {platform.machine()}, {nproc} cpu(s), Python {platform.python_version()}. "
              + srv_line +
              f"Load: `bench/http_load.py` raw sockets, {total} reqs per throughput cell"
              + (", --quick" if args.quick else "")
              + ". cpu cores = /proc jiffies over wall time; rss MiB = server peak VmHWM.", ""]
    if res_rows:
        report += ["## resources (sustained 5 s load; cpu from /proc jiffies, rss = VmHWM)", "",
                   "| workers | concurrency | req/s | ok | errors | cpu cores | cpu % | rss end MiB | rss peak MiB |",
                   "| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |"]
        for w, c, res, cores, pct_cpu, rss_end, hwm in res_rows:
            report.append(f"| {w} | {c} | {res.ok / res.wall:.0f} | {res.ok} | {res.errors} | "
                          f"{cores:.2f} | {pct_cpu:.0f} | {rss_end:.1f} | {hwm:.1f} |")
        report.append("")
    if sl_rows:
        report += ["## slowloris probe (stalled half-sent heads)", "",
                   "| workers | stalled heads | normal request status | latency ms |",
                   "| ---: | ---: | ---: | ---: |"]
        report += [f"| {w} | {st} | {status} | {lat:.0f} |" for w, st, status, lat in sl_rows]
        report.append("")
    report += ["## results", ""] + rows + [""]
    text = "\n".join(report)
    if args.out:
        with open(args.out, "w", encoding="utf-8") as f:
            f.write(text + "\n")
        print(f"\nwrote report to {args.out}", flush=True)
    print(text, flush=True)


if __name__ == "__main__":
    main()
