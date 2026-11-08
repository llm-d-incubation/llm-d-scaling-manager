#!/usr/bin/env bash
#
# Executes enable_physical_limiter (deploy/lib/physical_limiter.sh) against a
# stubbed kubectl, and asserts WHO it grants cluster-wide node read to.
#
# It exists because the answer used to be "whatever any labelled Deployment
# names". The command found controllers by the label
# app.kubernetes.io/name=scaling-manager, which anyone who can create
# a Deployment can set, and bound the ServiceAccount that Deployment named --
# `default`, say, which every pod in that namespace runs as. A cluster-admin
# command then handed a tenant-chosen account node read across the cluster, and
# reported it as "granted".
#
# What is asserted is the bindings the command would APPLY, read back from the
# manifests the stub receives -- not the source, and not the log line.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FAIL=0
CASES=0

for tool in yq jq; do
    command -v "$tool" >/dev/null 2>&1 || {
        echo "FATAL: $tool is required by this check, and is missing. Nothing below would be about the code."
        exit 2
    }
done

# CR-stripped copies, for the reason hack/check-limiter-declaration.sh gives.
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
for lib in common.sh limiter_policy.sh physical_limiter.sh; do
    tr -d '\r' < "$ROOT/deploy/lib/$lib" > "$WORK/$lib"
done
BLUE=''; GREEN=''; YELLOW=''; RED=''; NC=''
unset LLMSM_QUOTAS LLMSM_QUOTA_SCOPE LLMSM_QUOTA_KUEUE LLMSM_QUOTA_KUEUE_BORROW LLMSM_SCOPE LLMSM_WATCH_NS LLMSM_LIMITER LLMSM_LIMITER_TYPE \
    LLMSM_LIMITER_TARGETS LLMSM_CONTROLLER_SA LLMSM_CONTROLLER_IMAGE_REPOS LLMSM_IMAGE_REPO
LLMSM_NS="scaling-manager-system"
LLMSM_POLICY_NS="scaling-manager-policy"
# shellcheck disable=SC1090
. "$WORK/common.sh"
# shellcheck disable=SC1090
. "$WORK/limiter_policy.sh"
# shellcheck disable=SC1090
. "$WORK/physical_limiter.sh"

case_begin() { CASES=$((CASES + 1)); CASE_FAIL_AT="$FAIL"; }
fail() { echo "FAIL $*"; FAIL=$((FAIL + 1)); }
ok()   { [ "$FAIL" -eq "${CASE_FAIL_AT:-0}" ] || return 0; echo "ok   $*"; }

# ---------------------------------------------------------------------------
# The stub. Deployments come from $KDEPLOYS/<namespace>.json; a namespace named
# in $KFAIL_NS cannot be read. Every applied manifest is reduced to one GRANT
# line per binding: `<binding kind> <binding name> <namespace>/<account>`.
# ---------------------------------------------------------------------------
STUB="$WORK/stub"; mkdir -p "$STUB"
cat > "$STUB/kubectl" <<'STUBEOF'
#!/usr/bin/env bash
printf 'CALL[%s]\n' "$*" >> "$KCALLS"
# KWARN: a warning on stderr on every call, which still SUCCEEDS -- a deprecated
# API, a webhook, client-side throttling. Never data.
[ -z "${KWARN:-}" ] || echo 'Warning: client-side throttling: waited 1.2s, not priority and fairness' >&2
# Parsed, not glob-matched: `-o json` is a prefix of `-o jsonpath=...`, and a
# pattern on the joined string answered every jsonpath read with the whole list.
ns=""; out=""; prev=""; name=""; pos=0
for a in "$@"; do
  case "$prev" in
    -n) ns="$a" ;;
    -o) out="$a" ;;
    -l) : ;;
    *) case "$a" in
         -*) : ;;
         *) pos=$((pos + 1)); [ "$pos" -eq 3 ] && name="$a" ;;   # get deploy NAME
       esac ;;
  esac
  case "$a" in -o*=*) out="${a#-o}" ;; esac
  prev="$a"
done
deploys() {
  case " ${KFAIL_NS:-} " in *" $ns "*) exit 1 ;; esac
  if [ -f "$KDEPLOYS/$ns.json" ]; then cat "$KDEPLOYS/$ns.json"; else printf '{"items":[]}'; fi
}
# Failure injection, each matched against the joined arguments or the applied
# document: KFAIL_APPLY (a substring of the document), KFAIL_PATCH (a substring
# of the arguments),
# KCM_NOTFOUND / KCM_FORBIDDEN (every policy ConfigMap read).
case "$1" in
  auth)
    # KCANI_WATCH_NS: namespaces whose controller account may watch every
    # Namespace -- what a cluster-scoped install gets from its manager
    # ClusterRole. Every other account may not.
    case "$*" in
      "auth can-i watch namespaces --as system:serviceaccount:"*)
        as="$*"; as="${as##*system:serviceaccount:}"; as="${as%%:*}"
        case " ${KCANI_WATCH_NS:-} " in *" $as "*) echo yes; exit 0 ;; esac
        echo no; exit 1 ;;
      # KCANI_LIST_PODS: namespaces whose controller account may list Pods in
      # every namespace; KCANI_LIST_PODS=unknown answers nothing (no
      # impersonation). Every other account may not.
      "auth can-i list pods --all-namespaces --as system:serviceaccount:"*)
        as="$*"; as="${as##*system:serviceaccount:}"; as="${as%%:*}"
        [ "${KCANI_LIST_PODS:-}" != unknown ] || exit 1
        case " ${KCANI_LIST_PODS:-} " in *" $as "*) echo yes; exit 0 ;; esac
        echo no; exit 1 ;;
    esac
    exit 0 ;;
  create)
    printf 'apiVersion: v1\nkind: %s\nmetadata:\n  name: %s\n' "$2" "$3"
    exit 0 ;;
  label) cat; exit 0 ;;
  patch)
    [ -n "${KFAIL_PATCH:-}" ] && [ "$*" != "${*#*"$KFAIL_PATCH"}" ] && { echo "Error from server (Forbidden): cannot patch" >&2; exit 1; }
    exit 0 ;;
  apply)
    doc="$(cat)"
    [ -n "${KFAIL_APPLY:-}" ] && [ "$doc" != "${doc#*"$KFAIL_APPLY"}" ] && { echo "Error from server (Forbidden): cannot apply" >&2; exit 1; }
    printf '%s\n' "$doc" | yq -N e 'select(.kind == "ClusterRoleBinding" or .kind == "RoleBinding") | .kind + " " + .metadata.name + " " + .subjects[0].namespace + "/" + .subjects[0].name' - \
      | sed 's/^/GRANT /' >> "$KGRANTS"
    # The Namespace rule of every ClusterRole: its resourceNames, or * for none.
    printf '%s\n' "$doc" | yq -N e 'select(.kind == "ClusterRole") | .metadata.name + " " + ([.rules[] | select(.resources[] == "namespaces") | ((.resourceNames // ["*"]) | join(","))] | join(";"))' - \
      | sed 's/^/NSRULE /' >> "$KGRANTS"
    # The revoke label of every namespaced Role and RoleBinding, or - for none.
    printf '%s\n' "$doc" | yq -N e 'select(.kind == "Role" or .kind == "RoleBinding") | .kind + " " + .metadata.name + " " + .metadata.namespace + " " + (.metadata.labels["llm-d.ai/scaling-manager-kueue-controller-reader"] // "-")' - \
      | sed 's/^/RBLABEL /' >> "$KGRANTS"
    exit 0 ;;
  get)
    case "$2" in
      deploy)
        if [ "$*" != "${*#*-A}" ]; then printf '%s\n' "${KDEPLOY_NS_LIST:-}"; exit 0; fi
        case "$out" in
          json) deploys ;;
          jsonpath=*)
            # A plain field path is evaluated against the fixture, so the code
            # under test reads what a cluster would answer. Filter expressions
            # (the LLMSM_WATCH_NAMESPACE env lookup) answer empty: no fixture
            # sets one.
            path="${out#jsonpath=}"; path="${path#\{}"; path="${path%\}}"
            case "$path" in *"[?("*) exit 0 ;; esac
            if [ -n "$name" ]; then
              deploys | jq -r --arg n "$name" ".items[] | select(.metadata.name == \$n) | ${path} // empty"
            else
              deploys | jq -r "${path} // empty"
            fi ;;
        esac ;;
      # A namespace with <ns>.policy has that policy; every other one a plain one.
      configmap)
        [ -z "${KCM_FORBIDDEN:-}" ] || { echo 'Error from server (Forbidden): configmaps is forbidden' >&2; exit 1; }
        [ -z "${KCM_NOTFOUND:-}" ] || { echo 'Error from server (NotFound): configmaps "scaling-manager-scaling-policy-config" not found' >&2; exit 1; }
        if [ -f "$KDEPLOYS/$ns.policy" ]; then cat "$KDEPLOYS/$ns.policy"; else printf 'scaleUpThreshold: 0.85'; fi ;;
      # The live ClusterRole <name>: $KDEPLOYS/clusterrole-<name>.yaml, else
      # NotFound; KCR_FORBIDDEN makes every read Forbidden.
      clusterrole)
        [ -z "${KCR_FORBIDDEN:-}" ] || { echo 'Error from server (Forbidden): clusterroles.rbac.authorization.k8s.io is forbidden' >&2; exit 1; }
        if [ -f "$KDEPLOYS/clusterrole-$name.yaml" ]; then cat "$KDEPLOYS/clusterrole-$name.yaml"; exit 0; fi
        echo "Error from server (NotFound): clusterroles.rbac.authorization.k8s.io \"$name\" not found" >&2; exit 1 ;;
      # The ClusterQueue names of <ns>'s LocalQueues, one per line, from
      # $KDEPLOYS/lq-<ns>; the cohort of ClusterQueue <name> from
      # $KDEPLOYS/cq-<name>. KLQ_FORBIDDEN makes every LocalQueue read Forbidden.
      localqueues.kueue.x-k8s.io)
        [ -z "${KLQ_FORBIDDEN:-}" ] || { echo 'Error from server (Forbidden): localqueues.kueue.x-k8s.io is forbidden' >&2; exit 1; }
        [ ! -f "$KDEPLOYS/lq-$ns" ] || cat "$KDEPLOYS/lq-$ns" ;;
      clusterqueues.kueue.x-k8s.io)
        [ ! -f "$KDEPLOYS/cq-$name" ] || cat "$KDEPLOYS/cq-$name" ;;
      # The parent of Cohort <name>, from $KDEPLOYS/cohort-<name>; NotFound
      # without that file.
      cohorts.kueue.x-k8s.io)
        if [ -f "$KDEPLOYS/cohort-$name" ]; then cat "$KDEPLOYS/cohort-$name"; else
          echo "Error from server (NotFound): cohorts.kueue.x-k8s.io \"$name\" not found" >&2; exit 1; fi ;;
      # One live ClusterRoleBinding: listed in KCRB_LIVE, else NotFound.
      clusterrolebinding)
        case " ${KCRB_LIVE:-} " in
          *" $name "*) echo "clusterrolebinding.rbac.authorization.k8s.io/$name" ;;
          *) echo "Error from server (NotFound): clusterrolebindings.rbac.authorization.k8s.io \"$name\" not found" >&2; exit 1 ;;
        esac ;;
      # The live ClusterRoleBindings: $KDEPLOYS/crbs.json, else none.
      clusterrolebindings)
        if [ -f "$KDEPLOYS/crbs.json" ]; then cat "$KDEPLOYS/crbs.json"; else echo '{"items":[]}'; fi ;;
      *) : ;;
    esac ;;
  *) : ;;
