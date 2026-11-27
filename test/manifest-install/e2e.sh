#!/usr/bin/env bash
#
# Installs the scaling manager from the one release manifest,
# scaling-manager.yaml, with the two scripts of docs/guides/kubectl-install
# (scaling-manager-admin.sh, scaling-manager-owner.sh; proposal PR #404,
# phase 1), and checks each step.
# `make test-e2e-manifest-install` runs it on a kind cluster whose
# prerequisites (Prometheus, KEDA, the llm-d CRDs, the model servers and the
# EPP) the existing e2e setup installed, without the scaling manager.
#
#   test/manifest-install/e2e.sh <dir with the rendered release files>
#
# It copies the manifest to a scratch directory and works there. The one
# exception is SMOKE_CMD, the existing smoke suite, run against this install.
#
# Environment:
#   MODEL_NS        the namespace with the model servers (default llm-d-sim)
#   KEDA_NS         where KEDA runs (default keda-system)
#   MONITORING_NS   where the e2e Prometheus runs (default scaling-manager-monitoring)
#   PROMETHEUS_URL  default the e2e Prometheus Service in MONITORING_NS
#   PROMETHEUS_TLS_SECRET  the Secret in MONITORING_NS whose tls.crt signs
#                   Prometheus's certificate (default prometheus-web-tls)
#   SMOKE_CMD       a command that runs the smoke suite against this install
#                   (with passive off); unset, that step does not run
#   KEEP_INSTALL    true: stop before the uninstall
#
# Exit 0 only when every step passed. The summary lists each step.

set -uo pipefail

DIST="${1:?usage: $0 <dir with the rendered release files>}"
NS=scaling-manager-system
MODEL_NS="${MODEL_NS:-llm-d-sim}"
KEDA_NS="${KEDA_NS:-keda-system}"
MONITORING_NS="${MONITORING_NS:-scaling-manager-monitoring}"
PROMETHEUS_URL="${PROMETHEUS_URL:-https://kube-prometheus-stack-prometheus.$MONITORING_NS.svc.cluster.local:9090}"
PROMETHEUS_TLS_SECRET="${PROMETHEUS_TLS_SECRET:-prometheus-web-tls}"
SMOKE_CMD="${SMOKE_CMD:-}"
KEEP_INSTALL="${KEEP_INSTALL:-false}"
REPO_DIR="$(pwd)"
# A namespace watchNamespaces leaves out, with a ScaledObject on the scaler:
# the negative control for the namespace selection.
OUTSIDE_NS=scaling-manager-e2e-outside
M=scaling-manager.yaml
ADMIN="bash ./scaling-manager-admin.sh"
OWNER="bash ./scaling-manager-owner.sh"
STATUS_CM=scaling-manager-controller-status
DEPLOY=scaling-manager-controller-manager
INSTANCE="app.kubernetes.io/instance=$NS"

WORK="$(mktemp -d)"
LOGS="${E2E_LOG_DIR:-$WORK/logs}"
mkdir -p "$LOGS"
SUMMARY="$LOGS/summary.txt"
: > "$SUMMARY"

