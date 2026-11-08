#!/usr/bin/env python3
"""Serve a captured panels.json back over the Prometheus HTTP API.

Grafana does not know it is talking to a file. It asks for query_range, this
answers from the snapshot, and the dashboard renders exactly as it would
against the live cluster -- which is the point: the images are the dashboard,
not a redrawing of it, and they can be produced months later with no cluster.

Stdlib only, so it runs in the same interpreters as the capture step.

Matching is by query STRING. Grafana sends the same expr the dashboard holds,
and capture stored it under the rewritten (namespace-targeted) form, so both
forms are indexed.

  promshim.py --snapshot RUNDIR/snapshot/panels.json --port 9099
"""

import argparse
import json
import re
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import parse_qs, urlparse

SNAPSHOT = {}
INDEX = {}


def normalise(expr):
    """Key that survives whitespace AND the namespace matcher's spelling.

    Grafana reflows newlines in an expr, and the dashboard's namespace is a
    template variable -- so the SAME panel arrives as any of

        namespace=~"$namespace"      (the dashboard's literal text)
        namespace=~"team-a" (Grafana, after interpolation)
        namespace="team-a"  (what capture rewrote it to)

    Matching on the exact string would miss two of the three and render an empty
    panel that looks like a quiet run. A snapshot covers ONE namespace, so
    collapsing the matcher loses nothing.
    """
    text = re.sub(r"\s+", " ", (expr or "").strip())
    # The label is a variable too ($namespace_label), and interpolates to either
    # namespace or exported_namespace depending on how the metric was relabelled
    # -- so collapse the whole matcher, label included.
    text = re.sub(r'(?:\$namespace_label|\w*namespace)\s*=~?\s*"[^"]*"',
                  'namespace=<NS>', text)
    # A filter left on "All" constrains nothing, and each side spells that
    # differently: the dashboard says variant_name=~"$variant", capture writes
    # `.*`, and Grafana -- whose variable has no options here, because the shim
    # serves no label values -- sends the empty alternation `()`. Eight panels of
    # the operational dashboard rendered empty over that spelling difference with
    # the data sitting in the snapshot.
    text = re.sub(r'(\w+)\s*=~\s*"(?:\.\*|\.\+|\(\)|\$\w+|)"', r'\1=<ALL>', text)
    # Range windows are compared in seconds, because the same window is spelled
    # differently at each end: a panel asking for $__rate_interval reaches capture
    # as `60s` and Grafana as `1m0s`.
    return re.sub(r"\[(\d+[smhd](?:\d+[smhd])*)\]",
                  lambda m: "[%ds]" % duration_seconds(m.group(1)), text)


def duration_seconds(text):
    unit = {"s": 1, "m": 60, "h": 3600, "d": 86400}
    return sum(int(value) * unit[suffix]
               for value, suffix in re.findall(r"(\d+)([smhd])", text))


def build_index(snapshot):
    index = {}
    for panel in snapshot.get("panels", []):
        for entry in panel.get("series", []):
            for key in (entry.get("expr"), entry.get("original_expr")):
                if key:
                    index[normalise(key)] = entry.get("result", [])
    return index


def say(message):
    """Print and FLUSH.

    render.sh redirects this to shim.log, and Python block-buffers a redirected
    stdout -- so without the flush the "NO MATCH" line, the one thing that
    distinguishes "the run was quiet" from "Grafana asked something the snapshot
    does not hold", never reaches the file.
    """
    print(message, flush=True)


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):  # quieter than the default
        say("  shim: " + fmt % args)

    def _json(self, payload, status=200):
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):  # noqa: N802 - BaseHTTPRequestHandler's name
        parsed = urlparse(self.path)
        params = parse_qs(parsed.query)
        self._route(parsed.path, params)

    def do_POST(self):  # noqa: N802 - Grafana POSTs query_range
        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length).decode() if length else ""
        parsed = urlparse(self.path)
        params = parse_qs(body) or parse_qs(parsed.query)
        self._route(parsed.path, params)

    def _route(self, path, params):
        # Grafana probes this on datasource save/health.
        if path.endswith("/api/v1/status/buildinfo"):
            return self._json({"status": "success",
                               "data": {"version": "2.53.0", "features": {}}})

        if path.endswith("/api/v1/query_range") or path.endswith("/api/v1/query"):
            expr = normalise((params.get("query") or [""])[0])
            result = INDEX.get(expr)
            if result is None:
                # Loudly, in the shim's log: a silent empty result is
                # indistinguishable from a quiet period on the chart.
                say("  shim: NO MATCH for %r" % expr[:90])
                result = []
            return self._json({"status": "success",
                               "data": {"resultType": "matrix", "result": result}})

        if path.endswith("/api/v1/labels") or path.endswith("/api/v1/metadata"):
            return self._json({"status": "success", "data": []})

        if path.endswith("/-/healthy") or path.endswith("/-/ready"):
            self.send_response(200)
            self.end_headers()
            return self.wfile.write(b"ok")

        return self._json({"status": "success", "data": []})


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--snapshot", required=True)
    parser.add_argument("--port", type=int, default=9099)
    args = parser.parse_args()

    global SNAPSHOT, INDEX
    with open(args.snapshot, encoding="utf-8") as handle:
        SNAPSHOT = json.load(handle)
    INDEX = build_index(SNAPSHOT)

    window = SNAPSHOT.get("window", {})
    say("serving %d queries from %s" % (len(INDEX), args.snapshot))
    say("window: %s -> %s" % (window.get("start"), window.get("end")))
    HTTPServer(("0.0.0.0", args.port), Handler).serve_forever()


if __name__ == "__main__":
    main()
