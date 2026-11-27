package prefill

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The lab run of issue #384 (64 shared documents, Qwen3-32B, one prefill
// pod's GPU cache under the documents' working set), its clean windows
// pooled: on 2 prefill pods a request computed 7,532 of 15,962 prompt tokens;
// on 1 pod, 10,454; on 4, 3,205.
const (
	labWhole       = 15962.0
	labComputedAt1 = 10454.0
	labComputedAt2 = 7532.0
	labComputedAt4 = 3205.0
)

// Fitted on 2 pods and priced on 1, the floor reads the computed tokens at
// or above what 1 pod computed (about 11,750 against 10,454), where holding
// the 2-pod hit read 7,532, 28% under. On 4 pods the hit is held: 7,532,
// over the 3,205 measured, which is safe for capacity.
func TestHitAtCount_TheLabRunFittedOnTwoPods(t *testing.T) {
	h := 1 - labComputedAt2/labWhole
	at1, ok := HitAtCountOf(h, labComputedAt2, 2, 1, nil)
	require.True(t, ok)
	assert.Equal(t, HitFloor, at1.Rule)
	assert.InDelta(t, h/2, at1.Hit, 1e-12)
	assert.InDelta(t, labWhole*(1-h/2), at1.Computed, 1e-6, "the whole prompt does not move with the count")
	t.Logf("1 pod: hit %.3f, computed %.0f a request (measured %.0f, held %.0f)", at1.Hit, at1.Computed, labComputedAt1, labComputedAt2)
	assert.GreaterOrEqual(t, at1.Computed, labComputedAt1, "never under what 1 pod computed")
	assert.InDelta(t, 11750, at1.Computed, 100)
	assert.True(t, at1.Moved())
	assert.InDelta(t, h/2, at1.Lo, 1e-12)
	assert.Equal(t, h, at1.Hi)

	at4, ok := HitAtCountOf(h, labComputedAt2, 2, 4, nil)
	require.True(t, ok)
	assert.Equal(t, HitHeld, at4.Rule)
	assert.Equal(t, h, at4.Hit)
	assert.Equal(t, labComputedAt2, at4.Computed)
	assert.GreaterOrEqual(t, at4.Computed, labComputedAt4, "held is pessimistic at more pods")
	assert.False(t, at4.Moved())
	assert.Equal(t, h, at4.Lo)
	assert.Equal(t, 1.0, at4.Hi, "min(1, h x 2)")
}

// At the fit's count nothing moves.
func TestHitAtCount_TheSameCountMovesNothing(t *testing.T) {
	a, ok := HitAtCountOf(0.4, 6000, 3, 3, nil)
	require.True(t, ok)
	assert.Equal(t, HitAtCount{FitHit: 0.4, Hit: 0.4, FitCount: 3, Count: 3, Rule: HitSameCount, Lo: 0.4, Hi: 0.4,
		Computed: 6000, ComputedScale: 1, GPUScale: 1, TierScale: 1}, a)
	assert.False(t, a.Moved())
}

// The edges: no hit stays no hit at any count; a hit of 1 or more, a NaN or
// negative hit, no pods on either side, and a computed prompt that is not a
// positive finite number price nothing.
func TestHitAtCount_Edges(t *testing.T) {
	for _, n := range []int{1, 2, 8} {
		a, ok := HitAtCountOf(0, 5000, 4, n, nil)
		require.True(t, ok, "h = 0 at %d", n)
		assert.Equal(t, 0.0, a.Hit)
		assert.Equal(t, 5000.0, a.Computed)
		assert.False(t, a.Moved())
	}
	for name, c := range map[string]struct {
		h, computed     float64
		fitCount, count int
	}{
		"N' = 0":            {0.5, 5000, 2, 0},
		"N = 0":             {0.5, 5000, 0, 2},
		"N' < 0":            {0.5, 5000, 2, -1},
		"h = 1":             {1, 5000, 2, 1},
		"h above 1":         {1.2, 5000, 2, 1},
		"h negative":        {-0.1, 5000, 2, 1},
		"h NaN":             {math.NaN(), 5000, 2, 1},
		"h +Inf":            {math.Inf(1), 5000, 2, 1},
		"computed 0":        {0.5, 0, 2, 1},
		"computed NaN":      {0.5, math.NaN(), 2, 1},
		"computed +Inf":     {0.5, math.Inf(1), 2, 1},
		"computed negative": {0.5, -1, 2, 1},
	} {
		_, ok := HitAtCountOf(c.h, c.computed, c.fitCount, c.count, nil)
		assert.False(t, ok, name)
	}
}

// The floor scales the hit by N'/N at every pair below the fit's count, and
// the computed tokens by (1 - h')/(1 - h).
func TestHitAtCount_TheFloor(t *testing.T) {
	a, ok := HitAtCountOf(0.6, 4000, 4, 1, nil)
	require.True(t, ok)
	assert.InDelta(t, 0.15, a.Hit, 1e-12)
	assert.InDelta(t, 4000*0.85/0.4, a.Computed, 1e-9)
	assert.InDelta(t, 0.85/0.4, a.ComputedScale, 1e-12)
	a, ok = HitAtCountOf(0.6, 4000, 4, 3, nil)
	require.True(t, ok)
	assert.InDelta(t, 0.45, a.Hit, 1e-12)
}
