#!/usr/bin/env python3
"""staircase_analyze.py -- what a staircase run measured, against what was frozen.

  staircase_analyze.py steps  <run dir> <phase> [--discard 180]
  staircase_analyze.py report <run dir> <frozen.json> <spec.json> [--redo frozen-at-measured-ca2.json]
  staircase_analyze.py gate   <run dir> <frozen.json> <spec.json> --repeat 1
  staircase_analyze.py ca2    <run dir>

steps writes phases/<phase>/analysis.jsonl, one row a step. Client figures are
over the step's requests (the level column) due in its judged window: its hold less
the discard from its first request, cut at the moment the driver stopped. A failed
or cancelled request, which the driver writes a row for, counts as infinitely slow. Router figures are the delta of the EPP's cumulative histogram
across the window: the raw scrape when there is one, else Prometheus rates
integrated over it. Every row carries stopped_by, the rule that ended the phase
on that step, "" on a step no rule ended.

report writes report.md into the run directory. The client is the witness: the
mark is read from it, at the operating point only, on the envelope of its steps
(staircase_lib.envelope). The router is a cross-check by its mean, and never decides
a mark: its quantiles are coarse, and are kept in the rows but not scored. The knee
is reported apart, with no mark. A rate is
compared over the median of a bucket's repeats, and a verdict stands for the
bucket only if every repeat agrees. A phase with no `done` file crashed: it is
excluded and named, and a bucket with a repeat missing has no pass.

gate scores the gate buckets' TTFT at one repeat, and exits 1 on a fail or a missing
phase: staircase.sh stops the run on it. ca2 prints BUCKET=CA2 for each bursty bucket,
the mean Ca2 sent over its whole steps, for whatif freeze --ca2.

Standard library only.
"""
import argparse
import collections
import csv
import glob
import gzip
import json
import math
import os
import re
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import staircase_lib as lib  # noqa: E402
import staircase_load as load  # noqa: E402

TTFT = "llm_d_epp_request_ttft_seconds"
TPOT = "llm_d_epp_request_streaming_tpot_seconds"
# The time a request waits in the router's flow-control queue, which fills once the
# prefill gate closes: what the steps above the prefill onset are there to read.
QUEUE = "llm_d_epp_flow_control_request_queue_duration_seconds"
# A wait the report counts as one: over 10 ms, against a TTFT of hundreds.
QUEUE_WAITED = 0.01
PROM_STEP = 15
PHASE = re.compile(r"^(.+)-(on|off)(?:-r(\d+))?$")
INF = float("inf")


def read_lines(path):
    try:
        with open(path, encoding="utf8", errors="replace") as f:
            return f.readlines()
    except FileNotFoundError:
        return []


def read_requests(path):
    """The driver's rows, old format or new. The header repeats: every process writes one."""
    rows = []
    with gzip.open(path, "rt", encoding="utf8", errors="replace") as f:
        for r in csv.reader(f):
            if r and r[0] != "index" and len(r) >= 9:
                rows.append(r)
    return rows


def read_pod_map(path):
    """{request index: pod} from an optional index,pod CSV: a log join the driver cannot make."""
    out = {}
    for line in read_lines(path):
        parts = line.strip().split(",")
        if len(parts) == 2 and parts[0].isdigit():
            out[parts[0]] = parts[1]
    return out


def read_raw_epp(path):
    """The raw EPP scrape: {name: {t: {le or "": value}}}, summed over label sets, one EPP
    pod a poll: the first pod a poll names is the one read, so two pods' counters, which
    reset apart, are never added."""
    raw = collections.defaultdict(lambda: collections.defaultdict(lambda: collections.defaultdict(float)))
    pod_at = {}
    for line in read_lines(path):
        p = line.split()
        if len(p) < 4:
            continue
        if pod_at.setdefault(p[0], p[1]) != p[1]:
            continue
        name = p[2].split("{")[0]
        if not (name.startswith(TTFT) or name.startswith(TPOT) or name.startswith(QUEUE)):
            continue
        m = re.search(r'le="([^"]*)"', p[2])
        raw[name][lib.ts(p[0])][m[1] if m else ""] += float(p[-1])
    return raw


def read_prom(path):
    out = {}
    for line in read_lines(path):
        o = json.loads(line)
        if o.get("data"):
            out[o["name"]] = o["data"]["result"]
    return out


def read_replicas(path):
    """{role: [(t, spec, ready)]} of the capture's replica samples."""
    out = {"decode": [], "prefill": []}
    for line in read_lines(path):
        p = line.split()
        if len(p) == 4:
            role = "decode" if p[1].endswith("-decode") else "prefill" if p[1].endswith("-prefill") else None
            if role:
                out[role].append((lib.ts(p[0]), p[2], p[3]))
    return out


def replicas_seen(samples, a, b):
    """{role: sorted distinct counts, spec and ready} over the window."""
    return {role: sorted({int(c) for t, spec, ready in rows if a <= t < b for c in (spec, ready)})
            for role, rows in samples.items()}


def read_engine(path):
    """{role: {pod: {...}}} of the engines: "sum" and "count" are the TTFT counters as
    [(t, value)]; "running" and "waiting" the request gauges as {t: value}, summed over a
    pod's engines, since a data-parallel pod reports one series per engine."""
    out = {"decode": {}, "prefill": {}}
    for line in read_lines(path):
        p = line.split()
        if len(p) < 4:
            continue
        role = "decode" if "-decode-" in p[1] else "prefill" if "-prefill-" in p[1] else None
        if not role:
            continue
        pod = out[role].setdefault(p[1], {"sum": [], "count": [], "running": {}, "waiting": {}})
        if "time_to_first_token_seconds_" in p[2]:
            pod["sum" if "_sum" in p[2] else "count"].append((lib.ts(p[0]), float(p[-1])))
        elif p[2].startswith(("vllm:num_requests_running", "vllm:num_requests_waiting")):
            gauge = pod["running" if "_running" in p[2] else "waiting"]
            t = lib.ts(p[0])
            gauge[t] = gauge.get(t, 0.0) + float(p[-1])
    return out


def gauge_figures(pods, key, a, b):
    """{pod: [mean, max]} of a gauge over the polls in [a, b); pods with no poll there are left out."""
    out = {}
    for name, series in sorted(pods.items()):
        seen = [v for t, v in series[key].items() if a <= t < b]
        if seen:
            out[name] = [sum(seen) / len(seen), max(seen)]
    return out


