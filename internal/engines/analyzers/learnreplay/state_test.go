package learnreplay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// olsSweepRecording is the ols-sweep golden's recording, unzipped.
func olsSweepRecording(t *testing.T) []byte {
	t.Helper()
	gz, err := os.ReadFile(filepath.Join("testdata", "goldens", "ols-sweep", RecordingName))
	if err != nil {
		t.Fatal(err)
	}
	b, err := gunzip(gz)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The what-if planner's replay: stopped at the last cycle of ols-sweep, it
// reads the line the page's worked example is priced on, its tier and
// readings, the capacity, the batch cap and the shape.
func TestStateAt_OLSSweepLastCycle(t *testing.T) {
	st, err := StateAt(context.Background(), olsSweepRecording(t), Options{}, StateQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Namespace != "ns-01" || st.ModelID != "model-02" || st.Cycle != 226 {
		t.Fatalf("stopped at %s/%s cycle %d, want ns-01/model-02 cycle 226", st.Namespace, st.ModelID, st.Cycle)
	}
	if st.Policy == nil || st.FirstAt.IsZero() || !st.At.After(st.FirstAt) {
		t.Errorf("policy %v, first %v, at %v", st.Policy, st.FirstAt, st.At)
	}
	if len(st.Variants) != 2 {
		t.Fatalf("variants %+v, want the decode and the prefill one", st.Variants)
	}
	d, p := st.Variants[0], st.Variants[1]
	if d.Variant != "variant-01" || d.Role != "decode" || p.Role != "prefill" {
		t.Fatalf("variants %s/%s and %s/%s", d.Variant, d.Role, p.Variant, p.Role)
	}
	if math.Abs(d.Line.A-0.004438) > 5e-7 || math.Abs(d.Line.B-0.001557) > 5e-7 || d.Tier != whatif.TierOLS {
		t.Errorf("line %+v tier %q, want the page's ols line A 0.004438 B 0.001557", d.Line, d.Tier)
	}
	if d.PricingKey == "" || len(d.Readings.All) == 0 || len(d.Readings.Steady) == 0 {
		t.Errorf("key %q, %d readings, %d steady; want the line's window", d.PricingKey, len(d.Readings.All), len(d.Readings.Steady))
	}
	// The tracker's choice of fit, made again on the key's windows, gives the
	// analyzer's line back to the last bits, from the steady readings.
	if !d.Refitted || len(d.LineReadings) != len(d.Readings.Steady) || d.Baseline != d.Line.B {
		t.Errorf("refitted %v from %d readings (%d steady), baseline %g; want the ols line from the steady ones",
			d.Refitted, len(d.LineReadings), len(d.Readings.Steady), d.Baseline)
	}
	if d.CapacityTokens != 131072 || d.BatchCap != 64 || d.BatchCapSource != "flags" || d.Ranks != 1 || d.Replicas != 1 {
		t.Errorf("C %g, cap %d (%s), ranks %d, replicas %d; want 131072, 64 from the flags, 1, 1",
			d.CapacityTokens, d.BatchCap, d.BatchCapSource, d.Ranks, d.Replicas)
	}
	// The recording ends idle (cycles 224-226 serve nothing): the shape is the
	// last 20 cycles that had traffic, weighted by request rate.
	sh := d.Shape
	if sh.ToCycle != 223 || sh.FromCycle != 204 || sh.Cycles != 20 || sh.PrefixHitRate != 0 ||
		!(sh.InputTokens > 900 && sh.InputTokens < 6100) || !(sh.OutputTokens > 400 && sh.OutputTokens < 4100) {
		t.Errorf("shape %+v, want cycles 204-223 of traffic, lengths inside what they served", sh)
	}
	t.Logf("measured shape %+v", sh)
	if !p.Line.IsZero() || p.Tier != "" {
		t.Errorf("the prefill variant priced on line %+v tier %q", p.Line, p.Tier)
	}
	if len(st.TPOTWindows) != 0 {
		t.Errorf("%d TPOT windows from a recording made before the recorder carried them", len(st.TPOTWindows))
	}
}

// A cycle number stops the replay there: cycle 1 has no line yet, and the
// state of cycle 100 is not the last one's.
func TestStateAt_StopsAtTheCycle(t *testing.T) {
	rec := olsSweepRecording(t)
	first, err := StateAt(context.Background(), rec, Options{}, StateQuery{Namespace: "ns-01", ModelID: "model-02", Cycle: 1})
	if err != nil {
		t.Fatal(err)
	}
	if first.Cycle != 1 || !first.Variants[0].Line.IsZero() {
		t.Errorf("cycle %d line %+v, want cycle 1 with no line", first.Cycle, first.Variants[0].Line)
	}
	mid, err := StateAt(context.Background(), rec, Options{}, StateQuery{ModelID: "model-02", Cycle: 100})
	if err != nil {
		t.Fatal(err)
	}
	last, err := StateAt(context.Background(), rec, Options{}, StateQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if mid.Cycle != 100 || mid.Variants[0].Line.IsZero() || !mid.At.After(first.At) || !mid.At.Before(last.At) {
		t.Fatalf("cycle %d at %v, line %+v", mid.Cycle, mid.At, mid.Variants[0].Line)
	}
	if mid.Variants[0].Line == last.Variants[0].Line {
		t.Errorf("cycle 100 reads the last cycle's line %+v", mid.Variants[0].Line)
	}
}

func TestStateAt_Refuses(t *testing.T) {
	rec := olsSweepRecording(t)
	for name, c := range map[string]struct {
		q    StateQuery
		want string
	}{
		"a cycle past the last":   {StateQuery{Cycle: 227}, "holds 226"},
		"a model it lacks":        {StateQuery{ModelID: "model-99"}, "no cycle of model"},
		"a namespace it lacks":    {StateQuery{Namespace: "ns-99"}, "no saturation cycle"},
		"a negative cycle number": {StateQuery{Cycle: -1}, "want 1 or more"},
	} {
		_, err := StateAt(context.Background(), rec, Options{}, c.q)
		var q *QueryError
		if !errors.As(err, &q) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want a QueryError saying %q", name, err, c.want)
		}
	}
}

// A recording that does not replay is not the caller's question: StateAt's
// error for it is no QueryError, and a cancelled replay is one too.
func TestStateAt_ReplayFailureIsNoQueryError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := StateAt(ctx, olsSweepRecording(t), Options{}, StateQuery{})
	var q *QueryError
	if err == nil || errors.As(err, &q) {
		t.Errorf("cancelled: %v, want an error that is no QueryError", err)
	}
}

// A line the band does not give back, and the above-band fallback does, is
// an above-band line, read on the above-band readings: its observed fills
// are those, at or above itl.DefaultKSat.
func TestRefitFindsAnAboveBandLine(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	above := make([]itl.Observation, 0, 12)
	for i := range 12 {
		k := 0.86 + 0.01*float64(i%8)
		above = append(above, itl.Observation{K: k, ITLSec: 0.01 + 0.05*k + 0.0005*float64(i%3), Timestamp: t0.Add(time.Duration(i) * 15 * time.Second)})
	}
	line := itl.RefitAboveBand(above, nil, 0)
	if line.Tier == "" {
		t.Fatal("the fallback fits nothing; the case tests nothing")
	}
	vs := VariantState{Tier: whatif.TierPinnedB, Line: line.Model, Readings: itl.KeyReadings{Above: above}}
	vs.refit()
	if !vs.Refitted || vs.Tier != whatif.TierAboveBand || len(vs.LineReadings) != len(above) {
		t.Fatalf("%+v, want the above-band tier on the %d above-band readings", vs, len(above))
	}
	for _, o := range vs.LineReadings {
		if o.K < itl.DefaultKSat {
			t.Errorf("a reading at %g, under the fallback's floor", o.K)
		}
	}
	// A line neither fit gives back is left as it was, unbanded.
	other := VariantState{Tier: whatif.TierPinnedB, Line: itl.Model{A: 1, B: 0.002}, Readings: itl.KeyReadings{Above: above}}
	other.refit()
	if other.Refitted || other.Tier != whatif.TierPinnedB || other.LineReadings != nil {
		t.Errorf("%+v, want it as it was, with no readings", other)
	}
}

// A pinned line the analyzer priced at the prior on B (issue #467; the
// recorded PinnedAtBPrior) is given back by the refit through the default B
// with the analyzer's one-sided admission, which the state carries for the
// bands (OneSided). The gate is the recorded flag: a line recorded as not
// raised gets no admission.
func TestRefitFindsAOneSidedLine(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	all := make([]itl.Observation, 0, 36)
	for c := range 12 {
		for _, k := range []float64{0.96, 0.97, 0.98} {
			all = append(all, itl.Observation{K: k, ITLSec: 0.020 + 0.01*k, Timestamp: t0.Add(time.Duration(c) * 15 * time.Second)})
		}
	}
	adm := itl.Admission{OneSidedAboveKSat: true, OneSidedAt: 0.68, BPrior: itl.DefaultBPrior}
	line := itl.Refit(all, nil, itl.DefaultBaselineSec, 0, adm)
	if !line.PinnedAtBPrior {
		t.Fatal("the one-sided refit priced nothing one-sided; the case tests nothing")
	}
	// Recorded as raised: refit with the analyzer's admission, kept for the
	// bands.
	vs := VariantState{Tier: whatif.TierPinnedB, Line: line.Model, Readings: itl.KeyReadings{All: all},
		analyzerOneSided: adm, analyzerRaised: true}
	vs.refit()
	if !vs.Refitted || vs.Baseline != itl.DefaultBaselineSec || vs.OneSided != adm || len(vs.LineReadings) != len(all) {
		t.Fatalf("%+v, want it refitted one-sided through the default B on all %d readings", vs, len(all))
	}
	// Recorded as not raised: the gate is the flag, not the B, so the same
	// line is not refitted one-sided (and is not given back by the plain fit).
	off := VariantState{Tier: whatif.TierPinnedB, Line: line.Model, Readings: itl.KeyReadings{All: all},
		analyzerOneSided: adm}
	off.refit()
	if off.Refitted || off.OneSided.OneSidedAboveKSat {
		t.Errorf("negative control: recorded as not raised, %+v was refitted one-sided", off)
	}
	// A line through the default B the analyzer did not raise carries no
	// admission, whatever the admission it was priced under.
	inBand := make([]itl.Observation, 0, 12)
	for c := range 12 {
		k := 0.30 + 0.01*float64(c%3)
		inBand = append(inBand, itl.Observation{K: k, ITLSec: 0.006 + 0.05*k, Timestamp: t0.Add(time.Duration(c) * 15 * time.Second)})
	}
	plain := itl.Refit(inBand, nil, itl.DefaultBaselineSec, 0, itl.Admission{})
	un := VariantState{Tier: whatif.TierPinnedB, Line: plain.Model, Readings: itl.KeyReadings{All: inBand},
		analyzerOneSided: adm}
	un.refit()
	if !un.Refitted || un.OneSided.OneSidedAboveKSat {
		t.Errorf("%+v, want it refitted with no one-sided admission", un)
	}
}

// The measured shape weights each cycle with traffic by its request rate,
// and every cycle alike when none read a rate.
func TestShapeOfWeightsByRequestRate(t *testing.T) {
	samples := []shapeSample{
		{cycle: 7, il: 1000, ol: 100, hit: 0.2, req: 1},
		{cycle: 9, il: 2000, ol: 500, hit: 0.6, req: 3},
	}
	got := shapeOf(samples)
	if got.InputTokens != 1750 || got.OutputTokens != 400 || math.Abs(got.PrefixHitRate-0.5) > 1e-12 ||
		got.FromCycle != 7 || got.ToCycle != 9 || got.Cycles != 2 {
		t.Errorf("%+v, want IL 1750, OL 400, hit 0.5 over cycles 7-9", got)
	}
	samples[0].req, samples[1].req = 0, 0
	if got := shapeOf(samples); got.InputTokens != 1500 || got.OutputTokens != 300 {
		t.Errorf("no rate read: %+v, want the plain mean IL 1500, OL 300", got)
	}
	if got := shapeOf(nil); got != (MeasuredShape{}) {
		t.Errorf("no traffic: %+v, want zero", got)
	}
}

// The spread is read over the cycles that read it, with their own weights and
// mean: a window with one unread cycle keeps its reading, and a window with none
// has no reading, which is not D = 1.
func TestShapeOfReadsTheOutputSpread(t *testing.T) {
	// 1 request a second of 100 tokens beside 3 of 500: E[O] 400, E[O^2]
	// (1*1e4 + 3*25e4)/4 = 190000, D 1.1875, and a third cycle that read
	// none, at OL 9000, has no say in it.
	samples := []shapeSample{
		{cycle: 7, il: 1000, ol: 100, squared: 1e4, req: 1},
		{cycle: 8, il: 1000, ol: 9000, req: 2},
		{cycle: 9, il: 2000, ol: 500, squared: 25e4, req: 3},
	}
	if got := shapeOf(samples); math.Abs(got.OutputDispersion-1.1875) > 1e-12 {
		t.Errorf("D %g, want 1.1875 over the two cycles that read one", got.OutputDispersion)
	}
	samples[0].squared, samples[2].squared = 0, 0
	if got := shapeOf(samples); got.OutputDispersion != 0 {
		t.Errorf("D %g, want 0: no reading", got.OutputDispersion)
	}
}

// Rows are weighted by their request rate, as the analyzers weight them: 9
// requests a second of 100 tokens beside 1 of 1000 serve OL 190 and D 3.02, not
// a plain mean of 550. With no rate read, rows count alike, and a row with no
// rate beside rows with one has no say.
func TestNoteShapesWeightsRowsByRequestRate(t *testing.T) {
	row := func(pod string, rate, ol, in, hit float64, known bool) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{PodName: pod, VariantName: "d", RequestRate: rate, RequestRateKnown: known,
			AvgInputTokens: in, PrefixCacheHitRate: hit, AvgOutputTokens: ol,
			AvgOutputTokensSquared: ol * ol, AvgOutputTokensSquaredKnown: true, RunningRequests: 1}
	}
	near := func(what string, got, want float64) {
		t.Helper()
		if math.Abs(got-want) > 1e-9*max(1, math.Abs(want)) {
			t.Errorf("%s %g, want %g", what, got, want)
		}
	}
	shapes := map[string][]shapeSample{}
	noteShapes(shapes, []domain.ReplicaMetrics{row("a", 9, 100, 1000, 0.5, true), row("b", 1, 1000, 2000, 0, true)}, 1)
	got := shapeOf(shapes["d"])
	near("OL", got.OutputTokens, 190)
	near("IL", got.InputTokens, 1100)
	near("hit", got.PrefixHitRate, 0.45)
	near("D", got.OutputDispersion, 109000.0/(190*190))

	none := map[string][]shapeSample{}
	noteShapes(none, []domain.ReplicaMetrics{row("a", 0, 100, 1000, 0, false), row("b", 0, 1000, 1000, 0, false)}, 1)
	near("OL with no rate", shapeOf(none["d"]).OutputTokens, 550)

	mixed := map[string][]shapeSample{}
	noteShapes(mixed, []domain.ReplicaMetrics{row("a", 2, 100, 1000, 0, true), row("b", 0, 1000, 1000, 0, false)}, 1)
	near("OL beside a row with no rate", shapeOf(mixed["d"]).OutputTokens, 100)

	// Nor does such a row cost the cycle its reading.
	spread := map[string][]shapeSample{}
	idle := row("b", 0, 1000, 1000, 0, false)
	idle.AvgOutputTokensSquaredKnown = false
	noteShapes(spread, []domain.ReplicaMetrics{row("a", 2, 100, 1000, 0, true), idle}, 1)
	near("D beside an unrated, unread row", shapeOf(spread["d"]).OutputDispersion, 1)
}

