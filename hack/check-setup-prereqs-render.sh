#!/usr/bin/env bash
#
# Executes `make setup-prereqs RENDER=<dir>` -- deploy/install.sh, prereqs
# phase -- against STUB kubectl and helm executables, and asserts what a render
# promises: it makes no mutating call, and what it writes is what an admin can
# run. Nothing here touches a cluster: KUBECONFIG points at nothing, and the
# stub answers every read.
#
# The stub cluster is a fresh one: namespaces team-a and team-b, no KEDA, no
# Prometheus Operator, no CRDs. Cases vary that. Every call is logged; a call
# that would change the cluster is logged as MUTATION and still succeeds, so a
# tree without render mode runs to the end and the mutation count is the
# evidence.
#
# Pinned here:
#   - a render makes zero mutating kubectl or helm calls, cluster and
#     namespaced scope;
#   - admin.yaml is non-empty, parses, carries no server fields, and starts
#     with the Namespaces, then RBAC;
#   - admin.sh parses, applies admin.yaml, and installs KEDA with Helm (exact
#     chart and version) when KEDA_HELM_INSTALL=true finds none -- and not when
#     KEDA is there;
#   - namespaced LLMSM_WATCH_NS=team-a,team-b renders the manager Role and
#     RoleBinding in each;
#   - deletes of objects already on the cluster (an immutable roleRef) are
#     recorded in admin.sh, not made;
#   - a write question (`auth can-i create`) is answered for the admin, a read
#     question by the cluster;
#   - a child process that calls kubectl directly cannot write;
#   - RENDER refuses the controller phase and --check.

set -u

# CHECK_ROOT runs the check against another tree -- how its negative control is
# taken: point it at an extracted older deploy/ and config/ and cases fail.
ROOT="${CHECK_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
FAIL=0
PASS=0
# Every case reports ok or FAIL exactly once; a run that stops early must not
# pass having checked less. Change this deliberately with the cases.
EXPECT_CASES=27

for tool in jq yq openssl; do
    command -v "$tool" >/dev/null 2>&1 || { printf 'FATAL: %s is required.\n' "$tool" >&2; exit 1; }
done
# Under lint-deploy-scripts, command -v kubectl is hack/offline-kubectl, which the
# stubbed environment of this check would not hand the real kubectl on to; the real one is
# what its stub passes --local and --dry-run=client to.
REAL_KUBECTL="${OFFLINE_KUBECTL_REAL:-$(command -v kubectl || true)}"
[ -n "$REAL_KUBECTL" ] || { printf 'FATAL: kubectl is required (its offline --local and --dry-run=client output).\n' >&2; exit 1; }
# The directories of the tools the installer needs, behind the stubs.
TOOLPATH=""
for t in yq jq openssl git; do
    d="$(dirname "$(command -v "$t")")"
    case ":$TOOLPATH:" in *":$d:"*) ;; *) TOOLPATH="${TOOLPATH:+$TOOLPATH:}$d" ;; esac
done
if [ -n "${KUSTOMIZE:-}" ] && [ -x "$KUSTOMIZE" ]; then
    KBUILD="$KUSTOMIZE build"
elif [ -x "$ROOT/bin/kustomize" ]; then
    KBUILD="$ROOT/bin/kustomize build"
elif command -v kustomize >/dev/null 2>&1; then
    KBUILD="kustomize build"
else
    KBUILD="$REAL_KUBECTL kustomize"
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
# The tree under test, CR-stripped: a Windows checkout writes CRLF.
mkdir -p "$WORK/tree"
cp -r "$ROOT/deploy" "$ROOT/config" "$WORK/tree/"
find "$WORK/tree/deploy" -name '*.sh' -exec sed -i 's/\r$//' {} +
TREE="$WORK/tree"

ok()   { PASS=$((PASS + 1)); echo "ok   $1"; }
fail() { FAIL=$((FAIL + 1)); echo "FAIL $1"; [ -n "${2:-}" ] && printf '%s\n' "$2" | head -40 | sed 's/^/       /'; }

