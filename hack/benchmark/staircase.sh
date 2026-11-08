#!/usr/bin/env bash
#
# staircase.sh -- the TTFT staircase, end to end, on a fixed P/D fleet.
#
#   staircase.sh smoke <image> a short run, 3 GPUs at most: everything a real API server
#                              has to accept, then a deliberate abort and a teardown check
#   staircase.sh lapse <image> a short run whose runner is killed with -9, 2 GPUs: the
#                              deadline Job must delete the workloads on its own
#   staircase.sh run <image>   stand up, warm up, wait for the freeze, run every phase
#   staircase.sh freeze        the prediction and the ladder, from the warm-up's recording
#   staircase.sh analyze       one analysis.jsonl per phase, then report.md
#   staircase.sh redo          re-price at each bucket's measured Ca2, targets held
#   staircase.sh teardown      free everything this run owns, from any state
#   staircase.sh status        what the run has done, and what holds GPUs
#   staircase.sh abort         stop a run: its own exit path tears down
#
# Set BENCHMARK_NAMESPACE (a fresh one: run refuses one that exists), STAIRCASE_RUN
# (the run's directory, absolute: run refuses one a run has used) and
# BENCHMARK_PROMETHEUS_URL. Optional: STAIRCASE_MODEL, STAIRCASE_ACCELERATOR_KEY and
# _VALUE (a node label the engines select), STAIRCASE_DECODE_CPU and _MEMORY,
# STAIRCASE_MAX_MODEL_LEN, STAIRCASE_LOAD_IMAGE, STAIRCASE_POD_HEADER, STAIRCASE_SLO_TTFT.
#
# THE SAFETY THIS CARRIES, each paid for on a shared GPU cluster:
#   - An in-cluster Job deletes the workloads when the lease lapses, so a dead runner
#     frees the GPUs (staircase_deadline.py). Armed before the standup, and checked
#     while running: a Job that stops, or a hard cap that nears, aborts the run.
#   - The lease is renewed only while the main shell beats. Kill it, even with -9, and
#     renewal stops at once: the GPUs go at the last renewal plus STAIRCASE_LEASE.
#   - One EXIT trap tears down on every exit path that can run one. It runs from a
#     private copy of this directory, so an edit under a long run changes nothing.
#   - TERM and abort land within one kubectl read: KUBE_TIMEOUT + 10 s, 55 s by default,
#     and at once when no read is hung. Every kubectl is under timeout(1); each long one,
#     the gate's scoring and every wait run guarded, so a signal need not wait for them.
#   - teardown deletes only what this run owns (owned, which tells a mismatch from a
#     failed read and retries the read), and returns non-zero unless the workloads are
#     confirmed gone. The Job stays until they are.
#   - A saved capture is replaced only by a larger one (keep_larger), and the load is
#     copied out on every poll and again before its pod is deleted.
#   - No step runs unless the ladder and the frozen prediction match the hashes taken
#     when they were written (verify_frozen), and repeat 1 of the gate buckets is scored
#     in the loop: a fail stops the run and frees the GPUs (gate_check).
#   - Engine errors are read from the logs: a wrapper shell outlives a dead engine, and
#     the pod stays Running.

set -u

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="${STAIRCASE_ROOT:-$(cd "$HERE/../.." && pwd)}"
PY="${STAIRCASE_PY:-$(command -v python3 || command -v python)}" || { echo "python3 not found" >&2; exit 1; }

NS="${BENCHMARK_NAMESPACE:-}"
R="${STAIRCASE_RUN:-}"
MODEL="${STAIRCASE_MODEL:-Qwen/Qwen3-0.6B}"
SPEC="${STAIRCASE_SPEC:-$HERE/staircase_spec.yaml}"
LOAD_IMAGE="${STAIRCASE_LOAD_IMAGE:-ghcr.io/llm-d/llm-d-benchmark:v0.7.8}"
PROM="${BENCHMARK_PROMETHEUS_URL:-}"
SLO_TTFT="${STAIRCASE_SLO_TTFT:-200ms}"
SCENARIO="${BENCHMARK_SPEC:-guides/pd-disaggregation}"
WHATIF="${STAIRCASE_WHATIF:-$ROOT/bin/whatif}"
# The longest held-out prompt, its spread and the answer must fit.
MAX_MODEL_LEN="${STAIRCASE_MAX_MODEL_LEN:-40960}"
# The fleet a standup makes. The smoke run asks for the smallest.
START_P="${STAIRCASE_START_PREFILL:-2}"
START_D="${STAIRCASE_START_DECODE:-2}"
# Seconds: the hard cap on a whole run, the lease a renewal extends, how often it is
# renewed, how stale a heartbeat stops renewal, the wait for a freeze, a fleet change,
# a poll, a tick of a guarded wait, one kubectl.
HARD_CAP="${STAIRCASE_HARD_CAP:-345600}"
LEASE="${STAIRCASE_LEASE:-2400}"
LEASE_EVERY="${STAIRCASE_LEASE_EVERY:-120}"
HEART_MAX="${STAIRCASE_HEART_MAX:-300}"
FREEZE_WAIT="${STAIRCASE_FREEZE_WAIT:-9000}"
READY_WAIT="${STAIRCASE_READY_WAIT:-1200}"
POLL="${STAIRCASE_POLL:-20}"
TICK="${STAIRCASE_TICK:-1}"
KUBE_TIMEOUT="${STAIRCASE_KUBE_TIMEOUT:-45}"
ENGERR='hit an exception|Engine core initialization failed|ZeroDivisionError|Traceback'
RECORDINGS=/var/lib/scaling-manager/learner-recordings
RUN_LABEL=llm-d-scaling-manager/staircase-run

