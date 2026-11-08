package saturation

import (
	"math"
	"slices"
	"sort"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// The service-rate signal: one struct per variant per cycle, built from the
// values the cycle already priced with, which the engine publishes as the
// llmsm_itl_line_* and llmsm_service_rate_per_replica series.
//
// Built here, from c.itlModels, c.itlSamples and the priced replicas, rather
// than recomputed by whoever publishes it. A second computation of "the line
// this variant is priced on" is a second answer to the question, and the two
// would drift the way three copies of one weighted mean once did
// (docs/proposals/signals-as-metrics.md, "Making it not rot").

// itlWindowLen returns how many (k, ITL) readings the window filed under key
// holds, and 0 when there is no window. Read after noteITL has added and
// pruned this cycle's readings, so it counts what a fit would have used.
func (a *SaturationAnalyzer) itlWindowLen(key string) int {
	return a.itlTracker.WindowLen(key)
}

// attachServiceRates sets each variant's ServiceRate from what this cycle
// priced it on: the line fitLines resolved for it (own or borrowed), how many
// readings its own window holds, and the median saturated completion rate of
// its own replicas.
//
// The line and its sample count only while the variant has replicas
// reporting. One parked at zero beside serving siblings still has a window --
// its readings take 30 minutes to age out -- and may still be lent a line, but
// nothing prices it on either now, and published they read as a parked
// variant still fitting.
func (c *cycle) attachServiceRates(variants []domain.VariantCapacity, caps []capacity.ReplicaCapacity) {
	kPrice := pricingK(c.cfg)
	for i := range variants {
		name := variants[i].VariantName
		sig := &domain.ServiceRateSignal{Role: domain.CanonicalRole(c.rolesByVariant[name])}
		serving := variants[i].ObservedReplicas > 0
		// Present for every serving variant that keeps a window, zero
		// included. A window holding nothing is the commonest reason a decode
		// variant has no line -- the ~28 cycles a fit takes from load start --
		// and a series that vanished with the line could not say so.
		if n, ok := c.itlSamples[name]; ok && serving {
			sig.ITLWindowed = true
			sig.LineSamples = n
		}
		if line, ok := c.itlModels[name]; ok && serving && lineUsableAt(line, kPrice) {
			sig.HasLine = true
			sig.LineSlope = line.A
			sig.LineIntercept = line.B
			sig.ITLAtPricingK = line.ITLAt(kPrice)
		}
		sig.Rate, sig.RateSource = variantServiceRate(name, caps)
		// An aggregated variant sizes from its own line only once lifted
		// (ev-shindin/llm-scaling-manager#275), and says so: its line is priced by shape, not decode's.
		if sig.RateSource == domain.ServiceRateSourceDerived && sig.Role == domain.RoleBoth {
			sig.RateSource = domain.ServiceRateSourceDerivedAggregated
		}
		// An aggregated variant priced on no rate of its own still reports
		// the mu its line derives, under a source that says it sized nothing
		// (itlLineReportOnly, ev-shindin/llm-scaling-manager#432).
		if sig.RateSource == "" && serving {
			if r, ok := medianRate(c.reportedMu[name]); ok {
				sig.Rate, sig.RateSource = r, domain.ServiceRateSourceReportOnly
			}
		}
		variants[i].ServiceRate = sig
	}
}

// medianRate is the median of rates, the central pair averaged on an even
// count, as variantServiceRate takes it; false with none.
func medianRate(rates []float64) (float64, bool) {
	if len(rates) == 0 {
		return 0, false
	}
	s := slices.Sorted(slices.Values(rates))
	n := len(s)
	if n%2 == 0 {
		return (s[n/2-1] + s[n/2]) / 2, true
	}
	return s[n/2], true
}

// lineUsableAt reports whether a line can derive a service rate at k: the
// condition whatif.DeriveMuAtCap applies. A fit that passes itl.ValidModel, which checks
// the line at k_sat (0.85), can still be zero or negative at the pricing k
// (0.68 with the shipped defaults), and whatif.DeriveMuAtCap refuses it; publishing it
// would show a negative inter-token latency as the one a rate is priced at.
func lineUsableAt(line itl.Model, k float64) bool {
	return !line.IsZero() && line.ITLAt(k) > 0
}

// variantServiceRate returns the median saturated completion rate over a
// variant's own replicas that have one, and the source of the lower-median
// replica's reading. ("", 0) when none has a rate.
//
// Own replicas only, never a bridge: a warm pool Pod runs its engine at a
// different --gpu-memory-utilization, and the floor leaves bridges out of its
// median for that reason (floor.Estimate).
//
// The median averages the central pair on an even count, the convention the
// floor and aggregateByVariant use. One series carries one source, so the
// source is the lower-median replica's -- an actual replica's, as
// k2SourceLabel picks its label -- which on a variant whose replicas disagree
// names the reading at or just below the published rate. In practice they
// rarely disagree: every replica of a variant is keyed by the fleet's shape,
// not its own (computeReplicaCapacity), so they read the same window.
func variantServiceRate(variant string, caps []capacity.ReplicaCapacity) (float64, string) {
	type reading struct {
		rate   float64
		source string
	}
	var readings []reading
	for _, rc := range caps {
		if rc.VariantName != variant || rc.FromWarmPool {
			continue
		}
		// !(x > 0) rather than x <= 0, so a NaN is refused rather than
		// admitted; +Inf is refused for the same reason the floor's
		// priceable refuses it.
		if !(rc.SaturatedThroughput > 0) || math.IsInf(rc.SaturatedThroughput, 1) {
			continue
		}
		readings = append(readings, reading{rate: rc.SaturatedThroughput, source: serviceRateSource(rc)})
	}
	if len(readings) == 0 {
		return 0, ""
	}
	// Ties broken on the source so the label does not depend on the order
	// the rows arrived in.
	sort.Slice(readings, func(i, j int) bool {
		if readings[i].rate != readings[j].rate {
			return readings[i].rate < readings[j].rate
		}
		return readings[i].source < readings[j].source
	})
	n := len(readings)
	rate := readings[n/2].rate
	if n%2 == 0 {
		rate = (readings[n/2-1].rate + readings[n/2].rate) / 2
	}
	return rate, readings[(n-1)/2].source
}

// serviceRateSource names the path that answered one replica's rate.
//
// Derived is checked first: when the derived figure is the one in use, the
// Borrowed and Samples fields describe the measured window that was NOT used
// (capacity.ReplicaCapacity), and reading them would mislabel it.
func serviceRateSource(rc capacity.ReplicaCapacity) string {
	switch {
	case rc.SaturatedThroughputLowerBound:
		return domain.ServiceRateSourceLowerBound
	case rc.SaturatedThroughputLineBorrowed:
		// Set only when the derived figure is the one in use AND its line
		// is a sibling's (priceReplicas), so it is a subset of Derived.
		return domain.ServiceRateSourceBorrowedLine
	case rc.SaturatedThroughputDerived:
		return domain.ServiceRateSourceDerived
	case rc.SaturatedThroughputBorrowed:
		return domain.ServiceRateSourceNeighbour
	default:
		return domain.ServiceRateSourceMeasured
	}
}
