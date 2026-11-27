#!/usr/bin/env bash
#
# Renders the release files (deploy/release-manifests.sh) and asserts the
# rules of the one manifest, scaling-manager.yaml, which
# proposal PR #404 sets: applied with
# `kubectl apply --server-side -f`, again on every upgrade, so
#
#   the image by tag                    installs whatever the tag is today
#   a Secret with data                  a re-apply empties the certificates
#                                       the controller wrote
#   the settings ConfigMap, a           a re-apply resets what an operator set,
#   ScaledObject                        or scales before anyone reviewed it
#   a Secret read without resourceNames reads every Secret in reach
#   create on Secrets                   a service-account token under our name
#   a switch missing on the Deployment  the install runs as another path
#   not passive at start                it scales before anyone said so
#   a namespace other than its own      a file to edit before applying it
#                                       (KEDA's side is a file of its own,
#                                       applied with -n)
#   a placeholder                       nothing fills it on this path
#   a release script not summed         an unsigned script runs kubectl
#
# Then it runs the same assertions on broken copies, one fault each, and
# requires each to fail with the message of the rule it breaks.
#
# Needs yq v4, jq, sha256sum, GNU tar and kustomize (or kubectl); reads no
# cluster.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
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
DIGEST="sha256:$(printf 'manifest-install-check' | sha256sum | cut -c1-64)"
VERSION="v0.13.0-check.1"
M=scaling-manager.yaml
NS=scaling-manager-system
FLAGS="--watch-namespaces-live=true --keda-namespace=keda --self-check-interval=1m --restart-on-settings-change=true --external-scaler-tls-secrets=opaque --wait-for-settings=true --external-scaler-tls-live=true --external-scaler-tls-bootstrap=true"

GOOD="$WORK/good"
if ! bash "$ROOT/deploy/release-manifests.sh" --image "$REPO@$DIGEST" --version "$VERSION" --out "$GOOD" > "$WORK/render.log" 2>&1; then
    echo "FAIL: the render failed:"; tail -5 "$WORK/render.log"; exit 1
fi

