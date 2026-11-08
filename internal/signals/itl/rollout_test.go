package itl

import (
	"fmt"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
)

// A key's line across a rollout (rollout.go): two engines, A and B each their
// own, and readings at a wide spread, so a full fit is had in a few cycles.
const (
	oldA, oldB = 0.05, 0.0100
	newA, newB = 0.03, 0.0270
)

var rollStart = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

// rollRows is n pods of revision rev, Ready since ready, on line (a, b).
func rollRows(rev string, n int, ready time.Time, a, b float64) []domain.ReplicaMetrics {
	rows := make([]domain.ReplicaMetrics, 0, n)
	for i := range n {
		k := 0.25 + 0.5*float64(i)/float64(max(n-1, 1))
		rows = append(rows, domain.ReplicaMetrics{PodName: fmt.Sprintf("%s-%d", rev, i), VariantName: "v", Ready: true,
			ReadySince: ready, Revision: rev, KvUsageInstant: k, AvgITL: a*k + b})
	}
	return rows
}

// rollRig is a key at the full fit on old pods, with B learned.
type rollRig struct {
	t   *testing.T
	tr  *Tracker
	adm Admission
	at  time.Time
}

func newRollRig(t *testing.T) *rollRig {
	t.Helper()
	r := &rollRig{t: t, tr: NewTracker(), adm: Admission{TierHysteresis: true}, at: rollStart}
	for range 3 {
		r.cycle(rollRows("old", 6, rollStart.Add(-time.Hour), oldA, oldB), false)
	}
	if b, ok := r.tr.Baseline("k"); !ok || b < oldB-1e-9 || b > oldB+1e-9 {
		t.Fatalf("premise: B = %v (%v), want %v", b, ok, oldB)
	}
	return r
}

// cycle takes rows through RolloutRows and Observe, 15 s apart.
func (r *rollRig) cycle(rows []domain.ReplicaMetrics, rolling bool) (Reading, RolloutSight) {
	r.t.Helper()
	s := r.tr.RolloutRows("k", "v", rows, rolling)
	got := r.tr.Observe("k", "v", s.Rows, r.adm, r.at)
	r.at = r.at.Add(15 * time.Second)
	return got, s
}

// While only old pods serve, nothing is offered and the line stands.
func TestRollout_NoNewPodHoldsTheLine(t *testing.T) {
	r := newRollRig(t)
	got, s := r.cycle(rollRows("old", 6, rollStart.Add(-time.Hour), oldA, oldB), true)
	if !s.Held || s.Moved || len(s.Rows) != 0 {
		t.Fatalf("sight %+v", s)
	}
	if got.Tier == "" {
		t.Fatal("no line while the old pods serve")
	}
}

// A status that trips with no change of revision clears nothing: a plain scale.
func TestRollout_APlainScaleClearsNothing(t *testing.T) {
	r := newRollRig(t)
	held := r.tr.WindowLen("k")
	rows := rollRows("old", 6, rollStart.Add(-time.Hour), oldA, oldB)
	r.cycle(rows, true)
	if _, s := r.cycle(rows, false); s.Moved {
		t.Fatal("no revision changed")
	}
	if got := r.tr.WindowLen("k"); got < held {
		t.Fatalf("window %d, was %d", got, held)
	}
	if !r.tr.Readings("k").OLSLatched {
		t.Fatal("a plain scale reset the latch")
	}
}

// The new pods' readings alone reach the line, however few, and B stands until
// they earn one of their own. The case of 1 new pod among 5 old, then 3: fitted
// over both engines B was .0027 with one, and about 47 % too much capacity
// with three.
func TestRollout_AFitNeverSpansTwoEngines(t *testing.T) {
	for _, newPods := range []int{1, 3} {
		t.Run(fmt.Sprintf("%d new pods", newPods), func(t *testing.T) {
			r := newRollRig(t)
			mixed := func() []domain.ReplicaMetrics {
				return append(rollRows("old", 5, rollStart.Add(-time.Hour), oldA, oldB),
					rollRows("new", newPods, rollStart, newA, newB)...)
			}
			got, s := r.cycle(mixed(), true)
			if !s.Moved || len(s.Rows) != newPods {
				t.Fatalf("first cycle: sight %+v", s)
			}
			if got.Tier != TierPinnedB || !got.SlopeHeld {
				t.Fatalf("first cycle: tier %q held %v, want the slope held", got.Tier, got.SlopeHeld)
			}
			if r.tr.Readings("k").OLSLatched {
				t.Fatal("the latch stands on old pods' readings")
			}
			for i := 0; i < DefaultMinSamples && r.tr.WindowLen("k") < DefaultMinSamples-newPods; i++ {
				if got, _ = r.cycle(mixed(), true); !got.SlopeHeld {
					t.Fatalf("cycle %d: not held at %d readings", i, r.tr.WindowLen("k"))
				}
				if b, _ := r.tr.Baseline("k"); b != oldB && (b < oldB-1e-9 || b > oldB+1e-9) {
					t.Fatalf("cycle %d: B moved to %v on too few readings", i, b)
				}
			}
			if got := r.tr.WindowLen("k"); got < DefaultMinSamples-newPods {
				t.Fatalf("the new pods' readings did not reach the window: %d", got)
			}
			for _, rm := range r.tr.Readings("k").All {
				if rm.ITLSec > newA*rm.K+newB+1e-9 || rm.ITLSec < newA*rm.K+newB-1e-9 {
					t.Fatalf("an old pod's reading in the window: %+v", rm)
				}
			}
		})
	}
}