die() { echo "staircase: $*" >&2; exit 1; }
need() {
    [ -n "$NS" ] && [ -n "$R" ] || die "set BENCHMARK_NAMESPACE and STAIRCASE_RUN"
    case "$R" in /*) ;; *) die "STAIRCASE_RUN must be absolute: $R" ;; esac
    case "$NS" in default|kube-*|openshift-*) die "refusing namespace $NS: a staircase runs in a fresh one" ;; esac
}
t() { echo "$(date -u +%FT%TZ) $*" >> "$R/timeline.txt"; }

# Every call is bounded. --request-timeout does not bound exec or cp, so timeout(1) is
# on all of them, -k for a kubectl that ignores the TERM.
kube() { timeout -k 5 $((KUBE_TIMEOUT + 5)) kubectl --request-timeout="${KUBE_TIMEOUT}s" -n "$NS" "$@"; }
# kstream <seconds> <args> : a kubectl that streams: exec, cp, rollout status, wait.
# gstream is the same under guarded: an external command, not a function, so that the
# kill in guarded reaches timeout and not a subshell waiting for it.
kstream() { local s=$1; shift; timeout -k 10 "$s" kubectl -n "$NS" "$@"; }

gstream() { local s=$1; shift; guarded timeout -k 10 "$s" kubectl -n "$NS" "$@"; }

# aborted : the run was asked to stop. A teardown is what that asks for, and reads no abort.
aborted() { [ -z "${TEARDOWN:-}" ] && [ -e "$R/abort" ]; }
# beat : the main shell is alive. The capture loop renews the lease only on this.
beat() { : > "$R/heartbeat"; }

# guarded <command...> : run it in the background and wait a tick at a time: a trapped
# signal waits for a foreground child, so this is what makes TERM land at once. Beats,
# and stops the command on abort.
guarded() {
    local rc
    "$@" &
    CHILD=$!
    # Reads abort before the first tick, too: a command started after an abort stops.
    while kill -0 "$CHILD" 2>/dev/null; do
        beat
        if aborted; then kill "$CHILD" 2>/dev/null; break; fi
        sleep "$TICK"
    done
    wait "$CHILD"
    rc=$?
    CHILD=
    return "$rc"
}

# nap <seconds> : a sleep a signal or an abort need not wait out.
nap() { guarded sleep "$1"; }

# keep_larger <new> <saved> : a capture may never shrink the stored copy. A dead pod's
# NotFound once overwrote 400 lines of failure context.
keep_larger() {
    local new old=0
    [ -e "$1" ] || return 0
    new=$(wc -c < "$1")
    # Test first: a redirect that fails reports before any 2>/dev/null after it applies,
    # and the first save of a file always finds none.
    [ -e "$2" ] && old=$(wc -c < "$2")
    if [ "$new" -gt "$old" ]; then mv "$1" "$2"; else rm -f "$1"; fi
}

# running_pods [regex] : the Running pods, none being deleted.
running_pods() {
    kube get pods -o json 2>/dev/null | jq -r '.items[] | select(.metadata.deletionTimestamp == null)
        | select(.status.phase=="Running") | .metadata.name' | grep -E -- "${1:-.}"
}

names() { if [ -s "$R/names.sh" ]; then . "$R/names.sh"; fi; }

# --- ownership and the deadline, before anything else exists ---------------------------
# claim : mark the namespace as this run's: the lease holds the run's id, and the
# namespace a label with it. owned reads either: the harness may delete ConfigMaps.
claim() {
    local id lease
    id="$(date +%s)-$$-$RANDOM"
    echo "$id" > "$R/run-id"
    lease=$(($(date +%s) + 5400))
    kube create configmap staircase-lease --from-literal=until="$lease" --from-literal=run="$id" >/dev/null || return 1
    timeout -k 5 $((KUBE_TIMEOUT + 5)) kubectl --request-timeout="${KUBE_TIMEOUT}s" label namespace "$NS" "$RUN_LABEL=$id" >/dev/null \
        || t "namespace label failed: the lease alone marks ownership"
}

# owned : 0 when this run's id is on the namespace, by the lease or the label; 1 when
# both were read and neither holds it; 2 when they could not be read. A failed read is
# not a mismatch: at exit it would cancel the teardown, so it is retried.
owned() {
    local id out i read
    id="${STAIRCASE_RUN_ID:-$(cat "$R/run-id" 2>/dev/null)}"
    [ -n "$id" ] || return 1
    for i in $(seq 1 "${STAIRCASE_OWNED_TRIES:-6}"); do
        read=0
        if out=$(kube get configmap staircase-lease --ignore-not-found -o json 2>/dev/null); then
            read=$((read + 1))
            [ "$(echo "$out" | jq -r '.data.run // empty' 2>/dev/null)" = "$id" ] && return 0
        fi
        if out=$(timeout -k 5 $((KUBE_TIMEOUT + 5)) kubectl --request-timeout="${KUBE_TIMEOUT}s" get namespace "$NS" --ignore-not-found -o json 2>/dev/null); then
            read=$((read + 1))
            [ "$(echo "$out" | jq -r --arg l "$RUN_LABEL" '.metadata.labels[$l] // empty' 2>/dev/null)" = "$id" ] && return 0
        fi
        [ "$read" = 2 ] && return 1
        sleep "${STAIRCASE_OWNED_POLL:-5}"
    done
    return 2
}

arm_deadline() {
    local hard i
    hard=$(($(date +%s) + HARD_CAP))
    echo "$hard" > "$R/deadline-epoch.txt"
    kube create configmap staircase-deadline --from-file=staircase_deadline.py="$HERE/staircase_deadline.py" >/dev/null || return 1
    sed -e "s/__DEADLINE_EPOCH__/$hard/" -e "s/__NS__/$NS/" -e "s#__IMAGE__#$LOAD_IMAGE#" \
        "$HERE/staircase_deadline.yaml" > "$R/deadline.yaml"
    if grep -q '__[A-Z_]*__' "$R/deadline.yaml"; then t "deadline.yaml has an unfilled placeholder"; return 1; fi
    kube apply -f "$R/deadline.yaml" >/dev/null || return 1
    for i in $(seq 1 "${STAIRCASE_ARM_TRIES:-40}"); do
        kube logs job/staircase-deadline > "$R/deadline-start.log" 2>/dev/null
        if grep -q 'deadline at' "$R/deadline-start.log"; then t "deadline armed: $(cat "$R/deadline-start.log")"; return 0; fi
        aborted && return 1
        sleep "${STAIRCASE_ARM_POLL:-6}"
    done
    t "deadline job not running"
    return 1
}

renew_lease() {
    kube patch cm staircase-lease --type merge -p "{\"data\":{\"until\":\"$(($(date +%s) + LEASE))\"}}" >/dev/null 2>"$R/lease.err" \
        || echo "$(date -u +%FT%TZ) lease patch failed: $(head -1 "$R/lease.err")" >> "$R/lease-failures.log"
}

# --- standup ---------------------------------------------------------------------------
# standup_make : exec, so that kill reaches timeout, which passes the TERM on to make.
standup_make() {
    local set="prefill.replicas=$START_P,decode.replicas=$START_D,model.maxModelLen=$MAX_MODEL_LEN"
    if [ -n "${STAIRCASE_ACCELERATOR_KEY:-}" ]; then
        set="$set,decode.acceleratorType.labelKey=$STAIRCASE_ACCELERATOR_KEY,decode.acceleratorType.labelValue=${STAIRCASE_ACCELERATOR_VALUE:-}"
        set="$set,prefill.acceleratorType.labelKey=$STAIRCASE_ACCELERATOR_KEY,prefill.acceleratorType.labelValue=${STAIRCASE_ACCELERATOR_VALUE:-}"
    fi
    cd "$ROOT" || exit 1
    LLMDBENCH_SET="$set" exec timeout --signal=TERM -k 30 2700 make --no-print-directory benchmark-standup \
        BENCHMARK_NAMESPACE="$NS" BENCHMARK_SPEC="$SCENARIO" MODEL_ID="$MODEL" IMG="$1" \
        BENCHMARK_PROMETHEUS_URL="$PROM" BENCHMARK_DECODE_REPLICAS="$START_D" \
        BENCHMARK_DECODE_CPU="${STAIRCASE_DECODE_CPU:-8}" BENCHMARK_DECODE_MEMORY="${STAIRCASE_DECODE_MEMORY:-32Gi}" \
        BENCHMARK_KEDA_MIN_REPLICAS="$START_D" BENCHMARK_KEDA_MAX_REPLICAS="$START_D"
}

# prepare : read the names, keep the originals, list slo, flow control off.
prepare() {
    local dec pre epp cm router d len
    dec=$(kube get deploy -o name | sed 's#.*/##' | grep -- '-decode$')
    pre=$(kube get deploy -o name | sed 's#.*/##' | grep -- '-prefill$')
    epp=$(kube get deploy -o name | sed 's#.*/##' | grep -- '-epp$')
    if [ "$(echo "$dec" | grep -c .)" != 1 ] || [ "$(echo "$pre" | grep -c .)" != 1 ] || [ "$(echo "$epp" | grep -c .)" != 1 ]; then
        t "names: decode='$dec' prefill='$pre' epp='$epp'; want one each"; return 1
    fi
    cm=$(kube get cm -o json | jq -r '.items[] | select(.data["pd-config.yaml"] != null) | .metadata.name')
    [ "$(echo "$cm" | grep -c .)" = 1 ] || { t "EPP ConfigMap with pd-config.yaml: '$cm'"; return 1; }
    router=$(kube get svc -o json | jq -r --arg e "$epp" '.items[] | select(.metadata.name == $e) | select([.spec.ports[] | .port] | index(80)) | .metadata.name')
    [ -n "$router" ] || { t "no Service $epp with port 80"; return 1; }
    {
        echo "DEC=$dec"; echo "PRE=$pre"; echo "EPPD=$epp"; echo "EPPCM=$cm"
        echo "ROUTER=http://$router.$NS.svc:80"
    } > "$R/names.sh"
    t "names: $(tr '\n' ' ' < "$R/names.sh")"
    for d in "$dec" "$pre"; do
        len=$(kube get deploy "$d" -o json | jq -r '.spec.template.spec.containers[] | select(.name=="vllm") | .env[] | select(.name=="VLLM_MAX_MODEL_LEN") | .value')
        [ "$len" = "$MAX_MODEL_LEN" ] || { t "$d VLLM_MAX_MODEL_LEN is '$len', not $MAX_MODEL_LEN"; return 1; }
    done
    kube get cm "$cm" -o json | jq -r '.data["pd-config.yaml"]' > "$R/pd-config-on.yaml"
    "$PY" "$HERE/staircase_config.py" fc-off "$R/pd-config-on.yaml" "$R/pd-config-off.yaml" || { t "fc-off refused"; return 1; }
    kube get configmap scaling-manager-scaling-policy-config -o json | jq -r '.data.default' > "$R/policy-orig.yaml"
    "$PY" "$HERE/staircase_config.py" list-slo "$R/policy-orig.yaml" "$R/policy.yaml" "$SLO_TTFT" || { t "list-slo refused"; return 1; }
    jq -n --rawfile d "$R/policy.yaml" '{data: {default: $d}}' > "$R/policy-patch.json"
    kube patch configmap scaling-manager-scaling-policy-config --type merge --patch-file "$R/policy-patch.json" >/dev/null || { t "policy patch failed"; return 1; }
    t "slo listed, TTFT target $SLO_TTFT, shadow"
}

