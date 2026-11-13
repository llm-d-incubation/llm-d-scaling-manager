#!/usr/bin/env bash
# Executes the operator tools entry point (deploy/scaling-manager-tools.sh)
# and asserts what it does: from the checkout, or, with TOOLS_IMG=<image>, from
# the tools image the release publishes (Dockerfile.tools).
#
#   bash hack/check-tools-image.sh                      the checkout's entry point
#   TOOLS_IMG=<image> bash hack/check-tools-image.sh    the image; needs docker
#
# Every defect this covers builds, starts and prints help: an image whose
# scripts differ from the checkout's (it renders other manifests than the
# make targets), a yq or jq other than the one Dockerfile.tools pins, a
# dispatcher that drops an argument or the exit status, a workload-patch or
# ScaledObject plan that walks every namespace from a command that names one,
# an apply opt-in taken without --apply, a plan apply that rediscovers instead
# of reading its file, or an image that runs as root.
#
# Offline: workload-patch and scaledobjects-plan run against a stub kubectl that records each call
# and answers only the namespace-scoped listing, so a scan of anything else
# fails the run.
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT="$(pwd)"

for tool in jq yq; do
    command -v "$tool" >/dev/null 2>&1 || { echo "FAIL: $tool is required (not skipping: a skipped check reads as a pass)"; exit 1; }
done

T="$(mktemp -d)"
chmod 0777 "$T"
trap 'rm -rf "$T"' EXIT
FAILED=0
PASSED=0
ok()   { printf '  ok   %s\n' "$1"; PASSED=$((PASSED + 1)); }
fail() { printf '  FAIL %s\n' "$1"; FAILED=$((FAILED + 1)); }

IMG="${TOOLS_IMG:-}"
if [ -n "$IMG" ]; then
    command -v docker >/dev/null 2>&1 || { echo "FAIL: TOOLS_IMG is set and docker is not installed"; exit 1; }
    echo "Checking the tools image ${IMG}"
else
    echo "Checking the tools entry point in the checkout (TOOLS_IMG unset)"
fi

# tools <args...>: the entry point, from the image or the checkout. stdout and
# stderr as the tool writes them; the exit status is the tool's.
tools() {
    if [ -n "$IMG" ]; then
        docker run --rm --user "$(id -u):$(id -g)" "$IMG" "$@"
    else
        bash "$ROOT/deploy/scaling-manager-tools.sh" "$@"
    fi
}

# tools_stubbed <args...>: the same, with the stub kubectl first on PATH (in
# the image: mounted over its kubectl) and its call log in $T/kubectl.log.
# STUB_SCOPE, when set, is the caller's LLMSM_SCOPE.
tools_stubbed() {
    if [ -n "$IMG" ]; then
        docker run --rm --user "$(id -u):$(id -g)" \
            -v "$T/bin/kubectl:/usr/local/bin/kubectl:ro" -v "$T:/log" -e KUBECTL_LOG=/log/kubectl.log \
            ${STUB_SCOPE:+-e LLMSM_SCOPE="$STUB_SCOPE"} -w /log "$IMG" "$@"
    else
        (cd "$T" && PATH="$T/bin:$PATH" KUBECTL_LOG="$T/kubectl.log" env ${STUB_SCOPE:+LLMSM_SCOPE="$STUB_SCOPE"} \
            bash "$ROOT/deploy/scaling-manager-tools.sh" "$@")
    fi
}

mkdir -p "$T/bin"
# demo is empty; models holds one llm-d decode Deployment, whose ScaledObject
# the stub takes on `apply -f -` into applied.yaml beside the log.
cat > "$T/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$KUBECTL_LOG"
d="$(dirname "$KUBECTL_LOG")"
case "$*" in
    "get deployments -n demo -o json") echo '{"items":[]}' ;;
    "get deployments -n models -o json") cat "$d/models.json" ;;
    "get deployments qwen-decode -n models -o json") jq '.items[0]' "$d/models.json" ;;
    "get svc scaling-manager-external-scaler -n models -o name") echo service/scaling-manager-external-scaler ;;
    "apply -f -") cat > "$d/applied.yaml"; echo "scaledobject.keda.sh/qwen-decode-llmsm created" ;;
    *) echo "stub kubectl: no answer for: $*" >&2; exit 1 ;;
