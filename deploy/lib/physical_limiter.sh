#!/usr/bin/env bash
#
# Cluster-admin action: turn the physical (gpu-inventory) GPU limiter on, or off,
# for every scaling manager controller on the cluster that passes verification -- and name
# the ones that do not.
#
# Requires vars: LLMSM_LIMITER_TYPE, LLMSM_LIMITER_TARGETS. LLMSM_POLICY_NS, when set,
# must be scaling-manager-policy (pl_refuse_policy_ns_override).
# Optional vars: LLMSM_CONTROLLER_SA, LLMSM_CONTROLLER_IMAGE_REPOS, LLMSM_IMAGE_REPO
# (pl_controller_targets).
# Requires funcs: log_info/log_success/log_warning/log_error, llmsm_ns_suffix.
#
# Why this is one command rather than a documented sequence of four:
#
# Declaring the limiter is the easy part — a ConfigMap. The three things around it
# are what make it work, and each one, left out, fails in a way that looks like
# something else:
#
#   1. the policy must live where a TENANT CANNOT EDIT IT, or it is not a bound;
#   2. every controller must be able to READ it, or that controller refuses to
#      start rather than run unbounded while you believe a limit applies;
#   3. every controller must be able to LIST NODES, or it goes NotReady — a
#      GPU-aware optimizer that cannot resolve an accelerator would otherwise
#      charge every variant to no pool and silently stop scaling anything up.
#
# (3) is the one that turns this into an operation with blast radius: declaring
# the limiter takes out every existing the scaling manager that lacks node access, all at once.
# So this grants it to each of them FIRST, and says which controllers it touched.

# PL_WELL_KNOWN_POLICY_NS is the namespace an unlabelled self-managed controller
# reads cluster policy from: config.WellKnownPolicyNamespace
# (internal/config/helpers.go), a constant the controller does not let anything
# configure, so a tenant cannot point their scaling manager somewhere more permissive.
PL_WELL_KNOWN_POLICY_NS=scaling-manager-policy

# pl_policy_ns echoes the namespace cluster policy is published in: always the
# well-known one.
#
# It honoured LLMSM_POLICY_NS, and that was a bound that bound nobody: no
# controller reads a namespace by that setting. An unlabelled self-managed
# controller reads scaling-manager-policy whatever this command is told, so publishing to
# LLMSM_POLICY_NS=platform-policy wrote a policy no controller read, and the
# `wellknown` resolution then claimed controllers read it. A namespace a
# scaling-manager.llmd.ai/policy-namespace LABEL names is found per controller
# (pl_policy_resolution) and written without being named here. So
# LLMSM_POLICY_NS has no correct use left, and a value other than the well-known
# name is refused rather than ignored (pl_refuse_policy_ns_override): the caller
# meant something by it.
pl_policy_ns() {
    echo "$PL_WELL_KNOWN_POLICY_NS"
}

# pl_refuse_policy_ns_override stops the command when LLMSM_POLICY_NS names a
# namespace other than the well-known one. Called directly, never in `$( )`,
# where its exit would end only the subshell.
pl_refuse_policy_ns_override() {
    if [ -n "${LLMSM_POLICY_NS:-}" ] && [ "$LLMSM_POLICY_NS" != "$PL_WELL_KNOWN_POLICY_NS" ]; then
        log_error "LLMSM_POLICY_NS=$LLMSM_POLICY_NS is no longer supported: no controller reads policy from a namespace this command chooses. An unlabelled self-managed controller reads $PL_WELL_KNOWN_POLICY_NS (fixed in the controller); to send one elsewhere, label its namespace instead:
    kubectl label namespace <managed-namespace> scaling-manager.llmd.ai/policy-namespace=$LLMSM_POLICY_NS
and re-run without LLMSM_POLICY_NS -- this command then publishes there too."
    fi
}

# pl_controller_namespaces echoes every namespace running a scaling manager controller.
#
# Discovered rather than configured, because the hazard is precisely the install
# you forgot: this command's job is to leave NO controller unable to honour the
# policy it is about to publish. LLMSM_LIMITER_TARGETS overrides it when you mean
# to grant a subset — and then the ones you left out are named in the warning.
pl_controller_namespaces() {
    if [ -n "${LLMSM_LIMITER_TARGETS:-}" ]; then
        # Word-splitting is wanted here; PATHNAME expansion is not. Unguarded,
        # LLMSM_LIMITER_TARGETS='*' expanded against the working directory and this
        # command published a policy ConfigMap into a namespace named after every
        # file in it -- fifteen of them, measured. Same guard, same reason, as the
        # one on LLMSM_QUOTAS.
        local reset_t_glob=1
        case "$-" in *f*) reset_t_glob=0 ;; esac
        set -f
        printf '%s\n' $LLMSM_LIMITER_TARGETS
        [ "$reset_t_glob" = 1 ] && set +f
        return 0
    fi
    # A FAILED listing is not an empty cluster, and folding the two together is
    # how this command came to announce "the limiter is now in force for every
    # the scaling manager on this cluster" having reached none of them: `2>/dev/null` turned a
    # denied or erroring `get deploy -A` into no output, the caller warned "No
    # the scaling manager controller found", published to the well-known namespace only, and
    # every admin-owned controller went on reading its own unbounded ConfigMap.
    #
    # Measured: a controller in tenant-a, its policy still `limiters: []`, under
    # a SUCCESS banner claiming the opposite.
    # RETURNS non-zero; it does not log_error. Both callers invoke this inside
    # `$( )`, where log_error's `exit 1` ends the SUBSHELL and nothing else --
    # the command carried on and printed "the limiter is now in force for every
    # the scaling manager on this cluster" having listed none of them. The status is the only
    # thing that crosses a command substitution, so the refusal is the caller's
    # to make; both check it.
    local out
    if ! out="$(kubectl get deploy -A -l app.kubernetes.io/name=scaling-manager \
        -o jsonpath='{range .items[*]}{.metadata.namespace}{"\n"}{end}' 2>/dev/null)"; then
        return 1
    fi
    printf '%s' "$out" | sort -u
}

# pl_expected_sa echoes the ServiceAccount this project's controller runs as:
# config/base/rbac names it controller-manager and every overlay adds the scaling-manager-
# namePrefix. LLMSM_CONTROLLER_SA overrides it for an install built another way.
pl_expected_sa() {
    echo "${LLMSM_CONTROLLER_SA:-scaling-manager-controller-manager}"
}

# pl_image_repos echoes the image repositories a controller may run from,
# space-separated. LLMSM_CONTROLLER_IMAGE_REPOS, when set, is the whole list.
# Otherwise the published repository, plus the installer LLMSM_IMAGE_REPO (a repo
# or a full image reference; its tag and digest are dropped) -- so a controller
# built from a custom IMG is recognised without naming it twice.
pl_image_repos() {
    if [ -n "${LLMSM_CONTROLLER_IMAGE_REPOS:-}" ]; then
        echo "$LLMSM_CONTROLLER_IMAGE_REPOS"
        return 0
    fi
    local repos="ghcr.io/llm-d-incubation/llm-d-scaling-manager" extra
    if [ -n "${LLMSM_IMAGE_REPO:-}" ]; then
        extra="$(pl_image_repo "$LLMSM_IMAGE_REPO")"
        [ "$extra" = "$repos" ] || repos="$repos $extra"
    fi
    echo "$repos"
}

# pl_image_repo echoes an image reference without its tag and digest. A colon
# in the last path segment is a tag; one earlier is a registry port.
pl_image_repo() {
    local img="${1%%@*}"
    case "${img##*/}" in *:*) img="${img%:*}" ;; esac
    echo "$img"
}

# pl_named_target reports whether namespace $1 is named in LLMSM_LIMITER_TARGETS:
# a namespace the admin vouched for by name.
pl_named_target() {
    local t reset_glob=1
    [ -n "${LLMSM_LIMITER_TARGETS:-}" ] || return 1
    case "$-" in *f*) reset_glob=0 ;; esac
    set -f
    for t in $LLMSM_LIMITER_TARGETS; do
        if [ "$t" = "$1" ]; then
            [ "$reset_glob" = 1 ] && set +f
            return 0
        fi
    done
    [ "$reset_glob" = 1 ] && set +f
    return 1
}

