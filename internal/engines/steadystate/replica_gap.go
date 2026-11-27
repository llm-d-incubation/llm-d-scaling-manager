package steadystate

import (
	"context"
	"fmt"
	"slices"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/accelerator"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/slo"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
)

// The suggested/provided series (issue #439): per variant, X -- what the
// analyzers ask for before any limiter, maxReplicas ceiling or utilization
// share -- against Y, the target the controller sets (llmsm_desired_replicas),
// and the reasons between them. Nothing here changes a decision: the figures
// come from a cost-aware pass over copies of the requests, which the engine
// runs anyway when no limiter is declared, from a passive model's would-be
// pass fitted to the nodes (allocation.PassiveWouldBeFitted), from the targets
// a share planning in shadow would set (shareWouldBe), and from what the
// decision step already recorded on each decision.
//
// Where nothing is applied, Y is a would-be figure, and the series' mode label
// says so (replicaGapMode): a passive model's Y is its would-be target fitted
// to the nodes, and a variant a share plans in shadow has the share's would-be
// target as its Y. Only an active variant's Y is llmsm_desired_replicas.
//
// X is built from Y and what each stage took away, so the identity
// X - Y = sum(short) - borrowed holds by construction, and a stage after the
// decision step (the scale-to-zero enforcer, the sticky scale-down hold) moves
// X and Y together: it is not a reason the suggestion was not provided.

// replicaGapEntry is one variant's figures from the decision step.
type replicaGapEntry struct {
	// ask is the cost-aware target with no limiter, under the variant's
	// maxReplicas ceiling; overCap is what the ceilings cost the variant's
	// role, charged to the variants they held (replicaAsks).
	ask, overCap int
	// pass is the optimizer pass's target: after the limiters, before the
	// utilization share. For a passive model it is fitted to the nodes.
	pass int
	// limitShort is what the limiters held the variant below its ask, and
	// limitedBy which (LimitedBy, or ShadowLimitedBy for a passive model's
	// would-be pass): a role's shortfall, charged to the variants a limiter
	// held (planReplicaGaps).
	limitShort int
	limitedBy  string
	// shared says the utilization share set the target, shareTarget to what;
	// wouldBe that the share planned it in shadow, so set nothing.
	shared, wouldBe bool
	shareTarget     int
	// passive is a passive model's variant, and fitCut what node fit takes
	// from its would-be target (the decision's TargetReplicas, which is not
	// fitted to nodes).
	passive bool
	fitCut  int
	// held is a variant no InferencePool selects: nothing moves it.
	held bool
}

// replicaGapPlan is what the decision step knew of every variant it decided,
// keyed namespace/variant, and the quota constraints it decided under.
type replicaGapPlan struct {
	entries map[string]replicaGapEntry
	quotas  []*allocation.ResourceConstraints
}

// roleKey is the namespace/model/role a variant serves.
func roleKey(namespace, model, role string) string {
	return namespace + "/" + model + "/" + normalizeRole(role)
}

