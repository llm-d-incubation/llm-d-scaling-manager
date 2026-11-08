#!/usr/bin/env bash
#
# CLI help and argument parsing for deploy/install.sh.
# Requires vars: LLMSM_IMAGE_REPO, LLMSM_IMAGE_TAG, COMPATIBLE_ENV_LIST.
# Requires funcs: log_info/log_warning/log_error, containsElement().
#

print_help() {
  cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Bootstrap the scaling manager (optional), monitoring, and scaler backend on a Kubernetes or OpenShift cluster.
For llm-d (gateway, EPP, ModelService), see the llm-d project's installation guides.

Options:
  -i, --scaling-manager-image IMAGE        Container image for the scaling manager (default: $LLMSM_IMAGE_REPO:$LLMSM_IMAGE_TAG)
  -c, --check                  Run the prerequisite check and exit without deploying
  -p, --phase PHASE            prereqs | scaling-manager | all (default: all)
                                 prereqs  what a CLUSTER ADMIN does once per namespace:
                                          the namespace, the cluster-scoped RBAC, the
                                          ServiceMonitor, and the shared infrastructure
                                          (Prometheus, KEDA) if the cluster has none
                                 scaling-manager      the controller only — no cluster-scoped rights
                                          needed once prereqs are in place
                                 all      both, in order
  -u, --undeploy               Undeploy the scaling manager, monitoring, and scaler backend
  -e, --environment            kubernetes | openshift | kind-emulator (default: kubernetes)
  -h, --help                   Show this help and exit

Deprecated (ignored by install.sh; Helm chart removed):
  --release-name NAME          Helm release name (no-op; the scaling manager now installed via Kustomize)
  --accelerator TYPE           Same as ACCELERATOR_TYPE env

Environment Variables:

 The install
  IMG                          the scaling manager image as repo:tag or repo@sha256:<digest> (alternative to -i)
  LLMSM_NS                       Namespace to install the controller into (default: scaling-manager-system)
  LLMSM_SCOPE                    cluster | namespace (default: namespace)
                                 cluster   — manages every namespace, creates cluster-scoped RBAC,
                                             needs a cluster admin
                                 namespace — manages ONE namespace. Its cluster-scoped RBAC (metrics
                                             authn, EPP metrics, the node read gpu-inventory needs)
                                             is created once by an admin with --phase prereqs; the
                                             namespace's owner then installs the controller, and
                                             every later upgrade, with no cluster rights.
  LLMSM_WATCH_NS                 Namespace a namespace-scoped controller MANAGES, when that differs from
                               the one it runs in (default: its own). A comma-separated list
                               (team-a,team-b) or auto (every namespace with llm-d) manages
                               several from LLMSM_NS, with a Role in each and nothing cluster-wide
  LLMSM_REPLICAS                 Controller replicas. >1 is leader-election failover, NOT more throughput (default: 1)
  LLMSM_VERBOSITY                Controller --v, 0 to 5. 3 adds the per-replica capacity lines (default: unset, the shipped 2)
  DEPLOY_SCALING_MANAGER                   Deploy the scaling manager controller (default: true)
  SKIP_CHECKS                  Skip the prerequisite and permission checks (default: false)
  LLMSM_KUBE_CONTEXT             Pin every kubectl/helm call to this context, without
                               changing your global current-context (default: unset)
  RENDER                       A directory. The cluster-admin phase (--phase prereqs) reads the
                               cluster as usual and applies NOTHING: it writes admin.yaml (every
                               object) and admin.sh (Helm installs, deletes, then the apply) there
                               for a cluster admin to review and run. Needs read rights only.

 Scaling behaviour
  LLMSM_LIMITER                  (unset) | none | gpu-inventory | quota | gpu-inventory,quota. A list
                               declares every limiter in it, and scaling must fit all of them.
                               Unset keeps the limiters the live policy already declares (none on
                               a first install); none REMOVES them and scaling is UNBOUNDED.
                               gpu-inventory requires the controller to be able to list nodes,
                               which make setup-prereqs grants.
  LLMSM_QUOTAS                   REQUIRED when LLMSM_LIMITER names quota: the per-accelerator budget, as
                               TYPE=N pairs — LLMSM_QUOTAS='H200=8 A100=4'. Use -1 for no cap on a
                               type. There is no default: a quota limiter with no budget is not
                               unlimited, it is zero for every accelerator, and the fleet freezes.
                               The TYPE is the name scaling manager resolves, which it logs per variant.
  LLMSM_QUOTA_KUEUE              true | false (default). With a quota limiter, also read Kueue: per
                               namespace and accelerator type the SMALLER of the Kueue grant
                               (with cohort borrowing where it is safe) and LLMSM_QUOTAS applies.
                               Name the types in LLMSM_QUOTAS with -1 to let Kueue set the figure:
                               LLMSM_QUOTAS='H100=-1' LLMSM_QUOTA_KUEUE=true.
  LLMSM_QUOTA_KUEUE_BORROW       true (default) | false. With LLMSM_QUOTA_KUEUE=true, false writes
                               countBorrowable: false on the entry, so the Kueue cap stays at
                               nominal and no cohort borrowing is counted. Ignored, with a warning,
                               without LLMSM_QUOTA_KUEUE=true.
  LLMSM_QUOTA_SCOPE              namespace (default) | cluster. namespace caps each managed namespace
                               separately; cluster caps the sum across all of them -- but only
                               within ONE controller's policy. Published to several controllers
                               (make enable-physical-limiter), each enforces the cap on its own,
                               so N controllers permit N x the number.
  ENABLE_SCALE_TO_ZERO         Allow parking idle models at 0 replicas (default: true)
  LLMSM_DEFAULT_SO               Create default ScaledObjects for what is already running:
                               false (default) | plan (print and stop) | edit (plan, \$EDITOR, apply) | true (apply all)
  LLMSM_DEFAULT_SO_PLAN          Plan file. If it exists, apply exactly it and skip discovery.
  LLMSM_DEFAULT_SO_NS            A namespace, "scaling-manager" for the scaling manager's own, or "all" (default: derived from LLMSM_SCOPE)

 Monitoring
  DEPLOY_PROMETHEUS            Deploy the Prometheus stack (default: true). Skipped automatically when the
                               cluster already has a Prometheus outside MONITORING_NAMESPACE.
  PROMETHEUS_FORCE_INSTALL     Install even then (default: false). Two operators will contend over the same CRs.
  KUBE_PROMETHEUS_STACK_CHART_VERSION
                               Chart version the Prometheus stack is installed at (default: 92.7.0).
                               A newer release already installed is kept unless you set this.
  PROMETHEUS_URL               Where the scaling manager reads metrics from. Required when Prometheus is not deployed here.
  PROMETHEUS_TLS_INSECURE_SKIP_VERIFY
                               Skip verifying the Prometheus certificate. Default false; true for the
                               Prometheus this install deploys. See TLS configuration in
                               docs/reference/prometheus.md.
  PROMETHEUS_CA_CERT_PATH      CA file the controller verifies Prometheus with. Mount it yourself.
                               Unset, a re-run keeps the one the controller runs with, for the
                               same PROMETHEUS_URL; with none, the installer copies the CA from
                               the Prometheus TLS Secret into Secret scaling-manager-prometheus-ca and mounts it.
  PROMETHEUS_SECRET_NAME, PROMETHEUS_SECRET_NS
                               The Secret that CA is copied from (key ca.crt, else tls.crt; never
                               the key). Default: the one the Prometheus CR's web.tlsConfig names.
  PROMETHEUS_CA_FILE           A local CA file to copy instead.
  PROMETHEUS_TLS_PROBE         false skips the in-cluster TLS probe (default: true).
  PROMETHEUS_TLS_PROBE_IMAGE   curl image the probe runs (default: quay.io/curl/curl:8.11.1).
  DEPLOY_OPERATIONAL_DASHBOARD Publish the scaling manager Grafana dashboard (default: true). Works against an
                               existing Grafana — it is a sidecar-labelled ConfigMap.
  DASHBOARD_NS                 Namespace to publish the dashboard into (default: MONITORING_NAMESPACE)
  MONITORING_NAMESPACE         Namespace for the monitoring stack

 Warm pool
  WARMPOOL_PROXY_IMG           Override the pool proxy image. Optional: defaults to the published
                               one config/warmpool pins. Build your own with
                               make docker-build-warmpool-proxy docker-push-warmpool-proxy
  WARMPOOL_RUNTIME_CLASS       RuntimeClass for pool Pods (default: nvidia-legacy), or "none".
                               Cluster-specific: a name the cluster does not have fails admission,
                               and omitting one it needs gives containers with no GPU
                               (MONITORING_NAMESPACE above is reused to scrape lent pool Pods.
                               Without a scrape, a model reads as having LESS demand than it has
                               while a pool Pod is covering for it.)
                               Pools are only created for plan entries marked apply: yes; see
                               docs/guides/warm-pool/README.md

 Scaler backend and CRDs
  SCALER_BACKEND               keda (default) or none. The scaling manager needs KEDA to actuate; the preflight
                               checks for it before anything is created. KEDA older than 2.17
                               works, with a warning: its ScaledObjects get no fallback.
  KEDA_HELM_INSTALL            Install KEDA via Helm on kubernetes (default: false — assumes cluster KEDA).
                               Never on OpenShift: install the Custom Metrics Autoscaler operator there.
  KEDA_NAMESPACE               Namespace for KEDA (default: keda-system). Also the one the
                               external-scaler NetworkPolicy admits (written by the admin
                               phase, setup-prereqs); unset, the namespace serving the
                               external-metrics APIService, else the existing policy's. With
                               none of those, NO policy is written and :9090 stays open (warned)
  LLMSM_SCALER_NETWORKPOLICY     false skips the NetworkPolicy that admits :9090 only from KEDA
                               (default: true)
  LLMSM_SCALER_TLS               unset (default): mutual TLS on the external scaler when KEDA's side
                               is in place and no ScaledObject is left behind, else plaintext with
                               a warning. true: the same, but refuse instead. false: plaintext.
                               A controller that already has TLS keeps it
  LLMSM_SCALER_TLS_MIGRATE       true moves the ScaledObjects that name the scaler with it when its
                               TLS mode changes (default: false)
  LLMSM_SCALER_TLS_PROVIDER      auto (default) | cert-manager | openssl: who issues those
                               certificates. auto uses cert-manager when it is installed
  CRD_INSTALL                  if-missing (default) | always | never. Gateway API and GAIE CRDs are
                               cluster-scoped and shared; 'always' overwrites what other controllers use.
  DEPLOY_LWS                   Install the LeaderWorkerSet CRD if absent (default: false)

 Undeploy
  UNDEPLOY                     Undeploy mode (default: false)
  UNDEPLOY_SHARED              Also remove Prometheus, KEDA and EPP (default: false — they are shared
                               and this install may not have created them)
  DELETE_NAMESPACES            Delete the scaling manager and monitoring namespaces afterwards (default: false)
  DELETE_LLMD_NS               Also delete NAMESPACE — it holds the model servers (default: false)

 Other
  NAMESPACE                    Namespace used for EPP/model-server setup by the e2e and sample paths.
                               It does NOT scope what the scaling manager manages: The scaling manager has no watch and no listing, and
                               learns of a workload only when KEDA calls it about one.

Examples:
  # New cluster, everything
  $(basename "$0")

  # Existing llm-d cluster: use its Prometheus and KEDA, touch neither
  PROMETHEUS_URL=https://prom.monitoring.svc:9090 DEPLOY_PROMETHEUS=false \\
    SCALER_BACKEND=keda CRD_INSTALL=never $(basename "$0")

  IMG=registry.example.com/scaling-manager:dev $(basename "$0") -e kind-emulator

  $(basename "$0") -e openshift
EOF
}

