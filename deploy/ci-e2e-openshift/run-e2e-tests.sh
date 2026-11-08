#!/usr/bin/env bash
set -euo pipefail

echo "Running consolidated E2E tests on OpenShift with configuration:"
echo "  ENVIRONMENT: $ENVIRONMENT"
echo "  USE_SIMULATOR: $USE_SIMULATOR"
echo "  SCALE_TO_ZERO_ENABLED: $SCALE_TO_ZERO_ENABLED"
echo "  LLMSM_NAMESPACE: $LLMSM_NAMESPACE"
echo "  MONITORING_NAMESPACE: $MONITORING_NAMESPACE"
echo "  LLMD_NAMESPACE: $LLMD_NAMESPACE"
echo "  GATEWAY_NAME: $GATEWAY_NAME"
echo "  MODEL_ID: $MODEL_ID"
echo "  REQUEST_RATE: $REQUEST_RATE"
echo "  NUM_PROMPTS: $NUM_PROMPTS"
echo "  LLMSM_RELEASE_NAME: $LLMSM_RELEASE_NAME"
make test-e2e-full
