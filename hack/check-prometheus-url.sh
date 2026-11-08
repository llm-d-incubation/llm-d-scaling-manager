#!/usr/bin/env bash
#
# Executes the installer's Prometheus address selection and TLS verification
# against a stubbed kubectl and real certificates.
#
# A kube-prometheus-stack serves a certificate for its own Service
# (<release>-kube-prometheus-stack-prometheus), not for prometheus-operated.
# The installer picked prometheus-operated anyway, reported it as found, and
# the controller -- which verifies -- crash-looped on "x509: certificate is
# valid for ..., not prometheus-operated...". Every step of that parses and
# runs cleanly; only executing it against a certificate shows the wrong host.
#
# Needs jq, yq and openssl. Missing any is FATAL, never a skip.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1

for tool in jq yq openssl; do
    command -v "$tool" >/dev/null 2>&1 || { printf 'FATAL: %s is required.\n' "$tool" >&2; exit 1; }
done
if [ -n "${KUSTOMIZE:-}" ] && [ -x "$KUSTOMIZE" ]; then
    KBUILD=("$KUSTOMIZE" build)
elif [ -x bin/kustomize ]; then
    KBUILD=(bin/kustomize build)
elif command -v kustomize >/dev/null 2>&1; then
    KBUILD=(kustomize build)
elif command -v kubectl >/dev/null 2>&1; then
    KBUILD=(kubectl kustomize)
else
    printf 'FATAL: kustomize is required (make kustomize).\n' >&2
    exit 1
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
for lib in common.sh infra_scaling_manager.sh infra_monitoring.sh; do
    tr -d '\r' < "deploy/lib/$lib" > "$WORK/$lib"
done
BLUE='' GREEN='' YELLOW='' RED='' NC=''
# shellcheck source=/dev/null
source "$WORK/common.sh"
# shellcheck source=/dev/null
source "$WORK/infra_scaling_manager.sh"
# shellcheck source=/dev/null
source "$WORK/infra_monitoring.sh"

EXPECT_CASES=76
RAN=0
FAILED=0

check() {
    if [ "$2" = "$3" ]; then
        RAN=$((RAN + 1))
        printf 'ok    %s\n' "$1"
    else
        FAILED=$((FAILED + 1))
        printf 'FAIL  %s\n        want [%s]\n        got  [%s]\n' "$1" "$2" "$3" >&2
    fi
}

# has <name> <needle> <haystack>: a case that the text contains the needle.
has() {
    if printf '%s' "$3" | grep -qF -- "$2"; then check "$1" yes yes; else check "$1" yes "no: $(printf '%s' "$3" | tr '\n' '|' | cut -c1-600)"; fi
}

# lacks <name> <needle> <haystack>: a case that the text does not contain it.
lacks() {
    if printf '%s' "$3" | grep -qF -- "$2"; then check "$1" absent "present: $(printf '%s' "$3" | tr '\n' '|' | cut -c1-600)"; else check "$1" absent absent; fi
}

# ----------------------------------------------------------------------------
# Certificates: real ones, so the name parsing is the real parsing.
# ----------------------------------------------------------------------------
MON=llm-d-monitoring
CR=llmd-kube-prometheus-stack-prometheus
STACK="$CR.$MON.svc"
cert() {
    openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
        -keyout "$WORK/$1.key" -out "$WORK/$1.crt" -days 2 -subj "/CN=prometheus" \
        -addext "subjectAltName=$2" >/dev/null 2>&1 \
        || { printf 'FATAL: openssl could not make certificate %s.\n' "$1" >&2; exit 1; }
    base64 < "$WORK/$1.crt" | tr -d '\n' > "$WORK/$1.b64"
}
cert stack "DNS:$STACK,DNS:$CR,DNS:localhost,IP:127.0.0.1"
cert stackfqdn "DNS:$STACK.cluster.local,DNS:$STACK"
cert operated "DNS:prometheus-operated.$MON.svc.cluster.local"
cert wild "DNS:*.$MON.svc"
cert ext "DNS:prom.example.com"

