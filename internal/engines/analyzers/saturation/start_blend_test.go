package saturation

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
)

// Learner PR 5c (analyzer-learner.md, PR 5, changes A and C): the
// ScaledObject's replicaStartSeconds seed is the prior, and while fewer than
// startConfirmPods Pods are measured the published estimate blends the prior
// with their mean, held in [S/2, 2S] with a seed. Sizing reads the larger of
// the blend, the mean and the measured estimate, so it never reads a start
// shorter than one measured. Each test runs its switch on and, where the
// switch decides, off, and asserts both.

// blendAnalyzer is an analyzer with the two 5c switches set.
func blendAnalyzer(t *testing.T, readsSeed, blends bool) *SaturationAnalyzer {
	t.Helper()
	a := NewSaturationAnalyzer(capacity.NewStore())
	clock := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return clock }
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{
		SwitchStartReadsSeed: readsSeed, SwitchStartBlendsFirstSamples: blends}))
	return a
}

// blendStart brings up pods, one per sample, on confirmKey with seed (0 for
// none), the earlier ones still reporting, one cycle each, and returns the
// key's figures.
func blendStart(a *SaturationAnalyzer, seed float64, samples ...float64) startFigures {
	rows := make([]domain.ReplicaMetrics, 0, len(samples))
	for i, s := range samples {
		rows = append(rows, confirmReplica(fmt.Sprintf("p%d", i), s))
		at := a.now().Add(30 * time.Second)
		a.now = func() time.Time { return at }
		a.noteReplicaStartFor(startNote{key: confirmKey, namespace: "ns", modelID: "model",
			variant: confirmVariant, role: domain.RoleDecode, seed: seed}, rows, logr.Discard())
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.startFiguresLocked(confirmKey, seed)
}

// The design's cases (2.6), each the published estimate and the bound it
// must hold, and the reader cases: what sizing reads.
func TestStartBlend_TheDesignsCases(t *testing.T) {
	for _, c := range []struct {
		name       string
		seed       float64
		samples    []float64
		published  float64
		sized      float64
		lo, hi     float64
		strictlyIn bool
	}{
		{"seed 120, first sample 15", 120, []float64{15}, 85, 85, 60, 120, false},
		{"seed 120, first sample 460", 120, []float64{460}, (2*120 + 460) / 3.0, 460, 120, 240, false},
		{"seed 120, first sample 3600", 120, []float64{3600}, 240, 3600, 120, 240, false},
		{"no seed, first sample 15", 0, []float64{15}, (2*70 + 15) / 3.0, (2*70 + 15) / 3.0, 15, 70, true},
		{"no seed, first sample 460", 0, []float64{460}, 200, 460, 70, 460, true},
		{"no seed, first sample 463", 0, []float64{463}, 201, 463, 70, 463, true},
		{"no seed, 3 pods at 463", 0, []float64{463, 463, 463}, 463, 463, 463, 463, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := blendStart(blendAnalyzer(t, true, true), c.seed, c.samples...)
			assert.InDelta(t, c.published, f.published, 1e-9, "published")
			assert.InDelta(t, c.sized, f.sized, 1e-9, "sized")
			if c.strictlyIn {
				assert.True(t, f.published > c.lo && f.published < c.hi, "published %v strictly in (%v, %v)", f.published, c.lo, c.hi)
			} else {
				assert.True(t, f.published >= c.lo && f.published <= c.hi, "published %v in [%v, %v]", f.published, c.lo, c.hi)
			}
			assert.GreaterOrEqual(t, f.sized, f.measured, "sizing never reads a start shorter than measured")
		})
	}
}

// Off, the parent: the first sample is the estimate, published and sized,
// and the seed is no prior.
func TestStartBlend_OffTheFirstSampleIsTheEstimate(t *testing.T) {
	for _, s := range []float64{15, 460} {
		f := blendStart(blendAnalyzer(t, false, false), 120, s)
		assert.Equal(t, s, f.published, "off: published")
		assert.Equal(t, s, f.sized, "off: sized")
		assert.Equal(t, startSourceDefault, f.priorSource, "off: the seed is no prior")
	}
	// The blend alone, the seed off: no seed bounds it, the prior is 70.
	f := blendStart(blendAnalyzer(t, false, true), 120, 3600)
	assert.InDelta(t, (2*70+3600)/3.0, f.published, 1e-9, "the blend with the seed off is unbounded, toward 70")
}

