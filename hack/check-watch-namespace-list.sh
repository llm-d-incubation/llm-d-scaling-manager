#!/usr/bin/env bash
#
# Executes the installer's handling of LLMSM_WATCH_NS as a LIST (deploy/lib)
# against a stubbed kubectl: one namespace-scoped controller, in a namespace of
# its own, managing several llm-d namespaces.
#
# Pinned here:
#   - `auto` resolves to every namespace with llm-d model servers or an
#     InferencePool, never the controller's own; a list is trimmed and
#     de-duplicated; a bad name, an empty `auto`, a list on a cluster-scoped
#     install and a list holding LLMSM_NS are each refused.
#   - the per-namespace checks run once per listed namespace, never against the
#     controller's, and a listed namespace with no llm-d fails.
#   - the prereqs phase writes the manager Role and RoleBinding in each listed
#     namespace and nowhere else, and the monitors go to each.
#   - the Kueue and policy-namespace reads are narrowed to exactly the listed
#     namespaces (plus scaling-manager-policy and the controller's own), never every Namespace.
#   - the ScaledObject plan scans each listed namespace.
#   - a namespace-scoped quota over a list is keyed `default` (one budget each);
#     LLMSM_QUOTA_SCOPE=cluster gives one budget for all of them.
#   - the uninstall removes the Role and RoleBinding in each namespace the live
#     Deployment names.
#   - a single LLMSM_WATCH_NS behaves as before.
#   - `auto` skips a namespace annotated scaling-manager.llmd.ai/exclude=true and one
#     another controller manages, warning about each.
#   - a shrunk list revokes this install's labelled grant and monitors in the
#     dropped namespace only, and its Kueue reader names follow the new list;
#     the uninstall finds every namespace it ever set up by that label.
#   - enable-physical-limiter splits a managed list (the Kueue read, and a
#     policy in the controller's own namespace needs no restart).
#   - a non-default controller namespace is the one every grant names.
#   - gpu-inventory alone is refused for a list controller; beside a quota it
#     is taken, and the setup phase grants list on Pods cluster-wide for it.

set -u

# CHECK_ROOT runs the check against another tree -- how its negative control is
# taken: point it at an extracted older deploy/ and the cases must fail.
ROOT="${CHECK_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
FAIL=0
PASS=0
# Every case reports ok or FAIL exactly once; a run that stops early must not
# pass having checked less. Change this deliberately with the cases.
EXPECT_CASES=27

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
for lib in common.sh scaledobject.sh infra_monitoring.sh infra_scaling_manager.sh limiter_policy.sh cleanup.sh install_core.sh physical_limiter.sh; do
    tr -d '\r' < "$ROOT/deploy/lib/$lib" > "$WORK/$lib"
done

for tool in jq yq; do
    command -v "$tool" >/dev/null 2>&1 || { printf 'FATAL: %s is required.\n' "$tool" >&2; exit 1; }
done

ok()   { PASS=$((PASS + 1)); echo "ok   $1"; }
fail() { FAIL=$((FAIL + 1)); echo "FAIL $1"; [ -n "${2:-}" ] && printf '%s\n' "$2" | sed 's/^/       /'; }

# Each case runs in a subshell that sources the libraries afresh, so one case's
# globals cannot leak into the next.
CALLS="$WORK/calls.log"
STUB="$WORK/stub.sh"
cat > "$STUB" <<'STUBEOF'
BLUE='' GREEN='' YELLOW='' RED='' NC=''
for lib in common.sh scaledobject.sh infra_monitoring.sh infra_scaling_manager.sh limiter_policy.sh cleanup.sh; do
    # shellcheck source=/dev/null
    . "$WORK/$lib"
