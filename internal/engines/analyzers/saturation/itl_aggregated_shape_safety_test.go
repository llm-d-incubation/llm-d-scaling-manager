package saturation

import (
	"encoding/json"
	"testing"

	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// The safety rules of the aggregated line by shape (issue ev-shindin/llm-scaling-manager#275), each pinned
// through Analyze where it can be.

// A scale-up whose cold replicas move the hit rate is not a shape change.
//
//   - Cold replicas that serve little barely move the prompt-weighted hit
//     rate, however many: the window and the lift are kept on every cycle.
//     A median over replicas would read the fifteen cold ones as the fleet.
//   - Cold replicas that serve as much as the warm ones move it past the
//     tolerance for one cycle: the move does not hold for
//     aggShapeMoveCycles, so the window and the lift are kept.
func TestAggregatedShape_AColdScaleUpKeepsTheWindowAndTheLift(t *testing.T) {
	for _, tc := range []struct {
		name   string
		load   float64
		cycles int
	}{
		{"cold replicas serving little", 0.05, 4},
		{"one cycle of cold replicas at full load", 4, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newAggShapeRig(t, nil)
			r.hit = 0.7
			r.lifted(t)
			before := r.state(t)
			r.cold, r.coldHit, r.coldLoad = 15, 0, tc.load
			for i := range tc.cycles {
				r.step(t)
				st := r.state(t)
				assert.Equal(t, before.windowG, st.windowG, "cycle %d: the window moved", i)
				assert.True(t, st.lifted, "cycle %d: the lift was dropped", i)
				assert.Len(t, st.sigmas, aggSigmaMaxReadings, "cycle %d: the prefill readings were dropped", i)
			}
			r.coldHit = 0.7
			res := r.step(t)
			assert.Equal(t, domain.ServiceRateSourceDerivedAggregated, signal(t, res, "agg").RateSource,
				"the caches warmed and the variant is not sized from its line")
			assert.Equal(t, before.windowG, r.state(t).windowG)
		})
	}
}

// M2: a shape move that holds ends the lift on the cycle it drops the
// window. The hit rate moves from 0 to 0.7 (g a third lower, and the
// request lengths, which the fleet's shape hold watches, unchanged): on the
// first cycle the move is pending and the lift is kept, on the second it
// holds, the window is dropped and the figure is reported only.
func TestAggregatedShape_AShapeMoveEndsTheLift(t *testing.T) {
	r := newAggShapeRig(t, nil)
	r.lifted(t)
	before := r.state(t)
	r.hit = 0.7
	first := r.step(t)
	assert.Equal(t, domain.ServiceRateSourceDerivedAggregated, signal(t, first, "agg").RateSource, "a pending move ended the lift")
	second := r.step(t)
	assert.Equal(t, domain.ServiceRateSourceReportOnly, signal(t, second, "agg").RateSource,
		"the lift outlived the shape move that dropped the window")
	st := r.state(t)
	assert.False(t, st.lifted)
	assert.Less(t, st.windowG, 0.8*before.windowG, "precondition: the window moved")
}

// M3: a lifted figure is priced through capAtMeasured. One measured reading
// of the variant's own, half the derived rate, is on record: too thin for
// the measurement to price the replica itself, so the derived figure does,
// and at the lower of the two.
func TestAggregatedShape_ALiftedFigureIsCappedAtItsOwnMeasurementThroughAnalyze(t *testing.T) {
	r := newAggShapeRig(t, nil)
	res := r.lifted(t)
	derived := signal(t, res, "agg").Rate
	// Under the variant's own key, whichever output bucket the analyzer keys
	// the shape in (its smoothed output length): one window per bucket, so
	// the variant's own is among them and the others are neighbours. Tagged
	// with the arguments the variant runs, as a reading the analyzer took
	// itself is: an untagged window reads as one from other arguments.
	seeded := map[string]bool{}
	for _, out := range []float64{50, 150, 300, 500, 800, 1500, 3000, 6000, 12000} {
		key := r.a.throughputKey("test-model", "test-ns", "agg", "H100", 1, "", r.in, out, 5)
		if seeded[key] {
			continue
		}
		seeded[key] = true
		r.a.recordSaturatedThroughput(key, derived/2)
		r.a.mu.Lock()
		r.a.throughputWindowArgs[key] = windowArgs(keyParams(256))
		r.a.mu.Unlock()
	}
	sig := signal(t, r.step(t), "agg")
	assert.Equal(t, domain.ServiceRateSourceDerivedAggregated, sig.RateSource)
	assert.InDelta(t, derived/2, sig.Rate, 0.01*derived, "the lifted figure priced above the variant's own measurement")
}

