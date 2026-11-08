#!/usr/bin/env bash
#
# Executes the ScaledObject re-wire (llmsm_rewire_scaledobjects in
# deploy/lib/verify.sh, behind make scaledobjects-rewire and the last step of
# make scaledobjects-apply) against a STUBBED kubectl.
#
# KEDA asks the external scaler for the metric spec only when a ScaledObject's
# spec changes. When that call fails, the HPA stays on the CPU default for
# good, and the ScaledObject still reads Ready. The re-wire changes trigger
# metadata scalingManagerRewire so KEDA asks again. What this pins, each of which parses
# cleanly and is wrong on a cluster:
#
#   wired        not patched
#   unwired      patched on the trigger indices naming this scaler, and only
#                those; a ScaledObject naming another scaler is never touched
#   paused       skipped, not patched
#   unreadable   reported as unknown, never as wired, exit 3
#   DRY_RUN      nothing patched, the commands printed, non-zero exit;
#                a value other than true or false is refused, exit 2
#   still unwired after the retries: non-zero exit, named in the report
#   stale KEDA connection: the KEDA operator restart, not a re-wire, also
#                when every ScaledObject is wired (KEDA_RESTART=true does it,
#                except in a dry run); a copy from another CA: setup-prereqs
#   unlistable   exit 2, not "nothing to do"
#   wrong LLMSM_NS none names this scaler but some name another
#                scaling-manager-external-scaler.<namespace> and no scaler Service runs in
#                LLMSM_NS: a warning naming it (without a port), exit 3, not
#                "nothing to re-wire"; with the Service there, a second
#                install: exit 0; the Service unreadable: exit 3
#   address forms  this scaler by its short address (<host>:<port>), its
#                fully-qualified one or its bare host is re-wired; a
#                namespace that only starts with LLMSM_NS is another install;
#                the uninstall and pod-writer listers take the same forms;
#                an outside host (<ns>.attacker.example) is not this scaler,
#                upper case is; the jq and shell rules agree over one table
#   no address   a cpu trigger, which has no scalerAddress, names no scaler:
#                alone it is nothing to re-wire, exit 0; beside a trigger
#                naming another namespace or this scaler it changes nothing
#
# Every case asserts something that a missing function also fails, so a run
# against code without this feature fails rather than passes empty.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FAIL=0
PASS=0
CASES=0
# EXPECTED_CASES is how many cases this file runs. A case that silently stops
# running (a renamed function, an early exit) must fail the check.
EXPECTED_CASES=30
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

fail() { echo "  FAIL: $*"; FAIL=$((FAIL + 1)); }
pass() { PASS=$((PASS + 1)); }
ok() { if eval "$1"; then pass; else fail "$2"; fi; }
begin() { CASES=$((CASES + 1)); echo "== $*"; }

if ! command -v jq >/dev/null 2>&1; then
    echo "FAIL: jq is required (not skipping: a skipped check reads as a pass)"
    exit 1
fi

# Sourced through CR-stripped copies: a Windows checkout can write these as
# CRLF. A file that does not exist becomes an empty one, so every case fails on
# its missing function instead of the run aborting.
for f in constants common verify scaler_tls cleanup pod_writer; do
    if [ -f "$ROOT/deploy/lib/$f.sh" ]; then
        tr -d '\r' <"$ROOT/deploy/lib/$f.sh" >"$WORK/$f.sh"
    else
        : >"$WORK/$f.sh"
    fi
done

NS=scaling-manager-check
ADDR="scaling-manager-external-scaler.$NS.svc:9090"
OTHER="other-scaler.elsewhere.svc:9090"

forbidden() { echo "Error from server (Forbidden): $1 is forbidden: User \"tenant\" cannot get resource \"$1\"" >&2; }
notfound() { echo 'Error from server (NotFound): not found' >&2; }

# so <name> <state> <address>...: one ScaledObject in namespace apps, one
# trigger per address, into $WORK/so.json. An address "-" is a cpu trigger,
# which has no scalerAddress at all. <state> is how KEDA wired it:
#   wired         every trigger has its metric on the HPA and in the status
#   unwired       none has, whatever is patched
#   until-rewire  none has until a scalingManagerRewire patch reaches this ScaledObject
#   paused        unwired, and the Paused condition is True
#   so-forbidden  the ScaledObject cannot be read by name
#   hpa-forbidden its HPA cannot be read
so() {
    local name="$1" state="$2"
    shift 2
    [ -f "$WORK/so.json" ] || echo '{"items":[]}' >"$WORK/so.json"
    printf '%s\n' "$@" | jq -R . | jq -s --arg n "$name" --arg st "$state" '
        {metadata: {namespace: "apps", name: $n},
         spec: {triggers: [.[] | if . == "-" then {type: "cpu", metadata: {value: "80"}}
                                 else {type: "external", metadata: {scalerAddress: ., modelID: "m"}} end]},
         status: {conditions: (if $st == "paused" then [{type: "Paused", status: "True"}] else [] end)}}' >"$WORK/one.json"
    jq --slurpfile o "$WORK/one.json" '.items += $o' "$WORK/so.json" >"$WORK/so.tmp" && mv "$WORK/so.tmp" "$WORK/so.json"
    echo "$state" >"$WORK/state.$name"
}

