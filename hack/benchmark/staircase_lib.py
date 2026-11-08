"""staircase_lib.py -- the arithmetic of the TTFT staircase, with no I/O.

Imported by staircase_freeze.py and staircase_analyze.py, and exercised by
staircase_selftest.py (make lint-deploy-scripts). Standard library only: it
runs where the run's files are, which need not have numpy.

Rates are requests per second, times seconds. Docs: the plan
docs/plans/analyzers/ttft-staircase-repeat.md, and the specification in
docs/proposals/slo-capacity-planner-ttft.md, "Ground truth and the staircase".
"""
import math
import re
from datetime import datetime, timezone

# The mark, on predicted over measured. The page's: above MARK_HIGH the fleet is
# over-promised, below MARK_LOW under-promised.
MARK_HIGH = 1.10
MARK_LOW = 0.75
# A step holds when completions over its window come within this of its rate.
HOLD_TOLERANCE = 0.03
# How many standard deviations of a step's count of arrivals the rate it sent may sit
# from the rate it asked for. The count over a window of n expected arrivals has a
# variance of Ca^2 n, so a flat band trips by chance on bursty steps: r1's C, Ca^2 4 at
# 28.3 r/s over 420 s, has a standard deviation of 1.8%, and a flat 3% voided about one
# step in ten. At 3 a step is voided by chance one time in 370.
ACHIEVED_Z = 3.0
# A crossing between two steps whose readings differ by more than this factor is on the
# cliff: no interpolation says where between them the target falls. Twice the steepest
# rise between adjacent steps r1's frozen curves price below a knee, 1.96 in C-onset.
CLIFF_FACTOR = 4.0
# The verdict of a crossing on the cliff whose whole bracket is in the mark. The gate
# does not stop on it, and does not count it a pass.
CLIFF_IN = "on the cliff, in the mark"
# A step is void past any of these: the driver's send lag p99, seconds; the share of
# requests that failed; and how far Ca^2 sent may sit from Ca^2 asked. The page's 3 to
# 5 around 4 is 0.75 to 1.25 of it.
MAX_LAG_P99 = 0.1
MAX_FAILED_SHARE = 0.01
CA2_BAND = (0.75, 1.25)
# The verdict of a reading no valid step can place.
NO_VALID_CROSSING = "no valid crossing"
# The stop rules a phase can end on.
RULE_P90 = "client-p90"
RULE_INFLIGHT = "max-inflight"

LEVEL = re.compile(r"(\S+) LEVEL (\d+) rate=(\S+) in=(\d+) out=(\d+) ca2=(\S+) hold=(\d+)s")
RUNAWAY = re.compile(r"(\S+) RUNAWAY level=(\d+) (.*)")


def ts(text):
    """Epoch seconds of a UTC stamp, whole seconds."""
    return datetime.strptime(text, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc).timestamp()


def pct(values, q):
    """The q quantile of sorted values, nearest rank from below; NaN with none."""
    if not values:
        return math.nan
    return values[min(len(values) - 1, int(q * len(values)))]


def scv(values):
    """Squared coefficient of variation; NaN under two values or a zero mean."""
    if len(values) < 2:
        return math.nan
    mean = sum(values) / len(values)
    if mean == 0:
        return math.nan
    return sum((v - mean) ** 2 for v in values) / len(values) / mean ** 2


def arrival_ca2(times):
    """Ca^2 of a stream of send times."""
    ordered = sorted(times)
    return scv([y - x for x, y in zip(ordered, ordered[1:])])


def ca2_by_pod(pairs):
    """{pod: Ca^2} of (pod, send time) pairs. A row with no pod is not guessed at."""
    by = {}
    for pod, t in pairs:
        if pod:
            by.setdefault(pod, []).append(t)
    return {pod: arrival_ca2(times) for pod, times in sorted(by.items())}


def hist_quantile(cum, q):
    """(value, bucket low, bucket high) of sorted [(le, cumulative)], as histogram_quantile reads it.

    Coarse: linear inside a bucket, and the router's buckets are wide where the targets
    sit. r1's B at 13.3 r/s read p90 183.4 ms here against the client's 130.1. The
    report scores none of these; the router's mean, from _sum and _count, is exact.
    """
    total = cum[-1][1] if cum else 0
    if total <= 0:
        return math.nan, math.nan, math.nan
    rank, lo, below = q * total, 0.0, 0.0
    for le, count in cum:
        if count >= rank:
            if math.isinf(le):
                return math.inf, lo, math.inf
            if count == below:
                return le, lo, le
            return lo + (le - lo) * (rank - below) / (count - below), lo, le
        lo, below = le, count
    return math.inf, lo, math.inf


