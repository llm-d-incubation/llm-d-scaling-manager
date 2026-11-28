package whatif

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// The hit is read on the most pods any cycle of the window served, or on
// the latest cycle's where that is more: a window that spans a scale-down
// reads the larger count's hit. A nil window reads the latest count.
func TestPromptWindow_HitFitCount(t *testing.T) {
	w := &PromptWindow{}
	w.Note([]domain.ReplicaMetrics{computedRow(16000, 2, 32000), computedRow(16000, 2, 32000), computedRow(16000, 2, 32000)}, 0, time.Unix(0, 0))
	w.Note([]domain.ReplicaMetrics{computedRow(16000, 2, 32000)}, 1, time.Unix(15, 0))
	assert.Equal(t, 3, w.MaxPods())
	assert.Equal(t, 3, w.HitFitCount(1), "the scale-down's larger count")
	assert.Equal(t, 5, w.HitFitCount(5))
	var none *PromptWindow
	assert.Equal(t, 2, none.HitFitCount(2))
}

// The fit's hit is the window's computed-token hit at the measured prompt,
// else the share of the whole prompt asked that E[I_eff] leaves out.
func TestFitTotalHit(t *testing.T) {
	w := &PromptWindow{}
	w.Note([]domain.ReplicaMetrics{computedRow(16000, 2, 32000)}, 0, time.Unix(0, 0))
	p := Prefill{PromptTokens: 6000, WholePromptTokens: 8000}

	h, ok := FitTotalHit(p, true, w)
	require.True(t, ok)
	assert.InDelta(t, 0.75, h, 1e-12, "the window's: 16000 of 64000 tokens a second computed")
	h, ok = FitTotalHit(p, false, w)
	require.True(t, ok)
	assert.InDelta(t, 0.25, h, 1e-12, "a stated prompt: 1 - 6000/8000")
	h, ok = FitTotalHit(p, true, nil)
	require.True(t, ok)
	assert.InDelta(t, 0.25, h, 1e-12, "no window hit read: the whole prompt's")

	_, ok = FitTotalHit(Prefill{PromptTokens: 6000}, false, w)
	assert.False(t, ok, "no whole prompt")
	_, ok = FitTotalHit(Prefill{PromptTokens: 9000, WholePromptTokens: 8000}, false, w)
	assert.False(t, ok, "more computed than asked")
}

// Applied at another count, the hit moves E[I_eff] and the GPU hit as
// prefill.HitAtCountOf and HitAtCount.GPUHitAt price them; at the fit's
// count, with no prompt, or where nothing can be priced, p is left as it is.
func TestApplyHitAtCount(t *testing.T) {
	p := Prefill{Replicas: 1, PromptTokens: 5000, PromptHitRate: 0.4}
	r, ok := ApplyHitAtCount(&p, 0.5, 2, nil)
	require.True(t, ok)
	assert.Equal(t, prefill.HitFloor, r.Rule)
	assert.InDelta(t, 0.25, r.Hit, 1e-12, "the floor: 0.5 x 1/2")
	want, _ := prefill.HitAtCountOf(0.5, 5000, 2, 1, nil)
	assert.Equal(t, want, r)
	assert.InDelta(t, r.Computed, p.PromptTokens, 1e-9, "E[I_eff] moved")
	assert.InDelta(t, 0.2, p.PromptHitRate, 1e-12, "the GPU hit by the total's ratio")

	held := Prefill{Replicas: 4, PromptTokens: 5000, PromptHitRate: 0.4}
	r, ok = ApplyHitAtCount(&held, 0.5, 2, nil)
	require.True(t, ok)
	assert.Equal(t, prefill.HitHeld, r.Rule)
	assert.Equal(t, Prefill{Replicas: 4, PromptTokens: 5000, PromptHitRate: 0.4}, held, "a held hit moves nothing")

	for name, q := range map[string]Prefill{
		"the fit's count": {Replicas: 2, PromptTokens: 5000, PromptHitRate: 0.4},
		"no prompt":       {Replicas: 1, PromptHitRate: 0.4},
	} {
		before := q
		_, ok := ApplyHitAtCount(&q, 0.5, 2, nil)
		assert.False(t, ok, name)
		assert.Equal(t, before, q, name)
	}
	bad := Prefill{Replicas: 1, PromptTokens: 5000}
	_, ok = ApplyHitAtCount(&bad, 1, 2, nil)
	assert.False(t, ok, "a hit of 1 prices nothing")
	assert.Equal(t, 5000.0, bad.PromptTokens)
}

// f against the load is set from the curve P1 fitted at the count priced,
// where it is identified; elsewhere, and on one pod, f is left as it is.
func TestApplyFLoadCurve(t *testing.T) {
	good := prefill.FLoadCurve{Floor: 1.1, Excess: 300, Minutes: prefill.MinFLoadCurveMinutes}
	require.True(t, good.Identified())
	short := prefill.FLoadCurve{Floor: 1.1, Excess: 300, Minutes: prefill.MinFLoadCurveMinutes - 1}
	est := prefill.Estimate{FCurveByPods: map[int]prefill.FLoadCurve{1: good, 2: good, 3: short}}

	p := Prefill{Replicas: 2, Imbalance: 1.3}
	c, ok := ApplyFLoadCurve(&p, est)
	require.True(t, ok)
	assert.Equal(t, good, c)
	assert.Equal(t, 1.1, p.ImbalanceCurveFloor)
	assert.Equal(t, 300.0, p.ImbalanceCurveExcess)
	assert.Equal(t, 1.3, p.Imbalance, "f itself is kept: the price is the higher of the two")

	for name, q := range map[string]Prefill{"one pod": {Replicas: 1}, "too few minutes": {Replicas: 3}, "no curve": {Replicas: 4}} {
		c, ok := ApplyFLoadCurve(&q, est)
		assert.False(t, ok, name)
		assert.Zero(t, q.ImbalanceCurveFloor, name)
		assert.Zero(t, q.ImbalanceCurveExcess, name)
		if name == "too few minutes" {
			assert.Equal(t, short, c, "the curve read, for the caller to say why")
		}
	}
	c, ok = ApplyFLoadCurve(&Prefill{Replicas: 2}, prefill.Estimate{})
	assert.False(t, ok, "the switch off fits no curve")
	assert.Zero(t, c)
}
