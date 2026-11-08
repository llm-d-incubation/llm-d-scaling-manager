#!/usr/bin/env bash
#
# Executes policy_with_limiters() in deploy/lib/limiter_policy.sh and asserts
# that rebuilding a Kueue quota entry KEEPS the whole kueue block written by
# hand into the live one -- borrowNamespaces (the pilot list),
# assumeTestedVersion, controllerNamespace, the timings, refreshInterval,
# resources, releaseToNonReclaimingLenders -- and, unless LLMSM_QUOTA_KUEUE_BORROW is given, countBorrowable.
#
# It exists because `make deploy-scaling-manager LLMSM_QUOTA_KUEUE=true` with LLMSM_LIMITER set,
# and `make enable-physical-limiter LLMSM_LIMITER_TYPE=quota`, rebuilt the entry
# from install variables, and no variable carries borrowNamespaces: a re-run
# dropped the pilot list, and borrowing was counted for every namespace from
# then on. The first fix kept only borrowNamespaces and countBorrowable, so a
# re-run still dropped assumeTestedVersion (a digest-only Kueue image then
# stops borrowing) and every timing. The document stayed valid, so nothing
# failed.
#
# The installer composes AFTER its apply reset the policy, so it hands the
# limiters it read before the apply in LIMITERS_KEEP_FROM; the cluster-policy
# path composes over the live policy itself. Both are exercised.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FAIL=0
PASS=0

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
for lib in common.sh limiter_policy.sh; do
    tr -d '\r' < "$ROOT/deploy/lib/$lib" > "$WORK/$lib"
done
BLUE=''; GREEN=''; YELLOW=''; RED=''; NC=''
# shellcheck disable=SC1090
. "$WORK/common.sh"
unset LLMSM_QUOTAS LLMSM_QUOTA_SCOPE LLMSM_QUOTA_KUEUE LLMSM_QUOTA_KUEUE_BORROW LLMSM_SCOPE LLMSM_WATCH_NS LLMSM_LIMITER LLMSM_LIMITER_TYPE LIMITERS_KEEP_FROM
LLMSM_NS="scaling-manager-system"
# shellcheck disable=SC1090
. "$WORK/limiter_policy.sh"

for tool in yq jq; do
    command -v "$tool" >/dev/null 2>&1 || {
        echo "FATAL: $tool is required by this check, and is missing. Nothing below would be about the code."
        exit 2
    }
done

ok() { echo "ok   $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL $1"; FAIL=$((FAIL + 1)); }

# The live entry an operator piloted: borrowing for team-a only, and off.
OLD_ENTRY='- name: install-quota
  type: quota
  scope: namespace
  kueue:
    enabled: true
    countBorrowable: false
    borrowNamespaces: [team-a]
    assumeTestedVersion: true
    controllerNamespace: kueue-ops
    releaseToNonReclaimingLenders: true
    borrowCooldown: 20m
    placementTimeout: 9m
    lenderDebounce: 90s
    refreshInterval: 45s
    resources: [nvidia.com/gpu, example.com/gpu]
  namespaceQuotas:
    default:
      H100: -1'
LIVE_POLICY="scaleUpThreshold: 0.85
limiters:
$OLD_ENTRY"

# field <yaml> <path> prints a field of the first quota entry, or null.
field() {
    printf '%s\n' "$1" | yq "[.limiters[] | select(.type == \"quota\")] | .[0] | $2"
}

new_entry() {
    LLMSM_QUOTAS='H100=-1' LLMSM_QUOTA_KUEUE=true LLMSM_SCOPE=cluster limiter_set_yaml quota default
}

