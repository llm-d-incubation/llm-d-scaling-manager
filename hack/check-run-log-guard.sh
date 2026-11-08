#!/usr/bin/env bash
# Executes post_run_analyze.sh against a stub kubectl and fixture controller
# logs, and asserts the zero-line guard (docs/proposals/analyzer-learner.md
# 6.1): the run window must hold the line the controller logs at its -v,
# replica-capacity-summary at 2 and k2-decision at 3 or more; below 2 neither
# is logged and the analysis says it checked nothing; and a verbosity it
# cannot read fails rather than passes.
#
# Before the guard, only k2-decision was asked for, and only through
# dump_k2_decisions.py, which exits 0 on an empty window. So a run at the
# shipped --v=2, or a log that missed the run, finished as analysed. `bash -n`
# sees none of it.
#
# The analysis steps that do not read the controller log are stubbed; the
# guard, dump_k2_decisions.py and controller_pods.py run as they ship.
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BENCH="$ROOT/hack/benchmark"
EXPECTED_CASES=17

command -v python3 >/dev/null 2>&1 || {
    echo "FATAL: python3 is required by this check and is missing. Nothing below would be about the code."
    exit 2
}
python3 -c 'import yaml' 2>/dev/null || {
    echo "FATAL: python3 with PyYAML is required by this check (pip install pyyaml); the dump scripts refuse to start without it."
    exit 2
}

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT INT TERM

fails=0
cases=0
ok()  { cases=$((cases + 1)); echo "  ok: $1"; }
bad() { cases=$((cases + 1)); fails=$((fails + 1)); echo "  FAIL: $1" >&2; }

# The analysis, with the steps that read no controller log stubbed out.
COPY="$WORK/bench"
mkdir -p "$COPY"
cp "$BENCH/post_run_analyze.sh" "$BENCH/dump_k2_decisions.py" "$BENCH/controller_pods.py" "$COPY/"
[ -f "$BENCH/run_log_guard.py" ] && cp "$BENCH/run_log_guard.py" "$COPY/"
for s in dump_scaling_manager_target_timeseries.py dump_capacity_demand_estimate.py dump_epp_throughput.py \
         dump_scaling_manager_full_timeseries.py plot_two_variant_pipeline.py comparison_table.py; do
    printf 'print("stub")\n' > "$COPY/$s"
done

# A kubectl that lists the controller Pods STUB_PODS describes, or fails when
# STUB_KUBECTL=fail. STUB_PODS is ";"-separated, one Pod each: its phase, then
# its manager args, space-separated. Pod i is scaling-manager-controller-manager-h<i>-a<i>.
# Unset, it is one Running Pod with the args in STUB_ARGS. Logs come from the
# captured llmsm_controller.log, so a logs call is an error.
STUB="$WORK/bin"
mkdir -p "$STUB"
cat > "$STUB/kubectl" <<'STUBEOF'
#!/usr/bin/env python3
import json, os, sys

if os.environ.get("STUB_KUBECTL") == "fail":
    print("error: You must be logged in to the server (Unauthorized)", file=sys.stderr)
    sys.exit(1)
if sys.argv[1:3] != ["get", "pods"]:
    print(f"stub kubectl: unexpected call {sys.argv[1:]}", file=sys.stderr)
    sys.exit(1)
spec = os.environ.get("STUB_PODS") or "Running " + os.environ.get("STUB_ARGS", "")
items = []
for i, entry in enumerate(spec.split(";"), start=1):
    phase, *args = entry.split()
    rs = f"scaling-manager-controller-manager-h{i}"
    items.append({
        "metadata": {
            "name": f"{rs}-a{i}",
            "labels": {"control-plane": "controller-manager", "pod-template-hash": f"h{i}"},
            "ownerReferences": [{"apiVersion": "apps/v1", "kind": "ReplicaSet",
                                 "name": rs, "controller": True}],
        },
        "spec": {"containers": [{"name": "manager", "command": ["/manager"],
                                 "args": ["--leader-elect=true"] + args}]},
        "status": {"phase": phase},
    })
