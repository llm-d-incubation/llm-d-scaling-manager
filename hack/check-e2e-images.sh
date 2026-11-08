#!/usr/bin/env bash
#
# Fails when Go test code names a container image without a registry host.
#
# AGENTS.md: never use images from docker.io in e2e tests, and never rely on
# Docker Hub as the default registry. An image written as `curlimages/curl:x`
# has no host, so the container runtime resolves it to docker.io -- where
# anonymous pulls are rate limited and a CI run fails for reasons that have
# nothing to do with the change under test. Every image literal under test/
# must start with a registry host (a first path segment containing a dot or a
# port, or localhost) and that host must not be docker.io.
#
# What counts as an image literal: a quoted string on a line that mentions
# "image" (any case), shaped like a reference -- at least one slash and a tag
# or digest. Prose and paths do not have both.

set -u

ROOT="${CHECK_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
FAIL=0
PASS=0

while IFS= read -r hit; do
    file="${hit%%:*}"
    rest="${hit#*:}"
    line="${rest%%:*}"
    text="$(printf '%s' "${rest#*:}" | tr -d '\r')"
    for ref in $(printf '%s\n' "$text" | grep -oE '"[a-z0-9][a-z0-9._:/-]*/[a-z0-9._/-]+(:[A-Za-z0-9._-]+|@sha256:[0-9a-f]+)"' | tr -d '"'); do
        host="${ref%%/*}"
        case "$host" in
            docker.io|index.docker.io|registry-1.docker.io)
                FAIL=$((FAIL + 1)); echo "FAIL ${file#"$ROOT"/}:$line: $ref is on Docker Hub" ;;
            localhost|localhost:*|*.*|*:*)
                PASS=$((PASS + 1)) ;;
            *)
                FAIL=$((FAIL + 1)); echo "FAIL ${file#"$ROOT"/}:$line: $ref has no registry host, so it resolves to docker.io" ;;
        esac
    done
done < <(grep -rniE 'image' --include='*.go' "$ROOT/test" 2>/dev/null | grep -E '"[a-z0-9][a-z0-9._:/-]*/[a-z0-9._/-]+(:[A-Za-z0-9._-]+|@sha256:[0-9a-f]+)"')

echo "check-e2e-images: $PASS fully-qualified, $FAIL failed"
[ "$FAIL" -eq 0 ] && [ "$PASS" -gt 0 ]
