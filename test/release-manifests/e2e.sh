#!/usr/bin/env bash
#
# Installs the scaling manager from the rendered release files with kubectl
# only, the way docs/guides/install-in-namespace (or, with
# RELEASE_E2E_SCOPE=cluster, docs/guides/install-cluster-wide) tells an
# operator to, and checks each step. `make test-e2e-release-manifests` runs it
# on a kind cluster whose prerequisites (Prometheus, KEDA, the llm-d CRDs and
# EPP) the existing e2e setup installed, without the scaling manager.
#
#   test/release-manifests/e2e.sh <dir with the rendered release files>
#
# It copies the files to a scratch directory and works there, so nothing it
# runs can reach deploy/ or the rest of the checkout. The one exception is
# SMOKE_CMD, the existing smoke suite, which it runs against this install when
# set.
#
# Environment:
#   RELEASE_E2E_SCOPE  namespace (default): scaling-manager-namespaced.yaml
#                    and scaling-manager-preflight.yaml, the controller with
#                    the models. cluster: scaling-manager-cluster.yaml and
#                    scaling-manager-preflight-cluster.yaml, the controller in
#                    scaling-manager-system and the models in MODEL_NS.
#   NS               the controller's namespace (default llm-d-sim, where the
#                    e2e setup puts the model servers and the EPP; with scope
#                    cluster, scaling-manager-system, which the files name)
#   MODEL_NS         scope cluster only: the namespace with the model servers
#                    (default llm-d-sim). Scope namespace manages NS alone.
#   KEDA_NS          where KEDA runs (default keda-system)
#   MONITORING_NS    where the e2e Prometheus runs (default scaling-manager-monitoring)
#   PROMETHEUS_URL   default the e2e Prometheus Service in MONITORING_NS
#   PROMETHEUS_TLS_SECRET  the Secret in MONITORING_NS whose tls.crt signs
#                    Prometheus's certificate (default prometheus-web-tls)
#   SMOKE_CMD        a command that runs the smoke suite against this install
#                    (with passive off); unset, that step does not run
#   KEEP_INSTALL     true: stop before the uninstall
#
# Exit 0 only when every step passed. The summary lists each step.

set -uo pipefail

DIST="${1:?usage: $0 <dir with the rendered release files>}"
SCOPE="${RELEASE_E2E_SCOPE:-namespace}"
# The cluster-wide files name their namespace; nothing fills it in.
CLUSTER_NS=scaling-manager-system
case "$SCOPE" in
    namespace)
        NS="${NS:-llm-d-sim}"
        MODEL_NS="$NS"
        MAIN=scaling-manager-namespaced.yaml
        PREFLIGHT=scaling-manager-preflight.yaml
        # The guide splits each file between the cluster admin (part=admin)
        # and the namespace owner (part=controller), who applies the rest.
        OWNER_PART=(-l scaling-manager.llmd.ai/part=controller)
        ;;
    cluster)
        NS="${NS:-$CLUSTER_NS}"
        MODEL_NS="${MODEL_NS:-llm-d-sim}"
        [ "$NS" = "$CLUSTER_NS" ] || { echo "RELEASE_E2E_SCOPE=cluster: the cluster-wide files install in $CLUSTER_NS, not NS=$NS" >&2; exit 2; }
        [ "$MODEL_NS" != "$NS" ] || { echo "RELEASE_E2E_SCOPE=cluster: MODEL_NS must differ from $NS" >&2; exit 2; }
        MAIN=scaling-manager-cluster.yaml
        PREFLIGHT=scaling-manager-preflight-cluster.yaml
        # One admin applies the whole of each file.
        OWNER_PART=()
        ;;
    *) echo "RELEASE_E2E_SCOPE must be namespace or cluster, not '$SCOPE'" >&2; exit 2 ;;
esac
KEDA_NS="${KEDA_NS:-keda-system}"
MONITORING_NS="${MONITORING_NS:-scaling-manager-monitoring}"
PROMETHEUS_URL="${PROMETHEUS_URL:-https://kube-prometheus-stack-prometheus.$MONITORING_NS.svc.cluster.local:9090}"
PROMETHEUS_TLS_SECRET="${PROMETHEUS_TLS_SECRET:-prometheus-web-tls}"
SMOKE_CMD="${SMOKE_CMD:-}"
KEEP_INSTALL="${KEEP_INSTALL:-false}"
REPO_DIR="$(pwd)"

