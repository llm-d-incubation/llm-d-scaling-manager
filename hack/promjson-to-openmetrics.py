#!/usr/bin/env python3
"""Turn Prometheus query answers saved as JSON into OpenMetrics text.

The capacity planner's manual recording path
(docs/guides/record-for-capacity-planner/) saves raw samples with curl: one
instant query `metric{namespace="NS"}[RANGE]` per metric and time chunk, each
answer a JSON file with resultType "matrix". This script merges those files
into one OpenMetrics file, which `promtool tsdb create-blocks-from openmetrics`
turns into TSDB blocks for a local Prometheus. `whatif capture` then reads the
window from that Prometheus as it would from the original.

Chunks overlap at their edges (a range selector includes its end, and in
Prometheus 2 its start too), so a sample read twice is written once. A file
whose answer is not a success, or not a matrix, stops the conversion: a
failed chunk dropped quietly would read as a window with no load. Files with
an empty result are fine, a metric the fleet does not export. No sample at all
is an error.

Usage:
    promjson-to-openmetrics.py [-o OUT] FILE.json...
"""

import argparse
import decimal
import json
import sys


def escape(value):
    """Escape a label value for the OpenMetrics text format."""
    return value.replace("\\", "\\\\").replace("\n", "\\n").replace('"', '\\"')


def load(path, series):
    """Add the samples of one saved answer to series, keyed by label set."""
    with open(path, encoding="utf-8") as f:
        try:
            doc = json.load(f, parse_float=decimal.Decimal)
        except json.JSONDecodeError as e:
            raise ValueError(f"{path}: not JSON ({e})") from None
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
        labels = tuple(sorted((k, v) for k, v in metric.items() if k != "__name__"))
        samples = series.setdefault((name, labels), {})
        for ts, value in s.get("values") or []:
            samples[decimal.Decimal(str(ts))] = value
    return len(data.get("result") or [])


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


def main(argv):
    p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    p.add_argument("-o", "--output", help="the OpenMetrics file to write (default: stdout)")
    p.add_argument("files", nargs="+", help="saved /api/v1/query answers")
    args = p.parse_args(argv)
    series = {}
    try:
        for path in args.files:
            load(path, series)
    except (OSError, ValueError) as e:
        print(f"promjson-to-openmetrics: {e}", file=sys.stderr)
        return 1
    if not series:
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
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
