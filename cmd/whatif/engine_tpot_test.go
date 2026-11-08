package main

import (
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// tpotWindow is one window of 100 observations at a mean of 10 ms, half of them
// at or below 10 ms, and the p90 in the bucket that ends at edge.
func tpotWindow(edge float64) whatif.TPOTWindow {
	return whatif.TPOTWindow{Sum: 1, Count: 100, Buckets: []whatif.HistogramBucket{
		{UpperEdge: 0.010, Count: 50}, {UpperEdge: edge, Count: 95}, {UpperEdge: 2 * edge, Count: 100}}}
}

// Fine at a 10 ms mean: the q bucket is 2 ms wide. Coarse: it is 10 ms wide.
// Too wide: 20 ms, twice the mean, past whatif.MaxEngineBucketWidth.
var (
	fineWindow    = tpotWindow(0.012)
	coarseWindow  = tpotWindow(0.020)
	tooWideWindow = tpotWindow(0.030)
)

// unboundedWindow holds its p90 past the last finite edge.
var unboundedWindow = whatif.TPOTWindow{Sum: 1, Count: 100, Buckets: []whatif.HistogramBucket{
	{UpperEdge: 0.010, Count: 50}, {UpperEdge: math.Inf(1), Count: 100}}}

func windowsState(router, engine []whatif.TPOTWindow) *learnreplay.State {
	return &learnreplay.State{TPOTWindows: router, EngineTPOTWindows: engine}
}

func emptyWindow() whatif.TPOTWindow {
	return whatif.TPOTWindow{Buckets: []whatif.HistogramBucket{{UpperEdge: 0.01}}}
}

func TestEngineTPOT_TheOrder(t *testing.T) {
	cases := []struct {
		name           string
		router, engine []whatif.TPOTWindow
		wantEngine     bool
		wantReason     whatif.Reason
		wantCoarse     bool
		wantFlags      []whatif.Flag
	}{
		{name: "a fine router: the router", router: []whatif.TPOTWindow{fineWindow}, engine: []whatif.TPOTWindow{fineWindow}},
		{name: "a coarse router, a fine engine: the engine", router: []whatif.TPOTWindow{coarseWindow},
			engine: []whatif.TPOTWindow{fineWindow}, wantEngine: true},
		{name: "both coarse: the router, which is what the client sees", router: []whatif.TPOTWindow{coarseWindow},
			engine: []whatif.TPOTWindow{coarseWindow}, wantCoarse: true},
		{name: "a coarse router and no engine: the router", router: []whatif.TPOTWindow{coarseWindow}, wantCoarse: true},
		{name: "no router: the engine", engine: []whatif.TPOTWindow{fineWindow}, wantEngine: true},
		{name: "no router, a coarse engine: the engine, flagged", engine: []whatif.TPOTWindow{coarseWindow}, wantEngine: true, wantCoarse: true},
		{name: "an empty router: the engine", router: []whatif.TPOTWindow{emptyWindow()}, engine: []whatif.TPOTWindow{fineWindow}, wantEngine: true},
		{name: "no router, a bucket past the line: refused, flagged", engine: []whatif.TPOTWindow{tooWideWindow},
			wantReason: whatif.TPOTBucketTooWide, wantFlags: []whatif.Flag{whatif.CoarseTPOTBucket, whatif.EngineTPOTHistogram}},
		{name: "an empty router, a bucket past the line: refused", router: []whatif.TPOTWindow{emptyWindow()},
			engine: []whatif.TPOTWindow{tooWideWindow}, wantReason: whatif.TPOTBucketTooWide,
			wantFlags: []whatif.Flag{whatif.CoarseTPOTBucket, whatif.EngineTPOTHistogram}},
		{name: "a coarse router, a bucket past the line: the router", router: []whatif.TPOTWindow{coarseWindow},
			engine: []whatif.TPOTWindow{tooWideWindow}, wantCoarse: true},
		// The engine's data says more than that no histogram exists.
		{name: "no router, an unbounded engine: the engine's refusal", engine: []whatif.TPOTWindow{unboundedWindow},
			wantReason: whatif.TPOTQuantileUnbounded, wantFlags: []whatif.Flag{whatif.EngineTPOTHistogram}},
		{name: "a coarse router, an unbounded engine: the router", router: []whatif.TPOTWindow{coarseWindow},
			engine: []whatif.TPOTWindow{unboundedWindow}, wantCoarse: true},
		{name: "neither: refused", wantReason: whatif.NoTPOTHistogram},
		{name: "an empty router and an empty engine: refused", router: []whatif.TPOTWindow{emptyWindow()},
			engine: []whatif.TPOTWindow{emptyWindow()}, wantReason: whatif.NoTPOTHistogram},
		// The router's data is wrong: that is not the same as absent.
		{name: "a malformed router stands", router: []whatif.TPOTWindow{{Count: 5}}, engine: []whatif.TPOTWindow{fineWindow},
			wantReason: whatif.MalformedHistogram},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := answerFlags{set: map[string]bool{}}
			r, flags, refused := f.tpotRatioFor(windowsState(c.router, c.engine), 0.9)
			if c.wantReason != "" {
				require.NotNil(t, refused)
				assert.Equal(t, c.wantReason, refused.Reason)
				assert.ElementsMatch(t, c.wantFlags, refused.Flags)
				assert.False(t, f.engineTPOT)
				if len(c.wantFlags) > 0 {
					assert.Contains(t, refused.Detail, "--assume-tpot-ratio")
				}
				return
			}
			require.Nil(t, refused)
			assert.Positive(t, r.Value)
			assert.Equal(t, c.wantEngine, f.engineTPOT)
			assert.Equal(t, c.wantEngine, slices.Contains(flags, whatif.EngineTPOTHistogram))
			assert.Equal(t, c.wantCoarse, slices.Contains(flags, whatif.CoarseTPOTBucket))
		})
	}
}

