#!/usr/bin/env bash
# Utilization-share optimizer: check, shadow, activate, status, off.
#
# The optimizer is one `optimizer:` block in the scaling policy. Turning it on
# by hand meant reading the policy out, appending the block, patching it back
# and grepping the controller's log to learn whether it took. Each verb here
# does one of those steps, refuses what would be unsafe, and reports what the
# controller did with it.
#
#   utilization-share.sh check    -n NS   is there a budget to share; do the accelerators resolve
#   utilization-share.sh shadow   -n NS   compute and report, move nothing
#   utilization-share.sh activate -n NS   act on what shadow reported
#   utilization-share.sh status   -n NS   mode, recent decisions, transfers, warnings, marks
#   utilization-share.sh off      -n NS   back through shadow, wait for the marks to clear, remove the block
#
# Options:
#   -n, --namespace NS          the models' namespace (required, unless the
#                               controller manages a list of namespaces: then
#                               --controller-namespace alone, and every one of
#                               them is covered)
#   --controller-namespace NS   where the controller runs (default: NS)
#   --policy-namespace NS       where its scaling policy lives. Default: the one
#                               the controller logged at start (a policy
#                               namespace such as scaling-manager-policy), else its own.
#                               A namespace the controller does not read is refused.
#   --physical-groups           (shadow) also plan GPUs bounded only by the
#                               physical inventory; needed when the limiter is
#                               gpu-inventory and there is no quota. Refused for
#                               a namespace-scoped controller, which ignores it
#   --restart                   restart the controller when it logs no reload of
#                               the policy, then require the reload after start
#   --grant-shared              (activate) also grant the controller patch on
#                               pods in every other namespace that shares this
#                               budget, instead of naming them. For a
#                               cluster-scoped controller whose models span
#                               namespaces; needs create on Roles in each
#
# How a write is confirmed: a controller that publishes the ConfigMap
# scaling-manager-controller-status (data contract: internal/controllerstatus) records the
# policy resourceVersion it has applied, whether it accepted it, and the
# optimizer mode in force; the script waits for the version it wrote and reads
# the verdict there. An older controller without it is confirmed from its log
# (CYCLE_WAIT, default 45 s, is then how long a write waits for a refusal; keep
# it above the controller's cycle interval).
set -euo pipefail

# shellcheck source=lib/pod_writer.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/pod_writer.sh"
# For PL_DEPLOY_ROWS: which namespace the controller manages, read the way
# make enable-physical-limiter reads it.
# shellcheck source=lib/physical_limiter.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/physical_limiter.sh"
# For llmsm_controller_rollout_ready: a restart waits for a Ready controller, and
# `rollout status` alone passes at once for one scaled to 0.
# shellcheck source=lib/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/common.sh"

KUBECTL_TIMEOUT="${KUBECTL_TIMEOUT:-30s}"
CONTROLLER_DEPLOY="${CONTROLLER_DEPLOY:-scaling-manager-controller-manager}"
CONTROLLER_CONTAINER="${CONTROLLER_CONTAINER:-manager}"
POLICY_CM="scaling-manager-scaling-policy-config"
STATUS_CM="scaling-manager-controller-status"
MARK="llm-d.ai/utilization-share-transfer"
# The controller watches the ConfigMap, so a reload takes seconds.
RELOAD_WAIT="${RELOAD_WAIT:-90}"
CYCLE_WAIT="${CYCLE_WAIT:-45}"
# How long `off` waits in shadow for the transfer marks to clear.
MARK_WAIT="${MARK_WAIT:-180}"

info() { printf '  %s\n' "$*" >&2; }
ok()   { printf '[OK]   %s\n' "$*" >&2; }
warn() { printf '[WARN] %s\n' "$*" >&2; }
fail() { printf '[FAIL] %s\n' "$*" >&2; }
die()  { fail "$*"; exit 1; }

k() { kubectl --request-timeout="$KUBECTL_TIMEOUT" "$@"; }

# wait_controller_back waits for the restarted controller to roll out with a
# Ready replica, and dies saying why when it does not.
wait_controller_back() {
    llmsm_controller_rollout_ready "deploy/$CONTROLLER_DEPLOY" "$CONTROLLER_NS" 300s >/dev/null 2>&1 && return 0
    case "$LLMSM_CONTROLLER_NOT_READY" in
        scaled-to-zero) die "the controller Deployment $CONTROLLER_NS/$CONTROLLER_DEPLOY is at 0 replicas, so nothing reads the policy: scale it up (kubectl -n $CONTROLLER_NS scale deploy/$CONTROLLER_DEPLOY --replicas=1)" ;;
        *) die "the controller did not come back within 300 s" ;;
    esac
}

# ---------------------------------------------------------------------------
# Policy transforms: a policy on stdin, a word or a policy on stdout.
# hack/check-utilization-share-script.sh runs them against fixtures.
# ---------------------------------------------------------------------------

# us_doc_count -- how many YAML documents the policy holds. The controller
# reads only the first, so a write into several would change more than it reads.
us_doc_count() {
    yq -r 'document_index' | sort -u | grep -c . || true
}

# us_limiter_kind -- the limiter set the policy declares: none, quota,
# inventory, or inventory+quota when it declares both. Every declared limiter
# is built and a decision must fit all of them, so a list holding both is
# BOTH, not a quota (it was until the limiters composed). A list holding a
# type the controller does not know is none: its validation rejects the whole
# policy and it runs with no limiter (internal/config/saturation_scaling.go),
# so there is no budget to share.
us_limiter_kind() {
    local types t physical=0 quota=0
    types="$(yq -r '[.limiters[]?.type] | join(" ")')" || return 1
    for t in $types; do
        case "$t" in
            quota|gpu-inventory|inventory) ;;
            *) echo none; return 0 ;;
        esac
    done
    case " $types " in *" gpu-inventory "*|*" inventory "*) physical=1 ;; esac
    case " $types " in *" quota "*) quota=1 ;; esac
    case "$physical$quota" in
        11) echo inventory+quota ;;
        01) echo quota ;;
        10) echo inventory ;;
        *) echo none ;;
    esac
}