# pin_fleet <prefill> <decode> : ScaledObjects min = max, then wait for Ready, capped,
# reading the engines' logs: a crashed engine presents as a pod that never gets Ready.
pin_fleet() {
    local np=$1 nd=$2 so sos target want a b end p pinned=0
    names
    # A read that fails must not read as nothing to pin: the fleet would stay as it was.
    sos=$(kube get scaledobject -o json) || { t "ScaledObjects unreadable"; return 1; }
    for so in $(echo "$sos" | jq -r '.items[] | "\(.metadata.name)=\(.spec.scaleTargetRef.name)"'); do
        target=${so#*=}; so=${so%=*}
        case "$target" in
            "$DEC") want=$nd ;;
            "$PRE") want=$np ;;
            *) continue ;;
        esac
        kube patch scaledobject "$so" --type merge -p "{\"spec\":{\"minReplicaCount\":$want,\"maxReplicaCount\":$want}}" >/dev/null \
            || { t "ScaledObject $so patch failed"; return 1; }
        pinned=$((pinned + 1))
    done
    [ "$pinned" = 2 ] || { t "pinned $pinned ScaledObjects, want one per role"; return 1; }
    end=$(($(date +%s) + READY_WAIT))
    while [ "$(date +%s)" -lt "$end" ]; do
        beat
        aborted && { t "abort during pin_fleet"; return 1; }
        a=$(kube get deploy "$DEC" -o jsonpath='{.status.readyReplicas}')
        aborted && { t "abort during pin_fleet"; return 1; }
        b=$(kube get deploy "$PRE" -o jsonpath='{.status.readyReplicas}')
        aborted && { t "abort during pin_fleet"; return 1; }
        if [ "${a:-0}" = "$nd" ] && [ "${b:-0}" = "$np" ]; then t "fleet prefill=$b decode=$a"; return 0; fi
        for p in $(running_pods "^($DEC|$PRE)-"); do
            kube logs "$p" -c vllm --tail=300 2>/dev/null | grep -m1 -E "$ENGERR" | sed "s#^#$p: #" >> "$R/engine-errors.txt"
        done
        [ -s "$R/engine-errors.txt" ] && { t "engine error: $(head -3 "$R/engine-errors.txt" | tr '\n' ' ')"; return 1; }
        nap "$POLL"
    done
    t "fleet not Ready in ${READY_WAIT}s: prefill=${b:-0} decode=${a:-0}"
    return 1
}

# flow_control on|off : set the router's config and restart it; refuse any other state.
flow_control() {
    local f="$R/pd-config-$1.yaml" got
    names
    [ -s "$f" ] || { t "flow_control: $f missing"; return 1; }
    jq -n --rawfile d "$f" '{data: {"pd-config.yaml": $d}}' > "$R/epp-patch-$1.json"
    kube patch cm "$EPPCM" --type merge --patch-file "$R/epp-patch-$1.json" >/dev/null || { t "EPP config patch failed"; return 1; }
    got=$(kube get cm "$EPPCM" -o json | jq -r '.data["pd-config.yaml"]' | grep -cE '^(featureGates|flowControl):')
    if { [ "$1" = on ] && [ "$got" != 2 ]; } || { [ "$1" = off ] && [ "$got" != 0 ]; }; then
        t "flow control $1: the config has $got flow-control keys"; return 1
    fi
    kube rollout restart "deploy/$EPPD" >/dev/null || { t "EPP restart failed"; return 1; }
    gstream 330 rollout status "deploy/$EPPD" --timeout=300s >/dev/null || { t "EPP rollout failed"; return 1; }
    nap "${STAIRCASE_SETTLE:-20}"
    t "flow control $1"
}

# recorder_on : the recorder, as the last change before load.
recorder_on() {
    kube patch deployment scaling-manager-controller-manager --type strategic \
        --patch-file "$ROOT/config/components/learner-recorder/learner-recorder-deployment.yaml" >/dev/null \
        || { t "recorder patch failed"; return 1; }
    gstream 330 rollout status deployment/scaling-manager-controller-manager --timeout=300s >/dev/null || { t "recorder rollout failed"; return 1; }
    controller_pod || return 1
    echo "$CTL" > "$R/pod1.txt"
    nap "${STAIRCASE_SETTLE:-20}"
    gstream 120 exec "$(cat "$R/pod1.txt")" -c recordings -- sh -c "head -1 $RECORDINGS/learnrec-*.jsonl" > "$R/start-line.json" 2>&1
    [ "$(jq -r .Kind "$R/start-line.json" 2>/dev/null)" = start ] || { t "no recording start line: $(head -c 200 "$R/start-line.json")"; return 1; }
    t "recorder on, commit $(jq -r .Data.Commit "$R/start-line.json")"
}

# controller_pod : set CTL to the one Running controller, waiting out a rollout, capped.
# Not in a subshell: that would hold a signal for the whole wait.
controller_pod() {
    local end
    end=$(($(date +%s) + 180))
    while :; do
        beat
        aborted && return 1
        CTL=$(kube get pod -l control-plane=controller-manager -o json | jq -r '[.items[] | select(.metadata.deletionTimestamp == null) | select(.status.phase=="Running")] | .[].metadata.name')
        [ "$(echo "$CTL" | grep -c .)" = 1 ] && return 0
        [ "$(date +%s)" -ge "$end" ] && { t "controller pods after 180 s: '$CTL'"; return 1; }
        nap "$POLL"
    done
}

