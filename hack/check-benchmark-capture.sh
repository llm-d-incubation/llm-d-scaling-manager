#!/usr/bin/env bash
# Execute the benchmark capture helpers against a stub kubectl and assert the
# properties that cost real data when they were missing.
#
# `bash -n` sees none of this. A capture that replaces the run already on disk,
# that finalises a file it does not own, that watches a cluster nobody named, or
# that kills an unrelated process, all parse perfectly.
#
# Every case corresponds to a defect measured on this code, not to a
# hypothetical. The count is asserted at the end, because a block that stops
# running would otherwise read as green.
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SR="$ROOT/hack/benchmark/sample_replicas.sh"
TL="$ROOT/hack/benchmark/tail_scaling_manager_logs.sh"

EXPECTED_CHECKS=80

WORK="$(mktemp -d)"
STARTED_PIDS=""

# Kill what we started, on any exit path, by process group -- the captures lead
# their own groups. An EXIT-only trap leaves strays when CI is cancelled, and
# killing a wrapper alone reparents its children, which is how an earlier version
# left two hour-long sleeps per run on a developer box.
cleanup() {
    for p in $STARTED_PIDS; do
        kill -TERM "-$p" 2>/dev/null || kill -TERM "$p" 2>/dev/null || true
    done
    rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

fails=0
checks=0
ok() { checks=$((checks + 1)); echo "  ok: $1"; }
bad() { checks=$((checks + 1)); fails=$((fails + 1)); echo "  FAIL: $1" >&2; }

note_pid() { STARTED_PIDS="$STARTED_PIDS $1"; }
pid_of() { cat "$1.pid" 2>/dev/null || true; }
owner_of() { cat "$1.owner" 2>/dev/null || true; }

# The snapshot COUNT, not merely "it parses". A stop that lost the whole capture
# still produced VALID JSON -- {"snapshots": []} -- so every assembly case below
# is asserted against the lines that were really captured. -1 means "not even
# readable", which is distinct from 0 and must not compare equal to a line count.
count_of() {
    python3 - "$1" <<'PYCOUNT'
import json, sys
try:
    print(len(json.load(open(sys.argv[1], encoding="utf-8"))["snapshots"]))
except Exception:
    print(-1)
PYCOUNT
}
# Newlines, not non-blank lines. A line torn by a kill has no trailing newline
# and the assembler drops it, so `wc -l` agrees with the assembler on every input
# the sampler can produce -- where `grep -c .` counts the torn line and would fail
# a case spuriously.
lines_of() { wc -l < "$1" 2>/dev/null | tr -d " " || echo 0; }

cat > "$WORK/kubectl" <<'STUB'
#!/usr/bin/env bash
echo "$@" >> "${STUB_LOG:?}"
echo '{"items":[]}'
STUB
chmod +x "$WORK/kubectl"
export KUBECTL_CMD="$WORK/kubectl"
export STUB_LOG="$WORK/calls.log"
: > "$STUB_LOG"
export REPLICA_SAMPLE_INTERVAL=1

# ---- a populated output is not replaced ---------------------------------
OUT="$WORK/samples.json"
printf '{"snapshots":[{"a":1}]}' > "$OUT"
BEFORE="$(cat "$OUT")"
if bash "$SR" start demo-ns "$OUT" >"$WORK/o" 2>"$WORK/e"; then
    bad "start overwrote a populated output (exit 0)"
else
    grep -q "refusing to start" "$WORK/e" \
        && ok "start refuses a populated output" \
        || bad "start failed without a refusal: $(head -1 "$WORK/e")"
fi
[ "$(cat "$OUT")" = "$BEFORE" ] \
    && ok "the populated output is byte-for-byte untouched" \
    || bad "the populated output was modified: $(head -c 40 "$OUT")"

# ---- a refused start leaves no ownership record behind ------------------
# A refusal that keeps the claim would block the path for good.
[ ! -f "$OUT.owner" ] \
    && ok "a refused start leaves no ownership record" \
    || bad "a refused start left a record: $(owner_of "$OUT")"

# ---- and a stop after a refused start must not corrupt it --------------
bash "$SR" stop "$OUT" demo-ns >/dev/null 2>&1 || true
[ "$(cat "$OUT")" = "$BEFORE" ] \
    && ok "stop after a refused start leaves the file alone" \
    || bad "stop corrupted a file it never owned: $(head -c 48 "$OUT")"

# ---- --force replaces it; --context reaches kubectl and the record ----
: > "$STUB_LOG"
if bash "$SR" --context my-cluster --force start demo-ns "$OUT" >"$WORK/o" 2>&1; then
    ok "start with --force succeeds on a populated output"
    note_pid "$(pid_of "$OUT")"
else
    bad "start --force failed: $(head -2 "$WORK/o")"
fi
sleep 1
grep -q -- "--context my-cluster" "$STUB_LOG" \
    && ok "--context reaches the kubectl calls" \
    || bad "--context never reached kubectl: $(head -2 "$STUB_LOG")"

owner="$(owner_of "$OUT")"
case "$owner" in
    *"namespace=demo-ns"*) ok "the record names the namespace" ;;
    *) bad "record has no namespace: $owner" ;;
esac
case "$owner" in
    *"context=my-cluster"*) ok "the record names the context" ;;
    *) bad "record has no context: $owner" ;;
esac
case "$owner" in
    *starttime=[0-9]*) ok "the record carries the owner start time, so a reused pid is not mistaken for it" ;;
    *) bad "record has no starttime: $owner" ;;
esac

# ---- a LIVE capture is not stealable, whatever its output weighs -------
if bash "$SR" --context other-cluster start demo-ns "$OUT" >"$WORK/o" 2>"$WORK/e"; then
    bad "a second start stole a live capture (exit 0)"
    note_pid "$(pid_of "$OUT")"
