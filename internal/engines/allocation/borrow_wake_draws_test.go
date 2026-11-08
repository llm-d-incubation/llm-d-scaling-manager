package allocation

import (
	"testing"
	"time"
)

// Review N3: a wake that grows a reclaiming lender past what the free pool
// covers records what Kueue reclaims for it, so the next wake and the next
// steady-state cycle do not count the same borrowed GPUs as reclaimable. Queue
// r (Any) lent its 16 to a borrower; the pool is empty; an 8+8 pair wakes.
func TestWakeRecordsItsReclaim(t *testing.T) {
	c := borrowQuota(0, nsBorrow{ns: "r", guaranteed: 16, cap: 0, wn: 16, lentOut: 16})
	reclaim := ReclaimableNominal([]*ResourceConstraints{c}, "r")
	draws := WakeDraws([]*ResourceConstraints{c}, "r", "m", []WakeReplica{
		{Variant: "d", Accelerator: "H100", GPUs: 8}, {Variant: "p", Accelerator: "H100", GPUs: 8}}, nil, reclaim)
	record := NewBorrowInFlight()
	now := time.Now()
	record.RecordWake(draws, now)
	left := ReclaimableNominal(DeductReclaimed([]*ResourceConstraints{c}, record.Unreserved(now).InFlight), "r")
	if got := left["H100"].GPUs; got != 0 {
		t.Fatalf("after the pair's wake %d GPUs are still reclaimable, want 0 (draws %+v)", got, draws)
	}
	if len(record.Entries()) != 0 {
		t.Fatalf("a reclaim drew on the pool: %+v", record.Entries())
	}
}