# wired_now <name> succeeds when KEDA has wired it, per its state.
wired_now() {
    case "$(cat "$WORK/state.$1")" in
        wired) return 0 ;;
        until-rewire) grep -q "^patch scaledobject $1 -n apps .*scalingManagerRewire" "$WORK/patches" ;;
        *) return 1 ;;
    esac
}

# metric_names <name>: the metric names KEDA gives each trigger, s<index>-llmsm.
metric_names() {
    jq -c --arg n "$1" '[.items[] | select(.metadata.name == $n) | .spec.triggers | keys[] | "s\(.)-llmsm"]' "$WORK/so.json"
}

stub_kubectl() {
    printf '%s\n' "$*" >>"$WORK/kubectl.log"
    local a="$*" emn
    case "$a" in
        "get scaledobjects.keda.sh "*)
            [ "${STUB_SO_LIST:-}" != FORBIDDEN ] || { forbidden scaledobjects; return 1; }
            cat "$WORK/so.json"
            ;;
        "get scaledobject "*" -n apps -o json")
            set -- $a
            [ "$(cat "$WORK/state.$3")" != so-forbidden ] || { forbidden scaledobjects; return 1; }
            if wired_now "$3"; then emn="$(metric_names "$3")"; else emn='[]'; fi
            jq --arg n "$3" --argjson e "$emn" '.items[] | select(.metadata.name == $n) | .status.externalMetricNames = $e' "$WORK/so.json"
            ;;
        "get hpa keda-hpa-"*" -n apps -o json")
            set -- $a
            local name="${3#keda-hpa-}"
            [ "$(cat "$WORK/state.$name")" != hpa-forbidden ] || { forbidden horizontalpodautoscalers; return 1; }
            if wired_now "$name"; then
                metric_names "$name" | jq '{spec: {metrics: [.[] | {type: "External", external: {metric: {name: .}}}]}}'
            else
                echo '{"spec":{"metrics":[{"type":"Resource","resource":{"name":"cpu"}}]}}'
            fi
            ;;
        "patch scaledobject "*) printf '%s\n' "$a" >>"$WORK/patches" ;;
        "get svc scaling-manager-external-scaler -n $NS -o name")
            case "${STUB_SVC:-absent}" in
                present) echo "service/scaling-manager-external-scaler" ;;
                forbidden) forbidden services; return 1 ;;
                *) echo 'Error from server (NotFound): services "scaling-manager-external-scaler" not found' >&2; return 1 ;;
            esac
            ;;
        "logs -n $NS -l "*) : ;;
        "rollout restart "*) : ;;
        *) return 0 ;;
    esac
}

# run [VAR=value...]: runs llmsm_rewire_scaledobjects in a subshell with the
# libraries sourced and kubectl stubbed. Report lines land in $WORK/stdout,
# warnings in $WORK/warnings, info in $WORK/info. Sets RC.
run() {
    : >"$WORK/kubectl.log"
    : >"$WORK/warnings"
    : >"$WORK/info"
    : >"$WORK/patches"
    : >"$WORK/stdout"
    : >"$WORK/stderr"
    (
        # shellcheck disable=SC1091
        . "$WORK/constants.sh"
        # shellcheck disable=SC1091
        . "$WORK/common.sh"
        # shellcheck disable=SC1091
        . "$WORK/verify.sh"
        # shellcheck disable=SC1091
        . "$WORK/scaler_tls.sh"
        log_info() { echo "$1" >>"$WORK/info"; }
        log_success() { echo "$1" >>"$WORK/info"; }
        log_warning() { echo "$1" >>"$WORK/warnings"; }
        log_error() { echo "$1" >>"$WORK/warnings"; exit 1; }
        kubectl() { stub_kubectl "$@"; }
        LLMSM_NS=$NS
        LLMSM_SCOPE=namespace
        unset LLMSM_WATCH_NS DRY_RUN KEDA_RESTART
        KEDA_NAMESPACE_EXPLICIT=true
        KEDA_NAMESPACE=keda
        # No waiting in a check: each wait reads once.
        LLMSM_SCALER_REWIRE_WAIT=0
        LLMSM_SCALER_POLL_SECONDS=0
        local kv
        for kv in "$@"; do eval "$kv"; done
        declare -F llmsm_rewire_scaledobjects >/dev/null || { echo "llmsm_rewire_scaledobjects is not defined" >>"$WORK/warnings"; exit 3; }
        llmsm_rewire_scaledobjects >"$WORK/stdout" 2>>"$WORK/stderr"
    )
    RC=$?
}

