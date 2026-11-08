package e2e

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d-incubation/llm-d-scaling-manager/test/alerts"
)

const (
	prometheusRuleName  = "controller-manager-alerts"
	prometheusGroupName = "scaling-manager.rules"
)

// PrometheusRulesResponse represents the response from /api/v1/rules
type PrometheusRulesResponse struct {
	Status string `json:"status"`
	Data   struct {
		Groups []PrometheusRuleGroup `json:"groups"`
	} `json:"data"`
}

// PrometheusRuleGroup represents a rule group in Prometheus
type PrometheusRuleGroup struct {
	Name  string           `json:"name"`
	File  string           `json:"file"`
	Rules []PrometheusRule `json:"rules"`
}

// PrometheusRule represents a single rule in a group
type PrometheusRule struct {
	Name   string `json:"name"`
	Query  string `json:"query"`
	Health string `json:"health"`
	Type   string `json:"type"`
}

// scalingManagerMetricNames is derived from the constants that define the metrics, by the
// same helper the offline check in test/alerts uses. It used to be a slice
// maintained by hand, which is a second place to remember: llmsm_node_access_denied
// was missing from it from the day the metric was added, and the omission stayed
// invisible because this spec only runs with DEPLOY_ALERTING_RULES=true and a
// runtime Skip() reports as SUCCESS.
func scalingManagerMetricNames() map[string]bool {
	known, err := alerts.LLMSMMetricNames()
	Expect(err).NotTo(HaveOccurred(), "could not read the metric constants")
	return known
}

// queryPrometheusRules queries the in-cluster Prometheus /api/v1/rules endpoint.
// Returns the parsed response or an error.
func queryPrometheusRules() (*PrometheusRulesResponse, error) {
	prometheusURL := os.Getenv("PROMETHEUS_URL")
	GinkgoWriter.Printf("DEBUG: PROMETHEUS_URL env var = '%s'\n", prometheusURL)
	if prometheusURL == "" {
		// Default to in-cluster service URL if not set
		prometheusURL = fmt.Sprintf("https://kube-prometheus-stack-prometheus.%s.svc.cluster.local:9090", cfg.MonitoringNS)
		GinkgoWriter.Printf("DEBUG: Using default in-cluster URL = '%s'\n", prometheusURL)
	}

	rulesURL := prometheusURL + "/api/v1/rules"
	GinkgoWriter.Printf("DEBUG: Final rulesURL = '%s'\n", rulesURL)

	// Create HTTP client with TLS config for self-signed certs
	// Prometheus in e2e uses self-signed certs, so we skip verification
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 10 * time.Second,
	}

	resp, err := client.Get(rulesURL)
	if err != nil {
		return nil, fmt.Errorf("failed to query Prometheus rules: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status code %d: %s", resp.StatusCode, string(body))
	}

	var rulesResp PrometheusRulesResponse
	if err := json.NewDecoder(resp.Body).Decode(&rulesResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	if rulesResp.Status != "success" {
		return nil, fmt.Errorf("Prometheus API returned status: %s", rulesResp.Status)
	}

	return &rulesResp, nil
}

