#!/usr/bin/env bash
# The benchmark to run once, right after installing the scaling manager, to see it work.
#
#   hack/benchmark/smoke.sh <namespace>
#   make benchmark-smoke NAMESPACE=<namespace>
#
# Symmetric 1024-in/1024-out load at 15 req/s for five minutes against WHAT IS
# ALREADY DEPLOYED, then a dashboard snapshot if there is a dashboard to
# snapshot. It answers one question -- is the scaling manager actually scaling this? -- and
# answers it in five minutes.
#
# Override with SMOKE_PROFILE=decode-heavy, REQUEST_RATE=<n>, SMOKE_MINUTES=<n>.
# This header used to say decode-heavy at 10 req/s, which is what the script did
# before the rate and profile were changed for the reasons given at RATE below.
#
# Deliberately NOT llm-d-benchmark. That suite stands up its own stack and needs
# a CLI, uv, helm and helmfile; its numbers are for comparing runs. This needs a
# cluster you can already reach and nothing else: it finds the service that is
# serving, asks it which model it has, and sends requests. Plain Kubernetes and
# OpenShift alike -- no Route, no oc, no platform assumption anywhere.
#
# NOT a measurement of the model. Five minutes of arrivals is not a latency
# study, and a small model's TTFT says nothing about a large one's. For numbers
# worth comparing, use `make benchmark-run`.
#
# Decode-heavy on purpose: long outputs hold KV cache and keep requests running,
# and that is what saturation is computed from. The same rate with short outputs
# finishes each request sooner and can leave the signal too brief to see move.
set -o pipefail

case "${1:-}" in
    -h|--help)
        sed -n '2,/^[^#]/p' "$0" | sed -e 's/^# //' -e 's/^#//' -e '$d'
        exit 0
        ;;
esac

NS="${1:-${BENCHMARK_NAMESPACE:-${NAMESPACE:-}}}"
[ -n "$NS" ] || { echo "usage: smoke.sh <namespace>   (or set NAMESPACE)" >&2; exit 2; }
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# 10 req/s could not saturate anything and so could not demonstrate scaling: on
# Qwen3-0.6B it held ~115 concurrent requests, KV at 27%, both queues at 0, and
# utilisation flat at 0.46 against a 0.85 threshold. 40 req/s overshoots -- it
# drove KV to 99.9% and the engine queue to 431 running / 12 waiting, scaling
# 1 -> 8 in one step, which measures a stampede rather than a scaling decision.
# 15 is the middle: enough offered concurrency to pass the engine's default
# batch width of 256 and produce a real queue, without burying the engine.
RATE="${REQUEST_RATE:-15}"
MINUTES="${SMOKE_MINUTES:-5}"

# Workload shape.
#
# decode-heavy -- a short prompt, a long generation -- was the original default
# and is what a chat-style stack looks like. It does not move the autoscaler on
# a small model. Measured on Qwen3-0.6B at 10 req/s: ~115 concurrent requests
# holding ~570 KV tokens each, 65k against a 135k-token supply, so utilisation
# peaked at 48% against a scale-up threshold of 85%. Nothing was saturated and
# the scaling manager correctly held at one replica -- a run that cannot demonstrate the thing
# the smoke test exists to demonstrate.
#
# symmetric sends a prompt the same size as the generation, roughly 3.6x the KV
# per request, which does cross the threshold. It is the default for that
# reason; decode-heavy stays available for stacks already large enough to
# saturate without it.
# In-flight ceiling for the open loop. This has to sit ABOVE the engine's batch
# width or the load generator becomes the bottleneck instead of the engine: vLLM
# batches 256 sequences by default, and the old hard-coded cap of 200 meant the
# engine never filled, never queued, and so never produced the saturation signal
# the autoscaler needs. computeK2's Priority 1 ("observed") requires a saturated
# queue; below the batch width it can never fire, capacity stays a formula-based
# estimate, and utilisation sits flat however much load is offered.
MAX_INFLIGHT="${MAX_INFLIGHT:-600}"

