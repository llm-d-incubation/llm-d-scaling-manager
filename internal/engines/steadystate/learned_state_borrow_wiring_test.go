package steadystate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// llmsm_learned_state_borrowed is published from the engine's analyzer-series
// path and deleted from three places beside llmsm_engine_config. The emitter has
// its own tests; these drive the engine's methods, which is where a missing
// call would leave a borrow published after it ended.

// withBorrows is namedResult with borrows set on the named variants.
func withBorrows(nr allocation.NamedAnalyzerResult, borrows map[string][]domain.LearnedStateBorrow) allocation.NamedAnalyzerResult {
	for i := range nr.Result.VariantCapacities {
		vc := &nr.Result.VariantCapacities[i]
		vc.Borrowed = borrows[vc.VariantName]
	}
	return nr
}

func TestRecordAnalyzerMetrics_PublishesABorrowAndRemovesItWhenItEnds(t *testing.T) {
	e, registry := engineConfigEngine(t)
	k2 := map[string][]domain.LearnedStateBorrow{
		"canary": {{Quantity: domain.BorrowedK2, Donor: "main", Relaxed: 0.1}},
	}
	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{
		withBorrows(namedResult("saturation", nil, 5, "main", "canary"), k2),
	})
	require.ElementsMatch(t, []string{"canary"},
		seriesLabels(t, registry, constants.LLMSMLearnedStateBorrowed, constants.LabelVariantName))
	require.ElementsMatch(t, []string{"main"},
		seriesLabels(t, registry, constants.LLMSMLearnedStateBorrowed, constants.LabelDonor))

	// The canary measured its own k2: the next result carries no borrow.
	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{
		namedResult("saturation", nil, 5, "main", "canary"),
	})
	assert.Empty(t, seriesLabels(t, registry, constants.LLMSMLearnedStateBorrowed, constants.LabelVariantName),
		"a borrow that ended stayed published")
	assert.Empty(t, seriesLabels(t, registry, constants.LLMSMLearnedStateBorrowRelaxed, constants.LabelVariantName),
		"the relaxation of a borrow that ended stayed published")
}

// Another analyzer reporting the same variant with nothing borrowed must not
// unpublish the saturation analyzer's borrow.
func TestRecordAnalyzerMetrics_GathersBorrowsAcrossAnalyzers(t *testing.T) {
	e, registry := engineConfigEngine(t)
	k2 := map[string][]domain.LearnedStateBorrow{
		"canary": {{Quantity: domain.BorrowedK2, Donor: "main"}},
	}
	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{
		withBorrows(namedResult("saturation", nil, 5, "canary"), k2),
		namedResult("throughput", nil, 5, "canary"),
	})
	assert.ElementsMatch(t, []string{"canary"},
		seriesLabels(t, registry, constants.LLMSMLearnedStateBorrowed, constants.LabelVariantName),
		"an analyzer that borrows nothing unpublished another's borrow")
}

// A variant one analyzer stops reporting while saturation still prices it has
// not left, so its borrow stays published.
func TestEvictStaleAnalyzerSeries_KeepsABorrowAnotherAnalyzerStillReports(t *testing.T) {
	e, registry := engineConfigEngine(t)
	k2 := map[string][]domain.LearnedStateBorrow{
		"canary": {{Quantity: domain.BorrowedK2, Donor: "main"}},
	}
	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{
		withBorrows(namedResult("saturation", nil, 5, "main", "canary"), k2),
		namedResult("throughput", nil, 5, "main", "canary"),
	})
	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{
		withBorrows(namedResult("saturation", nil, 5, "main", "canary"), k2),
		namedResult("throughput", nil, 5, "main"),
	})
	assert.ElementsMatch(t, []string{"canary"},
		seriesLabels(t, registry, constants.LLMSMLearnedStateBorrowed, constants.LabelVariantName),
		"one analyzer dropping the variant unpublished a borrow saturation still reports")
}

func TestEvictStaleAnalyzerSeries_DeletesTheBorrowOfADepartedVariant(t *testing.T) {
	e, registry := engineConfigEngine(t)
	k2 := map[string][]domain.LearnedStateBorrow{
		"canary": {{Quantity: domain.BorrowedK2, Donor: "main"}},
	}
	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{
		withBorrows(namedResult("saturation", nil, 5, "main", "canary"), k2),
	})
	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{
		namedResult("saturation", nil, 5, "main"),
	})
	assert.Empty(t, seriesLabels(t, registry, constants.LLMSMLearnedStateBorrowed, constants.LabelVariantName),
		"a departed variant's borrow must go with its analyzer series")
}

// A cycle with no replica rows prices nothing and so borrows nothing: the
// docs say the series is absent then, and a parked model must not keep the
// borrow of its last priced cycle published for as long as it is parked.
func TestZeroObservedReplicas_DeletesTheModelsBorrows(t *testing.T) {
	e, registry := engineConfigEngine(t)
	k2 := map[string][]domain.LearnedStateBorrow{
		"canary": {{Quantity: domain.BorrowedK2, Donor: "main", Relaxed: 0.1}},
	}
	for _, m := range []string{"m1", "m2"} {
		e.recordAnalyzerMetrics("ns", m, []allocation.NamedAnalyzerResult{
			withBorrows(namedResult("saturation", nil, 5, "main", "canary"), k2),
		})
	}

	e.zeroObservedReplicas("ns", "m1")
	assert.ElementsMatch(t, []string{"m2"},
		seriesLabels(t, registry, constants.LLMSMLearnedStateBorrowed, constants.LabelModelName),
		"a model with no replica rows kept its borrow published")
	assert.ElementsMatch(t, []string{"m2"},
		seriesLabels(t, registry, constants.LLMSMLearnedStateBorrowRelaxed, constants.LabelModelName),
		"a model with no replica rows kept its relaxation published")

	// The first priced cycle after the gap publishes the borrow again.
	e.recordAnalyzerMetrics("ns", "m1", []allocation.NamedAnalyzerResult{
		withBorrows(namedResult("saturation", nil, 5, "main", "canary"), k2),
	})
	assert.ElementsMatch(t, []string{"m1", "m2"},
		seriesLabels(t, registry, constants.LLMSMLearnedStateBorrowed, constants.LabelModelName),
		"a borrow made after the gap was not published")
}

func TestPruneAndEvictAll_DeleteTheBorrowsOfADepartedModel(t *testing.T) {
	e, registry := engineConfigEngine(t)
	k2 := map[string][]domain.LearnedStateBorrow{
		"canary": {{Quantity: domain.BorrowedK2, Donor: "main"}},
	}
	for _, m := range []string{"m1", "m2"} {
		e.recordAnalyzerMetrics("ns", m, []allocation.NamedAnalyzerResult{
			withBorrows(namedResult("saturation", nil, 5, "canary"), k2),
		})
	}
	e.pruneAnalyzerSeries(map[string]bool{"ns/m1": true})
	assert.ElementsMatch(t, []string{"m1"},
		seriesLabels(t, registry, constants.LLMSMLearnedStateBorrowed, constants.LabelModelName),
		"a model no longer reconciled kept its borrow published")

	e.evictAllAnalyzerSeries()
	assert.Empty(t, seriesLabels(t, registry, constants.LLMSMLearnedStateBorrowed, constants.LabelModelName),
		"an idle cycle left a borrow published with nothing to refresh it")
}
