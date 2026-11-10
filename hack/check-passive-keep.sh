#!/usr/bin/env bash
#
# Executes the installer's passive-mode keep against a stub kubectl and asserts
# that passive mode is rendered INTO the apply -- LLMSM_PASSIVE in the manager
# ConfigMap, `passive` on the scaling-policy default entry, and the operator's
# other policy entries -- so no applied state is ever active; that a false is
# kept as false (yq's `//` reads false as absent); that a render or a read that
# fails stops the install rather than dropping passive mode; that a
# controller that read another LLMSM_PASSIVE at start is restarted; and that a
# re-install keeps the keys of the live default entry it does not render (the
# utilization-share optimizer block), writes the ones it does, and reports a
# rendered key the live entry had set otherwise.
#
# It exists because the first version restored all of it AFTER `kubectl apply
# -k`: a controller pod that started in between ran active, and a
# `passive: false` read through `// ""` came back empty and was dropped. Both
# parse, and both install green.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FAIL=0
PASS=0

for tool in yq jq; do
    command -v "$tool" >/dev/null 2>&1 || {
        echo "FATAL: $tool is required by this check, and is missing. Nothing below would be about the code."
        exit 2
    }
done

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
for lib in common.sh infra_scaling_manager.sh; do
    tr -d '\r' < "$ROOT/deploy/lib/$lib" > "$WORK/$lib"
done
BLUE=''; GREEN=''; YELLOW=''; RED=''; NC=''
# shellcheck source=/dev/null
. "$WORK/common.sh"
# shellcheck source=/dev/null
. "$WORK/infra_scaling_manager.sh"

for fn in llmsm_live_passive llmsm_live_get llmsm_passive_want llmsm_scale_to_zero_want llmsm_render_manager_config_patch \
          llmsm_render_policy_keep_patch llmsm_passive_restart_if_stale llmsm_yaml_scalar \
          llmsm_merge_live_default llmsm_warn_optimizer_without_limiters; do
    declare -F "$fn" >/dev/null || { echo "FATAL: $fn is not defined after sourcing the library."; exit 2; }
done

ok() { echo "ok   $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL $1"; FAIL=$((FAIL + 1)); }
# check <description> <command...>: ok when the command succeeds.
check() {
    local what="$1"
    shift
    if "$@"; then
        ok "$what"
    else
        fail "$what"
    fi
}
is() { [ "$1" = "$2" ]; }

# The live cluster, as the stub kubectl serves it. LIVE_READ is how a read of
# a ConfigMap goes: ok, notfound, forbidden or timeout. LIVE_POLICY_READ, when
# set, overrides it for the scaling-policy ConfigMap alone.
LIVE_MANAGER_CONFIG=''
LIVE_DEFAULT=''
LIVE_STATUS_PASSIVE=''
LIVE_READ=ok
LIVE_POLICY_READ=''
RESTARTED=0
RENDERED_POLICY='apiVersion: v1
kind: ConfigMap
metadata:
  name: scaling-manager-scaling-policy-config
data:
  default: |
    analyzers:
      - name: saturation
        score: 1.0
    scaleUpThreshold: 0.85'
kubectl() {
    case "$*" in
        "get configmap scaling-manager-manager-config"* | "get configmap scaling-manager-scaling-policy-config"*)
            local how="$LIVE_READ"
            case "$*" in
                *scaling-manager-scaling-policy-config*) how="${LIVE_POLICY_READ:-$LIVE_READ}" ;;
            esac
            case "$how" in
                notfound) echo 'Error from server (NotFound): configmaps "x" not found' >&2; return 1 ;;
                forbidden) echo 'Error from server (Forbidden): configmaps "x" is forbidden' >&2; return 1 ;;
                timeout) echo 'Unable to connect to the server: dial tcp 10.0.0.1:6443: i/o timeout' >&2; return 1 ;;
            esac
            case "$*" in
                *scaling-manager-manager-config*) printf '%s' "$LIVE_MANAGER_CONFIG" ;;
                *) printf '%s' "$LIVE_DEFAULT" ;;
            esac ;;
        "get configmap scaling-manager-controller-status"*) printf '%s' "$LIVE_STATUS_PASSIVE" ;;
        "kustomize "*) printf '%s\n' "$RENDERED_POLICY" ;;
        "rollout restart"*) RESTARTED=$((RESTARTED + 1)) ;;
        *) echo "unexpected kubectl $*" >&2; return 1 ;;
    esac
}