esac
exit 0
STUBEOF
chmod +x "$STUB/kubectl"

DEPLOYS="$WORK/deploys"; mkdir -p "$DEPLOYS"

# deploy_json <name> <serviceaccount|-> <manager image|-> [<other container name>]
# One Deployment object. `-` omits the field, so an absent ServiceAccount and an
# absent manager container are both expressible.
deploy_json() {
    local name="$1" sa="$2" image="$3" cname="${4:-manager}"
    jq -n --arg name "$name" --arg sa "$sa" --arg image "$image" --arg cname "$cname" '
        {metadata: {name: $name, labels: {"app.kubernetes.io/name": "scaling-manager"}},
         spec: {template: {spec: (
            (if $sa == "-" then {} else {serviceAccountName: $sa} end)
            + {containers: [{name: $cname, image: (if $image == "-" then "busybox" else $image end)}]}
         )}}}'
}
# put_ns <namespace> <deployment json>... -- the namespace fixture.
put_ns() {
    local ns="$1"; shift
    printf '%s\n' "$@" | jq -s '{apiVersion: "v1", kind: "List", items: .}' > "$DEPLOYS/$ns.json"
}

IMG="ghcr.io/ev-shindin/llm-scaling-manager:main"
put_ns tenant-a "$(deploy_json scaling-manager-controller-manager scaling-manager-controller-manager "$IMG")"
# The attack: the label, the real image, the namespace default account.
put_ns tenant-b "$(deploy_json impostor default "$IMG")"
# The controller account, but someone else's image.
put_ns tenant-c "$(deploy_json lookalike scaling-manager-controller-manager "quay.io/someone/anything:v1")"
# The controller account and image, but no `manager` container.
put_ns tenant-d "$(deploy_json sidecar scaling-manager-controller-manager "$IMG" app)"
# A genuine controller and an impostor side by side, impostor first: the old
# code read `.items[0]` and would have bound the impostor account.
put_ns tenant-e "$(deploy_json aaa-impostor tenant-admin "$IMG")" \
                "$(deploy_json scaling-manager-controller-manager scaling-manager-controller-manager "$IMG")"
# A pinned digest and a registry port must still be recognised as the repo.
put_ns tenant-f "$(deploy_json scaling-manager-controller-manager scaling-manager-controller-manager "ghcr.io/ev-shindin/llm-scaling-manager:v1.2@sha256:abc")"
# A self-managed controller (the base manifest: --watch-namespace from
# LLMSM_WATCH_NAMESPACE, which is $(POD_NAMESPACE)) with a sidecar listed FIRST.
# Read from containers[0], it has no flag, looks cluster-scoped, and is never
# granted the Namespace read it needs to find scaling-manager-policy.
put_ns tenant-g "$(jq -n --arg img "$IMG" '
    {metadata: {name: "scaling-manager-controller-manager", labels: {"app.kubernetes.io/name": "scaling-manager"}},
     spec: {selector: {matchLabels: {"control-plane": "controller-manager"}},
            template: {spec: {serviceAccountName: "scaling-manager-controller-manager", containers: [
              {name: "kube-rbac-proxy", image: "quay.io/brancz/kube-rbac-proxy:v0.18.0", args: ["--secure-listen-address=0.0.0.0:8443"]},
              {name: "manager", image: $img,
               args: ["--leader-elect=true", "--watch-namespace=$(LLMSM_WATCH_NAMESPACE)"],
               env: [{name: "POD_NAMESPACE", valueFrom: {fieldRef: {fieldPath: "metadata.namespace"}}},
                     {name: "LLMSM_WATCH_NAMESPACE", value: "$(POD_NAMESPACE)"}]}]}}}}')"

# run_enable [VAR=value ...] -- enable_physical_limiter in a subshell, the stub
# first on PATH. Output in $WORK/out, grants in $GRANTS, status in $RC.
run_enable() {
    GRANTS="$WORK/grants"; : > "$GRANTS"
    CALLS="$WORK/calls"; : > "$CALLS"
    (
        PATH="$STUB:$PATH"; export PATH
        KGRANTS="$GRANTS" KCALLS="$CALLS" KDEPLOYS="$DEPLOYS"
        export KGRANTS KCALLS KDEPLOYS
        KDEPLOY_NS_LIST="tenant-a tenant-b tenant-c tenant-d tenant-e tenant-f tenant-g"
        export KDEPLOY_NS_LIST
        KDEPLOY_NS_LIST="$(printf '%s\n' $KDEPLOY_NS_LIST)"
        for kv in "$@"; do export "${kv?}"; done
        "${PL_FN:-enable_physical_limiter}"
    ) >"$WORK/out" 2>&1
    RC=$?
}
granted_to() { grep -q "^GRANT ClusterRoleBinding scaling-manager-node-reader-rolebinding-[^ ]* $1\$" "$GRANTS"; }
any_grant_in() { grep -q "^GRANT [A-Za-z]* [^ ]* $1/" "$GRANTS"; }

# ---------------------------------------------------------------------------
case_begin
run_enable
if [ "$RC" -ne 0 ]; then
    fail "enable_physical_limiter failed on a cluster where everything answers: $(tail -15 "$WORK/out")"
elif ! granted_to tenant-a/scaling-manager-controller-manager; then
    fail "the genuine controller in tenant-a was not granted node read: $(cat "$GRANTS")"
else
    ok "a genuine controller is granted node read"
fi

case_begin
if any_grant_in tenant-b; then
    fail "a labelled Deployment running as tenant-b/default was GRANTED -- a tenant-chosen account now reads every node: $(grep tenant-b "$GRANTS")"
elif ! grep -q 'SKIPPED tenant-b/impostor' "$WORK/out"; then
    fail "the impostor in tenant-b was not granted but not named either; an admin cannot tell it was skipped: $(tail -15 "$WORK/out")"
else
    ok "a labelled impostor with another ServiceAccount is skipped, by name, and granted nothing"
fi

case_begin
if any_grant_in tenant-c; then
    fail "a Deployment running a foreign image was granted: $(grep tenant-c "$GRANTS")"
elif ! grep -q 'SKIPPED tenant-c/lookalike' "$WORK/out"; then
    fail "the foreign-image Deployment was not named as skipped: $(tail -15 "$WORK/out")"
else
    ok "the controller account on a foreign image is skipped"
fi

case_begin
if any_grant_in tenant-d; then
    fail "a Deployment with no manager container was granted: $(grep tenant-d "$GRANTS")"
elif ! grep -q 'SKIPPED tenant-d/sidecar' "$WORK/out"; then
    fail "the Deployment with no manager container was not named as skipped: $(tail -15 "$WORK/out")"
else
    ok "a Deployment with no manager container is skipped"
fi

case_begin
if grep -q 'tenant-e/tenant-admin' "$GRANTS"; then
    fail "the impostor listed first in tenant-e had its account granted: $(grep tenant-e "$GRANTS")"
elif ! granted_to tenant-e/scaling-manager-controller-manager; then
    fail "the genuine controller beside an impostor was not granted: $(cat "$GRANTS")"
else
    ok "beside an impostor, only the genuine controller is granted"
fi

case_begin
if ! granted_to tenant-f/scaling-manager-controller-manager; then
    fail "a controller image pinned by tag and digest was not recognised: $(tail -15 "$WORK/out")"
else
    ok "an image pinned by tag and digest is recognised by its repository"
fi

case_begin
listed_at="$(grep -n 'Controllers to be granted' "$WORK/out" | head -1 | cut -d: -f1)"
first_grant="$(grep -n 'CALL\[apply' "$CALLS" | head -1 | cut -d: -f1)"
if [ -z "$listed_at" ]; then
    fail "the command did not print the controllers it is about to grant: $(tail -15 "$WORK/out")"
elif ! sed -n "${listed_at},\$p" "$WORK/out" | grep -q 'tenant-a/scaling-manager-controller-manager as ServiceAccount scaling-manager-controller-manager'; then
    fail "the printed list does not name the genuine controller: $(tail -15 "$WORK/out")"
elif sed -n "${listed_at},/Publishing/p" "$WORK/out" | grep -q 'impostor as'; then
    fail "the printed list names the impostor as one to be granted: $(tail -15 "$WORK/out")"
elif [ -z "$first_grant" ]; then
    fail "no apply reached the stub at all; this case can no longer tell before from after"
else
    ok "the final list is printed, and holds only verified controllers"
fi