STATUS_CM=scaling-manager-controller-status
DEPLOY=scaling-manager-controller-manager
CA_PATH=/etc/scaling-manager-prometheus-ca/ca.crt

WORK="$(mktemp -d)"
LOGS="${E2E_LOG_DIR:-$WORK/logs}"
mkdir -p "$LOGS"
SUMMARY="$LOGS/summary.txt"
: > "$SUMMARY"

log() { printf '[%s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
pass() { printf 'PASS  %s\n' "$1" | tee -a "$SUMMARY"; }
notrun() { printf 'NOT RUN  %s: %s\n' "$1" "$2" | tee -a "$SUMMARY"; }
note() { printf 'NOTE  %s\n' "$1" | tee -a "$SUMMARY"; }

# fail <step> <why>: record, print what the cluster shows, stop.
fail() {
    printf 'FAIL  %s: %s\n' "$1" "$2" | tee -a "$SUMMARY"
    diagnostics
    echo "=== summary ==="; cat "$SUMMARY"
    echo "Logs and the files as applied: $LOGS, $WORK"
    exit 1
}

diagnostics() {
    echo "=== diagnostics ==="
    kubectl get pods -n "$NS" -o wide 2>&1 | head -30
    [ "$MODEL_NS" = "$NS" ] || kubectl get pods -n "$MODEL_NS" -o wide 2>&1 | head -30
    kubectl get configmap "$STATUS_CM" -n "$NS" -o yaml 2>&1 | grep -v -E '^\s+scalerCACert|BEGIN CERT|END CERT|^\s+[A-Za-z0-9+/=]{40,}$' | head -80
    kubectl logs -n "$NS" "deployment/$DEPLOY" --tail=80 2>&1 | tail -80
}

# wait_for <seconds> <what> <command...>: until the command succeeds.
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

status() { kubectl get configmap "$STATUS_CM" -n "$NS" -o "jsonpath={.data.$1}" 2>/dev/null; }
status_is() { [ "$(status "$1")" = "$2" ]; }
status_has_prefix() { case "$(status "$1")" in "$2"*) return 0 ;; esac; return 1; }
status_set() { [ -n "$(status "$1")" ]; }
settings_patch() { kubectl patch configmap scaling-manager-settings -n "$NS" --type merge -p "$1" >/dev/null; }

# The status mirrors the settings ConfigMap's resourceVersion once it has
# read a change: wait for that before reading the value a change affects.
wait_settings_read() {
    local rv
    rv="$(kubectl get configmap scaling-manager-settings -n "$NS" -o jsonpath='{.metadata.resourceVersion}')"
    wait_for 120 "the controller read settings resourceVersion $rv" status_is settingsResourceVersion "$rv"
}

rollout() { kubectl rollout status "deployment/$DEPLOY" -n "$NS" --timeout=300s; }

# run_preflight <name>: delete and apply the preflight Job, wait for it to end,
# save its log to $LOGS/preflight-<name>.log. Prints Complete or Failed.
run_preflight() {
    local name="$1" end="" i=0
    kubectl delete job scaling-manager-preflight -n "$NS" --ignore-not-found --wait=true >/dev/null
    kubectl apply -f "$PREFLIGHT" "${OWNER_PART[@]}" > "$LOGS/preflight-$name.apply.log" 2>&1 || return 1
    while [ "$i" -lt 600 ]; do
        end="$(kubectl get job scaling-manager-preflight -n "$NS" \
            -o jsonpath='{.status.conditions[?(@.status=="True")].type}' 2>/dev/null | grep -oE 'Complete|Failed' | head -1)"
        [ -n "$end" ] && break
        sleep 3; i=$((i + 3))
    done
    kubectl logs -n "$NS" job/scaling-manager-preflight > "$LOGS/preflight-$name.log" 2>&1
    log "preflight $name: ${end:-timed out}; $(grep -E '^[0-9]+ PASS' "$LOGS/preflight-$name.log" || echo 'no summary line')"
    echo "${end:-timeout}" > "$LOGS/preflight-$name.end"
}

# fails_in <log>: the checks a preflight log reports FAIL, one per line.
fails_in() { awk '$1 == "FAIL" {print $2}' "$1" | sort -u; }
status_of() { awk -v c="$2" '$2 == c {print $1; exit}' "$1"; }

# --- the files, as an operator downloads them ------------------------------

