package config

import (
	"math"
	"slices"
	"testing"
)

// A NaN or infinite number is cleared at load and reported, and the entry
// still validates. NaN passed every range check, and an infinite priority or
// score ordered the greedy-by-score fair share backwards (#171).
func TestResetUnusableFieldsClearsNonFiniteNumbers(t *testing.T) {
	want := []string{
		"priority", "kvCacheThreshold", "queueLengthThreshold", "scaleUpThreshold", "scaleDownBoundary",
		"analyzers[0].score", "analyzers[0].scaleUpThreshold", "analyzers[0].scaleDownBoundary",
	}
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		up, down := v, v
		p := ScalingPolicy{
			Priority: v, KvCacheThreshold: v, QueueLengthThreshold: v, ScaleUpThreshold: v, ScaleDownBoundary: v,
			Analyzers: []AnalyzerScoreConfig{{Name: "saturation", Score: v, ScaleUpThreshold: &up, ScaleDownBoundary: &down}},
		}
		ignored := p.ResetUnusableFields()
		fields := make([]string, 0, len(ignored))
		for _, f := range ignored {
			fields = append(fields, f.Field)
			if f.Reason != errNotFinite {
				t.Errorf("%v: %s reason %v", v, f.Field, f.Reason)
			}
		}
		if !slices.Equal(fields, want) {
			t.Errorf("%v: ignored %v, want %v", v, fields, want)
		}
		a := p.Analyzers[0]
		if p.Priority != 0 || p.KvCacheThreshold != 0 || p.QueueLengthThreshold != 0 || p.ScaleUpThreshold != 0 ||
			p.ScaleDownBoundary != 0 || a.Score != 0 || a.ScaleUpThreshold != nil || a.ScaleDownBoundary != nil {
			t.Errorf("%v: not every field cleared: %+v", v, p)
		}
		p.ApplyDefaults()
		if err := p.Validate(); err != nil {
			t.Errorf("%v: entry rejected after reset: %v", v, err)
		}
		if p.Priority != DefaultPriority || p.AnalyzerScore("saturation") != 1 {
			t.Errorf("%v: priority %v, score %v; want the defaults", v, p.Priority, p.AnalyzerScore("saturation"))
		}
	}

	up, down := 0.9, 0.6
	finite := ScalingPolicy{
		Priority: 5, KvCacheThreshold: 0.8, QueueLengthThreshold: 5, ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.7,
		Analyzers: []AnalyzerScoreConfig{{Name: "saturation", Score: MaxPrecedence, ScaleUpThreshold: &up, ScaleDownBoundary: &down}},
	}
	before := finite
	if ignored := finite.ResetUnusableFields(); len(ignored) != 0 {
		t.Errorf("finite values reported as unusable: %+v", ignored)
	}
	if finite.Priority != before.Priority || finite.Analyzers[0].Score != MaxPrecedence || *finite.Analyzers[0].ScaleUpThreshold != 0.9 {
		t.Errorf("finite values changed: %+v", finite)
	}
}

// A negative analyzer score is cleared and reported. AnalyzerScore read it as
// 1 without a word.
func TestResetUnusableFieldsClearsNegativeScore(t *testing.T) {
	p := ScalingPolicy{Analyzers: []AnalyzerScoreConfig{{Name: "saturation", Score: 2}, {Name: "throughput", Score: -5}}}
	ignored := p.ResetUnusableFields()
	if len(ignored) != 1 || ignored[0].Field != "analyzers[1].score" || ignored[0].Value != "-5" {
		t.Fatalf("ignored = %+v, want one analyzers[1].score entry", ignored)
	}
	if p.Analyzers[1].Score != 0 || p.Analyzers[0].Score != 2 {
		t.Errorf("scores = %v, %v; want 2 kept and -5 cleared", p.Analyzers[0].Score, p.Analyzers[1].Score)
	}
}

// AnalyzerScore never returns an infinite score, also for a policy that did
// not come through the ConfigMap.
func TestAnalyzerScoreIgnoresInfinity(t *testing.T) {
	p := ScalingPolicy{Analyzers: []AnalyzerScoreConfig{{Name: "saturation", Score: math.Inf(1)}}}
	if got := p.AnalyzerScore("saturation"); got != 1 {
		t.Errorf("AnalyzerScore = %v, want 1", got)
	}
}