done
#   MS_NS      namespaces whose Deployments carry llm-d.ai/role=decode
#   POOL_NS    namespaces with an InferencePool
#   LIVE_WATCH the live Deployment's LLMSM_WATCH_NAMESPACE
kubectl() {
    echo "kubectl $*" >> "$CALLS"
    case "$*" in
        "auth can-i list deployments -A") echo yes; return 0 ;;
        "auth can-i "*) echo yes; return 0 ;;
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
        "get namespace "*) return 0 ;;
        "get namespaces -o json")
            local ns items=""
            for ns in ${EXCLUDED_NS:-}; do
                items="${items:+$items,}{\"metadata\":{\"name\":\"$ns\",\"annotations\":{\"scaling-manager.llmd.ai/exclude\":\"true\"}}}"
            done
            echo "{\"items\":[${items}]}"; return 0 ;;
        "get deploy -A -l app.kubernetes.io/name=scaling-manager -o json")
            # OTHER_SCALING_MANAGER: "<controller ns>=<its LLMSM_WATCH_NAMESPACE>" words.
            local pair items=""
            for pair in ${OTHER_SCALING_MANAGER:-}; do
                items="${items:+$items,}{\"metadata\":{\"namespace\":\"${pair%%=*}\"},\"spec\":{\"template\":{\"spec\":{\"containers\":[{\"name\":\"manager\",\"env\":[{\"name\":\"LLMSM_WATCH_NAMESPACE\",\"value\":\"${pair#*=}\"}]}]}}}}"
            done
            echo "{\"items\":[${items}]}"; return 0 ;;
        "get clusterrolebindings -o jsonpath="*)
            # CRB_ROLES: the roleRef names of the cluster's bindings.
            local r; for r in ${CRB_ROLES:-}; do echo "$r"; done; return 0 ;;
        "get clusterroles -l llm-d.ai/applied-by=scaling-manager-setup-prereqs -o name")
            echo "clusterrole.rbac.authorization.k8s.io/scaling-manager-epp-metrics-scraper"; return 0 ;;
        "get rolebindings -A -l llm-d.ai/watch-grant-for="*)
            # GRANTED_NS: the namespaces holding this install's labelled grant.
            local ns
            for ns in ${GRANTED_NS:-}; do echo "$ns"; done; return 0 ;;
        "get clusterrole scaling-manager-kueue-reader-role-"*" -o yaml")
            if [ -n "${KUEUE_ROLE_NAMES:-}" ]; then
                printf '%s\n' 'rules:' '- resources: [clusterqueues]' '- resources: [namespaces]' '  resourceNames:'
                local n; for n in $KUEUE_ROLE_NAMES; do echo "  - $n"; done
                return 0
            fi
            echo 'Error from server (NotFound): clusterroles.rbac.authorization.k8s.io "x" not found' >&2; return 1 ;;
        "kustomize "*)
            printf '%s\n' 'apiVersion: rbac.authorization.k8s.io/v1' 'kind: Role' 'metadata:' \
                '  name: scaling-manager-manager-role' '  namespace: zz-ctl' 'rules: []'
            return 0 ;;
        "apply -f -") cat >> "$WORK/applied.yaml"; echo "---" >> "$WORK/applied.yaml"; return 0 ;;
        "get deploy -n "*"-l app.kubernetes.io/name=scaling-manager -o jsonpath="*)
            printf '%s' "${LIVE_WATCH:-}"; return 0 ;;
        "get deploy scaling-manager-controller-manager -n "*" -o jsonpath="*)
            printf '%s' "${LIVE_WATCH:-}"; return 0 ;;
    esac
    return 0
}
STUBEOF
export WORK CALLS STUB

# run <snippet>: sources the stub and the libs, then runs the snippet. Leaves
# out (stdout+stderr) and rc.
run() {
    : > "$CALLS"; : > "$WORK/applied.yaml"
    out="$(bash -c ". \"\$STUB\"; $1" 2>&1)"; rc=$?
}

# 1. auto finds every llm-d namespace, not the controller's own.
run 'MS_NS="team-a zz-ctl" POOL_NS="team-b"; LLMSM_NS=zz-ctl LLMSM_NS_EXPLICIT=zz-ctl LLMSM_SCOPE=namespace LLMSM_WATCH_NS=auto; llmsm_resolve_watch_namespaces; echo "WATCH=$LLMSM_WATCH_NS SRC=$LLMSM_WATCH_NS_SOURCE"'
if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -qx 'WATCH=team-a,team-b SRC=discovered'; then
    ok "auto resolves to team-a (model servers) and team-b (InferencePool), not the controller's namespace"
else
    fail "auto must resolve to team-a,team-b (rc=$rc)" "$out"
fi

# 2. A list is trimmed and de-duplicated.
run 'LLMSM_NS=zz-ctl LLMSM_NS_EXPLICIT=zz-ctl LLMSM_SCOPE=namespace LLMSM_WATCH_NS=" team-a, team-b,team-a"; llmsm_resolve_watch_namespaces; echo "WATCH=$LLMSM_WATCH_NS"'
if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -qx 'WATCH=team-a,team-b'; then
    ok "a list is trimmed and de-duplicated, in order"
