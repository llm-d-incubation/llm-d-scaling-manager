#!/usr/bin/env bash
#
# Executes llmsm_check_managed_namespaces (deploy/lib/infra_monitoring.sh) against
# a stubbed kubectl: the cluster-scoped preflight's per-namespace checks.
#
# A cluster-scoped `make check-prereqs` used to run the model-server and EPP
# checks once, against the controller's own namespace, which holds no model
# servers -- so it passed having checked nothing. It now finds every namespace
# with llm-d model servers or an InferencePool and runs the same checks in each.
# These cases pin what it concludes: which namespaces it finds, that each is
# checked with LLMSM_WATCH_NS set to it, the grouping, the per-namespace and
# overall summary, and that a namespace it may not read is "could not check",
# never passed.
#
# The two per-namespace checks are stubbed here: they are the namespace-scoped
# preflight's own, and this file is about running them per namespace. The
# discovery (llmsm_namespaces_with_model_servers) runs for real against the stub.

set -u

# CHECK_ROOT runs the check against another tree -- how its negative control is
# taken: point it at an extracted older deploy/ and the cases must fail.
ROOT="${CHECK_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
FAIL=0
PASS=0
# Every case reports ok or FAIL exactly once; a run that stops early must not
# pass having checked less. Change this deliberately with the cases.
EXPECT_CASES=14

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
# Sourced through CR-stripped copies: a Windows checkout writes these as CRLF.
for lib in common.sh scaledobject.sh infra_monitoring.sh install_core.sh; do
    tr -d '\r' < "$ROOT/deploy/lib/$lib" > "$WORK/$lib"
done

for tool in jq; do
    command -v "$tool" >/dev/null 2>&1 || { printf 'FATAL: %s is required.\n' "$tool" >&2; exit 1; }
done

BLUE='' GREEN='' YELLOW='' RED='' NC=''
LLMSM_NS=scaling-manager-system
MONITORING_NAMESPACE=scaling-manager-monitoring
# shellcheck source=/dev/null
for lib in common.sh scaledobject.sh infra_monitoring.sh; do . "$WORK/$lib"; done

ok()   { PASS=$((PASS + 1)); echo "ok   $1"; }
fail() { FAIL=$((FAIL + 1)); echo "FAIL $1"; [ -n "${2:-}" ] && printf '%s\n' "$2" | sed 's/^/       /'; }

# --- the stub -----------------------------------------------------------------
#
#   ALL_LIST     yes | no          may list workloads cluster-wide
#   MS_NS        namespaces whose Deployments carry llm-d.ai/role=decode
#   POOL_NS      namespaces with an InferencePool
#   FORBID_NS    namespaces whose Pods and ConfigMaps may not be read
#   FLOW_OFF     namespaces whose EPP has the flowControl gate off
#   UNSCRAPED_NS namespaces whose EPP nothing scrapes
#   UNSURE_NS    namespaces whose EPP scrape cannot be confirmed
CALLS="$WORK/calls.log"
kubectl() {
    case "$*" in
        "auth can-i list deployments -A")
            [ "${ALL_LIST:-yes}" = yes ] && { echo yes; return 0; }
            echo no; return 1 ;;
        "auth can-i list pods -n "*|"auth can-i get configmaps -n "*)
            local args="$*" ns
            ns="${args##* }"
            case " ${FORBID_NS:-} " in *" $ns "*) echo no; return 1 ;; esac
            echo yes; return 0 ;;
        "get deployments -A -o json")
            local ns items=""
            for ns in ${MS_NS:-}; do
                items="${items:+$items,}{\"metadata\":{\"name\":\"m\",\"namespace\":\"$ns\"},\"spec\":{\"template\":{\"metadata\":{\"labels\":{\"llm-d.ai/role\":\"decode\"}}}}}"
            done
            echo "{\"items\":[${items}]}"; return 0 ;;
        "get leaderworkersets -A -o json")
            echo 'error: the server does not have a resource type "leaderworkersets"' >&2; return 1 ;;
        "get inferencepools.inference.networking.k8s.io -A -o jsonpath="*)
            local ns
            for ns in ${POOL_NS:-}; do echo "$ns"; done; return 0 ;;
        "get inferencepools.inference.networking.x-k8s.io -A -o jsonpath="*)
            echo 'error: the server does not have a resource type' >&2; return 1 ;;
    esac
    return 0
}

