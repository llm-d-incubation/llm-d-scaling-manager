# Install with kubectl, from one manifest

## Overview

**New in v0.13.0.** Earlier releases do not carry these files; on those,
install from [the cluster-wide guide](../install-cluster-wide/). The path is
tested end to end on kind (`make test-e2e-manifest-install`, `make
test-e2e-manifest-migrate`).

One scaling manager serves the cluster, the way KEDA, Kueue and
LeaderWorkerSet install: signed files, applied as downloaded with `kubectl`.
No docker, no checkout, and no file is edited before it is applied.

How it runs:

- One controller, in `scaling-manager-system`.
- In each namespace it manages, it finds the InferencePools and reads each
  EPP's `/metrics` directly. It reads the model servers' metrics from
  Prometheus.
- KEDA calls its scaler for the ScaledObjects in those namespaces. The scaler
  refuses a ScaledObject in any other namespace.
- It starts **passive**: it decides and publishes, and moves nothing.

Two people install it, with one script each. Both scripts run `kubectl`
only, and `--print` shows the commands instead of running them.

| Who | Does | Script |
|---|---|---|
| Cluster admin | installs and upgrades the manager; owns the settings (Prometheus, the budget, passive, shadow, active); decides which namespaces it manages, with the `watchNamespaces` setting; grants an EPP token reviews where the router chart did not | `scaling-manager-admin.sh` |
| Namespace owner | adds the PodMonitor and the NetworkPolicy rule if the namespace needs them; registers ScaledObjects; optionally sets its own scaling policy; binds the pod-writer grant. Never deploys the manager | `scaling-manager-owner.sh` |

What the controller does by itself:

- **Its checks.** It runs the preflight checks every minute, and writes
  each result and `ready` to its status ConfigMap.
- **Each namespace's readiness.** It writes one line per namespace to the
  status ConfigMap, and the same line as an Event on the ScaledObjects and
  InferencePools in that namespace. A namespace owner reads it there.
- **Its TLS, and KEDA's copy.** It fills the scaler's certificate Secrets,
  which the manifest creates empty, and writes KEDA's copy of the client
  certificate. No step for a person.
- **Its settings.** It waits for them, live, with the reason in `ready`.
  `watchNamespaces` and the switches take effect without a restart. When a
  setting read only at start changes, it restarts itself.
- **A suggested plan.** It writes one ScaledObject per model server in each
  managed namespace into ConfigMap `scaling-manager-scaledobjects-plan`.

## Prerequisites

- [ ] Cluster-admin rights, for the cluster admin's part.
- [ ] **KEDA** 2.18 or later, in any namespace.
- [ ] A Prometheus with the Prometheus Operator, scraping the model servers.
- [ ] `kubectl` and `bash`; `cosign` to verify the download.

## Cluster admin

1. Download and verify the files:

<!-- guide:admin.download start -->
```bash
# The cluster admin. The release files and their signatures; every file is applied as downloaded, never edited.
V=<release, v0.13.0 or later>
URL=https://github.com/llm-d-incubation/llm-d-scaling-manager/releases/download/$V
for f in scaling-manager.yaml scaling-manager-keda.yaml scaling-manager-grants.yaml scaling-manager-monitoring.yaml \
    scaling-manager-admin.sh scaling-manager-owner.sh; do
  curl -fsSLO $URL/$f && curl -fsSLO $URL/$f.sigstore.json
  cosign verify-blob $f --bundle $f.sigstore.json \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    --certificate-identity "https://github.com/llm-d-incubation/llm-d-scaling-manager/.github/workflows/ci-release.yaml@refs/tags/$V"
done
```
<!-- guide:admin.download end -->

2. Install. If a private CA signed Prometheus's certificate, save it as
   `prometheus-ca.crt` first.

