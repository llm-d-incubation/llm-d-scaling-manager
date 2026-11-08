package saturation

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// restingRows is the twelve pods near k 0.61, ITL scale times the rig's line,
// and with wide two more that span the set value.
func restingRows(wide bool, scale float64) []domain.ReplicaMetrics {
	rows := make([]domain.ReplicaMetrics, 0, 14)
	for i := range 12 {
		rm := itlSettleRow(fmt.Sprintf("p%d", i), 0.50+0.02*float64(i))
		rm.AvgITL *= scale
		rows = append(rows, rm)
	}
	if wide {
		rows = append(rows, itlSettleRow("lo", 0.25), itlSettleRow("hi", 0.75))
	}
	return rows
}

// A resting full fit prices its own key and teaches nothing outside it: the
// shared line, the prior on B read from it, a sibling's borrow and the snapshot
// all stay on the last fit taken at the set values.
func TestITLResting_TheFitPricesItsKeyAndPublishesNothing(t *testing.T) {
	r := oneSidedRig(t, nil)
	lineAfter(t, r, 20, func(int) []domain.ReplicaMetrics { return restingRows(true, 1) })
	// The wide pods gone, the window holds a spread only the latch keeps.
	lineAfter(t, r, 6, func(int) []domain.ReplicaMetrics { return restingRows(false, 1) })
	require.Len(t, r.a.itlLines, 1, "premise: the line was published")
	var physicsKey string
	for k := range r.a.itlLines {
		physicsKey = k
	}
	published := r.a.itlLines[physicsKey]
	prior := r.a.itlBPrior(itl.PriorScope{}, physicsKey)

	own := lineAfter(t, r, 6, func(int) []domain.ReplicaMetrics { return restingRows(false, 1.02) })

	checked := false
	for key, held := range r.a.itlTracker.WindowLens() {
		if held == 0 {
			continue
		}
		checked = true
		read := r.a.itlTracker.Readings(key)
		require.True(t, read.OLSLatched, "premise: the key stands at the full fit")
		spread := read.Steady[len(read.Steady)-1].K
		lo, hi := spread, spread
		for _, o := range read.Steady {
			lo, hi = min(lo, o.K), max(hi, o.K)
		}
		require.Greater(t, hi-lo, itl.DefaultClearKSpread, "premise: over the clear value")
		require.Less(t, hi-lo, itl.DefaultMinKSpread, "premise: under the set value")
	}
	require.True(t, checked, "premise: no window held readings")
	require.NotEqual(t, published.model, own, "the key's own line must be the resting fit")
	require.Greater(t, own.ITLAt(0.68), published.model.ITLAt(0.68), "the resting fit must price the readings at 1.02x")

	require.Equal(t, published.model, r.a.itlLines[physicsKey].model, "a resting fit moved the shared line")
	require.Equal(t, published.learnedAt, r.a.itlLines[physicsKey].learnedAt, "a resting fit stamped the shared line")
	require.Equal(t, prior, r.a.itlBPrior(itl.PriorScope{}, physicsKey), "a resting fit moved the prior on B")
	borrowed, _, ok := r.a.borrowLine(physicsKey, "someone-else", false)
	require.True(t, ok)
	require.Equal(t, published.model, borrowed, "a sibling borrowed a resting fit")
	snap := r.a.ExportLearnedState().ITLLines[physicsKey]
	require.Equal(t, published.model, itl.Model{A: snap.A, B: snap.B}, "the snapshot holds a resting fit")
}
