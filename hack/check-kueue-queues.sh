#!/usr/bin/env bash
#
# Executes the preflight's Kueue-budget check (llmsm_report_kueue_queues and
# llmsm_report_kueue_cluster_budget in deploy/lib/infra_monitoring.sh) against a
# stubbed kubectl.
#
# With LLMSM_QUOTA_KUEUE=true the budget comes from Kueue, and Kueue charges a
# model server's GPUs only through a LocalQueue. A namespace with no LocalQueue,
# one on a queue that declares no GPU or does not admit the namespace, or a
# model server with no kueue.x-k8s.io/queue-name label all install cleanly and
# leave GPUs charged to no queue. These cases pin that each is refused with its
# fix, that a read it may not make is "cannot tell", never a pass, that a
# selected namespace (a LocalQueue named default) needs no label, and that the
# installer and the per-namespace summary call the check.

set -u

# CHECK_ROOT runs the check against another tree -- how its negative control is
# taken: point it at an extracted older deploy/ and the cases must fail.
ROOT="${CHECK_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
FAIL=0
PASS=0
# Every case reports ok or FAIL exactly once; a run that stops early must not
# pass having checked less. Change this deliberately with the cases.
EXPECT_CASES=24

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
for lib in common.sh scaledobject.sh infra_monitoring.sh install_core.sh limiter_policy.sh prereqs.sh; do
    tr -d '\r' < "$ROOT/deploy/lib/$lib" > "$WORK/$lib" 2>/dev/null || : > "$WORK/$lib"
done

for tool in jq yq; do
    command -v "$tool" >/dev/null 2>&1 || { printf 'FATAL: %s is required.\n' "$tool" >&2; exit 1; }
done

BLUE='' GREEN='' YELLOW='' RED='' NC=''
LLMSM_NS=llm-scaling
# shellcheck source=/dev/null
for lib in common.sh scaledobject.sh infra_monitoring.sh limiter_policy.sh; do . "$WORK/$lib"; done

ok()   { PASS=$((PASS + 1)); echo "ok   $1"; }
fail() { FAIL=$((FAIL + 1)); echo "FAIL $1"; [ -n "${2:-}" ] && printf '%s\n' "$2" | sed 's/^/       /'; }

# --- the stub -----------------------------------------------------------------
#
#   NO_KUEUE    yes: the LocalQueue CRD is absent
#   CQ_JSON     the ClusterQueue list
#   LQ_<ns>     the LocalQueue list of a namespace (ns with - as _)
#   LQ_FORBID   namespaces whose LocalQueue list is Forbidden
#   NSLABELS    the namespace labels, as JSON
#   DEP_JSON    the Deployment list
#   POD_JSON    the Pod list
cq() { # cq <name> <gpus> [selector-json]: one GPU ClusterQueue
    printf '{"metadata":{"name":"%s"},"spec":{"namespaceSelector":%s,"resourceGroups":[{"flavors":[{"name":"f","resources":[{"name":"cpu","nominalQuota":"100"},{"name":"nvidia.com/gpu","nominalQuota":"%s"}]}]}]}}' \
        "$1" "${3:-{\}}" "$2"
}
cpu_cq() { printf '{"metadata":{"name":"%s"},"spec":{"namespaceSelector":{},"resourceGroups":[{"flavors":[{"name":"f","resources":[{"name":"cpu","nominalQuota":"100"}]}]}]}}' "$1"; }
lq() { printf '{"metadata":{"name":"%s"},"spec":{"clusterQueue":"%s"}}' "$1" "$2"; }
list() { local IFS=,; printf '{"items":[%s]}' "$*"; }
dep() { # dep <name> [object queue] [template queue]
    local ol="{}" tl='{"llm-d.ai/role":"decode"}'
    [ -n "${2:-}" ] && ol="{\"kueue.x-k8s.io/queue-name\":\"$2\"}"
    [ -n "${3:-}" ] && tl="{\"llm-d.ai/role\":\"decode\",\"kueue.x-k8s.io/queue-name\":\"$3\"}"
    printf '{"kind":"Deployment","metadata":{"name":"%s","labels":%s},"spec":{"template":{"metadata":{"labels":%s}}}}' "$1" "$ol" "$tl"
}
pod() { # pod <name> [queue] [managed]
    local l='"llm-d.ai/role":"decode"'
    [ -n "${2:-}" ] && l="$l,\"kueue.x-k8s.io/queue-name\":\"$2\""
    [ -n "${3:-}" ] && l="$l,\"kueue.x-k8s.io/managed\":\"true\""
    printf '{"metadata":{"name":"%s","labels":{%s}},"status":{"phase":"Running"}}' "$1" "$l"
}

