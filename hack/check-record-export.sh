#!/usr/bin/env bash
# Runs the fetch and check steps of the capacity planner's manual recording
# (docs/guides/record-for-capacity-planner/guide.yaml, manual.fetch and
# manual.check) against a fake Prometheus (hack/testdata/record-export/fakeprom.py),
# then converts what they saved.
#
# What it guards exits 0 when wrong: a cache-tier series dropped from
# CACHE_METRICS (the export then never holds it), a series the fleet does not
# export read as a failed query (an older engine's export refused), a failed
# query read as an absent series (a window that lost data without a word),
# and the LMCache server's series looked up in the engines' namespace when it
# runs in another.
#
# Needs bash, curl 7.76 or later, jq, yq and python3.
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
GUIDE="$ROOT/docs/guides/record-for-capacity-planner/guide.yaml"
FAKE="$ROOT/hack/testdata/record-export/fakeprom.py"
CONVERT="$ROOT/hack/promjson-to-openmetrics.py"
TMP=$(mktemp -d)
PID=
cleanup() {
  [ -n "$PID" ] && kill "$PID" 2>/dev/null
  rm -rf "$TMP"
}
trap cleanup EXIT
fail=0

FETCH=$(yq -r ".manual.fetch.run" "$GUIDE")
CHECK=$(yq -r ".manual.check.run" "$GUIDE")
# Both extractions must hold the steps: two empty scripts run clean.
case "$FETCH" in *"CACHE_METRICS="*) ;; *)
  echo "FAIL: manual.fetch in $GUIDE has no CACHE_METRICS list" >&2; exit 1 ;; esac
case "$CHECK" in *"bad="*) ;; *)
  echo "FAIL: manual.check in $GUIDE did not extract" >&2; exit 1 ;; esac

# serve starts the fake Prometheus with its arguments; PROM_URL is its URL.
serve() {
  [ -n "$PID" ] && kill "$PID" 2>/dev/null && wait "$PID" 2>/dev/null
  rm -f "$TMP/port"
  python3 "$FAKE" --namespace llm --lmcache-namespace lmc --port-file "$TMP/port" "$@" &
  PID=$!
  for _ in $(seq 1 50); do [ -s "$TMP/port" ] && break; sleep 0.1; done
  [ -s "$TMP/port" ] || { echo "FAIL: the fake Prometheus did not start" >&2; exit 1; }
  PROM_URL="http://127.0.0.1:$(cat "$TMP/port")"
}

# record runs fetch then check in a fresh directory, as the guide's reader
# runs them in one shell; its output is in $TMP/$1.out.
record() {
  local dir="$TMP/$1"
  shift
  mkdir -p "$dir"
  (
    cd "$dir" || exit 1
    export PROM_URL NAMESPACE=llm FROM=1700000000 TO=1700000600 CHUNK=300 CURL_OPTS=""
    for kv in "$@"; do export "${kv?}"; done
    bash -c "$FETCH
$CHECK"
  ) > "$dir.out" 2>&1
}

PRESENT="vllm:kv_cache_usage_perc vllm:external_prefix_cache_hits_total vllm:prompt_tokens_cached_by_source_total lmcache_mp_lookup_hit_tokens_total llm_d_disagg_sidecar_prefill_duration_seconds_sum vllm:hisparse_cache_hits_total vllm:hisparse_cache_misses_total DCGM_FI_PROF_SM_ACTIVE"

# 1. Some cache-tier series present, the rest absent: nothing fails, every
# CACHE_METRICS name has its two chunks, and the check names which are there.
serve --present "$PRESENT"
record ok LMCACHE_NAMESPACE=lmc
out="$TMP/ok.out"
grep -qx "failed=0" "$out" || { echo "FAIL: an absent series failed the fetch:" >&2; cat "$out" >&2; fail=1; }
grep -qx "bad=0" "$out" || { echo "FAIL: an absent series failed the check:" >&2; cat "$out" >&2; fail=1; }
cache=$(printf '%s\n' "$FETCH" | sed -n '/^export CACHE_METRICS="/,/^"$/p' | sed 's/export CACHE_METRICS=//' | tr -d '"')
n=0
for m in $cache; do
  n=$((n + 1))
  have=$(ls "$TMP/ok/planner-export/${m//:/_}".*.json 2>/dev/null | wc -l)
  [ "$have" -eq 2 ] || { echo "FAIL: $m has $have of 2 chunks" >&2; fail=1; }