# us_share_cluster_budget -- what the share optimizer takes as the cluster-wide
# budget: quota, physical or none, the controller's Config.HasClusterQuota.
# Only a cluster-scope quota is a cluster budget, alone or beside the physical
# limiter; namespace-scope quotas bound their namespaces. Beside the physical
# limiter, the cluster figure is then the GPUs that exist; with namespace-scope
# quotas alone there is none, and a namespace they exclude is not shared.
us_share_cluster_budget() {
    local p kind cluster
    p="$(cat)"
    kind="$(printf '%s\n' "$p" | us_limiter_kind)" || return 1
    cluster="$(printf '%s\n' "$p" | yq -r '[.limiters[]? | select(.type == "quota" and .scope == "cluster")] | length')" || return 1
    case "$kind" in
        quota) if [ "$cluster" -gt 0 ]; then echo quota; else echo none; fi ;;
        inventory+quota) if [ "$cluster" -gt 0 ]; then echo quota; else echo physical; fi ;;
        inventory) echo physical ;;
        *) echo none ;;
    esac
}

# us_quota_keys -- the accelerator keys of the quota entries, one per line:
# every key whose value is a count.
us_quota_keys() {
    yq -r '[.limiters[]? | select(.type == "quota") | .. | select(tag == "!!int") | path | .[-1]] | unique | .[]'
}

# us_budget_scope -- whether namespaces share a budget, the way the controller
# builds its share groups (internal/engines/allocation/utilization_share_groups.go):
#
#   cluster    some namespaces share one group per accelerator
#   namespace  every planned namespace has its own budget
#   none       no group at all, so nothing is shared
#
# A namespace that a namespace-scoped entry covers -- listed, or by `default`,
# or not at all, which is a zero budget -- gets its own group. So with a quota
# entry the cluster group holds every namespace when there is no
# namespace-scoped entry, and otherwise only the namespaces excluded from EVERY
# namespace-scoped entry (us_excluded_namespaces). Those share it only under a
# cluster bound that is shared: a `scope: cluster` quota entry, or a
# gpu-inventory limiter beside the quota with physicalGroups: true. Otherwise
# an excluded namespace is in no group at all: the sum of the namespace quotas
# is not a budget for a namespace they do not cap (with no finite bound the
# controller logs it as unshared, and the default optimizer keeps it). Which
# accelerator types a bound gives a number is not read here.
# Without a quota, the inventory forms a cluster group only with
# physicalGroups: true, and no limiter forms none.
us_budget_scope() {
    local p kind out
    p="$(cat)"
    kind="$(printf '%s\n' "$p" | us_limiter_kind)" || return 1
    case "$kind" in
        none) echo none; return 0 ;;
        inventory)
            out="$(printf '%s\n' "$p" | yq -r '.optimizer.utilizationShare.physicalGroups == true')" || return 1
            if [ "$out" = true ]; then echo cluster; else echo none; fi
            return 0
            ;;
    esac
    out="$(printf '%s\n' "$p" | yq -r '[.limiters[]? | select(.type == "quota" and .scope == "namespace")] | length')" || return 1
    if [ "$out" = 0 ]; then echo cluster; return 0; fi
    out="$(printf '%s\n' "$p" | us_excluded_namespaces)" || return 1
    if [ -z "$out" ]; then echo namespace; return 0; fi
    out="$(printf '%s\n' "$p" | yq -r '[.limiters[]? | select(.type == "quota" and .scope == "cluster")] | length')" || return 1
    if [ "$out" != 0 ]; then echo cluster; return 0; fi
    if [ "$kind" = inventory+quota ]; then
        out="$(printf '%s\n' "$p" | yq -r '.optimizer.utilizationShare.physicalGroups == true')" || return 1
        if [ "$out" = true ]; then echo cluster; return 0; fi
    fi
    echo namespace
}

# us_excluded_namespaces -- the namespaces excluded from every namespace-scoped
# quota entry, one per line: under a quota with namespace-scoped entries, the
# only namespaces that can share the cluster group. Matched by equality: an
# array contains test matches substrings, so an exclude of team-a-prod read as
# excluding team-a.
us_excluded_namespaces() {
    yq -r '[.limiters[]? | select(.type == "quota" and .scope == "namespace") | (.exclude // [])] as $e
        | ($e[0] // []) | .[] | select(. as $n | ($e | map(select(any_c(. == $n))) | length) == ($e | length))'
}

# us_cluster_namespaces -- the block's clusterNamespaces, one per line: the only
# namespaces a cluster budget's groups plan when it lists any.
us_cluster_namespaces() {
    yq -r '(.optimizer.utilizationShare.clusterNamespaces // [])[]'
}

# us_block_state lives in lib/pod_writer.sh, which the installer shares.

# us_policy_with_mode <shadow|active|off> [physical] -- the policy with the
# block in that mode. Every other key, of the block and of the policy, is kept.
us_policy_with_mode() {
    local mode="$1" physical="${2:-}" out
    case "$mode" in
        shadow) out="$(yq '.optimizer.type = "utilizationShare" | .optimizer.utilizationShare.shadow = true')" ;;
        active) out="$(yq '.optimizer.type = "utilizationShare" | .optimizer.utilizationShare.shadow = false')" ;;
        off)    out="$(yq 'del(.optimizer)')" ;;
        *) return 2 ;;
    esac || return 1
    if [ "$physical" = physical ]; then
        out="$(printf '%s\n' "$out" | yq '.optimizer.utilizationShare.physicalGroups = true')" || return 1
    fi
    # Written back empty, this would blank the whole policy.
    [ -n "$(printf '%s' "$out" | tr -d '[:space:]')" ] || return 1
    printf '%s\n' "$out"
}

# ---------------------------------------------------------------------------
# Cluster reads and writes. A read that fails is never an empty answer, and
# kubectl's warnings never end up in what is read (or written back).
# ---------------------------------------------------------------------------

ERRF="$(mktemp)"
trap 'rm -f "$ERRF"' EXIT

# kread <var> <kubectl args...> -- stdout into <var>; stderr kept apart and
# returned only on failure.
kread() {
    local __var="$1" __out
    shift
    if __out="$(k "$@" 2>"$ERRF")"; then
        printf -v "$__var" '%s' "$__out"
        return 0
    fi
    printf -v "$__var" '%s' "$(cat "$ERRF")"
    return 1
}

