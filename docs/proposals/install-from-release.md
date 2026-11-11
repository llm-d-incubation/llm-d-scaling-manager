# Install from the release, not from a checkout

- **Status:** in progress. Phase 1 is built: `make release-manifests`
  (`deploy/release-manifests.sh`), its check, and the release workflow signing
  and attaching the files. What building it changed in this design is in
  [Found while building phase 1](#found-while-building-phase-1). Phase 3, the
  scaler TLS bootstrap, is built; what it changed is in
  [Found while building phase 3](#found-while-building-phase-3). The rest is
  not built.
- **Problem:** installing takes a checkout, `make`, and about 15,000 lines of
  shell under `deploy/`. A reviewer found the content complete and the
  procedure too complex. An operator should need the image and a short
  procedure.
- **Decision (already taken):** the release attaches rendered manifests. The
  operator applies them with `kubectl apply`, edits **one settings
  ConfigMap**, and runs a **preflight Job** that checks the prerequisites and
  says what is wrong. No Helm, no remote kustomize base, no operator.
  `make deploy-scaling-manager` stays a supported install path beside it
  ([Two install paths](#two-install-paths)).
- **Main path:** utilization share, passive first, then shadow, then active.
  The first apply must give a passive controller.
- **v0.11.0:** the manifests, the settings ConfigMap, the preflight Job, the
  install and passive guides rewritten on them, and a kind e2e that installs
  from the rendered files. See [Phases](#phases).

## Decisions (2026-11-10)

Taken by the maintainer on review of the draft. Decisions 1-4 answered open
questions; 5-8 accept the draft's recommendations. Nothing is open.

1. **`make deploy-scaling-manager` stays a supported user path** after
   v0.11.x, not development-only. There are two supported install paths, both
   documented and both tested ([Two install paths](#two-install-paths)).
2. **The settings ConfigMap is the new `scaling-manager-settings`**, with flat
   keys, and for one release the controller falls back to the old
   `scaling-manager-manager-config` `config.yaml` for a key it does not hold
   ([The settings ConfigMap](#the-settings-configmap)).
3. **The pod-writer grant is made only at activation**, as today. Going active
   is a settings patch plus applying one small RBAC file,
   `scaling-manager-activate.yaml`, per model namespace
   ([Passive, then shadow, then active](#passive-then-shadow-then-active)).
4. **The namespace is substituted with `sed` on `__NAMESPACE__`.** No render
   tool.
5. **The namespace and Kueue readers stay narrow** (`resourceNames`), not a
   cluster-wide `get namespaces`. A controller that manages more namespaces
   than its own needs each one added; preflight names each missing namespace
   and prints the `kubectl patch` that adds it.
6. **The scaler NetworkPolicy ships in the main file**, naming `keda` (KEDA's
   chart default) in the Kubernetes files and `openshift-keda` in the
   OpenShift files. Preflight reads KEDA's namespace and FAILs on a mismatch,
   with the patch.
7. **The `newTag` pin stays**, with its release-time check. The manifests are
   pinned by digest, but `git checkout <tag> && make deploy-scaling-manager` is
   a supported path (decision 1), and the pin is what makes it install that
   release's image.
8. **Scale-to-zero is off by default** on both paths. This follows the change
   that unified the `ENABLE_SCALE_TO_ZERO` default to off everywhere (PR #66);
   the settings file ships `scaleToZero: "false"`.

## Summary

The install an operator runs becomes:

```bash
# 1. Download the release's files. Check them (optional): see "Signing".
V=v0.11.0
URL=https://github.com/llm-d-incubation/llm-d-scaling-manager/releases/download/$V
curl -fsSLO $URL/scaling-manager-namespaced.yaml
curl -fsSLO $URL/scaling-manager-settings.yaml
curl -fsSLO $URL/scaling-manager-preflight.yaml
curl -fsSLO $URL/scaling-manager-model-namespace.yaml

# 2. Edit the settings: the Prometheus URL, at least. Passive is the default.
#    Then put everything in your namespace.
export NS=<your-namespace>
for f in scaling-manager-*.yaml; do sed -i "s/__NAMESPACE__/$NS/g" "$f"; done

# 3. Check. Read-only. Prints PASS, WARN, FAIL or UNKNOWN per check, and the fix.
kubectl apply -f scaling-manager-preflight.yaml
kubectl wait -n $NS --for=condition=complete --timeout=180s job/scaling-manager-preflight
kubectl logs -n $NS job/scaling-manager-preflight

# 4. Install.
kubectl create -f scaling-manager-settings.yaml      # once; never overwritten
kubectl apply  -f scaling-manager-namespaced.yaml
kubectl apply  -f scaling-manager-model-namespace.yaml
```

Then register each model with a ScaledObject, as today. Moving from passive to
shadow to active is a `kubectl patch` of the settings ConfigMap
([below](#passive-then-shadow-then-active)).

What changes for the operator: no checkout, no `make`, no `yq`/`jq`/`helm`.
What is kept: the same objects, RBAC, image and behaviour as a `make` install,
rendered from the same kustomize sources.

## What the install does today

`make deploy-scaling-manager` runs `deploy/install.sh`. In order:

1. **Check** (`make check-prereqs`, `install.sh --check`). Read-only. Tools
   (kubectl, helm, git, yq, jq, sometimes openssl and oc), the cluster
   connection, `kubectl auth can-i create` for every kind the install would
   apply, the controller ServiceAccount's own rights by impersonation, the
   single-install guard, KEDA's CRD, the Prometheus it would pick, model server
   and EPP metrics, Kueue. A Forbidden read is reported as "could not check",
   never as "absent".
2. **Prereqs** (`make setup-prereqs`, cluster admin). Namespaces; a
   kube-prometheus-stack when there is no Prometheus; a PodMonitor for the model
   servers and a ServiceMonitor plus scraper ServiceAccount and binding for each
   unscraped EPP, in every managed namespace; EPP `/metrics` authentication;
   KEDA only with `KEDA_HELM_INSTALL=true`, otherwise a CRD check; Gateway and
   inference CRDs when missing; LWS on request; the policy-namespace and Kueue
   reader grants; the scaler's mTLS certificates and KEDA's
   ClusterTriggerAuthentication; then the admin half of the overlay
   (Namespace, ClusterRoles and bindings, Roles, ServiceAccounts, Secrets, the
   ServiceMonitor, the scaler NetworkPolicy). `RENDER=<dir>` writes all this to
   `admin.yaml` and `admin.sh` instead of applying it.
3. **Controller** (namespace admin). The Deployment, the two ConfigMaps
   (`scaling-manager-manager-config`, `scaling-manager-scaling-policy-config`)
   and the Services. The phase is picked automatically: `scaling-manager` when
   the caller cannot create ClusterRoles or the admin half is already there.
4. **After the apply.** Writes the Prometheus settings and the limiter into the
   ConfigMaps, merges the live policy back (below), restarts the controller
   when the Prometheus settings or the passive default changed, optionally
   writes default ScaledObjects (`LLMSM_DEFAULT_SO`, off by default), then
   **verify**: the rollout, the log, the monitors, KEDA, and that KEDA wired the
   external metric onto each HPA. Only the rollout and the wiring fail the run.

What the overlays differ in: a namespaced install drops the cluster-wide
`manager-role` for a namespaced Role (`tenant-installable`), adds
`--watch-namespace`, and keeps narrow ClusterRoles for nodes, Kueue objects and
namespace reads. A cluster-wide install has `manager-role` cluster-wide. Both
carry ClusterRoles, so both start with a cluster admin. OpenShift adds the
`cluster-monitoring-view` binding, the Thanos URL and the service CA.

The release workflow (`ci-release.yaml`) publishes the image only: it runs the
PR checks, refuses a tag off a release line, checks that
`config/base/manager/kustomization.yaml` pins the tag, scans, pushes a
multi-arch image with SBOM and provenance, and signs the digest with keyless
cosign. The GitHub Release is published by hand and carries no files.

The guides (`docs/guides/install-in-namespace`, `install-cluster-wide`,
`try-in-passive-mode`, `utilization-share*`) and the references
(`install-options.md`, about 1,200 lines; `upgrade-and-uninstall.md`, about
1,300) are all written on `make`.

## Release artefacts

Attached to every GitHub Release, rendered by CI from the same overlays the
`make` install applies.

| file | what it holds | who applies it |
| --- | --- | --- |
| `scaling-manager-namespaced.yaml` | Kubernetes, one controller per namespace. Rendered from `config/overlays/namespace-scoped/kubernetes`. Namespace written as `__NAMESPACE__` | cluster admin for the `admin` part, namespace owner for the `controller` part |
| `scaling-manager-cluster.yaml` | Kubernetes, one controller for the cluster, in `scaling-manager-system`. Applies as is | cluster admin |
| `scaling-manager-namespaced-openshift.yaml`, `scaling-manager-cluster-openshift.yaml` | the same for OpenShift (Thanos URL, service CA, `cluster-monitoring-view`) | as above |
| `scaling-manager-settings.yaml` | the settings ConfigMap, with defaults and a comment per key | created once, by the namespace owner |
| `scaling-manager-preflight.yaml` | the preflight Job, its ServiceAccount and its read-only RBAC | whoever installs; the ClusterRole part needs a cluster admin once |
| `scaling-manager-model-namespace.yaml` | per model namespace: the model-server PodMonitor, and for a controller that manages it from elsewhere, its Role and RoleBinding there. Placeholders `__NAMESPACE__` (controller) and `__MODEL_NAMESPACE__` | namespace owner of the model namespace |
| `scaling-manager-activate.yaml` | the only grant of `patch` on pods: Role `scaling-manager-pod-writer` and its RoleBinding to the controller ([spelled out below](#the-activation-file)), applied when utilization share goes active or the warm pool is used, in each namespace where the controller must patch pods (controller namespace, model namespaces). Placeholders as above | owner of that namespace |
| `scaling-manager-keda-auth.yaml` | the ClusterTriggerAuthentication for mTLS. Placeholders `__NAMESPACE__`, `__KEDA_NAMESPACE__` | cluster admin |
| `scaling-manager-kueue.yaml` | optional: the Kueue read grants (ClusterQueues, cohorts, flavors; the Role in Kueue's namespace) | cluster admin, only with a Kueue-bounded quota |
| `scaling-manager-alerts.yaml` | optional: the PrometheusRule (`DEPLOY_ALERTING_RULES` today) | cluster admin (`part=admin`: the stock `admin` role does not grant `monitoring.coreos.com`) |
| `scaling-manager-policy-example.yaml` | an example scaling-policy ConfigMap. Not needed: the controller runs on built-in defaults without one | — |
| `scaling-manager-metrics-reader.yaml` | optional, Kubernetes: the binding that lets the Prometheus scraping the controller read its `/metrics`. Placeholders `__PROMETHEUS_NAMESPACE__`, `__PROMETHEUS_SA__` | cluster admin |
| `SHA256SUMS`, and a `.sigstore.json` bundle per file | checksums and signatures | — |

Rules every rendered manifest follows. A check in `hack/check-release-manifests.sh`,
run by `make lint-deploy-scripts`, asserts each one:

- **The image is pinned by digest**: `ghcr.io/llm-d-incubation/llm-d-scaling-manager@sha256:...`,
  with `imagePullPolicy: IfNotPresent`. Never `:main`, never a tag alone.
- **No object the operator edits.** No settings ConfigMap, no scaling-policy
  ConfigMap. A re-apply can then never reset what an operator changed. This is
  what replaces the re-install merge of PR #59 ([below](#what-the-scripts-did-that-a-manifest-cannot)).
- **No Namespace object.** The operator creates the namespace. So
  `kubectl delete -f` of the same file never deletes a namespace with model
  servers in it.
- **Every object is labelled** `app.kubernetes.io/part-of: scaling-manager`,
  `app.kubernetes.io/instance: <namespace>`, `app.kubernetes.io/version: <tag>`,
  and `scaling-manager.llmd.ai/part: admin|controller`. The `part` label lets
  a cluster admin and a namespace owner split the namespaced file without
  editing it:
  ```bash
  kubectl apply -f scaling-manager-namespaced.yaml -l scaling-manager.llmd.ai/part=admin       # cluster admin
  kubectl apply -f scaling-manager-namespaced.yaml -l scaling-manager.llmd.ai/part=controller  # namespace owner
  ```
  `admin` is the same set of kinds as `LLMSM_PREREQ_KINDS` today.
- **The namespace is a placeholder** in every namespaced file, `__NAMESPACE__`.
  An underscore is not valid in a Kubernetes name, so a file applied without
  the substitution fails at once instead of landing in a wrong namespace.
- **Per-install ClusterRole names carry the namespace**, not today's
  `sha256(namespace)[:8]`: `scaling-manager-node-reader-<namespace>`. A `sed`
  cannot compute a hash. The cluster-wide files keep fixed names, since there
  is one per cluster.

### How CI renders them

A new target, `make release-manifests IMG=<repo>@sha256:<digest> DIST=dist/`,
runs `kustomize build` on each overlay with the image set by digest, applies
the rules above with `yq`, and writes `dist/`. It is the same code path the
kind e2e uses ([Testing](#testing)), so a file that cannot install fails a PR,
not a release.

`ci-release.yaml` gains three steps after "Sign the image":

1. `make release-manifests` with the digest the push step reported.
2. `cosign sign-blob --yes --bundle <file>.sigstore.json <file>` for each file
   and for `SHA256SUMS`. Keyless, same OIDC identity as the image.
3. Upload the files to the release (`gh release upload`, or the REST API),
   creating a draft release when none exists. This needs `contents: write` on
   that one job.

Verifying a file is one command, printed in the run summary next to the image's:

```bash
cosign verify-blob scaling-manager-namespaced.yaml \
  --bundle scaling-manager-namespaced.yaml.sigstore.json \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity https://github.com/llm-d-incubation/llm-d-scaling-manager/.github/workflows/ci-release.yaml@refs/tags/$V
```

Because the manifests carry the digest, the release no longer depends on the
`newTag` pin in `config/base/manager/kustomization.yaml` for operators. The pin
still matters for `git checkout <tag> && make deploy-scaling-manager`, so it
stays (decision 7).

## The settings ConfigMap

One ConfigMap, `scaling-manager-settings`, in the controller's namespace.
**Flat keys**, one per setting, so a change is a `kubectl patch --type merge`
and not an edit of YAML inside a string. It replaces the `config.yaml` blob of
`scaling-manager-manager-config` and the installer variables that wrote it.

It is created with `kubectl create`, never `kubectl apply`, and it is not in
the main manifest. A second `create` fails with `AlreadyExists`, which is the
point: an upgrade never touches it.

| key | default in the file | replaces today | applies |
| --- | --- | --- | --- |
| `prometheusURL` | empty; preflight FAILs until set | `PROMETHEUS_URL` (detected) → `PROMETHEUS_BASE_URL` | restart |
| `prometheusInsecureSkipVerify` | `"false"` | `PROMETHEUS_TLS_INSECURE_SKIP_VERIFY` | restart |
| `passive` | `"true"` | `LLMSM_PASSIVE` (read at start today) | **live** |
| `utilizationShare` | `"off"`; `off`, `shadow` or `active` | `make utilization-share-shadow/-activate/-off` | **live** |
| `limiter` | empty; `gpu-inventory`, `quota`, or both comma-separated | `LLMSM_LIMITER` | live |
| `quotas` | empty; `H200=8,A100=4`, `-1` is no cap | `LLMSM_QUOTAS` | live |
| `quotaScope` | `namespace` | `LLMSM_QUOTA_SCOPE` | live |
| `kueue` | `"false"` | `LLMSM_QUOTA_KUEUE` | live, but needs `scaling-manager-kueue.yaml` |
| `kueueBorrow` | `"true"` | `LLMSM_QUOTA_KUEUE_BORROW` | live |
| `kueueNamespace` | `kueue-system` | `KUEUE_NAMESPACE` | live |
| `scaleToZero` | `"false"` (decision 8) | `ENABLE_SCALE_TO_ZERO` → `LLMSM_SCALE_TO_ZERO` | restart |
| `watchNamespaces` | empty: the controller's own (namespaced) or all (cluster-wide) | `LLMSM_WATCH_NS` | restart, and each namespace needs `scaling-manager-model-namespace.yaml` |
| `logVerbosity` | `"2"` | `LLMSM_VERBOSITY` → `--v` | restart in v0.11.0, live later |

The Prometheus cache settings, `GLOBAL_OPT_INTERVAL` and the `LEARNED_STATE_*`
horizons are advanced: they stay readable from the same ConfigMap under their
current names, are absent from the shipped file, and are documented in
[configuration-advanced.md](../reference/configuration-advanced.md).

**Not settings,** because they are the Deployment itself: the image (pinned in
the manifest; change it by applying another release), replicas, an image pull
secret (`kubectl patch serviceaccount`). Documented as one-line commands.

**How the controller reads it.**

- It watches `scaling-manager-settings` (one name, its own namespace, so the
  Role is narrow).
- A **live** key takes effect within a cycle. `passive`, `utilizationShare` and
  the limiter keys are folded into the **effective cluster `default` policy
  entry**: a key that is set replaces that field of the policy's `default`
  entry; an empty key leaves the policy's value alone. When both are set and
  differ, the setting wins and the controller logs both values once. Per-model
  and tier entries in the scaling-policy ConfigMap still override, as today
  (a model with `passive: false` scales while the controller is passive).
- A **restart** key is read at start. A change sets `restartRequired` in
  `scaling-manager-controller-status` with the key names, and an Event; the
  operator runs `kubectl rollout restart`. The controller does not restart
  itself.
- For one release, a key absent from the settings is read from
  `scaling-manager-manager-config` `config.yaml` if that exists. This is the
  migration path ([below](#migrating-an-install-made-before-v0110)).

The scaling-policy ConfigMap stays as it is, for thresholds, tiers and
per-model overrides. It is optional: with none, the controller uses its
built-in defaults, which equal the shipped `default` entry (a unit test pins
that equality). The release attaches an example to start from.

## What the scripts did that a manifest cannot

Each item, and where it goes. "Preflight" means the check reports it and prints
the command to fix it; nothing is applied for the operator.

| what the scripts do | how it is handled |
| --- | --- |
| **Generate the scaler's mTLS certificates** (openssl, or cert-manager when present; CA, serving leaf, KEDA client leaf; re-issue a leaf near expiry or with wrong SANs) | **Controller-side bootstrap.** At start the controller ensures the three Secrets in its namespace (`scaling-manager-external-scaler-ca`, `-tls`, `-client-tls`, same names and label as today). It creates what is missing, adopts what exists and chains, re-issues the serving leaf 30 days before expiry (it already hot-reloads it with `certwatcher`). The client leaf lives as long as the CA, as today, because KEDA never reloads it. Needs `create`/`get`/`update` on those three Secret names only. cert-manager is no longer used for this |
| **Copy KEDA's client Secret into KEDA's namespace and write the ClusterTriggerAuthentication** | **Documented admin step**, once per install: copy the client Secret into KEDA's namespace (the guide gives the command, which keeps only the data keys) and `kubectl apply -f scaling-manager-keda-auth.yaml`. The second file also writes the marker ConfigMap `scaling-manager-scaler-keda-auth` in the controller's namespace. Preflight prints both commands with the namespaces filled in |
| **Serve mTLS only when KEDA's side is ready, else plaintext with a warning** | **Controller-side**, at start: mTLS when the marker ConfigMap exists, plaintext with a warning otherwise, as the installer decides today. Applying the KEDA step therefore needs one restart of the controller, which the guide says |
| **Pick the Prometheus URL** (Thanos on OpenShift; else the Prometheus resource whose monitor selectors cover the model namespace, and a Service its certificate names) | **Preflight.** It runs the same selection, prints the URL it would choose and the `kubectl patch` that sets it, and checks the one that is set. The OpenShift manifests carry the Thanos URL already |
| **Copy the Prometheus CA into the controller's namespace and mount it** | The Deployment always mounts Secret `scaling-manager-prometheus-ca` with `optional: true`. The operator creates it with one `kubectl create secret generic ... --from-file=ca.crt`; preflight finds the CA the Prometheus serves with and prints that command. OpenShift uses the service CA, as today |
| **Merge the live policy on re-install** (PR #59: keep unrendered `default` keys, `optimizer`, `passive`, limiters and other entries) | **Not needed.** The policy ConfigMap is not in any manifest, so an apply never overwrites it. The shipped `default` entry becomes the built-in default |
| **Restart on a passive or Prometheus change** | `passive` becomes live. A Prometheus change sets `restartRequired` (above) |
| **Write the limiter into the policy** (`LLMSM_LIMITER`, `LLMSM_QUOTAS`, the Kueue keys) | Settings keys, live |
| **Name ClusterRoles per install** with a hash of the namespace | The name carries the namespace instead (rules above) |
| **Narrow `resourceNames`** on the Kueue and namespace readers to the managed namespaces | Static: the namespaced file names `__NAMESPACE__` and `scaling-manager-policy`. A controller that manages more namespaces (`watchNamespaces`) needs them added, which `scaling-manager-model-namespace.yaml` cannot do since they are on a ClusterRole. Preflight reports the missing names and prints the patch (decision 5) |
| **Create PodMonitors and EPP ServiceMonitors, copying the selector labels the Prometheus resource wants** | `scaling-manager-model-namespace.yaml` carries the model-server PodMonitor. Preflight reads the Prometheus resource and says which label the monitor needs, and whether the targets are `up`. EPP scraping and EPP `/metrics` authentication belong to the llm-d stack; preflight checks the EPP metrics are present and links the llm-d guide |
| **Discover the namespace** (the one namespace with model servers) and `LLMSM_WATCH_NS=auto` | Preflight lists namespaces with model servers and whether another controller manages them. The operator names the namespace |
| **Single-install guard** (no namespaced install beside a cluster-wide one) | Preflight FAILs on an overlap it can see, and says UNKNOWN when it may not list Deployments cluster-wide |
| **KEDA version branching** (warn outside tested versions; omit `fallback.behavior` from ScaledObjects before KEDA 2.17; re-wire an unwired HPA) | The manifests create no ScaledObject, so there is nothing to branch. Preflight reports the KEDA version, FAILs below the minimum we test, WARNs on an untested one, and says whether `fallback.behavior` exists. The ScaledObject example in the guide uses no `fallback` |
| **NetworkPolicy naming KEDA's namespace** (detected from the external-metrics APIService) | In the manifest with `keda`, `openshift-keda` on OpenShift (decision 6). Preflight reads KEDA's namespace and FAILs when the policy names another, with the patch. With mTLS, only KEDA can call the scaler whatever the policy says |
| **Install Prometheus, KEDA, LWS, Gateway and inference CRDs** | Prerequisites, not part of the install. Preflight checks each. `make` keeps installing them for kind and CI |
| **Pod-writer grant at `utilization-share activate`** (Role and RoleBinding in each model namespace) | Still granted only at activation (decision 3): `scaling-manager-activate.yaml`, applied per model namespace when going active ([the activation file](#the-activation-file)). The install files never grant `patch` on pods |
| **Publish cluster policy to a policy namespace** (`make enable-physical-limiter`) | Unchanged in v0.11.0: a cluster-admin `make` target. A manifest form later |
| **Default ScaledObjects** (`LLMSM_DEFAULT_SO=plan`) | Unchanged: off by default today, and `make scaledobjects-plan` stays. The guide shows the one ScaledObject a model needs |
| **Verify after install** (rollout, log, monitors, KEDA wiring) | The preflight image's second mode, `verify`, after the install: the same Job file with `args: [verify]` ([Preflight](#the-preflight-check)) |

## The preflight check

A subcommand of the controller binary, `manager preflight`, so it ships in the
release image and needs nothing else. The release attaches a Job that runs it.

**Two modes.**

- `check` (default), before the install: prerequisites.
- `verify`, after it: the controller is Ready, it read its settings, the
  scaler serves the protocol it should, and KEDA wired the external metric
  onto each ScaledObject's HPA (the "unwired" case `make verify-scaledobjects`
  finds today).

**What `check` checks.**

| check | FAIL when | UNKNOWN when |
| --- | --- | --- |
| KEDA | the `scaledobjects.keda.sh` CRD is absent, or the version is below the minimum | KEDA's Deployment cannot be read |
| KEDA mTLS step | — (WARN when the marker ConfigMap is absent: plaintext) | |
| Prometheus reachable | `prometheusURL` unset, refused, or its certificate fails against the mounted CA | the Pod cannot be created |
| Model metrics present | no `vllm:` series for any pod in a managed namespace | the query is denied |
| PodMonitor port | the PodMonitor names a port no model-server container has, or its targets are not `up` | PodMonitors cannot be read |
| Prometheus picks the monitor | the Prometheus resource's selectors do not select the PodMonitor | Prometheus resources cannot be read |
| Permissions | the controller's ServiceAccount lacks a verb the controller needs, by `SubjectAccessReview` per verb and resource | SubjectAccessReview is refused |
| Settings | a key does not parse (a bad quota, an unknown mode, `utilizationShare` without a limiter) | |
| Kueue (when `kueue: "true"`) | the Kueue CRDs are absent | they cannot be read |
| Overlap | a cluster-wide controller already manages this namespace | Deployments cannot be listed cluster-wide |
| Accelerator names (with a quota) | — (WARN when a quota key matches no node's accelerator label) | nodes cannot be read |

**Forbidden is not absent.** A refused read is UNKNOWN, with the object and the
verb that was refused, never PASS and never "not installed". Only a NotFound is
"absent". This is the rule the scripts follow today, kept as is.

**How it reports.** One line per check, then a summary, in the Job's log:

```text
FAIL  prometheus.reachable   https://prometheus-k8s.monitoring.svc:9091: x509: certificate signed by unknown authority
      fix: kubectl create secret generic scaling-manager-prometheus-ca -n team-a --from-file=ca.crt=<file>
UNKNOWN keda.version         cannot get deployments in keda: forbidden (needs a cluster admin's run)
PASS  keda.crd               scaledobjects.keda.sh v1alpha1
3 PASS, 1 WARN, 1 FAIL, 1 UNKNOWN
```

The Job exits 1 on any FAIL, 0 otherwise, so `kubectl wait --for=condition=complete`
and `--for=condition=failed` both work. UNKNOWN does not fail the Job, and the
summary says that an UNKNOWN is not a pass. The same lines are written as JSON
to the ConfigMap `scaling-manager-preflight-result`, for a script or a GitOps
health check to read.

**What it needs.** Its own ServiceAccount, `scaling-manager-preflight`, never
the controller's, so a preflight grant never widens the controller:

- a Role in the controller's namespace: get the settings ConfigMap, the
  Prometheus CA Secret and the TLS marker ConfigMap by name, and create and
  update `scaling-manager-preflight-result`. The reachability probe runs in the
  Job's own Pod, so it creates no Pod;
- a ClusterRole, read-only: `get`/`list` on CRDs, Deployments, PodMonitors,
  Prometheus resources, nodes, namespaces, Kueue objects; `create` on
  `subjectaccessreviews` (it answers questions, it grants nothing).

The ClusterRole is the `admin` part of the preflight file. A namespace owner
without it still gets a useful run: every cluster read shows UNKNOWN, and the
namespaced checks (settings, Prometheus, metrics, PodMonitor) still run. The
guide says to delete the preflight objects after the install if the cluster
admin wants them gone; they hold no secret.

Run from inside the cluster, the Prometheus check sees what the controller
will see: the same network, NetworkPolicies and DNS. A laptop check cannot.

## Passive, then shadow, then active

The shipped settings say `passive: "true"`, `utilizationShare: "off"`. A first
apply gives a controller that decides, logs and publishes, and changes no
replica count.

```bash
# Before sharing: declare the budget sharing will divide.
kubectl patch configmap scaling-manager-settings -n $NS --type merge \
  -p '{"data":{"limiter":"quota","quotas":"H200=8"}}'

# 1. Passive, sharing in shadow: it reports the moves it would make.
kubectl patch configmap scaling-manager-settings -n $NS --type merge \
  -p '{"data":{"utilizationShare":"shadow"}}'

# 2. Let it scale, sharing still in shadow.
kubectl patch configmap scaling-manager-settings -n $NS --type merge \
  -p '{"data":{"passive":"false"}}'

# 3. Let sharing act: first the pod-write grant, in every model namespace that shares,
#    then the switch.
kubectl apply -f scaling-manager-activate.yaml     # per model namespace, see below
kubectl patch configmap scaling-manager-settings -n $NS --type merge \
  -p '{"data":{"utilizationShare":"active"}}'
```

Each change is live. To confirm the controller took it, read
`scaling-manager-controller-status` (`passiveDefault`, `utilizationShareMode`,
`settingsResourceVersion`), as `deploy/utilization-share.sh` does today. Going
back is the same patch the other way. `utilizationShare: "off"` from `active`
goes through shadow first and clears the transfer marks before it drops the
block, the order `make utilization-share-off` follows today; the controller
does it, and reports `utilizationShareMode: draining` until it is done.

`make utilization-share-*` stays, as a wrapper over the same patch plus the
checks it runs.

### The activation file

`scaling-manager-activate.yaml` is the one RBAC change going active needs, and
the only grant of `patch` on pods in the release files: the warm pool needs it
too, in the namespaces it lends pods in. It is the grant
`deploy/lib/pod_writer.sh` makes today at `make utilization-share-activate`,
as a file. Applied in each namespace where the controller must patch pods
(each model namespace that shares the budget, the controller namespace for a
warm pool there), after `sed` fills both placeholders:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: scaling-manager-pod-writer
  namespace: __MODEL_NAMESPACE__
  labels:
    app.kubernetes.io/part-of: scaling-manager
rules:
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: scaling-manager-pod-writer-__NAMESPACE__
  namespace: __MODEL_NAMESPACE__
  labels:
    app.kubernetes.io/part-of: scaling-manager
    app.kubernetes.io/instance: __NAMESPACE__
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: scaling-manager-pod-writer
subjects:
- kind: ServiceAccount
  name: scaling-manager-controller-manager
  namespace: __NAMESPACE__
```

Why a file and not part of the install: `patch` on pods can change a live Pod's
image, so it is granted only where sharing acts, and only once it does. The
Role is the same in every namespace; the RoleBinding is named for the
controller's namespace, so two controllers granted in one namespace keep their
own subjects, as today (today's name carries a hash of the namespace; see
[naming](#release-artefacts)).

Preflight checks for it: with `utilizationShare: active` set and the grant
missing in a sharing namespace, it FAILs and prints the `kubectl apply` line.

Turning sharing off: set `utilizationShare: "off"`, wait for the status to leave
`draining`, then `kubectl delete -f scaling-manager-activate.yaml` in each
namespace. Deleting the grant first would leave transfer marks on pods the
controller can no longer clear.

## Upgrade

```bash
# 1. Download the new release's files and substitute the namespace, as above.
# 2. Check against the new image.
kubectl delete job scaling-manager-preflight -n $NS --ignore-not-found
kubectl apply -f scaling-manager-preflight.yaml   # the new one
# 3. Apply. Settings and policy are untouched: neither is in the file.
kubectl apply -f scaling-manager-namespaced.yaml
# 4. Verify.
```

The release notes list, per release: new settings keys and their defaults,
objects removed (to delete by hand, by name), and any key that changed meaning.
`kubectl apply --prune` is not used; it is easy to aim at the wrong objects.

Rollback is applying the older release's file. Settings keys the older
controller does not know are ignored.

## Uninstall

```bash
kubectl delete -f scaling-manager-namespaced.yaml --ignore-not-found
kubectl delete -f scaling-manager-model-namespace.yaml --ignore-not-found   # per model namespace
kubectl delete -f scaling-manager-activate.yaml --ignore-not-found          # per model namespace, if active
# What the controller and the operator wrote, by label:
kubectl delete configmap,secret -n $NS -l app.kubernetes.io/part-of=scaling-manager
```

Because no file holds a Namespace, `delete -f` is the inverse of `apply -f`.
Every object the controller creates (status, history and learned-state
ConfigMaps, the scaler TLS Secrets) carries the `part-of` label so the last
line finds them. The settings file carries it too. ScaledObjects are the
operator's and stay; the uninstall guide keeps today's warning that a model
parked at zero then never wakes. Set `utilizationShare: shadow` and wait a
cycle before uninstalling, as today, so no transfer marks are left on pods.

## Two install paths

Both are supported, documented and tested (decision 1).

| | release files | `make deploy-scaling-manager` |
| --- | --- | --- |
| for | operators who install a release | users who build from source, and those who want the scripted extras |
| needs | `kubectl`, the downloaded files | a checkout, `make`, `kubectl`, `yq`, `jq`, `helm`, sometimes `openssl` |
| finds Prometheus, CA, KEDA namespace, monitor labels | no: preflight reports, the operator sets | yes, as today |
| installs Prometheus, KEDA, LWS, CRDs on request | no | yes, as today |
| renders the admin half for someone else (`RENDER=`) | not needed: the `part=admin` objects of the file | yes |

**How they relate.** They must produce the same install, so an operator can
move between them in either direction:

- **Same sources.** In v0.11.0 `make deploy-scaling-manager` stops building its
  own kustomization for the core objects. It renders with the same code as
  `make release-manifests` (its image instead of the release digest), fills
  `__NAMESPACE__`, and applies that. Its detection, prerequisite installs and
  checks run around this apply, as today. One render, so the two cannot drift.
- **Same names.** `make` moves to the namespace-in-the-name ClusterRoles and
  pod-writer bindings in the same release, and deletes the hash-named ones it
  made before ([Migrating](#migrating-an-install-made-before-v0110)).
- **Same settings ConfigMap.** `make` writes `scaling-manager-settings` instead
  of `config.yaml`. It creates it when absent; when present, it merge-patches
  only the keys whose variables were passed on this run (`PROMETHEUS_URL` →
  `prometheusURL`, `LLMSM_PASSIVE` → `passive`, `LLMSM_LIMITER` → `limiter`,
  and so on, per the settings table). A key not passed keeps its live value,
  the rule `LLMSM_PASSIVE` and `LLMSM_LIMITER` follow today. Like the release
  files, it never applies the scaling-policy ConfigMap, so the PR #59 merge is
  retired from `make` too.
- **Switching.** From `make` to the release files: apply the release file for
  the same namespace; the objects already have those names. From the release
  files to `make`: run `make deploy-scaling-manager` with the same namespace;
  it finds the settings and keeps them. The uninstall of either path removes an
  install made by the other.

**What the guides lead with.** The release files. Each install guide ends with
"Installing from a checkout instead" naming the `make` command for the same
result, and `install-options.md` stays the reference for the `make` path.

**What else `make` keeps**, unchanged:

- Development and CI: `deploy-e2e-infra`, every `test-e2e-*`, kind with
  emulated GPUs, the benchmark targets.
- The release: `release-manifests`, and the checks on it.
- Tooling the manifests do not cover: `scaledobjects-plan`/`-apply`,
  `enable-physical-limiter`, `prepull`, `weights`, `warmpool`,
  `workload-patch`, `utilization-share-*` (as wrappers over the settings
  patch and the activation file).

## Migrating an install made before v0.11.0

An existing install keeps working; nothing forces a move. Re-running
`make deploy-scaling-manager` from v0.11.0 does the steps below itself. To move
to the release files by hand:

1. Create the settings ConfigMap. Preflight's `check` prints a filled-in
   `scaling-manager-settings.yaml` from the live
   `scaling-manager-manager-config` and policy `default` entry (Prometheus
   URL, passive, limiter, quotas, Kueue keys, scale-to-zero), for the operator
   to read and `kubectl create`. Until then the controller still reads
   `config.yaml` (above), so there is no gap.
2. Apply the release manifest. The scaling-policy ConfigMap, the scaler TLS
   Secrets and the KEDA ClusterTriggerAuthentication keep their names, so the
   controller adopts them and KEDA's connection is not broken.
3. Delete what changed name: the ClusterRoles and ClusterRoleBindings named
   with the hash suffix. Preflight lists them by name with the `kubectl delete`
   command; the hash is recomputed from the namespace, so only this install's
   are listed. Deleting them before step 2 would leave the controller with no
   rights for a moment, so the order matters, and the guide says so.
4. Delete `scaling-manager-manager-config` once the controller reports it read
   every key from the settings (`settingsSource: settings` in the status
   ConfigMap).

`make undeploy-scaling-manager` is **not** a migration step: it deletes the
policy ConfigMap and the ScaledObjects the installer created.

## Testing

- **A kind e2e that installs from the rendered files**, `make
  test-e2e-release-manifests`, in CI on every PR that touches `config/`,
  `cmd/` or `deploy/`. It runs `make release-manifests` with the image the job
  built, installs only the prerequisites (KEDA, Prometheus, the emulated model
  servers) with the existing e2e setup, then installs the scaling manager with
  `kubectl` alone, in a step that cannot reach `deploy/`. It runs:
  - the preflight Job, expecting PASS;
  - a negative control: a wrong `prometheusURL`, expecting the Job to fail
    with `prometheus.reachable` named;
  - the smoke suite against that install, with `ginkgo -v` and a check that
    the spec count is above zero;
  - passive → shadow → active by `kubectl patch`, asserting the status
    ConfigMap after each step and that no replica count moved while passive;
  - an upgrade: apply the previous release's file, then this one, and assert
    the settings and policy are unchanged.
- **The `make` path keeps its coverage.** The existing e2e jobs
  (`test-e2e-smoke-with-setup`, `test-e2e-full-with-setup`, the passive,
  multi-controller and Kueue suites) install through
  `make deploy-scaling-manager`, as today, and stay. Two additions:
  - the deploy-script fixture checks (`hack/check-passive-keep.sh` and the
    rest under `make lint-deploy-scripts`) move to the settings ConfigMap: a
    re-run keeps every key it was not given, and never applies the policy;
  - **path equivalence:** render the namespaced install both ways for the
    same namespace and image, and assert the two object sets are identical.
    Then, on kind, install with
    `make`, apply the release file over it, and assert nothing changed; and
    the reverse.
- **`hack/check-release-manifests.sh`**, in `make lint-deploy-scripts`: the
  rules above (digest, no operator ConfigMaps, no Namespace, labels, the
  placeholder, the `part` split matching `LLMSM_PREREQ_KINDS`).
- **Unit tests** for the settings reader (each key, live vs restart, the
  precedence over the policy `default` entry, the `config.yaml` fallback), the
  TLS bootstrap (create, adopt, re-issue, refuse an unlabelled Secret), and
  every preflight check including its UNKNOWN path.

## Phases

**v0.11.0: install without make.**

1. `make release-manifests`, the rules check, and the release workflow
   attaching and signing the files.
2. The settings ConfigMap and its reader, with `passive`, `utilizationShare`
   and the limiter keys live; the `config.yaml` fallback.
3. The scaler TLS bootstrap in the controller, and the marker-driven choice
   of mTLS or plaintext.
4. The preflight Job, `check` mode.
5. The kind e2e from the rendered files.
6. `make deploy-scaling-manager` on the same render, names and settings
   ConfigMap, with the path-equivalence test.
7. Docs: `install-in-namespace`, `install-cluster-wide` and
   `try-in-passive-mode` rewritten on the files; a short "install from the
   release" page that replaces most of `install-methods.md`;
   `upgrade-and-uninstall.md` gains the manifest form first.

**Later.**

- Preflight `verify` mode (until then the guide gives the three `kubectl`
  commands that check the rollout and the HPA wiring).
- `restartRequired` in the status; `logVerbosity` live.
- Migration help in preflight (the generated settings file, the old ClusterRole
  list). Until then the migration section lists the commands by hand.
- `enable-physical-limiter` as a manifest.
- The `utilization-share*` guides, the Kueue guide and `install-options.md`
  rewritten on the files.

## Found while building phase 1

Building the render showed where the text above was wrong or incomplete.

- **The overlay's namespaced manager Role carries `patch` on pods**, for the
  warm pool (`config/components/tenant-installable`), and `make` copies it
  into each `LLMSM_WATCH_NS` namespace. Copied as is, the model-namespace file
  would have granted what the activation file grants. Decided 2026-11-11:
  decision 3 holds, and the release files split it out. Their manager Role,
  in the controller namespace and in each model namespace, reads pods (get,
  list, watch) and never patches them; `scaling-manager-activate.yaml` is the
  only grant of `patch` on pods, applied to turn utilization share active *or*
  to use the warm pool, in each namespace where the controller must patch
  pods (the controller namespace, a model namespace, or both). The check
  fails on `patch` on pods anywhere else. The `make` path keeps the overlay
  Role, `patch` included, until it renders through the same code (phase 6).
- **The OpenShift Thanos URL was in `scaling-manager-manager-config`**, which
  no release file carries. The OpenShift manifests therefore do not carry it;
  the settings file names it in the comment on the `prometheusURL` key, and the
  OpenShift guide must set it (or a later release ships an OpenShift settings
  file).
- **The controller cannot start from the release files until the settings
  reader exists (phase 2).** Without the ConfigMap the Deployment's config
  volume is `optional`, and the kubelet then presents `config.yaml` as an empty
  directory: today's controller exits with `read /etc/scaling-manager/config.yaml:
  is a directory` (seen on kind). The reader must treat a missing or directory
  config file as absent. When `scaling-manager-manager-config` does exist (a
  migrated install), it is still mounted and read, which is the fallback.
- **Placeholders beyond `__NAMESPACE__`.** The model-namespace and activation
  files need `__MODEL_NAMESPACE__`, the KEDA file `__KEDA_NAMESPACE__`, the
  Kueue file `__KUEUE_NAMESPACE__`. The single `sed` loop in the
  [Summary](#summary) fills only the first, so the guides need the others.
- **The Kueue file holds the borrowing read too** (Workloads and LocalQueues
  in every namespace), since `kueueBorrow` defaults to `"true"`, and the Role
  in Kueue's namespace. Its ClusterRoles are redundant for a cluster-wide
  controller, whose manager ClusterRole has the same reads.
- **No Namespace object costs the OpenShift cluster-wide install a label.**
  The overlay put `openshift.io/user-monitoring: "true"` on the Namespace it
  created; the release files create none, so the guide must label it.
- **The cluster-wide files keep fixed names, and `make` hashes those too.** A
  cluster-wide install made with `make` has hash-suffixed ClusterRoles, so the
  migration's step 3 applies to it as well.
- **The alerts rule is named `controller-manager-alerts`**, unprefixed,
  because the alerts component is added outside the overlay's `namePrefix`,
  as the installer adds it.
- **The base binds the controller's `/metrics` to a fixed account**
  (`kube-prometheus-stack-prometheus` in `scaling-manager-monitoring`). The
  installer binds only an account a trusted source names, so the Kubernetes
  release files ship the metrics-reader ClusterRole without that binding, and
  `scaling-manager-metrics-reader.yaml` holds it with the account as
  placeholders. On OpenShift the binding names the install's own
  `metrics-scraper` account and stays in the main file.
- **A watched namespace needs more than `scaling-manager-model-namespace.yaml`.**
  The policy-namespace reader and the Kueue reader name only the controller's
  namespace (and `scaling-manager-policy`). Phase 2 must make
  `watchNamespaces` extend them: preflight prints the `kubectl patch` that adds
  each name, per decision 5. Until then the settings file's comment says so.
- **The files are a preview until phase 2 lands.** Every file opens with a
  `PREVIEW:` line and the draft release notes say the same, naming
  `make deploy-scaling-manager` as the supported install
  (`PREVIEW_NOTE` in `deploy/release-manifests.sh`; drop it, and its assertion
  in the check, with phase 2).
- **A published release's files are never replaced.** A re-run of the release
  job that renders different files for a published release is refused; with
  GitHub immutable releases on, a published release refuses uploads anyway.

## Found while building phase 3

- **The bootstrap is a flag the release files set,**
  `--external-scaler-tls-bootstrap`, not something every controller does. A
  `make` install provisions the certificates from the installer (openssl or
  cert-manager) and passes the certificate files; the flag refuses those. So
  the `make` path changes nothing and gets no Secret grant, until phase 6
  renders it through the same code. The grant is a Role of the release files
  only, `scaling-manager-scaler-tls-role`.
- **`create` on Secrets cannot be narrowed by name.** Kubernetes authorizes a
  create before the object has a name, so the Role grants `create` on any
  Secret in the controller's namespace, beside `get` and `update` on the three
  by name. It still reads no other Secret.
- **A CA Secret that exists is never replaced,** not even an expiring, expired
  or unreadable one, where the installer re-mints a CA within 30 days of its
  expiry. KEDA keeps the CA of its first connection until its operator
  restarts (PR #70), so the controller reports it in `scalerTLSAction` with the
  command that rotates it, and the operator runs it.
- **cert-manager's Secrets are left alone.** When any of the three is
  cert-manager's (its `cert-manager.io/certificate-name` annotation, an owner
  reference to a Certificate, or the installer's `cert-manager` label value),
  the controller writes none of them and serves what cert-manager issued. An
  unlabelled Secret under one of the names is never written either.
- **The mutual-TLS decision is made once, at start**, from the marker, as the
  proposal says. The controller verifies clients with the CA it started with:
  a renewed serving certificate from that CA is served from the next
  handshake, but one from a new CA is not, and the status asks for a restart.
- **The controller cannot see whether KEDA was wired.** After it mints a CA or
  issues KEDA's certificate again, `scalerTLSAction` asks for the copy until a
  handshake presents the new client certificate.
- **Secrets the controller writes carry `app.kubernetes.io/part-of`**, so the
  uninstall by label finds them. Secrets a `make` install wrote do not, and
  are adopted unchanged; `make undeploy-scaling-manager` removes those.

## Open questions

None. All eight were decided on 2026-11-10; see
[Decisions](#decisions-2026-11-10).

## Before you rely on this

Assumptions the design rests on:

- **`kubectl apply -f <file> -l <selector>` applies only the matching objects
  of a file.** Verified with kubectl v1.31 on kind (2026-11-11): `-l
  scaling-manager.llmd.ai/part=admin` created the 25 admin objects of the
  namespaced file and no Deployment or Service; `part=controller` then
  created exactly the Deployment and the two Services; a whole-file apply after
  both changed nothing. The namespaced release stays one file.
- **The migration keeps names.** It works only if the TLS bootstrap uses
  today's Secret names, and the KEDA step today's ClusterTriggerAuthentication
  name, both of which this proposal requires.
- **The built-in policy defaults equal the shipped `default` entry.** The
  thresholds were checked by reading the code; the unit test that pins the
  whole entry does not exist yet.
