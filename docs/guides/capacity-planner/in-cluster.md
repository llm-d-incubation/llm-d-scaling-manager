# Capacity planner: run it in a cluster

Run one planner in a namespace and let people open it from their browser.
They need nothing but `kubectl` (or an OpenShift login). The planner serves
the recordings you copy into it, and it never changes the cluster.

## Quick start

On plain Kubernetes, as the admin of one namespace, with `kubectl` 1.27 or
later, `git` and a default StorageClass. About two minutes.

<!-- guide:cluster.install start -->
```bash
# Plain Kubernetes, as the admin of one namespace. The planner listens on 127.0.0.1 inside its Pod; a Role and a
# RoleBinding let every authenticated user port-forward to that Pod, capacity-planner-0, and no other.
# Skip the create if your cluster admin made the namespace.
export NS=capacity-planner V=<release, v0.12.0 or later>
export SRC=https://github.com/llm-d-incubation/llm-d-scaling-manager//config/capacity-planner
kubectl create namespace ${NS}
kubectl apply -n ${NS} -k "${SRC}/kubernetes?ref=${V}"
kubectl -n ${NS} rollout status statefulset/capacity-planner
```
<!-- guide:cluster.install end -->

<!-- guide:cluster.sample start -->
```bash
# Load the sample through the loader, a short-lived Pod that mounts the recordings volume read-write.
# Your own recordings go in the same way.
curl -fsSL -o ttft-staircase-b-on.jsonl.gz \
  https://raw.githubusercontent.com/llm-d-incubation/llm-d-scaling-manager/${V}/internal/engines/analyzers/learnreplay/testdata/goldens/ttft-staircase-b-on/recording.jsonl.gz
kubectl apply -n ${NS} -k "${SRC}/loader?ref=${V}"
kubectl -n ${NS} wait --for=condition=Ready pod/capacity-planner-loader --timeout=120s
kubectl -n ${NS} cp ttft-staircase-b-on.jsonl.gz capacity-planner-loader:/recordings/ttft-staircase-b-on.jsonl.gz
kubectl -n ${NS} delete pod capacity-planner-loader
```
<!-- guide:cluster.sample end -->

Then everyone who opens it runs, on their own machine:

<!-- guide:cluster.open start -->
```bash
# Anyone allowed, from their own machine. Leave it running, then open http://localhost:8080. Use localhost or
# 127.0.0.1: the planner refuses other names. If port 8080 is busy, use 9090:8080 and open http://localhost:9090.
kubectl -n <namespace> port-forward pod/capacity-planner-0 8080:8080
```
<!-- guide:cluster.open end -->

