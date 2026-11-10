package saturation

import (
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// Property cases for the throughput floor (inventory row 11), read from the
// AnalyzerResult the saturation analyzer returns.

func floorCases() []propCase {
	return []propCase{
		{class: classKnownTruth, learner: "floor", name: "steady-order", family: familyDirect, tuning: tunedDefault, check: checkFloorTruth(nil)},
		{class: classMonotonicity, learner: "floor", name: "lambda-and-queue-rise", family: familyDirect, tuning: tunedDefault, check: checkFloorMonotone},
		{class: classMonotonicity, learner: "floor", name: "standing-local-queue", family: familyDirect, tuning: tunedDefault, check: checkFloorDrainBound(nil)},
		{class: classEdges, learner: "floor", name: "backlog-grew", family: familyDirect, tuning: tunedDefault, seeds: cheapSeeds, check: checkBacklogEdge(tuningDefault)},
		{class: classMonotonicity, learner: "floor", name: "lower-mu-never-prices-less", family: familyDirect, tuning: tunedDefault, check: checkFloorMuMonotone(nil)},
		{class: classKnownTruth, learner: "floor", name: "standing-queue-keeps-occupancy", family: familyDirect, tuning: tunedDefault, check: checkFloorStandingQueue(nil)},
	}
}

// checkFloorStandingQueue: a decode variant of 1 to 6 replicas, each holding
// resident KV and a queue charged as residency, with lambda equal to what
// they complete, read twice a BacklogGrowthLookback apart with the queue the
// same or a request or so longer: a fleet at capacity whose queue stands. The
// floor never prices it under occupancy's figure, queue charge included, at
// any mu from 0.5x to 2x what a replica completes
// (llm-d-incubation/llm-d-scaling-manager#54). stub, when set, edits the
// analyzer before it prices (a mutant).
func checkFloorStandingQueue(stub func(*SaturationAnalyzer)) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		n := 1 + r.IntN(6)
		p := 100_000 + 300_000*r.Float64()
		perReq := 500 + 2500*r.Float64()
		mu := (1 + 9*r.Float64())
		rows := make([]domain.ReplicaMetrics, n)
		queues, residents := make([]int, n), make([]float64, n)
		lambda := 0.0
		for i := range n {
			c := mu * (0.5 + 1.5*r.Float64())
			lambda += c
			rows[i] = domain.ReplicaMetrics{PodName: fmt.Sprintf("r%d", i), VariantName: "v", RequestRate: c, RequestRateKnown: true}
			queues[i] = 20 + r.IntN(180)
			residents[i] = p * (0.5 + 0.5*r.Float64())
		}
		vcs := []domain.VariantCapacity{{VariantName: "v", Role: domain.RoleDecode, ReplicaCount: n, PerReplicaCapacity: p}}
		a := NewSaturationAnalyzer(capacity.NewStore())
		if stub != nil {
			stub(a)
		}
		clock := propStart
		a.now = func() time.Time { return clock }
		price := func(less int) (got, measured float64) {
			rcs := make([]capacity.ReplicaCapacity, n)
			for i := range n {
				q := max(queues[i]-less*(i%2), 0)
				charge := float64(q) * perReq
				measured += residents[i] + charge
				rcs[i] = capacity.ReplicaCapacity{VariantName: "v", PodName: rows[i].PodName, QueueLength: q,
					LocalQueueDemand: int64(charge), SaturatedThroughput: mu, SaturatedThroughputSamples: 2}
			}
			in := makeAnalyzerInput(rows, nil)
			in.ArrivalRate = lambda
			return a.applyThroughputFloor(in, in.Config.(*config.ScalingPolicy), rcs, vcs,
				measured, nil, nil, 0, false, logr.Discard()), measured
		}
		price(r.IntN(2))
		clock = clock.Add(BacklogGrowthLookback)
		got, measured := price(0)
		return holds(got >= measured*(1-1e-9), kindWrongSide,
			"%d replicas at capacity, lambda %.2f, mu %.2f, the queue standing: priced %.0f under occupancy's %.0f",
			n, lambda, mu, got, measured)
	}
}

