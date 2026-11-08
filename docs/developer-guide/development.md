# Developer Guide

How to set up, build, run and change the scaling manager.

## Development Environment Setup

### Prerequisites

- Go 1.26.0+ (`go.mod`)
- Docker, Make, git
- kubectl 1.24+ (the installer refuses older), or `oc` on OpenShift
- Helm 3.8+ (the installer uses it for Prometheus and KEDA)
- Kind, for a local cluster
- `jq` 1.6+ and `yq` v4 (mikefarah's Go yq). CI pins yq v4.44.3; use it to
  reproduce a CI failure.
- `python3` with PyYAML, for the doc and dashboard checks

### Initial Setup

```bash
git clone https://github.com/llm-d-incubation/llm-d-scaling-manager.git
cd llm-d-scaling-manager
go mod download
make setup-envtest controller-gen kustomize
```

## Project Structure

Top level: `cmd/` (the manager, and `warmpool-proxy/`), `config/` (kustomize
bases, overlays and samples), `deploy/` (install scripts, `kind-emulator/`),
`hack/` (check and render scripts), `test/e2e/` (the e2e suite), `docs/`.

The packages under `internal/`, in the order one scaling decision flows through
them. The [signal dataflow](signal-dataflow.md#the-chain) draws the same chain
with the types that cross each boundary.

| Package | What it does |
|---------|--------------|
| `collector/` | Scrapes engine and EPP metrics (Prometheus or the pods), and attributes each replica to its variant. |
| `signals/` | Stateless arithmetic over those readings: `shape`, `itl`, `capacity`, `fleet`, `floor`. |
| `engines/analyzers/saturation`, `throughput`, `external` | Turn readings into demand and per-replica capacity. Never read a target. |
| `engines/aggregation/` | Supply, from the same per-replica capacity. |
| `engines/allocation/` | The optimizer and the GPU limiters (quota, inventory, Kueue). |
| `engines/steadystate/` | The per-cycle driver: runs the analyzers, applies thresholds, calls the optimizer, records the decision. |
| `scalingpolicy/` | Between the optimizer's plan and the published order: sticky scale-down, the reasons a model is blocked from scaling (to zero), and whether an inventory read is worth its cost. |
| `actuator/` | Emits the decision as metrics for the scale target. |
| `decision/` | Holds the latest decision per target in memory. |
| `scaler/` | KEDA's ExternalScaler gRPC service: serves the decision to KEDA. |
| `engines/scalefromzero/` | Wakes a model parked at zero replicas from the EPP queue. |
| `warmpool/` | Runs the warm pool: policy, pool Pods, the proxy. |
| `kueue/` | Reads GPU quotas from Kueue. |
| `gpunodes/` | Discovers GPU capacity and usage per node, per namespace and cluster-wide, for the limiters. |
| `gpuusage/` | Keeps the cluster's GPU usage picture refreshed independently of any engine, so it exists even when every model is parked. |
| `datastore/` | In-memory store of the EPP endpoint pools (and their metrics sources) and the namespaces being tracked. |
| `registry/` | The managed workloads, registered by KEDA calling the scaler. |
| `controller/` | Reconcilers for the ConfigMaps and InferencePools. |
| `config/` | Configuration types, loading and validation. |

Shared types live in `domain/`; the rest (`metrics/`, `logging/`, `utils/`,
`accelerator/`, ...) are support packages.

## Development Workflow

### Running Locally

#### Option 1: Outside the cluster

```bash
# Run the controller on your machine (connects to configured cluster)
make run
```

A controller run this way gets no KEDA calls, so it manages nothing until a
ScaledObject points at it. See [Debugging](debugging.md).

#### Option 2: In a Kind cluster

**Step 1 - create a cluster and deploy the infra:**

```bash
# New Kind cluster with emulated GPUs; drop CREATE_CLUSTER=true to reuse one
CREATE_CLUSTER=true make deploy-e2e-infra
```

This deploys the scaling manager, the llm-d EPP (GAIE standalone), Prometheus
and KEDA. It deploys no model.

**Step 2 (optional) - deploy a simulated model.** The samples under
`config/samples/simulator/nodeSelector/` come in four shapes:

```bash
# Decode only
kubectl apply -k config/samples/simulator/nodeSelector/decode/

# Prefill only
kubectl apply -k config/samples/simulator/nodeSelector/prefill/

# Both prefill and decode (disaggregated serving)
kubectl apply -k config/samples/simulator/nodeSelector/disaggregated/

# Decode as a LeaderWorkerSet
kubectl apply -k config/samples/simulator/nodeSelector/decode-lws/

# Then point the samples' triggers at the controller you installed
make scaledobjects-repoint
```

Each shape creates, in `llm-d-sim`, the workload (a Deployment of
`llm-d-inference-sim:v0.9.0`, or a LeaderWorkerSet for `decode-lws`), a Service
and a KEDA `ScaledObject`. It also creates a ServiceMonitor in
`scaling-manager-monitoring` that selects those pods.

**Always run `make scaledobjects-repoint` after applying.** The samples'
triggers name a controller in `scaling-manager-system`, but
`deploy-e2e-infra` installs it in `llm-d-sim`. A trigger that resolves to
nothing fails quietly: KEDA falls back to a CPU metric, the HPA reads
`cpu: <unknown>/80%`, and nothing scales. The target finds the running install
and rewrites only `scalerAddress`; see
[the kind emulator README](../../deploy/kind-emulator/).

The `ScaledObject` is what hands the workload to the scaling manager. The
controller watches and lists no workloads; it learns a variant exists from the
KEDA call that trigger causes. The trigger's `modelID` must match what the
container serves (`--model test-model` here): it groups variants for every
multi-variant decision. KEDA creates the HPA that scales the workload.

Check the chain is live:

```bash
kubectl -n llm-d-sim get scaledobject,hpa
kubectl -n llm-d-sim logs deploy/scaling-manager-controller-manager --tail=-1 | grep dev-model-decode
```

`deploy-e2e-infra` puts the controller in `llm-d-sim` (its
`CONTROLLER_NAMESPACE`). After `make deploy-scaling-manager`, use your `LLMSM_NS` instead
(default `scaling-manager-system`).

Clean up:

```bash
# Delete the sample with the same path you applied
kubectl delete -k config/samples/simulator/nodeSelector/decode/

# Delete the Kind cluster
make destroy-kind-cluster
```

### Making Changes

1. Create a branch: `git checkout -b feature/my-feature`.
2. Make your changes.
3. After changing `+kubebuilder:rbac` markers, run `make manifests`. It
   regenerates only `config/base/rbac/manager-clusterrole.yaml`.
   `internal/variant/deepcopy.go` is hand-maintained (no type has a
   `+kubebuilder:object` marker, so `make generate` produces nothing); edit it
   by hand when you change the structs in `internal/variant`.
4. Run `make test` and `make lint`.

## Building and Testing

```bash
# Binary at bin/manager
make build

# Image: build, push, or build multi-arch
make docker-build IMG=<your-registry>/scaling-manager-controller:tag
make docker-push IMG=<your-registry>/scaling-manager-controller:tag
PLATFORMS=linux/arm64,linux/amd64 make docker-buildx IMG=<your-registry>/scaling-manager-controller:tag

# Unit tests
make test

# E2E smoke tier against infra from make deploy-e2e-infra
make test-e2e-smoke
FOCUS="Basic Autoscaling" make test-e2e-smoke
```

Never run `go test ./...`: it also runs `test/e2e` against your current kube
context. A `FOCUS` that matches nothing runs 0 specs and still exits 0; check
that `Ran N of M Specs` has N above 0. The [Testing Guide](testing.md) and the
[E2E README](../../test/e2e/) cover the rest, including OpenShift.

## Debugging

To run the controller on your machine against a cluster, see
[Debugging](debugging.md).

### VSCode Launch Configuration

Create `.vscode/launch.json`:

```json
{
  "version": "0.2.0",
  "configurations": [
    {
      "name": "Debug Controller",
      "type": "go",
      "request": "launch",
      "mode": "auto",
      "program": "${workspaceFolder}/cmd/main.go",
      "env": {
        "KUBECONFIG": "${env:HOME}/.kube/config"
      },
      "args": []
    }
  ]
}
```

### Viewing Controller Logs

```bash
# llm-d-sim for the kind install; otherwise the LLMSM_NS you installed with
# (default scaling-manager-system)
kubectl logs -n "$LLMSM_NS" deploy/scaling-manager-controller-manager --tail=100 -f
```

A binary built with `go build -gcflags="all=-N -l" -o bin/manager cmd/main.go`
keeps the symbols a debugger such as Delve needs.

## Common Development Tasks

### Adding a New Metric

1. Add the metric name as a constant in `internal/constants/metrics.go`.
2. Declare and register the collector in `internal/metrics/metrics.go`.
3. Emit it from the code that owns the value.
4. Document it in [the metrics reference](../reference/metrics.md).
5. If a dashboard shows it, edit `deploy/grafana/operational-dashboard.json`
   and run `make dashboards-check` (CI runs it too).

### Modifying Optimization Logic

Change `internal/engines/allocation/` or `internal/engines/analyzers/`, add or
update unit tests, run `make test`, and update the design docs if the algorithm
changes.

## Documentation

Where a page goes is set by [AGENTS.md](../../AGENTS.md#documentation). Three
conventions that CI enforces:

- **Guides are generated.** The bash blocks in `docs/guides/<task>/README.md`
  come from its `guide.yaml`. Edit the YAML, run `make guides-render`, and
  `make guides-check` (which also checks every doc link).
- **Config file names** under `config/` follow `(<app>-)?<kind>.yaml`, e.g.
  `manager-clusterrole.yaml`.
- **Shell checks are fixtures.** A new check is a `hack/check-*.sh` wired into
  `make lint-deploy-scripts`; a check behind a new target is one nobody runs.

## Release Process

See [Release Process](release-process.md): the checklist, the tag and GitHub
Release (which tests, scans, builds, pushes and signs the image), and who may
release.

## Useful Commands

```bash
make fmt vet lint     # format, vet, lint
make lint-fix         # fix what the linter can
make destroy-kind-cluster create-kind-cluster   # reset the Kind cluster
```

For help, read [CONTRIBUTING.md](../../CONTRIBUTING.md) or search
[GitHub Issues](https://github.com/llm-d-incubation/llm-d-scaling-manager/issues).

## Known Setup Issues

### InferencePool CRD not found during `make deploy-e2e-infra`

```text
Error: no matches for kind "InferencePool" in version "inference.networking.x-k8s.io/v1alpha2"
ensure CRDs are installed first
```

`deploy/install-epp.sh` applies the CRDs, but the API server may not have
registered them before the next `kubectl apply`. Re-run
`make deploy-e2e-infra`; it is idempotent.
