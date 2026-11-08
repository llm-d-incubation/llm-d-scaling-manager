#!/usr/bin/env bash
#
# Runs deploy/ci-pr-checks/goldens-section.sh against fixture pull requests, and
# holds .github/workflows/ci-goldens-gate.yaml to the shape that makes it safe.
#
# Each case is a real two-commit git repository and a PR body. A case asserts
# the exit code AND a line of the output, so a gate that exits 0 having looked at
# nothing does not pass. The workflow itself checks only on a pull_request event
# (a merge group skips the job), which nothing here can raise: what this file
# proves is the script, and that
# the workflow reads the live body and hands it over as data. The real API is
# not called: a stub stands in for gh.

set -u

ROOT="${CHECK_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
GATE="$ROOT/deploy/ci-pr-checks/goldens-section.sh"
WORKFLOW="$ROOT/.github/workflows/ci-goldens-gate.yaml"
[ -f "$GATE" ] || { printf 'FATAL: %s not found\n' "$GATE" >&2; exit 1; }
[ -f "$WORKFLOW" ] || { printf 'FATAL: %s not found\n' "$WORKFLOW" >&2; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

FAIL=0
PASS=0
CASES=0

G=internal/engines/analyzers/learnreplay/testdata/goldens
A_RESULTS="$G/acceptance/results.csv"
A_LINES="$G/acceptance/lines.csv"
OLS_RESULTS="$G/ols-sweep/results.csv"
OLS_SLO="$G/ols-sweep-slo/results.csv"
TTFT_A="$G/ttft-staircase-a/lines.csv"
TTFT_B="$G/ttft-staircase-b/lines.csv"
COUNTS=internal/engines/steadystate/testdata/engine-run-event-counts.txt
ENGINE_WHATIF=cmd/whatif/testdata/engine-new.golden
WHATIF=cmd/whatif/testdata/ttft-open-loop-short.golden
SCHEMA=cmd/whatif/testdata/schema-v1.txt
DOC=docs/reference/metrics.md

# The base commit every fixture PR moves files of.
BASE="$WORK/base"
mkdir -p "$BASE"
git -C "$BASE" init -q
git -C "$BASE" config user.email fixture@example.invalid
git -C "$BASE" config user.name fixture
for f in "$A_RESULTS" "$A_LINES" "$OLS_RESULTS" "$OLS_SLO" "$TTFT_A" "$TTFT_B" "$G/goldens.yaml" \
    "$G/acceptance/recording.jsonl.gz" "$G/e1/lines.csv" \
    "$COUNTS" "$WHATIF" "$SCHEMA" "$DOC" cmd/main.go; do
    mkdir -p "$BASE/$(dirname "$f")"
    echo base >"$BASE/$f"
done
git -C "$BASE" add -A
git -C "$BASE" commit -q -m base
BASE_SHA="$(git -C "$BASE" rev-parse HEAD)"

# pr_with FILE...: leaves $BASE at a commit moving each FILE on top of the base.
pr_with() {
    git -C "$BASE" reset -q --hard "$BASE_SHA"
    local f
    for f in "$@"; do
        echo moved >>"$BASE/$f"
    done
    git -C "$BASE" add -A
    git -C "$BASE" commit -q -m pr --allow-empty
}

# expect NAME WANT_RC WANT_SUBSTRING BODY [BASE_REV HEAD_REV]
expect() {
    local name="$1" want_rc="$2" want="$3" body="$4" rev_a="${5:-$BASE_SHA}" rev_b="${6:-HEAD}" out rc=0
    CASES=$((CASES + 1))
    out="$(cd "$BASE" && PR_BODY="$body" PR_BODY_FILE="${BODY_FILE:-}" bash "$GATE" "$rev_a" "$rev_b" 2>&1)" || rc=$?
    if [ "$rc" -ne "$want_rc" ]; then
        FAIL=$((FAIL + 1)); echo "FAIL $name: exit $rc, want $want_rc: $(printf '%s' "$out" | tr '\n' ' ')"
    elif [ "${out#*"$want"}" = "$out" ]; then
        FAIL=$((FAIL + 1)); echo "FAIL $name: want \"$want\" in: $(printf '%s' "$out" | tr '\n' ' ')"
    else
        PASS=$((PASS + 1)); echo "ok   $name: $(printf '%s' "$out" | head -1)"
    fi
}

# section LINE...: a body with the section holding those lines.
section() {
    printf '## goldens:\n\n'
    printf '%s\n' "$@"
}

R='- `acceptance/results.csv`'
L='- `acceptance/lines.csv`'

# The four cases the gate exists for.
pr_with "$A_RESULTS"
expect "a moved golden and no section fails" 1 "no goldens: section" "$(printf 'Fixes #1\n\n## Proposed Changes\n\n- a change\n')"
expect "the failure names the file" 1 "not named: acceptance/results.csv" "no section here"
expect "a section naming it passes" 0 "the section names each" "$(section "$R")"
pr_with "$A_RESULTS" "$A_LINES"
expect "a partial section fails" 1 "does not hold up" "$(section "$R")"
expect "a partial section names what is missing" 1 "not named: acceptance/lines.csv" "$(section "$R")"
pr_with "$DOC"
expect "docs only, no section, passes" 0 "none of them a golden" "just docs"
expect "docs only, empty body, passes" 0 "none of them a golden" ""
expect "docs only, section none, passes" 0 "none of them a golden" "$(section none)"
expect "docs only reads no section" 0 "none of them a golden" "$(section 'an old body in prose')"
pr_with cmd/main.go "$DOC"
expect "code with no golden passes" 0 "2 files changed" "no section"
pr_with "$DOC"
expect "the file count is singular" 0 "1 file changed" "no section"

# What counts as a golden, and how each is named.
pr_with "$G/acceptance/recording.jsonl.gz"
expect "a recording is a golden" 1 "not named: acceptance/recording.jsonl.gz" "no section"
expect "and is named like one" 0 "names each" "$(section '- `acceptance/recording.jsonl.gz` - taken again')"
pr_with "$COUNTS"
expect "the engine event counts are a golden" 1 "not named: $COUNTS" "no section"
expect "the event counts by file name" 0 "names each" "$(section '- `engine-run-event-counts.txt`')"
expect "the event counts by full path" 0 "names each" "$(section "- \`$COUNTS\`")"
pr_with "$WHATIF"
expect "a whatif golden is a golden" 1 "not named: $WHATIF" "no section"
expect "a whatif golden by glob" 0 "names each" "$(section '- `cmd/whatif/testdata/ttft-*.golden` - one line each')"
expect "a whatif golden by its file-name glob" 0 "names each" "$(section '- `ttft-*.golden`')"
pr_with "$SCHEMA"
expect "the whatif schema is a golden" 1 "not named: $SCHEMA" "no section"
expect "the whatif schema by path" 0 "names each" "$(section "- \`$SCHEMA\`")"
pr_with "$G/goldens.yaml"
expect "the manifest is a golden" 1 "not named: goldens.yaml" "no section"
expect "the manifest by name" 0 "names each" "$(section '- `goldens.yaml`')"
pr_with "$A_RESULTS"
expect "a learner golden by full path" 0 "names each" "$(section "- \`$A_RESULTS\`")"

# The format, where it holds.
expect "free text after a dash" 0 "names each" "$(section '- `acceptance/results.csv` - decode up on 12 cycles, by `0.5 %`')"
expect "text before the heading is ignored" 0 "names each" "$(printf 'Fixes #1\n\n`x` goldens: here\n\n'; section "$R")"
expect "a line starting # ends the section" 0 "names each" "$(section "$R"; printf '\n### Replay summary\n\nany text `at all`\n```\n')"
expect "CRLF, as the web editor sends it" 0 "names each" "$(printf '## goldens:\r\n\r\n- `acceptance/results.csv`\r\n\r\n## Other\r\ntext\r\n')"
expect "blank lines are allowed" 0 "names each" "$(section '' "$R" '   ' '')"
expect "the section may end the body" 0 "names each" "$(printf '## goldens:\n- `acceptance/results.csv`')"
pr_with "$A_RESULTS" "$A_LINES"
expect "two lines name two" 0 "names each" "$(section "$R" "$L")"
expect "a fixture's outputs by glob" 0 "names each" "$(section '- `acceptance/*.csv`')"
pr_with "$TTFT_A" "$TTFT_B"
expect "a family by glob" 0 "names each" "$(section '- `ttft-staircase-*/lines.csv`')"
expect "a family's outputs by glob" 0 "names each" "$(section '- `ttft-staircase-*/*.csv`')"
expect "six literals in the fixture are enough" 0 "names each" "$(section '- `ttft-s*/*.csv`')"
pr_with "$A_RESULTS" "$OLS_RESULTS"
expect "a literal fixture and three literals" 1 "not named: ols-sweep/results.csv" "$(section '- `acceptance/*csv`')"
expect "a star does not cross a slash" 1 "does not hold up" "$(section '- `acc*.csv`' '- `ols*.csv`')"
pr_with "$COUNTS" "$ENGINE_WHATIF"
expect "a bare name fails across two golden roots" 1 "line 3 of the body: the name matches goldens under two directories" "$(section '- `eng*`')"
expect "and takes one root's file" 1 "not named: $ENGINE_WHATIF" "$(section '- `eng*.txt`')"
expect "a root-bound name takes that root alone" 0 "names each" "$(section "- \`$COUNTS\`" "- \`cmd/whatif/testdata/eng*.golden\`")"
pr_with "$WHATIF"
expect "the counts root does not name a whatif golden" 1 "not named: $WHATIF" "$(section '- `internal/engines/steadystate/testdata/ttft-open-loop-short.golden`')"
expect "the learner root does not name a whatif golden" 1 "not named: $WHATIF" "$(section "- \`$G/ttft-open-loop-short.golden\`")"
pr_with "$COUNTS"
expect "the whatif root does not name the event counts" 1 "not named: $COUNTS" "$(section '- `cmd/whatif/testdata/engine-run-event-counts.txt`')"
pr_with "$G/goldens.yaml"
expect "the whatif root does not name the manifest" 1 "not named: goldens.yaml" "$(section '- `cmd/whatif/testdata/goldens.yaml`')"
pr_with "$G/e1/lines.csv"
expect "a short fixture name is literal" 0 "names each" "$(section '- `e1/*.csv`')"
pr_with "$OLS_RESULTS"
expect "a fixture name is not a prefix of another" 1 "not named: ols-sweep/results.csv" "$(section '- `ols-sweep-slo/results.csv`')"
pr_with "$OLS_SLO"
expect "one fixture's glob stops at its own name" 1 "not named: ols-sweep-slo/results.csv" "$(section '- `ols-sweep/*.csv`')"

# none.
pr_with "$A_RESULTS"
expect "none fails when a golden moved" 1 "none, and a golden moved" "$(section none)"
expect "none beside the name still fails" 1 "none, and a golden moved" "$(section none "$R")"

# One section, opened by that one line.
expect "two sections fail" 1 "2 goldens: sections" "$(section "$R"; printf '\n'; section "$R")"
expect "two sections fail, the second empty" 1 "2 goldens: sections" "$(section "$R"; printf '\n## goldens:\n')"
for line in '### goldens:' '## goldens' '## Goldens:' ' ## goldens:' '## goldens: ' '##  goldens:' \
    'goldens:' '**goldens:**' '**Goldens**:' '- **Goldens:**' 'goldens: `acceptance/results.csv`'; do
    expect "not the heading: $line" 1 "no goldens: section" "$(printf '%s\n\n%s\n' "$line" "$R")"
done
expect "an empty section fails" 1 "not named: acceptance/results.csv" "$(printf '## goldens:\n\n## Other\n')"
expect "an empty body fails" 1 "no goldens: section" ""
expect "a name past the section does not count" 1 "not named: acceptance/results.csv" "$(printf '## goldens:\n\n# Notes\n\n%s\n' "$R")"
expect "an indented heading does not end the section" 1 "not of the required form" "$(section "$R" '  ## Notes')"

# Everything else in the section fails, beside a good name, so the line is the
# only reason. Each of these got through a Markdown parser written here once, or
# would have: fences of each kind and an indented closer, a span across lines, a
# setext heading, indented code, raw HTML, comments, and the forms earlier PRs
# used. `form` takes the section's lines, and wants the bad line's number.
form() {
    local name="$1"
    shift
    expect "not the form: $name" 1 "line 4 of the body: not of the required form" "$(section "$R" "$@")"
}
form "a fence" '```' "$R" '```'
form "a four-backtick fence" '````' '```' '````'
form "a tilde fence" '~~~' '~~~'
form "an indented closer" '  ```'
form "a one-line triple-backtick span" '``` x ```'
form "a fence in a list item" '- ```'
form "a fence in a quote" '> ```'
form "a span across lines" 'a `b' 'c` d'
form "a setext heading" 'Title' '---'
form "a thematic break" '---'
form "an equals underline" '==='
form "a dash, then indented code" '-     `acceptance/lines.csv`'
form "two spaces after the dash" '-  `acceptance/lines.csv`'
form "an indented list item" '  - `acceptance/lines.csv`'
form "a star bullet" '* `acceptance/lines.csv`'
form "a plus bullet" '+ `acceptance/lines.csv`'
form "a numbered item" '1. `acceptance/lines.csv`'
form "a quoted item" '> - `acceptance/lines.csv`'
form "a bare span" '`acceptance/lines.csv`'
form "a span in a sentence" 'moved `acceptance/lines.csv` up'
form "prose" 'acceptance/lines.csv moves'
form "a colon after the name" '- `acceptance/lines.csv`: up'
form "text with no dash" '- `acceptance/lines.csv` up'
form "a dash with no text" '- `acceptance/lines.csv` -'
form "two names on a line" '- `acceptance/lines.csv` `goldens.yaml`'
form "a double-backtick span" '- ``acceptance/lines.csv``'
form "escaped backticks" '- \`acceptance/lines.csv\`'
form "an unclosed span" '- `acceptance/lines.csv'
form "an empty span" '- ``'
form "a one-line comment" '<!-- `acceptance/lines.csv` -->'
form "a comment over lines" '<!--' 'how to' '-->'
form "raw HTML, a pre" '<pre>' "$R" '</pre>'
form "raw HTML, details" '<details>' '</details>'
form "raw HTML, an inline tag" '<b>acceptance/lines.csv</b>'
form "a table" '| Output | Rows |' '| --- | --- |'
form "none in a sentence" 'none move'
form "None" 'None'
form "none with a dash" '- none'
form "a tab before the dash" "$(printf '\t- `acceptance/lines.csv`')"
form "a trailing space" '- `acceptance/lines.csv` '
expect "the template's old comment fails" 1 "not of the required form" "$(section '<!-- for example `acceptance/results.csv`: up -->')"
expect "a section in a comment fails" 1 "not of the required form" "$(printf '<!--\n## goldens:\n- `acceptance/results.csv`\n-->\n')"
expect "a section in a fence fails" 1 "not of the required form" "$(printf '```\n## goldens:\n- `acceptance/results.csv`\n```\n')"

# Names, beside a good one, so the rule is the only reason.
name() {
    expect "not a name: $1" 1 "line 4 of the body: the name is not allowed" "$(section "$R" "- \`$1\`")"
}
for bad in '*' '**' '*/*' '?/*' '*.csv' '*/*.csv' '*.gz' '*e*' '*e*/*' '*/*e*' '*/re*.csv' 'ac*/*.csv' '???/*.csv' \
    '*/*csv' '*golden' '*txt' '*/*json' '*/results.csv' '*/lines.csv' 'ttft-*/lines.csv' 'ttft-*/*.csv' \
    '?tft-open-loop-short.golden' 'cmd/whatif/testdata/acceptance/results.csv' \
    'internal/engines/steadystate/testdata/acceptance/results.csv' \
    'acceptance/*' 'acceptance/???' 'acceptance/ab*' '*/*/results.csv' \
    '*.golden' '*.txt' '*.yaml' 'cmd/whatif/testdata/*' 'cmd/whatif/testdata/*.golden' 'cmd/whatif/testdata/*/ttft-open-loop-short.golden' \
    'cmd/*/testdata/ttft-open-loop-short.golden' 'internal/engines/steadystate/testdata/*.txt' \
    'internal/engines/steadystate/testdata/*/engine-run-event-counts.txt' \
    "$G/*" "$G/*/*" "$G/*/*.csv" "$G/*/*/lines.csv" \
    'internal/*/*/*/*/*/*/*' 'internal/*/*/*/*/*/*/*/results.csv' 'internal/engines/*/results.csv' \
    'internal/engines/analyzers/learnreplay/testdata/*/acceptance/results.csv' \
    '<!--' '@(acceptance)/results.csv' '[a-z]*/results.csv' 'a b.csv' '$(id)' '../acceptance/results.csv/x'; do
    name "$bad"
done
pr_with "$A_RESULTS" "$A_LINES"
expect "a literal directory lends a glob nothing" 1 "not allowed" "$(section '- `internal/*/*/*/*/*/*/*`')"
expect "a refused name names nothing" 1 "not named: acceptance/lines.csv" "$(section "$R" '- `*/*.csv`')"
expect "a name that is no file is not a pass" 1 "not named: acceptance/results.csv" "$(section '- `acceptance`' '- `acceptance/`')"

# Paths git would C-quote. Unquoted, each is a golden like any other.
for odd in 'a"b.csv' "$(printf 'a\tb.csv')" 'a\b.csv' "$(printf 'a\nb.csv')"; do
    pr_with "$G/acceptance/$odd"
    expect "a path git quotes is a golden: $(printf '%q' "$odd")" 1 "this PR moves 1 golden and" "no section"
    expect "and a glob names it: $(printf '%q' "$odd")" 0 "names each" "$(section '- `acceptance/*.csv`')"
done
pr_with "$G/acceptance/$(printf 'x\n::error::injected.csv')"
CASES=$((CASES + 1))
OUT="$(cd "$BASE" && PR_BODY="none" bash "$GATE" "$BASE_SHA" HEAD 2>&1)"
if printf '%s\n' "$OUT" | grep -q '^::'; then
    FAIL=$((FAIL + 1)); echo "FAIL a path reached a line of its own: $OUT"
else
    PASS=$((PASS + 1)); echo "ok   a path with a newline prints no workflow command"
fi

# Renames and deletions: both ends of a rename move.
pr_with "$A_RESULTS"
git -C "$BASE" reset -q --hard "$BASE_SHA"
git -C "$BASE" mv "$A_RESULTS" "$G/acceptance/renamed.csv"
git -C "$BASE" commit -q -m rename
expect "a rename moves both ends" 1 "not named: acceptance/results.csv" "$(section '- `acceptance/renamed.csv`')"
expect "a rename named at both ends passes" 0 "names each" "$(section '- `acceptance/renamed.csv`' "$R")"
git -C "$BASE" reset -q --hard "$BASE_SHA"
git -C "$BASE" rm -q "$A_LINES"
git -C "$BASE" commit -q -m delete
expect "a deleted golden moved" 1 "not named: acceptance/lines.csv" "no section"

# The body is data.
pr_with "$A_RESULTS"
PWN="$WORK/pwned"
for hostile in "\$(touch $PWN)" "\`touch $PWN\`" "; touch $PWN" "\${IFS}touch $PWN"; do
    expect "a hostile line is not run: $hostile" 1 "FAIL: goldens" "$(section "$hostile" "- \`$hostile\`")"
done
expect "a hostile free text is not run" 0 "names each" "$(section "$R - \$(touch $PWN) \`touch $PWN\`")"
CASES=$((CASES + 1))
if [ -e "$PWN" ]; then
    FAIL=$((FAIL + 1)); echo "FAIL a hostile body ran: $PWN exists"
else
    PASS=$((PASS + 1)); echo "ok   no hostile body ran"
fi
CASES=$((CASES + 1))
OUT="$(cd "$BASE" && PR_BODY="$(section '::error::injected' '- `::warning::injected`' '- `*/*.csv` - ::notice::injected' '- `injected/lines.csv`')" bash "$GATE" "$BASE_SHA" HEAD 2>&1)"
if [ "${OUT#*injected}" != "$OUT" ]; then
    FAIL=$((FAIL + 1)); echo "FAIL the body reached the job's output: $OUT"
else
    PASS=$((PASS + 1)); echo "ok   nothing of the body reaches the job's output"
fi

# The body in a file, as the workflow hands it over.
BODY_FILE="$WORK/body-in-a-file.md"
pr_with "$A_RESULTS"
section "$R" >"$BODY_FILE"
expect "a body in a file is read" 0 "names each" "no section"
printf '## goldens:\r\n\r\n- `acceptance/results.csv`' >"$BODY_FILE"
expect "a file with CRLF and no last newline is read" 0 "names each" "no section"
section none >"$BODY_FILE"
expect "the file wins over the env var" 1 "none, and a golden moved" "$(section "$R")"
: >"$BODY_FILE"
expect "an empty file is an empty body" 1 "no goldens: section" "$(section "$R")"
BODY_FILE="$WORK/no-such-file.md"
expect "a missing file fails, not an empty body" 2 "read no body" "$(section "$R")"
pr_with "$DOC"
expect "a missing file fails with no golden moved" 2 "read no body" ""
BODY_FILE="$WORK"
expect "a directory is not a body" 2 "read no body" ""
BODY_FILE=""

# An unread diff is not "no golden moved".
pr_with "$A_RESULTS"
expect "an unknown base fails" 2 "could not diff" "" 0000000000000000000000000000000000000000 HEAD
expect "an unresolved parent fails" 2 "could not diff" "" 'HEAD~99' HEAD
expect "an empty diff fails" 2 "the diff is empty" "" HEAD HEAD
CASES=$((CASES + 1))
if out="$(cd "$BASE" && bash "$GATE" 2>&1)"; then
    FAIL=$((FAIL + 1)); echo "FAIL no arguments exits 0: $out"
else
    PASS=$((PASS + 1)); echo "ok   no arguments is a usage error"
fi

# hold NAME GREP_PATTERN: the workflow must match; refuse NAME GREP_PATTERN: must
# not. Its comments say what it must not do, so they are left out.
WF_CODE="$WORK/workflow-code"
grep -Ev '^[[:space:]]*#' "$WORKFLOW" >"$WF_CODE"
hold() {
    CASES=$((CASES + 1))
    if grep -Eq "$2" "$WF_CODE"; then
        PASS=$((PASS + 1)); echo "ok   workflow: $1"
    else
        FAIL=$((FAIL + 1)); echo "FAIL workflow: $1: no line matches $2"
    fi
}
refuse() {
    CASES=$((CASES + 1))
    if grep -Eq "$2" "$WF_CODE"; then
        FAIL=$((FAIL + 1)); echo "FAIL workflow: $1: $(grep -En "$2" "$WF_CODE" | head -1)"
    else
        PASS=$((PASS + 1)); echo "ok   workflow: $1"
    fi
}
hold "it runs on a pull request" '^  pull_request:$'
hold "a merge queue gets a report" '^  merge_group:$'
hold "a merge group skips the job: it has no body" "^    if: github\.event_name == 'pull_request'$"
hold "queued merges do not cancel each other" '^  group: ci-goldens-gate-\$\{\{ github\.event\.pull_request\.number \|\| github\.run_id \}\}$'
hold "correcting the section re-runs it" '^    types: \[.*\bedited\b.*\]'
hold "a new commit re-runs it" '^    types: \[.*\bsynchronize\b.*\]'
hold "the body is read live, from the API" '^ +run: gh api "repos/\$GITHUB_REPOSITORY/pulls/\$PR_NUMBER" --jq .*>"\$PR_BODY_FILE"$'
hold "the number comes from the event" '^ +PR_NUMBER: \$\{\{ github\.event\.pull_request\.number \}\}$'
hold "the body arrives in a file outside the checkout" '^ +PR_BODY_FILE: \$\{\{ runner\.temp \}\}/pr-body\.md$'
hold "the token reads the code" '^  contents: read$'
hold "the token reads the pull request" '^  pull-requests: read$'
hold "it runs the script, on the merge and its base" '^ +run: bash deploy/ci-pr-checks/goldens-section\.sh HEAD\^1 HEAD$'
hold "the checkout keeps the merge's parent" '^ +fetch-depth: 2$'
hold "the checkout leaves no token behind" '^ +persist-credentials: false$'
refuse "no pull_request_target" 'pull_request_target'
refuse "nothing writes" ':[[:space:]]*write\b'
refuse "no other permission" '^ +(actions|checks|deployments|issues|packages|security-events|statuses|id-token):'
refuse "the saved payload is not the body" 'github\.event\.pull_request\.body|GITHUB_EVENT_PATH'
refuse "the body is not held in an env var" '^ +PR_BODY:'
refuse "the event never reaches a shell" '^ +run:.*\$\{\{'

# The token is on the one step that reads the body, and that step is over before
# anything of the PR is on disk.
CASES=$((CASES + 1))
TOKEN_LINES="$(grep -Ec 'GH_TOKEN|GITHUB_TOKEN|github\.token' "$WF_CODE")"
TOKEN_AT="$(grep -En '^ +GH_TOKEN: \$\{\{ github\.token \}\}$' "$WF_CODE" | cut -d: -f1)"
CHECKOUT_AT="$(grep -En 'uses: actions/checkout@' "$WF_CODE" | cut -d: -f1)"
if [ "$TOKEN_LINES" = 1 ] && [ -n "$TOKEN_AT" ] && [ -n "$CHECKOUT_AT" ] && [ "$TOKEN_AT" -lt "$CHECKOUT_AT" ]; then
    PASS=$((PASS + 1)); echo "ok   workflow: the token is gone before the checkout"
else
    FAIL=$((FAIL + 1)); echo "FAIL workflow: the token: $TOKEN_LINES lines, at line ${TOKEN_AT:-none}, checkout at ${CHECKOUT_AT:-none}"
fi

# The workflow's read itself, run the way a runner runs it, against a stub gh that
# stands in for the API: what it asks for, and that a failure stops the job.
FETCH="$(sed -n 's/^ *run: \(gh api .*\)$/\1/p' "$WF_CODE")"
STUB="$WORK/stub"
mkdir -p "$STUB"
printf '%s\n' '#!/usr/bin/env bash' 'printf "%s\n" "$1" "$2" "$3" "$4" >"$STUB_ARGS"' \
    '[ -z "${STUB_FAIL:-}" ] || exit 1' 'cat "$STUB_BODY"' >"$STUB/gh"
chmod +x "$STUB/gh"
section "$R" >"$WORK/live-body.md"
fetch() {
    GITHUB_REPOSITORY=owner/repo PR_NUMBER=7 PR_BODY_FILE="$WORK/fetched.md" PATH="$STUB:$PATH" \
        STUB_ARGS="$WORK/stub-args" STUB_BODY="$WORK/live-body.md" bash -e -c "$FETCH"
}
CASES=$((CASES + 1))
if ! fetch; then
    FAIL=$((FAIL + 1)); echo "FAIL workflow: the read failed against the stub: $FETCH"
elif ! cmp -s "$WORK/live-body.md" "$WORK/fetched.md"; then
    FAIL=$((FAIL + 1)); echo "FAIL workflow: the read did not write the body to the file"
elif [ "$(sed -n 2p "$WORK/stub-args")" != repos/owner/repo/pulls/7 ]; then
    FAIL=$((FAIL + 1)); echo "FAIL workflow: the read asked for $(sed -n 2p "$WORK/stub-args")"
else
    PASS=$((PASS + 1)); echo "ok   workflow: the read writes the live body to the file"
fi
pr_with "$A_RESULTS"
BODY_FILE="$WORK/fetched.md"
expect "the script reads what the read wrote" 0 "names each" "no section"
BODY_FILE=""
CASES=$((CASES + 1))
if STUB_FAIL=1 fetch; then
    FAIL=$((FAIL + 1)); echo "FAIL workflow: a failed read did not stop the job"
else
    PASS=$((PASS + 1)); echo "ok   workflow: a failed read stops the job"
fi

# A multi-line run block would hide an expression from the one-line check above.
CASES=$((CASES + 1))
if awk '
    /^[ ]+(- )?run:[ ]*[|>]/ { match($0, /^[ ]*/); indent = RLENGTH; inblock = 1; next }
    inblock { match($0, /^[ ]*/); if (length($0) && RLENGTH <= indent) inblock = 0 }
    inblock && /\$\{\{/ { bad = 1 }
    END { exit bad }
' "$WF_CODE"; then
    PASS=$((PASS + 1)); echo "ok   workflow: no expression in a run block"
else
    FAIL=$((FAIL + 1)); echo "FAIL workflow: an expression sits in a run block"
fi

echo "$CASES cases: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ] && [ "$PASS" -eq "$CASES" ] && [ "$CASES" -gt 0 ]