kubectl() {
    case "$*" in
        "get crd localqueues.kueue.x-k8s.io "*)
            [ "${NO_KUEUE:-no}" = yes ] && { echo 'Error from server (NotFound): customresourcedefinitions.apiextensions.k8s.io "localqueues.kueue.x-k8s.io" not found' >&2; return 1; }
            echo v1beta2; return 0 ;;
        "get clusterqueues.kueue.x-k8s.io -o json") printf '%s\n' "${CQ_JSON:-{\"items\":[]\}}"; return 0 ;;
        "get localqueues.kueue.x-k8s.io -n "*)
            local ns="${4}" var
            case " ${LQ_FORBID:-} " in *" $ns "*) echo 'Error from server (Forbidden): localqueues.kueue.x-k8s.io is forbidden' >&2; return 1 ;; esac
            var="LQ_${ns//-/_}"
            case "$*" in
                *jsonpath*) printf '%s' "${!var:-{\"items\":[]\}}" | jq -r '[.items[].spec.clusterQueue] | join(" ")'; return 0 ;;
            esac
            printf '%s\n' "${!var:-{\"items\":[]\}}"; return 0 ;;
        "get namespace "*" -o json")
            [ "${NSLABELS:-}" = FORBIDDEN ] && { echo 'Error from server (Forbidden): namespaces is forbidden' >&2; return 1; }
            printf '{"metadata":{"labels":%s}}\n' "${NSLABELS:-{\}}"; return 0 ;;
        "get deployments -n "*" -o json") printf '%s\n' "${DEP_JSON:-{\"items\":[]\}}"; return 0 ;;
        "get leaderworkersets -n "*) echo 'error: the server does not have a resource type "leaderworkersets"' >&2; return 1 ;;
        "get pods -n "*" -o json") printf '%s\n' "${POD_JSON:-{\"items\":[]\}}"; return 0 ;;
    esac
    return 0
}

# run_case <ns>: llmsm_report_kueue_queues for ns with a Kueue budget; leaves rc and out.
run_case() {
    out="$( (LLMSM_LIMITER=quota LLMSM_QUOTA_KUEUE=true LLMSM_WATCH_NS="$1"; llmsm_report_kueue_queues) 2>&1 )"; rc=$?
}
has() { printf '%s\n' "$out" | grep -qF -- "$1"; }

if ! declare -F llmsm_report_kueue_queues >/dev/null || ! declare -F llmsm_report_kueue_cluster_budget >/dev/null; then
    for n in $(seq 1 "$EXPECT_CASES"); do fail "case $n: llmsm_report_kueue_queues / llmsm_report_kueue_cluster_budget are not defined"; done
    echo "check-kueue-queues: $PASS passed, $FAIL failed"
    exit 1
fi

ONE_CQ="$(list "$(cq inference 4)")"
GOOD_DEP="$(list "$(dep model-a-decode serving serving)")"
GOOD_POD="$(list "$(pod model-a-decode-1 serving yes)")"

# 1. The well-lit shape: a LocalQueue on a GPU ClusterQueue, the model server
# labelled, its pods managed.
CQ_JSON="$ONE_CQ" LQ_team_a="$(list "$(lq serving inference)")" DEP_JSON="$GOOD_DEP" POD_JSON="$GOOD_POD" run_case team-a
if [ "$rc" -eq 0 ] && has "LocalQueue serving -> ClusterQueue inference (4 GPUs)" && has "the model servers in team-a are on LocalQueue serving"; then
    ok "a LocalQueue on a GPU ClusterQueue, with the model servers on it, passes"
else
    fail "the well-lit shape must pass, naming the queue and its GPUs (rc=$rc)" "$out"
fi

# 2. Without a Kueue budget the check says nothing and passes.
out="$( (LLMSM_LIMITER=quota LLMSM_QUOTA_KUEUE=false LLMSM_WATCH_NS=team-a; NO_KUEUE=yes llmsm_report_kueue_queues) 2>&1 )"; rc=$?
if [ "$rc" -eq 0 ] && [ -z "$out" ]; then
    ok "without LLMSM_QUOTA_KUEUE=true the check is silent"
else
    fail "the check must be silent without a Kueue budget (rc=$rc)" "$out"
fi

# 3. Kueue budget with no Kueue on the cluster.
NO_KUEUE=yes run_case team-a
if [ "$rc" -ne 0 ] && has "this cluster has no Kueue"; then
    ok "no Kueue on the cluster fails, naming it"