// M4: a change of engine arguments drops the variant's shape state with its
// window: the lift, and the prefill readings taken under the old arguments.
func TestAggregatedShape_AnArgsChangeDropsTheLiftAndTheShapeState(t *testing.T) {
	r := newAggShapeRig(t, nil)
	r.lifted(t)
	require.Len(t, r.state(t).sigmas, aggSigmaMaxReadings, "precondition: a full set of prefill readings")
	r.store.Update("test-ns", "test-model", "agg", capacity.Record{AcceleratorName: "H100", GpuCount: 1,
		EngineParams: keyParams(128)}, r.clock)
	res := r.step(t)
	assert.NotEqual(t, domain.ServiceRateSourceDerivedAggregated, signal(t, res, "agg").RateSource, "the lift outlived the arguments")
	st := r.state(t)
	assert.False(t, st.lifted)
	assert.LessOrEqual(t, len(st.sigmas), shapeReplicas, "prefill readings from the old arguments were kept")
}

// M5: a line's shape survives an export and a restore, through the
// document's JSON, c included; and a document from ev-shindin/llm-scaling-manager#432, with no line
// shapes, restores without error, its line with no shape.
func TestAggregatedShape_TheLineShapeSurvivesARestore(t *testing.T) {
	r := newAggShapeRig(t, nil)
	r.steps(t, 30)
	r.in, r.out = 4000, 250
	r.steps(t, 30)
	want := r.shapeOf(t)
	require.True(t, want.cOK, "precondition: c identified")

	roundTrip := func(s LearnedState) LearnedState {
		raw, err := json.Marshal(s)
		require.NoError(t, err)
		var out LearnedState
		require.NoError(t, json.Unmarshal(raw, &out))
		return out
	}
	doc := roundTrip(r.a.ExportLearnedState())
	require.NotEmpty(t, doc.ITLLineShapes, "the export carries no line shape")

	fresh := newAggShapeRig(t, nil)
	fresh.clock = r.clock
	rep := fresh.a.RestoreLearnedState(doc)
	require.Equal(t, 1, rep.ITLLines)
	got := fresh.shapeOf(t)
	assert.Equal(t, want.g, got.g)
	assert.Equal(t, want.sigmaOK, got.sigmaOK)
	assert.InDelta(t, want.sigma, got.sigma, 1e-12)
	assert.Len(t, got.points, len(want.points))
	require.True(t, got.cOK, "c was not refitted from the restored points")
	assert.InDelta(t, want.c, got.c, 1e-12)

	old := doc
	old.ITLLineShapes = nil
	raw, err := json.Marshal(old)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "itlLineShapes", "precondition: a ev-shindin/llm-scaling-manager#432 document")
	var oldDoc LearnedState
	require.NoError(t, json.Unmarshal(raw, &oldDoc))
	legacy := newAggShapeRig(t, nil)
	legacy.clock = r.clock
	require.Equal(t, 1, legacy.a.RestoreLearnedState(oldDoc).ITLLines)
	assert.Zero(t, legacy.shapeOf(t).g, "a ev-shindin/llm-scaling-manager#432 line restored with a shape")
}

// The forget-learned-state annotation drops the variant's shape state, its
// lift with it; and a restart restores no lift: a restored variant is
// reported only until it agrees again.
func TestAggregatedShape_AForgetAndARestartResetTheLift(t *testing.T) {
	r := newAggShapeRig(t, nil)
	r.lifted(t)
	r.a.ForgetVariant("test-ns", "test-model", "agg", "token-1")
	r.a.mu.Lock()
	left := len(r.a.itlAggregated)
	r.a.mu.Unlock()
	assert.Zero(t, left, "the forget left the shape state")

	r = newAggShapeRig(t, nil)
	r.lifted(t)
	doc := r.a.ExportLearnedState()
	fresh := newAggShapeRig(t, nil)
	fresh.clock = r.clock
	fresh.a.RestoreLearnedState(doc)
	fresh.arrival = 50
	assert.Equal(t, domain.ServiceRateSourceReportOnly, signal(t, fresh.step(t), "agg").RateSource,
		"a restart restored the lift")
}

