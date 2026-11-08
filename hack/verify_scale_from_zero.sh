#!/usr/bin/env bash
# Verify scale-from-zero end to end against a real cluster.
#
# WHY THIS EXISTS RATHER THAN AN E2E SPEC
#
# The Ginkgo scale-from-zero specs only run on the kind emulator, and there EPP
# never engages flow control -- measured across 152 samples of a full spec run,
# no llm_d_epp_flow_control_queue_size series ever appeared. So the specs skip,
# and the only environment where the feature works is the one with no automated
# coverage. That is how a week of failures went unattributed.
#
# It asserts the whole chain, so a failure says WHICH link broke:
#
#   requests queue at zero endpoints   (EPP is engaging flow control)
#     -> The scaling manager publishes an activation   (the engine consumed the signal)
#       -> the workload leaves zero    (KEDA acted on it)
#
# A wake for some other reason -- a floor, a manual scale, another controller --
# is not a pass, so the queue and the activation are checked too.
#
# THE PRECONDITION THAT KEPT BITING
#
# KEDA derives the HPA from the ScaledObject, and an HPA created while
# minReplicaCount was 1 keeps minReplicas: 1 -- so the workload cannot reach zero
# no matter what the ScaledObject says afterwards. Four earlier attempts failed
# here and were misread as "the scaling manager is holding the model active". So the HPA's own
# minReplicas is asserted before anything else runs, and the script says plainly
# when the precondition is unmet instead of blaming the autoscaler.
#
# Usage: verify_scale_from_zero.sh <namespace> <deployment> <modelID> [<scaler-namespace>]
# Env:   SFZ_MAX_REPLICAS (default 3), SFZ_LOAD (default 40), SFZ_WAKE_TIMEOUT (default 120),
#        SFZ_LLMSM_NAMESPACE (same as the 4th argument)
#
# <scaler-namespace> is where the scaling manager controller and its scaling-manager-external-scaler
# Service run. It is the model namespace only when the controller was installed
# beside its workloads; installed out of reach, in a namespace of its own, the
# model namespace has no scaler, and a ScaledObject naming it gets nothing from
# KEDA but a CPU fallback. Left unset, it is read from a scaling manager ScaledObject already
# in <namespace>, else taken to be <namespace>.
set -u

NS="${1:?usage: verify_scale_from_zero.sh <namespace> <deployment> <modelID> [<scaler-namespace>]}"
DEPLOY="${2:?deployment required}"
MODEL="${3:?modelID required}"
CTL_NS="${4:-${SFZ_LLMSM_NAMESPACE:-}}"
MAXR="${SFZ_MAX_REPLICAS:-3}"
LOAD="${SFZ_LOAD:-40}"
WAKE_TIMEOUT="${SFZ_WAKE_TIMEOUT:-120}"
SO="${DEPLOY}-sfz-verify"

fail() { echo; echo "FAIL: $*" >&2; exit 1; }
skip() { echo; echo "SKIP: $*"; exit 0; }

kubectl get deploy "$DEPLOY" -n "$NS" >/dev/null 2>&1 || fail "no deployment $DEPLOY in $NS"

if [ -z "$CTL_NS" ]; then
    addr=$(kubectl get scaledobject -n "$NS" \
        -o jsonpath='{range .items[*]}{range .spec.triggers[*]}{.metadata.scalerAddress}{"\n"}{end}{end}' 2>/dev/null \
        | grep '^scaling-manager-external-scaler\.' | head -1)
    CTL_NS=$(printf '%s' "$addr" | cut -d: -f1 | cut -d. -f2)
    [ -n "$CTL_NS" ] || CTL_NS="$NS"
fi
# Forbidden is not absent: a tenant may not read the controller's namespace.
if ! svc_err=$(kubectl get svc scaling-manager-external-scaler -n "$CTL_NS" -o name 2>&1 >/dev/null); then
    case "$svc_err" in
        *NotFound*|*"not found"*)
            fail "no scaling-manager-external-scaler Service in $CTL_NS, so the ScaledObject this writes would name a scaler nothing answers. Pass the controller's namespace as the 4th argument (or SFZ_LLMSM_NAMESPACE)." ;;
        *)
            echo "  cannot verify the scaler in $CTL_NS ($(printf '%s' "$svc_err" | head -1)); using it as given" ;;
    esac
fi
echo "  scaler: scaling-manager-external-scaler.${CTL_NS}"

