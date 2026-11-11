#!/usr/bin/env bash
#
# Executes the controller readiness check -- llmsm_controller_rollout_ready, and
# verify_deployment around it -- against a stub kubectl.
#
# `kubectl rollout status` says "successfully rolled out" at once for a
# Deployment scaled to 0, so an install over a controller at 0/0 printed "the scaling manager
# controller is running and ready" and exited 0. That parses perfectly and
# passes on every cluster where the controller is up; only a run against a
# Deployment at 0 replicas shows it.
#
# The stub answers the Deployment lookup with one controller, `rollout status`
# with exit $STUB_ROLLOUT, and the replica read with "$STUB_COUNTS"
# ("<spec.replicas>/<status.readyReplicas>", fields omitted as the API omits
# them); $STUB_COUNTS=FAIL makes that read fail. Every other call succeeds
# with no output, which verify_deployment reads as nothing else to report.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
FAILED=0
RAN=0
EXPECT_CASES=23

for lib in constants.sh common.sh infra_scaling_manager.sh infra_monitoring.sh verify.sh; do
    tr -d '\r' < "$ROOT/deploy/lib/$lib" > "$TMP/$lib"
done
BLUE='' GREEN='' YELLOW='' RED='' NC=''
for lib in constants.sh common.sh infra_scaling_manager.sh infra_monitoring.sh verify.sh; do
    # shellcheck source=/dev/null
    source "$TMP/$lib"
done
for fn in llmsm_controller_rollout_ready verify_deployment llmsm_append_replicas_patch; do
    declare -F "$fn" >/dev/null || {
        printf 'FATAL: %s is not defined after sourcing the library.\n' "$fn" >&2
        exit 1
    }
done
# Outside the scaling manager section verify_deployment asks Prometheus and the EPP; both
# are answered here so the stub needs to know only the Deployment.
llmsm_prometheus_endpoint() { echo "https://prom.example:9090"; }
llmsm_report_epp_flowcontrol() { return 0; }

mkdir -p "$TMP/bin"
cat > "$TMP/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
case "$*" in
    "rollout status"*)
        [ "$STUB_ROLLOUT" = 0 ] && echo 'deployment "scaling-manager-controller-manager" successfully rolled out'
        exit "$STUB_ROLLOUT"
        ;;
    "get deployment -n "*" -o name")
        echo "deployment.apps/scaling-manager-controller-manager"
        ;;
    "get deployment scaling-manager-controller-manager -n "*)
        printf '%s' "${STUB_LIVE:-}"
        ;;
    "get deployment.apps/scaling-manager-controller-manager "*)
        [ "$STUB_COUNTS" = FAIL ] && exit 1
        printf '%s' "$STUB_COUNTS"
        ;;
esac
exit 0
STUB
chmod +x "$TMP/bin/kubectl"
PATH="$TMP/bin:$PATH"

check() {
    if [ "$2" = "$3" ]; then
        RAN=$((RAN + 1))
        printf 'ok    %s\n' "$1"
    else
        FAILED=$((FAILED + 1))
        printf 'FAIL  %s\n        want [%s]\n        got  [%s]\n' "$1" "$2" "$3" >&2
    fi
}

# ready <rollout rc> <counts> echoes the helper's status and reason.
ready() {
    local rc=0
    STUB_ROLLOUT="$1" STUB_COUNTS="$2" \
        llmsm_controller_rollout_ready deployment.apps/scaling-manager-controller-manager wns 1s >/dev/null 2>&1 || rc=$?
    echo "$rc ${LLMSM_CONTROLLER_NOT_READY:-}"
}
export STUB_ROLLOUT STUB_COUNTS STUB_LIVE

check 'ready: one replica, one Ready'                     '0 '                   "$(ready 0 '1/1')"
check 'ready: two replicas, one Ready (a standby)'        '0 '                   "$(ready 0 '2/1')"
check 'ready: replicas unset (API default 1), one Ready'  '0 '                   "$(ready 0 '/1')"
check 'not ready: scaled to 0, rollout passes'            '1 scaled-to-zero'     "$(ready 0 '0/')"
check 'not ready: rollout passes with none Ready'         '1 no-ready-replica'   "$(ready 0 '1/')"
check 'not ready: readyReplicas 0 written out'            '1 no-ready-replica'   "$(ready 0 '1/0')"
check 'not ready: the rollout fails'                      '1 rollout'            "$(ready 1 '1/1')"
check 'not ready: the Deployment cannot be read after'    '1 unreadable'         "$(ready 0 FAIL)"

# verify <rollout rc> <counts> [<LLMSM_REPLICAS>] runs verify_deployment and
# echoes its status and whether it claimed the controller ready.
verify() {
    local out rc=0
    out="$(STUB_ROLLOUT="$1" STUB_COUNTS="$2" LLMSM_REPLICAS="${3:-1}" LLMSM_NS=wns LLMSM_SCOPE=cluster LLMSM_WATCH_NS='' \
        SCALER_BACKEND=none DEPLOY_PROMETHEUS=false DEPLOY_OPERATIONAL_DASHBOARD=false \
        LLMSM_VERIFY_TIMEOUT=1s verify_deployment 2>&1)" || rc=$?
    printf '%s %s %s\n' "$rc" \
        "$(grep -q 'the scaling manager controller is running and ready' <<<"$out" && echo claimed-ready || echo not-claimed)" \
        "$(grep -q 'scaled to 0 replicas' <<<"$out" && echo says-zero || echo silent)"
}

