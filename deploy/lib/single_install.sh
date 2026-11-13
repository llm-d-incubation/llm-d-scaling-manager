#!/usr/bin/env bash
#
# Refuse to install a second scaling manager on top of an existing one.
#
# Requires vars: LLMSM_NS.
# Requires funcs: log_info/log_success/log_warning/log_error, llmsm_install_scope.
#
# The contract:
#
#   ONE cluster-scoped the scaling manager, and nothing else.
#   OR any number of namespace-scoped scaling managers, at most one per namespace.
#
# Never a mix. A cluster-scoped controller already covers every namespace, so a
# namespace-scoped one beside it is a second controller for the same workloads;
# and a cluster-scoped one installed beside existing namespace-scoped ones
# swallows them the moment it starts.
#
# Note what does NOT partition, and what does. Discovery is call-driven: a workload
# registers with the scaling manager whose scalerAddress its trigger names, and every install
# has its own scaling-manager-external-scaler.<ns> service — so no two controllers ever see the
# same workload, whatever their scope. Each install also gets its own
# ClusterRoleBindings, suffixed with a hash of its namespace, so they cannot take
# permissions from one another. The namespace IS the instance identity; there is
# nothing else to name.
#
# What is NOT partitioned is the physical GPU pool. Namespace-scoped controllers
# each compute free capacity from the same nodes and allocate against it without
# seeing the other's claim, so several of them can oversubscribe a shared pool.
# That is a real caveat of this topology rather than a reason to forbid it: bound
# each install with a quota limiter, or give each tenant its own GPU nodes.
#

# llmsm_installations echoes "namespace name scope" for every scaling manager controller
# Deployment in the cluster. Scope is read from the container args rather than any
# label: --watch-namespace is what actually restricts the controller's cache, so it
# is the truth about what an install can reach.
# The args are read in the SAME list call, not one call per install. This function
# used to fetch each install's Deployment again to read its args, which is one round
# trip per scaling manager on the cluster: 12 installs on the shared GPU cluster cost ~13 SECONDS of the
# preflight, for information the first response already contained. The scope decision
# is unchanged -- it is still --watch-namespace in the container args, because that is
# what actually restricts the controller's cache -- it is just no longer paid for
# twice.
#
# Emitted as "namespace name scope" so callers are unaffected. The args are collapsed
# to one line inside the template; a newline in an arg would otherwise split one
# install across two records.
LLMSM_INSTALLS_TEMPLATE='{{range .items}}{{.metadata.namespace}} {{.metadata.name}} {{range (index .spec.template.spec.containers 0).args}}{{.}}~{{end}}{{"\n"}}{{end}}'

llmsm_installations() {
    local ns name args scope
    while read -r ns name args; do
        [ -n "$ns" ] || continue
        case "$args" in
            *--watch-namespace*) scope=namespace ;;
            *)                   scope=cluster ;;
        esac
        echo "$ns $name $scope"
    done < <(llmsm_installations_raw)
}