cp "$DIST"/scaling-manager-*.yaml "$WORK"/ || fail files "no release files in $DIST"
cd "$WORK" || exit 1
n="$(ls scaling-manager-*.yaml | wc -l)"
[ "$n" -ge 15 ] || fail files "expected the 15 release files in $DIST, found $n"
pass "files: $n release files copied to $WORK"

log "Kubernetes: $(kubectl version -o json 2>/dev/null | grep -m1 gitVersion | tr -d ' ",')"
log "scope $SCOPE: $MAIN and $PREFLIGHT; controller in $NS, models in $MODEL_NS"

# --- 1. namespace and placeholders -----------------------------------------

if [ "$SCOPE" = cluster ]; then
    # "Applies as is": the cluster-wide files name their namespace already.
    left="$(grep -l -E '__[A-Z_]+__' "$MAIN" "$PREFLIGHT" || true)"
    [ -z "$left" ] || fail placeholders "the cluster-wide file $left holds a placeholder: $(grep -o -h -E '__[A-Z_]+__' $left | sort -u | tr '\n' ' ')"
    grep -q "namespace: $NS\$" "$MAIN" || fail placeholders "$MAIN does not name namespace $NS"
    kubectl get namespace "$MODEL_NS" >/dev/null 2>&1 || fail namespace "no model namespace $MODEL_NS (the e2e setup makes it)"
fi
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null || fail namespace "cannot create $NS"
for f in scaling-manager-*.yaml; do
    sed -i -e "s/__NAMESPACE__/$NS/g" -e "s/__MODEL_NAMESPACE__/$MODEL_NS/g" "$f"
done
left="$(grep -l '__NAMESPACE__\|__MODEL_NAMESPACE__' "$MAIN" scaling-manager-settings.yaml \
    "$PREFLIGHT" scaling-manager-model-namespace.yaml scaling-manager-activate.yaml scaling-manager-scaler-tls.yaml || true)"
[ -z "$left" ] || fail placeholders "still in $left"
pass "namespace $NS and placeholders (model namespace $MODEL_NS)"

# --- 2. the admin part ------------------------------------------------------

if [ "$SCOPE" = namespace ]; then
    # The cluster admin's step, as the guide splits it: the admin part of the
    # main file and of the preflight file (its ClusterRole and binding), and
    # the model-namespace file, which is all part=admin (a Role in the model
    # namespace).
    {
        kubectl apply -f "$MAIN" -l scaling-manager.llmd.ai/part=admin \
            && kubectl apply -f "$PREFLIGHT" -l scaling-manager.llmd.ai/part=admin \
            && kubectl apply -f scaling-manager-model-namespace.yaml
    } > "$LOGS/apply-admin.log" 2>&1 || { cat "$LOGS/apply-admin.log"; fail admin "the cluster admin's applies failed"; }
    n="$(grep -c . "$LOGS/apply-admin.log")"
    kubectl get deployment "$DEPLOY" -n "$NS" >/dev/null 2>&1 && fail admin "part=admin created the controller Deployment"
    kubectl get job scaling-manager-preflight -n "$NS" >/dev/null 2>&1 && fail admin "part=admin of the preflight file created the Job"
    pass "admin part applied ($n objects: main file, preflight RBAC, model-namespace file), no Deployment, no Job"
else
    # The cluster-wide guide has one actor, the cluster admin, who applies
    # each file whole, in the guide's order: nothing of the main file before
    # the preflight has run.
    note "scope cluster: no admin/controller split; each file is applied whole, in the guide's order"
fi

# --- 3. settings and the Prometheus CA --------------------------------------

kubectl get secret "$PROMETHEUS_TLS_SECRET" -n "$MONITORING_NS" -o jsonpath='{.data.tls\.crt}' | base64 -d > "$WORK/prometheus-ca.crt"
[ -s "$WORK/prometheus-ca.crt" ] || fail settings "no tls.crt in Secret $PROMETHEUS_TLS_SECRET in $MONITORING_NS"
kubectl create secret generic scaling-manager-prometheus-ca -n "$NS" --from-file=ca.crt="$WORK/prometheus-ca.crt" >/dev/null \
    || fail settings "could not create Secret scaling-manager-prometheus-ca"
sed -i -e "s|^  prometheusURL: \"\"|  prometheusURL: \"$PROMETHEUS_URL\"|" \
       -e "s|^  prometheusCACertPath: \"\"|  prometheusCACertPath: \"$CA_PATH\"|" scaling-manager-settings.yaml