# check <dir>: every rule on <dir>/scaling-manager.yaml. Prints a line per
# failure; returns how many.
check() {
    local d="$1" f="$1/$M" js="$WORK/m.json" bad=0 n k
    say() { echo "  FAIL: $*"; bad=$((bad + 1)); }
    [ -s "$f" ] || { say "$M was not written"; return "$bad"; }
    grep -q "  $M\$" "$d/SHA256SUMS" || say "SHA256SUMS does not list $M"
    yq ea -o=json -I=0 '[.] | map(select(.kind != null))' "$f" > "$js" 2>"$WORK/yerr" || { say "$M does not parse: $(head -1 "$WORK/yerr")"; return "$bad"; }
    q() { jq -r --arg img "$REPO@$DIGEST" --arg v "$VERSION" --arg ns "$NS" "$1" "$js"; }

    [ "$(q '[.[] | select(.kind == "Namespace")] | map(.metadata.name) | join(",")')" = "$NS" ] \
        || say "$M must hold exactly one Namespace, $NS; holds: $(q '[.[] | select(.kind == "Namespace") | .metadata.name] | join(",")')"
    [ "$(q '.[0].kind + "/" + .[0].metadata.name')" = "Namespace/$NS" ] || say "the Namespace is not the first object, so the apply of the rest can fail on a fresh cluster"
    n="$(q '.[] | select(.kind == "Deployment" or .kind == "Job") | .spec.template.spec.containers[] | select(.image != $img) | .image')"
    [ -z "$n" ] && [ "$(q '[.[] | select(.kind == "Deployment")] | length')" = 1 ] || say "the controller image is not pinned to $REPO@$DIGEST, or not one Deployment: $n"
    grep -qE 'llm-d-scaling-manager:[A-Za-z0-9]' "$f" && say "$M names the image by tag"
    n="$(q '.[] | select((.kind == "ConfigMap" and (.metadata.name | test("settings|policy|manager-config"))) or .kind == "ScaledObject") | .kind + "/" + .metadata.name')"
    [ -z "$n" ] || say "$M holds what an operator owns (settings, policy, a ScaledObject): $n"
    n="$(q '.[] | select(.kind == "Secret" and .type != "kubernetes.io/service-account-token") | select(.type != "Opaque" or has("data") or has("stringData")) | .metadata.namespace + "/" + .metadata.name + " " + (.type // "-")')"
    [ -z "$n" ] || say "a Secret that is not Opaque with no data, which a re-apply would empty: $n"
    for k in scaling-manager-scaler-ca scaling-manager-scaler-serving scaling-manager-scaler-client; do
        [ "$(jq -r --arg k "$k" --arg ns "$NS" '[.[] | select(.kind == "Secret" and .metadata.name == $k and .metadata.namespace == $ns)] | length' "$js")" = 1 ] \
            || say "the scaler TLS Secret $k is not in $NS"
    done
    n="$(q '[.[] | select(.kind == "Deployment") | .spec.template.spec.containers[] | select(.name == "manager") | .env[]? | select(.name == "LLMSM_PASSIVE") | .value] | join(",")')"
    [ "$n" = true ] || say "the controller does not start passive (LLMSM_PASSIVE '$n', want true): it would scale before anyone set passive false"
    n="$(q '[.[] | select(.kind == "NetworkPolicy" and .metadata.name == "scaling-manager-external-scaler") | .spec.ingress[] | select(has("from")) | .from[] | .namespaceSelector.matchLabels | tostring] | join(" ")')"
    [ "$n" = '{"scaling-manager.llmd.ai/keda":"true"}' ] \
        || say "the scaler NetworkPolicy does not admit KEDA by the namespace label scaling-manager.llmd.ai/keda=true, so KEDA elsewhere needs an edit: $n"
    n="$(q '.[] | select(.kind == "Role" or .kind == "ClusterRole") | select(any(.rules[]?; ((.resources // []) | (index("secrets") or index("*"))) and ((.verbs // []) | (index("get") or index("list") or index("watch") or index("*"))) and ((.resourceNames // []) | length) == 0)) | .kind + "/" + .metadata.name')"
    [ -z "$n" ] || say "reads Secrets with no resourceNames: $n"
    n="$(q '.[] | select(.kind == "Role" or .kind == "ClusterRole") | select(any(.rules[]?; ((.resources // []) | (index("secrets") or index("*"))) and ((.verbs // []) | (index("create") or index("*"))))) | .kind + "/" + .metadata.name')"
    [ -z "$n" ] || say "grants create on Secrets: $n"
    for k in $FLAGS; do
        [ "$(jq -r --arg k "$k" '[.[] | select(.kind == "Deployment") | .spec.template.spec.containers[] | select(.name == "manager") | .args[] | select(. == $k)] | length' "$js")" = 1 ] \
            || say "the Deployment does not pass $k"
    done
    n="$(q '.[] | select(.metadata.labels["app.kubernetes.io/part-of"] != "scaling-manager" or .metadata.labels["app.kubernetes.io/instance"] != $ns or .metadata.labels["app.kubernetes.io/version"] != $v) | .kind + "/" + .metadata.name')"
    [ -z "$n" ] || say "objects missing part-of, instance $NS or version (the uninstall selects by them): $n"
    n="$(q '.[] | select(.metadata.namespace != null and .metadata.namespace != $ns) | .kind + "/" + .metadata.namespace + "/" + .metadata.name')"
    [ -z "$n" ] || say "an object outside $NS, which a KEDA or model namespace elsewhere would make an operator edit: $n"
    n="$(grep -nE '(namespace|metadata\.name): keda$' "$f" || true)"
    [ -z "$n" ] || say "$M names KEDA's namespace: $(echo "$n" | head -2 | tr '\n' ' ')"
    grep -q '^# .*sed ' "$f" && say "$M's header gives an edit to make before applying it"
    local sc
    for sc in scaling-manager-admin.sh scaling-manager-owner.sh; do
        [ -s "$d/$sc" ] || say "$sc was not written beside the manifests"
        grep -q "  $sc\$" "$d/SHA256SUMS" || say "SHA256SUMS does not list $sc, so it is not signed"
    done
    grep -nE '__[A-Z_]+__' "$f" >/dev/null && say "$M holds a placeholder nothing fills: $(grep -oE '__[A-Z_]+__' "$f" | sort -u | tr '\n' ' ')"

    # The plan ConfigMap: there, so the controller needs no create, and with
    # no data, so a re-apply keeps the plan the controller wrote.
    n="$(q '[.[] | select(.kind == "ConfigMap" and .metadata.name == "scaling-manager-scaledobjects-plan" and .metadata.namespace == $ns and (has("data") | not))] | length')"
    [ "$n" = 1 ] || say "the plan ConfigMap scaling-manager-scaledobjects-plan is not in $NS with no data"

    # The grants and the monitoring file: applied with kubectl apply -n, so
    # nothing names a namespace; selected by a label each; found by the
    # uninstall's instance selector.
    local g gj
    [ ! -e "$d/scaling-manager-label-guard.yaml" ] || say "scaling-manager-label-guard.yaml is written; watchNamespaces replaced the label opt-in it guarded"
    for g in scaling-manager-grants.yaml scaling-manager-monitoring.yaml scaling-manager-keda.yaml; do
        gj="$WORK/$g.json"
        [ -s "$d/$g" ] || { say "$g was not written"; continue; }
        grep -q "  $g\$" "$d/SHA256SUMS" || say "SHA256SUMS does not list $g"
        yq ea -o=json -I=0 '[.] | map(select(.kind != null))' "$d/$g" > "$gj" 2>/dev/null || { say "$g does not parse"; continue; }
        n="$(jq -r '.[] | select(.metadata.namespace != null) | .kind + "/" + .metadata.name' "$gj")"
        [ -z "$n" ] || say "$g: an object with a namespace, which kubectl apply -n does not move: $n"
        n="$(jq -r --arg ns "$NS" --arg v "$VERSION" '.[] | select(.metadata.labels["app.kubernetes.io/instance"] != $ns or .metadata.labels["app.kubernetes.io/part-of"] != "scaling-manager" or .metadata.labels["app.kubernetes.io/version"] != $v) | .kind + "/" + .metadata.name' "$gj")"
        [ -z "$n" ] || say "$g: objects the uninstall does not select (instance $NS): $n"
    done
    gj="$WORK/scaling-manager-grants.yaml.json"
    if [ -s "$gj" ]; then
        [ "$(jq -r '[.[] | .metadata.labels["scaling-manager.llmd.ai/grant"] // "none"] | sort | join(",")' "$gj")" = "dset-role-writer,dset-role-writer,pod-writer,pod-writer" ] \
            || say "the grants file does not hold a Role and a RoleBinding per grant label (pod-writer, dset-role-writer): $(jq -r '[.[] | .kind + "=" + (.metadata.labels["scaling-manager.llmd.ai/grant"] // "none")] | join(" ")' "$gj")"
        n="$(jq -r --arg ns "$NS" '. as $all | .[] | select(.kind == "RoleBinding") | . as $b
            | select(($b.subjects | length) != 1 or $b.subjects[0].kind != "ServiceAccount" or $b.subjects[0].name != "scaling-manager-controller-manager" or $b.subjects[0].namespace != $ns
                or ([$all[] | select(.kind == "Role" and .metadata.name == $b.roleRef.name and .metadata.labels["scaling-manager.llmd.ai/grant"] == $b.metadata.labels["scaling-manager.llmd.ai/grant"])] | length) != 1)
            | $b.metadata.name' "$gj")"
        [ -z "$n" ] || say "a grant RoleBinding binds another subject than the controller in $NS, or a Role of another grant: $n"
        n="$(jq -c '.[] | select(.kind == "Role" and .metadata.labels["scaling-manager.llmd.ai/grant"] == "pod-writer") | .rules' "$gj")"
        [ "$n" = '[{"apiGroups":[""],"resources":["pods"],"verbs":["patch"]}]' ] || say "the pod-writer Role grants more than patch on pods: $n"
    fi
    gj="$WORK/scaling-manager-monitoring.yaml.json"
    if [ -s "$gj" ]; then
        n="$(jq -r '.[] | select(.metadata.labels["scaling-manager.llmd.ai/monitor"] == null) | .kind + "/" + .metadata.name' "$gj")"
        [ -z "$n" ] || say "a monitoring object has no scaling-manager.llmd.ai/monitor label to select it by: $n"
        [ "$(jq -r '[.[] | .kind + "=" + .metadata.labels["scaling-manager.llmd.ai/monitor"]] | sort | join(",")' "$gj")" = "NetworkPolicy=epp-networkpolicy,PodMonitor=modelserver" ] \
            || say "the monitoring file does not hold the PodMonitor (modelserver) and the EPP NetworkPolicy (epp-networkpolicy): $(jq -r '[.[] | .kind] | join(",")' "$gj")"
        n="$(jq -c --arg ns "$NS" '.[] | select(.kind == "NetworkPolicy") | [(.spec.ingress[].from[] | {ns: .namespaceSelector.matchLabels["kubernetes.io/metadata.name"], pod: .podSelector.matchLabels["control-plane"]}), .spec.ingress[].ports[].port]' "$gj")"
        [ "$n" = '[{"ns":"'"$NS"'","pod":"controller-manager"},9090]' ] || say "the EPP NetworkPolicy admits more than the controller in $NS on port 9090: $n"
    fi
    # KEDA's side: the copy (Opaque, no data) and a Role to get and update it
    # by name, bound to the controller in $NS; no namespace in the file.
    gj="$WORK/scaling-manager-keda.yaml.json"
    if [ -s "$gj" ]; then
        [ "$(jq -r '[.[] | .kind] | sort | join(",")' "$gj")" = "Role,RoleBinding,Secret" ] \
            || say "scaling-manager-keda.yaml does not hold KEDA's copy, its Role and RoleBinding: $(jq -r '[.[] | .kind] | join(",")' "$gj")"
        n="$(jq -r '.[] | select(.kind == "Secret") | select(.type != "Opaque" or has("data") or has("stringData") or .metadata.name != "scaling-manager-external-scaler-scaling-manager-system") | .metadata.name' "$gj")"
        [ -z "$n" ] || say "KEDA's copy is not the Opaque Secret scaling-manager-external-scaler-$NS with no data: $n"
        n="$(jq -r '.[] | select(.kind == "Role") | .rules[] | select((.resources != ["secrets"]) or (.resourceNames != ["scaling-manager-external-scaler-scaling-manager-system"]) or ((.verbs | sort) != ["get", "update"])) | tostring' "$gj")"
        [ -z "$n" ] || say "the Role in KEDA's namespace grants more than get and update on KEDA's copy by name: $n"
        n="$(jq -r --arg ns "$NS" '.[] | select(.kind == "RoleBinding") | select(.subjects != [{"kind": "ServiceAccount", "name": "scaling-manager-controller-manager", "namespace": $ns}]) | .metadata.name' "$gj")"
        [ -z "$n" ] || say "the RoleBinding in KEDA's namespace binds another subject than the controller in $NS: $n"
    fi

    return "$bad"
}

