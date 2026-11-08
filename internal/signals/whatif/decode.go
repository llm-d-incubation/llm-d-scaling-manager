// Package whatif prices decode capacity from a fitted ITL line: the fill a
// mean-TPOT target allows, the token and request rate one replica sustains
// there, and the fill a fleet with headroom actually runs at.
//
// It is the arithmetic the slo and saturation analyzers price with, in one
// place, so that a what-if tool can answer with the same numbers the
// controller acts on (docs/proposals/slo-capacity-planner.md). It reads
// lines, shapes and engine parameters, and holds no state.
package whatif

import (
	"math"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// FillBand is the range of KV fill an ITL line is fitted over, and so the range
// a price is clamped to -- the policy's band (analyzerTuning.itlBand.minObservableK,
// analyzerTuning.itlBand.maxObservableK), which the window accepts readings
// inside. Both analyzers clamp to it: the slo analyzer's TPOT pricing and the
// saturation analyzer's PricingK. A capped replica's reading above the band
// teaches the line but does not widen the clamp. The line says nothing useful
// outside the range it was fitted on, and above itl.DefaultMaxObservableK the
// engine preempts rather than slowing down.
//
// An earlier design bounded the fill by the highest fill the fleet had been
// observed at, plus a step. That bound has a low fixed point: the fleet runs
// N / scaleUp replicas and the token rate is concave in k, so the realised
// fill sits below the priced one, each window's observed maximum pulls the
// bound down, and the fill converges near 0.05*s/(1-s) -- 0.28 at a scale-up
// threshold of 0.85. See docs/proposals/slo-tiers.md, "Bounds".
//
// Precondition: 0 < Lo < Hi < 1, the rule the policy's validation applies
// (config.ScalingPolicy.ITLBand returns only such a band). The functions here
// do not check it, except PricingK, which falls back to DefaultBand rather
// than let the result depend on the order its two clamps are applied in;
// PriceTPOT, which refuses such a band (OK false) rather than return a NaN fill
// marked OK; and the planner's questions (Decode), which refuse it with
// InvalidInput.
type FillBand struct{ Lo, Hi float64 }

// DefaultBand is the band of a policy that sets neither edge.
func DefaultBand() FillBand {
	return FillBand{Lo: itl.DefaultMinObservableK, Hi: itl.DefaultMaxObservableK}
}

// valid reports whether b meets FillBand's precondition. NaN at either end
// fails it, since NaN fails every comparison.
func (b FillBand) valid() bool {
	return b.Lo > 0 && b.Lo < b.Hi && b.Hi < 1
}

// Pricing is the outcome of inverting a line at a mean-TPOT target.
type Pricing struct {
	// Fill is the KV fill, as a fraction of physical KV, at which the line's
	// mean ITL equals the target, clamped to the band.
	Fill float64
	// Target is that fill before the clamp, for the log: above the band the
	// target is looser than the clamp lets the variant run.
	Target float64
	// Feasible is false when the line's ITL at the band's bottom already
	// reaches the target (equal counts): no fill in range meets it on this
	// variant.
	Feasible bool
	// OK is false when an input was missing or non-finite. Neither of the
	// other two fields means anything then.
	OK bool
}

// PriceTPOT inverts ITL(k) = A*k + B at a mean TPOT target, in seconds:
// k = (target - B) / A.
//
// TPOT is a MEAN, and the line is fitted on the mean inter-token latency
// (vllm:inter_token_latency_seconds, sum over count), so no tail model stands
// between the two.
//
// A band that breaks FillBand's precondition is an unusable input like the
// others: OK false. With a NaN edge the clamp returned NaN as the fill, and an
// inverted band returned whichever edge the last clamp applied, both marked OK.
// The slo analyzer cannot pass either: it takes its band from
// config.ScalingPolicy.ITLBand, which resets such a band to the defaults.
func PriceTPOT(band FillBand, line itl.Model, targetSeconds float64) Pricing {
	if !band.valid() || !Finite(line.A) || !Finite(line.B) || !Finite(targetSeconds) {
		return Pricing{}
	}
	if !(line.A > 0) || !(targetSeconds > 0) {
		return Pricing{}
	}
	k := (targetSeconds - line.B) / line.A
	if targetSeconds <= line.ITLAt(band.Lo) {
		return Pricing{Target: k, OK: true}
	}
	if !Finite(k) {
		return Pricing{}
	}
	return Pricing{Fill: min(max(k, band.Lo), band.Hi), Target: k, Feasible: true, OK: true}
}

// CappedSequences is the number of sequences resident on a replica at fill k
// (itl.Sequences), capped at replicaCap, the most the replica's engines admit.
// A replicaCap of 0 or below caps nothing.
//
// itl.Sequences does not apply the cap -- it prices a cache, not an engine --
// and short requests imply more resident sequences than the engine will admit,
// so every price of a replica caps the count here before dividing by ITL(k).
//
// replicaCap is the REPLICA's cap: one engine's max_num_seqs times the
// data-parallel ranks the replica runs, since capacityTokens is the whole
// replica's KV. Capping a pod of eight ranks at one rank's 256 priced it at
// an eighth of what it serves (issue ev-shindin/llm-scaling-manager#150).
func CappedSequences(k, capacityTokens, kvPerRequest, replicaCap float64) float64 {
	seqs := itl.Sequences(k, capacityTokens, kvPerRequest)
	if replicaCap > 0 {
		seqs = min(seqs, replicaCap)
	}
	return seqs
}

// ReplicaTokenRate is the generation tokens per second one replica sustains at
// fill k: the sequences resident there, capped at maxNumSeqs
// (CappedSequences), each advancing one token every ITL(k) seconds. Zero when
// any input is unusable.
//
// maxNumSeqs is the REPLICA's cap: the engine's max_num_seqs times the
// data-parallel ranks the replica runs. A maxNumSeqs of 0 caps nothing.
func ReplicaTokenRate(line itl.Model, k, capacityTokens, kvPerRequest float64, maxNumSeqs int64) float64 {
	seqs := CappedSequences(k, capacityTokens, kvPerRequest, float64(max(maxNumSeqs, 0)))
	if !(seqs > 0) {
		return 0
	}
	itlSec := line.ITLAt(k)
	if !(itlSec > 0) {
		return 0
	}
	rate := seqs / itlSec
	if !Finite(rate) {
		return 0
	}
	return rate
}

// ReachableFill is the fill a replica can actually reach at k: k itself, unless
// the engine's max_num_seqs admits fewer sequences than k implies. Then the
// cache never fills past maxNumSeqs*KVreq/C, and pricing the ITL of k would
// charge the latency of a load the engine cannot carry. Never below the band,
// where the line was not fitted: the ITL there is the higher, safer reading.
func ReachableFill(band FillBand, k, capacityTokens, kvPerRequest float64, maxNumSeqs int64) float64 {
	if maxNumSeqs <= 0 || !(capacityTokens > 0) || !(kvPerRequest > 0) {
		return k
	}
	kCap := float64(maxNumSeqs) * kvPerRequest / capacityTokens
	if kCap >= k {
		return k
	}
	return max(kCap, band.Lo)
}

// RealisedFill is the fill a replica actually runs at once the fleet carries
// scale-up headroom: N_slo / scaleUp replicas share the load, so each sustains
// scaleUp times the token rate it was priced at. The rate is not linear in k,
// so the fill that delivers it is found by bisection rather than by scaling k.
// Returns k itself when the headroom is unusable, and NaN when the rate is.
func RealisedFill(band FillBand, line itl.Model, k, scaleUp, capacityTokens, kvPerRequest float64, maxNumSeqs int64) float64 {
	priced := ReplicaTokenRate(line, k, capacityTokens, kvPerRequest, maxNumSeqs)
	if !(priced > 0) {
		return math.NaN()
	}
	if !(scaleUp > 0) || scaleUp >= 1 {
		return k
	}
	want := scaleUp * priced
	lo, hi := 0.0, k
	for range 60 {
		mid := (lo + hi) / 2
		if ReplicaTokenRate(line, mid, capacityTokens, kvPerRequest, maxNumSeqs) < want {
			lo = mid
		} else {
			hi = mid
		}
	}
	// Below the fitted range the line says nothing; the ITL at its bottom is
	// the higher, safer reading.
	return max(hi, band.Lo)
}

// Finite reports whether x is a number the pricing can use: neither NaN nor
// infinite. NaN fails every comparison, so a guard written as !(x > 0) already
// rejects it, but not +Inf; this rejects both.
func Finite(x float64) bool {
	return !math.IsNaN(x) && !math.IsInf(x, 0)
}