reset() { rm -f "$WORK/so.json" "$WORK"/state.*; }
patches_of() { grep -c "^patch scaledobject $1 " "$WORK/patches"; }
report_has() { grep -Eq "^ +$1 +apps/$2( |\$)" "$WORK/stdout"; }

begin "wired: nothing is patched, and it is reported wired"
reset
so app-a wired "$ADDR"
run
ok '[ "$RC" = 0 ] && [ ! -s "$WORK/patches" ] && report_has wired app-a' "wired: exit $RC (want 0), patches '$(cat "$WORK/patches")', report $(cat "$WORK/stdout")"

begin "unwired: re-wired on this scaler's trigger indices only, then reported wired"
reset
so app-b until-rewire "$ADDR" "$OTHER" "$ADDR"
so app-c wired "$ADDR"
so other-d unwired "$OTHER"
run LLMSM_SCALER_REWIRE_TRIES=3
ok '[ "$RC" = 0 ] && grep -q "\"path\":\"/spec/triggers/0/metadata/scalingManagerRewire\"" "$WORK/patches" && grep -q "\"path\":\"/spec/triggers/2/metadata/scalingManagerRewire\"" "$WORK/patches"' "unwired: exit $RC (want 0), not patched on triggers 0 and 2: $(cat "$WORK/patches")"
ok '! grep -q "/spec/triggers/1/" "$WORK/patches" && [ "$(patches_of app-c)" = 0 ] && [ "$(patches_of other-d)" = 0 ] && [ "$(patches_of app-b)" = 2 ]' "unwired: patched another trigger or ScaledObject (want app-b x2, triggers 0 and 2): $(cat "$WORK/patches")"
ok 'report_has wired app-b && report_has wired app-c && ! grep -q other-d "$WORK/stdout" && [ ! -s "$WORK/warnings" ]' "unwired: report $(cat "$WORK/stdout"), warnings $(cat "$WORK/warnings")"

begin "paused: skipped, not patched"
reset
so app-p paused "$ADDR"
run LLMSM_SCALER_REWIRE_TRIES=3
ok '[ "$RC" = 0 ] && [ ! -s "$WORK/patches" ] && report_has paused app-p && grep -q "skipped" "$WORK/stdout"' "paused: exit $RC (want 0), patches '$(cat "$WORK/patches")', report $(cat "$WORK/stdout")"

begin "unreadable ScaledObject or HPA: reported unknown, never wired, exit 3"
reset
so app-x so-forbidden "$ADDR"
so app-y hpa-forbidden "$ADDR"
so app-z wired "$ADDR"
run LLMSM_SCALER_REWIRE_TRIES=3
ok '[ "$RC" = 3 ] && [ ! -s "$WORK/patches" ] && report_has unknown app-x && report_has unknown app-y && ! report_has wired app-x && ! report_has wired app-y && report_has wired app-z' "unreadable: exit $RC, patches '$(cat "$WORK/patches")', report $(cat "$WORK/stdout")"

begin "DRY_RUN=true: lists, prints the commands, patches nothing, exits non-zero"
reset
so app-b until-rewire "$ADDR" "$OTHER" "$ADDR"
so app-c wired "$ADDR"
run DRY_RUN=true
ok '[ "$RC" = 1 ] && [ ! -s "$WORK/patches" ] && report_has unwired app-b && report_has wired app-c' "dry run: exit $RC (want 1), patches '$(cat "$WORK/patches")', report $(cat "$WORK/stdout")"
ok 'grep -q "kubectl patch scaledobject app-b -n apps --type json -p .*/spec/triggers/0/metadata/scalingManagerRewire" "$WORK/stdout" && grep -q "/spec/triggers/2/metadata/scalingManagerRewire" "$WORK/stdout" && ! grep -q "/spec/triggers/1/" "$WORK/stdout" && ! grep -q "scaledobject app-c" "$WORK/stdout"' "dry run: commands not printed for app-b triggers 0 and 2 only: $(cat "$WORK/stdout")"

