package saturation

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The ITL line of an aggregated variant, by request shape (issue ev-shindin/llm-scaling-manager#275),
// through Analyze and in its parts.

// shapeWorld is the physics an aggregated pod follows in these tests: a step
// takes b + (a0 + c*g)*k at shape g = C*IL_eff/(KVreq*OL), emits 1+spec
// tokens per sequence, and a request spends sigma*IL_eff steps in prefill,
// holding KV and emitting no token.
type shapeWorld struct {
	b, a0, c, sigma float64
	capTokens       float64
	spec            float64
}

// shapeTruth is the world the rig runs: a 5 ms prefill token cost per
// thousand tokens a step, and 0.1 prefill steps per prompt token.
var shapeTruth = shapeWorld{b: 0.006, a0: 0.02, c: 2e-5, sigma: 0.1, capTokens: 160_000}

func (w shapeWorld) kvReq(ilEff, ol float64) float64 { return ilEff + ol/2 }

func (w shapeWorld) g(ilEff, ol float64) float64 {
	return w.capTokens * ilEff / (w.kvReq(ilEff, ol) * ol)
}

// step is the time of one engine step at fill k.
func (w shapeWorld) step(k, ilEff, ol float64) float64 {
	return w.b + (w.a0+w.c*w.g(ilEff, ol))*k
}

// mu is the completions a second one replica sustains at the pricing fill
// (shapePricingK): by Little's law, its resident sequences over the steps
// each spends, prefill included.
func (w shapeWorld) mu(ilEff, ol float64) float64 {
	k := shapePricingK
	n := k * w.capTokens / w.kvReq(ilEff, ol)
	return n / ((ol/(1+w.spec) + w.sigma*ilEff) * w.step(k, ilEff, ol))
}

// aggShapeRig drives one aggregated (unlabeled) variant of shapeReplicas
// replicas through Analyze, at fills 0.20 to 0.65 (or 0.05, below the ITL
// band, with idle), each reporting what its world says at the rig's shape
// and prefix-cache hit rate. cold more replicas join below the band at
// coldHit, completing coldLoad of what a warm replica does.
type aggShapeRig struct {
	a        *SaturationAnalyzer
	store    *capacity.Store
	clock    time.Time
	in, out  float64
	hit      float64
	arrival  float64
	idle     bool
	world    shapeWorld
	cold     int
	coldHit  float64
	coldLoad float64
	// specKnown is whether a speculating replica reports its accepted tokens.
	specKnown bool
	// borrower adds a second aggregated variant, "agg-b", on the same engine
	// configuration, its replicas below the band at borrowerHit, so it has
	// no line of its own and borrows agg's (reuse turns sharing on).
	borrower    bool
	borrowerHit float64
	reuse       bool
	// ranks is the engine ranks each replica runs (data parallel), 1 when 0;
	// its KV capacity is the whole replica's.
	ranks int
	// mutate, when set, changes every warm row before Analyze reads it.
	mutate func(*domain.ReplicaMetrics)
}

const shapeReplicas = 10

func newAggShapeRig(t *testing.T, switches map[string]bool) *aggShapeRig {
	t.Helper()
	return newAggShapeRigOn(t, switches, keyParams(256))
}

func newAggShapeRigOn(t *testing.T, switches map[string]bool, params *capacity.EngineParams) *aggShapeRig {
	t.Helper()
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "agg", capacity.Record{AcceleratorName: "H100", GpuCount: 1,
		EngineParams: params}, propStart)
	store.Update("test-ns", "test-model", "agg-b", capacity.Record{AcceleratorName: "H100", GpuCount: 1,
		EngineParams: params}, propStart)
	r := &aggShapeRig{a: NewSaturationAnalyzer(store), store: store, clock: propStart, in: 1000, out: 500,
		world: shapeTruth, specKnown: true}
	r.a.now = func() time.Time { return r.clock }
	require.NoError(t, r.a.SetSwitchesForReplay(switches))
	return r
}

// row is one replica at fill k and hit rate hit, its completions scaled by
// load.
func (r *aggShapeRig) row(name string, k, hit, load float64) domain.ReplicaMetrics {
	return r.rowOf("agg", name, k, hit, load)
}

