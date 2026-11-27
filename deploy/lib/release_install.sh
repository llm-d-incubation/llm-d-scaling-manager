#!/usr/bin/env bash
#
# The install from the tools image: smt admin-setup, smt install, smt
# admin-finish, smt release-files. Design: docs/proposals/two-command-install.md.
# Sourced by deploy/scaling-manager-tools.sh after common.sh, constants.sh and
# scaler_tls.sh.
#
# The objects are the release files, baked into the image at
# /opt/scaling-manager-tools/release (Dockerfile.tools) or given with --files.
# Nothing here renders an object of its own except the settings values, the
# admin stamp, the Prometheus CA Secret and the namespace of a cluster-wide
# install; the KEDA side of mutual TLS is llmsm_scaler_keda_auth_apply
# (scaler_tls.sh), the function make setup-prereqs runs.
#
# Exit codes, the same for every command (docs/reference/tools-install.md):
#   0 done                     4 the cluster is not ready
#   1 the preflight has a FAIL 5 timed out
#   2 usage, or unusable files 6 --strict: an UNKNOWN the install cannot wait out
#   3 needs the cluster admin  7 a kubectl call failed
#
# Requires funcs: log_* and kc_capture (common.sh), llmsm_keda_apiservice_ns and
# llmsm_scaler_np_keda_ns (common.sh), llmsm_scaler_* (scaler_tls.sh). kubectl,
# jq, yq and sha256sum on PATH.

SMT_PART_LABEL=scaling-manager.llmd.ai/part
SMT_STAMP=scaling-manager-admin-setup
SMT_SETTINGS=scaling-manager-settings
SMT_PROM_CA_SECRET=scaling-manager-prometheus-ca
SMT_PROM_CA_PATH=/etc/scaling-manager-prometheus-ca/ca.crt
SMT_DEPLOY=scaling-manager-controller-manager
SMT_STATUS_CM=scaling-manager-controller-status
SMT_PREFLIGHT_JOB=scaling-manager-preflight
# On the preflight Job with --allow-http: the controller is applied with
# PROMETHEUS_TOKEN_PATH empty (internal/preflight TokenPathEmptiedEnv).
SMT_TOKEN_EMPTIED_ENV=LLMSM_PREFLIGHT_TOKEN_PATH_EMPTIED
# On the preflight Job: the namespaces the controller is about to manage
# (internal/preflight WatchNamespaceEnv), when they differ from the live ones.
SMT_PREFLIGHT_WATCH_ENV=LLMSM_PREFLIGHT_WATCH_NAMESPACE
SMT_CLUSTER_NS=scaling-manager-system
SMT_THANOS_URL=https://thanos-querier.openshift-monitoring.svc.cluster.local:9091
# The checks the preflight cannot make before the controller runs: UNKNOWN in
# the first run is expected, and the run after the install makes them.
SMT_NEEDS_CONTROLLER="rbac.controller keda.mtls scaler.tls"
# The migration section named when the live controller is not from the release
# files (an install made with make, before it renders from them).
SMT_MIGRATION_DOC="docs/proposals/install-from-release.md#phases"

# smt_exit <code> <message>: the message on stderr, then the exit code.
smt_exit() {
    echo "smt: $2" >&2
    exit "$1"
}
smt_usage() { smt_exit 2 "$1"; }

# smt_poll_step echoes the seconds one poll counts against a timeout: the
# sleep, SMT_POLL (default 3), and at least 1, so every wait ends.
smt_poll_step() {
    local p="${SMT_POLL:-3}"
    case "$p" in ""|*[!0-9]*) p=3 ;; esac
    [ "$p" -gt 0 ] || p=1
    printf '%s\n' "$p"
}

# smt_call <function> <args...>: a deploy/lib reader that echoes its answer,
# run in this shell (not in $( )), so the KC_ERR it leaves names the failure.
# The answer is in SMT_CALL_OUT; the status is the function's.
smt_call() {
    local rc=0
    "$@" > "$SMT_TMP/call.out" || rc=$?
    SMT_CALL_OUT="$(cat "$SMT_TMP/call.out")"
    return "$rc"
}

# smt_kubectl_failed <what>: a kubectl call failed; exit 7 naming it and why.
smt_kubectl_failed() {
    smt_exit 7 "kubectl $1 failed: $(kc_err_line "$KC_ERR")"
}

# smt_kc <args...>: kubectl, or exit 7. Output in KC_OUT.
smt_kc() {
    kc_capture "$@" || smt_kubectl_failed "$*"
}

# smt_get <resource> <name> [-n ns]: KC_OUT holds the object as JSON and it
# returns 0; returns 1 when it does not exist; exits 7 on any other failure
# (Forbidden is not absent).
smt_get() {
    local rc=0
    kc_capture get "$@" -o json || rc=$?
    [ "$rc" = 0 ] && return 0
    printf '%s' "$KC_ERR" | grep -qF '(NotFound)' && return 1
    smt_kubectl_failed "get $*"
}

# smt_check_ns <flag> <value>: a namespace name, or exit 2.
smt_check_ns() {
    case "$2" in
        ""|*[!a-z0-9-]*|-*|*-) smt_usage "$1: '$2' is not a namespace name" ;;
    esac
    [ "${#2}" -le 63 ] || smt_usage "$1: '$2' is longer than 63 characters"
}

# --- Flags -------------------------------------------------------------------

# smt_parse <command> <args...>: the flags every command shares, into SMT_*.
# A flag the command does not take is a usage error.
smt_parse() {
    local cmd="$1" allowed
    shift
    SMT_NS="" SMT_KEDA_NS="" SMT_PROM_URL="" SMT_PROM_CA="" SMT_QUOTA="" SMT_SETS=() SMT_FILES=""
    SMT_DRY_RUN=false SMT_OUT="" SMT_SCOPE=namespace SMT_MODEL_NS=() SMT_PLATFORM="" SMT_STRICT=false
    SMT_RESTART_KEDA=false SMT_IMAGE_REPO="" SMT_WL_IMAGE="" SMT_WL_OFF=false SMT_ALLOW_HTTP=false SMT_SINGLE=false SMT_DSETW=false SMT_NO_DSETW=false
    SMT_SHARE="" SMT_MS_PORT="" SMT_NO_EPP_MON=false
    case "$cmd" in
        admin-setup) allowed=" -n --keda-namespace --files --dry-run --out --platform --scope --model-namespace --single-namespace --dset-role-writer --no-dset-role-writer --modelserver-port --no-epp-monitor " ;;
        install) allowed=" -n --keda-namespace --prometheus-url --prometheus-ca --quota --utilization-share --set --files --dry-run --out --scope --model-namespace --platform --strict --restart-keda --image-repo --allow-http --weights-labeller-image --no-weights-labeller --dset-role-writer --no-dset-role-writer --modelserver-port --no-epp-monitor " ;;
        admin-finish) allowed=" -n --keda-namespace --files --platform --strict --restart-keda --scope " ;;
        release-files) allowed=" --out " ;;
    esac
    while [ $# -gt 0 ]; do
        local flag="$1"
        [ "$flag" = --namespace ] && flag=-n
        case " $allowed " in
            *" $flag "*) ;;
            *) smt_usage "$cmd: unknown option $1 (see $cmd --help)" ;;
        esac
        case "$flag" in
            --dry-run|--strict|--restart-keda|--no-weights-labeller|--allow-http|--single-namespace|--dset-role-writer|--no-dset-role-writer|--no-epp-monitor) ;;
            *) [ $# -ge 2 ] && [ -n "$2" ] || smt_usage "$cmd: $1 needs a value" ;;
        esac
        case "$flag" in
            -n) SMT_NS="$2"; shift 2 ;;
            --keda-namespace) SMT_KEDA_NS="$2"; shift 2 ;;
            --prometheus-url) SMT_PROM_URL="$2"; shift 2 ;;
            --prometheus-ca) SMT_PROM_CA="$2"; shift 2 ;;
            --quota) SMT_QUOTA="$2"; shift 2 ;;
            --utilization-share) SMT_SHARE="$2"; shift 2 ;;
            --modelserver-port) SMT_MS_PORT="$2"; shift 2 ;;
            --no-epp-monitor) SMT_NO_EPP_MON=true; shift ;;
            --set) SMT_SETS+=("$2"); shift 2 ;;
            --files) SMT_FILES="$2"; shift 2 ;;
            --out) SMT_OUT="$2"; shift 2 ;;
            --scope) SMT_SCOPE="$2"; shift 2 ;;
            --model-namespace) SMT_MODEL_NS+=("$2"); shift 2 ;;
            --platform) SMT_PLATFORM="$2"; shift 2 ;;
            --image-repo) SMT_IMAGE_REPO="$2"; shift 2 ;;
            --weights-labeller-image) SMT_WL_IMAGE="$2"; shift 2 ;;
            --no-weights-labeller) SMT_WL_OFF=true; shift ;;
            --dry-run) SMT_DRY_RUN=true; shift ;;
            --strict) SMT_STRICT=true; shift ;;
            --restart-keda) SMT_RESTART_KEDA=true; shift ;;
            --allow-http) SMT_ALLOW_HTTP=true; shift ;;
            --dset-role-writer) SMT_DSETW=true; shift ;;
            --no-dset-role-writer) SMT_NO_DSETW=true; shift ;;
            --single-namespace) SMT_SINGLE=true; shift ;;
        esac
    done
    case "$SMT_SCOPE" in
        namespace|cluster) ;;
        *) smt_usage "$cmd: --scope is namespace or cluster, not '$SMT_SCOPE'" ;;
    esac
    case "$SMT_PLATFORM" in
        ""|kubernetes|openshift) ;;
        *) smt_usage "$cmd: --platform is kubernetes or openshift, not '$SMT_PLATFORM'" ;;
    esac
    if [ "$SMT_SCOPE" = cluster ]; then
        case "$cmd" in
            admin-setup|admin-finish)
                smt_usage "$cmd does not take --scope cluster: a cluster-wide install has one actor, so one command does it all: smt install --scope cluster --keda-namespace <KEDA namespace> --prometheus-url <url> --model-namespace <ns>" ;;
        esac
        [ -z "$SMT_NS" ] || [ "$SMT_NS" = "$SMT_CLUSTER_NS" ] \
            || smt_usage "$cmd --scope cluster installs in $SMT_CLUSTER_NS, which the cluster-wide files name; leave out -n"
        SMT_NS="$SMT_CLUSTER_NS"
    elif [ "$cmd" != release-files ]; then
        [ -n "$SMT_NS" ] || smt_usage "$cmd needs -n <namespace>: the namespace the model servers run in"
    fi
    [ "$cmd" = release-files ] || smt_check_ns "$cmd -n" "$SMT_NS"
    [ -z "$SMT_KEDA_NS" ] || smt_check_ns "$cmd --keda-namespace" "$SMT_KEDA_NS"
    local m
    for m in "${SMT_MODEL_NS[@]}"; do smt_check_ns "$cmd --model-namespace" "$m"; done
    if [ "$SMT_SCOPE" = namespace ] && [ ${#SMT_MODEL_NS[@]} -gt 0 ]; then
        # The rule make applies to LLMSM_WATCH_NS (llmsm_resolve_watch_namespaces).
        for m in "${SMT_MODEL_NS[@]}"; do
            [ "$m" != "$SMT_NS" ] || smt_usage "$cmd: --model-namespace $m is -n, the namespace the controller runs in. A controller that manages other namespaces runs in a namespace of its own: leave $m out, or run it in another namespace"
        done
        mapfile -t SMT_MODEL_NS < <(printf '%s\n' "${SMT_MODEL_NS[@]}" | LC_ALL=C sort -u)
    fi
    [ "$SMT_SINGLE" = false ] || [ ${#SMT_MODEL_NS[@]} -eq 0 ] \
        || smt_usage "$cmd: --single-namespace and --model-namespace together; --single-namespace drops every model namespace"
    [ "$SMT_DSETW" = false ] || [ "$SMT_NO_DSETW" = false ] \
        || smt_usage "$cmd: --dset-role-writer and --no-dset-role-writer together; one or the other"
    if [ "$SMT_DRY_RUN" = true ]; then
        [ -n "$SMT_OUT" ] || smt_usage "$cmd --dry-run needs --out <dir>: the rendered objects are written there"
    elif [ "$cmd" != release-files ] && [ -n "$SMT_OUT" ]; then
        smt_usage "$cmd: --out is for --dry-run"
    fi
    if [ -n "$SMT_IMAGE_REPO" ]; then
        case "$SMT_IMAGE_REPO" in
            *@*|*/|/*) smt_usage "$cmd --image-repo: give REGISTRY/PATH, without a tag or digest" ;;
        esac
        case "${SMT_IMAGE_REPO##*/}" in *:*) smt_usage "$cmd --image-repo: give REGISTRY/PATH, without a tag" ;; esac
    fi
    if [ "$SMT_SCOPE" != cluster ] && { [ -n "$SMT_WL_IMAGE" ] || [ "$SMT_WL_OFF" = true ]; }; then
        smt_usage "$cmd: --weights-labeller-image and --no-weights-labeller are for --scope cluster; in one namespace, the cluster admin runs: smt weights labeller -n <ns> --image <tools image>"
    fi
    [ -z "$SMT_WL_IMAGE" ] || [ "$SMT_WL_OFF" = false ] || smt_usage "$cmd: --weights-labeller-image and --no-weights-labeller together; one or the other"
    case "$SMT_WL_IMAGE" in
        *[!A-Za-z0-9._:/@-]*) smt_usage "$cmd --weights-labeller-image: '$SMT_WL_IMAGE' is not an image reference" ;;
    esac
    if [ -n "$SMT_PROM_CA" ] && [ ! -r "$SMT_PROM_CA" ]; then
        smt_usage "$cmd --prometheus-ca: cannot read '$SMT_PROM_CA' (a path under the mounted /work)"
    fi
    if [ -n "$SMT_QUOTA" ]; then
        printf '%s' "$SMT_QUOTA" | grep -qE '^[A-Za-z0-9._-]+=-?[0-9]+(,[A-Za-z0-9._-]+=-?[0-9]+)*$' \
            || smt_usage "$cmd --quota: '$SMT_QUOTA' is not GPU=N[,GPU=N]"
    fi
    case "$SMT_SHARE" in
        ""|off|shadow|active) ;;
        *) smt_usage "$cmd --utilization-share is off, shadow or active, not '$SMT_SHARE'" ;;
    esac
    if [ -n "$SMT_MS_PORT" ]; then
        if [[ "$SMT_MS_PORT" =~ ^[0-9]+$ ]]; then
            [ "${#SMT_MS_PORT}" -le 5 ] && [ "$((10#$SMT_MS_PORT))" -ge 1 ] && [ "$((10#$SMT_MS_PORT))" -le 65535 ] \
                || smt_usage "$cmd --modelserver-port: $SMT_MS_PORT is not a port number (1 to 65535)"
            SMT_MS_PORT="$((10#$SMT_MS_PORT))"
        else
            # A container port name (IANA_SVC_NAME): what the PodMonitor can name.
            { [ "${#SMT_MS_PORT}" -le 15 ] && [[ "$SMT_MS_PORT" =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$ ]] \
                && [[ "$SMT_MS_PORT" =~ [a-z] ]] && [[ "$SMT_MS_PORT" != *--* ]]; } \
                || smt_usage "$cmd --modelserver-port: '$SMT_MS_PORT' is neither a container port name (up to 15 lower-case letters, digits and dashes) nor a port number"
        fi
    fi
    if [ "$cmd" = install ] && [ "$SMT_SCOPE" = namespace ]; then
        [ -z "$SMT_MS_PORT" ] || smt_usage "install --modelserver-port: the model-server PodMonitor is the cluster admin's, applied by admin-setup. They run: smt admin-setup -n $SMT_NS --modelserver-port $SMT_MS_PORT"
        [ "$SMT_NO_EPP_MON" = false ] || smt_usage "install --no-epp-monitor: the EPP ServiceMonitor is the cluster admin's, added by admin-setup. Give it to smt admin-setup"
    fi
    if [ -n "$SMT_PROM_URL" ]; then
        smt_check_scheme "$SMT_PROM_URL" "--prometheus-url $SMT_PROM_URL"
    fi
    if [ "$SMT_ALLOW_HTTP" = true ] && [ -n "$SMT_PROM_CA" ]; then
        smt_usage "$cmd --allow-http with --prometheus-ca: a plain HTTP Prometheus has no certificate, and the controller refuses TLS settings beside an http URL"
    fi
}

