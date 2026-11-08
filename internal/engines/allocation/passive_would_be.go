package allocation

import (
	"context"
	"maps"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/accelerator"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// SplitPassive splits requests into the models the controller scales and the
// passive ones it only observes.
func SplitPassive(requests []ModelScalingRequest) (active, passive []ModelScalingRequest) {
	for _, req := range requests {
		if req.Passive {
			passive = append(passive, req)
		} else {
			active = append(active, req)
		}
	}
	return active, passive
}

// ChargeGrowth returns a copy of constraints in which each decision's growth
// -- (target - current) replicas of its GPUs -- is charged as used, in the
// cluster pools and in its namespace's pools. It is what a pass left for the
// models after it, at the level of the pools: node fit and cohort borrowing
// are not charged.
func ChargeGrowth(constraints []*ResourceConstraints, decisions []domain.VariantDecision) []*ResourceConstraints {
	out := make([]*ResourceConstraints, 0, len(constraints))
	for _, c := range constraints {
		if c == nil {
			out = append(out, nil)
			continue
		}
		cp := *c
		cp.Pools = maps.Clone(c.Pools)
		if c.NamespacePools != nil {
			cp.NamespacePools = make(map[string]map[string]ResourcePool, len(c.NamespacePools))
			for ns, pools := range c.NamespacePools {
				cp.NamespacePools[ns] = maps.Clone(pools)
			}
		}
		for _, d := range decisions {
			grow := (d.TargetReplicas - d.CurrentReplicas) * max(1, d.GPUsPerReplica)
			if grow <= 0 {
				continue
			}
			charged := false
			if k, ok := accelerator.FindKey(cp.Pools, d.AcceleratorName); ok {
				p := cp.Pools[k]
				p.Used += grow
				cp.Pools[k] = p
				charged = true
			}
			if pools := cp.NamespacePools[d.Namespace]; pools != nil {
				if k, ok := accelerator.FindKey(pools, d.AcceleratorName); ok {
					p := pools[k]
					p.Used += grow
					pools[k] = p
					charged = true
				}
			}
			if charged {
				cp.TotalUsed += grow
				cp.TotalAvail = max(0, cp.TotalAvail-grow)
			}
		}
		out = append(out, &cp)
	}
	return out
}

// PassiveWouldBe is the passive models' would-be decisions, over what the
// active models' decisions left of constraints. So a passive model's would-be
// growth takes nothing from an active model, and the active models -- decided
// by the caller without the passive ones -- are allocated exactly as if the
// passive ones were absent at their real usage, which the limiters already
// count.
//
// Under GreedyByScoreOptimizer the pass is quiet (ShadowOptimize: it
// publishes, counts and logs nothing). Any other optimizer runs Optimize on
// the passive models alone: the cost-aware optimizer decides each model on
// its own and publishes nothing, so that is the same pass.
func PassiveWouldBe(ctx context.Context, optimizer ScalingOptimizer, passive []ModelScalingRequest,
	constraints []*ResourceConstraints, active []domain.VariantDecision) []domain.VariantDecision {
	if len(passive) == 0 {
		return nil
	}
	left := ChargeGrowth(constraints, active)
	if g, ok := optimizer.(*GreedyByScoreOptimizer); ok {
		return g.ShadowOptimize(ctx, passive, left)
	}
	return optimizer.Optimize(ctx, passive, left)
}