else
    grep -q "a capture already owns" "$WORK/e" \
        && ok "a live capture is not stealable" \
        || bad "second start failed for the wrong reason: $(head -1 "$WORK/e")"
fi
FIRST_PID="$(pid_of "$OUT")"
kill -0 "$FIRST_PID" 2>/dev/null \
    && ok "the first capture still runs and still owns the pidfile" \
    || bad "the first capture was displaced"

# ---- the whole capture tree dies on stop ------------------------------
# The sampler runs kubectl and python inside a command substitution, so they are
# grandchildren; `pkill -P` reached children only and they outlived the stop,
# printing BrokenPipeError after the success line.
bash "$SR" --context my-cluster stop "$OUT" demo-ns >"$WORK/o" 2>"$WORK/e" || true
# Polled, not slept: a fixed sleep was the only timing assumption in this file.
for _ in $(seq 1 50); do
    kill -0 "$FIRST_PID" 2>/dev/null || break
    sleep 0.1
done
if kill -0 "$FIRST_PID" 2>/dev/null; then
    bad "the capture survived its own stop"
else
    ok "stop ends the capture"
fi
python3 -c 'import json,sys; json.load(open(sys.argv[1]))' "$OUT" 2>/dev/null \
    && ok "the finalised samples file is valid JSON" \
    || bad "the finalised samples file does not parse"
[ ! -f "$OUT.owner" ] \
    && ok "stop drops the ownership record" \
    || bad "stop left a record behind: $(owner_of "$OUT")"

# ---- stop is idempotent ----------------------------------------------
bash "$SR" stop "$OUT" demo-ns >/dev/null 2>&1 || true
python3 -c 'import json,sys; json.load(open(sys.argv[1]))' "$OUT" 2>/dev/null \
    && ok "a second stop does not corrupt the file" \
    || bad "a second stop corrupted the file: $(tail -c 12 "$OUT")"

# ---- a reused pid is neither honoured nor killed ---------------------
# A pid alone is not an identity. An unrelated process whose pid landed in the
# record used to block start forever AND be killed by stop.
sleep 600 &
VICTIM=$!
note_pid "$VICTIM"
O6="$WORK/recycled.json"
: > "$O6"
printf 'namespace=old context=old started=then pid=%s starttime=1 cmd=x\n' "$VICTIM" > "$O6.owner"
echo "$VICTIM" > "$O6.pid"
if bash "$SR" start fresh-ns "$O6" >"$WORK/o" 2>"$WORK/e"; then
    note_pid "$(pid_of "$O6")"
    ok "a record whose start time does not match is not treated as live"
else
    bad "a reused pid blocked a new capture: $(head -1 "$WORK/e")"
fi
bash "$SR" stop "$O6" fresh-ns >/dev/null 2>&1 || true
kill -0 "$VICTIM" 2>/dev/null \
    && ok "stop did not kill the unrelated process holding that pid" \
    || bad "stop killed an unrelated process"
kill "$VICTIM" 2>/dev/null || true

# ---- mismatches are reported ----------------------------------------
O7="$WORK/mismatch.json"
bash "$SR" --context c-one start ns-one "$O7" >/dev/null 2>&1 && note_pid "$(pid_of "$O7")"
sleep 1
warn="$(bash "$SR" --context c-two stop "$O7" ns-two 2>&1 >/dev/null || true)"
case "$warn" in
    *"namespace=ns-two"*) ok "stop warns on a namespace mismatch" ;;
    *) bad "no namespace warning: $(echo "$warn" | head -1)" ;;
esac
case "$warn" in
    *"context=c-two"*) ok "stop warns on a context mismatch" ;;
    *) bad "no context warning: $(echo "$warn" | head -1)" ;;
esac

# ---- no namespace on stop is not a mismatch -------------------------
O2="$WORK/quiet.json"
bash "$SR" --context c1 start quiet-ns "$O2" >/dev/null 2>&1 && note_pid "$(pid_of "$O2")"
sleep 1
quiet="$(bash "$SR" stop "$O2" 2>&1 >/dev/null || true)"
# Specifically no MISMATCH complaint. It used to assert stderr was empty, which is
# a proxy and not the property: a capture of a namespace with nothing serving now
# says so there, and that message is wanted.
case "$quiet" in
    *"namespace="*|*"context="*)
        bad "stop without a namespace reported a mismatch: $(echo "$quiet" | head -1)" ;;
    *)  ok "stop without a namespace reports no mismatch" ;;
esac

# ---- the stop arguments may be given in either order ----------------
O3="$WORK/order.json"
bash "$SR" start order-ns "$O3" >/dev/null 2>&1 && note_pid "$(pid_of "$O3")"
sleep 1
P3="$(pid_of "$O3")"
bash "$SR" stop order-ns "$O3" >/dev/null 2>&1 || true
if [ -n "$P3" ] && kill -0 "$P3" 2>/dev/null; then
    bad "the reversed stop order left the capture running (pid $P3)"
else
    ok "stop accepts its arguments in either order"
fi

# ---- flags are honoured wherever they appear -----------------------
: > "$STUB_LOG"
O4="$WORK/after.json"
bash "$SR" start after-ns "$O4" --context trailing-ctx >/dev/null 2>&1 && note_pid "$(pid_of "$O4")"
sleep 1
grep -q -- "--context trailing-ctx" "$STUB_LOG" \
    && ok "a flag after the subcommand is still honoured" \
    || bad "a flag after the subcommand was dropped: $(head -1 "$STUB_LOG")"
bash "$SR" stop "$O4" after-ns >/dev/null 2>&1 || true

# ---- a stray positional is an error -------------------------------
if bash "$SR" start a-ns "$WORK/x.json" extra-junk >/dev/null 2>"$WORK/e"; then
    bad "a stray positional was accepted"
else
    grep -q "unexpected argument" "$WORK/e" \
        && ok "a stray positional is refused" \
        || bad "stray positional failed for the wrong reason: $(head -1 "$WORK/e")"