// A cycle's bound is its rows' only when every row read one, so that the mean
// it is set against is the same rows'. A row with no output length is no row
// of the shape, reading or not.
func TestNoteShapesReadsTheBoundOfEveryRow(t *testing.T) {
	row := func(pod string, read bool) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{PodName: pod, VariantName: "d", RequestRate: 1, RequestRateKnown: true,
			AvgOutputTokens: 100, AvgOutputTokensSquared: 2e4, AvgOutputTokensSquaredKnown: read}
	}
	idle := domain.ReplicaMetrics{PodName: "idle", VariantName: "d"}
	shapes := map[string][]shapeSample{}
	noteShapes(shapes, []domain.ReplicaMetrics{row("a", true), row("b", true), idle}, 1)
	if got := shapes["d"][0].squared; got != 2e4 {
		t.Errorf("every row read: %g, want 20000", got)
	}
	noteShapes(shapes, []domain.ReplicaMetrics{row("a", true), row("b", false)}, 2)
	if got := shapes["d"][1].squared; got != 0 {
		t.Errorf("one row unread: %g, want no reading", got)
	}
}

// The last cycle is a cycle number like any other: asking for it by number
// gives the same state, and the readings' cycles fall inside the replay.
func TestStateAt_LastIsTheLastCycle(t *testing.T) {
	rec := olsSweepRecording(t)
	last, err := StateAt(context.Background(), rec, Options{}, StateQuery{})
	if err != nil {
		t.Fatal(err)
	}
	byNumber, err := StateAt(context.Background(), rec, Options{}, StateQuery{Namespace: "ns-01", ModelID: "model-02", Cycle: 226})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(last, byNumber) {
		t.Errorf("the last cycle and cycle 226 differ:\n%+v\n%+v", last, byNumber)
	}
	d := last.Variants[0]
	if last.Cycles != 226 || d.ReadingsFrom < 1 || d.ReadingsTo > 226 || d.ReadingsFrom > d.ReadingsTo {
		t.Errorf("%d cycles, readings over cycles %d-%d", last.Cycles, d.ReadingsFrom, d.ReadingsTo)
	}
	t.Logf("line readings %d over cycles %d-%d", len(d.LineReadings), d.ReadingsFrom, d.ReadingsTo)
}

