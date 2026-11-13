#!/usr/bin/env bash
# Executes the TTFT staircase against a stub kubectl, make and whatif, and runs its
# arithmetic self-test.
#
# A run holds GPUs on a shared cluster for days, and what protects them is exit paths,
# which `bash -n` does not see. Each exit path listed under "exit paths" is taken here.
# This proves the script's control flow, nothing about a real API server.
#
# This check must itself be safe. A run it left behind once outlived its stub directory,
# and from then on `kubectl` would have meant the real binary. So:
#   - runs see a PATH of the stubs and a directory of named tools, and no kubectl but the
#     stub: with the stub gone, kubectl is not found. The check refuses to start if any
#     directory on that PATH offers another one.
#   - KUBECONFIG points at nothing, for the same reason.
#   - every run leads its own session, and on any exit of this script, by a signal too,
#     every session is killed. A watcher does it if this script is killed with -9.
#   - the last check is that no process names the work directory.
#
# The count is asserted at the end.
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
B="$ROOT/hack/benchmark"
PY="$(command -v python3 || command -v python)" || { echo "FAIL: python3 not found"; exit 1; }
EXPECTED_CHECKS=143

WORK="$(mktemp -d)"
STUB="$WORK/bin"
TOOLS="$WORK/tools"
mkdir -p "$STUB" "$TOOLS"
export KUBECONFIG=/dev/null
unset KUBERNETES_SERVICE_HOST KUBERNETES_SERVICE_PORT

PIDS=""
reap() {
    local p
    for p in $PIDS; do kill -TERM "-$p" 2>/dev/null; done
    sleep 0.3
    for p in $PIDS; do kill -KILL "-$p" 2>/dev/null; done
    pkill -KILL -f "$WORK" 2>/dev/null
    rm -rf "$WORK"
}
trap 'reap' EXIT
trap 'exit 143' INT TERM HUP

# If this script dies with -9, no trap runs: this does it.
(
    trap '' TERM INT HUP
    parent=$$
    while kill -0 "$parent" 2>/dev/null; do sleep 2; done
    pkill -KILL -f "$WORK" 2>/dev/null
    rm -rf "$WORK"
) > /dev/null 2>&1 &
disown

for tool in bash sh env setsid python3 jq yq timeout sleep date touch cat grep sed awk tr cut head tail wc ls cp mv rm mkdir seq \
    sha256sum gzip dirname basename mktemp tee sort; do
    found="$(command -v "$tool")" || { echo "FAIL: $tool not found"; exit 1; }
    ln -s "$found" "$TOOLS/$tool"
done
RUN_PATH="$STUB:$TOOLS"
for dir in ${RUN_PATH//:/ }; do
    if [ "$dir" != "$STUB" ] && [ -e "$dir/kubectl" ]; then
        echo "FAIL: refusing to run: $dir offers a kubectl behind the stub"
        exit 1
    fi
done

fails=0
checks=0
ok() { checks=$((checks + 1)); echo "  ok: $1"; }
bad() { checks=$((checks + 1)); fails=$((fails + 1)); echo "  FAIL: $1" >&2; }
expect() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }

echo "arithmetic..."
if "$PY" "$B/staircase_selftest.py"; then ok "self-test"; else bad "self-test"; fi

# A level no window can judge would read a NaN p90, and NaN never trips the frozen rule.
if "$PY" "$B/staircase_load.py" --url u --model m --out "$WORK/o" --schedule 6:10:50:3:1 --runaway 1 --dry-run > /dev/null 2>&1; then
    bad "a stop rule on a level with nothing to judge must refuse"
else
    ok "a stop rule on a level with nothing to judge refuses"
fi
if "$PY" "$B/staircase_load.py" --url u --model m --out "$WORK/o" --schedule 600:10:50:3:4 --runaway 1 --dry-run | grep -q 'Ca2 [34]\.'; then
    ok "the driver prints the Ca2 it will send"
else
    bad "the driver prints the Ca2 it will send"
fi

cat > "$STUB/make" <<'STUB'
#!/usr/bin/env bash
echo "make $*" >> "$LOG"
case "$*" in
    *benchmark-standup*)
        # A fresh standup: the router's config as the chart writes it, nothing torn down.
        rm -f "$STATE/tearing" "$STATE/fc-off"
        echo "set ${LLMDBENCH_SET:-}" >> "$LOG"
        if [ -n "${MAKE_SLEEP:-}" ]; then
            sleep "$MAKE_SLEEP" &
            echo $! > "$STATE/standup-sleep"
            wait
        fi
        [ -n "${MAKE_FAIL:-}" ] && exit 2 ;;
    *benchmark-teardown*) touch "$STATE/tearing" ;;
esac
exit 0
STUB

cat > "$STUB/whatif" <<'STUB'
#!/usr/bin/env bash
# whatif freeze --recording R --spec S -o OUT
while [ $# -gt 0 ]; do
    case $1 in -o) out=$2; shift ;; --spec) spec=$2; shift ;; esac
    shift
done
[ -n "${WHATIF_REFUSE:-}" ] && exit 3
[ -e "$out" ] && exit 2
sum=$(sha256sum "$spec" | cut -d' ' -f1)
sed "s/__SPEC__/$sum/" "$FROZEN_FIXTURE" > "$out"
STUB

cat > "$WORK/frozen.json" <<'JSON'
{"specSha256":"__SPEC__","gate":{"vp":true,"a":true,"decodeLine":true,"sloListed":true},"buckets":[
{"name":"B","prompt":8000,"output":200,"ca2":1,"prefill":2,"decode":2,"vp":288288,"imbalance":1.19,"decodeKnee":44,"knee":44,
"operatingRate":37,"meanTPOT":0.01,"onsets":{"Prefill":54.6},"curve":[{"rate":17.6}],
"targets":[{"percentile":90,"seconds":0.2,"predictedOff":37,"predictedOn":37}]},
{"name":"H","prompt":3000,"output":500,"ca2":1,"prefill":2,"decode":2,"vp":288288,"imbalance":1.19,"decodeKnee":44,"knee":44,
"operatingRate":37,"meanTPOT":0.01,"onsets":{"Prefill":54.6},"curve":[{"rate":17.6}],
"targets":[{"percentile":90,"seconds":0.2,"predictedOff":37,"predictedOn":37}]}]}
JSON

cat > "$WORK/spec.yaml" <<'YAML'
promptCV: 0.0577
holdSeconds: 600
discardSeconds: 180
maxHoldSeconds: 1500
runawayFactor: 2
maxInflight: 4000
repeats: 1
seedBase: 3000
phaseOverheadSeconds: 240
warmup:
  order: [sweep]
  sweep: {driver: open, schedule: "1:1:1000:32:1", seed: 11}
buckets:
- {name: B, fc: ["off", "on"], gate: true}
- {name: H, fc: ["off"]}
YAML

# The rows a finished phase returns: one a second through a 600 s level, fast or slow.
"$PY" - "$WORK" <<'PYROWS'
import calendar, gzip, sys, time
base = calendar.timegm(time.strptime("2026-11-06T02:00:00Z", "%Y-%m-%dT%H:%M:%SZ"))
for name, ttft in (("good", 0.1), ("slow", 5.0)):
    with gzip.open(sys.argv[1] + "/req-%s.gz" % name, "wt") as f:
        f.write("index,level,scheduled,sent,first,last,status,prompt_tokens,tokens,error,pod\n")
        for i in range(600):
            t = base + i
            f.write("%d,1,%.4f,%.4f,%.4f,%.4f,200,8000,101,,\n" % (i, t, t, t + ttft, t + ttft + 1))
PYROWS

cat > "$STUB/kubectl" <<'STUB'
#!/usr/bin/env bash
echo "kubectl $*" >> "$LOG"
args=" $* "
case "${HANG:-}" in
    cp) case "$args" in *" cp "*) exec sleep 300 ;; esac ;;
    exec) case "$args" in *" exec "*) exec sleep 300 ;; esac ;;
    get) case "$args" in *" get scaledobject "*) exec sleep 300 ;; esac ;;
    gzip) case "$args" in *" exec "*gzip*) exec sleep 300 ;; esac ;;
    patchon) case "$args" in *"patch cm eppcm"*epp-patch-on*) exec sleep 300 ;; esac ;;
    ready) case "$args" in *" get deploy m-"*jsonpath*) exec sleep 300 ;; esac ;;
esac
if [ -n "${OWN_FAIL:-}" ]; then
    case "$args" in
        *" get configmap staircase-lease "*|*" get namespace "*" -o json "*)
            n=$(cat "$STATE/own-reads" 2>/dev/null || echo 0); echo $((n + 1)) > "$STATE/own-reads"
            [ "$n" -lt "$OWN_FAIL" ] && exit 1 ;;
    esac
