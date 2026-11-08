#!/usr/bin/env bash
#
# EPP and InferencePool setup for all environments (kind-emulator, kubernetes, openshift).
# Installs Gateway API CRDs, GAIE CRDs, and the llm-d-router-standalone chart (EPP + InferencePool).
#
# Required vars: LLM_D_ROUTER_VERSION, GAIE_VERSION, NAMESPACE
#   LLM_D_ROUTER_VERSION  — chart + EPP image tag from llm-d/llm-d-router (e.g. v0.9.0)
#   GAIE_VERSION          — kubernetes-sigs GAIE CRDs ref (e.g. v1.5.0)
# Required funcs: log_info, log_success, log_warning
#

GATEWAY_API_VERSION=${GATEWAY_API_VERSION:-"v1.2.0"}
# Pinned HERE, not only in the Makefile. install.sh is a supported entry point and
# the guides invoke it directly, where a Makefile-only default leaves the ref EMPTY
# — `?ref=` resolves to the project's default branch, so a direct install pulled
# whatever was on GAIE main that morning onto the user's cluster.
GAIE_VERSION=${GAIE_VERSION:-"v1.5.0"}

# install_inference_crds installs the Gateway API and GAIE CRDs IF THEY ARE ABSENT.
#
# CRDs are cluster-scoped and shared. On an existing llm-d cluster they are already
# there, installed by llm-d at a version llm-d chose — and `kubectl apply` is not
# the no-op the old comment here claimed: it REPLACES the spec, so installing the scaling manager
# silently rewrote a running llm-d's CRDs to our pins. Downgrading a served version
# out from under live InferencePools breaks the cluster's own controllers, and
# nothing in this script would have said so.
#
# So the default is to install only what is missing. Adopting the cluster's existing
# version is also the correct outcome for the scaling manager: it only ever READS these types.
#   CRD_INSTALL=if-missing  (default) install absent CRDs, report and keep present ones
#   CRD_INSTALL=always      apply unconditionally — for a cluster you own
#   CRD_INSTALL=never       assume they are present (same as the old SKIP_CLUSTER_CRDS=true)
install_inference_crds() {
    local mode="${CRD_INSTALL:-if-missing}"
    # SKIP_CLUSTER_CRDS predates this and still works.
    [ "${SKIP_CLUSTER_CRDS:-false}" = "true" ] && mode="never"

    if [ "$mode" = "never" ]; then
        log_info "Skipping CRD installation (CRD_INSTALL=never) — assuming Gateway API and GAIE CRDs are present"
        return 0
    fi

    _install_crd_group "Gateway API" "${GATEWAY_API_VERSION}" \
        "gateways.gateway.networking.k8s.io" \
        -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml"

    _install_crd_group "GAIE" "${GAIE_VERSION}" \
        "inferencepools.inference.networking.x-k8s.io" \
        -k "https://github.com/kubernetes-sigs/gateway-api-inference-extension/config/crd/?ref=${GAIE_VERSION}"
}

# _install_crd_group applies one CRD set, honouring CRD_INSTALL.
# Args: <label> <version> <probe-crd> <kubectl apply args...>
_install_crd_group() {
    local label="$1" version="$2" probe="$3"
    shift 3

    if [ "${CRD_INSTALL:-if-missing}" != "always" ] && kubectl get crd "$probe" >/dev/null 2>&1; then
        local established
        established=$(kubectl get crd "$probe" -o jsonpath='{.spec.versions[*].name}' 2>/dev/null || true)
        log_info "${label} CRDs already on this cluster (served versions: ${established:-unknown}) — keeping them, not applying ${version}."
        log_info "  the scaling manager only reads these types, so the cluster's own version is the right one. Pass CRD_INSTALL=always to overwrite (this can break other controllers using them)."
        return 0
    fi

    log_info "Installing ${label} CRDs (${version})..."
    kubectl apply "$@" || log_warning "${label} CRD install returned non-zero — check whether they are usable before continuing"
}