else
    fail "a list must come out as team-a,team-b (rc=$rc)" "$out"
fi

# 3-6. Refusals.
run 'LLMSM_NS=zz-ctl LLMSM_NS_EXPLICIT=zz-ctl LLMSM_SCOPE=namespace LLMSM_WATCH_NS="team-a,Team_B"; llmsm_resolve_watch_namespaces'
if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q "'Team_B' is not a namespace name"; then
    ok "a name that is not a namespace name is refused"
else
    fail "Team_B must be refused (rc=$rc)" "$out"
fi
run 'MS_NS="" POOL_NS=""; LLMSM_NS=zz-ctl LLMSM_NS_EXPLICIT=zz-ctl LLMSM_SCOPE=namespace LLMSM_WATCH_NS=auto; llmsm_resolve_watch_namespaces'
if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q 'found no namespace with llm-d'; then
    ok "auto that finds nothing is refused, not installed managing nothing"
else
    fail "an empty auto must be refused (rc=$rc)" "$out"
fi
run 'LLMSM_NS=zz-ctl LLMSM_NS_EXPLICIT=zz-ctl LLMSM_SCOPE=cluster LLMSM_WATCH_NS="team-a,team-b"; llmsm_resolve_watch_namespaces'
if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q 'only a namespace-scoped install takes'; then
    ok "a list on a cluster-scoped install is refused"
else
    fail "a list with LLMSM_SCOPE=cluster must be refused (rc=$rc)" "$out"
fi
run 'LLMSM_NS=team-a LLMSM_NS_EXPLICIT=team-a LLMSM_SCOPE=namespace LLMSM_WATCH_NS="team-a,team-b"; llmsm_resolve_watch_namespaces'
if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q 'runs in a namespace of its own'; then
    ok "a list that holds the controller's own namespace is refused"
else
    fail "LLMSM_NS inside the list must be refused (rc=$rc)" "$out"
fi

# 7. The per-namespace checks run once per listed namespace, never the controller's.
CHECK_SNIPPET='MS_NS="team-a team-b"; LLMSM_NS=zz-ctl LLMSM_SCOPE=namespace LLMSM_WATCH_NS="team-a,team-b"
llmsm_serving_workload_count() { case " $MS_NS " in *" $1 "*) echo 1 ;; *) echo 0 ;; esac; }
llmsm_epp_services() { :; }
llmsm_report_modelserver_metrics() { echo "WATCH ms ${LLMSM_WATCH_NS}" >> "$CALLS"; return 0; }
llmsm_report_epp_flowcontrol() { echo "WATCH epp ${LLMSM_WATCH_NS}" >> "$CALLS"; return 0; }
llmsm_check_managed_namespaces'
run "$CHECK_SNIPPET"
if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q 'Checking the 2 namespace(s) this controller manages' \
    && [ "$(grep -c '^WATCH ' "$CALLS")" -eq 4 ] && grep -qx 'WATCH epp team-b' "$CALLS" \
    && ! grep -q 'WATCH .* zz-ctl$' "$CALLS" && printf '%s' "$out" | grep -q 'Namespaces: all 2 passed'; then
    ok "both checks run once in each listed namespace, never the controller's, and both pass"
else
    fail "the checks must run per listed namespace (rc=$rc)" "$out
$(cat "$CALLS")"
fi

# 8. A listed namespace with no llm-d fails.
run "${CHECK_SNIPPET/MS_NS=\"team-a team-b\"/MS_NS=\"team-a\"}"
if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q 'No llm-d found in team-b' \
    && printf '%s' "$out" | grep -q 'team-b: failed (no llm-d model servers)'; then
    ok "a listed namespace with no llm-d fails, with the namespace named"
else
    fail "team-b with no llm-d must fail (rc=$rc)" "$out"
fi

# 9. The Role and RoleBinding go to each listed namespace, and nowhere else.
run 'LLMSM_NS=zz-ctl LLMSM_SCOPE=namespace LLMSM_WATCH_NS="team-a,team-b"; llmsm_apply_watch_namespace_rbac "$WORK"'
applied="$(yq -r 'select(.kind != null) | .kind + " " + .metadata.namespace' "$WORK/applied.yaml" 2>/dev/null | grep -v '^---$' | sort | paste -sd, -)"
if [ "$rc" -eq 0 ] && [ "$applied" = "Role team-a,Role team-b,RoleBinding team-a,RoleBinding team-b" ]; then
    ok "the manager Role and RoleBinding are written in team-a and team-b only"
