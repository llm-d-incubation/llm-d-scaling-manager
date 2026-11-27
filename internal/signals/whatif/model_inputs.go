package whatif

import (
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// The signals the experimental latency models read, by name: what the
// controller reports missing for a model it would predict for
// (docs/proposals/predicted-latency-in-controller.md, "Signals"). The list
// follows what MixtureCycleOf and DecodeCycleOf read of a cycle, and what
// the models fit against from the router; a signal is present when at least
// one serving row of its role reads it this cycle, or, for the router's, when
// the router reported it. A missing signal is reported, never estimated.
//
// The models are not frozen: a signal a model stops or starts reading at the
// freeze is added or removed here, in the same change as the model.

// The latency a model input belongs to, as the slo analyzer's metrics name
// an objective.
const (
	ObjectiveTTFT = "ttft"
	ObjectiveTPOT = "tpot"
)

// The role a model input is read from: a pod role, or the router.
const (
	InputRolePrefill = domain.RolePrefill
	InputRoleDecode  = domain.RoleDecode
	InputRoleRouter  = "router"
)

// ModelInput is one signal an experimental latency model reads.
type ModelInput struct {
	// Name is the signal's name, kebab-case: the label value a missing
	// signal is reported under.
	Name string
	// Objective is ObjectiveTTFT or ObjectiveTPOT: the model that reads it.
	Objective string
	// Role is where it is read: InputRolePrefill, InputRoleDecode (on a
	// fleet with no decode role, its serving rows that are not prefill) or
	// InputRoleRouter.
	Role string
	// Optional is a signal the model reads where it is present and does
	// without where it is not (speculative decoding's accepted drafts): it
	// is never reported missing.
	Optional bool
	// row reads it from one serving row; router from the router's latency.
	row    func(rm domain.ReplicaMetrics) bool
	router func(r *domain.RouterLatency) bool
}

// ModelInputs is every signal the experimental TTFT model
// (QueueRankMixture) and TPOT distribution (TPOTRenewalLognormal) read, in
// the order they are reported.
var ModelInputs = []ModelInput{
	// The rank mixture: the prefill ranks' rates and timings, their pooled
	// histograms, the decode pods' TTFT, and the router's TTFT it fits on.
	{Name: "prefill-pods", Objective: ObjectiveTTFT, Role: InputRolePrefill,
		row: func(domain.ReplicaMetrics) bool { return true }},
	{Name: "prefill-request-rate", Objective: ObjectiveTTFT, Role: InputRolePrefill,
		row: func(rm domain.ReplicaMetrics) bool { return rm.RequestRateKnown }},
	{Name: "prefill-ttft-mean", Objective: ObjectiveTTFT, Role: InputRolePrefill,
		row: func(rm domain.ReplicaMetrics) bool { return rm.AvgTTFTKnown }},
	{Name: "prefill-time-mean", Objective: ObjectiveTTFT, Role: InputRolePrefill,
		row: func(rm domain.ReplicaMetrics) bool {
			return rm.AvgPrefillTimeKnown || anyEngine(rm, func(e domain.EngineReading) bool { return e.AvgPrefillTimeKnown })
		}},
	{Name: "queue-time-mean", Objective: ObjectiveTTFT, Role: InputRolePrefill,
		row: func(rm domain.ReplicaMetrics) bool {
			return rm.AvgQueueTimeKnown || anyEngine(rm, func(e domain.EngineReading) bool { return e.AvgQueueTimeKnown })
		}},
	{Name: "computed-token-rate", Objective: ObjectiveTTFT, Role: InputRolePrefill,
		row: func(rm domain.ReplicaMetrics) bool { return rm.PrefillComputedTokenRateKnown }},
	{Name: "computed-tokens-histogram", Objective: ObjectiveTTFT, Role: InputRolePrefill,
		row: func(rm domain.ReplicaMetrics) bool { return len(rm.ComputedTokensBuckets) > 0 }},
	{Name: "prompt-tokens-histogram", Objective: ObjectiveTTFT, Role: InputRolePrefill,
		row: func(rm domain.ReplicaMetrics) bool { return len(rm.PromptTokensBuckets) > 0 }},
	{Name: "queue-time-histogram", Objective: ObjectiveTTFT, Role: InputRolePrefill,
		row: func(rm domain.ReplicaMetrics) bool { return len(rm.QueueTimeBuckets) > 0 }},
	{Name: "decode-ttft-mean", Objective: ObjectiveTTFT, Role: InputRoleDecode,
		row: func(rm domain.ReplicaMetrics) bool { return rm.AvgTTFTKnown }},
	{Name: "router-ttft-histogram", Objective: ObjectiveTTFT, Role: InputRoleRouter,
		router: func(r *domain.RouterLatency) bool { return len(r.TTFTBuckets) > 0 }},
	{Name: "router-ttft-mean", Objective: ObjectiveTTFT, Role: InputRoleRouter,
		router: func(r *domain.RouterLatency) bool { return r.TTFTCountRate > 0 }},

	// The TPOT distribution: the decode pods' output lengths, token time,
	// accepted drafts and running sequences, and the router's TPOT
	// histogram it fits on.
	{Name: "decode-pods", Objective: ObjectiveTPOT, Role: InputRoleDecode,
		row: func(domain.ReplicaMetrics) bool { return true }},
	{Name: "decode-request-rate", Objective: ObjectiveTPOT, Role: InputRoleDecode,
		row: func(rm domain.ReplicaMetrics) bool { return rm.RequestRateKnown }},
	{Name: "itl-mean", Objective: ObjectiveTPOT, Role: InputRoleDecode,
		row: func(rm domain.ReplicaMetrics) bool { return rm.AvgITLKnown }},
	{Name: "output-tokens-histogram", Objective: ObjectiveTPOT, Role: InputRoleDecode,
		row: func(rm domain.ReplicaMetrics) bool { return len(rm.OutputTokensBuckets) > 0 }},
	{Name: "running-requests", Objective: ObjectiveTPOT, Role: InputRoleDecode,
		row: func(rm domain.ReplicaMetrics) bool {
			return rm.RunningRequestsKnown || anyEngine(rm, func(e domain.EngineReading) bool { return e.RunningRequestsKnown })
		}},
	{Name: "spec-accepted-per-step", Objective: ObjectiveTPOT, Role: InputRoleDecode, Optional: true,
		row: func(rm domain.ReplicaMetrics) bool { return rm.SpecAcceptedPerStepKnown }},
	{Name: "router-tpot-histogram", Objective: ObjectiveTPOT, Role: InputRoleRouter,
		router: func(r *domain.RouterLatency) bool { return len(r.TPOTBuckets) > 0 }},
	{Name: "router-tpot-mean", Objective: ObjectiveTPOT, Role: InputRoleRouter,
		router: func(r *domain.RouterLatency) bool { return r.TPOTCountRate > 0 }},
}

// anyEngine says one engine of rm's LeaderWorkerSet group reads what read
// asks; false for a row of no group.
func anyEngine(rm domain.ReplicaMetrics, read func(domain.EngineReading) bool) bool {
	for _, e := range rm.GroupEngines {
		if read(e) {
			return true
		}
	}
	return false
}

// MissingModelInputs is, for each objective, the names of the inputs of
// ModelInputs that in and router do not read this cycle, in ModelInputs'
// order; an objective with none missing is absent. The rows are the serving
// rows of prefillVariant and decodeVariant (every row of the role where it
// is empty), as MixtureCycleOf and DecodeCycleOf select them. A nil router
// reads no router signal. Optional inputs are never missing.
func MissingModelInputs(in domain.AnalyzerInput, router *domain.RouterLatency, prefillVariant, decodeVariant string) map[string][]string {
	roles := rolesOf(in)
	hasDecode := false
	for _, rm := range in.ReplicaMetrics {
		if domain.CanonicalRole(roles[rm.VariantName]) == domain.RoleDecode {
			hasDecode = true
			break
		}
	}
	byRole := map[string][]domain.ReplicaMetrics{}
	for _, rm := range in.ReplicaMetrics {
		if !prefill.Serving(rm) {
			continue
		}
		switch role := domain.CanonicalRole(roles[rm.VariantName]); {
		case role == domain.RolePrefill:
			if prefillVariant == "" || rm.VariantName == prefillVariant {
				byRole[InputRolePrefill] = append(byRole[InputRolePrefill], rm)
			}
		case role == domain.RoleDecode || !hasDecode:
			if decodeVariant == "" || rm.VariantName == decodeVariant {
				byRole[InputRoleDecode] = append(byRole[InputRoleDecode], rm)
			}
		}
	}
	out := map[string][]string{}
	for _, mi := range ModelInputs {
		if mi.Optional || mi.present(byRole[mi.Role], router) {
			continue
		}
		out[mi.Objective] = append(out[mi.Objective], mi.Name)
	}
	return out
}

// present says one of rows, or the router, reads mi.
func (mi ModelInput) present(rows []domain.ReplicaMetrics, router *domain.RouterLatency) bool {
	if mi.router != nil {
		return router != nil && mi.router(router)
	}
	for _, rm := range rows {
		if mi.row(rm) {
			return true
		}
	}
	return false
}