PROFILE="${SMOKE_PROFILE:-symmetric}"
case "$PROFILE" in
    symmetric)    PROMPT_TOKENS="${PROMPT_TOKENS:-1024}" ; MAX_TOKENS="${MAX_TOKENS:-1024}" ;;
    decode-heavy) PROMPT_TOKENS="${PROMPT_TOKENS:-16}"   ; MAX_TOKENS="${MAX_TOKENS:-1024}" ;;
    *) echo "SMOKE_PROFILE must be 'symmetric' or 'decode-heavy' (got: $PROFILE)" >&2; exit 2 ;;
esac
OUT="${SMOKE_OUT:-$REPO/benchmark-smoke-$NS}"
# A Kubernetes test image, not a Docker Hub one: registry.k8s.io is mirrored
# where docker.io often is not, and this has to pull on the cluster under test.
LOADER_IMAGE="${SMOKE_IMAGE:-registry.k8s.io/e2e-test-images/agnhost:2.47}"

say()  { printf '\n\033[0;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m    %s\033[0m\n' "$*" >&2; }

# --------------------------------------------------------- is the stack there
# Every piece is checked BEFORE any load is sent, and all of them are reported
# before exiting. Five minutes of traffic against a stack that cannot report is
# five minutes spent proving nothing -- and being told about one missing piece at
# a time, five minutes apart, is worse than being told about all of them now.
#
# The chain is: model servers serve, the EPP schedules and publishes the queue,
# the scaling manager reads both and decides, KEDA owns the HPA and actuates. A gap anywhere and
# the replica count does not move, which is exactly what this run is looking at.
say "Checking the stack in $NS"
MISSING=0
ok()   { printf '  \033[0;32m+\033[0m %s\n' "$*"; }
gone() { printf '  \033[0;31m-\033[0m %s\n' "$*"; MISSING=$((MISSING + 1)); }
fix()  { printf '      %s\n' "$*"; }

# KEDA: cluster-scoped, and the CRD is the thing that matters -- a ScaledObject
# cannot exist without it, and the scaling manager publishes decisions THROUGH it.
if kubectl get crd scaledobjects.keda.sh >/dev/null 2>&1; then
    ok "KEDA (scaledobjects.keda.sh)"
else
    gone "KEDA is not installed (no scaledobjects.keda.sh CRD)"
    fix "the installer adds it when the cluster has none: make deploy-scaling-manager NAMESPACE=$NS"
fi

# The scaling manager: a controller that manages THIS namespace. One that watches another is not
# the same as one watching yours, and reads identically in `kubectl get deploy -A`.
LLMSM_DEPLOY="$(kubectl get deploy -A -l app.kubernetes.io/name=scaling-manager \
    -o jsonpath='{range .items[*]}{.metadata.namespace}{"|"}{.spec.template.spec.containers[0].env[?(@.name=="LLMSM_WATCH_NAMESPACE")].value}{"|"}{.status.readyReplicas}{"\n"}{end}' 2>/dev/null)"
LLMSM_FOR_NS=""
while IFS='|' read -r wns watch ready; do
    [ -n "$wns" ] || continue
    case "$watch" in
        # The literal is the base default and means "its own namespace"; only an
        # install given LLMSM_WATCH_NS patches a real name in.
        ''|'$(POD_NAMESPACE)') watch="$wns" ;;
    esac
    if [ "$watch" = "$NS" ] && [ "${ready:-0}" -ge 1 ]; then LLMSM_FOR_NS="$wns"; fi
done <<EOF
$LLMSM_DEPLOY
EOF
if [ -n "$LLMSM_FOR_NS" ]; then
    ok "the scaling manager controller in $LLMSM_FOR_NS, managing $NS"
else
    gone "No ready the scaling manager controller manages $NS"
    fix "make deploy-scaling-manager NAMESPACE=$NS"
fi

