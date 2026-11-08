package alerts

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// kueueAlert is the named alert's rule, failing if it is absent.
type kueueAlert struct {
	Expr, For, Severity, Summary, Description, Runbook string
}

func findAlert(t *testing.T, name string) kueueAlert {
	t.Helper()
	for _, group := range loadRule(t).Spec.Groups {
		for _, r := range group.Rules {
			if r.Alert == name {
				return kueueAlert{Expr: r.Expr, For: r.For, Severity: r.Labels["severity"],
					Summary: r.Annotations["summary"], Description: r.Annotations["description"],
					Runbook: r.Annotations["runbook_url"]}
			}
		}
	}
	require.Failf(t, "alert not found", "no alert %q in the PrometheusRule", name)
	return kueueAlert{}
}

// R2-4: a failed Kueue read freezes every namespace of the entry, so the
// stale alert pages once per entry, counting namespaces, not once per
// namespace (200 pages for one fault).
func TestKueueReaderStalePagesOncePerEntry(t *testing.T) {
	a := findAlert(t, "LLMSMKueueReaderStale")
	assert.Equal(t, []string{"llmsm_quota_borrow_withheld"}, ExtractMetricNames(a.Expr))
	assert.Contains(t, a.Expr, `{reason="stale"}`)
	assert.Contains(t, a.Expr, "count by (limiter_name)")
	assert.Regexp(t, `>=\s*2\s*$`, a.Expr)
	assert.Equal(t, "5m", a.For)
	assert.Equal(t, "warning", a.Severity)
	assert.Contains(t, a.Summary, "$labels.limiter_name")
	assert.NotContains(t, a.Summary, "$labels.exported_namespace", "aggregated away")
	assert.True(t, strings.HasSuffix(a.Runbook, "#llmsmkueuereaderstale"))
}

// R8, R11, R2-3, R2-4: a namespace stale alone in its entry is named, and the
// description is right at the defaults: a failing ResourceQuota read turns into
// resourcequota-unreadable after five 30s refresh intervals, before 5 minutes.
func TestKueueReaderStaleNamespaceOnlyWhenAlone(t *testing.T) {
	a := findAlert(t, "LLMSMKueueReaderStaleNamespace")
	assert.Equal(t, []string{"llmsm_quota_borrow_withheld", "llmsm_quota_borrow_withheld"}, ExtractMetricNames(a.Expr))
	assert.Contains(t, a.Expr, "max by (limiter_name, exported_namespace)")
	assert.Contains(t, a.Expr, "and on (limiter_name)")
	assert.Regexp(t, `==\s*1\)\s*$`, a.Expr)
	assert.Equal(t, "5m", a.For)
	assert.Equal(t, "warning", a.Severity)
	assert.Contains(t, a.Summary, "$labels.exported_namespace")
	assert.Contains(t, a.Description, "before this alert fires", "the ResourceQuota cause is bounded at the defaults")
	assert.NotContains(t, a.Description, "fires alone", "the round-1 claim, impossible at the defaults")
	assert.True(t, strings.HasSuffix(a.Runbook, "#llmsmkueuereaderstale"))
}

// R3: Kueue reachable but its controller not running froze borrowing with no
// alert at all -- the version gauge reads 1 under assumeTestedVersion.
func TestKueueControllerDownHasItsOwnAlert(t *testing.T) {
	a := findAlert(t, "LLMSMKueueControllerDown")
	assert.Equal(t, []string{"llmsm_quota_borrow_withheld"}, ExtractMetricNames(a.Expr))
	assert.Contains(t, a.Expr, `{reason="kueue-controller-down"}`)
	assert.Equal(t, "warning", a.Severity)
	assert.Contains(t, a.Description, "KUEUE_NAMESPACE=", "how a namespace-scoped install gets the Role")
	assert.Contains(t, a.Description, "leader-election ID", "a custom Kueue leader-election ID reads as down (S5)")
	assert.True(t, strings.HasSuffix(a.Runbook, "#llmsmkueuecontrollerdown"))
}

// R3: an untested Kueue release withholds borrowing everywhere; it is said,
// at info, not left silent.
func TestKueueVersionUntestedIsInformational(t *testing.T) {
	a := findAlert(t, "LLMSMKueueVersionUntested")
	assert.Equal(t, []string{"llmsm_quota_borrow_withheld"}, ExtractMetricNames(a.Expr))
	assert.Contains(t, a.Expr, `{reason="kueue-version-untested"}`)
	assert.Equal(t, "info", a.Severity)
	assert.True(t, strings.HasSuffix(a.Runbook, "#llmsmkueueversionuntested"))
}

// R2: aggregated per controller instance, as one controller's view; a bare
// max() hid which one could not read the version.
func TestKueueVersionUnknownIsPerControllerInstance(t *testing.T) {
	a := findAlert(t, "LLMSMKueueVersionUnknown")
	assert.Contains(t, a.Expr, "max by (controller_instance) (llmsm_kueue_controller_version_known)")
	assert.Contains(t, a.Description, "assumeTestedVersion: true", "the documented fix")
}

// R2-5: a Deployment that cannot be read sets the version gauge to 0 and
// freezes borrowing as kueue-controller-down; one fault pages once, through
// the expression, matched per controller instance.
func TestKueueVersionUnknownIsQuietWhileControllerDown(t *testing.T) {
	a := findAlert(t, "LLMSMKueueVersionUnknown")
	assert.Contains(t, a.Expr, "unless on (controller_instance)")
	assert.Contains(t, a.Expr, `llmsm_quota_borrow_withheld{reason="kueue-controller-down"}`)
}

// R6: the drift alert compares Kueue's own figures with the scaling manager's
// reading of them; a second scaling manager cannot cause it.
func TestKueueBorrowDriftNamesNoImpossibleCause(t *testing.T) {
	a := findAlert(t, "LLMSMKueueBorrowDrift")
	assert.NotContains(t, a.Description, "second scaling manager")
}

// R11: the unplaced alert is per variant, names it, and waits past the
// placement timeout before paging.
func TestKueuePodsUnplacedAlert(t *testing.T) {
	a := findAlert(t, "LLMSMKueuePodsUnplaced")
	assert.Equal(t, []string{"llmsm_kueue_unplaced_pods"}, ExtractMetricNames(a.Expr))
	assert.Contains(t, a.Expr, "max by (exported_namespace, variant_name)")
	assert.Regexp(t, `>\s*0\s*$`, a.Expr)
	assert.Equal(t, "5m", a.For)
	assert.Equal(t, "warning", a.Severity)
	assert.Contains(t, a.Summary, "$labels.exported_namespace")
	assert.Contains(t, a.Summary, "$labels.variant_name")
	assert.True(t, strings.HasSuffix(a.Runbook, "#replicas-dropped-under-kueue-borrowing"))
}
