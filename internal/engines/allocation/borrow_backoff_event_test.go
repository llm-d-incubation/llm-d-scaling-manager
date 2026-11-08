package allocation

import (
	"testing"
	"time"
)

// Review W4: one reclaim backed the model off twice. Kueue preempted its
// borrowed pod (cooldown 10m), the replacement waited gated for the 5m
// placement timeout, and the release that followed doubled the cooldown to
// 20m. A placement-timeout release while a preemption's cooldown is in force
// is the same event; one after it ended is a repeat, and doubles.
func TestATimeoutReleaseInAPreemptionCooldownIsTheSameEvent(t *testing.T) {
	t0 := time.Unix(90_000, 0)
	g := NewBorrowGovernor(DefaultBorrowSettings())
	until := func() time.Time { return g.backoff["b/m@c"].Until }

	g.MarkPreemptedIn("b/m", []string{"c"}, t0)
	g.markBackoff("b/m", []string{"c"}, t0.Add(5*time.Minute), false)
	if got := until().Sub(t0); got != 10*time.Minute {
		t.Fatalf("a timeout release 5m into the preemption's cooldown left it %v, want 10m: one event", got)
	}

	// Across a restart: the ledger keeps what started the cooldown.
	raw, err := g.MarshalLedger()
	if err != nil {
		t.Fatal(err)
	}
	l, err := UnmarshalBorrowLedger(raw)
	if err != nil {
		t.Fatal(err)
	}
	restarted := NewBorrowGovernor(DefaultBorrowSettings())
	restarted.Restore(l)
	restarted.markBackoff("b/m", []string{"c"}, t0.Add(6*time.Minute), false)
	if got := restarted.backoff["b/m@c"].Until.Sub(t0); got != 10*time.Minute {
		t.Fatalf("after a restart a timeout release in the preemption's cooldown left it %v, want 10m", got)
	}

	// Control: after the cooldown ended, within one more, a release repeats.
	at := t0.Add(12 * time.Minute)
	g.markBackoff("b/m", []string{"c"}, at, false)
	if got := until().Sub(at); got != 20*time.Minute {
		t.Fatalf("a release after the cooldown ended set %v, want the doubled 20m", got)
	}
}