# copy_recordings <pod> <dir> : never shrink a saved recording.
copy_recordings() {
    local tmp="$2.tmp.$$" f
    mkdir -p "$2"; rm -rf "$tmp"
    gstream 300 cp -c recordings "$1:$RECORDINGS" "$tmp" >> "$R/copyrec.log" 2>&1
    for f in "$tmp"/*; do
        [ -f "$f" ] && keep_larger "$f" "$2/$(basename "$f")"
    done
    rm -rf "$tmp"
    kube logs --timestamps "$1" -c manager > "$2.log.tmp" 2>/dev/null
    keep_larger "$2.log.tmp" "$2.log"
}

# --- witnesses -------------------------------------------------------------------------
# witness : a pod that reaches Prometheus and reads the EPP's /metrics with the token the
# controller scrapes it with. The EPP's metrics are what the run reads the router's
# histograms and its flow control from, so a standup that cannot read them stops here.
# Through the API server's pod proxy they never could: the API server strips the
# caller's credentials before it proxies, and the EPP answers 401 to a request with
# none. r1 ran its whole warm-up and gate that way, on Prometheus alone.
witness() {
    local secret target epod ip port out
    names
    # The Secret the controller mounts at /var/run/secrets/epp-metrics: its token is the
    # one the EPP is known to accept.
    secret=$(kube get deploy scaling-manager-controller-manager -o json 2>/dev/null \
        | jq -r '.spec.template.spec.volumes[]? | select(.name == "epp-metrics-token") | .secret.secretName // empty')
    [ -n "$secret" ] || { t "the controller mounts no epp-metrics-token Secret: the EPP cannot be read"; return 1; }
    kube create configmap staircase-witness --from-file=staircase_promq.py="$HERE/staircase_promq.py" >/dev/null \
        || { t "witness configmap failed"; return 1; }
    pod_yaml witness witness staircase-witness /w "sleep 86400" 100m 128Mi 512Mi "$secret" > "$R/witness.yaml"
    kube apply -f "$R/witness.yaml" >> "$R/timeline.txt" 2>&1 || { t "witness apply failed"; return 1; }
    gstream 200 wait pod witness --for=condition=Ready --timeout=180s >> "$R/timeline.txt" 2>&1 || { t "witness pod not Ready"; return 1; }
    target=$(epp_pod)
    [ -n "$target" ] || { t "no Running EPP pod: the EPP cannot be read"; return 1; }
    read -r epod ip _ <<< "$target"
    for port in ${target#"$epod $ip"} 9090; do
        if out=$(epp_scrape "$ip" "$port" 2>&1) && echo "$out" | grep -q '^llm_d_epp_\|^# HELP llm_d_epp_'; then
            echo "$port" > "$R/epp-scrape.txt"; t "EPP metrics readable at port $port of $epod"; return 0
        fi
        t "EPP $epod port $port: $(echo "$out" | tail -1 | cut -c1-200)"
    done
    t "EPP metrics not readable with the controller's token: stopping"
    return 1
}

# epp_pod : "<pod> <pod IP> <container ports...>" of a Running EPP pod; empty with none. Read
# again on every use: flow control restarts the EPP each phase, and its IP moves.
epp_pod() {
    kube get pods -o json 2>/dev/null | jq -r --arg p "$EPPD-" '[.items[] | select(.metadata.deletionTimestamp == null)
        | select(.status.phase == "Running") | select(.metadata.name | startswith($p))] | first // empty
        | "\(.metadata.name) \(.status.podIP) \([.spec.containers[].ports[]?.containerPort | tostring] | join(" "))"'
}

# epp_scrape <pod IP> <port> : the EPP's /metrics, read from the witness with the token.
epp_scrape() {
    local host=$1
    case $host in *:*) host="[$host]" ;; esac
    kube exec witness -- python3 /w/staircase_promq.py scrape "http://$host:$2/metrics" /t/token
}

# pod_yaml <name> <label> <configmap> <mount> <command> <cpu> <mem request> <mem limit> [secret]
# A secret, when named, is mounted read-only at /t.
pod_yaml() {
    printf '%s\n' "apiVersion: v1" "kind: Pod" "metadata:" "  name: $1" "  namespace: $NS" "  labels: {app: $2}" \
        "spec:" "  restartPolicy: Never" "  containers:" "  - name: main" "    image: $LOAD_IMAGE" \
        "    command: [sh, -c, '$5']" "    resources:" "      requests: {cpu: \"$6\", memory: $7}" \
        "      limits: {memory: $8}" "    volumeMounts:" "    - {mountPath: $4, name: code, readOnly: true}" \
        "    - {mountPath: /out, name: out}"
    [ -n "${9:-}" ] && printf '%s\n' "    - {mountPath: /t, name: token, readOnly: true}"
    printf '%s\n' "  volumes:" "  - name: out" "    emptyDir: {}" "  - name: code" "    configMap: {name: $3}"
    [ -n "${9:-}" ] && printf '%s\n' "  - name: token" "    secret: {secretName: $9}"
    return 0
}

# job_check : the deadline Job is what frees the GPUs if this dies. If it has stopped,
# or the hard cap nears, stop the run while it can still tear down.
job_check() {
    local active hard
    hard=$(cat "$R/deadline-epoch.txt" 2>/dev/null || echo 0)
    if [ "$(date +%s)" -ge $((hard - ${STAIRCASE_CAP_MARGIN:-900})) ]; then
        t "the hard cap is near: abort"; touch "$R/abort"; return
    fi
    active=$(kube get job staircase-deadline -o jsonpath='{.status.active}' 2>/dev/null) || return 0
    if [ "$active" = 1 ]; then JOB_MISSES=0; return; fi
    JOB_MISSES=$((JOB_MISSES + 1))
    if [ "$JOB_MISSES" -ge 3 ]; then t "the deadline Job is not active: abort"; touch "$R/abort"; fi
}

# capture_loop <main pid> : poll until STOP: lease, replicas, engine metrics and errors,
# EPP metrics, controller log. Every file is append-only or replaced only by a larger
# copy. The lease is renewed only while the main shell beats, and the loop ends with the
# main shell: a helper that outlives its run must not keep the GPUs.
capture_loop() {
    local main=$1 ts p port epod ip now leased=0 age
    JOB_MISSES=0
    names
    while [ ! -e "$R/STOP" ]; do
        kill -0 "$main" 2>/dev/null || { t "capture: the main shell is gone; the lease lapses"; return 0; }
        ts=$(date -u +%FT%TZ); now=$(date +%s)
        if [ $((now - leased)) -ge "$LEASE_EVERY" ]; then
            leased=$now
            age=$((now - $(date -r "$R/heartbeat" +%s 2>/dev/null || echo 0)))
            if [ "$age" -lt "$HEART_MAX" ]; then renew_lease; else t "capture: heartbeat ${age}s old: lease not renewed"; fi
            job_check
        fi
        for p in "$DEC" "$PRE"; do
            echo "$ts $p $(kube get deploy "$p" -o jsonpath='{.spec.replicas} {.status.readyReplicas}' 2>/dev/null)" >> "$R/replicas.log"
        done
        for p in $(running_pods "^($DEC|$PRE)-"); do
            port=8200; case $p in "$PRE"-*) port=8000 ;; esac
            kube get --raw "/api/v1/namespaces/$NS/pods/$p:$port/proxy/metrics" 2>/dev/null \
                | grep -E '^vllm:(kv_cache_usage_perc|num_requests_waiting|num_requests_running|num_preemptions_total|prompt_tokens_total|generation_tokens_total|request_success_total|request_prefill_time_seconds_(sum|count)|request_queue_time_seconds_(sum|count)|time_to_first_token_seconds_(sum|count))' \
                | sed "s#^#$ts $p #" >> "$R/vllm-metrics.log"
            kube logs "$p" -c vllm --since=70s 2>/dev/null | grep -E "$ENGERR" | head -3 | sed "s#^#$ts $p #" >> "$R/engine-errors.txt"
        done
        if [ -s "$R/epp-scrape.txt" ]; then
            read -r epod ip _ <<< "$(epp_pod)"
            [ -n "$epod" ] && epp_scrape "$ip" "$(cat "$R/epp-scrape.txt")" 2>/dev/null \
                | grep -E '^llm_d_epp_(request_ttft_seconds|request_streaming_tpot_seconds|flow_control_)' \
                | sed "s#^#$ts $epod #" >> "$R/epp-metrics.log"
        fi
        sleep "$POLL"
    done
}

# --- load --------------------------------------------------------------------------------
# snapshot_load <dir> <pod> : log and per-request CSVs to local files, never shrinking.
snapshot_load() {
    kube logs "$2" > "$1/log.tmp" 2>/dev/null; keep_larger "$1/log.tmp" "$1/sweep.log"
    # A copy cut short is a broken archive, however long: it replaces nothing.
    if gstream 120 exec "$2" -- sh -c 'cat /out/w*.csv 2>/dev/null | gzip -1' > "$1/req.tmp" 2>/dev/null; then
        keep_larger "$1/req.tmp" "$1/requests.csv.gz"
    else
        rm -f "$1/req.tmp"
    fi
}

# load <dir> <pod> <open|closed> <cap seconds> <driver args...> : one load pod to SWEEP
# DONE. Saved on every poll, so nothing depends on the pod living.
load() {
    local d=$1 p=$2 kind=$3 cap=$4 cmd end i rc=1 ph
    shift 4
    names
    mkdir -p "$d"
    case $kind in
        open)
            cmd="python3 -u /code/staircase_load.py --url $ROUTER --model $MODEL --out /out"
            [ -n "${STAIRCASE_POD_HEADER:-}" ] && cmd="$cmd --pod-header $STAIRCASE_POD_HEADER"
            ;;
        closed) cmd="python3 -u /code/closed_loop_sweep.py --url $ROUTER --model $MODEL --stream --max-inflight 1024" ;;
        *) t "load: kind $kind"; return 1 ;;
    esac
    pod_yaml "$p" loadgen staircase-loadgen /code \
        "python3 -c \"import aiohttp\" 2>/dev/null || pip install -q aiohttp; $cmd $*; sleep 7200" 8 8Gi 16Gi > "$d/load.yaml"
    echo "$d" > "$R/load-dir-$p"
    kube apply -f "$d/load.yaml" >> "$R/timeline.txt" 2>&1 || { t "$p apply failed"; return 1; }
    for i in $(seq 1 60); do
        beat
        aborted && break
        kube logs "$p" 2>/dev/null | grep -q ' LEVEL 1 ' && break
        nap "$POLL"
    done
    if ! kube logs "$p" 2>/dev/null | grep -q ' LEVEL 1 '; then
        kube delete pod "$p" --ignore-not-found --grace-period=1 >/dev/null 2>&1
        t "$p did not start"; return 1
    fi
    end=$(($(date +%s) + cap))
    while [ "$(date +%s)" -lt "$end" ]; do
        beat
        # On abort the pod stays: teardown saves it, then deletes it (rescue_load).
        if aborted; then t "$p: abort"; return 1; fi
        snapshot_load "$d" "$p"
        if aborted; then t "$p: abort"; return 1; fi
        if grep -q 'SWEEP DONE' "$d/sweep.log" 2>/dev/null; then rc=0; break; fi
        ph=$(kube get pod "$p" -o jsonpath='{.status.phase}' 2>/dev/null)
        case "$ph" in Succeeded|Failed) t "$p phase $ph"; break ;; esac
        if [ -s "$R/engine-errors.txt" ]; then t "$p: engine errors"; break; fi
        nap "$POLL"
    done
    snapshot_load "$d" "$p"
    kube delete pod "$p" --ignore-not-found --grace-period=1 --timeout=40s >/dev/null 2>&1
    rm -f "$R/load-dir-$p"
    t "$p end rc=$rc: $(grep -E 'RUNAWAY|SWEEP DONE' "$d/sweep.log" 2>/dev/null | tail -2 | tr '\n' ' ' | cut -c1-240)"
    return $rc
}

# schedule_seconds <schedule> : the sum of its holds.
schedule_seconds() { echo "$1" | tr ',' '\n' | awk -F: '{s += $1} END {print s}'; }

# prom_range <from epoch> <to epoch> <out>
prom_range() {
    gstream 200 exec witness -- python3 /w/staircase_promq.py "$PROM" "$NS" "$1" "$2" > "$3.tmp" 2>/dev/null
    keep_larger "$3.tmp" "$3"
}

# warmup : the recording the prediction is made from, flow control off, slo listed from
# its first cycle. The order and the load are the spec's: what each stage teaches, and why
# V_P comes last, is in the plan.
warmup() {
    local d="$R/warmup" pod from stage driver sched seed cap
    mkdir -p "$d"
    pod=$(cat "$R/pod1.txt"); from=$(date +%s)
    for stage in $(jq -r '.warmup.order[]' "$R/spec.json"); do
        driver=$(jq -r ".warmup.$stage.driver" "$R/spec.json")
        sched=$(jq -r ".warmup.$stage.schedule" "$R/spec.json")
        seed=$(jq -r ".warmup.$stage.seed // 11" "$R/spec.json")
        cap=$(( $(schedule_seconds "$sched") + 900 ))
        t "warmup $stage"
        if [ "$driver" = open ]; then
            load "$d/$stage" "w-$stage" open "$cap" --schedule "$sched" --seed "$seed" --procs 2 --request-timeout 120 || return 1
        else
            load "$d/$stage" "w-$stage" closed "$cap" --schedule "$sched" || return 1
        fi
        copy_recordings "$pod" "$R/recordings-warmup"
    done
    nap "${STAIRCASE_SETTLE:-20}"
    copy_recordings "$pod" "$R/recordings-warmup"
    prom_range "$from" "$(date +%s)" "$d/prom.jsonl"
    t "warmup done: $(ls -l "$R/recordings-warmup" | awk 'NR>1 {print $NF"="$5}' | tr '\n' ' ')"
}

# --- the freeze ------------------------------------------------------------------------
# largest_recording : the warm-up's recording.
largest_recording() {
    # shellcheck disable=SC2012 # the largest recording: the sizes hold no odd names
    ls -S "$R"/recordings-warmup/*.jsonl 2>/dev/null | head -1
}

# refuse_freeze <why> : a run waiting on this freeze would hold its GPUs for nothing.
refuse_freeze() {
    touch "$R/abort"
    die "$1: the run aborts"
}

# freeze : run once, after the warm-up and before any step. A file that exists is never
# replaced, a refused bucket or a failed gate stops the run, and the hashes come last.
cmd_freeze() {
    need
    local rec
    [ -e "$R/ladder.ready" ] && die "already frozen: $R/ladder.ready"
    [ -d "$R/phases" ] && die "a phase has run: a prediction after a step is not a prediction"
    rec=$(largest_recording)
    [ -n "$rec" ] || die "no warm-up recording under $R/recordings-warmup"
    [ -x "$WHATIF" ] || die "$WHATIF: build it with make whatif"
    "$WHATIF" freeze --recording "$rec" --spec "$R/spec.yaml" -o "$R/frozen.json" || refuse_freeze "whatif freeze refused"
    "$PY" "$HERE/staircase_freeze.py" ladder "$R/frozen.json" "$R/spec.json" "$R/ladder.txt" "$R/spec.yaml" || refuse_freeze "no ladder"
    "$PY" "$HERE/staircase_freeze.py" plan "$R/frozen.json" "$R/spec.json" | tee "$R/plan.md"
    (cd "$R" && sha256sum frozen.json ladder.txt spec.yaml spec.json > frozen.sha256) || refuse_freeze "hashing failed"
    touch "$R/ladder.ready"
    t "frozen: $(tr '\n' ' ' < "$R/frozen.sha256" | cut -c1-200)"
}

# verify_frozen : no step runs, and no report is read, on a prediction or a band that moved.
verify_frozen() {
    (cd "$R" && sha256sum --check --status frozen.sha256) || { t "the frozen files do not match their hashes"; return 1; }
}

# --- a phase -----------------------------------------------------------------------------
# phase <name> <fc> <schedule> <runaway> <seed> <prefill> <decode> : flow control and the
# fleet, a fresh controller so the phase has a recording of its own, the load, the captures.
phase() {
    local n=$1 fc=$2 sched=$3 run=$4 seed=$5 np=$6 nd=$7 d pod hold rc from to
    d="$R/phases/$n"; mkdir -p "$d"
    [ -e "$d/done" ] && { t "phase $n already done"; return 0; }
    verify_frozen || return 1
    t "phase $n start fc=$fc runaway=$run seed=$seed fleet=$np+$nd"
    pin_fleet "$np" "$nd" || return 1
    flow_control "$fc" || return 1
    kube rollout restart deployment/scaling-manager-controller-manager >/dev/null
    gstream 330 rollout status deployment/scaling-manager-controller-manager --timeout=300s >/dev/null || { t "controller rollout failed"; return 1; }
    controller_pod || return 1
    pod=$CTL
    echo "$pod" > "$R/pod1.txt"
    nap "${STAIRCASE_SETTLE:-20}"
    from=$(date +%s); echo "$from" > "$d/start.txt"
    hold=$(schedule_seconds "$sched")
    load "$d" "lg-$(echo "$n" | tr 'A-Z' 'a-z')" open $((hold + 1500)) --schedule "$sched" --seed "$seed" --procs 6 --runaway "$run" \
        --max-inflight "$(jq -r .maxInflight "$R/spec.json")" --discard "$(jq -r .discardSeconds "$R/spec.json")"
    rc=$?
    nap "${STAIRCASE_SETTLE:-20}"
    to=$(date +%s); echo "$to" > "$d/end.txt"
    copy_recordings "$pod" "$d/rec"
    prom_range "$from" "$to" "$d/prom.jsonl"
    t "phase $n rc=$rc"
    [ "$rc" = 0 ] && touch "$d/done"
    return "$rc"
}

# analyze_steps : one analysis.jsonl per phase with requests. RUNNER=guarded in the run's
# own loop, which a signal or an abort must be able to interrupt.
analyze_steps() {
    local p
    for p in "$R"/phases/*/; do
        p=$(basename "$p")
        [ -n "${RUNNER:-}" ] && aborted && return 1
        [ -s "$R/phases/$p/requests.csv.gz" ] || { echo "$p: no requests"; continue; }
        ${RUNNER:-} "$PY" "$HERE/staircase_analyze.py" steps "$R" "$p" --discard "$(jq -r .discardSeconds "$R/spec.json")" || echo "$p: no steps to judge"
    done
}

