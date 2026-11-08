package steadystate

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// llmsm_learned_state_k2_source and llmsm_learned_state_k2_age_seconds are
// published beside the borrow gauges and deleted on the same paths. These
// drive the engine's methods, which is where a missing call would leave a
// variant's last k2 source published after it stopped being priced.

// k2Of describes the k2 a variant is priced on, for withK2.
type k2Of struct {
	role, source string
	age          time.Duration
	hasAge       bool
}

// withK2 is namedResult with a k2 source set on the named variants.
func withK2(nr allocation.NamedAnalyzerResult, k2 map[string]k2Of) allocation.NamedAnalyzerResult {
	for i := range nr.Result.VariantCapacities {
		vc := &nr.Result.VariantCapacities[i]
		if k, ok := k2[vc.VariantName]; ok {
			vc.Role, vc.K2Source, vc.K2Age, vc.HasK2Age = k.role, k.source, k.age, k.hasAge
		}
	}
	return nr
}

func TestRecordAnalyzerMetrics_PublishesTheK2SourceAndItsAge(t *testing.T) {
	e, registry := engineConfigEngine(t)
	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{
		withK2(namedResult("saturation", nil, 5, "main", "canary"), map[string]k2Of{
			"main":   {role: "decode", source: "P2-hist", age: 90 * time.Minute, hasAge: true},
			"canary": {role: "decode", source: "P4-k1"},
		}),
	})
	assert.Equal(t, map[string]float64{"P2-hist": 1, "P4-k1": 1},
		gaugeValues(t, registry, constants.LLMSMLearnedStateK2Source, constants.LabelSource))
	assert.Equal(t, map[string]float64{"main": 5400},
		gaugeValues(t, registry, constants.LLMSMLearnedStateK2AgeSeconds, constants.LabelVariantName),
		"only a measured figure has an age")
	assert.ElementsMatch(t, []string{"decode"},
		seriesLabels(t, registry, constants.LLMSMLearnedStateK2AgeSeconds, constants.LabelRole))
}

// A source that changes replaces the series under the old one, and a figure
// that stops being a measurement loses its age.
func TestRecordAnalyzerMetrics_ReplacesTheK2SourceWhenItChanges(t *testing.T) {
	e, registry := engineConfigEngine(t)
	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{
		withK2(namedResult("saturation", nil, 5, "canary"), map[string]k2Of{
			"canary": {role: "decode", source: "P2-borrow", age: time.Hour, hasAge: true},
		}),
	})
	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{
		withK2(namedResult("saturation", nil, 5, "canary"), map[string]k2Of{
			"canary": {role: "decode", source: "P1-obs", age: 0, hasAge: true},
		}),
	})
	assert.ElementsMatch(t, []string{"P1-obs"},
		seriesLabels(t, registry, constants.LLMSMLearnedStateK2Source, constants.LabelSource),
		"the source the variant left stayed published")
	assert.ElementsMatch(t, []string{"P1-obs"},
		seriesLabels(t, registry, constants.LLMSMLearnedStateK2AgeSeconds, constants.LabelSource))

	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{
		withK2(namedResult("saturation", nil, 5, "canary"), map[string]k2Of{
			"canary": {role: "decode", source: "P4-k1"},
		}),
	})
	assert.ElementsMatch(t, []string{"P4-k1"},
		seriesLabels(t, registry, constants.LLMSMLearnedStateK2Source, constants.LabelSource))
	assert.Empty(t, seriesLabels(t, registry, constants.LLMSMLearnedStateK2AgeSeconds, constants.LabelSource),
		"a figure that is no measurement kept the age of the last one")

	// The same source, its window gone: the source stays, its age goes.
	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{
		withK2(namedResult("saturation", nil, 5, "canary"), map[string]k2Of{
			"canary": {role: "decode", source: "P2-hist", age: time.Hour, hasAge: true},
		}),
	})
	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{
		withK2(namedResult("saturation", nil, 5, "canary"), map[string]k2Of{
			"canary": {role: "decode", source: "P2-hist"},
		}),
	})
	assert.ElementsMatch(t, []string{"P2-hist"},
		seriesLabels(t, registry, constants.LLMSMLearnedStateK2Source, constants.LabelSource))
	assert.Empty(t, seriesLabels(t, registry, constants.LLMSMLearnedStateK2AgeSeconds, constants.LabelSource),
		"an age with no measurement behind it stayed published")

	// No source at all this cycle: nothing is published for the variant.
	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{
		namedResult("saturation", nil, 5, "canary"),
	})
	assert.Empty(t, seriesLabels(t, registry, constants.LLMSMLearnedStateK2Source, constants.LabelSource),
		"a variant priced from no replica kept its last source")
}