# Model servers: what there is to scale. Read from the pod template, so a
# workload parked at zero still counts.
. "$REPO/deploy/lib/common.sh" 2>/dev/null || true
. "$REPO/deploy/lib/scaledobject.sh" 2>/dev/null || true
. "$REPO/deploy/lib/infra_monitoring.sh" 2>/dev/null || true
SERVERS=0
if command -v llmsm_serving_workload_count >/dev/null 2>&1; then
    SERVERS="$(llmsm_serving_workload_count "$NS" 2>/dev/null || echo 0)"
fi
if [ "${SERVERS:-0}" -gt 0 ]; then
    ok "$SERVERS llm-d model server(s)"
else
    gone "No llm-d model servers in $NS"
    fix "install llm-d there first; this drives load, it does not deploy models"
fi

# EPP: the scheduler. Without it there is no queue signal, and queued demand is
# the input that makes a decode-heavy run scale at all.
EPPS=""
command -v llmsm_epp_services >/dev/null 2>&1 && EPPS="$(llmsm_epp_services "$NS" 2>/dev/null)"
[ -n "$EPPS" ] || EPPS="$(kubectl get svc -n "$NS" -o name 2>/dev/null \
    | grep -E "router-epp|epp$" | cut -d/ -f2)"
if [ -n "$EPPS" ]; then
    ok "EPP: $(printf '%s' "$EPPS" | tr '\n' ',' | sed 's/,$//')"
else
    gone "No EPP (InferencePool endpoint picker) in $NS"
    fix "see docs/guides/testing-with-llm-d/ for a stack with one"
fi

# Registration: The scaling manager scales what a ScaledObject names, and nothing else.
SO_COUNT="$(kubectl get scaledobject -n "$NS" --no-headers 2>/dev/null | wc -l | tr -d ' ')"
if [ "${SO_COUNT:-0}" -gt 0 ]; then
    ok "$SO_COUNT ScaledObject(s) registered"
else
    gone "No ScaledObject in $NS, so the scaling manager is scaling nothing there"
    fix "make scaledobjects-plan NAMESPACE=$NS   then  make scaledobjects-apply"
fi

if [ "$MISSING" -gt 0 ]; then
    echo
    echo "  $MISSING piece(s) missing. Nothing was run." >&2
    echo "  A fuller check, with the metrics side too:  make check-prereqs NAMESPACE=$NS" >&2
    exit 1
fi

# The service already serving. Same detection wait_serving.sh uses, and the port
# is chosen BY NAME: a router-epp service exposes grpc-ext-proc first, so
# ports[0] is gRPC and sending HTTP at it gets nothing back at all.
SVC="${SMOKE_ENDPOINT_SVC:-}"
[ -n "$SVC" ] || SVC="$(kubectl get svc -n "$NS" -o name 2>/dev/null \
    | grep -E "router-epp|inference-gateway|epp$" | head -1 | cut -d/ -f2)"
[ -n "$SVC" ] || { echo "  No router/EPP service in $NS. Set SMOKE_ENDPOINT_SVC=<service>." >&2; exit 1; }
PORT="$(kubectl get svc "$SVC" -n "$NS" -o json 2>/dev/null | python3 -c '
import json, sys
ports = json.load(sys.stdin)["spec"].get("ports", [])
for p in ports:
    if p.get("name") == "http":
        print(p["port"]); sys.exit(0)
for p in ports:
    if p.get("port") in (80, 8000, 8080):
        print(p["port"]); sys.exit(0)
if ports:
    print(ports[0]["port"])')"
[ -n "$PORT" ] || { echo "  Could not resolve an HTTP port on $SVC." >&2; exit 1; }
BASE="http://${SVC}.${NS}.svc.cluster.local:${PORT}"
echo "  Endpoint: $BASE"

# Ask the endpoint what it serves rather than being told. A model name that does
# not match what is loaded returns an error per request, which looks like load
# being driven and produces no saturation at all.
MODEL="${MODEL_ID:-}"
if [ -z "$MODEL" ]; then
    MODEL="$(kubectl run smoke-probe-$$ -n "$NS" --rm -i --restart=Never --quiet \
        --image="$LOADER_IMAGE" --command -- \
        /bin/sh -c "curl -s --max-time 10 ${BASE}/v1/models" 2>/dev/null \
        | python3 -c 'import json,sys
