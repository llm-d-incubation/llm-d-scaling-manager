package steadystate

import (
	"slices"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// A short model whose confirmed hard imbalance moves at the pace because its
// burst is expected to end says so, whether or not the concurrency limit is
// reached: a model taking a replica a cycle through a long shortfall
// otherwise shows no reason at all. Control: with no burst on record its
// moves are unpaced, and it does not.
func TestUtilizationShareBurstEndingIsAModelsReason(t *testing.T) {
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
		for range allocation.ShareUnpacedAfterCycles {
			l.ConfirmHard(map[string]bool{"ns/B/both": true})
		}
		return shareBlockedReasons(l, g, ev, nil, nil, allocation.ShareSkipWaits{}, t0, tm)["ns/B"]
	}
	got := reasons(true)
	if !slices.Contains(got, constants.ScalingBlockedBurstEnding) {
		t.Fatalf("B, short and paced because its burst ends before a move lands, reports %v, want %s",
			got, constants.ScalingBlockedBurstEnding)
	}
	if slices.Contains(got, constants.ScalingBlockedTransferLimit) {
		t.Fatalf("B reports transfer-limit with no transfer in flight: %v", got)
	}
	if got := reasons(false); slices.Contains(got, constants.ScalingBlockedBurstEnding) {
		t.Fatalf("control: B, with no burst on record, is unpaced yet reports %v", got)
	}
}

// A role the plan capped on its mean need as a fast swinger says so on its
// model; one not capped does not.
func TestUtilizationShareFastSwingCappedIsAModelsReason(t *testing.T) {
	g, ev := shortGroup()
	reasons := func(meanCapped []string) []string {
		return shareBlockedReasons(allocation.NewShareLedger(), g, ev, nil, meanCapped, allocation.ShareSkipWaits{},
			time.Unix(0, 0), allocation.ShareTimings{})["ns/C"]
	}
	if got := reasons([]string{"ns/C/both"}); !slices.Contains(got, constants.ScalingBlockedFastSwingCapped) {
		t.Fatalf("C, capped on its mean, reports %v, want %s", got, constants.ScalingBlockedFastSwingCapped)
	}
	if got := reasons([]string{"ns/A/both"}); slices.Contains(got, constants.ScalingBlockedFastSwingCapped) {
		t.Fatalf("control: C, not capped on its mean, reports %v", got)
	}
}

// Every reason the optimizer reports is one it owns, so it is cleared when it
// stops holding.
func TestUtilizationSharePaceReasonsAreOwned(t *testing.T) {
	for _, r := range []string{constants.ScalingBlockedBurstEnding, constants.ScalingBlockedFastSwingCapped} {
		if !slices.Contains(constants.ScalingBlockedReasonsUtilizationShare, r) {
			t.Errorf("%s is not in ScalingBlockedReasonsUtilizationShare", r)
		}
	}
}
