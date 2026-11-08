#!/usr/bin/env bash
# Render a captured snapshot through the REAL Grafana dashboard, locally.
#
#   render.sh <snapshot-dir>
#
# <snapshot-dir> holds panels.json from hack/benchmark/snapshot.py. Output is
# one PNG per panel plus a full-dashboard PNG, written beside it.
#
# Why Grafana and not a plotting library: these are meant to BE the dashboard,
# so they must not drift from it in colour, axis or panel choice. Grafana reads
# deploy/grafana/benchmark-dashboard.json directly; the data comes from the
# snapshot through a shim that speaks the Prometheus API. Nothing here touches a
# cluster, so any past run can be re-rendered.
set -euo pipefail

SNAP_DIR="${1:?usage: render.sh <snapshot-dir>}"
SNAP_DIR="$(cd "$SNAP_DIR" && pwd)"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/../../.." && pwd)"
# Overridable so the operational dashboard can be rendered too -- useful for
# checking a layout change without a Grafana image renderer on the cluster
# (the shared GPU cluster has none: /render returns "No image renderer available/installed").
DASHBOARD="${DASHBOARD:-$REPO/deploy/grafana/benchmark-dashboard.json}"

[ -f "$SNAP_DIR/panels.json" ] || { echo "no panels.json in $SNAP_DIR"; exit 1; }
[ -f "$DASHBOARD" ] || { echo "dashboard not found: $DASHBOARD"; exit 1; }
# compose bind-mounts this path, and a relative one resolves against the compose
# file's directory instead of the caller's -- which mounts a directory that does
# not exist and leaves Grafana with no dashboard at all.
DASHBOARD="$(cd "$(dirname "$DASHBOARD")" && pwd)/$(basename "$DASHBOARD")"

command -v docker >/dev/null || { echo "docker is required"; exit 1; }

UID_DASH=$(python3 -c "import json;d=json.load(open('$DASHBOARD'));print(d.get('uid') or 'scaling-manager-benchmark')")
SLUG=$(python3 -c "import json,re;d=json.load(open('$DASHBOARD'));print(re.sub(r'[^a-z0-9]+','-',(d.get('title') or 'dashboard').lower()).strip('-'))")

# Images go in a directory named for the DASHBOARD, not straight into the
# snapshot. One snapshot can be rendered through both dashboards, and they share
# panel titles ("Deployment Replicas"), so a flat layout has the second render
# silently overwrite the first -- and dashboard.png always belonged to whichever
# ran last.
OUT_DIR="$SNAP_DIR/$SLUG"
mkdir -p "$OUT_DIR"

# `timeout` in a render URL is puppeteer's NAVIGATION timeout, and the page never
# reaches network-idle -- so EVERY panel waits it out and is screenshotted when
# the wait expires. It is dead time, not work: the same two panels rendered
# BYTE-IDENTICAL at 10s, 20s, 30s and 60s. Ten seconds plus headroom it is; a
# 34-panel pass costs 8 minutes instead of 35.
#
# It has to be long enough to survive the plugin failure that used to turn the
# wait into an outright 500 (see GF_PLUGINS_PREINSTALL_DISABLED in the compose
# file); raise it with SNAPSHOT_RENDER_TIMEOUT if a panel ever comes back blank.
RENDER_TIMEOUT="${SNAPSHOT_RENDER_TIMEOUT:-15}"
# Set RENDER_PANELS=none to render only the full dashboard. A layout change needs
# that one image, and the per-panel loop is a minute a panel on a 29-panel board.
RENDER_PANELS="${RENDER_PANELS:-all}"

# Ports are chosen free rather than fixed. Under WSL2 the port space is shared
# with Windows, so a fixed default collides with whatever the host happens to be
# running -- 9099 was taken by an unrelated node process, and the shim died with
# "Address already in use" after the compose stack had already been pulled.
# Set SNAPSHOT_{SHIM,GRAFANA}_PORT to pin them.
free_port() {
    python3 -c 'import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()'
}
PORT_GRAFANA="${SNAPSHOT_GRAFANA_PORT:-$(free_port)}"
PORT_SHIM="${SNAPSHOT_SHIM_PORT:-$(free_port)}"
PROJECT="scaling-manager-snapshot"
echo "grafana :$PORT_GRAFANA   shim :$PORT_SHIM"

cleanup() {
    docker compose -p "$PROJECT" -f "$HERE/docker-compose.yml" down -v >/dev/null 2>&1 || true
    [ -n "${SHIM_PID:-}" ] && kill "$SHIM_PID" 2>/dev/null || true
}
trap cleanup EXIT

# The shim runs on the HOST, not in compose: it needs the snapshot file, and
# host networking differs across platforms. Grafana reaches it through
# host.docker.internal, which the compose file maps for Linux too.
python3 "$HERE/promshim.py" --snapshot "$SNAP_DIR/panels.json" --port "$PORT_SHIM" \
    > "$OUT_DIR/shim.log" 2>&1 &
SHIM_PID=$!
sleep 1
kill -0 "$SHIM_PID" 2>/dev/null || { echo "shim failed to start:"; cat "$OUT_DIR/shim.log"; exit 1; }

# The window the snapshot covers -- Grafana is asked for exactly this range, so
# the images show the run and not "now", which would be empty.
FROM_MS=$(python3 -c "import json;print(int(json.load(open('$SNAP_DIR/panels.json'))['window']['start'])*1000)")
TO_MS=$(python3 -c "import json;print(int(json.load(open('$SNAP_DIR/panels.json'))['window']['end'])*1000)")

export SNAPSHOT_DIR="$SNAP_DIR" DASHBOARD_FILE="$DASHBOARD" \
       PORT_GRAFANA="$PORT_GRAFANA" PORT_SHIM="$PORT_SHIM"