fi

# ---- an empty --context= is an error ------------------------------
if bash "$SR" --context= start a-ns "$WORK/y.json" >/dev/null 2>"$WORK/e"; then
    bad "--context= was accepted and silently meant inherited"
else
    ok "an empty --context= is refused"
fi

# ---- a path that cannot be written fails loudly -------------------
RO="$WORK/ro"
mkdir -p "$RO"
chmod 500 "$RO"
if bash "$SR" --force start demo-ns "$RO/z.json" >"$WORK/o" 2>"$WORK/e"; then
    bad "start reported success for a path it cannot write: $(head -1 "$WORK/o")"
else
    grep -qE "cannot write|cannot create" "$WORK/e" \
        && ok "start refuses a path it cannot write, and says which" \
        || bad "unwritable path failed for the wrong reason: $(head -1 "$WORK/e")"
fi
chmod 700 "$RO"



# ---- a lone namespace is not an output path ------------------------------
# `stop <ns>` used to be accepted as `stop <outfile>`: exit 0, "nothing to stop",
# sampler still running. The tail guards this; the sampler did not, and it is the
# likelier mistake because the tail documents the opposite argument order.
O9="$WORK/lone.json"
bash "$SR" start lone-ns "$O9" >/dev/null 2>&1 && note_pid "$(pid_of "$O9")"
sleep 1
P9="$(pid_of "$O9")"
if bash "$SR" stop lone-ns >/dev/null 2>"$WORK/e"; then
    bad "the sampler accepted a lone namespace as its outfile"
else
    ok "a lone namespace on stop is refused"
fi
if [ -n "$P9" ] && kill -0 "$P9" 2>/dev/null; then
    ok "and the capture is left running rather than reported stopped"
else
    bad "the refused stop killed the capture anyway"
fi
bash "$SR" stop "$O9" lone-ns >/dev/null 2>&1 || true

# ---- a stop that does not stop must fail --------------------------------
# Reporting success and then dropping the record leaves a live capture invisible
# to the tooling, still appending to a file that already has its terminator.
# Simulated with a process that ignores SIGTERM, which is the same shape as a kill
# that cannot be delivered.
O10="$WORK/nokill.json"
bash -c 'trap "" TERM; sleep 300' &
STUBBORN=$!
note_pid "$STUBBORN"
printf '{"snapshots":[' > "$O10"
echo "$STUBBORN" > "$O10.pid"
ST10="$(sed -n 's/^[0-9][0-9]* (.*) //p' "/proc/$STUBBORN/stat" 2>/dev/null | awk '{print $20}')"
printf 'namespace=nk-ns context=nk-ctx started=now pid=%s starttime=%s cmd=x\n' \
    "$STUBBORN" "$ST10" > "$O10.owner"
if bash "$SR" stop "$O10" nk-ns >/dev/null 2>"$WORK/e"; then
    bad "stop reported success for a capture that is still running"
else
    grep -q "did not stop" "$WORK/e" \
        && ok "a stop that cannot end the capture fails and says so" \
        || bad "stop failed without explaining why: $(head -1 "$WORK/e")"
fi
[ -f "$O10.owner" ] \
    && ok "and it keeps the record rather than losing track of the capture" \
    || bad "a failed stop removed the record anyway"
kill -KILL "$STUBBORN" 2>/dev/null || true


# ---- only one of two concurrent stops finalises --------------------------
# Both used to pass capture_owned, both saw the capture already dead, and both
# appended a JSON terminator: 15 of 15 trials produced `...]}]}` while both
# invocations exited 0 reporting "unreadable".
O11="$WORK/concstop.json"
bash "$SR" start cs-ns "$O11" >/dev/null 2>&1 && note_pid "$(pid_of "$O11")"
sleep 1
bash "$SR" stop "$O11" cs-ns >/dev/null 2>&1 &
bash "$SR" stop "$O11" cs-ns >/dev/null 2>&1 &
wait
python3 -c 'import json,sys; json.load(open(sys.argv[1]))' "$O11" 2>/dev/null \
    && ok "two concurrent stops leave valid JSON" \
    || bad "concurrent stops corrupted the file: $(tail -c 12 "$O11")"

# ---- a namespace pair is not an output path ----------------------------
# The lone-argument guard was gated on the argument COUNT, so two namespaces
# skipped it: exit 0, "nothing to stop", capture still running.
O12="$WORK/twoarg.json"
bash "$SR" start ta-ns "$O12" >/dev/null 2>&1 && note_pid "$(pid_of "$O12")"
sleep 1
P12="$(pid_of "$O12")"
if bash "$SR" stop ta-ns other-ns >/dev/null 2>"$WORK/e"; then
    bad "two namespaces were accepted as a stop target"
else
    ok "two arguments that own no record are refused"
fi
if [ -n "$P12" ] && kill -0 "$P12" 2>/dev/null; then
    ok "and that capture is left running rather than reported stopped"
else
    bad "the refused two-argument stop killed the capture"
fi
bash "$SR" stop "$O12" ta-ns >/dev/null 2>&1 || true

# ---- the log tail carries the same guards -------------------------
LOG="$WORK/controller.log"
printf 'EARLIER RUN\n' > "$LOG"
if bash "$TL" start demo-ns "$LOG" >/dev/null 2>"$WORK/e"; then
    bad "tail start overwrote a populated log"
else
    grep -q "refusing to start" "$WORK/e" \
        && ok "tail start refuses a populated log" \
        || bad "tail start failed without a refusal"
fi
[ "$(cat "$LOG")" = "EARLIER RUN" ] \
    && ok "the populated log is untouched" \
    || bad "the populated log was modified"

: > "$STUB_LOG"
bash "$TL" --context other-cluster --force start demo-ns "$LOG" >/dev/null 2>&1 \
    && note_pid "$(pid_of "$LOG")"
