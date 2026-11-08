#!/usr/bin/env python3
"""
staircase_load.py -- open-loop load whose rate steps on a fixed schedule, with
one arrival stream shared by several processes. It drives the TTFT staircase
(docs/proposals/slo-capacity-planner-ttft.md, "Ground truth and the staircase",
and hack/benchmark/staircase.sh, which runs it in a pod).

WHY NOT closed_loop_sweep.py OR GUIDELLM
----------------------------------------
A closed loop sets the number in flight, so no queue model prices it. The
staircase needs a rate. guidellm has constant and Poisson rates only, and the
staircase's bucket C needs a renewal process with an interarrival Ca^2 of 4.

THE ARRIVALS
------------
--schedule is "hold:rate:in:out:ca2,...": seconds, requests a second, prompt
words, max_tokens, and the interarrival squared coefficient of variation.
ca2 1 is Poisson, ca2 0 is evenly spaced, and ca2 above 1 is a balanced
two-phase hyperexponential: mean 1/rate and that SCV, which is on/off traffic
with exponential phase lengths.

The send times are computed before the first request, from --seed, in one
stream. Each process takes every N-th request of it. Giving each process its
own stream would not do: a superposition of renewal processes tends to Poisson,
so N streams of SCV 4 are not one of SCV 4.

Prompts are n words drawn uniformly from 0.9n to 1.1n, and max_tokens likewise,
as closed_loop_sweep.py sends them. Each prompt starts with 32 random words, so
no two share a block and prefix hits do not reduce the prefill work. It sends
ignore_eos and never min_tokens: the P/D sidecar rewrites max_tokens to 1 on
the prefill leg, and min_tokens is then refused.

WHAT IT RECORDS
---------------
One CSV per process under --out, a row per finished request:

    index,level,scheduled,sent,first,last,status,prompt_tokens,tokens,error,pod

in epoch seconds. TTFT is first - scheduled, measured from when the request
was due, so the driver's own lag cannot hide in it; sent - scheduled is that
lag. A stream counts as ok only when it ends with data: [DONE], no event
carried an error, and it delivered tokens. pod is the response header
--pod-header names, empty without one: per-pod Ca2 is read from it, and an empty
column is reported as no reading, never as a figure.

THE LADDER'S END
----------------
At each level's end the parent reads the rows of the level after its first
--discard seconds. A request due in that window with no row yet counts as
infinitely slow. When the p90 of TTFT is above --runaway seconds the level ran
away: no later level is sent. The same stop follows more than --max-inflight
requests in flight, at any moment of a level.

These are two rules, and a phase must say which ended it: the first run's cap
ended two phases before the p90 rule could read a full window, and its log did
not name the rule. So a RUNAWAY line carries rule=client-p90 or rule=max-inflight,
and SWEEP DONE repeats it.

The log prints one LEVEL line at each change, one STATUS line every 30 s, a
JUDGED line per level, and SWEEP DONE at the end.

usage:
  staircase_load.py --url http://ROUTER:80 --model MODEL --schedule ... --out DIR
"""
import argparse
import asyncio
import json
import math
import multiprocessing
import os
import random
import re
import sys
import time

WORDS = ("time year people way day man thing woman life child world school state family student group "
         "country problem hand part place case week company system program question work government number "
         "night point home water room mother area money story fact month lot right study book eye job word "
         "business issue side kind head house service friend father power hour game line end member law car "
         "city community name president team minute idea kid body information back parent face others level "
         "office door health person art war history party result change morning reason research girl guy "
         "moment air teacher force education").split()

# The first event carrying text, not the first event: vLLM's first frame can
# be an empty delta.
FIRST_TEXT = re.compile(rb'"text":\s*"(?!")')
USAGE_OUT = re.compile(rb'"completion_tokens":\s*(\d+)')
USAGE_IN = re.compile(rb'"prompt_tokens":\s*(\d+)')
HEADER = "index,level,scheduled,sent,first,last,status,prompt_tokens,tokens,error,pod\n"
TAIL_BYTES = 2048
FLUSH_EVERY_S = 5
RULE_P90 = "client-p90"
RULE_INFLIGHT = "max-inflight"


