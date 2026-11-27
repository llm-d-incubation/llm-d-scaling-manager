package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

// gapRows reads one replica-gap family, keyed by the values of keys.
func gapRows(t *testing.T, reg *prometheus.Registry, name string, keys ...string) map[string]float64 {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	out := map[string]float64{}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			var k strings.Builder
			for _, key := range keys {
				for _, l := range m.GetLabel() {
					if l.GetName() == key {
						k.WriteString(l.GetValue() + "/")
					}
				}
			}
			out[k.String()] = m.GetGauge().GetValue()
		}
	}
	return out
}

// Each publish is the whole set: a reason that clears, a variant that goes and
// a quota pool that goes take their series with them, and an empty publish
// clears everything.
func TestPublishReplicaGaps_ReplacesTheSet(t *testing.T) {
	reg := prometheus.NewRegistry()
	require.NoError(t, InitMetrics(reg))
	short := func(reason, limiter string, n int) ReplicaShort {
		return ReplicaShort{Reason: reason, LimiterName: limiter, Replicas: n}
	}
	PublishReplicaGaps([]ReplicaGap{
		{Namespace: "ns", ModelName: "m", VariantName: "a", Role: "both", Suggested: 5, Provided: 2,
			Short: []ReplicaShort{short(constants.ReplicasShortReasonQuota, "q", 2), short(constants.ReplicasShortReasonMaxReplicas, "", 1)}},
		{Namespace: "ns", ModelName: "m", VariantName: "b", Role: "both", Suggested: 1, Provided: 3, Borrowed: 2},
	}, []QuotaAfterSuggested{{LimiterName: "q", Scope: "cluster", Accelerator: "A100", GPUs: -4}})
	require.Equal(t, map[string]float64{"a/": 5, "b/": 1}, gapRows(t, reg, constants.LLMSMReplicasSuggested, constants.LabelVariantName))
	require.Equal(t, map[string]float64{"a/": 2, "b/": 3}, gapRows(t, reg, constants.LLMSMReplicasProvided, constants.LabelVariantName))
	require.Equal(t, map[string]float64{"a/quota/q/": 2, "a/max-replicas//": 1},
		gapRows(t, reg, constants.LLMSMReplicasShort, constants.LabelVariantName, constants.LabelReason, constants.LabelLimiterName))
	require.Equal(t, map[string]float64{"a/": 0, "b/": 2}, gapRows(t, reg, constants.LLMSMReplicasBorrowed, constants.LabelVariantName))
	require.Equal(t, map[string]float64{"q/cluster/A100/": -4}, gapRows(t, reg, constants.LLMSMQuotaRemainingAfterSuggestedGPUs,
		constants.LabelLimiterName, constants.LabelScope, constants.LabelAccelerator))

	// b goes, a's reason changes, the quota pool goes.
	PublishReplicaGaps([]ReplicaGap{
		{Namespace: "ns", ModelName: "m", VariantName: "a", Role: "both", Suggested: 3, Provided: 2,
			Short: []ReplicaShort{short(constants.ReplicasShortReasonGPU, "", 1)}},
	}, nil)
	require.Equal(t, map[string]float64{"a/": 3}, gapRows(t, reg, constants.LLMSMReplicasSuggested, constants.LabelVariantName))
	require.Equal(t, map[string]float64{"a/gpu//": 1},
		gapRows(t, reg, constants.LLMSMReplicasShort, constants.LabelVariantName, constants.LabelReason, constants.LabelLimiterName))
	require.Equal(t, map[string]float64{"a/": 0}, gapRows(t, reg, constants.LLMSMReplicasBorrowed, constants.LabelVariantName))
	require.Empty(t, gapRows(t, reg, constants.LLMSMQuotaRemainingAfterSuggestedGPUs, constants.LabelLimiterName))

	// a's mode changes: its series under the old mode go.
	PublishReplicaGaps([]ReplicaGap{
		{Namespace: "ns", ModelName: "m", VariantName: "a", Role: "both", Mode: constants.ReplicasModeShadow,
			Suggested: 3, Provided: 3},
	}, nil)
	require.Equal(t, map[string]float64{"a/shadow/": 3},
		gapRows(t, reg, constants.LLMSMReplicasProvided, constants.LabelVariantName, constants.LabelMode))

	PublishReplicaGaps(nil, nil)
	for _, name := range []string{constants.LLMSMReplicasSuggested, constants.LLMSMReplicasProvided,
		constants.LLMSMReplicasShort, constants.LLMSMReplicasBorrowed} {
		require.Empty(t, gapRows(t, reg, name, constants.LabelVariantName), name)
	}
}

// A v2 learner's would-be suggestion is published on
// llmsm_replicas_suggested under its learner name, beside the analyzers' own
// (learner empty, which PromQL reads as absent), and on none of the other
// families: nothing provides or limits it.
func TestPublishReplicaGaps_V2SuggestionBesideTheOwn(t *testing.T) {
	reg := prometheus.NewRegistry()
	require.NoError(t, InitMetrics(reg))
	PublishReplicaGaps([]ReplicaGap{
		{Namespace: "ns", ModelName: "m", VariantName: "p", Role: "prefill", Mode: constants.ReplicasModePassive, Suggested: 2, Provided: 2},
		{Namespace: "ns", ModelName: "m", VariantName: "p", Role: "prefill", Mode: constants.ReplicasModePassive, Learner: "ttft-mixture",
			Suggested: 3},
	}, nil)
	require.Equal(t, map[string]float64{"p//": 2, "p/ttft-mixture/": 3},
		gapRows(t, reg, constants.LLMSMReplicasSuggested, constants.LabelVariantName, constants.LabelLearner))
	require.Equal(t, map[string]float64{"p/": 2}, gapRows(t, reg, constants.LLMSMReplicasProvided, constants.LabelVariantName))
	require.Equal(t, map[string]float64{"p/": 0}, gapRows(t, reg, constants.LLMSMReplicasBorrowed, constants.LabelVariantName))

	// Gone the next cycle, with the passive model's own kept.
	PublishReplicaGaps([]ReplicaGap{
		{Namespace: "ns", ModelName: "m", VariantName: "p", Role: "prefill", Mode: constants.ReplicasModePassive, Suggested: 2, Provided: 2},
	}, nil)
	require.Equal(t, map[string]float64{"p//": 2},
		gapRows(t, reg, constants.LLMSMReplicasSuggested, constants.LabelVariantName, constants.LabelLearner))
}
