package slo

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The planner's model rules that live in shared packages, so the controller
// can call them (issue llm-d-incubation/llm-d-scaling-manager#169). Each test
// calls the shared rule from this package, and holds the analyzer off it as
// the constructor builds it: no rule is used here yet.

// The floor below the band kept by sequence count (itl.BelowBandByCount,
// #457/#495) builds from this package, and the analyzer's decode leaves
// whatif.Decode.BelowBand unset, so no fill below the band is priced by it.
func TestParity_TheFloorByCountIsSharedAndUnused(t *testing.T) {
	obs := make([]itl.Observation, 0, 44)
	seqs := make([]float64, 0, 44)
	for range 22 {
		obs = append(obs, itl.Observation{K: 0.05, ITLSec: 0.010}, itl.Observation{K: 0.05, ITLSec: 0.020})
		seqs = append(seqs, 4, 40)
	}
	least, byCount := itl.BelowBandByCount(obs, seqs)
	assert.Equal(t, 4.0, least)
	assert.True(t, itl.ValidModel(byCount.A, byCount.B), "a line through the two floors: %+v", byCount)

	a := New(nil, nil)
	fleet := fleetReading{ready: map[string]int{"dec": 2}, rows: decodeRows("dec", 2)}
	fleet.shape.KVreq, fleet.shape.AvgOutputTokens = 1500, 1000
	d := pricingDetail{source: constants.SLOSourceOwn, band: whatif.DefaultBand()}
	d.line.Model, d.line.Tier = itl.Model{A: 0.02, B: 0.004}, itl.TierOLS
	got := a.ttftDecode(domain.AnalyzerInput{ModelID: "m", Namespace: "ns"}, fleet, []ttftDecodeLine{{state: decState, detail: d}})
	if assert.NotNil(t, got) {
		assert.Equal(t, whatif.BelowBand{}, got.BelowBand, "the controller builds no floor by count")
	}
}

// The served-work knee floor (whatif.ServedKneeFloor, #494) prices from this
// package; the analyzer keeps no served work (saturation's
// SwitchPrefillVPServedUnqueued is off from its constructor), and with none
// the knee is left as priced.
func TestParity_TheServedKneeFloorIsSharedAndUnused(t *testing.T) {
	p := whatif.Prefill{Replicas: 2, PromptTokens: 1000}
	lb := whatif.Answer{Value: 10, Flags: []whatif.Flag{whatif.VPLowerBound}}
	got, ok := whatif.ServedKneeFloor(lb, p, map[int]float64{2: 30000}, nil).(whatif.Answer)
	require.True(t, ok)
	assert.InDelta(t, 30.0, got.Value, 1e-9)
	assert.True(t, got.HasFlag(whatif.KneeAtServedWork))
	assert.Equal(t, whatif.Result(lb), whatif.ServedKneeFloor(lb, p, nil, nil), "no work kept, as in the controller")
}

// The prefix hit at another prefill count (whatif.ApplyHitAtCount,
// FitTotalHit, PromptWindow.HitFitCount, #425/#469/#472) and f against the
// load (whatif.ApplyFLoadCurve, #493) apply from this package. The analyzer
// calls neither: saturation's SwitchPrefillHitAtCount and
// SwitchPrefillFLoadCurve are off from its constructor, and with no curve
// fitted the curve sets nothing.
func TestParity_TheHitAtCountAndTheFLoadCurveApplyHereAndAreUnused(t *testing.T) {
	var w *whatif.PromptWindow
	fitCount := w.HitFitCount(2)
	assert.Equal(t, 2, fitCount)
	p := whatif.Prefill{Replicas: 1, PromptTokens: 6000, WholePromptTokens: 8000}
	h, ok := whatif.FitTotalHit(p, false, w)
	require.True(t, ok)
	r, ok := whatif.ApplyHitAtCount(&p, h, fitCount, nil)
	require.True(t, ok)
	assert.True(t, r.Moved())
	assert.InDelta(t, r.Computed, p.PromptTokens, 1e-9)

	q := whatif.Prefill{Replicas: 2, Imbalance: 1.3}
	_, ok = whatif.ApplyFLoadCurve(&q, prefill.Estimate{})
	assert.False(t, ok, "no curve fitted, as in the controller")
	assert.Zero(t, q.ImbalanceCurveFloor)
}

