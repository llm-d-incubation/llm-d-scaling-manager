#!/usr/bin/env bash
# Manage the FMA warm pool: the set of sleeping vLLM instances a scale-up can
# wake instead of rebuilding.
#
# WHY THIS EXISTS, AND WHY IT TAKES REPLICAS
# -----------------------------------------
# FMA reuses a sleeping instance only on an exact instance-ID match, and that ID
# is a hash that INCLUDES the GPU UUIDs (pkg/controller/dual-pods). So a sleeper
# is reusable only by a requester that reserved that same GPU. Two consequences:
#
#   * Sleepers made by the launcher-populator are keyed to GPUs it picked, which
#     no requester will reserve except by chance. They are never woken. Measured:
#     every such bind rebuilt from scratch, ~50s of model load.
#   * Sleepers made by scaling requesters UP then DOWN are keyed to GPUs the
#     scheduler actually hands out, so a later scale-up hits. Measured: 3s.
#
# The durable size of that pool is LauncherPopulationPolicy.launcherCount, which
# is PER NODE -- the populator prints `node=... desired=N` per node key and
# deletes anything above it as "excess launcher pod", warm instances included.
# That is what makes warmth fragile: exceed the count and the populator reaps
# your warm capacity ~20s after a scale-down.
#
# This script takes the pool size in REPLICAS, not per-node counts, and owns the
# translation itself. That is deliberate. The size is the one input that may
# later come from somewhere else -- The scaling manager already computes desired and peak
# replicas -- and a future producer should not need node topology or free-GPU
# accounting to say "keep 6 warm". Placement stays infra config; warming stays a
# separate step; only sizing changes hands. Keep it that way.
#
# Usage:
#   warm_pool.sh size <replicas>   set durable capacity to hold <replicas> warm
#   warm_pool.sh verify [replicas] gate: are that many actually asleep and ready?
#   warm_pool.sh report            what the pool looks like right now
#   warm_pool.sh coverage          covered/free GPUs per node -- the hit rate
#
# Env: WARM_POOL_NS (default $BENCHMARK_NAMESPACE), KUBECONFIG as usual.
set -u
# --help prints this file's header comment -- the documentation the script
# already carries, so it cannot drift from what the script does. Placed before
# any argument handling because several of these take a namespace as $1, and
# without it `--help` was consumed as one.
case "${1:-}" in
    -h|--help)
        sed -n '2,/^[^#]/p' "$0" | sed 's/^# \{0,1\}//; $d'
        exit 0
        ;;
esac


NS="${WARM_POOL_NS:-${BENCHMARK_NAMESPACE:-}}"
[ -n "$NS" ] || { echo "warm_pool: set WARM_POOL_NS or BENCHMARK_NAMESPACE" >&2; exit 2; }

SLEEP_LABEL='dual-pods.llm-d.ai/sleeping'
LAUNCHER_SEL='app.kubernetes.io/component=launcher'

policy_name() {
    kubectl get launcherpopulationpolicy -n "$NS" \
        -o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}

