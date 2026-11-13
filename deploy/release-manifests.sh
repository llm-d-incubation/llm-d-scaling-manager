#!/usr/bin/env bash
#
# Renders the files a release attaches, from the same kustomize overlays
# `make deploy-scaling-manager` applies. `make release-manifests` runs it; the
# release workflow runs that with the digest it pushed, then signs and uploads
# what it wrote. Design: docs/proposals/install-from-release.md.
#
#   deploy/release-manifests.sh --image <repo>@sha256:<digest> --version <tag> --out <dir>
#
# What it writes to <dir>, and the rules every manifest follows, which
# hack/check-release-manifests.sh asserts:
#
#   - the image pinned by digest, never a tag;
#   - no Namespace and no ConfigMap an operator edits (the settings and the
#     scaling policy), so `kubectl apply` never resets what an operator set and
#     `kubectl delete -f` never deletes a namespace;
#   - the controller namespace written as __NAMESPACE__ in every namespaced file
#     (an underscore is not valid in a name, so an unsubstituted file fails at
#     once), and the per-install ClusterRole names carrying it rather than a
#     hash, so `sed` can fill it in;
#   - the controller provisions the scaler's mutual-TLS certificates itself
#     (--external-scaler-tls-bootstrap, and the Role that lets it), since no
#     installer script runs to mint them;
#   - every object labelled part-of, instance, version, and
#     scaling-manager.llmd.ai/part: admin or controller. admin is what the
#     installer's prereqs phase applies (LLMSM_PREREQ_KINDS and
#     LLMSM_PREREQ_NAMED in deploy/lib/common.sh), plus any cluster-scoped or
#     monitoring.coreos.com object of the extra files.
#
# Beside the manifests it writes scaling-manager-tools-<version>.tar.gz, the
# operator tools as scripts (the files of the tools image), and SHA256SUMS
# lists it too.
#
# Needs yq v4, sha256sum, GNU tar, and kustomize (KUSTOMIZE, else kustomize or
# `kubectl kustomize` on PATH). Reads no cluster.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# The image name config/base/manager/kustomization.yaml gives the controller,
# which the digest pin below must match.
BASE_IMAGE="ghcr.io/llm-d-incubation/llm-d-scaling-manager"
# Where the cluster-wide files install: one controller per cluster.
CLUSTER_NS="scaling-manager-system"
PLACEHOLDER="__NAMESPACE__"
PART_LABEL="scaling-manager.llmd.ai/part"

# The files this writes, in the order SHA256SUMS lists them.
RELEASE_FILES=(
    scaling-manager-namespaced.yaml
    scaling-manager-namespaced-openshift.yaml
    scaling-manager-cluster.yaml
    scaling-manager-cluster-openshift.yaml
    scaling-manager-settings.yaml
    scaling-manager-model-namespace.yaml
    scaling-manager-activate.yaml
    scaling-manager-keda-auth.yaml
    scaling-manager-scaler-tls.yaml
    scaling-manager-kueue.yaml
    scaling-manager-alerts.yaml
    scaling-manager-policy-example.yaml
    scaling-manager-metrics-reader.yaml
    scaling-manager-preflight.yaml
    scaling-manager-preflight-cluster.yaml
)

# The files no end-to-end test installs yet. Each says so in its header;
# hack/check-release-manifests.sh asserts the line on exactly these files.
# The Kubernetes namespaced and cluster-wide files are installed by
# make test-e2e-release-manifests (RELEASE_E2E_SCOPE=namespace and cluster);
# no CI runs OpenShift.
UNTESTED_FILES="scaling-manager-cluster-openshift.yaml scaling-manager-namespaced-openshift.yaml"
UNTESTED_NOTE="NOT YET TESTED end to end; the tested installs are scaling-manager-namespaced.yaml and scaling-manager-cluster.yaml on Kubernetes (make test-e2e-release-manifests)."

die() { echo "release-manifests: $*" >&2; exit 1; }

usage() {
    echo "usage: $0 --image <repo>@sha256:<digest> --version <vX.Y.Z[-pre]> --out <dir>" >&2
    exit 2
}