begin "still unwired after the retries: bounded, named, non-zero exit"
reset
so app-s unwired "$ADDR"
run LLMSM_SCALER_REWIRE_TRIES=2
ok '[ "$RC" = 1 ] && [ "$(patches_of app-s)" = 2 ] && report_has unwired app-s && grep -q "still unwired" "$WORK/warnings"' "still unwired: exit $RC (want 1), $(patches_of app-s) patches (want 2), report $(cat "$WORK/stdout"), warnings $(cat "$WORK/warnings")"

# KEDA presenting a client certificate from before a reinstall is refused on
# every call; only an operator restart helps. hack/check-scaler-tls.sh pins
# how that is detected; here it is given, and the re-wire must name the
# restart instead of patching.
STALE='llmsm_scaler_keda_conn_state() { LLMSM_SCALER_KEDA_CONN=stale; LLMSM_SCALER_KEDA_CONN_PODS=keda-operator-7f9c; LLMSM_SCALER_KEDA_RUN_NS=keda; }'
begin "stale KEDA connection: names the KEDA operator restart, does not patch"
reset
so app-s unwired "$ADDR"
run LLMSM_SCALER_REWIRE_TRIES=3 "$STALE"
ok '[ "$RC" = 1 ] && [ ! -s "$WORK/patches" ] && grep -q "kubectl rollout restart deployment/keda-operator -n keda" "$WORK/warnings" && report_has unwired app-s' "stale: exit $RC (want 1), patches '$(cat "$WORK/patches")', warnings $(cat "$WORK/warnings")"

begin "stale KEDA connection, DRY_RUN=true: the restart, not the patch command"
reset
so app-s unwired "$ADDR"
run DRY_RUN=true "$STALE"
ok '[ "$RC" = 1 ] && [ ! -s "$WORK/patches" ] && grep -q "kubectl rollout restart deployment/keda-operator -n keda" "$WORK/warnings" && ! grep -q "kubectl patch scaledobject" "$WORK/stdout"' "stale dry run: exit $RC (want 1), warnings $(cat "$WORK/warnings"), stdout $(cat "$WORK/stdout")"

# A KEDA wired before the reinstall is refused the same way: every HPA reads
# wired, and nothing scales. llmsm_verify_scaler_wiring names the restart then.
begin "stale KEDA connection, every ScaledObject wired: names the restart, exit 1"
reset
so app-a wired "$ADDR"
run "$STALE"
ok '[ "$RC" = 1 ] && [ ! -s "$WORK/patches" ] && grep -q "kubectl rollout restart deployment/keda-operator -n keda" "$WORK/warnings" && grep -q "Not restarted" "$WORK/warnings" && ! grep -q "^rollout restart" "$WORK/kubectl.log" && report_has wired app-a' "wired + stale: exit $RC (want 1), patches '$(cat "$WORK/patches")', warnings $(cat "$WORK/warnings")"

begin "stale KEDA connection, every ScaledObject wired, KEDA_RESTART=true: restarts KEDA"
reset
so app-a wired "$ADDR"
run KEDA_RESTART=true "$STALE"
ok '[ "$RC" = 0 ] && [ ! -s "$WORK/patches" ] && grep -q "^rollout restart deployment/keda-operator -n keda" "$WORK/kubectl.log" && grep -q "KEDA_RESTART=true: restarted" "$WORK/info"' "wired + stale + KEDA_RESTART: exit $RC (want 0), kubectl $(grep rollout "$WORK/kubectl.log"), info $(cat "$WORK/info")"

begin "stale KEDA connection, every ScaledObject wired, DRY_RUN=true KEDA_RESTART=true: names it, restarts nothing"
reset
so app-a wired "$ADDR"
run DRY_RUN=true KEDA_RESTART=true "$STALE"
ok '[ "$RC" = 1 ] && ! grep -q "^rollout restart" "$WORK/kubectl.log" && grep -q "kubectl rollout restart deployment/keda-operator -n keda" "$WORK/warnings"' "wired + stale dry run: exit $RC (want 1), kubectl $(grep rollout "$WORK/kubectl.log"), warnings $(cat "$WORK/warnings")"

