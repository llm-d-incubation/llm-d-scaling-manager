package learnreplay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/slo"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/throughput"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle"
)

var genAt = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// genRow is a Ready replica at fill k.
func genRow(model, variant, pod string, k float64) domain.ReplicaMetrics {
	const kvCap = 100_000
	itl := 0.006 + 0.02*k
	return domain.ReplicaMetrics{
		PodName: pod, VariantName: variant, Namespace: "ns", ModelID: model,
		Ready: true, ReadySince: genAt.Add(-time.Hour), StartSeconds: 60, Revision: "r1",
		KvCacheUsage: k, KvCacheUsageKnown: true, KvUsageInstant: k, KvUsageInstantKnown: true,
		MaxRankKvCacheUsage: k, EngineRanks: 1, NumGpuBlocks: kvCap / 16, BlockSize: 16, TotalKvCapacityTokens: kvCap,
		TokensInUse: int64(k * kvCap), RunningRequests: int(k * 40), RunningRequestsKnown: true,
		AvgInputTokens: 900, AvgOutputTokens: 300, AvgInputTokensRecent: 900, AvgOutputTokensRecent: 300,
		AvgITL: itl, AvgITLKnown: true, GenerationTokenRate: k * 40 / itl, GenerationTokenRateKnown: true,
		RequestRate: k * 40 / itl / 300, RequestRateKnown: true, QueueLengthKnown: true,
	}
}

// genPolicy lists every analyzer, with a TPOT objective for the slo one.
func genPolicy() *config.ScalingPolicy {
	return &config.ScalingPolicy{
		KvCacheThreshold:     0.8,
		QueueLengthThreshold: 5,
		ScaleUpThreshold:     0.85,
		ScaleDownBoundary:    0.7,
		Analyzers:            []config.AnalyzerScoreConfig{{Name: "saturation"}, {Name: "slo"}, {Name: "throughput"}},
		SLO:                  &config.SLOPolicy{TPOT: &config.SLOObjective{Target: config.SLODuration{Duration: 40 * time.Millisecond}}},
	}
}

// genCapture is a capture of two models over steps: "m" with variant "a"
// active throughout and "b" parked, and "idle" whose one variant has no
// rows at all.
func genCapture(t *testing.T, steps int) []byte {
	t.Helper()
	var buf bytes.Buffer
	rec := learnrec.New(&buf, 0)
	start := fullStart()
	start.ThroughputConfig = &learnrec.ThroughputStart{SlopeHoldFor: time.Hour, OptimizationInterval: 15 * time.Second,
		SettleView: true, CapacityStore: true}
	rec.Record(learnrec.KindStart, genAt, start)
	params := capacity.EngineParams{Engine: inferenceengine.EngineVLLM, MaxNumSeqs: 64, BlockSize: 16, TensorParallelSize: 1}
	rec.Record(KindCaptureWindow, genAt, CaptureWindow{Namespace: "ns", From: genAt,
		To: genAt.Add(time.Duration(steps-1) * 15 * time.Second), Step: 15 * time.Second, Roles: "role-of",
		Variants: []CaptureVariant{
			{ModelID: "idle", Name: "i", Accelerator: "H100", GpuCount: 1, Params: params},
			{ModelID: "m", Name: "a", Role: domain.RoleBoth, Accelerator: "H100", GpuCount: 1, Params: params},
			{ModelID: "m", Name: "b", Role: domain.RoleBoth, Accelerator: "H100", GpuCount: 1, Params: params},
		}})
	for s := range steps {
		at := genAt.Add(time.Duration(s) * 15 * time.Second)
		rows := make([]domain.ReplicaMetrics, 0, 8)
		for j := range 8 {
			rows = append(rows, genRow("m", "a", fmt.Sprintf("a-%d", j), 0.3+0.05*float64(j)+0.002*float64(s%3)))
		}
		// Written out of model order: Generate plays models in key order.
		rec.Record(KindCaptureCycle, at, CaptureCycle{Input: learnrec.InputOf(domain.AnalyzerInput{
			ModelID: "m", Namespace: "ns", ReplicaMetrics: rows, Config: genPolicy(),
			VariantStates: []domain.VariantReplicaState{{VariantName: "a", CurrentReplicas: 8, DesiredReplicas: 8,
				GPUsPerReplica: 1, Role: domain.RoleBoth, AcceleratorName: "H100"}},
			RouterLatency: &domain.RouterLatency{WindowSeconds: 300, TPOTSumRate: 0.06, TPOTCountRate: 2,
				TTFTBuckets: []domain.LatencyBucket{{UpperBound: 1, Rate: 1.8}, {UpperBound: 2, Rate: 2}}},
		}), Parked: []string{"b"}})
		rec.Record(KindCaptureCycle, at, CaptureCycle{Input: learnrec.InputOf(domain.AnalyzerInput{
			ModelID: "idle", Namespace: "ns", Config: genPolicy(),
			VariantStates: []domain.VariantReplicaState{{VariantName: "i", CurrentReplicas: 1, DesiredReplicas: 1,
				GPUsPerReplica: 1, AcceleratorName: "H100"}},
		})})
	}
	require.NoError(t, rec.Err())
	return buf.Bytes()
}

