package steadystate

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/slo"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
)

// The v2 TTFT model's would-be ask joins the suggested series of a prefill
// variant nothing is applied to (passive, shadow), as the higher of the
// analyzers' own suggestion and the ask, under learner ttft-mixture; an
// active model's variant, a decode variant, another variant than the ask's,
// or a model with no ask has none. The gaps the decision step recorded are
// not touched.
func TestReplicaGap_V2AskOnlyWhereNothingIsApplied(t *testing.T) {
	gap := func(model, variant, role, mode string, suggested int) metrics.ReplicaGap {
		return metrics.ReplicaGap{Namespace: "ns", ModelName: model, VariantName: variant, Role: role, Mode: mode,
			Suggested: suggested, Provided: suggested}
	}
	asks := map[string]slo.ModelPrefillAsk{
		"passive": {Variant: "p", Pods: 5}, "shadow": {Variant: "p", Pods: 1}, "active": {Variant: "p", Pods: 9},
		"other": {Variant: "q", Pods: 4},
	}
	r := &replicaGapRecorder{
		gaps: []metrics.ReplicaGap{
			gap("passive", "p", domain.RolePrefill, constants.ReplicasModePassive, 2),
			gap("passive", "d", domain.RoleDecode, constants.ReplicasModePassive, 3),
			gap("shadow", "p", domain.RolePrefill, constants.ReplicasModeShadow, 2),
			gap("active", "p", domain.RolePrefill, constants.ReplicasModeActive, 2),
			gap("other", "p", domain.RolePrefill, constants.ReplicasModePassive, 2),
			gap("none", "p", domain.RolePrefill, constants.ReplicasModePassive, 2),
		},
		v2Ask: func(_, model string) (slo.ModelPrefillAsk, bool) {
			a, ok := asks[model]
			return a, ok
		},
	}
	before := append([]metrics.ReplicaGap(nil), r.gaps...)
	got := r.v2Gaps()
	assert.Equal(t, []metrics.ReplicaGap{
		{Namespace: "ns", ModelName: "passive", VariantName: "p", Role: domain.RolePrefill, Mode: constants.ReplicasModePassive,
			Learner: learn.TTFTMixture, Suggested: 5},
		{Namespace: "ns", ModelName: "shadow", VariantName: "p", Role: domain.RolePrefill, Mode: constants.ReplicasModeShadow,
			Learner: learn.TTFTMixture, Suggested: 2},
	}, got)
	assert.Equal(t, before, r.gaps)
	assert.Nil(t, (&replicaGapRecorder{gaps: before}).v2Gaps(), "no slo analyzer, no ask")
}
