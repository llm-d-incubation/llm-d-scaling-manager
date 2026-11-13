#!/usr/bin/env bash
# Prepare and register model servers for scaling, from the release's tools image or a checkout.
#
# The entry point of the tools image (Dockerfile.tools). It runs the same
# scripts the make targets run, so an operator needs no checkout of the
# repository: only docker or podman, and a kubeconfig.
#
#   scaling-manager-tools prepull      apply|status|delete ...   hold an engine image on every GPU node (deploy/prepull.sh)
#   scaling-manager-tools weights      apply|status|delete ...   keep a model's weights on every GPU node's disk (deploy/weights.sh)
#   scaling-manager-tools engine-cache apply|status|delete|publish|keys|verify ...
#                                                                keep the engines' compile caches on every GPU node's disk (deploy/enginecache.sh)
#   scaling-manager-tools workload-patch -n NS [--controller-namespace CNS] [--apply] [--apply-weights] [--apply-engine-cache] [--out FILE]
#                                                                report and write what the model servers in NS still need (make workload-patch)
#   scaling-manager-tools scaledobjects-plan -n NS [--controller-namespace CNS] [--out FILE]
#                                                                write an editable ScaledObject plan for the model servers in NS; applies nothing
#   scaling-manager-tools scaledobjects-apply -n NS --plan FILE [--controller-namespace CNS]
#                                                                apply exactly that plan file (make scaledobjects-apply)
#   scaling-manager-tools utilization-share check|shadow|activate|status|off -n NS ...
#                                                                the utilization-share optimizer (deploy/utilization-share.sh)
#   scaling-manager-tools version                                the release, and the kubectl, jq and yq inside
#
# `<command> --help` prints that script's options. `apply --dry-run` prints the
# manifests instead of applying them, for review or for a GitOps repository.
#
# workload-patch and the scaledobjects commands read every LLMSM_* variable
# their make targets read (docs/reference/workload-preparation.md has the
# workload-patch table); pass them with `docker run -e`. Unlike the make
# targets they scan exactly the namespace -n names, and -n is required: a
# command that reads as scoped to one namespace never walks the cluster.
# --controller-namespace names where the controller runs when that is not NS
# (the scaler address the plan writes).
#
# The mapping from make targets:
#   make prepull IMAGES=<img> NAMESPACE=<ns>            prepull apply -n <ns> --image <img>
#   make weights WEIGHTS_MODEL=<id> WEIGHTS_PATH=<dir> WEIGHTS_IMAGE=<img> NAMESPACE=<ns>
#                                                       weights apply -n <ns> --model <id> --path <dir> --image <img>
#   make engine-cache ENGINE_CACHE_PATH=<dir> ENGINE_CACHE_IMAGE=<img> NAMESPACE=<ns>
#                                                       engine-cache apply -n <ns> --path <dir> --image <img>
#   make workload-patch NAMESPACE=<ns>                  workload-patch -n <ns>
#   make scaledobjects-plan LLMSM_DEFAULT_SO_PLAN=<f> NAMESPACE=<ns>
#                                                       scaledobjects-plan -n <ns> --out <f>
#   make scaledobjects-apply LLMSM_DEFAULT_SO_PLAN=<f> NAMESPACE=<ns>
#                                                       scaledobjects-apply -n <ns> --plan <f>
#   make utilization-share-<verb> NAMESPACE=<ns>        utilization-share <verb> -n <ns>
#   make <x>-status NAMESPACE=<ns>                      <x> status -n <ns>
#   make prepull-delete NAMESPACE=<ns>                  prepull delete -n <ns> --all   (IMAGES=<img>: --image <img>)
#   make weights-delete NAMESPACE=<ns>                  weights delete -n <ns> --all   (WEIGHTS_MODEL=<id>: --model <id>)
#   make engine-cache-delete NAMESPACE=<ns>             engine-cache delete -n <ns>
#
# Warm pools need a checkout: the plan's warmPools section is created by
# deploy/warmpool.sh, which this image does not carry, and scaledobjects-apply
# refuses a plan that sets one to apply: yes.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

usage() {
    sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//; $d'
}

die() {
    echo "scaling-manager-tools: $*" >&2
    exit 2
}

# version prints what is installed, read from the binaries, never retyped.
cmd_version() {
    local v="unreleased (a checkout)"
    [ -f "$HERE/VERSION" ] && v="$(cat "$HERE/VERSION")"
    echo "scaling-manager-tools ${v}"
    local k="not found" j="not found" y="not found"
    command -v kubectl >/dev/null 2>&1 && k="$(kubectl version --client -o json 2>/dev/null | jq -r '.clientVersion.gitVersion' 2>/dev/null)"
    command -v jq >/dev/null 2>&1 && j="$(jq --version 2>/dev/null)"
    command -v yq >/dev/null 2>&1 && y="$(yq --version 2>/dev/null | sed 's/.* version //')"
    printf 'kubectl %s\n' "${k:-unreadable}"
    printf 'jq      %s\n' "${j:-unreadable}"
    printf 'yq      %s\n' "${y:-unreadable}"
    printf 'bash    %s\n' "${BASH_VERSION}"
}