log() { printf '[%s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
pass() { printf 'PASS  %s\n' "$1" | tee -a "$SUMMARY"; }
notrun() { printf 'NOT RUN  %s: %s\n' "$1" "$2" | tee -a "$SUMMARY"; }

fail() {
    printf 'FAIL  %s: %s\n' "$1" "$2" | tee -a "$SUMMARY"
    diagnostics
    echo "=== summary ==="; cat "$SUMMARY"
    echo "Logs and the files as applied: $LOGS, $WORK"
    exit 1
}

diagnostics() {
    echo "=== diagnostics ==="
    kubectl get pods -n "$NS" -o wide 2>&1 | head -10
    kubectl get configmap "$STATUS_CM" -n "$NS" -o yaml 2>&1 | grep -v -E 'BEGIN CERT|END CERT|^\s+[A-Za-z0-9+/=]{40,}$' | head -100
    kubectl logs -n "$NS" "deployment/$DEPLOY" --tail=100 2>&1 | tail -100
    kubectl logs -n "$NS" "deployment/$DEPLOY" --previous --tail=30 2>&1 | tail -30
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

# status <key>: one key of the status ConfigMap; dots in a key (preflight.<check>)
# are escaped, or jsonpath reads them as nested fields.
status() { kubectl get configmap "$STATUS_CM" -n "$NS" -o "jsonpath={.data.${1//./\\.}}" 2>/dev/null; }
status_is() { [ "$(status "$1")" = "$2" ]; }
status_has_prefix() { case "$(status "$1")" in "$2"*) return 0 ;; esac; return 1; }
status_contains() { case "$(status "$1")" in *"$2"*) return 0 ;; esac; return 1; }
settings_patch() { kubectl patch configmap scaling-manager-settings -n "$NS" --type merge -p "$1" >/dev/null; }
secret_sha() { kubectl get secret "$2" -n "$1" -o jsonpath='{.data.tls\.crt}' 2>/dev/null | sha256sum | cut -c1-16; }
rollout() { kubectl rollout status "deployment/$DEPLOY" -n "$NS" --timeout=300s; }
ready_ok() { status_is ready ok; }

# --- 0. the file, as an operator downloads it --------------------------------

FILES="$M scaling-manager-keda.yaml scaling-manager-grants.yaml scaling-manager-monitoring.yaml scaling-manager-admin.sh scaling-manager-owner.sh"
for f in $FILES SHA256SUMS; do cp "$DIST/$f" "$WORK"/ || fail files "no $f in $DIST"; done
cd "$WORK" || exit 1
sha256sum -c --ignore-missing --quiet SHA256SUMS > "$LOGS/sums.log" 2>&1 || fail files "a file does not match SHA256SUMS: $(cat "$LOGS/sums.log")"
for f in $FILES; do grep -q "  $f\$" SHA256SUMS || fail files "SHA256SUMS does not list $f"; done
pass "files: $FILES match SHA256SUMS (signature: checked on a release with cosign verify-blob, not here)"
log "Kubernetes: $(kubectl version -o json 2>/dev/null | grep -m1 gitVersion | tr -d ' ",')"
kubectl get namespace "$MODEL_NS" >/dev/null 2>&1 || fail namespace "no model namespace $MODEL_NS (the e2e setup makes it)"
# Nothing is edited: KEDA runs in $KEDA_NS, not keda, and the files stay as
# downloaded (their sums are checked again at the end).

# --- 1. the manifest and KEDA's side, before any setting ---------------------

$ADMIN upgrade --keda-namespace "$KEDA_NS" > "$LOGS/apply-1.log" 2>&1 || { cat "$LOGS/apply-1.log"; fail "apply" "scaling-manager-admin.sh upgrade"; }
pass "scaling-manager-admin.sh upgrade: the manifest, scaling-manager-keda.yaml in $KEDA_NS, its label ($(grep -c . "$LOGS/apply-1.log") lines)"

# Before the settings exist the controller waits for them, live, and says
# why in its status, instead of exiting into the kubelet's crash back-off.
pod_restarts() { kubectl get pods -n "$NS" -l control-plane=controller-manager -o jsonpath='{range .items[*]}{.status.containerStatuses[0].restartCount}{"\n"}{end}' | awk '{s += $1} END {print s + 0}'; }
wait_for 240 "ready says the settings are missing" status_contains ready "no ConfigMap scaling-manager-settings" \
    || fail "waiting" "ready is '$(status ready)' before the settings exist"
sleep 15
[ "$(pod_restarts)" = 0 ] || fail "waiting" "the controller restarted $(pod_restarts) times while waiting for its settings"
pass "with no settings the controller waits, live (0 restarts): ready '$(status ready)'"

