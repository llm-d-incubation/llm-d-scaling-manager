#!/usr/bin/env bash
set -euo pipefail

 echo "Deploying the scaling manager and llm-d infrastructure..."
echo "  ACCELERATOR_TYPE: $ACCELERATOR_TYPE"
echo "  NAMESPACE: $NAMESPACE"
echo "  LLMSM_NS: $LLMSM_NS"
echo "  LLMSM_IMAGE_TAG: $LLMSM_IMAGE_TAG"
echo "  CONTROLLER_INSTANCE: $CONTROLLER_INSTANCE"
echo "  HF token configuration: ✓"
# Base monitoring + the scaling manager + scaler backend.
./deploy/install.sh --environment openshift

# Create llm-d namespace and HuggingFace token secret.
kubectl create namespace "$LLMD_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -
kubectl create secret generic llm-d-hf-token \
  --from-literal="HF_TOKEN=${HF_TOKEN}" \
  -n "$LLMD_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

# OpenShift: install GAIE CRDs before standalone chart.
kubectl apply -k "https://github.com/kubernetes-sigs/gateway-api-inference-extension/config/crd/?ref=${GAIE_VERSION}" || true

# EPP / scheduler via install-epp.sh (handles llm-d-router-standalone chart +
# flowControl feature gate + tokenreview RBAC).
LLMSM_PROJECT="$GITHUB_WORKSPACE" \
NAMESPACE="$LLMD_NAMESPACE" \
GAIE_VERSION="$GAIE_VERSION" \
LLM_D_ROUTER_VERSION="$LLM_D_ROUTER_VERSION" \
ENVIRONMENT=openshift \
ENABLE_SCALE_TO_ZERO=true \
SKIP_CLUSTER_CRDS=true \
./deploy/install-epp.sh