# PL_DEPLOY_ROWS is the program that turns a Deployment list into one row per
# Deployment: name, ServiceAccount, manager image, managed namespace, selector.
#
#   * ServiceAccount: absent is the namespace default account.
#   * manager image: the container NAMED manager, never containers[0] -- a
#     sidecar listed first would otherwise decide both the image check and
#     which namespace the controller manages. A dash when there is none.
#   * managed namespace: what --watch-namespace resolves to, the way the kubelet
#     expands it: a $(VAR) reference is looked up in the manager env, and a
#     fieldRef to metadata.namespace is the pod namespace. A flag beats the
#     WATCH_NAMESPACE env var (viper). A star means cluster-scoped. The literal
#     $(POD_NAMESPACE) left unexpanded is the pod namespace too, as before.
#   * selector: spec.selector.matchLabels as k=v,k=v, or a dash.
PL_DEPLOY_ROWS='
def resolve($c; $ns; $v; $depth):
  if ($v | type) == "string" and ($v | startswith("$(")) and ($v | endswith(")")) and $depth < 4 then
    ($v[2:-1]) as $n
    | ([$c.env[]? | select(.name == $n)][0]) as $e
    | if $e == null then $v
      elif ($e.valueFrom.fieldRef.fieldPath // "") == "metadata.namespace" then $ns
      elif $e.valueFrom != null then $v
      else resolve($c; $ns; ($e.value // ""); $depth + 1) end
  else $v end;
def managed($c; $ns):
  if $c == null then "*" else
    ((($c.command // [])[1:]) + ($c.args // [])) as $a
    | ([range(0; $a | length) | select($a[.] | test("^--?watch-namespace(=|$)"))] | last) as $i
    | (if $i == null then ([$c.env[]? | select(.name == "WATCH_NAMESPACE") | .value // ""][0] // "")
       elif ($a[$i] | test("=")) then ($a[$i] | sub("^[^=]*="; ""))
       else ($a[$i + 1] // "") end) as $raw
    | resolve($c; $ns; $raw; 0) as $w
    | if $w == "" then "*" elif $w == "$(POD_NAMESPACE)" then $ns else $w end
  end;
.items[]
| ([.spec.template.spec.containers[]? | select(.name == "manager")][0]) as $m
| [ .metadata.name,
    (.spec.template.spec.serviceAccountName // .spec.template.spec.serviceAccount // "default"),
    ($m.image // "-"),
    managed($m; $ns),
    ((.spec.selector.matchLabels // {}) | to_entries | map(.key + "=" + .value) | join(",") | if . == "" then "-" else . end)
  ] | @tsv'

# pl_controller_targets reads namespaces on stdin, one per line, and echoes
# `<namespace> <deployment> <serviceaccount> <managed> <selector>` for each
# Deployment there that passes verification -- the only ones this command may
# grant to. <managed> is the namespace the controller manages (`*` for
# cluster-scoped, `-` when not known); <selector> is its pod selector.
#
# The label alone is not identity. Anyone who can create a Deployment can put
# app.kubernetes.io/name=scaling-manager on it and name any
# ServiceAccount in their namespace -- `default`, say, which every pod there
# runs as -- and this cluster-admin command then granted that account
# cluster-wide node read. So a labelled Deployment is granted only when it is
# also shaped like an install of this project:
#
#   * its pod runs as the controller ServiceAccount (pl_expected_sa);
#   * it has a container named `manager` (config/base/manager/deployment.yaml)
#     whose image comes from a controller repository (pl_image_repos). Args are
#     not used: installs patch them, and a flag is as easy to copy as an image.
#
# SIGNALS, NOT PROOF. Every one of them is satisfiable by any tenant: they can
# create a ServiceAccount of that name and run the public image in a container
# named manager. What the check buys is narrower -- the grant goes only to an
# account of THAT name, never to `default` or one the Deployment merely names --
# and it does not stop a tenant who means it. Where tenants are not trusted, run
# their controllers in admin-owned namespaces (the LLMSM_WATCH_NS shape) and name
# exactly those namespaces in LLMSM_LIMITER_TARGETS.
#
# A namespace named in LLMSM_LIMITER_TARGETS relaxes the image check (the admin
# vouched for it by name, which is how a controller from a mirror or a custom
# IMG gets granted); the ServiceAccount and the manager container are still
# required there. A named namespace with no labelled Deployment is SKIPPED
# (`! <namespace> - - no controller found`): there is nothing to bind there.
#
# Every skipped Deployment is named on stderr, and echoed on stdout as
# `! <namespace> <deployment> <managed> <reason...>` (no namespace starts with
# `!`), so the caller can say which controllers the limiter does NOT bind -- a
# list on stderr alone scrolled away above the success banner. RETURNS
# non-zero when a namespace cannot be read, for the reason
# pl_controller_namespaces gives.
pl_controller_targets() {
    local ns json rows name sa image managed sel repo r allowed expected repos reason
    expected="$(pl_expected_sa)"
    repos="$(pl_image_repos)"
    while IFS= read -r ns; do
        [ -n "$ns" ] || continue
        if ! json="$(kubectl get deploy -n "$ns" -l app.kubernetes.io/name=scaling-manager \
            -o json 2>/dev/null)" || [ -z "$json" ]; then
            return 1   # kubectl answers a List even when nothing matches
        fi
        if ! rows="$(printf '%s' "$json" | jq -r --arg ns "$ns" "$PL_DEPLOY_ROWS" 2>/dev/null)"; then
            return 1
        fi
        if [ -z "$rows" ]; then
            # Named, and nothing there: SKIPPED, never written. Granting the
            # account a controller "about to be installed" would run as, and
            # writing policy where it would read, bound nothing that runs today
            # -- and an install that later comes up managing another namespace
            # reads a different policy altogether. The install's own prereqs
            # phase grants a controller its read when it is installed
            # (pl_grant_published_policy_read).
            if [ -n "${LLMSM_LIMITER_TARGETS:-}" ]; then
                log_warning "  SKIPPED $ns: no controller found (no labelled Deployment). Not granted, and no policy written there."
                echo "! $ns - - no controller found"
            fi
            continue
        fi
        while IFS=$'\t' read -r name sa image managed sel; do
            reason=""
            if [ "$sa" != "$expected" ]; then
                reason="it runs as ServiceAccount '$sa', not the controller's '$expected' (LLMSM_CONTROLLER_SA names a different controller account)"
            elif [ "$image" = "-" ]; then
                reason="it has no container named 'manager', so it is not shaped like a controller install"
            else
                repo="$(pl_image_repo "$image")"
                allowed=0
                for r in $repos; do [ "$repo" = "$r" ] && allowed=1; done
                if [ "$allowed" -eq 0 ]; then
                    if pl_named_target "$ns"; then
                        log_info "  $ns/$name: manager image '$image' is not from $repos; accepted because LLMSM_LIMITER_TARGETS names $ns"
                    else
                        reason="its manager container runs '$image', which is not from a controller repository ($repos). A genuine controller from a mirror or a custom IMG: name $ns in LLMSM_LIMITER_TARGETS, or the mirror in LLMSM_CONTROLLER_IMAGE_REPOS"
                    fi
                fi
            fi
            if [ -n "$reason" ]; then
                log_warning "  SKIPPED $ns/$name: $reason. Not granted."
                echo "! $ns $name $managed $reason"
                continue
            fi
            echo "$ns $name $sa $managed $sel"
        done <<< "$rows"
    done
}

# pl_policy_label echoes the scaling-manager.llmd.ai/policy-namespace label on Namespace $1,
# or nothing when it has none. On a failed read it echoes kubectl's error and
# returns non-zero: a label that could not be read is not an absent one, because
# it may point somewhere else entirely.
#
# The streams are kept apart (kc_capture): with 2>&1 a warning kubectl printed
# on stderr while SUCCEEDING was read as the label, and the policy was then
# written to -- and the controller granted -- a namespace named "Warning: ...".
pl_policy_label() {
    local rc=0
    kc_capture get namespace "$1" \
        -o jsonpath='{.metadata.labels.scaling-manager\.llmd\.ai/policy-namespace}' || rc=$?
    if [ "$rc" -ne 0 ]; then
        printf '%s\n' "${KC_ERR:-kubectl exited $rc}"
        return "$rc"
    fi
    printf '%s' "$KC_OUT"
}

# pl_policy_resolution echoes `<namespace> <source>`: where the controller in
# namespace $1, managing namespace $2 (`*` cluster-scoped, `-` unknown), reads
# its policy ConfigMap, and why -- own | label | wellknown.
#
# The controller's rule (internal/controller/policy_namespace.go), and it is
# deliberate: only a SELF-MANAGED controller -- one whose watch namespace is its
# own, i.e. the tenant-owned shape -- reads policy from elsewhere: the namespace
# its managed Namespace is labelled with (scaling-manager.llmd.ai/policy-namespace), else the
# well-known one. An admin-owned controller (cluster-scoped, or namespace-scoped
# pointed at someone else's namespace) keeps reading its OWN ConfigMap, because
# letting scaling-manager-policy win there would silently flip a correctly bounded cluster to
# unbounded the moment anyone created that namespace for an unrelated tenant.
#
# The label is honoured here as pl_grant_published_policy_read and
# utilization-share.sh honour it: writing to scaling-manager-policy for a labelled
# namespace was a no-op that reported success.
#
# RETURNS non-zero when the label cannot be read; callers call this in `$( )`.
pl_policy_resolution() {
    local ns="$1" managed="${2:--}" label
    case "$managed" in
        '*'|-) echo "$ns own"; return 0 ;;
    esac
    if [ "$managed" != "$ns" ]; then
        echo "$ns own"      # watches someone else's namespace: admin-owned
        return 0
    fi
    label="$(pl_policy_label "$ns")" || return 1
    if [ -n "$label" ]; then
        echo "$label label"
    else
        echo "$PL_WELL_KNOWN_POLICY_NS wellknown"
    fi
}

# pl_policy_cm_ns echoes only the namespace of pl_policy_resolution.
pl_policy_cm_ns() {
    local r
    r="$(pl_policy_resolution "$@")" || return 1
    echo "${r%% *}"
}

# pl_needs_restart reports whether the controller whose pods match selector $2
# in namespace $1 started before namespace $3 existed -- i.e. resolved its policy
# namespace before the well-known one was there.
#
# The resolution runs once, before the manager starts. A controller that came up
# when scaling-manager-policy did not exist is reading its own ConfigMap and will go on doing
# so -- so publishing policy without restarting it is a no-op that looks like a
# success. Compared against the namespace's creation time rather than assumed,
# because restarting controllers that do not need it is its own small outage. A
# namespace that does not exist yet is about to be created, after every running
# pod started.
#
# The NAMED Deployment's own pods (its matchLabels), and the newest Running one:
# the controller label also selects impostors, and `.items[0]` of it read
# whichever pod the API listed first -- a terminating one, or someone else's.
pl_needs_restart() {
    local ns="$1" sel="${2:--}" target="$3" started created rc=0
    [ "$sel" != "-" ] && [ -n "$sel" ] || return 1
    kc_capture get namespace "$target" -o jsonpath='{.metadata.creationTimestamp}' || rc=$?
    created="$KC_OUT"
    if [ "$rc" -ne 0 ]; then
        case "$KC_ERR" in
            *NotFound*|*"not found"*) created="" ;;
            *) return 1 ;;   # cannot tell: never restart on a guess
        esac
    elif [ -z "$created" ]; then
        return 1             # an answer with no timestamp: cannot tell either
    fi
    started="$(kubectl get pods -n "$ns" -l "$sel" -o json 2>/dev/null \
        | jq -r '[.items[]? | select(.status.phase == "Running") | .status.startTime // empty] | sort | last // empty' 2>/dev/null)" || started=""
    [ -n "$started" ] || return 1
    [ -n "$created" ] || return 0
    [[ "$started" < "$created" ]]   # RFC3339 sorts lexically
}

# pl_grant_node_read grants one controller the cluster-scoped node read.
#
# Same object names the namespace-scoped overlay uses, so a later install or
# upgrade is an idempotent apply over these rather than a second, competing pair.
pl_grant_node_read() {
    local ns="$1" sa="$2" suffix
    suffix="$(llmsm_ns_suffix "$ns")"
    kubectl apply -f - >/dev/null <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: scaling-manager-node-reader-role-${suffix}
  labels:
    app.kubernetes.io/name: scaling-manager
rules:
- apiGroups: [""]
  resources: ["nodes"]
  verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: scaling-manager-node-reader-rolebinding-${suffix}
  labels:
    app.kubernetes.io/name: scaling-manager
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: scaling-manager-node-reader-role-${suffix}
subjects:
- kind: ServiceAccount
  name: ${sa}
  namespace: ${ns}
EOF
}

# pl_grant_kueue_read grants one controller the cluster-scoped Kueue read a quota
# entry with kueue.enabled needs: ClusterQueues and ResourceFlavors, and get on
# Namespaces to evaluate a ClusterQueue namespaceSelector. The LocalQueues of
# its nominal grant come with the controller's own Role. The cluster-wide
# Workload and LocalQueue read that cohort borrowing needs is NOT here: it is
# its own ClusterRole, granted only to a controller whose policy counts
# borrowing (pl_grant_kueue_borrow_read).
#
# Same object names as components/kueue-reader, for the same reason as
# pl_grant_node_read. Without this, a cluster policy carrying the kueue block
# reached every controller, and one installed before the kueue-reader
# prerequisite existed read it, logged "external quota source unreadable and
# never read" each cycle, and ran on the static entry alone -- which for the
# recommended `-1` budget is no cap at all.
#
# $3 is the namespace the controller manages (`*` cluster-scoped). The Namespace
# rule it writes is the one the installer writes for the same controller, so the
# two commands cannot undo each other:
#
#   * namespace-scoped: the MANAGED namespace, plus every name the live role
#     already carries (llmsm_kueue_reader_names, shared with the installer). It
#     lists LocalQueues there and nowhere else. Granting its OWN namespace
#     instead took `get namespaces/foo` from a controller in scaling-manager-foo managing
#     foo on every enable, and its Kueue source bounded foo at nothing until the
#     next setup-prereqs gave it back. Never fewer names than the role holds.
#   * cluster-scoped: every Namespace (no resourceNames) when the admin named
#     its namespace in LLMSM_LIMITER_TARGETS, or when its ServiceAccount already
#     reads every Namespace -- a cluster-scoped install does, through its manager
#     ClusterRole -- so the rule widens nothing. Otherwise its own namespace,
#     plus the names the role carries.
#
# <managed> comes from the Deployment's args and env, which whoever owns the
# namespace can edit. One named Namespace is what the installer would grant for
# the same args; the read of EVERY Namespace is not, so a tenant that drops
# --watch-namespace is never handed it by a cluster-admin command.
# The ClusterQueue and ResourceFlavor reads stay as the installer renders them
# (components/kueue-reader): every Kueue-reading controller needs them, and a
# list cannot be limited by name.
pl_grant_kueue_read() {
    local ns="$1" sa="$2" managed="${3:--}" suffix names="" first="$1" held="" rc=0 n
    suffix="$(llmsm_ns_suffix "$ns")"
    case "$managed" in
        '*')
            if pl_named_target "$ns"; then
                first='*'
            else
                held="$(pl_can_watch_namespaces "$ns" "$sa")"
                [ "$held" = yes ] && first='*'
            fi
            ;;
        -|'') ;;
        *) first="$managed" ;;
    esac
    if [ "$first" != '*' ]; then
        llmsm_kueue_reader_names "$ns" "$first" || rc=$?
        if [ "$rc" -ne 0 ]; then
            log_warning "  Could not read ClusterRole scaling-manager-kueue-reader-role-${suffix} (${KC_ERR}). Not rewriting it without knowing which Namespaces it already names."
            return 1
        fi
        # An empty resourceNames list is NO restriction, so a managed value that
        # is not a Namespace name (an unexpanded $(VAR), say) falls back to the
        # controller's own namespace rather than to every Namespace.
        [ -n "$LLMSM_KUEUE_READER_NAMES" ] || LLMSM_KUEUE_READER_NAMES="$ns"
        for n in $LLMSM_KUEUE_READER_NAMES; do names="${names:+$names, }\"$n\""; done
        names="
  resourceNames: [${names}]"
        case "$held" in
            no) log_warning "  $ns/$sa looks cluster-scoped (no --watch-namespace) but cannot read Namespaces cluster-wide, which a cluster-scoped install gets from its manager ClusterRole. Its Kueue Namespace read is limited to ${LLMSM_KUEUE_READER_NAMES// /, }, so Kueue quota in any other namespace is bounded at nothing. If it is a cluster-scoped install, re-run its prerequisites: $(LLMSM_NS="$ns" LLMSM_SCOPE=cluster LLMSM_WATCH_NS='' llmsm_make_cmd setup-prereqs)" ;;
            unknown) log_warning "  $ns/$sa looks cluster-scoped (no --watch-namespace); could not tell whether it reads Namespaces cluster-wide (impersonation was not allowed). Its Kueue Namespace read is limited to ${LLMSM_KUEUE_READER_NAMES// /, }. Check: kubectl auth can-i watch namespaces --as system:serviceaccount:${ns}:${sa}" ;;
        esac
    fi
    kubectl apply -f - >/dev/null <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: scaling-manager-kueue-reader-role-${suffix}
  labels:
    app.kubernetes.io/name: scaling-manager
rules:
- apiGroups: ["kueue.x-k8s.io"]
  resources: ["clusterqueues", "cohorts", "resourceflavors"]
  verbs: ["get", "list"]
- apiGroups: [""]
  resources: ["namespaces"]${names}
  verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: scaling-manager-kueue-reader-rolebinding-${suffix}
  labels:
    app.kubernetes.io/name: scaling-manager
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: scaling-manager-kueue-reader-role-${suffix}
subjects:
- kind: ServiceAccount
  name: ${sa}
  namespace: ${ns}
EOF
}

# pl_grant_kueue_controller_read grants ServiceAccount $2 in $1, through a Role
# in the Kueue namespace, get on the
# kueue-controller-manager Deployment and on the controller's leader-election
# Lease, both by name: borrowing reads the version from the one and liveness
# from the other. Without it a namespace-scoped controller reads both as
# Forbidden, which freezes borrowing (kueue-controller-down). A cluster-scoped
# install has both from its manager ClusterRole.
#
# The Kueue namespace is KUEUE_NAMESPACE when given, else $3 -- the
# kueue.controllerNamespace the policy keeps (policy_kueue_namespace) -- else
# kueue-system. Without $3 a re-run that did not repeat KUEUE_NAMESPACE put the
# Role in kueue-system while the controller read the namespace its policy names.
#
# A Role, not part of components/kueue-reader: a kustomize overlay moves every
# namespaced object into the install namespace, and this one must live in the
# Kueue namespace. Skipped, with a note, when that namespace does not exist.
#
# Both carry LLMSM_KUEUE_CONTROLLER_READER_LABEL=<suffix of $1>, so the uninstall
# and disable-limiter find them by label wherever KUEUE_NAMESPACE put them
# (pl_revoke_kueue_controller_read): the namespace is not recorded anywhere
# else, and a revoke that guessed kueue-system left a grant made elsewhere.
LLMSM_KUEUE_CONTROLLER_READER_LABEL="llm-d.ai/scaling-manager-kueue-controller-reader"
pl_grant_kueue_controller_read() {
    local ns="$1" sa="$2" kns="${KUEUE_NAMESPACE:-${3:-kueue-system}}" suffix
    suffix="$(llmsm_ns_suffix "$ns")"
    if ! kubectl get namespace "$kns" >/dev/null 2>&1; then
        log_info "  No namespace $kns (KUEUE_NAMESPACE, else the policy's kueue.controllerNamespace): not granting the Kueue controller read. Kueue borrowing, if counted, freezes as kueue-controller-down until it is granted."
        return 0
    fi
    kubectl apply -f - >/dev/null <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: scaling-manager-kueue-controller-reader-${suffix}
  namespace: ${kns}
  labels:
    app.kubernetes.io/name: scaling-manager
    ${LLMSM_KUEUE_CONTROLLER_READER_LABEL}: "${suffix}"
rules:
- apiGroups: ["apps"]
  resources: ["deployments"]
  resourceNames: ["kueue-controller-manager"]
  verbs: ["get"]
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  resourceNames: ["c1f6bfd2.kueue.x-k8s.io"]
  verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: scaling-manager-kueue-controller-reader-${suffix}
  namespace: ${kns}
  labels:
    app.kubernetes.io/name: scaling-manager
    ${LLMSM_KUEUE_CONTROLLER_READER_LABEL}: "${suffix}"
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: scaling-manager-kueue-controller-reader-${suffix}
subjects:
- kind: ServiceAccount
  name: ${sa}
  namespace: ${ns}
EOF
}

# pl_revoke_kueue_controller_read removes the Role and RoleBinding that
# pl_grant_kueue_controller_read made for the controller installed in $1: by
# label in every namespace, then by name in KUEUE_NAMESPACE (default
# kueue-system) for a grant made before the label existed. A label delete that
# is refused cluster-wide is reported, not passed over. Prints what failed and
# returns 1 when anything did.
pl_revoke_kueue_controller_read() {
    local ns="$1" suffix name kns="${KUEUE_NAMESPACE:-kueue-system}" out err=""
    suffix="$(llmsm_ns_suffix "$ns")"
    name="scaling-manager-kueue-controller-reader-${suffix}"
    if ! out="$(kubectl delete role,rolebinding -A -l "${LLMSM_KUEUE_CONTROLLER_READER_LABEL}=${suffix}" \
        --ignore-not-found 2>&1 >/dev/null)"; then
        err="by label ${LLMSM_KUEUE_CONTROLLER_READER_LABEL}=${suffix} in every namespace: ${out}"
    fi
    if ! out="$(kubectl delete role,rolebinding "$name" -n "$kns" --ignore-not-found 2>&1 >/dev/null)"; then
        err="${err:+$err; }by name in ${kns}: ${out}"
    fi
    [ -z "$err" ] && return 0
    printf '%s' "$err"
    return 1
}

# pl_grant_kueue_borrow_read grants ServiceAccount $2 in $1 the cluster-wide
# Kueue read that counting cohort borrowing needs: Workloads (get, list, watch)
# and LocalQueues (get, list) in EVERY namespace. A lender waiting in another
# namespace must stop new borrowing, and a LowerPriority lender is judged by
# its Workloads' priorities, so the controller's own namespace is not enough.
#
# Its own ClusterRole, not part of components/kueue-reader, because it reaches
# every tenant's Kueue Workloads, pod templates included. The installer grants
# it only to a controller some limiter policy of which counts borrowing (a
# quota entry with kueue.enabled and countBorrowable not false), and revokes it
# when none does (pl_sync_kueue_borrow_read). Denied, borrowing is withheld as
# lenders-unreadable; the nominal Kueue bound is unaffected. A cluster-scoped
# install reads Workloads through its manager ClusterRole whatever this says.
pl_grant_kueue_borrow_read() {
    local ns="$1" sa="$2" suffix
    suffix="$(llmsm_ns_suffix "$ns")"
    kubectl apply -f - >/dev/null <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: scaling-manager-kueue-borrow-reader-role-${suffix}
  labels:
    app.kubernetes.io/name: scaling-manager
rules:
- apiGroups: ["kueue.x-k8s.io"]
  resources: ["workloads"]
  verbs: ["get", "list", "watch"]
- apiGroups: ["kueue.x-k8s.io"]
  resources: ["localqueues"]
  verbs: ["get", "list"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: scaling-manager-kueue-borrow-reader-rolebinding-${suffix}
  labels:
    app.kubernetes.io/name: scaling-manager
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: scaling-manager-kueue-borrow-reader-role-${suffix}
subjects:
- kind: ServiceAccount
  name: ${sa}
  namespace: ${ns}
EOF
}

# pl_revoke_kueue_borrow_read removes what pl_grant_kueue_borrow_read made for
# the controller installed in $1: the binding first, then the role. Absent is
# fine. Prints what failed and returns 1 when anything did.
pl_revoke_kueue_borrow_read() {
    local suffix out err=""
    suffix="$(llmsm_ns_suffix "$1")"
    if ! out="$(kubectl delete clusterrolebinding "scaling-manager-kueue-borrow-reader-rolebinding-${suffix}" --ignore-not-found 2>&1 >/dev/null)"; then
        err="ClusterRoleBinding scaling-manager-kueue-borrow-reader-rolebinding-${suffix}: ${out}"
    fi
    if ! out="$(kubectl delete clusterrole "scaling-manager-kueue-borrow-reader-role-${suffix}" --ignore-not-found 2>&1 >/dev/null)"; then
        err="${err:+$err; }ClusterRole scaling-manager-kueue-borrow-reader-role-${suffix}: ${out}"
    fi
    [ -z "$err" ] && return 0
    printf '%s' "$err"
    return 1
}

# pl_sync_kueue_borrow_read grants ServiceAccount $2 in $1 the borrowing read
# when $3 is yes, revokes it when $3 is no, and leaves it as it is, with a
# warning, when $3 is unknown -- a policy that could not be read is not one
# that counts nothing, and revoking on a guess would stop borrowing. Logs what
# it did; returns 1 when a grant or revoke failed.
#
# A revoke of a read that was granted says to restart the controller: its
# Workload informer keeps the Workloads it already cached, and a policy that
# still counts borrowing would go on counting lenders from that stale copy.
pl_sync_kueue_borrow_read() {
    local ns="$1" sa="$2" want="$3" suffix err had=no
    suffix="$(llmsm_ns_suffix "$ns")"
    case "$want" in
        yes)
            if ! pl_grant_kueue_borrow_read "$ns" "$sa"; then
                log_warning "  Could not grant $ns/$sa the Kueue borrowing read (ClusterRole scaling-manager-kueue-borrow-reader-role-${suffix}). Borrowing is withheld (lenders-unreadable) until it is granted; the nominal Kueue cap still applies."
                return 1
            fi
            log_info "  $ns/$sa may read Kueue Workloads and LocalQueues in every namespace: its policy counts cohort borrowing (ClusterRole scaling-manager-kueue-borrow-reader-role-${suffix})"
            ;;
        no)
            if kc_capture get clusterrolebinding "scaling-manager-kueue-borrow-reader-rolebinding-${suffix}" -o name && [ -n "$KC_OUT" ]; then
                had=yes
            fi
            if ! err="$(pl_revoke_kueue_borrow_read "$ns")"; then
                log_warning "  Could not revoke the Kueue borrowing read of $ns/$sa: $err"
                return 1
            fi
            if [ "$had" = yes ]; then
                log_warning "  Revoked the cluster-wide Kueue Workload read of $ns/$sa: no policy it reads counts cohort borrowing. RESTART THE CONTROLLER in $ns (kubectl rollout restart deployment -n $ns <controller>): until then it keeps the Workloads it already cached."
            fi
            ;;
        *)
            log_warning "  Could not tell whether a policy of $ns/$sa counts Kueue cohort borrowing${4:+ ($4)}, so its cluster-wide Workload read (ClusterRole scaling-manager-kueue-borrow-reader-role-${suffix}) is left as it is."
            ;;
    esac
    return 0
}

# pl_published_kueue <policy namespace> <limiter> reads the policy in the
# namespace and sets, for what enable_physical_limiter is about to publish
# there: PL_BORROW_WANTED (yes | no | unknown) -- whether the policy, with this
# limiter composed in, counts cohort borrowing -- and PL_KUEUE_KEPT_NS, the
# kueue.controllerNamespace it keeps. Composed the way pl_set_limiter composes,
# so a kept countBorrowable: false (LLMSM_QUOTA_KUEUE_BORROW not given) counts.
#
# Decided from that policy ALONE. A controller that reads its policy from
# another namespace reads its limiters from nowhere else, and this command
# makes sure it does (the Namespace read, the policy read, the restart). Its
# own ConfigMap -- which the tenant writes, for a self-managed controller --
# used to be ORed in, so a tenant who put kueue.enabled there was granted the
# read of every tenant's Workloads by this cluster-admin command, and one who
# broke it kept that read for good (unknown is never revoked). The admin phase
# decides the same way (llmsm_kueue_borrow_wanted).
PL_BORROW_WANTED=unknown
PL_KUEUE_KEPT_NS=""
pl_published_kueue() {
    local cm_ns="$1" limiter="$2" cur="" rc=0 composed
    PL_BORROW_WANTED=unknown
    PL_KUEUE_KEPT_NS=""
    kc_capture get configmap scaling-manager-scaling-policy-config -n "$cm_ns" -o jsonpath='{.data.default}' || rc=$?
    if [ "$rc" -ne 0 ]; then
        case "$KC_ERR" in
            *NotFound*|*"not found"*) ;;
            *) return 0 ;;
        esac
    else
        cur="$KC_OUT"
    fi
    [ -n "$cur" ] || cur='limiters: []'
    PL_KUEUE_KEPT_NS="$(printf '%s\n' "$cur" | policy_kueue_namespace)"
    # A subshell: policy_declared_limiters refuses through log_error, which
    # exits, and its warnings were already printed by the validation above.
    composed="$( (policy_declared_limiters "$limiter" "$cur" default compose && printf '%s\n' "$POLICY_DECLARED") 2>/dev/null)" || composed=""
    [ -n "$composed" ] || return 0
    PL_BORROW_WANTED="$(printf '%s\n' "$composed" | policy_counts_borrowing)" || PL_BORROW_WANTED=unknown
    [ -n "$PL_BORROW_WANTED" ] || PL_BORROW_WANTED=unknown
}

# pl_can_watch_namespaces echoes yes | no | unknown: whether ServiceAccount $2
# in $1 may WATCH every Namespace. Watch, not get: the kueue-reader role grants
# get only, so a yes comes from somewhere else -- the manager ClusterRole of a
# cluster-scoped install, or an admin's own grant -- and never from the role
# pl_grant_kueue_read is about to rewrite. "unknown" when the caller cannot
# impersonate, never folded into either answer.
pl_can_watch_namespaces() {
    local ns="$1" sa="$2" rc=0
    kc_capture auth can-i watch namespaces --as "system:serviceaccount:${ns}:${sa}" || rc=$?
    case "$KC_OUT" in
        yes*) echo yes ;;
        no*)  if [ "$rc" -ne 0 ]; then echo no; else echo unknown; fi ;;
        *)    echo unknown ;;
    esac
}

