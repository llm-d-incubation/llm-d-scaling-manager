#!/usr/bin/env python3
"""Negative controls for the PodMonitor cloning added to add_variant.py.

Offline: these assert the selection and cloning rules without a cluster, which
is what the cluster run could not do -- it only ever exercised the happy path.
"""
import importlib.util
import os
import sys

spec = importlib.util.spec_from_file_location(
    "av", os.path.join(os.path.dirname(os.path.abspath(__file__)), "benchmark", "add_variant.py"))
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)

CHART_PM = {
    "metadata": {"name": "mdl-decode-podmonitor",
                 "labels": {"release": "llmd", "helm.sh/chart": "x",
                            "app.kubernetes.io/managed-by": "Helm"}},
    "spec": {
        "selector": {"matchLabels": {
            "llm-d.ai/inference-serving": "true",
            "llm-d.ai/inferenceServing": "true",
            "llm-d.ai/model": "mdl", "llm-d.ai/role": "decode"}},
        "podMetricsEndpoints": [{"port": "metrics", "path": "/metrics", "interval": "30s"}],
    },
}
# What a previous run of this script left behind.
VARIANT_PM = {
    "metadata": {"name": "mdl-decode-podmonitor-v2", "labels": {"release": "llmd"}},
    "spec": {
        "selector": {"matchLabels": {
            "llm-d.ai/inferenceServing": "true", "llm-d.ai/model": "mdl",
            "llm-d.ai/role": "decode", "scaling-manager.llmd.ai/variant": "v2"}},
        "podMetricsEndpoints": [{"port": "metrics"}],
    },
}

fails = []


def check(name, cond, detail=""):
    print(("  ok   " if cond else "  FAIL ") + name + (("  " + detail) if detail and not cond else ""))
    if not cond:
        fails.append(name)


# --- cloning -------------------------------------------------------------
pm = m.make_secondary_podmonitor(CHART_PM, {"suffix": "v3"}, "ns")
sel = pm["spec"]["selector"]["matchLabels"]
check("inherits the chart's port (never guessed)",
      pm["spec"]["podMetricsEndpoints"][0]["port"] == "metrics")
check("drops the primary's kebab discriminator",
      "llm-d.ai/inference-serving" not in sel, str(sel))
check("selects the new variant", sel.get("scaling-manager.llmd.ai/variant") == "v3")
check("keeps the model label", sel.get("llm-d.ai/model") == "mdl")
check("keeps release label (Prometheus selects PodMonitors by it)",
      pm["metadata"]["labels"].get("release") == "llmd")
check("drops helm ownership labels",
      not any(k.startswith("helm.sh/") for k in pm["metadata"]["labels"])
      and "app.kubernetes.io/managed-by" not in pm["metadata"]["labels"])

# A PodMonitor with no endpoints cannot be cloned into anything useful.
check("refuses a PodMonitor with no endpoints",
      m.make_secondary_podmonitor(
          {"metadata": {"name": "x", "labels": {}}, "spec": {"selector": {}}},
          {"suffix": "v2"}, "ns") is None)

# --- selection -----------------------------------------------------------
m.kubectl = lambda *a, **k: __import__("json").dumps(
    {"items": [VARIANT_PM, CHART_PM]})          # variant listed FIRST
got = m.find_primary_podmonitor("ns", "mdl-decode")
check("never clones a previous variant's PodMonitor",
      got is not None and got["metadata"]["name"] == "mdl-decode-podmonitor",
      "picked " + (got["metadata"]["name"] if got else "None"))

m.kubectl = lambda *a, **k: __import__("json").dumps({"items": [VARIANT_PM]})
check("returns None when only variant copies exist",
      m.find_primary_podmonitor("ns", "mdl-decode") is None)

renamed = {"metadata": {"name": "totally-different", "labels": {}},
           "spec": {"selector": {"matchLabels": {"llm-d.ai/model": "mdl"}},
                    "podMetricsEndpoints": [{"port": "metrics"}]}}
m.kubectl = lambda *a, **k: __import__("json").dumps({"items": [renamed]})
check("falls back to a renamed PodMonitor by model label",
      m.find_primary_podmonitor("ns", "mdl-decode", "mdl") is not None)
check("no fallback without a model hash",
      m.find_primary_podmonitor("ns", "mdl-decode") is None)

m.kubectl = lambda *a, **k: ""
check("survives kubectl failing", m.find_primary_podmonitor("ns", "d") is None)
m.kubectl = lambda *a, **k: "not json"
check("survives unparseable output", m.find_primary_podmonitor("ns", "d") is None)

print()
print("FAILED: %s" % ", ".join(fails) if fails else "all checks passed")
sys.exit(1 if fails else 0)
