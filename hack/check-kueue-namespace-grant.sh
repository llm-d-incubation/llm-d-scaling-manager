#!/usr/bin/env bash
#
# Executes the preflight that refuses a controller-only install whose controller
# could not read the Namespace a Kueue quota needs, against a stubbed kubectl.
#
# The Kueue reader GETs each LocalQueue's Namespace to evaluate a ClusterQueue
# namespaceSelector. A kueue-reader role created before that read existed lacks
# it, an UPGRADE in the controller-only phase (INSTALL_PHASE=scaling-manager) does not touch
# it, and the new controller then bounds the namespace at nothing -- it cannot
# scale up. Nothing about that parses or applies differently, so `bash -n` and a
# green install both miss it.
#
# WRITING A CASE HERE: a function that does not exist refuses nothing. The
# guard below refuses to run unless it is defined, and EXPECT_CASES refuses a run
# that ended early.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
command -v yq >/dev/null 2>&1 || { printf 'FATAL: yq is required.\n' >&2; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
for lib in common.sh limiter_policy.sh prereqs.sh; do
    tr -d '\r' < "$ROOT/deploy/lib/$lib" > "$WORK/$lib"
done
BLUE='' GREEN='' YELLOW='' RED='' NC=''
unset LLMSM_QUOTA_KUEUE_BORROW KUEUE_NAMESPACE
# shellcheck source=/dev/null
source "$WORK/common.sh"
# shellcheck source=/dev/null
source "$WORK/limiter_policy.sh"
# shellcheck source=/dev/null
source "$WORK/prereqs.sh"
for fn in llmsm_check_kueue_namespace_read llmsm_check_kueue_cohort_read llmsm_kueue_source_configured check_permissions \
          llmsm_kueue_borrow_wanted llmsm_check_kueue_borrow_read; do
    declare -F "$fn" >/dev/null || {
        printf 'FATAL: %s is not defined after sourcing the library.\n' "$fn" >&2
        exit 1
    }
done

EXPECT_CASES=75
RAN=0
FAILED=0

check() {
    if [ "$2" = "$3" ]; then
        RAN=$((RAN + 1))
        printf 'ok    %s\n' "$1"
    else
        FAILED=$((FAILED + 1))
        printf 'FAIL  %s\n        want [%s]\n        got  [%s]\n' "$1" "$2" "$3" >&2
    fi
}

POLICY_WITH_KUEUE='limiters:
- name: q
  type: quota
  kueue:
    enabled: true'

# stub_kubectl answers from the STUB_* variables below. Errors go to stderr,
# where the API server puts them, and STUB_WARN=1 adds a warning there on every
# call while still SUCCEEDING -- what a deprecated API or a webhook does. A
# warning is never data: no verdict may change with it.
stub_kubectl() {
    echo "$*" >> "$WORK/kubectl.log"
    [ -z "${STUB_WARN:-}" ] || echo 'Warning: the kubectl client is older than the server' >&2
    case "$*" in
        "auth can-i create "*)
            # check_permissions: the caller's own create rights.
            case " ${STUB_DENY_CREATE:-} " in
                *" $4 "*) echo no; return 1 ;;
            esac
            echo yes; return 0
            ;;
        "auth can-i "*)
            case "${STUB_CANI:-no}" in
                yes) echo yes; return 0 ;;
                no) echo no; return 1 ;;
                *) echo 'Error from server (Forbidden): users "x" is forbidden: cannot impersonate' >&2; return 1 ;;
            esac
            ;;
        "api-resources "*)
            printf '%s\n' \
                'configmaps cm v1 true ConfigMap' \
                'services svc v1 true Service' \
                'serviceaccounts sa v1 true ServiceAccount' \
                'secrets  v1 true Secret' \
                'deployments deploy apps/v1 true Deployment' \
                'networkpolicies netpol networking.k8s.io/v1 true NetworkPolicy' \
                'roles  rbac.authorization.k8s.io/v1 true Role' \
                'rolebindings  rbac.authorization.k8s.io/v1 true RoleBinding' \
                'clusterroles  rbac.authorization.k8s.io/v1 false ClusterRole' \
                'clusterrolebindings  rbac.authorization.k8s.io/v1 false ClusterRoleBinding'
            ;;
        "get namespace scaling-manager-policy -o name")
            # Whether scaling-manager-policy exists: STUB_WELLKNOWN yes | no | FORBIDDEN,
            # by default yes exactly when a policy is published there.
            case "${STUB_WELLKNOWN:-$([ -n "${STUB_POLICY:-}" ] && echo yes || echo no)}" in
                yes) echo namespace/scaling-manager-policy ;;
                FORBIDDEN) echo 'Error from server (Forbidden): namespaces "scaling-manager-policy" is forbidden' >&2; return 1 ;;
                *) echo 'Error from server (NotFound): namespaces "scaling-manager-policy" not found' >&2; return 1 ;;
            esac
            ;;
        "get namespace "*)
            case "${STUB_LABEL:-}" in
                FORBIDDEN) echo 'Error from server (Forbidden): namespaces is forbidden' >&2; return 1 ;;
                *) printf '%s' "${STUB_LABEL:-}" ;;
            esac
            ;;
        "get configmap scaling-manager-scaling-policy-config -n "*)
            local cns="${5:-}" body=""
            case " ${STUB_CM_FORBIDDEN:-} " in
                *" $cns "*) echo 'Error from server (Forbidden): configmaps is forbidden' >&2; return 1 ;;
            esac
            case "$cns" in
                scaling-manager-policy) body="${STUB_POLICY:-}" ;;
                "${STUB_LABEL:-none}") body="${STUB_LABELLED_POLICY:-}" ;;
                "$LLMSM_NS") body="${STUB_OWN:-}" ;;
            esac
            [ -n "$body" ] || { echo 'Error from server (NotFound): configmaps "scaling-manager-scaling-policy-config" not found' >&2; return 1; }
            printf '%s\n' "$body" ;;
        *) return 0 ;;
    esac
}

