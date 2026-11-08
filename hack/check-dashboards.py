#!/usr/bin/env python3
"""Check the Grafana dashboards under deploy/grafana/ for defects a reader cannot see.

Grafana repacks a dashboard on load, so a broken layout still displays -- just not
the layout the JSON describes. Nine overlapping panel pairs sat in the operational
dashboard for months that way, each new panel appended at coordinates that
collided with one already there. Nothing failed; the dashboard simply showed an
arbitrary arrangement.

The same blindness covers the namespace labels. llmsm_* series carry the workload
namespace as `exported_namespace` and the controller's as `namespace`, so the
dashboards offer a `$namespace_label` variable to choose between them. Engine and
EPP series carry only a plain `namespace`, so using the variable there empties the
panel for whoever picks the other value -- and dropping the matcher altogether
plots every tenant on the cluster, which is how the EPP queue panel came to draw
three models for a namespace that serves one.

Checked here because none of it needs a cluster, a datasource or a browser.
"""
import json
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
DASHBOARDS = sorted((ROOT / "deploy" / "grafana").glob("*-dashboard.json"))

GRID_COLUMNS = 24

# Series whose namespace label is the WORKLOAD's, chosen by the reader.
LLMSM = re.compile(r"^llmsm_")
# Series emitted by the EPP or the engines. These carry a plain `namespace` only.
WORKLOAD = re.compile(r"^(llm_d_epp_|inference_|vllm:|sglang:)")
METRIC = re.compile(r"\b(llmsm_[a-z0-9_]+|llm_d_epp_[a-z0-9_]+|inference_[a-z0-9_]+)\b|(vllm:[a-z0-9_:]+|sglang:[a-z0-9_:]+)")
LABEL_NAME = re.compile(r"([$\w]+)\s*(?:=~|!~|=|!=)")


def panels_of(dashboard):
    """Every panel, rows included, as (panel, siblings-key) pairs."""
    for panel in dashboard.get("panels", []):
        yield panel, "top"
        for child in panel.get("panels") or []:
            yield child, f"row:{panel.get('title', panel.get('id'))}"


def rows_of(dashboard):
    """Every panel with the row it belongs to, or None.

    A row that is EXPANDED holds nothing: Grafana leaves its panels in the
    top-level list and membership is "everything until the next row". Only a
    COLLAPSED row nests its children. Both spellings mean the same thing to a
    reader, so both have to mean the same thing here.
    """
    current = None
    for panel in dashboard.get("panels", []):
        if panel.get("type") == "row":
            current = panel.get("title")
            for child in panel.get("panels") or []:
                yield child, current
            continue
        yield panel, current


def overlaps(a, b):
    ax, ay, aw, ah = (a["gridPos"][k] for k in "xywh")
    bx, by, bw, bh = (b["gridPos"][k] for k in "xywh")
    return ax < bx + bw and bx < ax + aw and ay < by + bh and by < ay + ah


def check_layout(name, dashboard, fail):
    groups = {}
    for panel, group in panels_of(dashboard):
        pos = panel.get("gridPos")
        title = panel.get("title") or f"id {panel.get('id')}"
        if not pos or not all(k in pos for k in "xywh"):
            fail(f"NO GRIDPOS    {name}: {title}")
            continue
        if pos["x"] < 0 or pos["y"] < 0 or pos["w"] < 1 or pos["h"] < 1:
            fail(f"BAD GRIDPOS   {name}: {title} {pos}")
            continue
        if pos["x"] + pos["w"] > GRID_COLUMNS:
            fail(f"OFF GRID      {name}: {title} ends at column {pos['x'] + pos['w']} of {GRID_COLUMNS}")
        groups.setdefault(group, []).append(panel)

    for group, members in groups.items():
        for i, a in enumerate(members):
            for b in members[i + 1:]:
                if overlaps(a, b):
                    fail(f"OVERLAP       {name} [{group}]: "
                         f"{a.get('title')} {a['gridPos']} vs {b.get('title')} {b['gridPos']}")


def check_identity(name, dashboard, fail):
    """Ids address a panel for rendering; titles name the file it renders to."""
    seen_ids, seen_titles = {}, {}
    for panel, _ in panels_of(dashboard):
        pid, title = panel.get("id"), panel.get("title")
        if pid is None:
            fail(f"NO ID         {name}: {title}")
        elif pid in seen_ids:
            fail(f"DUPLICATE ID  {name}: {title} and {seen_ids[pid]} are both id {pid}")
        else:
            seen_ids[pid] = title
        if panel.get("type") == "row":
            continue
        if not title:
            fail(f"NO TITLE      {name}: panel id {pid}")
        elif title.lower() in seen_titles:
            fail(f"DUPLICATE     {name}: two panels titled {title!r} render to one image")
        else:
            seen_titles[title.lower()] = pid


