package decision

import (
	"testing"
	"time"
)

const (
	actNS    = "chat"
	actModel = "meta/llama-3-8b"
)

// newTestActivations returns a registry whose clock the caller drives.
func newTestActivations(now *time.Time) *Activations {
	a := NewActivations()
	a.now = func() time.Time { return *now }
	return a
}

// TestWithinRetentionHoldsThenLapses is the whole point of the registry: a model
// woken from zero is protected for the retention period and no longer after it.
func TestWithinRetentionHoldsThenLapses(t *testing.T) {
	now := time.Now()
	a := newTestActivations(&now)
	const retention = 10 * time.Minute

	if a.WithinRetention(actNS, actModel, retention) {
		t.Fatal("a model that was never woken must not be held")
	}

	a.Mark(actNS, actModel)
	if !a.WithinRetention(actNS, actModel, retention) {
		t.Fatal("a model woken just now must be held")
	}

	now = now.Add(retention - time.Second)
	if !a.WithinRetention(actNS, actModel, retention) {
		t.Fatal("a model must still be held one second before its retention lapses")
	}

	now = now.Add(2 * time.Second)
	if a.WithinRetention(actNS, actModel, retention) {
		t.Fatal("the hold must lapse once the retention period has passed, " +
			"handing the model back to normal idle accounting")
	}
}

// TestMarkExtendsTheHold: the engine re-publishes every poll while requests are
// still queued, and each of those is fresh evidence of demand.
func TestMarkExtendsTheHold(t *testing.T) {
	now := time.Now()
	a := newTestActivations(&now)
	const retention = time.Minute

	a.Mark(actNS, actModel)
	now = now.Add(50 * time.Second)
	a.Mark(actNS, actModel) // still queued: demand is current
	now = now.Add(50 * time.Second)

	if !a.WithinRetention(actNS, actModel, retention) {
		t.Fatal("re-marking must extend the hold; 50s after the second mark is inside a 1m retention")
	}
}

// TestRetentionIsPerModelAndNamespace guards the key: holding one model must not
// hold an unrelated one, in this namespace or another.
func TestRetentionIsPerModelAndNamespace(t *testing.T) {
	now := time.Now()
	a := newTestActivations(&now)
	const retention = time.Minute

	a.Mark(actNS, actModel)

	if a.WithinRetention(actNS, "other/model", retention) {
		t.Fatal("a different model in the same namespace must not be held")
	}
	if a.WithinRetention("other-ns", actModel, retention) {
		t.Fatal("the same model in a different namespace must not be held")
	}
}

// TestClearReleasesTheHold covers handing a genuinely-serving model back to
// normal accounting before its retention would otherwise lapse.
func TestClearReleasesTheHold(t *testing.T) {
	now := time.Now()
	a := newTestActivations(&now)
	const retention = time.Minute

	a.Mark(actNS, actModel)
	a.Clear(actNS, actModel)

	if a.WithinRetention(actNS, actModel, retention) {
		t.Fatal("Clear must release the hold")
	}
}

// TestNonPositiveRetentionDisablesTheHold: an operator who configures no
// retention gets no hold, rather than an accidental default.
func TestNonPositiveRetentionDisablesTheHold(t *testing.T) {
	now := time.Now()
	a := newTestActivations(&now)

	a.Mark(actNS, actModel)

	for _, retention := range []time.Duration{0, -time.Minute} {
		if a.WithinRetention(actNS, actModel, retention) {
			t.Fatalf("retention %s must disable the hold", retention)
		}
	}

	// Disabling the hold must not throw the record away: retention is config and
	// can be raised again, and this path is not evidence the wake is over.
	if len(a.m) != 1 {
		t.Fatalf("a disabled hold must keep its entry, have %d", len(a.m))
	}
}

// TestLapsedHoldsArePruned: nothing else prunes the registry, so a read that
// finds a hold has lapsed is what collects it. Without this an entry survives
// for the life of the process, one per model ever woken.
func TestLapsedHoldsArePruned(t *testing.T) {
	now := time.Now()
	a := newTestActivations(&now)
	const retention = time.Minute

	a.Mark(actNS, actModel)
	a.Mark("other-ns", actModel)
	if len(a.m) != 2 {
		t.Fatalf("expected both wakes recorded, have %d", len(a.m))
	}

	now = now.Add(2 * retention)
	if a.WithinRetention(actNS, actModel, retention) {
		t.Fatal("the hold must have lapsed")
	}

	if _, ok := a.m[activationKey(actNS, actModel)]; ok {
		t.Fatal("reading a lapsed hold must drop it")
	}
	// Only the entry that was read is collected; the other model's hold is
	// untouched, since its own retention is not this caller's to judge.
	if _, ok := a.m[activationKey("other-ns", actModel)]; !ok {
		t.Fatal("an unread hold must survive another model's pruning")
	}
}

// A wake's duration is measured from the FIRST publish of an episode, not the
// last. The engine re-publishes every poll while the queue drains, so measuring
// from the most recent Mark would report one poll interval for every wake --
// the metric would look perfect and mean nothing.
func TestActivations_CompleteWake_MeasuresFromFirstMark(t *testing.T) {
	now := time.Unix(1000, 0)
	a := NewActivations()
	a.now = func() time.Time { return now }

	a.Mark("ns", "m")
	now = now.Add(10 * time.Second)
	a.Mark("ns", "m") // re-publish while the request is still queued
	now = now.Add(5 * time.Second)

	took, ok := a.CompleteWake("ns", "m")
	if !ok {
		t.Fatal("no wake episode was open")
	}
	if took != 15*time.Second {
		t.Errorf("wake measured %v, want 15s (from the first Mark, not the second)", took)
	}
}

// One wake yields one observation. The model stays serving for many polls after
// it comes up, and an episode that did not close would contribute a sample on
// every one of them, dragging the distribution toward the steady state.
func TestActivations_CompleteWake_OnlyOnce(t *testing.T) {
	a := NewActivations()
	a.Mark("ns", "m")
	if _, ok := a.CompleteWake("ns", "m"); !ok {
		t.Fatal("first CompleteWake reported no episode")
	}
	if took, ok := a.CompleteWake("ns", "m"); ok {
		t.Errorf("second CompleteWake reported a duration of %v; the episode should be closed", took)
	}
}

// A model that never woke has nothing to report. Guards the case where the
// engine sees an active model it did not wake -- every ordinary serving model,
// on every poll.
func TestActivations_CompleteWake_NeverWoken(t *testing.T) {
	a := NewActivations()
	if took, ok := a.CompleteWake("ns", "never-woken"); ok {
		t.Errorf("reported a %v wake for a model that was never activated", took)
	}
}

// Clear ends an episode without recording it. A wake abandoned before the model
// served is not a slow wake; counting it would put a false tail on the
// histogram exactly where a warm-pool regression is read.
func TestActivations_Clear_DropsTheEpisode(t *testing.T) {
	a := NewActivations()
	a.Mark("ns", "m")
	a.Clear("ns", "m")
	if took, ok := a.CompleteWake("ns", "m"); ok {
		t.Errorf("a cleared episode still reported %v", took)
	}
}
