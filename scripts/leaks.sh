#!/bin/sh
# Build a Zexi program with AddressSanitizer + LeakSanitizer and report what it
# leaked. The count is the number in this file, not a feeling.
#
#   scripts/leaks.sh tests/closures.zx
#
# Requires a C compiler that ships the sanitizers (gcc/clang do).
set -eu

prog=${1:-tests/closures.zx}
[ -f "$prog" ] || { echo "no such program: $prog" >&2; exit 2; }

cc_bin=${CC:-cc}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# One translation unit, exactly as `zexic emit` prints it.
if [ -n "${ZEXIC:-}" ]; then
    zexic=$ZEXIC
elif command -v zexic > /dev/null 2>&1; then
    zexic=$(command -v zexic)
else
    zexic=$(dirname "$0")/../build/zexic
    [ -x "$zexic" ] || { echo "build the compiler first (go build -o build/zexic ./cmd/zexic), or set ZEXIC=" >&2; exit 2; }
fi
"$zexic" emit "$prog" > "$work/out.c"
"$cc_bin" -std=gnu11 -O1 -g -fsanitize=address -o "$work/prog" "$work/out.c"

set +e
"$work/prog" > /dev/null 2> "$work/err.txt"
status=$?
set -e

if [ "$status" -ne 0 ]; then
    echo "$prog: FAILED (exit $status)"
    cat "$work/err.txt"
    exit 1
fi

if grep -q "LeakSanitizer" "$work/err.txt"; then
    echo "$prog: leaks detected"
    grep -E "^(Direct|Indirect) leak|SUMMARY" "$work/err.txt"
    exit 1
else
    echo "$prog: clean (no leaks, no sanitizer errors)"
fi