# Nodes the policy is allowed to place launchers on. An empty hostname list
# means "any node matching the label selector", in which case we cannot know the
# count without listing nodes -- do that only then, so the common path is cheap.
policy_nodes() {
    local n
    n=$(kubectl get launcherpopulationpolicy -n "$NS" -o json 2>/dev/null | python3 -c '
import json, sys
try:
    pol = json.load(sys.stdin)["items"][0]
except Exception:
    sys.exit(0)
sel = (pol["spec"].get("enhancedNodeSelector") or {}).get("labelSelector") or {}
for e in sel.get("matchExpressions") or []:
    if e.get("key") == "kubernetes.io/hostname" and e.get("operator") == "In":
        print(len(e.get("values") or []))
        sys.exit(0)
' 2>/dev/null)
    if [ -n "${n:-}" ] && [ "$n" -gt 0 ] 2>/dev/null; then
        echo "$n"; return
    fi
    kubectl get nodes -l nvidia.com/gpu.present=true --no-headers 2>/dev/null | grep -c . || echo 0
}

# Free GPUs per node: allocatable minus what scheduled pods request. allocatable
# is TOTAL capacity and does not drop as pods consume it, so headroom has to be
# computed. This is the denominator of the hit rate -- see cmd_coverage.
free_gpus_by_node() {
    kubectl get nodes -l nvidia.com/gpu.present=true -o json 2>/dev/null > /tmp/wp_nodes.json || return 1
    kubectl get pods -A -o json 2>/dev/null > /tmp/wp_pods.json || return 1
    python3 - <<'PY'
import json
nodes = json.load(open("/tmp/wp_nodes.json"))["items"]
pods = json.load(open("/tmp/wp_pods.json"))["items"]
used = {}
for p in pods:
    if (p.get("status", {}).get("phase") or "") in ("Succeeded", "Failed"):
        continue
    node = (p.get("spec", {}) or {}).get("nodeName")
    if not node:
        continue
    n = 0
    for c in (p["spec"].get("containers") or []):
        res = c.get("resources") or {}
        v = (res.get("requests") or {}).get("nvidia.com/gpu") or \
            (res.get("limits") or {}).get("nvidia.com/gpu")
        try:
            n += int(v or 0)
        except (TypeError, ValueError):
            pass
    if n:
        used[node] = used.get(node, 0) + n
for node in nodes:
    name = node["metadata"]["name"]
    try:
        alloc = int(node["status"]["allocatable"].get("nvidia.com/gpu", 0))
    except (TypeError, ValueError, KeyError):
        alloc = 0
    print("%s %d" % (name, alloc - used.get(name, 0)))
PY
}

# Sleeping instances per node, counted from the LAUNCHERS' own instance lists.
# Pod labels describe the POD, not its instances: a launcher that gets bound and
# builds flips sleeping=false with nothing destroyed, so pod labels overstate
# losses and understate coverage.
sleepers_by_node() {
    kubectl get pods -n "$NS" -l "$LAUNCHER_SEL" \
        -o jsonpath='{range .items[*]}{.metadata.name} {.spec.nodeName}{"\n"}{end}' \
        2>/dev/null | while read -r pod node; do
        [ -n "$pod" ] || continue
        n=$(kubectl exec -n "$NS" "$pod" -c inference-server -- \
            curl -s --max-time 8 http://localhost:8001/v2/vllm/instances 2>/dev/null \
            | python3 -c 'import json,sys
try: print(len(json.load(sys.stdin).get("instances") or []))
except Exception: print(0)' 2>/dev/null || echo 0)
        echo "$node ${n:-0}"
    done | awk '{a[$1]+=$2} END {for (k in a) print k, a[k]}'
}

# What actually predicts a wake.
#
# Reuse keys on the GPU UUID, so a requester wakes a sleeper only if the
# scheduler hands it that exact GPU. The hit rate on a node is therefore
# covered/free, and the pool-wide rate is the average over the nodes a requester
# can land on -- NOT "is there a sleeper somewhere".
#
# This is why spreading is the wrong instinct: 4 sleepers on 4 nodes of 5 free
# GPUs each gives 1/5 per node, while the same 4 concentrated on one node with 4
# free GPUs gives 4/4. Same cost, ~5x the hit rate. Measured: spread pools gave
# 0/3, 0/3 and 1/3; a saturated node gave 3/3.
cmd_coverage() {
    echo "  node                     free  covered  hit rate"
    local tf=0 tc=0
    free_gpus_by_node > /tmp/wp_free.txt || { echo "  cannot read nodes/pods" >&2; return 1; }
    sleepers_by_node  > /tmp/wp_cov.txt  || true
    while read -r node free; do
        [ -n "$node" ] || continue
        local cov
        cov=$(awk -v n="$node" '$1==n {print $2}' /tmp/wp_cov.txt | head -1)
        cov="${cov:-0}"
        [ "${free:-0}" -gt 0 ] 2>/dev/null || [ "$cov" -gt 0 ] 2>/dev/null || continue
        local rate="n/a"
        if [ "${free:-0}" -gt 0 ] 2>/dev/null; then
            rate=$(awk -v c="$cov" -v f="$free" 'BEGIN{printf "%.0f%%", (c>f?f:c)*100/f}')
        fi
        printf "  %-24s %4s  %7s  %8s\n" "$node" "$free" "$cov" "$rate"
        tf=$((tf + free)); tc=$((tc + cov))
    done < /tmp/wp_free.txt
    echo
    echo "  covered $tc of $tf free GPUs across these nodes"
    echo "  A requester lands on ONE node, so what matters is that node's ratio."
    echo "  For reliable wakes, cover EVERY free GPU on the nodes requesters can"
    echo "  reach -- concentrate the pool rather than spreading it."
}

# Sleepers that are actually usable: labelled sleeping AND Running AND Ready.
# A pod that is sleeping-but-not-ready is not warm capacity, it is a pod that
# happens to be idle.
ready_sleepers() {
    kubectl get pods -n "$NS" -l "$LAUNCHER_SEL" -o json 2>/dev/null | python3 -c '
import json, sys
n = 0
for p in json.load(sys.stdin).get("items", []):
    lbl = (p["metadata"].get("labels") or {}).get("dual-pods.llm-d.ai/sleeping")
    if lbl != "true":
        continue
    if p["status"].get("phase") != "Running":
        continue
    if any(c["type"] == "Ready" and c["status"] == "True"
           for c in p["status"].get("conditions", [])):
        n += 1
print(n)
' 2>/dev/null || echo 0
}

cmd_size() {
    local want="${1:?usage: warm_pool.sh size <replicas>}"
    local pol nodes per
    pol=$(policy_name)
    [ -n "$pol" ] || { echo "warm_pool: no LauncherPopulationPolicy in $NS" >&2; exit 1; }
    nodes=$(policy_nodes)
    [ "${nodes:-0}" -gt 0 ] 2>/dev/null || { echo "warm_pool: no candidate nodes" >&2; exit 1; }

    # launcherCount is per node, so round UP: a pool one slot short of the
    # ceiling means the last scale-up step rebuilds cold, and a run is only as
    # fast as its slowest replica.
    per=$(( (want + nodes - 1) / nodes ))
    [ "$per" -ge 1 ] || per=1

    echo "  policy=$pol nodes=$nodes want=${want} replicas -> launcherCount=$per per node (capacity $((per * nodes)))"
    # Spreading is the wrong shape for wakes, and this is where it happens.
    # Reuse keys on the GPU UUID, so the hit rate on the node a requester lands
    # on is covered/free -- dividing `want` across many nodes drives that ratio
    # DOWN even though the pool is nominally the right size. Measured: pools
    # spread one-per-node gave 0/3, 0/3 and 1/3; a node whose free GPUs were all
    # covered gave 3/3. Warn rather than refuse: spreading is still right for
    # covering several models, or when requesters are unpinned and you want some
    # warmth everywhere.
    if [ "$nodes" -gt 1 ] && [ "$per" -lt 4 ]; then
        echo
        echo "  NOTE: this spreads $want replicas over $nodes nodes, $per per node."
        echo "        A requester lands on ONE node and is handed ONE of its free GPUs,"
        echo "        so its chance of waking is that node's covered/free ratio -- $per out"
        echo "        of however many GPUs are free there, typically 4-8. For reliable"
        echo "        wakes, concentrate: restrict the policy to fewer nodes and cover"
        echo "        every free GPU on them. Check with:  warm_pool.sh coverage"
    fi
    kubectl patch launcherpopulationpolicy "$pol" -n "$NS" --type=merge \
        -p "{\"spec\":{\"countForLauncher\":[{\"launcherConfigName\":\"$(kubectl get launcherpopulationpolicy "$pol" -n "$NS" -o jsonpath='{.spec.countForLauncher[0].launcherConfigName}' 2>/dev/null)\",\"launcherCount\":$per}]}}" >/dev/null \
        && echo "  patched" || { echo "  patch FAILED" >&2; exit 1; }
}

cmd_verify() {
    local want="${1:-}" have nodes
    have=$(ready_sleepers)
    if [ -z "$want" ]; then
        echo "  sleeping + Running + Ready launchers: $have"
        return 0
    fi
    echo "  want >= $want warm, have $have"
    if [ "$have" -ge "$want" ]; then
        echo "  OK"
        return 0
    fi
    echo "  NOT WARM: a scale-up will rebuild instead of waking (~50s per replica)" >&2
    return 1
}

cmd_report() {
    echo "  --- launchers ---"
    kubectl get pods -n "$NS" -l "$LAUNCHER_SEL" \
        -o custom-columns="NAME:.metadata.name,NODE:.spec.nodeName,SLEEPING:.metadata.labels.dual-pods\.llm-d\.ai/sleeping,BOUND:.metadata.labels.dual-pods\.llm-d\.ai/dual" \
        --no-headers 2>/dev/null | sed 's/^/    /'
    echo "  --- instances (GPU is the reuse key) ---"
    for p in $(kubectl get pods -n "$NS" -l "$LAUNCHER_SEL" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
        kubectl exec -n "$NS" "$p" -c inference-server -- \
            curl -s --max-time 6 http://localhost:8001/v2/vllm/instances 2>/dev/null \
        | POD="$p" python3 -c '
import json, os, sys
raw = sys.stdin.read().strip()
pod = os.environ["POD"][-6:]
if not raw:
    print("    %-8s <no answer>" % pod); sys.exit()
try:
    d = json.loads(raw)
except Exception:
    print("    %-8s <unparseable>" % pod); sys.exit()
inst = d.get("instances") or []
if not inst:
    print("    %-8s <no instances>" % pod)
for i in inst:
    print("    %-8s inst=%-15s gpu=%s" % (
        pod, (i.get("instance_id") or "")[:14], (i.get("gpu_uuids") or ["-"])[0][:22]))
' 2>/dev/null
    done
    echo "  --- capacity ---"
    echo "    policy nodes: $(policy_nodes)   launcherCount: $(kubectl get launcherpopulationpolicy -n "$NS" -o jsonpath='{.items[0].spec.countForLauncher[0].launcherCount}' 2>/dev/null)"
    echo "    warm now:     $(ready_sleepers)"
}

case "${1:-}" in
    size)   shift; cmd_size "$@" ;;
    verify) shift; cmd_verify "$@" ;;
    report)   shift; cmd_report "$@" ;;
    coverage) shift; cmd_coverage "$@" ;;
    *) sed -n '/^# Usage:/,/^# Env:/p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
