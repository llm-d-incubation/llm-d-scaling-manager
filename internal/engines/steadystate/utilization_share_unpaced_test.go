package steadystate

import (
	"slices"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// A confirmed hard imbalance is exempt from the concurrency limit only while
// its moves are unpaced: a receiver whose burst is expected to end before a
// move lands moves at the pace, so the limit is what holds it back, and it
// says so.
func TestUtilizationShareShortBurstReceiverReportsTheTransferLimit(t *testing.T) {
	tm := allocation.ShareTimings{Window: time.Minute, ReleaseTimeout: time.Hour, FillTimeout: time.Hour,
		ReversalHold: 10 * time.Minute, SwingWindow: time.Hour}
	t0 := time.Unix(0, 0)
	g, ev := shortGroup()
	reasons := func(short bool) []string {
		l := allocation.NewShareLedger()
		if short {
			// B burst for 3 minutes, well under a move (SwingWindow/8).
			for i, n := range []float64{2, 20, 20, 20, 2, 2} {
				l.RecordNeeds(map[string]float64{"ns/B/both": n}, t0.Add(time.Duration(i-10)*time.Minute), tm)
			}
		}
		held := map[string]int{"ns/A/both": 14, "ns/C/both": 6}
		for range allocation.ShareMaxConcurrentTransfers {
			l.Start(allocation.ShareTransfer{Donor: "ns/A/both", Receiver: "ns/C/both", GPUs: 1, DonorGPUs: 1}, held, t0, tm)
		}
		for range allocation.ShareUnpacedAfterCycles {
			l.ConfirmHard(map[string]bool{"ns/B/both": true})
		}
		return shareBlockedReasons(l, g, ev, nil, nil, allocation.ShareSkipWaits{}, t0, tm)["ns/B"]
	}
	if got := reasons(true); !slices.Contains(got, constants.ScalingBlockedTransferLimit) {
		t.Fatalf("B, whose burst ends before a move lands, moves at the pace: want transfer-limit, got %v", got)
	}
	if got := reasons(false); slices.Contains(got, constants.ScalingBlockedTransferLimit) {
		t.Fatalf("control: B's confirmed hard imbalance is unpaced, yet reports transfer-limit: %v", got)
	}
}
