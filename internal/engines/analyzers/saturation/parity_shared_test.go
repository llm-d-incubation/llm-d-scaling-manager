package saturation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/fleet"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// The planner's prefill rules that live in shared packages, so the
// controller can call them (issue llm-d-incubation/llm-d-scaling-manager#169).
// Each test calls the shared rule from this package, and holds the switch
// that gates it off in the analyzer the constructor builds: no rule is used
// here yet.

// The prefill pods' served work by pod count (prefill.ServedWork, #494)
// keeps from this package, and the switch the planner keeps it behind,
// SwitchPrefillVPServedUnqueued, is off from the constructor.
func TestParity_TheServedWorkIsSharedAndOff(t *testing.T) {
	var w prefill.ServedWork
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for range 21 {
		w.Note(prefill.ServedWorkSample{At: at, Pods: 2, Sum: 50, OK: true, QueueOK: true})
		at = at.Add(15 * time.Second)
	}
	assert.Equal(t, map[int]float64{2: 50}, w.Peaks(), "21 cycles 15 s apart span prefill.ServedWorkWindow")

	a := NewSaturationAnalyzer(capacity.NewStore())
	fields := a.switchFields()
	require.Contains(t, fields, SwitchPrefillVPServedUnqueued)
	assert.False(t, *fields[SwitchPrefillVPServedUnqueued], "the controller keeps no served work")
}

// The prefix hit at another prefill count (prefill.HitAtCountOf and
// HitAtCount.GPUHitAt, #425/#469/#472) and f against the load
// (prefill.FLoadCurve, #493) price from this package; their switches,
// SwitchPrefillHitAtCount and SwitchPrefillFLoadCurve, are off from the
// constructor, so the controller fits no curve and moves no hit.
func TestParity_TheHitAtCountAndTheFLoadCurveAreSharedAndOff(t *testing.T) {
	r, ok := prefill.HitAtCountOf(0.5, 5000, 2, 1, nil)
	require.True(t, ok)
	assert.InDelta(t, 0.2, r.GPUHitAt(0.4), 1e-12)
	assert.False(t, prefill.FLoadCurve{}.Identified())

	a := NewSaturationAnalyzer(capacity.NewStore())
	fields := a.switchFields()
	for _, name := range []string{SwitchPrefillHitAtCount, SwitchPrefillFLoadCurve} {
		require.Contains(t, fields, name)
		assert.False(t, *fields[name], "%s: off from the constructor", name)
	}
}

// The fleet's spread of output lengths, D, is fleet.OutputMoments' reading
// at the analyzer's weights, fleet.Mean's default: any positive request
// rate. learnreplay's TestParity_TheOutputSpreadIsTheAnalyzersReading pins
// the planner's reading of the same rows: one figure where every rate is
// read; where one is not, the analyzer counts that row and the planner
// leaves it out, and where none is, the planner weighs the rows alike.
func TestParity_TheOutputSpreadIsShared(t *testing.T) {
	row := func(pod string, rate, out, squared float64, known bool) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{PodName: pod, VariantName: "dec", RequestRate: rate, RequestRateKnown: known,
			AvgInputTokens: 1000, AvgOutputTokens: out, AvgOutputTokensSquared: squared, AvgOutputTokensSquaredKnown: true}
	}
	rows := func(first, second bool) []domain.ReplicaMetrics {
		return []domain.ReplicaMetrics{row("dec-0", 9, 100, 20000, first), row("dec-1", 1, 1000, 1.5e6, second)}
	}
	roles := map[string]string{"dec": "decode"}
	const both = 168000.0 / (190 * 190)
	shared, ok := fleet.OutputDispersion(rows(true, true), outputCounted(roles))
	require.True(t, ok)
	assert.Equal(t, shared, fleetOutputDispersion(rows(true, true), roles), "the shared reading, byte for byte")
	assert.InDelta(t, both, fleetOutputDispersion(rows(true, true), roles), 1e-12)
	assert.InDelta(t, both, fleetOutputDispersion(rows(true, false), roles), 1e-12,
		"the analyzer's weights: a positive rate counts, read or not (the planner reads 2)")
	assert.InDelta(t, both, fleetOutputDispersion(rows(false, false), roles), 1e-12,
		"no rate read: still weighed by the rates (the planner weighs the rows alike, 760000/550^2)")
}

// The draft tokens a step accepts are read by one rule
// (domain.ReplicaMetrics.AcceptedPerStep): the analyzer's step budget holds
// them at num_speculative_tokens, k 3 here, so a reading of 3.5 emits 4 a
// step; learnreplay's TestParity_TheAcceptedPerStepIsOneRule pins the
// planner's reading of the same row, unclamped at 3.5.
func TestParity_TheAcceptedPerStepIsOneRuleHeldAtK(t *testing.T) {
	rm := budgetRow()
	rm.SpecAcceptedPerStep, rm.SpecAcceptedPerStepKnown = 3.5, true
	params := budgetParams()
	require.Equal(t, 3, params.SpeculativeTokens())
	accepted, ok := rm.AcceptedPerStep(params.SpeculativeTokens())
	require.True(t, ok)
	b := decodeStepBudget(rm, domain.RoleDecode, params)
	assert.Equal(t, 1+accepted, b.emitted, "the shared reading, byte for byte")
	assert.Equal(t, 4.0, b.emitted, "held at k")
	assert.Equal(t, 4.0, b.stepTokens)
}