# --- 2. the install: settings, Prometheus's CA, watchNamespaces ---------------
#
# The e2e Prometheus has a private CA: install --prometheus-ca puts it in the
# fixed Secret the Deployment mounts and sets caFromSecret.
kubectl get secret "$PROMETHEUS_TLS_SECRET" -n "$MONITORING_NS" -o jsonpath='{.data.tls\.crt}' | base64 -d > "$WORK/prometheus-ca.crt"
[ -s "$WORK/prometheus-ca.crt" ] || fail settings "no tls.crt in Secret $PROMETHEUS_TLS_SECRET in $MONITORING_NS"
$ADMIN install --prometheus-url "$PROMETHEUS_URL" --prometheus-ca prometheus-ca.crt --keda-namespace "$KEDA_NS" \
    --watch-namespaces "$MODEL_NS" > "$LOGS/install.log" 2>&1 || { cat "$LOGS/install.log"; fail settings "scaling-manager-admin.sh install"; }
pass "scaling-manager-admin.sh install --watch-namespaces $MODEL_NS --keda-namespace $KEDA_NS (prometheusURL, caFromSecret, kedaNamespace)"

# Nothing in the e2e setup scrapes the model servers: the namespace owner
# applies the release's PodMonitor.
$OWNER monitor "$MODEL_NS" > "$LOGS/podmonitor.log" 2>&1 || { cat "$LOGS/podmonitor.log"; fail owner "scaling-manager-owner.sh monitor"; }
kubectl get podmonitor scaling-manager-modelserver-metrics -n "$MODEL_NS" -o jsonpath='{.metadata.labels.app\.kubernetes\.io/instance}' | grep -qx "$NS" \
    || fail owner "the PodMonitor in $MODEL_NS does not carry the install's instance label"
pass "scaling-manager-owner.sh monitor $MODEL_NS"

# --- 4. the controller starts in place once the settings exist, and gets ready

rollout > "$LOGS/rollout-1.log" 2>&1 || fail controller "the Deployment did not become Ready: $(cat "$LOGS/rollout-1.log")"
# prometheusURL and caFromSecret are read at start: the waiting controller
# reads them and starts, with no restart (--wait-for-settings).
wait_for 300 "the controller read prometheusURL" status_contains settingsSources "prometheusURL=settings" \
    || fail controller "settingsSources: '$(status settingsSources)'"
[ "$(pod_restarts)" = 0 ] || fail controller "the controller restarted $(pod_restarts) times on its way to reading the settings; it should start in place"
wait_for 300 "ready=ok" ready_ok || { kubectl get configmap "$STATUS_CM" -n "$NS" -o yaml | grep -E '^\s+(ready|preflight\.)' ; fail ready "ready is '$(status ready)'"; }
pass "ready ok; preflight: $(kubectl get configmap "$STATUS_CM" -n "$NS" -o json | jq -r '[.data | to_entries[] | select(.key | startswith("preflight.")) | .value | split(":")[0]] | group_by(.) | map("\(length) \(.[0])") | join(", ")')"
kubectl get configmap "$STATUS_CM" -n "$NS" -o json | jq -r '.data | to_entries[] | select(.key | startswith("preflight.")) | select(.value | startswith("ok") | not) | "      \(.key) \(.value)"' | cut -c1-220 | tee -a "$SUMMARY"
kubectl get events -n "$NS" --field-selector "involvedObject.name=$STATUS_CM" -o name | grep -q . \
    || fail ready "no Event on $STATUS_CM for the preflight checks"
pass "the preflight checks are Events on $STATUS_CM"

wait_for 120 "namespacesManaged=$MODEL_NS" status_is namespacesManaged "$MODEL_NS" || fail namespaces "namespacesManaged is '$(status namespacesManaged)'"
wait_for 120 "namespace.$MODEL_NS readiness" status_has_prefix "namespace.$MODEL_NS" "" || true
[ -n "$(status "namespace.$MODEL_NS")" ] || fail namespaces "no readiness key namespace.$MODEL_NS"
pass "namespacesManaged: $(status namespacesManaged); namespace.$MODEL_NS: $(status "namespace.$MODEL_NS" | cut -c1-120)"

# --- 5. KEDA's side, written by the controller -------------------------------

