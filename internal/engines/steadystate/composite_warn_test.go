package steadystate

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// TestCompositeWarnsOfAModelLevelOnlyAnalyzerOnPD: an analyzer with no
// per-role demand defines no need for a prefill or decode variant. Listed on
// a P/D model it has no effect, and the composite says so; on an aggregated
// model it sizes as usual and nothing is logged.
func TestCompositeWarnsOfAModelLevelOnlyAnalyzerOnPD(t *testing.T) {
	const msg = "listed analyzer reports no per-role demand, so it cannot size this P/D model"
	modelLevel := liveEntry("throughput", 60, 10) // RoleDemand nil

	pd := allocation.NamedAnalyzerResult{
		Name: domain.SaturationAnalyzerName,
		Live: true,
		Result: &domain.AnalyzerResult{
			AnalyzerName: domain.SaturationAnalyzerName,
			RoleDemand:   map[string]float64{domain.RoleDecode: 900},
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "v1", Role: domain.RoleDecode, ReplicaCount: 2, PerReplicaCapacity: 300, Reason: "measured"},
			},
		},
	}
	// The capacity build joins discovery's role onto every analyzer's
	// variants (buildCapacities), so on P/D its variant reads as decode.
	onPD := liveEntry("throughput", 60, 10)
	onPD.Result.VariantCapacities[0].Role = domain.RoleDecode
	ctx, logs := zapObserverCtx(t)
	named := []allocation.NamedAnalyzerResult{pd, onPD}
	buildComposite(ctx, named, named, 0.85, 0.7)
	lines := logs.FilterMessage(msg).All()
	if assert.Len(t, lines, 1) {
		assert.Equal(t, "throughput", lines[0].ContextMap()["analyzer"])
	}
	assert.Equal(t, string(allocation.DecisionSingle), composeAll(ctx, named).Result.VariantCapacities[0].Reason,
		"saturation alone sizes decode")

	ctx, logs = zapObserverCtx(t)
	named = []allocation.NamedAnalyzerResult{liveEntry(domain.SaturationAnalyzerName, 800, 200), modelLevel}
	assert.Equal(t, string(allocation.DecisionAgree), composeAll(ctx, named).Result.VariantCapacities[0].Reason)
	assert.Empty(t, logs.FilterMessage(msg).All(), "aggregated: it sizes")
}

// composeAll builds the composite with every entry eligible.
func composeAll(ctx context.Context, named []allocation.NamedAnalyzerResult) allocation.NamedAnalyzerResult {
	return buildComposite(ctx, named, named, 0.85, 0.7)
}
