#!/usr/bin/env bash
#
# Prerequisite checks for deploy/install.sh.
# Requires vars: REQUIRED_TOOLS.
# Requires funcs: log_info/log_success/log_error.
#

# Minimum versions. Only tools whose minimum actually matters are listed; a tool
# absent from here is checked for presence alone, because inventing a floor
# nobody has verified turns a working install into a refused one.
readonly MIN_KUBECTL_VERSION="1.24"
readonly MIN_HELM_VERSION="3.8"
# The overlay filters use go-yq v4 syntax (strenv(), -o=json). yq v3 takes a
# different command form entirely, and the python "yq" is a jq wrapper that
# accepts the expressions and emits JSON. Both pass a presence check and then
# behave differently, so the version is gated, not just the name.
readonly MIN_YQ_VERSION="4.0"
# jq had no floor at all, which is how an old one reached an operator and made
# `check-prereqs` report a namespace as holding no model servers: 1.5 parses
# `a + b as $x | body` as `a + (b as $x | body)` and dies on string + boolean.
# The predicates are parenthesised now and work on any version, and
# hack/check-jq-predicates.sh keeps them that way -- this floor is so the next
# expression does not have to rediscover which jq it may assume.
readonly MIN_JQ_VERSION="1.6"

# version_at_least compares dotted versions without sort -V, which BSD/macOS
# lacks in the form this needs. Compares major.minor only: patch levels have
# never been the thing that breaks a deploy here.
version_at_least() {
    local have="$1" want="$2"
    local have_major="${have%%.*}" want_major="${want%%.*}"
    local have_rest="${have#*.}" want_rest="${want#*.}"
    local have_minor="${have_rest%%.*}" want_minor="${want_rest%%.*}"
    # Non-numeric (a dev build, an unparsed string) is treated as new enough:
    # refusing to install on a version we could not read is the worse failure.
    [[ "$have_major$have_minor$want_major$want_minor" =~ ^[0-9]+$ ]] || return 0
    (( have_major > want_major )) && return 0
    (( have_major < want_major )) && return 1
    (( have_minor >= want_minor ))
}

# tool_version prints a tool's major.minor, or nothing if it cannot be read.
tool_version() {
    local tool="$1" raw=""
    case "$tool" in
        kubectl) raw=$(kubectl version --client 2>/dev/null | grep -oE 'v[0-9]+\.[0-9]+' | head -1) ;;
        helm)    raw=$(helm version --short 2>/dev/null | grep -oE 'v[0-9]+\.[0-9]+' | head -1) ;;
        yq)      raw=$(yq --version 2>/dev/null | grep -oE '[0-9]+\.[0-9]+' | head -1) ;;
        *)       return 0 ;;
    esac
    echo "${raw#v}"
}

# append_conditional_tools adds, to the caller's `tools` array, what this
# particular invocation needs on top of REQUIRED_TOOLS. They are conditional
# because requiring them always would refuse installs that never touch them —
# and, worse, NOT checking them at all lets an install fail halfway, after it has
# already created namespaces and RBAC.
#
# It appends to a caller-declared array rather than echoing into mapfile, because
# mapfile is bash 4+ and macOS still ships bash 3.2.
append_conditional_tools() {
    # yq and jq edit the shipped ConfigMaps in place, and the undeploy filters the
    # rendered overlay with yq. Both are effectively unconditional: the install
    # always patches PROMETHEUS_BASE_URL, so patch_manager_config always runs.
    # They were listed as conditional, which let the preflight pass and the install
    # then die after namespaces, RBAC and the Deployment already existed.
    tools+=("yq" "jq")
    # column formats the ScaledObject plan.
    if [ "${LLMSM_DEFAULT_SO:-false}" != "false" ]; then
        tools+=("column")
    fi
    # openssl mints the self-signed cert for a Prometheus this install deploys.
    # It also mints the external scaler certificates when there is no
    # cert-manager; asked for whenever scaler TLS is requested, since whether
    # cert-manager answers is only known later.
    if [ "${DEPLOY_PROMETHEUS:-true}" = "true" ] || [ "${LLMSM_SCALER_TLS:-}" = "true" ]; then
        tools+=("openssl")
    fi
    if [ "${ENVIRONMENT:-kubernetes}" = "openshift" ]; then
        tools+=("oc")
    fi
}

check_prerequisites() {
    log_info "Checking prerequisites..."

    local missing_tools=() outdated_tools=()
    local tools=("${REQUIRED_TOOLS[@]}")
    append_conditional_tools

    for tool in "${tools[@]}"; do
        [ -n "$tool" ] || continue
        if ! command -v "$tool" &> /dev/null; then
            missing_tools+=("$tool")
            continue
        fi
        local have want=""
        have=$(tool_version "$tool")
        case "$tool" in
            kubectl) want="$MIN_KUBECTL_VERSION" ;;
            helm)    want="$MIN_HELM_VERSION" ;;
            yq)      want="$MIN_YQ_VERSION" ;;
            jq)      want="$MIN_JQ_VERSION" ;;
        esac
        if [ -n "$want" ] && [ -n "$have" ] && ! version_at_least "$have" "$want"; then
            outdated_tools+=("$tool $have (need >= $want)")
        fi
    done

    # Keep deploy paths deterministic: fail fast instead of prompting for installs.
    if [ ${#missing_tools[@]} -ne 0 ]; then
        log_error "Missing required tools: ${missing_tools[*]}. Install them on PATH, or set SKIP_CHECKS=true to bypass this check (not recommended)."
    fi
    if [ ${#outdated_tools[@]} -ne 0 ]; then
        log_error "Tools too old: ${outdated_tools[*]}. Upgrade them, or set SKIP_CHECKS=true to bypass this check (not recommended)."
    fi

    # kind-emulator is exempt because its cluster is never an input. The INSTALL
    # path provisions it before reaching here (see install_core.sh), and the
    # PREFLIGHT path deliberately creates nothing at all -- "they provision the
    # cluster rather than inspect it". Demanding one in either case reports a
    # fault that is not one.
    #
    # deploy/kind-emulator/install.sh checks for the cluster itself and says
    # something better when it is absent and CREATE_CLUSTER=false, so nothing is
    # lost here.
    if [ "${ENVIRONMENT:-}" = "kind-emulator" ]; then
        log_success "All prerequisites met (tools: ${tools[*]})"
        log_info "  Cluster reachability is not checked on kind-emulator: the cluster is this run's own."
        return 0
    fi

    # A reachable cluster is as much a prerequisite as a binary, and finding out
    # here beats finding out after the first namespace has been created.
    # `kubectl get --raw /version`, not `kubectl cluster-info`: cluster-info LISTS
    # SERVICES IN kube-system, which a namespace tenant may not do. So the very
    # first check in the tenant path failed for every real tenant, and told them
    # their kubeconfig was wrong when it was correct and working. /version is
    # served to any authenticated caller and needs no RBAC, which is exactly the
    # question being asked here: can this kubeconfig reach a cluster at all.
    if ! kubectl get --raw /version > /dev/null 2>&1; then
        # ${LLMSM_KUBE_CONTEXT:-...} for the same reason as in llmsm_report_cluster:
        # current-context ignores --context, so naming it here told the operator to
        # check a context the run was not using.
        log_error "Cannot reach a Kubernetes cluster with context '${LLMSM_KUBE_CONTEXT:-$(kubectl config current-context 2>/dev/null || echo 'none set')}'. Check your kubeconfig, or set SKIP_CHECKS=true to bypass this check."
    fi

    log_success "All prerequisites met (tools: ${tools[*]})"
}

# check_permissions verifies the caller can create what the install produces, and
# that the controller will be able to read what its configuration requires.
#
# `make check-prereqs` used to answer only "are the binaries here", and passed for a
# user who could not create a single object the install makes. A preflight that
# cannot fail on the most likely failure is not a preflight.
#
# The node rule is the interesting one, and it is conditional by design:
#
#   physical limiter ON   nodes are REQUIRED. A variant whose accelerator cannot be
#                         resolved is charged to no pool, receives no GPU budget and
#                         NEVER SCALES UP — silently, because nothing errors. An
#                         install that cannot read nodes is therefore broken, and
#                         this is an error.
#   physical limiter OFF  the install still works, degraded: accelerators stay
#                         unresolved, so metrics lose the accelerator label and the
#                         capacity model cannot reuse learned capacity. A warning,
#                         not a refusal — and it says what changes if the limiter is
#                         turned on later.
# can_i_as answers a permission question ABOUT ANOTHER SUBJECT, distinguishing
# "no" from "could not ask".
#
# `kubectl auth can-i --as` impersonates, which the CALLER must be allowed to do.
# A cluster admin can; plenty of legitimate installers cannot. Both cases exit
# non-zero, so treating non-zero as "denied" turned "I lack impersonate" into
# "the controller cannot read nodes" — a hard, wrong refusal to install, and one
# that gets worse the less privileged the user is.
#
# Echoes: yes | no | unknown
can_i_as() {
    local subject="$1"; shift
    local out rc=0
    # stdout only: the answer is yes/no there, and a warning on stderr is not
    # an answer (kc_capture).
    kc_capture auth can-i "$@" --as "$subject" || rc=$?
    out="$KC_OUT"
    case "$out" in
        yes*) echo yes ;;
        no*)  [ $rc -ne 0 ] && echo no || echo unknown ;;
        *)    echo unknown ;;
    esac
}