// rowOf is row for a replica of variant.
func (r *aggShapeRig) rowOf(variant, name string, k, hit, load float64) domain.ReplicaMetrics {
	w := r.world
	ilEff := r.in * (1 - hit)
	rm := makeReplicaMetrics(name, variant, int64(k*w.capTokens), int64(w.capTokens), 0, r.in, r.out)
	if r.ranks > 1 {
		rm.EngineRanks = r.ranks
	}
	rm.KvUsageInstant, rm.KvUsageInstantKnown = k, true
	rm.PrefixCacheHitRate = hit
	stepSec := w.step(k, ilEff, r.out)
	rm.AvgITL, rm.AvgITLKnown = stepSec, true
	n := math.Round(k * w.capTokens / w.kvReq(ilEff, r.out))
	rm.RunningRequests, rm.RunningRequestsKnown = int(n), true
	rm.RequestRate, rm.RequestRateKnown = load*n/((r.out/(1+w.spec)+w.sigma*ilEff)*stepSec), true
	rm.GenerationTokenRate, rm.GenerationTokenRateKnown = rm.RequestRate*r.out, true
	if w.spec > 0 && r.specKnown {
		rm.SpecAcceptedPerStep, rm.SpecAcceptedPerStepKnown = w.spec, true
	}
	return rm
}

func (r *aggShapeRig) step(t *testing.T) *domain.AnalyzerResult {
	t.Helper()
	rows := make([]domain.ReplicaMetrics, 0, shapeReplicas+r.cold)
	for i := range shapeReplicas {
		k := 0.20 + 0.05*float64(i)
		if r.idle {
			k = 0.05
		}
		rm := r.row(fmt.Sprintf("agg-%d", i), k, r.hit, 1)
		if r.mutate != nil {
			r.mutate(&rm)
		}
		rows = append(rows, rm)
	}
	for i := range r.cold {
		rows = append(rows, r.row(fmt.Sprintf("agg-cold-%d", i), 0.10, r.coldHit, r.coldLoad))
	}
	states := []domain.VariantReplicaState{{VariantName: "agg", AcceleratorName: "H100",
		CurrentReplicas: shapeReplicas + r.cold, GPUsPerReplica: 1}}
	if r.borrower {
		for i := range shapeReplicas {
			rows = append(rows, r.rowOf("agg-b", fmt.Sprintf("agg-b-%d", i), 0.05, r.borrowerHit, 1))
		}
		states = append(states, domain.VariantReplicaState{VariantName: "agg-b", AcceleratorName: "H100",
			CurrentReplicas: shapeReplicas, GPUsPerReplica: 1})
	}
	in := makeAnalyzerInput(rows, states)
	in.ArrivalRate = r.arrival
	in.Config.(*config.ScalingPolicy).EnableLearnedStateReuse = r.reuse
	res, err := r.a.Analyze(context.Background(), in)
	require.NoError(t, err)
	r.clock = r.clock.Add(propCycle)
	return res
}

// occTestInput is an occupancyInput for variant "agg" over rows, in the
// default ITL band at 1,000 in / 500 out.
func occTestInput(key string, rows []domain.ReplicaMetrics, params *capacity.EngineParams) occupancyInput {
	return occupancyInput{key: key, variant: "agg", rows: rows, params: params, minK: 0.15, maxK: 0.80,
		fleetIL: 1000, fleetOL: 500}
}

func (r *aggShapeRig) steps(t *testing.T, n int) *domain.AnalyzerResult {
	t.Helper()
	var res *domain.AnalyzerResult
	for range n {
		res = r.step(t)
	}
	return res
}

// shapeOf is the shape kept with the variant's published line.
func (r *aggShapeRig) shapeOf(t *testing.T) lineShape {
	t.Helper()
	r.a.mu.Lock()
	defer r.a.mu.Unlock()
	for _, l := range r.a.itlLines {
		return l.shape.clone()
	}
	t.Fatal("the variant published no line")
	return lineShape{}
}