# pl_can_list_pods echoes yes | no | unknown: whether ServiceAccount $2 in $1
# may list Pods in every namespace. A namespace-scoped controller's GPU usage
# walk needs it (internal/gpunodes, WithPodReader); denied, it counts only its
# own namespace's Pods. "unknown" when the caller cannot impersonate.
pl_can_list_pods() {
    local ns="$1" sa="$2" rc=0
    kc_capture auth can-i list pods --all-namespaces --as "system:serviceaccount:${ns}:${sa}" || rc=$?
    case "$KC_OUT" in
        yes*) echo yes ;;
        no*)  if [ "$rc" -ne 0 ]; then echo no; else echo unknown; fi ;;
        *)    echo unknown ;;
    esac
}

# pl_cohort_root echoes the root of cohort $1's tree. Kueue's hierarchical
# cohorts (a Cohort object's spec.parentName, spec.parent before v1beta2) let
# ClusterQueues in sibling cohorts borrow from each other through their
# parent, so two borrowers are compared by the tree they share, not by their
# direct cohort. A cohort with no Cohort object, or whose Cohort cannot be read
# (no CRD, Forbidden), is taken as its own root: siblings under a parent that
# cannot be read are not linked, and are not warned about. At most 8 levels.
pl_cohort_root() {
    local c="$1" depth=0
    while [ "$depth" -lt 8 ]; do
        kc_capture get cohorts.kueue.x-k8s.io "$c" -o jsonpath='{.spec.parentName}{.spec.parent}' || break
        [ -n "$KC_OUT" ] && [ "$KC_OUT" != "$c" ] || break
        c="$KC_OUT"
        depth=$((depth + 1))
    done
    printf '%s' "$c"
}

