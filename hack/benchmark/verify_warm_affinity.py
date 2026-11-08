"""Does the rendered warmAffinity actually pull requesters onto launcher nodes?

The claim `fma.warmAffinity` rests on is that a *preferred* podAffinity is a
strong enough signal to beat the scheduler's default spreading. That is a claim
about behaviour, not about YAML, and it cannot be checked by reading the
template -- so this runs both arms against a real cluster and compares.

The affinity under test is EXTRACTED FROM THE RENDERED TEMPLATE, not retyped, so
what runs is the artifact patch_harness.sh actually produces.

Setup mirrors the shared GPU cluster in miniature: launchers occupy a strict SUBSET of the
eligible nodes, and the requester is eligible everywhere. Measured on the shared GPU cluster,
that ratio is 5 launcher nodes out of 14 GPU nodes -- so an unconstrained
requester lands beside warm capacity about a third of the time, and worse in
practice because default spreading pushes it away from the nodes already running
launchers. Here the ratio is 1 of 3 and the control arm reproduces exactly that
scatter.

Needs any cluster with >= 3 schedulable nodes; no GPUs and no FMA install. Runs
in its own namespace and deletes it afterwards.

  make benchmark-verify-warm-affinity

Requires python3 with jinja2 + PyYAML (both are llm-d-benchmark dependencies)
and an llm-d-benchmark clone patched by `make benchmark-patch`.
"""
import copy
import json
import subprocess
import sys

import jinja2
import yaml

NS = "fma-affinity-test"
ROOT = "llm-d-benchmark/config/templates"
IMAGE = "registry.k8s.io/pause:3.9"   # never docker.io (AGENTS.md)
REPLICAS = 6


def kc(*args, check=True, stdin=None):
    p = subprocess.run(["kubectl", *args], capture_output=True, text=True,
                       input=stdin)
    if check and p.returncode != 0:
        sys.exit("kubectl %s failed:\n%s" % (" ".join(args), p.stderr))
    return p.stdout


def rendered_affinity():
    """The requester affinity block, straight out of the patched template."""
    base = yaml.safe_load(open(ROOT + "/values/defaults.yaml", encoding="utf-8"))
    base.setdefault("namespace", {})["name"] = NS
    base["model_id_label"] = "testmodel"
    base.setdefault("model", {})["name"] = "test/model"
    base["scenarioName"] = "test"
    base.setdefault("decode", {})["acceleratorType"] = {
        "labelKey": "nvidia.com/gpu.product",
        "labelValue": "NVIDIA-H100-80GB-HBM3"}
    ctx = copy.deepcopy(base)
    ctx["fma"]["enabled"] = True
    ctx["fma"]["requester"]["replicas"] = REPLICAS
    ctx["fma"]["launcherNodeSelection"] = {"enabled": False}
    ctx["fma"]["warmAffinity"] = {"enabled": True}
    src = open(ROOT + "/jinja/24_fma-deployment.yaml.j2", encoding="utf-8").read()
    env = jinja2.Environment(undefined=jinja2.ChainableUndefined)
    out = env.from_string(src).render(**ctx)
    for d in yaml.safe_load_all(out):
        if d and d.get("kind") == "Deployment" and "requester" in d["metadata"]["name"]:
            aff = d["spec"]["template"]["spec"].get("affinity") or {}
            # An unpatched clone renders no podAffinity at all. Say so, rather
            # than running the experiment and reporting a scheduling failure for
            # what is really a missing patch.
            if not aff.get("podAffinity"):
                sys.exit(
                    "the rendered template has no podAffinity: this clone is not "
                    "patched.\nRun `make benchmark-patch` (fix 3) and try again.")
            # Drop the GPU nodeAffinity: kind nodes carry no accelerator labels,
            # and it is not what is under test here.
            aff.pop("nodeAffinity", None)
            return aff
    sys.exit("no requester Deployment in the rendered template")


