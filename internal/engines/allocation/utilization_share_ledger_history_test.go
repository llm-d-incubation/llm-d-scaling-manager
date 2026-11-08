package allocation

import (
	"math"
	"testing"
	"time"
)

// recordFor records needs every 30s from start for d, with peak at its middle.
func recordFor(l *ShareLedger, start time.Time, d time.Duration, needs, peak map[string]float64) time.Time {
	tm := spareTimings()
	now := start
	peakAt := (d / 2).Truncate(30 * time.Second)
	for at := time.Duration(0); at <= d; at += 30 * time.Second {
		now = start.Add(at)
		n := needs
		if at == peakAt {
			n = peak
		}
		l.RecordNeeds(n, now, tm)
	}
	return now
}

// A ledger rebuilt from a stored history knows the peaks the old one knew: the
// cap is not forced on for a window after a restart where the peaks fit.
func TestShareNeedHistoryRestoresKnownPeaks(t *testing.T) {
	tm := spareTimings()
	in := spareRun(2, 0.4, 7)
	old := NewShareLedger()
	t0 := time.Unix(1_000_000, 0)
	last := recordFor(old, t0, tm.SwingWindow+5*time.Minute,
		map[string]float64{"L": 2, "Q": 0.4}, map[string]float64{"L": 3, "Q": 0.4})
	h := old.NeedHistory(last, tm)

	restart := last.Add(90 * time.Second)
	if p := PlanShareTransfers(NewShareLedger(), in, restart, tm); !p.HeadroomCapped {
		t.Fatal("setup: a fresh ledger must cap (peaks unknown)")
	}
	l := NewShareLedger()
	if res := l.RestoreNeedHistory(h, restart, tm); res.Roles != 2 || res.KnownNow != 2 || res.KnownAt.After(restart) {
		t.Fatalf("restored %+v, want 2 roles both known by %v", res, restart)
	}
	for _, r := range []string{"L", "Q"} {
		if !l.PeakKnown(r, restart, tm) {
			t.Fatalf("%s: peak not known after restoring a whole window of history", r)
		}
	}
	// The one reading at 3 was never a peak: the restored ledger knows the
	// peak the old one knew.
	if got, want := l.PeakNeed("L", 0), old.PeakNeed("L", 0); got != want {
		t.Fatalf("restored peak of L = %v, the old ledger's %v", got, want)
	}
	if p := PlanShareTransfers(l, in, restart, tm); p.HeadroomCapped {
		t.Fatalf("restored ledger capped on peak claims %d, though the restored peaks fit", p.PeakClaimGPUs)
	}
}

// A restart keeps the live rule that a peak counts only once two readings in a
// row reach it: isolated spikes stay filtered after a restore -- one a minute,
// or one in each of two neighbouring minutes -- and a real two-reading burst
// survives it, inside one minute's bucket or across two, on a minute bucket or
// on a long window's coarser one.
func TestShareNeedHistoryRestoreKeepsTheTwoReadingRule(t *testing.T) {
	cases := []struct {
		name   string
		cycle  time.Duration
		window time.Duration
		// spikes are readings at need 12 over a baseline of 1; every is a
		// spike each such period instead.
		spikes []time.Duration
		every  time.Duration
		want   float64
	}{
		{name: "lone spike every minute, 15s cycle", cycle: 15 * time.Second, window: time.Hour, every: time.Minute, want: 1},
		{name: "lone spike every minute, 30s cycle", cycle: 30 * time.Second, window: time.Hour, every: time.Minute, want: 1},
		{name: "lone spikes in neighbouring minutes", cycle: 30 * time.Second, window: time.Hour,
			spikes: []time.Duration{100 * time.Minute, 101*time.Minute + 30*time.Second}, want: 1},
		{name: "two readings inside one minute", cycle: 15 * time.Second, window: time.Hour,
			spikes: []time.Duration{100*time.Minute + 15*time.Second, 100*time.Minute + 30*time.Second}, want: 12},
		{name: "two readings across two minutes", cycle: 30 * time.Second, window: time.Hour,
			spikes: []time.Duration{100*time.Minute + 30*time.Second, 101 * time.Minute}, want: 12},
		{name: "two readings inside one 2-minute bucket, 6h window", cycle: 30 * time.Second, window: 6 * time.Hour,
			spikes: []time.Duration{400 * time.Minute, 400*time.Minute + 30*time.Second}, want: 12},
		{name: "two readings across two 2-minute buckets, 6h window", cycle: 30 * time.Second, window: 6 * time.Hour,
			spikes: []time.Duration{401*time.Minute + 30*time.Second, 402 * time.Minute}, want: 12},
		{name: "lone spikes in neighbouring 2-minute buckets, 6h window", cycle: 30 * time.Second, window: 6 * time.Hour,
			spikes: []time.Duration{401*time.Minute + 30*time.Second, 402*time.Minute + 30*time.Second}, want: 1},
	}
	t0 := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tm := spareTimings()
			tm.SwingWindow = c.window
			end := c.window + 10*time.Minute
			if n := len(c.spikes); n > 0 {
				end = max(end, c.spikes[n-1]+5*time.Minute)
			}
			l := NewShareLedger()
			var now time.Time
			for at := time.Duration(0); at <= end; at += c.cycle {
				now = t0.Add(at)
				need := 1.0
				if c.every > 0 && at%c.every == 0 {
					need = 12
				}
				for _, s := range c.spikes {
					if s == at {
						need = 12
					}
				}
				l.RecordNeeds(map[string]float64{"A": need}, now, tm)
			}
			live := l.PeakNeed("A", 0)
			if live != c.want {
				t.Fatalf("setup: live peak %v, want %v", live, c.want)
			}
			restart := now.Add(40 * time.Second)
			l2 := NewShareLedger()
			l2.RestoreNeedHistory(l.NeedHistory(now, tm), restart, tm)
			if got := l2.PeakNeed("A", 0); got != live {
				t.Fatalf("restored peak %v, live %v: a restart changed what counts as a peak", got, live)
			}
		})
	}
}