else
    fail "Role and RoleBinding must be written in each listed namespace only (rc=$rc, applied=$applied)" "$out"
fi

# 10. The monitors are looked after in each listed namespace.
run 'LLMSM_NS=zz-ctl LLMSM_SCOPE=namespace LLMSM_WATCH_NS="team-a,team-b"; llmsm_monitor_namespaces | paste -sd: -'
if [ "$rc" -eq 0 ] && [ "$(printf '%s' "$out" | tail -1)" = "team-a:team-b" ]; then
    ok "the monitors go to each listed namespace"
else
    fail "llmsm_monitor_namespaces must list team-a and team-b, one per line (rc=$rc)" "$out"
fi

# 11. The Kueue and policy-namespace reads are narrowed to the listed names.
run 'LLMSM_NS=zz-ctl LLMSM_SCOPE=namespace LLMSM_WATCH_NS="team-a,team-b"; k="$WORK/k.yaml"; : > "$k"
llmsm_append_kueue_reader_scope_patch "$k" zz-ctl $(llmsm_watch_words)
llmsm_append_policy_namespace_reader_scope_patch "$k" zz-ctl $(llmsm_watch_words) ""
cat "$k"'
kueue_names="$(printf '%s\n' "$out" | awk '/scaling-manager-kueue-reader-role/{exit} /^      - /{print $2}' | tr -d '"' | paste -sd, -)"
policy_names="$(printf '%s\n' "$out" | awk '/name: scaling-manager-kueue-reader-role/{f=1;next} f && /^      - /{print $2}' | tr -d '"' | sort | paste -sd, -)"
if [ "$rc" -eq 0 ] && [ "$kueue_names" = "team-a,team-b" ] && [ "$policy_names" = "scaling-manager-policy,team-a,team-b,zz-ctl" ]; then
    ok "the Namespace reads are narrowed to the listed namespaces (and scaling-manager-policy and its own), not every Namespace"
else
    fail "the reads must name exactly the listed namespaces (kueue=$kueue_names policy=$policy_names)" "$out"
fi

# 12. The ScaledObject plan scans each listed namespace.
run 'LLMSM_NS=zz-ctl LLMSM_SCOPE=namespace LLMSM_WATCH_NS="team-a,team-b"; so_target_namespaces | paste -sd: -'
if [ "$rc" -eq 0 ] && [ "$(printf '%s' "$out" | tail -1)" = "team-a:team-b" ]; then
    ok "the ScaledObject plan scans team-a and team-b"
else
    fail "so_target_namespaces must give team-a and team-b, one per line (rc=$rc)" "$out"
fi

# 13-14. The quota over a list: one budget each by default, one for all with scope cluster.
run 'LLMSM_NS=zz-ctl LLMSM_SCOPE=namespace LLMSM_WATCH_NS="team-a,team-b" LLMSM_QUOTAS="H100=4"; limiter_entry_yaml quota'
if [ "$rc" -eq 0 ] && printf '%s\n' "$out" | grep -qx '    default:' && ! printf '%s' "$out" | grep -q 'team-a,team-b'; then
    ok "a namespace-scoped quota over a list is keyed default: one budget per namespace"
else
    fail "the namespace quota must be keyed default, never on the list (rc=$rc)" "$out"
fi
run 'LLMSM_NS=zz-ctl LLMSM_SCOPE=namespace LLMSM_WATCH_NS="team-a,team-b" LLMSM_QUOTAS="H100=4" LLMSM_QUOTA_SCOPE=cluster; limiter_entry_yaml quota'
if [ "$rc" -eq 0 ] && printf '%s\n' "$out" | grep -qx '  scope: cluster' && printf '%s\n' "$out" | grep -qx '    H100: 4'; then
    ok "LLMSM_QUOTA_SCOPE=cluster gives one budget for every listed namespace"
else
    fail "the cluster quota must carry H100: 4 under scope: cluster (rc=$rc)" "$out"
fi