# llmsm_cluster_policy_ns echoes the namespace this install's controller will read
# limiters and quotas from, or nothing when none is published.
#
# It resolves the way the CONTROLLER does, because a preflight that consults a
# different source than the thing it is checking is worse than no preflight:
#
#   1. a scaling-manager.llmd.ai/policy-namespace label on the namespace being managed
#   2. the default namespace hardcoded in the scaling manager
#
# Step 2's name is read OUT OF THE BINARY rather than repeated here. One
# definition, in Go; a bash copy would be free to drift, and the drift shows up as
# a preflight that passes while the controller reads a different namespace.
llmsm_cluster_policy_ns() {
    local managed="${LLMSM_WATCH_NS:-$LLMSM_NS}" pointed="" out rc
    # Namespaces are cluster-scoped, so a tenant holding only namespaced rights
    # cannot read the object their own namespace is. Folding that denial into "no
    # label" answered with the built-in default policy namespace when the truth
    # was that the question could not be asked — and the label may point
    # somewhere else entirely. "unknown" is echoed so the caller can say so.
    rc=0
    kc_capture get namespace "$managed" \
        -o jsonpath='{.metadata.labels.scaling-manager\.llmd\.ai/policy-namespace}' || rc=$?
    if [ $rc -ne 0 ]; then
        printf '%s' "$KC_ERR" | grep -qi 'forbidden' && { echo unknown; return 0; }
        return 0
    fi
    pointed="$KC_OUT"
    [ -n "$pointed" ] || return 0
    kubectl get namespace "$pointed" >/dev/null 2>&1 || return 0
    echo "$pointed"
}

# llmsm_cluster_policy_limiter echoes the limiter type cluster policy declares, or
# nothing. Reads the same ConfigMap and the same `default` entry the controller
# does.
# Echoes the limiter cluster policy declares, or nothing.
#
# Echoes the literal "unknown" when the ConfigMap cannot be READ, which is not
# the same as no limiter being declared. Reporting Forbidden as "none" told a
# tenant "scaling is UNBOUNDED" while a gpu-inventory limiter was in force — the
# opposite of the truth, and in the direction that sounds harmless.
# llmsm_report_cluster names the cluster every later command will act on.
#
# Read-only and never fatal: an unreachable cluster is check_prerequisites'
# finding to report, and this running first must not pre-empt the better error.
llmsm_report_cluster() {
    local ctx server
    # LLMSM_KUBE_CONTEXT wins, because it is what every other command will use.
    # `kubectl config current-context` reports the kubeconfig's current context and
    # ignores --context, so with a context pinned this line named the cluster the
    # run was NOT talking to -- the exact contradiction this function exists to
    # remove. Caught by pinning a context that does not exist: the banner still
    # named the real one.
    if [ -n "${LLMSM_KUBE_CONTEXT:-}" ]; then
        ctx="$LLMSM_KUBE_CONTEXT"
    else
        ctx=$(kubectl config current-context 2>/dev/null) || ctx=""
    fi
    server=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}' 2>/dev/null) || server=""
    if [ -z "$ctx" ] && [ -z "$server" ]; then
        log_warning "Cluster: no current kubeconfig context. Everything below will fail until one is set."
        return 0
    fi
    log_info "Cluster:    ${ctx:-<no context name>}${LLMSM_KUBE_CONTEXT:+  (pinned by LLMSM_KUBE_CONTEXT)}"
    [ -n "$server" ] && log_info "  server:   $server"
    # Only when it is set: naming the file is useful precisely when it is not the
    # default one, and noise when it is.
    [ -n "${KUBECONFIG:-}" ] && log_info "  kubeconfig: $KUBECONFIG"
    return 0
}

llmsm_cluster_policy_limiter() {
    local policy_ns="$1" cm="" listing rc=0
    [ -n "$policy_ns" ] || return 0
    # || rc=$?, not ; rc=$? -- see the note in install_core.sh. Without it a
    # Forbidden listing kills this under `set -e` before the Forbidden branch
    # below can return "unknown", and the caller's own assignment fails too.
    kc_capture get configmap -n "$policy_ns" -o name || rc=$?
    listing="$KC_OUT"
    if [ $rc -ne 0 ]; then
        printf '%s' "$KC_ERR" | grep -qi 'forbidden' && echo "unknown"
        return 0
    fi
    cm="$(printf '%s\n' "$listing" | grep -E "configmap/(scaling-manager-)?scaling-policy-config$" | head -1 | cut -d/ -f2 || true)"
    [ -n "$cm" ] || return 0
    # The physical limiter when the list declares one, wherever it sits: every
    # declared limiter is built, and it is the one that needs node read. The
    # first entry alone missed it behind a quota. The types are listed by the
    # parser and chosen here, in the shell.
    local types
    types="$(kubectl get configmap "$cm" -n "$policy_ns" -o jsonpath='{.data.default}' 2>/dev/null \
        | yq -r '(.limiters // [])[] | .type' 2>/dev/null || true)"
    if printf '%s\n' "$types" | grep -qxE 'gpu-inventory|inventory'; then
        echo gpu-inventory
        return 0
    fi
    printf '%s\n' "$types" | head -1
}

# check_tenant_install enforces the contract for a scope that creates no
# cluster-scoped objects.
#
# Limiters and quotas are CLUSTER-defined. A tenant reads them; they do not write
# them. So the only question at install time is whether this install can honour
# what the cluster already decided:
#
#   physical limiter declared   the controller must be able to list nodes, which
#                               only a cluster admin can grant. Without it every
#                               variant is charged to no accelerator pool, gets no
#                               budget and NEVER SCALES UP — so the install FAILS
#                               here rather than succeeding into that state.
#   anything else               install proceeds with a warning saying what is and
#                               is not bounding it.
check_tenant_install() {
    local sa="system:serviceaccount:${LLMSM_NS}:scaling-manager-controller-manager"
    local policy_ns limiter node_read

    # A controller that manages several namespaces runs in one of its own, so
    # it reads its own ConfigMap (internal/controller/policy_namespace.go: not
    # self-managed), which this install writes. No label on a managed
    # namespace redirects it.
    if llmsm_watch_is_multi; then
        log_info "Policy: this controller manages $(llmsm_watch_words | sed 's/ /, /g') from $LLMSM_NS, so it reads its limiters from its own ConfigMap there (scaling-manager-scaling-policy-config), which this install writes."
        return 0
    fi

    policy_ns="$(llmsm_cluster_policy_ns)"
    if [ "$policy_ns" = "unknown" ]; then
        log_warning "Could not read the ${LLMSM_WATCH_NS:-$LLMSM_NS} namespace object, so which cluster policy governs this install is unknown — a namespace is cluster-scoped, and namespaced rights do not reach it."
        log_warning "  If that policy declares the gpu-inventory limiter, this controller also needs node read access, and stays NotReady without it. A cluster admin can tell you, or 'make setup-prereqs' grants it either way."
        return 0
    fi
    if [ -z "$policy_ns" ]; then
        # No label on the managed namespace. The controller will fall back to its
        # built-in default policy namespace, whose NAME this script deliberately
        # does not know — one definition, in Go, so the two cannot drift. Which
        # means the limiter cannot be resolved from here, and the check moves to
        # where the name lives: the controller refuses to become ready if cluster
        # policy demands node access it does not have, and verify_deployment turns
        # that into a failed install.
        log_info "This namespace carries no scaling-manager.llmd.ai/policy-namespace label, so the controller uses the well-known policy namespace if it exists and the controller can read Namespaces, and its own ConfigMap otherwise. Its log says which: 'Cluster policy comes from'."
        log_warning "  If no cluster policy exists there either, scaling here is bounded only by each workload's maxReplicas. A ResourceQuota on ${LLMSM_NS} is the way to bound it."
        return 0
    fi

    limiter="$(llmsm_cluster_policy_limiter "$policy_ns")"
    log_info "Cluster policy: ${policy_ns} (limiter: ${limiter:-none})"

    if [ "$limiter" = "unknown" ]; then
        # Forbidden, not absent. Reporting it as a limiter named "unknown" said
        # the install was bounded when nobody had established that it was — and
        # skipped the node-access check that a gpu-inventory policy makes
        # load-bearing, so a variant that can never scale up would look healthy.
        log_warning "Could not read the cluster policy, so whether a limiter bounds this install is unknown. If it declares the gpu-inventory limiter, this controller also needs node read access, or every variant it manages stays at its current size."
        return 0
    fi
    if [ "$limiter" != "gpu-inventory" ]; then
        if [ -z "$limiter" ]; then
            log_warning "Cluster policy declares no limiter, so scaling is UNBOUNDED. A ResourceQuota on ${LLMSM_NS} is the only thing that will bound it."
        else
            log_info "Cluster policy bounds this install with the '${limiter}' limiter, which needs no cluster-scoped access"
        fi
        return 0
    fi

    node_read="$(can_i_as "$sa" list nodes)"
    case "$node_read" in
        yes)
            log_success "Cluster policy declares the gpu-inventory limiter, and ${sa} can list nodes"
            ;;
        *)
            log_error "Cluster policy declares the gpu-inventory limiter, and ${sa} cannot list nodes.

A GPU-aware limiter allocates out of per-accelerator pools. Without node access a
variant's accelerator never resolves, it is charged to no pool, it receives no
budget, and it NEVER SCALES UP — with nothing in the logs to say why. Installing
into that state would look like it worked.

Reading nodes is cluster-scoped, so only a cluster admin can grant it. Ask for:

  kubectl create clusterrole scaling-manager-node-reader --verb=get,list,watch --resource=nodes
  kubectl create clusterrolebinding scaling-manager-node-reader-${LLMSM_NS} \\
    --clusterrole=scaling-manager-node-reader --serviceaccount=${LLMSM_NS}:scaling-manager-controller-manager

Or ask them to install the scaling manager for you (LLMSM_SCOPE=namespace), which grants it."
            ;;
    esac
}

