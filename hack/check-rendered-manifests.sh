#!/usr/bin/env bash
#
# Renders the shipped overlays, and the overlay the installer builds on top of
# them, and asserts properties of what would be applied.
#
# The kustomize-build CI job proves each overlay BUILDS; nothing asserted what
# it builds. A grant to an account that does not exist, or a hardening field a
# component patch quietly drops, renders without complaint.
#
# Needs kustomize ($KUSTOMIZE, else bin/kustomize, else `kustomize`, else
# `kubectl kustomize`) and yq. Missing either is FATAL, never a skip.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1

command -v yq >/dev/null 2>&1 || { printf 'FATAL: yq is required.\n' >&2; exit 1; }
if [ -n "${KUSTOMIZE:-}" ]; then
    KBUILD=("$KUSTOMIZE" build)
elif [ -x bin/kustomize ]; then
    KBUILD=(bin/kustomize build)
elif command -v kustomize >/dev/null 2>&1; then
    KBUILD=(kustomize build)
elif command -v kubectl >/dev/null 2>&1; then
    KBUILD=(kubectl kustomize)
else
    printf 'FATAL: kustomize is required (make kustomize).\n' >&2
    exit 1
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
for lib in common.sh infra_scaling_manager.sh install_core.sh limiter_policy.sh physical_limiter.sh prereqs.sh; do
    tr -d '\r' < "deploy/lib/$lib" > "$WORK/$lib"
done
BLUE='' GREEN='' YELLOW='' RED='' NC=''
# shellcheck source=/dev/null
source "$WORK/common.sh"
# shellcheck source=/dev/null
source "$WORK/infra_scaling_manager.sh"
# shellcheck source=/dev/null
source "$WORK/install_core.sh"
# shellcheck source=/dev/null
source "$WORK/limiter_policy.sh"
# shellcheck source=/dev/null
source "$WORK/physical_limiter.sh"
# shellcheck source=/dev/null
source "$WORK/prereqs.sh"
for fn in llmsm_append_crb_name_patches llmsm_append_metrics_reader_subject_patch llmsm_kueue_source_configured \
          llmsm_append_kueue_reader_scope_patch llmsm_append_policy_namespace_reader_scope_patch \
          llmsm_append_install_kueue_reader_scope_patch llmsm_kueue_reader_names pl_grant_kueue_read pl_grant_kueue_borrow_read \
          llmsm_rendered_prereq_objects llmsm_prometheus_scraper_sa llmsm_missing_prereqs llmsm_append_replicas_patch \
          llmsm_append_verbosity_patch llmsm_append_log_level_patch llmsm_append_keda_hpa_repair_component; do
    declare -F "$fn" >/dev/null || {
        printf 'FATAL: %s is not defined after sourcing the library.\n' "$fn" >&2
        exit 1
    }
done

EXPECT_CASES=88
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

# render_installer <scraper> renders the cluster-scoped Kubernetes overlay the
# way deploy_scaling_manager_controller wraps it, with the metrics-reader subject decided.
render_installer() {
    local dir
    dir="$(mktemp -d "$WORK/ovl.XXXXXX")"
    ln -s "$ROOT/config/overlays/cluster-scoped/kubernetes" "$dir/base"
    printf 'namespace: wns\nresources:\n- ./base\n' > "$dir/kustomization.yaml"
    llmsm_append_crb_name_patches "$dir/kustomization.yaml" wns
    llmsm_append_metrics_reader_subject_patch "$dir/kustomization.yaml" wns "$1"
    "${KBUILD[@]}" "$dir"
}

reader_subjects() {
    yq -N -r 'select(.kind == "ClusterRoleBinding" and (.metadata.name | test("^scaling-manager-metrics-reader-rolebinding"))) | .subjects[] | .namespace + "/" + .name'
}

# ----------------------------------------------------------------------------
# The controller /metrics grant names the Prometheus actually here
# ----------------------------------------------------------------------------
out="$(render_installer 'obs prom-sa')" || out=''
[ -n "$out" ] || { printf 'FATAL: the installer overlay did not render.\n' >&2; exit 1; }
check 'metrics-reader: the detected Prometheus is the only subject' \
    'obs/prom-sa' "$(printf '%s\n' "$out" | reader_subjects)"

out="$(render_installer '')" || out=''
[ -n "$out" ] || { printf 'FATAL: the installer overlay did not render.\n' >&2; exit 1; }
check 'metrics-reader: no Prometheus known -> no binding at all' \
    '' "$(printf '%s\n' "$out" | reader_subjects)"
# The delete must take only that binding: the cluster-scoped overlay renders
# three others (manager, metrics-auth, epp-metrics-reader).
check 'metrics-reader: the other bindings survive the delete' \
    '3' \
    "$(printf '%s\n' "$out" | yq -N -r 'select(.kind == "ClusterRoleBinding") | .metadata.name' | grep -c '^scaling-manager-')"

