package steadystate

import (
	"bytes"
	"context"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/registration"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
)

// routerAndPoolSource answers the router latency and flow-control pool
// queries for model "m"; every other query has no series.
type routerAndPoolSource struct{ emptyMetricsSource }

func (routerAndPoolSource) Refresh(_ context.Context, spec source.RefreshSpec) (map[string]*source.MetricResult, error) {
	model := map[string]string{"target_model_name": "m"}
	with := func(kv ...string) map[string]string {
		out := map[string]string{"target_model_name": "m", "inference_pool": "pool"}
		for i := 0; i+1 < len(kv); i += 2 {
			out[kv[i]] = kv[i+1]
		}
		return out
	}
	answers := map[string][]source.MetricValue{
		registration.QuerySLORouterTTFTBuckets:      {{Labels: map[string]string{"target_model_name": "m", "le": "+Inf"}, Value: 2}},
		registration.QuerySLORouterTPOTSum:          {{Labels: model, Value: 0.1}},
		registration.QuerySLORouterTPOTCount:        {{Labels: model, Value: 2}},
		registration.QuerySLORouterTPOTBuckets:      {{Labels: map[string]string{"target_model_name": "m", "le": "+Inf"}, Value: 2}},
		registration.QueryFlowControlPoolModels:     {{Labels: with(), Value: 0}},
		registration.QueryFlowControlPoolSaturation: {{Labels: with("stage", "decode"), Value: 0.5}},
	}
	out := make(map[string]*source.MetricResult, len(spec.Queries))
	for _, q := range spec.Queries {
		out[q] = &source.MetricResult{QueryName: q, Values: answers[q]}
	}
	return out, nil
}

// The input builder collects the router latency and the flow-control pools
// every cycle while a learner recording is on, whatever the policy lists, so
// the recording carries them for the TTFT model (analyzer-learner.md, 2.9).
// An engine with no collector leaves both nil.
func TestInputBuilder_CollectsTheRouterLatencyAndPoolsEveryCycle(t *testing.T) {
	policy := config.ScalingPolicy{ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.70}

	e := &Engine{analyzers: testAnalyzers(nil)}
	e.ReplicaMetricsCollector = collector.NewReplicaMetricsCollector(routerAndPoolSource{}, nil, nil, nil, nil)
	e.ReplicaMetricsCollector.BeginRecordingCycle(true)
	in := e.buildAnalyzerInput(context.Background(), "m", "ns", nil, policy, nil, nil, nil, nil, nil, 0)
	if in.RouterLatency == nil {
		t.Fatal("no router latency collected")
	}
	if got := in.RouterLatency.TPOTBuckets; len(got) != 1 || !math.IsInf(got[0].UpperBound, 1) || got[0].Rate != 2 {
		t.Errorf("TPOTBuckets = %v", got)
	}
	want := []domain.FlowControlPool{{WindowSeconds: registration.FlowControlWindowSeconds,
		DecodeSaturation: 0.5, DecodeSaturationKnown: true}}
	if !reflect.DeepEqual(in.FlowControlPools, want) {
		t.Errorf("FlowControlPools = %+v, want %+v", in.FlowControlPools, want)
	}

	none := &Engine{analyzers: testAnalyzers(nil)}
	in = none.buildAnalyzerInput(context.Background(), "m", "ns", nil, policy, nil, nil, nil, nil, nil, 0)
	if in.RouterLatency != nil || in.FlowControlPools != nil {
		t.Errorf("no collector: router %v, pools %v; want nil", in.RouterLatency, in.FlowControlPools)
	}

	// A router that exports nothing is nil, not a zero latency.
	silent := &Engine{analyzers: testAnalyzers(nil)}
	silent.ReplicaMetricsCollector = collector.NewReplicaMetricsCollector(emptyMetricsSource{}, nil, nil, nil, nil)
	silent.ReplicaMetricsCollector.BeginRecordingCycle(true)
	in = silent.buildAnalyzerInput(context.Background(), "m", "ns", nil, policy, nil, nil, nil, nil, nil, 0)
	if in.RouterLatency != nil || in.FlowControlPools != nil {
		t.Errorf("silent router: router %v, pools %v; want nil", in.RouterLatency, in.FlowControlPools)
	}
}

// askedSource is routerAndPoolSource that also notes every query it was
// asked for.
type askedSource struct {
	routerAndPoolSource
	mu    sync.Mutex
	asked map[string]int
}

func (s *askedSource) Refresh(ctx context.Context, spec source.RefreshSpec) (map[string]*source.MetricResult, error) {
	s.mu.Lock()
	if s.asked == nil {
		s.asked = map[string]int{}
	}
	for _, q := range spec.Queries {
		s.asked[q]++
	}
	s.mu.Unlock()
	return s.routerAndPoolSource.Refresh(ctx, spec)
}

// The recording gate (analyzer-learner.md, 2.9): the engine turns the
// record-only queries on for a cycle exactly when its recorder is writing
// (beginCollectorCycle). Off, building the input issues none of the router
// or flow-control queries; on, it issues each once; once the recorder has
// stopped, none again.
func TestInputBuilder_RouterAndPoolQueriesOnlyWhileRecording(t *testing.T) {
	policy := config.ScalingPolicy{ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.70}
	recordOnly := []string{registration.QuerySLORouterTTFTBuckets, registration.QuerySLORouterTPOTSum,
		registration.QuerySLORouterTPOTCount, registration.QuerySLORouterTPOTBuckets, registration.QueryFlowControlWaitBuckets,
		registration.QueryFlowControlPoolModels, registration.QueryFlowControlPoolSaturation, registration.QueryFlowControlOutcomes}

	cycle := func(e *Engine) {
		e.beginCollectorCycle(nil)
		defer e.ReplicaMetricsCollector.EndCycle()
		// Two models in one namespace: the second reads the memo.
		for _, model := range []string{"m", "m2"} {
			e.buildAnalyzerInput(context.Background(), model, "ns", nil, policy, nil, nil, nil, nil, nil, 0)
		}
	}

	src := &askedSource{}
	e := &Engine{analyzers: testAnalyzers(nil)}
	e.ReplicaMetricsCollector = collector.NewReplicaMetricsCollector(src, nil, nil, nil, nil)

	cycle(e)
	if len(src.asked) != 0 {
		t.Errorf("recording off: asked %v, want nothing", src.asked)
	}

	rec := learnrec.New(&bytes.Buffer{}, 0)
	e.learnRec = rec
	cycle(e)
	for _, q := range recordOnly {
		if src.asked[q] != 1 {
			t.Errorf("recording on: %s asked %d times, want once", q, src.asked[q])
		}
	}

	require.NoError(t, rec.Close())
	src.asked = nil
	cycle(e)
	if len(src.asked) != 0 {
		t.Errorf("recorder stopped: asked %v, want nothing", src.asked)
	}

	// On again, then a recorder that reaches its byte cap mid-run: the cycle
	// after it stops writing asks nothing.
	small := learnrec.New(&bytes.Buffer{}, 512)
	e.learnRec = small
	src.asked = nil
	cycle(e)
	if src.asked[registration.QueryFlowControlPoolModels] != 1 {
		t.Errorf("recording on again: asked %v", src.asked)
	}
	for i := 0; small.Active() && i < 100; i++ {
		small.Record("filler", time.Time{}, strings.Repeat("x", 256))
	}
	require.False(t, small.Active(), "precondition: the recorder reached its cap")
	src.asked = nil
	cycle(e)
	if len(src.asked) != 0 {
		t.Errorf("recorder at its cap: asked %v, want nothing", src.asked)
	}
}
