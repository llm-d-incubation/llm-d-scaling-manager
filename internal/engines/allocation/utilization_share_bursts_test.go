package allocation

import (
	"math"
	"slices"
	"testing"
	"time"
)

// burstLedger records needs for role, one a minute.
func burstLedger(l *ShareLedger, role string, needs ...float64) *ShareLedger {
	if l == nil {
		l = NewShareLedger()
	}
	tm := simTimings()
	t0 := time.Unix(10000, 0)
	for i, n := range needs {
		l.RecordNeeds(map[string]float64{role: n}, t0.Add(time.Duration(i)*time.Minute), tm)
	}
	return l
}

// One reading -- a glitch, or a tenant's forged metric -- is not the top of
// a role's range: taken as the high, a spike of 100 among 3-minute bursts to
// 9 put the midpoint above every real burst, and the role had none.
func TestShareBurstsIgnoreASingleSpike(t *testing.T) {
	l := burstLedger(nil, "R", 1, 1, 9, 9, 9, 1, 1, 100, 1, 1, 9, 9, 9, 1, 1)
	if !l.LongBursts("R", 3*time.Minute) {
		t.Fatal("one reading of 100 hid two 3-minute bursts to 9")
	}
}

// A burst ends on two readings at or below its midpoint, not one: a single
// dip ended a burst, logged a short one in its place, and started the rest as
// a new burst with a whole last burst still ahead. R's last burst lasted 6
// minutes and the one under way 4, a dip of one reading in it: 2 minutes
// left, so it ends within 3.
func TestShareBurstsAreNotEndedByOneDip(t *testing.T) {
	l := burstLedger(nil, "R", 1, 1, 9, 9, 9, 9, 9, 9, 1, 1, 9, 9, 9, 1, 9)
	if !l.BurstEndsWithin("R", 3*time.Minute) {
		t.Fatal("one dip ended a burst: its last 6-minute burst, 4 minutes in, does not end within 3")
	}
	// Two readings below end it.
	if l := burstLedger(nil, "R", 1, 1, 9, 9, 9, 9, 9, 9, 1, 1, 9, 9, 9, 1, 1); !l.LongBursts("R", 3*time.Minute) {
		t.Fatal("control: two readings below did not end a 3-minute burst")
	}
}

// A dip shorter than join does not split a burst for needBurstsJoined: R
// was busy for 22 minutes with a 2-minute dip in the middle, one burst. A
// gap of 20 minutes between two bursts still counts two.
func TestShareBurstsJoinedAcrossAShortDip(t *testing.T) {
	nines := func(n int) []float64 { return slices.Repeat([]float64{9}, n) }
	ones := func(n int) []float64 { return slices.Repeat([]float64{1}, n) }
	l := burstLedger(nil, "R", slices.Concat(ones(2), nines(10), ones(2), nines(10), ones(2))...)
	if got := l.needBursts("R").completed; got != 2 {
		t.Fatalf("setup: the dip of two readings split the burst into %d, want 2", got)
	}
	if got := l.needBurstsJoined("R", 5*time.Minute).completed; got != 1 {
		t.Errorf("a 2-minute dip split the burst: %d completed with join 5m, want 1", got)
	}
	if got, want := l.needBurstsJoined("R", 0), l.needBursts("R"); got != want {
		t.Errorf("join 0 read %+v, needBursts %+v", got, want)
	}
	l = burstLedger(nil, "R", slices.Concat(ones(2), nines(5), ones(20), nines(5), ones(2))...)
	if got := l.needBurstsJoined("R", 5*time.Minute).completed; got != 2 {
		t.Errorf("bursts 20 minutes apart joined: %d completed with join 5m, want 2", got)
	}
}