# ----------------------------------------------------------------------------
# The install sets the controller's replicas, at the default 1 too
# ----------------------------------------------------------------------------
# The manifest has no replicas field, so `kubectl apply` kept whatever count
# the live Deployment had: a re-install over a controller scaled to 0 left it
# at 0. The patch used to be written only for LLMSM_REPLICAS other than 1.
render_replicas() {
    local dir
    dir="$(mktemp -d "$WORK/ovl.XXXXXX")"
    ln -s "$ROOT/config/overlays/cluster-scoped/kubernetes" "$dir/base"
    printf 'namespace: wns\nresources:\n- ./base\n' > "$dir/kustomization.yaml"
    LLMSM_REPLICAS="$1" llmsm_append_replicas_patch "$dir/kustomization.yaml" 2>/dev/null
    "${KBUILD[@]}" "$dir" \
        | yq -N -r 'select(.kind == "Deployment" and .metadata.name == "scaling-manager-controller-manager") | .spec.replicas'
}
check 'replicas: the default LLMSM_REPLICAS=1 is written, not left to the live object' \
    '1' "$(render_replicas 1)"
check 'replicas: LLMSM_REPLICAS=2 is written' '2' "$(render_replicas 2)"
check 'replicas: LLMSM_REPLICAS unset writes nothing (the manifest has no replicas)' \
    'null' "$(render_replicas '')"

# ----------------------------------------------------------------------------
# LLMSM_VERBOSITY reaches the manager's args, and only a level it logs at
# ----------------------------------------------------------------------------
# The per-replica capacity lines are at --v=3 and the shipped deployment passes
# no --v, so a benchmark that cannot set it loses its k1/k2 report.
render_verbosity() {
    local dir
    dir="$(mktemp -d "$WORK/ovl.XXXXXX")"
    ln -s "$ROOT/config/overlays/cluster-scoped/kubernetes" "$dir/base"
    printf 'namespace: wns\nresources:\n- ./base\n' > "$dir/kustomization.yaml"
    ( LLMSM_VERBOSITY="$1" llmsm_append_verbosity_patch "$dir/kustomization.yaml" 2>/dev/null ) || { echo refused; return; }
    "${KBUILD[@]}" "$dir" \
        | yq -N -r 'select(.kind == "Deployment" and .metadata.name == "scaling-manager-controller-manager")
            | .spec.template.spec.containers[] | select(.name == "manager") | .args[] | select(test("^--v="))'
}
check 'verbosity: LLMSM_VERBOSITY=3 adds --v=3 to the manager' '--v=3' "$(render_verbosity 3)"
check 'verbosity: unset adds no --v (the shipped default)' '' "$(render_verbosity '')"
check 'verbosity: a non-integer is refused' 'refused' "$(render_verbosity abc)"
check 'verbosity: a level above the most verbose one is refused' 'refused' "$(render_verbosity 6)"

# ----------------------------------------------------------------------------
# LLMSM_LOG_LEVEL reaches the manager's LOG_LEVEL, and only a name it knows
# ----------------------------------------------------------------------------
# The installer used to set LLMSM_LOG_LEVEL and write it nowhere, and the
# controller read no LOG_LEVEL: a debug install logged exactly what an info one
# did. The controller reads LOG_LEVEL now (internal/logging); this is the half
# that must put it there.
render_log_level() {
    local dir
    dir="$(mktemp -d "$WORK/ovl.XXXXXX")"
    ln -s "$ROOT/config/overlays/cluster-scoped/kubernetes" "$dir/base"
    printf 'namespace: wns\nresources:\n- ./base\n' > "$dir/kustomization.yaml"
    ( LLMSM_LOG_LEVEL="$1" llmsm_append_log_level_patch "$dir/kustomization.yaml" 2>/dev/null ) || { echo refused; return; }
    "${KBUILD[@]}" "$dir" \
        | yq -N -r 'select(.kind == "Deployment" and .metadata.name == "scaling-manager-controller-manager")
            | .spec.template.spec.containers[] | select(.name == "manager") | .env[] | select(.name == "LOG_LEVEL") | .value'
}
check 'log level: LLMSM_LOG_LEVEL=debug sets the manager LOG_LEVEL' 'debug' "$(render_log_level debug)"
check 'log level: unset keeps the shipped info' 'info' "$(render_log_level '')"
check 'log level: LOG_LEVEL is set once, not appended beside the shipped one' '1' \
    "$(render_log_level trace | grep -c .)"
check 'log level: a name the controller does not know is refused' 'refused' "$(render_log_level verbose)"
# The cases above call the patch functions directly; these hold the install to
# calling them, or a removed call passes every case above.
check 'log level: the controller install applies the LOG_LEVEL patch' '1' \
    "$(declare -f deploy_scaling_manager_controller | grep -c 'llmsm_append_log_level_patch "$tmp_overlay/kustomization.yaml"')"
check 'verbosity: the controller install applies the --v patch' '1' \
    "$(declare -f deploy_scaling_manager_controller | grep -c 'llmsm_append_verbosity_patch "$tmp_overlay/kustomization.yaml"')"