def now():
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def parse_schedule(text):
    """Return [(hold, rate, in, out, ca2)]."""
    levels = []
    for part in text.split(","):
        if not part:
            continue
        hold, rate, n_in, n_out, ca2 = part.split(":")
        level = (int(hold), float(rate), int(n_in), int(n_out), float(ca2))
        if level[0] <= 0 or not level[1] > 0 or level[2] <= 0 or level[3] <= 0 or level[4] < 0:
            sys.exit("bad level %r" % part)
        levels.append(level)
    if not levels:
        sys.exit("empty schedule")
    return levels


def gap_sampler(rng, rate, ca2):
    """Return a function drawing one gap, mean 1/rate, SCV ca2 (see the module doc)."""
    if ca2 == 0:
        return lambda: 1.0 / rate
    if ca2 == 1:
        return lambda: rng.expovariate(rate)
    if ca2 < 1:
        sys.exit("ca2 %g: want 0, 1, or above 1" % ca2)
    p = (1 + math.sqrt((ca2 - 1) / (ca2 + 1))) / 2
    fast, slow = 2 * p * rate, 2 * (1 - p) * rate
    return lambda: rng.expovariate(fast if rng.random() < p else slow)


def arrivals(levels, seed):
    """Return every request of the schedule as (due offset, level, words, max_tokens), in order."""
    out, start = [], 0.0
    for number, (hold, rate, n_in, n_out, ca2) in enumerate(levels, 1):
        rng = random.Random(seed * 1000 + number)
        gap = gap_sampler(rng, rate, ca2)
        t = start + gap()
        while t < start + hold:
            out.append((t, number, rng.randint(int(n_in * 0.9), int(n_in * 1.1)),
                        rng.randint(int(n_out * 0.9), int(n_out * 1.1))))
            t += gap()
        start += hold
    return out


class Texts:
    """A pool of long random texts, so a prompt costs a slice, not a draw per word."""

    def __init__(self, seed, longest):
        rng = random.Random(seed)
        self.rng = rng
        self.pool = []
        for _ in range(32):
            words = rng.choices(WORDS, k=int(longest * 1.2))
            text = " ".join(words)
            self.pool.append((text, len(text) / len(words)))

    def prompt(self, words):
        text, chars_per_word = self.rng.choice(self.pool)
        # A slice may cut a word; the prompt's length is what matters, and the
        # engine's own count comes back in usage.
        return " ".join(self.rng.choices(WORDS, k=32)) + " " + text[:int(max(words - 32, 1) * chars_per_word)]


async def read_stream(r):
    """Read a streamed response to its end; return (first, last, prompt_tokens, tokens, error)."""
    first = last = None
    head, tail = b"", b""
    async for chunk in r.content.iter_any():
        t = time.time()
        if first is None:
            head = (head + chunk)[-TAIL_BYTES:]
            if FIRST_TEXT.search(head):
                first = t
                head = b""
        tail = (tail + chunk)[-TAIL_BYTES:]
        last = t
    error = ""
    if b"[DONE]" not in tail:
        error = "stream ended without [DONE]"
    if b'"error"' in tail:
        error = "error in stream"
    m_in, m_out = USAGE_IN.findall(tail), USAGE_OUT.findall(tail)
    n_in = int(m_in[-1]) if m_in else 0
    n_out = int(m_out[-1]) if m_out else 0
    if first is None and not error:
        error = "no text in stream"
    return first, last, n_in, n_out, error