def selectors(expr):
    """Yield (metric, label-names) for every metric reference in a PromQL expression."""
    for match in METRIC.finditer(expr):
        metric = match.group(0)
        rest = expr[match.end():]
        braces = re.match(r"\s*\{([^}]*)\}", rest)
        yield metric, set(LABEL_NAME.findall(braces.group(1))) if braces else set()


def check_namespace_labels(name, dashboard, fail):
    for panel, _ in panels_of(dashboard):
        title = panel.get("title")
        for target in panel.get("targets") or []:
            expr = (target.get("expr") or "").strip()
            for metric, labels in selectors(expr):
                if LLMSM.match(metric):
                    wrong = labels & {"namespace", "exported_namespace"}
                    if wrong:
                        fail(f"FIXED LABEL   {name}: {title}: {metric} matches on "
                             f"{'/'.join(sorted(wrong))}; llmsm_* series must use $namespace_label")
                elif WORKLOAD.match(metric):
                    if "$namespace_label" in labels or "exported_namespace" in labels:
                        fail(f"VARIED LABEL  {name}: {title}: {metric} carries a plain `namespace` "
                             f"only; $namespace_label empties it whenever a reader picks the other value")
                    elif "namespace" not in labels:
                        fail(f"UNSCOPED      {name}: {title}: {metric} has no namespace matcher, "
                             f"so it plots every tenant on the cluster")


# A row saying its panels are not namespace-scoped. Everything else is.
CLUSTER_ROW = "all namespaces"


def check_row_scope(name, dashboard, fail):
    """A row promises a scope; the queries under it have to keep that promise.

    The operational dashboard mixes cluster-wide health with namespace-scoped
    workload panels, and a reader can only tell them apart by the row they sit
    under. Nothing enforced that: "Models blocked from scaling" sat among the
    scoped panels while counting the whole cluster, and read 1 next to a chart of
    the same thing reading zero.
    """
    if not any(p.get("type") == "row" for p in dashboard.get("panels", [])):
        return  # a dashboard with no rows makes no promise to keep

    for panel, row in rows_of(dashboard):
        if panel.get("type") == "row":
            continue
        title = panel.get("title")
        if row is None:
            fail(f"NO ROW        {name}: {title} sits outside every row, so its scope is unstated")
            continue
        exprs = " ".join((t.get("expr") or "") for t in panel.get("targets") or [])
        if not exprs.strip():
            continue
        scoped = any("namespace" in labels or "$namespace_label" in labels
                     for _, labels in selectors(exprs))
        if CLUSTER_ROW in row.lower() and scoped:
            fail(f"ROW SCOPE     {name}: {title} filters on the namespace but sits under "
                 f"{row!r}, which tells the reader it does not")
        elif CLUSTER_ROW not in row.lower() and not scoped:
            fail(f"ROW SCOPE     {name}: {title} sits under {row!r} but has no namespace "
                 f"matcher, so it ignores the Namespace variable it appears to follow")


# Every non-test file of the constants package: the utilization-share and SLO
# metrics are declared in files of their own, and a panel on one of them was
# otherwise reported as naming a metric that does not exist.
METRIC_CONSTANTS = ROOT / "internal" / "constants"
LLMSM_METRIC = re.compile(r"\b(llmsm_[a-z0-9_]+)\b")
HISTOGRAM_SUFFIXES = ("_bucket", "_sum", "_count")


def declared_metrics():
    """Every llmsm_* metric name the controller declares, or None if unreadable."""
    files = [f for f in sorted(METRIC_CONSTANTS.glob("*.go")) if not f.name.endswith("_test.go")]
    if not files:
        return None
    names = set()
    for f in files:
        names |= set(re.findall(r'"(llmsm_[a-z0-9_]+)"', f.read_text(encoding="utf-8")))
    return names


