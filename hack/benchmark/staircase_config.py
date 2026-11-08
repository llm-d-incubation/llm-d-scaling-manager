#!/usr/bin/env python3
"""staircase_config.py -- the two config edits a staircase makes, by text, each refusing a surprise.

  staircase_config.py fc-off   <pd-config.yaml> <out>
  staircase_config.py list-slo <policy.yaml> <out> <ttft target, e.g. 200ms>

fc-off drops the router's flowControl feature gate and block. list-slo adds the
slo analyzer and a TTFT target to a scaling policy, so the first cycle of the
warm-up holds slo: S2's gate reads V_P, a, slo and a decode line on one cycle,
and the first staircase's warm-up ran without it. slo is shadow, and the
prediction does not read the target.

Both work on text so that they keep the file as rendered, and both refuse rather
than guess: an anchor that does not match exactly once is an error.
"""
import re
import sys


def drop_flow_control(src):
    """The router's config with no featureGates or flowControl block."""
    lines, out, i, gates, blocks = src.split("\n"), [], 0, 0, 0
    while i < len(lines):
        line = lines[i]
        if line == "featureGates:":
            gates += 1
            j = i + 1
            items = []
            # The rendered config puts the list items at column 0 ("- flowControl").
            while j < len(lines) and (lines[j].startswith("  ") or lines[j].startswith("- ")):
                items.append(lines[j].strip())
                j += 1
            if items != ["- flowControl"]:
                sys.exit("featureGates holds %r, not only flowControl" % items)
            i = j
            continue
        if line == "flowControl:":
            blocks += 1
            j = i + 1
            while j < len(lines) and (lines[j].startswith(" ") or lines[j] == ""):
                j += 1
            i = j
            continue
        out.append(line)
        i += 1
    if gates != 1 or blocks != 1:
        sys.exit("found featureGates=%d flowControl=%d blocks, want 1 and 1" % (gates, blocks))
    res = "\n".join(out)
    if re.search(r"^(featureGates|flowControl):", res, re.M):
        sys.exit("output still holds a flow-control key")
    for key in ("plugins:", "schedulingProfiles:"):
        if not re.search("^" + key, res, re.M):
            sys.exit("output lost " + key)
    return res


def list_slo(policy, target):
    """The policy with slo beside saturation, and a TTFT target at p95."""
    if policy.count("  - name: saturation\n") != 1:
        sys.exit("want one saturation analyzer entry")
    if "\nslo:" in policy or "name: slo" in policy:
        sys.exit("slo is already in the policy")
    m = re.search(r"analyzers:\n  - name: saturation\n(    .*\n)*", policy)
    if not m:
        sys.exit("no analyzers block with saturation first")
    res = policy[:m.end()] + "  - name: slo\n" + policy[m.end():]
    return res.rstrip("\n") + "\nslo:\n  ttft: {target: %s, percentile: 95}\n" % target


def main():
    if len(sys.argv) < 4:
        sys.exit(__doc__)
    verb, src, dst = sys.argv[1:4]
    with open(src, encoding="utf8", newline="") as f:
        text = f.read()
    if verb == "fc-off" and len(sys.argv) == 4:
        res = drop_flow_control(text)
    elif verb == "list-slo" and len(sys.argv) == 5:
        res = list_slo(text, sys.argv[4])
    else:
        sys.exit(__doc__)
    with open(dst, "w", encoding="utf8", newline="") as f:
        f.write(res)


if __name__ == "__main__":
    main()