// state is a copy of the variant's shape state.
func (r *aggShapeRig) state(t *testing.T) aggState {
	t.Helper()
	return r.stateOf(t, "agg")
}

// stateOf is a copy of variant's shape state.
func (r *aggShapeRig) stateOf(t *testing.T, variant string) aggState {
	t.Helper()
	r.a.mu.Lock()
	defer r.a.mu.Unlock()
	for key, st := range r.a.itlAggregated {
		if strings.HasPrefix(key, "test-ns|test-model|"+variant+"|") {
			return *st
		}
	}
	t.Fatalf("%s has no shape state", variant)
	return aggState{}
}

// lifted runs the rig to its lift, from a fresh start: 13 cycles to fit,
// then 6 at 50 requests/s, and requires the figure to size.
func (r *aggShapeRig) lifted(t *testing.T) *domain.AnalyzerResult {
	t.Helper()
	r.steps(t, 13)
	r.arrival = 50
	res := r.steps(t, 6)
	require.Equal(t, domain.ServiceRateSourceDerivedAggregated, signal(t, res, "agg").RateSource, "precondition: lifted")
	return res
}

// shapePricingK is the fill the rig's policy prices mu at.
const shapePricingK = 0.8 * 0.85

// An aggregated variant whose line has held the GPS check for aggLiftCycles
// cycles sizes from it: the service-rate signal is derived (not reported
// only), at the rate the physics gives at the pricing fill -- which counts
// the steps a request spends in prefill, so it is about 17% under the rate
// k and ITL(k) alone would give at 1,000 in / 500 out with 0.1 prefill steps
// a prompt token. Its demand is not the switch-off run's.
func TestAggregatedShape_ALiftedLineSizesAtTheCorrectedRate(t *testing.T) {
	run := func(switches map[string]bool) (*domain.AnalyzerResult, *aggShapeRig) {
		r := newAggShapeRig(t, switches)
		r.steps(t, 13)
		r.arrival = 50
		return r.steps(t, 6), r
	}
	res, r := run(nil)
	sig := signal(t, res, "agg")
	want := r.world.mu(r.in, r.out)
	uncorrected := want * (r.out + r.world.sigma*r.in) / r.out
	assert.Equal(t, domain.ServiceRateSourceDerivedAggregated, sig.RateSource, "a lifted aggregated line does not size: %+v", sig)
	assert.InDelta(t, want, sig.Rate, 0.02*want, "the priced rate is not the physics' at the pricing fill (uncorrected %.4f)", uncorrected)
	assert.Less(t, sig.Rate, 0.9*uncorrected, "the prefill steps are not priced")

	off, _ := run(map[string]bool{SwitchITLAggregatedSizes: false})
	assert.Equal(t, domain.ServiceRateSourceReportOnly, signal(t, off, "agg").RateSource)
	assert.InDelta(t, want, signal(t, off, "agg").Rate, 0.02*want, "the reported rate is the corrected one with the lift off too")
	assert.NotEqual(t, off.TotalDemand, res.TotalDemand, "sizing from the line moved no demand")
}

// A line fitted at one shape does not price a heavier one without the shape
// term. The variant is fitted and lifted at 1,000 in / 500 out, then the
// shape moves to 4,000 in / 250 out (g 2.4 times as large) while its
// replicas fall below the ITL band, so its slope is held at the old shape
// and c is not identified: the variant derives no figure at all, reported or
// priced, and logs the case.
func TestAggregatedShape_AnOldLineDoesNotPriceAHeavierShape(t *testing.T) {
	r := newAggShapeRig(t, nil)
	r.steps(t, 13)
	r.arrival = 50
	require.Equal(t, domain.ServiceRateSourceDerivedAggregated, signal(t, r.steps(t, 6), "agg").RateSource, "precondition: lifted")
	r.in, r.out, r.idle = 4000, 250, true
	for i := range 30 {
		res := r.step(t)
		sig := signal(t, res, "agg")
		assert.NotEqual(t, domain.ServiceRateSourceDerivedAggregated, sig.RateSource, "cycle %d: the old-shape line sized the new shape", i)
		assert.NotEqual(t, domain.ServiceRateSourceReportOnly, sig.RateSource, "cycle %d: the old-shape line reported a figure for the new shape", i)
	}
	sh := r.shapeOf(t)
	assert.False(t, sh.cOK, "precondition: c is not identified from one shape")
	assert.InDelta(t, r.world.g(1000, 500), sh.g, 0.05*r.world.g(1000, 500), "the line lost the shape it was fitted at")
}