POLICY=""
POLICY_RV=""
POLICY_FOUND=0
read_policy() {
    local out
    if kread out get configmap "$POLICY_CM" -n "$POLICY_NS" -o json; then
        POLICY="$(printf '%s' "$out" | jq -r '.data.default // ""')" || die "could not parse $POLICY_NS/$POLICY_CM"
        POLICY_RV="$(printf '%s' "$out" | jq -r '.metadata.resourceVersion // ""')"
        POLICY_FOUND=1
        if [ "$(printf '%s\n' "$POLICY" | us_doc_count)" -gt 1 ]; then
            die "the policy in $POLICY_NS/$POLICY_CM holds more than one YAML document; the controller reads only the first. Fix it by hand first."
        fi
        return 0
    fi
    case "$out" in
        *NotFound*|*"not found"*) POLICY=""; POLICY_RV=""; POLICY_FOUND=0 ;;
        *) die "could not read $POLICY_NS/$POLICY_CM: $out" ;;
    esac
}

block_state() {
    local s
    s="$(printf '%s\n' "$POLICY" | us_block_state)" || die "could not parse the policy in $POLICY_NS/$POLICY_CM"
    printf '%s' "$s"
}

limiter_kind() {
    local s
    s="$(printf '%s\n' "$POLICY" | us_limiter_kind)" || die "could not parse the policy in $POLICY_NS/$POLICY_CM"
    printf '%s' "$s"
}

share_cluster_budget() {
    local s
    s="$(printf '%s\n' "$POLICY" | us_share_cluster_budget)" || die "could not parse the policy in $POLICY_NS/$POLICY_CM"
    printf '%s' "$s"
}

# write_policy <policy> -- only over the version just read: a change made in
# between (a limiter edit while `off` waited) fails the write instead of being
# overwritten.
WRITTEN_RV=""
write_policy() {
    local out
    kread out patch configmap "$POLICY_CM" -n "$POLICY_NS" --type=merge \
        -p "$(jq -cn --arg d "$1" --arg rv "$POLICY_RV" '{metadata:{resourceVersion:$rv},data:{"default":$d}}')" \
        -o jsonpath='{.metadata.resourceVersion}' \
        || die "could not write $POLICY_NS/$POLICY_CM (if it changed since it was read, rerun): $out"
    WRITTEN_RV="$out"
}

# STATUS -- the controller's published status as JSON data, or empty when the
# controller publishes none (an older version). Unreadable is not absent.
STATUS=""
read_status() {
    local out
    if kread out get configmap "$STATUS_CM" -n "$CONTROLLER_NS" -o json; then
        STATUS="$(printf '%s' "$out" | jq -c '.data // {}')" || die "could not parse $CONTROLLER_NS/$STATUS_CM"
        return 0
    fi
    case "$out" in
        *NotFound*|*"not found"*) STATUS="" ;;
        *) die "could not read $CONTROLLER_NS/$STATUS_CM: $out" ;;
    esac
}
status_field() { printf '%s' "$STATUS" | jq -r --arg k "$1" '.[$k] // ""'; }

# us_block_json -- the optimizer block of the policy on stdin, as compact JSON
# with sorted keys (null when there is none), so two policies compare by the
# block alone.
us_block_json() {
    yq -o=json -I=0 '.optimizer' | jq -cS '.'
}

# later_version_keeps_block <rv> -- the ConfigMap is NOW at <rv> and still
# carries the optimizer block this script wrote (POLICY). A concurrent write --
# an admin raising a quota, another tool -- moves the resourceVersion past the
# one written, and the controller then reports THAT version; requiring the
# written one exactly failed a write the controller had applied. Only the
# current version is accepted, so a status that lags behind ours never passes,
# and only with our block intact, so a later write that replaced the block does
# not count as ours being applied.
later_version_keeps_block() {
    local want_rv="$1" out cur_rv cur want
    kread out get configmap "$POLICY_CM" -n "$POLICY_NS" -o json || return 1
    cur_rv="$(printf '%s' "$out" | jq -r '.metadata.resourceVersion // ""')" || return 1
    [ -n "$cur_rv" ] && [ "$cur_rv" = "$want_rv" ] || return 1
    cur="$(printf '%s' "$out" | jq -r '.data.default // ""' | us_block_json)" || return 1
    want="$(printf '%s\n' "$POLICY" | us_block_json)" || return 1
    [ -n "$want" ] && [ "$cur" = "$want" ]
}

# confirm_by_status <want mode> -- wait until the controller reports the version
# just written (or a later one that still carries the block written, see
# later_version_keeps_block), then require it accepted and running in <want>.
confirm_by_status() {
    local want="$1" deadline restarted=0 rv acc mode err
    deadline=$(( $(date +%s) + RELOAD_WAIT ))
    while :; do
        read_status
        rv="$(status_field policyResourceVersion)"
        if [ "$(status_field policyNamespace)" = "$POLICY_NS" ] && [ -n "$rv" ]; then
            [ "$rv" != "$WRITTEN_RV" ] || break
            if later_version_keeps_block "$rv"; then
                info "the policy was written again after this script (version $rv); that version still carries the block written"
                WRITTEN_RV="$rv"
                break
            fi
        fi
        if [ "$(date +%s)" -ge "$deadline" ]; then
            if [ "$RESTART" -eq 1 ] && [ "$restarted" -eq 0 ]; then
                info "the controller has not applied version $WRITTEN_RV; restarting it"
                kread err rollout restart "deploy/$CONTROLLER_DEPLOY" -n "$CONTROLLER_NS" || die "could not restart the controller: $err"
                wait_controller_back
                restarted=1
                deadline=$(( $(date +%s) + RELOAD_WAIT ))
                continue
            fi
            die "the controller has not applied version $WRITTEN_RV of $POLICY_NS/$POLICY_CM within ${RELOAD_WAIT}s (it reports version '${rv:-none}' from '$(status_field policyNamespace)'). The policy WAS written.$( [ "$restarted" -eq 1 ] || echo ' Rerun with --restart (RESTART=1) to restart the controller.')"
        fi
        sleep 2
    done
    acc="$(status_field policyAccepted)"
    [ "$acc" != false ] || die "the controller rejected the policy: $(status_field policyError)"
    mode="$(status_field utilizationShare)"
    [ "$mode" != invalid ] || die "the controller found the optimizer block invalid: $(status_field utilizationShareError)"
    [ "$mode" = "$want" ] || die "the controller applied the policy but runs the optimizer in mode '$mode', not '$want'"
    ok "the controller applied version $WRITTEN_RV: utilization share is $mode"
}

