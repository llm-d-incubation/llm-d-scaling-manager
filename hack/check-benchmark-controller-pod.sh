#!/usr/bin/env bash
# Executes the benchmark scripts that read the scaling manager controller's log against a
# stub kubectl, and asserts they read the controller's Pods and no other.
#
# They used to select the controller by app.kubernetes.io/name. Warm-pool Pods
# carry that label too, so a pool Pod's log was read as the controller's: its
# lines landed in the captured controller log, in the decision dumps, and in the
# metric-error warning of the two-model verify. The e2e relay Pods carry every
# controller label, control-plane=controller-manager included, so no label
# selector is safe; ownership is (hack/benchmark/controller_pods.py).
#
# The stub serves three Pods that share those labels: the controller, a
# warm-pool Pod and a relay Pod. Each case asserts which Pods a script read the
# log of, and that the controller's lines still arrive. `bash -n` sees none of
# this: the label selector parsed perfectly and selected the wrong Pods.
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BENCH="$ROOT/hack/benchmark"
EXPECTED_CASES=19

for tool in python3; do
    command -v "$tool" >/dev/null 2>&1 || {
        echo "FATAL: $tool is required by this check and is missing. Nothing below would be about the code."
        exit 2
    }
done
python3 -c 'import yaml' 2>/dev/null || {
    echo "FATAL: python3 with PyYAML is required by this check (pip install pyyaml); the dump scripts refuse to start without it."
    exit 2
}