wait_for 120 "scalerTLS mutual TLS" status_has_prefix scalerTLS "mutual TLS" || fail "scaler TLS" "scalerTLS is '$(status scalerTLS)'"
copy="scaling-manager-external-scaler-$NS"
[ -n "$(kubectl get secret "$copy" -n "$KEDA_NS" -o jsonpath='{.data.tls\.crt}')" ] || fail "scaler TLS" "KEDA's copy $copy in $KEDA_NS is empty"
[ "$(secret_sha "$KEDA_NS" "$copy")" = "$(secret_sha "$NS" scaling-manager-scaler-client)" ] \
    || fail "scaler TLS" "KEDA's copy does not hold the client certificate"
status_is kedaCopyStale false || fail "scaler TLS" "kedaCopyStale is '$(status kedaCopyStale)'"
kubectl get configmap scaling-manager-scaler-keda-auth -n "$NS" >/dev/null 2>&1 && fail "scaler TLS" "a marker ConfigMap exists; nothing on this path writes one"
# The in-controller keda.mtls judges this path by the controller's status,
# not by a marker; its first run can precede the status ConfigMap.
wait_for 150 "preflight.keda.mtls ok" status_has_prefix preflight.keda.mtls ok \
    || fail "scaler TLS" "preflight.keda.mtls is '$(status preflight.keda.mtls)'"
pass "mutual TLS with no step by a person: KEDA's copy in $KEDA_NS holds the client certificate; kedaCopyStale false; preflight.keda.mtls ok"

# --- 5b. a changed restart setting restarts the controller -------------------

restarts_more_than() { [ "$(pod_restarts)" -gt "$1" ]; }
before="$(pod_restarts)"
$ADMIN set logVerbosity=3 > /dev/null || fail "self-restart" "scaling-manager-admin.sh set logVerbosity=3"
wait_for 180 "the controller restarted itself" restarts_more_than "$before" || fail "self-restart" "restartCount stayed $before after a restart setting changed"
rollout > "$LOGS/rollout-restart.log" 2>&1 || fail "self-restart" "the restarted controller did not become Ready"
wait_for 180 "settingsSources logVerbosity=settings" status_contains settingsSources "logVerbosity=settings" \
    || fail "self-restart" "settingsSources: '$(status settingsSources)'"
wait_for 180 "ready=ok again" ready_ok || fail "self-restart" "ready is '$(status ready)'"
pass "a changed restart setting (logVerbosity) restarted the controller once ($before -> $(pod_restarts)); ready ok again"

# --- 6. a re-apply keeps what the controller and the operator wrote ----------

ca_before="$(secret_sha "$NS" scaling-manager-scaler-ca)"
copy_before="$(secret_sha "$KEDA_NS" "$copy")"
settings_patch '{"data":{"utilizationShare":"shadow"}}'
$ADMIN upgrade --keda-namespace "$KEDA_NS" > "$LOGS/apply-2.log" 2>&1 || { cat "$LOGS/apply-2.log"; fail "re-apply" "a second scaling-manager-admin.sh upgrade"; }
[ "$(secret_sha "$NS" scaling-manager-scaler-ca)" = "$ca_before" ] || fail "re-apply" "the scaler CA changed on a re-apply"
[ "$(secret_sha "$KEDA_NS" "$copy")" = "$copy_before" ] || fail "re-apply" "KEDA's copy changed on a re-apply"
[ "$(kubectl get configmap scaling-manager-settings -n "$NS" -o jsonpath='{.data.utilizationShare}')" = shadow ] \
    || fail "re-apply" "the live utilizationShare patch did not survive the upgrade"
settings_patch '{"data":{"utilizationShare":"off"}}'
pass "upgrade (re-apply of the manifest and KEDA's side): scaler CA, KEDA's copy and a live switch kept"

# --- 6b. the per-namespace grants ----------------------------------------------

