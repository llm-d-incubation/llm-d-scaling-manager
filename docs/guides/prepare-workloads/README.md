# Prepare model servers to scale

## Overview

A scale-up helps only once the new replica is Ready. This guide takes the
seconds out of that start, with one configuration, from the release alone: no
checkout, no `make`.

The recommended configuration puts three things on each GPU node's own disk:

| On the node | Why | Measured |
| --- | --- | --- |
| **The engine image**, held by a small pod | a node without it pulls 10-20 GB before the container starts | at least 67-80 s of pull, Qwen3-32B on vLLM v0.26.0 |
| **The weights**, on the node's local NVMe | a shared volume removes the download, not the read, and replicas that start together share its bandwidth | a 700 GB model: 1403 s from the shared volume on two nodes, about 104 s from local NVMe |
| **The engine's compile caches** (`VLLM_CACHE_ROOT`, `FLASHINFER_WORKSPACE_DIR`, `TRITON_CACHE_DIR`) and Python bytecode (`PYTHONPYCACHEPREFIX`) | otherwise every replica compiles from nothing | about 11 s of compile (14 s cold, 3 s cached), and on vLLM 0.26 about 11 s of bytecode per start (0.32 ships its own bytecode) |

Together, on Qwen3-32B and one H200: 102.6 s to Ready unprepared, 48.3 s
prepared, plus the pull a new node would have paid. Every number and how it was
measured: [Replica start time](../../reference/start-time.md).

It costs disk on every GPU node: the model's size, plus a few GB of caches. A
cluster admin runs step 3, because it creates PersistentVolumes.

