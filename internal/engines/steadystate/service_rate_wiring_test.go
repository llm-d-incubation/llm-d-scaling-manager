package steadystate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// The service-rate series are published from one place and deleted from
// three, the same shape as llmsm_engine_config (engine_config_wiring_test.go),
// and for the same reason these drive the engine's own methods: the emitter's
// specs stay green when a call site is deleted.

// serviceRateResult is a saturation result whose variants each carry a
// service-rate signal with a line, a sample count and a derived rate.
func serviceRateResult(pricingK float64, variants ...string) allocation.NamedAnalyzerResult {
	nr := namedResult(domain.SaturationAnalyzerName, nil, 5, variants...)
	nr.Result.PricingK = pricingK
	for i := range nr.Result.VariantCapacities {
		nr.Result.VariantCapacities[i].ServiceRate = &domain.ServiceRateSignal{
			HasLine: true, LineSlope: 0.03, LineIntercept: 0.002, ITLAtPricingK: 0.0224,
			ITLWindowed: true, LineSamples: 12,
			Role: domain.RoleDecode, Rate: 4.5, RateSource: domain.ServiceRateSourceDerived,
		}
	}
	return nr
}

var serviceRateFamilies = []string{
	constants.LLMSMITLLineSlope,
	constants.LLMSMITLLineIntercept,
	constants.LLMSMITLSecondsAtPricingK,
	constants.LLMSMITLLineSamples,
	constants.LLMSMServiceRatePerReplica,
}

func TestRecordAnalyzerMetrics_PublishesTheServiceRateSignal(t *testing.T) {
	e, registry := engineConfigEngine(t)

	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{serviceRateResult(0.68, "v1", "v2")})

	for _, name := range serviceRateFamilies {
		assert.ElementsMatch(t, []string{"v1", "v2"},
			seriesLabels(t, registry, name, constants.LabelVariantName), name)
	}
	assert.ElementsMatch(t, []string{"m"},
		seriesLabels(t, registry, constants.LLMSMPricingK, constants.LabelModelName))
	assert.ElementsMatch(t, []string{"derived", "derived"},
		seriesLabels(t, registry, constants.LLMSMServiceRatePerReplica, constants.LabelSource))
}

func TestRecordAnalyzerMetrics_PublishesNothingForAResultWithoutASignal(t *testing.T) {
	// Another analyzer's result carries no signal; it must not publish a zero.
	e, registry := engineConfigEngine(t)

	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{namedResult("throughput", nil, 5, "v1")})

	for _, name := range append(serviceRateFamilies, constants.LLMSMPricingK) {
		assert.Empty(t, seriesLabels(t, registry, name, constants.LabelModelName), name)
	}
}

func TestEvictStaleAnalyzerSeries_DeletesTheServiceRateOfADepartedVariant(t *testing.T) {
	e, registry := engineConfigEngine(t)

	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{serviceRateResult(0.68, "v1", "v2")})
	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{serviceRateResult(0.68, "v1")})

	for _, name := range serviceRateFamilies {
		assert.ElementsMatch(t, []string{"v1"},
			seriesLabels(t, registry, name, constants.LabelVariantName),
			"%s: a departed variant's series must go with its target", name)
	}
	assert.Len(t, seriesLabels(t, registry, constants.LLMSMPricingK, constants.LabelModelName), 1)
}

func TestEvictStaleAnalyzerSeries_DeletesPricingKNoLongerReported(t *testing.T) {
	e, registry := engineConfigEngine(t)

	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{serviceRateResult(0.68, "v1")})
	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{serviceRateResult(0, "v1")})

	assert.Empty(t, seriesLabels(t, registry, constants.LLMSMPricingK, constants.LabelModelName),
		"a pricing k the analyzer stopped reporting must not stay frozen")
}