# ----------------------------------------------------------------------------
# The stub: one JSON "world" per case, read by a kubectl that knows the calls
# the installer makes. Every call is logged to $WORK/calls.
# ----------------------------------------------------------------------------
# world <cert|none> [stack: new|old|none] [ref: secret|configmap] [forbid: json list] [tls: yes|no]
world() {
    local certname="$1" stack="${2:-new}" ref="${3:-secret}" forbid="${4:-[]}" tls="${5:-yes}" b64="" pem=""
    if [ "$certname" != none ]; then
        b64="$(cat "$WORK/$certname.b64")"
        pem="$(cat "$WORK/$certname.crt")"
    fi
    jq -n --arg mon "$MON" --arg cr "$CR" --arg stack "$stack" --arg ref "$ref" \
        --argjson forbid "$forbid" --arg tls "$tls" --arg b64 "$b64" --arg pem "$pem" '
        {forbid: $forbid,
         crs: [{ns: $mon, name: $cr, spec: (if $tls == "yes" then {web: {tlsConfig: {
                 cert: (if $ref == "secret" then {secret: {name: "prom-tls", key: "tls.crt"}}
                        else {configMap: {name: "prom-tls-cert", key: "tls.crt"}} end),
                 keySecret: {name: "prom-tls", key: "tls.key"}}}} else {} end)}],
         svcs: ([{ns: $mon, name: "prometheus-operated", selector: {"app.kubernetes.io/name": "prometheus"},
                  ports: [{name: "web", port: 9090}]},
                 {ns: $mon, name: "thanos-query", selector: {"app.kubernetes.io/name": "thanos-query"},
                  ports: [{name: "http", port: 10902}]}]
                + (if $stack == "new" then [{ns: $mon, name: $cr,
                      selector: {"app.kubernetes.io/name": "prometheus", "operator.prometheus.io/name": $cr},
                      ports: [{name: "reloader-web", port: 8080}, {name: "http-web", port: 9090}]}]
                   elif $stack == "old" then [{ns: $mon, name: $cr, selector: {prometheus: $cr},
                      ports: [{name: "web", port: 9090}]}]
                   else [] end)),
         objs: (if $b64 == "" then [] else
                [{kind: "secret", ns: $mon, name: "prom-tls", data: {"tls.crt": $b64, "tls.key": "UFJJVkFURQ=="}},
                 {kind: "configmap", ns: $mon, name: "prom-tls-cert", data: {"tls.crt": $pem}}] end)}' > "$WORK/world.json"
    : > "$WORK/calls"
    rm -f "$WORK/probe.json" "$WORK/applied"
}

# add_obj <json> adds a Secret or ConfigMap ({kind, ns, name, data, labels}) to the world.
add_obj() {
    jq --argjson o "$1" '.objs += [$o]' "$WORK/world.json" > "$WORK/world.next" && mv "$WORK/world.next" "$WORK/world.json"
}

# probe_world <phase> <exit code> <log> [waiting reason] sets what the probe Pod reports.
probe_world() {
    jq --arg phase "$1" --arg code "$2" --arg log "$3" --arg waiting "${4:-}" \
        '.probe = {phase: $phase, code: $code, log: $log, waiting: $waiting}' \
        "$WORK/world.json" > "$WORK/world.next" && mv "$WORK/world.next" "$WORK/world.json"
}

forbidden() {
    printf 'Error from server (Forbidden): %s is forbidden: User "tenant" cannot %s resource "%s"\n' "$1" "$2" "$1" >&2
    return 1
}

