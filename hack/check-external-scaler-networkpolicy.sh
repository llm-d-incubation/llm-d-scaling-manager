#!/usr/bin/env bash
#
# Renders every shipped overlay and asserts the external-scaler NetworkPolicy
# admits the KEDA external scaler port (:9090) ONLY from KEDA's namespace, while
# every other port the controller serves stays reachable.
#
# The scaler has no authentication of its own by default, and every call
# registers the workload it names with the caller's trigger metadata, so a Pod
# that reaches :9090 can rewrite another tenant's scaler metadata and read any
# decision. The policy is the boundary, and each way of getting it wrong parses
# and applies cleanly:
#
#   a podSelector that matches no controller Pod   policy selects nothing: open
#   a `from:` that is empty, {} or a bare podSelector   open to a namespace or more
#   a controller port with no rule                  metrics or probes go dark
#   the wrong KEDA namespace                        KEDA refused, nothing scales
#
# Then it executes the installer's llmsm_append_scaler_networkpolicy_patch against
# a STUBBED kubectl, renders the result, and asserts which namespace it chose,
# or that it wrote NO policy (and said :9090 is open) when no trusted source
# knows where KEDA runs -- a guess fails closed wherever it is wrong. Then that
# the policy belongs to the admin phase, and the post-deploy
# llmsm_verify_scaler_networkpolicy, which compares the policy with where KEDA runs.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FAIL=0
PASS=0
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

fail() { echo "  FAIL: $*"; FAIL=$((FAIL + 1)); }
pass() { PASS=$((PASS + 1)); }

if ! command -v yq >/dev/null 2>&1; then
    echo "FAIL: yq is required (not skipping: a skipped check reads as a pass)"
    exit 1
fi

# render <dir> <out>: kustomize build with whatever is available.
render() {
    if [ -n "${KUSTOMIZE:-}" ] && [ -x "${KUSTOMIZE}" ]; then
        "$KUSTOMIZE" build "$1" >"$2"
    elif command -v kustomize >/dev/null 2>&1; then
        command kustomize build "$1" >"$2"
    elif command -v kubectl >/dev/null 2>&1; then
        command kubectl kustomize "$1" >"$2"
    else
        echo "FAIL: neither kustomize nor kubectl is available to render overlays"
        exit 1
    fi
}

POLICY='select(.kind == "NetworkPolicy" and .metadata.name == "scaling-manager-external-scaler")'
SCALER_PORT=9090