grep -q "prometheusURL: \"$PROMETHEUS_URL\"" scaling-manager-settings.yaml || fail settings "the sed did not set prometheusURL"
grep -q "prometheusCACertPath: \"$CA_PATH\"" scaling-manager-settings.yaml || fail settings "the sed did not set prometheusCACertPath"
kubectl create -f scaling-manager-settings.yaml >/dev/null || fail settings "kubectl create -f scaling-manager-settings.yaml"
if kubectl create -f scaling-manager-settings.yaml > "$LOGS/settings-create-again.log" 2>&1; then
    fail settings "a second kubectl create of the settings succeeded"
fi
grep -q AlreadyExists "$LOGS/settings-create-again.log" || fail settings "a second create failed, but not with AlreadyExists: $(cat "$LOGS/settings-create-again.log")"
pass "settings created (prometheusURL, prometheusCACertPath set); a second create is AlreadyExists"

# --- 4. preflight before the install ----------------------------------------
#
# The release file's scaler NetworkPolicy admits KEDA from namespace keda
# (KEDA's chart default); the e2e KEDA runs in keda-system. The admin part
# applied that policy, so the preflight must FAIL keda.networkpolicy and print
# the kubectl patch that fixes the live object. The operator runs it, and makes
# the same change in the file so a later apply of the file does not undo it.

run_preflight before-fix
pf="$LOGS/preflight-before-fix.log"
[ "$(status_of "$pf" keda.networkpolicy)" = FAIL ] \
    || fail "preflight before the install" "expected keda.networkpolicy FAIL for KEDA in $KEDA_NS, got '$(status_of "$pf" keda.networkpolicy)'"
fix="$(grep -A1 -E '^FAIL +keda.networkpolicy' "$pf" | sed -n 's/^ *fix: //p')"
[ -n "$fix" ] || fail "preflight before the install" "keda.networkpolicy printed no fix"
[ "$(cat "$LOGS/preflight-before-fix.end")" = Failed ] || fail "preflight before the install" "a FAIL did not fail the Job"
pass "preflight names keda.networkpolicy FAIL with a fix; the Job failed"

if [ "$SCOPE" = namespace ]; then
    log "running the preflight's fix: $fix"
    bash -c "$fix" > "$LOGS/networkpolicy-fix.log" 2>&1 || { cat "$LOGS/networkpolicy-fix.log"; fail "preflight fix" "the printed fix did not run: $fix"; }
    kubectl get networkpolicy scaling-manager-external-scaler -n "$NS" -o yaml | grep -q "kubernetes.io/metadata.name: $KEDA_NS" \
        || fail "preflight fix" "after the fix the NetworkPolicy does not name $KEDA_NS"
    sed -i "s#kubernetes.io/metadata.name: keda\$#kubernetes.io/metadata.name: $KEDA_NS#" "$MAIN"
    pass "the printed fix admits $KEDA_NS (and the file says the same)"
else
    # Nothing of the cluster file is applied yet, so the fix is an edit of
    # the file. The cluster-wide guide gives its own sed for it (the printed
    # one names the namespaced files); run the guide's.
    log "running the cluster-wide guide's sed for KEDA in $KEDA_NS"
    sed -i "s#kubernetes.io/metadata.name: keda\$#kubernetes.io/metadata.name: $KEDA_NS#" "$MAIN"
    [ "$(grep -c "kubernetes.io/metadata.name: $KEDA_NS\$" "$MAIN")" -ge 1 ] \
        || fail "preflight fix" "after the guide's sed $MAIN does not name $KEDA_NS"
    pass "the guide's sed makes $MAIN admit $KEDA_NS"
fi

run_preflight before-install
pf="$LOGS/preflight-before-install.log"
fails="$(fails_in "$pf")"
before_pass="settings keda.crd keda.version keda.networkpolicy prometheus.reachable"
if [ "$SCOPE" = cluster ] && [ "$fails" = keda.networkpolicy ] \
    && grep -A1 -E '^FAIL +keda.networkpolicy' "$pf" | grep -q 'not applied yet'; then
    # The preflight cannot read the edited file: until the NetworkPolicy is
    # applied it judges the file as shipped (keda), so this FAIL stays until
    # the install. The run after the install must PASS it.
    grep -E '^FAIL +keda.networkpolicy' "$pf" | grep -q "KEDA runs in $KEDA_NS:" \
        || fail "preflight before the install" "the keda.networkpolicy FAIL does not name $KEDA_NS: $(grep -m1 -E '^FAIL +keda.networkpolicy' "$pf")"
    [ "$(cat "$LOGS/preflight-before-install.end")" = Failed ] \
        || fail "preflight before the install" "keda.networkpolicy FAILed, but the Job ended '$(cat "$LOGS/preflight-before-install.end")', not Failed"
    note "scope cluster: keda.networkpolicy FAILs until the cluster file is applied (the preflight reads the shipped file, not the edited one); the run after the install must PASS it"
    fails=""
    before_pass="settings keda.crd keda.version prometheus.reachable"
