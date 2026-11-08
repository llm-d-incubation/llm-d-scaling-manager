#!/usr/bin/env bash
#
# Common logging and utility helpers for deploy/install.sh.
# Requires vars: BLUE, GREEN, YELLOW, RED, NC.
# Exposes funcs: log_info/log_warning/log_success/log_error,
# containsElement(), should_skip_helm_repo_update().
#

log_info() {
    echo -e "${BLUE}[INFO]${NC} $1" >&2
}

log_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1" >&2
}

log_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1" >&2
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1" >&2
    exit 1
}

# Helm repo update behavior:
# - Default: DO NOT skip (`helm repo update` runs)
# - Opt-in: set `SKIP_HELM_REPO_UPDATE=true` to skip (faster, but requires repo indexes to already exist)
should_skip_helm_repo_update() {
    local skip="${SKIP_HELM_REPO_UPDATE:-false}"
    echo "$skip"
}

# Used to check if the environment variable is in a list
containsElement() {
    local e match="$1"
    shift
    for e; do [[ "$e" == "$match" ]] && return 0; done
    return 1
}

# kc_capture runs `kubectl "$@"` and keeps its two streams apart: KC_OUT is
# stdout, KC_ERR is stderr, and the status is kubectl's.
#
# A read that merged them with 2>&1 parsed whatever kubectl printed on stderr as
# the answer. kubectl prints warnings there on SUCCESS -- a deprecated API, an
# admission webhook, a client-side throttling notice -- so a label read came
# back as "Warning: ...", and a count read as a YAML document that yq could not
# parse. Match NotFound and Forbidden against KC_ERR, read data from KC_OUT only.
#
# Sets variables rather than echoing, so the caller stays in its own shell: call
# it as `kc_capture get ... || rc=$?`, never inside `$( )`.
kc_capture() {
    local errf rc=0
    KC_OUT="" KC_ERR=""
    errf="$(mktemp)" || { KC_ERR="mktemp failed"; return 125; }
    KC_OUT="$(kubectl "$@" 2>"$errf")" || rc=$?
    KC_ERR="$(cat "$errf")"
    rm -f "$errf"
    return "$rc"
}

# kc_not_found reports whether stderr text $1 is the API server saying the
# object does not exist -- and nothing else (Forbidden, timeouts, a webhook
# error) is.
kc_not_found() {
    case "$1" in
        *NotFound*|*"not found"*) return 0 ;;
    esac
    return 1
}

# llmsm_check_so_name refuses a ScaledObject name KEDA would refuse later. KEDA
# names the HPA keda-hpa-<ScaledObject> and an HPA name holds 63 characters, so a
# ScaledObject name holds 54 (KEDA v2.21.0, scaledobject_webhook.go). Stopped here
# it is a line with the name in it; past here it is a ScaledObject that never
# gets an HPA. Only for objects this repository generates, which never set
# spec.advanced.horizontalPodAutoscalerConfig.name.
llmsm_check_so_name() {
    [ "${#1}" -le 54 ] || log_error "ScaledObject name $1 is ${#1} characters, and KEDA takes 54: keda-hpa-<name> must fit in 63. Shorten the workload or pool name, or set spec.advanced.horizontalPodAutoscalerConfig.name on your own ScaledObject."
}

# llmsm_make_cmd echoes the make command that repeats THIS install's shape for
# target $1; any VAR=value arguments after it are put in front.
#
# A remedy printed as `NAMESPACE=$LLMSM_NS make setup-prereqs` was wrong for two of
# the three shapes: in the out-of-reach one (controller in llmsm-<ns>, managing
# <ns>) it named the controller's namespace as the one to manage, and in
# cluster scope it dropped SCOPE=cluster, so the admin who ran it set up a
# namespace-scoped install nobody asked for.
#
#   namespace scope, self-managed   NAMESPACE=<ns> make <target>
#   namespace scope, out of reach   NAMESPACE=<managed> make <target> LLMSM_NS=<ctl> LLMSM_WATCH_NS=<managed>
#   cluster scope                   make <target> SCOPE=cluster LLMSM_NS=<ctl>
llmsm_make_cmd() {
    local target="$1" pre="" kv
    shift
    for kv in "$@"; do pre="$pre$kv "; done
    if [ "$(llmsm_install_scope)" = cluster ]; then
        printf '%smake %s SCOPE=cluster LLMSM_NS=%s\n' "$pre" "$target" "$LLMSM_NS"
    elif llmsm_watch_is_multi; then
        printf '%smake %s LLMSM_NS=%s LLMSM_WATCH_NS=%s\n' "$pre" "$target" "$LLMSM_NS" "$LLMSM_WATCH_NS"
    elif [ -n "${LLMSM_WATCH_NS:-}" ] && [ "$LLMSM_WATCH_NS" != "$LLMSM_NS" ]; then
        printf '%sNAMESPACE=%s make %s LLMSM_NS=%s LLMSM_WATCH_NS=%s\n' "$pre" "$LLMSM_WATCH_NS" "$target" "$LLMSM_NS" "$LLMSM_WATCH_NS"
    else
        printf '%sNAMESPACE=%s make %s\n' "$pre" "$LLMSM_NS" "$target"
    fi
}

# --- Several managed namespaces ----------------------------------------------
#
# LLMSM_WATCH_NS names the namespaces a namespace-scoped controller MANAGES when
# they differ from the one it runs in: one namespace, a comma-separated list
# (team-a,team-b), or `auto` -- every namespace with llm-d model servers or an
# InferencePool, found once at install time and written into the Deployment.
# The controller caches exactly those namespaces and its own, and the prereqs
# phase gives it its manager Role in each, nowhere else.
#
# The single-namespace steps read `${LLMSM_WATCH_NS:-$LLMSM_NS}`. Each one that acts
# on a namespace runs once per listed namespace with LLMSM_WATCH_NS set to it, the
# way llmsm_check_managed_namespaces already ran them for a cluster-scoped install.

# llmsm_watch_list echoes the managed namespaces, one per line: the LLMSM_WATCH_NS
# list, else LLMSM_NS.
llmsm_watch_list() {
    if [ -n "${LLMSM_WATCH_NS:-}" ]; then
        printf '%s\n' "$LLMSM_WATCH_NS" | tr ',' '\n' | sed 's/^[[:space:]]*//; s/[[:space:]]*$//' | grep -v '^$' || true
    else
        printf '%s\n' "$LLMSM_NS"
    fi
}

# llmsm_watch_words echoes the managed namespaces on one line, space-separated.
llmsm_watch_words() {
    llmsm_watch_list | paste -sd' ' -
}

# llmsm_watch_is_multi reports whether LLMSM_WATCH_NS lists more than one namespace.
llmsm_watch_is_multi() {
    case "${LLMSM_WATCH_NS:-}" in *,*) ;; *) return 1 ;; esac
    [ "$(llmsm_watch_list | grep -c .)" -gt 1 ]
}

# llmsm_discover_watch_namespaces echoes every namespace with llm-d model servers
# or an InferencePool, one per line, sorted. Empty when there are none.
llmsm_discover_watch_namespaces() {
    {
        if declare -F llmsm_namespaces_with_model_servers >/dev/null; then
            llmsm_namespaces_with_model_servers 2>/dev/null || true
        fi
        local kind
        for kind in inferencepools.inference.networking.k8s.io inferencepools.inference.networking.x-k8s.io; do
            kubectl get "$kind" -A -o jsonpath='{range .items[*]}{.metadata.namespace}{"\n"}{end}' 2>/dev/null || true
        done
    } | grep -v '^$' | sort -u || true
}

# llmsm_excluded_namespaces echoes every namespace annotated
# scaling-manager.llmd.ai/exclude=true, one per line. Empty when Namespaces cannot be read.
llmsm_excluded_namespaces() {
    kubectl get namespaces -o json 2>/dev/null \
        | jq -r '.items[] | select((.metadata.annotations // {})["scaling-manager.llmd.ai/exclude"] == "true") | .metadata.name' 2>/dev/null || true
}

# llmsm_managed_by_others echoes `<namespace> <controller namespace>` for every
# namespace another scaling manager controller (not the one in LLMSM_NS) manages by name: a
# self-managed controller its own, an out-of-reach one each of its
# WATCH_NAMESPACE list. Read from the Deployments' LLMSM_WATCH_NAMESPACE env, as
# the installer writes it. Empty when Deployments cannot be listed.
llmsm_managed_by_others() {
    kubectl get deploy -A -l app.kubernetes.io/name=scaling-manager -o json 2>/dev/null \
        | jq -r --arg own "$LLMSM_NS" '.items[]
            | select(.metadata.namespace != $own)
            | .metadata.namespace as $ns
            | ([.spec.template.spec.containers[]? | select(.name == "manager") | .env[]? | select(.name == "LLMSM_WATCH_NAMESPACE") | .value // ""][0] // "") as $w
            | (if $w == "" or $w == "$(POD_NAMESPACE)" then $ns else $w end)
            | split(",")[] | select(. != "") | . + " " + $ns' 2>/dev/null || true
}

# llmsm_resolve_watch_namespaces settles LLMSM_WATCH_NS: `auto` becomes the list it
# finds, a list is trimmed and de-duplicated, and every name is checked to be a
# namespace name. `auto` skips a namespace annotated scaling-manager.llmd.ai/exclude=true
# and one another controller already manages, each with a warning. Refuses a
# list on a cluster-scoped install (it manages every namespace already), a list
# that holds LLMSM_NS (the controller runs outside the namespaces it manages; LLMSM_NS
# defaults to scaling-manager-system), and `auto` that finds nothing
# or may not look.
llmsm_resolve_watch_namespaces() {
    [ -n "${LLMSM_WATCH_NS:-}" ] || return 0
    local list n
    if [ "$LLMSM_WATCH_NS" = auto ]; then
        if ! kubectl auth can-i list deployments -A >/dev/null 2>&1; then
            log_error "LLMSM_WATCH_NS=auto finds the namespaces running llm-d by listing workloads cluster-wide, which is not permitted for you. Name them instead: LLMSM_WATCH_NS=<namespace>,<namespace>"
        fi
        local found excluded others n owner
        found="$(llmsm_discover_watch_namespaces | grep -vxF -- "$LLMSM_NS" || true)"
        # A namespace that opted out (scaling-manager.llmd.ai/exclude: "true") is never
        # taken by discovery; name it to manage it anyway.
        excluded="$(llmsm_excluded_namespaces)"
        # Nor one another install already manages: two controllers deciding for
        # the same workloads is the conflict check_single_installation refuses,
        # and discovery must not create it silently.
        others="$(llmsm_managed_by_others)"
        list=""
        for n in $found; do
            if printf '%s\n' "$excluded" | grep -qxF -- "$n"; then
                log_warning "LLMSM_WATCH_NS=auto: skipping $n, annotated scaling-manager.llmd.ai/exclude=true. Name it in LLMSM_WATCH_NS to manage it anyway."
                continue
            fi
            owner="$(printf '%s\n' "$others" | awk -v n="$n" '$1 == n {print $2; exit}')"
            if [ -n "$owner" ]; then
                log_warning "LLMSM_WATCH_NS=auto: skipping $n, already managed by the controller in $owner. Name it in LLMSM_WATCH_NS only after removing it from that controller."
                continue
            fi
            list="${list:+$list,}$n"
        done
        [ -n "$list" ] || log_error "LLMSM_WATCH_NS=auto found no namespace with llm-d model servers or an InferencePool that is not excluded or managed by another controller. Install llm-d first, or name the namespaces: LLMSM_WATCH_NS=<namespace>,<namespace>"
        LLMSM_WATCH_NS_SOURCE=discovered
    else
        list="$(llmsm_watch_list | awk '!seen[$0]++' | paste -sd, -)"
        LLMSM_WATCH_NS_SOURCE=named
    fi
    for n in $(printf '%s' "$list" | tr ',' ' '); do
        case "$n" in
            *[!a-z0-9-]*|-*|*-) log_error "LLMSM_WATCH_NS: '$n' is not a namespace name (lowercase letters, digits and '-')." ;;
        esac
    done
    LLMSM_WATCH_NS="$list"
    export LLMSM_WATCH_NS LLMSM_WATCH_NS_SOURCE
    llmsm_watch_is_multi || return 0
    if [ "$(llmsm_install_scope)" != namespace ]; then
        log_error "LLMSM_WATCH_NS lists several namespaces, which only a namespace-scoped install takes (LLMSM_SCOPE=namespace). A cluster-scoped controller manages every namespace."
    fi
    # The controller in one of the namespaces it manages would be owned by that
    # namespace's tenant, who could then lift any bound on the others.
    if llmsm_watch_list | grep -qxF -- "$LLMSM_NS"; then
        log_error "LLMSM_WATCH_NS ($LLMSM_WATCH_NS) includes $LLMSM_NS, the namespace the controller runs in. A controller that manages several namespaces runs in a namespace of its own: set LLMSM_NS to one that is not in the list."
    fi
    # gpu-inventory alone would make the cluster's free GPUs this controller's
    # budget, and every namespace-scoped controller would plan the same free
    # GPUs. Beside a quota it only places; the quota is the budget.
    local lset
    if [ -n "${LLMSM_LIMITER:-}" ] && lset="$(limiter_types_of "$LLMSM_LIMITER" 2>/dev/null)" \
        && limiter_set_has "$lset" physical && ! limiter_set_has "$lset" quota; then
        log_error "LLMSM_LIMITER=$LLMSM_LIMITER on a controller that manages a list of namespaces: gpu-inventory alone is refused, because two namespace-scoped controllers would plan the same free GPUs. Bound the budget with a quota and keep gpu-inventory for placement: LLMSM_LIMITER=gpu-inventory,quota LLMSM_QUOTA_SCOPE=cluster LLMSM_QUOTAS=<product>=<gpus>"
    fi
}

# llmsm_watch_is_listed reports whether LLMSM_WATCH_NS is a list or `auto` -- the
# shape where NAMESPACE names no single managed namespace.
llmsm_watch_is_listed() {
    case "${LLMSM_WATCH_NS:-}" in *,*|auto) return 0 ;; esac
    # `auto` that found a single namespace is still one the caller did not name.
    [ "${LLMSM_WATCH_NS_SOURCE:-}" = discovered ]
}

# llmsm_adopt_live_watch_namespaces sets LLMSM_WATCH_NS, for the commands that plan
# and apply ScaledObjects, to the namespaces the installed controller in LLMSM_NS
# manages, when the caller named none (or said `auto`) and named no NAMESPACE.
# Read from the Deployment, so a plan registers exactly what the controller
# watches: a namespace found later, and not watched, is not registered.
llmsm_adopt_live_watch_namespaces() {
    [ "$(llmsm_install_scope)" = namespace ] || return 0
    [ -z "${NAMESPACE_EXPLICIT:-}" ] || return 0
    case "${LLMSM_WATCH_NS:-}" in ''|auto) ;; *) return 0 ;; esac
    local live
    live="$(kubectl get deploy scaling-manager-controller-manager -n "$LLMSM_NS" \
        -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="LLMSM_WATCH_NAMESPACE")].value}' 2>/dev/null || true)"
    # Only a list, or for `auto` any name: a single out-of-reach namespace with
    # LLMSM_WATCH_NS unset keeps the behaviour it had.
    case "$live" in
        ''|'$(POD_NAMESPACE)'|"$LLMSM_NS") return 0 ;;
        *,*) ;;
        *) [ "${LLMSM_WATCH_NS:-}" = auto ] || return 0 ;;
    esac
    LLMSM_WATCH_NS="$live"
    export LLMSM_WATCH_NS
}

