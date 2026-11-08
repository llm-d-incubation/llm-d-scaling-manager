#!/usr/bin/env python3
"""Fail when the tenant Role has drifted from the generated ClusterRole.

`config/components/tenant-installable/manager-role.yaml` is the controller's
permissions as a NAMESPACED Role, and it is maintained BY HAND as
"manager-clusterrole.yaml with the cluster-scoped resources removed". The
ClusterRole is generated from kubebuilder markers, so it moves whenever a
controller gains a permission; the Role does not move with it.

It drifted, and the drift was invisible. `pods` gained `patch` for the warm
pool -- the controller adds a borrowed Pod to a model's InferencePool and takes
it out again -- but only in the generated half. On every namespace-scoped
install the controller then started, read its own permissions, and disabled the
pool:

    the warm pool is disabled: this controller may not patch Pods, so it could
    never lend one

which is the right refusal and the wrong reason: the guide says the shipped RBAC
grants it. Nothing failed at install time, no manifest was invalid, and the pool
was created and held its accelerators.

So: every namespaced (apiGroup, resource) the ClusterRole grants must be granted
by the Role with AT LEAST the same verbs. The Role may grant more (it does:
`patch` on pods, which the ClusterRole leaves to role/scaling-manager-pod-writer per namespace),
and it may omit the cluster-scoped resources listed below, which are the only
ones a Role cannot carry.
"""

import sys
from pathlib import Path

try:
    import yaml
except ImportError:  # pragma: no cover
    print("ERROR: PyYAML is required. pip install pyyaml", file=sys.stderr)
    sys.exit(1)

ROOT = Path(__file__).resolve().parent.parent
CLUSTER_ROLE = ROOT / "config" / "base" / "rbac" / "manager-clusterrole.yaml"
TENANT_ROLE = ROOT / "config" / "components" / "tenant-installable" / "manager-role.yaml"

# The only resources a Role legitimately cannot carry. Each is documented in the
# tenant Role's own header, with why an install without it still works.
CLUSTER_SCOPED = {
    ("", "nodes"),
    ("", "nodes/status"),
    ("", "namespaces"),
    # Kueue ClusterQueues and ResourceFlavors: read only by a quota entry with
    # kueue.enabled, granted by components/kueue-reader (prereqs phase). The
    # namespaced half, localqueues and workloads, is in the tenant Role and is
    # checked. Their cluster-wide read, which cohort borrowing needs, is NOT in
    # any component: the installer grants it only to a controller whose policy
    # counts borrowing (see NO_COMPONENT_GRANTS below).
    ("kueue.x-k8s.io", "clusterqueues"),
    ("kueue.x-k8s.io", "resourceflavors"),
    # Kueue Cohorts: listed to tell a flat cohort from a nested one, granted by
    # components/kueue-reader. Denied, borrowing reads hierarchy-unknown and is
    # not counted; the nominal Kueue bound is unaffected.
    ("kueue.x-k8s.io", "cohorts"),
    # The Kueue controller's leader-election Lease: namespaced, but in the
    # Kueue namespace, which the tenant Role cannot reach. The prereqs phase
    # grants it by name with a Role there (pl_grant_kueue_controller_read).
    # Denied, borrowing freezes as kueue-controller-down.
    ("coordination.k8s.io", "leases"),
}


def grants(path):
    """{(apiGroup, resource): set(verbs)} from a Role or ClusterRole document."""
    doc = yaml.safe_load(path.read_text(encoding="utf-8"))
    if not doc or "rules" not in doc:
        raise SystemExit("no rules in %s -- the parser found nothing, which would "
                         "compare equal to any other empty side." % path)
    out = {}
    for rule in doc["rules"] or []:
        for group in rule.get("apiGroups") or [""]:
            for resource in rule.get("resources") or []:
                out.setdefault((group, resource), set()).update(rule.get("verbs") or [])
    if not out:
        raise SystemExit("extracted no grants from %s" % path)
    return out