In **Source**, pick `ttft-staircase-b-on.jsonl.gz` and press **Load**.
[Read the planner's answer](read-the-answer.md) walks through what it says.

On OpenShift, the planner sits behind a Route and the cluster's login
instead: install it as in [OpenShift](#openshift), load recordings the same
way with `oc` and the `openshift/loader` overlay, and open the Route's address ([Open it](#open-it)).

The rest of this page is the detail: who may do what, the prerequisites in
full, letting others load or upload recordings, narrowing who can open it,
hiding real names, upgrading and removing it.

## Who does what

| Step | Who | How often |
| --- | --- | --- |
| [Install it](#install-it) | an admin of one namespace; no cluster admin needed | once, and again on each upgrade |
| [Load recordings](#load-recordings) | anyone who may create Pods in that namespace: its admin, or a user with `edit` there; or, [if the admin turns it on](#or-let-people-upload-from-the-page), anyone who can open it | each time there is new data |
| [Open it](#open-it) | anyone you allow: by default, everyone who can log in to the cluster | any time |

- **Plain Kubernetes.** Nothing is exposed. Each person runs
  `kubectl port-forward`, and Kubernetes RBAC decides who may.
- **OpenShift.** A Route behind the cluster's login (and so its SSO).

## Install it

For the namespace admin.

### Prerequisites

- [ ] Admin of a namespace that holds nothing but the planner. You create it
  below, or your cluster admin creates it for you.
- [ ] `kubectl` 1.27 or later (or `oc`), and `git`: `kubectl apply -k` fetches
  the manifests from GitHub.
- [ ] One CPU free on a node: the planner requests one, for loading
  recordings ([Long recordings load slowly](#long-recordings-load-slowly)).
- [ ] A default StorageClass, for the planner's 1 GiB recordings volume. The
  loader writes it as uid 65532, so the StorageClass must apply `fsGroup`, or
  the volume root must be writable by that uid.

```bash
# The namespace, and the release: v0.12.0 or later.
export NS=capacity-planner V=<release, v0.12.0 or later>
export SRC=https://github.com/llm-d-incubation/llm-d-scaling-manager//config/capacity-planner
```

### Plain Kubernetes

```bash
# Skip the first command if your cluster admin made the namespace. The planner listens on 127.0.0.1 inside its Pod;
# a Role and a RoleBinding let every authenticated user port-forward to that Pod, capacity-planner-0, and no other.
kubectl create namespace ${NS}
kubectl apply -n ${NS} -k "${SRC}/kubernetes?ref=${V}"
kubectl -n ${NS} rollout status statefulset/capacity-planner
```

To let only one group open it, see [Narrow who can open it](#narrow-who-can-open-it).

### OpenShift

The Route sends people to the cluster's login page. After login, an
oauth-proxy sidecar forwards them to the planner on the Pod's 127.0.0.1. The
proxy image is Red Hat's, from `registry.redhat.io`, which the cluster's
global pull secret covers.

```bash
# Skip the first command if your cluster admin made the namespace. The secret signs the proxy's session cookie.
oc new-project ${NS}
oc -n ${NS} create secret generic capacity-planner-proxy --from-literal=session_secret="$(openssl rand -hex 16)"
# Any user who can log in to the cluster may open it. Use openshift-sar in place of openshift to narrow that (below).
oc apply -n ${NS} -k "${SRC}/openshift?ref=${V}"
oc -n ${NS} rollout status statefulset/capacity-planner
```

The Route and the oauth-proxy give up after 30 s, and the first load of a
day-long recording takes minutes: raise both as in
[Long recordings load slowly](#long-recordings-load-slowly).

### Long recordings load slowly

The shipped limit, 2 GiB, is enough for day-long recordings. The planner reads
a recording from disk in passes when someone first picks it, and keeps only the
state it needs. Measured on a 24-hour recording of a 16-replica P/D fleet
(95 MB compressed): under 80 MiB while it loads and after.

- **Time.** The first load of a day-long recording takes several minutes, and
  about twice as long on a busy node; asking for the experimental models adds
  more. Picking another cycle of the same recording loads it again. Wait for
  the first load; asking again starts a second one.
- **CPU.** A load replays the recording on one core, so the planner requests
  one CPU. Do not lower it: on a busy node a load with a tenth of a core takes
  ten times as long. There is no CPU limit, so spare cores still serve plans.
- **Deadline.** The planner lets a request run for 30 minutes once it has
  arrived; a load still running then is cut off.

On OpenShift, the Route and the oauth-proxy each stop waiting for an answer
after 30 s, so a long load fails there unless you raise both. These patches
were rendered with `kubectl kustomize`, not run on a live OpenShift cluster.

```bash
# Keep the directory, and apply it again after each upgrade, with the new V.
# Put any other patch (upload, experimental models, openshift-sar) in this same file.
cd "$(mktemp -d)"
cat > kustomization.yaml <<EOF
resources:
  - ${SRC}/openshift?ref=${V}
patches:
  - target:
      kind: Route
      name: capacity-planner
    patch: |-
      - op: add
        path: /metadata/annotations
        value: {haproxy.router.openshift.io/timeout: 31m}
  - target:
      kind: StatefulSet
      name: capacity-planner
    patch: |-
      - op: test
        path: /spec/template/spec/containers/1/name
        value: oauth-proxy
      - op: add
        path: /spec/template/spec/containers/1/args/-
        value: --upstream-timeout=31m
EOF
oc apply -n ${NS} -k .
```

Both wait a minute past the planner's 30, so the planner's own answer, or
its error, arrives first.

### Let someone else load recordings

Loading needs Pod create in the namespace. To let a user do it without making
them its admin:

```bash
kubectl -n ${NS} create rolebinding capacity-planner-loaders --clusterrole=edit --user=<user>
```

`edit` also lets them read the recordings volume. Give it only to people who
may see every recording in the namespace.

## Load recordings

For whoever loads data. A recording comes from
[Record a window](../record-for-capacity-planner/). Name each file after what
it holds, such as `qwen3-32b.jsonl.gz`: people pick a recording by its file
name. If the people who open the planner should not see your namespace, model
and Pod names, load an exported copy instead
([Real names in recordings](#real-names-in-recordings)).

```bash
# Use oc and the openshift/loader overlay on OpenShift. The loader is a short-lived Pod that mounts the recordings
# volume read-write; the planner sees new files without a restart. Copy as many files as you like in one go.
kubectl apply -n ${NS} -k "${SRC}/loader?ref=${V}"
kubectl -n ${NS} wait --for=condition=Ready pod/capacity-planner-loader
kubectl -n ${NS} cp qwen3-32b.jsonl.gz capacity-planner-loader:/recordings/qwen3-32b.jsonl.gz
kubectl -n ${NS} exec capacity-planner-loader -- ls -l /recordings
kubectl -n ${NS} delete pod capacity-planner-loader
```

- **Replace a recording:** copy a file of the same name over it.
- **Remove one:** `kubectl -n ${NS} exec capacity-planner-loader -- rm /recordings/<file>`
  while the loader runs.
- **Permission denied on `kubectl cp`:** the StorageClass ignores `fsGroup`
  (`kubectl get csidriver <driver> -o jsonpath='{.spec.fsGroupPolicy}'`). Ask
  your admin for one that applies it, or a volume whose root uid 65532 can
  write.
- The loader stops by itself after an hour. A stopped loader stays until it
  is deleted: delete it before you load again.

### Or let people upload from the page

For the namespace admin. Off by default. Turned on, everyone who can open
the planner can upload a recording from the page, and everyone sees every
upload. Uploads are kept as sent (no renaming of real names) in the Pod's
`/tmp`, the last 4. A restart loses them: a new Pod starts with an empty
`/tmp`, and a planner container that restarts in the same Pod deletes the
uploads it finds there as it starts. Use the loader for recordings that
should stay.

```bash
# On OpenShift, see below. Keep the directory, and apply it again after each upgrade, with the new V.
# To also narrow who can open it, put both patches in this one file.
cd "$(mktemp -d)"
cat > kustomization.yaml <<EOF
resources:
  - ${SRC}/kubernetes?ref=${V}
patches:
  - target:
      kind: StatefulSet
      name: capacity-planner
    patch: |-
      - op: add
        path: /spec/template/spec/containers/0/args/-
        value: --allow-upload
      - op: add
        path: /spec/template/spec/containers/0/volumeMounts/-
        value: {name: uploads, mountPath: /tmp}
      - op: add
        path: /spec/template/spec/volumes/-
        value: {name: uploads, emptyDir: {sizeLimit: 2Gi}}
EOF
kubectl apply -n ${NS} -k .
```

On OpenShift, use `oc`, put `openshift` (or `openshift-sar`) in place of
`kubernetes`, and add the two timeout patches from
[Long recordings load slowly](#long-recordings-load-slowly) under `patches:`.
The Route and the oauth-proxy each stop waiting for an answer after 30 s,
and checking a large upload takes longer; the planner allows an upload 15
minutes, which their 31 minutes cover.

- The largest upload is 256 MiB; a day-long recording of a 16-replica P/D
  fleet is about 95 MB compressed. To change it, add
  `--upload-max-size=<size>` the same way, and keep `sizeLimit` at least
  (5 + `--max-inputs`) times it: 4 kept, 1 arriving, and up to `--max-inputs`
  pushed out but still loading (2 by default, so 7 times).
- The planner refuses a file that is not a learner recording, and says why.
  [Upload a recording](api.md#upload-a-recording) has the details.

### Or show the experimental v2 models on the page

For the namespace admin. Off by default. With these two flags every check
the page or the API runs (`POST /v1/backtest`) also prices TTFT with the
rank-mixture model and the TPOT percentiles with the per-request TPOT
distribution, without anyone asking for them. Neither is validated: every
figure they price is flagged and reads OUTSIDE VALIDATED REGION, and the
page says so. Plans and predictions are unchanged.
[What the two models are](page-and-commands.md#serve-with-the-experimental-v2-models).

```bash
# Keep the directory, and apply it again after each upgrade, with the new V.
# Put this patch beside any other in the same file. On OpenShift, use oc and openshift (or openshift-sar).
cd "$(mktemp -d)"
cat > kustomization.yaml <<EOF
resources:
  - ${SRC}/kubernetes?ref=${V}
patches:
  - target:
      kind: StatefulSet
      name: capacity-planner
    patch: |-
      - op: add
        path: /spec/template/spec/containers/0/args/-
        value: --experimental-ttft=v2
      - op: add
        path: /spec/template/spec/containers/0/args/-
        value: --experimental-tpot=v2
EOF
kubectl apply -n ${NS} -k .
```

## Open it

Send this to the people who use it, with your namespace in it.

### Plain Kubernetes

You need `kubectl` and a kubeconfig for the cluster.

```bash
# Leave it running, then open http://localhost:8080. Use localhost or 127.0.0.1: the planner refuses other names.
# If port 8080 is busy on your machine, use 9090:8080 and open http://localhost:9090.
kubectl -n <namespace> port-forward pod/capacity-planner-0 8080:8080
```

In **Source**, pick a recording by its file name.

- **Forbidden:** you are not in the group allowed to open it; ask the
  namespace admin.
- **The page stops answering** after an upgrade: the Pod was replaced, which
  ends every port-forward. Run the command again.

### OpenShift

Open the address the admin gets from this command, and log in:

```bash
echo "https://$(oc -n ${NS} get route capacity-planner -o jsonpath='{.spec.host}')"
```

## Narrow who can open it

### Plain Kubernetes

By default the RoleBinding names the group `system:authenticated`: everyone
who can log in to the cluster, ServiceAccounts included. It lets them
port-forward to the planner's Pod and nothing else. To allow one group, apply
your own kustomization over the shipped one:

```bash
# Replace planners with your group. Keep the directory, and apply it again after each upgrade, with the new V.
cd "$(mktemp -d)"
cat > kustomization.yaml <<EOF
resources:
  - ${SRC}/kubernetes?ref=${V}
patches:
  - target:
      kind: RoleBinding
      name: capacity-planner-viewer
    patch: |-
      - op: replace
        path: /subjects
        value:
          - apiGroup: rbac.authorization.k8s.io
            kind: Group
            name: planners
EOF
kubectl apply -n ${NS} -k .
```

Reading the namespace (`view`) is not enough to open the planner; the Role
`capacity-planner-viewer` is.

### OpenShift

Deploy the `openshift-sar` overlay in place of `openshift`. Then only users
who may `get` the Service `capacity-planner` in the namespace get in: anyone
with `view`, `edit` or `admin` there, and anyone you bind to the shipped
Role:

```bash
oc apply -n ${NS} -k "${SRC}/openshift-sar?ref=${V}"
oc -n ${NS} create rolebinding capacity-planner-viewer --role=capacity-planner-viewer --group=<group>
```

The proxy asks with the user's own token, so neither overlay needs a cluster
admin.

## Real names in recordings

A recording holds the namespaces, models, variants and Pods it saw. Where
the people you share the planner with should not see them, serve an
exported copy. It renames every name to a pseudonym such as `variant-03`,
and refuses a recording that holds anything it cannot place. The export is
not in the released planner: run it from a clone of the release, with Go 1.26
or later:

```bash
# The export reads an uncompressed recording: gunzip -k keeps the original.
gunzip -k /path/to/qwen3-32b.jsonl.gz
git clone https://github.com/llm-d-incubation/llm-d-scaling-manager.git && cd llm-d-scaling-manager && git checkout ${V}
go run ./cmd/learnergolden export -in /path/to/qwen3-32b.jsonl -out /path/to/qwen3-32b-export.jsonl.gz
```

Load the export, not the original.

## Upgrade

Apply the same overlay with the new release, then tell people to run their
port-forward again. The recordings stay.

```bash
export V=<new release>
kubectl apply -n ${NS} -k "${SRC}/kubernetes?ref=${V}"
kubectl -n ${NS} rollout status statefulset/capacity-planner
```

## Remove it

```bash
# Name the overlay you applied (kubernetes, openshift or openshift-sar). Deletes the recordings claim. If your StorageClass's
# reclaim policy is Retain, its volume stays (kubectl get pv | grep capacity-planner-recordings) for your cluster admin to delete.
kubectl delete -n ${NS} -k "${SRC}/kubernetes?ref=${V}"
# OpenShift only: the cookie secret.
kubectl -n ${NS} delete secret capacity-planner-proxy
```

## Details

- **Why port-forward is safe.** The planner has no authentication of its own
  (`--auth=none`) and binds `127.0.0.1:8080` in its Pod, so nothing outside the
  Pod reaches it, and there is no Service. `kubectl port-forward` arrives on
  the Pod's loopback after the API server checks RBAC: `get` on the Pod
  `capacity-planner-0`, and `create` (or `get`, for kubectl's WebSocket
  upgrade) on its `pods/portforward`. The Role names that one Pod, which is
  why the planner is a StatefulSet of one. The planner also refuses a request
  whose `Host` is not a loopback name ([Requests](api.md#requests)), so a web
  page cannot reach it through your port-forward by rebinding its own name.
- **A namespace of its own** still matters: anyone who may create Pods in the
  namespace can mount the recordings volume and read the recordings.
- **On OpenShift**, the proxy sends the planner `Host: 127.0.0.1:8080`
  (`--pass-host-header=false`); with the Route's name the planner would answer
  403. The Route re-encrypts to the proxy's service-CA certificate. The
  planner container gets no ServiceAccount token; the proxy does, as its OAuth
  client secret. Users with `edit` in the namespace can `oc port-forward`
  past the proxy; that grants them nothing new, since `edit` already lets them
  run Pods there and read the volume.
- **The proxy image.** The overlay names
  `registry.redhat.io/openshift4/ose-oauth-proxy-rhel9:v4.19`. To match your
  cluster's minor version, add an `images:` entry with another `newTag` in a
  kustomization of your own, as in [Narrow who can open
  it](#narrow-who-can-open-it).
- **The planner image** is the release's own
  (`ghcr.io/llm-d-incubation/llm-d-scaling-manager/capacity-planner:${V}`): the
  `?ref=` you apply picks it.
- **Hardening.** Non-root, read-only root filesystem, no capabilities, no
  privilege escalation, memory limited, and the recordings mounted
  read-only.
- **Recordings only.** The in-cluster planner does not read Prometheus. To
  plan from a live window, run it [on your machine](install-options.md#start-the-planner).
  People can also upload a recording from the page, if the admin
  [turns it on](#or-let-people-upload-from-the-page).
- **Kubernetes authentication.** `serve --auth=kubernetes`
  ([Kubernetes authentication](api.md#kubernetes-authentication)) is for API
  clients that send a bearer token. A browser sends none, which is why these
  overlays use port-forward or oauth-proxy in front of `--auth=none`.