# llmsm_install_scope echoes the install scope: "cluster" or "namespace".
#
# The two differ in WHO CAN INSTALL as much as in what the controller reads:
#
#   cluster     manages every namespace. Its manager ClusterRole reads every
#               namespace, and nodes.
#   namespace   manages ONE namespace. Its manager role is a namespaced Role, so
#               it reads nothing outside — but it still carries cluster-scoped
#               RBAC for the things no Role can express: the gpu-inventory
#               limiter (nodes), authenticated metrics (TokenReview) and EPP
#               metrics (nonResourceURLs).
#
# Neither scope is installable by a namespace admin in one command, and pretending
# otherwise was the bug: the PHASE split is what makes a tenant install real. An
# admin runs the prereqs phase once for a namespace; its owner installs and
# upgrades the controller from then on.
#
# LLMSM_SCOPE selects it. It is the SCENARIO selector: which of the scaling manager's install shapes
# this is, and therefore what the preflight is entitled to expect.
#
# The default is `namespace` EVERYWHERE, because that is the supported scenario --
# llm-d already working in one namespace, the scaling manager added to it. It used to be inferred
# from the platform ("namespace" on OpenShift, "cluster" elsewhere), which was not a
# decision anybody made: the comment here said it "preserves the historical
# inference". The effect was that `deploy/install.sh` run directly on Kubernetes or
# kind selected the CLUSTER scenario -- a different shape, with different
# preconditions -- without the caller asking for it. `make` never did, because
# Makefile's SCOPE already defaults to namespace.
llmsm_install_scope() {
    local scope="${LLMSM_SCOPE:-}"
    if [ -z "$scope" ]; then
        scope="namespace"

        # The old platform inference, kept for reference and no longer reached. It
        # is left in place deliberately rather than deleted: it records what the
        # default used to be, for anyone reading a bug report from before this
        # change.
        #
        #   if [ "${ENVIRONMENT:-}" = "openshift" ]; then
        #       scope="namespace"
        #   else
        #       scope="cluster"
        #   fi
    fi
    case "$scope" in
        cluster|namespace) echo "$scope" ;;
        *) log_error "LLMSM_SCOPE must be 'cluster' or 'namespace', got '$scope'" ;;
    esac
}

# llmsm_warn_unsupported_scope says so, once, when the caller has selected a scenario
# that is not the one being supported right now.
#
# Cluster scope is NOT disabled -- it is left working, and anyone for whom it works
# should keep using it. What it does not get is the preflight's namespace-centric
# expectations, which are written for scenario 1 and would be wrong here: in a
# cluster-scoped install the namespaces scaling manager manages need not exist yet, need not hold
# model servers yet, and are discovered rather than named.
llmsm_warn_unsupported_scope() {
    [ "$(llmsm_install_scope)" = "cluster" ] || return 0
    log_warning "LLMSM_SCOPE=cluster — one controller for every namespace. This is WIP and not the scenario currently supported end to end."
    log_warning "  It is not disabled: if it works for you, keep using it. The checks below run once for each namespace with llm-d model servers or an InferencePool."
    log_warning "  To install it without them:  SKIP_CHECKS=true"
}

# The limiter set an install declares. Here rather than in limiter_policy.sh
# because the prerequisite checks read it too, and they are sourced, and
# tested, with only this file beside them.
#
# limiter_types_of <list> echoes the limiter set an LLMSM_LIMITER value names,
# normalised: space-separated, the physical limiter first, each kind once, and
# `none` for an empty set. The list separates entries with commas or spaces:
# `gpu-inventory,quota`.
#
# Every declared limiter is built and a decision must fit all of them, so the
# list is a SET of bounds: the physical one (the GPUs that exist) and the quota
# (an allowance). At most one physical entry, under either spelling, which the
# controller also refuses; `none` beside anything else is a contradiction.
# Refusals go through log_error.
limiter_types_of() {
    local list="${1:-none}" t physical="" quota="" saw_none=0 n=0 reset_glob=1
    case "$-" in *f*) reset_glob=0 ;; esac
    set -f
    for t in $(printf '%s' "$list" | tr ',' ' '); do
        n=$((n + 1))
        case "$t" in
            none) saw_none=1 ;;
            gpu-inventory|inventory)
                if [ -n "$physical" ] && [ "$physical" != "$t" ]; then
                    [ "$reset_glob" = 1 ] && set +f
                    log_error "LLMSM_LIMITER names the physical limiter twice ('$physical' and '$t'). There is one physical cluster; declare it once."
                fi
                physical="$t" ;;
            quota) quota="quota" ;;
            *)
                [ "$reset_glob" = 1 ] && set +f
                log_error "LLMSM_LIMITER entries must be 'none', 'gpu-inventory' or 'quota' (a comma-separated list composes them: gpu-inventory,quota), got '$t' in '$list'" ;;
        esac
    done
    [ "$reset_glob" = 1 ] && set +f
    if [ "$saw_none" = 1 ] && { [ -n "$physical" ] || [ -n "$quota" ]; }; then
        log_error "LLMSM_LIMITER='$list' names 'none' beside a limiter. Name the limiters, or none alone."
    fi
    if [ "$n" -eq 0 ] || { [ -z "$physical" ] && [ -z "$quota" ]; }; then
        echo none
        return 0
    fi
    printf '%s\n' "$(printf '%s %s' "$physical" "$quota" | sed 's/^ *//; s/ *$//')"
}

# limiter_set_has <set> <kind> reports whether a limiter_types_of set holds the
# kind: `physical` (either spelling) or `quota`.
limiter_set_has() {
    case "$2" in
        physical) case " $1 " in *" gpu-inventory "*|*" inventory "*) return 0 ;; esac ;;
        quota) case " $1 " in *" quota "*) return 0 ;; esac ;;
    esac
    return 1
}

# llmsm_limiter_set echoes the limiter set this install declares: LLMSM_LIMITER,
# normalised by limiter_types_of. Unset reads as none here; the install itself
# keeps the live policy's limiters when LLMSM_LIMITER is not given.
llmsm_limiter_set() {
    limiter_types_of "${LLMSM_LIMITER:-none}"
}

# llmsm_scope_is_tenant reports whether this install manages ONE namespace.
#
# It says nothing about what the install creates. That is a question about the
# RENDERED overlay — ask llmsm_rendered_kinds, never this. Both scopes carry
# cluster-scoped RBAC; what makes a namespace-scoped install a tenant's is that
# an admin created that RBAC in the prereqs phase, not the scope name.
llmsm_scope_is_tenant() {
    [ "$(llmsm_install_scope)" = "namespace" ]
}

# llmsm_scaler_namespaces echoes every namespace running a scaling manager external-scaler
# Service, one per line -- that is, every install a KEDA trigger could name.
#
# The Service is the right thing to look for rather than the Deployment: the
# address in a ScaledObject resolves to a Service, so a Service is what makes an
# address answerable, and one without a ready controller behind it is a
# different fault with a different fix.
#
# Empty output means "none found OR not allowed to look", and the two are
# different problems. Callers that act on the answer must say which they assumed.
llmsm_scaler_namespaces() {
    local out
    if out=$(kubectl get svc -A -l app.kubernetes.io/name=scaling-manager \
        --field-selector metadata.name=scaling-manager-external-scaler \
        -o jsonpath='{range .items[*]}{.metadata.namespace}{"\n"}{end}' 2>/dev/null); then
        printf '%s\n' "$out" | grep -v '^[[:space:]]*$' | sort -u
        return 0
    fi
    # No cluster-wide list. The one namespace we can name is still worth checking
    # -- and a Forbidden answer there is said, not folded into "none".
    local rc=0
    llmsm_scaler_svc_state "$LLMSM_NS" || rc=$?
    case "$rc" in
        0) echo "$LLMSM_NS" ;;
        1) : ;;
        *) log_warning "Cannot list Services cluster-wide, and cannot verify (Forbidden or unreadable) a scaling-manager-external-scaler Service in $LLMSM_NS either: ${LLMSM_SCALER_SVC_ERR:-no detail}. Pass LLMSM_NS=<namespace> to name the install." ;;
    esac
    return 0
}

# Which ScaledObject trigger addresses name the scaler of the install in a
# namespace <ns>: scaling-manager-external-scaler.<ns>, scaling-manager-external-scaler.<ns>.svc or
# scaling-manager-external-scaler.<ns>.svc.cluster.local, each alone or followed by
# :<port>, compared without regard to case (DNS does not regard it). Nothing
# else: not scaling-manager-external-scaler.<ns>-other, another install, and not
# scaling-manager-external-scaler.<ns>.example.com, a host outside the cluster.
#
# One rule, written twice: LLMSM_SCALER_ADDRESS_JQ for the jq programs, which
# put it in front of their own (jq -r "$LLMSM_SCALER_ADDRESS_JQ"'...'), and
# llmsm_names_scaler for shell callers. hack/check-scaledobject-rewire.sh runs
# both over one table of addresses and fails when they disagree.
#
# In jq, `ADDRESS | llmsm_names_scaler($ns)` is the test, and `llmsm_scaler_ns` is
# the namespace an address names, or null when it names none.
#
# Both are total: an empty or null address -- a cpu or prometheus trigger has
# no scalerAddress -- names no scaler, and is not an error. An error there
# aborted the whole program, so one ordinary trigger made every ScaledObject
# unreadable.
LLMSM_SCALER_ADDRESS_JQ='def llmsm_names_scaler($ns): ((. // "") | ascii_downcase) as $a | ("scaling-manager-external-scaler." + (($ns // "") | ascii_downcase)) as $p | any($p, $p + ".svc", $p + ".svc.cluster.local"; . as $h | $a == $h or ($a | startswith($h + ":"))); def llmsm_scaler_ns: ((. // "") | ascii_downcase) as $a | if ($a | startswith("scaling-manager-external-scaler.")) then (($a | ltrimstr("scaling-manager-external-scaler.") | split(".")[0] // "") | split(":")[0] // "") as $ns | (if ($ns != "") and ($a | llmsm_names_scaler($ns)) then $ns else null end) else null end;'

# llmsm_names_scaler succeeds when address $2 names the scaler in namespace $1,
# by the rule above.
llmsm_names_scaler() {
    # tr, not ${2,,}: macOS still ships bash 3.2.
    local a p h
    a="$(printf '%s' "$2" | tr '[:upper:]' '[:lower:]')"
    p="scaling-manager-external-scaler.$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')"
    for h in "$p" "$p.svc" "$p.svc.cluster.local"; do
        [ "$a" = "$h" ] && return 0
        case "$a" in "$h":*) return 0 ;; esac
    done
    return 1
}

# llmsm_scaler_ns echoes the namespace address $1 names a scaler in, by the rule
# above; nothing, and a non-zero exit, when it names none.
llmsm_scaler_ns() {
    local a ns
    a="$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')"
    case "$a" in scaling-manager-external-scaler.*) ;; *) return 1 ;; esac
    ns="${a#scaling-manager-external-scaler.}"
    ns="${ns%%[.:]*}"
    [ -n "$ns" ] && llmsm_names_scaler "$ns" "$a" || return 1
    printf '%s\n' "$ns"
}

# llmsm_scaler_svc_state reports on the scaling-manager-external-scaler Service in namespace $1:
# 0 it is there, 1 the API server says it is not, 2 could not look -- Forbidden,
# or any other error. On 2, LLMSM_SCALER_SVC_ERR holds kubectl's first error line.
#
# Three answers because two of them were being read as one. A tenant whose
# controller runs in a namespace they may not read gets Forbidden for a Service
# that is there and answering, and treating that as "no scaler" either refused a
# correct LLMSM_NS or rewrote a working address. Call it directly, not in $( ):
# the variable is the caller's.
llmsm_scaler_svc_state() {
    local rc=0
    LLMSM_SCALER_SVC_ERR=""
    kc_capture get svc scaling-manager-external-scaler -n "$1" -o name || rc=$?
    [ "$rc" -eq 0 ] && return 0
    kc_not_found "$KC_ERR" && return 1
    LLMSM_SCALER_SVC_ERR="$(printf '%s\n' "$KC_ERR" | grep -v '^[[:space:]]*$' | head -1)"
    return 2
}

# llmsm_any_pod_ready reports whether any pod matching selector $2 in namespace $1
# is READY. Pass -A for cluster-wide.
#
# `kubectl get pods | grep -q Running` is not this check, and the difference is
# the one that matters when something is wrong: a pod stuck at "0/1 Running" --
# crash-looping, or failing a probe -- matches Running and is not ready. On a
# multi-pod list it is worse, because it answers yes when ANY line says Running,
# including one belonging to a replica that is fine while the one you care about
# is not.
#
# The Ready CONDITION is what "is it up" means, so that is what this reads. A
# failed call returns non-zero rather than "not ready": being unable to look is
# not the same as looking and finding nothing, and a caller that gates on this
# must be able to tell them apart.
llmsm_any_pod_ready() {
    local scope="$1" selector="$2" out
    local path='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}'
    if [ "$scope" = "-A" ]; then
        out=$(kubectl get pods -A -l "$selector" -o jsonpath="$path" 2>/dev/null) || return 2
    else
        out=$(kubectl get pods -n "$scope" -l "$selector" -o jsonpath="$path" 2>/dev/null) || return 2
    fi
    printf '%s\n' "$out" | grep -qx 'True'
}

# llmsm_controller_rollout_ready waits up to $3 for Deployment $1 (a kubectl
# name, deployment.apps/<name>) in namespace $2 to finish its rollout, and then
# requires a Ready replica. `kubectl rollout status` alone says "successfully
# rolled out" at once for a Deployment scaled to 0: every one of its zero
# replicas is updated and available. A re-run of make deploy-scaling-manager over a
# controller someone had scaled to 0 then reported it running and ready.
#
# Returns 0 when the rollout finished and at least one replica is Ready, 1
# otherwise. On 1, LLMSM_CONTROLLER_NOT_READY says why: "scaled-to-zero" when
# spec.replicas is 0, "no-ready-replica" when the rollout passed with none
# Ready, "unreadable" when the Deployment could not be read after the rollout,
# and "rollout" when the rollout itself failed or timed out. Rollout output
# goes to stdout, as kubectl prints it; callers that want it quiet redirect.
llmsm_controller_rollout_ready() {
    local deploy="$1" ns="$2" timeout="$3" counts spec ready
    LLMSM_CONTROLLER_NOT_READY=""
    if ! kubectl rollout status "$deploy" -n "$ns" --timeout="$timeout"; then
        LLMSM_CONTROLLER_NOT_READY=rollout
        return 1
    fi
    counts=$(kubectl get "$deploy" -n "$ns" \
        -o jsonpath='{.spec.replicas}{"/"}{.status.readyReplicas}' 2>/dev/null) || {
        LLMSM_CONTROLLER_NOT_READY=unreadable
        return 1
    }
    # Split on "/", not whitespace: either side may be empty, and `read` would
    # move a lone readyReplicas into the replicas field.
    spec="${counts%%/*}"
    ready="${counts#*/}"
    [ "$ready" != "$counts" ] || ready=""
    # An absent spec.replicas is the API default of 1; an absent readyReplicas
    # is 0 (the status omits zero counts).
    spec="${spec:-1}"
    ready="${ready:-0}"
    if [ "$spec" = "0" ]; then
        LLMSM_CONTROLLER_NOT_READY=scaled-to-zero
        return 1
    fi
    case "$ready" in
        ''|*[!0-9]*|0)
            LLMSM_CONTROLLER_NOT_READY=no-ready-replica
            return 1
            ;;
    esac
    return 0
}

# llmsm_pods_exist reports whether any pod matches selector $2 in namespace $1
# (-A for cluster-wide), regardless of readiness.
#
# Pairs with llmsm_any_pod_ready to separate "it is here and unhealthy" from "it is
# not here". Only the first justifies telling someone their cluster is broken;
# the second usually means we looked in the wrong place.
llmsm_pods_exist() {
    local scope="$1" selector="$2" out
    if [ "$scope" = "-A" ]; then
        out=$(kubectl get pods -A -l "$selector" -o name 2>/dev/null) || return 2
    else
        out=$(kubectl get pods -n "$scope" -l "$selector" -o name 2>/dev/null) || return 2
    fi
    [ -n "$(printf '%s' "$out" | tr -d '[:space:]')" ]
}

# llmsm_scaler_has_endpoints reports whether the external-scaler Service in $1 has a
# ready backend -- a controller actually listening, not merely an address that
# resolves.
#
# A Service with no Endpoints is the shape a half-removed install leaves behind,
# and it is indistinguishable from a live one by name alone. Anything CHOOSING
# between installs must use this; anything deciding whether to leave another
# install's object alone must not, because there the Service existing at all is
# reason enough to keep hands off.
#
# EndpointSlice is read first (Endpoints is deprecated and unserved in newer
# clusters) with Endpoints as the fallback, so this works either side of that
# change rather than silently reporting "no backend" on every Service.
llmsm_scaler_has_endpoints() {
    local ns="$1" out
    if out=$(kubectl get endpointslice -n "$ns" -l kubernetes.io/service-name=scaling-manager-external-scaler \
        -o jsonpath='{range .items[*]}{range .endpoints[*]}{.addresses[0]}{"\n"}{end}{end}' 2>/dev/null); then
        [ -z "$(printf '%s' "$out" | tr -d '[:space:]')" ] || return 0
    fi
    out=$(kubectl get endpoints scaling-manager-external-scaler -n "$ns" \
        -o jsonpath='{.subsets[*].addresses[*].ip}' 2>/dev/null) || return 1
    [ -n "$(printf '%s' "$out" | tr -d '[:space:]')" ]
}