try:
    print(json.load(sys.stdin)["data"][0]["id"])
except Exception:
    pass' 2>/dev/null)"
fi
[ -n "$MODEL" ] || { echo "  $BASE/v1/models named no model. Pass MODEL_ID=<id>." >&2; exit 1; }
echo "  Model:    $MODEL"

# ------------------------------------------------------------------------ load
say "Driving ${PROFILE} load at ${RATE} req/s for ${MINUTES}m (prompt ~${PROMPT_TOKENS} tok, generation ${MAX_TOKENS} tok)"
START_EPOCH="$(date -u +%s)"
JOB="scaling-manager-smoke-$(date -u +%H%M%S)"
DURATION="$((MINUTES * 60))"
GAP="$(awk "BEGIN{printf \"%.3f\", 1/${RATE}}")"
# Prompt filler, built here so the in-pod script stays small. ~18 tokens per
# sentence, so PROMPT_TOKENS/18 repeats lands near the requested size without
# needing a tokenizer in the pod.
REPS="$(awk "BEGIN{n=int(${PROMPT_TOKENS}/18); if(n<1)n=1; print n}")"
FILLER="$(awk -v n="$REPS" 'BEGIN{s="";for(i=0;i<n;i++)s=s "The quick brown fox jumps over the lazy dog and then continues running through the forest. ";print s}')"

# One pod, open loop: a request every 1/RATE seconds whether or not the previous
# one finished. A closed loop (N workers in lockstep) throttles itself exactly
# when the server slows down, which is the moment worth measuring. In-flight is
# capped so a stalled server cannot fan out without bound.
LOAD_SCRIPT="end=\$(( \$(date +%s) + ${DURATION} ))
sent=0
while [ \$(date +%s) -lt \$end ]; do
  if [ \$(jobs -p | wc -l) -lt ${MAX_INFLIGHT} ]; then
    # The nonce goes FIRST, and it is what makes a long prompt cost anything.
    # Every request used to send a byte-identical prompt, so vLLM prefix-cached
    # it once and every later request reused those blocks -- a 1024-token prompt
    # would have added no KV at all and the run would have measured decode while
    # calling itself symmetric. A unique leading nonce misses on the first block
    # and forces the whole prompt to be computed and held per request.
    printf '{\"model\":\"%s\",\"prompt\":\"%s %s\",\"max_tokens\":${MAX_TOKENS},\"temperature\":0.7}' \
           '${MODEL}' \"req\$sent-\$(date +%s%N)\" '${FILLER}' \
      | curl -s -o /dev/null -w '%{http_code}\\n' --max-time 120 -H 'Content-Type: application/json' \\
             --data-binary @- '${BASE}/v1/completions' >> /tmp/codes 2>/dev/null &
    sent=\$((sent+1))
  fi
  sleep ${GAP}
done
wait
# Requests SENT is not requests SERVED, and only the second answers whether the
# run is worth reading. This reported dispatch alone while telling the operator
# to confirm the error count was zero -- a number it did not produce. curl writes
# each status to /tmp/codes; 000 means the request never completed at all, and
# counts as failed here because it is one.
ok=\$(grep -c '^2' /tmp/codes 2>/dev/null) || ok=0
bad=\$(grep -cv '^2' /tmp/codes 2>/dev/null) || bad=0
echo \"dispatched \$sent requests; \$ok ok, \$bad failed\""

python3 - "$NS" "$JOB" "$LOADER_IMAGE" "$LOAD_SCRIPT" <<'PYEOF' | kubectl apply -n "$NS" -f - >/dev/null
import json, sys
ns, job, image, script = sys.argv[1:5]
print(json.dumps({
    "apiVersion": "batch/v1", "kind": "Job",
    "metadata": {"name": job, "namespace": ns,
                 "labels": {"app.kubernetes.io/name": "scaling-manager-smoke"}},
    "spec": {"backoffLimit": 0, "template": {
        "metadata": {"labels": {"app.kubernetes.io/name": "scaling-manager-smoke"}},
        "spec": {"restartPolicy": "Never", "containers": [{
            "name": "load", "image": image,
            "command": ["/bin/sh", "-c"], "args": [script],
            "resources": {"requests": {"cpu": "100m", "memory": "128Mi"}}}]}}}}))