# check_policy <label> <rendered.yaml> <expected KEDA namespace>
check_policy() {
    local label="$1" rendered="$2" want_ns="$3" np="$WORK/np.yaml"
    echo "== $label"
    local n
    n="$(yq "$POLICY | .metadata.name" "$rendered" | grep -c . || true)"
    if [ "$n" != "1" ]; then
        fail "$label: expected exactly one NetworkPolicy scaling-manager-external-scaler, rendered $n"
        return
    fi
    pass
    yq "$POLICY" "$rendered" >"$np"

    # The policy must select the controller Pod, or it restricts nothing.
    local dep_labels sel kv
    dep_labels="$(yq 'select(.kind == "Deployment" and .metadata.name == "scaling-manager-controller-manager") | .spec.template.metadata.labels | to_entries | .[] | .key + "=" + .value' "$rendered")"
    sel="$(yq '.spec.podSelector.matchLabels // {} | to_entries | .[] | .key + "=" + .value' "$np")"
    if [ -z "$sel" ] || [ -z "$dep_labels" ]; then
        fail "$label: podSelector or controller Pod labels are empty (selector: '$sel')"
    else
        local ok=1
        for kv in $sel; do
            printf '%s\n' "$dep_labels" | grep -qxF "$kv" || { fail "$label: podSelector $kv is not on the controller Pod"; ok=0; }
        done
        [ "$ok" = 1 ] && pass
    fi
    if yq '.spec.policyTypes[]' "$np" | grep -qx Ingress; then pass; else fail "$label: policyTypes lacks Ingress"; fi

    # Every port the controller serves: 9090 only from KEDA, the rest from anywhere.
    local ports port rules i open restricted
    ports="$(yq 'select(.kind == "Deployment" and .metadata.name == "scaling-manager-controller-manager") | .spec.template.spec.containers[].ports[].containerPort' "$rendered" | sort -u)"
    if ! printf '%s\n' "$ports" | grep -qx "$SCALER_PORT"; then
        fail "$label: the controller no longer serves :$SCALER_PORT -- update this check"
    fi
    rules="$(yq '.spec.ingress | length' "$np")"
    for port in $ports; do
        open=0
        restricted=0
        for ((i = 0; i < rules; i++)); do
            yq ".spec.ingress[$i].ports[].port" "$np" | grep -qx "$port" || continue
            if [ "$(yq ".spec.ingress[$i].from // [] | length" "$np")" = "0" ]; then
                open=1
            else
                restricted=1
                if [ "$port" = "$SCALER_PORT" ]; then
                    check_scaler_peers "$label" "$np" "$i" "$want_ns"
                fi
            fi
        done
        if [ "$port" = "$SCALER_PORT" ]; then
            if [ "$open" = 1 ]; then
                fail "$label: :$port is admitted by a rule with no from: (every source)"
            elif [ "$restricted" = 0 ]; then
                fail "$label: no rule admits :$port, so KEDA cannot call the scaler"
            else
                pass
            fi
        elif [ "$open" = 1 ]; then
            pass
        else
            fail "$label: :$port has no rule admitting every source (metrics and probes must stay reachable)"
        fi
    done
}

# check_scaler_peers: every peer of rule <i> must be EXACTLY a namespaceSelector
# naming <want_ns>. A bare podSelector means "this namespace", {} means "every
# namespace", and an ipBlock means whatever the CIDR says -- none is KEDA.
check_scaler_peers() {
    local label="$1" np="$2" i="$3" want_ns="$4" peers j keys labels
    peers="$(yq ".spec.ingress[$i].from | length" "$np")"
    for ((j = 0; j < peers; j++)); do
        keys="$(yq ".spec.ingress[$i].from[$j] | keys | join(\",\")" "$np")"
        labels="$(yq ".spec.ingress[$i].from[$j].namespaceSelector.matchLabels // {} | to_entries | map(.key + \"=\" + .value) | join(\",\")" "$np")"
        if [ "$keys" != "namespaceSelector" ] || [ "$labels" != "kubernetes.io/metadata.name=$want_ns" ]; then
            fail "$label: :$SCALER_PORT peer $j is {$keys} [$labels], want only namespaceSelector kubernetes.io/metadata.name=$want_ns"
        else
            pass
        fi
    done
}

for overlay in "$ROOT"/config/overlays/*/*/; do
    name="${overlay%/}"
    name="${name#"$ROOT"/}"
    want=keda-system
    case "$name" in */openshift) want=openshift-keda ;; esac
    if ! render "$overlay" "$WORK/rendered.yaml" 2>"$WORK/err"; then
        fail "$name: does not render: $(head -3 "$WORK/err")"
        continue
    fi
    check_policy "$name" "$WORK/rendered.yaml" "$want"
done

# --- The installer's patch, against a stubbed kubectl.
#
# Sourced through a CR-stripped copy: a Windows checkout can write these files
# as CRLF, and bash then stops at the first function definition.
tr -d '\r' <"$ROOT/deploy/lib/constants.sh" >"$WORK/constants.sh"
tr -d '\r' <"$ROOT/deploy/lib/common.sh" >"$WORK/common.sh"
tr -d '\r' <"$ROOT/deploy/lib/infra_scaling_manager.sh" >"$WORK/infra_scaling_manager.sh"
tr -d '\r' <"$ROOT/deploy/lib/verify.sh" >"$WORK/verify.sh"

# stub_answer <value>: a value is printed; FORBIDDEN and NOTFOUND fail the way
# the API server does, on stderr. A Forbidden read must never look like "none".
stub_answer() {
    case "$1" in
        FORBIDDEN) echo 'Error from server (Forbidden): forbidden: User "tenant" cannot get this' >&2; return 1 ;;
        NOTFOUND) echo 'Error from server (NotFound): not found' >&2; return 1 ;;
        *) [ -z "$1" ] || printf '%s\n' "$1" ;;
    esac
}