# --- Where KEDA runs, for the external-scaler NetworkPolicy.
#
# Two readers, each returning 0 with an answer, 1 for "looked, and there is
# none", and 2 for "could not look". 2 is never folded into 1: a Forbidden read
# reported as "none found" sent the install to keda-system on a cluster whose
# KEDA runs in `keda`, and KEDA was then refused by the policy written for it.
# Shared by the install (llmsm_resolve_scaler_keda_namespace) and the post-deploy
# verify (llmsm_verify_scaler_networkpolicy), which must agree on what "KEDA is
# there" means.

# llmsm_keda_apiservice_ns echoes the namespace of the Service behind the
# v1beta1.external.metrics.k8s.io APIService, when that Service is KEDA's.
#
# The strongest answer there is: the APIService is cluster-scoped and written by
# whoever installed KEDA, so no tenant can plant it, and KEDA's metrics server
# and operator are installed into one namespace by the Helm chart, the operator
# manifests and OpenShift's Custom Metrics Autoscaler alike. Returns 3 when the
# APIService is served by something that is not KEDA (prometheus-adapter); the
# Service it names is then echoed as "<namespace>/<name>" for the message.
llmsm_keda_apiservice_ns() {
    local rc=0 ns name
    kc_capture get apiservice v1beta1.external.metrics.k8s.io \
        -o jsonpath='{.spec.service.namespace}{" "}{.spec.service.name}' || rc=$?
    if [ "$rc" -ne 0 ]; then
        # Anchored on the API server status "(NotFound)", never the bare word:
        # a Forbidden message naming a namespace notfound-prod is not absent.
        printf '%s' "$KC_ERR" | grep -qF '(NotFound)' && return 1
        return 2
    fi
    read -r ns name <<< "$KC_OUT"
    [ -n "$ns" ] || return 1
    case "$name" in
        *keda*) printf '%s\n' "$ns"; return 0 ;;
        *) printf '%s/%s\n' "$ns" "$name"; return 3 ;;
    esac
}

# llmsm_scaler_np_keda_ns echoes the namespace an existing scaling-manager-external-scaler
# NetworkPolicy in $1 admits to :9090. Returns 1 when there is no such policy,
# 2 when it could not be read, and 4 when it EXISTS but admits :9090 from
# anything other than exactly one namespace -- present, so not "no policy".
llmsm_scaler_np_keda_ns() {
    local ns="$1" rc=0 found
    kc_capture get networkpolicy scaling-manager-external-scaler -n "$ns" \
        -o jsonpath='{range .spec.ingress[*]}{range .ports[*]}{.port}{" "}{end}{"|"}{range .from[*]}{.namespaceSelector.matchLabels.kubernetes\.io/metadata\.name}{" "}{end}{"\n"}{end}' || rc=$?
    if [ "$rc" -ne 0 ]; then
        # Anchored, as in llmsm_keda_apiservice_ns.
        printf '%s' "$KC_ERR" | grep -qF '(NotFound)' && return 1
        return 2
    fi
    found="$(printf '%s\n' "$KC_OUT" | awk -F'|' '{
            n = split($1, p, " "); hit = 0
            for (i = 1; i <= n; i++) if (p[i] == "9090") hit = 1
            if (hit) { m = split($2, f, " "); for (j = 1; j <= m; j++) print f[j] }
        }' | sort -u)"
    [ -n "$found" ] || return 4
    [ "$(printf '%s\n' "$found" | wc -l | tr -d ' ')" = 1 ] || return 4
    printf '%s\n' "$found"
}

# There is no third reader. "Where a Deployment named keda-operator runs" was
# one, and it was plantable: any tenant can create a Deployment of that name, and
# the answer then pointed the policy at their namespace (opening :9090 to them)
# or made it "ambiguous" and stopped the install.

# LLMSM_SHARED_CLUSTER_ROLE_BINDINGS are the cluster-scoped bindings every overlay
# applies under fixed names. They are renamed per install so two installs cannot
# take them from one another.
LLMSM_SHARED_CLUSTER_ROLE_BINDINGS=(
    scaling-manager-epp-metrics-reader-role-binding
    scaling-manager-manager-cluster-monitoring-view
    scaling-manager-manager-rolebinding
    scaling-manager-metrics-auth-rolebinding
    scaling-manager-metrics-reader-rolebinding
    # scaling-manager-prometheus-cluster-monitoring-view was here. The binding is gone —
    # see components/openshift/kustomization.yaml for why it never did anything.
    # Dropped from this list too, rather than kept as a no-op: the rename patch
    # only matches objects the overlay renders, so a name listed here for an
    # object that no longer exists implies a cleanup that does not happen.
    # An install made BEFORE its removal keeps its suffixed copy until that
    # install is uninstalled with the tree that still rendered it.
    # Namespace scope only (the cluster-scoped manager role already reads nodes).
    # Listed unconditionally anyway: the rename patch is a no-op when the object
    # is absent, and leaving it out meant two installs shared one binding — the
    # second apply replaced its subject list, so the first controller lost node
    # access and its readiness gate turned that into a NotReady pod.
    scaling-manager-node-reader-rolebinding
    # Same story for the Kueue reads a kueue-enabled quota entry needs.
    scaling-manager-kueue-reader-rolebinding
    # And for the Namespace get that finds the cluster policy namespace.
    scaling-manager-policy-namespace-reader-rolebinding
)

# LLMSM_OWNED_CLUSTER_ROLES are the ClusterRoles this project defines, mapped to the
# binding that references each. They are renamed per install for the same reason
# the bindings are, and it took a shared cluster to make the reason concrete:
# the shared GPU cluster had ten the scaling manager installs sharing four ClusterRoles under fixed names.
# Identical rules make an apply a no-op, so nothing had gone wrong yet — but an
# install carrying different rules would have rewritten the permissions of ten
# other controllers, cluster-wide, with no error and no restart to notice.
#
# Only OUR roles. `cluster-monitoring-view` is OpenShift's own: its bindings are
# renamed, its roleRef must not be.
#
# Kustomize's name-reference transformer would repoint roleRefs automatically for
# a nameSuffix, but nameSuffix renames every resource — including the
# external-scaler Service that every ScaledObject trigger addresses by name. So
# the roleRef is repointed explicitly, alongside the rename.
LLMSM_OWNED_CLUSTER_ROLES=(
    "scaling-manager-manager-role:scaling-manager-manager-rolebinding"
    "scaling-manager-metrics-auth-role:scaling-manager-metrics-auth-rolebinding"
    "scaling-manager-metrics-reader:scaling-manager-metrics-reader-rolebinding"
    "scaling-manager-epp-metrics-reader-role:scaling-manager-epp-metrics-reader-role-binding"
    # Namespace scope only; the patches are no-ops when the object is absent.
    # The role is node-reader-ROLE — the binding drops the suffix, the role does
    # not, and getting that wrong leaves the role unrenamed and the binding
    # pointing at a name nothing creates.
    "scaling-manager-node-reader-role:scaling-manager-node-reader-rolebinding"
    "scaling-manager-kueue-reader-role:scaling-manager-kueue-reader-rolebinding"
    "scaling-manager-policy-namespace-reader-role:scaling-manager-policy-namespace-reader-rolebinding"
)

# llmsm_ns_suffix echoes the per-namespace suffix appended to those names.
# sha256sum is GNU; macOS ships shasum. The suffix must be IDENTICAL on whatever
# host installs and whatever host uninstalls — a different suffix means the
# uninstall leaks every ClusterRoleBinding it was meant to remove — so this picks
# one implementation deterministically rather than depending on the box.
llmsm_ns_suffix() {
    if command -v sha256sum >/dev/null 2>&1; then
        printf '%s' "$1" | sha256sum | cut -c1-8
    else
        printf '%s' "$1" | shasum -a 256 | cut -c1-8
    fi
}

# llmsm_kueue_reader_names sets LLMSM_KUEUE_READER_NAMES to the Namespaces, space
# separated, that the Kueue reader of the controller installed in $1 may `get`:
# $2 first (the namespace it manages), then every name the live ClusterRole
# scaling-manager-kueue-reader-role-<suffix> already carries on its Namespace rule.
#
# ONE answer for the two commands that write that ClusterRole: the installer's
# prereqs phase (llmsm_append_kueue_reader_scope_patch) and
# `make enable-physical-limiter` (pl_grant_kueue_read). Each used to compute its
# own set -- the installer the managed namespace, enable the controller's own --
# so for a controller in scaling-manager-foo managing foo every enable took `get
# namespaces/foo` away, its Kueue source bounded foo at nothing, and the next
# setup-prereqs gave it back. Both now start from the managed namespace and keep
# what is there: a name an admin added by hand survives either command. A rule
# with no resourceNames adds nothing -- narrowing an unrestricted read to the
# managed namespace is what the installer always did.
#
# Only DNS-label names are kept, since they are written into YAML. Sets
# variables rather than echoing (call it as `llmsm_kueue_reader_names ... ||
# rc=$?`). A role that exists and cannot be read RETURNS non-zero with the
# reason in KC_ERR: Forbidden is not absent, and writing the role without
# knowing what it holds may remove a name.
llmsm_kueue_reader_names() {
    local ns="$1" managed="$2" rc=0 n existing="" seen=" "
    LLMSM_KUEUE_READER_NAMES=""
    KC_ERR=""
    kc_capture get clusterrole "scaling-manager-kueue-reader-role-$(llmsm_ns_suffix "$ns")" -o yaml || rc=$?
    if [ "$rc" -ne 0 ]; then
        kc_not_found "$KC_ERR" || { KC_ERR="${KC_ERR:-kubectl exited $rc}"; return 1; }
    elif [ -n "$KC_OUT" ]; then
        if ! existing="$(printf '%s\n' "$KC_OUT" \
            | yq '.rules[] | select((.resources // [])[] == "namespaces") | (.resourceNames // [])[]' 2>&1)"; then
            KC_ERR="could not parse it: $existing"
            return 1
        fi
    fi
    local -a all
    # read -a, not an unquoted expansion: a `*` in a hand-edited role must not
    # glob into the names of files in the working directory.
    # $2 may name several managed namespaces, comma- or space-separated (a
    # controller that manages a list). Split it first: as one word it failed the
    # name check below and was dropped, narrowing the read to what the role held.
    managed="$(printf '%s' "$managed" | tr ',' ' ')"
    # A kept name that was a managed namespace of this install before, and is
    # not one now, is the list shrinking: recompute, do not merge.
    local dropped="" g
    if [ -n "${LLMSM_KUEUE_READER_PREVIOUS:-}" ]; then
        for g in $LLMSM_KUEUE_READER_PREVIOUS; do
            case " $managed " in *" $g "*) ;; *) dropped="$dropped $g " ;; esac
        done
    fi
    read -r -a all <<< "$managed $(printf '%s' "$existing" | tr '\n' ' ')"
    for n in "${all[@]}"; do
        case "$dropped" in *" $n "*) continue ;; esac
        case "$n" in ''|*[!a-z0-9-]*) continue ;; esac
        case "$seen" in *" $n "*) continue ;; esac
        seen="$seen$n "
        LLMSM_KUEUE_READER_NAMES="${LLMSM_KUEUE_READER_NAMES:+$LLMSM_KUEUE_READER_NAMES }$n"
    done
    return 0
}

# llmsm_append_crb_name_patches appends the rename patches to a kustomization file.
#
# Deploy AND undeploy must both use this. They diverged once, with real cost: the
# install renamed the bindings and the uninstall did not, so `kubectl delete -k`
# resolved the FIXED names and removed whichever install currently owned them —
# an uninstall of one scaling manager stripping a different, healthy one of its permissions —
# while leaking its own suffixed bindings. One definition, used by both.
llmsm_append_crb_name_patches() {
    local kustomization="$1" ns="$2" suffix crb pair role binding
    suffix="$(llmsm_ns_suffix "$ns")"
    printf 'patches:\n' >> "$kustomization"
    for crb in "${LLMSM_SHARED_CLUSTER_ROLE_BINDINGS[@]}"; do
        cat >> "$kustomization" <<EOF
- patch: |-
    - op: replace
      path: /metadata/name
      value: ${crb}-${suffix}
  target:
    kind: ClusterRoleBinding
    name: ${crb}
EOF
    done
    # The roles, and the roleRef of the binding that names each. Both, or the
    # binding points at a ClusterRole that no longer exists under that name and
    # the controller silently has no permissions at all.
    for pair in "${LLMSM_OWNED_CLUSTER_ROLES[@]}"; do
        role="${pair%%:*}"; binding="${pair#*:}"
        cat >> "$kustomization" <<EOF
- patch: |-
    - op: replace
      path: /metadata/name
      value: ${role}-${suffix}
  target:
    kind: ClusterRole
    name: ${role}
- patch: |-
    - op: replace
      path: /roleRef/name
      value: ${role}-${suffix}
  target:
    kind: ClusterRoleBinding
    name: ${binding}
EOF
    done
    # The OpenShift ServiceMonitor pins the serving cert's name with
    # tlsConfig.serverName, and that is a plain STRING: `namespace:` rewrites
    # namespace fields, not a namespace spelled inside a value. Left alone it
    # keeps naming the overlay's default namespace, the service-ca SAN never
    # matches, and Prometheus rejects every scrape with
    #
    #   http: TLS handshake error ...: remote error: tls: bad certificate
    #
    # once per scrape interval, forever. Nothing else fails: the controller is
    # healthy and idle, so the only symptom is that no llmsm_* series ever appear
    # — which also silently empties any benchmark report built from them. Found
    # by installing into a namespace that is not the overlay default and reading
    # the controller log.
    #
    # It belongs here rather than in the component because only this layer knows
    # the namespace: a component's replacement runs before the outer
    # `namespace:` transform, when the Service still has no namespace at all.
    if [ "${ENVIRONMENT:-}" = "openshift" ]; then
        cat >> "$kustomization" <<EOF
- patch: |-
    - op: replace
      path: /spec/endpoints/0/tlsConfig/serverName
      value: scaling-manager-controller-manager-metrics-service.${ns}.svc
  target:
    kind: ServiceMonitor
EOF
    fi
}

# llmsm_overlay_dir echoes the absolute Kustomize overlay directory for the selected
# scope and platform. Deploy and cleanup MUST resolve it the same way, or an
# uninstall leaves behind exactly the resources the other overlay owns.
llmsm_overlay_dir() {
    local scope platform
    scope="$(llmsm_install_scope)"
    if [ "${ENVIRONMENT:-}" = "openshift" ]; then
        platform="openshift"
    else
        platform="kubernetes"
    fi
    local dir="$LLMSM_PROJECT/config/overlays/${scope}-scoped/${platform}"
    [ -d "$dir" ] || log_error "No overlay for scope '${scope}' on '${platform}' (looked in $dir)"
    (cd "$dir" && pwd)
}

