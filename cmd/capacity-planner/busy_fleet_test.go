package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
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
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
)

// A synthetic P/D fleet that never goes quiet: two prefill LWS groups of
// four engines with a queue on every cycle, and nine decode pods that fill
// from 0.06 to 0.38, never 0.30 above the band's 0.15 floor.

var busyAt = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

// busyFleetSteps is the capture's cycles: 80 minutes at 15 s.
const busyFleetSteps = 320

// busyPrefillRow is one cycle of prefill group pod: rr requests/s of ieff
// computed tokens, the prefill time a0 + b*ieff stretched by its per-engine
// occupancy, a queue on every sample.
func busyPrefillRow(pod string, rr, ieff float64) domain.ReplicaMetrics {
	const ranks, a0, perToken = 4, 0.25, 80e-6
	b, l := perToken*ieff, rr/ranks
	pt := (a0 + b) / (1 - b*l)
	return domain.ReplicaMetrics{
		PodName: pod, VariantName: "pre", Namespace: "ns", ModelID: "m",
		Ready: true, ReadySince: busyAt.Add(-time.Hour), Revision: "r1", EngineRanks: ranks,
		KvCacheUsage: 0.2, KvCacheUsageKnown: true, NumGpuBlocks: 100000, BlockSize: 16, TotalKvCapacityTokens: 1600000,
		AvgInputTokens: ieff * 10, AvgInputTokensRecent: ieff * 10, AvgOutputTokens: 1, AvgOutputTokensRecent: 1,
		PrefixCacheHitRate: 0.9, QueueLength: 2, QueueLengthKnown: true, QueueMin: 1, QueueMinKnown: true,
		RequestRate: rr, RequestRateKnown: true,
		PrefillComputedTokenRate: rr * ieff, PrefillComputedTokenRateKnown: true,
		AvgServiceTime: pt, AvgServiceTimeKnown: true, AvgPrefillTime: pt, AvgPrefillTimeKnown: true,
		AvgQueueTime: 0.05, AvgQueueTimeKnown: true, AvgTTFT: pt + 0.05 + 0.4, AvgTTFTKnown: true,
		RunningRequests: 4, RunningRequestsKnown: true,
	}
}

// busyFleetRecording captures the fleet and generates its recording.
func busyFleetRecording(t *testing.T) string {
	t.Helper()
	return busyFleetRecordingOf(t, 1)
}

