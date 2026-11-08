#!/usr/bin/env bash
set -euo pipefail

echo "Waiting for the scaling manager controller to be ready..."
kubectl rollout status deployment -l app.kubernetes.io/name=scaling-manager -n "$LLMSM_NAMESPACE" --timeout=300s || true
kubectl get pods -n "$LLMSM_NAMESPACE"
