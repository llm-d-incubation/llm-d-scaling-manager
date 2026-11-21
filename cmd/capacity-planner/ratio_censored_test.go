package main

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// R_q is read by interval-censored maximum likelihood where the replay's
// switch says so (saturation.SwitchTPOTRatioCensored), at the q bucket's
// edge otherwise (#279): on the router's 10 / 25 ms edges around a 15 ms
// mean, about 1.27 against the edge read's higher figure.
func TestTPOTRatio_TheCensoredReadOnTheSwitch(t *testing.T) {
	// 1000 requests, lognormal at a mean of 15 ms and a spread of 0.2: 2.7%
	// under 10 ms, 99.6% under 25 ms; its p90 over its mean is 1.267.
	w := whatif.TPOTWindow{Sum: 15, Count: 1000, SumMeasured: true, Buckets: []whatif.HistogramBucket{
		{UpperEdge: 0.010, Count: 27}, {UpperEdge: 0.025, Count: 996}, {UpperEdge: 0.050, Count: 1000}, {UpperEdge: math.Inf(1), Count: 1000}}}
	st := &learnreplay.State{TPOTWindows: []whatif.TPOTWindow{w, w, w}}
	f := answerFlags{set: map[string]bool{}}
	edge, _, u := f.tpotRatioFor(st, 0.9)
	require.Nil(t, u)
	st.TPOTRatioCensored = true
	censored, _, u := f.tpotRatioFor(st, 0.9)
	require.Nil(t, u)
	assert.Less(t, censored.Value, edge.Value, "the censored read is under the edge read")
	assert.InDelta(t, 1.267, censored.Value, 0.05)
}

// The censored read is not replaced by a coarse engine's edge reading where
// the router's buckets are coarse: the fit reads inside the coarse bucket,
// which is what the switch is for (#279 round 2). Without the switch the
// router's edge reading stands there.
func TestTPOTRatio_TheCensoredReadIsNotSwappedForACoarseEngine(t *testing.T) {
	router := []whatif.TPOTWindow{censoredWindow, censoredWindow, censoredWindow}
	st := &learnreplay.State{TPOTWindows: router, EngineTPOTWindows: []whatif.TPOTWindow{coarseWindow, coarseWindow, coarseWindow}}
	f := answerFlags{set: map[string]bool{}}
	edge, flags, u := f.tpotRatioFor(st, 0.9)
	require.Nil(t, u)
	require.NotContains(t, flags, whatif.EngineTPOTHistogram, "off: both coarse, the router's edge")
	st.TPOTRatioCensored = true
	f = answerFlags{set: map[string]bool{}}
	got, flags, u := f.tpotRatioFor(st, 0.9)
	require.Nil(t, u)
	want, _, _ := whatif.TPOTRatioCensored(router, 0.9)
	assert.NotContains(t, flags, whatif.EngineTPOTHistogram)
	assert.False(t, f.engineTPOT)
	assert.InDelta(t, want.Value, got.Value, 1e-12)
	assert.Less(t, got.Value, edge.Value)
}

// A fine engine reading stands over the fit, on the switch as off: it is a
// measurement where the fit inside a coarse router bucket is a model of one.
// So on a host-bound variant the switch never refuses (hostBoundRefusal)
// what the engine's reading priced off (#279 round 2).
func TestTPOTRatio_AFineEngineStandsOverTheFit(t *testing.T) {
	router := []whatif.TPOTWindow{censoredWindow, censoredWindow, censoredWindow}
	engine := []whatif.TPOTWindow{fineWindow, fineWindow, fineWindow}
	host := pricedVariant{state: hostVariant("d", 0.0017, 0.00175)}
	for _, on := range []bool{false, true} {
		st := &learnreplay.State{TPOTWindows: router, EngineTPOTWindows: engine, TPOTRatioCensored: on}
		f := answerFlags{set: map[string]bool{}, engineTPOT: !on}
		r := f.ratioFor(st, 0.9)
		require.Nil(t, r.refused, "censored %v", on)
		want, _, _ := whatif.EngineTPOTRatio(engine, 0.9)
		assert.InDelta(t, want.Value, r.ratio.Value, 1e-12, "censored %v", on)
		assert.Contains(t, r.flags, whatif.EngineTPOTHistogram, "censored %v", on)
		assert.True(t, f.engineTPOT, "censored %v", on)
		assert.Equal(t, hostFigure, onHostBound(hostFigure, host, r), "censored %v: the host-bound variant prices", on)
	}
	// The flag is the last read's: a router reading after an engine one
	// does not carry it over.
	f := answerFlags{set: map[string]bool{}, engineTPOT: true}
	_, flags, u := f.tpotRatioFor(&learnreplay.State{TPOTWindows: []whatif.TPOTWindow{fineWindow}}, 0.9)
	require.Nil(t, u)
	assert.NotContains(t, flags, whatif.EngineTPOTHistogram)
	assert.False(t, f.engineTPOT)
}

// censoredWindow is 1000 requests, lognormal at a mean of 15 ms and a
// spread of 0.2, on the router's 10 / 25 / 50 ms edges: 2.7% under 10 ms,
// 99.6% under 25 ms; its p90 over its mean is 1.267.
var censoredWindow = whatif.TPOTWindow{Sum: 15, Count: 1000, SumMeasured: true, Buckets: []whatif.HistogramBucket{
	{UpperEdge: 0.010, Count: 27}, {UpperEdge: 0.025, Count: 996}, {UpperEdge: 0.050, Count: 1000}, {UpperEdge: math.Inf(1), Count: 1000}}}