# Honest about what verification is: signals any tenant can satisfy.
case_begin
if sed -n "${listed_at:-1},/Publishing/p" "$WORK/out" | grep -q 'signals, not proof'; then
    ok "the list says verification is signals, not proof"
else
    fail "the list of controllers to be granted presents verification as proof: $(tail -15 "$WORK/out")"
fi

# The banner names who is bound and who is NOT. "In force for every scaling manager on this
# cluster" was printed with three labelled Deployments in the skipped list.
case_begin
banner="$(sed -n '/limiter is now in force/,$p' "$WORK/out")"
if grep -q 'every scaling manager' "$WORK/out"; then
    fail "the banner still claims the limiter is in force for every scaling manager: $(grep 'every scaling manager' "$WORK/out")"
elif ! grep -q 'in force for .*tenant-a/scaling-manager-controller-manager' <<<"$banner"; then
    fail "the banner does not name the controllers it bound: $banner"
elif ! grep -q 'NOT bound' <<<"$banner"; then
    fail "the banner does not say which controllers are NOT bound: $banner"
else
    missing=""
    for d in tenant-b/impostor tenant-c/lookalike tenant-d/sidecar; do
        sed -n '/NOT bound/,$p' <<<"$banner" | grep -q "$d" || missing="$missing $d"
    done
    if [ -n "$missing" ]; then
        fail "the NOT-bound list after the banner leaves out:$missing: $banner"
    else
        ok "the banner names the bound controllers, and every skipped one as NOT bound"
    fi
fi

# containers[0] is not the manager. The sidecar-first controller in tenant-g
# manages its own namespace, so it needs the Namespace read to find scaling-manager-policy.
case_begin
if grep -q '^GRANT ClusterRoleBinding scaling-manager-policy-namespace-reader-rolebinding-[^ ]* tenant-g/scaling-manager-controller-manager$' "$GRANTS"; then
    ok "the manager container, not containers[0], decides the managed namespace"
else
    fail "the self-managed controller behind a sidecar was not granted the Namespace read (read as cluster-scoped from containers[0]?): $(grep tenant-g "$GRANTS")"
fi

# ---------------------------------------------------------------------------
# LLMSM_LIMITER_TARGETS is an allowlist of namespaces, not of Deployments: naming
# the impostor namespace does not make its Deployment genuine.
case_begin
run_enable LLMSM_LIMITER_TARGETS=tenant-b
if [ "$RC" -ne 0 ]; then
    fail "enable with LLMSM_LIMITER_TARGETS=tenant-b failed: $(tail -15 "$WORK/out")"
elif any_grant_in tenant-b; then
    fail "naming tenant-b in LLMSM_LIMITER_TARGETS granted its impostor: $(cat "$GRANTS")"
elif any_grant_in tenant-a; then
    fail "LLMSM_LIMITER_TARGETS=tenant-b still granted tenant-a; it is no longer an allowlist: $(cat "$GRANTS")"
else
    ok "LLMSM_LIMITER_TARGETS does not turn an impostor into a controller, and limits the grant to the named namespaces"
fi

# A named namespace with no controller is SKIPPED: nothing granted, nothing
# written there, and it is listed as NOT bound, with the reason.
case_begin
run_enable LLMSM_LIMITER_TARGETS=tenant-new
if [ "$RC" -ne 0 ]; then
    fail "enable with a target holding no controller failed: $(tail -15 "$WORK/out")"
elif any_grant_in tenant-new; then
    fail "a named namespace with no controller was granted: $(grep tenant-new "$GRANTS")"
elif grep -q 'CALL\[\(patch\|apply\|create\) .*-n tenant-new' "$CALLS"; then
    fail "a policy was written into a named namespace with no controller: $(grep tenant-new "$CALLS")"
elif ! sed -n '/NOT bound/,$p' "$WORK/out" | grep -q 'tenant-new: no controller found'; then
    fail "the namespace with no controller is not listed as NOT bound, 'no controller found': $(tail -15 "$WORK/out")"
else
    ok "a named namespace with no controller is skipped, written nothing, and listed as NOT bound"
fi

# A namespace the admin NAMED relaxes the image check -- a controller built
# from a mirror or a custom IMG -- but nothing else (tenant-b above: still not).
case_begin
run_enable LLMSM_LIMITER_TARGETS=tenant-c
if [ "$RC" -ne 0 ]; then
    fail "enable with LLMSM_LIMITER_TARGETS=tenant-c failed: $(tail -15 "$WORK/out")"
elif ! granted_to tenant-c/scaling-manager-controller-manager; then
    fail "a controller on a foreign image in a namespace the admin NAMED was skipped; a mirror-built controller can then never be granted: $(tail -15 "$WORK/out")"
else
    ok "a named namespace relaxes the image check, and only that"
fi

# A mirror is accepted when named, and the account can be renamed.
case_begin
run_enable LLMSM_CONTROLLER_IMAGE_REPOS=quay.io/someone/anything
if ! granted_to tenant-c/scaling-manager-controller-manager; then
    fail "LLMSM_CONTROLLER_IMAGE_REPOS did not admit the mirror it names: $(tail -15 "$WORK/out")"
elif any_grant_in tenant-a; then
    fail "LLMSM_CONTROLLER_IMAGE_REPOS did not REPLACE the default repository list: $(cat "$GRANTS")"
else
    ok "LLMSM_CONTROLLER_IMAGE_REPOS names the repositories a controller may run from"
fi

# ---------------------------------------------------------------------------
# A namespace that cannot be read is a refusal, not a skip: the command cannot
# know what it is leaving ungranted.
case_begin
run_enable KFAIL_NS=tenant-c
if [ "$RC" -eq 0 ]; then
    fail "enable exited 0 having failed to read a controller namespace: $(tail -15 "$WORK/out")"
elif grep -q 'in force' "$WORK/out"; then
    fail "enable announced the limiter after failing to read a controller namespace: $(tail -15 "$WORK/out")"
elif [ -s "$GRANTS" ]; then
    fail "enable granted before refusing: $(cat "$GRANTS")"
else
    ok "an unreadable controller namespace stops enable before it grants anything"
fi

# pl_image_repo, directly.
case_begin
for pair in \
    "ghcr.io/ev-shindin/llm-scaling-manager:main=ghcr.io/ev-shindin/llm-scaling-manager" \
    "ghcr.io/ev-shindin/llm-scaling-manager@sha256:abc=ghcr.io/ev-shindin/llm-scaling-manager" \
    "registry.local:5000/scaling-manager=registry.local:5000/scaling-manager" \
    "registry.local:5000/scaling-manager:dev=registry.local:5000/scaling-manager"; do
    in="${pair%=*}"; want="${pair##*=}"
    got="$(pl_image_repo "$in")"
    [ "$got" = "$want" ] || fail "pl_image_repo $in gave $got, want $want"
done
ok "pl_image_repo strips tags and digests, and keeps a registry port"

# The installer image (a repo or a full reference) joins the published repo when
# no explicit list is given, so a controller built from a custom IMG is known.
case_begin
got="$(LLMSM_IMAGE_REPO=registry.local:5000/me/scaling-manager:dev pl_image_repos)"
if [ "$got" = "ghcr.io/ev-shindin/llm-scaling-manager registry.local:5000/me/scaling-manager" ]; then
    ok "pl_image_repos adds the installer image repository to the published one"
else
    fail "pl_image_repos with LLMSM_IMAGE_REPO=registry.local:5000/me/scaling-manager:dev gave '$got'"
fi

# disable: a skipped controller whose own policy still declares a limiter is
# reported, and not written to.
case_begin
printf 'limiters:\n- type: gpu-inventory\n  name: physical\n' > "$DEPLOYS/tenant-c.policy"
PL_FN=disable_physical_limiter run_enable
rm -f "$DEPLOYS/tenant-c.policy"
if [ "$RC" -ne 0 ]; then
    fail "disable failed on a cluster where everything answers: $(tail -15 "$WORK/out")"
elif grep -q 'CALL\[patch configmap scaling-manager-scaling-policy-config -n tenant-c' "$CALLS"; then
    fail "disable wrote the policy of a Deployment that failed verification"
elif ! grep -q 'tenant-c/lookalike.*STILL declares 1 limiter' "$WORK/out"; then
    fail "disable left a limiter in a skipped controller's policy without saying so: $(tail -15 "$WORK/out")"
else
    ok "disable names the limiter it left in a skipped controller's policy, and does not write it"
fi

# ---------------------------------------------------------------------------
# The Kueue reader: enable writes the Namespace rule the installer writes for
# the same controller (llmsm_kueue_reader_names, shared), or the two undo each
# other on every run.
#
# A namespace-scoped controller gets its MANAGED namespace -- what it lists
# LocalQueues in -- plus every name the live role already carries. A
# cluster-scoped-looking one gets every Namespace only when the admin NAMED its
# namespace, or its account already reads every Namespace (the manager
# ClusterRole of a cluster-scoped install); a tenant that drops --watch-namespace
# is never handed that read.
kueue_rule() { grep "^NSRULE scaling-manager-kueue-reader-role-$(llmsm_ns_suffix "$1") " "$GRANTS" | tail -1 | awk '{print $NF}'; }
kueue_warning() { grep "$1/scaling-manager-controller-manager looks cluster-scoped" "$WORK/out"; }
case_begin
run_enable LLMSM_LIMITER_TYPE=quota LLMSM_QUOTA_KUEUE=true LLMSM_QUOTAS=H200=8
if [ "$RC" -ne 0 ]; then
    fail "enable with a Kueue quota failed: $(tail -15 "$WORK/out")"
elif [ "$(kueue_rule tenant-g)" != "tenant-g" ]; then
    fail "the namespace-scoped controller in tenant-g got Kueue get namespaces on [$(kueue_rule tenant-g)], want only tenant-g"
elif [ "$(kueue_rule tenant-a)" != "tenant-a" ]; then
    fail "the cluster-scoped-LOOKING controller in tenant-a, not named and not reading Namespaces cluster-wide, got Kueue get namespaces on [$(kueue_rule tenant-a)], want only tenant-a: a tenant drops --watch-namespace and reads every Namespace"