# smt_is_http <url>: the URL is plain HTTP.
smt_is_http() {
    case "$(printf '%s' "$1" | tr 'A-Z' 'a-z')" in http://*) return 0 ;; esac
    return 1
}

# smt_check_scheme <url> <what names it>: --allow-http is given exactly when
# the Prometheus URL the controller reads is plain HTTP, or exit 2. With it,
# the settings allow HTTP and the controller is applied without a token (the
# controller refuses to send one over HTTP); without it, it is applied with
# its ServiceAccount token, which an http URL would make it refuse.
smt_check_scheme() {
    if smt_is_http "$1"; then
        [ "$SMT_ALLOW_HTTP" = true ] \
            || smt_usage "$2 is plain HTTP, which the controller refuses by default. Add --allow-http to read it unencrypted (the controller then sends Prometheus no token), or give an https URL"
    elif [ "$SMT_ALLOW_HTTP" = true ]; then
        smt_usage "--allow-http is for a plain HTTP Prometheus, and $2 is not one: leave --allow-http out"
    fi
}

# --- The release files -------------------------------------------------------

# smt_version echoes the release this image is, or empty in a checkout.
smt_version() {
    [ -f "$HERE/VERSION" ] && cat "$HERE/VERSION"
}

# smt_baked_dir echoes where the image carries the release files.
smt_baked_dir() {
    printf '%s\n' "${SMT_BAKED_DIR:-$HERE/release}"
}

# smt_load_files: copies the release files (--files, else those of the image) to a
# scratch directory, SMT_DIR, after checking SHA256SUMS and the version.
# Exits 2 when there are none, or they do not check out. Applies nothing.
smt_load_files() {
    local src="$SMT_FILES" v bad f n
    if [ -z "$src" ]; then
        src="$(smt_baked_dir)"
        if [ ! -f "$src/SHA256SUMS" ]; then
            smt_usage "this image carries no release files (a development build). Give --files <dir> with the release files and their SHA256SUMS, or run the release's tools image"
        fi
    fi
    [ -d "$src" ] || smt_usage "--files $src: no such directory"
    [ -f "$src/SHA256SUMS" ] || smt_usage "--files $src: no SHA256SUMS there; download it with the files"
    # --ignore-missing: SHA256SUMS lists the tools tarball, which the image
    # does not carry. sha256sum still fails when it verified no file at all.
    if ! (cd "$src" && sha256sum -c --ignore-missing --quiet SHA256SUMS) > "$SMT_TMP/sums.out" 2>&1; then
        smt_usage "the release files in $src do not match their SHA256SUMS; refusing them: $(tr '\n' ' ' < "$SMT_TMP/sums.out")"
    fi
    SMT_DIR="$SMT_TMP/files"
    mkdir -p "$SMT_DIR"
    n=0
    for f in "$src"/scaling-manager.yaml "$src"/scaling-manager-*.yaml; do
        [ -f "$f" ] || continue
        # Listed in SHA256SUMS, or it was not checked.
        grep -qE "  ${f##*/}\$" "$src/SHA256SUMS" || smt_usage "${f##*/} in $src is not listed in its SHA256SUMS"
        cp "$f" "$SMT_DIR/"
        n=$((n + 1))
    done
    [ "$n" -gt 0 ] || smt_usage "no release files in $src"
    v="$(smt_version)"
    if [ -n "$v" ]; then
        bad="$(cd "$SMT_DIR" && for f in scaling-manager*.yaml; do
            yq ea '[.metadata.labels["app.kubernetes.io/version"] // ""] | .[]' "$f" 2>/dev/null | grep -vxF -- "$v" | sed "s|^|$f: |" | head -1
        done | head -3)"
        [ -z "$bad" ] || smt_usage "these release files are not $v, the release of this image (${bad}). The install logic and the files ship together: use the tools image of the files' release"
    fi
    SMT_FILES_FROM="$src"
}

# smt_need <file...>: each release file is present, or exit 2.
smt_need() {
    local f
    for f in "$@"; do
        [ -f "$SMT_DIR/$f" ] || smt_usage "the release files lack $f"
    done
}

# smt_files_for: the file names for SMT_SCOPE and SMT_PLATFORM.
smt_files_for() {
    local suffix=""
    [ "$SMT_PLATFORM" = openshift ] && suffix=-openshift
    if [ "$SMT_SCOPE" = cluster ]; then
        SMT_MAIN="scaling-manager-cluster$suffix.yaml"
        SMT_PREFLIGHT=scaling-manager-preflight-cluster.yaml
        SMT_PREFLIGHT_CRB=scaling-manager-preflight
        SMT_DSETW_FILE=scaling-manager-dset-role-writer-cluster.yaml
    else
        SMT_MAIN="scaling-manager-namespaced$suffix.yaml"
        SMT_PREFLIGHT=scaling-manager-preflight.yaml
        SMT_PREFLIGHT_CRB="scaling-manager-preflight-$SMT_NS"
        SMT_DSETW_FILE=scaling-manager-dset-role-writer.yaml
    fi
}

# smt_admin_hash echoes the hash of the admin objects as shipped: the
# part=admin objects of the main and preflight files and the model-namespace
# file, before any substitution, without the version label (a release that does
# not change them needs no admin). Read from the unsubstituted copy.
smt_admin_hash() {
    local f
    for f in "$SMT_MAIN" scaling-manager-preflight.yaml scaling-manager-model-namespace.yaml; do
        [ "$SMT_SCOPE" = cluster ] && [ "$f" = scaling-manager-preflight.yaml ] && f="$SMT_PREFLIGHT"
        PART="$SMT_PART_LABEL" yq ea -o json -I 0 \
            'select(.kind != null and .metadata.labels[strenv(PART)] == "admin") | del(.metadata.labels["app.kubernetes.io/version"])' \
            "$SMT_SHIPPED/$f"
    done | sha256sum | awk '{print $1}'
}

# smt_render: SMT_SHIPPED keeps the files as shipped; the copies in SMT_DIR get
# the namespaces, the KEDA namespace in the scaler NetworkPolicy, and
# --image-repo.
smt_render() {
    local f model="${1:-$SMT_NS}"
    SMT_SHIPPED="$SMT_TMP/shipped"
    if [ ! -d "$SMT_SHIPPED" ]; then
        mkdir -p "$SMT_SHIPPED"
        cp "$SMT_DIR"/scaling-manager*.yaml "$SMT_SHIPPED/"
    fi
    for f in "$SMT_DIR"/scaling-manager*.yaml; do
        sed -e "s/__NAMESPACE__/$SMT_NS/g" -e "s/__MODEL_NAMESPACE__/$model/g" "$SMT_SHIPPED/${f##*/}" > "$f" \
            || smt_exit 2 "cannot render ${f##*/} for namespace $SMT_NS"
    done
    if [ -n "${SMT_KEDA_NS:-}" ]; then
        # The same rewrite deploy/release-manifests.sh makes when it writes the
        # platform default, here with the namespace KEDA runs in.
        KEDA_NS="$SMT_KEDA_NS" yq -i '(select(.kind == "NetworkPolicy" and .metadata.name == "scaling-manager-external-scaler") | .spec.ingress[] | select(has("from")) | .from[] | select(has("namespaceSelector")) | .namespaceSelector.matchLabels | select(has("kubernetes.io/metadata.name")) | .["kubernetes.io/metadata.name"]) = strenv(KEDA_NS)' \
            "$SMT_DIR/$SMT_MAIN" || smt_exit 2 "cannot write KEDA namespace $SMT_KEDA_NS into $SMT_MAIN"
    fi
    if [ "${SMT_ALLOW_HTTP:-false}" = true ]; then
        # The controller refuses to send its token over plain HTTP, and env
        # beats the settings: empty PROMETHEUS_TOKEN_PATH by name, as
        # llmsm_reconcile_prometheus_scheme does for make. Rendered rather than
        # patched, so a re-run with the same flags applies the same object.
        D="$SMT_DEPLOY" yq -i '(select(.kind == "Deployment" and .metadata.name == strenv(D)) | .spec.template.spec.containers[] | select(.name == "manager") | .env[] | select(.name == "PROMETHEUS_TOKEN_PATH") | .value) = ""' \
            "$SMT_DIR/$SMT_MAIN" || smt_exit 2 "cannot empty PROMETHEUS_TOKEN_PATH in $SMT_MAIN"
        # The preflight runs before this Deployment is applied: tell it, so the
        # token path of a live controller (one crash-looping on it, say) is
        # not held against the install that removes it.
        J="$SMT_PREFLIGHT_JOB" E="$SMT_TOKEN_EMPTIED_ENV" yq -i '(select(.kind == "Job" and .metadata.name == strenv(J)) | .spec.template.spec.containers[] | select(.name == "preflight") | .env) += [{"name": strenv(E), "value": "true"}]' \
            "$SMT_DIR/$SMT_PREFLIGHT" || smt_exit 2 "cannot set $SMT_TOKEN_EMPTIED_ENV in $SMT_PREFLIGHT"
    fi
    if [ -n "$SMT_IMAGE_REPO" ]; then
        for f in "$SMT_MAIN" "$SMT_PREFLIGHT"; do
            REPO="$SMT_IMAGE_REPO" yq -i '(select(.kind == "Deployment" or .kind == "Job") | .spec.template.spec.containers[] | select(.image | test("@sha256:")) | .image) |= (strenv(REPO) + "@" + sub("^[^@]*@", ""))' \
                "$SMT_DIR/$f" || smt_exit 2 "cannot move the images of $f to $SMT_IMAGE_REPO"
        done
    fi
}

# smt_model_file <model namespace>: the model-namespace file for that
# namespace, written to SMT_TMP; echoes its path.
smt_model_file() {
    local out="$SMT_TMP/model-namespace-$1.yaml"
    sed -e "s/__NAMESPACE__/$SMT_NS/g" -e "s/__MODEL_NAMESPACE__/$1/g" "$SMT_SHIPPED/scaling-manager-model-namespace.yaml" > "$out" \
        || smt_exit 2 "cannot render scaling-manager-model-namespace.yaml for $1"
    printf '%s\n' "$out"
}

# --- The model-server port and the EPP monitor ----------------------------------
#
# The model-namespace file carries PodMonitor scaling-manager-modelserver-metrics,
# which scrapes the container port named `modelserver`, as llm-d's own base
# names it. A PodMonitor naming a port no container declares has no target at
# all: nothing is scraped and nothing reports an error. --modelserver-port
# renders it for another port, by name or by number (targetPort, which every
# Prometheus Operator version reads). Without the flag a re-run keeps the port
# the live PodMonitor scrapes, as it keeps the model namespaces: a re-run must
# not take a working scrape away.
SMT_MS_PODMONITOR=scaling-manager-modelserver-metrics

# smt_ms_port_shipped echoes the port the release PodMonitor scrapes.
smt_ms_port_shipped() {
    yq ea 'select(.kind == "PodMonitor") | .spec.podMetricsEndpoints[0].port // ""' "$SMT_SHIPPED/scaling-manager-model-namespace.yaml"
}

# smt_ms_port_decide <namespace>: SMT_MS_PORT_USE, the port the PodMonitor
# there is rendered for: the flag; else the live one's when it differs from
# the shipped port; else empty (as shipped). --dry-run reads no cluster.
smt_ms_port_decide() {
    local ns="$1" live rc=0 shipped
    SMT_MS_PORT_USE="$SMT_MS_PORT"
    [ -z "$SMT_MS_PORT_USE" ] || return 0
    [ "$SMT_DRY_RUN" != true ] || return 0
    kc_capture get podmonitor "$SMT_MS_PODMONITOR" -n "$ns" -o json || rc=$?
    if [ "$rc" != 0 ]; then
        printf '%s' "$KC_ERR" | grep -qF '(NotFound)' && return 0
        smt_kubectl_failed "get podmonitor $SMT_MS_PODMONITOR -n $ns"
    fi
    live="$(printf '%s' "$KC_OUT" | jq -r '(.spec.podMetricsEndpoints[0] // {}) | (.port // .targetPort // "") | tostring')"
    shipped="$(smt_ms_port_shipped)"
    [ -n "$live" ] && [ "$live" != "$shipped" ] || return 0
    SMT_MS_PORT_USE="$live"
    echo "  PodMonitor $SMT_MS_PODMONITOR in $ns scrapes port $live: kept (--modelserver-port $shipped goes back to the shipped port)."
}

# smt_ms_port_apply <file> <port>: the PodMonitor of the file scrapes <port>,
# a name or a number; nothing to do for an empty port.
smt_ms_port_apply() {
    local f="$1" p="$2" n
    [ -n "$p" ] || return 0
    n="$(yq ea '[select(.kind == "PodMonitor")] | length' "$f")"
    [ "$n" = 1 ] || smt_exit 2 "${f##*/} holds $n PodMonitors, not the one model-server PodMonitor"
    if [[ "$p" =~ ^[0-9]+$ ]]; then
        P="$p" yq -i '(select(.kind == "PodMonitor") | .spec.podMetricsEndpoints[]) |= (del(.port) | .targetPort = env(P))' "$f"
    else
        P="$p" yq -i '(select(.kind == "PodMonitor") | .spec.podMetricsEndpoints[]) |= (del(.targetPort) | .port = strenv(P))' "$f"
    fi || smt_exit 2 "cannot set the model-server port $p in ${f##*/}"
}

# smt_apply_model_ns <namespace> <file>: the model-namespace file, its
# PodMonitor scraping the port smt_ms_port_decide chose.
smt_apply_model_ns() {
    smt_ms_port_decide "$1"
    smt_ms_port_apply "$2" "$SMT_MS_PORT_USE"
    smt_apply "$2"
    [ -z "$SMT_MS_PORT_USE" ] || echo "  PodMonitor $SMT_MS_PODMONITOR scrapes port $SMT_MS_PORT_USE."
}

# smt_epp_monitors <namespace>: a ServiceMonitor for each EPP there that
# nothing scrapes yet, with the identity it scrapes as (the EPP authenticates
# /metrics). The arrival rate, the flow-control queue and scale from zero read
# the EPP's metrics, and the docs require them scraped; make setup-prereqs adds
# the same objects (llmsm_ensure_llmd_monitors, whose functions this calls).
# An EPP already scraped, by llm-d's router monitoring say, gets none: a
# second scrape doubles every rate read from it. Never fatal.
smt_epp_monitors() {
    local ns="$1" svc svcs monitors f rc=0
    [ "$SMT_NO_EPP_MON" = false ] || { echo "  EPP ServiceMonitor: not added (--no-epp-monitor)."; return 0; }
    kc_capture get crd servicemonitors.monitoring.coreos.com -o name || rc=$?
    if [ "$rc" != 0 ]; then
        if printf '%s' "$KC_ERR" | grep -qF '(NotFound)'; then
            echo "  EPP ServiceMonitor: no ServiceMonitor API on this cluster (no Prometheus Operator), so none added. Scrape the EPP in $ns some other way."
        else
            echo "  EPP ServiceMonitor: could not tell whether the ServiceMonitor API is served ($(kc_err_line "$KC_ERR")), so none added."
        fi
        return 0
    fi
    # The owner label of llmsm_epp_scrape_docs: this controller's namespace.
    LLMSM_NS="$SMT_NS"
    svcs="$(llmsm_epp_services "$ns")"
    if [ -z "$svcs" ]; then
        echo "  EPP ServiceMonitor: no InferencePool in $ns names an EPP, so none added."
        return 0
    fi
    for svc in $svcs; do
        monitors="$(llmsm_epp_scrapers "$ns" "$svc" | sed 's/^unevaluated://' | paste -sd, - | sed 's/,/, /g')"
        if [ -n "$monitors" ]; then
            echo "  EPP $svc: scraped already, or may be ($monitors); none added."
            continue
        fi
        f="$SMT_TMP/epp-scrape-$ns-$svc.yaml"
        if ! llmsm_epp_scrape_docs "$ns" "$svc" > "$f"; then
            echo "  EPP $svc: nothing scrapes it, and its Service names no metrics port (http-metrics or metrics), so no ServiceMonitor was added. Scrape it yourself."
            continue
        fi
        echo "  EPP $svc: nothing scrapes it; ServiceMonitor $LLMSM_EPP_SCRAPE_NAME, and the ServiceAccount it scrapes as:"
        smt_apply "$f"
    done
}