# The kinds a CLUSTER ADMIN owns, and the phase split built on them.
#
# Everything here either is cluster-scoped or needs a permission a namespace admin
# does not have with the stock `admin` ClusterRole. Splitting the install on this
# line is what makes a tenant install real rather than aspirational: an admin runs
# the prereqs phase once for a namespace, and whoever owns that namespace can then
# install and upgrade the controller without holding any cluster-scoped right.
#
#   Namespace           cluster-scoped to create.
#   ClusterRole/Binding cluster-scoped, obviously.
#   ServiceMonitor      namespaced, but the stock `admin` ClusterRole does NOT
#                       grant monitoring.coreos.com — verified on a real cluster,
#                       where it was one of two denials standing between a
#                       namespace admin and a working "self-service" install.
#   Role/RoleBinding    namespaced, and still not the tenant's to create: RBAC
#                       forbids granting permissions you do not hold yourself
#                       (privilege-escalation prevention), and the controller's
#                       Role names resources the stock `admin` ClusterRole does
#                       not carry. A tenant applying it gets "attempting to grant
#                       RBAC permissions not currently held", the Role is skipped,
#                       its RoleBinding then fails with "not found", and the
#                       controller comes up unable to read a ConfigMap in its own
#                       namespace — a CrashLoopBackOff two removes from its cause.
#                       Verified with a token-scoped kubeconfig; it cannot be
#                       fixed by granting more to the tenant without granting them
#                       everything the controller has.
# ServiceAccount and Secret are here for the ServiceMonitor's sake, and the
# ordering is the whole point.
#
# On OPENSHIFT the ServiceMonitor authenticates with `bearerTokenSecret:
# scaling-manager-metrics-scraper-token`. (On Kubernetes it uses `bearerTokenFile` -- a
# projected service-account token the kubelet supplies -- so there is no Secret
# for the operator to resolve and this failure cannot occur there. The kinds move
# on both platforms anyway: the objects reference each other on both, and one
# phase boundary is easier to reason about than a per-platform one.) The prometheus-operator resolves that Secret when
# it first sees the ServiceMonitor, and if the Secret is absent it REJECTS the
# object -- `reason=InvalidConfiguration`, "unable to get secret" -- and does not
# retry when the Secret later appears. Nothing re-evaluates it: a metadata write
# is not enough, and re-running this phase applies identical content, so
# `kubectl apply` reports "unchanged" and the operator never looks again.
#
# With the Secret in the CONTROLLER phase, the two-phase install therefore created
# the ServiceMonitor 33 minutes before the Secret it needs, and the scaling manager's own metrics
# were permanently uncollected -- no `up` series, no `llmsm_*` series -- while every
# install step reported success. Measured on the shared GPU cluster: the install said
# "All components verified successfully!" and Thanos had nothing.
#
# The single-command install (INSTALL_PHASE=all) was unaffected, because it applies
# everything at once and the render orders Secret before ServiceMonitor. That is
# why this survived: it is the two-person split, the shape the guides recommend,
# that breaks. The two namespaces on that cluster running this code with working
# metrics both had their Secret created 3 seconds BEFORE their ServiceMonitor.
#
# Both kinds belong to an admin anyway -- they are namespaced, and creating them
# needs no rights a namespace owner lacks, but they are part of the same
# monitoring wiring the ServiceMonitor is. Moving them here keeps the three
# objects that reference each other in one phase, applied in render order, which
# puts ServiceAccount and Secret ahead of ServiceMonitor.
#
# This list also drives the phase-2 gate (llmsm_rendered_prereq_objects), so the
# controller phase now checks for them and names them if an admin's phase 1
# predates this change.
#
# Note the filter matches on KIND alone -- it cannot name objects. Every
# ServiceAccount and Secret in the render is therefore admin-owned, today the
# two controller/epp-metrics ServiceAccounts and their service-account-token
# Secrets, which is what is wanted. A Secret added to the base later becomes
# admin-owned silently, and a tenant would meet it as a missing prerequisite at
# phase 2 rather than as anything more obvious. Adding one means deciding which
# phase owns it.
LLMSM_PREREQ_KINDS=(Namespace ClusterRole ClusterRoleBinding ServiceMonitor Role RoleBinding ServiceAccount Secret)

# llmsm_prereq_kind_filter echoes a yq expression selecting (or with $1=exclude,
# rejecting) the admin-owned kinds. One definition, so the prereqs phase and the
# controller phase cannot disagree about where the line is and leave a kind that
# neither applies.
# LLMSM_PREREQ_NAMED are objects the prereqs phase owns BY NAME, for kinds it does
# not own wholesale.
#
# Only scaling-manager-metrics-server-ca, and it has to be here: the ServiceMonitor above is
# admin-owned and references this ConfigMap as its `ca`. The prometheus-operator
# resolves that the moment the ServiceMonitor appears and REJECTS it if the
# ConfigMap is absent, then never reconsiders -- so a two-phase install created
# the monitor in phase 1, the ConfigMap in phase 2, and left a standing rejection:
#
#   failed to get ca "<configmap=scaling-manager-metrics-server-ca,key=service-ca.crt>":
#   configmaps "scaling-manager-metrics-server-ca" not found
#
# with no scrape target, no llmsm_* series, and a dashboard whose scaling manager panels are
# empty while its vllm:* panels fill. Exactly the failure llm-d/llm-d-workload-variant-autoscaler#1516 fixed for the
# ServiceMonitor's Secret; the CA was missed because it is a different kind.
#
# By NAME rather than by kind, because the overlay renders three ConfigMaps and
# the other two are the tenant's: scaling-manager-manager-config and, especially,
# scaling-manager-scaling-policy-config, which a namespace owner is meant to edit. Making
# every ConfigMap admin-owned to fix one of them would take the scaling policy
# with it.
#
# NetworkPolicy/scaling-manager-external-scaler is here for a different reason: it must name
# the namespace KEDA runs in, and only the admin's phase can read that (the
# external.metrics APIService is cluster-scoped). Applied from the tenant's
# phase it was always a guess -- keda-system -- and with KEDA in `keda` it
# refused KEDA and nothing scaled. The tenant phase now neither writes nor
# requires it: llmsm_missing_prereqs reports it OPTIONAL, because an admin phase
# that could not find KEDA leaves it out on purpose
# (llmsm_append_scaler_networkpolicy_patch) rather than write a guess.
LLMSM_PREREQ_NAMED=(
    "ConfigMap/scaling-manager-metrics-server-ca"
    "NetworkPolicy/scaling-manager-external-scaler"
)

llmsm_prereq_kind_filter() {
    local mode="${1:-select}" k n expr=""
    for k in "${LLMSM_PREREQ_KINDS[@]}"; do
        [ -n "$expr" ] && expr="$expr or "
        expr="${expr}.kind == \"$k\""
    done
    for n in "${LLMSM_PREREQ_NAMED[@]}"; do
        expr="$expr or (.kind == \"${n%%/*}\" and .metadata.name == \"${n##*/}\")"
    done
    if [ "$mode" = "exclude" ]; then
        echo "select(($expr) | not)"
    else
        echo "select($expr)"
    fi
}

# llmsm_resolve_namespace points a namespace-scoped install at NAMESPACE when the
# caller named no LLMSM_NS.
#
# Here rather than in the Makefile because every entry point needs it — install,
# undeploy and check — and `llmsm_install_scope` is the only thing that knows the
# scope once LLMSM_SCOPE is empty and the platform default applies. The make-side
# version reached only the 12 phase targets, so `NAMESPACE=x make
# undeploy-scaling-manager-on-k8s` resolved to the DEFAULT namespace while the install it was
# meant to undo had gone to x.
#
# Cluster scope is untouched: that controller manages every namespace and belongs
# in its own, not inside whichever one runs llm-d.
# llmsm_bootstrap_env gives a caller that sources these libraries directly — rather
# than going through install.sh — the same namespace install.sh would have used.
#
# The targets that plan and apply ScaledObjects do exactly that, so none of
# install.sh's preamble runs for them: they saw LLMSM_NS's default and ignored
# `export NAMESPACE=…` entirely. The plan then scanned the wrong namespace and
# found nothing, or — worse, because it looks like it worked — wrote every trigger
# with a scaler address in a namespace where no scaler runs. KEDA reports that as
# a failing trigger on a healthy-looking install, which reads as "the scaling manager is broken"
# rather than "the scaling manager was handed the wrong address".
#
# ${VAR-…} without the colon, so an explicitly empty value set by install.sh is
# left alone: a nested call must not re-capture an LLMSM_NS that a default filled in.
llmsm_bootstrap_env() {
    # CONTROLLER_NAMESPACE names the controller's namespace too (see install.sh).
    LLMSM_NS=${LLMSM_NS:-${CONTROLLER_NAMESPACE:-}}
    LLMSM_NS_EXPLICIT=${LLMSM_NS_EXPLICIT-${LLMSM_NS:-}}
    NAMESPACE_EXPLICIT=${NAMESPACE_EXPLICIT-${NAMESPACE:-}}
    export LLMSM_NS_EXPLICIT NAMESPACE_EXPLICIT
    LLMSM_NS=${LLMSM_NS:-scaling-manager-system}
    export LLMSM_NS
    llmsm_resolve_namespace
    # The namespaces the installed controller manages, when it manages several
    # and the caller named none: the plan and the apply register exactly those.
    llmsm_adopt_live_watch_namespaces
    llmsm_resolve_watch_namespaces
    # NOT llmsm_require_namespace. llmsm_bootstrap_env is shared with the read-only
    # tooling -- so-list, so-park, so-freeze, so-resume, scaledobjects-plan,
    # dashboard -- and requiring the managed namespace here refused all of them:
    #
    #   $ make scaledobjects-plan
    #   [ERROR] NAMESPACE is not set, and it is required.
    #   The scaling manager is added to a namespace where llm-d is ALREADY RUNNING, so the
    #   namespace is an input to this install, ...
    #
    # for a command that plans and installs nothing. The requirement belongs to the
    # install, so it is called from the check and install paths in install_core.sh,
    # which is also where its own comment says it should be.
}

# llmsm_require_namespace stops unless the caller named the namespace scaling manager is to manage.
#
# NAMESPACE and LLMSM_NS are DIFFERENT questions, and the fallback below made them look
# like one:
#
#   NAMESPACE   the namespace scaling manager MANAGES -- where llm-d is running. Mandatory in
#               namespace scope: it is the whole input to the scenario.
#   LLMSM_NS      the namespace the CONTROLLER IS INSTALLED INTO. It matters when those
#               two differ, which is the cluster-wide and multi-tenant shape, not
#               this one.
#
# So LLMSM_NS is deliberately NOT accepted as a way to name the target here. Someone
# who sets only LLMSM_NS has said where the controller goes, not what it manages, and
# guessing the second from the first is how an install ends up in a namespace with no
# llm-d in it.
#
# What this replaces: LLMSM_NS fell back to `scaling-manager-system` and
# llmsm_autoselect_namespace then searched the cluster and silently ADOPTED the single
# namespace that ran model servers -- or, finding none, kept the fallback. Both paths
# could install a healthy-looking controller into a namespace with nothing to scale,
# which is the silent failure this preflight exists to prevent. The fallback and the
# discovery are both still in the file: discovery is how a CLUSTER-scoped install
# finds the namespaces it manages, which is its proper use.
llmsm_require_namespace() {
    [ "$(llmsm_install_scope)" = "namespace" ] || return 0
    [ -z "${NAMESPACE_EXPLICIT:-}" ] || return 0
    # A controller that manages a list (or `auto`) is told its namespaces by
    # LLMSM_WATCH_NS; NAMESPACE would name only one of them.
    if declare -F llmsm_watch_is_listed >/dev/null && llmsm_watch_is_listed; then
        return 0
    fi
    # LLMSM_WATCH_NS naming one namespace outside LLMSM_NS names it too: the managed
    # namespace was given, so there is nothing to guess. A list shrunk to one
    # namespace on a re-install lands here.
    if [ -n "${LLMSM_WATCH_NS:-}" ] && [ "$LLMSM_WATCH_NS" != "$LLMSM_NS" ]; then
        return 0
    fi
    # kind-emulator provisions llm-d, the EPP and the scaling manager in one sequence and owns its own
    # namespaces: `make deploy-e2e-infra` calls install.sh with no NAMESPACE on
    # purpose, using the controller-namespace defaults. Requiring it there would break
    # the e2e suite to enforce a rule about a scenario the suite is not running.
    [ "${ENVIRONMENT:-}" != "kind-emulator" ] || return 0

    local found="" count=0 hint=""
    # A hint, not a choice. Listing workloads cluster-wide is exactly what a namespace
    # admin may not do, so this is best-effort and its absence is not an error.
    if kubectl auth can-i list deployments -A >/dev/null 2>&1; then
        found="$(llmsm_namespaces_with_model_servers 2>/dev/null || true)"
        count=$(printf '%s' "$found" | grep -c . || true)
    fi
    # Capped. On a shared cluster this list is long -- 100+ namespaces on the shared GPU cluster --
    # and an error message that scrolls the instruction off the screen has buried the
    # one line the reader needed.
    if [ "${count:-0}" -gt 0 ]; then
        # Built with explicit newlines: $( ) strips trailing ones, so appending the
        # "and N more" line after a command substitution put it on the end of the
        # last namespace instead of its own line.
        hint="
llm-d model servers are running in:
$(printf '%s' "$found" | head -10 | sed 's/^/  - /')"
        [ "$count" -gt 10 ] && hint="${hint}
  … and $((count - 10)) more"
        hint="${hint}
"
    fi

    log_error "NAMESPACE is not set, and it is required.

The scaling manager is added to a namespace where llm-d is ALREADY RUNNING, so the namespace is an
input to this install, not something to guess. Name the one llm-d serves from:

    NAMESPACE=<namespace> make <target>
${hint}
NAMESPACE is llm-d's own variable, so if you followed one of its guides it is already
exported in that shell.

(LLMSM_NS is a different setting: it names where the CONTROLLER is installed, which
differs from the managed namespace in the cluster-wide shape, and when the controller
is installed out of reach in a namespace of its own: then pass both, as
NAMESPACE=<managed> LLMSM_NS=<controller namespace> LLMSM_WATCH_NS=<managed>.)"
}

llmsm_resolve_namespace() {
    LLMSM_NS_SOURCE=default
    export LLMSM_NS_SOURCE
    [ -z "${LLMSM_NS_EXPLICIT:-}" ] || { LLMSM_NS_SOURCE=explicit; return 0; }
    [ -n "${NAMESPACE_EXPLICIT:-}" ] || return 0   # stays "default": discovery may still fill it
    [ "$(llmsm_install_scope)" = "namespace" ] || return 0
    LLMSM_NS="$NAMESPACE_EXPLICIT"
    LLMSM_NS_SOURCE=namespace-env
    export LLMSM_NS LLMSM_NS_SOURCE
}

# llmsm_prepare_overlay_base populates $1/base with the overlay this install will
# apply.
#
# The preflight, the deploy and the undeploy all go through here. They must: a
# preflight that infers the shape instead of reading it is how a namespace-scoped
# OpenShift install passed `--check` and then failed partway through
# `kubectl apply -k`, and an undeploy that built the base differently left
# cluster-scoped objects behind after reporting a clean removal.
#
# It used to re-render the overlay under LLMSM_ADMIN_GRANTS, dropping the
# unauthenticated-metrics component and adding node-reader, so that one command
# could serve both a cluster admin and a tenant. The phase split removed the
# question: everything cluster-scoped belongs to the prereqs phase, which is the
# admin's by definition, so the overlay is now the same for both and this is a
# symlink.
llmsm_prepare_overlay_base() {
    local tmp_overlay="$1" kustomize_overlay
    kustomize_overlay="$(llmsm_overlay_dir)"
    # Symlinked so kustomization.yaml can reference it with a relative path —
    # Kustomize rejects absolute paths in resources.
    ln -s "$kustomize_overlay" "$tmp_overlay/base"
}

# llmsm_repair_immutable_rolerefs deletes this install's ClusterRoleBindings whose
# roleRef no longer matches what will be applied, so the apply can recreate them.
#
# `roleRef` is IMMUTABLE. An upgrade that changes which ClusterRole a binding
# points at therefore fails with
#
#   ClusterRoleBinding ... is invalid: roleRef: ... cannot change roleRef
#
# and the install stops halfway — which is exactly what happened the first time
# the ClusterRoles were given per-namespace names, on a cluster with an install
# already on it. Found by running the upgrade, not by reading it.
#
# It only ever deletes a binding whose name matches what THIS install renders,
# i.e. one already carrying this namespace's suffix. Another install's binding
# has a different suffix and cannot be selected here. The controller is without
# that binding for the moment between the delete and the apply below.
llmsm_repair_immutable_rolerefs() {
    local name want have
    while read -r name want; do
        [ -n "$name" ] && [ -n "$want" ] || continue
        have="$(kubectl get clusterrolebinding "$name" -o jsonpath='{.roleRef.name}' 2>/dev/null || true)"
        [ -n "$have" ] || continue          # absent: the apply just creates it
        [ "$have" = "$want" ] && continue   # already right
        log_info "Recreating ClusterRoleBinding $name — roleRef is immutable and must change ($have -> $want)"
        kubectl delete clusterrolebinding "$name" --ignore-not-found >/dev/null 2>&1 || true
    done < <(llmsm_render_manifests \
        | yq 'select(.kind == "ClusterRoleBinding") | .metadata.name + " " + .roleRef.name' 2>/dev/null \
        | grep -v '^null' || true)
}