# A model is at zero only when EVERY variant serving it is. Park one and leave a
# sibling running and the InferencePool still has endpoints, so EPP routes rather
# than queues and no wake is ever needed -- queue stays flat at 0 and the test
# reports "flow control not engaging", which is wrong and misleading.
#
# This bites hardest with FMA: an FMA requester binds a launcher, and a BOUND
# launcher carries llm-d.ai/inferenceServing=true, so it joins the pool as a real
# endpoint. Parking the decode variant alone leaves the launcher serving.
#
# Siblings are found the way scaling manager groups them: every ScaledObject whose trigger
# metadata names the same modelID.
SIBLINGS=$(kubectl get scaledobject -n "$NS" -o json 2>/dev/null | MODEL="$MODEL" python3 -c '
import json, os, sys
want = os.environ["MODEL"]
out = []
for so in json.load(sys.stdin).get("items", []):
    for t in so["spec"].get("triggers") or []:
        if (t.get("metadata") or {}).get("modelID") == want:
            n = (so["spec"].get("scaleTargetRef") or {}).get("name")
            if n and n not in out:
                out.append(n)
            break
print(" ".join(out))')
[ -n "$SIBLINGS" ] || SIBLINGS="$DEPLOY"
case " $SIBLINGS " in *" $DEPLOY "*) ;; *) SIBLINGS="$SIBLINGS $DEPLOY" ;; esac
echo "  variants of $MODEL to park: $SIBLINGS"

