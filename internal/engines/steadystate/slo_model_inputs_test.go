package steadystate

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/slo"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// Off by default: no namespace is named, so the collector issues no query
// of the v2 models', and nothing is observed or published.
func TestSLOV2Inputs_OffByDefault(t *testing.T) {
	e := &Engine{}
	groups := map[string][]llmdvariant.Variant{"m": {{ObjectMeta: metav1.ObjectMeta{Namespace: "ns"}}}}
	assert.Nil(t, e.predictionNamespaces(groups))
	e.observeModelInputs(context.Background(), &modelData{namespace: "ns", modelID: "m"}, nil)
	assert.Empty(t, e.sloModelInputs)
	e.retainModelInputs(nil)
}

// A model whose policy does not list the slo analyzer is not observed, the
// switch on or not: nothing would publish its v2 figures, and its window
// would hold one of the analyzer's slots for nothing.
func TestSLOV2Inputs_SkipsAModelThatDoesNotListSLO(t *testing.T) {
	e := &Engine{sloAnalyzer: sloReadingModelInputs(t)}
	policy := &config.ScalingPolicy{Analyzers: []config.AnalyzerScoreConfig{{Name: domain.SaturationAnalyzerName}}}
	e.observeModelInputs(context.Background(), &modelData{namespace: "ns", modelID: "m"}, policy)
	assert.Empty(t, e.sloModelInputs)
}

// On, every namespace with a model is named, once.
func TestSLOV2Inputs_NamesEveryNamespaceWhenOn(t *testing.T) {
	e := &Engine{sloAnalyzer: sloReadingModelInputs(t)}
	groups := map[string][]llmdvariant.Variant{
		"a": {{ObjectMeta: metav1.ObjectMeta{Namespace: "ns1"}}},
		"b": {{ObjectMeta: metav1.ObjectMeta{Namespace: "ns1"}}},
		"c": {{ObjectMeta: metav1.ObjectMeta{Namespace: "ns2"}}},
		"d": nil,
	}
	assert.Equal(t, map[string]bool{"ns1": true, "ns2": true}, e.predictionNamespaces(groups))
}

// The v2 models read the replicas the analyzers read, with what the
// collector kept on them: a pod routing dropped is dropped here too.
func TestSameReplicas_TheAnalyzersPods(t *testing.T) {
	kept := []domain.ReplicaMetrics{{PodName: "a", AvgPrefillTimeKnown: true}, {PodName: "b"}, {PodName: "c", AvgPrefillTimeKnown: true}}
	read := []domain.ReplicaMetrics{{PodName: "c"}, {PodName: "a"}}
	assert.Equal(t, []domain.ReplicaMetrics{kept[0], kept[2]}, sameReplicas(kept, read))
	assert.Empty(t, sameReplicas(kept, nil))
}

// The prune deletes the v2 series of a model no longer active, and keeps
// the others'.
func TestSLOV2Inputs_RetainDeletesInactiveModels(t *testing.T) {
	registry := prometheus.NewRegistry()
	require.NoError(t, metrics.InitMetrics(registry))
	a := sloReadingModelInputs(t)
	e := &Engine{sloAnalyzer: a, sloModelInputs: map[string][2]string{}}
	observe := func(models ...string) {
		e.sloModelInputsSeen = map[string]bool{}
		for _, m := range models {
			a.ObserveModelInputs(domain.AnalyzerInput{Namespace: "ns", ModelID: m}, nil)
			e.sloModelInputs["ns/"+m] = [2]string{"ns", m}
			e.sloModelInputsSeen["ns/"+m] = true
		}
	}
	observe("m", "n")
	count := func() int {
		n, err := testutil.GatherAndCount(registry, constants.LLMSMSLOModelCoverage)
		require.NoError(t, err)
		return n
	}
	require.Equal(t, 4*len(constants.SLOModelReasons), count())
	e.retainModelInputs(map[string]bool{"ns/n": true})
	assert.Equal(t, 2*len(constants.SLOModelReasons), count())
	// Still reconciled, but not observed on the last cycle (it turned
	// active, say): its series go too, not frozen at their last value.
	e.retainModelInputs(map[string]bool{"ns/n": true})
	assert.Zero(t, count())
	observe("n")
	e.retainModelInputs(nil)
	assert.Zero(t, count())
}

// sloReadingModelInputs is an slo analyzer with slo.SwitchV2Inputs on.
func sloReadingModelInputs(t *testing.T) *slo.Analyzer {
	t.Helper()
	a := slo.New(nil, nil)
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{slo.SwitchV2Inputs: true}))
	return a
}

// With the switch off, a passive model still runs the v2 models: the
// controller moves nothing for it, so they cost queries and no risk. Its
// namespace alone is named, and an active model's is not.
func TestSLOV2Inputs_RunForAPassiveModelWithTheSwitchOff(t *testing.T) {
	e := &Engine{sloAnalyzer: slo.New(nil, nil), cyclePassive: map[string]bool{"ns2/b": true}}
	groups := map[string][]llmdvariant.Variant{
		"a": {{ObjectMeta: metav1.ObjectMeta{Namespace: "ns1"}, Spec: llmdvariant.Spec{ModelID: "a"}}},
		"b": {{ObjectMeta: metav1.ObjectMeta{Namespace: "ns2"}, Spec: llmdvariant.Spec{ModelID: "b"}}},
	}
	assert.Equal(t, map[string]bool{"ns2": true}, e.predictionNamespaces(groups))
	assert.NotNil(t, e.modelInputs("ns2", "b"))
	assert.Nil(t, e.modelInputs("ns1", "a"), "an active model with the switch off")
}
