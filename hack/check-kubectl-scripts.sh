#!/usr/bin/env bash
#
# Checks deploy/kubectl/scaling-manager-admin.sh and scaling-manager-owner.sh,
# the scripts the one-manifest install ships beside scaling-manager.yaml
# (docs/guides/kubectl-install):
#
#   - --print of every command equals the pinned commands in
#     hack/testdata/kubectl-scripts/ (UPDATE=1 rewrites them);
#   - without --print, the same commands run, in the same order, against a
#     stub kubectl, with PATH holding nothing but that stub: a script that
#     calls anything other than kubectl fails here;
#   - a bad argument exits 2 and runs nothing; a failed kubectl exits 1;
#   - no command edits a release file (no sed, no -i), and the files are
#     applied from --dir as named.
#
# Reads no cluster. Needs bash only.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GOLD="$ROOT/hack/testdata/kubectl-scripts"
ADMIN="$ROOT/deploy/kubectl/scaling-manager-admin.sh"
OWNER="$ROOT/deploy/kubectl/scaling-manager-owner.sh"
BASH_BIN="$(command -v bash)"
PASS=0
FAIL=0
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

ok() { PASS=$((PASS + 1)); echo "  ok: $1"; }
bad() { FAIL=$((FAIL + 1)); echo "  FAIL: $1"; }

# The release files, as downloaded (contents do not matter to the scripts).
DL="$WORK/dl"
mkdir -p "$DL"
for f in scaling-manager.yaml scaling-manager-keda.yaml scaling-manager-grants.yaml scaling-manager-monitoring.yaml; do
    echo "# $f" > "$DL/$f"
done
printf -- '-----BEGIN CERTIFICATE-----\n' > "$DL/prometheus-ca.crt"
printf 'apiVersion: v1\nkind: ConfigMap\n' > "$DL/policy.yaml"

# The stub kubectl (hack/testdata/kubectl-scripts/kubectl-stub), with an
# absolute bash shebang: PATH holds nothing but the stub.
STUB="$WORK/stub"
mkdir -p "$STUB"
{ printf '#!%s\n' "$BASH_BIN"; cat "$GOLD/kubectl-stub"; } > "$STUB/kubectl"
chmod +x "$STUB/kubectl"

# The commands checked, one per line: script, then its arguments.
CASES="$WORK/cases"
cat > "$CASES" <<'EOF'
admin install --prometheus-url https://prometheus.monitoring.svc:9090
admin install --prometheus-url https://prometheus.monitoring.svc:9090 --prometheus-ca prometheus-ca.crt --keda-namespace keda-system --watch-namespaces team-a,team-b
admin upgrade
admin upgrade --keda-namespace keda-system
admin move --keda-namespace keda-system
admin namespaces team-a,team-b
admin namespaces --all
admin set passive=false utilizationShare=shadow
admin grant-epp team-a team-a-epp
admin status
admin uninstall --keda-namespace keda-system
owner monitor team-a
owner network team-a
owner register team-a
owner register team-a policy.yaml
owner unregister team-a
owner policy team-a policy.yaml
owner grant pod-writer team-a
owner grant dset-role-writer team-a
owner status team-a
EOF

script_of() { case "$1" in admin) echo "$ADMIN" ;; owner) echo "$OWNER" ;; esac; }

# --- --print, pinned ---------------------------------------------------------
for who in admin owner; do
    out="$WORK/$who.print"
    : > "$out"
    while read -r s args; do
        [ "$s" = "$who" ] || continue
        echo "\$ scaling-manager-$who.sh --print $args" >> "$out"
        # shellcheck disable=SC2086
        (cd "$DL" && "$BASH_BIN" "$(script_of "$who")" --print $args) >> "$out" 2>&1 || echo "exit $?" >> "$out"
    done < "$CASES"
    if [ "${UPDATE:-}" = 1 ]; then
        mkdir -p "$GOLD"; cp "$out" "$GOLD/$who.txt"; echo "  wrote $GOLD/$who.txt"
    fi
    if [ ! -s "$GOLD/$who.txt" ]; then
        bad "no pinned output $GOLD/$who.txt (UPDATE=1 writes it)"
    elif diff -u "$GOLD/$who.txt" "$out" > "$WORK/$who.diff"; then
        ok "scaling-manager-$who.sh --print: $(grep -c '^\$ ' "$out") commands, $(grep -vc -e '^\$ ' -e '^#' "$out") kubectl lines, as pinned"
    else
        bad "scaling-manager-$who.sh --print differs from $GOLD/$who.txt:"; cat "$WORK/$who.diff"
    fi
    if grep -E '(^| )(sed|awk|perl|jq|yq|curl) ' "$out" | grep -v '^#' | grep -q .; then
        bad "scaling-manager-$who.sh prints a command other than kubectl"
    elif grep -v '^#' "$out" | grep -v '^\$ ' | grep -v '^kubectl ' | grep -q .; then
        bad "scaling-manager-$who.sh prints a line that is not a kubectl command: $(grep -v '^#' "$out" | grep -v '^\$ ' | grep -v '^kubectl ' | head -1)"
    else
        ok "scaling-manager-$who.sh prints kubectl commands only"
    fi