def launcher_pod(name, node, sleeping):
    labels = {"app.kubernetes.io/component": "launcher"}
    if sleeping:
        labels["dual-pods.llm-d.ai/sleeping"] = "true"
    return {"apiVersion": "v1", "kind": "Pod",
            "metadata": {"name": name, "namespace": NS, "labels": labels},
            "spec": {"nodeName": node,
                     "containers": [{"name": "c", "image": IMAGE}]}}


def requester_deploy(affinity):
    spec = {"containers": [{"name": "c", "image": IMAGE}]}
    if affinity:
        spec["affinity"] = affinity
    return {"apiVersion": "apps/v1", "kind": "Deployment",
            "metadata": {"name": "requester", "namespace": NS},
            "spec": {"replicas": REPLICAS,
                     "selector": {"matchLabels": {"llm-d.ai/role": "requester"}},
                     "template": {"metadata": {"labels": {"llm-d.ai/role": "requester"}},
                                  "spec": spec}}}


def placement():
    out = kc("get", "pods", "-n", NS, "-l", "llm-d.ai/role=requester", "-o", "json")
    counts = {}
    for p in json.loads(out).get("items", []):
        node = p["spec"].get("nodeName")
        if node:
            counts[node] = counts.get(node, 0) + 1
    return counts


def run_arm(label, affinity, warm_node):
    kc("delete", "deployment", "requester", "-n", NS, "--ignore-not-found")
    kc("wait", "--for=delete", "pod", "-l", "llm-d.ai/role=requester",
       "-n", NS, "--timeout=90s", check=False)
    kc("apply", "-f", "-", stdin=yaml.safe_dump(requester_deploy(affinity)))
    kc("rollout", "status", "deployment/requester", "-n", NS, "--timeout=120s")
    counts = placement()
    on_warm = counts.get(warm_node, 0)
    print("  %-22s %s" % (label, counts))
    print("  %-22s %d/%d on the launcher node" % ("", on_warm, REPLICAS))
    return on_warm


nodes = kc("get", "nodes", "-o",
           "jsonpath={.items[*].metadata.name}").split()
if len(nodes) < 3:
    sys.exit("need >= 3 schedulable nodes, found %d" % len(nodes))
warm_node = nodes[0]
print("nodes: %s" % nodes)
print("launcher node (the only one): %s\n" % warm_node)

kc("create", "namespace", NS, check=False)
try:
    # Two sleeping launchers on ONE node; the other nodes hold none.
    for i in range(2):
        kc("apply", "-f", "-",
           stdin=yaml.safe_dump(launcher_pod("launcher-%d" % i, warm_node, True)))
    kc("wait", "--for=condition=Ready", "pod", "-l",
       "app.kubernetes.io/component=launcher", "-n", NS, "--timeout=120s")

    aff = rendered_affinity()
    print("affinity under test (from the rendered template):")
    print("  weights: %s" % [t["weight"] for t in
                             aff["podAffinity"][
                                 "preferredDuringSchedulingIgnoredDuringExecution"]])
    print()

    print("control arm -- no affinity (today's behaviour):")
    without = run_arm("no affinity", None, warm_node)
    print()
    print("treatment arm -- rendered warmAffinity:")
    with_aff = run_arm("warmAffinity", aff, warm_node)
    print()

    if with_aff > without and with_aff >= REPLICAS * 0.5:
        print("PASS: affinity pulled %d/%d onto the launcher node (control: %d/%d)"
              % (with_aff, REPLICAS, without, REPLICAS))
        rc = 0
    else:
        print("FAIL: affinity did not improve colocation "
              "(with=%d, without=%d, of %d)" % (with_aff, without, REPLICAS))
        rc = 1
finally:
    kc("delete", "namespace", NS, "--ignore-not-found", "--wait=false", check=False)
    print("(namespace %s deleted)" % NS)

sys.exit(rc)
