#!/usr/bin/env python3
"""What the utilization-share optimizer did in each arm of the two-model run.

The TTFT and accelerator-second tables come from two_model_report.py, which
compares shadow and share against the today baseline exactly as it compares the
pool arms against nopool. This adds what only the controller knows: which mode
it ran in, the transfers it made, the model-seconds a model was held back and
why, and the seconds a replica move was planned.

It refuses, rather than reports, three runs that would mislead:

- arms whose limiters differ: the comparison is of two optimizers under ONE
  quota, and a different quota is a different experiment;
- an arm whose controller did not hold the arm's mode for the whole window: a
  policy that was never read measures the previous arm under a new name;
- a today or shadow arm in which a transfer happened: neither may move a GPU,
  so one that did was not the arm it is labelled.

`--rewrite-policy ARM` is the other half: it rewrites a policy's `optimizer:`
block for an arm (stdin to stdout), and is what the driver runs before each arm.

`--placement GPUS_JSONL` prints where an arm's pods landed, from the sampler's
own record, as JSON (the driver keeps it as placement.json), and
`--placement-summary PLACEMENT_JSON` one line of it. The report prints the
placement of every arm: across several nodes a replica of N GPUs needs N free
on one node, so where the fleet landed decides what the optimizer could fund.
"""

import argparse
import json
import os
import sys

ARMS = ("today", "shadow", "share")
MODE = {"today": "off", "shadow": "shadow", "share": "active"}
OUTCOMES = ("done", "aborted", "fill-timeout", "wrong-pod")


def strip_optimizer(text):
    """The policy with its top-level optimizer: block removed, nothing else."""
    out, skip = [], False
    for line in text.splitlines():
        if line[:1] not in ("", " ", "\t", "#"):
            skip = line.split(":", 1)[0].strip() == "optimizer"
        if not skip:
            out.append(line)
    while out and not out[-1].strip():
        out.pop()
    return out


def rewrite_policy(text, arm):
    """The policy as arm ARM runs it: today has no optimizer block, shadow and
    share the utilization-share optimizer with shadow on and off."""
    if arm not in ARMS:
        raise ValueError("unknown arm %r (want one of %s)" % (arm, ", ".join(ARMS)))
    out = strip_optimizer(text)
    if arm != "today":
        out += ["optimizer:", "  type: utilizationShare", "  utilizationShare:",
                "    shadow: " + ("true" if arm == "shadow" else "false")]
    return "\n".join(out) + "\n"


def series(snap, prefix, label):
    """{label value: number} for the query in snap that starts with prefix."""
    for q, res in (snap or {}).items():
        if q.startswith(prefix):
            if isinstance(res, dict):
                return None
            return {r["metric"].get(label, ""): float(r["value"][1]) for r in res}
    return None


def scalar(snap, prefix):
    for q, res in (snap or {}).items():
        if q.startswith(prefix):
            if isinstance(res, dict):
                return None
            return float(res[0]["value"][1]) if res else 0.0
    return None


def placement(gpus_jsonl):
    """Where an arm's pods landed, from its sampler's file.

    {"nodes": {node: {"peak_gpus": most GPUs held there at once,
                      "models": {model: {"pods": distinct pods seen,
                                         "peak_replicas": most at once}}}},
     "pods": {pod: {"node", "model", "gpus"}}, "samples": n}

    Only Running pods holding an accelerator are sampled, which is what held
    one. A sample the sampler marked failed or stalled carries no pods.
    """
    nodes, pods, samples = {}, {}, 0
    with open(gpus_jsonl) as fh:
        for line in fh:
            line = line.strip()
            if not line:
                continue
            try:
                s = json.loads(line)
            except ValueError:
                continue
            if "pods" not in s:
                continue
            samples += 1
            held, count = {}, {}
            for p in s["pods"]:
                node = p.get("node") or "?"
                model = p.get("model") or p.get("pool") or "?"
                gpus = p.get("gpus") or 0
                pods.setdefault(p.get("name", "?"), {"node": node, "model": model, "gpus": gpus})
                held[node] = held.get(node, 0) + gpus
                count[(node, model)] = count.get((node, model), 0) + 1
            for node, g in held.items():
                n = nodes.setdefault(node, {"peak_gpus": 0, "models": {}})
                n["peak_gpus"] = max(n["peak_gpus"], g)
            for (node, model), c in count.items():
                m = nodes[node]["models"].setdefault(model, {"pods": 0, "peak_replicas": 0})
                m["peak_replicas"] = max(m["peak_replicas"], c)
    for info in pods.values():
        m = nodes.setdefault(info["node"], {"peak_gpus": 0, "models": {}})["models"].setdefault(
            info["model"], {"pods": 0, "peak_replicas": 0})
        m["pods"] += 1
    return {"nodes": nodes, "pods": pods, "samples": samples}


