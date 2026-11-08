package allocation

import (
	"context"
	"math"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// NamedAnalyzerResult pairs an analyzer's name with its result, the engine-owned
// capacity aggregates derived from that result, and mutable working counters for
// the optimizer's allocation loop.
// It is the type of ModelScalingRequest.CompositeSignal and is
// only used inside the engine→optimizer contract; it is not a general-purpose
// interfaces type.
//
// The split of ownership is the point: Result carries the analyzer's pure
// (D, P) signal, and every field below it is written by the engine's
// capacity-build step from that signal. Nothing an analyzer returns can
// contradict the supply or the scaling signals the optimizer acts on.
//
// Remaining and Spare are initialised from RequiredCapacity and SpareCapacity
// by the engine (model scope) and decremented in place by applyAllocation as
// the optimizer allocates replicas.
// For disaggregated (P/D) models, the optimizer calls initRoleState
// to populate RoleSpare and RoleReleasable per role and initialize
// picker-local demand.
// The original Result values are never mutated.
type NamedAnalyzerResult struct {
	Name      string
	Result    *domain.AnalyzerResult
	Score     float64            // per-analyzer weight from AnalyzerScoreConfig; used for fair-share priority
	Remaining float64            // mutable remaining required capacity; P-scope for disaggregated, model-scope otherwise
	Spare     float64            // mutable remaining spare capacity; model-scope (non-disaggregated only)
	RoleSpare map[string]float64 // per-role mutable spare; set by initRoleState; nil for non-disaggregated
	// RoleReleasable is the per-role capacity that may actually be handed back,
	// measured against a target utilisation INSIDE the hysteresis band rather
	// than against its floor. RoleSpare answers "may this role release at all";
	// this answers "how many replicas", and they are different questions:
	// dividing RoleSpare by one replica requires the fleet to be a whole
	// replica below scaleDownBoundary, which at nine replicas means a
	// utilisation of 0.622 against a boundary of 0.70. Run T sat at 0.643 and
	// could not shed a replica it did not need.
	//
	// Mutable and decremented alongside RoleSpare as replicas are committed, so
	// a role with several variants cannot release the same capacity twice.
	RoleReleasable    map[string]float64
	ScaleUpThreshold  float64 // resolved scale-up threshold used to compute RC
	ScaleDownBoundary float64 // resolved scale-down boundary used to compute SC

	// Model-level supply aggregates — written by the engine's capacity-build
	// step. They are derived from Result.VariantCapacities so the linearity
	// invariant (supply = Σ_v replicas × per-replica P) holds by construction:
	//   TotalSupply            = Σ_v ReplicaCount × PerReplicaCapacity
	//   TotalAnticipatedSupply = Σ_v (ReplicaCount + PendingReplicas - StuckReplicas) × PerReplicaCapacity
	//   Utilization            = Result.TotalDemand / TotalSupply (0 when TotalSupply == 0)
	// TotalAnticipatedSupply counts pending replicas so they offset demand,
	// preventing double-scaling.
	TotalSupply            float64
	TotalAnticipatedSupply float64
	Utilization            float64

	// Scaling signals — written by the engine's capacity-build step; read by the
	// optimizer:
	//   RC = max(0, Result.TotalDemand/scaleUp − TotalAnticipatedSupply)
	//   SC = max(0, TotalSupply                − Result.TotalDemand/scaleDown)
	RequiredCapacity float64 // >0 means scale-up needed
	SpareCapacity    float64 // >0 means scale-down possible

	// RoleCapacities holds per-role capacity aggregation for P/D disaggregated
	// models. nil when no disaggregation is active (all variants are role
	// "both"). Assembled by the engine's capacity-build step from
	// Result.RoleDemand + per-variant supply.
	RoleCapacities map[string]domain.RoleCapacity

	// Live indicates the analyzer produced a non-error, informative result within the
	// staleness window. Set by the engine each cycle. Non-live analyzers are excluded
	// from the scale-down veto so a registered-but-uninformative analyzer (no metrics,
	// error state, never analyzed) cannot block scale-down. Recovery is automatic: a
	// fresh informative result makes it live again on the next cycle.
	Live bool

	// Holds is what the composite forbids each role this cycle, keyed by role
	// (domain.RoleBoth on an aggregated model); nil when nothing is held. The
	// engine has already zeroed the forbidden RequiredCapacity or
	// SpareCapacity; an optimizer that sizes from demand directly reads it here.
	Holds map[string]RoleHold

	// ReleaseBound is, for each role another analyzer's frame also sizes
	// (domain.RoleBoth on an aggregated model), the most capacity it may hand
	// back this cycle, in its own units, with every frame left at or below the
	// release target: OtherFrames[role].Releasable at it. initRoleState caps
	// RoleReleasable at it, so the release budget, the larger of RoleSpare and
	// RoleReleasable, honours it. nil when only saturation sizes the model.
	ReleaseBound map[string]float64

	// OtherFrames is, for the same roles, what the analyzers other than the
	// one the role is priced in read of it. The share optimizer and rescale
	// size from demand, at their own utilization, so they read it through
	// OtherFrameDemand: as demand, not as a floor, it holds a role no deeper
	// than its own analyzer would and still shares a shortfall by weight.
	OtherFrames map[string]OtherFrames
}

// FrameReading is one analyzer's reading of a role in its own frame: Supply,
// what its PRCs make of the running replicas; Demand; and Ratio, the most of
// its capacity one unit of the chosen frame's capacity is worth over those
// replicas, so shedding X of the chosen frame's capacity sheds at most
// X x Ratio of this one's.
type FrameReading struct {
	Supply, Demand, Ratio float64
}

// OtherFrames is a role's supply in the frame it is priced in, Supply, and
// the readings of the other analyzers that size it, Frames.
type OtherFrames struct {
	Supply float64
	Frames []FrameReading
}

// Releasable is the most of the chosen frame's capacity the role can shed
// with every other frame left at or below utilization u: for each frame its
// supply above D / u, over its ratio. +Inf when no frame constrains it, and
// 0 when one already sits above u.
func (o OtherFrames) Releasable(u float64) float64 {
	out := math.Inf(1)
	if !(u > 0) {
		return out
	}
	for _, f := range o.Frames {
		// !(x >= 0) also skips a NaN, which max and min would carry on.
		if !(f.Ratio > 0) || !(f.Supply >= 0) || !(f.Demand >= 0) {
			continue
		}
		out = min(out, max(f.Supply-f.Demand/u, 0)/f.Ratio)
	}
	return out
}

// Demand is the demand, in the chosen frame's units, that an optimizer
// sizing at utilization u reads as keeping what every other frame needs:
// u x (Supply - Releasable(u)). It is never above u x Supply, so another
// frame can stop a release but never grows the role -- growth follows the
// chosen frame alone -- and 0 when no frame constrains it.
func (o OtherFrames) Demand(u float64) float64 {
	rel := o.Releasable(u)
	if math.IsInf(rel, 1) || !(o.Supply > 0) {
		return 0
	}
	return u * max(o.Supply-rel, 0)
}

// RoleHold is what the composite forbids one role this cycle. The composite
// logs each held role with the reasons that hold it ("composite holds a
// role").
type RoleHold struct {
	// NoScaleUp: nothing may grow the role. Set when saturation is not
	// listed and no analyzer the policy lists can size the role, or, on
	// P/D, when another role of the model is held that way: grown against a
	// role that cannot grow, it is capacity the other cannot feed.
	NoScaleUp bool
	// NoScaleDown: nothing may shrink the role. Set when saturation is not
	// listed and no analyzer the policy lists can size the role; when a
	// listed analyzer other than saturation that covers it is inside its
	// staleness grace with no reading; or when saturation holds the role
	// (HeldRoleDemand) and another analyzer's demand replaced its held
	// figure, since scale-down needs every analyzer to agree.
	NoScaleDown bool
}

// ModelScalingRequest bundles the analyzer result with variant state for one model.
// The optimizer receives a slice of these — one per model — and produces decisions.
type ModelScalingRequest struct {
	ModelID         string
	Namespace       string
	CompositeSignal NamedAnalyzerResult // the one composed signal the engine hands the optimizer, already reduced to a single entry
	VariantStates   []domain.VariantReplicaState
	// Variants is the authoritative per-variant metadata (identity, cost,
	// accelerator, replica state) from the discovery step. When populated it is
	// the source of truth for variant metadata, superseding the copies the
	// saturation analyzer laundered onto its VariantCapacity output. May be nil
	// on non-primary paths (e.g. tests), in which case the optimizer falls back
	// to the analyzer-supplied metadata.
	Variants      []domain.VariantMetadata
	Priority      float64 // Model priority (default 1.0)
	Disaggregated bool    // true when model has prefill+decode variants

	// WeightClass and Weight are the model's utilization-share weight as its
	// scaling-policy entry states it (at most one is set). They are resolved
	// against the weight classes in force by config.UtilizationShare.Weight.
	WeightClass string
	Weight      config.ModelWeight

	// Passive is a model the controller observes and never scales
	// (docs/reference/scaling-policy.md, "passive"). Its decisions are
	// would-be figures: nothing acts on them, so the Kueue release paths
	// count no GPU its would-be shrink frees and lower nothing of it.
	Passive bool
}

// passiveModels is the requests' passive models, keyed "namespace/modelID".
func passiveModels(requests []ModelScalingRequest) map[string]bool {
	var out map[string]bool
	for _, req := range requests {
		if req.Passive {
			if out == nil {
				out = map[string]bool{}
			}
			out[req.Namespace+"/"+req.ModelID] = true
		}
	}
	return out
}

// ScalingOptimizer makes final scaling decisions for all models.
//
// Implementations:
//   - CostAwareOptimizer: processes each model independently, minimizes cost (unlimited mode)
//   - GreedyByScoreOptimizer: fair-shares GPUs across models (limited mode)
type ScalingOptimizer interface {
	// Name returns optimizer identifier for logging/metrics.
	Name() string

	// Optimize produces VariantDecisions from analyzer results and optional constraints.
	// constraints may be nil in unlimited mode.
	Optimize(ctx context.Context, requests []ModelScalingRequest, constraints []*ResourceConstraints) []domain.VariantDecision
}
