#!/usr/bin/env bash
# Prints the engine image the llm-d-benchmark clone will deploy -- the
# repository and tag from its config/templates/values/defaults.yaml, anchors
# resolved -- so a standup can hold that exact reference on the accelerator
# nodes before the harness deploys it. Given the scenario, its `images.vllm`
# (under `shared`, or a stack's own) overrides the default field by field, as
# the harness merges it; several distinct images print comma-separated, shared
# first. Prints nothing (exit 1) when the clone or the values are not where the
# pinned harness keeps them; a standup that cannot name the image must not
# guess one.
#
#   engine_image.sh <llm-d-benchmark clone dir> [scenario file]
set -euo pipefail

REPO_DIR="${1:?usage: engine_image.sh <llm-d-benchmark clone dir> [scenario file]}"
SCENARIO="${2:-}"
DEFAULTS="$REPO_DIR/config/templates/values/defaults.yaml"
[ -f "$DEFAULTS" ] || exit 1
# A named scenario that is not there is a caller bug, not "no override".
[ -z "$SCENARIO" ] || [ -f "$SCENARIO" ] || exit 1

# The harness venv has PyYAML; fall back to the system python.
PY="$REPO_DIR/.venv/bin/python"
[ -x "$PY" ] || PY="${PYTHON:-python3}"

"$PY" - "$DEFAULTS" "$SCENARIO" <<'PYEOF'
import sys
try:
    import yaml
except ImportError:
    sys.exit(1)
d = yaml.safe_load(open(sys.argv[1], encoding="utf-8"))
base = (d.get("images") or {}).get("vllm") or {}
layers = [[base]]
if sys.argv[2]:
    s = yaml.safe_load(open(sys.argv[2], encoding="utf-8")) or {}
    shared = ((s.get("shared") or {}).get("images") or {}).get("vllm") or {}
    # A stack inherits the shared override, then applies its own.
    layers = [[base, shared]]
    layers += [[base, shared, (st.get("images") or {}).get("vllm") or {}]
               for st in (s.get("scenario") or []) if isinstance(st, dict)]
images = []
for layer in layers:
    o = {}
    for l in layer:
        o.update({k: v for k, v in l.items() if v})
    repo, tag = o.get("repository"), o.get("tag")
    if not repo or not tag:
        sys.exit(1)
    ref = "%s:%s" % (repo, tag)
    if ref not in images:
        images.append(ref)
print(",".join(images))
PYEOF
