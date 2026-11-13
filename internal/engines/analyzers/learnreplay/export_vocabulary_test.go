package learnreplay

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
)

// Every rate source the code defines is a word the export keeps. Before
// "neighbour" and "lower-bound" were in the vocabulary, a recording whose
// variant priced from a neighbouring bucket's measured rate was refused
// ("ServiceRate.RateSource in saturation.Analyze holds a value that is not
// allowed after the export"), the kind start-time recording among them.
func TestVocabulary_HoldsEveryServiceRateSource(t *testing.T) {
	for _, source := range []string{
		domain.ServiceRateSourceDerived,
		domain.ServiceRateSourceBorrowedLine,
		domain.ServiceRateSourceMeasured,
		domain.ServiceRateSourceNeighbour,
		domain.ServiceRateSourceLowerBound,
		domain.ServiceRateSourceReportOnly,
		domain.ServiceRateSourceDerivedAggregated,
	} {
		assert.True(t, vocabulary[source], "%q is not in the export's vocabulary", source)
	}
}

// The role fields of prefill's lower-bound window keys are code words the
// export keeps (issue llm-d-incubation/llm-d-scaling-manager#97). Before
// they were in the vocabulary, every recording of a P/D model whose prefill
// held a busy-share window was refused ("a key of Throughput in
// saturation.ExportLearnedState is not allowed after the export").
func TestVocabulary_HoldsThePrefillLowerBoundRoles(t *testing.T) {
	roles := saturation.LowerBoundRoles()
	require.NotEmpty(t, roles, "precondition: the saturation analyzer names its lower-bound roles")
	for _, role := range roles {
		assert.True(t, vocabulary[role], "%q is not in the export's vocabulary", role)
		assert.True(t, shapePart(role), "%q is not read as a window key's shape part", role)
	}
}

// A run under the utilization-share optimizer records the policy's optimizer
// type, config.OptimizerTypeUtilizationShare, in every saturation Analyze.
// It is the code's own word, so the export keeps it and the export replays.
// Before the word was in the vocabulary, every recording of such a run was
// refused ("Optimizer.Type in saturation.Analyze holds a value that is not
// allowed after the export"), the E1 benchmark scenario among them.
func TestExport_KeepsTheUtilizationShareOptimizerType(t *testing.T) {
	pol := &config.ScalingPolicy{
		KvCacheThreshold: 0.8,
		Optimizer: &config.OptimizerConfig{
			Type:             config.OptimizerTypeUtilizationShare,
			UtilizationShare: &config.UtilizationShareConfig{Shadow: false},
		},
	}
	rec := probeRecording(t, "ns", "mdl", []string{"v"}, pol, nil)
	require.Contains(t, string(rec), `"Type":"`+config.OptimizerTypeUtilizationShare+`"`, "precondition: the recording holds the optimizer type")

	out, _, err := Export(rec, ExportOptions{Key: []byte("k")})
	require.NoError(t, err)
	assert.Contains(t, string(out), `"Type":"`+config.OptimizerTypeUtilizationShare+`"`, "the optimizer type is kept as it is")

	r, err := Replay(context.Background(), out, Options{Mode: Fidelity})
	require.NoError(t, err)
	assert.Empty(t, r.Failed())
	assert.Empty(t, r.Mismatches, "the export does not replay")
}
