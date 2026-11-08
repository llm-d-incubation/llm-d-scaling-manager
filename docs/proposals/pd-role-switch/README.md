# Replicas that change P/D role in place (experimental)

> **Not a supported path.** This needs a patched vLLM that serves
> `POST /switch_pd_role`. No released vLLM has that route, and this release of
> the scaling manager does not ship or drive it. Read this as a design note with
> a manifest you can try, not as a guide. The scoping experiment it grew from is
> [`test/experiments/pd-role-swap/`](../../../test/experiments/pd-role-swap/);
> the plan for measuring what it costs is [evaluation.md](evaluation.md).

A prefill replica and a decode replica of the same model differ by configuration,
not by weights. An engine can therefore be told to change role and does so in
**378 ms**, without reloading about 700 GiB of weights or starting a new pod.

This note deploys GLM-5.2-FP8 on llm-d in a shape where that is usable. The
shape matters: a Deployment-based llm-d P/D deployment **cannot** host role
switching, for a reason measured below.

The engine side lives in the vLLM fork, not here:

| what | where |
| --- | --- |
| the switch itself, `POST /switch_pd_role` | [`vllm/v1/engine/pd_role.py`](https://github.com/ev-shindin/vllm-conf/blob/feat/pd-role-switch/vllm/v1/engine/pd_role.py) |
| measurements, reproduction, troubleshooting | [`tools/pd_role_switch/README.md`](https://github.com/ev-shindin/vllm-conf/blob/feat/pd-role-switch/tools/pd_role_switch/README.md) |
| running the fork on a stock vLLM image | same README, step 1b |
| what it costs, for a non-engine audience | [`tools/pd_role_switch/ARCHITECTURE.md`](https://github.com/ev-shindin/vllm-conf/blob/feat/pd-role-switch/tools/pd_role_switch/ARCHITECTURE.md) |

## Why a role label in a selector cannot work

llm-d decides which endpoints are prefill and which are decode from a **pod
label**, `llm-d.ai/role`. Its Deployment-based P/D deployments put that label in
each role Deployment's `spec.selector`:

```json
"selector": {"matchLabels": {"llm-d.ai/guide": "...", "llm-d.ai/role": "prefill", ...}}
```

A Deployment's selector is immutable, and a Deployment owns exactly the pods
matching it. So relabelling a running pod does not move it between roles — it
removes the pod from its owner.

`check-label-semantics.sh` demonstrates it both ways in about a minute, with no
GPUs:

```bash
./check-label-semantics.sh "$NAMESPACE"
```

```
role IN  selector: pods=3 owner_of_relabelled=<none, ORPHANED>
role OUT selector: pods=2 owner_of_relabelled=labeltest-role-out-of-selector-54c5d4f558

PASS  role in the selector: relabelling orphaned the pod and forced a replacement
PASS  role out of the selector: pod kept its owner, no replacement
```

Relabelling with the role in a selector gets you an orphan **and** a cold
replacement — the opposite of the point. So the role must be kept out of every
selector.

## The deployment

[`glm52-switchable.yaml`](glm52-switchable.yaml) runs every switchable replica in
**one** LeaderWorkerSet. Each group is one replica: two 8-GPU pods at EP=16, the
topology the switch was measured on. The leader serves the API; the worker is
headless.

LeaderWorkerSet selects its pods on its own `leaderworkerset.sigs.k8s.io/*`
labels, so `llm-d.ai/role` is an ordinary mutable label on the leader pod. The
label-semantics script checks the Deployment case; the LeaderWorkerSet case has
not been checked yet.

The InferencePool selects on `llm-d.ai/guide`, which only leaders carry, so a
role change never ejects a replica from the pool, and the endpoint picker needs
no changes — it already filters on the role label and does not care who owns
the pod.

**The ratio becomes a labelling decision.** Scaling the LeaderWorkerSet changes
total capacity; relabelling changes the prefill:decode split.

```bash
kubectl apply -n "$NAMESPACE" -f glm52-switchable.yaml
```

Settings in there that are not guessable, and each of which has cost a run:

| setting | why |
| --- | --- |
| `VLLM_NIXL_SIDE_CHANNEL_HOST` from `status.podIP` | the peer dials this for the KV handshake; the default `localhost` cannot work across pods |
| `containerPort: 5600` named `nixl` | the KV side channel |
| `VLLM_SERVER_DEV_MODE=1` | `/switch_pd_role` is a dev route. Without it every switch returns 404 — which reads as a 6 ms switch on an engine that still answers correctly |
| `VLLM_USE_DEEP_GEMM=0`, no `--moe-backend` | `deepep_v2` pads its layout and deep_gemm asserts on the padded tensor |
| `VLLM_DEEPEP_V2_ALLOW_HYBRID_MODE=0` | 0 across nodes, 1 on a single node; the value inverts |
| `VLLM_PD_KEEP_PREVIOUS=1` | parks the outgoing buffer, which keeps CUDA graphs valid and makes the return switch a reuse |
| `--max-num-batched-tokens=2048` | boot at the **prefill** budget. It sizes buffers at init, so a switch may lower it but never raise it past launch |
| `kv_role: kv_both` | covers both directions; llm-d sets it on both roles and the switch leaves it alone |

## Changing a replica's role

Four steps. Only step 3 is the engine's. Act on the leader pod; the worker
follows.

```bash
POD=$(kubectl get pod -n "$NAMESPACE" -l llm-d.ai/guide=glm52-switchable \
        -o name | head -1)

# 1. out of BOTH roles, so the picker stops selecting it. Pool membership is
#    untouched -- that keys on llm-d.ai/guide.
kubectl label -n "$NAMESPACE" "$POD" llm-d.ai/role-

# 2. let in-flight requests finish

# 3. switch the engine -- and CHECK IT. A switch that did nothing also returns
#    fast and still answers correctly, so timing and output prove nothing.
kubectl exec -n "$NAMESPACE" "$POD" -- curl -s -X POST localhost:8000/switch_pd_role \
  -H 'Content-Type: application/json' \
  -d '{"backend":"deepep_v2","max_num_tokens":128,"max_num_batched_tokens":128}'
#    expect ranks_switched == 16 and layers_switched > 0

# 4. back in, as the new role
kubectl label -n "$NAMESPACE" "$POD" llm-d.ai/role=decode
```

Between steps 1 and 4 the replica serves nothing. That is the cost of doing it
safely — a few hundred milliseconds, against the minutes a replacement replica
takes.

**The engine must not relabel itself.** That would put Kubernetes write
credentials in every inference pod so each could mutate cluster state about
itself. The engine owns the mechanism; the controller owns the decision, the
label and the ordering.

## Where the scaling manager comes in

Steps 1, 2 and 4 are policy and orchestration, and belong to the scaling
manager — it already watches load. **That controller does not exist yet**;
today the sequence above is run by hand.

Permissions are mostly there. The default ClusterRole only reads pods, but
`patch` on pods is granted per namespace by `role/scaling-manager-pod-writer`
([`deploy/lib/pod_writer.sh`](../../../deploy/lib/pod_writer.sh)) for the warm
pool and the utilization-share optimizer, and by the namespace-scoped install's
Role. The warm pool already writes InferencePool selector labels onto pods, so
label writes have a precedent. The manager does not use that grant for role
switching. It would also need `pods/exec`, or a network path to the leader, to
send the switch.

What it would need to decide: when the ratio is wrong, and which replica to
convert. A natural first signal is decode KV utilisation near its limit while
prefill has headroom.

## What is proven, and what is not

Measured on hardware (details in the fork's README):

- the switch, 16/16 ranks at EP=16 across two nodes, output identical
- KV transfers between two GLM-5.2 engines and **still transfers after both
  change role**, in the reversed direction
- both role buffers resident for 286 MiB; the return switch allocates 0 MiB
- the label semantics above, for a Deployment, both ways

Not yet run: **this manifest end to end under a live endpoint picker**, and the
label semantics under a LeaderWorkerSet. The pieces are each verified; the
assembly is not. Nor is the throughput cost free — a switchable engine gives up
about 13.7% of decode throughput against a dedicated decode replica, because it
cannot use the same expert kernel. The fork's README has that comparison in
full.