# pl_cohorts_of echoes, space-separated, the root Kueue cohorts
# (pl_cohort_root) of the ClusterQueues the LocalQueues in namespace $1 point
# at: nothing when none has a cohort.
# Returns non-zero when that cannot be told -- a cluster-scoped controller
# (`*`) or an unknown one (`-`) manages more than one namespace, and a read
# that fails is not an empty answer.
pl_cohorts_of() {
    local managed="$1" cq cqs="" cohorts="" one
    case "$managed" in '*'|-|'') return 1 ;; esac
    # A controller that manages a list: the cohorts of every namespace in it.
    for one in $(printf '%s' "$managed" | tr ',' ' '); do
        kc_capture get localqueues.kueue.x-k8s.io -n "$one" \
            -o jsonpath='{range .items[*]}{.spec.clusterQueue}{"\n"}{end}' || return 1
        cqs="$cqs
$KC_OUT"
    done
    cqs="$(printf '%s\n' "$cqs" | grep -v '^$' | sort -u)"
    for cq in $cqs; do
        # cohortName in v1beta2, cohort in v1beta1: one of them is set.
        kc_capture get clusterqueues.kueue.x-k8s.io "$cq" \
            -o jsonpath='{.spec.cohortName}{.spec.cohort}' || return 1
        [ -z "$KC_OUT" ] || cohorts="$cohorts $(pl_cohort_root "$KC_OUT")"
    done
    # shellcheck disable=SC2086 # one word per cohort
    [ -z "$cohorts" ] || printf '%s\n' $cohorts | sort -u | tr '\n' ' '
}

# pl_warn_shared_cohort_borrowers warns when Kueue cohort borrowing is granted
# to more than one controller whose namespaces share a cohort. $1 holds one
# line per borrowing controller: `<namespace>/<deployment> <managed namespace>`.
#
# Each controller counts only the borrowing IT has in flight, so two of them in
# one cohort each see the same free GPUs and each orders them: the cohort is
# over-ordered, and Kueue admits one and leaves the other's pods pending. The
# documented arrangement is one borrowing controller per cohort. Named per
# cohort when the cohorts can be read; otherwise every borrower is named, with
# the condition. Warns only: countBorrowable stays on by default.
pl_warn_shared_cohort_borrowers() {
    local rows="$1" label managed cohorts c n=0 all="" unknown="" pairs="" names
    while read -r label managed; do
        [ -n "$label" ] || continue
        n=$((n + 1))
        all="${all:+$all, }$label"
        if cohorts="$(pl_cohorts_of "$managed")"; then
            for c in $cohorts; do pairs="${pairs}${c} ${label}"$'\n'; done
        else
            unknown="${unknown:+$unknown, }$label"
        fi
    done <<< "$rows"
    [ "$n" -gt 1 ] || return 0
    pairs="$(printf '%s' "$pairs" | sort -u)"
    for c in $(printf '%s\n' "$pairs" | awk 'NF { print $1 }' | uniq -d); do
        names="$(printf '%s\n' "$pairs" | awk -v c="$c" '$1 == c { print $2 }' | paste -sd, - | sed 's/,/, /g')"
        log_warning "  KUEUE COHORT ${c} IS BORROWED FROM BY MORE THAN ONE CONTROLLER: ${names}."
    done
    if [ -n "$unknown" ]; then
        log_warning "  Kueue cohort borrowing is granted to ${n} controllers (${all}), and the cohort of ${unknown} could not be read: a cluster-scoped controller, or no LocalQueue or ClusterQueue read. If two of them share a cohort, the warning below applies."
    fi
    if [ -n "$unknown" ] || printf '%s\n' "$pairs" | awk 'NF { print $1 }' | uniq -d | grep -q .; then
        log_warning "    Each controller counts only the borrowing it has in flight, so two in one cohort each spend the same free GPUs, and Kueue leaves the pods of one of them pending."
        log_warning "    Keep one borrowing controller per cohort: give the others a policy with countBorrowable: false (LLMSM_QUOTA_KUEUE_BORROW=false, in a policy namespace of their own), or put their ClusterQueues in separate cohorts."
    fi
}

# pl_kueue_borrowers_besides echoes `<namespace>/<deployment> <managed>` for
# every OTHER controller the installer granted the Kueue borrowing read
# (ClusterRoleBinding scaling-manager-kueue-borrow-reader-rolebinding-*), skipping the
# controllers in the namespaces given as arguments: the ones the caller already
# lists. <managed> is `-` when its Deployment cannot be read. Prints nothing
# when the bindings cannot be listed. A cluster-scoped controller reads
# Workloads through its manager ClusterRole and is not found here.
pl_kueue_borrowers_besides() {
    local ns rows row
    kc_capture get clusterrolebindings -o json || return 0
    for ns in $(printf '%s' "$KC_OUT" | jq -r '.items[]? | select(.metadata.name | startswith("scaling-manager-kueue-borrow-reader-rolebinding-")) | .subjects[0].namespace // empty' 2>/dev/null | sort -u); do
        case " $* " in *" $ns "*) continue ;; esac
        rows="$(printf '%s\n' "$ns" | pl_controller_targets 2>/dev/null)" || rows=""
        row="$(printf '%s\n' "$rows" | awk '$1 != "!" && NF >= 4 { print $1 "/" $2 " " $4; exit }')"
        printf '%s\n' "${row:-$ns/scaling-manager-controller-manager -}"
    done
}

# pl_grant_policy_read lets one controller READ the policy ConfigMap. Read only,
# never write: the point of putting policy in an admin-owned namespace is that its
# subject cannot change it.
pl_grant_policy_read() {
    local ns="$1" sa="$2" policy_ns="$3" suffix
    suffix="$(llmsm_ns_suffix "$ns")"
    kubectl apply -f - >/dev/null <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: scaling-manager-policy-reader
  namespace: ${policy_ns}
  labels:
    app.kubernetes.io/name: scaling-manager
rules:
- apiGroups: [""]
  resources: ["configmaps"]
  verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: scaling-manager-policy-reader-${suffix}
  namespace: ${policy_ns}
  labels:
    app.kubernetes.io/name: scaling-manager
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: scaling-manager-policy-reader
subjects:
- kind: ServiceAccount
  name: ${sa}
  namespace: ${ns}
EOF
}

# pl_grant_namespace_read grants one controller `get` on Namespaces, which it
# needs to SEE the policy namespace at all.
#
# A namespace-scoped controller holds only a Role, and a Role cannot reach a
# cluster-scoped Namespace. Without this, publishing policy into scaling-manager-policy was
# a no-op for every such controller on Kubernetes: its lookup was Forbidden, it
# fell back to its own ConfigMap, and this command still reported the cluster
# bounded. Same object names as components/policy-namespace-reader, so a later
# install or upgrade is an idempotent apply over these.
#
# Limited by resourceNames to the Namespaces the controller actually reads, each
# by name with a single `get` (ResolvePolicyNamespace: the well-known scaling-manager-policy,
# and its managed Namespace for the label; never a list or a watch): its own
# namespace, scaling-manager-policy, and the policy namespace resolved for it. Arguments
# after the second are those names. The label is read at controller start, so
# pointing scaling-manager.llmd.ai/policy-namespace somewhere else later means re-running
# `make enable-physical-limiter` (the read of the new namespace, and the
# restart that re-resolves it).
pl_grant_namespace_read() {
    local ns="$1" sa="$2" suffix names="" seen=" " n
    shift 2
    for n in "$ns" scaling-manager-policy "$@"; do
        [ -n "$n" ] || continue
        case "$seen" in *" $n "*) continue ;; esac
        seen="$seen$n "
        names="${names:+$names, }\"$n\""
    done
    suffix="$(llmsm_ns_suffix "$ns")"
    kubectl apply -f - >/dev/null <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: scaling-manager-policy-namespace-reader-role-${suffix}
  labels:
    app.kubernetes.io/name: scaling-manager
