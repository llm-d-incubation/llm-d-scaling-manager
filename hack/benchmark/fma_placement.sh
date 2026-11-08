#!/usr/bin/env bash
# Check that the FMA placement a scenario asks for can actually take effect.
#
# WHY THIS EXISTS
# ---------------
# A wake that finds a sleeping vLLM is ready in ~3s; one that rebuilds takes
# ~50-90s. Which one you get is decided by PLACEMENT: dual-pods binds a
# requester to a launcher on the SAME NODE, so a requester that lands where no
# sleeper lives forces a cold build. Every FMA-vs-non-FMA number this repo took
# before 2026-08-18 was measured on the cold path by construction.
#
# The failure that made that possible is not a wrong value -- it is
# configuration that reads as enabled and does nothing:
#
#   * `fma.launcherNodeSelection.enabled: true` sat under `fma.enabled: false`.
#     step_06_fma_deploy.py skips itself entirely when FMA is not among the
#     deployed methods, so the code that selects and labels a node never ran,
#     the requester nodeSelector was never rendered, and the hotstart warmup
#     step early-returned too. The whole `fma:` block was dead config.
#   * A node selector that matches ZERO nodes is indistinguishable from no
#     selector at all: the pods schedule anywhere and everything reports
#     success.
#
# Both shapes report success. That is what this script is for: it fails loudly
# on configuration that cannot do what it says, instead of letting a run produce
# cold-path numbers under a warm-path label.
#
# Usage:
#   fma_placement.sh check  <scenario.yaml>      static: can this config work?
#   fma_placement.sh verify <namespace> [scen]   live: is the cluster that way?
#
# `check` needs no cluster and runs before standup. `verify` reads the cluster
# and belongs after standup / before a run.
#
# Env: WARM_REPLICAS resolves an unsubstituted __WARM_REPLICAS__ token in the
#      source scenario (benchmark-scenarios substitutes it on the way into the
#      clone; the versioned copy still carries the token).
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


YQ="${YQ:-yq}"
command -v "$YQ" >/dev/null 2>&1 || {
    echo "fma_placement: $YQ not found (benchmark-standup already requires it)" >&2
    exit 2
}

SCENARIO=""

# Read one key out of the first scenario entry that has an `fma` block.
#
# `| tostring` rather than yq's `//` alternative operator on purpose: `false //
# "null"` returns the ALTERNATIVE, so every disabled boolean would read as
# missing and every check below would pass on exactly the config it exists to
# catch.
fma_get() {
    "$YQ" -r "[.scenario[] | select(has(\"fma\"))] | .[0].fma${1} | tostring" \
        "$SCENARIO" 2>/dev/null
}

# The node label key node selection uses. Mirrors step_06's default.
node_label() {
    local v
    v=$(fma_get ".launcherNodeSelection.nodeLabel")
    if [ -n "$v" ] && [ "$v" != "null" ]; then
        echo "$v"
    else
        echo "fma-hotstart"
    fi
}

# requester.replicas as an integer. The versioned scenario carries the
# __WARM_REPLICAS__ token, which only benchmark-scenarios substitutes; resolve
# it from the environment so `check` sees the value the run will actually use.
requester_replicas() {
    local v
    v=$(fma_get ".requester.replicas")
    case "$v" in
        *__WARM_REPLICAS__*) v="${WARM_REPLICAS:-}" ;;
    esac
    case "${v:-}" in
        ''|null|*[!0-9]*) echo "" ;;
        *)                echo "$v" ;;
    esac
}

# Pods by node, split into launchers and requesters. Reads `kubectl get pods -o
# json` on stdin and prints one line per node plus a SUMMARY line the caller
# parses. python3 rather than jq: warm_pool.sh already depends on it and jq is
# not a documented dependency of this repo.
PY_PLACEMENT='
import json, sys
launchers, requesters = {}, {}
for p in json.load(sys.stdin).get("items", []):
    labels = (p.get("metadata", {}) or {}).get("labels", {}) or {}
    node = (p.get("spec", {}) or {}).get("nodeName")
    if not node:
        continue
    if labels.get("app.kubernetes.io/component") == "launcher":
        launchers[node] = launchers.get(node, 0) + 1
    elif labels.get("llm-d.ai/role") == "requester":
        requesters[node] = requesters.get(node, 0) + 1
for node in sorted(set(launchers) | set(requesters)):
    print("    %-28s launchers=%-3d requesters=%d" % (
        node, launchers.get(node, 0), requesters.get(node, 0)))
stranded = sum(n for node, n in requesters.items() if node not in launchers)
print("SUMMARY %d %d %d" % (len(launchers), sum(requesters.values()), stranded))
'

