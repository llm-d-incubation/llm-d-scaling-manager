package steadystate

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// emptyMetricsSource answers every query with no series: a model scaled to
// zero, or one whose pods nothing scrapes.
type emptyMetricsSource struct{}

func (emptyMetricsSource) QueryList() *source.QueryList { return source.NewQueryList() }

func (emptyMetricsSource) Refresh(_ context.Context, spec source.RefreshSpec) (map[string]*source.MetricResult, error) {
	out := make(map[string]*source.MetricResult, len(spec.Queries))
	for _, q := range spec.Queries {
		out[q] = &source.MetricResult{QueryName: q, Values: []source.MetricValue{}}
	}
	return out, nil
}

func (emptyMetricsSource) Get(string, map[string]string) *source.CachedValue { return nil }

// failingMetricsSource fails every refresh: the metrics backend unreachable.
type failingMetricsSource struct{ emptyMetricsSource }

func (failingMetricsSource) Refresh(context.Context, source.RefreshSpec) (map[string]*source.MetricResult, error) {
	return nil, errors.New("connection refused")
}

// parkedVA is model m's one variant, scaled by Deployment d.
func parkedVA() llmdvariant.Variant {
	return llmdvariant.Variant{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: "ns"},
		Spec: llmdvariant.Spec{
			ModelID:        "m",
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "d"},
		},
	}
}

// parkedTarget is Deployment d scaled to zero, as a parked model's is: the
// skip path runs with its scale target read, not with none.
func parkedTarget() *appsv1.Deployment {
	d := deployWithArgs("--max-num-seqs=128")
	zero := int32(0)
	d.Spec.Replicas = &zero
	return d
}

// The skip path is where a parked model's series are told that nothing was
// observed. The helper it calls is tested on its own; this drives
// prepareModelData through a collector that returns no rows, so removing the
// call -- not just emptying the helper -- fails too.
func TestPrepareModelData_NothingCollectedClearsTheSeries(t *testing.T) {
	e, registry := engineConfigEngine(t)
	e.ReplicaMetricsCollector = collector.NewReplicaMetricsCollector(emptyMetricsSource{}, nil, nil, record.NewFakeRecorder(10), nil)

	nr := serviceRateResult(0.68, "v")
	nr.Result.VariantCapacities[0].ObservedReplicas = 3
	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{nr})

	md, err := e.prepareModelData(context.Background(), "m",
		[]llmdvariant.Variant{parkedVA()},
		fake.NewClientBuilder().WithObjects(parkedTarget()).Build())
	require.NoError(t, err)
	require.Nil(t, md, "no replica rows: the model is skipped")

	for _, name := range append(serviceRateFamilies, constants.LLMSMPricingK) {
		assert.Empty(t, seriesLabels(t, registry, name, constants.LabelModelName),
			"%s: a model with nothing collected must not keep its last reading", name)
	}
	assert.Equal(t, map[string]float64{"v": 0},
		gaugeValues(t, registry, constants.LLMSMAnalyzerObservedReplicas, constants.LabelVariantName),
		"nothing collected reads as nothing observed")
}

// A failed collection is not an empty one: the controller could not see the
// model, which says nothing about whether it is serving. Its series keep their
// last values, as llmsm_analyzer_target does, and the error is returned.
func TestPrepareModelData_FailedCollectionKeepsTheSeries(t *testing.T) {
	e, registry := engineConfigEngine(t)
	e.ReplicaMetricsCollector = collector.NewReplicaMetricsCollector(failingMetricsSource{}, nil, nil, record.NewFakeRecorder(10), nil)

	nr := serviceRateResult(0.68, "v")
	nr.Result.VariantCapacities[0].ObservedReplicas = 3
	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{nr})

	md, err := e.prepareModelData(context.Background(), "m",
		[]llmdvariant.Variant{parkedVA()},
		fake.NewClientBuilder().WithObjects(parkedTarget()).Build())
	require.Error(t, err, "a failed collection must surface")
	require.Nil(t, md)

	for _, name := range append(serviceRateFamilies, constants.LLMSMPricingK) {
		assert.ElementsMatch(t, []string{"m"}, seriesLabels(t, registry, name, constants.LabelModelName),
			"%s: a failed collection deleted the last reading", name)
	}
	assert.Equal(t, map[string]float64{"v": 3},
		gaugeValues(t, registry, constants.LLMSMAnalyzerObservedReplicas, constants.LabelVariantName),
		"a failed collection read as nothing observed")
}
