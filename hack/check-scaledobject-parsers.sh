#!/usr/bin/env bash
#
# Executes the model-identification path in deploy/lib/scaledobject.sh against
# fixture command lines and pod specs.
#
# Sibling of check-workload-gaps.sh, and it exists for the same reason: these
# parsers decide which model every ScaledObject is written for, every defect
# they have shipped parses cleanly, and each was found on a cluster one install
# at a time -- `python` answered as the model of an SGLang server, a CRLF that
# matched no metric series, a --served-model-name read as the literal
# "$MODEL_NAME". None is a syntax error and all of them silently point the scaling manager at a
# model no metric series reports.
#
# WRITING A CASE HERE: an expected-EMPTY case proves nothing on its own -- a
# function that does not exist returns empty too. The guard below refuses to run
# unless both functions are defined, and EXPECT_CASES refuses to pass a run that
# ended early; between them an empty answer means the function decided on it.
#
# The last group is not a string test. It lifts the discovery jq query out of
# the library and runs it against a pod fixture, because the record that query
# builds is pipe-delimited and newline-framed, and the two ways to break that
# framing -- a field holding the delimiter, a field holding a newline -- are
# invisible from the shell functions alone. Lifted rather than copied, so it
# cannot pass against a stale duplicate of the query.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# jq is not optional: the record group below cannot run without it, and a group
# that skips is how a green run comes to mean nothing.
command -v jq >/dev/null 2>&1 || {
    printf 'FATAL: jq is required to run these checks.\n' >&2
    exit 1
}

# Sourced through a CR-stripped copy, for the reason check-workload-gaps.sh
# gives: a Windows checkout with core.autocrlf makes bash stop at the first
# function definition, which looks exactly like every function being missing.
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
for lib in common.sh scaler_tls.sh scaledobject.sh; do
    tr -d '\r' < "$ROOT/deploy/lib/$lib" > "$WORK/$lib"
done
# common.sh requires these; without them the first log_warning dies under set -u.
BLUE='' GREEN='' YELLOW='' RED='' NC=''
# shellcheck source=/dev/null
source "$WORK/common.sh"
# shellcheck source=/dev/null
source "$WORK/scaledobject.sh"

for fn in so_model_id so_resolve_env_ref; do
    declare -F "$fn" >/dev/null || {
        printf 'FATAL: %s is not defined after sourcing the library.\n' "$fn" >&2
        exit 1
    }
done

EXPECT_CASES=125
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
# so_model_id: which token on a command line is the model
# ----------------------------------------------------------------------------
check 'so_model_id reads --served-model-name <v>' \
    'Qwen/Qwen3-0.6B' \
    "$(so_model_id 'vllm serve /model-cache/x --served-model-name Qwen/Qwen3-0.6B --port 8000')"

check 'so_model_id reads --served-model-name=<v>' \
    'Qwen/Qwen3-0.6B' \
    "$(so_model_id 'vllm serve --served-model-name=Qwen/Qwen3-0.6B')"

check 'so_model_id prefers the served name over --model' \
    'Qwen/Qwen3-0.6B' \
    "$(so_model_id '--model /model-cache/models/Qwen/Qwen3-0.6B --served-model-name Qwen/Qwen3-0.6B')"

check 'so_model_id falls back to --model' \
    '/model-cache/models/Qwen' \
    "$(so_model_id 'vllm serve --model /model-cache/models/Qwen --port 8000')"

# The llm-d flagship-guide shape: command ["vllm","serve"], model first in args.
check 'so_model_id reads the positional model after serve' \
    'Qwen/Qwen3-32B' \
    "$(so_model_id 'vllm serve Qwen/Qwen3-32B --tensor-parallel-size=2')"

# No `serve` to anchor on. Empty is the answer; the bare "first non-flag token"
# fallback that used to live here returned `python`.
check 'so_model_id answers nothing for an sglang launcher' \
    '' \
    "$(so_model_id 'python -m sglang.launch_server --model-path /model-cache/x')"

check 'so_model_id answers nothing for an empty command line' \
    '' \
    "$(so_model_id '')"

# ----------------------------------------------------------------------------
# so_resolve_env_ref: a model name that is a variable reference
# ----------------------------------------------------------------------------
ENVS='MODEL_PATH=/model-cache/qwen BASE=Qwen/Qwen3 MODEL_NAME=Qwen/Qwen3-0.6B'

check 'so_resolve_env_ref passes a real model name through' \
    'Qwen/Qwen3-0.6B' \
    "$(so_resolve_env_ref 'Qwen/Qwen3-0.6B' "$ENVS")"

# The shell-script spelling: customCommand runs under bash inside the container.
check 'so_resolve_env_ref answers $VARNAME' \
    'Qwen/Qwen3-0.6B' \
    "$(so_resolve_env_ref '$MODEL_NAME' "$ENVS")"

check 'so_resolve_env_ref answers ${VARNAME}' \
    'Qwen/Qwen3-0.6B' \
    "$(so_resolve_env_ref '${MODEL_NAME}' "$ENVS")"

# The Kubernetes spelling: expanded by the kubelet, so the pod spec keeps the
# literal. config/scenarios/cicd/kind.yaml and examples/sim.yaml both use it.
check 'so_resolve_env_ref answers $(VARNAME)' \
    'Qwen/Qwen3-0.6B' \
    "$(so_resolve_env_ref '$(MODEL_NAME)' "$ENVS")"

check 'so_resolve_env_ref drops an unset reference rather than echoing it raw' \
    '' \
    "$(so_resolve_env_ref '$NOT_SET_HERE' "$ENVS")"

# Compound tokens resolve to nothing on purpose: half a name is a modelID that
# looks right and matches no metric series.
check 'so_resolve_env_ref drops ${VAR}suffix' \
    '' \
    "$(so_resolve_env_ref '${BASE}-instruct' "$ENVS")"

check 'so_resolve_env_ref drops $(VAR)suffix' \
    '' \
    "$(so_resolve_env_ref '$(MODEL_NAME)x' "$ENVS")"

check 'so_resolve_env_ref drops an unclosed brace' \
    '' \
    "$(so_resolve_env_ref '${MODEL_NAME' "$ENVS")"

check 'so_resolve_env_ref drops a bare $' \
    '' \
    "$(so_resolve_env_ref '$' "$ENVS")"

# printf, not echo: echo would swallow this as an option and answer empty.
check 'so_resolve_env_ref keeps a token that begins with a dash' \
    '-n' \
    "$(so_resolve_env_ref '-n' "$ENVS")"

check 'so_resolve_env_ref answers nothing with no env list at all' \
    '' \
    "$(so_resolve_env_ref '$MODEL_NAME' '')"