esac
EOF
cat > "$T/models.json" <<'EOF'
{"items":[{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"qwen-decode","namespace":"models"},
 "spec":{"replicas":1,"template":{"metadata":{"labels":{"llm-d.ai/inferenceServing":"true","llm-d.ai/role":"decode"}},
 "spec":{"containers":[{"name":"vllm","image":"quay.io/example/vllm:v1","args":["--model","Qwen/Qwen3-0.6B","--served-model-name","Qwen/Qwen3-0.6B"]}]}}}}]}
EOF
chmod 0644 "$T/models.json"
chmod 0755 "$T/bin/kubectl"

# ---------------------------------------------------------------------------
echo "The commands"
out="$(tools help 2>&1)" && rc=0 || rc=$?
missing=""
for c in prepull weights engine-cache workload-patch scaledobjects-plan scaledobjects-apply utilization-share version; do
    printf '%s' "$out" | grep -q "scaling-manager-tools $c" || missing="$missing $c"
done
if [ "$rc" = 0 ] && [ -z "$missing" ]; then ok "help exits 0 and names every command"; else fail "help: exit $rc, missing:${missing}"; fi

out="$(tools no-such-command 2>&1)" && rc=0 || rc=$?
if [ "$rc" = 2 ] && printf '%s' "$out" | grep -q "unknown command 'no-such-command'"; then
    ok "an unknown command exits 2 and says which"
else
    fail "unknown command: exit $rc, output: $(printf '%s' "$out" | head -2)"
fi

for c in prepull weights engine-cache; do
    out="$(tools "$c" --help 2>&1)" && rc=0 || rc=$?
    if [ "$rc" = 0 ] && printf '%s' "$out" | grep -q -- "--dry-run"; then ok "$c --help reaches the script"; else fail "$c --help: exit $rc"; fi
done
# utilization-share.sh prints its usage on stderr and exits 2.
out="$(tools utilization-share --help 2>&1)" && rc=0 || rc=$?
if printf '%s' "$out" | grep -q "utilization-share.sh check"; then ok "utilization-share --help reaches the script"; else fail "utilization-share --help: exit $rc"; fi
out="$(tools utilization-share check 2>&1)" && rc=0 || rc=$?
if [ "$rc" != 0 ] && printf '%s' "$out" | grep -q "models' namespace"; then ok "utilization-share check without -n exits non-zero ($rc)"; else fail "utilization-share check without -n: exit $rc"; fi
# The verb and -n reach the script: check reads the controller status in demo
# first, and a read the stub refuses is a failure, not a pass.
: > "$T/kubectl.log"
out="$(tools_stubbed utilization-share check -n demo 2>&1)" && rc=0 || rc=$?
if [ "$rc" != 0 ] && grep -q "get configmap scaling-manager-controller-status -n demo" "$T/kubectl.log"; then
    ok "utilization-share check -n demo reads the controller status in demo, and fails when it cannot"
else
    fail "utilization-share check -n demo: exit $rc, calls: $(tr '\n' ';' < "$T/kubectl.log")"
fi

# A failing script's status reaches the caller: an apply missing what it needs.
for c in prepull weights engine-cache; do
    out="$(tools "$c" apply -n demo 2>&1)" && rc=0 || rc=$?
    if [ "$rc" != 0 ] && printf '%s' "$out" | grep -q "ERROR"; then
        ok "a refused $c apply exits non-zero ($rc)"
    else
        fail "$c apply with nothing to apply: exit $rc: $(printf '%s' "$out" | head -1)"
    fi
done