kubectl() {
    local W="$WORK/world.json" verb="$1" kind="" name="" ns="" all=0 out="" key obj
    printf '%s\n' "$*" >> "$WORK/calls"
    shift
    case "$verb" in
        api-resources|auth|delete) return 0 ;;
        logs) jq -r '.probe.log // ""' "$W"; return 0 ;;
        create)
            cat > "$WORK/probe.json"
            if jq -e '.forbid | index("pods")' "$W" >/dev/null; then forbidden pods create; return 1; fi
            return 0 ;;
        apply)
            cat >> "$WORK/applied"
            return 0 ;;
        get) ;;
        *) return 0 ;;
    esac
    kind="$1"
    shift
    while [ $# -gt 0 ]; do
        case "$1" in
            -n) ns="$2"; shift 2 ;;
            -A) all=1; shift ;;
            -o) out="$2"; shift 2 ;;
            -o*) out="${1#-o}"; shift ;;
            *) name="$1"; shift ;;
        esac
    done
    if jq -e --arg k "$kind" --arg kn "$kind:$ns" '.forbid | (index($k) or index($kn))' "$W" >/dev/null; then
        forbidden "$kind" get
        return 1
    fi
    case "$kind" in
        prometheuses.monitoring.coreos.com)
            if [ -z "$name" ]; then
                jq -r --arg ns "$ns" --argjson all "$all" \
                    '.crs[] | select($all == 1 or .ns == $ns) | if $all == 1 then .ns + " " + .name else .name end' "$W"
                return 0
            fi
            obj="$(jq -c --arg ns "$ns" --arg n "$name" '.crs[] | select(.ns == $ns and .name == $n)' "$W")"
            [ -n "$obj" ] || { echo "Error from server (NotFound): not found" >&2; return 1; }
            case "$out" in
                json) printf '%s' "$obj" | jq '{metadata: {name: .name, namespace: .ns}, spec: .spec}' ;;
                *tlsConfig*) printf '%s' "$obj" | jq -c '.spec.web.tlsConfig // empty' ;;
            esac ;;
        svc)
            if [ -z "$name" ]; then
                case "$out" in
                    json) jq --arg ns "$ns" '{items: [.svcs[] | select(.ns == $ns)
                            | {metadata: {name: .name}, spec: {selector: .selector, ports: .ports}}]}' "$W" ;;
                    *) jq -r --arg ns "$ns" '.svcs[] | select(.ns == $ns) | .name + " " + (.ports[0].port | tostring)' "$W" ;;
                esac
                return 0
            fi
            obj="$(jq -c --arg ns "$ns" --arg n "$name" '.svcs[] | select(.ns == $ns and .name == $n)' "$W")"
            [ -n "$obj" ] || { echo "Error from server (NotFound): not found" >&2; return 1; }
            case "$out" in
                "") echo "$name" ;;
                *) printf '%s' "$obj" | jq -r '.ports[] | select(.name == "web") | .port' ;;
            esac ;;
        secret|configmap)
            obj="$(jq -c --arg k "$kind" --arg ns "$ns" --arg n "$name" \
                '.objs[] | select(.kind == $k and .ns == $ns and .name == $n)' "$W")"
            [ -n "$obj" ] || { echo "Error from server (NotFound): not found" >&2; return 1; }
            case "$out" in
                "") echo "$name" ;;
                *metadata.labels*) printf '%s' "$obj" | jq -r '(.labels // {})["app.kubernetes.io/managed-by"] // ""' ;;
                *)
                    key="${out#jsonpath=\{.data.}"
                    key="${key%\}}"
                    key="${key//\\./.}"
                    printf '%s' "$obj" | jq -r --arg key "$key" '.data[$key] // empty' ;;
            esac ;;
        deployment) jq '.deployment' "$W" ;;
        pod)
            case "$out" in
                *phase*) jq -r '.probe.phase // ""' "$W" ;;
                *waiting*) jq -r '.probe.waiting // ""' "$W" ;;
                *exitCode*) jq -r '.probe.code // ""' "$W" ;;
            esac ;;
    esac
    return 0
}
sleep() { :; }

ENVIRONMENT=kubernetes
LLMSM_NS=scaling-manager-system
MONITORING_NAMESPACE=scaling-manager-monitoring
NAMESPACE="" PROMETHEUS_URL="" PROMETHEUS_URL_EXPLICIT=""

detect() { ( llmsm_detect_prometheus_url 2>/dev/null ); }

# ----------------------------------------------------------------------------
# Which Service: the one the certificate names
# ----------------------------------------------------------------------------
world stack
check 'kube-prometheus-stack, certificate names <stack>.svc: that Service, in the form named' \
    "https://$STACK:9090" "$(detect)"
lacks 'the private key is never read' 'tls\.key' "$(cat "$WORK/calls")"
world stackfqdn
check 'certificate names the stack Service with .cluster.local: that form' \
    "https://$STACK.cluster.local:9090" "$(detect)"
world stack new configmap
check 'certificate in a ConfigMap: read the same way' "https://$STACK:9090" "$(detect)"
world stack old
check 'older stack, selector prometheus=<cr>: still its own Service' "https://$STACK:9090" "$(detect)"
world wild
check 'a wildcard certificate: the stack Service before prometheus-operated' "https://$STACK:9090" "$(detect)"
world operated
check 'certificate names prometheus-operated only: prometheus-operated, though a stack Service exists' \
    "https://prometheus-operated.$MON.svc.cluster.local:9090" "$(detect)"
world operated none
check 'an operator install with only prometheus-operated: prometheus-operated' \
    "https://prometheus-operated.$MON.svc.cluster.local:9090" "$(detect)"
world stack new secret '["secret"]'
check 'certificate Forbidden: the CR own Service, not prometheus-operated' \
    "https://$STACK.cluster.local:9090" "$(detect)"