// scopedPriorRecording is a recording of the saturation analyzer on two
// decode variants of one model, on one engine: sib, whose replicas step
// through the band and learn B = 45 ms by a full fit, and v, whose three
// replicas sit at their declared cap of 64, so the analyzer prices v
// one-sided at a prior raised to sib's 45 ms.
func scopedPriorRecording(t *testing.T) []byte {
	t.Helper()
	f := newFixture(replayAt)
	f.buf.Reset()
	rec := learnrec.New(f.buf, 0)
	f.attach(rec)
	rec.Record(learnrec.KindStart, replayAt, defaultStart())
	params := capacity.EngineParams{Engine: inferenceengine.EngineVLLM, MaxNumSeqs: 256, DeclaredBatchCap: 64, Image: "img:1"}
	for _, v := range []string{"v", "sib"} {
		f.store.FileScaleTarget("ns", "m", v, "H200", 1, params, false, replayAt)
	}
	const a, b = 0.05, 0.045
	row := func(variant, pod string, k float64, capped bool) domain.ReplicaMetrics {
		rm := domain.ReplicaMetrics{PodName: pod, VariantName: variant, Ready: true, ReadySince: replayAt.Add(-time.Hour),
			KvCacheUsage: k, KvCacheUsageKnown: true, KvUsageInstant: k, KvUsageInstantKnown: true,
			AvgITL: a*k + b, AvgITLKnown: true, TotalKvCapacityTokens: 100000, TokensInUse: int64(k * 100000),
			AvgInputTokens: 1000, AvgOutputTokens: 500, GenerationTokenRate: 1000, GenerationTokenRateKnown: true,
			RequestRate: 2, RequestRateKnown: true}
		if capped {
			rm.RunningRequests, rm.QueueLength, rm.PreemptionRateKnown = 64, 5, true
		}
		return rm
	}
	states := []domain.VariantReplicaState{
		{VariantName: "v", Role: domain.RoleDecode, AcceleratorName: "H200", GPUsPerReplica: 1, CurrentReplicas: 3},
		{VariantName: "sib", Role: domain.RoleDecode, AcceleratorName: "H200", GPUsPerReplica: 1, CurrentReplicas: 2},
	}
	now := replayAt
	for c := range 80 {
		f.clk.SetTime(now)
		k := 0.25
		if (c/16)%2 == 1 {
			k = 0.65
		}
		rows := make([]domain.ReplicaMetrics, 0, 5)
		rows = append(rows, row("sib", "sib-0", k, false), row("sib", "sib-1", 0.45, false))
		for i, kc := range []float64{0.96, 0.97, 0.98} {
			rows = append(rows, row("v", fmt.Sprintf("v-%d", i), kc, true))
		}
		_, err := f.sat.Analyze(context.Background(), domain.AnalyzerInput{ModelID: "m", Namespace: "ns",
			Config:        &config.ScalingPolicy{KvCacheThreshold: 0.8, ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.7},
			VariantStates: states, ReplicaMetrics: rows})
		require.NoError(t, err)
		now = now.Add(15 * time.Second)
	}
	return bytes.Clone(f.buf.Bytes())
}