rules:
- apiGroups: [""]
  resources: ["namespaces"]
  resourceNames: [${names}]
  verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: scaling-manager-policy-namespace-reader-rolebinding-${suffix}
  labels:
    app.kubernetes.io/name: scaling-manager
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: scaling-manager-policy-namespace-reader-role-${suffix}
subjects:
- kind: ServiceAccount
  name: ${sa}
  namespace: ${ns}
EOF
}

# pl_can_read_namespace echoes yes | no | unknown: whether the controller's
# ServiceAccount may get the Namespace $3. Asked BEFORE granting, because a
# controller that could not read it resolved its policy to its own ConfigMap at
# startup and keeps doing so until it is restarted. "unknown" when the caller
# cannot impersonate -- never folded into either answer.
pl_can_read_namespace() {
    local ns="$1" sa="$2" target="$3" out rc=0
    kc_capture auth can-i get "namespaces/${target}" --as "system:serviceaccount:${ns}:${sa}" || rc=$?
    out="$KC_OUT"
    case "$out" in
        yes*) echo yes ;;
        no*)  if [ "$rc" -ne 0 ]; then echo no; else echo unknown; fi ;;
        *)    echo unknown ;;
    esac
}

# pl_grant_published_policy_read lets a controller being INSTALLED read a policy
# that was published before it existed.
#
# enable_physical_limiter grants the read to the controllers it finds; one
# installed afterwards resolved to the policy namespace, had no RoleBinding
# there, and refused to start ("refusing to start unbounded"). So the prereqs
# phase grants it too, whenever the namespace this controller will resolve to
# exists. Only a SELF-MANAGED controller (it manages the namespace it runs in)
# reads policy from another namespace; any other keeps its own ConfigMap, and
# gets nothing here.
#
# Never fatal: the install goes on, and a controller that cannot read a policy
# that exists says so at startup. Every outcome is logged.
pl_grant_published_policy_read() {
    local ns="$1" managed="$2" sa="${3:-scaling-manager-controller-manager}" target out rc=0
    [ "$managed" = "$ns" ] || return 0
    # The label wins, as in the controller. A failed read is reported, not
    # folded into "no label": the label may point somewhere else entirely.
    out="$(pl_policy_label "$managed")" || rc=$?
    if [ "$rc" -ne 0 ]; then
        log_warning "Could not read namespace $managed to find its cluster policy namespace: $out"
        log_warning "  If a cluster policy is published for it, grant ${ns}/${sa} its read: re-run this phase as a cluster admin."
        return 0
    fi
    target="${out:-$(pl_policy_ns)}"
    rc=0
    kc_capture get namespace "$target" -o name || rc=$?
    out="$KC_ERR"
    if [ "$rc" -ne 0 ]; then
        case "$out" in
            *NotFound*|*"not found"*)
                log_info "No cluster policy namespace ($target) yet; 'make enable-physical-limiter' grants this controller its read when it publishes one."
                ;;
            *)
                log_warning "Could not tell whether the cluster policy namespace $target exists: $out"
                log_warning "  If it does, ${ns}/${sa} cannot read it and the controller will refuse to start; re-run this phase as a cluster admin."
                ;;
        esac
        return 0
    fi
    if ! pl_grant_policy_read "$ns" "$sa" "$target"; then
        log_warning "Could not grant ${ns}/${sa} read of the cluster policy in $target; the controller will refuse to start until it can read it."
        return 0
    fi
    log_success "  ${ns}/${sa} may read cluster policy in $target (Role scaling-manager-policy-reader, RoleBinding scaling-manager-policy-reader-$(llmsm_ns_suffix "$ns"))"
}

# pl_set_limiter writes the limiter into the policy ConfigMap's default entry,
# preserving every other setting in it.
#
# It COMPOSES: an entry of the kind it declares (the physical limiter, or the
# quota) replaces the existing entries of that kind, and an entry of the other
# kind is kept. Every declared limiter is built and a decision must fit all of
# them, so enabling the physical limiter on a policy that already carries a
# quota bounds by both rather than dropping the quota. `none` still removes
# every limiter.
#
# `remove-<kind>` removes that kind only. When the policy declares no entry of
# that kind it writes nothing, warns, and returns 3; PL_OTHER_KEPT is set to the
# number of entries of the other kind that stay.
pl_set_limiter() {
    local policy_ns="$1" limiter="$2" current updated exists=0
    # ABSENT and FAILED are different answers, and folding them together
    # overwrote a live policy. `... 2>/dev/null || true` turned any non-zero exit
    # -- a transient API error, throttling, a partially privileged token -- into
    # an empty string, which fell through to `limiters: []` and REPLACED a policy
    # the platform team had tuned, with exit 0 and "the limiter is now in force
    # for every scaling manager on this cluster". Measured: scaleUpThreshold, scaleDownBoundary,
    # kvCacheThreshold and enableRescale all gone.
    #
    # So ask whether the ConfigMap exists FIRST, and treat a failure to answer as
    # a reason to stop rather than a reason to write.
    # NotFound, and nothing else, means "there is no policy yet".
    #
    # The first form of this guard asked `kubectl create namespace --dry-run=client`
    # as its second opinion. That is PURELY CLIENT-SIDE -- it exits 0 with no
    # cluster at all -- so the guard could never fire and every failed read still
    # fell through to `limiters: []` and overwrote the policy. Ask the error
    # instead: kubectl says NotFound when the object is absent and something else
    # when it could not tell.
    #
    # stdout and stderr apart (kc_capture): merged, a warning kubectl printed on
    # a SUCCESSFUL read became part of the policy text, and was written back.
    local probe_rc=0
    kc_capture get configmap scaling-manager-scaling-policy-config -n "$policy_ns" -o jsonpath='{.data.default}' || probe_rc=$?
    if [ "$probe_rc" -eq 0 ]; then
        exists=1
        current="$KC_OUT"
    else
        case "$KC_ERR" in
            *NotFound*|*"not found"*) : ;;   # genuinely absent: create it below
            *) log_error "could not read the policy ConfigMap in $policy_ns, and this is not a NotFound:
    ${KC_ERR:-kubectl exited $probe_rc}
    Refusing to overwrite a policy this command cannot see." ;;
        esac
    fi
    if [ -z "${current:-}" ]; then
        # A policy ConfigMap that carries ONLY limiters is complete and correct:
        # every other threshold has a default in the controller, and a partial
        # entry does not blank them.
        current="limiters: []"
    fi
    if [ "$limiter" = "none" ]; then
        # Checked. Unchecked, a yq that failed -- absent, or one that could not
        # parse the entry -- handed back an empty string, and the patch below
        # wrote default: "" over the policy: every threshold gone, under
        # "Removed the limiter".
        updated="$(printf '%s\n' "$current" | yq 'del(.limiters)')" \
            || log_error "could not remove the limiter from ${policy_ns}/scaling-manager-scaling-policy-config: yq del(.limiters) failed on its default entry. Nothing was written."
    elif [ "$limiter" = "remove-gpu-inventory" ] || [ "$limiter" = "remove-quota" ]; then
        # Remove ONE kind and keep the other, mirroring the composing enable:
        # disabling the physical limiter must not take a quota with it. When
        # nothing is left the list goes, exactly as `none` does.
        local keep kind="${limiter#remove-}" other n_kind n_all
        if [ "$limiter" = "remove-gpu-inventory" ]; then
            keep='(.limiters // []) | map(select(.type != "gpu-inventory" and .type != "inventory"))'
            other="quota"
        else
            keep='(.limiters // []) | map(select(.type != "quota"))'
            other="gpu-inventory"
        fi
        # Nothing of this kind declared: write nothing and say so, returning 3
        # ("nothing removed") for the caller to count. Reporting "Removed the
        # gpu-inventory limiter" over a policy holding only a quota is how the
        # documented undo of a quota left the quota in force.
        n_all="$(printf '%s\n' "$current" | yq '(.limiters // []) | length')" \
            || log_error "could not read the limiters declared in ${policy_ns}/scaling-manager-scaling-policy-config."
        n_kind="$(printf '%s\n' "$current" | yq "($keep) | length")" \
            || log_error "could not read the limiters declared in ${policy_ns}/scaling-manager-scaling-policy-config."
        n_kind=$((n_all - n_kind))
        # How many entries of the other kind stay, for the caller's summary.
        PL_OTHER_KEPT=$((n_all - n_kind))
        if [ "$n_kind" = "0" ]; then
            if [ "$n_all" = "0" ]; then
                log_warning "no ${kind} entry in ${policy_ns}/scaling-manager-scaling-policy-config; nothing removed; it declares no limiter."
            else
                log_warning "no ${kind} entry in ${policy_ns}/scaling-manager-scaling-policy-config; nothing removed; the ${other} limiter stays. To remove it: LLMSM_LIMITER_TYPE=${other}."
            fi
            return 3
        fi
        updated="$(printf '%s\n' "$current" | yq ".limiters = ($keep)")" \
            || log_error "could not remove the ${limiter#remove-} limiter from the policy in ${policy_ns}."
        if [ "$(printf '%s\n' "$updated" | yq '.limiters | length')" = "0" ]; then
            # Checked, as `none` is: an unchecked failure wrote an empty entry.
            updated="$(printf '%s\n' "$updated" | yq 'del(.limiters)')" \
                || log_error "could not remove the ${kind} limiter from ${policy_ns}/scaling-manager-scaling-policy-config: yq del(.limiters) failed on its default entry. Nothing was written."
        fi
    else
        # Through limiter_entry_yaml, the same builder the installer uses.
        #
        # This path wrote `[{"type": "quota"}]` of its own, which the controller
        # REJECTS ("name must not be empty") -- and a rejected entry costs the
        # whole `default` policy, so publishing it would have stripped the policy
        # from every scaling manager on the cluster and left them all unbounded, under the
        # banner "The quota limiter is now in force for every scaling manager on this
        # cluster." Exactly the claim a safety bound must never make falsely.
        # One call, and no status to forget. This path runs from a bare
        # `bash -c` in the Makefile with no `set -e`, so an unchecked failure
        # used to return 0 up the chain and enable_physical_limiter went on to
        # announce a limiter it had not written -- after blanking the policy in
        # every target namespace. policy_declared_limiters composes in THIS
        # shell, so a refusal ends the command rather than handing back an empty
        # string.
        #
        # The `default` third argument is not a detail. This ONE policy is
        # read by every controller on the cluster, across every namespace, so a
        # namespace-scoped budget here has to use the reserved per-unlisted-
        # namespace key. Keyed on a single namespace instead -- which is what the
        # installer's own rule would pick -- that namespace gets the budget and
        # every other one gets ZERO, so this command would stop scaling
        # everywhere it was meant to bound it.
        policy_declared_limiters "$limiter" "$current" default compose
        updated="$POLICY_DECLARED"
    fi
    # An entry that is now empty is removed, not written as "{}". A ConfigMap
    # whose default entry is an empty object is a policy that says nothing while
    # looking like one that says something, and the next reader has to open it to
    # find out which.
    if { [ "$limiter" = "none" ] || [ "${limiter#remove-}" != "$limiter" ]; } \
        && [ "$(printf '%s' "$updated" | tr -d '[:space:]')" = "{}" ]; then
        # The `default` ENTRY, not the ConfigMap. Deleting the object took every
        # other entry in it -- the per-model and named-tier policies an operator
        # added -- which is the same data loss this file was just fixed for, on
        # the disable path. A null value in a merge patch removes one key.
        # Checked: `|| true` here let disable report "Removed the limiter" over
        # a denied patch that removed nothing.
        if [ "$exists" -eq 1 ]; then
            local perr
            perr="$(kubectl patch configmap scaling-manager-scaling-policy-config -n "$policy_ns" --type=merge \
                -p '{"data":{"default":null}}' 2>&1 >/dev/null)" \
                || log_error "could not remove the limiter from ${policy_ns}/scaling-manager-scaling-policy-config (patching data.default to null failed): ${perr}"
        fi
        return 0
    fi
    # PATCH an existing ConfigMap; only CREATE a missing one.
    #
    # The create|apply pipeline below renders a manifest containing ONLY
    # data.default, and `kubectl apply`'s three-way merge deletes every key that
    # is in last-applied-configuration and not in the new manifest. Measured
    # against a real apiserver: a ConfigMap carrying a per-model override entry
    # beside `default` -- the shape config/base/manager/scaling-policy-configmap.yaml
    # documents -- came back with the override GONE, exit 0, "the limiter is now
    # in force for every scaling manager on this cluster". It only bites when the ConfigMap
    # was last managed by `kubectl apply -f` with those keys present, which is
    # the declarative shape and how the installer itself places it.
    #
    # A merge patch names one key and leaves the rest alone, which is what the
    # installer's own path has always done.
    #
    # Never an empty entry: whatever produced it failed, and default: "" is a
    # policy with every setting removed.
    if [ -z "$(printf '%s' "$updated" | tr -d '[:space:]')" ]; then
        log_error "refusing to write an empty default entry into ${policy_ns}/scaling-manager-scaling-policy-config (the new policy came out empty, so a step before this failed). Nothing was written."
    fi
    if [ "$exists" -eq 1 ]; then
        # Checked. Unchecked with a bare `return 0` after it, a denied
        # configmaps/patch still reported "the limiter is now in force for every
        # the scaling manager on this cluster" -- narrower than the pipeline it replaced, which
        # at least propagated its own status.
        kubectl patch configmap scaling-manager-scaling-policy-config -n "$policy_ns" --type=merge \
            -p "$(jq -n --arg d "$updated" '{metadata:{labels:{"app.kubernetes.io/name":"scaling-manager"}},data:{"default":$d}}')" >/dev/null \
            || log_error "could not write the policy into ${policy_ns}/scaling-manager-scaling-policy-config."
        return 0
    fi
    # Every stage's status, not only the last one's. The Makefile runs this from
    # a bare `bash -c` (no pipefail, no set -e), so the pipeline's own exit is
    # apply's alone and was never even read: a denied create or apply went on to
    # "the limiter is now in force".
    kubectl create configmap scaling-manager-scaling-policy-config -n "$policy_ns" \
        --from-literal=default="$updated" --dry-run=client -o yaml \
        | kubectl label --local -f - app.kubernetes.io/name=scaling-manager -o yaml \
        | kubectl apply -f - >/dev/null
    local st=("${PIPESTATUS[@]}")
    if [ "${st[0]}" -ne 0 ] || [ "${st[1]}" -ne 0 ] || [ "${st[2]}" -ne 0 ]; then
        log_error "could not create the policy ConfigMap ${policy_ns}/scaling-manager-scaling-policy-config (create/label/apply exit ${st[0]}/${st[1]}/${st[2]})."
    fi
}