elif ! kueue_warning tenant-a | grep -q 'cannot read Namespaces cluster-wide'; then
    fail "the narrowed cluster-scoped grant was not explained: $(grep -i kueue "$WORK/out" | head -3)"
elif ! kueue_warning tenant-a | grep -q 'make setup-prereqs SCOPE=cluster LLMSM_NS=tenant-a'; then
    fail "the narrowed cluster-scoped grant does not point at that install's prerequisites: $(kueue_warning tenant-a)"
elif kueue_warning tenant-a | grep -q 'LLMSM_LIMITER_TARGETS'; then
    fail "the narrowed cluster-scoped grant still advises LLMSM_LIMITER_TARGETS, which limits enable to the named namespaces: $(kueue_warning tenant-a)"
else
    ok "Kueue get namespaces, nothing named: each controller gets what it manages, and a narrowed cluster-scoped one is told to re-run its prerequisites"
fi
# A cluster-scoped install: its manager ClusterRole already reads every
# Namespace, so the unrestricted rule widens nothing -- and there is nothing to
# warn about.
case_begin
run_enable LLMSM_LIMITER_TYPE=quota LLMSM_QUOTA_KUEUE=true LLMSM_QUOTAS=H200=8 KCANI_WATCH_NS=tenant-a
if [ "$RC" -ne 0 ]; then
    fail "enable for a cluster-scoped install failed: $(tail -15 "$WORK/out")"
elif [ "$(kueue_rule tenant-a)" != "*" ]; then
    fail "the cluster-scoped install in tenant-a, whose account reads every Namespace already, got [$(kueue_rule tenant-a)]"
elif kueue_warning tenant-a >/dev/null; then
    fail "a cluster-scoped install that already reads every Namespace was warned about: $(kueue_warning tenant-a)"
elif [ "$(kueue_rule tenant-g)" != "tenant-g" ]; then
    fail "another account reading Namespaces widened tenant-g to [$(kueue_rule tenant-g)]"
else
    ok "Kueue get namespaces: a cluster-scoped install that reads every Namespace is granted it, quietly"
fi
case_begin
run_enable LLMSM_LIMITER_TYPE=quota LLMSM_QUOTA_KUEUE=true LLMSM_QUOTAS=H200=8 'LLMSM_LIMITER_TARGETS=tenant-a tenant-g'
if [ "$RC" -ne 0 ]; then
    fail "enable with a Kueue quota and named targets failed: $(tail -15 "$WORK/out")"
elif [ "$(kueue_rule tenant-a)" != "*" ]; then
    fail "the cluster-scoped controller in NAMED tenant-a got [$(kueue_rule tenant-a)]; it reads LocalQueues everywhere"
elif [ "$(kueue_rule tenant-g)" != "tenant-g" ]; then
    fail "naming tenant-g widened its namespace-scoped grant to [$(kueue_rule tenant-g)]"
else
    ok "Kueue get namespaces: the unrestricted read only for a named cluster-scoped controller"
fi
# The out-of-reach shape: a controller in scaling-manager-foo managing foo. The installer
# grants it `get namespaces/foo`; enable granted scaling-manager-foo and took foo away.
put_ns scaling-manager-foo "$(jq -n --arg img "$IMG" '
    {metadata: {name: "scaling-manager-controller-manager", labels: {"app.kubernetes.io/name": "scaling-manager"}},
     spec: {template: {spec: {serviceAccountName: "scaling-manager-controller-manager", containers: [
       {name: "manager", image: $img, args: ["--watch-namespace=foo"]}]}}}}')"
case_begin
run_enable LLMSM_LIMITER_TYPE=quota LLMSM_QUOTA_KUEUE=true LLMSM_QUOTAS=H200=8 KDEPLOY_NS_LIST=scaling-manager-foo
foo_unnamed="$(kueue_rule scaling-manager-foo)"
run_enable LLMSM_LIMITER_TYPE=quota LLMSM_QUOTA_KUEUE=true LLMSM_QUOTAS=H200=8 LLMSM_LIMITER_TARGETS=scaling-manager-foo
foo_named="$(kueue_rule scaling-manager-foo)"
if [ "$foo_unnamed" != "foo" ]; then
    fail "the out-of-reach controller in scaling-manager-foo managing foo got Kueue get namespaces on [$foo_unnamed], want foo: its Kueue source bounds foo at nothing"
elif [ "$foo_named" != "foo" ]; then
    fail "named in LLMSM_LIMITER_TARGETS, the out-of-reach controller in scaling-manager-foo got [$foo_named], want foo"
else
    ok "Kueue get namespaces: an out-of-reach controller gets the namespace it manages, named or not"
fi
# Never fewer names than the live role holds: one an admin added survives.
cat > "$DEPLOYS/clusterrole-scaling-manager-kueue-reader-role-$(llmsm_ns_suffix scaling-manager-foo).yaml" <<'CREOF'
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: placeholder
rules:
- apiGroups: ["kueue.x-k8s.io"]
  resources: ["clusterqueues", "resourceflavors"]
  verbs: ["get", "list"]
- apiGroups: [""]
  resources: ["namespaces"]
  resourceNames: ["foo", "admin-added"]
  verbs: ["get"]
CREOF
case_begin
run_enable LLMSM_LIMITER_TYPE=quota LLMSM_QUOTA_KUEUE=true LLMSM_QUOTAS=H200=8 KDEPLOY_NS_LIST=scaling-manager-foo
if [ "$(kueue_rule scaling-manager-foo)" != "foo,admin-added" ]; then
    fail "enable rewrote a role naming [foo, admin-added] to [$(kueue_rule scaling-manager-foo)]: a name the admin set was removed"
else
    ok "Kueue get namespaces: enable keeps every name the live role carries"
fi
# A role that cannot be read is not an absent one: the rewrite could remove a
# name, so enable stops instead.
case_begin
run_enable LLMSM_LIMITER_TYPE=quota LLMSM_QUOTA_KUEUE=true LLMSM_QUOTAS=H200=8 KDEPLOY_NS_LIST=scaling-manager-foo KCR_FORBIDDEN=1
if [ "$RC" -eq 0 ]; then
    fail "enable rewrote a kueue-reader role it could not read: $(tail -4 "$WORK/out")"
elif [ -n "$(kueue_rule scaling-manager-foo)" ]; then
    fail "enable applied a kueue-reader role it could not read: [$(kueue_rule scaling-manager-foo)]"
elif ! grep -q 'Not rewriting it' "$WORK/out"; then
    fail "the unreadable kueue-reader role was not named as the reason: $(tail -4 "$WORK/out")"
else
    ok "Kueue get namespaces: an unreadable live role stops enable before it is rewritten"
fi
# An empty resourceNames list is no restriction at all: a managed value that is
# not a Namespace name must fall back to the controller's own namespace.
rm -f "$DEPLOYS"/clusterrole-*.yaml
put_ns scaling-manager-foo "$(jq -n --arg img "$IMG" '
    {metadata: {name: "scaling-manager-controller-manager", labels: {"app.kubernetes.io/name": "scaling-manager"}},
     spec: {template: {spec: {serviceAccountName: "scaling-manager-controller-manager", containers: [
       {name: "manager", image: $img, args: ["--watch-namespace=Not_A_Namespace"]}]}}}}')"
case_begin
run_enable LLMSM_LIMITER_TYPE=quota LLMSM_QUOTA_KUEUE=true LLMSM_QUOTAS=H200=8 KDEPLOY_NS_LIST=scaling-manager-foo
if [ "$(kueue_rule scaling-manager-foo)" != "scaling-manager-foo" ]; then
    fail "a controller managing a non-Namespace name got Kueue get namespaces on [$(kueue_rule scaling-manager-foo)], want scaling-manager-foo (an empty list is every Namespace)"
else
    ok "Kueue get namespaces: a managed value that is not a Namespace name falls back to the own namespace"
fi
rm -f "$DEPLOYS/scaling-manager-foo.json" "$DEPLOYS"/clusterrole-*.yaml

# LLMSM_QUOTA_KUEUE_BORROW=false reaches the published policy as
# countBorrowable: false; unset, the field is not written. The patch payload is
# pretty-printed JSON over several lines, so the call is read up to the next one.
llmsm_policy_patch() { sed -n '/CALL.patch configmap scaling-manager-scaling-policy-config -n scaling-manager-policy/,/^CALL.auth/p' "$CALLS"; }
case_begin
run_enable LLMSM_LIMITER_TYPE=quota LLMSM_QUOTA_KUEUE=true LLMSM_QUOTA_KUEUE_BORROW=false LLMSM_QUOTAS=H200=-1
if [ "$RC" -ne 0 ]; then
    fail "enable with LLMSM_QUOTA_KUEUE_BORROW=false failed: $(tail -15 "$WORK/out")"
elif ! llmsm_policy_patch | grep -q 'countBorrowable: false'; then
    fail "LLMSM_QUOTA_KUEUE_BORROW=false did not reach the cluster policy: $(llmsm_policy_patch | head -12)"
else
    ok "LLMSM_QUOTA_KUEUE_BORROW=false writes countBorrowable: false into the cluster policy"
fi
case_begin
run_enable LLMSM_LIMITER_TYPE=quota LLMSM_QUOTA_KUEUE=true LLMSM_QUOTAS=H200=-1
if [ "$RC" -ne 0 ]; then
    fail "enable with a Kueue quota failed: $(tail -15 "$WORK/out")"
elif ! grep -q 'CALL\[patch configmap scaling-manager-scaling-policy-config -n scaling-manager-policy' "$CALLS"; then
    fail "enable wrote no cluster policy: $(tail -15 "$WORK/out")"
elif ! llmsm_policy_patch | grep -q 'kueue'; then
    fail "the cluster policy carries no kueue block, so the absence below proves nothing: $(llmsm_policy_patch | head -12)"
elif llmsm_policy_patch | grep -q 'countBorrowable'; then
    fail "an unset LLMSM_QUOTA_KUEUE_BORROW wrote countBorrowable: $(llmsm_policy_patch | head -12)"