# 1. A false is read as false, not as absent; absent and NotFound read as
#    empty; Forbidden is a failure, not an absence.
LIVE_MANAGER_CONFIG='GLOBAL_OPT_INTERVAL: "15s"
LLMSM_PASSIVE: "false"'
LIVE_DEFAULT='passive: false
scaleUpThreshold: 0.85'
llmsm_live_passive scaling-manager
check "LLMSM_PASSIVE: \"false\" reads as false" is "$LLMSM_LIVE_PASSIVE" false
check "passive: false reads as false" is "$LLMSM_LIVE_DEFAULT_PASSIVE" false
LIVE_MANAGER_CONFIG='GLOBAL_OPT_INTERVAL: "15s"'
LIVE_DEFAULT='scaleUpThreshold: 0.85'
llmsm_live_passive scaling-manager
check "absent keys read as empty" is "$LLMSM_LIVE_PASSIVE$LLMSM_LIVE_DEFAULT_PASSIVE" ""
LIVE_READ=notfound
if llmsm_live_passive scaling-manager && is "$LLMSM_LIVE_PASSIVE" ""; then
    ok "a ConfigMap that does not exist reads as empty"
else
    fail "a NotFound read failed or read '$LLMSM_LIVE_PASSIVE'"
fi
LIVE_READ=forbidden
if llmsm_live_passive scaling-manager 2>/dev/null; then
    fail "a Forbidden read was taken as absent"
else
    ok "a Forbidden read is a failure"
fi
LIVE_READ=ok

# 2. The install variable wins over the live value; anything else is refused.
LLMSM_LIVE_PASSIVE=true
unset LLMSM_PASSIVE
check "unset LLMSM_PASSIVE keeps the live true" is "$(llmsm_passive_want)" true
check "LLMSM_PASSIVE=false overrides the live true" is "$(LLMSM_PASSIVE=false llmsm_passive_want)" false
if (LLMSM_PASSIVE=yes llmsm_passive_want >/dev/null 2>&1); then
    fail "LLMSM_PASSIVE=yes accepted"
else
    ok "LLMSM_PASSIVE=yes refused"
fi

# 3. LLMSM_PASSIVE is rendered into the manager ConfigMap the apply carries,
#    with or without a Prometheus URL to render; a render that fails with
#    passive mode to keep is an error.
OVERLAY="$WORK/overlay"
mkdir -p "$OVERLAY"
LLMSM_PROJECT="$ROOT"
unset PROMETHEUS_URL
LLMSM_LIVE_PASSIVE=true
: > "$OVERLAY/kustomization.yaml"
llmsm_render_manager_config_patch "$OVERLAY"
got="$(yq '.data."config.yaml"' "$OVERLAY/manager-config-patch.yaml" 2>/dev/null | yq '.LLMSM_PASSIVE')"
check "the rendered manager ConfigMap carries LLMSM_PASSIVE true" is "$got" true
check "the manager patch is in the kustomization" grep -q 'manager-config-patch.yaml' "$OVERLAY/kustomization.yaml"
BROKEN="$WORK/broken"
mkdir -p "$BROKEN/config/base/manager"
printf 'data: [unclosed\n' > "$BROKEN/config/base/manager/manager-configmap.yaml"
LLMSM_PROJECT="$BROKEN"
: > "$OVERLAY/kustomization.yaml"
if llmsm_render_manager_config_patch "$OVERLAY" 2>/dev/null; then
    fail "a render yq could not do succeeded with LLMSM_PASSIVE to keep"
