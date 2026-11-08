package steadystate

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/registration"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// ttftGroups is one model per name, in namespace ns, its variant naming tier.
func ttftGroups(ns, tier string, models ...string) map[string][]llmdvariant.Variant {
	out := map[string][]llmdvariant.Variant{}
	for _, m := range models {
		va := llmdvariant.Variant{}
		va.Name, va.Namespace = m+"-v", ns
		va.Spec.ModelID, va.Spec.ScalingPolicy = m, tier
		out[ns+"|"+m] = append(out[ns+"|"+m], va)
	}
	return out
}

func ttftPolicyEngine(policies map[string]config.ScalingPolicy) *Engine {
	cfg := config.NewTestConfig()
	cfg.UpdateScalingPolicyConfig(policies)
	return &Engine{Config: cfg, analyzers: testAnalyzers(nil)}
}

func sloPolicy(ttft bool) config.ScalingPolicy {
	p := config.ScalingPolicy{Analyzers: []config.AnalyzerScoreConfig{{Name: domain.SLOAnalyzerName}}}
	p.SLO = &config.SLOPolicy{TPOT: &config.SLOObjective{Target: config.SLODuration{Duration: 50 * time.Millisecond}}}
	if ttft {
		p.SLO.TTFT = &config.SLOObjective{Target: config.SLODuration{Duration: time.Second}}
	}
	return p
}

// A namespace is named by the policy its models resolve to: a target with the
// analyzer that reads it, by the default entry or a tier, and not for a TPOT
// target alone or a target nothing reads.
func TestTTFTNamespaces(t *testing.T) {
	unlisted := sloPolicy(true)
	unlisted.Analyzers = nil
	cases := []struct {
		name     string
		policies map[string]config.ScalingPolicy
		tier     string
		want     bool
	}{
		{"a target in the default entry", map[string]config.ScalingPolicy{"default": sloPolicy(true)}, "", true},
		{"a TPOT target alone", map[string]config.ScalingPolicy{"default": sloPolicy(false)}, "", false},
		{"no policy", map[string]config.ScalingPolicy{"default": {}}, "", false},
		{"a target with the analyzer unlisted", map[string]config.ScalingPolicy{"default": unlisted}, "", false},
		{"a tier's target", map[string]config.ScalingPolicy{"default": sloPolicy(false), "gold": sloPolicy(true)}, "gold", true},
		{"another tier's target", map[string]config.ScalingPolicy{"default": sloPolicy(false), "gold": sloPolicy(true)}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ttftPolicyEngine(tc.policies).ttftNamespaces(ttftGroups("ns", tc.tier, "m", "m2"))
			assert.Equal(t, tc.want, got["ns"])
			assert.Len(t, got, map[bool]int{true: 1, false: 0}[tc.want])
		})
	}
}

// One model with a target names its namespace for its neighbours, in either
// map order, and no other.
func TestTTFTNamespaces_OneModelIsEnough(t *testing.T) {
	e := ttftPolicyEngine(map[string]config.ScalingPolicy{"default": sloPolicy(false), "gold": sloPolicy(true)})
	groups := ttftGroups("ns", "gold", "m")
	for k, v := range ttftGroups("ns", "", "m2", "m3") {
		groups[k] = v
	}
	for k, v := range ttftGroups("other", "", "m4") {
		groups[k] = v
	}
	for range 20 {
		assert.Equal(t, map[string]bool{"ns": true}, e.ttftNamespaces(groups))
	}
}

// The engine opens the collector's cycle with those namespaces: their
// collection asks the three queries with no recording on, and the record-only
// ones stay off.
func TestBeginCollectorCycle_NamesTheTTFTNamespaces(t *testing.T) {
	src := &askedSource{}
	e := ttftPolicyEngine(map[string]config.ScalingPolicy{"default": sloPolicy(true)})
	e.ReplicaMetricsCollector = collector.NewReplicaMetricsCollector(src, nil, nil, nil, nil)

	e.beginCollectorCycle(ttftGroups("ns", "", "m"))
	defer e.ReplicaMetricsCollector.EndCycle()
	_, err := e.ReplicaMetricsCollector.CollectReplicaMetrics(context.Background(), "m", "ns", nil, nil, nil)
	assert.NoError(t, err)
	for _, q := range []string{registration.QueryQueueMin, registration.QueryComputedTokensBuckets, registration.QueryAvgKVTransferTime} {
		assert.Equal(t, 1, src.asked[q], q)
	}
	assert.Zero(t, src.asked[registration.QueryAvgPrefillTime], "record-only")
}