// stepKinds is the kinds of a recording's events, one list per clock
// reading, the start left out.
func stepKinds(t *testing.T, recording []byte) [][]string {
	t.Helper()
	read, err := learnrec.Read(recording)
	require.NoError(t, err)
	var out [][]string
	var at time.Time
	for _, ev := range read.Events[1:] {
		if len(out) == 0 || !ev.Now.Equal(at) {
			out = append(out, nil)
			at = ev.Now
		}
		out[len(out)-1] = append(out[len(out)-1], ev.Kind)
	}
	return out
}

func TestGenerate_PlaysEachStepInTheEnginesOrder(t *testing.T) {
	rep, err := Replay(context.Background(), genCapture(t, 3), Options{Mode: Generate})
	require.NoError(t, err)
	assert.Empty(t, rep.Mismatches)
	assert.Equal(t, 3, rep.Applied)
	assert.Equal(t, map[string]int{saturation.KindAnalyze: 3, slo.KindAnalyze: 3, throughput.KindAnalyze: 3}, rep.Analyzes)

	want := []string{
		// 1. the slo cycle
		slo.KindBeginCycle,
		// 2. the settle tracker keeps the models with a variant that has
		// replicas (no event); 3. presence: the active variants model by
		// model, saturation, slo, then throughput, then the parked ones
		saturation.KindNoteVariantsPresent, slo.KindNoteVariantsPresent, throughput.KindNoteVariantsPresent, // idle: i
		saturation.KindNoteVariantsPresent, slo.KindNoteVariantsPresent, throughput.KindNoteVariantsPresent, // m: a
		saturation.KindNoteVariantsPresent, slo.KindNoteVariantsPresent, throughput.KindNoteVariantsPresent, // m: b, parked
		// 4. the sweeps
		saturation.KindEvictStaleHistory, capacity.KindEvictStale, slo.KindEvictStale,
		// 5. idle has no rows: held, and nothing more
		settle.KindHold,
		// 5-7. m: observed, its variant filed, analyzed
		settle.KindObserve,
		saturation.KindStableAcceleratorFor, capacity.KindFileScaleTarget,
		saturation.KindAnalyze, slo.KindAnalyze, throughput.KindAnalyze,
	}
	steps := stepKinds(t, rep.Recording)
	require.Len(t, steps, 3)
	for i, got := range steps {
		// The analyzers record what they check inside an Analyze (a line
		// published or borrowed); the order is of the calls.
		var calls []string
		for _, k := range got {
			if !checkKinds[k] {
				calls = append(calls, k)
			}
		}
		assert.Equal(t, want, calls, "step %d", i)
	}

	// The generated run is an ordinary recording at this build's commit: a
	// Fidelity replay reproduces it byte for byte.
	read, err := learnrec.Read(rep.Recording)
	require.NoError(t, err)
	var start learnrec.Start
	require.NoError(t, read.Events[0].DecodeData(&start))
	assert.Equal(t, learnrec.Commit(), start.Commit)
	again, err := Replay(context.Background(), rep.Recording, Options{Mode: Fidelity})
	require.NoError(t, err)
	assert.Empty(t, again.Failed(), "the generated recording does not replay: %v", again.Failed())
	assert.Equal(t, string(rep.Recording), string(again.Recording))

	// The slo analyzer was served the captured router latency.
	for _, ev := range read.Events {
		if ev.Kind != slo.KindAnalyze {
			continue
		}
		var d slo.AnalyzeEvent
		require.NoError(t, ev.DecodeData(&d))
		assert.True(t, d.RouterCalled)
		require.NotNil(t, d.RouterLatency)
		assert.Equal(t, 0.06, d.RouterLatency.TPOTSumRate)
	}
}

func TestGenerate_IsDeterministic(t *testing.T) {
	capture := genCapture(t, 4)
	a, err := Replay(context.Background(), capture, Options{Mode: Generate})
	require.NoError(t, err)
	b, err := Replay(context.Background(), capture, Options{Mode: Generate})
	require.NoError(t, err)
	assert.Equal(t, string(a.Recording), string(b.Recording))
	outA, err := Outputs(a.Recording)
	require.NoError(t, err)
	assert.NotEmpty(t, outA[OutputResults])
}

