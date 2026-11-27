#!/usr/bin/env bash
#
# scaling-manager-owner.sh: a namespace owner's side of the install from the
# one release manifest (docs/guides/kubectl-install). It never deploys the
# manager: the cluster admin does that, and lists the namespace in
# watchNamespaces. It runs kubectl only, in the owner's namespace, on the
# release files as downloaded, in --dir (default: this directory). --print
# shows the commands instead of running them, for git.
#
#   scaling-manager-owner.sh [--print] [--dir DIR] COMMAND NAMESPACE [ARGS]
#
# Commands:
#   monitor NAMESPACE
#       The model-server PodMonitor, where nothing scrapes the model servers
#       yet (llm-d's monitoring components scrape the same port).
#   network NAMESPACE
#       The NetworkPolicy rule letting the controller reach the EPP's
#       /metrics, only where a default deny is in place.
#   register NAMESPACE [FILE]
#       Apply ScaledObjects: FILE, or the controller's suggested ones
#       (ConfigMap scaling-manager-scaledobjects-plan), as reviewed.
#   unregister NAMESPACE
#       Delete the ScaledObjects the plan made (managed-by llm-d-scaling-manager).
#   policy NAMESPACE FILE
#       The namespace's own scaling policy (scaling-manager-scaling-policy-config),
#       and the namespace label that has the controller read it.
#   grant pod-writer|dset-role-writer NAMESPACE
#       Bind the controller to the grant: patch on pods (utilization share
#       active), or on DisaggregatedSets (the role writer).
#   status NAMESPACE
#       This namespace's readiness, as the controller records it in Events
#       here: nothing in the controller's namespace is read.
#
# Exit codes: 0 done, 1 a kubectl command failed, 2 usage.

set -uo pipefail

SYS=scaling-manager-system
PLAN=scaling-manager-scaledobjects-plan
PRINT=false
DIR=.

usage() {
    printf 'scaling-manager-owner.sh: %s\n' "$1" >&2
    printf 'usage: scaling-manager-owner.sh [--print] [--dir DIR] monitor|network|register|unregister|policy|grant|status NAMESPACE [ARGS]\n' >&2
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
    eval "$line" || { printf 'scaling-manager-owner.sh: failed: %s\n' "$line" >&2; exit 1; }
}

# note <text>: a comment with --print; otherwise said on stderr, so whoever
# runs the command reads it too.
note() { if $PRINT; then printf '# %s\n' "$1"; else printf 'scaling-manager-owner.sh: %s\n' "$1" >&2; fi; }

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

while [ $# -gt 0 ]; do
    case "$1" in
        --print) PRINT=true; shift ;;
        --dir) [ $# -ge 2 ] || usage "--dir needs a directory"; DIR="$2"; shift 2 ;;
        -h|--help) printf 'usage: scaling-manager-owner.sh [--print] [--dir DIR] monitor|network|register|unregister|policy|grant|status NAMESPACE [ARGS]\nThe commands and their arguments: the comment at the top of this file, and docs/guides/kubectl-install.\n'; exit 0 ;;
        -*) usage "unknown option $1" ;;
        *) break ;;
    esac
done
[ $# -gt 0 ] || usage "no command"
CMD="$1"; shift

case "$CMD" in
    monitor|network|status|unregister)
        [ $# -eq 1 ] || usage "$CMD needs NAMESPACE"
        valid_ns "$1"; N="$1" ;;
    register)
        [ $# -eq 1 ] || [ $# -eq 2 ] || usage "register needs NAMESPACE [FILE]"
        valid_ns "$1"; N="$1" ;;
    policy)
        [ $# -eq 2 ] || usage "policy needs NAMESPACE FILE"
        valid_ns "$1"; N="$1" ;;
    grant)
        [ $# -eq 2 ] || usage "grant needs pod-writer|dset-role-writer NAMESPACE"
        case "$1" in pod-writer|dset-role-writer) ;; *) usage "grant: '$1' is not pod-writer or dset-role-writer" ;; esac
        valid_ns "$2"; N="$2" ;;
    *) usage "unknown command $CMD" ;;
esac

case "$CMD" in monitor|network) need scaling-manager-monitoring.yaml ;; grant) need scaling-manager-grants.yaml ;; esac

case "$CMD" in
    monitor)
        run kubectl apply --server-side -n "$N" -f "$DIR/scaling-manager-monitoring.yaml" -l scaling-manager.llmd.ai/monitor=modelserver
        ;;
    network)
        note "Only where a default-deny ingress policy is in place: this one selects every pod of $N."
        run kubectl apply --server-side -n "$N" -f "$DIR/scaling-manager-monitoring.yaml" -l scaling-manager.llmd.ai/monitor=epp-networkpolicy
        ;;
    register)
        if [ $# -eq 2 ]; then
            $PRINT || [ -f "$2" ] || usage "register: no file $2"
            run kubectl apply -n "$N" -f "$2"
        else
            $PRINT && note "The controller's suggestion for $N, applied as is. Review it first: kubectl get configmap $PLAN -n $SYS -o jsonpath={.data.$N}"
            run kubectl get configmap "$PLAN" -n "$SYS" -o "jsonpath={.data.$N}" "|" kubectl apply -n "$N" -f -
        fi
        ;;
    unregister)
        run kubectl delete scaledobject -n "$N" -l app.kubernetes.io/managed-by=llm-d-scaling-manager --ignore-not-found
        ;;
    policy)
        $PRINT || [ -f "$2" ] || usage "policy: no file $2"
        run kubectl apply -n "$N" -f "$2"
        note "The label needs patch on the Namespace; without it, ask the cluster admin to run this line."
        run kubectl label namespace "$N" scaling-manager.llmd.ai/config-enabled=true --overwrite
        ;;
    grant)
        run kubectl apply --server-side -n "$N" -f "$DIR/scaling-manager-grants.yaml" -l "scaling-manager.llmd.ai/grant=$1"
        ;;
    status)
        run kubectl get events -n "$N" --field-selector source=scaling-manager-namespaces --sort-by=.lastTimestamp
        ;;
esac
