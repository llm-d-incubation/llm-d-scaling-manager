package registration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source/prometheus"
)

// eppQueueFixture is the promtool unit test that evaluates the scheduler
// flow-control queue queries over two namespaces serving the same model name
// (hack/check-collector-queries.sh runs it).
const eppQueueFixture = "testdata/epp-queue-namespace.test.yaml"

// The promtool fixture evaluates copies of the queries, so a copy that drifts
// from the registry tests nothing. Both queue queries must appear in the
// fixture exactly as the registry builds them, and nothing else may.
func TestEPPQueueQueriesMatchPromtoolFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.FromSlash(eppQueueFixture))
	if err != nil {
		t.Fatalf("reading %s: %v", eppQueueFixture, err)
	}
	var fixture struct {
		Tests []struct {
			PromQLExprTest []struct {
				Expr       string           `yaml:"expr"`
				ExpSamples []map[string]any `yaml:"exp_samples"`
			} `yaml:"promql_expr_test"`
		} `yaml:"tests"`
	}
	if err := yaml.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("parsing %s: %v", eppQueueFixture, err)
	}
	inFixture := map[string]int{}
	for _, tc := range fixture.Tests {
		for _, e := range tc.PromQLExprTest {
			// One sample per namespace, or the case cannot show them kept apart.
			if len(e.ExpSamples) < 2 {
				t.Errorf("fixture case %q expects %d samples; it must expect one per namespace", e.Expr, len(e.ExpSamples))
			}
			inFixture[e.Expr]++
		}
	}
	if len(inFixture) == 0 {
		t.Fatalf("%s holds no promql_expr_test cases", eppQueueFixture)
	}

	reg := source.NewSourceRegistry()
	if err := reg.Register("prometheus", prometheus.NewPrometheusSource(
		context.Background(), &mockPrometheusAPI{},
		prometheus.DefaultPrometheusSourceConfig())); err != nil {
		t.Fatalf("registering the prometheus source: %v", err)
	}
	RegisterSaturationQueries(reg)

	registered := map[string]bool{}
	for _, name := range []string{QuerySchedulerQueueSize, QuerySchedulerQueueBytes} {
		q, err := reg.Get("prometheus").QueryList().Build(name, nil)
		if err != nil {
			t.Fatalf("building %s: %v", name, err)
		}
		registered[q] = true
		if inFixture[q] != 1 {
			t.Errorf("%s: the registry builds\n  %s\nwhich the fixture evaluates %d times, not once", name, q, inFixture[q])
		}
		// Every aggregation must keep the namespace, or two namespaces serving
		// the same model name read one summed queue.
		if n, keep := strings.Count(q, "sum by ("), strings.Count(q, "sum by (namespace, "); n == 0 || n != keep {
			t.Errorf("%s: %d of %d aggregations keep the namespace:\n  %s", name, keep, n, q)
		}
	}
	for expr := range inFixture {
		if !registered[expr] {
			t.Errorf("the fixture evaluates a query the registry does not build:\n  %s", expr)
		}
	}
}