# gate_check <repeat> : score the gate buckets' repeat in the loop. A fail, or a phase
# that did not finish, stops the run: repeats cannot undo it, and it frees the GPUs.
gate_check() {
    RUNNER=guarded analyze_steps > "$R/gate-r$1.steps" 2>&1
    aborted && return 1
    guarded "$PY" "$HERE/staircase_analyze.py" gate "$R" "$R/frozen.json" "$R/spec.json" --repeat "$1" > "$R/gate-r$1.out" 2>&1
    local rc=$?
    t "gate, repeat $1: rc=$rc: $(tail -1 "$R/gate-r$1.out" | cut -c1-200)"
    return "$rc"
}

# --- the run --------------------------------------------------------------------------------
# finish : the one exit path. Tear down on anything that started. A second signal must
# not cut the teardown short: this is what frees the GPUs.
finish() {
    local rc=$?
    trap - EXIT
    trap ':' INT TERM HUP
    t "exit rc=$rc: teardown"
    touch "$R/STOP"
    [ -n "${CAP:-}" ] && kill "$CAP" 2>/dev/null
    [ -n "${CHILD:-}" ] && kill "$CHILD" 2>/dev/null
    if [ "${STARTED:-0}" = 1 ]; then
        timeout -k 30 2400 bash "$HERE/staircase.sh" teardown > "$R/teardown.out" 2>&1
        local trc=$?
        t "teardown rc=$trc: $(tail -2 "$R/teardown.out" | tr '\n' ' ' | cut -c1-240)"
        [ "$trc" = 0 ] || rc=1
        [ -n "${STAIRCASE_SMOKE:-}" ] && { smoke_verdict "$trc"; rc=$?; }
    fi
    exit "$rc"
}