// A stored history keeps one sample per minute: its sustained level, the
// highest of the lower readings of each two in a row ending in it.
func TestShareNeedHistoryKeepsTheSustainedLevelPerMinute(t *testing.T) {
	tm := spareTimings()
	l := NewShareLedger()
	t0 := time.Unix(1_000_020, 0).Truncate(time.Minute)
	for i := range 60 { // 10 minutes at 10s
		need := 1.0
		switch {
		case i%6 == 3: // one lone reading a minute
			need = float64(2 + i/6)
		case i == 4*6+1, i == 4*6+2: // two in a row in minute 4
			need = 7
		}
		l.RecordNeeds(map[string]float64{"L": need}, t0.Add(time.Duration(i)*10*time.Second), tm)
	}
	now := t0.Add(10 * time.Minute)
	got := l.NeedHistory(now, tm)["L"]
	if len(got.Samples) != 10 {
		t.Fatalf("stored %d samples for 10 minutes, want 10", len(got.Samples))
	}
	for i, s := range got.Samples {
		want := 1.0
		if i == 4 {
			want = 7 // two readings in a row at 7; the lone 6 after them is not a level
		}
		if s.Need != want {
			t.Fatalf("minute %d stored %v, want its sustained level %v", i, s.Need, want)
		}
	}
	if !got.Since.Equal(t0) {
		t.Fatalf("since = %v, want %v", got.Since, t0)
	}
}

// A stored sample that is not a usable need, is older than the window, or lies
// in the future is dropped on restore.
func TestShareNeedHistoryDropsInvalidSamples(t *testing.T) {
	tm := spareTimings()
	now := time.Unix(1_000_000, 0)
	h := ShareNeedHistory{"L": {Since: now.Add(-2 * tm.SwingWindow), Samples: []ShareNeedSample{
		{At: now.Add(-tm.SwingWindow - time.Minute), Need: 50},
		{At: now.Add(-3 * time.Minute), Need: math.NaN()},
		{At: now.Add(-2 * time.Minute), Need: math.Inf(1)},
		{At: now.Add(-90 * time.Second), Need: -4},
		{At: now.Add(-time.Minute), Need: 2.5},
		{At: now.Add(10 * time.Minute), Need: 40},
	}}, "gone": {Since: now.Add(-2 * tm.SwingWindow), Samples: []ShareNeedSample{
		{At: now.Add(-tm.SwingWindow - time.Second), Need: 9},
	}}}
	l := NewShareLedger()
	if res := l.RestoreNeedHistory(h, now, tm); res.Roles != 1 {
		t.Fatalf("restored %d roles, want only L", res.Roles)
	}
	if l.PeakKnown("gone", now, tm) || l.PeakNeed("gone", 0) != 0 {
		t.Fatal("a role with only expired samples was restored")
	}
	if got := l.PeakNeed("L", 0); got != 2.5 {
		t.Fatalf("restored peak of L = %v, want its one valid sample 2.5", got)
	}
	if got := l.NeedHistory(now, tm)["L"].Samples; len(got) != 1 || got[0].Need != 2.5 {
		t.Fatalf("stored again: %+v, want the one valid sample", got)
	}
}