print(json.dumps({"items": items}))
STUBEOF
chmod +x "$STUB/kubectl"

WINDOW="2026-10-16T10:00:00+00:00 -> 2026-10-16T10:10:00+00:00"
POD1="scaling-manager-controller-manager-h1-a1"
POD2="scaling-manager-controller-manager-h2-a2"

# line <time> <message> [zone] prints one controller log line in the shipped
# zap console encoding: ts, level, caller, message, fields.
line() {
    printf '2026-10-16T%s.000%s\tINFO\tsaturation/analyze.go:1\t%s\t{"modelID":"m","variant":"v"}\n' "$1" "${3-Z}" "$2"
}

# run <name> <log lines...>: a results dir holding them, through the analysis.
# Leaves its exit status in $RC and its output in $OUT.
run() {
    local rd="$WORK/results/$1"
    shift
    mkdir -p "$rd"
    printf 'harness_start: "2026-10-16T10:00:00Z"\nharness_stop: "2026-10-16T10:10:00Z"\n' > "$rd/run_metadata.yaml"
    : > "$rd/llmsm_controller.log"
    local l
    for l in "$@"; do
        printf '%s\n' "$l" >> "$rd/llmsm_controller.log"
    done
    OUT="$(PATH="$STUB:$PATH" bash "$COPY/post_run_analyze.sh" "$rd" demo-ns 2>&1)"
    RC=$?
}

# says <text>...: the output holds every <text>, or names the first missing.
says() {
    local t
    for t in "$@"; do
        printf '%s\n' "$OUT" | grep -qF -- "$t" || { MISSING="$t"; return 1; }
    done
}

got() { printf '%s\n' "$OUT" | grep -iE 'guard|checked' | tail -3; }

# passes <case> <text>...: the analysis exited 0 and said every <text>.
passes() {
    local name="$1"
    shift
    if [ "$RC" -ne 0 ]; then
        bad "$name: exit $RC, want 0; got: $(got)"
    elif ! says "$@"; then
        bad "$name: exit 0, but no \"$MISSING\"; got: $(got)"
    else
        ok "$name"
    fi
}

# refuses <case> <text>...: the analysis exited non-zero and said every <text>.
refuses() {
    local name="$1"
    shift
    if [ "$RC" -eq 0 ]; then
        bad "$name: exit 0, want a failure; got: $(got)"
    elif ! says "$@"; then
        bad "$name: exit $RC, but no \"$MISSING\"; got: $(got)"
    else
        ok "$name"
    fi
}

SUMMARY="$(line 10:05:00 replica-capacity-summary)"
K2="$(line 10:05:00 k2-decision)"
OTHER="$(line 10:05:00 scheduler-queue-demand)"

echo "at --v=2: replica-capacity-summary"
export STUB_ARGS="--v=2"
run v2-lines "$SUMMARY" "$(line 10:05:30 replica-capacity-summary)" "$OTHER"
passes "--v=2 with replica-capacity-summary lines in the window" \
    "2 replica-capacity-summary lines in the run window $WINDOW at --v=2"
run v2-none "$OTHER" "$K2"
refuses "--v=2 with no replica-capacity-summary line fails, naming the window and the line" \
    "zero replica-capacity-summary lines in the run window $WINDOW" "expected at --v=2"
refuses "a guard failure is named in the summary, and the steps after it still ran" \
    "[6] run_log_guard.py — the run window holds none" "[7/7] comparison_table.py" "Outputs:"
run v2-outside "$(line 09:59:00 replica-capacity-summary)" "$(line 10:11:00 replica-capacity-summary)"
refuses "--v=2 with lines only outside the window fails" \
    "zero replica-capacity-summary lines in the run window $WINDOW"