else
    fail "a Kueue budget on a cluster without Kueue must fail (rc=$rc)" "$out"
fi

# 4. No LocalQueue: fails, lists the GPU ClusterQueues, prints the create.
CQ_JSON="$ONE_CQ" DEP_JSON="$GOOD_DEP" run_case team-a
if [ "$rc" -ne 0 ] && has "no LocalQueue in team-a" && has "inference (4 GPUs)" && has '"clusterQueue":"inference"' && has "kueue.x-k8s.io/v1beta2"; then
    ok "no LocalQueue fails, naming the GPU ClusterQueues and the LocalQueue to create"
else
    fail "a namespace with no LocalQueue must fail with the create command (rc=$rc)" "$out"
fi

# 5. No LocalQueue and no GPU ClusterQueue: says to create one.
CQ_JSON="$(list "$(cpu_cq batch)")" run_case team-a
if [ "$rc" -ne 0 ] && has "No ClusterQueue declares a GPU quota either"; then
    ok "no GPU ClusterQueue at all says to create one"
else
    fail "with no GPU ClusterQueue the fix must say to create one (rc=$rc)" "$out"
fi

# 6. A LocalQueue on a ClusterQueue that does not exist.
CQ_JSON="$ONE_CQ" LQ_team_a="$(list "$(lq serving gone)")" DEP_JSON="$GOOD_DEP" run_case team-a
if [ "$rc" -ne 0 ] && has "points at ClusterQueue 'gone', which does not exist. Point it at inference."; then
    ok "a LocalQueue on a missing ClusterQueue fails, naming the one to use"
else
    fail "a dangling LocalQueue must fail with the queue to point at (rc=$rc)" "$out"
fi

# 7. A LocalQueue on a CPU-only ClusterQueue.
CQ_JSON="$(list "$(cq inference 4)" "$(cpu_cq batch)")" LQ_team_a="$(list "$(lq serving batch)")" DEP_JSON="$GOOD_DEP" run_case team-a
if [ "$rc" -ne 0 ] && has "ClusterQueue batch, which declares no GPU resource"; then
    ok "a LocalQueue on a CPU-only ClusterQueue fails"
else
    fail "a LocalQueue on a CPU-only ClusterQueue must fail (rc=$rc)" "$out"
fi

# 8. A namespaceSelector that does not admit the namespace.
CQ_JSON="$(list "$(cq inference 4 '{"matchLabels":{"team":"ml"}}')")" NSLABELS='{"team":"web"}' \
    LQ_team_a="$(list "$(lq serving inference)")" DEP_JSON="$GOOD_DEP" POD_JSON="$GOOD_POD" run_case team-a
if [ "$rc" -ne 0 ] && has "whose namespaceSelector does not admit team-a"; then
    ok "a ClusterQueue whose namespaceSelector leaves the namespace out fails"
else
    fail "a namespaceSelector that does not admit the namespace must fail (rc=$rc)" "$out"
fi

# 9. The same selector, with the namespace labelled to match: passes.
CQ_JSON="$(list "$(cq inference 4 '{"matchExpressions":[{"key":"team","operator":"In","values":["ml","ai"]}]}')")" NSLABELS='{"team":"ml"}' \
    LQ_team_a="$(list "$(lq serving inference)")" DEP_JSON="$GOOD_DEP" POD_JSON="$GOOD_POD" run_case team-a
if [ "$rc" -eq 0 ]; then
    ok "a matchExpressions selector that admits the namespace passes"
else
    fail "a selector that admits the namespace must pass (rc=$rc)" "$out"
fi

# 10. A null namespaceSelector admits no namespace (Kueue semantics).
CQ_JSON="$(list "$(cq inference 4 null)")" LQ_team_a="$(list "$(lq serving inference)")" DEP_JSON="$GOOD_DEP" POD_JSON="$GOOD_POD" run_case team-a
if [ "$rc" -ne 0 ] && has "does not admit team-a"; then
    ok "a ClusterQueue with no namespaceSelector admits nothing"
else
    fail "a null namespaceSelector must admit no namespace (rc=$rc)" "$out"
fi

# 11. Namespace labels unreadable with a selector: cannot tell, not a failure.
CQ_JSON="$(list "$(cq inference 4 '{"matchLabels":{"team":"ml"}}')")" NSLABELS=FORBIDDEN \
    LQ_team_a="$(list "$(lq serving inference)")" DEP_JSON="$GOOD_DEP" POD_JSON="$GOOD_POD" run_case team-a