# --- the smoke run ----------------------------------------------------------------------
# What only a real API server can answer, on 3 GPUs at most: the namespace label and the
# lease, the Job's rights, a fleet change, flow control both ways, the load pod and its
# captures, a deliberate abort, and a teardown that confirms. Each line of smoke.txt says
# what was seen. It proves none of the arithmetic, and no prediction.
smoke_check() { echo "$1 $2" >> "$R/smoke.txt"; t "smoke: $1 $2"; }

# smoke_read <name> <want> <got> : one comparison, the value in the line.
smoke_read() {
    if [ "$2" = "$3" ]; then smoke_check ok "$1: $3"; else smoke_check FAIL "$1: read '$3', want '$2'"; fi
}

smoke_body() {
    local id until after kind verb landed epod ip hold=$((2 * ${STAIRCASE_SMOKE_STEP:-90}))
    id=$(cat "$R/run-id")
    smoke_read "lease holds the run id" "$id" "$(kube get configmap staircase-lease -o json 2>/dev/null | jq -r '.data.run // empty')"
    smoke_read "namespace label holds the run id" "$id" "$(timeout -k 5 $((KUBE_TIMEOUT + 5)) kubectl --request-timeout="${KUBE_TIMEOUT}s" get namespace "$NS" -o json 2>/dev/null | jq -r --arg l "$RUN_LABEL" '.metadata.labels[$l] // empty')"
    smoke_read "deadline Job is active" 1 "$(kube get job staircase-deadline -o jsonpath='{.status.active}' 2>/dev/null)"
    for kind in deployments.apps statefulsets.apps daemonsets.apps scaledobjects.keda.sh; do
        for verb in deletecollection list; do
            smoke_read "the Job may $verb $kind" yes "$(kube auth can-i "$verb" "$kind" --as="system:serviceaccount:$NS:staircase-deadline" 2>/dev/null)"
        done
    done
    until=$(kube get configmap staircase-lease -o json 2>/dev/null | jq -r '.data.until // 0')
    renew_lease
    after=$(kube get configmap staircase-lease -o json 2>/dev/null | jq -r '.data.until // 0')
    if [ "$after" -gt "$until" ]; then smoke_check ok "the lease moved from $until to $after"; else smoke_check FAIL "the lease did not move: $until"; fi

    local step=${STAIRCASE_SMOKE_STEP:-90}
    if load "$R/smoke/first" lg-smoke-a open $((hold + 300)) --schedule "$step:2:500:20:1,$step:4:500:20:1" --seed 1 --procs 1 --discard 30 --max-inflight 500; then
        smoke_check ok "two steps ran on the fleet 1+1"
    else
        smoke_check FAIL "the first load did not finish"; return 1
    fi
    [ -s "$R/smoke/first/requests.csv.gz" ] && smoke_check ok "requests saved: $(wc -c < "$R/smoke/first/requests.csv.gz") bytes" \
        || smoke_check FAIL "no requests saved"
    if flow_control on; then smoke_check ok "flow control on"; else smoke_check FAIL "flow control on"; return 1; fi
    # The restart moved the EPP: the capture must find it again, flow control now among its series.
    read -r epod ip _ <<< "$(epp_pod)"
    if [ -n "$epod" ] && epp_scrape "$ip" "$(cat "$R/epp-scrape.txt")" 2>/dev/null | grep -q '^llm_d_epp_flow_control_'; then
        smoke_check ok "the restarted EPP reads, flow control among its series"
    else
        smoke_check FAIL "the restarted EPP does not read, or shows no flow control"
    fi
    if pin_fleet 1 2; then smoke_check ok "the fleet moved to 1+2"; else smoke_check FAIL "the fleet did not move to 1+2"; return 1; fi

    # The deliberate abort, inside a load, as it would come.
    (sleep "${STAIRCASE_SMOKE_ABORT:-60}"; date +%s > "$R/smoke-abort-at"; touch "$R/abort") &
    load "$R/smoke/second" lg-smoke-b open 900 --schedule 600:2:500:20:1 --seed 2 --procs 1 --discard 30 --max-inflight 500
    landed=$(($(date +%s) - $(cat "$R/smoke-abort-at" 2>/dev/null || date +%s)))
    if [ -e "$R/abort" ]; then smoke_check ok "abort landed in ${landed}s"; else smoke_check FAIL "the load ended without the abort"; fi
    return 1
}

# smoke_verdict <teardown rc> : after the teardown, what only then can be read.
smoke_verdict() {
    if [ "$1" = 0 ]; then smoke_check ok "teardown confirmed"; else smoke_check FAIL "teardown not confirmed: rc=$1"; fi
    if [ -s "$R/smoke/second/requests.csv.gz" ]; then smoke_check ok "the aborted load was saved"; else smoke_check FAIL "the aborted load was lost"; fi
    if [ "$(kube get pods -o json 2>/dev/null | jq '.items | length')" = 0 ]; then smoke_check ok "no pod left"; else smoke_check FAIL "pods left: see final-pods.txt"; fi
    if grep -q '^FAIL' "$R/smoke.txt"; then echo "smoke: FAILED: $R/smoke.txt"; return 1; fi
    echo "smoke: passed: $R/smoke.txt"
}