# ---------------------------------------------------------------------------
echo "The manifests are the checkout's"
# Every argument reaches the script: the same render as the checkout's own
# script, byte for byte, and a non-empty one with the objects expected.
render_check() {
    local name="$1" want="$2"; shift 2
    local script="$1"; shift
    local got="$T/$name.tools.yaml" ref="$T/$name.ref.yaml" kinds
    tools "$@" > "$got" 2>"$T/$name.err" || { fail "$name: the tool exited non-zero: $(head -2 "$T/$name.err")"; return; }
    shift
    bash "$ROOT/deploy/$script" "$@" > "$ref" 2>/dev/null || { fail "$name: the checkout's deploy/$script exited non-zero"; return; }
    kinds="$(yq ea '[.] | map(select(.kind != null) | .kind) | sort | join(",")' "$got")"
    if [ "$kinds" != "$want" ]; then
        fail "$name: rendered kinds '$kinds', want '$want'"
    elif ! cmp -s "$got" "$ref"; then
        fail "$name: the render differs from deploy/$script in this checkout"
        diff "$ref" "$got" | head -10 || true
    else
        ok "$name: $want, identical to deploy/$script"
    fi
}
render_check prepull "DaemonSet,DaemonSet" prepull.sh \
    prepull apply --dry-run -n demo --image quay.io/example/vllm:v1 --image registry.example.com/engine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef \
    --node-selector example.com/accelerator=h200 --toleration example.com/dedicated
render_check weights "DaemonSet,PersistentVolume,PersistentVolumeClaim,ServiceAccount" weights.sh \
    weights apply --dry-run -n demo --model Qwen/Qwen3-0.6B --path /mnt/local/weights/demo --image quay.io/example/vllm:v1
render_check engine-cache "DaemonSet,PersistentVolume,PersistentVolumeClaim,ServiceAccount" enginecache.sh \
    engine-cache apply --dry-run -n demo --path /mnt/local/weights/demo --image quay.io/example/vllm:v1

# The image the holder runs is the one asked for, not a default.
got="$(yq ea '[select(.kind == "DaemonSet") | .spec.template.spec.containers[0].image] | sort | join(" ")' "$T/prepull.tools.yaml")"
if [ "$got" = "quay.io/example/vllm:v1 registry.example.com/engine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" ]; then
    ok "prepull holds exactly the images named"
else
    fail "prepull holder images: '$got'"
fi

# ---------------------------------------------------------------------------
echo "workload-patch"
out="$(tools workload-patch 2>&1)" && rc=0 || rc=$?
if [ "$rc" != 0 ] && printf '%s' "$out" | grep -q "needs -n"; then ok "workload-patch without -n is refused"; else fail "workload-patch without -n: exit $rc"; fi

out="$(tools workload-patch -n demo --apply-weights 2>&1)" && rc=0 || rc=$?
if [ "$rc" != 0 ] && printf '%s' "$out" | grep -q "only with --apply"; then ok "--apply-weights without --apply is refused"; else fail "--apply-weights alone: exit $rc"; fi

out="$(tools workload-patch -n 'Bad_NS' 2>&1)" && rc=0 || rc=$?
if [ "$rc" != 0 ] && printf '%s' "$out" | grep -q "not a namespace name"; then ok "a namespace that is not a name is refused"; else fail "workload-patch -n Bad_NS: exit $rc"; fi

: > "$T/kubectl.log"
# The caller's scope must not widen it: a cluster-scoped LLMSM_SCOPE alone
# scans every namespace (so_target_namespaces), and -n is what pins it.
for STUB_SCOPE in "" cluster; do
    label="workload-patch -n demo${STUB_SCOPE:+ (LLMSM_SCOPE=$STUB_SCOPE)}"
    : > "$T/kubectl.log"
    out="$(tools_stubbed workload-patch -n demo 2>&1)" && rc=0 || rc=$?
    calls="$(cat "$T/kubectl.log")"
    [ -z "${CHECK_TOOLS_VERBOSE:-}" ] || printf '    kubectl: %s\n' "$(printf '%s' "$calls" | tr '\n' ';')"
    if [ -z "$calls" ]; then
        fail "$label made no kubectl call: nothing was read"
        continue
    fi
    if printf '%s\n' "$calls" | grep -qx "get deployments -n demo -o json"; then
        ok "$label lists the Deployments in demo"
    else
        fail "$label never listed deployments in demo; calls: $(printf '%s' "$calls" | tr '\n' ';')"
    fi
    other="$(printf '%s\n' "$calls" | grep -E '^get (deployments|leaderworkersets|pods|statefulsets|namespaces)' | grep -v -- ' -n demo ' || true)"
    wide="$(printf '%s\n' "$calls" | grep -E -- '(^| )(-A|--all-namespaces)( |$)' || true)"
    if [ -z "$other" ] && [ -z "$wide" ]; then
        ok "$label reads no workload outside demo"
    else
        fail "$label read outside demo: $(printf '%s %s' "$other" "$wide" | tr '\n' ';')"
    fi
    if [ "$rc" = 0 ]; then ok "$label on an empty namespace exits 0"; else fail "$label: exit $rc: $(printf '%s' "$out" | tail -2 | tr '\n' ';')"; fi