// checkFloorMuMonotone: one measured state -- a decode variant of 1 to 6
// replicas, each holding resident KV and a queue charged as residency, each
// completing a rate, under an arrival rate from 0.2x to 1.4x what the fleet
// completes at the base rate -- priced by the floor at eight mu from 0.25x to
// 3x the base. Demand never rises as mu does: a lower mu never prices the
// same queue lower (llm-d-incubation/llm-d-scaling-manager#54). stub, when
// set, edits each analyzer before it prices (a mutant).
func checkFloorMuMonotone(stub func(*SaturationAnalyzer)) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		n := 1 + r.IntN(6)
		p := 100_000 + 300_000*r.Float64()
		perReq := 500 + 2500*r.Float64()
		base := 1 + 9*r.Float64()
		lambda := float64(n) * base * (0.2 + 1.2*r.Float64())
		rows := make([]domain.ReplicaMetrics, n)
		rcs := make([]capacity.ReplicaCapacity, n)
		measured := 0.0
		for i := range n {
			pod := fmt.Sprintf("r%d", i)
			q := r.IntN(200)
			charge := float64(q) * perReq
			measured += p*(0.5+0.5*r.Float64()) + charge
			rows[i] = domain.ReplicaMetrics{PodName: pod, VariantName: "v",
				RequestRate: base * (0.3 + 0.9*r.Float64()), RequestRateKnown: true}
			rcs[i] = capacity.ReplicaCapacity{VariantName: "v", PodName: pod, QueueLength: q,
				LocalQueueDemand: int64(charge), SaturatedThroughputSamples: 2}
		}
		vcs := []domain.VariantCapacity{{VariantName: "v", Role: domain.RoleDecode, ReplicaCount: n, PerReplicaCapacity: p}}
		prev, prevMu := math.Inf(1), 0.0
		for k := range 8 {
			mu := base * (0.25 + 2.75*float64(k)/7)
			for i := range rcs {
				rcs[i].SaturatedThroughput = mu
			}
			a := NewSaturationAnalyzer(capacity.NewStore())
			if stub != nil {
				stub(a)
			}
			in := makeAnalyzerInput(rows, nil)
			in.ArrivalRate = lambda
			got := a.applyThroughputFloor(in, in.Config.(*config.ScalingPolicy), slices.Clone(rcs), vcs,
				measured, nil, nil, 0, false, logr.Discard())
			if got > prev*(1+1e-9) {
				return violation(kindFell, "%d replicas, lambda %.2f: at mu %.3f the floor priced %.0f, at mu %.3f %.0f: the lower mu priced less",
					n, lambda, prevMu, prev, mu, got)
			}
			prev, prevMu = got, mu
		}
		return nil
	}
}

// The floor's shipped figures the cases price against: the test policy's
// scale-up target (makeAnalyzerInput) and the drain time.
const (
	floorTarget = 0.85
	floorDrainT = 60.0 // seconds
)

// floorRig is an aggregated variant whose measured mu is about 1 req/s,
// learned on four replicas over four saturated cycles a minute apart
// (imbFloorFleetAt's seed).
func floorRig() (*propRig, error) {
	rig := newPropRig()
	for i := range 4 {
		seed := make([]domain.ReplicaMetrics, 0, 4)
		for _, p := range []string{"r0", "r1", "r2", "r3"} {
			seed = append(seed, imbRow(p, "v", 0.95, 10, 1000, 1000, []float64{1.0, 1.02, 0.98, 1.01}[i]))
		}
		if _, err := rig.analyze(seed, aggStates(4), 4, ThroughputSampleSpacing+time.Second); err != nil {
			return nil, err
		}
	}
	return rig, nil
}

// impliedReplicas is the demand a result prices, in replicas at the target.
func impliedReplicas(res *domain.AnalyzerResult) float64 {
	for _, vc := range res.VariantCapacities {
		if vc.VariantName == "v" && vc.PerReplicaCapacity > 0 {
			return res.TotalDemand / (floorTarget * vc.PerReplicaCapacity)
		}
	}
	return math.NaN()
}

// floorOrder is what the floor orders for rows at lambda, settled, in
// replicas, with the measured mu it priced from; a mutant edits it.
type floorOrder func(rows []domain.ReplicaMetrics, lambda float64) (replicas, mu float64, err error)

func floorImplied(rows []domain.ReplicaMetrics, lambda float64) (float64, float64, error) {
	rig, err := floorRig()
	if err != nil {
		return 0, 0, err
	}
	var res *domain.AnalyzerResult
	start := rig.clock
	for rig.clock.Sub(start) <= IdleSettledFor+K2SettleLookback+2*propCycle {
		if res, err = rig.analyze(rows, aggStates(len(rows)), lambda, propCycle); err != nil {
			return 0, 0, err
		}
	}
	return impliedReplicas(res), rig.measuredMu(), nil
}

