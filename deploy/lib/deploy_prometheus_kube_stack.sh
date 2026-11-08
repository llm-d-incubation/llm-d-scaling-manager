#!/usr/bin/env bash
#
# Shared kube-prometheus-stack install for Kubernetes-like environments
# (vanilla Kubernetes, Kind emulator, etc.). Sourced by deploy/*/install.sh.
# Requires vars: MONITORING_NAMESPACE, PROMETHEUS_SECRET_NAME,
# PROMETHEUS_PORT, PROMETHEUS_URL, KUBE_PROMETHEUS_STACK_CHART_VERSION.
# Requires funcs: log_info/log_warning/log_success.
#

# kps_installed_chart_version echoes the chart version of the
# kube-prometheus-stack release in MONITORING_NAMESPACE, or nothing when there
# is none. It FAILS, with helm's error on stderr, when the releases cannot be
# listed: a list that failed is not "no release". Read as one, a transient
# error or a Forbidden list installed the pin over a newer release that other
# installs share.
kps_installed_chart_version() {
    local out err rc=0 chart
    err="$(mktemp)"
    out="$(helm list -n "$MONITORING_NAMESPACE" -o json 2>"$err")" || rc=$?
    if [ "$rc" -ne 0 ]; then
        printf 'helm list -n %s failed (exit %s): %s\n' "$MONITORING_NAMESPACE" "$rc" "$(head -3 "$err")" >&2
        rm -f "$err"
        return 1
    fi
    rm -f "$err"
    if ! chart="$(printf '%s' "$out" | jq -r '[.[]? | select(.name == "kube-prometheus-stack") | .chart // ""] | .[0] // ""')"; then
        printf 'helm list -n %s answered something that is not JSON\n' "$MONITORING_NAMESPACE" >&2
        return 1
    fi
    case "$chart" in
        kube-prometheus-stack-*) printf '%s\n' "${chart#kube-prometheus-stack-}" ;;
    esac
}

# kps_chart_version_to_install echoes the chart version to install: the pin,
# unless a NEWER release is already installed and nobody set the pin.
#
# The release is shared by every scaling manager on the cluster, and other installs may
# have upgraded it. Installing the default pin over a newer one downgraded
# their Prometheus, which a chart does not promise to survive. An explicit
# KUBE_PROMETHEUS_STACK_CHART_VERSION is a choice and is honoured.
kps_chart_version_to_install() {
    local pin="$KUBE_PROMETHEUS_STACK_CHART_VERSION" installed
    if [ -n "${KUBE_PROMETHEUS_STACK_CHART_VERSION_EXPLICIT:-}" ]; then
        printf '%s\n' "$pin"
        return 0
    fi
    installed="$(kps_installed_chart_version)" || return 1
    if [ -n "$installed" ] && [ "$installed" != "$pin" ] \
        && [ "$(printf '%s\n%s\n' "$installed" "$pin" | sort -V | tail -1)" = "$installed" ]; then
        log_warning "kube-prometheus-stack $installed is installed in $MONITORING_NAMESPACE, newer than this install's pin ($pin). Keeping $installed rather than downgrading a release other installs share. To install $pin anyway, set KUBE_PROMETHEUS_STACK_CHART_VERSION=$pin."
        printf '%s\n' "$installed"
        return 0
    fi
    printf '%s\n' "$pin"
}

