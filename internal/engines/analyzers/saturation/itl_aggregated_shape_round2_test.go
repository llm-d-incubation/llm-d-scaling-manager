package saturation

import (
	"math"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/shape"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// Review round 2 of the aggregated line by shape (issue ev-shindin/llm-scaling-manager#275).

// A heavier shape with no c, inside the band, is priced at A_g*g*/gFit: with
// A_g = A + c*gFit and A >= 0, never under the true slope A + c*g*, for any
// split of A_g between the decode slope and the prefill term. Past the band
// it has no figure.
func TestAggregatedShape_TheWorstCaseIsNeverUnderTheTrueSlope(t *testing.T) {
	for _, a := range []float64{0, 0.001, 0.01, 0.05} {
		for _, c := range []float64{0, 1e-6, 2e-5, 1e-4} {
			for _, gFit := range []float64{100, 500} {
				lineA := a + c*gFit
				if !(lineA > 0) {
					continue
				}
				for _, f := range []float64{1.001, 1.1, 1.2} {
					g := gFit * f
					priced, priceCase, figure := resolveAggregatedPrice(lineA, gFit, g, 0, false)
					require.True(t, figure)
					assert.Equal(t, aggCaseWorstCase, priceCase)
					assert.GreaterOrEqual(t, priced, a+c*g-1e-15,
						"A=%v c=%v gFit=%v g=%v: priced under the true slope", a, c, gFit, g)
				}
				_, priceCase, figure := resolveAggregatedPrice(lineA, gFit, gFit*1.21, 0, false)
				assert.False(t, figure)
				assert.Equal(t, aggCaseUnpriced, priceCase)
			}
		}
	}
}

// liftState is a lifted variant's shape state, under key.
func liftState(a *SaturationAnalyzer, key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.itlAggregated[key] = &aggState{lifted: true, agree: aggLiftCycles}
}

// B1: the lift drops on the third consecutive disagreeing cycle and not
// before, and a cycle with no comparable replica between them moves
// nothing: it neither ends the lift nor breaks the run.
func TestAggregatedShape_TheLiftDropsOnTheThirdDisagreeingCycle(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	const key = "test-ns|test-model|agg|H100|1"
	liftState(a, key)
	disagree := func() {
		a.noteAggregatedLift("agg", &aggCycle{key: key, figure: true, sigmaOK: true, errs: []float64{50}}, false, logr.Discard())
	}
	none := func() {
		a.noteAggregatedLift("agg", &aggCycle{key: key, figure: true, sigmaOK: true}, false, logr.Discard())
	}
	lifted := func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.itlAggregated[key].lifted
	}
	disagree()
	assert.True(t, lifted(), "dropped on the first disagreeing cycle")
	none()
	assert.True(t, lifted(), "a cycle with no comparable replica dropped the lift")
	disagree()
	assert.True(t, lifted(), "dropped on the second disagreeing cycle")
	none()
	disagree()
	assert.False(t, lifted(), "not dropped on the third disagreeing cycle")
}

// B2: a borrowed |both line prices at the lender's shape. Two aggregated
// variants share an engine configuration; the lender runs at a 0.7 hit rate
// and is lifted, and the borrower, below the band at a 0 hit rate, has no
// line of its own: its shape is 47% heavier than the line's, there is no c,
// so it has no figure (it is not priced at its own shape as if the line were
// fitted there), and its lift is its own.
func TestAggregatedShape_ABorrowedLineIsPricedAtTheLendersShape(t *testing.T) {
	r := newAggShapeRig(t, nil)
	r.reuse, r.borrower, r.hit, r.borrowerHit = true, true, 0.7, 0
	res := r.lifted(t)
	b := signal(t, res, "agg-b")
	assert.True(t, b.HasLine, "precondition: the borrower holds the lender's line")
	assert.Empty(t, b.RateSource, "the borrower priced a heavier shape at its own g")
	assert.False(t, r.stateOf(t, "agg-b").lifted, "the borrower took the lender's lift")
	assert.True(t, r.stateOf(t, "agg").lifted)
}

// B3: a held slope restored across a restart, through Analyze, prices only
// when it is the line it published. The replicas sit below the band, so the
// slope is held; its shape comes from the published line only when their
// slopes agree, and a slope that differs is line-shape-unknown: no figure.
func TestAggregatedShape_ARestoredHeldSlopeTakesTheShapeOfItsOwnLineOnly(t *testing.T) {
	src := newAggShapeRig(t, nil)
	src.lifted(t)
	doc := src.a.ExportLearnedState()
	require.Len(t, doc.ITLSlopes, 1, "precondition: one held slope")

	restored := func(slopeScale float64) *domain.ServiceRateSignal {
		d := doc
		d.ITLSlopes = map[string]SlopeSnapshot{}
		for k, sl := range doc.ITLSlopes {
			sl.A *= slopeScale
			d.ITLSlopes[k] = sl
		}
		r := newAggShapeRig(t, nil)
		r.clock, r.idle, r.arrival = src.clock, true, 50
		r.a.RestoreLearnedState(d)
		return signal(t, r.step(t), "agg")
	}
	same := restored(1)
	assert.True(t, same.HasLine, "precondition: the held slope prices")
	assert.Equal(t, domain.ServiceRateSourceReportOnly, same.RateSource, "the restored slope lost its line's shape")
	other := restored(1.1)
	assert.True(t, other.HasLine, "precondition: the held slope prices")
	assert.Empty(t, other.RateSource, "a held slope that is not its line took the line's shape")
}