done
unset STUB_SCOPE

# ---------------------------------------------------------------------------
echo "scaledobjects-plan and scaledobjects-apply"
refuse() {
    local label="$1" want="$2"; shift 2
    out="$(tools "$@" 2>&1)" && rc=0 || rc=$?
    if [ "$rc" != 0 ] && printf '%s' "$out" | grep -q -- "$want"; then ok "$label"; else fail "$label: exit $rc: $(printf '%s' "$out" | head -1)"; fi
}
refuse "scaledobjects-plan without -n is refused" "needs -n" scaledobjects-plan
refuse "scaledobjects-apply without --plan is refused" "needs --plan" scaledobjects-apply -n demo
refuse "scaledobjects-apply with no plan file is refused" "no plan at" scaledobjects-apply -n demo --plan /nonexistent/plan.yaml
refuse "scaledobjects-plan --plan is refused (it writes, with --out)" "takes --out" scaledobjects-plan -n demo --plan x.yaml
refuse "a controller namespace that is not a name is refused" "not a namespace name" scaledobjects-plan -n demo --controller-namespace 'Bad_NS'

for STUB_SCOPE in "" cluster; do
    label="scaledobjects-plan -n demo${STUB_SCOPE:+ (LLMSM_SCOPE=$STUB_SCOPE)}"
    : > "$T/kubectl.log"
    out="$(tools_stubbed scaledobjects-plan -n demo 2>&1)" && rc=0 || rc=$?
    calls="$(cat "$T/kubectl.log")"
    [ -z "${CHECK_TOOLS_VERBOSE:-}" ] || printf '    kubectl: %s\n' "$(printf '%s' "$calls" | tr '\n' ';')"
    if printf '%s\n' "$calls" | grep -qx "get deployments -n demo -o json"; then
        ok "$label lists the Deployments in demo"
    else
        fail "$label never listed deployments in demo; calls: $(printf '%s' "$calls" | tr '\n' ';')"
    fi
    other="$(printf '%s\n' "$calls" | grep -E '^get (deployments|leaderworkersets|pods|statefulsets|namespaces)' | grep -v -- ' -n demo ' || true)"
    wide="$(printf '%s\n' "$calls" | grep -E -- '(^| )(-A|--all-namespaces)( |$)' || true)"
    if [ -z "$other" ] && [ -z "$wide" ]; then
        ok "$label reads no workload outside demo"
    else
        fail "$label read outside demo: $(printf '%s %s' "$other" "$wide" | tr '\n' ';')"
    fi
    if [ "$rc" = 0 ] && printf '%s' "$out" | grep -q "No llm-d model servers found"; then
        ok "$label on an empty namespace says so and exits 0"
    else
        fail "$label: exit $rc: $(printf '%s' "$out" | tail -2 | tr '\n' ';')"
    fi
done
unset STUB_SCOPE

