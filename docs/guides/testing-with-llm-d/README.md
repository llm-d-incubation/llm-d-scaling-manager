# Test the scaling manager against a full llm-d stack

## Overview

This brings up llm-d and the scaling manager on a local kind cluster with
**emulated GPUs**, so you can run the whole chain (model server, EPP, KEDA, the
scaling manager) on a laptop. The model servers are simulators: they answer
requests and emit vLLM metrics without a GPU.

On a real cluster, install llm-d from its own
[guides](https://github.com/llm-d/llm-d/tree/main/guides), then follow [Install
the scaling manager in a namespace](../install-in-namespace/).

## Prerequisites

- [ ] kind, kubectl, helm, jq and a container runtime. No GPUs.

## Installation Instructions

### 1. Create the cluster

<!-- guide:prerequisites.cluster start -->
```bash
make create-kind-cluster
```
<!-- guide:prerequisites.cluster end -->

Nodes are labelled with emulated GPU capacity, which is what lets the scaling manager resolve
accelerators and compute budgets.

### 2. Deploy the stack

<!-- guide:deploy.stack start -->
```bash
make deploy-e2e-infra SCALER_BACKEND=keda USE_SIMULATOR=true SCALE_TO_ZERO_ENABLED=true
```
<!-- guide:deploy.stack end -->

Prometheus, KEDA, the EPP and the scaling manager.

### 3. Deploy a model server

<!-- guide:deploy.workload start -->
```bash
kubectl apply -k config/samples/simulator/nodeSelector/decode/
```
<!-- guide:deploy.workload end -->

The sample includes its own ScaledObject, which is the registration. Its
`scalerAddress` names the default controller namespace, but the stack runs the
controller in `llm-d-sim`, so point it at the running install:

<!-- guide:deploy.repoint start -->
```bash
# Point the sample at the running install (it names the default controller namespace).
make scaledobjects-repoint
```
<!-- guide:deploy.repoint end -->

Skip this and KEDA cannot reach the scaler: the ScaledObject stays `READY False`
and the HPA reads `cpu: <unknown>/80%`.

## Verification

<!-- guide:verify.chain start -->
```bash
# The controller and the workload both run in llm-d-sim.
kubectl get scaledobject,hpa -n llm-d-sim
kubectl logs -n llm-d-sim deploy/scaling-manager-controller-manager --tail=-1 | grep scaling-decision
```
<!-- guide:verify.chain end -->

Or run the suite:

<!-- guide:verify.suite start -->
```bash
make test-e2e-smoke
```
<!-- guide:verify.suite end -->

`make test-e2e-full` runs everything. Pass `SCALE_TO_ZERO_ENABLED=true` to
include the scale-from-zero suites. Without it they skip, and the run still
reports success.

## Cleanup

<!-- guide:cleanup.cluster start -->
```bash
make destroy-kind-cluster
```
<!-- guide:cleanup.cluster end -->

## Configuration

Optional.

| Parameter | Default | Example |
| --- | --- | --- |
| `CLUSTER_NAME` | `kind-scaling-manager-gpu-cluster` | `my-test` |
| `CLUSTER_GPU_TYPE` | `nvidia-mix` | `nvidia` |
| `CLUSTER_NODES` | `3` | `1` |
| `CLUSTER_GPUS` | `16` | `4` |

<!-- guide:env.static.cluster start -->
```bash
export CLUSTER_GPU_TYPE=nvidia-mix CLUSTER_NODES=3 CLUSTER_GPUS=4
```
<!-- guide:env.static.cluster end -->

## Next

- [Testing](../../developer-guide/testing.md) — the suites, their labels and flags
- [Benchmark the scaling manager](../benchmarking/)
