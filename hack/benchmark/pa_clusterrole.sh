#!/usr/bin/env bash
# Satisfy llm-d-benchmark's "is prometheus-adapter installed?" probe without
# taking over a ClusterRole that belongs to somebody else.
#
# WHY THIS EXISTS
# ---------------
# prometheus-adapter and KEDA both register the external.metrics.k8s.io
# APIService, and a cluster has exactly one. We run KEDA, so standup must skip
# the prometheus-adapter install -- but its probe checks for the ClusterRole
# `prometheus-adapter-resource-reader` and refuses to continue without it. The
# cheap answer is to create a stub carrying the Helm ownership metadata the
# probe looks for.
#
# The trap is that the object is CLUSTER-scoped. `kubectl annotate --overwrite`
# on a shared cluster does not create our own copy, it rewrites whoever else's:
# point `meta.helm.sh/release-namespace` at our namespace and the real release's
# next `helm upgrade` fails on invalid ownership metadata. On a ~12-tenant
# cluster that is somebody's outage, caused by a benchmark.
#
# So: create it only when it is genuinely absent, and say plainly which case we
# are in otherwise. The probe passes on any existing object regardless of who
# owns it, so declining costs nothing.
#
# Usage:
#   pa_clusterrole.sh stub <monitoring-namespace>
#
# Exit status is 0 for every outcome except a real failure to create; "declined"
# is a success, not an error.
set -u
# --help prints this file's header comment -- the documentation the script
# already carries, so it cannot drift from what the script does. Placed before
# any argument handling because several of these take a namespace as $1, and
# without it `--help` was consumed as one.
case "${1:-}" in
    -h|--help)
        sed -n '2,/^[^#]/p' "$0" | sed 's/^# \{0,1\}//; $d'
        exit 0
        ;;
esac


CR=prometheus-adapter-resource-reader
ANNOTATION='meta.helm.sh/release-namespace'

cmd_stub() {
    local ours="${1:?usage: pa_clusterrole.sh stub <monitoring-namespace>}"

    echo "Not installing prometheus-adapter: it and KEDA both register the"
    echo "external.metrics.k8s.io APIService, and a cluster has exactly one."

    # Absent is the ONLY case in which we create anything. Checked first and
    # separately from reading the annotation, because "no annotation" and "no
    # object" are different states that need different answers -- folding them
    # together is how a guard ends up rewriting somebody else's object.
    if ! kubectl get clusterrole "$CR" >/dev/null 2>&1; then
        echo "Stubbing $CR ClusterRole so standup's existing-PA probe passes..."
        if ! kubectl create clusterrole "$CR" \
                --verb=get,list,watch --resource=pods,nodes; then
            echo "ERROR: could not create ClusterRole $CR." >&2
            echo "       standup's prometheus-adapter probe will fail without it." >&2
            return 1
        fi
        kubectl annotate --overwrite clusterrole "$CR" \
            meta.helm.sh/release-name=prometheus-adapter \
            "$ANNOTATION=$ours" || return 1
        kubectl label --overwrite clusterrole "$CR" \
            app.kubernetes.io/managed-by=Helm || return 1
        echo "Created and labeled $CR (release-namespace=$ours)."
        return 0
    fi

    local owner
    owner=$(kubectl get clusterrole "$CR" \
        -o "jsonpath={.metadata.annotations.meta\\.helm\\.sh/release-namespace}" \
        2>/dev/null || true)

    if [ -z "$owner" ]; then
        echo "ClusterRole $CR already exists and carries no release-namespace"
        echo "annotation. NOT rewriting its ownership: it is CLUSTER-scoped, something else"
        echo "created it, and --overwrite would take it over on a shared cluster."
        echo "The probe it exists to satisfy passes on the existing object anyway."
    elif [ "$owner" = "$ours" ]; then
        # Ours, from an earlier standup. Worth distinguishing: the branch below
        # would otherwise report "something else created it" about an object we
        # created ourselves, which reads as a conflict on every re-run.
        echo "ClusterRole $CR is already stubbed for $ours by an earlier"
        echo "standup. Leaving it as it is; nothing to do."
    else
        echo "NOT touching ClusterRole $CR: it already belongs to"
        echo "the Helm release prometheus-adapter in $owner — a REAL install, possibly"
        echo "someone else's on a shared cluster. Rewriting its ownership to $ours"
        echo "would make that release's next helm upgrade fail on invalid ownership metadata."
        echo "The probe it exists to satisfy passes on the existing object anyway."
    fi
    return 0
}

case "${1:-}" in
    stub) shift; cmd_stub "$@" ;;
    *) sed -n '/^# Usage:/,/^# Exit status/p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