else
    ok "a render yq cannot do fails with LLMSM_PASSIVE to keep"
fi
LLMSM_LIVE_PASSIVE=''
if llmsm_render_manager_config_patch "$OVERLAY" 2>/dev/null; then
    ok "with nothing to keep, an unrenderable manager ConfigMap is skipped as before"
else
    fail "with nothing to keep, the render failed the install"
fi
LLMSM_PROJECT="$ROOT"

# 4. The policy: the default entry rendered with the live passive, and the
#    operator's entries, all in the patch the apply carries.
: > "$OVERLAY/kustomization.yaml"
rm -f "$OVERLAY/policy-keep-patch.yaml"
LLMSM_LIVE_DEFAULT_PASSIVE=true
llmsm_render_policy_keep_patch "$OVERLAY" '{"data":{"chat.prod":"model_id: chat\nnamespace: prod\npassive: false\n"}}' >/dev/null
def="$(yq '.data.default' "$OVERLAY/policy-keep-patch.yaml" 2>/dev/null)"
check "the default entry carries passive: true" is "$(printf '%s\n' "$def" | yq '.passive')" true
check "the default entry keeps the rendered thresholds" is "$(printf '%s\n' "$def" | yq '.scaleUpThreshold')" 0.85
entry="$(yq '.data."chat.prod"' "$OVERLAY/policy-keep-patch.yaml" 2>/dev/null)"
check "the per-model entry with passive: false is in the apply" is "$(printf '%s\n' "$entry" | yq '.passive')" false
check "the policy patch is in the kustomization" grep -q 'policy-keep-patch.yaml' "$OVERLAY/kustomization.yaml"
: > "$OVERLAY/kustomization.yaml"
rm -f "$OVERLAY/policy-keep-patch.yaml"
LLMSM_LIVE_DEFAULT_PASSIVE=''
llmsm_render_policy_keep_patch "$OVERLAY" ''
if [ ! -e "$OVERLAY/policy-keep-patch.yaml" ] && ! grep -q policy-keep "$OVERLAY/kustomization.yaml"; then
    ok "nothing to keep writes no patch"
else
    fail "a patch was written with nothing to keep"
fi

# 5. A controller that read another LLMSM_PASSIVE at start is restarted; one
#    that read the same is not, nor one that published nothing.
LLMSM_LIVE_PASSIVE=true
LIVE_STATUS_PASSIVE=false; RESTARTED=0
llmsm_passive_restart_if_stale scaling-manager 2>/dev/null
check "a controller running with the other value is restarted" is "$RESTARTED" 1
LIVE_STATUS_PASSIVE=true; RESTARTED=0
llmsm_passive_restart_if_stale scaling-manager 2>/dev/null
check "a controller running with the same value is left alone" is "$RESTARTED" 0
LIVE_STATUS_PASSIVE=''; RESTARTED=0
llmsm_passive_restart_if_stale scaling-manager 2>/dev/null
check "a controller that published nothing is left to the rollout" is "$RESTARTED" 0

# 6. A re-install keeps the keys of the live default entry it does not render
#    -- the utilization-share optimizer block first among them -- writes the
#    keys it renders, and says which of those it changed. Before, the default
#    entry was the render alone, and a re-install turned sharing off.
#    The render here is the shipped ConfigMap, comments and all.
RENDERED_POLICY="$(tr -d '\r' < "$ROOT/config/base/manager/scaling-policy-configmap.yaml" \
    | yq '.metadata.name = "scaling-manager-scaling-policy-config"')"
SHIPPED_DEFAULT="$(printf '%s\n' "$RENDERED_POLICY" | yq '.data.default')"
SHARE_BLOCK='optimizer:
  type: utilizationShare
  utilizationShare:
    shadow: false'