# llmsm_api_resource echoes "<resource> <namespaced>" for a Kind, as THIS cluster
# defines it. Empty when the cluster has no such kind.
#
# The mapping is asked for rather than derived, because deriving it is wrong in
# ways that are silent: `kubectl auth can-i create networkpolicys` is not a
# denial, it is a question about a resource that does not exist, and it answers
# "no". A false denial refuses a legitimate install.
#
# Columns are NAME SHORTNAMES APIVERSION NAMESPACED KIND, and SHORTNAMES is often
# empty — so the ends are read, never a fixed column index.
llmsm_api_resource() {
    local kind="$1"
    kubectl api-resources --no-headers 2>/dev/null \
        | awk -v k="$kind" '$NF == k { print $1, $(NF-1); exit }' || true
}

# llmsm_autoselect_namespace points a namespace-scoped install at the namespace
# that actually runs llm-d, when nobody said which.
#
# Naming the namespace was never information the installer lacked — the model
# servers are labelled, so the cluster already knows. Asking for it anyway is how
# someone installs into the default namespace, gets a healthy controller managing
# nothing, and has no error to go on.
#
# It only ever acts when NOTHING was chosen: LLMSM_NS_SOURCE=default means the
# caller set neither LLMSM_NS nor NAMESPACE. Anything explicit is left alone.
#
# With several llm-d namespaces it REFUSES rather than picking. A namespace-scoped
# controller manages exactly one, and choosing someone's namespace for them is not
# a guess worth making silently.
llmsm_autoselect_namespace() {
    [ "${LLMSM_NS_SOURCE:-}" = "default" ] || return 0
    [ "$(llmsm_install_scope)" = "namespace" ] || return 0
    # The managed namespaces are named (LLMSM_WATCH_NS), and the controller
    # runs in LLMSM_NS, outside them: nothing to pick.
    [ -z "${LLMSM_WATCH_NS:-}" ] || return 0

    # Searching means listing workloads cluster-wide, which a NAMESPACE ADMIN may
    # not do. Say that, rather than falling back to the default namespace as
    # though the cluster genuinely had no llm-d in it: one is "tell me which
    # namespace", the other is "deploy llm-d first", and they are not the same
    # instruction.
    if ! kubectl auth can-i list deployments -A >/dev/null 2>&1; then
        log_info "Cannot search the cluster for llm-d model servers (listing workloads cluster-wide is not permitted for you), so the namespace cannot be found for you."
        log_info "  Name it:  NAMESPACE=<your namespace>   — llm-d's own variable, so if you followed one of its guides it is already exported."
        return 0
    fi

    local found count
    found="$(llmsm_namespaces_with_model_servers || true)"
    count="$(printf '%s' "$found" | grep -c . || true)"

    if [ "${count:-0}" -eq 1 ]; then
        LLMSM_NS="$(printf '%s' "$found" | tr -d '[:space:]')"
        LLMSM_NS_SOURCE=discovered
        export LLMSM_NS LLMSM_NS_SOURCE
        return 0
    fi

    if [ "${count:-0}" -gt 1 ]; then
        log_error "Several namespaces run llm-d model servers, and a namespace-scoped scaling manager manages exactly one:

$(printf '  - %s\n' $found)
Say which:

    NAMESPACE=<one of the above> make deploy-scaling-manager

Or install one scaling manager for all of them:

    make deploy-scaling-manager SCOPE=cluster"
    fi

    # None found, or not allowed to look. Either way, keep the default and let
    # llmsm_report_namespace explain which of the two it is.
    return 0
}

# llmsm_report_namespace says which namespace this install will use, how that was
# decided, and whether it holds anything to scale.
#
# The last part is the one worth running: a namespace-scoped controller manages
# exactly the namespace it runs in, so pointing it at the wrong one produces a
# controller that installs cleanly, reports healthy, and manages nothing. There is
# no error anywhere in that sequence — the only symptom is that nothing ever
# scales, which is indistinguishable from an idle cluster.
llmsm_report_namespace() {
    local ns="${LLMSM_NS}" scope managed count
    scope="$(llmsm_install_scope)"
    managed="${LLMSM_WATCH_NS:-$ns}"

    case "${LLMSM_NS_SOURCE:-}" in
        discovered)
            log_success "Namespace: $ns  (found: it is the only namespace running llm-d model servers)"
            log_info "  Override with LLMSM_NS=<ns> if you meant a different one." ;;
        llmd-ns)  log_info "Namespace: $ns  (from NAMESPACE)" ;;
        namespace-env)
            log_info "Namespace: $ns  (from NAMESPACE — the variable llm-d's own guides export)" ;;
        explicit) log_info "Namespace: $ns  (you set LLMSM_NS)" ;;
        *)        log_info "Namespace: $ns$([ "$ns" = "scaling-manager-system" ] && echo '  (the default)')" ;;
    esac

    if [ "$scope" = "cluster" ]; then
        log_info "  The controller runs there and manages EVERY namespace."
        return 0
    fi

    if llmsm_watch_is_multi; then
        # Each managed namespace is checked, and reported, by
        # llmsm_check_managed_namespaces.
        log_info "  It runs in $ns and manages $(llmsm_watch_words | sed 's/ /, /g') (LLMSM_WATCH_NS$([ "${LLMSM_WATCH_NS_SOURCE:-}" = discovered ] && echo '=auto: every namespace with llm-d'))."
        log_info "  Only those: it gets a Role in each, and no cluster-wide read of workloads."
        return 0
    elif [ "$managed" != "$ns" ]; then
        log_info "  It runs in $ns and manages $managed (LLMSM_WATCH_NS)."
    else
        log_info "  It runs in $ns and manages that same namespace — nothing outside it."
    fi

    # Does the managed namespace actually contain model servers?
    local ns_err=""
    if ! ns_err="$(kubectl get namespace "$managed" 2>&1 >/dev/null)"; then
        # FORBIDDEN IS NOT ABSENT, and this is the one place it matters most.
        # `namespaces` is cluster-scoped, so a namespace-scoped tenant is routinely
        # denied a read of their OWN namespace -- measured on the shared GPU cluster:
        #
        #   Error from server (Forbidden): namespaces "team-a" is forbidden:
        #   User "system:serviceaccount:team-a:builder" cannot get resource
        #   "namespaces" in API group "" in the namespace "team-a"
        #
        # Treating that as "does not exist" refuses the install of the exact persona
        # this scenario is written for, and tells them their namespace name is wrong
        # when it is right. There is a second signal that needs no cluster-scoped
        # right: the model-server count below is namespaced, so let it answer.
        case "$ns_err" in
            *[Ff]orbidden*)
                log_info "  Cannot read the namespace object for $managed (that is a cluster-scoped read, and you are namespace-scoped) -- judging it by what is running in it instead."
                ;;
            *)
        # STOP. The caller named this namespace -- NAMESPACE is mandatory in this
        # scope -- and in the supported scenario llm-d is already running in it, so
        # the namespace exists. A name that resolves to nothing is a typo, a stale
        # export, or the wrong cluster, and creating it would produce exactly the
        # thing this preflight exists to prevent: a healthy controller with nothing
        # to scale.
        #
        # kind-emulator is exempt: the e2e path provisions llm-d and the scaling manager in one
        # sequence and owns its own ordering. SKIP_CHECKS=true is the bypass for
        # anyone who means it -- and it has to be TESTED here, not just named in
        # the message below. It was not, so the escape hatch this text advertises
        # did nothing and the install died anyway (issue #27).
        if [ "${ENVIRONMENT:-}" != "kind-emulator" ] && [ "${SKIP_CHECKS:-false}" != "true" ]; then
            log_error "  Refusing to continue: namespace $managed does not exist.

You named it, and in this scenario llm-d is already running in it — so the name is
almost certainly wrong: a typo, a stale NAMESPACE export, or the wrong cluster.

  name the namespace llm-d serves from:  NAMESPACE=<namespace> make <target>
  or install without the checks:         SKIP_CHECKS=true"
        fi
        # Skipping the gate must not skip the finding, or SKIP_CHECKS becomes a
        # way to be uninformed rather than a way to proceed knowingly.
        if [ "${SKIP_CHECKS:-false}" = "true" ]; then
            log_warning "  SKIP_CHECKS=true: continuing although namespace $managed does not exist."
        fi
        log_info "  $managed does not exist yet; the install creates it."
        return 0
                ;;
        esac
    fi
    count="$(llmsm_model_server_count "$managed")"
    if [ "$count" -gt 0 ] 2>/dev/null; then
        log_success "  $managed holds $count llm-d model server(s) for it to manage."
    else
        # Terse by design: the STOP itself is the actionable message, and every
        # rationale that used to live here (why an empty namespace is a silent
        # failure rather than a warning, the "where else are they" convenience,
        # the permission-scoped exemption from an earlier revision) now lives once
        # in the guide's "How it works" section, not repeated at every call site.
        #
        # kind-emulator is exempt: the e2e path provisions llm-d and the scaling manager in one
        # sequence and owns the ordering itself. SKIP_CHECKS=true is the bypass, and
        # the only one -- a per-check LLMSM_ALLOW_* flag was tried and removed, because
        # the scenario is already selected by LLMSM_SCOPE and a second knob for the
        # same decision is a second thing to get wrong. It is tested here as well as
        # named in the message: it used not to be (issue #27).
        #
        # Cluster scope never reaches here: it returns above, because a
        # cluster-scoped controller manages namespaces that need not exist yet.
        if [ "${ENVIRONMENT:-}" != "kind-emulator" ] && [ "${SKIP_CHECKS:-false}" != "true" ]; then
            log_error "  No llm-d found in $managed. Please install llm-d on $managed."
            log_error "    SKIP_CHECKS=true to continue anyway — see docs/reference/operations.md#first-line-troubleshooting"
        fi
        # As above: say it even when the gate is skipped.
        if [ "${SKIP_CHECKS:-false}" = "true" ]; then
            log_warning "  SKIP_CHECKS=true: continuing although $managed holds no llm-d model servers; the scaling manager will have nothing to scale."
        fi
    fi
}

