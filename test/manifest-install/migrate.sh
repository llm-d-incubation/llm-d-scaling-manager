#!/usr/bin/env bash
#
# A move from an smt install to the one manifest, and mutual TLS through it
# (proposal PR #404, appendix G): `smt install --scope cluster` from the tools
# image, a ScaledObject on the scaler over mutual TLS, then `kubectl apply
# --server-side` of scaling-manager.yaml over it with scaling-manager-admin.sh
# move, and watchNamespaces. The controller must copy the smt CA into its new
# Opaque Secrets,
# so KEDA keeps its connection with no restart of KEDA's operator.
# `make test-e2e-manifest-migrate` runs it on the release-files e2e
# prerequisites, on a cluster where neither install ran yet.
#
#   test/manifest-install/migrate.sh <dir with the rendered release files>
#
# Environment: TOOLS_IMG (the tools image built with the release files),
# MODEL_NS, KEDA_NS, MONITORING_NS, PROMETHEUS_URL, PROMETHEUS_TLS_SECRET as in
# e2e.sh. Exit 0 only when every step passed.

set -uo pipefail

DIST="${1:?usage: $0 <dir with the rendered release files>}"
NS=scaling-manager-system
MODEL_NS="${MODEL_NS:-llm-d-sim}"
KEDA_NS="${KEDA_NS:-keda-system}"
MONITORING_NS="${MONITORING_NS:-scaling-manager-monitoring}"
PROMETHEUS_URL="${PROMETHEUS_URL:-https://kube-prometheus-stack-prometheus.$MONITORING_NS.svc.cluster.local:9090}"
PROMETHEUS_TLS_SECRET="${PROMETHEUS_TLS_SECRET:-prometheus-web-tls}"
TOOLS_IMG="${TOOLS_IMG:?set TOOLS_IMG to the tools image built with the release files}"
M=scaling-manager.yaml
ADMIN="bash ./scaling-manager-admin.sh"
STATUS_CM=scaling-manager-controller-status
DEPLOY=scaling-manager-controller-manager
SO=migrate-probe
COPY="scaling-manager-external-scaler-$NS"

WORK="$(mktemp -d)"
LOGS="${E2E_LOG_DIR:-$WORK/logs}"
mkdir -p "$LOGS" "$WORK/work"
SUMMARY="$LOGS/summary.txt"
: > "$SUMMARY"

log() { printf '[%s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
pass() { printf 'PASS  %s\n' "$1" | tee -a "$SUMMARY"; }
fail() {
    printf 'FAIL  %s: %s\n' "$1" "$2" | tee -a "$SUMMARY"
    echo "=== diagnostics ==="
    kubectl get pods -n "$NS" 2>&1 | head
    kubectl get scaledobject "$SO" -n "$MODEL_NS" -o yaml 2>&1 | sed -n '/^status:/,$p' | head -30
    kubectl get configmap "$STATUS_CM" -n "$NS" -o yaml 2>&1 | grep -E '^\s+(scalerTLS|kedaC|ready|namespaces|scalerUnreachable)' | cut -c1-200
    kubectl logs -n "$NS" "deployment/$DEPLOY" --tail=60 2>&1 | tail -60
    echo "=== summary ==="; cat "$SUMMARY"
    exit 1
}
wait_for() {
    local t="$1" what="$2" i=0
    shift 2
    until "$@" >/dev/null 2>&1; do
        i=$((i + 3))
        [ "$i" -lt "$t" ] || { log "timed out after ${t}s waiting for $what"; return 1; }
        sleep 3
    done
    log "ok: $what (${i}s)"
}
status() { kubectl get configmap "$STATUS_CM" -n "$NS" -o "jsonpath={.data.${1//./\\.}}" 2>/dev/null; }
status_is() { [ "$(status "$1")" = "$2" ]; }
status_has_prefix() { case "$(status "$1")" in "$2"*) return 0 ;; esac; return 1; }
secret_sha() { kubectl get secret "$2" -n "$1" -o jsonpath='{.data.tls\.crt}' 2>/dev/null | sha256sum | cut -c1-16; }
# so_wired: the probe ScaledObject's HPA carries the scaler's external metric
# and KEDA reports the ScaledObject Ready, so KEDA reaches the scaler.
so_wired() {
    kubectl get hpa "keda-hpa-$SO" -n "$MODEL_NS" -o jsonpath='{.spec.metrics[*].external.metric.name}' 2>/dev/null | grep -q . \
        && [ "$(kubectl get scaledobject "$SO" -n "$MODEL_NS" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" = True ]
}
smt() {
    local name="$1"
    shift
    log "smt $*"
    docker run --rm --network host --user "$(id -u):$(id -g)" \
        -v "$WORK/admin.kubeconfig:/kube/config:ro" -v "$WORK/work:/work" "$TOOLS_IMG" "$@" > "$LOGS/$name.log" 2>&1
}

