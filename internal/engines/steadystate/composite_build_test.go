package steadystate

import (
	"context"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
)

// runAnalyzersAndScore runs every analyzer for model modelID in namespace "ns"
// and records their series, as one engine cycle does before the composite is
// built, with no metrics, metadata or scale targets.
func (e *Engine) runAnalyzersAndScore(ctx context.Context, modelID string, cfg config.ScalingPolicy) ([]allocation.NamedAnalyzerResult, error) {
	results, err := e.runAnalyzers(ctx, modelID, "ns", nil, cfg, nil, nil, nil, nil, nil, 0)
	if err != nil {
		return nil, err
	}
	e.recordAnalyzerMetrics("ns", modelID, results)
	return results, nil
}

// liveEntry is an eligible analyzer entry for one non-disaggregated variant
// "v1" with demand and per-replica capacity prc.
func liveEntry(name string, demand, prc float64) allocation.NamedAnalyzerResult {
	return allocation.NamedAnalyzerResult{
		Name: name,
		Live: true,
		Result: &domain.AnalyzerResult{
			AnalyzerName: name,
			ModelID:      "m",
			Namespace:    "ns",
			TotalDemand:  demand,
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "v1", ReplicaCount: 3, PerReplicaCapacity: prc, TotalDemand: demand, Reason: "measured"},
			},
		},
	}
}

// TestBuildCompositeKeepsEverySaturationField: the composite is saturation's
// result with only the decided fields changed. A field built from a list
// would drop whatever saturation gains later: stuck replicas counted as
// arriving supply, the role holds the share optimizer reads.
func TestBuildCompositeKeepsEverySaturationField(t *testing.T) {
	sat := liveEntry(domain.SaturationAnalyzerName, 0, 0)
	sat.Result = &domain.AnalyzerResult{
		AnalyzerName: domain.SaturationAnalyzerName,
		ModelID:      "m",
		Namespace:    "ns",
		RoleDemand:   map[string]float64{domain.RolePrefill: 400, domain.RoleDecode: 900},
		VariantCapacities: []domain.VariantCapacity{{
			VariantName:                "p",
			Role:                       domain.RolePrefill,
			StuckReplicas:              2,
			PendingAges:                []float64{30, 90},
			ReplicaCount:               4,
			PendingReplicas:            3,
			ObservedReplicas:           5,
			PerReplicaCapacity:         100,
			Reason:                     "measured",
			WarmPoolReplicas:           1,
			WarmPoolPerReplicaCapacity: 90,
			WarmPoolCapacity:           90,
			TotalDemand:                400,
			Utilization:                1,
		}},
		HeldRoleDemand:          map[string]float64{domain.RolePrefill: 380},
		RoleStartSeconds:        map[string]float64{domain.RolePrefill: 120},
		VariantBusiestQueueLoad: map[string]float64{"p": 7},
		PricingK:                0.8,
	}
	named := []allocation.NamedAnalyzerResult{sat}

	composite := buildComposite(context.Background(), named, named, 0.85, 0.7)

	got := composite.Result
	want := *sat.Result
	assert.Equal(t, allocation.CompositeSignalName, got.AnalyzerName)
	assert.Equal(t, want.RoleDemand, got.RoleDemand)
	assert.Equal(t, want.HeldRoleDemand, got.HeldRoleDemand)
	assert.Equal(t, want.RoleStartSeconds, got.RoleStartSeconds)
	assert.Equal(t, want.VariantBusiestQueueLoad, got.VariantBusiestQueueLoad)
	assert.Equal(t, want.PricingK, got.PricingK)
	require.Len(t, got.VariantCapacities, 1)
	vc, wvc := got.VariantCapacities[0], want.VariantCapacities[0]
	assert.Equal(t, string(allocation.DecisionSingle), vc.Reason)
	wvc.Reason = vc.Reason
	assert.Equal(t, wvc, vc, "every field but Reason is saturation's")

	// Nothing aliases saturation's own result.
	got.HeldRoleDemand[domain.RolePrefill] = 0
	got.RoleDemand[domain.RoleDecode] = 0
	got.VariantCapacities[0].PendingAges[0] = 0
	assert.Equal(t, 380.0, sat.Result.HeldRoleDemand[domain.RolePrefill])
	assert.Equal(t, 900.0, sat.Result.RoleDemand[domain.RoleDecode])
	assert.Equal(t, 30.0, sat.Result.VariantCapacities[0].PendingAges[0])
}