# check_ns <what> <value>: a namespace name, or refuse.
check_ns() {
    case "$2" in
        ""|*[!a-z0-9-]*|-*|*-) die "$1: '$2' is not a namespace name" ;;
    esac
}

# scope_to <ns> [<controller ns>]: what `make <target> NAMESPACE=<ns>` exports,
# with the scan pinned to <ns> (LLMSM_DEFAULT_SO_NS) whatever LLMSM_SCOPE says.
scope_to() {
    export NAMESPACE="$1"
    export LLMSM_DEFAULT_SO_NS="$1"
    export LLMSM_SCOPE="${LLMSM_SCOPE:-namespace}"
    [ -z "${2:-}" ] || export LLMSM_NS="$2"
    # The libraries run as the make recipes run them, under `bash -c`: no
    # errexit, nounset or pipefail. They test statuses themselves, and under
    # errexit the first grep that matches nothing ended the plan, exit 1,
    # after "Scanning for llm-d model servers..." and nothing more.
    set +euo pipefail
    # The libraries print in colour and read these names.
    # shellcheck disable=SC2034
    BLUE='\033[0;34m' GREEN='\033[0;32m' YELLOW='\033[1;33m' RED='\033[0;31m' NC='\033[0m'
}

# cmd_workload_patch runs what `make workload-patch NAMESPACE=<ns>` runs, scoped
# to exactly <ns>, with the make variables as flags.
cmd_workload_patch() {
    local ns="" ctl=""
    while [ $# -gt 0 ]; do
        case "$1" in
            -n|--namespace|--out|--controller-namespace)
                [ $# -ge 2 ] || die "$1 needs a value" ;;
        esac
        case "$1" in
            -n|--namespace) ns="$2"; shift 2 ;;
            --controller-namespace) ctl="$2"; shift 2 ;;
            --out) export LLMSM_WORKLOAD_PATCH_FILE="$2"; shift 2 ;;
            --apply) export LLMSM_WORKLOAD_PATCH_APPLY=true; shift ;;
            --apply-weights) export LLMSM_WORKLOAD_PATCH_APPLY_WEIGHTS=true; shift ;;
            --apply-engine-cache) export LLMSM_WORKLOAD_PATCH_APPLY_ENGINE_CACHE=true; shift ;;
            -h|--help)
                sed -n '/^#   scaling-manager-tools workload-patch/,/^#   scaling-manager-tools scaledobjects-plan/p' "${BASH_SOURCE[0]}" | sed '$d; s/^# \{0,1\}//'
                echo "  --apply                 also apply the drain half to the live workloads (LLMSM_WORKLOAD_PATCH_APPLY=true)"
                echo "  --apply-weights         also apply the weights volume (LLMSM_WORKLOAD_PATCH_APPLY_WEIGHTS=true)"
                echo "  --apply-engine-cache    also apply the engine-cache volume (LLMSM_WORKLOAD_PATCH_APPLY_ENGINE_CACHE=true)"
                echo "  --out FILE              where the patch is written (default scaling-manager-workload-patch.yaml)"
                return 0 ;;
            *) die "workload-patch: unknown option $1 (see workload-patch --help)" ;;
        esac
    done
    [ -n "$ns" ] || die "workload-patch needs -n <namespace>: the namespace whose model servers it reads"
    check_ns workload-patch "$ns"
    [ -z "$ctl" ] || check_ns "workload-patch --controller-namespace" "$ctl"
    case "${LLMSM_WORKLOAD_PATCH_APPLY_WEIGHTS:-}${LLMSM_WORKLOAD_PATCH_APPLY_ENGINE_CACHE:-}" in
        *true*) [ "${LLMSM_WORKLOAD_PATCH_APPLY:-}" = true ] \
            || die "workload-patch: --apply-weights and --apply-engine-cache take effect only with --apply" ;;
    esac
    scope_to "$ns" "$ctl"
    # shellcheck source=lib/common.sh
    source "$HERE/lib/common.sh"
    # shellcheck source=lib/scaledobject.sh
    source "$HERE/lib/scaledobject.sh"
    llmsm_bootstrap_env
    llmsm_workload_patch
}

