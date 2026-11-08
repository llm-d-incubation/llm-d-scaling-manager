#!/usr/bin/env python3
"""
closed_loop_sweep.py -- closed-loop load whose concurrency and request shape
step on a fixed schedule. It drove the `ols-sweep` learner golden
(docs/developer-guide/testing.md, "Learner Goldens").

WHY CLOSED LOOP
---------------
The ITL learner's full fit needs steady readings of the decode pod's KV fill
(k) at two levels at least 0.30 apart. A fixed number of requests in flight
holds k at one level for as long as the level lasts; an open-loop rate does
not, because the number in flight drifts with the server's speed. For a
latency measurement the opposite holds -- see two_model_load.py.

THE SCHEDULE
------------
--schedule is "hold:inflight:in:out,..." -- seconds, requests in flight,
prompt words, and max_tokens. Each request is a random prompt of about `in`
words (about one token each) and asks for `out` tokens with ignore_eos. It
never sets min_tokens: the llm-d P/D routing sidecar rewrites max_tokens to 1
on the prefill leg, and a request carrying min_tokens is then refused with
HTTP 400. A level change leaves no gap: requests above the new count finish
and stop, and new ones start at once.

--scale multiplies every in-flight count, rounds it, and clamps it to
[1, --max-inflight]. Use it to fit the levels to a decode pod whose KV holds
more or less than the one the default schedule was written for.

The log prints one LEVEL line at each change and one STATUS line every 30 s,
so a step that did not happen, or requests that fail, are visible.

STREAMING
---------
--stream sends each request with stream: true and stream_options
include_usage, and reads the event stream to its end. The router measures
time to first token and time per output token only on a streamed response,
so a run that needs its llm_d_epp_request_ttft_seconds and
llm_d_epp_request_streaming_tpot_seconds (the slo analyzer's router
latency) needs it. It drove the `ols-sweep-slo` golden. A streamed request
counts as ok only when the stream ends with "data: [DONE]" and no event of it
carried an "error" (vLLM sends the error, then [DONE]). The default stays a
plain response, as `ols-sweep` ran.

usage:
  closed_loop_sweep.py --url http://ROUTER:80 --model MODEL [--schedule ...] [--scale F] [--stream]
"""
import argparse
import asyncio
import json
import random
import sys
import time

# The ols-sweep schedule: a 300 s first level, then eleven levels of 270 s.
DEFAULT_SCHEDULE = ("300:10:1000:4000,270:24:1000:4000,270:10:1000:4000,270:24:1000:500,"
                    "270:56:1000:500,270:5:6000:1000,270:11:6000:1000,270:4:6000:1000,"
                    "270:24:1000:4000,270:20:1000:500,270:10:6000:1000,270:8:1000:4000")

WORDS = ("time year people way day man thing woman life child world school state family student group "
         "country problem hand part place case week company system program question work government number "
         "night point home water room mother area money story fact month lot right study book eye job word "
         "business issue side kind head house service friend father power hour game line end member law car "
         "city community name president team minute idea kid body information back parent face others level "
         "office door health person art war history party result change morning reason research girl guy "
         "moment air teacher force education").split()


def parse_schedule(text, scale, max_inflight):
    """Return [(hold, inflight, in, out)], with each in-flight count scaled, rounded and clamped."""
    levels = []
    for part in text.split(","):
        if not part:
            continue
        hold, inflight, n_in, n_out = (int(x) for x in part.split(":"))
        inflight = min(max_inflight, max(1, round(inflight * scale)))
        levels.append((hold, inflight, n_in, n_out))
    if not levels:
        sys.exit("empty schedule")
    return levels


def now():
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


