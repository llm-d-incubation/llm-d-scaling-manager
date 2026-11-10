#!/usr/bin/env bash
#
# llm-d-scaling-manager infrastructure bootstrap: optional the scaling manager controller,
# Prometheus monitoring stack, and scaler backend (KEDA).
#
# For llm-d (gateway, EPP, ModelService), see the llm-d project guides at https://github.com/llm-d/llm-d.
# For EPP setup (all environments), run deploy/install-epp.sh after this script.
#
# Prerequisites:
# - kubectl and helm installed
# - Cluster credentials configured
#

set -e  # Exit on error
set -o pipefail  # Exit on pipe failure

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Configuration
LLMSM_PROJECT=${LLMSM_PROJECT:-$PWD}

# Namespaces
#
# NAMESPACE is llm-d's own variable: its guides say `export NAMESPACE=…` and pass
# `-n ${NAMESPACE}` to every command. This repo used to call the same thing
# LLMD_NS — a second name it invented for a namespace llm-d already named — which
# is accepted here for one release so nobody's scripts break on the rename.
if [ -n "${LLMD_NS:-}" ] && [ -z "${NAMESPACE:-}" ]; then
    NAMESPACE="$LLMD_NS"
    echo -e "\033[1;33m[WARNING]\033[0m LLMD_NS is deprecated and will be removed; use NAMESPACE (llm-d's own variable). Using NAMESPACE=$NAMESPACE" >&2
fi
# What the CALLER actually set, captured before the defaults below make that
# unknowable. llmsm_resolve_namespace uses these: `export NAMESPACE=…` has to reach
# every entry point — install, undeploy and check alike — and it cannot do that
# from the make targets, which is where this resolution used to live. It was in
# only 12 of them, so `NAMESPACE=x make undeploy-scaling-manager-on-k8s` uninstalled from the
# DEFAULT namespace while the install had gone to x.
# CONTROLLER_NAMESPACE is the same setting under the name the utilization-share
# commands take: either one names where the controller runs, and LLMSM_NS wins
# when both are set, so one exported variable reaches every step.
LLMSM_NS=${LLMSM_NS:-${CONTROLLER_NAMESPACE:-}}
LLMSM_NS_EXPLICIT=${LLMSM_NS:-}
NAMESPACE_EXPLICIT=${NAMESPACE:-}
export LLMSM_NS_EXPLICIT NAMESPACE_EXPLICIT
NAMESPACE=${NAMESPACE:-"llm-d-optimized-baseline"}
MONITORING_NAMESPACE=${MONITORING_NAMESPACE:-"scaling-manager-monitoring"}
LLMSM_NS=${LLMSM_NS:-"scaling-manager-system"}
# Set by the caller, the Prometheus TLS Secret is where the controller's CA is
# copied from (llmsm_prometheus_ca_source); unset, the Prometheus CR names it.
PROMETHEUS_SECRET_NAME_EXPLICIT=${PROMETHEUS_SECRET_NAME:-}
PROMETHEUS_SECRET_NS_EXPLICIT=${PROMETHEUS_SECRET_NS:-}
export PROMETHEUS_SECRET_NAME_EXPLICIT PROMETHEUS_SECRET_NS_EXPLICIT
PROMETHEUS_SECRET_NS=${PROMETHEUS_SECRET_NS:-$MONITORING_NAMESPACE}

