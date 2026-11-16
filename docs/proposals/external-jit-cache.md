# Workloads that bring their own JIT cache

Status: draft, not built.

**The task.** Some clusters already run a sidecar next to each model server that
restores the engine's compiled caches from a shared store before the engine
starts, and publishes them back once it has served. On those workloads,
`make workload-patch` should prepare the image and the weights, leave the
compile caches to the sidecar, and say so. Today it does the opposite: it
reports the workload as compiling from nothing and writes a patch that, if
applied, takes the caches away from the sidecar.

This page says how to recognise such a workload, what changes in the patch
report, what changes in the start-time estimate, and what stays as it is.

## What the sidecar does

Described from one implementation; the rules below key on its shape, not its
name.

- **Where the caches live.** The engine container points every compile cache
  (`VLLM_CACHE_ROOT`, `TRITON_CACHE_DIR`, `TORCHINDUCTOR_CACHE_DIR`,
  `FLASHINFER_WORKSPACE_BASE`, `CUDA_CACHE_PATH`, `XDG_CACHE_HOME`) at an
  `emptyDir` that it shares with the sidecar. Nothing compiles into shared
  storage directly.
- **Restore.** The sidecar runs as a native sidecar (an init container with
  `restartPolicy: Always`). Before the engine starts it looks in a shared
  store (a ReadWriteMany claim, or object storage mounted as a filesystem) for
  an archive under a key, unpacks it into the `emptyDir`, and marks the pod
  warm or cold. The key is the engine build plus GPU type, plus model and
  parallel sizes in one mode, and the group size in the other.
- **Per slot and role.** Archives are kept per worker slot of a
  LeaderWorkerSet group and per role (prefill, decode), so ranks and roles
  never overwrite each other.
- **Whole groups.** In group mode the leader decides once for the group: warm
  only if every slot of its role has an archive, otherwise the whole group
  starts cold. No group starts half warm.
- **Publish.** After the engine is healthy and has served real requests (by
  default 10 minutes and 100 requests), the pod archives its cache to the store
  if its slot has none yet. The first upload per slot wins.

What it does not do: hold the image on the node, or put the weights on the
node's disk. Those stay ours.

## Where the two collide today

| | ours today | with the sidecar | effect |
| --- | --- | --- | --- |
| Patch report | an engine whose `VLLM_CACHE_ROOT` sits on an `emptyDir` is "COMPILES FROM NOTHING" | that `emptyDir` is the sidecar's restore target | a false alarm on every such workload |
| Emitted patch | sets `VLLM_CACHE_ROOT`, `FLASHINFER_WORKSPACE_DIR`, `TRITON_CACHE_DIR` under `/engine-cache` | the same names already point at the sidecar's directory | applied, it moves the torch.compile and Triton caches off the sidecar's directory: the sidecar then publishes archives without them, and later pods are marked warm while they compile |
| LeaderWorkerSet apply | replaces an env entry of the same name in place | the sidecar's entries are those entries | the same, on every group |
| FlashInfer variable | `FLASHINFER_WORKSPACE_DIR`, which FlashInfer does not read (fixed separately: `FLASHINFER_WORKSPACE_BASE`) | `FLASHINFER_WORKSPACE_BASE` | once fixed, both set the same name to different places |
| `engine-cache keys` / `verify` | read the key from the engine log and look for it on the node directory | the key is in the `emptyDir`, never on the node | every node reported missing the key |
| Start-time estimate | one estimate per variant, learned from Pod Ready less creation | starts are two populations: warm (archive found) and cold (first group per key, or after an image change) | a cold start after warm ones is refused as an outlier until three Pods agree; a warm start after cold ones pulls the estimate down slowly |
| Longest credible start | 3600 s; the share pacing bound is 30 min | a group worker can wait up to 30 min for its leader's decision, then compile cold | a real start past an hour is dropped, not learned |

The node-directory engine cache and the sidecar never share a directory, so
the Triton autotune race we saw on a shared ReadWriteMany cache does not
occur between them. It can occur if both are configured and our half points
at a shared claim.

## What changes

### 1. Recognise the workload

A workload has an external cache manager when its pod template has either:

