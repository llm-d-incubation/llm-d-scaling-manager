package alerts

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The two node-read outcomes page differently. A gpu-inventory limiter declared
// alone without node read scales nothing up: llmsm_node_access_denied, critical.
// Beside a quota the physical limiter is only skipped and the quota still
// bounds; the inventory no longer raises llmsm_node_access_denied there, so the
// skip needs an alert of its own, and it must not page as critical.
func TestNodeReadDenialPagesCriticalOnlyForAPhysicalLimiterAlone(t *testing.T) {
	rule := loadRule(t)
	severities := map[string][]string{}
	for _, group := range rule.Spec.Groups {
		for _, r := range group.Rules {
			if r.Alert == "" {
				continue
			}
			for _, name := range ExtractMetricNames(r.Expr) {
				severities[name] = append(severities[name], r.Alert+"="+r.Labels["severity"])
			}
		}
	}

	denied := severities["llmsm_node_access_denied"]
	assert.NotEmpty(t, denied, "no alert on llmsm_node_access_denied")
	for _, s := range denied {
		assert.Contains(t, s, "=critical", "a physical limiter alone without node read scales nothing up: %s", s)
	}

	skipped := severities["llmsm_constraint_provider_skipped"]
	assert.NotEmpty(t, skipped,
		"no alert on llmsm_constraint_provider_skipped: a gpu-inventory limiter skipped beside a quota would go unalerted")
	for _, s := range skipped {
		assert.False(t, strings.HasSuffix(s, "=critical"),
			"a skipped provider leaves the other limiters bounding; it must not page as critical: %s", s)
	}
}

// SR-M4: the utilization-share conditions that hold until someone changes
// something page; each names its reason and its runbook, and none is critical
// (the fleet still scales under today's optimizer or holds what it runs).
func TestUtilizationShareAlertsShip(t *testing.T) {
	for name, want := range map[string]struct{ metric, matcher, forDur string }{
		"LLMSMUtilizationShareMarksUnreadable":   {"llmsm_model_scaling_blocked", `{reason="marks-unreadable"}`, "10m"},
		"LLMSMUtilizationShareFloorsExceedQuota": {"llmsm_model_scaling_blocked", `{reason="floors-exceed-quota"}`, "15m"},
		"LLMSMUtilizationShareModeInvalid":       {"llmsm_utilization_share_mode", `{mode="invalid"}`, "10m"},
	} {
		a := findAlert(t, name)
		assert.Equal(t, []string{want.metric}, ExtractMetricNames(a.Expr), name)
		assert.Contains(t, a.Expr, want.matcher, name)
		assert.Contains(t, a.Expr, "max by (", "%s: two controller pods must not page twice", name)
		assert.Equal(t, want.forDur, a.For, name)
		assert.Equal(t, "warning", a.Severity, name)
		assert.True(t, strings.HasSuffix(a.Runbook, "#the-optimizer-evaluates-but-never-moves-anything"), name)
	}
}

// One cause pages once, at critical: a parked model refusing requests pages
// through LLMSMModelParkedWhileRequestsRefused whatever the cause, and the
// no-wake-signal warning stays quiet while that condition holds.
func TestNoWakeSignalQuietWhileParkedAndRefused(t *testing.T) {
	parked := findAlert(t, "LLMSMModelParkedWhileRequestsRefused")
	assert.Equal(t, "critical", parked.Severity)
	assert.NotContains(t, parked.Expr, "no-wake-signal", "the critical page must not leave any cause out")
	assert.NotContains(t, parked.Expr, "unless", "the critical page must not leave any cause out")

	noWake := findAlert(t, "LLMSMModelHasNoWakeSignal")
	assert.Equal(t, "warning", noWake.Severity)
	assert.Equal(t, "("+`llmsm_model_scaling_blocked{reason="no-wake-signal"} == 1`+") unless on (exported_namespace, model_name) ("+parked.Expr+")", noWake.Expr,
		"the warning must be quiet exactly while the critical alert's condition holds")
}
