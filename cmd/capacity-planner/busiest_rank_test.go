package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// Synthetic (issue llm-d-incubation/llm-d-scaling-manager#312): no recording
// holds a data-parallel engine. A DP8 MoE decode whose busiest rank fills 1.25
// times the mean: with its line read at the busiest rank, the band's top is
// the busiest rank's, so the mean fill priced there is Hi/1.25 and the knee
// falls by 1.25. Without the switch the capacity is the variant's own.
func TestBusiestRankCapacity_PricesTheMeanAtTheTopOverTheRatio(t *testing.T) {
	vs := learnreplay.VariantState{CapacityTokens: 1_000_000, BusiestRankFill: true, BusiestRankRatio: 1.25}
	assert.InDelta(t, 800_000, busiestRankCapacity(vs), 1e-6)

	off := vs
	off.BusiestRankFill = false
	assert.InDelta(t, 1_000_000, busiestRankCapacity(off), 1e-6, "switch off: the variant's capacity")
	unread := vs
	unread.BusiestRankRatio = 0
	assert.InDelta(t, 1_000_000, busiestRankCapacity(unread), 1e-6, "no ratio read: the whole capacity")

	knee := func(c float64) float64 {
		d := whatif.Decode{Line: itl.Model{A: 0.02, B: 0.01}, Band: whatif.FillBand{Lo: 0.15, Hi: 0.8},
			CapacityTokens: c, KVPerRequest: 2000, OutputTokens: 500, ReplicaCap: 8 * 1024, Replicas: 1, Tier: whatif.TierOLS}
		a, ok := whatif.KneeRate(d).(whatif.Answer)
		require.True(t, ok, "%v", whatif.KneeRate(d))
		return a.Value
	}
	// 400 sequences at the mean's band top off; 320 on, at the same ITL.
	assert.InDelta(t, knee(1_000_000)/1.25, knee(busiestRankCapacity(vs)), 1e-9)
}

// The flag's words name the ratio, or say none was read.
func TestBusiestRankWords(t *testing.T) {
	assert.Contains(t, busiestRankWords(1.25), "fill / 1.25")
	assert.Contains(t, busiestRankWords(0), "busiest rank not read")
}

// dpRecording is w's fleet run as a data-parallel MoE engine of eight ranks
// per pod, its busiest rank ratio times as full as their mean, captured and
// generated into a recording.
func (w residentWorkload) dpRecording(t *testing.T, ratio float64) []byte {
	t.Helper()
	var buf bytes.Buffer
	rec := learnrec.New(&buf, 0)
	h := capacity.DefaultHorizons()
	rec.Record(learnrec.KindStart, residentAt, learnrec.Start{
		Saturation: learnrec.SaturationStart{EpisodeGap: h.EpisodeGap, VariantTimeout: h.VariantTimeout,
			BucketRetention: h.BucketRetention, BorrowTickGap: saturation.BorrowedK2TickGap},
		ThroughputConfig: &learnrec.ThroughputStart{SlopeHoldFor: time.Hour, OptimizationInterval: 15 * time.Second,
			SettleView: true, CapacityStore: true}})
	params := capacity.EngineParams{Engine: inferenceengine.EngineVLLM, MaxNumSeqs: 1024, BlockSize: 16, TensorParallelSize: 1,
		DataParallelSize: 8, ExpertParallel: true}
	rec.Record(learnreplay.KindCaptureWindow, residentAt, learnreplay.CaptureWindow{Namespace: "ns", From: residentAt,
		To: residentAt.Add(time.Duration(w.steps-1) * 15 * time.Second), Step: 15 * time.Second, Roles: "role-of",
		Variants: []learnreplay.CaptureVariant{
			{ModelID: "m", Name: "dec", Role: domain.RoleDecode, Accelerator: "H100", GpuCount: 8, Params: params},
		}})
	policy := &config.ScalingPolicy{KvCacheThreshold: 0.8, QueueLengthThreshold: 5, ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.7,
		Analyzers: []config.AnalyzerScoreConfig{{Name: "saturation"}}}
	for s := range w.steps {
		at := residentAt.Add(time.Duration(s) * 15 * time.Second)
		rows := make([]domain.ReplicaMetrics, 0, 6)
		for j := range 6 {
			rm := w.row(fmt.Sprintf("dec-%d", j), 0.3+0.05*float64(j)+0.002*float64(s%3))
			rm.EngineRanks, rm.MaxRankKvCacheUsage = 8, ratio*rm.KvCacheUsage
			rows = append(rows, rm)
		}
		rec.Record(learnreplay.KindCaptureCycle, at, learnreplay.CaptureCycle{Input: learnrec.InputOf(domain.AnalyzerInput{
			ModelID: "m", Namespace: "ns", ReplicaMetrics: rows, Config: policy,
			VariantStates: []domain.VariantReplicaState{{VariantName: "dec", CurrentReplicas: 6, DesiredReplicas: 6,
				GPUsPerReplica: 8, Role: domain.RoleDecode, AcceleratorName: "H100"}},
		})})
	}
	require.NoError(t, rec.Err())
	rep, err := generateRecording(context.Background(), buf.Bytes())
	require.NoError(t, err)
	return rep.Recording
}

