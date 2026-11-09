# Plans

Implementation plans and the design calls behind them, one directory per area.
A plan records what was decided and why, including what deviated once the work
landed — so read the status line first: several of these are finished, and one
is proposed and unscheduled.

These are **developer-facing working documents**, not a description of current
behaviour. For that, use the [reference](../README.md#reference--what-you-set-and-read) and
[concepts](../concepts/architecture.md) pages.

## analyzers

- **[Analyzer architecture refactor](analyzers/analyzer-architecture-refactor.md)**
  — implemented. Metadata discovery, and a pure contract for each analyzer:
  demand and a per-replica target. Its "After this plan" section records what
  changed since: analyzers now combine in the
  [composite](../proposals/composite-analyzer.md) before the optimizer.
- **[What `k2` measures, and why it has to persist](analyzers/k2-capacity-model.md)**
  — the design call on the capacity anchor. Over-estimating it breaks TTFT
  irrecoverably; under-estimating only costs money, which is why it never ages
  out.
- **[Retiring `kvCacheThreshold`](analyzers/kvcachethreshold-retirement.md)** —
  proposed, not scheduled. The correctness defects around the field were fixed
  separately; this is about the field itself.

- **[The next TTFT staircase](analyzers/ttft-staircase-repeat.md)** — proposed, not
  run. What the fresh frozen run must hold out and freeze, what it costs, and what
  to cut first.

## engine

- **[KEDA-driven discovery](engine/keda-driven-discovery.md)** — replacing
  listing and annotations with what KEDA already tells us. Increments 1–4
  landed; the remaining informer is named in the plan.
- **[Priority-weighted rescale, alpha](engine/rescale-alpha.md)** — a
  redistributive, priority-weighted allocation in the GPU-constrained optimizer.