# The controller's pods, by its Deployment's selector. With more than one
# replica only the leader reconciles and decides, and `kubectl logs deploy/X`
# reads one pod, which may be a standby.
SELECTOR=""
DEPLOY_JSON=""
controller_deploy() {
    [ -n "$DEPLOY_JSON" ] && return 0
    kread DEPLOY_JSON get deploy "$CONTROLLER_DEPLOY" -n "$CONTROLLER_NS" -o json \
        || die "could not read the controller $CONTROLLER_NS/$CONTROLLER_DEPLOY: $DEPLOY_JSON"
}
controller_selector() {
    [ -n "$SELECTOR" ] && return 0
    controller_deploy
    SELECTOR="$(printf '%s' "$DEPLOY_JSON" | jq -r '.spec.selector.matchLabels // {} | to_entries | map(.key + "=" + .value) | join(",")')"
    [ -n "$SELECTOR" ] || die "the controller Deployment has no matchLabels selector to find its pods by"
}

# controller_managed_ns -- the namespace the controller manages, or * when it
# is cluster-scoped: its --watch-namespace, resolved as PL_DEPLOY_ROWS resolves
# it (lib/physical_limiter.sh).
controller_managed_ns() {
    local rows
    controller_deploy
    rows="$(printf '{"items":[%s]}' "$DEPLOY_JSON" | jq -r --arg ns "$CONTROLLER_NS" "$PL_DEPLOY_ROWS")" \
        || die "could not read which namespace $CONTROLLER_NS/$CONTROLLER_DEPLOY manages"
    printf '%s' "$rows" | head -1 | cut -f4
}

# LOGS -- every controller pod's log [since <arg>]. Fails loudly: an unreadable
# log is not one that says nothing.
LOGS=""
read_logs() {
    controller_selector
    kread LOGS logs -n "$CONTROLLER_NS" -l "$SELECTOR" -c "$CONTROLLER_CONTAINER" \
        --tail=-1 --max-log-requests=10 "$@" \
        || die "could not read the log of $CONTROLLER_NS/$CONTROLLER_DEPLOY: $LOGS"
}

# The policy namespace in LOGS, if the controller logged one (at start, or on
# reading that namespace's policy).
logged_policy_ns() {
    printf '%s\n' "$LOGS" \
        | grep -E 'Cluster policy comes from the (well-known namespace|namespace this one is labelled with)|Loaded cluster policy from the policy namespace' \
        | grep -oE '"policyNamespace": ?"[^"]*"' | tail -1 | sed -E 's/.*: ?"//; s/"$//' || true
}

# derived_policy_ns -- the policy namespace by the controller's own rule
# (internal/controller/policy_namespace.go), for when its log no longer holds
# the line that names it: the kubelet rotates the log of a long-running pod.
# A controller that manages another namespace reads its own ConfigMap; one that
# manages its own reads the namespace its Namespace is labelled with, else
# scaling-manager-policy if that exists, else its own.
DERIVED=""
derived_policy_ns() {
    local out label
    if [ "$NS" != "$CONTROLLER_NS" ]; then
        DERIVED="$CONTROLLER_NS"
        return 0
    fi
    kread out get namespace "$NS" -o json \
        || die "could not read Namespace $NS to work out which policy the controller reads: $out. Pass --policy-namespace."
    label="$(printf '%s' "$out" | jq -r '.metadata.labels["scaling-manager.llmd.ai/policy-namespace"] // ""')"
    if [ -n "$label" ]; then
        DERIVED="$label"
        return 0
    fi
    if kread out get namespace scaling-manager-policy -o name; then
        DERIVED="scaling-manager-policy"
        return 0
    fi
    case "$out" in
        *NotFound*|*"not found"*) DERIVED="$CONTROLLER_NS" ;;
        *) die "could not tell whether a scaling-manager-policy namespace exists: $out. Pass --policy-namespace." ;;
    esac
}

# resolve_policy_ns -- POLICY_NS from the flag, else from the controller's log,
# else by the controller's rule.
resolve_policy_ns() {
    local logged
    read_status
    logged="$(status_field policyNamespace)"
    if [ -n "$logged" ]; then
        if [ -n "$POLICY_NS" ] && [ "$POLICY_NS" != "$logged" ]; then
            die "the controller reads its policy from $logged, not $POLICY_NS: a block written there would be ignored"
        fi
        POLICY_NS="$logged"
        info "policy namespace: $POLICY_NS (from $CONTROLLER_NS/$STATUS_CM)"
        return 0
    fi
    read_logs
    logged="$(logged_policy_ns)"
    if [ -z "$logged" ]; then
        derived_policy_ns
        [ "$DERIVED" = "$CONTROLLER_NS" ] || logged="$DERIVED"
    fi
    if [ -n "$POLICY_NS" ]; then
        if [ -n "$logged" ] && [ "$POLICY_NS" != "$logged" ]; then
            die "the controller reads its policy from $logged, not $POLICY_NS: a block written there would be ignored"
        fi
        if [ -z "$logged" ] && [ "$POLICY_NS" != "$CONTROLLER_NS" ]; then
            die "the controller reads its own policy in $CONTROLLER_NS, not $POLICY_NS: a block written there would be ignored"
        fi
        return 0
    fi
    POLICY_NS="${logged:-$CONTROLLER_NS}"
    [ -z "$logged" ] || info "policy namespace: $logged"
}