sleep 1
grep -q -- "--context other-cluster" "$STUB_LOG" \
    && ok "tail passes --context to kubectl" \
    || bad "tail did not pass --context: $(head -1 "$STUB_LOG")"

# a live tail whose output is still empty must not be stealable
: > "$LOG"
if bash "$TL" --context third-cluster start demo-ns "$LOG" >/dev/null 2>"$WORK/e"; then
    bad "a second tail stole a live capture with an empty log"
    note_pid "$(pid_of "$LOG")"
else
    grep -q "a capture already owns" "$WORK/e" \
        && ok "a live tail with an empty log is not stealable" \
        || bad "second tail failed for the wrong reason: $(head -1 "$WORK/e")"
fi

# tail stop requires its outfile: making it optional turned `stop <ns>` into
# exit 0, "nothing to stop", and a capture left running.
if bash "$TL" stop demo-ns >/dev/null 2>"$WORK/e"; then
    bad "tail stop accepted a missing outfile and reported success"
else
    ok "tail stop requires its outfile"
fi
bash "$TL" --context other-cluster stop demo-ns "$LOG" >/dev/null 2>&1 || true




# ---- a stop that cannot terminate the file does not report success ------
# A read-only output FILE no longer defeats a stop: the artefact is assembled to a
# temp file and renamed, and a rename needs no write permission on its target. A
# read-only DIRECTORY does still prevent it, and that must be reported rather than
# reported as success.
RODIR="$WORK/rodir"
mkdir -p "$RODIR"
O15="$RODIR/rofile.json"
bash "$SR" start ro-ns "$O15" >/dev/null 2>&1 && note_pid "$(pid_of "$O15")"
sleep 1
chmod 500 "$RODIR"
if bash "$SR" stop "$O15" ro-ns >/dev/null 2>"$WORK/e"; then
    bad "stop reported success when it could not assemble the artefact"
else
    grep -q "could not assemble" "$WORK/e" \
        && ok "a stop that cannot assemble its artefact says so and fails" \
        || bad "the assembly failure was not reported: $(head -1 "$WORK/e")"
fi
chmod 700 "$RODIR" 2>/dev/null || true

# ---- an output path containing a pipe --------------------------------
# The resolver used to return "<out>|<ns>" in one string, so such a path was split
# in the middle: stop reported success against a truncated name and the capture
# kept running.
O16="$WORK/pipe|name.json"
bash "$SR" start pp-ns "$O16" >/dev/null 2>&1 && note_pid "$(pid_of "$O16")"
sleep 1
P16="$(pid_of "$O16")"
bash "$SR" stop "$O16" pp-ns >/dev/null 2>&1 || true
if [ -n "$P16" ] && kill -0 "$P16" 2>/dev/null; then
    bad "a path containing a pipe left the capture running"
else
    ok "an output path containing a pipe is handled whole"
fi

# ---- the tail: concurrent stops, and the dedup temp name -------------
LC="$WORK/conc.log"
bash "$TL" --context tc-ctx start tc-ns "$LC" >/dev/null 2>&1 && note_pid "$(pid_of "$LC")"
sleep 1
printf 'dup\ndup\nuniq\n' >> "$LC"
bash "$TL" stop tc-ns "$LC" >"$WORK/o1" 2>"$WORK/e1" &
bash "$TL" stop tc-ns "$LC" >"$WORK/o2" 2>"$WORK/e2" &
wait
grep -q "cannot stat" "$WORK/e1" "$WORK/e2" 2>/dev/null \
    && bad "two tail stops raced on one dedup temp name" \
    || ok "two concurrent tail stops do not race on a temp name"
ls "$LC".dedup* >/dev/null 2>&1 \
    && bad "a dedup temp file was left behind" \
    || ok "no dedup temp file survives a tail stop"

# ---- the tail refuses a namespace pair too --------------------------
# The sampler grew this guard in round 4; the tail never did, which is the
# asymmetry the shared library exists to prevent.
LT="$WORK/twoarg.log"
bash "$TL" start tt-ns "$LT" >/dev/null 2>&1 && note_pid "$(pid_of "$LT")"
sleep 1
PT="$(pid_of "$LT")"
if bash "$TL" stop tt-ns other-ns >/dev/null 2>"$WORK/e"; then
    bad "the tail accepted two namespaces as a stop target"
else
    ok "the tail refuses two arguments that own no record"
fi
if [ -n "$PT" ] && kill -0 "$PT" 2>/dev/null; then
    ok "and leaves that tail running rather than reporting it stopped"
else
    bad "the refused two-argument tail stop killed the capture"
fi
bash "$TL" stop tt-ns "$LT" >/dev/null 2>&1 || true



# ---- the filing decision ---------------------------------------------
FC="$ROOT/hack/benchmark/file_capture.sh"
mkres() { rm -rf "$WORK/res"; mkdir -p "$WORK/res"; }

# a good set: everything is filed
mkres
printf '{"snapshots":[{"a":1}]}' > "$WORK/f.json"
printf '{"pods":[{"name":"p"}]}' > "$WORK/f.json.pod_timings.json"
printf 'log line\n' > "$WORK/f.log"
out="$(bash "$FC" "$WORK/res" "$WORK/f.json" "$WORK/f.log" 2>&1)"
[ -f "$WORK/res/metrics/processed/llmsm_replica_samples.json" ] \
    && [ -f "$WORK/res/metrics/processed/llmsm_pod_timings.json" ] \
    && [ -f "$WORK/res/llmsm_controller.log" ] \
    && ok "a complete capture files all three artefacts" \
    || bad "a complete capture did not file everything: $out"

# a corrupt samples file must not take the others down with it
mkres
printf '{"snapshots":[{"a":1}' > "$WORK/f.json"
out="$(bash "$FC" "$WORK/res" "$WORK/f.json" "$WORK/f.log" 2>&1)"
[ ! -f "$WORK/res/metrics/processed/llmsm_replica_samples.json" ] \
    && ok "an unparseable samples file is withheld" \
    || bad "an unparseable samples file was filed"
