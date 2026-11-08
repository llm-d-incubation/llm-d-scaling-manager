package allocation

import (
	"math"
	"testing"
)

// TestReplicaCountsDoNotOverflow: a need at or above 2^63 replicas -- one
// analyzer's tiny but finite capacity is enough -- converted to a negative int
// and then read as no need at all. It is bounded instead.
func TestReplicaCountsDoNotOverflow(t *testing.T) {
	for _, tc := range []struct {
		name          string
		demand, prc   float64
		up, removable int
	}{
		{"2^63", math.Exp2(63), 1, maxReplicaCount, maxReplicaCount},
		{"1e300", 1e300, 1, maxReplicaCount, maxReplicaCount},
		{"tiny capacity", 800, 1e-300, maxReplicaCount, maxReplicaCount},
		{"just under the bound", maxReplicaCount - 1, 1, maxReplicaCount - 1, maxReplicaCount - 1},
		{"NaN", math.NaN(), 1, 0, 0},
		{"ordinary", 1000, 300, 4, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := replicasForDemand(tc.demand, tc.prc); got != tc.up {
				t.Errorf("replicasForDemand(%v, %v) = %d, want %d", tc.demand, tc.prc, got, tc.up)
			}
			if got := safeReplicasForSpare(tc.demand, tc.prc); got != tc.removable {
				t.Errorf("safeReplicasForSpare(%v, %v) = %d, want %d", tc.demand, tc.prc, got, tc.removable)
			}
		})
	}
}
