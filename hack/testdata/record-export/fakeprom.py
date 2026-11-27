#!/usr/bin/env python3
"""A Prometheus query API for hack/check-record-export.sh.

Answers /api/v1/query the way Prometheus answers the manual recording's
instant query for a range, NAME{namespace='NS'}[RANGEs] at TIME: a matrix of
one series of samples every 15 s for each name in --present, in the
namespace --namespace (--lmcache-namespace for lmcache_mp_ names; DCGM_
names hold it in exported_namespace, not namespace), and an
empty matrix for any other name, as a real Prometheus answers a series it
does not hold. A prefix selector, {__name__=~'PREFIX.+',namespace='NS'},
answers every --present name it starts. A name in --fail answers with a Prometheus error and HTTP 422.
It writes the port it listens on to --port-file, then serves until killed.
"""

import argparse
import json
import re
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import parse_qs, urlparse

SELECTOR = re.compile(r"^(?P<name>[A-Za-z_:][A-Za-z0-9_:]*)?(\{(?P<matchers>[^{}]*)\})?\[(?P<range>\d+)s\]$")
MATCHER = re.compile(r"^(?P<label>[A-Za-z_][A-Za-z0-9_]*)(?P<op>=~|=)'(?P<value>[^']*)'$")
# The labels that name a Pod's namespace: an engine's own, and a DCGM
# series' exported one.
NS_LABELS = ("namespace", "exported_namespace")


def parse(query):
    """The names a selector asks for, and the namespace label and value it
    selects on: NAME{namespace='NS'}, NAME{exported_namespace='NS'}, or a
    name prefix {__name__=~'PREFIX.+',namespace='NS'}. None if it is none of
    these."""
    m = SELECTOR.match(query)
    if not m:
        return None
    name, prefix, ns_label, ns = m["name"], None, None, None
    for part in (m["matchers"] or "").split(","):
        if not part:
            continue
        mm = MATCHER.match(part)
        if not mm:
            return None
        if mm["label"] == "__name__" and mm["op"] == "=~" and mm["value"].endswith(".+") and name is None:
            prefix = mm["value"][:-2]
        elif mm["label"] in NS_LABELS and mm["op"] == "=":
            ns_label, ns = mm["label"], mm["value"]
        else:
            return None
    if (name is None) == (prefix is None):
        return None
    return name, prefix, ns_label, ns, int(m["range"])


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--present", default="")
    ap.add_argument("--fail", default="")
    ap.add_argument("--namespace", required=True)
    ap.add_argument("--lmcache-namespace", required=True)
    ap.add_argument("--port-file", required=True)
    args = ap.parse_args()
    present = set(args.present.split())
    fail = set(args.fail.split())

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def reply(self, code, body):
            data = json.dumps(body).encode()
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def do_GET(self):
            u = urlparse(self.path)
            q = parse_qs(u.query)
            if u.path != "/api/v1/query":
                self.reply(404, {"status": "error", "errorType": "not_found", "error": u.path})
                return
            sel = parse(q.get("query", [""])[0])
            if sel is None:
                self.reply(400, {"status": "error", "errorType": "bad_data", "error": "unexpected query"})
                return
            name, prefix, ns_label, ns, rng = sel
            if (name or prefix) in fail:
                self.reply(422, {"status": "error", "errorType": "execution",
                                 "error": "query processing would load too many samples into memory"})
                return
            names = [name] if prefix is None else sorted(n for n in present if n.startswith(prefix))
            result = []
            for n in names:
                want_ns = args.lmcache_namespace if n.startswith("lmcache_mp_") else args.namespace
                # A DCGM series holds the GPU's Pod namespace in
                # exported_namespace only.
                want_label = "exported_namespace" if n.startswith("DCGM_") else "namespace"
                if n not in present or (ns is not None and (ns != want_ns or ns_label != want_label)):
                    continue
                end = float(q["time"][0])
                start = end - rng
                values = []
                t = start + 15
                while t <= end:
                    values.append([t, str(int(t - start))])
                    t += 15
                labels = {"__name__": n, "pod": "m-dec-0"}
                if ns is not None:
                    labels[ns_label] = ns
                result.append({"metric": labels, "values": values})
            self.reply(200, {"status": "success", "data": {"resultType": "matrix", "result": result}})

    srv = HTTPServer(("127.0.0.1", 0), Handler)
    with open(args.port_file, "w") as f:
        f.write(str(srv.server_address[1]))
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass
    return 0


if __name__ == "__main__":
    sys.exit(main())