[ -f "$WORK/res/metrics/processed/llmsm_pod_timings.json" ] \
    && ok "and valid pod timings are still filed beside it" \
    || bad "valid pod timings died with the samples file: $out"
[ -f "$WORK/res/llmsm_controller.log" ] \
    && ok "and so is the controller log" \
    || bad "the controller log died with the samples file"

# a capture that never started must not file whatever is at the path
mkres
printf '{"snapshots":[{"PREVIOUS":"RUN"}]}' > "$WORK/f.json"
touch "$WORK/f.json.startfailed"
out="$(bash "$FC" "$WORK/res" "$WORK/f.json" "$WORK/f.log" 2>&1)"
[ ! -f "$WORK/res/metrics/processed/llmsm_replica_samples.json" ] \
    && ok "a capture that never started files nothing from its path" \
    || bad "a previous run's file was filed as this run's measurement"
case "$out" in
    *"never started"*) ok "and says that is why" ;;
    *) bad "the reason was not reported: $out" ;;
esac
rm -f "$WORK/f.json.startfailed"

# a capture that could not be stopped
mkres
printf '{"snapshots":[{"a":1}]}' > "$WORK/f.json"
touch "$WORK/f.json.stopfailed"
out="$(bash "$FC" "$WORK/res" "$WORK/f.json" "$WORK/f.log" 2>&1)"
[ ! -f "$WORK/res/metrics/processed/llmsm_replica_samples.json" ] \
    && ok "a capture that could not be stopped is withheld" \
    || bad "an incomplete capture was filed"
rm -f "$WORK/f.json.stopfailed"

# no results directory is not an error
out="$(bash "$FC" "$WORK/nosuchdir" "$WORK/f.json" "$WORK/f.log" 2>&1)"; rc=$?
[ "$rc" -eq 0 ] \
    && ok "a run with no results directory files nothing and does not fail" \
    || bad "a missing results directory was treated as an error"

# ---- assembling the artefact is idempotent ------------------------------
# This is the property that replaced seven rounds of finalise machinery. The
# capture appends one JSON object per line and stop assembles the array from those
# lines, so the answer cannot depend on who ran it, how many times, or whether a
# previous attempt was killed part-way.
OI="$WORK/idem.json"
bash "$SR" start idem-ns "$OI" >/dev/null 2>&1 && note_pid "$(pid_of "$OI")"
sleep 2
captured="$(lines_of "$OI.snapshots.jsonl")"
bash "$SR" stop "$OI" idem-ns >/dev/null 2>&1 || true
got="$(count_of "$OI")"
[ "$got" -gt 0 ] && [ "$got" = "$captured" ] \
    && ok "a stop assembles every captured line ($got snapshots)" \
    || bad "the artefact holds $got snapshots, not the $captured captured"

# ---- two concurrent stops produce identical bytes ----------------------
OC="$WORK/conc2.json"
bash "$SR" start c2-ns "$OC" >/dev/null 2>&1 && note_pid "$(pid_of "$OC")"
sleep 2
captured="$(lines_of "$OC.snapshots.jsonl")"
bash "$SR" stop "$OC" c2-ns >/dev/null 2>&1 &
bash "$SR" stop "$OC" c2-ns >/dev/null 2>&1 &
wait
# Validity was the whole assertion here for one round, and it passed while the
# loser of the two stops was writing {"snapshots": []} over the winner's work.
got="$(count_of "$OC")"
[ "$got" -gt 0 ] && [ "$got" = "$captured" ] \
    && ok "two concurrent stops leave every captured snapshot ($got)" \
    || bad "concurrent stops left $got snapshots, not the $captured captured"

# ---- and a stop repeated on the assembled file changes nothing ---------
# Where the old design appended a second terminator, re-running is now a no-op:
# the record is gone, so stop declines, and even if it did not the assembly is a
# function of the lines.
before="$(cat "$OC" 2>/dev/null)"
bash "$SR" stop "$OC" c2-ns >/dev/null 2>&1 || true
[ "$(cat "$OC" 2>/dev/null)" = "$before" ] \
    && ok "repeating a stop leaves the artefact byte-identical" \
    || bad "a repeated stop changed the artefact: $(tail -c 20 "$OC")"

# ---- a torn line costs its own snapshot, not the measurement -----------
# A kill mid-write can leave a partial line. The snapshots before it are still a
# measurement, so assembly drops the torn line rather than failing.
OT="$WORK/torn.json"
bash "$SR" start torn-ns "$OT" >/dev/null 2>&1 && note_pid "$(pid_of "$OT")"
sleep 2
printf '{"timestamp": "partial' >> "$OT.snapshots.jsonl"
bash "$SR" stop "$OT" torn-ns >/dev/null 2>&1 || true
python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); sys.exit(0 if d["snapshots"] else 1)' "$OT" 2>/dev/null \
    && ok "a torn line is dropped and the earlier snapshots survive" \
    || bad "a torn line cost the whole measurement: $(head -c 40 "$OT")"

# ---- the temp file does not survive a stop; the INPUT does -------------
ls "$OT".assembling.* >/dev/null 2>&1 \
    && bad "an intermediate .assembling file survived the stop" \
    || ok "no intermediate file survives a stop"

# Deleting the captured lines is what let a second stop read "absent" as "empty"
# and overwrite a good artefact. They stay: they are what makes assembly
# re-runnable, the path already carries the run id, and start truncates it.
[ -s "$OT.snapshots.jsonl" ] \
    && ok "the captured lines survive, so assembly stays re-runnable" \
    || bad "the stop deleted the lines it assembled from"