deploy_epp() {
    log_info "Deploying EPP infrastructure (llm-d-router-standalone chart ${LLM_D_ROUTER_VERSION}, GAIE CRDs ${GAIE_VERSION})..."

    local _lib_dir
    _lib_dir="$(dirname "${BASH_SOURCE[0]}")"

    # One implementation, called from both places. This block used to be a second,
    # slightly different copy of install_inference_crds: only this one honoured
    # SKIP_CLUSTER_CRDS, so the earlier call from deploy_scaling_manager_prerequisites applied
    # the CRDs anyway on exactly the shared clusters the flag exists to protect.
    install_inference_crds

    # llm-d namespace and dummy HF token secret for emulated environments.
    log_info "Creating llm-d namespace ($NAMESPACE)..."
    kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -
    # Create HF token secret only if it does not already exist (preserves real token on OpenShift CI).
    if ! kubectl get secret llm-d-hf-token -n "$NAMESPACE" &>/dev/null; then
        local hf_token="${HF_TOKEN:-dummy-token}"
        kubectl create secret generic llm-d-hf-token \
            --from-literal="HF_TOKEN=${hf_token}" \
            -n "$NAMESPACE"
        log_info "Created llm-d-hf-token secret in $NAMESPACE"
    else
        log_info "llm-d-hf-token secret already exists in $NAMESPACE — skipping"
    fi

    # llm-d-router-standalone chart — bundles EPP + Envoy proxy; no external
    # gateway controller needed. Chart + EPP image version is LLM_D_ROUTER_VERSION.
    # Override chart's production resource requests (4CPU/8Gi per container)
    # to fit a kind cluster.
    log_info "Installing llm-d-router-standalone chart (release=optimized-baseline, version=${LLM_D_ROUTER_VERSION})..."

    # When scale-to-zero is enabled, add flowControl feature gate to EPP config so the
    # scale-from-zero engine can read inference_extension_flow_control_queue_size metrics.
    local extra_helm_args=() epp_preexisting=false
    if [ "${ENABLE_SCALE_TO_ZERO:-false}" = "true" ]; then
        log_info "ENABLE_SCALE_TO_ZERO=true: enabling EPP flowControl feature gate..."
        extra_helm_args=(-f "$_lib_dir/epp-flow-control.values.yaml")
        # Asked BEFORE the upgrade, because afterwards the answer is always yes.
        if kubectl get deployment optimized-baseline-epp -n "$NAMESPACE" >/dev/null 2>&1; then
            epp_preexisting=true
        fi
    fi

    helm upgrade --install optimized-baseline \
        oci://ghcr.io/llm-d/charts/llm-d-router-standalone \
        -f "$_lib_dir/epp-base.values.yaml" \
        -f "$_lib_dir/epp-optimized-baseline.values.yaml" \
        "${extra_helm_args[@]}" \
        --set router.epp.resources.requests.cpu=100m \
        --set router.epp.resources.requests.memory=256Mi \
        --set router.epp.resources.limits.cpu=500m \
        --set router.epp.resources.limits.memory=512Mi \
        --set router.proxy.resources.requests.cpu=100m \
        --set router.proxy.resources.requests.memory=128Mi \
        --set router.proxy.resources.limits.cpu=500m \
        --set router.proxy.resources.limits.memory=256Mi \
        -n "$NAMESPACE" --version "$LLM_D_ROUTER_VERSION" --create-namespace

    # The flowControl gate lives in a ConfigMap, and the chart's pod template has no
    # checksum annotation over it -- so `helm upgrade` rewrites the ConfigMap, the
    # Deployment spec is unchanged, and an EPP that was already running keeps its
    # old config. It stays up, reports healthy, and exports no flow-control metrics.
    #
    # That is not cosmetic here: the queue depth those metrics carry is the ONLY
    # evidence of demand for a model parked at zero replicas, so scale-from-zero
    # silently never fires. Reported by an external evaluation, which had to run
    # `kubectl rollout restart` by hand to get the metrics at all.
    #
    # Only when the EPP predates this run: a freshly created one already has the
    # gate, and restarting it would just add a minute to every clean install.
    if [ "$epp_preexisting" = "true" ]; then
        log_info "Restarting the pre-existing EPP so it picks up the flowControl config (Helm does not)."
        kubectl rollout restart deployment/optimized-baseline-epp -n "$NAMESPACE" || \
            log_warning "Could not restart the EPP; if flow-control metrics are missing, restart it by hand."
    fi

    # Grant EPP SA permission to create tokenreviews/subjectaccessreviews so its
    # metrics endpoint authentication works (otherwise /metrics returns 500).
    # One binding PER NAMESPACE: a single cluster-wide name was rebound to the
    # last namespace installed, and every earlier EPP's /metrics went to 500.
    log_info "Applying EPP tokenreview RBAC..."
    kubectl apply -f "$_lib_dir/epp-tokenreview-rbac.yaml"
    kubectl create clusterrolebinding "optimized-baseline-epp-tokenreview-$NAMESPACE" \
        --clusterrole=optimized-baseline-epp-tokenreview \
        --serviceaccount="$NAMESPACE:optimized-baseline-epp" \
        --dry-run=client -o yaml | kubectl apply -f -

    # Wait for EPP to be available.
    log_info "Waiting for EPP deployment (optimized-baseline-epp) in $NAMESPACE..."
    kubectl wait --for=condition=Available deployment/optimized-baseline-epp \
        -n "$NAMESPACE" --timeout=120s || \
        log_warning "EPP not ready yet — check 'kubectl get pods -n $NAMESPACE'"

    log_success "EPP infrastructure deployed (llm-d-router-standalone ${LLM_D_ROUTER_VERSION})"
}

undeploy_epp() {
    log_info "Removing EPP infrastructure..."
    helm uninstall optimized-baseline -n "$NAMESPACE" --ignore-not-found 2>/dev/null || true
    kubectl delete secret llm-d-hf-token -n "$NAMESPACE" --ignore-not-found 2>/dev/null || true
    kubectl delete clusterrolebinding "optimized-baseline-epp-tokenreview-$NAMESPACE" --ignore-not-found 2>/dev/null || true
    # The name every namespace once shared.
    kubectl delete clusterrolebinding optimized-baseline-epp-tokenreview --ignore-not-found 2>/dev/null || true
    # The role is shared; it goes with the last binding that uses it.
    if ! kubectl get clusterrolebindings -o jsonpath='{.items[*].roleRef.name}' 2>/dev/null \
        | tr ' ' '\n' | grep -qx optimized-baseline-epp-tokenreview; then
        kubectl delete clusterrole optimized-baseline-epp-tokenreview --ignore-not-found 2>/dev/null || true
    fi
    log_success "EPP infrastructure removed"
}