# stub_kubectl answers the reads that locate KEDA from STUB_* variables:
#   STUB_APISVC   "<namespace> <service>" behind the external-metrics APIService
#   STUB_NP       the namespace an existing scaling-manager-external-scaler policy admits
#   STUB_DEPLOYS  namespaces holding a Deployment named keda-operator -- also a
#                 decoy: any tenant can create one, so nothing may trust it
#   STUB_PODS_*   namespaces of Pods carrying a KEDA operator LABEL -- decoys:
#                 any tenant can set a label, so nothing may trust them
# Unset means not found (an empty list, for the listings).
#   STUB_WARN     non-empty: every call also prints a kubectl warning on stderr,
#                 and SUCCEEDS -- what a deprecated API or a webhook does. It
#                 must never be read as data, nor change any answer.
stub_kubectl() {
    echo "$*" >>"$WORK/kubectl.log"
    [ -z "${STUB_WARN:-}" ] || echo 'Warning: networking.k8s.io/v1beta1 is deprecated; use v1' >&2
    local ns
    case "$*" in
        "get apiservice v1beta1.external.metrics.k8s.io "*) stub_answer "${STUB_APISVC:-NOTFOUND}" ;;
        "get networkpolicy scaling-manager-external-scaler -n scaling-manager-check "*)
            case "${STUB_NP:-NOTFOUND}" in
                FORBIDDEN | NOTFOUND) stub_answer "${STUB_NP:-NOTFOUND}" ;;
                *) printf '8443 8081 |\n9090 |%s \n' "$STUB_NP" ;;
            esac
            ;;
        "get deployments -A --field-selector metadata.name=keda-operator "*)
            case "${STUB_DEPLOYS:-}" in
                FORBIDDEN) stub_answer FORBIDDEN ;;
                *) for ns in ${STUB_DEPLOYS:-}; do printf '%s\n' "$ns"; done ;;
            esac
            ;;
        *"get pods -A -l $KEDA_OPERATOR_LABEL_SELECTOR "*) stub_answer "${STUB_PODS_STD:-}" ;;
        *"get pods -A -l app=keda-operator "*) stub_answer "${STUB_PODS_APP:-}" ;;
        *) return 0 ;;
    esac
}

