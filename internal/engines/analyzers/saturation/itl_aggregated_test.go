package saturation

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/shape"
)

// The ITL line of an aggregated variant (issue ev-shindin/llm-scaling-manager#432), through Analyze.

// aggVariant is one variant of an aggregatedRig fleet: its role label, the
// slope its replicas' ITL rises by, and whether they read inside the ITL
// band (below it they offer the window nothing).
type aggVariant struct {
	name, role string
	slope      float64
	inBand     bool
}

// aggregatedRig is one analyzer over a fleet of variants, all on the same
// engine configuration, accelerator and GPU count, so with reuse on they
// share one fingerprint. in and out are the request shape every replica
// reports, arrival the model's arrival rate (0: none stated), and noHold
// the policy's disableShapeChangeHold.
type aggregatedRig struct {
	a        *SaturationAnalyzer
	clock    time.Time
	variants []aggVariant
	reuse    bool
	in, out  float64
	arrival  float64
	noHold   bool
}

func newAggregatedRig(fitsAggregated, reuse bool, variants ...aggVariant) *aggregatedRig {
	store := capacity.NewStore()
	for _, v := range variants {
		store.Update("test-ns", "test-model", v.name, capacity.Record{AcceleratorName: "H100", GpuCount: 1,
			EngineParams: keyParams(256)}, propStart)
	}
	r := &aggregatedRig{a: NewSaturationAnalyzer(store), clock: propStart, variants: variants, reuse: reuse,
		in: 600, out: 200}
	r.a.now = func() time.Time { return r.clock }
	r.a.itlFitsAggregated = fitsAggregated
	return r
}

// run drives aggregatedCycles cycles (step), and returns the last cycle's
// result.
func (r *aggregatedRig) run(t *testing.T) *domain.AnalyzerResult {
	t.Helper()
	var res *domain.AnalyzerResult
	for range aggregatedCycles {
		res = r.step(t)
	}
	return res
}

// aggregatedReplicas is how many replicas each variant of the rig runs.
const aggregatedReplicas = 10

// step drives one cycle of aggregatedReplicas replicas per variant, at KV
// fills 0.20 to 0.65 (or 0.05, below the band), at the rig's shape and
// arrival rate.
func (r *aggregatedRig) step(t *testing.T) *domain.AnalyzerResult {
	t.Helper()
	states := make([]domain.VariantReplicaState, 0, len(r.variants))
	for _, v := range r.variants {
		states = append(states, domain.VariantReplicaState{VariantName: v.name, Role: v.role,
			AcceleratorName: "H100", CurrentReplicas: aggregatedReplicas, GPUsPerReplica: 1})
	}
	rows := make([]domain.ReplicaMetrics, 0, aggregatedReplicas*len(r.variants))
	for _, v := range r.variants {
		for i := range aggregatedReplicas {
			k := 0.20 + 0.05*float64(i)
			if !v.inBand {
				k = 0.05
			}
			rm := makeReplicaMetrics(fmt.Sprintf("%s-%d", v.name, i), v.name,
				int64(k*16000), 16000, 0, r.in, r.out)
			rm.KvUsageInstant, rm.KvUsageInstantKnown = k, true
			rm.AvgITL, rm.AvgITLKnown = v.slope*k+propTrueB, true
			rows = append(rows, rm)
		}
	}
	in := makeAnalyzerInput(rows, states)
	in.Config.(*config.ScalingPolicy).EnableLearnedStateReuse = r.reuse
	in.Config.(*config.ScalingPolicy).DisableShapeChangeHold = r.noHold
	in.ArrivalRate = r.arrival
	res, err := r.a.Analyze(context.Background(), in)
	require.NoError(t, err)
	r.clock = r.clock.Add(propCycle)
	return res
}

// aggregatedCycles is enough cycles for a variant reading inside the band
// to fit a line of its own.
const aggregatedCycles = 13

// line is the shared line a variant published, and its key; ok false when
// it published none.
func (r *aggregatedRig) line(variant string) (string, learnedLine, bool) {
	r.a.mu.Lock()
	defer r.a.mu.Unlock()
	for key, l := range r.a.itlLines {
		if strings.HasPrefix(l.fromVariant, "test-ns|test-model|"+variant+"|") {
			return key, l, true
		}
	}
	return "", learnedLine{}, false
}

