package whatif

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// pdInput is a P/D model's cycle: variant p1 prefill, d1 decode.
func pdInput(rows ...domain.ReplicaMetrics) domain.AnalyzerInput {
	return domain.AnalyzerInput{Namespace: "ns", ModelID: "m", ReplicaMetrics: rows,
		VariantStates: []domain.VariantReplicaState{{VariantName: "p1", Role: domain.RolePrefill},
			{VariantName: "d1", Role: domain.RoleDecode}}}
}

// fullPrefill and fullDecode read every signal their role gives.
func fullPrefill() domain.ReplicaMetrics {
	return domain.ReplicaMetrics{PodName: "p", VariantName: "p1", Ready: true, RequestRate: 2, RequestRateKnown: true,
		AvgTTFT: 0.4, AvgTTFTKnown: true, AvgPrefillTime: 0.2, AvgPrefillTimeKnown: true, AvgQueueTime: 0.01,
		AvgQueueTimeKnown: true, PrefillComputedTokenRate: 4000, PrefillComputedTokenRateKnown: true,
		ComputedTokensBuckets: []domain.LengthBucket{{UpperBound: 1000, Rate: 1}},
		PromptTokensBuckets:   []domain.LengthBucket{{UpperBound: 1000, Rate: 1}},
		QueueTimeBuckets:      []domain.LatencyBucket{{UpperBound: 0.1, Rate: 1}}}
}

func fullDecode() domain.ReplicaMetrics {
	return domain.ReplicaMetrics{PodName: "d", VariantName: "d1", Ready: true, RequestRate: 2, RequestRateKnown: true,
		AvgTTFT: 0.05, AvgTTFTKnown: true, AvgITL: 0.02, AvgITLKnown: true, RunningRequests: 8, RunningRequestsKnown: true,
		OutputTokensBuckets: []domain.LengthBucket{{UpperBound: 256, Rate: 2}}}
}

func fullRouter() *domain.RouterLatency {
	return &domain.RouterLatency{TTFTBuckets: []domain.LatencyBucket{{UpperBound: 1, Rate: 2}}, TTFTSumRate: 1, TTFTCountRate: 2,
		TPOTBuckets: []domain.LatencyBucket{{UpperBound: 0.05, Rate: 2}}, TPOTSumRate: 0.04, TPOTCountRate: 2}
}

// A cycle that reads every signal misses none; the speculative decoding
// count, which the model does without, is never missing.
func TestMissingModelInputs_NoneWhenEveryOneIsRead(t *testing.T) {
	assert.Empty(t, MissingModelInputs(pdInput(fullPrefill(), fullDecode()), fullRouter(), "", ""))
}

// What a controller that records nothing collected before
// slo-v2-inputs (the record-only queries and the router's TPOT
// histogram off) is reported by name, per objective, in the registry's order.
func TestMissingModelInputs_TheRecordOnlySignalsByName(t *testing.T) {
	p := fullPrefill()
	p.AvgPrefillTime, p.AvgPrefillTimeKnown, p.AvgQueueTime, p.AvgQueueTimeKnown = 0, false, 0, false
	p.PromptTokensBuckets, p.QueueTimeBuckets = nil, nil
	r := fullRouter()
	r.TPOTBuckets = nil
	got := MissingModelInputs(pdInput(p, fullDecode()), r, "", "")
	assert.Equal(t, map[string][]string{
		ObjectiveTTFT: {"prefill-time-mean", "queue-time-mean", "prompt-tokens-histogram", "queue-time-histogram"},
		ObjectiveTPOT: {"router-tpot-histogram"},
	}, got)
}

// A row that does not serve reads nothing, a row of another variant is not
// the model's, and a nil router reads no router signal.
func TestMissingModelInputs_OnlyServingRowsOfTheVariantsCount(t *testing.T) {
	notReady := fullPrefill()
	notReady.Ready = false
	other := fullDecode()
	other.VariantName = "d2"
	in := pdInput(notReady, other)
	in.VariantStates = append(in.VariantStates, domain.VariantReplicaState{VariantName: "d2", Role: domain.RoleDecode})
	got := MissingModelInputs(in, nil, "p1", "d1")
	assert.Contains(t, got[ObjectiveTTFT], "prefill-pods")
	assert.Contains(t, got[ObjectiveTTFT], "router-ttft-histogram")
	assert.Contains(t, got[ObjectiveTPOT], "decode-pods")
	assert.Contains(t, got[ObjectiveTPOT], "router-tpot-mean")
	for _, mi := range ModelInputs {
		if mi.Optional {
			assert.NotContains(t, got[mi.Objective], mi.Name)
		}
	}
}

// An aggregated fleet has no prefill role: the rank mixture has no rank to
// read, and says so, while the TPOT distribution reads its serving rows, as
// DecodeCycleOf does.
func TestMissingModelInputs_AnAggregatedFleet(t *testing.T) {
	d := fullDecode()
	d.VariantName = "a1"
	in := domain.AnalyzerInput{ReplicaMetrics: []domain.ReplicaMetrics{d},
		VariantStates: []domain.VariantReplicaState{{VariantName: "a1"}}}
	got := MissingModelInputs(in, fullRouter(), "", "")
	assert.Equal(t, "prefill-pods", got[ObjectiveTTFT][0])
	assert.Empty(t, got[ObjectiveTPOT])
}

// A LeaderWorkerSet group whose engines read the timings is not missing
// them because the merged row does not.
func TestMissingModelInputs_AGroupsEnginesCount(t *testing.T) {
	p := fullPrefill()
	p.AvgPrefillTimeKnown, p.AvgQueueTimeKnown = false, false
	p.GroupEngines = []domain.EngineReading{{PodName: "p", AvgPrefillTimeKnown: true, AvgQueueTimeKnown: true}}
	assert.Empty(t, MissingModelInputs(pdInput(p, fullDecode()), fullRouter(), "", ""))
}

// Every input names its objective and role from the sets the metrics
// carry, each name once, and reads from exactly one source.
func TestModelInputs_AreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, mi := range ModelInputs {
		assert.False(t, seen[mi.Name], "%s listed twice", mi.Name)
		seen[mi.Name] = true
		assert.Contains(t, []string{ObjectiveTTFT, ObjectiveTPOT}, mi.Objective, mi.Name)
		assert.Contains(t, []string{InputRolePrefill, InputRoleDecode, InputRoleRouter}, mi.Role, mi.Name)
		assert.NotEqual(t, mi.row == nil, mi.router == nil, "%s reads from exactly one source", mi.Name)
		assert.Equal(t, mi.Role == InputRoleRouter, mi.router != nil, mi.Name)
	}
}