# ----------------------------------------------------------------------------
# The expected set of admin-owned objects makes the install's own decision
# ----------------------------------------------------------------------------
# llmsm_rendered_prereq_objects is what the controller-only phase requires to
# exist. It listed the metrics-reader binding even when the install had DROPPED
# it (no Prometheus account it trusts), so that phase reported it missing and
# refused, forever. Rendered here through the real function, with only kubectl
# (the kustomize build and the Prometheus CR listing) and the URL detection
# stubbed.
prereq_objects() {
    (
        LLMSM_PROJECT="$ROOT" LLMSM_NS=wns ENVIRONMENT=kubernetes LLMSM_SCOPE="$1" LLMSM_WATCH_NS=''
        PROMETHEUS_SA="$2" PROMETHEUS_URL='https://prom.obs.svc:9090'
        PROMETHEUS_URL_EXPLICIT=1 DEPLOY_PROMETHEUS=false
        kubectl() {
            case "$1" in
                # `command`: KBUILD may itself be `kubectl kustomize`.
                kustomize) command "${KBUILD[@]}" "$2" ;;
                *) return 0 ;;   # no Prometheus CR anywhere
            esac
        }
        llmsm_rendered_prereq_objects
    )
}
# kueue_reader_objects <LLMSM_LIMITER> [<own policy>] counts the Kueue reader's
# ClusterRole and binding in the namespace-scoped install's expected set, with
# the controller's own policy ConfigMap as given (absent when empty) and no
# scaling-manager-policy namespace.
kueue_reader_objects() {
    (
        LLMSM_PROJECT="$ROOT" LLMSM_NS=wns ENVIRONMENT=kubernetes LLMSM_SCOPE=namespace LLMSM_WATCH_NS=''
        LLMSM_LIMITER="$1" KR_OWN="${2:-}"
        PROMETHEUS_SA='' PROMETHEUS_URL='https://prom.obs.svc:9090'
        PROMETHEUS_URL_EXPLICIT=1 DEPLOY_PROMETHEUS=false
        kubectl() {
            case "$1 $*" in
                kustomize*) command "${KBUILD[@]}" "$2" ;;
                *"get configmap scaling-manager-scaling-policy-config -n wns "*)
                    [ -n "$KR_OWN" ] || { echo 'Error from server (NotFound): configmaps "scaling-manager-scaling-policy-config" not found' >&2; return 1; }
                    printf '%s\n' "$KR_OWN" ;;
                *"get namespace scaling-manager-policy -o name")
                    echo 'Error from server (NotFound): namespaces "scaling-manager-policy" not found' >&2; return 1 ;;
                *) return 0 ;;
            esac
        }
        llmsm_rendered_prereq_objects | grep -c '^ClusterRole scaling-manager-kueue-reader-role-\|^ClusterRoleBinding scaling-manager-kueue-reader-rolebinding-' || true
    )
}
# components/kueue-reader is in every namespace-scoped overlay, and kustomize
# cannot make a component conditional: an install declaring no limiter was
# granted a Kueue read it never uses. The installer drops it -- unless a policy
# the controller reads still configures a Kueue source, or the limiter is kept.
check 'kueue-reader: LLMSM_LIMITER=none and no Kueue source -> not rendered' '0' "$(kueue_reader_objects none)"
check 'kueue-reader: LLMSM_LIMITER=quota -> rendered' '2' "$(kueue_reader_objects quota)"
check 'kueue-reader: LLMSM_LIMITER unset keeps the live limiter -> rendered' '2' "$(kueue_reader_objects '')"
check 'kueue-reader: LLMSM_LIMITER=none over a policy with a Kueue source -> rendered' '2' \
    "$(kueue_reader_objects none "$(printf 'limiters:\n- name: q\n  type: quota\n  kueue:\n    enabled: true\n')")"

reader_crb="ClusterRoleBinding scaling-manager-metrics-reader-rolebinding-$(llmsm_ns_suffix wns)"
for scope in cluster namespace; do
    objs="$(prereq_objects "$scope" '')"
    [ -n "$objs" ] || { printf 'FATAL: llmsm_rendered_prereq_objects rendered nothing (%s scope).\n' "$scope" >&2; exit 1; }
    check "$scope scope: no trusted Prometheus account -> the binding is not expected" '0' \
        "$(printf '%s\n' "$objs" | grep -cxF "$reader_crb")"
    check "$scope scope: PROMETHEUS_SA -> the binding is expected" '1' \
        "$(prereq_objects "$scope" obs/prom-sa | grep -cxF "$reader_crb")"
done

# ----------------------------------------------------------------------------
# Who the metrics-reader binding names: only an account the caller CHOSE
# ----------------------------------------------------------------------------
# scraper <PROMETHEUS_SA> <PROMETHEUS_URL> <explicit: 1|''> <DEPLOY_PROMETHEUS>
# runs llmsm_prometheus_scraper_sa against a stub that knows two Prometheus CRs:
# one in `obs` running as prom-sa, one in `dflt` with no serviceAccountName.
# An AUTO-DETECTED URL is whichever Prometheus the API listed first -- on a
# shared cluster someone else's -- and must never decide the subject.
scraper() {
    (
        PROMETHEUS_SA="$1" PROMETHEUS_URL="$2" PROMETHEUS_URL_EXPLICIT="$3" DEPLOY_PROMETHEUS="$4"
        MONITORING_NAMESPACE=scaling-manager-mon
        log_warning() { :; }
        kubectl() {
            case "$*" in
                "get prometheuses.monitoring.coreos.com -n obs "*) echo "obs prom-sa" ;;
                "get prometheuses.monitoring.coreos.com -n dflt "*) echo "dflt " ;;
                *) return 0 ;;
            esac
        }
        llmsm_prometheus_scraper_sa
    )
}
check 'metrics-reader subject: an auto-detected Prometheus is never granted' '' \
    "$(scraper '' 'https://prometheus-operated.obs.svc.cluster.local:9090' '' false)"