fi
# FLAKY: the first FLAKY_N calls naming it fail with FLAKY_MSG, as the API server says it.
if [ -n "${FLAKY:-}" ]; then
    case "$args" in
        *"$FLAKY"*)
            n=$(cat "$STATE/flaky" 2>/dev/null || echo 0); echo $((n + 1)) > "$STATE/flaky"
            [ "$n" -lt "${FLAKY_N:-1}" ] && { echo "$FLAKY_MSG" >&2; exit 1; } ;;
    esac
fi
pd='featureGates:\n- flowControl\nplugins:\n- type: a\nflowControl:\n  maxBytes: 1\nschedulingProfiles:\n- name: b\n'
[ -e "$STATE/fc-off" ] && pd='plugins:\n- type: a\nschedulingProfiles:\n- name: b\n'
case "$args" in
    *" create namespace "*) touch "$STATE/ns" ;;
    *" label namespace "*) [ -z "${LABEL_FAIL:-}" ] || exit 1; echo "${*: -1}" | sed 's/.*=//' > "$STATE/label" ;;
    *" get namespace "*" -o json "*) printf '{"metadata":{"labels":{"llm-d-scaling-manager/staircase-run":"%s"}}}\n' "${LABEL_RUN-$(cat "$STATE/label" 2>/dev/null)}" ;;
    *" get namespace "*"--ignore-not-found -o name"*)
        [ -n "${NS_FORBID:-}" ] && { echo 'Error from server (Forbidden): namespaces "stair-ns" is forbidden' >&2; exit 1; }
        [ -n "${NS_EXISTS:-}" ] && echo namespace/stair-ns
        exit 0 ;;
    *" get namespace "*) [ -n "${NS_EXISTS:-}" ]; exit ;;
    *" get configmap staircase-lease "*)
        n=$(grep -c 'patch cm staircase-lease' "$LOG")
        [ -n "${LAPSE_HOLD:-}" ] && n=$(date +%s)
        printf '{"data":{"run":"%s","until":"%s"}}\n' "${LEASE_RUN-$(cat "$RUN/run-id" 2>/dev/null)}" "$n" ;;
    *" auth can-i "*) echo "${CAN_I:-yes}" ;;
    *" patch scaledobject "*)
        case "$args" in *so-d*) w=d ;; *) w=p ;; esac
        echo "$args" | sed 's/.*minReplicaCount\\*":\([0-9]*\).*/\1/' > "$STATE/so-$w" ;;
    *" logs job/staircase-deadline "*)
        [ -n "${ARM_FAIL:-}" ] || echo "t deadline at x"
        [ -e "$STATE/freed" ] && echo "t deadline reached: lease 1 hard 2" ;;
    *" get job staircase-deadline "*) [ -n "${JOB_DEAD:-}" ] || [ -e "$STATE/job-gone" ] || echo 1 ;;
    *" delete job staircase-deadline "*) touch "$STATE/job-gone" ;;
    *" apply -f "*"deadline.yaml"*) rm -f "$STATE/job-gone" ;;
    *" get deploy -o name "*) printf 'deployment.apps/m-decode\ndeployment.apps/m-prefill\ndeployment.apps/m-epp\n' ;;
    *" get cm -o json "*) printf '{"items":[{"metadata":{"name":"eppcm"},"data":{"pd-config.yaml":"x"}}]}\n' ;;
    *" get cm eppcm -o json "*) printf '{"data":{"pd-config.yaml":"%s"}}\n' "$pd" ;;
    *" patch cm eppcm "*) case "$args" in *epp-patch-off*) touch "$STATE/fc-off" ;; *) rm -f "$STATE/fc-off" ;; esac ;;
    *" get svc -o json "*) printf '{"items":[{"metadata":{"name":"m-epp"},"spec":{"ports":[{"port":80}]}}]}\n' ;;
    *" get configmap scaling-manager-scaling-policy-config "*) printf '{"data":{"default":"analyzers:\\n  - name: saturation\\n    score: 1\\n"}}\n' ;;
    *" get deploy m-decode -o json "*|*" get deploy m-prefill -o json "*)
        # STUB_GPUS: the GPUs the engine container asks for, as the chart rendered them.
        printf '{"spec":{"template":{"spec":{"containers":[{"name":"vllm","args":["serve","m"],"resources":{"limits":{"nvidia.com/gpu":"%s"}},"env":[{"name":"VLLM_MAX_MODEL_LEN","value":"40960"}]}],"nodeSelector":{"gpu":"x"}}}}}\n' "${STUB_GPUS:-1}" ;;
    *" get scaledobject -o json "*) printf '{"items":[{"metadata":{"name":"so-d"},"spec":{"scaleTargetRef":{"name":"m-decode"}}},{"metadata":{"name":"so-p"},"spec":{"scaleTargetRef":{"name":"m-prefill"}}}]}\n' ;;
    *" get deploy m-"*"jsonpath="*)
        case "$args" in *m-decode*) f="$STATE/so-d" ;; *) f="$STATE/so-p" ;; esac
        if [ -n "${NEVER_READY:-}" ]; then echo 0
        elif [ -n "${READY_THEN_NOT:-}" ] && [ -e "$RUN/ladder.ready" ]; then echo 0
        elif [ -n "${NEVER_MOVE:-}" ] && grep -q 'flow control on' "$RUN/timeline.txt"; then echo 1
        else cat "$f" 2>/dev/null || echo 2; fi ;;
    *" get pod -l control-plane=controller-manager -o json "*)
        printf '{"items":[{"metadata":{"name":"ctl"},"status":{"phase":"Running"}}]}\n' ;;
    *" get pod -l app=loadgen -o json "*)
        if [ -n "${LOAD_ALIVE:-}" ]; then printf '{"items":[{"metadata":{"name":"lg-b-off-r1"}}]}\n'; else printf '{"items":[]}\n'; fi ;;
    *" get pod witness "*)
        # WITNESS_DIES: the first look finds it Failed, as a pod whose sleep ended.
        if [ -n "${WITNESS_DIES:-}" ] && [ ! -e "$STATE/witness-died" ]; then touch "$STATE/witness-died"; echo Failed; exit 0; fi
        # WITNESS_PENDING: a witness on its way up, as one another caller just made.
        if [ -n "${WITNESS_PENDING:-}" ]; then echo Pending; exit 0; fi
        echo Running ;;
    *" wait pod witness "*) [ -z "${WITNESS_NOT_READY:-}" ] || { echo "timed out waiting for the condition" >&2; exit 1; } ;;
    *" get pods -o json "*)
        if [ -e "$STATE/tearing" ] && [ -n "${GET_FAIL:-}" ]; then exit 1; fi
        # The EPP, until the teardown: a gRPC port that serves no metrics, then 9090.
        if [ -e "$STATE/tearing" ] || [ -n "${NO_EPP_POD:-}" ]; then printf '{"items":[]}\n'; exit 0; fi
        printf '{"items":[{"metadata":{"name":"m-epp-1"},"status":{"phase":"Running","podIP":"10.0.0.9"},"spec":{"containers":[{"ports":[{"containerPort":9002}]}]}}]}\n' ;;
    *" get deploy scaling-manager-controller-manager -o json "*)
        # DEP_NOTICE: kubectl prints a notice on stderr, as it does while an
        # aggregated API is down, and the read still succeeds (or fails).
        [ -z "${DEP_NOTICE:-}" ] || echo 'E1113 09:55:01.123456 4242 memcache.go:265] "Unhandled Error" err="could not get resource list for external.metrics.k8s.io/v1beta1: the server is currently unable to handle the request"' >&2
        [ -n "${CONTROLLER_READ_FAIL:-}" ] && { echo "The connection to the server was refused" >&2; exit 1; }
        [ -n "${NO_EPP_SECRET:-}" ] && { printf '{"spec":{"template":{"spec":{"volumes":[]}}}}\n'; exit 0; }
        printf '{"spec":{"template":{"spec":{"volumes":[{"name":"epp-metrics-token","secret":{"secretName":"scaling-manager-epp-metrics-token"}}]}}}}\n' ;;
    *" exec witness "*" scrape http://10.0.0.9:9090/metrics /t/token "*)
        if [ -n "${EPP_DENIED:-}" ]; then echo "scrape: http://10.0.0.9:9090/metrics answered 401: b'Unauthorized'" >&2; exit 1; fi
        # SCRAPE_FLAKY: the standup's read works, the capture's fail.
        if [ -n "${SCRAPE_FLAKY:-}" ] && [ -e "$STATE/scraped" ]; then echo "scrape: http://10.0.0.9:9090/metrics: timed out" >&2; exit 1; fi
        touch "$STATE/scraped"
        printf 'llm_d_epp_request_ttft_seconds_bucket{le="0.1"} 3\nllm_d_epp_request_ttft_seconds_count 3\n'
        [ -e "$STATE/fc-off" ] || printf 'llm_d_epp_flow_control_queue_size 0\n' ;;
    *" exec witness "*" scrape "*) echo "scrape: connection refused" >&2; exit 1 ;;
    *" get deploy -o json "*)
        # The lapse case: workloads until the Job has freed them, which the check says.
        if [ -n "${LAPSE_HOLD:-}" ] && [ ! -e "$STATE/freed" ] && [ ! -e "$STATE/tearing" ]; then
            printf '{"items":[{"metadata":{"name":"m-decode"}}]}\n'; exit 0
        fi
        if [ -e "$STATE/tearing" ]; then
            [ -n "${GET_FAIL:-}" ] && exit 1
            [ -n "${LEFT:-}" ] && { printf '{"items":[{"metadata":{"name":"m-decode"}}]}\n'; exit 0; }
        fi
        printf '{"items":[]}\n' ;;
    *" get deploy -o json "*|*" get statefulset -o json "*|*" get daemonset -o json "*)
        if [ -e "$STATE/tearing" ]; then
            [ -n "${GET_FAIL:-}" ] && exit 1
            case "$args" in *" get deploy "*) [ -n "${LEFT:-}" ] && { printf '{"items":[{"metadata":{"name":"m-decode"}}]}\n'; exit 0; } ;; esac
        fi
        printf '{"items":[]}\n' ;;
    *" api-resources "*) [ -n "${HAS_LWS:-}" ] && echo leaderworkersets.leaderworkerset.x-k8s.io ;;
    *" get leaderworkersets "*) printf '{"items":[]}\n' ;;
    *" delete leaderworkersets "*) [ -n "${HAS_LWS:-}" ]; exit ;;
    *" get pods --no-headers "*) : ;;
    *" get --raw "*) : ;;
    *" exec ctl "*) printf '{"Kind":"start","Data":{"Commit":"abc"}}\n' ;;
    *" cp "*) d="${*: -1}"; mkdir -p "$d"; echo recording-bytes > "$d/learnrec-1.jsonl" ;;
    *" exec "*"gzip"*)
        if [ -n "${SNAP_PARTIAL:-}" ]; then head -c 5000 /dev/zero; exit 1; fi
        if [ -n "${SLOW:-}" ]; then cat "$WORK/req-slow.gz"; else cat "$WORK/req-good.gz"; fi ;;
    *" logs lg-smoke-b "*) printf '2026-11-06T02:00:00Z LEVEL 1 rate=2 in=1 out=1 ca2=1 hold=600s\n' ;;
    *" logs lg-"*|*" logs w-"*)
        if [ -n "${LOAD_STUCK:-}" ]; then exit 0; fi
        printf '2026-11-06T02:00:00Z LEVEL 1 rate=1 in=1 out=1 ca2=1 hold=600s\n'
        [ -n "${LOAD_RUNS:-}" ] || printf '2026-11-06T02:10:00Z SWEEP DONE ok=1 err=0 runaway=no\n' ;;
    *" apply -f "*"load.yaml"*)
        case "$args" in *"/phases/"*)
            echo "step" >> "$STATE/steps"
            [ -n "${TAMPER:-}" ] && echo "tampered" >> "$RUN/ladder.txt"
            [ -n "${ENGINE_ERR:-}" ] && echo "boom" >> "$RUN/engine-errors.txt"
            ;;
        esac ;;