async def fire(sess, url, model, text, row, t0, rows, shared, pod_header=""):
    """Send one request; append its CSV row."""
    index, (due, level, words, tokens) = row
    scheduled = t0 + due
    body = {"model": model, "prompt": text, "max_tokens": tokens, "ignore_eos": True, "temperature": 0,
            "stream": True, "stream_options": {"include_usage": True}}
    sent = time.time()
    first = last = None
    status, n_in, n_out, error, pod = 0, 0, 0, "", ""
    with shared["inflight"].get_lock():
        shared["inflight"].value += 1
    try:
        async with sess.post(url, data=json.dumps(body), headers={"Content-Type": "application/json"}) as r:
            status = r.status
            if pod_header:
                pod = r.headers.get(pod_header, "")
            if status == 200:
                first, last, n_in, n_out, error = await read_stream(r)
            else:
                error = (await r.text())[:100]
    except asyncio.CancelledError:
        # Cut at the drain's end: it still gets a row, or a request that ran away would
        # leave the analysis, which reads rows, as if it had never been sent.
        error = "cancelled"
    except Exception as e:  # noqa: BLE001 -- counted and shown; the load goes on
        error = type(e).__name__
    with shared["inflight"].get_lock():
        shared["inflight"].value -= 1
    ok = status == 200 and not error and n_out > 0
    with shared["ok" if ok else "err"].get_lock():
        shared["ok" if ok else "err"].value += 1
    rows.append("%d,%d,%.4f,%.4f,%.4f,%.4f,%d,%d,%d,%s,%s\n" % (
        index, level, scheduled, sent, first or 0, last or 0, status, n_in, n_out,
        error.replace(",", ";").replace("\n", " ")[:100], pod.replace(",", ";")))


async def run_worker(i, n, a, plan, t0, shared):
    import aiohttp  # imported here so --help works without it

    mine = [(j, row) for j, row in enumerate(plan) if j % n == i]
    texts = Texts(a.seed * 7919 + i, max(row[2] for row in plan))
    url = a.url.rstrip("/") + "/v1/completions"
    rows, tasks = [], set()
    path = os.path.join(a.out, "w%d.csv" % i)

    async def flusher(f):
        while True:
            await asyncio.sleep(FLUSH_EVERY_S)
            if rows:
                f.writelines(rows)
                rows.clear()
                f.flush()

    timeout = aiohttp.ClientTimeout(total=a.request_timeout)
    with open(path, "w") as f:
        f.write(HEADER)
        flush = asyncio.create_task(flusher(f))
        async with aiohttp.ClientSession(timeout=timeout, connector=aiohttp.TCPConnector(limit=0),
                                         read_bufsize=2 ** 20) as sess:
            for row in mine:
                wait = t0 + row[1][0] - time.time()
                if wait > 0:
                    await asyncio.sleep(wait)
                if time.time() >= shared["stop"].value:
                    break
                task = asyncio.create_task(fire(sess, url, a.model, texts.prompt(row[1][2]), row, t0, rows, shared,
                                                a.pod_header))
                tasks.add(task)
                task.add_done_callback(tasks.discard)
            if tasks:
                await asyncio.wait(tasks, timeout=a.drain)
            left = list(tasks)
            for task in left:
                task.cancel()
            if left:
                await asyncio.gather(*left, return_exceptions=True)
        flush.cancel()
        f.writelines(rows)


def worker(i, n, a, plan, t0, shared):
    asyncio.run(run_worker(i, n, a, plan, t0, shared))


