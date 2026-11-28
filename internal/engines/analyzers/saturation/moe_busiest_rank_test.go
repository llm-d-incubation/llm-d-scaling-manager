package saturation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// The fixtures below are synthetic: no recording holds a data-parallel
// engine. Eight ranks, the busiest 1.25 times as full as their mean: the
// spread the review computed for DP8 at 32 sequences a rank and a request KV
// cv of 1 (issue llm-d-incubation/llm-d-scaling-manager#312).

// moeDP8 is an expert-parallel MoE engine over eight data-parallel ranks.
func moeDP8() *capacity.EngineParams {
	p := deploymentParams()
	p.ExpertParallel, p.DataParallelSize = true, 8
	return p
}

// The switch is off in a new analyzer, so the controller reads the mean.
func TestMoEBusiestRank_OffFromTheConstructor(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	assert.False(t, a.moeBusiestRank)
	require.Contains(t, ReplaySwitchNames(), SwitchMoEBusiestRank)
	assert.False(t, a.BusiestRankFillFor(moeDP8()), "off: no variant is read at its busiest rank")
}

// On, with no model config read, the expert-parallel flag alone gives the MoE
// class: an expert-parallel engine over more than one data-parallel rank is
// read at its busiest rank, and an engine with neither flag nor config is not
// known to be MoE and is read at its mean. TestMoEBusiestRank_KeysOnTheMoEClass
// holds the cases a config decides.
func TestMoEBusiestRank_WithoutConfigTheFlagDecides(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchMoEBusiestRank: true}))
	assert.True(t, a.BusiestRankFillFor(moeDP8()))
	noFlag := moeDP8()
	noFlag.ExpertParallel = false
	assert.False(t, a.BusiestRankFillFor(noFlag), "no flag, no config: not known to be MoE")
	one := moeDP8()
	one.DataParallelSize = 1
	assert.False(t, a.BusiestRankFillFor(one), "expert parallelism over one rank")
	assert.False(t, a.BusiestRankFillFor(nil), "no arguments recorded")
	spread := moeDP8()
	spread.DataParallelSizeLocal = 1
	assert.False(t, a.BusiestRankFillFor(spread), "one rank a pod: no ratio to read")
	sg := moeDP8()
	sg.Engine = inferenceengine.EngineSGLang
	assert.False(t, a.BusiestRankFillFor(sg), "SGLang")
}

// On, the gate is the MoE class from the model's config (issue
// llm-d-incubation/llm-d-scaling-manager#503), not the expert-parallel flag:
// vLLM steps a MoE model's data-parallel ranks in lockstep with or without
// expert parallelism, and a dense model's ranks step independently whatever
// its flags say.
func TestMoEBusiestRank_KeysOnTheMoEClass(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchMoEBusiestRank: true}))
	moeConfig := capacity.ModelConfig{Source: "config.json", NumExperts: 128, ExpertsPerToken: 8}
	denseConfig := capacity.ModelConfig{Source: "config.json"}

	tpOnly := moeDP8()
	tpOnly.ExpertParallel, tpOnly.ModelConfig = false, moeConfig
	require.Equal(t, capacity.MoETensorParallel, tpOnly.MoEClass())
	assert.True(t, a.BusiestRankFillFor(tpOnly), "a MoE model under DP 8 without expert parallelism steps in lockstep")

	ep := moeDP8()
	ep.ModelConfig = moeConfig
	require.Equal(t, capacity.MoEExpertParallel, ep.MoEClass())
	assert.True(t, a.BusiestRankFillFor(ep), "a MoE model with expert parallelism")

	dense := moeDP8()
	dense.ModelConfig = denseConfig
	require.Equal(t, capacity.MoEDense, dense.MoEClass())
	assert.False(t, a.BusiestRankFillFor(dense), "a dense config with the expert-parallel flag set")

	unknown := moeDP8()
	unknown.ExpertParallel = false
	require.Equal(t, capacity.MoEUnknown, unknown.MoEClass())
	assert.False(t, a.BusiestRankFillFor(unknown), "no config and no flag: not known to be MoE")

	tpOneRank := moeDP8()
	tpOneRank.ExpertParallel, tpOneRank.ModelConfig, tpOneRank.DataParallelSize = false, moeConfig, 1
	assert.False(t, a.BusiestRankFillFor(tpOneRank), "a MoE model over one rank")
}

