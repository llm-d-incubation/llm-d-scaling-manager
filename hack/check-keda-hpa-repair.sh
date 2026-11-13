#!/usr/bin/env bash
#
# Executes the installer's KEDA HPA repair grant handling (deploy/lib) against a
# STUBBED kubectl and the real kustomize. What this pins, each of which
# parses cleanly and is wrong on a cluster:
#
#   revoke        without KEDA_HPA_REPAIR=true, the prereqs phase deletes the
#                 grant by the names the install RENDERS (the per-install
#                 suffix in cluster scope), one object per kubectl call; a
#                 Forbidden delete is reported with the admin's command, never
#                 as removed; nothing there says nothing
#   watched copy  with it, the Role copied into a namespace managed from
#                 outside is the rendered HPA Role (selected by name), renamed,
#                 labelled and bound to the controller's ServiceAccount;
#                 without it, the copy is deleted, and Forbidden is reported
#   undeploy      the uninstall's render always carries the component, so it
#                 deletes the grant whatever KEDA_HPA_REPAIR says now
#   refusal       a value other than true or false stops with an error naming
#                 KEDA_HPA_REPAIR
#
# LIB_DIR overrides deploy/lib, so a negative control can run this against a
# broken copy. Every case asserts something a missing function also fails.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LIB_DIR="${LIB_DIR:-$ROOT/deploy/lib}"
FAIL=0
CASES=0
EXPECTED_CASES=10
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

fail() { echo "  FAIL: $*"; FAIL=$((FAIL + 1)); }
begin() { CASES=$((CASES + 1)); echo "== $*"; }

command -v yq >/dev/null 2>&1 || { echo "FAIL: yq is required (not skipping: a skipped check reads as a pass)"; exit 1; }
if [ -n "${KUSTOMIZE:-}" ]; then
    KBUILD=("$KUSTOMIZE" build)
elif [ -x "$ROOT/bin/kustomize" ]; then
    KBUILD=("$ROOT/bin/kustomize" build)
elif command -v kustomize >/dev/null 2>&1; then
    KBUILD=(kustomize build)
else
    echo "FAIL: kustomize is required (make kustomize)"
    exit 1
fi

for f in constants common infra_scaling_manager cleanup physical_limiter pod_writer scaler_tls; do
    if [ -f "$LIB_DIR/$f.sh" ]; then tr -d '\r' <"$LIB_DIR/$f.sh" >"$WORK/$f.sh"; else : >"$WORK/$f.sh"; fi
done

CTRL=llmsm-ctl
WATCHED=team-a

# stub_kubectl records every call in $WORK/calls and answers:
#   kustomize DIR      the real render
#   apply -f -         stdin appended to $WORK/applied.yaml
#   delete -f -        stdin appended to $WORK/deleted.yaml
#   delete KIND NAME   $DELETE_MODE: deleted | forbidden | absent
#   get ...            NotFound
# anything else succeeds silently.
stub_kubectl() {
    printf '%s\n' "$*" >>"$WORK/calls"
    case "$1" in
        kustomize) "${KBUILD[@]}" "$2" ;;
        apply) cat >>"$WORK/applied.yaml"; printf -- '---\n' >>"$WORK/applied.yaml" ;;
        delete)
            if [ "${2:-}" = "-f" ]; then cat >>"$WORK/deleted.yaml"; return 0; fi
            case "${DELETE_MODE:-absent}" in
                deleted) echo "$2.rbac.authorization.k8s.io \"$3\" deleted" ;;
                forbidden) echo "Error from server (Forbidden): $2.rbac.authorization.k8s.io \"$3\" is forbidden: User \"tenant\" cannot delete resource" >&2; return 1 ;;
            esac ;;
        get) echo 'Error from server (NotFound): not found' >&2; return 1 ;;
    esac
    return 0
}

# in_lib runs "$@" in a subshell with the libraries sourced and kubectl stubbed,
# output to $WORK/out.
in_lib() {
    : >"$WORK/calls"; : >"$WORK/applied.yaml"; : >"$WORK/deleted.yaml"
    (
        BLUE='' GREEN='' YELLOW='' RED='' NC=''
        for f in constants common infra_scaling_manager cleanup physical_limiter pod_writer scaler_tls; do
            # shellcheck source=/dev/null
            source "$WORK/$f.sh"
        done
        kubectl() { stub_kubectl "$@"; }
        LLMSM_PROJECT="$ROOT"
        LLMSM_NS="$CTRL"
        "$@"
    ) >"$WORK/out" 2>&1
}