kubectl get namespace "$NS" >/dev/null 2>&1 && fail setup "$NS exists: run on a cluster where no scaling manager is installed"
# The flattened admin credentials never outlive the run, on any exit path.
trap 'rm -f "$WORK/admin.kubeconfig"' EXIT
kubectl config view --minify --flatten > "$WORK/admin.kubeconfig" && chmod 0600 "$WORK/admin.kubeconfig"
kubectl get secret "$PROMETHEUS_TLS_SECRET" -n "$MONITORING_NS" -o jsonpath='{.data.tls\.crt}' | base64 -d > "$WORK/work/prometheus-ca.crt"
chmod 0644 "$WORK/work/prometheus-ca.crt"

# --- 1. the smt install, and a ScaledObject over mutual TLS -----------------

smt install install --scope cluster --keda-namespace "$KEDA_NS" --model-namespace "$MODEL_NS" \
    --prometheus-url "$PROMETHEUS_URL" --prometheus-ca prometheus-ca.crt \
    || { rc=$?; tail -30 "$LOGS/install.log"; fail "smt install" "exit $rc"; }
status_has_prefix scalerTLS "mutual TLS" || fail "smt install" "scalerTLS is '$(status scalerTLS)'"
printf 'apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: %s\n  namespace: %s\nspec:\n  replicas: 1\n  selector:\n    matchLabels: {app: %s}\n  template:\n    metadata:\n      labels: {app: %s}\n    spec:\n      containers:\n      - name: pause\n        image: registry.k8s.io/pause:3.10\n---\napiVersion: keda.sh/v1alpha1\nkind: ScaledObject\nmetadata:\n  name: %s\n  namespace: %s\nspec:\n  scaleTargetRef: {name: %s}\n  minReplicaCount: 1\n  maxReplicaCount: 2\n  pollingInterval: 5\n  triggers:\n  - type: external\n    authenticationRef: {kind: ClusterTriggerAuthentication, name: %s}\n    metadata:\n      scalerAddress: scaling-manager-external-scaler.%s.svc:9090\n' \
    "$SO" "$MODEL_NS" "$SO" "$SO" "$SO" "$MODEL_NS" "$SO" "$COPY" "$NS" > "$WORK/probe.yaml"
kubectl apply -f "$WORK/probe.yaml" > "$LOGS/probe.log" 2>&1 || { cat "$LOGS/probe.log"; fail probe "applying the probe ScaledObject"; }
wait_for 240 "the probe ScaledObject wired over mutual TLS" so_wired || fail probe "KEDA does not reach the smt install's scaler"
ca_smt="$(secret_sha "$NS" scaling-manager-external-scaler-ca)"
copy_smt="$(secret_sha "$KEDA_NS" "$COPY")"
keda_pod="$(kubectl get pods -n "$KEDA_NS" -l app.kubernetes.io/name=keda-operator -o jsonpath='{.items[0].metadata.uid}')"
pass "smt install --scope cluster: mutual TLS; the probe ScaledObject is Ready with the scaler's metric (CA $ca_smt)"

# --- 2. the move: the manifest over it, KEDA's side, watchNamespaces ---------

for f in "$M" scaling-manager-keda.yaml scaling-manager-admin.sh; do cp "$DIST/$f" "$WORK/$f" || fail move "no $f in $DIST"; done
cd "$WORK" || exit 1
# move: --force-conflicts takes the controller Deployment over from smt, whose
# client-side apply owns fields the manifest also sets. Only the move needs
# it; the upgrade after it does not. Nothing is edited for KEDA in $KEDA_NS.
$ADMIN move --keda-namespace "$KEDA_NS" > "$LOGS/apply.log" 2>&1 || { cat "$LOGS/apply.log"; fail move "scaling-manager-admin.sh move over the smt install"; }
$ADMIN upgrade --keda-namespace "$KEDA_NS" > "$LOGS/apply-again.log" 2>&1 || { cat "$LOGS/apply-again.log"; fail move "scaling-manager-admin.sh upgrade, without --force-conflicts, after the move"; }
$ADMIN namespaces "$MODEL_NS" > /dev/null || fail move "scaling-manager-admin.sh namespaces $MODEL_NS"
kubectl rollout status "deployment/$DEPLOY" -n "$NS" --timeout=300s > "$LOGS/rollout.log" 2>&1 || fail move "the controller did not roll out"
pass "the move: scaling-manager-admin.sh move over the smt install ($(grep -c . "$LOGS/apply.log") lines), then upgrade, and watchNamespaces $MODEL_NS"