// Two shapes far enough apart identify c: run at 1,000/500 and then at
// 4,000/250, each past the shape hold, and the line keeps a point at each,
// whose slopes give the physics' c and decode slope.
func TestAggregatedShape_TwoShapesIdentifyTheShapeTerm(t *testing.T) {
	r := newAggShapeRig(t, nil)
	r.steps(t, 30)
	one := r.shapeOf(t)
	require.Len(t, one.points, 1, "one shape, one point")
	assert.False(t, one.cOK)
	r.in, r.out = 4000, 250
	r.steps(t, 30)
	two := r.shapeOf(t)
	require.Len(t, two.points, 2)
	require.True(t, two.cOK, "two shapes 2.4x apart did not identify c: %+v", two)
	assert.InDelta(t, r.world.c, two.c, 0.05*r.world.c)
	assert.InDelta(t, r.world.a0, two.a0, 0.05*r.world.a0)
}

// With c identified, a heavier shape is priced through it: after
// 1,000/500 and 4,000/250, the shape moves to 8,000/250 (g 1.9 times the
// heavier point's) with the replicas below the band, so the held line
// prices it through the shape term. The lift ended with the shape move, so
// the figure is reported, at the physics' rate for the new shape.
func TestAggregatedShape_AHeavierShapeIsPricedThroughTheTerm(t *testing.T) {
	r := newAggShapeRig(t, nil)
	r.steps(t, 30)
	r.in, r.out = 4000, 250
	r.steps(t, 30)
	require.True(t, r.shapeOf(t).cOK, "precondition: c identified")
	r.in, r.out, r.idle = 8000, 250, true
	sig := signal(t, r.step(t), "agg")
	want := r.world.mu(r.in, r.out)
	assert.Equal(t, domain.ServiceRateSourceReportOnly, sig.RateSource)
	assert.InDelta(t, want, sig.Rate, 0.03*want, "the heavier shape is not priced through the shape term")
}

// Never extrapolated toward a better price. With c identified, a shape
// lighter than the line's own (g* <= gFit) is priced at the line's slope
// as measured -- an ITL no lower than measured -- and any heavier one is
// moved by c, upward, however small the move. Without c a shape up to 20%
// heavier is priced at the worst case, a heavier one has no figure, and
// a line with no shape none either.
func TestAggregatedShape_NeverExtrapolatesTowardABetterPrice(t *testing.T) {
	const lineA, lineG, c = 0.03, 500.0, 2e-5
	for _, g := range []float64{50, 250, 400, 500} {
		a, priceCase, figure := resolveAggregatedPrice(lineA, lineG, g, c, true)
		require.True(t, figure)
		assert.Equal(t, aggCaseCovered, priceCase, "g=%v", g)
		assert.Equal(t, lineA, a, "g=%v: a lighter shape was priced below the measured slope", g)
	}
	for _, g := range []float64{510, 590, 1000} {
		a, priceCase, figure := resolveAggregatedPrice(lineA, lineG, g, c, true)
		require.True(t, figure)
		assert.Equal(t, aggCaseShapeTerm, priceCase, "g=%v: a heavier shape inside the band skipped the term", g)
		assert.InDelta(t, lineA+c*(g-lineG), a, 1e-12, "g=%v", g)
		assert.Greater(t, a, lineA)
	}
	a, priceCase, figure := resolveAggregatedPrice(lineA, lineG, 590, 0, false)
	require.True(t, figure)
	assert.Equal(t, aggCaseWorstCase, priceCase, "no c: inside the band, the worst case c allows")
	assert.InDelta(t, lineA*590/lineG, a, 1e-12)

	_, priceCase, figure = resolveAggregatedPrice(lineA, lineG, 1000, 0, false)
	assert.False(t, figure)
	assert.Equal(t, aggCaseUnpriced, priceCase)
	_, priceCase, figure = resolveAggregatedPrice(lineA, 0, 400, c, true)
	assert.False(t, figure)
	assert.Equal(t, aggCaseLineShapeUnknown, priceCase)
	_, priceCase, figure = resolveAggregatedPrice(lineA, lineG, 0, c, true)
	assert.False(t, figure)
	assert.Equal(t, aggCaseShapeUnknown, priceCase)
}