if check "$GOOD" > "$WORK/good.log" 2>&1; then
    pass
    echo "  ok: $M: $(jq 'length' "$WORK/m.json") objects, every rule holds"
else
    cat "$WORK/good.log"
    fail "the render breaks the rules above"
fi

# A server-side re-apply keeps what the controller wrote only while the file
# holds no data for it: the same render twice is the same bytes.
if bash "$ROOT/deploy/release-manifests.sh" --image "$REPO@$DIGEST" --version "$VERSION" --out "$WORK/again" >/dev/null 2>&1 \
    && cmp -s "$GOOD/$M" "$WORK/again/$M"; then
    pass
else
    fail "a second render of $M differs"
fi

# Negative controls: one fault per copy; each must fail for its own rule.
mutate() {
    local name="$1" expect="$2" expr="$3" file="${4:-$M}" d="$WORK/m-$1"
    rm -rf "$d"; cp -r "$GOOD" "$d"
    case "$expr" in
        append-settings)
            printf -- '---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: scaling-manager-settings\n  namespace: %s\ndata:\n  passive: "true"\n' "$NS" >> "$d/$M" ;;
        *) yq -i "$expr" "$d/$file" ;;
    esac
    (cd "$d" && sha256sum scaling-manager*.yaml scaling-manager-admin.sh scaling-manager-owner.sh > SHA256SUMS)
    if check "$d" > "$WORK/m-$name.log" 2>&1; then
        fail "negative control '$name' was not caught"
    elif grep -q -- "$expect" "$WORK/m-$name.log"; then
        pass
        echo "  ok: caught '$name': $(grep -m1 -- "$expect" "$WORK/m-$name.log" | sed 's/^ *FAIL: //' | cut -c1-160)"
    else
        fail "negative control '$name' failed, but not for '$expect': $(grep -m1 FAIL "$WORK/m-$name.log")"
    fi
}
mutate image-by-tag "not pinned" \
    '(select(.kind == "Deployment") | .spec.template.spec.containers[0].image) = "ghcr.io/llm-d-incubation/llm-d-scaling-manager:main"'
