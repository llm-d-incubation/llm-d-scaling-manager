package collector

import (
	"context"
	"math"
	"sort"
	"strconv"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/registration"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// ttftInputHistograms are the TTFT model's per-pod histograms: each query and
// the bound-to-rate map it fills.
var ttftInputHistograms = []struct {
	query string
	field func(*podMetricData) *map[float64]float64
}{
	{registration.QueryPromptTokensBuckets, func(d *podMetricData) *map[float64]float64 { return &d.promptTokensBuckets }},
	{registration.QueryComputedTokensBuckets, func(d *podMetricData) *map[float64]float64 { return &d.computedTokensBuckets }},
	{registration.QueryTTFTBuckets, func(d *podMetricData) *map[float64]float64 { return &d.ttftBuckets }},
	{registration.QueryQueueTimeBuckets, func(d *podMetricData) *map[float64]float64 { return &d.queueTimeBuckets }},
	{registration.QueryTPOTBuckets, func(d *podMetricData) *map[float64]float64 { return &d.tpotBuckets }},
}

// ttftInputFigures are the TTFT model's per-pod figures read under
// extractNonNegative's rule: each query and the value and presence bit it
// fills. The three means, and the queue minimum, which is not a mean but is
// read the same way (a zero is a queue that emptied in the minute).
var ttftInputFigures = []struct {
	query string
	field func(*podMetricData) (*float64, *bool)
}{
	{registration.QueryAvgQueueTime, func(d *podMetricData) (*float64, *bool) { return &d.avgQueueTime, &d.avgQueueTimeKnown }},
	{registration.QueryAvgPrefillTime, func(d *podMetricData) (*float64, *bool) { return &d.avgPrefillTime, &d.avgPrefillTimeKnown }},
	{registration.QueryAvgKVTransferTime, func(d *podMetricData) (*float64, *bool) {
		return &d.avgKVTransferTime, &d.avgKVTransferTimeKnown
	}},
	{registration.QueryQueueMin, func(d *podMetricData) (*float64, *bool) { return &d.queueMin, &d.queueMinKnown }},
}

// extractTTFTInputs reads the series the TTFT model learns from
// (registration/ttft_inputs.go) into the instances the model-scoped queries
// already found. They are supplementary like the output-length histogram:
// they attach to an instance and never define one, and a failed query leaves
// each one absent, an unknown and never a zero. A query the cycle did not
// issue (replicaQueryLists) has no result and reads the same way.
func (c *ReplicaMetricsCollector) extractTTFTInputs(ctx context.Context, modelID, namespace string,
	results map[string]*source.MetricResult, podData map[string]*podMetricData) {
	for _, h := range ttftInputHistograms {
		c.extractBuckets(ctx, modelID, namespace, results[h.query], podData, h.field)
	}
	for _, f := range ttftInputFigures {
		c.extractNonNegative(ctx, modelID, namespace, results[f.query], podData, f.field)
	}
}

// extractNonNegative reads one per-instance figure into the value and
// presence bit field returns, for the instances already found. A finite,
// non-negative value is a reading, a zero included (no wait). A NaN is the 0/0
// of a window with no request, and is not one; nor is an infinity, a
// negative, another model's series or a failed query. Sticky across series
// under one key, like ITL: once a series is read, a later unusable one leaves
// it.
func (c *ReplicaMetricsCollector) extractNonNegative(ctx context.Context, modelID, namespace string,
	result *source.MetricResult, podData map[string]*podMetricData, field func(*podMetricData) (*float64, *bool)) {
	if result == nil || result.HasError() {
		return
	}
	for _, value := range result.Values {
		if value.Labels[seriesModelLabel] != modelID {
			continue
		}
		if !value.Finite() || value.Value < 0 {
			continue
		}
		instanceKey, _, _ := c.buildInstanceKey(ctx, namespace, value.Labels)
		if instanceKey == "" || podData[instanceKey] == nil {
			continue
		}
		v, known := field(podData[instanceKey])
		*v, *known = value.Value, true
	}
}

// bucketBound is the upper bound of one histogram bucket series, and whether
// the series is a usable bucket: its le label parses to a number that is not
// NaN ("+Inf" is a bound), and its rate is a finite, non-negative number. A
// NaN rate the Prometheus source stored as 0 is not usable (Finite reads its
// flag).
func bucketBound(value source.MetricValue) (float64, bool) {
	le, err := strconv.ParseFloat(value.Labels["le"], 64)
	if err != nil || math.IsNaN(le) {
		return 0, false
	}
	if !value.Finite() || value.Value < 0 {
		return 0, false
	}
	return le, true
}

// bucket is the shape every histogram bucket in a row has: domain.LengthBucket
// (tokens) and domain.LatencyBucket (seconds).
type bucket interface {
	~struct {
		UpperBound float64
		Rate       float64
	}
}

// rawBucket is bucket's underlying struct, which each bucket type converts
// to and from.
type rawBucket = struct {
	UpperBound float64
	Rate       float64
}

// sortedBuckets is a histogram as a row carries it: one bucket per bound,
// sorted by bound. Nil when byBound holds no bound, so an absent histogram
// is never an empty one.
func sortedBuckets[B bucket](byBound map[float64]float64) []B {
	if len(byBound) == 0 {
		return nil
	}
	out := make([]B, 0, len(byBound))
	for le, rate := range byBound {
		out = append(out, B(rawBucket{UpperBound: le, Rate: rate}))
	}
	sort.Slice(out, func(i, j int) bool { return rawBucket(out[i]).UpperBound < rawBucket(out[j]).UpperBound })
	return out
}

// mergeBuckets is one pod's histogram from its ranks': each bound's rate
// summed, since a request any rank served is one the pod served. Nil unless
// every rank had a histogram: a rank without one would read as a rank that
// served nothing.
func mergeBuckets[B bucket](group []domain.ReplicaMetrics, field func(domain.ReplicaMetrics) []B) []B {
	byBound := map[float64]float64{}
	for _, m := range group {
		bs := field(m)
		if bs == nil {
			return nil
		}
		for _, b := range bs {
			byBound[rawBucket(b).UpperBound] += rawBucket(b).Rate
		}
	}
	return sortedBuckets[B](byBound)
}

// queueTimeCountRate is the rate of requests the rank's queue-time histogram
// counted, its +Inf bucket; NaN when the rank has no such bucket, which makes
// weightedBy weight the pod by request rate instead.
func queueTimeCountRate(m domain.ReplicaMetrics) float64 {
	for _, b := range m.QueueTimeBuckets {
		if math.IsInf(b.UpperBound, 1) {
			return b.Rate
		}
	}
	return math.NaN()
}

// mergeKnownSum is one pod's figure from its ranks' as a sum, known only
// when every rank read one: the queue minimum, where the sum of each rank's
// least is the least the pod's own queue (the ranks' sum) can have been.
func mergeKnownSum(group []domain.ReplicaMetrics, field func(domain.ReplicaMetrics) (float64, bool)) (float64, bool) {
	var sum float64
	for _, m := range group {
		v, known := field(m)
		if !known {
			return 0, false
		}
		sum += v
	}
	return sum, true
}

// mergeKnownMean is one pod's mean from its ranks', weighted by weight, and
// known only when every rank read one. Unknown, the mean is 0, so the value
// never carries a reading the bit denies.
//
// A mean's exact weight is the rate of the observations it averages, per
// rank. weight gives that where the row carries it; nil, or a rank it gives
// none for (NaN), weights the whole pod by request rate instead, the
// completions a second, as service time is (weightedBy, weightedByRequestRate):
//
//   - AvgQueueTime by queueTimeCountRate, the +Inf bucket of the rank's own
//     queue-time histogram: the requests that left the queue, exactly;
//   - AvgPrefillTime by request rate: every completed request prefilled once
//     on the pod that served it, so the two rates differ only by requests in
//     flight at the window's edges;
//   - AvgKVTransferTime by request rate, which is NOT its count: the NIXL
//     mean is per transfer handle, and a request may post several at once
//     (one per remote rank it reads from, or per memory kind), or none on a
//     pod that did not prefill it. The transfer count is not queried.
//
// A pod of one rank, the common case, is its rank's mean whatever the weight.
func mergeKnownMean(group []domain.ReplicaMetrics, field func(domain.ReplicaMetrics) (float64, bool),
	weight func(domain.ReplicaMetrics) float64) (float64, bool) {
	for _, m := range group {
		if _, known := field(m); !known {
			return 0, false
		}
	}
	value := func(m domain.ReplicaMetrics) float64 {
		x, _ := field(m)
		return x
	}
	var v float64
	if weight == nil {
		v = weightedByRequestRate(group, value)
	} else {
		v = weightedBy(group, value, weight)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}
