#!/usr/bin/env bash
#
# Executes the llm-d monitor handling in deploy/lib/infra_monitoring.sh against
# a stubbed kubectl:
#
#   - setup adds the model-server PodMonitor and the EPP ServiceMonitor a fresh
#     llm-d install lacks, only where nothing scrapes the target yet, marked so
#     undeploy removes exactly those (llmsm_ensure_llmd_monitors);
#   - the checks report a monitor setup will add as work, not as a failure,
#     when the run includes the admin phase (LLMSM_MONITORS_PLANNED);
#   - an EPP with the flowControl gate off is a warning, never a refusal;
#   - the model-server PodMonitor names a container port, and when no model
#     server declares it the install warns instead of reporting it in place.
#
# A fresh llm-d install has no monitors and no flow-control gate, and the
# preflight used to refuse it and hand the reader commands to run in every
# namespace. Every case asserts something PRESENT, so a missing library fails.

set -u

# CHECK_ROOT runs the check against another tree -- how its negative control is
# taken: point it at an extracted older deploy/ and the cases must fail.
ROOT="${CHECK_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
FAIL=0
PASS=0
# Every case reports ok or FAIL exactly once; a run that stops early must not
# pass having checked less. Change this deliberately with the cases.
EXPECT_CASES=19

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
# Sourced through CR-stripped copies: a Windows checkout writes these as CRLF.
for lib in common.sh scaledobject.sh infra_monitoring.sh install_core.sh cleanup.sh; do
    tr -d '\r' < "$ROOT/deploy/lib/$lib" > "$WORK/$lib" 2>/dev/null || : > "$WORK/$lib"
done
mkdir -p "$WORK/config/modelserver-metrics"
tr -d '\r' < "$ROOT/config/modelserver-metrics/modelserver-podmonitor.yaml" \
    > "$WORK/config/modelserver-metrics/modelserver-podmonitor.yaml"

for tool in jq yq; do
    command -v "$tool" >/dev/null 2>&1 || { printf 'FATAL: %s is required.\n' "$tool" >&2; exit 1; }
done

BLUE='' GREEN='' YELLOW='' RED='' NC=''
LLMSM_NS=scaling-manager-system
LLMSM_PROJECT="$WORK"
ENVIRONMENT=kubernetes
# shellcheck source=/dev/null
for lib in common.sh scaledobject.sh infra_monitoring.sh; do . "$WORK/$lib"; done

ok()   { PASS=$((PASS + 1)); echo "ok   $1"; }
fail() { FAIL=$((FAIL + 1)); echo "FAIL $1"; [ -n "${2:-}" ] && printf '%s\n' "$2" | sed 's/^/       /'; }

# --- the stub -----------------------------------------------------------------
#
#   NO_CRDS        yes: the Prometheus Operator CRDs are absent
#   PROM_JSON      the Prometheus CR listing
#   APPLIED        every document handed to `kubectl apply -f`
#   CALLS          every kubectl command line
#   OWN_PM         yes: the scaling manager's own model-server PodMonitor exists
#   PODS_JSON      the model-server pods the PodMonitor selects; PODS_FORBIDDEN
#                  yes: listing them is refused
APPLIED="$WORK/applied.yaml"
CALLS="$WORK/calls.log"
NO_CRDS=no OWN_PM=no PODS_FORBIDDEN=no PODS_JSON='{"items":[]}'
PROM_JSON='{"items":[{"spec":{"podMonitorSelector":{"matchLabels":{"release":"prom"}},"serviceMonitorSelector":{"matchLabels":{"release":"prom"}}}}]}'
SVC_JSON='{"metadata":{"labels":{"app.kubernetes.io/name":"optimized-baseline-epp","helm.sh/chart":"x-1.0"}},"spec":{"ports":[{"name":"http","port":80},{"name":"http-metrics","port":9090}]}}'
kubectl() {
    echo "kubectl $*" >> "$CALLS"
    case "$*" in
        "get crd "*)
            [ "$NO_CRDS" = yes ] && return 1
            return 0 ;;
        "get prometheuses.monitoring.coreos.com -A -o json")
            printf '%s' "$PROM_JSON"; return 0 ;;
        "get svc "*" -o json")
            printf '%s' "$SVC_JSON"; return 0 ;;
        "get podmonitor -n "*)
            [ "$OWN_PM" = yes ] && echo "scaling-manager-modelserver-metrics 1d"
            return 0 ;;
        "get pods -n team-a -l llm-d.ai/role in (decode,prefill) -o json")
            if [ "$PODS_FORBIDDEN" = yes ]; then
                echo 'Error from server (Forbidden): pods is forbidden: User "u" cannot list resource "pods"' >&2
                return 1
            fi
            printf '%s' "$PODS_JSON"; return 0 ;;
        "apply -f -"*)
            cat >> "$APPLIED"; echo "---" >> "$APPLIED"; return 0 ;;
        "apply -f "*)
            cat "$3" >> "$APPLIED"; echo "---" >> "$APPLIED"; return 0 ;;
        "auth can-i "*)
            echo yes; return 0 ;;
    esac
    return 0
}

