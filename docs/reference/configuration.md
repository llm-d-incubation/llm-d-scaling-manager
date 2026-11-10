# Configuration reference

What you can set, where, and which settings matter. Most installs set about
twenty things once ([Set for your setup](#set-for-your-setup)) and tune a
handful per model ([Tune per workload](#tune-per-workload)). Everything else has
a calibrated default: [advanced settings](configuration-advanced.md).

Looking for an installer variable, or a section such as *External scaler
mutual TLS* or *Learned state across a restart*? It moved to
[installer options](install-options.md).

In the tables, **bold** marks the settings most installs change.

## Where settings live

| Place | What it is | A change applies |
| --- | --- | --- |
| **Installer** | `make` and `deploy/install.sh` variables. They write the places below. Every one: [installer options](install-options.md) | on the next install run |
| **Manager ConfigMap** | `scaling-manager-manager-config`, key `config.yaml`, in the controller's namespace. Every key can also be set as an environment variable of the same name on the controller Deployment. A flag beats an environment variable, which beats the ConfigMap, which beats the built-in default | after a controller restart |
| **Flag** | an `args` entry on the controller Deployment | after a controller restart |
| **Policy** | `scaling-manager-scaling-policy-config`: a `default` entry, named tiers, per-model overrides ([scaling policy](scaling-policy.md)). "Policy, `default`" means the cluster `default` entry only | live, within a cycle (\*) |
| **Trigger** | a ScaledObject's trigger `metadata`, plus its `minReplicaCount` and `maxReplicaCount` ([trigger reference](scaledobject-trigger.md)) | live, when KEDA next calls |
| **Label / annotation** | on a Namespace or on a workload's pod template | live |

(\*) A namespace-scoped controller that reads a policy ConfigMap outside the
namespace it manages reads it only at startup; an edit there needs a restart.

## Set for your setup

Set these once, when you install. Most have an installer variable, shown in the
second column, that writes them for you.

### Install shape

| Name | Where set | Default | What it changes | Applies |
| --- | --- | --- | --- | --- |
| `LLMSM_SCOPE` (Make: `SCOPE`) | Installer | `namespace` | One controller per namespace, or one for the whole cluster. Picks the RBAC and the managed namespaces ([cluster-wide install](../guides/install-cluster-wide/)) | reinstall |
| `LLMSM_NS` | Installer | `scaling-manager-system` | The controller's namespace, where its ConfigMaps live | reinstall |
| `NAMESPACE` | Installer | none; required at namespace scope | Where your model servers run. Read by the installer only | install |
| `--watch-namespace` (env `WATCH_NAMESPACE`); installer `LLMSM_WATCH_NS` | Flag / Installer | built in: empty, every namespace. A namespace-scoped install sets it to the controller's namespace, or to `LLMSM_WATCH_NS` (a list, or `auto`) | The namespaces whose workloads the controller reads. A workload outside them is called about by KEDA but never scaled | restart |
| `KEDA_NAMESPACE` | Installer | `keda-system` when the installer installs KEDA; otherwise detected | The only namespace the scaler's NetworkPolicy admits | reinstall |
| `LLMSM_SCALER_TLS`; flags `--external-scaler-cert-path`, `--external-scaler-client-ca` | Installer | unset: mutual TLS when KEDA's side is in place, else plaintext with a warning | Whether KEDA talks to the scaler over mutual TLS ([how](install-options.md#external-scaler-mutual-tls)) | reinstall |

### Prometheus

| Name | Where set | Default | What it changes | Applies |
| --- | --- | --- | --- | --- |
| `PROMETHEUS_BASE_URL`; installer `PROMETHEUS_URL` | Manager ConfigMap | required, no built-in default. The shipped ConfigMap names a kube-prometheus-stack in `scaling-manager-monitoring`; the installer detects one ([how](install-options.md#how-the-prometheus-address-is-chosen)) | The Prometheus every signal is read from. The controller exits at startup when it cannot reach it | restart |
| `PROMETHEUS_CA_CERT_PATH` | Manager ConfigMap | empty | The CA that signed Prometheus' certificate. The installer copies and mounts it when it can | restart |
| `PROMETHEUS_TLS_INSECURE_SKIP_VERIFY` | Manager ConfigMap | `false`. The installer writes `true` for a Prometheus it deploys itself, with a warning | Skip the certificate check ([TLS](prometheus.md#tls-configuration)) | restart |

### Observe before it acts

| Name | Where set | Default | What it changes | Applies |
| --- | --- | --- | --- | --- |
| **`LLMSM_PASSIVE`** | Manager ConfigMap; installer `LLMSM_PASSIVE` | `false` | Passive for every model: it decides and publishes, and changes no replica count. The installer restarts the controller when the value changes | restart |
| **`passive`** | Policy, any entry | inherits `LLMSM_PASSIVE` | Passive for one namespace, tier or model ([passive](scaling-policy.md#passive)) | live |

### GPU budget

Without a limiter, scaling is bounded only by each ScaledObject's
`maxReplicaCount`. Utilization share needs one.

| Name | Where set | Default | What it changes | Applies |
| --- | --- | --- | --- | --- |
| **`limiters`**; installer `LLMSM_LIMITER` | Policy, `default` | none | The GPU budget: `quota` (GPUs you declare), `gpu-inventory` (GPUs on the nodes), or both ([quota limiter](quota-limiter.md), [GPU limiter](gpu-limiter.md)) | live |
| `limiters[].quotas`, `namespaceQuotas`, `scope`; installer `LLMSM_QUOTAS`, `LLMSM_QUOTA_SCOPE` | Policy, `default` | no quota; installer scope `namespace` | GPUs per accelerator type, for the cluster or per namespace. `-1` is no cap. The key must be the accelerator name the controller logs (for example `H200` or `NVIDIA-H200`) | live |
| `limiters[].kueue.enabled`; installer `LLMSM_QUOTA_KUEUE` | Policy, `default` | `false` | Also bound each namespace by its Kueue ClusterQueue quota ([Kueue path](../well-lit-paths/kueue-bounded-quotas/)) | live |
| `limiters[].kueue.controllerNamespace`; installer `KUEUE_NAMESPACE` | Policy, `default` | `kueue-system` | Where Kueue runs. Set it only when Kueue runs elsewhere | live |
| `LLMSM_NODE_SELECTOR` | Controller environment | empty: every GPU node | The nodes the `gpu-inventory` limiter counts, as a label selector | restart |

### Utilization share

The main path. Written by `make utilization-share-shadow` and
`make utilization-share-activate`
([the journey](../well-lit-paths/utilization-share/)).

| Name | Where set | Default | What it changes | Applies |
| --- | --- | --- | --- | --- |
| **`optimizer.type: utilizationShare`** | Policy, `default` | unset: off | Share the limiter's budget between models: need first, headroom by weight, GPUs moved by transfer ([optimizer](scaling-policy.md#optimizer-cluster-default-only-live)) | live |
| **`optimizer.utilizationShare.shadow`** | Policy, `default` | `false`. **A block without it acts** | Compute and report what it would move, and move nothing | live |
| `optimizer.utilizationShare.physicalGroups` | Policy, `default` | `false` | Also share under a `gpu-inventory` limiter alone. Cluster-scoped controller only | live |

### Learned state

| Name | Where set | Default | What it changes | Applies |
| --- | --- | --- | --- | --- |
| **`LEARNED_STATE_SNAPSHOT`** | Controller environment or manager ConfigMap | `false` | Keep what the learners measured (capacity, throughput, latency line, start time) across a restart or leader change, in the ConfigMap `scaling-manager-learned-state`. Off, a restart relearns from cold ([what is kept](install-options.md#learned-state-across-a-restart)) | restart |

### Each workload

| Name | Where set | Default | What it changes | Applies |
| --- | --- | --- | --- | --- |
| **`modelID`** | Trigger | required | The model this workload serves; workloads with the same `modelID` are sized together. Must equal the engine's `--served-model-name`. `make scaledobjects-plan` reads it for you | live |
| `scalerAddress` | Trigger | written by `make scaledobjects-plan` | Where KEDA calls the scaler | live |
| `kueue.x-k8s.io/queue-name` | Label, pod template | none | The Kueue queue the pods are admitted through. Kueue users only ([Kueue guide](../guides/kueue-borrowing/)) | next pod |

### Namespaces

| Name | Where set | Default | What it changes | Applies |
| --- | --- | --- | --- | --- |
| `scaling-manager.llmd.ai/config-enabled=true` | Label, Namespace | absent | Read a scaling-policy ConfigMap in this namespace too ([namespace-local policy](scaling-policy.md#namespace-local-scaling-policy-map)) | live |
| `scaling-manager.llmd.ai/policy-namespace=<ns>` | Label, Namespace | absent | Read this namespace's limiters and quotas from `<ns>`. A cluster admin sets it; a namespace admin cannot change it | live |

## Tune per workload

The settings that change how a model scales. Set them on the `default` entry
for every model, on a named tier for a class of models (a trigger picks it with
`scalingPolicy`), or on a per-model override.

### When to add and remove capacity

| Name | Where set | Default | What it changes | Applies |
| --- | --- | --- | --- | --- |
| **`scaleUpThreshold`** | Policy, any entry | `0.85` | How full a replica gets before capacity is added. Lower is more headroom and more GPUs | live |
| **`scaleDownBoundary`** | Policy, any entry | `0.70` | How empty the fleet gets before a replica is removed. Keep it below `scaleUpThreshold` | live |
| **`scalingPolicy`** | Trigger | the `default` entry's `defaultPolicy`, else `default` | The tier this workload scales under ([tiers](scaling-policy.md#3-named-policy-tiers)) | live |
| `defaultPolicy` | Policy, `default` | empty | The tier for workloads whose trigger names none | live |
| `model_id` + `namespace` | Policy, any key | none | Make an entry a per-model override ([overrides](scaling-policy.md#4-per-model-overrides)) | live |
| **`minReplicaCount`**, **`maxReplicaCount`** | ScaledObject | the plan writes `1` and `10` | The floor and ceiling KEDA holds the workload between. A floor is never taken by utilization share | live |
| `priority` | Policy, any entry | `1.0` | Weight when a limiter is short and models compete | live |
| `shapeChangeHoldSeconds` | Policy, any entry | `0` (built in: 5 minutes) | How long scale-down waits after the request shape changes. Raise it for very long generations | live |

### Sharing a quota

| Name | Where set | Default | What it changes | Applies |
| --- | --- | --- | --- | --- |
| **`weightClass`** | Policy, model or tier entry | `standard` | The model's share of spare GPUs: `best-effort` 0.5, `standard` 1, `important` 2, `critical` 4 ([for model owners](scaling-policy.md#for-model-owners-what-the-optimizer-may-take-and-how-to-protect-a-model)) | live |
| `weight` | Policy, model or tier entry | from `weightClass` | A numeric weight instead of a class | live |
| **`optimizer.utilizationShare.maxHeadroomReplicas`** | Policy, `default` | `1` | Spare replicas per unit of weight while the quota is contended | live |
| `optimizer.utilizationShare.reserveGPUs` | Policy, `default` | `0` | GPUs held back from sharing, for scale-from-zero | live |
| `optimizer.utilizationShare.namespaces.<ns>.enabled` | Policy, `default` | `true` | Take one namespace out of sharing | live |
| `optimizer.utilizationShare.clusterNamespaces` | Policy, `default` | every namespace | Under a cluster quota, share only in these; other models keep today's optimizer (a canary) | live |
| `limiters[].exclude` | Policy, `default` | none | Namespaces the limiter does not bound | live |
| `limiters[].kueue.countBorrowable`; installer `LLMSM_QUOTA_KUEUE_BORROW` | Policy, `default` | `true` | Count what Kueue lets a queue borrow from its cohort | live |

### Which analyzers decide

| Name | Where set | Default | What it changes | Applies |
| --- | --- | --- | --- | --- |
| **`analyzers`** | Policy, any entry | `saturation` alone | Add `throughput` to size from token throughput too, or `slo` to report latency targets. The model is sized to the largest need ([analyzers](scaling-policy.md#configuring-analyzers)) | live; enabling `throughput` for the first time needs a restart |
| **`slo.ttft.target`**, `slo.ttft.percentile` | Policy, tier entry | none; percentile `90` | Time-to-first-token target. Reported only: the `slo` analyzer never changes a replica count ([slo](scaling-policy.md#slo-observe-only)) | live |
| **`slo.tpot.target`** | Policy, tier entry | none | Mean time-per-output-token target. Reported only | live |

### Scale to zero and back

| Name | Where set | Default | What it changes | Applies |
| --- | --- | --- | --- | --- |
| **`scaleToZero.enabled`** | Policy, any entry | inherits `LLMSM_SCALE_TO_ZERO` | Allow this model to be parked at zero. It also needs `minReplicaCount: 0` ([scale to zero](scaling-policy.md#scaletozero)) | live |
| `LLMSM_SCALE_TO_ZERO`; installer `ENABLE_SCALE_TO_ZERO` | Manager ConfigMap | `false` in the code, the shipped ConfigMap and the installer. A re-install that does not name `ENABLE_SCALE_TO_ZERO` keeps the live value | The fleet default for `scaleToZero.enabled` | restart |
| `scaleToZero.retentionPeriod` | Policy, any entry | `10m` | Idle time before a model goes to zero | live |
| `scaleFromZero.requirePrefill` | Policy, any entry | `false` | Under P/D, wake prefill and decode together, never decode alone ([scale from zero](scaling-policy.md#scalefromzero)) | live |

Whatever the default, no model reaches zero until its ScaledObject's `minReplicaCount`
is `0`.

### Seeds the learners replace

The learners measure these per variant. A seed only covers the time before the
first measurement, so a figure of the right order is enough.

| Name | Where set | Default | What it changes | Applies |
| --- | --- | --- | --- | --- |
| `replicaStartSeconds` | Trigger | built in: 70 s | How long a new replica takes to become Ready, until three pods have been measured | live |
| `defaultOutputTokens` | Trigger | built in: 512 | Tokens one request generates, until a replica has completed one. Set it for long generations | live |
| `variantCost` | Trigger | `10.0` | Relative cost of one replica. Only the ratio between one model's variants matters | live |
| `llm-d.ai/max-num-seqs` | Annotation, Deployment or LeaderWorkerSet | none | A batch cap the controller cannot read from the engine's arguments ([batch cap](scaling-policy.md#the-batch-cap-llm-daimax-num-seqs-and-enablebatchcaplearning)) | live |
| `llm-d.ai/forget-learned-state` | Annotation, Deployment or LeaderWorkerSet | none | Drop what was learned for this variant, for example after a model change. Acted on once per value; change the value to ask again | live |

### Warm pool

| Name | Where set | Default | What it changes | Applies |
| --- | --- | --- | --- | --- |
| `warmPool` | Trigger, model's ScaledObject | none | The pool this model borrows a sleeping replica from while its own starts ([warm pool](scaledobject-trigger.md#warm-pool-keys)) | live |
| `warmPoolName` | Trigger, pool's ScaledObject | none | Marks a ScaledObject as a warm pool | live |
| `warmPoolCopies` | Trigger | automatic | How many sleeping copies of the model the pool holds | live |
| `warmPoolRetained` | Trigger, pool's ScaledObject | `false` | The pool is serving capacity, not a bridge ([retained pool](../well-lit-paths/retained-pool/)) | live |

## Not used

These names appear in manifests or older docs but change nothing:

- `defaultOutputTokens` on a scaling-policy entry: ignored on purpose. Set it on
  the trigger.
- `analyzerName` on a policy entry: it has one legal value, `saturation`.
- `--warm-pool-namespace`: ignored.
- `WEBHOOK_CERT_PATH`, `WEBHOOK_CERT_NAME`, `WEBHOOK_CERT_KEY`: the controller
  registers no webhooks.