def counter_delta(series, a, b):
    """The delta between the last samples at or before a and b; None without both."""
    xa = [v for t, v in series if t <= a]
    xb = [v for t, v in series if t <= b]
    if not xa or not xb or xb[-1] < xa[-1]:
        return None
    return xb[-1] - xa[-1]


def engine_mean(pods, a, b):
    """A role's mean TTFT over the window, in seconds; None with no request counted."""
    total = count = 0.0
    for series in pods.values():
        s, c = counter_delta(series["sum"], a, b), counter_delta(series["count"], a, b)
        if s is not None and c is not None:
            total, count = total + s, count + c
    return total / count if count > 0 else None


def raw_delta(raw, base, a, b):
    """The delta of base's _bucket, _sum and _count across the window; None if unreadable."""
    bk = raw.get(base + "_bucket")
    if not bk:
        return None
    stamps = sorted(bk)
    ta = [t for t in stamps if t <= a]
    tb = [t for t in stamps if t <= b]
    if not ta or not tb or tb[-1] - ta[-1] < 0.8 * (b - a) or b - tb[-1] > 60:
        return None
    x, y = bk[ta[-1]], bk[tb[-1]]
    if any(y.get(k, 0) < v for k, v in x.items()):  # a counter went backwards: an EPP restart
        return None
    out = {"cum": sorted((INF if k == "+Inf" else float(k), y[k] - x.get(k, 0)) for k in y), "src": "raw"}
    for sfx in ("_sum", "_count"):
        s = raw.get(base + sfx)
        if s and ta[-1] in s and tb[-1] in s:
            out[sfx] = s[tb[-1]][""] - s[ta[-1]][""]
    return out


def prom_delta(prom, key, a, b):
    """A rate series integrated over the window, by bucket; None with no sample."""
    acc = collections.defaultdict(float)
    for s in prom.get(key, []):
        le = s["metric"].get("le")
        if le is None:
            continue
        for t, v in s["values"]:
            if a <= float(t) < b and v != "NaN":
                acc[INF if le == "+Inf" else float(le)] += float(v) * PROM_STEP
    return {"cum": sorted(acc.items()), "src": "prom"} if acc else None


def series_mean(prom, key, a, b):
    """{label values: (mean, peak)} over the window."""
    out = {}
    for s in prom.get(key, []):
        vals = [float(v) for t, v in s["values"] if a <= float(t) < b and v != "NaN"]
        if vals:
            out[" ".join(str(v) for v in s["metric"].values()) or "all"] = (sum(vals) / len(vals), max(vals))
    return out


def prom_total(prom, key, a, b):
    """A rate summed over its series, integrated over the window."""
    return sum(float(v) * PROM_STEP for s in prom.get(key, []) for t, v in s["values"]
               if a <= float(t) < b and v != "NaN")


def client_figures(win, span, pods=None):
    """The figures of one window's rows. A row is ok on 200, a first token and no error."""
    ok = [r for r in win if r[6] == "200" and float(r[4]) > 0 and (len(r) < 10 or r[9] == "")]
    missing = [INF] * (len(win) - len(ok))
    ttft = sorted(float(r[4]) - float(r[2]) for r in ok) + missing
    sent_ttft = sorted(float(r[4]) - float(r[3]) for r in ok) + missing
    tpot = sorted((float(r[5]) - float(r[4])) / (int(r[8]) - 1) for r in ok if int(r[8]) > 1)
    lag = sorted(float(r[3]) - float(r[2]) for r in win)
    prompts = [int(r[7]) for r in ok if int(r[7]) > 0]
    mean_p = sum(prompts) / len(prompts) if prompts else math.nan
    spread = math.sqrt(lib.scv(prompts)) if len(prompts) > 1 else math.nan
    pairs = [((len(r) > 10 and r[10]) or (pods or {}).get(r[0], ""), float(r[3])) for r in win]
    by_pod = lib.ca2_by_pod(pairs)
    passed = {id(r) for r in ok}
    failed = [r for r in win if id(r) not in passed]
    other = sum(1 for r in failed if not lib.fleet_failure(r[6], r[9] if len(r) > 9 else ""))
    # The quantile's band at the burstiness the step sent: n arrivals at a Ca2 above 1
    # carry about n / Ca2 independent readings, so the Poisson band widens by sqrt(Ca2).
    sent_ca2 = lib.arrival_ca2([float(r[3]) for r in win])
    widen = math.sqrt(sent_ca2) if sent_ca2 == sent_ca2 and sent_ca2 > 1 else 1.0
    cis = {}
    for q in (50, 90, 95):
        cis["client_ttft_p%d_lo" % q], cis["client_ttft_p%d_hi" % q] = lib.pct_ci(ttft, q / 100, 1.96 * widen)
    return dict(cis, failed_other=other,
                due=len(win), failed=len(win) - len(ok), completed=len(ok), achieved=len(win) / span,
                ca2_sent=sent_ca2, ca2_by_pod=by_pod or None,
                lag_p99=lib.pct(lag, 0.99), prompt_mean=mean_p, prompt_cv=spread,
                client_ttft_p50=lib.pct(ttft, .5), client_ttft_p90=lib.pct(ttft, .9),
                client_ttft_p95=lib.pct(ttft, .95), client_sent_ttft_p90=lib.pct(sent_ttft, .9),
                client_sent_ttft_p95=lib.pct(sent_ttft, .95),
                client_ttft_mean=sum(t for t in ttft if t != INF) / len(ok) if ok else math.nan,
                client_tpot_mean=sum(tpot) / len(tpot) if tpot else math.nan,
                client_tpot_p90=lib.pct(tpot, .9), client_tpot_p95=lib.pct(tpot, .95))


def router_figures(row, raw, prom, a, b):
    """The router's histogram quantiles into row: raw scrape first, Prometheus else."""
    for key, base, prom_key in (("router_ttft", TTFT, "ttft_bucket"), ("router_tpot", TPOT, "tpot_bucket")):
        h = raw_delta(raw, base, a, b) or prom_delta(prom, prom_key, a, b)
        if h:
            row[key + "_src"], row[key + "_n"] = h["src"], h["cum"][-1][1]
            for q in (.5, .9, .95):
                v, lo, hi = lib.hist_quantile(h["cum"], q)
                row["%s_p%d" % (key, q * 100)] = v
                row["%s_p%d_lo" % (key, q * 100)], row["%s_p%d_hi" % (key, q * 100)] = lo, hi
            if h.get("_count"):
                row[key + "_mean"] = h["_sum"] / h["_count"]
        count = prom_total(prom, prom_key.replace("_bucket", "_count"), a, b)
        if count and key + "_mean" not in row:
            row[key + "_mean"] = prom_total(prom, prom_key.replace("_bucket", "_sum"), a, b) / count