world none new secret '[]' no
check 'no TLS: http, the CR own Service' "http://$STACK.cluster.local:9090" "$(detect)"
world ext
check 'certificate names no Service: the CR own Service, left to the check' \
    "https://$STACK.cluster.local:9090" "$(detect)"

# ----------------------------------------------------------------------------
# An explicit PROMETHEUS_URL wins
# ----------------------------------------------------------------------------
world stack
check 'explicit PROMETHEUS_URL: the endpoint is the one given' 'https://prom.example.com:443' \
    "$( PROMETHEUS_URL_EXPLICIT='https://prom.example.com:443'; llmsm_prometheus_endpoint )"
check 'explicit PROMETHEUS_URL: resolution leaves it alone' "https://prometheus-operated.$MON.svc:9090" \
    "$( PROMETHEUS_URL_EXPLICIT=1 PROMETHEUS_URL="https://prometheus-operated.$MON.svc:9090"
        llmsm_resolve_prometheus_url >/dev/null 2>&1; printf '%s' "$PROMETHEUS_URL" )"
check 'no explicit URL: resolution takes the detected one' "https://$STACK:9090" \
    "$( PROMETHEUS_URL='https://kube-prometheus-stack-prometheus.default:9090'; LLMSM_PROMETHEUS_URL_RESOLVED=""
        llmsm_resolve_prometheus_url >/dev/null 2>&1; printf '%s' "$PROMETHEUS_URL" )"

# ----------------------------------------------------------------------------
# The name check
# ----------------------------------------------------------------------------
cert_check() { ( rc=0; llmsm_prometheus_cert_check "$1" "${2:-}" 2>/dev/null || rc=$?; printf '%s|%s|%s' "$rc" "$LLMSM_PROM_CERT_NAMES" "$LLMSM_PROM_CERT_SUGGEST" ); }
world stack
check 'name check: prometheus-operated against the stack certificate is a mismatch, with the names and a URL that works' \
    "1|$STACK, $CR, localhost|https://$STACK:9090" "$(cert_check "https://prometheus-operated.$MON.svc.cluster.local:9090")"
check 'name check: the stack Service matches' "0|$STACK, $CR, localhost|" "$(cert_check "https://$STACK:9090")"
check 'name check: .cluster.local form the certificate lacks is a mismatch' \
    "1|$STACK, $CR, localhost|https://$STACK:9090" "$(cert_check "https://$STACK.cluster.local:9090")"
check 'name check: PROMETHEUS_SERVER_NAME naming the certificate matches' "0|$STACK, $CR, localhost|" \
    "$(cert_check "https://prometheus-operated.$MON.svc.cluster.local:9090" "$STACK")"
check 'name check: a Service that is not a Prometheus CR (its own certificate): cannot tell' '2||' \
    "$(cert_check "https://thanos-query.$MON.svc:10902")"
check 'name check: outside the cluster: cannot tell' '2||' "$(cert_check 'https://prom.example.com')"
world stack new secret '["secret"]'
check 'name check: certificate Forbidden is cannot-tell, never a match' '2||' \
    "$(cert_check "https://prometheus-operated.$MON.svc.cluster.local:9090")"

# ----------------------------------------------------------------------------
# The preflight report: found is not a success when the name is wrong
# ----------------------------------------------------------------------------
world ext
out="$( llmsm_report_prometheus 2>&1 )"
lacks 'report: a mismatch is not printed as a success' '[SUCCESS] Prometheus:' "$out"
has 'report: a mismatch lists the names the certificate holds' 'valid for: prom.example.com' "$out"
world stack
has 'report: a matching address is a success' "[SUCCESS] Prometheus: https://$STACK:9090" "$( llmsm_report_prometheus 2>&1 )"

# ----------------------------------------------------------------------------
# The install-time verification
# ----------------------------------------------------------------------------
cfg() {
    printf 'PROMETHEUS_BASE_URL: "%s"\nPROMETHEUS_TLS_INSECURE_SKIP_VERIFY: "%s"\n' "$1" "$2"
    [ -z "${3:-}" ] || printf 'PROMETHEUS_CA_CERT_PATH: "%s"\n' "$3"
}
deployment() {
    jq --argjson mount "$1" '.deployment = {spec: {template: {spec: {
        securityContext: {runAsNonRoot: true},
        containers: [{name: "manager", volumeMounts: ([{name: "manager-config", mountPath: "/etc/scaling-manager"}]
            + (if $mount then [{name: "prometheus-ca", mountPath: "/etc/prometheus-certs", readOnly: true}] else [] end))}],
        volumes: [{name: "manager-config", configMap: {name: "scaling-manager-manager-config"}},
                  {name: "prometheus-ca", secret: {secretName: "prometheus-ca"}}]}}}}' \
        "$WORK/world.json" > "$WORK/world.next" && mv "$WORK/world.next" "$WORK/world.json"
}
verify() { ( PROMETHEUS_TLS_PROBE_TIMEOUT=3; llmsm_verify_prometheus_tls "$1" ) 2>&1; printf '\nrc=%s' "$?"; }
CA=/etc/prometheus-certs/ca.crt