# llmsm_render_manifests prints the manifests this install would apply, with the
# namespace transform and the per-namespace ClusterRoleBinding renames applied —
# so the NAMES it prints are the names that will exist on the cluster. The image
# pin is not applied: no caller of this asks about the image.
#
# Empty output means the overlay could not be rendered. Callers must treat that as
# "unknown", never as "nothing".
llmsm_render_manifests() {
    local tmp ns="${LLMSM_NS:-scaling-manager-system}"
    tmp="$(mktemp -d)" || return 0
    if llmsm_prepare_overlay_base "$tmp" >/dev/null 2>&1; then
        printf 'namespace: %s\nresources:\n- ./base\n' "$ns" > "$tmp/kustomization.yaml"
        llmsm_append_crb_name_patches "$tmp/kustomization.yaml" "$ns"
        # The same per-install decisions deploy_scaling_manager_controller applies, so the
        # objects this lists are the objects that install creates. Without the
        # metrics-reader one, an install that DROPPED that binding (no
        # Prometheus account it trusts) left it in the expected set, and the
        # controller-only phase reported it missing and refused forever.
        #
        # Guarded because Makefile targets source this file without
        # infra_scaling_manager.sh; none of them renders, so the guard never decides an
        # install.
        if [ "$(llmsm_install_scope)" = "namespace" ] && declare -F llmsm_append_kueue_reader_scope_patch >/dev/null; then
            local managed_words managed_labels="" m
            managed_words="$(LLMSM_NS="$ns" llmsm_watch_words)"
            for m in $managed_words; do managed_labels="$managed_labels $(llmsm_managed_policy_label "$m")"; done
            # shellcheck disable=SC2086 # one word per Namespace name
            llmsm_append_kueue_reader_scope_patch "$tmp/kustomization.yaml" "$ns" $managed_words
            # shellcheck disable=SC2086 # one word per Namespace name
            llmsm_append_policy_namespace_reader_scope_patch "$tmp/kustomization.yaml" "$ns" \
                $managed_words "${LLMSM_POLICY_NS:-}" $managed_labels
            if llmsm_kueue_reader_unneeded; then
                llmsm_append_kueue_reader_delete "$tmp/kustomization.yaml" "$ns"
            fi
        fi
        if [ "${ENVIRONMENT:-}" != "openshift" ] && declare -F llmsm_append_metrics_reader_decision >/dev/null; then
            llmsm_append_metrics_reader_decision "$tmp/kustomization.yaml" "$ns" >/dev/null 2>&1
        fi
        # `|| true` because the callers run under `set -e` with pipefail, and a
        # render that fails is the case they exist to handle: it must reach them
        # as empty output, not as the whole install script exiting.
        kubectl kustomize "$tmp" 2>/dev/null || true
    fi
    rm -rf "$tmp"
}

# llmsm_rendered_kinds echoes, one per line, each distinct Kind this install would
# create. With $1=controller, only the kinds of the objects the controller-only
# phase (INSTALL_PHASE=scaling-manager) applies: everything llmsm_prereq_kind_filter does not
# give to the admin.
#
# Filtered by OBJECT, not by kind name. NetworkPolicy/scaling-manager-external-scaler is
# the only NetworkPolicy in the render and is admin-owned by name
# (LLMSM_PREREQ_NAMED); dropping only LLMSM_PREREQ_KINDS left NetworkPolicy in the
# tenant's list, so INSTALL_PHASE=scaling-manager and --check demanded `create
# networkpolicies` for an object that phase never applies.
llmsm_rendered_kinds() {
    if [ "${1:-}" = controller ]; then
        llmsm_render_manifests \
            | yq "$(llmsm_prereq_kind_filter exclude) | .kind" 2>/dev/null \
            | grep -Ev '^(---|null|)$' | sort -u || true
        return 0
    fi
    # Only column 0 — `kind:` also appears indented under roleRef, and with a
    # leading dash under subjects.
    llmsm_render_manifests | awk '/^kind: /{print $2}' | sort -u || true
}

# llmsm_launcher_scrapers echoes the name of every PodMonitor in $1 that would
# actually generate a scrape target for an FMA launcher pod. $2, if given, is a
# PodMonitor name to ignore.
#
# "Would generate a target" is the whole point, and it is NOT the same as
# "selects". llm-d's own vllm-<model> PodMonitor selects a launcher the moment it
# binds -- the serving labels are stamped on it then -- but names its endpoint by
# port NAME, and a launcher declares no container ports, so the operator resolves
# nothing and produces no target. A caller asking "is anything scraping these?"
# would get the wrong answer from a selector test, in both directions: it would
# report scraping where there is none, and refuse to add scraping where it is
# needed.
#
# So a monitor counts only if it could reach the pod:
#   - it sets targetPort (a number bypasses the named-port lookup), or
#   - it rewrites __address__ in a relabeling, or
#   - it names a port that a launcher pod actually declares.
#
# Selectors are resolved by asking the API server rather than by comparing label
# maps, so a monitor that reaches launchers by any combination is caught.
# matchExpressions are not evaluated -- kubectl takes no expression selector -- so
# an expression-only monitor can slip through. This is a guard for the realistic
# case, not a proof, and both callers treat it as advisory.
#
# Echoes nothing when there are no launcher pods: with nothing to resolve against,
# "no monitor reaches them" is the only answer that can be defended.
llmsm_launcher_scrapers() {
    local ns="$1" exclude="${2:-}" launchers launcher_ports pm sel rewrites ports matched p
    launchers=$(kubectl get pods -n "$ns" -l app.kubernetes.io/component=launcher \
        -o name 2>/dev/null)
    [ -n "$launchers" ] || return 0

    launcher_ports=$(kubectl get pods -n "$ns" -l app.kubernetes.io/component=launcher \
        -o jsonpath='{range .items[*].spec.containers[*].ports[*]}{.name}{"\n"}{end}' 2>/dev/null \
        | grep -v '^$' | sort -u)

    kubectl get podmonitor -n "$ns" -o json 2>/dev/null \
      | jq -r --arg ex "$exclude" '
          .items[]
          | select($ex == "" or .metadata.name != $ex)
          | [ .metadata.name,
              ((.spec.selector.matchLabels // {}) | to_entries
                | map("\(.key)=\(.value)") | join(",")),
              ( [ (.spec.podMetricsEndpoints // [])[]
                  | (.targetPort != null)
                    or ((.relabelings // []) | any(.targetLabel == "__address__")) ]
                | any ),
              ( [ (.spec.podMetricsEndpoints // [])[] | .port // empty ] | join(" ") ) ]
          | @tsv' 2>/dev/null \
      | while IFS=$'\t' read -r pm sel rewrites ports; do
            [ -n "$sel" ] || continue
            matched=$(kubectl get pods -n "$ns" -l "$sel" -o name 2>/dev/null)
            [ -n "$matched" ] || continue
            printf '%s\n' "$matched" | grep -Fxq -f <(printf '%s\n' "$launchers") || continue
            if [ "$rewrites" = "true" ]; then
                printf '%s\n' "$pm"
                continue
            fi
            for p in $ports; do
                if printf '%s\n' "$launcher_ports" | grep -Fxq -- "$p"; then
                    printf '%s\n' "$pm"
                    break
                fi
            done
        done
}

# llmsm_rendered_prereq_objects echoes "<Kind> <name>" for every admin-owned object
# this install needs, at the names it will really have.
llmsm_rendered_prereq_objects() {
    # yq writes a `---` between documents even when each renders to one scalar.
    llmsm_render_manifests \
        | yq "$(llmsm_prereq_kind_filter select) | .kind + \" \" + .metadata.name" 2>/dev/null \
        | grep -Ev '^(---|null)' || true
}

# --- Render mode (make setup-prereqs RENDER=<dir>) ----------------------------
#
# The cluster-admin phase, rendered for an admin instead of applied. Every read
# and detection runs against the cluster exactly as in a live run, with the
# caller's own (read-only) rights; nothing is written. What the live run would
# write goes to <dir>:
#
#   admin.yaml   every object it would create or apply, cleaned of server
#                fields, in apply order (namespaces and RBAC first)
#   admin.sh     what is not a plain object -- Helm installs, deletes, patches
#                of objects already on the cluster -- each with the reason, and
#                `kubectl apply -f admin.yaml`
#
# How: llmsm_render_init replaces `kubectl` and `helm` with the shell functions
# below, the same mechanism LLMSM_KUBE_CONTEXT uses, so every call in the admin
# phase goes through one place and no call site knows about rendering. Reads
# go to the cluster. Writes are captured: an apply or create becomes an object
# in a local store, a patch or label of a stored object is applied to the
# stored copy, and anything else is recorded as a command. Later reads of a
# stored object are answered from the store, so the phase sees its own writes
# as it would on the cluster. A Helm install it records also answers the reads
# that only the installed chart could (its CRDs, KEDA's API), so the decisions
# after it are the live run's.
#
# Permission questions about the caller (`kubectl auth can-i` without --as) are
# answered yes: they ask what the person who runs admin.sh may do, and that is
# a cluster admin. A render therefore needs read rights only. Waits and rollout
# checks on objects that were only rendered are skipped and say so.
#
# A PATH shim catches any child process that calls kubectl or helm directly and
# refuses its writes, so a render can never write to the cluster.

# llmsm_render_active reports whether this run renders instead of applying.
llmsm_render_active() {
    [ -n "${LLMSM_RENDER_DIR:-}" ]
}

# llmsm_render_init starts render mode for directory $1. Refuses anything but the
# cluster-admin phase. Clears the outputs of an earlier render first, so a
# render that fails leaves no admin.sh that looks current.
llmsm_render_init() {
    local dir="$1"
    [ -n "$dir" ] || return 0
    if [ "${CHECK_ONLY:-false}" = "true" ] || [ "${UNDEPLOY:-false}" = "true" ]; then
        log_error "RENDER renders the cluster-admin phase only (make setup-prereqs RENDER=<dir>); it does not combine with --check or --undeploy."
    fi
    if [ -z "${INSTALL_PHASE_EXPLICIT:-}" ]; then
        INSTALL_PHASE=prereqs
        INSTALL_PHASE_EXPLICIT=prereqs
        export INSTALL_PHASE INSTALL_PHASE_EXPLICIT
    fi
    if [ "${INSTALL_PHASE:-}" != "prereqs" ]; then
        log_error "RENDER renders the cluster-admin phase only: make setup-prereqs RENDER=<dir>. The controller phase (INSTALL_PHASE=${INSTALL_PHASE}) is run by the namespace owner, after the admin has run admin.sh."
    fi
    if [ "${ENVIRONMENT:-}" = "kind-emulator" ]; then
        log_error "RENDER does not apply to ENVIRONMENT=kind-emulator, which creates its own cluster."
    fi
    mkdir -p "$dir" || log_error "RENDER=$dir: cannot create the directory."
    LLMSM_RENDER_DIR="$(cd "$dir" && pwd)" || log_error "RENDER=$dir: cannot enter the directory."
    LLMSM_RENDER_WORK="$LLMSM_RENDER_DIR/.render-work"
    rm -rf "$LLMSM_RENDER_WORK"
    rm -f "$LLMSM_RENDER_DIR/admin.yaml" "$LLMSM_RENDER_DIR/admin.sh"
    mkdir -p "$LLMSM_RENDER_WORK/obj" "$LLMSM_RENDER_WORK/virt" "$LLMSM_RENDER_WORK/ns" "$LLMSM_RENDER_WORK/bin" \
        "$LLMSM_RENDER_WORK/pre.d" "$LLMSM_RENDER_WORK/post.d" \
        || log_error "RENDER=$dir: cannot write there."
    : > "$LLMSM_RENDER_WORK/order"
    : > "$LLMSM_RENDER_WORK/gone"
    : > "$LLMSM_RENDER_WORK/virt-crds"
    LLMSM_RENDER_KUBECTL="$(type -P kubectl || true)"
    LLMSM_RENDER_HELM="$(type -P helm || true)"
    [ -n "$LLMSM_RENDER_KUBECTL" ] || log_error "RENDER: kubectl is not on PATH."
    export LLMSM_RENDER_DIR LLMSM_RENDER_WORK LLMSM_RENDER_KUBECTL LLMSM_RENDER_HELM
    llmsm_render_write_path_shim
    PATH="$LLMSM_RENDER_WORK/bin:$PATH"
    export PATH
    kubectl() { llmsm_render_kubectl "$@"; }
    helm()    { llmsm_render_helm "$@"; }
    log_info "RENDER=$LLMSM_RENDER_DIR: this run reads the cluster with your rights and applies NOTHING; it needs read rights only."
    log_info "  Permission checks are answered for the cluster admin who runs admin.sh. Where the output below says created, applied or removed, it means written to admin.yaml or admin.sh."
}

# llmsm_render_write_path_shim writes kubectl and helm executables that refuse
# writes, for any child process that bypasses the shell functions.
llmsm_render_write_path_shim() {
    local tool real
    for tool in kubectl helm; do
        real="$LLMSM_RENDER_KUBECTL"
        [ "$tool" = kubectl ] || real="$LLMSM_RENDER_HELM"
        [ -n "$real" ] || continue
        cat > "$LLMSM_RENDER_WORK/bin/$tool" <<EOF
#!/usr/bin/env bash
# Render mode: reads pass through, writes are refused (deploy/lib/common.sh).
case " \$* " in
    *" --dry-run"*|*" --local"*|*" kustomize "*) exec "$real" "\$@" ;;
esac
for a in "\$@"; do
    case "\$a" in
        -*) continue ;;
        apply|create|patch|label|annotate|delete|replace|scale|set|edit|rollout|taint|cordon|uncordon|drain|run|expose|autoscale|exec|cp|install|upgrade|uninstall|rollback|repo|certificate)
            echo "RENDER: refusing '$tool \$*' from a child process; a render writes nothing to the cluster." >&2
            exit 1 ;;
        *) break ;;
    esac
done
exec "$real" "\$@"
EOF
        chmod +x "$LLMSM_RENDER_WORK/bin/$tool"
    done
}

# llmsm_render_real_kubectl runs the real kubectl, on the pinned context if any.
# A read the cluster refuses is noted (llmsm_render_note_forbidden): the live
# run, made by an admin, would have seen the object, so the render may differ.
llmsm_render_real_kubectl() {
    local errf rc=0
    errf="$(mktemp)" || return 125
    if [ -n "${LLMSM_KUBE_CONTEXT:-}" ]; then
        "$LLMSM_RENDER_KUBECTL" --context "$LLMSM_KUBE_CONTEXT" "$@" 2>"$errf" || rc=$?
    else
        "$LLMSM_RENDER_KUBECTL" "$@" 2>"$errf" || rc=$?
    fi
    [ "$rc" -eq 0 ] || llmsm_render_note_forbidden "$errf"
    cat "$errf" >&2
    rm -f "$errf"
    return "$rc"
}

# llmsm_render_real_helm runs the real helm, on the pinned context if any.
llmsm_render_real_helm() {
    local errf rc=0
    [ -n "$LLMSM_RENDER_HELM" ] || { echo "helm: not on PATH" >&2; return 127; }
    errf="$(mktemp)" || return 125
    if [ -n "${LLMSM_KUBE_CONTEXT:-}" ]; then
        "$LLMSM_RENDER_HELM" --kube-context "$LLMSM_KUBE_CONTEXT" "$@" 2>"$errf" || rc=$?
    else
        "$LLMSM_RENDER_HELM" "$@" 2>"$errf" || rc=$?
    fi
    [ "$rc" -eq 0 ] || llmsm_render_note_forbidden "$errf"
    cat "$errf" >&2
    rm -f "$errf"
    return "$rc"
}

# llmsm_render_note_forbidden records, from kubectl or helm stderr in file $1,
# the resource a Forbidden read named, for the warning llmsm_render_finish gives.
llmsm_render_note_forbidden() {
    local res
    grep -qi 'forbidden' "$1" 2>/dev/null || return 0
    # Impersonation is not a read: a question --as another identity that is
    # refused is answered "unknown", and the run already says so.
    grep -qi 'cannot impersonate' "$1" && return 0
    res="$(grep -oiE 'cannot (get|list|watch) resource "[^"]+"( in API group "[^"]*")?' "$1" | head -1)"
    printf '%s\n' "${res:-$(grep -i -m1 forbidden "$1" | cut -c1-200)}" >> "$LLMSM_RENDER_WORK/forbidden"
}

# llmsm_render_local_kubectl runs the real kubectl for a purely local operation
# (--local, kustomize, --dry-run=client), never on a context.
llmsm_render_local_kubectl() {
    "$LLMSM_RENDER_KUBECTL" "$@"
}

# llmsm_render_quote echoes its arguments as one shell-quoted command line.
llmsm_render_quote() {
    local a out=""
    for a in "$@"; do
        if [[ "$a" =~ ^[A-Za-z0-9_./:=@,+%-]+$ ]]; then
            out="$out $a"
        else
            out="$out '${a//\'/\'\\\'\'}'"
        fi
    done
    printf '%s\n' "${out# }"
}

# llmsm_render_lower echoes $1 in lower case.
llmsm_render_lower() {
    printf '%s' "$1" | tr '[:upper:]' '[:lower:]'
}

# llmsm_render_key echoes the store key for kind $1, namespace $2, name $3.
llmsm_render_key() {
    printf '%s__%s__%s' "$(llmsm_render_lower "$1")" "${2:-_}" "$3" | sed 's/[^A-Za-z0-9._-]/_/g'
}

# llmsm_render_kinds echoes the lower-case kind resource $1 names: kubectl takes
# plurals, short names and resource.group (prometheuses.monitoring.coreos.com
# is kind prometheus).
llmsm_render_kinds() {
    local r
    r="$(llmsm_render_lower "${1%%.*}")"
    case "$r" in
        ns) r=namespace ;; cm) r=configmap ;; sa) r=serviceaccount ;; deploy) r=deployment ;;
        svc) r=service ;; crd|crds) r=customresourcedefinition ;; netpol) r=networkpolicy ;;
        sts) r=statefulset ;; ds) r=daemonset ;; rs) r=replicaset ;; ep|endpoints) r=endpoints ;;
        prometheus|*status|*ingress) ;;
        *ies) r="${r%ies}y" ;;
        *uses) r="${r%es}" ;;
        *sses) r="${r%es}" ;;
        *ss) ;;
        *s) r="${r%s}" ;;
    esac
    printf '%s\n' "$r"
}

