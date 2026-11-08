#!/usr/bin/env bash
#
# Asserts that every namespace-scoped controller is granted what it needs to
# FIND and READ a published cluster policy, by executing the grant functions
# against a stubbed kubectl.
#
# Two bugs, both silent:
#
#   A. A namespace-scoped controller holds only a Role, which cannot reach a
#      cluster-scoped Namespace. Its lookup of the well-known `scaling-manager-policy`
#      namespace was Forbidden, the controller read that as "absent", and ran on
#      its own ConfigMap -- while `make enable-physical-limiter` reported the
#      cluster bounded. Fixed by a Namespace `get` grant: the
#      policy-namespace-reader component (prereqs phase) and
#      pl_grant_namespace_read (enable-physical-limiter), plus a restart for a
#      controller that could not see the namespace when it started.
#   B. A controller installed AFTER the policy was published resolved to it but
#      had no RoleBinding there, and refused to start. Fixed by the prereqs phase
#      granting the read whenever the policy namespace already exists.
#
# What is asserted is what the functions would APPLY, not what their source
# says.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FAIL=0
CASES=0

# Sourced through CR-stripped copies, as hack/check-limiter-declaration.sh does:
# a Windows checkout can write these as CRLF, and bash then stops at the first
# function definition.
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
for lib in common.sh limiter_policy.sh physical_limiter.sh; do
    tr -d '\r' < "$ROOT/deploy/lib/$lib" > "$WORK/$lib"
done
BLUE=''; GREEN=''; YELLOW=''; RED=''; NC=''
unset LLMSM_QUOTAS LLMSM_QUOTA_SCOPE LLMSM_QUOTA_KUEUE LLMSM_SCOPE LLMSM_WATCH_NS LLMSM_LIMITER LLMSM_LIMITER_TYPE LLMSM_LIMITER_TARGETS
LLMSM_NS="scaling-manager-system"
LLMSM_POLICY_NS="scaling-manager-policy"
# shellcheck disable=SC1090
. "$WORK/common.sh"
# shellcheck disable=SC1090
. "$WORK/limiter_policy.sh"
# shellcheck disable=SC1090
. "$WORK/physical_limiter.sh"

for tool in yq jq; do
    command -v "$tool" >/dev/null 2>&1 || {
        echo "FATAL: $tool is required by this check, and is missing. Nothing below would be about the code."
        exit 2
    }
done

case_begin() { CASES=$((CASES + 1)); CASE_FAIL_AT="$FAIL"; }
fail() { echo "FAIL $*"; FAIL=$((FAIL + 1)); }
ok()   { [ "$FAIL" -eq "${CASE_FAIL_AT:-0}" ] || return 0; echo "ok   $*"; }