# ----------------------------------------------------------------------------
# The discovery record: one workload per LINE, fields split on a pipe
# ----------------------------------------------------------------------------
# The program is the last one closed by the `] | join("|"))` that ends the
# record. Extraction failing is a FAILURE, not a skip.
prog="$(awk '
    /jq -r --argjson p/            { buf = ""; collecting = 1; next }
    collecting && /join\("\|"\)\)/ { buf = buf $0; print buf; exit }
    collecting                     { buf = buf $0 "\n" }
' "$WORK/scaledobject.sh")"
case "$prog" in
    *"')") prog="${prog%??}" ;;   # the shell quote and paren closing the call
    *)     prog="" ;;
esac

if [ -z "$prog" ]; then
    FAILED=$((FAILED + 1))
    printf 'FAIL  could not lift the discovery jq program out of the library\n' >&2
else
    # BLOCK is what a YAML block scalar (`value: |`) writes: a value with a
    # newline in it. Left in the record it splits this one workload across two
    # lines -- the real entry losing the args field that trails it, and with it
    # the model it is being read for, plus a phantom entry named after the tail.
    # SPACED would split the env list itself, PIPED the record fields. The proxy
    # container is first on purpose: the engine is container 1.
    fixture="$WORK/pods.json"
    cat > "$fixture" <<'JSON'
{"items":[{"metadata":{"name":"decode","labels":{}},"spec":{"template":{"metadata":{"labels":{"app":"d"}},"spec":{"containers":[
  {"name":"proxy","image":"routing-proxy","args":[]},
  {"name":"vllm","image":"vllm/vllm-openai",
   "env":[{"name":"BLOCK","value":"/a\nb"},
          {"name":"SPACED","value":"a b"},
          {"name":"PIPED","value":"a|b"},
          {"name":"FROM_SECRET"},
          {"name":"MODEL_NAME","value":"Qwen/Qwen3-0.6B"}],
   "command":["vllm","serve"],
   "args":["--served-model-name","$(MODEL_NAME)"]}]}}}}]}
JSON
    out="$(jq -r --argjson p '["spec","template"]' "$prog" "$fixture" 2>&1)"

    check 'discovery emits one line per workload' \
        '1' \
        "$(printf '%s\n' "$out" | wc -l | tr -d ' ')"

    IFS='|' read -r r_name r_labels _r_objlabels r_envs r_args <<<"$out"
    check 'discovery names the workload' 'decode' "$r_name"
    check 'discovery reads the pod labels' 'app=d' "$r_labels"
    check 'discovery keeps only single-token env values' \
        'MODEL_NAME=Qwen/Qwen3-0.6B' "$r_envs"
    check 'discovery reads args off the ENGINE container' \
        'vllm serve --served-model-name $(MODEL_NAME)' "$r_args"

    # What the whole chain is for.
    check 'discovery resolves to a usable modelID' \
        'Qwen/Qwen3-0.6B' \
        "$(so_resolve_env_ref "$(so_model_id "$r_args")" "$r_envs")"
fi

# ----------------------------------------------------------------------------
# so_pause_target_gpus: GPUs one replica of a scale target holds
# ----------------------------------------------------------------------------
# The function reads the target with kubectl; a stub serves the fixture. The
# LWS cases pin the controller's arithmetic (GetTotalGPUsPerReplica): the leader
# runs the workerTemplate when there is no leaderTemplate.
declare -F so_pause_target_gpus >/dev/null || {
    printf 'FATAL: so_pause_target_gpus is not defined after sourcing the library.
' >&2
    exit 1
}
SO_FIXTURE=''
kubectl() { printf '%s' "$SO_FIXTURE"; }
gpus_of() { SO_FIXTURE="$1"; so_pause_target_gpus ns kind name; }
c() { printf '{"name":"c","resources":{"requests":{"%s":"%s"}}}' "$1" "$2"; }

check 'pause GPUs: a Deployment sums its containers' '3'     "$(gpus_of "{\"spec\":{\"template\":{\"spec\":{\"containers\":[$(c nvidia.com/gpu 2),$(c nvidia.com/gpu 1)]}}}}")"
check 'pause GPUs: an LWS without a leaderTemplate runs the worker as leader' '8'     "$(gpus_of "{\"spec\":{\"leaderWorkerTemplate\":{\"size\":2,\"workerTemplate\":{\"spec\":{\"containers\":[$(c amd.com/gpu 4)]}}}}}")"
check 'pause GPUs: an LWS with a leaderTemplate counts it once' '10'     "$(gpus_of "{\"spec\":{\"leaderWorkerTemplate\":{\"size\":3,\"leaderTemplate\":{\"spec\":{\"containers\":[$(c nvidia.com/gpu 2)]}},\"workerTemplate\":{\"spec\":{\"containers\":[$(c nvidia.com/gpu 4)]}}}}}")"
check 'pause GPUs: an LWS of size 1 without a leaderTemplate is one worker' '4'     "$(gpus_of "{\"spec\":{\"leaderWorkerTemplate\":{\"workerTemplate\":{\"spec\":{\"containers\":[$(c nvidia.com/gpu 4)]}}}}}")"
check 'pause GPUs: no request reads as one GPU, as the controller defaults' '1'     "$(gpus_of '{"spec":{"template":{"spec":{"containers":[{"name":"c"}]}}}}')"
unset -f kubectl

# ----------------------------------------------------------------------------
# The KEDA fallback on generated and adopted ScaledObjects
# ----------------------------------------------------------------------------
# Read back through yq, not grepped: the block is spliced into a heredoc, and
# the failure that matters is one that still prints the words but at the wrong
# indent -- a fallback under advanced, or an advanced block that lost its key.
command -v yq >/dev/null 2>&1 || {
    printf 'FATAL: yq is required for the fallback group.\n' >&2
    exit 1
}
for fn in render_default_scaledobject render_scaledobject so_adopt_patch so_adopt_needs_fallback \
          so_fallback_check so_fallback_failures so_keda_fallback_probe so_fallback_note so_show_plan \
          so_adopt_fallback_note so_value_trigger_warning; do
    declare -F "$fn" >/dev/null || {
        printf 'FATAL: %s is not defined after sourcing the library.\n' "$fn" >&2
        exit 1
    }
done
# Every case below runs against a KEDA that has fallback.behavior unless it says
# otherwise. Set, not probed: the probe would call the real kubectl.
SO_KEDA_FALLBACK_BEHAVIOR=yes
render_with() {   # render_with <min> -- with whatever LLMSM_SO_FALLBACK* is set
    render_default_scaledobject ns Deployment decode Qwen/Qwen3-0.6B addr:9090 "$1" 8 '' ''
}
fb_of() { yq -o=json '.spec.fallback' | jq -r 'if . == null then "none" else [.failureThreshold, .replicas, .behavior] | map(tostring) | join(",") end'; }

check 'fallback: a generated object gets one, replicas at its minReplicaCount' \
    '3,2,currentReplicasIfHigher' "$(render_with 2 | fb_of)"