world stack
deployment true
out="$(verify "$(cfg "https://prometheus-operated.$MON.svc.cluster.local:9090" false "$CA")")"
check 'verify: a name mismatch stops the install' 'rc=1' "$(printf '%s' "$out" | tail -1)"
has 'verify: the stop names the certificate names' "valid for: $STACK, $CR, localhost" "$out"
has 'verify: the stop says how to set PROMETHEUS_URL' "PROMETHEUS_URL=https://$STACK:9090" "$out"
lacks 'verify: a known mismatch does not probe' 'create' "$(cat "$WORK/calls")"

world stack
deployment true
probe_world Succeeded 0 200
out="$(verify "$(cfg "https://$STACK:9090" false "$CA")")"
check 'verify: a matching name and a probe that connects pass' 'rc=0' "$(printf '%s' "$out" | tail -1)"
has 'verify: says it verified, and with what' "Prometheus TLS verified from inside the cluster: https://$STACK:9090 (the CA at $CA" "$out"
check 'probe: never an option that skips verification' '0' \
    "$(jq '[.spec.containers[0].command[] | select(. == "-k" or . == "--insecure" or . == "--proxy-insecure")] | length' "$WORK/probe.json")"
check 'probe: the CA the controller is given' "--cacert $CA https://$STACK:9090" \
    "$(jq -r '.spec.containers[0].command | .[-3:] | join(" ")' "$WORK/probe.json")"
check 'probe: only the CA volume, mounted read-only where the controller has it' 'prometheus-ca|/etc/prometheus-certs|true' \
    "$(jq -r '[(.spec.volumes | map(.name) | join(",")), .spec.containers[0].volumeMounts[0].mountPath, (.spec.containers[0].volumeMounts[0].readOnly | tostring)] | join("|")' "$WORK/probe.json")"
check 'probe: no service account token, restricted security context' 'false|true|false|RuntimeDefault' \
    "$(jq -r '[(.spec.automountServiceAccountToken | tostring), (.spec.securityContext.runAsNonRoot | tostring), (.spec.containers[0].securityContext.allowPrivilegeEscalation | tostring), .spec.securityContext.seccompProfile.type] | join("|")' "$WORK/probe.json")"

world ext
deployment true
probe_world Failed 60 'curl: (60) SSL: no alternative certificate subject name matches target host name'
out="$(verify "$(cfg 'https://prom.ext.svc:9090' false "$CA")")"
check 'probe: a refused name stops the install' 'rc=1' "$(printf '%s' "$out" | tail -1)"
has 'probe: a refused name says to change PROMETHEUS_URL' 'PROMETHEUS_URL=https://<one of those names>' "$out"

world stack
deployment true
probe_world Failed 60 'curl: (60) SSL certificate problem: unable to get local issuer certificate'
out="$(verify "$(cfg "https://$STACK:9090" false "$CA")")"
check 'probe: an untrusted CA stops the install' 'rc=1' "$(printf '%s' "$out" | tail -1)"
has 'probe: an untrusted CA names the admin phase that copies it' 'make setup-prereqs' "$out"
lacks 'probe: and never offers skipping verification' 'SKIP_VERIFY' "$out"

world stack
deployment true
probe_world Failed 6 'curl: (6) Could not resolve host: nowhere.mon.svc'
out="$(verify "$(cfg 'https://nowhere.mon.svc:9090' false "$CA")")"
check 'probe: a host that does not resolve stops the install' 'rc=1' "$(printf '%s' "$out" | tail -1)"

check 'rerun command: a variable set again is printed once' \
    "PROMETHEUS_URL=https://b NAMESPACE=scaling-manager-system make deploy-scaling-manager" \
    "$( PROMETHEUS_URL_EXPLICIT=https://a; llmsm_rerun_cmd PROMETHEUS_URL=https://b )"

world stack new secret '["pods"]'
deployment true
out="$(verify "$(cfg "https://$STACK:9090" false "$CA")")"
check 'probe: Forbidden to create a Pod is a warning, not a stop' 'rc=0' "$(printf '%s' "$out" | tail -1)"
has 'probe: and it says the probe was refused' 'creating a Pod in scaling-manager-system was refused' "$out"

