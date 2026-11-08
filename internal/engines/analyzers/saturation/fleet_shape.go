package saturation

import (
	"slices"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/fleet"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/shape"
)

// The one shape the whole fleet is serving this cycle, out of readings that
// disagree.
//
// Split out of analyzer.go, which held all 40 of this package's top-level
// declarations in 2380 lines; see docs/proposals/analyzer-structure.md.
// Nothing changed in the move.

// withExpectedOutputTokens returns replicaMetrics with expected filled in as
// the output length of every OUTPUT-GENERATING replica that reports none.
//
// It exists so the scheduler queue can be priced on the first cycle of load.
// estimateSchedulerQueueDemand charges the queue Q x avgOutput, and avgOutput
// is an average over replicas that have COMPLETED something; on a cold fleet
// none has, so the queue is worth nothing to the role that will generate it.
// Measured on run QL: decode's share read {"decode":0} for the first two
// cycles with 141 requests already queued.
//
// A COPY, never a mutation of the caller's slice. The input is the analyzer's
// own AnalyzerInput, read by several other steps in the same cycle, and a
// replica whose reported output length was quietly rewritten would change what
// every one of them measured -- including the throughput keys and the shape
// tracker, which must follow what the fleet actually served.
//
// Only replicas that generate output, and only those reporting zero: a prefill
// replica completes about one token per request, so filling it in would move a
// figure that is already correct, and a replica with a real reading is not
// improved by a default.
//
// Returns the input unchanged when expected is not positive or nothing needs
// filling, so the common case allocates nothing.
func withExpectedOutputTokens(replicaMetrics []domain.ReplicaMetrics,
	rolesByVariant map[string]string, expected float64) []domain.ReplicaMetrics {
	if !(expected > 0) {
		return replicaMetrics
	}
	needed := slices.ContainsFunc(replicaMetrics, func(rm domain.ReplicaMetrics) bool {
		return !(rm.AvgOutputTokens > 0) && generatesOutput(rm, rolesByVariant)
	})
	if !needed {
		return replicaMetrics
	}
	out := slices.Clone(replicaMetrics)
	for i := range out {
		if !(out[i].AvgOutputTokens > 0) && generatesOutput(out[i], rolesByVariant) {
			out[i].AvgOutputTokens = expected
		}
	}
	return out
}

// computeModelWorkloadAverages computes the model-level average input tokens,
// output tokens, and prefix cache hit rate from replica metrics across all
// variants. These averages enable capacity estimation for zero-replica variants
// using the k2 derivation formula, and scheduler queue demand estimation.
//
// The output length is averaged over the replicas that GENERATE output, which
// on a P/D fleet excludes prefill. A prefill replica completes every request
// after one token (it hands off to decode), so its own
// vllm:request_generation_tokens averages ~1 -- not a measurement of the
// workload's output length but a property of the role. Folding it into an
// unweighted mean halves the model's output length at one prefill per decode
// replica and quarters it at three, and everything priced per queued request
// downstream (the scheduler queue's output-token charge, the zero-replica k2
// derivation) shrinks with it. Measured on a P/D run: 213 requests queued at
// the scheduler were charged 500 output tokens each against a 1000-token
// workload, because the one prefill replica's ~1 averaged against the one
// decode replica's 1000.
//
// Input tokens and the prefix-cache hit rate are still averaged over every
// replica: both roles see the same prompts, and in a P/D deployment the prefix
// cache that a queued prompt can hit lives on the prefill side.
//
// rolesByVariant maps variant name to its P/D role; a variant absent from it
// is treated as domain.RoleBoth, so a non-disaggregated fleet averages over
// every replica exactly as before.
func computeModelWorkloadAverages(replicaMetrics []domain.ReplicaMetrics, rolesByVariant map[string]string) (avgInput, avgOutput, avgHitRate float64) {
	var count, outputCount int
	for _, rm := range replicaMetrics {
		if rm.AvgInputTokens > 0 || rm.AvgOutputTokens > 0 {
			avgInput += rm.AvgInputTokens
			avgHitRate += rm.PrefixCacheHitRate
			count++
			if generatesOutput(rm, rolesByVariant) {
				avgOutput += rm.AvgOutputTokens
				outputCount++
			}
		}
	}
	if count > 0 {
		avgInput /= float64(count)
		avgHitRate /= float64(count)
	}
	if outputCount > 0 {
		avgOutput /= float64(outputCount)
	}
	return avgInput, avgOutput, avgHitRate
}