esac
exit 0
STUB
chmod +x "$STUB"/*

# fresh <name> : a run directory and a log of its own.
fresh() {
    export RUN="$WORK/$1" LOG="$WORK/$1.log" STATE="$WORK/$1.state"
    mkdir -p "$STATE"; : > "$LOG"
}

# env_line : the stubs' environment, on the restricted PATH.
env_line() {
    exec_env=("$TOOLS/env" PATH="$RUN_PATH" WORK="$WORK" BENCHMARK_NAMESPACE=stair-ns STAIRCASE_RUN="$RUN" BENCHMARK_PROMETHEUS_URL=http://p:9090
        STAIRCASE_SPEC="$WORK/spec.yaml" STAIRCASE_ROOT="$WORK" STAIRCASE_WHATIF="$STUB/whatif" FROZEN_FIXTURE="$WORK/frozen.json"
        STAIRCASE_POLL=0.2 STAIRCASE_TICK=0.1 STAIRCASE_SETTLE=0 STAIRCASE_ARM_POLL=0 STAIRCASE_TEARDOWN_POLL=0 STAIRCASE_TEARDOWN_TRIES=3
        STAIRCASE_LEASE_EVERY=1 STAIRCASE_FREEZE_WAIT=40 STAIRCASE_READY_WAIT=2 STAIRCASE_OWNED_POLL=0 STAIRCASE_WITNESS_RETRY=0
        STAIRCASE_KUBE_RETRY_WAIT=0)
}

# base_env <VAR=value...> <command...> : run it to its end under the stubs.
base_env() { env_line; ("${exec_env[@]}" "$@"); }

# start <VAR=value...> <run args...> : a run in a session of its own; $pid leads it.
start() {
    env_line
    setsid "${exec_env[@]}" "$@" bash "$B/staircase.sh" run img > "$RUN.out" 2>&1 &
    pid=$!
    PIDS="$PIDS $pid"
}

# wait_run : wait for $pid, capped; $rc is its exit, 99 if it never ended.
wait_run() {
    local i
    for i in $(seq 1 450); do
        kill -0 "$pid" 2>/dev/null || { wait "$pid"; rc=$?; return 0; }
        sleep 0.2
    done
    rc=99
}

# wait_for <pattern> <file> : capped, so a broken run fails rather than hangs.
wait_for() {
    local i
    for i in $(seq 1 150); do
        grep -q "$1" "$2" 2>/dev/null && return 0
        sleep 0.2
    done
    return 1
}

# Whether a teardown reached the harness and the workloads, whatever happened before.
tore_down() { grep -q 'make --no-print-directory benchmark-teardown' "$LOG" && grep -q 'delete deploy --all' "$LOG"; }
steps() { cat "$STATE/steps" 2>/dev/null | wc -l; }
leases() { grep -c 'patch cm staircase-lease' "$LOG"; }

echo "a whole run..."
fresh ok
start
if wait_for 'waiting for the freeze' "$RUN/timeline.txt"; then ok "reaches the freeze"; else bad "reaches the freeze"; cat "$RUN.out"; fi
expect "the warm-up left a recording" '[ -s "$RUN/recordings-warmup/learnrec-1.jsonl" ]'
expect "nothing ran before the freeze" '[ ! -e "$STATE/steps" ]'
base_env bash "$B/staircase.sh" freeze > "$WORK/freeze.out" 2>&1
expect "freeze writes the hashes, spec.json among them" '[ -e "$RUN/ladder.ready" ] && grep -q spec.json "$RUN/frozen.sha256"'
expect "the plan and its assumptions are printed" 'grep -q "GPU-hours" "$RUN/plan.md" && grep -q "What the prediction assumes" "$RUN/plan.md"'
gpuh() { STAIRCASE_TP="$1" "$PY" "$B/staircase_freeze.py" plan "$RUN/frozen.json" "$RUN/spec.json" | awk -F'|' '/^\| gate,/ { gsub(/ /, "", $5); print $5 }'; }
expect "at TP 2 the plan counts twice the GPU-hours" 'awk -v a="$(gpuh 1)" -v b="$(gpuh 2)" "BEGIN { d = b - 2 * a; exit !(a > 0 && d <= 1 && d >= -1) }"'
wait_run
expect "the run exits 0" '[ "$rc" = 0 ]'
expect "all three phases ran" '[ "$(steps)" = 3 ]'
expect "the gate was scored, and passed" 'grep -q "gate, repeat 1: rc=0" "$RUN/timeline.txt"'
expect "the deadline is armed before the standup" '[ "$(grep -n "apply -f .*deadline.yaml" "$LOG" | head -1 | cut -d: -f1)" -lt "$(grep -n benchmark-standup "$LOG" | head -1 | cut -d: -f1)" ]'
expect "the lease is claimed before anything else" 'grep -q "create configmap staircase-lease --from-literal=until=.* --from-literal=run=" "$LOG"'
expect "slo is listed before the recorder" '[ "$(grep -n "patch configmap scaling-manager-scaling-policy-config" "$LOG" | head -1 | cut -d: -f1)" -lt "$(grep -n "patch deployment scaling-manager-controller-manager" "$LOG" | head -1 | cut -d: -f1)" ]'
expect "the lease was renewed" '[ "$(leases)" -gt 0 ]'
expect "the fleet was pinned" 'grep -q "\"minReplicaCount\":2,\"maxReplicaCount\":2" "$LOG"'
expect "teardown on success, the Job after the workloads" 'tore_down && [ "$(grep -n "delete deploy --all" "$LOG" | cut -d: -f1 | head -1)" -lt "$(grep -n "delete job staircase-deadline" "$LOG" | cut -d: -f1 | head -1)" ]'
expect "the private copy ran" '[ -s "$RUN/bin/staircase.sh" ] && grep -q "run start" "$RUN/timeline.txt"'
expect "the EPP is read from the witness with the controller token, never through the API proxy" \
    'grep -q "EPP metrics readable at port 9090 of m-epp-1" "$RUN/timeline.txt" && grep -q "secretName: scaling-manager-epp-metrics-token" "$RUN/witness.yaml" && grep -q "exec witness -- python3 /w/staircase_promq.py scrape http://10.0.0.9:9090/metrics /t/token" "$LOG" && grep -q "llm_d_epp_request_ttft_seconds_bucket" "$RUN/epp-metrics.log" && ! grep -q "pods/m-epp-1:[0-9]*/proxy" "$LOG"'
