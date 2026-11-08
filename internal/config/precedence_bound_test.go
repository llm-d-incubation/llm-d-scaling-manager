package config

import "testing"

// A priority or score above MaxPrecedence is cleared and reported. Finite but
// huge, it passed: a priority of 1e308 overflowed priority x demand to +Inf.
func TestResetUnusableFieldsClearsAPrecedenceAboveTheMax(t *testing.T) {
	p := ScalingPolicy{
		Priority:  1e308,
		Analyzers: []AnalyzerScoreConfig{{Name: "saturation", Score: 2e6}, {Name: "throughput", Score: MaxPrecedence}},
	}
	ignored := p.ResetUnusableFields()
	if len(ignored) != 2 || ignored[0].Field != "priority" || ignored[1].Field != "analyzers[0].score" {
		t.Fatalf("ignored = %+v, want priority and analyzers[0].score", ignored)
	}
	if ignored[0].Reason != errAboveMaxPrecedence || ignored[0].Value != "1e+308" {
		t.Errorf("priority reported as %q: %v", ignored[0].Value, ignored[0].Reason)
	}
	if p.Priority != 0 || p.Analyzers[0].Score != 0 {
		t.Errorf("priority %v, score %v; want both cleared", p.Priority, p.Analyzers[0].Score)
	}
	if p.Analyzers[1].Score != MaxPrecedence {
		t.Errorf("a score at the bound changed to %v", p.Analyzers[1].Score)
	}

	at := ScalingPolicy{Priority: MaxPrecedence}
	if ignored := at.ResetUnusableFields(); len(ignored) != 0 {
		t.Errorf("a priority at the bound was reported: %+v", ignored)
	}
}