# install_case <label> <overlay> <want> [VAR=value...]
#   want: a namespace   the policy is rendered, admitting :9090 from it only
#         ABSENT        opted out: not rendered, and an existing one deleted
#         OPEN          no trusted source: NOT rendered, NOT deleted, and a loud
#                       warning that :9090 is open naming make setup-prereqs
#         CONTROLLER    the controller phase: no read, no warning, no delete
#         KEPT          opted out in the controller phase: nothing deleted, and
#                       the state is said to be UNKNOWN, never "open"
#         UNKNOWN       a policy may exist (unreadable, or unrecognised): NOT
#                       rendered, NOT deleted, said to be left as it is, state
#                       UNKNOWN, with the command to check it -- never "NO
#                       NetworkPolicy"
#         REFUSED       the patch function exits 1
# EXPECT_UNREAD=1 among the values also asserts it said what it could not read.
# EXPECT_CMD=<text> asserts the warnings carry that remedy command verbatim, and
# EXPECT_NOT=<text> that they never contain <text>.
install_case() {
    local label="$1" overlay="$2" want="$3" dir="$WORK/install" kv expect_unread="" expect_cmd="" expect_not=""
    shift 3
    for kv in "$@"; do
        case "$kv" in
            EXPECT_UNREAD=1) expect_unread=1 ;;
            EXPECT_CMD=*) expect_cmd="${kv#EXPECT_CMD=}" ;;
            EXPECT_NOT=*) expect_not="${kv#EXPECT_NOT=}" ;;
        esac
    done
    case "$want" in ABSENT | REFUSED | OPEN | CONTROLLER | KEPT | UNKNOWN) echo "== $label" ;; esac
    rm -rf "$dir"
    mkdir -p "$dir"
    ln -s "$ROOT/$overlay" "$dir/base"
    printf 'namespace: scaling-manager-check\nresources:\n- ./base\npatches:\n' >"$dir/kustomization.yaml"
    : >"$WORK/kubectl.log"
    : >"$WORK/warnings"
    (
        # shellcheck disable=SC1091
        . "$WORK/constants.sh"
        # shellcheck disable=SC1091
        . "$WORK/common.sh"
        # shellcheck disable=SC1091
        . "$WORK/infra_scaling_manager.sh"
        log_info() { :; }
        log_warning() { echo "$1" >>"$WORK/warnings"; }
        log_error() { echo "$1" >"$WORK/refusal"; exit 1; }
        kubectl() { stub_kubectl "$@"; }
        LLMSM_NS=scaling-manager-check
        KEDA_NAMESPACE=keda-system
        KEDA_NAMESPACE_EXPLICIT=false
        KEDA_HELM_INSTALL=false
        ENVIRONMENT=kubernetes
        local kv
        for kv in "$@"; do
            case "$kv" in EXPECT_*) continue ;; esac
            eval "$kv"
        done
        if ! declare -F llmsm_append_scaler_networkpolicy_patch >/dev/null; then
            echo "llmsm_append_scaler_networkpolicy_patch is not defined" >"$WORK/refusal"
            exit 2
        fi
        llmsm_append_scaler_networkpolicy_patch "$dir/kustomization.yaml"
    )
    local rc=$?
    if [ "$want" = REFUSED ]; then
        if [ "$rc" = 1 ]; then pass; else fail "$label: expected a refusal, got exit $rc"; fi
        return
    fi
    if [ "$rc" != 0 ]; then
        fail "$label: exit $rc: $(cat "$WORK/refusal" 2>/dev/null)"
        return
    fi
    if [ -n "$expect_cmd" ]; then
        if grep -qF -- "$expect_cmd" "$WORK/warnings"; then pass; else fail "$label: the remedy is not '$expect_cmd': $(grep -i 'make ' "$WORK/warnings" | head -2)"; fi
    fi
    if [ -n "$expect_not" ]; then
        if grep -qF -- "$expect_not" "$WORK/warnings"; then fail "$label: the warnings say '$expect_not': $(grep -F -- "$expect_not" "$WORK/warnings" | head -2)"; else pass; fi
    fi
    case "$want" in
        UNKNOWN)
            if grep -q "delete networkpolicy" "$WORK/kubectl.log"; then fail "$label: a policy that may exist was deleted"; else pass; fi
            if grep -q 'NO NetworkPolicy\|reachable by ANY Pod' "$WORK/warnings"; then
                fail "$label: a policy that may exist was reported absent and the port open: $(grep 'NO NetworkPolicy\|reachable by ANY' "$WORK/warnings" | head -1)"
            elif grep -q 'left as it is, state UNKNOWN' "$WORK/warnings" && grep -q 'kubectl get networkpolicy scaling-manager-external-scaler -n scaling-manager-check' "$WORK/warnings"; then
                pass
            else
                fail "$label: not said to be left as it is with its state unknown, and how to check it: $(head -3 "$WORK/warnings")"
            fi
            if ! render "$dir" "$WORK/installed.yaml" 2>"$WORK/err"; then
                fail "$label: the patched overlay does not render: $(head -3 "$WORK/err")"
            elif [ -z "$(yq "$POLICY | .metadata.name" "$WORK/installed.yaml")" ]; then
                pass
            else
                fail "$label: a policy was rendered over one that may exist"
            fi
            return
            ;;
        CONTROLLER)
            if [ ! -s "$WORK/kubectl.log" ]; then pass; else fail "$label: the controller phase still read the cluster for KEDA: $(tr '\n' ';' <"$WORK/kubectl.log")"; fi
            if [ ! -s "$WORK/warnings" ]; then pass; else fail "$label: the controller phase warned about a policy it does not apply: $(head -2 "$WORK/warnings")"; fi
            return
            ;;
        KEPT)
            if grep -q "delete networkpolicy" "$WORK/kubectl.log"; then fail "$label: the controller phase deleted the admin-owned policy"; else pass; fi
            # It left the admin's policy alone, so it cannot say the port is open.
            if grep -q 'reachable from every Pod\|NO NetworkPolicy' "$WORK/warnings"; then
                fail "$label: the controller phase said :9090 is open about a policy it left untouched: $(head -1 "$WORK/warnings")"
            elif grep -q 'left as it is, state UNKNOWN' "$WORK/warnings" && grep -q 'kubectl get networkpolicy scaling-manager-external-scaler -n scaling-manager-check' "$WORK/warnings"; then
                pass
            else
                fail "$label: not said to be left as it is with its state unknown: $(head -2 "$WORK/warnings")"
            fi
            return
            ;;
    esac
    if ! render "$dir" "$WORK/installed.yaml" 2>"$WORK/err"; then
        fail "$label: the patched overlay does not render: $(head -3 "$WORK/err")"
        return
    fi
    case "$want" in
        ABSENT)
            if [ -z "$(yq "$POLICY | .metadata.name" "$WORK/installed.yaml")" ]; then pass; else fail "$label: the policy is still rendered"; fi
            if grep -q "delete networkpolicy scaling-manager-external-scaler -n scaling-manager-check" "$WORK/kubectl.log"; then pass; else fail "$label: an existing policy is not removed on opt-out"; fi
            return
            ;;
        OPEN)
            if [ -z "$(yq "$POLICY | .metadata.name" "$WORK/installed.yaml")" ]; then pass; else fail "$label: a policy was rendered from a guess: admits $(yq "$POLICY | .spec.ingress[1].from[0].namespaceSelector.matchLabels" "$WORK/installed.yaml" | tr '\n' ' ')"; fi
            if grep -q "delete networkpolicy" "$WORK/kubectl.log"; then fail "$label: an existing policy was deleted on a guess"; else pass; fi
            if grep -q 'NO NetworkPolicy' "$WORK/warnings" && grep -q 'KEDA_NAMESPACE=' "$WORK/warnings" && grep -q 'make setup-prereqs' "$WORK/warnings"; then pass; else fail "$label: no loud warning that :9090 is open, naming KEDA_NAMESPACE=<ns> make setup-prereqs: $(head -3 "$WORK/warnings")"; fi
            if [ -n "$expect_unread" ]; then
                if grep -q 'Not readable' "$WORK/warnings"; then pass; else fail "$label: an unreadable source was not reported as unknown"; fi
            fi
            return
            ;;
    esac
    if [ -s "$WORK/warnings" ]; then
        fail "$label: a KNOWN namespace was warned about: $(head -2 "$WORK/warnings")"
    else
        pass
    fi
    check_policy "$label" "$WORK/installed.yaml" "$want"
}

