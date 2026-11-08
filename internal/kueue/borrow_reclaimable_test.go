package kueue

import "testing"

// Review N1 / K2-1: a reclaiming queue's LentOut is what peers borrow that
// Kueue would preempt them for, past what the free pool covers -- not its
// unused nominal. 8-GPU units.
func TestLentOutIsWhatPeersBorrow(t *testing.T) {
	for _, tc := range []struct {
		name          string
		q             QueueResource
		peers         []QueueResource
		lent, after   int
		wantFreeCheck int
	}{
		{"unused nominal, nobody borrows: nothing lent out, growth from the pool",
			QueueResource{Nominal: 32, Reserved: 16}, []QueueResource{{Nominal: 16, Reserved: 8}}, 0, 16, 24},
		{"a peer borrows all of q's unused nominal: all of it lent out",
			QueueResource{Nominal: 32, Reserved: 16}, []QueueResource{{Nominal: 0, Reserved: 16, Borrowed: 16}}, 16, 0, 0},
		{"a peer borrows 8 of 16 unused: 8 from the pool first, then 8 lent out",
			QueueResource{Nominal: 16}, []QueueResource{{Nominal: 0, Reserved: 8, Borrowed: 8}}, 8, 8, 8},
		{"kept part grows without preemption",
			QueueResource{Nominal: 16, LendingLimit: ptr(8)}, []QueueResource{{Nominal: 0, Reserved: 8, Borrowed: 8}}, 8, 8, 0},
		{"a peer borrows from another lender's spare: q lends nothing it needs back",
			QueueResource{Nominal: 8}, []QueueResource{{Nominal: 0, Reserved: 8, Borrowed: 8}, {Nominal: 8}}, 0, 8, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			qs := make([]QueueState, 0, 1+len(tc.peers))
			qs = append(qs, withReclaim(member("q", tc.q), ReclaimAny))
			for i, p := range tc.peers {
				qs = append(qs, member(string(rune('a'+i)), p))
			}
			f := ComputeBorrowing(topology(qs...), false).Queues["q"][0]
			if f.CohortFree != tc.wantFreeCheck {
				t.Fatalf("cohort free %d, want %d", f.CohortFree, tc.wantFreeCheck)
			}
			if f.LentOut != tc.lent || f.PreemptAfter != tc.after {
				t.Fatalf("LentOut %d PreemptAfter %d, want %d and %d", f.LentOut, f.PreemptAfter, tc.lent, tc.after)
			}
		})
	}
	never := ComputeBorrowing(topology(member("q", QueueResource{Nominal: 16}),
		member("a", QueueResource{Nominal: 0, Reserved: 16, Borrowed: 16})), false).Queues["q"][0]
	if never.LentOut != 0 || never.PreemptAfter != 0 {
		t.Fatalf("a queue that cannot reclaim: LentOut %d PreemptAfter %d, want 0 and 0", never.LentOut, never.PreemptAfter)
	}
}
