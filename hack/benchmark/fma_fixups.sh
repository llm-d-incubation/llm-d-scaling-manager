#!/usr/bin/env bash
# Post-standup fixes for the FMA install in a benchmark namespace.
#
# The FMA standup comes from llm-d-benchmark, which is gitignored and re-checked
# out to a pinned tag, so anything corrected by hand is gone the next time
# someone stands the environment up. These two corrections are worth keeping,
# because without them FMA benchmarking does not merely look bad -- it produces
# no data at all, in a way that reads as a fault somewhere else entirely.
#
# FIX 1 -- let the launcher reflect its own state.
#
# Each launcher pod runs a `state-change-reflector` sidecar that patches labels
# onto its own pod. The standup gives it the namespace `default` ServiceAccount,
# which cannot patch pods, so it fails 403 and retries every 5s forever.
#
# The consequence is not cosmetic. When a requester is deleted, the launcher it
# was bound to keeps `llm-d.ai/inferenceServing=true` and its stale
# `dual-pods.llm-d.ai/dual` label, so it stays in the InferencePool advertising
# an endpoint that cannot serve. EPP dispatches to it and ~20% of requests come
# back 503. guidellm validates its backend once before generating any load, so a
# single unlucky probe kills every worker:
#
#   httpx.HTTPStatusError: 503 Service Unavailable for '.../health'
#   RuntimeError: Worker process group startup failed: error_event is set
#
# The run then writes no results.json and every metric reports "?".
#
# FIX 2 -- run controllers new enough to reconcile the unbind.
#
# The standup pins v0.6.0-alpha.13. In that build, dual-pods drops a fresh
# notification for an item already queued with a future processAfter (a prior
# rate-limited retry) -- upstream #696, "make nodeData.Add pull forward delayed
# queue items". Fix 1's 403s generate exactly those retries, so the unbind
# notification lands in the bin and the stale label survives. The two defects
# compound: one manufactures the retries, the other swallows the correction.
#
# Both changes are namespaced. The FMA controllers run with
# `--namespace=<ns>` and a namespaced Role, so nothing here reaches another
# tenant. The CRDs are the only cluster-scoped surface and are NOT touched --
# they are already at the 0.6.4 shape on our cluster anyway.
#
# Idempotent. Safe to run after every standup, and that is the intent.
#
# Usage: fma_fixups.sh <namespace> [fma-version]
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


NS="${1:?usage: fma_fixups.sh <namespace> [fma-version]}"
VERSION="${2:-${FMA_VERSION:-v0.6.4}}"
REG="${FMA_REG:-ghcr.io/llm-d-incubation/llm-d-fast-model-actuation}"

# The images this script pins each deployment to. Overridable because a build of
# a fork lives somewhere else entirely -- ours is
# ghcr.io/ev-shindin/dual-pods-controller, which shares no path prefix with the
# upstream registry, so FMA_REG alone cannot express it.
#
# Without this the trap is silent and expensive: benchmark-standup re-runs these
# fixups, so a standup in the middle of an experiment resets the controller to
# the stock version and the run measures unmodified behaviour while every log
# still says the fork is being tested. Set the image, not just the version:
#
#   FMA_CONTROLLER_IMAGE=ghcr.io/ev-shindin/dual-pods-controller:aa072ef \
#     make benchmark-fma-fixups BENCHMARK_NAMESPACE=<ns>
#
# The populator is pinned separately and stays upstream unless it too is forked.
CONTROLLER_IMAGE="${FMA_CONTROLLER_IMAGE:-${REG}/dual-pods-controller:${VERSION}}"
POPULATOR_IMAGE="${FMA_POPULATOR_IMAGE:-${REG}/launcher-populator:${VERSION}}"

kubectl get ns "$NS" >/dev/null 2>&1 || { echo "fma_fixups: no namespace $NS" >&2; exit 1; }

echo "=== fix 1: allow the launcher reflector to patch its own pod ==="
kubectl apply -f - >/dev/null <<YAML && echo "  Role/RoleBinding applied"
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: fma-launcher-state-reflector
  namespace: ${NS}
rules:
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list", "watch", "patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: fma-launcher-state-reflector
  namespace: ${NS}
subjects:
  - kind: ServiceAccount
    name: default
    namespace: ${NS}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: fma-launcher-state-reflector
YAML

if [ "$(kubectl auth can-i patch pods --as="system:serviceaccount:${NS}:default" -n "$NS" 2>/dev/null)" = "yes" ]; then
    echo "  verified: the launcher SA can patch pods"
else
    echo "  WARNING: the launcher SA still cannot patch pods -- stale bindings will persist" >&2
fi

echo
echo "=== fix 2: FMA controllers ==="
echo "  controller: $CONTROLLER_IMAGE"
echo "  populator:  $POPULATOR_IMAGE"
# Container names differ per deployment (dual-pods uses "controller", the
# populator uses "launcher-populator"), so read the name rather than assume it --
# `kubectl set image` fails with "unable to find container named ..." otherwise,
# and it fails per-deployment, so half the upgrade silently does not happen.
upgrade() { # $1=deployment-name-substring  $2=full image ref to pin to
    local dep
    dep=$(kubectl get deploy -n "$NS" -o name 2>/dev/null | grep -- "$1" | head -1 | cut -d/ -f2)
    [ -n "$dep" ] || { echo "  no deployment matching '$1' -- FMA may not be stood up here"; return 0; }
    local c
    c=$(kubectl get deploy "$dep" -n "$NS" -o jsonpath='{.spec.template.spec.containers[0].name}' 2>/dev/null)
    local cur
    cur=$(kubectl get deploy "$dep" -n "$NS" -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null)
    if [ "$cur" = "$2" ]; then
        echo "  $dep already at $2"
        return 0
    fi
    kubectl set image "deploy/$dep" "$c=$2" -n "$NS" >/dev/null \
        && echo "  $dep: $cur -> $2" \
        || { echo "  FAILED to upgrade $dep" >&2; return 1; }
    kubectl rollout status "deploy/$dep" -n "$NS" --timeout=180s >/dev/null 2>&1 \
        && echo "    rolled out" || echo "    WARNING: rollout did not complete" >&2
}
upgrade dual-pods-controller "$CONTROLLER_IMAGE"
upgrade launcher-populator   "$POPULATOR_IMAGE"

echo
echo "=== launcher reflectors ==="
# A launcher started before the RoleBinding existed holds a client that is still
# being refused, and it will keep being refused for the pod's whole life. Restart
# only those: on a re-run nothing is denied, and deleting healthy launchers would
# throw away exactly the warm sleeping instances the benchmark depends on.
denied=0
for p in $(kubectl get pods -n "$NS" -l app.kubernetes.io/component=launcher \
             -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
    if kubectl logs "$p" -n "$NS" -c state-change-reflector --tail=40 2>/dev/null | grep -q "403"; then
        denied=$((denied + 1))
    fi
done
if [ "$denied" -gt 0 ]; then
    echo "  $denied launcher(s) still being refused; restarting them"
    kubectl delete pods -n "$NS" -l app.kubernetes.io/component=launcher --wait=false >/dev/null 2>&1 \
        && echo "  deleted; the populator recreates them with the permission in place"
else
    echo "  none refused -- leaving them alone (a restart would discard warm instances)"
fi

echo "done"
