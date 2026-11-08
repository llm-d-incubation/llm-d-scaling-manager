#!/usr/bin/env bash
#
# llm-d-scaling-manager KIND Emulator Deployment Script
# Automated deployment of the scaling manager with llm-d infrastructure on Kind cluster with llm-d-inference-sim simulator
#
# Prerequisites:
# - kubectl installed and configured
# - helm installed
# - kind installed (for cluster creation)
# - Docker installed and running
#

set -e  # Exit on error
set -o pipefail  # Exit on pipe failure

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Configuration (Kind emulator). EPP deploy is via deploy/install-epp.sh — set LLM_D_ROUTER_VERSION / GAIE_VERSION / NAMESPACE there or in Makefile.
LLMSM_PROJECT=${LLMSM_PROJECT:-$PWD}
NAMESPACE_SUFFIX="sim"

# Namespaces
NAMESPACE="llm-d-$NAMESPACE_SUFFIX"
MONITORING_NAMESPACE="scaling-manager-monitoring"
# CONTROLLER_NAMESPACE is the same setting, as in deploy/install.sh.
LLMSM_NS=${LLMSM_NS:-${CONTROLLER_NAMESPACE:-"scaling-manager-system"}}

# Simulator image — must match defaultModelServiceSimulatorImage in test/e2e/fixtures/model_service_conventions.go
SIM_IMAGE=${SIM_IMAGE:-"ghcr.io/llm-d/llm-d-inference-sim:v0.9.0"}

# Warm pool images, pre-loaded for the same reason as the simulator's.
# WARMPOOL_EMULATOR_IMAGE must match warmPoolEmulatorImage in
# test/e2e/fixtures/warm_pool_builder.go; WARMPOOL_PROXY_IMAGE takes the same
# env var and default as the suite's own WarmPoolProxyImage (test/e2e/config.go),
# so overriding one overrides what is pre-loaded.
WARMPOOL_EMULATOR_IMAGE=${WARMPOOL_EMULATOR_IMAGE:-"registry.access.redhat.com/ubi9/python-311:latest"}
WARMPOOL_PROXY_IMAGE=${WARMPOOL_PROXY_IMAGE:-"ghcr.io/ev-shindin/warmpool-proxy:v11"}

# The scaling manager Configuration
LLMSM_RECONCILE_INTERVAL=${LLMSM_RECONCILE_INTERVAL:-"60s"} # the scaling manager controller reconcile interval - tests set 30s interval
LLMSM_LOG_LEVEL="debug" # the scaling manager log level set to debug for emulated environments
# Prometheus Configuration
PROMETHEUS_SVC_NAME="kube-prometheus-stack-prometheus"
PROMETHEUS_BASE_URL="https://$PROMETHEUS_SVC_NAME.$MONITORING_NAMESPACE.svc.cluster.local"
PROMETHEUS_PORT="9090"
PROMETHEUS_URL="$PROMETHEUS_BASE_URL:$PROMETHEUS_PORT"
PROMETHEUS_SECRET_NAME="prometheus-web-tls"

# KIND cluster configuration
CLUSTER_NAME=${CLUSTER_NAME:-"kind-scaling-manager-gpu-cluster"}
CLUSTER_NODES=${CLUSTER_NODES:-"3"}
# 16, matching the Makefile and setup.sh. This is the value CI actually gets --
# the Makefile does not export CLUSTER_GPUS to this script -- so leaving it at 4
# meant the e2e ran against a quarter of what a make-built cluster advertises.
CLUSTER_GPUS=${CLUSTER_GPUS:-"16"}
CLUSTER_GPU_TYPE=${CLUSTER_GPU_TYPE:-"mix"}

# Flags for deployment steps
CREATE_CLUSTER=${CREATE_CLUSTER:-false}

# Undeployment flags
DELETE_CLUSTER=${DELETE_CLUSTER:-false}

# Kind-specific prerequisites
REQUIRED_TOOLS=("kind")

