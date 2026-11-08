#!/usr/bin/env python3
"""Seed an FMA warm pool that wakes RELIABLY, then measure it over N cycles.

WHY THE OBVIOUS SETUP DOES NOT WAKE
-----------------------------------
FMA reuses a sleeping vLLM only on an exact instance-ID match, and that ID hashes
the GPU UUIDs. So a requester wakes a sleeper only if the scheduler hands it the
very GPU that sleeper holds. The hit rate is therefore

    covered free GPUs on the node the requester lands on
    ---------------------------------------------------
    total free GPUs on that node

NOT "is there a sleeper nearby". Measured on the shared GPU cluster: launchers pinned to 5 nodes,
requesters eligible on all 14, one sleeper per node out of 7-8 GPUs -- 0 woke /
3 rebuilt, twice, even with placement affinity putting 3/3 replicas on launcher
nodes.

Both terms have to be forced for the ratio to be 1:

  * the DENOMINATOR, by pinning requesters to ONE node (hard nodeSelector on
    kubernetes.io/hostname) and restricting the LauncherPopulationPolicy to it;
  * the NUMERATOR, by seeding a sleeper onto every free GPU of that node -- scale
    requesters up to the free-GPU count so each reserves one, wait for all to
    serve, then scale to zero so each launcher's instance sleeps keyed to a GPU
    the scheduler actually hands out.

launcherCount must be raised to the same number FIRST. The populator reaps
launchers above it, per node, about 20s after a scale-down -- seeding depth
before raising it just feeds the reaper.

WHAT THE FORK CHANGES HERE
--------------------------
A saturated node wakes on stock FMA too, because every requester hits Priority 1
and no reclaim is reached. The fork (ghcr.io/ev-shindin/dual-pods-controller,
"do not reclaim a sleeping instance that only conflicts on the inference port")
matters when a cycle MISSES -- a tenant took a GPU, or the count is off by one.
On stock, that miss destroys a sleeper and the pool decays cycle over cycle; with
the fork the miss is harmless and the remaining sleepers stay warm. So run this
for several cycles: cycle 1 tests the seeding, cycles 2..N test that warmth
survives use.

A sleeper does NOT reserve its GPU -- the launcher requests none. On a shared
cluster another tenant can take a covered GPU between cycles, and that shows up
here as a rebuild. It is a real failure mode, not noise, and it is reported.

Usage:
  wake_cycle.py --namespace NS [--node NODE] [--sleepers N] [--cycles C] [--dry-run]

Everything mutated is backed up first and restored in a finally block: the
requester Deployment (replicas + nodeSelector), its KEDA ScaledObject (deleted
for the duration, or it reverts every scale within seconds), and the
LauncherPopulationPolicy (node set + launcherCount).
"""

import argparse
import json
import subprocess
import sys
import time

WAKE_THRESHOLD_S = 15  # measured: wakes 2-3s, rebuilds 43-95s
SLEEP_LABEL = "dual-pods.llm-d.ai/sleeping"
LAUNCHER_SEL = "app.kubernetes.io/component=launcher"


class Kube:
    def __init__(self, namespace, dry_run=False):
        self.ns = namespace
        self.dry_run = dry_run

    def __call__(self, *args, check=True, mutating=False, timeout=180):
        if mutating and self.dry_run:
            print("    DRY-RUN: kubectl %s" % " ".join(args))
            return ""
        p = subprocess.run(["kubectl", "-n", self.ns, *args],
                           capture_output=True, text=True, timeout=timeout)
        if check and p.returncode != 0:
            raise RuntimeError("kubectl %s failed:\n%s" % (" ".join(args), p.stderr))
        return p.stdout

    def json(self, *args, **kw):
        out = self(*args, "-o", "json", **kw)
        return json.loads(out) if out.strip() else {}


