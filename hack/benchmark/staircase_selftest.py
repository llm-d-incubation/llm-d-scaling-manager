#!/usr/bin/env python3
"""staircase_selftest.py -- the staircase's arithmetic against figures worked by hand.

Run by hack/check-staircase.sh. A failed line prints what it read; the count is
asserted, so a section that stops running does not read as green.
"""
import asyncio
import gzip
import http.server
import importlib.util
import io
import json
import math
import multiprocessing
import os
import random
import re
import subprocess
import sys
import tempfile
import threading
import urllib.error
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import staircase_analyze as analyze  # noqa: E402
import staircase_config as config  # noqa: E402
import staircase_deadline as deadline  # noqa: E402
import staircase_freeze as freeze  # noqa: E402
import staircase_lib as lib  # noqa: E402

spec_load = importlib.util.spec_from_file_location("staircase_load", os.path.join(HERE, "staircase_load.py"))
load = importlib.util.module_from_spec(spec_load)
spec_load.loader.exec_module(load)

EXPECTED = 200
checks = fails = 0


def ok(name, cond, got=""):
    global checks, fails
    checks += 1
    if not cond:
        fails += 1
        print("FAIL: %s %s" % (name, got))


def near(name, got, want, tol=1e-9):
    ok(name, abs(got - want) <= tol, "got %r want %r" % (got, want))


def step(rate, p90, level=1, achieved=None, **kw):
    row = dict(rate=rate, level=level, achieved=achieved or rate, client_ttft_p90=p90)
    row.update(kw)
    return row


# --- percentiles and histograms ---------------------------------------------------------
near("pct, nearest rank", lib.pct([1, 2, 3, 4, 5, 6, 7, 8, 9, 10], 0.9), 10)
near("pct, below the top", lib.pct(list(range(1, 21)), 0.9), 19)
ok("pct of nothing", math.isnan(lib.pct([], 0.5)))
# 100 requests: 50 under 0.1, 90 under 0.2, all under 0.4. p90 sits on the 0.2 edge,
# p95 a quarter of the way through 0.2 to 0.4.
CUM = [(0.1, 50), (0.2, 90), (0.4, 100), (math.inf, 100)]
near("hist p50", lib.hist_quantile(CUM, 0.5)[0], 0.1)
near("hist p90", lib.hist_quantile(CUM, 0.9)[0], 0.2)
near("hist p95", lib.hist_quantile(CUM, 0.95)[0], 0.3)
ok("hist bucket", lib.hist_quantile(CUM, 0.95)[1:] == (0.2, 0.4))
ok("hist past the last edge", lib.hist_quantile([(0.1, 1), (math.inf, 100)], 0.9)[0] == math.inf)
ok("hist empty", math.isnan(lib.hist_quantile([(0.1, 0), (math.inf, 0)], 0.9)[0]))

# --- Ca2 --------------------------------------------------------------------------------
near("gaps equal: Ca2 0", lib.arrival_ca2([0, 1, 2, 3, 4]), 0)
# Gaps 1 and 3: mean 2, variance 1, so 1/4.
near("gaps 1 and 3", lib.arrival_ca2([0, 1, 4, 5, 8]), 0.25)
ok("one send has no Ca2", math.isnan(lib.arrival_ca2([1.0])))
BY_POD = lib.ca2_by_pod([("a", 0), ("b", 1), ("a", 2), ("b", 3), ("a", 4), ("", 9)])
ok("Ca2 by pod, no pod is dropped", sorted(BY_POD) == ["a"] or sorted(BY_POD) == ["a", "b"], BY_POD)
near("Ca2 by pod, even gaps", BY_POD["a"], 0)
ok("Ca2 by pod, none known", lib.ca2_by_pod([("", 1), ("", 2)]) == {})

for target in (0, 1, 2, 4, 8):
    rng = random.Random(7)
    gap = load.gap_sampler(rng, 40.0, target)
    gaps = [gap() for _ in range(200000)]
    near("sampler mean, Ca2 %g" % target, sum(gaps) / len(gaps), 1 / 40.0, 3e-4)
    near("sampler Ca2 %g" % target, lib.scv(gaps), target, 0.02 + 0.03 * target)
plan = load.arrivals(load.parse_schedule("100:10:1000:200:1,100:20:8000:200:4"), 5)
ok("schedule splits by level", {r[1] for r in plan} == {1, 2}, len(plan))
ok("arrivals are the seed's", plan == load.arrivals(load.parse_schedule("100:10:1000:200:1,100:20:8000:200:4"), 5))
ok("another seed, other arrivals", plan != load.arrivals(load.parse_schedule("100:10:1000:200:1,100:20:8000:200:4"), 6))

# --- the noise floor --------------------------------------------------------------------
# The page's worked case: rho 0.85 at Cs^2 1 is about 330 E[S]; 90 s at E[S] 0.267 s.
near("relaxation at rho 0.85", lib.relaxation(1, 0.85, 1, 1) / 2 * 2, 2 / (1 - math.sqrt(0.85)) ** 2)
ok("about 330 E[S]", 300 < lib.relaxation(1, 0.85, 1, 1) / 1 < 370, lib.relaxation(1, 0.85, 1, 1))
ok("rho 1 never relaxes", lib.relaxation(1, 1.0, 1, 1) == math.inf)
B = dict(prompt=8000.0, vp=288288.0, imbalance=1.193, prefill=2, decode=2, decodeKnee=43.953, ca2=1)
need, rho_p, rho_d = lib.noise_floor(38.679, B, 0.0577)
# M2's table: B at step 6, decode rho 0.88, 4T 95 s.
near("M2, decode rho at 38.7", rho_d, 0.88, 0.001)
near("M2, 4T at 38.7", need, 95, 1)
near("prefill rho", rho_p, 38.679 * 8000 / 288288 * 1.193 / 2, 1e-9)
near("at the prefill knee rho reads 1", lib.noise_floor(57.4, dict(B, prefillKnee=57.4), 0.0577)[1], 1)
ok("and no window holds it", lib.noise_floor(57.4, dict(B, prefillKnee=57.4), 0.0577)[0] == math.inf)
need_c, _, _ = lib.noise_floor(38.679, dict(B, ca2=4), 0.0577, 4.2)
near("M2, C's 4T at 38.7", need_c, 247, 5)

# --- where the ladder meets a target ----------------------------------------------------
RUNG = [step(10, 0.1, 1), step(20, 0.2, 2), step(30, 0.4, 3)]
ok("interpolated", lib.measured(RUNG, "client_ttft_p90", 0.3) == ("=", 25.0), lib.measured(RUNG, "client_ttft_p90", 0.3))
ok("first step misses", lib.measured(RUNG, "client_ttft_p90", 0.05) == ("below", 10))
ok("top still meets", lib.measured(RUNG, "client_ttft_p90", 1.0) == ("at least", 30))
ok("an infinite step brackets the crossing: it is on the cliff",
   lib.measured([step(10, 0.1), step(20, math.inf)], "client_ttft_p90", 0.2, bounds=False) == ("cliff", (10, 20)))
SHORT = [step(10, 0.1, 1), step(20, 0.2, 2), step(30, 0.4, 3, achieved=20.0)]
# A rate off its band can move a reading either way: it closes no bracket, so it is open.
ok("a crossing on a short step is a bound, open above", lib.measured(SHORT, "client_ttft_p90", 0.3) == ("bound", (20, math.inf)),
   lib.measured(SHORT, "client_ttft_p90", 0.3))
NOISY = [step(10, 0.1, 1), step(20, 0.2, 2, noise_flag=True), step(30, 0.4, 3)]
# A step under the noise floor reads low: it cannot be the low end, so the bracket widens.
ok("a crossing past a noisy step is a bound from the last step that can hold it",
   lib.measured(NOISY, "client_ttft_p90", 0.3) == ("bound", (10, 30)), lib.measured(NOISY, "client_ttft_p90", 0.3))
# The reviewer's probe: 30 valid 0.10 s, 34 short of its schedule 0.15 s, 36 valid 0.30 s,
# target 0.2 s. Bracketed from 34 it read 0.96 to 1.01 of 34.5 and passed; 34 reads low,
# so the bracket is 30 to 36, 0.96 to 1.15, across the mark's edge.
PROBE = [step(30, 0.10, 1), step(34, 0.15, 2, scheduled=10000, recorded=9000), step(36, 0.30, 3)]
GOT = lib.measured(PROBE, "client_ttft_p90", 0.2)
ok("a step short of its schedule is no low end", GOT == ("bound", (30, 36)), GOT)
ok("and the probe's bound is across the mark, not in it", lib.mark(34.5, *GOT)[1] == "no mark, a bound", lib.mark(34.5, *GOT))
JUMPED = [step(30, 0.10, 1), step(34, 0.15, 2, noise_flag=True), step(36, 5.0, 3)]
GOT = lib.measured(JUMPED, "client_ttft_p90", 0.2)
ok("a bound whose ends are past the cliff factor apart is a jump", GOT == ("jump", (30, 36)), GOT)
ok("and a jump is never in the mark, even with its bracket inside it", lib.mark(32, *GOT)[1] == "no mark, a bound across a jump",
   lib.mark(32, *GOT))