# ---------------------------------------------------------------------------
# The stub. Every invocation is recorded as CALL[...]; every `apply -f -` also
# records the manifest it was given between APPLY<< and >>APPLY.
# ---------------------------------------------------------------------------
STUB="$WORK/stub"; mkdir -p "$STUB"
cat > "$STUB/kubectl" <<'STUBEOF'
#!/usr/bin/env bash
printf 'CALL[%s]\n' "$*" >> "$KLOG"
case "$*" in
  "auth can-i create clusterrolebindings"*) echo yes; exit 0 ;;
  "auth can-i get namespaces/"*)
      if [ "${CANI_NS:-no}" = yes ]; then echo yes; exit 0; fi
      echo no; exit 1 ;;
  # Read in full BEFORE the block is opened: the other stages of a
  # `create | apply` pipeline run concurrently and log their CALL lines, and
  # one landing inside an open block made the whole stream unparseable.
  "apply -f -"*)
      doc="$(cat)"
      printf 'APPLY<<\n%s\n>>APPLY\n' "$doc" >> "$KLOG"; exit 0 ;;
  "get namespace "*)
      name="$3"
      case "$*" in
        *'policy-namespace'*)
            if [ -n "${MANAGED_GET_ERR:-}" ]; then echo "$MANAGED_GET_ERR" >&2; exit 1; fi
            printf '%s' "${NS_LABEL:-}"; exit 0 ;;
        *'creationTimestamp'*) printf '2026-01-01T00:00:00Z'; exit 0 ;;
      esac
      case " ${EXISTING_NS:-} " in
        *" $name "*) echo "namespace/$name"; exit 0 ;;
      esac
      if [ -n "${NS_GET_ERR:-}" ]; then echo "$NS_GET_ERR" >&2; exit 1; fi
      echo "Error from server (NotFound): namespaces \"$name\" not found" >&2; exit 1 ;;
  "get deploy -A"*) printf '%s\n' "${DEPLOY_NS_LIST:-}"; exit 0 ;;
  # The verification read of enable-physical-limiter: one genuine,
  # SELF-MANAGED controller -- the base manifest shape, --watch-namespace
  # expanded from LLMSM_WATCH_NAMESPACE, which is $(POD_NAMESPACE) -- with a
  # sidecar listed FIRST, so a reader of containers[0] sees no flag at all.
  "get deploy -n "*)
      case "$*" in
        *' -o json')   # exactly -o json
            printf '%s' '{"items":[{"metadata":{"name":"scaling-manager-controller-manager"},"spec":{"selector":{"matchLabels":{"control-plane":"controller-manager"}},"template":{"spec":{"serviceAccountName":"scaling-manager-controller-manager","containers":[{"name":"kube-rbac-proxy","image":"quay.io/brancz/kube-rbac-proxy:v0.18.0","args":["--upstream=http://127.0.0.1:8080/"]},{"name":"manager","image":"ghcr.io/llm-d-incubation/llm-d-scaling-manager:main","args":["--leader-elect=true","--watch-namespace=$(LLMSM_WATCH_NAMESPACE)"],"env":[{"name":"POD_NAMESPACE","valueFrom":{"fieldRef":{"fieldPath":"metadata.namespace"}}},{"name":"LLMSM_WATCH_NAMESPACE","value":"$(POD_NAMESPACE)"}]}]}}}}]}' ;;
      esac
      exit 0 ;;
  # By the Deployment's own selector, the controller's pods: the newest Running
  # one started after scaling-manager-policy was created; an older Running one and a
  # Pending one did not. By the controller LABEL, an impostor's pod comes first
  # and started long before -- what `.items[0]` of the label used to read.
  "get pods -n "*)
      case "$*" in
        *"-l control-plane=controller-manager"*)
            if [ -n "${PODS_JSON:-}" ]; then
                printf '%s' "$PODS_JSON"
            else
                printf '%s' '{"items":[{"status":{"phase":"Running","startTime":"2025-12-15T00:00:00Z"}},{"status":{"phase":"Running","startTime":"2026-02-01T00:00:00Z"}},{"status":{"phase":"Pending"}}]}'
            fi ;;
        *"-l app.kubernetes.io/name=scaling-manager"*)
            case "$*" in
              *jsonpath*) printf '2025-06-01T00:00:00Z' ;;
              *) printf '%s' '{"items":[{"status":{"phase":"Running","startTime":"2025-06-01T00:00:00Z"}}]}' ;;
            esac ;;
      esac
      exit 0 ;;
  "get configmap"*)
      echo 'Error from server (NotFound): configmaps "scaling-manager-scaling-policy-config" not found' >&2; exit 1 ;;
  "create "*"--dry-run=client"*)
      printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: stub\n'; exit 0 ;;
  "label --local"*) cat; exit 0 ;;
esac
exit 0
STUBEOF
chmod +x "$STUB/kubectl"

# run <function and args...> -- in a SUBSHELL (log_error exits), stub first on
# PATH. Leaves the call log in $LOG, stderr+stdout in $OUT, status in $RC.
run() {
    LOG="$WORK/log"; : > "$LOG"
    (
        PATH="$STUB:$PATH"; export PATH
        KLOG="$LOG"; export KLOG
        export CANI_NS NS_LABEL EXISTING_NS NS_GET_ERR MANAGED_GET_ERR DEPLOY_NS_LIST PODS_JSON
        "$@"
    ) >"$WORK/out" 2>&1
    RC=$?
    OUT="$(cat "$WORK/out")"
}

# applied_doc <kind> <name> -- the applied manifest of that kind and name, as
# JSON, or nothing.
applied_doc() {
    awk '/^APPLY<<$/{on=1; print "---"; next} /^>>APPLY$/{on=0; next} on' "$LOG" \
        | yq -o=json -I=0 "select(.kind == \"$1\" and .metadata.name == \"$2\")" 2>/dev/null
}

