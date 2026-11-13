#!/usr/bin/env bash
# Prepare model servers for scaling, from the release's tools image or a checkout.
#
# The entry point of the tools image (Dockerfile.tools). It runs the same
# scripts the make targets run, so an operator needs no checkout of the
# repository: only docker or podman, and a kubeconfig.
#
#   scaling-manager-tools prepull      apply|status|delete ...   hold an engine image on every GPU node (deploy/prepull.sh)
#   scaling-manager-tools weights      apply|status|delete ...   keep a model's weights on every GPU node's disk (deploy/weights.sh)
#   scaling-manager-tools engine-cache apply|status|delete|publish|keys|verify ...
#                                                                keep the engines' compile caches on every GPU node's disk (deploy/enginecache.sh)
#   scaling-manager-tools workload-patch -n NS [--apply] [--apply-weights] [--apply-engine-cache] [--out FILE]
#                                                                report and write what the model servers in NS still need (make workload-patch)
#   scaling-manager-tools version                                the release, and the kubectl, jq and yq inside
#
# `<command> --help` prints that script's options. `apply --dry-run` prints the
# manifests instead of applying them, for review or for a GitOps repository.
#
# workload-patch reads every LLMSM_* variable `make workload-patch` reads
# (docs/reference/workload-preparation.md has the table); pass them with
# `docker run -e`. Unlike the make target it scans exactly the namespace -n
# names, and -n is required: a command that reads as scoped to one namespace
# never walks the cluster.
#
# The mapping from make targets:
#   make prepull IMAGES=<img> NAMESPACE=<ns>            prepull apply -n <ns> --image <img>
#   make weights WEIGHTS_MODEL=<id> WEIGHTS_PATH=<dir> WEIGHTS_IMAGE=<img> NAMESPACE=<ns>
#                                                       weights apply -n <ns> --model <id> --path <dir> --image <img>
#   make engine-cache ENGINE_CACHE_PATH=<dir> ENGINE_CACHE_IMAGE=<img> NAMESPACE=<ns>
#                                                       engine-cache apply -n <ns> --path <dir> --image <img>
#   make workload-patch NAMESPACE=<ns>                  workload-patch -n <ns>
#   make <x>-status / <x>-delete NAMESPACE=<ns>         <x> status|delete -n <ns>
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
    printf 'kubectl %s\n' "$(kubectl version --client -o json 2>/dev/null | jq -r '.clientVersion.gitVersion' 2>/dev/null || echo 'not found')"
    printf 'jq      %s\n' "$(jq --version 2>/dev/null || echo 'not found')"
    printf 'yq      %s\n' "$(yq --version 2>/dev/null | sed 's/.* version //' || echo 'not found')"
    printf 'bash    %s\n' "${BASH_VERSION}"
}

# cmd_workload_patch runs what `make workload-patch NAMESPACE=<ns>` runs, scoped
# to exactly <ns> (LLMSM_DEFAULT_SO_NS), with the make variables as flags.
cmd_workload_patch() {
    local ns=""
    while [ $# -gt 0 ]; do
        case "$1" in
            -n|--namespace|--out)
                [ $# -ge 2 ] || die "$1 needs a value" ;;
        esac
        case "$1" in
            -n|--namespace) ns="$2"; shift 2 ;;
            --out) export LLMSM_WORKLOAD_PATCH_FILE="$2"; shift 2 ;;
            --apply) export LLMSM_WORKLOAD_PATCH_APPLY=true; shift ;;
            --apply-weights) export LLMSM_WORKLOAD_PATCH_APPLY_WEIGHTS=true; shift ;;
            --apply-engine-cache) export LLMSM_WORKLOAD_PATCH_APPLY_ENGINE_CACHE=true; shift ;;
            -h|--help)
                sed -n '/^#   scaling-manager-tools workload-patch/,/^#   scaling-manager-tools version/p' "${BASH_SOURCE[0]}" | sed '$d; s/^# \{0,1\}//'
                echo "  --apply                 also apply the drain half to the live workloads (LLMSM_WORKLOAD_PATCH_APPLY=true)"
                echo "  --apply-weights         also apply the weights volume (LLMSM_WORKLOAD_PATCH_APPLY_WEIGHTS=true)"
                echo "  --apply-engine-cache    also apply the engine-cache volume (LLMSM_WORKLOAD_PATCH_APPLY_ENGINE_CACHE=true)"
                echo "  --out FILE              where the patch is written (default scaling-manager-workload-patch.yaml)"
                return 0 ;;
            *) die "workload-patch: unknown option $1 (see workload-patch --help)" ;;
        esac
    done
    [ -n "$ns" ] || die "workload-patch needs -n <namespace>: the namespace whose model servers it reads"
    case "$ns" in
        *[!a-z0-9-]*|-*|*-) die "workload-patch: '$ns' is not a namespace name" ;;
    esac
    case "${LLMSM_WORKLOAD_PATCH_APPLY_WEIGHTS:-}${LLMSM_WORKLOAD_PATCH_APPLY_ENGINE_CACHE:-}" in
        *true*) [ "${LLMSM_WORKLOAD_PATCH_APPLY:-}" = true ] \
            || die "workload-patch: --apply-weights and --apply-engine-cache take effect only with --apply" ;;
    esac
    export NAMESPACE="$ns"
    export LLMSM_DEFAULT_SO_NS="$ns"
    export LLMSM_SCOPE="${LLMSM_SCOPE:-namespace}"
    # The libraries print in colour and read these names.
    # shellcheck disable=SC2034
    BLUE='\033[0;34m' GREEN='\033[0;32m' YELLOW='\033[1;33m' RED='\033[0;31m' NC='\033[0m'
    # shellcheck source=lib/common.sh
    source "$HERE/lib/common.sh"
    # shellcheck source=lib/scaledobject.sh
    source "$HERE/lib/scaledobject.sh"
    llmsm_bootstrap_env
    llmsm_workload_patch
}

CMD="${1:-}"
[ $# -eq 0 ] || shift
case "$CMD" in
    ""|-h|--help|help) usage ;;
    version|--version) cmd_version ;;
    prepull) exec bash "$HERE/prepull.sh" "$@" ;;
    weights) exec bash "$HERE/weights.sh" "$@" ;;
    engine-cache) exec bash "$HERE/enginecache.sh" "$@" ;;
    workload-patch) cmd_workload_patch "$@" ;;
    *) echo "scaling-manager-tools: unknown command '$CMD'" >&2; usage >&2; exit 2 ;;
esac