// planReplicaGaps records, after the optimizer pass and before the
// utilization share, each decision's pass target and what bound it, beside
// its ask. fitted is the passive models' would-be pass fitted to the nodes.
//
// A limiter's shortfall is counted per role: what the role's variants ask for
// less what the pass gave them, charged to the variants a limiter held, in
// name order, each at most its own ask less its pass. A sibling the pass grew
// in a held variant's place covers it: the role is not short by it.
func planReplicaGaps(ctx context.Context, requests []allocation.ModelScalingRequest,
	decisions, fitted []domain.VariantDecision, constraints []*allocation.ResourceConstraints) *replicaGapPlan {
	asks, overCap := replicaAsks(ctx, requests)
	passive := map[string]bool{}
	for _, r := range requests {
		if r.Passive {
			passive[utils.GetNamespacedKey(r.Namespace, r.ModelID)] = true
		}
	}
	fit := make(map[string]*domain.VariantDecision, len(fitted))
	for i := range fitted {
		fit[utils.GetNamespacedKey(fitted[i].Namespace, fitted[i].VariantName)] = &fitted[i]
	}
	p := &replicaGapPlan{entries: make(map[string]replicaGapEntry, len(decisions))}
	roleShort := map[string]int{} // per role: asked less given
	roleOf := map[string]string{}
	var limited []string
	for i := range decisions {
		d := &decisions[i]
		k := utils.GetNamespacedKey(d.Namespace, d.VariantName)
		ask, ok := asks[k]
		if !ok {
			ask = d.TargetReplicas
		}
		en := replicaGapEntry{ask: ask, overCap: overCap[k], pass: d.TargetReplicas, held: d.HeldUnrouted,
			passive: passive[utils.GetNamespacedKey(d.Namespace, d.ModelID)]}
		isLimited := false
		switch {
		case d.WasLimited:
			isLimited, en.limitedBy = true, d.LimitedBy
		case d.ShadowLimitedBy != "":
			isLimited, en.limitedBy = true, d.ShadowLimitedBy
		}
		if f, ok := fit[k]; ok && f.TargetReplicas < en.pass {
			en.fitCut, en.pass = en.pass-f.TargetReplicas, f.TargetReplicas
			isLimited, en.limitedBy = true, f.ShadowLimitedBy
			if en.limitedBy == "" {
				en.limitedBy = allocation.NodeFitLimiterName
			}
		}
		role := roleKey(d.Namespace, d.ModelID, d.Role)
		roleOf[k] = role
		roleShort[role] += en.ask - en.pass
		if isLimited && !en.held {
			limited = append(limited, k)
		}
		p.entries[k] = en
	}
	slices.Sort(limited)
	for _, k := range limited {
		en := p.entries[k]
		if n := min(en.ask-en.pass, roleShort[roleOf[k]]); n > 0 {
			en.limitShort = n
			roleShort[roleOf[k]] -= n
			p.entries[k] = en
		}
	}
	for _, c := range constraints {
		if c != nil && c.ProviderName != allocation.PhysicalLimiterName {
			p.quotas = append(p.quotas, c)
		}
	}
	return p
}

// noteShare records the targets the utilization share set over the pass.
func (p *replicaGapPlan) noteShare(decisions []domain.VariantDecision) {
	if p == nil {
		return
	}
	for i := range decisions {
		d := &decisions[i]
		if d.ReasonCategory() != domain.DecisionReasonUtilizationShare {
			continue
		}
		k := utils.GetNamespacedKey(d.Namespace, d.VariantName)
		if en, ok := p.entries[k]; ok {
			en.shared, en.wouldBe, en.shareTarget = true, false, d.TargetReplicas
			p.entries[k] = en
		}
	}
}

// noteShareWouldBe records the targets a share planning in shadow would set
// (utilizationShareState.wouldBe), keyed namespace/variant.
func (p *replicaGapPlan) noteShareWouldBe(targets map[string]int) {
	if p == nil {
		return
	}
	for k, t := range targets {
		if en, ok := p.entries[k]; ok {
			en.shared, en.wouldBe, en.shareTarget = true, true, t
			p.entries[k] = en
		}
	}
}