# The scaling manager Configuration (required when DEPLOY_SCALING_MANAGER=true)
# This tree's manifests pass --external-scaler-bind-address, a flag the
# images under ghcr.io/llm-d do not have: :latest there crash-loops on it with
# "unknown flag" and the install has nothing to run. The default therefore
# points at a build of THIS repository, matching the Makefile's IMG.
#
# :main, not :latest -- built by ci-main-image on every push to main, so it
# cannot silently go stale. Both tags select the Always pull policy in
# infra_scaling_manager.sh, so this changes what is pulled, not how.
LLMSM_DEFAULT_IMAGE_REPO="ghcr.io/llm-d-incubation/llm-d-scaling-manager"
#
# The tag is the one config/base/manager/kustomization.yaml names: `main` on the
# main branch, the release tag on a release commit (release-process.md pins it
# there), so a checkout of vX.Y.Z installs vX.Y.Z without being told.
LLMSM_DEFAULT_IMAGE_TAG=$(awk '$1 == "newTag:" {gsub(/["\r]/, "", $2); print $2; exit}' "$LLMSM_PROJECT/config/base/manager/kustomization.yaml" 2>/dev/null)
LLMSM_DEFAULT_IMAGE_TAG=${LLMSM_DEFAULT_IMAGE_TAG:-"main"}
# What a re-run with no image named installs. A printed re-run command carries
# IMG only when this install runs something else (llmsm_rerun_install_vars).
LLMSM_DEFAULT_IMAGE_REF="$LLMSM_DEFAULT_IMAGE_REPO:$LLMSM_DEFAULT_IMAGE_TAG"
LLMSM_IMAGE_REPO=${LLMSM_IMAGE_REPO:-$LLMSM_DEFAULT_IMAGE_REPO}
LLMSM_IMAGE_TAG=${LLMSM_IMAGE_TAG:-$LLMSM_DEFAULT_IMAGE_TAG}
# A digest (IMG=repo@sha256:<hex>) pins exactly what runs; see llmsm_split_image_ref.
LLMSM_IMAGE_DIGEST=${LLMSM_IMAGE_DIGEST:-}
# Empty means "choose by reference": Always for a moving tag (main, latest, ...),
# IfNotPresent for a release tag or a digest -- see llmsm_image_pull_policy. This
# defaulted to Always, which made that choice dead code: a pinned release tag was
# re-pulled on every pod start for nothing.
LLMSM_IMAGE_PULL_POLICY=${LLMSM_IMAGE_PULL_POLICY:-}
# An existing docker-registry Secret in LLMSM_NS, for an image in a private
# registry. Named rather than created here: a script that built the secret would
# have to be handed a password, and this way the credential is created once by
# whoever owns it and never passes through an install run.
LLMSM_IMAGE_PULL_SECRET=${LLMSM_IMAGE_PULL_SECRET:-}
# The controller's LOG_LEVEL by name (error|warn|info|debug|trace). Unset keeps
# the shipped info; an explicit LLMSM_VERBOSITY beats it.
LLMSM_LOG_LEVEL=${LLMSM_LOG_LEVEL:-}
# Optional: multi-controller isolation (sets controller_instance on metrics / selectors when non-empty).

# Scale-to-zero for the fleet (LLMSM_SCALE_TO_ZERO). Unset keeps what the live
# controller has, and a first install takes the shipped false: the default the
# controller, the shipped ConfigMap and install-epp.sh all have.
ENABLE_SCALE_TO_ZERO=${ENABLE_SCALE_TO_ZERO:-}

# Prometheus Configuration
PROM_CA_CERT_PATH=${PROM_CA_CERT_PATH:-"/tmp/prometheus-ca.crt"}
PROMETHEUS_SECRET_NAME=${PROMETHEUS_SECRET_NAME:-"prometheus-web-tls"}
# Pinned like KEDA_CHART_VERSION. Unpinned, every install took whatever the chart
# repository served that day, and a re-run upgraded the release shared by every
# the scaling manager on the cluster to it. A newer release already installed is kept unless the
# caller set it (kps_chart_version_to_install), so the default never downgrades.
KUBE_PROMETHEUS_STACK_CHART_VERSION_EXPLICIT=${KUBE_PROMETHEUS_STACK_CHART_VERSION:-}
KUBE_PROMETHEUS_STACK_CHART_VERSION=${KUBE_PROMETHEUS_STACK_CHART_VERSION:-92.7.0}