# keep_run <live default> runs what the install runs before the apply, the way
# deploy_scaling_manager_controller does, and leaves the patch in $OVERLAY, the
# messages in $WORK/keep.err, and the render's exit status in KEEP_RC. The
# render runs in a subshell: a refusal is log_error, which exits.
KEEP_RC=0
keep_run() {
    LIVE_DEFAULT="$1"
    : > "$OVERLAY/kustomization.yaml"
    : > "$WORK/keep.err"
    rm -f "$OVERLAY/policy-keep-patch.yaml"
    KEEP_RC=0
    llmsm_live_passive scaling-manager 2>> "$WORK/keep.err" || { KEEP_RC=$?; return "$KEEP_RC"; }
    (llmsm_render_policy_keep_patch "$OVERLAY" '') 2>> "$WORK/keep.err" || KEEP_RC=$?
    return "$KEEP_RC"
}
kept() { yq '.data.default' "$OVERLAY/policy-keep-patch.yaml" 2>/dev/null | yq "$1"; }
no_patch() { [ ! -e "$OVERLAY/policy-keep-patch.yaml" ] && ! grep -q policy-keep "$OVERLAY/kustomization.yaml"; }
warned() { grep -q "^\[WARNING\] .*$1" "$WORK/keep.err"; }
SHIPPED_POLICY="$RENDERED_POLICY"

LIVE_READ=ok
LIVE_MANAGER_CONFIG='GLOBAL_OPT_INTERVAL: "15s"'
keep_run "$SHIPPED_DEFAULT
$SHARE_BLOCK"
check "re-install: the render succeeds" is "$KEEP_RC" 0
check "re-install: the optimizer block is kept" is "$(kept '.optimizer.type')" utilizationShare
check "re-install: the optimizer block keeps its mode" is "$(kept '.optimizer.utilizationShare.shadow')" false
check "re-install: the rendered thresholds are written" is "$(kept '.scaleUpThreshold')" 0.85
check "re-install: the analyzers are written" is "$(kept '.analyzers[0].name')" saturation
check "re-install: nothing it writes differed, so no conflict is reported" is "$(grep -c 'sets otherwise' "$WORK/keep.err")" 0
check "re-install: the kept keys are named" grep -q 'does not render: optimizer' "$WORK/keep.err"

keep_run "$(printf '%s\n' "$SHIPPED_DEFAULT" | yq '.scaleUpThreshold = 0.9 | .limiters = [{"type": "gpu-inventory"}] | .defaultPolicy = "batch"')
$SHARE_BLOCK"
check "owned key changed: the rendered value is written" is "$(kept '.scaleUpThreshold')" 0.85
check "owned key changed: the conflict is a warning" warned 'live entry sets otherwise'
check "owned key changed: the conflict names both values" grep -q '^    scaleUpThreshold: 0.9 -> 0.85$' "$WORK/keep.err"
check "owned key changed: the optimizer block is still kept" is "$(kept '.optimizer.type')" utilizationShare
check "an operator key beside it is kept" is "$(kept '.defaultPolicy')" batch
check "LLMSM_LIMITER unset: the live limiters are carried in the apply" is "$(kept '.limiters[0].type')" gpu-inventory
LIVE_WITH_LIMITER="$(printf '%s
' "$SHIPPED_DEFAULT" | yq '.limiters = [{"type": "gpu-inventory"}]')
$SHARE_BLOCK"
for lim in none quota; do
    LLMSM_LIMITER="$lim" keep_run "$LIVE_WITH_LIMITER"
    check "LLMSM_LIMITER=$lim: the live limiters are not carried" is "$KEEP_RC $(kept 'has("limiters")') $(kept '.optimizer.type')" "0 false utilizationShare"
done

