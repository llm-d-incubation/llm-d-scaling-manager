package steadystate

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// TestWarnUnsizableShortfallSkipsAnAnalyzerThatPricedNothing: the warning is
// for a shortfall no variant can absorb. An analyzer that priced no variant,
// as the slo analyzer does before its line is ready or for a role it does not
// price, carries demand with no PRC and has no capacity to be missing; warned,
// every such model logged a stuck-looking shortfall every cycle.
func TestWarnUnsizableShortfallSkipsAnAnalyzerThatPricedNothing(t *testing.T) {
	const msg = "analyzer needs capacity but no variant has a per-replica capacity to scale with"
	result := func(reason string) *allocation.NamedAnalyzerResult {
		return &allocation.NamedAnalyzerResult{
			RequiredCapacity: 588,
			Result: &domain.AnalyzerResult{
				AnalyzerName: "slo", ModelID: "m", Namespace: "ns", TotalDemand: 500,
				VariantCapacities: []domain.VariantCapacity{{VariantName: "v", Reason: reason}},
			},
		}
	}

	for _, reason := range []string{allocation.ReasonNoData, allocation.ReasonError} {
		ctx, logs := zapObserverCtx(t)
		warnUnsizableShortfall(ctx, result(reason))
		assert.Empty(t, logs.FilterMessage(msg).All(), reason)
	}

	ctx, logs := zapObserverCtx(t)
	warnUnsizableShortfall(ctx, result("measured"))
	assert.Len(t, logs.FilterMessage(msg).All(), 1, "a measured variant with no capacity is still a shortfall")
}
