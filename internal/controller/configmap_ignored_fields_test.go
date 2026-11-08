package controller

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/test/testutil"
)

// A field cleared as unusable must be as visible as a rejected entry: returned
// by key for the status, and counted in the error metric. Only logging it left
// an operator who does not read the controller log with no sign that their
// retention or hold had no effect.
func TestParseReportsIgnoredFields(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics: %v", err)
	}

	data := map[string]string{
		"default": "scaleToZero:\n  retentionPeriod: 5 minutes\n" +
			"shapeChangeHoldSeconds: -10\n",
		"batch": "shapeChangeHoldSeconds: -1\n",
		"clean": "scaleUpThreshold: 0.9\n",
		// Rejected outright: reported in rejected only, never in ignored.
		"broken": "kvCacheThreshold: 1.5\nshapeChangeHoldSeconds: -3\n",
	}
	configs, count, rejected, ignored := parseScalingPolicyConfigRejects(data, logr.Discard())
	if count != 3 || len(configs) != 3 {
		t.Fatalf("kept %d entries, want default, batch and clean", count)
	}
	if _, ok := rejected["broken"]; !ok || len(rejected) != 1 {
		t.Fatalf("rejected = %v, want only the broken entry", rejected)
	}

	fields := func(key string) []string {
		out := make([]string, 0, len(ignored[key]))
		for _, f := range ignored[key] {
			out = append(out, f.Field)
		}
		return out
	}
	if got := fields(config.GlobalDefaultsKey); len(got) != 2 ||
		got[0] != "scaleToZero.retentionPeriod" || got[1] != "shapeChangeHoldSeconds" {
		t.Errorf("default entry ignored = %v, want retentionPeriod and shapeChangeHoldSeconds", got)
	}
	if got := fields("batch"); len(got) != 1 || got[0] != "shapeChangeHoldSeconds" {
		t.Errorf("batch entry ignored = %v, want shapeChangeHoldSeconds", got)
	}
	if _, ok := ignored["clean"]; ok {
		t.Errorf("clean entry reported ignored fields: %v", ignored["clean"])
	}
	if _, ok := ignored["broken"]; ok {
		t.Errorf("rejected entry reported ignored fields too: %v", ignored["broken"])
	}
	if len(ignored) != 2 {
		t.Errorf("ignored has %d keys, want 2: %v", len(ignored), ignored)
	}

	// Parsing alone counts nothing: the policy namespace's ConfigMap is parsed
	// the same way, and its ignored fields are not reported.
	if got := testutil.GetErrorMetricValue(registry, constants.ComponentController, ignoredFieldErrorType); got != 0 {
		t.Errorf("error metric after parsing alone = %v, want 0", got)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "cm"}}
	(&ConfigMapReconciler{}).reportIgnoredFields(context.Background(), cm, ignored)

	// One count per ignored field of a kept entry: two in default, one in
	// batch. The rejected entry's field is not counted; that entry is counted
	// as rejected instead.
	if got := testutil.GetErrorMetricValue(registry, constants.ComponentController, ignoredFieldErrorType); got != 3 {
		t.Errorf("error metric for ignored fields = %v, want 3", got)
	}
}
