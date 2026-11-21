package saturation

import (
	"slices"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// SwitchMoEBusiestRank is the replay switch that fits the ITL line of a
// data-parallel MoE engine against its busiest rank's KV fill, not the mean
// over its ranks (busiestRankRows).
//
// vLLM runs the data-parallel ranks of an expert-parallel MoE engine in
// lockstep: each step is padded to the busiest rank's batch, idle ranks run
// dummy batches, and the expert all-to-all syncs every MoE layer, so the
// pod's step time follows its busiest rank. Its data-parallel balancer evens
// out request counts, not KV, so the busiest rank fills first: at a mean
// fill at the band's top it can be full, preempt, and stall every rank. A
// line and a band read on the mean put the knee too high (issue
// llm-d-incubation/llm-d-scaling-manager#312).
//
// On, for a variant whose engine runs expert parallelism over more than one
// data-parallel rank (BusiestRankFillFor), the line's readings and its band
// are the busiest rank's, and the capacity planner prices the mean fill at
// the band's top over the busiest/mean ratio its replay read
// (learnreplay.VariantState.BusiestRankRatio). Dense engines' ranks step
// independently and are unchanged. Planner-only and off from the
// constructor; the planner's replay does not turn it on
// (learnreplay.PlannerSwitches): no recording we hold runs data parallelism,
// so the evidence for it is synthetic.
const SwitchMoEBusiestRank = "moe-dp-busiest-rank"

// BusiestRankFillFor reports whether a variant run under params has its ITL
// line read at its busiest rank: SwitchMoEBusiestRank on, and a vLLM engine
// with expert parallelism over more than one data-parallel rank in the pod
// (--data-parallel-size-local where it is set, below the global size). The
// busiest/mean ratio is read per pod, so a pod of one rank reads none.
//
// Not covered: an MoE engine with data parallelism but no expert parallelism
// (vLLM steps those ranks in lockstep too; the arguments do not say the
// model is MoE), and SGLang, whose lockstep comes from --enable-dp-attention,
// which the parser does not read, and whose fill queries take the busiest
// rank already.
func (a *SaturationAnalyzer) BusiestRankFillFor(params *capacity.EngineParams) bool {
	if !a.moeBusiestRank || params == nil || params.Engine == inferenceengine.EngineSGLang || !params.ExpertParallel {
		return false
	}
	local := max(params.DataParallelSize, 1)
	if params.DataParallelSizeLocal > 0 {
		local = min(params.DataParallelSizeLocal, local)
	}
	return local > 1
}

// busiestRankRows is rows with each fill the ITL pairs with carried to the
// busiest rank: KvUsageMean and KvUsageInstant times the row's
// busiest/mean rank fill ratio (domain.ReplicaMetrics.BusiestRankFillRatio),
// at most 1, so KvUsageForITL reads BusiestRankKvUsageForITL. A row with no
// ratio read (one rank, or either series missing) keeps its mean. rows
// itself is not changed.
func busiestRankRows(rows []domain.ReplicaMetrics) []domain.ReplicaMetrics {
	out := slices.Clone(rows)
	for i := range out {
		r, ok := out[i].BusiestRankFillRatio()
		if !ok {
			continue
		}
		out[i].KvUsageMean = min(out[i].KvUsageMean*r, 1)
		out[i].KvUsageInstant = min(out[i].KvUsageInstant*r, 1)
	}
	return out
}