check 'metrics-reader subject: auto-detected beside DEPLOY_PROMETHEUS, outside MONITORING_NAMESPACE: none' '' \
    "$(scraper '' 'https://prometheus-operated.obs.svc.cluster.local:9090' '' true)"
check 'metrics-reader subject: an explicit PROMETHEUS_URL reads its Prometheus CR' 'obs prom-sa' \
    "$(scraper '' 'https://prometheus-operated.obs.svc.cluster.local:9090' 1 false)"
check 'metrics-reader subject: explicit URL, CR on the default account: none' '' \
    "$(scraper '' 'https://prometheus-operated.dflt.svc:9090' 1 false)"
check 'metrics-reader subject: PROMETHEUS_SA is taken as given' 'ops scraper' \
    "$(scraper 'ops/scraper' 'https://prometheus-operated.obs.svc:9090' '' false)"
check 'metrics-reader subject: the stack this install deploys' 'scaling-manager-mon kube-prometheus-stack-prometheus' \
    "$(scraper '' 'https://kube-prometheus-stack-prometheus.scaling-manager-mon.svc.cluster.local:9090' '' true)"

# ----------------------------------------------------------------------------
# --check and INSTALL_PHASE=scaling-manager: objects an admin phase may leave out ON PURPOSE
# ----------------------------------------------------------------------------
# The external-scaler NetworkPolicy (written only where KEDA's namespace is
# known) and the metrics-reader binding (only for a Prometheus account the
# caller named) are absent on a correct install, and a tenant cannot see why.
# Reported MISSING, they made --check fail forever and INSTALL_PHASE=scaling-manager refuse.
missing_prereqs() {
    (
        LLMSM_PROJECT="$ROOT" LLMSM_NS=wns ENVIRONMENT=kubernetes LLMSM_SCOPE=namespace LLMSM_WATCH_NS=''
        PROMETHEUS_SA=obs/prom-sa PROMETHEUS_URL='https://prom.obs.svc:9090'
        PROMETHEUS_URL_EXPLICIT=1 DEPLOY_PROMETHEUS=false
        kubectl() {
            case "$1" in
                kustomize) command "${KBUILD[@]}" "$2" ;;
                get) echo "Error from server (NotFound): $2 \"$3\" not found" >&2; return 1 ;;
                *) return 0 ;;
            esac
        }
        llmsm_missing_prereqs
    )
}
mp="$(missing_prereqs)"
[ -n "$mp" ] || { printf 'FATAL: llmsm_missing_prereqs reported nothing on an empty cluster.\n' >&2; exit 1; }
check '--check: an absent external-scaler NetworkPolicy is OPTIONAL' '1' \
    "$(printf '%s\n' "$mp" | grep -c '^OPTIONAL NetworkPolicy/scaling-manager-external-scaler|')"
check '--check: an absent metrics-reader binding is OPTIONAL' '1' \
    "$(printf '%s\n' "$mp" | grep -c "^OPTIONAL ClusterRoleBinding/scaling-manager-metrics-reader-rolebinding-$(llmsm_ns_suffix wns)|")"
check '--check: neither is also reported missing' '0' \
    "$(printf '%s\n' "$mp" | grep -v '^OPTIONAL ' | grep -c 'scaling-manager-external-scaler\|scaling-manager-metrics-reader-rolebinding')"
check '--check: a required object is still MISSING, not optional' '1' \
    "$(printf '%s\n' "$mp" | grep -cx "ClusterRoleBinding/scaling-manager-metrics-auth-rolebinding-$(llmsm_ns_suffix wns)")"

# The kinds INSTALL_PHASE=scaling-manager is checked for are the kinds of what it APPLIES.
# NetworkPolicy/scaling-manager-external-scaler is admin-owned by name, and dropping only
# the admin KINDS left NetworkPolicy in the list: a tenant was asked for
# `create networkpolicies`, for an object that phase never applies.
controller_kinds() {
    (
        LLMSM_PROJECT="$ROOT" LLMSM_NS=wns ENVIRONMENT=kubernetes LLMSM_SCOPE=namespace LLMSM_WATCH_NS=''
        PROMETHEUS_SA='' DEPLOY_PROMETHEUS=false
        kubectl() {
            case "$1" in
                kustomize) command "${KBUILD[@]}" "$2" ;;
                *) return 0 ;;
            esac
        }
        llmsm_rendered_kinds controller
    )
}
ck="$(controller_kinds | tr '\n' ' ')"
check 'INSTALL_PHASE=scaling-manager kinds: no NetworkPolicy, nothing admin-owned' '0' \
    "$(printf '%s\n' $ck | grep -cx 'NetworkPolicy\|ClusterRole\|ClusterRoleBinding\|Role\|RoleBinding\|ServiceMonitor\|ServiceAccount\|Secret\|Namespace')"
check 'INSTALL_PHASE=scaling-manager kinds: still what it applies (Deployment, ConfigMap, Service)' '3' \
    "$(printf '%s\n' $ck | grep -cx 'Deployment\|ConfigMap\|Service')"

