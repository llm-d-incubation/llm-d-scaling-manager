#!/usr/bin/env bash
#
# Runs deploy/ci-pr-checks/tools-pin.sh on fixture trees, then its `pr` rule on
# this tree. The v0.12.2 fixture covers a release commit that
# pinned v0.12.1's tools digest (30e265eb...) in every install guide, so a user
# following the docs at the tag ran the release before. That tree must fail.
# The controller examples must also use their release's image and checkout.

set -u
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PIN="$ROOT/deploy/ci-pr-checks/tools-pin.sh"
W="$(mktemp -d)"
trap 'rm -rf "$W"' EXIT
PASS=0
FAIL=0
ok()  { printf '  ok   %s\n' "$1"; PASS=$((PASS + 1)); }
bad() { printf '  FAIL %s\n' "$1"; FAIL=$((FAIL + 1)); }

IMG=ghcr.io/llm-d-incubation/llm-d-scaling-manager/tools
D121=sha256:30e265ebcdd7ff110faa6adb3aac92afad0a314cc28a9df12cc35e6f73d50855
D122=sha256:fc34a5e01283b5167e3e1accc0e7421ae6dccc42652d50e493a5f2a12ee59695

# tree <dir> <newTag> <ref>: a docs tree pinning tools<ref> in a guide.yaml,
# its README (CRLF, as a Windows checkout writes it) and README.md, with a
# placeholder and a variable that are no pins.
tree() {
    local d="$W/$1"
    rm -rf "$d"
    mkdir -p "$d/config/base/manager" "$d/docs/guides/g" "$d/docs/proposals"
    printf 'images:\n- name: controller\n  newTag: %s\n' "$2" > "$d/config/base/manager/kustomization.yaml"
    printf 'tools:\n  setup:\n    run: |\n      TOOLS=%s%s\n      smt version\n' "$IMG" "$3" > "$d/docs/guides/g/guide.yaml"
    # shellcheck disable=SC2016 # The fixture contains literal Markdown fences.
    printf '```bash\r\nTOOLS=%s%s\r\nsmt version\r\n```\r\n' "$IMG" "$3" > "$d/docs/guides/g/README.md"
    printf 'TOOLS=%s%s\n' "$IMG" "$3" > "$d/README.md"
    # shellcheck disable=SC2016 # The fixture contains a literal shell variable.
    printf 'TOOLS=%s@sha256:<the release tools digest>\nTOOLS=%s:$V\n' "$IMG" "$IMG" > "$d/docs/proposals/p.md"
    printf '%s\n' "$d"
}
run() { ROOT="$1" bash "$PIN" "${@:2}" > "$W/out" 2>&1; }

echo "tools-pin.sh on fixture trees"
t="$(tree v0122 v0.12.2 "@$D121")"
run "$t" pr; rc=$?
if [ "$rc" = 1 ] && grep -q "not v0.12.2's" "$W/out" && [ "$(grep -c "@$D121" "$W/out")" = 3 ]; then
    ok "the v0.12.2 release commit, pinning v0.12.1's digest, fails pr naming all 3 pins"
else
    bad "v0.12.2 fixture: exit $rc: $(head -3 "$W/out" | tr '\n' ' ')"
fi
run "$t" check v0.12.2 "$D122"; rc=$?
if [ "$rc" = 1 ]; then ok "and fails check v0.12.2 with v0.12.2's own digest"; else bad "check with v0.12.2's digest: exit $rc"; fi
t="$(tree rel v0.12.3 :v0.12.3)"
run "$t" pr; rc=$?
if [ "$rc" = 0 ] && grep -q '3 pins name v0.12.3' "$W/out"; then ok "a release commit pinning its own tag passes"; else bad "own tag: exit $rc: $(cat "$W/out")"; fi
run "$t" check v0.12.4; rc=$?
if [ "$rc" = 1 ]; then ok "check of another version fails"; else bad "other version: exit $rc"; fi
t="$(tree main122 main "@$D122")"
run "$t" pr; rc=$?
if [ "$rc" = 0 ]; then ok "main pinning a digest everywhere passes"; else bad "main digest: exit $rc: $(cat "$W/out")"; fi
run "$t" check v0.12.2 "$D122"; rc=$?
if [ "$rc" = 0 ]; then ok "check v0.12.2 with its digest passes after the docs PR"; else bad "check with digest: exit $rc"; fi
t="$(tree mainmain main :main)"
run "$t" pr; rc=$?
if [ "$rc" = 1 ] && grep -q 'not a release' "$W/out"; then ok "main pinning tools:main fails"; else bad "tools:main: exit $rc"; fi
t="$(tree mixed main "@$D122")"
printf 'TOOLS=%s@%s\n' "$IMG" "$D121" > "$t/README.md"
run "$t" pr; rc=$?
if [ "$rc" = 1 ] && grep -q '2 different tools images' "$W/out"; then ok "two releases' images in one tree fail"; else bad "mixed: exit $rc: $(head -2 "$W/out")"; fi
t="$(tree none main "@$D122")"
rm -f "$t/README.md" "$t/docs/guides/g/README.md" "$t/docs/guides/g/guide.yaml"
run "$t" pr; rc=$?
if [ "$rc" = 1 ] && grep -q 'nothing to check is not a pass' "$W/out"; then ok "no pin found is a failure, not a pass"; else bad "empty extraction: exit $rc"; fi

