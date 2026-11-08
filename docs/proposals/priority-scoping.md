# Proposal: Scoping Priority to a Contention Domain

**Authors:** [TBD]
**Status:** Draft
**Created:** 2026-08-17
**Last Updated:** 2026-08-18

---

## Problem Statement

`priority` is a single float on a scaling-policy entry, compared against priorities
written by other people for other workloads. Nothing states whose numbers are
comparable with whose, so "what does `priority: 5` mean?" has no answer that
survives a second tenant.

**A tenant can weight themselves.** Any namespace with a tracked workload may carry
its own `scaling-manager-scaling-policy-config` (`shouldWatchNamespaceLocalConfigMap`), and a
namespace-local map **replaces** the global one rather than merging with it
(`resolveScalingPolicyConfigMap`). A tenant writes their own `default` entry and
their own `priority`, and in the greedy pass that number is compared directly
against every other namespace's.

**A platform owner cannot weight a namespace.** The lever they actually want —
"research gets a third of what production gets when they contend" — does not exist.
The only cluster-level lever is a quota, which is a hard bound, not a share.

### What is already scoped, and what is not

This makes the change much smaller than "priority is global" suggests, because the
two consumers of `priority` differ:

| consumer | contention domain today | cross-namespace? |
| --- | --- | --- |
| `computeRescaleTargets` (water-fill) | `(acceleratorType, namespace-quota \| cluster)` — `applyRescale` groups by exactly this | **No**, when the namespace has its own quota |
| `fairShareValue` (greedy ordering) | every model on the cluster, one flat list | **Yes**, always |

The rescale pass has already answered the question: a namespace with its own quota
is a closed domain, and priorities inside it compete only with each other. What it
lacks is any weight *between* domains. The greedy pass has not answered it at all —
`fsv = priority × Σ Score × demand` orders every model on the cluster together.

## Goals

- Give `priority` a stated contention domain, so a number is only compared with
  numbers written by the same owner.
- Let a platform owner weight namespaces against each other.
- Make tenant self-declaration structurally pointless rather than policed.
- Change nothing for existing configs unless opted in.

## Non-Goals

- **Replacing quotas.** Priority splits contended capacity; a quota bounds it.
  Limiters remain constraints only.
- **Preemption.** Nothing here evicts running replicas.
- **A new CRD.** This extends the existing ConfigMaps, as ScalingPolicy did.
- **Cross-accelerator fairness.** Each accelerator pool is priced and scheduled
  separately; standing in one says nothing about another.

## Scope decision

| option | verdict | why |
| --- | --- | --- |
| priority per namespace, cluster level | **adopt** | the missing knob; owner is unambiguous — the party that already writes cluster quotas |
| priority per model, namespace level | **adopt, as a relative weight** | tenants know which of *their* models matters; nobody else does |
| priority per variant | **reject** | variants of one model are substitutes the optimizer picks between on cost and capacity. A per-variant priority competes with that objective, and no diagnostic would separate "priority said so" from "the optimizer chose badly". `variantCost` is already the per-variant knob |
| priority per named policy | **adopt as the mechanism** | not a fourth scope: a tier carries a weight and models select it by name, so retuning a class is one edit. Already wired end to end (`scalingPolicy` metadata → variant spec → `ResolveScalingPolicyForTier`) |

## Design

### Effective weight

For model `m` of namespace `n`, evaluated **within one contention group** `g`:

```text
                            p(m)
  W(m, g) = Wns(n) × ──────────────────────
                      Σ p(m') , m' ∈ n ∩ g
```

- `Wns(n)` — namespace weight, cluster policy only, default `1.0`.
- `p(m)` — the model's resolved `priority` (default → tier → override, unchanged),
  default `1.0`.
- The denominator sums over models of that namespace **present in `g`**, so a
  namespace does not lose standing for models idle elsewhere.

A contention group is what `applyRescale` already groups by: an accelerator type
paired with a budget scope (a namespace quota, or the cluster).

**Invariants.**

1. *Scale invariance.* Multiplying every `p` in a namespace by any `k > 0` leaves
   `W` unchanged. This is what closes the self-weighting hole: a tenant writing
   `1000` everywhere gets exactly what `1.0` gave them.
2. *Per-group conservation.* Within a group, `Σ W(m, g)` over a namespace's models
   equals `Wns(n)`.

Invariant 2 holds **per group, not per cluster**. A namespace whose models span two
accelerator types is normalised independently in each, so it holds `Wns` in both
pools. That is intended and follows from the non-goal above: the pools are separate
budgets, and a share of one is not convertible into a share of the other. It is
called out because the weaker reading — "a namespace's total standing across the
cluster is `Wns`" — is false and would be a reasonable thing to assume.

### Which pass each weight actually affects

