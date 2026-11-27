#!/usr/bin/env bash
#
# scaling-manager-admin.sh: the cluster admin's side of the install from the
# one release manifest (docs/guides/kubectl-install). It runs kubectl only,
# on the release files as downloaded, in --dir (default: this directory). It
# never edits a file. --print shows the commands instead of running them:
# keep them in git and let Argo CD or Flux apply the same files.
#
#   scaling-manager-admin.sh [--print] [--dir DIR] COMMAND [ARGS]
#
# Commands:
#   install --prometheus-url URL [--prometheus-ca FILE] [--keda-namespace NS]
#           [--watch-namespaces NS,NS]
#       Apply scaling-manager.yaml and scaling-manager-keda.yaml, label KEDA's
#       namespace, write the settings. Passive: nothing is scaled until
#       `set passive=false`.
#   upgrade [--keda-namespace NS]
#       Apply the new release's files. The settings and ScaledObjects stay.
#   move [--keda-namespace NS]
#       Take a cluster-wide smt install over, once (--force-conflicts), and
#       remove its cluster-wide DisaggregatedSet role writer grant: this
#       install grants it per namespace (scaling-manager-owner.sh grant).
#   namespaces NS,NS | --all
#       Set watchNamespaces, live: the namespaces the controller manages.
#       --all empties it: every namespace, as on every cluster-wide install.
#       An install without --watch-namespaces manages every namespace too.
#   set KEY=VALUE ...
#       Set settings keys (passive, utilizationShare, limiter, quotas, ...).
#   grant-epp NAMESPACE SERVICEACCOUNT
#       Let an EPP's ServiceAccount review the controller's token, where the
#       router chart did not grant it (create tokenreviews and
#       subjectaccessreviews).
#   status
#       The controller's status: ready, the managed namespaces, each
#       namespace's readiness, the checks.
#   uninstall [--keda-namespace NS]
#       Remove everything this install made, in every namespace, and what a
#       moved smt install left (labelled with the same instance). The
#       ScaledObjects an owner registered from their own file stay: each
#       owner deletes theirs first.
#
# Exit codes: 0 done, 1 a kubectl command failed, 2 usage.

set -uo pipefail

NS=scaling-manager-system
SETTINGS=scaling-manager-settings
STATUS=scaling-manager-controller-status
INSTANCE="app.kubernetes.io/instance=$NS"
KEDA_LABEL=scaling-manager.llmd.ai/keda
MANAGER=scaling-manager-admin
PRINT=false
DIR=.

usage() {
    printf 'scaling-manager-admin.sh: %s\n' "$1" >&2
    printf 'usage: scaling-manager-admin.sh [--print] [--dir DIR] install|upgrade|move|namespaces|set|grant-epp|status|uninstall [ARGS]\n' >&2
    exit 2
}

# word <arg>: the argument as a shell word, quoted only when it must be.
word() {
    if [[ "$1" =~ ^[A-Za-z0-9_./:=,@%+-]+$ ]]; then
        printf '%s' "$1"
    else
        printf "'%s'" "${1//\'/\'\\\'\'}"
    fi
}

# kubectl, in run mode: $KUBECTL when set (a test stub), else kubectl.
kubectl() { command "${KUBECTL:-kubectl}" "$@"; }

# run <word...> [| <word...>]...: one command line of kubectl stages joined by
# "|" arguments. --print prints it; otherwise it runs, and a failure exits 1.
run() {
    local line="" a
    for a in "$@"; do
        if [ "$a" = "|" ]; then line+=" |"; else line+="${line:+ }$(word "$a")"; fi
    done
    if $PRINT; then
        printf '%s\n' "$line"
        return 0
    fi
    eval "$line" || { printf 'scaling-manager-admin.sh: failed: %s\n' "$line" >&2; exit 1; }
}

# try <word...>: run, but a failure is said and the script goes on (an
# uninstall step whose object may be gone already).
try() {
    local line="" a
    for a in "$@"; do line+="${line:+ }$(word "$a")"; done
    if $PRINT; then
        printf '%s\n' "$line"
        return 0
    fi
    eval "$line" || printf 'scaling-manager-admin.sh: failed, going on: %s\n' "$line" >&2
}

# note <text>: a comment, printed with --print.
note() { printf '# %s\n' "$1"; }

# need <file>...: each release file is in --dir, or exit 2 before any
# command runs. --print checks nothing: it may run where the files are not.
need() {
    local f
    $PRINT && return 0
    for f in "$@"; do
        [ -f "$DIR/$f" ] || usage "no $f in $DIR: download it with the release (see docs/guides/kubectl-install)"
    done
}

