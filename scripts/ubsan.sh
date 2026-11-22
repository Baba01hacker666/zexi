#!/bin/sh
# scripts/ubsan.sh — sweep every shippable program's generated C through
# UBSan + AddressSanitizer and report which programs report undefined
# behaviour. Prints one line per clean program and one line per offender, and
# exits 1 if anything reports.
#
#   scripts/ubsan.sh                 # sweep tests/*.zx at -O0 and -O2
#   scripts/ubsan.sh tests/shift.zx  # sweep a single program
#
# set -x
# set -e

set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"

if [ -n "${1:-}" ]; then
    programs="$1"
else
    programs=$(ls tests/*.zx)
fi

zexic=${ZEXIC:-build/zexic}
cc_bin=${CC:-cc}
# Programs that are servers, need a C library, or need a wrapper harness:
# not part of the stock sweep.
skip="http_server net_local http_client threads pool web_lib web_result web_template modules modules_lib easy_lib panic_message"

found_ub=0
for opt in 0 2; do
    echo "### opt=$opt"
    for prog in $programs; do
        b=$(basename "$prog" .zx)
        case " $skip " in
            *" $b "*) continue ;;
        esac
        "$zexic" emit --opt-level="$opt" "$prog" > /tmp/ub.c 2>/dev/null || {
            echo "$b: EMIT-FAIL"; found_ub=1; continue
        }
        if ! "$cc_bin" -std=gnu11 -O1 -fwrapv -fsanitize=undefined -fno-sanitize-recover=all \
                -o /tmp/ubprog /tmp/ub.c 2>/tmp/ub.ccerr; then
            echo "$b: CC-FAIL"; head -3 /tmp/ub.ccerr >&2; found_ub=1; continue
        fi
        if ! timeout 8 /tmp/ubprog >/dev/null 2>/tmp/ub.runerr; then
            if grep -q "runtime error" /tmp/ub.runerr; then
                echo "$b: UB"
                grep "runtime error" /tmp/ub.runerr | head -3 >&2
                found_ub=1
            else
                echo "$b: RUN-FAIL ($(head -1 /tmp/ub.runerr))"
            fi
        fi
    done
done
if [ "$found_ub" -eq 0 ]; then
    echo "clean: no sanitizer errors across the sweep"
else
    echo "offenders found" >&2
    exit 1
fi