# The per-namespace checks, standing in for the real ones with their real lines.
llmsm_report_modelserver_metrics() {
    echo "WATCH modelserver ${LLMSM_WATCH_NS:-}" >> "$CALLS"
    log_success "  Model-server metrics: podmonitor/m already scrapes them."
    return 0
}
llmsm_report_epp_flowcontrol() {
    local ns="${LLMSM_WATCH_NS:-}"
    echo "WATCH epp $ns" >> "$CALLS"
    case " ${UNSURE_NS:-} " in *" $ns "*)
        log_warning "  EPP metrics: cannot tell whether epp is scraped — podmonitor/x select by matchExpressions, which this check does not evaluate."
        return 0 ;;
    esac
    case " ${FLOW_OFF:-} " in *" $ns "*)
        log_success "  EPP metrics: epp is scraped by servicemonitor/epp."
        log_warning "  EPP flow control: off on epp. Not refused: scaling works without it. Two things do not:"
        return 0 ;;
    esac
    case " ${UNSCRAPED_NS:-} " in *" $ns "*)
        log_warning "  Router monitoring not enabled; please enable EPP metrics."
        return 1 ;;
    esac
    log_success "  EPP metrics: epp is scraped by servicemonitor/epp."
    log_success "  EPP flow control: enabled on epp (its config declares featureGates: [flowControl])."
    return 0
}

# run_case leaves rc and out.
run_case() {
    : > "$CALLS"
    out="$( (llmsm_check_managed_namespaces) 2>&1 )"; rc=$?
}

if ! declare -F llmsm_check_managed_namespaces >/dev/null; then
    fail "llmsm_check_managed_namespaces is not defined -- the cluster-scoped preflight checks no namespace"
fi

# 1. Found, not named: model servers in team-a, only an InferencePool in team-b.
ALL_LIST=yes MS_NS="team-a" POOL_NS="team-a team-b" FORBID_NS="" FLOW_OFF="" UNSURE_NS="" UNSCRAPED_NS=""
run_case
if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q 'Checking the 2 namespace(s)' \
    && printf '%s' "$out" | grep -q 'Namespaces: all 2 passed'; then
    ok "finds team-a (model servers) and team-b (InferencePool only), and both pass"
else
    fail "both namespaces must be found and pass (rc=$rc)" "$out"
fi

# 2. Each namespace is checked with LLMSM_WATCH_NS set to it, by both checks.
if [ "$(grep -c '^WATCH ' "$CALLS")" -eq 4 ] && grep -qx 'WATCH epp team-b' "$CALLS" \
    && grep -qx 'WATCH modelserver team-a' "$CALLS" && ! grep -q "WATCH .* $LLMSM_NS\$" "$CALLS"; then
    ok "both checks run once per namespace, with LLMSM_WATCH_NS set to it, never the controller's"
else
    fail "each check must run once per found namespace" "$(cat "$CALLS")"
fi

# 3. Grouped: team-a's lines sit between its header and team-b's.
grouped="$(printf '%s\n' "$out" | awk '/Namespace team-a$/{a=1;next} /Namespace team-b$/{a=0} a' | grep -c 'EPP flow control: enabled')"
if [ "$grouped" -eq 1 ]; then
    ok "results are grouped under each namespace's header"
else
    fail "team-a's results must appear under its header (found $grouped)" "$out"
fi

# 4. One failing namespace fails the whole check, and the summary names why.
UNSCRAPED_NS="team-b"
run_case
if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q 'team-b: failed (EPP not scraped)' \
    && printf '%s' "$out" | grep -q 'team-a: passed' \
    && printf '%s' "$out" | grep -q 'Namespaces: 1 passed, 1 failed, 0 could not be checked'; then
    ok "a namespace whose EPP nothing scrapes fails, by name and reason; the other still passes"
else
    fail "an unscraped EPP in team-b must fail the check and say so (rc=$rc)" "$out"
fi
UNSCRAPED_NS=""

# 4b. Flow control off is a note on a pass, never a failure: llm-d ships it off,
# and only scale-from-zero needs it.
FLOW_OFF="team-b"
run_case
if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q 'team-b: passed (EPP flow control off: no scale from zero)' \
    && printf '%s' "$out" | grep -q 'Namespaces: all 2 passed'; then
    ok "flow control off passes the namespace, and the summary says what it costs"
else
    fail "flow control off in team-b must pass with a note (rc=$rc)" "$out"
fi
FLOW_OFF=""

