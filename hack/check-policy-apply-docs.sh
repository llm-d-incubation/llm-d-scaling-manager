#!/usr/bin/env bash
#
# Fails when a document tells the reader to run
#   kubectl apply -f deploy/configmap-scaling-policy.yaml
#
# That sample has no `limiters:` and names its own namespace. Applied over an
# install, it replaces the `default` entry the installer wrote and silently
# removes the GPU limit: the fleet is unbounded and nothing says so. The docs
# send readers to `kubectl edit` on the installed ConfigMap, or to a re-run of
# the install with LLMSM_LIMITER, instead.
#
# Only a command line counts (optionally indented, as inside a list item); prose
# that warns against the command names it without starting a line with it.

set -u

ROOT="${CHECK_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
FAIL=0
CHECKED=0

while IFS= read -r f; do
    CHECKED=$((CHECKED + 1))
    while IFS= read -r hit; do
        [ -n "$hit" ] || continue
        FAIL=$((FAIL + 1))
        echo "FAIL ${f#"$ROOT"/}:${hit%%:*}: applies the sample policy, which drops limiters: ${hit#*:}"
    done < <(tr -d '\r' < "$f" | grep -nE '^[[:space:]]*(\$[[:space:]]*)?kubectl apply -f [^ ]*configmap-scaling-policy\.yaml')
done < <(find "$ROOT/docs" "$ROOT/deploy" "$ROOT/README.md" -name '*.md' -not -path '*/docs/plans/*' 2>/dev/null)

echo "check-policy-apply-docs: $CHECKED files checked, $FAIL failed"
[ "$FAIL" -eq 0 ] && [ "$CHECKED" -gt 0 ]