# --- the stub -----------------------------------------------------------------
#
#   STUB_NS        namespaces that exist (space-separated)
#   STUB_KEDA      present: KEDA's CRD, API, ready operator and APIService
#   STUB_CRB       "<name>=<roleRef>" ClusterRoleBindings that exist
#   STUB_DENY      true: `auth can-i` answers no (a read-only identity)
#   STUB_LOG       every call; mutating ones prefixed MUTATION
mkdir -p "$WORK/bin"
cat > "$WORK/bin/kubectl" <<STUBEOF
#!/usr/bin/env bash
REAL="$REAL_KUBECTL"
KBUILD="$KBUILD"
STUBEOF
cat >> "$WORK/bin/kubectl" <<'STUBEOF'
log() { printf '%s\n' "$*" >> "$STUB_LOG"; }
offline() { KUBECONFIG="$STUB_WORK/none" "$REAL" "$@" 2>/dev/null; }
notfound() { echo "Error from server (NotFound): $1 \"$2\" not found" >&2; exit 1; }
emit() { # emit <json> <format>
    local f="$STUB_WORK/emit.$$.json"
    printf '%s\n' "$1" > "$f"
    case "$2" in
        json) cat "$f" ;;
        "") jq -r '.metadata.name' "$f" ;;
        *) offline annotate --local -f "$f" stub-noop- -o "$2" ;;
    esac
    rm -f "$f"
}
args=("$@")
case " $* " in
    *" --local "*) log "kubectl $*"; offline "$@"; exit ;;
    *" --dry-run=client "*) log "kubectl $*"; offline "$@"; exit ;;
esac
verb="" rest=() skip=0
for a in "$@"; do
    if [ "$skip" = 1 ]; then skip=0; continue; fi
    case "$a" in --context|--kubeconfig|-n|--namespace) [ -z "$verb" ] && { skip=1; continue; } ;; esac
    if [ -z "$verb" ]; then case "$a" in -*) continue ;; *) verb="$a"; continue ;; esac; fi
    rest+=("$a")
done
case "$verb" in
    apply|create|patch|label|annotate|delete|replace|scale|edit|set|taint|drain|cordon)
        case " $* " in *" --dry-run=server "*) log "kubectl $*"; cat >/dev/null 2>&1; exit 0 ;; esac
        log "MUTATION kubectl $*"
        cat >/dev/null 2>&1 || true
        exit 0 ;;
    rollout)
        case " $* " in *" restart "*) log "MUTATION kubectl $*"; exit 0 ;; esac
        log "kubectl $*"; exit 0 ;;
    wait) log "kubectl $*"; exit 0 ;;
    kustomize) log "kubectl $*"; $KBUILD "${rest[@]}"; exit ;;
esac
log "kubectl $*"
case "$verb" in
    config)
        case " $* " in
            *" current-context "*) echo stub-ctx ;;
            *" view "*) echo "https://stub.invalid:6443" ;;
        esac
        exit 0 ;;
    version) echo '{"clientVersion":{"gitVersion":"v1.31.4"}}'; exit 0 ;;
    api-resources)
        case " $* " in *"route.openshift.io"*) exit 0 ;; esac
        cat <<'EOF'
namespaces        ns    v1                                false  Namespace
configmaps        cm    v1                                true   ConfigMap
secrets                 v1                                true   Secret
serviceaccounts   sa    v1                                true   ServiceAccount
services          svc   v1                                true   Service
deployments       deploy apps/v1                          true   Deployment
networkpolicies   netpol networking.k8s.io/v1             true   NetworkPolicy
clusterroles            rbac.authorization.k8s.io/v1      false  ClusterRole
clusterrolebindings     rbac.authorization.k8s.io/v1      false  ClusterRoleBinding
roles                   rbac.authorization.k8s.io/v1      true   Role
rolebindings            rbac.authorization.k8s.io/v1      true   RoleBinding
EOF
        exit 0 ;;
    auth)
        case " $* " in
            *" can-i "*)
                if [ "${STUB_DENY:-false}" = true ]; then echo no; exit 1; fi
                echo yes; exit 0 ;;
        esac
        exit 0 ;;
    get) ;;
    *) exit 0 ;;