# run_check [VAR=value...] runs llmsm_check_kueue_namespace_read (or CHECK_FN) and echoes
# REFUSED, WARNED or OK, then the can-i question asked ("-" for none).
#   STUB_CANI      yes | no | unknown (impersonation denied)
#   STUB_POLICY    the cluster policy default entry in scaling-manager-policy ("" = none)
#   STUB_OWN       the default entry of the controller's own ConfigMap ("" = none)
#   STUB_LABEL     the managed Namespace's policy-namespace label, or FORBIDDEN
#   STUB_LABELLED_POLICY  the default entry in the namespace that label names
#   STUB_CM_FORBIDDEN     namespaces whose policy ConfigMap read is Forbidden
run_check() {
    : > "$WORK/kubectl.log"
    : > "$WORK/out"
    (
        log_info() { :; }
        log_success() { :; }
        log_warning() { echo "WARNED" >> "$WORK/out"; }
        log_error() { echo "REFUSED" >> "$WORK/out"; printf '%s\n' "$1" > "$WORK/refusal"; exit 1; }
        kubectl() { stub_kubectl "$@"; }
        LLMSM_NS=scaling-manager-ctl LLMSM_WATCH_NS='' LLMSM_SCOPE=namespace INSTALL_PHASE=scaling-manager
        LLMSM_LIMITER=quota LLMSM_QUOTA_KUEUE=true STUB_CANI=no STUB_POLICY=''
        local kv
        for kv in "$@"; do eval "$kv"; done
        "${CHECK_FN:-llmsm_check_kueue_namespace_read}"
    )
    local verdict asked
    verdict="$(head -1 "$WORK/out")"
    asked="$(grep '^auth can-i ' "$WORK/kubectl.log" | head -1 | awk '{print $3, $4}')"
    printf '%s %s' "${verdict:-OK}" "${asked:--}"
}

check 'upgrade, Kueue configured, Namespace get denied -> refused' \
    'REFUSED get namespaces/scaling-manager-ctl' "$(run_check)"
check 'the refusal names the remedy' '1' \
    "$(run_check >/dev/null; grep -c 'make setup-prereqs' "$WORK/refusal")"
check 'granted -> proceeds' \
    'OK get namespaces/scaling-manager-ctl' "$(run_check STUB_CANI=yes)"
check 'LLMSM_WATCH_NS: the managed namespace is the one asked about' \
    'REFUSED get namespaces/tenant-a' "$(run_check LLMSM_WATCH_NS=tenant-a)"
check 'cluster scope asks for the unrestricted read' \
    'REFUSED get namespaces' "$(run_check LLMSM_SCOPE=cluster)"
check 'impersonation denied -> warned, not refused' \
    'WARNED get namespaces/scaling-manager-ctl' "$(run_check STUB_CANI=unknown)"
# The prereqs and all phases APPLY the role that grants it.
check 'INSTALL_PHASE=all is not refused (its apply grants the read)' \
    'OK -' "$(run_check INSTALL_PHASE=all)"
check 'no Kueue source -> nothing asked' \
    'OK -' "$(run_check LLMSM_LIMITER=none LLMSM_QUOTA_KUEUE=false)"
check 'LLMSM_QUOTA_KUEUE without the quota limiter -> nothing asked' \
    'OK -' "$(run_check LLMSM_LIMITER=gpu-inventory)"