// The planner's two TTFT defaults that are this package's switches
// (learnreplay.PlannerSwitches: SwitchTTFTRouterWaitInPrefill at
// whatif.DefaultRouterWaitInPrefill, SwitchTTFTImbalanceAcrossCounts at
// whatif.DefaultPlannerImbalanceAcrossCounts) are on for the planner and off
// from the constructor, and a replay sets each by name.
func TestParity_ThePlannerTTFTDefaultsAreOffFromTheConstructor(t *testing.T) {
	require.True(t, whatif.DefaultRouterWaitInPrefill, "precondition: on for the planner")
	require.True(t, whatif.DefaultPlannerImbalanceAcrossCounts, "precondition: on for the planner")
	a := New(nil, nil)
	fields := a.switchFields()
	for _, name := range []string{SwitchTTFTRouterWaitInPrefill, SwitchTTFTImbalanceAcrossCounts} {
		require.Contains(t, fields, name)
		assert.False(t, *fields[name], "%s: off from the constructor", name)
	}
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchTTFTRouterWaitInPrefill: true, SwitchTTFTImbalanceAcrossCounts: true}))
	assert.True(t, a.ttftRouterWaitInPrefill)
	assert.True(t, a.ttftImbalanceAcrossCounts)
}

// The TPOT learner's gate (tpotFoldsPass) reads the shared scorer's fold
// counts, and pins the rule it was validated on by name (ModelTPOTFoldRule,
// audit section 1.3 (1) item 2). The rule's figures on one step are pinned
// here, from this package: the joint bracket's p90 [20 ms, 25 ms] and p95
// [25 ms, 40 ms], from the router's 20 ms and 40 ms edges and the engines' 25
// ms. A planner change to the bracket's constants or its reading fails this
// test; a planner change that renames the rule fails the gate below.
func TestParity_TheTPOTGateHoldsItsBracketRule(t *testing.T) {
	require.Equal(t, whatif.TPOTBracketJointV1, ModelTPOTFoldRule)
	assert.Equal(t, 2.0, whatif.JointBracketSE, "rule %s", ModelTPOTFoldRule)
	assert.Equal(t, 0.005, whatif.JointBracketAllowance, "rule %s", ModelTPOTFoldRule)
	assert.Equal(t, 50.0, whatif.MinEngineAboveFirstEdge, "rule %s", ModelTPOTFoldRule)

	const window, n = 300.0, 20000.0
	cum := func(edges, f []float64, total float64) []domain.LatencyBucket {
		bs := make([]domain.LatencyBucket, 0, len(edges)+1)
		for i, e := range edges {
			bs = append(bs, domain.LatencyBucket{UpperBound: e, Rate: f[i] * total / window})
		}
		return append(bs, domain.LatencyBucket{UpperBound: math.Inf(1), Rate: total / window})
	}
	router := cum([]float64{0.005, 0.01, 0.02, 0.04, 0.06, 0.08}, []float64{0.02, 0.30, 0.85, 0.97, 0.99, 0.995}, n)
	anchored := func(f float64) float64 { return 0.5 + 0.5*(f-0.3)/0.7 }
	engine := cum([]float64{0.01, 0.025, 0.05, 0.075, 0.1},
		[]float64{0.5, anchored(0.92), anchored(0.98), anchored(0.993), anchored(0.998)}, 2*n)
	edges, rates := make([]float64, len(router)), make([]float64, len(router))
	for i, b := range router {
		edges[i], rates[i] = b.UpperBound, b.Rate
	}
	st, ok := whatif.TPOTDistStepOf(edges, rates, window, 0.014, []whatif.DecodeCycle{{Output: map[float64]float64{2: 0, 100: 1}}})
	require.True(t, ok)
	st = whatif.WithEngineTPOT(st, engine)

	for _, c := range []struct{ q, lo, hi float64 }{{0.9, 0.020, 0.025}, {0.95, 0.025, 0.040}} {
		b, ok := whatif.JointBracket(st, c.q)
		require.True(t, ok)
		assert.InDelta(t, c.lo, b.Lo, 1e-12, "p%g", 100*c.q)
		assert.InDelta(t, c.hi, b.Hi, 1e-12, "p%g", 100*c.q)
		assert.True(t, b.Engine, "the engines' histogram joins")
	}

	// No spread: every percentile is the mean, 22 ms, inside the p90's
	// bracket and under the p95's.
	var sc whatif.TPOTDistScorer
	_, p90, p95 := sc.Add(whatif.TPOTDist{}, st, 0.022, nil)
	assert.True(t, p90)
	assert.False(t, p95)
	fold := sc.Score()
	assert.Equal(t, ModelTPOTFoldRule, fold.Rule)
	assert.Equal(t, 1, fold.P90Inside)
	assert.True(t, tpotFoldsPass(whatif.TPOTDistReport{Fold: []whatif.TPOTDistScore{fold}}))

	// A fold scored by any other rule fails the gate, however many of its
	// steps read inside: the controller takes a new rule only by name.
	other := fold
	other.Rule = "joint-router-engine-v2"
	assert.False(t, tpotFoldsPass(whatif.TPOTDistReport{Fold: []whatif.TPOTDistScore{fold, other}}))
	assert.True(t, tpotFoldsPass(whatif.TPOTDistReport{}), "no fold, no gate")
}
