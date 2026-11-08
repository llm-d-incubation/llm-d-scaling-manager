package controller

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/test/testutil"
)

// badRetentionDefault is quotaDefault with a new threshold and a retention
// period that does not parse. The threshold tells the new entry from the
// previous one.
const badRetentionDefault = quotaDefault + "scaleUpThreshold: 0.91\n" +
	"scaleToZero:\n  retentionPeriod: 5 minutes\n"

// TestBadRetentionKeepsTheNewEntry: an entry whose only fault is an unusable
// retentionPeriod is cleared and KEPT. It must not be rejected, which would
// keep the previous entry instead and drop the operator's other edits.
func TestBadRetentionKeepsTheNewEntry(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		t.Fatal(err)
	}
	f := newRejectsFixture(t)
	f.cfg.MarkConfigMapsBootstrapComplete()

	f.apply(rejectsSystemNS, map[string]string{config.GlobalDefaultsKey: quotaDefault})
	_ = f.events()

	f.apply(rejectsSystemNS, map[string]string{config.GlobalDefaultsKey: badRetentionDefault})
	for _, e := range f.events() {
		if strings.Contains(e, constants.K8SEventConfigEntryRejected) {
			t.Errorf("an entry with only a bad retentionPeriod was rejected: %s", e)
		}
	}
	if got := gaugeValue(t, registry, constants.LLMSMConfigRejectedEntries); got != 0 {
		t.Errorf("%s = %v, want 0", constants.LLMSMConfigRejectedEntries, got)
	}
	entry, ok := f.cfg.ScalingPolicyEntry("", config.GlobalDefaultsKey)
	if !ok {
		t.Fatal("default entry missing after the edit")
	}
	if entry.ScaleUpThreshold != 0.91 {
		t.Errorf("scaleUpThreshold = %v, want 0.91 from the new entry, not the previous one", entry.ScaleUpThreshold)
	}
	if entry.ScaleToZero != nil && entry.ScaleToZero.RetentionPeriod != "" {
		t.Errorf("retentionPeriod = %q, want it cleared", entry.ScaleToZero.RetentionPeriod)
	}
	if got := limiterSet(f.cfg); got != string(config.LimiterTypeQuota) {
		t.Errorf("limiter mode %q, want quota", got)
	}
	if got := testutil.GetErrorMetricValue(registry, constants.ComponentController, ignoredFieldErrorType); got != 1 {
		t.Errorf("error metric for ignored fields = %v, want 1", got)
	}
}

// TestPolicyNamespaceIgnoredFieldsNotReported: only limiters and the optimizer
// are read from the policy namespace's ConfigMap, so a bad retentionPeriod
// there is not counted as an ignored field. Counting it would report a value
// that nothing reads.
func TestPolicyNamespaceIgnoredFieldsNotReported(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		t.Fatal(err)
	}
	f := newRejectsFixture(t)
	f.cfg.SetPolicyNamespace(rejectsPolicyNS, config.PolicySourceWellKnown)
	f.cfg.MarkConfigMapsBootstrapComplete()

	f.apply(rejectsPolicyNS, map[string]string{config.GlobalDefaultsKey: badRetentionDefault})
	if got := limiterSet(f.cfg); got != string(config.LimiterTypeQuota) {
		t.Fatalf("limiter mode %q, want quota from the policy namespace", got)
	}
	if got := testutil.GetErrorMetricValue(registry, constants.ComponentController, ignoredFieldErrorType); got != 0 {
		t.Errorf("error metric for ignored fields = %v after a policy-namespace load, want 0", got)
	}

	// The same entry in the controller's own ConfigMap is read, and counted.
	f.apply(rejectsSystemNS, map[string]string{config.GlobalDefaultsKey: badRetentionDefault})
	if got := testutil.GetErrorMetricValue(registry, constants.ComponentController, ignoredFieldErrorType); got != 1 {
		t.Errorf("error metric for ignored fields = %v after the controller's own load, want 1", got)
	}
}