# llmsm_render_cluster_scoped reports whether lower-case kind $1 is cluster-scoped.
llmsm_render_cluster_scoped() {
    case "$1" in
        namespace|node|clusterrole|clusterrolebinding|customresourcedefinition|apiservice|\
        persistentvolume|storageclass|priorityclass|clustertriggerauthentication|clusterissuer|\
        clusterqueue|resourceflavor|cohort|validatingwebhookconfiguration|mutatingwebhookconfiguration|\
        validatingadmissionpolicy|validatingadmissionpolicybinding|runtimeclass|ingressclass) return 0 ;;
    esac
    return 1
}

# llmsm_render_find echoes the stored (or chart-provided) file for resource $1
# named $2 in namespace $3, or fails. A name deleted by this render and not
# written again answers "gone" (status 2).
llmsm_render_find() {
    local res="$1" name="$2" ns="$3" kind n key
    for kind in $(llmsm_render_kinds "$res"); do
        for n in "$ns" ""; do
            key="$(llmsm_render_key "$kind" "$n" "$name")"
            if [ -f "$LLMSM_RENDER_WORK/obj/$key.yaml" ]; then
                printf '%s\n' "$LLMSM_RENDER_WORK/obj/$key.yaml"
                return 0
            fi
            if grep -qxF "$key" "$LLMSM_RENDER_WORK/gone" 2>/dev/null; then
                return 2
            fi
            if [ -f "$LLMSM_RENDER_WORK/virt/$key.yaml" ]; then
                printf '%s\n' "$LLMSM_RENDER_WORK/virt/$key.yaml"
                return 0
            fi
        done
    done
    return 1
}

# llmsm_render_virtual_crd reports whether resource $1 is served only by a chart
# this render records (so the cluster does not know its type yet).
llmsm_render_virtual_crd() {
    local r
    r="$(llmsm_render_lower "$1")"
    case "$r" in
        *.*) grep -qxF "$r" "$LLMSM_RENDER_WORK/virt-crds" ;;
        *)   grep -qE "^(${r}|${r}s|${r}es)\." "$LLMSM_RENDER_WORK/virt-crds" ;;
    esac
}

# llmsm_render_ns_state echoes yes, no or unknown: whether namespace $1 exists on
# the cluster (not in the render). Cached for the run.
llmsm_render_ns_state() {
    local ns="$1" f out rc=0
    f="$LLMSM_RENDER_WORK/ns/$(printf '%s' "$ns" | sed 's/[^A-Za-z0-9._-]/_/g')"
    if [ -f "$f" ]; then cat "$f"; return 0; fi
    out="$(llmsm_render_real_kubectl get namespace "$ns" -o name 2>&1)" || rc=$?
    if [ "$rc" -eq 0 ]; then
        echo yes > "$f"
    elif printf '%s' "$out" | grep -qF '(NotFound)'; then
        echo no > "$f"
    else
        echo unknown > "$f"
    fi
    cat "$f"
}

# llmsm_render_print prints stored object file $1 in kubectl output format $2.
llmsm_render_print() {
    local file="$1" fmt="$2" kind name
    case "$fmt" in
        json) yq -o=json '.' "$file" ;;
        yaml) cat "$file" ;;
        name)
            kind="$(yq '.kind' "$file")"; name="$(yq '.metadata.name' "$file")"
            printf '%s/%s\n' "$(llmsm_render_lower "$kind")" "$name" ;;
        "")
            name="$(yq '.metadata.name' "$file")"
            printf 'NAME\n%s\n' "$name" ;;
        *) llmsm_render_local_kubectl annotate --local -f "$file" scaling-manager.llmd.ai/render-noop- -o "$fmt" ;;
    esac
}

# llmsm_render_empty_list prints an empty list in kubectl output format $1.
llmsm_render_empty_list() {
    case "$1" in
        json) printf '{"apiVersion":"v1","items":[],"kind":"List","metadata":{}}\n' ;;
        yaml) printf 'apiVersion: v1\nitems: []\nkind: List\nmetadata: {}\n' ;;
        "") echo "No resources found" >&2 ;;
    esac
    return 0
}

