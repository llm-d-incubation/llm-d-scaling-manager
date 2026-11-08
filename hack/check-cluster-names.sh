#!/usr/bin/env bash
#
# Fails when a tracked file names one of the internal clusters measurements
# were taken on.
#
# Comments, docs and tests say "the shared GPU cluster" or describe the
# hardware ("a multi-node H200 cluster") instead, as PR bodies already do. The
# names themselves are not written here: the check compares SHA-256 digests,
# so the list does not publish what it guards.
#
# What is compared: each line is lowercased and its non-alphanumerics dropped,
# and from every place a word starts (after a non-alphanumeric, or at a
# camelCase capital) a window as long as each listed name is hashed. So a name
# is found plain, in any case, hyphenated or split by - _ or spaces, joined to
# a word after it (NameCluster, namecluster) and with any suffix (name001).
# A name joined to a word BEFORE it (clustername) is not, nor is text in a
# file that is not UTF-8 (a UTF-16 file reads as binary and is skipped).
#
# To add a name, append "<length>:<first 16 hex digits of the digest>" to
# DIGESTS, from: printf %s <name> | wc -c; printf %s <name> | sha256sum
# A name that is also how ordinary text starts (one is a common project word
# plus a letter, so that word followed by any word starting with the letter
# would match) is listed as "x<length>:<digest>": it
# matches only as whole words, joined or split (one to three words), never as
# the start of a longer word.
# Lowercase, alphanumerics only. A maintainer keeps the names themselves in an
# untracked local note; re-derive the list from it rather than from grep hits.
#
# Two files are exempt because a deployment depends on the name: the PR e2e
# workflow selects its runners by label, and verify-correct-cluster.sh refuses
# a run whose API server is the nightly-only cluster.
#
# The check first runs against fixtures holding a synthetic word, in each form
# above, whose digest is in the list, so a scan that matches nothing cannot
# pass silently.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PY="$(command -v python3 || command -v python)" || { echo "FAIL: python3 not found"; exit 1; }

DIGESTS="7:550139a3a4338ebe 7:556c7df75b76c6cc 6:a3d92ef00ea150eb 5:7700a7306a4966d9 x5:523959063745fcda 7:cb13384f3ea6eb0a x5:8bca72418b5ee480"
EXEMPT=".github/workflows/ci-e2e-openshift.yaml deploy/ci-e2e-openshift/verify-correct-cluster.sh"

# scan <git-dir> prints "<path>:<line>" for every hit and exits 1 if any.
scan() {
    git -C "$1" ls-files -z | DIGESTS="$DIGESTS" EXEMPT="$EXEMPT" "$PY" -c '
import hashlib, os, re, sys
root = sys.argv[1]
pairs = [p.split(":") for p in os.environ["DIGESTS"].split()]
digests = {d for n, d in pairs if not n.startswith("x")}
lengths = sorted({int(n) for n, _ in pairs if not n.startswith("x")})
exact = {d for n, d in pairs if n.startswith("x")}
exact_lengths = {int(n[1:]) for n, _ in pairs if n.startswith("x")}
longest_exact = max(exact_lengths, default=0)
exempt = set(os.environ["EXEMPT"].split())
word = re.compile(r"[A-Za-z0-9]+")
camel = re.compile(r"(?<=[a-z])[A-Z]")
exact_seen, window_seen = (set(), set()), (set(), set())

def matches(candidates, listed, seen):
    """Whether any candidate hashes to a listed digest; each is hashed once per run."""
    clean, dirty = seen
    for w in candidates - clean - dirty:
        (dirty if hashlib.sha256(w.encode()).hexdigest()[:16] in listed else clean).add(w)
    return not candidates.isdisjoint(dirty)

hits = 0
for rel in sys.stdin.read().split("\0"):
    if not rel or rel in exempt:
        continue
    try:
        with open(os.path.join(root, rel), "rb") as fh:
            data = fh.read()
    except OSError:
        continue
    if b"\0" in data[:8192]:
        continue
    for n, line in enumerate(data.decode("utf-8", "replace").splitlines(), 1):
        tokens = word.findall(line)
        if not tokens:
            continue
        norm = "".join(tokens).lower()
        starts, at = [], 0
        for tok in tokens:
            starts.append(at)
            starts.extend(at + m.start() for m in camel.finditer(tok))
            at += len(tok)
        # Whole words: each run of one to three consecutive words, joined.
        spans = set()
        for i in range(len(tokens)):
            span = ""
            for tok in tokens[i:i + 3]:
                span += tok
                if len(span) > longest_exact:
                    break
                if len(span) in exact_lengths:
                    spans.add(span.lower())
        end = len(norm)
        windows = {norm[s:s + size] for s in starts for size in lengths if s + size <= end}
        if matches(spans, exact, exact_seen) or matches(windows, digests, window_seen):
            print(f"{rel}:{n}")
            hits += 1
sys.exit(1 if hits else 0)
' "$1"
}

T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT

# Self-test: one line per form the scan claims to find. The fixture words are
# assembled from octal escapes at runtime, because this file is scanned too and
# the scan would find them written out in any of the forms it is testing.
git -C "$T" init -q
q="$(printf '\172\172')"
Q="$(printf '\132\132')"
{
    printf 'measured on %sfix%s today\n' "$q" "$q"
    printf 'MEASURED ON %sFIX%s\n' "$Q" "$Q"
    printf 'kind: %s-fix-%s\n' "$q" "$q"
    printf 'a %s fix %s run\n' "$q" "$q"
    printf 'measuredOn%sfix%sCluster\n' "$Q" "$q"
    printf 'host %sfix%s042.example\n' "$q" "$q"
    printf 'the %sfix%scluster\n' "$q" "$q"
    printf 'only %s-ex-z here\n' "$q"
    printf 'nothing to see here, not even %sexztail\n' "$q"
} > "$T/fixture.md"
git -C "$T" add fixture.md
want="$(printf 'fixture.md:%s\n' 1 2 3 4 5 6 7 8)"
if out="$(scan "$T")" || [ "$out" != "$want" ]; then
    echo "FAIL: the scan did not report exactly the fixture lines 1-8 (got: ${out:-nothing})"
    exit 1
fi

if ! out="$(scan "$ROOT")"; then
    echo "FAIL: tracked files name an internal cluster; say \"the shared GPU cluster\" or describe its hardware:"
    printf '  %s\n' $out
    exit 1
fi
echo "ok: no tracked file names an internal cluster"