# llmsm_model_server_count counts llm-d model servers in one namespace, by the same
# marker and on the same pod-template basis the ScaledObject scan uses — `-l` on
# the object matches nothing, because llm-d labels the pod template.
llmsm_model_server_count() {
    local ns="$1" total=0 resource pod n
    for resource in deployments:"$SO_POD_PATH_DEPLOYMENT" leaderworkersets:"$SO_POD_PATH_LWS"; do
        pod="${resource#*:}"; resource="${resource%%:*}"
        # Keep exactly one integer. `|| echo 0` on a pipeline that already printed
        # a count appends a SECOND value — the resource kind may be absent (no LWS
        # CRD) or unreadable, and then `$((total + 0\n0))` is a syntax error that
        # takes the preflight down mid-report. Last line, digits only.
        n="$(kubectl get "$resource" -n "$ns" -o json 2>/dev/null \
            | jq --argjson p "$pod" --argjson markers "$(so_serving_markers_json)" '
                [ .items[]
                  | (((getpath($p) // (if $p == ["spec", "leaderWorkerTemplate", "leaderTemplate"] then getpath(["spec", "leaderWorkerTemplate", "workerTemplate"]) else null end) // {}) | .metadata.labels // {}) + (.metadata.labels // {}))
                  | select(to_entries
                           | any((.key + "=" + (.value|tostring)) as $kv
                                 | ($markers | index($kv)) != null))
                ] | length' 2>/dev/null | tail -1 | tr -cd '0-9' || true)"
        total=$((total + ${n:-0}))
    done
    echo "$total"
}

# llmsm_policy_candidates sets LLMSM_POLICY_CANDS to the namespaces whose
# scaling-manager-scaling-policy-config decides this controller's limiters, the way the
# controller resolves them (internal/controller/policy_namespace.go):
#
#   * an admin-owned controller (cluster scope, or LLMSM_WATCH_NS naming another
#     namespace): its own, LLMSM_NS;
#   * a SELF-MANAGED namespace-scoped controller whose policy namespace
#     RESOLVES -- its managed Namespace carries a scaling-manager.llmd.ai/policy-namespace
#     label, or scaling-manager-policy exists: that namespace ONLY. The controller then
#     reads limiters from nowhere else, so its own ConfigMap, which the tenant
#     writes, must not decide an admin's cluster-wide grant;
#   * a self-managed one whose policy namespace does not resolve: its own;
#   * one where that cannot be told (the label or scaling-manager-policy unreadable): the
#     policy namespace, then its own.
#
# LLMSM_POLICY_CANDS_UNREAD names what could not be read finding them, and
# LLMSM_POLICY_OWN_TENANT is yes when LLMSM_NS is the tenant's own namespace (a
# self-managed controller), where an unreadable policy counts as none
# (llmsm_kueue_borrow_wanted). Globals, set in the caller's shell.
LLMSM_POLICY_CANDS=""
LLMSM_POLICY_CANDS_UNREAD=""
LLMSM_POLICY_OWN_TENANT=no
llmsm_policy_candidates() {
    local managed policy_ns rc=0 resolved=""
    managed="${LLMSM_WATCH_NS:-$LLMSM_NS}"
    LLMSM_POLICY_CANDS="$LLMSM_NS"
    LLMSM_POLICY_CANDS_UNREAD=""
    LLMSM_POLICY_OWN_TENANT=no
    if [ "$(llmsm_install_scope)" = "namespace" ] && [ "$managed" = "$LLMSM_NS" ]; then
        LLMSM_POLICY_OWN_TENANT=yes
        # Streams apart (kc_capture): a warning on stderr is neither the label
        # nor a NotFound.
        kc_capture get namespace "$managed" \
            -o jsonpath='{.metadata.labels.scaling-manager\.llmd\.ai/policy-namespace}' || rc=$?
        if [ "$rc" -ne 0 ]; then
            case "$KC_ERR" in
                *NotFound*|*"not found"*) policy_ns=scaling-manager-policy ;;
                *)
                    # The label may point anywhere; scaling-manager-policy is still worth a look.
                    LLMSM_POLICY_CANDS_UNREAD=" Namespace $managed (its policy-namespace label);"
                    policy_ns=scaling-manager-policy resolved=unknown ;;
            esac
        elif [ -n "$KC_OUT" ]; then
            policy_ns="$KC_OUT" resolved=yes   # a label resolves whether or not its namespace exists
        else
            policy_ns=scaling-manager-policy
        fi
        if [ -z "$resolved" ]; then
            rc=0
            kc_capture get namespace scaling-manager-policy -o name || rc=$?
            if [ "$rc" -eq 0 ]; then
                resolved=yes
            elif kc_not_found "$KC_ERR"; then
                resolved=no
            else
                LLMSM_POLICY_CANDS_UNREAD="$LLMSM_POLICY_CANDS_UNREAD Namespace scaling-manager-policy;"
                resolved=unknown
            fi
        fi
        case "$resolved" in
            yes) LLMSM_POLICY_CANDS="$policy_ns" ;;
            no) ;;
            *) case " $LLMSM_POLICY_CANDS " in *" $policy_ns "*) ;; *) LLMSM_POLICY_CANDS="$policy_ns $LLMSM_POLICY_CANDS" ;; esac ;;
        esac
    fi
}

# llmsm_kueue_borrow_wanted sets LLMSM_KUEUE_BORROW_WANTED to yes | no | unknown:
# whether some limiter policy this controller reads counts Kueue cohort
# borrowing (policy_counts_borrowing), and so needs the cluster-wide Workload
# read the admin phase grants (pl_sync_kueue_borrow_read). The policies are
# those of llmsm_policy_candidates -- once a policy namespace resolves, that one
# alone -- and its own one as THIS install leaves it:
#
#   * LLMSM_LIMITER naming quota with LLMSM_QUOTA_KUEUE=true: LLMSM_QUOTA_KUEUE_BORROW
#     decides, and unset keeps what the live entry says, as policy_with_limiters
#     keeps it -- a live countBorrowable: false stays false;
#   * LLMSM_LIMITER naming no Kueue quota: nothing is counted there;
#   * LLMSM_LIMITER unset: the live policy, which the install keeps.
#
# unknown, with LLMSM_KUEUE_BORROW_UNREAD naming what, when no policy says yes
# and one an ADMIN owns could not be read: the caller leaves the grant as it
# is. The tenant's own policy (llmsm_policy_is_tenants) that cannot be read or
# parsed counts as no, with a warning.
#
# The tenant's own policy -- a self-managed controller whose policy namespace
# does not resolve -- never decides the read on its own. Only this run's
# explicit admin settings do: LLMSM_LIMITER naming quota with
# LLMSM_QUOTA_KUEUE=true grants it, and LLMSM_LIMITER naming no Kueue quota, or
# LLMSM_QUOTA_KUEUE_BORROW=false, revokes it. With LLMSM_LIMITER unset the answer
# is unknown, so the grant is left as it is: a plain re-run neither grants the
# read nor revokes one an earlier explicit run granted. A note says so; a
# tenant ConfigMap that counts borrowing is warned about, naming those
# settings, and sets LLMSM_KUEUE_BORROW_TENANT_ONLY=yes so the tenant-phase check
# still says the controller lacks the read. Also sets
# LLMSM_KUEUE_KEPT_NS, the kueue.controllerNamespace a policy keeps, for the
# Kueue controller read (pl_grant_kueue_controller_read).
LLMSM_KUEUE_BORROW_WANTED=unknown
LLMSM_KUEUE_BORROW_UNREAD=""
LLMSM_KUEUE_BORROW_TENANT_ONLY=no
LLMSM_KUEUE_KEPT_NS=""

# llmsm_policy_is_tenants reports whether the policy ConfigMap in namespace $1 is
# the tenant's own: the controller's own namespace, for a self-managed
# controller (LLMSM_POLICY_OWN_TENANT, set by llmsm_policy_candidates).
llmsm_policy_is_tenants() {
    [ "$1" = "$LLMSM_NS" ] && [ "${LLMSM_POLICY_OWN_TENANT:-no}" = yes ]
}

# llmsm_warn_tenant_policy_unread says that the tenant's own policy in $1 $2
# ("cannot be read", "does not parse") and so counts as no borrowing. Left
# unknown, the grant it once earned was never revoked: a tenant kept an
# admin's cluster-wide Workload read by breaking its own ConfigMap.
llmsm_warn_tenant_policy_unread() {
    log_warning "  $1/scaling-manager-scaling-policy-config $2. It is the tenant's own policy, so it counts as not borrowing: it does not keep the cluster-wide Kueue Workload read."
}