cmd_run() {
    need
    [ $# = 1 ] || die "usage: staircase.sh run <image>"
    [ -n "$PROM" ] || die "set BENCHMARK_PROMETHEUS_URL"
    if [ -z "${STAIRCASE_COPY:-}" ]; then
        [ -e "$R/timeline.txt" ] && die "$R/timeline.txt exists: a run has used this directory"
        timeout -k 5 $((KUBE_TIMEOUT + 5)) kubectl --request-timeout="${KUBE_TIMEOUT}s" get namespace "$NS" >/dev/null 2>&1 \
            && die "namespace $NS exists: a fresh one is wanted"
        mkdir -p "$R/bin" || die "cannot write $R"
        cp -f "$HERE"/staircase* "$HERE/closed_loop_sweep.py" "$R/bin/" || die "cannot copy the tooling"
        cp -f "$SPEC" "$R/spec.yaml" || die "no spec at $SPEC"
        command -v yq >/dev/null || die "yq not found"
        yq -o=json "$R/spec.yaml" > "$R/spec.json" || die "spec does not parse"
        STAIRCASE_COPY=1 STAIRCASE_ROOT="$ROOT" STAIRCASE_WHATIF="$WHATIF" exec bash "$R/bin/staircase.sh" run "$1"
    fi
    STARTED=0; CAP=; CHILD=
    beat
    trap finish EXIT
    trap 'exit 143' INT TERM HUP
    t "run start image=$1"
    timeout -k 5 $((KUBE_TIMEOUT + 5)) kubectl --request-timeout="${KUBE_TIMEOUT}s" create namespace "$NS" >/dev/null || { t "namespace create failed"; exit 1; }
    claim || { t "claim failed: nothing here marks the namespace as this run's"; exit 1; }
    STARTED=1
    arm_deadline || { t "no deadline: refusing to hold GPUs without one"; exit 1; }
    guarded standup_make "$1" > "$R/standup.log" 2>&1 || { t "standup failed"; exit 1; }
    prepare || exit 1
    pin_fleet "$START_P" "$START_D" || exit 1
    flow_control off || exit 1
    kube create configmap staircase-loadgen --from-file=staircase_load.py="$HERE/staircase_load.py" \
        --from-file=closed_loop_sweep.py="$HERE/closed_loop_sweep.py" >/dev/null || { t "loadgen configmap failed"; exit 1; }
    recorder_on || exit 1
    witness || exit 1
    capture_loop "$$" > "$R/capture.out" 2>&1 &
    CAP=$!
    [ -n "${STAIRCASE_SMOKE:-}" ] && { smoke_body; exit 1; }
    if [ -n "${STAIRCASE_LAPSE:-}" ]; then
        # Beat until killed. A runner nobody kills stops here, and tears down.
        touch "$R/lapse-ready"
        local stop=$(($(date +%s) + ${STAIRCASE_LAPSE_CAP:-3600}))
        while [ "$(date +%s)" -lt "$stop" ]; do
            beat
            aborted && exit 1
            nap "$POLL"
        done
        t "lapse: nobody killed the runner"
        exit 1
    fi
    warmup || exit 1
    t "waiting for the freeze (capped at ${FREEZE_WAIT}s)"
    local end=$(($(date +%s) + FREEZE_WAIT))
    while [ ! -e "$R/ladder.ready" ]; do
        beat
        [ "$(date +%s)" -ge "$end" ] && { t "no freeze in time"; exit 1; }
        aborted && { t "abort"; exit 1; }
        sleep "$TICK"
    done
    verify_frozen || exit 1
    local name fc sched run seed np nd
    while read -r name fc sched run seed np nd; do
        [ -n "$name" ] || continue
        beat
        aborted && { t "abort"; exit 1; }
        if [ "$name" = gate-check ]; then
            gate_check "$fc" || { t "the gate failed: stopping"; exit 1; }
            continue
        fi
        phase "$name" "$fc" "$sched" "$run" "$seed" "$np" "$nd" || t "phase $name failed (going on unless the engines did)"
        [ -s "$R/engine-errors.txt" ] && { t "engine errors: stopping"; exit 1; }
        verify_frozen || exit 1
    done < "$R/ladder.txt"
    t "all phases done"
}

# rescue_load : save what is only in a load pod, before anything deletes it.
rescue_load() {
    local p d
    for p in $(kube get pod -l app=loadgen -o json 2>/dev/null | jq -r '.items[].metadata.name'); do
        d=$(cat "$R/load-dir-$p" 2>/dev/null)
        [ -n "$d" ] || d="$R/rescued/$p"
        mkdir -p "$d"
        snapshot_load "$d" "$p"
    done
}

# left : how many workloads remain, the deadline Job's own pod aside. Fails on a read
# that fails: an error is not an empty namespace.
left() {
    local kind n total=0 out
    for kind in deploy statefulset daemonset; do
        out=$(kube get "$kind" -o json 2>/dev/null) || return 1
        n=$(echo "$out" | jq '.items | length') || return 1
        total=$((total + n))
    done
    if [ -n "$(kube api-resources --api-group=leaderworkerset.x-k8s.io -o name 2>/dev/null)" ]; then
        out=$(kube get leaderworkersets -o json 2>/dev/null) || return 1
        n=$(echo "$out" | jq '.items | length') || return 1
        total=$((total + n))
    fi
    out=$(kube get pods -o json 2>/dev/null) || return 1
    n=$(echo "$out" | jq '[.items[] | select(.metadata.labels["job-name"] != "staircase-deadline")] | length') || return 1
    echo $((total + n))
}

# teardown : free what this run owns, from any state; safe to re-run. Saves what is still
# only in the cluster first, then the harness, then whatever is left, and keeps the
# deadline Job until the workloads are confirmed gone.
cmd_teardown() {
    need
    TEARDOWN=1
    local p i n kind own=0
    owned || own=$?
    case "$own" in
        0) ;;
        1) die "refusing teardown: namespace $NS does not carry this run's id (see $R/run-id, or set STAIRCASE_RUN_ID)" ;;
        *) echo "TEARDOWN NOT DONE: could not read who owns $NS. Nothing was deleted. The deadline Job stands; run this again." >&2
           exit 1 ;;
    esac
    rescue_load
    kube delete pod -l app=loadgen --ignore-not-found --grace-period=5 --timeout=40s 2>&1
    for p in $(kube get pod -l control-plane=controller-manager -o json 2>/dev/null | jq -r '.items[].metadata.name'); do
        if kube get pod "$p" -o jsonpath='{.spec.containers[*].name}' 2>/dev/null | grep -qw recordings; then
            copy_recordings "$p" "$R/recordings-$p"
        fi
    done
    (cd "$ROOT" && timeout -k 30 1200 make --no-print-directory benchmark-teardown BENCHMARK_NAMESPACE="$NS" BENCHMARK_SPEC="$SCENARIO") > "$R/benchmark-teardown.log" 2>&1
    echo "benchmark-teardown rc=$?"
    # One at a time: kubectl refuses a whole list over one kind the API does not have.
    for kind in scaledobject leaderworkersets deploy daemonset statefulset; do
        kube delete "$kind" --all --wait=false 2>&1
    done
    kube delete pod -l 'job-name!=staircase-deadline' --grace-period=30 --timeout=40s 2>&1
    n=unread
    for i in $(seq 1 "${STAIRCASE_TEARDOWN_TRIES:-30}"); do
        if n=$(left); then
            [ "$n" = 0 ] && break
        else
            n=unread
            echo "reading what is left failed: trying again"
        fi
        sleep "${STAIRCASE_TEARDOWN_POLL:-10}"
    done
    kube get pods > "$R/final-pods.txt" 2>&1
    if [ "$n" != 0 ]; then
        echo "TEARDOWN NOT CONFIRMED: $n workloads left in $NS. The deadline Job stays; check by hand." >&2
        return 1
    fi
    kube delete job staircase-deadline --ignore-not-found --wait=false 2>&1
    kube delete pod -l job-name=staircase-deadline --grace-period=5 --timeout=40s 2>&1
    echo "confirmed: no workload left in $NS"
}