# ----------------------------------------------------------------------------
# The namespace-scoped Kueue reader gets ONE Namespace: the managed one
# ----------------------------------------------------------------------------
# internal/kueue GETs a LocalQueue's Namespace by name, and a namespace-scoped
# controller lists LocalQueues in its managed namespace only.
render_kueue_role() {
    local dir overlay="$1" managed="$2"
    dir="$(mktemp -d "$WORK/ovl.XXXXXX")"
    ln -s "$ROOT/config/overlays/$overlay" "$dir/base"
    printf 'namespace: wns\nresources:\n- ./base\n' > "$dir/kustomization.yaml"
    llmsm_append_crb_name_patches "$dir/kustomization.yaml" wns
    llmsm_append_kueue_reader_scope_patch "$dir/kustomization.yaml" wns "$managed"
    "${KBUILD[@]}" "$dir" | yq -N "select(.kind == \"ClusterRole\" and .metadata.name == \"scaling-manager-kueue-reader-role-$(llmsm_ns_suffix wns)\")"
}
# render_kueue_all <overlay> renders the whole namespace-scoped overlay.
render_kueue_all() {
    local dir
    dir="$(mktemp -d "$WORK/ovl.XXXXXX")"
    ln -s "$ROOT/config/overlays/$1" "$dir/base"
    printf 'namespace: wns\nresources:\n- ./base\n' > "$dir/kustomization.yaml"
    llmsm_append_crb_name_patches "$dir/kustomization.yaml" wns
    "${KBUILD[@]}" "$dir"
}
for overlay in namespace-scoped/kubernetes namespace-scoped/openshift; do
    role="$(render_kueue_role "$overlay" tenant-a)" || role=''
    [ -n "$role" ] || { printf 'FATAL: no kueue-reader ClusterRole rendered for %s.\n' "$overlay" >&2; exit 1; }
    check "$overlay: kueue-reader gets only the managed Namespace" 'tenant-a' \
        "$(printf '%s\n' "$role" | yq -r '.rules[] | select(.resources[] == "namespaces") | .resourceNames | join(",")')"
    check "$overlay: kueue-reader Kueue rules stay unnamed" 'true' \
        "$(printf '%s\n' "$role" | yq -r '[.rules[] | select(.apiGroups[] == "kueue.x-k8s.io") | has("resourceNames") | not] | all')"
    # The cluster-wide Workload and LocalQueue read reaches every tenant, so
    # the overlay never grants it: the installer does, and only when a policy
    # counts borrowing (pl_grant_kueue_borrow_read). An overlay that renders
    # it hands it to every namespace-scoped install, LLMSM_LIMITER=none included.
    check "$overlay: no rendered ClusterRole reads Kueue Workloads or LocalQueues" '0' \
        "$(render_kueue_all "$overlay" | yq -N 'select(.kind == "ClusterRole") | [.rules[] | select(.resources[] == "workloads" or .resources[] == "localqueues")] | length' | grep -cv '^0$')"
done

# ----------------------------------------------------------------------------
# Install, enable-physical-limiter, install: one Kueue Namespace rule throughout
# ----------------------------------------------------------------------------
# Both write ClusterRole scaling-manager-kueue-reader-role-<suffix>. The installer granted
# the managed namespace and enable the controller's own, so for a controller in
# scaling-manager-foo managing foo every enable took `get namespaces/foo` away and every
# setup-prereqs gave it back. Run in sequence against one live role: the
# installer's apply is its render, enable's is what pl_grant_kueue_read sends.
LIVE_ROLE="$WORK/live-kueue-role.yaml"
seq_kubectl() {
    case "$1 ${2:-}" in
        "get clusterrole")
            [ -s "$LIVE_ROLE" ] && { cat "$LIVE_ROLE"; return 0; }
            echo "Error from server (NotFound): clusterroles.rbac.authorization.k8s.io \"$3\" not found" >&2
            return 1 ;;
        "apply -f") yq 'select(.kind == "ClusterRole")' - > "$LIVE_ROLE" ;;
        "auth can-i") echo no; return 1 ;;
        kustomize*) command kubectl "$@" ;;
        *) return 0 ;;
    esac
}
live_names() { yq -r '.rules[] | select(.resources[] == "namespaces") | (.resourceNames // ["*"]) | join(",")' "$LIVE_ROLE"; }
# seq_install <ns> <managed>: the prereqs phase rendering and applying the role.
seq_install() {
    local dir
    dir="$(mktemp -d "$WORK/ovl.XXXXXX")"
    ln -s "$ROOT/config/overlays/namespace-scoped/kubernetes" "$dir/base"
    printf 'namespace: %s\nresources:\n- ./base\n' "$1" > "$dir/kustomization.yaml"
    (
        kubectl() { seq_kubectl "$@"; }
        log_error() { echo "REFUSED $1" >&2; exit 1; }
        LLMSM_APPLY_SCOPE=prereqs
        llmsm_append_crb_name_patches "$dir/kustomization.yaml" "$1"
        llmsm_append_install_kueue_reader_scope_patch "$dir/kustomization.yaml" "$1" "$2"
    ) || return 1
    "${KBUILD[@]}" "$dir" \
        | yq "select(.kind == \"ClusterRole\" and .metadata.name == \"scaling-manager-kueue-reader-role-$(llmsm_ns_suffix "$1")\")" > "$LIVE_ROLE"
}
# seq_enable <ns> <managed>: make enable-physical-limiter for that controller.
seq_enable() {
    (
        kubectl() { seq_kubectl "$@"; }
        log_warning() { :; }
        unset LLMSM_LIMITER_TARGETS
        pl_grant_kueue_read "$1" scaling-manager-controller-manager "$2"
    )
}
# seq_run <ns> <managed>: the rule after install, after enable, after install.
seq_run() {
    local a b c
    seq_install "$1" "$2" && a="$(live_names)"
    seq_enable "$1" "$2" && b="$(live_names)"
    seq_install "$1" "$2" && c="$(live_names)"
    printf '%s %s %s' "${a:-none}" "${b:-none}" "${c:-none}"
}
: > "$LIVE_ROLE"
check 'install/enable/install, out of reach (scaling-manager-foo manages foo): foo every time' \
    'foo foo foo' "$(seq_run scaling-manager-foo foo)"