check 'a cluster policy with kueue.enabled is a configured source' \
    'REFUSED get namespaces/scaling-manager-ctl' \
    "$(run_check LLMSM_LIMITER=none LLMSM_QUOTA_KUEUE=false "STUB_POLICY='$POLICY_WITH_KUEUE'")"
check 'a cluster policy without kueue is not' \
    'OK -' \
    "$(run_check LLMSM_LIMITER=none LLMSM_QUOTA_KUEUE=false "STUB_POLICY='limiters: [{name: g, type: gpu-inventory}]'")"

# The policy the controller ACTUALLY reads, resolved the way the controller
# resolves it (pl_policy_resolution), and Forbidden is unknown, not none.
check 'its own ConfigMap with kueue is a source: an upgrade need not repeat LLMSM_QUOTA_KUEUE' \
    'REFUSED get namespaces/scaling-manager-ctl' \
    "$(run_check LLMSM_LIMITER=none LLMSM_QUOTA_KUEUE=false "STUB_OWN='$POLICY_WITH_KUEUE'")"
check 'admin-owned (manages another namespace): kueue only in scaling-manager-policy, which it never reads -> nothing asked' \
    'OK -' \
    "$(run_check LLMSM_LIMITER=none LLMSM_QUOTA_KUEUE=false LLMSM_WATCH_NS=tenant-a "STUB_POLICY='$POLICY_WITH_KUEUE'")"
check 'a label-named policy namespace with kueue is a source' \
    'REFUSED get namespaces/scaling-manager-ctl' \
    "$(run_check LLMSM_LIMITER=none LLMSM_QUOTA_KUEUE=false STUB_LABEL=platform-policy "STUB_LABELLED_POLICY='$POLICY_WITH_KUEUE'")"
check 'an unreadable policy is unknown: warned, not silently no source' \
    'WARNED get namespaces/scaling-manager-ctl' \
    "$(run_check LLMSM_LIMITER=none LLMSM_QUOTA_KUEUE=false STUB_WELLKNOWN=yes STUB_CM_FORBIDDEN=scaling-manager-policy)"
check 'unknown source, read already granted -> quiet' \
    'OK get namespaces/scaling-manager-ctl' \
    "$(run_check LLMSM_LIMITER=none LLMSM_QUOTA_KUEUE=false STUB_WELLKNOWN=yes STUB_CM_FORBIDDEN=scaling-manager-policy STUB_CANI=yes)"

# A warning kubectl prints on stderr while succeeding is not data. Merged into
# the answer with 2>&1, it became the label ("Warning: ..."), and a `yes` that
# starts with "Warning" was no longer a yes.
check 'stderr warning: the label still names the policy namespace' \
    'REFUSED get namespaces/scaling-manager-ctl' \
    "$(run_check LLMSM_LIMITER=none LLMSM_QUOTA_KUEUE=false STUB_WARN=1 STUB_LABEL=platform-policy "STUB_LABELLED_POLICY='$POLICY_WITH_KUEUE'")"
check 'stderr warning: a granted read is still granted' \
    'OK get namespaces/scaling-manager-ctl' "$(run_check STUB_WARN=1 STUB_CANI=yes)"
check 'stderr warning: a NotFound policy is still no source' \
    'OK -' "$(run_check LLMSM_LIMITER=none LLMSM_QUOTA_KUEUE=false STUB_WARN=1)"

# --- check_permissions in the controller-only phase asks only for what that
# phase applies.
#
# NetworkPolicy/scaling-manager-external-scaler is the admin's by NAME (LLMSM_PREREQ_NAMED)
# and the render's only NetworkPolicy. Filtering by kind name left
# NetworkPolicy in the tenant's list, so INSTALL_PHASE=scaling-manager and --check
# demanded `create networkpolicies` of a tenant for an object that phase never
# applies. The render is stubbed to the shapes that matter: a tenant ConfigMap
# beside the admin's named one, the admin's NetworkPolicy, and admin kinds.
RENDER_FIXTURE='apiVersion: v1
kind: ConfigMap
metadata: {name: scaling-manager-manager-config}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: scaling-manager-metrics-server-ca}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: scaling-manager-controller-manager}
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: scaling-manager-external-scaler}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: scaling-manager-manager-role}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: scaling-manager-leader-election-role}'
# run_perm <INSTALL_PHASE> [VAR=value...]: echoes the create rights asked for
# (sorted), then REFUSED if check_permissions refused on a create right.
run_perm() {
    local phase="$1"
    shift
    : > "$WORK/kubectl.log"
    : > "$WORK/refusal"
    (
        log_info() { :; }
        log_success() { :; }
        log_warning() { :; }
        log_error() { printf '%s\n' "$1" > "$WORK/refusal"; exit 1; }
        kubectl() { stub_kubectl "$@"; }
        llmsm_render_manifests() { printf '%s\n' "$RENDER_FIXTURE"; }
        LLMSM_NS=scaling-manager-ctl LLMSM_WATCH_NS='' LLMSM_SCOPE=namespace INSTALL_PHASE="$phase"
        LLMSM_LIMITER=none LLMSM_QUOTA_KUEUE=false STUB_CANI=yes STUB_DENY_CREATE=networkpolicies
        local kv
        for kv in "$@"; do eval "$kv"; done
        check_permissions
    ) >/dev/null 2>&1
    printf '%s' "$(grep '^auth can-i create ' "$WORK/kubectl.log" | awk '{print $4}' | sort -u | tr '\n' ' ')"
    grep -q 'You cannot create everything' "$WORK/refusal" && printf 'REFUSED'
}
check 'INSTALL_PHASE=scaling-manager: no create networkpolicies asked, not refused' \
    'configmaps deployments ' "$(run_perm scaling-manager)"
