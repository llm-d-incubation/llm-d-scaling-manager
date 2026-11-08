package config

import "testing"

// An unusable scaleToZero.retentionPeriod is cleared at load and reported once,
// and the entry still validates: failing it would drop the whole entry.
func TestResetUnusableFieldsClearsBadRetentionPeriod(t *testing.T) {
	for _, bad := range []string{"5 minutes", "10", "0s", "-1m"} {
		p := ScalingPolicy{ScaleToZero: &ScaleToZeroEnvelope{RetentionPeriod: bad}}
		p.ApplyEntryDefaults()
		ignored := p.ResetUnusableFields()
		if len(ignored) != 1 || ignored[0].Field != "scaleToZero.retentionPeriod" || ignored[0].Value != bad {
			t.Errorf("retentionPeriod %q: ignored = %+v, want one scaleToZero.retentionPeriod entry", bad, ignored)
		}
		if p.ScaleToZero.RetentionPeriod != "" {
			t.Errorf("retentionPeriod %q was kept as %q, want it cleared", bad, p.ScaleToZero.RetentionPeriod)
		}
		if err := p.Validate(); err != nil {
			t.Errorf("retentionPeriod %q: entry rejected after reset: %v", bad, err)
		}
	}
	for _, good := range []string{"", "90s", "5m", "1h30m"} {
		p := ScalingPolicy{ScaleToZero: &ScaleToZeroEnvelope{RetentionPeriod: good}}
		p.ApplyEntryDefaults()
		if ignored := p.ResetUnusableFields(); len(ignored) != 0 {
			t.Errorf("retentionPeriod %q reported as unusable: %+v", good, ignored)
		}
		if p.ScaleToZero.RetentionPeriod != good {
			t.Errorf("retentionPeriod %q changed to %q", good, p.ScaleToZero.RetentionPeriod)
		}
	}
}

// A negative shapeChangeHoldSeconds is cleared, so a tier that sets one inherits
// the default entry's hold instead of replacing it with a value no getter honours.
func TestResetUnusableFieldsClearsNegativeShapeChangeHold(t *testing.T) {
	tier := ScalingPolicy{ShapeChangeHoldSeconds: -30}
	ignored := tier.ResetUnusableFields()
	if len(ignored) != 1 || ignored[0].Field != "shapeChangeHoldSeconds" || ignored[0].Value != "-30" {
		t.Fatalf("ignored = %+v, want one shapeChangeHoldSeconds entry", ignored)
	}

	set := ScalingPolicySet{
		GlobalDefaultsKey: {ShapeChangeHoldSeconds: 900},
		"long-context":    tier,
	}
	got := ResolveScalingPolicyForTier(set, "m", "ns", "long-context")
	if got.ShapeChangeHoldSeconds != 900 {
		t.Errorf("shapeChangeHoldSeconds = %d, want the default entry's 900", got.ShapeChangeHoldSeconds)
	}

	zero := ScalingPolicy{}
	if ignored := zero.ResetUnusableFields(); len(ignored) != 0 {
		t.Errorf("an unset hold was reported as unusable: %+v", ignored)
	}
}