: > "$LIVE_ROLE"
check 'install/enable/install, tenant-owned (team manages team): team every time' \
    'team team team' "$(seq_run team team)"
# A name an admin added by hand survives both commands.
cat > "$LIVE_ROLE" <<'CREOF'
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: placeholder
rules:
- apiGroups: ["kueue.x-k8s.io"]
  resources: ["clusterqueues", "resourceflavors"]
  verbs: ["get", "list"]
- apiGroups: [""]
  resources: ["namespaces"]
  resourceNames: ["admin-added"]
  verbs: ["get"]
CREOF
check 'install/enable/install over an admin-added name: kept by both' \
    'foo,admin-added foo,admin-added foo,admin-added' "$(seq_run scaling-manager-foo foo)"
# enable-physical-limiter writes the kueue-reader role without the Workload
# and LocalQueue rules too, as the overlay renders it; the borrowing read is
# its own role (pl_grant_kueue_borrow_read), which carries both and no more.
: > "$LIVE_ROLE"
seq_enable scaling-manager-foo foo
check 'enable writes no Workload or LocalQueue rule into the kueue-reader role' '0' \
    "$(yq '[.rules[] | select(.resources[] == "workloads" or .resources[] == "localqueues")] | length' "$LIVE_ROLE")"
: > "$LIVE_ROLE"
( kubectl() { seq_kubectl "$@"; }; pl_grant_kueue_borrow_read scaling-manager-foo scaling-manager-controller-manager )
check 'the borrowing role reads Workloads cluster-wide' 'get,list,watch' \
    "$(yq -r '.rules[] | select(.resources[] == "workloads") | .verbs | sort | join(",")' "$LIVE_ROLE")"
check 'the borrowing role lists LocalQueues cluster-wide, and reads nothing else' 'get,list 2' \
    "$(yq -r '.rules[] | select(.resources[] == "localqueues") | .verbs | sort | join(",")' "$LIVE_ROLE") $(yq '.rules | length' "$LIVE_ROLE")"

# ----------------------------------------------------------------------------
# The namespace-scoped policy-namespace reader gets only the Namespaces it reads
# ----------------------------------------------------------------------------
# ResolvePolicyNamespace GETs scaling-manager-policy and the managed Namespace by name.
# enable-physical-limiter writes the SAME ClusterRole limited by resourceNames;
# an install that renders the shipped rule unnamed re-widened it to every
# Namespace in the cluster.
render_policy_ns_role() {
    local dir overlay="$1"
    shift
    dir="$(mktemp -d "$WORK/ovl.XXXXXX")"
    ln -s "$ROOT/config/overlays/$overlay" "$dir/base"
    printf 'namespace: wns\nresources:\n- ./base\n' > "$dir/kustomization.yaml"
    llmsm_append_crb_name_patches "$dir/kustomization.yaml" wns
    llmsm_append_policy_namespace_reader_scope_patch "$dir/kustomization.yaml" wns "$@"
    "${KBUILD[@]}" "$dir" | yq -N "select(.kind == \"ClusterRole\" and .metadata.name == \"scaling-manager-policy-namespace-reader-role-$(llmsm_ns_suffix wns)\")"
}
policy_ns_names() {
    yq -r '.rules[] | select(.resources[] == "namespaces") | .resourceNames // [] | sort | join(",")'
}
for overlay in namespace-scoped/kubernetes namespace-scoped/openshift; do
    # Self-managed, labelled to a policy namespace of its own.
    role="$(render_policy_ns_role "$overlay" wns '' platform-policy)" || role=''
    [ -n "$role" ] || { printf 'FATAL: no policy-namespace-reader ClusterRole rendered for %s.\n' "$overlay" >&2; exit 1; }
    check "$overlay: policy-namespace reader gets own, scaling-manager-policy and the labelled namespace" \
        'platform-policy,scaling-manager-policy,wns' "$(printf '%s\n' "$role" | policy_ns_names)"
    # Admin-owned, managing another namespace; LLMSM_POLICY_NS repeats scaling-manager-policy.
    role="$(render_policy_ns_role "$overlay" tenant-a scaling-manager-policy '')" || role=''
    [ -n "$role" ] || { printf 'FATAL: no policy-namespace-reader ClusterRole rendered for %s.\n' "$overlay" >&2; exit 1; }
    check "$overlay: policy-namespace reader names each Namespace once, nothing unread" \
        'scaling-manager-policy,tenant-a,wns' "$(printf '%s\n' "$role" | policy_ns_names)"