SA="system:serviceaccount:$NS:$DEPLOY"
can() { kubectl auth can-i "$1" "$2" -n "$MODEL_NS" --as="$SA" 2>/dev/null; }
# can_dset: a SubjectAccessReview, since kubectl auth can-i resolves a
# resource by discovery, and the DisaggregatedSet CRD may not be installed.
can_dset() {
    local allowed
    allowed="$(printf '{"apiVersion":"authorization.k8s.io/v1","kind":"SubjectAccessReview","spec":{"user":"%s","groups":["system:serviceaccounts"],"resourceAttributes":{"namespace":"%s","verb":"patch","group":"disaggregatedset.x-k8s.io","resource":"disaggregatedsets"}}}' "$SA" "$MODEL_NS" \
        | kubectl create -f - -o jsonpath='{.status.allowed}' 2>/dev/null)"
    if [ "$allowed" = true ]; then echo yes; else echo no; fi
}
[ "$(can patch pods)" = no ] || fail grants "the controller may patch pods in $MODEL_NS before the grant"
[ "$(can_dset)" = no ] || fail grants "the controller may patch disaggregatedsets before the grant"
$OWNER grant pod-writer "$MODEL_NS" > "$LOGS/grant.log" 2>&1 \
    || { cat "$LOGS/grant.log"; fail grants "scaling-manager-owner.sh grant pod-writer"; }
[ "$(can patch pods)" = yes ] || fail grants "the controller may not patch pods in $MODEL_NS after the pod-writer grant"
[ "$(can_dset)" = no ] || fail grants "the pod-writer grant also grants the role writer"
$OWNER grant dset-role-writer "$MODEL_NS" > "$LOGS/grant-dset.log" 2>&1 \
    || { cat "$LOGS/grant-dset.log"; fail grants "scaling-manager-owner.sh grant dset-role-writer"; }
[ "$(can_dset)" = yes ] || fail grants "the controller may not patch disaggregatedsets after its grant"
pass "grants: patch on pods and on disaggregatedsets in $MODEL_NS only after the owner applies each grant there"

# --- 6c. the suggested ScaledObjects, reviewed and applied -------------------

PROBE=plan-probe-decode
printf 'apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: %s\n  namespace: %s\nspec:\n  replicas: 1\n  selector:\n    matchLabels: {app: %s}\n  template:\n    metadata:\n      labels: {app: %s, llm-d.ai/role: decode}\n    spec:\n      containers:\n      - name: vllm\n        image: registry.k8s.io/pause:3.10\n        args: ["--model", "e2e/plan-probe-model"]\n' \
    "$PROBE" "$MODEL_NS" "$PROBE" "$PROBE" > plan-probe.yaml
kubectl apply -f plan-probe.yaml > "$LOGS/plan-probe.log" 2>&1 || { cat "$LOGS/plan-probe.log"; fail plan "applying the probe model server"; }
plan_of() { kubectl get configmap scaling-manager-scaledobjects-plan -n "$NS" -o "jsonpath={.data.${MODEL_NS//./\\.}}" 2>/dev/null; }
plan_has_probe() { plan_of | grep -q "name: $PROBE"; }
wait_for 150 "the plan suggests a ScaledObject for $PROBE" plan_has_probe || fail plan "the plan for $MODEL_NS: $(plan_of | head -5)"
plan_of | grep -q "modelID: e2e/plan-probe-model" || fail plan "the suggested ScaledObject does not carry the model id"
plan_of | yq 'select(.metadata.name == "'"$PROBE"'")' > so-probe.yaml
$OWNER register "$MODEL_NS" so-probe.yaml > "$LOGS/plan-apply.log" 2>&1 || { cat "$LOGS/plan-apply.log"; fail plan "scaling-manager-owner.sh register with the suggested ScaledObject"; }
so_ready() { [ "$(kubectl get scaledobject "$PROBE" -n "$MODEL_NS" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" = True ]; }
wait_for 180 "the suggested ScaledObject is Ready" so_ready || fail plan "ScaledObject $PROBE: $(kubectl get scaledobject "$PROBE" -n "$MODEL_NS" -o jsonpath='{.status.conditions}')"
# The namespace owner reads the readiness in their own namespace: an Event
# on the ScaledObject, from the controller's namespace reporter.
owner_sees() { $OWNER status "$MODEL_NS" 2>/dev/null | grep -q "ScalingManagerNamespace"; }
wait_for 120 "an Event in $MODEL_NS with the namespace's readiness" owner_sees || fail plan "no ScalingManagerNamespace Event in $MODEL_NS: $($OWNER status "$MODEL_NS" 2>&1 | tail -3)"
pass "the owner reads the readiness in $MODEL_NS: $($OWNER status "$MODEL_NS" | grep ScalingManagerNamespace | tail -1 | cut -c1-160)"
kubectl delete -f so-probe.yaml -f plan-probe.yaml --ignore-not-found > /dev/null
pass "the plan suggests a ScaledObject for a model server (model id from --model); applied as reviewed, KEDA wires it Ready over mutual TLS"