SUFFIX_A="$(llmsm_ns_suffix tenant-a)"

# ---------------------------------------------------------------------------
# The component every namespace-scoped install applies in the prereqs phase.
# ---------------------------------------------------------------------------
case_begin
role_file="$ROOT/config/components/policy-namespace-reader/policy-namespace-reader-clusterrole.yaml"
if [ ! -f "$role_file" ]; then
    fail "config/components/policy-namespace-reader is missing: no namespace-scoped controller can read the Namespace that holds cluster policy"
else
    rules="$(tr -d '\r' < "$role_file" | yq -o=json -I=0 '.rules')"
    [ "$(printf '%s' "$rules" | jq -r '[.[] | select(.resources | index("namespaces")) | .verbs[]] | index("get") != null')" = "true" ] \
        || fail "the policy-namespace-reader ClusterRole does not grant get on namespaces: $rules"
    [ "$(printf '%s' "$rules" | jq -r '[.[].resources[]] | unique | join(",")')" = "namespaces" ] \
        || fail "the policy-namespace-reader ClusterRole grants more than namespaces: $rules"
    ok "the policy-namespace-reader ClusterRole grants get on namespaces, and nothing else"
fi

for platform in kubernetes openshift; do
    case_begin
    if tr -d '\r' < "$ROOT/config/overlays/namespace-scoped/$platform/kustomization.yaml" \
        | yq -e '.components[] | select(. == "../../../components/policy-namespace-reader/")' >/dev/null 2>&1; then
        ok "the namespace-scoped $platform overlay includes the policy-namespace-reader component"
    else
        fail "the namespace-scoped $platform overlay does not include components/policy-namespace-reader"
    fi
done

# Renamed per install, or two installs share one binding and the second apply
# replaces the first one's subject.
case_begin
printf '%s\n' "${LLMSM_SHARED_CLUSTER_ROLE_BINDINGS[@]}" | grep -qx 'scaling-manager-policy-namespace-reader-rolebinding' \
    || fail "scaling-manager-policy-namespace-reader-rolebinding is not renamed per install (LLMSM_SHARED_CLUSTER_ROLE_BINDINGS)"
printf '%s\n' "${LLMSM_OWNED_CLUSTER_ROLES[@]}" | grep -qx 'scaling-manager-policy-namespace-reader-role:scaling-manager-policy-namespace-reader-rolebinding' \
    || fail "scaling-manager-policy-namespace-reader-role is not renamed per install (LLMSM_OWNED_CLUSTER_ROLES)"
ok "the policy-namespace-reader role and binding are renamed per install"

# ---------------------------------------------------------------------------
# Bug B: the prereqs phase grants the read of a policy published earlier.
# ---------------------------------------------------------------------------
if ! declare -F pl_grant_published_policy_read >/dev/null; then
    case_begin
    fail "pl_grant_published_policy_read is not defined: a controller installed after the policy was published gets no read of it, and refuses to start"
