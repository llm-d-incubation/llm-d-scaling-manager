# Release Process

How to cut a release of the llm-d-scaling-manager: what to merge to `main`, how
to tag it, and what to do when the release workflow fails.

## Quick reference

A release from `main`, `-rc` ones included, takes **three PRs to `main`** and
one tag. A patch to an older line follows [Patch releases](#patch-releases)
instead.

| Step | Where | What |
|------|--------|------|
| 1. Release PR | PR to `main` | `newTag: vX.Y.Z`; merge it |
| 2. Tag | Your clone | Tag the merge commit of the release PR, check it, push the tag |
| 3. Wait | GitHub Actions | The release run must succeed |
| 4. Reset PR | PR to `main` | `newTag: main` again, only after the run succeeded |
| 5. Publish | GitHub | Publish the draft GitHub Release the run made, with the release notes; verify the signatures |
| 6. Docs PR | PR to `main` | Point the install, upgrade and benchmark docs at the release and its digest |

---

## Scope of a release

1. **A version tag** in this repository, `vMAJOR.MINOR.PATCH` or
   `vMAJOR.MINOR.PATCH-PRERELEASE` (e.g. `v0.6.0`, `v0.6.0-rc1`).
2. **A container image** at `ghcr.io/<repository owner>/llm-d-scaling-manager:<tag>`,
   i.e. `ghcr.io/llm-d-incubation/llm-d-scaling-manager:<tag>` for this repository.
3. **Release notes**, in the GitHub Release. They are not kept as a file in
   this repository.
4. **Release files**, attached to the GitHub Release by the run: the install
   manifests rendered with the image pinned by digest, `SHA256SUMS`, and a
   `<file>.sigstore.json` signature bundle for each. See
   [The release files](#the-release-files).
5. **A tools image** at `ghcr.io/<repository owner>/llm-d-scaling-manager/tools:<tag>`:
   `deploy/prepull.sh`, `deploy/weights.sh`, `deploy/enginecache.sh`,
   `deploy/utilization-share.sh`, the workload-patch report and the
   ScaledObject plan, with kubectl, jq and yq, from `Dockerfile.tools`.
   Operators prepare and register their model servers with it and no checkout
   ([Prepare model servers](../guides/prepare-workloads/)).

A tag is released only from `main`, or from its own line's
`release-MAJOR.MINOR` branch (a `v0.10.2` from `release-0.10`) once that line
has a tag on `main`, and only when `newTag` on the tagged commit equals the tag. See [Patch releases](#patch-releases).

---

## Prerequisites

- [ ] You may push tags to this repository and publish GitHub Releases.
- [ ] The GHCR package `llm-d-scaling-manager` gives this repository **Write**
  access ([first release](#the-first-release-to-a-new-package)).
- [ ] The commit you will tag passed its PR checks, including the e2e runs. The
  release reruns the PR checks, not the e2e suites.

The workflow checks where a tag is, not who reviewed it. Review on `main` and
`release-*` is enforced only by the repository's branch rules, if set.

---

## Pre-release checklist

1. **Open the release PR to `main`.** It sets `newTag: vX.Y.Z` in
   `config/base/manager/kustomization.yaml`. The release fails before it builds
   anything if `newTag` is not the tag.
2. **Bump dependency pins**, if the release moves one (for example
   [llm-d-inference-sim](https://github.com/llm-d/llm-d-inference-sim)): the
   [Makefile](../../Makefile), `test/e2e/fixtures/model_service_builder.go` and
   `test/utils/resources/llmdsim.go`. Same PR.
3. **Draft the release notes** for the GitHub Release: user-facing and notable
   changes, and the upgrade steps. You paste them when you publish.
4. **Merge the PR, then tag right away.** From the merge until the release run
   pushes the image, an install from `main` pulls `:vX.Y.Z`, which does not
   exist yet.

---

## Creating the release

### 1. Push the tag

Tag the merge commit of the release PR, not the tip of `main` (another PR may
have merged since) and not your local branch. The command checks that the tagged
commit pins the release and is on `main`, and pushes only then.

`REMOTE` is the name of your remote for `llm-d-incubation/llm-d-scaling-manager`
(`git remote -v` lists them). It is not always `origin`.

```bash
REMOTE=<remote for llm-d-incubation/llm-d-scaling-manager>
git fetch "$REMOTE"
git tag -m "llm-d-scaling-manager vX.Y.Z" vX.Y.Z <merge commit of the release PR>
git show vX.Y.Z:config/base/manager/kustomization.yaml | grep -x '  newTag: vX.Y.Z' \
  && git merge-base --is-ancestor vX.Y.Z "$REMOTE/main" && git push "$REMOTE" vX.Y.Z
```

`-m` makes an annotated tag, and with `tag.gpgSign` set a signed one; without
`-m`, a signing setup opens an editor for the tag message. The workflow reads
the commit the tag points at, so a lightweight, annotated or signed tag all
work. Push the tag by name: `git push --tags` would also push every other tag
your clone holds.

The push starts [`ci-release.yaml`](../../.github/workflows/ci-release.yaml).
Watch it in the **Actions** tab.

### 2. Wait for the run to succeed

Do not merge anything that changes `newTag` yet. If the run fails, see
[If the release fails](#if-the-release-fails).

### 3. Set `newTag` back to `main`

Once the run has succeeded, open a second PR to `main` that sets `newTag: main`
again, and merge it. Until it merges, an install from `main` deploys the release
image instead of `:main`. Merged earlier, a re-tag after a failed run would fail
the pin check.

### 4. Publish the GitHub Release

The run created a **draft** release for the tag, already marked as a
pre-release for a prerelease tag, with the release files attached. If a
draft release for the tag existed before the run, it attached the files to
that one instead. A release that is already **published** is never changed: a
run whose files differ from the attached ones fails, and one whose files are
identical uploads nothing. In **Releases**, open the draft, check that the
files are there (each `.yaml`, `SHA256SUMS`, and a `.sigstore.json` for each),
paste the release notes, and publish. Keep the draft's first line: it names
the two supported installs, the attached files and `make deploy-scaling-manager`.

### 5. Point the docs at the release

Open a docs PR to `main` that replaces the previous version with the new one in
the install, upgrade and benchmark docs, pins the new multi-arch index digest
from the run's summary where the docs pin one, and adds the upgrade section for
this release to `docs/reference/upgrade-and-uninstall.md`.

### If the release fails

Find the failed step in the run.

**It failed before "Build and push the multi-arch image".** Nothing was
published.

- A flaky check: open the run and use **Re-run failed jobs**.
- Anything else: delete the tag, fix the problem with a PR to `main` (for a
  patch, to its release branch) that keeps
  `newTag: vX.Y.Z`, tag that PR's merge commit as in step 1, and repeat:
  ```bash
  git push "$REMOTE" --delete vX.Y.Z && git tag -d vX.Y.Z
  ```

**It failed at or after "Build and push the multi-arch image".** An image may
be public under that tag. Do not move or delete the tag.

- The cause is outside the code (for example, the package access below): fix
  it, then use **Re-run failed jobs**. A re-run uses the workflow and the code
  at the tag.
- The fix needs a change to the code or the workflow: release `vX.Y.(Z+1)`
  instead.

**It failed in the `release-files` job.** The image is published and signed;
only the files are missing or incomplete. Use **Re-run failed jobs**: the job
renders, signs and uploads again, replacing whatever an earlier attempt
attached. Do not publish the release until it has succeeded.

---

## Patch releases

An urgent fix for a released line ships from that line's branch while `main`
moves on: `v0.10.2` from `release-0.10`. The workflow releases a tag from a
`release-MAJOR.MINOR` branch only if the tag is of that line and the line
already has a tag on `main` (an `-rc` tag counts).

**Creating a line's branch** (once per line). Start it at the line's latest
release and add the release-branch CI change, which a tag runs from the tagged
commit:

```bash
REMOTE=<remote for llm-d-incubation/llm-d-scaling-manager>
git fetch --tags "$REMOTE"
git push "$REMOTE" 'v0.10.1^{commit}:refs/heads/release-0.10'
```

Then open a PR to `release-0.10` that cherry-picks the commit adding
release-branch support to `ci-release.yaml`, `ci-pr-checks.yaml` and
`non-main-gatekeeper.yml`. Merge it first: until it is on the branch, the
branch's tags are refused.

**Each patch:**

1. **Fix on `main` first**, then open a PR to `release-0.10` that cherry-picks
   the merged commit (`git cherry-pick -x <sha>`). If `main` no longer has the
   bug, open the fix against the release branch alone and say why in the PR.
   PRs to `release-*` run the same checks as PRs to `main`.
2. **Release PR to `release-0.10`**, not to `main`. It sets
   `newTag: v0.10.2` and moves every version reference on the branch to
   `v0.10.2`; list them with
   `git grep -n 'v0\.10\.[0-9]' -- docs deploy hack/benchmark`. It adds
   "Upgrading from v0.10.1 to v0.10.2" to
   `docs/reference/upgrade-and-uninstall.md`. Pre-release checklist items 2
   and 3 apply.
3. **Tag its merge commit**, as in [step 1](#1-push-the-tag), with the release
   branch in place of `main`:
   ```bash
   REMOTE=<remote for llm-d-incubation/llm-d-scaling-manager>
   git fetch "$REMOTE"
   git tag -m "llm-d-scaling-manager v0.10.2" v0.10.2 <merge commit>
   git show v0.10.2:config/base/manager/kustomization.yaml | grep -x '  newTag: v0.10.2' \
     && git merge-base --is-ancestor v0.10.2 "$REMOTE/release-0.10" && git push "$REMOTE" v0.10.2
   ```
4. **No reset PR.** The branch keeps the pin; `main` never had it.
5. **Publish** as in [step 4](#4-publish-the-github-release). Untick
   **Set as the latest release** unless the patch is the highest final release.
6. **Docs PR to `main`**: add the upgrade section. Point `main`'s install docs
   at the patch only if it is the highest final release.

If the run fails, recover as in [If the release fails](#if-the-release-fails),
with the fix PR going to the release branch instead of `main`.

The `latest` image tag moves only for the highest final release; a `v0.10.2`
after `v0.11.0` leaves it on `v0.11.0`.

---

## The first release to a new package

The package `llm-d-scaling-manager` already exists for this repository: it is
public, and this repository has Write access to it. Nothing needs doing unless
that changes, for example after a move to another owner.

The release pushes with the built-in `GITHUB_TOKEN`. No `GHCR_TOKEN` secret is
used. If the repository has no Write access to the package, the run fails at
**Build and push the multi-arch image** with:

```
denied: permission_denied: write_package
```

To fix it:

1. Open the package page: **Packages** on the repository owner's profile, then
   `llm-d-scaling-manager`.
2. **Package settings** → **Manage Actions access** → **Add repository**.
   Pick this repository and give it **Write**.
3. In the failed run, choose **Re-run failed jobs**. No new tag is needed.

A package GitHub creates on the first push is **private**. Users then get
`401 Unauthorized` when they pull. In **Package settings**, set its visibility
to **Public**.

Make the first release after this workflow lands an `-rc1` tag. It tests the
release run end to end without moving `latest`.

The tools image is a second package, `llm-d-scaling-manager/tools`, which the
first release that carries the `release-tools` job creates. It starts
**private**: set it to **Public** after that run, and check that this
repository has **Write** on it, as above. Until then `docker pull` of the
tools image fails with `401 Unauthorized` (or `denied`) for every operator.

---

## Verifying a release

Replace the owner `llm-d-incubation` with yours if you release from a fork. The
workflow signs with cosign v3; verify with cosign v3 or later, because cosign v2
reports `no signatures found` for these images. With no cosign installed, run
its image: `docker run --rm ghcr.io/sigstore/cosign/cosign:v3.0.6 verify ...`.

Record the index digest in the release notes, so users can deploy by digest:
`docker buildx imagetools inspect "$IMAGE:vX.Y.Z"` prints it as `Digest:`.

```bash
IMAGE=ghcr.io/llm-d-incubation/llm-d-scaling-manager
cosign verify "$IMAGE:vX.Y.Z" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity "https://github.com/llm-d-incubation/llm-d-scaling-manager/.github/workflows/ci-release.yaml@refs/tags/vX.Y.Z"

# SBOM and provenance attached to the index
docker buildx imagetools inspect "$IMAGE:vX.Y.Z" --format '{{ json .SBOM }}'
docker buildx imagetools inspect "$IMAGE:vX.Y.Z" --format '{{ json .Provenance }}'
```

The release files are signed by the same identity. Download a file, its
bundle and `SHA256SUMS`, then verify with cosign 2.4 or later (earlier
versions cannot read these bundles):

```bash
V=vX.Y.Z
cosign verify-blob scaling-manager-namespaced.yaml \
  --bundle scaling-manager-namespaced.yaml.sigstore.json \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity "https://github.com/llm-d-incubation/llm-d-scaling-manager/.github/workflows/ci-release.yaml@refs/tags/$V"
sha256sum -c SHA256SUMS --ignore-missing
```

To accept any release tag of this workflow rather than one, use
`--certificate-identity-regexp '^https://github\.com/llm-d-incubation/llm-d-scaling-manager/\.github/workflows/ci-release\.yaml@refs/tags/v[0-9]+\.[0-9]+\.[0-9]+(-.+)?$'`
in place of `--certificate-identity`.

Deploy by digest (`IMG=$IMAGE@sha256:...`) when you need the exact bytes that
were verified; a tag can be moved after the fact, a digest cannot.

Installing a release needs no `IMG`: `git checkout vX.Y.Z && make deploy-scaling-manager`
deploys `:vX.Y.Z`, pulled `IfNotPresent` because a release tag does not move.

---

## Enabling others to cut releases

Grant them permission to push tags and to publish Releases. No registry secret
needs sharing. Point them at this page.

---

## Details

### What the workflow does

[`ci-release.yaml`](../../.github/workflows/ci-release.yaml) runs once per tag
push. Releases run one at a time. GitHub keeps one run waiting: a third tag
pushed while one runs and one waits cancels the waiting run, which you then
re-run from the **Actions** tab.

1. **Gate.** Calls [`ci-pr-checks.yaml`](../../.github/workflows/ci-pr-checks.yaml)
   on the tagged commit: `golangci-lint`, `make build`, `make test`,
   `make lint-deploy-scripts`, the tools image build and check, the docs
   checks and the kustomize builds.
2. **Validate.** Refuses a tag whose commit is on neither `main` nor the tag's
   `release-MAJOR.MINOR` branch, a tag that is not
   `vMAJOR.MINOR.PATCH[-PRERELEASE]`, and a commit whose `newTag` is not the tag.
3. **Scan before push.** Builds the `linux/amd64` image locally and scans it
   with Trivy. A **fixable CRITICAL** vulnerability fails the release. A CRITICAL
   with no upstream fix is reported but does not block. A second, non-blocking
   scan prints every HIGH and CRITICAL finding. The same blocking scan runs in
   the PR checks' `e2e-tests-smoke` job, so a finding fails the PR that brings
   it in before it can fail a tag.
4. **Build and push once.** One `linux/amd64,linux/arm64` build, pushed with an
   SBOM and a max-mode SLSA provenance attestation.
5. **Tags.** Always `:<version>`. `:latest` moves only for the highest final
   release; a prerelease (`-rc1`) never moves it. `hack/check-release-latest.sh`
   tests this.
6. **Sign.** Keyless `cosign sign` of the pushed digest, with the workflow's
   OIDC identity as the signer. The run summary prints the digest and the
   verify command.
7. **Release files** (job `release-files`, after the image job). Runs
   `make release-manifests` with the pushed digest, signs each file and
   `SHA256SUMS` with keyless `cosign sign-blob --bundle`, and uploads them all
   with `gh release upload --clobber`, creating a draft release when the tag has
   none, and refusing to change a published one. It is the only job with
   `contents: write`, and it builds no image. Its yq is pinned by sha256 and
   its Go (for `make kustomize`) comes from `go.mod`.
   `hack/check-release-manifests.sh` checks the job's shape and runs its sign
   step against a stub cosign.
8. **Tools image** (job `release-tools`, after the image job). Builds
   `Dockerfile.tools`, scans it like the controller image, pushes
   `linux/amd64,linux/arm64` to `<image>/tools:<version>` (and `:latest` only
   when the controller's `latest` moved), and signs the digest.
   `hack/check-release-latest.sh` checks the job's shape and runs its tag and
   sign steps. The PR checks' `tools-image` job builds both platforms on every
   code change and runs `hack/check-tools-image.sh` against the amd64 image.
   Verify it as the controller image, with `$IMAGE/tools`.

### The release files

Rendered by `deploy/release-manifests.sh` from the same kustomize overlays
`make deploy-scaling-manager` applies, plus the sources in `config/release/`.
Design and rules: [install-from-release.md](../proposals/install-from-release.md).

| file | holds |
|------|-------|
| `scaling-manager-namespaced.yaml`, `-namespaced-openshift.yaml` | one controller per namespace; namespace written `__NAMESPACE__` |
| `scaling-manager-cluster.yaml`, `-cluster-openshift.yaml` | one controller for the cluster, in `scaling-manager-system` |
| `scaling-manager-settings.yaml` | the `scaling-manager-settings` ConfigMap with every key's default |
| `scaling-manager-model-namespace.yaml` | per model namespace: the model-server PodMonitor, the controller's Role and RoleBinding |
| `scaling-manager-activate.yaml` | the only grant of `patch` on pods (the manager Roles only read pods): applied for `utilizationShare: active` or the warm pool, in each namespace where the controller must patch pods |
| `scaling-manager-keda-auth.yaml` | the ClusterTriggerAuthentication for scaler mTLS and its marker ConfigMap |
| `scaling-manager-kueue.yaml` | optional Kueue reads |
| `scaling-manager-alerts.yaml` | optional PrometheusRule |
| `scaling-manager-policy-example.yaml` | an example scaling-policy ConfigMap |
| `scaling-manager-metrics-reader.yaml` | optional, Kubernetes: binds the scraping Prometheus account to the metrics-reader ClusterRole |
| `scaling-manager-preflight.yaml` | the preflight Job (`manager preflight check`, image by digest), its ServiceAccount, and its read-only Role and ClusterRole; reads pods and PodMonitors in the controller's namespace only, and no Secret ([preflight.md](../reference/preflight.md)) |
| `scaling-manager-preflight-cluster.yaml` | the same for the cluster-wide install in `scaling-manager-system` (`--scope=cluster`): lists pods, Deployments and PodMonitors in every namespace. Applies as is |

`make test-e2e-release-manifests` installs the namespaced files on kind with
`kubectl` only and checks them, and `RELEASE_E2E_SCOPE=cluster` does the same
for the cluster-wide files. CI runs both on PRs that touch what the files are
made from; run them before a release too
([Testing](testing.md#install-from-the-release-files)). The OpenShift files
are not tested end to end and say so in their header.

Every object carries `app.kubernetes.io/part-of`, `instance`, `version` and
`scaling-manager.llmd.ai/part: admin|controller`; `admin` is exactly what the
installer's prereqs phase applies (`LLMSM_PREREQ_KINDS` and
`LLMSM_PREREQ_NAMED` in `deploy/lib/common.sh`). `make lint-deploy-scripts`
runs `hack/check-release-manifests.sh`, which renders the files, asserts the
rules, and then breaks a copy one rule at a time to show each break is caught.

To render them locally, give an image by digest:

```bash
make release-manifests IMG=ghcr.io/llm-d-incubation/llm-d-scaling-manager@sha256:<digest> \
  RELEASE_VERSION=vX.Y.Z DIST=dist
```

To try them on kind, substitute the placeholders and apply each part by its
label, the way a cluster admin and a namespace owner would. KEDA and
prometheus-operator CRDs must exist for the KEDA and monitoring objects.
Besides `__NAMESPACE__`, the KEDA file needs `__KEDA_NAMESPACE__`, the Kueue
file `__KUEUE_NAMESPACE__`, the per-model-namespace files
`__MODEL_NAMESPACE__`, and the metrics-reader file `__PROMETHEUS_NAMESPACE__`
and `__PROMETHEUS_SA__`:

```bash
kubectl create namespace team-a
for f in dist/scaling-manager-*.yaml; do
  sed -e 's/__NAMESPACE__/team-a/g' -e 's/__MODEL_NAMESPACE__/team-a/g' \
      -e 's/__KEDA_NAMESPACE__/keda/g' -e 's/__KUEUE_NAMESPACE__/kueue-system/g' \
      "$f" > "/tmp/$(basename "$f")"
done
kubectl apply -f /tmp/scaling-manager-namespaced.yaml -l scaling-manager.llmd.ai/part=admin
kubectl apply -f /tmp/scaling-manager-namespaced.yaml -l scaling-manager.llmd.ai/part=controller
kubectl create -f /tmp/scaling-manager-settings.yaml
```

`kubectl apply -f <file> -l <selector>` applies only the matching objects
(checked with kubectl v1.31 on kind: the admin apply created no Deployment or
Service, the controller apply created exactly those three objects).