# Function to check Kind emulator-specific prerequisites
# - checks for kind, kubectl and helm
# - creates Kind cluster if CREATE_CLUSTER=true, otherwise tries to use an existing cluster
# - loads the scaling manager image into Kind cluster
check_specific_prerequisites() {
    log_info "Checking Kubernetes-specific prerequisites..."
    
    local missing_tools=()
    
    # Check for required tools (including Kubernetes-specific ones)
    for tool in "${REQUIRED_TOOLS[@]}"; do
        if ! command -v "$tool" &> /dev/null; then
            missing_tools+=($tool)
        fi
    done
    
    if [ ${#missing_tools[@]} -ne 0 ]; then
        log_error "Missing required tools: ${missing_tools[*]}"
    fi
    
    # Create or use existing KIND cluster
    if [ "$CREATE_CLUSTER" = "true" ]; then
        # Check if the specific cluster exists - if so, delete and recreate
        if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
            log_info "KIND cluster '${CLUSTER_NAME}' already exists, tearing it down and recreating..."
            kind delete cluster --name "${CLUSTER_NAME}"
        else 
            log_info "KIND cluster '${CLUSTER_NAME}' not found, creating it..."
        fi
        create_kind_cluster

    else
        log_info "Cluster creation skipped (CREATE_CLUSTER=false)"
        # Verify the Kind cluster exists
        if ! kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
            log_error "KIND cluster '${CLUSTER_NAME}' not found and CREATE_CLUSTER=false"
        fi
        # Set kubectl context to the Kind cluster
        kubectl config use-context "kind-${CLUSTER_NAME}" &> /dev/null
    fi
    # Verify kubectl can connect to the cluster
    if ! kubectl cluster-info &> /dev/null; then
        log_error "Failed to connect to KIND cluster '${CLUSTER_NAME}'"
    fi
    log_success "Using KIND cluster '${CLUSTER_NAME}'"

    # Load the scaling manager image into KIND cluster
    load_image

    # Pre-load the simulator image so tests don't pull it cold (avoids PodReadyTimeout).
    load_sim_image

    # Same for the warm pool's images, for the same reason and with the same
    # symptom: the pool Pod is what a warm-pool spec waits on.
    load_warmpool_images

    log_success "All Kind emulated deployment prerequisites met"
}

# Creates Kind cluster using `setup.sh` script for GPU emulation
create_kind_cluster() {
    log_info "Creating KIND cluster with GPU emulation..."
    
    # Check if cluster already exists
    if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
        log_warning "KIND cluster '${CLUSTER_NAME}' already exists"
        log_info "Deleting existing cluster to create a fresh one..."
        kind delete cluster --name "${CLUSTER_NAME}"
    fi
    
    # Run setup.sh to create the cluster
    local SETUP_SCRIPT="${LLMSM_PROJECT}/deploy/kind-emulator/setup.sh"
    
    if [ ! -f "$SETUP_SCRIPT" ]; then
        log_error "Setup script not found at: $SETUP_SCRIPT"
        exit 1
    fi
    
    log_info "Running setup script with: cluster=$CLUSTER_NAME, nodes=$CLUSTER_NODES, gpus=$CLUSTER_GPUS, type=$CLUSTER_GPU_TYPE"
    bash "$SETUP_SCRIPT" -c "${CLUSTER_NAME}" -n "$CLUSTER_NODES" -g "$CLUSTER_GPUS" -t "$CLUSTER_GPU_TYPE"
    
    # Ensure kubectl context is set to the new cluster
    kubectl config use-context "kind-${CLUSTER_NAME}" &> /dev/null
    
    log_success "KIND cluster '${CLUSTER_NAME}' created successfully"
}

# Loads the scaling manager image into the Kind cluster.
# When pulling from a registry, we pull a single platform (KIND_IMAGE_PLATFORM) to avoid
# "content digest ... not found" errors from kind load (multi-platform manifests reference
# blobs not included in the export stream; see kubernetes-sigs/kind#3795, #3845).
load_image() {
    log_info "Loading the scaling manager image '$LLMSM_IMAGE_REPO:$LLMSM_IMAGE_TAG' into KIND cluster..."
    
    # If LLMSM_IMAGE_PULL_POLICY is IfNotPresent, skip pulling and use local image only
    if [ "$LLMSM_IMAGE_PULL_POLICY" = "IfNotPresent" ]; then
        log_info "Using local image only (LLMSM_IMAGE_PULL_POLICY=IfNotPresent)"
        
        # Check if the image exists locally
        if ! docker image inspect "$LLMSM_IMAGE_REPO:$LLMSM_IMAGE_TAG" >/dev/null 2>&1; then
            log_error "Image '$LLMSM_IMAGE_REPO:$LLMSM_IMAGE_TAG' not found locally - Please build the image first (e.g., 'make docker-build IMG=$LLMSM_IMAGE_REPO:$LLMSM_IMAGE_TAG')"
        else
            log_success "Found local image '$LLMSM_IMAGE_REPO:$LLMSM_IMAGE_TAG'"
        fi
    else
        # Pull a single-platform image so kind load does not hit "content digest not found"
        # (multi-platform manifests can reference blobs that are not in the docker save stream).
        local platform="${KIND_IMAGE_PLATFORM:-}"
        if [ -z "$platform" ]; then
            case "$(uname -m)" in
                aarch64|arm64) platform="linux/arm64" ;;
                *) platform="linux/amd64" ;;
            esac
        fi
        log_info "Pulling single-platform image for KIND (platform=$platform) to avoid load errors..."
        if ! docker pull --platform "$platform" "$LLMSM_IMAGE_REPO:$LLMSM_IMAGE_TAG"; then
            log_warning "Failed to pull image '$LLMSM_IMAGE_REPO:$LLMSM_IMAGE_TAG' (platform=$platform)"
            log_info "Attempting to use existing local image..."
            if ! docker image inspect "$LLMSM_IMAGE_REPO:$LLMSM_IMAGE_TAG" >/dev/null 2>&1; then
                log_error "Image '$LLMSM_IMAGE_REPO:$LLMSM_IMAGE_TAG' not found locally - Please build or pull the image"
                exit 1
            fi
        else
            log_success "Pulled image '$LLMSM_IMAGE_REPO:$LLMSM_IMAGE_TAG' (platform=$platform)"
        fi
    fi
    
    # Load the image into the KIND cluster via the shared helper.
    # Tries `kind load docker-image` first, falls back to crictl-per-node for the
    # containerd image store issue (kubernetes-sigs/kind#3795).
    local full_image="$LLMSM_IMAGE_REPO:$LLMSM_IMAGE_TAG"
    if ! _load_into_kind "$full_image"; then
        log_error "Failed to load the scaling manager image '$full_image' into KIND cluster '$CLUSTER_NAME'"
        exit 1
    fi
}