# The real llmsm_epp_scrapers, kept before it is stubbed below, for case 15.
eval "real_scaling_manager_epp_scrapers() $(declare -f llmsm_epp_scrapers | tail -n +2)"

# The discovery and conflict tests are the checks' own and have their own
# fixtures; here they are set per case.
SERVERS=1 MS_CONFLICT="" EPP_SVCS="optimized-baseline-epp" EPP_SCRAPERS="" FLOW_STATE=on
llmsm_serving_workload_count() { echo "$SERVERS"; }
llmsm_modelserver_scrape_conflict() { [ -z "$MS_CONFLICT" ] || echo "$MS_CONFLICT"; }
llmsm_epp_services() { [ -z "$EPP_SVCS" ] || echo "$EPP_SVCS"; }
llmsm_epp_scrapers() { [ -z "$EPP_SCRAPERS" ] || echo "$EPP_SCRAPERS"; }
llmsm_epp_flowcontrol_state() { echo "$FLOW_STATE"; }

reset() {
    : > "$APPLIED"; : > "$CALLS"
    NO_CRDS=no SERVERS=1 MS_CONFLICT="" EPP_SVCS="optimized-baseline-epp" EPP_SCRAPERS="" FLOW_STATE=on
    OWN_PM=no PODS_FORBIDDEN=no PODS_JSON='{"items":[]}'
    LLMSM_LLMD_MONITORS_ADDED=()
    LLMSM_MONITORS_PLANNED=false
}

# kind_doc echoes the applied document of kind $1.
kind_doc() { yq "select(.kind == \"$1\")" "$APPLIED" 2>/dev/null; }

if ! declare -F llmsm_ensure_llmd_monitors >/dev/null; then
    fail "llmsm_ensure_llmd_monitors is not defined -- setup adds no monitors"
fi

# 1. A fresh llm-d namespace: both monitors are added.
reset
out="$(llmsm_ensure_llmd_monitors team-a 2>&1)"
if [ "$(kind_doc PodMonitor | yq '.metadata.name')" = "scaling-manager-modelserver-metrics" ] \
    && [ "$(kind_doc ServiceMonitor | yq '.metadata.name')" = "scaling-manager-epp-metrics" ] \
    && printf '%s' "$out" | grep -q 'team-a: added PodMonitor scaling-manager-modelserver-metrics, ServiceMonitor scaling-manager-epp-metrics'; then
    ok "a namespace with nothing scraped gets the PodMonitor and the EPP ServiceMonitor"
else
    fail "both monitors must be applied in a fresh namespace" "$out
$(cat "$APPLIED")"
fi

# 2. Every object it applies is marked, in the namespace, and labelled for the
# cluster's Prometheus (release=prom), or that Prometheus never scrapes it.
marks="$(yq -N 'select(.kind != null) | .metadata.labels["llm-d.ai/applied-by"]' "$APPLIED" | sort -u)"
nss="$(yq -N 'select(.kind != null and .kind != "ClusterRole" and .kind != "ClusterRoleBinding") | .metadata.namespace' "$APPLIED" | sort -u)"
if [ "$marks" = "scaling-manager-setup-prereqs" ] && [ "$nss" = "team-a" ] \
    && [ "$(kind_doc PodMonitor | yq '.metadata.labels.release')" = "prom" ] \
    && [ "$(kind_doc ServiceMonitor | yq '.metadata.labels.release')" = "prom" ]; then
    ok "every applied object is marked, namespaced to team-a, and carries the Prometheus selector label"
else
    fail "applied objects must be marked and labelled (marks='$marks' nss='$nss')" "$(cat "$APPLIED")"
fi