# smt_select <part> <file>: the objects of that part.
smt_select() {
    PART="$SMT_PART_LABEL" P="$1" yq ea 'select(.kind != null and .metadata.labels[strenv(PART)] == strenv(P))' "$2"
}

# --- The cluster -------------------------------------------------------------

# smt_detect_platform sets SMT_PLATFORM from --platform, else API discovery
# (security.openshift.io), which any authenticated user may read.
smt_detect_platform() {
    [ -z "$SMT_PLATFORM" ] || return 0
    if [ "$SMT_DRY_RUN" = true ]; then
        SMT_PLATFORM=kubernetes
        return 0
    fi
    smt_kc api-versions
    if printf '%s\n' "$KC_OUT" | grep -q '^security\.openshift\.io/'; then
        SMT_PLATFORM=openshift
    else
        SMT_PLATFORM=kubernetes
    fi
}

# smt_require_namespace: the namespace exists, or exit 4. Never creates it.
smt_require_namespace() {
    local rc=0
    kc_capture get namespace "$SMT_NS" -o name || rc=$?
    [ "$rc" = 0 ] && return 0
    if printf '%s' "$KC_ERR" | grep -qF '(NotFound)'; then
        smt_exit 4 "namespace $SMT_NS does not exist. Create it, or name the namespace the model servers run in"
    fi
    printf '%s' "$KC_ERR" | grep -qF '(Forbidden)' || smt_kubectl_failed "get namespace $SMT_NS"
    # A namespace owner may not read Namespaces. Every namespace has the
    # ServiceAccount default, which an owner may read in their own.
    rc=0
    kc_capture get serviceaccount default -n "$SMT_NS" -o name || rc=$?
    [ "$rc" = 0 ] && return 0
    if printf '%s' "$KC_ERR" | grep -qF '(Forbidden)'; then
        smt_exit 4 "you may not read namespace $SMT_NS: is it the name of the namespace the model servers run in, and is it yours? ($(kc_err_line "$KC_ERR"))"
    fi
    printf '%s' "$KC_ERR" | grep -qF '(NotFound)' \
        && smt_exit 4 "namespace $SMT_NS does not exist (it has no ServiceAccount default). Name the namespace the model servers run in"
    smt_kubectl_failed "get serviceaccount default -n $SMT_NS"
}

# smt_resolve_keda_ns <command>: sets SMT_KEDA_NS. The flag; else the live
# source namespace of the scaler NetworkPolicy; else the namespace serving the
# external-metrics APIService when KEDA serves it (the detection make
# setup-prereqs uses); on OpenShift, openshift-keda. None: exit 4.
smt_resolve_keda_ns() {
    local found rc
    [ -n "$SMT_KEDA_NS" ] && { SMT_KEDA_FROM="--keda-namespace"; return 0; }
    if [ "$SMT_DRY_RUN" = true ]; then
        SMT_KEDA_NS=keda
        [ "$SMT_PLATFORM" = openshift ] && SMT_KEDA_NS=openshift-keda
        SMT_KEDA_FROM="the platform default (--dry-run reads no cluster)"
        return 0
    fi
    rc=0
    smt_call llmsm_scaler_np_keda_ns "$SMT_NS" || rc=$?
    found="$SMT_CALL_OUT"
    case "$rc" in
        0) SMT_KEDA_NS="$found"; SMT_KEDA_FROM="the scaler NetworkPolicy already in $SMT_NS"; return 0 ;;
        2) smt_exit 7 "cannot read NetworkPolicy scaling-manager-external-scaler in $SMT_NS: $(kc_err_line "$KC_ERR")" ;;
    esac
    rc=0
    smt_call llmsm_keda_apiservice_ns || rc=$?
    found="$SMT_CALL_OUT"
    case "$rc" in
        0) SMT_KEDA_NS="$found"; SMT_KEDA_FROM="the namespace serving the external-metrics APIService"; return 0 ;;
        2) smt_exit 7 "cannot read APIService v1beta1.external.metrics.k8s.io: $(kc_err_line "$KC_ERR")" ;;
    esac
    if [ "$SMT_PLATFORM" = openshift ]; then
        SMT_KEDA_NS=openshift-keda
        SMT_KEDA_FROM="the OpenShift default"
        return 0
    fi
    smt_exit 4 "cannot find the namespace KEDA runs in (no scaler NetworkPolicy in $SMT_NS, and the external-metrics APIService is not served by KEDA${found:+: $found}). Give --keda-namespace <namespace>"
}

# smt_apply <file> [part]: kubectl apply, the part only when given. Exit 7.
smt_apply() {
    local sel=()
    [ -z "${2:-}" ] || sel=(-l "$SMT_PART_LABEL=$2")
    smt_kc apply -f "$1" "${sel[@]}"
    printf '%s\n' "$KC_OUT" | sed '/^$/d; s/^/  /'
}

# smt_stamp_yaml: the admin stamp, a ConfigMap in SMT_NS.
smt_stamp_yaml() {
    jq -n --arg ns "$SMT_NS" --arg name "$SMT_STAMP" --arg part "$SMT_PART_LABEL" \
        --arg v "$(smt_version)" --arg keda "$SMT_KEDA_NS" --arg hash "$(smt_admin_hash)" --arg p "$SMT_PLATFORM" \
        --arg models "$(smt_model_list)" --arg dw "${SMT_DSETW_WANT:-false}" \
        '{apiVersion: "v1", kind: "ConfigMap",
          metadata: {name: $name, namespace: $ns,
                     labels: {"app.kubernetes.io/part-of": "scaling-manager", ($part): "admin"}},
          data: {release: $v, kedaNamespace: $keda, adminObjectsSha256: $hash, platform: $p, dsetRoleWriter: $dw}}
         | if $models != "" then .data.modelNamespaces = $models else . end' | yq -P
}

# smt_admin_line [keda namespace] [model namespaces, comma-separated]
# [role writer: true] echoes the admin-setup command for this namespace.
smt_admin_line() {
    local m line
    line="smt admin-setup -n $SMT_NS --keda-namespace ${1:-<the namespace KEDA runs in>}"
    for m in $(printf '%s' "${2:-}" | tr ',' ' '); do line="$line --model-namespace $m"; done
    [ "${3:-false}" != true ] || line="$line --dset-role-writer"
    printf '%s\n' "$line"
}

# --- The DisaggregatedSet role writer ------------------------------------------
#
# The grant of the role writer (DSET_ROLE_WRITER, internal/dsetwriter): get,
# list and patch on disaggregatedsets, where the controller lists them. A
# namespaced controller lists them in the namespaces it manages: each model
# namespace, or its own when it has none. So the grant is a Role and RoleBinding
# in each of those (named -<controller namespace> outside its own, like the
# preflight reads); a cluster-wide one gets a ClusterRole. admin-setup (and
# install --scope cluster) applies it with --dset-role-writer, removes it with
# --no-dset-role-writer, and without either keeps what the last run recorded, as
# it keeps the model namespaces: on the Namespace reader, a ClusterRole only a
# cluster admin writes, never the stamp, which the namespace owner may edit.
# install sets the switch, DSET_ROLE_WRITER=true, on the controller.
SMT_DSETW_ROLE=scaling-manager-dset-role-writer-role
SMT_DSETW_BINDING=scaling-manager-dset-role-writer-rolebinding
SMT_DSETW_ANNOTATION=scaling-manager.llmd.ai/dset-role-writer