# cmd_scaledobjects <plan|apply> ...: `make scaledobjects-plan` and
# `make scaledobjects-apply`, scoped to exactly -n. apply takes only an
# existing plan file: without one the library would discover and apply every
# model server it finds, which is not what a command naming a file means.
cmd_scaledobjects() {
    local verb="$1" ns="" ctl="" file=""
    shift
    while [ $# -gt 0 ]; do
        case "$1" in
            -n|--namespace|--controller-namespace|--out|--plan)
                [ $# -ge 2 ] || die "$1 needs a value" ;;
        esac
        case "$1" in
            -n|--namespace) ns="$2"; shift 2 ;;
            --controller-namespace) ctl="$2"; shift 2 ;;
            --out) [ "$verb" = plan ] || die "scaledobjects-apply takes --plan, not --out"; file="$2"; shift 2 ;;
            --plan) [ "$verb" = apply ] || die "scaledobjects-plan takes --out, not --plan"; file="$2"; shift 2 ;;
            -h|--help)
                sed -n "/^#   scaling-manager-tools scaledobjects-${verb}/,+1p" "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
                echo "  the plan's own comments explain every field: set apply: yes, no or adopt, the modelID and the bounds"
                echo "  --controller-namespace CNS   where the controller runs, when it is not NS (a cluster-wide install: scaling-manager-system)"
                echo "  warm pools need a checkout: deploy/warmpool.sh creates them, and this image does not carry it"
                return 0 ;;
            *) die "scaledobjects-${verb}: unknown option $1 (see scaledobjects-${verb} --help)" ;;
        esac
    done
    [ -n "$ns" ] || die "scaledobjects-${verb} needs -n <namespace>: the namespace whose model servers it registers"
    check_ns "scaledobjects-${verb}" "$ns"
    [ -z "$ctl" ] || check_ns "scaledobjects-${verb} --controller-namespace" "$ctl"
    if [ "$verb" = plan ]; then
        file="${file:-scaling-manager-scaledobjects-plan.yaml}"
    else
        [ -n "$file" ] || die "scaledobjects-apply needs --plan <file>: the plan scaledobjects-plan wrote, edited"
        [ -f "$file" ] || die "scaledobjects-apply: no plan at '$file' (scaledobjects-plan writes one)"
        if [ ! -f "$HERE/warmpool.sh" ]; then
            local pools
            pools="$(yq '[(.warmPools // [])[] | select((.apply // "no" | tostring | downcase) == "yes" or (.apply // "no" | tostring | downcase) == "true") | .name] | join(", ")' "$file" 2>/dev/null)" \
                || die "scaledobjects-apply: could not read the plan $file as YAML"
            [ -z "$pools" ] || die "scaledobjects-apply: the plan sets warm pool(s) $pools to apply: yes. Warm pools need a checkout (deploy/warmpool.sh creates them, and this image does not carry it): set them to apply: \"no\" here, and create them from a checkout"
        fi
    fi
    scope_to "$ns" "$ctl"
    export LLMSM_DEFAULT_SO_PLAN="$file"
    # shellcheck source=lib/common.sh
    source "$HERE/lib/common.sh"
    if [ "$verb" = plan ]; then
        export LLMSM_DEFAULT_SO=plan
        # shellcheck source=lib/scaledobject.sh
        source "$HERE/lib/scaledobject.sh"
        llmsm_bootstrap_env
        install_default_scaledobjects
        return
    fi
    export LLMSM_DEFAULT_SO=true
    # shellcheck source=lib/constants.sh
    source "$HERE/lib/constants.sh"
    # shellcheck source=lib/scaledobject.sh
    source "$HERE/lib/scaledobject.sh"
    # shellcheck source=lib/verify.sh
    source "$HERE/lib/verify.sh"
    # shellcheck source=lib/scaler_tls.sh
    source "$HERE/lib/scaler_tls.sh"
    # shellcheck disable=SC2034
    KEDA_NAMESPACE_EXPLICIT=false
    # shellcheck disable=SC2034
    [ -z "${KEDA_NAMESPACE:-}" ] || KEDA_NAMESPACE_EXPLICIT=true
    llmsm_bootstrap_env
    local rc=0
    install_default_scaledobjects || rc=$?
    # As make scaledobjects-apply: re-wire what KEDA left on the CPU default.
    DRY_RUN=false llmsm_rewire_scaledobjects || true
    return "$rc"
}

CMD="${1:-}"
[ $# -eq 0 ] || shift
case "$CMD" in
    ""|-h|--help|help) usage ;;
    version|--version) cmd_version ;;
    prepull) exec bash "$HERE/prepull.sh" "$@" ;;
    weights) exec bash "$HERE/weights.sh" "$@" ;;
    engine-cache) exec bash "$HERE/enginecache.sh" "$@" ;;
    utilization-share) exec bash "$HERE/utilization-share.sh" "$@" ;;
    workload-patch) cmd_workload_patch "$@" ;;
    scaledobjects-plan) cmd_scaledobjects plan "$@" ;;
    scaledobjects-apply) cmd_scaledobjects apply "$@" ;;
    *) echo "scaling-manager-tools: unknown command '$CMD'" >&2; usage >&2; exit 2 ;;
esac