COPY='llmsm_scaler_keda_conn_state() { LLMSM_SCALER_KEDA_CONN=copy; LLMSM_SCALER_KEDA_CONN_PODS=keda-operator-7f9c; LLMSM_SCALER_KEDA_RUN_NS=keda; }'
begin "KEDA given a certificate from another CA, every ScaledObject wired: setup-prereqs and the restart, exit 1"
reset
so app-a wired "$ADDR"
run "$COPY"
ok '[ "$RC" = 1 ] && [ ! -s "$WORK/patches" ] && grep -q "setup-prereqs" "$WORK/warnings" && grep -q "kubectl rollout restart deployment/keda-operator -n keda" "$WORK/warnings"' "wired + copy: exit $RC (want 1), warnings $(cat "$WORK/warnings")"

begin "DRY_RUN=1: refused before anything is read or patched, exit 2"
reset
so app-b until-rewire "$ADDR"
run DRY_RUN=1
ok '[ "$RC" = 2 ] && [ ! -s "$WORK/patches" ] && [ ! -s "$WORK/kubectl.log" ] && grep -q "DRY_RUN must be true or false" "$WORK/warnings"' "DRY_RUN=1: exit $RC (want 2), patches '$(cat "$WORK/patches")', kubectl $(tr '\n' ';' <"$WORK/kubectl.log"), warnings $(cat "$WORK/warnings")"

begin "ScaledObjects cannot be listed: exit 2, not nothing to do"
reset
so app-s unwired "$ADDR"
run STUB_SO_LIST=FORBIDDEN
ok '[ "$RC" = 2 ] && [ ! -s "$WORK/patches" ] && grep -q "Could not list" "$WORK/warnings"' "unlistable: exit $RC (want 2), warnings $(cat "$WORK/warnings")"

# LLMSM_NS defaults to NAMESPACE, so a re-wire run from the workload namespace
# looks for scaling-manager-external-scaler.<that namespace> and finds nothing. That must
# not read as "nothing to re-wire".
begin "none names this scaler, some name another scaling manager namespace: warns naming it, exit 3"
reset
so app-o unwired "scaling-manager-external-scaler.scaling-manager-elsewhere.svc:9090"
so other-d unwired "$OTHER"
run
ok '[ "$RC" = 3 ] && [ ! -s "$WORK/patches" ] && grep -q "scaling-manager-elsewhere" "$WORK/warnings" && grep -q "LLMSM_NS=scaling-manager-elsewhere" "$WORK/warnings" && ! grep -q "nothing to re-wire" "$WORK/info"' "other scaling manager namespace: exit $RC (want 3), patches '$(cat "$WORK/patches")', warnings $(cat "$WORK/warnings"), info $(cat "$WORK/info")"

# A short address resolves through KEDA's DNS search path; the namespace is
# the part before the port.
begin "none names this scaler, one names another by its short address: the namespace without the port"
reset
so app-o unwired "scaling-manager-external-scaler.scaling-manager-elsewhere:9090"
run
ok '[ "$RC" = 3 ] && grep -q "LLMSM_NS=scaling-manager-elsewhere " "$WORK/warnings" && ! grep -q "scaling-manager-elsewhere:9090" "$WORK/warnings"' "short address: exit $RC (want 3), warnings $(cat "$WORK/warnings")"

# A correct LLMSM_NS whose install has no ScaledObject yet, beside a second
# install whose ScaledObjects are visible: not a wrong LLMSM_NS.
begin "none names this scaler, another install's do, and the scaler runs in LLMSM_NS: exit 0"
reset
so app-o unwired "scaling-manager-external-scaler.scaling-manager-elsewhere.svc:9090"
run STUB_SVC=present
ok '[ "$RC" = 0 ] && [ ! -s "$WORK/patches" ] && [ ! -s "$WORK/warnings" ] && grep -q "scaling-manager-elsewhere" "$WORK/info"' "second install: exit $RC (want 0), warnings $(cat "$WORK/warnings"), info $(cat "$WORK/info")"

begin "none names this scaler, another install's do, and the scaler Service cannot be read: exit 3"
reset
so app-o unwired "scaling-manager-external-scaler.scaling-manager-elsewhere.svc:9090"
run STUB_SVC=forbidden
ok '[ "$RC" = 3 ] && [ ! -s "$WORK/patches" ] && grep -q "could not be read" "$WORK/warnings"' "svc unreadable: exit $RC (want 3), warnings $(cat "$WORK/warnings")"

begin "none names any scaling manager scaler: nothing to re-wire, exit 0"
reset
so other-d unwired "$OTHER"
run
ok '[ "$RC" = 0 ] && [ ! -s "$WORK/patches" ] && [ ! -s "$WORK/warnings" ] && grep -q "nothing to re-wire" "$WORK/info"' "no scaling manager scaler: exit $RC (want 0), warnings $(cat "$WORK/warnings"), info $(cat "$WORK/info")"

