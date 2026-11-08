package config

import (
	"strings"
	"testing"
)

// A retention shorter than 30s is unusable like a typo: it would park a model
// between two requests and wake it on the next, cycling a whole node's GPUs.
// It is ignored, so the layer below applies.
func TestResetUnusableFieldsClearsTooShortRetention(t *testing.T) {
	for _, short := range []string{"1s", "10s", "29s", "29999ms"} {
		p := ScalingPolicy{ScaleToZero: &ScaleToZeroEnvelope{RetentionPeriod: short}}
		ignored := p.ResetUnusableFields()
		if len(ignored) != 1 || p.ScaleToZero.RetentionPeriod != "" {
			t.Errorf("retentionPeriod %q: ignored = %+v, kept %q; want it cleared and reported",
				short, ignored, p.ScaleToZero.RetentionPeriod)
		}
	}
	for _, ok := range []string{"30s", "1m"} {
		p := ScalingPolicy{ScaleToZero: &ScaleToZeroEnvelope{RetentionPeriod: ok}}
		if ignored := p.ResetUnusableFields(); len(ignored) != 0 {
			t.Errorf("retentionPeriod %q reported as unusable: %+v", ok, ignored)
		}
	}
}

// A tenant-written value is reported on every read, in the log, an Event and
// the status. It is truncated where it is captured, and the reason does not
// repeat it in full.
func TestIgnoredRetentionValueIsTruncated(t *testing.T) {
	long := strings.Repeat("x", 5000)
	p := ScalingPolicy{ScaleToZero: &ScaleToZeroEnvelope{RetentionPeriod: long}}
	ignored := p.ResetUnusableFields()
	if len(ignored) != 1 {
		t.Fatalf("ignored = %d fields, want 1", len(ignored))
	}
	if got := len(ignored[0].Value); got > 64+32 {
		t.Errorf("reported value is %d bytes, want at most 64 plus a short suffix", got)
	}
	if !strings.Contains(ignored[0].Value, "more bytes") {
		t.Errorf("truncated value %.80q... does not say it was cut", ignored[0].Value)
	}
	if s := ignored[0].String(); len(s) > 300 {
		t.Errorf("reported field is %d bytes: %.120s...", len(s), s)
	}
}
