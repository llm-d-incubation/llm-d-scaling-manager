# Sourced by a check that reads no cluster: names the real kubectl for
# hack/offline-kubectl/kubectl (OFFLINE_KUBECTL_REAL, unless lint-deploy-scripts already
# did) and puts that directory first on PATH, so every kubectl the check and what it
# runs call either renders offline or fails.
OFFLINE_KUBECTL_REAL="${OFFLINE_KUBECTL_REAL:-$(command -v kubectl || true)}"
export OFFLINE_KUBECTL_REAL
PATH="$(cd "$(dirname "${BASH_SOURCE[0]}")/../offline-kubectl" && pwd):$PATH"
export PATH
