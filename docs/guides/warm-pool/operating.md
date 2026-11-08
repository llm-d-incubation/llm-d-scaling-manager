# Operating a pool: scraping, checks, troubleshooting

[← Warm pool guide](README.md)

## Scraping the pool, and why the demand number depends on it

The installer passes its monitoring namespace (`MONITORING_NAMESPACE`) to every
pool it creates. Creating a pool by hand, name it yourself:

```
deploy/warmpool.sh create -n <namespace> --name <pool> ... --monitoring-namespace <where Prometheus runs>
```

That creates a PodMonitor for the pool and admits that namespace to the serving
port. Both are needed. The shipped `config/warmpool` manifests name no monitoring
namespace (a default could admit somebody else's), so `kubectl apply -k
config/warmpool` gives an unscraped pool.

**Why it matters.** A **lent** pool Pod serves one model's traffic. If nothing
scrapes it, that load moves onto the bridge and the model's measured demand
*falls*, just when the shortfall that caused the borrow is worst. It comes back
when the bridge is returned, looking like a new spike rather than capacity
leaving. Nothing looks wrong: the pool works, the model serves, the number is
just too small.

- **The proxy's `:8000` is scraped, not the engines' ports.** The proxy forwards
  `/metrics` to whichever engine is awake, so one address covers every model in
  the Pod.
- **Only awake Pods are targets.** A pool Pod is Ready only while a model is
  awake in it, and the PodMonitor keeps Ready targets only. Otherwise every idle
  Pod would be a permanently-DOWN target.
- **Scrape the engines every 10 s**, the pool's and the models' alike
  ([why](#the-10-s-scrape-interval)). The pool's PodMonitor and the shipped
  model-server PodMonitor (`config/modelserver-metrics/`) already do. Set the
  same on model servers you deploy from your own values
  (`monitoring.podmonitor.interval` in a modelservice values file, as the
  `two-variant-scaling-manager` and `workload-autoscaling` benchmark scenarios do).

> **Check your Prometheus actually selects it.** The operator reads only
> PodMonitors matching its `podMonitorSelector`. A stack installed with a
> selector like `release: kube-prometheus-stack` ignores this one **silently**:
> the PodMonitor exists, no scrape job is generated, and demand under-reads as
> above. Look for `podMonitor/<namespace>/llmsm-warm-pool-<pool>/0` in the
> generated config:
>
> ```
> kubectl get secret prometheus-<prometheus-name> -n <monitoring-ns> -o jsonpath='{.data.prometheus\.yaml\.gz}' | base64 -d | gunzip | grep warm-pool
> ```
>
> If it is absent, add the label your Prometheus selects on to the PodMonitor.

The scaling manager counts the bridge's numbers asymmetrically:

| | counted? | why |
| --- | --- | --- |
| the bridge's **demand** | **yes**, into the model's total | it is the model's traffic, wherever it is being served |
| the bridge's **capacity** | **no**, never into supply | the Pod is borrowed. Counted as supply it would tell the optimizer the fleet is already big enough and suppress the scale-up the bridge exists to cover — after which the pool holds the Pod indefinitely, because the replicas that would release it are the ones it prevented |

The capacity is still measured and published per variant for the retained-pool
switching decision, where the pool *is* the capacity. Look for
`warm-pool-bridge-supply` in the controller log.

## Checking it works

The pool logs its state whenever it changes:

```bash
# The pool's state lines from the controller log
kubectl logs deploy/scaling-manager-controller-manager -n <scaling-manager-namespace> | grep "warm pool"
```

```
warm pool state {"pool":"default","state":"pods=2 free=2 resident=1 variants=1 lent=0 accelerator=NVIDIA-H100-80GB-HBM3"}
```

- `pods` / `free`: how many exist, and how many can be lent
- `resident`: models held warm (`0` on a pool that has warmed nothing)
- `lent`: bridges open now
- `accelerator`: what the pool's Pods sit on. `unknown` means the scaling
  manager cannot read the nodes, so it will not match models against the pool.

A steady pool logs this **once**, so no news is good news. Then the metrics:

| metric | means |
| --- | --- |
| `llmsm_warmpool_borrow_total{outcome="hit"}` | a scale-up was bridged |
| `…{outcome="miss"}` | the model was not warm — raise `preload-top`, or the pool is too small to hold it |
| `…{outcome="blocked"}` | the model *was* warm but no Pod was free — raise `replicas` |
| `llmsm_warmpool_bridge_seconds` | how long bridges last |
| `llmsm_warmpool_free_pods` | the reserve, live |

Watch `bridge_seconds`. A bridge should last about as long as an ordinary
replica takes to start. Bridges sitting at `max-hold` mean the scale-ups they
cover are failing, and the pool is hiding it.

## Troubleshooting

**Nothing is ever warmed, and no errors.** Check `replicas` against
`sleep-min-size` ([Sizing](sizing.md#sizing)). The log says
`warm pool cannot admit any model: every Pod is reserve`.

**The pool reports itself empty while Pods are running.** The log says
`no warm pool Pod could be read … usually the pool NetworkPolicy`. Its ingress
`namespaceSelector` must name the namespace the controller runs in ([step
2](install.md#2-edit-the-four-things-the-manifests-cannot-know)).

**The pool does not start at all.** The log says
`the warm pool is disabled: this controller may not patch Pods`.
Grant it in the pool namespace (Role `scaling-manager-pod-writer`). From the repository
root, as someone who may create Roles there:

```bash
bash deploy/lib/pod_writer.sh <pool namespace> <controller namespace> | kubectl apply -f -
```

A namespace-scoped controller then needs a restart; a cluster-scoped one
retries on its own:

```bash
kubectl rollout restart deployment/scaling-manager-controller-manager -n <controller namespace>
```

**A model never gets a warm copy in a multi-pool namespace.** The log says
`variant will get no warm copy`, with the reason: it named no pool, or one that
does not exist. Fix it in the ScaledObject trigger metadata.
`deploy/warmpool.sh plan -n <namespace>` lists every such model without waiting
for the log:

```
- model-b    selects: h100  <- NO SUCH POOL
```

**The first spike is never bridged.** Expected: a model must miss twice before
the pool warms it, so it does not spend a load on a one-off. Set `preload-top` to
warm your busiest variants up front, or `warmPoolCopies: "1"` on the one model
you cannot afford to miss.

**The pool never resizes, or is never used.** Check its ScaledObject exists,
that `scalerAddress` names the scaling manager's namespace, and that
`warmPoolName` matches the Deployment's `llm-d.ai/warm-pool` label. Without a
trigger there is no pool, and the log says:

```
warm pool Deployments are holding accelerators but no ScaledObject declares them
```

`plan` lists the pools a namespace has declared; a Deployment missing from it
holds accelerators for nothing. Give it a trigger, or `delete` it, which removes
both objects so the state cannot recur.

**Two scale-ups of one model, only one bridged.** Automatic mode holds one warm
copy per model. Set `warmPoolCopies` to the number of concurrent scale-ups you
want covered.

## Details

### The 10 s scrape interval

The scale-up signal, and so the lend, is a queue at the engine. It can move only
when Prometheus holds a sample showing it, so the scrape interval is jitter on
every decision. Measured on a two-model run, from each rate step to the scale-up
decision: 25 / 56 / 56 / 87 s at 30 s, 15 / 60 / 60 / 76 s at 10 s. The jitter
goes; the ~60 s the queue itself takes to form does not, because that is the
signal, not the scrape. In those four rises the controller added under 7 s after
the sample; its bound is the 15 s optimization interval plus the ScaledObject's
5 s poll.

The benchmark harness defaults to 30 s; `hack/benchmark/patch_harness.sh`
(fix 13) makes it 10 s, so a scenario that sets no interval (the P/D one) scrapes
at 10 s too. `make lint-deploy-scripts` checks every interval this repo ships.

The controller's `PROMETHEUS_METRICS_CACHE_TTL` (30 s) is not on this path. The
per-cycle replica collector queries Prometheus every cycle and memoises within
the cycle only, so at 10 s every 15 s cycle reads a fresh row: on a run, a
`requestRate` that changes every cycle.