# llmsm_split_image_ref sets LLMSM_IMAGE_REPO, LLMSM_IMAGE_TAG and LLMSM_IMAGE_DIGEST
# from one image reference. Returns 1, changing nothing, when $1 names neither a
# tag nor a digest.
#
# Splitting on the FIRST colon, as this used to, read a registry port as the tag:
# localhost:5000/scaling-manager:dev became repo "localhost", tag "5000/scaling-manager:dev". And a
# digest -- repo@sha256:<hex>, the one form that pins exactly what runs -- only
# rendered because the halves happened to glue back together, while the hex was
# treated as a tag everywhere else. A digest is carried as its own value now, so
# the overlay can pin it and the pull policy can tell it from a moving tag.
llmsm_split_image_ref() {
  local ref="$1" name
  if [[ "$ref" == *@* ]]; then
    name="${ref%%@*}"
    LLMSM_IMAGE_DIGEST="${ref#*@}"
    # repo:tag@digest: the digest decides what is pulled; keep the tag for display.
    if [[ "${name##*/}" == *:* ]]; then
      LLMSM_IMAGE_REPO="${name%:*}"
      LLMSM_IMAGE_TAG="${name##*:}"
    else
      LLMSM_IMAGE_REPO="$name"
      LLMSM_IMAGE_TAG=""
    fi
    return 0
  fi
  # Only a colon in the LAST path segment separates a tag.
  [[ "${ref##*/}" == *:* ]] || return 1
  LLMSM_IMAGE_REPO="${ref%:*}"
  LLMSM_IMAGE_TAG="${ref##*:}"
  LLMSM_IMAGE_DIGEST=""
}