check 'fallback: the rest of the spec survives the splice' \
    '2,8,true,300' \
    "$(render_with 2 | yq -o=json '.spec' | jq -r '[.minReplicaCount, .maxReplicaCount, .advanced.restoreToOriginalReplicaCount, .advanced.horizontalPodAutoscalerConfig.behavior.scaleDown.stabilizationWindowSeconds] | map(tostring) | join(",")')"
# A floor of 0 gets NO fallback. KEDA sends a trigger error on an object with
# fallback.replicas 0 and minReplicaCount 0 to scaleToZeroOrIdle, so a zero
# fallback parks a serving model on any error window.
check 'fallback: a floor of zero gets none' \
    'none' "$(render_with 0 | fb_of)"
check 'fallback: a floor written 00 is still zero' \
    'none' "$(render_with 00 | fb_of)"
check 'fallback: a floor of zero still renders a valid object' \
    '0,8,true' \
    "$(render_with 0 | yq -o=json '.spec' | jq -r '[.minReplicaCount, .maxReplicaCount, .advanced.restoreToOriginalReplicaCount] | map(tostring) | join(",")')"
check 'fallback: LLMSM_SO_FALLBACK_FAILURES sets failureThreshold' \
    '5,1,currentReplicasIfHigher' "$(LLMSM_SO_FALLBACK_FAILURES=5 render_with 1 | fb_of)"
check 'fallback: LLMSM_SO_FALLBACK=false leaves it out' \
    'none' "$(LLMSM_SO_FALLBACK=false render_with 1 | fb_of)"
check 'fallback: LLMSM_SO_FALLBACK=false still renders the advanced block' \
    'true' "$(LLMSM_SO_FALLBACK=false render_with 1 | yq -r '.spec.advanced.restoreToOriginalReplicaCount')"
check 'fallback: LLMSM_SO_FALLBACK must be true or false' \
    'refused' "$( (LLMSM_SO_FALLBACK=yes so_fallback_check) 2>/dev/null && echo accepted || echo refused)"
check 'fallback: a failureThreshold of 0 is refused' \
    'refused' "$( (LLMSM_SO_FALLBACK_FAILURES=0 so_fallback_failures) > /dev/null 2>&1 && echo accepted || echo refused)"
check 'fallback: a failureThreshold with a leading zero is refused' \
    'refused' "$( (LLMSM_SO_FALLBACK_FAILURES=010 so_fallback_failures) > /dev/null 2>&1 && echo accepted || echo refused)"
check 'fallback: a failureThreshold of int32 max is accepted' \
    '2147483647' "$(LLMSM_SO_FALLBACK_FAILURES=2147483647 so_fallback_failures 2>/dev/null)"
check 'fallback: a failureThreshold above int32 is refused' \
    'refused' "$( (LLMSM_SO_FALLBACK_FAILURES=2147483648 so_fallback_failures) > /dev/null 2>&1 && echo accepted || echo refused)"
check 'fallback: a failureThreshold of eleven digits is refused' \
    'refused' "$( (LLMSM_SO_FALLBACK_FAILURES=99999999999 so_fallback_failures) > /dev/null 2>&1 && echo accepted || echo refused)"

# KEDA before 2.17 has no fallback.behavior: its CRD prunes the field and the
# fallback that is left is static at minReplicaCount. None at all instead.
check 'fallback: KEDA < 2.17 gets none' \
    'none' "$(SO_KEDA_FALLBACK_BEHAVIOR=no render_with 2 | fb_of)"
check 'fallback: an unreadable KEDA CRD gets none' \
    'none' "$(SO_KEDA_FALLBACK_BEHAVIOR=unknown render_with 2 | fb_of)"

# The probe, against a stub kubectl. Each in a subshell with the cache cleared,
# so the answer is the probe and not the value set above. The stub answers
# `kubectl explain` from EXPLAIN_* and everything else (the CRD read) from
# CRD_*. The explain texts are what kubectl 1.31 printed against an envtest API
# server serving the KEDA 2.18 CRD, with and without fallback.behavior.
EXPLAIN_OUT='error: unable to reach the server'
EXPLAIN_RC=1
CRD_FIXTURE=''
CRD_FIXTURE_RC=0
CRD_ERR=''
kubectl() {
    if [ "$1" = explain ]; then
        printf '%s\n' "$EXPLAIN_OUT"
        return "$EXPLAIN_RC"
    fi
    [ -z "$CRD_ERR" ] || printf '%s\n' "$CRD_ERR" >&2
    printf '%s' "$CRD_FIXTURE"
    return "$CRD_FIXTURE_RC"
}
probe() { ( unset SO_KEDA_FALLBACK_BEHAVIOR; so_keda_fallback_probe 2>/dev/null; printf '%s' "$SO_KEDA_FALLBACK_BEHAVIOR" ); }
probe_warning() { ( unset SO_KEDA_FALLBACK_BEHAVIOR; so_keda_fallback_probe 2>&1 >/dev/null ); }
FORBIDDEN='Error from server (Forbidden): customresourcedefinitions.apiextensions.k8s.io "scaledobjects.keda.sh" is forbidden: User "tenant" cannot get resource "customresourcedefinitions" in API group "apiextensions.k8s.io" at the cluster scope'

# OpenAPI first: any authenticated user may read it, where a tenant usually
# may not get the CRD -- so these answer with the CRD read Forbidden.
CRD_FIXTURE_RC=1
CRD_ERR="$FORBIDDEN"
EXPLAIN_OUT=$'GROUP:      keda.sh\nKIND:       ScaledObject\nVERSION:    v1alpha1\n\nFIELD: behavior <string>'
EXPLAIN_RC=0
check 'probe: the OpenAPI answers for a tenant that may not read the CRD' 'yes' "$(probe)"
EXPLAIN_OUT=$'GROUP:      keda.sh\nKIND:       ScaledObject\nVERSION:    v1alpha1\n\nerror: field "behavior" does not exist'
EXPLAIN_RC=1
check 'probe: an OpenAPI without fallback.behavior is older' 'no' "$(probe)"
EXPLAIN_OUT=$'error: field "fallback" does not exist'
check 'probe: an OpenAPI without fallback at all is older' 'no' "$(probe)"
EXPLAIN_OUT='error: unable to reach the server'
check 'probe: neither source readable is unknown' 'unknown' "$(probe)"
check 'probe: and the warning names Forbidden' \
    'named' "$(probe_warning | grep -q 'Forbidden to this identity' && echo named || echo unnamed)"
CRD_ERR=''

# The CRD read, when the OpenAPI could not answer.
CRD_FIXTURE_RC=0
CRD_FIXTURE='{"spec":{"versions":[{"name":"v1alpha1","served":true,"schema":{"openAPIV3Schema":{"properties":{"spec":{"properties":{"fallback":{"properties":{"failureThreshold":{},"replicas":{},"behavior":{"enum":["static","currentReplicas"]}}}}}}}}}]}}'
check 'probe: a CRD with fallback.behavior is KEDA 2.17 or later' 'yes' "$(probe)"
CRD_FIXTURE='{"spec":{"versions":[{"name":"v1alpha1","served":true,"schema":{"openAPIV3Schema":{"properties":{"spec":{"properties":{"fallback":{"properties":{"failureThreshold":{},"replicas":{}}}}}}}}}]}}'
check 'probe: a CRD without it is older' 'no' "$(probe)"
check 'probe: an older CRD says so in a warning' \
    'warned' "$(probe_warning | grep -q 'KEDA 2.17' && echo warned || echo silent)"