if base_env bash "$B/staircase.sh" freeze > /dev/null 2>&1; then bad "a second freeze must refuse"; else ok "a second freeze refuses"; fi
if base_env bash "$B/staircase.sh" run img > /dev/null 2>&1; then bad "a used directory must refuse"; else ok "a used directory refuses"; fi
if base_env bash "$B/staircase.sh" analyze > "$WORK/analyze.out" 2>&1 && [ -s "$RUN/report.md" ]; then ok "analyze reads the run"; else bad "analyze reads the run"; cat "$WORK/analyze.out"; fi
"$TOOLS/sed" -i 's/0\.0577/0.0578/' "$RUN/spec.json"
if base_env bash "$B/staircase.sh" analyze > /dev/null 2>&1; then bad "a moved band must refuse the report"; else ok "a moved band refuses the report"; fi

echo "ownership..."
fresh nofreeze
mkdir -p "$RUN/recordings-warmup" "$RUN/phases"
echo x > "$RUN/recordings-warmup/a.jsonl"
if base_env bash "$B/staircase.sh" freeze > /dev/null 2>&1; then bad "a freeze after a phase must refuse"; else ok "a freeze after a phase refuses"; fi

fresh exists
if base_env NS_EXISTS=1 bash "$B/staircase.sh" run img > /dev/null 2>&1; then bad "an existing namespace must refuse"; else ok "an existing namespace refuses"; fi
expect "and nothing was created or torn down" '! grep -q "create namespace" "$LOG" && ! tore_down'

fresh dflt
# The id matches, so the namespace's name alone must refuse.
mkdir -p "$RUN"; echo id > "$RUN/run-id"
for ns in default kube-system; do
    if base_env BENCHMARK_NAMESPACE=$ns bash "$B/staircase.sh" teardown > /dev/null 2>&1; then bad "$ns must refuse teardown"; else ok "$ns refuses teardown"; fi
done
expect "and nothing was deleted" '! grep -q " delete " "$LOG"'
if base_env BENCHMARK_NAMESPACE=default STAIRCASE_FREEZE_WAIT=1 bash "$B/staircase.sh" run img > /dev/null 2>&1; then bad "default must refuse a run"; else ok "default refuses a run"; fi

fresh other
mkdir -p "$RUN"; echo mine > "$RUN/run-id"
if base_env LEASE_RUN=theirs bash "$B/staircase.sh" teardown > /dev/null 2>&1; then bad "another run's namespace must refuse"; else ok "another run's namespace refuses"; fi
expect "and nothing was deleted" '! grep -q " delete " "$LOG"'
if base_env LEASE_RUN= LABEL_RUN=mine bash "$B/staircase.sh" teardown > /dev/null 2>&1; then ok "the namespace label alone marks it"; else bad "the namespace label alone marks it"; fi

fresh blip
mkdir -p "$RUN"; echo id > "$RUN/run-id"
if base_env OWN_FAIL=2 bash "$B/staircase.sh" teardown > "$RUN.out" 2>&1; then ok "a failed read of the owner is retried"; else bad "a failed read of the owner is retried"; cat "$RUN.out"; fi
expect "and the teardown runs" 'tore_down'

fresh unread
mkdir -p "$RUN"; echo id > "$RUN/run-id"
if base_env OWN_FAIL=99 bash "$B/staircase.sh" teardown > "$RUN.out" 2>&1; then bad "an unreadable owner must not pass"; else ok "an unreadable owner does not pass"; fi
expect "says it could not read, not that the id is wrong, and deletes nothing" 'grep -q "could not read who owns" "$RUN.out" && ! grep -q "does not carry" "$RUN.out" && ! grep -q " delete " "$LOG"'

fresh noid
mkdir -p "$RUN"
if base_env LEASE_RUN= bash "$B/staircase.sh" teardown > /dev/null 2>&1; then bad "a directory with no id must refuse"; else ok "a directory with no id refuses"; fi

echo "exit paths..."
fresh standupfail
start MAKE_FAIL=1; wait_run
expect "a failed standup fails and tears down, no recorder" '[ "$rc" != 0 ] && tore_down && ! grep -q "patch deployment scaling-manager-controller-manager" "$LOG"'

fresh eppdenied
start EPP_DENIED=1; wait_run
expect "an EPP that refuses the token stops the run before the warm-up, says why, and tears down" \
    '[ "$rc" != 0 ] && grep -q "EPP metrics not readable with the controller.s token: stopping" "$RUN/timeline.txt" && grep -q "answered 401" "$RUN/timeline.txt" && ! grep -q "warmup" "$RUN/timeline.txt" && tore_down'

fresh noeppsecret
start NO_EPP_SECRET=1; wait_run
expect "a controller with no EPP token stops the run before the warm-up" \
    '[ "$rc" != 0 ] && grep -q "mounts no epp-metrics-token Secret" "$RUN/timeline.txt" && ! grep -q "warmup" "$RUN/timeline.txt" && tore_down'

fresh ctlreadfail
start CONTROLLER_READ_FAIL=1; wait_run
expect "a controller Deployment that cannot be read is retried and named an API error, not a missing volume" \
    '[ "$rc" != 0 ] && [ "$(grep -c "get deploy scaling-manager-controller-manager -o json" "$LOG")" = 3 ] && grep -q "could not be read (API error)" "$RUN/timeline.txt" && ! grep -q "mounts no epp-metrics-token" "$RUN/timeline.txt" && ! grep -q "warmup" "$RUN/timeline.txt" && tore_down'

fresh depnotice
start DEP_NOTICE=1
wait_for 'EPP metrics readable' "$RUN/timeline.txt"
expect "a notice on stderr while the controller Deployment reads is not a missing epp-metrics-token volume" \
    'grep -q "EPP metrics readable at port 9090 of m-epp-1" "$RUN/timeline.txt" && ! grep -q "mounts no epp-metrics-token" "$RUN/timeline.txt"'
kill -TERM "$pid" 2>/dev/null; wait_run

fresh depnoticefail
start DEP_NOTICE=1 CONTROLLER_READ_FAIL=1; wait_run
expect "a controller Deployment read that fails behind a notice names the error, not the notice" \
    '[ "$rc" != 0 ] && grep -q "controller Deployment read failed (try 1 of 3): The connection to the server was refused" "$RUN/timeline.txt" && ! grep -q "memcache" "$RUN/timeline.txt" && tore_down'

fresh noepppod
start NO_EPP_POD=1; wait_run
expect "no Running EPP pod stops the run before the warm-up, after its tries" \
    '[ "$rc" != 0 ] && [ "$(grep -c "no Running, Ready EPP pod (try" "$RUN/timeline.txt")" = 3 ] && grep -q "no Running EPP pod: the EPP cannot be read" "$RUN/timeline.txt" && ! grep -q "warmup" "$RUN/timeline.txt" && tore_down'

fresh witnessnotready
start WITNESS_NOT_READY=1; wait_run
expect "a witness that never turns Ready stops the run before the warm-up" \
    '[ "$rc" != 0 ] && grep -q "witness pod not Ready" "$RUN/timeline.txt" && ! grep -q "exec witness" "$LOG" && ! grep -q "warmup" "$RUN/timeline.txt" && tore_down'

fresh witnessdies
start WITNESS_DIES=1 SCRAPE_FLAKY=1
if wait_for 'EPP scrape failed 3 polls running' "$RUN/timeline.txt"; then ok "failed EPP scrapes in a row reach the timeline, with the error"; else bad "failed EPP scrapes in a row reach the timeline, with the error"; fi
expect "a witness found dead is re-created from its spec, which outlives the run" \
    'grep -q "witness is Failed: re-creating it" "$RUN/timeline.txt" && grep -q "witness re-created" "$RUN/timeline.txt" && [ "$(grep -c "apply -f .*witness.yaml" "$LOG")" = 2 ] && grep -q "delete pod witness" "$LOG" && grep -q "sleep infinity" "$RUN/witness.yaml"'
kill -TERM "$pid" 2>/dev/null; wait_run

