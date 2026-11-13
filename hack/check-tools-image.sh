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
# of reading its file, a warm pool applied from an image that cannot create
# one, a --controller-namespace that never reaches the scaler address, a
# command whose arguments differ from what its make target passes (each of 21
# targets, compared through a stub bash), a patch file that does not land in
# the mounted working directory, or an image that runs as root or cannot read
# a 0600 kubeconfig as the caller.
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
printf 'LLMSM_NS=%s APPLY=%s WEIGHTS=%s CACHE=%s\n' "${LLMSM_NS:-}" "${LLMSM_WORKLOAD_PATCH_APPLY:-}" "${LLMSM_WORKLOAD_PATCH_APPLY_WEIGHTS:-}" "${LLMSM_WORKLOAD_PATCH_APPLY_ENGINE_CACHE:-}" >> "$d/env.log"
case "$*" in
    "get deployments -n demo -o json") echo '{"items":[]}' ;;
    "get deployments -n models -o json") cat "$d/models.json" ;;
    "get deployments qwen-decode -n models -o json") jq '.items[0]' "$d/models.json" ;;
    "get svc scaling-manager-external-scaler -n models -o name") echo service/scaling-manager-external-scaler ;;
    "get svc scaling-manager-external-scaler -n ctl -o name") echo service/scaling-manager-external-scaler ;;
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
refuse "weights --memory with no value says so" "--memory needs a value" weights apply -n demo --memory

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
echo "--controller-namespace and the apply opt-ins reach the libraries"
# A cluster-wide controller: the plan's scaler address names its namespace,
# not the model namespace.
: > "$T/kubectl.log"; rm -f "$T/ctl-plan.yaml" "$T/applied.yaml"
out="$(tools_stubbed scaledobjects-plan -n models --controller-namespace ctl --out ctl-plan.yaml 2>&1)" && rc=0 || rc=$?
out2="$(tools_stubbed scaledobjects-apply -n models --controller-namespace ctl --plan ctl-plan.yaml 2>&1)" && rc2=0 || rc2=$?
got="$( [ -f "$T/applied.yaml" ] && yq 'select(.kind == "ScaledObject") | .spec.triggers[0].metadata.scalerAddress' "$T/applied.yaml" 2>/dev/null || true)"
case "$rc|$rc2|$got" in
    "0|0|scaling-manager-external-scaler.ctl."*) ok "--controller-namespace ctl: the applied ScaledObject calls the scaler in ctl ($got)" ;;
    *) fail "--controller-namespace ctl: plan exit $rc, apply exit $rc2, scalerAddress '$got': $(printf '%s' "$out2" | tail -2 | tr '\n' ';')" ;;
esac

# What workload-patch exports, as the stub kubectl sees it.
: > "$T/env.log"
tools_stubbed workload-patch -n demo >/dev/null 2>&1 || true
plain="$(sort -u "$T/env.log" | head -1)"
: > "$T/env.log"
tools_stubbed workload-patch -n demo --controller-namespace ctl --apply --apply-weights --apply-engine-cache >/dev/null 2>&1 || true
opted="$(sort -u "$T/env.log" | head -1)"
if [ "$plain" = "LLMSM_NS=demo APPLY= WEIGHTS= CACHE=" ] && [ "$opted" = "LLMSM_NS=ctl APPLY=true WEIGHTS=true CACHE=true" ]; then
    ok "workload-patch: --controller-namespace sets LLMSM_NS, --apply* the three LLMSM_WORKLOAD_PATCH_APPLY* variables"
else
    fail "workload-patch exports: without flags '$plain', with them '$opted'"
fi

# ---------------------------------------------------------------------------
echo "The patch file lands in the working directory"
rm -f "$T/out-patch.yaml"
if [ -n "$IMG" ]; then
    out="$(docker run --rm --user "$(id -u):$(id -g)" -v "$T/bin/kubectl:/usr/local/bin/kubectl:ro" \
        -v "$T:/work" -e KUBECTL_LOG=/work/kubectl.log "$IMG" workload-patch -n models --out out-patch.yaml 2>&1)" && rc=0 || rc=$?
else
    out="$(tools_stubbed workload-patch -n models --out out-patch.yaml 2>&1)" && rc=0 || rc=$?