CRD_FIXTURE='{"spec":{"versions":[{"name":"v1alpha1","served":true,"schema":{"openAPIV3Schema":{"properties":{"spec":{"properties":{"fallback":{"properties":{"replicas":{}}}}}}}}},{"name":"v1alpha2","served":false,"schema":{"openAPIV3Schema":{"properties":{"spec":{"properties":{"fallback":{"properties":{"behavior":{}}}}}}}}}]}}'
check 'probe: the field on an unserved version only does not count' 'no' "$(probe)"
CRD_FIXTURE=''
CRD_FIXTURE_RC=1
check 'probe: a CRD that cannot be read is unknown, not new' 'unknown' "$(probe)"
CRD_FIXTURE_RC=0
CRD_FIXTURE='not json'
check 'probe: a CRD that does not parse is unknown' 'unknown' "$(probe)"

# A plan whose every entry renders from a template writes no fallback, so it
# does not probe, and does not warn about one.
rows_of() { local a; for a in "$@"; do printf '%s\037ns\037Deployment\037d\037m\0372\0374\037\037\n' "$a"; done; }
probed() { ( unset SO_KEDA_FALLBACK_BEHAVIOR; so_fallback_check "$1" 2>/dev/null; printf '%s' "${SO_KEDA_FALLBACK_BEHAVIOR:-unprobed}" ); }
check 'probe: a template-only plan is not probed' \
    'unprobed' "$(LLMSM_DEFAULT_SO_TEMPLATE=/x probed "$(rows_of yes no yes)")"
check 'probe: an adopt entry in a template plan is' \
    'unknown' "$(LLMSM_DEFAULT_SO_TEMPLATE=/x probed "$(rows_of yes adopt)")"
check 'probe: a generated entry is' \
    'unknown' "$(probed "$(rows_of yes)")"
unset -f kubectl

# What the plan table and the apply lines say about each entry.
check 'note: a generated entry with a floor gets one' '(+fallback)' "$(so_fallback_note yes 2)"
check 'note: a floor of zero says why it gets none' '(no fallback: floor 0)' "$(so_fallback_note yes 0)"
check 'note: an old KEDA says why it gets none' '(no fallback: KEDA < 2.17)' "$(SO_KEDA_FALLBACK_BEHAVIOR=no so_fallback_note yes 2)"
check 'note: an adopted entry keeps a fallback it has' '(+fallback unless it has one)' "$(so_fallback_note adopt 2)"
check 'note: an entry not applied says nothing' '-' "$(so_fallback_note no 2)"
check 'note: a template decides its own' '(fallback: as the template says)' "$(LLMSM_DEFAULT_SO_TEMPLATE=/x so_fallback_note yes 2)"
PLAN_FIXTURE="$WORK/plan.yaml"
printf '%s\n' 'plan:' \
    '  - {apply: yes, namespace: ns, kind: Deployment, name: up, modelID: m, minReplicas: 2, maxReplicas: 4}' \
    '  - {apply: yes, namespace: ns, kind: Deployment, name: parked, modelID: m, minReplicas: 0, maxReplicas: 4}' \
    > "$PLAN_FIXTURE"