else
    case_begin
    CANI_NS=no NS_LABEL='' EXISTING_NS='scaling-manager-policy' NS_GET_ERR='' MANAGED_GET_ERR='' \
        run pl_grant_published_policy_read tenant-a tenant-a
    rb="$(applied_doc RoleBinding "scaling-manager-policy-reader-$SUFFIX_A")"
    if [ "$RC" -ne 0 ]; then
        fail "pl_grant_published_policy_read failed: $OUT"
    elif [ -z "$rb" ]; then
        fail "a self-managed controller installed after scaling-manager-policy exists got no RoleBinding there: $(cat "$LOG")"
    else
        [ "$(printf '%s' "$rb" | jq -r '.metadata.namespace')" = "scaling-manager-policy" ] \
            || fail "the policy-read RoleBinding is not in scaling-manager-policy: $rb"
        [ "$(printf '%s' "$rb" | jq -r '.subjects[0].namespace + "/" + .subjects[0].name')" = "tenant-a/scaling-manager-controller-manager" ] \
            || fail "the policy-read RoleBinding binds the wrong subject: $rb"
        [ -n "$(applied_doc Role scaling-manager-policy-reader)" ] \
            || fail "the scaling-manager-policy-reader Role the binding references was not applied"
        ok "a controller installed after scaling-manager-policy exists is granted its read"
    fi

    case_begin
    CANI_NS=no NS_LABEL='' EXISTING_NS='' NS_GET_ERR='' MANAGED_GET_ERR='' \
        run pl_grant_published_policy_read tenant-a tenant-a
    if [ "$RC" -ne 0 ]; then
        fail "pl_grant_published_policy_read failed when no policy namespace exists: $OUT"
    elif grep -q '^APPLY<<' "$LOG"; then
        fail "a RoleBinding was applied into a policy namespace that does not exist: $(cat "$LOG")"
    else
        ok "no policy namespace yet: nothing is granted, and the install goes on"
    fi

    case_begin
    CANI_NS=no NS_LABEL='platform-policy' EXISTING_NS='platform-policy scaling-manager-policy' NS_GET_ERR='' MANAGED_GET_ERR='' \
        run pl_grant_published_policy_read tenant-a tenant-a
    rb="$(applied_doc RoleBinding "scaling-manager-policy-reader-$SUFFIX_A")"
    if [ "$(printf '%s' "$rb" | jq -r '.metadata.namespace // ""')" = "platform-policy" ]; then
        ok "the scaling-manager.llmd.ai/policy-namespace label wins over scaling-manager-policy, as it does in the controller"
    else
        fail "the label on the managed namespace was not followed: $(cat "$LOG")"
    fi

    # An admin-owned controller keeps its own ConfigMap: nothing to grant, and
    # nothing to ask the cluster.
    case_begin
    CANI_NS=no NS_LABEL='' EXISTING_NS='scaling-manager-policy' NS_GET_ERR='' MANAGED_GET_ERR='' \
        run pl_grant_published_policy_read scaling-manager-system team-b
    if grep -q '^CALL' "$LOG"; then
        fail "an admin-owned controller (manages team-b, runs in scaling-manager-system) was granted policy reads: $(cat "$LOG")"
    else
        ok "an admin-owned controller is left alone"
    fi

    # Forbidden is not absent: no grant, but a warning that says so.
    case_begin
    CANI_NS=no NS_LABEL='' EXISTING_NS='' MANAGED_GET_ERR='' \
        NS_GET_ERR='Error from server (Forbidden): namespaces "scaling-manager-policy" is forbidden' \
        run pl_grant_published_policy_read tenant-a tenant-a
    if [ "$RC" -ne 0 ]; then
        fail "a Forbidden existence check aborted the install: $OUT"
    elif grep -q '^APPLY<<' "$LOG"; then
        fail "a grant was applied although the namespace's existence was never established"
    elif ! printf '%s' "$OUT" | grep -q 'Could not tell whether'; then
        fail "a Forbidden existence check was reported as an absent policy namespace: $OUT"
    else
        ok "a Forbidden existence check is reported, not read as absent"
    fi
fi

# ---------------------------------------------------------------------------
# Bug A: enable-physical-limiter grants every self-managed controller the
# Namespace get, and restarts the ones that could not see the policy namespace.
# ---------------------------------------------------------------------------
case_begin
CANI_NS=no NS_LABEL='' EXISTING_NS='scaling-manager-policy' NS_GET_ERR='' MANAGED_GET_ERR='' DEPLOY_NS_LIST='tenant-a' \
    run enable_physical_limiter
crole="$(applied_doc ClusterRole "scaling-manager-policy-namespace-reader-role-$SUFFIX_A")"
crb="$(applied_doc ClusterRoleBinding "scaling-manager-policy-namespace-reader-rolebinding-$SUFFIX_A")"
if [ "$RC" -ne 0 ]; then
    fail "enable_physical_limiter failed under the stub: $OUT"
elif [ -z "$crole" ] || [ -z "$crb" ]; then
    fail "enable_physical_limiter did not grant tenant-a get on namespaces, so on Kubernetes it cannot see scaling-manager-policy and the published limiter never applies to it"
