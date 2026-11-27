#!/usr/bin/env bash
#
# Refuses (exit 1) unless the release files baked into a tools image are the
# ones the release is about to upload: SHA256SUMS and every manifest and
# signature bundle byte for byte, and no tools tarball inside. The release
# workflow runs it on the pushed image, by digest, before the upload
# (ci-release.yaml, release-files); hack/check-release-install.sh runs it on
# fixtures. docs/proposals/two-command-install.md, "Where the files come from".
#
#   hack/compare-baked-release-files.sh <baked dir> <dist dir>
#   hack/compare-baked-release-files.sh --image <image> <dist dir>    needs docker
#   hack/compare-baked-release-files.sh --platform linux/arm64 --image <image> <dist dir>
#                                                       one variant of a multi-arch image
#
# <dist dir> is what make release-manifests wrote and the signing step signed:
# the manifests, scaling-manager-tools-<version>.tar.gz, SHA256SUMS and a
# <file>.sigstore.json beside each.

set -euo pipefail

usage() { echo "usage: $0 <baked dir> | --image <image>  <dist dir>" >&2; exit 2; }

IMAGE="" BAKED="" PLATFORM=""
if [ "${1:-}" = --platform ]; then
    PLATFORM="${2:-}"
    shift 2 || usage
    [ "${1:-}" = --image ] || usage
fi
if [ "${1:-}" = --image ]; then
    IMAGE="${2:-}"
    shift 2 || usage
else
    BAKED="${1:-}"
    shift || usage
fi
DIST="${1:-}"
[ -n "$DIST" ] && { [ -n "$IMAGE" ] || [ -n "$BAKED" ]; } || usage
[ -d "$DIST" ] || { echo "no dist directory $DIST" >&2; exit 2; }

T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT
if [ -n "$IMAGE" ]; then
    command -v docker >/dev/null 2>&1 || { echo "--image needs docker" >&2; exit 2; }
    cid="$(docker create ${PLATFORM:+--platform "$PLATFORM"} "$IMAGE" help)"
    rc=0
    docker cp "$cid:/opt/scaling-manager-tools/release" "$T/baked" >/dev/null || rc=$?
    docker rm "$cid" >/dev/null || true
    [ "$rc" = 0 ] || { echo "REFUSED: $IMAGE${PLATFORM:+ ($PLATFORM)} has no /opt/scaling-manager-tools/release" >&2; exit 1; }
    BAKED="$T/baked"
fi
[ -d "$BAKED" ] || { echo "REFUSED: no baked directory $BAKED" >&2; exit 1; }

bad=0
refuse() { echo "REFUSED: $*" >&2; bad=1; }

[ -f "$DIST/SHA256SUMS" ] || { echo "REFUSED: $DIST has no SHA256SUMS" >&2; exit 1; }
if [ ! -f "$BAKED/SHA256SUMS" ]; then
    refuse "the image carries no SHA256SUMS: it was built without --build-context release=<dist>"
elif ! cmp -s "$BAKED/SHA256SUMS" "$DIST/SHA256SUMS"; then
    refuse "the image's SHA256SUMS differs from the one to upload"
    diff "$DIST/SHA256SUMS" "$BAKED/SHA256SUMS" >&2 || true
fi

# Every file of the release but the tools tarball, the capacity planner
# binaries and their bundles is baked, and
# equal. Counted, so an extraction that found nothing cannot pass.
n=0 bundles=0
for f in "$DIST"/*; do
    [ -f "$f" ] || continue
    b="${f##*/}"
    case "$b" in scaling-manager-tools-*|capacity-planner-*) continue ;; esac
    n=$((n + 1))
    case "$b" in *.sigstore.json) bundles=$((bundles + 1)) ;; esac
    if [ ! -f "$BAKED/$b" ]; then
        refuse "$b is not in the image"
    elif ! cmp -s "$f" "$BAKED/$b"; then
        refuse "$b in the image differs from the one to upload (a re-signed bundle is new bytes: rebuild the image in the same run)"
    fi
done
for f in "$BAKED"/*; do
    [ -f "$f" ] || continue
    b="${f##*/}"
    case "$b" in
        scaling-manager-tools-*|capacity-planner-*) refuse "the image carries $b; it ships beside the image, not in it" ;;
        *) [ -f "$DIST/$b" ] || refuse "the image carries $b, which the release does not upload" ;;
    esac
done
# scaling-manager.yaml, the one manifest, has no dash after the prefix.
manifests="$(find "$DIST" -maxdepth 1 \( -name 'scaling-manager.yaml' -o -name 'scaling-manager-*.yaml' \) | wc -l | tr -d ' ')"
[ "$manifests" -gt 0 ] || refuse "no manifest in $DIST"
[ "$bundles" -gt "$manifests" ] || refuse "only $bundles bundles for $manifests manifests and SHA256SUMS in $DIST: unsigned files"
[ "$bad" = 0 ] || exit 1
echo "baked release files match${PLATFORM:+ ($PLATFORM)}: $n files ($manifests manifests, $bundles bundles, SHA256SUMS)"