mutate tls-secret-data "Opaque with no data" \
    '(select(.kind == "Secret" and .metadata.name == "scaling-manager-scaler-ca") | .data) = {"tls.crt": ""}'
mutate keda-copy-typed "KEDA's copy is not the Opaque Secret" \
    '(select(.kind == "Secret") | .type) = "kubernetes.io/tls"' scaling-manager-keda.yaml
mutate settings-in-file "what an operator owns" append-settings
mutate no-live-watch "does not pass --watch-namespaces-live=true" \
    '(select(.kind == "Deployment") | .spec.template.spec.containers[0].args) |= map(select(. != "--watch-namespaces-live=true"))'
mutate not-passive "does not start passive" \
    '(select(.kind == "Deployment") | .spec.template.spec.containers[0].env) |= map(select(.name != "LLMSM_PASSIVE"))'
mutate keda-by-name "does not admit KEDA by the namespace label" \
    '(select(.kind == "NetworkPolicy") | .spec.ingress[] | select(has("from")) | .from[0].namespaceSelector.matchLabels) = {"kubernetes.io/metadata.name": "keda"}'
mutate keda-object-in-manifest "an object outside" \
    '(select(.kind == "Role" and .metadata.name == "scaling-manager-scaledobjects-plan") | .metadata.namespace) = "keda"'
