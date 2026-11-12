#!/usr/bin/env python3
"""Turn Prometheus query answers saved as JSON into OpenMetrics text.

The capacity planner's manual recording path
(docs/guides/record-for-capacity-planner/) saves raw samples with curl: one
instant query `metric{namespace="NS"}[RANGE]` per metric and time chunk, each
answer a JSON file named METRIC.CHUNKSTART.json with resultType "matrix". This
script merges those files into one OpenMetrics file, which
`promtool tsdb create-blocks-from openmetrics` turns into TSDB blocks for a
local Prometheus. `whatif capture` then reads the window from that Prometheus
as it would from the original.

Chunks overlap at their edges (a range selector includes its end, and in
Prometheus 2 its start too), so a sample read twice is written once.

Refused, because each would read as a window with less load than it had:
- a file whose answer is not a success (an error body curl saved), not JSON,
  or not a matrix;
- a chunk one metric has and another lacks (a request that saved no file);
- a native histogram sample, which OpenMetrics text cannot carry;
- answers with no sample at all.
Files with an empty result are fine: a metric the fleet does not export.

It prints the median interval between samples, the scrape interval, and the
lookback a local Prometheus should serve the blocks with: four times it.

Usage:
    promjson-to-openmetrics.py [-o OUT] FILE.json...
"""

import argparse
import decimal
import json
import os
import re
import statistics
import sys

# METRIC.CHUNKSTART.json, the name the guide's fetch loop gives each answer.
CHUNK_FILE = re.compile(r"^(?P<metric>[^.]+)\.(?P<start>\d+)\.json$")


def escape(value):
    """Escape a label value for the OpenMetrics text format."""
    return value.replace("\\", "\\\\").replace("\n", "\\n").replace('"', '\\"')


def load(path, series):
    """Add the samples of one saved answer to series, keyed by label set."""
    with open(path, encoding="utf-8", errors="replace") as f:
        try:
            doc = json.load(f, parse_float=decimal.Decimal)
        except json.JSONDecodeError as e:
            raise ValueError(f"{path}: not JSON, so not a query answer ({e})") from None
    if not isinstance(doc, dict) or doc.get("status") != "success":
        err = doc.get("error", "no status") if isinstance(doc, dict) else "not an object"
        raise ValueError(f"{path}: the query did not succeed: {err}")
    data = doc.get("data") or {}
    if data.get("resultType") != "matrix":
        raise ValueError(f"{path}: resultType is {data.get('resultType')!r}, not 'matrix'; "
                         "ask an instant query with a range selector, metric{...}[RANGE]")
    for s in data.get("result") or []:
        metric = s.get("metric") or {}
        name = metric.get("__name__")
        if not name:
            raise ValueError(f"{path}: a series has no __name__; select the metric by name, not by a function of it")
        if s.get("histograms"):
            raise ValueError(f"{path}: {name} has native histogram samples, which this converter cannot "
                             "write; use a TSDB snapshot for this window")
        labels = tuple(sorted((k, v) for k, v in metric.items() if k != "__name__"))
        samples = series.setdefault((name, labels), {})
        for ts, value in s.get("values") or []:
            samples[decimal.Decimal(str(ts))] = value


def check_chunks(paths):
    """Refuse a chunk some metric was saved for and another was not."""
    starts = {}
    for path in paths:
        m = CHUNK_FILE.match(os.path.basename(path))
        if not m:
            print(f"promjson-to-openmetrics: warning: {path} is not named METRIC.CHUNKSTART.json; "
                  "its chunks are not checked", file=sys.stderr)
            continue
        starts.setdefault(m["metric"], set()).add(int(m["start"]))
    every = set().union(*starts.values()) if starts else set()
    missing = sorted(f"{metric}.{start}" for metric, have in starts.items() for start in every - have)
    if missing:
        shown = ", ".join(missing[:5]) + (f" and {len(missing) - 5} more" if len(missing) > 5 else "")
        raise ValueError(f"{len(missing)} chunks have no file: {shown}; fetch them again")


def write(series, out):
    """Write series as OpenMetrics, grouped by name, each in time order."""
    n = 0
    for name, labels in sorted(series):
        rendered = ",".join(f'{k}="{escape(v)}"' for k, v in labels)
        head = f"{name}{{{rendered}}}" if rendered else name
        for ts in sorted(series[(name, labels)]):
            out.write(f"{head} {series[(name, labels)][ts]} {ts}\n")
            n += 1
    out.write("# EOF\n")
    return n


def scrape_interval(series):
    """The median interval between consecutive samples of a series, or None."""
    gaps = []
    for samples in series.values():
        ts = sorted(samples)
        gaps += [float(b - a) for a, b in zip(ts, ts[1:])]
    return statistics.median(gaps) if gaps else None


def main(argv):
    p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    p.add_argument("-o", "--output", help="the OpenMetrics file to write (default: stdout)")
    p.add_argument("files", nargs="+", help="saved /api/v1/query answers")
    args = p.parse_args(argv)
    series = {}
    try:
        check_chunks(args.files)
        for path in args.files:
            load(path, series)
    except (OSError, ValueError) as e:
        print(f"promjson-to-openmetrics: {e}", file=sys.stderr)
        return 1
    if not any(series.values()):
        print(f"promjson-to-openmetrics: no sample in {len(args.files)} files", file=sys.stderr)
        return 1
    if args.output:
        with open(args.output, "w", encoding="utf-8", newline="\n") as out:
            n = write(series, out)
    else:
        n = write(series, sys.stdout)
    names = len({name for name, _ in series})
    print(f"promjson-to-openmetrics: {n} samples, {len(series)} series, {names} metrics "
          f"from {len(args.files)} files", file=sys.stderr)
    interval = scrape_interval(series)
    if interval:
        print(f"promjson-to-openmetrics: scrape interval about {interval:g}s; "
              f"serve with --query.lookback-delta={4 * max(1, round(interval))}s", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
