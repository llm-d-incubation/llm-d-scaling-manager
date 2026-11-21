package main

import (
	"context"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// The fleet needed at each f, in words: the same at every f in one clause,
// else each f's fleet, and an f no fleet holds at named.
func TestFSensitivityWords(t *testing.T) {
	n := whatifapi.FleetCounts{Prefill: 3, Decode: 3}
	for _, c := range []struct {
		name string
		rows []whatifapi.ReportNeededAtF
		want string
	}{
		{"none", nil, ""},
		{"the same", []whatifapi.ReportNeededAtF{{F: 1, Prefill: 3, Decode: 3}, {F: 1.5, Prefill: 3, Decode: 3}},
			"; the same at f 1 to 1.5"},
		{"moves", []whatifapi.ReportNeededAtF{{F: 1, Prefill: 3, Decode: 3}, {F: 1.5, Prefill: 4, Decode: 3}},
			"; at f 1 needs 3 + 3, at f 1.5 needs 4 + 3"},
		{"unmet", []whatifapi.ReportNeededAtF{{F: 1, Prefill: 2, Decode: 3}, {F: 1.5, Unmet: "fleet"}},
			"; at f 1 needs 2 + 3, at f 1.5 no fleet holds"},
	} {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, fSensitivityWords(n, c.rows))
		})
	}
}

// Issue llm-d-incubation/llm-d-scaling-manager#300: the staircase learned f
// at two prefill pods only. At its p95 load with 16k prompts the fleet needed
// is read at the curve's f, about 1, and the verdict gives the fleet needed
// at f 1 and at f 1.5 beside it: at 1.5 the pods past two are worth less, and
// more of them are needed.
func TestAtReplicas_TheFleetNeededAtEachF(t *testing.T) {
	st := stairReplay.state(t)
	q := stairQuery(2, 2, 41.7)
	sh, err := whatifapi.ParseReportShape("16k/200/0.3")
	require.NoError(t, err)
	q.Shapes = []whatifapi.ReportShapeAsk{sh}
	ctx, cancel := context.WithTimeout(t.Context(), reportDeadline)
	defer cancel()
	rep, err := buildReport(ctx, st, &servedState{st: st, decodeVariant: "variant-01", imbalanceAcrossCounts: true}, q)
	require.NoError(t, err)
	v := rep.Shapes[0].Verdict
	require.Equal(t, whatifapi.VerdictUnder, v.Verdict, v.Summary)
	require.NotNil(t, v.Needed, v.Summary)
	n := *v.Needed
	require.GreaterOrEqual(t, n.Prefill, 2, "precondition: the answer rests on f, %s", v.Summary)
	require.Len(t, n.FSensitivity, 2, v.Summary)
	lo, hi := n.FSensitivity[0], n.FSensitivity[1]
	assert.Equal(t, 1.0, lo.F)
	assert.Equal(t, whatif.ImbalanceFloor, hi.F)
	assert.Empty(t, lo.Unmet)
	assert.LessOrEqual(t, lo.Prefill, n.Prefill, "f 1 needs no more than the curve's f, at least 1")
	assert.Greater(t, hi.Prefill, lo.Prefill, "at f 1.5 more prefill pods are needed")
	assert.Contains(t, v.Summary, fSensitivityWords(whatifapi.FleetCounts{Prefill: n.Prefill, Decode: n.Decode}, n.FSensitivity))
}

// A fixed f prices only the counts P1 has not learned: at the staircase's
// two pods both rows keep the f learned there, as the floor and the curve
// do; at three, none learned, they take the f fixed.
func TestFixedImbalance_ALearnedCountKeepsItsF(t *testing.T) {
	st := stairReplay.state(t)
	s := &servedState{st: st, decodeVariant: "variant-01", imbalanceAcrossCounts: true}
	learned := stairF2(t)
	for _, c := range []struct {
		pods    int
		fixed   float64
		want    float64
		assumed bool
	}{{2, 1, learned, false}, {2, whatif.ImbalanceFloor, learned, false}, {3, 1, 1, true}, {3, whatif.ImbalanceFloor, whatif.ImbalanceFloor, true}} {
		k := whatifapi.Knobs{Prefill: c.pods, Decode: 2, TTFTPercentile: 0.9, Headroom: 0.85, Assume: stairQuery(2, 2, 0).Assume}
		sf, err := s.fleetAtImbalance(k, c.fixed)
		require.NoError(t, err)
		require.Nil(t, sf.pf.refused)
		assert.InDelta(t, c.want, sf.pf.fleet.Prefill.Imbalance, 1e-12, "%d pods at f %g", c.pods, c.fixed)
		assert.Equal(t, c.assumed, sf.pf.fleet.Prefill.ImbalanceAssumed, "%d pods at f %g", c.pods, c.fixed)
	}
}

// The planner reads the curve only where P1 learned some count: with none
// learned (none at all, or only an f that is not one), an unlearned count
// keeps the floor, max(1.5, the raw share), and not whatif.FreshImbalance.
func TestTTFT_NoCountLearnedKeepsTheFloor(t *testing.T) {
	for _, c := range []struct {
		name   string
		byPods map[int]float64
		raw    float64
		want   float64
		source string
	}{
		{"none", nil, 0, whatif.ImbalanceFloor, shapeAssumed},
		{"none, a raw share", nil, 1.2, whatif.ImbalanceFloor, shapeAssumed},
		{"none, a hot raw share", nil, 1.8, 1.8, shapeAssumed},
		{"NaN", map[int]float64{2: math.NaN()}, 1.2, whatif.ImbalanceFloor, shapeAssumed},
		{"Inf", map[int]float64{2: math.Inf(1)}, 1.2, whatif.ImbalanceFloor, shapeAssumed},
		{"one learned", map[int]float64{2: 1}, 0, 1, "fitted from counts {2}"},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := learnedState()
			st.Prefill[0].Pods, st.Prefill[0].RawShare = 2, c.raw
			st.Prefill[0].Estimate.FByPods = c.byPods
			pf := fleetOf(t, st, answerFlags{imbalanceAcrossCounts: true, prefill: 3})
			require.Nil(t, pf.refused)
			assert.InDelta(t, c.want, pf.fleet.Prefill.Imbalance, 1e-12)
			assert.True(t, pf.fleet.Prefill.ImbalanceAssumed)
			assert.Equal(t, c.source, pf.in.ImbalanceSource)
		})
	}
}

// --imbalance-across-counts at the planner's default states nothing, so it
// puts no figure outside the envelope; set away from it, it does.
func TestEnvelope_ImbalanceAcrossCountsAtTheDefault(t *testing.T) {
	for _, c := range []struct {
		on   bool
		want []string
	}{{whatif.DefaultPlannerImbalanceAcrossCounts, nil}, {!whatif.DefaultPlannerImbalanceAcrossCounts, []string{"--imbalance-across-counts"}}} {
		f := answerFlags{imbalanceAcrossCounts: c.on, set: map[string]bool{"imbalance-across-counts": true}}
		assert.Equal(t, c.want, f.statedOutside(), "on %v", c.on)
	}
}