else
    [ "$(printf '%s' "$crole" | jq -r '.rules[0].resources[0] + ":" + (.rules[0].verbs | join(","))')" = "namespaces:get" ] \
        || fail "the namespace-read ClusterRole does not grant exactly get on namespaces: $crole"
    [ "$(printf '%s' "$crb" | jq -r '.subjects[0].namespace + "/" + .subjects[0].name')" = "tenant-a/scaling-manager-controller-manager" ] \
        || fail "the namespace-read binding names the wrong subject: $crb"
    [ "$(printf '%s' "$crb" | jq -r '.roleRef.name')" = "scaling-manager-policy-namespace-reader-role-$SUFFIX_A" ] \
        || fail "the namespace-read binding points at a ClusterRole that was not created: $crb"
    ok "enable_physical_limiter grants a self-managed controller get on namespaces"
fi

case_begin
# By name: enable-physical-limiter restarts the Deployment it verified, never
# whatever else carries the controller label.
if grep -q '^CALL\[rollout restart deploy scaling-manager-controller-manager -n tenant-a' "$LOG"; then
    ok "a controller that could not see scaling-manager-policy is restarted, so it re-resolves its policy source"
else
    fail "a controller that could not read namespace scaling-manager-policy was granted it but not restarted; it keeps its own ConfigMap until something restarts it: $(grep '^CALL' "$LOG")"
fi

# The Namespace read is limited to the names the controller GETs by name
# (ResolvePolicyNamespace: scaling-manager-policy, and its managed Namespace for the label).
case_begin
names="$(printf '%s' "$crole" | jq -c '.rules[0].resourceNames // [] | sort')"
if [ "$names" = '["scaling-manager-policy","tenant-a"]' ]; then
    ok "the Namespace read names only the managed namespace and scaling-manager-policy"
else
    fail "the Namespace read is not limited to the namespaces the controller reads (resourceNames=$names): $crole"
fi

# The restart is announced in the list printed BEFORE anything changes, with
# the upgrade case named: every self-managed controller restarts on the first
# enable after the Namespace read was added.
case_begin
listed="$(sed -n '/Controllers to be granted/,/Publishing cluster policy/p' <<<"$OUT")"
if ! grep -q 'tenant-a/scaling-manager-controller-manager .*WILL BE RESTARTED' <<<"$listed"; then
    fail "the pre-grant list does not say tenant-a will be restarted: $OUT"
elif ! grep -q 'first enable after upgrading' <<<"$listed"; then
    fail "the pre-grant list does not warn that the first enable after an upgrade restarts every self-managed controller: $OUT"
else
    ok "the restart is announced before anything is changed"
fi

# A self-managed controller that could already see scaling-manager-policy, and started
# after it existed, is STILL restarted: it does not watch scaling-manager-policy
# (cmd/main.go caches ConfigMaps in its managed namespace only), so the policy
# this command writes reaches it only at its next start. Not restarting it was
# how "the limiter is now in force" was printed for a controller still running
# on the policy it read at startup. The reason given is that one, not a
# pretended inability to read.
case_begin
CANI_NS=yes NS_LABEL='' EXISTING_NS='scaling-manager-policy' NS_GET_ERR='' MANAGED_GET_ERR='' DEPLOY_NS_LIST='tenant-a' \
    run enable_physical_limiter
if [ "$RC" -ne 0 ]; then
    fail "enable_physical_limiter failed under the stub: $OUT"
elif ! grep -q '^CALL\[rollout restart' "$LOG"; then
    fail "a self-managed controller that does not watch scaling-manager-policy was not restarted, so it never reads the policy just written: $OUT"
elif ! grep -q 'tenant-a/scaling-manager-controller-manager .*WILL BE RESTARTED: it does not watch scaling-manager-policy' <<<"$OUT"; then
    fail "the restart of a controller that could already read scaling-manager-policy does not say why (it does not watch it): $OUT"
else
    ok "a controller that could already see scaling-manager-policy is restarted, because it does not watch it"
fi

# ...and one whose newest Running pod predates scaling-manager-policy still is.
case_begin
CANI_NS=yes NS_LABEL='' EXISTING_NS='scaling-manager-policy' NS_GET_ERR='' MANAGED_GET_ERR='' DEPLOY_NS_LIST='tenant-a' \
    PODS_JSON='{"items":[{"status":{"phase":"Running","startTime":"2025-12-15T00:00:00Z"}},{"status":{"phase":"Succeeded","startTime":"2026-03-01T00:00:00Z"}}]}' \
    run enable_physical_limiter