llmsm_kueue_borrow_wanted() {
    local ns rc doc ans=no unread kept_false undecided=""
    LLMSM_KUEUE_BORROW_WANTED=unknown
    LLMSM_KUEUE_BORROW_UNREAD=""
    LLMSM_KUEUE_BORROW_TENANT_ONLY=no
    LLMSM_KUEUE_KEPT_NS=""
    llmsm_policy_candidates
    unread="$LLMSM_POLICY_CANDS_UNREAD"
    for ns in $LLMSM_POLICY_CANDS; do
        rc=0
        kc_capture get configmap scaling-manager-scaling-policy-config -n "$ns" -o jsonpath='{.data.default}' || rc=$?
        doc="$KC_OUT"
        if [ "$rc" -ne 0 ]; then
            case "$KC_ERR" in
                *NotFound*|*"not found"*) doc="" ;;
                *)
                    if llmsm_policy_is_tenants "$ns" && [ -z "${LLMSM_LIMITER:-}" ]; then
                        undecided="$ns"
                    elif llmsm_policy_is_tenants "$ns"; then
                        llmsm_warn_tenant_policy_unread "$ns" "cannot be read"
                    else
                        unread="$unread $ns/scaling-manager-scaling-policy-config;"
                    fi
                    continue ;;
            esac
        fi
        [ -n "$doc" ] || doc='limiters: []'
        [ -n "$LLMSM_KUEUE_KEPT_NS" ] || LLMSM_KUEUE_KEPT_NS="$(printf '%s\n' "$doc" | policy_kueue_namespace)"
        if llmsm_policy_is_tenants "$ns" && [ -z "${LLMSM_LIMITER:-}" ]; then
            # The tenant writes this ConfigMap, and the read reaches every
            # tenant's Workloads: only the admin's explicit settings decide it.
            undecided="$ns"
            if [ "$(printf '%s\n' "$doc" | policy_counts_borrowing)" = yes ]; then
                LLMSM_KUEUE_BORROW_TENANT_ONLY=yes
                log_warning "  $ns/scaling-manager-scaling-policy-config counts Kueue cohort borrowing, but it is the tenant's own policy (no policy namespace resolves), so it does not grant the cluster-wide Kueue Workload read. To grant it, re-run with this install's LLMSM_LIMITER=quota and LLMSM_QUOTA_KUEUE=true."
            fi
            continue
        fi
        if [ "$ns" = "$LLMSM_NS" ] && [ -n "${LLMSM_LIMITER:-}" ]; then
            # This install rewrites its own entry.
            # An LLMSM_LIMITER limiter_types_of refuses names no quota here; the
            # install refuses it on its own.
            if ! limiter_set_has "$(limiter_types_of "$LLMSM_LIMITER" 2>/dev/null)" quota \
                || [ "${LLMSM_QUOTA_KUEUE:-false}" != "true" ]; then
                continue
            fi
            # A scope: cluster entry counts no borrowing, so it needs no
            # cluster-wide Workload read (policy_counts_borrowing).
            [ "${LLMSM_QUOTA_SCOPE:-namespace}" != cluster ] || continue
            case "${LLMSM_QUOTA_KUEUE_BORROW:-}" in
                false) continue ;;
                true) ans=yes; continue ;;
            esac
            kept_false="$(printf '%s\n' "$doc" | yq '[.limiters[]? | select(.type == "quota" and .kueue.countBorrowable == false)] | length' 2>/dev/null)" || kept_false=""
            case "$kept_false" in
                0) ans=yes ;;
                ''|null|*[!0-9]*)
                    if llmsm_policy_is_tenants "$ns"; then
                        llmsm_warn_tenant_policy_unread "$ns" "does not parse"
                    else
                        unread="$unread $ns/scaling-manager-scaling-policy-config (does not parse);"
                    fi ;;
            esac
            continue
        fi
        case "$(printf '%s\n' "$doc" | policy_counts_borrowing)" in
            yes) ans=yes ;;
            no) ;;
            *)
                if llmsm_policy_is_tenants "$ns"; then
                    llmsm_warn_tenant_policy_unread "$ns" "does not parse"
                else
                    unread="$unread $ns/scaling-manager-scaling-policy-config (does not parse);"
                fi ;;
        esac
    done
    if [ "$ans" = yes ]; then
        LLMSM_KUEUE_BORROW_WANTED=yes
    elif [ -n "$unread" ]; then
        LLMSM_KUEUE_BORROW_UNREAD="$(printf '%s' "${unread%;}" | sed 's/^ *//')"
    elif [ -n "$undecided" ]; then
        LLMSM_KUEUE_BORROW_UNREAD="$undecided/scaling-manager-scaling-policy-config is the tenant's own policy and this run sets no LLMSM_LIMITER"
        log_info "  The cluster-wide Kueue Workload read of $LLMSM_NS is left as it is: its policy is the tenant's own ($undecided/scaling-manager-scaling-policy-config), which decides nothing alone. Re-run with LLMSM_LIMITER=quota LLMSM_QUOTA_KUEUE=true to grant it, or add LLMSM_QUOTA_KUEUE_BORROW=false to revoke it."
    else
        LLMSM_KUEUE_BORROW_WANTED=no
    fi
}

# llmsm_check_kueue_borrow_read warns when a controller-only install counts
# Kueue cohort borrowing and its controller cannot list Workloads in every
# namespace. That read is granted by the admin phase only to a controller
# whose policy counts borrowing, so a tenant who turns borrowing on later has
# to ask for it. Denied, borrowing is withheld (lenders-unreadable) and the
# nominal Kueue cap applies, so this warns and never refuses.
llmsm_check_kueue_borrow_read() {
    [ "${INSTALL_PHASE:-all}" = "scaling-manager" ] || return 0
    local sa
    llmsm_kueue_borrow_wanted
    [ "$LLMSM_KUEUE_BORROW_WANTED" = yes ] || [ "$LLMSM_KUEUE_BORROW_TENANT_ONLY" = yes ] || return 0
    sa="system:serviceaccount:${LLMSM_NS}:scaling-manager-controller-manager"
    case "$(can_i_as "$sa" list workloads.kueue.x-k8s.io --all-namespaces)" in
        yes) log_success "The controller can list Kueue Workloads in every namespace, which counting borrowing needs" ;;
        no)
            log_warning "This install counts Kueue cohort borrowing, and $sa cannot list Kueue Workloads in every namespace."
            log_warning "  Borrowing is then withheld (lenders-unreadable); the nominal Kueue cap still applies. The read reaches every tenant's Workloads, so it is granted only when asked: a cluster admin re-runs '$(llmsm_make_cmd setup-prereqs)' with this install's LLMSM_LIMITER and LLMSM_QUOTA_KUEUE. LLMSM_QUOTA_KUEUE_BORROW=false keeps nominal and needs no such read."
            ;;
        *) log_warning "Could not verify that $sa may list Kueue Workloads in every namespace (checking needs impersonate permission). Without it, cohort borrowing is withheld." ;;
    esac
}

# llmsm_kueue_source_configured echoes where a Kueue quota source is configured
# for this controller, nothing when there is none, or `unknown <what>` when a
# policy it may read could not be read.
#
# Sources, in the controller's own terms (internal/controller/policy_namespace.go,
# mirrored by pl_policy_resolution):
#
#   this install's LLMSM_LIMITER naming quota, with LLMSM_QUOTA_KUEUE=true;
#   the policies llmsm_policy_candidates names: the controller's OWN ConfigMap
#     in LLMSM_NS for an admin-owned controller (cluster scope, or LLMSM_WATCH_NS
#     naming another namespace), and for a self-managed one whose policy
#     namespace does not resolve. An upgrade that does not repeat
#     LLMSM_LIMITER/LLMSM_QUOTA_KUEUE still runs with the kueue block an earlier
#     install wrote there;
#   for a SELF-MANAGED controller only (it manages the namespace it runs in),
#     the namespace its managed Namespace's scaling-manager.llmd.ai/policy-namespace label
#     names, else scaling-manager-policy when it exists -- that one alone. An admin-owned
#     controller never reads scaling-manager-policy, so a kueue block there must not
#     refuse it.
#
# A Forbidden or otherwise unreadable read is "unknown", never "no source": the
# caller warns rather than staying silent about a source it could not see.
llmsm_kueue_source_configured() {
    if limiter_set_has "$(limiter_types_of "${LLMSM_LIMITER:-none}" 2>/dev/null)" quota \
        && [ "${LLMSM_QUOTA_KUEUE:-false}" = "true" ]; then
        echo "LLMSM_QUOTA_KUEUE=true"
        return 0
    fi
    local out rc unread ns n
    llmsm_policy_candidates
    unread="$LLMSM_POLICY_CANDS_UNREAD"
    for ns in $LLMSM_POLICY_CANDS; do
        rc=0
        kc_capture get configmap scaling-manager-scaling-policy-config -n "$ns" -o jsonpath='{.data.default}' || rc=$?
        out="$KC_OUT"
        if [ "$rc" -ne 0 ]; then
            case "$KC_ERR" in
                *NotFound*|*"not found"*) ;;
                *) unread="$unread $ns/scaling-manager-scaling-policy-config;" ;;
            esac
            continue
        fi
        n="$(printf '%s\n' "$out" | yq '[.limiters[]? | select(.kueue.enabled == true)] | length' 2>/dev/null)" || n=""
        case "$n" in
            0|null) ;;
            '') unread="$unread $ns/scaling-manager-scaling-policy-config (does not parse);" ;;
            *) echo "the policy in $ns/scaling-manager-scaling-policy-config"; return 0 ;;
        esac
    done
    if [ -n "$unread" ]; then
        echo "unknown${unread%;}"
    fi
    return 0
}