def queue_figures(row, raw, prom, a, b):
    """The router queue's wait into row: how many waited, the share over QUEUE_WAITED, the
    mean, and a coarse p90. Raw scrape first, Prometheus else; nothing with neither."""
    h = raw_delta(raw, QUEUE, a, b) or prom_delta(prom, "fc_queue_bucket", a, b)
    if not h or not h["cum"] or h["cum"][-1][1] <= 0:
        return
    total = h["cum"][-1][1]
    # The histogram says nothing finer than its edges: the share counted is the one past
    # the largest edge at or under QUEUE_WAITED, and that edge is kept beside it.
    under = [(le, c) for le, c in h["cum"] if le <= QUEUE_WAITED]
    row["fc_wait_n"], row["fc_wait_src"] = total, h["src"]
    if under:
        edge, count = max(under)
        row["fc_waited_share"], row["fc_waited_edge"] = max(0.0, 1 - count / total), edge
    row["fc_wait_p90_coarse"] = lib.hist_quantile(h["cum"], 0.9)[0]
    if h.get("_count"):
        row["fc_wait_mean"] = h["_sum"] / h["_count"]
    else:
        count = prom_total(prom, "fc_queue_count", a, b)
        if count:
            row["fc_wait_mean"] = prom_total(prom, "fc_queue_sum", a, b) / count


PLAN_LINE = re.compile(r"\d+ levels, \d+ s, \d+ requests, seed (-?\d+): (\S+)")


def read_plan(lines, rows):
    """([(due offset, level)], t0) of the seeded schedule the driver logged, t0 the epoch its
    offsets count from, read off a row (its scheduled time less its due offset); (None, 0)
    when the log has no plan line or no row lines up with it. The plan line is looked for
    anywhere in the log, not only at its head."""
    for line in lines:
        m = PLAN_LINE.search(line)
        if m:
            plan = [(due, n) for due, n, _, _ in load.arrivals(load.parse_schedule(m[2]), int(m[1]))]
            for r in rows:
                i = int(r[0]) if r[0].isdigit() else -1
                if 0 <= i < len(plan) and plan[i][1] == int(r[1]):
                    return plan, float(r[2]) - plan[i][0]
            return None, 0.0
    return None, 0.0


def read_epp_pods(path):
    """[(t, pod)] of the EPP pods the raw scrape read, one a poll."""
    seen = {}
    for line in read_lines(path):
        p = line.split(None, 2)
        if len(p) >= 2:
            seen.setdefault(p[0], p[1])
    return [(lib.ts(t), pod) for t, pod in seen.items()]


def poll_interval(pods, a, b):
    """The median seconds between a prefill pod's polls in [a, b); None with under two."""
    gaps = []
    for series in pods.values():
        ts_ = sorted(t for t in series["running"] if a <= t < b)
        gaps += [y - x for x, y in zip(ts_, ts_[1:])]
    return lib.median(gaps) if gaps else None


def analyze_steps(run, phase, discard):
    """One row a judged step, stop rule on each."""
    d = os.path.join(run, "phases", phase)
    levels, stop = lib.parse_log(read_lines(d + "/sweep.log"))
    rows = read_requests(d + "/requests.csv.gz")
    pods = read_pod_map(d + "/pod-map.csv")
    raw, prom = read_raw_epp(run + "/epp-metrics.log"), read_prom(d + "/prom.jsonl")
    engine = read_engine(run + "/vllm-metrics.log")
    replicas = read_replicas(run + "/replicas.log")
    plan, t0 = read_plan(read_lines(d + "/sweep.log"), rows)
    errors = [lib.ts(line.split()[0]) for line in read_lines(run + "/engine-errors.txt") if line.strip()]
    epp = read_epp_pods(run + "/epp-metrics.log")
    by_level = collections.defaultdict(list)
    for r in rows:
        by_level[int(r[1])].append(r)
    out = []
    for level in levels:
        # The log's stamp drifts from the plan by seconds; the first request does not.
        mine = by_level.get(level["n"], [])
        if mine:
            level["start"] = min(float(r[2]) for r in mine)
        a, b = lib.judged_window(level, stop, discard)
        span = b - a
        win = [r for r in mine if a <= float(r[2]) < b]
        if not win or span <= 0:
            continue
        row = dict(phase=phase, level=level["n"], rate=level["rate"], ca2_asked=level["ca2"], window_s=span,
                   hold_s=level["hold"], truncated=span < level["hold"] - discard,
                   stopped_by=stop["rule"] if stop and stop["level"] == level["n"] else "")
        row.update(client_figures(win, span, pods))
        if plan is not None:
            # The seeded schedule fixed what was sent: the rate is what it put in the window,
            # and a step whose rows fall short of it lost requests the analysis cannot see.
            row["scheduled"] = sum(1 for due, n in plan if n == level["n"] and a <= t0 + due < b)
            row["recorded"] = len(win)
            row["achieved"] = row["scheduled"] / span
        else:
            # No schedule to count against: validity falls back to the band around the rate
            # asked, and the report names the step.
            row["schedule_unread"] = True
        row["engine_errors"] = sum(1 for t in errors if a <= t < b)
        row["epp_pods"] = sorted({pod for t, pod in epp if a <= t < b})
        row["completion_rate"] = sum(1 for r in rows if r[6] == "200" and a <= float(r[5]) < b) / span
        router_figures(row, raw, prom, a, b)
        queue_figures(row, raw, prom, a, b)
        pre, dec = engine_mean(engine["prefill"], a, b), engine_mean(engine["decode"], a, b)
        if pre is not None and dec is not None and row["client_ttft_mean"] == row["client_ttft_mean"]:
            row["engine_prefill_mean"], row["engine_decode_mean"] = pre, dec
            row["hop_mean"] = row["client_ttft_mean"] - pre - dec
        # The prefill engines' running cap L, as each pod's poll read it: reported, not scored.
        row["prefill_running"] = gauge_figures(engine["prefill"], "running", a, b)
        row["prefill_waiting"] = gauge_figures(engine["prefill"], "waiting", a, b)
        row["engine_poll_s"] = poll_interval(engine["prefill"], a, b)
        row["replicas"] = replicas_seen(replicas, a, b)
        row["fc_saturation"] = series_mean(prom, "fc_saturation", a, b)
        row["fc_queue"] = series_mean(prom, "fc_queue", a, b)
        row["waiting"] = series_mean(prom, "waiting", a, b)
        row["kv"] = series_mean(prom, "kv", a, b)
        half = (a + b) / 2
        row["drift_p90"] = drift(win, half)
        out.append(row)
    return out, stop