# smt_dsetw_targets echoes the namespaces the grant goes in, one per line:
# the model namespaces, else the namespace of the controller.
smt_dsetw_targets() {
    if [ ${#SMT_MODEL_NS[@]} -gt 0 ]; then
        printf '%s\n' "${SMT_MODEL_NS[@]}"
    else
        printf '%s\n' "$SMT_NS"
    fi
}

# smt_dsetw_names <namespace>: the Role and RoleBinding names there.
smt_dsetw_names() {
    if [ "$1" = "$SMT_NS" ]; then
        printf '%s %s\n' "$SMT_DSETW_ROLE" "$SMT_DSETW_BINDING"
    else
        printf '%s %s\n' "$SMT_DSETW_ROLE-$SMT_NS" "$SMT_DSETW_BINDING-$SMT_NS"
    fi
}

# smt_dsetw_file <namespace>: the grant for that namespace (cluster scope: the
# ClusterRole), from the release file, written to SMT_TMP; echoes its path.
smt_dsetw_file() {
    local out="$SMT_TMP/dset-role-writer-$1.yaml" role binding
    smt_need "$SMT_DSETW_FILE"
    if [ "$SMT_SCOPE" = cluster ]; then
        cp "$SMT_DIR/$SMT_DSETW_FILE" "$out" || smt_exit 2 "cannot copy $SMT_DSETW_FILE"
        printf '%s\n' "$out"
        return 0
    fi
    read -r role binding <<< "$(smt_dsetw_names "$1")"
    sed -e "s/__NAMESPACE__/$SMT_NS/g" "$SMT_SHIPPED/$SMT_DSETW_FILE" \
        | M="$1" R="$role" B="$binding" yq ea '
            select(.kind != null)
            | .metadata.namespace = strenv(M)
            | (select(.kind == "Role") | .metadata.name) = strenv(R)
            | (select(.kind == "RoleBinding") | .metadata.name) = strenv(B)
            | (select(.kind == "RoleBinding") | .roleRef.name) = strenv(R)' > "$out" \
        || smt_exit 2 "cannot render $SMT_DSETW_FILE for $1"
    [ "$(yq ea '[select(.kind == "Role" or .kind == "RoleBinding")] | length' "$out")" = 2 ] \
        || smt_exit 2 "$SMT_DSETW_FILE does not hold one Role and one RoleBinding"
    printf '%s\n' "$out"
}

# smt_dsetw_recorded: the last admin-setup (install --scope cluster) granted
# the role writer: the annotation on the Namespace reader of this controller,
# or for a cluster-wide install the ClusterRole of the grant. Both are
# cluster-scoped, so only a cluster admin can have written them.
smt_dsetw_recorded() {
    if [ "$SMT_SCOPE" = cluster ]; then
        smt_get clusterrole "$SMT_DSETW_ROLE" || return 1
        [ "$(printf '%s' "$KC_OUT" | jq -r --arg k "$SMT_INSTANCE_LABEL" '.metadata.labels[$k] // ""')" = "$SMT_NS" ]
        return
    fi
    smt_get clusterrole "scaling-manager-policy-namespace-reader-role-$SMT_NS" || return 1
    [ "$(printf '%s' "$KC_OUT" | jq -r --arg a "$SMT_DSETW_ANNOTATION" '.metadata.annotations[$a] // ""')" = true ]
}

# smt_dsetw_decide sets SMT_DSETW_WANT, whether this run grants the role
# writer: the flags, else what the last run recorded (a re-run keeps it).
# --dry-run reads no cluster: the flag only.
smt_dsetw_decide() {
    SMT_DSETW_WANT=false
    if [ "$SMT_DSETW" = true ]; then
        SMT_DSETW_WANT=true
    elif [ "$SMT_NO_DSETW" = false ] && [ "$SMT_DRY_RUN" != true ] && smt_dsetw_recorded; then
        SMT_DSETW_WANT=true
        echo "DisaggregatedSet role writer: granted by the last run, so kept (give --no-dset-role-writer to remove it)."
    fi
    # Here, not in smt_dsetw_file, which runs inside $( ), where exit 2 would
    # end only the subshell. The file is read only when a grant is rendered.
    [ "$SMT_DSETW_WANT" = false ] || smt_need "$SMT_DSETW_FILE"
}

# smt_render_dsetw_record: a namespaced grant is recorded on the Namespace
# reader, for the next re-run. Without it the file carries no annotation, and
# kubectl apply removes the one applied before.
smt_render_dsetw_record() {
    [ "$SMT_SCOPE" = namespace ] && [ "${SMT_DSETW_WANT:-false}" = true ] || return 0
    local n
    R="scaling-manager-policy-namespace-reader-role-$SMT_NS" A="$SMT_DSETW_ANNOTATION" yq -i \
        '(select(.kind == "ClusterRole" and .metadata.name == strenv(R)) | .metadata.annotations[strenv(A)]) = "true"' \
        "$SMT_DIR/$SMT_MAIN" || smt_exit 2 "cannot record the role writer on the Namespace reader of $SMT_MAIN"
    n="$(R="scaling-manager-policy-namespace-reader-role-$SMT_NS" A="$SMT_DSETW_ANNOTATION" yq ea '[select(.kind == "ClusterRole" and .metadata.name == strenv(R)) | .metadata.annotations[strenv(A)]] | length' "$SMT_DIR/$SMT_MAIN")"
    [ "$n" = 1 ] || smt_exit 2 "$SMT_MAIN holds no Namespace reader scaling-manager-policy-namespace-reader-role-$SMT_NS to record the role writer on"
}

# smt_dsetw_revoke <namespace>: removes the grant of this controller there,
# binding first, through smt_revoke_one (only an object whose instance label
# names this controller).
smt_dsetw_revoke() {
    local role binding
    if [ "$SMT_SCOPE" = cluster ]; then
        smt_revoke_one "$SMT_NS" clusterrolebinding "$SMT_DSETW_BINDING"
        smt_revoke_one "$SMT_NS" clusterrole "$SMT_DSETW_ROLE"
        return 0
    fi
    read -r role binding <<< "$(smt_dsetw_names "$1")"
    smt_revoke_one "$1" rolebinding "$binding"
    smt_revoke_one "$1" role "$role"
}

# smt_dsetw_apply: with SMT_DSETW_WANT, the grant in each target namespace
# (cluster scope: the ClusterRole); every grant of this controller elsewhere,
# or all of them without it, removed. A namespace dropped from the model list
# loses it in smt_revoke_model.
smt_dsetw_apply() {
    local ns targets
    targets="$(smt_dsetw_targets)"
    if [ "$SMT_DSETW_WANT" = true ]; then
        echo "DisaggregatedSet role writer grant (get, list and patch on disaggregatedsets):"
        if [ "$SMT_SCOPE" = cluster ]; then
            smt_apply "$(smt_dsetw_file "$SMT_NS")"
            return 0
        fi
        while read -r ns; do
            smt_apply "$(smt_dsetw_file "$ns")"
        done <<< "$targets"
    else
        echo "DisaggregatedSet role writer: not granted; a grant an earlier run made is removed."
        if [ "$SMT_SCOPE" = cluster ]; then
            smt_dsetw_revoke "$SMT_NS"
            return 0
        fi
    fi
    # The namespaces that should hold no grant of this controller.
    for ns in "$SMT_NS" "${SMT_MODEL_NS[@]}"; do
        if [ "$SMT_DSETW_WANT" = true ] && printf '%s\n' "$targets" | grep -qxF -- "$ns"; then
            continue
        fi
        smt_dsetw_revoke "$ns"
    done
}

# smt_render_dsetw_switch <on: true|false>: DSET_ROLE_WRITER=true on the
# controller, or none, rendered so a re-run applies the same object. The make
# install sets it the same way (config/components/dset-role-writer*).
smt_render_dsetw_switch() {
    if [ "$1" = true ]; then
        D="$SMT_DEPLOY" yq -i '(select(.kind == "Deployment" and .metadata.name == strenv(D)) | .spec.template.spec.containers[] | select(.name == "manager") | .env) |= ((. // []) | map(select(.name != "DSET_ROLE_WRITER")) + [{"name": "DSET_ROLE_WRITER", "value": "true"}])' \
            "$SMT_DIR/$SMT_MAIN" || smt_exit 2 "cannot set DSET_ROLE_WRITER in $SMT_MAIN"
    else
        D="$SMT_DEPLOY" yq -i 'del(select(.kind == "Deployment" and .metadata.name == strenv(D)) | .spec.template.spec.containers[] | select(.name == "manager") | .env[] | select(.name == "DSET_ROLE_WRITER"))' \
            "$SMT_DIR/$SMT_MAIN" || smt_exit 2 "cannot remove DSET_ROLE_WRITER from $SMT_MAIN"
    fi
}

# smt_install_dsetw <stamp dsetRoleWriter> <keda namespace> <models>: a
# namespaced install decides the switch. --dset-role-writer: on, when the admin
# granted it, else exit 3. --no-dset-role-writer: off. Neither: as the live
# controller has it, while the grant stands. The stamp is the namespace
# owner's to edit, so the Role is read too, in each namespace the owner may
# read it in; one that is NotFound is exit 3, one that may not be read is
# left to the stamp.
smt_install_dsetw() {
    local dw="$1" ns role binding live="" on=false
    if [ "$SMT_DSETW" = true ]; then
        if [ "$dw" != true ]; then
            echo "--dset-role-writer turns the DisaggregatedSet role writer on, and the cluster admin has not granted it in $SMT_NS (ConfigMap $SMT_STAMP has no dsetRoleWriter: \"true\"). Ask them to run, with this tools image:" >&2
            echo "  $(smt_admin_line "$2" "$3" true)" >&2
            exit 3
        fi
        while read -r ns; do
            read -r role binding <<< "$(smt_dsetw_names "$ns")"
            kc_capture get role "$role" -n "$ns" -o name && continue
            if printf '%s' "$KC_ERR" | grep -qF '(NotFound)'; then
                echo "--dset-role-writer: ConfigMap $SMT_STAMP says the role writer is granted, but Role $role is not in $ns. Ask the cluster admin to run, with this tools image:" >&2
                echo "  $(smt_admin_line "$2" "$3" true)" >&2
                exit 3
            fi
            # Only a refusal is left to the stamp; any other failure is one.
            printf '%s' "$KC_ERR" | grep -qF '(Forbidden)' || smt_kubectl_failed "get role $role -n $ns"
            echo "  may not read Role $role in $ns; ConfigMap $SMT_STAMP says it is granted"
        done < <(smt_dsetw_targets)
        on=true
    elif [ "$SMT_NO_DSETW" = false ] && [ "$dw" = true ] && smt_get deployment "$SMT_DEPLOY" -n "$SMT_NS"; then
        live="$(printf '%s' "$KC_OUT" | jq -r '[.spec.template.spec.containers[]? | select(.name == "manager") | .env[]? | select(.name == "DSET_ROLE_WRITER") | .value][0] // ""')"
        if [ "$live" = true ]; then
            on=true
            echo "DisaggregatedSet role writer: on, as the live controller has it (give --no-dset-role-writer to turn it off)."
        fi
    fi
    SMT_DSETW_ON="$on"
    smt_render_dsetw_switch "$on"
}

# --- Several model namespaces ------------------------------------------------
#
# A namespaced controller that manages other namespaces than its own, the
# setup make installs with LLMSM_WATCH_NS (docs/guides/utilization-share-namespaced).
# admin-setup --model-namespace applies, in each, the model-namespace file (the
# Role of the controller, its RoleBinding, the model-server PodMonitor) and the
# two reads of the preflight there; names them in the Namespace reader; and
# writes the list into the stamp. A namespace left out of the list is revoked.
# install reads the list from the stamp and sets it on LLMSM_WATCH_NAMESPACE of
# the controller, as make does. Nothing cluster-wide is added on workloads.
SMT_WATCH_ENV=LLMSM_WATCH_NAMESPACE
SMT_INSTANCE_LABEL=app.kubernetes.io/instance
SMT_POLICY_LABEL=scaling-manager.llmd.ai/policy-namespace

# smt_model_list echoes SMT_MODEL_NS, comma-separated.
smt_model_list() {
    local IFS=,
    printf '%s\n' "${SMT_MODEL_NS[*]}"
}

# SMT_MGR_RB_RE matches the manager RoleBinding of any controller in a
# namespace: the one of a namespaced install there (no suffix), and the copy a
# controller in another namespace gets (smt: -<namespace>, make: -<hash>).
SMT_MGR_RB_RE='^scaling-manager-manager-rolebinding(-.*)?$'
SMT_POD_WRITER_ROLE=scaling-manager-pod-writer
SMT_MODELS_ANNOTATION=scaling-manager.llmd.ai/model-namespaces

# smt_admin_recorded_models echoes the model namespaces the last admin-setup
# recorded on the Namespace reader of this controller, comma-separated. A
# cluster-scoped object, so only a cluster admin can have written it. Empty
# when there is none. Forbidden is not absent: exit 7.
smt_admin_recorded_models() {
    smt_get clusterrole "scaling-manager-policy-namespace-reader-role-$SMT_NS" || return 0
    printf '%s' "$KC_OUT" | jq -r --arg a "$SMT_MODELS_ANNOTATION" '.metadata.annotations[$a] // ""'
}

# smt_granted_models echoes the namespaces, other than SMT_NS, holding the
# model-namespace RoleBinding of this controller, one per line, sorted. Those
# a cluster admin granted: the stamp is not read here, since the namespace
# owner may edit it. Forbidden is not absent: exit 7.
smt_granted_models() {
    smt_kc get rolebindings -A -l "$SMT_INSTANCE_LABEL=$SMT_NS" -o json
    printf '%s' "$KC_OUT" | jq -r --arg n "scaling-manager-manager-rolebinding-$SMT_NS" --arg ns "$SMT_NS" --arg k "$SMT_INSTANCE_LABEL" \
        '.items[] | select(.metadata.name == $n and .metadata.namespace != $ns and .metadata.labels[$k] == $ns) | .metadata.namespace' \
        | LC_ALL=C sort -u
}

# smt_other_controllers <namespace>: echoes the manager RoleBindings of other
# controllers there, space-separated (empty when none). Forbidden: exit 7.
smt_other_controllers() {
    smt_kc get rolebindings -n "$1" -o json
    printf '%s' "$KC_OUT" | jq -r --arg re "$SMT_MGR_RB_RE" --arg own "scaling-manager-manager-rolebinding-$SMT_NS" \
        '[.items[] | .metadata.name | select(test($re) and . != $own)] | join(" ")'
}

# smt_cluster_guard: a namespaced admin-setup that would overlap a cluster-wide
# controller is refused (exit 4): one with model namespaces (the cluster-wide
# one manages every namespace), and one in the namespace of the cluster-wide
# files, whose grants carry the same names. Read before any write.
smt_cluster_guard() {
    local why=""
    [ ${#SMT_MODEL_NS[@]} -gt 0 ] || [ "$SMT_NS" = "$SMT_CLUSTER_NS" ] || return 0
    if smt_get deployment "$SMT_DEPLOY" -n "$SMT_CLUSTER_NS" \
        && ! printf '%s' "$KC_OUT" | jq -e '[.spec.template.spec.containers[]? | select(.name == "manager") | .args[]? | select(startswith("--watch-namespace"))] | length > 0' >/dev/null; then
        why="Deployment $SMT_DEPLOY in $SMT_CLUSTER_NS, with no --watch-namespace"
    elif smt_get clusterrole scaling-manager-manager-role && smt_get clusterrolebinding scaling-manager-manager-rolebinding; then
        # The grant of the cluster-wide files without their controller: an
        # install under way, or one removed in part.
        why="ClusterRole scaling-manager-manager-role and ClusterRoleBinding scaling-manager-manager-rolebinding, the grant of the cluster-wide files. If they are left over from a removed install: kubectl delete clusterrolebinding scaling-manager-manager-rolebinding; kubectl delete clusterrole scaling-manager-manager-role"
    fi
    [ -z "$why" ] || smt_exit 4 "a cluster-wide scaling manager is installed ($why). It manages every namespace already, and a second controller for the same models is an overlap the preflight FAILs on. Remove it first, or manage these namespaces from it (smt install --scope cluster --model-namespace <ns>)"
}

# smt_admin_models: the model namespaces admin-setup applies, before any
# write. --single-namespace: none. Else the flags; without them, the
# namespaces this controller is granted in now, so a re-run (an upgrade) keeps
# them. Each must exist, and no other controller may manage it (exit 4). Sets
# SMT_PREV_MODELS, the namespaces granted before, and SMT_POLICY_NAMES.
smt_admin_models() {
    local m found others rest
    smt_call smt_granted_models
    SMT_PREV_MODELS="$(printf '%s\n' "$SMT_CALL_OUT" | grep -v '^$' | paste -sd' ' -)"
    SMT_POLICY_NAMES=""
    if [ "$SMT_SINGLE" = true ]; then
        SMT_MODEL_NS=()
    elif [ ${#SMT_MODEL_NS[@]} -eq 0 ] && [ -n "${SMT_PREV_MODELS// /}" ]; then
        # Kept: granted here AND named by the last admin-setup on the
        # Namespace reader, a ClusterRole only a cluster admin writes. A
        # RoleBinding alone does not count: the owner of any namespace may
        # create one of this name there.
        smt_call smt_admin_recorded_models
        found=""
        for m in $SMT_PREV_MODELS; do
            printf '%s\n' "$SMT_CALL_OUT" | tr ',' '\n' | grep -qxF -- "$m" && found="$found $m"
        done
        read -r -a SMT_MODEL_NS <<< "$found"
        [ ${#SMT_MODEL_NS[@]} -eq 0 ] \
            || echo "Model namespaces: $(smt_model_list | sed 's/,/, /g') (kept: the last admin-setup named them, and this controller is granted in them; give --model-namespace to change them, --single-namespace to drop them all)."
    fi
    smt_cluster_guard
    [ ${#SMT_MODEL_NS[@]} -gt 0 ] || return 0
    for m in "${SMT_MODEL_NS[@]}"; do
        if ! smt_get namespace "$m"; then
            rest="$(printf '%s\n' "${SMT_MODEL_NS[@]}" | grep -vxF -- "$m" | sed 's/^/--model-namespace /' | paste -sd' ' -)"
            smt_exit 4 "model namespace $m does not exist. Run again without it: smt admin-setup -n $SMT_NS ${rest:---single-namespace}"
        fi
        # The namespace a managed namespace names for its policy: make grants
        # its read too (llmsm_managed_policy_label).
        found="$(printf '%s' "$KC_OUT" | jq -r --arg k "$SMT_POLICY_LABEL" '.metadata.labels[$k] // ""')"
        [ -z "$found" ] || SMT_POLICY_NAMES="$SMT_POLICY_NAMES $found"
        smt_call smt_other_controllers "$m"
        others="$SMT_CALL_OUT"
        [ -z "$others" ] || smt_exit 4 "namespace $m is managed by another scaling manager already (RoleBinding $others there): two controllers deciding for one model is an overlap the preflight FAILs on. Leave $m out, or remove it from that controller first"
    done
}

# smt_render_readers: the Namespace reader of the main file names the model
# namespaces too, with the own namespace of the controller, scaling-manager-policy and the
# policy namespaces they name: the set make narrows it to
# (llmsm_append_policy_namespace_reader_scope_patch). Nothing without a list.
smt_render_readers() {
    local names n
    [ ${#SMT_MODEL_NS[@]} -gt 0 ] || return 0
    # The list this admin-setup grants, recorded on the reader for the next
    # re-run without flags (smt_admin_recorded_models). Without a list the
    # file carries none, and kubectl apply removes the one applied before.
    R="scaling-manager-policy-namespace-reader-role-$SMT_NS" A="$SMT_MODELS_ANNOTATION" W="$(smt_model_list)" yq -i \
        '(select(.kind == "ClusterRole" and .metadata.name == strenv(R)) | .metadata.annotations[strenv(A)]) = strenv(W)' \
        "$SMT_DIR/$SMT_MAIN" || smt_exit 2 "cannot record the model namespaces on the Namespace reader of $SMT_MAIN"
    names="$(printf '%s\n' "$SMT_NS" "${SMT_MODEL_NS[@]}" scaling-manager-policy $SMT_POLICY_NAMES | awk 'NF && !seen[$0]++' | paste -sd, -)"
    R="scaling-manager-policy-namespace-reader-role-$SMT_NS" N="$names" yq -i \
        '(select(.kind == "ClusterRole" and .metadata.name == strenv(R)) | .rules[] | select(.resources[0] == "namespaces") | .resourceNames) = (strenv(N) | split(","))' \
        "$SMT_DIR/$SMT_MAIN" || smt_exit 2 "cannot name the model namespaces in the Namespace reader of $SMT_MAIN"
    n="$(R="scaling-manager-policy-namespace-reader-role-$SMT_NS" yq ea '[select(.kind == "ClusterRole" and .metadata.name == strenv(R)) | .rules[] | select(.resources[0] == "namespaces") | .resourceNames | join(",")] | .[]' "$SMT_DIR/$SMT_MAIN")"
    [ "$n" = "$names" ] || smt_exit 2 "$SMT_MAIN holds no Namespace reader scaling-manager-policy-namespace-reader-role-$SMT_NS to name the model namespaces in"
}

# smt_render_watch: with model namespaces, the controller manages them: the
# list on its LLMSM_WATCH_NAMESPACE, which its --watch-namespace reads, as make
# sets it. Without, the Deployment is the one of the release file, unchanged.
smt_render_watch() {
    local n
    [ ${#SMT_MODEL_NS[@]} -gt 0 ] || return 0
    D="$SMT_DEPLOY" E="$SMT_WATCH_ENV" W="$(smt_model_list)" yq -i \
        '(select(.kind == "Deployment" and .metadata.name == strenv(D)) | .spec.template.spec.containers[] | select(.name == "manager") | .env[] | select(.name == strenv(E)) | .value) = strenv(W)' \
        "$SMT_DIR/$SMT_MAIN" || smt_exit 2 "cannot set $SMT_WATCH_ENV in $SMT_MAIN"
    n="$(D="$SMT_DEPLOY" E="$SMT_WATCH_ENV" yq ea '[select(.kind == "Deployment" and .metadata.name == strenv(D)) | .spec.template.spec.containers[] | select(.name == "manager") | .env[] | select(.name == strenv(E)) | .value] | .[]' "$SMT_DIR/$SMT_MAIN")"
    [ "$n" = "$(smt_model_list)" ] || smt_exit 2 "$SMT_MAIN: the controller has no $SMT_WATCH_ENV to set the model namespaces on"
}

# smt_render_preflight_watch: the preflight Job checks the namespaces this
# install applies (SMT_PREFLIGHT_WATCH_ENV), not those of the live
# Deployment, when there is a list or the live one names others: after an
# admin shrank the list, the live Deployment still names a namespace the
# controller may no longer read, and the install that drops it must not be
# stopped by it. A single-namespace install over the same is left as it was.
smt_render_preflight_watch() {
    local want live=""
    want="$(smt_model_list)"
    [ -n "$want" ] || want="$SMT_NS"
    if [ "$SMT_DRY_RUN" != true ] && smt_get deployment "$SMT_DEPLOY" -n "$SMT_NS"; then
        live="$(printf '%s' "$KC_OUT" | jq -r --arg e "$SMT_WATCH_ENV" \
            '[.spec.template.spec.containers[]? | select(.name == "manager") | .env[]? | select(.name == $e) | .value // ""][0] // ""')"
    fi
    [ "$live" != '$(POD_NAMESPACE)' ] || live="$SMT_NS"
    if [ ${#SMT_MODEL_NS[@]} -eq 0 ] && { [ -z "$live" ] || [ "$live" = "$want" ]; }; then
        return 0
    fi
    J="$SMT_PREFLIGHT_JOB" E="$SMT_PREFLIGHT_WATCH_ENV" W="$want" yq -i \
        '(select(.kind == "Job" and .metadata.name == strenv(J)) | .spec.template.spec.containers[] | select(.name == "preflight") | .env) += [{"name": strenv(E), "value": strenv(W)}]' \
        "$SMT_DIR/$SMT_PREFLIGHT" || smt_exit 2 "cannot set $SMT_PREFLIGHT_WATCH_ENV in $SMT_PREFLIGHT"
}

# smt_preflight_reads_file <model namespace>: the reads of pods by the preflight
# and PodMonitors, copied from its Role in the controller namespace into
# the model namespace, bound to its ServiceAccount (docs/reference/preflight.md,
# "Other watched namespaces"). Written to SMT_TMP; echoes its path.
smt_preflight_reads_file() {
    local out="$SMT_TMP/preflight-reads-$1.yaml" n
    N="$SMT_NS" M="$1" yq ea '
        select((.kind == "Role" or .kind == "RoleBinding") and .metadata.name == "scaling-manager-preflight")
        | .metadata.name = .metadata.name + "-" + strenv(N)
        | .metadata.namespace = strenv(M)
        | (select(.kind == "Role") | .rules) |= map(select((.resources | contains(["pods"])) or (.resources | contains(["podmonitors"]))))
        | (select(.kind == "RoleBinding") | .roleRef.name) = .roleRef.name + "-" + strenv(N)' \
        "$SMT_DIR/$SMT_PREFLIGHT" > "$out" || smt_exit 2 "cannot copy the preflight reads for $1"
    n="$(yq ea -o json -I 0 'select(.kind == "Role")' "$out" | jq -r '[.rules[] | (.resources | join(",")) + ":" + (.verbs | join(","))] | join(" ")')"
    [ "$n" = "pods:list podmonitors:list" ] || smt_exit 2 "the preflight Role of $SMT_PREFLIGHT does not read exactly pods and PodMonitors ($n)"
    printf '%s\n' "$out"
}

# smt_revoke_model <namespace>: removes from a namespace taken out of the list
# what admin-setup applied there for this controller (the model-namespace
# file, the preflight reads) and its activation grant, each object by kind and
# name and only when its instance label names this controller. Two objects
# have one name per namespace and may serve another controller too: the
# PodMonitor stays while another controller manages the namespace, and the
# pod writer Role while another pod writer RoleBinding binds it, as make
# keeps it (llmsm_remove_pod_writers). With no other controller left, the
# namespace also leaves the weights labeller list.
smt_revoke_model() {
    local ns="$1" f kind group name res shared="" others pw
    for f in "$(smt_model_file "$ns")" "$(smt_preflight_reads_file "$ns")" "$(smt_activate_file "$ns")"; do
        while read -r kind group name; do
            [ -n "$name" ] || continue
            res="$(printf '%s' "$kind" | tr 'A-Z' 'a-z')"
            [ "$group" = - ] || res="$res.$group"
            if [ "$kind" = PodMonitor ] || { [ "$kind" = Role ] && [ "$name" = "$SMT_POD_WRITER_ROLE" ]; }; then
                shared="$shared $res/$name"
                continue
            fi
            smt_revoke_one "$ns" "$res" "$name"
        done < <(yq ea -o json -I 0 'select(.kind != null)' "$f" \
            | jq -r '.kind + " " + (if (.apiVersion | test("/")) then (.apiVersion | split("/") | .[0]) else "-" end) + " " + .metadata.name')
    done
    # The role writer grant, by name: no release file need be rendered.
    smt_dsetw_revoke "$ns"
    smt_call smt_other_controllers "$ns"
    others="$SMT_CALL_OUT"
    smt_kc get rolebindings -n "$ns" -o json
    pw="$(printf '%s' "$KC_OUT" | jq -r --arg p "$SMT_POD_WRITER_ROLE-" '[.items[] | .metadata.name | select(startswith($p))] | join(" ")')"
    for f in $shared; do
        res="${f%%/*}" name="${f#*/}"
        if [ "$name" = "$SMT_POD_WRITER_ROLE" ] && [ -n "$pw" ]; then
            echo "  $ns: kept Role/$name, which RoleBinding $pw still binds"
        elif [ "$name" != "$SMT_POD_WRITER_ROLE" ] && [ -n "$others" ]; then
            echo "  $ns: kept ${res%%.*}/$name, which the other controller there uses (RoleBinding $others)"
        else
            smt_revoke_one "$ns" "$res" "$name"
        fi
    done
    [ -z "$others" ] || return 0
    if bash "$HERE/weights.sh" labeller --remove -n "$ns" > "$SMT_TMP/labeller.out" 2>&1; then
        sed 's/^/  /' "$SMT_TMP/labeller.out"
    fi
    if [ "$SMT_PLATFORM" = openshift ]; then
        echo "  $ns keeps the label openshift.io/user-monitoring=true. If only this install set it: kubectl label namespace $ns openshift.io/user-monitoring-"
    fi
}

# smt_revoke_one <namespace> <resource> <name>: deletes the object when its
# instance label names this controller.
smt_revoke_one() {
    smt_get "$2" "$3" -n "$1" || return 0
    [ "$(printf '%s' "$KC_OUT" | jq -r --arg k "$SMT_INSTANCE_LABEL" '.metadata.labels[$k] // ""')" = "$SMT_NS" ] || return 0
    smt_kc delete "$2" "$3" -n "$1" --ignore-not-found
    echo "  $1: deleted $2/$3"
}

# smt_activate_file <namespace>: the activation grant for that namespace.
smt_activate_file() {
    local out="$SMT_TMP/activate-$1.yaml"
    sed -e "s/__NAMESPACE__/$SMT_NS/g" -e "s/__MODEL_NAMESPACE__/$1/g" "$SMT_SHIPPED/scaling-manager-activate.yaml" > "$out" \
        || smt_exit 2 "cannot render scaling-manager-activate.yaml for $1"
    printf '%s\n' "$out"
}

# --- Settings ----------------------------------------------------------------

# smt_settings_pairs: SMT_PAIRS, the settings keys the flags set, KEY=VALUE.
# One table: --prometheus-url prometheusURL, --prometheus-ca
# prometheusCACertPath, --quota limiter=quota and quotas,
# --utilization-share utilizationShare, --allow-http prometheusAllowHTTP=true,
# --set KEY=VALUE.
smt_settings_pairs() {
    local kv key known
    SMT_PAIRS=()
    [ -z "$SMT_PROM_URL" ] || SMT_PAIRS+=("prometheusURL=$SMT_PROM_URL")
    [ -z "$SMT_PROM_CA" ] || SMT_PAIRS+=("prometheusCACertPath=$SMT_PROM_CA_PATH")
    [ -z "$SMT_QUOTA" ] || SMT_PAIRS+=("limiter=quota" "quotas=$SMT_QUOTA")
    [ -z "$SMT_SHARE" ] || SMT_PAIRS+=("utilizationShare=$SMT_SHARE")
    [ "$SMT_ALLOW_HTTP" != true ] || SMT_PAIRS+=("prometheusAllowHTTP=true")
    known="$(yq '.data | keys | .[]' "$SMT_SHIPPED/scaling-manager-settings.yaml")"
    for kv in "${SMT_SETS[@]}"; do
        key="${kv%%=*}"
        [ "$key" != "$kv" ] || smt_usage "--set $kv: give KEY=VALUE"
        [ "$key" != DSET_ROLE_WRITER ] || smt_usage "--set DSET_ROLE_WRITER: give --dset-role-writer; it turns the role writer on in the controller, and needs its grant (smt admin-setup --dset-role-writer, or install --scope cluster --dset-role-writer)"
        printf '%s\n' "$known" | grep -qxF -- "$key" || smt_usage "--set $key: not a key of scaling-manager-settings ($(printf '%s' "$known" | tr '\n' ' '))"
        [ "$key" != passive ] || smt_usage "--set passive: the install is passive. After smt admin-finish, turn it off with:  $(smt_patch_cmd passive "${kv#*=}")"
        case "$key" in
            prometheusURL) smt_usage "--set prometheusURL: give --prometheus-url" ;;
            prometheusCACertPath) smt_usage "--set prometheusCACertPath: give --prometheus-ca <file>; it creates the Secret the path reads" ;;
            quotas) smt_usage "--set quotas: give --quota \"GPU=N\"" ;;
            prometheusAllowHTTP) smt_usage "--set prometheusAllowHTTP: give --allow-http; it also applies the controller without PROMETHEUS_TOKEN_PATH, which a plain HTTP Prometheus needs" ;;
            limiter) [ -z "$SMT_QUOTA" ] || smt_usage "--set limiter with --quota: --quota sets limiter to quota" ;;
            utilizationShare) [ -z "$SMT_SHARE" ] || smt_usage "--set utilizationShare with --utilization-share: give one of them" ;;
        esac
        smt_check_setting "$key" "${kv#*=}"
        SMT_PAIRS+=("$kv")
    done
}

# smt_check_setting <key> <value>: a value the controller accepts for that
# key (internal/config/settings.go), or exit 2. The controller rejects a bad
# value only when it reads the settings, and a rejected limiter costs the
# whole default entry; here the install stops before anything is written.
# Keys with free-form values (prometheusServerName) pass.
smt_check_setting() {
    local k="$1" v="$2" t
    case "$k" in
        utilizationShare)
            case "$v" in off|shadow|active) return 0 ;; esac
            smt_usage "--set utilizationShare=$v: off, shadow or active" ;;
        limiter)
            for t in $(printf '%s' "$v" | tr ',' ' '); do
                case "$t" in gpu-inventory|quota) ;; *) smt_usage "--set limiter=$v: gpu-inventory, quota, or both comma-separated" ;; esac
            done ;;
        quotaScope)
            case "$v" in namespace|cluster) return 0 ;; esac
            smt_usage "--set quotaScope=$v: namespace or cluster" ;;
        kueue|kueueBorrow|scaleToZero|prometheusInsecureSkipVerify)
            # strconv.ParseBool, which the controller reads them with.
            case "$v" in 1|t|T|TRUE|true|True|0|f|F|FALSE|false|False) return 0 ;; esac
            smt_usage "--set $k=$v: true or false" ;;
        logVerbosity)
            case "$v" in ""|*[!0-9]*) smt_usage "--set logVerbosity=$v: a number, 0 or more" ;; esac ;;
        kueueNamespace)
            smt_check_ns "--set kueueNamespace" "$v" ;;
        watchNamespaces)
            for t in $(printf '%s' "$v" | tr ',' ' '); do smt_check_ns "--set watchNamespaces" "$t"; done ;;
    esac
}