# llmsm_check_kueue_namespace_read refuses a controller-only install whose
# controller could not read the Namespace a Kueue quota needs.
#
# The Kueue reader GETs the Namespace of each LocalQueue, to evaluate a
# ClusterQueue namespaceSelector before its grant is counted. A kueue-reader
# ClusterRole created before that read existed lacks `get namespaces`, and an
# UPGRADE in the controller-only phase does not touch it -- that is the admin's
# half. The new controller then withholds every selector-bearing queue's grant:
# the namespace is bounded at nothing, i.e. it cannot scale up, with one log line
# to say why. So it is a missing prerequisite, reported before anything moves.
#
# Only for INSTALL_PHASE=scaling-manager. The prereqs and all phases apply the role that
# grants it, so asking first would refuse the very apply that fixes it. A
# namespace-scoped role grants the managed namespace by name (resourceNames), so
# that is the name asked about; cluster scope reads LocalQueues everywhere and
# needs the unrestricted read.
#
# A source that could not be read (llmsm_kueue_source_configured: unknown) is
# asked about too, but never refused on: a missing read is then a WARNING that
# names what could not be read.
llmsm_check_kueue_namespace_read() {
    [ "${INSTALL_PHASE:-all}" = "scaling-manager" ] || return 0
    local source sa managed target answer one
    if llmsm_watch_is_multi; then
        for one in $(llmsm_watch_words); do
            (LLMSM_WATCH_NS="$one"; llmsm_check_kueue_namespace_read) || exit 1
        done
        return 0
    fi
    source="$(llmsm_kueue_source_configured)"
    [ -n "$source" ] || return 0
    sa="system:serviceaccount:${LLMSM_NS}:scaling-manager-controller-manager"
    managed="${LLMSM_WATCH_NS:-$LLMSM_NS}"
    if [ "$(llmsm_install_scope)" = "namespace" ]; then
        target="namespaces/$managed"
    else
        target="namespaces"
    fi
    answer="$(can_i_as "$sa" get "$target")"
    case "$source" in
        unknown*)
            [ "$answer" != yes ] || return 0
            log_warning "Could not tell whether a Kueue quota source is configured for this controller: not readable with your permissions:${source#unknown}."
            log_warning "  If one is, $sa needs get on $target ($([ "$answer" = no ] && echo 'it does NOT have it' || echo 'could not verify: checking needs impersonate permission')), or the namespace is bounded at nothing and does not scale up. A cluster admin re-running '$(llmsm_make_cmd setup-prereqs)' grants it."
            return 0
            ;;
    esac
    case "$answer" in
        yes)
            log_success "The controller can get $target, which the Kueue quota source ($source) needs"
            ;;
        no)
            log_error "A Kueue quota source is configured ($source), and $sa cannot get $target.

The Kueue reader reads each LocalQueue's Namespace to evaluate a ClusterQueue
namespaceSelector. Without it every such queue's grant is withheld, so the
namespace is bounded at NOTHING and does not scale up. The kueue-reader role an
earlier version created lacks this read, and this phase cannot change it.

Ask a cluster admin to run, once, for this namespace:

    $(llmsm_make_cmd setup-prereqs)

then re-run this install."
            ;;
        *)
            log_warning "A Kueue quota source is configured ($source); could not verify that $sa may get $target (checking needs impersonate permission)."
            log_warning "  If the kueue-reader role predates that read, the namespace is bounded at nothing. A cluster admin re-running '$(llmsm_make_cmd setup-prereqs)' grants it."
            ;;
    esac
}

# llmsm_check_kueue_cohort_read warns when a controller-only install's controller
# could not list Kueue Cohort objects.
#
# The Kueue reader lists Cohorts to tell a flat cohort from a nested one before
# it counts borrowing. Denied, borrowing reads as hierarchy-unknown and is not
# counted; the nominal Kueue bound is unaffected, so this warns and never
# refuses. Same phase and source rules as llmsm_check_kueue_namespace_read.
llmsm_check_kueue_cohort_read() {
    [ "${INSTALL_PHASE:-all}" = "scaling-manager" ] || return 0
    local source sa
    source="$(llmsm_kueue_source_configured)"
    case "$source" in
        ''|unknown*) return 0 ;;
    esac
    sa="system:serviceaccount:${LLMSM_NS}:scaling-manager-controller-manager"
    case "$(can_i_as "$sa" list cohorts.kueue.x-k8s.io)" in
        yes) log_success "The controller can list Kueue Cohorts, which counting borrowing needs" ;;
        no)
            log_warning "A Kueue quota source is configured ($source), and $sa cannot list cohorts.kueue.x-k8s.io."
            log_warning "  Cohort borrowing is then not counted (reason hierarchy-unknown); the nominal Kueue cap still applies. A cluster admin re-running 'NAMESPACE=$LLMSM_NS make setup-prereqs' grants it."
            ;;
        *) log_warning "Could not verify that $sa may list cohorts.kueue.x-k8s.io (checking needs impersonate permission). Without it, Cohort borrowing is not counted." ;;
    esac
}