# 1. Cluster-policy path: composed over the live policy.
ENTRY="$(new_entry)" || { echo "FATAL: limiter_set_yaml failed"; exit 2; }
[ -n "$ENTRY" ] || { echo "FATAL: limiter_set_yaml emitted nothing; nothing below would be about the keep"; exit 2; }
OUT="$(policy_with_limiters "$LIVE_POLICY" "$ENTRY" compose)"
[ "$(field "$OUT" '.kueue.borrowNamespaces | join(",")')" = "team-a" ] \
    && ok "enable-physical-limiter keeps borrowNamespaces from the live entry" \
    || fail "enable-physical-limiter dropped borrowNamespaces: $(field "$OUT" '.kueue')"
[ "$(field "$OUT" '.kueue.countBorrowable')" = "false" ] \
    && ok "enable-physical-limiter keeps countBorrowable: false when LLMSM_QUOTA_KUEUE_BORROW is not given" \
    || fail "enable-physical-limiter dropped countBorrowable: $(field "$OUT" '.kueue')"
# R4: every hand-written field of the block, not only the pilot list.
for kv in 'assumeTestedVersion=true' 'controllerNamespace=kueue-ops' 'borrowCooldown=20m' \
    'placementTimeout=9m' 'lenderDebounce=90s' 'refreshInterval=45s' \
    'resources | join(",")=nvidia.com/gpu,example.com/gpu' 'enabled=true' \
    'releaseToNonReclaimingLenders=true'; do
    f="${kv%%=*}" want="${kv#*=}"
    [ "$(field "$OUT" ".kueue.$f")" = "$want" ] \
        && ok "enable-physical-limiter keeps kueue.$f" \
        || fail "enable-physical-limiter dropped kueue.$f: $(field "$OUT" '.kueue')"
done
[ "$(printf '%s\n' "$OUT" | yq '.limiters | length')" = "1" ] \
    && ok "one quota entry after the rebuild, not two" \
    || fail "the rebuild left $(printf '%s\n' "$OUT" | yq '.limiters | length') entries"
[ "$(printf '%s\n' "$OUT" | yq '.scaleUpThreshold')" = "0.85" ] \
    && ok "the rest of the policy is kept" \
    || fail "the rest of the policy was lost: $OUT"

# 2. An explicit LLMSM_QUOTA_KUEUE_BORROW decides countBorrowable; the pilot list
#    is still kept.
OUT="$(LLMSM_QUOTA_KUEUE_BORROW=true policy_with_limiters "$LIVE_POLICY" "$ENTRY" compose)"
[ "$(field "$OUT" '.kueue.countBorrowable')" = "null" ] \
    && ok "LLMSM_QUOTA_KUEUE_BORROW=true is not overridden by the live countBorrowable: false" \
    || fail "the live countBorrowable overrode LLMSM_QUOTA_KUEUE_BORROW=true: $(field "$OUT" '.kueue')"
[ "$(field "$OUT" '.kueue.borrowNamespaces | join(",")')" = "team-a" ] \
    && ok "borrowNamespaces is kept beside an explicit LLMSM_QUOTA_KUEUE_BORROW" \
    || fail "borrowNamespaces dropped beside LLMSM_QUOTA_KUEUE_BORROW: $(field "$OUT" '.kueue')"

# 3. Install path: the apply reset the policy; the limiters read before it come
#    in LIMITERS_KEEP_FROM, as JSON (llmsm_live_limiters).
RESET_POLICY='scaleUpThreshold: 0.85'
KEEP="$(printf '%s\n' "$OLD_ENTRY" | yq -o=json -I=0 '.')"
OUT="$(LIMITERS_KEEP_FROM="$KEEP" policy_with_limiters "$RESET_POLICY" "$ENTRY")"
[ "$(field "$OUT" '.kueue.borrowNamespaces | join(",")')" = "team-a" ] \
    && ok "deploy-scaling-manager keeps borrowNamespaces read before its apply reset the policy" \
    || fail "deploy-scaling-manager dropped borrowNamespaces: $(field "$OUT" '.kueue')"
[ "$(field "$OUT" '.kueue.countBorrowable')" = "false" ] \
    && ok "deploy-scaling-manager keeps countBorrowable read before its apply" \
    || fail "deploy-scaling-manager dropped countBorrowable: $(field "$OUT" '.kueue')"
