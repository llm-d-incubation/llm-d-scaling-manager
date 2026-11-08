package saturation

import (
	"strings"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// borrowedK2 is borrowedK2WithKv for a replica that reported no KV total, the
// form the specs are written against, with the sibling borrow on (every
// call site asks with it on). The model is the one ownKey names; the second
// argument is the call sites' model, which is always that one.
func (a *SaturationAnalyzer) borrowedK2(ownKey, _ string, own int64, ownParams *capacity.EngineParams, serving, _ bool) (k2Borrow, bool) {
	_, _, rest, _ := splitVariantKey(ownKey)
	model, _, _ := strings.Cut(rest, "|")
	return a.borrowedK2WithKv(ownKey, model, own, ownParams, 0, serving, true)
}

// borrowClocks is the start of every armed lower bound, keyed like the
// bounds: what the tests read where they used to read k2BorrowedSince.
func borrowClocks(a *SaturationAnalyzer) map[string]time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]time.Time{}
	for k, lb := range a.lowerBounds {
		if lb.armed {
			out[k] = lb.since
		}
	}
	return out
}

// previousFigures is every lower bound's own previous figure (lowerBound.prior),
// keyed like the bounds: what the tests read where they used to read priorK2.
func previousFigures(a *SaturationAnalyzer) map[string]int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]int64{}
	for k, lb := range a.lowerBounds {
		if lb.prior > 0 {
			out[k] = lb.prior
		}
	}
	return out
}