check 'verify: a Ready controller passes'                  '0 claimed-ready silent'   "$(verify 0 '1/1')"
check 'verify: 0/0 replicas fails, and says it is at 0'    '1 not-claimed says-zero'  "$(verify 0 '0/')"
check 'verify: a rollout with none Ready fails'            '1 not-claimed silent'     "$(verify 0 '1/')"
check 'verify: a failed rollout fails'                     '1 not-claimed silent'     "$(verify 1 '1/1')"
# Asked for: LLMSM_REPLICAS=0 is a staged install, warned about and not failed.
check 'verify: 0/0 with LLMSM_REPLICAS=0 passes, and says it is at 0' '0 not-claimed says-zero' "$(verify 0 '0/' 0)"
check 'verify: LLMSM_REPLICAS=0 does not excuse a rollout with none Ready' '1 not-claimed silent' "$(verify 0 '1/' 0)"

# verify_keda <trust rc> runs verify_deployment with KEDA as the scaler backend
# and a Ready controller, the scaler checks answered here except
# llmsm_verify_scaler_keda_trust, which returns <trust rc>, and echoes the
# status. KEDA's copy of the certificates not matching the scaler means no
# KEDA call is ever served: that must fail the verify, and so the install.
verify_keda() {
    local rc=0
    (
        llmsm_verify_scaler_networkpolicy() { :; }
        llmsm_verify_scaler_tls() { :; }
        llmsm_verify_scaler_wiring() { return 0; }
        llmsm_verify_scaler_keda_trust() { return "$STUB_TRUST"; }
        STUB_TRUST="$1" STUB_ROLLOUT=0 STUB_COUNTS='1/1' LLMSM_REPLICAS=1 LLMSM_NS=wns LLMSM_SCOPE=cluster LLMSM_WATCH_NS='' \
            SCALER_BACKEND=keda DEPLOY_PROMETHEUS=false DEPLOY_OPERATIONAL_DASHBOARD=false \
            KEDA_NAMESPACE=keda LLMSM_VERIFY_TIMEOUT=1s verify_deployment >/dev/null 2>&1
    ) || rc=$?
    echo "$rc"
}
check 'verify: KEDA with certificates that match the scaler passes' '0' "$(verify_keda 0)"
check 'verify: KEDA with certificates that do not match the scaler fails' '1' "$(verify_keda 1)"

# Every controller wait uses the same helper; a bare rollout status reports a
# controller at 0 as rolled out. Comment lines aside.
tr -d '\r' < "$ROOT/deploy/utilization-share.sh" > "$TMP/utilization-share.sh"
check 'every controller wait uses the helper (no bare rollout status)' '0' \
    "$(cat "$TMP/infra_scaling_manager.sh" "$TMP/verify.sh" "$TMP/utilization-share.sh" | grep -v '^[[:space:]]*#' | grep -c 'kubectl rollout status')"

# llmsm_append_replicas_patch: refuses what is not a count, and says when a
# re-install changes the live one. log_error exits, so each runs in a subshell.
patch_with() { # patch_with <LLMSM_REPLICAS> <live spec.replicas> -> rc and what it said
    local out rc=0
    : > "$TMP/k.yaml"
    out="$(STUB_LIVE="$2" LLMSM_REPLICAS="$1" llmsm_append_replicas_patch "$TMP/k.yaml" wns 2>&1)" || rc=$?
    printf '%s %s %s\n' "$rc" \
        "$(grep -q 'is not a non-negative integer' <<<"$out" && echo refused || echo taken)" \
        "$(grep -q 'the live Deployment runs' <<<"$out" && echo says-change || echo silent)"
}
check 'replicas: 1 over a live 1 is taken, silently'        '0 taken silent'      "$(patch_with 1 1)"
check 'replicas: 1 over a live 2 says the count changes'    '0 taken says-change' "$(patch_with 1 2)"
check 'replicas: 0 is taken'                                '0 taken silent'      "$(patch_with 0 0)"
check 'replicas: abc is refused'                            '1 refused silent'    "$(patch_with abc 1)"
check 'replicas: -1 is refused'                             '1 refused silent'    "$(patch_with -1 1)"
check 'replicas: 01 is refused'                             '1 refused silent'    "$(patch_with 01 1)"

if [ "$FAILED" -gt 0 ]; then
    printf '\n%d controller readiness assertion(s) failed.\n' "$FAILED" >&2
    exit 1
fi
if [ "$RAN" -ne "$EXPECT_CASES" ]; then
    printf '\n%d case(s) reported ok, %d expected -- the run did not finish.\n' "$RAN" "$EXPECT_CASES" >&2
    exit 1
fi
printf '\nController readiness checks passed (%d cases).\n' "$RAN"
