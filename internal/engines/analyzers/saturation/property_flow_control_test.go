package saturation

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/floor"
)

// Property cases for a fleet under flow control (issue #51): the requests
// wait at the router, the engines' own queues read empty, and so neither k2
// nor the mu written beside it is ever learned.
//
// k2 not being learned is not what matters there. Capacity is min(k1, k2),
// and a replica that fills past k1 (0.8 of its KV) before it queues has a k2
// above k1, which cannot move capacity: known-truth/k2/above-k1-leaves-
// capacity holds that. What a P1-obs reading also writes is the replica's
// measured mu, and mu prices the throughput floor. With none, the floor
// prices from the mu the ITL line derives at pricingK (whatif.DeriveMuAtCap,
// SaturatedThroughputDerived). The floor cases ask whether that fallback
// orders at least what the fleet needs, and no more than the learned mu
// would have.

func flowControlCases() []propCase {
	return []propCase{
		{class: classKnownTruth, learner: "floor", name: "flow-control-unlearned-mu-decode", family: familyDirect, tuning: tunedDefault, check: checkFlowControlFloor(fcDecode, nil)},
		{class: classKnownTruth, learner: "floor", name: "flow-control-unlearned-mu-pd-decode", family: familyDirect, tuning: tunedDefault, check: checkFlowControlFloor(fcPD, nil)},
		{class: classKnownTruth, learner: "floor", name: "flow-control-unlearned-mu-pd-prefill-bound", family: familyDirect, tuning: tunedDefault, check: checkFlowControlFloor(fcPDPrefillBound, nil)},
		{class: classKnownTruth, learner: "floor", name: "flow-control-unlearned-mu-pd-decode-owes-backlog", family: familyDirect, tuning: tunedDefault, check: checkFlowControlFloor(fcPD.owingBacklog(), nil)},
		{class: classKnownTruth, learner: "floor", name: "flow-control-unlearned-mu-pd-prefill-bound-owes-backlog", family: familyDirect, tuning: tunedDefault, check: checkFlowControlFloor(fcPDPrefillBound.owingBacklog(), nil)},
		{class: classKnownTruth, learner: "floor", name: "flow-control-unlearned-mu-pd-prefill-bound-decode-full-owes-backlog", family: familyDirect, tuning: tunedDefault, check: checkPrefillBoundDecodeFull},
		{class: classKnownTruth, learner: "floor", name: "flow-control-unlearned-mu-pd-decode-owes-backlog-completions-high", family: familyDirect, tuning: tunedDefault, check: checkCompletionsHigh(fcPD)},
		{class: classKnownTruth, learner: "floor", name: "flow-control-unlearned-mu-pd-prefill-bound-owes-backlog-completions-high", family: familyDirect, tuning: tunedDefault, check: checkCompletionsHigh(fcPDPrefillBound)},
		{class: classKnownTruth, learner: "floor", name: "flow-control-unlearned-mu-pd-prefill-bound-owes-backlog-twice-the-context", family: familyDirect, tuning: tunedDefault, check: checkTwiceTheContext},
		{class: classKnownTruth, learner: "floor", name: "flow-control-unlearned-mu-aggregated", family: familyDirect, tuning: tunedDefault, check: checkFlowControlFloor(fcAggregated, nil)},
		{class: classKnownTruth, learner: "floor", name: "flow-control-unlearned-mu-dp8", family: familyDirect, tuning: tunedDefault, check: checkFlowControlFloor(fcDP8, nil)},
		{class: classKnownTruth, learner: "k2", name: "above-k1-leaves-capacity", family: familyDirect, tuning: tunedDefault, check: checkK2AboveK1Capacity},
	}
}

// fcShape is the fleet a flow-control case runs: two pods of variant v,
// with a prefill variant beside them on P/D.
type fcShape struct {
	name string
	// role is v's role; "" is aggregated.
	role string
	// pd adds a prefill variant p, so the router's queue is charged to it.
	pd bool
	// prefillBound is a P/D fleet whose prefill is the bottleneck: prefill
	// serves all it can, decode takes what it passes on and is not full,
	// and the router holds the rest. Decode is the bottleneck otherwise.
	prefillBound bool
	// ranks is the engine ranks a pod runs (data parallel).
	ranks int
	// owesBacklog runs both runs with SwitchDecodeOwesRouterBacklog on
	// (issue llm-d-incubation/llm-d-scaling-manager#51): decode is charged
	// the router's queue prefill can hand on. Off, the constructor's
	// default, decode-bound P/D under-orders (the ledgered pd-decode case).
	owesBacklog bool
	// switches are more replay switches both runs set on. One this build
	// does not know (ReplaySwitchNames) is left out, so a case can name a
	// switch another change adds, and runs it once both are in.
	switches []string
}

