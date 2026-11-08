#!/usr/bin/env bash
#
# Executes the installer's default-choosing helpers against fixtures.
#
# These decide what a plain `make deploy-scaling-manager` ships: which image is pulled and
# how, and whether the operator is told when the result is not reproducible.
# Every defect here parses cleanly and installs cleanly -- a registry port read
# as a tag, a digest treated as a moving tag, a floating tag deployed in
# silence -- so `bash -n` and a green install both miss it.
#
# WRITING A CASE HERE: an expected-EMPTY case proves nothing on its own -- a
# function that does not exist returns empty too. The guard below refuses to run
# unless every function is defined, and EXPECT_CASES refuses to pass a run that
# ended early.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

command -v make >/dev/null 2>&1 || {
    printf 'FATAL: make is required to run these checks.\n' >&2
    exit 1
}
command -v yq >/dev/null 2>&1 || {
    printf 'FATAL: yq is required to run these checks.\n' >&2
    exit 1
}
command -v jq >/dev/null 2>&1 || {
    printf 'FATAL: jq is required to run these checks.\n' >&2
    exit 1
}

# Sourced through CR-stripped copies: a Windows checkout with core.autocrlf
# makes bash stop at the first function definition, which looks exactly like
# every function being missing.
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
for lib in common.sh cli.sh infra_scaling_manager.sh scaledobject.sh verify.sh scaler_tls.sh; do
    tr -d '\r' < "$ROOT/deploy/lib/$lib" > "$WORK/$lib"
done
BLUE='' GREEN='' YELLOW='' RED='' NC=''
# shellcheck source=/dev/null
source "$WORK/common.sh"
# shellcheck source=/dev/null
source "$WORK/cli.sh"
# shellcheck source=/dev/null
source "$WORK/infra_scaling_manager.sh"
# shellcheck source=/dev/null
source "$WORK/scaledobject.sh"
# shellcheck source=/dev/null
source "$WORK/verify.sh"
# shellcheck source=/dev/null
source "$WORK/scaler_tls.sh"

for fn in llmsm_split_image_ref llmsm_image_ref_fields llmsm_image_pull_policy llmsm_warn_floating_image \
          llmsm_warn_prometheus_tls llmsm_prometheus_skip_verify llmsm_prometheus_scraper_sa llmsm_prometheus_url_namespace \
          llmsm_append_metrics_reader_decision render_default_scaledobject \
          llmsm_read_live_prometheus_tls llmsm_warn_prometheus_tls_upgrade \
          llmsm_prometheus_ca_path llmsm_refuse_unknown_prometheus_tls llmsm_rerun_cmd llmsm_render_manager_config_patch \
          llmsm_hint_untrusted_prometheus_cert llmsm_write_prometheus_settings \
          llmsm_rerun_install_vars llmsm_same_prometheus_as_live llmsm_prometheus_keys llmsm_restart_on_prometheus_change \
          llmsm_image_ref print_summary llmsm_summary_scaledobjects; do
    declare -F "$fn" >/dev/null || {
        printf 'FATAL: %s is not defined after sourcing the library.\n' "$fn" >&2
        exit 1
    }
done

# What a re-run command carries (llmsm_rerun_install_vars) comes from these; a
# value inherited from the shell running this check would change every
# expected command.
unset ENVIRONMENT_EXPLICIT PROMETHEUS_URL_EXPLICIT DEPLOY_PROMETHEUS \
      LLMSM_IMAGE_REPO LLMSM_IMAGE_TAG LLMSM_IMAGE_DIGEST LLMSM_DEFAULT_IMAGE_REF LLMSM_WATCH_NS

EXPECT_CASES=155
RAN=0
FAILED=0

# check <name> <want> <got>
check() {
    if [ "$2" = "$3" ]; then
        RAN=$((RAN + 1))
        printf 'ok    %s\n' "$1"
    else
        FAILED=$((FAILED + 1))
        printf 'FAIL  %s\n        want [%s]\n        got  [%s]\n' "$1" "$2" "$3" >&2
    fi
}

# ----------------------------------------------------------------------------
# Image reference: IMG -> repo, tag, digest
# ----------------------------------------------------------------------------
split() {
    LLMSM_IMAGE_REPO='' LLMSM_IMAGE_TAG='' LLMSM_IMAGE_DIGEST=''
    llmsm_split_image_ref "$1" || { printf 'rejected'; return 0; }
    printf '%s|%s|%s' "$LLMSM_IMAGE_REPO" "$LLMSM_IMAGE_TAG" "$LLMSM_IMAGE_DIGEST"
}

check 'image: repo:tag' \
    'ghcr.io/o/llm-d-scaling-manager|main|' \
    "$(split ghcr.io/o/llm-d-scaling-manager:main)"

# Splitting on the first colon read the port as the tag.
check 'image: a registry port is not a tag' \
    'localhost:5000/scaling-manager|dev|' \
    "$(split localhost:5000/scaling-manager:dev)"

check 'image: a digest is carried as a digest' \
    'ghcr.io/o/scaling-manager||sha256:abc123' \
    "$(split ghcr.io/o/scaling-manager@sha256:abc123)"

check 'image: repo:tag@digest keeps both' \
    'localhost:5000/scaling-manager|v1.2.3|sha256:abc123' \
    "$(split localhost:5000/scaling-manager:v1.2.3@sha256:abc123)"

check 'image: a bare repo is rejected' \
    'rejected' \
    "$(split localhost:5000/scaling-manager)"

# Through parse_args, the path IMG actually takes: a tagless IMG keeps the
# defaults rather than installing half of it.
via_img() {
    (
        LLMSM_IMAGE_REPO=default/repo LLMSM_IMAGE_TAG=deftag LLMSM_IMAGE_DIGEST=''
        IMG="$1"
        parse_args 2>/dev/null
        printf '%s|%s|%s' "$LLMSM_IMAGE_REPO" "$LLMSM_IMAGE_TAG" "$LLMSM_IMAGE_DIGEST"
    )
}

check 'IMG: registry port with tag' \
    'host:5000/team/scaling-manager|v1|' \
    "$(via_img host:5000/team/scaling-manager:v1)"

check 'IMG: digest pins by digest' \
    'ghcr.io/o/scaling-manager||sha256:0123abcd' \
    "$(via_img ghcr.io/o/scaling-manager@sha256:0123abcd)"

check 'IMG: no tag keeps the default image' \
    'default/repo|deftag|' \
    "$(via_img host:5000/team/scaling-manager)"

check 'IMG: plain repo:tag' \
    'ghcr.io/o/scaling-manager|main|' \
    "$(via_img ghcr.io/o/scaling-manager:main)"

# The fields the Makefile reads (deploy-e2e-infra, LLMSM_CONTROLLER_IMAGE_REPOS).
# It split IMG inline on the last colon, so a digest became repo
# "ghcr.io/o/w@sha256" with the hex as its tag.
DIGEST="sha256:$(printf '0123456789abcdef%.0s' 1 2 3 4)"
check 'fields: registry port with tag' 'localhost:5000/w|dev|' "$(llmsm_image_ref_fields localhost:5000/w:dev)"
check 'fields: ref-<sha> tag' 'ghcr.io/o/w|ref-abc|' "$(llmsm_image_ref_fields ghcr.io/o/w:ref-abc)"
check 'fields: digest' "ghcr.io/o/w||$DIGEST" "$(llmsm_image_ref_fields "ghcr.io/o/w@$DIGEST")"
check 'fields: bare repo pulls latest' 'ghcr.io/o/w|latest|' "$(llmsm_image_ref_fields ghcr.io/o/w)"

# ----------------------------------------------------------------------------
# The install summary: the image it names, and whether it says nothing scales
# ----------------------------------------------------------------------------
# ref <IMG>: the reference llmsm_image_ref rebuilds from IMG's split.
ref() {
    (
        LLMSM_IMAGE_REPO='' LLMSM_IMAGE_TAG='' LLMSM_IMAGE_DIGEST=''
        llmsm_split_image_ref "$1" || { printf 'rejected'; exit 0; }
        llmsm_image_ref
    )
}
check 'image ref: repo:tag' 'ghcr.io/o/scaling-manager:main' "$(ref ghcr.io/o/scaling-manager:main)"
check 'image ref: a digest is repo@digest' "ghcr.io/o/scaling-manager@$DIGEST" "$(ref "ghcr.io/o/scaling-manager@$DIGEST")"
check 'image ref: tag and digest' "localhost:5000/scaling-manager:v1@$DIGEST" "$(ref "localhost:5000/scaling-manager:v1@$DIGEST")"

