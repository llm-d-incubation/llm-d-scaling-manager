package slo

import (
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// SwitchSpecAcceptedAtMostK is the replay switch that reads a decode row's
// accepted drafts per step (domain.ReplicaMetrics.SpecAcceptedPerStep)
// against k, the draft tokens a step proposes at most
// (num_speculative_tokens), where the v2 TPOT model reads the row
// (whatif.DecodeCycleAcceptedAtMost): over k by a rounding error it is read
// at k, and over k by more the row gives no token time, as the ITL line
// withholds it (domain.ReplicaMetrics.ITLPerToken, ITLOverMax).
//
// No step accepts more than k, so such a count is not one a step produced.
// Read as it is, it puts the token time ITL / (1 + alpha) under what a step
// that accepted every draft takes, and the TPOT model prices low. Off, as
// before, it is read as it is. Off from the constructor; the capacity
// planner's replay turns it on (learnreplay.PlannerSwitches), for the
// planner's own decode series and its line rows' sequence counts too. A row
// of an engine whose flags state no k is read as it is either way.
const SwitchSpecAcceptedAtMostK = "spec-accepted-at-most-k"

// maxAcceptedFor is each variant's k of model namespace/modelID, as the
// capacity store's engine params read it (EngineParams.SpeculativeTokens),
// where SwitchSpecAcceptedAtMostK is on; nil, which holds nothing, where it
// is off.
func (a *Analyzer) maxAcceptedFor(namespace, modelID string) whatif.MaxAccepted {
	if !a.specAcceptedAtMostK {
		return nil
	}
	return func(variant string) int {
		return a.engineParams(namespace, modelID, variant).SpeculativeTokens()
	}
}