# llmsm_image_ref echoes the image reference LLMSM_IMAGE_REPO, LLMSM_IMAGE_TAG and
# LLMSM_IMAGE_DIGEST name: repo@digest, repo:tag@digest, or repo:tag. Gluing
# repo and tag alone printed "repo:" for a digest, which names nothing.
llmsm_image_ref() {
  local ref="$LLMSM_IMAGE_REPO"
  [ -z "${LLMSM_IMAGE_TAG:-}" ] || ref="$ref:$LLMSM_IMAGE_TAG"
  [ -z "${LLMSM_IMAGE_DIGEST:-}" ] || ref="$ref@$LLMSM_IMAGE_DIGEST"
  printf '%s\n' "$ref"
}

# llmsm_image_ref_fields echoes "<repo>|<tag>|<digest>" for an image reference,
# split by llmsm_split_image_ref. A reference with neither tag nor digest is
# repo|latest|, which is what a container runtime pulls for it.
#
# For the Makefile, which used to split IMG inline on the last colon: a digest
# then became repo "ghcr.io/o/w@sha256" with the hex as its tag.
llmsm_image_ref_fields() {
  local LLMSM_IMAGE_REPO="" LLMSM_IMAGE_TAG="" LLMSM_IMAGE_DIGEST=""
  if ! llmsm_split_image_ref "$1"; then
    LLMSM_IMAGE_REPO="$1"
    LLMSM_IMAGE_TAG="latest"
  fi
  printf '%s|%s|%s\n' "$LLMSM_IMAGE_REPO" "$LLMSM_IMAGE_TAG" "$LLMSM_IMAGE_DIGEST"
}