// B4: a reading that is not a number gives no prefill reading, panics
// nowhere and lifts nothing; and a NaN sigma or g gives no figure, never an
// uncorrected one.
func TestAggregatedShape_ANaNReadingTakesNoPrefillReadingAndLiftsNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*domain.ReplicaMetrics)
	}{
		{"NaN prompt length", func(rm *domain.ReplicaMetrics) { rm.AvgInputTokens = math.NaN() }},
		{"NaN completion rate", func(rm *domain.ReplicaMetrics) { rm.RequestRate = math.NaN() }},
		{"NaN hit rate", func(rm *domain.ReplicaMetrics) { rm.PrefixCacheHitRate = math.NaN() }},
		{"infinite ITL", func(rm *domain.ReplicaMetrics) { rm.AvgITL = math.Inf(1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := NewSaturationAnalyzer(capacity.NewStore())
			rows := sigmaRows(5, 0.1)
			for i := range rows {
				tc.mutate(&rows[i])
			}
			require.NotPanics(t, func() { a.noteOccupancy(occTestInput("k", rows, nil), logr.Discard()) })
			a.mu.Lock()
			st := *a.itlAggregated["k"]
			a.mu.Unlock()
			assert.Empty(t, st.sigmas, "a reading that is not a number was taken")

			r := newAggShapeRig(t, nil)
			r.mutate = tc.mutate
			r.steps(t, 13)
			r.arrival = 50
			var res *domain.AnalyzerResult
			require.NotPanics(t, func() { res = r.steps(t, 6) })
			assert.NotEqual(t, domain.ServiceRateSourceDerivedAggregated, signal(t, res, "agg").RateSource)
		})
	}

	_, ok := occupancyFactor(500, 1000, math.NaN())
	assert.False(t, ok, "a NaN sigma corrected nothing and passed")
	_, priceCase, figure := resolveAggregatedPrice(0.03, 500, math.NaN(), 2e-5, true)
	assert.False(t, figure)
	assert.Equal(t, aggCaseShapeUnknown, priceCase)

	c := &cycle{a: NewSaturationAnalyzer(capacity.NewStore()), muDivisor: 500}
	ac := &aggCycle{figure: true, a: 0.03, sigma: math.NaN(), sigmaOK: true}
	rm := makeReplicaMetrics("agg-0", "agg", 80_000, 160_000, 0, 1000, 500)
	s := shape.New(1000, 500, 0)
	p := c.priceAggregated(ac, rm, itl.Model{A: 0.03, B: 0.006}, keyParams(256), 256, s, s, shapePricingK)
	assert.False(t, p.derived.OK, "a NaN sigma priced a figure")
	assert.Equal(t, aggCaseOccupancyUnknown, p.derived.Why)
}

// B5: with the shape term off, the lift switch on lifts nothing: the lift
// needs the shape term's figure.
func TestAggregatedShape_TheLiftNeedsTheShapeTerm(t *testing.T) {
	r := newAggShapeRig(t, map[string]bool{SwitchITLAggregatedShapeTerm: false, SwitchITLAggregatedSizes: true})
	r.steps(t, 13)
	r.arrival = 50
	sig := signal(t, r.steps(t, 6), "agg")
	assert.Equal(t, domain.ServiceRateSourceReportOnly, sig.RateSource, "sized with the shape term off")
}

// B6: an aggregated variant whose pods run eight engine ranks (data
// parallel) is checked and priced on the whole pod's KV (whatif.CapacityTokens),
// its sequences capped at eight ranks' batch: it lifts, at the physics' rate.
func TestAggregatedShape_AnEightRankVariantIsPricedOnTheWholePod(t *testing.T) {
	r := newAggShapeRig(t, nil)
	r.ranks = 8
	r.world.capTokens = 8 * 160_000
	res := r.lifted(t)
	want := r.world.mu(r.in, r.out)
	assert.InDelta(t, want, signal(t, res, "agg").Rate, 0.02*want)

	rm := makeReplicaMetrics("agg-0", "agg", int64(0.5*r.world.capTokens), int64(r.world.capTokens), 0, r.in, r.out)
	rm.EngineRanks = 8
	require.Equal(t, 8, rm.Ranks())
	assert.Equal(t, r.world.capTokens, whatif.CapacityTokens(keyParams(256), rm.Ranks(), rm.TotalKvCapacityTokens))
}

// The derived-mu line says when there has been no comparable cycle yet,
// rather than logging an error of 0.
func TestAggregatedShape_NoComparableCycleIsNotAZeroError(t *testing.T) {
	assert.Equal(t, aggNoComparableCycle, (&aggCycle{}).medErrLogged())
	assert.Equal(t, 12.5, (&aggCycle{medErr: 12.5, medErrOK: true}).medErrLogged())
}