# _load_into_kind FULL_IMAGE
# Loads a pre-pulled image into the KIND cluster: tries `kind load docker-image` first,
# then falls back to crictl-per-node for the containerd image store issue
# (kubernetes-sigs/kind#3795). Returns 0 on success, 1 on failure.
_load_into_kind() {
    local full_image="$1"
    local load_stderr
    if load_stderr="$(kind load docker-image "$full_image" --name "$CLUSTER_NAME" 2>&1)"; then
        log_success "Image '$full_image' loaded into KIND cluster '$CLUSTER_NAME'"
        return 0
    fi

    if ! echo "$load_stderr" | grep -qiE "docker save|multi-?platform|manifest|content digest|no such image|not found"; then
        log_warning "'kind load docker-image' failed for '$full_image': $load_stderr"
        return 1
    fi

    # Reproduced with kind v0.25.0 and Docker 29 (containerd image store): an
    # image pulled with --platform is saved as its whole multi-platform index,
    # and the node's `ctr import --all-platforms` stops on a platform that was
    # never pulled ("content digest ...: not found"). It is not our image name
    # or tag. The fallback costs no extra disk: kind load also puts one copy on
    # every node, which is all a per-node pull stores.
    log_warning "'kind load docker-image' failed (containerd image store issue: $(printf '%s\n' "$load_stderr" | grep -v '^[[:space:]]*$' | tail -1)) — falling back to pulling directly into KIND nodes"
    local nodes
    nodes="$(kind get nodes --name "$CLUSTER_NAME")" || return 1
    for node in $nodes; do
        if ! docker exec "$node" crictl pull "$full_image" 2>&1; then
            log_warning "Failed to pre-pull '$full_image' on node '$node'"
            return 1
        fi
    done
    log_success "Image '$full_image' pulled directly into KIND cluster '$CLUSTER_NAME' nodes"
    return 0
}

# Pre-loads the llm-d-inference-sim image into the KIND cluster so tests that create
# model service Deployments don't pull it cold and hit PodReadyTimeout.
load_sim_image() {
    log_info "Pre-loading simulator image '$SIM_IMAGE' into KIND cluster..."

    local platform="${KIND_IMAGE_PLATFORM:-}"
    if [ -z "$platform" ]; then
        case "$(uname -m)" in
            aarch64|arm64) platform="linux/arm64" ;;
            *) platform="linux/amd64" ;;
        esac
    fi

    if ! docker pull --platform "$platform" "$SIM_IMAGE"; then
        log_warning "Failed to pull simulator image '$SIM_IMAGE' — tests may be slow on first run"
        return
    fi

    if _load_into_kind "$SIM_IMAGE"; then
        _drop_host_copy "$SIM_IMAGE"
    else
        log_warning "Failed to load simulator image into KIND cluster — tests may be slow on first run"
    fi
}