fi
[ -z "$fails" ] || { cat "$pf"; fail "preflight before the install" "FAIL: $(echo $fails)"; }
if [ -z "$(fails_in "$pf")" ]; then
    [ "$(cat "$LOGS/preflight-before-install.end")" = Complete ] || fail "preflight before the install" "the Job did not complete"
fi
for c in $before_pass; do
    [ "$(status_of "$pf" "$c")" = PASS ] || fail "preflight before the install" "$c is $(status_of "$pf" "$c"), not PASS"
done
pass "preflight before the install: no unexpected FAIL; $(grep -E '^[0-9]+ PASS' "$pf")"
grep -E '^(WARN|UNKNOWN) ' "$pf" | sed 's/^/      /' | tee -a "$SUMMARY"

# --- 5. the scaler TLS placeholders, then the controller --------------------

kubectl create -f scaling-manager-scaler-tls.yaml > "$LOGS/scaler-tls-create.log" 2>&1 \
    || { cat "$LOGS/scaler-tls-create.log"; fail "scaler TLS Secrets" "kubectl create -f scaling-manager-scaler-tls.yaml"; }
pass "scaler TLS Secrets created empty"

kubectl apply -f "$MAIN" "${OWNER_PART[@]}" > "$LOGS/apply-controller.log" 2>&1 \
    || { cat "$LOGS/apply-controller.log"; fail controller "kubectl apply -f $MAIN ${OWNER_PART[*]} failed"; }
if [ "$SCOPE" = cluster ]; then
    # The guide's next step, in each namespace with model servers: the
    # model-server PodMonitor (and a Role the cluster-wide controller does
    # not need).
    kubectl apply -f scaling-manager-model-namespace.yaml > "$LOGS/apply-model-namespace.log" 2>&1 \
        || { cat "$LOGS/apply-model-namespace.log"; fail controller "kubectl apply -f scaling-manager-model-namespace.yaml ($MODEL_NS)"; }
    kubectl get podmonitors.monitoring.coreos.com -n "$MODEL_NS" -l app.kubernetes.io/part-of=scaling-manager -o name | grep -q . \
        || fail controller "no scaling-manager PodMonitor in $MODEL_NS after the model-namespace file"
fi
# Optional file: the alerting rules. The smoke suite's PrometheusAlerts specs
# check them (DEPLOY_ALERTING_RULES=true in SMOKE_CMD).
kubectl apply -f scaling-manager-alerts.yaml > "$LOGS/apply-alerts.log" 2>&1 \
    || { cat "$LOGS/apply-alerts.log"; fail alerts "kubectl apply -f scaling-manager-alerts.yaml"; }
rollout > "$LOGS/rollout-1.log" 2>&1 || fail controller "the Deployment did not become Ready: $(cat "$LOGS/rollout-1.log")"
wait_for 120 "the status ConfigMap" status_set settingsSource || fail controller "no settingsSource in $STATUS_CM"
wait_for 120 "status passiveDefault=true" status_is passiveDefault true || fail controller "passiveDefault is '$(status passiveDefault)'"
wait_for 60 "status utilizationShare=off" status_is utilizationShare off || fail controller "utilizationShare is '$(status utilizationShare)'"
case "$(status settingsSource)" in
    *settings*) ;;
    *) fail controller "settingsSource is '$(status settingsSource)', not the settings ConfigMap" ;;
esac
pass "controller Ready; passive, utilizationShare off; settingsSource $(status settingsSource)"

# --- 6. KEDA's side of mutual TLS, by the steps the controller prints -------

wait_for 120 "status scalerTLS" status_has_prefix scalerTLS plaintext || fail "scaler TLS" "scalerTLS is '$(status scalerTLS)'"
action="$(status scalerTLSAction)"
printf '%s\n' "$action" > "$LOGS/scalerTLSAction-1.txt"
line="$(printf '%s\n' "$action" | grep -m1 'wire KEDA')" || fail "scaler TLS" "scalerTLSAction has no 'wire KEDA' step: $action"
cmd="${line#*then restart the controller: }"
[ "$cmd" != "$line" ] || fail "scaler TLS" "cannot find the command in: $line"
log "running the scalerTLSAction step with KEDA_NS=$KEDA_NS"
KEDA_NS="$KEDA_NS" bash -c "$cmd" > "$LOGS/scalerTLSAction-1.log" 2>&1 \
    || { cat "$LOGS/scalerTLSAction-1.log"; fail "scaler TLS" "the printed step failed"; }
