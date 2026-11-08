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

// Issue #438: the estimate for a variant at zero replicas is one engine
// rank's figure, and a vLLM pod of several data-parallel ranks runs that many
// engines. With SwitchZeroReplicaDataParallel on it is multiplied by the
// ranks the leader pod runs (zeroReplicaRanks), as computeK2 multiplies a
// live replica's by the ranks it reports. The model runs 1000 in / 500 out
// with equal lengths under 8192 batched tokens, so one rank is S x 1250.

// storedFallback is the source the zero-replica estimate names when it has
// no workload averages and prices the stored budget.
const storedFallback = "deployment-stored-fallback"

// zeroReplicaDPOff turns SwitchZeroReplicaDataParallel off.
func zeroReplicaDPOff(a *SaturationAnalyzer) { a.zeroReplicaDP = false }

// dpParams is a vLLM engine of dp data-parallel ranks of tp GPUs each, with
// no cap in its flags, whose scale target declares the cap declared (0 for
// none).
func dpParams(dp, tp int, declared int64) *capacity.EngineParams {
	p := declaredParams(declared)
	p.DataParallelSize, p.TensorParallelSize = dp, tp
	return p
}

// dpRun is one cycle through Analyze of variant-z at zero replicas on gpus
// A100s per replica, its record derived from params.
type dpRun struct {
	params *capacity.EngineParams
	gpus   int
	// role is variant-z's; decode when empty.
	role string
	// podGPUs is each pod's GPUs in one replica (VariantReplicaState.PodGPUs).
	podGPUs []int
	// rec changes variant-z's record before it is stored; with filed the
	// record is the store's own reading of params instead (FileScaleTarget).
	rec   func(*capacity.Record)
	filed bool
	// also are other variants' records.
	also map[string]capacity.Record
	edit func(*SaturationAnalyzer)
	// cold leaves out the live variant, so the model has no workload
	// averages and the estimate falls back to the stored budget.
	cold bool
}