# smt_share_mode echoes the utilizationShare these flags set, or empty.
smt_share_mode() {
    local kv
    for kv in "${SMT_PAIRS[@]}"; do
        [ "${kv%%=*}" != utilizationShare ] || { printf '%s\n' "${kv#*=}"; return 0; }
    done
}

# smt_share_needs_limiter: settings created with utilizationShare shadow or
# active name a limiter, or exit 2. The optimizer divides a GPU budget; with
# none it has nothing to divide, and the share mode would read as set while
# nothing is computed.
smt_share_needs_limiter() {
    local mode kv
    mode="$(smt_share_mode)"
    case "$mode" in shadow|active) ;; *) return 0 ;; esac
    for kv in "${SMT_PAIRS[@]}"; do
        [ "${kv%%=*}" != limiter ] || [ -z "${kv#*=}" ] || return 0
    done
    smt_usage "utilizationShare $mode divides a GPU budget, and these flags set no limiter: add --quota \"GPU=N\" (the GPUs of that product the models may hold together; kubectl get nodes -L nvidia.com/gpu.product names the product), or --set limiter=gpu-inventory"
}

# smt_share_targets echoes the namespaces whose pods sharing patches, one a
# line: the model namespaces, else the namespace of the controller.
smt_share_targets() {
    if [ ${#SMT_MODEL_NS[@]} -gt 0 ]; then
        printf '%s\n' "${SMT_MODEL_NS[@]}"
    else
        printf '%s\n' "$SMT_NS"
    fi
}

# smt_share_grant: with utilizationShare active from the flags, the pod-patch
# grant (scaling-manager-activate.yaml) in each namespace sharing reaches,
# before the settings and the preflight, whose activation check FAILs without
# it. One already there is left as it is. A refused apply is exit 3, with the
# command the cluster admin runs: a namespace owner may grant it in their own
# namespace, not always in another.
smt_share_grant() {
    local m f rc
    [ "$(smt_share_mode)" = active ] || return 0
    smt_need scaling-manager-activate.yaml
    echo "Utilization share active: the pod-patch grant (scaling-manager-activate.yaml):"
    while read -r m; do
        if kc_capture get rolebinding "scaling-manager-pod-writer-$SMT_NS" -n "$m" -o name \
            && kc_capture get role scaling-manager-pod-writer -n "$m" -o name; then
            echo "  $m: in place"
            continue
        fi
        f="$(smt_activate_file "$m")"
        rc=0
        kc_capture apply -f "$f" || rc=$?
        if [ "$rc" = 0 ]; then
            printf '%s\n' "$KC_OUT" | sed '/^$/d; s/^/  /'
            continue
        fi
        printf '%s' "$KC_ERR" | grep -qF '(Forbidden)' || smt_kubectl_failed "apply the pod-patch grant in $m"
        echo "--utilization-share active: you may not grant the controller patch on pods in $m ($(kc_err_line "$KC_ERR")). Ask the cluster admin to run, from the release files (smt release-files --out <dir>):" >&2
        echo "  sed -e \"s/__NAMESPACE__/$SMT_NS/g\" -e \"s/__MODEL_NAMESPACE__/$m/g\" scaling-manager-activate.yaml | kubectl apply -f -" >&2
        echo "Then run this install again; or install with --utilization-share shadow, which needs no grant." >&2
        exit 3
    done < <(smt_share_targets)
}

# smt_settings_file: the settings file with the values of the flags, in SMT_TMP.
smt_settings_file() {
    local kv out="$SMT_TMP/settings.yaml"
    cp "$SMT_DIR/scaling-manager-settings.yaml" "$out"
    for kv in "${SMT_PAIRS[@]}"; do
        K="${kv%%=*}" V="${kv#*=}" yq -i '.data[strenv(K)] = strenv(V)' "$out" \
            || smt_exit 2 "cannot set ${kv%%=*} in the settings"
    done
    printf '%s\n' "$out"
}

# smt_patch_cmd <key> <value>: the kubectl patch that sets one settings key.
smt_patch_cmd() {
    local j
    j="$(jq -cn --arg k "$1" --arg v "$2" '{data: {($k): $v}}')"
    # Inside single quotes a quote is written as: quote, backslash, quote, quote.
    j="$(printf '%s' "$j" | sed "s/'/'\\\\''/g")"
    printf "kubectl patch configmap %s -n %s --type merge -p '%s'\n" "$SMT_SETTINGS" "$SMT_NS" "$j"
}

# smt_settings: creates the settings from the flags when there are none, and
# never writes existing ones: a flag that differs is reported with its patch.
smt_settings() {
    local kv live cur f
    smt_settings_pairs
    if smt_get configmap "$SMT_SETTINGS" -n "$SMT_NS"; then
        live="$KC_OUT"
        echo "Settings: $SMT_SETTINGS exists; left as it is."
        for kv in "${SMT_PAIRS[@]}"; do
            cur="$(printf '%s' "$live" | jq -r --arg k "${kv%%=*}" '.data[$k] // ""')"
            if [ "$cur" != "${kv#*=}" ]; then
                echo "  ${kv%%=*} is \"$cur\" there, not \"${kv#*=}\" as given. Not changed; to change it:"
                echo "    $(smt_patch_cmd "${kv%%=*}" "${kv#*=}")"
            fi
        done
        return 0
    fi
    f="$(smt_settings_file)"
    smt_kc create -f "$f"
    SMT_SETTINGS_CREATED=true
    echo "Settings: created $SMT_SETTINGS ($(printf '%s ' "${SMT_PAIRS[@]%%=*}")from the flags; passive)."
}

# smt_share_summary: after an install whose flags set utilizationShare, what
# is in force and the read that shows it. Settings this run did not create
# were left as they are; smt_settings printed the patch for each difference.
smt_share_summary() {
    local mode
    mode="$(smt_share_mode)"
    [ -n "$mode" ] || return 0
    if [ "$SMT_SETTINGS_CREATED" != true ]; then
        echo "Utilization share: $SMT_SETTINGS existed, so utilizationShare $mode was not written (the patch is above, when it differs)."
        return 0
    fi
    echo "Utilization share: $mode$([ -z "$SMT_QUOTA" ] || printf ', quota %s' "$SMT_QUOTA"). While a model of a group is passive, the group computes and publishes its shares and moves nothing (llmsm_utilization_share_passive_shadow is 1)."
    echo "  In force once the controller has read the settings, within a minute:"
    echo "    kubectl get configmap $SMT_STATUS_CM -n $SMT_NS -o jsonpath='{.data.limiter} {.data.utilizationShare}'"
}

# smt_settings_preconditions: with no settings, a Prometheus URL is needed
# before anything is created. Run before the first write.
#
# The URL the controller will read decides --allow-http (smt_check_scheme): the
# one in the live settings when they exist, since they are never written, else
# the one the flag gives. A flag URL of the other scheme than the live one is
# a switch the settings must make first: the refusal prints every patch it
# needs (smt_switch_patches).
smt_settings_preconditions() {
    local live allow want=false had=false
    if smt_get configmap "$SMT_SETTINGS" -n "$SMT_NS"; then
        live="$(printf '%s' "$KC_OUT" | jq -r '.data.prometheusURL // ""')"
        allow="$(printf '%s' "$KC_OUT" | jq -r '.data.prometheusAllowHTTP // ""')"
        smt_is_http "$live" && had=true
        [ -n "$SMT_PROM_URL" ] && smt_is_http "$SMT_PROM_URL" && want=true
        if [ -n "$live" ] && [ -n "$SMT_PROM_URL" ] && [ "$had" != "$want" ]; then
            smt_usage "prometheusURL in the live $SMT_SETTINGS is $live, and --prometheus-url $SMT_PROM_URL changes its scheme. install never writes existing settings: run these, then the same command again right away (until then a controller restart fails):
$(smt_switch_patches "$want" | sed 's/^/  /')"
        fi
        [ -z "$live" ] || smt_check_scheme "$live" "prometheusURL in the live $SMT_SETTINGS ($live)"
        if [ "$SMT_ALLOW_HTTP" = true ] && [ "$allow" != true ]; then
            smt_usage "prometheusAllowHTTP in the live $SMT_SETTINGS is \"$allow\", not \"true\", so the controller would refuse the http URL. install never writes existing settings: run this, then the same command again:
  $(smt_patch_cmd prometheusAllowHTTP true)"
        fi
        return 0
    fi
    if [ -z "$SMT_PROM_URL" ]; then
        if [ "$SMT_PLATFORM" = openshift ]; then
            SMT_PROM_URL="$SMT_THANOS_URL"
            smt_check_scheme "$SMT_PROM_URL" "the OpenShift default, $SMT_PROM_URL,"
            echo "No --prometheus-url: the OpenShift default, $SMT_PROM_URL."
        else
            smt_usage "no $SMT_SETTINGS in $SMT_NS yet, and no --prometheus-url: give the Prometheus base URL, https (http with --allow-http)"
        fi
    fi
    smt_settings_pairs
    smt_share_needs_limiter
}

# smt_switch_patches <to http: true|false>: the kubectl patches, one a line,
# that switch the live settings (KC_OUT) to SMT_PROM_URL. To http: allow it,
# and clear the TLS settings the controller refuses beside an http URL. To
# https: stop allowing http, and with --prometheus-ca point at its CA.
smt_switch_patches() {
    local to_http="$1" v
    smt_patch_cmd prometheusURL "$SMT_PROM_URL"
    smt_patch_cmd prometheusAllowHTTP "$to_http"
    if [ "$to_http" = true ]; then
        v="$(printf '%s' "$KC_OUT" | jq -r '.data.prometheusInsecureSkipVerify // ""' | tr 'A-Z' 'a-z')"
        case "$v" in true|t|1) smt_patch_cmd prometheusInsecureSkipVerify false ;; esac
        for v in prometheusCACertPath prometheusServerName; do
            [ -z "$(printf '%s' "$KC_OUT" | jq -r --arg k "$v" '.data[$k] // ""')" ] || smt_patch_cmd "$v" ""
        done
    elif [ -n "$SMT_PROM_CA" ] && [ "$(printf '%s' "$KC_OUT" | jq -r '.data.prometheusCACertPath // ""')" != "$SMT_PROM_CA_PATH" ]; then
        smt_patch_cmd prometheusCACertPath "$SMT_PROM_CA_PATH"
    fi
}

# smt_prometheus_ca: with --prometheus-ca, creates or updates Secret
# scaling-manager-prometheus-ca. SMT_CA_CHANGED=true when an existing one
# changed (the controller reads it at start).
smt_prometheus_ca() {
    local want cur
    SMT_CA_CHANGED=false
    [ -n "$SMT_PROM_CA" ] || return 0
    want="$(base64 < "$SMT_PROM_CA" | tr -d '\n')"
    if smt_get secret "$SMT_PROM_CA_SECRET" -n "$SMT_NS"; then
        cur="$(printf '%s' "$KC_OUT" | jq -r '.data["ca.crt"] // ""')"
        if [ "$cur" = "$want" ]; then
            echo "Prometheus CA: Secret $SMT_PROM_CA_SECRET unchanged."
            return 0
        fi
        SMT_CA_CHANGED=true
    fi
    smt_prometheus_ca_yaml > "$SMT_TMP/prometheus-ca.yaml"
    smt_kc apply --server-side --force-conflicts --field-manager=scaling-manager-tools -f "$SMT_TMP/prometheus-ca.yaml"
    echo "Prometheus CA: Secret $SMT_PROM_CA_SECRET $([ "$SMT_CA_CHANGED" = true ] && echo updated || echo written)."
}

smt_prometheus_ca_yaml() {
    jq -n --arg ns "$SMT_NS" --arg n "$SMT_PROM_CA_SECRET" --rawfile ca "$SMT_PROM_CA" \
        '{apiVersion: "v1", kind: "Secret", type: "Opaque",
          metadata: {name: $n, namespace: $ns, labels: {"app.kubernetes.io/part-of": "scaling-manager"}},
          data: {"ca.crt": ($ca | @base64)}}' | yq -P
}

# --- The preflight -----------------------------------------------------------

# smt_preflight <label>: delete and create the preflight Job, wait for it,
# print its log. Sets SMT_PF_FAIL, SMT_PF_WARN, SMT_PF_UNKNOWN (check names)
# and keeps the log in SMT_TMP/preflight-<label>.log. Exit 5 on a timeout.
smt_preflight() {
    local label="$1" end="" i=0 t="${SMT_PREFLIGHT_TIMEOUT:-600}" log="$SMT_TMP/preflight-$1.log"
    echo "Preflight ($label):"
    smt_kc delete job "$SMT_PREFLIGHT_JOB" -n "$SMT_NS" --ignore-not-found --wait=true
    smt_kc apply -f "$SMT_DIR/$SMT_PREFLIGHT" -l "$SMT_PART_LABEL=controller"
    while :; do
        smt_get job "$SMT_PREFLIGHT_JOB" -n "$SMT_NS" || smt_exit 7 "the preflight Job disappeared while it ran"
        end="$(printf '%s' "$KC_OUT" | jq -r '[.status.conditions[]? | select(.status == "True") | .type | select(. == "Complete" or . == "Failed")] | first // ""')"
        [ -n "$end" ] && break
        [ "$i" -lt "$t" ] || smt_exit 5 "the preflight Job did not finish within ${t}s: kubectl logs -n $SMT_NS job/$SMT_PREFLIGHT_JOB"
        sleep "${SMT_POLL:-3}"
        i=$((i + $(smt_poll_step)))
    done
    smt_kc logs -n "$SMT_NS" "job/$SMT_PREFLIGHT_JOB"
    printf '%s\n' "$KC_OUT" > "$log"
    sed 's/^/  /' "$log"
    SMT_PF_FAIL="$(awk '$1 == "FAIL" {print $2}' "$log" | sort -u | tr '\n' ' ')"
    SMT_PF_WARN="$(awk '$1 == "WARN" {print $2}' "$log" | sort -u | tr '\n' ' ')"
    SMT_PF_UNKNOWN="$(awk '$1 == "UNKNOWN" {print $2}' "$log" | sort -u | tr '\n' ' ')"
    # The checks its own account was refused: the cluster reads admin-finish
    # removes with ClusterRoleBinding SMT_PREFLIGHT_CRB.
    SMT_PF_FORBIDDEN="$(awk '$1 == "UNKNOWN" && /forbidden/ {print $2}' "$log" | sort -u | tr '\n' ' ')"
    if [ "$end" = Failed ] && [ -z "$SMT_PF_FAIL" ]; then
        smt_exit 1 "the preflight Job failed without a FAIL line: kubectl logs -n $SMT_NS job/$SMT_PREFLIGHT_JOB"
    fi
    SMT_PF_LAST="$log"
}

# smt_unknown_other echoes the UNKNOWN checks the controller does not answer.
smt_unknown_other() {
    local c out="" excused
    excused="$(smt_unknown_excused)"
    for c in $SMT_PF_UNKNOWN; do
        case " $SMT_NEEDS_CONTROLLER " in *" $c "*) continue ;; esac
        case " $excused " in *" $c "*) continue ;; esac
        out="$out $c"
    done
    printf '%s\n' "${out# }"
}

