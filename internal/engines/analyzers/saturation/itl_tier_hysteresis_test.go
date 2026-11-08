package saturation

import (
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// A line the tier dwell kept reads held, from either side: what-if prints it
// so, and does not refit a pinned line the window would have fitted in full.
func TestITLSource_ARefusedChangeOfTierReadsHeld(t *testing.T) {
	for name, r := range map[string]itl.Reading{
		"a held slope":      {Tier: itl.TierPinnedB, SlopeHeld: true},
		"a refused entry":   {Tier: itl.TierPinnedB, TierHeld: true},
		"a refused leaving": {Tier: itl.TierPinnedB, TierHeld: true, SlopeHeld: true},
	} {
		if got := itlSource(r); got != itlSourceHeld {
			t.Errorf("%s: source %q", name, got)
		}
	}
	if got := itlSource(itl.Reading{Tier: itl.TierPinnedB}); got != itlSourcePinned {
		t.Errorf("a pinned fit: source %q", got)
	}
}
