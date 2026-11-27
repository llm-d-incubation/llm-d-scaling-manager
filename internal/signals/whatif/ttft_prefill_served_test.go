package whatif

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A prefill knee priced from a lower bound of V_P is raised to the rate the
// prefill pods' sustained work serves at the prompt priced, at the pod count
// priced, and flagged; a knee from a V_P that is no bound, or at another pod
// count, or over the floor, or with no work kept, is left as priced.
func TestServedKneeFloor(t *testing.T) {
	p := Prefill{Replicas: 2, PromptTokens: 1000}
	served := map[int]float64{2: 30000, 3: 90000}
	lb := Answer{Value: 10, Flags: []Flag{VPLowerBound}, Basis: Basis{TTFT: &TTFTBasis{KneeRate: 10}}}

	got, ok := ServedKneeFloor(lb, p, served, nil).(Answer)
	require.True(t, ok)
	assert.InDelta(t, 30.0, got.Value, 1e-9, "30000 tok/s over 1000-token prompts")
	assert.True(t, got.HasFlag(KneeAtServedWork))
	assert.True(t, got.HasFlag(VPLowerBound), "still a lower bound")
	assert.InDelta(t, 30.0, got.Basis.TTFT.KneeRate, 1e-9)
	require.NotNil(t, got.Sensitivity, "the operating point reads the knee's span")
	assert.InDelta(t, 30.0, got.Sensitivity.Lo, 1e-9, "no lower than the work measured")
	assert.InDelta(t, 10.0, lb.Basis.TTFT.KneeRate, 1e-9, "the priced answer is not changed in place")
	assert.False(t, lb.HasFlag(KneeAtServedWork))

	spanned := lb
	spanned.Sensitivity = &Sensitivity{Lo: 8, Hi: 45}
	got, ok = ServedKneeFloor(spanned, p, served, nil).(Answer)
	require.True(t, ok)
	assert.Equal(t, Sensitivity{Lo: 30, Hi: 45}, *got.Sensitivity, "the span's high side as priced where higher")

	plain := Answer{Value: 10}
	assert.Equal(t, plain, ServedKneeFloor(plain, p, served, nil), "a knee from a V_P that is no bound: a window past it contradicts the model")
	high := Answer{Value: 40, Flags: []Flag{VPLowerBound}}
	assert.Equal(t, high, ServedKneeFloor(high, p, served, nil), "never lowered")
	other := p
	other.Replicas = 4
	assert.Equal(t, lb, ServedKneeFloor(lb, other, served, nil), "no work read at the pod count priced")
	assert.Equal(t, lb, ServedKneeFloor(lb, p, nil, nil), "no work kept: the switch off")
	noPrompt := p
	noPrompt.PromptTokens = 0
	assert.Equal(t, lb, ServedKneeFloor(lb, noPrompt, served, nil), "no prompt to serve")
	refused := Unanswerable{Reason: NoPrefillCapacity}
	assert.Equal(t, Result(refused), ServedKneeFloor(refused, p, served, nil))
}

// #477: where the prefill pods served at the pod count priced only with
// their backlog growing, no work floors the knee: it is left at its lower
// bound and flagged ServedWorkUnsteady. A count with steady work is floored
// as before, unflagged.
func TestServedKneeFloor_UnsteadyWorkIsNoFloor(t *testing.T) {
	p := Prefill{Replicas: 2, PromptTokens: 1000}
	served, unsteady := map[int]float64{3: 90000}, map[int]bool{2: true}
	lb := Answer{Value: 10, Flags: []Flag{VPLowerBound}}
	got, ok := ServedKneeFloor(lb, p, served, unsteady).(Answer)
	require.True(t, ok)
	assert.InDelta(t, 10.0, got.Value, 1e-9, "left at its lower bound")
	assert.True(t, got.HasFlag(ServedWorkUnsteady))
	assert.False(t, got.HasFlag(KneeAtServedWork))
	assert.False(t, lb.HasFlag(ServedWorkUnsteady), "the priced answer is not changed in place")

	p.Replicas = 3
	got, ok = ServedKneeFloor(lb, p, served, unsteady).(Answer)
	require.True(t, ok)
	assert.InDelta(t, 90.0, got.Value, 1e-9)
	assert.False(t, got.HasFlag(ServedWorkUnsteady))
}
