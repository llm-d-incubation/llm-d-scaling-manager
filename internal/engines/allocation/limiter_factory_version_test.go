package allocation

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
)

// Review R2-2: an entry's answer on llmsm_kueue_controller_version_known
// outlived the entry. A quota entry that counted borrowing and could not read
// the Kueue controller's version published 0; the operator then removed it
// (or disabled the limiter), and the series stayed at 0, paging, until the
// process restarted. A rebuild keeps only the live entries' answers.
func TestLimiterRebuildPrunesVersionKnown(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { metrics.PruneKueueVersionKnown(nil) })
	versionKnown := func() []float64 {
		mfs, err := registry.Gather()
		if err != nil {
			t.Fatal(err)
		}
		var out []float64
		for _, mf := range mfs {
			if mf.GetName() == constants.LLMSMKueueControllerVersionKnown {
				for _, m := range mf.GetMetric() {
					out = append(out, m.GetGauge().GetValue())
				}
			}
		}
		return out
	}

	metrics.PublishKueueVersionKnown("removed-entry", false, true)
	if got := versionKnown(); len(got) != 1 || got[0] != 0 {
		t.Fatalf("before the rebuild the series is %v, want [0]", got)
	}
	if _, err := NewLimiterFromConfig(config.NewTestConfig(), nil); err != nil {
		t.Fatal(err)
	}
	if got := versionKnown(); len(got) != 0 {
		t.Fatalf("after a rebuild without the entry the series is %v, want none: nothing counts borrowing", got)
	}
}