# llmsm_render_get answers `kubectl get`: a stored object from the store, a read
# in a namespace that does not exist yet as empty, everything else from the
# cluster.
llmsm_render_get() {
    local a res="" name="" ns="" fmt="" list=false ignore=false raw="" pos=0 file rc=0 out kind
    local args=("$@")
    while [ $# -gt 0 ]; do
        a="$1"; shift
        case "$a" in
            -n|--namespace) ns="$1"; shift ;;
            --namespace=*) ns="${a#*=}" ;;
            -o|--output) fmt="$1"; shift ;;
            -o*) fmt="${a#-o}"; fmt="${fmt#=}" ;;
            --output=*) fmt="${a#*=}" ;;
            -A|--all-namespaces|--all-namespaces=true) list=true ;;
            -l|--selector|--field-selector) list=true; shift ;;
            -l*|--selector=*|--field-selector=*) list=true ;;
            --ignore-not-found|--ignore-not-found=true) ignore=true ;;
            --raw) raw="$1"; shift ;;
            --raw=*) raw="${a#*=}" ;;
            -*) ;;
            *)
                pos=$((pos + 1))
                if [ "$pos" -eq 1 ]; then res="$a"; else name="$a"; fi ;;
        esac
    done
    if [ -n "$raw" ]; then
        file="$LLMSM_RENDER_WORK/virt/raw$(printf '%s' "$raw" | sed 's/[^A-Za-z0-9._-]/_/g').json"
        if [ -f "$file" ]; then cat "$file"; return 0; fi
        llmsm_render_real_kubectl get "${args[@]}"
        return
    fi
    case "$res" in */*) name="${res#*/}"; res="${res%%/*}" ;; esac
    case "$res" in *,*) list=true ;; esac
    if [ -n "$name" ] && [ "$list" = false ]; then
        rc=0
        file="$(llmsm_render_find "$res" "$name" "$ns")" || rc=$?
        if [ "$rc" -eq 0 ]; then
            llmsm_render_print "$file" "$fmt"
            return
        fi
        if [ "$rc" -eq 2 ] || { [ -n "$ns" ] && ! llmsm_render_cluster_scoped "$(llmsm_render_kinds "$res" | head -1)" \
                && [ "$(llmsm_render_ns_state "$ns")" = no ]; } || llmsm_render_virtual_crd "$res"; then
            # Deleted by this render, in a namespace the cluster does not have
            # yet, or of a type only a chart this render installs serves.
            [ "$ignore" = true ] && return 0
            echo "Error from server (NotFound): $res \"$name\" not found" >&2
            return 1
        fi
        llmsm_render_real_kubectl get "${args[@]}"
        return
    fi
    if [ -n "$res" ] && [ "${res#*,}" = "$res" ]; then
        llmsm_render_list "$res" "$ns" "$fmt" "${args[@]}"
        return
    fi
    llmsm_render_real_kubectl get "${args[@]}"
}

# llmsm_render_local_items echoes the store and chart files of resource $1 in
# namespace $2 ("" for every namespace), stored objects first.
llmsm_render_local_items() {
    local res="$1" ns="$2" kind f key k rest seen=" "
    for kind in $(llmsm_render_kinds "$res" | sort -u); do
        for f in "$LLMSM_RENDER_WORK"/obj/"${kind}"__*.yaml "$LLMSM_RENDER_WORK"/virt/"${kind}"__*.yaml; do
            [ -f "$f" ] || continue
            key="$(basename "$f" .yaml)"
            case "$seen" in *" $key "*) continue ;; esac
            seen="$seen$key "
            grep -qxF "$key" "$LLMSM_RENDER_WORK/gone" && continue
            k="${key%%__*}"; rest="${key#*__}"
            [ "$k" = "$kind" ] || continue
            if [ -n "$ns" ] && ! llmsm_render_cluster_scoped "$kind"; then
                [ "${rest%%__*}" = "$(printf '%s' "$ns" | sed 's/[^A-Za-z0-9._-]/_/g')" ] || continue
            fi
            printf '%s\n' "$f"
        done
    done
}

# llmsm_render_selector_jq echoes a jq condition for kubectl label selector $1:
# key=value, key==value, key!=value and bare key, comma-separated.
llmsm_render_selector_jq() {
    local sel="$1" term cond="true" k v
    local IFS=,
    for term in $sel; do
        case "$term" in
            *!=*) k="${term%%!=*}"; v="${term#*!=}"; cond="$cond and ((.metadata.labels // {})[\"$k\"] != \"$v\")" ;;
            *==*) k="${term%%==*}"; v="${term#*==}"; cond="$cond and ((.metadata.labels // {})[\"$k\"] == \"$v\")" ;;
            *=*)  k="${term%%=*}";  v="${term#*=}";  cond="$cond and ((.metadata.labels // {})[\"$k\"] == \"$v\")" ;;
            *)    cond="$cond and ((.metadata.labels // {}) | has(\"$term\"))" ;;
        esac
    done
    printf '%s' "$cond"
}

# llmsm_render_list answers a list read of resource $1 in namespace $2 (output
# format $3; the original arguments follow): the cluster's items, with what this
# render stored -- or a chart it records creates -- merged in, and what it
# deleted taken out. A read of a type only such a chart serves, or in a
# namespace the cluster does not have yet, has no cluster part.
llmsm_render_list() {
    local res="$1" ns="$2" fmt="$3" a sel="" all=false tmp files f cluster="" rc=0 tpl inner out
    shift 3
    local args=("$@")
    while [ $# -gt 0 ]; do
        a="$1"; shift
        case "$a" in
            -l|--selector) sel="$1"; shift ;;
            -l=*|--selector=*) sel="${a#*=}" ;;
            -l*) sel="${a#-l}" ;;
            -A|--all-namespaces|--all-namespaces=true) all=true ;;
        esac
    done
    [ "$all" = false ] || ns=""
    files="$(llmsm_render_local_items "$res" "$ns")"
    if [ -z "$files" ] && ! llmsm_render_virtual_crd "$res"; then
        if [ -n "$ns" ] && ! llmsm_render_cluster_scoped "$(llmsm_render_kinds "$res" | head -1)" \
            && [ "$(llmsm_render_ns_state "$ns")" = no ]; then
            llmsm_render_empty_list "$fmt"
            return 0
        fi
        llmsm_render_real_kubectl get "${args[@]}"
        return
    fi
    tmp="$(mktemp -d)" || return 1
    if ! llmsm_render_virtual_crd "$res" \
        && { [ -z "$ns" ] || llmsm_render_cluster_scoped "$(llmsm_render_kinds "$res" | head -1)" \
             || [ "$(llmsm_render_ns_state "$ns")" != no ]; }; then
        if [ -n "$ns" ]; then
            cluster="$(llmsm_render_real_kubectl get "$res" -n "$ns" ${sel:+-l "$sel"} -o json 2>"$tmp/err")" || rc=$?
        else
            cluster="$(llmsm_render_real_kubectl get "$res" -A ${sel:+-l "$sel"} -o json 2>"$tmp/err")" || rc=$?
        fi
        if [ "$rc" -ne 0 ]; then
            # Forbidden is not absent: the list fails as it would have.
            cat "$tmp/err" >&2
            rm -rf "$tmp"
            return "$rc"
        fi
    fi
    printf '%s' "${cluster:-}" | jq -c '.items[]?' > "$tmp/items" 2>/dev/null || : > "$tmp/items"
    : > "$tmp/local"
    while read -r f; do
        [ -n "$f" ] || continue
        yq -o=json -I=0 '.' "$f" >> "$tmp/local"
    done <<< "$files"
    # The store key (llmsm_render_key) of each item: an object this render wrote
    # replaces the cluster's, and one it deleted is left out.
    jq -c -n --slurpfile loc "$tmp/local" --slurpfile cl "$tmp/items" \
        --argjson gone "$(jq -R -s -c 'split("\n") | map(select(. != ""))' "$LLMSM_RENDER_WORK/gone")" '
        def key: [((.kind // "") | ascii_downcase), ((.metadata.namespace // "") | if . == "" then "_" else . end), (.metadata.name // "")]
                 | join("__") | gsub("[^A-Za-z0-9._-]"; "_");
        ($loc | map(key)) as $lk
        | {apiVersion: "v1", kind: "List", metadata: {},
           items: (($cl | map(select(key as $k | ($lk | index($k)) == null and ($gone | index($k)) == null))) + $loc)}' > "$tmp/list.json" \
        || { rm -rf "$tmp"; echo "RENDER: could not merge the list of $res" >&2; return 1; }
    if [ -n "$sel" ]; then
        jq -c "{apiVersion, kind, metadata, items: [.items[] | select($(llmsm_render_selector_jq "$sel"))]}" "$tmp/list.json" > "$tmp/l2" \
            && mv "$tmp/l2" "$tmp/list.json"
    fi
    case "$fmt" in
        json) cat "$tmp/list.json"; echo ;;
        yaml) yq -p=json -o=yaml -P '.' "$tmp/list.json" ;;
        name) jq -r '.items[] | ((.kind | ascii_downcase) + "/" + .metadata.name)' "$tmp/list.json" ;;
        "")
            if [ "$(jq '.items | length' "$tmp/list.json")" = 0 ]; then
                echo "No resources found" >&2
            else
                echo "NAME"; jq -r '.items[].metadata.name' "$tmp/list.json"
            fi ;;
        jsonpath=*|go-template=*)
            tpl="${fmt#*=}"; inner=""
            case "$fmt" in
                jsonpath=*)
                    case "$tpl" in
                        "{range .items[*]}"*"{end}") inner="${tpl#"{range .items[*]}"}"; inner="${inner%"{end}"}" ;;
                    esac ;;
                go-template=*)
                    case "$tpl" in
                        "{{range .items}}"*"{{end}}") inner="${tpl#"{{range .items}}"}"; inner="${inner%"{{end}}"}" ;;
                    esac ;;
            esac
            if [ -n "$inner" ]; then
                jq -c '.items[]' "$tmp/list.json" | while IFS= read -r out; do
                    printf '%s\n' "$out" > "$tmp/one.json"
                    llmsm_render_local_kubectl annotate --local -f "$tmp/one.json" scaling-manager.llmd.ai/render-noop- -o "${fmt%%=*}=$inner"
                done
            elif [ "${tpl#*".items[*]"}" != "$tpl" ]; then
                # {.items[*].x}: x of every item, space-separated, as kubectl prints it.
                tpl="${tpl//".items[*]"/}"
                jq -c '.items[]' "$tmp/list.json" | while IFS= read -r out; do
                    printf '%s\n' "$out" > "$tmp/one.json"
                    llmsm_render_local_kubectl annotate --local -f "$tmp/one.json" scaling-manager.llmd.ai/render-noop- -o "jsonpath=$tpl"
                done | paste -sd' ' -
            else
                # {.items[0].x}: the first item of the merged list.
                tpl="${tpl//".items[0]"/}"
                jq '.items[0] // empty' "$tmp/list.json" > "$tmp/one.json"
                if [ -s "$tmp/one.json" ]; then
                    llmsm_render_local_kubectl annotate --local -f "$tmp/one.json" scaling-manager.llmd.ai/render-noop- -o "${fmt%%=*}=$tpl"
                fi
            fi ;;
        *) llmsm_render_real_kubectl get "${args[@]}" ;;
    esac
    rm -rf "$tmp"
    return 0
}

# llmsm_render_why echoes why the current command is needed, from the function
# that issued it, for the comment above its line in admin.sh.
llmsm_render_why() {
    local f
    for f in "${FUNCNAME[@]:1}"; do
        case "$f" in
            llmsm_render_*|kubectl|helm|kc_capture) continue ;;
            deploy_keda) echo "KEDA is not on the cluster; the scaling manager needs it to scale anything (KEDA_HELM_INSTALL=true)." ;;
            deploy_prometheus_kube_stack) echo "No Prometheus on the cluster; the scaling manager reads its metrics from this one." ;;
            deploy_scaling_manager_prerequisites_kube_like|deploy_scaling_manager_prerequisites) echo "A dependency of the scaling manager that the cluster does not have." ;;
            _install_crd_group|install_inference_crds) echo "CRDs the scaling manager watches and the cluster does not have." ;;
            llmsm_repair_immutable_rolerefs) echo "roleRef is immutable: this binding must point at a new role, so it is deleted and admin.yaml recreates it." ;;
            llmsm_revoke_namespace|llmsm_prune_unlisted_namespaces) echo "This namespace is no longer in LLMSM_WATCH_NS; the controller's grant there is removed." ;;
            llmsm_append_scaler_networkpolicy_patch) echo "LLMSM_SCALER_NETWORKPOLICY=false: the external scaler NetworkPolicy is removed." ;;
            create_namespaces_shared_loop) echo "The namespace is stuck terminating and must finish before it is created again." ;;
            pl_revoke_kueue_borrow_read|pl_sync_kueue_borrow_read) echo "No policy of this install counts Kueue borrowing, so its cluster-wide Workload read is revoked." ;;
            pl_revoke_kueue_controller_read) echo "The Kueue controller read is no longer needed." ;;
            llmsm_scaler_keda_*|llmsm_scaler_tls_*) echo "KEDA keeps the scaler's client certificate until its operator restarts." ;;
            llmsm_revoke_pod_list_grant) echo "gpu-inventory is no longer declared; the cluster-wide Pod list is revoked." ;;
            *) echo "From ${f} (deploy/lib)." ;;
        esac
        return 0
    done
    echo "From the setup-prereqs phase."
}

# llmsm_render_record appends command "$@" to section $1 (pre: before admin.yaml
# is applied, post: after), with its reason, and says so.
llmsm_render_record() {
    local section="$1" why line n
    shift
    why="$(llmsm_render_why)"
    line="$(llmsm_render_quote "$@")"
    # One file per command: a quoted argument may span lines.
    n="$(find "$LLMSM_RENDER_WORK/$section.d" -type f | wc -l | tr -d ' ')"
    printf '# %s\n%s\n' "$why" "$line" > "$LLMSM_RENDER_WORK/$section.d/$(printf '%05d' "$((n + 1))")"
    log_info "RENDER: recorded in admin.sh: $(printf '%s' "$line" | head -1 | cut -c1-200)"
}

# llmsm_render_capture reads manifests from file $1 (YAML or JSON, one or many
# documents, Lists expanded) into the store, defaulting a namespaced object's
# namespace to $2, and prints what it captured as kubectl does.
llmsm_render_capture() {
    local in="$1" defns="$2" tmp f kind ns name key lkind
    tmp="$(mktemp -d)" || return 1
    {
        yq 'select((.kind // "") | test("List$")) | .items[] | split_doc' "$in" 2>/dev/null || true
        echo "---"
        yq 'select(((.kind // "") | test("List$")) | not)' "$in" 2>/dev/null || true
    } > "$tmp/all.yaml"
    yq 'select(. != null and .kind != null)
        | del(.status) | del(.metadata.resourceVersion) | del(.metadata.uid)
        | del(.metadata.creationTimestamp) | del(.metadata.managedFields)
        | del(.metadata.generation) | del(.metadata.selfLink)
        | del(.metadata.annotations["kubectl.kubernetes.io/last-applied-configuration"])
        | del(.metadata.annotations | select(length == 0))
        | del(.metadata.labels | select(length == 0))' "$tmp/all.yaml" > "$tmp/clean.yaml" \
        || { rm -rf "$tmp"; echo "RENDER: could not parse the manifests to capture" >&2; return 1; }
    mkdir -p "$tmp/docs"
    # -P: JSON input (jq-built Secrets) comes out as block YAML, for review.
    D="$tmp/docs" yq -P -s 'strenv(D) + "/" + ($index | tostring)' 'select(. != null)' "$tmp/clean.yaml" 2>/dev/null || true
    local i=0
    while [ -f "$tmp/docs/$i.yml" ]; do
        f="$tmp/docs/$i.yml"
        i=$((i + 1))
        [ -s "$f" ] || continue
        kind="$(yq '.kind // ""' "$f")"; name="$(yq '.metadata.name // ""' "$f")"; ns="$(yq '.metadata.namespace // ""' "$f")"
        [ -n "$kind" ] && [ -n "$name" ] || continue
        lkind="$(llmsm_render_lower "$kind")"
        if [ -z "$ns" ] && [ -n "$defns" ] && ! llmsm_render_cluster_scoped "$lkind"; then
            ns="$defns"
            NS="$ns" yq -i '.metadata.namespace = strenv(NS)' "$f"
        fi
        key="$(llmsm_render_key "$kind" "$ns" "$name")"
        # One document per file, without its separators: admin.yaml adds them.
        grep -vxE -- '---|\.\.\.' "$f" > "$LLMSM_RENDER_WORK/obj/$key.yaml" || true
        grep -qxF "$key" "$LLMSM_RENDER_WORK/order" || echo "$key" >> "$LLMSM_RENDER_WORK/order"
        if grep -qxF "$key" "$LLMSM_RENDER_WORK/gone"; then
            grep -vxF "$key" "$LLMSM_RENDER_WORK/gone" > "$tmp/gone" || true
            cat "$tmp/gone" > "$LLMSM_RENDER_WORK/gone"
        fi
        printf '%s/%s rendered\n' "$lkind" "$name"
    done
    rm -rf "$tmp"
}

# llmsm_render_apply captures `kubectl apply|create -f/-k`. A remote file or
# kustomization is recorded as a command instead.
llmsm_render_apply() {
    local a ns="" dry="" srcs=() kdirs=() src tmp rc=0
    local args=("$@")
    while [ $# -gt 0 ]; do
        a="$1"; shift
        case "$a" in
            --context|--kubeconfig) shift ;;
            -f|--filename) srcs+=("$1"); shift ;;
            -f=*|--filename=*) srcs+=("${a#*=}") ;;
            -k|--kustomize) kdirs+=("$1"); shift ;;
            -k=*|--kustomize=*) kdirs+=("${a#*=}") ;;
            -n|--namespace) ns="$1"; shift ;;
            --namespace=*) ns="${a#*=}" ;;
            --dry-run=*) dry="${a#*=}" ;;
            --dry-run) dry=client ;;
            --field-manager|-o|--output|--selector|-l) shift ;;
        esac
    done
    case "$dry" in
        server)
            # Validation the admin's own apply repeats. The cluster cannot
            # validate objects whose namespace or type only this render makes.
            for src in ${srcs[@]+"${srcs[@]}"}; do [ "$src" != - ] || cat > /dev/null; done
            return 0 ;;
        client) llmsm_render_real_kubectl apply "${args[@]}"; return ;;
    esac
    tmp="$(mktemp)" || return 1
    # Every source ends with a newline before the separator: a document that
    # does not end in one would otherwise swallow the `---`.
    for src in ${srcs[@]+"${srcs[@]}"}; do
        case "$src" in
            -) cat >> "$tmp"; printf '\n---\n' >> "$tmp" ;;
            http://*|https://*) llmsm_render_record pre kubectl apply -f "$src" ;;
            *)
                if [ -d "$src" ]; then
                    cat "$src"/*.yaml "$src"/*.yml "$src"/*.json 2>/dev/null >> "$tmp" || true
                else
                    cat "$src" >> "$tmp" || { rm -f "$tmp"; return 1; }
                fi
                printf '\n---\n' >> "$tmp" ;;
        esac
    done
    for src in ${kdirs[@]+"${kdirs[@]}"}; do
        if [ -d "$src" ]; then
            llmsm_render_local_kubectl kustomize "$src" >> "$tmp" || { rm -f "$tmp"; return 1; }
            printf '\n---\n' >> "$tmp"
        else
            llmsm_render_record pre kubectl apply -k "$src"
        fi
    done
    llmsm_render_capture "$tmp" "$ns" || rc=$?
    rm -f "$tmp"
    return "$rc"
}

# llmsm_render_create captures `kubectl create <type> ...` through its own
# client-side dry run.
llmsm_render_create() {
    local a out rc=0 tmp
    for a in "$@"; do
        case "$a" in
            --dry-run|--dry-run=*) llmsm_render_real_kubectl create "$@"; return ;;
            -f|--filename|-f=*|--filename=*|-k|--kustomize) llmsm_render_apply "$@"; return ;;
        esac
    done
    tmp="$(mktemp)" || return 1
    llmsm_render_real_kubectl create "$@" --dry-run=client -o yaml > "$tmp" || rc=$?
    if [ "$rc" -eq 0 ]; then
        llmsm_render_capture "$tmp" "" || rc=$?
    fi
    rm -f "$tmp"
    return "$rc"
}

# llmsm_render_target parses `<verb> <resource> <name> -n <ns>` style arguments
# into RT_RES, RT_NAME, RT_NS (RT_SELECTOR when a label selector is given).
llmsm_render_target() {
    local a pos=0
    RT_RES="" RT_NAME="" RT_NS="" RT_SELECTOR="" RT_ALL=false RT_SLASH=false
    while [ $# -gt 0 ]; do
        a="$1"; shift
        case "$a" in
            -n|--namespace) RT_NS="$1"; shift ;;
            --namespace=*) RT_NS="${a#*=}" ;;
            -l|--selector) RT_SELECTOR="$1"; shift ;;
            -l=*|--selector=*) RT_SELECTOR="${a#*=}" ;;
            -A|--all-namespaces) RT_ALL=true ;;
            -p|--patch|--type|--patch-file|-o|--output|--timeout|--field-manager|--grace-period|--context|--kubeconfig) shift ;;
            -*) ;;
            *)
                pos=$((pos + 1))
                if [ "$pos" -eq 1 ]; then RT_RES="$a"; elif [ "$pos" -eq 2 ] && [ "$RT_SLASH" = false ]; then RT_NAME="$a"; fi
                case "$pos:$a" in 1:*/*) RT_SLASH=true; RT_NAME="${a#*/}"; RT_RES="${a%%/*}" ;; esac ;;
        esac
    done
}

# llmsm_render_edit handles patch/label/annotate (verb $1): on a stored object it
# is applied to the stored copy; on an object already on the cluster it is
# recorded for admin.sh.
llmsm_render_edit() {
    local verb="$1" file rc=0 tmp
    shift
    llmsm_render_target "$@"
    if [ -n "$RT_NAME" ] && [ -z "$RT_SELECTOR" ] && file="$(llmsm_render_find "$RT_RES" "$RT_NAME" "$RT_NS")" \
        && [ "${file#"$LLMSM_RENDER_WORK/obj/"}" != "$file" ]; then
        local rest=() a skip=0 pos=0
        for a in "$@"; do
            if [ "$skip" = 1 ]; then skip=0; continue; fi
            case "$a" in
                -n|--namespace|-o|--output) skip=1; continue ;;
                --namespace=*|-o*|--output=*) continue ;;
                -*) rest+=("$a"); continue ;;
            esac
            pos=$((pos + 1))
            # The resource and name are replaced by -f; any other positional
            # (a label, an annotation) stays.
            if [ "$RT_SLASH" = true ] && [ "$pos" -le 1 ]; then continue; fi
            if [ "$RT_SLASH" = false ] && [ "$pos" -le 2 ]; then continue; fi
            rest+=("$a")
        done
        tmp="$(mktemp)" || return 1
        if llmsm_render_local_kubectl "$verb" --local -f "$file" ${rest[@]+"${rest[@]}"} -o yaml > "$tmp"; then
            llmsm_render_capture "$tmp" "" >/dev/null || rc=$?
            printf '%s/%s %s (rendered)\n' "$(llmsm_render_lower "$RT_RES")" "$RT_NAME" "$verb"
        else
            rc=1
        fi
        rm -f "$tmp"
        return "$rc"
    fi
    llmsm_render_record pre kubectl "$verb" "$@"
}

# llmsm_render_delete records a delete for admin.sh when there is something to
# delete, drops a stored object of that name, and remembers the name as gone.
llmsm_render_delete() {
    local args=("$@") a has_ignore=false out rc=0 file key kind res
    for a in "$@"; do
        case "$a" in --ignore-not-found|--ignore-not-found=true) has_ignore=true ;; esac
    done
    llmsm_render_target "$@"
    if [ -n "$RT_NAME" ] && [ -z "$RT_SELECTOR" ]; then
        for res in ${RT_RES//,/ }; do
            rc=0
            file="$(llmsm_render_find "$res" "$RT_NAME" "$RT_NS")" || rc=$?
            if [ "$rc" -eq 0 ] && [ "${file#"$LLMSM_RENDER_WORK/obj/"}" != "$file" ]; then
                key="$(basename "$file" .yaml)"
                rm -f "$file"
                grep -vxF "$key" "$LLMSM_RENDER_WORK/order" > "$LLMSM_RENDER_WORK/order.tmp" || true
                mv "$LLMSM_RENDER_WORK/order.tmp" "$LLMSM_RENDER_WORK/order"
            fi
            for kind in $(llmsm_render_kinds "$res" | head -1); do
                key="$(llmsm_render_key "$kind" "$(llmsm_render_cluster_scoped "$kind" || printf '%s' "$RT_NS")" "$RT_NAME")"
                grep -qxF "$key" "$LLMSM_RENDER_WORK/gone" || echo "$key" >> "$LLMSM_RENDER_WORK/gone"
            done
        done
    fi
    # Only what would delete something: a delete of nothing is noise in a
    # script an admin has to review. An unreadable answer is kept.
    rc=0
    if [ -n "$RT_NAME" ]; then
        if [ -n "$RT_NS" ] && [ "$(llmsm_render_ns_state "$RT_NS")" = no ]; then
            out=""
        else
            # A NotFound for one of several resources is not an error about
            # the others; anything else is kept, and the delete recorded.
            out="$(llmsm_render_real_kubectl get "$RT_RES" "$RT_NAME" ${RT_NS:+-n "$RT_NS"} -o name 2>&1)" || rc=$?
            if [ "$rc" -ne 0 ]; then
                out="$(printf '%s\n' "$out" | grep -vE '\(NotFound\)|the server doesn.t have a resource type' || true)"
                rc=0
            fi
        fi
    elif [ -n "$RT_SELECTOR" ]; then
        if [ "$RT_ALL" = true ]; then
            out="$(llmsm_render_real_kubectl get "$RT_RES" -A -l "$RT_SELECTOR" -o name 2>&1)" || rc=$?
        elif [ -n "$RT_NS" ] && [ "$(llmsm_render_ns_state "$RT_NS")" = no ]; then
            out=""
        else
            out="$(llmsm_render_real_kubectl get "$RT_RES" ${RT_NS:+-n "$RT_NS"} -l "$RT_SELECTOR" -o name 2>&1)" || rc=$?
        fi
        if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -qE 'the server doesn.t have a resource type'; then
            out=""; rc=0
        fi
    else
        out=unknown; rc=1
    fi
    if [ "$rc" -eq 0 ] && [ -z "$out" ]; then
        printf '%s "%s" not found, nothing to delete (rendered)\n' "$RT_RES" "${RT_NAME:-$RT_SELECTOR}" >&2
        return 0
    fi
    local section=pre
    if [ "$has_ignore" = true ]; then
        llmsm_render_record "$section" kubectl delete "${args[@]}"
    else
        llmsm_render_record "$section" kubectl delete "${args[@]}" --ignore-not-found
    fi
    printf '%s "%s" deleted (rendered)\n' "$RT_RES" "${RT_NAME:-$RT_SELECTOR}"
}

