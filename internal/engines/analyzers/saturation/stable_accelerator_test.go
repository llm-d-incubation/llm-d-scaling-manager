package saturation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// StableAcceleratorFor exists so the engine-configuration metric can report the
// accelerator the learned-state key is actually built with. Everything here is
// about one property: it answers the same question stableAccelerator answers,
// and WRITES NOTHING while doing it.
//
// That property is the whole reason the accessor exists rather than a second
// caller of stableAccelerator. The memo's lastUsed is the eviction clock
// EvictStaleHistory reads, so a reader that touched it would keep the memo alive
// for as long as the metric publishes -- which is every cycle, for every
// variant, forever. The metric would quietly pin the map it describes.

func memoSeeded(t *testing.T, key, name string, age time.Duration) (*SaturationAnalyzer, time.Time) {
	t.Helper()
	a := NewSaturationAnalyzer(capacity.NewStore())
	stamp := time.Now().Add(-age)
	a.lastAccelerator[key] = acceleratorMemo{name: name, lastUsed: stamp}
	return a, stamp
}

func TestStableAcceleratorFor_ResolvedReadingWins(t *testing.T) {
	// A resolved reading is the truth and needs no memo at all, so this must not
	// even look at the map -- it is the common case, on every cycle.
	a, stamp := memoSeeded(t, "ns/v1", "NVIDIA-H100", time.Hour)

	assert.Equal(t, "NVIDIA-H200", a.StableAcceleratorFor("ns", "v1", "NVIDIA-H200"))
	assert.Equal(t, "NVIDIA-H100", a.lastAccelerator["ns/v1"].name,
		"a reader overwrote the memo; only the analyzer may record a reading")
	assert.Equal(t, stamp, a.lastAccelerator["ns/v1"].lastUsed,
		"a reader advanced the eviction clock")
}

func TestStableAcceleratorFor_UnresolvedFallsBackToTheMemo(t *testing.T) {
	// The case the whole thing is for: the pods disagree, the key still says
	// H200 because that is what last resolved, and so must the metric.
	for _, unresolved := range []string{
		"", constants.DefaultAcceleratorName, constants.UnresolvedAcceleratorType,
	} {
		a, _ := memoSeeded(t, "ns/v1", "NVIDIA-H200", time.Hour)
		assert.Equal(t, "NVIDIA-H200", a.StableAcceleratorFor("ns", "v1", unresolved),
			"unresolved reading %q did not fall back to the remembered accelerator", unresolved)
	}
}

// TestStableAcceleratorFor_DoesNotTouchTheEvictionClock is the spec that
// separates this from stableAccelerator, which deliberately touches lastUsed on
// exactly this path. Make this one call stableAccelerator instead and it fails;
// nothing else here would notice.
func TestStableAcceleratorFor_DoesNotTouchTheEvictionClock(t *testing.T) {
	a, stamp := memoSeeded(t, "ns/v1", "NVIDIA-H200", 48*time.Hour)

	for range 5 {
		require.Equal(t, "NVIDIA-H200", a.StableAcceleratorFor("ns", "v1", ""))
	}
	assert.Equal(t, stamp, a.lastAccelerator["ns/v1"].lastUsed,
		"reading through the metric path refreshed the memo, so publishing the "+
			"metric would keep every variant that ever existed un-evictable")

	// And the sweep must still be able to take it, which is the consequence that
	// matters: a memo the reader had refreshed would survive this.
	horizonsForTest(a, 0)
	a.EvictStaleHistory()
	assert.Empty(t, a.lastAccelerator,
		"the memo outlived a zero horizon, so something refreshed it")
}

func TestStableAcceleratorFor_NothingRememberedReturnsTheReadingAndRecordsNothing(t *testing.T) {
	// A never-resolvable variant keeps behaving exactly as it does today, and an
	// absent memo stays absent: a read must not populate the map it reports on.
	a := NewSaturationAnalyzer(capacity.NewStore())

	assert.Equal(t, constants.UnresolvedAcceleratorType,
		a.StableAcceleratorFor("ns", "never", constants.UnresolvedAcceleratorType))
	assert.Empty(t, a.lastAccelerator,
		"a read created a memo; the metric would then invent per-variant state")
}

// TestStableAcceleratorFor_AgreesWithTheKeyItExplains is the claim the metric
// rests on. These are two separate functions with two separate bodies, so the
// only thing that keeps them answering the same question is a spec that asks
// both -- which is exactly what was missing when the publish site was handed the
// raw reading instead.
func TestStableAcceleratorFor_AgreesWithTheKeyItExplains(t *testing.T) {
	readings := []string{
		"NVIDIA-H200", "NVIDIA-H100", "AMD-MI300X",
		"", constants.DefaultAcceleratorName, constants.UnresolvedAcceleratorType,
	}
	for _, seeded := range []string{"", "NVIDIA-H200"} {
		for _, reading := range readings {
			a := NewSaturationAnalyzer(capacity.NewStore())
			b := NewSaturationAnalyzer(capacity.NewStore())
			if seeded != "" {
				a.lastAccelerator["ns/v1"] = acceleratorMemo{name: seeded, lastUsed: time.Now()}
				b.lastAccelerator["ns/v1"] = acceleratorMemo{name: seeded, lastUsed: time.Now()}
			}
			assert.Equal(t,
				b.stableAccelerator("ns", "v1", reading),
				a.StableAcceleratorFor("ns", "v1", reading),
				"the metric and the key disagree for seeded=%q reading=%q", seeded, reading)
		}
	}
}