kubectl get clustertriggerauthentication "scaling-manager-external-scaler-$NS" >/dev/null || fail "scaler TLS" "no ClusterTriggerAuthentication"
kubectl get secret "scaling-manager-external-scaler-$NS" -n "$KEDA_NS" >/dev/null || fail "scaler TLS" "no client Secret copy in $KEDA_NS"
rollout > "$LOGS/rollout-2.log" 2>&1 || fail "scaler TLS" "the restarted controller did not become Ready"
wait_for 120 "status scalerTLS mutual TLS" status_has_prefix scalerTLS "mutual TLS" || fail "scaler TLS" "scalerTLS is '$(status scalerTLS)'"
pass "scalerTLSAction step ran; the controller serves mutual TLS: $(status scalerTLS | cut -c1-120)"

run_preflight after-install
pf="$LOGS/preflight-after-install.log"
fails="$(fails_in "$pf")"
[ -z "$fails" ] || { cat "$pf"; fail "preflight after the install" "FAIL: $(echo $fails)"; }
for c in settings keda.mtls keda.networkpolicy prometheus.reachable rbac.controller; do
    [ "$(status_of "$pf" "$c")" = PASS ] || fail "preflight after the install" "$c is $(status_of "$pf" "$c"), not PASS"
done
# scaler.tls dials the scaler from the Job's Pod, and the shipped NetworkPolicy
# admits port 9090 from KEDA's namespace only: the check cannot PASS from the
# controller's namespace, and must say UNKNOWN and why, never PASS or FAIL.
# Mutual TLS is shown instead by scalerTLS above and by KEDA presenting its
# certificate in step 7.
case "$(status_of "$pf" scaler.tls)" in
    PASS) ;;
    UNKNOWN) grep -E '^UNKNOWN +scaler.tls' "$pf" | grep -q NetworkPolicy \
        || fail "preflight after the install" "scaler.tls is UNKNOWN for a reason other than the NetworkPolicy: $(grep -E '^UNKNOWN +scaler.tls' "$pf")" ;;
    *) fail "preflight after the install" "scaler.tls is '$(status_of "$pf" scaler.tls)'" ;;
esac
pass "preflight after the install: no FAIL; keda.mtls, keda.networkpolicy, rbac.controller PASS; scaler.tls $(status_of "$pf" scaler.tls); $(grep -E '^[0-9]+ PASS' "$pf")"
grep -E '^(WARN|UNKNOWN) ' "$pf" | sed 's/^/      /' | tee -a "$SUMMARY"

# --- 7. a model scales, through KEDA and the scaler over mutual TLS ---------

if [ -n "$SMOKE_CMD" ]; then
    settings_patch '{"data":{"passive":"false"}}'
    wait_settings_read || fail smoke "settings not read"
    wait_for 60 "status passiveDefault=false" status_is passiveDefault false || fail smoke "passiveDefault is '$(status passiveDefault)'"
    log "running the smoke suite against this install: $SMOKE_CMD"
    (cd "$REPO_DIR" && bash -c "$SMOKE_CMD") > "$LOGS/smoke.log" 2>&1
    rc=$?
    plain="$(sed 's/\x1b\[[0-9;]*m//g' "$LOGS/smoke.log")"
    ran="$(printf '%s\n' "$plain" | grep -oE 'Ran [0-9]+ of [0-9]+ Specs' | tail -1)"
    counts="$(printf '%s\n' "$plain" | grep -oE '[0-9]+ Passed \| [0-9]+ Failed \| [0-9]+ Pending \| [0-9]+ Skipped' | tail -1)"
    log "smoke: exit $rc, ${ran:-no Ran line}, ${counts:-no counts}"
    printf '%s\n' "$plain" | grep -E '^\s*\[FAIL\]' | sort -u | head -20
    spec_n="$(printf '%s' "$ran" | awk '{print $2}')"
    [ "${spec_n:-0}" -gt 0 ] || fail smoke "ran 0 specs"
    [ "$rc" -eq 0 ] || fail smoke "exit $rc ($ran: $counts); see $LOGS/smoke.log"
    ran="$ran: $counts"
    # KEDA presented the client certificate the controller issued, so the
    # step that asked to copy it is gone.
    wait_for 120 "scalerTLSAction cleared" status_is scalerTLSAction "" || fail smoke "scalerTLSAction still: $(status scalerTLSAction)"
    pass "smoke suite against the release install: $ran, exit 0; KEDA presented its certificate (scalerTLSAction empty)"
    settings_patch '{"data":{"passive":"true"}}'
    wait_settings_read || fail "back to passive" "settings not read"
    wait_for 60 "status passiveDefault=true" status_is passiveDefault true || fail "back to passive" "passiveDefault is '$(status passiveDefault)'"
