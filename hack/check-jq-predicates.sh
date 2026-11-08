#!/usr/bin/env bash
#
# Guards the jq predicates in deploy/ that decide whether a label set marks an
# llm-d model server.
#
# There is one expression, hand-copied to seven places across four files. An
# external evaluation hit it as `string and boolean cannot be added`, which made
# `make check-prereqs` report that a namespace held no model servers and exit 2
# on a namespace that plainly did. The cause is jq's `as` binding: written
#
#     any(.key + "=" + (.value|tostring) as $kv | ...)
#
# older jq parses it as `a + (b as $x | body)` and evaluates string + boolean.
# The parentheses that fix it are easy to drop on the next copy, and nothing
# would notice: every caller redirects jq's stderr to /dev/null and falls back to
# an empty result, so the failure looks exactly like "no model servers here".
#
# Two checks, because neither is sufficient alone:
#   1. every `as $kv` binding in deploy/ is parenthesised   -- catches a new copy
#   2. the real definitions, extracted from the file rather than retyped here,
#      classify known label sets correctly                  -- catches the rest
#
# Usage: hack/check-jq-predicates.sh
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1

fail=0
note() { printf '  %s\n' "$1"; }

# ---------------------------------------------------------------- 1. the lint
# Every `as $kv` must bind a parenthesised expression. Matching on `any((` is
# enough: that is the only shape any of these ever take.
unparenthesised=0
while IFS= read -r hit; do
    [ -n "$hit" ] || continue
    case "$hit" in
        *'any(('*) ;;
        *) note "NOT PARENTHESISED: $hit"; unparenthesised=$((unparenthesised + 1)) ;;
    esac
done <<EOF
$(grep -rn 'as \$kv' deploy/ 2>/dev/null | grep -v 'check-jq-predicates')
EOF

total=$(grep -rc 'as \$kv' deploy/ 2>/dev/null | awk -F: '{s+=$2} END{print s+0}')
if [ "$unparenthesised" -ne 0 ]; then
    note "$unparenthesised of $total marker predicates bind an unparenthesised expression"
    fail=1
else
    note "$total marker predicates, all parenthesised"
fi

# ------------------------------------------------------------ 2. run the real thing
if ! command -v jq >/dev/null 2>&1; then
    note "jq not on PATH — skipping the execution check"
    exit $fail
fi

# Extract the definitions from the file, so this tests what ships rather than a
# copy that can drift away from it.
defs="$(sed -n '/^ *# A label map is a model server when any of its k=v pairs is a marker\./,/^ *\.items\[\]?/p' \
        deploy/lib/infra_monitoring.sh | sed '$d' | tr -d '\r')"

case "$defs" in
    *'def serving_map('*'def serving_exprs('*) ;;
    *) note "could not extract serving_map/serving_exprs from infra_monitoring.sh"; exit 1 ;;
esac

MARKERS='["llm-d.ai/inferenceServing=true","llm-d.ai/role=decode"]'

# input | program | expected
run_case() {
    local desc="$1" input="$2" prog="$3" want="$4" got
    got="$(printf '%s' "$input" | jq -r --argjson markers "$MARKERS" "$defs $prog" 2>&1)"
    if [ "$got" != "$want" ]; then
        note "FAIL  $desc: got '$got', want '$want'"
        fail=1
    fi
}

run_case "marker present"            '{"llm-d.ai/role":"decode"}'                'serving_map(.)' 'true'
run_case "marker absent"             '{"app":"nginx"}'                           'serving_map(.)' 'false'
run_case "numeric value, no marker"  '{"replicas":3}'                            'serving_map(.)' 'false'
run_case "null value"                '{"llm-d.ai/role":null}'                    'serving_map(.)' 'false'
run_case "empty label map"           '{}'                                        'serving_map(.)' 'false'
run_case "value alone is not a pair" '{"other":"decode"}'                        'serving_map(.)' 'false'
run_case "matchExpressions marker"   '[{"key":"llm-d.ai/role","operator":"In","values":["decode"]}]' 'serving_exprs(.)' 'true'
run_case "matchExpressions absent"   '[{"key":"app","operator":"In","values":["nginx"]}]'            'serving_exprs(.)' 'false'
run_case "matchExpressions numeric"  '[{"key":"app","operator":"In","values":[3]}]'                  'serving_exprs(.)' 'false'

if [ "$fail" -eq 0 ]; then
    note "marker predicates classify all 9 fixtures correctly (jq $(jq --version 2>/dev/null))"
    echo "jq predicates OK"
else
    echo "jq predicate check FAILED" >&2
fi
exit $fail