def drift(win, half):
    """Client p90 in the window's second half over its first: a step still moving reads far from 1."""
    def p90(part):
        return lib.pct(sorted(float(r[4]) - float(r[2]) if r[6] == "200" and float(r[4]) > 0 else INF for r in part), .9)
    first, second = p90([r for r in win if float(r[2]) < half]), p90([r for r in win if float(r[2]) >= half])
    if not first > 0 or first == INF or second == INF or second != second:
        return math.nan
    return second / first


def load_steps(run, phase):
    with open(os.path.join(run, "phases", phase, "analysis.jsonl"), encoding="utf8") as f:
        rows = [json.loads(line) for line in f]
    return sorted(rows, key=lambda r: r["level"])


def flag_steps(rows, bucket, prompt_cv):
    """Set what voids a step, given what the bucket asked for: the noise floor, a fleet that moved, a Ca2 to rerun."""
    fleet = {"decode": bucket["decode"], "prefill": bucket["prefill"]}
    for r in rows:
        need, rho_p, rho_d = lib.noise_floor(r["rate"], bucket, prompt_cv, r["ca2_sent"] if bucket["ca2"] != 1 else 1.0)
        r["need_s"], r["rho_prefill"], r["rho_decode"] = need, rho_p, rho_d
        r["noise_flag"] = r["window_s"] < need
        r["replica_change"] = any(seen and seen != [fleet[role]] for role, seen in r.get("replicas", {}).items())
        r["ca2_rerun"] = lib.ca2_out_of_band(r["ca2_sent"], bucket["ca2"])


def phases_of(run):
    """({(bucket, fc): {repeat: phase}} of the finished phases, [the phases that crashed]).

    A phase is finished when it has a done file and an analysis. Anything else under
    phases/ crashed: its rows are not a result.
    """
    out, crashed = collections.defaultdict(dict), []
    root = os.path.join(run, "phases")
    for name in sorted(os.listdir(root)):
        m = PHASE.match(name)
        if not m:
            continue
        if os.path.exists(os.path.join(root, name, "done")) and os.path.exists(os.path.join(root, name, "analysis.jsonl")):
            out[m[1], m[2]][int(m[3] or 1)] = name
        else:
            crashed.append(name)
    return out, crashed


# The verdict of a bound whose whole bracket is in the mark: the gate passes it.
BOUND_IN = "in the mark, a bound"


def fail(verdict):
    return verdict.startswith("FAIL")


def router_agreement(repeats):
    """The router's mean TTFT less the client's over a bucket's steps, in ms, as text.

    The cross-check the router can give: its mean comes from _sum and _count and is
    exact, where its quantiles are read inside wide buckets. Steps with a failure are
    left out: the client counts a failure as infinitely slow, the router not at all.
    """
    diffs = [1000 * (r["router_ttft_mean"] - r["client_ttft_mean"]) for rows in repeats.values() for r in rows
             if r.get("router_ttft_mean") is not None and math.isfinite(r["router_ttft_mean"])
             and math.isfinite(r.get("client_ttft_mean", math.nan)) and r.get("failed", 0) == 0]
    if not diffs:
        return "no router mean"
    return "%+.1f to %+.1f over %d steps" % (min(diffs), max(diffs), len(diffs))


def judge(predicted, per_repeat, missing=()):
    """(median measured text, ratio, verdict) over the repeats' measured() results.

    A bucket passes only if every repeat does. A repeat with no figure of its own
    leaves the bucket with no mark, and so does a repeat missing altogether: "every
    repeat agrees" cannot be judged on what is left. The median is of the figures that
    exist. A fail stands whatever is missing.
    """
    verdicts = [lib.mark(predicted, k, v)[1] for k, v in per_repeat]
    if missing and not any(fail(v) for v in verdicts):
        text = lib.show(*per_repeat[0]) if per_repeat else "none finished"
        return text, "n/a", "no mark, repeat %s missing" % ",".join(str(n) for n in missing)
    # A bracket's middle; an open one (no high end) by its low end.
    values = [((v[0] + v[1]) / 2 if math.isfinite(v[1]) else v[0]) if k in ("bound", "cliff", "jump") else v
              for k, v in per_repeat if v is not None]
    centre = lib.median(values) if values else None
    bound = any("a bound" in v for v in verdicts)
    cliff = any("on the cliff" in v for v in verdicts)
    if any(fail(v) for v in verdicts):
        verdict = "FAIL, a bound" if bound else "FAIL, on the cliff" if cliff else "FAIL"
    elif lib.NO_VALID_CROSSING in verdicts:
        verdict = lib.NO_VALID_CROSSING
    elif all(v.startswith("in the mark") or v == lib.CLIFF_IN for v in verdicts):
        # A bound outranks the cliff: the gate passes a bound in the mark, and does not
        # score the cliff.
        verdict = "in the mark, a bound" if bound else lib.CLIFF_IN if cliff else "in the mark"
    else:
        verdict = "no mark, a bound" if bound else "no mark, on the cliff" if cliff else "no mark"
    if len(per_repeat) == 1:
        kind, value = per_repeat[0]
        return lib.show(kind, value), lib.mark(predicted, kind, value)[0], lib.mark(predicted, kind, value)[1]
    ratio = "%.2f" % (predicted / centre) if centre else "n/a"
    return "median %.1f of %d" % (centre, len(values)) if centre else "n/a", ratio, verdict


def onset_ladder(bucket):
    """True for a frozen bucket whose ladder is in shares of its prefill knee: the prefill
    knee is the lower one, as in B-onset and C-onset, which double decode so that the steps
    run up to the prefill flow-control onset. Read from the frozen file, which every run
    carries, so it needs nothing the spec of a run frozen before it was written lacks."""
    prefill = bucket.get("prefillKnee")
    return prefill is not None and prefill <= bucket["knee"]