check 'INSTALL_PHASE=all still asks it, and a denial refuses (the stub can tell)' \
    'clusterroles configmaps deployments networkpolicies roles REFUSED' "$(run_perm all)"
# A render that comes back empty falls back to a fixed kind list. That list
# carried the admin kinds (Role, RoleBinding, Secret, ServiceAccount,
# ServiceMonitor) into the controller-only phase, and a tenant denied them was
# refused. ServiceMonitor is not served by the stub, so it is never asked.
ADMIN_KINDS_DENIED='roles rolebindings secrets serviceaccounts'
check 'unrendered fallback, INSTALL_PHASE=scaling-manager: only the tenant kinds asked, not refused' \
    'configmaps deployments services ' \
    "$(run_perm scaling-manager RENDER_FIXTURE= "STUB_DENY_CREATE='$ADMIN_KINDS_DENIED'")"
check 'unrendered fallback, INSTALL_PHASE=all: the admin kinds still asked, a denial refuses' \
    'configmaps deployments rolebindings roles secrets serviceaccounts services REFUSED' \
    "$(run_perm all RENDER_FIXTURE= "STUB_DENY_CREATE='$ADMIN_KINDS_DENIED'")"
# LLMSM_LIMITER is a list: one naming quota beside gpu-inventory still reads Kueue.
check 'a list naming quota with Kueue is a configured source' \
    'REFUSED get namespaces/scaling-manager-ctl' "$(run_check LLMSM_LIMITER=gpu-inventory,quota)"

# The physical limiter needs node read, which only the admin setup phase grants.
# Composed with a quota, a controller that cannot list nodes is refused with
# that remedy, rather than installed into NotReady.
# run_node_check [VAR=value...] echoes REFUSED, WARNED or OK for
# llmsm_check_limiter_node_read over the set llmsm_limiter_set resolves.
run_node_check() {
    : > "$WORK/out"
    (
        log_info() { :; }
        log_success() { :; }
        log_warning() { echo "WARNED" >> "$WORK/out"; }
        log_error() { echo "REFUSED" >> "$WORK/out"; printf '%s\n' "$1" > "$WORK/refusal"; exit 1; }
        kubectl() {
            case "${STUB_CANI:-no}" in
                yes) echo yes; return 0 ;;
                no) echo no; return 1 ;;
                *) echo 'Error from server (Forbidden): cannot impersonate'; return 1 ;;
            esac
        }
        LLMSM_SCOPE=cluster LLMSM_LIMITER=none LLMSM_QUOTA_KUEUE=false STUB_CANI=no INSTALL_PHASE=scaling-manager
        local kv
        for kv in "$@"; do eval "$kv"; done
        llmsm_check_limiter_node_read "$(llmsm_limiter_set)" "system:serviceaccount:scaling-manager-ctl:scaling-manager-controller-manager"
    )
    printf '%s' "$(head -1 "$WORK/out")"
    [ -s "$WORK/out" ] || printf 'OK'
}
check 'gpu-inventory,quota without node read -> refused' \
    'REFUSED' "$(run_node_check LLMSM_LIMITER=gpu-inventory,quota)"
check 'the node-read refusal names make setup-prereqs' '1' \
    "$(run_node_check LLMSM_LIMITER=gpu-inventory,quota >/dev/null; grep -c 'make setup-prereqs' "$WORK/refusal")"
check 'a Kueue quota alone adds no physical limiter: only a warning' \
    'WARNED' "$(run_node_check LLMSM_LIMITER=quota LLMSM_QUOTA_KUEUE=true)"