// signal is a variant's service-rate signal in res.
func signal(t *testing.T, res *domain.AnalyzerResult, variant string) *domain.ServiceRateSignal {
	t.Helper()
	for _, vc := range res.VariantCapacities {
		if vc.VariantName == variant {
			require.NotNil(t, vc.ServiceRate, "%s has no service-rate signal", variant)
			return vc.ServiceRate
		}
	}
	t.Fatalf("%s is not in the result", variant)
	return nil
}

// An unlabeled (aggregated) variant fits a line of its own, publishes it,
// and reports the mu it derives: the service-rate signal has the line and a
// rate under source derived-report-only. Off, as before: no line and no
// rate.
func TestITLAggregated_AnUnlabeledVariantFitsALineAndDerivesMu(t *testing.T) {
	for _, role := range []string{"", domain.RoleBoth} {
		t.Run("role="+role, func(t *testing.T) {
			r := newAggregatedRig(true, false, aggVariant{name: "agg", role: role, slope: propTrueA, inBand: true})
			res := r.run(t)
			sig := signal(t, res, "agg")
			assert.True(t, sig.HasLine, "an aggregated variant fitted no line: %+v", sig)
			assert.InDelta(t, propTrueA, sig.LineSlope, 0.005)
			assert.InDelta(t, propTrueB, sig.LineIntercept, 0.002)
			assert.Equal(t, domain.ServiceRateSourceReportOnly, sig.RateSource, "its mu is not derived from its line and reported")
			assert.Greater(t, sig.Rate, 0.0)
			assert.Equal(t, domain.RoleBoth, sig.Role)
			_, _, ok := r.line("agg")
			assert.True(t, ok, "the aggregated line was not published")

			off := newAggregatedRig(false, false, aggVariant{name: "agg", role: role, slope: propTrueA, inBand: true})
			offSig := signal(t, off.run(t), "agg")
			assert.False(t, offSig.HasLine, "with the switch off an aggregated variant fits no line")
			assert.Empty(t, offSig.RateSource)
			_, _, ok = off.line("agg")
			assert.False(t, ok)
		})
	}
}

// A decode variant is priced the same with the switch on and off: the
// change reaches only an aggregated variant.
func TestITLAggregated_ADecodeVariantIsUnchanged(t *testing.T) {
	on := newAggregatedRig(true, false, aggVariant{name: "dec", role: domain.RoleDecode, slope: propTrueA, inBand: true})
	off := newAggregatedRig(false, false, aggVariant{name: "dec", role: domain.RoleDecode, slope: propTrueA, inBand: true})
	resOn, resOff := on.run(t), off.run(t)
	assert.Equal(t, resOff.VariantCapacities, resOn.VariantCapacities)
	assert.Equal(t, resOff.RoleDemand, resOn.RoleDemand)
	assert.True(t, signal(t, resOn, "dec").HasLine, "the decode variant fitted no line")
	keyOn, _, okOn := on.line("dec")
	keyOff, _, okOff := off.line("dec")
	require.True(t, okOn && okOff)
	assert.Equal(t, keyOff, keyOn, "the decode line moved key")
	assert.False(t, strings.HasSuffix(keyOn, "|"+domain.RoleBoth))
}

// Prefill stays out, with the switch on: a prefill variant whose rows carry
// an ITL fits no line, publishes none and derives no rate from one, beside a
// decode variant that does.
func TestITLAggregated_PrefillFitsNoLine(t *testing.T) {
	r := newAggregatedRig(true, false,
		aggVariant{name: "pre", role: domain.RolePrefill, slope: propTrueA, inBand: true},
		aggVariant{name: "dec", role: domain.RoleDecode, slope: propTrueA, inBand: true})
	res := r.run(t)
	pre := signal(t, res, "pre")
	assert.False(t, pre.HasLine, "prefill fitted a line: %+v", pre)
	assert.False(t, pre.ITLWindowed, "prefill kept an ITL window")
	assert.NotEqual(t, domain.ServiceRateSourceDerived, pre.RateSource)
	_, _, ok := r.line("pre")
	assert.False(t, ok, "prefill published a line")
	assert.True(t, signal(t, res, "dec").HasLine)
	for _, rep := range r.a.LearnerReports()["test-ns|test-model"] {
		if rep.Learner == learn.ITL {
			assert.NotEqual(t, "pre", rep.Variant, "prefill filed an ITL learner report")
		}
	}
}

