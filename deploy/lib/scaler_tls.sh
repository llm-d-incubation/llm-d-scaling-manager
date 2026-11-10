#!/usr/bin/env bash
#
# Certificates for the KEDA external scaler (:9090), and the controller flags
# that serve them.
#
# The controller serves the scaler over mutual TLS: a serving certificate for
# the scaler Service, a client certificate for KEDA, and the controller pinned to
# that client's identity. KEDA presents its certificate through a
# ClusterTriggerAuthentication the admin phase writes, and every ScaledObject the
# installer writes points at it (see "KEDA's side" below).
#
# LLMSM_SCALER_TLS unset is the default: TLS once KEDA's side is in place and every
# ScaledObject naming the scaler can follow it; plaintext, with a warning, when
# not. LLMSM_SCALER_TLS=true makes either of those a refusal. LLMSM_SCALER_TLS=false
# stays on plaintext. LLMSM_SCALER_TLS_MIGRATE=true moves existing ScaledObjects
# with the scaler.
#
# Where the certificates come from:
#
#   cert-manager present   an Issuer chain (self-signed root -> CA Issuer) and
#                          three Certificates. cert-manager renews them.
#   no cert-manager        a private CA and two leaves minted here with openssl,
#                          stored as Secrets. A re-install re-issues a leaf that
#                          is missing, expires within LLMSM_SCALER_RENEW_DAYS, names
#                          other hosts, or no longer chains to the CA. The CA is
#                          kept until it is missing or itself expiring.
#   OpenShift              the SERVING certificate comes from the service CA
#                          (an annotation on the scaler Service); the service CA
#                          issues serving certificates only, so KEDA's client
#                          certificate still comes from cert-manager or openssl.
#
# Never overwrites a Secret, Issuer or Certificate it did not create: everything
# it writes carries the label in LLMSM_SCALER_TLS_LABEL, and anything else under
# one of its names is a refusal. A Secret it cannot read is a refusal too --
# Forbidden is "unknown", never "absent".
#
# Rotation and the running controller: the serving certificate is watched
# (controller-runtime certwatcher) and reloaded without a restart. The client CA
# is read once at start, so the Deployment's pod template carries its digest:
# a changed CA changes the template and rolls the controller, an unchanged one
# does not.
#
# Requires vars: LLMSM_NS, ENVIRONMENT. Requires funcs: log_info, log_warning,
# log_error, and kc_capture (common.sh) for the KEDA side; kubectl and jq on PATH
# (openssl for the no-cert-manager path).

LLMSM_SCALER_TLS_SECRET=scaling-manager-external-scaler-tls
LLMSM_SCALER_CLIENT_SECRET=scaling-manager-external-scaler-client-tls
LLMSM_SCALER_CA_SECRET=scaling-manager-external-scaler-ca
LLMSM_SCALER_TLS_LABEL=scaling-manager.llmd.ai/scaler-tls
LLMSM_SCALER_TLS_ANNOTATION=scaling-manager.llmd.ai/scaler-tls
LLMSM_SCALER_CA_HASH_ANNOTATION=scaling-manager.llmd.ai/scaler-client-ca-sha256
LLMSM_SCALER_CERT_DIR=/etc/scaling-manager/scaler-tls
LLMSM_SCALER_CLIENT_CA_DIR=/etc/scaling-manager/scaler-client-ca
LLMSM_SCALER_CLIENT_SAN_DEFAULT=scaling-manager-keda-client
LLMSM_SCALER_LEAF_DAYS=365
LLMSM_SCALER_CA_DAYS=3650
# KEDA reads its client certificate when it first dials a scaler address and
# keeps that connection until its operator restarts, so the certificate lives
# as long as the CA: a renewed one would not reach KEDA before a restart (see
# "KEDA's side").
LLMSM_SCALER_CLIENT_DAYS=$LLMSM_SCALER_CA_DAYS
LLMSM_SCALER_RENEW_DAYS=30

# llmsm_scaler_dns_names echoes the names the serving certificate must carry: the
# Service by its short in-cluster name, and by the fully qualified one the
# installer writes into every ScaledObject's scalerAddress.
llmsm_scaler_dns_names() {
    printf '%s %s\n' "scaling-manager-external-scaler.${LLMSM_NS}.svc" "scaling-manager-external-scaler.${LLMSM_NS}.svc.cluster.local"
}

# llmsm_scaler_client_san echoes the identity KEDA's client certificate carries
# and the controller accepts (--external-scaler-client-san).
llmsm_scaler_client_san() {
    printf '%s\n' "${LLMSM_SCALER_CLIENT_SAN:-$LLMSM_SCALER_CLIENT_SAN_DEFAULT}"
}

# llmsm_scaler_check_client_san refuses an LLMSM_SCALER_CLIENT_SAN that is not one
# lowercase DNS name. The value goes into an openssl config, a YAML manifest and
# a controller flag; a comma, a newline or a space would add names or keys there.
# Each label is 1 to 63 characters and starts and ends with a letter or digit,
# and the whole name is at most 253, as RFC 1123 has it.
llmsm_scaler_check_client_san() {
    local san="${LLMSM_SCALER_CLIENT_SAN:-}" label='[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?'
    local re="^${label}(\.${label})*\$"
    [ -n "$san" ] || return 0
    [ "${#san}" -le 253 ] && [[ "$san" =~ $re ]] && return 0
    log_error "LLMSM_SCALER_CLIENT_SAN must be one lowercase DNS name: labels of letters, digits and hyphens, each starting and ending with a letter or digit and at most 63 long, joined by single dots, 253 in all (got: ${san})."
}

# llmsm_scaler_not_found succeeds when kubectl error text $1 is the API server
# saying the object or resource does not exist. Anchored on kubectl's own
# "(NotFound)", never on the bare word: a namespace or object named notfound-prod
# appears in a Forbidden message too, and Forbidden is "unknown", never "absent".
llmsm_scaler_not_found() {
    printf '%s' "$1" | grep -qF '(NotFound)'
}

# llmsm_scaler_tls_read_live sets LLMSM_SCALER_TLS_LIVE from the running controller
# Deployment:
#   absent      no Deployment yet (a new install)
#   unreadable  the read failed for another reason (LLMSM_SCALER_TLS_LIVE_ERR)
#   plaintext   no TLS flags
#   manual      TLS flags this installer did not set
#   <provider>  installer-provisioned TLS: cert-manager or openssl
llmsm_scaler_tls_read_live() {
    local out rc=0 provider has_flag
    LLMSM_SCALER_TLS_LIVE=absent
    LLMSM_SCALER_TLS_LIVE_ERR=""
    LLMSM_SCALER_TLS_LIVE_FLAGS=""
    out="$(kubectl get deployment scaling-manager-controller-manager -n "$LLMSM_NS" -o json 2>&1)" || rc=$?
    if [ "$rc" != 0 ]; then
        if llmsm_scaler_not_found "$out"; then
            return 0
        fi
        LLMSM_SCALER_TLS_LIVE=unreadable
        LLMSM_SCALER_TLS_LIVE_ERR="$(printf '%s\n' "$out" | head -1)"
        return 0
    fi
    provider="$(printf '%s' "$out" | jq -r --arg k "$LLMSM_SCALER_TLS_ANNOTATION" \
        '.spec.template.metadata.annotations[$k] // ""' 2>/dev/null)" || provider=""
    has_flag="$(printf '%s' "$out" | jq -r \
        '[.spec.template.spec.containers[]? | (.args // [])[] | select(startswith("--external-scaler-cert-path"))] | length' \
        2>/dev/null)" || has_flag=0
    LLMSM_SCALER_TLS_LIVE_FLAGS="$(printf '%s' "$out" | jq -r \
        '[.spec.template.spec.containers[]? | (.args // [])[] | select(startswith("--external-scaler-"))] | .[]' \
        2>/dev/null)" || LLMSM_SCALER_TLS_LIVE_FLAGS=""
    if [ -n "$provider" ]; then
        LLMSM_SCALER_TLS_LIVE="$provider"
    elif [ "${has_flag:-0}" != 0 ]; then
        LLMSM_SCALER_TLS_LIVE=manual
    else
        LLMSM_SCALER_TLS_LIVE=plaintext
    fi
}

# llmsm_cert_manager_api sets LLMSM_CERT_MANAGER_API to present, absent or unknown.
# The discovery document is readable by every authenticated user, so a tenant
# gets the same answer an admin does; any error but NotFound is unknown.
llmsm_cert_manager_api() {
    local out rc=0
    out="$(kubectl get --raw /apis/cert-manager.io 2>&1)" || rc=$?
    if [ "$rc" = 0 ]; then
        LLMSM_CERT_MANAGER_API=present
    elif llmsm_scaler_not_found "$out"; then
        LLMSM_CERT_MANAGER_API=absent
    else
        LLMSM_CERT_MANAGER_API=unknown
        LLMSM_CERT_MANAGER_API_ERR="$(printf '%s\n' "$out" | head -1)"
    fi
}

# llmsm_scaler_object_state reads object $1 (a resource, e.g. secret) named $2 in
# LLMSM_NS and sets LLMSM_OBJ_STATE to absent, ours, foreign or unreadable, with the
# object in LLMSM_OBJ_JSON. "ours" means it carries LLMSM_SCALER_TLS_LABEL.
llmsm_scaler_object_state() {
    local resource="$1" name="$2" out rc=0 owner
    LLMSM_OBJ_JSON=""
    LLMSM_OBJ_STATE=absent
    LLMSM_OBJ_ERR=""
    out="$(kubectl get "$resource" "$name" -n "$LLMSM_NS" -o json 2>&1)" || rc=$?
    if [ "$rc" != 0 ]; then
        if llmsm_scaler_not_found "$out"; then
            return 0
        fi
        LLMSM_OBJ_STATE=unreadable
        LLMSM_OBJ_ERR="$(printf '%s\n' "$out" | head -1)"
        return 0
    fi
    LLMSM_OBJ_JSON="$out"
    owner="$(printf '%s' "$out" | jq -r --arg k "$LLMSM_SCALER_TLS_LABEL" '.metadata.labels[$k] // ""' 2>/dev/null)" || owner=""
    if [ -n "$owner" ]; then
        LLMSM_OBJ_STATE=ours
    else
        LLMSM_OBJ_STATE=foreign
    fi
}

# llmsm_scaler_require_writable refuses unless object $1/$2 is absent or ours.
llmsm_scaler_require_writable() {
    local resource="$1" name="$2"
    llmsm_scaler_object_state "$resource" "$name"
    case "$LLMSM_OBJ_STATE" in
        absent|ours) return 0 ;;
        unreadable)
            llmsm_scaler_drop_tmp
            log_error "Cannot read $resource/$name in $LLMSM_NS ($LLMSM_OBJ_ERR), so whether this installer created it is unknown. Grant get on $resource there, or set LLMSM_SCALER_TLS=false." ;;
        *)
            llmsm_scaler_drop_tmp
            log_error "$resource/$name in $LLMSM_NS was not created by this installer (no $LLMSM_SCALER_TLS_LABEL label); refusing to overwrite it. Remove or rename it, or set LLMSM_SCALER_TLS=false." ;;
    esac
}

# llmsm_scaler_secret_field writes data key $1 of LLMSM_OBJ_JSON, decoded, to file $2
# (empty when the key is absent).
llmsm_scaler_secret_field() {
    local key="$1" file="$2" b64
    b64="$(printf '%s' "$LLMSM_OBJ_JSON" | jq -r --arg k "$key" '.data[$k] // ""' 2>/dev/null)" || b64=""
    if [ -n "$b64" ]; then
        printf '%s' "$b64" | base64 -d > "$file" 2>/dev/null || : > "$file"
    else
        : > "$file"
    fi
}

# llmsm_scaler_cert_sans echoes the DNS names certificate $1 carries, sorted and
# space-separated.
llmsm_scaler_cert_sans() {
    openssl x509 -in "$1" -noout -text 2>/dev/null \
        | awk '/Subject Alternative Name/ { getline; print }' \
        | tr ',' '\n' | sed -n 's/^ *DNS://p' | sort -u | tr '\n' ' '
}

# llmsm_scaler_sorted_names echoes its arguments sorted, unique, space-separated,
# in the form llmsm_scaler_cert_sans prints.
llmsm_scaler_sorted_names() {
    printf '%s\n' "$@" | sort -u | tr '\n' ' '
}

# llmsm_scaler_cert_ok succeeds when certificate $1 chains to CA $2, stays valid
# for LLMSM_SCALER_RENEW_DAYS more days, and names exactly the hosts in $3.
llmsm_scaler_cert_ok() {
    local cert="$1" ca="$2" want="$3"
    [ -s "$cert" ] && [ -s "$ca" ] || return 1
    openssl verify -CAfile "$ca" "$cert" >/dev/null 2>&1 || return 1
    openssl x509 -in "$cert" -noout -checkend $((LLMSM_SCALER_RENEW_DAYS * 86400)) >/dev/null 2>&1 || return 1
    # shellcheck disable=SC2086 # $want is a space-separated list on purpose
    [ "$(llmsm_scaler_cert_sans "$cert")" = "$(llmsm_scaler_sorted_names $want)" ]
}

# llmsm_scaler_new_ca mints a CA into $1/ca.crt and $1/ca.key.
llmsm_scaler_new_ca() {
    local d="$1"
    printf '[req]\ndistinguished_name=dn\nx509_extensions=v3\nprompt=no\n[dn]\nCN=llmsm-scaler-ca.%s\n[v3]\nbasicConstraints=critical,CA:TRUE\nkeyUsage=critical,keyCertSign,cRLSign\nsubjectKeyIdentifier=hash\n' \
        "$LLMSM_NS" > "$d/ca.cnf"
    openssl req -x509 -new -newkey rsa:2048 -nodes -sha256 -days "$LLMSM_SCALER_CA_DAYS" \
        -keyout "$d/ca.key" -out "$d/ca.crt" -config "$d/ca.cnf" >/dev/null 2>&1
}

# llmsm_scaler_new_leaf mints $1/$2.crt and $1/$2.key, signed by $1/ca.*, with
# common name $3, extended key usage $4 and the DNS names that follow.
llmsm_scaler_new_leaf() {
    local d="$1" name="$2" cn="$3" eku="$4" san="" n
    shift 4
    for n in "$@"; do san="${san:+$san,}DNS:$n"; done
    printf '[req]\ndistinguished_name=dn\nprompt=no\n[dn]\nCN=%s\n' "$cn" > "$d/$name.cnf"
    printf 'basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=%s\nsubjectAltName=%s\n' \
        "$eku" "$san" > "$d/$name.ext"
    openssl req -new -newkey rsa:2048 -nodes -sha256 -keyout "$d/$name.key" -out "$d/$name.csr" \
        -config "$d/$name.cnf" >/dev/null 2>&1 || return 1
    openssl x509 -req -sha256 -in "$d/$name.csr" -CA "$d/ca.crt" -CAkey "$d/ca.key" \
        -set_serial "0x$(openssl rand -hex 16)" -days "$LLMSM_SCALER_LEAF_DAYS" \
        -extfile "$d/$name.ext" -out "$d/$name.crt" >/dev/null 2>&1
}

# llmsm_scaler_write_secret writes kubernetes.io/tls Secret $1 (role $2) from the
# files $3 (tls.crt), $4 (tls.key) and $5 (ca.crt). Server-side apply, so no
# last-applied annotation carries a second copy of the private key; the
# ownership check before it already established the Secret is ours.
llmsm_scaler_write_secret() {
    local name="$1" role="$2" crt="$3" key="$4" ca="$5"
    jq -n --arg n "$name" --arg ns "$LLMSM_NS" --arg l "$LLMSM_SCALER_TLS_LABEL" --arg r "$role" \
        --rawfile crt "$crt" --rawfile key "$key" --rawfile ca "$ca" \
        '{apiVersion: "v1", kind: "Secret", type: "kubernetes.io/tls",
          metadata: {name: $n, namespace: $ns,
                     labels: {"app.kubernetes.io/managed-by": "llm-d-scaling-manager", ($l): $r}},
          data: {"tls.crt": ($crt | @base64), "tls.key": ($key | @base64), "ca.crt": ($ca | @base64)}}' \
        | kubectl apply --server-side --force-conflicts --field-manager=llm-d-scaling-manager -f - >/dev/null \
        || { llmsm_scaler_drop_tmp; log_error "Could not write Secret $name in $LLMSM_NS."; }
}

# llmsm_scaler_tmp_begin makes the private directory in LLMSM_SCALER_TMPD that holds
# private keys while certificates are minted or copied, and arms INT, TERM and
# HUP traps that remove it, so a Ctrl-C or a closed terminal does not leave a
# key on disk.
#
# Those three only, never EXIT: deploy_scaling_manager_controller owns the EXIT trap (its
# throw-away overlay), and replacing it would leak that directory instead. The
# handlers exit, which still runs that EXIT trap. The INT, TERM and HUP traps in
# force before are saved and put back by llmsm_scaler_drop_tmp.
#
# Every command between this and llmsm_scaler_drop_tmp is guarded (|| ...): the
# installer runs under set -e and pipefail, and an unguarded failure would exit
# there with the key still in the directory.
llmsm_scaler_tmp_begin() {
    LLMSM_SCALER_TMPD="$(mktemp -d)" || log_error "Could not create a temporary directory for the scaler certificates."
    chmod 700 "$LLMSM_SCALER_TMPD" || true
    LLMSM_SCALER_SAVED_TRAPS="$(trap -p INT TERM HUP)"
    trap 'llmsm_scaler_drop_tmp; exit 130' INT
    trap 'llmsm_scaler_drop_tmp; exit 143' TERM
    trap 'llmsm_scaler_drop_tmp; exit 129' HUP
}

# llmsm_scaler_drop_tmp removes the directory in LLMSM_SCALER_TMPD, which holds
# private keys while certificates are minted, before a refusal exits, and puts
# back the INT, TERM and HUP traps llmsm_scaler_tmp_begin replaced.
llmsm_scaler_drop_tmp() {
    [ -z "${LLMSM_SCALER_TMPD:-}" ] || rm -rf "$LLMSM_SCALER_TMPD"
    LLMSM_SCALER_TMPD=""
    if [ -n "${LLMSM_SCALER_SAVED_TRAPS+set}" ]; then
        trap - INT TERM HUP
        eval "$LLMSM_SCALER_SAVED_TRAPS"
        unset LLMSM_SCALER_SAVED_TRAPS
    fi
}