// A fast variant's figure as each Pod comes: the blend from the prior to the
// measurement, and full authority from startConfirmPods Pods on. Two Pods at
// 15 s publish 33.3; the third publishes 15.
func TestStartBlend_ReachesTheMeasurementAtThreePods(t *testing.T) {
	a := blendAnalyzer(t, true, true)
	f1 := blendStart(a, 0, 15)
	assert.InDelta(t, 155.0/3, f1.published, 1e-9)
	assert.Equal(t, 1, f1.pods)
	f2 := blendStart(a, 0, 15, 15)
	assert.InDelta(t, (70+2*15)/3.0, f2.published, 1e-9)
	assert.Equal(t, 2, f2.pods)
	f3 := blendStart(a, 0, 15, 15, 15)
	assert.Equal(t, 15.0, f3.published)
	assert.Equal(t, 15.0, f3.sized)
	assert.Equal(t, startConfirmPods, f3.pods)
	a.mu.Lock()
	_, counting := a.startSamples[confirmKey]
	a.mu.Unlock()
	assert.False(t, counting, "a key at startConfirmPods keeps no count")
}

// Sizing is the largest of the blend, the mean and the measured estimate:
// 460 then 15 is a mean of 237.5 and an EWMA of 326.5. Sizing at the mean
// alone reads a shorter start than the parent sized at (326.5); the max of
// blend and mean alone kills it.
func TestStartBlend_SizingNeverReadsShorterThanTheParent(t *testing.T) {
	on := blendStart(blendAnalyzer(t, true, true), 0, 460, 15)
	off := blendStart(blendAnalyzer(t, false, false), 0, 460, 15)
	assert.InDelta(t, 0.7*460+0.3*15, off.sized, 1e-9, "the parent's EWMA")
	assert.InDelta(t, 237.5, on.mean, 1e-9)
	assert.InDelta(t, (70+2*237.5)/3, on.published, 1e-9)
	assert.InDelta(t, off.sized, on.sized, 1e-9, "on sizes at the parent's EWMA, the largest")
	assert.GreaterOrEqual(t, on.sized, off.sized)
}

// The 5b guard compares with the measured estimate, never the blend: a seed
// of 120 and a first sample of 15 s publish 85, and a second Pod at 200 s is
// over 3 x 15 = 45, so it is refused and joins 5b's run. Compared with the
// blend (3 x 85 = 255) it would be admitted.
func TestStartBlend_TheGuardReadsTheMeasurementNotTheBlend(t *testing.T) {
	a := blendAnalyzer(t, true, true)
	f := blendStart(a, 120, 15, 200)
	assert.Equal(t, 15.0, f.measured, "200 s is over 3 x 15 s: refused, the estimate stays at the measured 15")
	assert.Equal(t, 1, f.pods, "the refused Pod is not counted")
	a.mu.Lock()
	run := len(a.startOutlierPods[confirmKey])
	a.mu.Unlock()
	assert.Equal(t, 1, run, "and joins 5b's run")
}

// A seeded variant with no start measured is sized at the seed: the floor
// projects over it and share pacing reads it (A). Unseeded, it stays
// unprojected. Off, the seed is read by nothing.
func TestStartSeed_AnUnmeasuredSeededVariantIsSizedAtTheSeed(t *testing.T) {
	input := domain.AnalyzerInput{Namespace: "ns", ModelID: "m", VariantStates: []domain.VariantReplicaState{
		{VariantName: "seeded", AcceleratorName: "H200", GPUsPerReplica: 1, StartSeedSeconds: 120, StartSeedSet: true},
		{VariantName: "bare", AcceleratorName: "H200", GPUsPerReplica: 1},
	}}
	roleOf := map[string]string{"seeded": domain.RoleDecode, "bare": domain.RolePrefill}
	for _, on := range []bool{true, false} {
		a := blendAnalyzer(t, on, true)
		starts := a.startSecondsByRole(input, roleOf)
		fastest, sources := starts.sized, starts.source
		slowest, seeded := a.slowestStartSecondsByRole(input, roleOf)
		if on {
			assert.Equal(t, map[string]bool{domain.RoleDecode: true}, seeded, "pacing says the start is a seed")
		} else {
			assert.Empty(t, seeded)
		}
		assert.Empty(t, starts.parent, "5b sized no seed")
		if on {
			assert.Equal(t, 120.0, fastest[domain.RoleDecode])
			assert.Equal(t, startSourceSeed, sources[domain.RoleDecode])
			assert.Equal(t, 120.0, slowest[domain.RoleDecode])
		} else {
			assert.NotContains(t, fastest, domain.RoleDecode, "off: the seed is read by nothing")
			assert.NotContains(t, slowest, domain.RoleDecode)
		}
		assert.NotContains(t, fastest, domain.RolePrefill, "an unseeded, unmeasured role is not projected")
		assert.NotContains(t, slowest, domain.RolePrefill)
	}
}

