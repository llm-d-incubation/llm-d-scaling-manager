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
#   a hashed or missing name suffix     `sed` cannot compute it, or two installs
#                                       share one ClusterRole
#   a binding to a Role not in the file the controller has no rights, silently
#   a subject outside the install       a grant to an account nobody chose
#   a missing or wrong part label       `apply -l part=admin` skips an object, or
#                                       hands a namespace owner a ClusterRole
#   a wider Namespace read              more than the controller reads
#   a placeholder left after `sed`      or a name `sed` turns invalid
#
# Then it runs the same assertions on broken copies of the render, one fault
# each, and requires each to fail with the message of the rule it breaks: a
# check that passes everything is worse than none.
#
# Offline: kubectl runs through hack/offline-kubectl. `kubectl apply
# --dry-run=client` needs a server (it downloads the OpenAPI schema and the
# REST mapping), so the substituted files are read with `kubectl label
# --local` instead, which decodes every object the way apply does and needs
# none. A kind run is in docs/developer-guide/release-process.md.

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
NSF="scaling-manager-namespaced.yaml scaling-manager-namespaced-openshift.yaml"
CLF="scaling-manager-cluster.yaml scaling-manager-cluster-openshift.yaml"
MAIN_FILES="$NSF $CLF"
ALL_FILES="$MAIN_FILES scaling-manager-settings.yaml scaling-manager-model-namespace.yaml scaling-manager-activate.yaml scaling-manager-keda-auth.yaml scaling-manager-scaler-tls.yaml scaling-manager-kueue.yaml scaling-manager-alerts.yaml scaling-manager-policy-example.yaml scaling-manager-metrics-reader.yaml scaling-manager-preflight.yaml scaling-manager-preflight-cluster.yaml"

render() {
    bash "$ROOT/deploy/release-manifests.sh" --image "$1" --version "$VERSION" --out "$2"
}

kustomize_build() {
    if [ -n "${KUSTOMIZE:-}" ] && [ -x "${KUSTOMIZE}" ]; then
        "$KUSTOMIZE" build "$1"
    elif command -v kustomize >/dev/null 2>&1; then
        kustomize build "$1"
    else
        kubectl kustomize "$1"
    fi
}

# The admin part as the installer defines it, read by sourcing the installer's
# own library rather than by the renderer's parse of it.
ADMIN_SELECT="$(cd "$ROOT" && env -i PATH="$PATH" HOME="$WORK" bash -c '
    BLUE="" GREEN="" YELLOW="" RED="" NC=""
    . deploy/lib/common.sh >/dev/null 2>&1 || exit 1
    llmsm_prereq_kind_filter select')" || ADMIN_SELECT=""

# The cluster-scoped names each main file must hold, from the overlay itself:
# the overlay's names, less what the release files move out (the Kueue reader,
# and on Kubernetes the metrics-reader binding with its fixed subject); a
# namespaced file adds -__NAMESPACE__ to each, a cluster file keeps them.
expected_names() {
    local scope="$1" platform="$2" suffix=""
    [ "$scope" = namespace ] && suffix="-__NAMESPACE__"
    kustomize_build "$ROOT/config/overlays/$scope-scoped/$platform" \
        | yq ea -o=json -I=0 '[.] | map(select(.kind != null))' \
        | jq -r --arg s "$suffix" --arg p "$platform" '.[]
            | select(.kind == "ClusterRole" or .kind == "ClusterRoleBinding")
            | select(.metadata.name | startswith("scaling-manager-kueue-reader-") | not)
            | select(($p == "kubernetes" and .kind == "ClusterRoleBinding" and .metadata.name == "scaling-manager-metrics-reader-rolebinding") | not)
            | .kind + "/" + .metadata.name + $s' | sort
}
mkdir -p "$WORK/names"
for scope in namespace cluster; do
    for platform in kubernetes openshift; do
        expected_names "$scope" "$platform" > "$WORK/names/$scope-$platform" 2>"$WORK/names/err"
        [ -s "$WORK/names/$scope-$platform" ] || { echo "FAIL: could not render config/overlays/$scope-scoped/$platform: $(head -2 "$WORK/names/err")"; exit 1; }
    done
done
expected_file() {
    case "$1" in
        scaling-manager-namespaced.yaml) echo namespace-kubernetes ;;
        scaling-manager-namespaced-openshift.yaml) echo namespace-openshift ;;
        scaling-manager-cluster.yaml) echo cluster-kubernetes ;;
        scaling-manager-cluster-openshift.yaml) echo cluster-openshift ;;
    esac
}