// Synthetic, end to end (issue llm-d-incubation/llm-d-scaling-manager#312):
// a recording of a DP8 expert-parallel decode whose busiest rank fills 1.25
// times the mean, replayed with the switch on and priced as the planner
// prices it. The replay marks the variant and reads the ratio; the planner
// prices its capacity over the ratio, labels the capacity it shows, and
// lists the flag with the ratio among the TTFT answer's assumed inputs (its
// JSON). Off, none of it.
func TestBusiestRank_ReplayToPrice(t *testing.T) {
	w := residentWorkload{il: 2000, ol: 400, hit: 0, overhead: 1, capTokens: 1_000_000, steps: 40}
	rec := w.dpRecording(t, 1.25)
	priced := func(on bool) (pricedVariant, string) {
		t.Helper()
		sw := map[string]map[string]bool{learnreplay.PackageSaturation: {saturation.SwitchMoEBusiestRank: on}}
		st, err := learnreplay.StateAt(context.Background(), rec, learnreplay.Options{Switches: sw}, learnreplay.StateQuery{})
		require.NoError(t, err)
		f := answerFlags{model: "m", cycle: "last", window: captureFlags{}, set: map[string]bool{}}
		variants, err := f.decodeVariants(st)
		require.NoError(t, err)
		require.Len(t, variants, 1)
		pf := f.readFleet(st, nil, ttftQuestion{q: 0.9}, &variants[0])
		js, err := json.Marshal(pf.in)
		require.NoError(t, err)
		return variants[0], string(js)
	}
	off, offJSON := priced(false)
	on, onJSON := priced(true)
	require.Greater(t, off.state.CapacityTokens, 0.0)

	assert.False(t, off.state.BusiestRankFill)
	assert.Zero(t, off.state.BusiestRankRatio)
	assert.InDelta(t, off.state.CapacityTokens, off.decode.CapacityTokens, 1e-9, "off: the variant's capacity")
	assert.Empty(t, off.capacitySource())
	assert.NotContains(t, offJSON, string(busiestRankFill))
	assert.NotContains(t, off.flags, busiestRankFill)

	assert.True(t, on.state.BusiestRankFill, "the replay marks the variant")
	assert.InDelta(t, 1.25, on.state.BusiestRankRatio, 1e-9, "the replay reads the ratio")
	assert.InDelta(t, on.state.CapacityTokens/1.25, on.decode.CapacityTokens, 1e-6, "on: priced over the ratio")
	assert.Contains(t, on.capacitySource(), "1.25")
	assert.Contains(t, on.flags, busiestRankFill)
	assert.Contains(t, onJSON, string(busiestRankFill)+" (fill is the busiest rank's; mean fill = fill / 1.25")
}
