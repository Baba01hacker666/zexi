#!/usr/bin/env python3
"""Zexi benchmark harness.

For every benchmark in bench/ this builds each available implementation
(Zexi -> C -> native, plain C, and Rust when `rustc` is present), runs it at
each requested size, and records whole-process wall time and peak RSS.

Rules:

  * Every implementation of one benchmark at one size must print the same
    checksum line. A mismatch is reported and the cell is marked, because a
    faster implementation that computes something else is not a result.
  * Timing is whole-process wall clock, deliberately including startup and
    teardown. Each binary gets an untimed warmup run before the timed rounds.
  * Peak RSS is the child's `ru_maxrss`, sampled by the harness per run.
  * The Zexi and C implementations go through the same C compiler at the same
    optimization level, so a row compares languages and generated code, not
    toolchains; Rust is `rustc -O`.
  * Sizes are scale factors passed as the program's first argument; the
    programs repeat their workload that many times, so time scales while
    memory stays put.

    python3 bench/run_bench.py                     # everything, sizes 1,2,4
    python3 bench/run_bench.py --only fib,sort     # a subset
    python3 bench/run_bench.py --sizes 1,3         # chosen scale factors
    python3 bench/run_bench.py --reps 5            # more timing rounds
    python3 bench/run_bench.py --exe path/to/zexic # a prebuilt compiler

Results are written to bench/results.md and bench/results.json (or --out /
--json). The machine's details are recorded; compare rows only on the machine
they name.
"""
import argparse
import json
import os
import platform
import shutil
import statistics
import subprocess
import sys
import tempfile
import threading
import time
from datetime import datetime

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BENCHES = ["fib", "loops", "sort", "matmul", "strbuild", "match", "bounds"]
CFLAGS = ["-std=gnu11", "-O2", "-fwrapv"]  # what zexic passes to cc
RUSTFLAGS = ["-O", "--edition", "2021"]
LANGS = ["zexi", "c", "rust"]


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


def build_one(name, zexic, cc, rustc, out_dir):
    """Build every implementation that has a source file.

    Returns ({lang: binary}, {lang: error}). A language whose runtime is
    missing (no rustc) is simply absent from both maps.
    """
    bins, errs = {}, {}
    zsrc = os.path.join(ROOT, "bench", name, name + ".zx")
    csrc = os.path.join(ROOT, "bench", name, name + ".c")
    rsrc = os.path.join(ROOT, "bench", name, name + ".rs")

    if os.path.exists(zsrc):
        zbin = os.path.join(out_dir, name + "_zexi")
        p = sh([zexic, "build", zsrc, "-o", zbin, "--opt-level=2", "--cc=" + cc])
        if p.returncode != 0:
            errs["zexi"] = "zexic build failed:\n" + (p.stderr or p.stdout)
        else:
            bins["zexi"] = zbin
    if os.path.exists(csrc):
        cbin = os.path.join(out_dir, name + "_c")
        p = sh([cc] + CFLAGS + ["-o", cbin, csrc])
        if p.returncode != 0:
            errs["c"] = "cc failed:\n" + p.stderr
        else:
            bins["c"] = cbin
    if rustc and os.path.exists(rsrc):
        rbin = os.path.join(out_dir, name + "_rust")
        p = sh([rustc] + RUSTFLAGS + ["-o", rbin, rsrc])
        if p.returncode != 0:
            errs["rust"] = "rustc failed:\n" + p.stderr
        else:
            bins["rust"] = rbin
    return bins, errs


def peak_rss_kb(pid, samples):
    """Record the highest VmHWM seen in /proc/<pid>/status.

    `ru_maxrss` from wait4 is inherited across fork/exec on Linux (a tiny
    child reports the parent's high-water mark), so it cannot compare
    implementations. VmHWM is the process's own peak and is readable while
    it runs; a small poller samples it, and the last sample is close enough
    because VmHWM never decreases.
    """
    try:
        with open("/proc/%d/status" % pid, encoding="utf-8") as f:
            for line in f:
                if line.startswith("VmHWM:"):
                    samples.append(int(line.split()[1]))
                    return
    except (OSError, ValueError, IndexError):
        pass