ok("a bracket at exactly the mark's limits, 0.75 and 1.10, is in it", lib.mark(33, "bound", (30, 44))[1] == "in the mark, a bound",
   lib.mark(33, "bound", (30, 44)))
ok("an open bracket is never in the mark", lib.mark(35, "bound", (34, math.inf))[1] == "no mark, a bound"
   and lib.mark(20, "bound", (34, math.inf))[1] == "FAIL, a bound")
ok("NaN reads no data", lib.measured([step(10, math.nan)], "client_ttft_p90", 1)[0] == "no data")
ok("no row reads no data", lib.measured([], "client_ttft_p90", 1)[0] == "no data")

ALLVOID = [step(10, 0.1, 1, noise_flag=True), step(20, 0.2, 2, noise_flag=True), step(30, 0.4, 3, noise_flag=True)]
ok("every step void has no crossing", lib.measured(ALLVOID, "client_ttft_p90", 0.3) == ("void", None))
FIRSTVOID = [step(10, 0.5, 1, noise_flag=True), step(20, 0.6, 2)]
ok("a void first step over target is void, not a fail", lib.measured(FIRSTVOID, "client_ttft_p90", 0.3) == ("void", None))
TOPVOID = [step(10, 0.1, 1), step(20, 0.2, 2, noise_flag=True)]
ok("at least reads the top valid step", lib.measured(TOPVOID, "client_ttft_p90", 1.0) == ("at least", 10))
CUT = dict(rate=10, achieved=10, window_s=150, truncated=True, stopped_by=lib.RULE_INFLIGHT)
ok("a cut step is void", lib.invalid(CUT) and "max-inflight" in lib.validity(CUT)[0])
ok("a cap on its step is void even uncut", lib.invalid(dict(CUT, truncated=False)))
ok("the p90 rule at a step's end voids nothing", not lib.invalid(dict(CUT, truncated=False, stopped_by=lib.RULE_P90)))
CUTRUNG = [step(10, 0.1, 1), step(20, 0.2, 2), dict(step(30, 0.4, 3), truncated=True, stopped_by=lib.RULE_INFLIGHT)]
# A cut can only raise a reading: it may be the low end, never the high one.
ok("a crossing on a cut step is a bound, open above", lib.measured(CUTRUNG, "client_ttft_p90", 0.3) == ("bound", (20, math.inf)))

# --- the mark ---------------------------------------------------------------------------
ok("in the mark", lib.mark(10, "=", 10)[1] == "in the mark")
ok("edge at 1.10 passes", lib.mark(11, "=", 10)[1] == "in the mark")
ok("over 1.10", lib.mark(11.1, "=", 10)[1] == "FAIL, over-promises")
ok("edge at 0.75 passes", lib.mark(7.5, "=", 10)[1] == "in the mark")
ok("under 0.75", lib.mark(7.4, "=", 10)[1] == "FAIL, under-promises")
ok("below the ladder over-promises", lib.mark(36, "below", 17)[1] == "FAIL, over-promises")
ok("at least carries no mark", lib.mark(36, "at least", 30)[1] == "no mark")
ok("a bound inside", lib.mark(40, "bound", (38.7, 41.3))[1] == "in the mark, a bound")
ok("a bound straddling", lib.mark(44, "bound", (38.7, 41.3))[1] == "no mark, a bound")
ok("a bound outside", lib.mark(60, "bound", (38.7, 41.3))[1] == "FAIL, a bound")
ok("every repeat must pass", analyze.judge(10, [("=", 10), ("=", 5)])[2] == "FAIL")
ok("no valid crossing has its verdict", lib.mark(10, "void", None)[1] == lib.NO_VALID_CROSSING)
ok("judge keeps the bound", analyze.judge(10, [("bound", (9.5, 10.5)), ("bound", (9.5, 10.5)), ("=", 10)])[2] == "in the mark, a bound")
ok("judge keeps no valid crossing", analyze.judge(10, [("void", None), ("=", 10)])[2] == lib.NO_VALID_CROSSING)
ok("judge keeps a fail over all of it", analyze.judge(10, [("void", None), ("=", 5)])[2].startswith("FAIL"))
ok("a repeat with no figure, no mark", analyze.judge(10, [("=", 10), ("at least", 8)])[2] == "no mark")
near("median of the repeats", float(analyze.judge(10, [("=", 8), ("=", 10), ("=", 12)])[1]), 1.0)

# --- which rule stopped a step ----------------------------------------------------------
LOG = ["2026-11-06T02:00:00Z LEVEL 1 rate=10 in=8000 out=200 ca2=1 hold=600s\n",
       "2026-11-06T02:10:00Z LEVEL 2 rate=20 in=8000 out=200 ca2=1 hold=600s\n"]
_, old_cap = lib.parse_log(LOG + ["2026-11-06T02:15:30Z RUNAWAY level=2 inflight=4535\n"])
_, old_p90 = lib.parse_log(LOG + ["2026-11-06T02:20:00Z RUNAWAY level=2 client-p90=0.4124 above 0.3286\n"])
_, new_cap = lib.parse_log(LOG + ["2026-11-06T02:15:30Z RUNAWAY level=2 rule=max-inflight inflight=4535\n"])
_, new_p90 = lib.parse_log(LOG + ["2026-11-06T02:20:00Z RUNAWAY level=2 rule=client-p90 client-p90=0.4 above 0.3\n"])
ok("the first run's cap line", old_cap["rule"] == lib.RULE_INFLIGHT)
ok("the first run's p90 line", old_p90["rule"] == lib.RULE_P90)
ok("the driver's cap line", new_cap["rule"] == lib.RULE_INFLIGHT and "4535" in new_cap["detail"])
ok("the driver's p90 line", new_p90["rule"] == lib.RULE_P90)
ok("no stop", lib.parse_log(LOG)[1] is None)
LEVELS = lib.parse_log(LOG)[0]
a, b = lib.judged_window(LEVELS[1], new_cap, 180)
near("a cap cuts the window at the stop", b - a, 150)
a, b = lib.judged_window(LEVELS[0], new_cap, 180)
near("an earlier step keeps its window", b - a, 420)
a, b = lib.judged_window(LEVELS[1], dict(new_cap, t=LEVELS[1]["start"] + 60), 180)
near("a stop in the discard leaves nothing", b - a, 0)
ok("the knee, held then not", lib.knee_bracket([
    dict(rate=10, failed=0, completion_rate=10, achieved=10), dict(rate=20, failed=0, completion_rate=19.5, achieved=20),
    dict(rate=30, failed=0, completion_rate=20, achieved=30), dict(rate=40, failed=0, completion_rate=40, achieved=40)]) == (20, 30))
ok("a failure is not held", not lib.held(dict(failed=1, completion_rate=10, achieved=10)))

# --- what voids a step ------------------------------------------------------------------
GOOD = dict(rate=10, achieved=10, lag_p99=0.01, failed=0, due=1000)
ok("a good step", not lib.invalid(GOOD))
ok("send lag voids", lib.invalid(dict(GOOD, lag_p99=0.2)))
ok("1% failed is not void", not lib.invalid(dict(GOOD, failed=10)))
ok("over 1% failed voids", lib.invalid(dict(GOOD, failed=11)))
ok("a fleet that moved voids", lib.invalid(dict(GOOD, replica_change=True)))
ok("a Ca2 to rerun voids", lib.invalid(dict(GOOD, ca2_rerun=True, ca2_sent=6, ca2_asked=4)))
ok("3 to 5 around 4 stands", not lib.ca2_out_of_band(3.0, 4) and not lib.ca2_out_of_band(5.0, 4))
ok("outside 3 to 5 reruns", lib.ca2_out_of_band(2.9, 4) and lib.ca2_out_of_band(5.1, 4))
ok("Poisson has no band", not lib.ca2_out_of_band(1.4, 1))
FLEET = dict(prompt=8000.0, vp=288288.0, imbalance=1.19, prefill=2, decode=2, decodeKnee=44.0, ca2=1)
FLAGGED = [dict(GOOD, rate=20, achieved=20, ca2_sent=1.0, window_s=420, replicas=dict(decode=[2, 3], prefill=[2])),
           dict(GOOD, rate=20, achieved=20, ca2_sent=1.0, window_s=420, replicas=dict(decode=[2], prefill=[2]))]
analyze.flag_steps(FLAGGED, FLEET, 0.0577)
ok("replica samples other than the fleet's", FLAGGED[0]["replica_change"] and not FLAGGED[1]["replica_change"])
ok("a repeat missing is no pass", "missing" in analyze.judge(10, [("=", 10)], [2])[2])
ok("a fail stands with a repeat missing", analyze.judge(10, [("=", 5)], [2])[2].startswith("FAIL"))
ok("no repeat finished is no pass", "missing" in analyze.judge(10, [], [1])[2])

