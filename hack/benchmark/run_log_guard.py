#!/usr/bin/env python3
"""Fails a run whose window holds none of the lines its controller logs.

The zero-line guard of docs/proposals/analyzer-learner.md 6.1. Which line a
run must hold depends on the controller's -v:

  - 3 or higher: k2-decision, one per replica per cycle at V(3).
  - 2 (the shipped level): replica-capacity-summary, one per variant per
    cycle at V(2). k2-decision is not logged there, so a guard on it would
    fail every run at the shipped level, and no guard at all let a run with
    no controller lines read as analysed.
  - below 2: neither line is logged, so there is nothing to check. The guard
    says so and exits 3: not a pass, and not a failure of the run.

The verbosity is CONTROLLER_VERBOSITY when set, otherwise the -v of the
controller's Running Pods in the namespace (their manager args, the lowest of
them during a rollout; none named is the shipped default). A Failed, Evicted
or Succeeded Pod left by an earlier install is not the run's controller and
is not read. Read it promptly: a controller reinstalled since the run reports
its own -v, not the run's.

The log is the one dump_k2_decisions.py reads: the captured
llmsm_controller.log in the results dir, else the live controller log. The
window is harness_start to harness_stop from run_metadata.yaml.

Exit status: 0 when the window holds at least one line, 1 when it holds none,
2 when the verbosity, the window or the log could not be read, 3 when the
verbosity logs neither line and nothing was checked. An unknown verbosity is
not a pass.

Usage
-----
  python hack/benchmark/run_log_guard.py <results>/<treatment>_<i> -n NAMESPACE
"""
import argparse
import json
import os
import subprocess
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import dump_k2_decisions as dk  # noqa: E402
from controller_pods import CONTROLLER_LABEL, is_controller_pod, verbosity  # noqa: E402

# DEFAULT_LEVEL is the -v at which replica-capacity-summary is logged:
# logging.DEFAULT (saturation/replica_log.go). VERBOSE_LEVEL is the -v at
# which k2-decision is: perReplicaLogLevel, logging.VERBOSE.
DEFAULT_LEVEL = 2
VERBOSE_LEVEL = 3
SUMMARY_MSG = "replica-capacity-summary"
VERBOSITY_ENV = "CONTROLLER_VERBOSITY"

EXIT_LINES, EXIT_NONE, EXIT_UNREADABLE, EXIT_NOT_CHECKED = 0, 1, 2, 3


class Unreadable(Exception):
    """Something the guard needs could not be read."""


def run_verbosity(namespace):
    """Returns (verbosity, where it was read) or raises Unreadable."""
    env = os.environ.get(VERBOSITY_ENV, "")
    if env:
        try:
            v = int(env)
        except ValueError:
            raise Unreadable(f"{VERBOSITY_ENV}={env!r} is not an integer") from None
        return v, f"{VERBOSITY_ENV}={env}"
    cmd = ["kubectl", "get", "pods", "-n", namespace, "-l", CONTROLLER_LABEL, "-o", "json"]
    proc = subprocess.run(cmd, capture_output=True, text=True)
    if proc.returncode != 0:
        raise Unreadable(f"{' '.join(cmd)} failed (exit {proc.returncode}): {proc.stderr.strip()}")
    try:
        pods = [p for p in json.loads(proc.stdout).get("items") or [] if is_controller_pod(p)]
    except (json.JSONDecodeError, AttributeError) as e:
        raise Unreadable(f"{' '.join(cmd)} did not return a Pod list: {e}") from None

    def name(p):
        return p["metadata"].get("name") or "?"

    def phase(p):
        return (p.get("status") or {}).get("phase") or "-"

    running = [p for p in pods if phase(p) == "Running"]
    if not running:
        listed = ", ".join(f"{name(p)} {phase(p)}" for p in pods) or "none"
        raise Unreadable(f"no Running controller Pod in namespace {namespace!r} (listed: {listed})")
    levels = {}
    for p in running:
        try:
            levels[name(p)] = verbosity(p)
        except ValueError as e:
            raise Unreadable(f"Pod {name(p)}: {e}") from None
    lowest = min(levels.values())
    named = ", ".join(f"{n} --v={v}" for n, v in sorted(levels.items()))
    return lowest, f"Running controller Pods: {named}"


def window(results_dir):
    """Returns (start, stop) from run_metadata.yaml or raises Unreadable."""
    meta_path = results_dir / "run_metadata.yaml"
    if not meta_path.is_file():
        raise Unreadable(f"run_metadata.yaml not found in {results_dir}")
    try:
        meta = dk.yaml.safe_load(meta_path.read_text())
        start, stop = dk.parse_iso(meta["harness_start"]), dk.parse_iso(meta["harness_stop"])
    except (dk.yaml.YAMLError, KeyError, TypeError, ValueError) as e:
        raise Unreadable(f"{meta_path} has no readable harness_start/harness_stop: {e}") from None
    if start.tzinfo is None or stop.tzinfo is None:
        raise Unreadable(f"{meta_path}: harness_start/harness_stop carry no time zone")
    return start, stop


def count_lines(logs, message, start, stop):
    """Counts message's lines stamped inside [start, stop], or raises Unreadable."""
    n = 0
    for line in logs.splitlines():
        m = dk.LOG_LINE.match(line)
        if not m or m.group("msg") != message:
            continue
        try:
            ts = dk.parse_iso(m.group("ts"))
            inside = start <= ts <= stop
        except (TypeError, ValueError) as e:
            raise Unreadable(f"{message} line stamped {m.group('ts')!r}: {e}") from None
        if inside:
            n += 1
    return n


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("results_dir", help="Path to .../results/<treatment>_<i>")
    ap.add_argument("-n", "--namespace", required=True)
    args = ap.parse_args()
    rd = Path(args.results_dir).resolve()

    def unreadable(e):
        print(f"ERROR: run-log guard: could not read what the run must hold: {e}. "
              f"Set {VERBOSITY_ENV}=<the run's --v> if the controller is gone.", file=sys.stderr)
        return EXIT_UNREADABLE

    try:
        level, where = run_verbosity(args.namespace)
    except Unreadable as e:
        return unreadable(e)
    if level < DEFAULT_LEVEL:
        print(f"run-log guard: nothing to check at --v={level} ({where}): neither {SUMMARY_MSG} "
              f"(--v={DEFAULT_LEVEL}) nor {dk.K2_MSG} (--v={VERBOSE_LEVEL}) is logged there. "
              "The run window was NOT checked.")
        return EXIT_NOT_CHECKED
    message = dk.K2_MSG if level >= VERBOSE_LEVEL else SUMMARY_MSG

    try:
        start, stop = window(rd)
    except Unreadable as e:
        return unreadable(e)
    since = int((dk.datetime.now(dk.timezone.utc) - start).total_seconds()) + 90
    try:
        logs = dk.fetch_logs(args.namespace, since, results_dir=rd)
    except SystemExit:
        print("ERROR: run-log guard: could not read the controller log (above).", file=sys.stderr)
        return EXIT_UNREADABLE
    try:
        n = count_lines(logs, message, start, stop)
    except Unreadable as e:
        return unreadable(e)

    span = f"{start.isoformat()} -> {stop.isoformat()}"
    if n == 0:
        print(f"ERROR: run-log guard: zero {message} lines in the run window {span}, "
              f"expected at --v={level} ({where}). The log does not cover the run, or "
              "the controller did not analyse it.", file=sys.stderr)
        return EXIT_NONE
    print(f"run-log guard: {n} {message} lines in the run window {span} at --v={level} ({where})")
    return EXIT_LINES


if __name__ == "__main__":
    sys.exit(main())
