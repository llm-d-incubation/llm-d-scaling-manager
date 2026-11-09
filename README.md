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
- **[Why use it](docs/why.md)**: what it was measured to do, what it costs,
  and how mature each part is.

## Install

You need llm-d model servers in a namespace, KEDA, a Prometheus, and a checkout
of this repository with `kubectl`, `jq`, `yq` and `make`. The model servers must
drain on termination, because sharing scales a donor down: `make workload-patch`
writes the patch ([preparing a workload](docs/reference/workload-preparation.md)).
This installs it passive with a GPU quota, so it decides and changes nothing.
`<gpu product>` is the `GPU.PRODUCT` column of
`kubectl get nodes -L nvidia.com/gpu.product`:

```bash
export NAMESPACE=<the namespace running llm-d>
make check-prereqs                    # read-only: tools, namespace, Prometheus
make setup-prereqs                    # once per namespace, by a cluster admin
make deploy-scaling-manager LLMSM_PASSIVE=true \
  LLMSM_LIMITER=quota LLMSM_QUOTAS='<gpu product>=<gpus>'
make scaledobjects-plan LLMSM_DEFAULT_SO_PLAN=plan.yaml   # lists your model servers
make scaledobjects-apply LLMSM_DEFAULT_SO_PLAN=plan.yaml  # registers them with KEDA
make utilization-share-check          # is there a budget to share?
make utilization-share-shadow         # report what sharing would move
```

Leave it through each model's busy period and read what it would have done
(how: [per model](docs/guides/try-in-passive-mode/README.md#verification),
[for sharing](docs/guides/try-in-passive-mode/README.md#utilization-share-in-passive-mode)).
Then go active:

```bash
make deploy-scaling-manager LLMSM_PASSIVE=false
make utilization-share-shadow
make utilization-share-activate
```

Models that KEDA already scales, a controller run by a cluster admin, and
every check along the way are in the guides:
[try it in passive mode](docs/guides/try-in-passive-mode/) and
[turn on utilization share](docs/guides/utilization-share/). Without a quota,
leave out `LLMSM_LIMITER` and the sharing steps: each model is then scaled on
its own ([install in a namespace](docs/guides/install-in-namespace/)).

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