IMAGE="" VERSION="" OUT=""
while [ $# -gt 0 ]; do
    case "$1" in
        --image) IMAGE="${2:-}"; shift 2 ;;
        --version) VERSION="${2:-}"; shift 2 ;;
        --out) OUT="${2:-}"; shift 2 ;;
        -h|--help) usage ;;
        *) echo "release-manifests: unknown argument $1" >&2; usage ;;
    esac
done
[ -n "$IMAGE" ] && [ -n "$VERSION" ] && [ -n "$OUT" ] || usage

# A digest, never a tag: a tag can be moved after the release is signed.
IMAGE_REPO="${IMAGE%@*}"
IMAGE_DIGEST="${IMAGE#*@}"
[ "$IMAGE_REPO" != "$IMAGE" ] || die "--image $IMAGE has no @sha256:<digest>; the release files pin the image by digest"
printf '%s' "$IMAGE_DIGEST" | grep -qxE 'sha256:[0-9a-f]{64}' \
    || die "--image $IMAGE: '$IMAGE_DIGEST' is not sha256:<64 hex digits>"
# A tag sits after the last /; a colon before it is a registry port
# (localhost:5000/repo@sha256:...), which is fine.
case "${IMAGE_REPO##*/}" in
    *:*) die "--image $IMAGE: give the repository and the digest, without a tag" ;;
esac
case "$IMAGE_REPO" in
    ""|*/) die "--image $IMAGE: no repository before the digest" ;;
esac
printf '%s' "$VERSION" | grep -qxE 'v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z][0-9A-Za-z.-]*)?' \
    || die "--version $VERSION is not vMAJOR.MINOR.PATCH[-PRERELEASE]"

command -v yq >/dev/null 2>&1 || die "yq is required"
command -v sha256sum >/dev/null 2>&1 || die "sha256sum is required"

kustomize_build() {
    if [ -n "${KUSTOMIZE:-}" ] && [ -x "${KUSTOMIZE}" ]; then
        "$KUSTOMIZE" build "$1"
    elif command -v kustomize >/dev/null 2>&1; then
        kustomize build "$1"
    elif command -v kubectl >/dev/null 2>&1; then
        kubectl kustomize "$1"
    else
        die "neither kustomize nor kubectl is available to render the overlays"
    fi
}