done

# --- run mode = --print, with nothing but kubectl on PATH --------------------
n=0 same=0
while read -r s args; do
    n=$((n + 1))
    log="$WORK/run-$n.log"
    : > "$log"
    # shellcheck disable=SC2086
    (cd "$DL" && env -i PATH="$STUB" STUB_LOG="$log" "$BASH_BIN" "$(script_of "$s")" $args) > "$WORK/run-$n.out" 2>&1
    rc=$?
    # shellcheck disable=SC2086
    want="$(cd "$DL" && "$BASH_BIN" "$(script_of "$s")" --print $args | grep -v '^#' | while IFS= read -r l; do
        rest="$l"
        while [[ "$rest" == *" | kubectl "* ]]; do printf '%s\n' "${rest%% | kubectl *}"; rest="kubectl ${rest#* | kubectl }"; done
        printf '%s\n' "$rest"
    done)"
    # Sorted: the stages of a pipe run at once and log in any order; the
    # order of the commands is what the pinned --print output shows.
    if [ "$rc" = 0 ] && [ "$(sort "$log")" = "$(printf '%s\n' "$want" | sort)" ]; then
        same=$((same + 1))
    else
        bad "$s $args: run mode (exit $rc) called kubectl otherwise than --print shows: $(diff <(printf '%s\n' "$want") "$log" | head -4 | tr '\n' ' ') $(head -2 "$WORK/run-$n.out" | tr '\n' ' ')"
    fi
done < "$CASES"
[ "$same" = "$n" ] && ok "every command ($n) runs exactly the kubectl calls --print shows, with only kubectl on PATH"

# --- refusals ----------------------------------------------------------------
refuse() {
    local label="$1" want="$2"; shift 2
    local log="$WORK/refuse.log"
    : > "$log"
    (cd "$DL" && env -i PATH="$STUB" STUB_LOG="$log" "$BASH_BIN" "$@") > "$WORK/refuse.out" 2>&1
    local rc=$?
    if [ "$rc" = "$want" ] && { [ "$want" = 1 ] || [ ! -s "$log" ]; }; then
        ok "$label: exit $rc$([ "$want" = 2 ] && echo ', no kubectl call')"
    else
        bad "$label: exit $rc, want $want; kubectl calls: $(wc -l < "$log"): $(head -1 "$WORK/refuse.out")"
    fi
}
refuse "install without --prometheus-url" 2 "$ADMIN" install
refuse "install with a URL that is not http(s)" 2 "$ADMIN" install --prometheus-url prometheus:9090
refuse "a KEDA namespace that is not a name" 2 "$ADMIN" upgrade --keda-namespace Keda_NS
refuse "watchNamespaces with a bad name" 2 "$ADMIN" namespaces team-a,Team_B
refuse "an empty namespace list (--all is explicit)" 2 "$ADMIN" namespaces ""
refuse "install with an empty --watch-namespaces (leave it out for every namespace)" 2 "$ADMIN" install --prometheus-url https://p:9090 --watch-namespaces ""
refuse "set with a quote in a value" 2 "$ADMIN" set 'limiter=a"b'
refuse "set without =" 2 "$ADMIN" set passive
refuse "an unknown admin command" 2 "$ADMIN" deploy
refuse "install with no scaling-manager.yaml in --dir" 2 "$ADMIN" --dir "$WORK/empty" install --prometheus-url https://p:9090
refuse "the owner script never deploys the manager" 2 "$OWNER" install team-a
refuse "an unknown grant" 2 "$OWNER" grant cluster-admin team-a
refuse "an owner command without a namespace" 2 "$OWNER" monitor
LOGF="$WORK/fail.log"; : > "$LOGF"
(cd "$DL" && env -i PATH="$STUB" STUB_LOG="$LOGF" STUB_FAIL=1 "$BASH_BIN" "$ADMIN" upgrade) > "$WORK/fail.out" 2>&1
rc=$?
if [ "$rc" = 1 ] && [ "$(wc -l < "$LOGF")" = 1 ] && grep -q 'failed: kubectl apply' "$WORK/fail.out"; then
    ok "a failed kubectl stops the script at that command, exit 1"
else
    bad "a failed kubectl: exit $rc after $(wc -l < "$LOGF") calls: $(head -1 "$WORK/fail.out")"
fi

# --- no file is edited --------------------------------------------------------
if grep -nE '(^|[^-])\bsed\b|\s-i\s|>\s*"?\$DIR|>\s*"?\$\(file' "$ADMIN" "$OWNER" | grep -v '^\S*:[0-9]*:\s*#' | grep -q .; then
    bad "a script may edit a file: $(grep -nE '(^|[^-])\bsed\b|\s-i\s' "$ADMIN" "$OWNER" | grep -v ':\s*#' | head -2)"
else
    ok "neither script writes to a release file or calls sed"
fi
for f in "$DL"/*.yaml; do
    [ "$(cat "$f")" = "# ${f##*/}" ] || [ "${f##*/}" = policy.yaml ] || bad "${f##*/} changed during the runs"
done

echo
echo "kubectl scripts: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
