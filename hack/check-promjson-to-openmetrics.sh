#!/usr/bin/env bash
# Runs hack/promjson-to-openmetrics.py, the converter of the capacity
# planner's manual recording (docs/guides/record-for-capacity-planner/), on
# fixture answers, then backfills its output with promtool.
#
# What it guards parses and exits 0 when wrong: a sample read twice at a
# chunk edge written twice (promtool then refuses the block, or keeps one),
# a label value with a quote or a backslash written unescaped (the line
# misparses), and every way a window loses load without a word: a failed
# chunk (an error body, or no file at all), a native histogram dropped, an
# export with no sample converted to an empty file.
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
GOOD="$FIX/mixed.1700000000.json $FIX/mixed.1700000030.json $FIX/empty.1700000000.json $FIX/empty.1700000030.json"

# 1. Two overlapping chunks and an empty answer give the expected text, and
# the scrape interval read from them (15 s) gives a 60 s lookback.
# shellcheck disable=SC2086
if ! python3 "$CONVERT" -o "$TMP/out.om" $GOOD 2>"$TMP/err"; then
  echo "FAIL: the converter refused good answers: $(cat "$TMP/err")" >&2
  fail=1
elif ! diff -u "$FIX/expected.om" "$TMP/out.om" >&2; then
  echo "FAIL: the converter's output differs from expected.om" >&2
  fail=1
elif ! grep -q -- "--query.lookback-delta=60s" "$TMP/err"; then
  echo "FAIL: the converter did not suggest a 60s lookback for 15 s samples: $(cat "$TMP/err")" >&2
  fail=1
fi

# 2. Answers the converter must refuse, each with the words that name why.
refuse() {
  local what=$1 want=$2
  shift 2
  if python3 "$CONVERT" -o "$TMP/bad.om" "$@" 2>"$TMP/err"; then
    echo "FAIL: the converter accepted $what" >&2
    fail=1
  elif ! grep -q -- "$want" "$TMP/err"; then
    echo "FAIL: refusing $what, the converter did not say \"$want\": $(cat "$TMP/err")" >&2
    fail=1
  fi
}
refuse "a failed query" "error.1700000000.json: the query did not succeed" \
  "$FIX/mixed.1700000000.json" "$FIX/error.1700000000.json"
refuse "an error body that is not JSON" "html.1700000000.json: not JSON" \
  "$FIX/mixed.1700000000.json" "$FIX/html.1700000000.json"
refuse "a vector answer" "vector.1700000000.json: resultType" \
  "$FIX/mixed.1700000000.json" "$FIX/vector.1700000000.json"
refuse "a chunk with no file" "empty.1700000030" \
  "$FIX/mixed.1700000000.json" "$FIX/mixed.1700000030.json" "$FIX/empty.1700000000.json"
refuse "native histograms" "native.1700000000.json: vllm:time_to_first_token_seconds has native histogram" \
  "$FIX/mixed.1700000000.json" "$FIX/native.1700000000.json"
refuse "answers with no sample" "no sample" "$FIX/empty.1700000000.json"
refuse "series with no sample" "no sample" "$FIX/nosamples.1700000000.json"

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
got=0
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
echo "ok: promjson-to-openmetrics merges chunks, escapes labels, refuses 7 broken exports; promtool backfilled $got of $want samples"
