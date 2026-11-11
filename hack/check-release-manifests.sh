#!/usr/bin/env bash
#
# Renders the release files (deploy/release-manifests.sh) and asserts the rules
# docs/proposals/install-from-release.md sets for them. Every rule here is one
# a broken render passes `kustomize build`, `bash -n` and a YAML parser with:
#
#   the image by tag, or :main          installs, and runs whatever the tag is today
#   a Namespace in a file               `kubectl delete -f` deletes the namespace
#                                       and the model servers in it
#   the settings or policy ConfigMap    every re-apply resets what an operator set
#   a hashed ClusterRole name           `sed` cannot compute it
#   a roleRef left on the old name      the controller has no rights, silently
#   a missing or wrong part label       `apply -l part=admin` skips an object, or
#                                       hands a namespace owner a ClusterRole
#   a placeholder left after `sed`      or a name `sed` turns invalid
#
# Then it runs the same assertions on broken copies of the render, one fault
# each, and requires every one to fail: a check that passes everything is
# worse than none.
#
# Offline: kubectl runs through hack/offline-kubectl. `kubectl apply
# --dry-run=client` needs a server (it downloads the OpenAPI schema and the
# REST mapping), so the substituted files are parsed with `kubectl label
# --local` instead, which reads every object the way apply does and needs none.
# A kind cluster run is in docs/developer-guide/release-process.md.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=hack/lib/offline-kubectl.sh
source "$ROOT/hack/lib/offline-kubectl.sh"

FAIL=0
PASS=0
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

fail() { echo "  FAIL: $*"; FAIL=$((FAIL + 1)); }
pass() { PASS=$((PASS + 1)); }

for tool in yq jq sha256sum; do
    command -v "$tool" >/dev/null 2>&1 || { echo "FAIL: $tool is required (not skipping: a skipped check reads as a pass)"; exit 1; }
done

REPO="ghcr.io/llm-d-incubation/llm-d-scaling-manager"
DIGEST="sha256:$(printf 'release-manifests-check' | sha256sum | cut -c1-64)"
VERSION="v0.11.0-check.1"
PART="scaling-manager.llmd.ai/part"
MAIN_FILES="scaling-manager-namespaced.yaml scaling-manager-namespaced-openshift.yaml scaling-manager-cluster.yaml scaling-manager-cluster-openshift.yaml"
ALL_FILES="$MAIN_FILES scaling-manager-settings.yaml scaling-manager-model-namespace.yaml scaling-manager-activate.yaml scaling-manager-keda-auth.yaml scaling-manager-kueue.yaml scaling-manager-alerts.yaml scaling-manager-policy-example.yaml"

render() {
    bash "$ROOT/deploy/release-manifests.sh" --image "$1" --version "$VERSION" --out "$2"
}

# The admin part as the installer defines it, read by sourcing the installer's
# own library rather than by the renderer's parse of it.
ADMIN_SELECT="$(cd "$ROOT" && env -i PATH="$PATH" HOME="$WORK" bash -c '
    BLUE="" GREEN="" YELLOW="" RED="" NC=""
    . deploy/lib/common.sh >/dev/null 2>&1 || exit 1
    llmsm_prereq_kind_filter select')" || ADMIN_SELECT=""