# Flags for deployment steps
DEPLOY_PROMETHEUS=${DEPLOY_PROMETHEUS:-true}
# Create the llm-d namespace. Set false when llm-d already runs somewhere else:
# an empty llm-d namespace looks like the place to deploy models, and the scaling manager would
# not be watching it.
# Creating an llm-d namespace is OFF by default. It only ever produced an EMPTY
# one — deploy/install-epp.sh creates its own when it deploys EPP — and an empty
# llm-d namespace is worse than none: it looks like the place to deploy models,
# and the scaling manager is not watching it. Turning it on is for the demo path that wants the
# namespace to exist before anything is put in it.
DEPLOY_LLMD_NS=${DEPLOY_LLMD_NS:-false}
DEPLOY_OPERATIONAL_DASHBOARD=${DEPLOY_OPERATIONAL_DASHBOARD:-true}
DEPLOY_ALERTING_RULES=${DEPLOY_ALERTING_RULES:-false}
DEPLOY_SCALING_MANAGER=${DEPLOY_SCALING_MANAGER:-true}
SKIP_CHECKS=${SKIP_CHECKS:-false}

# LLMSM_KUBE_CONTEXT pins this run to one cluster.
#
# Everything here otherwise resolves the cluster from the ambient kubeconfig, so
# an operator working across two of them has no way to say which one they mean
# except by changing their global current-context -- and a preflight that picks
# the wrong cluster still reports success while mutating it. That happened.
#
# Implemented as shims rather than by threading a flag through every call site:
# the libraries invoke kubectl and helm by name in a few hundred places, and a
# flag reaches only the ones somebody remembered to change. `command` prevents
# the shim recursing into itself. Shell functions are inherited by subshells, so
# $(...) and process substitutions get them too.
#
# KUBECONFIG needs no shim: kubectl and helm both read it already. Point it at a
# throwaway file whose current-context is the cluster you mean and this variable
# is unnecessary.
if [ -n "${LLMSM_KUBE_CONTEXT:-}" ]; then
    kubectl() { command kubectl --context "$LLMSM_KUBE_CONTEXT" "$@"; }
    helm()    { command helm --kube-context "$LLMSM_KUBE_CONTEXT" "$@"; }
fi

# Scaler backend: keda | none.
# - keda on kubernetes: expects cluster CRD unless KEDA_HELM_INSTALL=true (then this script installs Helm KEDA).
# - keda on openshift: platform-managed KEDA only (no Helm install from this script).
# - none: skip scaler install (cluster already provides external metrics).
SCALER_BACKEND=${SCALER_BACKEND:-keda}
# Recorded before the default lands: the external-scaler NetworkPolicy trusts an
# explicit KEDA_NAMESPACE, and otherwise looks for where KEDA actually runs.
KEDA_NAMESPACE_EXPLICIT=false
[ -n "${KEDA_NAMESPACE:-}" ] && KEDA_NAMESPACE_EXPLICIT=true
KEDA_NAMESPACE=${KEDA_NAMESPACE:-keda-system}
# Pinned for reproducible Helm installs (used when deploy_keda actually runs helm upgrade).
KEDA_CHART_VERSION=${KEDA_CHART_VERSION:-2.21.0}
# On kubernetes: default false (cluster-managed KEDA); kind-emulator flows often set true or use cluster path.
KEDA_HELM_INSTALL=${KEDA_HELM_INSTALL:-false}

# LeaderWorkerSet. Set true when LWS tests run (e.g. full e2e suite). Defaults false so smoke and benchmarks skip it.
DEPLOY_LWS=${DEPLOY_LWS:-false}
LWS_NAMESPACE=${LWS_NAMESPACE:-"lws-system"}
LWS_CHART_VERSION=${LWS_CHART_VERSION:-"0.8.0"}