# Count requester Deployments whose pod template carries a podAffinity.
#
# Requesters are identified by the POD TEMPLATE label, not the Deployment
# object's own labels -- llm-d puts `llm-d.ai/role` on the template only, so
# `kubectl get deploy -l llm-d.ai/role=requester` matches NOTHING. That is the
# same mistake that made so_discover find zero model servers on a real cluster:
# `-l` reads the object's labels and nothing else. Verified against the shared GPU cluster,
# where the requester Deployment carries only `app: fma-requester-<model>`.
PY_HAS_AFFINITY='
import json, sys
total = with_aff = 0
for d in json.load(sys.stdin).get("items", []):
    tmpl = (d.get("spec", {}) or {}).get("template", {}) or {}
    labels = (tmpl.get("metadata", {}) or {}).get("labels", {}) or {}
    if labels.get("llm-d.ai/role") != "requester":
        continue
    total += 1
    if ((tmpl.get("spec", {}) or {}).get("affinity") or {}).get("podAffinity"):
        with_aff += 1
print("%d %d" % (with_aff, total))
'

cmd_check() {
    SCENARIO="${1:?usage: fma_placement.sh check <scenario.yaml>}"
    [ -f "$SCENARIO" ] || {
        echo "fma_placement: no such scenario: $SCENARIO" >&2; exit 2; }

    local has_fma fma_en sel_en aff_en label replicas rc=0
    has_fma=$("$YQ" -r '[.scenario[] | select(has("fma"))] | length' "$SCENARIO" 2>/dev/null)
    if [ "${has_fma:-0}" = "0" ]; then
        echo "fma_placement: $(basename "$SCENARIO") declares no fma block; nothing to check."
        return 0
    fi

    fma_en=$(fma_get ".enabled")
    sel_en=$(fma_get ".launcherNodeSelection.enabled")
    aff_en=$(fma_get ".warmAffinity.enabled")
    label=$(node_label)
    replicas=$(requester_replicas)

    echo "fma_placement: $(basename "$SCENARIO")"
    echo "  fma.enabled=$fma_en  launcherNodeSelection=$sel_en  warmAffinity=$aff_en  requester.replicas=${replicas:-<unresolved>}"

    # ---- inert block -------------------------------------------------------
    # Everything under `fma:` is consumed by code that first checks whether FMA
    # is a deployed method (standup step_06) or reads fma.enabled directly (run
    # step_02a_fma_warmup_hotstart). With fma.enabled false, all of it is
    # decoration -- including the node selection that decides warm vs cold.
    if [ "$fma_en" != "true" ]; then
        if [ "$sel_en" = "true" ] || [ "$aff_en" = "true" ] ||
           { [ -n "$replicas" ] && [ "$replicas" -gt 0 ] 2>/dev/null; }; then
            echo ""
            echo "ERROR: this scenario configures FMA placement/warmup under fma.enabled=false."
            echo "       Nothing consumes it. step_06_fma_deploy skips itself when FMA is not a"
            echo "       deployed method, so no node is selected or labeled and the requester's"
            echo "       nodeSelector/affinity is never rendered; step_02a_fma_warmup_hotstart"
            echo "       returns early on the same flag, so WARM_REPLICAS warms nothing."
            echo "       The run then measures the COLD path while reporting a warm config."
            echo ""
            echo "       Choose one:"
            echo "         - set fma.enabled: true so standup owns the FMA install, or"
            echo "         - drop launcherNodeSelection/warmAffinity/requester.replicas from this"
            echo "           scenario and place the already-deployed FMA yourself, verifying with"
            echo "           'make benchmark-fma-verify BENCHMARK_NAMESPACE=<ns>'."
            echo ""
            rc=1
        else
            echo "  fma.enabled=false and nothing under fma: is set; consistent."
        fi
    fi

    # ---- contradictory placement modes ------------------------------------
    # The nodeSelector is a hard predicate and the affinity is a soft score.
    # Both rendered together is not "belt and braces": the selector decides,
    # and the affinity silently does nothing on top of it.
    if [ "$sel_en" = "true" ] && [ "$aff_en" = "true" ]; then
        echo ""
        echo "ERROR: launcherNodeSelection and warmAffinity are both enabled."
        echo "       launcherNodeSelection renders a hard nodeSelector pinning the requester to"
        echo "       one labeled node; warmAffinity renders a preferred podAffinity. The hard"
        echo "       predicate wins and the affinity contributes nothing. Pick one mode."
        echo ""
        rc=1
    fi

    # ---- pinned mode: state its ceiling out loud --------------------------
    # Not an error: pinning is the right mode on a dedicated benchmark cluster.
    # It is a ceiling worth printing, because it is invisible in the config.
    if [ "$sel_en" = "true" ]; then
        echo ""
        echo "NOTE: launcherNodeSelection pins the warm pool to a SINGLE node."
        echo "      step_06 scores GPU nodes, takes the best one, strips '$label' from every"
        echo "      other node, and sizes requester replicas, LauncherPopulationPolicy"
        echo "      launcherCount and the KEDA ScaledObject ceiling to that node's FREE GPU"
        echo "      count at that instant. Consequences: the pool cannot grow past one node"
        echo "      (labeling a second is undone by the next standup), the ceiling is a"
        echo "      point-in-time reading, and standup FAILS outright when no single node has"
        echo "      free GPUs -- the common case on a shared cluster."
        echo "      For a pool that spans nodes, use warmAffinity instead."
    fi

    if [ "$rc" -eq 0 ]; then
        echo "  OK"
    fi
    return "$rc"
}