if [ "$rc" -eq 0 ] && has "cannot tell whether ClusterQueue inference admits team-a"; then
    ok "unreadable namespace labels are cannot-tell, not a pass or a failure"
else
    fail "unreadable namespace labels must read as cannot tell (rc=$rc)" "$out"
fi

# 12. A Forbidden LocalQueue list is cannot-tell, never no LocalQueue.
CQ_JSON="$ONE_CQ" LQ_FORBID=team-a run_case team-a
if [ "$rc" -eq 0 ] && has "cannot tell whether team-a has a LocalQueue" && ! has "no LocalQueue in"; then
    ok "a Forbidden LocalQueue list is cannot-tell, never no LocalQueue"
else
    fail "Forbidden is not absent: a denied LocalQueue list must not read as none (rc=$rc)" "$out"
fi

# 13. A model server without the queue label fails, with the template patch.
CQ_JSON="$ONE_CQ" LQ_team_a="$(list "$(lq serving inference)")" DEP_JSON="$(list "$(dep model-a-decode)")" POD_JSON="$GOOD_POD" run_case team-a
if [ "$rc" -ne 0 ] && has "deployment/model-a-decode carries no kueue.x-k8s.io/queue-name label" \
    && has '{"spec":{"template":{"metadata":{"labels":{"kueue.x-k8s.io/queue-name":"serving"}}}}}'; then
    ok "a model server with no queue label fails, with the pod-template patch"
else
    fail "an unlabelled model server must fail with the template patch (rc=$rc)" "$out"
fi

# 14. The label on the pod template alone is enough.
CQ_JSON="$ONE_CQ" LQ_team_a="$(list "$(lq serving inference)")" DEP_JSON="$(list "$(dep model-a-decode "" serving)")" POD_JSON="$GOOD_POD" run_case team-a
if [ "$rc" -eq 0 ]; then
    ok "the queue label on the pod template alone passes"
else
    fail "a pod-template queue label must be enough (rc=$rc)" "$out"
fi

# 15. A model server naming a LocalQueue that is not there.
CQ_JSON="$ONE_CQ" LQ_team_a="$(list "$(lq serving inference)")" DEP_JSON="$(list "$(dep model-a-decode other other)")" POD_JSON="$GOOD_POD" run_case team-a
if [ "$rc" -ne 0 ] && has "names LocalQueue 'other', which is not a usable LocalQueue in team-a"; then
    ok "a model server on a LocalQueue that is not there fails"
else
    fail "a model server naming a missing LocalQueue must fail (rc=$rc)" "$out"
fi

# 16. A namespace selected by a LocalQueue named default needs no label.
CQ_JSON="$ONE_CQ" LQ_team_a="$(list "$(lq default inference)")" DEP_JSON="$(list "$(dep model-a-decode)")" POD_JSON="$(list "$(pod model-a-decode-1 default yes)")" run_case team-a
if [ "$rc" -eq 0 ]; then
    ok "a LocalQueue named default selects the namespace: no label needed"
else
    fail "a namespace with a default LocalQueue must pass without labels (rc=$rc)" "$out"
fi

# 17. Pods that started before the queue: running outside Kueue.
CQ_JSON="$ONE_CQ" LQ_team_a="$(list "$(lq default inference)")" DEP_JSON="$(list "$(dep model-a-decode)")" POD_JSON="$(list "$(pod model-a-decode-1)")" run_case team-a
if [ "$rc" -ne 0 ] && has "1 model-server pod(s) in team-a run outside Kueue" && has "They started before their model server was on a queue"; then
    ok "model-server pods without the queue label fail: they run outside Kueue"
else
    fail "unlabelled running pods must fail (rc=$rc)" "$out"
fi

# 18. Labelled pods Kueue does not manage: the namespace is not covered.
CQ_JSON="$ONE_CQ" LQ_team_a="$(list "$(lq serving inference)")" DEP_JSON="$GOOD_DEP" POD_JSON="$(list "$(pod model-a-decode-1 serving)")" run_case team-a
if [ "$rc" -ne 0 ] && has "so Kueue does not manage pods in team-a"; then
    ok "labelled pods Kueue does not manage fail: the namespace is not covered"
else
    fail "labelled but unmanaged pods must fail, naming the coverage (rc=$rc)" "$out"
fi

# 19. LocalQueues on two GPU ClusterQueues: warned, not refused.
CQ_JSON="$(list "$(cq inference 4)" "$(cq spare 2)")" LQ_team_a="$(list "$(lq serving inference)" "$(lq extra spare)")" \
    DEP_JSON="$GOOD_DEP" POD_JSON="$GOOD_POD" run_case team-a