# llmsm_scaler_same_object succeeds when object $1 (JSON, as read) already has
# what object $2 (JSON, as it would be applied) sets: field $3 (spec or data)
# equal, and every label of $2. A write that would change nothing is skipped.
llmsm_scaler_same_object() {
    [ -n "$1" ] || return 1
    jq -e -n --argjson cur "$1" --argjson want "$2" --arg f "$3" '
        ($cur[$f] // null) == ($want[$f] // null)
        and ([($want.metadata.labels // {}) | to_entries[] | . as $e | ($cur.metadata.labels // {})[$e.key] == $e.value] | all)' \
        >/dev/null 2>&1
}

# llmsm_scaler_sha256 echoes the SHA-256 of file $1.
llmsm_scaler_sha256() {
    openssl dgst -sha256 "$1" 2>/dev/null | awk '{ print $NF }'
}

# llmsm_scaler_check_openshift_serving makes room for the service CA to write the
# serving Secret: absent or already the service CA's is fine, one this installer
# wrote earlier is removed, anything else is a refusal. Every refusal drops the
# temporary key directory first, in case a caller already made one.
llmsm_scaler_check_openshift_serving() {
    local svc
    llmsm_scaler_object_state secret "$LLMSM_SCALER_TLS_SECRET"
    case "$LLMSM_OBJ_STATE" in
        absent) return 0 ;;
        unreadable)
            llmsm_scaler_drop_tmp
            log_error "Cannot read Secret $LLMSM_SCALER_TLS_SECRET in $LLMSM_NS ($LLMSM_OBJ_ERR), so whether the service CA owns it is unknown. Grant get on secrets there, or set LLMSM_SCALER_TLS=false." ;;
        ours)
            kubectl delete secret "$LLMSM_SCALER_TLS_SECRET" -n "$LLMSM_NS" --ignore-not-found >/dev/null 2>&1 \
                || { llmsm_scaler_drop_tmp; log_error "Could not remove Secret $LLMSM_SCALER_TLS_SECRET in $LLMSM_NS so the service CA can issue it."; }
            return 0 ;;
    esac
    svc="$(printf '%s' "$LLMSM_OBJ_JSON" | jq -r '.metadata.annotations["service.beta.openshift.io/originating-service-name"] // ""' 2>/dev/null)" || svc=""
    [ "$svc" = scaling-manager-external-scaler ] && return 0
    llmsm_scaler_drop_tmp
    log_error "Secret $LLMSM_SCALER_TLS_SECRET in $LLMSM_NS was not issued by the OpenShift service CA for scaling-manager-external-scaler, nor by this installer; refusing to take it over. Remove or rename it, or set LLMSM_SCALER_TLS=false."
}

# llmsm_scaler_tls_provision_openssl mints or refreshes the CA and leaves with
# openssl and sets LLMSM_SCALER_CLIENT_CA_HASH.
llmsm_scaler_tls_provision_openssl() {
    local d ca_new=false issued="" kept="" serving=true dns san ca_json client_json serving_json=""
    [ "${ENVIRONMENT:-}" = openshift ] && serving=false

    # Every refusal first, before the temporary directory exists, so none can
    # leave a private key (or an empty directory) behind.
    llmsm_scaler_require_writable secret "$LLMSM_SCALER_CA_SECRET"
    ca_json="$LLMSM_OBJ_JSON"
    llmsm_scaler_require_writable secret "$LLMSM_SCALER_CLIENT_SECRET"
    client_json="$LLMSM_OBJ_JSON"
    if [ "$serving" = true ]; then
        llmsm_scaler_require_writable secret "$LLMSM_SCALER_TLS_SECRET"
        serving_json="$LLMSM_OBJ_JSON"
    else
        llmsm_scaler_check_openshift_serving
    fi

    llmsm_scaler_tmp_begin
    d="$LLMSM_SCALER_TMPD"
    LLMSM_OBJ_JSON="$ca_json"
    llmsm_scaler_secret_field tls.crt "$d/ca.crt"
    llmsm_scaler_secret_field tls.key "$d/ca.key"
    LLMSM_OBJ_JSON="$client_json"
    llmsm_scaler_secret_field tls.crt "$d/client.old"
    llmsm_scaler_secret_field ca.crt "$d/client-ca.old"
    LLMSM_OBJ_JSON="$serving_json"
    llmsm_scaler_secret_field tls.crt "$d/serving.old"

    # A provider switch: Certificates this installer created would keep
    # rewriting the Secrets below.
    llmsm_cert_manager_api
    if [ "$LLMSM_CERT_MANAGER_API" = present ]; then
        kubectl delete certificates.cert-manager.io,issuers.cert-manager.io -n "$LLMSM_NS" \
            -l "$LLMSM_SCALER_TLS_LABEL" --ignore-not-found >/dev/null 2>&1 || true
    fi

    if [ ! -s "$d/ca.crt" ] || [ ! -s "$d/ca.key" ] \
        || ! openssl x509 -in "$d/ca.crt" -noout -checkend $((LLMSM_SCALER_RENEW_DAYS * 86400)) >/dev/null 2>&1; then
        llmsm_scaler_new_ca "$d" || { llmsm_scaler_drop_tmp; log_error "openssl could not mint the scaler CA."; }
        llmsm_scaler_write_secret "$LLMSM_SCALER_CA_SECRET" ca "$d/ca.crt" "$d/ca.key" "$d/ca.crt"
        ca_new=true
        issued="$issued CA"
    else
        kept="$kept CA"
    fi

    if [ "$serving" = true ]; then
        dns="$(llmsm_scaler_dns_names)"
        if [ "$ca_new" = true ] || ! llmsm_scaler_cert_ok "$d/serving.old" "$d/ca.crt" "$dns"; then
            # shellcheck disable=SC2086 # two names, split on purpose
            llmsm_scaler_new_leaf "$d" serving "llmsm-external-scaler.${LLMSM_NS}.svc" serverAuth $dns \
                || { llmsm_scaler_drop_tmp; log_error "openssl could not mint the scaler serving certificate."; }
            llmsm_scaler_write_secret "$LLMSM_SCALER_TLS_SECRET" serving "$d/serving.crt" "$d/serving.key" "$d/ca.crt"
            issued="$issued serving"
        else
            kept="$kept serving"
        fi
    fi

    san="$(llmsm_scaler_client_san)"
    if [ "$ca_new" = true ] || ! llmsm_scaler_cert_ok "$d/client.old" "$d/ca.crt" "$san" \
        || ! cmp -s "$d/client-ca.old" "$d/ca.crt"; then
        LLMSM_SCALER_LEAF_DAYS="$LLMSM_SCALER_CLIENT_DAYS" llmsm_scaler_new_leaf "$d" client "$san" clientAuth "$san" \
            || { llmsm_scaler_drop_tmp; log_error "openssl could not mint the KEDA client certificate."; }
        llmsm_scaler_write_secret "$LLMSM_SCALER_CLIENT_SECRET" client "$d/client.crt" "$d/client.key" "$d/ca.crt"
        issued="$issued client"
    else
        kept="$kept client"
    fi

    LLMSM_SCALER_CLIENT_CA_HASH="$(llmsm_scaler_sha256 "$d/ca.crt")" || LLMSM_SCALER_CLIENT_CA_HASH=""
    llmsm_scaler_drop_tmp
    [ -n "$LLMSM_SCALER_CLIENT_CA_HASH" ] || log_error "openssl could not digest the scaler CA."
    [ -z "$issued" ] || log_info "  issued:${issued} (openssl; the serving certificate is valid ${LLMSM_SCALER_LEAF_DAYS} days, KEDA's client certificate ${LLMSM_SCALER_CLIENT_DAYS} days, the CA ${LLMSM_SCALER_CA_DAYS} days; a re-install re-issues one within ${LLMSM_SCALER_RENEW_DAYS} days of its expiry)"
    [ -z "$kept" ] || log_info "  kept, still valid:${kept}"
    if [ "$serving" = false ]; then
        log_info "  serving certificate: issued by the OpenShift service CA into Secret $LLMSM_SCALER_TLS_SECRET"
    fi
}

# llmsm_scaler_cert_manager_manifest prints the Issuers and Certificates.
llmsm_scaler_cert_manager_manifest() {
    local labels nested san dns1 dns2
    san="$(llmsm_scaler_client_san)"
    read -r dns1 dns2 <<< "$(llmsm_scaler_dns_names)"
    labels="  labels:
    app.kubernetes.io/managed-by: llm-d-scaling-manager
    ${LLMSM_SCALER_TLS_LABEL}: cert-manager"
    # The same labels one level deeper, under secretTemplate, so cert-manager
    # stamps them on the Secrets it writes and they read as this installer's.
    nested="$(printf '%s\n' "$labels" | sed 's/^/  /')"
    cat <<EOF
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: scaling-manager-external-scaler-selfsigned
  namespace: ${LLMSM_NS}
${labels}
spec:
  selfSigned: {}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: scaling-manager-external-scaler-ca
  namespace: ${LLMSM_NS}
${labels}
spec:
  isCA: true
  commonName: llmsm-scaler-ca.${LLMSM_NS}
  secretName: ${LLMSM_SCALER_CA_SECRET}
  duration: $((LLMSM_SCALER_CA_DAYS * 24))h
  renewBefore: $((LLMSM_SCALER_RENEW_DAYS * 24))h
  privateKey:
    algorithm: RSA
    size: 2048
    rotationPolicy: Never
  issuerRef:
    name: scaling-manager-external-scaler-selfsigned
    kind: Issuer
  secretTemplate:
${nested}
---
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: scaling-manager-external-scaler-ca
  namespace: ${LLMSM_NS}
${labels}
spec:
  ca:
    secretName: ${LLMSM_SCALER_CA_SECRET}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: scaling-manager-external-scaler-client
  namespace: ${LLMSM_NS}
${labels}
spec:
  commonName: ${san}
  dnsNames:
  - ${san}
  secretName: ${LLMSM_SCALER_CLIENT_SECRET}
  duration: $((LLMSM_SCALER_CLIENT_DAYS * 24))h
  renewBefore: $((LLMSM_SCALER_RENEW_DAYS * 24))h
  usages:
  - client auth
  - digital signature
  - key encipherment
  issuerRef:
    name: scaling-manager-external-scaler-ca
    kind: Issuer
  secretTemplate:
${nested}
EOF
    [ "${ENVIRONMENT:-}" = openshift ] && return 0
    cat <<EOF
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: scaling-manager-external-scaler
  namespace: ${LLMSM_NS}
${labels}
spec:
  commonName: llmsm-external-scaler.${LLMSM_NS}.svc
  dnsNames:
  - ${dns1}
  - ${dns2}
  secretName: ${LLMSM_SCALER_TLS_SECRET}
  duration: $((LLMSM_SCALER_LEAF_DAYS * 24))h
  renewBefore: $((LLMSM_SCALER_RENEW_DAYS * 24))h
  usages:
  - server auth
  - digital signature
  - key encipherment
  issuerRef:
    name: scaling-manager-external-scaler-ca
    kind: Issuer
  secretTemplate:
${nested}
EOF
}

# llmsm_scaler_tls_provision_cert_manager applies the Issuer chain and the
# Certificates, waits for them, and sets LLMSM_SCALER_CLIENT_CA_HASH. When
# cert-manager does not accept or issue them, the default gives up on TLS for
# this run (LLMSM_SCALER_TLS_WANTED=false) and LLMSM_SCALER_TLS=true refuses.
llmsm_scaler_tls_provision_cert_manager() {
    local d out rc=0 cert certs="scaling-manager-external-scaler-ca scaling-manager-external-scaler-client" secret
    [ "${ENVIRONMENT:-}" = openshift ] || certs="$certs scaling-manager-external-scaler"
    # A render applies nothing, so cert-manager issues nothing, and KEDA's side
    # needs the issued client Secret. openssl mints them in the render itself.
    if declare -F llmsm_render_active >/dev/null && llmsm_render_active; then
        llmsm_scaler_tls_give_up "RENDER cannot render certificates cert-manager would issue (KEDA's side copies the issued Secret). Re-run the render with LLMSM_SCALER_TLS_PROVIDER=openssl." \
            "a render cannot wait for cert-manager to issue the certificates; re-run the render with LLMSM_SCALER_TLS_PROVIDER=openssl to keep TLS"
        return 0
    fi
    for cert in scaling-manager-external-scaler-selfsigned scaling-manager-external-scaler-ca; do
        llmsm_scaler_require_writable issuers.cert-manager.io "$cert"
    done
    for cert in $certs; do
        llmsm_scaler_require_writable certificates.cert-manager.io "$cert"
    done
    for secret in "$LLMSM_SCALER_CA_SECRET" "$LLMSM_SCALER_CLIENT_SECRET"; do
        llmsm_scaler_require_writable secret "$secret"
    done
    if [ "${ENVIRONMENT:-}" = openshift ]; then
        llmsm_scaler_check_openshift_serving
    else
        llmsm_scaler_require_writable secret "$LLMSM_SCALER_TLS_SECRET"
    fi

    d="$(mktemp -d)"
    llmsm_scaler_cert_manager_manifest > "$d/certs.yaml"
    # A dry run on the server goes through cert-manager's webhook: CRDs that are
    # installed with no webhook answering fail here, before anything is written.
    out="$(kubectl apply --dry-run=server -f "$d/certs.yaml" 2>&1)" || rc=$?
    if [ "$rc" != 0 ]; then
        rm -rf "$d"
        llmsm_scaler_tls_give_up "cert-manager is installed but would not accept the scaler's Issuers and Certificates in $LLMSM_NS:
$(printf '%s\n' "$out" | head -3)
Fix cert-manager (or the permission), or re-run with LLMSM_SCALER_TLS_PROVIDER=openssl." \
            "cert-manager would not accept the scaler's Issuers and Certificates in $LLMSM_NS ($(printf '%s\n' "$out" | head -1)); fix cert-manager, or re-run with LLMSM_SCALER_TLS_PROVIDER=openssl"
        return 0
    fi
    if ! kubectl apply -f "$d/certs.yaml" >/dev/null; then
        rm -rf "$d"
        llmsm_scaler_tls_give_up "Could not apply the scaler Issuers and Certificates in $LLMSM_NS." \
            "the scaler Issuers and Certificates could not be applied in $LLMSM_NS"
        return 0
    fi
    for cert in $certs; do
        if ! kubectl wait --for=condition=Ready "certificates.cert-manager.io/$cert" -n "$LLMSM_NS" --timeout=120s >/dev/null 2>&1; then
            rm -rf "$d"
            llmsm_scaler_tls_give_up "Certificate $cert in $LLMSM_NS is not Ready after 120s. See: kubectl describe certificate $cert -n $LLMSM_NS" \
                "Certificate $cert in $LLMSM_NS is not Ready after 120s (kubectl describe certificate $cert -n $LLMSM_NS)"
            return 0
        fi
    done

    llmsm_scaler_object_state secret "$LLMSM_SCALER_CLIENT_SECRET"
    llmsm_scaler_secret_field ca.crt "$d/ca.crt"
    if [ ! -s "$d/ca.crt" ]; then
        rm -rf "$d"
        llmsm_scaler_tls_give_up "Secret $LLMSM_SCALER_CLIENT_SECRET in $LLMSM_NS has no ca.crt after issuance (${LLMSM_OBJ_ERR:-empty}), so the controller would have no client CA." \
            "Secret $LLMSM_SCALER_CLIENT_SECRET in $LLMSM_NS has no ca.crt after issuance (${LLMSM_OBJ_ERR:-empty})"
        return 0
    fi
    LLMSM_SCALER_CLIENT_CA_HASH="$(llmsm_scaler_sha256 "$d/ca.crt")" || LLMSM_SCALER_CLIENT_CA_HASH=""
    rm -rf "$d"
    [ -n "$LLMSM_SCALER_CLIENT_CA_HASH" ] || log_error "openssl could not digest the scaler client CA."
    log_info "  cert-manager Certificates Ready: $certs (cert-manager renews them; the serving certificate reloads without a restart)"
    if [ "${ENVIRONMENT:-}" = openshift ]; then
        log_info "  serving certificate: issued by the OpenShift service CA into Secret $LLMSM_SCALER_TLS_SECRET"
    fi
}

# llmsm_append_scaler_tls_patch points the controller at the certificates: the
# three external-scaler flags, the two Secret mounts, and the pod-template
# annotations that record the provider and the client CA digest. On OpenShift it
# also asks the service CA for the serving certificate. Appends to the
# `patches:` list of kustomization $1, which must already exist.
llmsm_append_scaler_tls_patch() {
    local kustomization="$1" san
    san="$(llmsm_scaler_client_san)"
    cat >> "$kustomization" <<PATCH
- patch: |-
    - op: test
      path: /spec/template/spec/containers/0/name
      value: manager
    - op: add
      path: /spec/template/spec/containers/0/args/-
      value: --external-scaler-cert-path=${LLMSM_SCALER_CERT_DIR}
    - op: add
      path: /spec/template/spec/containers/0/args/-
      value: --external-scaler-client-ca=${LLMSM_SCALER_CLIENT_CA_DIR}/ca.crt
    - op: add
      path: /spec/template/spec/containers/0/args/-
      value: --external-scaler-client-san=${san}
    - op: add
      path: /spec/template/spec/containers/0/volumeMounts/-
      value: {name: scaler-tls, mountPath: ${LLMSM_SCALER_CERT_DIR}, readOnly: true}
    - op: add
      path: /spec/template/spec/containers/0/volumeMounts/-
      value: {name: scaler-client-ca, mountPath: ${LLMSM_SCALER_CLIENT_CA_DIR}, readOnly: true}
    - op: add
      path: /spec/template/spec/volumes/-
      value: {name: scaler-tls, secret: {secretName: ${LLMSM_SCALER_TLS_SECRET}, items: [{key: tls.crt, path: tls.crt}, {key: tls.key, path: tls.key}]}}
    - op: add
      path: /spec/template/spec/volumes/-
      value: {name: scaler-client-ca, secret: {secretName: ${LLMSM_SCALER_CLIENT_SECRET}, items: [{key: ca.crt, path: ca.crt}]}}
    - op: add
      path: /spec/template/metadata/annotations/scaling-manager.llmd.ai~1scaler-tls
      value: ${LLMSM_SCALER_TLS_PROVIDER_RESOLVED}
    - op: add
      path: /spec/template/metadata/annotations/scaling-manager.llmd.ai~1scaler-client-ca-sha256
      value: "${LLMSM_SCALER_CLIENT_CA_HASH}"
  target:
    kind: Deployment
    name: scaling-manager-controller-manager
PATCH
    [ "${ENVIRONMENT:-}" = openshift ] || return 0
    cat >> "$kustomization" <<PATCH
- patch: |-
    apiVersion: v1
    kind: Service
    metadata:
      name: scaling-manager-external-scaler
      annotations:
        service.beta.openshift.io/serving-cert-secret-name: ${LLMSM_SCALER_TLS_SECRET}
  target:
    kind: Service
    name: scaling-manager-external-scaler
PATCH
}

# --- KEDA's side -------------------------------------------------------------
#
# KEDA presents its client certificate through a ClusterTriggerAuthentication,
# and a ClusterTriggerAuthentication reads Secrets only in KEDA's own
# namespace (KEDA_CLUSTER_OBJECT_NAMESPACE on the operator). So the ADMIN phase
# copies the client Secret there and writes the ClusterTriggerAuthentication.
# The copy never goes to a model namespace: whoever can read it can pose as KEDA.
# A ConfigMap in LLMSM_NS (LLMSM_SCALER_KEDA_MARKER) records what was written, so the
# controller phase, which cannot read KEDA's namespace, can tell KEDA is ready.
#
# One KEDA fact decides the scalerAddress (pkg/scalers/external_scaler.go,
# v2.18): KEDA keeps ONE gRPC connection per scalerAddress string, built with the
# TLS settings of the first ScaledObject that dials it, and never closes it while
# KEDA runs. A ScaledObject's TLS settings therefore do nothing on an address KEDA
# already dialled in plaintext. TLS ScaledObjects use the Service's short name,
# scaling-manager-external-scaler.<ns>.svc:9090; plaintext ones keep
# scaling-manager-external-scaler.<ns>.svc.cluster.local:9090. Same Service, a different
# pool key: a fresh connection, and no KEDA restart. The serving certificate
# names both hosts.

LLMSM_SCALER_KEDA_MARKER=scaling-manager-scaler-keda-auth

# llmsm_scaler_mixed_risk echoes, for a message, what a ScaledObject whose
# trigger does not match the scaler's mode risks. It is not simply "broken":
# on a real cluster, one on the TLS address without the authenticationRef was
# Ready and scaled, because it rode the connection an authenticated one had
# opened. Which one dials first after KEDA starts decides it.
llmsm_scaler_mixed_risk() {
    printf '%s' "This is unsupported, and depends on order. KEDA keeps ONE connection per scaler address, opened with the settings of whichever ScaledObject dials first after KEDA starts. These may work now only because another ScaledObject opened that connection. After a KEDA operator restart, if one of them dials first, the connection is opened wrong and EVERY ScaledObject on that address can stop scaling, until they are fixed and KEDA is restarted again."
}

# llmsm_scaler_keda_auth_name echoes the name of the ClusterTriggerAuthentication,
# and of the Secret copy it reads, for the controller in namespace $1 (LLMSM_NS).
llmsm_scaler_keda_auth_name() {
    printf 'scaling-manager-external-scaler-%s\n' "${1:-$LLMSM_NS}"
}

# llmsm_scaler_address echoes the scalerAddress for the scaler in namespace $1:
# the TLS form when $2 is tls, the plaintext form otherwise.
llmsm_scaler_address() {
    if [ "${2:-plaintext}" = tls ]; then
        printf 'scaling-manager-external-scaler.%s.svc:9090\n' "$1"
    else
        printf 'scaling-manager-external-scaler.%s.svc.cluster.local:9090\n' "$1"
    fi
}

# llmsm_scaler_mode_of echoes how the controller in namespace $1 serves the scaler:
#   tls        mutual TLS this installer provisioned
#   manual     TLS flags set by hand (no KEDA side from this installer)
#   plaintext  no TLS, or no controller there
#   unknown    its Deployment could not be read
llmsm_scaler_mode_of() {
    local out rc=0
    out="$(kubectl get deployment scaling-manager-controller-manager -n "$1" -o json 2>&1)" || rc=$?
    if [ "$rc" != 0 ]; then
        if llmsm_scaler_not_found "$out"; then echo plaintext; else echo unknown; fi
        return 0
    fi
    printf '%s' "$out" | jq -r --arg a "$LLMSM_SCALER_TLS_ANNOTATION" '
        ([.spec.template.spec.containers[]? | select(.name == "manager") | (.args // [])[]
          | select(startswith("--external-scaler-cert-path"))] | length) as $f
        | (.spec.template.metadata.annotations[$a] // "") as $p
        | if $f == 0 then "plaintext" elif $p != "" then "tls" else "manual" end' 2>/dev/null || echo unknown
}

# llmsm_scaler_address_mode echoes the mode a ScaledObject for the scaler in
# namespace $1 is written in: tls or plaintext, as llmsm_scaler_mode_of reads it
# (hand-set TLS is plaintext here: whoever set it owns KEDA's side). When the
# Deployment cannot be read, an explicit LLMSM_SCALER_TLS=true or false says
# which; with neither it echoes unknown, and the caller warns.
llmsm_scaler_address_mode() {
    local mode
    mode="$(llmsm_scaler_mode_of "$1")"
    if [ "$mode" = unknown ]; then
        case "${LLMSM_SCALER_TLS:-}" in
            true) mode=tls ;;
            false) mode=plaintext ;;
        esac
    fi
    case "$mode" in
        tls|unknown) printf '%s\n' "$mode" ;;
        *) echo plaintext ;;
    esac
}

