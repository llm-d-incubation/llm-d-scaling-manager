package saturation

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/floor"
)

// Issue llm-d-incubation/llm-d-scaling-manager#51. With no mu, prefill used to
// lose its share of the router queue (the queue's prompt tokens) from its
// demand. Prefill learns no mu while its own queue reads empty, and a backlog
// that waits in the router keeps its queue empty, so prefill was never
// charged for the router's backlog at all. SwitchPrefillKeepsRouterPrompts
// keeps the share.

// prefillRouterFleet is a P/D fleet whose prefill has no mu: prefill resident
// KV, decode resident KV, and a router queue of queued requests whose prompt
// tokens are prefill's share and whose generation is decode's.
type prefillRouterFleet struct {
	prefillReplicas, decodeReplicas int
	prefillResident, decodeResident float64
	queued                          float64
	prefillShare, decodeShare       float64
	// decodeMu is decode's mu, or 0 for none.
	decodeMu float64
	lambda   float64
}

// prefillRouterK1 is each replica's capacity in prefillRouterFleet.
const prefillRouterK1 = 900_000.0

// growthCap is floor.LowerBoundGrowthCap of f's ready prefill supply, at the
// scale-up threshold the analyzer sizes with.
func (f prefillRouterFleet) growthCap() float64 {
	cfg := makeAnalyzerInput(nil, nil).Config.(*config.ScalingPolicy)
	scaleUp, _ := cfg.AnalyzerThresholds(domain.SaturationAnalyzerName)
	return floor.LowerBoundGrowthCap(scaleUp, float64(f.prefillReplicas)*prefillRouterK1)
}

// price runs the floor over f with stub applied to the analyzer, and returns
// prefill's demand after it and the model total.
func (f prefillRouterFleet) price(stub func(*SaturationAnalyzer)) (prefill, total float64) {
	return f.priceLogged(stub, logr.Discard())
}

// priceLogged is price with the floor's lines written to logger.
func (f prefillRouterFleet) priceLogged(stub func(*SaturationAnalyzer), logger logr.Logger) (prefill, total float64) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	if stub != nil {
		stub(a)
	}
	clock := propStart
	a.now = func() time.Time { return clock }
	const k1 = prefillRouterK1
	vcs := []domain.VariantCapacity{
		{VariantName: "p", Role: domain.RolePrefill, ReplicaCount: f.prefillReplicas, PerReplicaCapacity: k1,
			TotalDemand: f.prefillResident},
		{VariantName: "d", Role: domain.RoleDecode, ReplicaCount: f.decodeReplicas, PerReplicaCapacity: k1,
			TotalDemand: f.decodeResident},
	}
	rcs := make([]capacity.ReplicaCapacity, 0, f.prefillReplicas+f.decodeReplicas)
	for i := range f.prefillReplicas {
		rcs = append(rcs, capacity.ReplicaCapacity{VariantName: "p", PodName: fmt.Sprintf("p%d", i)})
	}
	for i := range f.decodeReplicas {
		rc := capacity.ReplicaCapacity{VariantName: "d", PodName: fmt.Sprintf("d%d", i)}
		if f.decodeMu > 0 {
			rc.SaturatedThroughput, rc.SaturatedThroughputSamples = f.decodeMu, 2
		}
		rcs = append(rcs, rc)
	}
	roleDemand := map[string]float64{
		domain.RolePrefill: f.prefillResident + f.prefillShare,
		domain.RoleDecode:  f.decodeResident + f.decodeShare,
	}
	eppByRole := map[string]float64{domain.RolePrefill: f.prefillShare, domain.RoleDecode: f.decodeShare}
	total = f.prefillResident + f.decodeResident + f.prefillShare + f.decodeShare
	input := makeAnalyzerInput(nil, nil)
	input.ArrivalRate = f.lambda
	total = a.applyThroughputFloor(input, input.Config.(*config.ScalingPolicy), rcs, vcs, total, roleDemand, eppByRole,
		f.queued, false, logger)
	return roleDemand[domain.RolePrefill], total
}

