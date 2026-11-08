#!/usr/bin/env bash
#
# Finds apostrophes inside single-quoted jq/yq programs.
#
# The programs in deploy/lib are embedded in SINGLE-QUOTED shell strings, and
# they carry `#` comments. An apostrophe in one of those comments ends the shell
# quoting; the next one reopens it. So:
#
#   * an ODD number of apostrophes leaves the quoting unbalanced, and `bash -n`
#     reports a syntax error somewhere confusing;
#   * an EVEN number REBALANCES. `bash -n` passes, the text between the two
#     apostrophes is handed to the shell as code, and jq silently receives a
#     TRUNCATED program. Every workload then reads as unparseable.
#
# Both have happened in this repo. The even case shipped in a commit and was
# only caught because a fixture test executed the program; the odd case broke a
# working tree an hour later. This check costs nothing and catches both.
#
# The rule is simply: write "the plan does" rather than "the plan's", inside
# these programs. Apostrophes in ordinary shell comments are fine -- only the
# region between the quotes that open a jq/yq program is scanned.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FOUND=0

for f in "$ROOT"/deploy/lib/*.sh "$ROOT"/deploy/*.sh; do
    [ -f "$f" ] || continue
    # awk, because this is a state machine over lines: enter on a line that opens
    # a single-quoted jq/yq program, leave on the line whose apostrophe closes it.
    out="$(tr -d '\r' < "$f" | awk '
        # A line that starts a program: `jq ... ` followed by an opening quote
        # that is not closed on the same line.
        !inprog && /(^|[^[:alnum:]_])(jq|yq)([[:space:]]|$)/ {
            line = $0
            # Count apostrophes; an odd count opens a string that stays open.
            n = gsub(/'"'"'/, "&", line)
            if (n % 2 == 1) { inprog = 1; start = NR; next }
        }
        inprog {
            line = $0
            n = gsub(/'"'"'/, "&", line)
            # Inside a program, ANY apostrophe on a comment line is the bug: it
            # is meant as punctuation and the shell reads it as a quote.
            if (line ~ /^[[:space:]]*#/ && n > 0) {
                printf "%d: %s\n", NR, $0
                bad = 1
            }
            if (n % 2 == 1) { inprog = 0 }
        }
        END { exit(bad ? 1 : 0) }
    ')" || {
        printf 'APOSTROPHE INSIDE A QUOTED PROGRAM: %s\n' "${f#"$ROOT"/}" >&2
        printf '%s\n' "$out" | sed 's/^/    /' >&2
        FOUND=1
    }
done

if [ "$FOUND" -ne 0 ]; then
    cat >&2 <<'MSG'

Rewrite the comment without the apostrophe ("the plan does", not "the plan's").
An even number of them rebalances the quoting, so `bash -n` will not save you --
jq just receives a truncated program and every workload reads as unparseable.
MSG
    exit 1
fi

echo "no apostrophes inside quoted jq/yq programs"
