package registration

import (
	"strings"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
)

// TestSLORouterQueries asserts on the queries the REGISTRY BUILDS, as the
// arrival-rate guard does, for the same reason: a broken router query reads as
// "the router exports nothing", which leaves the SLO silently unjudged rather
// than failing.
func TestSLORouterQueries(t *testing.T) {
	reg := source.NewSourceRegistry()
	if err := reg.Register("prometheus", source.NewNoOpSource()); err != nil {
		t.Fatal(err)
	}
	RegisterSLOQueries(reg)
	ql := reg.Get("prometheus").QueryList()
	build := func(name string) string {
		q, err := ql.Build(name, map[string]string{source.ParamNamespace: "ns"})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return strings.Join(strings.Fields(q), " ")
	}

	ttft := build(QuerySLORouterTTFTBuckets)
	for _, want := range []string{
		"llm_d_epp_request_ttft_seconds_bucket",
		`streaming="true"`, // a non-streaming TTFT is the whole request
		`namespace="ns"`,
		"sum by (model_name, target_model_name, le)",
		"rate(", "[" + SLORouterWindow + "]",
	} {
		if !strings.Contains(ttft, want) {
			t.Errorf("TTFT query lacks %q: %s", want, ttft)
		}
	}
	if strings.Index(ttft, "sum by") > strings.Index(ttft, "rate(") {
		t.Errorf("TTFT query must rate inside the sum: %s", ttft)
	}
	for name, metric := range map[string]string{
		QuerySLORouterTTFTSum:   "llm_d_epp_request_ttft_seconds_sum",
		QuerySLORouterTTFTCount: "llm_d_epp_request_ttft_seconds_count",
	} {
		q := build(name)
		// As the buckets: a non-streaming TTFT is the whole request.
		for _, want := range []string{metric, `streaming="true"`, `namespace="ns"`, "sum by (model_name, target_model_name)", "[" + SLORouterWindow + "]"} {
			if !strings.Contains(q, want) {
				t.Errorf("%s lacks %q: %s", name, want, q)
			}
		}
	}
	for name, metric := range map[string]string{
		QuerySLORouterTPOTSum:   "llm_d_epp_request_streaming_tpot_seconds_sum",
		QuerySLORouterTPOTCount: "llm_d_epp_request_streaming_tpot_seconds_count",
	} {
		q := build(name)
		// The namespace too: without it the mean blends in a same-named
		// model's streams from every other namespace.
		if !strings.Contains(q, metric) || !strings.Contains(q, "sum by (model_name, target_model_name)") ||
			!strings.Contains(q, "["+SLORouterWindow+"]") || !strings.Contains(q, `namespace="ns"`) {
			t.Errorf("%s: %s", name, q)
		}
		if strings.Contains(q, "streaming=") {
			t.Errorf("%s selects on a streaming label the TPOT histogram does not carry: %s", name, q)
		}
	}
	if SLORouterWindowSeconds != 300 || SLORouterWindow != "5m" {
		t.Errorf("window %q / %v disagree", SLORouterWindow, SLORouterWindowSeconds)
	}
}