fi
if [ -s "$T/out-patch.yaml" ] \
    && [ "$(yq ea '[select(.kind == "Deployment") | .metadata.name] | join(",")' "$T/out-patch.yaml" 2>/dev/null)" = qwen-decode ] \
    && yq ea 'select(.kind == "Deployment") | .spec.template.spec.containers[] | select(.name == "vllm") | .lifecycle.preStop' "$T/out-patch.yaml" 2>/dev/null | grep -q . \
    && grep -q 'VLLM_CACHE_ROOT' "$T/out-patch.yaml"; then
    ok "workload-patch -n models --out out-patch.yaml writes the drain hook and the cache variables for qwen-decode (exit $rc)"
else
    fail "workload-patch --out: exit $rc, file $( [ -s "$T/out-patch.yaml" ] && echo written || echo missing): $(printf '%s' "$out" | tail -2 | tr '\n' ';')"
fi

# ---------------------------------------------------------------------------
echo "Warm pools need a checkout"
printf 'plan: []\nwarmPools:\n  - name: pool-a\n    namespace: models\n    apply: "yes"\n  - name: pool-b\n    namespace: models\n    apply: "no"\n' > "$T/wp-plan.yaml"
if [ -n "$IMG" ]; then
    out="$(tools_stubbed scaledobjects-apply -n models --plan wp-plan.yaml 2>&1)" && rc=0 || rc=$?
else
    # The image carries no deploy/warmpool.sh; neither does this copy.
    rm -rf "$T/copy"; mkdir -p "$T/copy"
    cp "$ROOT/deploy/scaling-manager-tools.sh" "$ROOT/deploy/prepull.sh" "$ROOT/deploy/weights.sh" \
        "$ROOT/deploy/enginecache.sh" "$ROOT/deploy/utilization-share.sh" "$T/copy/"
    cp -R "$ROOT/deploy/lib" "$T/copy/lib"
    out="$(cd "$T" && PATH="$T/bin:$PATH" KUBECTL_LOG="$T/kubectl.log" bash "$T/copy/scaling-manager-tools.sh" scaledobjects-apply -n models --plan wp-plan.yaml 2>&1)" && rc=0 || rc=$?
fi
if [ "$rc" != 0 ] && printf '%s' "$out" | grep -q "warm pool(s) pool-a to apply: yes. Warm pools need a checkout"; then
    ok "scaledobjects-apply refuses a plan that sets a warm pool to apply: yes, naming it"
else
    fail "a warm pool set to yes: exit $rc: $(printf '%s' "$out" | tail -1)"
fi

# ---------------------------------------------------------------------------
echo "The apply and delete commands the docs print run as written"
# The mapping lives in prose as well as in the table below: the entry point's
# header, the guide, the reference page's make-to-tool table. A delete printed
# without --all (or --image / --model) is refused by the script, and nothing
# else would notice the page saying it. Each documented prepull, weights and
# engine-cache apply or delete is run here, its placeholders filled, with
# --dry-run, which reads no cluster and must exit 0.
DOC_FILES="deploy/scaling-manager-tools.sh README.md docs/reference/workload-preparation.md"
for f in "$ROOT"/docs/guides/*/README.md "$ROOT"/docs/guides/*/guide.yaml; do
    DOC_FILES="$DOC_FILES ${f#"$ROOT"/}"
done
fill() {
    sed -e 's/<ns>/demo/g' -e 's/\$NAMESPACE/demo/g' -e 's/\${NAMESPACE}/demo/g' \
        -e 's#<img>#quay.io/example/vllm:v1#g' -e 's#\$ENGINE_IMAGE#quay.io/example/vllm:v1#g' \
        -e 's#<hugging face id>#Qwen/Qwen3-0.6B#g' -e 's#<id>#Qwen/Qwen3-0.6B#g' \
        -e 's#<dir>#/mnt/local/weights/demo#g' -e 's#\$NODE_DIR#/mnt/local/weights/demo#g'
}
before_docs=$FAILED
doc_cmds=0
doc_files_with_delete=""
for f in $DOC_FILES; do
    while IFS= read -r line; do
        [ -n "$line" ] || continue
        doc_cmds=$((doc_cmds + 1))
        case "$line" in *" delete "*) case " $doc_files_with_delete " in *" $f "*) ;; *) doc_files_with_delete="$doc_files_with_delete $f" ;; esac ;; esac
        cmd="$(printf '%s' "$line" | fill)"
        case "$cmd" in *"<"*|*'$'*) fail "$f: a documented command with a placeholder this check cannot fill: $line"; continue ;; esac
        case " $cmd " in *" --dry-run "*) ;; *) cmd="$cmd --dry-run" ;; esac
        # shellcheck disable=SC2086
        out="$(tools $cmd 2>&1)" && rc=0 || rc=$?
        [ "$rc" = 0 ] || fail "$f documents \`$line\`, which the script refuses (exit $rc): $(printf '%s' "$out" | grep -m1 -E 'ERROR|needs|unknown' || printf '%s' "$out" | tail -1)"
    done < <(grep -ohE '(prepull|weights|engine-cache) (apply|delete) -n [^`|#(]*' "$ROOT/$f" | sed -e 's/ > .*$//' -e 's/[[:space:]]*$//' | sort -u)
