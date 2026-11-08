#!/usr/bin/env bash
#
# Executes hack/benchmark/patch_harness.sh fix 18 -- the harness's standup --
# on a fixture carrying the upstream anchor, then RUNS the patched
# _add_helm_repos against a recording helm stand-in.
#
#   fix 18  a reused Istio (gateway.skipInstall) adds no Istio Helm repository.
#           The standup added it anyway, and when the repository started
#           answering 403 every standup on a cluster that already runs Istio
#           failed over a repository it never reads.
#
# The harness clone is not in CI, so the fixture reproduces the upstream lines
# the fix anchors on; a fix that stops matching upstream fails at standup with
# "anchor missing", and the anchor here is the same string.

set -u
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PH="$ROOT/hack/benchmark/patch_harness.sh"
PY="${PYTHON:-python3}"
FAILED=0
ok()   { printf '  ok   %s\n' "$1"; }
fail() { printf '  FAIL %s\n' "$1"; FAILED=1; }

T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT

tr -d '\r' < "$PH" | sed -n '/fix 18 (.*) failed"$/,/^PYEOF$/p' | sed '1d;$d' > "$T/fix18.py"
[ -s "$T/fix18.py" ] || { fail "fix 18 is not in patch_harness.sh"; echo "harness standup check FAILED"; exit 1; }

# --- fixture: upstream _add_helm_repos ---------------------------------------
cat > "$T/step_02.py" <<'EOF'
class AdminPrerequisitesStep:
    def _add_helm_repos(self, cmd, plan_config: dict, errors: list):
        """Add configured Helm repositories."""
        helm_repos = plan_config.get("helmRepositories", {})
        gateway_class = plan_config.get("gateway", {}).get("className")
        added_classic_repo = False

        for repo_key, repo_info in helm_repos.items():
            # The Istio repository is only required when Istio is the selected
            # Gateway provider. epponly/agentgateway deployments otherwise gain
            # an unnecessary network dependency during every standup.
            if repo_key == "istio" and gateway_class != "istio":
                cmd.logger.log_info(
                    f"Skipping Istio Helm repo for gateway.className="
                    f"{gateway_class or 'unset'}"
                )
                continue
            repo_name = repo_info.get("name", repo_key)
            repo_url = repo_info.get("url", "").strip()
            if not repo_url:
                continue

            if repo_url.startswith("oci://"):
                cmd.logger.log_info(
                    f"OCI registry detected for {repo_name} -- no repo add required"
                )
                continue

            result = cmd.helm("repo", "add", repo_name, repo_url, "--force-update")
            if not result.success:
                errors.append(f"Failed to add helm repo {repo_name}: {result.stderr}")
            else:
                added_classic_repo = True

        if added_classic_repo:
            cmd.helm("repo", "update")
EOF

out="$("$PY" "$T/fix18.py" "$T/step_02.py" 2>&1)"
case "$out" in *"): applied"*) ok "fix 18 applies to the upstream anchor" ;; *) fail "fix 18 on the fixture: $out" ;; esac
out="$("$PY" "$T/fix18.py" "$T/step_02.py" 2>&1)"
case "$out" in *"already applied"*) ok "fix 18 is idempotent" ;; *) fail "fix 18 second run: $out" ;; esac
"$PY" -m py_compile "$T/step_02.py" && ok "fix 18: the patched step compiles" || fail "fix 18: the patched step does not compile"
printf 'x = 1\n' > "$T/blank.py"
if "$PY" "$T/fix18.py" "$T/blank.py" >/dev/null 2>&1; then fail "fix 18 must fail on a file without its anchor"; else ok "fix 18: a missing anchor is a hard error"; fi

# --- run the patched loop against a recording helm ---------------------------
cat > "$T/run.py" <<'PY'
import sys
sys.path.insert(0, sys.argv[1])
from step_02 import AdminPrerequisitesStep

class R:
    success, stderr = True, ""

class Log:
    def __init__(self): self.lines = []
    def log_info(self, m, **_): self.lines.append(m)

class Cmd:
    def __init__(self): self.calls, self.logger = [], Log()
    def helm(self, *a): self.calls.append(" ".join(a)); return R()

REPOS = {"istio": {"url": "https://istio.example/charts"}, "llmDModelservice": {"url": "https://ms.example/"}}
bad = []
def run(gw):
    cmd = Cmd(); errors = []
    AdminPrerequisitesStep()._add_helm_repos(cmd, {"helmRepositories": REPOS, "gateway": gw}, errors)
    return [c for c in cmd.calls if c.startswith("repo add")], cmd.logger.lines
def check(cond, msg):
    print(("  ok   " if cond else "  FAIL ") + msg)
    if not cond: bad.append(msg)

adds, logs = run({"className": "istio", "skipInstall": True})
check(not any(" istio " in a for a in adds), "fix 18: a reused Istio (skipInstall) adds no Istio repository: %s" % adds)
check(any("llmDModelservice" in a for a in adds), "fix 18: the other repositories are still added")
check(any("skipInstall" in m for m in logs), "fix 18: the log says why the Istio repository was skipped")
adds, _ = run({"className": "istio"})
check(any(" istio " in a for a in adds), "fix 18: an Istio the standup installs still gets its repository: %s" % adds)
adds, _ = run({"className": "agentgateway", "skipInstall": True})
check(not any(" istio " in a for a in adds), "fix 18: another gateway class still skips it, as upstream does")
sys.exit(1 if bad else 0)
PY
"$PY" "$T/run.py" "$T" || FAILED=1

if [ "$FAILED" -ne 0 ]; then
    echo "harness standup check FAILED"
    exit 1
fi
echo "harness standup check OK"