done

# ----------------------------------------------------------------------------
# The controller container runs on a read-only root filesystem, in every overlay
# ----------------------------------------------------------------------------
# Components patch this Deployment by index (args, env, volumes); a patch that
# replaces the securityContext or the volume list would drop either silently.
for overlay in cluster-scoped/kubernetes cluster-scoped/openshift \
               namespace-scoped/kubernetes namespace-scoped/openshift; do
    out="$("${KBUILD[@]}" "config/overlays/$overlay")" || out=''
    [ -n "$out" ] || { printf 'FATAL: %s did not render.\n' "$overlay" >&2; exit 1; }
    dep="$(printf '%s\n' "$out" | yq -N 'select(.kind == "Deployment" and .metadata.labels."control-plane" == "controller-manager")')"
    [ -n "$dep" ] || { printf 'FATAL: no controller Deployment in %s.\n' "$overlay" >&2; exit 1; }
    check "$overlay: manager has readOnlyRootFilesystem" 'true' \
        "$(printf '%s\n' "$dep" | yq -r '.spec.template.spec.containers[] | select(.name == "manager") | .securityContext.readOnlyRootFilesystem')"
    # /tmp writable, and from an emptyDir: a hostPath or a missing volume is not it.
    check "$overlay: /tmp is an emptyDir" 'emptyDir' \
        "$(printf '%s\n' "$dep" | yq -r '(.spec.template.spec.containers[] | select(.name == "manager") | .volumeMounts[] | select(.mountPath == "/tmp") | .name) as $v | .spec.template.spec.volumes[] | select(.name == $v) | keys | map(select(. != "name")) | .[0]')"
done

# ----------------------------------------------------------------------------
# No cluster-wide write on pods for the controller
# ----------------------------------------------------------------------------
# A Pod's image is mutable on a live Pod, so `patch` (or `update`) on pods
# granted cluster-wide lets a stolen controller token run code as any
# ServiceAccount, kube-system included. The controller writes pods only where a
# warm pool or the utilization-share optimizer runs, and gets that write per
# namespace (deploy/lib/pod_writer.sh).
for overlay in cluster-scoped/kubernetes cluster-scoped/openshift; do
    out="$("${KBUILD[@]}" "config/overlays/$overlay")" || out=''
    [ -n "$out" ] || { printf 'FATAL: %s did not render.\n' "$overlay" >&2; exit 1; }
    roles="$(printf '%s\n' "$out" | yq -N -r 'select(.kind == "ClusterRoleBinding" and ([.subjects[]? | select(.kind == "ServiceAccount" and .name == "scaling-manager-controller-manager")] | length > 0) and .roleRef.kind == "ClusterRole") | .roleRef.name' | sort -u)"
    [ -n "$roles" ] || { printf 'FATAL: no ClusterRole is bound to the controller in %s.\n' "$overlay" >&2; exit 1; }
    writes=0
    # One `contains` per (resource, verb) pair, chained selects, no `and`:
    # select(A and B) over two collected sub-lists passed EVERY rule under yq
    # 4.53, which reads as a grant that is not there. This form was checked
    # under 4.53 and the CI pin 4.44.3: it names pods for the old role and nothing
    # for this one.
    for r in $roles; do
        for res in pods '*'; do
            for verb in patch update '*'; do
                n="$(printf '%s\n' "$out" | R="$r" RES="$res" VERB="$verb" yq -N \
                    'select(.kind == "ClusterRole" and .metadata.name == strenv(R)) | .rules[] | select(.resources | contains([strenv(RES)])) | select(.verbs | contains([strenv(VERB)])) | .resources | join(",")' \
                    | grep -c .)"
                writes=$((writes + n))
            done
        done
    done
    check "$overlay: no ClusterRole bound to the controller writes pods cluster-wide" '0' "$writes"
done

# ----------------------------------------------------------------------------
# OpenShift: the ServiceMonitor scrapes as its own account, never the controller
# ----------------------------------------------------------------------------
# A legacy token Secret never expires. Minted for the controller's SA it was the
# controller's whole identity for anyone who could read Secrets in its
# namespace; the scrape needs only `get /metrics`.
for overlay in cluster-scoped/openshift namespace-scoped/openshift; do
    out="$("${KBUILD[@]}" "config/overlays/$overlay")" || out=''
    [ -n "$out" ] || { printf 'FATAL: %s did not render.\n' "$overlay" >&2; exit 1; }
    check "$overlay: no long-lived token Secret for the controller ServiceAccount" '0' \
        "$(printf '%s\n' "$out" | yq -N -r 'select(.kind == "Secret" and .type == "kubernetes.io/service-account-token") | .metadata.annotations."kubernetes.io/service-account.name"' | grep -cx 'scaling-manager-controller-manager')"
    sm_secret="$(printf '%s\n' "$out" | yq -N -r 'select(.kind == "ServiceMonitor" and .metadata.name == "scaling-manager-controller-manager-metrics-monitor") | .spec.endpoints[0].bearerTokenSecret.name')"
    check "$overlay: the ServiceMonitor token belongs to scaling-manager-metrics-scraper" 'scaling-manager-metrics-scraper' \
        "$(printf '%s\n' "$out" | yq -N -r "select(.kind == \"Secret\" and .metadata.name == \"${sm_secret}\") | .metadata.annotations.\"kubernetes.io/service-account.name\"")"
    check "$overlay: metrics-reader names scaling-manager-metrics-scraper alone" 'scaling-manager-system/scaling-manager-metrics-scraper' \
        "$(printf '%s\n' "$out" | reader_subjects)"