done
# Every page that documents the mapping must have been read: an extraction
# that finds nothing passes everything.
for f in deploy/scaling-manager-tools.sh docs/reference/workload-preparation.md docs/guides/prepare-workloads/guide.yaml docs/guides/prepare-workloads/README.md; do
    case " $doc_files_with_delete " in *" $f "*) ;; *) fail "found no delete command in $f: the extraction read nothing there" ;; esac
done
if [ "$doc_cmds" -ge 12 ] && [ "$FAILED" -eq "$before_docs" ]; then
    ok "every documented apply and delete runs as written ($doc_cmds commands, delete in:$doc_files_with_delete)"
fi

# ---------------------------------------------------------------------------
echo "Each command passes what its make target passes"
# A stub bash records the deploy script each side runs and its arguments; any
# other call (make's recipe shell) goes to the real bash. Checkout only: the
# image has no make, and runs this same entry point.
if [ -z "$IMG" ]; then
    command -v make >/dev/null 2>&1 || { fail "make is required for the make-target comparison (not skipping)"; }
    REAL_BASH="$(command -v bash)"
    mkdir -p "$T/sbin"
    printf '%s\n' '#!/bin/sh' \
        'case "$1" in' \
        '    */prepull.sh|*/weights.sh|*/enginecache.sh|*/utilization-share.sh)' \
        '        s="${1##*/}"; shift; printf "%s %s\n" "$s" "$*" >> "$ARGV_LOG"; exit 0 ;;' \
        'esac' \
        "exec $REAL_BASH \"\$@\"" > "$T/sbin/bash"
    chmod 0755 "$T/sbin/bash"
    argv_of() {
        : > "$T/argv.log"
        if [ "$1" = make ]; then
            shift
            (cd "$ROOT" && env -u NAMESPACE -u CONTROLLER_NAMESPACE -u LLMSM_NS -u LLMSM_NAMESPACE -u LLMSM_WATCH_NS -u IMAGES -u PREPULL_NODE_SELECTOR -u PREPULL_TOLERATIONS -u POLICY_NAMESPACE -u RESTART GOTOOLCHAIN=local PATH="$T/sbin:$PATH" ARGV_LOG="$T/argv.log" make -s --no-print-directory "$@" >/dev/null 2>&1) || true
        else
            env -u NAMESPACE -u CONTROLLER_NAMESPACE -u LLMSM_NS -u LLMSM_NAMESPACE -u LLMSM_WATCH_NS -u IMAGES -u PREPULL_NODE_SELECTOR -u PREPULL_TOLERATIONS -u POLICY_NAMESPACE -u RESTART GOTOOLCHAIN=local PATH="$T/sbin:$PATH" ARGV_LOG="$T/argv.log" "$REAL_BASH" "$ROOT/deploy/scaling-manager-tools.sh" "$@" >/dev/null 2>&1 || true
        fi
        tr -s ' ' < "$T/argv.log" | sed 's/ $//'
    }
    before=$FAILED
    rows=0
    while IFS='|' read -r mk tl; do
        [ -n "$mk" ] || continue
        rows=$((rows + 1))
        # shellcheck disable=SC2086
        want="$(eval "argv_of make $mk")"
        # shellcheck disable=SC2086
        got="$(eval "argv_of $tl")"
        if [ -n "$want" ] && [ "$want" = "$got" ]; then
            :
        else
            fail "make $mk ran [$want]; the tool ($tl) ran [$got]"
        fi
    done <<'ROWS'