# --- r1's gate, read again: each rule against the figures that showed it wrong ---------
# The envelope. r1's A-on, client p95 against its 58.0 ms target: step 2 read 64.6 over
# one bad minute, and the old rule put the crossing there, at 16.8 r/s.
A_ON = [step(12.754, 0, 1, client_ttft_p95=0.0467), step(19.131, 0, 2, client_ttft_p95=0.0646),
        step(23.914, 0, 3, client_ttft_p95=0.0564), step(25.508, 0, 4, client_ttft_p95=0.0587),
        step(27.103, 0, 5, client_ttft_p95=0.0615)]
GOT = lib.measured(A_ON, "client_ttft_p95", 0.05798)
ok("r1 A-on p95: a one-minute transient no longer sets the crossing (was 16.8)",
   GOT[0] == "=" and 24.9 < GOT[1] < 25.1, GOT)
ok("the envelope lowers step 2 to step 3's reading, and nothing else",
   lib.envelope(A_ON, "client_ttft_p95") == [0.0467, 0.0564, 0.0564, 0.0587, 0.0615], lib.envelope(A_ON, "client_ttft_p95"))
ok("a void step lowers nothing", lib.measured(
    [step(10, 0.1, 1), step(20, 0.35, 2), step(30, 0.15, 3, noise_flag=True)], "client_ttft_p90", 0.3) == ("=", 18.0))
ok("a ladder that only rises is read as before", lib.envelope(RUNG, "client_ttft_p90") == [0.1, 0.2, 0.4])

# The rate sent against the rate asked, judged at the count's own spread.
near("the band at r1's C step 5: 3 sd of a Ca2 4 count over 420 s", lib.achieved_tolerance(28.299, 420, 4), 0.0551, 1e-4)
SENT = dict(GOOD, window_s=420, ca2_asked=4)
ok("r1 C-off step 5, 29.73 of 28.30, is not void (was)", not lib.invalid(dict(SENT, rate=28.299, achieved=29.73)),
   lib.validity(dict(SENT, rate=28.299, achieved=29.73)))
ok("r1 C-on step 3, 24.21 of 24.97, is not void (was)", not lib.invalid(dict(SENT, rate=24.970, achieved=24.21)))
ok("r1 A-off step 1, 13.20 of 12.75 Poisson, is not void (was)",
   not lib.invalid(dict(SENT, ca2_asked=1, rate=12.754, achieved=13.20)))
ok("a shortfall the spread does not explain still voids", lib.invalid(dict(SENT, rate=28.299, achieved=26.0)))
ok("a long Poisson step is held tighter than 3%: 2% over 1320 s voids",
   lib.invalid(dict(SENT, ca2_asked=1, window_s=1320, rate=33.293, achieved=33.293 * 1.02)))

# The C ladder at 2 + 2, from r1's frozen figures and the spec this tree carries.
C_R1 = dict(name="C", prompt=8000, output=200, ca2=4, prefill=2, decode=2, vp=289923.64, imbalance=1.232429351816981,
            prefillKnee=55.87084111432735, decodeKnee=33.29334228265582, knee=33.29334228265582)
LADDER_SPEC = dict(promptCV=0.0577, discardSeconds=180, holdSeconds=600, maxHoldSeconds=1500)
with open(os.path.join(HERE, "staircase_spec.yaml"), encoding="utf8") as f:
    SPEC_TEXT = f.read()
C_LINE = [line for line in SPEC_TEXT.splitlines() if line.startswith("- {name: C,")]
C_STEPS = re.search(r"steps: \[([^\]]*)\]", C_LINE[0] if C_LINE else "") or re.search(r"(?m)^steps: \[([^\]]*)\]", SPEC_TEXT)
C_STEPS = [float(x) for x in C_STEPS[1].split(",")]
C_LADDER = freeze.ladder_steps(dict(C_R1, curve=[dict(rate=s * C_R1["knee"]) for s in C_STEPS]), LADDER_SPEC)
NEAR = [(rate, hold) for rate, hold, _ in C_LADDER if 0.80 <= rate / C_R1["knee"] < 0.95
        and hold - LADDER_SPEC["discardSeconds"] >= lib.noise_floor(rate, C_R1, 0.0577, 5.0)[0]]
ok("C at 2 + 2 has six steps from 0.80 to 0.95 of the knee a crossing can land on (r1 had 2)", len(NEAR) >= 6,
   [(round(r, 2), h) for r, h in NEAR])
STEP6 = freeze.ladder_steps(dict(C_R1, curve=[dict(rate=29.964)]), LADDER_SPEC)[0]
ok("r1's C-on step 6 holds the floor at the Ca2 it sent, 4.12 (needed 468 s in a 457 s window)",
   STEP6[1] - 180 >= lib.noise_floor(29.964, C_R1, 0.0577, 4.12)[0], STEP6)

# The cliff. r1's C-onset, client p90 against 963.4 ms: 591.6 ms at 51.4 r/s, then 31.5 s at
# 53.6, where 1441 requests failed.
C_ONSET = [step(39.110, 0.3407, 1), step(47.490, 0.4942, 2), step(51.401, 0.5916, 3),
           step(53.636, 31.5023, 4, failed=1441, due=58028)]
GOT = lib.measured(C_ONSET, "client_ttft_p90", 0.9634)
ok("r1 C-onset p90 is on the cliff, not a bound", GOT == ("cliff", (51.401, 53.636)), GOT)
ok("and its bracket, 0.89 to 0.92 of 47.5, is in the mark", lib.mark(47.490, *GOT)[1] == lib.CLIFF_IN)
ok("a bracket across the mark is no mark", lib.mark(40.0, *GOT)[1] == "no mark, on the cliff")
ok("a bracket past it fails", lib.mark(70.0, *GOT)[1] == "FAIL, on the cliff")
ok("the cliff needs a valid step below the crossing: a void one makes a bound", lib.measured(
    [step(47.490, 0.4942, 2, noise_flag=True), step(51.401, 0.5916, 3), step(53.636, 31.5, 4)], "client_ttft_p90", 0.9634)[0] == "cliff"
   and lib.measured([step(47.490, 0.4942, 2), step(51.401, 0.5916, 3, noise_flag=True), step(53.636, 31.5, 4)],
                    "client_ttft_p90", 0.9634)[0] == "jump")
ok("a top step void by send lag brackets no cliff: the client times from the schedule", lib.measured(
    [step(51.401, 0.5916, 3), step(53.636, 31.5, 4, lag_p99=2.0)], "client_ttft_p90", 0.9634)[0] == "bound")
ok("steps less than the factor apart interpolate", lib.measured(RUNG, "client_ttft_p90", 0.3) == ("=", 25.0))
ok("the cliff never reads as a pass over repeats", analyze.judge(47.49, [GOT, GOT])[2] == lib.CLIFF_IN)
# What else keeps a top step from bracketing a cliff: each alone makes a bound.
BELOW = step(51.401, 0.5916, 3)
for why, extra in (("a cut", dict(truncated=True)), ("the in-flight cap", dict(stopped_by=lib.RULE_INFLIGHT)),
                   ("a fleet that moved", dict(replica_change=True)), ("a Ca2 to rerun", dict(ca2_rerun=True, ca2_sent=6, ca2_asked=4)),
                   ("a failure not the fleet's", dict(failed_other=3)), ("a restarted EPP", dict(epp_pods=["epp-a", "epp-b"])),
                   ("an engine error", dict(engine_errors=1))):
    TOP = step(53.636, 31.5, 4, **extra)
    ok("%s in the top step brackets no cliff" % why, lib.measured([BELOW, TOP], "client_ttft_p90", 0.9634)[0] == "bound",
       lib.measured([BELOW, TOP], "client_ttft_p90", 0.9634))
ok("fleet failures: a 5xx, a 429, a timeout, a run past the drain; not a refused connection or a 4xx",
   all(lib.fleet_failure(s, e) for s, e in (("503", ""), ("429", "x"), ("0", "TimeoutError"), ("0", "cancelled")))
   and not any(lib.fleet_failure(s, e) for s, e in (("0", "ClientConnectorError"), ("400", "bad"))))
ok("and fleet failures alone keep the cliff", lib.measured([BELOW, step(53.636, 31.5, 4, failed=1441, due=58028, failed_other=0)],
                                                          "client_ttft_p90", 0.9634)[0] == "cliff")

# The crossing is read at the rate sent. r1's C-off step 5 asked 28.30 and its seeded
# schedule put 29.73 into the window: its reading is 29.73's.
SENT_RUNG = [step(26.635, 0.30, 4, achieved=26.70, scheduled=11214, recorded=11214),
             step(28.299, 0.34, 5, achieved=29.73, scheduled=12487, recorded=12487)]