if [ "$RC" -ne 0 ]; then
    fail "enable_physical_limiter failed under the stub: $OUT"
elif grep -q '^CALL\[rollout restart deploy scaling-manager-controller-manager -n tenant-a' "$LOG"; then
    ok "a controller whose newest Running pod started before scaling-manager-policy existed is restarted"
else
    fail "a controller that started before scaling-manager-policy existed was not restarted: $(grep '^CALL' "$LOG")"
fi

# ---------------------------------------------------------------------------
# The scaling-manager.llmd.ai/policy-namespace label on the managed namespace wins, as it
# does in the controller: the read, the Namespace grant and the policy go there.
# ---------------------------------------------------------------------------
case_begin
CANI_NS=yes NS_LABEL='platform-policy' EXISTING_NS='platform-policy scaling-manager-policy' NS_GET_ERR='' MANAGED_GET_ERR='' DEPLOY_NS_LIST='tenant-a' \
    run enable_physical_limiter
rb="$(applied_doc RoleBinding "scaling-manager-policy-reader-$SUFFIX_A")"
crole="$(applied_doc ClusterRole "scaling-manager-policy-namespace-reader-role-$SUFFIX_A")"
if [ "$RC" -ne 0 ]; then
    fail "enable_physical_limiter failed for a labelled namespace: $OUT"
elif [ "$(printf '%s' "$rb" | jq -r '.metadata.namespace // ""')" != "platform-policy" ]; then
    fail "the policy read was granted outside the namespace the label names: $(printf '%s' "$rb" | jq -c '.metadata')"
elif [ "$(printf '%s' "$crole" | jq -c '.rules[0].resourceNames | sort')" != '["platform-policy","scaling-manager-policy","tenant-a"]' ]; then
    fail "the Namespace read does not cover the labelled policy namespace: $crole"
elif ! grep -q '^CALL\[create configmap scaling-manager-scaling-policy-config -n platform-policy' "$LOG"; then
    fail "the limiter was not published where the labelled controller reads it: $(grep '^CALL' "$LOG")"
else
    ok "a labelled namespace's controller is granted, and bounded, in the namespace the label names"
fi

# A label that cannot be read is not an absent one: refuse before any change.
case_begin
CANI_NS=yes NS_LABEL='' EXISTING_NS='scaling-manager-policy' NS_GET_ERR='' DEPLOY_NS_LIST='tenant-a' \
    MANAGED_GET_ERR='Error from server (Forbidden): namespaces "tenant-a" is forbidden' \
    run enable_physical_limiter
if [ "$RC" -eq 0 ]; then
    fail "enable_physical_limiter went on without reading the managed namespace label: $OUT"
elif grep -q '^APPLY<<' "$LOG"; then
    fail "enable_physical_limiter changed the cluster before refusing over an unreadable label: $(grep '^CALL' "$LOG")"
elif ! grep -q 'could not read Namespace tenant-a' <<<"$OUT"; then
    fail "the refusal does not name the namespace it could not read: $OUT"
else
    ok "an unreadable policy-namespace label stops enable before it changes anything"
fi

# ---------------------------------------------------------------------------
# The prereqs phase calls the grant. A function nothing calls fixes nothing.
# ---------------------------------------------------------------------------
case_begin
tr -d '\r' < "$ROOT/deploy/install.sh" | grep -qE '^source "\$DEPLOY_LIB_DIR/physical_limiter.sh"' \
    || fail "deploy/install.sh does not source physical_limiter.sh, so the prereqs phase cannot grant the policy read"
tr -d '\r' < "$ROOT/deploy/lib/install_core.sh" | grep -qE '^[[:space:]]+pl_grant_published_policy_read ' \
    || fail "install_core.sh never calls pl_grant_published_policy_read"
ok "the prereqs phase calls the policy-read grant"