# The conflict advice fits the key: enableRescale is read only from default,
# so a named policy cannot hold it.
keep_run "$(printf '%s
' "$SHIPPED_DEFAULT" | yq '.enableRescale = true')"
check "enableRescale conflict: says it is read only from default" grep -q 'enableRescale is read only from the default entry' "$WORK/keep.err"
check "enableRescale conflict alone: no named-policy advice" is "$(grep -c 'named policy' "$WORK/keep.err")" 0
keep_run "$(printf '%s
' "$SHIPPED_DEFAULT" | yq '.scaleUpThreshold = 0.9')"
check "threshold conflict: named-policy advice, no enableRescale line" is "$(grep -c 'named policy' "$WORK/keep.err") $(grep -c 'enableRescale is read only' "$WORK/keep.err")" "1 0"

# LLMSM_LIMITER=none with an optimizer block: warned, in its mode, with the way off.
LLMSM_NS=scaling-manager
for case_ in "false active" "true shadow"; do
    LLMSM_LIVE_DEFAULT="$(printf '%s
' "$LIVE_WITH_LIMITER" | yq ".optimizer.utilizationShare.shadow = ${case_% *}")"
    llmsm_warn_optimizer_without_limiters 2> "$WORK/none.err"
    check "LLMSM_LIMITER=none, block ${case_#* }: warned in its mode, with utilization-share-off" \
        grep -qE "^\[WARNING\] .*optimizer block \(${case_#* }\).*make utilization-share-off" "$WORK/none.err"
done
grep -A8 '^        none)$' "$WORK/infra_scaling_manager.sh" | grep -q '^ *llmsm_warn_optimizer_without_limiters$' \
    && ok "the LLMSM_LIMITER=none branch calls the optimizer warning" \
    || fail "the LLMSM_LIMITER=none branch no longer calls llmsm_warn_optimizer_without_limiters"
LLMSM_LIVE_DEFAULT="$SHIPPED_DEFAULT"
llmsm_warn_optimizer_without_limiters 2> "$WORK/none.err"
check "LLMSM_LIMITER=none, no optimizer block: no warning" is "$(wc -c < "$WORK/none.err" | tr -d ' ')" 0

# A list the installer owns, changed in the live entry: reported by name, and
# the rendered list written whole.
keep_run "$(printf '%s\n' "$SHIPPED_DEFAULT" | yq '.analyzers += [{"name": "slo", "score": 0.5}]')
$SHARE_BLOCK"
check "a changed analyzers list is a warned conflict" grep -q '^    analyzers: ' "$WORK/keep.err"
check "a changed analyzers list: the rendered list is written" is "$(kept '.analyzers | length') $(kept '.analyzers[0].name')" "1 saturation"

keep_run "$SHIPPED_DEFAULT
passive: true
$SHARE_BLOCK"
check "passive and the optimizer block are kept together" is "$(kept '.passive') $(kept '.optimizer.type')" "true utilizationShare"

# LLMSM_PASSIVE is the manager ConfigMap's; either value leaves the block alone.
for want in true false; do
    LLMSM_PASSIVE="$want" keep_run "$SHIPPED_DEFAULT
$SHARE_BLOCK"
    check "LLMSM_PASSIVE=$want keeps the optimizer block" is "$KEEP_RC $(kept '.optimizer.utilizationShare.shadow')" "0 false"
done
unset LLMSM_PASSIVE

# A render that itself carries passive and limiters: those two are never
# reported as conflicts, and the live passive is the one applied.
RENDERED_POLICY="$(printf '%s\n' "$SHIPPED_POLICY" | yq '.data.default = (.data.default + "passive: false" + "\n" + "limiters: []" + "\n")')"
keep_run "$SHIPPED_DEFAULT
passive: true
limiters:
  - type: gpu-inventory
$SHARE_BLOCK"
check "rendered passive/limiters: no conflict is reported for them" is "$(grep -cE '^    (passive|limiters):' "$WORK/keep.err")" 0
check "rendered passive/limiters: the live passive is applied" is "$(kept '.passive')" true
check "rendered limiters, LLMSM_LIMITER unset: the live list replaces the rendered one" is "$(kept '.limiters | length') $(kept '.limiters[0].type')" "1 gpu-inventory"
RENDERED_POLICY="$SHIPPED_POLICY"