install_case "installer: explicit KEDA_NAMESPACE wins" config/overlays/cluster-scoped/kubernetes my-keda \
    KEDA_NAMESPACE=my-keda KEDA_NAMESPACE_EXPLICIT=true "STUB_APISVC='keda keda-operator-metrics-apiserver'" STUB_DEPLOYS=elsewhere
install_case "installer: the external-metrics APIService wins over a decoy Pod" config/overlays/namespace-scoped/kubernetes keda \
    "STUB_APISVC='keda keda-operator-metrics-apiserver'" STUB_PODS_STD=tenant-decoy STUB_PODS_APP=tenant-decoy
install_case "installer: an APIService served by prometheus-adapter is not KEDA, nor is a keda-operator Deployment" config/overlays/cluster-scoped/kubernetes OPEN \
    "STUB_APISVC='monitoring prometheus-adapter'" STUB_DEPLOYS=found-keda
install_case "installer: OpenShift CMA found by its APIService" config/overlays/namespace-scoped/openshift openshift-keda \
    "STUB_APISVC='openshift-keda keda-metrics-apiserver'" ENVIRONMENT=openshift
install_case "installer: a Deployment named keda-operator is not a source (a tenant can plant one)" config/overlays/namespace-scoped/kubernetes OPEN \
    STUB_APISVC=FORBIDDEN STUB_DEPLOYS=tenant-planted EXPECT_UNREAD=1
install_case "installer: a Pod labelled app=keda-operator is not KEDA" config/overlays/cluster-scoped/kubernetes OPEN \
    STUB_PODS_STD=tenant-ns STUB_PODS_APP=tenant-ns
install_case "installer: two label decoys do not abort the install" config/overlays/cluster-scoped/kubernetes OPEN \
    STUB_PODS_STD=keda-a STUB_PODS_APP=keda-b
install_case "installer: two keda-operator Deployments no longer block the install" config/overlays/cluster-scoped/kubernetes OPEN \
    "STUB_DEPLOYS='keda-a keda-b'"
install_case "installer: an existing policy's namespace is kept" config/overlays/namespace-scoped/kubernetes admin-keda \
    STUB_APISVC=FORBIDDEN STUB_NP=admin-keda STUB_DEPLOYS=other-keda
# Unreadable is not absent: a policy may be there, and it is left as it is.
# Saying "NO NetworkPolicy ... open" about it sent an admin with a correct
# policy to "fix" a port that was closed.
install_case "installer: an unreadable policy is UNKNOWN, left as is, never called absent" config/overlays/cluster-scoped/kubernetes UNKNOWN \
    STUB_APISVC=FORBIDDEN STUB_NP=FORBIDDEN STUB_DEPLOYS=FORBIDDEN STUB_PODS_STD=FORBIDDEN STUB_PODS_APP=FORBIDDEN
install_case "installer: a policy admitting :9090 from two namespaces is UNKNOWN, left as is" config/overlays/namespace-scoped/kubernetes UNKNOWN \
    "STUB_NP='keda-a keda-b'"
# The remedy names THIS install's shape. NAMESPACE=$LLMSM_NS was right only for a
# self-managed controller: out of reach, it set up llmsm-<ns> as the managed
# namespace; cluster-scoped, it dropped SCOPE=cluster.
install_case "installer remedy: self-managed shape" config/overlays/namespace-scoped/kubernetes OPEN \
    "EXPECT_CMD=KEDA_NAMESPACE=<namespace KEDA runs in> NAMESPACE=scaling-manager-check make setup-prereqs"
install_case "installer remedy: out-of-reach shape names the MANAGED namespace" config/overlays/namespace-scoped/kubernetes OPEN \
    LLMSM_WATCH_NS=tenant \
    "EXPECT_CMD=KEDA_NAMESPACE=<namespace KEDA runs in> NAMESPACE=tenant make setup-prereqs LLMSM_NS=scaling-manager-check LLMSM_WATCH_NS=tenant" \
    "EXPECT_NOT=NAMESPACE=scaling-manager-check"
