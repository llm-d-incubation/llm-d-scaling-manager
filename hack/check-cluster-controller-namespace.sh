#!/usr/bin/env bash
#
# A cluster-scoped install into a namespace other than the default: one
# setting, CONTROLLER_NAMESPACE (or LLMSM_NS), reaches every step.
#
# Pinned here:
#   - every make step of the cluster-wide utilization-share guide, run with
#     CONTROLLER_NAMESPACE=llm-scaling exported, hands the scripts llm-scaling
#     and never the default; with it unset, none names llm-scaling (the control
#     that shows the grep can fail).
#   - the cluster-scoped overlay rendered for llm-scaling puts every namespaced
#     object there, binds every ClusterRoleBinding to a subject there, and never
#     names the default.
#   - deploy-e2e-infra's scaling-band override patches the ConfigMap in the
#     namespace the install was given, not LLMSM_NS's default.
#   - the kind emulator takes CONTROLLER_NAMESPACE as deploy/install.sh does.
#   - in the guide, the namespace step is the only one naming the default, and
#     every later step that names the controller's namespace reads it.

set -u
# No cluster is read: kubectl renders and refuses the rest (hack/offline-kubectl). make -n
# still runs a recipe line that calls $(MAKE), and those lines call kubectl too.
PATH="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/offline-kubectl:$PATH"; export PATH

# CHECK_ROOT runs the check against another tree -- how its negative control is
# taken: point it at an extracted older tree and the cases must fail.
ROOT="${CHECK_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
FAIL=0
PASS=0
# Every case reports ok or FAIL exactly once; a run that stops early must not
# pass having checked less. Change this deliberately with the cases.
EXPECT_CASES=6
DEFAULT_NS=scaling-manager-system
CTL=llm-scaling
GUIDE="$ROOT/docs/guides/utilization-share-cluster-wide/guide.yaml"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

for tool in yq make; do
    command -v "$tool" >/dev/null 2>&1 || { printf 'FATAL: %s is required.\n' "$tool" >&2; exit 1; }
done

ok()   { PASS=$((PASS + 1)); echo "ok   $1"; }
fail() { FAIL=$((FAIL + 1)); echo "FAIL $1"; [ -n "${2:-}" ] && printf '%s\n' "$2" | sed 's/^/       /'; }

# guide_make_lines prints each `make ...` command of the guide, one per line,
# continuation lines joined.
guide_make_lines() {
    local k
    for k in $(tr -d '\r' < "$GUIDE" | yq -r '.steps | keys | .[]'); do
        tr -d '\r' < "$GUIDE" | yq -r ".steps.$k.run" | sed -e ':a' -e '/\\$/N; s/\\\n[[:space:]]*/ /; ta' \
            | grep -E '^make ' | sed "s/^/$k /"
    done
}

# dry_make <ctl-ns or empty> <make args...>: the recipe make would run.
dry_make() {
    local ctl="$1"
    shift
    (cd "$ROOT" && env -u LLMSM_NS -u CONTROLLER_NAMESPACE ${ctl:+CONTROLLER_NAMESPACE=$ctl} \
        NAMESPACE=team-a ACCELERATOR=NVIDIA-A100-PCIE-80GB GPUS=4 \
        make --no-print-directory -n "$@" 2>&1)
}

# 1-2. Every guide make step hands the scripts the controller namespace. The
# guide's own variables, as a reader sets them, so its words expand.
ACCELERATOR=NVIDIA-A100-PCIE-80GB GPUS=4
lines="$(guide_make_lines)"
n=$(printf '%s\n' "$lines" | grep -c .)
bad="" quiet=""
while read -r step cmd; do
    [ -n "$step" ] || continue
    # shellcheck disable=SC2086 # the guide's own words
    eval "set -- ${cmd#make }"
    out="$(dry_make "$CTL" "$@")"
    if ! printf '%s' "$out" | grep -qE "LLMSM_NS=$CTL([^a-z0-9-]|\$)|--controller-namespace \"$CTL\"" \
        || printf '%s' "$out" | grep -q "$DEFAULT_NS"; then
        bad="$bad $step"
    fi
    out="$(dry_make "" "$@")"
    # The namespace forms only: the image is named llm-d-scaling-manager.
    if printf '%s' "$out" | grep -qE "LLMSM_NS=$CTL([^a-z0-9-]|\$)|--controller-namespace \"$CTL\""; then quiet="$quiet $step"; fi
done <<< "$lines"
if [ "$n" -ge 10 ] && [ -z "$bad" ]; then
    ok "all $n make steps of the guide hand the scripts $CTL, never $DEFAULT_NS"
else
    fail "every guide make step must pass $CTL on (found $n; wrong:${bad:- none})"
fi
if [ "$n" -ge 10 ] && [ -z "$quiet" ]; then
    ok "with CONTROLLER_NAMESPACE unset, none of the $n names $CTL"
else
    fail "with CONTROLLER_NAMESPACE unset, no step may name $CTL (found $n; naming it:${quiet:- none})"
fi

# 3. The cluster-scoped overlay, rendered for llm-scaling.
for lib in common.sh scaledobject.sh infra_monitoring.sh infra_scaling_manager.sh; do
    tr -d '\r' < "$ROOT/deploy/lib/$lib" > "$WORK/$lib"