# llmsm_scaler_trigger_auth_json echoes the authenticationRef a trigger naming the
# TLS scaler in namespace $1 carries.
llmsm_scaler_trigger_auth_json() {
    jq -nc --arg n "$(llmsm_scaler_keda_auth_name "$1")" '{name: $n, kind: "ClusterTriggerAuthentication"}'
}

# llmsm_scaler_install_puts_keda succeeds when this install itself installs KEDA,
# which deploy_scaler_backend does before the controller: on kind, and on
# Kubernetes with KEDA_HELM_INSTALL=true.
llmsm_scaler_install_puts_keda() {
    [ "${SCALER_BACKEND:-keda}" = keda ] || return 1
    case "${ENVIRONMENT:-}" in
        kind-emulator) return 0 ;;
        kubernetes) [ "${KEDA_HELM_INSTALL:-false}" = true ] ;;
        *) return 1 ;;
    esac
}

# llmsm_scaler_keda_run_ns sets LLMSM_SCALER_KEDA_RUN_NS to the namespace KEDA's
# operator runs in, or to empty when it is not known. Only sources a tenant
# cannot choose count: KEDA_NAMESPACE set by the caller (install.sh defaults it
# to keda-system, so only when KEDA_NAMESPACE_EXPLICIT=true), the namespace
# serving the external-metrics APIService, or the namespace this install puts
# KEDA in. Never the NetworkPolicy in LLMSM_NS, which that namespace's admin can
# edit.
llmsm_scaler_keda_run_ns() {
    local kns=""
    LLMSM_SCALER_KEDA_RUN_NS=""
    if [ "${KEDA_NAMESPACE_EXPLICIT:-false}" = true ]; then
        kns="${KEDA_NAMESPACE:-}"
    elif declare -F llmsm_keda_apiservice_ns >/dev/null && kns="$(llmsm_keda_apiservice_ns 2>/dev/null)"; then
        :
    elif llmsm_scaler_install_puts_keda; then
        kns="${KEDA_NAMESPACE:-}"
    else
        kns=""
    fi
    LLMSM_SCALER_KEDA_RUN_NS="$kns"
}

# llmsm_scaler_keda_object_ns sets LLMSM_SCALER_KEDA_OBJ_NS to the namespace KEDA
# reads a ClusterTriggerAuthentication's Secrets from, or to empty when it is
# not known (LLMSM_SCALER_KEDA_OBJ_WHY says why). The private key is copied there,
# so it starts from llmsm_scaler_keda_run_ns. KEDA's operator may move it with
# KEDA_CLUSTER_OBJECT_NAMESPACE; when the Deployments there cannot be read, that
# is unknown, not unset, unless KEDA_NAMESPACE was given explicitly.
llmsm_scaler_keda_object_ns() {
    local kns rc=0 env=""
    LLMSM_SCALER_KEDA_OBJ_NS=""
    LLMSM_SCALER_KEDA_OBJ_WHY=""
    llmsm_scaler_keda_run_ns
    kns="$LLMSM_SCALER_KEDA_RUN_NS"
    if [ -z "$kns" ]; then
        LLMSM_SCALER_KEDA_OBJ_WHY="where KEDA runs is not known. Set KEDA_NAMESPACE=<the namespace KEDA runs in>"
        return 0
    fi
    kc_capture get deployments -n "$kns" -o json || rc=$?
    if [ "$rc" != 0 ]; then
        if [ "${KEDA_NAMESPACE_EXPLICIT:-false}" = true ]; then
            LLMSM_SCALER_KEDA_OBJ_NS="$kns"
            return 0
        fi
        LLMSM_SCALER_KEDA_OBJ_WHY="the Deployments in $kns, where KEDA runs, cannot be read ($(printf '%s\n' "$KC_ERR" | head -1)), so whether KEDA_CLUSTER_OBJECT_NAMESPACE moves its Secrets elsewhere is unknown. Grant list on deployments there, or set KEDA_NAMESPACE=$kns"
        return 0
    fi
    # A value from a fieldRef is the operator's own namespace, which is kns.
    env="$(printf '%s' "$KC_OUT" | jq -r '[.items[]?.spec.template.spec.containers[]?.env[]?
        | select(.name == "KEDA_CLUSTER_OBJECT_NAMESPACE") | .value // empty] | first // ""' 2>/dev/null)" || env=""
    LLMSM_SCALER_KEDA_OBJ_NS="${env:-$kns}"
}

# llmsm_scaler_keda_api echoes present when the ClusterTriggerAuthentication kind
# is served, absent when it is not, unknown when discovery failed.
llmsm_scaler_keda_api() {
    local out rc=0
    out="$(kubectl get --raw /apis/keda.sh/v1alpha1 2>&1)" || rc=$?
    if [ "$rc" != 0 ]; then
        if llmsm_scaler_not_found "$out"; then echo absent; else echo unknown; fi
        return 0
    fi
    if printf '%s' "$out" | jq -e '[.resources[]?.name] | index("clustertriggerauthentications")' >/dev/null 2>&1; then
        echo present
    else
        echo absent
    fi
}

# llmsm_scaler_keda_auth_check is everything llmsm_scaler_keda_auth_apply checks
# before it writes, so the install can run it before it writes anything at all:
# KEDA's API, where KEDA reads its Secrets, the rights to write there, and that
# the copy, the ClusterTriggerAuthentication and the marker are absent or this
# installer's. A foreign or unreadable one is a refusal. Anything else that
# keeps KEDA's side from being written goes through llmsm_scaler_keda_not_ready
# (a refusal when LLMSM_SCALER_TLS=true, a warning otherwise) and returns 1.
# Sets LLMSM_SCALER_KEDA_OBJ_NS.
llmsm_scaler_keda_auth_check() {
    local name api out rc=0
    name="$(llmsm_scaler_keda_auth_name)"
    api="$(llmsm_scaler_keda_api)"
    if [ "$api" != present ]; then
        llmsm_scaler_keda_not_ready "KEDA's ClusterTriggerAuthentication kind is $api on this cluster (is KEDA installed?)"
        return 1
    fi
    llmsm_scaler_keda_object_ns
    if [ -z "$LLMSM_SCALER_KEDA_OBJ_NS" ]; then
        llmsm_scaler_keda_not_ready "$LLMSM_SCALER_KEDA_OBJ_WHY"
        return 1
    fi
    if ! kubectl auth can-i create clustertriggerauthentications.keda.sh >/dev/null 2>&1 \
        || ! kubectl auth can-i create secrets -n "$LLMSM_SCALER_KEDA_OBJ_NS" >/dev/null 2>&1; then
        llmsm_scaler_keda_not_ready "you may not create ClusterTriggerAuthentications, or Secrets in $LLMSM_SCALER_KEDA_OBJ_NS"
        return 1
    fi
    # The copy and the ClusterTriggerAuthentication: absent or ours only.
    LLMSM_NS="$LLMSM_SCALER_KEDA_OBJ_NS" llmsm_scaler_object_state secret "$name"
    case "$LLMSM_OBJ_STATE" in
        absent|ours) ;;
        unreadable) log_error "Cannot read Secret $name in $LLMSM_SCALER_KEDA_OBJ_NS ($LLMSM_OBJ_ERR). Grant get on secrets there, or set LLMSM_SCALER_TLS=false." ;;
        *) log_error "Secret $name in $LLMSM_SCALER_KEDA_OBJ_NS was not created by this installer (no $LLMSM_SCALER_TLS_LABEL label); refusing to overwrite it. Remove or rename it, or set LLMSM_SCALER_TLS=false." ;;
    esac
    out="$(kubectl get clustertriggerauthentications.keda.sh "$name" -o json 2>&1)" || rc=$?
    if [ "$rc" = 0 ]; then
        printf '%s' "$out" | jq -e --arg k "$LLMSM_SCALER_TLS_LABEL" '.metadata.labels[$k] // empty' >/dev/null 2>&1 \
            || log_error "ClusterTriggerAuthentication $name was not created by this installer (no $LLMSM_SCALER_TLS_LABEL label); refusing to overwrite it. Remove or rename it, or set LLMSM_SCALER_TLS=false."
    elif ! llmsm_scaler_not_found "$out"; then
        log_error "Cannot read ClusterTriggerAuthentication $name ($(printf '%s\n' "$out" | head -1)). Grant get on clustertriggerauthentications.keda.sh, or set LLMSM_SCALER_TLS=false."
    fi
    # The marker: the controller phase trusts what it says, so a same-named
    # ConfigMap someone else wrote is never taken over.
    llmsm_scaler_object_state configmap "$LLMSM_SCALER_KEDA_MARKER"
    case "$LLMSM_OBJ_STATE" in
        absent|ours) ;;
        unreadable) log_error "Cannot read ConfigMap $LLMSM_SCALER_KEDA_MARKER in $LLMSM_NS ($LLMSM_OBJ_ERR). Grant get on configmaps there, or set LLMSM_SCALER_TLS=false." ;;
        *) log_error "ConfigMap $LLMSM_SCALER_KEDA_MARKER in $LLMSM_NS was not created by this installer (no $LLMSM_SCALER_TLS_LABEL label); refusing to overwrite it. Remove or rename it, or set LLMSM_SCALER_TLS=false." ;;
    esac
    return 0
}