// Beside a measured sibling, a seed never shortens the floor's projection:
// the fastest measured variant wins over a shorter seed. Share pacing takes
// the slowest, and a longer seed lengthens it.
func TestStartSeed_AMeasuredSiblingWinsTheFloorAndASlowerSeedThePacing(t *testing.T) {
	input := domain.AnalyzerInput{Namespace: "ns", ModelID: "m", VariantStates: []domain.VariantReplicaState{
		{VariantName: "measured", AcceleratorName: "H200", GPUsPerReplica: 1},
		{VariantName: "seeded", AcceleratorName: "H200", GPUsPerReplica: 8, StartSeedSeconds: 30, StartSeedSet: true},
		{VariantName: "slow-seed", AcceleratorName: "H100", GPUsPerReplica: 8, StartSeedSeconds: 900, StartSeedSet: true},
	}}
	roleOf := map[string]string{"measured": domain.RoleDecode, "seeded": domain.RoleDecode, "slow-seed": domain.RoleDecode}
	a := blendAnalyzer(t, true, true)
	key := a.itlWindowKey("ns", "m", "measured", "H200", 1)
	a.startSeconds[key] = 90
	starts := a.startSecondsByRole(input, roleOf)
	fastest, sources := starts.sized, starts.source
	assert.Equal(t, 90.0, fastest[domain.RoleDecode], "the 30 s seed does not shorten the projection")
	assert.Equal(t, 90.0, starts.parent[domain.RoleDecode])
	assert.Equal(t, startSourceMeasured, sources[domain.RoleDecode])
	slowest, seeded := a.slowestStartSecondsByRole(input, roleOf)
	assert.Equal(t, 900.0, slowest[domain.RoleDecode])
	assert.Empty(t, seeded, "a role with a measured variant is not named seeded, though a seed is its maximum")
}

// A bad seed reaches nothing: 5a hands the analyzer only a seed it parsed,
// but a seed of 0 or less, NaN included, is no prior here either.
func TestStartSeed_ANonPositiveSeedIsNoPrior(t *testing.T) {
	a := blendAnalyzer(t, true, true)
	for _, seed := range []float64{0, -5} {
		a.mu.Lock()
		f := a.startFiguresLocked(confirmKey, seed)
		a.mu.Unlock()
		assert.Equal(t, DefaultReplicaStartSeconds, f.published)
		assert.Equal(t, 0.0, f.sized, "unprojected")
		assert.Equal(t, startSourceDefault, f.source)
	}
}

