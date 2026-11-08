package slo

import (
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// Each fit counts under its tier, and a cycle with no line counts nowhere.
func TestPathCounts_EachTierCountsUnderItsOwnName(t *testing.T) {
	a := New(nil, nil)
	assert.Equal(t, map[string]int64{PathITLOLS: 0, PathITLPinned: 0}, a.PathCounts())
	for _, tier := range []string{itl.TierOLS, itl.TierPinnedB, itl.TierPinnedB, ""} {
		a.paths.countFit(tier)
	}
	assert.Equal(t, map[string]int64{PathITLOLS: 1, PathITLPinned: 2}, a.PathCounts())
	assert.ElementsMatch(t, PathNames(), slices.Collect(maps.Keys(a.PathCounts())))
}