// owingBacklog is s with SwitchDecodeOwesRouterBacklog on.
func (s fcShape) owingBacklog() fcShape {
	s.name += "-owes-backlog"
	s.owesBacklog = true
	return s
}

var (
	fcDecode     = fcShape{name: "decode", role: domain.RoleDecode, ranks: 1}
	fcPD         = fcShape{name: "pd-decode", role: domain.RoleDecode, pd: true, ranks: 1}
	fcAggregated = fcShape{name: "aggregated", role: "", ranks: 1}
	fcDP8        = fcShape{name: "dp8", role: domain.RoleDecode, ranks: 8}
)

// fcPDPrefillBound is P/D with prefill the bottleneck.
var fcPDPrefillBound = fcShape{name: "pd-prefill-bound", role: domain.RoleDecode, pd: true, prefillBound: true, ranks: 1}

// The generator. Every engine rank follows a true ITL line, ITL(k) = a k + b
// with a seeded slope (fcGen), and holds requests of 1000 in / 500 out, 1250
// tokens of KV each; its batch cap (512) never binds. At a KV fraction k a
// rank runs k x KV / 1250 sequences and completes them at that over ITL(k) x
// 500 a second (fcRate). The ITL a replica reports carries fcITLNoise. The
// fleet saturates at kTrue, 0.85-0.95 of its KV: above k1 (0.8), so a k2
// learned there cannot move capacity.
const (
	fcPods      = 2
	fcKVReq     = 1250.0
	fcOutput    = 500.0
	fcMaxSeqs   = 512
	fcRouterQ   = 40 // the router's queue on the first flow-controlled cycle
	fcWarmup    = 16 // cycles of the ramp the line is fitted on
	fcLearnFor  = 20 // cycles the learned run's engines queue
	fcSaturated = 16 // cycles under flow control, in both runs
	// fcKPrice is the fill the derived mu is priced at, and the fill the
	// autoscaler targets: kvCacheThreshold x scaleUpThreshold (pricingK).
	fcKPrice = 0.8 * floorTarget
	// fcRateNoise is the relative noise on what a replica completes: a
	// reading repeated exactly is not a new sample (exactRepeatLocked).
	fcRateNoise = 0.01
	// fcITLNoise is the relative noise on the ITL a replica reports, so the
	// fitted line, and the mu derived from it, is not the truth by
	// construction.
	fcITLNoise = 0.02
	// fcITLB is the true line's intercept, away from the 6 ms default.
	fcITLB = 0.010
)

// fcGen is one seed's draw: the fill the fleet saturates at, the arrivals
// over what it serves, the true line's slope, and the seed the noise streams
// are drawn from (the same in both runs of a seed).
type fcGen struct {
	kTrue, rho, slope float64
	// prefillOver is, on P/D, one prefill replica's true mu over what the
	// decode fleet serves full: 1.5-3 where decode binds, 0.5-0.8 where
	// prefill does (fcShape.prefillBound).
	prefillOver float64
	seed        uint64
}

// fcGenFor draws seed's generator for shape s: kTrue 0.85-0.95, rho
// 1.1-1.5, the line's slope 0.03-0.10 s per unit of k, and prefillOver.
// A prefill-bound fleet's rho is 2.2-3.0 of what its one prefill replica
// serves, so prefill still binds at the two replicas the analyzer orders
// for it (one doubling, floor.LowerBoundGrowthCap): below 2 the ordered
// prefill outruns the arrivals, and decode is owed the backlog it then
// hands on, as where decode binds.
func fcGenFor(s fcShape, seed uint64) fcGen {
	r := propRand(seed)
	g := fcGen{seed: seed}
	g.kTrue = 0.85 + 0.1*r.Float64()
	g.rho = 1.1 + 0.4*r.Float64()
	g.slope = 0.03 + 0.07*r.Float64()
	u := r.Float64()
	g.prefillOver = 1.5 + 1.5*u
	if s.prefillBound {
		g.prefillOver = 0.5 + 0.3*u
		g.rho = 2.2 + 2*(g.rho-1.1)
	}
	return g
}