# llmsm_render_can_i answers `kubectl auth can-i <args>` in render mode. A
# question about another identity (--as), or about a read, goes to the cluster:
# this run reads with the caller's own rights, and a read it may not make must
# be answered as such. A question about a write is about the cluster admin who
# runs admin.sh, and is answered yes.
llmsm_render_can_i() {
    local a verb="" ns="" skip=0
    for a in "$@"; do
        case "$a" in
            --as|--as=*) llmsm_render_real_kubectl auth "$@"; return ;;
        esac
    done
    for a in "$@"; do
        if [ "$skip" = 1 ]; then skip=0; ns="$a"; continue; fi
        case "$a" in
            -n|--namespace) skip=1; continue ;;
            --namespace=*) ns="${a#*=}"; continue ;;
            can-i|-*) continue ;;
        esac
        [ -n "$verb" ] || verb="$a"
    done
    case "$verb" in
        get|list|watch)
            # In a namespace the cluster does not have yet, every read is
            # answered by this render's own objects.
            if [ -n "$ns" ] && [ "$(llmsm_render_ns_state "$ns")" = no ]; then
                echo yes
                return 0
            fi
            llmsm_render_real_kubectl auth "$@" ;;
        *) echo yes ;;
    esac
}

# llmsm_render_kubectl is `kubectl` in render mode.
llmsm_render_kubectl() {
    local verb="" a i=0 takes=false pre=() rest=()
    for a in "$@"; do
        i=$((i + 1))
        if [ "$takes" = true ]; then takes=false; continue; fi
        case "$a" in
            -n|--namespace|--context|--kubeconfig|--request-timeout|-s|--server|--cluster|--user) takes=true; continue ;;
            -*) continue ;;
        esac
        verb="$a"
        break
    done
    case " $* " in
        *" --local "*|*" --local=true "*) llmsm_render_local_kubectl "$@"; return ;;
    esac
    if [ -n "$verb" ]; then
        pre=("${@:1:i-1}")
        rest=("${@:i+1}")
    fi
    set -- ${pre[@]+"${pre[@]}"} ${rest[@]+"${rest[@]}"}
    case "$verb" in
        get) llmsm_render_get "$@" ;;
        apply) llmsm_render_apply "$@" ;;
        create) llmsm_render_create "$@" ;;
        patch|label|annotate) llmsm_render_edit "$verb" "$@" ;;
        delete) llmsm_render_delete "$@" ;;
        kustomize) llmsm_render_local_kubectl kustomize "$@" ;;
        auth)
            case " $* " in
                *" can-i "*) llmsm_render_can_i "$@" ;;
                *" reconcile "*) llmsm_render_record pre kubectl auth "$@" ;;
                *) llmsm_render_real_kubectl auth "$@" ;;
            esac ;;
        wait)
            log_info "RENDER: skipped 'kubectl wait $*': it waits on objects that are only rendered."
            return 0 ;;
        rollout)
            case " $* " in
                *" status "*|*" history "*)
                    log_info "RENDER: skipped 'kubectl rollout $*': it checks objects that are only rendered."
                    return 0 ;;
                *) llmsm_render_record post kubectl rollout "$@" ;;
            esac ;;
        replace|scale|set|edit|taint|cordon|uncordon|drain|certificate|run|expose|autoscale|exec|cp|attach|debug)
            log_warning "RENDER: 'kubectl $verb' is not rendered as an object; recorded as a command for the admin to review."
            llmsm_render_record pre kubectl "$verb" "$@" ;;
        "") llmsm_render_real_kubectl "$@" ;;
        *) llmsm_render_real_kubectl "$verb" "$@" ;;
    esac
}

# llmsm_render_chart records what a Helm chart install makes exist, for the reads
# after it: its CRDs and, for KEDA, the API the scaler TLS asks for.
llmsm_render_chart() {
    local chart="$1" ns="$2" crd
    case "$chart" in
        *kedacore/keda|*/keda)
            for crd in scaledobjects.keda.sh scaledjobs.keda.sh triggerauthentications.keda.sh clustertriggerauthentications.keda.sh; do
                llmsm_render_virtual_object CustomResourceDefinition "" "$crd"
            done
            printf '{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"keda.sh/v1alpha1","resources":[{"name":"scaledobjects","namespaced":true,"kind":"ScaledObject"},{"name":"scaledjobs","namespaced":true,"kind":"ScaledJob"},{"name":"triggerauthentications","namespaced":true,"kind":"TriggerAuthentication"},{"name":"clustertriggerauthentications","namespaced":false,"kind":"ClusterTriggerAuthentication"}]}\n' \
                > "$LLMSM_RENDER_WORK/virt/raw_apis_keda.sh_v1alpha1.json"
            llmsm_render_virtual_object APIService "" v1beta1.external.metrics.k8s.io \
                ".spec.service.namespace = \"$ns\" | .spec.service.name = \"keda-operator-metrics-apiserver\""
            llmsm_render_virtual_object Deployment "$ns" keda-operator
            ;;
        *kube-prometheus-stack)
            for crd in prometheuses podmonitors servicemonitors prometheusrules alertmanagers alertmanagerconfigs probes thanosrulers scrapeconfigs prometheusagents; do
                llmsm_render_virtual_object CustomResourceDefinition "" "$crd.monitoring.coreos.com"
            done
            # The Prometheus the chart creates, as deploy_prometheus_kube_stack
            # configures it: served over TLS from PROMETHEUS_SECRET_NAME, behind
            # its own Service. Read later to pick its URL and copy its CA.
            llmsm_render_virtual_object Prometheus "$ns" kube-prometheus-stack-prometheus \
                ".apiVersion = \"monitoring.coreos.com/v1\"
                 | .spec.web.tlsConfig.cert.secret.name = \"${PROMETHEUS_SECRET_NAME:-prometheus-web-tls}\"
                 | .spec.web.tlsConfig.cert.secret.key = \"tls.crt\""
            llmsm_render_virtual_object Service "$ns" kube-prometheus-stack-prometheus \
                ".spec.selector[\"operator.prometheus.io/name\"] = \"kube-prometheus-stack-prometheus\"
                 | .spec.ports = [{\"name\": \"http-web\", \"port\": ${PROMETHEUS_PORT:-9090}}]"
            ;;
        *lws)
            llmsm_render_virtual_object CustomResourceDefinition "" leaderworkersets.leaderworkerset.x-k8s.io ;;
    esac
}

# llmsm_render_virtual_object stores object kind $1 / namespace $2 / name $3,
# with yq edit $4, as one a recorded chart creates. Never written to admin.yaml.
llmsm_render_virtual_object() {
    local kind="$1" ns="$2" name="$3" edit="${4:-.}" key api=v1
    key="$(llmsm_render_key "$kind" "$ns" "$name")"
    case "$kind" in
        CustomResourceDefinition) api=apiextensions.k8s.io/v1 ;;
        APIService) api=apiregistration.k8s.io/v1 ;;
        Deployment) api=apps/v1 ;;
    esac
    A="$api" K="$kind" N="$name" NS="$ns" yq -n '.apiVersion = strenv(A) | .kind = strenv(K) | .metadata.name = strenv(N)
        | .metadata.namespace = strenv(NS) | del(.metadata | select(.namespace == "") | .namespace)' \
        | yq "$edit" > "$LLMSM_RENDER_WORK/virt/$key.yaml"
    [ "$kind" != CustomResourceDefinition ] || echo "$name" >> "$LLMSM_RENDER_WORK/virt-crds"
}

# llmsm_render_helm is `helm` in render mode: installs, upgrades, uninstalls and
# repository changes are recorded; reads go to the cluster.
llmsm_render_helm() {
    local verb="" a ns="" chart="" pos=0 skip=0
    for a in "$@"; do
        if [ "$skip" = 1 ]; then skip=0; ns="$a"; continue; fi
        case "$a" in
            -n|--namespace) skip=1; continue ;;
            --namespace=*) ns="${a#*=}"; continue ;;
            -*) continue ;;
        esac
        pos=$((pos + 1))
        [ "$pos" -eq 1 ] && verb="$a"
        [ "$pos" -eq 3 ] && chart="$a"
    done
    case "$verb" in
        upgrade|install)
            llmsm_render_record pre helm "$@"
            llmsm_render_chart "$chart" "$ns"
            echo "Release rendered: helm $verb (recorded in admin.sh)" ;;
        uninstall|delete|rollback)
            llmsm_render_record pre helm "$@" ;;
        repo)
            case " $* " in
                *" add "*) llmsm_render_record pre helm "$@" --force-update ;;
                *" update"*) llmsm_render_record pre helm "$@" ;;
                *) llmsm_render_real_helm "$@" ;;
            esac ;;
        list|ls|status|get|history)
            # A release cannot be in a namespace that does not exist yet.
            if [ -n "$ns" ] && [ "$(llmsm_render_ns_state "$ns")" = no ]; then
                case "$verb" in
                    list|ls) case " $* " in *" json "*|*"-o=json"*|*"--output=json"*) echo "[]" ;; esac; return 0 ;;
                    *) echo "Error: release: not found" >&2; return 1 ;;
                esac
            fi
            llmsm_render_real_helm "$@" ;;
        *) llmsm_render_real_helm "$@" ;;
    esac
}

# llmsm_render_kind_rank echoes the apply-order rank of lower-case kind $1:
# namespaces, then CRDs and RBAC, then what monitors and bindings reference.
llmsm_render_kind_rank() {
    case "$1" in
        namespace) echo 0 ;; customresourcedefinition) echo 1 ;; clusterrole) echo 2 ;;
        clusterrolebinding) echo 3 ;; role) echo 4 ;; rolebinding) echo 5 ;;
        serviceaccount) echo 6 ;; secret) echo 7 ;; configmap) echo 8 ;; *) echo 9 ;;
    esac
}


# llmsm_render_commands prints the recorded commands of section $1 (pre or post),
# in the order they were recorded, each under its reason.
llmsm_render_commands() {
    local f
    for f in "$LLMSM_RENDER_WORK/$1.d"/*; do
        [ -f "$f" ] || continue
        cat "$f"
    done
}

# llmsm_render_finish writes admin.yaml and admin.sh from the store and the
# recorded commands, and prints what was rendered and what the admin runs.
llmsm_render_finish() {
    llmsm_render_active || return 0
    local out="$LLMSM_RENDER_DIR" w="$LLMSM_RENDER_WORK" key n=0 ctx server has_helm=false secrets=0 line first=true
    : > "$w/sorted"
    while read -r key; do
        [ -n "$key" ] && [ -f "$w/obj/$key.yaml" ] || continue
        n=$((n + 1))
        printf '%s %06d %s\n' "$(llmsm_render_kind_rank "${key%%__*}")" "$n" "$key" >> "$w/sorted"
    done < "$w/order"
    ctx="${LLMSM_KUBE_CONTEXT:-$(llmsm_render_real_kubectl config current-context 2>/dev/null || true)}"
    server="$(llmsm_render_real_kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}' 2>/dev/null || true)"
    {
        echo "# Rendered by make setup-prereqs RENDER=<dir>, $(date -u +%Y-%m-%dT%H:%M:%SZ)"
        echo "# Cluster read: ${ctx:-unknown} ${server}"
        echo "# Controller namespace: ${LLMSM_NS}, scope: $(llmsm_install_scope)${LLMSM_WATCH_NS:+, manages: $LLMSM_WATCH_NS}"
        echo "# Every object the cluster-admin phase creates or updates, in apply order."
        echo "# admin.sh applies it."
        while read -r _ _ key; do
            if [ "$first" = true ]; then first=false; else echo "---"; fi
            cat "$w/obj/$key.yaml"
            case "$key" in secret__*) secrets=$((secrets + 1)) ;; esac
        done < <(sort "$w/sorted")
    } > "$out/admin.yaml"
    [ "$secrets" -eq 0 ] || chmod 600 "$out/admin.yaml"
    # Not `| grep -q`: under pipefail its early exit fails the producer.
    case "$(llmsm_render_commands pre)" in
        "helm upgrade "*|"helm install "*|*$'\n'"helm upgrade "*|*$'\n'"helm install "*) has_helm=true ;;
    esac
    {
        echo "#!/usr/bin/env bash"
        echo "# Rendered by make setup-prereqs RENDER=<dir>, $(date -u +%Y-%m-%dT%H:%M:%SZ)"
        echo "# For cluster: ${ctx:-unknown} ${server}"
        echo "# Controller namespace: ${LLMSM_NS}, scope: $(llmsm_install_scope)${LLMSM_WATCH_NS:+, manages: $LLMSM_WATCH_NS}"
        echo "#"
        echo "# The cluster-admin half of the scaling manager install. Review it, then"
        echo "# run it as a cluster admin, against the cluster above:  bash admin.sh"
        echo "# It needs kubectl$( [ "$has_helm" = true ] && echo " and helm") and admin.yaml beside it; no repository checkout."
        echo "set -euo pipefail"
        echo 'cd "$(dirname "$0")"'
        echo 'echo "Applying the scaling manager prerequisites to: $(kubectl config current-context)"'
        echo ""
        if [ -n "$(ls -A "$w/pre.d")" ]; then
            echo "# --- Before the objects ----------------------------------------------------"
            if [ "$has_helm" = true ]; then
                echo "# First pass: the namespaces, RBAC and Secrets the charts below need (the"
                echo "# Prometheus TLS Secret, KEDA's namespace). Objects of kinds only those charts"
                echo "# define cannot apply yet; they fail here and the second pass applies them."
                echo "kubectl apply -f admin.yaml || true"
            fi
            llmsm_render_commands pre
            echo ""
        fi
        echo "# --- The objects -----------------------------------------------------------"
        echo "# Every object the cluster-admin phase creates or updates (admin.yaml)."
        echo "kubectl apply -f admin.yaml"
        if [ -n "$(ls -A "$w/post.d")" ]; then
            echo ""
            echo "# --- After the objects -----------------------------------------------------"
            llmsm_render_commands post
        fi
        echo ""
        echo 'echo "Done. The namespace owner can now install the controller: make deploy-scaling-manager INSTALL_PHASE=scaling-manager"'
    } > "$out/admin.sh"
    chmod +x "$out/admin.sh"
    local forbidden=""
    [ ! -s "$w/forbidden" ] || forbidden="$(sort -u "$w/forbidden")"
    rm -rf "$w"

    echo "" >&2
    log_success "Rendered the cluster-admin phase for $LLMSM_NS into $out. Nothing was applied."
    log_info "  admin.yaml: $(grep -c '^kind: ' "$out/admin.yaml" | tr -d ' ') objects"
    grep -E '^kind: ' "$out/admin.yaml" | sort | uniq -c | sed 's/kind: //' | while read -r n line; do
        log_info "      $n $line"
    done
    log_info "  admin.sh:   $(grep -cE '^(kubectl|helm) ' "$out/admin.sh" | tr -d ' ') commands"
    grep -E '^(kubectl|helm) ' "$out/admin.sh" | while IFS= read -r line; do
        log_info "      $(printf '%s' "$line" | cut -c1-150)"
    done
    if [ "$secrets" -gt 0 ]; then
        log_warning "  admin.yaml holds $secrets Secret(s), private keys among them (it is mode 600). Hand it over privately, and delete it once it is applied."
    fi
    if [ -n "$forbidden" ]; then
        # Forbidden is not absent: the live run, as an admin, would have seen
        # these, and decided on what it saw.
        log_warning "  Some reads were refused for you, so this render may differ from what a cluster admin's run would do:"
        printf '%s\n' "$forbidden" | head -12 | while IFS= read -r line; do log_warning "      $line"; done
        log_warning "  Get read access to them (docs/reference/configuration.md, RENDER) and render again, or have the admin check admin.sh closely."
    fi
    log_info "Give both files to a cluster admin, who reviews them and runs:"
    log_info "    bash $out/admin.sh"
}