// Through Analyze: the ITL line's reading of a DP8 MoE decode replica is at
// its busiest rank with the switch on, at the mean off.
func TestMoEBusiestRank_LineReadAtTheBusiestRank(t *testing.T) {
	assert.InDelta(t, 0.4, busiestRankReadingK(t, false), 1e-12)
	assert.InDelta(t, 0.5, busiestRankReadingK(t, true), 1e-12)
}

// busiestRankReadingK runs one Analyze of a DP8 MoE decode replica at a mean
// fill of 0.4, its busiest rank at 0.5, and returns the k of the reading its
// ITL window took.
func busiestRankReadingK(t *testing.T, on bool) float64 {
	t.Helper()
	ctx, _ := observedCtx(t)
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "variant-d", capacity.Record{
		AcceleratorName: "H200", GpuCount: 8, EngineParams: moeDP8(),
	}, time.Now())
	a := NewSaturationAnalyzer(store)
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchMoEBusiestRank: on}))
	rm := makeReplicaMetrics("pod-1", "variant-d", 40_000, 100_000, 0, 100, 50)
	rm.Ready, rm.EngineRanks, rm.MaxRankKvCacheUsage = true, 8, 0.5
	rm.KvUsageMean, rm.KvUsageMeanKnown, rm.AvgITL, rm.AvgITLKnown = 0.4, true, 0.02, true
	in := makeAnalyzerInput([]domain.ReplicaMetrics{rm}, []domain.VariantReplicaState{
		{VariantName: "variant-d", Role: domain.RoleDecode, AcceleratorName: "H200",
			CurrentReplicas: 1, GPUsPerReplica: 8},
	})
	_, err := a.Analyze(ctx, in)
	require.NoError(t, err)
	all := a.itlTracker.Readings(a.itlWindowKey("test-ns", "test-model", "variant-d", "H200", 8)).All
	require.Len(t, all, 1, "the replica's one reading")
	return all[0].K
}

// The rows the line reads at the busiest rank: each fill times the row's
// busiest/mean ratio, at most 1; a row with no ratio keeps its mean, and the
// caller's rows are not changed.
func TestBusiestRankRows(t *testing.T) {
	rows := []domain.ReplicaMetrics{
		{PodName: "in", EngineRanks: 8, KvCacheUsage: 0.4, MaxRankKvCacheUsage: 0.5,
			KvUsageMean: 0.4, KvUsageMeanKnown: true, KvUsageInstant: 0.42, KvUsageInstantKnown: true},
		{PodName: "full", EngineRanks: 8, KvCacheUsage: 0.9, MaxRankKvCacheUsage: 1, KvUsageMean: 0.95, KvUsageMeanKnown: true},
		{PodName: "one", EngineRanks: 1, KvCacheUsage: 0.4, MaxRankKvCacheUsage: 0.4, KvUsageMean: 0.4, KvUsageMeanKnown: true},
	}
	got := busiestRankRows(rows)
	assert.InDelta(t, 0.5, got[0].KvUsageForITL(), 1e-12)
	assert.InDelta(t, rows[0].BusiestRankKvUsageForITL(), got[0].KvUsageForITL(), 1e-12)
	assert.InDelta(t, 0.525, got[0].KvUsageInstant, 1e-12)
	assert.InDelta(t, 1, got[1].KvUsageMean, 1e-12, "at most 1")
	assert.InDelta(t, 0.4, got[2].KvUsageMean, 1e-12, "one rank: no ratio, the mean")
	assert.InDelta(t, 0.4, rows[0].KvUsageMean, 1e-12, "the caller's rows are kept")
}