check_permissions() {
    log_info "Checking permissions..."

    local ns="${LLMSM_NS}" denied=() sa="system:serviceaccount:${LLMSM_NS}:scaling-manager-controller-manager"
    local kinds kind resource namespaced cluster_denied="" unknown=()
    # The limiter set this install will declare, normalised (a list such as
    # gpu-inventory,quota declares both), printed so the reader sees it before
    # anything changes. Unset keeps the live policy's limiters.
    local lset node_set node_source=named
    lset="$(llmsm_limiter_set)" || exit 1
    node_set="$lset"
    if [ -n "${LLMSM_LIMITER:-}" ]; then
        log_info "Limiters this install declares: ${lset}"
    else
        # Unset keeps the live policy's limiters, so THEY are what the
        # controller will need node read for. Reading LLMSM_LIMITER alone said
        # "no limiter is configured" over a live gpu-inventory,quota policy --
        # the upgrade that went NotReady.
        node_set="$(llmsm_live_limiter_set "$ns")"
        node_source=live
        [ "$node_set" = none ] || log_info "LLMSM_LIMITER is not set: the live policy's limiters are kept: ${node_set}"
    fi

    # What this install creates, read from the RENDERED overlay rather than
    # inferred from the scope name. The scope does not decide it: the
    # namespace-scoped overlay creates ten cluster-scoped objects on Kubernetes
    # (five ClusterRoles and five ClusterRoleBindings) and twelve on OpenShift, where
    # the platform's monitoring wiring — cluster-monitoring-view for Thanos, among
    # others — adds two more bindings that components/tenant-installable does not
    # subtract.
    #
    # Inferring it meant a non-admin passed `--check` on OpenShift and then failed
    # partway through `kubectl apply -k`, in exactly the half-installed state this
    # preflight exists to prevent. It also under-checked BOTH scopes: neither
    # branch asked about Secrets or ServiceMonitors, and the cluster branch never
    # asked about Roles or RoleBindings.
    # Only the kinds THIS PHASE creates.
    #
    # INSTALL_PHASE=scaling-manager is the tenant's half, and every cluster-scoped object in
    # the overlay belongs to the admin's half — already created, by someone else,
    # before the tenant runs anything. Checking "can you create a ClusterRole"
    # there asks the one question whose answer is no by design, and this check
    # exits on a no. So the documented tenant path failed for exactly the person
    # it was written for, with advice telling them to run the command that had
    # just failed. Existence of those objects is checked separately, by
    # require_prereqs_present, which is the right question for this phase.
    # Filtered by object (llmsm_rendered_kinds controller), not by kind name: an
    # admin-owned object of a kind the tenant otherwise owns -- the
    # scaling-manager-external-scaler NetworkPolicy -- must not make this phase ask for
    # rights to create it.
    if [ "${INSTALL_PHASE:-all}" = "scaling-manager" ]; then
        kinds="$(llmsm_rendered_kinds controller || true)"
    else
        kinds="$(llmsm_rendered_kinds || true)"
    fi
    if [ -z "$kinds" ]; then
        log_warning "Could not render the install overlay, so this check falls back to the kinds each scope is expected to create. It may miss something the overlay adds; the install itself renders the same overlay and will fail if it is broken."
        kinds=$'ConfigMap\nDeployment\nRole\nRoleBinding\nSecret\nService\nServiceAccount\nServiceMonitor'
        llmsm_scope_is_tenant || kinds="$kinds"$'\nClusterRole\nClusterRoleBinding\nNamespace'
        # The fallback obeys the phase too. Unfiltered, it asked a tenant in the
        # controller-only phase for create rights on Roles, RoleBindings,
        # Secrets, ServiceAccounts and ServiceMonitors -- the admin's half
        # (LLMSM_PREREQ_KINDS) -- and refused the install that phase exists for.
        if [ "${INSTALL_PHASE:-all}" = "scaling-manager" ]; then
            kinds="$(printf '%s\n' "$kinds" | grep -vxF -f <(printf '%s\n' "${LLMSM_PREREQ_KINDS[@]}") || true)"
        fi
    fi

    for kind in $kinds; do
        set -- $(llmsm_api_resource "$kind")
        resource="${1:-}"; namespaced="${2:-}"
        if [ -z "$resource" ]; then
            # A kind this cluster does not define. NOT a failure: this check runs
            # before the install deploys anything, and the install itself may
            # create the CRD later in the same run — ServiceMonitor when
            # DEPLOY_PROMETHEUS=true is exactly that. Unanswerable, so it is
            # reported rather than either failed or hidden.
            unknown+=("$kind")
            continue
        fi
        if [ "$namespaced" = "true" ]; then
            kubectl auth can-i create "$resource" -n "$ns" >/dev/null 2>&1 \
                || denied+=("create $resource in $ns")
            continue
        fi
        # The namespace itself, only when it does not exist yet — requiring
        # namespace creation from someone installing into a namespace an admin
        # already made for them would refuse a perfectly good install.
        # The Namespace is handled after this loop: the installer creates LLMSM_NS
        # imperatively whatever the overlay contains, so the render is not the
        # authority on it.
        [ "$kind" = "Namespace" ] && continue
        if ! kubectl auth can-i create "$resource" >/dev/null 2>&1; then
            denied+=("create $resource (cluster-scoped)")
            cluster_denied=1
        fi
    done

    # LLMSM_NS, asked separately from the render and at BOTH scopes.
    #
    # create_namespaces materializes LLMSM_NS imperatively whenever it is missing,
    # so the namespace-scoped overlay not containing a Namespace object means
    # nothing here — components/tenant-installable deletes it precisely because a
    # tenant installs into a namespace that already exists. Reading only the
    # render therefore passed a preflight that the install then failed on its
    # first write, which is what this whole check exists to prevent.
    #
    # Only when it is missing: requiring namespace creation from someone
    # installing into a namespace an admin already made for them would refuse a
    # perfectly good install. On OpenShift a self-provisioner creates namespaces
    # through projectrequests rather than namespaces, and either is enough.
    if ! kubectl get namespace "$ns" >/dev/null 2>&1; then
        if ! kubectl auth can-i create namespaces >/dev/null 2>&1 \
            && ! kubectl auth can-i create projectrequests >/dev/null 2>&1; then
            denied+=("create namespace $ns (it does not exist yet)")
            cluster_denied=1
        fi
    fi

    # The Role and RoleBinding the prereqs phase writes in each managed
    # namespace when the controller runs outside it.
    if [ "${INSTALL_PHASE:-all}" != "scaling-manager" ] && [ -n "${LLMSM_WATCH_NS:-}" ]; then
        local watched
        for watched in $(llmsm_watch_words); do
            [ "$watched" != "$ns" ] || continue
            kubectl auth can-i create roles -n "$watched" >/dev/null 2>&1 \
                || denied+=("create roles in $watched (the controller's permissions there)")
            kubectl auth can-i create rolebindings -n "$watched" >/dev/null 2>&1 \
                || denied+=("create rolebindings in $watched (the controller's permissions there)")
        done
    fi

    # The external scaler certificates and KEDA's side of them (LLMSM_SCALER_TLS).
    # Written beside the overlay rather than rendered in it, so the loop above
    # never sees them. One "<resource> <namespace>" per line; "-" is cluster-scoped.
    if declare -F llmsm_scaler_tls_permission_needs >/dev/null; then
        local tls_res tls_ns tls_needs
        tls_needs="$(llmsm_scaler_tls_permission_needs)"
        while read -r tls_res tls_ns; do
            [ -n "$tls_res" ] || continue
            if [ "$tls_ns" = - ]; then
                kubectl auth can-i create "$tls_res" >/dev/null 2>&1 \
                    || { denied+=("create $tls_res, cluster-scoped (external scaler TLS; LLMSM_SCALER_TLS=false skips it)"); cluster_denied=1; }
            else
                kubectl auth can-i create "$tls_res" -n "$tls_ns" >/dev/null 2>&1 \
                    || denied+=("create $tls_res in $tls_ns (external scaler TLS; LLMSM_SCALER_TLS=false skips it)")
            fi
        done <<< "$tls_needs"
    fi

    if [ ${#unknown[@]} -ne 0 ]; then
        log_info "Not checked, because this cluster has no such resource yet: ${unknown[*]}. The install may create it (a ServiceMonitor needs the Prometheus Operator CRDs, which DEPLOY_PROMETHEUS=true installs). If it does not, the apply fails on the missing kind."
    fi

    if [ ${#denied[@]} -ne 0 ]; then
        local advice="Ask for the objects above, or have an admin run the install."
        if [ -n "$cluster_denied" ] && llmsm_scope_is_tenant; then
            advice="LLMSM_SCOPE=namespace narrows what the CONTROLLER reads. It does not make the
install self-service on every platform, and this overlay creates cluster-scoped
objects:

  $(llmsm_overlay_dir)

On OpenShift that is by design — the Thanos and user-workload-monitoring wiring
is cluster-scoped, and without it the controller cannot reach Prometheus at all.

You do not need those rights to run the controller. Have an admin create them
once for this namespace:

    $(llmsm_make_cmd setup-prereqs)

then install with '$(llmsm_make_cmd deploy-scaling-manager) INSTALL_PHASE=scaling-manager', which needs none of them."
        fi
        log_error "You cannot create everything this install produces. Denied:
$(printf '  - %s\n' "${denied[@]}")

$advice"
    fi

    # The quota limiter's budget, checked HERE rather than where it is written.
    #
    # The limiter is declared at the very END of deploy_scaling_manager_controller -- after
    # namespaces, monitoring, CRDs, RBAC, the controller Deployment and the
    # scaler backend have all been applied. A missing or malformed LLMSM_QUOTAS
    # therefore aborted an install that had already left a RUNNING, UNBOUNDED
    # controller behind, which is the state the operator was trying to avoid.
    #
    # BEFORE the tenant early-return, not after it. It sat after, which made it
    # dead code on the DEFAULT path: llmsm_scope_is_tenant is true whenever the
    # scope is `namespace`, which is the default everywhere. Measured on
    # OpenShift -- LLMSM_QUOTAS='H200=12abc' was refused only once the install had
    # printed "Deploying llm-d-scaling-manager...", which is exactly the
    # late failure the comment claimed to have moved.
    #
    # The tenant contract below is about what a tenant may READ; LLMSM_LIMITER is
    # about what THIS install WRITES into its own policy, and a tenant-scoped
    # install writes one too. The two are independent.
    #
    # No `declare -F` guard: install.sh sources limiter_policy.sh
    # unconditionally, so a missing function is a broken source tree, and
    # skipping a safety check on it would fail open.
    if limiter_set_has "$lset" quota; then
        limiter_entry_yaml quota >/dev/null || exit 1
        log_success "LLMSM_QUOTAS parses, so the quota limiter can be declared at the end of the install"
    fi

    # The Kueue reader's Namespace get, which an upgrade does not bring with it.
    # See llmsm_check_kueue_namespace_read. Before the tenant early-return: the
    # controller-only phase is exactly the one that cannot fix it.
    llmsm_check_kueue_namespace_read
    llmsm_check_kueue_cohort_read
    llmsm_check_kueue_borrow_read

    # A tenant install has its own contract for what the CONTROLLER will need, and
    # it is not the node-read question below: the physical limiter's inventory is
    # CLUSTER-defined, and a tenant reads it rather than declaring it.
    if llmsm_scope_is_tenant; then
        check_tenant_install
        log_success "Permissions look sufficient"
        return 0
    fi

    # What the CONTROLLER will need once it is running.
    llmsm_check_limiter_node_read "$node_set" "$sa" "$node_source"
    llmsm_check_limiter_pod_list "$node_set" "$sa"

    log_success "Permissions look sufficient"
}

# llmsm_check_limiter_pod_list <limiter set> <service account> warns when a
# namespace-scoped controller declares gpu-inventory and cannot list Pods in
# every namespace. Its GPU usage walk then counts only its own namespace's
# Pods, so GPUs other namespaces hold on the same nodes read as free. Nothing
# in this install grants that read, and an existing install keeps working
# without it, so this warns and never refuses; the controller reports it at
# runtime too (llmsm_pod_access_denied, podListDenied in scaling-manager-controller-status).
llmsm_check_limiter_pod_list() {
    local lset="$1" sa="$2"
    [ "$(llmsm_install_scope)" = "namespace" ] || return 0
    limiter_set_has "$lset" physical || return 0
    case "$(can_i_as "$sa" list pods --all-namespaces)" in
        yes) log_success "The controller can list Pods in every namespace, which the gpu-inventory limiter needs to see other namespaces' GPUs" ;;
        no)
            log_warning "The limiters '${lset}' include gpu-inventory, and $sa cannot list Pods in every namespace."
            log_warning "  Its GPU usage then counts only the Pods of its own namespace: if other namespaces run GPU pods on the same nodes, their GPUs read as FREE and scale-ups can be admitted onto full nodes; if none do, nothing is miscounted. The controller reports it as llmsm_pod_access_denied=1 and podListDenied in its scaling-manager-controller-status ConfigMap."
            log_warning "  Remedy: grant $sa list on pods in every namespace, or bound this namespace with a quota limiter instead of gpu-inventory."
            ;;
        *) log_warning "Could not verify that $sa may list Pods in every namespace (checking needs impersonate permission). Without it the gpu-inventory limiter does not see GPUs other namespaces hold on the same nodes; watch llmsm_pod_access_denied." ;;
    esac
}

# llmsm_check_limiter_node_read <limiter set> <service account> [named|live]
# checks the node read a physical limiter needs.
#
# With gpu-inventory in the set LLMSM_LIMITER names (named, the default), a
# controller that cannot list nodes is REFUSED, with the remedy: the admin
# setup phase grants it. Installing anyway leaves the physical limiter the
# operator asked for unenforced: alone, the controller goes NotReady and the
# fleet never scales up; beside a quota, the controller skips it and only the
# quota bounds.
#
# A live set -- LLMSM_LIMITER unset, the live policy's limiters kept -- is
# WARNED about instead: an upgrade must not be refused for a policy it did not
# write, and the warning carries the same remedy.
llmsm_check_limiter_node_read() {
    local lset="$1" sa="$2" source="${3:-named}" node_read effect
    node_read="$(can_i_as "$sa" list nodes)"
    if limiter_set_has "$lset" physical; then
        if limiter_set_has "$lset" quota; then
            effect="The controller stays Ready but SKIPS the physical limiter: only the quota bounds scaling, and llmsm_constraint_provider_skipped{reason=\"node_read_denied\"} reports it."
        else
            effect="The controller would also go NotReady (its gpu-policy readiness check)."
        fi
        if [ "$node_read" = no ] && [ "$source" = live ]; then
            log_warning "The live policy's limiters '${lset}' include gpu-inventory, which needs the controller to list nodes, and $sa cannot. ${effect}"
            log_warning "  Remedy: have a cluster admin run '$(llmsm_make_cmd setup-prereqs)', which grants node read; or drop gpu-inventory from the policy (re-run with LLMSM_LIMITER naming the limiters to keep)."
            return 0
        fi
        # A run that includes the admin phase applies the role that grants the
        # node read (the manager ClusterRole at cluster scope, node-reader at
        # namespace scope), so asking first refuses the very apply that fixes
        # it -- the rule llmsm_check_kueue_namespace_read follows. On a fresh
        # cluster the ServiceAccount does not even exist yet: `make deploy-scaling-manager
        # SCOPE=cluster LLMSM_LIMITER=gpu-inventory` was refused on every new
        # cluster. Only the controller-only phase (INSTALL_PHASE=scaling-manager) cannot
        # grant it; the post-install check and llmsm_node_access_denied report a
        # grant that did not take.
        if [ "$node_read" != yes ] && [ "${INSTALL_PHASE:-all}" != "scaling-manager" ]; then
            log_info "The gpu-inventory limiter needs the controller to list nodes; this run's admin phase grants it."
            return 0
        fi
        case "$node_read" in
            yes)
                log_success "The controller can list nodes, which the gpu-inventory limiter requires"
                ;;
            no)
                log_error "The limiters '${lset}' include gpu-inventory, which needs the controller to list nodes, and $sa cannot.