done
[ "$n" -ge 158 ] || { echo "FAIL: CACHE_METRICS holds $n names, fewer than the 158 the export list needs" >&2; fail=1; }
for m in vllm:external_prefix_cache_hits_total vllm:prompt_tokens_cached_by_source_total lmcache_mp_lookup_hit_tokens_total llm_d_disagg_sidecar_prefill_duration_seconds_sum vllm:hisparse_ DCGM_FI_PROF_SM_ACTIVE; do
  grep -qx "present: $m" "$out" || { echo "FAIL: the check did not report $m present" >&2; fail=1; }
done
for m in vllm:kv_offload_load_time_total sglang:prefill_effective_tokens_total llm_d_epp_request_cached_tokens_bucket http_request_duration_seconds_sum vllm:request_render_duration_seconds_sum DCGM_FI_DEV_GPU_UTIL; do
  grep -qx "absent: $m" "$out" || { echo "FAIL: the check did not report $m absent" >&2; fail=1; }
done
if grep -q "^present: vllm:kv_cache_usage_perc" "$out"; then
  echo "FAIL: the check reported a series outside CACHE_METRICS" >&2; fail=1
fi

# 1b. What was saved converts: the present cache-tier series reach the
# OpenMetrics file, and the empty answers convert to nothing.
if ! python3 "$CONVERT" -o "$TMP/ok.om" "$TMP"/ok/planner-export/*.json 2>"$TMP/conv.err"; then
  echo "FAIL: the converter refused the export: $(cat "$TMP/conv.err")" >&2; fail=1
else
  for m in vllm:external_prefix_cache_hits_total lmcache_mp_lookup_hit_tokens_total vllm:hisparse_cache_hits_total vllm:hisparse_cache_misses_total DCGM_FI_PROF_SM_ACTIVE; do
    grep -q "^$m{" "$TMP/ok.om" || { echo "FAIL: $m did not reach the converted file" >&2; fail=1; }
  done
fi

# 2. Without LMCACHE_NAMESPACE the LMCache server's series is looked up in
# the engines' namespace, where this fake does not hold it: absent, not failed.
record nolmc
grep -qx "absent: lmcache_mp_lookup_hit_tokens_total" "$TMP/nolmc.out" || {
  echo "FAIL: LMCACHE_NAMESPACE is not what finds the LMCache server's series" >&2; fail=1; }

# 2b. DCGM series are selected by DCGM_SELECTOR, not by the engines'
# namespace label: one that selects another namespace reads them absent.
grep -qx "absent: DCGM_FI_PROF_SM_ACTIVE" "$TMP/nolmc.out" && {
  echo "FAIL: the default DCGM_SELECTOR did not find the GPU's Pod namespace" >&2; fail=1; }
record dcgm LMCACHE_NAMESPACE=lmc "DCGM_SELECTOR=exported_namespace='other'"
grep -qx "absent: DCGM_FI_PROF_SM_ACTIVE" "$TMP/dcgm.out" || {
  echo "FAIL: DCGM_SELECTOR is not what selects the DCGM series" >&2; fail=1; }

# 3. A cache-tier query that fails is a failure, not an absence.
serve --present "$PRESENT" --fail "vllm:kv_offload_load_bytes_total"
record failing LMCACHE_NAMESPACE=lmc
out="$TMP/failing.out"
grep -qx "failed=1" "$out" || { echo "FAIL: a failed cache-tier query did not fail the fetch" >&2; cat "$out" >&2; fail=1; }
grep -q "^FAILED: vllm:kv_offload_load_bytes_total at " "$out" || { echo "FAIL: the fetch did not name the failed series" >&2; fail=1; }
grep -qx "bad=1" "$out" || { echo "FAIL: a failed cache-tier query passed the check" >&2; fail=1; }

# 4. The check names each empty kube-state-metrics series it reads, with why
# and what is lost; one the fleet exports is not named.
out="$TMP/ok.out"
for m in kube_pod_created kube_pod_deletion_timestamp kube_deployment_annotations kube_node_labels; do
  grep -q "^EMPTY: $m: " "$out" || { echo "FAIL: the check did not name the empty $m" >&2; fail=1; }
done
grep -q "^EMPTY: kube_node_labels: .*--metric-labels-allowlist=nodes=\[nvidia.com/gpu.product\]" "$out" || {
  echo "FAIL: the kube_node_labels hint does not name the allowlist" >&2; fail=1; }
serve --present "$PRESENT kube_node_labels kube_pod_created"
record ksm LMCACHE_NAMESPACE=lmc
for m in kube_node_labels kube_pod_created; do
  if grep -q "^EMPTY: $m: " "$TMP/ksm.out"; then echo "FAIL: the check named $m empty when it was not" >&2; fail=1; fi
done
grep -q "^EMPTY: kube_deployment_annotations: " "$TMP/ksm.out" || { echo "FAIL: the hints stopped after a present series" >&2; fail=1; }

# 5. Engine arguments from each kind of workload, in Path 2's describe step and
# Path 1's engineargs step: a DisaggregatedSet role picked by its llm-d.ai/role
# label (on the role, or on a template), a LeaderWorkerSet, a Deployment. A
# fake kubectl answers from fixtures.
DESCRIBE=$(yq -r ".manual.describe.run" "$GUIDE")
ARGS1=$(yq -r ".record.engineargs.run" "$GUIDE")
# The fake is run through bash, so it works whether or not its executable bit
# survived the checkout.
FIXBIN="$TMP/bin"
mkdir -p "$FIXBIN"
printf '#!/usr/bin/env bash\nexec bash "%s" "$@"\n' "$ROOT/hack/testdata/record-export/fake-kubectl" > "$FIXBIN/kubectl"
chmod +x "$FIXBIN/kubectl"
WANT_DEC='vllm serve Qwen/Qwen3-32B --max-num-seqs 256 --kv-transfer-config {"kv_connector":"NixlConnector","kv_role":"kv_both"} --kv-offloading-size 64'
WANT_PRE='vllm serve Qwen/Qwen3-32B --tensor-parallel-size 8 --kv-transfer-config {"kv_connector":"NixlConnector","kv_role":"kv_both"} --max-num-batched-tokens 16384'
# with_workloads prints script $1 with its two workload lines set to $2 and $3,
# and fails when either line is not there to set.
with_workloads() {
  local s
  s=$(printf '%s\n' "$1" | sed -e "s|^export DECODE_WORKLOAD=.*|export DECODE_WORKLOAD=$2|" \
    -e "s|^export PREFILL_WORKLOAD=.*|export PREFILL_WORKLOAD=$3|")
  case "$s" in *"export DECODE_WORKLOAD=$2"*"export PREFILL_WORKLOAD=$3"*) printf '%s\n' "$s" ;; *) return 1 ;; esac
}
# describe runs Path 2's describe step for decode $2 and prefill $3 in a fresh
# directory $TMP/$1; its output is $TMP/$1.out and its window.txt beside it.
describe() {
  local dir="$TMP/$1" script
  script=$(with_workloads "$DESCRIBE" "$2" "$3") || { echo "FAIL: the describe step has no workload lines to set" >&2; fail=1; return; }
  mkdir -p "$dir/planner-export"
  ( cd "$dir" || exit 1
    export PATH="$FIXBIN:$PATH" NAMESPACE=llm FROM=1700000000 TO=1700000600
    bash -c "$script" ) > "$TMP/$1.out" 2>&1
}
# args1 runs Path 1's engineargs step the same way, then prints what it exported.
args1() {
  local script
  script=$(with_workloads "$ARGS1" "$2" "$3") || { echo "FAIL: the engineargs step has no workload lines to set" >&2; fail=1; return; }
  ( export PATH="$FIXBIN:$PATH" NAMESPACE=llm
    bash -c "$script
echo \"ENGINE_ARGS=\${ENGINE_ARGS-unset}\"
echo \"PREFILL_ENGINE_ARGS=\${PREFILL_ENGINE_ARGS-unset}\"" ) > "$TMP/$1.out" 2>&1
}
describe dset dset/qwen dset/qwen
w="$TMP/dset/planner-export/window.txt"
grep -qxF "DECODE_ARGS=$WANT_DEC" "$w" 2>/dev/null || { echo "FAIL: describe read the DisaggregatedSet's decode role wrong:" >&2; cat "$TMP/dset.out" >&2; fail=1; }
grep -qxF "PREFILL_ARGS=$WANT_PRE" "$w" 2>/dev/null || { echo "FAIL: describe read the DisaggregatedSet's prefill role wrong:" >&2; cat "$TMP/dset.out" >&2; fail=1; }
args1 dset1 dset/qwen dset/qwen
grep -qxF "ENGINE_ARGS=$WANT_DEC" "$TMP/dset1.out" || { echo "FAIL: Path 1 read the decode role wrong:" >&2; cat "$TMP/dset1.out" >&2; fail=1; }
grep -qxF "PREFILL_ENGINE_ARGS=$WANT_PRE" "$TMP/dset1.out" || { echo "FAIL: Path 1 read the prefill role wrong:" >&2; cat "$TMP/dset1.out" >&2; fail=1; }
describe lws lws/qwen-decode ""
grep -qxF "DECODE_ARGS=Qwen/Qwen3-32B --max-num-seqs 64" "$TMP/lws/planner-export/window.txt" 2>/dev/null || {
  echo "FAIL: describe read the LeaderWorkerSet wrong:" >&2; cat "$TMP/lws.out" >&2; fail=1; }
if grep -q "^PREFILL_ARGS=" "$TMP/lws/planner-export/window.txt" 2>/dev/null; then echo "FAIL: a prefill line without PREFILL_WORKLOAD" >&2; fail=1; fi
args1 deploy1 deploy/qwen-decode ""
grep -qxF "ENGINE_ARGS=Qwen/Qwen3-32B --max-num-seqs 32" "$TMP/deploy1.out" || { echo "FAIL: Path 1 read the Deployment wrong:" >&2; cat "$TMP/deploy1.out" >&2; fail=1; }
grep -qxF "PREFILL_ENGINE_ARGS=unset" "$TMP/deploy1.out" || { echo "FAIL: Path 1 set PREFILL_ENGINE_ARGS without PREFILL_WORKLOAD" >&2; fail=1; }

# 5b. A DisaggregatedSet role named decode but labelled nothing is not read
# by its name: describe stops, says why, and writes no window.txt.
describe unlabelled dset/unlabelled ""
[ -e "$TMP/unlabelled/planner-export/window.txt" ] && { echo "FAIL: describe wrote window.txt for an unlabelled role" >&2; fail=1; }
grep -q "no role labelled llm-d.ai/role=decode" "$TMP/unlabelled.out" || { echo "FAIL: describe did not say the role has no label:" >&2; cat "$TMP/unlabelled.out" >&2; fail=1; }

if [ "$fail" -ne 0 ]; then
  exit 1
fi
echo "OK: the manual recording saves $n cache-tier series, reads absent ones as absent and failed ones as failed, names each empty kube-state-metrics series, and reads engine arguments from a DisaggregatedSet role, a LeaderWorkerSet and a Deployment"