func TestGenerate_AnUnlistedAnalyzerDoesNotRun(t *testing.T) {
	var buf bytes.Buffer
	rec := learnrec.New(&buf, 0)
	rec.Record(learnrec.KindStart, genAt, fullStart())
	rec.Record(KindCaptureWindow, genAt, CaptureWindow{Namespace: "ns", Step: 15 * time.Second,
		Variants: []CaptureVariant{{ModelID: "m", Name: "a", GpuCount: 1}}})
	rec.Record(KindCaptureCycle, genAt, CaptureCycle{Input: learnrec.InputOf(domain.AnalyzerInput{
		ModelID: "m", Namespace: "ns", Config: &config.ScalingPolicy{KvCacheThreshold: 0.8},
		ReplicaMetrics: []domain.ReplicaMetrics{genRow("m", "a", "a-0", 0.4)},
		VariantStates:  []domain.VariantReplicaState{{VariantName: "a", CurrentReplicas: 1, GPUsPerReplica: 1}},
	})})
	rep, err := Replay(context.Background(), buf.Bytes(), Options{Mode: Generate})
	require.NoError(t, err)
	assert.Equal(t, map[string]int{saturation.KindAnalyze: 1}, rep.Analyzes)
}

func TestGenerate_RefusesABrokenCapture(t *testing.T) {
	good := genCapture(t, 2)
	read, err := learnrec.Read(good)
	require.NoError(t, err)
	// rebuild writes events (kind, now, raw data) again, renumbered.
	rebuild := func(edit func(evs []learnrec.RawEvent) []learnrec.RawEvent) []byte {
		evs := edit(append([]learnrec.RawEvent(nil), read.Events...))
		var buf bytes.Buffer
		for i, ev := range evs {
			line, err := learnrec.Line(uint64(i+1), ev.Now, ev.Kind, ev.Data)
			require.NoError(t, err)
			buf.Write(line)
			buf.WriteByte('\n')
		}
		return buf.Bytes()
	}
	for _, c := range []struct {
		name    string
		capture []byte
		opts    Options
		want    string
	}{
		{"no window", rebuild(func(evs []learnrec.RawEvent) []learnrec.RawEvent {
			return append(evs[:1], evs[2:]...)
		}), Options{}, "a start line and a window line"},
		{"time runs back", rebuild(func(evs []learnrec.RawEvent) []learnrec.RawEvent {
			evs[4].Now = genAt.Add(-time.Minute)
			return evs
		}), Options{}, "a cycle at"},
		{"a model twice in a step", rebuild(func(evs []learnrec.RawEvent) []learnrec.RawEvent {
			return append(evs[:3], evs[2:]...)
		}), Options{}, "twice in one step"},
		{"another kind among the cycles", rebuild(func(evs []learnrec.RawEvent) []learnrec.RawEvent {
			evs[3].Kind = saturation.KindAnalyze
			return evs
		}), Options{}, "holds only cycles"},
		{"switches", good, Options{Switches: map[string]map[string]bool{PackageSaturation: {}}}, "takes no switches"},
		{"a cut-off last line", good[:len(good)-5], Options{}, "cut off"},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.opts.Mode = Generate
			_, err := Replay(context.Background(), c.capture, c.opts)
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.want)
			var bad *CaptureError
			assert.Equal(t, c.name != "switches", errors.As(err, &bad), "the capture's fault, or the caller's")
		})
	}

	t.Run("a variant not in the window", func(t *testing.T) {
		var buf bytes.Buffer
		rec := learnrec.New(&buf, 0)
		rec.Record(learnrec.KindStart, genAt, defaultStart())
		rec.Record(KindCaptureWindow, genAt, CaptureWindow{Namespace: "ns", Step: 15 * time.Second})
		rec.Record(KindCaptureCycle, genAt, CaptureCycle{Input: learnrec.InputOf(domain.AnalyzerInput{
			ModelID: "m", Namespace: "ns", Config: &config.ScalingPolicy{},
			ReplicaMetrics: []domain.ReplicaMetrics{genRow("m", "a", "a-0", 0.4)},
			VariantStates:  []domain.VariantReplicaState{{VariantName: "a", CurrentReplicas: 1}},
		})})
		_, err := Replay(context.Background(), buf.Bytes(), Options{Mode: Generate})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not in the capture's window")
		var bad *CaptureError
		assert.True(t, errors.As(err, &bad))
	})
}

// parkCapture is a capture of two models, "n" active at every step and "m"
// whose variant "a" has replicas at the first and last steps and is parked
// whole between them; at the step allParked, when not negative, both are
// parked.
func parkCapture(t *testing.T, steps, allParked int) []byte {
	t.Helper()
	return parkCaptureFrom(t, defaultStart(), steps, allParked)
}

