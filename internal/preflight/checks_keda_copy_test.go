package preflight

import (
	"context"
	"strings"
	"testing"
)

// On the one-manifest install the controller writes KEDA's copy itself and
// there is no marker: keda.mtls reads the controller's status instead, and
// never tells a person to copy the certificate by hand.
func TestKEDAMTLSWrittenByController(t *testing.T) {
	cases := []struct {
		name   string
		status map[string]string
		want   Status
		detail string
	}{
		{"mutual TLS, copy current", map[string]string{statusScalerTLS: "mutual TLS: ...", statusKEDACopyStale: "false"}, Pass, "keeps KEDA's copy"},
		{"plaintext", map[string]string{statusScalerTLS: "plaintext: the controller could not write KEDA's copy"}, Fail, "plaintext"},
		{"copy stale", map[string]string{statusScalerTLS: "mutual TLS: ...", statusKEDACopyStale: "true"}, Fail, "does not hold the current"},
		{"KEDA refused", map[string]string{statusScalerTLS: "mutual TLS: ...", statusScalerUnreachable: "bad certificate"}, Fail, "bad certificate"},
		{"not said yet", map[string]string{}, Unknown, "has not said"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.remove("ConfigMap", KEDAMarkerConfigMap)
			f.remove("ConfigMap", ControllerStatusConfigMap)
			f.add(cm(testNS, ControllerStatusConfigMap, tc.status))
			env := f.env(t)
			env.ControllerWritesKEDACopy = true
			r := only(t, Run(context.Background(), env), "keda.mtls")
			if r.Status != tc.want || !strings.Contains(r.Detail, tc.detail) {
				t.Errorf("got %s %q, want %s containing %q", r.Status, r.Detail, tc.want, tc.detail)
			}
			if strings.Contains(r.Fix, "kubectl create secret generic") {
				t.Errorf("tells a person to copy the certificate the controller writes:\n%s", r.Fix)
			}
		})
	}

	// Negative control: the same status without the switch is judged by the
	// marker, which this install does not have.
	f := newFixture(t)
	f.remove("ConfigMap", KEDAMarkerConfigMap)
	if r := only(t, f.run(t), "keda.mtls"); r.Status != Warn {
		t.Errorf("without ControllerWritesKEDACopy and no marker: %s %q, want WARN", r.Status, r.Detail)
	}
}
