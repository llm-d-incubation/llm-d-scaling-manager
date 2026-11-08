#!/usr/bin/env bash
#
# Renders the kube-prometheus-stack release the installer deploys, and fails
# when any image in it resolves to Docker Hub.
#
# AGENTS.md: never use images from docker.io in e2e tests. The e2e monitoring
# stack is not Go code, so hack/check-e2e-images.sh never saw it: the chart's
# Grafana defaulted to docker.io/grafana/grafana, and two unrelated PRs failed
# e2e setup within the hour on "kube-prometheus-stack-grafana not ready" before
# a single spec ran. Docker Hub rate-limits anonymous pulls from CI runners.
#
# This runs the REAL deploy_prometheus_kube_stack against stub kubectl and
# openssl, with a helm stub that turns its `helm upgrade --install` into a
# `helm template` of the pinned chart. So the --set list under test is the one
# the installer ships, not a copy of it; a chart bump that adds a Docker Hub
# image fails here too.
#
# Every image must name a registry host (a first path segment with a dot or a
# port, or localhost) other than docker.io. Needs helm and network access to
# pull the chart; KPS_CHART_DIR=<dir> uses an already-untarred chart instead.

set -u

ROOT="${CHECK_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
REPO_URL="https://prometheus-community.github.io/helm-charts"

command -v helm >/dev/null 2>&1 || {
    printf 'FATAL: helm is required to run this check.\n' >&2
    exit 1
}

VERSION="$(sed -n 's/^KUBE_PROMETHEUS_STACK_CHART_VERSION=\${KUBE_PROMETHEUS_STACK_CHART_VERSION:-\([0-9][0-9.]*\)}$/\1/p' "$ROOT/deploy/install.sh" | tr -d '\r')"
[ -n "$VERSION" ] || {
    printf 'FATAL: could not read the default KUBE_PROMETHEUS_STACK_CHART_VERSION from deploy/install.sh.\n' >&2
    exit 1
}

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

CHART="${KPS_CHART_DIR:-}"
if [ -z "$CHART" ]; then
    # The chart CDN has answered 5xx before; three tries before a red.
    for attempt in 1 2 3; do
        if helm pull kube-prometheus-stack --repo "$REPO_URL" --version "$VERSION" \
            --untar --untardir "$WORK/chart" >"$WORK/pull.log" 2>&1; then
            break
        fi
        [ "$attempt" -eq 3 ] && {
            printf 'FATAL: helm pull kube-prometheus-stack %s failed 3 times:\n' "$VERSION" >&2
            cat "$WORK/pull.log" >&2
            exit 1
        }
        sleep 10
    done
    CHART="$WORK/chart/kube-prometheus-stack"
fi
[ -f "$CHART/Chart.yaml" ] || {
    printf 'FATAL: %s is not a chart directory.\n' "$CHART" >&2
    exit 1
}

# The installer's function, run in a subshell so its stubs and variables stay
# there. DEPLOY_OPERATIONAL_DASHBOARD=true is what the e2e targets set, and it
# is the superset: false only removes Grafana.
(
    log_info() { :; }
    log_success() { :; }
    log_warning() { printf 'installer warning: %s\n' "$*" >&2; }
    log_error() { printf 'FATAL (installer): %s\n' "$*" >&2; exit 1; }
    install_operational_dashboard() { :; }
    kubectl() { return 0; }
    openssl() { return 0; }
    helm() {
        case "$1" in
            repo|status) return 0 ;;
            list) printf '[]\n' ;;
            upgrade)
                shift
                [ "${1:-}" = "--install" ] && shift
                local release="$1"
                shift 2
                local args=()
                while [ $# -gt 0 ]; do
                    case "$1" in
                        --version) shift 2 ;;
                        --timeout=*|--wait|--atomic) shift ;;
                        --timeout) shift 2 ;;
                        *) args+=("$1"); shift ;;
                    esac
                done
                command helm template "$release" "$CHART" "${args[@]}"
                ;;
            *) printf 'unexpected helm call: helm %s\n' "$*" >&2; return 1 ;;
        esac
    }
    MONITORING_NAMESPACE=scaling-manager-monitoring
    PROMETHEUS_SECRET_NAME=prometheus-web-tls
    PROMETHEUS_PORT=9090
    PROMETHEUS_URL=https://kube-prometheus-stack-prometheus.scaling-manager-monitoring.svc.cluster.local:9090
    KUBE_PROMETHEUS_STACK_CHART_VERSION="$VERSION"
    KUBE_PROMETHEUS_STACK_CHART_VERSION_EXPLICIT="$VERSION"
    DEPLOY_OPERATIONAL_DASHBOARD=true
    SKIP_HELM_REPO_UPDATE=true
    unset GRAFANA_IMAGE_REGISTRY
    # shellcheck source=../deploy/lib/deploy_prometheus_kube_stack.sh
    source "$ROOT/deploy/lib/deploy_prometheus_kube_stack.sh"
    deploy_prometheus_kube_stack
) </dev/null >"$WORK/rendered.yaml" 2>"$WORK/render.err" || {
    printf 'FATAL: rendering the installer release failed:\n' >&2
    cat "$WORK/render.err" >&2
    exit 1
}