// A run under way when the history starts is not a completed burst: its
// start is not known, and taken as the history's -- a long burst whose first
// minutes aged out of the window -- it was counted short, predicting an early
// end and making a long burster look like a fast swinger. Its length is a
// lower bound, so it still counts toward the longest: a burster whose one
// long burst straddles the window's start is still a long burster.
func TestShareBurstsSkipTheRunUnderWayAtTheStart(t *testing.T) {
	if l := burstLedger(nil, "R", 9, 9, 1, 1, 9, 9, 1, 1); l.SwingsFast("R", 3*time.Minute) {
		t.Fatal("a cut 2-minute run and one 2-minute burst were counted two completed bursts: a fast swinger")
	}
	if l := burstLedger(nil, "R", 9, 9, 9, 9, 9, 9, 1, 1, 9, 9); l.BurstEndsWithin("R", 6*time.Minute) {
		t.Fatal("a run under way when the history started predicted the end of the burst after it")
	}
	if l := burstLedger(nil, "R", 9, 9, 9, 9, 1, 1, 9, 9, 1, 1); !l.LongBursts("R", 4*time.Minute) {
		t.Fatal("a run of at least 4 minutes, under way when the history started, did not count toward the longest")
	}
}

// A role's expected end is its last completed burst, not its longest: one
// long burst made every short burst after it look long for a whole window,
// and they took their whole shortfall at once. R burst 10 minutes, then 3;
// 1 minute into the next, it ends within 3.
func TestShareBurstEndsOnTheLastBurst(t *testing.T) {
	l := burstLedger(nil, "R", 1, 1, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 1, 1, 9, 9, 9, 1, 1, 9, 9)
	if !l.BurstEndsWithin("R", 3*time.Minute) {
		t.Fatal("after a 3-minute burst, one a minute in does not end within 3: judged on the 10-minute one")
	}
}

// A burst that has already outlasted the last has no expected end: the
// history does not predict it, and the long burst unpaced moves exist for
// was held at the pace for the rest of it.
func TestShareBurstOutlastingTheLastHasNoExpectedEnd(t *testing.T) {
	l := burstLedger(nil, "R", 1, 1, 9, 9, 9, 1, 1, 9, 9, 9, 9, 9, 9)
	if l.BurstEndsWithin("R", time.Hour) {
		t.Fatal("a burst 5 minutes in, after a 3-minute one, is expected to end")
	}
	// Control: 1 minute in, 2 are left.
	if l := burstLedger(nil, "R", 1, 1, 9, 9, 9, 1, 1, 9, 9); !l.BurstEndsWithin("R", 3*time.Minute) {
		t.Fatal("control: 1 minute into a burst after a 3-minute one, it does not end within 3")
	}
}

// A steady need with noise has no expected end: it crosses its own midpoint
// every few readings, and its "bursts" of two minutes held a steady shortfall
// at the pace. Control: the same shape over a range of half its peak does.
func TestShareSteadyNeedHasNoExpectedEnd(t *testing.T) {
	l := burstLedger(nil, "R", 10, 10.1, 10.1, 9.9, 9.9, 10.1, 10.1, 9.9, 9.9)
	if l.BurstEndsWithin("R", 3*time.Minute) {
		t.Fatal("a need steady at 10 within 0.1 is expected to end its burst")
	}
	if l := burstLedger(nil, "R", 5, 10, 10, 5, 5, 10, 10, 5, 5); !l.BurstEndsWithin("R", 3*time.Minute) {
		t.Fatal("control: 2-minute bursts from 5 to 10 are not expected to end within 3 minutes")
	}
}

