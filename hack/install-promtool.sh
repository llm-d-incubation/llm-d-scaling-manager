#!/usr/bin/env bash
# Install a pinned promtool at $1 from the Prometheus release tarball.
# `go install .../cmd/promtool` is refused: prometheus's go.mod carries replace
# directives. The tarball is checked against a sha256 committed below, not one
# fetched beside it: a checksum from the same release only catches corruption,
# and a replaced release would carry a matching one.
#
#   hack/install-promtool.sh <path to install at> <version, e.g. 2.54.1>
#
# A new version needs its four sums added from the release's sha256sums.txt.
set -eu

DEST=$1
VERSION=$2
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in
  x86_64 | amd64) ARCH=amd64 ;;
  aarch64 | arm64) ARCH=arm64 ;;
  *) echo "FAIL: no promtool release for $(uname -m)" >&2; exit 1 ;;
esac
NAME="prometheus-${VERSION}.${OS}-${ARCH}"

case "$NAME" in
  prometheus-2.54.1.linux-amd64) WANT=31715ef65e8a898d0f97c8c08c03b6b9afe485ac84e1698bcfec90fc6e62924f ;;
  prometheus-2.54.1.linux-arm64) WANT=3d9946ca730f2679bbd63e9d40e559a0ba227a638d237e723af1a99bd7098263 ;;
  prometheus-2.54.1.darwin-amd64) WANT=841e823ddee1da5fcbce5266bd448c2511297c570a6310a7bf81b226f0def5ea ;;
  prometheus-2.54.1.darwin-arm64) WANT=ac80faf6bb4e135f58e7d3f2423f561286d07205a93fef2e4bf2c56ce1484038 ;;
  *) echo "FAIL: no pinned checksum for $NAME; add it to $0" >&2; exit 1 ;;
esac

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
# --retry-all-errors covers the release CDN's occasional HTTP 5xx, which plain
# --retry does not.
curl -fsSL --proto '=https' --retry 6 --retry-delay 10 --retry-all-errors -o "$WORK/$NAME.tar.gz" \
  "https://github.com/prometheus/prometheus/releases/download/v${VERSION}/$NAME.tar.gz"
GOT=$( (sha256sum "$WORK/$NAME.tar.gz" 2>/dev/null || shasum -a 256 "$WORK/$NAME.tar.gz") | awk '{print $1}')
if [ "$GOT" != "$WANT" ]; then
  echo "FAIL: $NAME.tar.gz checksum $GOT, pinned $WANT" >&2
  exit 1
fi
tar -xzf "$WORK/$NAME.tar.gz" -C "$WORK" "$NAME/promtool"
mkdir -p "$(dirname "$DEST")"
mv "$WORK/$NAME/promtool" "$DEST"
"$DEST" --version | head -1