# summary <ScaledObject list JSON, or FORBIDDEN> <IMG>: print_summary against a
# kubectl that answers only the ScaledObject list.
summary() {
    (
        SUMMARY_SO="$1"
        LLMSM_IMAGE_REPO='' LLMSM_IMAGE_TAG='' LLMSM_IMAGE_DIGEST=''
        llmsm_split_image_ref "$2" || exit 1
        kubectl() {
            case "$*" in
                "get scaledobjects.keda.sh "*)
                    if [ "$SUMMARY_SO" = FORBIDDEN ]; then
                        echo 'Error from server (Forbidden): scaledobjects.keda.sh is forbidden: User "tenant" cannot list resource "scaledobjects"' >&2
                        return 1
                    fi
                    printf '%s' "$SUMMARY_SO" ;;
                *) return 1 ;;
            esac
        }
        llmsm_prometheus_endpoint() { return 1; }
        LLMSM_NS=scaling-manager-sum LLMSM_SCOPE=namespace LLMSM_WATCH_NS='' SCALER_BACKEND=keda
        DEPLOY_OPERATIONAL_DASHBOARD=false MONITORING_NAMESPACE=scaling-manager-mon
        print_summary 2>/dev/null
    )
}
NO_SO='{"items":[]}'
OUR_SO='{"items":[{"metadata":{"namespace":"scaling-manager-sum","name":"chat"},"spec":{"triggers":[{"type":"external-push","metadata":{"scalerAddress":"scaling-manager-external-scaler.scaling-manager-sum.svc:9090"}}]}}]}'
OTHER_SO='{"items":[{"metadata":{"namespace":"scaling-manager-sum","name":"chat"},"spec":{"triggers":[{"type":"external-push","metadata":{"scalerAddress":"scaling-manager-external-scaler.elsewhere.svc:9090"}}]}}]}'

# A digest IMG printed "(ghcr.io/...:)" on a real cluster: the tag, empty.
check 'summary: a digest IMG names repo@digest' "  Controller:   scaling-manager-sum  (ghcr.io/o/scaling-manager@$DIGEST)" \
    "$(summary "$NO_SO" "ghcr.io/o/scaling-manager@$DIGEST" | grep 'Controller:')"
check 'summary: a tag IMG names repo:tag' '  Controller:   scaling-manager-sum  (ghcr.io/o/scaling-manager:main)' \
    "$(summary "$NO_SO" ghcr.io/o/scaling-manager:main | grep 'Controller:')"

# "nothing scales yet" printed on a re-install with a ScaledObject in place.
check 'summary: no ScaledObject says nothing scales yet' 1 \
    "$(summary "$NO_SO" ghcr.io/o/scaling-manager:main | grep -c 'NEXT: nothing scales yet')"
check 'summary: one naming this scaler is counted, and no NEXT' '0|1' \
    "$(out="$(summary "$OUR_SO" ghcr.io/o/scaling-manager:main)"; printf '%s|%s' "$(printf '%s\n' "$out" | grep -c 'NEXT:')" "$(printf '%s\n' "$out" | grep -c 'ScaledObjects: 1 name this scaler')")"
check 'summary: one naming another scaler does not count' 1 \
    "$(summary "$OTHER_SO" ghcr.io/o/scaling-manager:main | grep -c 'NEXT: nothing scales yet')"
# Forbidden is not "none".
check 'summary: an unreadable list is not called none' '0|1' \
    "$(out="$(summary FORBIDDEN ghcr.io/o/scaling-manager:main)"; printf '%s|%s' "$(printf '%s\n' "$out" | grep -c 'NEXT: nothing scales yet')" "$(printf '%s\n' "$out" | grep -c 'could not be listed')")"

# The Makefile itself, evaluated by make: the recipe and the repository list run
# exactly these expressions.
make_eval() {
    make -s --no-print-directory -C "$ROOT" --eval "b133-print: ; @echo '$1'" b133-print "${@:2}" 2>/dev/null
}
check 'Makefile: the e2e IMG split is the tested helper (digest)' "ghcr.io/o/w||$DIGEST" \
    "$(make_eval "\$(shell \$(LLMSM_IMAGE_REF_FIELDS) 'ghcr.io/o/w@$DIGEST')")"