# llmsm_scaler_keda_auth_apply is the admin step: copy the client Secret to KEDA's
# namespace, write the ClusterTriggerAuthentication, and record both in the
# marker ConfigMap. Runs when TLS is wanted (LLMSM_SCALER_TLS_WANTED), after KEDA
# is installed. A refusal when TLS was asked for explicitly; a warning and no
# change when it is only the default, so the controller stays plaintext.
llmsm_scaler_keda_auth_apply() {
    [ "${LLMSM_SCALER_TLS_WANTED:-false}" = true ] || return 0
    local name d obj_state sha not_after ca_src
    name="$(llmsm_scaler_keda_auth_name)"
    llmsm_scaler_keda_auth_check || return 0

    llmsm_scaler_tmp_begin
    d="$LLMSM_SCALER_TMPD"
    llmsm_scaler_object_state secret "$LLMSM_SCALER_CLIENT_SECRET"
    if [ "$LLMSM_OBJ_STATE" != ours ]; then
        llmsm_scaler_drop_tmp
        llmsm_scaler_keda_not_ready "KEDA's client certificate (Secret $LLMSM_SCALER_CLIENT_SECRET in $LLMSM_NS) is $LLMSM_OBJ_STATE${LLMSM_OBJ_ERR:+: $LLMSM_OBJ_ERR}"
        return 0
    fi
    llmsm_scaler_secret_field tls.crt "$d/tls.crt"
    llmsm_scaler_secret_field tls.key "$d/tls.key"
    llmsm_scaler_secret_field ca.crt "$d/ca.crt"
    ca_src="the installer's CA"
    if [ "${ENVIRONMENT:-}" = openshift ]; then
        # The serving certificate is the service CA's, so KEDA must trust that.
        kubectl get configmap openshift-service-ca.crt -n "$LLMSM_NS" -o jsonpath='{.data.service-ca\.crt}' > "$d/ca.crt" 2>/dev/null || : > "$d/ca.crt"
        ca_src="the OpenShift service CA"
    fi
    if [ ! -s "$d/tls.crt" ] || [ ! -s "$d/tls.key" ] || [ ! -s "$d/ca.crt" ]; then
        llmsm_scaler_drop_tmp
        llmsm_scaler_keda_not_ready "KEDA's client certificate, key or ${ca_src} could not be read in $LLMSM_NS"
        return 0
    fi

    # The copy, read again: whether it changed decides the KEDA restart notice.
    # llmsm_scaler_keda_auth_check settled the ClusterTriggerAuthentication and
    # the marker.
    LLMSM_NS="$LLMSM_SCALER_KEDA_OBJ_NS" llmsm_scaler_object_state secret "$name"
    obj_state="$LLMSM_OBJ_STATE"
    local changed=false
    if [ "$obj_state" = ours ]; then
        # KEDA builds a scaler connection's TLS once and keeps it until KEDA
        # restarts, so a changed copy does not reach a connection already open.
        llmsm_scaler_secret_field tls.crt "$d/old.crt"
        llmsm_scaler_secret_field ca.crt "$d/old.ca"
        cmp -s "$d/old.crt" "$d/tls.crt" && cmp -s "$d/old.ca" "$d/ca.crt" || changed=true
    fi
    case "$obj_state" in
        absent|ours) ;;
        unreadable) llmsm_scaler_drop_tmp; log_error "Cannot read Secret $name in $LLMSM_SCALER_KEDA_OBJ_NS ($LLMSM_OBJ_ERR). Grant get on secrets there, or set LLMSM_SCALER_TLS=false." ;;
        *) llmsm_scaler_drop_tmp; log_error "Secret $name in $LLMSM_SCALER_KEDA_OBJ_NS was not created by this installer (no $LLMSM_SCALER_TLS_LABEL label); refusing to overwrite it. Remove or rename it, or set LLMSM_SCALER_TLS=false." ;;
    esac

    # Each of the three is written only when it differs from what is there: a
    # re-install with nothing to renew writes nothing on KEDA's side.
    if [ "$obj_state" = ours ] && [ "$changed" = false ]; then
        :
    else
        jq -n --arg n "$name" --arg ns "$LLMSM_SCALER_KEDA_OBJ_NS" --arg l "$LLMSM_SCALER_TLS_LABEL" --arg src "$LLMSM_NS" \
            --rawfile crt "$d/tls.crt" --rawfile key "$d/tls.key" --rawfile ca "$d/ca.crt" \
            '{apiVersion: "v1", kind: "Secret", type: "Opaque",
              metadata: {name: $n, namespace: $ns,
                         labels: {"app.kubernetes.io/managed-by": "llm-d-scaling-manager", ($l): "keda-client"},
                         annotations: {"scaling-manager.llmd.ai/copied-from": ($src + "/scaling-manager-external-scaler-client-tls")}},
              data: {"tls.crt": ($crt | @base64), "tls.key": ($key | @base64), "ca.crt": ($ca | @base64)}}' \
            | kubectl apply --server-side --force-conflicts --field-manager=llm-d-scaling-manager -f - >/dev/null \
            || { llmsm_scaler_drop_tmp; log_error "Could not write Secret $name in $LLMSM_SCALER_KEDA_OBJ_NS."; }
    fi
    local want cur rc_cur=0
    want="$(jq -nc --arg n "$name" --arg l "$LLMSM_SCALER_TLS_LABEL" --arg s "$name" \
        '{apiVersion: "keda.sh/v1alpha1", kind: "ClusterTriggerAuthentication",
          metadata: {name: $n, labels: {"app.kubernetes.io/managed-by": "llm-d-scaling-manager", ($l): "keda-client"}},
          spec: {secretTargetRef: [
            {parameter: "caCert", name: $s, key: "ca.crt"},
            {parameter: "tlsClientCert", name: $s, key: "tls.crt"},
            {parameter: "tlsClientKey", name: $s, key: "tls.key"}]}}')"
    cur="$(kubectl get clustertriggerauthentications.keda.sh "$name" -o json 2>/dev/null)" || rc_cur=$?
    if [ "$rc_cur" != 0 ] || ! llmsm_scaler_same_object "$cur" "$want" spec; then
        printf '%s' "$want" \
            | kubectl apply --server-side --force-conflicts --field-manager=llm-d-scaling-manager -f - >/dev/null \
            || { llmsm_scaler_drop_tmp; log_error "Could not write ClusterTriggerAuthentication $name."; }
    fi

    sha="$(llmsm_scaler_sha256 "$d/tls.crt")" || sha=""
    not_after="$(openssl x509 -in "$d/tls.crt" -noout -enddate 2>/dev/null | sed 's/^notAfter=//')" || not_after=""
    # The certificate itself (public), so the controller phase checks its
    # expiry with openssl rather than parsing a date. clientSan is the identity
    # it carries: the controller phase compares it with the one it pins.
    want="$(jq -nc --arg ns "$LLMSM_NS" --arg l "$LLMSM_SCALER_TLS_LABEL" --arg m "$LLMSM_SCALER_KEDA_MARKER" \
        --arg cta "$name" --arg on "$LLMSM_SCALER_KEDA_OBJ_NS" --arg sha "$sha" --rawfile crt "$d/tls.crt" \
        --arg cas "$(llmsm_scaler_sha256 "$d/ca.crt")" --arg san "$(llmsm_scaler_cert_sans "$d/tls.crt" | sed 's/ *$//')" \
        '{apiVersion: "v1", kind: "ConfigMap",
          metadata: {name: $m, namespace: $ns, labels: {"app.kubernetes.io/managed-by": "llm-d-scaling-manager", ($l): "keda-client"}},
          data: {clusterTriggerAuthentication: $cta, secretNamespace: $on, clientCertSha256: $sha, clientCert: $crt, caSha256: $cas, clientSan: $san}}')"
    rc_cur=0
    cur="$(kubectl get configmap "$LLMSM_SCALER_KEDA_MARKER" -n "$LLMSM_NS" -o json 2>/dev/null)" || rc_cur=$?
    if [ "$rc_cur" != 0 ] || ! llmsm_scaler_same_object "$cur" "$want" data; then
        printf '%s' "$want" \
            | kubectl apply --server-side --force-conflicts --field-manager=llm-d-scaling-manager -f - >/dev/null \
            || { llmsm_scaler_drop_tmp; log_error "Could not write ConfigMap $LLMSM_SCALER_KEDA_MARKER in $LLMSM_NS."; }
    fi
    llmsm_scaler_drop_tmp
    LLMSM_SCALER_KEDA_READY=true
    log_success "KEDA's side of the scaler TLS: ClusterTriggerAuthentication $name, reading Secret $name in $LLMSM_SCALER_KEDA_OBJ_NS (client certificate expires $not_after; KEDA trusts ${ca_src})"
    if [ "$changed" = true ]; then
        log_warning "KEDA's client certificate or the CA it trusts changed. KEDA keeps the TLS of a connection it already has until it restarts, so restart KEDA's operator now:  $(llmsm_scaler_keda_restart_cmd)"
    elif [ "$obj_state" = absent ] && llmsm_scaler_keda_operator_may_run && llmsm_scaler_ca_was_kept; then
        # The uninstall kept the CA for exactly this (llmsm_scaler_tls_undeploy).
        log_info "KEDA's side was written for the first time while KEDA's operator is running. The scaler's CA was kept from an earlier install here (Secret $LLMSM_SCALER_CA_SECRET), and this install's certificates are signed by it, so a connection KEDA's operator still holds from then keeps working: no restart needed."
    elif [ "$obj_state" = absent ] && llmsm_scaler_keda_operator_may_run; then
        # A first write while KEDA runs: after an uninstall and reinstall, KEDA
        # still holds the connection it built with the old certificate.
        # A first install and a reinstall look the same here: an uninstall
        # removes everything it wrote, the marker included, and KEDA does not
        # show which connections it holds. So both get the warning, and the
        # controller's log tells them apart after the fact
        # (llmsm_scaler_keda_conn_state, run by verify-deployment and after a
        # ScaledObject move).
        local tls_rows tls_rc=0
        tls_rows="$(llmsm_scaler_plaintext_scaledobjects plaintext)" || tls_rc=$?
        if [ "$tls_rc" != 0 ] || [ -n "$tls_rows" ]; then
            log_warning "KEDA's side was written for the first time while KEDA's operator is running, and ScaledObjects already use the scaler's TLS address (or they cannot be listed). If this scaler was installed here before (an uninstall and reinstall), KEDA still holds the old certificate and every handshake fails until it restarts. Then restart KEDA's operator:  $(llmsm_scaler_keda_restart_cmd)"
        else
            log_warning "KEDA's side was written for the first time while KEDA's operator is running. The installer cannot tell a first install from a reinstall. If this namespace had the scaler before, KEDA keeps the old client certificate until its operator restarts, and without a restart the HPAs stay unwired (on the CPU default). On a first install, nothing to do. After the install,  $(llmsm_make_cmd verify-deployment)  tells which. To be safe now, restart KEDA's operator:  $(llmsm_scaler_keda_restart_cmd)"
        fi
    fi
}

# llmsm_scaler_ca_was_kept succeeds when the scaler CA in LLMSM_NS is this
# installer's and an uninstall kept it for KEDA (LLMSM_SCALER_CA_KEPT_ANNOTATION).
llmsm_scaler_ca_was_kept() {
    llmsm_scaler_object_state secret "$LLMSM_SCALER_CA_SECRET"
    [ "$LLMSM_OBJ_STATE" = ours ] || return 1
    printf '%s' "$LLMSM_OBJ_JSON" | jq -e --arg a "$LLMSM_SCALER_CA_KEPT_ANNOTATION" '.metadata.annotations[$a] // empty' >/dev/null 2>&1
}

# llmsm_scaler_keda_operator_ns echoes the namespace KEDA's operator runs in,
# for a message: llmsm_scaler_keda_run_ns, else the namespace KEDA's Secrets are
# in (from this run, or from the marker), else a placeholder.
llmsm_scaler_keda_operator_ns() {
    [ -n "${LLMSM_SCALER_KEDA_RUN_NS:-}" ] || llmsm_scaler_keda_run_ns
    printf '%s\n' "${LLMSM_SCALER_KEDA_RUN_NS:-${LLMSM_SCALER_KEDA_OBJ_NS:-${LLMSM_SCALER_KEDA_MARKER_NS:-<KEDA namespace>}}}"
}

# llmsm_scaler_keda_restart_cmd echoes the command that restarts KEDA's operator,
# the Deployment KEDA's own manifests and Helm chart name keda-operator.
llmsm_scaler_keda_restart_cmd() {
    printf 'kubectl rollout restart deployment/keda-operator -n %s\n' "$(llmsm_scaler_keda_operator_ns)"
}

# llmsm_scaler_keda_stale_act handles a KEDA that still holds a connection from
# before a reinstall (LLMSM_SCALER_KEDA_CONN=stale): it names the ScaledObjects
# ($1, one per line, <namespace>/<name> first) that stay unwired, and the exact
# restart. KEDA is shared, so it restarts the operator only with
# KEDA_RESTART=true; otherwise the caller reports a failure. Returns 0
# when it restarted KEDA, 1 when it did not.
llmsm_scaler_keda_stale_act() {
    local rows="$1" ns
    ns="$(llmsm_scaler_keda_operator_ns)"
    log_warning "=================================================================="
    log_warning "KEDA's operator still holds its connection to the scaler from before a reinstall, so every call it makes is refused. These ScaledObjects stay unwired, with their HPAs on the CPU default and the scaling manager's decisions reaching nothing, until KEDA's operator restarts:"
    printf '%s\n' "$rows" | sed '/^$/d; s/ .*//; s/^/    /' >&2
    llmsm_scaler_keda_conn_report || true
    if [ "${KEDA_RESTART:-false}" = true ]; then
        if kubectl rollout restart deployment/keda-operator -n "$ns" >/dev/null 2>&1; then
            log_success "KEDA_RESTART=true: restarted KEDA's operator in $ns. It wires the ScaledObjects again as it starts."
            log_warning "=================================================================="
            return 0
        fi
        log_warning "  KEDA_RESTART=true, but restarting KEDA's operator in $ns failed. Run it yourself:  $(llmsm_scaler_keda_restart_cmd)"
    else
        log_warning "  Not restarted: KEDA's operator is shared with everything else that uses KEDA. Nothing scales until it restarts, so this is reported as a failure. Restart it yourself with the command above, or run this again with KEDA_RESTART=true."
    fi
    log_warning "=================================================================="
    return 1
}

# llmsm_scaler_keda_restart_after_undeploy handles KEDA's pooled connection when
# the uninstall removed KEDA's side of the scaler TLS while ScaledObjects used
# it (LLMSM_SCALER_KEDA_RESTART_WANTED). Call it after the ScaledObjects are gone.
# KEDA is shared, so the restart is opt-in (KEDA_RESTART=true). Without it, a
# reinstall finds the stale connection once its ScaledObjects dial, and names
# the restart then (llmsm_scaler_keda_stale_act).
llmsm_scaler_keda_restart_after_undeploy() {
    [ "${LLMSM_SCALER_KEDA_RESTART_WANTED:-false}" = true ] || return 0
    local ns rows rc=0
    ns="$(llmsm_scaler_keda_operator_ns)"
    if [ "${KEDA_RESTART:-false}" != true ]; then
        if [ "${LLMSM_SCALER_CA_KEPT:-false}" = true ]; then
            log_info "KEDA's operator is not restarted (it is shared; KEDA_RESTART=true lets the uninstall do it). It keeps its connection to the removed scaler, which trusts the kept CA, so a reinstall in $LLMSM_NS needs no restart. To restart it now:  $(llmsm_scaler_keda_restart_cmd)"
        else
            log_info "KEDA's operator is not restarted (it is shared; KEDA_RESTART=true lets the uninstall do it). It keeps its connection to the removed scaler. If you install this scaler in $LLMSM_NS again, the install finds that connection and prints the restart. To restart it now:  $(llmsm_scaler_keda_restart_cmd)"
        fi
        return 0
    fi
    # Only with no ScaledObject left on the TLS address. One that stays (adopted,
    # or made by someone else) would have KEDA rebuild the pooled address on
    # restart with the ClusterTriggerAuthentication gone: empty auth, plaintext.
    # Then the restart belongs after the reinstall, as it always did.
    rows="$(llmsm_scaler_plaintext_scaledobjects plaintext)" || rc=$?
    if [ "$rc" != 0 ] || [ -n "$rows" ]; then
        log_warning "ScaledObjects still use the scaler's TLS address$([ "$rc" != 0 ] && printf ' (or they cannot be listed)'), so KEDA's operator is not restarted now. If you install this scaler in $LLMSM_NS again, restart it after the install:  $(llmsm_scaler_keda_restart_cmd)"
        return 0
    fi
    if kubectl rollout restart deployment/keda-operator -n "$ns" >/dev/null 2>&1; then
        log_success "Restarted KEDA's operator in $ns, so a reinstall here gets a new connection to the scaler."
        # Nothing trusts the kept CA any more (llmsm_scaler_tls_undeploy).
        if [ "${LLMSM_SCALER_CA_KEPT:-false}" = true ]; then
            if kubectl delete secret "$LLMSM_SCALER_CA_SECRET" -n "$LLMSM_NS" --ignore-not-found >/dev/null 2>&1; then
                LLMSM_SCALER_CA_KEPT=false
                log_info "Removed the external scaler's CA, Secret $LLMSM_SCALER_CA_SECRET in $LLMSM_NS, kept until KEDA restarted."
            else
                log_warning "Could not remove the external scaler's CA, Secret $LLMSM_SCALER_CA_SECRET in $LLMSM_NS:  kubectl delete secret $LLMSM_SCALER_CA_SECRET -n $LLMSM_NS"
            fi
        fi
    else
        log_warning "Could not restart KEDA's operator in $ns. Before installing this scaler in $LLMSM_NS again, restart it:  $(llmsm_scaler_keda_restart_cmd)"
    fi
}

# llmsm_scaler_keda_operator_may_run succeeds unless KEDA's operator Deployment is
# known to be absent: a Deployment that cannot be read may be running.
llmsm_scaler_keda_operator_may_run() {
    local ns rc=0
    ns="$(llmsm_scaler_keda_operator_ns)"
    kc_capture get deployment keda-operator -n "$ns" -o name || rc=$?
    [ "$rc" = 0 ] && return 0
    ! llmsm_scaler_not_found "$KC_ERR"
}

# llmsm_scaler_keda_not_ready reports that KEDA's side could not be written: a
# refusal when LLMSM_SCALER_TLS=true was asked for, a warning otherwise.
llmsm_scaler_keda_not_ready() {
    LLMSM_SCALER_KEDA_READY=false
    if [ "${LLMSM_SCALER_TLS:-}" = true ]; then
        log_error "LLMSM_SCALER_TLS=true, but KEDA's side cannot be written: $1."
    fi
    log_warning "The external scaler stays on plaintext gRPC: KEDA's side of TLS cannot be written ($1)."
}

# llmsm_scaler_keda_marker_state sets LLMSM_SCALER_KEDA_READY from the marker the
# admin phase leaves in LLMSM_NS, for the controller phase: true when it names
# this install's ClusterTriggerAuthentication and its client certificate has not
# expired. LLMSM_SCALER_KEDA_WHY says why not, LLMSM_SCALER_KEDA_STALE that KEDA
# holds an older (still valid) client certificate than the one in LLMSM_NS, and
# LLMSM_SCALER_KEDA_NOT_AFTER when the one KEDA holds expires, and
# LLMSM_SCALER_KEDA_EXPIRING whether that is within LLMSM_SCALER_RENEW_DAYS.
# LLMSM_SCALER_KEDA_CA_STALE is true when KEDA trusts another CA than the one
# that signs the serving certificate now, and unknown when the marker does not
# record it (an older installer wrote it), unread when the current CA cannot be
# read, and false when the two match. LLMSM_SCALER_KEDA_MARKER_NS is where
# the marker says KEDA's copy went.
#
# LLMSM_SCALER_KEDA_SAN_STALE is true when the identity KEDA's certificate carries
# is not $1, the one the controller pins (default llmsm_scaler_client_san): the
# controller then rejects every handshake, so KEDA's side is not ready.
llmsm_scaler_keda_marker_state() {
    local out rc=0 cta sha cur d pinned="${1:-}" held
    [ -n "$pinned" ] || pinned="$(llmsm_scaler_client_san)"
    LLMSM_SCALER_KEDA_READY=false
    LLMSM_SCALER_KEDA_WHY=""
    LLMSM_SCALER_KEDA_SAN_STALE=false
    LLMSM_SCALER_KEDA_STALE=false
    LLMSM_SCALER_KEDA_NOT_AFTER=""
    LLMSM_SCALER_KEDA_EXPIRING=false
    LLMSM_SCALER_KEDA_CA_STALE=false
    LLMSM_SCALER_KEDA_MARKER_NS=""
    out="$(kubectl get configmap "$LLMSM_SCALER_KEDA_MARKER" -n "$LLMSM_NS" -o json 2>&1)" || rc=$?
    if [ "$rc" != 0 ]; then
        if llmsm_scaler_not_found "$out"; then
            LLMSM_SCALER_KEDA_WHY="the cluster admin has not written KEDA's side yet"
        else
            LLMSM_SCALER_KEDA_WHY="ConfigMap $LLMSM_SCALER_KEDA_MARKER in $LLMSM_NS could not be read"
        fi
        return 0
    fi
    cta="$(printf '%s' "$out" | jq -r '.data.clusterTriggerAuthentication // ""')"
    sha="$(printf '%s' "$out" | jq -r '.data.clientCertSha256 // ""')"
    LLMSM_SCALER_KEDA_MARKER_NS="$(printf '%s' "$out" | jq -r '.data.secretNamespace // ""')"
    if [ "$cta" != "$(llmsm_scaler_keda_auth_name)" ]; then
        LLMSM_SCALER_KEDA_WHY="ConfigMap $LLMSM_SCALER_KEDA_MARKER names ClusterTriggerAuthentication '${cta}', not $(llmsm_scaler_keda_auth_name)"
        return 0
    fi
    d="$(mktemp -d)"
    printf '%s' "$out" | jq -r '.data.clientCert // ""' > "$d/keda.crt"
    LLMSM_SCALER_KEDA_NOT_AFTER="$(openssl x509 -in "$d/keda.crt" -noout -enddate 2>/dev/null | sed 's/^notAfter=//')"
    if ! openssl x509 -in "$d/keda.crt" -noout -checkend 0 >/dev/null 2>&1; then
        rm -rf "$d"
        LLMSM_SCALER_KEDA_WHY="the client certificate KEDA holds has expired or cannot be read (${LLMSM_SCALER_KEDA_NOT_AFTER:-no date})"
        return 0
    fi
    # The identity KEDA presents: recorded by the admin phase, or read off the
    # certificate when an older installer wrote the marker.
    held="$(printf '%s' "$out" | jq -r '.data.clientSan // ""')"
    [ -n "$held" ] || held="$(llmsm_scaler_cert_sans "$d/keda.crt" | sed 's/ *$//')"
    if [ "$held" != "$pinned" ]; then
        rm -rf "$d"
        LLMSM_SCALER_KEDA_SAN_STALE=true
        LLMSM_SCALER_KEDA_WHY="KEDA's client certificate carries the identity '${held}', but the controller accepts only '${pinned}', so it rejects every handshake. A cluster admin runs:  $(llmsm_make_cmd setup-prereqs "LLMSM_SCALER_CLIENT_SAN=${pinned}")  and then restarts KEDA's operator:  $(llmsm_scaler_keda_restart_cmd)"
        return 0
    fi
    LLMSM_SCALER_KEDA_READY=true
    openssl x509 -in "$d/keda.crt" -noout -checkend $((LLMSM_SCALER_RENEW_DAYS * 86400)) >/dev/null 2>&1 \
        || LLMSM_SCALER_KEDA_EXPIRING=true
    llmsm_scaler_object_state secret "$LLMSM_SCALER_CLIENT_SECRET"
    llmsm_scaler_secret_field tls.crt "$d/tls.crt"
    cur=""
    [ -s "$d/tls.crt" ] && cur="$(llmsm_scaler_sha256 "$d/tls.crt")"
    rm -rf "$d"
    [ -z "$cur" ] || [ "$cur" = "$sha" ] || LLMSM_SCALER_KEDA_STALE=true
    # The CA KEDA trusts against the one that signs the serving certificate
    # now. The OpenShift service CA rotates on its own.
    local cas want
    cas="$(printf '%s' "$out" | jq -r '.data.caSha256 // ""')"
    d="$(mktemp -d)"
    if [ "${ENVIRONMENT:-}" = openshift ]; then
        kubectl get configmap openshift-service-ca.crt -n "$LLMSM_NS" -o jsonpath='{.data.service-ca\.crt}' > "$d/ca" 2>/dev/null || : > "$d/ca"
    else
        llmsm_scaler_secret_field ca.crt "$d/ca"
    fi
    want=""
    [ -s "$d/ca" ] && want="$(llmsm_scaler_sha256 "$d/ca")"
    rm -rf "$d"
    # A marker from an installer that did not record the CA: cannot tell.
    # The current CA unreadable: not "the same", which verify would print.
    if [ -z "$cas" ]; then
        LLMSM_SCALER_KEDA_CA_STALE=unknown
    elif [ -z "$want" ]; then
        LLMSM_SCALER_KEDA_CA_STALE=unread
    elif [ "$cas" != "$want" ]; then
        LLMSM_SCALER_KEDA_CA_STALE=true
    fi
}