# smt_unknown_excused echoes the UNKNOWN checks refused to the preflight
# account once admin-finish removed its cluster reads: expected after it, so
# --strict does not stop on them. Only while that ClusterRoleBinding is not
# there; a namespace owner may not read it, which counts as not there, since
# its reads are what the refusals show.
smt_unknown_excused() {
    [ -n "${SMT_PF_FORBIDDEN:-}" ] || return 0
    kc_capture get clusterrolebinding "$SMT_PREFLIGHT_CRB" -o name && return 0
    printf '%s\n' "$SMT_PF_FORBIDDEN"
}

# smt_preflight_gate <what stops>: exit 1 on a FAIL, 6 on --strict and an
# UNKNOWN the install cannot wait out.
smt_preflight_gate() {
    local other
    if [ -n "$SMT_PF_FAIL" ]; then
        smt_exit 1 "the preflight has a FAIL (${SMT_PF_FAIL% }): $1. Each FAIL above prints its fix; then run this again"
    fi
    other="$(smt_unknown_other)"
    if [ "$SMT_STRICT" = true ] && [ -n "$other" ]; then
        smt_exit 6 "--strict: the preflight could not check $other: $1"
    fi
}

# smt_preflight_first: the preflight before the scaler TLS Secrets and the
# controller. When it stops the install and this run created the settings,
# it removes them, so the next run creates them again from its flags: a wrong
# --prometheus-url must not stick in settings no controller has read yet.
smt_preflight_first() {
    local why="stopped before the scaler TLS Secrets and the controller; nothing more was created"
    smt_preflight before-install
    if [ "$SMT_SETTINGS_CREATED" = true ] && { [ -n "$SMT_PF_FAIL" ] || { [ "$SMT_STRICT" = true ] && [ -n "$(smt_unknown_other)" ]; }; }; then
        smt_kc delete configmap "$SMT_SETTINGS" -n "$SMT_NS" --ignore-not-found
        why="stopped before the scaler TLS Secrets and the controller, and removed $SMT_SETTINGS, which this run created: the next run creates it from its flags"
    fi
    smt_preflight_gate "$why"
}

# smt_summary_lines: the WARN and UNKNOWN lines of the last preflight, again.
smt_summary_lines() {
    [ -n "${SMT_PF_LAST:-}" ] || return 0
    local other
    if [ -n "$SMT_PF_WARN" ]; then
        echo "Warnings (the install went on):"
        grep -E '^WARN ' "$SMT_PF_LAST" | sed 's/^/  /'
    fi
    other="$(smt_unknown_other)"
    if [ -n "$SMT_PF_UNKNOWN" ]; then
        echo "Not checked (UNKNOWN is not a pass):"
        grep -E '^UNKNOWN ' "$SMT_PF_LAST" | sed 's/^/  /'
        case " $SMT_PF_UNKNOWN " in
            *" scaler.tls "*) echo "  scaler.tls stays UNKNOWN: the scaler NetworkPolicy admits KEDA only, so the preflight Job cannot dial it. scalerTLS in $SMT_STATUS_CM shows mutual TLS instead." ;;
        esac
    fi
}

# --- install steps -----------------------------------------------------------

# smt_check_stamp: the admin stamp exists and names these admin objects, or
# exit 3 with the admin-setup line.
#
# The model namespaces come from the stamp: only the cluster admin grants the
# controller its Role in another namespace. --model-namespace, when given, must
# name the same list, or exit 3 with the admin-setup line for the list given.
smt_check_stamp() {
    local have want keda models flags dw
    want="$(smt_admin_hash)"
    flags="$(smt_model_list)"
    if ! smt_get configmap "$SMT_STAMP" -n "$SMT_NS"; then
        echo "The cluster admin has not run admin-setup for $SMT_NS (no ConfigMap $SMT_STAMP). Ask them to run, with this tools image:" >&2
        echo "  $(smt_admin_line "" "$flags" "$SMT_DSETW")" >&2
        exit 3
    fi
    have="$(printf '%s' "$KC_OUT" | jq -r '.data.adminObjectsSha256 // ""')"
    keda="$(printf '%s' "$KC_OUT" | jq -r '.data.kedaNamespace // ""')"
    models="$(printf '%s' "$KC_OUT" | jq -r '.data.modelNamespaces // ""')"
    dw="$(printf '%s' "$KC_OUT" | jq -r '.data.dsetRoleWriter // "false"')"
    if [ "$have" != "$want" ]; then
        echo "The admin objects in $SMT_NS are from another release ($(printf '%s' "$KC_OUT" | jq -r '.data.release // "unknown"')): this release changes them, and only a cluster admin may apply them. Ask them to run, with this tools image ($(smt_version)):" >&2
        echo "  $(smt_admin_line "$keda" "${flags:-$models}" "$([ "$dw" = true ] || [ "$SMT_DSETW" = true ] && echo true)")" >&2
        exit 3
    fi
    if [ -n "$flags" ] && [ "$flags" != "$models" ]; then
        echo "The cluster admin set up $SMT_NS to manage ${models:-only $SMT_NS} (ConfigMap $SMT_STAMP), not $flags as --model-namespace says. Only a cluster admin may change which namespaces the controller reaches. Ask them to run, with this tools image:" >&2
        echo "  $(smt_admin_line "$keda" "$flags" "$([ "$dw" = true ] || [ "$SMT_DSETW" = true ] && echo true)")" >&2
        exit 3
    fi
    if [ -z "$flags" ] && [ -n "$models" ]; then
        mapfile -t SMT_MODEL_NS < <(printf '%s\n' "$models" | tr ',' '\n' | grep -v '^$')
    fi
    SMT_STAMP_KEDA="$keda"
    smt_install_dsetw "$dw" "$keda" "$models"
}

# smt_check_adoptable: a live controller Deployment must come from the release
# files (their part label); an install made with make is not adopted yet.
smt_check_adoptable() {
    smt_get deployment "$SMT_DEPLOY" -n "$SMT_NS" || { SMT_HAD_CONTROLLER=false; return 0; }
    SMT_HAD_CONTROLLER=true
    printf '%s' "$KC_OUT" | jq -e --arg l "$SMT_PART_LABEL" '.metadata.labels[$l] // empty' >/dev/null && return 0
    smt_exit 4 "Deployment $SMT_DEPLOY in $SMT_NS was not installed from the release files (no $SMT_PART_LABEL label): an install made with make deploy-scaling-manager. Upgrade it with make, or move it to the release files first: $SMT_MIGRATION_DOC"
}

# smt_scaler_secrets: the scaler TLS Secrets, created when absent, never
# applied (that would empty Secrets the controller filled).
smt_scaler_secrets() {
    local n name
    n="$(yq ea '[select(.kind == "Secret")] | length' "$SMT_DIR/scaling-manager-scaler-tls.yaml")"
    [ "$n" -gt 0 ] || smt_usage "scaling-manager-scaler-tls.yaml holds no Secret"
    for name in $(yq e -N 'select(.kind == "Secret") | .metadata.name' "$SMT_DIR/scaling-manager-scaler-tls.yaml"); do
        if smt_get secret "$name" -n "$SMT_NS"; then
            echo "  secret/$name exists; kept"
            continue
        fi
        N="$name" yq ea 'select(.kind == "Secret" and .metadata.name == strenv(N))' "$SMT_DIR/scaling-manager-scaler-tls.yaml" > "$SMT_TMP/secret-$name.yaml"
        smt_kc create -f "$SMT_TMP/secret-$name.yaml"
        echo "  secret/$name created, empty: the controller fills it"
    done
}

# smt_controller: the part=controller objects, then the rollout.
smt_controller() {
    echo "Controller:"
    smt_apply "$SMT_DIR/$SMT_MAIN" controller
    if [ "${SMT_CA_CHANGED:-false}" = true ] && [ "${SMT_HAD_CONTROLLER:-false}" = true ]; then
        echo "  the Prometheus CA changed, and the controller reads it at start: restarting it"
        smt_kc rollout restart "deployment/$SMT_DEPLOY" -n "$SMT_NS"
    fi
    smt_rollout
}

smt_rollout() {
    local rc=0
    kc_capture rollout status "deployment/$SMT_DEPLOY" -n "$SMT_NS" --timeout="${SMT_ROLLOUT_TIMEOUT:-300s}" || rc=$?
    if [ "$rc" != 0 ]; then
        case "$KC_ERR" in
            *"timed out"*|*"exceeded its progress deadline"*) smt_exit 5 "the controller did not become ready within ${SMT_ROLLOUT_TIMEOUT:-300s}: kubectl describe deployment/$SMT_DEPLOY -n $SMT_NS" ;;
        esac
        smt_kubectl_failed "rollout status deployment/$SMT_DEPLOY -n $SMT_NS"
    fi
    echo "  deployment/$SMT_DEPLOY ready"
}

# --- admin-finish steps ------------------------------------------------------