// checkFloorPrefillRouterPrompts: a P/D fleet whose prefill has no mu, 1 to 4
// replicas of each role, decode with or without a mu, and a router queue. On
// even seeds the queue is 10 to 500 requests of 1000 to 8000 prompt tokens
// (less a prefix-cache hit of up to half), mostly under the cap. On odd seeds
// it is 2000 to 4000 requests of 6000 to 8000 prompt tokens (less a hit of up
// to a quarter), at least 9M tokens: over the cap of the largest fleet drawn
// (4 replicas), so the cap binds. Prefill's demand keeps the queue's prompt tokens -- work
// prefill takes first -- up to one doubling of its ready supply
// (floor.LowerBoundGrowthCap), and never under its own resident KV. stub,
// when set, edits the analyzer before it prices (a mutant).
func checkFloorPrefillRouterPrompts(stub func(*SaturationAnalyzer)) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		queued := float64(10 + r.IntN(491))
		in, out := 1000+7000*r.Float64(), 100+1900*r.Float64()
		hit := 0.5 * r.Float64()
		if seed%2 == 1 {
			queued = float64(2000 + r.IntN(2001))
			in = 6000 + 2000*r.Float64()
			hit = 0.25 * r.Float64()
		}
		f := prefillRouterFleet{
			prefillReplicas: 1 + r.IntN(4), decodeReplicas: 1 + r.IntN(4),
			prefillResident: 20_000 + 200_000*r.Float64(), decodeResident: 100_000 + 600_000*r.Float64(),
			queued:       queued,
			prefillShare: queued * in * (1 - hit),
			decodeShare:  queued * out,
			lambda:       1 + 30*r.Float64(),
		}
		if r.IntN(2) == 0 {
			f.decodeMu = 1 + 9*r.Float64()
		}
		prefill, total := f.price(stub)
		growth := f.growthCap()
		want := max(f.prefillResident, min(f.prefillResident+f.prefillShare, growth))
		if prefill < want*(1-1e-9) {
			return violation(kindWrongSide, "prefill with no mu, %d replicas, %.0f queued at the router: priced %.0f, under its resident %.0f plus the queue's prompts %.0f, capped at %.0f",
				f.prefillReplicas, queued, prefill, f.prefillResident, f.prefillShare, growth)
		}
		if prefill > max(f.prefillResident, growth)*(1+1e-9) {
			return violation(kindOverPriced, "prefill with no mu, %d replicas, %.0f queued at the router: priced %.0f, over one doubling of its ready supply %.0f",
				f.prefillReplicas, queued, prefill, growth)
		}
		return holds(total >= prefill, kindWrongSide, "the model total %.0f is under prefill's own demand %.0f", total, prefill)
	}
}

// On, prefill keeps the share and the model total carries it; off, both drop
// it by exactly the share, as before.
func TestPrefillWithNoMuKeepsTheRouterPrompts(t *testing.T) {
	f := prefillRouterFleet{
		prefillReplicas: 2, decodeReplicas: 2,
		prefillResident: 100_000, decodeResident: 400_000,
		queued: 200, prefillShare: 200 * 6000, decodeShare: 200 * 1000,
		lambda: 12,
	}
	on, onTotal := f.price(nil)
	off, offTotal := f.price(func(a *SaturationAnalyzer) { a.prefillKeepsPrompts = false })
	if want := f.prefillResident + f.prefillShare; on != want {
		t.Errorf("on: prefill priced %.0f, want its resident plus the router's prompts, %.0f", on, want)
	}
	if off != f.prefillResident {
		t.Errorf("off (the control): prefill priced %.0f, want its resident alone, %.0f", off, f.prefillResident)
	}
	if onTotal-offTotal != f.prefillShare {
		t.Errorf("the model total moved %.0f, want the share, %.0f", onTotal-offTotal, f.prefillShare)
	}
}