// TestBuildCompositeKeepsSaturationPRCExactly: when saturation sets the need,
// the composite prices with saturation's own PRC, not D/(D/P), which is not
// always P and moves a replica count sitting on a boundary.
func TestBuildCompositeKeepsSaturationPRCExactly(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	inexact := 0
	for range 2000 {
		d, p := r.Float64()*1000+1, r.Float64()*300+1
		if d/(d/p) != p {
			inexact++
		}
		sat := liveEntry(domain.SaturationAnalyzerName, d, p)
		named := []allocation.NamedAnalyzerResult{sat}
		// An analyzer needing the same is a tie, priced in saturation's frame.
		tie := []allocation.NamedAnalyzerResult{sat, liveEntry("throughput", d, p)}
		for _, eligible := range [][]allocation.NamedAnalyzerResult{named, nil, tie} {
			composite := buildComposite(context.Background(), named, eligible, 0.85, 0.7)
			require.Equal(t, p, composite.Result.VariantCapacities[0].PerReplicaCapacity,
				"demand %v, prc %v, path %s", d, p, composite.Result.VariantCapacities[0].Reason)
		}
	}
	require.Positive(t, inexact, "the sample must contain values where D/(D/P) != P")
}

// TestBuildCompositePricesTheLargestNeed: another analyzer needing more
// replicas than saturation sets the demand that need comes to at
// saturation's own PRC, N x PRC_sat; the PRC is saturation's.
func TestBuildCompositePricesTheLargestNeed(t *testing.T) {
	sat := liveEntry(domain.SaturationAnalyzerName, 800, 200) // needs 4
	other := liveEntry("throughput", 60, 10)                  // needs 6
	named := []allocation.NamedAnalyzerResult{sat, other}

	composite := buildComposite(context.Background(), named, named, 0.85, 0.7)

	vc := composite.Result.VariantCapacities[0]
	assert.Equal(t, string(allocation.DecisionAgree), vc.Reason)
	assert.Equal(t, 200.0, vc.PerReplicaCapacity)
	assert.InDelta(t, 1200, composite.Result.TotalDemand, 1e-9)
	assert.InDelta(t, 1200, vc.TotalDemand, 1e-9)
	assert.InDelta(t, 1200.0/(3*200), vc.Utilization, 1e-9)
}

// TestBuildCompositeIgnoresANonFiniteNeed: one analyzer's +Inf or NaN demand
// is no reading. Taken as the largest need it priced every replica at zero.
func TestBuildCompositeIgnoresANonFiniteNeed(t *testing.T) {
	for _, demand := range []float64{math.Inf(1), math.NaN(), math.MaxFloat64} {
		sat := liveEntry(domain.SaturationAnalyzerName, 800, 200)
		other := liveEntry("throughput", demand, 1e-300)
		named := []allocation.NamedAnalyzerResult{sat, other}

		composite := buildComposite(context.Background(), named, named, 0.85, 0.7)

		vc := composite.Result.VariantCapacities[0]
		assert.Equal(t, 200.0, vc.PerReplicaCapacity, "demand %v", demand)
		assert.Equal(t, 800.0, composite.Result.TotalDemand, "demand %v", demand)
		assert.Equal(t, string(allocation.DecisionSingle), vc.Reason, "demand %v", demand)
	}
}

// TestBuildCompositeGrowsARoleSaturationSeesAsIdle: with no saturation demand
// another analyzer's need still reaches the optimizer, as N x PRC_sat. Under
// the PRC rewrite there was nothing to divide, saturation's PRC was kept, and
// the need was lost.
func TestBuildCompositeGrowsARoleSaturationSeesAsIdle(t *testing.T) {
	sat := liveEntry(domain.SaturationAnalyzerName, 0, 200)
	other := liveEntry("throughput", 60, 10) // needs 6
	named := []allocation.NamedAnalyzerResult{sat, other}

	composite := buildComposite(context.Background(), named, named, 0.85, 0.7)

	vc := composite.Result.VariantCapacities[0]
	assert.Equal(t, 200.0, vc.PerReplicaCapacity)
	assert.InDelta(t, 1200, composite.Result.TotalDemand, 1e-9)
	assert.InDelta(t, 1200, vc.TotalDemand, 1e-9, "the only variant carries all of it")
	assert.Positive(t, composite.RequiredCapacity, "6 replicas wanted, 3 running")
}

func TestExcludeByName(t *testing.T) {
	a, b := liveEntry("a", 1, 1), liveEntry("b", 1, 1)
	for _, tc := range []struct {
		name  string
		in    []allocation.NamedAnalyzerResult
		drop  string
		names []string
	}{
		{"absent", []allocation.NamedAnalyzerResult{a, b}, "c", []string{"a", "b"}},
		{"first", []allocation.NamedAnalyzerResult{a, b}, "a", []string{"b"}},
		{"last", []allocation.NamedAnalyzerResult{a, b}, "b", []string{"a"}},
		{"every copy", []allocation.NamedAnalyzerResult{a, b, a}, "a", []string{"b"}},
		{"empty", nil, "a", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			namesOf := func(in []allocation.NamedAnalyzerResult) []string {
				names := make([]string, 0, len(in))
				for _, nr := range in {
					names = append(names, nr.Name)
				}
				return names
			}
			before := namesOf(tc.in)
			out := excludeByName(tc.in, tc.drop)
			assert.Equal(t, tc.names, namesOf(out))
			assert.Equal(t, before, namesOf(tc.in), "the input is not modified")
		})
	}
}