# reload_seen -- LOGS hold the line the controller logs when it reads
# POLICY_NS's policy.
#
# Here-strings, never `printf | grep -q`: under pipefail, grep -q exiting at its
# first match kills printf with SIGPIPE, and a large log then reads as one that
# does not hold the line at all.
reload_seen() {
    if [ "$POLICY_NS" = "$CONTROLLER_NS" ]; then
        grep -q 'Updated global scaling policy' <<<"$LOGS"
    else
        grep -qE "Loaded cluster policy from the policy namespace.*\"policyNamespace\": ?\"$POLICY_NS\"" <<<"$LOGS"
    fi
}

# wait_reload <start epoch> -- until the reload line appears after <start>.
# --since is measured on the node's clock, so a local clock that runs ahead
# cannot hide it; the 10 s margin only admits lines from just before.
wait_reload() {
    local start="$1" deadline=$(( $1 + RELOAD_WAIT ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        read_logs --since="$(( $(date +%s) - start + 10 ))s"
        reload_seen && return 0
        sleep 5
    done
    return 1
}

# confirm_reload <start epoch> -- the controller read the policy written at
# <start> and did not refuse it within a cycle. A refusal is logged once, when
# its text changes, by a cycle that plans models.
confirm_reload() {
    local start="$1" bad
    if ! wait_reload "$start"; then
        [ "$RESTART" -eq 1 ] || die "the controller logged no reload of $POLICY_NS/$POLICY_CM within ${RELOAD_WAIT}s. The policy WAS written. Rerun with --restart (RESTART=1) to restart the controller and require the reload."
        info "no reload logged; restarting the controller"
        kread bad rollout restart "deploy/$CONTROLLER_DEPLOY" -n "$CONTROLLER_NS" || die "could not restart the controller: $bad"
        wait_controller_back
        start="$(date +%s)"
        wait_reload "$start" || die "the restarted controller logged no read of $POLICY_NS/$POLICY_CM either: check --policy-namespace"
    fi
    sleep "$CYCLE_WAIT"
    read_logs --since="$(( $(date +%s) - start + 10 ))s"
    bad="$(printf '%s\n' "$LOGS" | grep -E 'invalid optimizer block|Invalid saturation scaling' | tail -1 || true)"
    [ -z "$bad" ] || die "the controller refused the policy: ${bad:0:400}"
    ok "the controller read the policy and did not refuse it"
}

# set_mode <shadow|active|off> [physical] -- from the policy as it is NOW.
set_mode() {
    local want="$1" updated start got
    read_policy
    [ "$POLICY_FOUND" -eq 1 ] || die "$POLICY_NS/$POLICY_CM disappeared; nothing was written"
    updated="$(printf '%s\n' "$POLICY" | us_policy_with_mode "$want" "${2:-}")" \
        || die "could not rewrite the policy; nothing was written"
    got="$(printf '%s\n' "$updated" | us_block_state)" || die "could not parse the rewritten policy; nothing was written"
    [ "${got% *}" = "$want" ] || die "the rewritten policy reads ${got% *}, not $want; nothing was written"
    if [ "$updated" = "$(printf '%s\n' "$POLICY")" ]; then
        ok "the policy is already $want"
        return 0
    fi
    start="$(date +%s)"
    write_policy "$updated"
    POLICY="$updated"
    info "wrote the optimizer block ($want) into $POLICY_NS/$POLICY_CM"
    if [ -n "$STATUS" ]; then
        confirm_by_status "$want"
    else
        confirm_reload "$start"
    fi
}

# MARKED -- pods carrying a transfer mark.
MARKED=""
read_marks() {
    local pods managed one part all=""
    # Every namespace a controller managing a list manages; else NS.
    managed="$(controller_managed_ns)"
    case "$managed" in *,*) ;; *) managed="$NS" ;; esac
    for one in $(printf '%s' "$managed" | tr ',' ' '); do
        kread pods get pods -n "$one" -o json || die "could not list pods in $one, so the transfer marks were not checked: $pods"
        part="$(printf '%s' "$pods" | jq -r --arg m "$MARK" --arg ns "$one" --arg own "$NS" \
            '.items[] | select(.metadata.annotations[$m] != null) | (if $ns == $own then "" else $ns + "/" end) + .metadata.name')" \
            || die "could not parse the pod list in $one, so the transfer marks were not checked"
        [ -z "$part" ] || all="${all:+$all
}$part"
    done
    MARKED="$all"
}

# ---------------------------------------------------------------------------
# Verbs.
# ---------------------------------------------------------------------------