check 'plan table: a FALLBACK column per entry' \
    'up:(+fallback)|parked:(no fallback: floor 0)' \
    "$(so_show_plan "$PLAN_FIXTURE" 2>/dev/null | awk '$4 == "up" || $4 == "parked" { print $4 ":" substr($0, index($0, "(")) }' | sed 's/ *$//' | paste -sd'|' -)"

# Adoption: added only to an object with none, never merged into one it has.
check 'adopt: add_fallback=yes adds the default block' \
    '3,2,currentReplicasIfHigher' \
    "$(so_adopt_patch m addr 2 8 10.0 '' yes | jq -r '.spec.fallback | [.failureThreshold, .replicas, .behavior] | map(tostring) | join(",")')"
check 'adopt: without add_fallback the patch has no fallback key' \
    'false' "$(so_adopt_patch m addr 2 8 10.0 '' '' | jq -r '.spec | has("fallback")')"

# The scaler's TLS address carries KEDA's ClusterTriggerAuthentication; the
# plaintext one carries none (deploy/lib/scaler_tls.sh, "KEDA's side").
check 'tls: a generated object on the TLS address references the ClusterTriggerAuthentication' \
    'scaling-manager-external-scaler-scaling-manager/ClusterTriggerAuthentication' \
    "$(render_default_scaledobject ns Deployment decode m scaling-manager-external-scaler.scaling-manager.svc:9090 1 8 '' '' | yq -o=json '.spec.triggers[0].authenticationRef' | jq -r '.name + "/" + .kind')"
check 'tls: a generated object on the plaintext address has no authenticationRef' \
    'null' \
    "$(render_default_scaledobject ns Deployment decode m scaling-manager-external-scaler.scaling-manager.svc.cluster.local:9090 1 8 '' '' | yq -o=json '.spec.triggers[0].authenticationRef')"
check 'tls: an adopt patch carries the authenticationRef it is given' \
    'scaling-manager-external-scaler-scaling-manager' \
    "$(so_adopt_patch m scaling-manager-external-scaler.scaling-manager.svc:9090 2 8 10.0 '' '' '' '{"name":"scaling-manager-external-scaler-scaling-manager","kind":"ClusterTriggerAuthentication"}' | jq -r '.spec.triggers[0].authenticationRef.name')"
check 'tls: an adopt patch with none given writes none' \
    'false' "$(so_adopt_patch m addr 2 8 10.0 '' '' | jq -r '.spec.triggers[0] | has("authenticationRef")')"
SO_FIXTURE=''
SO_FIXTURE_RC=0
kubectl() { printf '%s' "$SO_FIXTURE"; return "$SO_FIXTURE_RC"; }
check 'adopt: an object with no fallback gets one' \
    'yes' "$(so_adopt_needs_fallback ns so 2)"
check 'adopt: an object with a floor of zero gets none' \
    '' "$(so_adopt_needs_fallback ns so 0)"
check 'adopt: an old KEDA adds none' \
    '' "$(SO_KEDA_FALLBACK_BEHAVIOR=no so_adopt_needs_fallback ns so 2)"
SO_FIXTURE='{"failureThreshold":9,"replicas":4}'
check 'adopt: an object with a fallback keeps it' \
    '' "$(so_adopt_needs_fallback ns so 2)"
SO_FIXTURE='{"failureThreshold":3,"replicas":0}'
check 'adopt: a kept zero fallback on a floor of zero is warned about' \
    'warned' "$(so_adopt_needs_fallback ns so 0 2>&1 >/dev/null | grep -q 'replicas is 0 with minReplicaCount 0' && echo warned || echo silent)"
check 'adopt: a kept zero fallback above a floor is not' \
    'silent' "$(so_adopt_needs_fallback ns so 2 2>&1 >/dev/null | grep -q 'replicas is 0' && echo warned || echo silent)"
SO_FIXTURE='{"failureThreshold":3,"replicas":4}'
check 'adopt: a kept fallback above the new floor is warned about' \
    'warned' "$(so_adopt_needs_fallback ns so 2 2>&1 >/dev/null | grep -q 'replicas (4) is above the minReplicaCount this plan sets (2)' && echo warned || echo silent)"
SO_FIXTURE='{"failureThreshold":3,"replicas":2}'
check 'adopt: a kept fallback at the new floor is not' \
    'silent' "$(so_adopt_needs_fallback ns so 2 2>&1 >/dev/null | grep -q 'is above the minReplicaCount' && echo warned || echo silent)"
SO_FIXTURE=''
SO_FIXTURE_RC=1
check 'adopt: an object that cannot be read is not one without a fallback' \
    'unreadable' "$(so_adopt_needs_fallback ns so 2 2>/dev/null)"
check 'adopt: and the apply line says it was left as it was' \
    '(fallback unchanged: could not read it)' "$(so_adopt_fallback_note "$(so_adopt_needs_fallback ns so 2 2>/dev/null)" 2)"
check 'adopt: on a floor of zero too' \
    '(fallback unchanged: could not read it)' "$(so_adopt_fallback_note "$(so_adopt_needs_fallback ns so 0 2>/dev/null)" 0)"
SO_FIXTURE_RC=0
check 'adopt: a kept fallback says so' \
    '(kept its own fallback)' "$(so_adopt_fallback_note '' 2)"
check 'adopt: an added fallback says so' \
    '(+fallback)' "$(so_adopt_fallback_note yes 2)"
check 'adopt: LLMSM_SO_FALLBACK=false adds none' \
    '' "$(LLMSM_SO_FALLBACK=false so_adopt_needs_fallback ns so 2)"
unset -f kubectl

# A template owns its fallback, except the one shape KEDA scales to zero on.
TMPL_FIXTURE="$WORK/zero-fallback-template.yaml"
printf '%s\n' 'apiVersion: keda.sh/v1alpha1' 'kind: ScaledObject' 'metadata:' '  name: {{NAME}}-llmsm' \
    'spec:' '  minReplicaCount: {{MIN}}' '  fallback:' '    failureThreshold: 3' '    replicas: {{MIN}}' > "$TMPL_FIXTURE"
check 'template: fallback.replicas 0 on a floor of 0 is refused' \
    'refused' "$( (LLMSM_DEFAULT_SO_TEMPLATE="$TMPL_FIXTURE" render_scaledobject ns Deployment d m addr 0 4) > /dev/null 2>&1 && echo accepted || echo refused)"
check 'template: and prints no manifest for kubectl to apply' \
    '' "$( (LLMSM_DEFAULT_SO_TEMPLATE="$TMPL_FIXTURE" render_scaledobject ns Deployment d m addr 0 4) 2>/dev/null)"
check 'template: the same template above a floor renders' \
    '2' "$(LLMSM_DEFAULT_SO_TEMPLATE="$TMPL_FIXTURE" render_scaledobject ns Deployment d m addr 2 4 2>/dev/null | yq -r '.spec.fallback.replicas')"
check 'template: the shipped template renders a floor of 0 with no fallback' \
    'none,0' "$(LLMSM_DEFAULT_SO_TEMPLATE="$ROOT/config/samples/keda/external-scaler/scaledobject-template.yaml" render_scaledobject ns Deployment d m addr 0 4 2>/dev/null | yq -o=json '.spec' | jq -r '[(.fallback // "none"), .minReplicaCount] | map(tostring) | join(",")')"

# The same zero fallback behind another document: yq answers once per
# document, and the guard used to compare that whole multi-line answer with
# "true".
TMPL_MULTI="$WORK/multi-doc-template.yaml"
{ printf '%s\n' 'apiVersion: v1' 'kind: ConfigMap' 'metadata:' '  name: {{NAME}}-notes' 'data:' '  a: b' '---'
  cat "$TMPL_FIXTURE"; } > "$TMPL_MULTI"
check 'template: a zero fallback in a multi-document template is refused' \
    'refused' "$( (LLMSM_DEFAULT_SO_TEMPLATE="$TMPL_MULTI" render_scaledobject ns Deployment d m addr 0 4) > /dev/null 2>&1 && echo accepted || echo refused)"
check 'template: the multi-document template above a floor renders both' \
    'ConfigMap,ScaledObject' "$(LLMSM_DEFAULT_SO_TEMPLATE="$TMPL_MULTI" render_scaledobject ns Deployment d m addr 2 4 2>/dev/null | yq -r '.kind' | grep -v '^---$' | paste -sd, -)"
TMPL_BROKEN="$WORK/broken-template.yaml"
printf '%s\n' 'kind: ScaledObject' 'spec:' '  minReplicaCount: {{MIN}}' '  fallback: [unclosed' > "$TMPL_BROKEN"
check 'template: a template yq cannot read is refused, not passed' \
    'refused' "$( (LLMSM_DEFAULT_SO_TEMPLATE="$TMPL_BROKEN" render_scaledobject ns Deployment d m addr 0 4) > /dev/null 2>&1 && echo accepted || echo refused)"

# metricType. The scaler answers a replica count, which only AverageValue keeps
# a count, so the generator and the adopt patch say so explicitly and a
# template that asks for Value is warned about.
check 'metricType: the generator writes AverageValue on its trigger' \
    'external-push:AverageValue' "$(render_with 2 | yq -r '.spec.triggers[0] | .type + ":" + .metricType')"
check 'metricType: the adopt patch writes AverageValue on its trigger' \
    'AverageValue' "$(so_adopt_patch m addr 2 8 10.0 '' '' | jq -r '.spec.triggers[0].metricType')"
TMPL_VALUE="$WORK/value-template.yaml"
printf '%s\n' 'apiVersion: keda.sh/v1alpha1' 'kind: ScaledObject' 'metadata:' '  name: {{NAME}}-llmsm' \
    'spec:' '  minReplicaCount: {{MIN}}' '  triggers:' '    - type: external-push' '      name: scaling-manager-external-scaler' \
    '      metricType: Value' '      metadata:' '        scalerAddress: {{SCALER_ADDRESS}}' > "$TMPL_VALUE"
check 'metricType: a template trigger with Value is warned about' \
    'warned' "$(LLMSM_DEFAULT_SO_TEMPLATE="$TMPL_VALUE" render_scaledobject ns Deployment d m addr 2 4 2>&1 >/dev/null | grep -q 'has metricType: Value' && echo warned || echo silent)"
check 'metricType: and still renders' \
    'Value' "$(LLMSM_DEFAULT_SO_TEMPLATE="$TMPL_VALUE" render_scaledobject ns Deployment d m addr 2 4 2>/dev/null | yq -r '.spec.triggers[0].metricType')"
check 'metricType: the shipped template is not warned about' \
    'silent' "$(LLMSM_DEFAULT_SO_TEMPLATE="$ROOT/config/samples/keda/external-scaler/scaledobject-template.yaml" render_scaledobject ns Deployment d m addr 2 4 2>&1 >/dev/null | grep -q 'metricType' && echo warned || echo silent)"

# ----------------------------------------------------------------------------

# ----------------------------------------------------------------------------
# so_apply_plan / so_repoint_stale: which scaler an address names, and what an
# adopt keeps
# ----------------------------------------------------------------------------
# Executed against a stubbed kubectl that records every patch and apply. Two
# failures this covers, both found by review and neither visible to `bash -n`:
#
#   - In the out-of-reach shape (controller in its own namespace, managing a
#     tenant one) LLMSM_NS is the TENANT namespace when NAMESPACE is set, and every
#     trigger was written as scaling-manager-external-scaler.<tenant>, where nothing runs.
#     KEDA falls back to CPU and the install looks healthy.
#   - `apply: adopt` merge-patches the triggers LIST, which replaces it, so every
#     re-run stripped the keys set by hand on the trigger (defaultOutputTokens,
#     warmPool, ...), and repointed objects already working against a live scaling manager.
#
# Cases are asserted on what reached the API server -- the recorded patch and
# apply -- never on the library source.
command -v yq >/dev/null 2>&1 || {
    printf 'FATAL: yq is required to run the plan checks (so_plan_rows reads the plan with it).\n' >&2
    exit 1
}

SOW="$WORK/so"
mkdir -p "$SOW"

# STUB_SVC          "ns=present ns2=forbidden" -- any other namespace is NotFound
# STUB_RB           the RoleBinding list served for every namespace
# STUB_SOS          the ScaledObject list served for every namespace and -A
# STUB_SO_FORBIDDEN 1: a GET of one named ScaledObject is Forbidden
so_stub() {
    printf '%s\n' "$*" >> "$SOW/kubectl.log"
    local a ns="" prev=""
    local empty='{"items":[]}'
    for a in "$@"; do
        [ "$prev" = "-n" ] && ns="$a"
        prev="$a"
    done
    case "$1 $2" in
        "get svc")
            case " ${STUB_SVC:-} " in
                *" $ns=present "*) echo "service/scaling-manager-external-scaler"; return 0 ;;
                *" $ns=forbidden "*)
                    echo "Error from server (Forbidden): services \"scaling-manager-external-scaler\" is forbidden: User \"tenant\" cannot get resource \"services\" in API group \"\" in the namespace \"$ns\"" >&2
                    return 1 ;;
            esac
            echo 'Error from server (NotFound): services "scaling-manager-external-scaler" not found' >&2
            return 1 ;;
        "get rolebinding")
            printf '%s\n' "${STUB_RB:-$empty}" ;;
        "get scaledobject")
            case "$3" in
                -A|-n) printf '%s\n' "${STUB_SOS:-$empty}" ;;
                *)
                    if [ -n "${STUB_SO_FORBIDDEN:-}" ]; then
                        echo 'Error from server (Forbidden): scaledobjects.keda.sh is forbidden' >&2
                        return 1
                    fi
                    local one
                    one=$(printf '%s' "${STUB_SOS:-$empty}" \
                        | jq -ce --arg n "$3" '.items[] | select(.metadata.name == $n)') || {
                        echo "Error from server (NotFound): scaledobjects.keda.sh \"$3\" not found" >&2
                        return 1
                    }
                    # The one jsonpath the library asks of a named object:
                    # its spec.fallback, empty when it has none.
                    case " $* " in
                        *" jsonpath={.spec.fallback} "*)
                            printf '%s' "$one" | jq -c '.spec.fallback // empty' ;;
                        *) printf '%s\n' "$one" ;;
                    esac ;;
            esac ;;
        "patch scaledobject")
            local p="" next=0
            for a in "$@"; do
                [ "$next" = 1 ] && { p="$a"; next=0; }
                [ "$a" = "-p" ] && next=1
            done
            printf '%s\n' "$p" >> "$SOW/patched" ;;
        "apply -f")
            cat >> "$SOW/applied" ;;
        *) return 0 ;;
    esac
}