// TestCompositeTakesSaturationsThresholds: the composite's demand is
// saturation's, so it is judged against saturation's band, a per-analyzer
// override included, not the policy's global one.
func TestCompositeTakesSaturationsThresholds(t *testing.T) {
	up, down := 0.6, 0.4
	cfg := config.ScalingPolicy{
		ScaleUpThreshold:  0.85,
		ScaleDownBoundary: 0.70,
		Analyzers: []config.AnalyzerScoreConfig{{
			Name: domain.SaturationAnalyzerName, ScaleUpThreshold: &up, ScaleDownBoundary: &down,
		}},
	}
	e := satOnlyEngine(liveEntry(domain.SaturationAnalyzerName, 800, 200).Result)

	req, err := e.collectV2ModelRequest(context.Background(), "m", "ns", nil, cfg, nil, nil, nil, nil, nil, 0)
	require.NoError(t, err)

	assert.Equal(t, up, req.CompositeSignal.ScaleUpThreshold)
	assert.Equal(t, down, req.CompositeSignal.ScaleDownBoundary)
}

// TestCompositeRecordKeepsTheEngineConfig: the analyzer series are recorded
// once a cycle. Recorded twice, the first record of each cycle evicted the
// composite's series from the one before, and each variant's engine
// configuration with them.
func TestCompositeRecordKeepsTheEngineConfig(t *testing.T) {
	registry := prometheus.NewRegistry()
	require.NoError(t, metrics.InitMetrics(registry))
	e := satOnlyEngine(liveEntry(domain.SaturationAnalyzerName, 800, 200).Result)
	e.metricsEmitter = metrics.NewMetricsEmitter()

	for cycle := 1; cycle <= 3; cycle++ {
		e.metricsEmitter.RecordEngineConfig("ns", "m", "v1", "H100", 1, "fp", 1,
			make([]string, len(constants.EngineConfigFlagLabels)))
		_, err := e.collectV2ModelRequest(context.Background(), "m", "ns", nil, scaleCfg, nil, nil, nil, nil, nil, 0)
		require.NoError(t, err)

		assert.Len(t, seriesLabels(t, registry, constants.LLMSMEngineConfig, constants.LabelVariantName), 1,
			"cycle %d: the engine configuration series survives", cycle)
		assert.ElementsMatch(t,
			[]string{domain.SaturationAnalyzerName, allocation.CompositeSignalName},
			seriesLabels(t, registry, constants.LLMSMAnalyzerTarget, constants.LabelAnalyzerName),
			"cycle %d", cycle)
	}
}

// TestReportSignalReason: the reason is set for a composite with no usable
// signal, cleared for one with a signal, and cleared for a model this cycle
// built no composite for, so it does not stand from an earlier cycle.
func TestReportSignalReason(t *testing.T) {
	registry := prometheus.NewRegistry()
	require.NoError(t, metrics.InitMetrics(registry))
	reasons := func() []string {
		return seriesLabels(t, registry, constants.LLMSMModelScalingBlocked, constants.LabelReason)
	}
	request := func(prc float64, reason string) *allocation.ModelScalingRequest {
		e := satOnlyEngine(&domain.AnalyzerResult{
			TotalDemand:       800,
			VariantCapacities: []domain.VariantCapacity{{VariantName: "v1", ReplicaCount: 1, PerReplicaCapacity: prc, Reason: reason}},
		})
		req, err := e.collectV2ModelRequest(context.Background(), "rsr-m", "rsr-ns", nil, scaleCfg, nil, nil, nil, nil, nil, 0)
		require.NoError(t, err)
		return req
	}

	reportSignalReason("rsr-ns", "rsr-m", request(0, allocation.ReasonNoData))
	assert.Equal(t, []string{constants.ScalingBlockedNoCompositeSignal}, reasons())

	reportSignalReason("rsr-ns", "rsr-m", nil)
	assert.Empty(t, reasons(), "a cycle with no composite clears it")

	reportSignalReason("rsr-ns", "rsr-m", request(0, allocation.ReasonNoData))
	reportSignalReason("rsr-ns", "rsr-m", request(200, "measured"))
	assert.Empty(t, reasons(), "a usable signal clears it")
}