install_case "installer remedy: cluster scope says SCOPE=cluster" config/overlays/cluster-scoped/kubernetes OPEN \
    LLMSM_SCOPE=cluster \
    "EXPECT_CMD=KEDA_NAMESPACE=<namespace KEDA runs in> make setup-prereqs SCOPE=cluster LLMSM_NS=scaling-manager-check" \
    "EXPECT_NOT=NAMESPACE=scaling-manager-check"
# A warning kubectl prints on stderr while succeeding is not data.
install_case "installer: a kubectl warning on stderr does not change where KEDA is" config/overlays/cluster-scoped/kubernetes keda \
    STUB_WARN=1 "STUB_APISVC='keda keda-operator-metrics-apiserver'"
install_case "installer: a kubectl warning on stderr does not change the policy kept" config/overlays/namespace-scoped/kubernetes admin-keda \
    STUB_WARN=1 STUB_APISVC=FORBIDDEN STUB_NP=admin-keda
install_case "installer: Forbidden on OpenShift: no openshift-keda guess" config/overlays/namespace-scoped/openshift OPEN \
    STUB_APISVC=FORBIDDEN STUB_DEPLOYS=FORBIDDEN ENVIRONMENT=openshift EXPECT_UNREAD=1
install_case "installer: nothing visible on Kubernetes: no keda-system guess" config/overlays/cluster-scoped/kubernetes OPEN
install_case "installer: Helm-installed KEDA is not there yet" config/overlays/cluster-scoped/kubernetes helm-keda \
    KEDA_NAMESPACE=helm-keda KEDA_HELM_INSTALL=true
install_case "installer: KEDA_HELM_INSTALL=true with KEDA already elsewhere follows the APIService" config/overlays/cluster-scoped/kubernetes keda \
    KEDA_HELM_INSTALL=true "STUB_APISVC='keda keda-operator-metrics-apiserver'"
install_case "installer: KEDA_HELM_INSTALL=true with the APIService unreadable is no answer" config/overlays/cluster-scoped/kubernetes OPEN \
    KEDA_NAMESPACE=helm-keda KEDA_HELM_INSTALL=true STUB_APISVC=FORBIDDEN EXPECT_UNREAD=1
install_case "installer: the prereqs phase writes the policy it can locate" config/overlays/namespace-scoped/kubernetes keda \
    LLMSM_APPLY_SCOPE=prereqs "STUB_APISVC='keda keda-operator-metrics-apiserver'"
install_case "installer: the controller phase neither reads for KEDA nor warns" config/overlays/namespace-scoped/kubernetes CONTROLLER \
    LLMSM_APPLY_SCOPE=controller STUB_APISVC=FORBIDDEN STUB_NP=FORBIDDEN
install_case "installer: LLMSM_SCALER_NETWORKPOLICY=false drops the policy" config/overlays/cluster-scoped/kubernetes ABSENT \
    LLMSM_SCALER_NETWORKPOLICY=false
install_case "installer: opting out in the controller phase leaves the admin policy alone" config/overlays/namespace-scoped/kubernetes KEPT \
    LLMSM_SCALER_NETWORKPOLICY=false LLMSM_APPLY_SCOPE=controller
install_case "installer: LLMSM_SCALER_NETWORKPOLICY=bogus refuses" config/overlays/cluster-scoped/kubernetes REFUSED \
    LLMSM_SCALER_NETWORKPOLICY=bogus

# --- The CI kind e2e: `make deploy-e2e-infra` must hand the install the
# namespace kind's KEDA is Helm-installed into.
#
# No source the resolver trusts answers on kind: deploy_keda installs KEDA AFTER
# the controller, so there is no external-metrics APIService when the policy is
# decided. Without an explicit KEDA_NAMESPACE the e2e installed with :9090 open,
# and the policy users get was never exercised. What the recipe passes is read
# by running it under `make -n` with $(MAKE) replaced by a recorder -- a line
# that references $(MAKE) runs even under -n -- and then fed to the resolver the
# way deploy/install.sh turns it into KEDA_NAMESPACE_EXPLICIT.
if ! command -v make >/dev/null 2>&1; then
    fail "deploy-e2e-infra: make is required for this check (not skipping: a skipped check reads as a pass)"
else
    mkdir -p "$WORK/mk"
    cat >"$WORK/mk/make" <<'MKEOF'