// estimate runs the cycle and returns variant-z's zero-replica-capacity-
// estimate line from source and the per-replica capacity Analyze published.
func (c dpRun) estimate(t *testing.T, source string) (map[string]any, float64) {
	t.Helper()
	ctx, logs := observedCtx(t)
	store := capacity.NewStore()
	if c.filed {
		store.FileScaleTarget("test-ns", "test-model", zeroVariant, "A100", c.gpus, *c.params, false, time.Now())
	} else {
		r := capacity.Record{
			AcceleratorName: "A100", GpuCount: c.gpus, EngineParams: c.params,
			EffectiveCapacity: 1, LearnedFrom: "deployment",
		}
		if c.rec != nil {
			c.rec(&r)
		}
		store.Update("test-ns", "test-model", zeroVariant, r, time.Now())
	}
	for name, other := range c.also {
		store.Update("test-ns", "test-model", name, other, time.Now())
	}
	a := NewSaturationAnalyzer(store)
	if c.edit != nil {
		c.edit(a)
	}
	role := c.role
	if role == "" {
		role = domain.RoleDecode
	}
	zero := domain.VariantReplicaState{VariantName: zeroVariant, Role: role, AcceleratorName: "A100",
		CurrentReplicas: 0, GPUsPerReplica: c.gpus, PodGPUs: c.podGPUs}
	in := makeAnalyzerInput(nil, []domain.VariantReplicaState{zero})
	if !c.cold {
		rm := makeReplicaMetrics("pod-1", "variant-a", 50_000, 1_000_000, 0, 1000, 500)
		in = makeAnalyzerInput([]domain.ReplicaMetrics{rm}, []domain.VariantReplicaState{
			{VariantName: "variant-a", Role: domain.RoleDecode, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
			zero,
		})
	}
	result, err := a.Analyze(ctx, in)
	require.NoError(t, err)
	var line map[string]any
	for _, e := range logs.FilterMessage("zero-replica-capacity-estimate").All() {
		if f := e.ContextMap(); f["variant"] == zeroVariant && f["source"] == source {
			line = f
		}
	}
	require.NotNil(t, line, "no %s estimate for variant-z", source)
	for _, vc := range result.VariantCapacities {
		if vc.VariantName == zeroVariant {
			return line, vc.PerReplicaCapacity
		}
	}
	require.FailNow(t, "Analyze published no capacity for variant-z")
	return nil, 0
}

// derived is estimate from the deployment-derived figure.
func (c dpRun) derived(t *testing.T) (map[string]any, float64) {
	t.Helper()
	return c.estimate(t, derivedEstimate)
}

// A decode pod of 8 ranks that declares 64 per rank is priced at all eight
// from zero: 8 x 64 x 1250 = 640,000, not one rank's 80,000. The cap the
// line names stays per rank.
func TestZeroReplicaDataParallel_AnEightRankDecodeIsPricedAtEveryRank(t *testing.T) {
	f, perReplica := dpRun{params: dpParams(8, 1, 64), gpus: 8}.derived(t)
	assert.EqualValues(t, 640_000, f["derivedCapacity"])
	assert.EqualValues(t, 640_000, f["perReplicaCapacity"])
	assert.EqualValues(t, 640_000, perReplica)
	assert.EqualValues(t, 64, f["batchCap"])
	assert.Equal(t, batchCapDeclared, f["batchCapSource"])
	assert.EqualValues(t, 8, f["engineRanks"])
}

// With no cap declared, the whole-node 8-GPU pod of 8 ranks is priced at the
// default per rank: 8 x 256 x 1250 = 2,560,000. Four ranks of two GPUs on the
// same node are 4 x 320,000: the ranks multiply, not the GPUs.
func TestZeroReplicaDataParallel_AWholeNodePodWithNoDeclaredCap(t *testing.T) {
	f, perReplica := dpRun{params: dpParams(8, 1, 0), gpus: 8, podGPUs: []int{8}}.derived(t)
	assert.EqualValues(t, 2_560_000, f["derivedCapacity"])
	assert.EqualValues(t, 2_560_000, perReplica)
	assert.EqualValues(t, 256, f["batchCap"])
	assert.Equal(t, batchCapDefault, f["batchCapSource"])
	assert.EqualValues(t, 8, f["engineRanks"])

	f, perReplica = dpRun{params: dpParams(4, 2, 0), gpus: 8}.derived(t)
	assert.EqualValues(t, 1_280_000, f["derivedCapacity"])
	assert.EqualValues(t, 1_280_000, perReplica)
	assert.EqualValues(t, 4, f["engineRanks"])

	// An aggregated variant scales as decode does.
	f, _ = dpRun{params: dpParams(8, 1, 64), gpus: 8, role: domain.RoleBoth}.derived(t)
	assert.EqualValues(t, 640_000, f["derivedCapacity"])
}

// A LeaderWorkerSet group of two 8-GPU pods running 16 ranks is priced at
// the ranks its leader runs, the ones its live row reports: the local size
// where stated, 8 x 320,000; with none stated, one rank, since the global 16
// counts the worker's ranks too.
func TestZeroReplicaDataParallel_AGroupIsPricedAtItsLeadersRanks(t *testing.T) {
	local := dpParams(16, 1, 0)
	local.DataParallelSizeLocal = 8
	f, perReplica := dpRun{params: local, gpus: 16, podGPUs: []int{8, 8}}.derived(t)
	assert.EqualValues(t, 2_560_000, f["derivedCapacity"])
	assert.EqualValues(t, 2_560_000, perReplica)
	assert.EqualValues(t, 8, f["engineRanks"])

	f, perReplica = dpRun{params: dpParams(16, 1, 0), gpus: 16, podGPUs: []int{8, 8}}.derived(t)
	assert.EqualValues(t, 320_000, f["derivedCapacity"])
	assert.EqualValues(t, 320_000, perReplica)
	assert.EqualValues(t, 1, f["engineRanks"])

	// One pod that states its local size runs those ranks, never more than
	// the global size.
	one := dpParams(8, 1, 0)
	one.DataParallelSizeLocal = 4
	f, _ = dpRun{params: one, gpus: 8, podGPUs: []int{8}}.derived(t)
	assert.EqualValues(t, 4, f["engineRanks"])
	one.DataParallelSizeLocal = 16
	f, _ = dpRun{params: one, gpus: 8, podGPUs: []int{8}}.derived(t)
	assert.EqualValues(t, 8, f["engineRanks"])
}

// With the pods of a replica unknown (no pod requests a GPU explicitly, so
// PodGPUs is nil) and no local size stated, the global size is read only when
// the replica's GPUs are exactly its ranks'. With no requests GPUsPerReplica
// defaults to 1, for a LeaderWorkerSet group of 16 ranks and a single pod of
// 8 alike, and both stay at one rank; so does a count that does not add up.
// A replica of 16 GPUs for 16 ranks of one GPU reads as one pod: what is left
// of the ambiguity, pinned.
func TestZeroReplicaDataParallel_UnknownPodsNeedTheGPUsToMatch(t *testing.T) {
	for _, tc := range []struct {
		dp, gpus int
		ranks    int64
	}{
		{16, 1, 1},
		{8, 1, 1},
		{16, 32, 1},
		{16, 16, 16},
	} {
		f, perReplica := dpRun{params: dpParams(tc.dp, 1, 0), gpus: tc.gpus}.derived(t)
		assert.EqualValues(t, tc.ranks, f["engineRanks"], "dp %d on %d GPUs", tc.dp, tc.gpus)
		assert.EqualValues(t, tc.ranks*320_000, perReplica, "dp %d on %d GPUs", tc.dp, tc.gpus)
	}
}

// With no workload averages yet, the estimate falls back to the stored
// budget, one rank's 8192 tokens a step; a pod of 8 ranks runs eight:
// 65,536. Off, 8192 as before.
func TestZeroReplicaDataParallel_TheColdStartBudgetIsEveryRanks(t *testing.T) {
	c := dpRun{params: dpParams(8, 1, 0), gpus: 8, filed: true, cold: true}
	f, perReplica := c.estimate(t, storedFallback)
	assert.EqualValues(t, 65_536, f["perReplicaCapacity"])
	assert.EqualValues(t, 65_536, perReplica)
	assert.EqualValues(t, 8, f["engineRanks"])

	c.edit = zeroReplicaDPOff
	f, perReplica = c.estimate(t, storedFallback)
	assert.EqualValues(t, 8192, perReplica)
	assert.EqualValues(t, 1, f["engineRanks"])
}

// Off, every rank count prices as one rank, as before #438.
func TestZeroReplicaDataParallel_TheSwitchPicksTheRanks(t *testing.T) {
	f, perReplica := dpRun{params: dpParams(8, 1, 64), gpus: 8, edit: zeroReplicaDPOff}.derived(t)
	assert.EqualValues(t, 80_000, f["derivedCapacity"])
	assert.EqualValues(t, 80_000, perReplica)
	assert.EqualValues(t, 1, f["engineRanks"])

	f, perReplica = dpRun{params: dpParams(8, 1, 0), gpus: 8, edit: zeroReplicaDPOff}.derived(t)
	assert.EqualValues(t, 320_000, f["derivedCapacity"])
	assert.EqualValues(t, 320_000, perReplica)
}

// The clamps the path applies bound the pod's figure as they bound one
// rank's: what the variant was last measured at, its own k1 (a carried KV
// total, the sum over ranks), and a compatible variant's live figure. Each
// sits between one rank's 320,000 and the pod's 2,560,000, so off it does
// not bind and on it does.
func TestZeroReplicaDataParallel_TheClampsStillBound(t *testing.T) {
	sibling := capacity.Record{
		AcceleratorName: "A100", GpuCount: 8, EngineParams: dpParams(8, 1, 0),
		EffectiveCapacity: 900_000, LearnedFrom: capacity.LearnedFromLive,
	}
	for _, tc := range []struct {
		name  string
		rec   func(*capacity.Record)
		also  map[string]capacity.Record
		bound int64
		by    string
	}{
		{"prior-live", func(r *capacity.Record) { r.PriorLiveCapacity = 500_000 }, nil, 500_000, "prior-live"},
		{"own-k1", func(r *capacity.Record) { r.TotalKvCapacityTokens = 1_000_000 }, nil, 800_000, "own-k1"},
		{"compatible", nil, map[string]capacity.Record{"variant-s": sibling}, 900_000, "compatible-variant-live"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := dpRun{params: dpParams(8, 1, 0), gpus: 8, rec: tc.rec, also: tc.also}
			f, perReplica := c.derived(t)
			assert.EqualValues(t, 2_560_000, f["derivedCapacity"])
			assert.EqualValues(t, tc.bound, perReplica)
			assert.Equal(t, tc.by, f["boundedBy"])

			c.edit = zeroReplicaDPOff
			f, perReplica = c.derived(t)
			assert.EqualValues(t, 320_000, perReplica)
			assert.Equal(t, "derived", f["boundedBy"])
		})
	}
}

