package fleet

import (
	"math"
	"slices"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/shape"
)

// The spread of output lengths behind the KV a resident request holds
// (issues #276, #433), read one way for every analyzer: the saturation
// analyzer's fleet, the slo analyzer's fleet and the throughput analyzer's
// variant each pass the rows they price as include, and their own weights
// as opts.

// SquaredBoundRead reports whether a replica read a bound on E[O^2] at all: a
// positive, finite AvgOutputTokensSquared with its presence bit set. NaN fails
// the comparison; +Inf is refused by name, as Mean refuses it, so a row the
// mean of E[O^2] would drop is never counted as one that read it.
func SquaredBoundRead(rm domain.ReplicaMetrics) bool {
	return rm.AvgOutputTokensSquaredKnown && rm.AvgOutputTokensSquared > 0 &&
		!math.IsInf(rm.AvgOutputTokensSquared, 0)
}

// OutputMoments is E[O] and the bound on E[O^2] over the replicas include
// selects, both Mean with opts, so with one set of weights over one set of
// rows, and whether they make a reading: some row is selected, every selected
// row read the bound (SquaredBoundRead), and both means are positive.
//
// Every row, not most: a row with no bound (an engine that publishes no
// histogram, a relabelling that drops it) would otherwise count as a row
// whose requests are all one length, and the mix of the two is neither's
// spread.
func OutputMoments(replicas []domain.ReplicaMetrics, include func(domain.ReplicaMetrics) bool,
	opts ...Option) (mean, squared float64, ok bool) {
	if !slices.ContainsFunc(replicas, include) ||
		slices.ContainsFunc(replicas, func(rm domain.ReplicaMetrics) bool {
			return include(rm) && !SquaredBoundRead(rm)
		}) {
		return 0, 0, false
	}
	mean = Mean(replicas, func(rm domain.ReplicaMetrics) float64 { return rm.AvgOutputTokens }, include, opts...)
	squared = Mean(replicas, func(rm domain.ReplicaMetrics) float64 { return rm.AvgOutputTokensSquared }, include, opts...)
	if !(mean > 0) || !(squared > 0) {
		return 0, 0, false
	}
	return mean, squared, true
}

// OutputDispersion is D = E[O^2]/E[O]^2 over the replicas include selects
// (OutputMoments, shape.Dispersion), and whether it is a reading; 1 and false
// when it is not.
func OutputDispersion(replicas []domain.ReplicaMetrics, include func(domain.ReplicaMetrics) bool,
	opts ...Option) (float64, bool) {
	mean, squared, ok := OutputMoments(replicas, include, opts...)
	if !ok {
		return 1, false
	}
	return shape.Dispersion(mean, squared), true
}