if keep_run "$SHIPPED_DEFAULT" && no_patch; then
    ok "a live entry holding only rendered keys writes no patch"
else
    fail "a live entry with nothing of its own wrote a patch, or failed (rc $KEEP_RC)"
fi

LIVE_READ=notfound
if keep_run '' && no_patch && [ -z "${LLMSM_LIVE_DEFAULT:-}" ]; then
    ok "first install (no ConfigMap): no patch, the apply creates it as rendered"
else
    fail "first install wrote a patch, failed (rc $KEEP_RC) or read a live default ('${LLMSM_LIVE_DEFAULT:-}')"
fi
LIVE_READ=ok

if keep_run '# nothing but a comment' && no_patch && [ ! -s "$WORK/keep.err" ]; then
    ok "a comment-only live entry has nothing to keep, and says nothing"
else
    fail "a comment-only live entry wrote a patch, failed (rc $KEEP_RC), or printed: $(cat "$WORK/keep.err")"
fi

if keep_run '- not
- a mapping' && no_patch && warned 'not a YAML mapping'; then
    ok "a live entry that is not a mapping is replaced, with a warning"
else
    fail "a live entry that is not a mapping was kept from, failed, or replaced without a warning"
fi

# 7. A policy that cannot be read is not a policy that does not exist: the
#    read fails, keeps nothing, and the install's call refuses -- it never goes
#    on to apply the rendered entry over what it could not see.
grep -q 'llmsm_live_passive "\$LLMSM_NS" || log_error' "$WORK/infra_scaling_manager.sh" \
    && ok "the install refuses when llmsm_live_passive fails" \
    || fail "the install no longer refuses on a failed llmsm_live_passive"
for how in forbidden timeout; do
    LIVE_POLICY_READ="$how"
    LLMSM_LIVE_DEFAULT=stale
    rc=0
    llmsm_live_passive scaling-manager 2>/dev/null || rc=$?
    check "policy read $how (manager read ok): the read fails" is "$([ "$rc" -ne 0 ] && echo failed)" failed
    check "policy read $how: no live default is taken" is "$LLMSM_LIVE_DEFAULT" ""
    rc=0
    (llmsm_live_passive scaling-manager || log_error "refusing") >/dev/null 2>&1 || rc=$?
    check "policy read $how: the install refuses" is "$rc" 1
done
LIVE_POLICY_READ=''

# 8. A render or a merge that fails, with something to keep, is a refusal and
#    writes no patch: the apply would otherwise reset what it could not keep.
for render in empty null; do
    case "$render" in
        empty) RENDERED_POLICY='' ;;
        null) RENDERED_POLICY="$(printf '%s\n' "$SHIPPED_POLICY" | yq 'del(.data.default)')" ;;
    esac
    keep_run "$SHARE_BLOCK"
    check "render $render, live optimizer block: refused, no patch" is "$KEEP_RC $(no_patch && echo none)" "1 none"
    keep_run 'passive: true'
    check "render $render, live passive only: refused, no patch" is "$KEEP_RC $(no_patch && echo none)" "1 none"
done
RENDERED_POLICY="$SHIPPED_POLICY"

# The merge's last step is yq with LLMSM_KEEP_KEYS set; make that one fail.
yq() {
    if [ -n "${FAIL_MERGE:-}" ] && [ -n "${LLMSM_KEEP_KEYS:-}" ]; then
        return 1
    fi
    command yq "$@"
}
FAIL_MERGE=1 keep_run "$SHIPPED_DEFAULT
$SHARE_BLOCK"
check "a merge that fails: refused, no patch" is "$KEEP_RC $(no_patch && echo none)" "1 none"
check "a merge that fails: the error names the step" grep -q '^\[ERROR\] could not merge the live default entry' "$WORK/keep.err"
unset -f yq