`Wns` is a *common factor* among a namespace's models. In a namespace-quota group
every model shares it, so it cancels and the group's split is unchanged.

That is not a defect; it is the division of labour:

> **A quota decides how much a namespace gets. A weight decides how a *shared* pool
> splits when several namespaces contend for it.**

A namespace with a closed quota is therefore unaffected by its weight, and should
be — its entitlement is already stated, in GPUs, by the quota. `Wns` governs
cluster-scoped rescale groups and the greedy ordering pass, which are exactly the
places where models from different namespaces meet.

This is worth stating in the operator docs, because "I set `production: 5.0` and
nothing changed" is the predictable first support question, and the answer is
"production has its own quota; the weight applies where you share".

### Config surface

`Wns` lives in the cluster policy ConfigMap beside `limiters`, which is already the
boundary a tenant cannot write across:

```yaml
# cluster policy ConfigMap (scaling-manager-policy, or the scaling manager system namespace)
namespaceWeights:
  default: 1.0          # reserved key: any namespace not listed
  production: 5.0
  research: 1.0
  batch: 0.2
```

The reserved `default` key matches `QuotaLimiterReservedNamespaceKey`, so the two
cluster-level maps read alike.

```go
// On ScalingPolicy, read ONLY from cluster policy — see NamespaceWeights().
NamespaceWeights map[string]float64 `yaml:"namespaceWeights,omitempty"`
```

Reading follows `effectiveLimitersLocked` exactly: cluster policy when present,
otherwise the **global** map's `default` entry — never a namespace-local map. That
single rule is the whole enforcement point; no policy engine, no admission webhook.

```go
func (c *Config) NamespaceWeight(ns string) float64 {
    w := c.effectiveNamespaceWeightsLocked()   // clusterPolicy ?: global["default"]
    if v, ok := w[ns]; ok {
        return v
    }
    if v, ok := w[QuotaLimiterReservedNamespaceKey]; ok {
        return v
    }
    return DefaultPriority   // 1.0
}
```

### Where it lands

| change | site |
| --- | --- |
| parse + validate `namespaceWeights` | `internal/config/saturation_scaling.go` |
| cluster-policy-only accessor | `internal/config/config.go`, beside `effectiveLimitersLocked` |
| normalise per group, apply to the weight | `internal/engines/allocation/rescale.go` — `rescaleInputsForGroup`, `w := m.Priority * m.Demand` |
| normalise per group, apply to ordering | `internal/engines/allocation/greedy_score_optimizer.go` — `fairShareValue` |
| deterministic tie-break | `sortByRemainingDesc` — see Review finding 3 |
| report the arithmetic | `internal/engines/steadystate/policy_report.go` |

### Validation

| rule | rationale |
| --- | --- |
| weight must be `> 0` | `0` means "never gets contended capacity", which is a quota of zero — one concept, one place |
| weight must be `<= MaxQuotaValue` | keeps `Σ` clear of overflow, matching the quota bound |
| a non-finite weight is rejected at load | a `NaN` propagates into every comparison in the group and orders arbitrarily |
| an unknown namespace key is reported, not rejected | it is almost always a typo, and it silently grants nothing. Reported like an unknown policy tier |

### Opt-in

Gate on the presence of `namespaceWeights`. Absent, resolution is exactly as today
— one flat cluster-wide comparison — at the cost of one map lookup.

Normalisation changes established behaviour: today a lone model at `priority: 10`
outranks a lone model at `1.0` in another namespace; under this design they are
equal unless the weights say otherwise. Cluster-wide opt-in is the right
granularity, because a half-migrated cluster comparing normalised weights against
raw ones would be worse than either.

## Observability

The effective weight is derived from three inputs across two ConfigMaps, one of
which may be absent, so "why did this model lose?" must be answerable without a
rebuild. Extend the existing effective-policy report (`policy_report.go`) — which
already logs the resolved band per model — with `Wns`, the raw `p`, the group's
namespace sum, and the resulting `W`.

Whether to also export a gauge is left open: a per-model weight is a value rather
than a diagnostic condition, so it does not belong on
`llmsm_model_scaling_blocked`, but a new gauge needs its own justification.

## Test plan

**Unit — the algebra, which is the part that must not be subtly wrong.**

- Scale invariance: multiply every `p` in a namespace by 1000, assert identical
  targets. This is invariant 1 and the security property both.
- Per-group conservation: `Σ W` over a namespace equals `Wns` within a group.
- Two accelerator types: assert a namespace holds `Wns` in *each*, pinning the
  corrected reading of invariant 2 so a later "fix" cannot quietly make it
  cluster-wide.
- Namespace-quota group: weights change nothing (the cancellation above).
- Cluster-scoped group with two namespaces: split follows `Wns` ratio.
- Absent `namespaceWeights`: golden test against today's targets, byte-identical.
- Validation: `0`, negative, `NaN`, over-max.