// fleetOutputLength is the output length the fleet is serving this cycle: the
// generating replicas' average output tokens, weighted by their request rate
// (fleet.Mean).
func fleetOutputLength(replicas []domain.ReplicaMetrics, rolesByVariant map[string]string) float64 {
	return fleet.Mean(replicas,
		func(rm domain.ReplicaMetrics) float64 { return rm.AvgOutputTokens },
		func(rm domain.ReplicaMetrics) bool { return generatesOutput(rm, rolesByVariant) })
}

// fleetOutputDispersion is the spread of the output lengths the fleet is
// serving, D = E[O^2] / E[O]^2, over the generating replicas fleetOutputLength
// averages and with its weights, so the two are one reading of one fleet.
// E[O^2] is each replica's lower bound (AvgOutputTokensSquared), so D is too:
// it prices the KV a resident request holds towards the truth and never past
// it (domain.SquaredMeanLowerBound).
//
// 1, no spread, unless EVERY generating replica with an output length read
// the bound. One without it (an engine that publishes no histogram, a
// relabelling that drops it) would otherwise count as a replica whose
// requests are all one length, and the mix of the two is neither fleet's
// spread.
func fleetOutputDispersion(replicas []domain.ReplicaMetrics, rolesByVariant map[string]string) float64 {
	d, _ := fleet.OutputDispersion(replicas, outputCounted(rolesByVariant))
	return d
}

// fleetOutputMoments is the fleet's E[O] and its bound on E[O^2], both
// request-weighted over the same generating replicas (fleetOutputDispersion
// says which), and whether every one of them read the bound
// (fleet.OutputMoments; a bound of +Inf is no reading).
func fleetOutputMoments(replicas []domain.ReplicaMetrics, rolesByVariant map[string]string) (mean, squared float64, ok bool) {
	return fleet.OutputMoments(replicas, outputCounted(rolesByVariant))
}

// outputCounted selects the replicas the fleet's spread is read over: the
// generating replicas with an output length, as fleetOutputLength averages.
func outputCounted(rolesByVariant map[string]string) func(domain.ReplicaMetrics) bool {
	return func(rm domain.ReplicaMetrics) bool {
		return rm.AvgOutputTokens > 0 && generatesOutput(rm, rolesByVariant)
	}
}

// modelOutputFactor is the factor the zero-replica estimate multiplies the
// model's mean output length by in its KV per request, I + f x O/2, so that
// the generated half is the fleet's own E[O^2] / (2 E[O]), request-weighted.
//
// Not the fleet's D. The estimate's O is computeModelWorkloadAverages' plain
// mean over replicas, while D is request-weighted, and D x plain/2 mixes two
// populations: one replica at 9 requests a second of 100 tokens beside one at
// 1 of 1000 reads D 3.02 against a plain mean of 550, and priced I + 830 where
// the fleet's requests hold I + 287 -- 42% too much capacity at I = 1000. So
// f = E[O^2] / (E[O] x plain). 1, the estimate as before, with no bound read
// (fleetOutputMoments) or no plain mean.
//
// The factor is under 1 when the request-weighted mean is well under the
// plain one: 99 requests a second of 100 tokens beside 1 of 1000 read f
// 0.33. estimateCapacityAtCapSpread reads it as 1, deliberately: the
// estimate is never priced below the I + O/2 it had before ev-shindin/llm-scaling-manager#276, which here
// is above what the requests hold (1275 against 1091). That is the old
// over-statement, not a new one. The log carries the factor as computed.
func modelOutputFactor(replicas []domain.ReplicaMetrics, rolesByVariant map[string]string, plainMean float64) float64 {
	mean, squared, ok := fleetOutputMoments(replicas, rolesByVariant)
	if !ok || !(plainMean > 0) {
		return 1
	}
	return squared / (mean * plainMean)
}