// A new pod that flaps moves nothing: while it is not Ready the old revision is
// the only one Ready, and the line holds, offered neither its rows nor a clear.
// Its readings build up across the flap, so a crash-looping rollout reaches
// 10 readings. Off the flap, a rollback that finishes still moves the key.
func TestRollout_AFlappingNewPodMovesNothing(t *testing.T) {
	r := newRollRig(t)
	early := rollStart.Add(-time.Hour)
	old := func() []domain.ReplicaMetrics { return rollRows("old", 5, early, oldA, oldB) }
	newPods := func(ready bool) []domain.ReplicaMetrics {
		rows := rollRows("new", 2, rollStart, newA, newB)
		for i := range rows {
			rows[i].Ready = ready
		}
		return rows
	}
	if _, s := r.cycle(append(old(), newPods(true)...), true); !s.Moved {
		t.Fatal("premise: the first new rows move the key")
	}
	want := 2
	for flap := range 8 {
		_, s := r.cycle(append(old(), newPods(false)...), true)
		if !s.Held || s.Moved || len(s.Rows) != 0 {
			t.Fatalf("flap %d, new pods down: sight %+v", flap, s)
		}
		if got := r.tr.WindowLen("k"); got != want {
			t.Fatalf("flap %d: the window holds %d, want %d", flap, got, want)
		}
		_, s = r.cycle(append(old(), newPods(true)...), true)
		if s.Held || s.Moved || len(s.Rows) != 2 || s.Rows[0].Revision != "new" {
			t.Fatalf("flap %d, new pods back: sight %+v", flap, s)
		}
		want += 2
		if got := r.tr.WindowLen("k"); got != want {
			t.Fatalf("flap %d: the window holds %d, want %d", flap, got, want)
		}
	}
	if got := r.tr.WindowLen("k"); got < DefaultMinSamples {
		t.Fatalf("the flapping rollout reached %d readings", got)
	}
	for _, o := range r.tr.Readings("k").All {
		if o.ITLSec > newA*o.K+newB+1e-9 || o.ITLSec < newA*o.K+newB-1e-9 {
			t.Fatalf("an old pod's reading in the window: %+v", o)
		}
	}
	if _, s := r.cycle(old(), false); !s.Moved {
		t.Fatal("a rollback that finished did not move the key")
	}
}

// A second rollout on top of the first is no flap: a newer revision Ready beside
// the others moves the key on.
func TestRollout_ASecondRolloutMovesTheKeyOn(t *testing.T) {
	r := newRollRig(t)
	early := rollStart.Add(-time.Hour)
	r.cycle(append(rollRows("old", 5, early, oldA, oldB), rollRows("new", 2, rollStart, newA, newB)...), true)
	third := rollRows("third", 2, rollStart.Add(time.Minute), newA, newB)
	_, s := r.cycle(append(rollRows("new", 2, rollStart, newA, newB), third...), true)
	if !s.Moved || len(s.Rows) != 2 || s.Rows[0].Revision != "third" {
		t.Fatalf("sight %+v", s)
	}
}

// With three new pods the window fills, and past the dwell the fit is theirs
// alone: the new engine's B, not a blend.
func TestRollout_TheFullFitIsTheNewEngines(t *testing.T) {
	r := newRollRig(t)
	for range int(DefaultTierDwell/(15*time.Second)) + 8 {
		r.cycle(append(rollRows("old", 5, rollStart.Add(-time.Hour), oldA, oldB), rollRows("new", 3, rollStart, newA, newB)...), true)
	}
	b, _ := r.tr.Baseline("k")
	if b < newB-1e-9 || b > newB+1e-9 {
		t.Fatalf("B = %v, want the new engine's %v, not a blend", b, newB)
	}
}

