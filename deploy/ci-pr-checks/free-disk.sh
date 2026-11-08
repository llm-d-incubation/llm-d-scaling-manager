#!/usr/bin/env bash
#
# Frees runner disk before an e2e job builds an image and creates a kind cluster.
#
# Two unrelated PRs failed e2e setup within the hour with Grafana "not ready",
# and both logs carried the runner's own warning, "You are running out of disk
# space ... Free space left: 43 MB" (and 89 MB), minutes before the failure. A
# kind node that cannot unpack an image leaves the Pod waiting until helm's
# --wait gives up, and the error names the Deployment, not the disk.
#
# Removes preinstalled toolchains no e2e job uses (Go comes from setup-go into
# /opt/hostedtoolcache/go, which stays), then prunes Docker, which at job start
# holds nothing of ours. Prints `df -h /` before and after and what each
# directory freed, so the next disk failure is readable from the log.
# Never fails the job: a directory that is not on this runner image is skipped.

set -u

avail_kb() { df --output=avail -k / | tail -1 | tr -d ' '; }

echo "::group::df -h / before cleanup"
df -h /
echo "::endgroup::"

start_kb="$(avail_kb)"
for dir in \
    /usr/share/dotnet \
    /usr/local/lib/android \
    /opt/ghc \
    /usr/local/.ghcup \
    /opt/hostedtoolcache/CodeQL \
    /usr/share/swift \
    /usr/local/share/powershell \
    /usr/local/share/chromium \
    /usr/local/julia*; do
    [ -e "$dir" ] || { echo "skip $dir (not on this runner)"; continue; }
    before_kb="$(avail_kb)"
    sudo rm -rf "$dir"
    after_kb="$(avail_kb)"
    echo "freed $(( (after_kb - before_kb) / 1024 )) MiB: $dir"
done

before_kb="$(avail_kb)"
docker system prune -af --volumes >/dev/null 2>&1 || echo "docker system prune failed; continuing"
after_kb="$(avail_kb)"
echo "freed $(( (after_kb - before_kb) / 1024 )) MiB: docker system prune"

echo "::group::df -h / after cleanup"
df -h /
echo "::endgroup::"
echo "free-disk: freed $(( ($(avail_kb) - start_kb) / 1024 )) MiB in total, $(( $(avail_kb) / 1024 )) MiB available on /"

# Peak usage: sample free space every 15 s for the rest of the job (the runner
# kills orphan processes when the job ends). A df at the end misses the peak,
# because a job that deletes its kind cluster has given the space back by then.
# deploy/ci-pr-checks/disk-report.sh prints the low point.
nohup setsid bash -c 'while :; do printf "%s %s\n" "$(date -u +%H:%M:%S)" "$(df --output=avail -k / | tail -1 | tr -d " ")"; sleep 15; done' \
    >/tmp/free-disk-samples.txt 2>/dev/null </dev/null &
echo "free-disk: sampling free space to /tmp/free-disk-samples.txt"