# --- 7. negative control: a namespace watchNamespaces leaves out --------------

kubectl create namespace "$OUTSIDE_NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
printf 'apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: outside\n  namespace: %s\nspec:\n  replicas: 1\n  selector:\n    matchLabels: {app: outside}\n  template:\n    metadata:\n      labels: {app: outside}\n    spec:\n      containers:\n      - name: pause\n        image: registry.k8s.io/pause:3.10\n---\napiVersion: keda.sh/v1alpha1\nkind: ScaledObject\nmetadata:\n  name: outside\n  namespace: %s\nspec:\n  scaleTargetRef: {name: outside}\n  minReplicaCount: 1\n  maxReplicaCount: 2\n  triggers:\n  - type: external\n    authenticationRef: {kind: ClusterTriggerAuthentication, name: %s}\n    metadata:\n      scalerAddress: scaling-manager-external-scaler.%s.svc:9090\n' \
    "$OUTSIDE_NS" "$OUTSIDE_NS" "$copy" "$NS" > outside.yaml
$OWNER register "$OUTSIDE_NS" outside.yaml > "$LOGS/outside.log" 2>&1 || { cat "$LOGS/outside.log"; fail "outside" "registering the ScaledObject in $OUTSIDE_NS"; }
wait_for 180 "namespacesRefused names $OUTSIDE_NS" status_contains namespacesRefused "$OUTSIDE_NS: it is not in the watchNamespaces setting" \
    || fail "outside" "the scaler did not refuse $OUTSIDE_NS: namespacesRefused '$(status namespacesRefused)'"
outside_sees() { $OWNER status "$OUTSIDE_NS" 2>/dev/null | grep -q "ScalingManagerNamespaceNotManaged"; }
wait_for 120 "the owner of $OUTSIDE_NS reads why, in $OUTSIDE_NS" outside_sees || fail "outside" "no NotManaged Event in $OUTSIDE_NS"
pass "a namespace watchNamespaces leaves out is refused at the scaler, and its owner reads why there: $($OWNER status "$OUTSIDE_NS" | grep NotManaged | tail -1 | cut -c1-160)"

# watchNamespaces takes effect without a restart: the admin lists the
# namespace, and the same ScaledObject is served, with no restart.
restarts_before="$(pod_restarts)"
$ADMIN namespaces "$MODEL_NS,$OUTSIDE_NS" > /dev/null || fail "outside" "scaling-manager-admin.sh namespaces"
wait_for 120 "namespacesManaged=$MODEL_NS,$OUTSIDE_NS" status_is namespacesManaged "$(printf '%s\n' "$MODEL_NS" "$OUTSIDE_NS" | sort | paste -sd,)" \
    || fail "outside" "namespacesManaged is '$(status namespacesManaged)' after the setting listed $OUTSIDE_NS"
outside_ready() { [ "$(kubectl get scaledobject outside -n "$OUTSIDE_NS" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" = True ]; }
wait_for 240 "the ScaledObject in $OUTSIDE_NS is Ready" outside_ready || fail "outside" "ScaledObject outside: $(kubectl get scaledobject outside -n "$OUTSIDE_NS" -o jsonpath='{.status.conditions}')"
[ "$(pod_restarts)" = "$restarts_before" ] || fail "outside" "the controller restarted ($restarts_before -> $(pod_restarts)) on a watchNamespaces change"
pass "watchNamespaces listed $OUTSIDE_NS live: managed and its ScaledObject Ready with no restart ($(pod_restarts) restarts)"
kubectl delete namespace "$OUTSIDE_NS" --wait=false >/dev/null