check 'Makefile: deploy-e2e-infra splits IMG with the helper and passes the digest' '2' \
    "$(sed -n '/^deploy-e2e-infra:/,/^[^[:space:]#].*:/p' "$ROOT/Makefile" | tr -d '\r' \
        | grep -c -e 'IMG_FIELDS="\$\$(\$(LLMSM_IMAGE_REF_FIELDS)' -e 'LLMSM_IMAGE_DIGEST=\$\$IMAGE_DIGEST')"
check 'Makefile: controller repos, default IMG' 'ghcr.io/llm-d-incubation/llm-d-scaling-manager' \
    "$(make_eval '$(LLMSM_CONTROLLER_IMAGE_REPOS)')"
check 'Makefile: controller repos, a custom IMG is appended' 'ghcr.io/llm-d-incubation/llm-d-scaling-manager localhost:5000/w' \
    "$(make_eval '$(LLMSM_CONTROLLER_IMAGE_REPOS)' IMG=localhost:5000/w:dev)"
check 'Makefile: controller repos, a digest of the published repo adds nothing' 'ghcr.io/llm-d-incubation/llm-d-scaling-manager' \
    "$(make_eval '$(LLMSM_CONTROLLER_IMAGE_REPOS)' "IMG=ghcr.io/llm-d-incubation/llm-d-scaling-manager@$DIGEST")"
check 'Makefile: controller repos stay overridable' 'registry.local/scaling-manager' \
    "$(make_eval '$(LLMSM_CONTROLLER_IMAGE_REPOS)' IMG=localhost:5000/w:dev LLMSM_CONTROLLER_IMAGE_REPOS=registry.local/scaling-manager)"

# ----------------------------------------------------------------------------
# Pull policy: IfNotPresent only for what cannot move
# ----------------------------------------------------------------------------
check 'pull policy: main is Always'          'Always'       "$(llmsm_image_pull_policy main '')"
check 'pull policy: a digest is IfNotPresent' 'IfNotPresent' "$(llmsm_image_pull_policy main sha256:abc)"
check 'pull policy: a release tag is IfNotPresent' 'IfNotPresent' "$(llmsm_image_pull_policy v0.6.0 '')"
# ref-<sha> is re-pushed after a merge in the OpenShift e2e; unknown means Always.
check 'pull policy: an unclassified tag is Always' 'Always' "$(llmsm_image_pull_policy ref-0123abcd '')"

# ----------------------------------------------------------------------------
# Floating-tag warning
# ----------------------------------------------------------------------------
warned() { llmsm_warn_floating_image "$@" 2>&1 | grep -c 'FLOATING tag' || true; }

check 'warning: main is announced'      '1' "$(warned ghcr.io/o/scaling-manager main '')"
check 'warning: latest is announced'    '1' "$(warned ghcr.io/o/scaling-manager latest '')"
check 'warning: a release tag is quiet' '0' "$(warned ghcr.io/o/scaling-manager v0.6.0 '')"
check 'warning: a digest is quiet'      '0' "$(warned ghcr.io/o/scaling-manager main sha256:abc)"

# ----------------------------------------------------------------------------
# Prometheus TLS: an unverified HTTPS connection is announced
# ----------------------------------------------------------------------------
tls_warned() { llmsm_warn_prometheus_tls "$1" 2>&1 | grep -c 'will NOT verify' || true; }

# The shipped Kubernetes config, read from the file rather than restated here.
SHIPPED_CFG="$(tr -d '\r' < "$ROOT/config/base/manager/manager-configmap.yaml" | yq '.data."config.yaml"')"
[ -n "$SHIPPED_CFG" ] || { printf 'FATAL: could not read the shipped manager config.\n' >&2; exit 1; }
# The shipped config verifies: a manifest applied without the installer sends
# its bearer token only to a Prometheus it has verified.
check 'tls: the shipped config verifies' 'false' \
    "$(printf '%s\n' "$SHIPPED_CFG" | yq -r '.PROMETHEUS_TLS_INSECURE_SKIP_VERIFY')"
# What the installer writes over it, and that its choice is announced.
# sentinel <cmd...> prints the command's output, `unset` when it printed
# nothing, and ERROR<rc> when it failed: an empty expectation also passed for a
# function that crashed.
sentinel() {
    local o rc=0
    o="$("$@")" || rc=$?
    if [ "$rc" -ne 0 ]; then printf 'ERROR%s' "$rc"; elif [ -z "$o" ]; then printf unset; else printf '%s' "$o"; fi
}
# skip_for <ENVIRONMENT> <URL> <explicit skip> [DEPLOY_PROMETHEUS] [PROMETHEUS_URL_EXPLICIT];
# the installer's own stack lives in MONITORING_NAMESPACE=our-mon.
skip_for() {
    ( ENVIRONMENT="$1" PROMETHEUS_URL="$2" PROMETHEUS_TLS_INSECURE_SKIP_VERIFY="$3" \
        DEPLOY_PROMETHEUS="${4:-true}" PROMETHEUS_URL_EXPLICIT="${5:-}" MONITORING_NAMESPACE=our-mon \
        sentinel llmsm_prometheus_skip_verify )
}
OWN_URL='https://kube-prometheus-stack-prometheus.our-mon.svc.cluster.local:9090'
k8s_skip="$(skip_for kubernetes "$OWN_URL" '')"
check 'tls: the stack this install deploys, no CA to give: verification off' 'true' "$k8s_skip"
check 'tls: the same stack, found again by a re-run, stays off' 'true' \
    "$(skip_for kubernetes 'https://prometheus-operated.our-mon.svc.cluster.local:9090' '')"
# Was 'true' for ANY https URL: the controller then sent its token, unverified,
# to a Prometheus nobody had vouched for.
check 'tls: any other https Prometheus keeps verification on' 'unset' "$(skip_for kubernetes https://prom.obs.svc:9090 '')"
check 'tls: an external https Prometheus keeps verification on' 'unset' "$(skip_for kubernetes https://prom.example.com:9090 '')"
check 'tls: DEPLOY_PROMETHEUS=false keeps verification on, even in MONITORING_NAMESPACE' 'unset' \
    "$(skip_for kubernetes "$OWN_URL" '' false)"
check 'tls: OpenShift keeps the shipped verification (service CA)' 'unset' "$(skip_for openshift https://t:9091 '')"
check 'tls: an explicit choice wins' 'false' "$(skip_for kubernetes "$OWN_URL" false)"
check 'tls: an explicit true is honoured for any Prometheus' 'true' "$(skip_for kubernetes https://prom.obs.svc:9090 true)"
# Own stack is the Service this install deploys, not "anything in
# MONITORING_NAMESPACE": a Thanos or a second Prometheus there is somebody
# else's, and was sent the token unverified.
check 'tls: a Thanos in MONITORING_NAMESPACE is not the own stack: verified' 'unset' \
    "$(skip_for kubernetes 'https://thanos-querier.our-mon.svc:9091' '')"
check 'tls: another Prometheus in MONITORING_NAMESPACE is not the own stack: verified' 'unset' \
    "$(skip_for kubernetes 'https://team-prom.our-mon.svc.cluster.local:9090' '')"
check 'tls: prometheus-operated named by the caller is not assumed to be ours: verified' 'unset' \
    "$(skip_for kubernetes 'https://prometheus-operated.our-mon.svc.cluster.local:9090' '' true \
        'https://prometheus-operated.our-mon.svc.cluster.local:9090')"
check 'tls: the default Service named by the caller is still the own stack' 'true' \
    "$(skip_for kubernetes "$OWN_URL" '' true "$OWN_URL")"
# The host is what the URL connects to. With userinfo, the stack name sits
# before the "@" and the connection goes after it: these were judged the own
# stack, and the token went to evil.example.com unverified.
check 'tls: userinfo naming the stack is not the own stack (port-like)' 'unset' \
    "$(skip_for kubernetes 'https://kube-prometheus-stack-prometheus.our-mon.svc:443@evil.example.com/' '')"
check 'tls: userinfo naming the stack is not the own stack (user:password)' 'unset' \
    "$(skip_for kubernetes 'https://kube-prometheus-stack-prometheus.our-mon.svc:x@evil.example.com:9090' '')"
check 'tls: the stack name in the path is not the own stack' 'unset' \
    "$(skip_for kubernetes 'https://evil.example.com/kube-prometheus-stack-prometheus.our-mon.svc.cluster.local:9090' '')"
check 'tls: a query or fragment is not the own stack' 'unset|unset' \
    "$(skip_for kubernetes "$OWN_URL?x=1" '')|$(skip_for kubernetes "$OWN_URL#f" '')"

# An UPGRADE. Earlier releases shipped verification off for every Prometheus,
# and the installer re-renders the whole config.yaml, so a controller that
# connected unverified, with no CA, had verification turned on by the upgrade
# and could no longer connect. The install still ended "complete".
# skip_live <live state> <live skip> <live CA> <URL> [explicit] [live URL]
# prints <value>|<kept warnings>|<other-Prometheus warnings>. The live URL
# defaults to <URL>: the same Prometheus as before.
skip_live() {
    (
        LLMSM_LIVE_PROM_TLS="$1" LLMSM_LIVE_SKIP_VERIFY="$2" LLMSM_LIVE_CA_PATH="$3" LLMSM_LIVE_PROM_URL="${6-$4}"
        LLMSM_LIVE_PROM_TLS_ERR='Error from server (Forbidden)'
        ENVIRONMENT=kubernetes PROMETHEUS_URL="$4" PROMETHEUS_TLS_INSECURE_SKIP_VERIFY="${5:-}"
        DEPLOY_PROMETHEUS=true MONITORING_NAMESPACE=our-mon PROMETHEUS_CA_CERT_PATH='' LLMSM_NS=wns
        out="$(llmsm_warn_prometheus_tls_upgrade 2>&1)"
        printf '%s|%s|%s' "$(sentinel llmsm_prometheus_skip_verify)" \
            "$(printf '%s\n' "$out" | grep -c 'stays OFF, as it was before this upgrade')" \
            "$(printf '%s\n' "$out" | grep -c 'This install points at')"
    )
}
EXT_URL='https://prom.obs.svc:9090'
check 'tls upgrade: live true, no CA: kept off, and said so' 'true|1|0' "$(skip_live read true '' "$EXT_URL")"
check 'tls upgrade: the warning names both ways to turn it on' '2' \
    "$( (LLMSM_LIVE_PROM_TLS=read LLMSM_LIVE_SKIP_VERIFY=true LLMSM_LIVE_CA_PATH='' LLMSM_LIVE_PROM_URL="$EXT_URL"
         PROMETHEUS_TLS_INSECURE_SKIP_VERIFY='' PROMETHEUS_CA_CERT_PATH=''
         LLMSM_NS=wns ENVIRONMENT=kubernetes PROMETHEUS_URL="$EXT_URL" DEPLOY_PROMETHEUS=true MONITORING_NAMESPACE=our-mon
         llmsm_warn_prometheus_tls_upgrade 2>&1 | grep -cE 'PROMETHEUS_CA_CERT_PATH=<file>|PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=false') )"
check 'tls upgrade: live false stays verified, quietly' 'unset|0|0' "$(skip_live read false '' "$EXT_URL")"
# A CA path kept from the live config keeps the live skip value. Was 'false':
# a controller that ran with a CA path and verification OFF had verification
# turned on by the upgrade, unannounced -- the change the upgrade rule exists
# to prevent. Only an explicit CA path means verify.
check 'tls upgrade: live true with a CA path stays off, as it was' 'true|0|0' \
    "$(skip_live read true /etc/ca/ca.crt "$EXT_URL")"
check 'tls upgrade: an explicit CA path verifies even where live skipped' 'false' \
    "$( (LLMSM_LIVE_PROM_TLS=read LLMSM_LIVE_SKIP_VERIFY=true LLMSM_LIVE_CA_PATH=/etc/ca/ca.crt LLMSM_LIVE_PROM_URL="$EXT_URL"
         ENVIRONMENT=kubernetes PROMETHEUS_URL="$EXT_URL" PROMETHEUS_TLS_INSECURE_SKIP_VERIFY='' PROMETHEUS_CA_CERT_PATH=/etc/pki/p.pem
         DEPLOY_PROMETHEUS=true MONITORING_NAMESPACE=our-mon sentinel llmsm_prometheus_skip_verify) )"
# The same Prometheus with a trailing slash is still the same one. Was: another
# Prometheus, so verification was turned on and the controller lost it.
check 'tls upgrade: a trailing slash is the same Prometheus: kept off' 'true|1|0' \
    "$(skip_live read true '' "$EXT_URL" '' "$EXT_URL/")"
check 'tls upgrade: a new install verifies, quietly' 'unset|0|0' "$(skip_live absent '' '' "$EXT_URL")"
check 'tls upgrade: an explicit false wins over live true, quietly' 'false|0|0' "$(skip_live read true '' "$EXT_URL" false)"
check 'tls upgrade: plain http takes no TLS setting' 'unset|0|0' "$(skip_live read true '' http://prom.obs.svc:9090)"
check 'tls upgrade: the stack this install deploys stays off, no upgrade warning' 'true|0|0' \
    "$(skip_live read false '' 'https://prometheus-operated.our-mon.svc.cluster.local:9090')"
