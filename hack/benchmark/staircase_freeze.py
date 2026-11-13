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
window holds four relaxation times, at the top of the Ca2 band for a bursty
bucket, up to maxHoldSeconds; a step past that is flagged, not dropped.
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
    """[(rate, hold, flagged)] of one bucket.

    A bursty step is judged at the Ca^2 it sent (staircase_analyze.flag_steps), which
    stands anywhere in the band around the one asked for, so its window is sized for the
    band's top. Sized for the Ca^2 asked, r1's C-on step 6 held 457 s and needed 468 at
    the Ca^2 it sent: void by 11 s.
    """
    out = []
    discard, hold, cap = spec["discardSeconds"], spec["holdSeconds"], spec["maxHoldSeconds"]
    ca2 = bucket["ca2"] if bucket["ca2"] == 1 else bucket["ca2"] * lib.CA2_BAND[1]
    # The measurement-only steps above the prefill flow-control onset come last, held as
    # any other step: the analysis reports them and scores none.
    rates = [p["rate"] for p in bucket["curve"]] + sorted(lib.measure_rates(bucket, spec))
    for rate in rates:
        need, _, _ = lib.noise_floor(rate, bucket, spec["promptCV"], ca2)
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


def measure_refusals(frozen, spec):
    """Why the measurement-only steps cannot run as asked; empty when they can.

    They must run with flow control on (off, no gate closes), sit above every step the
    bucket scores, and below its decode onset: past that, decode binds first and the
    wait read is no longer the prefill gate's. At 2 + 2, B's prefill onset (53.2 r/s in
    r1) is past its decode knee (33.3), so no such step exists there.
    """
    bad, by_name = [], {b["name"]: b for b in spec.get("buckets", [])}
    for b in frozen["buckets"]:
        sent = by_name.get(b["name"], {})
        if not sent.get("measureAboveOnset"):
            continue
        rates = lib.measure_rates(b, spec)
        decode = (b.get("onsets") or {}).get("Decode") or 0
        if not rates:
            bad.append("%s: measurement steps asked, no prefill onset frozen" % b["name"])
            continue
        if sent.get("fc") != ["on"]:
            bad.append("%s: measurement steps above the flow-control onset run with flow control on alone" % b["name"])
        if not all(r < decode for r in rates):
            bad.append("%s: measurement steps %s reach the decode onset %.1f" % (
                b["name"], ", ".join("%.1f" % r for r in rates), decode))
        if b["curve"] and min(rates) <= max(p["rate"] for p in b["curve"]):
            bad.append("%s: measurement steps must sit above every scored step" % b["name"])
    return bad


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
    print("| Bucket | N_p + N_d | Lower knee | Onset | Steps | Measure only | Lengthened | Flagged | Hours a phase, all steps |", file=out)
    print("| --- | --- | --- | --- | --- | --- | --- | --- | --- |", file=out)
    cost = {}
    # A pod holds STAIRCASE_TP GPUs (1 unset), so GPU-hours are pods times that.
    gpus_per_pod = int(os.environ.get("STAIRCASE_TP") or 1)
    for b in frozen["buckets"]:
        steps = ladder_steps(b, spec)
        hours = (sum(h for _, h, _ in steps) + spec["phaseOverheadSeconds"]) / 3600
        cost[b["name"]] = (hours, (b["prefill"] + b["decode"]) * gpus_per_pod)
        print("| %s | %d + %d | %.1f | %.1f | %d | %d | %d | %d | %.2f |" % (
            b["name"], b["prefill"], b["decode"], b["knee"], b["onsets"]["Prefill"], len(steps),
            len(lib.measure_rates(b, spec)), sum(1 for _, h, _ in steps if h > spec["holdSeconds"]),
            sum(1 for _, _, f in steps if f), hours))
    print(file=out)
    print("| Set | Phases | Hours | GPU-hours |", file=out)
    print("| --- | --- | --- | --- |", file=out)
    rows = {}
    for ph in phases(frozen, spec):
        hours, gpus = cost[ph["bucket"]["name"]]
        key = "gate" if ph["gate"] else "held out"
        got = rows.setdefault(key, [0, 0.0, 0.0])
        got[0], got[1], got[2] = got[0] + 1, got[1] + hours, got[2] + hours * gpus
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
    bad = refusals(frozen, spec_sha) + measure_refusals(frozen, spec)
    if bad:
        sys.exit("refusing to write a ladder:\n  " + "\n  ".join(bad))
    print("%d phases" % write_ladder(frozen, spec, sys.argv[4]))


if __name__ == "__main__":
    main()