// A restored Since must be backed by samples: one recent sample under a Since
// a day back -- written by hand, or kept over a shorter window -- does not make
// a peak known at once. A zero Since is the oldest sample.
func TestShareNeedHistorySinceNeedsSamples(t *testing.T) {
	tm := spareTimings()
	now := time.Unix(2_000_000, 0)
	l := NewShareLedger()
	res := l.RestoreNeedHistory(ShareNeedHistory{
		"forged": {Since: now.Add(-24 * time.Hour), Samples: []ShareNeedSample{{At: now.Add(-30 * time.Second), Need: 0}}},
		"zero":   {Samples: []ShareNeedSample{{At: now.Add(-20 * time.Minute), Need: 1}, {At: now.Add(-time.Minute), Need: 1}}},
	}, now, tm)
	if l.PeakKnown("forged", now, tm) || l.PeakKnown("zero", now, tm) || res.KnownNow != 0 {
		t.Fatalf("a Since the samples do not cover made a peak known: %+v", res)
	}
	if want := now.Add(-20 * time.Minute).Add(tm.SwingWindow); !res.KnownAt.Equal(want) {
		t.Fatalf("peaks known at %v, want the zero-Since role's oldest sample plus a window, %v", res.KnownAt, want)
	}
}

// What a restore reports is what the planner will use: a history 20 minutes
// old restores its roles, but none is known until a window after its start.
func TestShareNeedHistoryRestoreReportsWhenKnown(t *testing.T) {
	tm := simTimings()
	now := time.Unix(200_000, 0)
	for _, age := range []time.Duration{20 * time.Minute, tm.SwingWindow + time.Minute} {
		since := now.Add(-age)
		h := ShareNeedHistory{}
		for _, k := range []string{"A", "B"} {
			var ss []ShareNeedSample
			for at := since; !at.After(now.Add(-time.Minute)); at = at.Add(time.Minute) {
				ss = append(ss, ShareNeedSample{At: at, Need: 2})
			}
			h[k] = ShareRoleNeedHistory{Since: since, Samples: ss}
		}
		res := NewShareLedger().RestoreNeedHistory(h, now, tm)
		wantKnown := 0
		if age > tm.SwingWindow {
			wantKnown = 2
		}
		if res.Roles != 2 || res.KnownNow != wantKnown || !res.KnownAt.Equal(since.Add(tm.SwingWindow)) {
			t.Fatalf("history %v old: %+v, want 2 roles, %d known now, known at %v", age, res, wantKnown, since.Add(tm.SwingWindow))
		}
	}
}

// A history that ends long before now was not observed through the gap: its
// peaks still count, but the role's peak is not known until a window passes.
func TestShareNeedHistoryWithAGapIsNotKnown(t *testing.T) {
	tm := spareTimings()
	now := time.Unix(1_000_000, 0)
	lastSeen := now.Add(-ShareHistoryMaxGap - time.Minute)
	h := ShareNeedHistory{"L": {Since: now.Add(-2 * tm.SwingWindow), Samples: []ShareNeedSample{
		{At: lastSeen.Add(-time.Minute), Need: 6}, {At: lastSeen, Need: 6}}}}
	l := NewShareLedger()
	if res := l.RestoreNeedHistory(h, now, tm); res.Roles != 1 || res.KnownNow != 0 || !res.KnownAt.IsZero() {
		t.Fatalf("restored %+v across a gap, want 1 role whose Since is not restored", res)
	}
	if l.PeakKnown("L", now, tm) {
		t.Fatal("peak taken as known across an unobserved gap")
	}
	if got := l.PeakNeed("L", 0); got != 6 {
		t.Fatalf("peak of L = %v, want the restored 6 kept", got)
	}
	// Recorded from now on, its peak is known a window later: the gap is
	// covered, not left unknown for good.
	at := now
	for ; at.Sub(now) <= tm.SwingWindow; at = at.Add(30 * time.Second) {
		l.RecordNeeds(map[string]float64{"L": 1}, at, tm)
	}
	if !l.PeakKnown("L", at, tm) {
		t.Fatal("a window after a gap, L's peak is still not known")
	}
}

// Flushing forgets every peak: as after a restart with nothing stored.
func TestShareForgetNeedHistory(t *testing.T) {
	tm := spareTimings()
	l := NewShareLedger()
	knownFor(l, time.Unix(1000, 0), "L")
	l.RecordNeeds(map[string]float64{"L": 7}, time.Unix(1000, 0), tm)
	l.ForgetNeedHistory()
	if l.PeakKnown("L", time.Unix(1000, 0), tm) || l.PeakNeed("L", 0) != 0 {
		t.Fatal("history survived ForgetNeedHistory")
	}
}