def placement_summary(pl):
    """One line: per node, the GPUs held there at most and each model's replicas."""
    if not pl or not pl.get("nodes"):
        return "no pod held an accelerator in any sample"
    parts = []
    for node in sorted(pl["nodes"]):
        n = pl["nodes"][node]
        models = ", ".join("%s x%d" % (m, v["peak_replicas"]) for m, v in sorted(n["models"].items()))
        parts.append("%s: up to %d GPU(s) (%s)" % (node, n["peak_gpus"], models))
    return "; ".join(parts)


def load_placement(d):
    """The arm's placement.json, else derived from its gpus.jsonl, else None."""
    try:
        with open(os.path.join(d, "placement.json")) as fh:
            return json.load(fh)
    except (OSError, ValueError):
        pass
    try:
        return placement(os.path.join(d, "gpus.jsonl"))
    except OSError:
        return None


def placement_table(rows):
    """rows: [(label, placement or None)]. A markdown table, a line per node."""
    out = ["| arm | node | most GPUs held at once | replicas at once, by model | pods seen |",
           "| --- | --- | --- | --- | --- |"]
    for label, pl in rows:
        if not pl or not pl.get("nodes"):
            out.append("| %s | - | - | no placement recorded | - |" % label)
            continue
        for node in sorted(pl["nodes"]):
            n = pl["nodes"][node]
            out.append("| %s | %s | %d | %s | %d |" % (
                label, node, n["peak_gpus"],
                ", ".join("%s: %d" % (m, v["peak_replicas"]) for m, v in sorted(n["models"].items())),
                sum(v["pods"] for v in n["models"].values())))
    return out


def load_arm(root, arm):
    d = os.path.join(root, arm)
    if not os.path.isfile(os.path.join(d, "meta.json")):
        return None
    snap = policy = None
    try:
        with open(os.path.join(d, "share.json")) as fh:
            snap = json.load(fh)
    except (OSError, ValueError):
        pass
    try:
        with open(os.path.join(d, "policy.yaml")) as fh:
            policy = fh.read()
    except OSError:
        pass
    return {
        "name": arm,
        "policy": policy,
        "snap": snap,
        "transfers": series(snap, "sum by (outcome)", "outcome"),
        "blocked": series(snap, "sum by (reason)", "reason"),
        "planned": scalar(snap, "count_over_time((max("),
        "modes": series(snap, "min by (mode)", "mode"),
        "placement": load_placement(d),
    }


def problems(arms):
    """Why these arms cannot be compared, or [] when they can."""
    out = []
    base = arms[0]
    for a in arms:
        if a["policy"] is None:
            out.append("the %s arm did not keep the policy it ran under (policy.yaml), so "
                       "nothing establishes that its quota was the same" % a["name"])
        elif base["policy"] is not None and strip_optimizer(a["policy"]) != strip_optimizer(base["policy"]):
            out.append("the %s and %s arms ran under different policies apart from the "
                       "optimizer block: a different quota is a different experiment"
                       % (base["name"], a["name"]))
        if a["snap"] is None:
            out.append("the %s arm has no controller metrics (share.json), so neither its "
                       "mode nor its transfers are known" % a["name"])
            continue
        modes = a["modes"]
        want = MODE[a["name"]]
        if modes is None:
            out.append("the %s arm's mode query failed" % a["name"])
        elif modes.get(want) != 1.0:
            out.append("the controller did not hold mode '%s' for the whole %s arm (min over "
                       "the window: %s): it measured something else for part of it"
                       % (want, a["name"], modes.get(want, "absent")))
        moved = sum((a["transfers"] or {}).values())
        if a["name"] != "share" and moved >= 0.5:
            out.append("the %s arm recorded %.0f transfer(s); it may not move a GPU, so it "
                       "was not the arm it is labelled" % (a["name"], moved))
    return out