# 15. The uninstall removes the grant in each namespace the live Deployment names.
run 'LIVE_WATCH="team-a,team-b"; LLMSM_NS=zz-ctl LLMSM_SCOPE=namespace; llmsm_undeploy_watch_namespace_rbac; echo "WATCH=$LLMSM_WATCH_NS"'
dels="$(grep -E '^kubectl delete (role|rolebinding) ' "$CALLS" | awk '{print $3" "$6}' | sort | paste -sd, -)"
if [ "$rc" -eq 0 ] && [ "$dels" = "role team-a,role team-b,rolebinding team-a,rolebinding team-b" ] \
    && printf '%s' "$out" | grep -qx 'WATCH=team-a,team-b'; then
    ok "the uninstall removes the Role and RoleBinding in each namespace the controller managed"
else
    fail "the uninstall must delete in team-a and team-b (rc=$rc, deletes=$dels)" "$out
$(cat "$CALLS")"
fi

# 16. A single LLMSM_WATCH_NS is unchanged: not a list, keyed on itself.
run 'LLMSM_NS=scaling-manager-team-a LLMSM_SCOPE=namespace LLMSM_WATCH_NS=team-a LLMSM_QUOTAS="H100=4"; llmsm_resolve_watch_namespaces; llmsm_watch_is_multi && echo MULTI; limiter_entry_yaml quota'
if [ "$rc" -eq 0 ] && ! printf '%s' "$out" | grep -qx MULTI && printf '%s\n' "$out" | grep -qx '    team-a:'; then
    ok "a single LLMSM_WATCH_NS is not a list, and its quota is keyed on it as before"
else
    fail "a single LLMSM_WATCH_NS must behave as before (rc=$rc)" "$out"
fi

# 17. auto skips a namespace that opted out, and one another controller manages.
run 'MS_NS="team-a team-b team-c" POOL_NS=""; EXCLUDED_NS="team-b"; OTHER_SCALING_MANAGER="scaling-manager-other=team-c"
LLMSM_NS=zz-ctl LLMSM_NS_EXPLICIT=zz-ctl LLMSM_SCOPE=namespace LLMSM_WATCH_NS=auto; llmsm_resolve_watch_namespaces; echo "WATCH=$LLMSM_WATCH_NS"'
if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -qx 'WATCH=team-a' \
    && printf '%s' "$out" | grep -q 'skipping team-b, annotated scaling-manager.llmd.ai/exclude=true' \
    && printf '%s' "$out" | grep -q 'skipping team-c, already managed by the controller in scaling-manager-other'; then
    ok "auto skips an excluded namespace and one another controller manages, and warns about each"
else
    fail "auto must resolve to team-a alone, warning about team-b and team-c (rc=$rc)" "$out"
fi

# 18. A shrunk list revokes this install's grant and monitors in the dropped namespace only.
run 'GRANTED_NS="team-a team-b team-c"; LLMSM_NS=zz-ctl LLMSM_SCOPE=namespace LLMSM_WATCH_NS="team-a,team-b"; llmsm_apply_watch_namespace_rbac "$WORK"'
revoked="$(grep -E '^kubectl delete (rolebindings,roles|podmonitors)' "$CALLS" | awk '{for (i = 1; i <= NF; i++) if ($i == "-n") print $(i + 1)}' | sort -u | paste -sd, -)"
if [ "$rc" -eq 0 ] && [ "$revoked" = "team-c" ] \
    && grep -q "^kubectl delete rolebindings,roles -n team-c -l llm-d.ai/watch-grant-for=" "$CALLS" \
    && grep -q "^kubectl delete podmonitors.*-n team-c -l llm-d.ai/applied-by=scaling-manager-setup-prereqs,llm-d.ai/applied-for=zz-ctl" "$CALLS" \
    && printf '%s' "$out" | grep -q 'in team-c, which LLMSM_WATCH_NS no longer lists'; then
    ok "shrinking the list removes, by this install's labels, the grant and monitors in the dropped namespace only"
else
    fail "a re-install with team-c dropped must revoke in team-c and nowhere else (rc=$rc, revoked=$revoked)" "$out
$(cat "$CALLS")"
fi

# 19. The Kueue reader names follow the new list: a dropped namespace loses its name, a hand-added one stays.
run 'KUEUE_ROLE_NAMES="team-a team-b team-c scaling-manager-extra"; LLMSM_KUEUE_READER_PREVIOUS="team-a team-b team-c"
LLMSM_NS=zz-ctl; llmsm_kueue_reader_names zz-ctl "team-a,team-b"; echo "NAMES=$LLMSM_KUEUE_READER_NAMES"'
if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -qx 'NAMES=team-a team-b scaling-manager-extra'; then
    ok "the Kueue reader names are recomputed from the new list, keeping a name the install never managed"
