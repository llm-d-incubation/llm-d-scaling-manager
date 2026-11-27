#!/usr/bin/env bash
#
# The tools image the docs tell users to run: every `TOOLS=` line naming
# ghcr.io/llm-d-incubation/llm-d-scaling-manager/tools by a tag or a digest,
# in README.md and docs/ (guide.yaml and the README rendered from it alike).
#
# The tools image carries the release files and their signature bundles, both
# made by the release run, so its digest exists only after the tag is pushed
# and the image built: the tagged commit cannot name it. Before this, the
# release commit kept the previous release's digest, and a user who followed
# the docs at a tag ran the tools, and installed the files, of the release
# before. So a release pins its own image by tag, and the docs PR after the
# run moves the pin to the digest the run printed:
#
#   release PR (newTag vX.Y.Z)    tools-pin.sh pin :vX.Y.Z
#   release run, before building  tools-pin.sh check vX.Y.Z   (ci-release.yaml)
#   docs PR after the run         tools-pin.sh pin @sha256:<digest>
#   every PR                      tools-pin.sh pr             (hack/check-tools-pin.sh)
#
# Usage:
#   tools-pin.sh list                       each pin: <file>:<line> <ref>
#   tools-pin.sh check <version> [<digest>] each pin is tools:<version>, or
#                                           tools@<digest> (sha256:<64 hex>)
#   tools-pin.sh pr                         the rule for the tree's newTag: a
#                                           release tag, check it; main, all
#                                           pins one ref, a digest or a release tag
#   tools-pin.sh pin <ref>                  rewrite every pin to :vX.Y.Z or
#                                           @sha256:<64 hex>
# Add --controller before the command to check or update the controller
# examples instead. A tag pin also updates their manifest checkout version.
# ROOT: the tree to read (default: the repository this script is in).
# Exit 0 pass, 1 a pin breaks the rule (or none found), 2 usage.
set -euo pipefail

ROOT="${ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
KIND=tools
CONTROLLER_OPTION=
PIN_START=
IMAGE='ghcr.io/llm-d-incubation/llm-d-scaling-manager'
CONTROLLER_DOCS=(
    docs/reference/install-methods.md
    docs/reference/upgrade-and-uninstall.md
    docs/guides/benchmarking/README.md
)
if [ "${1:-}" = --controller ]; then
    KIND=controller
    CONTROLLER_OPTION=' --controller'
    ASSIGNMENT="CONTROLLER_IMAGE=$IMAGE"
    PIN_START='^'
    shift
else
    ASSIGNMENT="TOOLS=$IMAGE/tools"
fi
# The ref ends at white space or the end of the line, so a placeholder
# (tools@sha256:<digest>) or a variable (tools:$V) is no pin.
PIN_RE="$PIN_START${ASSIGNMENT//./\\.}"'[:@][^[:space:]<$]+([[:space:]]|$)'
VERSION_RE='^CONTROLLER_VERSION=([^[:space:]]+)[[:space:]]*$'
TAG_RE='^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$'
DIGEST_RE='^sha256:[0-9a-f]{64}$'

die() { echo "tools-pin: $*" >&2; exit 2; }

# files: the files that may hold a pin, one a line.
files() {
    if [ "$KIND" = controller ]; then
        local f
        for f in "${CONTROLLER_DOCS[@]}"; do
            [ ! -f "$ROOT/$f" ] || printf '%s\n' "$ROOT/$f"
        done
        return
    fi
    { [ ! -f "$ROOT/README.md" ] || echo "$ROOT/README.md"
      [ ! -d "$ROOT/docs" ] || find "$ROOT/docs" -type f \( -name '*.md' -o -name '*.yaml' \) -not -path '*/plans/*'
    } | LC_ALL=C sort
}

# pins: <file relative to ROOT>:<line> <ref>, one a line.
pins() {
    local f
    while IFS= read -r f; do
        { grep -nE "$PIN_RE" "$f" || true; } | tr -d '\r' | sed -E "s#^([0-9]+):.*${ASSIGNMENT//./\\.}([:@][^[:space:]]+).*#${f#"$ROOT"/}:\1 \2#"
    done < <(files)
}

# need_pins <pins>: refuse an empty extraction, which would pass every rule.
need_pins() {
    if [ "$KIND" = controller ]; then
        local f
        for f in "${CONTROLLER_DOCS[@]}"; do
            if [ ! -f "$ROOT/$f" ] || [ "$(grep -cE "$PIN_RE" "$ROOT/$f" || true)" -ne 1 ] ||
                [ "$(grep -cE "$VERSION_RE" "$ROOT/$f" || true)" -ne 1 ]; then
                echo "tools-pin: $f must have exactly one controller image pin and one CONTROLLER_VERSION assignment" >&2
                exit 1
            fi
        done
    fi
    [ -n "$1" ] && return 0
    echo "tools-pin: no $ASSIGNMENT pin found under $ROOT (README.md, docs/): nothing to check is not a pass" >&2
    exit 1
}