// A k1 from --num-gpu-blocks-override is one engine's KV, which the store
// reads as the replica's (FileScaleTarget): 20,000 blocks of 16 at 0.8 is
// 256,000, under one rank's 320,000. It holds an 8-rank pod to about one
// rank whatever the switch says: the known under-price this change leaves
// for its own, pinned so that changing it is deliberate.
func TestZeroReplicaDataParallel_ABlocksOverrideStillHoldsItToOneRank(t *testing.T) {
	params := dpParams(8, 1, 0)
	params.NumGpuBlocksOverride = 20_000
	for _, edit := range []func(*SaturationAnalyzer){nil, zeroReplicaDPOff} {
		f, perReplica := dpRun{params: params, gpus: 8, filed: true, edit: edit}.derived(t)
		assert.EqualValues(t, 256_000, perReplica)
		assert.Equal(t, "own-k1", f["boundedBy"])
	}
}

// Controls, the same with the switch on or off: one rank (stated, or not
// stated at all) prices as before; a prefill variant keeps one rank's
// figure, as it keeps the flags' cap; and an SGLang variant keeps one rank
// whatever --dp-size says, as its live replica reports no rank count and is
// priced at one (zeroReplicaRanks).
func TestZeroReplicaDataParallel_OneRankPrefillAndSGLangAreUnchanged(t *testing.T) {
	sglang := dpParams(8, 1, 0)
	sglang.Engine = inferenceengine.EngineSGLang
	for _, edit := range []func(*SaturationAnalyzer){nil, zeroReplicaDPOff} {
		for _, dp := range []int{0, 1} {
			f, perReplica := dpRun{params: dpParams(dp, 1, 0), gpus: 1, edit: edit}.derived(t)
			assert.EqualValues(t, 320_000, f["derivedCapacity"])
			assert.EqualValues(t, 320_000, perReplica)
			assert.EqualValues(t, 1, f["engineRanks"])
		}
		f, perReplica := dpRun{params: dpParams(8, 1, 0), gpus: 8, role: domain.RolePrefill, edit: edit}.derived(t)
		assert.EqualValues(t, 320_000, f["derivedCapacity"])
		assert.EqualValues(t, 320_000, perReplica)
		assert.EqualValues(t, 1, f["engineRanks"])

		f, perReplica = dpRun{params: sglang, gpus: 8, podGPUs: []int{8}, edit: edit}.derived(t)
		assert.EqualValues(t, 320_000, f["derivedCapacity"])
		assert.EqualValues(t, 320_000, perReplica)
		assert.EqualValues(t, 1, f["engineRanks"])
	}
}
