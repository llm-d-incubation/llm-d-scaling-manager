package whatif

import (
	"math"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// itl.Admission.PricedRate restates PriceTPOT, ReachableFill and
// ReplicaTokenRate, which itl cannot import. The tier latch's margin means
// what the slo analyzer prices only while they agree.
func TestPricedRate_IsWhatTheSLOLinePrices(t *testing.T) {
	band := FillBand{Lo: 0.15, Hi: 0.80}
	for _, line := range []itl.Model{{A: 0.03, B: 0.0025}, {A: 0.004, B: 0.0015}, {A: 0.12, B: 0.01}} {
		for _, target := range []float64{0.005, 0.02, 0.05, 1} {
			for _, maxSeqs := range []int64{0, 10, 64, 256, 1024} {
				const capTokens, kv = 300000.0, 1500.0
				p := PriceTPOT(band, line, target)
				if !p.Feasible {
					continue
				}
				fill := ReachableFill(band, p.Fill, capTokens, kv, maxSeqs)
				want := ReplicaTokenRate(line, fill, capTokens, kv, maxSeqs)
				adm := itl.Admission{MinK: band.Lo, MaxK: band.Hi, OneSidedTarget: target,
					TierPrice: itl.TierPrice{CapacityTokens: capTokens, KVPerRequest: kv, MaxSeqs: maxSeqs}}
				if got := adm.PricedRate(line); math.Abs(got-want) > 1e-9*want {
					t.Errorf("line %+v target %v cap %d: PricedRate %v, ReplicaTokenRate %v", line, target, maxSeqs, got, want)
				}
			}
		}
	}
}