# run_so <function> [VAR=value...] runs it in a subshell against the stub and
# echoes its verdict: REFUSED when it refused, else OK. WARN is appended when it
# warned. What it wrote is left in $SOW/patched and $SOW/applied.
run_so() {
    local fn="$1"
    shift
    : > "$SOW/kubectl.log"; : > "$SOW/patched"; : > "$SOW/applied"; : > "$SOW/out"
    (
        log_info() { :; }
        log_success() { :; }
        log_warning() { printf '%s\n' "$1" >> "$SOW/warned"; }
        log_error() { printf 'REFUSED\n' >> "$SOW/out"; printf '%s\n' "$1" > "$SOW/refusal"; exit 1; }
        kubectl() { so_stub "$@"; }
        : > "$SOW/warned"
        LLMSM_NS=tenant LLMSM_NS_EXPLICIT='' LLMSM_DEFAULT_SO_PLAN='' LLMSM_DEFAULT_SO_NS=''
        STUB_SVC='' STUB_RB='' STUB_SOS='' STUB_SO_FORBIDDEN=''
        local kv
        for kv in "$@"; do eval "$kv"; done
        "$fn" "$SOW/plan.yaml" >/dev/null 2>&1
    )
    local v
    v="$(head -1 "$SOW/out")"
    [ -n "$v" ] || v=OK
    [ -s "$SOW/warned" ] && v="$v WARN"
    printf '%s' "$v"
}

# The address a run wrote: from the created object, else the patch.
written_addr() {
    { grep -h 'scalerAddress:' "$SOW/applied" 2>/dev/null | sed -E 's/.*scalerAddress:[[:space:]]*"?([^"]*)"?.*/\1/'
      jq -r '.spec.triggers[0].metadata.scalerAddress // empty' "$SOW/patched" 2>/dev/null
    } | head -1
}

plan_entry() {   # plan_entry <apply>
    cat > "$SOW/plan.yaml" <<YAML
plan:
  - apply: "$1"
    namespace: tenant
    kind: Deployment
    name: decode
    modelID: "Qwen/Qwen3-0.6B"
    minReplicas: 1
    maxReplicas: 4
    variantCost: "10.0"
YAML
}

