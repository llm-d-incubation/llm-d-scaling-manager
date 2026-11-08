#!/usr/bin/env bash
set -euo pipefail

# PR E2E tests must run on the cluster set aside for them, not on one reserved
# for nightly runs. EXPECTED_CLUSTER is a part of the expected cluster's API
# server URL (its host name, say); the workflow passes it from a repository
# variable so no cluster is named in the tree. A runner connected to any other
# cluster fails here, before anything is installed.
if [ -z "${EXPECTED_CLUSTER:-}" ]; then
  echo "::error::EXPECTED_CLUSTER is not set: set it to a part of the expected cluster's API server URL (repository variable E2E_OPENSHIFT_CLUSTER)."
  exit 1
fi
CLUSTER_API=$(kubectl cluster-info 2>/dev/null | head -1 | grep -oE 'https://[^ ]+' || true)
echo "Cluster API: ${CLUSTER_API:-unknown}"
case "$CLUSTER_API" in
  *"$EXPECTED_CLUSTER"*) ;;
  *)
    echo "::error::This runner is connected to ${CLUSTER_API:-an unknown cluster}, not the expected cluster (EXPECTED_CLUSTER=$EXPECTED_CLUSTER). PR CI should only use runners on that cluster."
    exit 1
    ;;
esac
echo "Cluster verified: the API server matches EXPECTED_CLUSTER"