# pl_policy_is_live reports whether a controller managing $1 (`*`
# cluster-scoped, `-` unknown) sees an edit to the policy in namespace $2
# without a restart.
#
# cmd/main.go: a namespace-scoped controller caches ConfigMaps in the namespace
# it manages ONLY -- the policy namespace is deliberately not added, because an
# informer that cannot list it never syncs. So a policy anywhere else is read at
# startup and not again. A cluster-scoped controller caches every namespace's
# ConfigMaps (the policy namespace without the label filter) and reloads live.
# Unknown is answered "live": nothing to restart on a guess.
pl_policy_is_live() {
    case "$1" in
        '*'|-|'') return 0 ;;
    esac
    # A controller that manages a list caches its own namespace too
    # (cmd/main.go cacheNamespaces), so a policy there, or in any namespace it
    # manages, is live. $3, when given, is the controller's own namespace.
    case ",$1," in *",$2,"*) return 0 ;; esac
    case "$1" in
        *,*) [ -n "${3:-}" ] && [ "$2" = "$3" ] ;;
        *) return 1 ;;
    esac
}

# pl_ensure_namespace creates Namespace $1 if it is absent, and ends the
# command with an error naming it when that fails.
pl_ensure_namespace() {
    local manifest err
    # Streams apart (kc_capture): a notice on stderr is not part of the manifest.
    kc_capture create namespace "$1" --dry-run=client -o yaml \
        || log_error "could not render Namespace $1: $(kc_err_line "$KC_ERR"). Nothing was published."
    manifest="$KC_OUT"
    err="$(printf '%s\n' "$manifest" | kubectl apply -f - 2>&1 >/dev/null)" \
        || log_error "could not create Namespace $1, where the policy is to be published: ${err}. Nothing was published."
}

# pl_target_label echoes how a target is named in messages: <ns>/<deployment>,
# or <ns> alone when no Deployment was found there.
pl_target_label() {
    if [ "$2" = "-" ]; then echo "$1"; else echo "$1/$2"; fi
}

# pl_split_targets <pl_controller_targets output> sets PL_GRANT (the verified
# rows) and PL_SKIP (the skipped ones, `! ` dropped:
# `<namespace> <deployment> <managed> <reason...>`).
pl_split_targets() {
    PL_GRANT="$(printf '%s\n' "$1" | grep -v '^!' | grep . || true)"
    PL_SKIP="$(printf '%s\n' "$1" | grep '^! ' | sed 's/^! //' || true)"
}

# pl_declared_limiters echoes how many limiters the policy ConfigMap in $1
# declares: 0 when there is no ConfigMap. RETURNS non-zero when it cannot tell.
#
# Streams apart (kc_capture): a warning on stderr is not part of the policy, and
# merged it made yq fail on a policy that parses -- "cannot tell" for a read
# that succeeded. And only a number is an answer: a yq that printed nothing, or
# something else, is "cannot tell" too, never 0.
pl_declared_limiters() {
    local rc=0 n
    kc_capture get configmap scaling-manager-scaling-policy-config -n "$1" -o jsonpath='{.data.default}' || rc=$?
    if [ "$rc" -ne 0 ]; then
        case "$KC_ERR" in
            *NotFound*|*"not found"*) echo 0; return 0 ;;
            *) return 1 ;;
        esac
    fi
    n="$(printf '%s\n' "$KC_OUT" | yq '(.limiters // []) | length' 2>/dev/null)" || return 1
    case "$n" in
        ''|*[!0-9]*) return 1 ;;
    esac
    echo "$n"
}

# pl_require_tools ends the command, naming the tool, when kubectl, jq or yq is
# missing. Enable and disable both write the policy with jq and yq.
pl_require_tools() {
    local tool
    for tool in kubectl jq yq; do
        command -v "$tool" >/dev/null 2>&1 || \
            log_error "$tool is required by this command and is not on PATH."
    done
}