check 'gpu-inventory,quota with node read -> proceeds' \
    'OK' "$(run_node_check LLMSM_LIMITER=gpu-inventory,quota STUB_CANI=yes)"
check 'a named composed set without node read says the physical limiter would be skipped' '1' \
    "$(run_node_check LLMSM_LIMITER=gpu-inventory,quota >/dev/null; grep -c 'SKIPS the physical limiter' "$WORK/refusal")"
# The cases above are the controller-only phase, which cannot grant the read.
# A run that includes the admin phase applies the role that grants it, so it
# must not be refused for lacking it: `make deploy-scaling-manager SCOPE=cluster
# LLMSM_LIMITER=gpu-inventory` was refused on every fresh cluster, where the
# ServiceAccount did not exist yet.
check 'gpu-inventory alone, INSTALL_PHASE=all, no node read yet -> proceeds' \
    'OK' "$(run_node_check LLMSM_LIMITER=gpu-inventory INSTALL_PHASE=all)"
check 'gpu-inventory alone, INSTALL_PHASE=auto -> proceeds' \
    'OK' "$(run_node_check LLMSM_LIMITER=gpu-inventory INSTALL_PHASE=auto)"
check 'gpu-inventory alone, INSTALL_PHASE=scaling-manager, no node read -> refused' \
    'REFUSED' "$(run_node_check LLMSM_LIMITER=gpu-inventory)"
check 'gpu-inventory, INSTALL_PHASE=all, unverifiable -> proceeds' \
    'OK' "$(run_node_check LLMSM_LIMITER=gpu-inventory INSTALL_PHASE=all STUB_CANI=unknown)"

# LLMSM_LIMITER unset keeps the LIVE policy's limiters, so the node read is
# checked against them (check_permissions passes the live set). An upgrade
# over a live gpu-inventory,quota policy without node read used to read
# "no limiter is configured"; it is warned, with the remedy, not refused.
run_live_node_check() {
    : > "$WORK/out"; : > "$WORK/warning"
    (
        log_info() { :; }
        log_success() { :; }
        log_warning() { echo "WARNED" >> "$WORK/out"; printf '%s\n' "$1" >> "$WORK/warning"; }
        log_error() { echo "REFUSED" >> "$WORK/out"; exit 1; }
        kubectl() { echo no; return 1; }
        LLMSM_NS=scaling-manager-ctl LLMSM_SCOPE=cluster
        llmsm_check_limiter_node_read "$1" "system:serviceaccount:scaling-manager-ctl:scaling-manager-controller-manager" live
    )
    printf '%s' "$(head -1 "$WORK/out")"
    [ -s "$WORK/out" ] || printf 'OK'
}
check 'a live gpu-inventory,quota policy without node read -> warned, not refused' \
    'WARNED' "$(run_live_node_check 'gpu-inventory quota')"
check 'the live warning names make setup-prereqs and the physical skip' '2' \
    "$(run_live_node_check 'gpu-inventory quota' >/dev/null; grep -c 'setup-prereqs\|SKIPS the physical limiter' "$WORK/warning")"
# The Cohort list (borrowing). Missing it only stops borrowing from being
# counted, so it warns and never refuses.
check 'cohort list denied -> warned, not refused' \
    'WARNED list cohorts.kueue.x-k8s.io' "$(run_check CHECK_FN=llmsm_check_kueue_cohort_read)"
check 'cohort list granted -> proceeds' \
    'OK list cohorts.kueue.x-k8s.io' "$(run_check CHECK_FN=llmsm_check_kueue_cohort_read STUB_CANI=yes)"
check 'cohort list unverifiable -> warned' \
    'WARNED list cohorts.kueue.x-k8s.io' "$(run_check CHECK_FN=llmsm_check_kueue_cohort_read STUB_CANI=unknown)"
check 'cohort list: INSTALL_PHASE=all asks nothing' \
    'OK -' "$(run_check CHECK_FN=llmsm_check_kueue_cohort_read INSTALL_PHASE=all)"
check 'cohort list: no Kueue source asks nothing' \
    'OK -' "$(run_check CHECK_FN=llmsm_check_kueue_cohort_read LLMSM_LIMITER=none LLMSM_QUOTA_KUEUE=false)"
check 'cohort list: an unreadable policy asks nothing (the namespace check warns)' \
    'OK -' "$(run_check CHECK_FN=llmsm_check_kueue_cohort_read LLMSM_LIMITER=none LLMSM_QUOTA_KUEUE=false STUB_CM_FORBIDDEN=scaling-manager-policy)"