// PrometheusAlerts test suite validates the deployed PrometheusRule resource.
// This test:
// - Verifies PrometheusRule exists (deployed via install.sh)
// - Validates all expected alert rules are present with correct structure
// - Validates alert expressions reference only known the scaling manager metrics
//
// This test does NOT:
// - Create or delete PrometheusRule resources (expects them to be deployed)
// - Validate that alerts actually fire when conditions are met (would require metric injection)
var _ = Describe("PrometheusAlerts", Label("smoke"), Label("prometheus-alerts"), Ordered, func() {
	BeforeAll(func() {
		// Skip if DEPLOY_ALERTING_RULES is not set to true
		deployAlertingRules := os.Getenv("DEPLOY_ALERTING_RULES")
		if deployAlertingRules != "true" {
			Skip("DEPLOY_ALERTING_RULES not set to 'true' - skipping Prometheus alerts tests. " +
				"Set DEPLOY_ALERTING_RULES=true when running 'make deploy-e2e-infra' to enable these tests.")
		}
		GinkgoWriter.Println("✓ DEPLOY_ALERTING_RULES is set to 'true'")

		// Check if PrometheusRule CRD is available
		By("Checking if PrometheusRule CRD is available")
		_, err := k8sClient.Discovery().ServerResourcesForGroupVersion("monitoring.coreos.com/v1")
		Expect(err).NotTo(HaveOccurred(),
			"PrometheusRule CRD must be available. "+
				"Ensure Prometheus Operator is installed in the cluster.")
		GinkgoWriter.Println("✓ PrometheusRule CRD is available")
	})

	It("should have PrometheusRule deployed", func() {
		By("Verifying PrometheusRule exists")
		prometheusRule := &promoperator.PrometheusRule{}
		err := crClient.Get(ctx, client.ObjectKey{
			Name:      prometheusRuleName,
			Namespace: cfg.LLMSMNamespace,
		}, prometheusRule)
		Expect(err).NotTo(HaveOccurred(),
			"PrometheusRule must be deployed. "+
				"Ensure 'make deploy-e2e-infra' was run with DEPLOY_ALERTING_RULES=true.")

		GinkgoWriter.Printf("✓ PrometheusRule '%s' exists in namespace '%s'\n",
			prometheusRuleName, cfg.LLMSMNamespace)

		By("Verifying PrometheusRule has correct structure")
		Expect(prometheusRule.Spec.Groups).To(HaveLen(1), "Should have 1 rule group")
		Expect(prometheusRule.Spec.Groups[0].Name).To(Equal("scaling-manager.rules"))

		// The alert NAMES, read from the manifest we ship rather than written out
		// here. A hand-written list is a third place to remember an alert exists, and
		// it had already drifted: it asserted five names against a manifest that had
		// shipped six since the commit adding both, so it could never have passed —
		// unseen because this spec is labelled `smoke`, which `test-e2e-full`
		// excludes. Comparing against the manifest asserts the thing worth asserting:
		// the cluster has what we ship.
		expectedAlerts, alertsErr := alerts.AlertNames()
		Expect(alertsErr).NotTo(HaveOccurred())
		alertNames := make([]string, 0, len(prometheusRule.Spec.Groups[0].Rules))
		for _, rule := range prometheusRule.Spec.Groups[0].Rules {
			alertNames = append(alertNames, rule.Alert)
		}
		Expect(alertNames).To(ConsistOf(expectedAlerts),
			"deployed alerts must match config/components/prometheus-alerts/prometheusrule.yaml")
		GinkgoWriter.Println("✓ PrometheusRule structure is valid")
	})

	It("should have rules loaded and healthy in Prometheus", func() {
		// This test requires access to Prometheus, which may not be available in all test environments.
		// It will skip if PROMETHEUS_URL is not set or if Prometheus is not reachable.

		By("Checking if Prometheus is accessible")
		_, err := queryPrometheusRules()
		if err != nil {
			prometheusURL := os.Getenv("PROMETHEUS_URL")
			if prometheusURL == "" {
				Skip("PROMETHEUS_URL not set and in-cluster Prometheus is not accessible. " +
					"This test requires Prometheus to be reachable. " +
					"Set PROMETHEUS_URL to point to an accessible Prometheus instance or run tests inside the cluster.")
			}
			// PROMETHEUS_URL was provided explicitly, so the operator intends this
			// check to run — an unreachable endpoint is a real failure, not a skip.
			// Otherwise a broken ruleSelector/endpoint would pass silently.
			Fail(fmt.Sprintf("Prometheus not accessible at %s: %v. "+
				"PROMETHEUS_URL is set, so this rules-loaded check must run.", prometheusURL, err))
		}

		By("Waiting for Prometheus operator to reconcile rules (~60s)")
		var scalingManagerGroup *PrometheusRuleGroup
		Eventually(func() error {
			rulesResp, err := queryPrometheusRules()
			if err != nil {
				return fmt.Errorf("failed to query Prometheus: %w", err)
			}

			// Find the scaling-manager.rules group
			for i := range rulesResp.Data.Groups {
				if rulesResp.Data.Groups[i].Name == prometheusGroupName {
					scalingManagerGroup = &rulesResp.Data.Groups[i]
					break
				}
			}

			if scalingManagerGroup == nil {
				return fmt.Errorf("rule group '%s' not found in Prometheus", prometheusGroupName)
			}

			return nil
		}, 60*time.Second, 5*time.Second).Should(Succeed(),
			"Prometheus should load the scaling-manager.rules group. "+
				"Verify Prometheus ruleSelector/ruleNamespaceSelector matches the PrometheusRule labels.")

		GinkgoWriter.Printf("✓ Rule group '%s' is loaded in Prometheus\n", prometheusGroupName)

		By("Verifying all rules are healthy")
		// Read from the manifest we ship, like the structure spec above: a list
		// written out here asserted 5 rules against a file holding 10.
		expectedRules, rulesErr := alerts.AlertNames()
		Expect(rulesErr).NotTo(HaveOccurred())
		Expect(scalingManagerGroup.Rules).To(HaveLen(len(expectedRules)),
			"Prometheus should load every rule in config/components/prometheus-alerts/prometheusrule.yaml")

		foundRules := make(map[string]bool)
		for _, rule := range scalingManagerGroup.Rules {
			foundRules[rule.Name] = true

			Expect(rule.Health).To(Equal("ok"),
				"Rule '%s' should have health=ok, got '%s'. Check for syntax errors in the PromQL expression.",
				rule.Name, rule.Health)

			GinkgoWriter.Printf("  ✓ Rule '%s' is healthy\n", rule.Name)
		}

		for _, expectedRule := range expectedRules {
			Expect(foundRules).To(HaveKey(expectedRule),
				"Expected rule '%s' not found in Prometheus. Verify PrometheusRule was reconciled correctly.",
				expectedRule)
		}

		GinkgoWriter.Printf("✓ All %d rules are loaded and healthy in Prometheus\n", len(expectedRules))
		GinkgoWriter.Println("\nNote: This test verifies rules are visible to Prometheus but does NOT validate that alerts fire when conditions are met.")
	})

	It("should have all expected alert rules defined", func() {
		By("Retrieving PrometheusRule")
		prometheusRule := &promoperator.PrometheusRule{}
		err := crClient.Get(ctx, client.ObjectKey{
			Name:      prometheusRuleName,
			Namespace: cfg.LLMSMNamespace,
		}, prometheusRule)
		Expect(err).NotTo(HaveOccurred(), "PrometheusRule should exist")

		By("Verifying every shipped alert rule is present")
		expectedAlerts, alertsErr := alerts.AlertNames()
		Expect(alertsErr).NotTo(HaveOccurred())

		rules := prometheusRule.Spec.Groups[0].Rules
		foundAlerts := make(map[string]bool)
		for _, rule := range rules {
			if rule.Alert != "" {
				foundAlerts[rule.Alert] = true
				GinkgoWriter.Printf("  ✓ Found alert: %s\n", rule.Alert)
			}
		}

		for _, expectedAlert := range expectedAlerts {
			Expect(foundAlerts).To(HaveKey(expectedAlert),
				"PrometheusRule should contain alert: "+expectedAlert)
		}
		GinkgoWriter.Printf("✓ All %d expected alert rules are present\n", len(expectedAlerts))
	})

	It("should have valid alert rule structure", func() {
		By("Retrieving PrometheusRule")
		prometheusRule := &promoperator.PrometheusRule{}
		err := crClient.Get(ctx, client.ObjectKey{
			Name:      prometheusRuleName,
			Namespace: cfg.LLMSMNamespace,
		}, prometheusRule)
		Expect(err).NotTo(HaveOccurred())

		By("Validating each alert rule has required fields")
		rules := prometheusRule.Spec.Groups[0].Rules
		for _, rule := range rules {
			if rule.Alert == "" {
				continue
			}

			// Verify alert name
			Expect(rule.Alert).NotTo(BeEmpty(), "Alert should have a name")

			// Verify expression
			Expect(rule.Expr.String()).NotTo(BeEmpty(), "Alert should have an expression")

			// Verify severity label
			Expect(rule.Labels).To(HaveKey("severity"), "Alert should have severity label")

			// Verify annotations
			Expect(rule.Annotations).To(HaveKey("summary"), "Alert should have summary annotation")
			Expect(rule.Annotations).To(HaveKey("description"), "Alert should have description annotation")

			GinkgoWriter.Printf("  ✓ Alert '%s' has valid structure\n", rule.Alert)
		}
		GinkgoWriter.Println("✓ All alert rules have valid structure")
	})

	It("should only reference known the scaling manager metrics in alert expressions", func() {
		By("Retrieving PrometheusRule")
		prometheusRule := &promoperator.PrometheusRule{}
		err := crClient.Get(ctx, client.ObjectKey{
			Name:      prometheusRuleName,
			Namespace: cfg.LLMSMNamespace,
		}, prometheusRule)
		Expect(err).NotTo(HaveOccurred())

		By("Building a map of valid the scaling manager metric names")
		validMetrics := scalingManagerMetricNames()

		By("Validating each alert expression references only known metrics")
		rules := prometheusRule.Spec.Groups[0].Rules
		for _, rule := range rules {
			if rule.Alert == "" {
				continue
			}

			expr := rule.Expr.String()
			referencedMetrics := alerts.ExtractMetricNames(expr)

			GinkgoWriter.Printf("  Checking alert '%s':\n", rule.Alert)
			GinkgoWriter.Printf("    Expression: %s\n", expr)
			GinkgoWriter.Printf("    Referenced metrics: %v\n", referencedMetrics)

			for _, metric := range referencedMetrics {
				Expect(alerts.IsKnown(metric, validMetrics)).To(BeTrue(),
					"Alert '%s' references unknown metric '%s'. "+
						"If this is a new scaling manager metric, add it to internal/constants/metrics.go and scalingManagerMetricNames in this test. "+
						"If this is a typo or renamed metric, update the alert expression. "+
						"Note: Prometheus auto-generates _total/_count/_sum/_bucket suffixes for counters/histograms.",
					rule.Alert, metric)
			}

			GinkgoWriter.Printf("  ✓ Alert '%s' references only known metrics\n", rule.Alert)
		}
		GinkgoWriter.Println("✓ All alert expressions reference known the scaling manager metrics")
	})

})
