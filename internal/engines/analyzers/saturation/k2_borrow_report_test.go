package saturation

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// Replicas in different output buckets borrow at different stages of
// relaxation. The variant reports the one held hardest: that is the replica
// the borrow constrains most, and the one an operator needs to see.
func TestK2BorrowOfReportsTheLeastRelaxedReplica(t *testing.T) {
	rows := []capacity.ReplicaCapacity{
		{K2Priority: capacity.K2SrcBorrowed, K2BorrowedFrom: "main", K2BorrowRelaxed: 0.8},
		{K2Priority: capacity.K2SrcObserved},
		{K2Priority: capacity.K2SrcBorrowed, K2BorrowedFrom: "main", K2BorrowRelaxed: 0.2},
		{K2Priority: capacity.K2SrcBorrowed, K2BorrowedFrom: "main", K2BorrowRelaxed: 0.5},
	}
	assert.Equal(t, []domain.LearnedStateBorrow{
		{Quantity: domain.BorrowedK2, Donor: "main", DonorNamespace: "ns", Relaxed: 0.2},
	}, k2BorrowOf("ns", rows))

	assert.Empty(t, k2BorrowOf("ns", []capacity.ReplicaCapacity{{K2Priority: capacity.K2SrcObserved}}),
		"a variant that borrowed nothing reported a borrow")
}

// A bridge is a warm-pool Pod lent to the variant; ownRowsOf leaves it out.
func TestOwnRowsOfLeavesOutBridges(t *testing.T) {
	rows := []capacity.ReplicaCapacity{
		{PodName: "own", K2Priority: capacity.K2SrcBorrowed, K2BorrowedFrom: "main"},
		{PodName: "bridge", FromWarmPool: true, K2Priority: capacity.K2SrcBorrowed, K2BorrowedFrom: "pool"},
	}
	got := ownRowsOf(rows)
	assert.Len(t, got, 1)
	assert.Equal(t, "own", got[0].PodName)
}