// speculatingParams is keyParams with vLLM speculative decoding, two draft
// tokens a step.
func speculatingParams() *capacity.EngineParams {
	p := keyParams(256)
	p.SpeculativeConfig = `{"method": "eagle", "num_speculative_tokens": 2}`
	return p
}

// On a speculating engine a request takes OL/(1+s) decode steps: the prefill
// steps are read against those, and the lifted rate is the physics' at one
// accepted draft token a step. Read against OL, every reading would be
// refused and nothing would lift.
func TestAggregatedShape_ASpeculatingVariantIsReadInDecodeSteps(t *testing.T) {
	r := newAggShapeRigOn(t, nil, speculatingParams())
	r.world.spec = 1
	res := r.lifted(t)
	want := r.world.mu(r.in, r.out)
	assert.InDelta(t, want, signal(t, res, "agg").Rate, 0.02*want)
	st := r.state(t)
	assert.Zero(t, st.refused, "prefill readings were refused against the output's tokens")
	assert.InDelta(t, 1.0, st.specPerStep, 1e-12)
}

// A speculating engine whose replicas report no accepted tokens takes no
// prefill reading and stays reported only, logged once.
func TestAggregatedShape_ASpeculatingVariantWithNoAcceptedTokensStaysReportOnly(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	const key = "test-ns|test-model|agg|H100|1"
	core, logs := observer.New(zapcore.Level(-logging.DEFAULT))
	logger := zapr.NewLogger(zap.New(core))
	for range 2 {
		a.noteOccupancy(occTestInput(key, sigmaRows(5, 0.1), speculatingParams()), logger)
	}
	a.mu.Lock()
	st := *a.itlAggregated[key]
	a.mu.Unlock()
	assert.Equal(t, aggBlockedSpecUnknown, st.blocked)
	assert.Empty(t, st.sigmas)
	assert.Equal(t, 1, logs.FilterMessage(aggBlockedSpecUnknown).Len(), "not logged exactly once")

	ac := &aggCycle{figure: true, sigmaOK: true, blocked: st.blocked}
	assert.Equal(t, aggBlockedSpecUnknown, aggLiftReason(ac, 0, false))

	r := newAggShapeRigOn(t, nil, speculatingParams())
	r.world.spec, r.specKnown = 1, false
	r.steps(t, 13)
	r.arrival = 50
	res := r.steps(t, 6)
	assert.NotEqual(t, domain.ServiceRateSourceDerivedAggregated, signal(t, res, "agg").RateSource)
	assert.Equal(t, aggBlockedSpecUnknown, r.state(t).blocked)
}

// sglangParams is keyParams on SGLang.
func sglangParams() *capacity.EngineParams {
	p := keyParams(256)
	p.Engine = inferenceengine.EngineSGLang
	return p
}

// SGLang publishes a completion rate per pod, but its running count may
// leave out requests in chunked prefill, so the prefill steps would read as
// about none and over-state mu. Not verified: an SGLang variant takes no
// prefill reading and stays reported only, logged once.
func TestAggregatedShape_AnSGLangVariantStaysReportOnly(t *testing.T) {
	r := newAggShapeRigOn(t, nil, sglangParams())
	r.steps(t, 13)
	r.arrival = 50
	res := r.steps(t, 6)
	assert.NotEqual(t, domain.ServiceRateSourceDerivedAggregated, signal(t, res, "agg").RateSource, "an SGLang variant sized")
	st := r.state(t)
	assert.Equal(t, aggBlockedSGLang, st.blocked)
	assert.Empty(t, st.sigmas)

	a := NewSaturationAnalyzer(capacity.NewStore())
	core, logs := observer.New(zapcore.Level(-logging.DEFAULT))
	logger := zapr.NewLogger(zap.New(core))
	for range 3 {
		a.noteOccupancy(occTestInput("k", sigmaRows(3, 0.1), sglangParams()), logger)
	}
	assert.Equal(t, 1, logs.FilterMessage(aggBlockedSGLang).Len(), "not logged exactly once")
}