enable_physical_limiter() {
    local limiter="${LLMSM_LIMITER_TYPE:-gpu-inventory}"
    # This target does not go through check_prerequisites -- only
    # deploy/install.sh does -- and the write path now builds its patch with jq.
    # Absent, `-p ""` reaches kubectl, the patch is rejected, and with no `set -e`
    # on this path the command still announced the limiter in force.
    pl_require_tools
    pl_refuse_policy_ns_override
    local policy_ns; policy_ns="$(pl_policy_ns)"
    local namespaces ns sa granted=0

    case "$limiter" in
        gpu-inventory|quota) ;;
        *) log_error "LLMSM_LIMITER_TYPE must be gpu-inventory or quota (got '$limiter')" ;;
    esac
    # Validated HERE, before the namespace is created and the grants are made.
    # pl_set_limiter is the last step of a sequence that has already changed the
    # cluster, so a budget this rejects must stop the command while it still has
    # changed nothing. Discards the output: this call is for its refusals, and
    # every one of them is a log_error, which exits the command directly -- this
    # is not a command substitution, so there is no subshell to swallow it.
    # `default` is the key pl_set_limiter will use, so what is validated here is
    # what gets written.
    limiter_entry_yaml "$limiter" default >/dev/null
    # Both write paths warn about a budget keyed on an accelerator no node has.
    [ "$limiter" = "quota" ] && warn_unadvertised_accelerators

    if ! kubectl auth can-i create clusterrolebindings >/dev/null 2>&1; then
        log_error "This is a cluster-admin action: it publishes policy a tenant cannot edit, and grants the node read that policy then requires. You cannot create ClusterRoleBindings on this cluster."
    fi

    # The STATUS, checked here, because it is the only thing that crosses a
    # command substitution: pl_controller_namespaces cannot refuse on its own.
    if ! namespaces="$(pl_controller_namespaces)"; then
        log_error "could not list the scaling manager controllers on this cluster, so this command cannot know which ones it would leave unable to honour the policy it is about to publish. Refusing to announce a bound it cannot deliver.
    Grant the listing, or name the targets explicitly: LLMSM_LIMITER_TARGETS='ns-a ns-b'"
    fi
    if [ -z "$namespaces" ]; then
        log_warning "No scaling manager controller found on this cluster. Publishing the policy anyway — any scaling manager installed later reads it at startup."
    fi
    # Only Deployments that pass verification are granted; the rest are named as
    # skipped. The status, checked, for the same subshell reason.
    local all deploy managed sel
    if ! all="$(printf '%s\n' "$namespaces" | pl_controller_targets)"; then
        log_error "could not read the scaling manager controller Deployments, so this command cannot tell which ones are genuine. Refusing to grant to what it cannot verify."
    fi
    pl_split_targets "$all"
    if [ -n "$namespaces" ] && [ -z "$PL_GRANT" ]; then
        log_warning "No labelled Deployment passed verification, so no controller is granted anything."
    fi

    # The PLAN, settled before anything changes: where each controller reads
    # policy, and whether it must be restarted to read it. A Namespace that
    # cannot be read stops the command here rather than half-way through the
    # grants. Asked before the grant, because a controller that could not SEE the
    # namespace it reads policy through resolved to its own ConfigMap at startup,
    # and a grant alone does not change that -- it needs a restart too.
    local plan="" res cm_ns src ns_read restart n_restart=0 note
    # fd 3, so nothing in the body can read the list off stdin.
    while read -r ns deploy sa managed sel <&3; do
        [ -n "$ns" ] || continue
        if ! res="$(pl_policy_resolution "$ns" "$managed")"; then
            log_error "could not read Namespace $ns, so this command cannot tell whether a scaling-manager.llmd.ai/policy-namespace label points the controller in it at another policy namespace. Refusing to publish where it may not be read."
        fi
        cm_ns="${res%% *}"; src="${res#* }"
        restart=no
        if [ "$src" != own ] && [ "$deploy" != "-" ]; then
            # The label is read off the managed Namespace; the well-known
            # namespace is looked up by name.
            if [ "$src" = label ]; then
                ns_read="$(pl_can_read_namespace "$ns" "$sa" "$ns")"
            else
                ns_read="$(pl_can_read_namespace "$ns" "$sa" "$cm_ns")"
            fi
            if [ "$ns_read" = no ]; then
                restart=cannot-read
            elif [ "$src" = wellknown ] && pl_needs_restart "$ns" "$sel" "$cm_ns"; then
                restart=started-before
            elif [ "$ns_read" = unknown ]; then
                restart=unknown
            fi
        fi
        # And a controller that does not WATCH the namespace it reads policy
        # from sees the edit this command is about to make only at its next
        # start (pl_policy_is_live), so it is restarted after the write.
        case "$restart" in
            no|unknown) pl_policy_is_live "$managed" "$cm_ns" "$ns" || restart=not-watched ;;
        esac
        plan="${plan}${ns} ${deploy} ${sa} ${cm_ns} ${src} ${restart} ${managed}"$'\n'
    done 3<<< "$PL_GRANT"

    if [ -n "$plan" ]; then
        log_info "Controllers to be granted (namespace/Deployment, ServiceAccount, where it reads policy):"
        log_info "  Verified by signals, not proof: any tenant can satisfy them. Where tenants are not trusted, see LLMSM_LIMITER_TARGETS in docs/guides/admin-gpu-bounding."
        while read -r ns deploy sa cm_ns src restart managed <&3; do
            [ -n "$ns" ] || continue
            case "$restart" in
                cannot-read)
                    note="; WILL BE RESTARTED: it cannot read the Namespace it finds its policy through, so it is on its own ConfigMap"
                    n_restart=$((n_restart + 1)) ;;
                started-before)
                    note="; WILL BE RESTARTED: it started before $cm_ns existed"
                    n_restart=$((n_restart + 1)) ;;
                not-watched)
                    note="; WILL BE RESTARTED: it does not watch $cm_ns, so it reads the policy there only at startup"
                    n_restart=$((n_restart + 1)) ;;
                unknown) note="; may need a restart (could not impersonate it to ask)" ;;
                *) note="" ;;
            esac
            log_info "  $ns/$deploy as ServiceAccount $sa; reads policy from $cm_ns$note"
        done 3<<< "$plan"
        if [ "$n_restart" -gt 0 ]; then
            log_warning "  $n_restart controller(s) above will be restarted (kubectl rollout restart) after the policy is written: a namespace-scoped controller resolves where its policy lives once, at startup, and does not watch a policy namespace other than the one it manages, so it reads a change there only when it starts. On the first enable after upgrading to a release with the Namespace read grant, that is every self-managed namespace-scoped controller -- and it remains true on every later enable or disable."
        fi
    fi

    log_info "Publishing cluster policy in $policy_ns (limiter: $limiter)"
    # Each step's status checked: this runs from a bare `bash -c` with no
    # `set -e`, and an unchecked failure here went on to "now in force".
    pl_ensure_namespace "$policy_ns"
    # And every namespace a scaling-manager.llmd.ai/policy-namespace label points at: the
    # controller reads it whether or not it exists, so the Role, the binding and
    # the policy below need it to.
    local made=" $policy_ns "
    while read -r ns deploy sa cm_ns src restart managed <&3; do
        [ "$src" = label ] || continue
        case "$made" in *" $cm_ns "*) continue ;; esac
        made="$made$cm_ns "
        pl_ensure_namespace "$cm_ns"
    done 3<<< "$plan"

    # Grants BEFORE the policy. The other order publishes a limiter that every
    # controller without node access must refuse — a self-inflicted outage across
    # the cluster, arriving as NotReady pods rather than as an error from this
    # command.
    local targets="" restart_list="" bound="" pods_blind="" pods_unknown="" borrowers="" run_ns=""
    while read -r ns deploy sa cm_ns src restart managed <&3; do
        [ -n "$ns" ] || continue
        if [ "$limiter" = "gpu-inventory" ]; then
            pl_grant_node_read "$ns" "$sa" \
                || log_error "could not grant $ns/$sa the node read (ClusterRole/ClusterRoleBinding scaling-manager-node-reader-*-$(llmsm_ns_suffix "$ns")). Nothing was published; the grants made before this one stay in place."
            # A namespace-scoped controller counts GPU usage from a Pod list.
            # Without list on pods cluster-wide -- this command does not grant
            # it -- it counts only its own namespace's Pods.
            if [ "$managed" != '*' ]; then
                case "$(pl_can_list_pods "$ns" "$sa")" in
                    yes) ;;
                    no) pods_blind="${pods_blind:+$pods_blind, }$ns/$deploy" ;;
                    *) pods_unknown="${pods_unknown:+$pods_unknown, }$ns/$deploy" ;;
                esac
            fi
        fi
        if [ "$limiter" = "quota" ]; then
            # What the policy in cm_ns will say once this quota is composed in:
            # the only policy the controller reads its limiters from.
            pl_published_kueue "$cm_ns" "$limiter"
            [ "$PL_BORROW_WANTED" = yes ] && borrowers="${borrowers}${ns}/${deploy} ${managed}"$'\n'
            run_ns="$run_ns $ns"
        fi
        if [ "$limiter" = "quota" ] && [ "${LLMSM_QUOTA_KUEUE:-false}" = "true" ]; then
            pl_grant_kueue_read "$ns" "$sa" "$managed" \
                || log_error "could not grant $ns/$sa the Kueue read (ClusterRole/ClusterRoleBinding scaling-manager-kueue-reader-*-$(llmsm_ns_suffix "$ns")). Nothing was published; the grants made before this one stay in place."
            pl_grant_kueue_controller_read "$ns" "$sa" "$PL_KUEUE_KEPT_NS" \
                || log_error "could not grant $ns/$sa the Kueue controller read (Role/RoleBinding scaling-manager-kueue-controller-reader-$(llmsm_ns_suffix "$ns") in ${KUEUE_NAMESPACE:-${PL_KUEUE_KEPT_NS:-kueue-system}}). Nothing was published; the grants made before this one stay in place."
        fi
        if [ "$limiter" = "quota" ]; then
            # The cluster-wide Workload read, only while the published quota
            # counts borrowing: LLMSM_QUOTA_KUEUE_BORROW=false, a kept
            # countBorrowable: false, or a quota without Kueue revokes it.
            pl_sync_kueue_borrow_read "$ns" "$sa" "$PL_BORROW_WANTED" "${cm_ns}/scaling-manager-scaling-policy-config unreadable" \
                || log_error "could not change $ns/$sa's Kueue borrowing read (ClusterRole/ClusterRoleBinding scaling-manager-kueue-borrow-reader-*-$(llmsm_ns_suffix "$ns")). Nothing was published; the grants made before this one stay in place."
        fi
        if [ "$src" != own ]; then
            pl_grant_namespace_read "$ns" "$sa" "$cm_ns" \
                || log_error "could not grant $ns/$sa the Namespace read it finds its policy through (scaling-manager-policy-namespace-reader-*-$(llmsm_ns_suffix "$ns")). Nothing was published."
            pl_grant_policy_read "$ns" "$sa" "$cm_ns" \
                || log_error "could not grant $ns/$sa read of the policy in $cm_ns (Role scaling-manager-policy-reader, RoleBinding scaling-manager-policy-reader-$(llmsm_ns_suffix "$ns")). Nothing was published."
        fi
        case "$restart" in
            cannot-read|started-before|not-watched) restart_list="$restart_list $ns/$deploy" ;;
            unknown) log_warning "  Could not tell whether $ns/$sa could already read the Namespace it finds its policy through (impersonation was not allowed). If its controller logs 'Cannot tell whether a cluster admin published policy', restart it." ;;
        esac
        case " $targets " in *" $cm_ns "*) : ;; *) targets="$targets $cm_ns" ;; esac
        log_success "  $ns/$sa — reads policy from ${cm_ns}$([ "$limiter" = "gpu-inventory" ] && echo ', and may list nodes')$([ "$limiter" = "quota" ] && [ "${LLMSM_QUOTA_KUEUE:-false}" = "true" ] && echo ', and may read Kueue')"
        granted=$((granted + 1))
        bound="${bound:+$bound, }$ns/$deploy"
    done 3<<< "$plan"

    # The well-known namespace too, matching what disable already clears — and it
    # is the ONLY target when the cluster has no controller yet. Without it, this
    # command created the policy namespace, wrote nothing into it, and reported
    # the limiter in force: the exact claim a safety bound must never make
    # falsely. A scaling manager installed the next day would have read an empty namespace and
    # scaled with no GPU budget at all.
    case " $targets " in *" $policy_ns "*) : ;; *) targets="$targets $policy_ns" ;; esac

    # Every ConfigMap some controller actually reads, not just the well-known one.
    for cm_ns in $targets; do
        pl_set_limiter "$cm_ns" "$limiter"
    done

    # A controller that resolved its policy source before it could see it reads
    # its OWN namespace and will never look again, and one that does not watch
    # its policy namespace reads the new policy only when it starts. Without
    # this the command reports success and changes nothing for either. The named
    # Deployment, not the label: the label also selects the Deployments skipped
    # above. A failed restart is a controller this did NOT bind, so it ends the
    # command with an error instead of a banner.
    local entry not_restarted=""
    for entry in $restart_list; do
        ns="${entry%%/*}"; deploy="${entry#*/}"
        log_info "  Restarting $ns/$deploy: it reads its policy source only at startup"
        if ! kubectl rollout restart deploy "$deploy" -n "$ns" >/dev/null 2>&1; then
            log_warning "    could not restart $ns/$deploy"
            not_restarted="${not_restarted:+$not_restarted, }$ns/$deploy"
        fi
    done
    if [ -n "$not_restarted" ]; then
        log_error "the ${limiter} limiter is written (policy in: ${targets# }), but these controllers could not be restarted and do NOT read it yet: ${not_restarted}. Restart them (kubectl rollout restart deploy <name> -n <namespace>) before relying on the bound."
    fi

    # The banner names WHO is bound. "In force for every scaling manager on this cluster" was
    # printed while a controller from a mirror sat in the skipped list, reading
    # its own unbounded ConfigMap.
    echo ""
    if [ "$granted" -gt 0 ]; then
        log_success "The ${limiter} limiter is now in force for the ${granted} controller(s) granted above: ${bound}."
    else
        log_success "The ${limiter} limiter is published in ${policy_ns}. No running controller was granted, so it is in force for none yet; a scaling manager installed later reads it at startup."
    fi
    if [ -n "$PL_SKIP" ]; then
        log_warning "  NOT bound by this command -- labelled Deployments it skipped, and named namespaces with no controller; granted nothing:"
        while read -r ns deploy managed note <&3; do
            [ -n "$ns" ] || continue
            log_warning "    $(pl_target_label "$ns" "$deploy"): $note"
        done 3<<< "$PL_SKIP"
        log_warning "  A skipped controller keeps whatever policy it read before. One that can see a policy namespace it has no read grant for refuses to start at its next restart. If one is genuine, name its namespace in LLMSM_LIMITER_TARGETS and re-run."
    fi
    # After the banner, so it does not scroll away above it: a bound these
    # controllers hold only against their own namespace's GPUs.
    if [ -n "$pods_blind" ] || [ -n "$pods_unknown" ]; then
        if [ -n "$pods_blind" ]; then
            log_warning "  GPU USAGE NOT SEEN ACROSS NAMESPACES: these namespace-scoped controllers cannot list Pods cluster-wide: ${pods_blind}."
        fi
        if [ -n "$pods_unknown" ]; then
            log_warning "  Could not check (impersonation was not allowed) whether these namespace-scoped controllers may list Pods cluster-wide: ${pods_unknown}."
        fi
        log_warning "    Without it the gpu-inventory limiter counts only the GPUs its own namespace holds: if other namespaces run GPU pods on the same nodes, their GPUs read as FREE and scale-ups are admitted onto full nodes; if none do, nothing is miscounted. This command grants node read, not that."
        log_warning "    Each such controller says so at runtime: llmsm_pod_access_denied=1, and podListDenied in its scaling-manager-controller-status ConfigMap."
        log_warning "    Remedy: grant its ServiceAccount list on pods in every namespace, or bound it with a quota limiter instead (LLMSM_LIMITER_TYPE=quota) -- a quota beside gpu-inventory leaves the physical limiter reading the same undercount."
    fi
    # Borrowers granted by earlier runs too: onboarding one tenant per run, the
    # run alone never holds two.
    if [ -n "$borrowers" ]; then
        # shellcheck disable=SC2086 # one word per namespace
        borrowers="${borrowers}$(pl_kueue_borrowers_besides $run_ns)"
    fi
    pl_warn_shared_cohort_borrowers "$borrowers"
    log_info "  Policy in:   $(echo $targets | tr ' ' ',') (ConfigMap scaling-manager-scaling-policy-config, data.default.limiters)"
    log_info "  Controllers: ${granted} granted, $(printf '%s' "$PL_SKIP" | grep -c . || true) skipped"
    if [ -n "${LLMSM_LIMITER_TARGETS:-}" ]; then
        log_warning "  LLMSM_LIMITER_TARGETS was set, so only those namespaces were granted. Any OTHER controller on this cluster that reads this policy cannot honour it, and will report NotReady until it is granted too."
    fi
    log_info "  A cluster-scoped controller reloads policy live. A namespace-scoped one reads a policy outside the namespace it manages only at startup; this command restarted the granted ones that needed it, and any later edit to that policy needs a restart too. A granted controller that cannot serve it reports NotReady."
    log_info "  Check:       kubectl get pods -A -l app.kubernetes.io/name=scaling-manager"
    log_info "               a controller that stays NotReady is one this did not reach."
}