A GPU-aware limiter allocates out of per-accelerator pools. Without nodes, a
variant's accelerator never resolves, it is charged to no pool, it receives no
budget, and it NEVER SCALES UP — with nothing in the logs to say why.
${effect}

Node read is cluster-scoped; the admin setup phase grants it: have a cluster
admin run 'make setup-prereqs', then install again. Or install without the
physical limiter: drop gpu-inventory from LLMSM_LIMITER."
                ;;
            *)
                # Cannot impersonate, so this is unanswerable from here. It is not a
                # reason to refuse: the install itself grants the node read, and the
                # controller publishes llmsm_node_access_denied if it turns out not to
                # have it. Say what to watch instead of guessing.
                log_warning "Could not verify that $sa may list nodes (checking needs impersonate permission, which you do not have)."
                log_warning "  The gpu-inventory limiter requires it. After the install, confirm with: kubectl get --raw /metrics | grep llmsm_node_access_denied — 1 means every variant is getting no GPU budget and will not scale up."
                ;;
        esac
    else
        case "$node_read" in
            no)
                # The admin phase grants it, as above: before an install the
                # ServiceAccount does not exist, and this warned on every fresh
                # cluster about a read the same run was about to grant.
                if [ "${INSTALL_PHASE:-all}" != "scaling-manager" ]; then
                    log_info "The controller will list nodes to resolve accelerators; this run's admin phase grants it."
                    return 0
                fi
                log_warning "$sa cannot list nodes. Without a physical limiter this is survivable but degraded: accelerators stay unresolved, so metrics lose the accelerator label and the capacity model cannot reuse learned capacity across variants."
                log_warning "  It stops being fine the moment someone sets a gpu-inventory limiter: every variant would then get no GPU budget and stop scaling up. Watch llmsm_node_access_denied."
                ;;
            unknown)
                log_info "Skipped the controller's node-read check (needs impersonate permission). No limiter is configured, so nothing depends on it yet."
                ;;
        esac
    fi
}

# llmsm_report_scaledobjects says what already registers workloads with the scaling manager. It never
# fails, and the distinction it rests on is the one worth stating, because an earlier
# revision of this file got it backwards and failed the guide's own flow.
#
# Two different questions:
#
#   objects to scale        the llm-d model servers. The scaling manager's PREMISE. None of them is
#                           fatal -- llmsm_report_namespace stops for it -- because the
#                           scenario is "llm-d already works here", and without them
#                           it does not.
#   KEDA ScaledObject CRs   the registrations. The scaling manager's OWN OUTPUT. None yet is the BEST
#                           state: it is exactly what a namespace looks like the
#                           moment before the registration step, which creates them.
#                           Some already present is fine too -- the plan offers
#                           `adopt` per entry, so the operator decides each one.
#
# So a controller with zero ScaledObjects is not a fault. It is either the documented
# mid-flow state between `deploy-scaling-manager` and `scaledobjects-apply`, or a forgotten
# registration step -- and NOTHING IN CLUSTER STATE DISTINGUISHES THEM.
llmsm_report_scaledobjects() {
    local ns="${LLMSM_WATCH_NS:-$LLMSM_NS}" count where in_where one
    if declare -F llmsm_watch_is_multi >/dev/null && llmsm_watch_is_multi; then
        for one in $(llmsm_watch_words); do
            (LLMSM_WATCH_NS="$one"; llmsm_report_scaledobjects) || true
        done
        return 0
    fi
    # A cluster-scoped controller registers workloads in every namespace, never
    # in its own. Counting its own namespace reported "none yet" beside a
    # cluster full of registrations, and the commands it printed, without
    # SCOPE=cluster, scan only the controller's namespace and find nothing.
    where="-n $ns" in_where="in $ns"
    if [ "$(llmsm_install_scope)" = cluster ]; then
        where="-A" in_where="on the cluster"
    fi

    # One list, read three ways below. A list that fails -- Forbidden, the
    # ScaledObject CRD not installed yet, a timeout -- is said and the check
    # stops: folded into a count of 0 it printed "none registered yet" to a
    # tenant who may simply not list ScaledObjects. `|| rc=$?` is the "never
    # fails" contract above: under `set -e` in the caller a non-zero exit would
    # otherwise abort the whole script here.
    #
    # "Registered with the scaling manager" here is a trigger naming the scaler of THIS install
    # (llmsm_names_scaler, with LLMSM_NS): the scaler of another install on the same
    # cluster is another scaler, not a registration with this one.
    #
    # The reason quoted is kubectl's own error line, not stderr's first line:
    # client-go logs discovery trouble (E1028 ... memcache.go ... could not get
    # resource list for external.metrics.k8s.io) ahead of it, and quoting that
    # named an unrelated API as the reason.
    local so rc=0 count
    # shellcheck disable=SC2086 # $where is one or two words by construction
    kc_capture get scaledobject $where -o json || rc=$?
    if [ "$rc" -ne 0 ]; then
        so=$(printf '%s\n' "$KC_ERR" | grep -m1 -E '^(error|Error from server)' || true)
        [ -n "$so" ] || so=$(printf '%s\n' "$KC_ERR" | grep -v '^[[:space:]]*$' | tail -n1 || true)
        log_warning "  ScaledObjects: could not list them $in_where (${so:-kubectl exited $rc}) -- not checked."
        return 0
    fi
    so="$KC_OUT"
    count=$(printf '%s' "$so" | jq -r --arg ns "$LLMSM_NS" "$LLMSM_SCALER_ADDRESS_JQ"'
        [ .items[]?
          | select(any(.spec.triggers[]?; .metadata.scalerAddress | llmsm_names_scaler($ns))) ] | length' 2>/dev/null) || count=""
    case "$count" in
        ''|*[!0-9]*)
            log_warning "  ScaledObjects: could not list them $in_where (kubectl did not return a readable list) -- not checked."
            return 0 ;;
    esac
    if [ "$count" -gt 0 ]; then
        log_success "  ScaledObjects: $count workload(s) $in_where already registered with the scaling manager."
        log_info "    The plan will offer 'adopt' for each, so you choose what happens to them."

        # EXISTING is not the same as WORKING, and the difference is silent.
        #
        # A ScaledObject whose Ready condition is False is one KEDA is not acting
        # on: no HPA is created, so KEDA never calls the external scaler, so the scaling manager
        # is never told the workload exists. Everything else looks right --
        # `kubectl get scaledobject` lists it, the install reports success, the
        # controller is healthy and idle -- and the only trace is a condition
        # nobody reads.
        #
        # Measured on OpenShift: a ScaledObject outlived its Deployment, and when
        # a Deployment of the same name was recreated 18 days later KEDA did not
        # re-reconcile it. It sat at
        #
        #   ScaledObjectCheckFailed: deployments.apps "<name>" not found
        #
        # naming a Deployment that existed and was Ready, with
        # lastTransitionTime unset. An annotation touch did not clear it; only
        # deleting and recreating the ScaledObject did.
        local notready
        notready=$(printf '%s' "$so" | jq -r --arg ns "$LLMSM_NS" "$LLMSM_SCALER_ADDRESS_JQ"'
            .items[]?
            | select(any(.spec.triggers[]?; .metadata.scalerAddress | llmsm_names_scaler($ns)))
            | select([ .status.conditions[]? | select(.type == "Ready" and .status == "True") ] | length == 0)
            | "      " + .metadata.namespace + "/" + .metadata.name + ": " +
              ([ .status.conditions[]? | select(.type == "Ready") | .message // .reason ][0] // "no Ready condition yet")
        ' 2>/dev/null) || notready=""
        if [ -n "$notready" ]; then
            log_warning "    Registered, but NOT Ready — KEDA is not acting on these, so the scaling manager is never called about them and they do not scale:"
            printf '%s\n' "$notready" | while IFS= read -r line; do log_warning "$line"; done
            log_warning "      A ScaledObject that outlived its target stays in this state even after the target comes back. Recreate it:"
            log_warning "      $(llmsm_make_cmd scaledobjects-apply) LLMSM_DEFAULT_SO_PLAN=<plan>   # after deleting the stale one"
        fi
        return 0
    fi
    # The count above is ScaledObjects naming THIS scaler, not every ScaledObject:
    # ones that use another scaler can sit on the same workloads. Saying "none"
    # beside them read as a fresh cluster. The commands go through llmsm_make_cmd
    # so they carry LLMSM_NS: printed without it, apply looks for the scaler in the
    # default namespace and exits 2.
    local others
    others=$(printf '%s' "$so" | jq -r --arg ns "$LLMSM_NS" "$LLMSM_SCALER_ADDRESS_JQ"'
        [ .items[]?
          | select(any(.spec.triggers[]?; .metadata.scalerAddress | llmsm_names_scaler($ns)) | not) ] | length' 2>/dev/null) || others=0
    # Deliberately log_info, not a warning: this is the expected state here.
    log_info "  ScaledObjects: none $in_where registered with the scaling manager yet — registration is the next step, and nothing scales until it runs:"
    if [ "${others:-0}" -gt 0 ]; then
        log_info "    $others other ScaledObject(s) $in_where use another scaler. Any that target these model servers are adopted in the plan (apply: adopt), not registered fresh."
    fi
    log_info "      $(llmsm_make_cmd scaledobjects-plan) LLMSM_DEFAULT_SO_PLAN=scaling-manager-plan.yaml   # then edit it"
    log_info "      $(llmsm_make_cmd scaledobjects-apply) LLMSM_DEFAULT_SO_PLAN=scaling-manager-plan.yaml"
}