esac
# --- get ---
res="" name="" ns="" fmt="" list=false raw=""
set -- "${args[@]}"
pos=0
while [ $# -gt 0 ]; do
    a="$1"; shift
    case "$a" in
        get) [ "$pos" = 0 ] && { pos=1; continue; } ;;
    esac
    case "$a" in
        -n|--namespace) ns="$1"; shift ;;
        --context|--kubeconfig) shift ;;
        -o|--output) fmt="$1"; shift ;;
        -o*) fmt="${a#-o}"; fmt="${fmt#=}" ;;
        --output=*) fmt="${a#*=}" ;;
        -A|--all-namespaces) list=true ;;
        -l|--selector|--field-selector) list=true; shift ;;
        --raw) raw="$1"; shift ;;
        -*) ;;
        *) if [ -z "$res" ]; then res="$a"; else name="$a"; fi ;;
    esac
done
case "$res" in */*) name="${res#*/}"; res="${res%%/*}" ;; esac
if [ -n "$raw" ]; then
    case "$raw" in
        /version) echo '{"gitVersion":"v1.31.0"}'; exit 0 ;;
        /apis/keda.sh/v1alpha1)
            [ "${STUB_KEDA:-}" = present ] && { echo '{"resources":[{"name":"scaledobjects"},{"name":"clustertriggerauthentications"}]}'; exit 0; }
            echo 'Error from server (NotFound): the server could not find the requested resource' >&2; exit 1 ;;
        *) echo 'Error from server (NotFound): the server could not find the requested resource' >&2; exit 1 ;;
    esac
fi
[ -n "$name" ] || list=true
if [ "$list" = true ]; then
    case "$res:${STUB_KEDA:-}" in
        pods:present)
            case " ${args[*]} " in
                *"keda-operator"*)
                    item='{"apiVersion":"v1","kind":"Pod","metadata":{"name":"keda-operator-1","namespace":"keda-system","labels":{"app.kubernetes.io/name":"keda-operator"}},"status":{"conditions":[{"type":"Ready","status":"True"}]}}'
                    case "$fmt" in
                        json) printf '{"apiVersion":"v1","kind":"List","items":[%s]}\n' "$item" ;;
                        name) echo pod/keda-operator-1 ;;
                        *) emit "$item" "$(printf '%s' "$fmt" | sed 's/{range \.items\[\*\]}//; s/{end}$//')" ;;
                    esac
                    exit 0 ;;
            esac ;;
    esac
    case "$fmt" in
        json) echo '{"apiVersion":"v1","kind":"List","items":[]}' ;;
        yaml) printf 'apiVersion: v1\nkind: List\nitems: []\n' ;;
        "") echo "No resources found" >&2 ;;
    esac
    exit 0
fi
obj=""
case "$res" in
    namespace|namespaces|ns)
        case " ${STUB_NS:-} " in
            *" $name "*) obj="{\"apiVersion\":\"v1\",\"kind\":\"Namespace\",\"metadata\":{\"name\":\"$name\"},\"status\":{\"phase\":\"Active\"}}" ;;
        esac ;;
    crd|crds|customresourcedefinition|customresourcedefinitions)
        case "$name:${STUB_KEDA:-}" in
            scaledobjects.keda.sh:present)
                obj='{"apiVersion":"apiextensions.k8s.io/v1","kind":"CustomResourceDefinition","metadata":{"name":"scaledobjects.keda.sh"},"spec":{"versions":[{"name":"v1alpha1","schema":{"openAPIV3Schema":{"properties":{"spec":{"properties":{"fallback":{"properties":{"behavior":{"type":"string"}}}}}}}}}]}}' ;;
        esac ;;
    apiservice|apiservices)
        [ "$name:${STUB_KEDA:-}" = "v1beta1.external.metrics.k8s.io:present" ] && \
            obj='{"apiVersion":"apiregistration.k8s.io/v1","kind":"APIService","metadata":{"name":"v1beta1.external.metrics.k8s.io"},"spec":{"service":{"namespace":"keda-system","name":"keda-operator-metrics-apiserver"}}}' ;;
    deployment|deployments|deploy)
        [ "$name:$ns:${STUB_KEDA:-}" = "keda-operator:keda-system:present" ] && \
            obj='{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"keda-operator","namespace":"keda-system"}}' ;;
    clusterrolebinding|clusterrolebindings)
        for crb in ${STUB_CRB:-}; do
            if [ "${crb%%=*}" = "$name" ]; then
                obj="{\"apiVersion\":\"rbac.authorization.k8s.io/v1\",\"kind\":\"ClusterRoleBinding\",\"metadata\":{\"name\":\"$name\"},\"roleRef\":{\"apiGroup\":\"rbac.authorization.k8s.io\",\"kind\":\"ClusterRole\",\"name\":\"${crb#*=}\"},\"subjects\":[]}"
            fi
        done ;;
esac
[ -n "$obj" ] || notfound "$res" "$name"
emit "$obj" "$fmt"
STUBEOF
cat > "$WORK/bin/helm" <<'STUBEOF'
#!/usr/bin/env bash
case " $* " in
    *" upgrade "*|*" install "*|*" uninstall "*|*" rollback "*|*" repo add "*|*" repo update "*)
        printf 'MUTATION helm %s\n' "$*" >> "$STUB_LOG"; exit 0 ;;
esac
printf 'helm %s\n' "$*" >> "$STUB_LOG"
case " $* " in
    *" list "*) echo "[]" ;;
    *" status "*) echo "Error: release: not found" >&2; exit 1 ;;
    *" version "*) echo "v3.16.3+g" ;;
esac
exit 0
STUBEOF
chmod +x "$WORK/bin/kubectl" "$WORK/bin/helm"
: > "$WORK/none"

# run_render <out-dir> VAR=value...: one render, in a clean environment, as
# `make setup-prereqs RENDER=<dir>` runs it. Leaves rc, out, muts.
run_render() {
    local dir="$1"
    shift
    export STUB_LOG="$WORK/calls.$RANDOM.log"
    : > "$STUB_LOG"
    out="$(cd "$TREE" && env -i HOME="$WORK" PATH="$WORK/bin:$TOOLPATH:/usr/local/bin:/usr/bin:/bin" \
        KUBECONFIG="$WORK/none" STUB_LOG="$STUB_LOG" STUB_WORK="$WORK" TMPDIR="$WORK" \
        LLMSM_PROJECT="$ROOT" ENVIRONMENT=kubernetes INSTALL_PHASE=prereqs \
        SKIP_CHECKS=true SKIP_HELM_REPO_UPDATE=true NAMESPACE=team-a \
        LLMSM_NS=scaling-manager-system \
        RENDER="$dir" "$@" bash "$TREE/deploy/install.sh" 2>&1)"
    rc=$?
    muts="$(grep -c '^MUTATION' "$STUB_LOG" || true)"
}

# yaml_kinds <file> echoes "Kind ns/name" per document.
yaml_kinds() {
    yq '(.kind // "") + " " + (.metadata.namespace // "") + "/" + (.metadata.name // "")' "$1" 2>/dev/null | grep -vE '^(---| /)$'
}

# --- cluster scope, fresh cluster, KEDA_HELM_INSTALL=true ----------------------

D1="$WORK/r1"
run_render "$D1" STUB_NS="default kube-system team-a team-b" LLMSM_SCOPE=cluster KEDA_HELM_INSTALL=true
if [ "$rc" -eq 0 ]; then ok "cluster: the render exits 0"; else fail "cluster: the render must exit 0 (rc=$rc)" "$(printf '%s\n' "$out" | tail -25)"; fi
if [ "$muts" = 0 ]; then
    ok "cluster: the render made zero mutating kubectl/helm calls"
else
    fail "cluster: the render made $muts mutating call(s)" "$(grep '^MUTATION' "$STUB_LOG" | head -10)"
fi
if [ -s "$D1/admin.yaml" ] && yq 'true' "$D1/admin.yaml" >/dev/null 2>&1 \
    && [ "$(yq '.kind // "EMPTY"' "$D1/admin.yaml" | grep -cx 'EMPTY')" = 0 ]; then
    ok "cluster: admin.yaml is non-empty, parses, and holds no empty document ($(grep -c '^kind: ' "$D1/admin.yaml") objects)"
else
    fail "cluster: admin.yaml must exist, parse, and hold no empty document" "$(ls -la "$D1" 2>&1; yq '.kind // "EMPTY"' "$D1/admin.yaml" 2>&1 | sort | uniq -c | head)"
fi
if [ -s "$D1/admin.yaml" ] && ! grep -qE '^\s*(resourceVersion|uid|creationTimestamp|managedFields|generation):|^status:|last-applied-configuration' "$D1/admin.yaml"; then
    ok "cluster: admin.yaml carries no server fields"
else
    fail "cluster: admin.yaml must carry no status, uid, resourceVersion, creationTimestamp, managedFields, generation or last-applied" \
        "$(grep -nE '^\s*(resourceVersion|uid|creationTimestamp|managedFields|generation):|^status:|last-applied-configuration' "$D1/admin.yaml" 2>&1 | head -5)"
fi
kinds1="$(yaml_kinds "$D1/admin.yaml" 2>/dev/null)"
if printf '%s\n' "$kinds1" | head -1 | grep -q '^Namespace ' \
    && printf '%s\n' "$kinds1" | grep -q '^Namespace /scaling-manager-system$' \
    && printf '%s\n' "$kinds1" | grep -q '^ClusterRole /' \
    && printf '%s\n' "$kinds1" | grep -q '^ClusterRoleBinding /'; then
    ok "cluster: admin.yaml starts with the Namespaces and carries the cluster RBAC"
else
    fail "cluster: admin.yaml must start with Namespaces and carry ClusterRoles and ClusterRoleBindings" "$(printf '%s\n' "$kinds1" | head -12)"
fi
last_ns="$(printf '%s\n' "$kinds1" | grep -n '^Namespace ' | tail -1 | cut -d: -f1)"
first_rbac="$(printf '%s\n' "$kinds1" | grep -nE '^(ClusterRole|ClusterRoleBinding|Role|RoleBinding) ' | head -1 | cut -d: -f1)"
first_other="$(printf '%s\n' "$kinds1" | grep -nvE '^(Namespace|CustomResourceDefinition|ClusterRole|ClusterRoleBinding|Role|RoleBinding) ' | head -1 | cut -d: -f1)"
if [ -n "$last_ns" ] && [ -n "$first_rbac" ] && [ -n "$first_other" ] && [ "$last_ns" -lt "$first_rbac" ] && [ "$first_rbac" -lt "$first_other" ]; then
    ok "cluster: apply order is namespaces, then RBAC, then the rest"
else
    fail "cluster: apply order must be namespaces, RBAC, rest (last ns=$last_ns, first rbac=$first_rbac, first other=$first_other)"
fi
if [ -s "$D1/admin.sh" ] && bash -n "$D1/admin.sh" 2>/dev/null; then
    ok "cluster: admin.sh exists and parses"
else
    fail "cluster: admin.sh must exist and parse" "$(bash -n "$D1/admin.sh" 2>&1 | head -5)"
fi
if grep -qE '^helm upgrade -i keda kedacore/keda --version 2\.21\.0 -n keda-system ' "$D1/admin.sh" 2>/dev/null; then
    ok "cluster: admin.sh installs KEDA with Helm, chart kedacore/keda 2.21.0"
else
    fail "cluster: admin.sh must carry the KEDA Helm install (KEDA_HELM_INSTALL=true, none on the cluster)" "$(grep -n 'helm' "$D1/admin.sh" 2>&1 | head -5)"
fi
if grep -qE '^helm upgrade --install kube-prometheus-stack prometheus-community/kube-prometheus-stack --version 92\.7\.0 ' "$D1/admin.sh" 2>/dev/null; then
    ok "cluster: admin.sh installs kube-prometheus-stack 92.7.0 (the cluster has no Prometheus)"
else
    fail "cluster: admin.sh must carry the kube-prometheus-stack Helm install" "$(grep -n 'helm' "$D1/admin.sh" 2>&1 | head -5)"
fi
if grep -qx 'kubectl apply -f admin.yaml' "$D1/admin.sh" 2>/dev/null \
    && [ "$(grep -n 'helm upgrade -i keda' "$D1/admin.sh" | head -1 | cut -d: -f1)" -lt "$(grep -nx 'kubectl apply -f admin.yaml' "$D1/admin.sh" | tail -1 | cut -d: -f1)" ]; then
    ok "cluster: admin.sh applies admin.yaml after the Helm installs"
else
    fail "cluster: admin.sh must end with kubectl apply -f admin.yaml, after the Helm installs"
fi
if awk '/^(helm|kubectl) / { if (prev !~ /^#/) bad = 1 } { prev = $0 } END { exit bad }' "$D1/admin.sh" 2>/dev/null; then
    ok "cluster: every command in admin.sh has a comment above it"
else
    fail "cluster: every helm/kubectl line in admin.sh must have a comment above it" "$(grep -nB1 -E '^(helm|kubectl) ' "$D1/admin.sh" | head -12)"
fi
if printf '%s' "$out" | grep -q "bash $D1/admin.sh"; then
    ok "cluster: the run ends with the command the admin runs"
else
    fail "cluster: the run must print 'bash <dir>/admin.sh'" "$(printf '%s\n' "$out" | tail -8)"
fi
if printf '%s' "$out" | grep -qE 'Prerequisites ready for'; then
    fail "cluster: a render must not print the 'Prerequisites ready' summary of a live run"
else
    ok "cluster: no 'Prerequisites ready' summary for a run that applied nothing"
fi

# --- namespaced, LLMSM_WATCH_NS=team-a,team-b, option A quota --------------------

D2="$WORK/r2"
run_render "$D2" STUB_NS="default kube-system team-a team-b" LLMSM_SCOPE=namespace \
    LLMSM_WATCH_NS=team-a,team-b KEDA_HELM_INSTALL=true \
    LLMSM_LIMITER=quota LLMSM_QUOTA_SCOPE=cluster 'LLMSM_QUOTAS=NVIDIA-A100-PCIE-80GB=4'
if [ "$rc" -eq 0 ] && [ "$muts" = 0 ]; then
    ok "namespaced: the render exits 0 with zero mutating calls"
else
    fail "namespaced: the render must exit 0 with no mutating call (rc=$rc, mutations=$muts)" \
        "$(grep '^MUTATION' "$STUB_LOG" | head -5; printf '%s\n' "$out" | tail -15)"
fi
kinds2="$(yaml_kinds "$D2/admin.yaml" 2>/dev/null)"
if printf '%s\n' "$kinds2" | grep -q '^Role team-a/scaling-manager-manager-role-' && printf '%s\n' "$kinds2" | grep -q '^RoleBinding team-a/scaling-manager-manager-rolebinding-' \
    && printf '%s\n' "$kinds2" | grep -q '^Role team-b/scaling-manager-manager-role-' && printf '%s\n' "$kinds2" | grep -q '^RoleBinding team-b/scaling-manager-manager-rolebinding-'; then
    ok "namespaced: admin.yaml grants the manager Role and RoleBinding in team-a and team-b"
else
    fail "namespaced: admin.yaml must carry the manager Role and RoleBinding in each managed namespace" "$(printf '%s\n' "$kinds2" | grep -E '^Role' | head)"
fi
if printf '%s\n' "$kinds2" | grep -q '^Namespace /scaling-manager-system$' \
    && ! printf '%s\n' "$kinds2" | grep -qE '^Namespace /team-(a|b)$'; then
    ok "namespaced: admin.yaml creates the controller namespace, not the managed ones that exist"
else
    fail "namespaced: admin.yaml must create the controller namespace and leave existing ones" "$(printf '%s\n' "$kinds2" | grep '^Namespace')"
fi

# --- KEDA already on the cluster -------------------------------------------------

D3="$WORK/r3"
run_render "$D3" STUB_NS="default kube-system team-a team-b keda-system" STUB_KEDA=present LLMSM_SCOPE=cluster KEDA_HELM_INSTALL=true
if [ "$rc" -eq 0 ] && [ "$muts" = 0 ] && [ -s "$D3/admin.sh" ] && ! grep -q 'kedacore/keda' "$D3/admin.sh"; then
    ok "KEDA present: admin.sh has no KEDA Helm install"
else
    fail "KEDA present: the render must not install KEDA (rc=$rc, mutations=$muts)" "$(grep -n 'keda' "$D3/admin.sh" 2>&1 | head -5)"
fi

# --- an immutable roleRef on the cluster ----------------------------------------

# The manager binding's name as the first render wrote it (suffixed per
# install), so the fixture names a binding this install owns.
CRB="$(yq 'select(.kind == "ClusterRoleBinding" and (.metadata.name | test("^scaling-manager-manager-rolebinding-"))) | .metadata.name' "$D1/admin.yaml" 2>/dev/null | grep -v '^---$' | head -1)"
CRB="${CRB:-scaling-manager-manager-rolebinding-unknown}"
D4="$WORK/r4"
run_render "$D4" STUB_NS="default kube-system team-a team-b" LLMSM_SCOPE=cluster KEDA_HELM_INSTALL=true \
    STUB_CRB="$CRB=some-old-role"
if [ "$rc" -eq 0 ] && [ "$muts" = 0 ] \
    && grep -qxF "kubectl delete clusterrolebinding $CRB --ignore-not-found" "$D4/admin.sh" 2>/dev/null \
    && grep -B1 -xF "kubectl delete clusterrolebinding $CRB --ignore-not-found" "$D4/admin.sh" | head -1 | grep -qi 'roleRef is immutable'; then
    ok "roleRef: the delete an immutable roleRef needs is in admin.sh, with its reason, and was not made"
else
    fail "roleRef: admin.sh must delete $CRB, whose roleRef changes, say why, and the render must not (rc=$rc, mutations=$muts)" \
        "$(grep -n -B1 'delete' "$D4/admin.sh" 2>&1 | head -8)"
fi
if N="$CRB" yq 'select(.kind == "ClusterRoleBinding" and .metadata.name == strenv(N)) | .metadata.name' "$D4/admin.yaml" 2>/dev/null | grep -qxF "$CRB"; then
    ok "roleRef: admin.yaml recreates the binding"
else
    fail "roleRef: admin.yaml must carry $CRB, which the delete removes"
fi

# --- the helper itself -----------------------------------------------------------
#
# Sourced, with the stub on PATH, to ask the questions a full run cannot isolate.
helper() {
    (cd "$TREE" && env -i HOME="$WORK" PATH="$WORK/bin:$TOOLPATH:/usr/local/bin:/usr/bin:/bin" \
        KUBECONFIG="$WORK/none" STUB_LOG="$WORK/helper.log" STUB_WORK="$WORK" TMPDIR="$WORK" STUB_DENY=true \
        INSTALL_PHASE_EXPLICIT=prereqs INSTALL_PHASE=prereqs bash -c '
            BLUE="" GREEN="" YELLOW="" RED="" NC=""
            . deploy/lib/common.sh
            declare -F llmsm_render_init >/dev/null || { echo NO_RENDER_MODE; exit 3; }
            llmsm_render_init "'"$WORK/h"'" >/dev/null 2>&1
            '"$1" 2>&1)
}
: > "$WORK/helper.log"
h="$(helper 'kubectl auth can-i create clusterroles; echo "rc=$?"; kubectl auth can-i list deployments -A; echo "rc=$?"')"
if [ "$(printf '%s\n' "$h" | head -4 | tr '\n' ' ')" = "yes rc=0 no rc=1 " ]; then
    ok "can-i: a write question is answered for the admin (yes), a read question by the cluster (no)"
else
    fail "can-i: create must answer yes and list must reach the cluster's no" "$h"
fi
h="$(helper 'printf "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c1, namespace: team-a}\n" | kubectl apply -f - >/dev/null; kubectl get configmap c1 -n team-a -o jsonpath={.metadata.name}; echo; kubectl label configmap c1 -n team-a a=b >/dev/null; kubectl get configmap c1 -n team-a -o jsonpath={.metadata.labels.a}')"
if [ "$(printf '%s\n' "$h" | tr '\n' ' ')" = "c1 b " ]; then
    ok "store: a rendered object reads back, and a label on it lands on the stored copy"
else
    fail "store: an applied object must read back, and a label edit it" "$h"
fi
h="$(helper 'bash -c "kubectl delete namespace team-a"; echo "rc=$?"')"
if printf '%s' "$h" | grep -q 'refusing' && printf '%s' "$h" | grep -q 'rc=1' && ! grep -q '^MUTATION kubectl delete namespace team-a' "$WORK/helper.log"; then
    ok "child process: a kubectl write from a child process is refused and never reaches the cluster"
else
    fail "child process: a direct kubectl write must be refused" "$h"
fi

# The render's reads of the live cluster, behind a notice kubectl prints on
# stderr while the read itself succeeds (or fails saying nothing else). Read
# with 2>&1, a notice was an object to delete. NOISY_MODE: empty (the list
# matched nothing) or fail (exit 1, only the notice on stderr).
cat > "$WORK/noisy-kubectl" <<'NOISYEOF'
#!/usr/bin/env bash
echo 'E1113 09:55:01.123456 4242 memcache.go:265] "Unhandled Error" err="could not get resource list for external.metrics.k8s.io/v1beta1: the server is currently unable to handle the request"' >&2
case "${NOISY_MODE:-empty}" in
    fail) exit 1 ;;
esac
exit 0
NOISYEOF
chmod +x "$WORK/noisy-kubectl"
h="$(helper 'LLMSM_RENDER_KUBECTL="'"$WORK/noisy-kubectl"'"; export NOISY_MODE=empty; llmsm_render_delete clusterrolebindings -l app.kubernetes.io/name=nothing-here')"
if printf '%s' "$h" | grep -q 'not found, nothing to delete (rendered)'; then
    ok "render delete: a selector that matched nothing is nothing to delete, whatever kubectl printed on stderr"
else
    fail "render delete: behind a notice, an empty match must record no delete" "$h"
fi
h="$(helper 'LLMSM_RENDER_KUBECTL="'"$WORK/noisy-kubectl"'"; export NOISY_MODE=fail; llmsm_render_delete clusterrolebinding some-binding; echo "ns=$(llmsm_render_ns_state team-z)"')"
if ! printf '%s' "$h" | grep -q 'nothing to delete' && printf '%s' "$h" | grep -q '^ns=unknown$'; then
    ok "render delete: a read that failed saying only notices is unknown, so the delete is recorded and the namespace unknown"
else
    fail "render delete: a failure behind notices alone must record the delete, and leave the namespace unknown" "$h"
fi

# --- refusals ----------------------------------------------------------------------

run_render "$WORK/r5" STUB_NS="default team-a" LLMSM_SCOPE=cluster INSTALL_PHASE=scaling-manager
if [ "$rc" -ne 0 ] && [ "$muts" = 0 ] && printf '%s' "$out" | grep -q 'RENDER renders the cluster-admin phase only'; then
    ok "refusal: RENDER with INSTALL_PHASE=scaling-manager refuses, and writes nothing"
else
    fail "refusal: RENDER must refuse the controller phase (rc=$rc, mutations=$muts)" "$(printf '%s\n' "$out" | tail -5)"
fi
run_render "$WORK/r6" STUB_NS="default team-a" LLMSM_SCOPE=cluster CHECK_ONLY=true
if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q 'RENDER renders the cluster-admin phase only'; then
    ok "refusal: RENDER with --check refuses"
else
    fail "refusal: RENDER must refuse --check (rc=$rc)" "$(printf '%s\n' "$out" | tail -5)"
fi
# A second render into the same directory replaces, never appends.
cp "$D1/admin.sh" "$WORK/first.sh"
run_render "$D1" STUB_NS="default kube-system team-a team-b" LLMSM_SCOPE=cluster KEDA_HELM_INSTALL=true
if [ "$rc" -eq 0 ] && [ "$(grep -c '^helm upgrade -i keda' "$D1/admin.sh")" = 1 ] \
    && [ "$(grep -vE '^# Rendered|^# For cluster' "$D1/admin.sh" | md5sum)" = "$(grep -vE '^# Rendered|^# For cluster' "$WORK/first.sh" | md5sum)" ]; then
    ok "re-render: a second render into the same directory gives the same admin.sh"
else
    fail "re-render: a second render must replace admin.sh, not add to it" "$(diff "$WORK/first.sh" "$D1/admin.sh" | head -10)"
fi

TOTAL=$((PASS + FAIL))
echo ""
echo "setup-prereqs render: $PASS passed, $FAIL failed, $TOTAL cases ran (expected $EXPECT_CASES)"
if [ "$TOTAL" -ne "$EXPECT_CASES" ]; then
    echo "FAIL: $TOTAL cases ran, expected $EXPECT_CASES -- a case was skipped or added without updating EXPECT_CASES" >&2
    exit 1
fi
[ "$FAIL" -eq 0 ]