done

# ----------------------------------------------------------------------------
# The KEDA HPA repair grant exists only with KEDA_HPA_REPAIR=true
# ----------------------------------------------------------------------------
# get, list and patch on every HPA is a grant nothing else needs, so no overlay
# carries it: the installer adds components/keda-hpa-repair (cluster scope) or
# keda-hpa-repair-namespaced (namespace scope) only when asked, with the
# switch on the Deployment. Rendered the installer's way, per overlay.
render_hpa_repair() {
    local overlay="$1" on="$2" dir scope=namespace
    case "$overlay" in cluster-scoped/*) scope=cluster ;; esac
    dir="$(mktemp -d "$WORK/ovl.XXXXXX")"
    ln -s "$ROOT/config/overlays/$overlay" "$dir/base"
    printf 'namespace: wns\nresources:\n- ./base\n' > "$dir/kustomization.yaml"
    if [ "$on" = true ]; then
        LLMSM_SCOPE="$scope" LLMSM_PROJECT="$ROOT" llmsm_append_keda_hpa_repair_component "$dir"
    fi
    llmsm_append_crb_name_patches "$dir/kustomization.yaml" wns
    "${KBUILD[@]}" "$dir"
}
hpa_grants() {
    yq -N -r 'select((.kind == "Role" or .kind == "ClusterRole") and ([.rules[]?.resources[]?] | any_c(. == "horizontalpodautoscalers"))) | .kind + "/" + .metadata.name + ":" + ([.rules[] | select(.resources[]? == "horizontalpodautoscalers") | .verbs[]] | join(","))'
}
hpa_switch() {
    yq -N -r 'select(.kind == "Deployment" and .metadata.name == "scaling-manager-controller-manager") | .spec.template.spec.containers[0].env[] | select(.name == "KEDA_HPA_REPAIR") | .value'
}
wns_suffix="$(llmsm_ns_suffix wns)"
for overlay in cluster-scoped/kubernetes cluster-scoped/openshift namespace-scoped/kubernetes namespace-scoped/openshift; do
    out="$(render_hpa_repair "$overlay" false)" || out=''
    [ -n "$out" ] || { printf 'FATAL: %s did not render.\n' "$overlay" >&2; exit 1; }
    check "$overlay: no role grants horizontalpodautoscalers without KEDA_HPA_REPAIR" '' \
        "$(printf '%s\n' "$out" | hpa_grants)"
    check "$overlay: the repair is off without KEDA_HPA_REPAIR" '' "$(printf '%s\n' "$out" | hpa_switch)"

    out="$(render_hpa_repair "$overlay" true)" || out=''
    [ -n "$out" ] || { printf 'FATAL: %s did not render with the KEDA HPA repair.\n' "$overlay" >&2; exit 1; }
    case "$overlay" in
        cluster-scoped/*)
            want_grant="ClusterRole/scaling-manager-keda-hpa-repair-role-${wns_suffix}:get,list,patch"
            binding="$(printf '%s\n' "$out" | yq -N -r "select(.kind == \"ClusterRoleBinding\" and .metadata.name == \"scaling-manager-keda-hpa-repair-rolebinding-${wns_suffix}\") | .roleRef.name + \" \" + .subjects[0].namespace + \"/\" + .subjects[0].name")"
            want_binding="scaling-manager-keda-hpa-repair-role-${wns_suffix} wns/scaling-manager-controller-manager"
            ;;
        *)
            want_grant="Role/scaling-manager-keda-hpa-repair-role:get,list,patch"
            binding="$(printf '%s\n' "$out" | yq -N -r 'select(.kind == "RoleBinding" and .metadata.name == "scaling-manager-keda-hpa-repair-rolebinding") | .roleRef.name + " " + .subjects[0].namespace + "/" + .subjects[0].name')"
            want_binding="scaling-manager-keda-hpa-repair-role wns/scaling-manager-controller-manager"
            ;;
    esac
    check "$overlay: KEDA_HPA_REPAIR=true grants get, list, patch on HPAs, in one role of its own" "$want_grant" \
        "$(printf '%s\n' "$out" | hpa_grants)"
    check "$overlay: KEDA_HPA_REPAIR=true binds that role to the controller" "$want_binding" "$binding"
    check "$overlay: KEDA_HPA_REPAIR=true turns the repair on in the Deployment" 'true' "$(printf '%s\n' "$out" | hpa_switch)"
done

# ----------------------------------------------------------------------------

if [ "$FAILED" -gt 0 ]; then
    printf '\n%d rendered-manifest assertion(s) failed.\n' "$FAILED" >&2
    exit 1
fi
if [ "$RAN" -ne "$EXPECT_CASES" ]; then
    printf '\n%d case(s) reported ok, %d expected -- the run did not finish.\n' \
        "$RAN" "$EXPECT_CASES" >&2
    exit 1
fi
printf '\nRendered manifest checks passed (%d cases).\n' "$RAN"