# This scaler named by its short address: KEDA resolves it through its DNS
# search path, so it is as live as the long one, and must be re-wired, not
# read as "nothing to re-wire".
begin "one names this scaler by its short address: re-wired, not nothing to re-wire"
reset
so app-sh until-rewire "scaling-manager-external-scaler.$NS:9090"
run LLMSM_SCALER_REWIRE_TRIES=3
ok '[ "$RC" = 0 ] && grep -q "\"path\":\"/spec/triggers/0/metadata/scalingManagerRewire\"" "$WORK/patches" && report_has wired app-sh && ! grep -q "nothing to re-wire" "$WORK/info" && [ ! -s "$WORK/warnings" ]' "short address: exit $RC (want 0), patches '$(cat "$WORK/patches")', report $(cat "$WORK/stdout"), warnings $(cat "$WORK/warnings"), info $(cat "$WORK/info")"

begin "the fully-qualified address and the bare host name this scaler too"
reset
so app-fq until-rewire "scaling-manager-external-scaler.$NS.svc.cluster.local:9090" "$OTHER" "scaling-manager-external-scaler.$NS"
run LLMSM_SCALER_REWIRE_TRIES=3
ok '[ "$RC" = 0 ] && grep -q "/spec/triggers/0/metadata/scalingManagerRewire" "$WORK/patches" && grep -q "/spec/triggers/2/metadata/scalingManagerRewire" "$WORK/patches" && ! grep -q "/spec/triggers/1/" "$WORK/patches" && report_has wired app-fq' "long and bare: exit $RC (want 0), patches '$(cat "$WORK/patches")' (want triggers 0 and 2), report $(cat "$WORK/stdout")"

# A namespace that only starts with this one is another install's.
begin "a namespace that starts with this one is another: not re-wired, named as another"
reset
so app-px unwired "scaling-manager-external-scaler.$NS-other.svc:9090" "scaling-manager-external-scaler.$NS-other:9090"
run
ok '[ "$RC" = 3 ] && [ ! -s "$WORK/patches" ] && grep -q "LLMSM_NS=$NS-other" "$WORK/warnings" && ! grep -q "nothing to re-wire" "$WORK/info"' "prefixed namespace: exit $RC (want 3), patches '$(cat "$WORK/patches")', warnings $(cat "$WORK/warnings")"

# The other two listers of this scaler's ScaledObjects: the uninstall one
# (llmsm_scaledobjects_calling_us, cleanup.sh) and the pod-writer grant one
# (llmsm_pod_writer_namespaces, pod_writer.sh). kubectl hands back the rows its
# go-template renders.
lister() {
    local fn="$1" rows="$2"
    shift 2
    (
        # shellcheck disable=SC1091
        . "$WORK/common.sh"
        # shellcheck disable=SC1091
        . "$WORK/cleanup.sh"
        # shellcheck disable=SC1091
        . "$WORK/pod_writer.sh"
        kubectl() { case "$*" in "get scaledobject -A "*) printf '%s\n' "$rows" ;; *) return 1 ;; esac; }
        LLMSM_NS=$NS
        declare -F "$fn" >/dev/null || { echo "$fn is not defined"; exit 3; }
        "$fn" "$@" | tr '\n' ' '
    )
}

begin "uninstall lister: every address form of this scaler, not a namespace that starts with it"
ROWS="apps/long scaling-manager-external-scaler.$NS.svc:9090
apps/fq scaling-manager-external-scaler.$NS.svc.cluster.local:9090
apps/short scaling-manager-external-scaler.$NS:9090
apps/bare scaling-manager-external-scaler.$NS
apps/upper scaling-manager-External-Scaler.$NS.SVC:9090
apps/px scaling-manager-external-scaler.$NS-other:9090
apps/evil scaling-manager-external-scaler.$NS.attacker.example:9090
apps/other $OTHER"
GOT="$(lister llmsm_scaledobjects_calling_us "$ROWS")"
ok '[ "$GOT" = "apps/bare apps/fq apps/long apps/short apps/upper " ]' "uninstall lister: got '$GOT' (want apps/bare apps/fq apps/long apps/short apps/upper)"