// busyFleetRecordingOf is busyFleetRecording of a fleet whose replicas are
// groups of one engine row in groupOf: the variants state groupOf times the
// rows as replicas, and the router serves groupOf times the rows' rate, as
// on a fleet of multi-Pod groups whose rows report one rank of several
// (issue llm-d-incubation/llm-d-scaling-manager#331). 1 is the busy fleet,
// its router serving the rows' rate.
func busyFleetRecordingOf(t *testing.T, groupOf int) string {
	t.Helper()
	var buf bytes.Buffer
	rec := learnrec.New(&buf, 0)
	h := capacity.DefaultHorizons()
	rec.Record(learnrec.KindStart, busyAt, learnrec.Start{
		Saturation: learnrec.SaturationStart{EpisodeGap: h.EpisodeGap, VariantTimeout: h.VariantTimeout,
			BucketRetention: h.BucketRetention, BorrowTickGap: saturation.BorrowedK2TickGap},
		ThroughputConfig: &learnrec.ThroughputStart{SlopeHoldFor: time.Hour, OptimizationInterval: 15 * time.Second,
			SettleView: true, CapacityStore: true}})
	dec := capacity.EngineParams{Engine: inferenceengine.EngineVLLM, MaxNumSeqs: 1024, BlockSize: 16, TensorParallelSize: 1}
	pre := capacity.EngineParams{Engine: inferenceengine.EngineVLLM, MaxNumSeqs: 4, BlockSize: 16, TensorParallelSize: 1,
		MaxNumBatchedTokens: 8192, EffectiveMaxBatchedTokens: 8192}
	rec.Record(learnreplay.KindCaptureWindow, busyAt, learnreplay.CaptureWindow{Namespace: "ns", From: busyAt,
		To: busyAt.Add(time.Duration(busyFleetSteps-1) * 15 * time.Second), Step: 15 * time.Second, Roles: "role-of",
		Variants: []learnreplay.CaptureVariant{
			{ModelID: "m", Name: "dec", Role: domain.RoleDecode, Accelerator: "H100", GpuCount: 1, Params: dec},
			{ModelID: "m", Name: "pre", Role: domain.RolePrefill, Accelerator: "H100", GpuCount: 4, Params: pre},
		}})
	policy := &config.ScalingPolicy{KvCacheThreshold: 0.8, QueueLengthThreshold: 5, ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.7,
		Analyzers: []config.AnalyzerScoreConfig{{Name: "saturation"}}}
	w := residentWorkload{il: 6000, ol: 300, hit: 0.9, overhead: 1, capTokens: 2_000_000}
	rng := rand.New(rand.NewPCG(19, 23))
	for s := range busyFleetSteps {
		at := busyAt.Add(time.Duration(s) * 15 * time.Second)
		rows := make([]domain.ReplicaMetrics, 0, 11)
		for j := range 9 {
			rm := w.row(fmt.Sprintf("dec-%d", j), 0.06+0.04*float64(j)+0.002*float64(s%3))
			rows = append(rows, rm)
		}
		for j := range 2 {
			rows = append(rows, busyPrefillRow(fmt.Sprintf("pre-%d", j), 1+3*rng.Float64(), 1000+5000*rng.Float64()))
		}
		in := domain.AnalyzerInput{
			ModelID: "m", Namespace: "ns", ReplicaMetrics: rows, Config: policy,
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "dec", CurrentReplicas: 9 * groupOf, DesiredReplicas: 9 * groupOf, GPUsPerReplica: 1, Role: domain.RoleDecode, AcceleratorName: "H100"},
				{VariantName: "pre", CurrentReplicas: 2 * groupOf, DesiredReplicas: 2 * groupOf, GPUsPerReplica: 4, Role: domain.RolePrefill, AcceleratorName: "H100"},
			},
		}
		for _, rm := range rows[:9] {
			in.ArrivalRate += float64(groupOf) * rm.RequestRate
		}
		rec.Record(learnreplay.KindCaptureCycle, at, learnreplay.CaptureCycle{Input: learnrec.InputOf(in)})
	}
	require.NoError(t, rec.Err())
	rep, err := generateRecording(context.Background(), buf.Bytes())
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), fmt.Sprintf("busy-%d.jsonl", groupOf))
	require.NoError(t, os.WriteFile(path, rep.Recording, 0o600))
	return path
}

// On the busy fleet, at-replicas states both busy-fleet terms in its text
// and its JSON: a from engine timing, the adapted band floor with the
// policy's beside it, and the line that its TTFT is not validated.
func TestBusyFleet_AtReplicasFlagsBothTermsAndTheCaveat(t *testing.T) {
	path := busyFleetRecording(t)
	text, _ := runCmd(t, "at-replicas", "--recording", path, "--decode", "9", "--prefill", "2")
	assert.Contains(t, text, string(aFromEngineTiming), text)
	assert.Contains(t, text, "from engine prefill timing", text)
	assert.Contains(t, text, string(adaptedBandFloor)+" (0.06, the policy's 0.15)", text)
	assert.Contains(t, text, busyFleetTTFTNote, text)

	js, _ := runCmd(t, "at-replicas", "--recording", path, "--decode", "9", "--prefill", "2", "-o", "json")
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(js), &out), js)
	assert.Contains(t, js, `"`+string(aFromEngineTiming)+`"`)
	assert.Contains(t, js, `"`+string(adaptedBandFloor)+`"`)
	assert.Contains(t, js, busyFleetTTFTNote)
	assert.Contains(t, js, string(adaptedBandFloor)+" (0.06, the policy's 0.15)")
}