else
    fail "the Kueue reader must name team-a team-b scaling-manager-extra (rc=$rc)" "$out"
fi

# 20. The uninstall removes every namespace this install ever set up, found by label.
run 'LIVE_WATCH="team-a,team-b"; GRANTED_NS="team-a team-c"; LLMSM_NS=zz-ctl LLMSM_SCOPE=namespace; llmsm_undeploy_watch_namespace_rbac; echo "WATCH=$LLMSM_WATCH_NS"'
dels="$(grep -E '^kubectl delete (role|rolebinding) ' "$CALLS" | awk '{print $3" "$6}' | sort -u | paste -sd, -)"
if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -qx 'WATCH=team-a,team-b,team-c' \
    && [ "$dels" = "role team-a,role team-b,role team-c,rolebinding team-a,rolebinding team-b,rolebinding team-c" ]; then
    ok "the uninstall finds a namespace an earlier list set up by its label, and removes it too"
else
    fail "the uninstall must cover team-a, team-b and the labelled team-c (rc=$rc, deletes=$dels)" "$out"
fi

# 21. make enable-physical-limiter splits a managed list before naming the Kueue read.
run '. "$WORK/physical_limiter.sh"; LLMSM_NS=zz-ctl; pl_grant_kueue_read zz-ctl scaling-manager-controller-manager "team-a,team-b"'
names="$(yq -r 'select(.kind == "ClusterRole") | .rules[1].resourceNames | join(",")' "$WORK/applied.yaml" 2>/dev/null | grep -v '^$' | head -1)"
if [ "$rc" -eq 0 ] && [ "$names" = "team-a,team-b" ]; then
    ok "enable-physical-limiter names each managed namespace in the Kueue read, not the controller's"
else
    fail "the Kueue read must name team-a,team-b (rc=$rc, names=$names)" "$out"
fi

# 22. A policy in the controller's own namespace is live for a list controller: no restart.
run '. "$WORK/physical_limiter.sh"; pl_policy_is_live "team-a,team-b" zz-ctl zz-ctl && echo LIVE-OWN; pl_policy_is_live "team-a,team-b" scaling-manager-policy zz-ctl || echo RESTART-OTHER; pl_policy_is_live team-a zz-ctl zz-ctl || echo RESTART-SINGLE'
if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -qx LIVE-OWN && printf '%s' "$out" | grep -qx RESTART-OTHER \
    && printf '%s' "$out" | grep -qx RESTART-SINGLE; then
    ok "a list controller reloads a policy in its own namespace live; elsewhere, and a single one, still restart"
else
    fail "pl_policy_is_live must be live for the list controller's own namespace only (rc=$rc)" "$out"
fi

# 23. A non-default controller namespace reaches every grant: the RoleBinding subject,
#     the grant label and the monitor owner label name it, never the default.
run 'LLMSM_NS=llm-scaling LLMSM_SCOPE=namespace LLMSM_WATCH_NS="team-a,team-b"; llmsm_apply_watch_namespace_rbac "$WORK"'
subj="$(yq -r 'select(.kind == "RoleBinding") | .subjects[0].namespace' "$WORK/applied.yaml" 2>/dev/null | grep -v '^---$' | sort -u | paste -sd, -)"
if [ "$rc" -eq 0 ] && [ "$subj" = "llm-scaling" ] && ! grep -q 'scaling-manager-system' "$WORK/applied.yaml" "$CALLS" \
    && grep -q "llm-d.ai/watch-grant-for=$(printf '%s' llm-scaling | sha256sum | cut -c1-8)" "$CALLS"; then
    ok "a controller in llm-scaling is bound and labelled as llm-scaling in each managed namespace"
else
    fail "the grants must name llm-scaling, never the default namespace (rc=$rc, subjects=$subj)" "$out"
fi

