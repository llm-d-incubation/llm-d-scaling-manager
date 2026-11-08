package collector

import (
	"context"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/registration"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// extractOutputBuckets reads the output-length histogram into the instances
// the model-scoped queries already found, one rate per bucket bound, and skips
// the rest, like the other supplementary series (extractSequenceCap).
//
// The query is engine-specific, and filterResultsToModel has narrowed it to
// this model already, dropping a series with no model_name or another one.
// The same check here keeps the block from resting on that.
//
// A series whose bound does not parse, or whose rate is not a finite
// non-negative number, is dropped. A dropped finite bucket only widens the
// span between its neighbours (the counts are cumulative), so the bound
// stays a bound, a little lower. A dropped +Inf bucket leaves the requests
// above the last bound uncounted, and domain.SquaredMeanLowerBound refuses
// the histogram.
func (c *ReplicaMetricsCollector) extractOutputBuckets(ctx context.Context, modelID, namespace string,
	results map[string]*source.MetricResult, podData map[string]*podMetricData) {
	c.extractBuckets(ctx, modelID, namespace, results[registration.QueryOutputTokensBuckets], podData,
		func(d *podMetricData) *map[float64]float64 { return &d.outputTokensBuckets })
}

// extractBuckets reads one per-instance histogram query into the map field
// returns, for the instances the model-scoped queries already found, by
// bucket bound. The rules are extractOutputBuckets': this model's series
// only, a bound that parses, a finite non-negative rate. The map stays nil
// until one bucket is read, so a missing series, a failed query and a
// histogram of NaNs all leave the instance without one: absent, never a
// histogram of zeros.
func (c *ReplicaMetricsCollector) extractBuckets(ctx context.Context, modelID, namespace string,
	result *source.MetricResult, podData map[string]*podMetricData, field func(*podMetricData) *map[float64]float64) {
	if result == nil || result.HasError() {
		return
	}
	for _, value := range result.Values {
		if value.Labels[seriesModelLabel] != modelID {
			continue
		}
		le, ok := bucketBound(value)
		if !ok {
			continue
		}
		instanceKey, _, _ := c.buildInstanceKey(ctx, namespace, value.Labels)
		if instanceKey == "" || podData[instanceKey] == nil {
			continue
		}
		buckets := field(podData[instanceKey])
		if *buckets == nil {
			*buckets = make(map[float64]float64)
		}
		// Two series with one bound (a model reached under two label
		// sets) are one bucket.
		(*buckets)[le] += value.Value
	}
}

// outputTokensSquared reduces an instance's histogram and its mean output
// length to the lower bound on E[O^2] (domain.SquaredMeanLowerBound), and
// whether it is a reading.
func outputTokensSquared(buckets map[float64]float64, mean float64) (float64, bool) {
	if len(buckets) == 0 {
		return 0, false
	}
	bs := make([]domain.LengthBucket, 0, len(buckets))
	for le, rate := range buckets {
		bs = append(bs, domain.LengthBucket{UpperBound: le, Rate: rate})
	}
	return domain.SquaredMeanLowerBound(bs, mean)
}