// checkFloorTruth: a steady fleet with no queue -- two of its six replicas
// idle -- is ordered lambda / (mu x target), mu the measured mu the analyzer
// holds.
func checkFloorTruth(order floorOrder) func(uint64) error {
	if order == nil {
		order = floorImplied
	}
	return func(seed uint64) error {
		r := propRand(seed)
		lambda := 2 + 2*r.Float64()
		rows := make([]domain.ReplicaMetrics, 6)
		for p := range rows {
			rate, frac := lambda/4, 0.4
			if p >= 4 {
				rate, frac = 0, 0.02 // idle
			}
			rows[p] = imbRow(fmt.Sprintf("r%d", p), "v", frac, 0, 1000, 1000, rate)
		}
		got, mu, err := order(rows, lambda)
		if err != nil {
			return err
		}
		if !(mu > 0) || math.IsNaN(got) {
			return violation(kindNotLearned, "no measured mu (%.3f) or no order (%.2f)", mu, got)
		}
		want := lambda / (mu * floorTarget)
		return holds(relErr(got, want) <= propTruthTol, kindBiased, "lambda %.2f at mu %.3f: ordered %.2f replicas against %.2f", lambda, mu, got, want)
	}
}

// checkFloorDrainBound: a queue of 300 pinned on one replica, standing, beside
// idle siblings: the order is bounded by what the fleet can drain -- lambda
// plus the replica's own queue up to what it drains in the drain time
// (mu x T).
func checkFloorDrainBound(order floorOrder) func(uint64) error {
	if order == nil {
		order = floorImplied
	}
	return func(seed uint64) error {
		r := propRand(seed)
		lambda := 2 + r.Float64()
		rows := []domain.ReplicaMetrics{
			imbRow("r0", "v", 0.95, 300, 1000, 1000, 1.0),
			imbRow("r1", "v", 0.10, 0, 1000, 1000, 0.33),
			imbRow("r2", "v", 0.10, 0, 1000, 1000, 0.33),
			imbRow("r3", "v", 0.10, 0, 1000, 1000, 0.34),
		}
		got, mu, err := order(rows, lambda)
		if err != nil {
			return err
		}
		if !(mu > 0) || math.IsNaN(got) {
			return violation(kindNotLearned, "no measured mu (%.3f) or no order (%.2f)", mu, got)
		}
		bound := (lambda + math.Min(300, mu*floorDrainT)/floorDrainT) / (mu * floorTarget) * (1 + propTruthTol)
		return holds(got <= bound, kindUnbounded, "a standing queue of 300 on one replica at lambda %.2f: ordered %.2f replicas, over the %.2f it can drain", lambda, got, bound)
	}
}

// checkFloorMonotone: lambda rises and the queue with it, every replica
// busy; along the trajectory the order never falls by more than 2 %.
func checkFloorMonotone(seed uint64) error {
	r := propRand(seed)
	rig, err := floorRig()
	if err != nil {
		return err
	}
	prev, queue := 0.0, 0.0
	for i := range 40 {
		lambda := 2 + 4*float64(i)/40*(1+0.01*r.NormFloat64())
		queue += math.Max(0, lambda-4) * propCycle.Seconds() / 4
		rows := make([]domain.ReplicaMetrics, 4)
		for p := range rows {
			rows[p] = imbRow(fmt.Sprintf("r%d", p), "v", 0.95, int(queue), 1000, 1000, math.Min(lambda/4, 1))
		}
		res, err := rig.analyze(rows, aggStates(4), lambda, propCycle)
		if err != nil {
			return err
		}
		got := impliedReplicas(res)
		if math.IsNaN(got) {
			return violation(kindNotLearned, "cycle %d: no order", i)
		}
		if got < prev*(1-0.02) {
			return violation(kindFell, "cycle %d: lambda %.2f, queue %.0f per replica: the order fell %.2f -> %.2f", i, lambda, queue, prev, got)
		}
		prev = math.Max(prev, got)
	}
	return nil
}

// checkBacklogEdge: the backlog stands at 50 with 2 % autocorrelated noise; the
// noise-free backlog never grows, so backlogGrew never sets.
func checkBacklogEdge(tn config.AnalyzerTuning) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		a := NewSaturationAnalyzer(capacity.NewStore())
		clock := propStart
		a.now = func() time.Time { return clock }
		t := tuning{AnalyzerTuning: tn.WithDefaults()}
		noise := arNoise(r, 100, 0.02, 0.8)
		v := make([]bool, len(noise))
		for i, e := range noise {
			v[i] = a.noteBacklogGrowth("ns", "m", map[string]float64{"decode": 50 * (1 + e)}, t)["decode"]
			clock = clock.Add(propCycle)
		}
		return holds(reversals(v) == 0, kindFlipped, "a standing backlog of 50 with 2%% noise read as grown: %d reversals in %d cycles", reversals(v), len(v))
	}
}