# llmsm_scaler_scaledobjects_json echoes the ScaledObjects in the namespaces this
# install watches as one {"items": [...]} list. Without KEDA installed there
# can be none, so the list is empty. Exits 2 when they cannot be read.
llmsm_scaler_scaledobjects_json() {
    local out rc=0 err
    err="$(mktemp)"
    if [ "$(llmsm_install_scope)" = "namespace" ] && declare -F llmsm_watch_is_multi >/dev/null && llmsm_watch_is_multi; then
        # Each managed namespace, merged into one list.
        local one part
        out='{"items":[]}'
        for one in $(llmsm_watch_words); do
            part="$(kubectl get scaledobjects.keda.sh -n "$one" -o json 2>"$err")" || { rc=$?; break; }
            out="$(jq -c --argjson p "$part" '.items += ($p.items // [])' <<<"$out")" || { rc=1; break; }
        done
    elif [ "$(llmsm_install_scope)" = "namespace" ]; then
        out="$(kubectl get scaledobjects.keda.sh -n "${LLMSM_WATCH_NS:-$LLMSM_NS}" -o json 2>"$err")" || rc=$?
    else
        out="$(kubectl get scaledobjects.keda.sh -A -o json 2>"$err")" || rc=$?
    fi
    if [ "$rc" != 0 ]; then
        # KEDA not installed yet: there can be no ScaledObjects.
        if grep -qi "doesn.t have a resource type" "$err"; then rm -f "$err"; echo '{"items":[]}'; return 0; fi
        rm -f "$err"
        return 2
    fi
    rm -f "$err"
    printf '%s' "$out"
}

# llmsm_scaler_other_namespaces echoes, one per line, each namespace other than
# $LLMSM_NS that a trigger in the watched namespaces names as
# scaling-manager-external-scaler.<namespace>: the LLMSM_NS those ScaledObjects point at.
# $1, when given, is the list llmsm_scaler_scaledobjects_json already read.
# Exits 2 when the ScaledObjects cannot be read.
llmsm_scaler_other_namespaces() {
    local out="${1:-}"
    [ -n "$out" ] || out="$(llmsm_scaler_scaledobjects_json)" || return 2
    printf '%s' "$out" | jq -r --arg ns "$LLMSM_NS" "$LLMSM_SCALER_ADDRESS_JQ"'
        [.items[]? | (.spec.triggers // [])[] | (.metadata.scalerAddress // "")
         | llmsm_scaler_ns | select(. != null and . != ($ns | ascii_downcase))] | unique | .[]' 2>/dev/null || return 2
}

# llmsm_scaler_plaintext_scaledobjects echoes <namespace>/<name> of every
# ScaledObject, in the namespaces this install watches, with a trigger naming
# this install's scaler that would NOT reach it over this installer's TLS: the
# plaintext address, or no authenticationRef to the ClusterTriggerAuthentication.
# With $1 = plaintext, the reverse: triggers still on the TLS address or
# reference. With $1 = any, every one naming this scaler (llmsm_names_scaler in
# common.sh says which addresses do). $2, when given, is the list llmsm_scaler_scaledobjects_json already read. Exits 2 when the
# ScaledObjects cannot be read: unknown, not none.
llmsm_scaler_plaintext_scaledobjects() {
    local want="${1:-tls}" out="${2:-}"
    [ -n "$out" ] || out="$(llmsm_scaler_scaledobjects_json)" || return 2
    printf '%s' "$out" | jq -r --arg ns "$LLMSM_NS" \
        --arg a "$(llmsm_scaler_address "$LLMSM_NS" tls)" --arg n "$(llmsm_scaler_keda_auth_name)" --arg w "$want" "$LLMSM_SCALER_ADDRESS_JQ"'
        [.items[]? | . as $so | (.spec.triggers // [])[]
         | select((.metadata.scalerAddress // "") | llmsm_names_scaler($ns))
         | ((.metadata.scalerAddress == $a)
            and ((.authenticationRef.name // "") == $n)
            and ((.authenticationRef.kind // "") == "ClusterTriggerAuthentication")) as $tls
         | select(if $w == "any" then true elif $w == "tls" then ($tls | not) else ((.metadata.scalerAddress == $a) or (.authenticationRef != null)) end)
         | ($so.metadata.namespace + "/" + $so.metadata.name)] | unique | .[]' 2>/dev/null || return 2
}

# llmsm_scaler_migrate_scaledobjects rewrites the triggers naming this scaler on
# every ScaledObject in $1 (<namespace>/<name> lines) to mode $2: tls (the TLS
# address and the authenticationRef) or plaintext (neither). Only those
# triggers change; the list is rewritten in place. LLMSM_SCALER_MIGRATE_FAILED
# lists the ones that could not be patched.
llmsm_scaler_migrate_scaledobjects() {
    local rows="$1" mode="$2" ns name triggers ok=0 failed=""
    local a ref
    a="$(llmsm_scaler_address "$LLMSM_NS" "$mode")"
    ref="$(llmsm_scaler_trigger_auth_json "$LLMSM_NS")"
    while IFS=/ read -r ns name; do
        [ -n "$name" ] || continue
        triggers="$(kubectl get scaledobject "$name" -n "$ns" -o json 2>/dev/null \
            | jq -c --arg ns "$LLMSM_NS" --arg a "$a" --argjson r "$ref" --arg m "$mode" "$LLMSM_SCALER_ADDRESS_JQ"'[ .spec.triggers[]?
                | if ((.metadata.scalerAddress // "") | llmsm_names_scaler($ns))
                  then (.metadata.scalerAddress = $a)
                       | (if $m == "tls" then .authenticationRef = $r else del(.authenticationRef) end)
                  else . end ]' 2>/dev/null)" || triggers=""
        if [ -n "$triggers" ] && [ "$triggers" != null ] \
            && kubectl patch scaledobject "$name" -n "$ns" --type merge \
                -p "{\"spec\":{\"triggers\":$triggers}}" >/dev/null 2>&1; then
            ok=$((ok + 1))
        else
            failed="${failed}${ns}/${name}"$'\n'
        fi
    done <<< "$rows"
    LLMSM_SCALER_MIGRATE_FAILED="$failed"
    log_info "Moved $ok ScaledObject(s) to the $mode scaler address$([ "$mode" = tls ] && printf ' with authenticationRef %s' "$(llmsm_scaler_keda_auth_name)")."
}

# llmsm_resolve_scaler_tls decides whether this install WANTS the scaler on TLS
# (LLMSM_SCALER_TLS_WANTED) and who issues the certificates
# (LLMSM_SCALER_TLS_PROVIDER_RESOLVED: cert-manager or openssl). Whether TLS is
# actually turned on is llmsm_scaler_tls_prepare's call: KEDA must be ready for it.
#
# LLMSM_SCALER_TLS unset means the default: TLS, on a new install and on an
# upgrade from plaintext, once KEDA is ready for it (LLMSM_SCALER_TLS_DEFAULTED).
# An install whose controller already has TLS keeps it: an upgrade must never
# quietly go back to plaintext. A controller whose TLS flags this installer did
# not set is a refusal rather than a guess, since the apply would drop them.
# Sets variables rather than echoing, because log_error is an exit and an exit
# inside $( ) ends only the subshell.
llmsm_resolve_scaler_tls() {
    LLMSM_SCALER_TLS_ON=false
    LLMSM_SCALER_TLS_KEPT=false
    LLMSM_SCALER_TLS_WANTED=false
    LLMSM_SCALER_TLS_DEFAULTED=false
    LLMSM_SCALER_TLS_PROVIDER_RESOLVED=""
    llmsm_scaler_check_client_san
    llmsm_scaler_tls_read_live
    case "${LLMSM_SCALER_TLS:-}" in
        true)
            LLMSM_SCALER_TLS_WANTED=true
            if [ "$LLMSM_SCALER_TLS_LIVE" = manual ]; then
                log_warning "LLMSM_SCALER_TLS=true takes over the external scaler TLS flags set by hand on the running controller in $LLMSM_NS. These are replaced by the installer's certificates:"
                printf '%s\n' "$LLMSM_SCALER_TLS_LIVE_FLAGS" | sed 's/^/    /' >&2
            fi
            ;;
        false) ;;
        "")
            case "$LLMSM_SCALER_TLS_LIVE" in
                absent|plaintext)
                    LLMSM_SCALER_TLS_WANTED=true
                    LLMSM_SCALER_TLS_DEFAULTED=true ;;
                unreadable)
                    log_error "Could not read Deployment scaling-manager-controller-manager in $LLMSM_NS ($LLMSM_SCALER_TLS_LIVE_ERR), so whether its external scaler serves TLS is unknown and cannot be kept. Re-run with LLMSM_SCALER_TLS=true or LLMSM_SCALER_TLS=false." ;;
                manual)
                    log_error "The running controller in $LLMSM_NS serves the external scaler over TLS with flags this installer did not set, and this apply would drop them. Re-run with LLMSM_SCALER_TLS=true to have the installer provision and keep TLS, or LLMSM_SCALER_TLS=false to accept plaintext." ;;
                *)
                    LLMSM_SCALER_TLS_WANTED=true
                    LLMSM_SCALER_TLS_KEPT=true ;;
            esac
            ;;
        *) log_error "LLMSM_SCALER_TLS must be true or false (got '${LLMSM_SCALER_TLS}')" ;;
    esac
    [ "$LLMSM_SCALER_TLS_WANTED" = true ] || return 0

    local want="${LLMSM_SCALER_TLS_PROVIDER:-}"
    if [ -z "$want" ]; then
        case "$LLMSM_SCALER_TLS_LIVE" in
            cert-manager|openssl) want="$LLMSM_SCALER_TLS_LIVE" ;;
            *) want=auto ;;
        esac
    fi
    case "$want" in
        cert-manager|openssl) ;;
        auto)
            llmsm_cert_manager_api
            case "$LLMSM_CERT_MANAGER_API" in
                present) want=cert-manager ;;
                absent) want=openssl ;;
                *)
                    llmsm_scaler_tls_skip "could not tell whether cert-manager is installed (${LLMSM_CERT_MANAGER_API_ERR:-discovery failed}); set LLMSM_SCALER_TLS_PROVIDER=cert-manager or LLMSM_SCALER_TLS_PROVIDER=openssl"
                    return 0 ;;
            esac
            ;;
        *) log_error "LLMSM_SCALER_TLS_PROVIDER must be auto, cert-manager or openssl (got '$want')" ;;
    esac
    LLMSM_SCALER_TLS_PROVIDER_RESOLVED="$want"
}

# llmsm_scaler_tls_skip gives up on TLS for this run: a refusal when
# LLMSM_SCALER_TLS=true asked for it, a warning and plaintext when it was only the
# default. A controller that already has TLS is never sent back by a default.
llmsm_scaler_tls_skip() {
    if [ "${LLMSM_SCALER_TLS:-}" = true ] || [ "${LLMSM_SCALER_TLS_KEPT:-false}" = true ]; then
        log_error "External scaler TLS cannot be set up: $1."
    fi
    log_warning "The external scaler stays on plaintext gRPC (TLS is the default, but $1)."
    LLMSM_SCALER_TLS_WANTED=false
}

# llmsm_scaler_tls_give_up is llmsm_scaler_tls_skip with its own refusal text: $1
# when LLMSM_SCALER_TLS=true asked for TLS or the controller already has it, else
# a warning giving reason $2, and plaintext (LLMSM_SCALER_TLS_WANTED=false). The
# default never aborts an install the docs say stays on plaintext.
llmsm_scaler_tls_give_up() {
    if [ "${LLMSM_SCALER_TLS:-}" = true ] || [ "${LLMSM_SCALER_TLS_KEPT:-false}" = true ]; then
        log_error "$1"
    fi
    llmsm_scaler_tls_skip "$2"
}

# llmsm_scaler_tls_migrate_cmd echoes the command that moves the ScaledObjects
# with the install, for a message. It names INSTALL_PHASE only when the caller
# set it on this run, and then in this run's phase: all (admin and controller
# in one pass) or scaling-manager (the controller alone, after setup-prereqs). Otherwise
# the installer works the phase out again, as it did this time, and the
# command matches the one the docs give.
llmsm_scaler_tls_migrate_cmd() {
    if [ -z "${INSTALL_PHASE_EXPLICIT:-}" ]; then
        llmsm_make_cmd deploy-scaling-manager "LLMSM_SCALER_TLS=$1" LLMSM_SCALER_TLS_MIGRATE=true
        return 0
    fi
    local phase=all
    [ "${LLMSM_APPLY_SCOPE:-all}" = all ] || phase=scaling-manager
    llmsm_make_cmd deploy-scaling-manager "LLMSM_SCALER_TLS=$1" LLMSM_SCALER_TLS_MIGRATE=true "INSTALL_PHASE=$phase"
}

# llmsm_scaler_tls_check_scaledobjects decides what happens to the ScaledObjects
# that name this scaler when its mode changes to $1 (tls or plaintext). KEDA
# keeps one connection per scalerAddress, so ScaledObjects on the old address
# cannot share the new mode: each must move with it. With
# LLMSM_SCALER_TLS_MIGRATE=true the install moves them after the controller rolls
# (LLMSM_SCALER_TLS_MIGRATE_ROWS). Without it, a change asked for explicitly is a
# refusal that lists them; a change that is only the default is skipped. Returns
# 1 when the change must not happen.
llmsm_scaler_tls_check_scaledobjects() {
    local mode="$1" rows rc=0 asked=false
    LLMSM_SCALER_TLS_MIGRATE_ROWS=""
    [ -n "${LLMSM_SCALER_TLS:-}" ] && asked=true
    rows="$(llmsm_scaler_plaintext_scaledobjects "$mode")" || rc=$?
    if [ "$rc" != 0 ]; then
        [ "$asked" = true ] && log_error "Cannot list the ScaledObjects that name this scaler, so whether they can follow it to $mode is unknown. Grant list on scaledobjects.keda.sh, or leave LLMSM_SCALER_TLS unset."
        log_warning "The external scaler stays on plaintext gRPC: the ScaledObjects that name it cannot be listed, so whether they would follow it to TLS is unknown."
        return 1
    fi
    [ -n "$rows" ] || return 0
    if [ "${LLMSM_SCALER_TLS_MIGRATE:-false}" = true ]; then
        LLMSM_SCALER_TLS_MIGRATE_ROWS="$rows"
        LLMSM_SCALER_TLS_MIGRATE_MODE="$mode"
        log_info "These ScaledObjects move to the $mode scaler address once the controller is applied (LLMSM_SCALER_TLS_MIGRATE=true):"
        printf '%s\n' "$rows" | head -20 | sed 's/^/    /' >&2
        return 0
    fi
    local what="TLS" verb="carry no authenticationRef to ClusterTriggerAuthentication $(llmsm_scaler_keda_auth_name) on the TLS address"
    [ "$mode" = plaintext ] && { what="plaintext"; verb="still use the TLS address or an authenticationRef"; }
    if [ "$asked" = true ]; then
        log_error "These ScaledObjects name this scaler and $verb, so moving it to $what would leave them on the other mode:
$(printf '%s\n' "$rows" | head -20 | sed 's/^/    /')
$(llmsm_scaler_mixed_risk)
Move them with the install:  $(llmsm_scaler_tls_migrate_cmd "$( [ "$mode" = tls ] && echo true || echo false)")"
    fi
    log_warning "The external scaler stays on plaintext gRPC. TLS is the default, but these ScaledObjects name it and would need to move with it:"
    printf '%s\n' "$rows" | head -20 | sed 's/^/    /' >&2
    log_warning "  Move them with the install:  $(llmsm_scaler_tls_migrate_cmd true)"
    return 1
}

