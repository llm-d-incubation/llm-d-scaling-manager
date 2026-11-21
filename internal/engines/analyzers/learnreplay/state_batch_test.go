package learnreplay

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// batchInput is one cycle of one decode pod at fill k running n sequences
// on one rank, its mean ITL itlSec.
func batchInput(k float64, n int, itlSec float64) domain.AnalyzerInput {
	return domain.AnalyzerInput{Config: &config.ScalingPolicy{}, ReplicaMetrics: []domain.ReplicaMetrics{{
		PodName: "p", VariantName: "v", Ready: true, KvUsageMean: k, KvUsageMeanKnown: true,
		AvgITL: itlSec, AvgITLKnown: true, RunningRequests: n, RunningRequestsKnown: true,
	}}}
}

// batchFixture is a decode variant's rows over two shapes, at KVreq 1500 and
// 8000 on C = 764,672 tokens, whose ITL is the line plus 0.034 ms a sequence
// past 120: the term a joint fit of the q32j warm-up and its 1000/1000 phase
// gave. Rows outside the band and rows with no running count are noted too,
// and must be left out.
func batchFixture(line itl.Model) []domain.AnalyzerInput {
	term := whatif.BatchTerm{Slope: 0.034e-3, Roof: 120, Ranks: 1}
	ins := make([]domain.AnalyzerInput, 0, 203)
	for _, kv := range []float64{1500, 8000} {
		for i := range 100 {
			k := 0.15 + 0.35*float64(i)/99
			n := int(min(k*764672/kv, 256))
			// A deterministic wobble of 0.2 ms, so no fit is exact.
			wobble := 0.2e-3 * float64(i%5-2) / 2
			ins = append(ins, batchInput(k, n, line.ITLAt(k)+term.At(float64(n))+wobble))
		}
	}
	unread := batchInput(0.3, 200, 0.05)
	unread.ReplicaMetrics[0].RunningRequestsKnown = false
	return append(ins, batchInput(0.9, 256, 0.05), batchInput(0.05, 10, 0.05), unread)
}

// With the switch on, the rows in the band are kept and the term is fitted
// on the residuals of the variant's line; with it off, nothing is kept and
// the fit is the zero value.
func TestBatchSamples_FitsTheDecodeVariantWithTheSwitch(t *testing.T) {
	line := itl.Model{A: 0.03, B: 0.012}
	for _, on := range []bool{true, false} {
		b := newBatchSamples(on)
		for _, in := range batchFixture(line) {
			b.note(in)
		}
		st := &State{Variants: []VariantState{
			{Variant: "v", Role: "decode", Line: line, Ranks: 1},
			{Variant: "pf", Role: "prefill", Line: line, Ranks: 1},
		}}
		b.fit(st)
		if !on {
			assert.Empty(t, b.rows, "off keeps nothing")
			assert.Equal(t, whatif.BatchFit{}, st.Variants[0].BatchFit)
			continue
		}
		fit := st.Variants[0].BatchFit
		require.Empty(t, fit.Why, "%+v", fit)
		assert.Equal(t, 200, fit.Rows, "the rows in the band with a running count")
		assert.InDelta(t, 0.034e-3, fit.Term.Slope, 0.004e-3)
		assert.InDelta(t, 120, fit.Term.Roof, 15)
		assert.Equal(t, whatif.BatchFit{}, st.Variants[1].BatchFit, "a prefill variant is not fitted")
	}
}

// Through a replay: on, the decode rows reach the fit (a one-shape sweep, so
// the gates refuse the term: its rows cannot tell it from the line's slope);
// off, the state carries no fit. And the states read at earlier cycles fit as
// a replay stopped there does.
func TestStateAt_BatchTermThroughTheReplay(t *testing.T) {
	rec := olsSweepRecording(t)
	on := Options{Switches: map[string]map[string]bool{PackageSaturation: {saturation.SwitchITLBatchTerm: true}}}
	st, err := StateAt(context.Background(), rec, on, StateQuery{})
	require.NoError(t, err)
	var rows int
	for _, v := range st.Variants {
		rows += v.BatchFit.Rows
		t.Logf("%s: %+v", v.Variant, v.BatchFit)
		if v.BatchFit.Rows > 0 && v.BatchFit.Why == "" {
			assert.False(t, v.BatchFit.Term.IsZero(), "an accepted fit carries its term")
		}
	}
	assert.Positive(t, rows, "the replay's decode rows reach the fit")

	off := Options{Switches: map[string]map[string]bool{PackageSaturation: {saturation.SwitchITLBatchTerm: false}}}
	st, err = StateAt(context.Background(), rec, off, StateQuery{})
	require.NoError(t, err)
	for _, v := range st.Variants {
		assert.Equal(t, whatif.BatchFit{}, v.BatchFit, "off: no fit")
	}

	src := BytesSource(rec)
	_, early, err := StatesFrom(context.Background(), src, on, StateQuery{}, func(c int) []int { return []int{c / 2} })
	require.NoError(t, err)
	for c, got := range early {
		want, err := StateFrom(context.Background(), src, on, StateQuery{Cycle: c})
		require.NoError(t, err)
		for i := range want.Variants {
			assert.Equal(t, want.Variants[i].BatchFit, got.Variants[i].BatchFit, "cycle %d", c)
		}
	}
}

// A speculating vLLM variant is not fitted: its rows read the step's mean ITL
// while its line may be per accepted token, so their residuals are not the
// batch's cost.
func TestBatchSamples_ASpeculatingVariantIsRefused(t *testing.T) {
	line := itl.Model{A: 0.03, B: 0.012}
	b := newBatchSamples(true)
	for _, in := range batchFixture(line) {
		b.note(in)
	}
	spec := &capacity.EngineParams{Engine: inferenceengine.EngineVLLM, SpeculativeConfig: `{"method":"mtp","num_speculative_tokens":1}`}
	st := &State{Variants: []VariantState{{Variant: "v", Role: "decode", Line: line, Ranks: 1, Params: spec}}}
	b.fit(st)
	fit := st.Variants[0].BatchFit
	assert.Equal(t, whatif.BatchTermSpeculative, fit.Why)
	assert.True(t, fit.Term.IsZero())
	assert.Equal(t, 200, fit.Rows)
}

// The term is priced per rank as its rows were read: rows that did not read
// their ranks divide by one, so the term carries one rank even where the
// variant is configured with eight.
func TestBatchSamples_TheTermsRanksAreTheRows(t *testing.T) {
	line := itl.Model{A: 0.03, B: 0.012}
	b := newBatchSamples(true)
	for _, in := range batchFixture(line) {
		b.note(in)
	}
	st := &State{Variants: []VariantState{{Variant: "v", Role: "decode", Line: line, Ranks: 8}}}
	b.fit(st)
	require.Empty(t, st.Variants[0].BatchFit.Why)
	assert.Equal(t, 1, st.Variants[0].BatchFit.Term.Ranks)
}