# smt_wait_client_cert: until the controller filled the KEDA client certificate.
smt_wait_client_cert() {
    local i=0 t="${SMT_CERT_TIMEOUT:-180}" crt
    smt_get deployment "$SMT_DEPLOY" -n "$SMT_NS" \
        || smt_exit 4 "no controller in $SMT_NS (Deployment $SMT_DEPLOY): the namespace owner has not run smt install -n $SMT_NS yet"
    while :; do
        if smt_get secret "$LLMSM_SCALER_CLIENT_SECRET" -n "$SMT_NS"; then
            crt="$(printf '%s' "$KC_OUT" | jq -r '.data["tls.crt"] // ""')"
            [ -n "$crt" ] && return 0
        fi
        [ "$i" -lt "$t" ] || smt_exit 5 "the controller has not filled Secret $LLMSM_SCALER_CLIENT_SECRET in $SMT_NS within ${t}s. Has the namespace owner run smt install? kubectl logs -n $SMT_NS deployment/$SMT_DEPLOY"
        sleep "${SMT_POLL:-3}"
        i=$((i + $(smt_poll_step)))
    done
}

# smt_finish_keda_ns: sets SMT_KEDA_NS for admin-finish. The flag; else the
# stamp admin-setup wrote, which must agree with the live NetworkPolicy; else
# the APIService detection. None, or a disagreement: exit 4.
smt_finish_keda_ns() {
    local stamp np rc=0 found
    [ -n "$SMT_KEDA_NS" ] && { SMT_KEDA_FROM="--keda-namespace"; return 0; }
    if [ "$SMT_SCOPE" = namespace ] && smt_get configmap "$SMT_STAMP" -n "$SMT_NS"; then
        stamp="$(printf '%s' "$KC_OUT" | jq -r '.data.kedaNamespace // ""')"
        smt_call llmsm_scaler_np_keda_ns "$SMT_NS" || rc=$?
        np="$SMT_CALL_OUT"
        [ "$rc" != 2 ] || smt_exit 7 "cannot read NetworkPolicy scaling-manager-external-scaler in $SMT_NS: $(kc_err_line "$KC_ERR")"
        if [ -n "$stamp" ] && [ "$rc" = 0 ] && [ "$np" = "$stamp" ]; then
            SMT_KEDA_NS="$stamp"
            SMT_KEDA_FROM="ConfigMap $SMT_STAMP, which the scaler NetworkPolicy agrees with"
            return 0
        fi
        smt_exit 4 "ConfigMap $SMT_STAMP says KEDA runs in '${stamp}', and the scaler NetworkPolicy in $SMT_NS admits '${np:-nothing it can name}'. Run smt admin-setup -n $SMT_NS --keda-namespace <namespace> again, or give --keda-namespace"
    fi
    rc=0
    smt_call llmsm_keda_apiservice_ns || rc=$?
    found="$SMT_CALL_OUT"
    case "$rc" in
        0) SMT_KEDA_NS="$found"; SMT_KEDA_FROM="the namespace serving the external-metrics APIService"; return 0 ;;
        2) smt_exit 7 "cannot read APIService v1beta1.external.metrics.k8s.io: $(kc_err_line "$KC_ERR")" ;;
    esac
    smt_exit 4 "cannot find the namespace KEDA runs in. Give --keda-namespace <namespace>"
}

# smt_library_exits: in a subshell that runs a deploy/lib function, make its
# refusals (log_error, an exit 1 there) exit with the codes of this tool: 7 for
# a write kubectl could not make, 4 for anything else (a refusal a person
# must resolve: a foreign or unreadable object, KEDA not ready). Exit 1 here
# means a preflight FAIL, which a refusal is not.
smt_library_exits() {
    log_error() {
        echo -e "${RED}[ERROR]${NC} $1" >&2
        case "$1" in
            "Could not write"*) exit 7 ;;
            *) exit 4 ;;
        esac
    }
}

# smt_keda_side: the KEDA side of mutual TLS, by llmsm_scaler_keda_auth_apply
# (deploy/lib/scaler_tls.sh), the function make setup-prereqs runs. It exits
# on a refusal (log_error), so it runs in a subshell that reports back.
# Sets SMT_KEDA_WROTE.
smt_keda_side() {
    local state="$SMT_TMP/keda-side" rc=0
    : > "$state"
    (
        smt_library_exits
        export LLMSM_SCALER_TLS_WANTED=true LLMSM_NS="$SMT_NS" ENVIRONMENT="$SMT_PLATFORM"
        export KEDA_NAMESPACE="$SMT_KEDA_NS" KEDA_NAMESPACE_EXPLICIT=true LLMSM_TOOLS_MODE=true LLMSM_SCOPE="$SMT_SCOPE"
        LLMSM_SCALER_KEDA_READY=false
        LLMSM_SCALER_KEDA_WROTE=false
        llmsm_scaler_keda_auth_apply
        printf '%s %s %s\n' "$LLMSM_SCALER_KEDA_READY" "$LLMSM_SCALER_KEDA_WROTE" "${LLMSM_SCALER_KEDA_OBJ_NS:-}" > "$state"
    ) || rc=$?
    case "$rc" in
        0) ;;
        4) smt_exit 4 "KEDA's side of mutual TLS was refused; the error above says why and what to do" ;;
        *) smt_exit 7 "writing KEDA's side failed (the line above says which call)" ;;
    esac
    read -r SMT_KEDA_READY SMT_KEDA_WROTE SMT_KEDA_OBJ_NS < "$state"
    [ "$SMT_KEDA_READY" = true ] || smt_exit 4 "KEDA's side of mutual TLS was not written (the warning above says why)"
}

# smt_wait_mtls: until the controller reports scalerTLS: mutual TLS.
smt_wait_mtls() {
    local i=0 t="${SMT_MTLS_TIMEOUT:-180}" v=""
    while :; do
        if smt_get configmap "$SMT_STATUS_CM" -n "$SMT_NS"; then
            v="$(printf '%s' "$KC_OUT" | jq -r '.data.scalerTLS // ""')"
            case "$v" in "mutual TLS"*) echo "  scalerTLS: $v"; return 0 ;; esac
        fi
        [ "$i" -lt "$t" ] || smt_exit 5 "scalerTLS is '${v}', not mutual TLS, after ${t}s: kubectl get configmap $SMT_STATUS_CM -n $SMT_NS -o jsonpath='{.data.scalerTLSAction}'"
        sleep "${SMT_POLL:-3}"
        i=$((i + $(smt_poll_step)))
    done
}

# smt_keda_connection: the stale-connection check make verify-deployment runs
# (llmsm_scaler_keda_conn_state). KEDA is shared: restarted only with
# --restart-keda.
smt_keda_connection() {
    (
        smt_library_exits
        export LLMSM_NS="$SMT_NS" KEDA_NAMESPACE="$SMT_KEDA_NS" KEDA_NAMESPACE_EXPLICIT=true LLMSM_TOOLS_MODE=true LLMSM_SCOPE="$SMT_SCOPE"
        LLMSM_SCALER_KEDA_RUN_NS=""
        llmsm_scaler_keda_conn_state
        case "$LLMSM_SCALER_KEDA_CONN" in
            none) echo "  KEDA holds no old connection to the scaler" ;;
            stale)
                llmsm_scaler_keda_conn_report || true
                if [ "$SMT_RESTART_KEDA" = true ]; then
                    kubectl rollout restart deployment/keda-operator -n "$SMT_KEDA_NS" >/dev/null \
                        || { echo "smt: restarting KEDA's operator in $SMT_KEDA_NS failed" >&2; exit 7; }
                    echo "  --restart-keda: restarted KEDA's operator in $SMT_KEDA_NS"
                else
                    echo "  KEDA's operator is shared, so it was not restarted. Restart it, or run this again with --restart-keda: $(llmsm_scaler_keda_restart_cmd)"
                fi ;;
            *) llmsm_scaler_keda_conn_report || true ;;
        esac
    ) || exit $?
}

# smt_finish <what>: the admin-finish steps, for both scopes.
smt_finish() {
    echo "KEDA's client certificate:"
    smt_wait_client_cert
    echo "  filled by the controller"
    [ "$SMT_SCOPE" = cluster ] || smt_finish_keda_ns
    echo "KEDA's side of mutual TLS (KEDA in $SMT_KEDA_NS, from ${SMT_KEDA_FROM:-the flag}):"
    smt_keda_side
    if [ "$SMT_KEDA_WROTE" = true ]; then
        echo "  written; restarting the controller, which reads it at start"
        smt_kc rollout restart "deployment/$SMT_DEPLOY" -n "$SMT_NS"
        smt_rollout
    else
        echo "  already in place; nothing restarted"
    fi
    smt_wait_mtls
    # The check reads the controller log by its label, which the preflight
    # Job pods carry too; the Job is created again below.
    smt_kc delete job "$SMT_PREFLIGHT_JOB" -n "$SMT_NS" --ignore-not-found --cascade=foreground --wait=true
    smt_keda_connection
    smt_preflight final
    if [ -n "$SMT_PF_FAIL" ] || { [ "$SMT_STRICT" = true ] && [ -n "$(smt_unknown_other)" ]; }; then
        echo "Kept ClusterRoleBinding $SMT_PREFLIGHT_CRB, so the preflight can run again. After the fix:" >&2
        echo "  kubectl delete clusterrolebinding $SMT_PREFLIGHT_CRB --ignore-not-found" >&2
        smt_preflight_gate "kept ClusterRoleBinding $SMT_PREFLIGHT_CRB"
    fi
    smt_kc delete clusterrolebinding "$SMT_PREFLIGHT_CRB" --ignore-not-found
    echo "Removed ClusterRoleBinding $SMT_PREFLIGHT_CRB: the preflight's cluster reads are needed only while it runs."
}

# --- The weights labeller ------------------------------------------------------
#
# The labeller (deploy/weights.sh labeller; docs/proposals/weights-node-gate.md)
# patches nodes, so it is a cluster-wide piece: the cluster-wide install
# applies and updates it, watching every --model-namespace and every namespace
# it already watches -- but only where it has work: a model namespace with a
# weights DaemonSet (weights.sh apply), or a labeller already running. Else it
# says how to add it later. A namespaced admin-setup only checks it is there,
# adds its own namespace to its watch list with namespaced rights, and
# otherwise prints the command.
SMT_WL_NS=scaling-manager-weights-labeller
SMT_WL_DS_SELECTOR=app.kubernetes.io/component=node-local-weights,app.kubernetes.io/managed-by=scaling-manager-weights

# smt_wl_image echoes the image the labeller runs: --weights-labeller-image,
# else the tools image of this release. Returns 1 when there is none to name:
# a development image, or a mirror (--image-repo) whose tools path this
# cannot know.
smt_wl_image() {
    local v
    if [ -n "$SMT_WL_IMAGE" ]; then
        printf '%s\n' "$SMT_WL_IMAGE"
        return 0
    fi
    [ -z "$SMT_IMAGE_REPO" ] || return 1
    v="$(smt_version)"
    case "$v" in ""|unreleased) return 1 ;; esac
    printf 'ghcr.io/llm-d-incubation/llm-d-scaling-manager/tools:%s\n' "$v"
}

# smt_wl_list echoes the --model-namespace list, comma-separated.
smt_wl_list() {
    smt_model_list
}