# Environment-related variables
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Detected when unset. OpenShift changes the monitoring wiring and the default
# scope, and getting it wrong is not subtle — the controller keeps the Kubernetes
# Prometheus URL and exits on a Prometheus it cannot reach. API discovery answers
# it, and any authenticated user may read that (unlike the platform namespaces a
# tenant cannot see).
#
# ENVIRONMENT_EXPLICIT records what the caller set, before detection: a printed
# re-run command repeats it (llmsm_rerun_install_vars), and a detected one it
# leaves to be detected again.
ENVIRONMENT_EXPLICIT=${ENVIRONMENT:-}
export ENVIRONMENT_EXPLICIT
if [ -z "${ENVIRONMENT:-}" ]     && [ -n "$(kubectl api-resources --api-group=route.openshift.io -o name 2>/dev/null)" ]; then
    ENVIRONMENT=openshift
fi
ENVIRONMENT=${ENVIRONMENT:-"kubernetes"}
COMPATIBLE_ENV_LIST=("kubernetes" "openshift" "kind-emulator")
REQUIRED_TOOLS=("kubectl" "helm" "git")
DEPLOY_LIB_DIR="$SCRIPT_DIR/lib"

PRODUCTION_ENV_LIST=("openshift")

# Shared deploy helpers
# shellcheck source=lib/verify.sh
source "$DEPLOY_LIB_DIR/verify.sh"
# shellcheck source=lib/common.sh
source "$DEPLOY_LIB_DIR/common.sh"
# shellcheck source=lib/constants.sh
source "$DEPLOY_LIB_DIR/constants.sh"
# shellcheck source=lib/wait_helpers.sh
source "$DEPLOY_LIB_DIR/wait_helpers.sh"
# shellcheck source=lib/cli.sh
source "$DEPLOY_LIB_DIR/cli.sh"
# shellcheck source=lib/prereqs.sh
source "$DEPLOY_LIB_DIR/prereqs.sh"
# shellcheck source=lib/infra_scaler_backend.sh
source "$DEPLOY_LIB_DIR/infra_scaler_backend.sh"
# shellcheck source=lib/scaler_runtime.sh
source "$DEPLOY_LIB_DIR/scaler_runtime.sh"
# shellcheck source=lib/limiter_policy.sh
source "$DEPLOY_LIB_DIR/limiter_policy.sh"
# Function definitions only; the prereqs phase uses its policy-read grant.
# shellcheck source=lib/physical_limiter.sh
source "$DEPLOY_LIB_DIR/physical_limiter.sh"
# shellcheck source=lib/pod_writer.sh
source "$DEPLOY_LIB_DIR/pod_writer.sh"
# shellcheck source=lib/infra_scaling_manager.sh
source "$DEPLOY_LIB_DIR/infra_scaling_manager.sh"
# shellcheck source=lib/scaler_tls.sh
source "$DEPLOY_LIB_DIR/scaler_tls.sh"
# shellcheck source=lib/infra_epp.sh
source "$DEPLOY_LIB_DIR/infra_epp.sh"
# shellcheck source=lib/infra_monitoring.sh
source "$DEPLOY_LIB_DIR/infra_monitoring.sh"
# shellcheck source=lib/epp_auth.sh
source "$DEPLOY_LIB_DIR/epp_auth.sh"
# shellcheck source=lib/single_install.sh
source "$DEPLOY_LIB_DIR/single_install.sh"
# shellcheck source=lib/scaledobject.sh
source "$DEPLOY_LIB_DIR/scaledobject.sh"
# shellcheck source=lib/cleanup.sh
source "$DEPLOY_LIB_DIR/cleanup.sh"
# shellcheck source=lib/install_core.sh
source "$DEPLOY_LIB_DIR/install_core.sh"

# Captured BEFORE the platform script defaults it. Those defaults name the
# Prometheus this installer would deploy, so once they have run there is no way
# left to tell "the user asked for this URL" from "nobody was asked" — and the
# second case is the one that should go looking for the cluster's own Prometheus.
PROMETHEUS_URL_EXPLICIT=${PROMETHEUS_URL:-}
export PROMETHEUS_URL_EXPLICIT