# R2-1: the admin phase grants the cluster-wide Workload read only when a policy
# of this install counts cohort borrowing (llmsm_kueue_borrow_wanted), and the
# Kueue controller read goes to the namespace a kept controllerNamespace names
# (R2-6). run_borrow echoes the decision and that namespace (- for none).
POLICY_NO_BORROW='limiters:
- name: q
  type: quota
  kueue:
    enabled: true
    countBorrowable: false'
POLICY_KUEUE_OPS='limiters:
- name: q
  type: quota
  kueue:
    enabled: true
    controllerNamespace: kueue-ops'
run_borrow() {
    (
        log_info() { :; }
        log_success() { :; }
        log_warning() { :; }
        log_error() { echo REFUSED; exit 1; }
        kubectl() { stub_kubectl "$@"; }
        LLMSM_NS=scaling-manager-ctl LLMSM_WATCH_NS='' LLMSM_SCOPE=namespace
        LLMSM_LIMITER='' LLMSM_QUOTA_KUEUE=false LLMSM_QUOTA_KUEUE_BORROW='' STUB_POLICY=''
        local kv
        for kv in "$@"; do eval "$kv"; done
        llmsm_kueue_borrow_wanted
        printf '%s %s' "$LLMSM_KUEUE_BORROW_WANTED" "${LLMSM_KUEUE_KEPT_NS:--}"
    )
}
# run_borrow_notes: the info lines llmsm_kueue_borrow_wanted prints, one per line.
run_borrow_notes() {
    (
        log_info() { printf '%s\n' "$*"; }
        log_success() { :; }
        log_warning() { :; }
        log_error() { echo REFUSED; exit 1; }
        kubectl() { stub_kubectl "$@"; }
        LLMSM_NS=scaling-manager-ctl LLMSM_WATCH_NS='' LLMSM_SCOPE=namespace
        LLMSM_LIMITER='' LLMSM_QUOTA_KUEUE=false LLMSM_QUOTA_KUEUE_BORROW='' STUB_POLICY=''
        local kv
        for kv in "$@"; do eval "$kv"; done
        llmsm_kueue_borrow_wanted
    )
}
# run_borrow_warnings: the warnings llmsm_kueue_borrow_wanted prints, one per line.
run_borrow_warnings() {
    (
        log_info() { :; }
        log_success() { :; }
        log_warning() { printf '%s\n' "$*"; }
        log_error() { echo REFUSED; exit 1; }
        kubectl() { stub_kubectl "$@"; }
        LLMSM_NS=scaling-manager-ctl LLMSM_WATCH_NS='' LLMSM_SCOPE=namespace
        LLMSM_LIMITER='' LLMSM_QUOTA_KUEUE=false LLMSM_QUOTA_KUEUE_BORROW='' STUB_POLICY=''
        local kv
        for kv in "$@"; do eval "$kv"; done
        llmsm_kueue_borrow_wanted
    )
}
check 'borrow read: no policy namespace and no limiter -> left as it is' 'unknown -' "$(run_borrow)"
check 'borrow read: LLMSM_LIMITER=quota with Kueue -> granted' 'yes -' \
    "$(run_borrow LLMSM_LIMITER=quota LLMSM_QUOTA_KUEUE=true)"
check 'borrow read: LLMSM_QUOTA_KUEUE_BORROW=false -> revoked' 'no -' \
    "$(run_borrow LLMSM_LIMITER=quota LLMSM_QUOTA_KUEUE=true LLMSM_QUOTA_KUEUE_BORROW=false)"
check 'borrow read: LLMSM_LIMITER=none over a live borrowing policy -> revoked' 'no -' \
    "$(run_borrow LLMSM_LIMITER=none "STUB_OWN='$POLICY_WITH_KUEUE'")"
check 'borrow read: a kept countBorrowable: false stays false' 'no -' \
    "$(run_borrow LLMSM_LIMITER=quota LLMSM_QUOTA_KUEUE=true "STUB_OWN='$POLICY_NO_BORROW'")"
check 'borrow read: LLMSM_QUOTA_KUEUE_BORROW=true overrides the kept false' 'yes -' \
    "$(run_borrow LLMSM_LIMITER=quota LLMSM_QUOTA_KUEUE=true LLMSM_QUOTA_KUEUE_BORROW=true "STUB_OWN='$POLICY_NO_BORROW'")"
check 'borrow read: LLMSM_LIMITER unset keeps an admin-owned live borrowing policy' 'yes -' \
    "$(run_borrow LLMSM_WATCH_NS=tenant-a "STUB_OWN='$POLICY_WITH_KUEUE'")"
check 'borrow read: a self-managed controller reading a borrowing cluster policy' 'yes -' \
    "$(run_borrow "STUB_POLICY='$POLICY_WITH_KUEUE'")"