# Kept off only for the SAME Prometheus. Pointed at another one, what was true
# of the old one says nothing: the new default, and the operator is told.
check 'tls upgrade: live true for ANOTHER Prometheus: verified, and said so' 'unset|0|1' \
    "$(skip_live read true '' "$EXT_URL" '' 'https://old-prom.obs.svc:9090')"
check 'tls upgrade: live true, no live URL recorded: verified, and said so' 'unset|0|1' \
    "$(skip_live read true '' "$EXT_URL" '' '')"
# A live CA path is not carried to another Prometheus, and the operator is told,
# with the re-run that puts it back. Was: carried, in silence.
# ca_dropped <live URL> [explicit CA] counts that re-run in the upgrade warning.
ca_dropped() {
    ( LLMSM_LIVE_PROM_TLS=read LLMSM_LIVE_SKIP_VERIFY=false LLMSM_LIVE_CA_PATH=/etc/ca/ca.crt LLMSM_LIVE_PROM_URL="$1"
      ENVIRONMENT=kubernetes PROMETHEUS_URL="$EXT_URL" PROMETHEUS_TLS_INSECURE_SKIP_VERIFY='' PROMETHEUS_CA_CERT_PATH="${2:-}"
      DEPLOY_PROMETHEUS=true MONITORING_NAMESPACE=our-mon LLMSM_NS=wns LLMSM_SCOPE=namespace INSTALL_PHASE_EXPLICIT=''
      llmsm_warn_prometheus_tls_upgrade 2>&1 | grep -c 'PROMETHEUS_CA_CERT_PATH=/etc/ca/ca.crt NAMESPACE=wns make deploy-scaling-manager' || true )
}
check 'tls upgrade: a live CA dropped for another Prometheus is said, with the re-run' '1' \
    "$(ca_dropped 'https://old-prom.obs.svc:9090')"
check 'tls upgrade: the same Prometheus keeps its CA, quietly' '0' "$(ca_dropped "$EXT_URL")"
check 'tls upgrade: an explicit CA path is not a drop' '0' "$(ca_dropped 'https://old-prom.obs.svc:9090' /etc/pki/p.pem)"

# The CA path. The re-render replaces config.yaml with the shipped one, so a CA
# the controller runs with was dropped by every re-run: verify-with-CA became
# verify-against-public-roots, and a private-CA Prometheus refused it.
# render_tls <live skip> <live CA> <URL> [explicit CA] [live URL] prints the
# rendered <CA>|<skip> from the ConfigMap patch the first apply uses. The live
# URL defaults to <URL>.
render_tls() {
    (
        R="$(mktemp -d)"
        : > "$R/kustomization.yaml"
        LLMSM_PROJECT="$ROOT" LLMSM_LIVE_PROM_TLS=read LLMSM_LIVE_SKIP_VERIFY="$1" LLMSM_LIVE_CA_PATH="$2"
        LLMSM_LIVE_PROM_URL="${5-$3}" ENVIRONMENT=kubernetes PROMETHEUS_URL="$3"
        PROMETHEUS_TLS_INSECURE_SKIP_VERIFY='' PROMETHEUS_CA_CERT_PATH="${4:-}"
        DEPLOY_PROMETHEUS=true MONITORING_NAMESPACE=our-mon PROMETHEUS_URL_EXPLICIT=''
        llmsm_render_manager_config_patch "$R" >/dev/null 2>&1
        tr -d '\r' < "$R/manager-config-patch.yaml" 2>/dev/null | yq '.data."config.yaml"' \
            | yq -r '(.PROMETHEUS_CA_CERT_PATH // "") + "|" + (.PROMETHEUS_TLS_INSECURE_SKIP_VERIFY // "")'
        rm -rf "$R"
    )
}
check 'tls render: live verify + CA, external Prometheus: a re-run keeps the CA and verifies' \
    '/etc/ca/ca.crt|false' "$(render_tls false /etc/ca/ca.crt "$EXT_URL")"
# Kept from live, the CA keeps the live skip value too. Was '|false': turned on.
check 'tls render: the own stack with a live CA keeps the CA and the live skip' \
    '/etc/ca/ca.crt|true' "$(render_tls true /etc/ca/ca.crt "$OWN_URL")"
check 'tls render: the own stack with a live CA and verification on keeps both' \
    '/etc/ca/ca.crt|false' "$(render_tls false /etc/ca/ca.crt "$OWN_URL")"
# Another Prometheus may have another CA: the live path is not carried to it.
# Was '/etc/ca/ca.crt|false' -- the old CA, pointed at a new Prometheus.
check 'tls render: a live CA is dropped when the Prometheus changes' \
    '|false' "$(render_tls false /etc/ca/ca.crt "$EXT_URL" '' 'https://old-prom.obs.svc:9090')"
check 'tls render: a trailing slash does not drop the live CA' \
    '/etc/ca/ca.crt|false' "$(render_tls false /etc/ca/ca.crt "$EXT_URL" '' "$EXT_URL/")"
check 'tls render: an explicit PROMETHEUS_CA_CERT_PATH wins over the live one' \
    '/etc/pki/prom.pem|false' "$(render_tls false /etc/ca/ca.crt "$EXT_URL" /etc/pki/prom.pem)"
check 'tls render: no CA anywhere writes none' '|false' "$(render_tls false '' "$EXT_URL")"
check 'tls: OpenShift never takes a CA path from the installer (its overlay sets the service CA)' 'unset' \
    "$( (ENVIRONMENT=openshift PROMETHEUS_URL=https://t:9091 PROMETHEUS_CA_CERT_PATH=/x LLMSM_LIVE_PROM_TLS=read LLMSM_LIVE_CA_PATH=/y
         sentinel llmsm_prometheus_ca_path) )"
# The own stack, rendered: verification off, and on for any other Prometheus.
check 'tls render: the own stack renders skip true' '|true' "$(render_tls false '' "$OWN_URL")"
# OpenShift takes no installer TLS choice, even with a live "true" -- for an
# external URL or one that looks like the own stack.
check 'tls: OpenShift with live true and an external URL writes nothing' 'unset' \
    "$( (ENVIRONMENT=openshift PROMETHEUS_URL="$EXT_URL" LLMSM_LIVE_PROM_TLS=read LLMSM_LIVE_SKIP_VERIFY=true LLMSM_LIVE_CA_PATH=''
         LLMSM_LIVE_PROM_URL="$EXT_URL" PROMETHEUS_TLS_INSECURE_SKIP_VERIFY='' DEPLOY_PROMETHEUS=true MONITORING_NAMESPACE=our-mon
         sentinel llmsm_prometheus_skip_verify) )"
check 'tls: OpenShift with live true and the own-stack URL writes nothing' 'unset' \
    "$( (ENVIRONMENT=openshift PROMETHEUS_URL="$OWN_URL" LLMSM_LIVE_PROM_TLS=read LLMSM_LIVE_SKIP_VERIFY=true LLMSM_LIVE_CA_PATH=''
         LLMSM_LIVE_PROM_URL="$OWN_URL" PROMETHEUS_TLS_INSECURE_SKIP_VERIFY='' DEPLOY_PROMETHEUS=true MONITORING_NAMESPACE=our-mon
         sentinel llmsm_prometheus_skip_verify) )"
check 'tls upgrade: the own stack with live true stays off, no upgrade warning' 'true|0|0' \
    "$(skip_live read true '' "$OWN_URL")"

# The write path after the apply: what it patches into the live config. A stub
# kubectl answers the read-back; patch_manager_config records each expression.
write_tls() { # write_tls <live skip> <live CA> <URL>: prints <skip>;<CA> as patched, then the warnings seen
    (
        P="$(mktemp)"
        printf '%s\n' "$SHIPPED_CFG" > "$P"
        # Each patch is APPLIED, with the environment the caller gave it, so a
        # value passed through strenv is checked as well as a spliced one.
        patch_manager_config() { local c; c="$(yq "$1" "$P" 2>/dev/null)" && printf '%s\n' "$c" > "$P"; }
        kubectl() { printf '%s\n' "$SHIPPED_CFG"; }
        LLMSM_NS=wns LLMSM_SCOPE=namespace INSTALL_PHASE_EXPLICIT='' ENVIRONMENT=kubernetes
        LLMSM_LIVE_PROM_TLS=read LLMSM_LIVE_SKIP_VERIFY="$1" LLMSM_LIVE_CA_PATH="$2" LLMSM_LIVE_PROM_URL="$3"
        PROMETHEUS_URL="$3" PROMETHEUS_URL_EXPLICIT="$3" PROMETHEUS_TLS_INSECURE_SKIP_VERIFY='' PROMETHEUS_CA_CERT_PATH=''
        DEPLOY_PROMETHEUS=true MONITORING_NAMESPACE=our-mon
        out="$(llmsm_write_prometheus_settings 2>&1)"
        printf '%s;%s\n' "$(yq -r '.PROMETHEUS_TLS_INSECURE_SKIP_VERIFY // ""' "$P")" "$(yq -r '.PROMETHEUS_CA_CERT_PATH // ""' "$P")"
        printf '%s\n' "$out" | grep -c 'stays OFF, as it was before this upgrade'
        [ -z "${WRITE_TLS_URL_OUT:-}" ] || yq -r '.PROMETHEUS_BASE_URL' "$P" > "$WRITE_TLS_URL_OUT"
        rm -f "$P"
    )
}
check 'tls write: live true, no CA, same Prometheus: patches true and says it stays off' \
    "$(printf '%s\n%s' 'true;' 1)" "$(write_tls true '' "$EXT_URL")"