// On a recording where v is priced at a prior raised by a scoped sibling
// (scopedPriorRecording), StateAt reads the admission the analyzer priced v
// with (BPrior 45 ms, at the pricing k), refits the line with it, and the
// band of the line's ITL at the pricing k brackets the controller's line.
// Refitted without the admission (the plain fit), the band lies under it.
func TestStateAt_TheBandBracketsTheControllersLineOnAScopedPrior(t *testing.T) {
	rec := scopedPriorRecording(t)
	st, err := StateAt(context.Background(), rec, Options{}, StateQuery{Namespace: "ns", ModelID: "m"})
	require.NoError(t, err)
	var v *VariantState
	for i := range st.Variants {
		if st.Variants[i].Variant == "v" {
			v = &st.Variants[i]
		}
	}
	require.NotNil(t, v, "no state for v")
	require.True(t, v.Refitted, "v's line was not refitted: %+v", v.Line)
	require.True(t, v.OneSided.OneSidedAboveKSat, "v's refit carries no one-sided admission")
	require.InDelta(t, 0.045, v.OneSided.BPrior, 1e-12, "the prior is not the scoped sibling's B")
	k := v.OneSided.OneSidedAt
	require.Positive(t, k, "no pricing k recorded")
	val := func(m itl.Model) (float64, bool) { return m.ITLAt(k), true }
	ctrl := v.Line.ITLAt(k)
	on := whatif.BootstrapLines(v.Readings, whatif.Refit{Tier: v.Tier, Line: v.Line, Baseline: v.Baseline,
		MinSettled: v.MinSettled, OneSided: v.OneSided}, whatif.DefaultResampling())
	off := whatif.BootstrapLines(v.Readings, whatif.Refit{Tier: v.Tier, Line: v.Line, Baseline: v.Baseline,
		MinSettled: v.MinSettled}, whatif.DefaultResampling())
	band := whatif.BandAnswer(whatif.Answer{Value: ctrl}, on, val)
	plain := whatif.BandAnswer(whatif.Answer{Value: ctrl}, off, val)
	require.NotNil(t, band.Band)
	require.NotNil(t, plain.Band)
	t.Logf("controller %.5f s at k %.2f; band with the admission [%.5f, %.5f], without [%.5f, %.5f]",
		ctrl, k, band.Band.Lo, band.Band.Hi, plain.Band.Lo, plain.Band.Hi)
	require.True(t, band.Band.Lo <= ctrl+1e-12 && ctrl <= band.Band.Hi+1e-12, "the band does not bracket the controller's line")
	require.Less(t, plain.Band.Hi, ctrl, "negative control: without the admission the band reached the controller's line")
}

// A full fit kept under the set values by the tier latch (issue #495) is
// refitted by the clear values: the same line, not the pinned fit.
func TestVariantState_ALatchedFullFitIsRefitted(t *testing.T) {
	obs := make([]itl.Observation, 0, 19)
	for i := range 19 {
		k := 0.46 + 0.015*float64(i)
		obs = append(obs, itl.Observation{K: k, ITLSec: 0.03*k + 0.0025})
	}
	line, ok := itl.Fit(obs)
	require.True(t, ok)
	vs := VariantState{Tier: whatif.TierOLS, Line: line, Readings: itl.KeyReadings{All: obs, Steady: obs, OLSLatched: true}}
	vs.refit()
	require.True(t, vs.Refitted, "a latched full fit was not refitted")
	require.Equal(t, whatif.TierOLS, vs.Tier)

	off := VariantState{Tier: whatif.TierOLS, Line: line, Readings: itl.KeyReadings{All: obs, Steady: obs}}
	off.refit()
	require.Equal(t, whatif.TierPinnedB, off.Tier, "negative control: unlatched, the spread is under the set value")
}