begin "pod-writer lister: every address form of this scaler, and the bare host is not unattributed"
ROWS="t-long scaling-manager-external-scaler.$NS.svc:9090 - m
t-short scaling-manager-external-scaler.$NS:9090 - m
t-bare scaling-manager-external-scaler.$NS - m
t-up scaling-manager-EXTERNAL-SCALER.$NS.SVC.CLUSTER.LOCAL:9090 - m
t-px scaling-manager-external-scaler.$NS-other:9090 - m
t-evil scaling-manager-external-scaler.$NS.attacker.example:9090 - m
t-ip 10.0.0.1:9090 - m"
GOT="$(lister llmsm_pod_writer_namespaces "$ROWS" "$NS" models)"
ok '[ "$GOT" = "t-bare t-long t-short t-up " ]' "pod-writer lister, models: got '$GOT' (want t-bare t-long t-short t-up)"
GOT="$(lister llmsm_pod_writer_namespaces "$ROWS" "$NS" unattributed)"
ok '[ "$GOT" = "t-evil scaling-manager-external-scaler.$NS.attacker.example:9090 t-ip 10.0.0.1:9090 " ]' "pod-writer lister, unattributed: got '$GOT' (want t-evil and t-ip)"

# A host outside the cluster that only starts with this scaler name is not
# this scaler, and case does not matter: DNS does not regard it.
begin "re-wire: an outside host is not this scaler; an upper-case address is"
reset
so app-ci until-rewire "scaling-manager-External-Scaler.$NS.SVC:9090" "scaling-manager-external-scaler.$NS.attacker.example:9090"
run LLMSM_SCALER_REWIRE_TRIES=3
ok '[ "$RC" = 0 ] && grep -q "/spec/triggers/0/metadata/scalingManagerRewire" "$WORK/patches" && ! grep -q "/spec/triggers/1/" "$WORK/patches" && report_has wired app-ci' "case and outside host: exit $RC (want 0), patches '$(cat "$WORK/patches")' (want trigger 0 only), report $(cat "$WORK/stdout")"

# common.sh writes the rule twice, once for jq and once for the shell. Both
# run over one table, each against the expected answer, so they cannot drift.
begin "the jq and shell address rules agree, address by address"
RULE_TABLE="yes scaling-manager-external-scaler.$NS
yes scaling-manager-external-scaler.$NS:9090
yes scaling-manager-external-scaler.$NS.svc
yes scaling-manager-external-scaler.$NS.svc:9090
yes scaling-manager-external-scaler.$NS.svc.cluster.local
yes scaling-manager-external-scaler.$NS.svc.cluster.local:9090
yes scaling-manager-EXTERNAL-SCALER.$NS.SVC:9090
no scaling-manager-external-scaler.$NS.attacker.example:9090
no scaling-manager-external-scaler.$NS.svc.attacker:9090
no scaling-manager-external-scaler.$NS.svc.cluster.local.evil:9090
no scaling-manager-external-scaler.$NS.other.svc:9090
no scaling-manager-external-scaler.$NS-other:9090
no scaling-manager-external-scaler.$NS-other.svc:9090
no scaling-manager-external-scaler:9090
no scaling-manager-external-scaler
no scaling-manager-external-scaler.
no scaling-manager-external-scaler.:9090
no
no $OTHER
no 10.0.0.1:9090"
RULE_OUT="$(
    # shellcheck disable=SC1091
    . "$WORK/common.sh"
    declare -F llmsm_names_scaler >/dev/null || { echo "llmsm_names_scaler is not defined"; exit 0; }
    [ -n "${LLMSM_SCALER_ADDRESS_JQ:-}" ] || { echo "LLMSM_SCALER_ADDRESS_JQ is not set"; exit 0; }
    printf '%s\n' "$RULE_TABLE" | while read -r want addr; do
        if llmsm_names_scaler "$NS" "$addr"; then sh=yes; else sh=no; fi
        jq=$(jq -nr --arg a "$addr" --arg ns "$NS" "$LLMSM_SCALER_ADDRESS_JQ"'$a | if llmsm_names_scaler($ns) then "yes" else "no" end' 2>&1)
        [ "$sh" = "$want" ] && [ "$jq" = "$want" ] || echo "$addr: want $want, shell $sh, jq $jq"
        # The namespace an address names: this one for a yes, none for these no.
        shns=$(llmsm_scaler_ns "$addr" || true)
        jqns=$(jq -nr --arg a "$addr" "$LLMSM_SCALER_ADDRESS_JQ"'$a | llmsm_scaler_ns // ""' 2>&1)
        if [ "$want" = yes ]; then wantns=$NS; else wantns=""; fi
        case "$addr" in *"$NS-other"*) wantns="$NS-other" ;; esac
        [ "$shns" = "$wantns" ] && [ "$jqns" = "$wantns" ] || echo "$addr: namespace want '$wantns', shell '$shns', jq '$jqns'"
    done
)"
ok '[ -z "$RULE_OUT" ]' "rules disagree: $RULE_OUT"
NULL_OUT="$(
    # shellcheck disable=SC1091
    . "$WORK/common.sh"
    jq -nc --arg ns "$NS" "${LLMSM_SCALER_ADDRESS_JQ:-}"'[null | llmsm_names_scaler($ns), llmsm_scaler_ns]' 2>&1
)"
ok '[ "$NULL_OUT" = "[false,null]" ]' "a null address: jq gave '$NULL_OUT' (want [false,null])"