echo "tools-pin.sh pin"
t="$(tree pin v0.12.3 "@$D122")"
cp "$t/docs/proposals/p.md" "$W/p.before"
run "$t" pin :v0.12.3; rc=$?
if [ "$rc" = 0 ] && ROOT="$t" bash "$PIN" check v0.12.3 > /dev/null 2>&1 && cmp -s "$W/p.before" "$t/docs/proposals/p.md" \
    && [ "$(tr -cd '\r' < "$t/docs/guides/g/README.md" | wc -c)" = 4 ]; then
    ok "pin :v0.12.3 moves every pin, keeps the CRLF line ends, and leaves the placeholder and the variable"
else
    bad "pin: exit $rc: $(cat "$W/out")"
fi
run "$t" pin "@$D122"; rc=$?
if [ "$rc" = 0 ] && ROOT="$t" bash "$PIN" check v0.12.3 "$D122" > /dev/null 2>&1; then ok "pin @<digest> moves them to the digest"; else bad "pin digest: exit $rc"; fi
for r in :main @sha256:abc v0.12.3; do
    run "$t" pin "$r"; rc=$?
    if [ "$rc" = 2 ]; then ok "pin $r is usage"; else bad "pin $r: exit $rc"; fi
done

echo "tools-pin.sh --controller"
t="$(tree controller v0.12.3 :v0.12.3)"
controller_docs=(
    docs/reference/install-methods.md
    docs/reference/upgrade-and-uninstall.md
    docs/guides/benchmarking/README.md
)
for f in "${controller_docs[@]}"; do
    mkdir -p "$t/$(dirname "$f")"
    printf 'CONTROLLER_VERSION=v0.12.2\r\nCONTROLLER_IMAGE=%s@%s\r\n' "${IMG%/tools}" "$D122" > "$t/$f"
done
printf 'IMG=%s:main\r\n' "${IMG%/tools}" >> "$t/${controller_docs[0]}"
run "$t" --controller check v0.12.3; rc=$?
if [ "$rc" = 1 ] && [ "$(grep -c "@$D122" "$W/out")" = 3 ]; then
    ok "a release refuses the previous digest in all three controller examples"
else
    bad "stale controller examples: exit $rc: $(cat "$W/out")"
fi
if run "$t" --controller pin :v0.12.3 && run "$t" --controller pr &&
    grep -q '3 pins name v0.12.3' "$W/out" &&
    grep -q "^IMG=${IMG%/tools}:main" "$t/${controller_docs[0]}" &&
    [ "$(tr -cd '\r' < "$t/${controller_docs[0]}" | wc -c)" = 3 ]; then
    ok "pinning the release tag also changes the checkout and leaves other images and CRLF alone"
else
    bad "controller tag pin: $(cat "$W/out")"
fi
sed -i 's/^CONTROLLER_VERSION=v0.12.3/CONTROLLER_VERSION=v0.12.2/' "$t/${controller_docs[1]}"
run "$t" --controller pr; rc=$?
if [ "$rc" = 1 ] && grep -q "upgrade-and-uninstall.md checks out 'v0.12.2'" "$W/out"; then
    ok "a stale checkout fails even with the correct controller image tag"
else
    bad "stale checkout: exit $rc: $(cat "$W/out")"
fi
if run "$t" --controller pin :v0.12.3 && run "$t" --controller pin "@$D122"; then
    sed -i 's/newTag: v0.12.3/newTag: main/' "$t/config/base/manager/kustomization.yaml"
    if run "$t" --controller pr && run "$t" --controller check v0.12.3 "$D122"; then
        ok "the follow-up docs pin the published digest and retain the release checkout"
    else
        bad "controller digest pin: $(cat "$W/out")"
    fi
else
    bad "preparing the controller digest pin: $(cat "$W/out")"
fi
run "$t" --controller check v0.12.3 "$D121"; rc=$?
if [ "$rc" = 1 ]; then ok "a different digest is refused"; else bad "wrong digest: exit $rc"; fi
sed -i "s/$D122/$D121/" "$t/${controller_docs[2]}"
run "$t" --controller pr; rc=$?
if [ "$rc" = 1 ] && grep -q '2 different controller images' "$W/out"; then
    ok "main rejects mixed controller digests"
else
    bad "mixed controller digests: exit $rc: $(cat "$W/out")"
fi
sed -i 's/^CONTROLLER_IMAGE=/# CONTROLLER_IMAGE=/' "$t/${controller_docs[0]}"
run "$t" --controller pr; rc=$?
if [ "$rc" = 1 ] && grep -q 'install-methods.md must have exactly one' "$W/out"; then
    ok "a commented-out controller pin is rejected"
else
    bad "missing controller pin: exit $rc: $(cat "$W/out")"
fi

echo "this tree"
if ROOT="$ROOT" bash "$PIN" pr; then ok "this tree's pins follow its newTag"; else bad "this tree: see above"; fi
if ROOT="$ROOT" bash "$PIN" --controller pr; then ok "this tree's controller pins follow its newTag"; else bad "this tree's controller pins: see above"; fi

EXPECT_CASES=23
echo "docs image pins: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ] || exit 1
[ "$PASS" -eq "$EXPECT_CASES" ] || { echo "FAIL ran $PASS cases, expected $EXPECT_CASES"; exit 1; }
