#!/usr/bin/env python3
"""staircase_freeze.py -- from a frozen prediction to a ladder, and what it costs.

  staircase_freeze.py ladder <frozen.json> <spec.json> <ladder.txt> <spec.yaml>
  staircase_freeze.py plan   <frozen.json> <spec.json>

`whatif freeze` writes frozen.json; the spec is staircase_spec.yaml as JSON
(staircase.sh converts it with yq). ladder writes one phase a line:

    name fc schedule runaway seed prefill decode

with one `gate-check 1` line after repeat 1 of the gate buckets: staircase.sh scores
them there, and a fail stops the run. It refuses a frozen file with a refused bucket, an unpriced target, or a spec
other than the one it was frozen from. plan prints, per bucket, the steps with
their noise floor, and the GPU-hours. A step's hold is lengthened until its
window holds four relaxation times, up to maxHoldSeconds; a step past that is
flagged, not dropped.
"""
import hashlib
import json
import math
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import staircase_lib as lib  # noqa: E402


def load(path):
    with open(path, encoding="utf8") as f:
        return json.load(f)


def ladder_steps(bucket, spec):
    """[(rate, hold, flagged)] of one bucket."""
    out = []
    discard, hold, cap = spec["discardSeconds"], spec["holdSeconds"], spec["maxHoldSeconds"]
    for point in bucket["curve"]:
        rate = point["rate"]
        need, _, _ = lib.noise_floor(rate, bucket, spec["promptCV"])
        want = hold if math.isinf(need) else max(hold, discard + math.ceil(need))
        length = cap if math.isinf(need) else min(want, cap)
        out.append((rate, length, need > length - discard))
    return out


def phases(frozen, spec):
    """Every phase of the run, in order: repeat 1 of the gate buckets, the rest of repeat 1, then 2, then 3."""
    by_name = {b["name"]: b for b in spec["buckets"]}
    out = []
    for repeat in range(1, spec["repeats"] + 1):
        for bucket in frozen["buckets"]:
            sent = by_name[bucket["name"]]
            for fc in sent["fc"]:
                out.append({"name": "%s-%s-r%d" % (bucket["name"], fc, repeat), "fc": fc, "repeat": repeat,
                            "bucket": bucket, "gate": bool(sent.get("gate"))})
    out.sort(key=lambda p: (p["repeat"], not p["gate"]) if p["repeat"] == 1 else (p["repeat"], False))
    for i, phase in enumerate(out, 1):
        phase["seed"] = spec["seedBase"] + i
    return out


def schedule(bucket, spec):
    """The driver's --schedule for a bucket."""
    return ",".join("%d:%.3f:%d:%d:%g" % (hold, rate, bucket["prompt"], bucket["output"], bucket["ca2"])
                    for rate, hold, _ in ladder_steps(bucket, spec))


def p90_target(bucket):
    """The bucket's p90 target; the frozen stop rule is written on it."""
    return [t["seconds"] for t in bucket["targets"] if t["percentile"] == 90][0]


def refusals(frozen, spec_sha):
    """Why this file cannot start a run; empty when it can."""
    bad = []
    if spec_sha is not None and frozen.get("specSha256") != spec_sha:
        bad.append("the frozen file was written from another spec")
    gate = frozen.get("gate", {})
    for name in ("vp", "a", "decodeLine", "sloListed"):
        if not gate.get(name):
            bad.append("S2's gate: no %s at the frozen cycle" % name)
    for b in frozen["buckets"]:
        if b.get("refused"):
            bad.append("%s: %s" % (b["name"], b["refused"]))
        if not b.get("curve"):
            bad.append("%s: no ladder" % b["name"])
        for t in b.get("targets", []):
            if not (t["predictedOff"] > 0 and t["predictedOn"] > 0):
                bad.append("%s p%g: no predicted rate" % (b["name"], t["percentile"]))
    return bad


def write_ladder(frozen, spec, path):
    """Write the ladder; a file that exists is never replaced."""
    lines, gated = [], False
    for ph in phases(frozen, spec):
        if gated is False and not (ph["repeat"] == 1 and ph["gate"]):
            lines.append("gate-check 1\n")
            gated = True
        b = ph["bucket"]
        lines.append("%s %s %s %.4f %d %d %d\n" % (ph["name"], ph["fc"], schedule(b, spec),
                                                   spec["runawayFactor"] * p90_target(b), ph["seed"],
                                                   b["prefill"], b["decode"]))
    if not gated:
        lines.append("gate-check 1\n")
    with open(path, "x", encoding="utf8") as f:
        f.writelines(lines)
    return len(lines)


def plan(frozen, spec, out=sys.stdout):
    """The noise-floor table and the GPU-hours, gate and held-out apart."""
    print("| Bucket | N_p + N_d | Lower knee | Onset | Steps | Lengthened | Flagged | Hours a phase, all steps |", file=out)
    print("| --- | --- | --- | --- | --- | --- | --- | --- |", file=out)
    cost = {}
    for b in frozen["buckets"]:
        steps = ladder_steps(b, spec)
        hours = (sum(h for _, h, _ in steps) + spec["phaseOverheadSeconds"]) / 3600
        cost[b["name"]] = (hours, b["prefill"] + b["decode"])
        print("| %s | %d + %d | %.1f | %.1f | %d | %d | %d | %.2f |" % (
            b["name"], b["prefill"], b["decode"], b["knee"], b["onsets"]["Prefill"], len(steps),
            sum(1 for _, h, _ in steps if h > spec["holdSeconds"]), sum(1 for _, _, f in steps if f), hours))
    print(file=out)
    print("| Set | Phases | Hours | GPU-hours |", file=out)
    print("| --- | --- | --- | --- |", file=out)
    rows = {}
    for ph in phases(frozen, spec):
        hours, pods = cost[ph["bucket"]["name"]]
        key = "gate" if ph["gate"] else "held out"
        got = rows.setdefault(key, [0, 0.0, 0.0])
        got[0], got[1], got[2] = got[0] + 1, got[1] + hours, got[2] + hours * pods
    for key, (n, hours, gpu) in sorted(rows.items()):
        print("| %s, %d repeats | %d | %.1f | %.0f |" % (key, spec["repeats"], n, hours, gpu), file=out)
    print("\nWhat the prediction assumes. Read this before the first step: `router hop` here means R was not learned.\n", file=out)
    for b in frozen["buckets"]:
        print("- %s: flags %s; assumed: %s" % (b["name"], ", ".join(b.get("flags") or []) or "none",
                                               "; ".join(b.get("assumed") or []) or "nothing"), file=out)
    return rows


def main():
    if len(sys.argv) < 4:
        sys.exit(__doc__)
    verb = sys.argv[1]
    frozen, spec = load(sys.argv[2]), load(sys.argv[3])
    if verb == "plan":
        plan(frozen, spec)
        return
    if verb != "ladder" or len(sys.argv) != 6:
        sys.exit(__doc__)
    with open(sys.argv[5], "rb") as f:
        spec_sha = hashlib.sha256(f.read()).hexdigest()
    bad = refusals(frozen, spec_sha)
    if bad:
        sys.exit("refusing to write a ladder:\n  " + "\n  ".join(bad))
    print("%d phases" % write_ladder(frozen, spec, sys.argv[4]))


if __name__ == "__main__":
    main()