// One model with an aggregated and a decode variant on the same engine
// configuration, reuse on: each fits and publishes its own line under its
// own key, neither borrows the other's, and a variant of either role with
// nothing of its own borrows only from its own role.
func TestITLAggregated_AnAggregatedAndADecodeVariantKeepSeparateLines(t *testing.T) {
	r := newAggregatedRig(true, true,
		aggVariant{name: "agg", role: "", slope: 2 * propTrueA, inBand: true},
		aggVariant{name: "dec", role: domain.RoleDecode, slope: propTrueA, inBand: true},
		aggVariant{name: "agg-idle", role: "", slope: propTrueA, inBand: false},
		aggVariant{name: "dec-idle", role: domain.RoleDecode, slope: propTrueA, inBand: false})
	res := r.run(t)

	aggKey, aggLine, ok := r.line("agg")
	require.True(t, ok, "the aggregated variant published no line")
	decKey, decLine, ok := r.line("dec")
	require.True(t, ok, "the decode variant published no line")
	assert.NotEqual(t, aggKey, decKey, "one key for both roles' lines")
	assert.Equal(t, decKey+"|"+domain.RoleBoth, aggKey, "the aggregated key is the decode key with its role")
	assert.InDelta(t, 2*propTrueA, aggLine.model.A, 0.01, "the aggregated line is not its own fit")
	assert.InDelta(t, propTrueA, decLine.model.A, 0.005, "the decode line is not its own fit")

	agg, dec := signal(t, res, "agg"), signal(t, res, "dec")
	assert.Equal(t, domain.ServiceRateSourceReportOnly, agg.RateSource, "the aggregated variant borrowed")
	assert.Equal(t, domain.ServiceRateSourceDerived, dec.RateSource, "the decode variant borrowed")
	assert.InDelta(t, aggLine.model.A, agg.LineSlope, 1e-12)
	assert.InDelta(t, decLine.model.A, dec.LineSlope, 1e-12)

	aggIdle, decIdle := signal(t, res, "agg-idle"), signal(t, res, "dec-idle")
	assert.InDelta(t, aggLine.model.A, aggIdle.LineSlope, 1e-12, "an idle aggregated variant borrowed another role's line")
	assert.InDelta(t, decLine.model.A, decIdle.LineSlope, 1e-12, "an idle decode variant borrowed another role's line")
	for _, vc := range res.VariantCapacities {
		for _, b := range vc.Borrowed {
			if b.Quantity != domain.BorrowedITLLine {
				continue
			}
			switch vc.VariantName {
			case "agg-idle":
				assert.Equal(t, "agg", b.Donor)
			case "dec-idle":
				assert.Equal(t, "dec", b.Donor)
			default:
				t.Errorf("%s borrowed a line from %s", vc.VariantName, b.Donor)
			}
		}
	}
}

// onAndOff runs scenario on a rig of one variant of role with the switch on
// and with it off, and returns each run's total demand per cycle and the
// on run's last service-rate signal.
func onAndOff(t *testing.T, role string, scenario func(r *aggregatedRig) []*domain.AnalyzerResult) (on, off []float64, sig *domain.ServiceRateSignal) {
	t.Helper()
	totals := func(fits bool) ([]float64, *domain.AnalyzerResult) {
		r := newAggregatedRig(fits, false, aggVariant{name: "v", role: role, slope: propTrueA, inBand: true})
		r.in, r.out = 1000, 500
		results := scenario(r)
		require.NotEmpty(t, results)
		out := make([]float64, len(results))
		for i, res := range results {
			out[i] = res.TotalDemand
		}
		return out, results[len(results)-1]
	}
	on, last := totals(true)
	off, _ = totals(false)
	return on, off, signal(t, last, "v")
}