UNDEPLOY=${UNDEPLOY:-false}
# CHECK_ONLY runs the prerequisite check and exits. Same code path the install
# takes, so "make check-prereqs" passing and the install then failing on a
# prerequisite is not a state these two can reach.
CHECK_ONLY=${CHECK_ONLY:-false}
# Which half of the install to run: prereqs (cluster admin) | scaling-manager (namespace
# admin) | all. The default keeps the single-command install unchanged.
# Captured before the default, like LLMSM_NS above: "the caller asked for all
# phases" and "the caller said nothing" are different, and only the second one
# may be resolved by looking at the cluster.
INSTALL_PHASE_EXPLICIT=${INSTALL_PHASE:-}
export INSTALL_PHASE_EXPLICIT
INSTALL_PHASE=${INSTALL_PHASE:-all}
# Allow a second scaling manager alongside an existing one. Refused by default because both
# would allocate from the same pool of free GPUs without seeing each other's
# claims. See deploy/lib/single_install.sh.

# Default ScaledObjects. A ScaledObject is the REGISTRATION — the scaling manager is only asked
# about workloads KEDA calls it about — so an install with none anywhere is a
# controller that never scales anything and looks healthy doing it.
# LLMSM_DEFAULT_SO_NS: a namespace, "scaling-manager", or "all" for every namespace holding llm-d model
# servers (cluster-scoped installs only). Defaults to NAMESPACE.
# false | plan (list and stop) | edit (list, $EDITOR, confirm) | true (apply all)
LLMSM_DEFAULT_SO=${LLMSM_DEFAULT_SO:-false}
# Unset means "what this install can reach": every namespace holding model servers
# when cluster-scoped, its own namespace when namespace-scoped. Defaulting it to
# NAMESPACE here would defeat that, and did — the scope-derived default then applied
# only to the make targets, so the same variable behaved differently depending on
# how you invoked it.
LLMSM_DEFAULT_SO_NS=${LLMSM_DEFAULT_SO_NS:-}
# An existing file is applied as-is, edits included, with no terminal needed.
LLMSM_DEFAULT_SO_PLAN=${LLMSM_DEFAULT_SO_PLAN:-}
# Your own ScaledObject template instead of the shipped one. See
# config/samples/keda/external-scaler/scaledobject-template.yaml.
LLMSM_DEFAULT_SO_TEMPLATE=${LLMSM_DEFAULT_SO_TEMPLATE:-}
LLMSM_DEFAULT_SO_MIN=${LLMSM_DEFAULT_SO_MIN:-1}
LLMSM_DEFAULT_SO_MAX=${LLMSM_DEFAULT_SO_MAX:-10}
DELETE_NAMESPACES=${DELETE_NAMESPACES:-false}
# Delete the llm-d namespace too. Separate and explicit: it holds the model servers,
# and an uninstall never restates the install flags, so this must never be inferred.
DELETE_LLMD_NS=${DELETE_LLMD_NS:-false}
# With UNDEPLOY=true, also remove Prometheus, the scaler backend and EPP. Off by
# default: those are shared, this install may not have created them, and removing
# them takes out every other thing on the cluster that uses them.
UNDEPLOY_SHARED=${UNDEPLOY_SHARED:-false}
# Controller replicas. Leader-elected, so extras are warm standbys for failover,
# not extra throughput — only the leader runs the optimization loops.
LLMSM_REPLICAS=${LLMSM_REPLICAS:-1}
# Controller --v. Empty keeps the shipped 2; 3 adds the per-replica capacity lines.
LLMSM_VERBOSITY=${LLMSM_VERBOSITY:-}

# Orchestration lives in deploy/lib/install_core.sh (keeps this entrypoint to variable defaults + sourcing only).
main "$@"