GOT = lib.measured(SENT_RUNG, "client_ttft_p90", 0.32, x="achieved")
ok("the crossing is interpolated on the rate sent, not the rate asked", GOT[0] == "=" and abs(GOT[1] - (26.70 + 0.5 * (29.73 - 26.70))) < 1e-9, GOT)
COMPLETE = dict(GOOD, scheduled=12000, recorded=11990)
ok("a step whose rows meet its schedule, to the slack, is not void", not lib.invalid(COMPLETE), lib.validity(COMPLETE))
ok("a step short of its schedule is void, whatever its rate", lib.validity(dict(COMPLETE, recorded=11980)) == ["recorded 11980 of 12000 scheduled"],
   lib.validity(dict(COMPLETE, recorded=11980)))
ok("with a schedule the rate band is not read: the schedule fixed the rate", not lib.invalid(dict(COMPLETE, rate=28.299, achieved=26.0, window_s=420)))
DYING = [step(10, 0.30, 1, due=4000), step(20, 0.12, 2, due=8000, scheduled=8000, recorded=5000)]
ok("a dying load reads low, and lowers no earlier step", lib.envelope(DYING, "client_ttft_p90") == [0.30, 0.12])

# The envelope lowers past the sampling band or on a transient, and pools inside it.
POOL = [step(10, 0.20, 1, due=3000, client_ttft_p90_lo=0.17), step(20, 0.18, 2, due=1000)]
VALS, MARKS = lib.envelope_marks(POOL, "client_ttft_p90")
ok("an excess inside the step's band pools the two, weighted by requests", MARKS == ["pooled", "pooled"]
   and abs(VALS[0] - 0.195) < 1e-9 and abs(VALS[1] - 0.195) < 1e-9, (VALS, MARKS))
ok("past the band it is lowered", lib.envelope_marks([dict(POOL[0], client_ttft_p90_lo=0.19), POOL[1]], "client_ttft_p90")
   == ([0.18, 0.18], ["lowered", ""]))
ok("a step that moved in its window is lowered though inside its band",
   lib.envelope_marks([dict(POOL[0], drift_p90=2.0), POOL[1]], "client_ttft_p90")[1] == ["lowered", ""])
USED = []
lib.measured([step(5, 0.1, 0)] + POOL, "client_ttft_p90", 0.15, used=USED)
ok("a crossing read on a pooled step says so", USED == ["pooled"] and analyze.moved_note(USED) == " (read on pooled steps)", USED)
near("the quantile band: ranks q n -/+ 1.96 sqrt(n q (1 - q)) of 100", lib.pct_ci(list(range(1, 101)), 0.9)[0], 84)


class CancelledPost:
    """A session whose request is cut while running, as the drain cuts it."""

    def post(self, *args, **kwargs):
        return self

    async def __aenter__(self):
        raise asyncio.CancelledError()

    async def __aexit__(self, *args):
        return False


async def cancelled_row():
    rows = []
    shared = {k: multiprocessing.Value("q", 0) for k in ("ok", "err", "inflight")}
    await load.fire(CancelledPost(), "u", "m", "t", (7, (1.0, 2, 10, 5)), 100.0, rows, shared)
    return rows, shared


ROWS, SHARED = asyncio.run(cancelled_row())
ok("a cancelled request still writes a row", len(ROWS) == 1 and "cancelled" in ROWS[0] and ROWS[0].startswith("7,2,"), ROWS)
ok("and leaves nothing in flight", SHARED["inflight"].value == 0 and SHARED["err"].value == 1)

# --- a phase from its files -------------------------------------------------------------
with tempfile.TemporaryDirectory() as run:
    d = os.path.join(run, "phases", "B-off")
    os.makedirs(d)
    base = lib.ts("2026-11-06T02:00:00Z")
    with open(d + "/sweep.log", "w", encoding="utf8") as f:
        # Level 2's stamp is 6 s late, as a log's can be: the rows say when it began.
        f.writelines([LOG[0], "2026-11-06T02:10:06Z LEVEL 2 rate=20 in=8000 out=200 ca2=1 hold=600s\n",
                      "2026-11-06T02:15:30Z RUNAWAY level=2 rule=max-inflight inflight=4535\n"])
    with gzip.open(d + "/requests.csv.gz", "wt") as f:
        f.write("index,level,scheduled,sent,first,last,status,prompt_tokens,tokens,error,pod\n")
        for i in range(600):  # step 1: one a second, every 2 s to each of two pods
            t = base + i
            f.write("%d,1,%.4f,%.4f,%.4f,%.4f,200,8000,101,,pod-%d\n" % (i, t, t, t + 0.1, t + 1.1, i % 2))
        for i in range(300):  # step 2, up to the stop and past it: 10% never finish
            t = base + 600 + i
            if i == 30:  # a row of level 1 due late must not land in level 2's window
                f.write("9,1,%.4f,%.4f,%.4f,%.4f,200,8000,101,,pod-0\n" % (t + 200, t + 200, t + 200.1, t + 201))
            bad = i % 10 == 0
            f.write("%d,2,%.4f,%.4f,%.4f,%.4f,%d,8000,101,%s,pod-0\n" % (
                1000 + i, t, t, 0 if bad else t + 0.2, 0 if bad else t + 1.2, 429 if bad else 200, "x" if bad else ""))
    rows, stop = analyze.analyze_steps(run, "B-off", 180)
    ok("two steps", len(rows) == 2, len(rows))
    one, two = rows
    near("step 1 window", one["window_s"], 420)
    ok("step 1 no rule", one["stopped_by"] == "" and not one["truncated"])
    near("step 1 due", one["due"], 420)
    near("step 1 TTFT", one["client_ttft_p90"], 0.1, 1e-3)
    near("step 1 TPOT: 1 s over 100 gaps", one["client_tpot_mean"], 0.01, 1e-6)
    near("step 1 completions", one["completion_rate"], 1, 1e-9)
    near("step 1 evenly spaced on each pod", one["ca2_by_pod"]["pod-1"], 0, 1e-3)
    ok("step 2 says what stopped it", two["stopped_by"] == lib.RULE_INFLIGHT and two["truncated"])
    near("step 2 window is the 150 s to the stop, from its first row", two["window_s"], 150)
    near("step 2 achieved is over that window", two["achieved"], 120 / 150, 1e-9)
    near("step 2 reads its own level's rows", two["due"], 120)
    ok("step 2 failures count as infinitely slow", two["client_ttft_p95"] == math.inf and two["failed"] == 12, two["failed"])