def wait4_run(argv):
    """Run argv, returning (stdout, stderr, returncode, seconds, rss_kb)."""
    with tempfile.TemporaryFile() as out, tempfile.TemporaryFile() as err:
        t0 = time.perf_counter()
        p = subprocess.Popen(argv, stdout=out, stderr=err)
        samples = []
        stop = threading.Event()

        def poll():
            while not stop.is_set():
                peak_rss_kb(p.pid, samples)
                stop.wait(0.002)

        sampler = threading.Thread(target=poll, daemon=True)
        sampler.start()
        if hasattr(os, "wait4"):
            _, status, _ = os.wait4(p.pid, 0)
            p.returncode = os.waitstatus_to_exitcode(status)
        else:  # pragma: no cover - POSIX without wait4
            p.wait()
        stop.set()
        sampler.join()
        dt = time.perf_counter() - t0
        out.seek(0)
        err.seek(0)
        return (out.read().decode("utf-8", "replace"),
                err.read().decode("utf-8", "replace"), p.returncode, dt,
                max(samples) if samples else None)


def run_size(binary, size, reps, warmup):
    """Warm up, then time `reps` runs. Returns a result dict or an error."""
    for _ in range(warmup):
        out, err, code, _, _ = wait4_run([binary, str(size)])
        if code != 0:
            return {"error": "warmup exit %d: %s" % (code, err.strip())}
    times, rss, stdout = [], [], ""
    for _ in range(reps):
        out, err, code, dt, kb = wait4_run([binary, str(size)])
        if code != 0:
            return {"error": "exit %d: %s" % (code, err.strip())}
        times.append(dt)
        if kb is not None:
            rss.append(kb)
        stdout = out
    return {
        "checksum": stdout.strip().split("\n"),
        "best_ms": min(times) * 1e3,
        "median_ms": statistics.median(times) * 1e3,
        "rss_kb": max(rss) if rss else None,
    }


