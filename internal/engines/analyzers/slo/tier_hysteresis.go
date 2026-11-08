package slo

import (
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// SwitchITLTierHysteresis is the replay switch for the latch on the slo
// line's tier (issue #495, itl.Admission.TierHysteresis). Off, each cycle
// chooses its tier alone, as before.
const SwitchITLTierHysteresis = "slo-itl-tier-hysteresis"

// tierHysteresisOn reports SwitchITLTierHysteresis.
func (a *Analyzer) tierHysteresisOn() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.itlTierHysteresis
}

// capacityInputs is what a replica's price reads of the fleet.
type capacityInputs struct {
	tokens, kvPerRequest float64
	ranks                int64
}

// tierPrice tells the latch how the line is priced: at the target, on this
// engine (itl.Admission.PricedRate), as variantCapacity prices it.
func (a *Analyzer) tierPrice(adm *itl.Admission, tpotTarget float64, in capacityInputs, params *capacity.EngineParams) {
	batchCap, _ := a.pricedBatchCap(params)
	adm.OneSidedTarget = tpotTarget
	adm.TierPrice = itl.TierPrice{CapacityTokens: in.tokens, KVPerRequest: in.kvPerRequest, MaxSeqs: batchCap * in.ranks}
}