# A trigger with no scalerAddress -- cpu, prometheus, cron -- is the ordinary
# case. It names no scaler, and must not make the list unreadable.
begin "only triggers with no scalerAddress: nothing to re-wire, exit 0"
reset
so cpu-a unwired "-"
so cpu-b unwired "-" "-"
run
ok '[ "$RC" = 0 ] && [ ! -s "$WORK/patches" ] && [ ! -s "$WORK/warnings" ] && grep -q "nothing to re-wire" "$WORK/info"' "no address: exit $RC (want 0), warnings $(cat "$WORK/warnings"), info $(cat "$WORK/info")"

begin "a trigger with no scalerAddress beside one naming another namespace: that namespace, exit 3"
reset
so mix-o unwired "-" "scaling-manager-external-scaler.scaling-manager-elsewhere.svc:9090"
run
ok '[ "$RC" = 3 ] && [ ! -s "$WORK/patches" ] && grep -q "LLMSM_NS=scaling-manager-elsewhere" "$WORK/warnings" && ! grep -q "could not be read" "$WORK/warnings"' "mix, other namespace: exit $RC (want 3), warnings $(cat "$WORK/warnings")"
OTHERS="$(
    # shellcheck disable=SC1091
    . "$WORK/constants.sh"; . "$WORK/common.sh"; . "$WORK/verify.sh"; . "$WORK/scaler_tls.sh"
    LLMSM_NS=$NS
    llmsm_scaler_other_namespaces "$(cat "$WORK/so.json")"; echo "rc=$?"
)"
ok '[ "$(printf "%s" "$OTHERS" | tr "\n" " ")" = "scaling-manager-elsewhere rc=0" ]' "llmsm_scaler_other_namespaces with a cpu trigger: '$OTHERS' (want scaling-manager-elsewhere, rc=0)"

begin "a trigger with no scalerAddress beside one naming this scaler: re-wired on that trigger"
reset
so mix-t until-rewire "-" "$ADDR"
run LLMSM_SCALER_REWIRE_TRIES=3
ok '[ "$RC" = 0 ] && grep -q "/spec/triggers/1/metadata/scalingManagerRewire" "$WORK/patches" && ! grep -q "/spec/triggers/0/" "$WORK/patches" && report_has wired mix-t' "mix, this scaler: exit $RC (want 0), patches '$(cat "$WORK/patches")' (want trigger 1 only), report $(cat "$WORK/stdout")"

begin "the make targets: scaledobjects-apply re-wires, scaledobjects-rewire exists with help"
MK="$(tr -d '\r' <"$ROOT/Makefile")"
APPLY="$(printf '%s\n' "$MK" | awk '/^scaledobjects-apply:/ { on = 1; next } on && /^[^\t]/ { exit } on')"
REWIRE="$(printf '%s\n' "$MK" | awk '/^scaledobjects-rewire:/ { on = 1; next } on && /^[^\t]/ { exit } on')"
ok 'printf "%s" "$APPLY" | grep -q "llmsm_rewire_scaledobjects" && printf "%s" "$APPLY" | grep -q "DRY_RUN=false"' "scaledobjects-apply does not re-wire: $APPLY"
ok 'printf "%s\n" "$MK" | grep -q "^scaledobjects-rewire: ## " && printf "%s" "$REWIRE" | grep -q "llmsm_rewire_scaledobjects" && printf "%s" "$REWIRE" | grep -q "DRY_RUN=\$(DRY_RUN)"' "scaledobjects-rewire missing, without help, or not passing DRY_RUN: $REWIRE"

echo ""
echo "scaledobject re-wire: $CASES cases, $PASS assertions passed, $FAIL failed"
if [ "$CASES" != "$EXPECTED_CASES" ]; then
    echo "FAIL: ran $CASES cases, expected $EXPECTED_CASES"
    exit 1
fi
if [ "$FAIL" -gt 0 ]; then
    exit 1
fi
if [ "$PASS" = 0 ]; then
    echo "FAIL: no assertion ran"
    exit 1
fi
echo "scaledobject re-wire OK ($CASES cases)"