// c is fitted only from points that are this physics and far enough apart.
func TestAggregatedShape_FitShapeTerm(t *testing.T) {
	at := propStart
	pts := func(gs ...float64) []shapePoint {
		out := make([]shapePoint, 0, len(gs))
		for _, g := range gs {
			out = append(out, shapePoint{g: g, a: 0.02 + 2e-5*g, at: at})
		}
		return out
	}
	a0, c, ok := fitShapeTerm(pts(250, 620))
	require.True(t, ok)
	assert.InDelta(t, 0.02, a0, 1e-12)
	assert.InDelta(t, 2e-5, c, 1e-15)

	_, _, ok = fitShapeTerm(pts(250))
	assert.False(t, ok, "one shape identifies nothing")
	_, _, ok = fitShapeTerm(pts(250, 360))
	assert.False(t, ok, "two shapes under aggShapeMinSpread apart")
	_, _, ok = fitShapeTerm([]shapePoint{{g: 250, a: 0.03, at: at}, {g: 620, a: 0.02, at: at}})
	assert.False(t, ok, "a slope falling with prefill (c < 0) is not this physics")
	_, _, ok = fitShapeTerm([]shapePoint{{g: 250, a: 0.001, at: at}, {g: 620, a: 0.03, at: at}})
	assert.False(t, ok, "a negative decode slope (a0 <= 0) is not this physics")

	var s lineShape
	for i, g := range []float64{100, 110, 300, 700, 1500, 3000, 6000, 12000} {
		s.addPoint(shapePoint{g: g, a: 0.02 + 2e-5*g, at: at.Add(time.Duration(i) * time.Minute)})
	}
	require.Len(t, s.points, aggMaxShapePoints, "the points are bounded")
	assert.Equal(t, 300.0, s.points[0].g, "110 replaced 100 (within the tolerance), and the oldest went first")
	assert.True(t, s.cOK)
}

// sigmaRows is n replicas of variant "agg" at fill 0.5, each completing at
// the rate that gives it sigma prefill steps per prompt token at
// 1,000 in / 500 out.
func sigmaRows(n int, sigma float64) []domain.ReplicaMetrics {
	rows := make([]domain.ReplicaMetrics, 0, n)
	for i := range n {
		rm := makeReplicaMetrics(fmt.Sprintf("agg-%d", i), "agg", 80_000, 160_000, 0, 1000, 500)
		rm.KvUsageInstant, rm.KvUsageInstantKnown = 0.5, true
		rm.AvgITL, rm.AvgITLKnown = 0.02, true
		rm.RunningRequests, rm.RunningRequestsKnown = 64, true
		rm.RequestRate, rm.RequestRateKnown = 64/((500+sigma*1000)*0.02), true
		rows = append(rows, rm)
	}
	return rows
}