world stack
deployment false
out="$(verify "$(cfg "https://$STACK:9090" false "$CA")")"
has 'probe: a CA path no volume holds is named' 'mounts no volume that holds it' "$out"

world stack
deployment true
probe_world Failed 7 'curl: (7) Failed to connect'
has 'probe: a connection it could not make is not a verdict' 'Not a verdict on the certificate' \
    "$(verify "$(cfg "https://$STACK:9090" false "$CA")")"

world stack
deployment true
probe_world Pending '' '' ImagePullBackOff
has 'probe: an image it cannot pull is named' 'PROMETHEUS_TLS_PROBE_IMAGE' "$(verify "$(cfg "https://$STACK:9090" false "$CA")")"

world stack
deployment true
out="$(verify "$(cfg "https://prometheus-operated.$MON.svc.cluster.local:9090" true)")"
check 'verify: verification off, nothing to verify' "rc=0" "$(printf '%s' "$out" | tail -1)"

world stack
deployment true
PROMETHEUS_TLS_PROBE=false verify "$(cfg "https://$STACK:9090" false "$CA")" >/dev/null
lacks 'PROMETHEUS_TLS_PROBE=false: no Pod' 'create' "$(cat "$WORK/calls")"

# ----------------------------------------------------------------------------
# An upgrade from the old answer is the same Prometheus
# ----------------------------------------------------------------------------
same() { ( LLMSM_LIVE_PROM_URL="$1" PROMETHEUS_URL="$2" PROMETHEUS_URL_EXPLICIT="${3:-}"; llmsm_same_prometheus_as_live && echo same || echo different ); }
check 'live prometheus-operated, detected stack Service, same namespace: the same Prometheus' same \
    "$(same "https://prometheus-operated.$MON.svc.cluster.local:9090" "https://$STACK:9090")"
check 'the same move, but the caller set the URL: a new decision' different \
    "$(same "https://prometheus-operated.$MON.svc.cluster.local:9090" "https://$STACK:9090" 1)"

# ----------------------------------------------------------------------------
# The controller-log hint names the fix for a name mismatch
# ----------------------------------------------------------------------------
out="$( llmsm_hint_untrusted_prometheus_cert "x509: certificate is valid for $STACK, not prometheus-operated.$MON.svc.cluster.local" 2>&1 )"
has 'hint: a name mismatch points at PROMETHEUS_URL' 'PROMETHEUS_URL=https://<name>:<port>' "$out"

# ----------------------------------------------------------------------------
# The CA: copied for the controller with nothing done by hand
# ----------------------------------------------------------------------------
PROMETHEUS_SECRET_NAME_EXPLICIT="" PROMETHEUS_SECRET_NS_EXPLICIT="" PROMETHEUS_CA_FILE=""
PROMETHEUS_CA_CERT_PATH="" PROMETHEUS_TLS_INSECURE_SKIP_VERIFY="" DEPLOY_PROMETHEUS=false
LLMSM_LIVE_PROM_TLS=absent
applied_ca() { yq -r 'select(.kind == "Secret") | .data["ca.crt"]' "$WORK/applied" 2>/dev/null | base64 -d 2>/dev/null; }
# sync <url> runs the CA copy for PROMETHEUS_URL=<url> and prints
# "<rc>|<LLMSM_PROM_CA_MOUNT>|<ca path>|<skip verify>" -- the settings the controller gets.
sync() {
    ( PROMETHEUS_URL="$1"
      llmsm_sync_prometheus_ca
      printf '%s|%s|%s|%s' 0 "$LLMSM_PROM_CA_MOUNT" "$(llmsm_prometheus_ca_path)" "$(llmsm_prometheus_skip_verify)" ) 2>"$WORK/sync.err" \
        || printf '%s|||' "$?"
}

world stack
check 'CA: the stack TLS Secret is copied, mounted, and verified against' \
    "0|1|/etc/scaling-manager-prometheus-ca/ca.crt|false" "$(sync "https://$STACK:9090")"
check 'CA: the copy is the serving certificate (self-signed, its own CA)' "$(cat "$WORK/stack.crt")" "$(applied_ca)"
check 'CA: the copy is labelled as the installer made it' 'scaling-manager-installer' \
    "$(yq -r 'select(.kind == "Secret") | .metadata.labels["app.kubernetes.io/managed-by"]' "$WORK/applied")"