# check_dir <dir>: every rule, on the files in <dir>. Prints a line per
# failure and returns how many there were. Each file is read once with yq into
# a JSON array of its objects; the predicates are jq, whose select and any
# behave the same on every version CI may have.
check_dir() {
    local d="$1" f n bad=0 sub="$WORK/sub" js="$WORK/json" keda want got
    say() { echo "  FAIL: $*"; bad=$((bad + 1)); }

    for f in $ALL_FILES SHA256SUMS; do
        [ -s "$d/$f" ] || say "$f was not written"
    done
    [ "$bad" = 0 ] || return "$bad"
    (cd "$d" && sha256sum -c --quiet SHA256SUMS >/dev/null 2>&1) || say "SHA256SUMS does not match the files"
    [ "$(wc -l < "$d/SHA256SUMS")" -eq "$(echo $ALL_FILES | wc -w)" ] || say "SHA256SUMS does not list exactly the $(echo $ALL_FILES | wc -w) release files"

    rm -rf "$js"; mkdir -p "$js"
    for f in $ALL_FILES; do
        yq ea -o=json -I=0 '[.] | map(select(.kind != null))' "$d/$f" > "$js/$f.json" 2>"$WORK/yerr" \
            || { say "$f does not parse: $(head -1 "$WORK/yerr")"; continue; }
    done
    [ "$bad" = 0 ] || return "$bad"

    q() { jq -r --arg v "$VERSION" --arg p "$PART" --arg img "$REPO@$DIGEST" "$2" "$js/$1.json"; }

    for f in $ALL_FILES; do
        [ "$(q "$f" 'length')" -gt 0 ] || say "$f holds no object"
        n="$(q "$f" '.[] | select(.metadata.labels["app.kubernetes.io/part-of"] != "scaling-manager"
                or (.metadata.labels["app.kubernetes.io/instance"] // "") == ""
                or .metadata.labels["app.kubernetes.io/version"] != $v
                or ((.metadata.labels[$p] // "") | test("^(admin|controller)$") | not))
            | .kind + "/" + .metadata.name')"
        [ -z "$n" ] || say "$f: objects missing part-of, instance, version or $PART: $(echo $n)"
        n="$(q "$f" '.[] | select(.kind == "Namespace") | .metadata.name')"
        [ -z "$n" ] || say "$f carries a Namespace: $n"
        n="$(q "$f" '.[] | select(.kind == "Deployment") | .spec.template.spec.containers[]
            | select(.image != $img or .imagePullPolicy != "IfNotPresent") | .image + " " + (.imagePullPolicy // "unset")')"
        [ -z "$n" ] || say "$f: a container not pinned to $REPO@$DIGEST with IfNotPresent: $n"
        grep -qE 'llm-d-scaling-manager:[A-Za-z0-9]' "$d/$f" && say "$f names the image by tag"
        n="$(q "$f" '.[] | select(.kind | IN("ClusterRole", "ClusterRoleBinding", "Role", "RoleBinding"))
            | .metadata.name | select(test("-[0-9a-f]{8}$"))')"
        [ -z "$n" ] || say "$f: a name with a hash suffix, which sed cannot produce: $n"
        # patch on pods can change a live Pod's image. Never cluster-wide; in a
        # namespace only through the activation file's pod-writer Role, or the
        # namespaced manager Role, which carries it for the warm pool in the
        # namespace it manages (config/components/tenant-installable).
        n="$(q "$f" '.[] | select(.kind == "Role" or .kind == "ClusterRole")
            | select(any(.rules[]?; ((.resources // []) | index("pods")) and ((.verbs // []) | (index("patch") or index("*")))))
            | select(.kind == "ClusterRole" or (.metadata.name | test("^scaling-manager-(pod-writer|manager-role(-__NAMESPACE__)?)$") | not))
            | .kind + "/" + .metadata.name')"
        [ -z "$n" ] || say "$f grants patch on pods beyond the pod-writer and namespaced manager Roles: $n"
    done

    for f in $MAIN_FILES; do
        n="$(q "$f" '.[] | select(.kind == "ConfigMap" and (.metadata.name | IN("scaling-manager-settings",
                "scaling-manager-scaling-policy-config", "scaling-manager-manager-config"))) | .metadata.name')"
        [ -z "$n" ] || say "$f carries a ConfigMap the operator edits: $n"
        # Every ClusterRoleBinding names a ClusterRole in the file, or the
        # platform's own cluster-monitoring-view.
        n="$(q "$f" '[.[] | select(.kind == "ClusterRole") | .metadata.name] as $roles
            | .[] | select(.kind == "ClusterRoleBinding" and .roleRef.name != "cluster-monitoring-view")
            | select(.roleRef.name as $r | $roles | index($r) | not) | .metadata.name + " -> " + .roleRef.name')"
        [ -z "$n" ] || say "$f: a ClusterRoleBinding whose ClusterRole is not in the file: $n"
        # The part split is the installer prereqs phase, exactly.
        if [ -n "$ADMIN_SELECT" ]; then
            n="$(q "$f" ".[] | ([.] | map($ADMIN_SELECT) | length > 0) as \$want
                | select((\$want and .metadata.labels[\$p] != \"admin\") or ((\$want | not) and .metadata.labels[\$p] != \"controller\"))
                | .kind + \"/\" + .metadata.name + \"=\" + .metadata.labels[\$p]")"
            [ -z "$n" ] || say "$f: $PART differs from the installer split (LLMSM_PREREQ_KINDS/NAMED): $(echo $n)"
        fi
        n="$(q "$f" '.[] | select(.kind == "Deployment" and .metadata.labels[$p] != "controller") | .metadata.name')"
        [ -z "$n" ] || say "$f: the Deployment is not in the controller part"
        # KEDA's namespace in the scaler NetworkPolicy (decision 6).
        keda=keda
        case "$f" in *openshift*) keda=openshift-keda ;; esac
        n="$(q "$f" '[.[] | select(.kind == "NetworkPolicy") | .spec.ingress[] | .from[]? | .namespaceSelector.matchLabels["kubernetes.io/metadata.name"]] | join(",")')"
        [ "$n" = "$keda" ] || say "$f: the scaler NetworkPolicy admits '$n', not $keda"
    done

    # Namespaced files: the placeholder wherever a namespace goes, the
    # per-install ClusterRole names carrying it, Namespace reads narrowed.
    for f in scaling-manager-namespaced.yaml scaling-manager-namespaced-openshift.yaml; do
        n="$(q "$f" '.[] | select(.metadata.namespace != null and .metadata.namespace != "__NAMESPACE__") | .kind + "/" + .metadata.name')"
        [ -z "$n" ] || say "$f: objects outside __NAMESPACE__: $(echo $n)"
        n="$(q "$f" '.[] | select((.kind == "ClusterRole" or .kind == "ClusterRoleBinding") and (.metadata.name | endswith("-__NAMESPACE__") | not)) | .metadata.name')"
        [ -z "$n" ] || say "$f: cluster-scoped names without the namespace: $(echo $n)"
        n="$(q "$f" '.[] | select(.kind == "ClusterRole") | select(any(.rules[]; ((.resources // []) | index("namespaces")) and ((.resourceNames // []) | length) == 0)) | .metadata.name')"
        [ -z "$n" ] || say "$f: a Namespace read with no resourceNames (decision 5 keeps it narrow): $n"
    done
    for f in scaling-manager-cluster.yaml scaling-manager-cluster-openshift.yaml; do
        grep -q '__[A-Z_]*NAMESPACE__' "$d/$f" && say "$f holds a placeholder; it applies as is"
        n="$(q "$f" '.[] | select(.metadata.namespace != null and .metadata.namespace != "scaling-manager-system") | .kind + "/" + .metadata.name')"
        [ -z "$n" ] || say "$f: objects outside scaling-manager-system: $(echo $n)"
    done

    # The settings: the keys and defaults the proposal documents, no more.
    n="$(q scaling-manager-settings.yaml '.[] | select(.kind == "ConfigMap") | .metadata.name + " " + (.data | tojson)')"
    [ "$n" = 'scaling-manager-settings {"prometheusURL":"","prometheusInsecureSkipVerify":"false","passive":"true","utilizationShare":"off","limiter":"","quotas":"","quotaScope":"namespace","kueue":"false","kueueBorrow":"true","kueueNamespace":"kueue-system","scaleToZero":"false","watchNamespaces":"","logVerbosity":"2"}' ] \
        || say "scaling-manager-settings.yaml: name, keys or defaults differ from the proposal: $n"

    # The activation file grants patch on pods and nothing else.
    n="$(q scaling-manager-activate.yaml '[.[] | select(.kind == "Role") | .rules] | tojson')"
    [ "$n" = '[[{"apiGroups":[""],"resources":["pods"],"verbs":["patch"]}]]' ] || say "the activation Role grants $n, not patch on pods"

    # Substitute every placeholder as the guide will, then read what is left.
    rm -rf "$sub"; mkdir -p "$sub"
    for f in $ALL_FILES; do
        sed -e 's/__NAMESPACE__/team-a/g' -e 's/__MODEL_NAMESPACE__/models-b/g' \
            -e 's/__KEDA_NAMESPACE__/keda/g' -e 's/__KUEUE_NAMESPACE__/kueue-system/g' "$d/$f" > "$sub/$f"
        grep -q '__[A-Z_]*__' "$sub/$f" && say "$f: a placeholder sed does not fill: $(grep -o '__[A-Z_]*__' "$sub/$f" | sort -u | tr '\n' ' ')"
        yq ea -o=json -I=0 '[.] | map(select(.kind != null))' "$sub/$f" > "$js/sub-$f.json" 2>/dev/null \
            || { say "$f after sed does not parse"; continue; }
        n="$(jq -r '.[] | .metadata.name, (.metadata.namespace // empty), .metadata.labels["app.kubernetes.io/instance"]
            | select(test("^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$") | not)' "$js/sub-$f.json")"
        [ -z "$n" ] || say "$f after sed: not a valid name: $(echo $n)"
        want="$(jq length "$js/sub-$f.json")"
        got="$(kubectl label --local -f "$sub/$f" release-check=1 -o name 2>"$WORK/kerr" | wc -l)"
        [ "$got" = "$want" ] || say "$f after sed: kubectl read $got of $want objects: $(head -2 "$WORK/kerr")"
    done
    return "$bad"
}

echo "Rendering the release files..."
GOOD="$WORK/good"
if ! render "$REPO@$DIGEST" "$GOOD" > "$WORK/render.log" 2>&1; then
    cat "$WORK/render.log"
    echo "FAIL: deploy/release-manifests.sh failed"
    exit 1
fi
[ -n "$ADMIN_SELECT" ] && pass || fail "could not read llmsm_prereq_kind_filter from deploy/lib/common.sh"

if check_dir "$GOOD"; then
    pass
    echo "  ok: the rendered files follow every rule"
else
    fail "the rendered files break the rules above"
fi

# The renderer refuses what would break a rule at the source.
for bad_image in "$REPO:v0.11.0" "$REPO:main@$DIGEST" "$REPO@sha256:abc"; do
    if render "$bad_image" "$WORK/refused" >/dev/null 2>&1; then
        fail "deploy/release-manifests.sh accepted --image $bad_image"
    else
        pass
    fi
done
if bash "$ROOT/deploy/release-manifests.sh" --image "$REPO@$DIGEST" --version 0.11 --out "$WORK/refused" >/dev/null 2>&1; then
    fail "deploy/release-manifests.sh accepted --version 0.11"
else
    pass
fi

# Negative controls: one fault per copy, each of which must be caught.
mutate() {
    local name="$1" file="$2" expr="$3" d="$WORK/m-$1"
    rm -rf "$d"; cp -r "$GOOD" "$d"
    yq -i "$expr" "$d/$file"
    (cd "$d" && sha256sum $ALL_FILES > SHA256SUMS)
    if check_dir "$d" > "$WORK/m-$name.log" 2>&1; then
        fail "negative control '$name' was not caught"
    else
        pass
        echo "  ok: caught '$name': $(grep -m1 FAIL "$WORK/m-$name.log" | sed 's/^ *FAIL: //')"
    fi
}
mutate image-by-tag scaling-manager-namespaced.yaml \
    '(select(.kind == "Deployment") | .spec.template.spec.containers[0].image) = "ghcr.io/llm-d-incubation/llm-d-scaling-manager:main"'
mutate namespace-object scaling-manager-cluster.yaml \
    '(select(.kind == "ServiceAccount") | .kind) = "Namespace"'
mutate policy-configmap scaling-manager-namespaced.yaml \
    '(select(.kind == "Secret") | .kind) = "ConfigMap" | (select(.kind == "ConfigMap") | .metadata.name) = "scaling-manager-scaling-policy-config"'
mutate hash-name scaling-manager-namespaced.yaml \
    '(select(.kind == "ClusterRole") | .metadata.name) |= sub("-__NAMESPACE__$", "-1a2b3c4d")'
mutate stale-roleref scaling-manager-namespaced-openshift.yaml \
    '(select(.kind == "ClusterRoleBinding" and .roleRef.name != "cluster-monitoring-view") | .roleRef.name) |= sub("-__NAMESPACE__$", "")'
mutate wrong-part scaling-manager-namespaced.yaml \
    '(select(.kind == "ClusterRole") | .metadata.labels["scaling-manager.llmd.ai/part"]) = "controller"'
mutate missing-label scaling-manager-kueue.yaml \
    'del(.metadata.labels["app.kubernetes.io/instance"])'
mutate wrong-keda scaling-manager-namespaced.yaml \
    '(select(.kind == "NetworkPolicy") | .spec.ingress[1].from[0].namespaceSelector.matchLabels["kubernetes.io/metadata.name"]) = "keda-system"'
mutate fixed-namespace scaling-manager-namespaced.yaml \
    '(select(.kind == "Service") | .metadata.namespace) = "scaling-manager-system"'
mutate active-by-default scaling-manager-settings.yaml \
    '(select(.kind == "ConfigMap") | .data.passive) = "false"'
mutate cluster-pod-write scaling-manager-namespaced.yaml \
    '(select(.kind == "ClusterRole" and .metadata.name == "scaling-manager-node-reader-role-__NAMESPACE__") | .rules) += [{"apiGroups": [""], "resources": ["pods"], "verbs": ["patch"]}]'
mutate kueue-pod-write scaling-manager-kueue.yaml \
    '(select(.kind == "Role") | .rules) += [{"apiGroups": [""], "resources": ["pods"], "verbs": ["*"]}]'
mutate unfilled-placeholder scaling-manager-keda-auth.yaml \
    '(select(.kind == "ConfigMap") | .data.secretNamespace) = "__KEDA_NS__"'
mutate undecodable-object scaling-manager-policy-example.yaml \
    '(select(.kind == "ConfigMap") | .metadata.annotations.replicas) = 3'
mutate invalid-name scaling-manager-alerts.yaml \
    '(select(.kind == "PrometheusRule") | .metadata.name) = "Alerts_X"'

# --- the release workflow: what renders, signs and uploads these files -------
WF="$ROOT/.github/workflows/ci-release.yaml"
expect_wf() {
    local got
    got="$(yq -r "$3" "$WF" 2>&1)"
    if [ "$got" = "$2" ]; then pass; else fail "ci-release.yaml: $1: got '$got', want '$2'"; fi
}
expect_wf "release-files runs after the image is pushed and signed" 'release' '[.jobs."release-files".needs] | flatten | join(",")'
expect_wf "release-files may upload (contents) and sign (id-token), nothing more" \
    'contents=write,id-token=write' '.jobs."release-files".permissions | to_entries | map(.key + "=" + .value) | sort | join(",")'
expect_wf "the image job still cannot write the release" 'read' '.jobs.release.permissions.contents'
expect_wf "the release-files checkout leaves no credentials behind" 'false' \
    '.jobs."release-files".steps[] | select(.uses == "actions/checkout*") | .with."persist-credentials"'
expect_wf "the files are rendered by the digest the image job pushed" '${{ steps.push.outputs.digest }}|${{ needs.release.outputs.digest }}' \
    '.jobs.release.outputs.digest + "|" + (.jobs."release-files".steps[] | select(.name == "Render the release files") | .env.DIGEST)'
expect_wf "the render goes through make release-manifests by that digest" 'true' \
    '.jobs."release-files".steps[] | select(.name == "Render the release files") | .run | test("make release-manifests IMG=\"\$IMAGE@\$DIGEST\"") | tostring'

# The sign step, executed with a stub cosign over the good render: one bundle
# per file, SHA256SUMS included, and a refusal when a file is missing.
mkdir -p "$WORK/wf/bin"
cat > "$WORK/wf/bin/cosign" <<'STUB'
#!/usr/bin/env bash
# Records the call and writes the bundle it names, as cosign sign-blob does.
echo "$*" >> "$STUB_LOG"
while [ $# -gt 0 ]; do case "$1" in --bundle) echo '{}' > "$2"; shift 2 ;; *) shift ;; esac; done
STUB
chmod +x "$WORK/wf/bin/cosign"
yq -r '.jobs."release-files".steps[] | select(.name == "Sign the release files") | .run' "$WF" > "$WORK/wf/sign.sh"
sign_run() {
    (cd "$WORK/wf/run" && STUB_LOG="$WORK/wf/cosign.log" PATH="$WORK/wf/bin:$PATH" bash "$WORK/wf/sign.sh") > "$WORK/wf/sign.out" 2>&1
}
rm -rf "$WORK/wf/run"; mkdir -p "$WORK/wf/run"; cp -r "$GOOD" "$WORK/wf/run/dist"; : > "$WORK/wf/cosign.log"
if sign_run; then
    want=$(( $(echo $ALL_FILES | wc -w) + 1 ))
    got="$(grep -c '^sign-blob --yes --bundle ' "$WORK/wf/cosign.log")"
    bundles="$(find "$WORK/wf/run/dist" -name '*.sigstore.json' | wc -l)"
    if [ "$got" = "$want" ] && [ "$bundles" = "$want" ] && [ -s "$WORK/wf/run/dist/SHA256SUMS.sigstore.json" ]; then
        pass
        echo "  ok: the sign step signs all $want files, SHA256SUMS included"
    else
        fail "the sign step made $got cosign calls and $bundles bundles, want $want each"
    fi
else
    fail "the sign step failed on a complete render: $(cat "$WORK/wf/sign.out")"
fi
rm -rf "$WORK/wf/run/dist"/*.yaml "$WORK/wf/run/dist"/*.sigstore.json
if sign_run; then fail "the sign step passed with no manifest rendered"; else pass; fi

echo ""
echo "release manifests: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
