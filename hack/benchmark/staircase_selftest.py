#!/usr/bin/env python3
"""staircase_selftest.py -- the staircase's arithmetic against figures worked by hand.

Run by hack/check-staircase.sh. A failed line prints what it read; the count is
asserted, so a section that stops running does not read as green.
"""
import asyncio
import gzip
import importlib.util
import io
import json
import math
import multiprocessing
import os
import random
import sys
import tempfile
import urllib.error

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

EXPECTED = 115
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
ok("an infinite step ends at the one before",
   lib.measured([step(10, 0.1), step(20, math.inf)], "client_ttft_p90", 0.2, bounds=False) == ("=", 10))
SHORT = [step(10, 0.1, 1), step(20, 0.2, 2), step(30, 0.4, 3, achieved=20.0)]
ok("a crossing on a short step is a bound", lib.measured(SHORT, "client_ttft_p90", 0.3) == ("bound", (20, 30)))
NOISY = [step(10, 0.1, 1), step(20, 0.2, 2, noise_flag=True), step(30, 0.4, 3)]
ok("a crossing on a noisy step is a bound", lib.measured(NOISY, "client_ttft_p90", 0.3) == ("bound", (20, 30)))
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
ok("a crossing on a cut step is a bound", lib.measured(CUTRUNG, "client_ttft_p90", 0.3) == ("bound", (20, 30)))

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
    # In the mark as a bound, 37 over 35 to 40: only the void step can stop it.
    TWO = [dict(ROW, rate=35.0, achieved=35.0), dict(ROW, level=2, rate=40.0, achieved=40.0, client_ttft_p90=0.5, lag_p99=1.0)]
    with open(os.path.join(run, "phases", "B-off-r1", "analysis.jsonl"), "w", encoding="utf8") as f:
        f.writelines(json.dumps(r) + "\n" for r in TWO)
    ok("a crossing on a void step stops the gate", analyze.gate(run, FROZEN_ONE, GATE_SPEC, 1, io.StringIO()) == 1)

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