# llmsm_scaler_tls_prepare is the install step, run by every phase.
#   admin phase (prereqs, all)  provision the certificates and KEDA's side
#   controller phase (controller, all)  turn TLS on (or off) on the Deployment:
#       patches the throw-away overlay $1/kustomization.yaml
# The controller phase turns TLS on only when KEDA is ready for it and every
# ScaledObject naming the scaler can follow (llmsm_scaler_tls_check_scaledobjects).
llmsm_scaler_tls_prepare() {
    local kustomization="$1" scope="${LLMSM_APPLY_SCOPE:-all}"
    LLMSM_SCALER_KEDA_DEFERRED=false
    LLMSM_SCALER_KEDA_READY=false
    LLMSM_SCALER_TLS_MIGRATE_ROWS=""
    llmsm_resolve_scaler_tls

    if [ "$LLMSM_SCALER_TLS_WANTED" != true ]; then
        case "$LLMSM_SCALER_TLS_LIVE" in
            cert-manager|openssl)
                [ "$scope" = prereqs ] && return 0
                llmsm_scaler_tls_check_scaledobjects plaintext || return 0
                log_warning "LLMSM_SCALER_TLS=false: the external scaler goes back to PLAINTEXT gRPC. Its certificates and KEDA's ClusterTriggerAuthentication stay; undeploy removes them." ;;
            *)
                # Plaintext stays plaintext (LLMSM_SCALER_TLS=false, or the
                # default gave up): ScaledObjects on the TLS address are stuck.
                llmsm_scaler_tls_stay_plaintext ;;
        esac
        return 0
    fi
    # openssl mints the certificates without cert-manager, and digests the
    # client CA with it: needed on both paths.
    if ! command -v openssl >/dev/null 2>&1; then
        llmsm_scaler_tls_skip "openssl is not on PATH (it mints the scaler certificates when there is no cert-manager, and digests the client CA either way)"
        llmsm_scaler_tls_stay_plaintext
        return 0
    fi
    # The default asks for no rights the permission check did not: without them
    # it stays on plaintext, rather than failing halfway through.
    if [ "$LLMSM_SCALER_TLS_DEFAULTED" = true ]; then
        local verb res denied=""
        if ! kubectl auth can-i create secrets -n "$LLMSM_NS" >/dev/null 2>&1; then
            denied="you may not create Secrets in $LLMSM_NS"
        else
            for res in secrets $([ "$LLMSM_SCALER_TLS_PROVIDER_RESOLVED" = cert-manager ] && echo issuers.cert-manager.io certificates.cert-manager.io); do
                for verb in get create; do
                    [ -z "$denied" ] || break 2
                    kubectl auth can-i "$verb" "$res" -n "$LLMSM_NS" >/dev/null 2>&1 || denied="you may not $verb $res in $LLMSM_NS"
                done
            done
        fi
        if [ -n "$denied" ]; then
            llmsm_scaler_tls_skip "$denied"
            llmsm_scaler_tls_stay_plaintext
            return 0
        fi
    fi

    # KEDA's side, checked before anything is written: a refusal there leaves
    # nothing behind. The admin phase writes it after the certificates. KEDA is
    # normally there by now: this install puts it in before the controller. If
    # its API is not served yet, it is written after the controller
    # (llmsm_scaler_tls_after_keda). The controller phase reads what the admin left.
    local keda_side=none
    case "$scope" in
        prereqs|all)
            if [ "$(llmsm_scaler_keda_api)" = present ]; then
                if llmsm_scaler_keda_auth_check; then keda_side=write; fi
            elif llmsm_scaler_install_puts_keda; then
                keda_side=deferred
            else
                llmsm_scaler_keda_not_ready "KEDA is not installed (its ClusterTriggerAuthentication kind is not served)"
            fi ;;
    esac

    if [ "$LLMSM_SCALER_TLS_PROVIDER_RESOLVED" = openssl ]; then
        llmsm_scaler_tls_provision_openssl
    else
        llmsm_scaler_tls_provision_cert_manager
    fi
    # The default gives up when cert-manager does not issue (llmsm_scaler_tls_give_up).
    if [ "$LLMSM_SCALER_TLS_WANTED" != true ]; then
        llmsm_scaler_tls_stay_plaintext
        return 0
    fi

    case "$scope" in
        prereqs|all)
            case "$keda_side" in
                write) llmsm_scaler_keda_auth_apply ;;
                deferred)
                    LLMSM_SCALER_KEDA_DEFERRED=true
                    LLMSM_SCALER_KEDA_READY=true ;;
                *) LLMSM_SCALER_KEDA_READY=false ;;
            esac ;;
        *)
            llmsm_scaler_keda_marker_state
            # KEDA trusting an old CA fails every handshake. A controller that
            # already has TLS keeps it and is warned (below); one that does
            # not yet is not turned on into that.
            if [ "$LLMSM_SCALER_KEDA_READY" = true ] && [ "$LLMSM_SCALER_KEDA_CA_STALE" = true ] \
                && [ "$LLMSM_SCALER_TLS_KEPT" != true ]; then
                LLMSM_SCALER_KEDA_READY=false
                LLMSM_SCALER_KEDA_WHY="KEDA trusts a different CA than the one that signs the scaler's certificate now, so every handshake would fail. A cluster admin runs:  $(llmsm_make_cmd setup-prereqs)  and then restarts KEDA's operator:  $(llmsm_scaler_keda_restart_cmd)"
                llmsm_scaler_keda_not_ready "$LLMSM_SCALER_KEDA_WHY"
            elif [ "$LLMSM_SCALER_KEDA_READY" != true ]; then
                # The identity mismatch carries its own fix, with the KEDA restart.
                local fix
                fix="$(llmsm_make_cmd setup-prereqs)"
                [ "$LLMSM_SCALER_KEDA_SAN_STALE" != true ] || fix=""
                # LLMSM_SCALER_TLS=true refuses whether TLS is on already or not.
                if [ "$LLMSM_SCALER_TLS_KEPT" = true ] && [ "${LLMSM_SCALER_TLS:-}" != true ]; then
                    log_warning "External scaler TLS stays on, but KEDA's side is not usable: $LLMSM_SCALER_KEDA_WHY.${fix:+ A cluster admin runs:  $fix}"
                else
                    llmsm_scaler_keda_not_ready "$LLMSM_SCALER_KEDA_WHY${fix:+; a cluster admin runs:  $fix}"
                fi
            else
                if [ "$LLMSM_SCALER_KEDA_STALE" = true ]; then
                    log_warning "KEDA holds an older client certificate than the one in $LLMSM_NS (it expires ${LLMSM_SCALER_KEDA_NOT_AFTER}). It still works until then; a cluster admin refreshes it with:  $(llmsm_make_cmd setup-prereqs)"
                fi
                llmsm_scaler_keda_ca_report log_warning
            fi ;;
    esac
    [ "$scope" != prereqs ] || return 0
    if [ "$LLMSM_SCALER_KEDA_READY" != true ] && [ "$LLMSM_SCALER_TLS_KEPT" != true ]; then
        llmsm_scaler_tls_warn_stranded
        return 0
    fi

    if [ "$LLMSM_SCALER_TLS_KEPT" = true ]; then
        # Already on, so nothing here can refuse: ScaledObjects still on the
        # plaintext address are reported, and moved when asked.
        local rows rc=0
        rows="$(llmsm_scaler_plaintext_scaledobjects tls)" || rc=$?
        if [ "$rc" = 0 ] && [ -n "$rows" ]; then
            if [ "${LLMSM_SCALER_TLS_MIGRATE:-false}" = true ]; then
                LLMSM_SCALER_TLS_MIGRATE_ROWS="$rows"
                LLMSM_SCALER_TLS_MIGRATE_MODE=tls
            else
                log_warning "These ScaledObjects name this scaler on the plaintext address or without the authenticationRef $(llmsm_scaler_keda_auth_name):"
                printf '%s\n' "$rows" | head -20 | sed 's/^/    /' >&2
                log_warning "  $(llmsm_scaler_mixed_risk)"
                log_warning "  Move them:  $(llmsm_scaler_tls_migrate_cmd true)"
            fi
        fi
    else
        llmsm_scaler_tls_check_scaledobjects tls || { llmsm_scaler_tls_warn_stranded; return 0; }
    fi

    LLMSM_SCALER_TLS_ON=true
    if [ "$LLMSM_SCALER_TLS_KEPT" = true ]; then
        log_info "External scaler TLS stays on (the running controller has it; LLMSM_SCALER_TLS=false turns it off), certificates from $LLMSM_SCALER_TLS_PROVIDER_RESOLVED"
    else
        log_info "External scaler TLS: mutual TLS, certificates from $LLMSM_SCALER_TLS_PROVIDER_RESOLVED; ScaledObjects reach it at $(llmsm_scaler_address "$LLMSM_NS" tls) with authenticationRef $(llmsm_scaler_keda_auth_name)"
    fi
    llmsm_append_scaler_tls_patch "$kustomization"
}

# llmsm_scaler_keda_ca_report reports, through log function $1, what
# llmsm_scaler_keda_marker_state found about the CA KEDA trusts: nothing when it
# is current, the fix when it moved on, and "cannot tell" when the marker does
# not record it.
llmsm_scaler_keda_ca_report() {
    local say="$1"
    case "${LLMSM_SCALER_KEDA_CA_STALE:-false}" in
        true)
            "$say" "KEDA trusts a different CA than the one that signs the scaler's certificate now (on OpenShift the service CA rotates by itself), so every handshake fails and nothing scales. A cluster admin runs:  $(llmsm_make_cmd setup-prereqs)  and then restarts KEDA's operator:  $(llmsm_scaler_keda_restart_cmd)" ;;
        unknown)
            "$say" "Cannot tell which CA KEDA trusts: ConfigMap $LLMSM_SCALER_KEDA_MARKER in $LLMSM_NS was written by an older installer and does not record it. A cluster admin re-runs:  $(llmsm_make_cmd setup-prereqs)" ;;
    esac
}

# llmsm_scaler_keda_conn_state sets LLMSM_SCALER_KEDA_CONN from the controller's
# log of the last LLMSM_SCALER_REFUSAL_SINCE (default 5m):
#   stale    it refused handshakes from a Pod in KEDA's namespace because their
#            client certificate is signed by an unknown authority, while the
#            certificate KEDA was given (the marker's copy) is signed by the
#            current client CA. KEDA presents an older certificate than the one
#            it was given: it keeps one connection per scaler address, built
#            once, and never closes it. Seen after an uninstall and reinstall in
#            the same namespace, with KEDA's operator running throughout.
#   copy     the same refusals, but the certificate KEDA was given is not signed
#            by the current client CA either
#   none     no such refusal from a Pod in KEDA's namespace
#   unknown  the log, KEDA's Pods, the marker or the client CA cannot be read
#            (LLMSM_SCALER_KEDA_CONN_WHY says which)
# LLMSM_SCALER_KEDA_CONN_PODS names the refused KEDA Pods. The controller logs at
# most one refusal per remote host per minute, so a KEDA that keeps retrying
# shows up within the window.
llmsm_scaler_keda_conn_state() {
    local kns logs pods out rc=0 hosts d
    LLMSM_SCALER_KEDA_CONN=unknown
    LLMSM_SCALER_KEDA_CONN_WHY=""
    LLMSM_SCALER_KEDA_CONN_PODS=""
    [ -n "${LLMSM_SCALER_KEDA_RUN_NS:-}" ] || llmsm_scaler_keda_run_ns
    kns="$LLMSM_SCALER_KEDA_RUN_NS"
    if [ -z "$kns" ]; then
        LLMSM_SCALER_KEDA_CONN_WHY="where KEDA runs is not known (set KEDA_NAMESPACE)"
        return 0
    fi
    logs="$(kubectl logs -n "$LLMSM_NS" -l "$LLMSM_CONTROLLER_LABEL_SELECTOR" -c manager \
        --since="${LLMSM_SCALER_REFUSAL_SINCE:-5m}" --tail=-1 --max-log-requests=10 2>&1)" || rc=$?
    if [ "$rc" != 0 ]; then
        LLMSM_SCALER_KEDA_CONN_WHY="the controller's log in $LLMSM_NS cannot be read ($(printf '%s\n' "$logs" | head -1))"
        return 0
    fi
    # The remote host of each refusal for an unknown authority, port and IPv6
    # brackets dropped. Console and JSON log lines both quote the field.
    # Both directions of the same stale connection: the controller refusing
    # KEDA's old client certificate ("unknown authority"), and KEDA refusing the
    # controller's new serving certificate with the CA it cached, which the
    # controller logs as "remote error: tls: bad certificate". Seen on kind
    # after an uninstall and a reinstall.
    hosts="$(printf '%s\n' "$logs" | grep -F 'refused a TLS handshake' | grep -E 'unknown authority|bad certificate' \
        | sed -n 's/.*"remote": *"\([^"]*\)".*/\1/p' | sed 's/:[0-9]*$//; s/^\[//; s/\]$//' | sort -u)" || hosts=""
    if [ -z "$hosts" ]; then
        LLMSM_SCALER_KEDA_CONN=none
        return 0
    fi
    rc=0
    pods="$(kubectl get pods -n "$kns" -o json 2>&1)" || rc=$?
    if [ "$rc" != 0 ]; then
        LLMSM_SCALER_KEDA_CONN_WHY="the controller refused handshakes for an unknown authority, but the Pods in $kns, where KEDA runs, cannot be read ($(printf '%s\n' "$pods" | head -1)), so whether KEDA sent them is not known"
        return 0
    fi
    LLMSM_SCALER_KEDA_CONN_PODS="$(printf '%s' "$pods" | jq -r --arg h "$hosts" '
        ($h | split("\n")) as $hs
        | [.items[]? | select([(.status.podIPs // [])[]?.ip, (.status.podIP // empty)] | any(. as $ip | $hs | any(. == $ip)))
           | .metadata.name] | unique | join(" ")' 2>/dev/null)" || LLMSM_SCALER_KEDA_CONN_PODS=""
    if [ -z "$LLMSM_SCALER_KEDA_CONN_PODS" ]; then
        LLMSM_SCALER_KEDA_CONN=none
        return 0
    fi
    # The certificate KEDA was given, against the client CA the controller
    # trusts now.
    rc=0
    out="$(kubectl get configmap "$LLMSM_SCALER_KEDA_MARKER" -n "$LLMSM_NS" -o json 2>&1)" || rc=$?
    d="$(mktemp -d)"
    [ "$rc" = 0 ] && printf '%s' "$out" | jq -r '.data.clientCert // ""' > "$d/keda.crt" 2>/dev/null || : > "$d/keda.crt"
    llmsm_scaler_object_state secret "$LLMSM_SCALER_CLIENT_SECRET"
    llmsm_scaler_secret_field ca.crt "$d/ca.crt"
    if [ ! -s "$d/keda.crt" ] || [ ! -s "$d/ca.crt" ]; then
        rm -rf "$d"
        LLMSM_SCALER_KEDA_CONN_WHY="the controller refused KEDA's Pod(s) $LLMSM_SCALER_KEDA_CONN_PODS for a certificate signed by an unknown authority, but the certificate KEDA was given (ConfigMap $LLMSM_SCALER_KEDA_MARKER) or the client CA (Secret $LLMSM_SCALER_CLIENT_SECRET) in $LLMSM_NS cannot be read"
        return 0
    fi
    if openssl verify -CAfile "$d/ca.crt" "$d/keda.crt" >/dev/null 2>&1; then
        LLMSM_SCALER_KEDA_CONN=stale
    else
        LLMSM_SCALER_KEDA_CONN=copy
    fi
    rm -rf "$d"
}

# llmsm_scaler_keda_conn_report reports what llmsm_scaler_keda_conn_state found.
# Succeeds when the cause is on KEDA's side (stale or copy): changing a
# ScaledObject cannot fix that, so the caller must not offer the re-wire. For
# unknown it names the KEDA restart as the other possible fix and fails, so the
# caller still lists the re-wire. Prints nothing for none.
llmsm_scaler_keda_conn_report() {
    case "${LLMSM_SCALER_KEDA_CONN:-none}" in
        stale)
            log_warning "  Cause: KEDA's operator still presents an OLD client certificate. The controller refused handshakes from KEDA's Pod(s) $LLMSM_SCALER_KEDA_CONN_PODS: certificate signed by unknown authority. The certificate KEDA was given (ClusterTriggerAuthentication $(llmsm_scaler_keda_auth_name)) is signed by the current CA, so KEDA is still using a connection it opened before. KEDA keeps one connection per scaler address and never closes it, so this follows an uninstall and reinstall in the same namespace. Changing the ScaledObjects does not help. Restart KEDA's operator:  $(llmsm_scaler_keda_restart_cmd)"
            return 0 ;;
        copy)
            log_warning "  Cause: KEDA's client certificate is not signed by the scaler's current client CA. The controller refused handshakes from KEDA's Pod(s) $LLMSM_SCALER_KEDA_CONN_PODS: certificate signed by unknown authority, and the certificate KEDA was given (ClusterTriggerAuthentication $(llmsm_scaler_keda_auth_name)) does not chain to the client CA in Secret $LLMSM_SCALER_CLIENT_SECRET. A cluster admin runs:  $(llmsm_make_cmd setup-prereqs)  and then restarts KEDA's operator:  $(llmsm_scaler_keda_restart_cmd)"
            return 0 ;;
        unknown)
            log_warning "  Whether KEDA still presents an old client certificate is not checked: ${LLMSM_SCALER_KEDA_CONN_WHY}. One of two fixes applies. If this scaler was uninstalled and reinstalled in this namespace while KEDA's operator kept running (the controller log then shows \"refused a TLS handshake\" with \"certificate signed by unknown authority\"), restart KEDA's operator:  $(llmsm_scaler_keda_restart_cmd)  Otherwise change the ScaledObjects again (below)."
            return 1 ;;
    esac
    return 1
}

# llmsm_scaler_keda_trust_state sets LLMSM_SCALER_KEDA_TRUST from what KEDA was
# actually given, the Secret its ClusterTriggerAuthentication reads in KEDA's
# namespace, rather than from the marker the admin phase wrote:
#   ok        its ca.crt verifies the scaler's serving certificate, and the
#             controller's client CA verifies its tls.crt
#   mismatch  one of them does not, or the copy is missing
#             (LLMSM_SCALER_KEDA_TRUST_WHY says which and what KEDA then does)
#   unread    something could not be read: not checked, never "ok"
#             (LLMSM_SCALER_KEDA_TRUST_WHY says what)
# It reads what KEDA reads from the Secret; a KEDA that kept an older
# connection presents what it read then (llmsm_scaler_keda_conn_state).
llmsm_scaler_keda_trust_state() {
    local name copy_ns="" d out rc=0
    LLMSM_SCALER_KEDA_TRUST=unread
    LLMSM_SCALER_KEDA_TRUST_WHY=""
    name="$(llmsm_scaler_keda_auth_name)"
    if ! command -v openssl >/dev/null 2>&1; then
        LLMSM_SCALER_KEDA_TRUST_WHY="openssl is not on PATH"
        return 0
    fi
    llmsm_scaler_keda_object_ns
    copy_ns="$LLMSM_SCALER_KEDA_OBJ_NS"
    if [ -z "$copy_ns" ]; then
        out="$(kubectl get configmap "$LLMSM_SCALER_KEDA_MARKER" -n "$LLMSM_NS" -o json 2>/dev/null)" || rc=$?
        [ "$rc" != 0 ] || copy_ns="$(printf '%s' "$out" | jq -r '.data.secretNamespace // ""' 2>/dev/null)" || copy_ns=""
    fi
    if [ -z "$copy_ns" ]; then
        LLMSM_SCALER_KEDA_TRUST_WHY="where KEDA reads its Secrets is not known (${LLMSM_SCALER_KEDA_OBJ_WHY:-set KEDA_NAMESPACE})"
        return 0
    fi
    LLMSM_NS="$copy_ns" llmsm_scaler_object_state secret "$name"
    case "$LLMSM_OBJ_STATE" in
        absent)
            LLMSM_SCALER_KEDA_TRUST=mismatch
            LLMSM_SCALER_KEDA_TRUST_WHY="KEDA's copy of the client certificate, Secret $name in $copy_ns, does not exist: KEDA presents no certificate and the scaler refuses it"
            return 0 ;;
        unreadable)
            LLMSM_SCALER_KEDA_TRUST_WHY="Secret $name in $copy_ns, which KEDA reads, cannot be read ($LLMSM_OBJ_ERR)"
            return 0 ;;
    esac
    d="$(mktemp -d)"
    llmsm_scaler_secret_field ca.crt "$d/keda-ca.crt"
    llmsm_scaler_secret_field tls.crt "$d/keda.crt"
    llmsm_scaler_object_state secret "$LLMSM_SCALER_TLS_SECRET"
    if [ "$LLMSM_OBJ_STATE" = unreadable ]; then
        rm -rf "$d"
        LLMSM_SCALER_KEDA_TRUST_WHY="the scaler's serving certificate, Secret $LLMSM_SCALER_TLS_SECRET in $LLMSM_NS, cannot be read ($LLMSM_OBJ_ERR)"
        return 0
    fi
    llmsm_scaler_secret_field tls.crt "$d/serving.crt"
    llmsm_scaler_object_state secret "$LLMSM_SCALER_CLIENT_SECRET"
    if [ "$LLMSM_OBJ_STATE" = unreadable ]; then
        rm -rf "$d"
        LLMSM_SCALER_KEDA_TRUST_WHY="the controller's client CA, Secret $LLMSM_SCALER_CLIENT_SECRET in $LLMSM_NS, cannot be read ($LLMSM_OBJ_ERR)"
        return 0
    fi
    llmsm_scaler_secret_field ca.crt "$d/client-ca.crt"
    if [ ! -s "$d/serving.crt" ] || [ ! -s "$d/client-ca.crt" ]; then
        rm -rf "$d"
        LLMSM_SCALER_KEDA_TRUST_WHY="the scaler's serving certificate (Secret $LLMSM_SCALER_TLS_SECRET) or client CA (Secret $LLMSM_SCALER_CLIENT_SECRET) in $LLMSM_NS is missing"
        return 0
    fi
    LLMSM_SCALER_KEDA_TRUST=mismatch
    if [ ! -s "$d/keda-ca.crt" ] || ! openssl verify -CAfile "$d/keda-ca.crt" "$d/serving.crt" >/dev/null 2>&1; then
        LLMSM_SCALER_KEDA_TRUST_WHY="the CA KEDA trusts (ca.crt of Secret $name in $copy_ns) does not verify the scaler's serving certificate (Secret $LLMSM_SCALER_TLS_SECRET in $LLMSM_NS): KEDA rejects the scaler, which logs \"remote error: tls: bad certificate\""
    elif [ ! -s "$d/keda.crt" ] || ! openssl verify -CAfile "$d/client-ca.crt" "$d/keda.crt" >/dev/null 2>&1; then
        LLMSM_SCALER_KEDA_TRUST_WHY="the controller's client CA (Secret $LLMSM_SCALER_CLIENT_SECRET in $LLMSM_NS) does not verify KEDA's client certificate (tls.crt of Secret $name in $copy_ns): the scaler refuses KEDA"
    else
        LLMSM_SCALER_KEDA_TRUST=ok
    fi
    rm -rf "$d"
}