verb_check() {
    local rc=0 kind state line accels events keys a
    read_policy
    [ "$POLICY_FOUND" -eq 1 ] || die "no scaling policy in $POLICY_NS: there is no limiter, so there is no budget to share"
    info "policy: $POLICY_NS/$POLICY_CM"
    kind="$(limiter_kind)"
    state="$(block_state)"
    case "$kind" in
        quota) ok "limiter: quota; the optimizer shares each quota group" ;;
        inventory+quota)
            # Both bound every decision. Which one is the share's CLUSTER budget
            # is the controller's rule (HasClusterQuota), said here so the
            # numbers status prints can be read.
            if [ "$(share_cluster_budget)" = quota ]; then
                ok "limiters: gpu-inventory and quota, both in force; the share's cluster budget is the cluster-scope quota, and the GPUs that exist bound it too"
            else
                ok "limiters: gpu-inventory and quota, both in force; the share's cluster budget is the GPUs that exist, and each namespace-scope quota bounds its namespace"
            fi ;;
        inventory)
            if [ "${state#* }" = physical ] && [ "$(controller_managed_ns)" != "*" ]; then
                warn "limiter: gpu-inventory, and the block sets physicalGroups, but this controller is namespace-scoped and ignores it (utilizationShareNote in $STATUS_CM): the optimizer plans nothing here. Bound the namespace with a quota"
            elif [ "${state#* }" = physical ]; then
                ok "limiter: gpu-inventory, and the block plans physical groups"
            else
                warn "limiter: gpu-inventory and no quota. The optimizer plans nothing here until the block sets physicalGroups: run shadow with --physical-groups (PHYSICAL_GROUPS=true)"
            fi ;;
        *) fail "the policy declares no limiters: list, so there is no budget to share (the mode would read invalid). Declare one: a quota at install, or make enable-physical-limiter"; rc=1 ;;
    esac

    read_logs
    line="$(printf '%s\n' "$LOGS" | grep -E 'GPU limiter (constructed|\(re\)built from config)|Invalid saturation scaling' | tail -1 || true)"
    case "$line" in
        "") warn "the controller's log names no limiter: it logs one when the limiter is built, and the kubelet may have rotated that line away" ;;
        *"Invalid saturation scaling"*) fail "the controller refused the policy: ${line:0:300}"; rc=1 ;;
        *) info "controller: ${line:0:300}" ;;
    esac

    # Both log encoders: `"accelerator": "x"` (console) and `"accelerator":"x"` (json).
    accels="$(printf '%s\n' "$LOGS" | grep -oE '"accelerator": ?"[^"]*"' | sed -E 's/.*: ?"//; s/"$//' | sort -u || true)"
    if [ -z "$accels" ]; then
        warn "the controller has logged no accelerator yet; rerun once the models have been planned"
    else
        info "accelerators the controller resolves: $(printf '%s' "$accels" | tr '\n' ' ')"
    fi
    # A cluster-scoped controller plans models of every namespace into one
    # group, so an unresolved accelerator in another namespace matters as much
    # as one here: that model gets no budget and the group plans around it.
    # Read every namespace for it; NS alone passed a cluster where only the
    # other namespace was broken.
    local where="-n $NS" in_where="in $NS" managed one part
    managed="$(controller_managed_ns)"
    if [ "$managed" = "*" ]; then
        where="-A" in_where="in the namespaces this cluster-scoped controller manages"
    fi
    case "$managed" in
        *,*)
            # A namespace-scoped controller managing a list: each of them.
            in_where="in $(printf '%s' "$managed" | sed 's/,/, /g')"
            events=""
            for one in $(printf '%s' "$managed" | tr ',' ' '); do
                kread part get events -n "$one" --field-selector reason=AcceleratorNotResolved -o name \
                    || die "could not read events in $one: $part"
                [ -z "$part" ] || events="${events:+$events
}$part"
            done ;;
        *)
            # shellcheck disable=SC2086 # $where is one or two words by construction
            kread events get events $where --field-selector reason=AcceleratorNotResolved -o name \
                || die "could not read events $in_where: $events" ;;
    esac
    if [ -n "$events" ]; then
        fail "$(printf '%s\n' "$events" | grep -c .) AcceleratorNotResolved event(s) $in_where: those variants get no budget. List them: kubectl get events $where --field-selector reason=AcceleratorNotResolved"; rc=1
    else
        ok "no AcceleratorNotResolved events"
    fi
    if { [ "$kind" = quota ] || [ "$kind" = inventory+quota ]; } && [ -n "$accels" ]; then
        keys="$(printf '%s\n' "$POLICY" | us_quota_keys)" || die "could not parse the quota entries"
        for a in $accels; do
            grep -qxF -- "$a" <<<"$keys" \
                || warn "the quota has no '$a' key: its models are charged to no quota group (the key must be the name the controller logs)"
        done
    fi

    case "${state% *}" in
        off) info "optimizer block: none (today's optimizer). Next: shadow" ;;
        shadow) info "optimizer block: shadow. Next: status, then activate" ;;
        active) info "optimizer block: active" ;;
        other:*) warn "the policy has an optimizer block this script does not own (${state% *}); it will not replace it" ;;
    esac
    return "$rc"
}

verb_shadow() {
    local state kind physical="" managed
    read_policy
    if [ "$PHYSICAL" -eq 1 ]; then
        managed="$(controller_managed_ns)"
        [ "$managed" = "*" ] \
            || die "--physical-groups is refused: the controller manages only $managed (namespace-scoped), and a namespace-scoped controller ignores physicalGroups: every such controller would plan the same free GPUs of the cluster. Bound the namespace with a quota limiter instead, or run the share from a cluster-scoped controller"
    fi
    [ "$POLICY_FOUND" -eq 1 ] || die "no scaling policy in $POLICY_NS: there is no limiter, so there is no budget to share"
    state="$(block_state)"
    case "${state% *}" in
        other:*) die "the policy already has an optimizer block this script does not own (${state% *}); change that block instead" ;;
        shadow) if [ "$PHYSICAL" -eq 0 ] || [ "${state#* }" = physical ]; then ok "already in shadow"; return 0; fi ;;
    esac
    kind="$(limiter_kind)"
    [ "$kind" != none ] || die "the policy declares no limiters: list, so there is no budget to share"
    if [ "$PHYSICAL" -eq 1 ]; then
        physical=physical
    elif [ "$kind" = inventory ] && [ "${state#* }" != physical ]; then
        warn "the limiter is gpu-inventory and there is no quota: without --physical-groups the optimizer plans nothing"
    fi
    set_mode shadow "$physical"
    info "shadow computes and reports and moves nothing. Read 'status' over a few bursts before 'activate'."
}

# warn_ungranted_namespaces -- activate grants only in NS, and the optimizer
# also moves pods in the other namespaces of the same budget. So it names each
# of those it can see -- a namespace holding a ScaledObject that calls this
# controller -- where the controller may not patch pods, with the command that
# grants it there. It grants nothing there itself.
#
# Which namespaces share the budget, from the policy (us_budget_scope): none
# when every planned namespace has its own budget or no group forms; under
# namespace-scoped quotas, only the namespaces excluded from all of them, and
# only under a cluster-scoped quota or a gpu-inventory limiter with
# physicalGroups: true; otherwise every namespace.
# clusterNamespaces, when the block lists some, narrows either. A cluster budget is split per accelerator, which this script
# cannot see, so the warning says "if". A list it cannot read is said, not
# taken for none.
# us_managed_list -- the namespaces a namespace-scoped controller managing a
# list manages, one per line. Fails for any other controller. Its ScaledObjects
# can only be in those, so this is the set that shares its budget, read
# without a cluster-wide list.
us_managed_list() {
    local managed
    managed="$(controller_managed_ns)"
    case "$managed" in *,*) ;; *) return 1 ;; esac
    printf '%s\n' "$managed" | tr ',' '\n' | grep -v '^$'
}