RB_CTL='{"items":[{"metadata":{"name":"scaling-manager-manager-scaling-manager-ctl"},"subjects":[{"kind":"ServiceAccount","name":"scaling-manager-controller-manager","namespace":"scaling-manager-ctl"}]}]}'
# so_adopted <scaler namespace> -- an adopted object carrying two hand-set keys.
so_adopted() {
    printf '{"items":[{"metadata":{"name":"decode-so","namespace":"tenant"},"spec":{"scaleTargetRef":{"name":"decode"},"minReplicaCount":1,"maxReplicaCount":4,"triggers":[{"type":"external-push","name":"scaling-manager-external-scaler","metadata":{"scalerAddress":"scaling-manager-external-scaler.%s.svc.cluster.local:9090","modelID":"Qwen/Qwen3-0.6B","variantCost":"10.0","defaultOutputTokens":"512","warmPool":"pool-a"}}]}}]}' "$1"
}

# --- which scaler a created object names
plan_entry yes
check 'apply: out of reach, the scaler comes from the RoleBinding the installer wrote' \
    'OK|scaling-manager-external-scaler.scaling-manager-ctl.svc.cluster.local:9090' \
    "$(run_so so_apply_plan STUB_SVC='scaling-manager-ctl=present' "STUB_RB='$RB_CTL'")|$(written_addr)"

check 'apply: no scaler in LLMSM_NS and nothing names one -- refused, nothing written' \
    'REFUSED|' \
    "$(run_so so_apply_plan)|$(written_addr)"

check 'apply: the refusal names the command that fixes it' \
    'yes' \
    "$(grep -q 'NAMESPACE=tenant make scaledobjects-apply LLMSM_NS=<controller-namespace> LLMSM_WATCH_NS=tenant' "$SOW/refusal" && echo yes || echo no)"

# Following that command must scan the TENANT namespace. With LLMSM_NS given and
# no LLMSM_WATCH_NS, so_target_namespaces scanned the controller's own, empty one.
tns() { ( unset LLMSM_WATCH_NS LLMSM_DEFAULT_SO_NS; for kv in "$@"; do eval "$kv"; done; so_target_namespaces 2>/dev/null ); }
check 'plan: NAMESPACE differing from an explicit LLMSM_NS is the namespace scanned'     'tenant' "$(tns LLMSM_SCOPE=namespace LLMSM_NS=scaling-manager-ctl NAMESPACE_EXPLICIT=tenant)"
check 'plan: an explicit LLMSM_WATCH_NS still wins'     'other' "$(tns LLMSM_SCOPE=namespace LLMSM_NS=scaling-manager-ctl NAMESPACE_EXPLICIT=tenant LLMSM_WATCH_NS=other)"
check 'plan: no NAMESPACE scans LLMSM_NS, as before'     'scaling-manager-ctl' "$(tns LLMSM_SCOPE=namespace LLMSM_NS=scaling-manager-ctl NAMESPACE_EXPLICIT=)"

check 'apply: an explicit LLMSM_NS the tenant may not read is used, with a warning' \
    'OK WARN|scaling-manager-external-scaler.scaling-manager-ctl.svc.cluster.local:9090' \
    "$(run_so so_apply_plan LLMSM_NS=scaling-manager-ctl LLMSM_NS_EXPLICIT=scaling-manager-ctl STUB_SVC='scaling-manager-ctl=forbidden')|$(written_addr)"

check 'apply: an explicit LLMSM_NS with no scaler Service is refused' \
    'REFUSED|' \
    "$(run_so so_apply_plan LLMSM_NS=scaling-manager-ctl LLMSM_NS_EXPLICIT=scaling-manager-ctl)|$(written_addr)"

check 'apply: two live installs named in the namespace -- refused, not guessed' \
    'REFUSED|' \
    "$(run_so so_apply_plan "STUB_SVC='scaling-manager-ctl=present other=present'" "STUB_RB='$RB_CTL'" "STUB_SOS='$(so_adopted other | sed 's/decode/elsewhere/g')'")|$(written_addr)"

# --- what an adopt keeps
plan_entry adopt
check 'adopt: keeps the hand-set trigger keys (defaultOutputTokens, warmPool)' \
    'OK|512|pool-a|Qwen/Qwen3-0.6B' \
    "$(run_so so_apply_plan STUB_SVC='scaling-manager-ctl=present' "STUB_SOS='$(so_adopted scaling-manager-ctl)'")|$(jq -r '.spec.triggers[0].metadata | [.defaultOutputTokens, .warmPool, .modelID] | join("|")' "$SOW/patched" 2>/dev/null)"

# Keeping trigger keys and the fallback rules are separate: the keys are trigger
# metadata, the fallback is spec, metricType is the trigger's own and always
# AverageValue. One patch has to carry all three.
check 'adopt: keeps hand-set keys AND adds a fallback AND writes AverageValue' \
    'OK|512|pool-a|AverageValue|3,1,currentReplicasIfHigher' \
    "$(run_so so_apply_plan STUB_SVC='scaling-manager-ctl=present' "STUB_SOS='$(so_adopted scaling-manager-ctl)'")|$(jq -r '[.spec.triggers[0].metadata.defaultOutputTokens, .spec.triggers[0].metadata.warmPool, .spec.triggers[0].metricType, (.spec.fallback | [.failureThreshold, .replicas, .behavior] | map(tostring) | join(","))] | join("|")' "$SOW/patched" 2>/dev/null)"

check 'adopt: an object with its own fallback keeps it, and its hand-set keys' \
    'OK|512|false' \
    "$(run_so so_apply_plan STUB_SVC='scaling-manager-ctl=present' "STUB_SOS='$(so_adopted scaling-manager-ctl | jq -c '.items[0].spec.fallback = {failureThreshold: 5, replicas: 1}')'")|$(jq -r '[.spec.triggers[0].metadata.defaultOutputTokens, (.spec | has("fallback") | tostring)] | join("|")' "$SOW/patched" 2>/dev/null)"

check 'adopt: out of reach, re-applying keeps the address the object already works on' \
    'scaling-manager-external-scaler.scaling-manager-ctl.svc.cluster.local:9090' \
    "$(run_so so_apply_plan STUB_SVC='scaling-manager-ctl=present' "STUB_SOS='$(so_adopted scaling-manager-ctl)'" >/dev/null; written_addr)"

check 'adopt: an object on another LIVE install is not moved without LLMSM_NS' \
    'scaling-manager-external-scaler.other.svc.cluster.local:9090' \
    "$(run_so so_apply_plan LLMSM_NS=scaling-manager-ctl "STUB_SVC='scaling-manager-ctl=present other=present'" "STUB_SOS='$(so_adopted other)'" >/dev/null; written_addr)"

check 'adopt: an explicit LLMSM_NS does move it' \
    'scaling-manager-external-scaler.scaling-manager-ctl.svc.cluster.local:9090' \
    "$(run_so so_apply_plan LLMSM_NS=scaling-manager-ctl LLMSM_NS_EXPLICIT=scaling-manager-ctl "STUB_SVC='scaling-manager-ctl=present other=present'" "STUB_SOS='$(so_adopted other)'" >/dev/null; written_addr)"