// The gauge's source: default, seed or measured with the seed read; off, the
// parent's, which labels the 70 s default "seed".
func TestStartSeed_TheGaugeNamesTheSource(t *testing.T) {
	source := func(on bool, seed float64, measured bool, pods int) string {
		a := blendAnalyzer(t, on, true)
		if measured {
			a.startSeconds[confirmKey] = 90
			if pods < startConfirmPods {
				a.startSamples[confirmKey] = startSample{pods: pods, mean: 90}
			}
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.startGaugeSourceLocked(a.startFiguresLocked(confirmKey, seed))
	}
	assert.Equal(t, startSourceDefault, source(true, 0, false, 0))
	assert.Equal(t, startSourceSeed, source(true, 120, false, 0))
	assert.Equal(t, startSourceBlended, source(true, 120, true, 1), "one Pod and a seed: a blend")
	assert.Equal(t, startSourceBlended, source(true, 0, true, 2), "two Pods and the default: a blend")
	assert.Equal(t, startSourceMeasured, source(true, 120, true, startConfirmPods))
	assert.Equal(t, "seed", source(false, 0, false, 0), "off: the 70 s default is labelled seed, as before")
	assert.Equal(t, "seed", source(false, 120, false, 0))
	assert.Equal(t, startSourceMeasured, source(false, 120, true, 1), "off: the parent's, measured")
	assert.Equal(t, startSourceMeasured, source(false, 120, true, startConfirmPods))
}

// A seed its first measured start contradicts -- outside [S/2, 2S] -- is
// warned of, once per key and seed, naming both; one inside the bounds, or
// with no seed, is not. A changed seed warns again. With the 5c switches off
// nothing warns.
func TestStartSeed_AContradictedSeedIsWarnedOfOnce(t *testing.T) {
	warnings := func(on bool, seeds []float64, samples ...float64) []map[string]any {
		var out []map[string]any
		logger := funcr.NewJSON(func(obj string) {
			var m map[string]any
			if json.Unmarshal([]byte(obj), &m) == nil && m["msg"] == StartSeedContradictedMessage {
				out = append(out, m)
			}
		}, funcr.Options{Verbosity: 10})
		a := blendAnalyzer(t, on, on)
		rows := make([]domain.ReplicaMetrics, 0, len(samples))
		for i, s := range samples {
			rows = append(rows, confirmReplica(fmt.Sprintf("p%d", i), s))
			at := a.now().Add(30 * time.Second)
			a.now = func() time.Time { return at }
			a.noteReplicaStartFor(startNote{key: confirmKey, namespace: "ns", modelID: "model",
				variant: confirmVariant, role: domain.RoleDecode, seed: seeds[min(i, len(seeds)-1)]}, rows, logger)
		}
		return out
	}
	got := warnings(true, []float64{3600}, 70, 70)
	require.Len(t, got, 1, "once, though two cycles blend toward a contradicted seed")
	assert.Equal(t, float64(3600), got[0]["replicaStartSeconds"])
	assert.Equal(t, float64(70), got[0]["measuredSeconds"])
	assert.Equal(t, float64(1800), got[0]["lowSeconds"])
	assert.InDelta(t, (2*3600+70)/3.0, got[0]["sizedSeconds"], 1e-9, "sized at the blend toward the seed, the warning says so")
	assert.GreaterOrEqual(t, got[0]["sizedSeconds"], float64(1800), "and never under the seed's half")
	assert.Len(t, warnings(true, []float64{30}, 70), 1, "under the bound too: 70 > 2 x 30")
	assert.Empty(t, warnings(true, []float64{120}, 70), "inside [60, 240]: no warning")
	assert.Empty(t, warnings(true, []float64{120}, 60), "at the bound S/2: no warning")
	assert.Empty(t, warnings(true, []float64{0}, 3000), "no seed, nothing to contradict")
	assert.Len(t, warnings(true, []float64{3600, 2000}, 70, 70), 2, "a changed seed warns again")
	assert.Empty(t, warnings(false, []float64{3600}, 70), "off: the seed bounds nothing")
	assert.Len(t, warnings(true, []float64{3600}, 70, 70, 70, 70), 1, "still once, and none after three Pods give full authority")
}

// The gauge carries the published figure, not the sized one: a first start
// of 460 s with no seed publishes 200 s under source blended, though sizing
// reads 460 s.
func TestStartBlend_TheGaugePublishesTheBlend(t *testing.T) {
	registry := prometheus.NewRegistry()
	require.NoError(t, metrics.InitMetrics(registry))
	a := blendAnalyzer(t, true, true)
	a.noteReplicaStartFor(startNote{key: confirmKey, namespace: "ns", modelID: "model",
		variant: confirmVariant, role: domain.RoleDecode}, confirmRows(confirmReplica("p0", 460)), logr.Discard())
	mfs, err := registry.Gather()
	require.NoError(t, err)
	var got []float64
	for _, mf := range mfs {
		if mf.GetName() != constants.LLMSMReplicaStartSecondsEstimate {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if labels[constants.LabelVariantName] == confirmVariant {
				assert.Equal(t, startSourceBlended, labels[constants.LabelStartSource])
				got = append(got, m.GetGauge().GetValue())
			}
		}
	}
	assert.Equal(t, []float64{200}, got, "one series, at the published 200 s")
}

// Analyze hands each variant its own seed: a seeded P/D decode beside an
// unseeded prefill publishes the blend toward its seed, and the prefill
// toward the default.
func TestStartBlend_AnalyzeHandsTheSeed(t *testing.T) {
	a := blendAnalyzer(t, true, true)
	dec := makeReplicaMetrics("d0", "dec", 400_000, tracedKv, 0, 1000, 6000)
	dec.Ready, dec.StartSeconds = true, 300
	pre := makeReplicaMetrics("p0", "pre", 400_000, tracedKv, 0, 1000, 1)
	pre.Ready, pre.StartSeconds = true, 300
	in := makeAnalyzerInput([]domain.ReplicaMetrics{dec, pre}, []domain.VariantReplicaState{
		{VariantName: "dec", Role: domain.RoleDecode, AcceleratorName: "H100", GPUsPerReplica: 1, CurrentReplicas: 1,
			StartSeedSeconds: 600, StartSeedSet: true},
		{VariantName: "pre", Role: domain.RolePrefill, AcceleratorName: "H100", GPUsPerReplica: 1, CurrentReplicas: 1},
	})
	_, err := a.Analyze(t.Context(), in)
	require.NoError(t, err)
	figure := func(variant string, seed float64) startFigures {
		key := a.itlWindowKey(in.Namespace, in.ModelID, variant, "H100", 1)
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.startFiguresLocked(key, seed)
	}
	assert.InDelta(t, (2*600+300)/3.0, figure("dec", 600).published, 1e-9, "decode blends toward its 600 s seed")
	assert.InDelta(t, (2*70+300)/3.0, figure("pre", 0).published, 1e-9, "prefill toward the default")
	reported := 0
	for _, r := range a.LearnerReports()[in.Namespace+"|"+in.ModelID] {
		if r.Learner == learn.Start {
			assert.Equal(t, 1, r.Samples, "%s", r.Variant)
			reported++
		}
	}
	assert.Equal(t, 2, reported, "a start report per variant")
	// What the reports cannot show, the line does: decode's prior is its seed.
	ctx, logs := observedCtx(t)
	b := blendAnalyzer(t, true, true)
	_, err = b.Analyze(ctx, in)
	require.NoError(t, err)
	priors := map[string]any{}
	for _, e := range logs.FilterMessage("replica-start-measured").All() {
		m := e.ContextMap()
		priors[m["variant"].(string)] = m["prior"]
	}
	assert.Equal(t, map[string]any{"dec": 600.0, "pre": 70.0}, priors)
}

// replica-start-measured gains pods, prior, priorSource and sizedSeconds
// with the blend on, and is the parent's line off.
func TestStartBlend_TheMeasuredLineNamesTheBlend(t *testing.T) {
	lines := func(on bool) []map[string]any {
		var out []map[string]any
		logger := funcr.NewJSON(func(obj string) {
			var m map[string]any
			if json.Unmarshal([]byte(obj), &m) == nil && m["msg"] == "replica-start-measured" {
				out = append(out, m)
			}
		}, funcr.Options{Verbosity: 10})
		a := blendAnalyzer(t, on, on)
		a.noteReplicaStartFor(startNote{key: confirmKey, namespace: "ns", modelID: "model",
			variant: confirmVariant, role: domain.RoleDecode, seed: 120},
			confirmRows(confirmReplica("p0", 460)), logger)
		return out
	}
	on := lines(true)
	require.Len(t, on, 1)
	assert.InDelta(t, (2*120+460)/3.0, on[0]["estimateSeconds"], 1e-9)
	assert.Equal(t, float64(1), on[0]["pods"])
	assert.Equal(t, float64(120), on[0]["prior"])
	assert.Equal(t, startSourceSeed, on[0]["priorSource"])
	assert.Equal(t, float64(460), on[0]["sizedSeconds"])
	off := lines(false)
	require.Len(t, off, 1)
	assert.Equal(t, float64(460), off[0]["estimateSeconds"])
	for _, k := range []string{"pods", "prior", "priorSource", "sizedSeconds"} {
		assert.NotContains(t, off[0], k, "off: the parent's line")
	}
}

// The start learner files a report per variant and logs learner-write when
// the published estimate is first set, moves past StartWriteBand, or changes
// source; restored says so until the key's first admitted start.
func TestStartLearner_ReportsAndWrites(t *testing.T) {
	var writes []map[string]any
	logger := funcr.NewJSON(func(obj string) {
		var m map[string]any
		if json.Unmarshal([]byte(obj), &m) == nil && m["msg"] == "learner-write" {
			writes = append(writes, m)
		}
	}, funcr.Options{Verbosity: 10})
	a := blendAnalyzer(t, true, true)
	note := startNote{key: confirmKey, namespace: "ns", modelID: "model", variant: confirmVariant, role: domain.RoleDecode}
	cycle := func(rows ...domain.ReplicaMetrics) []learn.Report {
		at := a.now().Add(30 * time.Second)
		a.now = func() time.Time { return at }
		a.beginStartCycle("ns", "model")
		a.noteReplicaStartFor(note, rows, logger)
		a.closeStartCycle("ns", "model", map[string]string{confirmVariant: domain.RoleDecode})
		return a.LearnerReports()["ns|model"]
	}
	reps := cycle(confirmReplica("p0", 460))
	require.Len(t, reps, 1)
	r := reps[0]
	assert.Equal(t, learn.Start, r.Learner)
	assert.Equal(t, confirmKey, r.PricingKey)
	assert.Equal(t, learn.Source(startSourceMeasured), r.Source)
	assert.Equal(t, 1, r.Samples)
	assert.Equal(t, int64(1), r.Writes)
	assert.Equal(t, a.now(), r.LastAdmitted)
	require.Len(t, writes, 1)
	assert.Equal(t, true, writes[0]["firstWrite"])
	assert.InDelta(t, 200.0, writes[0]["value"], 1e-9)
	assert.Equal(t, false, writes[0]["restored"])

	cycle(confirmReplica("p0", 460))
	assert.Len(t, writes, 1, "nothing admitted, nothing moved: no line")
	cycle(confirmReplica("p0", 460), confirmReplica("p1", 460))
	require.Len(t, writes, 2, "the blend moved from 200 to 330")
	assert.InDelta(t, 200.0, writes[1]["previousValue"], 1e-9)

	// Restored: the learner says so until the key admits a start.
	a = blendAnalyzer(t, true, true)
	admitted := a.now().Add(-time.Hour)
	a.mu.Lock()
	a.installLocked(LearnedState{
		StartSeconds:    map[string]float64{confirmKey: 300},
		StartSamples:    map[string]StartSampleSnapshot{confirmKey: {Pods: 1, MeanSeconds: 300}},
		StartAdmittedAt: map[string]time.Time{confirmKey: admitted},
		VariantSeenAt:   map[string]time.Time{confirmKey: a.now()},
	}, "", true)
	a.mu.Unlock()
	writes = nil
	reps = cycle(confirmReplica("p0", 0))
	require.Len(t, writes, 1)
	assert.Equal(t, true, writes[0]["restored"])
	assert.InDelta(t, (2*70+300)/3.0, writes[0]["value"], 1e-9)
	require.Len(t, reps, 1)
	assert.Equal(t, 1, reps[0].Samples)
	assert.Equal(t, admitted, reps[0].LastAdmitted)
	cycle(confirmReplica("p1", 300))
	require.Len(t, writes, 2)
	assert.Equal(t, false, writes[1]["restored"], "a start admitted ends restored")
}

// Off, no learner: no report, no learner-write, no count kept.
func TestStartLearner_OffFilesNothing(t *testing.T) {
	a := blendAnalyzer(t, true, false)
	a.beginStartCycle("ns", "model")
	a.noteReplicaStartFor(startNote{key: confirmKey, namespace: "ns", modelID: "model",
		variant: confirmVariant, role: domain.RoleDecode}, confirmRows(confirmReplica("p0", 460)), logr.Discard())
	a.closeStartCycle("ns", "model", nil)
	assert.Empty(t, a.LearnerReports()["ns|model"])
	assert.Empty(t, a.startSamples)
	assert.Empty(t, a.startAdmittedAt)
	assert.Empty(t, a.exportLearnedState().StartSamples)
}

// The count and the stamp go in the snapshot, beside the estimate, and come
// back: after a restart the blend continues where it was, not at full
// authority. An estimate with no count (an older document) has full
// authority, so an upgrade moves no fleet; a count no live key could hold is
// not installed.
func TestStartBlend_TheCountSurvivesARestart(t *testing.T) {
	a := blendAnalyzer(t, true, true)
	blendStart(a, 0, 460)
	s := a.exportLearnedState()
	require.Equal(t, StartSampleSnapshot{Pods: 1, MeanSeconds: 460}, s.StartSamples[confirmKey])
	require.Equal(t, a.now(), s.StartAdmittedAt[confirmKey])
	s.VariantSeenAt = map[string]time.Time{confirmKey: a.now()}

	restored := func(st LearnedState, blends bool) startFigures {
		b := blendAnalyzer(t, true, blends)
		b.now = a.now
		b.mu.Lock()
		defer b.mu.Unlock()
		b.installLocked(st, "", true)
		return b.startFiguresLocked(confirmKey, 0)
	}
	f := restored(s, true)
	assert.InDelta(t, 200.0, f.published, 1e-9, "the blend restored")
	assert.Equal(t, 460.0, f.sized)
	assert.Equal(t, 460.0, restored(s, false).published, "off: the count is not read")

	old := s
	old.StartSamples, old.StartAdmittedAt = nil, nil
	assert.Equal(t, 460.0, restored(old, true).published, "a document without the count: full authority")
	// And it keeps it: a new Pod's start folds into the estimate by the
	// EWMA, as on the parent, and does not start a count toward the prior.
	c := blendAnalyzer(t, true, true)
	c.now = a.now
	c.mu.Lock()
	c.installLocked(old, "", true)
	c.mu.Unlock()
	f = blendStart(c, 0, 400)
	assert.InDelta(t, 0.7*460+0.3*400, f.published, 1e-9, "full authority after a new start")
	assert.Equal(t, startConfirmPods, f.pods)

	// A mean over the longest start the collector reports is clamped to it.
	big := s
	big.StartSamples = map[string]StartSampleSnapshot{confirmKey: {Pods: 1, MeanSeconds: 1e6}}
	clamped := restored(big, true)
	assert.Equal(t, maxRestoredStartSeconds, clamped.mean)
	assert.Equal(t, maxRestoredStartSeconds, clamped.sized, "sized at the clamp, not at 1e6 s")

	for _, bad := range []StartSampleSnapshot{{Pods: startConfirmPods, MeanSeconds: 460}, {Pods: 0, MeanSeconds: 460},
		{Pods: 1, MeanSeconds: 0}, {Pods: 1, MeanSeconds: math.NaN()}} {
		st := s
		st.StartSamples = map[string]StartSampleSnapshot{confirmKey: bad}
		assert.Equal(t, 460.0, restored(st, true).published, "a count %+v no live key holds is not installed", bad)
		// Nor written back: the next snapshot holds no such count.
		b := blendAnalyzer(t, true, true)
		b.now = a.now
		b.mu.Lock()
		b.installLocked(st, "", true)
		b.mu.Unlock()
		assert.Empty(t, b.exportLearnedState().StartSamples, "a count %+v is not exported again", bad)
	}

	// The pending path: exported again before the variant's first cycle,
	// the count goes out with its estimate.
	b := blendAnalyzer(t, true, true)
	b.now = a.now
	b.mu.Lock()
	b.pendingRestore["ns/"+confirmVariant] = &pendingVariant{expect: "fp", state: LearnedState{
		StartSeconds: map[string]float64{confirmKey: 460}, StartSamples: s.StartSamples,
		StartAdmittedAt: s.StartAdmittedAt, VariantSeenAt: s.VariantSeenAt}}
	b.mu.Unlock()
	out := b.exportLearnedState()
	assert.Equal(t, s.StartSamples, out.StartSamples, "a pending count is exported with its estimate")
	assert.Equal(t, s.StartAdmittedAt, out.StartAdmittedAt)
}

// A forget reaches the start learner: the estimate, its count and stamp go,
// and the key is reported forgotten by the operator on the next report. The
// learner's own forget drops them by itself, not only through the variant's
// other maps (dropVariantLocked).
func TestStartLearner_ForgetReachesIt(t *testing.T) {
	own := blendAnalyzer(t, true, true)
	blendStart(own, 0, 460)
	own.mu.Lock()
	dropped := own.forgetStartLocked("ns", "model", confirmVariant, learn.ByOperator)
	own.mu.Unlock()
	assert.Equal(t, 3, dropped, "the estimate, its count and its stamp")
	assert.Empty(t, own.startSeconds)
	assert.Empty(t, own.startSamples)
	assert.Empty(t, own.startAdmittedAt)

	a := blendAnalyzer(t, true, true)
	blendStart(a, 0, 460)
	n := a.forgetVariant("ns", "model", confirmVariant, "tok")
	assert.Positive(t, n)
	assert.Empty(t, a.startSeconds)
	assert.Empty(t, a.startSamples)
	assert.Empty(t, a.startAdmittedAt)
	// The next cycle, with nothing noted for the variant, files the forget.
	a.beginStartCycle("ns", "model")
	a.closeStartCycle("ns", "model", map[string]string{confirmVariant: domain.RoleDecode})
	var forgotten []learn.Forgotten
	for _, r := range a.LearnerReports()["ns|model"] {
		if r.Learner == learn.Start {
			forgotten = append(forgotten, r.Forgotten...)
		}
	}
	assert.Equal(t, []learn.Forgotten{{Key: confirmKey, By: learn.ByOperator}}, forgotten)
}

// floorRun is the floor rig's fleet (one full replica queued 300 deep, three
// near idle, mu about 1 each) analyzed for eight cycles at lambda with the
// switches set, the first row reporting start (0 for none) and the variant
// seeded with seed (0 for none). It returns the last throughput-demand-floor
// line and the result.
func floorRun(t *testing.T, switches map[string]bool, lambda, start, seed float64) (map[string]any, *domain.AnalyzerResult) {
	t.Helper()
	rig, err := floorRig()
	require.NoError(t, err)
	require.NoError(t, rig.a.SetSwitchesForReplay(switches))
	ctx, logs := observedCtx(t)
	rig.ctx = ctx
	rows := []domain.ReplicaMetrics{
		imbRow("r0", "v", 0.95, 300, 1000, 1000, 1.0),
		imbRow("r1", "v", 0.10, 0, 1000, 1000, 0.33),
		imbRow("r2", "v", 0.10, 0, 1000, 1000, 0.33),
		imbRow("r3", "v", 0.10, 0, 1000, 1000, 0.34),
	}
	rows[0].StartSeconds = start
	states := aggStates(len(rows))
	if seed > 0 {
		states[0].StartSeedSeconds, states[0].StartSeedSet = seed, true
	}
	var res *domain.AnalyzerResult
	for range 8 {
		res, err = rig.analyze(rows, states, lambda, propCycle)
		require.NoError(t, err)
	}
	floors := logs.FilterMessage("throughput-demand-floor").All()
	require.NotEmpty(t, floors, "premise: the floor binds")
	return floors[len(floors)-1].ContextMap(), res
}

// Through Analyze, the design's negative control: a seeded variant with no
// start measured, arrivals over what the fleet serves. The parent projects
// nothing (startSource none); with the seed read the floor projects over the
// 50 s seed and orders more, and share pacing reads it.
func TestStartSeed_ThroughAnalyzeTheFloorProjectsOverTheSeed(t *testing.T) {
	on, onRes := floorRun(t, map[string]bool{SwitchStartReadsSeed: true}, 10, 0, 50)
	off, offRes := floorRun(t, map[string]bool{SwitchStartReadsSeed: false}, 10, 0, 50)
	assert.Equal(t, 50.0, on["startEstimateSeconds"])
	assert.Equal(t, startSourceSeed, on["startSource"])
	assert.Equal(t, 50.0, onRes.RoleStartSeconds[domain.RoleBoth], "share pacing reads the seed")
	assert.Equal(t, map[string]bool{domain.RoleBoth: true}, onRes.RoleStartSeeded, "and is told it is a seed")
	assert.Nil(t, offRes.RoleStartSeeded)
	assert.Equal(t, "none", off["startSource"], "the parent: nothing projected")
	assert.NotContains(t, offRes.RoleStartSeconds, domain.RoleBoth)
	assert.Greater(t, onRes.TotalDemand, offRes.TotalDemand, "projected over the seed, the floor orders more")
}

// Through Analyze: a fast variant's first start, 15 s, with no seed, and
// arrivals over what the fleet serves. The parent projects the backlog over
// 15 s; 5c over the blend, 51.7 s, and orders more.
func TestStartBlend_ThroughAnalyzeAFastFirstStartIsProjectedOverTheBlend(t *testing.T) {
	on, onRes := floorRun(t, map[string]bool{SwitchStartBlendsFirstSamples: true}, 10, 15, 0)
	off, offRes := floorRun(t, map[string]bool{SwitchStartBlendsFirstSamples: false}, 10, 15, 0)
	assert.Equal(t, 15.0, off["startEstimateSeconds"], "the parent projects over the one sample")
	assert.InDelta(t, 155.0/3, on["startEstimateSeconds"], 1e-9, "5c projects over the blend")
	assert.Equal(t, startSourceMeasured, on["startSource"])
	assert.Greater(t, onRes.TotalDemand, offRes.TotalDemand, "a longer start orders more here")
}

// Through Analyze: where a longer start orders LESS -- the fleet drains
// faster than work arrives, so the projection credits the Ready replicas
// with more drain -- the floor keeps the parent's figure and start. 5c can
// only order more, never less. Both a seed and a blend.
func TestStartBlend_ThroughAnalyzeALongerStartNeverOrdersLess(t *testing.T) {
	for _, c := range []struct {
		name        string
		start, seed float64
		parentStart any
		parentFrom  string
	}{
		{"a fast first start, blended", 15, 0, 15.0, startSourceMeasured},
		{"a seed, nothing measured", 0, 120, 0.0, "none"},
		// The clamp holds the published blend at 2S = 40 s, far under the
		// 460 s measured; sizing still reads 460, as the parent does.
		{"a short seed clamped under a slow sample", 460, 20, 460.0, startSourceMeasured},
	} {
		t.Run(c.name, func(t *testing.T) {
			on, onRes := floorRun(t, map[string]bool{}, 2.5, c.start, c.seed)
			off, offRes := floorRun(t, map[string]bool{SwitchStartReadsSeed: false, SwitchStartBlendsFirstSamples: false},
				2.5, c.start, c.seed)
			assert.Equal(t, offRes.TotalDemand, onRes.TotalDemand, "never less than the parent")
			assert.Equal(t, off["flooredTo"], on["flooredTo"])
			assert.Equal(t, c.parentStart, on["startEstimateSeconds"], "the line names the start that priced it")
			assert.Equal(t, c.parentFrom, on["startSource"])
		})
	}
}