# _drop_host_copy IMAGE
# With KIND_PRUNE_HOST_IMAGES=true (CI sets it), removes the host Docker copy of
# a registry image once the KIND nodes hold their own. The nodes never read the
# host copy again, and on a CI runner it is disk the run needs: the warm pool's
# ubi9/python-311 alone is 1.55 GB unpacked. Off by default, so a local re-run
# does not pull it again. Never used for the scaling manager image, which may exist only on
# the host.
_drop_host_copy() {
    [ "${KIND_PRUNE_HOST_IMAGES:-false}" = "true" ] || return 0
    if docker image rm "$1" >/dev/null 2>&1; then
        log_info "Removed the host copy of '$1' (KIND_PRUNE_HOST_IMAGES=true); the KIND nodes keep theirs"
    else
        log_warning "Could not remove the host copy of '$1'; continuing"
    fi
}

# Pre-loads the warm pool's images, for the same reason as the simulator's: a
# warm-pool spec waits on the POOL Pod, and a cold pull is charged against that
# wait rather than against anything the spec is testing.
#
# Measured on a fresh cluster: the attribution spec failed after its full 300s
# with "pods=0 ... the pool never warmed the model, so there was never anything
# to lend", while the emulator image (402MB) was still being pulled.
#
# THAT PULL WAS NOT THE CAUSE, and this does not make that spec pass. Re-run on
# a cluster built from scratch WITH both images pre-loaded, it fails identically:
# kubelet reports the proxy image "already present on machine", both containers
# are Created and Started with zero restarts, and then "Readiness probe failed".
# The pool Pod never goes Ready, the controller cannot read its supervisor on
# :8001, and a Pod it cannot read is dropped from the observation -- which is
# what produces pods=0.
#
# Kept anyway, on its own merits: a 400MB pull on the critical path of a
# readiness wait is worth removing whether or not it is the failure of the day,
# which is the same reason load_sim_image exists. The fresh-cluster failure is
# still open; the next step is capturing the supervisor container's logs before
# AfterSuite deletes the Pod.
#
# Warnings, never errors: a pool image that cannot be pulled is a slow test, not
# a broken cluster, and the specs that use it skip on their own.
load_warmpool_images() {
    local platform="${KIND_IMAGE_PLATFORM:-}"
    if [ -z "$platform" ]; then
        case "$(uname -m)" in
            aarch64|arm64) platform="linux/arm64" ;;
            *) platform="linux/amd64" ;;
        esac
    fi

    local image
    for image in "$WARMPOOL_EMULATOR_IMAGE" "$WARMPOOL_PROXY_IMAGE"; do
        [ -n "$image" ] || continue
        log_info "Pre-loading warm pool image '$image' into KIND cluster..."
        if ! docker pull --platform "$platform" "$image"; then
            log_warning "Failed to pull warm pool image '$image' — warm pool tests may be slow on first run"
            continue
        fi
        if _load_into_kind "$image"; then
            _drop_host_copy "$image"
        else
            log_warning "Failed to load warm pool image '$image' into KIND cluster — warm pool tests may be slow on first run"
        fi
    done
}


_scaling_manager_deploy_lib="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../lib"
# shellcheck source=deploy_prometheus_kube_stack.sh
source "${_scaling_manager_deploy_lib}/deploy_prometheus_kube_stack.sh"
# shellcheck source=kube_like_adapter.sh
source "${_scaling_manager_deploy_lib}/kube_like_adapter.sh"

#### REQUIRED FUNCTION used by deploy/install.sh ####
delete_namespaces() {
    delete_namespaces_kube_like
    if [ "$DELETE_CLUSTER" = true ]; then
        delete_kind_cluster
    fi
}

# Deletes the Kind cluster
# Used when DELETE_CLUSTER=true by delete_namespaces()
delete_kind_cluster() {
    log_info "Deleting KIND cluster '${CLUSTER_NAME}'..."
    
    if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
        kind delete cluster --name "${CLUSTER_NAME}"
        log_success "KIND cluster '${CLUSTER_NAME}' deleted"
    else
        log_warning "KIND cluster '${CLUSTER_NAME}' not found"
    fi
}
