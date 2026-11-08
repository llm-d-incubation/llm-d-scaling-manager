package collector

import (
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/common/model"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/registration"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// bucketSamples is the test pod's output-length histogram, one series per
// bound, as Prometheus returns it; modelName is its model_name.
func bucketSamples(modelName string, rates map[float64]float64) model.Vector {
	vec := make(model.Vector, 0, len(rates))
	for le, r := range rates {
		bound := strconv.FormatFloat(le, 'g', -1, 64)
		if math.IsInf(le, 1) {
			bound = "+Inf"
		}
		vec = append(vec, &model.Sample{
			Metric: model.Metric{
				seriesModelLabel: model.LabelValue(modelName),
				"pod":            presencePod,
				"instance":       "10.0.0.1:8000",
				"le":             model.LabelValue(bound),
			},
			Value:     model.SampleValue(r),
			Timestamp: model.TimeFromUnixNano(time.Now().UnixNano()),
		})
	}
	return vec
}

// The histogram and the mean output length, from the query results to
// AvgOutputTokensSquared: the bound on E[O^2] the KV per request is priced
// from (issue ev-shindin/llm-scaling-manager#276). Four requests a second at or under 100 tokens and six
// above, at a mean of 250: the least E[O^2] puts the four at 100 and the six
// at 350.
func TestCollectReplicaMetrics_OutputTokensSquared(t *testing.T) {
	inf := math.Inf(1)
	hist := map[float64]float64{100: 4, 500: 10, inf: 10}
	want := (4*100.0*100 + 6*350.0*350) / 10
	collect := func(series map[string]model.Value) domain.ReplicaMetrics {
		t.Helper()
		return collectParsed(t, series, nil, nil)
	}

	got := collect(map[string]model.Value{
		registration.QueryAvgOutputTokens:     presenceSample(250),
		registration.QueryOutputTokensBuckets: bucketSamples("test-model", hist),
	})
	if !got.AvgOutputTokensSquaredKnown || math.Abs(got.AvgOutputTokensSquared-want) > 1e-3*want {
		t.Fatalf("AvgOutputTokensSquared = %v known %v, want %v known", got.AvgOutputTokensSquared, got.AvgOutputTokensSquaredKnown, want)
	}

	for name, series := range map[string]map[string]model.Value{
		"no histogram (SGLang)": {registration.QueryAvgOutputTokens: presenceSample(250)},
		"no mean":               {registration.QueryOutputTokensBuckets: bucketSamples("test-model", hist)},
		"another model's histogram": {
			registration.QueryAvgOutputTokens:     presenceSample(250),
			registration.QueryOutputTokensBuckets: bucketSamples("other-model", hist),
		},
		"no +Inf bucket": {
			registration.QueryAvgOutputTokens:     presenceSample(250),
			registration.QueryOutputTokensBuckets: bucketSamples("test-model", map[float64]float64{100: 4, 500: 10}),
		},
		"a mean the buckets cannot hold": {
			registration.QueryAvgOutputTokens:     presenceSample(900),
			registration.QueryOutputTokensBuckets: bucketSamples("test-model", hist),
		},
	} {
		if got := collect(series); got.AvgOutputTokensSquaredKnown || got.AvgOutputTokensSquared != 0 {
			t.Errorf("%s: AvgOutputTokensSquared = %v known %v, want no reading", name, got.AvgOutputTokensSquared, got.AvgOutputTokensSquaredKnown)
		}
	}
}

// A pod of two instances: the bound is weighted as the mean is, and known
// only when every instance that completed requests read one.
func TestMergePodInstances_OutputTokensSquared(t *testing.T) {
	rank := func(out, sq, rate float64, known bool) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{PodName: "p", ModelID: "m", AvgOutputTokens: out, RequestRate: rate,
			AvgOutputTokensSquared: sq, AvgOutputTokensSquaredKnown: known}
	}
	got := collapseToPods([]domain.ReplicaMetrics{rank(100, 20000, 1, true), rank(300, 180000, 3, true)})
	if len(got) != 1 || !got[0].AvgOutputTokensSquaredKnown || got[0].AvgOutputTokensSquared != (20000+3*180000)/4.0 {
		t.Fatalf("merged = %+v, want E[O^2] 140000 known", got)
	}
	if got := collapseToPods([]domain.ReplicaMetrics{rank(100, 20000, 1, true), rank(300, 0, 3, false)}); got[0].AvgOutputTokensSquaredKnown {
		t.Error("an instance with output and no histogram must leave the pod's unknown")
	}
	if got := collapseToPods([]domain.ReplicaMetrics{rank(100, 20000, 1, true), rank(0, 0, 0, false)}); !got[0].AvgOutputTokensSquaredKnown {
		t.Error("an instance that completed nothing adds nothing, and must not hide the other's reading")
	}
	if got := collapseToPods([]domain.ReplicaMetrics{rank(100, 20000, 0, true), rank(0, 0, 0, false)}); got[0].AvgOutputTokensSquaredKnown {
		t.Error("a plain mean over an instance with no output doubles the spread, and is not a reading")
	}
	// An instance with a request rate and no mean (its mean series missing)
	// weighs both figures with a zero: two equal instances read the spread
	// doubled.
	if got := collapseToPods([]domain.ReplicaMetrics{rank(100, 20000, 1, true), rank(0, 0, 1, false)}); got[0].AvgOutputTokensSquaredKnown {
		t.Errorf("an instance with a rate and no output length must leave the pod unknown, got E[O^2] %v against a mean %v",
			got[0].AvgOutputTokensSquared, got[0].AvgOutputTokens)
	}
}
