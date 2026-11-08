#!/usr/bin/env bash
#
# Finds comment lines that sit INSIDE a continued make recipe. Neither form
# works, and they fail differently.
#
# A recipe line ending in a backslash continues into the next line, and make
# hands the whole thing to one shell.
#
# `@#` on a continuation line is not a comment at all: make never sees it as a
# recipe line, so it never strips the `@`, and the shell tries to run it:
#
#     @#: command not found
#
# A plain `#` is worse, because it is a valid shell comment and the damage is
# structural:
#
#   - WITHOUT a trailing backslash it ENDS the command there. Everything after
#     it is a separate recipe line, so a comment inside an if/else leaves the
#     shell an unterminated block:
#
#         bash: -c: line 7: syntax error: unexpected end of file
#
#     That is how `make benchmark-install` broke -- the first step of the first
#     guide, failing from a clean tree on every platform.
#
#   - WITH a trailing backslash make joins it to the NEXT line, the shell reads
#     the join as one comment, and the command inside it disappears with no
#     error at all. Silent, and therefore the worse of the two.
#
# So there is no way to write a comment inside a continued recipe. Put it above
# the recipe as a `@#` line on its own -- that is what the fix does, and this
# check is what keeps it that way.
#
# Both shipped: the `@#` form in the model-cache target, the plain `#` form in
# benchmark-install. Each was caught by running it against a cluster, because
# they look exactly like every other comment in the file.
#
# Written as a script rather than inline awk in the Makefile because make eats
# `$` and backslashes in a recipe, which silently turned the first version of
# this check into `awk: syntax error` followed by "OK" -- a lint that always
# passes, which is worse than no lint at all.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MAKEFILE="$ROOT/Makefile"
[ -f "$MAKEFILE" ] || { echo "no Makefile at $MAKEFILE" >&2; exit 1; }

bad="$(tr -d '\r' < "$MAKEFILE" | awk '
    # Only recipe lines matter (they start with a tab).
    /^\t/ {
        if (cont && $0 ~ /^\t[[:space:]]*@#/) {
            printf "  Makefile:%d: [@# runs as a command] %s\n", NR, $0
            found = 1
        }
        else if (cont && $0 ~ /^\t[[:space:]]*#/) {
            if ($0 ~ /\\$/)
                printf "  Makefile:%d: [swallows the next line] %s\n", NR, $0
            else
                printf "  Makefile:%d: [truncates the command] %s\n", NR, $0
            found = 1
        }
        # A trailing backslash continues into the next line.
        cont = ($0 ~ /\\$/)
        next
    }
    { cont = 0 }
    END { exit(found ? 1 : 0) }
')" && { echo "make recipe comments OK"; exit 0; }

echo "ERROR: a comment sits inside a continued recipe:" >&2
printf '%s\n' "$bad" >&2
cat >&2 <<'MSG'

Move it ABOVE the recipe, as a `@#` line before the first command. There is no
correct way to comment inside a continued recipe:

  @#  inside a continuation   make leaves the `@` on, the shell runs it
  #   with no backslash       make ends the command there -- an if/else loses
                              its `fi` and bash reports a syntax error
  #   with a backslash        make joins it to the next line, the shell reads
                              both as one comment, the command vanishes silently
MSG
exit 1