def relaxation(service, rho, ca2, cs2):
    """One relaxation time, E[S](Ca^2 + Cs^2)/(1 - sqrt(rho))^2; infinite at rho 1 or more."""
    if rho >= 1:
        return math.inf
    return service * (ca2 + cs2) / (1 - math.sqrt(rho)) ** 2


def noise_floor(rate, bucket, prompt_cv, ca2=None):
    """What a step's window must hold: four relaxation times of the slower role.

    Returns (seconds, prefill rho, decode rho). Prefill's service time is
    measured: I/V_P, and Cs^2 is the prompt's spread. Its rho is the rate over the
    frozen prefill knee where the file has one: the knee holds the margin and the
    rescale V_P alone does not, and at the knee rho must read 1. Decode has no measured
    service time for a request in its queue, so E[S] is N_d over the frozen
    knee and Cs^2 is taken as 1: an estimate, as the proposal says.
    """
    arrivals = bucket["ca2"] if ca2 is None else ca2
    service_p = bucket["prompt"] / bucket["vp"]
    rho_p = rate * service_p * max(bucket["imbalance"], 1) / bucket["prefill"]
    if bucket.get("prefillKnee", 0) > 0:
        rho_p = rate / bucket["prefillKnee"]
    need = 4 * relaxation(service_p, rho_p, arrivals, prompt_cv ** 2)
    rho_d = 0.0
    if bucket["decodeKnee"] > 0:
        rho_d = rate / bucket["decodeKnee"]
        need = max(need, 4 * relaxation(bucket["decode"] / bucket["decodeKnee"], rho_d, arrivals, 1.0))
    return need, rho_p, rho_d


def measure_rates(bucket, spec):
    """The bucket's measurement-only rates: its spec's measureAboveOnset, as multiples of
    the frozen prefill flow-control onset. Empty for a bucket that names none.

    Above the onset the router's prefill gate closes and requests wait in its queue,
    a wait the model flags (AboveFlowControlOnset) and does not price. These steps
    read that wait; they are never scored.
    """
    sent = next((b for b in spec.get("buckets", []) if b.get("name") == bucket["name"]), {})
    onset = (bucket.get("onsets") or {}).get("Prefill") or 0
    return [m * onset for m in sent.get("measureAboveOnset", [])] if onset > 0 else []


def is_measure(rate, rates):
    """Whether a step's rate is one of the measurement-only rates (to the ladder's 3 decimals)."""
    return any(abs(rate - m) <= 5e-4 + 1e-9 for m in rates)


def parse_log(lines):
    """The driver's log: (levels, stop). A stop is {t, level, rule, detail} or None.

    Reads both forms of the RUNAWAY line: "rule=..." since the driver names it,
    and the first run's, whose rule shows only in its field.
    """
    levels, stop = [], None
    for line in lines:
        m = LEVEL.match(line)
        if m:
            levels.append({"start": ts(m[1]), "n": int(m[2]), "rate": float(m[3]), "in": int(m[4]),
                           "out": int(m[5]), "ca2": float(m[6]), "hold": int(m[7])})
            continue
        m = RUNAWAY.match(line)
        if m and stop is None:
            detail = m[3].strip()
            named = re.match(r"rule=(\S+)\s*(.*)", detail)
            if named:
                rule, detail = named[1], named[2]
            elif detail.startswith("inflight="):
                rule = RULE_INFLIGHT
            elif detail.startswith("client-p90="):
                rule = RULE_P90
            else:
                rule = "unknown"
            stop = {"t": ts(m[1]), "level": int(m[2]), "rule": rule, "detail": detail}
    return levels, stop


def judged_window(level, stop, discard):
    """The (from, to) a step is judged over: its hold less the discard, cut at a stop inside it.

    The in-flight cap stops a step part-way, and a rate over the full window would
    then read a shortfall that was never sent. Empty, from == to, when the stop
    came first.
    """
    start, end = level["start"] + discard, level["start"] + level["hold"]
    if stop and stop["level"] == level["n"]:
        end = min(end, stop["t"])
    return start, max(start, end)