warn_ungranted_namespaces() {
    local list ns only excluded odd
    case "$(printf '%s\n' "$POLICY" | us_budget_scope)" in
        namespace)
            info "every planned namespace has its own quota, and one the quota excludes is shared only under a cluster-scoped quota or physicalGroups, so no other namespace shares this budget"
            return 0 ;;
        none)
            info "this limiter forms no shared group, so no other namespace shares this budget"
            return 0 ;;
    esac
    only="$(printf '%s\n' "$POLICY" | us_cluster_namespaces)" || die "could not parse the policy in $POLICY_NS/$POLICY_CM"
    excluded="$(printf '%s\n' "$POLICY" | us_excluded_namespaces)" || die "could not parse the policy in $POLICY_NS/$POLICY_CM"
    if list="$(us_managed_list)"; then
        : # a controller managing a list: exactly those namespaces
    elif ! list="$(llmsm_pod_writer_namespaces "$CONTROLLER_NS" models 2>&1)"; then
        [ "$GRANT_SHARED" -eq 0 ] \
            || die "could not list ScaledObjects cluster-wide, so --grant-shared cannot tell which namespaces share this budget (${list:0:300}). Run activate in each of them instead"
        warn "could not list ScaledObjects cluster-wide, so other namespaces sharing this budget are unknown (${list:0:300}). Run activate in each of them, or the optimizer cannot move their pods"
        return 0
    fi
    if odd="$(llmsm_pod_writer_namespaces "$CONTROLLER_NS" unattributed 2>/dev/null)" && [ -n "$odd" ]; then
        warn "ScaledObjects whose scalerAddress does not say which controller they call: $(printf '%s\n' "$odd" | awk '{printf "%s (%s) ", $1, $2}'). If one calls this controller and shares this budget, run activate in its namespace too"
    fi
    for ns in $list; do
        [ "$ns" != "$NS" ] || continue
        if [ -n "$only" ] && ! grep -qxF -- "$ns" <<<"$only"; then
            continue
        fi
        if [ -n "$excluded" ] && ! grep -qxF -- "$ns" <<<"$excluded"; then
            continue
        fi
        if [ "$GRANT_SHARED" -eq 1 ] && [ "$(llmsm_pod_writes_allowed "$ns" "$CONTROLLER_NS")" != yes ]; then
            llmsm_ensure_pod_writes "$ns" "$CONTROLLER_NS" \
                || die "could not grant the controller patch on pods in $ns (role/scaling-manager-pod-writer): ${LLMSM_POD_WRITER_ERR}. This needs create on roles and rolebindings there; or an admin runs: $(llmsm_pod_writer_admin_hint "$ns" "$CONTROLLER_NS")"
            ok "the controller in $CONTROLLER_NS may patch pods in $ns (role/scaling-manager-pod-writer), which shares this budget"
            continue
        fi
        case "$(llmsm_pod_writes_allowed "$ns" "$CONTROLLER_NS")" in
            yes) ;;
            no) warn "the controller may not patch pods in $ns. If its models use the same accelerator, they share this budget and the optimizer cannot move their pods. Run: $(us_activate_cmd "$ns")" ;;
            *) warn "could not tell whether the controller may patch pods in $ns. If its models use the same accelerator, they share this budget; run: $(us_activate_cmd "$ns")" ;;
        esac
    done
}

# us_shared_namespaces -- NS, then every other namespace that shares its
# budget, by the rules warn_ungranted_namespaces applies: none under quotas per
# namespace or a limiter that forms no group; else the namespaces whose
# ScaledObjects call this controller, narrowed by clusterNamespaces and the
# quota exclusions. Returns 1 when those ScaledObjects cannot be listed.
us_shared_namespaces() {
    local list ns only excluded
    echo "$NS"
    case "$(printf '%s\n' "$POLICY" | us_budget_scope)" in
        namespace|none) return 0 ;;
    esac
    only="$(printf '%s\n' "$POLICY" | us_cluster_namespaces)" || return 1
    excluded="$(printf '%s\n' "$POLICY" | us_excluded_namespaces)" || return 1
    list="$(us_managed_list)" || list="$(llmsm_pod_writer_namespaces "$CONTROLLER_NS" models 2>/dev/null)" || return 1
    for ns in $list; do
        [ "$ns" != "$NS" ] || continue
        if [ -n "$only" ] && ! grep -qxF -- "$ns" <<<"$only"; then continue; fi
        if [ -n "$excluded" ] && ! grep -qxF -- "$ns" <<<"$excluded"; then continue; fi
        echo "$ns"
    done
}

# us_activate_cmd <ns> -- the make command that runs activate in <ns> for this
# controller and policy.
us_activate_cmd() {
    printf 'make utilization-share-activate NAMESPACE=%s CONTROLLER_NAMESPACE=%s%s\n' "$1" "$CONTROLLER_NS" \
        "${POLICY_NS:+ POLICY_NAMESPACE=$POLICY_NS}"
}

verb_activate() {
    local state
    read_policy
    [ "$POLICY_FOUND" -eq 1 ] || die "no scaling policy in $POLICY_NS: there is nothing to activate"
    state="$(block_state)"
    case "${state% *}" in
        active|shadow) ;;
        off) die "no optimizer block: run shadow first and read what it would move" ;;
        *) die "the policy has an optimizer block this script does not own (${state% *}); it will not replace it" ;;
    esac
    # Active moves pods' deletion cost, which is a patch. The controller's
    # ClusterRole only reads pods, so the write is granted here, in this
    # namespace alone (lib/pod_writer.sh), before the mode that needs it --
    # and also when the block is already active, which is how a second
    # namespace sharing one budget gets its grant.
    llmsm_ensure_pod_writes "$NS" "$CONTROLLER_NS" \
        || die "could not grant the controller patch on pods in $NS (role/scaling-manager-pod-writer): ${LLMSM_POD_WRITER_ERR}. This needs create on roles and rolebindings there; or an admin runs: $(llmsm_pod_writer_admin_hint "$NS" "$CONTROLLER_NS")"
    case "$LLMSM_POD_WRITER_RESULT" in
        already) ok "the controller in $CONTROLLER_NS can already patch pods in $NS" ;;
        *) ok "the controller in $CONTROLLER_NS may patch pods in $NS (role/scaling-manager-pod-writer)" ;;
    esac
    warn_ungranted_namespaces
    if [ "${state% *}" = active ]; then
        ok "already active"
        return 0
    fi
    set_mode active
    info "for two to three minutes nothing in the group scales (quiet-period); the same follows every controller restart"
}