# 3. The EPP ServiceMonitor scrapes the metrics port with a token, and selects
# the Service by its name label, never the chart version.
sm="$(kind_doc ServiceMonitor)"
if [ "$(printf '%s' "$sm" | yq '.spec.endpoints[0].port')" = "http-metrics" ] \
    && [ "$(printf '%s' "$sm" | yq '.spec.endpoints[0].authorization.credentials.name')" = "scaling-manager-epp-metrics-scraper-token" ] \
    && [ "$(printf '%s' "$sm" | yq '.spec.selector.matchLabels | keys | join(",")')" = "app.kubernetes.io/name" ]; then
    ok "the EPP ServiceMonitor: port http-metrics, a bearer token, selected by name"
else
    fail "the EPP ServiceMonitor is wrong" "$sm"
fi

# 4. ...and the token's ServiceAccount may GET /metrics, which the EPP checks.
crb="$(kind_doc ClusterRoleBinding)"
if [ "$(printf '%s' "$crb" | yq '.subjects[0].namespace')" = "team-a" ] \
    && [ "$(printf '%s' "$crb" | yq '.roleRef.name')" = "scaling-manager-epp-metrics-scraper" ] \
    && [ "$(kind_doc ClusterRole | yq '.rules[0].nonResourceURLs[0]')" = "/metrics" ] \
    && [ "$(kind_doc Secret | yq '.metadata.annotations["kubernetes.io/service-account.name"]')" = "scaling-manager-epp-metrics-scraper" ]; then
    ok "the scrape identity: a token Secret, bound to GET /metrics, per namespace"
else
    fail "the scrape identity is wrong" "$(cat "$APPLIED")"
fi

# 5. Already scraped: nothing is added, so no target is scraped twice.
reset; MS_CONFLICT="podmonitor/decode"; EPP_SCRAPERS="servicemonitor/epp"
out="$(llmsm_ensure_llmd_monitors team-a 2>&1)"
if [ ! -s "$APPLIED" ] && grep -q 'get svc\|get crd' "$CALLS"; then
    ok "a namespace already scraped gets nothing added"
else
    fail "nothing may be applied over an existing scrape" "$out
$(cat "$APPLIED")"
fi

# 6. No Prometheus Operator: says so, applies nothing, does not fail.
reset; NO_CRDS=yes
out="$(llmsm_ensure_llmd_monitors team-a 2>&1)"; rc=$?
if [ "$rc" -eq 0 ] && [ ! -s "$APPLIED" ] && printf '%s' "$out" | grep -q 'no Prometheus Operator CRDs'; then
    ok "no Prometheus Operator CRDs: says so and applies nothing"
else
    fail "without the CRDs nothing is applied, and it says why (rc=$rc)" "$out"
fi

# 7. DEPLOY_LLMD_MONITORS=false turns it off.
reset
out="$(DEPLOY_LLMD_MONITORS=false llmsm_apply_llmd_monitors 2>&1)"
if [ ! -s "$APPLIED" ] && ! printf '%s' "$out" | grep -q 'Adding the monitors' \
    && declare -F llmsm_apply_llmd_monitors >/dev/null; then
    ok "DEPLOY_LLMD_MONITORS=false adds nothing"
else
    fail "DEPLOY_LLMD_MONITORS=false must add nothing" "$out"
fi

# 8. An EPP with flow control off is a warning: the report passes.
reset; EPP_SCRAPERS="servicemonitor/epp"; FLOW_STATE=off
out="$( (LLMSM_WATCH_NS=team-a llmsm_report_epp_flowcontrol) 2>&1)"; rc=$?
if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q 'EPP flow control: off on optimized-baseline-epp. Not refused' \
    && printf '%s' "$out" | grep -q 'never wakes'; then
    ok "flow control off: a warning that says what it costs, and the check passes"
else
    fail "flow control off must warn and pass (rc=$rc)" "$out"
fi

# 9. An EPP nothing scrapes still fails the check when no setup will add it.
reset; FLOW_STATE=on
out="$( (LLMSM_WATCH_NS=team-a llmsm_report_epp_flowcontrol) 2>&1)"; rc=$?
if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q 'Router monitoring not enabled' \
    && printf '%s' "$out" | grep -q 'make setup-prereqs'; then
    ok "an unscraped EPP fails without the admin phase, and names the step that fixes it"
else
    fail "an unscraped EPP must fail when nothing will add its monitor (rc=$rc)" "$out"
fi

# 10. ...and is work, not a failure, when the admin phase will add it.
reset; LLMSM_MONITORS_PLANNED=true
out="$( (LLMSM_WATCH_NS=team-a llmsm_report_epp_flowcontrol) 2>&1)"; rc=$?
if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q 'Setup adds ServiceMonitor scaling-manager-epp-metrics'; then
    ok "an unscraped EPP passes as 'setup adds' when the admin phase runs"
