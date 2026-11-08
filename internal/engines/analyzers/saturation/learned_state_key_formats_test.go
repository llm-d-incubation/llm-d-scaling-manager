package saturation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// The keys of the learned-state document, as v0.10.1 wrote them: one sample
// per map, taken from the back-compat fixtures
// (steadystate/testdata/learned-state). A restored document is found again
// only under the keys today's builders make, so a builder that changes its
// format strands every document written before it. Changing one fails here;
// a deliberate change needs a migration of the stored keys, not an edit of
// this table.
func TestLearnedStateKeyFormats_AreV0101s(t *testing.T) {
	now := time.Date(2026, 3, 2, 9, 11, 0, 0, time.UTC)
	a := NewSaturationAnalyzer(capacity.NewStore()).WithClock(clocktesting.NewFakePassiveClock(now))
	const (
		ns, model, variant, accel = "ns-01", "model-01", "variant-01", "NVIDIA-H100-80GB-HBM3"
		prevArgs                  = "5e1d0c3b9a7f2468"
	)
	medium := float64(ShortOutputThreshold) // the first length in "medium"
	long := float64(MediumOutputThreshold)  // the first length in "long"
	require.Equal(t, "medium", classifyOutputLength(medium), "precondition")
	require.Equal(t, "long", classifyInputLength(long), "precondition")

	k2 := a.historyKey(model, ns, variant, accel, 1, "decode", medium, 5)
	throughput := a.throughputKey(model, ns, variant, accel, 1, "decode", long, medium, 5)
	itl := a.itlWindowKey(ns, model, variant, accel, 1)

	for _, tc := range []struct{ maps, got, want string }{
		// The k2 window key, and every map keyed like it.
		{"k2, k2Args, lowerBounds, priorK2, k2BorrowedSince", k2,
			"ns-01|variant-01|model-01|NVIDIA-H100-80GB-HBM3|1|decode|medium|q5"},
		{"k2Stash", stashKey(k2, prevArgs),
			"ns-01|variant-01|model-01|NVIDIA-H100-80GB-HBM3|1|decode|medium|q5#5e1d0c3b9a7f2468"},
		// The throughput window key.
		{"throughput, throughputArgs", throughput,
			"ns-01|variant-01|model-01|NVIDIA-H100-80GB-HBM3|1|decode|ilong|medium|q5"},
		{"throughputStash", stashKey(throughput, prevArgs),
			"ns-01|variant-01|model-01|NVIDIA-H100-80GB-HBM3|1|decode|ilong|medium|q5#5e1d0c3b9a7f2468"},
		// The ITL window key.
		{"itlBaseline, itlSlopes, startSeconds, variantSeenAt", itl,
			"ns-01|model-01|variant-01|NVIDIA-H100-80GB-HBM3|1"},
		{"itlBaselineStash", stashKey(itl, prevArgs),
			"ns-01|model-01|variant-01|NVIDIA-H100-80GB-HBM3|1#5e1d0c3b9a7f2468"},
		// A line is filed under its configuration, or under the window key
		// when the configuration was not read (both are in v0.10.1's goldens).
		{"itlLines, shared", a.itlPhysicsKey(ns, model, variant, accel, 1, "c4158ae264689362"),
			"model-01|NVIDIA-H100-80GB-HBM3|1|c4158ae264689362"},
		{"itlLines, own", a.itlPhysicsKey(ns, model, variant, accel, 1, ""),
			"ns-01|model-01|variant-01|NVIDIA-H100-80GB-HBM3|1"},
	} {
		assert.Equal(t, tc.want, tc.got, tc.maps)
	}

	// The per-variant maps (accelerators, forgotten, config, args) are keyed
	// namespace/variant, and a restore finds a window's variant there by
	// reading the window key back (variantOfKey).
	for _, key := range []string{k2, throughput, itl} {
		v, m, ok := variantOfKey(key)
		require.True(t, ok, key)
		assert.Equal(t, "ns-01/variant-01", v, key)
		assert.Equal(t, model, m, key)
	}
	_ = a.stableAccelerator(ns, variant, accel)
	a.ForgetVariant(ns, model, "variant-03", "2026-03-02-reprofile")
	exp := a.ExportLearnedState()
	assert.Contains(t, exp.Accelerators, "ns-01/variant-01", "accelerators")
	assert.Contains(t, exp.Forgotten, "ns-01/variant-03", "forgotten")
}
