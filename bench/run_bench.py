#!/usr/bin/env python3
"""Zexi benchmark harness.

For every benchmark in bench/, this builds the Zexi implementation (zexic
→ C → native) and the plain-C implementation with the same C compiler,
runs them, and records whole-process wall time. Rules:

  * Every implementation of one benchmark must print the same checksum
    lines. A mismatch is reported and the row is marked, because a faster
    implementation that computes something else is not a result.
  * Timing is whole-process wall clock, deliberately including startup and
    teardown. Benchmarks are sized so process creation is a rounding error.
  * Both implementations are compiled by the same C compiler at the same
    optimization level, so a row compares the languages and their generated
    code, not two toolchains.

    python3 bench/run_bench.py                     # everything
    python3 bench/run_bench.py --only fib,sort     # a subset
    python3 bench/run_bench.py --reps 5            # more timing rounds
    python3 bench/run_bench.py --exe path/to/zexic # a prebuilt compiler

Results are written to bench/results.md (or --out). The machine's details
are recorded in the report; compare rows only on the machine they name.
"""
import argparse
import os
import platform
import re
import shutil
import statistics
import subprocess
import sys
import time
from datetime import date

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BENCHES = ["fib", "loops", "sort", "matmul", "strbuild", "match"]
CFLAGS = ["-std=gnu11", "-O2", "-fwrapv"]  # what zexic passes to cc


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


def build_one(name, zexic, cc, out_dir):
    zsrc = os.path.join(ROOT, "bench", name, name + ".zx")
    csrc = os.path.join(ROOT, "bench", name, name + ".c")
    zbin = os.path.join(out_dir, name + "_zexi")
    cbin = os.path.join(out_dir, name + "_c")

    p = sh([zexic, "build", zsrc, "-o", zbin, "--opt-level=2", "--cc=" + cc])
    if p.returncode != 0:
        return None, None, "zexic build failed:\n" + (p.stderr or p.stdout)
    p = sh([cc] + CFLAGS + ["-o", cbin, csrc])
    if p.returncode != 0:
        return None, None, "cc failed:\n" + p.stderr
    return zbin, cbin, None


def run_timed(binary, reps):
    """Run reps times; return (checksum_lines, best_seconds, median_seconds)."""
    times = []
    out = ""
    for _ in range(reps):
        t0 = time.perf_counter()
        p = sh([binary])
        times.append(time.perf_counter() - t0)
        if p.returncode != 0:
            return None, 0, 0, "exit %d: %s" % (p.returncode, p.stderr.strip())
        out = p.stdout
    return out.strip().split("\n"), min(times), statistics.median(times), None


def env_line(cc):
    uname = " ".join(platform.uname()[:2] + (platform.uname()[2],))
    ccv = sh([cc, "--version"]).stdout.split("\n")[0]
    return "generated %s · %s · %s" % (date.today().isoformat(), uname, ccv)


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--only", help="comma-separated benchmark names")
    ap.add_argument("--reps", type=int, default=3, help="timing rounds per implementation (default 3)")
    ap.add_argument("--exe", help="path to a built zexic")
    ap.add_argument("--cc", default=os.environ.get("CC", "cc"))
    ap.add_argument("--out", default=os.path.join(ROOT, "bench", "results.md"))
    args = ap.parse_args()

    names = args.only.split(",") if args.only else BENCHES
    for n in names:
        if n not in BENCHES:
            raise SystemExit("unknown benchmark %r (have %s)" % (n, ", ".join(BENCHES)))

    zexic = find_compiler(args.exe)
    out_dir = os.path.join(ROOT, "build", "bench")
    os.makedirs(out_dir, exist_ok=True)

    rows = []
    for name in names:
        print("== %s" % name)
        zbin, cbin, err = build_one(name, zexic, args.cc, out_dir)
        if err:
            print("   build failed: " + err.split("\n")[0])
            rows.append((name, None, None, None, "build failed"))
            continue
        z_out, z_best, z_med, z_err = run_timed(zbin, args.reps)
        c_out, c_best, c_med, c_err = run_timed(cbin, args.reps)
        if z_err or c_err:
            note = "run failed: " + (z_err or c_err).split("\n")[0]
            print("   " + note)
            rows.append((name, None, None, None, note))
            continue
        if z_out != c_out:
            note = "checksum mismatch: zexi %r vs C %r" % (z_out, c_out)
            print("   " + note)
            rows.append((name, None, None, None, note))
            continue
        ratio = c_best / z_best if z_best > 0 else 0
        print("   zexi %.1f ms, C %.1f ms, C/zexi %.2f  [%s]"
              % (z_best * 1e3, c_best * 1e3, ratio, ", ".join(z_out)))
        rows.append((name, (z_best, z_med), (c_best, c_med), ratio, ""))

    with open(args.out, "w", encoding="utf-8", newline="\n") as f:
        f.write("# Zexi benchmarks\n\n")
        f.write(env_line(args.cc) + "\n\n")
        f.write("Wall time of the whole process, best and median of %d runs. "
                "`C/zexi` above 1.0 means the Zexi build was faster. Each row's "
                "two implementations print the same checksum; a row that does "
                "not is marked, not ranked.\n\n" % args.reps)
        f.write("| benchmark | zexi best | zexi med | C best | C med | C/zexi | note |\n")
        f.write("| --- | --- | --- | --- | --- | --- | --- |\n")
        for name, z, c, ratio, note in rows:
            if z is None:
                f.write("| %s | — | — | — | — | — | %s |\n" % (name, note))
            else:
                f.write("| %s | %.1f ms | %.1f ms | %.1f ms | %.1f ms | %.2f | %s |\n"
                        % (name, z[0] * 1e3, z[1] * 1e3, c[0] * 1e3, c[1] * 1e3, ratio, note))
    print("wrote " + os.path.relpath(args.out, ROOT))
    return 0


if __name__ == "__main__":
    sys.exit(main())