else
    ok "an unset LLMSM_QUOTA_KUEUE_BORROW leaves countBorrowable out of the cluster policy"
fi
# S3: the Role and RoleBinding in the Kueue namespace carry the install suffix
# as a label, so the uninstall finds them wherever KUEUE_NAMESPACE put them.
case_begin
want_label="$(llmsm_ns_suffix tenant-a)"
if ! grep -q "^RBLABEL Role scaling-manager-kueue-controller-reader-$want_label kueue-system $want_label\$" "$GRANTS"; then
    fail "the Kueue-namespace Role of tenant-a carries no install-suffix label: $(grep RBLABEL "$GRANTS" | head -6)"
elif ! grep -q "^RBLABEL RoleBinding scaling-manager-kueue-controller-reader-$want_label kueue-system $want_label\$" "$GRANTS"; then
    fail "the Kueue-namespace RoleBinding of tenant-a carries no install-suffix label: $(grep RBLABEL "$GRANTS" | head -6)"
else
    ok "the Kueue controller read is labelled with the install suffix"
fi

# R2-1: the cluster-wide Workload and LocalQueue read reaches every tenant, so
# enable grants it only to a controller whose published policy counts cohort
# borrowing, and revokes it from one whose policy no longer does.
borrow_granted() { grep -q "^GRANT ClusterRoleBinding scaling-manager-kueue-borrow-reader-rolebinding-$(llmsm_ns_suffix "${1%%/*}") $1\$" "$GRANTS"; }
borrow_revoked() { grep -qF "CALL[delete clusterrolebinding scaling-manager-kueue-borrow-reader-rolebinding-$(llmsm_ns_suffix "$1") --ignore-not-found]" "$CALLS"; }
case_begin
run_enable LLMSM_LIMITER_TYPE=quota LLMSM_QUOTA_KUEUE=true LLMSM_QUOTAS=H200=-1
if [ "$RC" -ne 0 ]; then
    fail "enable with a Kueue quota failed: $(tail -15 "$WORK/out")"
elif ! borrow_granted tenant-g/scaling-manager-controller-manager || ! borrow_granted tenant-a/scaling-manager-controller-manager; then
    fail "a policy counting borrowing did not get its controllers the borrowing read: $(grep 'borrow' "$GRANTS" | head -4)"
elif borrow_revoked tenant-g; then
    fail "enable revoked the borrowing read of a controller whose policy counts borrowing"
else
    ok "a quota counting borrowing grants its controllers the cluster-wide Workload read"
fi
case_begin
run_enable LLMSM_LIMITER_TYPE=quota LLMSM_QUOTA_KUEUE=true LLMSM_QUOTA_KUEUE_BORROW=false LLMSM_QUOTAS=H200=-1
if [ "$RC" -ne 0 ]; then
    fail "enable with LLMSM_QUOTA_KUEUE_BORROW=false failed: $(tail -15 "$WORK/out")"
elif grep -q 'scaling-manager-kueue-borrow-reader' "$GRANTS"; then
    fail "LLMSM_QUOTA_KUEUE_BORROW=false still granted the borrowing read: $(grep 'borrow' "$GRANTS" | head -4)"
elif ! borrow_revoked tenant-g || ! borrow_revoked tenant-a; then
    fail "LLMSM_QUOTA_KUEUE_BORROW=false did not revoke the borrowing read: $(grep 'CALL\[delete' "$CALLS" | head -4)"
else
    ok "LLMSM_QUOTA_KUEUE_BORROW=false revokes the borrowing read and grants none"
fi
case_begin
run_enable LLMSM_LIMITER_TYPE=quota LLMSM_QUOTAS=H200=8
if [ "$RC" -ne 0 ]; then
    fail "enable with a quota without Kueue failed: $(tail -15 "$WORK/out")"
elif grep -q 'scaling-manager-kueue-borrow-reader' "$GRANTS" || ! borrow_revoked tenant-g; then
    fail "a quota without Kueue left or granted the borrowing read: $(grep 'borrow' "$GRANTS" "$CALLS" | head -4)"
else
    ok "a quota without Kueue revokes the borrowing read"
fi
# A kept countBorrowable: false (LLMSM_QUOTA_KUEUE_BORROW not given) counts as
# false, and a kept controllerNamespace places the Kueue controller read
# (R2-6): tenant-g reads scaling-manager-policy, tenant-a its own plain policy.
case_begin
printf 'limiters:\n- name: kq\n  type: quota\n  scope: cluster\n  quotas:\n    H200: -1\n  kueue:\n    enabled: true\n    countBorrowable: false\n    controllerNamespace: kueue-ops\n' > "$DEPLOYS/scaling-manager-policy.policy"
run_enable LLMSM_LIMITER_TYPE=quota LLMSM_QUOTA_KUEUE=true LLMSM_QUOTAS=H200=-1
rm -f "$DEPLOYS/scaling-manager-policy.policy"
want_g="$(llmsm_ns_suffix tenant-g)"
if [ "$RC" -ne 0 ]; then
    fail "enable over a kept countBorrowable: false failed: $(tail -15 "$WORK/out")"
elif borrow_granted tenant-g/scaling-manager-controller-manager || ! borrow_revoked tenant-g; then
    fail "a kept countBorrowable: false still left tenant-g the borrowing read: $(grep 'borrow' "$GRANTS" | head -4)"
elif ! borrow_granted tenant-a/scaling-manager-controller-manager; then
    fail "tenant-a, whose own policy counts borrowing, lost the borrowing read: $(grep 'borrow' "$GRANTS" | head -4)"
elif ! grep -q "^RBLABEL Role scaling-manager-kueue-controller-reader-$want_g kueue-ops $want_g\$" "$GRANTS"; then
    fail "the Kueue controller read of tenant-g is not in the kept controllerNamespace kueue-ops: $(grep RBLABEL "$GRANTS" | head -4)"
else
    ok "a kept countBorrowable: false revokes the borrowing read; a kept controllerNamespace places the controller read"
fi
case_begin
run_enable
if grep -q 'scaling-manager-kueue-borrow-reader' "$GRANTS" "$CALLS"; then
    fail "enabling the physical limiter touched the borrowing read: $(grep 'borrow' "$GRANTS" "$CALLS" | head -4)"
else
    ok "enabling the physical limiter leaves the borrowing read alone"
fi
# LLMSM_QUOTA_KUEUE_BORROW without LLMSM_QUOTA_KUEUE: there is no Kueue block to put
# it on, so the published policy carries neither, and the command says so.
case_begin
run_enable LLMSM_LIMITER_TYPE=quota LLMSM_QUOTA_KUEUE_BORROW=false LLMSM_QUOTAS=H200=8
if [ "$RC" -ne 0 ]; then
    fail "enable with LLMSM_QUOTA_KUEUE_BORROW but no LLMSM_QUOTA_KUEUE failed: $(tail -15 "$WORK/out")"
elif ! grep -q 'CALL\[patch configmap scaling-manager-scaling-policy-config -n scaling-manager-policy' "$CALLS"; then
    fail "enable wrote no cluster policy, so the absence below proves nothing: $(tail -15 "$WORK/out")"
elif llmsm_policy_patch | grep -q 'kueue\|countBorrowable'; then
    fail "LLMSM_QUOTA_KUEUE_BORROW without LLMSM_QUOTA_KUEUE wrote a Kueue field: $(llmsm_policy_patch | head -12)"
elif ! grep -q 'LLMSM_QUOTA_KUEUE_BORROW=false is set, but LLMSM_QUOTA_KUEUE is not true.*IGNORED' "$WORK/out"; then
    fail "LLMSM_QUOTA_KUEUE_BORROW without LLMSM_QUOTA_KUEUE was ignored silently: $(tail -6 "$WORK/out")"
else
    ok "LLMSM_QUOTA_KUEUE_BORROW without LLMSM_QUOTA_KUEUE is ignored by enable, with a warning"
fi

# A namespace-scoped controller does not watch a policy namespace outside the
# one it manages, so it sees the new policy only after a restart -- which
# enable must do, after the write. A cluster-scoped one reloads live.
case_begin
run_enable
restart_g="$(grep -n 'CALL\[rollout restart deploy scaling-manager-controller-manager -n tenant-g\]' "$CALLS" | head -1 | cut -d: -f1)"
write_wp="$(grep -n 'CALL\[patch configmap scaling-manager-scaling-policy-config -n scaling-manager-policy' "$CALLS" | head -1 | cut -d: -f1)"
if [ -z "$restart_g" ]; then
    fail "the self-managed controller in tenant-g, which reads scaling-manager-policy only at startup, was not restarted: $(grep -i restart "$WORK/out")"
elif [ -z "$write_wp" ] || [ "$restart_g" -lt "$write_wp" ]; then
    fail "tenant-g was restarted before the policy it must read was written (restart at $restart_g, write at ${write_wp:-never})"
elif grep -q 'CALL\[rollout restart deploy scaling-manager-controller-manager -n tenant-a\]' "$CALLS"; then
    fail "the cluster-scoped controller in tenant-a was restarted; it reloads policy live"
elif grep -q 'applies live' "$WORK/out"; then
    fail "enable still says the policy applies live to every controller: $(grep 'applies live' "$WORK/out")"
else
    ok "namespace-scoped controllers are restarted after the write; a cluster-scoped one is not"
fi

# Enabling the physical limiter COMPOSES with a quota the cluster policy
# already declares. Every declared limiter is built and both bound, so the
# quota must survive the write; the command used to replace the whole list.
case_begin
printf 'limiters:\n- name: tenant-quota\n  type: quota\n  scope: cluster\n  quotas:\n    H200: 8\n' > "$DEPLOYS/scaling-manager-policy.policy"
run_enable
rm -f "$DEPLOYS/scaling-manager-policy.policy"
# The payload spans lines: from the patch call up to the next recorded call.
written="$(awk '/^CALL\[/ { on = 0 } /^CALL\[patch configmap scaling-manager-scaling-policy-config -n scaling-manager-policy/ { on = 1 } on' "$CALLS")"
if [ "$RC" -ne 0 ]; then
    fail "enable failed over a policy that declares a quota: $(tail -15 "$WORK/out")"
elif [ -z "$written" ]; then
    fail "enable did not patch the scaling-manager-policy ConfigMap: $(grep -c CALL "$CALLS") calls"
elif [ "$written" = "${written#*name: tenant-quota}" ]; then
    fail "enabling the physical limiter dropped the quota already in the cluster policy: $written"
elif [ "$written" = "${written#*type: gpu-inventory}" ]; then
    fail "enable did not declare the physical limiter: $written"
else
    ok "enabling the physical limiter keeps the quota the cluster policy already declares"
fi

# And disable is its mirror: it removes the physical entry and keeps the quota.
case_begin
printf 'limiters:\n- type: gpu-inventory\n- name: tenant-quota\n  type: quota\n  scope: cluster\n  quotas:\n    H200: 8\n' > "$DEPLOYS/scaling-manager-policy.policy"
PL_FN=disable_physical_limiter run_enable
rm -f "$DEPLOYS/scaling-manager-policy.policy"
written="$(awk '/^CALL\[/ { on = 0 } /^CALL\[patch configmap scaling-manager-scaling-policy-config -n scaling-manager-policy/ { on = 1 } on' "$CALLS")"
if [ "$RC" -ne 0 ]; then
    fail "disable failed over a policy that declares a quota too: $(tail -15 "$WORK/out")"
elif [ "$written" = "${written#*name: tenant-quota}" ]; then
    fail "disabling the physical limiter dropped the quota in the cluster policy: $written"
elif [ "$written" != "${written#*gpu-inventory}" ]; then
    fail "disable left the physical limiter in the cluster policy: $written"
else
    ok "disabling the physical limiter keeps the quota the cluster policy declares"
fi
case_begin
if ! grep -q 'Removed the gpu-inventory limiter from scaling-manager-policy/scaling-manager-scaling-policy-config; the quota limiter declared there stays' "$WORK/out"; then
    fail "disable did not say the quota stays beside the removed physical limiter: $(grep -i 'removed\|bound' "$WORK/out")"
elif grep -q 'bounded by nothing' "$WORK/out"; then
    fail "disable said scaling is bounded by nothing while the quota stays: $(grep 'bounded by nothing' "$WORK/out")"
else
    ok "disable says the quota still bounds, not that nothing does"
fi
# The quota stays, so the Kueue controller read it may need stays too.
case_begin
if grep -q 'CALL\[delete role,rolebinding\|CALL\[delete clusterrole' "$CALLS"; then
    fail "disabling the physical limiter revoked a Kueue read the quota may still need: $(grep 'CALL\[delete' "$CALLS" | head -2)"
else
    ok "disabling only the physical limiter keeps the Kueue controller read"
fi

# The documented undo of a QUOTA run without LLMSM_LIMITER_TYPE=quota: the policy
# holds only a quota, so there is no physical limiter to remove. It used to
# print "Removed the gpu-inventory limiter" and exit 0, with the quota in force.
case_begin
printf 'limiters:\n- name: tenant-quota\n  type: quota\n  scope: cluster\n  quotas:\n    H200: 8\n' > "$DEPLOYS/scaling-manager-policy.policy"
PL_FN=disable_physical_limiter run_enable
if [ "$RC" -eq 0 ]; then
    fail "disable exited 0 having removed nothing while the quota stays: $(tail -6 "$WORK/out")"
elif grep -q 'Removed the gpu-inventory limiter from scaling-manager-policy' "$WORK/out"; then
    fail "disable announced a removal from a policy holding only a quota: $(grep 'Removed the' "$WORK/out")"
elif grep -q 'CALL\[patch configmap scaling-manager-scaling-policy-config -n scaling-manager-policy' "$CALLS"; then
    fail "disable wrote a policy it had nothing to remove from"
elif ! grep -q 'no gpu-inventory entry in scaling-manager-policy/scaling-manager-scaling-policy-config; nothing removed; the quota limiter stays' "$WORK/out"; then
    fail "disable did not say there was no physical entry and the quota stays: $(tail -6 "$WORK/out")"
elif ! grep -q 'LLMSM_LIMITER_TYPE=quota' "$WORK/out"; then
    fail "disable did not say how to remove the quota: $(tail -4 "$WORK/out")"
else
    ok "disable over a quota-only policy removes nothing, exits non-zero, and names LLMSM_LIMITER_TYPE=quota"
fi
# And with LLMSM_LIMITER_TYPE=quota the quota goes.
case_begin
PL_FN=disable_physical_limiter run_enable LLMSM_LIMITER_TYPE=quota
rm -f "$DEPLOYS/scaling-manager-policy.policy"
if [ "$RC" -ne 0 ]; then
    fail "disable LLMSM_LIMITER_TYPE=quota failed over a quota-only policy: $(tail -6 "$WORK/out")"
elif ! grep -q 'CALL\[patch configmap scaling-manager-scaling-policy-config -n scaling-manager-policy --type=merge -p {"data":{"default":null}}' "$CALLS"; then
    fail "disable LLMSM_LIMITER_TYPE=quota did not remove the quota-only default entry: $(grep 'CALL\[patch' "$CALLS")"
elif ! grep -q 'Removed the quota limiter from scaling-manager-policy/scaling-manager-scaling-policy-config' "$WORK/out"; then
    fail "disable LLMSM_LIMITER_TYPE=quota did not report the removal: $(tail -6 "$WORK/out")"
else
    ok "disable LLMSM_LIMITER_TYPE=quota removes a quota-only policy's entry"
fi
# S4: the quota gone, the Kueue controller read it needed goes too, by label --
# for tenant-g, the controller that reads scaling-manager-policy, and not for tenant-a,
# whose own policy this did not touch.
case_begin
if ! grep -qF "CALL[delete role,rolebinding -A -l llm-d.ai/scaling-manager-kueue-controller-reader=$(llmsm_ns_suffix tenant-g) --ignore-not-found]" "$CALLS"; then
    fail "disable LLMSM_LIMITER_TYPE=quota left the Kueue controller read of tenant-g: $(grep 'CALL\[delete' "$CALLS" | head -4)"
elif grep -qF "reader=$(llmsm_ns_suffix tenant-a)" "$CALLS"; then
    fail "disable LLMSM_LIMITER_TYPE=quota revoked tenant-a, whose policy it did not clear: $(grep 'CALL\[delete' "$CALLS" | head -4)"
else
    ok "disable LLMSM_LIMITER_TYPE=quota revokes the Kueue controller read by its label, only where the quota went"
fi
case_begin
if ! borrow_revoked tenant-g; then
    fail "disable LLMSM_LIMITER_TYPE=quota left the borrowing read of tenant-g: $(grep 'CALL\[delete' "$CALLS" | head -4)"
elif borrow_revoked tenant-a; then
    fail "disable LLMSM_LIMITER_TYPE=quota revoked the borrowing read of tenant-a, whose policy it did not clear"
else
    ok "disable LLMSM_LIMITER_TYPE=quota revokes the borrowing read where the quota went"
fi

# LLMSM_LIMITER_TYPE=all removes every limiter whatever its kind: the uninstall
# form. Over a composed policy both entries go, in one write, exit 0.
case_begin
printf 'limiters:\n- type: gpu-inventory\n- name: tenant-quota\n  type: quota\n  scope: cluster\n  quotas:\n    H200: 8\n' > "$DEPLOYS/scaling-manager-policy.policy"
PL_FN=disable_physical_limiter run_enable LLMSM_LIMITER_TYPE=all
rm -f "$DEPLOYS/scaling-manager-policy.policy"
if [ "$RC" -ne 0 ]; then
    fail "disable LLMSM_LIMITER_TYPE=all failed over a composed policy: $(tail -6 "$WORK/out")"
elif ! grep -q 'CALL\[patch configmap scaling-manager-scaling-policy-config -n scaling-manager-policy --type=merge -p {"data":{"default":null}}' "$CALLS"; then
    fail "disable LLMSM_LIMITER_TYPE=all did not remove both limiters: $(grep 'CALL\[patch' "$CALLS")"
elif ! grep -q 'Removed every limiter from scaling-manager-policy/scaling-manager-scaling-policy-config' "$WORK/out"; then
    fail "disable LLMSM_LIMITER_TYPE=all did not report the removal: $(tail -6 "$WORK/out")"
else
    ok "disable LLMSM_LIMITER_TYPE=all removes the physical limiter and the quota together"
fi
# And with nothing declared anywhere it is still exit 0, with a warning: an
# uninstall script must not fail on a cluster that was never bounded.
case_begin
PL_FN=disable_physical_limiter run_enable LLMSM_LIMITER_TYPE=all
if [ "$RC" -ne 0 ]; then
    fail "disable LLMSM_LIMITER_TYPE=all exited $RC over policies declaring no limiter: $(tail -6 "$WORK/out")"
elif grep -q 'CALL\[patch ' "$CALLS"; then
    fail "disable LLMSM_LIMITER_TYPE=all wrote a policy that declares no limiter: $(grep 'CALL\[patch' "$CALLS" | head -1)"
elif ! grep -q 'WARNING.*Nothing removed: no scaling-policy ConfigMap this command reads declares a limiter' "$WORK/out"; then
    fail "disable LLMSM_LIMITER_TYPE=all did not warn that nothing was removed: $(tail -4 "$WORK/out")"
else
    ok "disable LLMSM_LIMITER_TYPE=all with nothing declared exits 0 and warns"
fi
# An unknown kind is refused before anything is read.
case_begin
PL_FN=disable_physical_limiter run_enable LLMSM_LIMITER_TYPE=inventory-and-quota
if [ "$RC" -eq 0 ]; then
    fail "disable accepted LLMSM_LIMITER_TYPE=inventory-and-quota"
elif ! grep -q 'LLMSM_LIMITER_TYPE must be gpu-inventory, quota or all' "$WORK/out"; then
    fail "disable did not name the accepted kinds: $(tail -3 "$WORK/out")"
else
    ok "disable refuses an unknown LLMSM_LIMITER_TYPE, naming gpu-inventory, quota and all"
fi

# Messages may not claim more than the code did. Each failure below must end
# the command non-zero, name the step, and never print "in force".
# expect_failed <label> <substring the error must carry>
expect_failed() {
    if [ "$RC" -eq 0 ]; then
        fail "$1: exited 0: $(tail -6 "$WORK/out")"
    elif grep -q 'in force\|Removed the limiter' "$WORK/out"; then
        fail "$1: still announced success: $(grep 'in force\|Removed the limiter' "$WORK/out")"
    elif ! grep -q "$2" "$WORK/out"; then
        fail "$1: the error does not name the step ('$2'): $(tail -4 "$WORK/out")"
    else
        ok "$1"
    fi
}
case_begin
run_enable KFAIL_APPLY=scaling-manager-node-reader-role
expect_failed "a node-read grant that fails stops enable" "node read"
case_begin
run_enable 'KFAIL_APPLY=kind: namespace'
expect_failed "a policy Namespace that cannot be created stops enable" "could not create Namespace scaling-manager-policy"
case_begin
run_enable KCM_NOTFOUND=1 KFAIL_APPLY=scaling-manager-scaling-policy-config
expect_failed "a policy ConfigMap create that fails mid-pipeline stops enable" "could not create the policy ConfigMap"
case_begin
PL_FN=disable_physical_limiter run_enable KCM_FORBIDDEN=1
expect_failed "disable: a Forbidden policy read is an error, not nothing to disable" "not a NotFound"
case_begin
printf 'limiters:\n- type: gpu-inventory\n  name: physical\n' > "$DEPLOYS/tenant-a.policy"
PL_FN=disable_physical_limiter run_enable LLMSM_LIMITER_TARGETS=tenant-a KFAIL_PATCH=null
rm -f "$DEPLOYS/tenant-a.policy"
expect_failed "disable: a failed removal of the default entry is an error, not Removed" "could not remove the limiter from tenant-a"

# ---------------------------------------------------------------------------
# disable_physical_limiter writes nothing it does not have to, and nothing empty.
# ---------------------------------------------------------------------------
# No limiter declared anywhere: nothing to remove -- no write, no restart, and
# no "Removed" or "bounded by nothing" about a cluster that was never bounded.
# tenant-g reads scaling-manager-policy only at startup, so a write there used to restart it.
case_begin
PL_FN=disable_physical_limiter run_enable
if [ "$RC" -ne 0 ]; then
    fail "disable with no limiter declared failed: $(tail -6 "$WORK/out")"
elif grep -q 'CALL\[patch \|CALL\[apply \|CALL\[create configmap' "$CALLS"; then
    fail "disable wrote a policy that declares no limiter: $(grep 'CALL\[patch \|CALL\[apply \|CALL\[create configmap' "$CALLS" | head -2)"
elif grep -q 'CALL\[rollout restart' "$CALLS"; then
    fail "disable restarted a controller over a policy it did not change: $(grep 'CALL\[rollout restart' "$CALLS" | head -2)"
elif grep -q 'Removed the limiter\|bounded by nothing' "$WORK/out"; then
    fail "disable claimed a removal it did not make: $(grep 'Removed the limiter\|bounded by nothing' "$WORK/out" | head -2)"
elif ! grep -q 'No limiter declared in scaling-manager-policy/scaling-manager-scaling-policy-config; nothing to remove' "$WORK/out"; then
    fail "disable did not say there was nothing to remove: $(tail -4 "$WORK/out")"
elif ! grep -q 'WARNING.*Nothing removed: no scaling-policy ConfigMap this command reads declares a limiter' "$WORK/out"; then
    fail "disable with no limiter anywhere did not end on a warning that nothing was removed: $(tail -4 "$WORK/out")"
else
    ok "disable with no limiter declared writes nothing, restarts nothing, and says so"
fi

# A yq that fails on del(.limiters) is an error naming the step -- never an
# empty default written under "Removed the limiter".
REAL_YQ="$(command -v yq)"
mkdir -p "$WORK/yqdel" "$WORK/noyq"
cat > "$WORK/yqdel/yq" <<YQEOF
#!/usr/bin/env bash
for a in "\$@"; do case "\$a" in *"del("*) echo "Error: bad expression" >&2; exit 1 ;; esac; done
exec "$REAL_YQ" "\$@"
YQEOF
chmod +x "$WORK/yqdel/yq"
case_begin
printf 'scaleUpThreshold: 0.9\nlimiters:\n- type: gpu-inventory\n  name: physical\n' > "$DEPLOYS/tenant-a.policy"
GRANTS="$WORK/grants"; : > "$GRANTS"; CALLS="$WORK/calls"; : > "$CALLS"
(
    PATH="$STUB:$WORK/yqdel:$PATH"; export PATH
    KGRANTS="$GRANTS" KCALLS="$CALLS" KDEPLOYS="$DEPLOYS"; export KGRANTS KCALLS KDEPLOYS
    LLMSM_LIMITER_TARGETS=tenant-a; export LLMSM_LIMITER_TARGETS
    disable_physical_limiter
) >"$WORK/out" 2>&1
RC=$?
rm -f "$DEPLOYS/tenant-a.policy"
if grep -q 'CALL\[patch configmap scaling-manager-scaling-policy-config -n tenant-a' "$CALLS"; then
    fail "disable patched tenant-a after yq del(.limiters) failed: $(grep 'CALL\[patch' "$CALLS" | head -1)"
else
    expect_failed "disable: a yq del(.limiters) that fails is an error naming the step" "yq del(.limiters) failed"
fi
# yq absent altogether: refused up front, as enable is.
ln -sf "$STUB/kubectl" "$WORK/noyq/kubectl"
ln -sf "$(command -v jq)" "$WORK/noyq/jq"
case_begin
GRANTS="$WORK/grants"; : > "$GRANTS"; CALLS="$WORK/calls"; : > "$CALLS"
(
    PATH="$WORK/noyq"; export PATH
    KGRANTS="$GRANTS" KCALLS="$CALLS" KDEPLOYS="$DEPLOYS"; export KGRANTS KCALLS KDEPLOYS
    disable_physical_limiter
) >"$WORK/out" 2>&1
RC=$?
expect_failed "disable: no yq on PATH is refused, naming it" "yq is required"

# A warning kubectl prints on stderr while succeeding is not data. Merged with
# 2>&1 it became the policy-namespace label -- a namespace named "Warning: ..."
# granted and written to -- and made a declared limiter uncountable.
case_begin
run_enable KWARN=1
ns_rule="$(grep "^NSRULE scaling-manager-policy-namespace-reader-role-$(llmsm_ns_suffix tenant-g) " "$GRANTS" | tail -1 | awk '{print $NF}')"
if [ "$RC" -ne 0 ]; then
    fail "enable with a kubectl warning on stderr failed: $(tail -6 "$WORK/out")"
elif [ "$ns_rule" != "tenant-g,scaling-manager-policy" ]; then
    fail "with a warning on stderr, tenant-g's Namespace read is [$ns_rule], want tenant-g,scaling-manager-policy"
elif grep -q 'CALL\[[a-z]* .*Warning' "$CALLS"; then
    fail "a warning was used as a name: $(grep 'CALL\[[a-z]* .*Warning' "$CALLS" | head -1)"
else
    ok "a kubectl warning on stderr changes nothing enable grants or writes"
fi
case_begin
printf 'limiters:\n- type: gpu-inventory\n  name: physical\n' > "$DEPLOYS/tenant-c.policy"
PL_FN=disable_physical_limiter run_enable KWARN=1
rm -f "$DEPLOYS/tenant-c.policy"
if ! grep -q 'tenant-c/lookalike.*STILL declares 1 limiter' "$WORK/out"; then
    fail "with a warning on stderr, disable could not count a declared limiter: $(grep tenant-c "$WORK/out" | tail -2)"
else
    ok "a kubectl warning on stderr does not make a declared limiter uncountable"
fi

# The well-known namespace is fixed in the controller: LLMSM_POLICY_NS cannot move
# it, so naming another is refused, and `wellknown` always means scaling-manager-policy.
case_begin
run_enable LLMSM_POLICY_NS=platform-policy
if [ "$RC" -eq 0 ]; then
    fail "LLMSM_POLICY_NS=platform-policy was accepted: a policy no controller reads: $(grep -i 'policy in' "$WORK/out")"
elif [ -s "$GRANTS" ]; then
    fail "LLMSM_POLICY_NS=platform-policy granted before refusing: $(cat "$GRANTS")"
elif ! grep -q 'scaling-manager.llmd.ai/policy-namespace' "$WORK/out"; then
    fail "the LLMSM_POLICY_NS refusal does not say to label the namespace instead: $(tail -4 "$WORK/out")"
else
    ok "LLMSM_POLICY_NS naming another namespace is refused before anything changes"
fi
case_begin
got="$(PATH="$STUB:$PATH" KCALLS="$WORK/calls" KDEPLOYS="$DEPLOYS" LLMSM_POLICY_NS=platform-policy pl_policy_resolution tenant-g tenant-g)"
if [ "$got" = "scaling-manager-policy wellknown" ]; then
    ok "an unlabelled self-managed controller resolves to scaling-manager-policy whatever LLMSM_POLICY_NS says"
else
    fail "pl_policy_resolution for an unlabelled self-managed controller gave '$got', want 'scaling-manager-policy wellknown'"
fi

# ---------------------------------------------------------------------------
# MT1: a namespace-scoped controller counts GPU usage from a Pod list, and
# this command grants node read, not that list. Without it, GPUs other
# namespaces hold on the same nodes read as free. Published silently before.
case_begin
run_enable
if [ "$RC" -ne 0 ]; then
    fail "enable failed: $(tail -6 "$WORK/out")"
elif ! sed -n '/limiter is now in force/,$p' "$WORK/out" | grep -q 'cannot list Pods cluster-wide: tenant-g/scaling-manager-controller-manager\.'; then
    fail "the self-managed tenant-g cannot list Pods cluster-wide and gpu-inventory was published to it without a warning after the banner: $(grep -i 'pods' "$WORK/out" | head -3)"
elif grep 'cannot list Pods cluster-wide' "$WORK/out" | grep -q 'tenant-a/'; then
    fail "a cluster-scoped controller (its manager ClusterRole lists Pods) was named as unable to: $(grep 'cannot list Pods' "$WORK/out")"
else
    ok "gpu-inventory published to a namespace-scoped controller without a cluster-wide Pod list is warned about, by name"
fi
case_begin
run_enable KCANI_LIST_PODS=tenant-g
if grep -q 'cannot list Pods cluster-wide\|may list Pods cluster-wide' "$WORK/out"; then
    fail "a controller that may list Pods cluster-wide was warned about: $(grep 'Pods cluster-wide' "$WORK/out")"
else
    run_enable KCANI_LIST_PODS=unknown
    if ! grep -q 'Could not check .* may list Pods cluster-wide: tenant-g/scaling-manager-controller-manager\.' "$WORK/out"; then
        fail "a Pod list that could not be checked was not reported as unchecked: $(grep -i 'pods' "$WORK/out" | head -3)"
    else
        ok "a controller that may list Pods is not warned about, and one that cannot be checked is said to be unchecked"
    fi
fi

# MT4: two controllers granted Kueue borrowing whose namespaces share a cohort
# each spend the same free GPUs. Named per cohort when it can be read.
selfmanaged_json() {
    jq -n --arg img "$IMG" --arg name "$1" '
        {metadata: {name: $name, labels: {"app.kubernetes.io/name": "scaling-manager"}},
         spec: {template: {spec: {serviceAccountName: "scaling-manager-controller-manager", containers: [
           {name: "manager", image: $img, args: ["--watch-namespace=$(POD_NAMESPACE)"],
            env: [{name: "POD_NAMESPACE", valueFrom: {fieldRef: {fieldPath: "metadata.namespace"}}}]}]}}}}'
}
put_ns tenant-h "$(selfmanaged_json scaling-manager-controller-manager)"
put_ns tenant-i "$(selfmanaged_json scaling-manager-controller-manager)"
printf 'cq-h\n' > "$DEPLOYS/lq-tenant-h"
printf 'cq-i\n' > "$DEPLOYS/lq-tenant-i"
printf 'cq-g\n' > "$DEPLOYS/lq-tenant-g"
printf 'pool' > "$DEPLOYS/cq-cq-h"
printf 'pool' > "$DEPLOYS/cq-cq-i"
printf 'other' > "$DEPLOYS/cq-cq-g"
case_begin
run_enable LLMSM_LIMITER_TYPE=quota LLMSM_QUOTA_KUEUE=true LLMSM_QUOTAS=H200=-1 'LLMSM_LIMITER_TARGETS=tenant-g tenant-h tenant-i'
if [ "$RC" -ne 0 ]; then
    fail "enable with a borrowing Kueue quota failed: $(tail -6 "$WORK/out")"
elif ! grep -q 'KUEUE COHORT pool IS BORROWED FROM BY MORE THAN ONE CONTROLLER: tenant-h/scaling-manager-controller-manager, tenant-i/scaling-manager-controller-manager\.' "$WORK/out"; then
    fail "two borrowing controllers in cohort pool were not named: $(grep -i 'cohort' "$WORK/out" | head -4)"
elif grep -q 'KUEUE COHORT other' "$WORK/out"; then
    fail "a cohort with one borrowing controller was warned about: $(grep 'KUEUE COHORT other' "$WORK/out")"
elif ! grep -q 'Keep one borrowing controller per cohort' "$WORK/out"; then
    fail "the shared-cohort warning gives no remedy: $(grep -i 'cohort' "$WORK/out" | head -4)"
else
    ok "Kueue borrowing granted to two controllers sharing a cohort is warned about, naming the cohort and both"
fi
case_begin
run_enable LLMSM_LIMITER_TYPE=quota LLMSM_QUOTA_KUEUE=true LLMSM_QUOTAS=H200=-1 'LLMSM_LIMITER_TARGETS=tenant-g tenant-h tenant-i' KLQ_FORBIDDEN=1
if ! grep -q 'borrowing is granted to 3 controllers (tenant-g/scaling-manager-controller-manager, tenant-h/scaling-manager-controller-manager, tenant-i/scaling-manager-controller-manager), and the cohort of' "$WORK/out"; then
    fail "with the cohorts unreadable, the borrowers were not all named: $(grep -i 'cohort' "$WORK/out" | head -4)"
else
    run_enable LLMSM_LIMITER_TYPE=quota LLMSM_QUOTA_KUEUE=true LLMSM_QUOTA_KUEUE_BORROW=false LLMSM_QUOTAS=H200=-1 'LLMSM_LIMITER_TARGETS=tenant-g tenant-h tenant-i'
    if grep -qi 'cohort.*more than one\|borrowing is granted to' "$WORK/out"; then
        fail "LLMSM_QUOTA_KUEUE_BORROW=false grants no borrowing, and still warned: $(grep -i 'cohort' "$WORK/out" | head -2)"
    else
        ok "with the cohorts unreadable every borrower is named; with no borrowing nothing is"
    fi
fi
# 184-3: onboarding one tenant per run, the run alone never holds two
# borrowers. A controller an earlier run granted (its binding is live) in the
# same cohort is named too.
case_begin
printf '%s' '{"items":[{"metadata":{"name":"scaling-manager-kueue-borrow-reader-rolebinding-x"},"subjects":[{"kind":"ServiceAccount","name":"scaling-manager-controller-manager","namespace":"tenant-i"}]}]}' > "$DEPLOYS/crbs.json"
run_enable LLMSM_LIMITER_TYPE=quota LLMSM_QUOTA_KUEUE=true LLMSM_QUOTAS=H200=-1 'LLMSM_LIMITER_TARGETS=tenant-h'
if [ "$RC" -ne 0 ]; then
    fail "enable for one borrowing tenant failed: $(tail -6 "$WORK/out")"
elif ! grep -q 'KUEUE COHORT pool IS BORROWED FROM BY MORE THAN ONE CONTROLLER: tenant-h/scaling-manager-controller-manager, tenant-i/scaling-manager-controller-manager\.' "$WORK/out"; then
    fail "a borrower granted by an earlier run in the same cohort was not named: $(grep -i 'cohort' "$WORK/out" | head -4)"
else
    run_enable LLMSM_LIMITER_TYPE=quota LLMSM_QUOTA_KUEUE=true LLMSM_QUOTAS=H200=-1 'LLMSM_LIMITER_TARGETS=tenant-i'
    if grep -qi 'cohort.*more than one\|borrowing is granted to' "$WORK/out"; then
        fail "the run's own controller, granted earlier, was counted twice: $(grep -i 'cohort' "$WORK/out" | head -2)"
    else
        ok "a borrower an earlier run granted in the same cohort is named; the run's own earlier grant is not counted twice"
    fi
fi
rm -f "$DEPLOYS/crbs.json"

# A revoked Workload read: the controller keeps the Workloads its informer
# cached, so the admin is told to restart it -- and only when there was a
# grant to revoke.
revoke_tenant_a() { pl_sync_kueue_borrow_read tenant-a scaling-manager-controller-manager no; }
case_begin
run_enable PL_FN=revoke_tenant_a "KCRB_LIVE=scaling-manager-kueue-borrow-reader-rolebinding-$(llmsm_ns_suffix tenant-a)"
if [ "$RC" -ne 0 ]; then
    fail "revoking a granted Workload read failed: $(tail -4 "$WORK/out")"
elif ! grep -q 'RESTART THE CONTROLLER in tenant-a' "$WORK/out"; then
    fail "a revoked Workload read did not say to restart the controller: $(tail -4 "$WORK/out")"
else
    run_enable PL_FN=revoke_tenant_a
    if grep -q 'RESTART THE CONTROLLER' "$WORK/out"; then
        fail "a revoke with nothing granted asked for a restart: $(grep RESTART "$WORK/out")"
    else
        ok "a revoked Workload read says to restart the controller; nothing to revoke says nothing"
    fi
fi
# Hierarchical cohorts: ClusterQueues in sibling cohorts under one parent
# borrow from each other through it, so their borrowers share that tree.
case_begin
printf 'child-h' > "$DEPLOYS/cq-cq-h"
printf 'child-i' > "$DEPLOYS/cq-cq-i"
printf 'root' > "$DEPLOYS/cohort-child-h"
printf 'root' > "$DEPLOYS/cohort-child-i"
run_enable LLMSM_LIMITER_TYPE=quota LLMSM_QUOTA_KUEUE=true LLMSM_QUOTAS=H200=-1 'LLMSM_LIMITER_TARGETS=tenant-g tenant-h tenant-i'
if ! grep -q 'KUEUE COHORT root IS BORROWED FROM BY MORE THAN ONE CONTROLLER: tenant-h/scaling-manager-controller-manager, tenant-i/scaling-manager-controller-manager\.' "$WORK/out"; then
    fail "two borrowers in sibling cohorts under one parent were not named: $(grep -i 'cohort' "$WORK/out" | head -4)"
elif grep -q 'KUEUE COHORT other' "$WORK/out"; then
    fail "a cohort with no parent and one borrower was warned about: $(grep 'KUEUE COHORT other' "$WORK/out")"
else
    ok "borrowers in sibling cohorts under one parent are named by the parent"
fi
rm -f "$DEPLOYS"/cohort-*
rm -f "$DEPLOYS"/lq-* "$DEPLOYS"/cq-* "$DEPLOYS/tenant-h.json" "$DEPLOYS/tenant-i.json"

case_begin
CASES_EXPECTED=66
if [ "$CASES" -ne "$CASES_EXPECTED" ]; then
    fail "$CASES cases ran, not $CASES_EXPECTED. Update CASES_EXPECTED deliberately."
else
    ok "all $CASES cases ran"
fi

if [ "$FAIL" -ne 0 ]; then
    echo "physical limiter targets check FAILED"
    exit 1
fi
echo "physical limiter targets OK"
