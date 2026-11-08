package metrics

import (
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

// The start estimate is published under one source at a time: default, seed
// or measured. A move to another source deletes the others, so a query
// without a source selector reads one figure, never two added.
func TestReplicaStartSecondsEstimateKeepsOneSource(t *testing.T) {
	_, registry := engineConfigEmitter(t)
	series := func(source string) (float64, bool) {
		return seriesValue(t, registry, constants.LLMSMReplicaStartSecondsEstimate, map[string]string{
			constants.LabelVariantName: "v", constants.LabelStartSource: source,
		})
	}
	for _, step := range []struct {
		source  string
		seconds float64
	}{
		{ReplicaStartSourceDefault, 70},
		{ReplicaStartSourceSeed, 120},
		{ReplicaStartSourceBlended, 233.3},
		{ReplicaStartSourceMeasured, 460},
		{ReplicaStartSourceDefault, 70},
	} {
		SetReplicaStartSecondsEstimate("ns", "v", step.seconds, step.source)
		// Every source, listed here rather than read from replicaStartSources:
		// a source left off that list is a child never deleted.
		for _, s := range []string{ReplicaStartSourceDefault, ReplicaStartSourceSeed,
			ReplicaStartSourceBlended, ReplicaStartSourceMeasured} {
			v, ok := series(s)
			if s == step.source {
				if !ok || v != step.seconds {
					t.Fatalf("after %s: %s series = %v (present %v), want %v", step.source, s, v, ok, step.seconds)
				}
				continue
			}
			if ok {
				t.Fatalf("after %s: the %s series is still published (%v)", step.source, s, v)
			}
		}
	}
}

// A fresh registry forgets what source a variant was published under: after
// InitMetrics, a call keeps nothing from before, and a series of another
// source set on the new registry is deleted by the first call.
func TestReplicaStartSecondsEstimateAfterInit(t *testing.T) {
	_, _ = engineConfigEmitter(t)
	SetReplicaStartSecondsEstimate("ns", "x", 90, ReplicaStartSourceMeasured)
	_, registry := engineConfigEmitter(t)
	stray := variantSeriesLabels("ns", "x")
	stray[constants.LabelStartSource] = ReplicaStartSourceSeed
	replicaStartSecondsEstimate.With(stray).Set(1)
	SetReplicaStartSecondsEstimate("ns", "x", 91, ReplicaStartSourceMeasured)
	if _, ok := seriesValue(t, registry, constants.LLMSMReplicaStartSecondsEstimate, map[string]string{
		constants.LabelVariantName: "x", constants.LabelStartSource: ReplicaStartSourceSeed,
	}); ok {
		t.Fatal("after InitMetrics the first call kept another source's child")
	}
}

// The other sources are deleted when the source changes, not on every call:
// a call that keeps the source leaves a child of another source alone (here
// one set behind the setter's back), and the next change deletes it.
func TestReplicaStartSecondsEstimateDeletesOnlyOnAChange(t *testing.T) {
	_, registry := engineConfigEmitter(t)
	present := func(source string) bool {
		_, ok := seriesValue(t, registry, constants.LLMSMReplicaStartSecondsEstimate, map[string]string{
			constants.LabelVariantName: "w", constants.LabelStartSource: source,
		})
		return ok
	}
	SetReplicaStartSecondsEstimate("ns", "w", 70, ReplicaStartSourceDefault)
	stray := variantSeriesLabels("ns", "w")
	stray[constants.LabelStartSource] = ReplicaStartSourceSeed
	replicaStartSecondsEstimate.With(stray).Set(1)
	SetReplicaStartSecondsEstimate("ns", "w", 71, ReplicaStartSourceDefault)
	if !present(ReplicaStartSourceSeed) {
		t.Fatal("an unchanged source deleted the other children")
	}
	SetReplicaStartSecondsEstimate("ns", "w", 90, ReplicaStartSourceMeasured)
	if present(ReplicaStartSourceSeed) || present(ReplicaStartSourceDefault) {
		t.Fatal("a change of source left the other children")
	}
}