check 'borrow read: an admin-owned controller never reads scaling-manager-policy' 'no -' \
    "$(run_borrow LLMSM_WATCH_NS=tenant-a "STUB_POLICY='$POLICY_WITH_KUEUE'")"
check 'borrow read: an unreadable admin-owned policy is unknown, never no' 'unknown -' \
    "$(run_borrow LLMSM_WATCH_NS=tenant-a STUB_CM_FORBIDDEN=scaling-manager-ctl)"
# MT3: the tenant writes a self-managed controller's own ConfigMap. Once its
# policy namespace resolves (scaling-manager-policy exists, or a label names one) the
# controller reads limiters from there alone, and so does this decision; and
# the tenant's own policy that cannot be read or parsed counts as no.
check 'borrow read: scaling-manager-policy exists, so the tenant own borrowing policy does not decide' 'no -' \
    "$(run_borrow STUB_WELLKNOWN=yes "STUB_OWN='$POLICY_WITH_KUEUE'")"
check 'borrow read: a labelled policy namespace decides alone' 'no -' \
    "$(run_borrow STUB_LABEL=team-policy "STUB_LABELLED_POLICY='scaleUpThreshold: 0.85'" "STUB_OWN='$POLICY_WITH_KUEUE'")"
# 184-2: with no policy namespace resolved the candidate is the tenant's own
# ConfigMap. The tenant writes it, so it never grants the read of every
# tenant's Workloads alone: only this run's explicit LLMSM_LIMITER and
# LLMSM_QUOTA_KUEUE do. The tenant-phase check still warns that the read is missing.
check 'borrow read: no policy namespace resolves, the tenant own borrowing policy alone grants nothing' 'unknown -' \
    "$(run_borrow STUB_WELLKNOWN=no "STUB_OWN='$POLICY_WITH_KUEUE'")"
# A read an earlier run granted on the admin's explicit settings survives a
# plain re-run (no LLMSM_LIMITER): unknown leaves it, and says why. Only explicit
# settings revoke it.
check 'borrow read: a grant from an explicit run survives a plain re-run' 'unknown -' \
    "$(run_borrow STUB_WELLKNOWN=no "STUB_OWN='$POLICY_NO_BORROW'")"
check 'borrow read: a plain re-run says the read is left as it is' '1' \
    "$(run_borrow_notes STUB_WELLKNOWN=no | grep -c 'Workload read of scaling-manager-ctl is left as it is')"
check 'borrow read: explicit settings with LLMSM_QUOTA_KUEUE_BORROW=false revoke it' 'no -' \
    "$(run_borrow STUB_WELLKNOWN=no LLMSM_LIMITER=quota LLMSM_QUOTA_KUEUE=true LLMSM_QUOTA_KUEUE_BORROW=false "STUB_OWN='$POLICY_WITH_KUEUE'")"
check 'borrow read: no policy namespace resolves, the admin explicit LLMSM_LIMITER and LLMSM_QUOTA_KUEUE grant' 'yes -' \
    "$(run_borrow STUB_WELLKNOWN=no LLMSM_LIMITER=quota LLMSM_QUOTA_KUEUE=true "STUB_OWN='$POLICY_WITH_KUEUE'")"
check 'borrow read: the tenant own borrowing policy is warned about, naming the admin settings' '1' \
    "$(run_borrow_warnings STUB_WELLKNOWN=no "STUB_OWN='$POLICY_WITH_KUEUE'" | grep -c 'LLMSM_LIMITER=quota and LLMSM_QUOTA_KUEUE=true')"
check 'borrow read, tenant phase: a tenant own borrowing policy without the read is warned' \
    'WARNED list workloads.kueue.x-k8s.io' \
    "$(run_check CHECK_FN=llmsm_check_kueue_borrow_read LLMSM_LIMITER='' LLMSM_QUOTA_KUEUE=false "STUB_OWN='$POLICY_WITH_KUEUE'")"
check 'borrow read: an unreadable tenant own policy counts as no under explicit settings' 'no -' \
    "$(run_borrow LLMSM_LIMITER=quota LLMSM_QUOTA_KUEUE=true STUB_CM_FORBIDDEN=scaling-manager-ctl)"
check 'borrow read: an unreadable tenant own policy on a plain run is left as it is' 'unknown -' \
    "$(run_borrow STUB_CM_FORBIDDEN=scaling-manager-ctl)"
check 'borrow read: a tenant own policy that does not parse counts as no under explicit settings' 'no -' \
    "$(run_borrow LLMSM_LIMITER=quota LLMSM_QUOTA_KUEUE=true "STUB_OWN='limiters: ['")"