// A window that keeps its readings over idle replicas refits on every cycle
// and stamps the held slope anew, so it never ages. Cleared, there is nothing
// to refit: the fit time stays, and the hold ends on time.
func TestRollout_AnIdleFleetDoesNotRestampTheSlope(t *testing.T) {
	r := newRollRig(t)
	fitted := r.at.Add(-15 * time.Second)
	idle := rollRows("new", 3, rollStart, newA, newB)
	for i := range idle {
		idle[i].KvUsageInstant, idle[i].AvgITL = 0, 0
	}
	var got Reading
	for range 5 {
		got, _ = r.cycle(idle, true)
	}
	if !got.SlopeHeld || !got.SlopeFittedAt.Equal(fitted) {
		t.Fatalf("held %v fitted %v, want the slope of %v", got.SlopeHeld, got.SlopeFittedAt, fitted)
	}
}

// A revision that changes between two cycles with no status to say so moves
// the key all the same, and a key's first revision is no change.
func TestRollout_ARevisionChangeWithNoStatus(t *testing.T) {
	r := newRollRig(t)
	if _, s := r.cycle(rollRows("new", 6, rollStart, newA, newB), false); !s.Moved {
		t.Fatal("a new revision did not move the key")
	}
	if got := r.tr.WindowLen("k"); got != 6 {
		t.Fatalf("window %d, want the new pods' 6", got)
	}
	if _, s := newRollRigCold(t).cycle(rollRows("old", 6, rollStart, oldA, oldB), false); s.Moved {
		t.Fatal("a first revision moved the key")
	}
}

func newRollRigCold(t *testing.T) *rollRig {
	return &rollRig{t: t, tr: NewTracker(), at: rollStart}
}

// With no memory, one revision in a rollout may be either; two can be ordered.
// No revision at all, or two with no Ready time, cannot.
func TestRollout_WhatCannotBeTold(t *testing.T) {
	early := rollStart.Add(-time.Hour)
	for name, rows := range map[string][]domain.ReplicaMetrics{
		"one revision": rollRows("a", 3, early, oldA, oldB),
		"no revision":  rollRows("", 3, early, oldA, oldB),
		"no order": append(rollRows("a", 2, time.Time{}, oldA, oldB),
			rollRows("b", 2, time.Time{}, oldA, oldB)...),
	} {
		if s := newRollRigCold(t).tr.RolloutRows("k", "v", rows, true); !s.Held {
			t.Errorf("%s: %+v, want held", name, s)
		}
	}
	both := append(rollRows("a", 2, early, oldA, oldB), rollRows("b", 2, rollStart, oldA, oldB)...)
	if s := newRollRigCold(t).tr.RolloutRows("k", "v", both, true); s.Held || len(s.Rows) != 2 || s.Rows[0].Revision != "b" {
		t.Errorf("two revisions: %+v", s)
	}
}

// An old pod restarted in a stalled rollout is younger than the new pods, and
// does not make its revision the new one: the oldest Ready pod of each decides.
func TestRollout_ARestartedOldPodDoesNotMoveTheKeyBack(t *testing.T) {
	r := newRollRig(t)
	early := rollStart.Add(-time.Hour)
	r.cycle(append(rollRows("old", 5, early, oldA, oldB), rollRows("new", 3, rollStart, newA, newB)...), true)
	old := rollRows("old", 5, early, oldA, oldB)
	old[0].ReadySince = rollStart.Add(time.Hour)
	if _, s := r.cycle(append(old, rollRows("new", 3, rollStart, newA, newB)...), true); s.Moved || len(s.Rows) != 3 || s.Rows[0].Revision != "new" {
		t.Fatalf("sight %+v", s)
	}
}

// The revision outlives a forget and goes with the variant.
func TestRollout_TheRevisionGoesWithTheVariant(t *testing.T) {
	r := newRollRig(t)
	match := func(k string) bool { return k == "k" }
	r.tr.ForgetKeys(r.at, learn.ByOperator, match)
	if _, ok := r.tr.revisions["k"]; !ok {
		t.Fatal("a forget took the revision")
	}
	r.tr.ForgetKeys(r.at, learn.ByEvicted, match)
	if _, ok := r.tr.revisions["k"]; ok {
		t.Fatal("an eviction left the revision")
	}
	r = newRollRig(t)
	r.tr.Sweep(r.at, match)
	if _, ok := r.tr.revisions["k"]; ok {
		t.Fatal("a sweep left the revision")
	}
}