# llmsm_scaler_tls_stay_plaintext names the ScaledObjects a plaintext scaler
# strands (llmsm_scaler_tls_warn_stranded), from every phase but the admin's.
llmsm_scaler_tls_stay_plaintext() {
    [ "${LLMSM_APPLY_SCOPE:-all}" = prereqs ] || llmsm_scaler_tls_warn_stranded
}

# llmsm_scaler_tls_warn_stranded warns about ScaledObjects on the TLS address or
# reference while the scaler stays on plaintext (llmsm_scaler_mixed_risk). With
# LLMSM_SCALER_TLS_MIGRATE=true it moves them back to the plaintext address
# instead, once the controller is applied (LLMSM_SCALER_TLS_MIGRATE_ROWS).
llmsm_scaler_tls_warn_stranded() {
    local rows rc=0
    rows="$(llmsm_scaler_plaintext_scaledobjects plaintext)" || rc=$?
    [ "$rc" = 0 ] && [ -n "$rows" ] || return 0
    if [ "${LLMSM_SCALER_TLS_MIGRATE:-false}" = true ]; then
        LLMSM_SCALER_TLS_MIGRATE_ROWS="$rows"
        LLMSM_SCALER_TLS_MIGRATE_MODE=plaintext
        log_info "The external scaler stays on plaintext gRPC. These ScaledObjects move back to the plaintext scaler address once the controller is applied (LLMSM_SCALER_TLS_MIGRATE=true):"
        printf '%s\n' "$rows" | head -20 | sed 's/^/    /' >&2
        return 0
    fi
    log_warning "These ScaledObjects use the scaler's TLS address or authenticationRef, but it stays on plaintext:"
    printf '%s\n' "$rows" | head -20 | sed 's/^/    /' >&2
    log_warning "  $(llmsm_scaler_mixed_risk)"
    log_warning "  Fix what keeps TLS off (above), or move them back:  $(llmsm_scaler_tls_migrate_cmd false)"
}

# llmsm_scaler_tls_after_keda writes KEDA's side when this install put KEDA in
# but its API was not served yet when the certificates were written. A
# fallback: KEDA is installed before the controller, so this is rare. The
# controller was already given TLS on the promise, so this is a refusal
# whatever LLMSM_SCALER_TLS says.
llmsm_scaler_tls_after_keda() {
    [ "${LLMSM_SCALER_KEDA_DEFERRED:-false}" = true ] || return 0
    LLMSM_SCALER_KEDA_DEFERRED=false
    LLMSM_SCALER_TLS=true llmsm_scaler_keda_auth_apply
}