# 24. gpu-inventory alone is refused for a list controller; beside a quota it is taken.
run 'LLMSM_NS=zz-ctl LLMSM_SCOPE=namespace LLMSM_WATCH_NS="team-a,team-b" LLMSM_LIMITER=gpu-inventory; llmsm_resolve_watch_namespaces'
alone_rc=$rc alone_out="$out"
run 'LLMSM_NS=zz-ctl LLMSM_SCOPE=namespace LLMSM_WATCH_NS="team-a,team-b" LLMSM_LIMITER=gpu-inventory,quota; llmsm_resolve_watch_namespaces; echo "WATCH=$LLMSM_WATCH_NS"'
if [ "$alone_rc" -ne 0 ] && printf '%s' "$alone_out" | grep -q 'would plan the same free GPUs' \
    && [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -qx 'WATCH=team-a,team-b'; then
    ok "gpu-inventory alone is refused for a list controller, and gpu-inventory,quota is taken"
else
    fail "gpu-inventory alone must be refused and gpu-inventory,quota taken (alone rc=$alone_rc, both rc=$rc)" "$alone_out
$out"
fi

# 25. Option B: the setup phase grants the cluster-wide Pod list, read-only, to this controller only.
run 'LLMSM_NS=llm-scaling LLMSM_SCOPE=namespace LLMSM_WATCH_NS="team-a,team-b" LLMSM_LIMITER=gpu-inventory,quota; llmsm_apply_pod_list_grant'
verbs="$(yq -r 'select(.kind == "ClusterRole") | .rules[] | (.resources | join(",")) + ":" + (.verbs | join(","))' "$WORK/applied.yaml" 2>/dev/null | grep -v '^---$' | paste -sd' ' -)"
subj="$(yq -r 'select(.kind == "ClusterRoleBinding") | .subjects[0].namespace + "/" + .subjects[0].name' "$WORK/applied.yaml" 2>/dev/null | grep -v '^---$' | paste -sd' ' -)"
run 'LLMSM_NS=llm-scaling LLMSM_SCOPE=namespace LLMSM_WATCH_NS="team-a,team-b" LLMSM_LIMITER=quota; llmsm_apply_pod_list_grant; echo "APPLIED=$(grep -c . "$WORK/applied.yaml")"'
if [ "$verbs" = "pods:list" ] && [ "$subj" = "llm-scaling/scaling-manager-controller-manager" ] && printf '%s' "$out" | grep -qx 'APPLIED=0'; then
    ok "gpu-inventory beside a quota gets list on Pods cluster-wide and nothing more; a quota alone gets none"
else
    fail "the Pod list grant must be pods:list for llm-scaling, and absent with a quota alone (verbs=$verbs subj=$subj)" "$out"
fi

# 26. The uninstall removes the shared EPP scrape ClusterRole with its last
#     binding, and keeps it while another binding still uses it.
run 'CRB_ROLES="other-role"; LLMSM_NS=llm-scaling LLMSM_SCOPE=namespace LLMSM_WATCH_NS="team-a,team-b"; llmsm_remove_llmd_monitors'
last_gone=$(grep -c '^kubectl delete clusterrole scaling-manager-epp-metrics-scraper ' "$CALLS")
run 'CRB_ROLES="scaling-manager-epp-metrics-scraper"; LLMSM_NS=llm-scaling LLMSM_SCOPE=namespace LLMSM_WATCH_NS="team-a,team-b"; llmsm_remove_llmd_monitors'
still_bound=$(grep -c '^kubectl delete clusterrole scaling-manager-epp-metrics-scraper ' "$CALLS")
if [ "$last_gone" -ge 1 ] && [ "$still_bound" -eq 0 ]; then
    ok "the EPP scrape ClusterRole goes with its last binding, and stays while one is left"
else
    fail "the EPP scrape ClusterRole must go only with its last binding (unbound deletes=$last_gone, bound deletes=$still_bound)" "$out"
fi

# 27. A list shrunk to one namespace needs no NAMESPACE: LLMSM_WATCH_NS names it.
run 'LLMSM_NS=llm-scaling LLMSM_SCOPE=namespace LLMSM_WATCH_NS=team-a NAMESPACE_EXPLICIT=""; llmsm_require_namespace && echo NAMED'
if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -qx NAMED; then
    ok "a single LLMSM_WATCH_NS outside LLMSM_NS is the managed namespace: NAMESPACE is not also required"
else
    fail "llmsm_require_namespace must accept LLMSM_WATCH_NS=team-a with LLMSM_NS=llm-scaling (rc=$rc)" "$out"
fi

total=$((PASS + FAIL))
echo ""
echo "$PASS passed, $FAIL failed ($total of $EXPECT_CASES cases)"
if [ "$total" -ne "$EXPECT_CASES" ]; then
    echo "FAIL expected $EXPECT_CASES cases, ran $total"
    exit 1
fi
[ "$FAIL" -eq 0 ]