check 'adopt: an object that cannot be read is not patched over' \
    '0' \
    "$(run_so so_apply_plan STUB_SVC='scaling-manager-ctl=present' "STUB_SOS='$(so_adopted scaling-manager-ctl)'" STUB_SO_FORBIDDEN=1 >/dev/null; grep -c . "$SOW/patched")"

# --- what the plan says about an adopted object
STUB_SOS="$(so_adopted scaling-manager-ctl)"
kubectl() { so_stub "$@"; }
IFS=$'\037' read -r _n _min _max _cost _pol _scaling_manager e_model e_kept e_addr <<< "$(so_existing_info tenant decode)"
check 'plan: so_existing_info reads the modelID, kept keys and address back' \
    'Qwen/Qwen3-0.6B|defaultOutputTokens,warmPool|scaling-manager-external-scaler.scaling-manager-ctl.svc.cluster.local:9090' \
    "${e_model:-}|${e_kept:-}|${e_addr:-}"
unset -f kubectl
STUB_SOS=''

kept_note() {
    local note='' existing_kept="$1" existing_name=decode-so
    so_kept_keys_note 2>/dev/null
    printf '%s' "$note"
}
check 'plan: the note names the keys an adopt keeps' \
    'yes' \
    "$(case "$(kept_note 'defaultOutputTokens,warmPool')" in *'defaultOutputTokens, warmPool'*) echo yes ;; *) echo no ;; esac)"

prefer() {   # prefer <discovered> <existing> [SO_PREFER_EXISTING_MODEL]
    local model="$1" existing_model="$2" existing_name=decode-so note=''
    SO_PREFER_EXISTING_MODEL="${3:-true}" so_prefer_existing_model tenant decode 2>/dev/null
    printf '%s' "$model"
}
check 'plan: a re-discovered modelID does not replace the one the object carries' \
    'Qwen/Qwen3-0.6B' "$(prefer /models/qwen Qwen/Qwen3-0.6B)"
check 'plan: verify-scaledobjects still sees what the container serves' \
    '/models/qwen' "$(prefer /models/qwen Qwen/Qwen3-0.6B false)"

# --- repoint: Forbidden is not dead
REPOINT_SOS='{"items":[{"metadata":{"name":"decode-so","namespace":"tenant"},"spec":{"triggers":[{"type":"external-push","metadata":{"scalerAddress":"scaling-manager-external-scaler.gone.svc.cluster.local:9090","modelID":"m"}}]}}]}'
check 'repoint: an address in a namespace that cannot be read is left alone' \
    '0' \
    "$(run_so so_repoint_stale LLMSM_NS=scaling-manager-ctl LLMSM_NS_EXPLICIT=scaling-manager-ctl "STUB_SVC='scaling-manager-ctl=present gone=forbidden'" "STUB_SOS='$REPOINT_SOS'" >/dev/null; grep -c . "$SOW/patched")"
check 'repoint: an explicit LLMSM_NS the tenant may not read is accepted, and a dead address repointed' \
    'OK WARN|scaling-manager-external-scaler.scaling-manager-ctl.svc.cluster.local:9090' \
    "$(run_so so_repoint_stale LLMSM_NS=scaling-manager-ctl LLMSM_NS_EXPLICIT=scaling-manager-ctl STUB_SVC='scaling-manager-ctl=forbidden' "STUB_SOS='$REPOINT_SOS'")|$(written_addr)"

# --- repoint: which scaler an address names. The short address
# scaling-manager-external-scaler.<ns>:<port> names <ns>, so it is this install and not a
# candidate; scaling-manager-ctl-other is another namespace, not scaling-manager-ctl.
REPOINT_SOS='{"items":[{"metadata":{"name":"decode-so","namespace":"tenant"},"spec":{"triggers":[{"type":"external-push","metadata":{"scalerAddress":"scaling-manager-external-scaler.scaling-manager-ctl:9090","modelID":"m"}}]}}]}'
check 'repoint: the short address of this install is not taken for another' \
    '0|0' \
    "$(run_so so_repoint_stale LLMSM_NS=scaling-manager-ctl LLMSM_NS_EXPLICIT=scaling-manager-ctl STUB_SVC='scaling-manager-ctl=forbidden' "STUB_SOS='$REPOINT_SOS'" >/dev/null; grep -c . "$SOW/patched")|$(grep -c 'points at' "$SOW/warned")"
REPOINT_SOS='{"items":[{"metadata":{"name":"decode-so","namespace":"tenant"},"spec":{"triggers":[{"type":"external-push","metadata":{"scalerAddress":"scaling-manager-external-scaler.scaling-manager-ctl-other.svc.cluster.local:9090","modelID":"m"}}]}}]}'
check 'repoint: a dead namespace that starts with this one is repointed here' \
    'scaling-manager-external-scaler.scaling-manager-ctl.svc.cluster.local:9090' \
    "$(run_so so_repoint_stale LLMSM_NS=scaling-manager-ctl LLMSM_NS_EXPLICIT=scaling-manager-ctl STUB_SVC='scaling-manager-ctl=present' "STUB_SOS='$REPOINT_SOS'" >/dev/null; written_addr)"

# KEDA names the HPA keda-hpa-<ScaledObject>, and that must fit in 63.
check 'so name: the longest name KEDA takes passes' \
    'ok' "$( (llmsm_check_so_name "$(printf 'a%.0s' $(seq 54))" && echo ok) 2>/dev/null)"
check 'so name: one more is refused, and says so' \
    '1|keda-hpa' \
    "$( out="$( (llmsm_check_so_name "$(printf 'a%.0s' $(seq 55))") 2>&1 )"; rc=$?; printf '%s|%s' "$rc" "$(printf '%s' "$out" | grep -o 'keda-hpa' | head -1)")"
# Only creating a pool checks: delete, monitor and plan must still reach a pool
# whose name is already over the limit.
check 'so name: a pool is checked where it is created, and nowhere else' \
    'cmd_create' \
    "$(awk '/^[a-z_]+\(\) \{/{fn=$1} /^[[:space:]]*llmsm_check_so_name/{print (fn == "" ? "top level" : fn)}' \
        "$(dirname "$0")/../deploy/warmpool.sh" | tr -d '()' | paste -sd, -)"

if [ "$FAILED" -gt 0 ]; then
    printf '\n%d model-identification assertion(s) failed.\n' "$FAILED" >&2
    exit 1
fi
# Same guard as check-workload-gaps.sh: a run that ends early otherwise exits 0
# having checked less than it claims.
if [ "$RAN" -ne "$EXPECT_CASES" ]; then
    printf '\n%d case(s) reported ok, %d expected -- the run did not finish.\n' \
        "$RAN" "$EXPECT_CASES" >&2
    exit 1
fi
printf '\nModel identification checks passed.\n'
