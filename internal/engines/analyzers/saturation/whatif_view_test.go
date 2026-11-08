package saturation

import (
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// PricingBatchCap reads the learned cap the analyzer prices at and never
// stamps it as used: a what-if question asked of a replay must not keep a
// cap alive that the analyzer would have swept. pricingBatchCap, the
// analyzer's own read, does stamp it: the control that the stamp is there to
// be seen.
func TestPricingBatchCapNeverStamps(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	params := defaultedParams()
	key := batchCapKey("ns", "m", "v")
	a.batchCaps[key] = &learnedBatchCap{perRank: 120, args: windowArgs(params)}

	perRank, source := a.PricingBatchCap("ns", "m", "v", params, true)
	if perRank != 120 || source != batchCapLearned {
		t.Fatalf("priced %d (%s), want the learned 120; the case tests nothing", perRank, source)
	}
	if at := a.batchCaps[key].usedAt; !at.IsZero() {
		t.Errorf("PricingBatchCap stamped the learned cap as used at %v", at)
	}

	a.pricingBatchCap("ns", "m", "v", params, true)
	if a.batchCaps[key].usedAt.IsZero() {
		t.Error("pricingBatchCap did not stamp the learned cap: the control sees no stamp to keep out")
	}
}
