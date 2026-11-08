#!/usr/bin/env bash
#
# Every kubectl request a benchmark-* make recipe makes carries
# --request-timeout.
#
# kubectl has no default timeout. A `kubectl get pvc model-pvc` in the standup's
# model-cache step hung for seven and a half minutes on a shared cluster whose
# API server stalled, with nothing printed, until it was killed by hand -- the
# standup looked busy the whole time. A read that gives up after
# BENCHMARK_KUBECTL_TIMEOUT fails with a reason instead.
#
# Covered: get, apply, create, delete, patch, label, api-resources. Not covered,
# deliberately: rollout status and wait, which carry their own --timeout and are
# long watches that a request timeout would cut short.
#
# Written as a script for the reason hack/check-make-recipes.sh gives: make eats
# dollars and backslashes in a recipe.

set -u
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MAKEFILE="${1:-$ROOT/Makefile}"

bad="$(tr -d '\r' < "$MAKEFILE" | awk '
    /^[A-Za-z0-9_.-]+:/ { target = $1; sub(/:.*/, "", target) }
    /^\t/ && target ~ /^benchmark/ {
        line = $0
        if (line ~ /^\t@#/) next
        # a command printed for the reader is text, not a call
        if (line ~ /echo[ \t]+"[^"]*kubectl/) next
        if (line ~ /kubectl[ \t]+(get|apply|create|delete|patch|label|api-resources)([ \t]|$)/ && line !~ /--request-timeout/)
            printf "%s:%d [%s] %s\n", FILENAME, NR, target, line
    }
' FILENAME="$MAKEFILE")"

if [ -z "$bad" ]; then
    n="$(tr -d '\r' < "$MAKEFILE" | awk '/^[A-Za-z0-9_.-]+:/ { t = $1 } /^\t/ && t ~ /^benchmark/ && /--request-timeout/ { c++ } END { print c + 0 }')"
    if [ "$n" -eq 0 ]; then
        echo "ERROR: no benchmark recipe carries --request-timeout at all; the check read nothing" >&2
        exit 1
    fi
    echo "benchmark kubectl request timeouts OK ($n call(s))"
    exit 0
fi
echo "ERROR: a benchmark recipe calls kubectl with no --request-timeout (it can hang with no output):" >&2
printf '%s\n' "$bad" >&2
echo "Add --request-timeout=\$(BENCHMARK_KUBECTL_TIMEOUT) after kubectl." >&2
exit 1