export STUB_ARGS=""
run shipped "$SUMMARY"
passes "no -v in the args is the shipped --v=2" \
    "1 replica-capacity-summary lines in the run window $WINDOW at --v=2"

echo "below --v=2: nothing to check, and said so"
export STUB_ARGS="-v=1"
run v1 "$OTHER"
passes "--v=1 logs neither line: not a failure, and it says it checked nothing" \
    "nothing to check at --v=1" "The run window was NOT checked" "Not checked:" \
    "[6] run_log_guard.py — the controller ran below --v=2"

echo "at --v=3 or more: k2-decision"
export STUB_ARGS="--v=3"
run v3-lines "$K2" "$SUMMARY"
passes "--v=3 with k2-decision lines in the window" \
    "1 k2-decision lines in the run window $WINDOW at --v=3"
run v3-none "$SUMMARY" "$OTHER"
refuses "--v=3 with no k2-decision line fails, naming the window and the line" \
    "zero k2-decision lines in the run window $WINDOW" "expected at --v=3"
export STUB_ARGS="--v 5"
run v5-split-flag "$K2"
passes "--v 5, the flag and its value as two args" "1 k2-decision lines in the run window $WINDOW at --v=5"

echo "several Pods: the lowest Running one"
STUB_PODS="Running --v=2;Running --v=3" run rollout "$SUMMARY"
passes "a rollout at --v=2 and --v=3 is checked at 2, naming both Pods" \
    "1 replica-capacity-summary lines in the run window $WINDOW at --v=2" \
    "$POD1 --v=2, $POD2 --v=3"
STUB_PODS="Failed --v=1;Running --v=3" run leftover "$K2"
passes "a Failed Pod left by an earlier install at a lower -v is not read" \
    "1 k2-decision lines in the run window $WINDOW at --v=3 (Running controller Pods: $POD2 --v=3)"
STUB_PODS="Failed --v=2;Succeeded --v=2" run none-running "$SUMMARY"
refuses "no Running controller Pod is unreadable, naming what was listed" \
    "could not read what the run must hold" "no Running controller Pod" "$POD1 Failed, $POD2 Succeeded"

echo "the verbosity: read, or said to be unreadable"
export STUB_ARGS="--v=high"
run v-garbage "$SUMMARY" "$K2"
refuses "a -v that is not an integer is unreadable, not the shipped level" \
    "could not read what the run must hold" "not an integer"
export STUB_ARGS="--v=2"
STUB_KUBECTL=fail run v-kubectl-fails "$SUMMARY" "$K2"
refuses "a Pod list kubectl cannot read is unreadable" \
    "could not read what the run must hold" "Unauthorized"
CONTROLLER_VERBOSITY=3 run v-env "$K2"
passes "CONTROLLER_VERBOSITY names the level over the Pods" \
    "1 k2-decision lines in the run window $WINDOW at --v=3 (CONTROLLER_VERBOSITY=3)"
CONTROLLER_VERBOSITY=three run v-env-garbage "$SUMMARY" "$K2"
refuses "a CONTROLLER_VERBOSITY that is not an integer is unreadable" \
    "could not read what the run must hold" "CONTROLLER_VERBOSITY='three' is not an integer"
run naive-stamp "$(line 10:05:00 replica-capacity-summary '')"
refuses "a log line stamped with no time zone is unreadable, not a traceback" \
    "could not read what the run must hold" "replica-capacity-summary line stamped '2026-10-16T10:05:00.000'"

# ---- the count itself ------------------------------------------------------
# Asserted without ok()/bad(), which would change the number being asserted.
if [ "$cases" -eq "$EXPECTED_CASES" ]; then
    echo "  ok: all $EXPECTED_CASES cases ran"
else
    echo "  FAIL: expected $EXPECTED_CASES cases, ran $cases -- a block stopped running" >&2
    fails=$((fails + 1))
fi

echo "run-log guard checks: $cases run, $fails failed"
[ "$fails" -eq 0 ] || exit 1
