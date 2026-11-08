#!/usr/bin/env bash
# Sample serving replica counts for the duration of a benchmark run.
#
# The harness already writes metrics/processed/replica_status_timeseries.json,
# and on every FMA run measured it came back with snapshots but no controllers:
# collect_metrics.sh filters them with
#
#     model_filter="${LLMDBENCH_HARNESS_STACK_NAME:-}"
#     if model_filter and model != model_filter: continue
#
# comparing a STACK name against the llm-d.ai/model LABEL. Ours are
# "inference-scheduling-llmsm" and "qwen-qwe-...", which never match, so every
# controller is dropped. The variable cannot be overridden from here either:
# run_only.sh writes it into the harness pod spec from endpoint_stack_name,
# which is also used as --stack, so it cannot simply be set to the model.
#
# Rather than depend on that being fixed upstream, sample it ourselves. The
# result is the same shape the harness produces, so postprocess reads it with
# the same code.
#
# Usage:
#   sample_replicas.sh [--context <ctx>] [--force] start <namespace> <outfile>
#   sample_replicas.sh stop <outfile> [<namespace>]
#
# The namespace on stop is optional and only used to warn when it does not
# match the one the capture was started with.
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

# Ownership, argument parsing and the kubectl wrapper are shared with the other
# capture helper. They used to be duplicated per script and had begun to drift.
# shellcheck source=hack/benchmark/capture_lib.sh
. "$(dirname "$0")/capture_lib.sh"

capture_parse_args "$@" || exit 2
set -- ${CAPTURE_POSITIONAL[@]+"${CAPTURE_POSITIONAL[@]}"}

CMD="${1:?usage: $0 [--context <ctx>] [--force] start <namespace> <outfile> | stop <outfile> [<namespace>]}"
INTERVAL="${REPLICA_SAMPLE_INTERVAL:-10}"

_snapshot() {
    local ns="$1" raw
    # The exit status, taken BEFORE python sees anything. Piping kubectl straight
    # in meant a rejected call and an empty namespace produced the same snapshot:
    # one per interval, zero controllers, filed as a measurement. A failed poll is
    # skipped instead -- the convention the rest of this harness already uses --
    # and its reason goes to the capture's stderr channel, which the sampler
    # subshell already points at, so there is no redirect here to discard it.
    raw="$(capture_kube --namespace "$ns" get deployments,statefulsets -o json)" || return 1
    printf '%s' "$raw" | python3 -c '
import json, sys
from datetime import datetime, timezone
try:
    data = json.load(sys.stdin)
except Exception:
    # Unparseable output is a failed poll too, not an empty cluster.
    sys.exit(1)
snap = {"timestamp": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "controllers": []}
for item in data.get("items", []):
    tmpl = item.get("spec", {}).get("template", {}).get("metadata", {}).get("labels", {})
    # Same predicate as the harness, WITHOUT the model filter that empties it:
    # a serving pod template, or the FMA requester.
    if tmpl.get("llm-d.ai/inferenceServing") != "true" and tmpl.get("llm-d.ai/role") != "requester":
        continue
    st = item.get("status", {})
    snap["controllers"].append({
        "name": item.get("metadata", {}).get("name", ""),
        "kind": item.get("kind", "Deployment"),
        "desired_replicas": item.get("spec", {}).get("replicas", 0),
        "ready_replicas": st.get("readyReplicas", 0) or 0,
        "available_replicas": st.get("availableReplicas", 0) or 0,
    })
print(json.dumps(snap))
'
}