# --- 3. the CA moved, and KEDA stays connected -------------------------------

has_ca() { [ "$(secret_sha "$NS" scaling-manager-scaler-ca)" = "$ca_smt" ]; }
wait_for 180 "the new CA Secret holds the smt CA" has_ca || fail "CA" "scaling-manager-scaler-ca holds $(secret_sha "$NS" scaling-manager-scaler-ca), the smt CA is $ca_smt"
# To a file first: with pipefail, grep -q ending the pipe early kills
# kubectl logs with SIGPIPE and fails the check although the line is there.
# Every controller pod: during the rollout an old and a new one both run, and
# the copy is made by whichever pass comes first.
kubectl logs -n "$NS" -l control-plane=controller-manager --tail=-1 --prefix > "$LOGS/controller.log" 2>&1
kubectl logs -n "$NS" -l control-plane=controller-manager --tail=-1 --prefix --previous >> "$LOGS/controller.log" 2>&1
log "controller restarts after the move: $(kubectl get pods -n "$NS" -l control-plane=controller-manager -o jsonpath='{range .items[*]}{.metadata.name}={.status.containerStatuses[0].restartCount} {end}')"
grep -q "copied the certificates of the earlier install" "$LOGS/controller.log" \
    || fail "CA" "the controller did not say it copied the earlier install's certificates"
new_tls() { case "$(status scalerTLS)" in "mutual TLS: Secrets scaling-manager-scaler-ca,"*) return 0 ;; esac; return 1; }
wait_for 180 "scalerTLS mutual TLS from the new Secrets" new_tls || fail "CA" "scalerTLS is '$(status scalerTLS)'"
status_is kedaCopyStale false || fail "CA" "kedaCopyStale is '$(status kedaCopyStale)'"
[ "$(secret_sha "$KEDA_NS" "$COPY")" = "$copy_smt" ] || fail "CA" "KEDA's copy changed; KEDA would need a restart"
wait_for 180 "namespacesManaged=$MODEL_NS" status_is namespacesManaged "$MODEL_NS" || fail "CA" "namespacesManaged '$(status namespacesManaged)'"
pass "the controller copied the smt CA into its Opaque Secrets; KEDA's copy unchanged; mutual TLS"

wait_for 180 "kedaConnection current" status_is kedaConnection current || fail "KEDA" "kedaConnection is '$(status kedaConnection)'"
so_wired || fail "KEDA" "the probe ScaledObject is no longer Ready"
[ "$(kubectl get pods -n "$KEDA_NS" -l app.kubernetes.io/name=keda-operator -o jsonpath='{.items[0].metadata.uid}')" = "$keda_pod" ] \
    || fail "KEDA" "KEDA's operator Pod changed; the move must not need its restart"
[ -z "$(status scalerUnreachable)" ] || fail "KEDA" "scalerUnreachable: $(status scalerUnreachable)"
pass "KEDA presents its certificate to the moved controller (kedaConnection current), the probe stays Ready, KEDA's operator not restarted"

# --- 4. clean up --------------------------------------------------------------

{
    kubectl delete -f "$WORK/probe.yaml" --ignore-not-found
    kubectl delete -n "$KEDA_NS" -f scaling-manager-keda.yaml --ignore-not-found
    kubectl label namespace "$KEDA_NS" scaling-manager.llmd.ai/keda-
    kubectl delete -f "$M" --ignore-not-found --wait=true --timeout=300s
    kubectl delete clusterrole,clusterrolebinding,clustertriggerauthentication -l app.kubernetes.io/part-of=scaling-manager --ignore-not-found
    kubectl delete rolebinding,role,podmonitor,servicemonitor,secret,configmap -n "$MODEL_NS" -l app.kubernetes.io/part-of=scaling-manager --ignore-not-found
} > "$LOGS/cleanup.log" 2>&1 || fail cleanup "$(tail -5 "$LOGS/cleanup.log")"
kubectl get namespace "$NS" >/dev/null 2>&1 && fail cleanup "namespace $NS is still there"
pass "cleaned up"
echo "=== summary ==="
cat "$SUMMARY"