// A variant whose role changes under the same source moves its series: the
// old role's source and age are not left beside the new ones.
func TestRecordAnalyzerMetrics_MovesTheK2SourceWhenTheRoleChanges(t *testing.T) {
	e, registry := engineConfigEngine(t)
	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{
		withK2(namedResult("saturation", nil, 5, "canary"), map[string]k2Of{
			"canary": {role: "decode", source: "P2-hist", age: time.Hour, hasAge: true},
		}),
	})
	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{
		withK2(namedResult("saturation", nil, 5, "canary"), map[string]k2Of{
			"canary": {role: "prefill", source: "P2-hist", age: time.Hour, hasAge: true},
		}),
	})
	assert.ElementsMatch(t, []string{"prefill"},
		seriesLabels(t, registry, constants.LLMSMLearnedStateK2Source, constants.LabelRole),
		"the role the variant left kept its k2 source")
	assert.ElementsMatch(t, []string{"prefill"},
		seriesLabels(t, registry, constants.LLMSMLearnedStateK2AgeSeconds, constants.LabelRole),
		"the role the variant left kept its k2 age")
}

// Another analyzer reporting the same variant with no k2 must not unpublish
// the saturation analyzer's source.
func TestRecordAnalyzerMetrics_GathersTheK2SourceAcrossAnalyzers(t *testing.T) {
	e, registry := engineConfigEngine(t)
	k2 := map[string]k2Of{"canary": {role: "decode", source: "P2-hist", age: time.Hour, hasAge: true}}
	for _, order := range [][]allocation.NamedAnalyzerResult{
		{withK2(namedResult("saturation", nil, 5, "canary"), k2), namedResult("throughput", nil, 5, "canary")},
		{namedResult("throughput", nil, 5, "canary"), withK2(namedResult("saturation", nil, 5, "canary"), k2)},
	} {
		e.recordAnalyzerMetrics("ns", "m", order)
		assert.ElementsMatch(t, []string{"P2-hist"},
			seriesLabels(t, registry, constants.LLMSMLearnedStateK2Source, constants.LabelSource),
			"an analyzer that learns no k2 unpublished the saturation analyzer's source")
	}
}

func TestK2SourceIsDeletedWithTheBorrows(t *testing.T) {
	k2 := map[string]k2Of{"canary": {role: "decode", source: "P2-hist", age: time.Hour, hasAge: true}}
	publish := func(e *Engine, model string, variants ...string) {
		e.recordAnalyzerMetrics("ns", model, []allocation.NamedAnalyzerResult{
			withK2(namedResult("saturation", nil, 5, variants...), k2),
		})
	}

	t.Run("a departed variant", func(t *testing.T) {
		e, registry := engineConfigEngine(t)
		publish(e, "m", "main", "canary")
		publish(e, "m", "main")
		assert.Empty(t, seriesLabels(t, registry, constants.LLMSMLearnedStateK2Source, constants.LabelVariantName))
		assert.Empty(t, seriesLabels(t, registry, constants.LLMSMLearnedStateK2AgeSeconds, constants.LabelVariantName))
	})

	t.Run("a model with no replica rows", func(t *testing.T) {
		e, registry := engineConfigEngine(t)
		publish(e, "m1", "canary")
		publish(e, "m2", "canary")
		e.zeroObservedReplicas("ns", "m1")
		assert.ElementsMatch(t, []string{"m2"},
			seriesLabels(t, registry, constants.LLMSMLearnedStateK2Source, constants.LabelModelName),
			"a model with no replica rows kept its k2 source published")
		assert.ElementsMatch(t, []string{"m2"},
			seriesLabels(t, registry, constants.LLMSMLearnedStateK2AgeSeconds, constants.LabelModelName))

		publish(e, "m1", "canary")
		assert.ElementsMatch(t, []string{"m1", "m2"},
			seriesLabels(t, registry, constants.LLMSMLearnedStateK2Source, constants.LabelModelName),
			"the first priced cycle after the gap did not publish again")
	})

	t.Run("a model no longer reconciled, and an idle cycle", func(t *testing.T) {
		e, registry := engineConfigEngine(t)
		publish(e, "m1", "canary")
		publish(e, "m2", "canary")
		e.pruneAnalyzerSeries(map[string]bool{"ns/m1": true})
		assert.ElementsMatch(t, []string{"m1"},
			seriesLabels(t, registry, constants.LLMSMLearnedStateK2Source, constants.LabelModelName))
		e.evictAllAnalyzerSeries()
		assert.Empty(t, seriesLabels(t, registry, constants.LLMSMLearnedStateK2Source, constants.LabelModelName))
		assert.Empty(t, seriesLabels(t, registry, constants.LLMSMLearnedStateK2AgeSeconds, constants.LabelModelName))
	})
}