WORK="$(mktemp -d)"
TAIL_PID=""
cleanup() {
    [ -n "$TAIL_PID" ] && { kill -TERM "-$TAIL_PID" 2>/dev/null || kill -TERM "$TAIL_PID" 2>/dev/null || true; }
    rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

fails=0
cases=0
ok()  { cases=$((cases + 1)); echo "  ok: $1"; }
bad() { cases=$((cases + 1)); fails=$((fails + 1)); echo "  FAIL: $1" >&2; }

STUB="$WORK/bin"
mkdir -p "$STUB"
# A kubectl that answers `get pods` and `logs` from a fixed Pod set, applying
# the label selector the way the API server does, and records in $KSERVED every
# Pod whose log it returned. `deploy/<name>` picks a Pod by the Deployment's
# selector, as kubectl does -- which Pod of several is unspecified there, and
# the stub takes the first.
cat > "$STUB/kubectl" <<'STUBEOF'
#!/usr/bin/env python3
import json, os, sys

TS = "2026-10-16T10:00:00.000000Z"
CTL = {"control-plane": "controller-manager",
       "app.kubernetes.io/name": "scaling-manager"}

def pod(name, labels, owner=None):
    meta = {"name": name, "labels": labels}
    if owner:
        meta["ownerReferences"] = [{"apiVersion": "apps/v1", "kind": "ReplicaSet",
                                    "name": owner, "controller": True}]
    return {"metadata": meta}

# Warm pool and relay first: a pick by selector that takes the first match
# lands on one of them, as it can on a real cluster.
PODS = [
    pod("llmsm-warm-pool-demo-def34-x1", {"app.kubernetes.io/name": "scaling-manager",
                                        "pod-template-hash": "def34"},
        "llmsm-warm-pool-demo-def34"),
    pod("relay", dict(CTL)),
    pod("scaling-manager-controller-manager-abc12-a1", dict(CTL, **{"pod-template-hash": "abc12"}),
        "scaling-manager-controller-manager-abc12"),
]
if os.environ.get("STUB_PODS") == "two":
    PODS.append(pod("scaling-manager-controller-manager-fed98-b1", dict(CTL, **{"pod-template-hash": "fed98"}),
                    "scaling-manager-controller-manager-fed98"))
if os.environ.get("STUB_PODS") == "none":
    PODS = PODS[:2]

def terms(sel):
    out, depth, cur = [], 0, ""
    for ch in sel:
        if ch == "(":
            depth += 1
        elif ch == ")":
            depth -= 1
        if ch == "," and depth == 0:
            out.append(cur.strip()); cur = ""
        else:
            cur += ch
    if cur.strip():
        out.append(cur.strip())
    return out

def matches(p, sel):
    labels = p["metadata"]["labels"]
    for t in terms(sel):
        if " in " in t:
            k, vals = t.split(" in ", 1)
            vals = [v.strip() for v in vals.strip().strip("()").split(",")]
            if labels.get(k.strip()) not in vals:
                return False
        elif "=" in t:
            k, v = t.replace("==", "=").split("=", 1)
            if labels.get(k.strip()) != v.strip():
                return False
        else:
            raise SystemExit(f"stub: unsupported selector term {t!r}")
    return True

args = sys.argv[1:]
with open(os.environ.get("KCALLS", os.devnull), "a") as f:
    f.write(" ".join(args) + "\n")
sel, positional, i = None, [], 0
while i < len(args):
    a = args[i]
    if a in ("-n", "--namespace", "--context", "-o", "--output"):
        i += 2; continue
    if a in ("-l", "--selector"):
        sel = args[i + 1]; i += 2; continue
    if a.startswith("-"):
        i += 1; continue
    positional.append(a); i += 1

if positional[:2] == ["get", "pods"]:
    items = [p for p in PODS if sel is None or matches(p, sel)]
    print(json.dumps({"items": items}))
    sys.exit(0)

if positional[:1] == ["logs"]:
    target = positional[1] if len(positional) > 1 else None
    if sel is not None:
        served = [p for p in PODS if matches(p, sel)]
    elif target and target.split("/")[0] in ("deploy", "deployment"):
        served = [p for p in PODS if matches(p, ",".join(f"{k}={v}" for k, v in CTL.items()))][:1]
    elif target:
        served = [p for p in PODS if p["metadata"]["name"] == target.split("/")[-1]]
    else:
        served = []
    if not served:
        print("No resources found", file=sys.stderr)
    with open(os.environ["KSERVED"], "a") as f:
        for p in served:
            f.write(p["metadata"]["name"] + "\n")
    for p in served:
        name = p["metadata"]["name"]
        print(f"{TS}\tINFO\tengine/engine.go:1\tApplied saturation decision via shared cache\t"
              + json.dumps({"variant": name, "target": 1}))
        print(f"{TS}\tERROR\tcollector/prometheus.go:1\tprometheus query error\t"
              + json.dumps({"pod": name}))
    sys.exit(0)

joined = " ".join(args)
print(f"stub: unexpected kubectl {joined}", file=sys.stderr)
sys.exit(1)
STUBEOF
chmod +x "$STUB/kubectl"
export KCALLS="$WORK/calls.log"
export KSERVED="$WORK/served.log"

# served -- the Pods whose log was read since the last reset, sorted, one line.
served() { sort -u "$KSERVED" 2>/dev/null | tr '\n' ' ' | sed 's/ $//'; }
reset_served() { : > "$KSERVED"; }

CTL_A="scaling-manager-controller-manager-abc12-a1"
CTL_B="scaling-manager-controller-manager-fed98-b1"

# expect_served <label> <expected Pods, space separated, sorted>
expect_served() {
    local got
    got="$(served)"
    if [ "$got" = "$2" ]; then
        ok "$1 reads the log of: ${2:-no Pod}"
    else
        bad "$1 read the log of [${got:-nothing}], not [$2]"
    fi
}

# ---- tail_scaling_manager_logs.sh -----------------------------------------------------
# Run for a few reconnects (the stub's `logs -f` returns at once), then stopped.
tail_run() {
    local out="$1"
    reset_served
    KUBECTL_CMD="$STUB/kubectl" bash "$BENCH/tail_scaling_manager_logs.sh" start demo-ns "$out" \
        >"$WORK/tail.o" 2>"$WORK/tail.e" || { bad "tail start failed: $(head -2 "$WORK/tail.e")"; return 1; }
    TAIL_PID="$(cat "$out.pid" 2>/dev/null || true)"
    sleep 3
    KUBECTL_CMD="$STUB/kubectl" bash "$BENCH/tail_scaling_manager_logs.sh" stop demo-ns "$out" \
        >"$WORK/tail.o" 2>"$WORK/tail.e" || bad "tail stop failed: $(head -2 "$WORK/tail.e")"
    TAIL_PID=""
}

echo "tail_scaling_manager_logs.sh"
LOG="$WORK/one/llmsm_controller.log"; mkdir -p "$WORK/one"
export STUB_PODS=one
if tail_run "$LOG"; then
    expect_served "the tail" "$CTL_A"
    grep -q "$CTL_A" "$LOG" \
        && ok "the captured log holds the controller's lines" \
        || bad "the captured log has no controller line: $(head -c 200 "$LOG")"
    if grep -qE 'warm-pool|relay' "$LOG"; then
        bad "the captured log holds another Pod's lines: $(grep -E 'warm-pool|relay' "$LOG" | head -1)"
    else
        ok "the captured log holds no warm-pool or relay line"
    fi
fi
LOG2="$WORK/two/llmsm_controller.log"; mkdir -p "$WORK/two"
export STUB_PODS=two
if tail_run "$LOG2"; then
    expect_served "the tail across a rollout" "$CTL_A $CTL_B"
    grep -q "$CTL_B" "$LOG2" \
        && ok "both controller Pods reach the captured log" \
        || bad "the second controller Pod is missing from the captured log"
fi
# The selector each reconnect used: one logs call, by the controller hashes.
grep -q "logs -n demo-ns -l control-plane=controller-manager,pod-template-hash in (abc12,fed98)" "$KCALLS" \
    && ok "the tail follows every controller Pod in one logs call" \
    || bad "no logs call named both controller hashes: $(grep ' logs ' "$KCALLS" | tail -1)"

# ---- the dump scripts -----------------------------------------------------
# A results directory with no captured tail, so both take the live kubectl path.
RD="$WORK/results/run_1"
mkdir -p "$RD"
cat > "$RD/run_metadata.yaml" <<'META'
harness_start: "2026-10-16T09:59:00Z"
harness_stop: "2026-10-16T10:01:00Z"
META

echo "dump_k2_decisions.py"
reset_served
if STUB_PODS=one PATH="$STUB:$PATH" python3 "$BENCH/dump_k2_decisions.py" "$RD" -n demo-ns \
        >"$WORK/k2.o" 2>"$WORK/k2.e"; then
    ok "dump_k2_decisions.py exits 0"
else
    bad "dump_k2_decisions.py failed: $(tail -2 "$WORK/k2.e")"
fi
expect_served "dump_k2_decisions.py" "$CTL_A"
variants="$(python3 -c '
import json, sys
ev = json.load(open(sys.argv[1]))["events"]
print(" ".join(sorted({e.get("variant", "") for e in ev})))
' "$RD/metrics/processed/k2_decisions.json" 2>/dev/null)"
[ "$variants" = "$CTL_A" ] \
    && ok "its events come from the controller alone" \
    || bad "its events came from [${variants:-nothing}], not [$CTL_A]"

reset_served
if STUB_PODS=two PATH="$STUB:$PATH" python3 "$BENCH/dump_k2_decisions.py" "$RD" -n demo-ns \
        >"$WORK/k2.o" 2>"$WORK/k2.e"; then
    expect_served "dump_k2_decisions.py across a rollout" "$CTL_A $CTL_B"
else
    bad "dump_k2_decisions.py failed across a rollout: $(tail -2 "$WORK/k2.e")"
fi

# No controller Pod at all: an error naming the cause, not a warm-pool log.
reset_served
if STUB_PODS=none PATH="$STUB:$PATH" python3 "$BENCH/dump_k2_decisions.py" "$RD" -n demo-ns \
        >"$WORK/k2.o" 2>"$WORK/k2.e"; then
    bad "dump_k2_decisions.py exited 0 with no controller Pod"
else
    grep -q "no Pod of Deployment scaling-manager-controller-manager" "$WORK/k2.e" \
        && ok "with no controller Pod it refuses and says why" \
        || bad "it refused for another reason: $(tail -1 "$WORK/k2.e")"
fi
expect_served "dump_k2_decisions.py with no controller Pod" ""

echo "dump_scaling_manager_target_timeseries.py"
reset_served
if STUB_PODS=one PATH="$STUB:$PATH" python3 "$BENCH/dump_scaling_manager_target_timeseries.py" "$RD" -n demo-ns \
        >"$WORK/ts.o" 2>"$WORK/ts.e"; then
    ok "dump_scaling_manager_target_timeseries.py exits 0"
else
    bad "dump_scaling_manager_target_timeseries.py failed: $(tail -2 "$WORK/ts.e")"
fi
expect_served "dump_scaling_manager_target_timeseries.py" "$CTL_A"
grep -q "snapshots from controller logs" "$WORK/ts.o" \
    && ok "its samples come from the controller log, not the Thanos fallback" \
    || bad "it did not use the controller log: $(tail -1 "$WORK/ts.o") $(tail -1 "$WORK/ts.e")"
reset_served
STUB_PODS=two PATH="$STUB:$PATH" python3 "$BENCH/dump_scaling_manager_target_timeseries.py" "$RD" -n demo-ns \
    >"$WORK/ts.o" 2>"$WORK/ts.e" || true
expect_served "dump_scaling_manager_target_timeseries.py across a rollout" "$CTL_A $CTL_B"

# ---- two_model_pool.sh ----------------------------------------------------
# Sourced: the metric-error read is one step of `verify`, which needs a cluster.
echo "two_model_pool.sh"
reset_served
errs="$(
    # shellcheck disable=SC1091
    . "$BENCH/two_model_pool.sh" >/dev/null 2>&1
    NS=demo-ns
    PATH="$STUB:$PATH"
    export STUB_PODS=one
    controller_metric_errors 2>&1
)"
expect_served "the verify metric-error read" "$CTL_A"
printf '%s\n' "$errs" | grep -q "$CTL_A" \
    && ok "the verify shows the controller's own metric errors" \
    || bad "the verify showed no controller error: ${errs:-nothing}"
if printf '%s\n' "$errs" | grep -qE 'warm-pool|relay'; then
    bad "the verify shows another Pod's errors as the controller's: $errs"
else
    ok "the verify shows no warm-pool or relay error"
fi

# ---- the count itself ------------------------------------------------------
# Asserted without ok()/bad(), which would change the number being asserted.
if [ "$cases" -eq "$EXPECTED_CASES" ]; then
    echo "  ok: all $EXPECTED_CASES cases ran"
else
    echo "  FAIL: expected $EXPECTED_CASES cases, ran $cases -- a block stopped running" >&2
    fails=$((fails + 1))
fi

echo "benchmark controller-pod checks: $cases run, $fails failed"
[ "$fails" -eq 0 ] || exit 1
