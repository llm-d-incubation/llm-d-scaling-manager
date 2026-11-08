#!/usr/bin/env bash
#
# Runs deploy/ci-pr-checks/disk-report.sh against sample-file fixtures.
#
# The report runs after every e2e job whatever its result, so it must never
# exit non-zero: a red report step fails a run whose tests passed. It did: a
# sample line with an empty value ("10:00:15 ", what a sampler killed mid-line
# writes) made its arithmetic a bash syntax error and the step exit 1.
# Each case asserts exit 0 AND the line the report should print, so a script
# that exits 0 having printed nothing does not pass either.

set -u

ROOT="${CHECK_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
REPORT="$ROOT/deploy/ci-pr-checks/disk-report.sh"
[ -f "$REPORT" ] || { printf 'FATAL: %s not found\n' "$REPORT" >&2; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# df and docker are stubbed: the cases are about the samples, and the report
# must not depend on what this machine's disk or Docker says.
mkdir -p "$WORK/bin"
printf '#!/bin/sh\necho "stub $(basename "$0")"\n' >"$WORK/bin/df"
printf '#!/bin/sh\nexit 1\n' >"$WORK/bin/docker"
chmod +x "$WORK/bin/df" "$WORK/bin/docker"

FAIL=0
PASS=0
CASES=0

# case_ NAME FILE_OR_- EXPECTED_SUBSTRING
case_() {
    local name="$1" file="$2" want="$3" out rc=0
    CASES=$((CASES + 1))
    out="$(PATH="$WORK/bin:$PATH" DISK_SAMPLES="$file" bash "$REPORT" 2>&1)" || rc=$?
    local last
    last="$(printf '%s\n' "$out" | grep '^disk-report:' | tail -1)"
    if [ "$rc" -ne 0 ]; then
        FAIL=$((FAIL + 1)); echo "FAIL $name: exit $rc: $(printf '%s' "$out" | tail -2 | tr '\n' ' ')"
    elif [ "${last#*"$want"}" = "$last" ]; then
        FAIL=$((FAIL + 1)); echo "FAIL $name: want a line containing \"$want\", got \"$last\""
    else
        PASS=$((PASS + 1)); echo "ok   $name: $last"
    fi
}

f="$WORK/s"
printf '10:00:00 900000\n10:00:15 44032\n10:00:30 500000\n' >"$f.good"
case_ "three good samples report the lowest" "$f.good" "lowest free space 43 MiB at 10:00:15 UTC"

printf '10:00:00 5000\n10:00:15 \n' >"$f.emptyval"
case_ "a sample with an empty value is skipped" "$f.emptyval" "1 of 2 samples readable; lowest free space 4 MiB at 10:00:00"

printf '10:00:00 abc\n10:00:15 12x\n10:00:30\n' >"$f.junk"
case_ "only non-numeric samples" "$f.junk" "no readable free-space samples"

printf '10:00:00 2048000\ngarbage line here\n10:00:30 -5\n10:00:45 1048576\n' >"$f.mixed"
case_ "mixed lines keep only the numeric ones" "$f.mixed" "2 of 4 samples readable; lowest free space 1024 MiB at 10:00:45"

: >"$f.empty"
case_ "an empty sample file" "$f.empty" "no free-space samples"

case_ "a missing sample file" "$WORK/does-not-exist" "no free-space samples"

printf '\n\n' >"$f.blank"
case_ "blank lines only" "$f.blank" "no readable free-space samples"

EXPECT_CASES=7
[ "$CASES" -eq "$EXPECT_CASES" ] || { FAIL=$((FAIL + 1)); echo "FAIL ran $CASES cases, expected $EXPECT_CASES"; }
echo "check-disk-report: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ] && [ "$PASS" -eq "$EXPECT_CASES" ]