lacks 'CA: the private key is never read' 'tls\.key' "$(cat "$WORK/calls")"
lacks 'CA: the private key is never written' 'UFJJVkFURQ==' "$(cat "$WORK/applied")"

world stack
add_obj "$(jq -n --arg mon "$MON" --arg ca "$(cat "$WORK/operated.b64")" --arg crt "$(cat "$WORK/stack.b64")" \
    '{kind: "secret", ns: $mon, name: "prom-tls", data: {"ca.crt": $ca, "tls.crt": $crt}}')"
jq '.objs |= (map(select(.name != "prom-tls")) + [last])' "$WORK/world.json" > "$WORK/world.next" && mv "$WORK/world.next" "$WORK/world.json"
sync "https://$STACK:9090" >/dev/null
check 'CA: a Secret with ca.crt gives ca.crt, not the serving certificate' "$(cat "$WORK/operated.crt")" "$(applied_ca)"

world stack
bundle="$(printf '%s\n%s\n' "$(cat "$WORK/stack.crt")" "$(cat "$WORK/stack.key")" | base64 | tr -d '\n')"
jq --arg b "$bundle" '(.objs[] | select(.name == "prom-tls") | .data["tls.crt"]) = $b' "$WORK/world.json" > "$WORK/world.next" && mv "$WORK/world.next" "$WORK/world.json"
sync "https://$STACK:9090" >/dev/null
lacks 'CA: a certificate bundle carrying a key gives up the certificate alone' 'PRIVATE KEY' "$(applied_ca)"

# The full automatic path: detection, the copy, the config and the Deployment
# the first apply would create.
world stack
deployment true
OVL="$(mktemp -d "$WORK/ovl.XXXXXX")"
ln -s "$ROOT/config/overlays/cluster-scoped/kubernetes" "$OVL/base"
printf 'namespace: scaling-manager-system\nresources:\n- ./base\npatches:\n' > "$OVL/kustomization.yaml"
( PROMETHEUS_URL='https://kube-prometheus-stack-prometheus.scaling-manager-monitoring.svc.cluster.local:9090'
  LLMSM_PROMETHEUS_URL_RESOLVED="" LLMSM_PROJECT="$ROOT"
  llmsm_resolve_prometheus_url
  llmsm_sync_prometheus_ca
  llmsm_render_manager_config_patch "$OVL"
  llmsm_append_prometheus_ca_mount_patch "$OVL/kustomization.yaml" ) >/dev/null 2>&1
command "${KBUILD[@]}" "$OVL" > "$WORK/rendered.yaml" 2>"$WORK/rendered.err" \
    || printf 'kustomize build failed: %s\n' "$(head -3 "$WORK/rendered.err")" >&2
cfgout="$(yq -r 'select(.kind == "ConfigMap" and .metadata.name == "scaling-manager-manager-config") | .data["config.yaml"]' "$WORK/rendered.yaml")"
check 'automatic: the config names the Service the certificate names' "https://$STACK:9090" \
    "$(printf '%s\n' "$cfgout" | yq -r '.PROMETHEUS_BASE_URL')"
check 'automatic: the config verifies, with the copied CA' 'false|/etc/scaling-manager-prometheus-ca/ca.crt' \
    "$(printf '%s\n' "$cfgout" | yq -r '.PROMETHEUS_TLS_INSECURE_SKIP_VERIFY + "|" + .PROMETHEUS_CA_CERT_PATH')"
check 'automatic: the Deployment mounts the CA read-only at that path' '/etc/scaling-manager-prometheus-ca|true|scaling-manager-prometheus-ca|ca.crt' \
    "$(yq -r 'select(.kind == "Deployment" and .metadata.name == "scaling-manager-controller-manager") | .spec.template.spec as $s
        | ($s.containers[0].volumeMounts[] | select(.name == "scaling-manager-prometheus-ca")) as $m
        | ($s.volumes[] | select(.name == "scaling-manager-prometheus-ca")) as $v
        | $m.mountPath + "|" + ($m.readOnly | tostring) + "|" + $v.secret.secretName + "|" + ($v.secret.items | map(.key) | join(","))' "$WORK/rendered.yaml")"

# A namespace admin cannot read the monitoring namespace Secrets.
world stack new secret "[\"secret:$MON\"]"
add_obj '{"kind": "secret", "ns": "scaling-manager-system", "name": "scaling-manager-prometheus-ca", "data": {"ca.crt": "eA=="}, "labels": {"app.kubernetes.io/managed-by": "scaling-manager-installer"}}'
check 'CA: source Forbidden, a copy the admin phase made: mounted' \
    "0|1|/etc/scaling-manager-prometheus-ca/ca.crt|false" "$(sync "https://$STACK:9090")"