check 'borrow read: an admin-owned policy that does not parse stays unknown' 'unknown -' \
    "$(run_borrow LLMSM_WATCH_NS=tenant-a "STUB_OWN='limiters: ['")"
check 'borrow read: scaling-manager-policy unreadable, both are read and the tenant own one grants nothing' 'unknown -' \
    "$(run_borrow STUB_WELLKNOWN=FORBIDDEN "STUB_OWN='$POLICY_WITH_KUEUE'")"
check 'borrow read: the kept controllerNamespace is read' 'yes kueue-ops' \
    "$(run_borrow LLMSM_WATCH_NS=tenant-a "STUB_OWN='$POLICY_KUEUE_OPS'")"
check 'borrow read, tenant phase: not granted -> warned' \
    'WARNED list workloads.kueue.x-k8s.io' "$(run_check CHECK_FN=llmsm_check_kueue_borrow_read)"
check 'borrow read, tenant phase: granted -> proceeds' \
    'OK list workloads.kueue.x-k8s.io' "$(run_check CHECK_FN=llmsm_check_kueue_borrow_read STUB_CANI=yes)"
check 'borrow read, tenant phase: LLMSM_QUOTA_KUEUE_BORROW=false asks nothing' \
    'OK -' "$(run_check CHECK_FN=llmsm_check_kueue_borrow_read LLMSM_QUOTA_KUEUE_BORROW=false)"

# Wired into check_permissions -- which --check and the install both run --
# BEFORE the tenant early-return, or the namespace-scoped default never reaches it.
body="$(tr -d '\r' < "$ROOT/deploy/lib/prereqs.sh" | sed -n '/^check_permissions() {/,/^}/p')"
call_line="$(printf '%s\n' "$body" | grep -n '^[[:space:]]*llmsm_check_kueue_namespace_read$' | head -1 | cut -d: -f1)"
tenant_line="$(printf '%s\n' "$body" | grep -n 'if llmsm_scope_is_tenant; then' | tail -1 | cut -d: -f1)"
check 'check_permissions calls it' 'yes' "$([ -n "$call_line" ] && echo yes || echo no)"
check 'before the tenant early-return' 'yes' \
    "$([ -n "$call_line" ] && [ -n "$tenant_line" ] && [ "$call_line" -lt "$tenant_line" ] && echo yes || echo no)"
cohort_line="$(printf '%s\n' "$body" | grep -n '^[[:space:]]*llmsm_check_kueue_cohort_read$' | head -1 | cut -d: -f1)"
check 'check_permissions calls the cohort check before the tenant early-return' 'yes' \
    "$([ -n "$cohort_line" ] && [ -n "$tenant_line" ] && [ "$cohort_line" -lt "$tenant_line" ] && echo yes || echo no)"
borrow_line="$(printf '%s\n' "$body" | grep -n '^[[:space:]]*llmsm_check_kueue_borrow_read$' | head -1 | cut -d: -f1)"
check 'check_permissions calls the borrow-read check before the tenant early-return' 'yes' \
    "$([ -n "$borrow_line" ] && [ -n "$tenant_line" ] && [ "$borrow_line" -lt "$tenant_line" ] && echo yes || echo no)"
# And the admin phase acts on it: llmsm_run_prereqs_phase syncs the grant from
# the decision, and passes the kept controllerNamespace to the controller read.
prereqs_body="$(tr -d '\r' < "$ROOT/deploy/lib/install_core.sh" | sed -n '/^llmsm_run_prereqs_phase() {/,/^}/p')"
check 'the prereqs phase syncs the borrowing read from llmsm_kueue_borrow_wanted' '2' \
    "$(printf '%s\n' "$prereqs_body" | grep -c '^[[:space:]]*llmsm_kueue_borrow_wanted$\|pl_sync_kueue_borrow_read "$LLMSM_NS"')"
check 'the prereqs phase grants the Kueue controller read in the kept namespace' '1' \
    "$(printf '%s\n' "$prereqs_body" | grep -c 'pl_grant_kueue_controller_read "$LLMSM_NS" scaling-manager-controller-manager "$LLMSM_KUEUE_KEPT_NS"')"

if [ "$FAILED" -gt 0 ]; then
    printf '\n%d Kueue namespace-grant assertion(s) failed.\n' "$FAILED" >&2
    exit 1
fi
if [ "$RAN" -ne "$EXPECT_CASES" ]; then
    printf '\n%d case(s) reported ok, %d expected -- the run did not finish.\n' \
        "$RAN" "$EXPECT_CASES" >&2
    exit 1
fi
printf '\nKueue namespace-grant checks passed (%d cases).\n' "$RAN"