parse_args() {
  if [[ -n "$IMG" ]]; then
    log_info "Detected IMG environment variable: $IMG"
    if ! llmsm_split_image_ref "$IMG"; then
      log_warning "IMG has wrong format (want repo:tag or repo@sha256:<digest>), using default image"
    fi
  fi

  while [[ $# -gt 0 ]]; do
    case "$1" in
      -i|--scaling-manager-image)
        if ! llmsm_split_image_ref "$2"; then
          LLMSM_IMAGE_REPO="$2"
        fi
        shift 2
        ;;
      -c|--check)             CHECK_ONLY=true; shift ;;
      -p|--phase)
        INSTALL_PHASE="$2" ; shift 2
        case "$INSTALL_PHASE" in
          prereqs|scaling-manager|all) ;;
          *) log_error "Invalid --phase: $INSTALL_PHASE. Valid options are: prereqs, scaling-manager, all" ;;
        esac
        ;;
      -u|--undeploy)          UNDEPLOY=true; shift ;;
      -e|--environment)
        ENVIRONMENT="$2" ; ENVIRONMENT_EXPLICIT="$2" ; shift 2
        if ! containsElement "$ENVIRONMENT" "${COMPATIBLE_ENV_LIST[@]}"; then
          log_error "Invalid environment: $ENVIRONMENT. Valid options are: ${COMPATIBLE_ENV_LIST[*]}"
        fi
        ;;
      --accelerator)
        export ACCELERATOR_TYPE="$2"
        shift 2
        ;;
      --release-name)
        # Legacy CI/Helm — install.sh no longer installs via Helm; value is ignored.
        shift 2
        ;;
      -h|--help)              print_help; exit 0 ;;
      *)
        echo "Error: Unknown option: $1" >&2
        print_help
        exit 1
        ;;
    esac
  done
}