# llmsm_installations_raw lists the scaling manager Deployments cluster-wide, and reports the
# difference between "none found" and "not allowed to look".
#
# Listing across namespaces needs cluster-wide read, which a namespace admin does
# not have. Swallowing that Forbidden made the guard silently PASS for exactly the
# installer it exists to protect: a tenant could add a second controller to a
# cluster that already had one and be told nothing.
llmsm_installations_raw() {
    local out rc
    # control-plane=controller-manager is what makes it a CONTROLLER, and the
    # name label alone is not. Every scaling manager component carries the name label --
    # notably the warm-pool Deployment, which is a data-plane workload with no
    # reconcile loop at all. Matching on the name alone found one and, because
    # its args carry no --watch-namespace, classified it cluster-scoped by the
    # rule below. The install then refused with "A cluster-scoped scaling manager is already
    # installed: team-a/llmsm-warm-pool", naming a Deployment that
    # manages nothing, and there was no way to proceed except to disable a check
    # that was right in principle.
    # || rc=$?, not ; rc=$? -- see the note in install_core.sh. This one runs in
    # a process substitution, so the plain form fails silently: the subshell dies
    # on a Forbidden list, the caller reads no rows, and "no other scaling manager installed"
    # is exactly the answer that lets a second controller be installed.
    # The streams apart (kc_capture): a notice kubectl prints on stderr while
    # the list succeeds (a discovery error, a throttling notice) read, with
    # 2>&1, as a row -- "E1113 ..." a namespace, with no --watch-namespace, so
    # a cluster-scoped install that refused this one.
    rc=0
    kc_capture get deployments -A -l app.kubernetes.io/name=scaling-manager,control-plane=controller-manager -o go-template="$LLMSM_INSTALLS_TEMPLATE" || rc=$?
    out="$KC_OUT"
    if [ $rc -ne 0 ]; then
        # Unread is not none, whatever the reason: a timeout said nothing and
        # read as "no other scaling manager installed".
        if grep -qi 'forbidden' <<< "$(kc_drop_notices "$KC_ERR")"; then
            log_warning "Cannot check whether another scaling manager is already installed (listing Deployments cluster-wide is not permitted for you)."
        else
            log_warning "Cannot check whether another scaling manager is already installed (listing Deployments cluster-wide failed: $(kc_err_line "$KC_ERR"))."
        fi
        log_warning "  Two controllers managing the same workloads each allocate from the same free GPUs without seeing the other's claims. Confirm with whoever administers the cluster."
        return 0
    fi
    printf '%s
' "$out"
}

check_single_installation() {
    local peers="" cluster_wide="" ns name scope
    while read -r ns name scope; do
        [ -n "$ns" ] || continue
        # Our own namespace is an upgrade, not a collision.
        if [ "$ns" = "$LLMSM_NS" ]; then
            log_info "Existing the scaling manager in $LLMSM_NS — this install upgrades it in place."
            continue
        fi
        peers="${peers:+$peers, }$ns/$name ($scope-scoped)"
        [ "$scope" = "cluster" ] && cluster_wide="${cluster_wide:+$cluster_wide, }$ns/$name"
    done < <(llmsm_installations)

    [ -n "$peers" ] || return 0

    local want
    want="$(llmsm_install_scope)"

    # A cluster-wide controller already manages every namespace. Nothing joins it.
    if [ -n "$cluster_wide" ]; then
        log_error "A cluster-scoped scaling manager is already installed: $cluster_wide.

It manages every namespace, including $LLMSM_NS, so a second controller here would be
a second controller for the same workloads.

Pick one:
  - use it as it is — it already covers $LLMSM_NS
  - upgrade it:                     LLMSM_NS=${cluster_wide%%/*} ...
  - replace it with per-namespace controllers:
        make undeploy-scaling-manager SCOPE=cluster LLMSM_NS=${cluster_wide%%/*}
    then install one LLMSM_SCOPE=namespace per namespace that needs one
  - or remove it and install this one in its place"
    fi

    # Installing cluster-scoped on top of existing namespace-scoped installs would
    # swallow their namespaces the moment it starts.
    if [ "$want" = "cluster" ]; then
        log_error "Installing a cluster-scoped scaling manager, but namespace-scoped ones already exist: $peers.

A cluster-scoped controller manages every namespace, including theirs — both would
then decide for the same workloads.

Pick one:
  - install this one namespace-scoped too:   LLMSM_SCOPE=namespace
  - or remove the existing ones first, then install cluster-scoped"
    fi

    # Namespace-scoped beside namespace-scoped: the supported multi-controller shape.
    log_info "the scaling manager already installed in: $peers. Continuing: this install is namespace-scoped, so it manages only $LLMSM_NS and they manage only theirs."
    log_info "  Each install has its own external-scaler Service and its own ClusterRoleBindings (suffixed per namespace), so they do not overlap. They DO draw on the same physical GPUs — bound them with quota limiters, or give each its own GPU nodes."
}
