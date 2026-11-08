# Warm pool

A small population of GPU-**holding** Pods that keep several models resident but
asleep, so capacity for any of them arrives in seconds instead of ~41 s.

Design: [`docs/proposals/fast-model-loading.md`](../docs/proposals/fast-model-loading.md)
and [the implementation design](../docs/proposals/fast-model-loading-implementation.md).

**Status: built, and running on a test cluster.** A pool Pod holding two models
has served real EPP traffic on an H100 OpenShift test cluster, switching between them in ~437 ms. No
pool exists until you create one: see the
[warm pool guide](../docs/guides/warm-pool/) (`deploy/warmpool.sh`). The proxy
image in `config/warmpool` is pinned by digest under the project's registry
namespace; read the note beside it before replacing it.

## Why this exists rather than using Fast Model Actuation

FMA solves a neighbouring problem and most of it is genuinely reusable — the
launcher here is FMA's, copied rather than rewritten. One thing is not reusable,
and it is the thing that decides whether a warm pool works:

**In FMA the launcher requests no GPU; the *requester* holds it.** When the
requester goes away the device allocation is released while the sleeping engine's
`cumem` handles still refer to memory on that device, which another tenant then
takes. Measured on an H100 OpenShift test cluster: two of three unbound sleepers **could not be woken at
all** —

```
pfw8b: HTTP 500  CUDA error: invalid argument   cumem_allocator.cpp:145
v6fdd: HTTP 500  CUDA error: out of memory      cumem_allocator.cpp:139
```

A warm copy that does not own its device is not slower, it is unreliable. So here
**the pool Pod holds the GPUs for as long as it holds models**, which is the one
structural change from FMA and the reason this is a separate component rather
than a fork.

The second reason is scope: FMA's Kubernetes half (dual-pods controller,
launcher-populator, `LauncherPopulationPolicy`, the requester/SPI/proxy path)
exists to pair transient requester Pods with launchers. A pool needs none of it:
the scaling manager already runs a control loop, knows the InferencePools, and decides what
should be warm.

## What is copied, and from where

Copied from `ev-shindin/llm-d-fast-model-actuation`, branch `feat/reuse-by-model`,
commit **`65ba31b`** (upstream `2c01cf8`, plus the port-conflict reclaim fix in
`aa072ef` and the follower-rank fix in `65ba31b`):

| file | origin | why it earns its place |
| --- | --- | --- |
| `supervisor/launcher.py` | `inference_server/launcher/launcher.py` | spawns and supervises vLLM engines inside one container. Proven: every 2-3 s wake measured on the test cluster came from an instance it created |
| `supervisor/gputranslator.py` | `inference_server/launcher/gputranslator.py` | GPU UUID ↔ index mapping via pynvml, with a mock mode for tests |
| `supervisor/tests/` | the same tree | the evidence the above works; kept so adaptation is not blind |

> **Vendored, not maintained here.** These files are a copy taken at fork commit
> `65ba31b`; edit them upstream and re-copy rather than in place.
>
> Which image runs depends on the pool. A SINGLE-POD pool runs upstream
> `launcher:v0.6.4`, pinned by digest in
> `config/warmpool/warmpool-deployment.yaml` -- a different build from this copy,
> and it does not need the follower fix because a group of one has no follower. A
> GROUP pool must be given `--launcher-image` naming a build that carries it:
> `ghcr.io/ev-shindin/fma-launcher:v0.6.4-headless`, whose `launcher.py` is
> byte-identical to this copy (verified by extracting it from the published
> image). Nothing automatic keeps the three in step; this copy fell a commit
> behind the image once already, which hid the very fix that makes group pools
> work from anyone reading it here.
>
> The tests under `supervisor/tests/` came with the source and are not wired into
> any make target or CI job.

## The split of responsibilities

The launcher has **no sleep or wake endpoint**, and that is correct rather than
missing: in FMA the *controller* calls vLLM's own HTTP API directly
(`inference-server.go` calls `/wake_up`, `/sleep` and `/is_sleeping` on the
instance). The scaling manager takes that role.

| responsibility | where |
| --- | --- |
| spawn / list / delete engines in a Pod | supervisor (this directory) |
| `/sleep`, `/wake_up`, `/is_sleeping` | **the scaling manager**, over HTTP to the engine's port |
| which models are warm, which wakes, which is evicted | **the scaling manager** — it is an allocation problem, which is what the scaling manager is for |
| joining a woken model to its InferencePool | **the scaling manager** — label membership; readiness comes from an ordinary probe against the proxy's `/readyz`, not a Pod readiness gate |

No policy lives in the Pod. That is deliberate: the cache policy is the part most
likely to change, and it must be changeable without touching the data plane.

## What had to change, and where it changed

All four are done. Note where: three of them turned out to be decisions the scaling manager
makes when CALLING the launcher, not modifications to the launcher itself, which
is why the copy here runs unmodified.

1. **The Pod holds the GPUs.** Done in the manifest. The pool Deployment requests `nvidia.com/gpu`, and
   the supervisor allocates the container's own devices among instances rather
   than being handed a device by a requester.
2. **Instance identity keyed by model**, not by a hash over GPU UUIDs. Done in
   the scaling manager (`pool.InstanceID`): the launcher takes the ID from its caller. The hash is
   correct *for FMA* — `CUDA_VISIBLE_DEVICES` is fixed at process start, so a
   sleeper is not portable between GPUs — but our reuse question is "is this model
   resident in this Pod", which the model name answers.
3. **Ports assigned from a local range**, not derived from an
   InferenceServerConfig. Done in the scaling manager (`pool.freePort`): the launcher takes the
   port from its caller too.
   FMA's ISC-derived port is what made two instances of one model collide, and the
   port-conflict fix in `aa072ef` is a workaround for it.
4. **Drop the launcher-notifier sidecar** — never copied, so nothing to do:
   it maintains the `dual-pods.llm-d.ai/sleeping` label for the dual-pods
   controller. The scaling manager reads instance state from the supervisor instead, which the
   measurements showed is the reliable source — the Pod label is per-Pod and flips
   for reasons unrelated to whether a given model is asleep.