# Per-pod timings, so a run can say whether FMA WOKE a sleeping instance or
# rebuilt one. That distinction is invisible in replica counts -- both look like
# "a replica arrived" -- and it is the difference between 3s and ~50-80s. We
# only found it by reading controller logs by hand; measuring it makes it a
# number in the results table instead.
#
# Emitted as JSON lines and deduped at stop, because pods vanish on scale-down:
# a single collection at the end would miss exactly the replicas we care about.
_pods_snapshot() {
    local ns="$1"
    capture_kube --namespace "$ns" get pods -o json \
      | python3 -c '
import json, sys
try:
    data = json.load(sys.stdin)
except Exception:
    sys.exit(0)

# Launcher creation times, so we can tell a pre-existing launcher (which COULD
# have been woken) from one built for this bind (which certainly was not).
launchers = {}
for p in data.get("items", []):
    lb = (p["metadata"].get("labels") or {})
    if lb.get("app.kubernetes.io/component") == "launcher":
        launchers[p["metadata"]["name"]] = p["metadata"].get("creationTimestamp")

for p in data.get("items", []):
    m = p["metadata"]
    lb = m.get("labels") or {}
    # Launchers also carry an inference-serving label, but they are not scale
    # -target replicas -- they are the pool a replica binds INTO. Counting them
    # here would mix the thing being measured with the thing it waits for.
    if lb.get("app.kubernetes.io/component") == "launcher":
        continue
    serving = lb.get("llm-d.ai/inference-serving") == "true" or lb.get("llm-d.ai/inferenceServing") == "true"
    requester = lb.get("llm-d.ai/role") == "requester" or lb.get("app") == "dp-app"
    if not (serving or requester):
        continue
    ready = None
    for c in p.get("status", {}).get("conditions", []):
        if c.get("type") == "Ready" and c.get("status") == "True":
            ready = c.get("lastTransitionTime")
    dual = lb.get("dual-pods.llm-d.ai/dual")
    print(json.dumps({
        "name": m.get("name"),
        "node": p.get("spec", {}).get("nodeName"),
        "created": m.get("creationTimestamp"),
        "ready_at": ready,
        "bound_launcher": dual,
        "launcher_created": launchers.get(dual) if dual else None,
        "is_requester": bool(requester),
    }))
'
}