docker compose -p "$PROJECT" -f "$HERE/docker-compose.yml" up -d

echo "waiting for grafana on :$PORT_GRAFANA ..."
for _ in $(seq 1 60); do
    if curl -sf "http://localhost:$PORT_GRAFANA/api/health" >/dev/null 2>&1; then break; fi
    sleep 2
done
curl -sf "http://localhost:$PORT_GRAFANA/api/health" >/dev/null || {
    echo "grafana did not come up"; docker compose -p "$PROJECT" -f "$HERE/docker-compose.yml" logs --tail=30; exit 1; }

# The dashboard takes a namespace variable, so the render URL has to say which
# one -- otherwise Grafana renders the variable's default ("All") and the panel
# titles claim a scope the snapshot does not have.
NS_VAR=$(python3 -c "import json;print(json.load(open('$SNAP_DIR/panels.json')).get('namespace') or '')")
# The LABEL is a dashboard variable too, and its default (exported_namespace) is
# wrong wherever the metric was not relabelled. Pass what capture actually
# queried, or the panels render empty against a perfectly good snapshot.
NS_LABEL=$(python3 -c "import json;print(json.load(open('$SNAP_DIR/panels.json')).get('namespace_label') or 'namespace')")
if [ "$RENDER_PANELS" = "none" ]; then
    echo "RENDER_PANELS=none: skipping the per-panel images"
else
echo "rendering panels from $DASHBOARD (namespace=${NS_VAR:-<all>}, label=$NS_LABEL)"
python3 - "$OUT_DIR" "$PORT_GRAFANA" "$UID_DASH" "$SLUG" "$FROM_MS" "$TO_MS" "$NS_VAR" "$NS_LABEL" "$DASHBOARD" "$RENDER_TIMEOUT" <<'PY'
import json, subprocess, sys, re, pathlib
out_dir, port, uid, slug, frm, to = sys.argv[1:7]
ns_var = sys.argv[7] if len(sys.argv) > 7 else ""
ns_label = sys.argv[8] if len(sys.argv) > 8 else "namespace"
timeout = sys.argv[10] if len(sys.argv) > 10 else "60"
# Panels come from the DASHBOARD being rendered, not from the snapshot.
#
# The snapshot records the panels that existed when it was captured. Driving the
# loop from that list renders panel ids the dashboard may no longer have, and
# Grafana answers those with a near-empty PNG rather than an error: re-rendering
# an August snapshot after a panel was deleted produced a 5KB
# panel-scaling-manager-desired-ratio.png that looked like a successful render of a panel
# that does not exist, and silently skipped a panel added since.
#
# Reading the dashboard instead means the images always match what a viewer
# would see. Panels whose queries the snapshot lacks render empty, which is
# honest and already visible in shim.log as NO MATCH.
#
# Panels nested in a ROW count too. A collapsed row hides its children in the
# dashboard view, but they are still panels and /render/d-solo answers for them
# by id -- so the Serving row's six would otherwise be the only ones with no
# image, precisely because the row is collapsed by default.
dash = json.load(open(sys.argv[9]))


def flatten(panel_list):
    out = []
    for panel in panel_list:
        if panel.get("type") == "row":
            out += flatten(panel.get("panels") or [])
        elif "id" in panel:
            out.append(panel)
    return out


panels = flatten(dash.get("panels", []))
for panel in panels:
    safe = re.sub(r"[^a-z0-9]+", "-", panel["title"].lower()).strip("-")
    out = pathlib.Path(out_dir) / f"panel-{safe}.png"
    url = (f"http://localhost:{port}/render/d-solo/{uid}/{slug}"
           f"?panelId={panel['id']}&from={frm}&to={to}&width=1000&height=400&tz=UTC&timeout={timeout}"
           + (f"&var-namespace={ns_var}" if ns_var else "")
           + f"&var-namespace_label={ns_label}")
    rc = subprocess.run(["curl", "-sf", "-o", str(out), url]).returncode
    size = out.stat().st_size if out.exists() else 0
    print(f"  {panel['title'][:34]:34} {out.name:44} {'ok' if rc==0 and size>1000 else 'FAILED'} ({size}B)")
PY
fi

# The full-dashboard height comes from the grid, not a constant. 1400px suits a
# five-panel benchmark dashboard and CROPS the operational one, which runs to 121
# grid rows -- so the layout, the only thing this image is really good for, was
# the part cut off. Grafana's grid cell is 30px on an 8px margin; panels inside a
# COLLAPSED row take no vertical space, which the top-level scan already reflects.
FULL_H=$(python3 -c "
import json
d = json.load(open('$DASHBOARD'))
rows = max((p['gridPos']['y'] + p['gridPos']['h'] for p in d.get('panels', []) if 'gridPos' in p), default=20)
print(min(max(rows * 38 + 80, 600), 8000))
")
FULL="$OUT_DIR/dashboard.png"
curl -sf -o "$FULL" \
  "http://localhost:$PORT_GRAFANA/render/d/$UID_DASH/$SLUG?from=$FROM_MS&to=$TO_MS&width=1200&height=$FULL_H&tz=UTC&timeout=$((RENDER_TIMEOUT * 2))${NS_VAR:+&var-namespace=$NS_VAR}&var-namespace_label=$NS_LABEL" \
  && echo "  full dashboard -> $(basename "$FULL") ${FULL_H}px ($(wc -c < "$FULL")B)" \
  || echo "  full dashboard render FAILED"

echo
echo "images in $OUT_DIR:"
ls -1 "$OUT_DIR"/*.png 2>/dev/null | sed 's/^/  /' || echo "  none produced"
echo "shim log: $OUT_DIR/shim.log (grep 'NO MATCH' for queries the snapshot lacks)"