# --- 8. a model scales, through KEDA and the scaler over mutual TLS ----------

if [ -n "$SMOKE_CMD" ]; then
    # The smoke specs make namespaces with generated names: every namespace
    # (an empty watchNamespaces), and active.
    $ADMIN namespaces --all > /dev/null || fail smoke "scaling-manager-admin.sh namespaces --all"
    wait_for 60 "namespacesManaged=*" status_is namespacesManaged "*" || fail smoke "namespacesManaged is '$(status namespacesManaged)'"
    $ADMIN set passive=false > /dev/null || fail smoke "scaling-manager-admin.sh set passive=false"
    wait_for 60 "passiveDefault=false" status_is passiveDefault false || fail smoke "passiveDefault is '$(status passiveDefault)'"
    log "running the smoke suite against this install: $SMOKE_CMD"
    (cd "$REPO_DIR" && bash -c "$SMOKE_CMD") > "$LOGS/smoke.log" 2>&1
    rc=$?
    plain="$(sed 's/\x1b\[[0-9;]*m//g' "$LOGS/smoke.log")"
    ran="$(printf '%s\n' "$plain" | grep -oE 'Ran [0-9]+ of [0-9]+ Specs' | tail -1)"
    counts="$(printf '%s\n' "$plain" | grep -oE '[0-9]+ Passed \| [0-9]+ Failed \| [0-9]+ Pending \| [0-9]+ Skipped' | tail -1)"
    log "smoke: exit $rc, ${ran:-no Ran line}, ${counts:-no counts}"
    printf '%s\n' "$plain" | grep -E '^\s*\[FAIL\]' | sort -u | head -20
    [ "$(printf '%s' "$ran" | awk '{print $2}')" -gt 0 ] 2>/dev/null || fail smoke "ran 0 specs"
    [ "$rc" -eq 0 ] || fail smoke "exit $rc ($ran: $counts); see $LOGS/smoke.log"
    wait_for 120 "kedaConnection=current" status_is kedaConnection current || fail smoke "kedaConnection is '$(status kedaConnection)'"
    pass "smoke suite against the manifest install: $ran: $counts; kedaConnection current"
    $ADMIN set passive=true > /dev/null
else
    notrun "smoke suite" "SMOKE_CMD unset"
fi

if [ "$KEEP_INSTALL" = true ]; then
    notrun uninstall "KEEP_INSTALL=true"
    echo "=== summary ==="; cat "$SUMMARY"
    exit 0
fi

# --- 9. uninstall (appendix F) -----------------------------------------------

$ADMIN uninstall --keda-namespace "$KEDA_NS" > "$LOGS/uninstall.log" 2>&1 || { cat "$LOGS/uninstall.log"; fail uninstall "scaling-manager-admin.sh uninstall"; }
left=""
for r in $(kubectl api-resources --verbs=list -o name 2>/dev/null | grep -v -E '^(events|events.events.k8s.io)$'); do
    o="$(kubectl get "$r" -A -l "$INSTANCE" -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}' 2>/dev/null | sed "s|^|$r |")"
    [ -z "$o" ] || left="$left$o"$'\n'
done
left="$(printf '%s' "$left" | grep . | sort -u)"
printf '%s\n' "$left" > "$LOGS/left-after-uninstall.txt"
[ -z "$left" ] || fail uninstall "left behind: $(echo $left)"
kubectl get namespace "$NS" >/dev/null 2>&1 && fail uninstall "namespace $NS is still there"
[ -z "$(kubectl get namespace -l scaling-manager.llmd.ai/keda -o name)" ] || fail uninstall "a namespace still carries scaling-manager.llmd.ai/keda"
sha256sum -c --ignore-missing --quiet SHA256SUMS > "$LOGS/sums-after.log" 2>&1 || fail uninstall "a release file changed during the run: $(cat "$LOGS/sums-after.log")"
pass "uninstall: nothing labelled $INSTANCE left, namespace $NS gone, KEDA's namespace unlabelled; every release file as downloaded"

echo "=== summary ==="
cat "$SUMMARY"
echo "Logs: $LOGS"