case "$CMD" in
  start)
    NS="${2:?namespace required}"; OUT="${3:?outfile required}"
    shift 3 || true
    # A stray argument here is almost always a flag in the wrong place, and a
    # dropped --context means watching the wrong cluster while believing
    # otherwise. Refuse rather than proceed.
    [ "$#" -eq 0 ] || { echo "unexpected argument: $1" >&2; exit 2; }
    # Ownership first: "another process owns this" is both more specific and more
    # actionable than "there are bytes here", and the log tail's output is
    # legitimately empty for the first seconds of every capture, so byte count
    # cannot be what protects it. A refusal after the claim gives the claim back.
    capture_claim "$OUT" "$NS" || exit 1
    capture_guard_data "$OUT" || { capture_release "$OUT"; exit 1; }
    capture_verify_writable "$OUT" || { capture_release "$OUT"; exit 1; }
    # Append-only, one JSON object per line. The artefact is assembled from these
    # at stop, so there is no last byte for two stops to fight over.
    #
    # Checked, because the artefact is now a function of this file and nothing
    # else. A stale one that could not be truncated -- read-only, or owned by
    # another user at a reused path -- was assembled as this run's measurement and
    # reached the report table with a previous run's replica counts in it. That is
    # the only way this capture can publish a WRONG number rather than lose a run,
    # and `set -u` without `set -e` is why an unchecked redirect carried on.
    for f in "$OUT.snapshots.jsonl" "$OUT.pods.jsonl" "$OUT.stderr"; do
        if ! : > "$f"; then
            echo "cannot truncate $f -- refusing to start a capture that would" >&2
            echo "  assemble a previous run's data as this one's" >&2
            capture_release "$OUT"
            exit 1
        fi
    done
    # A run that records no pod timings must not file the PREVIOUS run's: the
    # write at stop is best-effort, so a leftover would be picked up as this
    # run's measurement.
    rm -f "$OUT.pod_timings.json"
    # stderr to the capture's own file, not to /dev/null. Round 6 sent the whole
    # subshell to /dev/null to stop it holding the caller's stdout, and took the
    # sampler's last diagnostics channel with it: a sampler recording an empty
    # controller list because of an auth failure became indistinguishable from a
    # namespace with no serving deployments.
    # Job control, so the background capture leads its own process group and one
    # signal at stop reaches every descendant. Without it the sampler's kubectl
    # and python -- grandchildren, because they run inside a command substitution
    # -- outlived the stop and wrote BrokenPipeError into the log after the
    # success line.
    set -m
    (
      while :; do
        snap=$(_snapshot "$NS")
        if [ -n "$snap" ]; then
          printf '%s\n' "$snap" >> "$OUT.snapshots.jsonl"
        fi
        # `|| true` because a failed pod poll must not end the capture -- the
        # replica curve matters more than the timings -- but its reason now lands
        # in the stderr channel instead of /dev/null.
        _pods_snapshot "$NS" >> "$OUT.pods.jsonl" || true
        sleep "$INTERVAL"
      done
    ) >/dev/null 2>>"$OUT.stderr" &
    sampler_pid=$!
    set +m
    # `kill -0` first: the subshell can die on its own redirect before it runs a
    # line of the loop, and $! yields its pid either way. Without this, start
    # printed "replica sampler started" for a process that no longer existed, the
    # record was written for a dead pid -- which is also how `starttime=` comes out
    # EMPTY on an ordinary host -- and the run was filed as {"snapshots": []} with
    # no .startfailed to withhold it.
    if ! kill -0 "$sampler_pid" 2>/dev/null; then
        echo "the replica sampler died immediately after starting -- not recording it" >&2
        echo "  its own diagnostics, if any: $(tail -n 2 "$OUT.stderr" 2>/dev/null)" >&2
        capture_release "$OUT"
        exit 1
    fi
    if ! echo "$sampler_pid" > "$OUT.pid" || ! capture_adopt "$OUT" "$NS" "$sampler_pid"; then
        echo "cannot record the capture at $OUT -- stopping it rather than running unowned" >&2
        kill "$sampler_pid" 2>/dev/null || true
        capture_release "$OUT"
        exit 1
    fi
    echo "replica sampler started (pid $sampler_pid, every ${INTERVAL}s," \
         "context ${CAPTURE_CTX:-inherited}, namespace $NS) -> $OUT"
    ;;
  stop)
    # The two helpers took their stop arguments in opposite orders, and getting
    # them backwards used to exit 0 while leaving the capture running. Recognise
    # the path instead of trusting the position.
    : "${2:?outfile required}"
    # A LONE argument that owns no record and looks nothing like a path is almost
    # certainly a namespace given in the other script's order. Accepting it as the
    # outfile exited 0, said "nothing to stop", and left the sampler running.
    # Whatever the argument count. Gating this on a lone argument left
    # `stop <ns> <other>` exiting 0 with "nothing to stop" and the capture still
    # running -- the combination this check exists to remove.
    if ! capture_owned "$2" && ! capture_owned "${3:-}"; then
        case "$2$3" in
            */*) : ;;
            *) echo "no capture owns \"$2\", and it does not look like an output path" >&2
               echo "  usage: $0 stop <outfile> [<namespace>]" >&2
               exit 2 ;;
        esac
    fi
    capture_resolve_outfile "$2" "${3:-}"
    OUT="$CAPTURE_OUT"; NS_ARG="$CAPTURE_NS"
    if ! capture_owned "$OUT"; then
      echo "no capture owns $OUT -- nothing to stop, and nothing written" >&2
      exit 0
    fi
    capture_check_owner "$OUT" "$NS_ARG" "$CAPTURE_CTX"
    # If the capture would not die, stop here. Writing the JSON terminator and
    # dropping the record would leave a live capture invisible to the tooling,
    # still appending to a file that already looks finished.
    capture_stop "$OUT" "$OUT.pid" || exit 1
    # Assemble the artefact from the appended lines. A pure function of an
    # append-only input, written to a temp file and renamed, so it is idempotent:
    # two concurrent stops produce identical bytes and neither has to exclude the
    # other, and a stop killed at any point leaves the lines for the next one.
    #
    # This replaces seven rounds of machinery whose only job was "exactly one
    # process appends the last byte exactly once". Needing a holder identity, a
    # staleness rule and a crash window is what kept going wrong; an append-only
    # input needs none of the three.
    #
    # An empty snapshots list reads as "not measured" downstream, which is the
    # honest answer for a capture that recorded nothing -- unlike a zero replica
    # count.
    if ! python3 - "$OUT.snapshots.jsonl" "$OUT" <<'PYASM'
import json, os, sys
src, dst = sys.argv[1], sys.argv[2]
snaps = []
# An ABSENT input is an error, not an empty measurement. Reading it as empty is
# how a concurrent stop substituted {"snapshots": []} for a real capture: valid
# JSON, wrong content, and filed as the run's measurement.
with open(src, encoding="utf-8") as fh:
    for line in fh:
        line = line.strip()
        if not line:
            continue
        try:
            snaps.append(json.loads(line))
        except ValueError:
            # A line torn by a kill mid-write is dropped rather than fatal: the
            # snapshots before it are still a measurement.
            continue
tmp = dst + ".assembling." + str(os.getpid())
with open(tmp, "w", encoding="utf-8") as fh:
    json.dump({"snapshots": snaps}, fh)
os.replace(tmp, dst)
PYASM
    then
      # Silence here was the old shape: a stop that could not write its artefact
      # and reported success anyway. The capture is already down, so say what
      # happened and fail -- the recipe then withholds the file rather than filing
      # whatever was at the path.
      echo "could not assemble $OUT from its captured lines" >&2
      capture_finish "$OUT"
      exit 1
    fi
    # Dedupe the pod observations into one record per pod. Keep the observation
    # that has a Ready time: a pod is seen several times, and only later samples
    # carry the transition we want.
    # Keyed to this output, not a shared name: two runs in different
    # namespaces both wrote /tmp/llmsm_pod_timings.json and the second won.
    TIMINGS="$OUT.pod_timings.json"
    python3 - "$OUT.pods.jsonl" "$TIMINGS" <<'PY' 2>/dev/null || true
import json, sys
src, dst = sys.argv[1], sys.argv[2]
best = {}
try:
    for line in open(src, encoding="utf-8"):
        line = line.strip()
        if not line:
            continue
        try:
            r = json.loads(line)
        except Exception:
            continue
        n = r.get("name")
        if not n:
            continue
        prev = best.get(n)
        # Prefer a record that knows when the pod became Ready, and one that
        # knows which launcher it bound to -- both appear only after the fact.
        if (prev is None
                or (r.get("ready_at") and not prev.get("ready_at"))
                or (r.get("bound_launcher") and not prev.get("bound_launcher"))):
            best[n] = r
except FileNotFoundError:
    pass
json.dump({"pods": list(best.values())}, open(dst, "w", encoding="utf-8"))
print("  pod timings: %d pod(s) -> %s" % (len(best), dst))
PY
    # The pod observations are consumed into their own artefact above, so they go.
    # The SNAPSHOT lines stay: they are what makes assembly re-runnable, which is
    # the whole reason for appending them, and deleting them created a race that
    # turned "the data was removed" into "there was no data" -- a second stop
    # landing in the 40-80 ms window wrote {"snapshots": []} over a good artefact
    # in 19 of 20 trials. The path carries the run id and `start` truncates it.
    rm -f "$OUT.pods.jsonl"
    # The path as an ARGUMENT: interpolated into the source, a quote in it was a
    # syntax error that 2>/dev/null turned into an empty count.
    n=$(python3 - "$OUT" <<'PYCOUNT' 2>/dev/null
import json, sys
try:
    d = json.load(open(sys.argv[1], encoding="utf-8"))
    s = d.get("snapshots", [])
    c = sum(len(x.get("controllers", [])) for x in s)
    print("%d snapshot(s), %d controller sample(s)" % (len(s), c))
except Exception as e:
    print("unreadable:", e)
PYCOUNT
)
    capture_finish "$OUT"
    echo "replica sampler stopped: $n -> $OUT"
    # The count above already parsed the file. Reporting success while holding a
    # verdict of "unreadable" is how an unterminated file reached the results
    # tree as a run's measurement.
    case "$n" in
      unreadable:*) echo "the samples file at $OUT does not parse -- not a usable measurement" >&2
                    exit 1 ;;
    esac
    # Zero controllers has two causes and they are not the same answer: a namespace
    # with nothing serving is real and empty, while a capture whose polls were all
    # rejected measured nothing at all.
    #
    # The discriminator is the SNAPSHOT count, because a failed poll emits nothing
    # -- that is the exit status, already propagated into the data one step above.
    # Asking instead whether anything wrote to stderr re-derived it from the wrong
    # thing and inverted the defect: a cluster that prints a deprecation warning,
    # or an RBAC scope whose pod polls are Forbidden while its deployment polls all
    # answer, got exit 1 with that warning quoted as the reason its polls were
    # "failing" -- and the recipe withheld both artefacts of a correct capture.
    #
    # So stderr is the REASON, never the trigger.
    case "$n" in
      "0 snapshot(s), "*)
        echo "the capture at $OUT recorded no snapshots, so no poll ever answered:" >&2
        [ -s "$OUT.stderr" ] && sed -n '$p' "$OUT.stderr" >&2
        echo "  that is a run with no replica curve, not a namespace with no replicas" >&2
        exit 1 ;;
      *" 0 controller sample(s)")
        echo "  note: no serving controllers were seen in ${NS_ARG:-that namespace} for the whole capture." >&2
        echo "  Every poll answered, so this is an empty namespace rather than a failure." >&2
        ;;
    esac
    ;;
  *)
    echo "unknown command: $CMD" >&2; exit 2 ;;
esac