# rendered_names <scope> echoes "kind name" of the KEDA HPA repair objects the
# install renders for $CTRL: the component plus the per-install renames.
rendered_names() {
    local dir
    dir="$(mktemp -d "$WORK/ovl.XXXXXX")"
    ln -s "$ROOT/config/overlays/$1-scoped/kubernetes" "$dir/base"
    printf 'namespace: %s\nresources:\n- ./base\n' "$CTRL" >"$dir/kustomization.yaml"
    (
        BLUE='' GREEN='' YELLOW='' RED='' NC=''
        # shellcheck source=/dev/null
        source "$WORK/constants.sh"; source "$WORK/common.sh"
        LLMSM_SCOPE="$1" LLMSM_PROJECT="$ROOT" llmsm_append_keda_hpa_repair_component "$dir"
        llmsm_append_crb_name_patches "$dir/kustomization.yaml" "$CTRL"
    ) >/dev/null 2>&1
    "${KBUILD[@]}" "$dir" 2>/dev/null | yq -N -r 'select(.metadata.name | test("keda-hpa-repair")) | (.kind | downcase) + " " + .metadata.name' | sort
}

# deleted_names echoes "kind name" of every single-object delete in $WORK/calls.
deleted_names() {
    awk '$1 == "delete" && $2 != "-f" { print $2 " " $3 }' "$WORK/calls" | sort
}

for scope in cluster namespace; do
    want="$(rendered_names "$scope")"
    begin "revoke, $scope scope: deletes exactly the rendered names"
    [ -n "$want" ] || fail "the component rendered nothing for $scope scope"
    DELETE_MODE=deleted LLMSM_SCOPE="$scope" in_lib llmsm_revoke_keda_hpa_repair
    got="$(deleted_names)"
    [ "$got" = "$want" ] || fail "deleted [$(echo "$got" | paste -sd,)], rendered [$(echo "$want" | paste -sd,)]"
    grep -q 'removed the HPA grant' "$WORK/out" || fail "a delete that removed the grant was not reported: $(cat "$WORK/out")"
    grep -q 'INSTALL_PHASE=scaling-manager' "$WORK/out" || fail "the split-install step is not named: $(cat "$WORK/out")"
done

begin "revoke: one object per kubectl call (render mode records one target per call)"
LLMSM_SCOPE=cluster DELETE_MODE=deleted in_lib llmsm_revoke_keda_hpa_repair
n="$(awk '$1 == "delete"' "$WORK/calls" | awk '{ t = 0; for (i = 2; i <= NF; i++) if ($i !~ /^-/ && $(i-1) != "-n") t++; if (t != 2) bad++ } END { print bad + 0 }')"
[ "$n" = 0 ] || fail "$n delete call(s) name more than one object: $(cat "$WORK/calls")"

begin "revoke: Forbidden is reported with the admin's command, never as removed"
LLMSM_SCOPE=cluster DELETE_MODE=forbidden in_lib llmsm_revoke_keda_hpa_repair
grep -q 'Forbidden' "$WORK/out" || fail "a Forbidden delete was not reported: $(cat "$WORK/out")"
grep -q 'kubectl delete clusterrole scaling-manager-keda-hpa-repair-role-' "$WORK/out" || fail "the admin's command is missing: $(cat "$WORK/out")"
! grep -q 'removed the HPA grant' "$WORK/out" || fail "a Forbidden delete was reported as removed"

begin "revoke: nothing to delete says nothing"
LLMSM_SCOPE=cluster DELETE_MODE=absent in_lib llmsm_revoke_keda_hpa_repair
[ ! -s "$WORK/out" ] || fail "said something with nothing to delete: $(cat "$WORK/out")"

# The watched-namespace copy needs an overlay that already carries the
# namespace-scoped component, as the deploy's tmp overlay does.
OVL="$WORK/deploy-ovl"
mkdir -p "$OVL"
ln -s "$ROOT/config/overlays/namespace-scoped/kubernetes" "$OVL/base"
printf 'namespace: %s\nresources:\n- ./base\n' "$CTRL" >"$OVL/kustomization.yaml"
(
    BLUE='' GREEN='' YELLOW='' RED='' NC=''
    # shellcheck source=/dev/null
    source "$WORK/constants.sh"; source "$WORK/common.sh"
    LLMSM_SCOPE=namespace LLMSM_PROJECT="$ROOT" llmsm_append_keda_hpa_repair_component "$OVL"
) >/dev/null 2>&1