if [ "$rc" -eq 0 ] && has "LocalQueues on several GPU ClusterQueues (inference spare)"; then
    ok "LocalQueues on several GPU ClusterQueues warn (one queue's figure, not the sum)"
else
    fail "several GPU queues in one namespace must warn and pass (rc=$rc)" "$out"
fi

# 20. The summary names the reasons.
r="$(llmsm_ns_check_reasons '  Kueue budget: no LocalQueue in team-a, so Kueue grants it nothing.
  Kueue budget: deployment/m carries no kueue.x-k8s.io/queue-name label, so Kueue charges its GPUs to no queue.')"
n="$(llmsm_ns_check_notes '[SUCCESS]   Kueue budget: the model servers in team-a are on LocalQueue serving.')"
if [ "$r" = "no usable Kueue LocalQueue, model servers not on a Kueue queue" ] && [ "$n" = "on Kueue LocalQueue serving" ]; then
    ok "the namespace summary names the Kueue reasons and the queue"
else
    fail "llmsm_ns_check_reasons/notes must read the Kueue lines (got '$r' / '$n')"
fi

# 21. Cluster scope over the models' queues only: says the figure.
out="$( (LLMSM_LIMITER=quota LLMSM_QUOTA_KUEUE=true LLMSM_QUOTA_SCOPE=cluster LLMSM_QUOTAS='A100=-1'
         CQ_JSON="$ONE_CQ" LQ_team_a="$(list "$(lq serving inference)")" LQ_team_b="$(list "$(lq serving inference)")"
         llmsm_report_kueue_cluster_budget team-a team-b) 2>&1 )"
if printf '%s\n' "$out" | grep -qF "Kueue budget: 4 GPUs, the GPU quota of ClusterQueue(s) inference, shared by every managed namespace"; then
    ok "a cluster-scope Kueue budget over the models' queue reports its GPUs"
else
    fail "the cluster budget must report the queue's GPUs" "$out"
fi

# 22. Cluster scope with another GPU queue and -1: warns, with the finite cap.
out="$( (LLMSM_LIMITER=quota LLMSM_QUOTA_KUEUE=true LLMSM_QUOTA_SCOPE=cluster LLMSM_QUOTAS='A100=-1'
         CQ_JSON="$(list "$(cq inference 4)" "$(cq training 8)")" LQ_team_a="$(list "$(lq serving inference)")"
         llmsm_report_kueue_cluster_budget team-a) 2>&1 )"
if printf '%s\n' "$out" | grep -qF "GPU quota of EVERY ClusterQueue, 12 GPUs" && printf '%s\n' "$out" | grep -qF "LLMSM_QUOTAS='<accelerator>=4' instead of -1"; then
    ok "a cluster-scope Kueue budget that sums another GPU queue warns, with the cap"
else
    fail "the cluster budget must warn about a queue no managed namespace uses" "$out"
fi

# 23. A cluster-scope Kueue quota counts no borrowing: no Workload read.
if [ "$(printf 'limiters:\n- name: q\n  type: quota\n  scope: cluster\n  kueue:\n    enabled: true\n  quotas:\n    A100: -1\n' | policy_counts_borrowing)" = no ] \
    && [ "$(printf 'limiters:\n- name: q\n  type: quota\n  scope: namespace\n  kueue:\n    enabled: true\n' | policy_counts_borrowing)" = yes ]; then
    ok "a scope: cluster Kueue quota counts no borrowing; a namespace one does"
else
    fail "policy_counts_borrowing must answer no for scope: cluster (the controller never counts it)"
fi

# 24. The installer calls the check: per namespace, and on the single-namespace
# preflight and install.
core="$WORK/install_core.sh"
mon="$WORK/infra_monitoring.sh"
if grep -q 'llmsm_report_kueue_queues) 2>&1' "$mon" \
    && [ "$(grep -c 'llmsm_require_kueue_queues$' "$core")" = 2 ] \
    && grep -q 'llmsm_report_kueue_queues || true' "$core"; then
    ok "the per-namespace checks, the preflight and the install all run the Kueue check"
else
    fail "llmsm_check_managed_namespaces and both single-namespace paths must run the Kueue check"
fi

total=$((PASS + FAIL))
echo "check-kueue-queues: $PASS passed, $FAIL failed"
if [ "$total" -ne "$EXPECT_CASES" ]; then
    echo "check-kueue-queues: $total case(s) ran, $EXPECT_CASES expected -- the run did not finish, or a case was added without updating EXPECT_CASES"
    exit 1
fi
[ "$FAIL" -eq 0 ]