check 'tls write: live CA: patches verify and the CA path' \
    "$(printf '%s\n%s' 'false;/etc/ca/ca.crt' 0)" \
    "$(write_tls false /etc/ca/ca.crt "$EXT_URL")"
# The URL goes in through strenv. Spliced into the program text, a quote in it
# was yq syntax: the patch failed, or did what the URL said.
QUOTE_URL='https://prom.obs.svc:9090/x" | .PROMETHEUS_TLS_INSECURE_SKIP_VERIFY = "true'
WRITE_TLS_URL_OUT="$WORK/url.out" write_tls false '' "$QUOTE_URL" >/dev/null
check 'tls write: a quote in the URL is data, not yq' "$QUOTE_URL" "$(cat "$WORK/url.out" 2>/dev/null)"
check 'tls render: a quote in the URL is data, not yq' "$QUOTE_URL|false" \
    "$( (R="$(mktemp -d)"; : > "$R/kustomization.yaml"
         LLMSM_PROJECT="$ROOT" LLMSM_LIVE_PROM_TLS=absent ENVIRONMENT=kubernetes PROMETHEUS_URL="$QUOTE_URL"
         PROMETHEUS_TLS_INSECURE_SKIP_VERIFY='' PROMETHEUS_CA_CERT_PATH='' DEPLOY_PROMETHEUS=true MONITORING_NAMESPACE=our-mon
         PROMETHEUS_URL_EXPLICIT="$QUOTE_URL"
         llmsm_render_manager_config_patch "$R" >/dev/null 2>&1
         tr -d '\r' < "$R/manager-config-patch.yaml" 2>/dev/null | yq '.data."config.yaml"' \
             | yq -r '.PROMETHEUS_BASE_URL + "|" + (.PROMETHEUS_TLS_INSECURE_SKIP_VERIFY // "")'
         rm -rf "$R") )"

# Unreadable is unknown, never a new install, and the install cannot choose for
# the operator: it stops before anything is written. Was: verify, with a warning.
# refuse <live state> <URL> [explicit skip] [explicit CA] prints <exit>|<refusals>.
# The URL is passed as the caller's, so nothing is detected.
refuse() {
    local out rc=0
    out="$( (LLMSM_LIVE_PROM_TLS="$1" LLMSM_LIVE_PROM_TLS_ERR='Error from server (Forbidden)'
             ENVIRONMENT=kubernetes PROMETHEUS_URL="$2" PROMETHEUS_URL_EXPLICIT="$2" PROMETHEUS_TLS_INSECURE_SKIP_VERIFY="${3:-}"
             PROMETHEUS_CA_CERT_PATH="${4:-}" DEPLOY_PROMETHEUS=true MONITORING_NAMESPACE=our-mon
             LLMSM_NS=wns LLMSM_SCOPE=namespace INSTALL_PHASE_EXPLICIT=''
             llmsm_refuse_unknown_prometheus_tls) 2>&1)" || rc=$?
    printf '%s|%s' "$rc" "$(printf '%s\n' "$out" | grep -c 'PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=\(true\|false\) NAMESPACE=wns make deploy-scaling-manager')"
}
check 'tls refuse: live config unreadable: stops, naming both re-runs' '1|2' "$(refuse unknown "$EXT_URL")"
check 'tls refuse: an explicit choice is not refused' '0|0' "$(refuse unknown "$EXT_URL" true)"
# An explicit CA path means verify: the caller decided. Was refused.
check 'tls refuse: an explicit CA path is not refused' '0|0' "$(refuse unknown "$EXT_URL" '' /etc/pki/p.pem)"
# The Prometheus this install deploys gets the same setting whatever the live
# config held. Was refused.
check 'tls refuse: the stack this install deploys is not refused' '0|0' "$(refuse unknown "$OWN_URL")"
check 'tls refuse: plain http is not refused' '0|0' "$(refuse unknown http://prom.obs.svc:9090)"
check 'tls refuse: a readable config is not refused' '0|0' "$(refuse read "$EXT_URL")"

# What llmsm_read_live_prometheus_tls makes of the API answer. A stub kubectl
# prints $LIVE_CM, or a NotFound/Forbidden error on stderr like the API server.
live_read() {
    (
        LIVE_CM="$1"
        kubectl() {
            case "$LIVE_CM" in
                NOTFOUND) echo 'Error from server (NotFound): configmaps "scaling-manager-manager-config" not found' >&2; return 1 ;;
                FORBIDDEN) echo 'Error from server (Forbidden): configmaps "scaling-manager-manager-config" is forbidden' >&2; return 1 ;;
                *) printf '%s' "$LIVE_CM" ;;
            esac
        }
        llmsm_read_live_prometheus_tls wns
        printf '%s|%s|%s' "$LLMSM_LIVE_PROM_TLS" "$LLMSM_LIVE_SKIP_VERIFY" "$LLMSM_LIVE_CA_PATH"
    )
}
live_cm() { jq -cn --arg c "$1" '{data:{"config.yaml":$c}}'; }
check 'live config: NotFound is a new install' 'absent||' "$(live_read NOTFOUND)"
check 'live config: Forbidden is unknown, never absent' 'unknown||' "$(live_read FORBIDDEN)"
check 'live config: verification off is read' 'read|true|' \
    "$(live_read "$(live_cm "$(printf '%s\n' "$SHIPPED_CFG" | yq '.PROMETHEUS_TLS_INSECURE_SKIP_VERIFY = "true"')")")"
check 'live config: a CA path is read' 'read|true|/etc/ca/ca.crt' \
    "$(live_read "$(live_cm "$(printf '%s\n' "$SHIPPED_CFG" | yq '.PROMETHEUS_TLS_INSECURE_SKIP_VERIFY = "true" | .PROMETHEUS_CA_CERT_PATH = "/etc/ca/ca.crt"')")")"
# Every PROMETHEUS_* key is kept, to tell after the install whether one changed.
check 'live config: the PROMETHEUS_* keys are kept for the restart decision' 'PROMETHEUS_TLS_INSECURE_SKIP_VERIFY = true' \
    "$( (kubectl() { live_cm "$(printf '%s\n' "$SHIPPED_CFG" | yq '.PROMETHEUS_TLS_INSECURE_SKIP_VERIFY = "true"')"; }
         llmsm_read_live_prometheus_tls wns; printf '%s\n' "$LLMSM_LIVE_PROM_KEYS" | grep -x 'PROMETHEUS_TLS_INSECURE_SKIP_VERIFY = true') )"
check 'live config: no config.yaml is unknown' 'unknown||' "$(live_read '{"data":{}}')"
check 'live config: a config.yaml that is not YAML is unknown' 'unknown||' "$(live_read "$(live_cm 'a: [unclosed')")"
check 'live config: an answer that is not JSON is unknown' 'unknown||' "$(live_read 'not json {')"
INSTALLED_CFG="$(printf '%s\n' "$SHIPPED_CFG" | yq ".PROMETHEUS_TLS_INSECURE_SKIP_VERIFY = \"$k8s_skip\"")"
check 'tls: the installer choice on Kubernetes is warned about' '1' "$(tls_warned "$INSTALLED_CFG")"
# A CA path does not silence it while verification is off: a CA kept from live
# keeps the live skip value, and the CA then reads as though it verifies. Was 0.
check 'tls: a CA path with verification off is still warned about' '1' \
    "$(tls_warned "$(printf '%s\n' "$INSTALLED_CFG" | yq '.PROMETHEUS_CA_CERT_PATH = "/etc/ca/ca.crt"')")"