// The prefill steps are the difference of two near-equal figures, the
// residence and the output's own steps, so a reading under the output's
// steps is refused and counted, not read as a negative prefill; one exactly
// at them reads zero, the clamp; and the median holds through a reading far
// out.
func TestAggregatedShape_PrefillStepsSurviveANoisyReading(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	const key = "test-ns|test-model|agg|H100|1"
	rows := sigmaRows(5, 0.1)
	// Residence 10% under the output's own steps: refused.
	rows[1].RequestRate = 64 / (0.9 * 500 * 0.02)
	// A reading twenty times the rest.
	rows[3].RequestRate = 64 / ((500 + 2*1000) * 0.02)
	a.noteOccupancy(occTestInput(key, rows, nil), logr.Discard())
	a.mu.Lock()
	st := *a.itlAggregated[key]
	a.mu.Unlock()
	assert.Equal(t, int64(1), st.refused, "the reading under the output's steps was not refused")
	require.Len(t, st.sigmas, 4)
	for _, s := range st.sigmas {
		assert.GreaterOrEqual(t, s, 0.0, "a negative prefill was read")
	}
	sigma, ok := st.sigma()
	require.True(t, ok)
	assert.InDelta(t, 0.1, sigma, 1e-9, "the median moved with one noisy reading")

	// Exactly at the output's steps: zero prefill, not refused.
	b := NewSaturationAnalyzer(capacity.NewStore())
	b.noteOccupancy(occTestInput(key, sigmaRows(3, 0), nil), logr.Discard())
	b.mu.Lock()
	stb := *b.itlAggregated[key]
	b.mu.Unlock()
	assert.Equal(t, int64(0), stb.refused)
	got, ok := stb.sigma()
	require.True(t, ok)
	assert.InDelta(t, 0, got, 1e-9)
}

// A vLLM variant whose replicas report no running requests and no
// completion rate (a scrape that drops them) has no prefill reading, so its
// figure is never lifted: it stays reported only.
func TestAggregatedShape_NoPrefillReadingStaysReportOnly(t *testing.T) {
	r := newAggShapeRig(t, nil)
	var res *domain.AnalyzerResult
	for i := range 20 {
		if i == 13 {
			r.arrival = 50
		}
		res = r.stepWithout(t)
	}
	sig := signal(t, res, "agg")
	assert.Equal(t, domain.ServiceRateSourceReportOnly, sig.RateSource, "a variant with no prefill reading sized")
}

// stepWithout is step with no running-request count and no completion rate
// on any replica.
func (r *aggShapeRig) stepWithout(t *testing.T) *domain.AnalyzerResult {
	t.Helper()
	w := r.world
	rows := make([]domain.ReplicaMetrics, 0, shapeReplicas)
	for i := range shapeReplicas {
		k := 0.20 + 0.05*float64(i)
		rm := makeReplicaMetrics(fmt.Sprintf("agg-%d", i), "agg", int64(k*w.capTokens), int64(w.capTokens), 0, r.in, r.out)
		rm.KvUsageInstant, rm.KvUsageInstantKnown = k, true
		rm.AvgITL, rm.AvgITLKnown = w.step(k, r.in, r.out), true
		rm.GenerationTokenRate, rm.GenerationTokenRateKnown = k*w.capTokens/w.kvReq(r.in, r.out)/rm.AvgITL, true
		rows = append(rows, rm)
	}
	states := []domain.VariantReplicaState{{VariantName: "agg", AcceleratorName: "H100",
		CurrentReplicas: shapeReplicas, GPUsPerReplica: 1}}
	in := makeAnalyzerInput(rows, states)
	in.ArrivalRate = r.arrival
	res, err := r.a.Analyze(context.Background(), in)
	require.NoError(t, err)
	r.clock = r.clock.Add(propCycle)
	return res
}

// A lifted figure never prices above a measured rate of the variant's own,
// however thin; a figure that may (decode's) and a borrowed reading are left
// as they are.
func TestAggregatedShape_ALiftedFigureNeverPricesAboveTheMeasuredRate(t *testing.T) {
	derived := whatif.DerivedMu{Rate: 2.0, OK: true, CapAtMeasured: true}
	assert.Equal(t, 1.5, pricedDerivedRate(derived, throughputReading{rate: 1.5, samples: 1}))
	assert.Equal(t, 2.0, pricedDerivedRate(derived, throughputReading{rate: 3.0, samples: 1}), "a higher measurement does not raise it")
	assert.Equal(t, 2.0, pricedDerivedRate(derived, throughputReading{}), "no measurement")
	assert.Equal(t, 2.0, pricedDerivedRate(derived, throughputReading{rate: 1.5, borrowed: true}), "a borrowed reading is not the variant's own")
	derived.CapAtMeasured = false
	assert.Equal(t, 2.0, pricedDerivedRate(derived, throughputReading{rate: 1.5, samples: 1}), "decode's figure is unchanged")
}