else
    fail "a planned EPP monitor must pass and say setup adds it (rc=$rc)" "$out"
fi

# 11. The same for the model servers.
reset; LLMSM_MONITORS_PLANNED=true
out="$( (LLMSM_WATCH_NS=team-a llmsm_report_modelserver_metrics) 2>&1)"; rc=$?
reset
out2="$( (LLMSM_WATCH_NS=team-a llmsm_report_modelserver_metrics) 2>&1)"; rc2=$?
if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q 'Setup adds PodMonitor scaling-manager-modelserver-metrics' \
    && [ "$rc2" -ne 0 ] && printf '%s' "$out2" | grep -q 'Monitoring not enabled for your llm-d servers'; then
    ok "unscraped model servers: 'setup adds' with the admin phase, a failure without"
else
    fail "the model-server report must follow LLMSM_MONITORS_PLANNED (rc=$rc rc2=$rc2)" "$out
$out2"
fi

# 12. Planned only when the run includes the admin phase.
. "$WORK/install_core.sh" 2>/dev/null
got=""
for ph in all prereqs scaling-manager; do
    INSTALL_PHASE=$ph llmsm_set_monitors_planned
    got="$got $ph=$LLMSM_MONITORS_PLANNED"
done
DEPLOY_LLMD_MONITORS=false INSTALL_PHASE=all llmsm_set_monitors_planned
got="$got off=$LLMSM_MONITORS_PLANNED"
if [ "$got" = " all=true prereqs=true scaling-manager=false off=false" ]; then
    ok "planned for phases all and prereqs, not scaling-manager, not with DEPLOY_LLMD_MONITORS=false"
else
    fail "llmsm_set_monitors_planned is wrong:$got"
fi

# 13. Wired in: the prereqs phase adds them after the monitoring stack.
if awk '/^llmsm_run_prereqs_phase\(\)/{f=1} f && /deploy_monitoring_stack/{m=NR} f && /llmsm_apply_llmd_monitors/{print (m && NR > m) ? "after" : "before"; exit}' "$WORK/install_core.sh" | grep -qx after; then
    ok "the prereqs phase runs llmsm_apply_llmd_monitors after deploy_monitoring_stack"
else
    fail "llmsm_run_prereqs_phase must call llmsm_apply_llmd_monitors after deploy_monitoring_stack"
fi

# 14. Undeploy removes by the mark, and cleanup calls it.
reset; LLMSM_SCOPE=cluster
. "$WORK/common.sh"
out="$(llmsm_remove_llmd_monitors 2>&1)"
if grep -q 'delete podmonitors.monitoring.coreos.com,servicemonitors.monitoring.coreos.com,secrets,serviceaccounts -A -l llm-d.ai/applied-by=scaling-manager-setup-prereqs' "$CALLS" \
    && grep -q 'delete clusterrolebindings,clusterroles -l llm-d.ai/applied-by=scaling-manager-setup-prereqs' "$CALLS" \
    && grep -q 'llmsm_remove_llmd_monitors' "$WORK/cleanup.sh"; then
    ok "undeploy removes exactly what setup marked, and cleanup calls it"
else
    fail "undeploy must remove the marked monitors" "$(cat "$CALLS")"
fi

# 16-19. The PodMonitor scrapes the container port named `modelserver`, and a
# model server whose port has another name yields no target at all. The
# install said "PodMonitor in place" and nothing else. Found walking the guide
# on kind with a hand-written model fixture.
HTTP_PODS='{"items":[{"metadata":{"name":"decode-0"},"spec":{"containers":[{"name":"vllm","ports":[{"name":"http","containerPort":8000}]},{"name":"proxy","ports":[{"containerPort":9000}]}]}},{"metadata":{"name":"decode-1"},"spec":{"containers":[{"name":"vllm"}]}}]}'
NAMED_PODS='{"items":[{"metadata":{"name":"decode-0"},"spec":{"containers":[{"name":"vllm","ports":[{"name":"http","containerPort":8000}]}],"initContainers":[{"name":"sidecar","restartPolicy":"Always","ports":[{"name":"modelserver","containerPort":8200}]}]}}]}'

# 16. Setup applies it and warns, naming each pod and its ports.
reset; PODS_JSON="$HTTP_PODS"
out="$(llmsm_ensure_llmd_monitors team-a 2>&1)"
if [ "$(kind_doc PodMonitor | yq '.metadata.name')" = "scaling-manager-modelserver-metrics" ] \
    && printf '%s' "$out" | grep -q "no model-server pod in team-a declares it" \
    && printf '%s' "$out" | grep -qF "decode-0 (http, unnamed 9000); decode-1 (no ports)"; then
    ok "setup applies the PodMonitor and warns when no model server declares its port, naming the pods"