def fmt(v, unit=""):
    return "-" if v is None else "%.0f%s" % (v, unit)


def report(arms):
    print("## What the optimizer did")
    print("")
    print("| arm | mode | transfers done | aborted | fill-timeout | wrong-pod | "
          "s a move was planned | model-s held back |")
    print("| --- | --- | --- | --- | --- | --- | --- | --- |")
    for a in arms:
        t = a["transfers"] or {}
        held = sum((a["blocked"] or {}).values()) if a["blocked"] is not None else None
        print("| %s | %s | %s | %s | %s | %s | %s | %s |" % (
            a["name"], MODE[a["name"]],
            *(fmt(t.get(o, 0.0)) for o in OUTCOMES),
            fmt(a["planned"]), fmt(held)))
    print("")
    print("Model-seconds held back, by reason (a model blocked for 60 s is 60):")
    print("")
    reasons = sorted({r for a in arms for r in (a["blocked"] or {})})
    if not reasons:
        print("- no model was held back in any arm.")
    else:
        print("| reason | " + " | ".join(a["name"] for a in arms) + " |")
        print("| --- |" + " --- |" * len(arms))
        for r in reasons:
            print("| %s | %s |" % (r, " | ".join(fmt((a["blocked"] or {}).get(r, 0.0)) for a in arms)))
    print("")
    print("The shadow arm moves nothing, so its TTFT should match today's; the seconds "
          "a move was planned are what the optimizer would have acted on. A shadow arm "
          "that differs from today by as much as share does says the run's spread is "
          "larger than the effect.")
    print("")
    print("## Placement")
    print("")
    print("Where each arm's pods landed, from its GPU sampler. A replica of N GPUs needs N "
          "free GPUs on one node, so on several nodes this decides what the optimizer could "
          "fund: GPUs free on two nodes, one each, fund no 2-GPU replica.")
    print("")
    for line in placement_table([(a["name"], a.get("placement")) for a in arms]):
        print(line)
    print("")


def main(argv):
    p = argparse.ArgumentParser(description=__doc__,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--out-root", help="the directory holding today/, shadow/ and share/")
    p.add_argument("--rewrite-policy", metavar="ARM",
                   help="rewrite the policy on stdin for ARM and print it")
    p.add_argument("--placement", metavar="GPUS_JSONL",
                   help="print where the arm's pods landed, as JSON")
    p.add_argument("--placement-summary", metavar="PLACEMENT_JSON",
                   help="print one line of a placement.json")
    args = p.parse_args(argv)
    if args.placement:
        try:
            pl = placement(args.placement)
        except OSError as e:
            print(e, file=sys.stderr)
            return 1
        if not pl["samples"]:
            print("%s has no sample with pods in it" % args.placement, file=sys.stderr)
            return 1
        json.dump(pl, sys.stdout, indent=1, sort_keys=True)
        print("")
        return 0
    if args.placement_summary:
        try:
            with open(args.placement_summary) as fh:
                print(placement_summary(json.load(fh)))
        except (OSError, ValueError) as e:
            print(e, file=sys.stderr)
            return 1
        return 0
    if args.rewrite_policy:
        try:
            sys.stdout.write(rewrite_policy(sys.stdin.read(), args.rewrite_policy))
        except ValueError as e:
            print(e, file=sys.stderr)
            return 2
        return 0
    if not args.out_root:
        p.error("--out-root is required")
    arms = [a for a in (load_arm(args.out_root, n) for n in ARMS) if a]
    if not arms or arms[0]["name"] != "today":
        print("no today arm in %s: it is the baseline" % args.out_root, file=sys.stderr)
        return 1
    bad = problems(arms)
    if bad:
        print("## The arms are not comparable")
        print("")
        for b in bad:
            print("- " + b)
        return 1
    report(arms)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