def judged(a, plan, levels, number, end):
    """Return (p90 of TTFT over the level's judged window, rows, unfinished) as the CSVs stand."""
    start = end - max(levels[number - 1][0] - a.discard, 0)
    due = {j for j, row in enumerate(plan) if row[1] == number and start <= a.t0 + row[0] < end}
    ttft = {}
    for name in os.listdir(a.out):
        if not name.endswith(".csv"):
            continue
        with open(os.path.join(a.out, name)) as f:
            next(f, None)
            for line in f:
                p = line.split(",")
                if len(p) < 7:
                    continue
                j = int(p[0])
                if j in due and p[6] == "200" and float(p[4]) > 0:
                    ttft[j] = float(p[4]) - float(p[2])
    if not due:
        return math.nan, 0, 0
    vals = sorted(ttft.values()) + [math.inf] * (len(due) - len(ttft))
    return vals[min(len(vals) - 1, int(0.9 * len(vals)))], len(ttft), len(due) - len(ttft)


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--url", required=True, help="the router's base URL, e.g. http://ROUTER:80")
    ap.add_argument("--model", required=True, help="the served model name")
    ap.add_argument("--schedule", required=True, help="hold:rate:in:out:ca2,...")
    ap.add_argument("--out", required=True, help="directory for the per-process CSVs")
    ap.add_argument("--seed", type=int, default=20261105, help="seeds the arrivals and the prompts")
    ap.add_argument("--procs", type=int, default=6, help="sending processes")
    ap.add_argument("--discard", type=float, default=180, help="seconds of each level not judged, from its start")
    ap.add_argument("--runaway", type=float, default=0, help="p90 TTFT that ends the ladder, seconds; 0 is never")
    ap.add_argument("--max-inflight", type=int, default=4000, help="in flight that ends the ladder")
    ap.add_argument("--request-timeout", type=float, default=600, help="one request's limit, seconds")
    ap.add_argument("--drain", type=float, default=300, help="seconds to wait for requests in flight at the end")
    ap.add_argument("--pod-header", default="", help="response header naming the pod that served a request")
    ap.add_argument("--dry-run", action="store_true", help="print each level's measured Ca2 and exit")
    a = ap.parse_args()
    levels = parse_schedule(a.schedule)
    # A level with nothing to judge reads a NaN p90, and NaN passes the runaway rule.
    if a.runaway and any(lv[0] <= a.discard for lv in levels):
        sys.exit("a hold of %g s or less leaves nothing to judge: --discard is %g" % (a.discard, a.discard))
    plan = arrivals(levels, a.seed)
    total = sum(lv[0] for lv in levels)
    print("%s %d levels, %d s, %d requests, seed %d: %s" % (now(), len(levels), total, len(plan), a.seed, a.schedule),
          flush=True)
    if a.dry_run:
        for number in range(1, len(levels) + 1):
            times = [row[0] for row in plan if row[1] == number]
            gaps = [y - x for x, y in zip(times, times[1:])]
            m = sum(gaps) / len(gaps)
            print("level %d requests %d rate %.3f Ca2 %.3f" % (
                number, len(times), len(times) / levels[number - 1][0],
                sum((g - m) ** 2 for g in gaps) / len(gaps) / m ** 2))
        return
    os.makedirs(a.out, exist_ok=True)
    a.t0 = time.time() + 15  # time to load the texts before the first request
    ctx = multiprocessing.get_context("fork")
    shared = {"ok": ctx.Value("q", 0), "err": ctx.Value("q", 0), "inflight": ctx.Value("q", 0),
              "stop": ctx.Value("d", a.t0 + total + 1)}
    procs = [ctx.Process(target=worker, args=(i, a.procs, a, plan, a.t0, shared), daemon=True) for i in range(a.procs)]
    for p in procs:
        p.start()

    def status(number, last):
        print("%s STATUS level=%d ok=%d (+%d) err=%d inflight=%d" % (
            now(), number, shared["ok"].value, shared["ok"].value - last, shared["err"].value,
            shared["inflight"].value), flush=True)
        return shared["ok"].value

    last, ran_away = 0, ""
    for number, (hold, rate, n_in, n_out, ca2) in enumerate(levels, 1):
        begin = a.t0 + sum(lv[0] for lv in levels[:number - 1])
        end = begin + hold
        time.sleep(max(0, begin - time.time()))
        print("%s LEVEL %d rate=%g in=%d out=%d ca2=%g hold=%ds" % (now(), number, rate, n_in, n_out, ca2, hold),
              flush=True)
        while time.time() < end:
            time.sleep(min(30, max(0.1, end - time.time())))
            last = status(number, last)
            if shared["inflight"].value > a.max_inflight:
                print("%s RUNAWAY level=%d rule=%s inflight=%d" % (
                    now(), number, RULE_INFLIGHT, shared["inflight"].value), flush=True)
                ran_away = RULE_INFLIGHT
                break
        if ran_away:
            break
        time.sleep(FLUSH_EVERY_S + 1)
        p90, rows, unfinished = judged(a, plan, levels, number, end)
        print("%s JUDGED level=%d rate=%g client-p90=%.4f rows=%d unfinished=%d" % (
            now(), number, rate, p90, rows, unfinished), flush=True)
        if a.runaway and p90 > a.runaway:
            print("%s RUNAWAY level=%d rule=%s client-p90=%.4f above %g" % (
                now(), number, RULE_P90, p90, a.runaway), flush=True)
            ran_away = RULE_P90
            break
    shared["stop"].value = time.time()
    for p in procs:
        p.join(a.drain + 30)
    print("%s SWEEP DONE ok=%d err=%d runaway=%s" % (now(), shared["ok"].value, shared["err"].value,
                                                     ran_away or "no"), flush=True)


if __name__ == "__main__":
    main()
