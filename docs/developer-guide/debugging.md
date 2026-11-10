# Run the scaling manager locally for debugging (connects to Cluster & Prometheus)

Run the llm-d-scaling-manager on your machine (IDE or terminal) against a real
cluster. An SSH tunnel lets the cluster reach your process: Prometheus scrapes
its `/metrics`, and KEDA can call its external scaler. Your process queries the
cluster's Prometheus through a port-forward.

[![debugging-with-remote-clusters.png](debugging-with-remote-clusters.png)](debugging-with-remote-clusters.png)

## Prerequisites

- `kubectl` configured for the target cluster (set `KUBECONFIG` if needed).
- A shell that can run `ssh` and `kubectl`.
- The scaling manager installed in the cluster, scaled to zero replicas.

## Steps

1. Create a Secret `ssh-key` with your public key, in a namespace of your
   choice (here `debugging`):

   ```yaml
   apiVersion: v1
   kind: Secret
   metadata:
     name: ssh-key
   type: Opaque
   stringData:
     id_ed25519.pub: |
       <your-ssh-public-key-here>
   ```

2. Deploy the SSH gateway and its RBAC
   ([`debugging-ssh-tunnel.yaml`](debugging-ssh-tunnel.yaml) in this directory).
   Its Service exposes `22` (ssh), `8443` (metrics) and `9090` (external scaler):

   ```shell
   kubectl apply -f debugging-ssh-tunnel.yaml
   ```

3. Create a token for the `ssh-gateway` ServiceAccount. The ServiceMonitor and
   the curl checks below use it:

   ```shell
   kubectl -n debugging create token ssh-gateway --duration=24h > /tmp/scaling-manager.token
   ```

4. Port-forward Prometheus (thanos-querier) and the SSH gateway:

   ```shell
   # Prometheus at localhost:9091, for your local process
   kubectl port-forward -n openshift-monitoring svc/thanos-querier 9091:9091

   # SSH gateway at localhost:2222
   kubectl port-forward -n debugging svc/ssh-gateway 2222:22 &
   ```

5. Open a reverse SSH tunnel from the cluster to your machine. Keep the session
   open while you debug:

   ```shell
   ssh -o StrictHostKeyChecking=no \
       -o UserKnownHostsFile=/dev/null \
       -p 2222 \
       -R 0.0.0.0:8443:localhost:8443 \
       -R 0.0.0.0:9090:localhost:9090 \
       linuxserver.io@localhost
   ```

   Each `-R 0.0.0.0:<port>:localhost:<port>` opens that port on the gateway pod
   for other pods: `8443` for metrics, `9090` for the external scaler.

6. Create a ServiceMonitor so the in-cluster Prometheus scrapes your metrics
   through the gateway:

   ```yaml
   apiVersion: monitoring.coreos.com/v1
   kind: ServiceMonitor
   metadata:
     name: tunnel-metrics-monitor
     namespace: openshift-user-workload-monitoring
   spec:
     endpoints:
       - bearerTokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token
         interval: 10s
         path: /metrics
         port: metrics
         scheme: https
         tlsConfig:
           insecureSkipVerify: true
     namespaceSelector:
       matchNames:
         - debugging
     selector:
       matchLabels:
         app: ssh-gateway
   ```

7. Set the environment:

   ```shell
   export GLOBAL_OPT_INTERVAL=60s
   export KUBECONFIG=/path/to/your/kubeconfig
   export PROMETHEUS_BASE_URL=https://127.0.0.1:9091
   export PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true
   export PROMETHEUS_BEARER_TOKEN="$(</tmp/scaling-manager.token)"
   export LLMSM_SCALE_TO_ZERO=false
   ```

8. Run the scaling manager:

   ```shell
   /path/to/scaling-manager/binary --metrics-bind-address :8443 --metrics-secure=true --v=4
   ```

   Metrics are served on `https://localhost:8443`, and in the cluster at
   `https://ssh-gateway.debugging.svc.cluster.local:8443`. The external scaler
   listens on `:9090` (`--external-scaler-bind-address`).

9. Point a workload at your process. The controller scales only what KEDA asks
   it about, and KEDA dials the address in each ScaledObject's
   `scalerAddress` (normally the in-cluster `scaling-manager-external-scaler` Service on
   `9090`). Set that field on one ScaledObject to
   `ssh-gateway.debugging.svc.cluster.local:9090`.

Log verbosity comes from `--v=N` (`cmd/main.go`, default 2; `--v=4` shows debug
lines) or, when no `--v` is given, from the `LOG_LEVEL` environment variable
(`error`, `warn`, `info`, `debug`, `trace`; `internal/logging`).
`--zap-log-level` beats both. To raise verbosity in a cluster, set
`LOG_LEVEL=debug` on the manager container, or add `--v=4` to its `args`.

## Verification

```shell
# From your machine
curl -k https://localhost:8443/metrics -H "Authorization: Bearer $(</tmp/scaling-manager.token)"

# From the gateway pod (proves the tunnel reaches your process)
kubectl exec -n debugging deploy/ssh-gateway -- \
  curl -k https://localhost:8443/metrics -H "Authorization: Bearer $(</tmp/scaling-manager.token)"
```

From any other pod: `https://ssh-gateway.debugging.svc.cluster.local:8443/metrics`.

If something does not work:

- Are the port-forwards still running? Re-run them.
- Is the SSH session still connected?
- Read your local logs for TLS or auth errors.
- Read the Prometheus and Prometheus Operator logs in `openshift-monitoring`
  and `openshift-user-workload-monitoring`.
- A 401/403 from the ServiceMonitor: the token in `/tmp/scaling-manager.token` must belong
  to the ServiceAccount the ServiceMonitor uses.

**Security.** These steps skip TLS verification and use a 24h token, for a
local debug loop only. Do not use them in production.

## FAQ

- **Why forward thanos-querier?** The scaling manager reads metrics from
  Prometheus; the port-forward makes the cluster's endpoint reachable on
  localhost.
- **Why a reverse SSH tunnel?** It lets the cluster reach your process without
  exposing your machine to the cluster network. Actuation goes through the
  gRPC external scaler: KEDA calls it and scales the workload. `/metrics` is
  for observation only.
- **I may not create the ServiceAccount or the cluster roles.** Use an existing
  ServiceAccount with enough permissions: run the gateway Deployment under it
  and create the token for it.