else
    notrun "smoke suite" "SMOKE_CMD unset"
fi

# --- 8. passive, then shadow, then active -----------------------------------

accel="$(kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.labels.nvidia\.com/gpu\.product}{"\n"}{end}' | grep -m1 .)"
[ -n "$accel" ] || accel=H100
settings_patch "{\"data\":{\"limiter\":\"quota\",\"quotas\":\"$accel=64\"}}"
wait_settings_read || fail shadow "settings not read"
wait_for 60 "status limiter quota/..." status_has_prefix limiter quota/ || fail shadow "limiter is '$(status limiter)'"
settings_patch '{"data":{"utilizationShare":"shadow"}}'
wait_settings_read || fail shadow "settings not read"
wait_for 120 "status utilizationShare=shadow" status_is utilizationShare shadow || fail shadow "utilizationShare is '$(status utilizationShare)' ($(status utilizationShareError))"
status_is passiveDefault true || fail shadow "passiveDefault is '$(status passiveDefault)'"
pass "shadow: limiter $(status limiter), utilizationShare shadow, still passive"

settings_patch '{"data":{"passive":"false"}}'
wait_settings_read || fail "not passive" "settings not read"
wait_for 60 "status passiveDefault=false" status_is passiveDefault false || fail "not passive" "passiveDefault is '$(status passiveDefault)'"
pass "passive false, sharing still shadow"

kubectl apply -f scaling-manager-activate.yaml > "$LOGS/apply-activate.log" 2>&1 \
    || { cat "$LOGS/apply-activate.log"; fail active "kubectl apply -f scaling-manager-activate.yaml"; }
kubectl auth can-i patch pods -n "$MODEL_NS" --as="system:serviceaccount:$NS:$DEPLOY" | grep -qx yes \
    || fail active "the controller may not patch pods in $MODEL_NS after the activation file"
settings_patch '{"data":{"utilizationShare":"active"}}'
wait_settings_read || fail active "settings not read"
wait_for 120 "status utilizationShare=active" status_is utilizationShare active || fail active "utilizationShare is '$(status utilizationShare)' ($(status utilizationShareError))"
run_preflight active
pf="$LOGS/preflight-active.log"
fails="$(fails_in "$pf")"
[ -z "$fails" ] || { cat "$pf"; fail "preflight active" "FAIL: $(echo $fails)"; }
[ "$(status_of "$pf" activation)" = PASS ] || fail "preflight active" "activation is '$(status_of "$pf" activation)'"
pass "active: activation file applied, utilizationShare active; preflight activation PASS"

# --- 9. negative control: a wrong prometheusURL -----------------------------

settings_patch '{"data":{"prometheusURL":"https://prometheus.invalid.svc.cluster.local:9090"}}'
run_preflight wrong-prometheus
pf="$LOGS/preflight-wrong-prometheus.log"
[ "$(cat "$LOGS/preflight-wrong-prometheus.end")" = Failed ] || fail "negative control" "the Job did not fail with a wrong prometheusURL"
[ "$(status_of "$pf" prometheus.reachable)" = FAIL ] || fail "negative control" "prometheus.reachable is '$(status_of "$pf" prometheus.reachable)'"
pass "negative control: a wrong prometheusURL fails the Job, naming prometheus.reachable: $(grep -m1 -E '^FAIL +prometheus.reachable' "$pf" | cut -c1-140)"
settings_patch "{\"data\":{\"prometheusURL\":\"$PROMETHEUS_URL\"}}"

if [ "$KEEP_INSTALL" = true ]; then
    notrun uninstall "KEEP_INSTALL=true"
    echo "=== summary ==="; cat "$SUMMARY"
    exit 0
fi

# --- 10. uninstall -----------------------------------------------------------