#!/usr/bin/env bash
printf '%s|%s\n' "${KEDA_NAMESPACE-<unset>}" "$*" >>"$MKLOG"
MKEOF
    chmod +x "$WORK/mk/make"
    # e2e_keda_ns <ENVIRONMENT> [make VAR=value...]: the KEDA_NAMESPACE the
    # deploy-scaling-manager sub-make receives, or <unset>, or <not reached>.
    e2e_keda_ns() {
        local env="$1" line
        shift
        : >"$WORK/mk.log"
        (cd "$ROOT" && env -u KEDA_NAMESPACE -u E2E_KEDA_NAMESPACE MKLOG="$WORK/mk.log" \
            make --no-print-directory -n deploy-e2e-infra ENVIRONMENT="$env" MAKE="$WORK/mk/make" "$@" >/dev/null 2>&1)
        line="$(grep '|deploy-scaling-manager ' "$WORK/mk.log" | head -1)"
        if [ -z "$line" ]; then echo '<not reached>'; else echo "${line%%|*}"; fi
    }
    if ! tr -d '\r' <"$ROOT/deploy/install.sh" | grep -qF '[ -n "${KEDA_NAMESPACE:-}" ] && KEDA_NAMESPACE_EXPLICIT=true'; then
        fail "deploy/install.sh no longer derives KEDA_NAMESPACE_EXPLICIT from a set KEDA_NAMESPACE; update this check"
    fi
    for img in "IMG=" "IMG=quay.io/example/scaling-manager:e2e"; do
        echo "== make deploy-e2e-infra on kind ($img)"
        got="$(e2e_keda_ns kind-emulator "$img" SKIP_BUILD=true)"
        case "$got" in
            '<unset>' | '<not reached>' | '')
                fail "deploy-e2e-infra on kind ($img): deploy-scaling-manager got KEDA_NAMESPACE $got, so the install writes no NetworkPolicy in CI" ;;
            *)
                pass
                install_case "make deploy-e2e-infra on kind ($img): the policy admits KEDA from $got" \
                    config/overlays/cluster-scoped/kubernetes "$got" \
                    ENVIRONMENT=kind-emulator "KEDA_NAMESPACE=$got" KEDA_NAMESPACE_EXPLICIT=true \
                    KEDA_HELM_INSTALL=false STUB_APISVC=NOTFOUND LLMSM_SCOPE=cluster
                ;;
        esac
    done
    echo "== make deploy-e2e-infra elsewhere brings its own KEDA"
    got="$(e2e_keda_ns openshift IMG=)"
    if [ "$got" = '<unset>' ]; then pass; else fail "deploy-e2e-infra on openshift passed KEDA_NAMESPACE=$got; that platform's KEDA namespace is not this target's to name"; fi
fi

# --- Which phase owns the policy: the admin's, by name (LLMSM_PREREQ_NAMED).
#
# The tenant phase cannot read where KEDA runs, so a policy it applied was a
# guess. The phase split must put the policy in the prereqs half, and only there.
echo "== phase split: the policy is an admin-phase object"
if render "$ROOT/config/overlays/namespace-scoped/kubernetes" "$WORK/split.yaml" 2>"$WORK/err"; then
    sel="$(
        # shellcheck disable=SC1091
        . "$WORK/constants.sh"
        # shellcheck disable=SC1091
        . "$WORK/common.sh"
        yq "$(llmsm_prereq_kind_filter select) | .kind + \"/\" + .metadata.name" "$WORK/split.yaml" | grep -cx 'NetworkPolicy/scaling-manager-external-scaler'
    )"
    exc="$(
        # shellcheck disable=SC1091
        . "$WORK/constants.sh"
        # shellcheck disable=SC1091
        . "$WORK/common.sh"
        yq "$(llmsm_prereq_kind_filter exclude) | .kind + \"/\" + .metadata.name" "$WORK/split.yaml" | grep -cx 'NetworkPolicy/scaling-manager-external-scaler'
    )"
    if [ "$sel" = 1 ]; then pass; else fail "phase split: the prereqs phase does not apply NetworkPolicy/scaling-manager-external-scaler (selected $sel)"; fi
    if [ "$exc" = 0 ]; then pass; else fail "phase split: the controller phase still applies NetworkPolicy/scaling-manager-external-scaler"; fi
else
    fail "phase split: the namespace-scoped overlay does not render: $(head -3 "$WORK/err")"
fi