<!-- guide:admin.install start -->
```bash
# The manifest, KEDA's side in its namespace, the settings. Passive. Leave out --keda-namespace for keda, --prometheus-ca for a public CA.
bash scaling-manager-admin.sh install --prometheus-url https://prometheus.monitoring.svc:9090 \
  --prometheus-ca prometheus-ca.crt --keda-namespace keda --watch-namespaces team-a,team-b
```
<!-- guide:admin.install end -->

   It applies `scaling-manager.yaml`, applies `scaling-manager-keda.yaml` in
   KEDA's namespace and labels that namespace `scaling-manager.llmd.ai/keda=true`
   (the scaler's NetworkPolicy admits it by that label), and writes the
   settings ConfigMap: `prometheusURL`, `kedaNamespace`, `caFromSecret`,
   `watchNamespaces`.

   With GitOps, keep the commands, not a copy of the files:

<!-- guide:admin.print start -->
```bash
# The same, as the kubectl commands it runs, for git.
bash scaling-manager-admin.sh --print install --prometheus-url https://prometheus.monitoring.svc:9090 \
  --keda-namespace keda --watch-namespaces team-a,team-b
```
<!-- guide:admin.print end -->

   Never keep the switches `passive`, `utilizationShare` and `dsetRoleWriter`,
   or the grants, in git: a sync would undo a change made in an emergency.

3. Choose the namespaces it manages, at any time:

<!-- guide:admin.namespaces start -->
```bash
# Later, which namespaces the controller manages. Takes effect without a restart.
bash scaling-manager-admin.sh namespaces team-a,team-b,team-c
```
<!-- guide:admin.namespaces end -->

   An empty `watchNamespaces` (`namespaces --all`, or `install` without
   `--watch-namespaces`) manages every namespace, as on every cluster-wide
   install; any change to that meaning would come in a later release. On a
   shared cluster, list the namespaces: a fresh install with
   an empty list serves a ScaledObject in any namespace and shares one
   budget across all of them. A value the controller rejects keeps the list
   in force; a rejected value with no list before it manages nothing.

4. Grant an EPP token reviews, only where its router chart did not. The
   controller reads each EPP's `/metrics` with its own token, which the EPP
   reviews:

<!-- guide:admin.epp start -->
```bash
# Only where the router chart did not let the EPP review tokens; its /metrics answers 500 to the controller otherwise.
bash scaling-manager-admin.sh grant-epp team-a <the EPP's ServiceAccount>
```
<!-- guide:admin.epp end -->

5. Hand each namespace owner the controller's suggested ScaledObjects for
   their namespace, as a file: the owner cannot read the controller's
   namespace.

<!-- guide:admin.plan start -->
```bash
# The controller's suggested ScaledObjects for one namespace, as a file for its owner, who cannot read the controller's namespace.
kubectl get configmap scaling-manager-scaledobjects-plan -n scaling-manager-system -o jsonpath='{.data.team-a}' > team-a-scaledobjects.yaml
```
<!-- guide:admin.plan end -->

6. Once each namespace is `ok` and the counts it publishes look right, let
   it scale:

<!-- guide:admin.active start -->
```bash
# Once the readiness and the published counts look right, let it scale.
bash scaling-manager-admin.sh set passive=false
```
<!-- guide:admin.active end -->

## Namespace owner

Each command is for one namespace, and needs rights in that namespace only;
`policy` also labels the namespace, which needs the cluster admin. First get
your script and the two files it applies, from the release the cluster admin
installed:

<!-- guide:owner.download start -->
```bash
# The namespace owner. The owner's script and the two files it applies, verified like the admin's.
V=<the release the cluster admin installed>
URL=https://github.com/llm-d-incubation/llm-d-scaling-manager/releases/download/$V
for f in scaling-manager-owner.sh scaling-manager-monitoring.yaml scaling-manager-grants.yaml; do
  curl -fsSLO $URL/$f && curl -fsSLO $URL/$f.sigstore.json
  cosign verify-blob $f --bundle $f.sigstore.json \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    --certificate-identity "https://github.com/llm-d-incubation/llm-d-scaling-manager/.github/workflows/ci-release.yaml@refs/tags/$V"
done
```
<!-- guide:owner.download end -->

<!-- guide:owner.monitor start -->
```bash
# Only where nothing scrapes the model servers yet (llm-d's monitoring components scrape the same port).
bash scaling-manager-owner.sh monitor team-a
```
<!-- guide:owner.monitor end -->

<!-- guide:owner.network start -->
```bash
# Only where a default-deny ingress policy is in place.
bash scaling-manager-owner.sh network team-a
```
<!-- guide:owner.network end -->

The NetworkPolicy rule lets the controller reach port 9090 on every pod of
the namespace. In a namespace with no policy it would cut every other
ingress, so apply it only where a default deny is in place.

Register your ScaledObjects: the file the cluster admin handed you (review
min and max first), or your own. Without a file, the script applies the
suggestion straight from the controller's namespace, which only someone who
can read it, such as the cluster admin, can do.

<!-- guide:owner.register start -->
```bash
# Your ScaledObjects, from a file you wrote or the suggestion the cluster admin handed you.
bash scaling-manager-owner.sh register team-a scaledobjects.yaml
```
<!-- guide:owner.register end -->

The suggestion covers Deployments whose pods carry `llm-d.ai/role` `decode`
or `prefill`. Write LeaderWorkerSet and DisaggregatedSet ScaledObjects by
hand, with the same scaler address and ClusterTriggerAuthentication.

Your own scaling policy is optional:
`bash scaling-manager-owner.sh policy team-a policy.yaml` applies it and
labels the namespace so the controller reads it.

Bind the grant the admin's switches need:

<!-- guide:owner.grant start -->
```bash
# Before the admin sets utilizationShare active; dset-role-writer for a Static-role DisaggregatedSet. Never in git.
bash scaling-manager-owner.sh grant pod-writer team-a
```
<!-- guide:owner.grant end -->

Read your namespace's readiness, without reading the controller's namespace:

<!-- guide:owner.status start -->
```bash
# Your namespace's readiness, in your namespace.
bash scaling-manager-owner.sh status team-a
```
<!-- guide:owner.status end -->

| Event reason | Means |
|---|---|
| `ScalingManagerNamespaceReady` | managed, ScaledObjects registered, no grant missing |
| `ScalingManagerNamespaceWaiting` | managed, but no ScaledObject here has called the scaler yet |
| `ScalingManagerNamespaceNotReady` | a grant a switch needs is missing; the message has the command |
| `ScalingManagerNamespaceNotManaged` | not in `watchNamespaces`: the scaler refuses its ScaledObjects. Ask the cluster admin |

## Check it

<!-- guide:check.status start -->
```bash
# The admin. ready, the managed namespaces, each namespace's readiness, each check.
bash scaling-manager-admin.sh status
```
<!-- guide:check.status end -->

`ready` is `ok` when no check fails. A WARN or an UNKNOWN does not block it.
`namespacesManaged` is the list, or `*` for every namespace.
`namespace.<name>` is each namespace's readiness. `namespacesRefused` lists
the namespaces KEDA called the scaler for that are not managed.

## Upgrade and rollback

Download and verify the new release's files as in step 1, then:

<!-- guide:upgrade.apply start -->
```bash
# With the new release's files, downloaded and verified as above. The settings and the ScaledObjects stay. A rollback is the same command with the files of an older release that carries scaling-manager.yaml.
bash scaling-manager-admin.sh upgrade --keda-namespace keda
```
<!-- guide:upgrade.apply end -->

The settings and the ScaledObjects are not in the files, so they stay. The
scaler's certificate Secrets and KEDA's copy carry no data in the files, so
a re-apply keeps what the controller wrote, and KEDA stays connected. A
setting an older release does not know is listed in the status as unknown,
and ignored.

## Move from today's installs

**A cluster-wide `smt` install.** The controller copies the `smt` install's
scaler CA into its own Secrets, so KEDA stays connected with no restart of
its operator (`make test-e2e-manifest-migrate`).

<!-- guide:migrate.cluster start -->
```bash
# Over a cluster-wide smt install (--scope cluster). --force-conflicts takes the controller Deployment over from smt, this once; then list the namespaces it manages.
bash scaling-manager-admin.sh move --keda-namespace keda
bash scaling-manager-admin.sh namespaces team-a,team-b
```
<!-- guide:migrate.cluster end -->

A cluster-wide install from `scaling-manager-cluster.yaml` has the same
objects, and moves the same way; no e2e covers it yet.

`move` also deletes the `smt` install's cluster-wide DisaggregatedSet role
writer grant: here each namespace owner binds it
(`scaling-manager-owner.sh grant dset-role-writer <ns>`). What else the `smt`
install made stays until `uninstall`, which removes it by the same instance
label.

**A namespaced install** (one controller per namespace) is not moved in
place. Its scaler runs in that namespace, so its ScaledObjects name another
address and ClusterTriggerAuthentication. Uninstall it as its guide says,
install as above with the namespace in `watchNamespaces`, and register the
ScaledObjects again with `scaling-manager-owner.sh register`.

## Uninstall

<!-- guide:uninstall.all start -->
```bash
# Removes everything this install made, in every namespace, and what a moved smt install left. Sharing goes to shadow first, so no transfer mark stays on a pod.
bash scaling-manager-admin.sh uninstall --keda-namespace keda
kubectl get clusterrole,clusterrolebinding,clustertriggerauthentication -l app.kubernetes.io/instance=scaling-manager-system
```
<!-- guide:uninstall.all end -->

The last line lists what is left; it should list nothing. The settings go
with the namespace. The ScaledObjects an owner registered from their own
file are not labelled by this install and stay: each owner deletes theirs
first (`kubectl delete scaledobject -n <ns> <name>`).

## Not covered yet

- Suggested ScaledObjects for LeaderWorkerSets and DisaggregatedSets.
- The EPP's ServiceMonitor in `scaling-manager-monitoring.yaml`.
- The kustomize tarball, and the least-privilege path per namespace.