def achieved_tolerance(rate, window, ca2):
    """How far, as a share of rate, the rate sent over a window may sit from the rate asked.

    ACHIEVED_Z standard deviations of the count of arrivals, sqrt(Ca^2 n) for n expected,
    plus one arrival for a count that is whole, over n. A window with none expected
    holds no figure, and nothing sent is short of it.
    """
    n = rate * window
    if not n > 0:
        return math.inf
    return (ACHIEVED_Z * math.sqrt(max(ca2, 0.0) * n) + 1) / n


def pct_ci(values, q, z=1.96):
    """(low, high) of pct(values, q) by the order statistics' binomial band, z wide.

    The ranks q n -/+ z sqrt(n q (1 - q)), so the band a quantile of n independent
    readings would move in. Bursty arrivals correlate neighbours and widen it; this
    is the narrower, Poisson band. (None, None) for no values.
    """
    n = len(values)
    if not n:
        return None, None
    half = z * math.sqrt(n * q * (1 - q))
    lo = max(0, int(math.floor(q * n - half)) - 1)
    hi = min(n - 1, int(math.ceil(q * n + half)) - 1)
    return values[lo], values[max(lo, hi)]


# Failures a step's fleet makes: the engine's 5xx, the router's 429, a request that timed
# out or ran past the drain. A connection the client could not make, or a 4xx, is the
# harness's or the router's, and a step with one does not bracket a cliff.
FLEET_ERRORS = ("cancelled", "TimeoutError", "ServerTimeoutError", "stream ended without [DONE]",
                "error in stream", "no text in stream")


def fleet_failure(status, error):
    """Whether a failed request's row reads as the fleet's: status and error as the driver wrote them."""
    code = int(status) if str(status).isdigit() else 0
    return code >= 500 or code == 429 or (error or "") in FLEET_ERRORS


# A step's rows may fall short of its seeded schedule by this share, plus one row for a
# due time on the window's edge. A load process that died leaves more missing than that.
COMPLETE_SHARE = 0.001


def validity(row):
    """Why a step is void: a crossing on it is a bound, not a figure. Empty on a good step."""
    why = []
    if "scheduled" in row:
        # The seeded schedule fixed the rate sent, so the reading is credited to it; what
        # can go wrong is that rows are missing.
        short = row["scheduled"] - row["recorded"]
        if short > 1 + COMPLETE_SHARE * row["scheduled"]:
            why.append("recorded %d of %d scheduled" % (row["recorded"], row["scheduled"]))
    else:
        # No schedule to check against: the band around the rate asked. A row with no
        # window, made by hand, keeps the flat one.
        band = (achieved_tolerance(row["rate"], row["window_s"], row.get("ca2_asked", 1))
                if "window_s" in row else HOLD_TOLERANCE)
        if abs(row["achieved"] / row["rate"] - 1) > band:
            why.append("achieved %.2f of %.2f, past %.1f%%" % (row["achieved"], row["rate"], 100 * band))
    if row.get("noise_flag"):
        why.append("under the noise floor")
    if row.get("lag_p99", 0) > MAX_LAG_P99:
        why.append("send lag p99 %.3f s" % row["lag_p99"])
    if row.get("truncated") or row.get("stopped_by") == RULE_INFLIGHT:
        why.append("cut short by %s" % (row.get("stopped_by") or "a stop"))
    if row.get("failed", 0) > MAX_FAILED_SHARE * max(row.get("due", 0), 1):
        why.append("failed %d of %d" % (row["failed"], row["due"]))
    if row.get("replica_change"):
        why.append("replicas changed")
    if row.get("ca2_rerun"):
        why.append("Ca2 sent %.2f of %g asked: rerun" % (row["ca2_sent"], row["ca2_asked"]))
    return why


def invalid(row):
    """Whether a crossing on this step is a bound."""
    return bool(validity(row))


def reads_the_fleet(row):
    """Whether a step's reading is the fleet's own, void or not.

    A window short of the noise floor reads low if anything, failed requests are the
    fleet's, and a rate a few percent off moves it little. Send lag (the client times
    from the schedule), a cut, a fleet that moved and a Ca^2 burstier than asked can all
    raise a reading the fleet did not make, and such a step brackets no cliff. So can a
    failure that is not the fleet's (fleet_failure), a restarted EPP (two EPP pods read
    in the window) and an engine error in it.
    """
    return not (row.get("lag_p99", 0) > MAX_LAG_P99 or row.get("truncated") or row.get("stopped_by") == RULE_INFLIGHT
                or row.get("replica_change") or row.get("ca2_rerun") or row.get("failed_other", 0) > 0
                or len(row.get("epp_pods") or []) > 1 or row.get("engine_errors", 0) > 0)