# Grants the tenant Role must carry that the ClusterRole no longer does, so the
# comparison above cannot see them go. A namespace-scoped controller has no
# role/scaling-manager-pod-writer: without `patch` on pods here its warm pool and the
# utilization-share optimizer cannot write a pod.
TENANT_REQUIRED = {
    ("", "pods"): {"patch"},
}


# Grants no ClusterRole in config/components may carry. Every component
# ClusterRole is rendered into the namespace-scoped overlays, so whatever one
# grants, every namespace-scoped install gets -- LLMSM_LIMITER=none included.
# Kueue Workloads and LocalQueues cluster-wide reach every tenant's Workloads,
# pod templates included; deploy/lib/physical_limiter.sh grants them
# (pl_grant_kueue_borrow_read) only when a policy counts cohort borrowing.
NO_COMPONENT_GRANTS = {
    ("kueue.x-k8s.io", "workloads"),
    ("kueue.x-k8s.io", "localqueues"),
}
COMPONENTS = ROOT / "config" / "components"


def component_cluster_role_problems():
    """Component ClusterRoles that carry a NO_COMPONENT_GRANTS resource."""
    problems = []
    checked = 0
    for path in sorted(COMPONENTS.glob("*/*.yaml")):
        for doc in yaml.safe_load_all(path.read_text(encoding="utf-8")):
            if not isinstance(doc, dict) or doc.get("kind") != "ClusterRole":
                continue
            checked += 1
            for rule in doc.get("rules") or []:
                for group in rule.get("apiGroups") or [""]:
                    for resource in rule.get("resources") or []:
                        if (group, resource) in NO_COMPONENT_GRANTS:
                            problems.append(
                                "  %s grants %s/%s cluster-wide to every namespace-scoped "
                                "install; only the installer may grant it, when a policy "
                                "counts Kueue borrowing" % (path.relative_to(ROOT), group, resource))
    if checked == 0:
        raise SystemExit("found no ClusterRole under %s -- the scan found nothing, "
                         "which would pass any grant." % COMPONENTS.relative_to(ROOT))
    return problems, checked


def main():
    cluster = grants(CLUSTER_ROLE)
    tenant = grants(TENANT_ROLE)

    comp_problems, comp_checked = component_cluster_role_problems()
    if comp_problems:
        print("ERROR: a component ClusterRole grants a read that must be conditional:",
              file=sys.stderr)
        print("\n".join(comp_problems), file=sys.stderr)
        return 1
    print("no component ClusterRole grants Kueue Workloads or LocalQueues (%d checked)"
          % comp_checked)

    problems = []
    for key, verbs in sorted(TENANT_REQUIRED.items()):
        missing = verbs - tenant.get(key, set())
        if missing:
            group, resource = key
            problems.append("  %s%s: the tenant Role must grant %s itself (no "
                            "role/scaling-manager-pod-writer is made for a namespace-scoped controller)"
                            % (group + "/" if group else "", resource, ",".join(sorted(missing))))
    for key, verbs in sorted(cluster.items()):
        if key in CLUSTER_SCOPED:
            continue
        have = tenant.get(key, set())
        missing = verbs - have
        if missing:
            group, resource = key
            problems.append("  %s%s: ClusterRole grants %s, tenant Role is missing %s"
                            % (group + "/" if group else "", resource,
                               ",".join(sorted(verbs)), ",".join(sorted(missing))))

    if problems:
        print("ERROR: the tenant Role has drifted from the generated ClusterRole:",
              file=sys.stderr)
        print("\n".join(problems), file=sys.stderr)
        print("\nAdd the verbs to %s, or -- if the resource really is cluster-scoped\n"
              "and an install without it still works -- list it in CLUSTER_SCOPED here\n"
              "and say why in that file's header." % TENANT_ROLE.relative_to(ROOT),
              file=sys.stderr)
        return 1

    print("tenant Role covers the ClusterRole (%d namespaced grants)"
          % (len(cluster) - len(CLUSTER_SCOPED & set(cluster))))
    return 0


if __name__ == "__main__":
    sys.exit(main())