check 'tls: verification on is quiet' '0' \
    "$(tls_warned "$(printf '%s\n' "$SHIPPED_CFG" | yq '.PROMETHEUS_TLS_INSECURE_SKIP_VERIFY = "false"')")"
check 'tls: plain HTTP is not this warning' '0' \
    "$(tls_warned "$(printf 'PROMETHEUS_BASE_URL: "http://p:9090"\nPROMETHEUS_TLS_INSECURE_SKIP_VERIFY: "true"\n')")"
# Verified with no CA: a private-CA certificate fails to connect, so both fixes
# are named. Not on OpenShift, whose overlay mounts the service CA.
ca_warned() { LLMSM_NS=wns llmsm_warn_prometheus_tls "$1" 2>&1 | grep -cE 'PROMETHEUS_CA_CERT_PATH=<file>|PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true' || true; }
VERIFIED_CFG="$(printf '%s\n' "$SHIPPED_CFG" | yq '.PROMETHEUS_BASE_URL = "https://prom.obs.svc:9090" | .PROMETHEUS_TLS_INSECURE_SKIP_VERIFY = "false"')"
check 'tls: verified with no CA names both fixes' '2' "$( (ENVIRONMENT=kubernetes; ca_warned "$VERIFIED_CFG") )"
check 'tls: verified with a CA path is quiet' '0' \
    "$( (ENVIRONMENT=kubernetes; ca_warned "$(printf '%s\n' "$VERIFIED_CFG" | yq '.PROMETHEUS_CA_CERT_PATH = "/etc/ca/ca.crt"')") )"
check 'tls: OpenShift is never warned (service CA)' '0' "$( (ENVIRONMENT=openshift; ca_warned "$VERIFIED_CFG") )"
# "Re-run" with no command was the whole remedy. The warning now says how to
# tell whether there is a problem, and the exact command for this install.
tls_hint() {
    ( ENVIRONMENT=kubernetes LLMSM_NS=wns LLMSM_SCOPE=namespace INSTALL_PHASE_EXPLICIT="${2:-}"
      llmsm_warn_prometheus_tls "$1" 2>&1 \
        | grep -cE 'kubectl logs -n wns deploy/scaling-manager-controller-manager \| grep -m1 x509|PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true NAMESPACE=wns make deploy-scaling-manager|PROMETHEUS_CA_CERT_PATH=<file> NAMESPACE=wns make deploy-scaling-manager' || true )
}
check 'tls: the verify warning names the log check and both exact re-runs' '3' "$(tls_hint "$VERIFIED_CFG")"
check 'tls: the re-run keeps a controller-only phase' '1' \
    "$( (ENVIRONMENT=kubernetes LLMSM_NS=wns LLMSM_SCOPE=namespace INSTALL_PHASE_EXPLICIT=scaling-manager
         llmsm_warn_prometheus_tls "$VERIFIED_CFG" 2>&1 | grep -c 'INSTALL_PHASE=scaling-manager PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true NAMESPACE=wns make deploy-scaling-manager') )"
check 'tls: the unverified warning names the exact re-run with a CA' '1' \
    "$( (ENVIRONMENT=kubernetes LLMSM_NS=wns LLMSM_SCOPE=namespace INSTALL_PHASE_EXPLICIT=''
         llmsm_warn_prometheus_tls "$INSTALLED_CFG" 2>&1 | grep -c 'PROMETHEUS_CA_CERT_PATH=<file> NAMESPACE=wns make deploy-scaling-manager') )"
# A controller that did not come up: its last lines are printed, and an x509
# refusal among them now comes with the two fixes.
cert_hint() {
    ( LLMSM_NS=wns LLMSM_SCOPE=namespace INSTALL_PHASE_EXPLICIT=''
      llmsm_hint_untrusted_prometheus_cert "$1" 2>&1 | grep -cE 'NAMESPACE=wns make deploy-scaling-manager' || true )
}
check 'verify: an x509 failure in the container lines prints both fixes' '2' \
    "$(cert_hint 'CRITICAL: Failed to connect to Prometheus: tls: failed to verify certificate: x509: certificate signed by unknown authority')"
check 'verify: the unknown-authority wording alone is matched' '2' "$(cert_hint 'certificate signed by unknown authority')"
check 'verify: another failure prints no TLS fixes' '0' "$(cert_hint 'unknown flag: --external-scaler-bind-address')"
# OpenShift takes no TLS settings from the installer: neither re-run changes it.
check 'verify: no TLS re-runs on OpenShift' '0' \
    "$( (ENVIRONMENT=openshift; cert_hint 'x509: certificate signed by unknown authority') )"

# A printed re-run repeats THIS install. It carried only the namespace shape and
# the TLS variable, so pasting it detected another Prometheus, installed
# kube-prometheus-stack, or swapped the image.
# rerun <VAR=value...> runs llmsm_rerun_cmd PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true
# with those set over a plain namespace install of the default image.
rerun() {
    ( ENVIRONMENT=kubernetes ENVIRONMENT_EXPLICIT='' PROMETHEUS_URL_EXPLICIT='' DEPLOY_PROMETHEUS=true
      LLMSM_IMAGE_REPO=ghcr.io/o/w LLMSM_IMAGE_TAG=main LLMSM_IMAGE_DIGEST='' LLMSM_DEFAULT_IMAGE_REF=ghcr.io/o/w:main
      LLMSM_NS=wns LLMSM_SCOPE=namespace INSTALL_PHASE_EXPLICIT=''
      local kv
      for kv in "$@"; do eval "$kv"; done
      llmsm_rerun_cmd PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true )
}
check 'rerun: a plain install adds nothing' \
    'PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true NAMESPACE=wns make deploy-scaling-manager' "$(rerun)"
check 'rerun: carries an explicit ENVIRONMENT, PROMETHEUS_URL, DEPLOY_PROMETHEUS=false and the image' \
    'ENVIRONMENT=kubernetes IMG=reg.local:5000/w:dev PROMETHEUS_URL=https://p.obs.svc:9090 DEPLOY_PROMETHEUS=false PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true NAMESPACE=wns make deploy-scaling-manager' \
    "$(rerun ENVIRONMENT_EXPLICIT=kubernetes LLMSM_IMAGE_REPO=reg.local:5000/w LLMSM_IMAGE_TAG=dev \
        PROMETHEUS_URL_EXPLICIT=https://p.obs.svc:9090 DEPLOY_PROMETHEUS=false)"
check 'rerun: a digest is carried as the digest' \
    'IMG=ghcr.io/o/w@sha256:abc PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true NAMESPACE=wns make deploy-scaling-manager' \
    "$(rerun LLMSM_IMAGE_DIGEST=sha256:abc)"
check 'rerun: the variables come before a controller-only phase' \
    'PROMETHEUS_URL=https://p.obs.svc:9090 INSTALL_PHASE=scaling-manager PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true NAMESPACE=wns make deploy-scaling-manager' \
    "$(rerun PROMETHEUS_URL_EXPLICIT=https://p.obs.svc:9090 INSTALL_PHASE_EXPLICIT=scaling-manager)"
check 'rerun: the x509 hint names PROMETHEUS_URL in both fixes' '2' \
    "$( (ENVIRONMENT=kubernetes LLMSM_NS=wns LLMSM_SCOPE=namespace INSTALL_PHASE_EXPLICIT='' PROMETHEUS_URL_EXPLICIT=https://p.obs.svc:9090
         llmsm_hint_untrusted_prometheus_cert 'x509: certificate signed by unknown authority' 2>&1 \
         | grep -c 'PROMETHEUS_URL=https://p.obs.svc:9090 PROMETHEUS_\(CA_CERT_PATH\|TLS_INSECURE_SKIP_VERIFY\)=[^ ]* NAMESPACE=wns make deploy-scaling-manager') )"

# ... and the summary line says so too.
check 'tls: a CA path with verification off leaves a summary line naming the CA' '1' \
    "$( (ENVIRONMENT=kubernetes LLMSM_NS=wns
         llmsm_warn_prometheus_tls "$(printf '%s\n' "$VERIFIED_CFG" | yq '.PROMETHEUS_TLS_INSECURE_SKIP_VERIFY = "true" | .PROMETHEUS_CA_CERT_PATH = "/etc/ca/ca.crt"')" >/dev/null 2>&1
         printf '%s\n' "${LLMSM_TLS_SUMMARY:-}" | grep -c 'although a CA path (/etc/ca/ca.crt) is set') )"