settings_patch '{"data":{"utilizationShare":"shadow"}}'
wait_settings_read || fail uninstall "settings not read"
wait_for 120 "status utilizationShare=shadow" status_is utilizationShare shadow || fail uninstall "utilizationShare is '$(status utilizationShare)'"
{
    kubectl delete -f "$MAIN" --ignore-not-found --wait=true
    kubectl delete -f scaling-manager-model-namespace.yaml --ignore-not-found
    kubectl delete -f scaling-manager-alerts.yaml --ignore-not-found
    kubectl delete -f scaling-manager-activate.yaml --ignore-not-found
    kubectl delete -f scaling-manager-keda-auth.yaml --ignore-not-found
    kubectl delete secret "scaling-manager-external-scaler-$NS" -n "$KEDA_NS" --ignore-not-found
    kubectl delete -f "$PREFLIGHT" --ignore-not-found
    kubectl delete configmap scaling-manager-preflight-result -n "$NS" --ignore-not-found
    kubectl delete secret scaling-manager-prometheus-ca -n "$NS" --ignore-not-found
    kubectl delete configmap,secret -n "$NS" -l 'app.kubernetes.io/part-of=scaling-manager,scaling-manager.llmd.ai/scaler-tls!=ca'
} > "$LOGS/uninstall.log" 2>&1 || { cat "$LOGS/uninstall.log"; fail uninstall "a delete failed"; }

# The smoke suite grants pod writes with a binding named the installer's way,
# scaling-manager-pod-writer-<sha256(namespace)[:8]> (test/e2e/fixtures/pod_writer.go).
# It is the test's, not the install's: remove it before looking for what the
# install left.
NAMESPACES="$NS"
[ "$MODEL_NS" = "$NS" ] || NAMESPACES="$NS $MODEL_NS"
for n in $NAMESPACES; do
    for b in $(kubectl get rolebindings -n "$n" -o name 2>/dev/null | grep -E '/scaling-manager-pod-writer-[0-9a-f]{8}$'); do
        log "removing the smoke fixture's $b in $n"
        kubectl delete -n "$n" "$b" --ignore-not-found >/dev/null
    done
done

# What is left of ours: every listable kind, by the part-of label, plus
# anything named for this install without it.
left=""
for r in $(kubectl api-resources --verbs=list -o name 2>/dev/null | grep -v -E '^(events|events.events.k8s.io)$'); do
    o="$(kubectl get "$r" -A -l app.kubernetes.io/part-of=scaling-manager -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}' 2>/dev/null | sed "s|^|$r |")"
    [ -z "$o" ] || left="$left$o"$'\n'
done
for r in clusterroles clusterrolebindings; do
    o="$(kubectl get "$r" -o name 2>/dev/null | grep -E "scaling-manager.*-$NS\$" | sed "s|^|unlabelled |")"
    [ -z "$o" ] || left="$left$o"$'\n'
done
for n in $NAMESPACES; do
    o="$(kubectl get configmaps,secrets,roles,rolebindings,serviceaccounts,services,deployments -n "$n" -o name 2>/dev/null | grep scaling-manager | sed "s|^|unlabelled |")"
    [ "$n" = "$NS" ] || o="$(printf '%s' "$o" | sed "s|^unlabelled |unlabelled $n/|")"
    [ -z "$o" ] || left="$left$o"$'\n'
done
left="$(printf '%s' "$left" | grep . | sort -u)"
printf '%s\n' "$left" > "$LOGS/left-after-uninstall.txt"
expected="secrets $NS/scaling-manager-external-scaler-ca"
unexpected="$(printf '%s\n' "$left" | grep -v -x -F "$expected" | grep -v -x -F "unlabelled secret/scaling-manager-external-scaler-ca" | grep . || true)"
[ -z "$unexpected" ] || fail uninstall "left behind: $(echo $unexpected)"
printf '%s\n' "$left" | grep -q -x -F "$expected" || fail uninstall "the scaler CA was not kept (the docs say it is)"
pass "uninstall: only the scaler CA Secret is left, as the docs say"
kubectl delete secret scaling-manager-external-scaler-ca -n "$NS" --ignore-not-found >/dev/null
# The guide keeps scaling-manager-system for a reinstall; this run made it.
[ "$SCOPE" = namespace ] || kubectl delete namespace "$NS" --ignore-not-found --wait=false >/dev/null

echo "=== summary ==="
cat "$SUMMARY"
echo "Logs: $LOGS"
