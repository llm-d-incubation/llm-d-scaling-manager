# Installing and creating a pool

[← Warm pool guide](README.md)

## Prerequisites

- [ ] A model already serving under the scaling manager ([Install the scaling
  manager in a namespace](../install-in-namespace/)).
- [ ] Free accelerators in the pool's namespace, of the same GPU model as the
  workloads it will warm.
- [ ] **A shared model cache: the SAME claim the model servers read.** Every
  pool Pod mounts a claim named `model-pvc` unless `--cache-claim NAME` says
  otherwise. Not optional ([what goes wrong](#picking-the-cache-claim)). None
  yet? Create one in the pool's namespace, with an RWX class:

  ```bash
  # Create an RWX model cache; with no size or class it lists the RWX classes this cluster serves
  make model-cache NAMESPACE=<namespace> LLMSM_MODEL_PVC_SIZE=<size> LLMSM_MODEL_PVC_CLASS=<rwx-class>
  ```

- [ ] You can create Roles and RoleBindings in the pool's namespace, and patch
  Pods there. If you cannot, `create` stops before creating anything and prints
  the command for an admin ([why](#the-pod-patch-grant)).
- [ ] **Two container images, from two owners** (below), pullable from the pool
  nodes.

### The images a pool Pod runs

Each pool Pod runs two containers, and only one is built here:

| container | image | owner |
| --- | --- | --- |
| `inference-server` (pool of Pods) | `ghcr.io/llm-d-incubation/llm-d-fast-model-actuation/launcher` | Fast Model Actuation |
| `inference-server` (pool of **groups**) | `ghcr.io/ev-shindin/fma-launcher:v0.6.4-headless` — `--launcher-image` is required, and `create` refuses without it | **our fork**, [ev-shindin/llm-d-fast-model-actuation](https://github.com/ev-shindin/llm-d-fast-model-actuation) — see [pools of groups](groups.md#a-group-needs-a-patched-supervisor-image) |
| `proxy` | the image `config/warmpool` pins, or `--proxy-image` | this repo (`make docker-build-warmpool-proxy`) |

A pool node must pull from `ghcr.io/llm-d-incubation`, or for a pool of groups
from `ghcr.io/ev-shindin`. On an air-gapped or mirrored registry, mirror
whichever applies. It is easy to miss: neither is built by this repo. A pool of
groups needs the fork, because the stock launcher cannot start a multi-node
follower at all. Why the stock launcher is required: see
[Details](#why-the-launcher-image-is-required).

## Turning it on during installation

The installer can create pools in the same pass that creates ScaledObjects. It
never does so by default: a pool holds its accelerators from the moment it
exists, so it acts only on an explicit yes.

```bash
# Discover what runs, write a plan and open it in $EDITOR
LLMSM_DEFAULT_SO=edit deploy/install.sh
```

Near the bottom of the plan is a `warmPools:` section: one suggestion per group
of models that could share a pool, every one `apply: no`:

```yaml
warmPools:
  - namespace: my-models
    name: h100
    accelerator: NVIDIA-H100-80GB-HBM3
    gpus: 1
    models: 4
    modelSize: 8B
    replicas: 2
    max: 6
    reserve: 1
    apply: no        # <- change to yes for the pools you want
```

Change the ones you want to `yes` and save. Pools are created **after** the
ScaledObjects, because a pool nothing can borrow from holds accelerators for
nothing. `LLMSM_DEFAULT_SO=plan` writes the plan and stops; apply it later with
`LLMSM_DEFAULT_SO_PLAN=<file>`.

The plan creates **bridges**: it carries no `type`/`maxHold`. For a retained
pool use the CLI (`deploy/warmpool.sh create --type retained ...`), or patch the
plan's pool afterwards; until then it is a bridge and can lend and reclaim.

Three values the plan cannot know:

| value | where it comes from | if it is missing |
| --- | --- | --- |
| `WARMPOOL_PROXY_IMG` | optional — defaults to the published image `config/warmpool` pins. Build your own with `make docker-build-warmpool-proxy docker-push-warmpool-proxy` | nothing: the default is used, and the installer says which |
| `MONITORING_NAMESPACE` | the install already has it — where it put the monitoring stack | the pool is created **unscraped**: it works, and every model it lends to reads as having less demand than it has |
| `WARMPOOL_RUNTIME_CLASS` | usually nothing — `create` adopts whatever the namespace's own GPU workloads use | nothing: most clusters need no RuntimeClass at all |

## Creating a pool by hand

A pool is a Deployment and its ScaledObject, which only work together, so one
command makes both:

```bash
# Create the pool's Deployment, ScaledObject and NetworkPolicy
deploy/warmpool.sh create -n <namespace> --name <pool>   --accelerator NVIDIA-H100-80GB-HBM3 --gpus 1   --models 4 --model-size 8B   --scaling-manager-namespace <where the scaling manager runs>
```

- `--models` and `--model-size` set the memory limit, which **is** the warm-set
  budget ([sizing](sizing.md#how-many-models-fit-in-one-pod)).
- `--dry-run` prints the manifests without applying them. `delete` removes both
  objects together; removing only one leaves accelerators nobody can borrow, or a
  trigger pointing at nothing.
- Always pass `--accelerator`. Without it the Pods may land on any GPU node, and
  the scaling manager declines every model whose accelerator it can prove
  differs.
- `create` also applies the ingress boundary: `:8000` from this namespace, and
  `:8001`, `:8002` and the engine range from the scaling manager's namespace
  only. `--no-network-policy` is for clusters that manage policy centrally, never
  for convenience: without a policy, `:8001` accepts caller-supplied argv and
  environment from anything that can reach the Pod IP, in a container that
  mounts the shared model cache read-write.

If the pool later reports itself **empty** while holding accelerators, suspect
the policy first. One naming the wrong scaling-manager namespace denies the
supervisor read, which looks exactly like a pool that is too small. Look for
`warm pool Pod could not be read` in the controller log.

The rest of this section is the manual path, for editing the manifests yourself.

### 1. Nothing to point at the pool

There is no step here, by design. A namespace-scoped controller warms the
namespace it watches; a cluster-scoped one acts on pools wherever a ScaledObject
trigger declares them. Neither needs telling, or a restart, when a pool appears.

### 2. Edit the four things the manifests cannot know

`config/warmpool` is a template, not a working install. Each of these is
cluster-specific, and three fail in a way that looks like something else:

| in | what to set | if you skip it |
| --- | --- | --- |
| `warmpool-networkpolicy.yaml` | the `>>> EDIT THIS <<<` `namespaceSelector` — the namespace the scaling manager runs in | every read fails; the pool reports itself **empty** while holding accelerators |
| `warmpool-scaledobject.yaml` | the two `>>> EDIT THIS <<<`, in `scalerAddress` and the `authenticationRef` name: the scaling manager's namespace. On a plaintext install, use the `.svc.cluster.local` address and delete the `authenticationRef` | applies cleanly, KEDA creates the HPA, the address never resolves, and the scaling manager never learns the pool exists |
| `warmpool-deployment.yaml` | the proxy `image` — build your own with `make docker-build-warmpool-proxy` | `ImagePullBackOff`; the shipped digest is a personal registry namespace |
| `warmpool-deployment.yaml` | `runtimeClassName`, and the `claimName` of your model cache | admission fails outright; or the second replica sits Pending forever if the claim is not **ReadWriteMany** |

The `scalerAddress` one is the quiet one: the placeholder is a legal string, so
nothing rejects it and nothing warns.

### 3. Deploy the pool

```bash
# Apply the edited template
kubectl apply -k config/warmpool -n <namespace>
```

`deploy/warmpool.sh create` renders the same two objects from flags, which is
why the edits above do not arise on that path.

## Adding and removing a pool later

Pools are ordinary day-2 objects. The controller discovers them from the
ScaledObjects that declare them on its next pass, so nothing needs a restart.

**What exists now**

```bash
# What this namespace wants, from its triggers, and any pool workload nothing declares
deploy/warmpool.sh plan -n <namespace>
# The pool objects that exist
kubectl get deploy,scaledobject,networkpolicy,podmonitor -n <namespace> -l app.kubernetes.io/component=warm-pool
```

**Add one**

```bash
# Create a scraped pool
deploy/warmpool.sh create -n <namespace> --name <pool>   --accelerator NVIDIA-H100-80GB-HBM3 --gpus 1   --models 4 --model-size 8B   --scaling-manager-namespace <where the scaling manager runs>   --monitoring-namespace <where Prometheus runs>
```

Then point models at it, in each one's ScaledObject trigger metadata:

```yaml
warmPool: <pool>       # which pool this variant may borrow from
warmPoolCopies: "1"    # optional: how many copies of THIS model to keep warm
```

A model with no `warmPool` uses the namespace's only pool, if there is exactly
one. Adding or removing that line is the whole of joining or leaving a pool; the
next reconcile acts on it.

**Add scraping to a pool that already exists**

```bash
# Add the PodMonitor and admit the scraper to the serving port
deploy/warmpool.sh monitor -n <namespace> --name <pool> --monitoring-namespace <where Prometheus runs>
```

`create` does this already. `monitor` is for older pools, or ones applied from
`config/warmpool`, which ships no PodMonitor because a static manifest cannot
know where Prometheus runs. It is safe to re-run: an already-admitted namespace
is left alone.

**Remove one**

```bash
# Remove the ScaledObject, workload, NetworkPolicy and PodMonitor together
deploy/warmpool.sh delete -n <namespace> --name <pool>
```

It deletes the ScaledObject first, so the scaling manager stops lending Pods
about to disappear, then the workload, then the NetworkPolicy and PodMonitor.
`--dry-run` shows what it would remove.

Then remove the `warmPool:` line from any model still naming it, or the scaling
manager reports a variant pointed at a pool that does not exist. The models keep
serving; they lose the bridge, so their next scale-up pays a full cold start.

> **Do not delete only the ScaledObject.** It *declares* the pool. What is left
> is a Deployment holding accelerators that the scaling manager reports as
> undeclared and never uses again. To pin a pool's size, set `minReplicaCount`
> equal to `maxReplicaCount` and keep the ScaledObject.

**Resize one**

Re-run `create` with different `--replicas`/`--max`/`--reserve`/`--type`/
`--max-hold`. It is an `apply`, so the objects update in place. `--type` and
`--max-hold` apply live on the next reconcile, with no restart, so a bridge can
become retained, and back, on a running pool.

> **Pass `--type` again on every re-run of a retained pool.** Omitting it emits
> nothing, and `apply` prunes what the previous apply wrote, so a re-run to
> change `--replicas` silently turns a retained pool back into a bridge, which
> then reclaims its Pod on the hold timeout.

Changing `--models`/`--model-size` changes the Pod's memory limit, which **rolls
the pool** and reloads every resident model. Decide the warm-set budget before
you fill it.

Changing the pool's KIND (a single-Pod pool to a group pool, or back) is
refused, because `kubectl apply` would leave the old workload running and
holding its GPUs where nothing looks for it. Delete the pool first.

## The pool is its ScaledObject

A warm pool is declared by a KEDA trigger, like everything else the scaling
manager knows about. `warmPoolName` makes it a pool; the Deployment beside it
only supplies the Pods.

```yaml
spec:
  minReplicaCount: 2
  maxReplicaCount: 6              # must EXCEED the reserve
  triggers:
    - type: external-push
      metadata:
        scalerAddress: scaling-manager-external-scaler.<scaling-manager-namespace>.svc:9090
        warmPoolName: default     # must match the Deployment's llm-d.ai/warm-pool label
        warmPoolSleepMinSize: "1" # the reserve
      authenticationRef:      # TLS, the default. Plaintext install: ...svc.cluster.local:9090 and no authenticationRef
        name: scaling-manager-external-scaler-<scaling-manager-namespace>
        kind: ClusterTriggerAuthentication
```

**Deleting this ScaledObject deletes the pool**, not just its elasticity: the
Deployment keeps holding accelerators, reported as undeclared. For a fixed size,
set `minReplicaCount` and `maxReplicaCount` equal. To remove a pool, remove both
objects with `deploy/warmpool.sh delete -n <namespace> --name <pool>`. It
deletes the ScaledObject, then the workload, then the NetworkPolicy `create`
made, in that order, so live Pods are never left unprotected. It is safe to
repeat. Models still naming that pool are then warmed by nothing; `plan` lists
them.

## Details

### The pod patch grant

- The scaling manager must `patch` pool Pods to lend them. Its ClusterRole
  only reads Pods.
- `deploy/warmpool.sh create` grants `patch` in the pool's namespace alone
  (Role `scaling-manager-pod-writer`), before it creates the pool. Kubernetes lets you grant
  only what you hold, so you need `patch` on Pods there too.
- A namespace-scoped scaling manager already has `patch` in its own Role.
  `create` sees that only if you may impersonate its ServiceAccount
  (`kubectl auth can-i --as`). If you may not, it makes the grant anyway, which
  does no harm.
- Without the grant, the scaling manager refuses to start the pool. It will
  not hold accelerators to warm models it could never lend.
- `deploy/warmpool.sh delete` keeps the grant, because other pools and the
  utilization-share optimizer may use it. It warns, and prints the commands
  that remove it. The uninstall removes it.

### Picking the cache claim

`create` warns when the claim you name is not one the namespace's GPU workloads
mount. That catches the common mistake: where the models mount `model-pvc` *at
path* `/model-cache`, a `model-cache` claim holding the Hugging Face cache
usually exists too. Pick that one and the Pod mounts something real, at the
right path, containing no models. The engine then starts on a `--model` path
that is not in the Pod, never answers, and the controller waits out its full
admission window before reporting only that the port did not respond.

`create` cannot check that the claim EXISTS, and says nothing when no GPU
workload is deployed yet. Without one, `create` still reports SUCCESS, and the
Pods sit `Pending` on

```
0/3 nodes are available: persistentvolumeclaim "model-pvc" not found
```

while the pool reports itself empty.

### Why the launcher image is required

The stock launcher image is **not** ours and is not optional. It is a full vLLM
runtime (vLLM, torch and CUDA) with FMA's launcher on top, and the pool needs
both halves:

- **vLLM has to be in the Pod.** A warm copy is an engine already loaded on this
  Pod's GPU.
- **The launcher is the API the scaling manager drives.** It serves
  `/v2/vllm/instances` on :8001, which is how the controller creates a model in a
  Pod, lists what is resident, and removes one. Without it a pool Pod holds
  accelerators and answers nothing, and the controller reports the pool EMPTY.
  That happened when a hand-written manifest lost this container.

You could substitute your own: the launcher's source is vendored at
`warmpool/supervisor/`, and any image offering the same instance API over vLLM
would do. FMA's is used because it is the one proven on this cluster. This repo
does not build that image, so the vendored source is for reading, not shipping;
if FMA publishes a newer launcher, the copy here may differ from the one running.

This is the live FMA dependency the [FMA post-mortem](../../proposals/fma-post-mortem.md)
warns about: FMA was dropped as an *actuation strategy*, but its launcher still
supervises every warm engine.