# The controller reads its config once, at startup (subPath mount). A changed
# PROMETHEUS_* key must restart it, or the fix the install reports never runs.
# restarts <live state> <live config> <config now> counts rollout restarts.
restarts() {
    ( R="$(mktemp)"
      kubectl() { printf '%s\n' "$*" >> "$R"; }
      LLMSM_NS=wns LLMSM_LIVE_PROM_TLS="$1" LLMSM_LIVE_PROM_KEYS="$(llmsm_prometheus_keys "$2")"
      llmsm_restart_on_prometheus_change "$3" >/dev/null 2>&1
      grep -c '^rollout restart deployment/scaling-manager-controller-manager -n wns$' "$R"
      rm -f "$R" )
}
SKIP_OFF_CFG="$(printf '%s\n' "$VERIFIED_CFG" | yq '.PROMETHEUS_TLS_INSECURE_SKIP_VERIFY = "true"')"
check 'restart: verification turned off restarts the controller' '1' "$(restarts read "$VERIFIED_CFG" "$SKIP_OFF_CFG")"
check 'restart: a CA path added restarts the controller' '1' \
    "$(restarts read "$VERIFIED_CFG" "$(printf '%s\n' "$VERIFIED_CFG" | yq '.PROMETHEUS_CA_CERT_PATH = "/etc/ca/ca.crt"')")"
check 'restart: nothing changed, no restart' '0' "$(restarts read "$VERIFIED_CFG" "$VERIFIED_CFG")"
check 'restart: a quoted and an unquoted true are not a change' '0' \
    "$(restarts read "$SKIP_OFF_CFG" "$(printf '%s\n' "$VERIFIED_CFG" | yq '.PROMETHEUS_TLS_INSECURE_SKIP_VERIFY = true')")"
check 'restart: another key changed, no restart' '0' \
    "$(restarts read "$VERIFIED_CFG" "$(printf '%s\n' "$VERIFIED_CFG" | yq '.LLMSM_SCALE_TO_ZERO = "true"')")"
check 'restart: a new install is not restarted' '0' "$(restarts absent '' "$VERIFIED_CFG")"
check 'restart: a live config that could not be read is restarted' '1' "$(restarts unknown '' "$VERIFIED_CFG")"
# The summary repeats it, since the warning scrolls past long before the end.
check 'tls: the warning leaves a line for the summary' 'set' \
    "$( (ENVIRONMENT=kubernetes LLMSM_NS=wns; llmsm_warn_prometheus_tls "$VERIFIED_CFG" >/dev/null 2>&1; [ -n "${LLMSM_TLS_SUMMARY:-}" ] && echo set) )"
check 'tls: a quiet config leaves no summary line' '' \
    "$( (ENVIRONMENT=kubernetes LLMSM_NS=wns LLMSM_TLS_SUMMARY=stale
         llmsm_warn_prometheus_tls "$(printf '%s\n' "$VERIFIED_CFG" | yq '.PROMETHEUS_CA_CERT_PATH = "/etc/ca/ca.crt"')" >/dev/null 2>&1
         printf '%s' "${LLMSM_TLS_SUMMARY:-}") )"

# ----------------------------------------------------------------------------
# Who may read the controller's /metrics: the Prometheus actually here
# ----------------------------------------------------------------------------
# A stub kubectl answering the Prometheus CR listing with $PROM_CRS
# ("<namespace> <serviceAccountName>" per line). It ignores -n on purpose: the
# function must itself keep only the namespace PROMETHEUS_URL addresses.
PROM_CRS=''
kubectl() { [ -z "$PROM_CRS" ] || printf '%s\n' "$PROM_CRS"; }
# scraper <CRs> <URL> <DEPLOY_PROMETHEUS> [PROMETHEUS_SA] [explicit URL]: the
# fifth argument is PROMETHEUS_URL_EXPLICIT, and defaults to the URL itself --
# the caller passed it. Pass '' for a URL the installer DETECTED.
scraper() {
    (
        PROM_CRS="$1" PROMETHEUS_URL="$2" DEPLOY_PROMETHEUS="$3" PROMETHEUS_SA="${4:-}"
        PROMETHEUS_URL_EXPLICIT="${5-$2}"
        MONITORING_NAMESPACE=our-mon
        llmsm_prometheus_scraper_sa 2>/dev/null
    )
}
OBS_URL='https://prometheus-operated.obs.svc.cluster.local:9090'
MON_URL='http://prom.mon.svc:9090'

check 'scraper: PROMETHEUS_SA wins' 'ns1 sa1' \
    "$(scraper 'mon prom' "$MON_URL" true ns1/sa1)"
check 'scraper: a malformed PROMETHEUS_SA grants nobody, not a detected CR' '' \
    "$(scraper 'mon prom' "$MON_URL" true just-a-name)"
check 'scraper: the CR in the namespace the URL names' 'obs obs-prom' \
    "$(scraper "$(printf 'mon prom\nobs obs-prom')" "$OBS_URL" false)"
# Was "otherwise the first CR": on a shared cluster, someone else's Prometheus.
check 'scraper: no URL namespace -> nobody, never the first CR' '' \
    "$(scraper "$(printf 'mon prom\nobs obs-prom')" 'https://prom.example:9090' false)"
check 'scraper: a CR only in another namespace is not chosen' '' \
    "$(scraper 'mon prom' "$OBS_URL" false)"
# Was granted `default`: every Pod in that namespace without its own account.
check 'scraper: a CR with no serviceAccountName (default) is not granted' '' \
    "$(scraper 'mon ' "$MON_URL" false)"
check 'scraper: CRs in that namespace disagreeing -> nobody' '' \
    "$(scraper "$(printf 'obs a\nobs b')" "$OBS_URL" false)"
check 'scraper: two CRs agreeing on one account' 'obs a' \
    "$(scraper "$(printf 'obs a\nobs a')" "$OBS_URL" false)"
# A DETECTED URL is whichever Prometheus the API listed first -- on a shared
# cluster someone else's -- so its CR never names the subject.
check 'scraper: a detected URL never decides the subject' '' \
    "$(scraper "$(printf 'mon prom\nobs obs-prom')" "$OBS_URL" false '' '')"
check 'scraper: no CR yet, our stack in MONITORING_NAMESPACE -> its account' \
    'our-mon kube-prometheus-stack-prometheus' \
    "$(scraper '' 'https://kube-prometheus-stack-prometheus.our-mon.svc.cluster.local:9090' true)"
check 'scraper: no CR, DEPLOY_PROMETHEUS, URL elsewhere -> nobody' '' \
    "$(scraper '' "$OBS_URL" true)"
check 'scraper: no CR and none deployed -> nobody' '' \
    "$(scraper '' 'https://prom.example:9090' false)"
check 'url namespace: service.namespace.svc' 'obs' "$(llmsm_prometheus_url_namespace "$OBS_URL")"
check 'url namespace: an external host has none' '' "$(llmsm_prometheus_url_namespace 'https://prom.example.com:9090')"

# The URL is resolved BEFORE the subject is chosen. Asked first, it read the
# namespace of the shipped default rather than of the Prometheus detected. And
# neither decides: the default's account is trusted only when this install
# deploys that stack (DEPLOY_PROMETHEUS=true), and a detected one never.
decision() {
    (
        PROM_CRS="$(printf 'obs obs-prom\nscaling-manager-monitoring kube-prometheus-stack-prometheus')"
        PROMETHEUS_URL='https://kube-prometheus-stack-prometheus.scaling-manager-monitoring.svc.cluster.local:9090'
        PROMETHEUS_URL_EXPLICIT='' LLMSM_PROMETHEUS_URL_RESOLVED='' PROMETHEUS_SA=''
        DEPLOY_PROMETHEUS=false
        llmsm_detect_prometheus_url() { printf '%s\n' "$OBS_URL"; }
        log_info() { :; }
        k="$(mktemp "$WORK/kust.XXXXXX")"
        llmsm_append_metrics_reader_decision "$k" wns >/dev/null 2>&1
        printf '%s' "$LLMSM_METRICS_READER_SCRAPER"
    )
}
check 'scraper: neither the detected URL nor the shipped default decides' '' "$(decision)"
# Where the order still matters: the stack this install deploys is recognised
# by the RESOLVED URL addressing MONITORING_NAMESPACE, not the shipped default's.
decision_ours() {
    (
        PROM_CRS=''
        PROMETHEUS_URL='https://kube-prometheus-stack-prometheus.scaling-manager-monitoring.svc.cluster.local:9090'
        PROMETHEUS_URL_EXPLICIT='' LLMSM_PROMETHEUS_URL_RESOLVED='' PROMETHEUS_SA=''
        DEPLOY_PROMETHEUS=true MONITORING_NAMESPACE=obs
        llmsm_detect_prometheus_url() { printf '%s\n' "$OBS_URL"; }
        log_info() { :; }
        k="$(mktemp "$WORK/kust.XXXXXX")"
        llmsm_append_metrics_reader_decision "$k" wns >/dev/null 2>&1
        printf '%s' "$LLMSM_METRICS_READER_SCRAPER"
    )
}
check 'scraper: the resolved URL, not the shipped default, finds the stack this install deploys' \
    'obs kube-prometheus-stack-prometheus' "$(decision_ours)"