// steps runs n cycles of r and returns their results.
func steps(t *testing.T, r *aggregatedRig, n int) []*domain.AnalyzerResult {
	t.Helper()
	out := make([]*domain.AnalyzerResult, 0, n)
	for range n {
		out = append(out, r.step(t))
	}
	return out
}

// The aggregated line is reported and sizes nothing (itlLineReportOnly,
// until ev-shindin/llm-scaling-manager#275): at steady load with 50 requests/s arriving, the variant
// reports the mu its line derives (about 0.396/s a replica, under source
// derived-report-only), and its demand is what it is with the switch off --
// 68,000 tokens of KV occupancy (10 replicas at fills 0.20-0.65 of 16,000)
// -- where a priced line would have ordered about 1,375,000
// (50 x 12,800 x 0.85 / 0.396).
func TestITLAggregated_TheDerivedMuIsReportedNotPriced(t *testing.T) {
	on, off, sig := onAndOff(t, "", func(r *aggregatedRig) []*domain.AnalyzerResult {
		r.run(t)
		r.arrival = 50
		return steps(t, r, 3)
	})
	assert.Equal(t, off, on, "the aggregated line moved the demand")
	assert.InDelta(t, 68000, on[len(on)-1], 1, "the occupancy alone")
	assert.True(t, sig.HasLine)
	assert.Equal(t, domain.ServiceRateSourceReportOnly, sig.RateSource)
	assert.InDelta(t, 0.396, sig.Rate, 0.001, "the reported mu")

	// A decode variant on the same run prices its line, as before.
	dec, _, decSig := onAndOff(t, domain.RoleDecode, func(r *aggregatedRig) []*domain.AnalyzerResult {
		r.run(t)
		r.arrival = 50
		return steps(t, r, 3)
	})
	assert.Equal(t, domain.ServiceRateSourceDerived, decSig.RateSource)
	assert.InDelta(t, 50*12800*0.85/decSig.Rate, dec[len(dec)-1], 0.005*dec[len(dec)-1], "decode: the floor's lambda x P x scaleUp / mu")
}

// Through a shape change (1,000/500 to 8,000/1,000), while the hold is
// outstanding and after it has ended, with the hold on and with it off, the
// aggregated line fitted under the old shape orders nothing and settles
// nothing: the demand and the hold are the switch-off run's on every cycle.
// A decode line settles the change and orders, as before.
func TestITLAggregated_AnOldShapeLineNeverOrders(t *testing.T) {
	for _, noHold := range []bool{false, true} {
		t.Run(fmt.Sprintf("noHold=%v", noHold), func(t *testing.T) {
			var outstanding []bool
			on, off, sig := onAndOff(t, "", func(r *aggregatedRig) []*domain.AnalyzerResult {
				r.arrival, r.noHold = 100, noHold
				r.run(t)
				r.in, r.out = 8000, 1000
				// 30 cycles: 7.5 minutes, past the 5-minute hold and window.
				var res []*domain.AnalyzerResult
				outstanding = outstanding[:0]
				for range 30 {
					res = append(res, r.step(t))
					_, _, out := r.a.fleetShapeState("test-ns", "test-model")
					outstanding = append(outstanding, out)
				}
				return res
			})
			assert.Equal(t, off, on, "the old-shape aggregated line moved the demand")
			assert.Equal(t, domain.ServiceRateSourceReportOnly, sig.RateSource)
			if !noHold {
				// outstanding is the off run's (the scenario ran on, then off);
				// the on run is checked against it through the demand above.
				assert.True(t, outstanding[0], "precondition: the change raised a hold")
			}
		})
	}
	dec, decOff, _ := onAndOff(t, domain.RoleDecode, func(r *aggregatedRig) []*domain.AnalyzerResult {
		r.arrival = 100
		r.run(t)
		r.in, r.out = 8000, 1000
		return steps(t, r, 8)
	})
	assert.Equal(t, decOff, dec, "decode is the same with the switch on or off")
	assert.Greater(t, dec[len(dec)-1], 10*0.85*aggregatedReplicas*12800, "decode: its own line orders through the change")
}