SVC=$(kubectl get svc -n "$NS" -o json 2>/dev/null | python3 -c '
import json, sys
for s in json.load(sys.stdin)["items"]:
    if "router-epp" in s["metadata"]["name"] or "inference-gateway" in s["metadata"]["name"]:
        ports = s["spec"]["ports"]
        p = [x for x in ports if x.get("name") == "http"] or ports
        print("%s:%s" % (s["spec"]["clusterIP"], p[0]["port"])); break')
EPPIP=$(kubectl get pods -n "$NS" -o json 2>/dev/null | python3 -c '
import json, sys
for p in json.load(sys.stdin)["items"]:
    if "epp" in p["metadata"]["name"] and p["status"].get("podIP"):
        print(p["status"]["podIP"]); break')
[ -n "$SVC" ] && [ -n "$EPPIP" ] || fail "could not resolve the router service or an EPP pod in $NS"
echo "  router=$SVC epp=$EPPIP model=$MODEL"

# Everything that could hold this workload above zero, saved verbatim for restore.
SAVED=$(mktemp); trap 'rm -f "$SAVED"' EXIT
kubectl get scaledobject -n "$NS" -o json 2>/dev/null | SIBLINGS="$SIBLINGS" SAVED="$SAVED" python3 -c '
import json, os, sys
targets = set(os.environ["SIBLINGS"].split())
out = {"apiVersion": "v1", "kind": "List", "items": []}
for so in json.load(sys.stdin).get("items", []):
    if (so["spec"].get("scaleTargetRef") or {}).get("name") not in targets:
        continue
    so.pop("status", None)
    for f in ("creationTimestamp", "resourceVersion", "uid", "generation", "managedFields", "selfLink"):
        so.get("metadata", {}).pop(f, None)
    out["items"].append(so)
json.dump(out, open(os.environ["SAVED"], "w"))
print(len(out["items"]))
' > /tmp/.sfz_nsaved 2>/dev/null
NSAVED=$(cat /tmp/.sfz_nsaved 2>/dev/null || echo 0)
ORIG=$(kubectl get deploy "$DEPLOY" -n "$NS" -o jsonpath='{.spec.replicas}' 2>/dev/null)
echo "  saved $NSAVED existing ScaledObject(s); original replicas=$ORIG"

restore() {
    kubectl delete scaledobject "$SO" -n "$NS" --ignore-not-found >/dev/null 2>&1
    kubectl delete pod -n "$NS" -l sfz-verify=load --wait=false >/dev/null 2>&1
    [ "${NSAVED:-0}" -gt 0 ] && kubectl apply -f "$SAVED" >/dev/null 2>&1
    for d in ${SIBLINGS:-$DEPLOY}; do kubectl scale deploy "$d" -n "$NS" --replicas=1 >/dev/null 2>&1; done
    rm -f "$SAVED" /tmp/.sfz_nsaved
    echo "  restored"
}
trap restore EXIT

echo "=== 1. detach every scaler, so KEDA deletes the HPA that carries the floor ==="
kubectl get scaledobject -n "$NS" -o json 2>/dev/null | SIBLINGS="$SIBLINGS" python3 -c '
import json, os, sys
targets = set(os.environ["SIBLINGS"].split())
for so in json.load(sys.stdin).get("items", []):
    if (so["spec"].get("scaleTargetRef") or {}).get("name") in targets:
        print(so["metadata"]["name"])
' | while read -r n; do kubectl delete scaledobject "$n" -n "$NS" --ignore-not-found >/dev/null 2>&1; done
for _ in $(seq 1 24); do
    kubectl get hpa -n "$NS" -o json 2>/dev/null | grep -q "\"name\": *\"$DEPLOY\"" || break
    sleep 5
done

echo "=== 2. park EVERY variant while nothing can scale them back ==="
for d in $SIBLINGS; do kubectl scale deploy "$d" -n "$NS" --replicas=0 >/dev/null 2>&1; done
for _ in $(seq 1 24); do
    tot=0
    for d in $SIBLINGS; do
        n=$(kubectl get deploy "$d" -n "$NS" -o jsonpath='{.status.replicas}' 2>/dev/null || echo 0)
        tot=$((tot + ${n:-0}))
    done
    [ "$tot" = "0" ] && break
    sleep 5
done
for d in $SIBLINGS; do
    n=$(kubectl get deploy "$d" -n "$NS" -o jsonpath='{.status.replicas}' 2>/dev/null || echo 0)
    [ "${n:-0}" = "0" ] || fail "$d stuck at $n with no scaler attached"
done
# The pool must be genuinely empty. A bound FMA launcher is an endpoint even
# though it is not one of the Deployments above: binding stamps
# llm-d.ai/inferenceServing=true onto the launcher pod, so it joins the pool and
# EPP routes to it instead of queueing.
# Draining is not instant: when a requester goes away the dual-pods controller
# has to unbind its launcher and clear llm-d.ai/inferenceServing before the pod
# leaves the pool. Checking immediately reads the pre-unbind state and reports a
# phantom endpoint.
echo "  all variants parked; waiting for the pool to drain..."
for _ in $(seq 1 24); do
    eps=$(kubectl get pods -n "$NS" -l "llm-d.ai/inferenceServing=true" --no-headers 2>/dev/null | grep -c .)
    [ "${eps:-0}" = "0" ] && break
    sleep 5
done
echo "  pool endpoints remaining: ${eps:-0}"
[ "${eps:-0}" = "0" ] || skip "the pool still has ${eps} endpoint(s) after parking every variant -- a bound FMA launcher carries llm-d.ai/inferenceServing=true and serves, so requests are routed rather than queued and no wake is needed"

echo "=== 3. attach a zero-capable scaler, and PROVE the HPA is zero-capable ==="
# The scaler's address and KEDA's authenticationRef follow its TLS mode
# (deploy/lib/scaler_tls.sh, "KEDA's side").
SCALER_ADDR="scaling-manager-external-scaler.${CTL_NS}.svc.cluster.local:9090"
SCALER_AUTH=""
if kubectl get deployment scaling-manager-controller-manager -n "$CTL_NS" -o jsonpath='{.spec.template.metadata.annotations.scaling-manager\.llmd\.ai/scaler-tls}' 2>/dev/null | grep -q .; then
    SCALER_ADDR="scaling-manager-external-scaler.${CTL_NS}.svc:9090"
    SCALER_AUTH="      authenticationRef: {name: scaling-manager-external-scaler-${CTL_NS}, kind: ClusterTriggerAuthentication}"
fi
kubectl apply -f - >/dev/null 2>&1 <<YAML
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata: {name: ${SO}, namespace: ${NS}}
spec:
  scaleTargetRef: {apiVersion: apps/v1, kind: Deployment, name: ${DEPLOY}}
  pollingInterval: 5
  minReplicaCount: 0
  maxReplicaCount: ${MAXR}
  advanced: {restoreToOriginalReplicaCount: false}
  triggers:
    - type: external-push
      name: scaling-manager-external-scaler
      metadata:
        scalerAddress: "${SCALER_ADDR}"
        modelID: "${MODEL}"
${SCALER_AUTH}
YAML
# NOT an HPA check. KEDA's HPA always carries minReplicas: 1 -- KEDA does 0<->1
# itself by scaling the Deployment and uses the HPA only for 1..max -- so
# asserting minReplicas==0 there can never pass and proves nothing.
#
# The real precondition is that the SCALER reports inactive. The scaling manager reports active
# whenever its decision for the model is >= 1, and it will keep deciding 1 from
# historical capacity ("reason":"P2-hist") for a model that has served recently.
# That is the autoscaler working correctly, not a fault -- but it means this test
# needs a genuinely idle model and must say so rather than fail.
for _ in $(seq 1 18); do
    [ "$(kubectl get deploy "$DEPLOY" -n "$NS" -o jsonpath='{.status.replicas}' 2>/dev/null || echo 0)" = "0" ] || break
    sleep 5
done
r=$(kubectl get deploy "$DEPLOY" -n "$NS" -o jsonpath='{.status.replicas}' 2>/dev/null || echo 0)
if [ "${r:-0}" != "0" ]; then
    tgt=$(kubectl logs deploy/scaling-manager-controller-manager -n "$CTL_NS" --tail=300 2>/dev/null \
          | grep -oE "\"name\":\"${SO%-sfz-verify}-llmsm\",\"curr\":[0-9]+,\"tgt\":[0-9]+[^,]*" | tail -1)
    skip "the scaler took it to $r before any load arrived: The scaling manager still decides this model needs replicas${tgt:+ ($tgt)}, so it is not idle and there is nothing to wake.
      Run this against a model that has been quiet long enough for the scaling manager to decide 0 -- a model serving recently keeps a floor from historical capacity."
fi
echo "  still at zero under a zero-capable scaler"

SINCE=$(date -u +%Y-%m-%dT%H:%M:%SZ)
echo "=== 4. drive $LOAD concurrent requests at the parked model ==="
kubectl run sfz-verify-load -n "$NS" --restart=Never --quiet --labels=sfz-verify=load \
  --image=registry.k8s.io/e2e-test-images/agnhost:2.47 --command -- \
  /bin/sh -c "for i in \$(seq 1 ${LOAD}); do (curl -s -o /dev/null --max-time 150 -X POST http://${SVC}/v1/completions -H 'Content-Type: application/json' -d '{\"model\":\"${MODEL}\",\"prompt\":\"hi\",\"max_tokens\":8}' &); done; sleep 160" >/dev/null 2>&1 &

TOK=$(kubectl create token default -n "$NS" --duration=20m 2>/dev/null)
queued=0; woke=0
for i in $(seq 1 $((WAKE_TIMEOUT / 10))); do
    sleep 10
    qs=$(kubectl run "sfzq${i}" -n "$NS" --rm -i --restart=Never --quiet \
          --image=registry.k8s.io/e2e-test-images/agnhost:2.47 --command -- \
          /bin/sh -c "curl -s --max-time 6 -H 'Authorization: Bearer ${TOK}' http://${EPPIP}:9090/metrics 2>/dev/null | grep '^llm_d_epp_flow_control_queue_size{' | sed 's/.*} //'" 2>/dev/null | tr -dc '0-9.')
    rep=$(kubectl get deploy "$DEPLOY" -n "$NS" -o jsonpath='{.status.replicas}' 2>/dev/null)
    printf "  t+%-3ss queue=%-6s replicas=%s\n" "$((i * 10))" "${qs:-?}" "${rep:-0}"
    case "${qs:-0}" in ''|0|0.0) ;; *) queued=1 ;; esac
    [ "${rep:-0}" -gt 0 ] 2>/dev/null && { woke=1; break; }
done

act=$(kubectl logs deploy/scaling-manager-controller-manager -n "$CTL_NS" --since-time="$SINCE" 2>/dev/null \
      | grep -c "Published scale-from-zero activation")

echo
echo "=== result ==="
printf "  requests queued at zero endpoints : %s\n" "$([ "$queued" = 1 ] && echo yes || echo NO)"
printf "  activation published by the scaling manager       : %s\n" "$([ "${act:-0}" -gt 0 ] && echo yes || echo NO)"
printf "  workload left zero                : %s\n" "$([ "$woke" = 1 ] && echo yes || echo NO)"

[ "$queued" = 1 ] || fail "EPP never queued: no wake signal exists here (flow control not engaging)"
[ "${act:-0}" -gt 0 ] || fail "the queue filled but the scaling manager published no activation: the engine did not consume the signal"
[ "$woke" = 1 ] || fail "activation published but the workload stayed at zero: KEDA did not act"
echo "PASS: scale-from-zero works end to end"