- the annotation `llm-d.ai/engine-cache: external` (explicit; wins), or
- an `emptyDir` that holds the engine's `VLLM_CACHE_ROOT` and is also mounted
  by another container of the pod (init or regular) that mounts durable
  storage (a claim, hostPath or NFS). That other container is the cache
  manager.

The second rule needs no name, image or variable from any one sidecar. A
workload that matches it without the annotation gets one line asking the owner
to add it, so the next reader does not have to infer it.

### 2. Report, do not patch, the compile caches

For such a workload `make workload-patch` and the tools image:

- emit no engine-cache half: no `VLLM_CACHE_ROOT`, `FLASHINFER_WORKSPACE_BASE`,
  `TRITON_CACHE_DIR`, no `/engine-cache` volume, and no guard;
- still report and emit the drain half and the weights half, and still check
  `prepull` status for its image;
- print what it found instead of "COMPILES FROM NOTHING": the manager
  container, the store it mounts, and that the compile caches are left to it;
- refuse `--apply-engine-cache` for it, with that reason.

`engine-cache keys` and `verify` say the pod's caches are managed elsewhere
rather than reporting the key missing on every node.

**Python bytecode.** The sidecar does not set `PYTHONPYCACHEPREFIX` (~11 s of
every start on vLLM 0.26, nothing on images that ship bytecode). Offered, not
applied: point it under the sidecar's directory,
`<cache dir>/pycache/<image key>`, so the bytecode rides in the same archive
and needs no volume of ours. The existing guard still unsets it on an image
that ships its own.

### 3. Learn warm and cold starts apart

A single estimate cannot describe a variant whose starts are 3 minutes warm
and 20 cold. Two steps:

1. **Now, no new signal.** Document that a workload with an external cache
   should set `replicaStartSeconds` on its ScaledObject to its warm start, and
   that the first group after an image change will be refused as an outlier
   until three Pods confirm it. Raise the longest credible start to cover a
   group worker's decision wait plus a cold compile, or exclude the wait
   explicitly.
2. **When the sidecar exposes its decision.** If the decision is published as
   a metric (for example a gauge per pod with `decision="warm|cold|mixed"`),
   the start learner keeps two estimates per variant and sizes with the one
   that applies: cold for a key whose store has no complete set, warm
   otherwise. Without that metric the controller does not guess from pod logs
   or the store.

### 4. Engine-cache backend: keep ours

We do not ship the sidecar as a backend of `make engine-cache`. Ours needs no
change to the engine container and no shared store at run time; theirs needs
a shared store and a cache key the operator maintains, and is the better fit
where nodes are not long-lived or a group must start all warm or all cold.
The guide names both and says which situation each is for.

## Worth offering upstream

To be raised with the sidecar's maintainers, not built here:

- `PYTHONPYCACHEPREFIX` keyed by image, with the guard that unsets it when the
  image ships its bytecode.
- A writable-directory guard: an unwritable cache variable is unset, so a
  storage problem costs one compile, not the replica.
- In the single-pod entrypoint, treat `tar` exit status 1 (a file changed
  while read) as success, as the group entrypoint already does; and keep one
  archive per slot there too, so the store does not grow with every pod.
- Expose the warm/cold/mixed decision and the restore seconds as a metric, so
  an autoscaler can learn the two start times apart.

## Not doing

- Reading the sidecar's logs, files or store from the controller.
- Changing the sidecar's pod spec, or emitting its manifests.
- Any change to `make prepull` or `make weights`.

## Tests

- `hack/check-workload-gaps.sh` fixtures: a Deployment and a LeaderWorkerSet
  with an external cache manager (by annotation, and by the shared-`emptyDir`
  rule) emit no engine-cache half and no "COMPILES FROM NOTHING"; the same
  pod without the manager container still does. Run against the parent commit
  first: the fixtures must fail there.
- A start-learner unit test with warm and cold samples from one variant.

## Open questions

- FlashInfer reads only `FLASHINFER_WORKSPACE_BASE` (`flashinfer/jit/env.py`);
  `FLASHINFER_WORKSPACE_DIR`, which our engine-cache patch sets, is a Python
  constant. Our engine cache has never held the FlashInfer kernels. That is
  fixed in its own change, ahead of this one.
- Should our engine cache also redirect `CUDA_CACHE_PATH` and
  `TORCHINDUCTOR_CACHE_DIR`, as the sidecar does? Measure before adding.
