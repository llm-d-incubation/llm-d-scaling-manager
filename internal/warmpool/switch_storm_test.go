package warmpool

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/warmpool/policy"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/warmpool/pool"
)

// A STANDING intent is not a new switch on every pass.
//
// A switch is decided from what is AWAKE, and the intent only lands once the
// chosen model is actually serving. Until then `awake` still reads empty, so the
// same candidate wins again on the very next pass -- and nothing brakes it,
// because a candidate that needs to scale up preempts MinInterval by design.
// That preemption is argued from there being an awake model to trade away, which
// in this case there is not.
//
// Measured on the shared GPU cluster, 2026-08-30: 263 identical intents in about four minutes,
// all `from ""` to the same variant. The noise was the least of it. The clock
// was re-stamped every pass, so MinInterval never elapsed -- a later switch on
// spare capacity could be deferred indefinitely by an intent that had not
// landed.
//
// The intent must still be RETURNED, though: a model that has not woken is
// exactly what the next pass should go on trying to wake. Only the announcement
// and the clock are suppressed.
func TestAStandingIntentDoesNotRestampTheSwitchClock(t *testing.T) {
	cfg := testConfig()
	cfg.Retained = true

	// Nothing is Serving, so awakeVariantIn returns "" every pass -- the state
	// the pool is in for as long as the wake has not completed.
	memberships := []pool.Membership{{Pod: podA(), State: pool.Asleep, Pool: "retained"}}
	variants := []policy.VariantDemand{
		{Model: pool.ModelRef{Namespace: "tenant", Variant: "urgent-one"}},
	}
	spec := PoolSpec{
		Name:   "retained",
		Config: cfg,
		Switch: SwitchConfig{SpareThreshold: 0.2, MinInterval: 10 * time.Minute},
	}

	r := New(&fakePool{}, &staticDemand{}, cfg)
	r.Pressure = func(_, variant string) (decision.Pressure, bool) {
		// Needs to scale up, which is the urgency that preempts the floor.
		return decision.Pressure{SpareFraction: 0.01, NeedsScaleUp: true}, true
	}

	clock := time.Now()
	r.now = func() time.Time { return clock }

	ctx := context.Background()

	if got := r.awakeIntent(ctx, spec, memberships, variants); got != "urgent-one" {
		t.Fatalf("first pass = %q, want the pool to choose urgent-one", got)
	}
	firstStamp := r.lastSwitchAt[spec.Name]
	if firstStamp.IsZero() {
		t.Fatal("a first switch must stamp the clock, or MinInterval measures from nothing")
	}

	// Later passes, with nothing changed: the model still has not woken.
	for i := 0; i < 5; i++ {
		clock = clock.Add(5 * time.Second)
		if got := r.awakeIntent(ctx, spec, memberships, variants); got != "urgent-one" {
			t.Fatalf("pass %d = %q, want the standing intent to be kept so the pool goes on "+
				"trying to wake the model", i+2, got)
		}
	}

	if got := r.lastSwitchAt[spec.Name]; !got.Equal(firstStamp) {
		t.Errorf("the switch clock was re-stamped by a standing intent (%v -> %v); "+
			"MinInterval then never elapses and a later spare-based switch is deferred forever",
			firstStamp, got)
	}
}

// A genuinely DIFFERENT candidate is still a new switch, and still preempts.
//
// The brake above must not become a lock: the whole point of the preemption is
// that a model which has just become urgent does not wait out the floor. Only a
// restatement of the same target is suppressed.
func TestANewCandidateIsStillANewSwitch(t *testing.T) {
	cfg := testConfig()
	cfg.Retained = true

	memberships := []pool.Membership{{Pod: podA(), State: pool.Asleep, Pool: "retained"}}
	spec := PoolSpec{
		Name:   "retained",
		Config: cfg,
		Switch: SwitchConfig{SpareThreshold: 0.2, MinInterval: 10 * time.Minute},
	}

	r := New(&fakePool{}, &staticDemand{}, cfg)
	r.Pressure = func(_, variant string) (decision.Pressure, bool) {
		return decision.Pressure{SpareFraction: 0.01, NeedsScaleUp: true}, true
	}
	clock := time.Now()
	r.now = func() time.Time { return clock }
	ctx := context.Background()

	first := []policy.VariantDemand{
		{Model: pool.ModelRef{Namespace: "tenant", Variant: "first-one"}},
	}
	if got := r.awakeIntent(ctx, spec, memberships, first); got != "first-one" {
		t.Fatalf("first pass = %q, want first-one", got)
	}
	firstStamp := r.lastSwitchAt[spec.Name]

	// A different model becomes the candidate, well inside MinInterval.
	clock = clock.Add(5 * time.Second)
	second := []policy.VariantDemand{
		{Model: pool.ModelRef{Namespace: "tenant", Variant: "second-one"}},
	}
	if got := r.awakeIntent(ctx, spec, memberships, second); got != "second-one" {
		t.Fatalf("second pass = %q, want the new candidate to preempt the floor", got)
	}
	if got := r.lastSwitchAt[spec.Name]; !got.After(firstStamp) {
		t.Error("a new candidate is a new switch and must re-stamp the clock")
	}
}