def env_lines(cc, rustc):
    u = platform.uname()
    lines = ["generated %s · %s %s · %s" %
             (datetime.now().strftime("%Y-%m-%d %H:%M"), u.system, u.machine, u.release),
             "cc: " + (sh([cc, "--version"]).stdout.split("\n")[0] or cc)]
    if rustc:
        lines.append("rustc: " + (sh([rustc, "--version"]).stdout.strip() or rustc))
    return lines


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--only", help="comma-separated benchmark names")
    ap.add_argument("--sizes", default="1,2,4",
                    help="comma-separated scale factors passed to each program (default 1,2,4)")
    ap.add_argument("--reps", type=int, default=3, help="timing rounds per implementation (default 3)")
    ap.add_argument("--warmup", type=int, default=1, help="untimed runs before timing (default 1)")
    ap.add_argument("--exe", help="path to a built zexic")
    ap.add_argument("--cc", default=os.environ.get("CC", "cc"))
    ap.add_argument("--rustc", default=shutil.which("rustc"),
                    help="Rust compiler for the .rs twins (default: found on PATH)")
    ap.add_argument("--out", default=os.path.join(ROOT, "bench", "results.md"))
    ap.add_argument("--json", default=os.path.join(ROOT, "bench", "results.json"))
    args = ap.parse_args()

    names = args.only.split(",") if args.only else BENCHES
    for n in names:
        if n not in BENCHES:
            raise SystemExit("unknown benchmark %r (have %s)" % (n, ", ".join(BENCHES)))
    try:
        sizes = [int(s) for s in args.sizes.split(",") if s.strip()]
    except ValueError:
        raise SystemExit("--sizes wants integers, got %r" % args.sizes)
    if not sizes or any(s < 1 for s in sizes):
        raise SystemExit("--sizes wants positive integers")

    zexic = find_compiler(args.exe)
    out_dir = os.path.join(ROOT, "build", "bench")
    os.makedirs(out_dir, exist_ok=True)
    if not args.rustc:
        print("note: no rustc found; Rust twins are skipped")

    report, json_rows = [], []
    for name in names:
        print("== %s" % name)
        bins, errs = build_one(name, zexic, args.cc, args.rustc, out_dir)
        for lang, err in errs.items():
            print("   %s build failed: %s" % (lang, err.split("\n")[0]))
        for size in sizes:
            results, checksum = {}, None
            for lang in LANGS:
                if lang not in bins:
                    continue
                if errs.get(lang):
                    results[lang] = {"error": errs[lang]}
                    continue
                r = run_size(bins[lang], size, args.reps, args.warmup)
                results[lang] = r
                if "error" not in r:
                    if checksum is None:
                        checksum = r["checksum"]
                    elif r["checksum"] != checksum:
                        r["error"] = "checksum mismatch: %r vs %r" % (r["checksum"], checksum)
            ok = {k: v for k, v in results.items() if "error" not in v}
            note = ""
            if len(ok) < 2:
                note = "; ".join("%s %s" % (k, v.get("error", "missing"))
                                 for k, v in results.items() if "error" in v) or "no implementations ran"
            if "zexi" in ok:
                for lang, r in ok.items():
                    r["ratio_vs_zexi"] = ok["zexi"]["best_ms"] / r["best_ms"]
            print("   size %-2d %s" % (size, "  ".join(
                "%s %.1f ms" % (k, v["best_ms"]) for k, v in ok.items()) + ("  " + note if note else "")))
            report.append((name, size, results, note))
            json_rows.append({"benchmark": name, "size": size, "checksum": checksum,
                              "implementations": results, "note": note})

    with open(args.out, "w", encoding="utf-8", newline="\n") as f:
        f.write("# Zexi benchmarks\n\n")
        for line in env_lines(args.cc, args.rustc):
            f.write(line + "\n")
        f.write("\nWhole-process wall time and peak RSS (VmHWM sampled while the "
                "process runs), best of %d timed runs after "
                "%d warmup run(s). Sizes are scale factors passed as argv[1]. "
                "`C/zexi` and `Rust/zexi` above 1.0 mean Zexi was faster. Each "
                "row's implementations print the same checksum; a row that does "
                "not is marked, not ranked.\n\n" % (args.reps, args.warmup))
        f.write("| benchmark | size | zexi | C | Rust | C/zexi | Rust/zexi | zexi RSS | C RSS | Rust RSS | note |\n")
        f.write("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |\n")
        for name, size, results, note in report:
            def cell(lang, key, fmt):
                r = results.get(lang)
                if not r or "error" in r or r.get(key) is None:
                    return "—"
                return fmt % r[key]

            z = cell("zexi", "best_ms", "%.1f ms")
            c = cell("c", "best_ms", "%.1f ms")
            ru = cell("rust", "best_ms", "%.1f ms")
            cr = cell("c", "ratio_vs_zexi", "%.2f")
            rr = cell("rust", "ratio_vs_zexi", "%.2f")
            zr = cell("zexi", "rss_kb", "%d KiB")
            crss = cell("c", "rss_kb", "%d KiB")
            rrss = cell("rust", "rss_kb", "%d KiB")
            f.write("| %s | %d | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n"
                    % (name, size, z, c, ru, cr, rr, zr, crss, rrss, note))
    print("wrote " + os.path.relpath(args.out, ROOT))

    with open(args.json, "w", encoding="utf-8", newline="\n") as f:
        json.dump({"generated": datetime.now().isoformat(timespec="seconds"),
                   "machine": "%s %s" % (platform.system(), platform.machine()),
                   "cc": args.cc, "rustc": args.rustc,
                   "reps": args.reps, "warmup": args.warmup, "sizes": sizes,
                   "results": json_rows}, f, indent=2)
        f.write("\n")
    print("wrote " + os.path.relpath(args.json, ROOT))
    return 0


if __name__ == "__main__":
    sys.exit(main())
