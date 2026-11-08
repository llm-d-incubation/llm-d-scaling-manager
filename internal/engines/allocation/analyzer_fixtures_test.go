package allocation

import (
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// satEntryFixture is the test-side builder for one analyzer entry in a
// ModelScalingRequest.
//
// It carries both halves of what the engine hands the optimizer in a single
// flat literal: the analyzer's own (D, P) signal, and — below the line — the
// aggregates the engine's capacity-build step derives from it. Production code
// cannot conflate the two, which is the point of the split: domain.AnalyzerResult
// has no supply and no scaling-signal fields, so an analyzer cannot write a
// supply that contradicts its own (D, P).
//
// Optimizer tests still need to state both, because they exercise what the
// optimizer *does with* a given (RequiredCapacity, SpareCapacity,
// RoleCapacities) — not how those were derived from (D, P). Deriving them here
// would couple every optimizer test to the threshold formula and make the
// intended scenario unreadable. Tests for the derivation itself live in
// internal/engines/saturation (engine_v2_capacity_build_test.go,
// engine_v2_threshold_test.go) and use the real types.
//
// named() performs the split, mirroring the engine's buildNamedResult.
type satEntryFixture struct {
	// --- Analyzer-owned: the pure (D, P) signal. ---
	AnalyzerName      string
	ModelID           string
	Namespace         string
	AnalyzedAt        time.Time
	VariantCapacities []vcFixture
	TotalDemand       float64
	RoleDemand        map[string]float64

	// --- Engine-owned: derived by buildCapacities in production. ---
	TotalSupply            float64
	TotalAnticipatedSupply float64
	Utilization            float64
	RequiredCapacity       float64
	SpareCapacity          float64
	RoleCapacities         map[string]domain.RoleCapacity
}

// vcFixture is one variant as a test states it: identity and capacity signal in
// a single literal, which is how a scenario reads most clearly at the call site.
//
// Production splits these across two owners — identity on domain.VariantMetadata
// from discovery, the (D, P) signal on domain.VariantCapacity from the analyzer —
// and deriveVariants/capacities below perform exactly that split, so fixtures
// still drive the real two-source join rather than a shortcut around it.
type vcFixture struct {
	VariantName     string
	AcceleratorName string
	Cost            float64
	Role            string

	ReplicaCount       int
	PendingReplicas    int
	PerReplicaCapacity float64
	Reason             string
	TotalDemand        float64
	Utilization        float64
}

// deriveVariants projects the identity half of a fixture onto the
// domain.VariantMetadata slice the discovery step would have produced.
func deriveVariants(f *satEntryFixture) []domain.VariantMetadata {
	if f == nil {
		return nil
	}
	out := make([]domain.VariantMetadata, 0, len(f.VariantCapacities))
	for _, vc := range f.VariantCapacities {
		out = append(out, domain.VariantMetadata{
			VariantName:     vc.VariantName,
			Role:            vc.Role,
			Cost:            vc.Cost,
			AcceleratorName: vc.AcceleratorName,
		})
	}
	return out
}

// capacities projects the signal half of a fixture onto the per-variant records
// an analyzer emits. Identity beyond the keying name and the role the analyzer
// attributed demand by is deliberately absent — that is the trimmed contract.
func (f *satEntryFixture) capacities() []domain.VariantCapacity {
	out := make([]domain.VariantCapacity, 0, len(f.VariantCapacities))
	for _, vc := range f.VariantCapacities {
		out = append(out, domain.VariantCapacity{
			VariantName:        vc.VariantName,
			Role:               vc.Role,
			ReplicaCount:       vc.ReplicaCount,
			PendingReplicas:    vc.PendingReplicas,
			PerReplicaCapacity: vc.PerReplicaCapacity,
			Reason:             vc.Reason,
			TotalDemand:        vc.TotalDemand,
			Utilization:        vc.Utilization,
		})
	}
	return out
}

// rec builds one variantRecord for the tests that drive a per-variant helper
// (variantsForRole, sortByCostEfficiencyAsc, …) directly rather than through an
// optimizer. Spelling out the embedded VariantMetadata at each of those call
// sites buries the one or two fields the test actually cares about.
func rec(name, role string, cost, prc float64) variantRecord {
	return variantRecord{
		VariantMetadata:    domain.VariantMetadata{VariantName: name, Role: role, Cost: cost},
		PerReplicaCapacity: prc,
	}
}

// withScore sets the fair-share weight the engine would have resolved from
// AnalyzerScoreConfig. Returns a copy so it chains off named().
func (n NamedAnalyzerResult) withScore() NamedAnalyzerResult {
	n.Score = 1.0
	return n
}

// result returns the analyzer-owned half, exactly as an analyzer would emit it.
func (f *satEntryFixture) result() *domain.AnalyzerResult {
	if f == nil {
		return nil
	}
	return &domain.AnalyzerResult{
		AnalyzerName:      f.AnalyzerName,
		ModelID:           f.ModelID,
		Namespace:         f.Namespace,
		AnalyzedAt:        f.AnalyzedAt,
		VariantCapacities: f.capacities(),
		TotalDemand:       f.TotalDemand,
		RoleDemand:        f.RoleDemand,
	}
}

// named builds the optimizer-facing entry the way the engine does: the
// analyzer's result under Result, the engine-owned aggregates alongside it, and
// the mutable Remaining/Spare counters seeded from RC/SC.
func (f *satEntryFixture) named() NamedAnalyzerResult {
	return NamedAnalyzerResult{
		Name:                   domain.SaturationAnalyzerName,
		Result:                 f.result(),
		TotalSupply:            f.TotalSupply,
		TotalAnticipatedSupply: f.TotalAnticipatedSupply,
		Utilization:            f.Utilization,
		RequiredCapacity:       f.RequiredCapacity,
		SpareCapacity:          f.SpareCapacity,
		RoleCapacities:         f.RoleCapacities,
		Remaining:              f.RequiredCapacity,
		Spare:                  f.SpareCapacity,
		Live:                   true,
	}
}