def ca2_out_of_band(sent, asked):
    """Whether a bursty step's sent Ca^2 sits outside the band around what it asked for."""
    return asked != 1 and not asked * CA2_BAND[0] <= sent <= asked * CA2_BAND[1]


def envelope(rows, key, bounds=True):
    """Each step's reading, lowered to the lowest a later valid step read.

    On one fleet a quantile of TTFT does not fall as the rate rises, so a step that
    reads above a later, faster step read a transient, not its rate. r1's A-on step 2
    read p95 64.6 ms over a window whose one bad minute took p50 from 38 to 70.5 ms,
    and steps 3 to 5 read 56.4 to 61.5: the crossing landed on step 2, at 16.8 r/s,
    where the steps around it put it at 25.0. Only a valid step lowers another, and a
    reading is only ever lowered: a transient adds latency, it never removes it. A
    median of sub-window quantiles was the other choice; it drops the slow minutes of
    a bursty step too, which are what a bursty step measures (r1's C-onset step 2,
    p95 587.3 ms whole, 571.0 by 30 s medians). Missing readings stay missing.

    A valid step is lowered only when its excess is past its own sampling error (the
    later reading under its quantile's low band, key + "_lo") or it shows a transient
    (drift_p90 past DRIFT_TRANSIENT either way); a step with no band is lowered. Any
    other excess is noise between two readings of one curve, and the steps are pooled
    instead: the isotonic fit, weighted by requests, of the valid steps not lowered.
    envelope_marks says which steps each rule moved.
    """
    return envelope_marks(rows, key, bounds)[0]


# A step whose second half reads its p90 this many times its first, or under 1 over it,
# moved during its window.
DRIFT_TRANSIENT = 1.5


def transient(row):
    """Whether a step's reading moved inside its window."""
    d = row.get("drift_p90")
    return isinstance(d, (int, float)) and d == d and (d > DRIFT_TRANSIENT or d < 1 / DRIFT_TRANSIENT)


def envelope_marks(rows, key, bounds=True):
    """(readings, marks) of envelope: a mark is "lowered", "pooled" or "" a step."""
    raw = [row.get(key) for row in rows]
    missing = [v is None or (isinstance(v, float) and math.isnan(v)) for v in raw]
    vals, marks = list(raw), [""] * len(rows)
    low = math.inf
    for i in reversed(range(len(rows))):
        if missing[i]:
            continue
        void = bounds and invalid(rows[i])
        if raw[i] > low:
            band = rows[i].get(key + "_lo")
            if void or band is None or low < band or transient(rows[i]) or math.isinf(raw[i]):
                vals[i], marks[i] = low, "lowered"
        if not void:
            low = min(low, raw[i])
    blocks = []
    for i, row in enumerate(rows):
        if missing[i] or marks[i] or (bounds and invalid(row)) or math.isinf(vals[i]):
            continue
        w = max(row.get("due") or 1, 1)
        blocks.append([w * vals[i], w, [i]])
        while len(blocks) > 1 and blocks[-2][0] / blocks[-2][1] > blocks[-1][0] / blocks[-1][1]:
            s, w2, members = blocks.pop()
            blocks[-1][0] += s
            blocks[-1][1] += w2
            blocks[-1][2] += members
    for s, w, members in blocks:
        if len(members) > 1:
            for i in members:
                vals[i], marks[i] = s / w, "pooled"
    return vals, marks


def measured(rows, key, target, x="rate", bounds=True, used=None):
    """Where the reading meets target along the ladder: (kind, value).

    kind is "=" (value interpolated), "below" (the first step misses), "at least"
    (the top valid step still meets it), "bound" (value a (low, high): the crossing
    lands on an invalid step), "cliff" (value a (low, high): the step below the
    crossing is valid, and the one above it reads the fleet (reads_the_fleet) at over
    CLIFF_FACTOR times as much, so no line between them says where the target falls,
    and any target between them crosses there), "void" (no valid step says
    where the crossing is: every step is void, or the first one is and misses) or "no
    data". The readings are the envelope's. An infinite step ends the climb at the
    step before it. used, a list, gets the envelope's marks of the steps the result
    was read from ("lowered", "pooled"), so a verdict can say it rests on one.
    """
    if bounds and rows and all(invalid(r) for r in rows):
        return "void", None
    prev = row_before = None
    top = None
    values, marks = envelope_marks(rows, key, bounds)
    mark_before = top_mark = ""

    def note(*ms):
        if used is not None:
            used.extend(m for m in ms if m)

    for row, value, m in zip(rows, values, marks):
        if value is None or (isinstance(value, float) and math.isnan(value)):
            return "no data", None
        if value > target:
            note(m, mark_before)
            if prev is None:
                return ("void", None) if bounds and invalid(row) else ("below", row[x])
            x0, y0 = prev
            jump = y0 > 0 and value > CLIFF_FACTOR * y0
            if jump and not (bounds and (invalid(row_before) or not reads_the_fleet(row))):
                return "cliff", (x0, row[x])
            # A jump no line spans, read on a step that is not the fleet's own, is a bound.
            if bounds and (invalid(row) or invalid(row_before) or jump):
                return "bound", (x0, row[x])
            if math.isinf(value):
                return "=", x0
            return "=", x0 + (target - y0) / (value - y0) * (row[x] - x0)
        prev, row_before, mark_before = (row[x], value), row, m
        if not (bounds and invalid(row)):
            top, top_mark = row[x], m
    if top is None:
        return "no data", None
    note(top_mark)
    return "at least", top