PYEOF
echo "  Job/${JOB} started"
kubectl wait --for=condition=complete --timeout=$((DURATION + 180))s \
    "job/${JOB}" -n "$NS" >/dev/null 2>&1 \
    || warn "the load job did not report complete; reporting on what did happen"
kubectl logs -n "$NS" "job/${JOB}" --tail=1 2>/dev/null | sed 's/^/  /'
kubectl delete job "${JOB}" -n "$NS" --wait=false >/dev/null 2>&1

# -------------------------------------------------------------------- snapshot
# GATED on a dashboard existing. The snapshot's value is that it is the
# dashboard's own queries over the run's window; with no dashboard deployed there
# is nothing to point the reader at afterwards, and a panels.json nobody can
# render is not a result. So look for one, and if there is none, say how to get
# one rather than writing a file and calling it a snapshot.
say "Looking for a dashboard to snapshot"
DASH_KIND=""
DASH_WHERE=""
if [ "$(kubectl get grafana -n "$NS" --no-headers 2>/dev/null | wc -l | tr -d ' ')" != "0" ]; then
    DASH_KIND="grafana-operator"; DASH_WHERE="$NS"
else
    for cm_ns in "$NS" "${MONITORING_NAMESPACE:-scaling-manager-monitoring}" \
                 openshift-user-workload-monitoring; do
        if kubectl get configmap scaling-manager-operation-dashboard -n "$cm_ns" >/dev/null 2>&1; then
            DASH_KIND="configmap"; DASH_WHERE="$cm_ns"; break
        fi
    done
fi

SNAP=""
if [ -z "$DASH_KIND" ]; then
    warn "No dashboard in reach, so nothing was snapshotted. The load still ran."
    warn "  OpenShift:  make dashboard NAMESPACE=$NS   (private Grafana, dashboard imported)"
    warn "  Kubernetes: import deploy/grafana/operational-dashboard.json into your Grafana"