# --- The post-deploy check: does the policy admit the namespace KEDA runs in?
#
# verify_case <label> <want: WARN | OPEN | UNKNOWN | QUIET> [VAR=value...]
# WARN: the mismatch warning is printed. OPEN: the "no NetworkPolicy: scaler
# port open" warning. UNKNOWN: a policy is there but not recognised -- said as
# UNKNOWN, never as open. QUIET: no warning at all.
verify_case() {
    local label="$1" want="$2"
    shift 2
    echo "== $label"
    : >"$WORK/warnings"
    : >"$WORK/kubectl.log"
    (
        # shellcheck disable=SC1091
        . "$WORK/constants.sh"
        # shellcheck disable=SC1091
        . "$WORK/common.sh"
        # shellcheck disable=SC1091
        . "$WORK/verify.sh"
        log_info() { :; }
        log_success() { :; }
        log_warning() { echo "$1" >>"$WORK/warnings"; }
        kubectl() { stub_kubectl "$@"; }
        LLMSM_NS=scaling-manager-check
        local kv
        for kv in "$@"; do eval "$kv"; done
        declare -F llmsm_verify_scaler_networkpolicy >/dev/null || exit 2
        llmsm_verify_scaler_networkpolicy
    )
    local rc=$?
    if [ "$rc" != 0 ]; then
        fail "$label: exit $rc (llmsm_verify_scaler_networkpolicy missing or failing)"
        return
    fi
    case "$want" in
        WARN)
            if grep -q 'NOTHING SCALES' "$WORK/warnings" && grep -q KEDA_NAMESPACE "$WORK/warnings"; then pass; else fail "$label: no loud mismatch warning naming KEDA_NAMESPACE"; fi
            ;;
        OPEN)
            if grep -q 'No NetworkPolicy: scaler port open' "$WORK/warnings"; then pass; else fail "$label: no warning that the scaler port is open: $(head -2 "$WORK/warnings")"; fi
            ;;
        UNKNOWN)
            if grep -q 'state UNKNOWN' "$WORK/warnings" && ! grep -q 'scaler port open' "$WORK/warnings"; then pass; else fail "$label: not reported as UNKNOWN: $(head -2 "$WORK/warnings")"; fi
            ;;
        QUIET)
            if [ ! -s "$WORK/warnings" ]; then pass; else fail "$label: warned: $(head -2 "$WORK/warnings")"; fi
            ;;
    esac
}

verify_case "verify: the policy admits keda-system, the APIService says keda" WARN \
    STUB_NP=keda-system "STUB_APISVC='keda keda-operator-metrics-apiserver'"
verify_case "verify: the policy admits where the APIService says KEDA runs" QUIET \
    STUB_NP=keda "STUB_APISVC='keda keda-operator-metrics-apiserver'"
verify_case "verify: a keda-operator Deployment cannot call the policy wrong" QUIET \
    STUB_NP=keda-system STUB_DEPLOYS=tenant-planted
verify_case "verify: the APIService decides even beside a decoy Deployment" WARN \
    STUB_NP=keda-system STUB_DEPLOYS=keda-system "STUB_APISVC='keda keda-operator-metrics-apiserver'"
verify_case "verify: nothing readable is not a mismatch" QUIET \
    STUB_NP=keda-system STUB_DEPLOYS=FORBIDDEN STUB_APISVC=FORBIDDEN
verify_case "verify: no policy: the scaler port is open, said as a warning" OPEN \
    STUB_DEPLOYS=keda
verify_case "verify: a policy admitting :9090 from two namespaces is UNKNOWN, not open" UNKNOWN     "STUB_NP='keda-a keda-b'"
verify_case "verify: opted out: still said, the port is open" OPEN \
    LLMSM_SCALER_NETWORKPOLICY=false
# The remedy repeats the install's shape (llmsm_make_cmd).
verify_case "verify: out-of-reach shape, the port is open" OPEN \
    STUB_DEPLOYS=keda LLMSM_WATCH_NS=tenant
if grep -qF 'KEDA_NAMESPACE=<namespace KEDA runs in> NAMESPACE=tenant make setup-prereqs LLMSM_NS=scaling-manager-check LLMSM_WATCH_NS=tenant' "$WORK/warnings" \
    && ! grep -qF 'NAMESPACE=scaling-manager-check' "$WORK/warnings"; then
    pass
else
    fail "verify remedy, out-of-reach shape: not the managed namespace's command: $(grep 'make ' "$WORK/warnings" | head -1)"
fi

echo ""
if [ "$FAIL" -gt 0 ]; then
    echo "external-scaler NetworkPolicy: $FAIL failed, $PASS passed"
    exit 1
fi
if [ "$PASS" = 0 ]; then
    echo "FAIL: no assertion ran"
    exit 1
fi
echo "external-scaler NetworkPolicy OK ($PASS assertions)"