// parkCaptureFrom is parkCapture under start.
func parkCaptureFrom(t *testing.T, start learnrec.Start, steps, allParked int) []byte {
	t.Helper()
	var buf bytes.Buffer
	rec := learnrec.New(&buf, 0)
	rec.Record(learnrec.KindStart, genAt, start)
	params := capacity.EngineParams{Engine: inferenceengine.EngineVLLM, MaxNumSeqs: 64, BlockSize: 16, TensorParallelSize: 1}
	rec.Record(KindCaptureWindow, genAt, CaptureWindow{Namespace: "ns", From: genAt, Step: 15 * time.Second,
		Roles: "role-of", Flags: []string{"rollouts not seen: every scale target filed as not rolling out"},
		Variants: []CaptureVariant{
			{ModelID: "m", Name: "a", Accelerator: "H100", GpuCount: 1, Params: params},
			{ModelID: "n", Name: "c", Accelerator: "H100", GpuCount: 1, Params: params},
		}})
	policy := &config.ScalingPolicy{KvCacheThreshold: 0.8, QueueLengthThreshold: 5, ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.7}
	cycle := func(model, variant string, active bool) CaptureCycle {
		if !active {
			return CaptureCycle{Input: learnrec.InputOf(domain.AnalyzerInput{ModelID: model, Namespace: "ns"}),
				Parked: []string{variant}}
		}
		return CaptureCycle{Input: learnrec.InputOf(domain.AnalyzerInput{
			ModelID: model, Namespace: "ns", Config: policy,
			ReplicaMetrics: []domain.ReplicaMetrics{genRow(model, variant, variant+"-0", 0.4)},
			VariantStates: []domain.VariantReplicaState{{VariantName: variant, CurrentReplicas: 1, DesiredReplicas: 1,
				GPUsPerReplica: 1, AcceleratorName: "H100"}},
		})}
	}
	for s := range steps {
		at := genAt.Add(time.Duration(s) * 15 * time.Second)
		rec.Record(KindCaptureCycle, at, cycle("m", "a", s == 0 || s == steps-1))
		rec.Record(KindCaptureCycle, at, cycle("n", "c", s != allParked))
	}
	require.NoError(t, rec.Err())
	return buf.Bytes()
}

// A model parked whole is in no model group, so the engine's prune forgets
// its settle record (retainSettle), and Generate does the same: back after
// one parked step, its replica joins again, where a tracker that kept the
// stale sighting would carry it on as already watched.
func TestGenerate_ForgetsTheSettleRecordOfAModelParkedWhole(t *testing.T) {
	rep, err := Replay(context.Background(), parkCapture(t, 3, -1), Options{Mode: Generate})
	require.NoError(t, err)
	assert.Equal(t, int64(3), rep.Paths[settlePathPrefix+string(settle.Joined)],
		"m's replica, n's, and m's again after its parked step")
	assert.Zero(t, rep.Paths[settlePathPrefix+string(settle.Rejoined)])

	// A step at which every model is parked is the engine's cycle with no
	// active variant: the slo cycle and the forgetting, and nothing more.
	rep, err = Replay(context.Background(), parkCapture(t, 3, 1), Options{Mode: Generate})
	require.NoError(t, err)
	steps := stepKinds(t, rep.Recording)
	require.Len(t, steps, 2, "the all-parked step records nothing")
	assert.Equal(t, int64(4), rep.Paths[settlePathPrefix+string(settle.Joined)], "n is forgotten too, and joins again")
}

// The capture's flags reach the generated recording's start line, and a
// Fidelity replay of it keeps them.
func TestGenerate_CarriesTheCapturesFlags(t *testing.T) {
	rep, err := Replay(context.Background(), parkCapture(t, 2, -1), Options{Mode: Generate})
	require.NoError(t, err)
	want := []string{"rollouts not seen: every scale target filed as not rolling out"}
	startOf := func(recording []byte) learnrec.Start {
		read, err := learnrec.Read(recording)
		require.NoError(t, err)
		var s learnrec.Start
		require.NoError(t, read.Events[0].DecodeData(&s))
		return s
	}
	assert.Equal(t, want, startOf(rep.Recording).Caveats)
	again, err := Replay(context.Background(), rep.Recording, Options{Mode: Fidelity})
	require.NoError(t, err)
	assert.Empty(t, again.Failed())
	assert.Equal(t, want, startOf(again.Recording).Caveats)

	// The export keeps them: their words are the capture's own.
	exported, _, err := Export(rep.Recording, ExportOptions{})
	require.NoError(t, err)
	assert.Equal(t, want, startOf(exported).Caveats)
}