// replicaAsks is each variant's ask, keyed namespace/variant: the target the
// cost-aware optimizer -- the one the engine runs when no limiter is declared
// -- sets with no limiter, under each variant's maxReplicas ceiling. overCap
// is what the ceilings cost: per role, the ask without them less the ask with
// them, charged to the variants the ceilings held, in name order. A ceiling a
// sibling variant absorbs costs the role nothing and is charged to no one.
//
// Both passes run on copies: the optimizer writes the per-role spare maps of
// the request it is given, and the ceilings are cleared on the second copy.
func replicaAsks(ctx context.Context, requests []allocation.ModelScalingRequest) (map[string]int, map[string]int) {
	quiet := ctrl.LoggerInto(ctx, logr.Discard())
	pass := func(uncapped bool) []domain.VariantDecision {
		reqs := make([]allocation.ModelScalingRequest, len(requests))
		for i, r := range requests {
			r.CompositeSignal.RoleSpare, r.CompositeSignal.RoleReleasable = nil, nil
			r.VariantStates = slices.Clone(r.VariantStates)
			if uncapped {
				for j := range r.VariantStates {
					r.VariantStates[j].MaxReplicas = nil
				}
			}
			reqs[i] = r
		}
		return allocation.NewCostAwareOptimizer().Optimize(quiet, reqs, nil)
	}
	asks := map[string]int{}
	roleOf := map[string]string{}
	capped := map[string]int{}
	for _, d := range pass(false) {
		k := utils.GetNamespacedKey(d.Namespace, d.VariantName)
		asks[k] = d.TargetReplicas
		role := roleKey(d.Namespace, d.ModelID, d.Role)
		roleOf[k] = role
		capped[role] += d.TargetReplicas
	}
	uncapped := map[string]int{}
	over := map[string]int{} // per variant: the uncapped ask above the capped one
	for _, d := range pass(true) {
		k := utils.GetNamespacedKey(d.Namespace, d.VariantName)
		uncapped[roleKey(d.Namespace, d.ModelID, d.Role)] += d.TargetReplicas
		if extra := d.TargetReplicas - asks[k]; extra > 0 {
			over[k] = extra
		}
	}
	overCap := map[string]int{}
	keys := make([]string, 0, len(over))
	for k := range over {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	left := map[string]int{}
	for role, n := range uncapped {
		left[role] = max(0, n-capped[role])
	}
	for _, k := range keys {
		role := roleOf[k]
		if n := min(over[k], left[role]); n > 0 {
			overCap[k] = n
			left[role] -= n
		}
	}
	return asks, overCap
}

// shortReason is the llmsm_replicas_short reason, and its limiter_name, for
// the bound LimitedBy names.
func shortReason(limitedBy string) (string, string) {
	switch limitedBy {
	case allocation.PhysicalLimiterName, "":
		return constants.ReplicasShortReasonGPU, ""
	case allocation.NodeFitLimiterName:
		return constants.ReplicasShortReasonNodeFit, ""
	default:
		// A quota entry, or a Kueue borrowing rule a quota entry applies.
		return constants.ReplicasShortReasonQuota, limitedBy
	}
}

// replicaGapMode is the mode label of a variant's series: whether its Y is
// applied (active) or would-be (passive, shadow).
func replicaGapMode(en replicaGapEntry) string {
	switch {
	case en.passive:
		return constants.ReplicasModePassive
	case en.wouldBe:
		return constants.ReplicasModeShadow
	}
	return constants.ReplicasModeActive
}

// gap is one variant's suggestion against provided, the target the decision
// is applied with. A variant the decision step did not record -- or one held
// unrouted, which nothing moves -- is provided what it is suggested.
//
// Y is provided, unless nothing is applied: a passive model's Y is provided
// fitted to the nodes, and a share planning in shadow moves Y by what it would
// move the pass's target.
func (p *replicaGapPlan) gap(d *domain.VariantDecision, provided int) metrics.ReplicaGap {
	g := metrics.ReplicaGap{Namespace: d.Namespace, ModelName: d.ModelID, VariantName: d.VariantName,
		Role: normalizeRole(d.Role), Mode: constants.ReplicasModeActive, Suggested: provided, Provided: provided}
	if p == nil {
		return g
	}
	en, ok := p.entries[utils.GetNamespacedKey(d.Namespace, d.VariantName)]
	if !ok {
		return g
	}
	g.Mode = replicaGapMode(en)
	if en.held {
		return g
	}
	y := max(0, provided-en.fitCut)
	limitShort := en.limitShort
	maxShort := en.overCap
	lent, borrowed := 0, 0
	if en.shared {
		delta := en.shareTarget - en.pass
		if en.wouldBe {
			y = max(0, y+delta)
		}
		switch {
		case delta < 0:
			lent = -delta
		case delta > 0:
			// The share filled what the limiters, then the ceiling, left
			// short first; past the suggestion it is borrowed.
			take := min(delta, limitShort)
			limitShort -= take
			delta -= take
			take = min(delta, maxShort)
			maxShort -= take
			borrowed = delta - take
		}
	}
	g.Suggested, g.Provided = y, y
	reason, limiter := shortReason(en.limitedBy)
	for _, s := range []metrics.ReplicaShort{
		{Reason: reason, LimiterName: limiter, Replicas: limitShort},
		{Reason: constants.ReplicasShortReasonMaxReplicas, Replicas: maxShort},
		{Reason: constants.ReplicasShortReasonShareLent, Replicas: lent},
	} {
		if s.Replicas > 0 {
			g.Short = append(g.Short, s)
			g.Suggested += s.Replicas
		}
	}
	g.Suggested -= borrowed
	g.Borrowed = borrowed
	if g.Suggested < 0 {
		// A stage after the share lowered the target below what it
		// borrowed: nothing is suggested, and all of it is borrowed.
		g.Borrowed -= g.Suggested
		g.Suggested = 0
	}
	return g
}

// gapVariant is a variant's suggestion with what the quota series charge it.
type gapVariant struct {
	namespace, accelerator     string
	current, gpusPerReplica, x int
}

// quotasAfterSuggested is what each quota pool would have left with every
// variant it bounds at its suggestion: its limit, less what is held now, less
// the GPUs the suggestions add (plus those they free). A namespace-scoped
// quota is read per namespace, any other per accelerator across the cluster.
// An unlimited pool publishes nothing.
func (p *replicaGapPlan) quotasAfterSuggested(vs []gapVariant) []metrics.QuotaAfterSuggested {
	if p == nil {
		return nil
	}
	left := func(pools map[string]allocation.ResourcePool, acc string, pool allocation.ResourcePool, ns string) int {
		n := pool.Limit - pool.Used
		for _, v := range vs {
			if ns != "" && v.namespace != ns {
				continue
			}
			if k, ok := accelerator.FindKey(pools, v.accelerator); ok && k == acc {
				n -= (v.x - v.current) * max(1, v.gpusPerReplica)
			}
		}
		return n
	}
	var out []metrics.QuotaAfterSuggested
	for _, c := range p.quotas {
		if c.NamespaceScoped || c.NamespacePools != nil {
			for ns, pools := range c.NamespacePools {
				for acc, pool := range pools {
					if pool.Limit >= 0 {
						out = append(out, metrics.QuotaAfterSuggested{LimiterName: c.ProviderName, Scope: ns,
							Accelerator: acc, GPUs: left(pools, acc, pool, ns)})
					}
				}
			}
			continue
		}
		for acc, pool := range c.Pools {
			if pool.Limit >= 0 {
				out = append(out, metrics.QuotaAfterSuggested{LimiterName: c.ProviderName,
					Scope: constants.UtilizationShareClusterScope, Accelerator: acc, GPUs: left(c.Pools, acc, pool, "")})
			}
		}
	}
	return out
}

// replicaGapRecorder collects a cycle's gaps as its decisions are applied, and
// publishes them once they all are.
type replicaGapRecorder struct {
	plan     *replicaGapPlan
	gaps     []metrics.ReplicaGap
	variants []gapVariant
	// v2Ask is the slo analyzer's v2 TTFT prefill ask for a model this
	// cycle (slo.Analyzer.ModelPrefillAsk); nil reads none.
	v2Ask func(namespace, modelID string) (slo.ModelPrefillAsk, bool)
}

// record returns d's gap at provided, and keeps it for publish.
func (r *replicaGapRecorder) record(d *domain.VariantDecision, provided int) metrics.ReplicaGap {
	g := r.plan.gap(d, provided)
	r.gaps = append(r.gaps, g)
	r.variants = append(r.variants, gapVariant{namespace: d.Namespace, accelerator: d.AcceleratorName,
		current: d.CurrentReplicas, gpusPerReplica: d.GPUsPerReplica, x: g.Suggested})
	return g
}

// publish replaces the suggested/provided series with this cycle's, the v2
// would-be suggestions among them (v2Gaps).
func (r *replicaGapRecorder) publish() {
	metrics.PublishReplicaGaps(append(r.gaps, r.v2Gaps()...), r.plan.quotasAfterSuggested(r.variants))
}

// v2Gaps is what the analyzers would suggest for the prefill variant of a
// model nothing is applied to (mode passive or shadow) with the slo analyzer
// sizing it on the v2 TTFT model: the higher of their own suggestion and the
// v2 ask, as the combined ask takes the highest analyzer's. Published on
// llmsm_replicas_suggested under learner ttft-mixture, with no provided
// figure: nothing applies or limits it. An active model has none: its
// sizing stays on the default models until the v2 models' turn-on evidence
// passes (docs/proposals/predicted-latency-in-controller.md).
func (r *replicaGapRecorder) v2Gaps() []metrics.ReplicaGap {
	if r.v2Ask == nil {
		return nil
	}
	var out []metrics.ReplicaGap
	for _, g := range r.gaps {
		if g.Mode == constants.ReplicasModeActive || g.Role != domain.RolePrefill {
			continue
		}
		ask, ok := r.v2Ask(g.Namespace, g.ModelName)
		if !ok || ask.Variant != g.VariantName {
			continue
		}
		out = append(out, metrics.ReplicaGap{Namespace: g.Namespace, ModelName: g.ModelName, VariantName: g.VariantName,
			Role: g.Role, Mode: g.Mode, Learner: learn.TTFTMixture, Suggested: max(g.Suggested, ask.Pods)})
	}
	return out
}

// constrainedEventMessage is the ResourceConstrained Event's text: the
// limiter's reason, and the suggestion against what is provided.
func constrainedEventMessage(d *domain.VariantDecision, g metrics.ReplicaGap) string {
	return fmt.Sprintf("%s: suggested %d replicas, provided %d", constrainedMessage(d), g.Suggested, g.Provided)
}