def falsifiers(data, frozen, spec, out):
    """The falsifiers set before the run: each says whether it held. The ITL slope is
    judged at the operating point only: a bucket on an onset ladder prints its slope as
    reported, and it counts toward nothing."""
    bands = spec.get("falsifiers", {})
    print("\n## Falsifiers\n", file=out)
    print("| Falsifier | Reading | Band | Held |", file=out)
    print("| --- | --- | --- | --- |", file=out)
    results = []

    def line(name, value, band, judged=True):
        if not judged:
            print("| %s | %.2f | %g to %g | reported, onset phase |" % (name, value, band[0], band[1]), file=out)
            return
        ok = lib.in_band(value, band)
        results.append(ok)
        print("| %s | %.2f | %g to %g | %s |" % (name, value, band[0], band[1], "yes" if ok else "NO"), file=out)

    hops = {}
    for (bucket, fc), repeats in sorted(data.items()):
        rows = [r for phase_rows in repeats.values() for r in phase_rows
                if "engine_decode_mean" in r and not r["truncated"] and r["failed"] == 0]
        if len(rows) >= 3 and "itlSlope" in bands:
            line("ITL slope, %s %s" % (bucket, fc), lib.ols_slope([r["client_tpot_mean"] for r in rows],
                                                                  [r["engine_decode_mean"] for r in rows]), bands["itlSlope"],
                 judged=not onset_ladder(frozen[bucket]))
        low = [r["hop_mean"] for r in rows if r["rate"] <= frozen[bucket]["operatingRate"]]
        if low:
            hops.setdefault(frozen[bucket]["prompt"], []).extend(low)
    if len(hops) >= 2 and "hopPerKiloToken" in bands:
        xs = sorted(hops)
        slope = lib.ols_slope([x / 1000 for x in xs], [1000 * lib.median(hops[x]) for x in xs])
        line("Hop, ms per 1000 prompt tokens, %d lengths" % len(xs), slope, bands["hopPerKiloToken"])
    if "burstRatio" in bands:
        for fc in ("off", "on"):
            b, c = data.get(("B", fc)), data.get(("C", fc))
            if not b or not c:
                continue
            for repeat in sorted(set(b) & set(c)):
                ratios = [rc["client_ttft_p90"] / rb["client_ttft_p90"] for rb, rc in zip(b[repeat], c[repeat])
                          if rb["level"] == rc["level"] and not rb["truncated"] and not rc["truncated"]
                          and rc["client_ttft_p90"] != INF and rb["client_ttft_p90"] != INF]
                if ratios:
                    line("C over B, client p90, flow control %s, repeat %d, median of %d steps" % (fc, repeat, len(ratios)),
                         lib.median(ratios), bands["burstRatio"])
    if not results:
        print("| none readable | | | |", file=out)
    return results


def expected_phases(spec, finished, frozen, only_gate, repeats):
    """{(bucket, fc): [the repeats a verdict needs]}, from the spec; from what ran without one."""
    if "buckets" not in spec or not any("fc" in b for b in spec["buckets"]):
        return {key: sorted(found) for key, found in finished.items() if key[0] in frozen}
    out = {}
    for b in spec["buckets"]:
        if b["name"] in frozen and (b.get("gate") or not only_gate):
            for fc in b["fc"]:
                out[b["name"], fc] = list(repeats)
    return out


def moved_note(used):
    """What a verdict's text adds when its crossing was read on a step the envelope moved."""
    if "lowered" in used:
        return " (read on a lowered step)"
    if "pooled" in used:
        return " (read on pooled steps)"
    return ""


def prefill_running_cap(data, measure, frozen, out, max_num_seqs=None):
    """One table a flow-control-on phase: each step's prefill engines' running and waiting
    requests, per pod, as the poll read vllm:num_requests_running and _waiting. The running
    cap L the router's gate stands in front of; reported, not scored."""
    print("\n## The prefill engines' running requests, flow control on\n", file=out)
    print("What each prefill pod ran at once, as its polls read it, scored by nothing. Per pod mean / "
          "max. The running max is the most any poll read: a sampled lower bound on the engine's "
          "running cap L, at the poll interval shown, and L itself only where a pod held at the cap "
          "with requests waiting in the engine. The prefill Deployment's args set max_num_seqs %s.\n"
          % ("to %d" % max_num_seqs if max_num_seqs else "nowhere the run recorded (vLLM's default applies)"), file=out)
    shown = 0
    for (bucket, fc), repeats in sorted(data.items()):
        if fc != "on":
            continue
        onset = frozen[bucket]["onsets"].get("Prefill")
        for n in sorted(repeats):
            extra = measure.get((bucket, fc, n), [])
            rows = sorted(repeats[n] + extra, key=lambda r: r["level"])
            print("### %s on r%d\n" % (bucket, n), file=out)
            print("| Step | Rate, r/s | Over the onset | Role | Running, per pod mean / max | Running max, all pods, a sampled lower bound | Waiting, per pod mean / max | Poll, s |", file=out)
            print("| --- | --- | --- | --- | --- | --- | --- | --- |", file=out)
            for r in rows:
                shown += 1
                running, waiting = r.get("prefill_running") or {}, r.get("prefill_waiting") or {}
                poll = r.get("engine_poll_s")
                print("| %d | %.1f | %s | %s | %s | %s | %s | %s |" % (
                    r["level"], r["rate"], "%.2fx" % (r["rate"] / onset) if onset else "no onset",
                    "measure only" if r in extra else "scored",
                    " ".join("%.1f / %.0f" % tuple(v) for v in running.values()) or "unread",
                    "%.0f" % max(v[1] for v in running.values()) if running else "unread",
                    " ".join("%.1f / %.0f" % tuple(v) for v in waiting.values()) or "unread",
                    "%.0f" % poll if poll else "unread"), file=out)
            print(file=out)
    if not shown:
        print("No flow-control-on step.", file=out)


MAX_NUM_SEQS = re.compile(r"--max[-_]num[-_]seqs[= ]+(\d+)")


def read_max_num_seqs(run):
    """max_num_seqs from the prefill Deployment's args the run saved (prefill-args.txt); None unset."""
    m = MAX_NUM_SEQS.search(" ".join(read_lines(os.path.join(run, "prefill-args.txt"))))
    return int(m[1]) if m else None


POST_HOC = ("Scored under two rules written on 2026-11-10, after repeat 1 of the second run was seen: "
            "the ITL slope is judged at the operating point only, and a bound wholly in the mark passes the gate.")


def post_hoc_note(run):
    """The line every report and gate carries: the rules changed after seeing data, and
    where the verdicts before them are kept (a resume's prior-<n>/gate-r*.out)."""
    kept = sorted(os.path.relpath(p, run).replace(os.sep, "/") for p in glob.glob(os.path.join(run, "prior-*", "gate-r*.out")))
    return "**%s** Verdicts from before them: %s." % (POST_HOC, ", ".join(kept) if kept else "none in this run")