else
    fail "setup must warn when no model server declares the scraped port" "$out"
fi

# 17. The report says in place only when a pod declares the port (a sidecar
# counts); otherwise it warns instead, and still passes.
reset; OWN_PM=yes; PODS_JSON="$NAMED_PODS"
out="$( (LLMSM_WATCH_NS=team-a llmsm_report_modelserver_metrics) 2>&1)"; rc=$?
reset; OWN_PM=yes; PODS_JSON="$HTTP_PODS"
out2="$( (LLMSM_WATCH_NS=team-a llmsm_report_modelserver_metrics) 2>&1)"; rc2=$?
if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q "PodMonitor is in place" && ! printf '%s' "$out" | grep -q WARNING \
    && [ "$rc2" -eq 0 ] && ! printf '%s' "$out2" | grep -q "PodMonitor is in place" \
    && printf '%s' "$out2" | grep -q "scrapes the container port named .modelserver., and no model-server pod in team-a declares it"; then
    ok "check-prereqs: in place when a pod declares the port, a warning naming the pods when none does"
else
    fail "the report must say in place only when a model server declares the port (rc=$rc rc2=$rc2)" "$out
$out2"
fi

# 18. A pod list that is refused is "could not check", never "no pods".
reset; OWN_PM=yes; PODS_FORBIDDEN=yes
out="$( (LLMSM_WATCH_NS=team-a llmsm_report_modelserver_metrics) 2>&1)"; rc=$?
if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q "could not check that a model server in team-a declares the port" \
    && printf '%s' "$out" | grep -q "Forbidden" && ! printf '%s' "$out" | grep -q "no model-server pod"; then
    ok "a Forbidden pod list is said as could not check, not as no pods"
else
    fail "a Forbidden pod list must read as could not check (rc=$rc)" "$out"
fi

# 19. No model-server pod yet (scaled to zero): said, and not a warning.
reset; OWN_PM=yes
out="$( (LLMSM_WATCH_NS=team-a llmsm_report_modelserver_metrics) 2>&1)"; rc=$?
if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q "no model-server pod runs in team-a yet" \
    && ! printf '%s' "$out" | grep -q WARNING; then
    ok "no model-server pod yet: says the port was not checked, without a warning"
else
    fail "with no model-server pod the report must say the port was not checked (rc=$rc)" "$out"
fi

# 15. The PodMonitor setup adds for the model servers is not read as a monitor
# that may scrape the EPP. It selects by matchExpressions, and counted, it made
# every namespace setup had just served read "cannot tell", and the EPP got no
# ServiceMonitor of its own. Found walking the guide on kind.
kubectl() {
    case "$*" in
        "get svc optimized-baseline-epp -n team-a -o json") printf '%s' "$SVC_JSON" ;;
        "get pods -n team-a -l "*) echo '{"items":[{"metadata":{"labels":{"app":"epp"}}}]}' ;;
        "get servicemonitors -n team-a -o json") echo '{"items":[]}' ;;
        "get podmonitors -n team-a -o json")
            echo '{"items":[{"metadata":{"name":"scaling-manager-modelserver-metrics","labels":{"app.kubernetes.io/component":"modelserver-metrics"}},"spec":{"selector":{"matchExpressions":[{"key":"llm-d.ai/role","operator":"In","values":["decode","prefill"]}]}}},
                            {"metadata":{"name":"other"},"spec":{"selector":{"matchExpressions":[{"key":"app","operator":"Exists"}]}}}]}' ;;
    esac
    return 0
}
got="$(real_scaling_manager_epp_scrapers team-a optimized-baseline-epp 2>&1)"
if [ "$got" = "unevaluated:podmonitor/other" ]; then
    ok "the model-server PodMonitor setup adds is not counted as a possible EPP scrape; another one still is"
else
    fail "only the foreign matchExpressions PodMonitor may be reported (got '$got')"
fi

total=$((PASS + FAIL))
echo "check-llmd-monitors: $PASS passed, $FAIL failed"
if [ "$total" -ne "$EXPECT_CASES" ]; then
    echo "check-llmd-monitors: $total case(s) ran, $EXPECT_CASES expected -- the run did not finish, or a case was added without updating EXPECT_CASES"
    exit 1
fi
[ "$FAIL" -eq 0 ]