fresh witnesspending
start WITNESS_PENDING=1
wait_for 'llm_d_epp' "$RUN/epp-metrics.log"
for i in $(seq 1 10); do sleep 0.2; done
expect "a Pending witness is left to come up, not deleted and made again"     '[ -s "$RUN/epp-metrics.log" ] && ! grep -q "re-creating" "$RUN/timeline.txt" && ! grep -q "delete pod witness" "$LOG" && [ "$(grep -c "apply -f .*witness.yaml" "$LOG")" = 1 ]'
kill -TERM "$pid" 2>/dev/null; wait_run

fresh armfail
start ARM_FAIL=1 STAIRCASE_ARM_TRIES=2; wait_run
expect "no deadline: no standup, and teardown" '[ "$rc" != 0 ] && ! grep -q benchmark-standup "$LOG" && tore_down'

fresh term
start MAKE_SLEEP=60
if wait_for 'make .*benchmark-standup' "$LOG" && wait_for . "$STATE/standup-sleep"; then
    begin=$(date +%s); kill -TERM "$pid"; wait_run; took=$(($(date +%s) - begin))
    expect "TERM during the standup tears down at once: ${took}s" 'tore_down && [ "$took" -lt 15 ]'
    expect "and the standup itself is stopped" '! kill -0 "$(cat "$STATE/standup-sleep")" 2>/dev/null'
else
    bad "the standup never started"; bad "no TERM case"
fi

fresh abortstandup
start MAKE_SLEEP=60
if wait_for . "$STATE/standup-sleep"; then
    touch "$RUN/abort"; wait_run
    expect "abort during the standup" '[ "$rc" != 0 ] && tore_down && ! kill -0 "$(cat "$STATE/standup-sleep")" 2>/dev/null'
else bad "abort during the standup"; fi

fresh hangcp
start HANG=cp
if wait_for ' cp ' "$LOG"; then
    begin=$(date +%s); kill -TERM "$pid"; wait_run; took=$(($(date +%s) - begin))
    expect "TERM with a hung cp lands: ${took}s" '[ "$took" -lt 25 ] && grep -q "exit rc=143" "$RUN/timeline.txt"'
else bad "TERM with a hung cp"; fi

fresh hangexec
start HANG=exec
if wait_for ' exec ctl ' "$LOG"; then
    touch "$RUN/abort"; begin=$(date +%s); wait_run; took=$(($(date +%s) - begin))
    expect "abort with a hung exec lands: ${took}s" '[ "$took" -lt 25 ] && grep -q "teardown" "$RUN/timeline.txt"'
else bad "abort with a hung exec"; fi

fresh hangget
start HANG=get STAIRCASE_KUBE_TIMEOUT=2
begin=$(date +%s); wait_run; took=$(($(date +%s) - begin))
expect "a hung read of the fleet stops the run, bounded by the kube timeout: ${took}s" '[ "$rc" != 0 ] && [ "$rc" != 99 ] && [ "$took" -lt 30 ] && grep -q "ScaledObjects unreadable" "$RUN/timeline.txt" && tore_down'

fresh hangsnap
start HANG=gzip
if wait_for 'gzip' "$LOG"; then
    begin=$(date +%s); kill -TERM "$pid"; wait_run; took=$(($(date +%s) - begin))
    expect "TERM with snapshot_load hung lands: ${took}s" '[ "$took" -lt 15 ] && grep -q "exit rc=143" "$RUN/timeline.txt"'
else bad "TERM with snapshot_load hung"; fi

fresh abortsnap
start HANG=gzip
if wait_for 'gzip' "$LOG"; then
    begin=$(date +%s); touch "$RUN/abort"; wait_run; took=$(($(date +%s) - begin))
    expect "abort with snapshot_load hung lands: ${took}s" '[ "$took" -lt 15 ] && tore_down'
else bad "abort with snapshot_load hung"; fi

fresh hangready
start HANG=ready STAIRCASE_KUBE_TIMEOUT=2 STAIRCASE_READY_WAIT=60
if wait_for 'get deploy m-decode -o jsonpath' "$LOG"; then
    begin=$(date +%s); touch "$RUN/abort"; wait_run; took=$(($(date +%s) - begin))
    expect "abort with the readiness reads hung lands within one read: ${took}s" '[ "$took" -lt 12 ] && tore_down'
else bad "abort with the readiness reads hung"; fi

fresh gatehang
cat > "$STUB/slowpy" <<'STUB'
#!/usr/bin/env bash
case "$*" in *" gate "*) touch "$STATE/in-gate"; exec sleep 300 ;; esac
exec python3 "$@"
STUB
chmod +x "$STUB/slowpy"
start STAIRCASE_PY="$STUB/slowpy"
wait_for 'waiting for the freeze' "$RUN/timeline.txt"
base_env bash "$B/staircase.sh" freeze > /dev/null 2>&1
for i in $(seq 1 100); do [ -e "$STATE/in-gate" ] && break; sleep 0.2; done
begin=$(date +%s); kill -TERM "$pid"; wait_run; took=$(($(date +%s) - begin))
expect "TERM during the gate's scoring lands: ${took}s" '[ -e "$STATE/in-gate" ] && [ "$took" -lt 15 ] && tore_down'

fresh abortpin
start NEVER_READY=1 STAIRCASE_READY_WAIT=60
if wait_for 'get deploy m-decode -o jsonpath' "$LOG"; then
    begin=$(date +%s); touch "$RUN/abort"; wait_run; took=$(($(date +%s) - begin))
    expect "abort during pin_fleet: ${took}s" '[ "$took" -lt 20 ] && tore_down'
else bad "abort during pin_fleet"; fi

fresh abortlevel
start LOAD_STUCK=1 STAIRCASE_POLL=0.5
if wait_for 'apply -f .*load.yaml' "$LOG"; then
    touch "$RUN/abort"; begin=$(date +%s); wait_run; took=$(($(date +%s) - begin))
    expect "abort while waiting for LEVEL 1: ${took}s" '[ "$took" -lt 20 ] && tore_down'
else bad "abort while waiting for LEVEL 1"; fi

fresh pintimeout
start NEVER_READY=1; wait_run
expect "a fleet that never gets Ready stops the run" '[ "$rc" != 0 ] && tore_down && ! grep -q "patch deployment scaling-manager-controller-manager" "$LOG"'

fresh nofreeze2
start STAIRCASE_FREEZE_WAIT=1; wait_run
expect "no freeze in time stops the run" '[ "$rc" != 0 ] && grep -q "no freeze in time" "$RUN/timeline.txt" && tore_down'

fresh refused
start STAIRCASE_FREEZE_WAIT=60
wait_for 'waiting for the freeze' "$RUN/timeline.txt"
base_env WHATIF_REFUSE=1 bash "$B/staircase.sh" freeze > /dev/null 2>&1
wait_run
expect "a refused freeze aborts the run, not waits out its cap" '[ -e "$RUN/abort" ] && tore_down && [ ! -e "$STATE/steps" ]'

fresh tamper
start TAMPER=1
wait_for 'waiting for the freeze' "$RUN/timeline.txt"
base_env bash "$B/staircase.sh" freeze > /dev/null 2>&1
wait_run
expect "a prediction that moved stops the run after that step" '[ "$rc" != 0 ] && [ "$(steps)" = 1 ] && tore_down'

fresh engine
start ENGINE_ERR=1
wait_for 'waiting for the freeze' "$RUN/timeline.txt"
base_env bash "$B/staircase.sh" freeze > /dev/null 2>&1
wait_run
expect "an engine error stops the run after that step" '[ "$rc" != 0 ] && [ "$(steps)" = 1 ] && grep -q "engine errors: stopping" "$RUN/timeline.txt" && tore_down'

fresh gatefail
start SLOW=1
wait_for 'waiting for the freeze' "$RUN/timeline.txt"
base_env bash "$B/staircase.sh" freeze > /dev/null 2>&1
wait_run
expect "a failed gate stops the run before the held-out phase" '[ "$rc" != 0 ] && [ "$(steps)" = 2 ] && grep -q "the gate failed" "$RUN/timeline.txt" && tore_down'

fresh crashed
start READY_THEN_NOT=1
wait_for 'waiting for the freeze' "$RUN/timeline.txt"
base_env bash "$B/staircase.sh" freeze > /dev/null 2>&1
wait_run
expect "phases whose fleet fails run no load, and fail the gate as missing" '[ "$rc" != 0 ] && [ "$(steps)" = 0 ] && grep -q "the gate failed" "$RUN/timeline.txt" && grep -q "missing" "$RUN/gate-r1.out" && tore_down'

fresh abortwait
start STAIRCASE_FREEZE_WAIT=600
wait_for 'waiting for the freeze' "$RUN/timeline.txt"
base_env bash "$B/staircase.sh" abort > /dev/null 2>&1
wait_run
expect "abort at the freeze wait tears down" 'tore_down'