# smt_weights_labeller: the cluster-wide install applies or updates the
# labeller. Skipped, and said so, with --no-weights-labeller, with no
# --model-namespace, with no image to run, and when no model namespace keeps
# weights on the node and no labeller runs yet. A read that fails skips it
# too, naming what could not be read (Forbidden is not absent); the install
# goes on.
smt_weights_labeller() {
    local img m n have="" running=false
    if [ "$SMT_WL_OFF" = true ]; then
        echo "Weights labeller: not applied (--no-weights-labeller)."
        return 0
    fi
    if [ ${#SMT_MODEL_NS[@]} -eq 0 ]; then
        echo "Weights labeller: not applied (no --model-namespace to watch)."
        return 0
    fi
    if ! img="$(smt_wl_image)"; then
        echo "Weights labeller: not applied: no tools image to run it with (a development image, or --image-repo). Give --weights-labeller-image <tools image>."
        return 0
    fi
    if ! kc_capture get deployment weights-labeller -n "$SMT_WL_NS" --ignore-not-found -o json; then
        echo "Weights labeller: not applied: could not read Deployment weights-labeller in $SMT_WL_NS ($(printf '%s' "$KC_ERR" | head -n 1))."
        return 0
    fi
    [ -z "$KC_OUT" ] || running=true
    if [ "$running" = false ]; then
        for m in "${SMT_MODEL_NS[@]}"; do
            if ! kc_capture get daemonset -n "$m" -l "$SMT_WL_DS_SELECTOR" -o json; then
                echo "Weights labeller: not applied: could not read the DaemonSets in $m ($(printf '%s' "$KC_ERR" | head -n 1)), so whether it keeps weights on the node is not known."
                return 0
            fi
            n="$(printf '%s' "$KC_OUT" | jq '.items | length' 2>/dev/null)" || n=0
            [ "${n:-0}" -eq 0 ] || have="${have:+$have, }$m"
        done
        if [ -z "$have" ]; then
            echo "Weights labeller: not applied: no model namespace keeps weights on the node (no weights.sh apply DaemonSet in $(smt_wl_list | sed 's/,/, /g'))."
            echo "  After weights apply, run this install again, or: smt weights labeller -n <model namespace> --image $img --merge"
            return 0
        fi
    fi
    echo "Weights labeller (labels the GPU nodes whose weights copy verifies; it patches nodes$([ "$running" = true ] && echo "; updating the one running" || echo "; weights on the node in $have")):"
    bash "$HERE/weights.sh" labeller -n "$(smt_wl_list)" --image "$img" --merge \
        || smt_exit 7 "the weights labeller was not applied (weights.sh labeller exited non-zero, above)"
}

# smt_weights_labeller_dry: the labeller as a file, for --dry-run. No cluster
# is read, so it watches the --model-namespace list only.
smt_weights_labeller_dry() {
    local img
    [ "$SMT_WL_OFF" = false ] && [ ${#SMT_MODEL_NS[@]} -gt 0 ] || return 0
    img="$(smt_wl_image)" || { echo "  (weights labeller not rendered: give --weights-labeller-image)"; return 0; }
    bash "$HERE/weights.sh" labeller -n "$(smt_wl_list)" --image "$img" --dry-run > "$SMT_OUT/60-weights-labeller.yaml" \
        || smt_exit 2 "the weights labeller did not render"
    echo "  $SMT_OUT/60-weights-labeller.yaml (only where a model namespace keeps weights on the node; it watches exactly --model-namespace and, applied, replaces a running labeller's list: keep the full list in git, and do not use labeller --add)"
}

# smt_admin_setup_labeller: admin-setup checks the labeller is there and adds
# this namespace to its watch list. Never fails admin-setup: the labeller is
# an addition to the weights preparation, not to the install.
#
# With model namespaces, it adds those: the model servers run there.
smt_admin_setup_labeller() {
    local rc ns
    echo "Weights labeller:"
    for ns in "${SMT_MODEL_NS[@]:-$SMT_NS}"; do
        rc=0
        bash "$HERE/weights.sh" labeller --add -n "$ns" > "$SMT_TMP/labeller.out" 2>&1 || rc=$?
        sed 's/^/  /' "$SMT_TMP/labeller.out"
        case "$rc" in
            0|3|4) ;;
            *) echo "  (weights.sh labeller --add -n $ns exited $rc; admin-setup goes on)" ;;
        esac
    done
}

# --- dry-run -----------------------------------------------------------------

# smt_dry_write <file> [part] <source>: one step file in SMT_OUT.
smt_dry_write() {
    local name="$1" part="$2" src="$3"
    if [ -n "$part" ]; then
        smt_select "$part" "$src" > "$SMT_OUT/$name"
    else
        yq ea 'select(.kind != null)' "$src" > "$SMT_OUT/$name"
    fi
    echo "  $SMT_OUT/$name"
}

smt_dry_admin() {
    local m f
    smt_dry_write 10-admin.yaml admin "$SMT_DIR/$SMT_MAIN"
    smt_dry_write 11-preflight-admin.yaml admin "$SMT_DIR/$SMT_PREFLIGHT"
    if [ "$SMT_SCOPE" = cluster ]; then
        for m in "${SMT_MODEL_NS[@]}"; do
            f="$(smt_model_file "$m")"
            smt_ms_port_apply "$f" "$SMT_MS_PORT"
            smt_dry_write "12-model-namespace-$m.yaml" "" "$f"
        done
    elif [ ${#SMT_MODEL_NS[@]} -gt 0 ]; then
        for m in "${SMT_MODEL_NS[@]}"; do
            f="$(smt_model_file "$m")"
            smt_ms_port_apply "$f" "$SMT_MS_PORT"
            smt_dry_write "12-model-namespace-$m.yaml" "" "$f"
            smt_dry_write "13-preflight-reads-$m.yaml" "" "$(smt_preflight_reads_file "$m")"
        done
    else
        smt_ms_port_apply "$SMT_DIR/scaling-manager-model-namespace.yaml" "$SMT_MS_PORT"
        smt_dry_write 12-model-namespace.yaml "" "$SMT_DIR/scaling-manager-model-namespace.yaml"
    fi
    [ "$SMT_NO_EPP_MON" = true ] || echo "  (an EPP ServiceMonitor is not rendered: whether one is needed, and for which port, depends on the EPP Services --dry-run does not read)"
    [ "${SMT_DSETW_WANT:-false}" = true ] || return 0
    if [ "$SMT_SCOPE" = cluster ]; then
        smt_dry_write 14-dset-role-writer.yaml "" "$(smt_dsetw_file "$SMT_NS")"
        return 0
    fi
    while read -r m; do
        if [ "$m" = "$SMT_NS" ]; then
            smt_dry_write 14-dset-role-writer.yaml "" "$(smt_dsetw_file "$m")"
        else
            smt_dry_write "14-dset-role-writer-$m.yaml" "" "$(smt_dsetw_file "$m")"
        fi
    done < <(smt_dsetw_targets)
}

smt_dry_install() {
    smt_settings_pairs
    [ -n "$SMT_PROM_URL" ] || [ "$SMT_PLATFORM" = openshift ] \
        || smt_usage "--dry-run renders a fresh install: give --prometheus-url"
    [ -n "$SMT_PROM_URL" ] || { SMT_PROM_URL="$SMT_THANOS_URL"; smt_check_scheme "$SMT_PROM_URL" "the OpenShift default, $SMT_PROM_URL,"; smt_settings_pairs; }
    smt_share_needs_limiter
    if [ "$(smt_share_mode)" = active ]; then
        smt_need scaling-manager-activate.yaml
        local m
        while read -r m; do
            smt_dry_write "15-activate-$m.yaml" "" "$(smt_activate_file "$m")"
        done < <(smt_share_targets)
    fi
    smt_dry_write 20-settings.yaml "" "$(smt_settings_file)"
    if [ -n "$SMT_PROM_CA" ]; then
        smt_prometheus_ca_yaml > "$SMT_OUT/25-prometheus-ca.yaml"
        echo "  $SMT_OUT/25-prometheus-ca.yaml"
    fi
    smt_dry_write 30-preflight.yaml controller "$SMT_DIR/$SMT_PREFLIGHT"
    smt_dry_write 40-scaler-tls.yaml "" "$SMT_DIR/scaling-manager-scaler-tls.yaml"
    smt_dry_write 50-controller.yaml controller "$SMT_DIR/$SMT_MAIN"
}

smt_dry_begin() {
    mkdir -p "$SMT_OUT" || smt_usage "--out $SMT_OUT: cannot create it"
    echo "Rendered for $SMT_PLATFORM, namespace $SMT_NS (--dry-run: no cluster was read; apply in this order):"
}

# --- The commands ------------------------------------------------------------

smt_begin() {
    SMT_TMP="$(mktemp -d)" || smt_exit 7 "cannot create a scratch directory"
    trap 'rm -rf "$SMT_TMP"' EXIT
    # The libraries print in colour on a terminal and read these names.
    # shellcheck disable=SC2034
    if [ -t 2 ]; then
        BLUE='\033[0;34m' GREEN='\033[0;32m' YELLOW='\033[1;33m' RED='\033[0;31m' NC='\033[0m'
    else
        BLUE='' GREEN='' YELLOW='' RED='' NC=''
    fi
    SMT_SETTINGS_CREATED=false
}

smt_header() {
    echo "smt $1 $(smt_version) (release files: ${SMT_FILES_FROM}; $SMT_PLATFORM; namespace $SMT_NS)"
    [ "$SMT_PLATFORM" != openshift ] || echo "  The OpenShift files are not yet tested end to end."
}

# smt_admin_setup -n NS [--keda-namespace KNS]: the part of the cluster admin.
smt_admin_setup() {
    smt_begin
    smt_parse admin-setup "$@"
    smt_load_files
    smt_detect_platform
    smt_files_for
    smt_need "$SMT_MAIN" "$SMT_PREFLIGHT" scaling-manager-model-namespace.yaml
    smt_need scaling-manager-activate.yaml
    if [ "$SMT_DRY_RUN" = true ]; then
        smt_resolve_keda_ns admin-setup
        SMT_POLICY_NAMES=""
        smt_dsetw_decide
        smt_render
        smt_render_readers
        smt_render_dsetw_record
        smt_dry_begin
        smt_dry_admin
        smt_stamp_yaml > "$SMT_OUT/19-admin-setup-stamp.yaml"
        echo "  $SMT_OUT/19-admin-setup-stamp.yaml"
        [ "$SMT_PLATFORM" != openshift ] || echo "  then: kubectl label namespace $SMT_NS openshift.io/user-monitoring=true --overwrite"
        echo "KEDA's namespace in the scaler NetworkPolicy: $SMT_KEDA_NS ($SMT_KEDA_FROM)."
        echo "Weights labeller: admin-setup adds ${SMT_MODEL_NS[*]:-$SMT_NS} to its watch list when one runs (smt weights labeller --add -n <namespace>)."
        if [ ${#SMT_MODEL_NS[@]} -eq 0 ] && [ "$SMT_SINGLE" = false ]; then
            echo "Rendered for a controller that manages $SMT_NS only: --dry-run reads no cluster, so not the namespaces admin-setup granted before. If it manages others, give each with --model-namespace; applied as they are, these files narrow the Namespace reader and the stamp to $SMT_NS."
        fi
        [ ${#SMT_MODEL_NS[@]} -eq 0 ] || echo "The Namespace reader names the model namespaces, not a policy namespace their scaling-manager.llmd.ai/policy-namespace label names (--dry-run reads no labels): add one by hand."
        [ "$SMT_DSETW" = true ] || echo "DisaggregatedSet role writer: not rendered (no --dset-role-writer; --dry-run reads no cluster, so not a grant the last run kept)."
        return 0
    fi
    smt_header admin-setup
    smt_require_namespace
    smt_admin_models
    smt_dsetw_decide
    smt_resolve_keda_ns admin-setup
    echo "KEDA runs in $SMT_KEDA_NS ($SMT_KEDA_FROM)."
    smt_render
    smt_render_readers
    smt_render_dsetw_record
    echo "Admin objects:"
    smt_apply "$SMT_DIR/$SMT_MAIN" admin
    echo "Preflight RBAC:"
    smt_apply "$SMT_DIR/$SMT_PREFLIGHT" admin
    if [ ${#SMT_MODEL_NS[@]} -eq 0 ]; then
        echo "Model namespace:"
        smt_apply_model_ns "$SMT_NS" "$SMT_DIR/scaling-manager-model-namespace.yaml"
        smt_epp_monitors "$SMT_NS"
    fi
    local m f
    for m in "${SMT_MODEL_NS[@]}"; do
        echo "Model namespace $m:"
        f="$(smt_model_file "$m")"
        smt_apply_model_ns "$m" "$f"
        smt_epp_monitors "$m"
        smt_apply "$(smt_preflight_reads_file "$m")"
        if [ "$SMT_PLATFORM" = openshift ]; then
            smt_kc label namespace "$m" openshift.io/user-monitoring=true --overwrite
        fi
    done
    for m in $SMT_PREV_MODELS; do
        printf '%s\n' "${SMT_MODEL_NS[@]}" | grep -qxF -- "$m" && continue
        echo "No longer managed, $m: the grants of this controller there are removed:"
        smt_revoke_model "$m"
    done
    smt_dsetw_apply
    if [ "$SMT_PLATFORM" = openshift ]; then
        smt_kc label namespace "$SMT_NS" openshift.io/user-monitoring=true --overwrite
        echo "Labelled namespace $SMT_NS openshift.io/user-monitoring=true."
    fi
    smt_stamp_yaml > "$SMT_TMP/stamp.yaml"
    smt_kc apply -f "$SMT_TMP/stamp.yaml"
    echo "Stamp: ConfigMap $SMT_STAMP ($(smt_version), KEDA in $SMT_KEDA_NS$([ ${#SMT_MODEL_NS[@]} -eq 0 ] || echo ", manages $(smt_model_list | sed 's/,/, /g')")$([ "$SMT_DSETW_WANT" = true ] && printf ', role writer granted'))."
    smt_admin_setup_labeller
    echo
    echo "Done. The namespace owner runs next, with their own kubeconfig:"
    echo "  smt install -n $SMT_NS --prometheus-url <https://prometheus:9090>$([ "$SMT_DSETW_WANT" = true ] && printf ' --dset-role-writer')   (an http:// Prometheus: add --allow-http)"
    if [ ${#SMT_MODEL_NS[@]} -gt 0 ] || [ -n "${SMT_PREV_MODELS// /}" ]; then
        echo "  It reads the model namespaces from the stamp. After a change to them, the owner runs it again, right away: until then the controller still names a namespace it lost. With two or more named it reports that one unreadable and manages the others; with one named, it cannot read it."
    fi
}

# smt_install: the part of the namespace owner, or with --scope cluster everything.
smt_install() {
    smt_begin
    smt_parse install "$@"
    smt_load_files
    smt_detect_platform
    smt_files_for
    smt_need "$SMT_MAIN" "$SMT_PREFLIGHT" scaling-manager-settings.yaml scaling-manager-scaler-tls.yaml scaling-manager-model-namespace.yaml
    if [ "$SMT_DRY_RUN" = true ]; then
        [ "$SMT_SCOPE" = namespace ] || smt_resolve_keda_ns install
        smt_dsetw_decide
        smt_render
        smt_render_dsetw_switch "$SMT_DSETW_WANT"
        [ "$SMT_SCOPE" = cluster ] || { smt_render_watch; smt_render_preflight_watch; }
        smt_dry_begin
        if [ "$SMT_SCOPE" = cluster ]; then
            jq -n --arg n "$SMT_NS" '{apiVersion: "v1", kind: "Namespace", metadata: {name: $n}}' | yq -P > "$SMT_OUT/05-namespace.yaml"
            echo "  $SMT_OUT/05-namespace.yaml"
            smt_dry_admin
        fi
        smt_dry_install
        [ "$SMT_SCOPE" = namespace ] || smt_weights_labeller_dry
        if [ "$SMT_SCOPE" = namespace ] && [ ${#SMT_MODEL_NS[@]} -eq 0 ]; then
            echo "Rendered for a controller that manages $SMT_NS only: --dry-run reads no stamp. If admin-setup named model namespaces, give the same --model-namespace list."
        fi
        [ "$SMT_SCOPE" = namespace ] || echo "KEDA's side of mutual TLS is not rendered: it copies the client certificate the controller makes. After the apply, run: smt install --scope cluster ... again."
        return 0
    fi
    smt_header install
    if [ "$SMT_SCOPE" = cluster ]; then
        smt_install_cluster
        return
    fi
    smt_render
    smt_require_namespace
    smt_check_stamp
    smt_render_watch
    smt_render_preflight_watch
    [ ${#SMT_MODEL_NS[@]} -eq 0 ] || echo "Manages $(smt_model_list | sed 's/,/, /g') (ConfigMap $SMT_STAMP); runs in $SMT_NS."
    smt_check_adoptable
    smt_settings_pairs
    smt_settings_preconditions
    smt_share_grant
    smt_settings
    smt_prometheus_ca
    smt_preflight_first
    echo "Scaler TLS Secrets:"
    smt_scaler_secrets
    smt_controller
    smt_preflight after-install
    smt_preflight_gate "the controller is installed"
    echo
    echo "Installed in $SMT_NS, passive$([ "${SMT_DSETW_ON:-false}" = true ] && printf '; the DisaggregatedSet role writer is on and writes no replicas while a model is passive')."
    smt_share_summary
    smt_summary_lines
    echo
    echo "Left for the cluster admin (KEDA's client certificate, then drop the preflight's cluster reads):"
    echo "  smt admin-finish -n $SMT_NS"
    echo "Then this prints \"mutual TLS\":"
    echo "  kubectl get configmap $SMT_STATUS_CM -n $SMT_NS -o jsonpath='{.data.scalerTLS}{\"\\n\"}'"
}

# smt_install_cluster: admin-setup, install and admin-finish in one, for the
# cluster-wide files in scaling-manager-system. No stamp: the one who would
# read it wrote the admin objects in this run.
smt_install_cluster() {
    local m f
    # Everything that can refuse, before the first write.
    for m in "${SMT_MODEL_NS[@]}"; do
        smt_get namespace "$m" || smt_exit 4 "model namespace $m does not exist"
    done
    smt_resolve_keda_ns install
    echo "KEDA runs in $SMT_KEDA_NS ($SMT_KEDA_FROM)."
    smt_dsetw_decide
    smt_render
    smt_render_dsetw_switch "$SMT_DSETW_WANT"
    smt_check_adoptable
    smt_settings_pairs
    smt_settings_preconditions
    if ! smt_get namespace "$SMT_NS"; then
        smt_kc create namespace "$SMT_NS"
        echo "Created namespace $SMT_NS."
    fi
    echo "Admin objects:"
    smt_apply "$SMT_DIR/$SMT_MAIN" admin
    echo "Preflight RBAC:"
    smt_apply "$SMT_DIR/$SMT_PREFLIGHT" admin
    [ ${#SMT_MODEL_NS[@]} -gt 0 ] || echo "No --model-namespace: no model-server PodMonitor is applied. Add each namespace with model servers with --model-namespace, and run this again."
    for m in "${SMT_MODEL_NS[@]}"; do
        echo "Model namespace $m:"
        f="$(smt_model_file "$m")"
        smt_apply_model_ns "$m" "$f"
        smt_epp_monitors "$m"
        if [ "$SMT_PLATFORM" = openshift ]; then
            smt_kc label namespace "$m" openshift.io/user-monitoring=true --overwrite
        fi
    done
    smt_dsetw_apply
    smt_share_grant
    smt_settings
    smt_prometheus_ca
    smt_preflight_first
    echo "Scaler TLS Secrets:"
    smt_scaler_secrets
    smt_controller
    smt_weights_labeller
    smt_finish
    echo
    echo "Installed in $SMT_NS for the cluster, passive, serving the scaler over mutual TLS$([ "$SMT_DSETW_WANT" = true ] && printf '; the DisaggregatedSet role writer is on and writes no replicas while a model is passive')."
    smt_share_summary
    smt_summary_lines
}

# smt_admin_finish -n NS: the cluster admin, after the install.
smt_admin_finish() {
    smt_begin
    smt_parse admin-finish "$@"
    smt_load_files
    smt_detect_platform
    smt_files_for
    smt_need "$SMT_PREFLIGHT"
    smt_header admin-finish
    smt_require_namespace
    smt_render
    smt_finish
    echo
    echo "Done: $SMT_NS serves the scaler over mutual TLS."
    smt_summary_lines
}

# smt_release_files --out DIR: the release files of the image, to verify or apply by hand.
smt_release_files() {
    local src f n=0
    smt_begin
    smt_parse release-files "$@"
    [ -n "$SMT_OUT" ] || smt_usage "release-files needs --out <dir>"
    src="$(smt_baked_dir)"
    [ -f "$src/SHA256SUMS" ] || smt_usage "this image carries no release files (a development build)"
    mkdir -p "$SMT_OUT" || smt_usage "--out $SMT_OUT: cannot create it"
    for f in "$src"/scaling-manager.yaml "$src"/scaling-manager-*.yaml "$src"/scaling-manager-*.sh "$src"/*.sigstore.json "$src/SHA256SUMS"; do
        [ -f "$f" ] || continue
        cp "$f" "$SMT_OUT/"
        n=$((n + 1))
    done
    echo "Wrote $n files of release $(smt_version) to $SMT_OUT."
    echo "scaling-manager-tools-$(smt_version).tar.gz is on the GitHub release, not in this image: SHA256SUMS lists it, so check with:"
    echo "  sha256sum -c SHA256SUMS --ignore-missing"
}
