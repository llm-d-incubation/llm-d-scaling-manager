#!/usr/bin/env bash
#
# hack/offline-kubectl/kubectl against stand-ins for the real kubectl: it refuses a
# cluster read, passes a render to OFFLINE_KUBECTL_REAL, refuses with no real one named,
# and ends a loop in which a check's stub calls it back (what fork-bombed
# check-setup-prereqs-render.sh when the real kubectl was looked up on PATH).
set -u
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
W="$ROOT/hack/offline-kubectl/kubectl"
EXPECT=4
n=0 failed=0
ok() { n=$((n + 1)); printf 'ok   %s\n' "$1"; }
bad() { n=$((n + 1)); failed=1; printf 'FAIL %s\n' "$1"; }
T="$(mktemp -d)" || exit 1
trap 'rm -rf "$T"' EXIT

# A real kubectl that records what reached it.
printf '#!/usr/bin/env bash\necho "real $*" >> "%s/calls"\n' "$T" > "$T/real"
chmod +x "$T/real"

if out=$(OFFLINE_KUBECTL_REAL="$T/real" "$W" get pods -A 2>&1); then bad "a cluster read must fail"
elif [ ! -e "$T/calls" ] && echo "$out" | grep -q 'reads no cluster'; then ok "a cluster read fails and never reaches the real kubectl"
else bad "a cluster read reached the real kubectl or did not say why: $out"; fi

if OFFLINE_KUBECTL_REAL="$T/real" "$W" kustomize config/x >/dev/null 2>&1 && grep -q '^real kustomize config/x$' "$T/calls"; then
    ok "a render reaches OFFLINE_KUBECTL_REAL"
else bad "a render did not reach OFFLINE_KUBECTL_REAL: $(cat "$T/calls" 2>/dev/null)"; fi

if out=$(env -u OFFLINE_KUBECTL_REAL "$W" kustomize config/x 2>&1); then bad "with no real kubectl named, a render must fail"
else ok "with no real kubectl named, a render fails: ${out%%$'\n'*}"; fi

# A stub that, like a check's, hands --dry-run=client to "the real kubectl", which is the wrapper.
printf '#!/usr/bin/env bash\nexec "%s" "$@"\n' "$W" > "$T/stub"
chmod +x "$T/stub"
if timeout 20 env OFFLINE_KUBECTL_REAL="$T/stub" "$W" create ns x --dry-run=client -o yaml >/dev/null 2>&1; then
    bad "a stub that calls the wrapper back must not succeed"
elif [ "$?" = 124 ]; then bad "a stub that calls the wrapper back loops until the timeout"
else ok "a stub that calls the wrapper back is refused at once, not looped"; fi

if [ "$n" != "$EXPECT" ]; then echo "FAIL: ran $n cases, expected $EXPECT"; failed=1; fi
echo "check-offline-kubectl: $n cases, $([ "$failed" = 0 ] && echo ok || echo failed)"
exit "$failed"