with tempfile.TemporaryDirectory() as run:
    ROW = dict(level=1, rate=1.0, achieved=1.0, ca2_sent=1.0, ca2_asked=1, window_s=420, truncated=False, stopped_by="",
               failed=0, due=420, lag_p99=0.01, client_ttft_p90=0.1, client_ttft_p95=0.1, client_tpot_mean=0.01,
               client_tpot_p90=0.01, client_tpot_p95=0.01, fc_saturation={}, fc_queue={}, completion_rate=1.0)
    for name in ("B-off-r1", "B-off-r2"):
        os.makedirs(os.path.join(run, "phases", name))
        with open(os.path.join(run, "phases", name, "analysis.jsonl"), "w", encoding="utf8") as f:
            f.write(json.dumps(ROW) + "\n")
    open(os.path.join(run, "phases", "B-off-r1", "done"), "w", encoding="utf8").close()
    FINISHED, CRASHED = analyze.phases_of(run)
    ok("a phase with no done file crashed", CRASHED == ["B-off-r2"] and FINISHED["B", "off"] == {1: "B-off-r1"}, CRASHED)
    BUCKET = dict(name="B", prompt=8000, output=200, ca2=1, prefill=2, decode=2, vp=288288.0, imbalance=1.19, decodeKnee=44.0, knee=44.0,
                  operatingRate=37.0, meanTPOT=0.02, onsets=dict(Prefill=54.6), curve=[], flags=["RouterHopAssumed"], assumed=["router hop"],
                  targets=[dict(percentile=90, seconds=0.2, predictedOff=37.0, predictedOn=37.0)])
    TEXT = io.StringIO()
    GOT = analyze.report(run, dict(buckets=[BUCKET]), dict(promptCV=0.0577, repeats=2, buckets=[dict(name="B", fc=["off"], gate=True)]), None, TEXT)
    ok("its repeat is missing, so no pass", "repeat 2 missing" in GOT["B", "off", "ttft", 90], GOT)
    ok("the report names it", "B-off-r2" in TEXT.getvalue())
    ok("the report carries the frozen flags", "RouterHopAssumed" in TEXT.getvalue())

    open(os.path.join(run, "phases", "B-off-r2", "done"), "w", encoding="utf8").close()
    GATE_SPEC = dict(promptCV=0.0577, repeats=1, buckets=[dict(name="B", fc=["off"], gate=True)])
    FROZEN_ONE = dict(buckets=[BUCKET])
    ok("a good step passes the gate", analyze.gate(run, FROZEN_ONE, GATE_SPEC, 1, io.StringIO()) == 0)
    with open(os.path.join(run, "phases", "B-off-r1", "analysis.jsonl"), "w", encoding="utf8") as f:
        f.write(json.dumps(dict(ROW, lag_p99=1.0)) + "\n")
    GATE_OUT = io.StringIO()
    ok("an all-void bucket stops the gate", analyze.gate(run, FROZEN_ONE, GATE_SPEC, 1, GATE_OUT) == 1)
    ok("and says why", lib.NO_VALID_CROSSING in GATE_OUT.getvalue(), GATE_OUT.getvalue())
    # A crossing on a void step is a bound, bracketed by 35 and 40 r/s. 37 predicted is
    # 0.93 to 1.06 of it, wholly in the mark [0.75, 1.10]: the verdict is the same
    # wherever in the bracket the crossing sits, so it passes.
    # A top step void by send lag reads high: it closes no bracket, and the gate stops. One
    # short of its schedule (a load that died) reads low if anything, and closes it.
    TWO = [dict(ROW, rate=35.0, achieved=35.0), dict(ROW, level=2, rate=40.0, achieved=40.0, client_ttft_p90=0.5, lag_p99=1.0)]
    with open(os.path.join(run, "phases", "B-off-r1", "analysis.jsonl"), "w", encoding="utf8") as f:
        f.writelines(json.dumps(r) + "\n" for r in TWO)
    GATE_OUT = io.StringIO()
    ok("a bound open above, its top step void by send lag, stops the gate",
       analyze.gate(run, FROZEN_ONE, GATE_SPEC, 1, GATE_OUT) == 1 and "no valid crossing" in GATE_OUT.getvalue(), GATE_OUT.getvalue())
    TWO = [dict(ROW, rate=35.0, achieved=35.0), dict(ROW, level=2, rate=40.0, achieved=40.0, client_ttft_p90=0.3, scheduled=1000, recorded=800)]
    with open(os.path.join(run, "phases", "B-off-r1", "analysis.jsonl"), "w", encoding="utf8") as f:
        f.writelines(json.dumps(r) + "\n" for r in TWO)
    GATE_OUT = io.StringIO()
    ok("a bound wholly in the mark passes the gate", analyze.gate(run, FROZEN_ONE, GATE_SPEC, 1, GATE_OUT) == 0
       and "1 bounds wholly in the mark, passed" in GATE_OUT.getvalue(), GATE_OUT.getvalue())

    def predicting(rate):
        return dict(buckets=[dict(BUCKET, targets=[dict(BUCKET["targets"][0], predictedOff=rate, predictedOn=rate)])])
    # 41 predicted is 1.03 to 1.17: across the mark's top edge.
    GATE_OUT = io.StringIO()
    ok("a bound across the mark's edge stops the gate", analyze.gate(run, predicting(41.0), GATE_SPEC, 1, GATE_OUT) == 1
       and lib.NO_VALID_CROSSING in GATE_OUT.getvalue(), GATE_OUT.getvalue())
    # 50 predicted is 1.25 to 1.43: wholly outside.
    GATE_OUT = io.StringIO()
    ok("a bound outside the mark stops the gate", analyze.gate(run, predicting(50.0), GATE_SPEC, 1, GATE_OUT) == 1
       and "FAIL, a bound" in GATE_OUT.getvalue(), GATE_OUT.getvalue())
    # The rules written after repeat 1 was seen are named in every gate and report, with
    # the verdicts from before them that a resume kept.
    os.makedirs(os.path.join(run, "prior-1"))
    with open(os.path.join(run, "prior-1", "gate-r1.out"), "w", encoding="utf8") as f:
        f.write("gate repeat 1: 16 readings, 7 stop the run\n")
    GATE_OUT = io.StringIO()
    analyze.gate(run, FROZEN_ONE, GATE_SPEC, 1, GATE_OUT)
    with open(os.path.join(run, "gate-r1.md"), encoding="utf8") as f:
        GATE_MD = f.read()
    ok("the gate and its report name both post-hoc rules, their date, and the verdict kept from before them",
       all(analyze.POST_HOC in t and "prior-1/gate-r1.out" in t for t in (GATE_OUT.getvalue(), GATE_MD))
       and "2026-11-10" in analyze.POST_HOC, GATE_OUT.getvalue())
    os.remove(os.path.join(run, "prior-1", "gate-r1.out"))
    os.rmdir(os.path.join(run, "prior-1"))

    # The cliff at the gate: r1's C-onset shape, scaled to this bucket's 0.2 s target and 37
    # r/s. Its void top step made a bound, and the bound stopped the gate.
    CLIFF = [dict(ROW, rate=35.0, achieved=35.0, client_ttft_p90=0.12),
             dict(ROW, level=2, rate=40.0, achieved=40.0, client_ttft_p90=6.5, failed=500, due=16800)]
    with open(os.path.join(run, "phases", "B-off-r1", "analysis.jsonl"), "w", encoding="utf8") as f:
        f.writelines(json.dumps(r) + "\n" for r in CLIFF)
    GATE_OUT = io.StringIO()
    ok("a crossing on the cliff, its bracket in the mark, does not stop the gate",
       analyze.gate(run, FROZEN_ONE, GATE_SPEC, 1, GATE_OUT) == 0, GATE_OUT.getvalue())
    ok("and is said to be on the cliff, not scored", "on the cliff and in the mark, not scored" in GATE_OUT.getvalue())
    FROZEN_LOW = dict(buckets=[dict(BUCKET, targets=[dict(BUCKET["targets"][0], predictedOff=28.0, predictedOn=28.0)])])
    ok("one whose bracket crosses the mark stops it", analyze.gate(run, FROZEN_LOW, GATE_SPEC, 1, io.StringIO()) == 1)

    # The router is a mean cross-check: its quantile, from wide buckets, crosses at the
    # first step here (0.5 s against 0.2), and is not scored.
    ROUTED = [dict(ROW, rate=35.0, achieved=35.0, client_ttft_mean=0.1, router_ttft_mean=0.1012, router_ttft_p90=0.5,
                   router_tpot_p90=1.0, client_ttft_p90=0.12)]
    with open(os.path.join(run, "phases", "B-off-r1", "analysis.jsonl"), "w", encoding="utf8") as f:
        f.writelines(json.dumps(r) + "\n" for r in ROUTED)
    TEXT = io.StringIO()
    analyze.report(run, FROZEN_ONE, GATE_SPEC, None, TEXT, repeats=[1], only_gate=True)
    TTFT_ROW = [line for line in TEXT.getvalue().splitlines() if line.startswith("| B | off | p90 |")]
    ok("the router's quantile crossing is not in the report", TTFT_ROW and "below the ladder" not in TTFT_ROW[0], TTFT_ROW)
    ok("its mean is, against the client's", TTFT_ROW and "+1.2 to +1.2 over 1 steps" in TTFT_ROW[0], TTFT_ROW)

# The EPP's /metrics wants a bearer token. The API server's pod proxy strips the caller's,
# so the old read came with none, and the EPP said 401; the witness scrape sends the
# controller's.