# The admin part, read from the installer so the two cannot disagree about
# where the line between the cluster admin and the namespace owner is.
admin_kinds="$(sed -n 's/^LLMSM_PREREQ_KINDS=(\(.*\))$/\1/p' "$ROOT/deploy/lib/common.sh")"
admin_named="$(sed -n '/^LLMSM_PREREQ_NAMED=(/,/^)/p' "$ROOT/deploy/lib/common.sh" | sed -n 's/^ *"\([^"]*\)" *$/\1/p' | tr '\n' ' ')"
[ -n "$admin_kinds" ] || die "could not read LLMSM_PREREQ_KINDS from deploy/lib/common.sh"
[ -n "$admin_named" ] || die "could not read LLMSM_PREREQ_NAMED from deploy/lib/common.sh"
export ADMIN_KINDS=" $admin_kinds " ADMIN_NAMED=" $admin_named "

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# render_overlay <scope> <platform> <namespace> <out>: kustomize build of
# config/overlays/<scope>-scoped/<platform>, moved into <namespace>, with the
# image pinned by digest. The same throw-away overlay shape the installer
# builds (deploy/lib/infra_scaling_manager.sh): the overlay as ./base, the
# namespace and the image set over it.
render_overlay() {
    local scope="$1" platform="$2" ns="$3" out="$4" dir
    dir="$WORK/overlay-$scope-$platform"
    mkdir -p "$dir"
    ln -s "$ROOT/config/overlays/$scope-scoped/$platform" "$dir/base"
    ln -s "$ROOT/config/components/prometheus-alerts" "$dir/prometheus-alerts"
    {
        printf 'apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n'
        printf 'namespace: %s\n' "$ns"
        printf 'resources:\n- ./base\n'
        printf 'components:\n- ./prometheus-alerts\n'
        printf 'images:\n- name: %s\n  newName: %s\n  digest: "%s"\n' "$BASE_IMAGE" "$IMAGE_REPO" "$IMAGE_DIGEST"
    } > "$dir/kustomization.yaml"
    kustomize_build "$dir" > "$out" || die "kustomize build of config/overlays/$scope-scoped/$platform failed"
    [ -s "$out" ] || die "kustomize build of config/overlays/$scope-scoped/$platform rendered nothing"
}

# label <instance>: every object part-of, instance, version and part.
label() {
    INSTANCE="$1" VERSION="$VERSION" PART_LABEL="$PART_LABEL" yq '
        .kind as $k
        | .metadata.name as $n
        | ((strenv(ADMIN_KINDS) | contains(" " + $k + " "))
           or (strenv(ADMIN_NAMED) | contains(" " + $k + "/" + $n + " "))
           or ($k == "Namespace" or $k == "ClusterRole" or $k == "ClusterRoleBinding" or $k == "ClusterTriggerAuthentication")
           or ((.apiVersion // "") | test("^monitoring\.coreos\.com/"))) as $admin
        | .metadata.labels["app.kubernetes.io/part-of"] = "scaling-manager"
        | .metadata.labels["app.kubernetes.io/instance"] = strenv(INSTANCE)
        | .metadata.labels["app.kubernetes.io/version"] = strenv(VERSION)
        | .metadata.labels[strenv(PART_LABEL)] = "controller"
        | (select($admin == true) | .metadata.labels[strenv(PART_LABEL)]) = "admin"'
}

# main_manifest <raw render> <namespace> <platform>: the release rules applied
# to an overlay render. Prints the manifest.
main_manifest() {
    local raw="$1" ns="$2" platform="$3" keda_ns=keda
    [ "$platform" = openshift ] && keda_ns=openshift-keda
    # On Kubernetes the base binds the controller /metrics to a fixed account,
    # kube-prometheus-stack-prometheus in scaling-manager-monitoring. The
    # installer binds only a scraper a trusted source names, so the release
    # files leave the binding out: scaling-manager-metrics-reader.yaml is that
    # binding with the account as a placeholder. On OpenShift the binding names
    # the install own metrics-scraper account and stays.
    # The Deployment mounts Secret scaling-manager-prometheus-ca, optional, at
    # /etc/scaling-manager-prometheus-ca (the path the installer uses): the
    # operator creates it and sets prometheusCACertPath to its ca.crt. Without
    # the mount, a Prometheus with a private CA could not be verified.
    NS="$ns" KEDA_NS="$keda_ns" PLATFORM="$platform" yq '
        select((.kind == "Namespace"
            or (.kind == "ConfigMap" and (.metadata.name == "scaling-manager-manager-config" or .metadata.name == "scaling-manager-scaling-policy-config"))
            or .kind == "PrometheusRule"
            or (strenv(PLATFORM) == "kubernetes" and .kind == "ClusterRoleBinding" and .metadata.name == "scaling-manager-metrics-reader-rolebinding")
            or ((.kind == "ClusterRole" or .kind == "ClusterRoleBinding") and (.metadata.name | test("^scaling-manager-kueue-reader-")))) | not)
        | (select(.kind == "Deployment") | .spec.template.spec.volumes[] | select(.configMap.name == "scaling-manager-manager-config") | .configMap.optional) = true
        | (select(.kind == "Deployment") | .spec.template.spec.containers[] | select(.name == "manager") | .args) += ["--external-scaler-tls-bootstrap=true"]
        | (select(.kind == "Deployment") | .spec.template.spec.volumes) += [{"name": "scaling-manager-prometheus-ca", "secret": {"secretName": "scaling-manager-prometheus-ca", "optional": true, "items": [{"key": "ca.crt", "path": "ca.crt"}]}}]
        | (select(.kind == "Deployment") | .spec.template.spec.containers[] | select(.name == "manager") | .volumeMounts) += [{"name": "scaling-manager-prometheus-ca", "mountPath": "/etc/scaling-manager-prometheus-ca", "readOnly": true}]
        | (select(.kind == "ServiceMonitor") | .spec.endpoints[].tlsConfig | select(has("serverName")) | .serverName) |= sub("PLACEHOLDER-NAMESPACE", strenv(NS))
        | (select(.kind == "NetworkPolicy") | .spec.ingress[] | select(has("from")) | .from[] | select(has("namespaceSelector")) | .namespaceSelector.matchLabels | select(has("kubernetes.io/metadata.name")) | .["kubernetes.io/metadata.name"]) = strenv(KEDA_NS)
        | (select(.kind == "ClusterRole" and .metadata.name == "scaling-manager-policy-namespace-reader-role") | .rules[] | select(.resources[0] == "namespaces") | .resourceNames) = [strenv(NS), "scaling-manager-policy"]
        ' "$raw" | strip_pod_patch
}

# scaler_tls_rbac <namespace>: the Role and RoleBinding the controller needs to
# provision the scaler certificates itself (--external-scaler-tls-bootstrap,
# set on the Deployment above): get and update on the three Secrets by name,
# never create, and get on the KEDA marker and the OpenShift service CA
# ConfigMaps. The Secrets are the placeholders of scaling-manager-scaler-tls.yaml.
# Only the release files carry it: a make install provisions the certificates
# from the installer and passes the certificate files instead.
scaler_tls_rbac() {
    local f
    for f in scaler-tls-role scaler-tls-rolebinding; do
        echo '---'
        sed "s/__NAMESPACE__/$1/g" "$ROOT/config/release/$f.yaml"
    done
}

# strip_pod_patch: the namespaced manager Role without patch on pods.
#
# The overlay Role (config/components/tenant-installable) carries it for the
# warm pool, so a namespaced make install can patch pods in the namespace it
# manages from the start. The release files grant it only through
# scaling-manager-activate.yaml, applied to turn utilization share active or
# to use the warm pool (decision 3 of the proposal), so this Role and its copy
# in each model namespace keep get, list and watch. The make install is
# unchanged until it renders through this script.
strip_pod_patch() {
    yq '(select(.kind == "Role" and .metadata.name == "scaling-manager-manager-role") | .rules[] | select(.resources | contains(["pods"])) | .verbs) |= map(select(. != "patch"))'
}

# per_install_names <namespace>: a namespaced install names its ClusterRoles
# and ClusterRoleBindings after its namespace, and the roleRefs follow.
per_install_names() {
    NS="$1" yq '
        (select((.kind == "ClusterRole" or .kind == "ClusterRoleBinding") and (.metadata.name | test("^scaling-manager-"))) | .metadata.name) |= . + "-" + strenv(NS)
        | (select(.roleRef.kind == "ClusterRole" and (.roleRef.name | test("^scaling-manager-"))) | .roleRef.name) |= . + "-" + strenv(NS)'
}

# header <file> <what> <how>: the comment every file starts with.
header() {
    printf '# %s, llm-d-scaling-manager %s\n' "$1" "$VERSION"
    case " $UNTESTED_FILES " in *" $1 "*) printf '# %s\n' "$UNTESTED_NOTE" ;; esac
    printf '# %s\n' "$2"
    [ -z "$3" ] || printf '# %s\n' "$3"
    printf '# Rendered by deploy/release-manifests.sh; see docs/proposals/install-from-release.md.\n'
}

# write <file> <what> <how>: header plus stdin, into $OUT.
write() {
    { header "$1" "$2" "$3"; cat; } > "$OUT/$1"
}

mkdir -p "$OUT"
for f in "${RELEASE_FILES[@]}" SHA256SUMS; do rm -f "$OUT/$f"; done

SED_HOW="Before applying: sed -i \"s/$PLACEHOLDER/<namespace>/g\" <file>"

for platform in kubernetes openshift; do
    suffix=""
    [ "$platform" = openshift ] && suffix="-openshift"

    render_overlay namespace "$platform" "$PLACEHOLDER" "$WORK/ns-$platform.yaml"
    { main_manifest "$WORK/ns-$platform.yaml" "$PLACEHOLDER" "$platform"; scaler_tls_rbac "$PLACEHOLDER"; } \
        | per_install_names "$PLACEHOLDER" | label "$PLACEHOLDER" \
        | write "scaling-manager-namespaced$suffix.yaml" \
            "One controller managing one namespace ($platform). Objects labelled $PART_LABEL=admin are the cluster admin's; =controller the namespace owner's." \
            "$SED_HOW"

    render_overlay cluster "$platform" "$CLUSTER_NS" "$WORK/cl-$platform.yaml"
    { main_manifest "$WORK/cl-$platform.yaml" "$CLUSTER_NS" "$platform"; scaler_tls_rbac "$CLUSTER_NS"; } | label "$CLUSTER_NS" \
        | write "scaling-manager-cluster$suffix.yaml" \
            "One controller for the cluster, in namespace $CLUSTER_NS ($platform). Create that namespace first; applies as is." \
            ""
done

NSRAW="$WORK/ns-kubernetes.yaml"

label "$PLACEHOLDER" < "$ROOT/config/release/settings-configmap.yaml" \
    | write scaling-manager-settings.yaml \
        "The settings ConfigMap, with the default of every key. Create it once with kubectl create -f; never apply it over a running install." \
        "$SED_HOW"

# The controller's Role, copied from the render so it cannot drift from what
# the controller needs, into a model namespace it manages from elsewhere.
{
    strip_pod_patch < "$NSRAW" | NS="$PLACEHOLDER" yq '
        select(.kind == "Role" and .metadata.name == "scaling-manager-manager-role")
        | .metadata.name = "scaling-manager-manager-role-" + strenv(NS)
        | .metadata.namespace = "__MODEL_NAMESPACE__"'
    echo '---'
    cat "$ROOT/config/release/model-namespace-rolebinding.yaml"
    echo '---'
    MODELDIR="$WORK/modelserver"
    mkdir -p "$MODELDIR"
    ln -s "$ROOT/config/modelserver-metrics" "$MODELDIR/base"
    printf 'namespace: __MODEL_NAMESPACE__\nresources:\n- ./base\n' > "$MODELDIR/kustomization.yaml"
    kustomize_build "$MODELDIR"
} | label "$PLACEHOLDER" \
  | write scaling-manager-model-namespace.yaml \
      "Per model namespace: the model-server PodMonitor, and the controller's Role and RoleBinding there (needed when the controller runs in another namespace)." \
      "Before applying: sed -e \"s/$PLACEHOLDER/<controller namespace>/g\" -e \"s/__MODEL_NAMESPACE__/<model namespace>/g\""

{
    cat "$ROOT/config/release/pod-writer-role.yaml"
    echo '---'
    cat "$ROOT/config/release/pod-writer-rolebinding.yaml"
} | label "$PLACEHOLDER" \
  | write scaling-manager-activate.yaml \
      "The only grant of patch on pods: apply it to turn utilization share active, or to use the warm pool, in each namespace where the controller must patch pods: the controller namespace, each model namespace, or both. Delete it when neither is in use." \
      "Before applying: sed -e \"s/$PLACEHOLDER/<controller namespace>/g\" -e \"s/__MODEL_NAMESPACE__/<namespace to grant in>/g\""

{
    cat "$ROOT/config/release/keda-auth-clustertriggerauthentication.yaml"
    echo '---'
    cat "$ROOT/config/release/keda-auth-configmap.yaml"
} | label "$PLACEHOLDER" \
  | write scaling-manager-keda-auth.yaml \
      "Mutual TLS between KEDA and the scaler: the ClusterTriggerAuthentication and the marker the controller reads. Copy the client Secret into KEDA's namespace first." \
      "Before applying: sed -e \"s/$PLACEHOLDER/<controller namespace>/g\" -e \"s/__KEDA_NAMESPACE__/<KEDA namespace>/g\""

# The placeholders are not in the main files: kubectl apply of a file holding
# them after the controller filled them sets the data back to empty (a
# client-side apply), or conflicts and then empties it (a server-side apply
# with --force-conflicts), and the controller would mint a new CA into it.
label "$PLACEHOLDER" < "$ROOT/config/release/scaler-tls-secret.yaml" \
    | write scaling-manager-scaler-tls.yaml \
        "The external scaler's three certificate Secrets, empty: the controller fills them and may not create Secrets. Create them once with kubectl create -f; never apply this file over a running install, and keep the CA while KEDA may use it." \
        "Before creating: sed -i \"s/$PLACEHOLDER/<controller namespace>/g\" <file>; for the cluster-wide files the namespace is $CLUSTER_NS"

{
    NS="$PLACEHOLDER" yq '
        select((.kind == "ClusterRole" or .kind == "ClusterRoleBinding") and (.metadata.name | test("^scaling-manager-kueue-reader-")))
        | (select(.kind == "ClusterRole") | .rules[] | select(.resources[0] == "namespaces") | .resourceNames) = [strenv(NS)]' "$NSRAW" \
        | per_install_names "$PLACEHOLDER"
    for f in kueue-borrow-reader-clusterrole kueue-borrow-reader-clusterrolebinding kueue-controller-reader-role kueue-controller-reader-rolebinding; do
        echo '---'
        cat "$ROOT/config/release/$f.yaml"
    done
} | label "$PLACEHOLDER" \
  | write scaling-manager-kueue.yaml \
      "Optional, with kueue: \"true\" in the settings: the Kueue reads (ClusterQueues, cohorts, flavors; Workloads for borrowing; the Kueue controller in its namespace)." \
      "Before applying: sed -e \"s/$PLACEHOLDER/<controller namespace>/g\" -e \"s/__KUEUE_NAMESPACE__/<Kueue namespace>/g\""

yq 'select(.kind == "PrometheusRule")' "$NSRAW" | label "$PLACEHOLDER" \
    | write scaling-manager-alerts.yaml \
        "Optional: the alerting rules, as a PrometheusRule in the controller's namespace." \
        "$SED_HOW"

yq 'select(.kind == "ConfigMap" and .metadata.name == "scaling-manager-scaling-policy-config")' "$NSRAW" | label "$PLACEHOLDER" \
    | write scaling-manager-policy-example.yaml \
        "An example scaling-policy ConfigMap to start from. Not needed: the controller runs on its built-in defaults without one." \
        "$SED_HOW"

label "$PLACEHOLDER" < "$ROOT/config/release/metrics-reader-clusterrolebinding.yaml" \
    | write scaling-manager-metrics-reader.yaml \
        "Optional, Kubernetes namespaced install: lets the Prometheus that scrapes the controller read its /metrics. Name that Prometheus account; nothing is bound by default. For the cluster-wide file: kubectl create clusterrolebinding scaling-manager-metrics-reader-rolebinding --clusterrole=scaling-manager-metrics-reader --serviceaccount=<prometheus namespace>:<prometheus serviceaccount>" \
        "Before applying: sed -e \"s/$PLACEHOLDER/<controller namespace>/g\" -e \"s/__PROMETHEUS_NAMESPACE__/<prometheus namespace>/g\" -e \"s/__PROMETHEUS_SA__/<prometheus serviceaccount>/g\""

# The preflight Job, its ServiceAccount and its read-only RBAC: the Job runs
# `manager preflight check` from the release image, pinned like the
# controller's. Two files: the namespaced one reads pods and PodMonitors in
# the controller's namespace only, and other Deployments by name; the
# cluster-wide one, for the install in scaling-manager-system, lists them in
# every namespace, which a cluster-wide controller manages.
preflight_sources() {
    local first=true f
    for f in "$@"; do
        [ "$first" = true ] || echo '---'
        first=false
        cat "$ROOT/config/release/$f.yaml"
    done
}
pin_preflight() {
    IMAGE="$IMAGE_REPO@$IMAGE_DIGEST" yq '(select(.kind == "Job") | .spec.template.spec.containers[].image) = strenv(IMAGE)'
}
PREFLIGHT_WHAT="The preflight check: a Job that reads the prerequisites and prints PASS, WARN, FAIL or UNKNOWN per check with the fix (docs/reference/preflight.md). Read-only, and it reads no Secret; part=admin is its ServiceAccount and RBAC, part=controller the Job. Delete the Job before applying it again, and the ClusterRoleBinding after the run."
preflight_sources preflight-serviceaccount preflight-role preflight-rolebinding preflight-clusterrole preflight-clusterrolebinding preflight-job \
  | pin_preflight | label "$PLACEHOLDER" \
  | write scaling-manager-preflight.yaml "$PREFLIGHT_WHAT (Namespaced install.)" "$SED_HOW"
preflight_sources preflight-serviceaccount preflight-role preflight-rolebinding preflight-cluster-clusterrole preflight-cluster-clusterrolebinding preflight-job \
  | sed "s/$PLACEHOLDER/$CLUSTER_NS/g" | pin_preflight \
  | yq '(select(.kind == "Job") | .spec.template.spec.containers[0].args) += ["--scope=cluster"]' \
  | label "$CLUSTER_NS" \
  | write scaling-manager-preflight-cluster.yaml "$PREFLIGHT_WHAT (Cluster-wide install, in $CLUSTER_NS; applies as is.)" ""

# Every file holds at least one object, or the render went wrong somewhere.
for f in "${RELEASE_FILES[@]}"; do
    n="$(yq ea '[.] | map(select(.kind != null)) | length' "$OUT/$f")"
    [ "$n" -gt 0 ] || die "$f holds no object"
done

# The operator tools as scripts, for a machine with no container runtime: the
# same files the tools image carries at /opt/scaling-manager-tools
# (Dockerfile.tools), under scaling-manager-tools/. Reproducible: sorted,
# owner 0, mtime 0, gzip without a name or time, so a re-run is byte-identical
# and a published release's SHA256SUMS still matches.
TOOLS_TARBALL="scaling-manager-tools-$VERSION.tar.gz"
TOOLS_SCRIPTS=(scaling-manager-tools.sh prepull.sh weights.sh enginecache.sh utilization-share.sh)
# GNU tar, as tar or (macOS, brew install gnu-tar) as gtar: --sort, --owner
# and --mtime are what make the archive reproducible, and bsdtar has none.
TAR=""
for t in tar gtar; do
    if command -v "$t" >/dev/null 2>&1 && "$t" --version 2>/dev/null | grep -q 'GNU tar'; then TAR="$t"; break; fi
done
[ -n "$TAR" ] || die "GNU tar is required to package $TOOLS_TARBALL reproducibly (on macOS: brew install gnu-tar, which installs gtar)"
stage="$WORK/tools/scaling-manager-tools"
mkdir -p "$stage/lib"
for f in "${TOOLS_SCRIPTS[@]}"; do
    install -m 0755 "$ROOT/deploy/$f" "$stage/$f"
done
# deploy/lib as the repository tracks it: in a git checkout, the tracked files
# only, so nothing generated or left over (a __pycache__, an editor's backup)
# is shipped; outside one (a source archive), every file but __pycache__.
if git -C "$ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    lib_files="$(git -C "$ROOT" ls-files -- deploy/lib)"
else
    lib_files="$(cd "$ROOT" && find deploy/lib -type f ! -path '*/__pycache__/*' | sort)"
fi
[ -n "$lib_files" ] || die "found no files under deploy/lib to package"
while IFS= read -r f; do
    rel="${f#deploy/}"
    mkdir -p "$stage/$(dirname "$rel")"
    install -m 0644 "$ROOT/$f" "$stage/$rel"
done <<< "$lib_files"
printf '%s\n' "$VERSION" > "$stage/VERSION"
chmod 0644 "$stage/VERSION"
find "$stage" -type d -exec chmod 0755 {} +
rm -f "$OUT"/scaling-manager-tools-*.tar.gz
"$TAR" --sort=name --owner=0 --group=0 --numeric-owner --mtime=@0 \
    -C "$WORK/tools" -cf - scaling-manager-tools | gzip -n -9 > "$OUT/$TOOLS_TARBALL" \
    || die "could not package $TOOLS_TARBALL"
[ -s "$OUT/$TOOLS_TARBALL" ] || die "$TOOLS_TARBALL is empty"

(cd "$OUT" && sha256sum "${RELEASE_FILES[@]}" "$TOOLS_TARBALL" > SHA256SUMS)
for f in "${RELEASE_FILES[@]}"; do
    printf '%-45s %3s objects\n' "$f" "$(yq ea '[.] | map(select(.kind != null)) | length' "$OUT/$f")"
done
printf '%-45s %3s files\n' "$TOOLS_TARBALL" "$("$TAR" -tzf "$OUT/$TOOLS_TARBALL" | grep -vc '/$')"
echo "Wrote ${#RELEASE_FILES[@]} manifests, $TOOLS_TARBALL and SHA256SUMS to $OUT (image $IMAGE_REPO@$IMAGE_DIGEST, version $VERSION)"