// --assume-tpot-ratio overrides both, as every --assume-* flag does.
func TestEngineTPOT_AnAssumedRatioWins(t *testing.T) {
	f := answerFlags{set: map[string]bool{"assume-tpot-ratio": true}, tpotRatio: 1.3}
	r, _, refused := f.tpotRatioFor(windowsState(nil, []whatif.TPOTWindow{fineWindow}), 0.9)
	require.Nil(t, refused)
	assert.True(t, r.Assumed)
	assert.False(t, f.engineTPOT)
}

// Through the command, on a recording with no router latency whose engine
// buckets open at 10 ms around a 3 ms mean: every percentile would read the
// same edge, so each is refused, and the refusal says why and what to pass.
func TestEngineTPOT_ABucketPastTheLineIsRefused(t *testing.T) {
	for _, pct := range []string{"90", "99", "99.9"} {
		q := []string{"replicas-to-slo", "--recording", goldenDir + "throughput-sweep-restart/recording.jsonl.gz", "--tpot-percentile", pct}
		text, code := runCmd(t, q...)
		assert.Equal(t, 3, code, text)
		assert.Contains(t, text, "refused: TPOTBucketTooWide", pct)
		assert.Contains(t, text, "--assume-tpot-ratio")
		assert.Contains(t, text, string(whatif.EngineTPOTHistogram))
		assert.Contains(t, text, string(whatif.CoarseTPOTBucket))

		out, _ := runCmd(t, append(q, "-o", "json")...)
		w := decodeOutput(t, out)
		require.NotEmpty(t, w.Roles)
		r := w.Roles[0].Results.PercentileTPOT.Refused
		require.NotNil(t, r, out)
		assert.ElementsMatch(t, []string{string(whatif.CoarseTPOTBucket), string(whatif.EngineTPOTHistogram)}, r.Flags)
	}
}

// question 1 refuses the same way, and keeps the flags.
func TestEngineTPOT_TheRateQuestionKeepsTheFlags(t *testing.T) {
	out, code := runCmd(t, "slo-to-rate", "--recording", goldenDir+"throughput-sweep-restart/recording.jsonl.gz", "--tpot", "20ms@p90", "-o", "json")
	require.Equal(t, 3, code, out)
	r := decodeOutput(t, out).Roles[0].Results.Rate.Refused
	require.NotNil(t, r, out)
	assert.Equal(t, string(whatif.TPOTBucketTooWide), r.Reason)
	assert.Contains(t, r.Flags, string(whatif.EngineTPOTHistogram))
}

