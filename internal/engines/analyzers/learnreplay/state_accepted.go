package learnreplay

import (
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// maxAccepted is each variant of in's model's k, the draft tokens a step
// proposes at most, as store's engine params read it
// (capacity.EngineParams.SpeculativeTokens), where on
// (slo.SwitchSpecAcceptedAtMostK); nil, which holds nothing, where off.
func maxAccepted(store *capacity.Store, in domain.AnalyzerInput, on bool) whatif.MaxAccepted {
	if !on || store == nil {
		return nil
	}
	return func(variant string) int {
		if rec := store.Get(in.Namespace, in.ModelID, variant); rec != nil {
			return rec.EngineParams.SpeculativeTokens()
		}
		return 0
	}
}

// filedDrafts is each variant's k as the recording last filed it
// (capacity.KindFileScaleTarget), by namespace, model and variant: the
// decode series' own pass reads no store.
type filedDrafts map[[3]string]int

// file notes one filed scale target's k.
func (f filedDrafts) file(d capacity.FileScaleTargetEvent) {
	f[[3]string{d.Namespace, d.ModelID, d.Variant}] = d.Params.SpeculativeTokens()
}

// of is the k of in's model's variants as filed so far.
func (f filedDrafts) of(in domain.AnalyzerInput) whatif.MaxAccepted {
	return func(variant string) int { return f[[3]string{in.Namespace, in.ModelID, variant}] }
}