# A model server is planned, and the edited plan is applied as a ScaledObject:
# the whole release-only registration, end to end against the stub.
: > "$T/kubectl.log"; rm -f "$T/so-plan.yaml" "$T/applied.yaml"
out="$(tools_stubbed scaledobjects-plan -n models --out so-plan.yaml 2>&1)" && rc=0 || rc=$?
entry="$( [ -f "$T/so-plan.yaml" ] && yq '[.. | select(tag == "!!map" and .name == "qwen-decode")][0] | .apply + "|" + .modelID' "$T/so-plan.yaml" 2>/dev/null || true)"
if [ "$rc" = 0 ] && [ "$entry" = 'yes|Qwen/Qwen3-0.6B' ]; then
    ok "scaledobjects-plan -n models writes an entry for the model server: apply yes, its modelID"
else
    fail "scaledobjects-plan -n models: exit $rc, entry '$entry': $(printf '%s' "$out" | tail -2 | tr '\n' ';')"
fi
out="$(tools_stubbed scaledobjects-apply -n models --plan so-plan.yaml 2>&1)" && rc=0 || rc=$?
got="$( [ -f "$T/applied.yaml" ] && yq 'select(.kind == "ScaledObject") | .metadata.namespace + "/" + .metadata.name + "|" + .spec.scaleTargetRef.name + "|" + .spec.triggers[0].metadata.scalerAddress' "$T/applied.yaml" 2>/dev/null || true)"
case "$got" in
    "models/qwen-decode-llmsm|qwen-decode|scaling-manager-external-scaler.models"*)
        if [ "$rc" = 0 ]; then ok "scaledobjects-apply applies that plan: ScaledObject $got"; else fail "scaledobjects-apply: applied, then exit $rc"; fi ;;
    *) fail "scaledobjects-apply --plan so-plan.yaml: exit $rc, applied '$got': $(printf '%s' "$out" | tail -2 | tr '\n' ';')" ;;
esac
wide="$(grep -E -- '(^| )(-A|--all-namespaces)( |$)' "$T/kubectl.log" || true)"
if [ -z "$wide" ]; then ok "the plan and the apply read no namespace but models"; else fail "plan/apply listed across namespaces: $(printf '%s' "$wide" | tr '\n' ';')"; fi

# ---------------------------------------------------------------------------
if [ -n "$IMG" ]; then
    echo "The image"
    uid="$(docker run --rm --entrypoint /usr/bin/id "$IMG" -u)"
    if [ "$uid" != 0 ]; then ok "runs as a non-root user by default (uid $uid)"; else fail "the image runs as root"; fi
    v="$(tools version 2>&1)" || true
    want_yq="$(sed -n 's/^ARG YQ_VERSION=//p' "$ROOT/Dockerfile.tools")"
    want_jq="$(sed -n 's/^ARG JQ_VERSION=//p' "$ROOT/Dockerfile.tools")"
    want_kubectl="$(sed -n 's#^FROM registry.k8s.io/kubectl:\(v[0-9.]*\)@.*#\1#p' "$ROOT/Dockerfile.tools")"
    for want in "yq      $want_yq" "jq      $want_jq" "kubectl $want_kubectl"; do
        if [ -n "${want##* }" ] && printf '%s\n' "$v" | grep -qxF "$want"; then ok "version: $want"; else fail "version does not print '$want': $(printf '%s' "$v" | tr '\n' ';')"; fi
    done
    # The patch lands in the working directory the operator mounts, as the
    # operator's own user; and /work is writable with nothing mounted.
    rm -f "$T/probe"
    out="$(docker run --rm --user "$(id -u):$(id -g)" -v "$T:/work" --entrypoint /bin/sh "$IMG" -c 'echo written > probe' 2>&1)" && rc=0 || rc=$?
    if [ "$rc" = 0 ] && [ -s "$T/probe" ]; then ok "writes to the mounted /work as the caller's --user"; else fail "--user write to /work: exit $rc: $out"; fi
    out="$(docker run --rm --entrypoint /bin/sh "$IMG" -c 'echo written > probe && cat probe' 2>&1)" && rc=0 || rc=$?
    if [ "$rc" = 0 ] && [ "$out" = written ]; then ok "writes to /work with nothing mounted"; else fail "unmounted /work: exit $rc: $out"; fi
fi

echo
echo "check-tools-image: ${PASSED} passed, ${FAILED} failed"
[ "$FAILED" = 0 ]
