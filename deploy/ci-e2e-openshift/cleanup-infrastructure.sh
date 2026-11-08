#!/usr/bin/env bash
set -euo pipefail

echo "Cleaning up ALL test infrastructure..."
echo "  LLMD_NAMESPACE: $LLMD_NAMESPACE"
echo "  LLMSM_NAMESPACE: $LLMSM_NAMESPACE"

echo "Removing cluster-scoped the scaling manager resources..."
kubectl delete clusterrole,clusterrolebinding -l app.kubernetes.io/name=scaling-manager --ignore-not-found || true

echo "Uninstalling llm-d helm releases..."
for release in $(helm list -n "$LLMD_NAMESPACE" -q 2>/dev/null); do
  echo "  Uninstalling release: $release"
  helm uninstall "$release" -n "$LLMD_NAMESPACE" --ignore-not-found --wait --timeout 60s || true
done

echo "Deleting llm-d namespace $LLMD_NAMESPACE..."
kubectl delete namespace "$LLMD_NAMESPACE" --ignore-not-found --timeout=120s || true

echo "Deleting the scaling manager namespace $LLMSM_NAMESPACE..."
kubectl delete namespace "$LLMSM_NAMESPACE" --ignore-not-found --timeout=120s || true

echo "Cleanup complete"