cmd_verify() {
    local ns="${1:?usage: fma_placement.sh verify <namespace> [scenario.yaml]}"
    SCENARIO="${2:-}"

    local sel_en="null" aff_en="null" label="fma-hotstart"
    if [ -n "$SCENARIO" ] && [ -f "$SCENARIO" ]; then
        sel_en=$(fma_get ".launcherNodeSelection.enabled")
        aff_en=$(fma_get ".warmAffinity.enabled")
        label=$(node_label)
    fi

    local launchers rc=0
    launchers=$(kubectl get pods -n "$ns" -l app.kubernetes.io/component=launcher \
        --no-headers 2>/dev/null | grep -c . || true)
    if [ "${launchers:-0}" -eq 0 ]; then
        echo "fma_placement: no launcher pods in $ns; FMA is not deployed here. Nothing to verify."
        return 0
    fi

    echo "fma_placement: placement in $ns"

    # Nodes carrying the pin label. Zero of them with node selection enabled is
    # the silent case: the selector matches nothing, so it constrains nothing.
    local labeled
    labeled=$(kubectl get nodes -l "$label=true" --no-headers 2>/dev/null | grep -c . || true)
    echo "  nodes labeled $label=true: ${labeled:-0}"

    # Where the launchers and requesters actually are. Colocation is the whole
    # question: a requester on a node with no launcher rebuilds from scratch.
    local placement summary lnodes rtotal stranded
    placement=$(kubectl get pods -n "$ns" -o json 2>/dev/null |
        python3 -c "$PY_PLACEMENT" 2>/dev/null)

    echo "$placement" | grep -v '^SUMMARY' || true
    summary=$(echo "$placement" | grep '^SUMMARY' || echo "SUMMARY 0 0 0")
    lnodes=$(echo "$summary" | awk '{print $2}')
    rtotal=$(echo "$summary" | awk '{print $3}')
    stranded=$(echo "$summary" | awk '{print $4}')
    echo "  launcher nodes: $lnodes   requesters: $rtotal   on a node with NO launcher: $stranded"

    if [ "$sel_en" = "true" ] && [ "${labeled:-0}" -eq 0 ]; then
        echo ""
        echo "ERROR: launcherNodeSelection is enabled but NO node carries $label=true."
        echo "       A selector matching zero nodes constrains nothing and reports success:"
        echo "       launchers and requesters schedule anywhere, and every scale-up rebuilds"
        echo "       cold (~50-90s) instead of waking a sleeper (~3s)."
        echo "       Either let standup select a node (fma.enabled: true) or label one:"
        echo "         kubectl label node <node> $label=true"
        echo ""
        rc=1
    fi

    if [ "$aff_en" = "true" ]; then
        local affcounts with_aff total_req
        affcounts=$(kubectl get deploy -n "$ns" -o json 2>/dev/null |
            python3 -c "$PY_HAS_AFFINITY" 2>/dev/null || echo "0 0")
        with_aff=$(echo "$affcounts" | awk '{print $1}')
        total_req=$(echo "$affcounts" | awk '{print $2}')
        echo "  requester deployments carrying a podAffinity: ${with_aff:-0}/${total_req:-0}"
        if [ "${with_aff:-0}" -eq 0 ]; then
            echo ""
            echo "ERROR: warmAffinity is enabled but no requester Deployment carries a podAffinity."
            echo "       The template patch did not reach this stack -- run 'make benchmark-patch'"
            echo "       and stand up again. Without it requesters schedule independently of the"
            echo "       warm pool and wakes fall back to cold rebuilds."
            echo ""
            rc=1
        fi
    fi

    if [ "${stranded:-0}" -gt 0 ]; then
        echo ""
        echo "WARNING: $stranded requester(s) sit on a node with no launcher. Binding is"
        echo "         node-local, so each of those is a cold rebuild on the next wake."
    fi

    if [ "$rc" -eq 0 ]; then
        echo "  OK"
    fi
    return "$rc"
}

case "${1:-}" in
    check)  shift; cmd_check "$@" ;;
    verify) shift; cmd_verify "$@" ;;
    *) sed -n '/^# Usage:/,/^# Env:/p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
