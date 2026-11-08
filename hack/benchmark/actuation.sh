#!/usr/bin/env bash
# Measure ACTUATION LATENCY: how long capacity takes to arrive after a scale-up.
#
# This is the claim FMA actually makes. It does not make tokens faster; it makes
# a replica ready sooner -- by waking a sleeping vLLM (seconds) instead of
# building one (a model load). A load benchmark mostly measures whether the model
# server keeps up, which FMA does not touch, and it fails for reasons that have
# nothing to do with the thing under test: a harness pod that exits non-zero, an
# endpoint that is not serving yet, a report converter that cannot read its own
# input. Every one of those cost us a run.
#
# So measure the mechanism directly: scale the target up, timestamp each new
# replica from creation to Ready, scale back, repeat. No load generator, no
# router, no Prometheus -- just the Kubernetes API. It runs in minutes and is
# repeatable, so a regression is visible rather than inferred.
#
# Deliberately measures POD creation -> Ready, not decision -> Ready. The
# decision latency (KEDA poll + the scaling manager interval) is identical for both variants and
# would only add noise to the comparison; actuation is the part that differs.
#
# Usage:
#   actuation.sh <deployment> [trials]
# Env:
#   ACTUATION_NS     namespace (default $BENCHMARK_NAMESPACE)
#   ACTUATION_BASE   replicas to sit at between trials (default 1)
#   ACTUATION_STEP   how many to add per trial (default 1)
#   ACTUATION_JSON   also write raw samples here
set -u
# --help prints this file's header comment -- the documentation the script
# already carries, so it cannot drift from what the script does. Placed before
# any argument handling because several of these take a namespace as $1, and
# without it `--help` was consumed as one.
case "${1:-}" in
    -h|--help)
        sed -n '2,/^[^#]/p' "$0" | sed 's/^# \{0,1\}//; $d'
        exit 0
        ;;
esac


DEPLOY="${1:?usage: actuation.sh <deployment> [trials]}"
TRIALS="${2:-5}"
NS="${ACTUATION_NS:-${BENCHMARK_NAMESPACE:-}}"
BASE="${ACTUATION_BASE:-1}"
STEP="${ACTUATION_STEP:-1}"
JSON="${ACTUATION_JSON:-}"
[ -n "$NS" ] || { echo "actuation: set ACTUATION_NS or BENCHMARK_NAMESPACE" >&2; exit 2; }

kubectl get deploy "$DEPLOY" -n "$NS" >/dev/null 2>&1 || {
    echo "actuation: no deployment $DEPLOY in $NS" >&2; exit 1; }

# The pod selector for this Deployment, taken from its own spec rather than
# guessed: FMA requesters carry app=dp-app, not the deployment name, so a
# hand-written selector silently matches nothing and every trial reads as zero.
SEL=$(kubectl get deploy "$DEPLOY" -n "$NS" -o json 2>/dev/null | python3 -c '
import json, sys
m = json.load(sys.stdin)["spec"]["selector"].get("matchLabels") or {}
print(",".join("%s=%s" % kv for kv in sorted(m.items())))
')
[ -n "$SEL" ] || { echo "actuation: could not read pod selector" >&2; exit 1; }

wait_ready() { # $1=n
    for _ in $(seq 1 200); do
        r=$(kubectl get deploy "$DEPLOY" -n "$NS" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)
        [ "${r:-0}" = "$1" ] && return 0
        sleep 2
    done
    return 1
}

echo "actuation: $DEPLOY in $NS  selector=$SEL  base=$BASE step=$STEP trials=$TRIALS"

kubectl scale deploy "$DEPLOY" -n "$NS" --replicas="$BASE" >/dev/null
wait_ready "$BASE" || echo "  warning: could not settle at base $BASE"

SAMPLES=$(mktemp)
trap 'rm -f "$SAMPLES"' EXIT

for t in $(seq 1 "$TRIALS"); do
    before=$(kubectl get pods -n "$NS" -l "$SEL" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null)
    kubectl scale deploy "$DEPLOY" -n "$NS" --replicas=$((BASE + STEP)) >/dev/null
    if ! wait_ready $((BASE + STEP)); then
        echo "  trial $t: TIMEOUT -- capacity never arrived"
        kubectl scale deploy "$DEPLOY" -n "$NS" --replicas="$BASE" >/dev/null
        wait_ready "$BASE" || true
        continue
    fi

    kubectl get pods -n "$NS" -l "$SEL" -o json 2>/dev/null \
      | BEFORE="$before" TRIAL="$t" python3 -c '
import json, os, sys, datetime
T = lambda s: datetime.datetime.strptime(s, "%Y-%m-%dT%H:%M:%SZ")
before = set(os.environ.get("BEFORE", "").split())
trial = os.environ["TRIAL"]
for p in json.load(sys.stdin)["items"]:
    m = p["metadata"]
    if m["name"] in before:
        continue
    ready = None
    for c in p["status"].get("conditions", []):
        if c["type"] == "Ready" and c["status"] == "True" and c.get("lastTransitionTime"):
            ready = T(c["lastTransitionTime"])
    if not ready:
        continue
    d = (ready - T(m["creationTimestamp"])).total_seconds()
    dual = (m.get("labels") or {}).get("dual-pods.llm-d.ai/dual", "")
    print("%s\t%.0f\t%s\t%s\t%s" % (trial, d, m["name"][-5:],
                                    p["spec"].get("nodeName", "-"), dual[-6:] or "-"))
' >> "$SAMPLES"

    last=$(tail -1 "$SAMPLES" | cut -f2)
    printf "  trial %-2s %4ss  %s\n" "$t" "${last:-?}" \
        "$( [ "${last:-999}" -le 15 ] 2>/dev/null && echo WOKE || echo rebuilt )"

    kubectl scale deploy "$DEPLOY" -n "$NS" --replicas="$BASE" >/dev/null
    wait_ready "$BASE" || true
    sleep 10   # let the controller settle the unbound launcher into sleep
done

echo
python3 - "$SAMPLES" "${JSON}" <<'PY'
import sys, statistics
rows = []
for line in open(sys.argv[1]):
    parts = line.rstrip("\n").split("\t")
    if len(parts) >= 2:
        rows.append((parts[0], float(parts[1]), *parts[2:]))
if not rows:
    print("  no samples: every trial timed out"); raise SystemExit(1)
d = sorted(r[1] for r in rows)
warm = sum(1 for x in d if x <= 15)
pct = lambda p: d[min(len(d) - 1, int(round((p / 100.0) * (len(d) - 1))))]
print("  samples   %d" % len(d))
print("  min       %.0fs" % d[0])
print("  median    %.0fs" % statistics.median(d))
print("  p95       %.0fs" % pct(95))
print("  max       %.0fs" % d[-1])
print("  woken     %d / %d   (<=15s)" % (warm, len(d)))
print("  rebuilt   %d / %d" % (len(d) - warm, len(d)))
if len(sys.argv) > 2 and sys.argv[2]:
    import json
    json.dump({"samples": [{"trial": r[0], "seconds": r[1]} for r in rows]},
              open(sys.argv[2], "w"))
    print("  raw       %s" % sys.argv[2])
PY