check_checkout_version() {
    [ "$KIND" = controller ] || return 0
    local expected="${1:-}" f version
    for f in "${CONTROLLER_DOCS[@]}"; do
        version="$(sed -nE "s/$VERSION_RE/\1/p" "$ROOT/$f")"
        expected="${expected:-$version}"
        if ! [[ "$version" =~ $TAG_RE ]] || [ "$version" != "$expected" ]; then
            echo "tools-pin: $f checks out '$version'; expected the controller release tag '$expected'" >&2
            exit 1
        fi
    done
}

cmd_check() {
    local version="${1:-}" digest="${2:-}" all bad
    [[ "$version" =~ $TAG_RE ]] || die "check: '$version' is not a release tag (vX.Y.Z)"
    [ -z "$digest" ] || [[ "$digest" =~ $DIGEST_RE ]] || die "check: '$digest' is not sha256:<64 hex>"
    all="$(pins)"
    need_pins "$all"
    bad="$(printf '%s\n' "$all" | awk -v t=":$version" -v d="@$digest" '$2 != t && ($2 != d || d == "@")')"
    if [ -n "$bad" ]; then
        echo "tools-pin: these docs pin a $KIND image that is not $version's$([ -z "$digest" ] || printf ' (%s)' "$digest"):" >&2
        printf '%s\n' "$bad" | sed 's/^/  /' >&2
        echo "  A release pins its own image by tag: deploy/ci-pr-checks/tools-pin.sh$CONTROLLER_OPTION pin :$version (docs/developer-guide/release-process.md)" >&2
        exit 1
    fi
    check_checkout_version "$version"
    echo "tools-pin: $(printf '%s\n' "$all" | wc -l | tr -d ' ') pins name $version's $KIND image"
}

cmd_pr() {
    local tag all refs r
    tag="$(awk '$1 == "newTag:" {print $2}' "$ROOT/config/base/manager/kustomization.yaml" | tr -d '"\r')"
    if [[ "$tag" =~ $TAG_RE ]]; then
        echo "tools-pin: newTag is $tag, a release commit"
        cmd_check "$tag"
        return
    fi
    all="$(pins)"
    need_pins "$all"
    refs="$(printf '%s\n' "$all" | awk '{print $2}' | LC_ALL=C sort -u)"
    if [ "$(printf '%s\n' "$refs" | wc -l)" -ne 1 ]; then
        echo "tools-pin: the docs pin $(printf '%s\n' "$refs" | wc -l | tr -d ' ') different $KIND images; one release's, everywhere:" >&2
        printf '%s\n' "$all" | sed 's/^/  /' >&2
        exit 1
    fi
    r="$refs"
    case "$r" in
        @*)
            [[ "${r#@}" =~ $DIGEST_RE ]] || { echo "tools-pin: '$r' is not @sha256:<64 hex>" >&2; exit 1; }
            check_checkout_version
            ;;
        :*)
            [[ "${r#:}" =~ $TAG_RE ]] || { echo "tools-pin: the docs pin $KIND$r, which is not a release: a digest, or a release tag" >&2; exit 1; }
            check_checkout_version "${r#:}"
            ;;
    esac
    echo "tools-pin: newTag is $tag; $(printf '%s\n' "$all" | wc -l | tr -d ' ') pins name $KIND$r"
}

cmd_pin() {
    local ref="${1:-}" f version='' n=0
    case "$ref" in
        :*) [[ "${ref#:}" =~ $TAG_RE ]] || die "pin: '$ref' is not :vX.Y.Z" ;;
        @*) [[ "${ref#@}" =~ $DIGEST_RE ]] || die "pin: '$ref' is not @sha256:<64 hex>" ;;
        *) die "pin: give :vX.Y.Z or @sha256:<64 hex>, not '$ref'" ;;
    esac
    need_pins "$(pins)"
    if [ "$KIND" = controller ]; then
        case "$ref" in
            :*) version="${ref#:}" ;;
            @*) check_checkout_version ;;
        esac
    fi
    while IFS= read -r f; do
        grep -qE "$PIN_RE" "$f" || continue
        PIN_START="$PIN_START" ASSIGNMENT="$ASSIGNMENT" REF="$ref" RELEASE_VERSION="$version" perl -i -pe '
            s{($ENV{PIN_START}\Q$ENV{ASSIGNMENT}\E)[:@][^\s<\$]+(?=\s|$)}{$1$ENV{REF}};
            s{^CONTROLLER_VERSION=\S+}{CONTROLLER_VERSION=$ENV{RELEASE_VERSION}} if length $ENV{RELEASE_VERSION};
        ' "$f"
        n=$((n + 1))
    done < <(files)
    echo "tools-pin: $n files now pin $KIND$ref"
}

case "${1:-}" in
    list) pins ;;
    check) shift; cmd_check "$@" ;;
    pr) cmd_pr ;;
    pin) shift; cmd_pin "$@" ;;
    *) die "usage: tools-pin.sh [--controller] (list | check <version> [<digest>] | pr | pin <ref>)" ;;
esac