done
# Offline: the render asks the cluster which Prometheus scrapes the controller, so
# with the caller's kubectl it read whatever cluster the current context named, and
# the result moved with that cluster's state. kubectl is a function here that renders
# and refuses everything else, KUBECONFIG names nothing, and the Prometheus answers a
# caller may have exported are cleared.
render_out="$(cd "$ROOT" && env -u PROMETHEUS_SA -u PROMETHEUS_URL -u PROMETHEUS_URL_EXPLICIT \
    -u LLMSM_PROMETHEUS_URL_RESOLVED -u LLMSM_LIVE_PROM_URL KUBECONFIG=/dev/null bash -c '
    BLUE="" GREEN="" YELLOW="" RED="" NC=""
    kubectl() {
        if [ "$1" = kustomize ]; then
            shift
            if command -v kustomize >/dev/null 2>&1; then command kustomize build "$@"; else command kubectl kustomize "$@"; fi
            return
        fi
        echo "offline check: kubectl $* is not run" >&2
        return 1
    }
    for l in common.sh scaledobject.sh infra_monitoring.sh infra_scaling_manager.sh; do . "'"$WORK"'/$l"; done
    LLMSM_PROJECT=$PWD LLMSM_SCOPE=cluster LLMSM_NS='"$CTL"' ENVIRONMENT=kubernetes
    llmsm_render_manifests' 2>&1)"
printf '%s\n' "$render_out" > "$WORK/render.yaml"
objs=$(grep -c '^kind:' "$WORK/render.yaml")
other_ns="$(yq -r 'select(.metadata.namespace != null) | .metadata.namespace' "$WORK/render.yaml" 2>/dev/null | grep -v '^---$' | sort -u | grep -vx "$CTL" | paste -sd, -)"
crb_subj="$(yq -r 'select(.kind == "ClusterRoleBinding") | .subjects[].namespace' "$WORK/render.yaml" 2>/dev/null | grep -v '^---$' | sort -u | paste -sd, -)"
if [ "$objs" -ge 10 ] && [ -z "$other_ns" ] && [ "$crb_subj" = "$CTL" ] && ! grep -q "$DEFAULT_NS" "$WORK/render.yaml"; then
    ok "the cluster overlay for $CTL: all $objs objects there, every ClusterRoleBinding bound there, no default"
else
    fail "the cluster overlay must render into $CTL (objects=$objs, other namespaces=${other_ns:-none}, CRB subjects=${crb_subj:-none})" "$(head -5 "$WORK/render.yaml")"
fi

# 4. deploy-e2e-infra's override patches where the install went.
mkdir -p "$WORK/mk"
printf '#!/usr/bin/env bash\nexit 0\n' > "$WORK/mk/make"
chmod +x "$WORK/mk/make"
out="$(cd "$ROOT" && env -u LLMSM_NS make --no-print-directory -n deploy-e2e-infra ENVIRONMENT=kind-emulator \
    CONTROLLER_NAMESPACE="$CTL" SKIP_BUILD=true IMG=quay.io/example/scaling-manager:e2e SCALE_UP_THRESHOLD=0.9 \
    MAKE="$WORK/mk/make" 2>&1)"
patch_ns="$(printf '%s\n' "$out" | grep -oE '^NS=[^;]*' | head -1)"
if [ "$patch_ns" = "NS=$CTL" ]; then
    ok "deploy-e2e-infra's scaling-band override patches $CTL, where it installed"
else
    fail "deploy-e2e-infra must patch the policy in $CTL (got '${patch_ns:-no NS= line}')"
fi

# 5. The kind emulator reads CONTROLLER_NAMESPACE.
line="$(tr -d '\r' < "$ROOT/deploy/kind-emulator/install.sh" | grep -E '^LLMSM_NS=' | head -1)"
got="$(env -u LLMSM_NS CONTROLLER_NAMESPACE="$CTL" bash -c "$line"'; echo "$LLMSM_NS"')"
def="$(env -u LLMSM_NS -u CONTROLLER_NAMESPACE bash -c "$line"'; echo "$LLMSM_NS"')"
if [ "$got" = "$CTL" ] && [ "$def" = "$DEFAULT_NS" ]; then
    ok "the kind emulator takes CONTROLLER_NAMESPACE, and keeps the default without it"
else
    fail "deploy/kind-emulator/install.sh must resolve LLMSM_NS from CONTROLLER_NAMESPACE (got '$got', unset '$def')"
fi

# 6. The guide names the default once, in its namespace step, and reads it after.
named="" unread=""
for k in $(tr -d '\r' < "$GUIDE" | yq -r '.steps | keys | .[]'); do
    run="$(tr -d '\r' < "$GUIDE" | yq -r ".steps.$k.run")"
    if printf '%s' "$run" | grep -q "$DEFAULT_NS"; then named="$named $k"; fi
    if printf '%s' "$run" | grep -qE -- '-n +[a-z]' ; then unread="$unread $k"; fi
done
ctl_run="$(tr -d '\r' < "$GUIDE" | yq -r '.steps.controllerns.run')"
if [ "$named" = " controllerns" ] && [ -z "$unread" ] \
    && printf '%s' "$ctl_run" | grep -q "^export CONTROLLER_NAMESPACE=.*$DEFAULT_NS"; then
    ok "the guide names $DEFAULT_NS only in its namespace step, and no later step hardcodes one"
else
    fail "the guide must name the default only in controllerns (named in:${named:- none}; hardcoded -n in:${unread:- none})"
fi

total=$((PASS + FAIL))
echo ""
echo "check-cluster-controller-namespace: $PASS passed, $FAIL failed ($total of $EXPECT_CASES cases)"
if [ "$total" -ne "$EXPECT_CASES" ]; then
    echo "FAIL expected $EXPECT_CASES cases, ran $total"
    exit 1
fi
[ "$FAIL" -eq 0 ]
