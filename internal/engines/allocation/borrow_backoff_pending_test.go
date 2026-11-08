package allocation

import (
	"testing"
	"time"
)

// Review N-m4: W4 compared a placement-timeout release against the
// preemption's cooldown alone. While a scale-up is pending the timeout is
// tripled, 15m: the release after one preemption lands past the 10m cooldown,
// and doubled it to 20m. It is the same event while it comes within the
// preemption plus the effective timeout (and one base timeout for the
// replacement pod to appear): the release starts the base cooldown, not a
// doubled one. Control: a release well past that window is a repeat.
func TestATripledTimeoutReleaseAfterAPreemptionIsTheSameEvent(t *testing.T) {
	t0 := time.Unix(90_000, 0)
	settings := DefaultBorrowSettings()
	g := NewBorrowGovernor(settings)
	g.MarkPreemptedIn("b/m", []string{"c"}, t0)
	tripled := 3 * settings.PlacementTimeout
	at := t0.Add(tripled + 30*time.Second)
	g.markBackoffAfter("b/m", []string{"c"}, at, false, tripled)
	e := g.backoff["b/m@c"]
	if e.Level != 0 || e.Until.Sub(at) != settings.Cooldown {
		t.Fatalf("the release %v after the preemption set level %d until +%v, want level 0 and the base %v",
			at.Sub(t0), e.Level, e.Until.Sub(at), settings.Cooldown)
	}

	// Control: a release past the window, within one cooldown of the last,
	// repeats and doubles.
	h := NewBorrowGovernor(settings)
	h.MarkPreemptedIn("b/m", []string{"c"}, t0)
	late := t0.Add(settings.Cooldown + time.Minute)
	h.markBackoffAfter("b/m", []string{"c"}, late, false, settings.PlacementTimeout)
	if got := h.backoff["b/m@c"].Level; got != 1 {
		t.Fatalf("a release outside the window set level %d, want 1: a repeat", got)
	}
}