// replicaOutputDispersion is one replica's own spread of output lengths, D =
// E[O^2] / E[O]^2, for the k2 it is priced at (estimateCapacityAtCapSpread):
// 1 with no bound read, while a shape change is outstanding (held), or with
// SwitchKVPerRequestOutputDispersion off.
//
// held is the gate the mu applies too (resolvePricing): across a shape change
// the [5m] histogram holds both shapes, and their difference reads as a spread
// of one workload's lengths. At IL 2000 and 1000 -> 100 out, half way through,
// the window reads D 1.67 and priced a resident request at 2459 against the
// 2050 the arriving requests hold: k2 20% high, where IL + O/2 is 11% high.
//
// A replica's D comes from its own histogram, however few requests it
// completed in the window. The ratio is biased low on few requests (the
// spread a handful of draws shows is less than the workload's), so a quiet
// replica's k2 errs low, which orders more replicas, and is noisy from cycle
// to cycle rather than wrong.
func (a *SaturationAnalyzer) replicaOutputDispersion(rm domain.ReplicaMetrics, held bool) float64 {
	if !a.kvOutputDispersion || held || !rm.AvgOutputTokensSquaredKnown {
		return 1
	}
	return shape.Dispersion(rm.AvgOutputTokens, rm.AvgOutputTokensSquared)
}

// fleetOutputLengthRecent is fleetOutputLength over the SHORT window, and is
// the derived mu's divisor (whatif.DeriveMuAtCap) WHILE A SHAPE CHANGE IS OUTSTANDING only.
//
// Zero when no generating replica reports the short-window figure -- an engine
// that does not publish the counter, or a fleet that completed nothing in the
// last minute.
//
// The caller falls back to fleetOutputLength then, and also whenever the shape
// is steady. That gate is not a precaution; it is a measured requirement. This
// window averages over requests that have COMPLETED, so on a fleet ramping into
// long generations it reads far below the length being served, which over-states
// mu and under-orders replicas. The caller's comment carries the bisect. The
// [5m] figure is wrong only for the few minutes after a shape change, which is
// exactly when this one is used.
func fleetOutputLengthRecent(replicas []domain.ReplicaMetrics, rolesByVariant map[string]string) float64 {
	return fleet.Mean(replicas,
		func(rm domain.ReplicaMetrics) float64 { return rm.AvgOutputTokensRecent },
		func(rm domain.ReplicaMetrics) bool { return generatesOutput(rm, rolesByVariant) })
}

// fleetPrefixHitRate is the prefill side's prefix-cache hit rate as ONE figure
// for the whole role, request-rate weighted.
//
// It passes fleet.ZeroIsAReading(), where the output and prompt lengths take
// fleet.Mean's default: that default skips a value of zero as absent, and a
// hit rate of zero is a reading, not a missing one -- a fleet with prefix
// caching off reports 0 on every replica, and skipping those would leave the
// mean to whichever replica happened to report something. It also bounds with
// fleet.Within(0, 1), because a hit rate is a fraction.
//
// One figure per role per cycle, for the same reason the output length is one
// figure: it buckets a key. rm.PrefixCacheHitRate is per REPLICA, so using it
// directly let two replicas of one variant land in different input buckets in
// the same cycle and split the window the bucket exists to hold together --
// the exact fault prefillOutputBucket was added to remove, on a new axis.
//
// Still not hysteretic: shape.Tracker tracks IL and OL, not this. A fleet
// whose hit rate drifts across a bucket boundary can therefore still move
// prefill's window, just not split it between replicas within a cycle.
func fleetPrefixHitRate(replicas []domain.ReplicaMetrics, rolesByVariant map[string]string) float64 {
	return fleet.Mean(replicas,
		func(rm domain.ReplicaMetrics) float64 { return rm.PrefixCacheHitRate },
		func(rm domain.ReplicaMetrics) bool {
			return domain.CanonicalRole(rolesByVariant[rm.VariantName]) == domain.RolePrefill
		},
		// A hit rate of zero is a reading -- a fleet with prefix caching off
		// reports it on every replica, and skipping those would leave the mean
		// to whichever replica happened to report something. Bounded because a
		// hit rate is a fraction.
		fleet.ZeroIsAReading(), fleet.Within(0, 1))
}
