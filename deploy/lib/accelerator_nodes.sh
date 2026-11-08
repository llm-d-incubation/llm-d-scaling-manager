#!/usr/bin/env bash
# Which nodes are the accelerator nodes -- for the per-node DaemonSets
# (deploy/prepull.sh, and whatever follows it) that have to land on every
# node a model server could be scheduled to.
#
# "Has a GPU" is not one label. NVIDIA GPU Feature Discovery writes
# nvidia.com/gpu.product (and nvidia.com/gpu.present); CoreWeave, GKE, EKS
# Auto Mode, Karpenter and the AMD operator each write their own key, and on
# those clusters a DaemonSet selecting on the GFD key runs on NO node while
# the model servers, which select on nothing, schedule to every GPU node.
# So the default is not a nodeSelector but a node affinity with one term per
# known product-label key -- terms are OR-ed, which a nodeSelector cannot
# express -- and the node listing filters on the same keys. Each term is
# "the key exists AND is not empty": CoreWeave writes gpu.nvidia.com/model=""
# on its CPU nodes, and Exists alone put a holder on four of them. An
# explicit selector replaces both: one or more KEY=VALUE pairs separated by
# commas, every one of which a node must carry (a nodeSelector map, and the
# equality-only form of a `kubectl get nodes -l` selector). It does not add
# to the default: no product label among the pairs means no GPU condition
# at all. More than one pair is how a cluster where no single label picks
# out the usable GPU nodes -- the one label that would exclude a broken
# node is also on the CPU nodes -- selects them: the product label AND that
# one. An empty VALUE is a valid label value and selects the nodes whose
# label is empty: gpu.nvidia.com/model= is CoreWeave's CPU nodes, not its
# GPU nodes.
#
# What places the model servers is their accelerator RESOURCE request, not
# a label; these keys are the labels that say a node has one. On a cluster
# mixing vendors the default lands a CUDA image on an AMD, Intel or Gaudi
# node too -- disk spent where the engine can never run -- which is why
# `status` names the key each node matched and warns on those, and why
# --node-selector is the answer there.
#
# The key list is the one deploy/lib/accelerator_labels.py and the
# controller resolve through; hack/check-accelerator-labels.sh fails when
# any copy drifts from the others.

# Keys whose presence on a node says an accelerator is there. Order is the
# canonical GFD key first, as in the other copies.
ACCELERATOR_PRODUCT_KEYS=(
    "nvidia.com/gpu.product"
    "gpu.nvidia.com/model"
    "gpu.nvidia.com/class"
    "cloud.google.com/gke-accelerator"
    "eks.amazonaws.com/instance-gpu-name"
    "karpenter.k8s.aws/instance-gpu-name"
    "karpenter.azure.com/sku-gpu-name"
    "amd.com/gpu.product-name"
    "beta.amd.com/gpu.product-name"
    "habana.ai/product.name"
    "gpu.intel.com/product"
)

# accelerator_selector_pairs prints the KEY=VALUE pairs of selector $1, one
# per line, in the order given. It splits on every comma and keeps empty
# elements, so accelerator_selector_ok sees "a=b,,c=d" and a trailing comma
# for what they are; callers that render run accelerator_selector_ok first.
accelerator_selector_pairs() {
    [ -n "$1" ] || return 0
    local rest="$1," pair
    while [ -n "$rest" ]; do
        pair="${rest%%,*}"
        rest="${rest#*,}"
        printf '%s\n' "$pair"
    done
}

# Kubernetes label syntax. A key is an optional prefix and a name: the
# prefix a DNS subdomain (at most 253 characters of lowercase letters,
# digits, '-' and '.', a letter or digit at each end) followed by '/'; the
# name at most 63 characters of letters, digits, '-', '_' and '.', a letter
# or digit at each end. A value is empty, or follows the name rule.
ACCELERATOR_LABEL_NAME_RE='^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$'
# The prefix is DNS-1123: dot-separated labels, each a letter or digit at
# both ends, so a..b, a.-b and a-.b are not prefixes.
ACCELERATOR_LABEL_PREFIX_RE='^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$'

