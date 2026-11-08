package allocation

import (
	"testing"
	"time"
)

func TestShareBurstEndsWithin(t *testing.T) {
	tm := spareTimings()
	t0 := time.Unix(10000, 0)
	record := func(needs ...float64) *ShareLedger {
		l := NewShareLedger()
		for i, n := range needs {
			l.RecordNeeds(map[string]float64{"R": n}, t0.Add(time.Duration(i)*time.Minute), tm)
		}
		return l
	}
	// A completed burst of 4 minutes, and one under way for 1.
	if l := record(1, 9, 9, 9, 9, 1, 1, 9, 9); !l.BurstEndsWithin("R", 4*time.Minute) {
		t.Error("3 minutes left of a 4-minute burst does not end within 4 minutes")
	}
	if l := record(1, 9, 9, 9, 9, 1, 1, 9, 9); l.BurstEndsWithin("R", 3*time.Minute) {
		t.Error("3 minutes left of a 4-minute burst ends within 3 minutes")
	}
	// Not under way: a whole burst is ahead.
	if l := record(1, 9, 9, 9, 9, 1, 1); l.BurstEndsWithin("R", 4*time.Minute) {
		t.Error("a 4-minute burst not yet started ends within 4 minutes")
	}
	if l := record(1, 9, 9, 9); l.BurstEndsWithin("R", time.Hour) {
		t.Error("a role with no completed burst has an expected end")
	}
	if l := record(5, 5, 5, 5); l.BurstEndsWithin("R", time.Hour) {
		t.Error("a flat need has an expected end")
	}
}

// lAfterBurst is how many replicas Q moves to L over the two cycles of a
// hard imbalance (spareRun) when L last burst for burst: completed before
// the burst under way, which starts at 900 (burstHistory) and has lasted
// 115s by the second cycle. With no completed burst, burst is 0.
func lAfterBurst(burst time.Duration) (moved int, perCycle []int) {
	tm := spareTimings()
	l := NewShareLedger()
	if burst > 0 {
		start := time.Unix(900, 0).Add(-burst - 2*time.Minute)
		for at := start; at.Before(time.Unix(900, 0)); at = at.Add(15 * time.Second) {
			need := 1.0
			if at.After(start) && at.Before(start.Add(15*time.Second+burst)) {
				need = 11
			}
			l.RecordNeeds(map[string]float64{"L": need}, at, tm)
		}
	}
	for _, p := range planEach(l, spareRun(11, 0.4, 7), 2) {
		n := qToL(p.Started)
		moved += n
		perCycle = append(perCycle, n)
	}
	return moved, perCycle
}

// A hard imbalance whose receiver's burst is expected to end before a move
// lands and serves a while moves at the pace, not the whole shortfall at
// once: what it takes all at once lands after the burst, or serves it for
// minutes, and drains its donors for their own next burst. L's last burst
// left it 1.1 move latencies -- the move would land, and serve a tenth of a
// latency. Control: with twice a latency left, with no completed burst on
// record, or once the burst under way has outlasted the last (30 seconds) --
// its history no longer predicts it -- it takes the whole shortfall over two
// cycles, as TestShareHardImbalanceTakesTheWholeShortfall does.
func TestShareBurstEndingFirstMovesAtThePace(t *testing.T) {
	lat := spareTimings().moveLatency()
	const ongoing = 115 * time.Second
	for _, burst := range []time.Duration{lat*11/10 + ongoing} {
		moved, perCycle := lAfterBurst(burst)
		for i, n := range perCycle {
			if n > ShareMaxReplicasPerCycle {
				t.Fatalf("burst %v: cycle %d moved %d to L, whose burst ends first: want at most the pace (%d)",
					burst, i, n, ShareMaxReplicasPerCycle)
			}
		}
		if moved < 1 {
			t.Fatalf("burst %v: L, short and hard-imbalanced, received nothing: %v", burst, perCycle)
		}
	}
	for _, burst := range []time.Duration{2*lat + ongoing, 0, 30 * time.Second} {
		if moved, perCycle := lAfterBurst(burst); moved != 5 {
			t.Fatalf("control, burst %v: Q -> L moved %v over two cycles, want 5", burst, perCycle)
		}
	}
}