valid_ns() { [[ "$1" =~ ^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$ ]] || usage "'$1' is not a namespace name"; }

keda_side() {
    run kubectl apply --server-side -n "$KEDA_NS" -f "$DIR/scaling-manager-keda.yaml"
    run kubectl label namespace "$KEDA_NS" "$KEDA_LABEL=true" --overwrite
}

# settings_apply <key=value>...: the keys, server-side applied by the
# admin's field manager; keys another manager set stay.
settings_apply() {
    local args=(kubectl create configmap "$SETTINGS" -n "$NS") kv
    for kv in "$@"; do args+=("--from-literal=$kv"); done
    run "${args[@]}" --dry-run=client -o yaml "|" kubectl apply --server-side --field-manager="$MANAGER" -f -
}

# settings_patch <key=value>...: a merge patch of the settings' data.
settings_patch() {
    local json="" kv k v
    for kv in "$@"; do
        k="${kv%%=*}" v="${kv#*=}"
        [[ "$kv" == *=* && "$k" =~ ^[A-Za-z][A-Za-z0-9_]*$ ]] || usage "'$kv' is not KEY=VALUE"
        [[ "$v" != *[\"\\]* ]] || usage "'$kv': a value may not hold a quote or a backslash"
        json+="${json:+,}\"$k\":\"$v\""
    done
    run kubectl patch configmap "$SETTINGS" -n "$NS" --type merge -p "{\"data\":{$json}}"
}

while [ $# -gt 0 ]; do
    case "$1" in
        --print) PRINT=true; shift ;;
        --dir) [ $# -ge 2 ] || usage "--dir needs a directory"; DIR="$2"; shift 2 ;;
        -h|--help) printf 'usage: scaling-manager-admin.sh [--print] [--dir DIR] install|upgrade|move|namespaces|set|grant-epp|status|uninstall [ARGS]\nThe commands and their arguments: the comment at the top of this file, and docs/guides/kubectl-install.\n'; exit 0 ;;
        -*) usage "unknown option $1" ;;
        *) break ;;
    esac
done
[ $# -gt 0 ] || usage "no command"
CMD="$1"; shift

KEDA_NS=keda PROM_URL="" PROM_CA="" WATCH="" WATCH_SET=false
case "$CMD" in install|upgrade|move|uninstall)
    while [ $# -gt 0 ]; do
        case "$1" in
            --keda-namespace) [ $# -ge 2 ] || usage "$1 needs a value"; KEDA_NS="$2"; shift 2 ;;
            --prometheus-url) [ "$CMD" = install ] && [ $# -ge 2 ] || usage "$1 is for install, with a value"; PROM_URL="$2"; shift 2 ;;
            --prometheus-ca) [ "$CMD" = install ] && [ $# -ge 2 ] || usage "$1 is for install, with a file"; PROM_CA="$2"; shift 2 ;;
            --watch-namespaces) [ "$CMD" = install ] && [ $# -ge 2 ] || usage "$1 is for install, with a list"; WATCH="$2"; WATCH_SET=true; shift 2 ;;
            *) usage "$CMD: unknown argument $1" ;;
        esac
    done
    valid_ns "$KEDA_NS" ;;
esac