# 7. Scale-to-zero has one default, false, and a re-install keeps the live
#    value. deploy/install.sh defaulted ENABLE_SCALE_TO_ZERO to true while the
#    controller, the shipped ConfigMap, install-epp.sh and the e2e Makefile all
#    said false; and because the apply re-renders config.yaml from the shipped
#    file, a re-install with false reset a live true without a word.
LIVE_READ=ok
LIVE_POLICY_READ=''
LIVE_DEFAULT=''
LIVE_MANAGER_CONFIG='GLOBAL_OPT_INTERVAL: "15s"
LLMSM_SCALE_TO_ZERO: "true"'
llmsm_live_passive scaling-manager
check "scale-to-zero: the live LLMSM_SCALE_TO_ZERO is read" is "$LLMSM_LIVE_SCALE_TO_ZERO" true
unset ENABLE_SCALE_TO_ZERO LLMSM_PASSIVE PROMETHEUS_URL
LLMSM_LIVE_PASSIVE=''
check "scale-to-zero: unset ENABLE_SCALE_TO_ZERO keeps the live true" is "$(llmsm_scale_to_zero_want)" true
check "scale-to-zero: ENABLE_SCALE_TO_ZERO=false overrides the live true" \
    is "$(ENABLE_SCALE_TO_ZERO=false llmsm_scale_to_zero_want)" false
if (ENABLE_SCALE_TO_ZERO=yes llmsm_scale_to_zero_want >/dev/null 2>&1); then
    fail "scale-to-zero: ENABLE_SCALE_TO_ZERO=yes accepted"
else
    ok "scale-to-zero: ENABLE_SCALE_TO_ZERO=yes refused"
fi
: > "$OVERLAY/kustomization.yaml"
rm -f "$OVERLAY/manager-config-patch.yaml"
llmsm_render_manager_config_patch "$OVERLAY"
got="$(yq '.data."config.yaml"' "$OVERLAY/manager-config-patch.yaml" 2>/dev/null | yq '.LLMSM_SCALE_TO_ZERO')"
check "scale-to-zero: the live true is rendered into the apply, with nothing else to render" is "$got" true
LIVE_MANAGER_CONFIG='GLOBAL_OPT_INTERVAL: "15s"'
llmsm_live_passive scaling-manager
check "scale-to-zero: a first install writes nothing over the shipped value" is "$(llmsm_scale_to_zero_want)" ""
shipped="$(yq '.data."config.yaml"' "$ROOT/config/base/manager/manager-configmap.yaml" | yq '.LLMSM_SCALE_TO_ZERO')"
ocp="$(yq '.data."config.yaml"' "$ROOT/config/components/openshift/configmap-patch.yaml" | yq '.LLMSM_SCALE_TO_ZERO')"
check "scale-to-zero: the shipped ConfigMaps say false, as the controller does" is "$shipped $ocp" "false false"
check "scale-to-zero: install.sh and install-epp.sh give no default but false" is \
    "$(grep -hE '^ENABLE_SCALE_TO_ZERO=' "$ROOT/deploy/install.sh" "$ROOT/deploy/install-epp.sh" | tr '\n' ' ')" \
    'ENABLE_SCALE_TO_ZERO=${ENABLE_SCALE_TO_ZERO:-} ENABLE_SCALE_TO_ZERO=${ENABLE_SCALE_TO_ZERO:-false} '

EXPECT_CASES=75
echo
echo "passive keep ($(yq --version 2>&1)): $PASS passed, $FAIL failed"
if [ "$FAIL" -ne 0 ]; then
    exit 1
fi
if [ "$PASS" -ne "$EXPECT_CASES" ]; then
    echo "FAIL ran $PASS cases, expected $EXPECT_CASES: the run ended early"
    exit 1
fi
