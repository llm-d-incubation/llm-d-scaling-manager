#!/usr/bin/env bash
# Runs hack/promjson-to-openmetrics.py, the converter of the capacity
# planner's manual recording (docs/guides/record-for-capacity-planner/), on
# fixture answers, then backfills its output with promtool.
#
# What it guards parses and exits 0 when wrong: a sample read twice at a
# chunk edge written twice (promtool then refuses the block, or keeps one),
# a label value with a quote or a backslash written unescaped (the line
# misparses), a failed chunk skipped (the window reads as no load), and an
# export with no sample at all converted to an empty file.
#
# PROMTOOL is the promtool binary (make promtool installs a pinned one).
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CONVERT="$ROOT/hack/promjson-to-openmetrics.py"
FIX="$ROOT/hack/testdata/promjson-to-openmetrics"
PROMTOOL=${PROMTOOL:-promtool}
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
fail=0

# 1. Two overlapping chunks and an empty answer give the expected text.
if ! python3 "$CONVERT" -o "$TMP/out.om" "$FIX/chunk1.json" "$FIX/chunk2.json" "$FIX/empty.json" 2>"$TMP/err"; then
  echo "FAIL: the converter refused good answers: $(cat "$TMP/err")" >&2
  fail=1
elif ! diff -u "$FIX/expected.om" "$TMP/out.om" >&2; then
  echo "FAIL: the converter's output differs from expected.om" >&2
  fail=1
fi

# 2. Answers the converter must refuse, each naming the file.
for bad in error vector; do
  if python3 "$CONVERT" -o "$TMP/bad.om" "$FIX/chunk1.json" "$FIX/$bad.json" 2>"$TMP/err"; then
    echo "FAIL: the converter accepted $bad.json" >&2
    fail=1
  elif ! grep -q "$bad.json" "$TMP/err"; then
    echo "FAIL: refusing $bad.json, the converter did not name it: $(cat "$TMP/err")" >&2
    fail=1
  fi
done
if python3 "$CONVERT" -o "$TMP/none.om" "$FIX/empty.json" 2>"$TMP/err"; then
  echo "FAIL: the converter wrote a file from answers with no sample" >&2
  fail=1
fi

# 3. promtool backfills the output, and the blocks hold every sample.
if ! command -v "$PROMTOOL" >/dev/null 2>&1; then
  echo "FAIL: promtool not found (PROMTOOL=$PROMTOOL); run make promtool" >&2
  exit 1
fi
want=$(grep -vc '^#' "$FIX/expected.om")
if [ "$want" -lt 1 ]; then
  echo "FAIL: expected.om holds no sample" >&2
  exit 1
fi
if [ "$fail" -eq 0 ]; then
  if ! "$PROMTOOL" tsdb create-blocks-from openmetrics "$TMP/out.om" "$TMP/tsdb" >"$TMP/bf" 2>&1; then
    echo "FAIL: promtool refused the converter's output:" >&2
    cat "$TMP/bf" >&2
    fail=1
  else
    # A read-only open replays the WAL, which a backfill does not write.
    mkdir -p "$TMP/tsdb/wal"
    "$PROMTOOL" tsdb dump --sandbox-dir-root="$TMP" --min-time=0 "$TMP/tsdb" >"$TMP/dump" 2>"$TMP/dumperr"
    got=$(grep -c ' 17000000' "$TMP/dump")
    if [ "$got" -ne "$want" ]; then
      echo "FAIL: the backfilled blocks hold $got samples, not the $want in expected.om" >&2
      cat "$TMP/dump" "$TMP/dumperr" >&2
      fail=1
    fi
  fi
fi

if [ "$fail" -ne 0 ]; then
  exit 1
fi
echo "ok: promjson-to-openmetrics merges chunks, escapes labels, refuses failed answers; promtool backfilled $got of $want samples"