class EPPMetrics(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        good = self.headers.get("Authorization") == "Bearer s3cret"
        body = b"llm_d_epp_request_ttft_seconds_count 3\n" if good else b"Unauthorized"
        self.send_response(200 if good else 401)
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


SERVER = http.server.HTTPServer(("127.0.0.1", 0), EPPMetrics)
threading.Thread(target=SERVER.serve_forever, daemon=True).start()
EPP_URL = "http://127.0.0.1:%d/metrics" % SERVER.server_address[1]
try:
    urllib.request.urlopen(EPP_URL, timeout=10)
    PROXIED = 200
except urllib.error.HTTPError as e:
    PROXIED = e.code
ok("a read with no token, as the API proxy makes it, is refused", PROXIED == 401, PROXIED)
with tempfile.TemporaryDirectory() as td:
    for name, token in (("good", "s3cret\n"), ("bad", "other\n")):
        with open(os.path.join(td, name), "w", encoding="utf8") as f:
            f.write(token)
    PROMQ = os.path.join(HERE, "staircase_promq.py")
    GOT = subprocess.run([sys.executable, PROMQ, "scrape", EPP_URL, os.path.join(td, "good")], capture_output=True, text=True, timeout=60)
    ok("the witness scrape, with the token, reads the EPP", GOT.returncode == 0 and "llm_d_epp_request_ttft_seconds_count" in GOT.stdout,
       (GOT.returncode, GOT.stdout[:100], GOT.stderr[-200:]))
    GOT = subprocess.run([sys.executable, PROMQ, "scrape", EPP_URL, os.path.join(td, "bad")], capture_output=True, text=True, timeout=60)
    ok("a token the EPP refuses fails the scrape, with the status", GOT.returncode != 0 and "answered 401" in GOT.stderr,
       (GOT.returncode, GOT.stderr[-200:]))
SERVER.shutdown()

# --- above the prefill flow-control onset: measured, never scored ------------------------
# r1's B-onset at 2 + 4: prefill onset 53.21, decode onset 64.62, curve up to the prefill
# knee 55.87. The spec asks for 1.10x and 1.15x the onset.
B_ONSET = dict(name="B-onset", prompt=8000, output=200, ca2=1, prefill=2, decode=4, vp=289923.64, imbalance=1.232429351816981,
               prefillKnee=55.87084111432735, decodeKnee=66.58668456531164, knee=55.87084111432735,
               onsets=dict(Prefill=53.21300019557984, Decode=64.61631586342887),
               curve=[dict(rate=s * 55.87084111432735) for s in (0.70, 0.85, 0.92, 0.96, 0.97, 1.00)])
MEASURE_SPEC = dict(LADDER_SPEC, buckets=[dict(name="B-onset", fc=["on"], measureAboveOnset=[1.10, 1.15])])
LAST = freeze.ladder_steps(B_ONSET, MEASURE_SPEC)[-2:]
ok("B-onset's ladder ends with 1.10x and 1.15x its prefill onset, held as its other steps",
   [round(r, 3) for r, _, _ in LAST] == [58.534, 61.195] and all(h == 1500 for _, h, _ in LAST), LAST)
ok("and the freeze accepts them", not freeze.measure_refusals(dict(buckets=[B_ONSET]), MEASURE_SPEC),
   freeze.measure_refusals(dict(buckets=[B_ONSET]), MEASURE_SPEC))
B_22 = dict(B_ONSET, name="B", decode=2, onsets=dict(Prefill=53.21300019557984, Decode=32.30815793171443))
ok("B at 2 + 2 has none to give: its prefill onset is past its decode onset", any("decode onset" in r for r in freeze.measure_refusals(
    dict(buckets=[B_22]), dict(MEASURE_SPEC, buckets=[dict(name="B", fc=["on"], measureAboveOnset=[1.05])]))))
ok("measurement steps with flow control off as well are refused", any("flow control on alone" in r for r in freeze.measure_refusals(
    dict(buckets=[B_ONSET]), dict(MEASURE_SPEC, buckets=[dict(name="B-onset", fc=["off", "on"], measureAboveOnset=[1.10])]))))
ok("measurement steps under a scored step are refused", any("above every scored step" in r for r in freeze.measure_refusals(
    dict(buckets=[dict(B_ONSET, curve=[dict(rate=60.0)])]), MEASURE_SPEC)))

with tempfile.TemporaryDirectory() as run:
    d = os.path.join(run, "phases", "B-onset-on-r1")
    os.makedirs(d)
    open(os.path.join(d, "done"), "w", encoding="utf8").close()
    ROW_ON = dict(ROW, client_ttft_p50=0.1, fc_saturation={"pool prefill": [0.05, 0.13]}, fc_queue={"all": [0, 0]}, need_s=1.0)
    # Scored steps meet the 0.4 s target to the top; the measure step runs away.
    STEPS = [dict(ROW_ON, level=1, rate=51.401, achieved=51.401, client_ttft_p90=0.30),
             dict(ROW_ON, level=2, rate=55.871, achieved=55.871, client_ttft_p90=0.35),
             dict(ROW_ON, level=3, rate=58.534, achieved=58.534, client_ttft_p50=0.052, client_ttft_p90=6.0, fc_waited_share=0.52, fc_wait_mean=2.5)]
    with open(os.path.join(d, "analysis.jsonl"), "w", encoding="utf8") as f:
        f.writelines(json.dumps(r) + "\n" for r in STEPS)
    FROZEN_ON = dict(buckets=[dict(B_ONSET, operatingRate=47.49, meanTPOT=0.0074,
                                   targets=[dict(percentile=90, seconds=0.4, predictedOff=52.0, predictedOn=52.0)])])
    SPEC_ON = dict(promptCV=0.0577, repeats=1, buckets=[dict(name="B-onset", fc=["on"], gate=True, measureAboveOnset=[1.10, 1.15])])
    TEXT = io.StringIO()
    GOT = analyze.report(run, FROZEN_ON, SPEC_ON, None, TEXT, repeats=[1], only_gate=True)
    ok("a measure-only step above the onset is not scored: the crossing stays at the top valid scored step",
       GOT["B-onset", "on", "ttft", 90] == "no mark" and "at least 51.4" in TEXT.getvalue(), (GOT, TEXT.getvalue()[-300:]))
    ok("and the report shows its queue wait", "| B-onset | 1 | 58.5 | 1.10x | measure only | 52.0 / 6000.0 | 52.0% | 2500.0 |"
       in TEXT.getvalue())

# The report reads the client's crossing at the rate each step sent: 0.4 s between 41.0
# (asked 40) and 52.0 (asked 50) is 46.5 sent; at the rates asked it would be 45.0.
with tempfile.TemporaryDirectory() as run:
    d = os.path.join(run, "phases", "B-onset-on-r1")
    os.makedirs(d)
    open(os.path.join(d, "done"), "w", encoding="utf8").close()
    with open(os.path.join(d, "analysis.jsonl"), "w", encoding="utf8") as f:
        f.writelines(json.dumps(r) + "\n" for r in (
            dict(ROW_ON, level=1, rate=40.0, achieved=41.0, client_ttft_p90=0.30, scheduled=17220, recorded=17220),
            dict(ROW_ON, level=2, rate=50.0, achieved=52.0, client_ttft_p90=0.50, scheduled=21840, recorded=21840)))
    TEXT = io.StringIO()
    analyze.report(run, FROZEN_ON, SPEC_ON, None, TEXT, repeats=[1], only_gate=True)
    LINE = [line for line in TEXT.getvalue().splitlines() if line.startswith("| B-onset | on | p90 |")]
    ok("the report's TTFT crossing is read at the rates the steps sent", LINE and "| 46.5 |" in LINE[0], LINE)

# The quantile band at the burstiness sent: bunches of four 10 ms apart, a second between
# bunches, so Ca2 well above 1, widen the Poisson band by sqrt(Ca2); an even stream does not.
def stream(gaps):
    t, rows = 0.0, []
    for i, g in enumerate(gaps):
        t += g
        rows.append([str(i), "1", "%.4f" % t, "%.4f" % t, "%.4f" % (t + 0.1 + 0.001 * (i % 97)), "%.4f" % (t + 1), "200", "100", "10", "", ""])
    return rows


BURSTY = stream([0.01, 0.01, 0.01, 1.0] * 100)
FIG = analyze.client_figures(BURSTY, 120.0)
TTFTS = sorted(float(r[4]) - float(r[2]) for r in BURSTY)
ok("a bursty step's quantile band is widened by sqrt(Ca2 sent)", FIG["ca2_sent"] > 1
   and FIG["client_ttft_p90_lo"] == lib.pct_ci(TTFTS, 0.9, 1.96 * math.sqrt(FIG["ca2_sent"]))[0]
   and FIG["client_ttft_p90_lo"] < lib.pct_ci(TTFTS, 0.9)[0], (FIG["ca2_sent"], FIG["client_ttft_p90_lo"], lib.pct_ci(TTFTS, 0.9)))
EVEN = stream([0.25] * 400)
FIG = analyze.client_figures(EVEN, 100.0)
ok("an even stream keeps the Poisson band", FIG["client_ttft_p90_lo"] == lib.pct_ci(sorted(float(r[4]) - float(r[2]) for r in EVEN), 0.9)[0])

# The queue wait from the raw scrape: 100 requests in the window, half over 10 ms, 10 s in all.
with tempfile.TemporaryDirectory() as run:
    base = lib.ts("2026-11-06T02:00:00Z")
    q = analyze.QUEUE
    with open(os.path.join(run, "epp-metrics.log"), "w", encoding="utf8") as f:
        for t, under, total, s in ((base + 170, 100, 100, 0.1), (base + 599, 150, 200, 10.1)):
            stamp = lib.datetime.fromtimestamp(t, lib.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
            f.write('%s epp %s_bucket{le="0.01"} %d\n%s epp %s_bucket{le="+Inf"} %d\n%s epp %s_sum %g\n%s epp %s_count %d\n' % (
                stamp, q, under, stamp, q, total, stamp, q, s, stamp, q, total))
    ROWQ = {}
    analyze.queue_figures(ROWQ, analyze.read_raw_epp(run + "/epp-metrics.log"), {}, base + 180, base + 600)
    ok("the queue wait from the raw scrape", ROWQ.get("fc_wait_n") == 100 and abs(ROWQ.get("fc_waited_share", 0) - 0.5) < 1e-9
       and abs(ROWQ.get("fc_wait_mean", 0) - 0.1) < 1e-9, ROWQ)

# The prefill engines' running cap from the per-pod scrape: a pod with two engines sums
# them per poll, a poll outside the window is left out, decode stays out of prefill.
with tempfile.TemporaryDirectory() as run:
    base = lib.ts("2026-11-06T02:00:00Z")

    def stamp(t):
        return lib.datetime.fromtimestamp(base + t, lib.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    with open(os.path.join(run, "vllm-metrics.log"), "w", encoding="utf8") as f:
        f.write("%s m-prefill-a vllm:num_requests_running{engine=\"0\"} 99\n" % stamp(100))
        f.write("%s m-prefill-a vllm:num_requests_running{engine=\"0\"} 3\n" % stamp(200))
        f.write("%s m-prefill-a vllm:num_requests_running{engine=\"1\"} 2\n" % stamp(200))
        f.write("%s m-prefill-a vllm:time_to_first_token_seconds_sum{engine=\"0\"} 1.5\n" % stamp(200))
        f.write("%s m-prefill-a vllm:num_requests_running{engine=\"0\"} 7\n" % stamp(400))
        f.write("%s m-prefill-b vllm:num_requests_running{engine=\"0\"} 4\n" % stamp(200))
        f.write("%s m-prefill-b vllm:num_requests_waiting{engine=\"0\"} 1\n" % stamp(200))
        f.write("%s m-prefill-b vllm:num_requests_running{engine=\"0\"} 6\n" % stamp(400))
        f.write("%s m-prefill-b vllm:num_requests_waiting{engine=\"0\"} 0\n" % stamp(400))
        f.write("%s m-decode-a vllm:num_requests_running{engine=\"0\"} 50\n" % stamp(200))
    ENGINE = analyze.read_engine(run + "/vllm-metrics.log")
    RUNNING = analyze.gauge_figures(ENGINE["prefill"], "running", base + 180, base + 600)
    WAITING = analyze.gauge_figures(ENGINE["prefill"], "waiting", base + 180, base + 600)
    ok("prefill running per pod: engines summed per poll, the poll before the window left out",
       RUNNING == {"m-prefill-a": [6.0, 7.0], "m-prefill-b": [5.0, 6.0]}, RUNNING)
    ok("prefill waiting per pod, and a pod that reported none is absent", WAITING == {"m-prefill-b": [0.5, 1.0]}, WAITING)
    ok("the TTFT counters still read beside the gauges, and decode stays out of prefill",
       ENGINE["prefill"]["m-prefill-a"]["sum"] == [(base + 200, 1.5)] and "m-decode-a" in ENGINE["decode"]
       and "m-decode-a" not in ENGINE["prefill"], ENGINE["prefill"]["m-prefill-a"]["sum"])
    SCORED = dict(level=1, rate=51.401)
    MEASURED = dict(level=2, rate=58.534, prefill_running=RUNNING, prefill_waiting=WAITING)
    TEXT = io.StringIO()
    analyze.prefill_running_cap({("B-onset", "on"): {1: [SCORED]}, ("B", "off"): {1: [SCORED]}},
                                {("B-onset", "on", 1): [MEASURED]}, {"B-onset": B_ONSET}, TEXT)
    ok("the report's running-cap table: one per flow-control-on phase, the measure-only step with its max across pods",
       "### B-onset on r1" in TEXT.getvalue() and "### B off" not in TEXT.getvalue()
       and "| 2 | 58.5 | 1.10x | measure only | 6.0 / 7 5.0 / 6 | 7 | 0.5 / 1 | unread |" in TEXT.getvalue()
       and "| 1 | 51.4 | 0.97x | scored | unread | unread | unread | unread |" in TEXT.getvalue(), TEXT.getvalue())
    ok("the table calls the max a sampled lower bound, and names max_num_seqs when the run saved it",
       "sampled lower bound" in TEXT.getvalue() and "vLLM's default applies" in TEXT.getvalue())
    near("the poll interval, from the polls themselves", analyze.poll_interval(ENGINE["prefill"], base, base + 600), 200)
    with open(os.path.join(run, "prefill-args.txt"), "w", encoding="utf8") as f:
        f.write("vllm serve m --port 8000 --max-num-seqs 64 --enforce-eager\n")
    ok("max_num_seqs read from the prefill args", analyze.read_max_num_seqs(run) == 64, analyze.read_max_num_seqs(run))
    with open(os.path.join(run, "epp-metrics.log"), "w", encoding="utf8") as f:
        f.write("%s epp-a %s_count 10\n%s epp-b %s_count 99\n" % (stamp(200), analyze.TTFT, stamp(200), analyze.TTFT))
    RAW = analyze.read_raw_epp(run + "/epp-metrics.log")
    ok("the raw scrape keeps one EPP pod a poll", RAW[analyze.TTFT + "_count"][base + 200][""] == 10.0, dict(RAW))

# The seeded schedule end to end: a phase whose second level lost its load halfway.
with tempfile.TemporaryDirectory() as run:
    base = lib.ts("2026-11-06T02:00:00Z")
    d = os.path.join(run, "phases", "S-off-r1")
    os.makedirs(d)
    SCHED = "600:2:100:10:1,600:4:100:10:1"
    PLAN = load.arrivals(load.parse_schedule(SCHED), 7)

    def stamp(t):
        return lib.datetime.fromtimestamp(base + t, lib.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    with open(os.path.join(d, "sweep.log"), "w", encoding="utf8") as f:
        f.write("%s 2 levels, 1200 s, %d requests, seed 7: %s\n" % (stamp(0), len(PLAN), SCHED))
        f.write("%s LEVEL 1 rate=2 in=100 out=10 ca2=1 hold=600s\n%s LEVEL 2 rate=4 in=100 out=10 ca2=1 hold=600s\n" % (stamp(1), stamp(601)))
    with gzip.open(os.path.join(d, "requests.csv.gz"), "wt", encoding="utf8") as f:
        f.write(load.HEADER)
        for i, (due, n, _, _) in enumerate(PLAN):
            if n == 2 and due > 900:
                continue
            t = base + 5 + due
            f.write("%d,%d,%.4f,%.4f,%.4f,%.4f,200,100,10,,\n" % (i, n, t, t, t + 0.1, t + 0.5))
    STEPS_S, _ = analyze.analyze_steps(run, "S-off-r1", 180)
    FIRST = STEPS_S[0] if STEPS_S else {}
    ok("the schedule is read from the driver's log: a whole step meets it, and its rate is the one it sent",
       FIRST.get("scheduled") == FIRST.get("recorded") and FIRST.get("scheduled", 0) > 600
       and abs(FIRST["achieved"] - FIRST["scheduled"] / FIRST["window_s"]) < 1e-9 and not lib.invalid(FIRST),
       {k: FIRST.get(k) for k in ("scheduled", "recorded", "achieved", "window_s")})
    SECOND = STEPS_S[1] if len(STEPS_S) > 1 else {}
    ok("a step whose load died is void by its missing rows", any("scheduled" in w for w in lib.validity(SECOND))
       if SECOND else False, {k: SECOND.get(k) for k in ("scheduled", "recorded")})
    with open(os.path.join(d, "sweep.log"), encoding="utf8") as f:
        LOG_LINES = f.readlines()
    ok("the plan line is found below the log's head", analyze.read_plan(["noise\n"] * 8 + LOG_LINES, analyze.read_requests(
        os.path.join(d, "requests.csv.gz")))[0] is not None)
    with open(os.path.join(d, "sweep.log"), "w", encoding="utf8") as f:
        f.writelines(LOG_LINES[1:])
    UNREAD, _ = analyze.analyze_steps(run, "S-off-r1", 180)
    ok("a step read without its schedule is marked, and keeps the band", UNREAD and all(r.get("schedule_unread") and "scheduled" not in r
                                                                         for r in UNREAD), [sorted(r) for r in UNREAD[:1]])


# --- the ITL-slope falsifier is judged at the operating point only ----------------------
# r2's repeat 1: the operating-point phases read 6.18 to 8.23, the onset phases 12.78 and
# 19.28. The knees are r2's frozen ones: B's lower knee is decode's, B-onset's prefill's.
def slope_rows(slope):
    return [dict(rate=10.0 + i, truncated=False, failed=0, client_tpot_mean=0.01 * (i + 1),
                 engine_decode_mean=slope * 0.01 * (i + 1), hop_mean=0.001) for i in range(4)]


SLOPE_FROZEN = {"B": dict(knee=35.63, prefillKnee=58.97, decodeKnee=35.63, operatingRate=1.0, prompt=8000),
                "B-onset": dict(knee=58.97, prefillKnee=58.97, decodeKnee=71.26, operatingRate=1.0, prompt=8000)}
SLOPE_SPEC = dict(falsifiers=dict(itlSlope=[5.5, 9.0]))
TEXT = io.StringIO()
GOT = analyze.falsifiers({("B", "on"): {1: slope_rows(7.2)}, ("B-onset", "on"): {1: slope_rows(12.78)}},
                         SLOPE_FROZEN, SLOPE_SPEC, TEXT)
ok("an onset bucket's ITL slope is printed as reported, not judged",
   "| ITL slope, B-onset on | 12.78 | 5.5 to 9 | reported, onset phase |" in TEXT.getvalue(), TEXT.getvalue())
ok("and counts toward nothing, while the operating-point one is judged", GOT == [True], GOT)
# The ladder of every bucket is in shares of its lower knee (the spec's steps), so the
# knee comparison is how the ladder was built, not a guess at it: a bucket of any name
# whose prefill knee is the lower one is on an onset ladder.
ok("a bucket with no prefill knee frozen is judged", not analyze.onset_ladder(dict(knee=35.6, decodeKnee=35.6)))
ok("a bucket of any name whose prefill knee is the lower one is on an onset ladder",
   analyze.onset_ladder(dict(name="H-16k", knee=29.48, prefillKnee=29.48, decodeKnee=30.0)))
ok("an operating-point slope out of band is still judged, and fails",
   analyze.falsifiers({("B", "on"): {1: slope_rows(12.78)}}, SLOPE_FROZEN, SLOPE_SPEC, io.StringIO()) == [False])

print("%d checks, %d failed" % (checks, fails))
if checks != EXPECTED:
    print("FAIL: ran %d checks, expected %d" % (checks, EXPECTED))
    fails += 1

# --- the ladder, the plan, the config edits -- reported through the same exit -----------
FROZEN = dict(specSha256="x", gate=dict(vp=True, a=True, decodeLine=True, sloListed=True), buckets=[
    dict(name="B", prompt=8000, output=200, ca2=1, prefill=2, decode=2, vp=288288.0, imbalance=1.193, decodeKnee=43.953, knee=43.953,
         onsets=dict(Prefill=54.6), curve=[dict(rate=17.6), dict(rate=41.3), dict(rate=42.0), dict(rate=46.0)],
         targets=[dict(percentile=90, seconds=0.2, predictedOff=37.0, predictedOn=37.0)])])
SPEC = dict(promptCV=0.0577, discardSeconds=180, holdSeconds=600, maxHoldSeconds=1500, runawayFactor=2, repeats=3, seedBase=3000,
            phaseOverheadSeconds=240, buckets=[dict(name="B", fc=["off", "on"], gate=True)])
steps = freeze.ladder_steps(FROZEN["buckets"][0], SPEC)
extra = []
if steps[0][1] != 600 or steps[0][2]:
    extra.append(("light step keeps its hold", steps[0]))
if steps[1][1] != 600 or steps[1][2]:
    extra.append(("M2's 41.3: 4T 392 s fits the 420 s window", steps[1]))
if not 900 <= steps[2][1] <= 910 or steps[2][2]:
    extra.append(("a step near the knee is lengthened to 180 s and 4T", steps[2]))
if steps[3][1] != 1500 or not steps[3][2]:
    extra.append(("past the knee: the cap, flagged", steps[3]))
names = [p["name"] for p in freeze.phases(FROZEN, SPEC)]
if names != ["B-off-r1", "B-on-r1", "B-off-r2", "B-on-r2", "B-off-r3", "B-on-r3"]:
    extra.append(("phase order", names))
HELD = json.loads(json.dumps(FROZEN))
HELD["buckets"].insert(0, dict(HELD["buckets"][0], name="H"))
HELD_SPEC = dict(SPEC, repeats=1, buckets=SPEC["buckets"] + [dict(name="H", fc=["off"])])
if [p["name"] for p in freeze.phases(HELD, HELD_SPEC)] != ["B-off-r1", "B-on-r1", "H-off-r1"]:
    extra.append(("gate buckets go first in repeat 1", ""))
seeds = [p["seed"] for p in freeze.phases(FROZEN, SPEC)]
if len(set(seeds)) != len(seeds):
    extra.append(("seeds repeat", seeds))
if freeze.refusals(FROZEN, "x"):
    extra.append(("a good file refused", freeze.refusals(FROZEN, "x")))
if not freeze.refusals(FROZEN, "other"):
    extra.append(("another spec's file accepted", ""))
NO_SLO = json.loads(json.dumps(FROZEN))
NO_SLO["gate"]["sloListed"] = False
if not any("sloListed" in r for r in freeze.refusals(NO_SLO, "x")):
    extra.append(("a warm-up without slo accepted", ""))
out = io.StringIO()
rows = freeze.plan(FROZEN, SPEC, out)
if rows["gate"][0] != 6:
    extra.append(("plan phases", rows))
if abs(rows["gate"][2] - rows["gate"][1] * 4) > 1e-9:
    extra.append(("GPU-hours are hours times pods", rows))
with tempfile.TemporaryDirectory() as td:
    path = os.path.join(td, "ladder.txt")
    freeze.write_ladder(FROZEN, SPEC, path)
    with open(path, encoding="utf8") as f:
        LINES = f.read().splitlines()
    first = LINES[0].split()
    if LINES[2] != "gate-check 1" or len(LINES) != 7:
        extra.append(("the gate check follows repeat 1 of the gate buckets", LINES))
    if first[0] != "B-off-r1" or first[3] != "0.4000" or first[5:] != ["2", "2"]:
        extra.append(("ladder line", first))
    try:
        freeze.write_ladder(FROZEN, SPEC, path)
        extra.append(("a ladder was replaced", ""))
    except FileExistsError:
        pass

PD = "apiVersion: x\nfeatureGates:\n- flowControl\nplugins:\n- type: a\nflowControl:\n  maxBytes: 1\n\nschedulingProfiles:\n- name: b\n"
GONE = config.drop_flow_control(PD)
if "flowControl" in GONE or "plugins:" not in GONE or "schedulingProfiles:" not in GONE:
    extra.append(("fc-off", GONE))
POLICY = "analyzers:\n  - name: saturation\n    score: 1\nother: 1\n"
LISTED = config.list_slo(POLICY, "200ms")
if "  - name: slo\nother: 1" not in LISTED or "ttft: {target: 200ms, percentile: 95}" not in LISTED:
    extra.append(("list-slo", LISTED))
for bad, fn in ((PD.replace("- flowControl", "- other"), config.drop_flow_control), (LISTED, lambda t: config.list_slo(t, "1s")),
                ("nothing\n", lambda t: config.list_slo(t, "1s"))):
    try:
        fn(bad)
        extra.append(("a surprise was edited", bad[:40]))
    except SystemExit:
        pass

def raises():
    raise OSError("down")


W = deadline.Watch(1000, 10 ** 9)
if W.armed or W.until != 1000 + deadline.GRACE:
    extra.append(("a new Job starts from the grace, never the hard cap", W.until))
if W.look(1001, raises) or W.armed:
    extra.append(("a failed read does not arm", ""))
W.until = 10 ** 8  # the bound alone, not the grace
W.look(1500, raises)
if W.failing != 1001:
    extra.append(("the bound runs from the first failure", W.failing))
if W.lapsed(1001 + deadline.READ_BOUND - 1):
    extra.append(("reads failing inside the bound keep the lease", ""))
if not W.lapsed(1001 + deadline.READ_BOUND):
    extra.append(("reads failing past the bound are a lapse, armed or not", ""))
if not W.look(1002, lambda: 5000) or not W.armed or W.look(1003, lambda: 5000):
    extra.append(("armed by the first read that works, once", ""))
if W.failing is not None or W.lapsed(4999) or not W.lapsed(5000):
    extra.append(("a read that works clears the failure, and the lease stands", W.failing))
W.look(6000, lambda: 9000)
W.look(6001, raises)
if W.until != 9000:
    extra.append(("a failed read keeps the last lease", W.until))
if not deadline.Watch(0, 50).lapsed(50):
    extra.append(("the hard cap binds the grace", ""))
CAPPED = deadline.Watch(0, 100)
CAPPED.look(1, lambda: 10 ** 6)
if not CAPPED.lapsed(100):
    extra.append(("a lease past the cap does not extend it", ""))
hit = []


def one_fails(path):
    hit.append(path)
    if "leaderworkersets" in path:
        raise OSError("no")
    return "ok"


if deadline.delete_all("n", one_fails) != 1 or len(hit) != len(deadline.PATHS):
    extra.append(("a failed delete must not stop the rest", hit))


def absent(path):
    if "leaderworkersets" in path:
        raise urllib.error.HTTPError(path, 404, "not found", {}, None)
    return "ok"


if deadline.delete_all("n", absent) != 0:
    extra.append(("a kind the API does not have passes", ""))
tries = []


def second_try(path):
    tries.append(path)
    if len(tries) <= len(deadline.PATHS):
        raise OSError("no")
    return "ok"


CLOCK = [0.0]
if deadline.free("n", second_try, lambda: CLOCK[0], lambda s_: CLOCK.__setitem__(0, CLOCK[0] + s_)) != 0 or len(tries) != 2 * len(deadline.PATHS):
    extra.append(("free retries until it works", len(tries)))
CLOCK[0] = 0.0
if deadline.free("n", one_fails, lambda: CLOCK[0], lambda s_: CLOCK.__setitem__(0, CLOCK[0] + s_)) != 1 or CLOCK[0] < deadline.DELETE_BOUND:
    extra.append(("free gives up at its bound, not before", CLOCK))

for name, got in extra:
    print("FAIL: %s %r" % (name, got))
fails += len(extra)
print("selftest %s" % ("failed" if fails else "ok"))
sys.exit(1 if fails else 0)