echo "an API server that does not answer..."
# r2 lost B-on-r1 to this on the EPP patch, and C-off-r1 to it on the EPP restart.
TRANSIENT_MSG='Unable to connect to the server: net/http: request canceled (Client.Timeout exceeded while awaiting headers)'
on_patches() { grep -c "patch cm eppcm .*epp-patch-on" "$LOG"; }
# frozen_run <VAR=value...> : a whole run, frozen when it waits.
frozen_run() {
    start "$@"
    wait_for 'waiting for the freeze' "$RUN/timeline.txt"
    base_env bash "$B/staircase.sh" freeze > /dev/null 2>&1
    wait_run
}
fresh flaky
frozen_run FLAKY=epp-patch-on FLAKY_N=1 FLAKY_MSG="$TRANSIENT_MSG"
expect "a patch the API server did not answer is sent again, and the phase goes on" \
    '[ "$rc" = 0 ] && [ "$(steps)" = 3 ] && [ "$(on_patches)" = 2 ] && grep -q "phase B-on-r1 rc=0" "$RUN/timeline.txt"'
expect "the retry is in the timeline, with the error" \
    'grep -q "no answer from the API server (try 1 of 4).*Client.Timeout exceeded while awaiting headers" "$RUN/timeline.txt"'

fresh flakyever
frozen_run FLAKY=epp-patch-on FLAKY_N=99 FLAKY_MSG="$TRANSIENT_MSG"
expect "one that never answers is tried 4 times, then the phase fails and the gate stops the run" \
    '[ "$rc" != 0 ] && [ "$(on_patches)" = 4 ] && grep -q "EPP config patch failed" "$RUN/timeline.txt" && grep -q "the gate failed" "$RUN/timeline.txt" && tore_down'

expect "each try is named, up to the last but one" \
    'grep -q "try 2 of 4" "$RUN/timeline.txt" && grep -q "try 3 of 4" "$RUN/timeline.txt" && ! grep -q "try 4 of 4" "$RUN/timeline.txt"'

for c in "notfound|Error from server (NotFound): configmaps \"eppcm\" not found" \
         "both|Error from server (Forbidden): configmaps \"eppcm\" is forbidden: context deadline exceeded"; do
    fresh "kr${c%%|*}"
    frozen_run FLAKY=epp-patch-on FLAKY_N=99 FLAKY_MSG="${c#*|}"
    expect "an answer is not asked again (${c%%|*}), whatever transient word it carries" \
        '[ "$rc" != 0 ] && [ "$(on_patches)" = 1 ] && ! grep -q "no answer from the API server" "$RUN/timeline.txt" && grep -q "EPP config patch failed" "$RUN/timeline.txt"'
done

fresh krhung
frozen_run HANG=patchon STAIRCASE_KUBE_TIMEOUT=1
expect "a call killed by its own timeout is not sent again" \
    '[ "$(on_patches)" = 1 ] && ! grep -q "no answer from the API server" "$RUN/timeline.txt" && grep -q "EPP config patch failed" "$RUN/timeline.txt"'

# What cost r2 C-off-r1: the EPP restart, once unanswered.
target="rollout restart deploy/m-epp"
fresh krrestartepp
frozen_run FLAKY="$target" FLAKY_N=1 FLAKY_MSG="$TRANSIENT_MSG"
expect "an EPP restart the API server did not answer is sent again, and the run goes on" \
    '[ "$rc" = 0 ] && [ "$(steps)" = 3 ] && grep -q "kubectl rollout restart deploy/m-epp: no answer from the API server (try 1 of 4)" "$RUN/timeline.txt"'
# The controller restart: once unanswered it is sent again; never answered, the phase
# fails rather than run on the last phase's controller and recording.
target="rollout restart deployment/scaling-manager-controller-manager"
fresh krrestartctl
frozen_run FLAKY="$target" FLAKY_N=1 FLAKY_MSG="$TRANSIENT_MSG"
expect "a controller restart the API server did not answer is sent again, and the run goes on" \
    '[ "$rc" = 0 ] && [ "$(steps)" = 3 ] && grep -q "kubectl rollout restart deployment/scaling-manager-controller-manager: no answer from the API server (try 1 of 4)" "$RUN/timeline.txt"'
fresh krrestartctlever
frozen_run FLAKY="$target" FLAKY_N=99 FLAKY_MSG="$TRANSIENT_MSG"
expect "a controller restart that never lands fails its phase: no load runs on the old controller" \
    '[ "$rc" != 0 ] && [ "$(steps)" = 0 ] && grep -q "controller restart failed" "$RUN/timeline.txt" && tore_down'

fresh krabort
start FLAKY=epp-patch-on FLAKY_N=99 FLAKY_MSG="$TRANSIENT_MSG" STAIRCASE_KUBE_RETRY_WAIT=30
wait_for 'waiting for the freeze' "$RUN/timeline.txt"
base_env bash "$B/staircase.sh" freeze > /dev/null 2>&1
if wait_for 'no answer from the API server (try 1 of 4)' "$RUN/timeline.txt"; then
    begin=$(date +%s); touch "$RUN/abort"; wait_run; took=$(($(date +%s) - begin))
    expect "an abort in a retry's wait lands at once and sends nothing more: ${took}s" \
        '[ "$took" -lt 15 ] && [ "$(on_patches)" = 1 ] && ! grep -q "try 2 of 4" "$RUN/timeline.txt" && tore_down'
else bad "an abort in a retry's wait"; fi

fresh kraborted
mkdir -p "$RUN"
(
    export PATH="$RUN_PATH" STAIRCASE_LIB=1 BENCHMARK_NAMESPACE=stair-ns STAIRCASE_RUN="$RUN" FLAKY=epp-patch-on FLAKY_N=99 \
        FLAKY_MSG="$TRANSIENT_MSG" STAIRCASE_KUBE_RETRY_WAIT=30 STAIRCASE_TICK=0.1
    # shellcheck source=/dev/null
    . "$B/staircase.sh"
    CHILD=
    touch "$R/abort"
    kretry patch cm eppcm --type merge --patch-file "$R/epp-patch-on.json" 2> /dev/null
)
expect "a run already aborting sends no retry and waits for none" \
    '[ "$(on_patches)" = 1 ] && ! grep -q "no answer from the API server" "$RUN/timeline.txt" 2>/dev/null'

fresh forbidden
frozen_run FLAKY=epp-patch-on FLAKY_N=99 FLAKY_MSG='Error from server (Forbidden): configmaps "eppcm" is forbidden: User "u" cannot patch resource "configmaps"'
expect "a Forbidden is an answer: sent once, the phase fails and the gate stops the run" \
    '[ "$rc" != 0 ] && [ "$(on_patches)" = 1 ] && ! grep -q "no answer from the API server" "$RUN/timeline.txt" && grep -q "EPP config patch failed" "$RUN/timeline.txt" && grep -q "the gate failed" "$RUN/timeline.txt" && tore_down'

echo "resume..."
# On the run the Forbidden stopped: B-off-r1 done, B-on-r1 not, the gate failed, torn down.
first_steps=$(steps); frozen_sum=$(sha256sum < "$RUN/frozen.json"); run_id=$(cat "$RUN/run-id")
resume_run() {
    env_line
    setsid "${exec_env[@]}" NS_EXISTS=1 "$@" bash "$B/staircase.sh" resume "${RESUME_IMG:-img}" > "$RUN.resume.out" 2>&1 &
    pid=$!
    PIDS="$PIDS $pid"
}
# refused <name> <message> <VAR=value...> : a resume that must stop before it changes
# anything: its message, no kubectl call that writes, and no prior-<n> made.
refused() {
    local name=$1 msg=$2
    shift 2
    : > "$LOG"
    base_env NS_EXISTS=1 "$@" bash "$B/staircase.sh" resume "${RESUME_IMG:-img}" > "$RUN.resume.out" 2>&1
    rc=$?
    expect "$name" '[ "$rc" != 0 ] && grep -q "$msg" "$RUN.resume.out" && ! grep -qE " (create|delete|label|patch|apply|rollout) " "$LOG" && [ ! -e "$RUN/prior-1" ]'
}
RESUME_IMG=other refused "another image than the run started on is refused" "started on image .img., not .other."
expect "and not one kubectl call was made" '[ ! -s "$LOG" ]'
cp "$RUN/frozen.json" "$WORK/frozen.keep"; echo " " >> "$RUN/frozen.json"
refused "a frozen file off its hash is refused, before any call" "do not match their hashes"
cp "$WORK/frozen.keep" "$RUN/frozen.json"
for f in run-id frozen.sha256; do
    mv "$RUN/$f" "$WORK/$f.keep"
    refused "a run with no $f is refused, before any call" "$f missing"
    expect "and no kubectl call was made ($f)" '[ ! -s "$LOG" ]'
    mv "$WORK/$f.keep" "$RUN/$f"