def report(run, frozen_file, spec, redo_file, out, repeats=None, only_gate=False):
    """The report; returns the verdicts by (bucket, fc, reading). repeats and only_gate narrow it to a gate."""
    frozen = {b["name"]: b for b in frozen_file["buckets"]}
    redo = {b["name"]: b for b in redo_file["buckets"]} if redo_file else {}
    ratio_q = {r["percentile"]: r.get("value") for r in frozen_file.get("tpotRatio", [])}
    finished, crashed = phases_of(run)
    expected = expected_phases(spec, finished, frozen, only_gate, repeats or range(1, spec.get("repeats", 1) + 1))
    data, missing, measure = {}, {}, {}
    for key, wanted in sorted(expected.items()):
        found = finished.get(key, {})
        data[key] = {n: load_steps(run, found[n]) for n in wanted if n in found}
        missing[key] = [n for n in wanted if n not in found]
        for rows in data[key].values():
            flag_steps(rows, frozen[key[0]], spec["promptCV"])
        # The measurement-only steps leave the scored rows here: no crossing, knee or
        # falsifier reads them.
        rates = lib.measure_rates(frozen[key[0]], spec)
        for n, rows in data[key].items():
            measure[key + (n,)] = [r for r in rows if lib.is_measure(r["rate"], rates)]
            data[key][n] = [r for r in rows if not lib.is_measure(r["rate"], rates)]

    print("# Staircase report\n", file=out)
    print(post_hoc_note(run) + "\n", file=out)
    if crashed:
        print("**Phases that did not finish, excluded: %s.**\n" % ", ".join(crashed), file=out)
    print("## What the prediction rests on\n", file=out)
    print("| Bucket | Ca2 priced | Flags | Assumed | Transfer priced, s per token | Every row's transfer, s | Warm-up prefix hit rate, not priced |", file=out)
    print("| --- | --- | --- | --- | --- | --- | --- |", file=out)
    for name, b in sorted(frozen.items()):
        shown = redo.get(name, b)
        print("| %s | %g%s | %s | %s | %.3g | %.3f | %.3f |" % (
            name, shown["ca2"], ", measured" if name in redo and redo[name]["ca2"] != b["ca2"] else ", as frozen",
            ", ".join(b.get("flags") or []) or "none", "; ".join(b.get("assumed") or []) or "nothing",
            b.get("transferSecondsPerToken", 0), b.get("allRowsTransferSeconds", 0), b.get("recordedPrefixHitRate", 0)), file=out)
    print(file=out)
    print("The client is the witness and the router a cross-check. The mark is at the operating point; "
          "the knee has none.\n", file=out)
    print("## How each phase ended\n", file=out)
    print("| Phase | Steps | Stopped by | On step | Window judged, s | Truncated | Noise-flagged steps |", file=out)
    print("| --- | --- | --- | --- | --- | --- | --- |", file=out)
    for (bucket, fc), repeats in sorted(data.items()):
        for n, rows in sorted(repeats.items()):
            stopped = [r for r in rows if r["stopped_by"]]
            last = stopped[0] if stopped else None
            print("| %s %s r%d | %d | %s | %s | %s | %s | %s |" % (
                bucket, fc, n, len(rows), last["stopped_by"] if last else "no rule", last["level"] if last else "",
                "%d" % last["window_s"] if last else "", "yes" if last and last["truncated"] else "no",
                ",".join(str(r["level"]) for r in rows if r["noise_flag"]) or "none"), file=out)

    print("\n## Steps that are void\n", file=out)
    print("A crossing on one of these is a bound. A step marked rerun did not send what it asked for.\n", file=out)
    print("| Phase | Step | Why |", file=out)
    print("| --- | --- | --- |", file=out)
    void = 0
    for (bucket, fc), repeats in sorted(data.items()):
        for n, rows in sorted(repeats.items()):
            for r in rows:
                why = lib.validity(r)
                if why:
                    void += 1
                    print("| %s %s r%d | %d | %s |" % (bucket, fc, n, r["level"], "; ".join(why)), file=out)
    if not void:
        print("| none | | |", file=out)
    unread = ["%s %s r%d step %d" % (bucket, fc, n, r["level"]) for (bucket, fc), repeats in sorted(data.items())
              for n, rows in sorted(repeats.items()) for r in rows if r.get("schedule_unread")]
    if unread:
        print("\nRead without their seeded schedule, so held to the band around the rate asked, and read at "
              "the rate their rows show: %s." % ", ".join(unread), file=out)

    print("\n## TTFT at the operating point\n", file=out)
    print("A step reading above a later valid step reads that step's figure: TTFT does not fall as the rate "
          "rises (lowered steps are listed below). A crossing between steps over %g times apart is on the "
          "cliff: its bracket is the reading, the gate stops on it unless the whole bracket is in the mark, "
          "and it never counts as a pass. The router's quantiles are read from wide buckets and are not "
          "scored; its mean, which is exact, is set against the client's.\n" % lib.CLIFF_FACTOR, file=out)
    print("| Bucket | Flow control | q | Target | Predicted r/s | Measured r/s, client | Ratio | Mark | Router mean less client mean, ms |", file=out)
    print("| --- | --- | --- | --- | --- | --- | --- | --- | --- |", file=out)
    verdicts = {}
    for (bucket, fc), repeats in sorted(data.items()):
        b = redo.get(bucket, frozen[bucket])
        for t in b["targets"]:
            q = int(t["percentile"])
            predicted = t["predictedOff"] if fc == "off" else t["predictedOn"]
            used = []
            client = [lib.measured(rows, "client_ttft_p%d" % q, t["seconds"], x="achieved", used=used)
                      for _, rows in sorted(repeats.items())]
            text, ratio, verdict = judge(predicted, client, missing[bucket, fc])
            verdicts[bucket, fc, "ttft", q] = verdict
            print("| %s | %s | p%d | %.1f ms | %.1f | %s | %s | %s%s | %s |" % (
                bucket, fc, q, t["seconds"] * 1000, predicted, text, ratio, verdict, moved_note(used),
                router_agreement(repeats)), file=out)

    print("\n## Steps the envelope moved\n", file=out)
    print("Lowered: the step read above a later valid step by more than its own sampling band, or "
          "moved inside its window. Pooled: the excess was inside the band, and the steps share their "
          "weighted mean. A verdict read on either says so.\n", file=out)
    print("| Phase | Step | Reading | How | Read, ms | Scored, ms |", file=out)
    print("| --- | --- | --- | --- | --- | --- |", file=out)
    moved = 0
    for (bucket, fc), repeats in sorted(data.items()):
        for n, rows in sorted(repeats.items()):
            for q in sorted({int(t["percentile"]) for t in frozen[bucket]["targets"]}):
                key = "client_ttft_p%d" % q
                for r, scored, how in zip(rows, *lib.envelope_marks(rows, key)):
                    if how and r.get(key) is not None:
                        moved += 1
                        print("| %s %s r%d | %d | p%d | %s | %.1f | %.1f |" % (
                            bucket, fc, n, r["level"], q, how, 1000 * r[key], 1000 * scored), file=out)
    if not moved:
        print("| none | | | | | |", file=out)

    print("\n## TPOT at the operating point\n", file=out)
    print("| Bucket | Flow control | Reading | Target | Predicted r/s | Measured r/s, client | Ratio | Mark | Router, cross-check |", file=out)
    print("| --- | --- | --- | --- | --- | --- | --- | --- | --- |", file=out)
    for (bucket, fc), repeats in sorted(data.items()):
        b = frozen[bucket]
        readings = [("mean", "router_tpot_mean", "client_tpot_mean", b["meanTPOT"])]
        for q in (90, 95):
            if ratio_q.get(q):
                readings.append(("p%d" % q, "router_tpot_p%d" % q, "client_tpot_p%d" % q, b["meanTPOT"] * ratio_q[q]))
        for name, router_key, client_key, target in readings:
            if not target > 0:
                continue
            client = [lib.measured(rows, client_key, target, x="achieved") for _, rows in sorted(repeats.items())]
            text, ratio, verdict = judge(b["operatingRate"], client, missing[bucket, fc])
            verdicts[bucket, fc, "tpot", name] = verdict
            if name == "mean":
                router = [lib.measured(rows, router_key, target, x="achieved") for _, rows in sorted(repeats.items())]
                rtext, rratio, _ = judge(b["operatingRate"], router, missing[bucket, fc])
                cross = "%s, %s" % (rtext, rratio)
            else:
                cross = "not scored: coarse buckets"
            print("| %s | %s | %s | %.1f ms | %.1f | %s | %s | %s | %s |" % (
                bucket, fc, name, target * 1000, b["operatingRate"], text, ratio, verdict, cross), file=out)

    print("\n## The knee, reported apart\n", file=out)
    print("A step held if completions in its window came within 3% of what was sent and nothing failed. No mark applies.\n", file=out)
    print("| Bucket | Flow control | Repeat | Predicted knee | Last held | First not held |", file=out)
    print("| --- | --- | --- | --- | --- | --- |", file=out)
    for (bucket, fc), repeats in sorted(data.items()):
        for n, rows in sorted(repeats.items()):
            low, high = lib.knee_bracket(rows)
            print("| %s | %s | %d | %.1f | %s | %s |" % (bucket, fc, n, frozen[bucket]["knee"],
                                                          "none" if low is None else "%.1f" % low,
                                                          "none" if high is None else "%.1f" % high), file=out)

    print("\n## The onset, flow control on\n", file=out)
    print("| Bucket | Repeat | Predicted prefill onset | Lowest step above it | Saturation there | Router queue, peak | Window over 4T | Testable |", file=out)
    print("| --- | --- | --- | --- | --- | --- | --- | --- |", file=out)
    for (bucket, fc), repeats in sorted(data.items()):
        onset = frozen[bucket]["onsets"]["Prefill"]
        if fc != "on" or not onset:
            continue
        for n, rows in sorted(repeats.items()):
            above = [r for r in rows if r["rate"] > onset and not r["truncated"]]
            if not above:
                print("| %s | %d | %.1f | none | | | | no: no step above it |" % (bucket, n, onset), file=out)
                continue
            r = above[0]
            saturation = max((v[0] for v in r["fc_saturation"].values()), default=math.nan)
            queue = max((v[1] for v in r["fc_queue"].values()), default=math.nan)
            print("| %s | %d | %.1f | %.1f | %.2f | %.0f | %.2f | %s |" % (
                bucket, n, onset, r["rate"], saturation, queue, r["window_s"] / r["need_s"],
                "no: under the noise floor" if r["noise_flag"] else "yes"), file=out)

    print("\n## Above the prefill onset: the router queue's wait\n", file=out)
    print("Every flow-control-on step above the frozen prefill onset. Measure-only steps are scored by nothing: "
          "the model flags this wait (AboveFlowControlOnset) and does not price it. Waited is the share of "
          "requests the router queue held past the largest bucket edge at or under %g ms, shown with it "
          "(the histogram reads nothing finer); its p90 is read inside wide buckets.\n" % (1000 * QUEUE_WAITED), file=out)
    print("| Bucket | Repeat | Step, r/s | Over the onset | Role | Client TTFT p50 / p90, ms | Waited | Wait mean, ms | Wait p90, ms, coarse | Prefill gate saturation, mean / peak | Queue, peak | Held |", file=out)
    print("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |", file=out)
    shown = 0
    for (bucket, fc), repeats in sorted(data.items()):
        onset = frozen[bucket]["onsets"]["Prefill"]
        if fc != "on" or not onset:
            continue
        for n in sorted(repeats):
            rows = sorted(repeats[n] + measure.get((bucket, fc, n), []), key=lambda r: r["level"])
            for r in rows:
                if r["rate"] <= onset:
                    continue
                shown += 1
                gate = r.get("fc_saturation", {})
                pre = [v for k, v in gate.items() if "prefill" in k.split()]
                print("| %s | %d | %.1f | %.2fx | %s | %.1f / %.1f | %s | %s | %s | %s | %s | %s |" % (
                    bucket, n, r["rate"], r["rate"] / onset,
                    "measure only" if r in measure.get((bucket, fc, n), []) else "scored",
                    1000 * r["client_ttft_p50"], 1000 * r["client_ttft_p90"],
                    ("%.1f%%" % (100 * r["fc_waited_share"]) + (" past %g ms" % (1000 * r["fc_waited_edge"]) if "fc_waited_edge" in r else ""))
                    if "fc_waited_share" in r else "unread",
                    "%.1f" % (1000 * r["fc_wait_mean"]) if "fc_wait_mean" in r else "unread",
                    "%.1f" % (1000 * r["fc_wait_p90_coarse"]) if "fc_wait_p90_coarse" in r else "unread",
                    "%.2f / %.2f" % tuple(pre[0]) if pre else "unread",
                    "%.0f" % max((v[1] for v in r.get("fc_queue", {}).values()), default=math.nan) if r.get("fc_queue") else "unread",
                    "yes" if lib.held(r) else "no"), file=out)
    if not shown:
        print("| none | | | | | | | | | | | |", file=out)

    prefill_running_cap(data, measure, frozen, out, read_max_num_seqs(run))

    print("\n## Model against client, per step\n", file=out)
    print("Report only: flow control off, the frozen curve at the step's rate.\n", file=out)
    print("| Bucket | Flow control | Repeat | Step, r/s | Client mean, ms | Model | Client p90, ms | Model | Error, p90 | Hop, ms |", file=out)
    print("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |", file=out)
    for (bucket, fc), repeats in sorted(data.items()):
        for n, rows in sorted(repeats.items()):
            for r in rows:
                point = next((p for p in frozen[bucket]["curve"] if p.get("p90") and abs(p["rate"] / r["rate"] - 1) < 0.005), None)
                if not point or r["client_ttft_p90"] == INF:
                    continue
                print("| %s | %s | %d | %.1f | %.1f | %.1f | %.1f | %.1f | %+.0f%% | %s |" % (
                    bucket, fc, n, r["rate"], 1000 * r["client_ttft_mean"], 1000 * point["mean"], 1000 * r["client_ttft_p90"],
                    1000 * point["p90"], 100 * (point["p90"] / r["client_ttft_p90"] - 1),
                    "%.1f" % (1000 * r["hop_mean"]) if "hop_mean" in r else ""), file=out)

    print("\n## Per-pod Ca2\n", file=out)
    print("| Bucket | Flow control | Repeat | Step | Sent Ca2 | By pod |", file=out)
    print("| --- | --- | --- | --- | --- | --- |", file=out)
    seen = False
    for (bucket, fc), repeats in sorted(data.items()):
        for n, rows in sorted(repeats.items()):
            for r in rows:
                if r.get("ca2_by_pod"):
                    seen = True
                    print("| %s | %s | %d | %d | %.2f | %s |" % (bucket, fc, n, r["level"], r["ca2_sent"], " ".join(
                        "%.2f" % v for v in r["ca2_by_pod"].values())), file=out)
    if not seen:
        print("| no pod identity in this run | | | | | |", file=out)
    falsifiers(data, frozen, spec, out)
    return verdicts