deploy_prometheus_kube_stack() {
    log_info "Deploying kube-prometheus-stack with TLS..."

    helm repo add prometheus-community https://prometheus-community.github.io/helm-charts || true
    if [ "${SKIP_HELM_REPO_UPDATE:-}" = "true" ]; then
        log_info "Skipping helm repo update (SKIP_HELM_REPO_UPDATE=true)"
    else
        helm repo update
    fi

    # A re-run keeps the certificate already there while it has 30 days left.
    # A new one on every run made Prometheus serve the old certificate until it
    # reloaded the Secret, while the controller had already been given the new
    # CA: a re-run of setup-prereqs followed by an install failed the TLS probe.
    local existing_crt
    existing_crt="$(kubectl get secret "$PROMETHEUS_SECRET_NAME" -n "$MONITORING_NAMESPACE" \
        -o jsonpath='{.data.tls\.crt}' 2>/dev/null | base64 -d 2>/dev/null || true)"
    if [ -n "$existing_crt" ] && printf '%s\n' "$existing_crt" | openssl x509 -noout -checkend 2592000 >/dev/null 2>&1 \
        && printf '%s\n' "$existing_crt" | openssl x509 -noout -text 2>/dev/null \
            | grep -q "DNS:kube-prometheus-stack-prometheus.${MONITORING_NAMESPACE}.svc"; then
        log_info "Keeping the Prometheus TLS certificate in $MONITORING_NAMESPACE/$PROMETHEUS_SECRET_NAME (valid for 30 days or more)"
    else
        log_info "Creating self-signed TLS certificate for Prometheus"
        openssl req -x509 -newkey rsa:2048 -nodes \
            -keyout /tmp/prometheus-tls.key \
            -out /tmp/prometheus-tls.crt \
            -days 365 \
            -subj "/CN=prometheus" \
            -addext "subjectAltName=DNS:kube-prometheus-stack-prometheus.${MONITORING_NAMESPACE}.svc.cluster.local,DNS:kube-prometheus-stack-prometheus.${MONITORING_NAMESPACE}.svc,DNS:prometheus,DNS:localhost" \
            &> /dev/null

        log_info "Creating Kubernetes secret for Prometheus TLS"
        # Not `&> /dev/null`. This apply failing is how the install learns the
        # monitoring namespace is missing, and suppressing its output turned that into
        # a bare exit under `set -e`: the last thing printed was "Creating Kubernetes
        # secret for Prometheus TLS" and there was no error anywhere.
        if ! kubectl create secret tls "$PROMETHEUS_SECRET_NAME" \
            --cert=/tmp/prometheus-tls.crt \
            --key=/tmp/prometheus-tls.key \
            -n "$MONITORING_NAMESPACE" \
            --dry-run=client -o yaml | kubectl apply -f -; then
            rm -f /tmp/prometheus-tls.key /tmp/prometheus-tls.crt
            log_error "Could not create the Prometheus TLS secret in $MONITORING_NAMESPACE (see the error above)."
        fi

        rm -f /tmp/prometheus-tls.key /tmp/prometheus-tls.crt
    fi

    log_info "Installing kube-prometheus-stack with TLS configuration"
    install_operational_dashboard

    # kube-prometheus-stack is one release in one namespace shared by every scaling manager on
    # the cluster, and "is there one already" is checked before this runs — which
    # is not atomic. Two first-time installs starting together both see no stack
    # and both write the same release. Helm marks an in-flight release
    # pending-install/upgrade, so refuse on that rather than racing it.
    local phelm_status
    phelm_status="$(helm status kube-prometheus-stack -n "$MONITORING_NAMESPACE" -o json 2>/dev/null | jq -r '.info.status // ""' 2>/dev/null || true)"
    case "$phelm_status" in
        pending-*)
            log_error "Another install is deploying kube-prometheus-stack in $MONITORING_NAMESPACE right now (helm reports it $phelm_status). It is shared by every scaling manager on this cluster, so this install would race it. Wait for it to finish and re-run, or pass PROMETHEUS_URL=<url> to use a Prometheus this install does not deploy."
            ;;
    esac

    if [ -z "${KUBE_PROMETHEUS_STACK_CHART_VERSION:-}" ]; then
        log_error "KUBE_PROMETHEUS_STACK_CHART_VERSION is empty: refusing an unpinned kube-prometheus-stack, which upgrades the release every scaling manager on this cluster shares to whatever the repository serves today."
    fi
    # Grafana is the one image this chart pulls from Docker Hub
    # (docker.io/grafana/grafana; Grafana publishes nowhere else). Docker Hub
    # rate-limits anonymous pulls, and a CI runner or a busy node that hits the
    # limit leaves Grafana Pending until --wait gives up. mirror.gcr.io is
    # Google's public Docker Hub mirror: same repository path, same tag, same
    # digest. The tag is left to the chart, so it moves with the chart pin.
    # The chart's Grafana test pod (docker.io/bats/bats) runs only on
    # `helm test`, which nothing here runs, so it is not rendered at all.
    # hack/check-kps-images.sh fails when a rendered image resolves to docker.io.
    local grafana_registry="${GRAFANA_IMAGE_REGISTRY:-mirror.gcr.io}"
    local kps_version
    kps_version="$(kps_chart_version_to_install)" \
        || log_error "Could not list the Helm releases in $MONITORING_NAMESPACE (see the error above), so this install cannot tell whether a newer kube-prometheus-stack is already there, and installing $KUBE_PROMETHEUS_STACK_CHART_VERSION could downgrade a release every scaling manager on the cluster shares. Fix the error and re-run, or set KUBE_PROMETHEUS_STACK_CHART_VERSION=<version> to choose the version yourself."
    helm upgrade --install kube-prometheus-stack prometheus-community/kube-prometheus-stack \
        --version "$kps_version" \
        -n "$MONITORING_NAMESPACE" \
        --set prometheus.prometheusSpec.serviceMonitorSelectorNilUsesHelmValues=false \
        --set prometheus.prometheusSpec.podMonitorSelectorNilUsesHelmValues=false \
        --set prometheus.service.type=ClusterIP \
        --set prometheus.service.port="$PROMETHEUS_PORT" \
        --set prometheus.prometheusSpec.web.tlsConfig.cert.secret.name="$PROMETHEUS_SECRET_NAME" \
        --set prometheus.prometheusSpec.web.tlsConfig.cert.secret.key=tls.crt \
        --set prometheus.prometheusSpec.web.tlsConfig.keySecret.name="$PROMETHEUS_SECRET_NAME" \
        --set prometheus.prometheusSpec.web.tlsConfig.keySecret.key=tls.key \
        --set grafana.enabled="$DEPLOY_OPERATIONAL_DASHBOARD" \
        --set grafana.image.registry="$grafana_registry" \
        --set grafana.testFramework.enabled=false \
        --set grafana.sidecar.dashboards.enabled=true \
        --set grafana.sidecar.dashboards.label=grafana_dashboard \
        --set 'grafana.datasources.datasources\.yaml.apiVersion=1' \
        --set-string 'grafana.datasources.datasources\.yaml.datasources[0].name=Prometheus' \
        --set-string 'grafana.datasources.datasources\.yaml.datasources[0].type=prometheus' \
        --set-string 'grafana.datasources.datasources\.yaml.datasources[0].url=https://kube-prometheus-stack-prometheus.'"$MONITORING_NAMESPACE"'.svc.cluster.local:9090' \
        --set-string 'grafana.datasources.datasources\.yaml.datasources[0].access=proxy' \
        --set-string 'grafana.datasources.datasources\.yaml.datasources[0].jsonData.httpMethod=POST' \
        --set-string 'grafana.datasources.datasources\.yaml.datasources[0].jsonData.timeInterval=30s' \
        --set 'grafana.datasources.datasources\.yaml.datasources[0].jsonData.tlsSkipVerify=true' \
        --set alertmanager.enabled=false \
        --timeout=10m \
        --wait

    log_success "kube-prometheus-stack deployed with TLS"
    log_info "Prometheus URL: $PROMETHEUS_URL"
}

undeploy_prometheus_kube_stack() {
    log_info "Uninstalling kube-prometheus-stack..."

    helm uninstall kube-prometheus-stack -n "$MONITORING_NAMESPACE" 2>/dev/null || \
        log_warning "Prometheus stack not found or already uninstalled"

    kubectl delete secret "$PROMETHEUS_SECRET_NAME" -n "$MONITORING_NAMESPACE" --ignore-not-found

    log_success "Prometheus stack uninstalled"
}
