package controller

import (
	"strings"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
)

// reservedNameDefault is a default entry written before the limiter names
// were reserved: its one quota entry is named gpu-limiter.
const reservedNameDefault = `limiters:
  - type: quota
    name: gpu-limiter
    scope: cluster
    quotas: { H100: 8 }
`

// ev-shindin/llm-scaling-manager#216: the release that reserved the names only warned, so an upgrade would
// not stall. From v0.10.0 a quota entry with a reserved name is a validation
// error: the "default" entry holding it is rejected, and a controller starting
// on it refuses to start rather than start unbounded -- from the controller's
// own ConfigMap and from a policy namespace alike. The error names the entry
// and says to rename it, because that is the only fix.
func TestReservedLimiterNameAtStartupRefusesToStart(t *testing.T) {
	for _, tc := range []struct {
		name     string
		policyNS bool
	}{
		{"the controller's own ConfigMap", false},
		{"a separate policy namespace", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRejectsFixture(t)
			ns := rejectsSystemNS
			if tc.policyNS {
				f.cfg.SetPolicyNamespace(rejectsPolicyNS, config.PolicySourceWellKnown)
				ns = rejectsPolicyNS
			}
			err := f.bootstrapWith(policyCM(ns, reservedNameDefault))
			if err == nil {
				t.Fatalf("startup with a quota entry named gpu-limiter succeeded with limiters %q, want a refusal",
					f.cfg.EffectiveLimiterSummary())
			}
			for _, want := range []string{"refusing to start unbounded", `quota entry name "gpu-limiter" is reserved`, "rename the entry"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("startup error %q does not say %q", err, want)
				}
			}
		})
	}
}