// The roles of one model are paced together: prefill's short bursts would
// hold it at the pace while decode, whose burst has no expected end, took
// its whole shortfall at once, and the model ran its burst at a skewed
// ratio. A role of another model does not count, nor one not bursting.
func TestShareModelRolesArePacedTogether(t *testing.T) {
	tm := ShareTimings{MoveLatency: 2 * time.Minute, SwingWindow: time.Hour}
	l := burstLedger(nil, "ns/M/prefill", 1, 1, 9, 9, 9, 1, 1, 9, 9)
	burstLedger(l, "ns/M/decode", 1, 1, 1, 1, 1, 1, 1, 9, 9)
	burstLedger(l, "ns/M/idle", 1, 1, 1, 1, 1, 1, 1, 1, 1)
	origins := map[string]ShareRoleOrigin{
		"ns/M/prefill": {Namespace: "ns", ModelID: "M", Role: "prefill"},
		"ns/M/decode":  {Namespace: "ns", ModelID: "M", Role: "decode"},
		"ns/M/idle":    {Namespace: "ns", ModelID: "M", Role: "idle"},
		"ns/X/both":    {Namespace: "ns", ModelID: "X"},
		"other/M/both": {Namespace: "other", ModelID: "M"},
	}
	model := ShareModelRoles(origins, "ns/M/prefill")
	if want := []string{"ns/M/decode", "ns/M/idle", "ns/M/prefill"}; !slices.Equal(model, want) {
		t.Fatalf("ShareModelRoles = %v, want %v", model, want)
	}
	if !l.ModelBurstEnds("ns/M/prefill", nil, tm) {
		t.Fatal("setup: prefill alone, 1 minute into a 3-minute burst, is not expected to end within 2.5")
	}
	if l.ModelBurstEnds("ns/M/prefill", model, tm) {
		t.Fatal("prefill is paced while decode, of the same model, bursts with no expected end")
	}
	// The planner judges its receivers by the same model.
	for range ShareUnpacedAfterCycles {
		l.ConfirmHard(map[string]bool{"ns/M/prefill": true})
	}
	p := &sharePlanRun{l: l, tm: tm, in: SharePlanInput{Origins: origins}}
	if !p.unpaced("ns/M/prefill") {
		t.Fatal("the planner held prefill at the pace beside decode's burst")
	}
	p.in.Origins = nil
	if p.unpaced("ns/M/prefill") {
		t.Fatal("control: judged alone, prefill's burst ends first and it is unpaced")
	}
}

// A steady need with noise is not a burst with no end: a decode role at 8
// GPUs give or take 0.2, judged as bursting, unpaced its prefill's every
// short burst whenever the noise sat above its midpoint; D itself, with no
// burst to end, is never paced for P's. And the answer is the model's: a
// role between bursts is paced with its partner whose burst is ending, as the
// partner is with it.
func TestShareModelBurstEndsJudgesTheModelAsOne(t *testing.T) {
	tm := ShareTimings{MoveLatency: 2 * time.Minute, SwingWindow: time.Hour}
	l := burstLedger(nil, "P", 1, 1, 9, 9, 9, 1, 1, 9, 9)
	burstLedger(l, "D", 8, 7.6, 7.6, 8.4, 8.4, 7.6, 7.6, 8.4, 8.4)
	if !l.ModelBurstEnds("P", nil, tm) {
		t.Fatal("setup: P alone, in a 3-minute burst, is not expected to end it within 2.5")
	}
	if b := l.needBursts("D"); b.ongoing == 0 || b.spread >= ShareFastSwingSpread {
		t.Fatalf("setup: D must sit above its midpoint with a spread under %v, got %+v", ShareFastSwingSpread, b)
	}
	if !l.ModelBurstEnds("P", []string{"P", "D"}, tm) {
		t.Fatal("D's steady need with noise unpaced P's short burst")
	}
	if l.ModelBurstEnds("D", []string{"P", "D"}, tm) {
		t.Fatal("D, steady and short, is held at the pace for P's burst: its shortfall has no end")
	}
	l = burstLedger(nil, "P", 1, 1, 9, 9, 9, 1, 1, 1, 1)
	burstLedger(l, "D", 1, 9, 9, 9, 1, 1, 1, 9, 9)
	if l.ModelBurstEnds("P", nil, tm) {
		t.Fatal("setup: P alone, between bursts 3 minutes long, is expected to end within 2.5")
	}
	if !l.ModelBurstEnds("D", nil, tm) {
		t.Fatal("setup: D alone, in a 3-minute burst, is not expected to end it within 2.5")
	}
	model := []string{"D", "P"}
	if p, d := l.ModelBurstEnds("P", model, tm), l.ModelBurstEnds("D", model, tm); !p || !d {
		t.Fatalf("one model judged two ways: P paced %v, D paced %v; want both, D's burst ending", p, d)
	}
}

