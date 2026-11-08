package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
)

// inertThresholdsPolicy sets a band on saturation (which decides), on an
// enabled throughput entry, and on a disabled external entry.
var inertThresholdsPolicy = map[string]string{
	config.GlobalDefaultsKey: "analyzers:\n  - name: saturation\n    scaleUpThreshold: 0.8\n" +
		"  - name: throughput\n    scaleUpThreshold: 0.9\n" +
		"  - name: queue\n    type: external\n    enabled: false\n    scaleUpThreshold: 0.9\n",
}

// inertWarnings runs handle on the scaling-policy ConfigMap in namespace and
// returns the inert-threshold warnings it logged.
func inertWarnings(t *testing.T, namespace string, handle func(ctx context.Context, cm *corev1.ConfigMap) error) []string {
	t.Helper()
	var lines []string
	ctx := log.IntoContext(context.Background(), funcr.New(func(_, args string) {
		if strings.Contains(args, config.InertAnalyzerThresholdsMessage) {
			lines = append(lines, args)
		}
	}, funcr.Options{}))
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: config.ScalingPolicyConfigMapName()},
		Data:       inertThresholdsPolicy,
	}
	if err := handle(ctx, cm); err != nil {
		t.Fatal(err)
	}
	return lines
}

// A band on an enabled analyzer other than saturation is accepted but does
// not move the decision, so the scaling-policy read warns once, naming the
// entry by its name. Saturation's band decides and a disabled entry sizes
// nothing, so neither warns; the cluster-policy read takes only limiters and
// the optimizer, so it does not warn at all.
func TestInertAnalyzerThresholdsWarnWhereBandsAreRead(t *testing.T) {
	f := newRejectsFixture(t)
	got := inertWarnings(t, rejectsSystemNS, func(ctx context.Context, cm *corev1.ConfigMap) error {
		return f.r.handleScalingPolicyConfigMap(ctx, cm, rejectsSystemNS, true)
	})
	if len(got) != 1 {
		t.Fatalf("want one warning, for throughput; got %d: %v", len(got), got)
	}
	for _, want := range []string{`"key"="default"`, `"analyzer"="throughput"`} {
		if !strings.Contains(got[0], want) {
			t.Errorf("warning %q lacks %s", got[0], want)
		}
	}

	f = newRejectsFixture(t)
	f.cfg.SetPolicyNamespace(rejectsPolicyNS, config.PolicySourceWellKnown)
	if got := inertWarnings(t, rejectsPolicyNS, f.r.handleClusterPolicyConfigMap); len(got) != 0 {
		t.Errorf("the cluster-policy read warned: %v", got)
	}
}
