#!/usr/bin/env bash
# Wait until the model is actually SERVABLE, not merely until pods are Ready.
#
# Deployment readyReplicas is not the same condition as "a request will succeed".
# Between them sit the EndpointSlice, the InferencePool's view of it, and the
# router/EPP. A benchmark started in that window dies immediately and unhelpfully:
# guidellm validates its backend before generating any load, and a 503 there
# kills every worker at once --
#
#   httpx.HTTPStatusError: Server error '503 Service Unavailable' for '.../health'
#   RuntimeError: Worker process group startup failed: error_event is set
#
# which reads as a load-generator crash rather than "the model was not up yet".
# That cost two full benchmark runs, and it is invisible in the results: the run
# produces no results.json at all, so every metric reports "?" and the failure
# looks like a tooling bug somewhere else entirely.
#
# So gate on the endpoint the harness itself will use, and fail loudly with the
# last status if it never comes up.
#
# Usage: wait_serving.sh <namespace> [timeout-seconds]
set -u
# --help prints this file's header comment -- the documentation the script
# already carries, so it cannot drift from what the script does. Placed before
# any argument handling because several of these take a namespace as $1, and
# without it `--help` was consumed as one.
case "${1:-}" in
    -h|--help)
        sed -n '2,/^[^#]/p' "$0" | sed 's/^# \{0,1\}//; $d'
        exit 0
        ;;
esac


NS="${1:?usage: wait_serving.sh <namespace> [timeout]}"
TIMEOUT="${2:-600}"

# The router service the harness detects. Prefer an explicit override, else the
# EPP/router service in the namespace.
svc="${BENCHMARK_ENDPOINT_SVC:-}"
if [ -z "$svc" ]; then
    svc=$(kubectl get svc -n "$NS" -o name 2>/dev/null \
          | grep -E "router-epp|inference-gateway|epp$" | head -1 | cut -d/ -f2)
fi
[ -n "$svc" ] || { echo "wait_serving: no router/EPP service found in $NS" >&2; exit 1; }

ip=$(kubectl get svc "$svc" -n "$NS" -o jsonpath='{.spec.clusterIP}' 2>/dev/null)
# NOT ports[0]: the router-epp service exposes grpc-ext-proc (9002), http-metrics
# (9090), zmq (5557) and http (80) in that order, so the first port is gRPC and
# probing it returns nothing at all. Pick the HTTP port by name, the way the
# harness's own endpoint detection does.
port=$(kubectl get svc "$svc" -n "$NS" -o json 2>/dev/null | python3 -c '
import json, sys
ports = json.load(sys.stdin)["spec"].get("ports", [])
for want in ("http",):                       # by name first
    for p in ports:
        if p.get("name") == want:
            print(p["port"]); sys.exit(0)
for p in ports:                              # then the conventional HTTP ports
    if p.get("port") in (80, 8000, 8080):
        print(p["port"]); sys.exit(0)
if ports:
    print(ports[0]["port"])
')
[ -n "$ip" ] && [ -n "$port" ] || { echo "wait_serving: could not resolve $svc" >&2; exit 1; }

url="http://${ip}:${port}/health"
echo "  waiting for $url (timeout ${TIMEOUT}s)"

# Probed from inside the cluster: the service ClusterIP is not routable from here.
# A short-lived pod per attempt would be slow, so poll from one.
deadline=$(( $(date +%s) + TIMEOUT ))
last="never answered"
attempt=0
while [ "$(date +%s)" -lt "$deadline" ]; do
    attempt=$((attempt + 1))
    code=$(kubectl run wait-serving-$$-$attempt -n "$NS" --rm -i --restart=Never --quiet \
             --image=registry.k8s.io/e2e-test-images/agnhost:2.47 --command -- \
             /bin/sh -c "curl -s -o /dev/null -w '%{http_code}' --max-time 5 '$url'" 2>/dev/null | tr -dc '0-9')
    if [ "${code:-}" = "200" ]; then
        echo "  serving (HTTP 200) after $(( TIMEOUT - (deadline - $(date +%s)) ))s"
        exit 0
    fi
    last="HTTP ${code:-<no answer>}"
    sleep 10
done

echo "wait_serving: still not serving after ${TIMEOUT}s (last: $last)" >&2
echo "wait_serving: starting a benchmark now would fail in backend validation," >&2
echo "wait_serving: producing no results.json and a table of '?' values." >&2
exit 1