// A long burster whose floor covers its peak waits on no release, so it caps
// no fast swinger on its mean. Control: the same burster with a floor below
// its peak does.
func TestShareFastSwingersNeedABursterAboveItsFloor(t *testing.T) {
	tm := simTimings()
	lat := tm.moveLatency()
	l := NewShareLedger()
	t0 := time.Unix(100000, 0)
	for at := time.Duration(0); at < 90*time.Minute; at += 30 * time.Second {
		a := 2.0
		if int(at.Minutes())/2%2 == 0 {
			a = 6
		}
		b := 0.5
		if at >= 10*time.Minute && at < 10*time.Minute+lat+5*time.Minute {
			b = 8
		}
		l.RecordNeeds(map[string]float64{"A": a, "B": b}, t0.Add(at), tm)
	}
	run := func(bFloor int) map[string]bool {
		p := &sharePlanRun{l: l, tm: tm, now: t0.Add(90 * time.Minute), peaks: map[string]int{"A": 6, "B": 8},
			roles: []ShareRole{{Key: "A", Floor: 1}, {Key: "B", Floor: bFloor}}}
		return p.fastSwingers()
	}
	if got := run(1); !got["A"] {
		t.Fatalf("setup: A, swinging fast beside B's long burst above its floor, is not a fast swinger: %v", got)
	}
	if got := run(8); got["A"] {
		t.Fatal("A capped on its mean for B, whose floor of 8 covers its peak of 8")
	}
}

// A role capped on its mean as a fast swinger is listed, so its model can
// say why it does not get its crest's headroom. Control: beside short
// bursts it is capped on its reading and not listed.
func TestShareMeanCappedRolesAreListed(t *testing.T) {
	if p := fastSwingPlan(true); !slices.Contains(p.MeanCapped, "A") {
		t.Fatalf("A, capped on its mean beside a long burster, is not listed: %v", p.MeanCapped)
	}
	if p := fastSwingPlan(false); len(p.MeanCapped) > 0 {
		t.Fatalf("control: beside short bursts, %v listed as capped on the mean", p.MeanCapped)
	}
}

// The mean is compared on the cap a role ends up with: where the others'
// peaks leave room above both, the mean lowers nothing, and A is not
// fast-swing-capped. Control: with no room, 5 on the mean is below 7 on the
// reading.
func TestShareMeanCapComparedWithTheRoom(t *testing.T) {
	run := func(bPeak int) (onMean, onReading int) {
		p := &sharePlanRun{in: SharePlanInput{Budget: 16},
			byKey: map[string]ShareRole{"A": {Key: "A", Ceiling: 8, ReplicaGPUs: 1}},
			peaks: map[string]int{"A": 6, "B": bPeak}}
		peakSum := 6 + bPeak
		return p.capWithRoom(ShareRole{Key: "A", Ceiling: 5}, peakSum), p.capWithRoom(ShareRole{Key: "A", Ceiling: 7}, peakSum)
	}
	if m, r := run(2); m != r {
		t.Fatalf("room of 14 beside B's peak of 2: capped %d on the mean against %d on the reading, want both 8", m, r)
	}
	if m, r := run(14); m >= r {
		t.Fatalf("control: no room beside B's peak of 14, the mean's %d is not below the reading's %d", m, r)
	}
}

// A restored need is bounded as a live one is: a stored need out of any
// budget's range overflowed MeanNeed to +Inf.
func TestShareRestoredNeedIsBounded(t *testing.T) {
	tm := simTimings()
	now := time.Unix(100000, 0)
	samples := make([]ShareNeedSample, 0, 360)
	for i := range 360 {
		samples = append(samples, ShareNeedSample{At: now.Add(-time.Duration(i) * 10 * time.Second), Need: 1e306})
	}
	l := NewShareLedger()
	l.RestoreNeedHistory(ShareNeedHistory{"R": {Samples: samples}}, now, tm)
	if m := l.MeanNeed("R", 0); math.IsInf(m, 0) || m > MaxShareBudgetGPUs {
		t.Fatalf("MeanNeed over restored needs of 1e306 = %v, want at most %d", m, MaxShareBudgetGPUs)
	}
}