def check_metric_names(name, dashboard, fail):
    """A panel querying a metric nobody emits renders "No data", which is exactly
    what a healthy quiet cluster looks like -- so a renamed or deleted metric can
    sit on a dashboard indefinitely. Only llmsm_* names are checkable: engine and
    EPP names belong to other projects."""
    declared = declared_metrics()
    if declared is None:
        return

    def known(metric):
        if metric in declared:
            return True
        return any(metric.endswith(s) and metric[: -len(s)] in declared
                   for s in HISTOGRAM_SUFFIXES)

    queries = []
    for panel, _ in panels_of(dashboard):
        for target in panel.get("targets") or []:
            queries.append((panel.get("title"), target.get("expr") or ""))
    for variable in dashboard.get("templating", {}).get("list", []):
        query = variable.get("query")
        query = query.get("query") if isinstance(query, dict) else query
        if isinstance(query, str):
            queries.append((f"${variable.get('name')} (variable)", query))

    for where, expr in queries:
        for metric in LLMSM_METRIC.findall(expr):
            if not known(metric):
                fail(f"NO SUCH METRIC {name}: {where}: {metric} is not declared in "
                     f"{METRIC_CONSTANTS.relative_to(ROOT).as_posix()}")


SLO_SOURCE_PAIR = re.compile(r'source="([a-z-]+)"\}\s*==\s*1\)\s*\*\s*(\d+)')


def declared_slo_sources():
    """The SLOSources list in internal/constants/slo_metrics.go, resolved to its
    values, in order; None if it cannot be read."""
    path = METRIC_CONSTANTS / "slo_metrics.go"
    if not path.exists():
        return None
    text = path.read_text(encoding="utf-8")
    values = dict(re.findall(r'\b(SLOSource[A-Za-z]+)\s*=\s*"([^"]+)"', text))
    block = re.search(r"var SLOSources = \[\]string\{([^}]*)\}", text)
    if not block:
        return None
    names = re.findall(r"\b(SLOSource[A-Za-z]+)\b", block.group(1))
    return [values.get(n) for n in names]


def check_slo_sources(name, dashboard, fail):
    """A panel that maps llmsm_slo_source values to codes and the codes back to
    names lists the sources by hand. A source added to constants.SLOSources, or
    one renamed, rendered as "No data" on it and failed nothing."""
    sources = declared_slo_sources()
    if sources is None:
        return
    for panel, _ in panels_of(dashboard):
        exprs = [t.get("expr") or "" for t in panel.get("targets") or []]
        pairs = [p for e in exprs if "llmsm_slo_source" in e for p in SLO_SOURCE_PAIR.findall(e)]
        if not pairs:
            continue
        title = panel.get("title")
        coded = {src: code for src, code in pairs}
        if sorted(coded) != sorted(sources):
            fail(f"SLO SOURCES   {name}: {title}: queries {sorted(coded)}, "
                 f"constants.SLOSources is {sorted(sources)}")
        options = {}
        for m in panel.get("fieldConfig", {}).get("defaults", {}).get("mappings") or []:
            options.update(m.get("options") or {})
        for src, code in coded.items():
            text = (options.get(code) or {}).get("text")
            if text != src:
                fail(f"SLO SOURCES   {name}: {title}: code {code} is queried as {src!r} "
                     f"and mapped to {text!r}")


def check_format(name, text, dashboard, fail):
    """Both files are exactly json.dumps(indent=2). Keeping them that way is what
    makes a one-line change show up as a one-line diff -- a Grafana export or a
    hand edit that reflows the file buries the change in a thousand lines."""
    if text != json.dumps(dashboard, indent=2):
        fail(f"REFORMATTED   {name}: not json.dumps(indent=2); re-dump it so diffs stay readable")


def main():
    failures = []
    if not DASHBOARDS:
        print("no dashboards found under deploy/grafana/")
        return 1
    for path in DASHBOARDS:
        text = path.read_text(encoding="utf-8").replace("\r\n", "\n").rstrip("\n")
        dashboard = json.loads(text)
        name = path.name
        check_format(name, text, dashboard, failures.append)
        check_layout(name, dashboard, failures.append)
        check_identity(name, dashboard, failures.append)
        check_namespace_labels(name, dashboard, failures.append)
        check_row_scope(name, dashboard, failures.append)
        check_metric_names(name, dashboard, failures.append)
        check_slo_sources(name, dashboard, failures.append)

    for line in failures:
        print(line)
    panels = sum(len(list(panels_of(json.loads(p.read_text(encoding="utf-8"))))) for p in DASHBOARDS)
    print(f"{len(DASHBOARDS)} dashboards, {panels} panels checked, {len(failures)} problem(s)")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