# check_dir <dir>: every rule, on the files in <dir>. Prints a line per
# failure and returns how many there were. Each file is read once with yq into
# a JSON array of its objects; the predicates are jq, whose select and any
# behave the same on every version CI may have.
check_dir() {
    local d="$1" f n bad=0 sub="$WORK/sub" js="$WORK/json" keda want got ins safile split
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

    q() { jq -r --arg v "$VERSION" --arg p "$PART" --arg img "$REPO@$DIGEST" --arg ins "${ins:-}" "$2" "$js/$1.json"; }

    for f in $ALL_FILES; do
        # The install namespace of the file, and the main file whose
        # ServiceAccounts its subjects may name.
        case "$f" in
            scaling-manager-cluster*) ins=scaling-manager-system; safile="$f" ;;
            scaling-manager-namespaced-openshift.yaml) ins=__NAMESPACE__; safile="$f" ;;
            scaling-manager-preflight.yaml) ins=__NAMESPACE__; safile="$f" ;;
            scaling-manager-preflight-cluster.yaml) ins=scaling-manager-system; safile="$f" ;;
            *) ins=__NAMESPACE__; safile=scaling-manager-namespaced.yaml ;;
        esac
        [ "$(q "$f" 'length')" -gt 0 ] || say "$f holds no object"
        # No preview notice: the files install on their own
        # (make test-e2e-release-manifests).
        head -3 "$d/$f" | grep -q '^# PREVIEW' && say "$f still opens with the PREVIEW notice"
        n="$(q "$f" '.[] | select(.metadata.labels["app.kubernetes.io/part-of"] != "scaling-manager"
                or (.metadata.labels["app.kubernetes.io/instance"] // "") == ""
                or .metadata.labels["app.kubernetes.io/version"] != $v
                or ((.metadata.labels[$p] // "") | test("^(admin|controller)$") | not))
            | .kind + "/" + .metadata.name')"
        [ -z "$n" ] || say "$f: objects missing part-of, instance, version or $PART: $(echo $n)"
        n="$(q "$f" '.[] | select(.kind == "Namespace") | .metadata.name')"
        [ -z "$n" ] || say "$f carries a Namespace: $n"
        n="$(q "$f" '.[] | select(.kind == "Deployment" or .kind == "Job") | .spec.template.spec.containers[]
            | select(.image != $img or .imagePullPolicy != "IfNotPresent") | .image + " " + (.imagePullPolicy // "unset")')"
        [ -z "$n" ] || say "$f: a container not pinned to $REPO@$DIGEST with IfNotPresent: $n"
        grep -qE 'llm-d-scaling-manager:[A-Za-z0-9]' "$d/$f" && say "$f names the image by tag"
        n="$(q "$f" '.[] | select(.kind | IN("ClusterRole", "ClusterRoleBinding", "Role", "RoleBinding"))
            | .metadata.name | select(test("-[0-9a-f]{8}$"))')"
        [ -z "$n" ] || say "$f: a name with a hash suffix, which sed cannot produce: $n"
        # patch on pods can change a live Pod image. Decision 3: it is granted
        # only by the Role in scaling-manager-activate.yaml, applied for
        # utilization share active or the warm pool. Never by a ClusterRole,
        # and never by the manager Role, which the overlay gives it for the
        # warm pool (config/components/tenant-installable).
        n="$(q "$f" '.[] | select(.kind == "Role" or .kind == "ClusterRole")
            | select(any(.rules[]?; ((.resources // []) | index("pods")) and ((.verbs // []) | (index("patch") or index("*")))))
            | .kind + "/" + .metadata.name')"
        if [ "$f" = scaling-manager-activate.yaml ]; then
            [ "$n" = "Role/scaling-manager-pod-writer" ] || say "$f: patch on pods should come from Role/scaling-manager-pod-writer alone, not: $n"
        else
            [ -z "$n" ] || say "$f grants patch on pods, which only scaling-manager-activate.yaml may: $n"
        fi
        # Secrets are read by name only: the controller's three scaler TLS
        # Secrets (scaling-manager-scaler-tls-role). A get, list or watch on
        # secrets with no resourceNames reads every Secret in reach, service
        # account tokens included.
        n="$(q "$f" '.[] | select(.kind == "Role" or .kind == "ClusterRole")
            | select(any(.rules[]?; ((.resources // []) | (index("secrets") or index("*")))
                and ((.verbs // []) | (index("get") or index("list") or index("watch") or index("*")))
                and ((.resourceNames // []) | length) == 0))
            | .kind + "/" + .metadata.name')"
        [ -z "$n" ] || say "$f reads Secrets with no resourceNames: $n"
        # Nothing creates Secrets: with create and a by-name get, the
        # controller could create a service-account-token Secret under one of
        # the scaler names and read the token. It fills the placeholders.
        n="$(q "$f" '.[] | select(.kind == "Role" or .kind == "ClusterRole")
            | select(any(.rules[]?; ((.resources // []) | (index("secrets") or index("*")))
                and ((.verbs // []) | (index("create") or index("*")))))
            | .kind + "/" + .metadata.name')"
        [ -z "$n" ] || say "$f grants create on Secrets: $n"
        # The part split. In the main files exactly the installer prereqs
        # phase; elsewhere that, plus every cluster-scoped and every
        # monitoring.coreos.com object (the stock admin role grants neither).
        if [ -n "$ADMIN_SELECT" ]; then
            split="([.] | map($ADMIN_SELECT) | length > 0)"
            case " $MAIN_FILES " in
                *" $f "*) ;;
                *) split="($split or (.kind | IN(\"ClusterRole\", \"ClusterRoleBinding\", \"ClusterTriggerAuthentication\", \"Namespace\")) or ((.apiVersion // \"\") | startswith(\"monitoring.coreos.com/\")))" ;;
            esac
            n="$(q "$f" ".[] | $split as \$want
                | select((\$want and .metadata.labels[\$p] != \"admin\") or ((\$want | not) and .metadata.labels[\$p] != \"controller\"))
                | .kind + \"/\" + .metadata.name + \"=\" + .metadata.labels[\$p]")"
            [ -z "$n" ] || say "$f: $PART differs from the installer split (LLMSM_PREREQ_KINDS/NAMED): $(echo $n)"
        fi
        # Every binding names a role in the same file: a RoleBinding a Role in
        # its own namespace, a ClusterRoleBinding a ClusterRole, bar the
        # platform-owned cluster-monitoring-view. The metrics-reader file binds
        # the ClusterRole of the namespaced file, checked below.
        if [ "$f" != scaling-manager-metrics-reader.yaml ]; then
            n="$(q "$f" '. as $all | .[] | select(.kind == "RoleBinding" or .kind == "ClusterRoleBinding")
                | . as $b
                | select(($b.kind == "ClusterRoleBinding" and $b.roleRef.name == "cluster-monitoring-view") | not)
                | select([$all[] | select(.kind == $b.roleRef.kind and .metadata.name == $b.roleRef.name
                          and ($b.roleRef.kind == "ClusterRole" or .metadata.namespace == $b.metadata.namespace))] | length == 0)
                | $b.kind + "/" + $b.metadata.name + " -> " + $b.roleRef.kind + "/" + $b.roleRef.name')"
            [ -z "$n" ] || say "$f: a binding names no Role or ClusterRole in the file: $n"
            # Every ServiceAccount subject is one of this install, in its namespace.
            n="$(jq -r --arg ins "$ins" --slurpfile sa "$js/$safile.json" '
                [$sa[0][] | select(.kind == "ServiceAccount") | .metadata.name] as $names
                | .[] | select(.kind == "RoleBinding" or .kind == "ClusterRoleBinding") | . as $b
                | .subjects[]? | select(.kind == "ServiceAccount")
                | select(.namespace != $ins or (.name as $n | $names | index($n) | not))
                | $b.metadata.name + ": " + (.namespace // "-") + "/" + .name' "$js/$f.json")"
            [ -z "$n" ] || say "$f: a ServiceAccount subject that is not this install own, in $ins: $n"
        fi
    done

    for f in $MAIN_FILES; do
        n="$(q "$f" '.[] | select(.kind == "ConfigMap" and (.metadata.name | IN("scaling-manager-settings",
                "scaling-manager-scaling-policy-config", "scaling-manager-manager-config"))) | .metadata.name')"
        [ -z "$n" ] || say "$f carries a ConfigMap the operator edits: $n"
        n="$(q "$f" '.[] | select(.kind == "Deployment" and .metadata.labels[$p] != "controller") | .metadata.name')"
        [ -z "$n" ] || say "$f: the Deployment is not in the controller part"
        # The ConfigMap is not shipped, so its volume must not block the Pod.
        n="$(q "$f" '[.[] | select(.kind == "Deployment") | .spec.template.spec.volumes[]
            | select(.configMap.name == "scaling-manager-manager-config") | .configMap.optional] | tojson')"
        [ "$n" = '[true]' ] || say "$f: the scaling-manager-manager-config volume is not optional: $n"
        # The Prometheus CA the operator creates is mounted, optional, where
        # prometheusCACertPath points: without it a private CA cannot be trusted.
        n="$(q "$f" '[.[] | select(.kind == "Deployment") | . as $d | .spec.template.spec.volumes[]
            | select(.secret.secretName == "scaling-manager-prometheus-ca" and .secret.optional == true) | .name as $v
            | $d.spec.template.spec.containers[] | select(.name == "manager") | .volumeMounts[]
            | select(.name == $v and .mountPath == "/etc/scaling-manager-prometheus-ca")] | length')"
        [ "$n" = 1 ] || say "$f: Secret scaling-manager-prometheus-ca is not mounted, optional, at /etc/scaling-manager-prometheus-ca ($n)"
        # No installer runs to mint the scaler certificates: the controller
        # provisions them (--external-scaler-tls-bootstrap), never with the
        # make path's certificate files, under the narrow Role, bound to its
        # own ServiceAccount.
        case "$f" in scaling-manager-cluster*) ins=scaling-manager-system ;; *) ins=__NAMESPACE__ ;; esac
        n="$(q "$f" '[.[] | select(.kind == "Deployment") | .spec.template.spec.containers[] | select(.name == "manager") | .args[]
            | select(startswith("--external-scaler-tls-bootstrap") or startswith("--external-scaler-cert-path") or startswith("--external-scaler-client-ca"))] | join(" ")')"
        [ "$n" = "--external-scaler-tls-bootstrap=true" ] || say "$f: the manager does not provision the scaler TLS itself (scaler TLS flags: '$n')"
        n="$(q "$f" '[.[] | select(.kind == "Role" and .metadata.name == "scaling-manager-scaler-tls-role") | .rules | map(.resourceNames //= [] | .resourceNames |= sort | .verbs |= sort)] | tojson')"
        [ "$n" = '[[{"apiGroups":[""],"resources":["secrets"],"resourceNames":["scaling-manager-external-scaler-ca","scaling-manager-external-scaler-client-tls","scaling-manager-external-scaler-tls"],"verbs":["get","update"]},{"apiGroups":[""],"resources":["configmaps"],"resourceNames":["openshift-service-ca.crt","scaling-manager-scaler-keda-auth"],"verbs":["get"]}]]' ] \
            || say "$f: the scaler TLS Role grants $n, not get and update on the three scaler Secrets and get on the KEDA marker and the service CA"
        # The placeholders live in their own create-only file: applied with
        # this one, they would empty what the controller filled.
        n="$(q "$f" '.[] | select(.kind == "Secret" and (.metadata.name | startswith("scaling-manager-external-scaler"))) | .metadata.name')"
        [ -z "$n" ] || say "$f carries a scaler TLS placeholder, which a re-apply would empty: $n"
        n="$(q "$f" '[.[] | select(.kind == "RoleBinding" and .roleRef.kind == "Role" and .roleRef.name == "scaling-manager-scaler-tls-role")
            | .subjects[] | .kind + "/" + .namespace + "/" + .name] | join(",")')"
        [ "$n" = "ServiceAccount/$ins/scaling-manager-controller-manager" ] || say "$f: the scaler TLS Role is bound to '$n', not the controller"
        keda=keda
        case "$f" in *openshift*) keda=openshift-keda ;; esac
        n="$(q "$f" '[.[] | select(.kind == "NetworkPolicy") | .spec.ingress[] | .from[]? | .namespaceSelector.matchLabels["kubernetes.io/metadata.name"]] | join(",")')"
        [ "$n" = "$keda" ] || say "$f: the scaler NetworkPolicy admits '$n', not $keda"
        # Cluster-scoped names: the overlay ones, suffixed in a namespaced file
        # and fixed in a cluster file.
        q "$f" '.[] | select(.kind == "ClusterRole" or .kind == "ClusterRoleBinding") | .kind + "/" + .metadata.name' | sort > "$WORK/got-names"
        if ! cmp -s "$WORK/got-names" "$WORK/names/$(expected_file "$f")"; then
            say "$f: cluster-scoped names differ from the overlay ones (+ or - lines): $(diff "$WORK/names/$(expected_file "$f")" "$WORK/got-names" | grep '^[<>]' | tr '\n' ' ')"
        fi
    done

    for f in $NSF; do
        n="$(q "$f" '.[] | select(.metadata.namespace != null and .metadata.namespace != "__NAMESPACE__") | .kind + "/" + .metadata.name')"
        [ -z "$n" ] || say "$f: objects outside __NAMESPACE__: $(echo $n)"
        n="$(q "$f" '.[] | select(.kind == "ClusterRole") | select(any(.rules[]; ((.resources // []) | index("namespaces")) and ((.resourceNames // []) | length) == 0)) | .metadata.name')"
        [ -z "$n" ] || say "$f: a Namespace read with no resourceNames (decision 5 keeps it narrow): $n"
        n="$(q "$f" '[.[] | select(.kind == "ClusterRole" and .metadata.name == "scaling-manager-policy-namespace-reader-role-__NAMESPACE__") | .rules[] | select(.resources | index("namespaces")) | .resourceNames] | tojson')"
        [ "$n" = '[["__NAMESPACE__","scaling-manager-policy"]]' ] || say "$f: the policy-namespace reader reads $n, not exactly __NAMESPACE__ and scaling-manager-policy"
    done
    n="$(q scaling-manager-namespaced-openshift.yaml '[.[] | select(.kind == "ServiceMonitor") | .spec.endpoints[].tlsConfig.serverName] | join(",")')"
    [ "$n" = "scaling-manager-controller-manager-metrics-service.__NAMESPACE__.svc" ] || say "scaling-manager-namespaced-openshift.yaml: ServiceMonitor serverName is '$n'"
    n="$(q scaling-manager-cluster-openshift.yaml '[.[] | select(.kind == "ServiceMonitor") | .spec.endpoints[].tlsConfig.serverName] | join(",")')"
    [ "$n" = "scaling-manager-controller-manager-metrics-service.scaling-manager-system.svc" ] || say "scaling-manager-cluster-openshift.yaml: ServiceMonitor serverName is '$n'"
    for f in $CLF; do
        grep -q '__[A-Z_]*NAMESPACE__' "$d/$f" && say "$f holds a placeholder; it applies as is"
        n="$(q "$f" '.[] | select(.metadata.namespace != null and .metadata.namespace != "scaling-manager-system") | .kind + "/" + .metadata.name')"
        [ -z "$n" ] || say "$f: objects outside scaling-manager-system: $(echo $n)"
    done

    # The Kueue file: its Namespace read names the controller namespace only.
    n="$(q scaling-manager-kueue.yaml '[.[] | select(.kind == "ClusterRole" and .metadata.name == "scaling-manager-kueue-reader-role-__NAMESPACE__") | .rules[] | select(.resources | index("namespaces")) | .resourceNames] | tojson')"
    [ "$n" = '[["__NAMESPACE__"]]' ] || say "scaling-manager-kueue.yaml: the Kueue Namespace read names $n, not exactly __NAMESPACE__"

    # The KEDA file: the marker names the ClusterTriggerAuthentication, which
    # reads the Secret of its own name in KEDA namespace.
    n="$(q scaling-manager-keda-auth.yaml '([.[] | select(.kind == "ClusterTriggerAuthentication")] | first) as $c
        | ([.[] | select(.kind == "ConfigMap" and .metadata.name == "scaling-manager-scaler-keda-auth")] | first) as $m
        | [$c.metadata.name, ([$c.spec.secretTargetRef[].name] | unique | join(",")), $m.data.clusterTriggerAuthentication, $m.data.secretNamespace] | join(" ")')"
    [ "$n" = "scaling-manager-external-scaler-__NAMESPACE__ scaling-manager-external-scaler-__NAMESPACE__ scaling-manager-external-scaler-__NAMESPACE__ __KEDA_NAMESPACE__" ] \
        || say "scaling-manager-keda-auth.yaml: ClusterTriggerAuthentication, its Secret and the marker disagree (name secret marker secretNamespace): $n"

    # The scaler TLS placeholders: the three Secrets, empty, of the type and
    # with the label the installer writes, in the controller namespace.
    n="$(q scaling-manager-scaler-tls.yaml '[.[] | [.kind, .metadata.name, .metadata.namespace, .type,
            .metadata.labels["scaling-manager.llmd.ai/scaler-tls"], (.data | tojson)] | join(" ")] | sort | join(";")')"
    [ "$n" = 'Secret scaling-manager-external-scaler-ca __NAMESPACE__ kubernetes.io/tls ca {"tls.crt":"","tls.key":"","ca.crt":""};Secret scaling-manager-external-scaler-client-tls __NAMESPACE__ kubernetes.io/tls client {"tls.crt":"","tls.key":"","ca.crt":""};Secret scaling-manager-external-scaler-tls __NAMESPACE__ kubernetes.io/tls serving {"tls.crt":"","tls.key":"","ca.crt":""}' ] \
        || say "scaling-manager-scaler-tls.yaml: the placeholders are not the three empty kubernetes.io/tls Secrets with the installer label: $n"

    # Per-model-namespace files: everything namespaced lands in the model namespace.
    for f in scaling-manager-model-namespace.yaml scaling-manager-activate.yaml; do
        n="$(q "$f" '.[] | select(.metadata.namespace != "__MODEL_NAMESPACE__") | .kind + "/" + .metadata.name')"
        [ -z "$n" ] || say "$f: objects outside __MODEL_NAMESPACE__: $(echo $n)"
    done
    n="$(q scaling-manager-activate.yaml '[.[] | select(.kind == "Role") | .rules] | tojson')"
    [ "$n" = '[[{"apiGroups":[""],"resources":["pods"],"verbs":["patch"]}]]' ] || say "the activation Role grants $n, not patch on pods"
    # The manager Roles keep reading pods: dropping patch must not drop the rule.
    for f in $NSF scaling-manager-model-namespace.yaml; do
        n="$(q "$f" '[.[] | select(.kind == "Role" and (.metadata.name | startswith("scaling-manager-manager-role"))) | .rules[] | select(.resources | index("pods")) | .verbs | sort] | tojson')"
        [ "$n" = '[["get","list","watch"]]' ] || say "$f: the manager Role pod verbs are $n, not get, list and watch"
    done

    # The metrics-reader file binds the namespaced ClusterRole to a named
    # account, and to nothing else.
    n="$(q scaling-manager-metrics-reader.yaml '[.[] | .kind + " " + .roleRef.name + " " + ([.subjects[] | .namespace + "/" + .name] | join(","))] | join(";")')"
    [ "$n" = "ClusterRoleBinding scaling-manager-metrics-reader-__NAMESPACE__ __PROMETHEUS_NAMESPACE__/__PROMETHEUS_SA__" ] \
        || say "scaling-manager-metrics-reader.yaml binds '$n'"
    grep -qx 'ClusterRole/scaling-manager-metrics-reader-__NAMESPACE__' "$WORK/names/namespace-kubernetes" \
        || say "scaling-manager-metrics-reader.yaml binds a ClusterRole the namespaced file does not hold"

    n="$(q scaling-manager-settings.yaml '.[] | select(.kind == "ConfigMap") | .metadata.name + " " + (.data | tojson)')"
    [ "$n" = 'scaling-manager-settings {"prometheusURL":"","prometheusCACertPath":"","prometheusInsecureSkipVerify":"false","passive":"true","utilizationShare":"off","limiter":"","quotas":"","quotaScope":"namespace","kueue":"false","kueueBorrow":"true","kueueNamespace":"kueue-system","scaleToZero":"false","watchNamespaces":"","logVerbosity":"2"}' ] \
        || say "scaling-manager-settings.yaml: name, keys or defaults differ from the proposal: $n"

    # The preflight files: the Job runs `manager preflight check` as its own
    # account, never the controller one, and is cleaned up and bounded in
    # time; its ClusterRole only reads (create on subjectaccessreviews asks a
    # question and grants nothing), its Role writes only the result
    # ConfigMap, and nothing reads a Secret. The namespaced file reads pods
    # and PodMonitors in its own namespace only, and Deployments by name: the
    # cluster-wide lists are the cluster file only, whose Job says so.
    for f in scaling-manager-preflight.yaml scaling-manager-preflight-cluster.yaml; do
        want="scaling-manager-preflight /manager preflight,check"
        [ "$f" = scaling-manager-preflight-cluster.yaml ] && want="$want,--scope=cluster"
        n="$(q "$f" '[.[] | select(.kind == "Job") | .spec.template.spec
            | .serviceAccountName + " " + (.containers[0].command | join(",")) + " " + (.containers[0].args | join(","))] | join(";")')"
        [ "$n" = "$want" ] || say "$f: the preflight Job runs '$n', not '$want'"
        n="$(q "$f" '[.[] | select(.kind == "Job") | .spec | ((.ttlSecondsAfterFinished // 0) > 0) and ((.activeDeadlineSeconds // 0) > 0)] | tojson')"
        [ "$n" = '[true]' ] || say "$f: the preflight Job lacks ttlSecondsAfterFinished or activeDeadlineSeconds"
        n="$(q "$f" '.[] | select(.kind == "ClusterRole") | .rules[]
            | select(((.verbs - ["get", "list", "watch"]) | length > 0) and ((.resources == ["subjectaccessreviews"] and .verbs == ["create"]) | not))
            | (.resources | join(",")) + ":" + (.verbs | join(","))')"
        [ -z "$n" ] || say "$f: the preflight ClusterRole is not read-only: $(echo $n)"
        n="$(q "$f" '.[] | select(.kind == "Role") | .rules[]
            | select((.verbs - ["get", "list", "watch"]) | length > 0)
            | select((.resources == ["configmaps"] and (.verbs == ["create"] or (.verbs == ["update"] and .resourceNames == ["scaling-manager-preflight-result"]))) | not)
            | (.resources | join(",")) + ":" + (.verbs | join(","))')"
        [ -z "$n" ] || say "$f: the preflight Role writes more than its result ConfigMap: $(echo $n)"
        n="$(q "$f" '.[] | select(.kind == "Role" or .kind == "ClusterRole") | .rules[]
            | select((.resources | index("secrets")) or (.resources | index("*"))) | .verbs | join(",")')"
        [ -z "$n" ] || say "$f: the preflight reads Secrets: $n"
    done
    n="$(q scaling-manager-preflight.yaml '.[] | select(.kind == "ClusterRole") | .rules[]
        | select(((.resources | index("pods")) or (.resources | index("podmonitors")) or ((.resources | index("deployments")) and ((.resourceNames // []) | length) == 0))
            and ((.verbs | index("list")) or (.verbs | index("watch")) or (.verbs | index("get"))))
        | (.resources | join(",")) + ":" + (.verbs | join(","))')"
    [ -z "$n" ] || say "scaling-manager-preflight.yaml: the namespaced preflight reads in every namespace: $(echo $n)"
    grep -q '__[A-Z_]*NAMESPACE__' "$d/scaling-manager-preflight-cluster.yaml" && say "scaling-manager-preflight-cluster.yaml holds a placeholder; it applies as is"

    # Substitute every placeholder as the guide will, then read what is left.
    rm -rf "$sub"; mkdir -p "$sub"
    for f in $ALL_FILES; do
        sed -e 's/__NAMESPACE__/team-a/g' -e 's/__MODEL_NAMESPACE__/models-b/g' \
            -e 's/__KEDA_NAMESPACE__/keda/g' -e 's/__KUEUE_NAMESPACE__/kueue-system/g' \
            -e 's/__PROMETHEUS_NAMESPACE__/monitoring/g' -e 's/__PROMETHEUS_SA__/prometheus-k8s/g' "$d/$f" > "$sub/$f"
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

# The renderer refuses what would break a rule at the source, and takes a
# registry with a port.
for bad_image in "$REPO:v0.11.0" "$REPO:main@$DIGEST" "$REPO@sha256:abc" "localhost:5000/scaling-manager:dev@$DIGEST" "@$DIGEST"; do
    if render "$bad_image" "$WORK/refused" >/dev/null 2>&1; then
        fail "deploy/release-manifests.sh accepted --image $bad_image"
    else
        pass
    fi
done
if render "localhost:5000/scaling-manager@$DIGEST" "$WORK/port" >/dev/null 2>&1 \
    && grep -q "image: localhost:5000/scaling-manager@$DIGEST" "$WORK/port/scaling-manager-namespaced.yaml"; then
    pass
else
    fail "deploy/release-manifests.sh refused or mis-pinned an image from a registry with a port (localhost:5000/scaling-manager@$DIGEST)"
fi
if bash "$ROOT/deploy/release-manifests.sh" --image "$REPO@$DIGEST" --version 0.11 --out "$WORK/refused" >/dev/null 2>&1; then
    fail "deploy/release-manifests.sh accepted --version 0.11"
else
    pass
fi

# Negative controls: one fault per copy. Each must fail with the message of
# the rule it breaks, not merely fail.
caught() {
    local name="$1" expect="$2" d="$3"
    (cd "$d" && sha256sum $ALL_FILES > SHA256SUMS)
    if check_dir "$d" > "$WORK/m-$name.log" 2>&1; then
        fail "negative control '$name' was not caught"
    elif grep -q -- "$expect" "$WORK/m-$name.log"; then
        pass
        echo "  ok: caught '$name': $(grep -m1 -- "$expect" "$WORK/m-$name.log" | sed 's/^ *FAIL: //' | cut -c1-200)"
    else
        fail "negative control '$name' failed, but not for '$expect': $(grep -m1 FAIL "$WORK/m-$name.log")"
    fi
}
mutate() {
    local name="$1" expect="$2" file="$3" expr="$4" d="$WORK/m-$1"
    rm -rf "$d"; cp -r "$GOOD" "$d"
    yq -i "$expr" "$d/$file"
    caught "$name" "$expect" "$d"
}
mutate image-by-tag "not pinned" scaling-manager-namespaced.yaml \
    '(select(.kind == "Deployment") | .spec.template.spec.containers[0].image) = "ghcr.io/llm-d-incubation/llm-d-scaling-manager:main"'
mutate namespace-object "carries a Namespace" scaling-manager-cluster.yaml \
    '(select(.kind == "ServiceAccount") | .kind) = "Namespace"'
mutate policy-configmap "ConfigMap the operator edits" scaling-manager-namespaced.yaml \
    '(select(.kind == "Secret") | .kind) = "ConfigMap" | (select(.kind == "ConfigMap") | .metadata.name) = "scaling-manager-scaling-policy-config"'
mutate hash-name "hash suffix" scaling-manager-namespaced.yaml \
    '(select(.kind == "ClusterRole") | .metadata.name) |= sub("-__NAMESPACE__$", "-1a2b3c4d")'
mutate stale-roleref "names no Role or ClusterRole" scaling-manager-namespaced-openshift.yaml \
    '(select(.kind == "ClusterRoleBinding" and .roleRef.name != "cluster-monitoring-view") | .roleRef.name) |= sub("-__NAMESPACE__$", "")'
mutate wrong-part "differs from the installer split" scaling-manager-namespaced.yaml \
    '(select(.kind == "ClusterRole") | .metadata.labels["scaling-manager.llmd.ai/part"]) = "controller"'
mutate missing-label "missing part-of" scaling-manager-kueue.yaml \
    'del(.metadata.labels["app.kubernetes.io/instance"])'
mutate wrong-keda "admits" scaling-manager-namespaced.yaml \
    '(select(.kind == "NetworkPolicy") | .spec.ingress[1].from[0].namespaceSelector.matchLabels["kubernetes.io/metadata.name"]) = "keda-system"'
mutate fixed-namespace "outside __NAMESPACE__" scaling-manager-namespaced.yaml \
    '(select(.kind == "Service") | .metadata.namespace) = "scaling-manager-system"'
mutate active-by-default "defaults differ" scaling-manager-settings.yaml \
    '(select(.kind == "ConfigMap") | .data.passive) = "false"'
mutate cluster-pod-write "patch on pods" scaling-manager-namespaced.yaml \
    '(select(.kind == "ClusterRole" and .metadata.name == "scaling-manager-node-reader-role-__NAMESPACE__") | .rules) += [{"apiGroups": [""], "resources": ["pods"], "verbs": ["patch"]}]'
mutate manager-role-pod-patch "patch on pods" scaling-manager-namespaced.yaml \
    '(select(.kind == "Role" and .metadata.name == "scaling-manager-manager-role") | .rules[] | select(.resources | contains(["pods"])) | .verbs) += ["patch"]'
mutate model-role-pod-patch "patch on pods" scaling-manager-model-namespace.yaml \
    '(select(.kind == "Role") | .rules[] | select(.resources | contains(["pods"])) | .verbs) += ["patch"]'
mutate manager-role-no-pod-read "pod verbs" scaling-manager-namespaced-openshift.yaml \
    '(select(.kind == "Role" and .metadata.name == "scaling-manager-manager-role") | .rules) |= map(select(.resources | contains(["pods"]) | not))'
mutate kueue-pod-write "patch on pods" scaling-manager-kueue.yaml \
    '(select(.kind == "Role") | .rules) += [{"apiGroups": [""], "resources": ["pods"], "verbs": ["*"]}]'
mutate unfilled-placeholder "placeholder sed does not fill" scaling-manager-keda-auth.yaml \
    '(select(.kind == "ConfigMap") | .data.secretNamespace) = "__KEDA_NS__"'
mutate undecodable-object "kubectl read" scaling-manager-policy-example.yaml \
    '(select(.kind == "ConfigMap") | .metadata.annotations.replicas) = 3'
mutate invalid-name "not a valid name" scaling-manager-alerts.yaml \
    '(select(.kind == "PrometheusRule") | .metadata.name) = "Alerts_X"'
mutate cta-controller "differs from the installer split" scaling-manager-keda-auth.yaml \
    '(select(.kind == "ClusterTriggerAuthentication") | .metadata.labels["scaling-manager.llmd.ai/part"]) = "controller"'
mutate monitoring-controller "differs from the installer split" scaling-manager-alerts.yaml \
    '(select(.kind == "PrometheusRule") | .metadata.labels["scaling-manager.llmd.ai/part"]) = "controller"'
mutate podmonitor-controller "differs from the installer split" scaling-manager-model-namespace.yaml \
    '(select(.kind == "PodMonitor") | .metadata.labels["scaling-manager.llmd.ai/part"]) = "controller"'
mutate volume-not-optional "is not optional" scaling-manager-namespaced.yaml \
    '(select(.kind == "Deployment") | .spec.template.spec.volumes[] | select(.name == "scaling-manager-config") | .configMap.optional) = false'
mutate prometheus-ca-unmounted "scaling-manager-prometheus-ca is not mounted" scaling-manager-namespaced.yaml \
    'del(select(.kind == "Deployment") | .spec.template.spec.volumes[] | select(.name == "scaling-manager-prometheus-ca"))'
mutate servername-unfixed "serverName" scaling-manager-namespaced-openshift.yaml \
    '(select(.kind == "ServiceMonitor") | .spec.endpoints[0].tlsConfig.serverName) = "scaling-manager-controller-manager-metrics-service.PLACEHOLDER-NAMESPACE.svc"'
mutate kueue-unnarrowed "Kueue Namespace read" scaling-manager-kueue.yaml \
    '(select(.kind == "ClusterRole" and .metadata.name == "scaling-manager-kueue-reader-role-__NAMESPACE__") | .rules[1].resourceNames) = []'
mutate policy-reader-wider "policy-namespace reader reads" scaling-manager-namespaced.yaml \
    '(select(.kind == "ClusterRole" and .metadata.name == "scaling-manager-policy-namespace-reader-role-__NAMESPACE__") | .rules[0].resourceNames) += ["kube-system"]'
mutate model-role-wrong-ns "outside __MODEL_NAMESPACE__" scaling-manager-model-namespace.yaml \
    '(select(.kind == "Role") | .metadata.namespace) = "__NAMESPACE__"'
mutate model-roleref-stale "names no Role or ClusterRole" scaling-manager-model-namespace.yaml \
    '(select(.kind == "RoleBinding") | .roleRef.name) = "scaling-manager-manager-role"'
mutate activate-subject-wrong "ServiceAccount subject" scaling-manager-activate.yaml \
    '(select(.kind == "RoleBinding") | .subjects[0].namespace) = "__MODEL_NAMESPACE__"'
mutate cluster-suffixed "cluster-scoped names differ" scaling-manager-cluster.yaml \
    '(select(.kind == "ClusterRole" or .kind == "ClusterRoleBinding") | .metadata.name) |= . + "-scaling-manager-system" | (select(.kind == "ClusterRoleBinding") | .roleRef.name) |= . + "-scaling-manager-system"'
mutate cta-secret-mismatch "disagree" scaling-manager-keda-auth.yaml \
    '(select(.kind == "ClusterTriggerAuthentication") | .spec.secretTargetRef[0].name) = "scaling-manager-external-scaler"'
mutate external-subject "ServiceAccount subject" scaling-manager-namespaced.yaml \
    '(select(.kind == "ClusterRoleBinding" and .metadata.name == "scaling-manager-node-reader-rolebinding-__NAMESPACE__") | .subjects[0].namespace) = "scaling-manager-monitoring"'
mutate fixed-metrics-subject "binds" scaling-manager-metrics-reader.yaml \
    '(select(.kind == "ClusterRoleBinding") | .subjects[0]) = {"kind": "ServiceAccount", "name": "kube-prometheus-stack-prometheus", "namespace": "scaling-manager-monitoring"}'
mutate scaler-tls-flag-dropped "provision the scaler TLS itself" scaling-manager-namespaced.yaml \
    '(select(.kind == "Deployment") | .spec.template.spec.containers[0].args) |= map(select(. != "--external-scaler-tls-bootstrap=true"))'
mutate scaler-tls-with-files "provision the scaler TLS itself" scaling-manager-cluster.yaml \
    '(select(.kind == "Deployment") | .spec.template.spec.containers[0].args) += ["--external-scaler-cert-path=/etc/scaling-manager/scaler-tls"]'
mutate scaler-tls-role-wider "reads Secrets with no resourceNames" scaling-manager-namespaced-openshift.yaml \
    '(select(.kind == "Role" and .metadata.name == "scaling-manager-scaler-tls-role") | .rules[0]) |= del(.resourceNames)'
mutate scaler-tls-role-other-name "scaler TLS Role grants" scaling-manager-cluster-openshift.yaml \
    '(select(.kind == "Role" and .metadata.name == "scaling-manager-scaler-tls-role") | .rules[0].resourceNames[0]) = "epp-metrics-token"'
mutate scaler-tls-unbound "bound to" scaling-manager-namespaced.yaml \
    'select((.kind == "RoleBinding" and .metadata.name == "scaling-manager-scaler-tls-rolebinding") | not)'
mutate secrets-create-back "grants create on Secrets" scaling-manager-namespaced.yaml \
    '(select(.kind == "Role" and .metadata.name == "scaling-manager-scaler-tls-role") | .rules) += [{"apiGroups": [""], "resources": ["secrets"], "verbs": ["create"]}]'
mutate secrets-create-wildcard "grants create on Secrets" scaling-manager-model-namespace.yaml \
    '(select(.kind == "Role") | .rules) += [{"apiGroups": [""], "resources": ["*"], "verbs": ["*"], "resourceNames": ["x"]}]'
mutate placeholder-in-main "carries a scaler TLS placeholder" scaling-manager-cluster.yaml \
    '(select(.kind == "Secret") | .metadata.name) = "scaling-manager-external-scaler-ca"'
mutate placeholder-opaque "the placeholders are not" scaling-manager-scaler-tls.yaml \
    '(select(.metadata.name == "scaling-manager-external-scaler-ca") | .type) = "Opaque"'
mutate placeholder-with-data "the placeholders are not" scaling-manager-scaler-tls.yaml \
    '(select(.metadata.name == "scaling-manager-external-scaler-tls") | .data["tls.crt"]) = "Zm9v"'
mutate placeholder-wrong-role "the placeholders are not" scaling-manager-scaler-tls.yaml \
    '(select(.metadata.name == "scaling-manager-external-scaler-client-tls") | .metadata.labels["scaling-manager.llmd.ai/scaler-tls"]) = "ca"'
mutate secrets-everywhere "reads Secrets with no resourceNames" scaling-manager-kueue.yaml \
    '(select(.kind == "ClusterRole") | .rules) += [{"apiGroups": [""], "resources": ["secrets"], "verbs": ["list"]}]'
mutate preflight-image-tag "not pinned" scaling-manager-preflight.yaml \
    '(select(.kind == "Job") | .spec.template.spec.containers[0].image) = "ghcr.io/llm-d-incubation/llm-d-scaling-manager:v0.11.0"'
mutate preflight-as-controller "preflight Job runs" scaling-manager-preflight.yaml \
    '(select(.kind == "Job") | .spec.template.spec.serviceAccountName) = "scaling-manager-controller-manager"'
mutate preflight-writes "not read-only" scaling-manager-preflight.yaml \
    '(select(.kind == "ClusterRole") | .rules) += [{"apiGroups": ["apps"], "resources": ["deployments"], "verbs": ["patch"]}]'
mutate preflight-role-writes "writes more than its result" scaling-manager-preflight.yaml \
    '(select(.kind == "Role") | .rules[] | select(.verbs[0] == "update") | .resourceNames) = []'
mutate preflight-reads-secrets "reads Secrets" scaling-manager-preflight.yaml \
    '(select(.kind == "Role") | .rules) += [{"apiGroups": [""], "resources": ["secrets"], "resourceNames": ["scaling-manager-external-scaler-tls"], "verbs": ["get"]}]'
mutate preflight-ns-lists-pods "reads in every namespace" scaling-manager-preflight.yaml \
    '(select(.kind == "ClusterRole") | .rules) += [{"apiGroups": [""], "resources": ["pods"], "verbs": ["list"]}]'
mutate preflight-ns-lists-deployments "reads in every namespace" scaling-manager-preflight.yaml \
    '(select(.kind == "ClusterRole") | .rules[] | select(.resources[0] == "deployments")) |= del(.resourceNames)'
mutate preflight-no-ttl "ttlSecondsAfterFinished" scaling-manager-preflight-cluster.yaml \
    'del(select(.kind == "Job") | .spec.ttlSecondsAfterFinished)'
mutate preflight-cluster-scope-lost "preflight Job runs" scaling-manager-preflight-cluster.yaml \
    '(select(.kind == "Job") | .spec.template.spec.containers[0].args) = ["preflight", "check"]'
mutate preflight-cluster-controller "differs from the installer split" scaling-manager-preflight.yaml \
    '(select(.kind == "ClusterRole") | .metadata.labels["scaling-manager.llmd.ai/part"]) = "controller"'
d="$WORK/m-preview-back"; rm -rf "$d"; cp -r "$GOOD" "$d"
sed -i '1a # PREVIEW: not yet installable on its own.' "$d/scaling-manager-activate.yaml"
caught preview-back "PREVIEW notice" "$d"

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

# The attach step, executed with a stub gh: a missing release becomes a draft
# and gets the files; a draft gets them; a published
# release with the same SHA256SUMS gets nothing; one with different files, or
# a release that cannot be read, fails the job.
cat > "$WORK/wf/bin/gh" <<'STUB'
#!/usr/bin/env bash
# STUB_STATE: absent, draft, published-same, published-other, forbidden.
echo "$*" >> "$STUB_LOG"
case "$1 $2" in
    "release view")
        case "$*" in *isDraft*) ;; *) exit 0 ;; esac
        case "$STUB_STATE" in
            absent) echo "release not found" >&2; exit 1 ;;
            forbidden) echo "HTTP 403: Resource not accessible by integration" >&2; exit 1 ;;
            draft) echo true ;;
            *) echo false ;;
        esac ;;
    "release download")
        while [ $# -gt 0 ]; do case "$1" in --dir) dir="$2"; shift 2 ;; *) shift ;; esac; done
        if [ "$STUB_STATE" = published-same ]; then cp "$STUB_SUMS" "$dir/SHA256SUMS"; else echo "other  x.yaml" > "$dir/SHA256SUMS"; fi ;;
esac
exit 0
STUB
chmod +x "$WORK/wf/bin/gh"
yq -r '.jobs."release-files".steps[] | select(.name == "Attach the files to the GitHub Release") | .run' "$WF" > "$WORK/wf/attach.sh"
rm -rf "$WORK/wf/run"; mkdir -p "$WORK/wf/run"; cp -r "$GOOD" "$WORK/wf/run/dist"
attach() {
    : > "$WORK/wf/gh.log"
    (cd "$WORK/wf/run" && STUB_STATE="$1" STUB_LOG="$WORK/wf/gh.log" STUB_SUMS="$GOOD/SHA256SUMS" VERSION=v0.11.0 PRERELEASE=false \
        PATH="$WORK/wf/bin:$PATH" bash -e "$WORK/wf/attach.sh") > "$WORK/wf/attach.out" 2>&1
}
for c in "absent ok create" "draft ok upload" "published-same ok none" "published-other refused none" "forbidden refused none"; do
    read -r state want acts <<< "$c"
    rc=0; attach "$state" || rc=$?
    uploads="$(grep -c '^release upload' "$WORK/wf/gh.log")"
    creates="$(grep -c '^release create .*--draft' "$WORK/wf/gh.log")"
    got="ok"; [ "$rc" -eq 0 ] || got="refused"
    case "$acts" in
        create) [ "$creates" = 1 ] && [ "$uploads" = 1 ] || got="$got(create=$creates,upload=$uploads)" ;;
        upload) [ "$creates" = 0 ] && [ "$uploads" = 1 ] || got="$got(create=$creates,upload=$uploads)" ;;
        none) [ "$uploads" = 0 ] || got="$got(upload=$uploads)" ;;
    esac
    if [ "$got" = "$want" ]; then pass; else fail "the attach step with the release $state: $got, want $want ($(tail -1 "$WORK/wf/attach.out"))"; fi
done

echo ""
echo "release manifests: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