// A router backlog far over what prefill holds is kept only up to one
// doubling of prefill's ready supply (floor.LowerBoundGrowthCap), as the
// floor caps a prefill priced on anything but its own measurement; the model
// total drops what the cap cut. Prefill's own resident KV is never cut.
func TestPrefillWithNoMuKeepsTheRouterPromptsUpToOneDoubling(t *testing.T) {
	f := prefillRouterFleet{
		prefillReplicas: 2, decodeReplicas: 2,
		prefillResident: 200_000, decodeResident: 400_000,
		queued: 2000, prefillShare: 2000 * 8000, decodeShare: 2000 * 200,
		lambda: 30,
	}
	growth := f.growthCap()
	if f.prefillResident+f.prefillShare <= growth {
		t.Fatalf("precondition: the backlog %.0f is over the cap %.0f", f.prefillResident+f.prefillShare, growth)
	}
	// The cut is written at the shipped verbosity (-v=2, logging.DEFAULT),
	// where an operator reads it without raising -v.
	core, logs := observer.New(zapcore.Level(-logging.DEFAULT))
	prefill, total := f.priceLogged(nil, zapr.NewLogger(zap.New(core)))
	fields := requireLogged(t, logs, "scheduler-queue-prefill-share-capped")
	if cut, want := fields["cutTokens"], f.prefillResident+f.prefillShare-growth; cut != want {
		t.Errorf("cutTokens %v, want what the cap took off, %v", cut, want)
	}
	if prefill != growth {
		t.Errorf("prefill priced %.0f with %.0f queued prompt tokens, want one doubling of its ready supply, %.0f",
			prefill, f.prefillShare, growth)
	}
	want := growth + f.decodeResident + f.decodeShare
	if math.Abs(total-want) > 1e-6*want {
		t.Errorf("the model total %.0f, want the capped prefill plus decode, %.0f", total, want)
	}

	// Resident KV over the cap: none of the share is kept, and nothing of
	// prefill's own is cut.
	f.prefillResident = 2 * growth
	if prefill, _ = f.price(nil); prefill != f.prefillResident {
		t.Errorf("prefill priced %.0f, want its own resident %.0f, uncut", prefill, f.prefillResident)
	}
}

// A prefill with a mu is the floor's to price, switch or not: the switch
// reads only a prefill the floor does not price.
func TestPrefillWithAMuIsNotTouchedByTheRouterPromptsSwitch(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	b := NewSaturationAnalyzer(capacity.NewStore())
	b.prefillKeepsPrompts = false
	price := func(a *SaturationAnalyzer) (map[string]float64, float64) {
		const k1 = 900_000.0
		vcs := []domain.VariantCapacity{
			{VariantName: "p", Role: domain.RolePrefill, ReplicaCount: 2, PerReplicaCapacity: k1, TotalDemand: 100_000},
			{VariantName: "d", Role: domain.RoleDecode, ReplicaCount: 2, PerReplicaCapacity: k1, TotalDemand: 400_000},
		}
		rcs := []capacity.ReplicaCapacity{
			{VariantName: "p", PodName: "p0", SaturatedThroughput: 20, SaturatedThroughputSamples: 2},
			{VariantName: "p", PodName: "p1", SaturatedThroughput: 20, SaturatedThroughputSamples: 2},
			{VariantName: "d", PodName: "d0", SaturatedThroughput: 6, SaturatedThroughputSamples: 2},
			{VariantName: "d", PodName: "d1", SaturatedThroughput: 6, SaturatedThroughputSamples: 2},
		}
		roleDemand := map[string]float64{domain.RolePrefill: 100_000 + 1_200_000, domain.RoleDecode: 400_000 + 200_000}
		eppByRole := map[string]float64{domain.RolePrefill: 1_200_000, domain.RoleDecode: 200_000}
		in := makeAnalyzerInput(nil, nil)
		in.ArrivalRate = 12
		total := a.applyThroughputFloor(in, in.Config.(*config.ScalingPolicy), rcs, vcs, 1_900_000, roleDemand, eppByRole,
			200, false, logr.Discard())
		return roleDemand, total
	}
	onRoles, onTotal := price(a)
	offRoles, offTotal := price(b)
	if onTotal != offTotal || onRoles[domain.RolePrefill] != offRoles[domain.RolePrefill] || onRoles[domain.RoleDecode] != offRoles[domain.RoleDecode] {
		t.Errorf("a prefill with a mu moved with the switch: on %v %.0f, off %v %.0f", onRoles, onTotal, offRoles, offTotal)
	}
}