mutate no-keda-copy "does not pass --keda-namespace=keda" \
    '(select(.kind == "Deployment") | .spec.template.spec.containers[0].args) |= map(select(. != "--keda-namespace=keda"))'
mutate keda-role-wider "more than get and update" \
    '(select(.kind == "Role") | .rules[0].verbs) += ["list"]' scaling-manager-keda.yaml
mutate keda-file-namespaced "does not move" \
    '(select(.kind == "Secret") | .metadata.namespace) = "keda"' scaling-manager-keda.yaml
mutate keda-other-subject "binds another subject" \
    '(select(.kind == "RoleBinding") | .subjects[0].namespace) = "team-a"' scaling-manager-keda.yaml
mutate secret-read-unscoped "no resourceNames" \
    '(select(.kind == "Role" and .metadata.name == "scaling-manager-scaler-tls-opaque") | .rules[0]) |= del(.resourceNames)'
mutate placeholder-left "placeholder" \
    '(select(.kind == "Role" and .metadata.name == "scaling-manager-scaler-tls-opaque") | .rules[0].resourceNames) = ["scaling-manager-scaler-ca-__NAMESPACE__"]'
mutate unlabelled "missing part-of" \
    'del(select(.kind == "ClusterTriggerAuthentication") | .metadata.labels["app.kubernetes.io/instance"])'

mutate plan-with-data "plan ConfigMap" \
    '(select(.kind == "ConfigMap" and .metadata.name == "scaling-manager-scaledobjects-plan") | .data) = {"team-a": ""}'
mutate grant-namespaced "does not move" \
    '(select(.kind == "Role" and .metadata.labels["scaling-manager.llmd.ai/grant"] == "pod-writer") | .metadata.namespace) = "team-a"' scaling-manager-grants.yaml
mutate grant-unlabelled "per grant label" \
    'del(select(.kind == "RoleBinding" and .metadata.labels["scaling-manager.llmd.ai/grant"] == "dset-role-writer") | .metadata.labels["scaling-manager.llmd.ai/grant"])' scaling-manager-grants.yaml
mutate grant-wider "more than patch on pods" \
    '(select(.kind == "Role" and .metadata.labels["scaling-manager.llmd.ai/grant"] == "pod-writer") | .rules[0].verbs) += ["delete"]' scaling-manager-grants.yaml
mutate grant-other-subject "another subject" \
    '(select(.kind == "RoleBinding" and .metadata.labels["scaling-manager.llmd.ai/grant"] == "pod-writer") | .subjects[0].namespace) = "team-a"' scaling-manager-grants.yaml
mutate epp-policy-wider "admits more than the controller" \
    '(select(.kind == "NetworkPolicy") | .spec.ingress[0].ports) += [{"port": 8080, "protocol": "TCP"}]' scaling-manager-monitoring.yaml
mutate monitor-unselected "does not select" \
    'del(select(.kind == "PodMonitor") | .metadata.labels["app.kubernetes.io/instance"])' scaling-manager-monitoring.yaml

echo ""
echo "manifest install: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