def discover(kc):
    """Find the requester Deployment, its ScaledObject and the LPP.

    The requester is identified by its POD TEMPLATE label: llm-d.ai/role lives on
    the template, not on the Deployment, so `kubectl get deploy -l ...` matches
    nothing.
    """
    deploy = None
    for d in kc.json("get", "deploy").get("items", []):
        tmpl = (d["spec"].get("template") or {}).get("metadata", {}) or {}
        if (tmpl.get("labels") or {}).get("llm-d.ai/role") == "requester":
            deploy = d["metadata"]["name"]
            break
    if not deploy:
        sys.exit("no requester Deployment found (pod template llm-d.ai/role=requester)")

    so = None
    for s in kc.json("get", "scaledobject", check=False).get("items", []):
        tgt = ((s["spec"].get("scaleTargetRef") or {}).get("name"))
        if tgt == deploy:
            so = s["metadata"]["name"]
            break

    lpp = None
    items = kc.json("get", "launcherpopulationpolicy", check=False).get("items", [])
    if items:
        lpp = items[0]["metadata"]["name"]
    return deploy, so, lpp


def free_gpus_by_node(kc):
    """allocatable - requested, per GPU node. allocatable does NOT drop as pods
    consume the resource, so headroom has to be computed."""
    nodes = json.loads(subprocess.run(
        ["kubectl", "get", "nodes", "-l", "nvidia.com/gpu.present=true", "-o", "json"],
        capture_output=True, text=True, timeout=180).stdout or '{"items":[]}')["items"]
    pods = json.loads(subprocess.run(
        ["kubectl", "get", "pods", "-A", "-o", "json"],
        capture_output=True, text=True, timeout=240).stdout or '{"items":[]}')["items"]
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
    out = {}
    for node in nodes:
        name = node["metadata"]["name"]
        try:
            alloc = int(node["status"]["allocatable"].get("nvidia.com/gpu", 0))
        except (TypeError, ValueError, KeyError):
            alloc = 0
        out[name] = alloc - used.get(name, 0)
    return out


def instances(kc, launcher):
    """The launcher's own view of its vLLM instances -- the only place instance
    IDs and their GPUs are visible. Pod labels describe the POD, not instances."""
    out = kc("exec", launcher, "-c", "inference-server", "--",
             "curl", "-s", "--max-time", "8",
             "http://localhost:8001/v2/vllm/instances", check=False, timeout=90)
    try:
        return json.loads(out).get("instances") or []
    except (ValueError, TypeError):
        return []


def pool_state(kc):
    """(sleeping instance count, {launcher: [gpu,...]}) across all launchers."""
    per, total = {}, 0
    for p in kc.json("get", "pods", "-l", LAUNCHER_SEL).get("items", []):
        name = p["metadata"]["name"]
        gpus = []
        for inst in instances(kc, name):
            gpus.append((inst.get("gpu_uuids") or ["-"])[0])
        per[name] = gpus
        total += len(gpus)
    return total, per


def requester_pods(kc):
    return {p["metadata"]["name"]: p
            for p in kc.json("get", "pods", "-l", "llm-d.ai/role=requester").get("items", [])}


def ready_seconds(pod):
    fmt = "%Y-%m-%dT%H:%M:%SZ"
    created = time.mktime(time.strptime(pod["metadata"]["creationTimestamp"], fmt))
    for c in pod["status"].get("conditions", []):
        if c["type"] == "Ready" and c["status"] == "True":
            return time.mktime(time.strptime(c["lastTransitionTime"], fmt)) - created
    return None