# BEFORE the controller is applied. With INSTALL_PHASE=all deploy_scaling_manager_controller
# also applies the Deployment, and a grant made after it raced the first pod,
# which could find the policy, no RoleBinding, and refuse to start.
case_begin
grant_at="$(tr -d '\r' < "$ROOT/deploy/lib/install_core.sh" | grep -nE '^[[:space:]]+pl_grant_published_policy_read ' | head -1 | cut -d: -f1)"
deploy_at="$(tr -d '\r' < "$ROOT/deploy/lib/install_core.sh" | grep -nE '^[[:space:]]+deploy_scaling_manager_controller[[:space:]]*$' | head -1 | cut -d: -f1)"
if [ -z "$grant_at" ] || [ -z "$deploy_at" ]; then
    fail "could not find the prereqs-phase grant ($grant_at) or controller apply ($deploy_at) in install_core.sh; this case can no longer tell their order"
elif [ "$grant_at" -lt "$deploy_at" ]; then
    ok "the policy read is granted before the controller Deployment is applied"
else
    fail "install_core.sh grants the policy read (line $grant_at) after deploy_scaling_manager_controller (line $deploy_at): with INSTALL_PHASE=all the first pod can start before its RoleBinding exists"
fi

# MT3 (reverses M-m4): enable-physical-limiter decides the cluster-wide
# Workload read from the policy the controller reads its limiters from, ALONE.
# It used to OR in the controller's own ConfigMap, which a self-managed
# controller's tenant writes: kueue.enabled there earned the read of every
# tenant's Workloads from this cluster-admin command, and a ConfigMap that
# could not be read kept it for good (unknown is never revoked). The admin
# phase decides the same way (llmsm_kueue_borrow_wanted).
case_begin
borrow_wanted() {   # borrow_wanted <policy in scaling-manager-policy> <own policy> [own ConfigMap error]
    (
        BW_CLUSTER="$1" BW_OWN="$2" BW_OWN_ERR="${3:-}"
        kubectl() {
            case "$*" in
                "get configmap scaling-manager-scaling-policy-config -n scaling-manager-policy "*) printf '%s\n' "$BW_CLUSTER" ;;
                "get configmap scaling-manager-scaling-policy-config -n tenant-a "*)
                    if [ -n "$BW_OWN_ERR" ]; then echo "$BW_OWN_ERR" >&2; return 1; fi
                    if [ -z "$BW_OWN" ]; then echo 'Error from server (NotFound): configmaps "scaling-manager-scaling-policy-config" not found' >&2; return 1; fi
                    printf '%s\n' "$BW_OWN" ;;
                *) return 0 ;;
            esac
        }
        LLMSM_QUOTAS='H200=8' LLMSM_QUOTA_KUEUE=true LLMSM_QUOTA_KUEUE_BORROW=false
        pl_published_kueue scaling-manager-policy quota tenant-a
        printf '%s' "$PL_BORROW_WANTED"
    ) 2>/dev/null
}
OWN_BORROWS='limiters:
- name: own-quota
  type: quota
  kueue:
    enabled: true'
got="$(borrow_wanted 'scaleUpThreshold: 0.85' "$OWN_BORROWS")"
[ "$got" = no ] || fail "a cluster quota without borrowing over a controller whose own (tenant-written) policy counts it gave '$got', not no: the tenant decides the cluster-wide Workload read"
got="$(borrow_wanted 'scaleUpThreshold: 0.85' '')"
[ "$got" = no ] || fail "control: no own policy, a cluster quota without borrowing gave '$got', not no"
got="$(borrow_wanted 'scaleUpThreshold: 0.85' 'limiters: [unparseable' 'Error from server (Forbidden): configmaps is forbidden')"
[ "$got" = no ] || fail "an own policy that cannot be read gave '$got', not no: the grant is never revoked"
ok "enable-physical-limiter decides the borrowing read from the policy namespace alone, never the tenant's own ConfigMap"

CASES_EXPECTED=20
case_begin
if [ "$CASES" -ne "$((CASES_EXPECTED + 1))" ]; then
    fail "$((CASES - 1)) cases ran, not $CASES_EXPECTED. Update CASES_EXPECTED deliberately."
else
    ok "all $CASES_EXPECTED cases ran"
fi

if [ "$FAIL" -ne 0 ]; then
    echo "policy namespace grants check FAILED"
    exit 1
fi
echo "policy namespace grants OK"
