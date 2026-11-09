#!/usr/bin/env bash
#
# hack/offline-kubectl/kubectl against stand-ins for the real kubectl: what it lets
# through and what it refuses, how it runs what it lets through (no kube context, no
# in-cluster account, its own directory off PATH), which "real" kubectls it refuses, and
# that a check's stub calling it back ends at once instead of looping (what fork-bombed
# check-setup-prereqs-render.sh when the real kubectl was looked up on PATH).
set -u
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
W="$ROOT/hack/offline-kubectl/kubectl"
EXPECT=17
n=0 failed=0
ok() { n=$((n + 1)); printf 'ok   %s\n' "$1"; }
bad() { n=$((n + 1)); failed=1; printf 'FAIL %s\n' "$1"; }
command -v timeout >/dev/null 2>&1 || { echo "FATAL: timeout(1) is required: the loop case must be bounded"; exit 1; }
T="$(mktemp -d)" || exit 1
trap 'rm -rf "$T"' EXIT

# A real kubectl that records what reached it, and the environment it ran in.
cat > "$T/real" <<EOF
#!/usr/bin/env bash
echo "real \$*" >> "$T/calls"
echo "KUBECONFIG=\${KUBECONFIG-unset} HOST=\${KUBERNETES_SERVICE_HOST-unset} PATH=\$PATH" >> "$T/env"
EOF
chmod +x "$T/real"
# run <args...>: the wrapper with the recording real kubectl and a pod's in-cluster variables.
run() { : > "$T/calls"; OFFLINE_KUBECTL_REAL="$T/real" KUBERNETES_SERVICE_HOST=10.0.0.1 KUBERNETES_SERVICE_PORT=443 \
    PATH="$(dirname "$W"):$PATH" timeout 20 "$W" "$@" > "$T/out" 2>&1; }
passes() { if run "$@" && [ "$(cat "$T/calls")" = "real $*" ]; then ok "passes: kubectl $*"; else bad "kubectl $* should reach the real kubectl: $(cat "$T/out" "$T/calls")"; fi; }
refuses() { if run "$@"; then bad "kubectl $* must be refused"
            elif [ -s "$T/calls" ]; then bad "kubectl $* reached the real kubectl"
            elif grep -q 'reads no cluster' "$T/out"; then ok "refuses: kubectl $*"
            else bad "kubectl $* failed without saying why: $(cat "$T/out")"; fi; }

refuses get pods -A
refuses get cm kustomize
refuses -n kustomize get pods
refuses apply -f x.yaml --dry-run=client
refuses apply -f x.yaml --dry-run=server
refuses delete cm x --dry-run=client
refuses version
passes kustomize config/x
passes version --client
passes create namespace x --dry-run=client -o yaml
passes annotate --local -f x.yaml a=b -o yaml

: > "$T/env"
run kustomize config/x
dir="$(cd "$(dirname "$W")" && pwd -P)"
if grep -q '^KUBECONFIG=/dev/null HOST=unset ' "$T/env" && ! grep -q "PATH=\(.*:\)\?$dir\(:\|$\)" "$T/env"; then
    ok "what passes runs with an empty kubeconfig, no in-cluster host, and the wrapper off PATH"
else bad "what passes ran with: $(cat "$T/env")"; fi

# Real kubectls that are not: none, relative, the wrapper itself, a symlink to it.
ln -s "$W" "$T/link"
for real in "" "real" "$W" "$T/link"; do
    : > "$T/calls"
    # From $T, where "real" names an executable file: only its being relative refuses it.
    if out=$(cd "$T" && OFFLINE_KUBECTL_REAL="$real" timeout 20 "$W" kustomize config/x 2>&1); then bad "OFFLINE_KUBECTL_REAL='$real' must be refused"
    elif echo "$out" | grep -q 'names no other kubectl'; then ok "OFFLINE_KUBECTL_REAL='${real/#$T/T}' is refused, and says so"
    else bad "OFFLINE_KUBECTL_REAL='$real' failed without saying why: $out"; fi
done

# A stub that, like a check's, hands --dry-run=client to "the real kubectl", which is
# the wrapper: the loop must end at once. Bounded by time and, in a subshell, by
# processes, so a broken wrapper fails this case instead of the machine.
printf '#!/usr/bin/env bash\nexec "%s" "$@"\n' "$W" > "$T/stub"
chmod +x "$T/stub"
cap=$(( $(ps -e --no-headers 2>/dev/null | wc -l) + 200 ))
( ulimit -u "$cap" 2>/dev/null; exec env OFFLINE_KUBECTL_REAL="$T/stub" PATH="$(dirname "$W"):$PATH" \
    timeout 10 "$W" create ns x --dry-run=client -o yaml ) > "$T/out" 2>&1
rc=$?
if [ "$rc" = 1 ] && grep -q 'refusing a loop' "$T/out"; then ok "a stub that calls the wrapper back is refused at once, not looped"
else bad "a stub that calls the wrapper back: rc=$rc (124 is a timeout, 127 a missing command): $(tail -2 "$T/out")"; fi

if [ "$n" != "$EXPECT" ]; then echo "FAIL: ran $n cases, expected $EXPECT"; failed=1; fi
echo "check-offline-kubectl: $n cases, $([ "$failed" = 0 ] && echo ok || echo failed)"
exit "$failed"
