# Well-lit paths

A well-lit path is a recipe for one scenario: what you get, what it costs,
when not to take it, and the evidence that it works. Pick the row that names
your problem. Every path except the first assumes
[the saturation path](scale-on-saturation/) is in place, and that a new
replica starts fast ([start-path checklist](../reference/workload-preparation.md#the-rest-of-the-start-path)).

| Path | What you get | Maturity | Start with |
| --- | --- | --- | --- |
| [Scale a model on saturation](scale-on-saturation/) | Replica counts set by what the engines are doing, not by hand or a CPU threshold | Stable | [Install in a namespace](../guides/install-in-namespace/) |
| [Scale to zero, and get back](scale-to-zero/) | Idle models give their accelerators back and wake on the next request | Stable | [Scale to zero](../guides/scale-to-zero/) |
| [Bridge a scale-up with a warm pool](warm-pool-bridge/) | A pre-loaded Pod serves a spike while the new replica starts | Stable | [Warm pool](../guides/warm-pool/) |
| [Hold several large models on one set of GPUs](retained-pool/) | Models too large to start on demand share GPUs, one awake at a time | **Experimental** | [Retained pools](../guides/warm-pool/multi-pool.md#retained-pools-which-model-holds-the-gpus) |
| [Bound a fleet by real GPUs](bound-by-gpus/) | Autoscaling never asks for more GPUs than the cluster has | Stable | [Bound every install by real GPUs](../guides/admin-gpu-bounding/) |
| [Cap what each tenant may take](tenant-gpu-quotas/) | Each team's autoscaling stays inside its promised GPU number | Stable (control path) | [The path's steps](tenant-gpu-quotas/#start-here) |
| [Bound tenants by the quotas Kueue already holds](kueue-bounded-quotas/) | Tenant caps read from Kueue, with no second copy, and cohort borrowing where it is safe | **Experimental** | [Borrow from a Kueue cohort](../guides/kueue-borrowing/) |
| [Share one GPU quota between models](utilization-share/) | Models that peak at different times share one quota on purpose | **Experimental** | [Utilization share](../guides/utilization-share/) |
| [Give classes of workloads different scaling behaviour](workload-classes/) | Interactive and batch tiers, without per-model config | Stable | [The path's steps](workload-classes/#start-here) |
| [Serve one model on two accelerator variants](accelerator-variants/) | The cost-efficient variant grows first | Stable | [Register the workloads](../guides/install-in-namespace/README.md#4-register-the-workloads) |
| [Scale a P/D-disaggregated model](pd-disaggregation/) | Prefill and decode scaled apart, each on its own evidence | **Experimental** | [Register the workloads](../guides/install-in-namespace/README.md#4-register-the-workloads) |

**Experimental** means the recipe may still change: the defaults are not
settled, or a leg of testing is missing. Each page says which. **Stable** does
not mean fully covered by CI: each page's *How it is tested* names what is not
covered. A **guide** ([guides/](../guides/)) is the steps; a path links to it
and does not repeat them. All the evidence side by side:
[how mature each part is](../why.md#how-mature-each-part-is).