class Sweep:
    def __init__(self, url, model, stream=False):
        self.url, self.model, self.stream = url.rstrip("/") + "/v1/completions", model, stream
        self.target = self.n_in = self.n_out = self.level = 0
        self.ok = self.err = self.active = 0
        self.lat = 0.0
        self.lasterr = ""

    def request(self, n_in, n_out):
        """One request body: a random prompt of about n_in words asking for about n_out tokens."""
        words = random.randint(int(n_in * 0.9), int(n_in * 1.1))
        body = {"model": self.model, "prompt": " ".join(random.choice(WORDS) for _ in range(words)),
                "max_tokens": random.randint(int(n_out * 0.9), int(n_out * 1.1)),
                "ignore_eos": True, "temperature": 0, "stream": self.stream}
        if self.stream:
            body["stream_options"] = {"include_usage": True}
        return body

    @staticmethod
    async def read_stream(r):
        """Read a streamed response to its end. It returns "" when the stream
        ended with data: [DONE] and no event carried an "error"; otherwise the
        error, or what the stream ended with."""
        last, error = "", ""
        # aiohttp reads lines of at most 64 KiB; a longer one raises ValueError,
        # which the worker's except counts as a failed request. SSE events here
        # are small, and vLLM sends errors as a data line, never as "event: error".
        async for raw in r.content:
            line = raw.decode("utf-8", "replace").strip()
            if not line:
                continue
            if line == "data: [DONE]":
                return error
            last = line
            if line.startswith("data: {") and '"error"' in line:
                try:
                    event = json.loads(line[len("data: "):])
                except ValueError:
                    event = {}
                if isinstance(event, dict) and "error" in event:
                    error = "error in stream: " + line[:160]
        return error or "stream ended without [DONE]: " + last[:160]

    async def worker(self, i, sess):
        self.active += 1
        try:
            while i < self.target:
                body = self.request(self.n_in, self.n_out)
                t0 = time.time()
                try:
                    async with sess.post(self.url, json=body) as r:
                        if r.status == 200 and self.stream:
                            failure = await self.read_stream(r)
                        else:
                            txt = await r.text()
                            failure = "" if r.status == 200 else "%d %s" % (r.status, txt[:200])
                        if not failure:
                            self.ok += 1
                            self.lat += time.time() - t0
                            continue
                        self.lasterr = failure
                except Exception as e:  # noqa: BLE001 -- counted and shown; the load goes on
                    self.lasterr = repr(e)[:200]
                self.err += 1
                await asyncio.sleep(1)
        finally:
            self.active -= 1

    async def status(self):
        last = 0
        while True:
            await asyncio.sleep(30)
            print("%s STATUS level=%d inflight=%d active=%d in=%d out=%d ok=%d (+%d) err=%d meanlat=%.1fs lasterr=%s" % (
                now(), self.level, self.target, self.active, self.n_in, self.n_out, self.ok, self.ok - last,
                self.err, self.lat / self.ok if self.ok else 0, self.lasterr), flush=True)
            last = self.ok

    async def run(self, levels):
        import aiohttp  # imported here so --help works without it

        timeout = aiohttp.ClientTimeout(total=1800)
        async with aiohttp.ClientSession(timeout=timeout, connector=aiohttp.TCPConnector(limit=0)) as sess:
            status = asyncio.create_task(self.status())
            tasks = {}
            for lv, (hold, inflight, n_in, n_out) in enumerate(levels, 1):
                self.level, self.target, self.n_in, self.n_out = lv, inflight, n_in, n_out
                print("%s LEVEL %d inflight=%d in=%d out=%d hold=%ds" % (now(), lv, inflight, n_in, n_out, hold),
                      flush=True)
                for i in range(inflight):
                    if i not in tasks or tasks[i].done():
                        tasks[i] = asyncio.create_task(self.worker(i, sess))
                await asyncio.sleep(hold)
            self.target = 0
            status.cancel()
            print("%s SWEEP DONE ok=%d err=%d" % (now(), self.ok, self.err), flush=True)


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--url", required=True, help="the router's base URL, e.g. http://ROUTER:80")
    ap.add_argument("--model", required=True, help="the served model name")
    ap.add_argument("--schedule", default=DEFAULT_SCHEDULE, help="hold:inflight:in:out,... (default: ols-sweep's)")
    ap.add_argument("--scale", type=float, default=1.0, help="multiply every in-flight count (default 1)")
    ap.add_argument("--max-inflight", type=int, default=64, help="clamp for a scaled count (default 64)")
    ap.add_argument("--stream", action="store_true",
                    help="stream each response (stream_options include_usage) and read it to its end")
    ap.add_argument("--dry-run", action="store_true", help="print the levels and exit")
    a = ap.parse_args()
    levels = parse_schedule(a.schedule, a.scale, a.max_inflight)
    total = sum(h for h, _, _, _ in levels)
    print("%s %d levels, %d s, stream=%s: %s" % (now(), len(levels), total, "on" if a.stream else "off",
                                                 ",".join("%d:%d:%d:%d" % lv for lv in levels)), flush=True)
    if a.dry_run:
        return
    asyncio.run(Sweep(a.url, a.model, a.stream).run(levels))


if __name__ == "__main__":
    main()
