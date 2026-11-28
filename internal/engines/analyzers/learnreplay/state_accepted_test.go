package learnreplay

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/slo"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// A row's sequences divide its step time by the tokens a step emits. A
// count of drafts over k by more than a rounding error is no count a step
// produced: the row reads the running gauge, and none without it. One over
// k by a rounding error is k; at or under k, and with no k stated, as read.
func TestRowSequences_WithholdsTheDraftsOverK(t *testing.T) {
	rm := fitRowInput(0.5, 210).ReplicaMetrics[0]
	rm.SpecAcceptedPerStep, rm.SpecAcceptedPerStepKnown = 4.5, true
	assert.Zero(t, rowSequences(rm, 3), "4.5 drafts over k 3, no gauge: none")
	rm.RunningRequests, rm.RunningRequestsKnown = 40, true
	assert.InDelta(t, 40, rowSequences(rm, 3), 1e-9, "4.5 drafts over k 3: the gauge")
	assert.InDelta(t, 210/5.5, rowSequences(rm, 0), 1e-9, "no k: as read")
	assert.InDelta(t, 210/5.5, rowSequences(rm, 5), 1e-9, "under k: as read")
	rm.SpecAcceptedPerStep = 3.000001
	assert.InDelta(t, 210/4.0, rowSequences(rm, 3), 1e-9, "over k by rounding: k")
}

// The decode series reads a row's accepted drafts against the k its variant
// was filed with, and a variant filed with no speculation as it is.
func TestDecodeSeries_ReadsTheDraftsAgainstTheFiledK(t *testing.T) {
	drafts := filedDrafts{}
	drafts.file(capacity.FileScaleTargetEvent{Namespace: "ns", ModelID: "m", Variant: "d1",
		Params: capacity.EngineParams{Engine: inferenceengine.EngineVLLM, SpeculativeConfig: `{"num_speculative_tokens":2}`}})
	drafts.file(capacity.FileScaleTargetEvent{Namespace: "ns", ModelID: "m", Variant: "d2",
		Params: capacity.EngineParams{Engine: inferenceengine.EngineVLLM}})
	row := func(v string) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{PodName: v + "-0", VariantName: v, Ready: true, RequestRate: 1, RequestRateKnown: true,
			AvgITL: 0.06, AvgITLKnown: true, SpecAcceptedPerStep: 3.5, SpecAcceptedPerStepKnown: true,
			RunningRequests: 4, RunningRequestsKnown: true}
	}
	at := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)
	in := seriesInput(1, row("d1"), row("d2"))
	c := decodeCycleOf(in, at, SeriesVariants{Decode: "d1"}, drafts.of(in))
	assert.Empty(t, c.Groups, "3.5 drafts over the filed k 2: no token time")
	assert.Equal(t, 1, c.RanksReported, "its running sequences are still read")

	fit := row("d1")
	fit.SpecAcceptedPerStep = 2
	in = seriesInput(1, fit, row("d2"))
	c = decodeCycleOf(in, at, SeriesVariants{Decode: "d1"}, drafts.of(in))
	require.Len(t, c.Groups, 1)
	assert.InDelta(t, 2, c.Groups[0].Accepted, 1e-12)
	assert.InDelta(t, 0.02, c.Groups[0].TokenTime, 1e-12, "a 60 ms step emitting 3 tokens")

	c = decodeCycleOf(in, at, SeriesVariants{Decode: "d2"}, drafts.of(in))
	require.Len(t, c.Groups, 1)
	assert.InDelta(t, 3.5, c.Groups[0].Accepted, 1e-12, "no k filed: as read")

	in = seriesInput(1, row("d1"))
	c = decodeCycleOf(in, at, SeriesVariants{Decode: "d1"}, nil)
	require.Len(t, c.Groups, 1)
	assert.InDelta(t, 3.5, c.Groups[0].Accepted, 1e-12, "switch off: as read")
}

// The planner reads the drafts against k; the controller's constructor does
// not (slo's TestSpecAcceptedAtMostK_OffFromTheConstructor).
func TestPlannerSwitches_TheSpecAcceptedAtMostKIsOn(t *testing.T) {
	assert.True(t, PlannerSwitches()[PackageSLO][slo.SwitchSpecAcceptedAtMostK])
	assert.Nil(t, maxAccepted(capacity.NewStore(), domain.AnalyzerInput{}, false), "off: read as they are")
}