# holding : deployments, statefulsets, daemonsets and LeaderWorkerSets left, plus pods
# that ask for a GPU: what the Job must free. A bare helper pod is not in its remit.
# Fails on a read that fails.
holding() {
    local kind n total=0 out
    for kind in deploy statefulset daemonset; do
        out=$(kube get "$kind" -o json 2>/dev/null) || return 1
        n=$(echo "$out" | jq '.items | length') || return 1
        total=$((total + n))
    done
    if [ -n "$(kube api-resources --api-group=leaderworkerset.x-k8s.io -o name 2>/dev/null)" ]; then
        out=$(kube get leaderworkersets -o json 2>/dev/null) || return 1
        n=$(echo "$out" | jq '.items | length') || return 1
        total=$((total + n))
    fi
    out=$(kube get pods -o json 2>/dev/null) || return 1
    n=$(echo "$out" | jq '[.items[] | select(any(.spec.containers[]?; .resources.limits["nvidia.com/gpu"] != null))] | length') || return 1
    echo $((total + n))
}

# lapse_end : whatever happened, nothing outlives this: the runner, and then a teardown.
lapse_end() {
    local rc=$?
    trap - EXIT
    trap ':' INT TERM HUP
    [ -n "${RUNNER_PID:-}" ] && kill -KILL "$RUNNER_PID" 2>/dev/null
    touch "$R/STOP"
    timeout -k 30 2400 bash "$HERE/staircase.sh" teardown > "$R/teardown.out" 2>&1
    local trc=$?
    if [ "$trc" = 0 ]; then smoke_check ok "teardown confirmed"; else smoke_check FAIL "teardown not confirmed: rc=$trc"; rc=1; fi
    if grep -q '^FAIL' "$R/smoke.txt" 2>/dev/null; then rc=1; fi
    [ "$rc" = 0 ] && echo "lapse: passed: $R/smoke.txt" || echo "lapse: FAILED: $R/smoke.txt"
    exit "$rc"
}

# lapse : the backstop itself, on 2 GPUs. A runner on a short lease, killed with -9, and
# then nobody but the deadline Job: it must delete the workloads on its own.
cmd_lapse() {
    need
    [ $# = 1 ] || die "usage: staircase.sh lapse <image>"
    local i end begin n lease
    mkdir -p "$R" || die "cannot write $R"
    [ -e "$R/timeline.txt" ] && die "$R/timeline.txt exists: a run has used this directory"
    export STAIRCASE_LAPSE=1 STAIRCASE_START_PREFILL=1 STAIRCASE_START_DECODE=1
    export STAIRCASE_LEASE="${STAIRCASE_LAPSE_LEASE:-300}" STAIRCASE_LEASE_EVERY="${STAIRCASE_LEASE_EVERY:-60}"
    RUNNER_PID=
    trap lapse_end EXIT
    trap 'exit 143' INT TERM HUP
    setsid bash "$HERE/staircase.sh" run "$1" > "$R.lapse-runner.out" 2>&1 &
    RUNNER_PID=$!
    end=$(($(date +%s) + ${STAIRCASE_LAPSE_START:-3600}))
    until [ -e "$R/lapse-ready" ]; do
        kill -0 "$RUNNER_PID" 2>/dev/null || { smoke_check FAIL "the runner died before the fleet was up"; exit 1; }
        [ "$(date +%s)" -ge "$end" ] && { smoke_check FAIL "no fleet in time"; exit 1; }
        sleep "$TICK"
    done
    # The claim's first lease is long: wait for a renewal to shorten it.
    for i in $(seq 1 120); do
        lease=$(kube get configmap staircase-lease -o json 2>/dev/null | jq -r '.data.until // 0')
        [ "${lease:-0}" -gt 0 ] && [ "$lease" -le $(($(date +%s) + STAIRCASE_LEASE + 5)) ] && break
        sleep "$TICK"
    done
    n=$(holding) || n=unread
    if [ "$n" = 0 ] || [ "$n" = unread ]; then smoke_check FAIL "nothing to free before the kill: $n"; exit 1; fi
    kill -KILL "$RUNNER_PID"
    RUNNER_PID=
    begin=$(date +%s)
    smoke_check ok "runner killed, $n workloads held, lease until $lease"
    end=$((begin + STAIRCASE_LEASE + ${STAIRCASE_LAPSE_WAIT:-600}))
    n=unread
    while [ "$(date +%s)" -lt "$end" ]; do
        if n=$(holding); then [ "$n" = 0 ] && break; else n=unread; fi
        sleep "$POLL"
    done
    kube logs job/staircase-deadline > "$R/deadline.log" 2>/dev/null
    if [ "$n" = 0 ]; then smoke_check ok "the Job freed them in $(($(date +%s) - begin))s"; else smoke_check FAIL "still holding $n after $(($(date +%s) - begin))s"; fi
    if grep -q 'deadline reached' "$R/deadline.log"; then smoke_check ok "the Job's log says the deadline was reached"; else smoke_check FAIL "the Job's log says no deadline"; fi
}

cmd_status() {
    need
    tail -12 "$R/timeline.txt" 2>/dev/null | cut -c1-200
    ls "$R/phases" 2>/dev/null | tr '\n' ' '; echo
    kube get pods -o custom-columns='NAME:.metadata.name,PHASE:.status.phase,GPU:.spec.containers[*].resources.limits.nvidia\.com/gpu' 2>&1 | head -20
}

cmd_abort() {
    need
    touch "$R/abort"
    echo "abort written: the run tears down within a minute; follow it with staircase.sh status"
}

# redo : re-price each bucket at its measured Ca2, targets held. The file the report reads
# beside the frozen one; the frozen one does not move.
cmd_redo() {
    need
    local ca2 args=() rec
    verify_frozen || die "the frozen files do not match their hashes"
    analyze_steps
    ca2=$("$PY" "$HERE/staircase_analyze.py" ca2 "$R") || die "no measured Ca2"
    for rec in $ca2; do args+=(--ca2 "$rec"); done
    [ ${#args[@]} -gt 0 ] || die "no bucket with a measured Ca2"
    rm -f "$R/frozen-measured-ca2.json"
    "$WHATIF" freeze --recording "$(largest_recording)" --spec "$R/spec.yaml" --targets "$R/frozen.json" "${args[@]}" \
        -o "$R/frozen-measured-ca2.json" || die "whatif freeze refused"
    echo "$R/frozen-measured-ca2.json: $ca2"
}

cmd_analyze() {
    need
    verify_frozen || die "the frozen files do not match their hashes: no report on a moved prediction or band"
    analyze_steps
    if [ -s "$R/frozen-measured-ca2.json" ]; then
        "$PY" "$HERE/staircase_analyze.py" report "$R" "$R/frozen.json" "$R/spec.json" --redo "$R/frozen-measured-ca2.json" || die "report failed"
    else
        "$PY" "$HERE/staircase_analyze.py" report "$R" "$R/frozen.json" "$R/spec.json" || die "report failed"
    fi
    echo "$R/report.md"
}

# Sourced by hack/check-staircase.sh, for its functions.
if [ "${STAIRCASE_LIB:-}" = 1 ]; then return 0; fi

case "${1:-}" in
    run) shift; cmd_run "$@" ;;
    lapse) shift; cmd_lapse "$@" ;;
    smoke)
        shift
        export STAIRCASE_SMOKE=1 STAIRCASE_START_PREFILL=1 STAIRCASE_START_DECODE=1
        cmd_run "$@"
        ;;
    freeze) cmd_freeze ;;
    analyze) cmd_analyze ;;
    redo) cmd_redo ;;
    teardown) cmd_teardown ;;
    status) cmd_status ;;
    abort) cmd_abort ;;
    *) sed -n '3,12p' "${BASH_SOURCE[0]}" >&2; exit 2 ;;
esac