def mark(predicted, kind, value):
    """(ratio text, verdict) of predicted against a measured() result."""
    if kind == "bound":
        lo, hi = predicted / value[1], predicted / value[0]
        if MARK_LOW <= lo and hi <= MARK_HIGH:
            return "%.2f to %.2f" % (lo, hi), "in the mark, a bound"
        if hi < MARK_LOW or lo > MARK_HIGH:
            return "%.2f to %.2f" % (lo, hi), "FAIL, a bound"
        return "%.2f to %.2f" % (lo, hi), "no mark, a bound"
    if kind == "cliff":
        lo, hi = predicted / value[1], predicted / value[0]
        if MARK_LOW <= lo and hi <= MARK_HIGH:
            return "%.2f to %.2f" % (lo, hi), CLIFF_IN
        if hi < MARK_LOW or lo > MARK_HIGH:
            return "%.2f to %.2f" % (lo, hi), "FAIL, on the cliff"
        return "%.2f to %.2f" % (lo, hi), "no mark, on the cliff"
    if kind == "void":
        return "n/a", NO_VALID_CROSSING
    if kind == "below":
        return "> %.2f" % (predicted / value), "FAIL, over-promises"
    if kind == "at least":
        return "at most %.2f" % (predicted / value), "no mark"
    if kind != "=":
        return "n/a", "no mark"
    ratio = predicted / value
    if ratio > MARK_HIGH:
        return "%.2f" % ratio, "FAIL, over-promises"
    if ratio < MARK_LOW:
        return "%.2f" % ratio, "FAIL, under-promises"
    return "%.2f" % ratio, "in the mark"


def show(kind, value):
    """A measured() result as text."""
    if kind == "void":
        return "no valid step"
    if value is None:
        return "n/a"
    if kind == "bound":
        return "%.1f to %.1f (bound)" % value
    if kind == "cliff":
        return "%.1f to %.1f (on the cliff)" % value
    return {"below": "below the ladder (%.1f)", "at least": "at least %.1f", "=": "%.1f"}[kind] % value


def held(row):
    """A step held when completions in its window came within the tolerance of what was sent, and none failed.

    Against the rate achieved, not the rate asked: bursty arrivals send a few percent
    either side of it over seven minutes, and that is no shortfall of the fleet's.
    """
    return row["failed"] == 0 and row["completion_rate"] >= row["achieved"] * (1 - HOLD_TOLERANCE)


def knee_bracket(rows):
    """(last rate held, first rate not held), either None: the measured knee, reported apart."""
    low = high = None
    for row in rows:
        if held(row):
            low = row["rate"]
        else:
            high = row["rate"]
            break
    return low, high


def ols_slope(xs, ys):
    """The least-squares slope of y on x; NaN under two distinct x."""
    n = len(xs)
    if n < 2:
        return math.nan
    mx, my = sum(xs) / n, sum(ys) / n
    den = sum((x - mx) ** 2 for x in xs)
    if den == 0:
        return math.nan
    return sum((x - mx) * (y - my) for x, y in zip(xs, ys)) / den


def median(values):
    """The median of finite and infinite values; NaN with none."""
    ordered = sorted(values)
    n = len(ordered)
    if not n:
        return math.nan
    if n % 2:
        return ordered[n // 2]
    return (ordered[n // 2 - 1] + ordered[n // 2]) / 2


def in_band(value, band):
    """Whether value lies in the closed band; NaN does not."""
    return band[0] <= value <= band[1]
