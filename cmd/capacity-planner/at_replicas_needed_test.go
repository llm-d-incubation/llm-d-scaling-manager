package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// The fleet a verdict says is needed rises with the load and is the fewest
// replicas in all (the staircase states 1 GPU a replica for both roles, where
// the fewest GPUs are the fewest replicas; TestAtReplicas_NeededFewestGPUs
// checks unequal GPUs): for each shape, across a sweep of loads, its total never
// falls as the load rises, it holds, and no fleet of one replica fewer in
// all holds. The TTFT target is at p95, where decode admission sets most of
// the 16k-prompt shape's TTFT: there a search that fixes decode at its own
// floor and adds prefill found 19 prefill + 5 decode at 41.7 r/s and 9 + 8
// at 62.6 r/s.
func TestAtReplicas_NeededRisesWithLoad(t *testing.T) {
	st := stairReplay.state(t)
	loads := []float64{30, 36, 41.7, 48, 54, 62.6}
	for _, shape := range []string{"measured", "1k/1k", "16k/200/0.3"} {
		t.Run(shape, func(t *testing.T) {
			sh, err := whatifapi.ParseReportShape(shape)
			require.NoError(t, err)
			q := stairQuery(2, 2, 0)
			q.TTFT.Percentile = 0.95
			q.Shapes = []whatifapi.ReportShapeAsk{sh}
			r := &reporter{st: st, s: &servedState{st: st, decodeVariant: "variant-01"}, q: q, fleets: map[fleetKey]*servedFleet{}}
			r.resolveTargets()
			r.sh = sh
			prev, prevLoad := 0, 0.0
			for _, load := range loads {
				r.load = load
				base, err := r.evaluateAt(2, 2, load, false)
				require.NoError(t, err)
				var v whatifapi.ReportVerdict
				_, _, err = r.needed(2, 2, base, &v)
				require.NoError(t, err)
				require.NotNil(t, v.Needed)
				n := *v.Needed
				require.Empty(t, n.Unmet, "%s at %g r/s", shape, load)
				total := n.Prefill + n.Decode
				assert.GreaterOrEqual(t, total, prev, "%s: %d + %d at %g r/s, %d in all at %g r/s", shape, n.Prefill, n.Decode, load, prev, prevLoad)
				prev, prevLoad = total, load

				holds := func(np, nd int) bool {
					t.Helper()
					e, err := r.evaluateAt(np, nd, load, false)
					require.NoError(t, err)
					return e.holds
				}
				assert.True(t, holds(n.Prefill, n.Decode), "%s: %d + %d at %g r/s", shape, n.Prefill, n.Decode, load)
				// Holding is monotone in each count, so no fleet of fewer in
				// all holds when none of one fewer does. r.peak is unset, so
				// needed asks nothing at the peak, and holding at the load is
				// the whole test; with a peak above the load this oracle
				// would also have to ask a cut role at the peak.
				for d := 1; d <= total-2; d++ {
					assert.False(t, holds(total-1-d, d), "%s at %g r/s: %d + %d holds, fewer than %d + %d",
						shape, load, total-1-d, d, n.Prefill, n.Decode)
				}
			}
		})
	}
}