Want a different setup, such as weights on a shared volume or no node
directory at all? See [Choosing differently](#choosing-differently).

## Prerequisites

- [ ] **The scaling manager installed**: [in a namespace](../install-in-namespace/)
  or [for the whole cluster](../install-cluster-wide/).
- [ ] **A directory on each GPU node's local NVMe**, the same path on every
  node, for example `/mnt/local/weights/<namespace>`. Not the node's root disk,
  not a network mount. On OpenShift it must be under `/var` and prepared first;
  see [Weights on the node's disk](../../reference/workload-preparation.md#weights-on-the-nodes-disk).
- [ ] **docker** (or podman), on Linux or macOS; on Windows, from WSL or Git
  Bash. The kubeconfig's credentials must be in the file (a token or a
  certificate): a login plugin (`exec:`) does not run inside the container; see
  [Without the container](#without-the-container). No container runtime at all?
  The same section.
- [ ] **A cluster admin** for step 3, and to read nodes for the `status`
  commands.

The tools image carries the release's scripts, with kubectl, jq and yq. Give
it a kubeconfig of the current context only (`--minify`), with its
certificates inlined (`--flatten`), rather than your whole `~/.kube/config`,
and define a short function for it:

<!-- guide:prerequisites.tools start -->
```bash
# The tools image of the release you installed. It gets a kubeconfig of the current context only, read-only; files land in this directory.
V=<release, for example v0.11.0>
export NAMESPACE=<your model namespace>
TOOLS=ghcr.io/llm-d-incubation/llm-d-scaling-manager/tools:$V
kubectl config view --minify --flatten > smt.kubeconfig && chmod 600 smt.kubeconfig
touch smt.env   # LLMSM_* settings for the tools, one NAME=value per line
smt() {
  docker run --rm --user "$(id -u):$(id -g)" --env-file "$PWD/smt.env" \
    -v "$PWD/smt.kubeconfig:/kube/config:ro" -v "$PWD:/work" "$TOOLS" "$@"
}
smt version
```
<!-- guide:prerequisites.tools end -->

Optionally, check the image was built by this repository's release workflow
(cosign v3). `cosign verify` prints the digest it checked; to run exactly that
image, set `TOOLS=ghcr.io/llm-d-incubation/llm-d-scaling-manager/tools@sha256:<digest>`.

<!-- guide:prerequisites.verify start -->
```bash
# Optional. Check the image was built and signed by this repository's release workflow.
cosign verify "$TOOLS" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity "https://github.com/llm-d-incubation/llm-d-scaling-manager/.github/workflows/ci-release.yaml@refs/tags/$V"
```
<!-- guide:prerequisites.verify end -->

### If it fails

- **`permission denied` on `/kube/config` or `/work`**: rootless podman needs
  `--userns=keep-id` in place of `--user ...`; rootless docker needs no
  `--user` at all. On an SELinux host (Fedora, RHEL), add `,z` to each volume:
  `-v "$PWD/smt.kubeconfig:/kube/config:ro,z" -v "$PWD:/work:z"`.
- **`connection refused` to `127.0.0.1` or `localhost`** (kind, a port-forward,
  a tunnel): inside the container that is the container itself. On Linux, add
  `--network host` to the `docker run`.
- **A login plugin** (`exec:` in the kubeconfig, for example on EKS or GKE), or
  a credential the flattened file cannot carry: run the scripts outside the
  container, [below](#without-the-container).
- **Mounting `~/.kube/config` itself** fails when it names certificate files
  by path (they are not in the container), or when `KUBECONFIG` lists several
  files: the flattened `smt.kubeconfig` avoids both. Make it again after
  `kubectl config use-context`, or after the credentials change.

## Steps

### 1. See what the model servers need

<!-- guide:deploy.report start -->
```bash
# What the model servers still need, per workload. Changes nothing in the cluster.
smt workload-patch -n $NAMESPACE
```
<!-- guide:deploy.report end -->

It names each model server that re-downloads its weights, compiles from
nothing, or drops requests on scale-down. Nothing is written yet if nothing is
missing. A controller in another namespace (a cluster-wide install runs in
`scaling-manager-system`)? Add `--controller-namespace <that namespace>` to
`workload-patch` and to `scaledobjects-plan`/`scaledobjects-apply`.

### 2. Hold the engine image on the GPU nodes

<!-- guide:deploy.image start -->
```bash
# The engine image, exactly as the model server's pod spec names it, held on every GPU node.
export ENGINE_IMAGE=<the image your model server runs>
smt prepull apply -n $NAMESPACE --image $ENGINE_IMAGE
```
<!-- guide:deploy.image end -->

One DaemonSet per image runs the image asleep on every node with a known GPU
label. It requests no GPU and 16 MiB of memory.

**Not every GPU node?** Each `apply` in this guide takes
`--node-selector <key>=<value>[,<key>=<value>]`: set it to what your model
servers select on, the same on every `apply`. On a cluster with GPUs of more
than one vendor it is required. More:
[Holding the image on the nodes](../../reference/workload-preparation.md#holding-the-image-on-the-nodes).

### 3. Cluster admin: weights and caches on the node's disk

<!-- guide:deploy.storage start -->
```bash
# Cluster admin (it creates PersistentVolumes). Weights and engine caches on each GPU node's local disk.
export NODE_DIR=/mnt/local/weights/$NAMESPACE   # a directory on the node's local NVMe
smt weights apply -n $NAMESPACE --model <hugging face id> --path $NODE_DIR --image $ENGINE_IMAGE
smt engine-cache apply -n $NAMESPACE --path $NODE_DIR --image $ENGINE_IMAGE
```
<!-- guide:deploy.storage end -->

`weights apply` prints the claim it made. Point the model at it: in llm-d's
modelservice values, `modelArtifacts.uri: pvc://<claim>/models/<hugging face id>`.
A gated model needs `--hf-token-secret <secret>`.

`engine-cache apply` makes the claim `engine-cache`. The patch in step 4
mounts it.

Then wait for every node:

<!-- guide:deploy.status start -->
```bash
# Per GPU node. Wait until every usable node reads present / prepared.
smt prepull status -n $NAMESPACE
smt weights status -n $NAMESPACE
smt engine-cache status -n $NAMESPACE
```
<!-- guide:deploy.status end -->

A cordoned node, or one with a taint the DaemonSet does not tolerate, is listed
as `excluded` and not counted.

### 4. Write the patch, and put it in the chart

<!-- guide:deploy.patch start -->
```bash
# Writes scaling-manager-workload-patch.yaml here. Copy it into your model server's chart values.
smt workload-patch -n $NAMESPACE
```
<!-- guide:deploy.patch end -->

Per model server, the file holds the drain hook, the `engine-cache` volume, and
the four variables under it. Copy it into the modelservice values, so the next
`helm upgrade` keeps it. Also copy the guard written as a comment beside the
cache half into the container's command: with it, a cache path that is not
writable costs one compile, not the replica.

To patch the live objects instead (the next `helm upgrade` reverts it, and it
restarts the pods): `smt workload-patch -n $NAMESPACE --apply --apply-engine-cache`.

## Verification

<!-- guide:verify.clean start -->
```bash
# Again, after the rollout. A model server it no longer names needs nothing more.
smt workload-patch -n $NAMESPACE
```
<!-- guide:verify.clean end -->

The first start on each node still compiles once; every start after that finds
the cache. To make the first start warm too, seed the cache:
[Engine caches on the node's disk](../../reference/workload-preparation.md#engine-caches-on-the-nodes-disk).

## Without the cluster: the manifests as files

<!-- guide:gitops.render start -->
```bash
# The same objects as files, for review or a GitOps repository. Reads no cluster.
smt prepull apply -n $NAMESPACE --image $ENGINE_IMAGE --dry-run > prepull.yaml
smt weights apply -n $NAMESPACE --model <hugging face id> --path $NODE_DIR --image $ENGINE_IMAGE --dry-run > weights.yaml
smt engine-cache apply -n $NAMESPACE --path $NODE_DIR --image $ENGINE_IMAGE --dry-run > engine-cache.yaml
```
<!-- guide:gitops.render end -->

`kubectl apply -f` each file. The `status` commands work on them as on the
objects `apply` made.

## Without the container

Each release attaches the same scripts as `scaling-manager-tools-<release>.tar.gz`,
signed like the install files. They run with your own `bash`, `kubectl`, `jq`
and `yq`, and your own kubeconfig, login plugins included. The release checks
them with yq v4.54.1 and jq 1.7.1, the versions the image carries; use those
or newer (yq must be mikefarah's Go yq, v4). `smt version` prints what it found.

<!-- guide:nocontainer.download start -->
```bash
# The same scripts from the release, for a machine with no container runtime. Needs bash, kubectl, jq, yq (mikefarah's, v4) and column (util-linux). On macOS, shasum -a 256 -c in place of sha256sum -c.
URL=https://github.com/llm-d-incubation/llm-d-scaling-manager/releases/download/$V
curl -fsSLO $URL/scaling-manager-tools-$V.tar.gz
curl -fsSLO $URL/SHA256SUMS
sha256sum -c SHA256SUMS --ignore-missing
tar -xzf scaling-manager-tools-$V.tar.gz
SMT_DIR=$PWD/scaling-manager-tools
touch smt.env
smt() { (set -a; . "$PWD/smt.env"; set +a; "$SMT_DIR/scaling-manager-tools.sh" "$@"); }
smt version
```
<!-- guide:nocontainer.download end -->

Every command on this page then works as written. To check the signature as
well as the checksum, verify `SHA256SUMS` with its `.sigstore.json` bundle:
[Verify the release files](../../reference/install-methods.md#verify-the-release-files).

From a checkout, the same commands are `make` targets (`make prepull`,
`make weights`, `make engine-cache`, `make workload-patch`).

## Warm pools need a checkout

`scaledobjects-plan` may suggest warm pools; neither the image nor the archive
can create them (that is `deploy/warmpool.sh`, with python3).
`scaledobjects-apply` refuses a plan that sets a warm pool to `apply: yes`.
Create pools from a checkout of the same release:
[Bridge a scale-up with a warm pool](../warm-pool/).

## Cleanup

<!-- guide:cleanup.remove start -->
```bash
# Removes the holders, claims, volumes and downloaders. The files on the nodes stay.
smt prepull delete -n $NAMESPACE --all
smt weights delete -n $NAMESPACE --all
smt engine-cache delete -n $NAMESPACE
```
<!-- guide:cleanup.remove end -->

`smt.kubeconfig` holds your credentials: delete it when you are done.

Scale the model servers that mount a claim to zero first: a mounted claim
stays `Terminating`. The files on the nodes stay; delete the directory on each
node when you no longer need them.

## Choosing differently

This page is the recommended configuration. The alternatives, their costs and
the measurements behind each choice are in
[Preparing a workload: every option](../../reference/workload-preparation.md):

- weights on a shared ReadWriteMany volume instead of the node's disk (less
  disk, a slower read);
- the caches on a shared claim you already have (no node directory, no
  PersistentVolume rights; a CSI driver can publish it read-only on a node);
- a seed, so the first start on each node is warm too;
- OpenShift paths and permissions;
- the drain hook, the engine's memory budget, and LeaderWorkerSets.

## Next

- [Bridge a scale-up with a warm pool](../warm-pool/): for the start that
  remains, keep a model loaded and asleep.
- [Replica start time](../../reference/start-time.md): what each option buys.