done
mv "$RUN/STOP" "$WORK/STOP.keep"; touch "$RUN/heartbeat"
refused "a runner that beat moments ago, with no STOP, is refused, its age named" "the run beat [0-9]*s ago and never stopped"
mv "$WORK/STOP.keep" "$RUN/STOP"
echo "2026-11-10T00:00:00Z exit rc=1: teardown" >> "$RUN/timeline.txt"
refused "a last session with no confirmed teardown is refused" "no confirmed teardown after the last session"
"$TOOLS/sed" -i '$d' "$RUN/timeline.txt"
refused "a namespace that cannot be read is refused, not taken as absent" "could not be read: not taken as absent" NS_FORBID=1
refused "a namespace another run owns is refused, nothing deleted" "is not this run's" LEASE_RUN=theirs LABEL_RUN=theirs
refused "a namespace still holding workloads is refused" "still holds 1 workloads" LEFT=1
rm -f "$STATE/job-gone"
refused "a deadline Job still standing is refused" "the deadline Job stands"
touch "$STATE/job-gone"
mv "$RUN/frozen-config/policy.yaml" "$WORK/policy.keep"
refused "a config snapshot missing a file is refused, before any call" "frozen-config/policy.yaml missing"
mv "$WORK/policy.keep" "$RUN/frozen-config/policy.yaml"
expect "the snapshot was written once, by the first session" '[ -s "$RUN/frozen-config/pd-config-on.yaml" ] && [ "$(cat "$RUN/frozen-config/prefill-args.txt")" = "serve m" ] && grep -q gpu "$RUN/frozen-config/node-selectors.json"'
done_run=$RUN
RUN=$WORK/flaky
refused "a run with every phase done is refused" "every phase of the ladder is done"
RUN=$done_run

# same_config: each compared file, moved in the snapshot, stops the resume after the standup.
for f in policy.yaml pd-config-on.yaml prefill-args.txt; do
    cp "$RUN/frozen-config/$f" "$WORK/$f.keep"; echo tampered >> "$RUN/frozen-config/$f"
    : > "$LOG"
    resume_run; wait_run
    expect "same_config: a fleet whose $f is not the frozen run's stops before any phase, and tears down" \
        '[ "$rc" != 0 ] && grep -q "$f is not the one the run was frozen on" "$RUN/timeline.txt" && [ "$(steps)" = "$first_steps" ] && tore_down'
    if [ "$f" = policy.yaml ]; then
        # The refused session's own files are now the run's: the snapshot still holds.
        : > "$LOG"
        resume_run; wait_run
        expect "same_config: a second resume after a refused one is refused too" \
            '[ "$rc" != 0 ] && [ "$(grep -c "policy.yaml is not the one the run was frozen on" "$RUN/timeline.txt")" = 2 ] && [ "$(steps)" = "$first_steps" ]'
    fi
    cp "$WORK/$f.keep" "$RUN/frozen-config/$f"
done
: > "$LOG"
# The namespace is gone, and the runner's heartbeat is old with no STOP: it goes on.
rm -f "$RUN/STOP"; touch -d '1 hour ago' "$RUN/heartbeat"
resume_run NS_EXISTS=; wait_run
expect "a resume runs only what did not finish, and scores the gate over both sessions" \
    '[ "$rc" = 0 ] && [ "$(($(steps) - first_steps))" = 2 ] && grep -q "phase B-off-r1 already done" "$RUN/timeline.txt" && grep -q "phase B-on-r1 rc=0" "$RUN/timeline.txt" && grep -q "gate, repeat 1: rc=0" "$RUN/timeline.txt"'
expect "with no warm-up and no freeze: the frozen files are the first session's" \
    'grep -q "run resume image=img" "$RUN/timeline.txt" && [ "$(sed -n "/run resume image=img/,\$p" "$RUN/timeline.txt" | grep -c "warmup")" = 0 ] && ! grep -q "/warmup/" "$LOG" && [ "$(sha256sum < "$RUN/frozen.json")" = "$frozen_sum" ]'
expect "the last gate and the unfinished phase are kept apart, the namespace made again and claimed under the same id" \
    '[ -s "$RUN/prior-1/gate-r1.out" ] && [ -d "$RUN/prior-1/phases/B-on-r1" ] && [ -s "$RUN/prior-1/bin/staircase.sh" ] && [ "$(cat "$RUN/run-id")" = "$run_id" ] && grep -q "create namespace stair-ns" "$LOG" && grep -q "label --overwrite namespace stair-ns llm-d-scaling-manager/staircase-run=$run_id" "$LOG" && tore_down'
expect "each phase says which session ran it" '[ -s "$RUN/phases/B-on-r1/session" ] && [ "$(cat "$RUN/phases/B-on-r1/session")" != "$(cat "$RUN/phases/B-off-r1/session")" ]'

echo "a dead runner..."
fresh kill9
start STAIRCASE_FREEZE_WAIT=60
if wait_for 'waiting for the freeze' "$RUN/timeline.txt" && wait_for 'patch cm staircase-lease' "$LOG"; then
    kill -KILL "$pid"
    sleep 3
    before=$(leases); sleep 3; after=$(leases)
    expect "kill -9: the lease stops being renewed" '[ "$before" = "$after" ]'
    expect "and the capture loop ends with the main shell" 'grep -q "the main shell is gone" "$RUN/timeline.txt"'
else bad "kill -9 case"; bad "kill -9 case"; fi

fresh stale
mkdir -p "$RUN"
env_line
(
    export PATH="$RUN_PATH" STAIRCASE_LIB=1 BENCHMARK_NAMESPACE=stair-ns STAIRCASE_RUN="$RUN" STAIRCASE_POLL=0.1 STAIRCASE_LEASE_EVERY=0 STAIRCASE_HEART_MAX=100
    # shellcheck source=/dev/null
    . "$B/staircase.sh"
    echo $(($(date +%s) + 99999)) > "$R/deadline-epoch.txt"
    touch -d '1 hour ago' "$R/heartbeat"
    sleep 30 & main=$!
    (sleep 1; touch "$R/STOP") &
    capture_loop "$main" > /dev/null 2>&1
    kill "$main" 2>/dev/null
)
expect "a stale heartbeat stops renewal, with the main shell alive" '[ "$(leases)" = 0 ] && grep -q "lease not renewed" "$RUN/timeline.txt"'

fresh jobdead
mkdir -p "$RUN"
(
    export PATH="$RUN_PATH" STAIRCASE_LIB=1 BENCHMARK_NAMESPACE=stair-ns STAIRCASE_RUN="$RUN" JOB_DEAD=1
    # shellcheck source=/dev/null
    . "$B/staircase.sh"
    echo $(($(date +%s) + 99999)) > "$R/deadline-epoch.txt"
    JOB_MISSES=0
    job_check; job_check
    [ ! -e "$R/abort" ] || exit 1
    job_check
    [ -e "$R/abort" ]
)
expect "a deadline Job that stopped aborts the run, on the third read" '[ $? = 0 ]'

fresh capnear
mkdir -p "$RUN"
(
    export PATH="$RUN_PATH" STAIRCASE_LIB=1 BENCHMARK_NAMESPACE=stair-ns STAIRCASE_RUN="$RUN"
    # shellcheck source=/dev/null
    . "$B/staircase.sh"
    date +%s > "$R/deadline-epoch.txt"
    JOB_MISSES=0
    job_check
    [ -e "$R/abort" ]
)
expect "a hard cap that nears aborts the run" '[ $? = 0 ]'

echo "teardown..."
fresh nolws
mkdir -p "$RUN"; echo id > "$RUN/run-id"
if base_env bash "$B/staircase.sh" teardown > "$RUN.out" 2>&1; then ok "no LeaderWorkerSet API does not fail it"; else bad "no LeaderWorkerSet API does not fail it"; cat "$RUN.out"; fi
expect "and the kinds after it are still deleted" 'grep -q "delete daemonset --all" "$LOG"'

fresh getfail
mkdir -p "$RUN"; echo id > "$RUN/run-id"
if base_env GET_FAIL=1 bash "$B/staircase.sh" teardown > "$RUN.out" 2>&1; then bad "a failed read must not read as nothing left"; else ok "a failed read does not read as nothing left"; fi
expect "the Job stays, and the message is loud" '! grep -q "delete job staircase-deadline" "$LOG" && grep -q "TEARDOWN NOT CONFIRMED" "$RUN.out"'

fresh leftover
mkdir -p "$RUN"; echo id > "$RUN/run-id"
if base_env LEFT=1 bash "$B/staircase.sh" teardown > "$RUN.out" 2>&1; then bad "a workload left must fail teardown"; else ok "a workload left fails teardown"; fi
expect "and the Job stays" '! grep -q "delete job staircase-deadline" "$LOG"'

