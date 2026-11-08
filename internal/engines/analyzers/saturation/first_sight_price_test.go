package saturation

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle/settlerow"
)

// Which way change E moves the price after a restart, measured
// (analyzer-learner.md, 4.4 and PR 5, change E). Each case runs the same
// rows from the same restored document, the switch on and off, and compares
// the per-replica capacity and the replicas it implies, cycle by cycle.

// priceCycle is what one cycle priced.
type priceCycle struct {
	capacity int64
	replicas float64
}

// priced runs rows through r for n cycles and returns what each priced.
func priced(t *testing.T, r *restartRig, n int, rows []domain.ReplicaMetrics, states []domain.VariantReplicaState) []priceCycle {
	t.Helper()
	out := make([]priceCycle, 0, n)
	for range n {
		r.tr.Observe(settleModel("test-ns", "test-model"), settlerow.Of(rows, map[string]string{"v": domain.RoleDecode}), r.now)
		res, err := r.a.Analyze(context.Background(), makeAnalyzerInput(rows, states))
		require.NoError(t, err)
		require.Len(t, res.VariantCapacities, 1)
		out = append(out, priceCycle{int64(res.VariantCapacities[0].PerReplicaCapacity), impliedReplicas(res)})
		r.now = r.now.Add(restartCycle)
	}
	return out
}

// tokenRows is four replicas at tokens with a queue, Ready for an hour.
func tokenRows(tokens int64, queue int) []domain.ReplicaMetrics {
	out := make([]domain.ReplicaMetrics, 0, 4)
	for i := range 4 {
		rm := makeReplicaMetrics(fmt.Sprintf("d%d", i), "v", tokens, 376_480, queue, 1000, 500)
		rm.GenerationTokenRate, rm.RequestRate = 3000, 6
		rm.ReadySince = restartT0.Add(-time.Hour)
		out = append(out, rm)
	}
	return out
}

// firstSightPrices is what on and off price after a restart from before.
func firstSightPrices(t *testing.T, before *restartRig, rows []domain.ReplicaMetrics) (on, off []priceCycle) {
	t.Helper()
	const cycles = 8
	on = priced(t, restartFromSnapshot(t, before, true, "v"), cycles, rows, fourStates())
	off = priced(t, restartFromSnapshot(t, before, false, "v"), cycles, rows, fourStates())
	for i := range cycles {
		t.Logf("cycle %d: on capacity %d, %.2f replicas; off capacity %d, %.2f replicas",
			i, on[i].capacity, on[i].replicas, off[i].capacity, off[i].replicas)
	}
	return on, off
}

// No k2 on record: the fleet saturates for the first time just after the
// restart. The wait holds the fleet at k1, the high figure, so it asks for
// fewer replicas. E ends that sooner: capacity falls to k2 sooner, and the
// fleet is sized HIGHER than with the wait, never lower.
func TestFirstSight_WithNoK2OnRecordTheFleetIsSizedHigher(t *testing.T) {
	before := snapshotRig(t, restartT0, "v")
	for range waitWindow + 2 {
		before.cycle(tokenRows(40_000, 0), fourStates())
	}
	on, off := firstSightPrices(t, before, tokenRows(260_000, 160))
	moved := false
	for i := range on {
		require.False(t, on[i].replicas < off[i].replicas-1e-9, "cycle %d: %+v against %+v", i, on[i], off[i])
		moved = moved || on[i].replicas > off[i].replicas+1e-9
	}
	require.True(t, moved, "premise: the wait prices k1 and E does not")
	require.Equal(t, on[len(on)-1], off[len(off)-1], "the two meet once the wait ends")
}

// A k2 on record that a restart finds stale and low: the fleet now carries
// more. This is the case that sizes lower: the readings the wait refused
// raise the window sooner. How much is the gap between the stored figure and
// the readings, for at most the wait, and the two meet when it ends.
func TestFirstSight_AStaleLowK2IsTheCaseThatSizesLower(t *testing.T) {
	before := snapshotRig(t, restartT0, "v")
	for range waitWindow + 2 {
		before.cycle(tokenRows(200_000, 160), fourStates())
	}
	on, off := firstSightPrices(t, before, tokenRows(260_000, 160))
	worst := 0.0
	for i := range on {
		require.False(t, on[i].capacity < off[i].capacity, "cycle %d: %+v against %+v", i, on[i], off[i])
		worst = math.Max(worst, off[i].replicas-on[i].replicas)
	}
	t.Logf("sized lower by at most %.3f replicas of %.3f", worst, off[0].replicas)
	require.Positive(t, worst, "premise: the case moves the fleet")
	last := len(on) - 1
	require.InDelta(t, off[last].replicas, on[last].replicas, 0.5*worst+1e-9, "the gap closes once the wait ends")
}
