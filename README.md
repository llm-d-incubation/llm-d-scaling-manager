# llm-d-scaling-manager

[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![CI](https://github.com/llm-d-incubation/llm-d-scaling-manager/actions/workflows/ci-pr-checks.yaml/badge.svg?branch=main)](https://github.com/llm-d-incubation/llm-d-scaling-manager/actions/workflows/ci-pr-checks.yaml)
[![Go](https://img.shields.io/github/go-mod/go-version/llm-d-incubation/llm-d-scaling-manager)](go.mod)

llm-d-scaling-manager sizes LLM inference fleets on llm-d, aggregated and
prefill/decode alike. It learns each model's capacity from its own servers
(throughput per replica, batch capacity, latency, start time) and orders the
replicas the load needs. Under a GPU quota it shares the GPUs between models
(**utilization share**, experimental: benchmarked with two models so far; see
[Why use it](docs/why.md#how-mature-each-part-is)): each model gets its need,
the spare goes by weight, and GPUs move as demand shifts. It can run
**passive** first and report what it would do without changing anything.
KEDA does the scaling.

```mermaid
flowchart LR
  prom[("Prometheus: vLLM, SGLang and router metrics")] --> coll[Collector]
  k8s[("Kubernetes: ScaledObjects, pods, nodes, Kueue")] --> coll
  coll --> learners
  subgraph learners [Learners]
    mu["Throughput per replica (mu)"]
    k2["Batch capacity in tokens (k2)"]
    itl[Latency line]
    st[Replica start time]
    snap[(Learned-state snapshot)]
  end
  learners --> ana["Analyzers: saturation sizes, throughput adds, SLO only reports"]
  ana --> engine
  subgraph engine [Engine]
    ss[Per-model sizing]
    us[Utilization share]
    lim["Limiters: quota, Kueue, physical GPUs"]
    wp[Warm pool]
  end
  engine --> scaler(["KEDA external scaler<br/>passive switches here"])
  scaler --> hpa[KEDA and HPA]
  hpa --> work[Deployments and LeaderWorkerSets]
```

Every 15 seconds it reads the metrics, updates what it has learned, works out
each model's need, fits it into the budget, and answers KEDA. In passive mode
the scaler answers with the current replica count, so nothing moves.
[How it fits together](docs/concepts/architecture.md).

## Start here

- **[The main path: share GPUs between models](docs/well-lit-paths/utilization-share/)**:
  install passive, watch, turn on sharing in shadow, then go active.
- **[Configuration](docs/reference/configuration.md)**: every setting, the
  ones to set for your setup and the ones to tune per model.
- **[How it decides](docs/concepts/architecture.md)**: the learners, the analyzers and the
  engine, in operator terms.
- **[Other scenarios](docs/well-lit-paths/)**: scale on saturation, scale to
  zero, warm pool, P/D, Kueue quotas and more.
- **[Capacity planner](docs/guides/capacity-planner/)**: a web page on your
  machine that prices a P/D fleet from what it did, and the fleet a rate needs.
- **[Why use it](docs/why.md)**: what it was measured to do, what it costs,
  and how mature each part is.

## Install

Both ways install **passive**: it works out what it would do and changes
nothing, until you switch it on.

- **With kubectl, from one manifest.** One controller for the cluster, signed
  files applied as downloaded by two scripts that run `kubectl` only, no
  docker and no checkout. New in v0.13.0; on an earlier release, use the
  `smt` tools image.
- **With the `smt` tools image.** In every release today. Also the way to run
  one controller per namespace, which the one manifest does not do.

**You need, either way:**

- llm-d model servers in one or more namespaces, KEDA, and the Prometheus
  Operator.
- Your GPU product and how many GPUs the models may hold together. The product
  is the `GPU.PRODUCT` column of `kubectl get nodes -L nvidia.com/gpu.product`.
- For P/D on a DisaggregatedSet: LeaderWorkerSet v0.10.0 or later
  (`kubectl api-resources --api-group=disaggregatedset.x-k8s.io` lists
  `disaggregatedsetrolescalers`).

### With kubectl, from one manifest

You need cluster-admin rights, and KEDA 2.18 or later in any namespace. The
release carries the files and two scripts that apply them as downloaded,
with `kubectl` only; `--print` shows the commands instead, for git. To
verify the signatures, see [install with kubectl](docs/guides/kubectl-install/).

1. Download the files, and install: the manifest, KEDA's side in its
   namespace, and the settings, with the namespaces it manages and the quota.

   ```bash
   V=<release, v0.13.0 or later>
   for f in scaling-manager.yaml scaling-manager-keda.yaml scaling-manager-grants.yaml \
       scaling-manager-monitoring.yaml scaling-manager-admin.sh scaling-manager-owner.sh; do
     curl -fsSLO https://github.com/llm-d-incubation/llm-d-scaling-manager/releases/download/$V/$f
   done
   export CTRL=scaling-manager-system MODEL_NAMESPACES="<ns1> <ns2>"
   bash scaling-manager-admin.sh install --prometheus-url https://<prometheus>:9090 \
     --keda-namespace <the namespace KEDA runs in> --watch-namespaces "$(echo $MODEL_NAMESPACES | tr ' ' ,)"
   bash scaling-manager-admin.sh set limiter=quota quotas=<gpu product>=<gpus> quotaScope=cluster utilizationShare=shadow
   ```

2. Check it, then register the models. `ready` is `ok` once every check
   passes, and each `namespace.<name>` line says what that namespace still
   needs. The controller suggests one ScaledObject per model server; review
   min and max, then apply them:

   ```bash
   bash scaling-manager-admin.sh status
   for ns in $MODEL_NAMESPACES; do
     bash scaling-manager-owner.sh register $ns          # or: register $ns <your ScaledObjects file>
   done
   ```

   Run as the cluster admin, `register` without a file reads the suggestion
   from the controller's namespace. A namespace owner without that right
   registers a file instead
   ([the namespace owner's part](docs/guides/kubectl-install/README.md#namespace-owner)).

   LeaderWorkerSets and DisaggregatedSets are not suggested yet. Where nothing
   scrapes the model servers, `scaling-manager-owner.sh monitor <ns>` adds the
   PodMonitor ([the namespace owner's part](docs/guides/kubectl-install/README.md#namespace-owner)).

Then [watch what it would do](#watch-what-it-would-do).

### With the `smt` tools image

You also need a cluster admin and a namespace owner, each with their own
kubeconfig. The lines marked `# admin` need the cluster admin.

#### 1. Get the `smt` command

Once per machine. The image carries the release files, signed with it
([verify it](docs/reference/tools-install.md#set-up-smt)). Use a token
kubeconfig, because the container runs no cloud CLI. For an API server at
`127.0.0.1` (kind), add `--network host` to the `docker run`.

```bash
TOOLS=ghcr.io/llm-d-incubation/llm-d-scaling-manager/tools@sha256:30e0f157d368d4c345673c45163b7994a55ff659f308ffe8988ebd1544d0cbca
mkdir -p ~/.smt && chmod 700 ~/.smt
smt() { docker run --rm --user "$(id -u):$(id -g)" \
  -v "${SMT_KUBECONFIG:-$HOME/.smt/kubeconfig}:/kube/config:ro" -v "$PWD:/work" "$TOOLS" "$@"; }
# One kubeconfig snapshot per role: run each line while kubectl is logged in as that role.
kubectl config view --minify --flatten > ~/.smt/admin.kubeconfig && chmod 600 ~/.smt/admin.kubeconfig   # admin
kubectl config view --minify --flatten > ~/.smt/kubeconfig && chmod 600 ~/.smt/kubeconfig               # owner
```

The owner needs only the stock `admin` ClusterRole, bound in the namespace
the scaling manager runs in.

#### 2. Install it passive, sharing in shadow

Pick the block for your layout. Each command checks before it writes, stops on
a problem with the fix, and can be run again.

**Models in several namespaces**: one controller in its own namespace, and
one GPU budget shared by all of them.

```bash
export CTRL=scaling-manager-system
export MODEL_NAMESPACES="<ns1> <ns2>"                    # every namespace with llm-d model servers
kubectl create namespace $CTRL                                                          # admin
SMT_KUBECONFIG=~/.smt/admin.kubeconfig smt admin-setup -n $CTRL \
  $(printf -- '--model-namespace %s ' $MODEL_NAMESPACES)                                # admin
smt install -n $CTRL --prometheus-url https://<prometheus>:9090 \
  --quota "<gpu product>=<gpus>" --set quotaScope=cluster --utilization-share shadow
SMT_KUBECONFIG=~/.smt/admin.kubeconfig smt admin-finish -n $CTRL                        # admin
```

**Models in one namespace**: the controller runs beside them.

```bash
export CTRL=<the namespace running llm-d> MODEL_NAMESPACES=$CTRL
SMT_KUBECONFIG=~/.smt/admin.kubeconfig smt admin-setup -n $CTRL                         # admin
smt install -n $CTRL --prometheus-url https://<prometheus>:9090 \
  --quota "<gpu product>=<gpus>" --utilization-share shadow
SMT_KUBECONFIG=~/.smt/admin.kubeconfig smt admin-finish -n $CTRL                        # admin
```

`install` should end with `Installed in <namespace>, passive.` and
`admin-finish` with `serves the scaler over mutual TLS`. Some options:

- A private CA for Prometheus: add `--prometheus-ca <file in this directory>`
  to `install`.
- KEDA not found: add `--keda-namespace <namespace>` to `admin-setup`.
- `admin-setup` adds the monitors that scrape the model servers and each
  router's endpoint picker, where nothing scrapes them yet.

#### 3. Register the models

One ScaledObject per model server. On a DisaggregatedSet, there is one per
role, prefill and decode.

```bash
for ns in $MODEL_NAMESPACES; do
  SMT_KUBECONFIG=~/.smt/admin.kubeconfig smt scaledobjects-plan -n $ns \
    --controller-namespace $CTRL --out plan-$ns.yaml              # read-only; edit the plan if you like
  SMT_KUBECONFIG=~/.smt/admin.kubeconfig smt scaledobjects-apply -n $ns \
    --controller-namespace $CTRL --plan plan-$ns.yaml
done
```

A DisaggregatedSet whose roles are `scaling.mode: Static` has no `/scale` for
KEDA. Add `--dset-role-writer` to `admin-setup` and `install`, and annotate
the set ([P/D in passive mode](docs/guides/try-in-passive-mode/README.md#on-a-disaggregatedset)).
In passive mode it still writes nothing.

### Watch what it would do

Either install. Leave it through each model's busy period. First check it is in shadow:

```bash
kubectl get configmap scaling-manager-controller-status -n $CTRL \
  -o jsonpath='{.data.limiter} {.data.utilizationShare}{"\n"}'        # quota/... shadow
```

Then read what it would do in Prometheus, or graph it beside what runs:

```promql
# 1 while sharing only reports (a passive model is in the group)
llmsm_utilization_share_passive_shadow
# the replicas each model would run, without sharing
max by (exported_namespace, variant_name) (llmsm_desired_replicas)
# the GPUs sharing would give each model, and which models a move would fix
llmsm_utilization_share_target_gpus
llmsm_utilization_share_actionable == 1
# how many replicas it would move
llmsm_utilization_share_replicas_to_move
```

Nothing moves, so `llmsm_utilization_share_transfers_total` stays flat. The
same decisions are in the controller's log, as `Utilization share: would
rebalance` lines, and each model gets a `PassiveRecommendation` Event where
its would-be count differs from what it runs. Per model, and what each metric
means: [verification](docs/guides/try-in-passive-mode/README.md#verification)
and [utilization share in passive mode](docs/guides/try-in-passive-mode/README.md#utilization-share-in-passive-mode).

### Go active, when you are ready

Sharing scales a donor down, so each model server must drain on termination
([prepare model servers](docs/guides/prepare-workloads/); with the tools
image, `smt workload-patch -n <ns>` reports what each one still needs).

First give the controller patch on pods in each model namespace. Installed
with kubectl, the namespace owner binds it from the release's grants file:

```bash
for ns in $MODEL_NAMESPACES; do
  bash scaling-manager-owner.sh grant pod-writer $ns
done
```

Installed with `smt`, it is the activation file the image carries:

```bash
smt release-files --out release
for ns in $MODEL_NAMESPACES; do                                       # admin: the one grant of patch on pods
  sed -e "s/__NAMESPACE__/$CTRL/g" -e "s/__MODEL_NAMESPACE__/$ns/g" \
    release/scaling-manager-activate.yaml | kubectl apply -f -
done
```

Then switch both on (installed with kubectl, the same is
`bash scaling-manager-admin.sh set passive=false utilizationShare=active`):

```bash
kubectl patch configmap scaling-manager-settings -n $CTRL --type merge \
  -p '{"data":{"passive":"false","utilizationShare":"active"}}'
```

Switch sharing with the settings key `utilizationShare`, as here, not with
`smt utilization-share shadow`/`activate`. The settings key wins over the
scaling policy that those commands write
([configuration](docs/reference/configuration.md)).

### Undo

```bash
kubectl patch configmap scaling-manager-settings -n $CTRL --type merge \
  -p '{"data":{"utilizationShare":"off"}}'                            # sharing off; the budget stays
```

To remove it completely: installed with kubectl, follow
[uninstall](docs/guides/kubectl-install/README.md#uninstall). Installed with
`smt`, follow the cleanup in
[install in a namespace](docs/guides/install-in-namespace/#cleanup); with
several namespaces, first run
`SMT_KUBECONFIG=~/.smt/admin.kubeconfig smt admin-setup -n $CTRL --single-namespace`
(admin), which takes the model namespaces back. Your models keep serving
throughout.

### More

- Every step with its checks:
  - [install with kubectl, from one manifest](docs/guides/kubectl-install/),
    including a move from a cluster-wide `smt` install;
  - [install in a namespace](docs/guides/install-in-namespace/);
  - [several namespaces, one budget](docs/guides/utilization-share-namespaced/);
  - [for the whole cluster](docs/guides/install-cluster-wide/);
  - [try it in passive mode](docs/guides/try-in-passive-mode/).
- Flags and exit codes: [install commands](docs/reference/tools-install.md).
- Without a GPU quota, leave out `limiter`, `quotas`, `quotaScope` and the
  `utilizationShare` patch (kubectl), or `--quota` and `--utilization-share`
  (`smt`), and each model is scaled on its own.
- From a checkout instead: `make deploy-scaling-manager`
  ([installer options](docs/reference/install-options.md)).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) and the
[developer guide](docs/developer-guide/development.md). Questions, bugs and
ideas: [GitHub issues](https://github.com/llm-d-incubation/llm-d-scaling-manager/issues).

## License

Apache 2.0 - see [LICENSE](LICENSE) for details.

## Origin

Started in 2026 from the code of llm-d's workload-variant-autoscaler; since
redesigned. Nearly 90% of the production Go code is new since August 2026,
and nothing has been taken from that project since. Not affiliated with or
endorsed by llm-d.

## Related Projects

- [llm-d-autoscaling](https://github.com/llm-d/llm-d-autoscaling) - llm-d's KEDA autoscaling blueprints
- [llm-d infrastructure](https://github.com/llm-d/llm-d-infra)
- [llm-d main repository](https://github.com/llm-d/llm-d)