func TestPruneAnalyzerSeries_DeletesTheServiceRateOfADepartedModel(t *testing.T) {
	// Two models with a same-named variant in one namespace: the prune must
	// take m2's series and leave m1's.
	e, registry := engineConfigEngine(t)
	for _, m := range []string{"m1", "m2"} {
		e.recordAnalyzerMetrics("ns", m, []allocation.NamedAnalyzerResult{serviceRateResult(0.68, "v")})
	}
	for _, name := range append(serviceRateFamilies, constants.LLMSMPricingK) {
		require.ElementsMatch(t, []string{"m1", "m2"}, seriesLabels(t, registry, name, constants.LabelModelName), name)
	}

	e.pruneAnalyzerSeries(map[string]bool{"ns/m1": true})

	for _, name := range append(serviceRateFamilies, constants.LLMSMPricingK) {
		assert.ElementsMatch(t, []string{"m1"}, seriesLabels(t, registry, name, constants.LabelModelName),
			"%s: a departed model's series must go, and only its", name)
	}
}

// A model with no replica rows skips analysis, and its service rate must not
// stay published at the last value for as long as it stays parked. Only its
// own series go -- the target series stay -- its observed-replicas count reads
// 0, and the next analyzed cycle publishes them again. Driven through
// onNothingObserved, the one call the skip path makes, so neither effect can
// be dropped from it alone.
func TestOnNothingObserved_SkippedModel(t *testing.T) {
	e, registry := engineConfigEngine(t)
	// Both models name their variant "v": a variant name is unique within a
	// model, not a namespace, so every delete must match on the model too.
	for _, m := range []string{"m1", "m2"} {
		nr := serviceRateResult(0.68, "v")
		nr.Result.VariantCapacities[0].ObservedReplicas = 3
		e.recordAnalyzerMetrics("ns", m, []allocation.NamedAnalyzerResult{nr})
	}
	require.Equal(t, map[string]float64{"m1": 3, "m2": 3},
		gaugeValues(t, registry, constants.LLMSMAnalyzerObservedReplicas, constants.LabelModelName))

	e.onNothingObserved("ns", "m2")

	assert.Equal(t, map[string]float64{"m1": 3, "m2": 0},
		gaugeValues(t, registry, constants.LLMSMAnalyzerObservedReplicas, constants.LabelModelName),
		"a skipped cycle must read as nothing observed for that model only")
	assert.Equal(t, map[string]float64{"m1": 4.5},
		gaugeValues(t, registry, constants.LLMSMServiceRatePerReplica, constants.LabelModelName),
		"the sibling model keeps its rate, at its value")

	for _, name := range append(serviceRateFamilies, constants.LLMSMPricingK) {
		assert.ElementsMatch(t, []string{"m1"}, seriesLabels(t, registry, name, constants.LabelModelName),
			"%s: a model priced on nothing this cycle must not keep its last reading", name)
	}
	assert.ElementsMatch(t, []string{"m1", "m2"},
		seriesLabels(t, registry, constants.LLMSMAnalyzerTarget, constants.LabelModelName),
		"the target series are kept through a skipped cycle")

	e.recordAnalyzerMetrics("ns", "m2", []allocation.NamedAnalyzerResult{serviceRateResult(0.68, "v")})
	for _, name := range append(serviceRateFamilies, constants.LLMSMPricingK) {
		assert.ElementsMatch(t, []string{"m1", "m2"}, seriesLabels(t, registry, name, constants.LabelModelName),
			"%s: the next analyzed cycle publishes again", name)
	}
	e.onNothingObserved("ns", "never-published")
}

func TestEvictAllAnalyzerSeries_ClearsEveryServiceRateSeries(t *testing.T) {
	e, registry := engineConfigEngine(t)
	for _, m := range []string{"m1", "m2"} {
		e.recordAnalyzerMetrics("ns", m, []allocation.NamedAnalyzerResult{serviceRateResult(0.68, "v")})
	}

	e.evictAllAnalyzerSeries()

	for _, name := range append(serviceRateFamilies, constants.LLMSMPricingK) {
		assert.Empty(t, seriesLabels(t, registry, name, constants.LabelModelName),
			"%s: an idle cycle must not leave a series with nothing to refresh it", name)
	}
}
