#!/usr/bin/env bash
#
# Prints the disk left at the end of an e2e job and the lowest free space the
# job reached, from the samples deploy/ci-pr-checks/free-disk.sh records.
#
# Never fails the job: it runs after the e2e whatever its result, and a report
# step that goes red turns a passing run into a failing one. A sample line is
# "HH:MM:SS <free KiB>"; the sampler can be killed mid-line, so a line whose
# value is empty or not a number is skipped, not computed with.
# DISK_SAMPLES=<file> reads another file (hack/check-disk-report.sh uses it).

set -u

df -h / || true
docker system df 2>/dev/null || true

samples="${DISK_SAMPLES:-/tmp/free-disk-samples.txt}"
if [ ! -s "$samples" ]; then
    echo "disk-report: no free-space samples in $samples (free-disk.sh did not run, or its sampler died)"
    exit 0
fi

# Prints "<valid> <total> <time> <KiB>" for the lowest numeric sample, or
# "<valid> <total>" when there is none.
summary="$(awk '
    { total++ }
    NF >= 2 && $2 ~ /^[0-9]+$/ {
        valid++
        if (low == "" || $2 + 0 < low + 0) { low = $2; at = $1 }
    }
    END {
        if (valid > 0) printf "%d %d %s %s\n", valid, total, at, low
        else printf "%d %d\n", valid + 0, total + 0
    }' "$samples" 2>/dev/null)" || summary=""

read -r valid total at low <<<"$summary" || true
case "${valid:-}:${low:-}" in
    [0-9]*:[0-9]*)
        echo "disk-report: $valid of $total samples readable; lowest free space $((low / 1024)) MiB at $at UTC"
        ;;
    *)
        echo "disk-report: no readable free-space samples in $samples (${total:-0} lines, none of them \"HH:MM:SS <KiB>\")"
        ;;
esac
exit 0