has 'CA: and it says the copy is from earlier' 'copied earlier' "$(cat "$WORK/sync.err")"

world stack new secret "[\"secret:$MON\"]"
check 'CA: source Forbidden and no copy: the install stops' '1|||' "$(sync "https://$STACK:9090")"
has 'CA: the stop names the admin phase' 'make setup-prereqs' "$(cat "$WORK/sync.err")"
has 'CA: the stop gives the copy command, certificate only' "jsonpath='{.data.tls\\.crt}' | base64 -d > ca.crt" "$(cat "$WORK/sync.err")"
lacks 'CA: the stop never offers skipping verification' 'SKIP_VERIFY' "$(cat "$WORK/sync.err")"

world stack
add_obj '{"kind": "secret", "ns": "scaling-manager-system", "name": "scaling-manager-prometheus-ca", "data": {"ca.crt": "eA=="}}'
check 'CA: a Secret of that name the installer did not make is mounted, not overwritten' \
    "0|1|/etc/scaling-manager-prometheus-ca/ca.crt|false" "$(sync "https://$STACK:9090")"
check 'CA: and nothing was written' '' "$(cat "$WORK/applied" 2>/dev/null)"

world stack
check 'CA: PROMETHEUS_CA_CERT_PATH set by the caller: no copy' "0||/etc/mine/ca.crt|false" \
    "$(PROMETHEUS_CA_CERT_PATH=/etc/mine/ca.crt sync "https://$STACK:9090")"
check 'CA: PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true: no copy' "0|||true" \
    "$(PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true sync "https://$STACK:9090")"
world none new secret '[]' no
check 'CA: plain http: no copy' "0|||" "$(sync "http://$STACK.cluster.local:9090")"

# A namespace admin may not list Prometheus resources at all: detection finds
# nothing, and the address the controller runs with is kept.
world stack new secret '["prometheuses.monitoring.coreos.com"]'
check 'namespace admin, nothing listable: the live address is kept, not the shipped default' "https://$STACK:9090" \
    "$( PROMETHEUS_URL='https://kube-prometheus-stack-prometheus.default:9090' LLMSM_PROMETHEUS_URL_RESOLVED=""
        LLMSM_LIVE_PROM_TLS=read LLMSM_LIVE_PROM_URL="https://$STACK:9090"
        llmsm_resolve_prometheus_url >/dev/null 2>&1; printf '%s' "$PROMETHEUS_URL" )"
check 'namespace admin, no copy: the installer CA path is not kept without its volume' '|' \
    "$( LLMSM_LIVE_PROM_TLS=read LLMSM_LIVE_PROM_URL="https://$STACK:9090" LLMSM_LIVE_CA_PATH=/etc/scaling-manager-prometheus-ca/ca.crt
        PROMETHEUS_URL="https://$STACK:9090"; llmsm_sync_prometheus_ca 2>/dev/null
        printf '%s|%s' "$LLMSM_PROM_CA_MOUNT" "$(llmsm_prometheus_ca_path)" )"

world stack
add_obj "$(jq -n --arg crt "$(cat "$WORK/wild.b64")" '{kind: "secret", ns: "ops", name: "my-prom-tls", data: {"tls.crt": $crt}}')"
PROMETHEUS_SECRET_NAME_EXPLICIT=my-prom-tls PROMETHEUS_SECRET_NS_EXPLICIT=ops sync "https://$STACK:9090" >/dev/null
check 'CA: PROMETHEUS_SECRET_NAME/NS set by the caller: copied from there' "$(cat "$WORK/wild.crt")" "$(applied_ca)"

world stack
PROMETHEUS_CA_FILE="$WORK/ext.crt" sync "https://$STACK:9090" >/dev/null
check 'CA: PROMETHEUS_CA_FILE: copied from the file' "$(cat "$WORK/ext.crt")" "$(applied_ca)"

printf '\n%d passed, %d failed (expected %d)\n' "$RAN" "$FAILED" "$EXPECT_CASES"
[ "$FAILED" -eq 0 ] || exit 1
if [ "$RAN" -ne "$EXPECT_CASES" ]; then
    printf 'FAIL: %d cases ran, expected %d -- a case was added or lost without updating EXPECT_CASES.\n' "$RAN" "$EXPECT_CASES" >&2
    exit 1
fi