// The success path through the command's own code, on a state: no recording
// holds a fine engine histogram and no router. The percentile answers, says
// from what, in the row, the flags and the JSON's inputs.
func TestEngineTPOT_ThePathThatAnswers(t *testing.T) {
	st := checkedState(1)
	st.EngineTPOTWindows = []whatif.TPOTWindow{fineWindow, fineWindow, fineWindow}
	require.Empty(t, st.TPOTWindows)

	f := answerFlags{set: map[string]bool{}}
	variants, err := f.decodeVariants(st)
	require.NoError(t, err)
	require.Len(t, variants, 1)
	ratio, flags, refused := f.tpotRatioFor(st, 0.9)
	require.Nil(t, refused)
	require.InDelta(t, 1.2, ratio.Value, 1e-9)
	assert.True(t, f.engineTPOT)
	assert.Equal(t, 3, f.tpotWindowCount(st))

	in := f.tpotInputs(st, 0, 0.9, ratio)
	assert.Equal(t, "engine histogram", in.RatioSource)
	assert.Equal(t, 3, in.Windows)
	assert.Regexp(t, `^R_q 1\.2\d* from 3 engine TPOT windows, not the router's$`, f.ratioNote(st, ratio))

	pv := variants[0]
	query := whatif.ReplicasQuery{Headroom: 0.85, PercentileTarget: &whatif.TPOTTarget{Percentile: 0.9, Ratio: ratio}}
	slo := whatif.SLOAtReplicas(pv.decode, query)
	slo.PercentileTPOT = pv.qualify(onRatio(slo.PercentileTPOT, ratio, flags))
	a, ok := slo.PercentileTPOT.(whatif.Answer)
	require.True(t, ok, "%#v", slo.PercentileTPOT)
	assert.Contains(t, a.Flags, whatif.EngineTPOTHistogram)
	assert.NotContains(t, a.Flags, whatif.CoarseTPOTBucket)

	var b strings.Builder
	answers := []replicasAnswer{{pv: pv, slo: slo, ratio: ratio, query: query, headroom: 0.85}}
	require.NoError(t, writeReplicasText(&b, &f, st, variants, answers, nil, nil, nil, "90"))
	assert.Regexp(t, `TPOT p90 .*per request; R_q 1\.2\d* from 3 engine TPOT windows, not the router's`, b.String())
	assert.Contains(t, b.String(), string(whatif.EngineTPOTHistogram))
	assert.NotContains(t, b.String(), "refused")
}

// A refusal priced on R_q says where R_q came from: an engine's ratio, or an
// assumed one, as an answer does.
func TestOnRatio_ARefusalKeepsWhatItRestsOn(t *testing.T) {
	floor := whatif.Unanswerable{Reason: whatif.TargetBelowFloor}
	engine := []whatif.Flag{whatif.CoarseTPOTBucket, whatif.EngineTPOTHistogram}

	got, ok := onRatio(floor, whatif.TPOTRatio{Value: 2}, engine).(whatif.Unanswerable)
	require.True(t, ok)
	assert.Equal(t, engine, got.Flags)
	assert.Empty(t, floor.Flags, "the refusal it was given is not changed")

	got, ok = onRatio(floor, whatif.TPOTRatio{Value: 1.3, Assumed: true}, nil).(whatif.Unanswerable)
	require.True(t, ok)
	assert.Equal(t, []whatif.Flag{whatif.AssumedTPOTRatio}, got.Flags)

	a, ok := onRatio(whatif.Answer{}, whatif.TPOTRatio{Value: 2}, engine).(whatif.Answer)
	require.True(t, ok)
	assert.Equal(t, engine, a.Flags)

	wrong, ok := onRatio(whatif.Unanswerable{Reason: whatif.InvalidInput}, whatif.TPOTRatio{Value: 1.3, Assumed: true}, engine).(whatif.Unanswerable)
	require.True(t, ok)
	assert.Empty(t, wrong.Flags, "a question asked wrongly rests on nothing")
}

// Through the command: a floor refused at an assumed ratio says so.
func TestOnRatio_AnAssumedRatioIsNotSilent(t *testing.T) {
	out, code := runCmd(t, slices.Concat([]string{"slo-to-rate", "--recording", olsSweep, "--model", "model-02", "--tpot", "2ms@p90",
		"--assume-tpot-ratio", "1.3", "-o", "json"}, workedShape)...)
	require.Equal(t, 3, code, out)
	r := decodeOutput(t, out).Roles[0].Results.Rate.Refused
	require.NotNil(t, r, out)
	assert.Equal(t, string(whatif.TargetBelowFloor), r.Reason)
	assert.Contains(t, r.Flags, string(whatif.AssumedTPOTRatio))
}