def wait_replicas(kc, deploy, want, timeout=900, require_ready=True):
    deadline = time.time() + timeout
    while time.time() < deadline:
        pods = [p for p in requester_pods(kc).values()
                if (p.get("metadata", {}).get("deletionTimestamp") is None)]
        if want == 0:
            if not pods:
                return True
        elif len(pods) >= want:
            if not require_ready or all(ready_seconds(p) is not None for p in pods):
                return True
        time.sleep(5)
    return False


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--namespace", required=True)
    ap.add_argument("--node", help="node to pin to; default = the LPP node with most free GPUs")
    ap.add_argument("--sleepers", type=int, help="default = free GPUs on the chosen node")
    ap.add_argument("--cycles", type=int, default=3)
    ap.add_argument("--dry-run", action="store_true")
    args = ap.parse_args()

    kc = Kube(args.namespace, args.dry_run)
    deploy, so, lpp = discover(kc)
    print("requester Deployment : %s" % deploy)
    print("ScaledObject         : %s" % (so or "(none)"))
    print("LauncherPopulationPolicy: %s" % (lpp or "(none)"))
    if not lpp:
        sys.exit("no LauncherPopulationPolicy -- cannot size the pool")

    lpp_obj = kc.json("get", "launcherpopulationpolicy", lpp)
    sel = ((lpp_obj["spec"].get("enhancedNodeSelector") or {}).get("labelSelector") or {})
    pool_nodes = []
    for e in (sel.get("matchExpressions") or []):
        if e.get("key") == "kubernetes.io/hostname" and e.get("operator") == "In":
            pool_nodes = e.get("values") or []
    free = free_gpus_by_node(kc)
    candidates = {n: free.get(n, 0) for n in (pool_nodes or free)}
    print("\nfree GPUs on candidate nodes: %s"
          % ", ".join("%s=%d" % (n, g) for n, g in
                      sorted(candidates.items(), key=lambda kv: -kv[1])))

    node = args.node or max(candidates, key=lambda n: candidates[n])
    n_free = free.get(node, 0)
    sleepers = args.sleepers or n_free
    print("\nchosen node   : %s (%d free GPUs)" % (node, n_free))
    print("sleepers      : %d" % sleepers)
    print("cycles        : %d" % args.cycles)
    if sleepers > n_free:
        sys.exit("cannot seed %d sleepers: only %d GPUs are free on %s"
                 % (sleepers, n_free, node))
    if sleepers < n_free:
        print("  NOTE: %d free GPUs are NOT covered, so a requester can still be handed\n"
              "        one of them and rebuild. Expected hit rate %d/%d."
              % (n_free - sleepers, sleepers, n_free))

    backup = {"deploy": kc.json("get", "deploy", deploy),
              "lpp": kc.json("get", "launcherpopulationpolicy", lpp)}
    if so:
        backup["so"] = kc.json("get", "scaledobject", so)
    orig_replicas = backup["deploy"]["spec"]["replicas"]
    lcname = backup["lpp"]["spec"]["countForLauncher"][0]["launcherConfigName"]
    print("\nbacked up: replicas=%d, launcherCount=%s, node set=%s"
          % (orig_replicas,
             backup["lpp"]["spec"]["countForLauncher"][0].get("launcherCount"),
             pool_nodes or "(label only)"))

    results = []
    try:
        if so:
            print("\n=== removing ScaledObject (KEDA reverts scales within seconds) ===")
            kc("delete", "scaledobject", so, "--wait=true", mutating=True)

        print("=== restricting the launcher pool to %s, launcherCount=%d ===" % (node, sleepers))
        lpp_patch = {"spec": {
            "enhancedNodeSelector": {"labelSelector": {
                "matchLabels": {"nvidia.com/gpu.present": "true"},
                "matchExpressions": [{"key": "kubernetes.io/hostname",
                                      "operator": "In", "values": [node]}]}},
            "countForLauncher": [{"launcherConfigName": lcname,
                                  "launcherCount": sleepers}]}}
        kc("patch", "launcherpopulationpolicy", lpp, "--type=merge",
           "-p", json.dumps(lpp_patch), mutating=True)

        print("=== pinning the requester to %s ===" % node)
        kc("patch", "deploy", deploy, "--type=merge", "-p", json.dumps(
            {"spec": {"template": {"spec": {
                "nodeSelector": {"kubernetes.io/hostname": node}}}}}), mutating=True)
        if not args.dry_run:
            live = kc.json("get", "deploy", deploy)
            got = (live["spec"]["template"]["spec"].get("nodeSelector") or {})
            if got.get("kubernetes.io/hostname") != node:
                sys.exit("pin did NOT apply; live nodeSelector=%r" % got)
            print("  confirmed on the live object: %s" % got)

        # ---- seed -------------------------------------------------------
        print("\n=== SEED: scaling to %d so each reserves a distinct GPU ===" % sleepers)
        kc("scale", "deploy", deploy, "--replicas=%d" % sleepers, mutating=True)
        if not args.dry_run:
            if not wait_replicas(kc, deploy, sleepers, timeout=1200):
                print("  WARNING: not all %d replicas became Ready" % sleepers)
            print("=== SEED: scaling to 0 so their launchers sleep ===")
            kc("scale", "deploy", deploy, "--replicas=0", mutating=True)
            wait_replicas(kc, deploy, 0, timeout=600)
            time.sleep(45)  # let the controller put instances to sleep
            total, per = pool_state(kc)
            print("  seeded instances: %d across %d launchers" % (total, len(per)))
            for l, gpus in sorted(per.items()):
                print("    %-46s %s" % (l[-44:], [g[:20] for g in gpus]))

        # ---- cycles -----------------------------------------------------
        for cycle in range(1, args.cycles + 1):
            if args.dry_run:
                break
            print("\n=== CYCLE %d/%d: scaling 0 -> %d ===" % (cycle, args.cycles, sleepers))
            before = set(requester_pods(kc))
            kc("scale", "deploy", deploy, "--replicas=%d" % sleepers, mutating=True)
            wait_replicas(kc, deploy, sleepers, timeout=900)
            new = {n: p for n, p in requester_pods(kc).items() if n not in before}
            woke = rebuilt = pending = 0
            for name, p in sorted(new.items()):
                secs = ready_seconds(p)
                if secs is None:
                    verdict, pending = "NOT READY", pending + 1
                elif secs <= WAKE_THRESHOLD_S:
                    verdict, woke = "WOKE", woke + 1
                else:
                    verdict, rebuilt = "REBUILT", rebuilt + 1
                print("  %-44s %5s  %s" % (name[-42:],
                                           "-" if secs is None else "%.0fs" % secs, verdict))
            print("  cycle %d: %d woke / %d rebuilt%s"
                  % (cycle, woke, rebuilt, " / %d pending" % pending if pending else ""))
            results.append((woke, rebuilt, pending))
            kc("scale", "deploy", deploy, "--replicas=0", mutating=True)
            wait_replicas(kc, deploy, 0, timeout=600)
            time.sleep(45)

        if results:
            print("\n=== SUMMARY ===")
            tw = sum(r[0] for r in results)
            tr = sum(r[1] for r in results)
            tp = sum(r[2] for r in results)
            for i, (w, r, pd) in enumerate(results, 1):
                print("  cycle %d: %d woke / %d rebuilt%s"
                      % (i, w, r, " / %d pending" % pd if pd else ""))
            print("  TOTAL  : %d woke / %d rebuilt%s  (%.0f%% wake)"
                  % (tw, tr, " / %d pending" % tp if tp else "",
                     100.0 * tw / max(tw + tr + tp, 1)))

    finally:
        print("\n=== restoring ===")
        try:
            kc("scale", "deploy", deploy, "--replicas=%d" % orig_replicas,
               check=False, mutating=True)
            kc("patch", "deploy", deploy, "--type=json", "-p",
               '[{"op":"remove","path":"/spec/template/spec/nodeSelector"}]',
               check=False, mutating=True)
            lpp_restore = {"spec": {
                "enhancedNodeSelector": backup["lpp"]["spec"].get("enhancedNodeSelector"),
                "countForLauncher": backup["lpp"]["spec"].get("countForLauncher")}}
            kc("patch", "launcherpopulationpolicy", lpp, "--type=merge",
               "-p", json.dumps(lpp_restore), check=False, mutating=True)
            if so:
                p = subprocess.run(["kubectl", "-n", args.namespace, "apply", "-f", "-"],
                                   input=json.dumps(backup["so"]), text=True,
                                   capture_output=True, timeout=120)
                if p.returncode != 0:
                    print("  ScaledObject restore FAILED: %s" % p.stderr.strip())
            live = kc.json("get", "deploy", deploy)
            print("  replicas=%s nodeSelector=%s launcherCount=%s"
                  % (live["spec"]["replicas"],
                     (live["spec"]["template"]["spec"].get("nodeSelector") or "removed"),
                     kc.json("get", "launcherpopulationpolicy", lpp)["spec"]
                       ["countForLauncher"][0].get("launcherCount")))
        except Exception as exc:                      # noqa: BLE001
            print("  RESTORE FAILED: %s" % exc)
            print("  originals are in the backup dict; restore by hand")


if __name__ == "__main__":
    main()
