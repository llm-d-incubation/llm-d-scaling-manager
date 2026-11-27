package whatifapi

import (
	"encoding/json"
	"math"
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// The vLLM buckets the check guide gives (issue
// llm-d-incubation/llm-d-scaling-manager#488) place a TPOT percentile
// anywhere from 5 ms to 53 ms: no bucket in that range is too wide for the
// check to score (coarseBucket). The guide and the recording guide give the
// same value. vLLM's default inter_token_latency edges (0.01, 0.025, 0.05)
// leave every bucket in the range too wide, the control.
func TestTPOTBucketsInTheGuidePlaceEveryPercentile(t *testing.T) {
	flag := regexp.MustCompile(`--custom-histogram-buckets '(\{[^']*\})'`)
	read := func(path string) []float64 {
		body, err := os.ReadFile(path)
		require.NoError(t, err)
		m := flag.FindSubmatch(body)
		require.NotNil(t, m, "%s gives no --custom-histogram-buckets value", path)
		var families map[string][]float64
		require.NoError(t, json.Unmarshal(m[1], &families), "%s", path)
		require.Len(t, families, 1, "%s", path)
		edges := families["inter_token_latency"]
		require.NotEmpty(t, edges, "%s: no inter_token_latency family", path)
		return edges
	}
	edges := read("../../docs/guides/capacity-planner/check.md")
	require.Equal(t, edges, read("../../docs/guides/record-for-capacity-planner/README.md"))
	for i := 1; i < len(edges); i++ {
		require.Greater(t, edges[i], edges[i-1], "edges not strictly increasing at %d (vLLM refuses them)", i)
	}

	// coarse lists the buckets from 5 ms to 53 ms a percentile sitting in
	// them cannot be placed in.
	coarse := func(edges []float64) []Band {
		var out []Band
		for i := 1; i < len(edges); i++ {
			if edges[i-1] < 0.005 || edges[i] > 0.053 {
				continue
			}
			bs := make([]domain.LatencyBucket, 0, len(edges)+1)
			for j, e := range edges {
				var r float64
				if j >= i {
					r = 1 // every request in bucket i
				}
				bs = append(bs, domain.LatencyBucket{UpperBound: e, Rate: r})
			}
			bs = append(bs, domain.LatencyBucket{UpperBound: math.Inf(1), Rate: 1})
			h, ok := histOf(bs)
			require.True(t, ok)
			if b := h.coarseBucket(0.9); b != nil {
				out = append(out, *b)
			}
		}
		return out
	}
	require.Empty(t, coarse(edges), "the guide's buckets too wide to place a p90")
	// Control: vLLM's default edges leave 10-25 ms and 25-50 ms unplaceable.
	require.Equal(t, []Band{{Lo: 0.01, Hi: 0.025}, {Lo: 0.025, Hi: 0.05}},
		coarse([]float64{0.01, 0.025, 0.05, 0.075, 0.1}))
}