# accelerator_label_key_ok says whether $1 is a label key; prints why not.
accelerator_label_key_ok() {
    local key="$1" prefix="" name="$1"
    case "$key" in
        */*/*) echo "not a label key: '${key}' (at most one '/', between a DNS-subdomain prefix and a name)"; return 1 ;;
        */*) prefix="${key%%/*}"; name="${key#*/}" ;;
    esac
    case "$key" in
        */*)
            if [ -z "$prefix" ] || [ "${#prefix}" -gt 253 ] || ! [[ "$prefix" =~ $ACCELERATOR_LABEL_PREFIX_RE ]]; then
                echo "not a label key: '${key}' (the prefix before '/' is a DNS subdomain: at most 253 lowercase letters, digits, '-' and '.', a letter or digit at each end of every dot-separated part)"
                return 1
            fi ;;
    esac
    if [ -z "$name" ] || [ "${#name}" -gt 63 ] || ! [[ "$name" =~ $ACCELERATOR_LABEL_NAME_RE ]]; then
        echo "not a label key: '${key}' (the name is at most 63 letters, digits, '-', '_' and '.', a letter or digit at each end)"
        return 1
    fi
}

# accelerator_label_value_ok says whether $1 is a label value; prints why
# not. Empty is one: Kubernetes allows it, and it selects the nodes whose
# label is empty -- gpu.nvidia.com/model= is CoreWeave's CPU nodes.
accelerator_label_value_ok() {
    local value="$1"
    [ -n "$value" ] || return 0
    if [ "${#value}" -gt 63 ] || ! [[ "$value" =~ $ACCELERATOR_LABEL_NAME_RE ]]; then
        echo "not a label value: '${value}' (empty, or at most 63 letters, digits, '-', '_' and '.', a letter or digit at each end; no spaces, no quotes)"
        return 1
    fi
}

# accelerator_selector_ok says whether $1 is one or more KEY=VALUE pairs of
# Kubernetes label syntax (above), separated by single commas: one '=' per
# pair, no key twice (a nodeSelector is a map). A VALUE may be empty, which
# selects nodes whose label is empty (not nodes that lack it). Empty
# altogether is fine (the default). Prints why not, returns 1, when it is
# not. A newline, carriage return or tab is refused before anything is
# split: the pairs are read a line at a time, so a newline would otherwise
# pass as a separator the nodeSelector and `kubectl get nodes -l` do not
# share.
accelerator_selector_ok() {
    [ -n "$1" ] || return 0
    case "$1" in
        *$'\n'*|*$'\r'*|*$'\t'*) echo "a newline, carriage return or tab in the selector (pairs are separated by one comma, no whitespace)"; return 1 ;;
    esac
    local pair key value seen="," why
    while IFS= read -r pair; do
        if [ -z "$pair" ]; then
            echo "an empty KEY=VALUE in '$1' (pairs are separated by one comma, none leading or trailing)"
            return 1
        fi
        case "$pair" in
            *=*) ;;
            *) echo "must be KEY=VALUE[,KEY=VALUE...] (a VALUE may be empty, which selects the nodes whose label is empty, not every node carrying the key), got '$pair'"; return 1 ;;
        esac
        key="${pair%%=*}"
        value="${pair#*=}"
        why="$(accelerator_label_key_ok "$key")" || { echo "$why"; return 1; }
        why="$(accelerator_label_value_ok "$value")" || { echo "$why"; return 1; }
        case "$seen" in
            *",${key},"*) echo "label key '${key}' given twice in '$1' (every pair must match, so one key takes one value)"; return 1 ;;
        esac
        seen="${seen}${key},"
    done < <(accelerator_selector_pairs "$1")
}

# accelerator_selector_from_json reads a nodeSelector map (JSON, as a
# DaemonSet's pod template carries it) on stdin and prints it as a selector,
# KEY=VALUE pairs joined by commas; empty for no map. A key or value holding
# a comma, '=' or a line break would not read back as the map it came from,
# so such a map prints as a fixed string accelerator_selector_ok refuses as
# one piece: it names no key, since a key holding a comma would split it.
accelerator_selector_from_json() {
    jq -r 'to_entries
        | if any(.[]; (.key + .value) | test("[,=\n\r\t]")) then "unreadable nodeSelector: a key or value holds a comma or an equals sign or a line break"
          else map(.key + "=" + .value) | join(",") end' 2>/dev/null || true
}

# accelerator_check_selector refuses a --node-selector that is not one. The
# string is substituted into a YAML document, where a quote or a newline
# would open a different field, and passed to `kubectl get nodes -l`, where
# anything beyond KEY=VALUE pairs (set-based terms, !=) would select nodes
# the DaemonSet's nodeSelector cannot express.
accelerator_check_selector() {
    local why
    why="$(accelerator_selector_ok "$1")" || log_error "--node-selector: ${why}"
}

# accelerator_check_toleration refuses a taint key that is not one. An empty
# key with operator Exists tolerates EVERY taint, which is not what a caller
# who typed --toleration "" meant.
accelerator_check_toleration() {
    case "$1" in
        ""|*[!A-Za-z0-9._/-]*) log_error "--toleration: not a taint key: '$1'" ;;
    esac
}

# accelerator_placement_yaml prints the pod-spec placement for $1: with
# KEY=VALUE pairs, a nodeSelector holding every pair in the order given;
# empty, the OR-ed affinity over the product keys. Indented for a pod
# template's spec (6 spaces), the way the DaemonSets here lay it out. Keys
# are quoted as values are: a key such as on, yes or true is a boolean to a
# YAML 1.1 reader, and a nodeSelector key must be a string.
accelerator_placement_yaml() {
    local selector="$1" pair
    if [ -n "$selector" ]; then
        echo "      nodeSelector:"
        while IFS= read -r pair; do
            printf '        "%s": "%s"\n' "${pair%%=*}" "${pair#*=}"
        done < <(accelerator_selector_pairs "$selector")
        return
    fi
    cat <<'EOF'
      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
EOF
    local key
    for key in "${ACCELERATOR_PRODUCT_KEYS[@]}"; do
        cat <<EOF
              - matchExpressions:
                  - key: "${key}"
                    operator: Exists
                  - key: "${key}"
                    operator: NotIn
                    values: [""]
EOF
    done
}

# accelerator_keys_json prints the key list as a JSON array, for jq.
accelerator_keys_json() {
    printf '%s\n' "${ACCELERATOR_PRODUCT_KEYS[@]}" | jq -R . | jq -s .
}

# accelerator_nodes_json prints the node list (a v1 NodeList JSON document)
# the placement for $1 would match: `kubectl get nodes -l KEY=VALUE[,...]`
# (a comma in a label selector is AND, as in the nodeSelector), or every
# node carrying any product key. Fails as kubectl fails (a Forbidden
# for a namespace tenant) -- the caller decides what that means.
accelerator_nodes_json() {
    local selector="$1"
    if [ -n "$selector" ]; then
        kubectl get nodes -l "$selector" -o json
        return
    fi
    kubectl get nodes -o json | jq --argjson keys "$(accelerator_keys_json)" \
        '.items |= map(select((.metadata.labels // {}) as $l | any($keys[]; $l[.] != null and $l[.] != "")))'
}

# accelerator_vendor_warning prints a warning when any of the nodes (a v1
# NodeList JSON document on stdin) matched only through a key that is not
# NVIDIA: a CUDA image is held there for nothing.
accelerator_vendor_warning() {
    local n
    n="$(jq --argjson keys "$(accelerator_keys_json)" \
        '[.items[] | (.metadata.labels // {}) as $l
          | first($keys[] | select($l[.] != null and $l[.] != "")) // ""
          | select(startswith("amd.com/") or startswith("beta.amd.com/") or startswith("habana.ai/") or startswith("gpu.intel.com/"))] | length')"
    [ "${n:-0}" -gt 0 ] || return 0
    log_warning "${n} node(s) carry an AMD, Intel or Gaudi accelerator label and are held too; if the image is CUDA-only that is disk for nothing -- --node-selector KEY=VALUE[,KEY=VALUE...] picks the nodes the model servers actually run on"
}

# accelerator_selector_text says, for a log line, what $1 selects: the
# selector itself for one pair (as it always read), every pair joined by
# " and " for several, so a reader does not take the comma for "or".
accelerator_selector_text() {
    if [ -z "$1" ]; then
        printf 'any node carrying a known GPU product label'
    else
        local pair text=""
        while IFS= read -r pair; do
            text="${text:+${text} and }${pair}"
        done < <(accelerator_selector_pairs "$1")
        printf '%s' "$text"
    fi
}

# USABLE NODES. A node a model server cannot be placed on is not one to wait
# for or to count. A DaemonSet does not see that: its controller tolerates
# node.kubernetes.io/unschedulable and the node-condition taints
# (disk-pressure, not-ready, ...) by itself, so its desired count includes a
# cordoned node under DiskPressure, the pod there is evicted or never starts,
# and numberReady never reaches desired -- a standup waiting on it ran out
# its whole 3600 s, and the status commands reported a finished preparation
# as 8/17 and failed. A node is excluded when it is cordoned, or when it
# carries a NoSchedule or NoExecute taint whose key the DaemonSet's own pod
# template does not tolerate. Tolerations the controller adds by itself do
# not count: they are exactly the ones an engine does not have.

# accelerator_ds_toleration_keys NS DAEMONSET FALLBACK... prints, as a JSON
# array, the taint keys the DaemonSet's pod template tolerates (what an apply
# with --toleration wrote), or the FALLBACK keys when it cannot be read. A
# toleration with no key tolerates every taint, and reads as "*".
accelerator_ds_toleration_keys() {
    local ns="$1" ds="$2" got
    shift 2
    got="$(kubectl get daemonset -n "$ns" "$ds" -o json 2>/dev/null \
        | jq -c '[.spec.template.spec.tolerations[]? | (.key // "*")] | select(length > 0)' 2>/dev/null || true)"
    if [ -n "$got" ]; then
        printf '%s' "$got"
        return
    fi
    printf '%s\n' "$@" | jq -R . | jq -sc 'map(select(. != ""))'
}

# accelerator_excluded_json KEYS reads a v1 NodeList on stdin and prints a
# JSON array of {name, reason} for the nodes the rule above excludes, KEYS
# being the tolerated taint keys as a JSON array.
accelerator_excluded_json() {
    jq -c --argjson tol "$1" '[.items[]
        | {name: .metadata.name,
           reason: (if .spec.unschedulable == true then "cordoned"
                    elif any($tol[]; . == "*") then ""
                    else ([.spec.taints[]?
                           | select(.effect == "NoSchedule" or .effect == "NoExecute")
                           | .key as $k
                           | select(any($tol[]; . == $k) | not)
                           | "tainted " + .key + ":" + .effect] | first // "") end)}
        | select(.reason != "")]'
}

# accelerator_usable_nodes_json EXCLUDED reads a v1 NodeList on stdin and
# prints it without the nodes in EXCLUDED (accelerator_excluded_json output).
accelerator_usable_nodes_json() {
    jq -c --argjson ex "$1" '.items |= map(select(.metadata.name as $n | any($ex[]; .name == $n) | not))'
}

# accelerator_print_excluded EXCLUDED prints a row per excluded node, in the
# status tables' layout, and a line saying why they are not counted.
accelerator_print_excluded() {
    local n node reason
    n="$(printf '%s' "$1" | jq 'length')"
    [ "${n:-0}" -gt 0 ] || return 0
    while IFS=$'\x1f' read -r node reason; do
        printf '  %-28s %-12s %s\n' "$node" excluded "$reason"
    done < <(printf '%s' "$1" | jq -r '.[] | [.name, .reason] | join("\u001f")')
    echo "  ${n} node(s) excluded and not counted: an engine cannot start there (cordoned, or tainted against it)"
}