case "$CMD" in
    install)
        [[ "$PROM_URL" =~ ^https?://[^/]+ ]] || usage "install needs --prometheus-url http(s)://host:port"
        need scaling-manager.yaml scaling-manager-keda.yaml
        if $WATCH_SET; then
            IFS=, read -ra list <<< "$WATCH"
            [ "${#list[@]}" -gt 0 ] || usage "--watch-namespaces: an empty list; leave it out to manage every namespace"
            for n in "${list[@]}"; do valid_ns "$n"; done
        fi
        $PRINT && note "The install: the manifest, KEDA's side in $KEDA_NS, the settings. Passive until: scaling-manager-admin.sh set passive=false"
        run kubectl apply --server-side -f "$DIR/scaling-manager.yaml"
        keda_side
        kv=("prometheusURL=$PROM_URL" "kedaNamespace=$KEDA_NS")
        if [ -n "$PROM_CA" ]; then
            $PRINT || [ -f "$PROM_CA" ] || usage "--prometheus-ca $PROM_CA: no such file"
            run kubectl create secret generic scaling-manager-prometheus-ca -n "$NS" --from-file=ca.crt="$PROM_CA" \
                --dry-run=client -o yaml "|" kubectl apply --server-side --field-manager="$MANAGER" -f -
            kv+=("caFromSecret=true")
        fi
        $WATCH_SET && kv+=("watchNamespaces=$WATCH")
        settings_apply "${kv[@]}"
        ;;
    upgrade)
        need scaling-manager.yaml scaling-manager-keda.yaml
        run kubectl apply --server-side -f "$DIR/scaling-manager.yaml"
        keda_side
        ;;
    move)
        need scaling-manager.yaml scaling-manager-keda.yaml
        $PRINT && note "Over an smt install made with --scope cluster: --force-conflicts takes the controller Deployment over from smt, this once."
        run kubectl apply --server-side --force-conflicts -f "$DIR/scaling-manager.yaml"
        keda_side
        settings_patch "kedaNamespace=$KEDA_NS"
        # The earlier install's cluster-wide role writer grant: bare names from
        # scaling-manager-dset-role-writer-cluster.yaml (smt --dset-role-writer),
        # suffixed with the first 8 hex digits of sha256("scaling-manager-system")
        # from make deploy (llmsm_ns_suffix). This install's writer acts only
        # where the owner bound it per namespace.
        $PRINT && note "The earlier install's cluster-wide DisaggregatedSet role writer grant: this install grants it per namespace."
        run kubectl delete clusterrolebinding scaling-manager-dset-role-writer-rolebinding \
            scaling-manager-dset-role-writer-rolebinding-5414d5c5 --ignore-not-found
        run kubectl delete clusterrole scaling-manager-dset-role-writer-role \
            scaling-manager-dset-role-writer-role-5414d5c5 --ignore-not-found
        ;;
    namespaces)
        [ $# -eq 1 ] || usage "namespaces needs one list, NS,NS, or --all"
        if [ "$1" = --all ]; then
            settings_patch "watchNamespaces="
        else
            IFS=, read -ra list <<< "$1"
            [ "${#list[@]}" -gt 0 ] || usage "namespaces: an empty list; --all manages every namespace"
            for n in "${list[@]}"; do valid_ns "$n"; done
            settings_patch "watchNamespaces=$1"
        fi
        ;;
    set)
        [ $# -gt 0 ] || usage "set needs KEY=VALUE"
        settings_patch "$@"
        ;;
    grant-epp)
        [ $# -eq 2 ] || usage "grant-epp needs NAMESPACE SERVICEACCOUNT"
        valid_ns "$1"
        [[ "$2" =~ ^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$ ]] || usage "'$2' is not a ServiceAccount name"
        run kubectl create clusterrolebinding "scaling-manager-epp-tokenreview.$1.$2" \
            --clusterrole="scaling-manager-epp-tokenreview-$NS" --serviceaccount="$1:$2" --dry-run=client -o yaml \
            "|" kubectl label --local -f - -o yaml "$INSTANCE" app.kubernetes.io/part-of=scaling-manager scaling-manager.llmd.ai/grant=epp-tokenreview \
            "|" kubectl apply --server-side --field-manager="$MANAGER" -f -
        ;;
    status)
        [ $# -eq 0 ] || usage "status takes no argument"
        run kubectl get configmap "$STATUS" -n "$NS" -o 'go-template={{range $k, $v := .data}}{{$k}}: {{$v}}{{"\n"}}{{end}}'
        ;;
    uninstall)
        need scaling-manager.yaml scaling-manager-keda.yaml
        $PRINT && note "Sharing to shadow first, and wait until the controller runs it, so no transfer mark stays on a pod."
        try kubectl patch configmap "$SETTINGS" -n "$NS" --type merge -p '{"data":{"utilizationShare":"shadow"}}'
        try kubectl wait --for=jsonpath='{.data.utilizationShare}'=shadow configmap/"$STATUS" -n "$NS" --timeout=60s
        run kubectl delete scaledobject -A -l "$INSTANCE,app.kubernetes.io/managed-by=llm-d-scaling-manager" --ignore-not-found
        run kubectl delete rolebinding,role,podmonitor,networkpolicy -A -l "$INSTANCE" --ignore-not-found
        run kubectl delete clusterrolebinding -l "$INSTANCE,scaling-manager.llmd.ai/grant=epp-tokenreview" --ignore-not-found
        run kubectl delete -n "$KEDA_NS" -f "$DIR/scaling-manager-keda.yaml" --ignore-not-found
        try kubectl label namespace "$KEDA_NS" "$KEDA_LABEL-"
        run kubectl delete -f "$DIR/scaling-manager.yaml" --ignore-not-found --wait=true --timeout=300s
        $PRINT && note "What a moved smt install left beside the manifest, labelled with the same instance."
        run kubectl delete servicemonitor,serviceaccount,secret,configmap -A -l "$INSTANCE" --ignore-not-found
        run kubectl delete clusterrolebinding,clusterrole,clustertriggerauthentication -l "$INSTANCE" --ignore-not-found
        ;;
    *) usage "unknown command $CMD" ;;
esac