prepull IMAGES=q/a:1,q/b:2 NAMESPACE=demo|prepull apply -n demo --image q/a:1 --image q/b:2
prepull IMAGES=q/a:1 NAMESPACE=demo PREPULL_NODE_SELECTOR=k=v PREPULL_TOLERATIONS=t1,t2|prepull apply -n demo --node-selector k=v --toleration t1 --toleration t2 --image q/a:1
prepull-status NAMESPACE=demo|prepull status -n demo
prepull-status NAMESPACE=demo IMAGES=q/a:1 PREPULL_NODE_SELECTOR=k=v|prepull status -n demo --node-selector k=v --image q/a:1
prepull-delete NAMESPACE=demo|prepull delete -n demo --all
prepull-delete NAMESPACE=demo IMAGES=q/a:1|prepull delete -n demo --image q/a:1
weights WEIGHTS_MODEL=Qwen/Q WEIGHTS_PATH=/mnt/local/w WEIGHTS_IMAGE=q/a:1 NAMESPACE=demo WEIGHTS_HF_TOKEN_SECRET=hf/tok WEIGHTS_CAPACITY=100Gi WEIGHTS_MEMORY=8Gi|weights apply -n demo --model Qwen/Q --path /mnt/local/w --image q/a:1 --hf-token-secret hf/tok --capacity 100Gi --memory 8Gi
weights-status NAMESPACE=demo WEIGHTS_MODEL=Qwen/Q|weights status -n demo --model Qwen/Q
weights-delete NAMESPACE=demo|weights delete -n demo --all
weights-delete NAMESPACE=demo WEIGHTS_MODEL=Qwen/Q|weights delete -n demo --model Qwen/Q
engine-cache ENGINE_CACHE_PATH=/mnt/local/w ENGINE_CACHE_IMAGE=q/a:1 NAMESPACE=demo ENGINE_CACHE_CAPACITY=50Gi ENGINE_CACHE_SEED_CLAIM=seed:sub|engine-cache apply -n demo --path /mnt/local/w --image q/a:1 --capacity 50Gi --seed-claim seed:sub
engine-cache-status NAMESPACE=demo|engine-cache status -n demo
engine-cache-delete NAMESPACE=demo|engine-cache delete -n demo
engine-cache-publish NAMESPACE=demo|engine-cache publish -n demo
engine-cache-keys NAMESPACE=demo ENGINE_CACHE_PODS='p1 p2'|engine-cache keys -n demo p1 p2
engine-cache-verify NAMESPACE=demo ENGINE_CACHE_KEYS=k1,k2|engine-cache verify -n demo --keys k1,k2
utilization-share-check NAMESPACE=demo|utilization-share check -n demo
utilization-share-shadow NAMESPACE=demo PHYSICAL_GROUPS=true|utilization-share shadow -n demo --physical-groups
utilization-share-activate NAMESPACE=demo GRANT_SHARED=true RESTART=1|utilization-share activate -n demo --restart --grant-shared
utilization-share-status NAMESPACE=demo CONTROLLER_NAMESPACE=ctl|utilization-share status -n demo --controller-namespace ctl
utilization-share-off NAMESPACE=demo POLICY_NAMESPACE=pol|utilization-share off -n demo --policy-namespace pol
ROWS
    [ "$rows" -ge 21 ] || fail "only $rows make rows ran"
    [ "$FAILED" -ne "$before" ] || ok "all $rows make targets and their tool commands run the same script with the same arguments"
fi

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
    # A kubeconfig only its owner may read (0600, as kubectl writes it),
    # mounted as the guide mounts it: the caller's --user reads it.
    printf 'apiVersion: v1\nkind: Config\nclusters:\n- name: c\n  cluster:\n    server: https://127.0.0.1:1\ncontexts:\n- name: check-context\n  context:\n    cluster: c\n    user: u\ncurrent-context: check-context\nusers:\n- name: u\n  user:\n    token: not-a-token\n' > "$T/smt.kubeconfig"
    chmod 0600 "$T/smt.kubeconfig"
    out="$(docker run --rm --user "$(id -u):$(id -g)" -v "$T/smt.kubeconfig:/kube/config:ro" --entrypoint kubectl "$IMG" config current-context 2>&1)" && rc=0 || rc=$?
    if [ "$rc" = 0 ] && [ "$out" = check-context ]; then ok "reads a 0600 kubeconfig mounted at /kube/config as the caller"; else fail "0600 kubeconfig: exit $rc: $out"; fi
fi

echo
echo "check-tools-image: ${PASSED} passed, ${FAILED} failed"
[ "$FAILED" = 0 ]