fresh rescue
mkdir -p "$RUN/phases/B-off-r1"; echo id > "$RUN/run-id"; echo "$RUN/phases/B-off-r1" > "$RUN/load-dir-lg-b-off-r1"
base_env LOAD_ALIVE=1 bash "$B/staircase.sh" teardown > /dev/null 2>&1
expect "teardown saves a load pod before deleting it" '[ -s "$RUN/phases/B-off-r1/requests.csv.gz" ] && [ "$(grep -n "gzip" "$LOG" | head -1 | cut -d: -f1)" -lt "$(grep -n "delete pod -l app=loadgen" "$LOG" | head -1 | cut -d: -f1)" ]'

echo "captures..."
fresh snapshots
start LOAD_RUNS=1 STAIRCASE_FREEZE_WAIT=60
if wait_for 'gzip' "$LOG"; then
    wait_for . "$RUN/warmup/sweep/requests.csv.gz"
    expect "the requests are on disk while the load runs" '[ -s "$RUN/warmup/sweep/requests.csv.gz" ] && kill -0 "$pid" 2>/dev/null'
    kill -TERM "$pid"; wait_run
else bad "snapshot on a poll"; fi
fresh partial
mkdir -p "$RUN"
echo "a good archive" > "$RUN/requests.csv.gz"
(
    export PATH="$RUN_PATH" STAIRCASE_LIB=1 BENCHMARK_NAMESPACE=stair-ns STAIRCASE_RUN="$RUN" SNAP_PARTIAL=1 STAIRCASE_TICK=0.1
    # shellcheck source=/dev/null
    . "$B/staircase.sh"
    CHILD=
    snapshot_load "$R" lg-x
)
expect "a copy cut short replaces nothing, however long" '[ "$(cat "$RUN/requests.csv.gz")" = "a good archive" ]'
(
    export STAIRCASE_LIB=1
    # shellcheck source=/dev/null
    . "$B/staircase.sh"
    cd "$WORK" || exit 1
    echo "the whole capture" > saved
    echo x > candidate
    keep_larger candidate saved
    [ "$(cat saved)" = "the whole capture" ] && [ ! -e candidate ] || exit 1
    echo "a longer capture than before" > candidate
    keep_larger candidate saved
    [ "$(cat saved)" = "a longer capture than before" ]
)
expect "a capture never shrinks the saved copy, and grows it" '[ $? = 0 ]'
(
    export STAIRCASE_LIB=1
    # shellcheck source=/dev/null
    . "$B/staircase.sh"
    cd "$WORK" || exit 1
    rm -f first-saved
    echo "a first copy" > candidate
    said=$(keep_larger candidate first-saved 2>&1)
    [ -z "$said" ] && [ "$(cat first-saved)" = "a first copy" ]
)
expect "the first save of a file saves it, and says nothing" '[ $? = 0 ]'
(
    export STAIRCASE_LIB=1
    # shellcheck source=/dev/null
    . "$B/staircase.sh"
    cd "$WORK" || exit 1
    [ -z "$(keep_larger no-such-candidate first-saved 2>&1)" ]
)
expect "a capture that never came replaces nothing, and says nothing" '[ $? = 0 ] && [ -s "$WORK/first-saved" ]'

echo "the smoke run..."
start_smoke() {
    env_line
    setsid "${exec_env[@]}" STAIRCASE_SMOKE_STEP=31 STAIRCASE_SMOKE_ABORT=1 "$@" bash "$B/staircase.sh" smoke img > "$RUN.out" 2>&1 &
    pid=$!
    PIDS="$PIDS $pid"
}
fresh smoke
start_smoke; wait_run
expect "a smoke run passes, aborted on purpose" '[ "$rc" = 0 ] && grep -q "smoke: passed" "$RUN.out"'
expect "it starts at 1+1 and moves to 1+2" 'grep -q "benchmark-standup.*BENCHMARK_DECODE_REPLICAS=1 " "$LOG" && grep -q "so-d --type merge -p {\"spec\":{\"minReplicaCount\":2" "$LOG"'
expect "and no warm-up, freeze or ladder" '[ ! -e "$RUN/ladder.ready" ] && ! grep -q "warmup" "$RUN/timeline.txt"'
expect "it reads the label, the lease and the Job rights, and each line says" '[ "$(grep -c "^ok" "$RUN/smoke.txt")" -ge 16 ] && ! grep -q "^FAIL" "$RUN/smoke.txt"'
expect "the abort landed and the teardown confirmed" 'grep -q "abort landed" "$RUN/smoke.txt" && grep -q "teardown confirmed" "$RUN/smoke.txt" && tore_down'
expect "the aborted load was saved before its pod went" 'grep -q "aborted load was saved" "$RUN/smoke.txt"'

fresh smokebad
start_smoke LEASE_RUN=theirs; wait_run
expect "a smoke run with a check that fails fails" '[ "$rc" != 0 ] && grep -q "^FAIL" "$RUN/smoke.txt"'

fresh smokerbac
start_smoke CAN_I=no; wait_run
expect "a Job without its rights fails the smoke" '[ "$rc" != 0 ] && grep -q "^FAIL the Job may deletecollection" "$RUN/smoke.txt"'

fresh smokepin
start_smoke NEVER_MOVE=1; wait_run
expect "a fleet that will not move fails the smoke, and tears down" '[ "$rc" != 0 ] && grep -q "^FAIL the fleet did not move" "$RUN/smoke.txt" && tore_down'

echo "tensor parallelism..."
expect "unasked, the standup sets no tensor-parallel size or prefill memory" \
    'grep -q "^set prefill.replicas=1," "$WORK/smoke.log" && ! grep -q "parallelism\|prefill.resources" "$WORK/smoke.log"'
fresh tp
start_smoke STAIRCASE_TP=2 STAIRCASE_PREFILL_MEMORY=64Gi STUB_GPUS=2; wait_run
expect "asked, the standup sets both roles' size and the prefill memory" \
    'grep -q "^set .*,prefill.parallelism.tensor=2,decode.parallelism.tensor=2,prefill.resources.requests.memory=64Gi,prefill.resources.limits.memory=64Gi$" "$LOG"'
expect "and a fleet that holds that many GPUs a pod runs" '[ "$rc" = 0 ] && grep -q "smoke: passed" "$RUN.out"'
fresh tpwrong
start_smoke STAIRCASE_TP=2 STUB_GPUS=1; wait_run
expect "a fleet that does not hold them stops before any load, and tears down" \
    '[ "$rc" != 0 ] && grep -q "asks for .1. GPUs, not the tensor-parallel size 2" "$RUN/timeline.txt" && [ ! -d "$RUN/smoke" ] && tore_down'

echo "the lapse run..."
start_lapse() {
    env_line
    setsid "${exec_env[@]}" LAPSE_HOLD=1 STAIRCASE_LAPSE_LEASE=1 STAIRCASE_LAPSE_WAIT=30 "$@" bash "$B/staircase.sh" lapse img > "$RUN.out" 2>&1 &
    pid=$!
    PIDS="$PIDS $pid"
}
fresh lapse
start_lapse
if wait_for 'runner killed' "$RUN/smoke.txt"; then
    runner_gone=$(grep -c 'exit rc=' "$RUN/timeline.txt")
    touch "$STATE/freed"
    wait_run
    expect "the runner dies with no exit path of its own" '[ "$runner_gone" = 0 ]'
    expect "the Job frees the workloads, and the run says so" '[ "$rc" = 0 ] && grep -q "the Job freed them" "$RUN/smoke.txt" && grep -q "deadline was reached" "$RUN/smoke.txt"'
    expect "and it still confirms a teardown" 'tore_down && grep -q "lapse: passed" "$RUN.out"'
else bad "the lapse run"; bad "no lapse case"; bad "no lapse case"; fi

fresh lapsefail
start_lapse STAIRCASE_LAPSE_WAIT=1; wait_run
expect "a Job that frees nothing fails the lapse, and it still tears down" '[ "$rc" != 0 ] && grep -q "^FAIL still holding" "$RUN/smoke.txt" && tore_down'

echo "this check..."
reap_sessions() {
    local p
    for p in $PIDS; do kill -KILL "-$p" 2>/dev/null; done
    sleep 0.3
}
reap_sessions
if pgrep -f "$WORK" > "$WORK.left" 2>/dev/null; then
    bad "no process is left naming the work directory"
    while read -r p; do tr '\0' ' ' < "/proc/$p/cmdline" | cut -c1-200; echo; done < "$WORK.left"
else
    ok "no process is left naming the work directory"
fi
rm -f "$WORK.left"

echo "$checks checks, $fails failed"
if [ "$checks" != "$EXPECTED_CHECKS" ]; then
    echo "FAIL: ran $checks checks, expected $EXPECTED_CHECKS" >&2
    exit 1
fi
[ "$fails" = 0 ]
