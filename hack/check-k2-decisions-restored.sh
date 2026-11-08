#!/usr/bin/env bash
# The k1/k2 report's Restored column reads restored from the lines that carry
# it, and the P-codes stay in Priority: a restore is a field, not a source
# (docs/proposals/analyzer-learner.md, 6.1).
#
# Fixture events through render_report, no cluster. Requires python3 with
# PyYAML: dump_k2_decisions.py refuses to start without it.
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

python3 -c 'import yaml' 2>/dev/null || {
    echo "FATAL: python3 with PyYAML is required by this check (pip install pyyaml)."
    exit 1
}

PYTHONPATH="$ROOT/hack/benchmark" python3 - <<'PYCHECK'
import sys
from datetime import datetime, timezone

import dump_k2_decisions as d

T = "2026-10-16T10:00:%02d+00:00"


def event(msg, second, **fields):
    return dict(fields, _msg=msg, _ts=T % second, variant="v", modelID="m")


def rows(events):
    start = datetime(2026, 10, 16, 10, 0, tzinfo=timezone.utc)
    report = d.render_report(events, start, start, d.DEFAULT_CYCLE_GAP)
    table = [line for line in report.splitlines() if line.startswith("|")]
    header = [c.strip() for c in table[0].strip("|").split("|")]
    return [dict(zip(header, (c.strip() for c in line.strip("|").split("|")))) for line in table[2:]]


CASES = [
    ("a restored k2 on its decision line",
     [event(d.K2_MSG, 0, priority="P2-hist", restored=True)], "k2", "P2-hist"),
    ("a figure learned here",
     [event(d.K2_MSG, 0, priority="P2-hist", restored=False)], "-", "P2-hist"),
    ("a line from before the field",
     [event(d.K2_MSG, 0, priority="P2-hist")], "-", "P2-hist"),
    ("every learner that says so on learner-write, sorted",
     [event(d.K2_MSG, 0, priority="P1-obs"),
      event(d.LEARNER_MSG, 0, learner="mu", restored=True),
      event(d.LEARNER_MSG, 0, learner="itl", restored=True),
      event(d.LEARNER_MSG, 0, learner="start", restored=False)], "itl,mu", "P1-obs"),
    ("k2 from both lines, named once",
     [event(d.K2_MSG, 0, priority="P2-hist", restored=True),
      event(d.LEARNER_MSG, 0, learner="k2", restored=True)], "k2", "P2-hist"),
]

fails = 0
for name, events, restored, priority in CASES:
    got = rows(events)
    if len(got) != 1:
        print(f"  FAIL: {name}: {len(got)} rows, want 1")
        fails += 1
    elif got[0]["Restored"] != restored or got[0]["Priority"] != priority:
        print(f"  FAIL: {name}: Restored {got[0]['Restored']!r} Priority {got[0]['Priority']!r}, "
              f"want {restored!r} {priority!r}")
        fails += 1
    else:
        print(f"  ok: {name}")
print(f"{len(CASES) - fails} of {len(CASES)} cases passed")
sys.exit(1 if fails else 0)
PYCHECK