// kAt is the KV fraction at which one pod of ranks completes rate a second
// on g's line (fcGen.rate is increasing in k), by bisection over (0, 1).
func (g fcGen) kAt(rate float64, ranks int) float64 {
	lo, hi := 0.0, 1.0
	for range 60 {
		mid := (lo + hi) / 2
		if g.rate(mid, ranks) < rate {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}

// itl is the true ITL at KV fraction k.
func (g fcGen) itl(k float64) float64 { return g.slope*k + fcITLB }

// rate is one pod's true completions a second at KV fraction k.
func (g fcGen) rate(k float64, ranks int) float64 {
	return float64(ranks) * k * float64(propKv) / fcKVReq / g.itl(k) / fcOutput
}

// fcRow is one pod of shape s at KV fraction k with queue waiting on its
// engines, completing rate and reporting itl.
func fcRow(s fcShape, pod string, k float64, queue int, rate, itl float64, ready time.Time) domain.ReplicaMetrics {
	kv := propKv * int64(s.ranks)
	rm := makeReplicaMetrics(pod, "v", int64(k*float64(kv)), kv, queue, 1000, fcOutput)
	rm.KvUsageInstant, rm.KvUsageInstantKnown = rm.KvCacheUsage, true
	rm.KvCacheUsageKnown = true
	rm.RequestRate, rm.RequestRateKnown = rate, true
	rm.GenerationTokenRate, rm.GenerationTokenRateKnown = rate*fcOutput, true
	rm.PrefillComputedTokenRate = rate * 1000
	rm.AvgITL, rm.AvgITLKnown = itl, true
	rm.ReadySince = ready
	if s.ranks > 1 {
		rm.EngineRanks = s.ranks
		rm.MaxRankQueueLength = queue / s.ranks
		rm.MaxRankKvCacheUsage = k
	}
	if s.pd {
		rm.CapacityQueueKnown, rm.CapacityQueueLength = true, queue
	}
	return rm
}

// fcStates is shape s's variant states.
func fcStates(s fcShape) []domain.VariantReplicaState {
	v := domain.VariantReplicaState{VariantName: "v", AcceleratorName: "H200", CurrentReplicas: fcPods, GPUsPerReplica: s.ranks, Role: s.role}
	if !s.pd {
		return []domain.VariantReplicaState{v}
	}
	return []domain.VariantReplicaState{v,
		{VariantName: "p", AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1, Role: domain.RolePrefill}}
}

// fcOutcome is what one run priced on its last cycle.
type fcOutcome struct {
	// replicas is v's role's demand in replicas at the scale-up target, and
	// perReplica the per-replica capacity.
	replicas, perReplica float64
	// measuredMu is the measured mu on record, 0 when none, and k2 the k2
	// learner's figure (propRig.k2), 0 when none.
	measuredMu, k2 float64
	// queue is the router's queue on the last cycle, and lambda the
	// arrival rate.
	queue, lambda float64
	// prefillOrdered is, on P/D, the prefill replicas the analyzer orders:
	// prefill's demand over its per-replica capacity at the target, rounded
	// up. prefillMu is one prefill replica's true mu, and horizon the
	// floor's drain horizon (floor.DrainHorizon over both roles' starts).
	prefillOrdered, prefillMu, horizon float64
	// mu is the service rate v was priced on (domain.ServiceRateSignal) and
	// source where it came from.
	mu     float64
	source string
}

// runFlowControl runs shape s on g: a ramp the ITL line is fitted on, then
// fcSaturated cycles of flow control with the fleet full at g.kTrue and
// arrivals at g.rho times what it serves: the excess waits at the router,
// from fcRouterQ up, and the engines' queues read empty. learned runs
// fcLearnFor cycles first at capacity with the queue on the engines instead
// (flow control off), so k2 and mu are learned there. wrap, when set, edits
// each of v's rows before the analyzer reads it (a mutant).
func runFlowControl(s fcShape, g fcGen, learned bool, wrap func(*domain.ReplicaMetrics)) (fcOutcome, error) {
	return runFlowControlLogged(context.Background(), s, g, learned, wrap)
}

// runFlowControlLogged is runFlowControl with Analyze logging to ctx's
// logger.
func runFlowControlLogged(ctx context.Context, s fcShape, g fcGen, learned bool, wrap func(*domain.ReplicaMetrics)) (fcOutcome, error) {
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "v", capacity.Record{AcceleratorName: "H200", GpuCount: s.ranks,
		EngineParams: keyParams(fcMaxSeqs)}, propStart)
	rig := newPropRig()
	rig.a = NewSaturationAnalyzer(store)
	rig.a.now = func() time.Time { return rig.clock }
	rig.ctx = ctx
	on := map[string]bool{}
	if s.owesBacklog {
		on[SwitchDecodeOwesRouterBacklog] = true
	}
	known := ReplaySwitchNames()
	for _, name := range s.switches {
		if slices.Contains(known, name) {
			on[name] = true
		}
	}
	if len(on) > 0 {
		if err := rig.a.SetSwitchesForReplay(on); err != nil {
			return fcOutcome{}, err
		}
	}
	ready := rig.clock.Add(-time.Hour)
	states := fcStates(s)
	itlNoise := propRand(g.seed + 3<<32)
	mu := g.rate(g.kTrue, s.ranks)
	prefillMu := g.prefillOver * fcPods * mu
	// prefillServes is what the prefill replica completes a second, set
	// per phase.
	prefillServes := 0.0
	fleet := func(k [fcPods]float64, queue int, rate [fcPods]float64) []domain.ReplicaMetrics {
		rows := make([]domain.ReplicaMetrics, 0, fcPods+1)
		for p := range fcPods {
			itl := g.itl(k[p]) * (1 + fcITLNoise*itlNoise.NormFloat64())
			rows = append(rows, fcRow(s, fmt.Sprintf("r%d", p), k[p], queue, rate[p], itl, ready))
			if wrap != nil {
				wrap(&rows[p])
			}
		}
		if s.pd {
			pre := satRow("p0", 20_000, 0, prefillServes)
			pre.VariantName, pre.ReadySince = "p", ready
			pre.GenerationTokenRate = prefillServes
			rows = append(rows, pre)
		}
		return rows
	}
	// The ramp: unsaturated, no queue anywhere, k from 0.20 to 0.625.
	for i := range fcWarmup {
		var k, rate [fcPods]float64
		lambda := 0.0
		for p := range fcPods {
			k[p] = 0.20 + 0.025*float64(i) + 0.05*float64(p)
			rate[p] = g.rate(k[p], s.ranks)
			lambda += rate[p]
		}
		prefillServes = lambda
		if _, err := rig.analyze(fleet(k, 0, rate), states, lambda, propCycle); err != nil {
			return fcOutcome{}, err
		}
	}
	full := [fcPods]float64{g.kTrue, g.kTrue}
	each := mu
	if s.prefillBound {
		// Decode takes what prefill passes on and is not full.
		each = prefillMu / fcPods
		k := g.kAt(each, s.ranks)
		full = [fcPods]float64{k, k}
	}
	served := func(r *rand.Rand) [fcPods]float64 {
		var rate [fcPods]float64
		for p := range rate {
			rate[p] = each * (1 + fcRateNoise*r.NormFloat64())
		}
		return rate
	}
	prefillServes = fcPods * each
	if learned {
		// Flow control off, the fleet at capacity: the queue stands on the
		// engines, and k2 and mu are learned.
		r := propRand(g.seed + 1<<32)
		for range fcLearnFor {
			if _, err := rig.analyze(fleet(full, 10*s.ranks, served(r)), states, fcPods*mu, propCycle); err != nil {
				return fcOutcome{}, err
			}
		}
	}
	// Flow control on.
	r := propRand(g.seed + 2<<32)
	lambda := g.rho * fcPods * each
	queue := float64(fcRouterQ)
	var res *domain.AnalyzerResult
	for i := range fcSaturated {
		if i > 0 {
			queue += (lambda - fcPods*each) * propCycle.Seconds()
		}
		q := int64(queue)
		rig.input = func(in *domain.AnalyzerInput) {
			in.SchedulerQueue = &domain.SchedulerQueueMetrics{QueueSize: q}
		}
		var err error
		if res, err = rig.analyze(fleet(full, 0, served(r)), states, lambda, propCycle); err != nil {
			return fcOutcome{}, err
		}
	}
	out := fcOutcome{measuredMu: rig.measuredMu(), k2: rig.k2(""), queue: queue, lambda: lambda, replicas: math.NaN(),
		prefillMu: prefillMu, horizon: floor.BacklogDrainSeconds}
	for _, st := range res.RoleStartSeconds {
		out.horizon = floor.DrainHorizon(out.horizon, st)
	}
	for _, vc := range res.VariantCapacities {
		if vc.VariantName == "p" && vc.PerReplicaCapacity > 0 {
			out.prefillOrdered = math.Ceil(res.RoleDemand[domain.RolePrefill] / (floorTarget * vc.PerReplicaCapacity))
		}
		if vc.VariantName == "v" {
			out.perReplica = vc.PerReplicaCapacity
			if vc.ServiceRate != nil {
				out.mu, out.source = vc.ServiceRate.Rate, vc.ServiceRate.RateSource
			}
		}
	}
	demand := res.TotalDemand
	if s.pd {
		demand = res.RoleDemand[domain.RoleDecode]
	}
	if out.perReplica > 0 {
		out.replicas = demand / (floorTarget * out.perReplica)
	}
	return out, nil
}

// fcTruth is what shape s needs on g, as run o priced it, in replicas: the
// arrivals and the router's queue drained over the floor's horizon
// (o.horizon), with every replica at the fill the autoscaler targets
// (fcKPrice), where the true line has it complete g.rate(fcKPrice).
//
// On P/D decode owes the queue too, at the rate prefill hands it on: every
// request the router holds reaches decode once prefill has taken it. So
// decode's backlog rate is the queue over the horizon, but no more than the
// prefill the analyzer orders (o.prefillOrdered) completes beyond the
// arrivals. Where decode binds that is the queue over the horizon; where
// prefill binds it is about none, and decode needs the arrivals alone.
func fcTruth(s fcShape, o fcOutcome, g fcGen) float64 {
	backlog := o.queue / o.horizon
	if s.pd {
		backlog = min(backlog, max(0, o.prefillOrdered*o.prefillMu-o.lambda))
	}
	return (o.lambda + backlog) / g.rate(fcKPrice, s.ranks)
}

// checkFlowControlFloor: shape s overloaded under flow control (fcGenFor),
// priced on its last cycle with the mu it learned with flow control off
// first (learned) and with none (never learned). A prefill-bound fleet's
// decode is never full and learns no mu either way: it is priced never
// learned only, and judged on the first bound alone, since there is no
// learned order to hold its cost to. Never learned:
//
//   - orders at least the truth (fcTruth), less propTruthTol: the side a
//     fallback errs to is more replicas;
//   - orders at most the larger of the learned order and the truth, plus
//     propTruthTol: more is a cost the learned mu would not have paid.
//
// No bound is drawn against the learned order from below. The learned mu is
// read at saturation (kTrue) and divided by the scale-up threshold, the
// derived one priced at the target fill and not divided again (replicaCost),
// so the learned order is the truth times mu(fcKPrice) / (target x
// mu(kTrue)), 1.04-1.12 on these lines: a floor under it at that ratio is the
// truth bound again.
//
// wrap, when set, edits each of v's rows in the never-learned run: the
// fallback under test, put back wrong (a mutant).
func checkFlowControlFloor(s fcShape, wrap func(*domain.ReplicaMetrics)) func(uint64) error {
	return func(seed uint64) error {
		g := fcGenFor(s, seed)
		lr := fcOutcome{replicas: math.Inf(-1)}
		if !s.prefillBound {
			var err error
			if lr, err = runFlowControl(s, g, true, nil); err != nil {
				return err
			}
			if !(lr.measuredMu > 0) || lr.source != domain.ServiceRateSourceMeasured {
				return violation(kindNotLearned, "%s: the learned run priced on %q mu %.2f (measured %.2f), not its measured mu", s.name, lr.source, lr.mu, lr.measuredMu)
			}
		}
		nl, err := runFlowControl(s, g, false, wrap)
		if err != nil {
			return err
		}
		truth := fcTruth(s, nl, g)
		msg := fmt.Sprintf("%s at %.3f of KV, rho %.2f, ITL slope %.3f, router queue %.0f, prefill ordered %.0f at mu %.2f: never learned ordered %.3f replicas (%s mu %.2f), learned %.3f (mu %.2f), truth %.3f (mu %.2f at the target fill)",
			s.name, g.kTrue, g.rho, g.slope, nl.queue, nl.prefillOrdered, nl.prefillMu, nl.replicas, nl.source, nl.mu, lr.replicas, lr.mu, truth, g.rate(fcKPrice, s.ranks))
		if math.IsNaN(nl.replicas) || math.IsNaN(lr.replicas) {
			return violation(kindNotLearned, "%s: no order", msg)
		}
		if nl.replicas < truth*(1-propTruthTol) {
			return violation(kindWrongSide, "%s: under the truth", msg)
		}
		if s.prefillBound {
			return nil
		}
		return holds(nl.replicas <= max(lr.replicas, truth)*(1+propTruthTol), kindOverPriced, "%s: over both", msg)
	}
}

// checkPrefillBoundDecodeFull: prefill-bound P/D whose decode reads full
// (0.70-0.80 of its KV, by seed) for reasons other than the router's queue,
// such as long requests. Prefill has no mu, decode still completes only what
// prefill passes on, so with SwitchDecodeOwesRouterBacklog on decode is
// owed none of the queue and orders what it orders off, within
// propTruthTol. A decode charged on its fill alone ordered 1.4-3.1x that
// (in review, seed 5 at 0.80: 8.13 decode replicas against a truth of 3.89).
func checkPrefillBoundDecodeFull(seed uint64) error {
	g := fcGenFor(fcPDPrefillBound, seed)
	fill := 0.70 + 0.025*float64((seed-1)%5)
	full := func(rm *domain.ReplicaMetrics) {
		rm.KvCacheUsage, rm.KvUsageInstant = fill, fill
		rm.TokensInUse = int64(fill * float64(rm.TotalKvCapacityTokens))
	}
	off, err := runFlowControl(fcPDPrefillBound, g, false, full)
	if err != nil {
		return err
	}
	on, err := runFlowControl(fcPDPrefillBound.owingBacklog(), g, false, full)
	if err != nil {
		return err
	}
	truth := fcTruth(fcPDPrefillBound, on, g)
	msg := fmt.Sprintf("prefill-bound P/D, decode at %.3f of KV, rho %.2f, router queue %.0f: decode ordered %.3f replicas on, %.3f off, truth %.3f",
		fill, g.rho, on.queue, on.replicas, off.replicas, truth)
	if math.IsNaN(on.replicas) || math.IsNaN(off.replicas) {
		return violation(kindNotLearned, "%s: no order", msg)
	}
	return holds(on.replicas <= off.replicas*(1+propTruthTol), kindOverPriced, "%s: on over off", msg)
}

// fcCompletionsHigh multiplies each decode row's completions by f: a
// completion rate read f times high.
func fcCompletionsHigh(f float64) func(*domain.ReplicaMetrics) {
	return func(rm *domain.ReplicaMetrics) {
		rm.RequestRate *= f
		rm.GenerationTokenRate *= f
	}
}

// checkCompletionsHigh: shape s with SwitchDecodeOwesRouterBacklog on and
// decode's completions read 1.10 and 1.20 times high. A decode charged on
// its served share alone took the whole queue at 1.10 where prefill binds
// (in review, seeds 3 and 5 at 2.4x the truth). The order stays within
// propTruthTol of the larger of the learned order (decode-bound) or the same
// reading with the switch off (prefill-bound), and the truth.
func checkCompletionsHigh(s fcShape) func(uint64) error {
	return func(seed uint64) error {
		g := fcGenFor(s, seed)
		for _, f := range []float64{1.10, 1.20} {
			on, err := runFlowControl(s.owingBacklog(), g, false, fcCompletionsHigh(f))
			if err != nil {
				return err
			}
			ref, err := runFlowControl(s, g, false, fcCompletionsHigh(f))
			if err != nil {
				return err
			}
			if !s.prefillBound {
				if ref, err = runFlowControl(s.owingBacklog(), g, true, nil); err != nil {
					return err
				}
			}
			truth := fcTruth(s, on, g)
			msg := fmt.Sprintf("%s, completions x%.2f, rho %.2f, router queue %.0f: decode ordered %.3f replicas on, reference %.3f, truth %.3f",
				s.name, f, g.rho, on.queue, on.replicas, ref.replicas, truth)
			if math.IsNaN(on.replicas) {
				return violation(kindNotLearned, "%s: no order", msg)
			}
			if !(on.replicas <= max(ref.replicas, truth)*(1+propTruthTol)) {
				return violation(kindOverPriced, "%s: over both", msg)
			}
		}
		return nil
	}
}

// switchKVAboveContext is the replay switch that prices a resident reading
// above the context as read (llm-d-incubation/llm-d-scaling-manager#188), by
// name. It is on from the constructor; the case names it so it stays on if
// the default moves.
const switchKVAboveContext = "kv-per-request-above-context"

// checkTwiceTheContext: prefill-bound P/D whose decode rows report their
// running count (RunningRequestsKnown, which fcRow does not set), each
// request reading twice the context, 2,500 tokens. With the switch that
// prices that reading on, decode's mu halves and its served share doubles,
// past what a right mu allows: in review, the two switches together
// ordered 27 decode replicas against a truth of 3.2. With
// SwitchDecodeOwesRouterBacklog on as well, decode orders what it orders
// with that switch alone, within propTruthTol, or the truth.
func checkTwiceTheContext(seed uint64) error {
	g := fcGenFor(fcPDPrefillBound, seed)
	twice := func(rm *domain.ReplicaMetrics) {
		rm.RunningRequests, rm.RunningRequestsKnown = int(float64(rm.TokensInUse)/(2*fcKVReq)), true
	}
	base := fcPDPrefillBound
	base.switches = []string{switchKVAboveContext}
	ref, err := runFlowControl(base, g, false, twice)
	if err != nil {
		return err
	}
	on, err := runFlowControl(base.owingBacklog(), g, false, twice)
	if err != nil {
		return err
	}
	truth := fcTruth(fcPDPrefillBound, on, g)
	msg := fmt.Sprintf("prefill-bound P/D at twice the context, rho %.2f, router queue %.0f: decode ordered %.3f replicas with both switches, %.3f with the KV one, truth %.3f",
		g.rho, on.queue, on.replicas, ref.replicas, truth)
	if math.IsNaN(on.replicas) {
		return violation(kindNotLearned, "%s: no order", msg)
	}
	return holds(on.replicas <= max(ref.replicas, truth)*(1+propTruthTol), kindOverPriced, "%s: over both", msg)
}

// checkK2AboveK1Capacity: each flow-control shape saturated at 0.85-0.95 of
// its KV, above k1, priced with k2 learned there (flow control off) and with
// none (flow control on). The learned run holds a k2 at or above k1, the
// other none, and the per-replica capacity is the same, k1, within 1e-9. A
// k2 above k1 cannot move capacity, so its absence costs none.
func checkK2AboveK1Capacity(seed uint64) error {
	g := fcGenFor(fcDecode, seed)
	g.rho = 1.2
	for _, s := range []fcShape{fcDecode, fcPD, fcAggregated, fcDP8} {
		lr, err := runFlowControl(s, g, true, nil)
		if err != nil {
			return err
		}
		nl, err := runFlowControl(s, g, false, nil)
		if err != nil {
			return err
		}
		k1 := float64(propKv*int64(s.ranks)) * 0.8
		if !(lr.k2 >= k1) {
			return violation(kindNotLearned, "%s saturated at %.3f of KV with the queue on its engines: k2 %.0f, not at or above k1 %.0f", s.name, g.kTrue, lr.k2, k1)
		}
		if nl.k2 != 0 {
			return violation(kindMoved, "%s under flow control: k2 %.0f learned with the engines' queues empty", s.name, nl.k2)
		}
		if !(lr.perReplica > 0 && nl.perReplica > 0) {
			return violation(kindNotLearned, "%s: no per-replica capacity (%.0f learned, %.0f not)", s.name, lr.perReplica, nl.perReplica)
		}
		if relErr(nl.perReplica, lr.perReplica) > 1e-9 || relErr(lr.perReplica, k1) > 1e-3 {
			return violation(kindMoved, "%s saturated at %.3f of KV: per-replica capacity %.0f with k2 %.0f learned, %.0f without (k1 %.0f)",
				s.name, g.kTrue, lr.perReplica, lr.k2, nl.perReplica, k1)
		}
	}
	return nil
}