unset -f kubectl

# ----------------------------------------------------------------------------
# A re-run that names no Prometheus KEEPS the metrics-reader binding
# ----------------------------------------------------------------------------
# "No trusted source answers" is what every upgrade sees whose caller did not
# repeat PROMETHEUS_SA, and the install DELETED the binding then: Prometheus
# stopped scraping the controller, with no error. llmsm_metrics_reader_binding is
# run against a stub that answers the binding read from $MR_CRB (a subject,
# NOTFOUND or FORBIDDEN, on stderr like the API server); every call is logged.
mr_run() {
    : > "$WORK/mr.calls"
    : > "$WORK/mr.out"
    (
        MR_CRB=NOTFOUND
        PROMETHEUS_URL='https://prom.example.com:9090' PROMETHEUS_URL_EXPLICIT=''
        LLMSM_PROMETHEUS_URL_RESOLVED='' PROMETHEUS_SA='' DEPLOY_PROMETHEUS=false
        LLMSM_NS=wns LLMSM_WATCH_NS='' LLMSM_SCOPE=namespace LLMSM_APPLY_SCOPE=all
        local kv
        for kv in "$@"; do eval "$kv"; done
        llmsm_detect_prometheus_url() { :; }
        log_info() { echo "INFO $1" >> "$WORK/mr.out"; }
        log_warning() { echo "WARN $1" >> "$WORK/mr.out"; }
        kubectl() {
            echo "$*" >> "$WORK/mr.calls"
            case "$*" in
                "get clusterrolebinding "*)
                    case "$MR_CRB" in
                        NOTFOUND) echo 'Error from server (NotFound): clusterrolebindings.rbac.authorization.k8s.io not found' >&2; return 1 ;;
                        FORBIDDEN) echo 'Error from server (Forbidden): clusterrolebindings.rbac.authorization.k8s.io is forbidden' >&2; return 1 ;;
                        *) printf '%s ' "$MR_CRB" ;;
                    esac ;;
                *) return 0 ;;
            esac
        }
        k="$WORK/mr.kust"
        printf 'patches:\n' > "$k"
        llmsm_metrics_reader_binding "$k" "$LLMSM_NS"
    )
}
mr_name="scaling-manager-metrics-reader-rolebinding-$(llmsm_ns_suffix wns)"
if declare -F llmsm_metrics_reader_binding >/dev/null; then
    mr_run MR_CRB=obs/old-prom
    check 'metrics-reader re-run, no source, binding present: nothing deleted' '0' \
        "$(grep -c '^delete ' "$WORK/mr.calls")"
    check 'metrics-reader re-run, no source, binding present: warned it was KEPT, by name' '1' \
        "$(grep -c "^WARN Kept ClusterRoleBinding $mr_name as it is (grants the controller's /metrics to: obs/old-prom)" "$WORK/mr.out")"
    check 'metrics-reader re-run, no source: the warning says how to repoint and how to remove' '2' \
        "$(grep -c 'PROMETHEUS_SA=<namespace>/<serviceaccount> NAMESPACE=wns make setup-prereqs$\|kubectl delete clusterrolebinding '"$mr_name"'$' "$WORK/mr.out")"
    mr_run MR_CRB=NOTFOUND
    check 'metrics-reader, no source, no binding: says none is granted, deletes nothing' '1 0' \
        "$(grep -c '^WARN No Prometheus is granted' "$WORK/mr.out") $(grep -c '^delete ' "$WORK/mr.calls")"
    mr_run MR_CRB=FORBIDDEN
    check 'metrics-reader, binding unreadable: unknown, left as is, nothing deleted' '1 0 0' \
        "$(grep -c 'is unknown; this install leaves it as it is' "$WORK/mr.out") $(grep -c '^WARN No Prometheus is granted' "$WORK/mr.out") $(grep -c '^delete ' "$WORK/mr.calls")"
    mr_run MR_CRB=obs/old-prom LLMSM_APPLY_SCOPE=controller
    check 'metrics-reader, controller-only phase: no read, no message, no patch' '0 0 1' \
        "$(grep -c . "$WORK/mr.calls") $(grep -c . "$WORK/mr.out") $(grep -c . "$WORK/mr.kust")"
    mr_run PROMETHEUS_SA=obs/prom-sa
    check 'metrics-reader, PROMETHEUS_SA: the binding is pointed at it' '1 1' \
        "$(grep -c '^INFO Controller /metrics readable by ServiceAccount prom-sa in obs' "$WORK/mr.out") $(grep -c 'name: prom-sa' "$WORK/mr.kust")"
    # The remedy repeats the install's shape: the out-of-reach controller in
    # llmsm-<ns> manages <ns>, and NAMESPACE=llmsm-<ns> would set up a different one.
    mr_run MR_CRB=obs/old-prom LLMSM_NS=scaling-manager-tenant LLMSM_WATCH_NS=tenant
    check 'metrics-reader remedy, out-of-reach shape: NAMESPACE is the managed one' '1 0' \
        "$(grep -c 'PROMETHEUS_SA=<namespace>/<serviceaccount> NAMESPACE=tenant make setup-prereqs LLMSM_NS=scaling-manager-tenant LLMSM_WATCH_NS=tenant$' "$WORK/mr.out") $(grep -c 'NAMESPACE=scaling-manager-tenant' "$WORK/mr.out")"
    mr_run MR_CRB=obs/old-prom LLMSM_SCOPE=cluster
    check 'metrics-reader remedy, cluster scope: SCOPE=cluster, no NAMESPACE' '1 0' \
        "$(grep -c 'PROMETHEUS_SA=<namespace>/<serviceaccount> make setup-prereqs SCOPE=cluster LLMSM_NS=wns$' "$WORK/mr.out") $(grep -c 'NAMESPACE=wns' "$WORK/mr.out")"
else
    check 'metrics-reader re-run: llmsm_metrics_reader_binding is defined' 'defined' 'missing'
fi
# And no other path in the installer deletes it.
check 'metrics-reader: the installer never deletes the binding' '0' \
    "$(tr -d '\r' < "$ROOT/deploy/lib/infra_scaling_manager.sh" | grep -v '^[[:space:]]*#' | grep -c 'delete clusterrolebinding .*scaling-manager-metrics-reader')"

# ----------------------------------------------------------------------------
# The shipped sample ScaledObject scales like the ones the installer writes
# ----------------------------------------------------------------------------
# The sample is what people copy. It had a 0s scale-down window at 100%/30s,
# so a copied sample shed replicas on a single low reading. (Before the scaling manager's first
# decision the scaler now answers the current replica count, so it no longer
# answers 0 there either.)
gen_behavior="$(
    unset LLMSM_SO_SCALE_UP_STABILIZATION LLMSM_SO_SCALE_UP_PERIOD \
          LLMSM_SO_SCALE_DOWN_STABILIZATION LLMSM_SO_SCALE_DOWN_PERIOD
    render_default_scaledobject ns Deployment t m addr:9090 1 10 '' '' \
        | yq -o=json -I=0 '.spec.advanced.horizontalPodAutoscalerConfig.behavior | sort_keys(..)'
)"
sample_behavior="$(tr -d '\r' < "$ROOT/config/samples/keda/external-scaler/scaledobject.yaml" \
    | yq -o=json -I=0 '.spec.advanced.horizontalPodAutoscalerConfig.behavior | sort_keys(..)')"
[ -n "$gen_behavior" ] && [ "$gen_behavior" != null ] || {
    printf 'FATAL: the generator rendered no behavior block.\n' >&2; exit 1; }
check 'sample ScaledObject: behavior equals the generator default' "$gen_behavior" "$sample_behavior"
# Named outright as well, so a generator that changed its own default to 0
# does not pass by agreement.
check 'sample ScaledObject: scale-down waits 300s' '300' \
    "$(printf '%s' "$sample_behavior" | yq -p=json '.scaleDown.stabilizationWindowSeconds')"

# ----------------------------------------------------------------------------

if [ "$FAILED" -gt 0 ]; then
    printf '\n%d install-default assertion(s) failed.\n' "$FAILED" >&2
    exit 1
fi
if [ "$RAN" -ne "$EXPECT_CASES" ]; then
    printf '\n%d case(s) reported ok, %d expected -- the run did not finish.\n' \
        "$RAN" "$EXPECT_CASES" >&2
    exit 1
fi
printf '\nInstall default checks passed (%d cases).\n' "$RAN"