// A line kept through light load (the held slope, issue ev-shindin/llm-scaling-manager#284): fitted in the
// band, then 35 minutes below it, so its window ages under the 10 readings a
// fit needs and the slope is held. Arrivals then come at 100/s. The held
// aggregated line is reported and orders nothing.
func TestITLAggregated_AHeldLineNeverOrders(t *testing.T) {
	on, off, sig := onAndOff(t, "", func(r *aggregatedRig) []*domain.AnalyzerResult {
		r.run(t)
		r.variants[0].inBand = false
		steps(t, r, 140)
		r.arrival = 100
		return steps(t, r, 3)
	})
	assert.Equal(t, off, on, "the held aggregated line moved the demand")
	assert.True(t, sig.HasLine, "precondition: the line is kept")
	assert.Less(t, sig.LineSamples, 10, "precondition: the window holds too few readings to fit, so the slope is held")
	assert.Equal(t, domain.ServiceRateSourceReportOnly, sig.RateSource)
}

// The learner report names an aggregated variant's own role, and with the
// switch off, decode, as every report did before.
func TestITLAggregated_TheLearnerReportNamesTheRole(t *testing.T) {
	for _, tc := range []struct {
		on   bool
		want string
	}{{true, domain.RoleBoth}, {false, domain.RoleDecode}} {
		t.Run(fmt.Sprintf("on=%v", tc.on), func(t *testing.T) {
			a := NewSaturationAnalyzer(capacity.NewStore())
			a.itlFitsAggregated = tc.on
			r := itl.Reading{Model: itl.Model{A: 0.04, B: 0.008}, Tier: itl.TierOLS}
			a.noteITLLearner("ns", "m", "v", "", "ns|m|v|H100|1", r, propStart, logr.Discard())
			a.closeITLCycle("ns", "m", map[string]string{"v": ""})
			reps := a.LearnerReports()["ns|m"]
			require.Len(t, reps, 1)
			assert.Equal(t, tc.want, reps[0].Role)
		})
	}
}

// A forgotten aggregated variant's report names its role too, and one the
// cycle does not name is reported as decode.
func TestITLAggregated_AForgottenVariantIsReportedUnderItsRole(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	a.mu.Lock()
	a.itlForgotten["ns|m"] = map[string][]learn.Forgotten{"agg": {{}}, "gone": {{}}}
	a.mu.Unlock()
	a.closeITLCycle("ns", "m", map[string]string{"agg": ""})
	got := map[string]string{}
	for _, rep := range a.LearnerReports()["ns|m"] {
		got[rep.Variant] = rep.Role
	}
	assert.Equal(t, map[string]string{"agg": domain.RoleBoth, "gone": domain.RoleDecode}, got)
}

// The GPS check reads an aggregated replica's line with the switch on, as it
// reads a decode replica's, and never a prefill replica's.
func TestITLAggregated_TheGPSCheckReadsAnAggregatedLine(t *testing.T) {
	for _, tc := range []struct {
		role string
		on   bool
		want bool
	}{
		{"", true, true},
		{"", false, false},
		{domain.RoleDecode, false, true},
		{domain.RolePrefill, true, false},
	} {
		t.Run(fmt.Sprintf("role=%q,on=%v", tc.role, tc.on), func(t *testing.T) {
			a := NewSaturationAnalyzer(capacity.NewStore())
			a.itlFitsAggregated = tc.on
			core, logs := observer.New(zapcore.Level(-logging.DEFAULT))
			fleet := shape.New(1000, 6000, 0)
			rm := makeReplicaMetrics("p-0", "v", 400_000, tracedKv, 10, 1000, 6000)
			rm.KvUsageInstant = 0.5
			// Half the token rate the line predicts: a mismatch to report.
			rm.GenerationTokenRate = itl.TokenRate(runPModel, 0.5, float64(tracedKv), fleet.KVreq) / 2
			a.noteLineMismatch(runPModel, tracedParams, rm, tc.role, fleet.KVreq, "", zapr.NewLogger(zap.New(core)))
			assert.Equal(t, tc.want, logs.FilterMessage("itl-gps-mismatch").Len() > 0)
		})
	}
}