else
    echo "  Found: $DASH_KIND in $DASH_WHERE"
    # The installer's detection answers "which Prometheus does the CONTROLLER
    # use", and that answer is an in-cluster Service address. This script runs on
    # a laptop, where it resolves to nothing:
    #
    #   curl https://thanos-querier.openshift-monitoring.svc.cluster.local:9091/... -> 000
    #
    # which failed the capture while every other step succeeded. So resolve a URL
    # reachable from HERE, and fall back to a port-forward, which needs no Route
    # and works the same on both platforms.
    PROM="${PROMETHEUS_URL:-}"
    PF_PID=""
    if [ -z "$PROM" ]; then
        ROUTE_HOST="$(kubectl get route -n openshift-monitoring thanos-querier \
            -o jsonpath='{.spec.host}' 2>/dev/null || true)"
        if [ -n "$ROUTE_HOST" ]; then
            PROM="https://${ROUTE_HOST}"
        else
            # No Route (plain Kubernetes, or a tenant who cannot read one).
            # Forward to whichever Prometheus service the cluster has.
            for cand in "openshift-monitoring/thanos-querier/9091" \
                        "${MONITORING_NAMESPACE:-scaling-manager-monitoring}/prometheus-operated/9090" \
                        "monitoring/prometheus-operated/9090"; do
                pf_ns="${cand%%/*}"; pf_rest="${cand#*/}"
                pf_svc="${pf_rest%%/*}"; pf_port="${pf_rest##*/}"
                kubectl get svc "$pf_svc" -n "$pf_ns" >/dev/null 2>&1 || continue
                kubectl port-forward -n "$pf_ns" "svc/$pf_svc" ":${pf_port}" >/tmp/scaling-manager-smoke-pf.$$ 2>&1 &
                PF_PID=$!
                for _ in 1 2 3 4 5 6 7 8 9 10; do
                    local_port="$(sed -n 's|.*127.0.0.1:\([0-9]*\).*|\1|p' /tmp/scaling-manager-smoke-pf.$$ 2>/dev/null | head -1)"
                    [ -n "$local_port" ] && break
                    sleep 1
                done
                if [ -n "${local_port:-}" ]; then
                    [ "$pf_port" = "9090" ] && PROM="http://127.0.0.1:${local_port}" \
                                            || PROM="https://127.0.0.1:${local_port}"
                    echo "  Port-forwarded $pf_ns/$pf_svc"
                    break
                fi
                kill "$PF_PID" 2>/dev/null; PF_PID=""
            done
        fi
    fi
    if [ -z "$PROM" ]; then
        warn "Could not reach a Prometheus from here; pass PROMETHEUS_URL=<url> to snapshot."
    else
        echo "  Prometheus: $PROM"
        mkdir -p "$OUT"
        # The run's own elapsed window, not a fixed --since: a slow start would
        # otherwise push the interesting minutes off the left of every chart.
        SINCE="$(( ($(date -u +%s) - START_EPOCH) / 60 + 2 ))m"
        # The CALLER's token first. A `default` ServiceAccount token is scoped to
        # the namespace and is refused by the platform monitoring stack, so
        # preferring it produced a 403 on exactly the cluster where the route
        # works. Whoever is running this already has whatever access they have.
        TOK="${PROMETHEUS_TOKEN:-}"
        if [ -z "$TOK" ] && command -v oc >/dev/null 2>&1; then
            TOK="$(oc whoami -t 2>/dev/null || true)"
        fi
        [ -n "$TOK" ] || TOK="$(kubectl create token default -n "$NS" 2>/dev/null || true)"
        # `python3 <script>`, not the script's own shebang: a checkout made on
        # Windows carries CRLF, and the kernel then looks for an interpreter
        # literally named `python3\r`. The file is fine; the shebang is the only
        # thing that cares, so do not go through it.
        if python3 "$REPO/hack/benchmark/snapshot.py" --namespace "$NS" \
                --prometheus-url "$PROM" ${TOK:+--token "$TOK"} --insecure \
                --since "$SINCE" --out "$OUT/snapshot" >/tmp/scaling-manager-smoke-snap.$$ 2>&1; then
            SNAP="$OUT/snapshot"
            echo "  Captured $SINCE of dashboard data"
        else
            warn "Capture failed:"
            tail -3 /tmp/scaling-manager-smoke-snap.$$ | sed 's/^/      /' >&2
            warn "  The run itself is unaffected."
        fi
        rm -f /tmp/scaling-manager-smoke-snap.$$
        [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null
        rm -f /tmp/scaling-manager-smoke-pf.$$
    fi
fi

# ---------------------------------------------------------------------- report
echo
echo "================================================================="
echo " the scaling manager smoke: $NS"
echo "================================================================="
echo
if [ -n "$SNAP" ]; then
    echo "  Snapshot: $SNAP/panels.json"
    echo "  Render:   hack/benchmark/snapshot-images/render.sh $SNAP"
fi
ROUTE="$(kubectl get route -n "$NS" \
    -o jsonpath='{.items[?(@.metadata.name=="scaling-manager-grafana-route")].spec.host}' 2>/dev/null || true)"
if [ -n "$ROUTE" ]; then
    echo "  Live:     https://${ROUTE}/d/scaling-manager-op-dash-v2/scaling-manager-operational-dashboard?var-namespace=${NS}"
fi
cat <<REPORT

  What to look for, in this order:
    1. replicas MOVED. Flat at one through a deep queue means the scaling manager never saw the
       load -- a metrics problem wearing a scaling decision's clothes.
    2. \"0 failed\" on the dispatch line above. Anything else and the latency
       panels are measuring retries, not service.
    3. queue depth and KV cache non-empty. Empty means the EPP or the model
       servers are not scraped: make check-prereqs NAMESPACE=$NS

  Replica history, with no dashboard at all:
    kubectl get hpa -n $NS -w

REPORT