# ---- an absent input is an error, not an empty measurement -------------
# Measured: a second stop landing between the first one's assembly and its `rm`
# read the absent file as zero snapshots and wrote {"snapshots": []} over the good
# artefact -- valid JSON, wrong content, exit 0, filed as the run's measurement.
# 19 of 20 trials in a 40-80 ms band.
OM="$WORK/missing.json"
bash "$SR" start miss-ns "$OM" >/dev/null 2>&1
MP="$(pid_of "$OM")"
note_pid "$MP"
sleep 2
kill -9 "-$MP" 2>/dev/null || kill -9 "$MP" 2>/dev/null || true
sleep 1
printf '{"snapshots":[{"REAL":"CAPTURE"}]}' > "$OM"
rm -f "$OM.snapshots.jsonl"
if bash "$SR" stop "$OM" miss-ns >/dev/null 2>&1; then
    bad "a stop whose captured lines were gone reported success"
else
    ok "a stop whose captured lines were gone fails instead of filing an empty artefact"
fi
grep -q REAL "$OM" 2>/dev/null \
    && ok "and it leaves the artefact already on disk alone" \
    || bad "an absent input overwrote the artefact: $(head -c 40 "$OM" 2>/dev/null)"

# ---- a claim treats an unreadable start time as "still ours" -----------
# The record ALWAYS carries a starttime= field, so on a host without /proc it is
# present and empty. Reading that as "not ours" declared a LIVE capture dead: stop
# killed nothing and reported success, a second start took the path, and two
# samplers appended to one file -- two namespaces' controllers summed into one
# valid, wrong fleet curve.
if (
    . "$ROOT/hack/benchmark/capture_lib.sh"
    sleep 300 &
    live=$!
    OA="$WORK/alive.json"
    printf 'namespace=a-ns context=x started=now pid=%s starttime= cmd=x\n' "$live" \
        > "$OA.owner"
    capture_alive "$OA" && r=alive || r=dead
    kill "$live" 2>/dev/null || true
    [ "$r" = alive ]
); then
    ok "a record with an unreadable start time is treated as live"
else
    bad "an empty starttime declared a live capture dead"
fi

# ---- a capture that never started files NEITHER of its artefacts --------
# Both come from the sampler, so both consult the sampler's flags. Deriving the
# flag name from the artefact made the timings check a name nothing writes, and a
# previous run's timings were filed as this run's measurement.
mkres
printf '{"snapshots":[{"PREVIOUS":"RUN"}]}' > "$WORK/f.json"
printf '{"pods":[{"name":"FROM-A-PREVIOUS-RUN"}]}' > "$WORK/f.json.pod_timings.json"
touch "$WORK/f.json.startfailed"
out="$(bash "$FC" "$WORK/res" "$WORK/f.json" "$WORK/f.log" 2>&1)"
[ ! -f "$WORK/res/metrics/processed/llmsm_pod_timings.json" ] \
    && ok "a capture that never started files neither of its artefacts" \
    || bad "a previous run's pod timings were filed: $out"
rm -f "$WORK/f.json.startfailed" "$WORK/f.json.pod_timings.json"

# ---- but a STOP refuses to signal what it cannot identify ---------------
# The same record, the opposite decision. "May this path be in use?" fails closed
# by assuming yes, which is what stops a second capture stealing it. Using that
# answer to decide what to KILL inverts it: fed this record with a recycled pid,
# stop sent TERM to an unrelated live process -- and the case above certified the
# predicate that did it. So a stop that can prove nothing refuses instead.
if (
    # Asserted, not assumed: against a tree with no capture_lib.sh this block used
    # to pass because `.` failed, capture_stop was not a command, and
    # command-not-found supplied the non-zero this case was reading as the refusal.
    . "$ROOT/hack/benchmark/capture_lib.sh" || exit 9
    command -v capture_stop >/dev/null || exit 9
    sleep 300 &
    victim=$!
    OS="$WORK/nosignal.json"
    printf 'namespace=s-ns context=x started=now pid=%s starttime= cmd=x\n' "$victim" \
        > "$OS.owner"
    echo "$victim" > "$OS.pid"
    capture_stop "$OS" "$OS.pid" >/dev/null 2>&1 && rc=0 || rc=1
    kill -0 "$victim" 2>/dev/null && survived=yes || survived=no
    kill "$victim" 2>/dev/null || true
    [ "$rc" = 1 ] && [ "$survived" = yes ]
); then
    ok "a stop that cannot identify its pid refuses instead of signalling it"
else
    bad "a stop killed a process it could not prove was its own, or reported success"
fi

# ---- a start that cannot truncate the captured lines refuses ------------
# The artefact is a function of this file and nothing else, so a stale one that
# cannot be truncated becomes THIS run's measurement. Measured: 1 snapshot, exit 0,
# a previous run's replica counts in the report table -- the only way this capture
# publishes a wrong NUMBER rather than losing a run.
OZ="$WORK/stalelines.json"
: > "$OZ.snapshots.jsonl"
chmod 444 "$OZ.snapshots.jsonl"
if bash "$SR" start stale-ns "$OZ" >/dev/null 2>&1; then
    bad "a start that could not truncate the captured lines reported success"
    note_pid "$(pid_of "$OZ")"
else
    ok "a start that cannot truncate the captured lines refuses"
fi
# Empty, not absent: capture_verify_writable proves the path writable by creating
# it, which is the point of it. What must not exist is a MEASUREMENT.
[ ! -s "$OZ" ] \
    && ok "and files no measurement for a run it refused to start" \
    || bad "a refused start still left a measurement: $(head -c 40 "$OZ" 2>/dev/null)"
chmod 644 "$OZ.snapshots.jsonl"

# ---- a start whose stderr channel is unwritable refuses -----------------
# This is how the sampler died at fork: the subshell's own `2>>` redirect failed,
# $! still yielded its pid, and the record was written for a process that no longer
# existed. start said "replica sampler started" and the run filed {"snapshots": []}
# with nothing set to withhold it. That dead-pid record is also the ordinary route
# to an EMPTY starttime= on a host that has /proc.
OE="$WORK/nostderr.json"
: > "$OE.stderr"
chmod 444 "$OE.stderr"
if bash "$SR" start noerr-ns "$OE" >/dev/null 2>&1; then
    bad "a start with an unwritable stderr channel reported success"
    note_pid "$(pid_of "$OE")"