disable_physical_limiter() {
    # The same tools enable asks for: this path rewrites the policy with yq and
    # builds the patch with jq, and without them it wrote an empty entry.
    pl_require_tools
    # The kind to remove: the one enable published (LLMSM_LIMITER_TYPE). Only
    # that kind goes; a limiter of the other kind in the same policy stays.
    # `all` removes every limiter, whatever its kind: the uninstall form, and
    # the one a script that must not care what was declared should use.
    #
    # Exit status: 0 when something was removed, and 0 -- with a warning --
    # when no policy this command reads declares any limiter. Non-zero only
    # when nothing was removed while a limiter of the OTHER kind stays, the
    # "undo my quota without LLMSM_LIMITER_TYPE=quota" mistake. Through make that
    # reads `make: *** [...] Error 1`, and make itself exits 2.
    local limiter="${LLMSM_LIMITER_TYPE:-gpu-inventory}"
    case "$limiter" in
        gpu-inventory|quota|all) ;;
        *) log_error "LLMSM_LIMITER_TYPE must be gpu-inventory, quota or all (got '$limiter')" ;;
    esac
    local other="quota"
    case "$limiter" in
        quota) other="gpu-inventory" ;;
        all) other="" ;;
    esac
    pl_refuse_policy_ns_override
    local policy_ns; policy_ns="$(pl_policy_ns)"
    local ns cm_ns res targets="" cleared=0 cleared_ns=" " to_restart="" set_rc other_left="" reads_from=""

    # The SAME per-controller resolution enable uses. Looking only at the
    # well-known namespace left the limiter in force everywhere it had actually
    # been written — an "off" that reported success and disabled nothing, which
    # is the worse direction for a command whose subject is a safety bound.
    local disable_namespaces
    if ! disable_namespaces="$(pl_controller_namespaces)"; then
        log_error "could not list the scaling manager controllers, so this command cannot know whose policy to clear. Refusing to report a limiter removed from controllers it never reached.
    Grant the listing, or name the targets explicitly: LLMSM_LIMITER_TARGETS='ns-a ns-b'"
    fi
    # The same verification enable uses: an unverified Deployment must not decide
    # where this command writes either -- but it is reported below, never dropped.
    local all deploy sa managed sel n note
    if ! all="$(printf '%s\n' "$disable_namespaces" | pl_controller_targets)"; then
        log_error "could not read the scaling manager controller Deployments, so this command cannot tell whose policy to clear. Refusing to report a limiter removed from controllers it never reached."
    fi
    pl_split_targets "$all"
    while read -r ns deploy sa managed sel <&3; do
        [ -n "$ns" ] || continue
        if ! res="$(pl_policy_resolution "$ns" "$managed")"; then
            log_error "could not read Namespace $ns, so this command cannot tell which policy namespace its controller reads. Refusing to report a limiter removed from a policy it may not have found."
        fi
        cm_ns="${res%% *}"
        case " $targets " in *" $cm_ns "*) : ;; *) targets="$targets $cm_ns" ;; esac
        # One that does not watch its policy namespace sees the removal only
        # when it restarts (pl_policy_is_live).
        pl_policy_is_live "$managed" "$cm_ns" "$ns" || to_restart="$to_restart $ns/$deploy/$cm_ns"
        reads_from="$reads_from $ns/$cm_ns"
    done 3<<< "$PL_GRANT"
    # The well-known namespace too, even with no controller reading it today: a
    # scaling manager installed tomorrow would read whatever was left there.
    case " $targets " in *" $policy_ns "*) : ;; *) targets="$targets $policy_ns" ;; esac

    local probe_rc declared
    for cm_ns in $targets; do
        # NotFound is "nothing to disable here"; anything else is a policy this
        # command could not see, and reporting it as absent is how an "off"
        # left a limiter in force.
        probe_rc=0
        kc_capture get configmap scaling-manager-scaling-policy-config -n "$cm_ns" -o name || probe_rc=$?
        if [ "$probe_rc" -ne 0 ]; then
            case "$KC_ERR" in
                *NotFound*|*"not found"*) continue ;;
            esac
            log_error "could not read ${cm_ns}/scaling-manager-scaling-policy-config, and this is not a NotFound: ${KC_ERR:-kubectl exited $probe_rc}
    Refusing to report a limiter removed from a policy this command cannot see."
        fi
        # Counted FIRST. A policy that declares no limiter has nothing to
        # remove, and writing it anyway rewrote the entry, restarted every
        # controller reading it, and announced "Removed the limiter" and
        # "bounded by nothing" for a cluster that was never bounded.
        if ! declared="$(pl_declared_limiters "$cm_ns")" || [ -z "$declared" ]; then
            log_error "could not tell how many limiters ${cm_ns}/scaling-manager-scaling-policy-config declares (its default entry could not be read or parsed with yq). Nothing was written there.
    Refusing to report a limiter removed from a policy this command cannot read."
        fi
        if [ "$declared" -eq 0 ]; then
            log_info "No limiter declared in ${cm_ns}/scaling-manager-scaling-policy-config; nothing to remove."
            continue
        fi
        if [ "$limiter" = "all" ]; then
            # Every kind: the same removal as LLMSM_LIMITER=none at install.
            pl_set_limiter "$cm_ns" none
            log_success "Removed every limiter from ${cm_ns}/scaling-manager-scaling-policy-config"
            cleared=$((cleared + 1))
            cleared_ns="$cleared_ns$cm_ns "
            continue
        fi
        set_rc=0
        PL_OTHER_KEPT=0
        pl_set_limiter "$cm_ns" "remove-$limiter" || set_rc=$?
        if [ "$set_rc" -eq 3 ]; then
            # Nothing of this kind there (pl_set_limiter said so). Note a policy
            # still bounded by the other kind: that decides the exit status below.
            [ "$PL_OTHER_KEPT" -gt 0 ] && other_left="$other_left $cm_ns"
            continue
        elif [ "$set_rc" -ne 0 ]; then
            log_error "could not remove the $limiter limiter from ${cm_ns}/scaling-manager-scaling-policy-config."
        fi
        if [ "$PL_OTHER_KEPT" -gt 0 ]; then
            log_success "Removed the $limiter limiter from ${cm_ns}/scaling-manager-scaling-policy-config; the $other limiter declared there stays"
            other_left="$other_left $cm_ns"
        else
            log_success "Removed the $limiter limiter from ${cm_ns}/scaling-manager-scaling-policy-config"
        fi
        cleared=$((cleared + 1))
        cleared_ns="$cleared_ns$cm_ns "
    done

    # The Kueue controller read (pl_grant_kueue_controller_read) and the
    # cluster-wide borrowing read (pl_grant_kueue_borrow_read) go with the
    # quota: a controller whose policy no longer declares one reads no Kueue,
    # and nothing else revokes either. The kueue-reader role stays: it reads
    # cluster-scoped quota objects only.
    local entry r_ns r_deploy r_cm not_restarted="" revoke_err
    if [ "$limiter" = "quota" ] || [ "$limiter" = "all" ]; then
        for entry in $reads_from; do
            r_ns="${entry%%/*}"; r_cm="${entry#*/}"
            case "$cleared_ns" in *" $r_cm "*) ;; *) continue ;; esac
            if revoke_err="$(pl_revoke_kueue_controller_read "$r_ns")"; then
                log_info "  Revoked the Kueue controller read of $r_ns (Role/RoleBinding scaling-manager-kueue-controller-reader-$(llmsm_ns_suffix "$r_ns"))"
            else
                log_warning "  Could not revoke the Kueue controller read of $r_ns: $revoke_err"
            fi
            if revoke_err="$(pl_revoke_kueue_borrow_read "$r_ns")"; then
                log_info "  Revoked the Kueue borrowing read of $r_ns (ClusterRole/ClusterRoleBinding scaling-manager-kueue-borrow-reader-*-$(llmsm_ns_suffix "$r_ns"))"
            else
                log_warning "  Could not revoke the Kueue borrowing read of $r_ns: $revoke_err"
            fi
        done
    fi
    for entry in $to_restart; do
        r_ns="${entry%%/*}"; r_cm="${entry##*/}"; r_deploy="${entry#*/}"; r_deploy="${r_deploy%/*}"
        case "$cleared_ns" in *" $r_cm "*) ;; *) continue ;; esac
        log_info "  Restarting $r_ns/$r_deploy: it reads its policy in $r_cm only at startup"
        if ! kubectl rollout restart deploy "$r_deploy" -n "$r_ns" >/dev/null 2>&1; then
            not_restarted="${not_restarted:+$not_restarted, }$r_ns/$r_deploy"
        fi
    done
    if [ -n "$not_restarted" ]; then
        log_error "the limiter is removed from the policy, but these controllers could not be restarted and still run with it: ${not_restarted}. Restart them (kubectl rollout restart deploy <name> -n <namespace>)."
    fi

    # The skipped ones, each with what is left in the policy it reads. Silence
    # here meant a limiter still in force in a mirror-built controller's own
    # ConfigMap under an "off" that looked complete.
    local left=0
    if [ -n "$PL_SKIP" ]; then
        log_warning "Labelled Deployments skipped by verification, and named namespaces with no controller -- this command did not write their policy:"
        while read -r ns deploy managed note <&3; do
            [ -n "$ns" ] || continue
            if ! res="$(pl_policy_resolution "$ns" "$managed")"; then
                log_warning "  $(pl_target_label "$ns" "$deploy"): $note. Could not read Namespace $ns to tell where it reads policy; check it by hand."
                continue
            fi
            cm_ns="${res%% *}"
            case " $targets " in
                *" $cm_ns "*)
                    log_warning "  $(pl_target_label "$ns" "$deploy"): $note. It reads policy from $cm_ns, cleared above."
                    continue ;;
            esac
            if ! n="$(pl_declared_limiters "$cm_ns")" || [ -z "$n" ]; then
                log_warning "  $(pl_target_label "$ns" "$deploy"): $note. Could not read ${cm_ns}/scaling-manager-scaling-policy-config; check it by hand."
                continue
            fi
            if [ "$n" -gt 0 ]; then
                log_warning "  $(pl_target_label "$ns" "$deploy"): $note. Its policy in ${cm_ns}/scaling-manager-scaling-policy-config STILL declares $n limiter(s): NOT cleared. Name $ns in LLMSM_LIMITER_TARGETS and re-run to clear it."
                left=$((left + 1))
            else
                log_warning "  $(pl_target_label "$ns" "$deploy"): $note. Its policy in ${cm_ns} declares no limiter."
            fi
        done 3<<< "$PL_SKIP"
    fi

    if [ "$cleared" -eq 0 ]; then
        # Non-zero, not a warning: the command removed nothing while a limiter
        # of the other kind still bounds these controllers. That is the shape
        # of "undo my quota" run without LLMSM_LIMITER_TYPE=quota, and a script
        # or runbook that goes on after exit 0 believes the bound is gone. A
        # policy with no limiter at all stays exit 0: nothing to do is done.
        if [ -n "$other_left" ]; then
            log_error "nothing removed: no $limiter entry in any policy this command reads. The $other limiter stays in:${other_left}. To remove it: make disable-limiter LLMSM_LIMITER_TYPE=${other}, or LLMSM_LIMITER_TYPE=all for every limiter"
        fi
        if [ "$left" -gt 0 ]; then
            log_warning "No verified controller policy declared a limiter, but $left skipped controller(s) above still do."
        else
            log_warning "Nothing removed: no scaling-policy ConfigMap this command reads declares a limiter. Nothing was bounded, so there is nothing to disable (exit 0)."
        fi
        return 0
    fi
    if [ -n "$other_left" ]; then
        log_info "The $other limiter still bounds the controllers that read the policy in:${other_left}."
    else
        log_warning "Scaling is now bounded by nothing the scaling manager knows about for the controllers that read the policies above: the optimizer allocates without a GPU budget, and a scale-from-zero wake is published without a capacity check."
    fi
    [ "$left" -eq 0 ] || log_warning "$left skipped controller(s) above still declare a limiter in their own policy."
    log_info "The cluster-wide RBAC is left in place — it grants reads only, and removing it would break the next enable. The Role in the Kueue namespace goes with the quota; enable grants it again."
}