# 5. A namespace it may not read is "could not check", never passed.
FORBID_NS="team-b"
run_case
if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q 'team-b: could not check (reading it is not permitted for you)' \
    && ! printf '%s' "$out" | grep -q 'team-b: passed' \
    && printf '%s' "$out" | grep -q 'Those are not passes'; then
    ok "a Forbidden namespace is 'could not check', not passed, and not a failure"
else
    fail "a Forbidden namespace must be 'could not check' (rc=$rc)" "$out"
fi

# 6. ...and is not checked at all.
if ! grep -q 'team-b' "$CALLS" && grep -q 'team-a' "$CALLS"; then
    ok "a Forbidden namespace is not run through the checks"
else
    fail "the checks must not run in a namespace that cannot be read" "$(cat "$CALLS")"
fi
FORBID_NS=""

# 7. A signal the checks could not confirm is not a pass either.
UNSURE_NS="team-a"
run_case
if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q 'team-a: could not check (some signals could not be confirmed)' \
    && ! printf '%s' "$out" | grep -q 'team-a: passed'; then
    ok "an unconfirmed signal makes the namespace 'could not check', not passed"
else
    fail "an unconfirmed signal must not pass (rc=$rc)" "$out"
fi
UNSURE_NS=""

# 8. Not allowed to list workloads cluster-wide: could not check any.
ALL_LIST=no
run_case
if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q 'could not check any' \
    && printf '%s' "$out" | grep -q 'This is not a pass' && [ ! -s "$CALLS" ]; then
    ok "no cluster-wide list: 'could not check any', never a pass"
else
    fail "a Forbidden cluster-wide list must say it could not check (rc=$rc)" "$out"
fi
ALL_LIST=yes

# 9. Nothing on the cluster: said, not passed.
MS_NS="" POOL_NS=""
run_case
if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q 'nothing to scale yet' \
    && ! printf '%s' "$out" | grep -q 'passed'; then
    ok "no model servers and no InferencePool: says so, passes nothing"
else
    fail "an empty cluster must be reported as nothing to scale (rc=$rc)" "$out"
fi

# 10. The summary has one line per namespace and one overall line.
MS_NS="team-a team-c" POOL_NS="team-a team-b"
run_case
summary="$(printf '%s\n' "$out" | sed -n '/Namespace summary:/,$p')"
if [ "$(printf '%s\n' "$summary" | grep -cE '  team-[abc]: ')" -eq 3 ] \
    && printf '%s' "$summary" | grep -q 'Namespaces: all 3 passed'; then
    ok "the summary has one line per namespace (3) and an overall line"
else
    fail "the summary must list each of the 3 namespaces once" "$out"
fi

# 11. Wired in: the cluster-scoped preflight calls it, and refuses on a failure.
core="$WORK/install_core.sh"
if grep -q 'llmsm_check_managed_namespaces' "$core" \
    && awk '/CHECK_ONLY:-false/{c=1} c && /llmsm_check_managed_namespaces/{print; exit}' "$core" | grep -q 'SKIP_CHECKS'; then
    ok "the --check path runs it at cluster scope, and refuses unless SKIP_CHECKS=true"
else
    fail "install_core.sh's --check path must call llmsm_check_managed_namespaces at cluster scope"
fi

# 11b. The install path runs it too, so a passing check and a refusing install
# cannot disagree about a namespace.
if awk '/exempt: the e2e path builds its own stack/{d=1} d && /llmsm_check_managed_namespaces/{f=1} f && /log_error/{print; exit}' "$core" | grep -q 'fix the namespaces above'; then
    ok "the install path runs it at cluster scope, and refuses on a failed namespace"
else
    fail "install_core.sh's install path must call llmsm_check_managed_namespaces at cluster scope"
fi

# 12. The reasons come from the lines the checks print.
r="$(llmsm_ns_check_reasons '  Monitoring not enabled for your llm-d servers; please enable Model-server metrics.
  Router monitoring not enabled; please enable EPP metrics.' 2>/dev/null)"
if [ "$r" = "model servers not scraped, EPP not scraped" ]; then
    ok "failure reasons are read from the checks' own lines"
else
    fail "llmsm_ns_check_reasons must name both missing signals (got '$r')"
fi

total=$((PASS + FAIL))
echo "check-cluster-namespace-checks: $PASS passed, $FAIL failed"
if [ "$total" -ne "$EXPECT_CASES" ]; then
    echo "check-cluster-namespace-checks: $total case(s) ran, $EXPECT_CASES expected -- the run did not finish, or a case was added without updating EXPECT_CASES"
    exit 1
fi
[ "$FAIL" -eq 0 ]