**E2E** — two namespaces contending for one accelerator pool with no namespace
quotas, weights 4:1, asserting the replica split follows the weights rather than
raw priorities. This is the only configuration in which the feature is observable
end to end, which is itself worth encoding.

## Migration

1. Land parsing, the accessor and `W(m, g)`, gated on presence. No config change,
   no behaviour change.
2. Document that a tier's or override's `priority` is namespace-relative once
   weights exist.
3. Platform owners add `namespaceWeights` when they want it.

No tenant config becomes invalid, and existing numbers keep their meaning relative
to that tenant's other models — the only meaning they ever reliably had.

## Alternatives considered

**Lock down namespace-local config instead.** Forbid `priority` in a namespace-local
map, or stop such maps replacing the global one. This addresses the tenant hole but
not the missing namespace knob, and it turns every tenant tuning request into a
platform-team ticket. Normalisation gets the same safety while leaving tenants
autonomous inside their share.

**Absolute weights with an admission check.** Keep priority absolute and validate
tenant values against a permitted ceiling. Needs a webhook, a per-namespace ceiling
to administer, and it still leaves numbers whose meaning depends on who wrote them.

**Reuse Kubernetes PriorityClass.** Right shape — name in the workload, value in a
cluster-scoped object — but it governs pod preemption and scheduling, not autoscaler
entitlement, and binding to it would make the scaling manager's decisions move when a cluster admin
retunes scheduling. The named-tier mechanism already gives the same split.

**Normalise by max instead of sum.** `p(m) / max(p)` avoids diluting a model when a
namespace adds unrelated workloads (see Review finding 1), but loses conservation:
a namespace's standing then grows with model count, which is the hole being closed.

## Review findings

The design above was reviewed against the code; these are the substantive results,
and the design incorporates them.

**1. Adding a model dilutes a namespace's existing models — accepted.** With a
fixed namespace share, a tenant who adds fifty unimportant models halves the weight
of their critical one. This is surprising, and it is nonetheless correct: the
alternative is that a tenant grows their standing by adding workloads, which is
precisely the hole being closed. Documented rather than mitigated; `max`
normalisation was considered and rejected under Alternatives.

**2. The claimed invariant was wrong.** The first draft asserted `Σ W` over a
namespace equals `Wns` without qualification. Groups are per accelerator type, so a
namespace spanning two pools holds `Wns` in each — its cluster-wide total is
`Wns × (pools it occupies)`. Corrected in the text, declared intended under
Non-Goals, and pinned by a test so it cannot be silently "fixed" later.

**3. Normalisation creates ties against a sort that cannot break them.**
`sortByRemainingDesc` is `sort.Slice` — unstable, with no tie-break. Today exact
ties are uncommon; under normalisation a namespace whose models all sit at the
default weight produces identical `W`, and equal-demand models then produce equal
`fsv`. Two models tying would be ordered arbitrarily, differently between cycles
and between controller replicas, for the same input. The design therefore requires
a deterministic tie-break on model key, matching what `modelPolicy` and
`FindModelOverride` already do. This is a pre-existing latent bug that the feature
would promote from rare to routine.

**4. `priority: 0` is not expressible and never was.** `ApplyDefaults` rewrites
`0` to `1.0`, while `Validate` accepts `>= 0` — so a config asking for zero
silently gets full default weight. The design does not change this, and rejects
`0` for `Wns` for consistency, but the existing asymmetry between the two functions
is worth fixing separately: accepting a value and then ignoring it is worse than
rejecting it.

**5. `fairShareValue` has a fallback that discards priority entirely.** When
`priority × weighted` is not positive it returns raw max demand, unweighted. Under
this design that path silently exits the fairness scheme. It is currently reachable
only via `Score = 0` or `priority = 0` — and per finding 4 the latter cannot occur
— so it is not a live defect, but any future change letting `W` reach zero would
make it one. Noted so the coupling is on record.

## Open questions

- **Does a namespace keep the right to disable rescale for its own quota group?**
  `RescaleFlags` is already resolved per namespace from that namespace's own config.
  Probably yes, since it governs only its own closed domain — but it should be a
  decision rather than an accident.
- **Should weight be per accelerator type?** `namespaceWeights` is a scalar, but
  groups are per type. A platform owner may well want "production dominates on
  H100, shares A100 evenly". The scalar form is a strict subset of a per-type map
  and can be widened compatibly later, so this proposes the scalar and leaves the
  door open.
- **Reserved-key collision.** A real namespace named `default` cannot be addressed,
  the same limitation `QuotaLimiterReservedNamespaceKey` already carries. Shared
  wart, shared fix, if ever.
