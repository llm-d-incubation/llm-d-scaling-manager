# Local development on kind with emulated GPUs

A kind cluster whose nodes advertise fake GPUs, with the scaling manager,
Prometheus, KEDA and an llm-d EPP on it — the same stack the e2e suite runs
against. Nothing here needs a GPU or a Hugging Face token.

```bash
CREATE_CLUSTER=true make deploy-e2e-infra
```

For the full llm-d stack on kind, see
[Test against a full llm-d stack](../../docs/guides/testing-with-llm-d/); for
every install variable, the [installer options](../../docs/reference/install-options.md).

## Prerequisites

Docker, kind, kubectl, Helm, jq and go-yq v4.

## What `deploy-e2e-infra` creates

| Component | Namespace |
| --- | --- |
| kind cluster `kind-scaling-manager-gpu-cluster`: 3 nodes, 16 emulated GPUs each, mixed vendors | — |
| The scaling manager controller (`scaling-manager-controller-manager`), cluster scope | `llm-d-sim` |
| kube-prometheus-stack | `scaling-manager-monitoring` |
| KEDA | `keda-system` |
| llm-d EPP and InferencePool (`llm-d-router-standalone` chart) | `llm-d-sim` |

No model server and no ScaledObject: the e2e specs create their own. Leave out
`CREATE_CLUSTER=true` to reuse a cluster that already exists. `IMG=<image>`
builds that image locally (unless `SKIP_BUILD=true`) and loads it into kind.

## Add a model server by hand

```bash
# prefill and decode (disaggregated serving)
kubectl apply -k config/samples/simulator/nodeSelector/disaggregated/
# or one of them
kubectl apply -k config/samples/simulator/nodeSelector/decode/
kubectl apply -k config/samples/simulator/nodeSelector/prefill/
kubectl apply -k config/samples/simulator/nodeSelector/decode-lws/

make scaledobjects-repoint
```

Each sample carries its own KEDA `ScaledObject`, and that object is the
registration: without it the scaling manager is never asked about the workload
and scales nothing.

**Run `make scaledobjects-repoint` after applying them.** The samples' triggers
name a controller in `scaling-manager-system`, but `deploy-e2e-infra`
installs it in `llm-d-sim`. A trigger that resolves to nothing fails quietly:
KEDA cannot fetch a metric spec, falls back to a CPU metric, and the HPA sits at
`cpu: <unknown>/80%` with `READY False` while nothing scales.

`scaledobjects-repoint` finds the running install itself and rewrites only
`scalerAddress`, leaving `modelID`, `variantCost` and replica bounds alone. It
is idempotent, and it touches only ScaledObjects that name the scaling manager
*and* point at an address where no scaler runs; one pointing at a second, live
install is left as it is. With several installs it will not guess — pass
`LLMSM_NS=<namespace>`.

## Cluster options

```bash
make create-kind-cluster CLUSTER_GPU_TYPE=mix CLUSTER_NODES=4 CLUSTER_GPUS=8
make destroy-kind-cluster
```

| Variable | `create-kind-cluster` default | Meaning |
| --- | --- | --- |
| `CLUSTER_GPU_TYPE` | `nvidia-mix` | `nvidia`, `amd`, `intel-gaudi`, `intel-i915`, `intel-xe`, `mix`, `nvidia-mix`, `amd-mix` |
| `CLUSTER_NODES` | `3` | nodes |
| `CLUSTER_GPUS` | `16` | emulated GPUs per node |
| `K8S_VERSION` | `v1.32.0` | node image version |

`CREATE_CLUSTER=true make deploy-e2e-infra` creates the cluster itself and
defaults to `mix`, 3 nodes and 16 GPUs. The GPUs are extended resources
(`nvidia.com/gpu`, `amd.com/gpu`, `habana.ai/gaudi`, `gpu.intel.com/i915`,
`gpu.intel.com/xe`) patched onto node status; no device exists.

`deploy/kind-emulator/setup.sh` and `teardown.sh` are what the two make targets
call; `deploy/kind-emulator/install.sh` is sourced by `deploy/install.sh` when
`ENVIRONMENT=kind-emulator`.

## Look at it

```bash
# controller logs
kubectl logs -n llm-d-sim deploy/scaling-manager-controller-manager -f

# controller metrics (HTTPS)
kubectl port-forward -n llm-d-sim svc/scaling-manager-controller-manager-metrics-service 8443:8443

# Prometheus
kubectl port-forward -n scaling-manager-monitoring svc/kube-prometheus-stack-prometheus 9090:9090

# what KEDA is doing
watch kubectl get scaledobject,hpa,deploy -n llm-d-sim
```

To send load, port-forward the gateway Service in `llm-d-sim`
(`kubectl get svc -n llm-d-sim`) to `localhost:8000`, then:

```bash
TARGET_URL=http://localhost:8000/v1/chat/completions \
MODEL_ID=<the sample's model> TOTAL_REQUESTS=100 BATCH_SIZE=10 \
  ./hack/burst_load_generator.sh
```

The script needs only `curl`; its header lists the other knobs (`BATCH_SLEEP`,
`MAX_TOKENS`, `CURL_TIMEOUT`).

## Run the e2e suite

```bash
make test-e2e-smoke
make test-e2e-full
```

See the [e2e README](../../test/e2e/) for what each runs and which specs skip
on which flags.

## Rebuild after a code change

```bash
make deploy-e2e-infra IMG=localhost/llm-d-scaling-manager:dev
```

It builds the image, loads it into kind, and re-applies the controller. Reusing
a tag can leave the running Pod on the old image; then
`kubectl rollout restart deploy/scaling-manager-controller-manager -n llm-d-sim`.

## Troubleshooting

**Cluster creation fails.** `kind delete cluster --name kind-scaling-manager-gpu-cluster`,
then create it again.

**Controller not starting.** Check its logs (above) and that KEDA's CRD is
served: `kubectl get crd scaledobjects.keda.sh`. The project installs no CRD of
its own.

**No GPUs on the nodes.** `kubectl get nodes -o json | jq '.items[].status.capacity'`
should list one of the extended resources above.

**`kind load` fails with "content digest ... not found".** Loading a
multi-platform image can miss blobs
([kubernetes-sigs/kind#3795](https://github.com/kubernetes-sigs/kind/issues/3795),
[#3845](https://github.com/kubernetes-sigs/kind/issues/3845)). The installer
already pulls a single platform first; to choose it yourself set
`KIND_IMAGE_PLATFORM=linux/amd64` (or `linux/arm64`), or build locally and
install with `LLMSM_IMAGE_PULL_POLICY=IfNotPresent`:

```bash
make docker-build IMG=ghcr.io/llm-d-incubation/llm-d-scaling-manager:main
CREATE_CLUSTER=true LLMSM_IMAGE_PULL_POLICY=IfNotPresent \
  make deploy-e2e-infra IMG=ghcr.io/llm-d-incubation/llm-d-scaling-manager:main
```

## More

- [Testing guide](../../docs/developer-guide/testing.md)
- [Development guide](../../docs/developer-guide/development.md)