verb_status() {
    local state decisions ev names warnings namespaces ns n total per
    read_policy
    state="$(block_state)"
    info "policy: $POLICY_NS/$POLICY_CM; optimizer block: ${state% *}$( [ "${state#* }" = physical ] && echo ', physical groups' || true)"
    if [ -n "$STATUS" ]; then
        info "controller: mode $(status_field utilizationShare), limiter $(status_field limiter), policy version $(status_field policyResourceVersion) accepted=$(status_field policyAccepted), leader $(status_field leader)"
        [ -z "$(status_field utilizationShareError)" ] || warn "optimizer block error: $(status_field utilizationShareError)"
        [ "$(status_field policyAccepted)" != false ] || warn "policy rejected: $(status_field policyError)"
    fi
    read_logs --since=30m
    decisions="$(printf '%s\n' "$LOGS" | grep -E 'Utilization share: (would rebalance|invalid optimizer block|transfer (started|ended|cancelled))' | tail -5 || true)"
    if [ -n "$decisions" ]; then
        printf '%s\n' "$decisions" | cut -c1-300 | sed 's/^/  log: /' >&2
    else
        info "no utilization-share decision logged in the last 30 min (normal with no load: shadow logs only when its moves change)"
    fi
    # Every namespace of the budget, not only NS: under a cluster budget the
    # giver and the receiver are usually in different namespaces, and counting
    # NS alone reported 0 moves while GPUs were moving.
    if ! namespaces="$(us_shared_namespaces)"; then
        warn "could not list the ScaledObjects calling this controller, so the other namespaces of this budget are unknown; events below are for $NS only"
        namespaces="$NS"
    fi
    [ "$(printf '%s\n' "$namespaces" | grep -c .)" -le 1 ] \
        || info "events counted in the namespaces of this budget: $(printf '%s\n' "$namespaces" | paste -sd' ' -)"
    for ev in UtilizationShareGiving UtilizationShareReceiving UtilizationShareReceived; do
        total=0; per=""
        for ns in $namespaces; do
            kread names get events -n "$ns" --field-selector "reason=$ev" -o name || die "could not read events in $ns: $names"
            n="$(printf '%s' "$names" | grep -c . || true)"
            total=$((total + n))
            [ "$n" -eq 0 ] || per="${per:+$per, }$ns $n"
        done
        info "$ev events: $total${per:+ ($per)}"
    done
    warnings=""
    for ns in $namespaces; do
        kread names get events -n "$ns" --field-selector type=Warning || die "could not read events in $ns: $names"
        [ -z "$names" ] || warnings="${warnings:+$warnings
}$names"
    done
    if grep -q UtilizationShare <<<"$warnings"; then
        warn "UtilizationShare warnings:"
        printf '%s\n' "$warnings" | grep UtilizationShare | sed 's/^/    /' >&2
    else
        ok "no UtilizationShare warnings"
    fi
    read_marks
    info "pods marked for a transfer: ${MARKED:-none}"
}

verb_off() {
    local state deadline
    read_policy
    state="$(block_state)"
    case "${state% *}" in
        off) ok "no optimizer block: already off"; return 0 ;;
        other:*) die "the optimizer block is not one this script owns (${state% *}); it will not remove it" ;;
        active) set_mode shadow ;;
    esac
    # Through shadow first, so the marks are gone before the block is: the
    # controller clears them in shadow too, and waiting here makes it visible.
    deadline=$(( $(date +%s) + MARK_WAIT ))
    read_marks
    while [ -n "$MARKED" ]; do
        [ "$(date +%s)" -lt "$deadline" ] \
            || die "pods still marked after ${MARK_WAIT}s, so the block stays in shadow: $(printf '%s' "$MARKED" | tr '\n' ' '). A mark written by another controller instance is not cleared by this one."
        sleep 10
        read_marks
    done
    ok "no pod is marked for a transfer"
    set_mode off
    info "today's optimizer runs again; the limiter is untouched"
}

usage() { sed -n '2,34p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' >&2; exit 2; }

main() {
    local verb="${1:-}"
    [ $# -gt 0 ] && shift
    NS="" CONTROLLER_NS="" POLICY_NS="" PHYSICAL=0 RESTART=0 GRANT_SHARED=0
    while [ $# -gt 0 ]; do
        case "$1" in
            --grant-shared) GRANT_SHARED=1; shift ;;
            -n|--namespace) NS="${2:-}"; shift 2 ;;
            --controller-namespace) CONTROLLER_NS="${2:-}"; shift 2 ;;
            --policy-namespace) POLICY_NS="${2:-}"; shift 2 ;;
            --physical-groups) PHYSICAL=1; shift ;;
            --restart) RESTART=1; shift ;;
            -h|--help) usage ;;
            *) fail "unknown argument: $1"; usage ;;
        esac
    done
    if [ -z "$NS" ] && [ -n "$CONTROLLER_NS" ]; then
        # A controller that manages a list of namespaces names them itself.
        local managed
        managed="$(controller_managed_ns)"
        case "$managed" in
            *,*) NS="${managed%%,*}"
                 info "the controller in $CONTROLLER_NS manages $(printf '%s' "$managed" | sed 's/,/, /g')" ;;
            *) die "the controller in $CONTROLLER_NS manages $managed, not a list: set the models' namespace with -n" ;;
        esac
    fi
    [ -n "$NS" ] || { fail "set the models' namespace with -n"; usage; }
    CONTROLLER_NS="${CONTROLLER_NS:-$NS}"
    [ "$GRANT_SHARED" -eq 0 ] || [ "$verb" = activate ] || die "--grant-shared is an activate option"
    case "$verb" in
        check|shadow|activate|status|off) resolve_policy_ns ;;
        *) usage ;;
    esac
    "verb_$verb"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    main "$@"
fi