begin "watched copy, on: the rendered HPA Role, renamed, labelled, bound to the controller"
KEDA_HPA_REPAIR=true LLMSM_SCOPE=namespace LLMSM_WATCH_NS="$WATCHED" in_lib llmsm_apply_watch_namespace_hpa_repair "$OVL"
role="$(yq -N -r 'select(.kind == "Role") | .metadata.namespace + "/" + .metadata.name + " " + ([.rules[].resources[]] | join(",")) + " " + ([.rules[].verbs[]] | join(","))' "$WORK/applied.yaml")"
case "$role" in
    "$WATCHED/scaling-manager-keda-hpa-repair-role-"*" horizontalpodautoscalers get,list,patch") ;;
    *) fail "applied Role [$role]; out: $(cat "$WORK/out")" ;;
esac
[ "$(yq -N -r 'select(.kind == "Role") | .metadata.labels."llm-d.ai/watch-grant-for"' "$WORK/applied.yaml")" != "" ] \
    || fail "the copied Role carries no watch-grant label, so the revoke would not find it"
binding="$(yq -N -r 'select(.kind == "RoleBinding") | .metadata.namespace + " " + .roleRef.name + " " + .subjects[0].namespace + "/" + .subjects[0].name' "$WORK/applied.yaml")"
case "$binding" in
    "$WATCHED scaling-manager-keda-hpa-repair-role-"*" $CTRL/scaling-manager-controller-manager") ;;
    *) fail "applied RoleBinding [$binding]" ;;
esac

begin "watched copy, off: the copy is deleted"
KEDA_HPA_REPAIR=false LLMSM_SCOPE=namespace LLMSM_WATCH_NS="$WATCHED" DELETE_MODE=deleted in_lib llmsm_apply_watch_namespace_hpa_repair "$OVL"
grep -q "^delete role scaling-manager-keda-hpa-repair-role-.* -n $WATCHED" "$WORK/calls" || fail "the Role copy was not deleted: $(cat "$WORK/calls")"
grep -q "^delete rolebinding scaling-manager-keda-hpa-repair-rolebinding-.* -n $WATCHED" "$WORK/calls" || fail "the RoleBinding copy was not deleted"
[ ! -s "$WORK/applied.yaml" ] || fail "applied something with the repair off"

begin "watched copy, off: Forbidden is reported"
KEDA_HPA_REPAIR=false LLMSM_SCOPE=namespace LLMSM_WATCH_NS="$WATCHED" DELETE_MODE=forbidden in_lib llmsm_apply_watch_namespace_hpa_repair "$OVL"
grep -q "Forbidden" "$WORK/out" || fail "a Forbidden delete in $WATCHED was not reported: $(cat "$WORK/out")"

begin "undeploy: deletes the grant though KEDA_HPA_REPAIR is unset"
unset KEDA_HPA_REPAIR
LLMSM_SCOPE=cluster in_lib undeploy_scaling_manager_controller
got="$(yq -N -r 'select(.metadata.name | test("keda-hpa-repair")) | .kind' "$WORK/deleted.yaml" | sort | paste -sd, -)"
[ "$got" = "ClusterRole,ClusterRoleBinding" ] || fail "the uninstall deletes [$got] of the grant, want ClusterRole,ClusterRoleBinding"

begin "refusal: a value other than true or false names KEDA_HPA_REPAIR"
KEDA_HPA_REPAIR=yes in_lib llmsm_keda_hpa_repair_on
rc=0; grep -q "KEDA_HPA_REPAIR must be true or false (got 'yes')" "$WORK/out" || rc=1
[ "$rc" = 0 ] || fail "no refusal naming the variable: $(cat "$WORK/out")"
KEDA_HPA_REPAIR=true in_lib llmsm_keda_hpa_repair_on && [ ! -s "$WORK/out" ] || fail "true was refused or noisy"

if [ "$CASES" -ne "$EXPECTED_CASES" ]; then
    echo "FAIL: $CASES cases ran, not $EXPECTED_CASES"
    FAIL=$((FAIL + 1))
fi
if [ "$FAIL" -gt 0 ]; then
    echo "check-keda-hpa-repair: $FAIL failure(s) in $CASES cases"
    exit 1
fi
echo "check-keda-hpa-repair: $CASES cases passed"
