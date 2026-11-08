package allocation

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// TestResolveSOSeparatesSaturationFromTheOthers: Others is the largest need
// among the analyzers other than saturation, also when saturation's is larger
// than all of them; the composite prices saturation's need as its own demand
// and the others' at saturation's PRC.
func TestResolveSOSeparatesSaturationFromTheOthers(t *testing.T) {
	sat := makeAnalyzer(domain.SaturationAnalyzerName, 800, 100) // 8
	a := makeAnalyzer("a", 50, 10)                               // 5
	b := makeAnalyzer("b", 60, 10)                               // 6

	d := ResolveSO([]NamedAnalyzerResult{a, sat, b}, &sat, "v")
	assert.Equal(t, 8.0, d.N)
	assert.True(t, d.Saturation)
	assert.True(t, d.OthersOK)
	assert.Equal(t, 6.0, d.Others)

	d = ResolveSO([]NamedAnalyzerResult{b, a}, &sat, "v")
	assert.Equal(t, 6.0, d.N)
	assert.False(t, d.Saturation, "saturation unlisted")
	assert.Equal(t, 6.0, d.Others)

	d = ResolveSO([]NamedAnalyzerResult{sat}, &sat, "v")
	assert.True(t, d.Saturation)
	assert.False(t, d.OthersOK)
}

// TestResolveSOSkipsANoDataVariantOfAnInformativeAnalyzer: an analyzer with a
// measured variant is eligible, and its no-data or error variants still
// define no need, whatever PRC they carry.
func TestResolveSOSkipsANoDataVariantOfAnInformativeAnalyzer(t *testing.T) {
	for _, reason := range []string{ReasonNoData, ReasonError} {
		e := NamedAnalyzerResult{
			Name: "throughput",
			Live: true,
			Result: &domain.AnalyzerResult{
				TotalDemand: 400,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "a", PerReplicaCapacity: 100, Reason: reasonMeasured},
					{VariantName: "b", PerReplicaCapacity: 200, Reason: reason},
				},
			},
		}
		entries := []NamedAnalyzerResult{e}
		b := ResolveSO(entries, nil, "b")
		assert.False(t, b.OK, reason)
		assert.Equal(t, DecisionNoSignal, b.Path, reason)
		a := ResolveSO(entries, nil, "a")
		assert.True(t, a.OK, reason)
		assert.Equal(t, 4.0, a.N, reason)
	}
}