for kv in 'assumeTestedVersion=true' 'controllerNamespace=kueue-ops' 'placementTimeout=9m' \
    'resources | join(",")=nvidia.com/gpu,example.com/gpu' 'releaseToNonReclaimingLenders=true'; do
    f="${kv%%=*}" want="${kv#*=}"
    [ "$(field "$OUT" ".kueue.$f")" = "$want" ] \
        && ok "deploy-scaling-manager keeps kueue.$f read before its apply" \
        || fail "deploy-scaling-manager dropped kueue.$f: $(field "$OUT" '.kueue')"
done

# 3b. What the install sets wins over what is kept: LLMSM_QUOTA_KUEUE_BORROW=false
#     over a live entry that counts borrowing writes false, and the old
#     enabled: false is overlaid by the enabled: true the install writes.
OFF_LIVE="$(printf '%s\n' "$LIVE_POLICY" | yq '.limiters[0].kueue.enabled = false | del(.limiters[0].kueue.countBorrowable)')"
OFF_ENTRY="$(LLMSM_QUOTA_KUEUE_BORROW=false LLMSM_QUOTAS='H100=-1' LLMSM_QUOTA_KUEUE=true LLMSM_SCOPE=cluster limiter_set_yaml quota default)"
OUT="$(LLMSM_QUOTA_KUEUE_BORROW=false policy_with_limiters "$OFF_LIVE" "$OFF_ENTRY" compose)"
[ "$(field "$OUT" '.kueue.countBorrowable')" = "false" ] && [ "$(field "$OUT" '.kueue.enabled')" = "true" ] \
    && [ "$(field "$OUT" '.kueue.placementTimeout')" = "9m" ] \
    && ok "the install variables overlay the kept block: countBorrowable false, enabled true, the rest kept" \
    || fail "the install variables did not overlay the kept block: $(field "$OUT" '.kueue')"

# 4. Another name, the only Kueue quota entry: still the one replaced.
RENAMED="$(printf '%s\n' "$LIVE_POLICY" | yq '.limiters[0].name = "tenant-quota"')"
OUT="$(policy_with_limiters "$RENAMED" "$ENTRY" compose)"
[ "$(field "$OUT" '.kueue.borrowNamespaces | join(",")')" = "team-a" ] \
    && [ "$(field "$OUT" '.kueue.assumeTestedVersion')" = "true" ] \
    && ok "a renamed single Kueue entry is kept from" \
    || fail "a renamed single Kueue entry was not kept from: $(field "$OUT" '.kueue')"

# Controls: nothing is added where there is nothing to keep, or no kueue block.
OUT="$(policy_with_limiters "$RESET_POLICY" "$ENTRY")"
[ "$(field "$OUT" '.kueue.borrowNamespaces')" = "null" ] && [ "$(field "$OUT" '.kueue.enabled')" = "true" ] \
    && ok "control: no live entry, the new entry as built and no borrowNamespaces invented" \
    || fail "borrowNamespaces invented from nothing: $(field "$OUT" '.kueue')"
PLAIN="$(LLMSM_QUOTAS='H100=8' LLMSM_SCOPE=cluster limiter_set_yaml quota default)"
OUT="$(policy_with_limiters "$LIVE_POLICY" "$PLAIN" compose)"
[ "$(field "$OUT" '.kueue')" = "null" ] \
    && ok "control: an entry without a kueue block gets none" \
    || fail "a kueue block was added to an entry without one: $(field "$OUT" '.kueue')"

echo "check-kueue-borrow-keep: $PASS passed, $FAIL failed"
# A run that ended early passes nothing it skipped: count the cases.
RAN=$((PASS + FAIL))
[ "$RAN" -eq 26 ] || { echo "FAIL: $RAN cases ran, not 26. Update the count deliberately."; exit 1; }
[ "$FAIL" -eq 0 ]