# Container images, plus the images the operator is told to run itself (the
# config reloader sidecar and the default base images).
{
    grep -oE '^[[:space:]]*(-[[:space:]]+)?image:[[:space:]]*"?[^"[:space:]]+' "$WORK/rendered.yaml" \
        | sed -E 's/.*image:[[:space:]]*"?//'
    grep -oE -- '--(prometheus-config-reloader|[a-z]+-default-base-image)=[^"[:space:],]+' "$WORK/rendered.yaml" \
        | sed -E 's/^[^=]*=//'
} | sort -u >"$WORK/images.txt"

FAIL=0
PASS=0
while IFS= read -r ref; do
    [ -n "$ref" ] || continue
    host="${ref%%/*}"
    [ "$host" = "$ref" ] && host=""
    case "$host" in
        docker.io|index.docker.io|registry-1.docker.io)
            FAIL=$((FAIL + 1)); echo "FAIL $ref is on Docker Hub" ;;
        localhost|localhost:*|*.*|*:*)
            PASS=$((PASS + 1)); echo "ok   $ref" ;;
        *)
            FAIL=$((FAIL + 1)); echo "FAIL $ref has no registry host, so it resolves to docker.io" ;;
    esac
done <"$WORK/images.txt"

# A render that lost Grafana or Prometheus would pass on fewer images.
# mirror.gcr.io is Google's pull-through mirror of Docker Hub. It serves what
# has been pulled through it, so a chart bump to a tag nobody has pulled yet
# may not be there, and the install would reach CI as ImagePullBackOff with the
# chart's tag in a registry that never had it. Ask the mirror for each tag.
manifest_exists() {
    if command -v crane >/dev/null 2>&1; then
        crane manifest "$1" >/dev/null 2>"$WORK/manifest.err"
    else
        docker manifest inspect "$1" >/dev/null 2>"$WORK/manifest.err"
    fi
}
MIRRORED="$(grep '^mirror\.gcr\.io/' "$WORK/images.txt" || true)"
if [ -n "$MIRRORED" ]; then
    command -v crane >/dev/null 2>&1 || command -v docker >/dev/null 2>&1 || {
        printf 'FATAL: crane or docker is required to check that mirror.gcr.io serves %s.\n' "$(printf '%s' "$MIRRORED" | tr '\n' ' ')" >&2
        exit 1
    }
    while IFS= read -r ref; do
        found=""
        for attempt in 1 2 3; do
            if manifest_exists "$ref"; then found=1; break; fi
            [ "$attempt" -lt 3 ] && sleep 5
        done
        if [ -n "$found" ]; then
            echo "ok   $ref is served by the mirror"
        else
            FAIL=$((FAIL + 1))
            echo "FAIL $ref: mirror.gcr.io does not serve this tag ($(head -1 "$WORK/manifest.err" | tr -d '\r')). Pull it through the mirror once, or set GRAFANA_IMAGE_REGISTRY to a registry that has it."
        fi
    done <<<"$MIRRORED"
fi

for want in grafana/grafana prometheus/prometheus prometheus-operator/prometheus-operator; do
    grep -q "/$want:" "$WORK/images.txt" || {
        FAIL=$((FAIL + 1)); echo "FAIL the render has no $want image; it is not the release the installer deploys"
    }
done

echo "check-kps-images: kube-prometheus-stack $VERSION, $PASS fully-qualified, $FAIL failed"
[ "$FAIL" -eq 0 ] && [ "$PASS" -gt 0 ]
