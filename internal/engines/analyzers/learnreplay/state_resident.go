package learnreplay

import (
	"slices"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/fleet"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/shape"
)

// The KV a running request holds on a variant that generates, as its engines
// count it: the tokens in use over the requests running, both read from the
// same scrape. On the decode side the whole context is resident, and a
// prefix-cache hit saves KV only when another running request holds the same
// blocks (saturation's prefix_cache_kv.go, issue ev-shindin/llm-scaling-manager#277), so the shape's
// discounted KVreq, IL x (1 - hit) + OL/2, is not what a request holds. Nor is
// the full context a ceiling: an engine can hold more per request than its
// prompt and output (an indexer's keys, a draft model's KV), and the reading
// counts that too. The planner prices decode from this reading
// (cmd/capacity-planner, pricedVariant.decode).

// minResidentCycles is the fewest cycles a resident reading is taken over;
// fewer is no reading.
const minResidentCycles = 3

// residentSample is one cycle's resident reading of a variant: the tokens in
// use and the requests running, summed over its rows read.
type residentSample struct {
	cycle   int
	tokens  float64
	running int
}

// ResidentKV is the KV a running request held on a variant, measured.
type ResidentKV struct {
	// PerRequest is the median over Cycles of each cycle's tokens in use over
	// its running requests, 0 with no reading.
	PerRequest float64
	// Running is the median of those cycles' running requests.
	Running int
	// Cycles is how many cycles were read, FromCycle and ToCycle the first
	// and the last.
	Cycles, FromCycle, ToCycle int
}

// Read reports a reading.
func (r ResidentKV) Read() bool { return r.PerRequest > 0 && r.Cycles >= minResidentCycles }

// noteResident keeps, per variant that is not prefill, the resident reading of
// each of its latest shapeCycles cycles that have one (the latest samples with
// a reading, not the latest cycles: after a lull below the band they can be
// old, as the line's rows are; FromCycle and ToCycle say which): summed over its Ready
// rows not lent by the warm pool, that read both the running requests and the
// tokens in use of the same scrape, with a KV capacity, and whose fill is in
// [the ITL band's bottom, 1]. Below the band a few requests read a prefix
// they share high, each carrying the whole of it (prefix_cache_kv.go).
func noteResident(resident map[string][]residentSample, in domain.AnalyzerInput, cycle int) {
	policy, _ := in.Config.(*config.ScalingPolicy)
	bottom, _ := policy.ITLBand()
	roles := rolesOf(in)
	now := map[string]*residentSample{}
	for _, rm := range in.ReplicaMetrics {
		if domain.CanonicalRole(roles[rm.VariantName]) == domain.RolePrefill {
			continue
		}
		// The saturation analyzer's row rule (fleetResidentKVPerRequest).
		tokens, running, ok := fleet.ResidentRead(rm, bottom)
		if !ok {
			continue
		}
		s := now[rm.VariantName]
		if s == nil {
			s = &residentSample{cycle: cycle}
			now[rm.VariantName] = s
		}
		s.tokens += tokens
		s.running += running
	}
	for v, s := range now {
		resident[v] = append(resident[v], *s)
		if n := len(resident[v]); n > shapeCycles {
			resident[v] = slices.Clone(resident[v][n-shapeCycles:])
		}
	}
}

// residentOf is the reading of samples for a variant serving shape m: the
// median per request over the samples with at least as many running requests
// as the spread of their ages asks (saturation.MinResidentSequences, at m
// with no hit), and no reading with fewer than minResidentCycles of them. Not
// bounded by m: the reading is what the engine holds.
func residentOf(samples []residentSample, m MeasuredShape) ResidentKV {
	need := saturation.MinResidentSequences(shape.New(m.InputTokens, m.OutputTokens, 0).WithOutputDispersion(m.OutputDispersion))
	var per []float64
	var running []int
	var out ResidentKV
	for _, s := range samples {
		if s.running < need || !(s.tokens > 0) {
			continue
		}
		per, running = append(per, s.tokens/float64(s.running)), append(running, s.running)
		if out.FromCycle == 0 {
			out.FromCycle = s.cycle
		}
		out.ToCycle = s.cycle
	}
	if len(per) < minResidentCycles {
		return ResidentKV{}
	}
	slices.Sort(per)
	slices.Sort(running)
	out.PerRequest, out.Running, out.Cycles = medianOfSorted(per), running[len(running)/2], len(per)
	return out
}

// medianOfSorted is the median of sorted xs, which is not empty.
func medianOfSorted(xs []float64) float64 {
	n := len(xs)
	if n%2 == 1 {
		return xs[n/2]
	}
	return (xs[n/2-1] + xs[n/2]) / 2
}