else
    ok "a start that cannot write its diagnostics channel refuses"
fi
chmod 644 "$OE.stderr"

# ---- a start truncates the stderr channel -------------------------------
# Left alone, it carried the PREVIOUS run's reason for anyone reading it to explain
# this run's bad capture.
OQ="$WORK/staleerr.json"
printf 'STALE-FROM-AN-EARLIER-RUN\n' > "$OQ.stderr"
bash "$SR" start staleerr-ns "$OQ" >/dev/null 2>&1 && note_pid "$(pid_of "$OQ")"
sleep 1
grep -q STALE-FROM-AN-EARLIER-RUN "$OQ.stderr" 2>/dev/null \
    && bad "a start left the previous run's diagnostics in place" \
    || ok "a start truncates the diagnostics channel"
bash "$SR" stop "$OQ" staleerr-ns >/dev/null 2>&1 || true

# ---- a crashed capture's lines are a measurement, and are guarded -------
# The artefact is assembled at stop, so a capture killed before its stop leaves
# $OUT at zero bytes and every snapshot it took in the lines beside it. Guarding
# only $OUT meant the next start on that path silently truncated the sole copy --
# the recovery the guide promises, gone without a word.
OK2="$WORK/crashed.json"
: > "$OK2"
printf '{"timestamp":"FROM-A-CRASHED-RUN","controllers":[]}\n' > "$OK2.snapshots.jsonl"
if bash "$SR" start crashed-ns "$OK2" >/dev/null 2>&1; then
    bad "a start discarded a crashed capture's lines without --force"
    note_pid "$(pid_of "$OK2")"
else
    ok "a crashed capture's lines are guarded like any other measurement"
fi
grep -q FROM-A-CRASHED-RUN "$OK2.snapshots.jsonl" 2>/dev/null \
    && ok "and they are still there after the refusal" \
    || bad "the refused start truncated them anyway"

# ---- a capture whose polls all fail is not an empty namespace -----------
# It produced one snapshot per interval with an empty controller list, which is
# byte-identical to a namespace with nothing serving, and the reason was discarded
# by `2>/dev/null` at source -- so the `.stderr` the recipe files was 0 bytes while
# every call was being rejected. The script's own header says it exists because the
# harness "came back with snapshots but no controllers"; it then did the same.
cat > "$WORK/kubectl-broken" <<'BROKEN'
#!/usr/bin/env bash
echo "error: You must be logged in to the server (Unauthorized)" >&2
exit 1
BROKEN
chmod +x "$WORK/kubectl-broken"
OB="$WORK/broken.json"
( export KUBECTL_CMD="$WORK/kubectl-broken"
  bash "$SR" start broken-ns "$OB" >/dev/null 2>&1 ) && note_pid "$(pid_of "$OB")"
sleep 3
out="$(KUBECTL_CMD="$WORK/kubectl-broken" bash "$SR" stop "$OB" broken-ns 2>&1)" && rc=0 || rc=1
[ "$rc" = 1 ] \
    && ok "a capture whose polls all failed fails the stop instead of reporting a measurement" \
    || bad "a capture that measured nothing reported success: $out"
case "$out" in
    *Unauthorized*) ok "and the stop says what the polls were failing with" ;;
    *) bad "the reason was discarded: $out" ;;
esac
[ -s "$OB.stderr" ] \
    && ok "and the diagnostics channel carries it rather than 0 bytes" \
    || bad "the stderr channel is empty while every poll was rejected"
# and nothing is fabricated into the artefact
got="$(count_of "$OB")"
[ "$got" = 0 ] \
    && ok "and no snapshots were fabricated from the failures" \
    || bad "failed polls were recorded as $got snapshot(s) of nothing"

# ---- but a namespace with nothing serving IS a real, empty answer --------
# The same zero controllers, the opposite verdict. Failing this one would make a
# correct capture of an idle namespace look like a broken cluster.
OY="$WORK/empty.json"
bash "$SR" start empty-ns "$OY" >/dev/null 2>&1 && note_pid "$(pid_of "$OY")"
sleep 2
out="$(bash "$SR" stop "$OY" empty-ns 2>&1)" && rc=0 || rc=1
[ "$rc" = 0 ] \
    && ok "a namespace with nothing serving is a successful, empty capture" \
    || bad "an empty namespace was reported as a failure: $out"
case "$out" in
    *"no serving controllers were seen"*) ok "and the stop says so rather than leaving it to be read off a zero" ;;
    *) bad "a capture with no controllers said nothing about it: $out" ;;
esac

# ---- a chatty cluster does not turn an empty namespace into a failure ----
# The verdict used to trigger on "did anything write to stderr", so a kubectl that
# prints a deprecation line while answering every poll correctly made an honestly
# empty namespace exit 1 -- with the deprecation quoted as the reason the polls were
# failing -- and the recipe then withheld BOTH artefacts of a correct capture. The
# discriminator is the snapshot count; stderr is the reason, never the trigger.
cat > "$WORK/kubectl-chatty" <<'CHATTY'
#!/usr/bin/env bash
echo "W0000 client config: the gcp auth plugin is deprecated" >&2
echo '{"items":[]}'
CHATTY
chmod +x "$WORK/kubectl-chatty"
OW="$WORK/chatty.json"
( export KUBECTL_CMD="$WORK/kubectl-chatty"
  bash "$SR" start chatty-ns "$OW" >/dev/null 2>&1 ) && note_pid "$(pid_of "$OW")"
