package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
)

// An ignored field is reported the way a rejected entry and an unknown key
// are: a Warning ConfigFieldIgnored Event on the ConfigMap, a
// llmsm_config_ignored_fields gauge for it that a deleted ConfigMap clears, and a
// log line that names the ConfigMap and carries the error_type text the
// LLMSMHighErrorRate runbook says to grep for. A tenant-written value is cut
// short, and a ConfigMap with many ignored fields lists ten and counts the rest.
func TestIgnoredFieldIsReportedLikeAnUnknownKey(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		t.Fatal(err)
	}
	f := newRejectsFixture(t)
	var logged strings.Builder
	ctx := log.IntoContext(context.Background(), funcr.New(func(prefix, args string) {
		logged.WriteString(args + "\n")
	}, funcr.Options{}))

	long := strings.Repeat("9", 4000)
	data := map[string]string{
		config.GlobalDefaultsKey: quotaDefault + "scaleToZero:\n  retentionPeriod: " + long + "x\n",
	}
	for i := range 12 {
		data[fmt.Sprintf("tier-%02d", i)] = "shapeChangeHoldSeconds: -1\n"
	}
	key := types.NamespacedName{Namespace: rejectsSystemNS, Name: config.ScalingPolicyConfigMapName()}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}, Data: data}
	if err := f.c.Create(ctx, cm); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}

	if got := gaugeValue(t, registry, "llmsm_config_ignored_fields"); got != 13 {
		t.Errorf("llmsm_config_ignored_fields = %v, want 13", got)
	}
	var fieldEvents []string
	for _, e := range f.events() {
		if strings.Contains(e, "Warning ConfigFieldIgnored") {
			fieldEvents = append(fieldEvents, e)
		}
	}
	if len(fieldEvents) != 11 || !strings.Contains(fieldEvents[10], "+3 more") {
		t.Errorf("want 10 ConfigFieldIgnored events and one counting 3 more, got %d: %v", len(fieldEvents), fieldEvents)
	}
	out := logged.String()
	if !strings.Contains(out, "Ignored unusable saturation scaling config field") {
		t.Errorf("no log line carries the error_type text the runbook greps for:\n%s", out)
	}
	if !strings.Contains(out, `"configMap"="`+key.Name+`"`) || !strings.Contains(out, `"namespace"="`+key.Namespace+`"`) {
		t.Errorf("the ignored-field log lines do not name the ConfigMap:\n%s", out)
	}
	if strings.Contains(out, long) || strings.Contains(strings.Join(fieldEvents, "\n"), long) {
		t.Error("a 4000-byte tenant value was reported in full")
	}

	// Fixed: the count drops to 0, a reading, not an absent series.
	cm.Data = map[string]string{config.GlobalDefaultsKey: quotaDefault}
	if err := f.c.Update(ctx, cm); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if got := gaugeValue(t, registry, "llmsm_config_ignored_fields"); got != 0 {
		t.Errorf("llmsm_config_ignored_fields = %v after the fields were fixed, want 0", got)
	}

	// A deleted ConfigMap's count is not reported on.
	if err := f.c.Delete(ctx, cm); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if got := gaugeValue(t, registry, "llmsm_config_ignored_fields"); got != -1 {
		t.Errorf("llmsm_config_ignored_fields still reported after the ConfigMap was deleted: %v", got)
	}
}
