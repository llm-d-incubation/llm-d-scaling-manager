# Workloads that bring their own JIT cache

**Status:** proposal, 2026-11-16. Built (#214): the annotation, the
per-variable rule over the four variables on main, the report, the conflict
and its revert, the refusal, and `engine-cache-keys` / `-verify`; see
[An external cache manager](../reference/workload-preparation.md#an-external-cache-manager).
Not built: the bytecode offer for images before vLLM v0.32, and the
start-time decisions. Dropped: the five variables of the cache-variable PR
(#213). A GPU run measured four of them (not `XDG_CACHE_HOME`) and found that
`VLLM_CACHE_ROOT` already keeps DeepGEMM and the
compiled model, and that the rest saved no more start time than starts vary
(one model, one GPU); see
[What `VLLM_CACHE_ROOT` already covers](../reference/workload-preparation.md#what-vllm_cache_root-already-covers).
The variable lists below predate that result.

**What you do.** If another component owns your engine's compile caches:

1. Annotate the pod template with
   `scaling-manager.llmd.ai/engine-cache-owner: external`.
2. Run `make workload-patch`.
3. Read the report: it lists the caches it left to the owner and any it still
   patches.

Adding the annotation changes the pod template, so it **restarts every
replica**, and each restart on a large model is a cold start. Add it in a quiet
window or with your next planned rollout.

## The problem

Some clusters run a cache manager, usually a sidecar, that keeps the engine's
compiled caches in a shared store. On those workloads `make workload-patch`
reports "COMPILES FROM NOTHING", and `--apply-engine-cache` moves the caches
out of the manager's directory. The manager then shares copies without them,
and later pods start "warm" but compile anyway.

## What we rely on

Only this, from no particular implementation:

- **Restore before start.** Before the engine starts, the manager copies the
  caches for the pod's key from the store into a directory inside the pod.
- **Publish after serving.** Once the engine has served for a while, the
  manager copies that directory back to the store.
- **First copy wins.** The first copy stored for a key is the one later pods
  restore, until the manager replaces it.

The manager computes the key. We assume only that a new image starts a new key.

## What changes

### The annotation

| value | effect |
| --- | --- |
| `external` | caches the pod spec points somewhere are left to the owner (below) |
| `ours` | today's behaviour; the report says the caches are ours |
| absent | today's behaviour |
| anything else (`External`, `yes`) | treated as `ours`, with a warning naming the value |

A LeaderWorkerSet is decided per template. If only one template is annotated,
the report says which one gets which.

Without the annotation, if an `emptyDir` holding an engine cache is also
mounted by a container that mounts storage (a claim, hostPath, NFS, CSI or
generic ephemeral volume), the report suggests the annotation. The suggestion
never changes the patch: a weights downloader with a scratch `emptyDir` has the
same shape, and a guess must not silence a real warning.

### Per cache variable, with `external`

- **Set in the pod spec:** left alone. No variable, mount or volume of ours
  for it.
- **Not set in the pod spec:** we patch it as today, and the report names it,
  with the owner's directory as the alternative that would put it in the
  shared copy. A variable set only inside the image counts as not set: repeat
  it in the pod spec to leave it to the owner.
- **FlashInfer:** if the owner sets `FLASHINFER_WORKSPACE_DIR` and not
  `FLASHINFER_WORKSPACE_BASE`, we do not patch it, but the warning stays,
  addressed to the owner: FlashInfer reads only `FLASHINFER_WORKSPACE_BASE`.
- The drain and weights patches, including the
  [weights gate](weights-node-gate.md)'s node affinity and wait container,
  and the `prepull` check do not change.
- `--apply-engine-cache` applies only the variables we still patch. If there
  are none, it refuses, with a non-zero exit and the reason.
- `make engine-cache-keys` and `make engine-cache-verify` say the caches are
  managed elsewhere instead of reporting the key missing on every node.

### A workload patched before it was annotated

It carries our `engine-cache` volume next to the manager. The report flags this
as a conflict and says:

- remove our volume, its mounts, and the cache variables that point under
  `/engine-cache`;
- delete the copies the manager stored while our patch was applied. They lack
  the moved caches and are restored until the manager replaces them;
- if our volume is a shared ReadWriteMany claim: several pods compiling into
  one claim have lost files, and left a pod Running and never Ready.

### What the report shows

```
deployment/qwen-decode: engine caches owned externally (scaling-manager.llmd.ai/engine-cache-owner=external)
  store: claim jit-store, mounted read-write by cache-sync; see Trust
  TRITON_CACHE_DIR: not set in the pod spec; patched under /engine-cache
```

If the store is not visible in the pod spec (mounted inside the manager's
container, for example):

```
  store: not visible in the pod spec; check who can write it
```

The patch file carries one comment:

```yaml
# engine caches: owned externally; only TRITON_CACHE_DIR is ours (see the report)
```

### Writable files

Restored files must be writable by the engine's user. Where they are not,
FlashInfer fails instead of compiling. The report still offers the writable
guard, as text the owner pastes, including PR #146's check of the whole
FlashInfer tree.

## Trust

Compiled caches are code: Triton and FlashInfer libraries run as native code,
and torch.compile artifacts include Python the engine runs. With a shared
store, **one bad copy reaches every node that starts that key**, and stays
there until it is replaced. Nothing checks a copy between store and restore.

- Whoever can write the store, or stores the first copy for a key, runs code in
  every later pod of that key. The engine container writes the directory the
  manager publishes, so a compromised engine pod that publishes first poisons
  the key everywhere.
- Recommended: only a publisher the operator controls can write the store;
  serving pods mount it read-only. That needs a manager with a separate
  publisher or a restore-only mode. Where every pod publishes, every pod can
  write, and the report says so.
- The engine container should never mount the store itself. The report warns
  if it does.
- Pin images by digest. Unpinned, you cannot tie the cached code to the image
  you checked. If the manager keys on the tag, a moved tag also restores old
  kernels into a new engine.

## Python bytecode: images before vLLM v0.32 only

From v0.32 on, vLLM images ship their bytecode and our guard unsets
`PYTHONPYCACHEPREFIX`, so there is nothing to offer. For older images the report
offers, without applying, `PYTHONPYCACHEPREFIX=<owner dir>/pycache/<image key>`,
so bytecode rides in the shared copy (about 11 s per start on vLLM 0.26). It
adds to the trust risk: a `.pyc` is checked only against its source's time
stamp and size.

## Upgrade note

For v0.12.0 in `docs/reference/upgrade-and-uninstall.md`:

- New: annotate `scaling-manager.llmd.ai/engine-cache-owner: external` when
  another component owns the compile caches; see
  `docs/reference/workload-preparation.md`.
- Separately: the vLLM v0.34 bump changes every cache key, so expect one cold
  start per key, with or without a cache manager.

## Not doing

- Reading the manager's logs, files or store.
- Changing the manager's pod spec, or emitting its manifests.
- Letting the suggestion change the patch.
- Any change to `make prepull` or `make weights`, or to the 3600 s start cap.
- Shipping a cache manager as a backend of `make engine-cache`. Ours needs no
  shared store; a manager fits where nodes are short-lived, or where caches
  must be shared across nodes. The guide names both.

## For maintainers

### Where the cache variables live today

On main, `deploy/lib/scaledobject.sh` spells them out in five places:

- the trim, `so_workload_patch_trim`;
- the `half` jq def in `so_workload_lws_json_patch`;
- the emitted env block;
- the writable guard loop;
- the classifier (on `VLLM_CACHE_ROOT`).

The trim and `half` list `VLLM_CACHE_ROOT`, `FLASHINFER_WORKSPACE_DIR`,
`TRITON_CACHE_DIR` and `PYTHONPYCACHEPREFIX`. PR #146 replaces
`FLASHINFER_WORKSPACE_DIR` with `FLASHINFER_WORKSPACE_BASE`. The cache PR makes
these one list and adds `DG_JIT_CACHE_DIR`, `TRTLLM_DG_CACHE_DIR`,
`TORCHINDUCTOR_CACHE_DIR`, `CUDA_CACHE_PATH` and `XDG_CACHE_HOME` to it. The
rules above then apply to the whole list.

### Restructuring the per-variable rule needs

After PR #146 the patch is written per group, not per variable: one block
always sets `VLLM_CACHE_ROOT`, `FLASHINFER_WORKSPACE_BASE` and
`TRITON_CACHE_DIR` together, and the trim drops `FLASHINFER_WORKSPACE_BASE`
from a container without `VLLM_CACHE_ROOT`. The build must:

- emit each variable on its own;
- drop that trim rule, so `FLASHINFER_WORKSPACE_BASE` can be emitted alone;
- never replace, on a LeaderWorkerSet, an env entry the owner set.

### vLLM v0.34 (unverified, check before building)

- `vllm download-kernels` (said to arrive in v0.32) fetches FlashInfer's
  precompiled kernels under `FLASHINFER_WORKSPACE_BASE`; with an owner they
  ride in the shared copy. The preparer mentions it, report only.
- The shared Triton compile pool (said to arrive in v0.33) must inherit
  `TRITON_CACHE_DIR`, or its workers compile outside the owner's directory.
- vLLM #65652 is said to change every cache key once at the bump.

### Start time

- `replicaStartSeconds` is only a starting value: once three Pods are
  measured, the measurement wins.
- The first start of every key is cold. **Decision:** recommend a cold
  `replicaStartSeconds` for the first start of each key, or none.
- **Decision:** the 3600 s cap stays. A pod that waits on another before
  compiling cold can exceed it, but only when the cold compile is very long,
  and raising the cap would let a brief readiness drop count as a start.
- Later, if the manager publishes a per-pod warm/cold metric, keep two
  estimates per variant: the next replica is warm if the last Pod of this
  variant and image was warm. One series per pod; no read of the store.

PR #146 asks whether its node directory should be mode 1777 or 1770. That does
not apply here: external mode creates no node directory of ours.

### Order

After PR #146 merges, one cache PR builds the single variable list with the
five new variables, the annotation, the restructuring above, and the report.
Start time is separate.

### Tests

`hack/check-workload-gaps.sh` fixtures. Each asserts a report line or patch
that does not exist yet, so each fails on the parent commit:

- `external`, every variable covered: no engine-cache patch, no "COMPILES FROM
  NOTHING", the "owned externally" line, guard text still offered.
- `external`, `TRITON_CACHE_DIR` uncovered: ours for it, and its report
  line.
- `external`, owner sets `VLLM_CACHE_ROOT` but not `FLASHINFER_WORKSPACE_BASE`:
  `FLASHINFER_WORKSPACE_BASE` emitted alone.
- `external`, owner sets `FLASHINFER_WORKSPACE_BASE` but not `VLLM_CACHE_ROOT`:
  ours for `VLLM_CACHE_ROOT`, the owner's `FLASHINFER_WORKSPACE_BASE` untouched,
  on a Deployment and a LeaderWorkerSet.
- `external`, owner sets `FLASHINFER_WORKSPACE_DIR` only: no FlashInfer patch,
  the warning addressed to the owner.
- `external`, cache variables set only in the image: ours emitted, each named
  as set only in the image.
- `ours`: today's patch, plus the "caches are ours" line.
- An unknown value: today's patch, plus the warning naming the value.
- No annotation, manager shape: today's patch, plus the suggestion.
- No annotation, downloader shape: today's warning and patch, plus the
  suggestion.
- LeaderWorkerSet annotated on one template: the per-template line.
- Patched before annotated: the conflict and the revert.
- `--apply-engine-cache` with every variable covered: refused, non-zero exit.

And a start-learner unit test with warm and cold samples from one variant.