# llmsm_scaler_sole_server_state sets LLMSM_SCALER_SOLE_WHY to why the controller's
# current Pods are not yet the only thing serving the scaler, or to empty when
# they are: no controller Pod is terminating, every one left is Ready, the
# Service's EndpointSlices list exactly those Pods (all ready), and, when the
# controller elects a leader, one of them holds the lease.
#
# `kubectl rollout status` does not mean this. It returns once the new Pod is
# Ready and the old ReplicaSet is scaled to zero, while the old Pod is still
# terminating and can still be an endpoint. Measured on an OpenShift cluster:
# the old Pod sent its GOAWAY at 13:40:23, KEDA reconciled the moved
# ScaledObject at 13:40:27 and got a plaintext answer on the TLS address, and
# the new Pod took the lease at 13:40:37.
#
# Waiting is not enough on its own. On kind, with the old Pod already gone,
# KEDA still failed on the TLS address for about two minutes with "first record
# does not look like a TLS handshake": the error its pooled connection last got
# from the plaintext Pod, while it waits to reconnect. llmsm_scaler_ensure_wired
# covers that.
#
# An EndpointSlice or Lease the caller may not read is not waited on; the names
# of what was skipped are in LLMSM_SCALER_SOLE_SKIPPED.
llmsm_scaler_sole_server_state() {
    local out rc=0 live
    LLMSM_SCALER_SOLE_WHY=""
    LLMSM_SCALER_SOLE_SKIPPED=""
    out="$(kubectl get pods -n "$LLMSM_NS" -l "$LLMSM_CONTROLLER_LABEL_SELECTOR" -o json 2>/dev/null)" || rc=$?
    if [ "$rc" != 0 ] || [ -z "$out" ]; then
        LLMSM_SCALER_SOLE_WHY="the controller Pods in $LLMSM_NS cannot be read"
        return 0
    fi
    LLMSM_SCALER_SOLE_WHY="$(printf '%s' "$out" | jq -r '
        [.items[]? | select(.metadata.deletionTimestamp != null) | .metadata.name] as $old
        | [.items[]? | select(.metadata.deletionTimestamp == null)
           | select(([.status.conditions[]? | select(.type == "Ready" and .status == "True")] | length) == 0)
           | .metadata.name] as $unready
        | [.items[]? | select(.metadata.deletionTimestamp == null) | .metadata.name] as $live
        | if ($old | length) > 0 then "old controller Pod(s) still terminating: " + ($old | join(" "))
          elif ($live | length) == 0 then "no controller Pod is running"
          elif ($unready | length) > 0 then "controller Pod(s) not Ready: " + ($unready | join(" "))
          else "" end' 2>/dev/null)" || LLMSM_SCALER_SOLE_WHY="the controller Pods in $LLMSM_NS cannot be parsed"
    [ -z "$LLMSM_SCALER_SOLE_WHY" ] || return 0
    live="$(printf '%s' "$out" | jq -c '[.items[]? | select(.metadata.deletionTimestamp == null) | .metadata.name] | sort')"

    rc=0
    out="$(kubectl get endpointslices -n "$LLMSM_NS" -l kubernetes.io/service-name=scaling-manager-external-scaler -o json 2>/dev/null)" || rc=$?
    if [ "$rc" != 0 ] || [ -z "$out" ]; then
        LLMSM_SCALER_SOLE_SKIPPED="EndpointSlices"
    else
        LLMSM_SCALER_SOLE_WHY="$(printf '%s' "$out" | jq -r --argjson live "$live" '
            [.items[]? | .endpoints[]?] as $eps
            | ([$eps[] | .targetRef.name // ""] | unique) as $names
            | [$eps[] | select((.conditions.ready // true) != true) | .targetRef.name // "?"] as $notready
            | if $names != $live then "the scaler Service routes to " + (if ($names | length) == 0 then "no Pod" else ($names | join(" ")) end) + ", not only to " + ($live | join(" "))
              elif ($notready | length) > 0 then "scaler endpoint(s) not ready: " + ($notready | join(" "))
              else "" end' 2>/dev/null)" || LLMSM_SCALER_SOLE_WHY="the scaler EndpointSlices cannot be parsed"
        [ -z "$LLMSM_SCALER_SOLE_WHY" ] || return 0
    fi

    # The lease, when the controller elects one. Until it holds it, the new
    # Pod serves a standby that it rebinds on election.
    rc=0
    out="$(kubectl get deployment scaling-manager-controller-manager -n "$LLMSM_NS" -o json 2>/dev/null)" || rc=$?
    if [ "$rc" = 0 ] && printf '%s' "$out" | jq -e '[.spec.template.spec.containers[]? | select(.name == "manager")
            | (.args // [])[] | select(. == "--leader-elect" or . == "--leader-elect=true")] | length > 0' >/dev/null 2>&1; then
        rc=0
        out="$(kubectl get leases -n "$LLMSM_NS" -o json 2>/dev/null)" || rc=$?
        if [ "$rc" != 0 ] || [ -z "$out" ]; then
            LLMSM_SCALER_SOLE_SKIPPED="${LLMSM_SCALER_SOLE_SKIPPED:+$LLMSM_SCALER_SOLE_SKIPPED and }the leader Lease"
        elif ! printf '%s' "$out" | jq -e --argjson live "$live" '[.items[]? | .spec.holderIdentity // "" | split("_")[0]
                | select(. as $h | $live | index($h))] | length > 0' >/dev/null 2>&1; then
            LLMSM_SCALER_SOLE_WHY="no current controller Pod holds the leader lease yet"
        fi
    fi
}

# llmsm_scaler_wait_sole_server waits, for at most LLMSM_SCALER_SETTLE_TIMEOUT
# seconds (default 180), until llmsm_scaler_sole_server_state finds nothing in
# the way. Returns 1 on timeout, with the reason in LLMSM_SCALER_SOLE_WHY.
llmsm_scaler_wait_sole_server() {
    local end
    end=$(( $(date +%s) + ${LLMSM_SCALER_SETTLE_TIMEOUT:-180} ))
    while :; do
        llmsm_scaler_sole_server_state
        [ -n "$LLMSM_SCALER_SOLE_WHY" ] || return 0
        [ "$(date +%s)" -lt "$end" ] || return 1
        sleep "${LLMSM_SCALER_POLL_SECONDS:-3}"
    done
}

# llmsm_scaler_so_wiring echoes how KEDA wired ScaledObject $2 in namespace $1:
#   wired        the HPA carries the external metric of every trigger naming
#                this scaler, and status.externalMetricNames lists it
#   unwired I..  it does not, for the triggers at these indices
#   paused       KEDA is told not to scale it
#   unknown      the ScaledObject or its HPA could not be read
# KEDA names a trigger's metric s<index>-<name>. It asks the scaler for that
# name only when the ScaledObject's spec changes, and builds the HPA without the
# metric when the call fails: then it stays on the CPU default for good.
llmsm_scaler_so_wiring() {
    local ns="$1" name="$2" so hpa rc=0 hname
    so="$(kubectl get scaledobject "$name" -n "$ns" -o json 2>/dev/null)" || rc=$?
    if [ "$rc" != 0 ] || [ -z "$so" ]; then echo unknown; return 0; fi
    hname="$(printf '%s' "$so" | jq -r --arg n "$name" '.status.hpaName // ("keda-hpa-" + $n)' 2>/dev/null)" || hname="keda-hpa-$name"
    rc=0
    hpa="$(kubectl get hpa "$hname" -n "$ns" -o json 2>&1)" || rc=$?
    if [ "$rc" != 0 ]; then
        if llmsm_scaler_not_found "$hpa"; then hpa='{}'; else echo unknown; return 0; fi
    fi
    printf '%s' "$so" | jq -r --arg ns "$LLMSM_NS" --argjson h "$hpa" "$LLMSM_SCALER_ADDRESS_JQ"'
        if ([.status.conditions[]? | select(.type == "Paused" and .status == "True")] | length) > 0 then "paused"
        else
          ([.spec.triggers // [] | to_entries[]
            | select((.value.metadata.scalerAddress // "") | llmsm_names_scaler($ns)) | .key]) as $idx
          | ([$h.spec.metrics[]? | select(.type == "External") | .external.metric.name // ""]) as $hm
          | (.status.externalMetricNames // []) as $em
          | [$idx[] | . as $i | ("s" + ($i | tostring) + "-") as $pre
             | select((([$hm[] | select(startswith($pre))] | length) == 0)
                      or (([$em[] | select(startswith($pre))] | length) == 0))
             | $i]
          | if length == 0 then "wired" else "unwired " + (map(tostring) | join(" ")) end
        end' 2>/dev/null || echo unknown
}

# llmsm_scaler_unwired_of checks the <namespace>/<name> rows in $1 and sets
# LLMSM_SCALER_UNWIRED to "<namespace>/<name> <index>..." for each one KEDA has
# not wired, and LLMSM_SCALER_WIRE_UNKNOWN to the rows it could not read.
llmsm_scaler_unwired_of() {
    local ns name st
    LLMSM_SCALER_UNWIRED=""
    LLMSM_SCALER_WIRE_UNKNOWN=""
    while IFS=/ read -r ns name; do
        [ -n "$name" ] || continue
        st="$(llmsm_scaler_so_wiring "$ns" "$name")"
        case "$st" in
            wired|paused) ;;
            unwired*) LLMSM_SCALER_UNWIRED="${LLMSM_SCALER_UNWIRED}${ns}/${name} ${st#unwired }"$'\n' ;;
            *) LLMSM_SCALER_WIRE_UNKNOWN="${LLMSM_SCALER_WIRE_UNKNOWN}${ns}/${name}"$'\n' ;;
        esac
    done <<< "$1"
}

# llmsm_scaler_wait_wired waits up to $2 seconds for every row in $1 to be wired
# (llmsm_scaler_unwired_of). Returns 1 while some are not.
llmsm_scaler_wait_wired() {
    local end
    end=$(( $(date +%s) + $2 ))
    while :; do
        llmsm_scaler_unwired_of "$1"
        [ -n "$LLMSM_SCALER_UNWIRED" ] || return 0
        [ "$(date +%s)" -lt "$end" ] || return 1
        sleep "${LLMSM_SCALER_POLL_SECONDS:-3}"
    done
}

# LLMSM_SCALER_REWIRE_KEY is the trigger metadata key a re-wire writes. The scaler
# ignores it. Changing it is a spec change, which is what makes KEDA ask for the
# metric spec again; an annotation is not.
LLMSM_SCALER_REWIRE_KEY=scalingManagerRewire

# llmsm_scaler_rewire_patch echoes the JSON patch that sets the re-wire key on
# the trigger at index $1 to $2.
llmsm_scaler_rewire_patch() {
    jq -nc --arg i "$1" --arg k "$LLMSM_SCALER_REWIRE_KEY" --arg v "$2" \
        '[{op: "add", path: ("/spec/triggers/" + $i + "/metadata/" + $k), value: $v}]'
}

# llmsm_scaler_rewire_cmd echoes, for a message, the command that re-wires the
# unwired row $1 ("<namespace>/<name> <index>...").
llmsm_scaler_rewire_cmd() {
    local so idxs ns name i cmd=""
    so="${1%% *}"
    idxs="${1#* }"
    ns="${so%%/*}"
    name="${so#*/}"
    for i in $idxs; do
        cmd="${cmd:+$cmd; }kubectl patch scaledobject $name -n $ns --type json -p '$(llmsm_scaler_rewire_patch "$i" "$(date +%s)")'"
    done
    printf '%s\n' "$cmd"
}

# llmsm_scaler_rewire makes KEDA read the metric spec again for the unwired rows
# in $1 ("<namespace>/<name> <index>..."): it patches the re-wire key on each
# trigger that names this scaler. Only those triggers, only those ScaledObjects.
llmsm_scaler_rewire() {
    local row so idxs ns name i stamp
    stamp="$(date +%s)"
    while IFS= read -r row; do
        [ -n "$row" ] || continue
        so="${row%% *}"
        idxs="${row#* }"
        ns="${so%%/*}"
        name="${so#*/}"
        for i in $idxs; do
            kubectl patch scaledobject "$name" -n "$ns" --type json \
                -p "$(llmsm_scaler_rewire_patch "$i" "$stamp")" >/dev/null 2>&1 || true
        done
    done <<< "$1"
}

# llmsm_scaler_ensure_wired checks that KEDA wired the HPA of every ScaledObject
# just moved ($1, <namespace>/<name> rows), and re-wires the ones it did not.
#
# KEDA asks the scaler for the metric spec once per spec change. The move is
# one; a call that fails then (a Pod going away, or KEDA's connection for the
# new address still backing off from the old mode, up to two minutes) leaves
# the HPA on the CPU default for good. So: wait LLMSM_SCALER_REWIRE_WAIT seconds
# (default 30), then up to LLMSM_SCALER_REWIRE_TRIES times (default 6) change the
# re-wire key and wait again. Never deletes or recreates a ScaledObject:
# KEDA does the same single call for a new one as for a changed one.
llmsm_scaler_ensure_wired() {
    local rows="$1" wait="${LLMSM_SCALER_REWIRE_WAIT:-30}" tries="${LLMSM_SCALER_REWIRE_TRIES:-6}" t=0
    [ -n "$rows" ] || return 0
    while ! llmsm_scaler_wait_wired "$rows" "$wait"; do
        # A KEDA that still presents an old client certificate: no re-wire
        # helps, only a restart of its operator.
        llmsm_scaler_keda_conn_state
        if [ "$LLMSM_SCALER_KEDA_CONN" = stale ]; then
            llmsm_scaler_keda_stale_act "$LLMSM_SCALER_UNWIRED" || return 1
            llmsm_scaler_wait_wired "$rows" "$wait"
            return
        fi
        if [ "$LLMSM_SCALER_KEDA_CONN" = copy ]; then
            log_warning "=================================================================="
            log_warning "KEDA has not wired the scaler's metric onto the HPA of these ScaledObjects, so they scale on the CPU default and the scaling manager's decisions do not reach them:"
            printf '%s' "$LLMSM_SCALER_UNWIRED" | sed 's/ .*//; s/^/    /' >&2
            llmsm_scaler_keda_conn_report || true
            log_warning "=================================================================="
            return 1
        fi
        if [ "$t" -ge "$tries" ]; then
            log_warning "=================================================================="
            log_warning "KEDA has not wired the scaler's metric onto the HPA of these ScaledObjects, so they scale on the CPU default and the scaling manager's decisions do not reach them:"
            printf '%s' "$LLMSM_SCALER_UNWIRED" | sed 's/ .*//; s/^/    /' >&2
            llmsm_scaler_keda_conn_report || true
            log_warning "  KEDA asks for the metric spec only when a ScaledObject's spec changes. Once the scaler answers (make verify-deployment), change it again:"
            printf '%s' "$LLMSM_SCALER_UNWIRED" | while IFS= read -r row; do
                [ -n "$row" ] && printf '    %s\n' "$(llmsm_scaler_rewire_cmd "$row")" >&2
            done
            log_warning "=================================================================="
            return 1
        fi
        t=$((t + 1))
        log_info "KEDA has not wired the scaler's metric for $(printf '%s' "$LLMSM_SCALER_UNWIRED" | sed 's/ .*//' | paste -sd' ' -); changing trigger metadata $LLMSM_SCALER_REWIRE_KEY so it asks again (try $t of $tries)."
        llmsm_scaler_rewire "$LLMSM_SCALER_UNWIRED"
    done
    if [ -n "$LLMSM_SCALER_WIRE_UNKNOWN" ]; then
        log_info "Could not read these ScaledObjects or their HPAs, so whether KEDA wired them is not checked: $(printf '%s' "$LLMSM_SCALER_WIRE_UNKNOWN" | paste -sd' ' -)"
    fi
    return 0
}

# llmsm_scaler_tls_finish moves the ScaledObjects that must follow the scaler to
# its new mode (LLMSM_SCALER_TLS_MIGRATE_ROWS), once the controller is applied.
# Only once its rollout finished (LLMSM_CONTROLLER_ROLLED_OUT, set by
# deploy_scaling_manager_controller) and the new Pods alone serve the scaler
# (llmsm_scaler_wait_sole_server): moved earlier, KEDA dials a mode an old Pod
# does not serve. Otherwise it names them and the command that moves them
# later. After the move, it checks KEDA wired each HPA (llmsm_scaler_ensure_wired).
llmsm_scaler_tls_finish() {
    [ -n "${LLMSM_SCALER_TLS_MIGRATE_ROWS:-}" ] || return 0
    local why="" rows
    if [ "${LLMSM_CONTROLLER_ROLLED_OUT:-true}" != true ]; then
        why="The controller rollout did not finish"
    elif ! llmsm_scaler_wait_sole_server; then
        why="The new controller Pods are not yet the only thing serving the scaler ($LLMSM_SCALER_SOLE_WHY)"
    fi
    if [ -n "$why" ]; then
        log_warning "$why, so these ScaledObjects were NOT moved to the $LLMSM_SCALER_TLS_MIGRATE_MODE scaler address:"
        printf '%s\n' "$LLMSM_SCALER_TLS_MIGRATE_ROWS" | head -20 | sed 's/^/    /' >&2
        log_warning "  Once  kubectl rollout status deployment/scaling-manager-controller-manager -n $LLMSM_NS  succeeds and the old Pods are gone, move them:  $(llmsm_scaler_tls_migrate_cmd "$([ "$LLMSM_SCALER_TLS_MIGRATE_MODE" = tls ] && echo true || echo false)")"
        LLMSM_SCALER_TLS_MIGRATE_ROWS=""
        return 0
    fi
    [ -z "${LLMSM_SCALER_SOLE_SKIPPED:-}" ] || log_info "Not waited on before the move: the $LLMSM_SCALER_SOLE_SKIPPED (not readable)."
    rows="$LLMSM_SCALER_TLS_MIGRATE_ROWS"
    llmsm_scaler_migrate_scaledobjects "$rows" "$LLMSM_SCALER_TLS_MIGRATE_MODE"
    LLMSM_SCALER_TLS_MIGRATE_ROWS=""
    if [ -n "$LLMSM_SCALER_MIGRATE_FAILED" ]; then
        log_warning "These ScaledObjects could not be moved to the $LLMSM_SCALER_TLS_MIGRATE_MODE scaler address. Until they are, they may stop scaling, and after a KEDA restart they can stop the others on their address too (GitOps may also put the old trigger back; change it at the source):"
        printf '%s' "$LLMSM_SCALER_MIGRATE_FAILED" | sed 's/^/    /' >&2
        rows="$(printf '%s\n' "$rows" | grep -vxF -f <(printf '%s' "$LLMSM_SCALER_MIGRATE_FAILED") || true)"
    fi
    llmsm_scaler_ensure_wired "$rows" || true
}

# llmsm_scaler_tls_permission_needs echoes "<resource> <namespace>" for each kind
# this phase creates for scaler TLS ("-" for a cluster-scoped kind), when TLS
# was asked for or is already on. The default asks for nothing: without the
# rights it stays on plaintext and says so. Echoes only, never refuses, so the
# permission check can call it in a substitution.
llmsm_scaler_tls_permission_needs() {
    local on=false provider="${LLMSM_SCALER_TLS_PROVIDER:-}" phase="${INSTALL_PHASE:-all}"
    llmsm_scaler_tls_read_live
    case "${LLMSM_SCALER_TLS:-}" in
        true) on=true ;;
        "") case "$LLMSM_SCALER_TLS_LIVE" in cert-manager|openssl) on=true ;; esac ;;
    esac
    [ "$on" = true ] || return 0
    echo "secrets $LLMSM_NS"
    if [ -z "$provider" ]; then
        case "$LLMSM_SCALER_TLS_LIVE" in
            cert-manager|openssl) provider="$LLMSM_SCALER_TLS_LIVE" ;;
            *) llmsm_cert_manager_api
               [ "$LLMSM_CERT_MANAGER_API" = present ] && provider=cert-manager ;;
        esac
    fi
    if [ "$provider" = cert-manager ]; then
        echo "certificates.cert-manager.io $LLMSM_NS"
        echo "issuers.cert-manager.io $LLMSM_NS"
    fi
    [ "$phase" != scaling-manager ] || return 0
    echo "configmaps $LLMSM_NS"
    echo "clustertriggerauthentications.keda.sh -"
    llmsm_scaler_keda_object_ns
    [ -z "$LLMSM_SCALER_KEDA_OBJ_NS" ] || echo "secrets $LLMSM_SCALER_KEDA_OBJ_NS"
}

# LLMSM_SCALER_CA_KEPT_ANNOTATION marks a scaler CA an uninstall kept because
# KEDA's operator may still hold a connection that trusts it. A reinstall that
# finds it knows KEDA's old connection verifies the new certificates.
LLMSM_SCALER_CA_KEPT_ANNOTATION=scaling-manager.llmd.ai/kept-for-keda

# llmsm_scaler_tls_undeploy removes what llmsm_scaler_tls_prepare created in LLMSM_NS:
# only objects carrying its label. The OpenShift service CA's serving Secret is
# the platform's and goes with the Service.
#
# Except the CA, while KEDA may still trust it. KEDA's operator keeps one
# connection per scaler address, with the CA and client certificate it first
# read, until it restarts (see "KEDA's side"). Removing the CA and reinstalling
# mints a new one, and KEDA then refuses every certificate the new install
# serves: measured on a GPU cluster, a reinstall reached no KEDA call and the
# controller found no model, for hours. So the CA Secret stays (annotated
# LLMSM_SCALER_CA_KEPT_ANNOTATION, LLMSM_SCALER_CA_KEPT=true) when ScaledObjects
# use the TLS address, or cannot be listed, or when KEDA's side stays (a
# controller-only uninstall). A reinstall signs its new leaves with it, and
# KEDA's pooled connection keeps working with no restart.
# llmsm_scaler_keda_restart_after_undeploy removes it once it restarted KEDA.
llmsm_scaler_tls_undeploy() {
    local keep_ca=false ca_rows ca_rc=0 cascade=--cascade=background
    LLMSM_SCALER_CA_KEPT=false
    if [ "${LLMSM_APPLY_SCOPE:-all}" = controller ]; then
        keep_ca=true
    else
        ca_rows="$(llmsm_scaler_plaintext_scaledobjects plaintext)" || ca_rc=$?
        if [ "$ca_rc" != 0 ] || [ -n "$ca_rows" ]; then keep_ca=true; fi
    fi
    if [ "$keep_ca" = true ]; then cascade=--cascade=orphan; fi
    llmsm_cert_manager_api
    if [ "$LLMSM_CERT_MANAGER_API" = present ]; then
        # Orphaned, not cascaded: with cert-manager's owner references on, a
        # cascade would garbage-collect the kept CA Secret with its Certificate.
        kubectl delete certificates.cert-manager.io,issuers.cert-manager.io -n "$LLMSM_NS" \
            -l "$LLMSM_SCALER_TLS_LABEL" --ignore-not-found "$cascade" >/dev/null 2>&1 || true
    fi
    if [ "$keep_ca" = true ]; then
        kubectl delete secret -n "$LLMSM_NS" -l "$LLMSM_SCALER_TLS_LABEL" \
            --field-selector "metadata.name!=$LLMSM_SCALER_CA_SECRET" --ignore-not-found >/dev/null 2>&1 \
            || log_warning "Could not remove the external scaler certificate Secrets (label $LLMSM_SCALER_TLS_LABEL) in $LLMSM_NS."
        llmsm_scaler_object_state secret "$LLMSM_SCALER_CA_SECRET"
        if [ "$LLMSM_OBJ_STATE" = ours ]; then
            kubectl annotate secret "$LLMSM_SCALER_CA_SECRET" -n "$LLMSM_NS" --overwrite \
                "$LLMSM_SCALER_CA_KEPT_ANNOTATION=$(date -u +%Y-%m-%dT%H:%M:%SZ)" >/dev/null 2>&1 || true
            LLMSM_SCALER_CA_KEPT=true
            if [ "${LLMSM_APPLY_SCOPE:-all}" = controller ]; then
                log_info "Kept Secret $LLMSM_SCALER_CA_SECRET in $LLMSM_NS, the external scaler's CA: KEDA's side (the cluster admin's) still holds a client certificate it signed. A reinstall here signs its new certificates with it, so KEDA keeps reaching the scaler. A cluster admin's  $(llmsm_make_cmd undeploy-scaling-manager)  removes both."
            else
                log_info "Kept Secret $LLMSM_SCALER_CA_SECRET in $LLMSM_NS, the external scaler's CA: ScaledObjects used the TLS address$([ "$ca_rc" != 0 ] && printf ' (or they cannot be listed)'), so KEDA's operator holds a connection that trusts this CA until it restarts. A reinstall here signs its new certificates with it, so KEDA keeps reaching the scaler with no restart. To remove it, restart KEDA's operator first ($(llmsm_scaler_keda_restart_cmd)), then:  kubectl delete secret $LLMSM_SCALER_CA_SECRET -n $LLMSM_NS  (an uninstall with KEDA_RESTART=true does both)."
            fi
        fi
    else
        kubectl delete secret -n "$LLMSM_NS" -l "$LLMSM_SCALER_TLS_LABEL" --ignore-not-found >/dev/null 2>&1 \
            || log_warning "Could not remove the external scaler certificate Secrets (label $LLMSM_SCALER_TLS_LABEL) in $LLMSM_NS."
    fi
    # KEDA's side, and the marker that records it, are the admin's: only an
    # uninstall that may write them removes them.
    [ "${LLMSM_APPLY_SCOPE:-all}" != controller ] || return 0
    # cta_gone and copy_gone: this installer's ClusterTriggerAuthentication and
    # copy are known to be gone (removed now, or not there). Until both are, the
    # marker stays: it is how a later uninstall finds the copy.
    local name marker_state copy_ns="" marker_ns="" out rc=0 removed=false cta_gone=false copy_gone=false
    name="$(llmsm_scaler_keda_auth_name)"
    # The marker records where the copy went. Read it before anything is
    # deleted, and delete it last: it is how a re-run finds the copy.
    llmsm_scaler_object_state configmap "$LLMSM_SCALER_KEDA_MARKER"
    marker_state="$LLMSM_OBJ_STATE"
    if [ "$marker_state" = ours ]; then
        marker_ns="$(printf '%s' "$LLMSM_OBJ_JSON" | jq -r '.data.secretNamespace // ""' 2>/dev/null)" || marker_ns=""
    fi

    out="$(kubectl get clustertriggerauthentications.keda.sh "$name" -o json 2>&1)" || rc=$?
    if [ "$rc" = 0 ]; then
        if ! printf '%s' "$out" | jq -e --arg k "$LLMSM_SCALER_TLS_LABEL" '.metadata.labels[$k] // empty' >/dev/null 2>&1; then
            log_warning "ClusterTriggerAuthentication $name was not created by this installer (no $LLMSM_SCALER_TLS_LABEL label); left in place."
            cta_gone=true
        elif kubectl delete clustertriggerauthentications.keda.sh "$name" --ignore-not-found >/dev/null 2>&1; then
            removed=true
            cta_gone=true
        else
            log_warning "Could not remove ClusterTriggerAuthentication $name."
        fi
    elif llmsm_scaler_not_found "$out"; then
        cta_gone=true
    else
        log_warning "Could not read ClusterTriggerAuthentication $name ($(printf '%s\n' "$out" | head -1)); left in place."
    fi

    # Where KEDA reads its Secrets, worked out from sources a tenant cannot
    # choose, comes first. The marker lives in the controller namespace, whose
    # admin can edit it, so it is used only when that is not known.
    llmsm_scaler_keda_object_ns
    copy_ns="$LLMSM_SCALER_KEDA_OBJ_NS"
    if [ -z "$copy_ns" ]; then
        copy_ns="$marker_ns"
    elif [ -n "$marker_ns" ] && [ "$marker_ns" != "$copy_ns" ]; then
        log_warning "ConfigMap $LLMSM_SCALER_KEDA_MARKER in $LLMSM_NS says KEDA's copy of the scaler client certificate went to $marker_ns, but KEDA reads its Secrets from $copy_ns. Only $copy_ns is cleaned up. Check $marker_ns by hand:  kubectl get secret $name -n $marker_ns --show-labels"
    fi
    if [ -n "$copy_ns" ]; then
        LLMSM_NS="$copy_ns" llmsm_scaler_object_state secret "$name"
        case "$LLMSM_OBJ_STATE" in
            absent) copy_gone=true ;;
            ours)
                if kubectl delete secret "$name" -n "$copy_ns" --ignore-not-found >/dev/null 2>&1; then
                    removed=true
                    copy_gone=true
                else
                    log_warning "Could not remove KEDA's copy of the scaler client certificate, Secret $name in $copy_ns."
                fi ;;
            foreign)
                log_warning "Secret $name in $copy_ns was not created by this installer (no $LLMSM_SCALER_TLS_LABEL label); left in place."
                copy_gone=true ;;
            unreadable) log_warning "Could not read Secret $name in $copy_ns ($LLMSM_OBJ_ERR); KEDA's copy of the scaler client key may still be there." ;;
        esac
    elif [ "$marker_state" != absent ]; then
        log_warning "Where KEDA's copy of the scaler client certificate went is not known (${LLMSM_SCALER_KEDA_OBJ_WHY:-the marker names no namespace}); Secret $name in KEDA's namespace may still be there."
    else
        # No marker and nowhere to look: nothing records a copy.
        copy_gone=true
    fi

    case "$marker_state" in
        ours)
            if [ "$cta_gone" = true ] && [ "$copy_gone" = true ]; then
                kubectl delete configmap "$LLMSM_SCALER_KEDA_MARKER" -n "$LLMSM_NS" --ignore-not-found >/dev/null 2>&1 || true
            else
                log_warning "ConfigMap $LLMSM_SCALER_KEDA_MARKER in $LLMSM_NS is kept: KEDA's side (ClusterTriggerAuthentication $name and its Secret in KEDA's namespace) was not removed, and the marker records where it is. A cluster admin removes both with:  $(llmsm_make_cmd undeploy-scaling-manager)"
            fi ;;
        foreign) log_warning "ConfigMap $LLMSM_SCALER_KEDA_MARKER in $LLMSM_NS was not created by this installer (no $LLMSM_SCALER_TLS_LABEL label); left in place." ;;
    esac
    # KEDA opens a connection for the TLS address only when a ScaledObject on
    # it dials, and then keeps it, with this client certificate, until its
    # operator restarts. So the advice applies only when one does now (or they
    # cannot be listed). One that moved off TLS earlier also left one behind;
    # the reinstall says so when it writes KEDA's side again.
    if [ "$removed" = true ]; then
        local tls_rows tls_rc=0
        tls_rows="$(llmsm_scaler_plaintext_scaledobjects plaintext)" || tls_rc=$?
        if [ "$tls_rc" != 0 ] || [ -n "$tls_rows" ]; then
            # With KEDA_RESTART=true the uninstall restarts it once those
            # ScaledObjects are gone (llmsm_scaler_keda_restart_after_undeploy).
            # By default it does not, KEDA being shared: a reinstall then finds
            # the stale connection when its ScaledObjects dial, and names the
            # restart (llmsm_scaler_keda_stale_act).
            LLMSM_SCALER_KEDA_RESTART_WANTED=true
            log_info "KEDA's side of the scaler TLS is removed, but ScaledObjects used the TLS address$([ "$tls_rc" != 0 ] && printf ' (or they cannot be listed)'), so KEDA's operator keeps a connection with the old client certificate until it restarts.$([ "${KEDA_RESTART:-false}" = true ] && printf ' KEDA_RESTART=true: this uninstall restarts it once the ScaledObjects are removed.')"
        fi
    fi
}