def measured_ca2(run):
    """{bucket: mean Ca2 sent over the finished phases' whole steps}, bursty buckets only."""
    sent = collections.defaultdict(list)
    for (bucket, _), repeats in phases_of(run)[0].items():
        for name in repeats.values():
            for r in load_steps(run, name):
                if r["ca2_asked"] != 1 and abs(r["achieved"] / r["rate"] - 1) < 0.2:
                    sent[bucket].append(r["ca2_sent"])
    return {bucket: sum(v) / len(v) for bucket, v in sent.items()}


def gate(run, frozen, spec, repeat, out):
    """Score one repeat of the gate buckets: 0 to go on, 1 to stop. Written beside the report."""
    with open(os.path.join(run, "gate-r%d.md" % repeat), "w", encoding="utf8") as f:
        verdicts = report(run, frozen, spec, None, f, repeats=[repeat], only_gate=True)
    # A crossing on a void step is a bound: only its bracket is known. It passes when the
    # whole bracket is in the mark, since the verdict is then the same wherever in it the
    # crossing sits; a bracket across the mark's edge, or outside it, stops the run. A
    # crossing on the cliff stops it unless its whole bracket is in the mark, and passes
    # nothing.
    bad = {k: v if fail(v) or "missing" in v or "on the cliff" in v else lib.NO_VALID_CROSSING for k, v in verdicts.items()
           if k[2] == "ttft" and v != BOUND_IN and (fail(v) or "missing" in v or "a bound" in v or v == lib.NO_VALID_CROSSING
                                                    or v == "no mark, on the cliff")}
    if not verdicts:
        print("gate repeat %d: no gate bucket to score" % repeat, file=out)
        return 1
    cliff = sum(1 for k, v in verdicts.items() if k[2] == "ttft" and v == lib.CLIFF_IN)
    bounds = sum(1 for k, v in verdicts.items() if k[2] == "ttft" and v == BOUND_IN)
    print(post_hoc_note(run), file=out)
    print("gate repeat %d: %d readings, %d stop the run%s%s%s" % (
        repeat, sum(1 for k in verdicts if k[2] == "ttft"), len(bad),
        ", %d bounds wholly in the mark, passed" % bounds if bounds else "",
        ", %d on the cliff and in the mark, not scored" % cliff if cliff else "",
        ": " + "; ".join("%s %s p%s %s" % (k[0], k[1], k[3], v) for k, v in sorted(bad.items())) if bad else ""), file=out)
    return 1 if bad else 0


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    sub = ap.add_subparsers(dest="verb", required=True)
    steps = sub.add_parser("steps")
    steps.add_argument("run")
    steps.add_argument("phase")
    steps.add_argument("--discard", type=float, default=180)
    rep = sub.add_parser("report")
    rep.add_argument("run")
    rep.add_argument("frozen")
    rep.add_argument("spec")
    rep.add_argument("--redo", default="")
    gt = sub.add_parser("gate")
    gt.add_argument("run")
    gt.add_argument("frozen")
    gt.add_argument("spec")
    gt.add_argument("--repeat", type=int, default=1)
    ca2 = sub.add_parser("ca2")
    ca2.add_argument("run")
    a = ap.parse_args()
    if a.verb == "ca2":
        for bucket, value in sorted(measured_ca2(a.run).items()):
            print("%s=%.4f" % (bucket, value))
        return
    if a.verb == "steps":
        rows, stop = analyze_steps(a.run, a.phase, a.discard)
        if not rows:
            sys.exit("%s: no judged step" % a.phase)
        path = os.path.join(a.run, "phases", a.phase, "analysis.jsonl")
        with open(path + ".tmp", "w", encoding="utf8") as f:
            for r in rows:
                f.write(json.dumps(r) + "\n")
        os.replace(path + ".tmp", path)
        print("%s: %d steps, stopped by %s" % (a.phase, len(rows), stop["rule"] if stop else "no rule"))
        return
    with open(a.frozen, encoding="utf8") as f:
        frozen = json.load(f)
    with open(a.spec, encoding="utf8") as f:
        spec = json.load(f)
    if a.verb == "gate":
        sys.exit(gate(a.run, frozen, spec, a.repeat, sys.stdout))
    redo = None
    if a.redo:
        with open(a.redo, encoding="utf8") as f:
            redo = json.load(f)
    with open(os.path.join(a.run, "report.md"), "w", encoding="utf8") as f:
        verdicts = report(a.run, frozen, spec, redo, f)
    print("%d readings; %d fail" % (len(verdicts), sum(1 for v in verdicts.values() if fail(v))))


if __name__ == "__main__":
    main()