sleep 3
out="$(KUBECTL_CMD="$WORK/kubectl-chatty" bash "$SR" stop "$OW" chatty-ns 2>&1)" && rc=0 || rc=1
[ "$rc" = 0 ] \
    && ok "a chatty kubectl does not fail a correct capture of an empty namespace" \
    || bad "a deprecation warning withheld a correct capture: $out"
got="$(count_of "$OW")"
[ "$got" -gt 0 ] \
    && ok "and its snapshots are kept ($got)" \
    || bad "the capture was reported empty despite answering every poll: $got"

# ---- nor does one failing poll among answering ones ----------------------
# Pod polls Forbidden by an RBAC scope, deployment polls answering: the `|| true`
# keeps the run going, and its stderr used to trip the verdict on 3 good snapshots.
cat > "$WORK/kubectl-nopods" <<'NOPODS'
#!/usr/bin/env bash
for a in "$@"; do
  case "$a" in
    pods) echo "Error from server (Forbidden): pods is forbidden" >&2; exit 1 ;;
  esac
done
echo '{"items":[]}'
NOPODS
chmod +x "$WORK/kubectl-nopods"
OP="$WORK/nopods.json"
( export KUBECTL_CMD="$WORK/kubectl-nopods"
  bash "$SR" start nopods-ns "$OP" >/dev/null 2>&1 ) && note_pid "$(pid_of "$OP")"
sleep 3
out="$(KUBECTL_CMD="$WORK/kubectl-nopods" bash "$SR" stop "$OP" nopods-ns 2>&1)" && rc=0 || rc=1
[ "$rc" = 0 ] \
    && ok "a failing pod poll does not withhold a replica curve that was measured" \
    || bad "one Forbidden poll withheld the whole capture: $out"

# ---- pid 0 and pid 1 are never signalled --------------------------------
# The signal is sent to the process GROUP, and `kill -TERM -1` does not mean group 1:
# it means every process this user may signal. `kill -TERM -0` means the caller's own
# group. Found by running it -- a record naming pid 1 took down this suite mid-run,
# which is also the only reason the negation was ever examined. Reachable with a
# stale pidfile as root, so it is guarded at the signal where all paths converge.
for victimpid in 0 1; do
    if (
        . "$ROOT/hack/benchmark/capture_lib.sh" || exit 9
        command -v capture_stop >/dev/null || exit 9
        # The library's own parse, so this case cannot drift from the code it covers.
        st="$(_capture_starttime "$victimpid")" || st=""
        OU="$WORK/nosignal-$victimpid.json"
        printf 'namespace=u-ns context=x started=now pid=%s starttime=%s cmd=x\n' \
            "$victimpid" "$st" > "$OU.owner"
        echo "$victimpid" > "$OU.pid"
        capture_stop "$OU" "$OU.pid" >/dev/null 2>&1 && exit 1
        # and it keeps the record rather than dropping the path on a bad reading
        [ -s "$OU.owner" ]
    ); then
        ok "pid $victimpid is refused rather than signalled as a process group"
    else
        bad "capture_stop was willing to signal pid $victimpid, or dropped its record"
    fi
done

# ---- a capture we may not signal has not stopped -------------------------
# The other half of the same predicate pair. capture_alive reads EPERM as "gone", so
# consulting it first merged "cannot be signalled" with "is not running": a capture
# capture_identified PROVES is ours and live got its artefact assembled and its
# record dropped while it went on appending. Another user's capture at a hand-named
# path is the reachable case.
if (
    . "$ROOT/hack/benchmark/capture_lib.sh" || exit 9
    command -v capture_stop >/dev/null || exit 9
    # Found, not hardcoded: readable, not signalable, and not 0 or 1 -- those are
    # refused earlier now, for the process-group reason.
    #
    # The OLDEST such process, not the first /proc lists. The first is whatever
    # sorts lowest as a string, and on WSL that is often the root-owned Relay of
    # another wsl.exe session: it exits when that command does, and a target that
    # exits during the stop's 5 s confirmation loop is "not running" -- a correct
    # exit 0 that this case read as the defect.
    other="" st=""
    for p in $(ls /proc 2>/dev/null); do
        case "$p" in
            ''|*[!0-9]*|0|1) continue ;;
        esac
        [ -r "/proc/$p/stat" ] || continue
        kill -0 "$p" 2>/dev/null && continue
        s="$(_capture_starttime "$p")" || continue
        [ -n "$s" ] || continue
        if [ -z "$other" ] || [ "$s" -lt "$st" ]; then other="$p"; st="$s"; fi
    done
    # No such process means the premise is absent, not that the property holds.
    [ -n "$other" ] || exit 9
    OV="$WORK/unsignalable.json"
    printf 'namespace=v-ns context=x started=now pid=%s starttime=%s cmd=x\n' \
        "$other" "$st" > "$OV.owner"
    echo "$other" > "$OV.pid"
    # It must NOT report success, and must keep the record. Say which process it
    # was and whether it outlived the stop, so a failure here is readable.
    if capture_stop "$OV" "$OV.pid" >/dev/null 2>&1; then
        echo "  target pid $other ($(cat "/proc/$other/comm" 2>/dev/null || echo 'gone before the stop returned'))" >&2
        exit 1
    fi
    [ -s "$OV.owner" ]
); then
    ok "a live capture we may not signal is refused, not reported stopped"
else
    bad "a capture that could not be signalled was reported stopped, or lost its record"
fi

# ---- the count itself --------------------------------------------
# Asserted without ok()/bad(), which would change the number being asserted.
if [ "$checks" -eq "$EXPECTED_CHECKS" ]; then
    echo "  ok: all $EXPECTED_CHECKS checks ran"
else
    echo "  FAIL: expected $EXPECTED_CHECKS checks, ran $checks -- a block stopped running" >&2
    fails=$((fails + 1))
fi

echo "benchmark capture checks: $checks run, $fails failed"
[ "$fails" -eq 0 ] || exit 1
