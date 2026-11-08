package saturation

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// Issue #420: the batch cap's readers besides P3 read the caps #399 added
// where the engine's flags state none. Every reader reads the
// llm-d.ai/max-num-seqs annotation; the learned cap reaches the derived mu
// only, never a reader that asks whether a replica is AT its cap. Each
// reader is driven through Analyze, with the flags unread (MaxNumSeqs the
// parser's default of 256, not stated).

// declaredParams is an engine whose flags state no cap, and whose scale
// target declares perRank.
func declaredParams(perRank int64) *capacity.EngineParams {
	p := deploymentParams()
	p.DeclaredBatchCap = perRank
	return p
}

// cappedITLWindow runs one cycle of a decode replica holding running
// sequences, 768 waiting, at KV 0.87 -- above the ITL band -- under params,
// with edit applied to the analyzer and the policy learning set, and returns
// the itl-window line.
func cappedITLWindow(t *testing.T, params *capacity.EngineParams, running int, learning bool,
	edit func(*SaturationAnalyzer)) map[string]any {
	t.Helper()
	ctx, logs := observedCtx(t)
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "variant-d", capacity.Record{
		GpuCount: 1, EngineParams: params, LearnedFrom: "deployment",
	}, time.Now())
	a := NewSaturationAnalyzer(store)
	if edit != nil {
		edit(a)
	}
	capped := makeReplicaMetrics("pod-1", "variant-d", 14000, 16000, 768, 1000, 500)
	capped.Ready = true
	capped.KvUsageInstant, capped.AvgITL, capped.AvgITLKnown = 0.87, 0.079, true
	capped.RunningRequests, capped.PreemptionRateKnown = running, true
	in := makeAnalyzerInput([]domain.ReplicaMetrics{capped}, []domain.VariantReplicaState{
		{VariantName: "variant-d", Role: domain.RoleDecode, AcceleratorName: "H100",
			CurrentReplicas: 1, GPUsPerReplica: 1},
	})
	in.Config.(*config.ScalingPolicy).EnableBatchCapLearning = learning
	_, err := a.Analyze(ctx, in)
	require.NoError(t, err)
	return requireLogged(t, logs, "itl-window")
}

// A replica at the cap its scale target declares teaches the line from
// above the band, as one at the cap its flags state does (issue #287).
// Before #420 only the flags' cap reached the window: the declared cap was
// read by P3 alone, and the reading counted as above the band.
func TestBatchCapReaders_TheITLWindowTakesAReplicaAtItsDeclaredCap(t *testing.T) {
	fields := cappedITLWindow(t, declaredParams(128), 128, false, nil)
	assert.EqualValues(t, 128, fields["batchCap"])
	assert.Equal(t, batchCapDeclared, fields["batchCapSource"])
	assert.EqualValues(t, 1, fields["capped"])
	assert.EqualValues(t, 0, fields["aboveBand"])

	// Below the declared cap the batch is still growing.
	fields = cappedITLWindow(t, declaredParams(128), 127, false, nil)
	assert.EqualValues(t, 0, fields["capped"])
	assert.EqualValues(t, 1, fields["aboveBand"])
}

// learnedCapOf is the analyzer edit that files a learned cap of perRank for
// variant under params, last confirmed at the time given and never used.
func learnedCapOf(variant string, perRank int64, params *capacity.EngineParams, at time.Time) func(*SaturationAnalyzer) {
	return func(a *SaturationAnalyzer) {
		a.batchCaps[batchCapKey("test-ns", "test-model", variant)] = &learnedBatchCap{
			perRank: perRank, args: windowArgs(params), at: at}
	}
}

// learnedCapEntry is variant's learned cap in a.
func learnedCapEntry(t *testing.T, a *SaturationAnalyzer, variant string) learnedBatchCap {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	lc := a.batchCaps[batchCapKey("test-ns", "test-model", variant)]
	require.NotNil(t, lc, "the learned cap is still filed")
	return *lc
}

// A learned cap is not a cap the ITL fit reads a replica as AT. It is the
// highest plateau seen, so it may sit below the true cap, and a replica
// held there by memory would be admitted above the band as capped. Nor is
// it stamped as used: an at-cap test does not price a replica.
//
// A guard, not switch-off evidence: it passes with SwitchBatchCapITLAdmission
// either way, since neither side reads the learned cap.
func TestBatchCapReaders_ALearnedCapIsNotAnITLCap(t *testing.T) {
	params := deploymentParams()
	var a *SaturationAnalyzer
	keep := func(an *SaturationAnalyzer) { a = an }
	confirmed := time.Date(2026, 10, 24, 11, 0, 0, 0, time.UTC)
	learned := learnedCapOf("variant-d", 128, params, confirmed)
	fields := cappedITLWindow(t, params, 128, true, func(an *SaturationAnalyzer) { learned(an); keep(an) })
	assert.EqualValues(t, 0, fields["batchCap"])
	assert.Equal(t, batchCapNone, fields["batchCapSource"])
	assert.EqualValues(t, 0, fields["capped"])
	assert.EqualValues(t, 1, fields["aboveBand"])
	lc := learnedCapEntry(t, a, "variant-d")
	assert.Equal(t, confirmed, lc.at, "the reading confirms nothing")
}

// The switch off reads the flags' cap alone, as before #420.
func TestBatchCapReaders_TheITLAdmissionSwitchPicksTheSource(t *testing.T) {
	off := func(a *SaturationAnalyzer) { a.batchCapITLAdmission = false }
	fields := cappedITLWindow(t, declaredParams(128), 128, false, off)
	assert.EqualValues(t, 0, fields["batchCap"])
	assert.EqualValues(t, 0, fields["capped"])
	assert.EqualValues(t, 1, fields["aboveBand"])

	stated := declaredParams(128)
	stated.MaxNumSeqs, stated.MaxNumSeqsSet = 128, true
	fields = cappedITLWindow(t, stated, 128, false, off)
	assert.EqualValues(t, 1, fields["capped"], "the flags' cap is read either way")
}

// pdDecodeAtCap runs four cycles of a P/D decode replica holding running
// sequences with every waiting request deferred (#287's shape), under
// params, with edit applied to the analyzer.
func pdDecodeAtCap(t *testing.T, params *capacity.EngineParams, running int,
	edit func(*SaturationAnalyzer)) (*SaturationAnalyzer, *domain.AnalyzerResult) {
	t.Helper()
	return pdDecodeAtCapLearning(t, params, running, false, edit)
}

// pdDecodeAtCapLearning is pdDecodeAtCap under a policy whose
// enableBatchCapLearning is learning. Learning, the row reads its running
// count and a preemption rate of 0, so a view that kept its queue would
// pass every learned-cap admission rule (batchCapRules).
func pdDecodeAtCapLearning(t *testing.T, params *capacity.EngineParams, running int, learning bool,
	edit func(*SaturationAnalyzer)) (*SaturationAnalyzer, *domain.AnalyzerResult) {
	t.Helper()
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", decodeVariant, capacity.Record{
		GpuCount: 1, EngineParams: params, LearnedFrom: "deployment",
	}, time.Now())
	a := NewSaturationAnalyzer(store)
	if edit != nil {
		edit(a)
	}
	clock := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return clock }
	rm := withReason(pdDecodeRow(halfCache), 0)
	rm.RunningRequests = running
	if learning {
		rm.RunningRequestsKnown, rm.PreemptionRateKnown = true, true
	}
	in := makeAnalyzerInput([]domain.ReplicaMetrics{rm, pdPrefillRow()}, pdStates)
	in.Config.(*config.ScalingPolicy).EnableBatchCapLearning = learning
	var res *domain.AnalyzerResult
	for range 4 {
		var err error
		res, err = a.Analyze(context.Background(), in)
		require.NoError(t, err)
		clock = clock.Add(30 * time.Second)
	}
	return a, res
}

// A P/D decode at the cap its scale target declares keeps its full queue,
// as one at the cap its flags state does: at the cap every waiting request
// reads deferred, and waits for a batch slot here (#287). Before #420 the
// view read the flags' cap alone, so an annotated decode at its cap dropped
// the queue: no k2, and no queue demand.
func TestBatchCapReaders_APDDecodeAtItsDeclaredCapKeepsTheFullQueue(t *testing.T) {
	a, res := pdDecodeAtCap(t, declaredParams(128), 128, nil)
	assert.True(t, recordedK2(a), "768 waiting behind a full batch: saturated")
	assert.InDelta(t, float64(halfCache)+queueCharge, decodeDemand(t, res), 1,
		"every request waits for a batch slot on this replica: the whole queue is charged")

	a, res = pdDecodeAtCap(t, declaredParams(128), 127, nil)
	assert.False(t, recordedK2(a), "below the declared cap a deferred request is in flight")
	assert.InDelta(t, float64(halfCache), decodeDemand(t, res), 1)
}

// A learned cap is not a cap the P/D decode view reads a decode as AT. It
// may sit below the true cap, where vLLM still promotes landed pulls: the
// deferred requests are in flight, not a backlog. Read as one, a decode at
// a learned 128 recorded k2 at half its cache, was charged 10x its demand,
// and -- observeBatchCap reads the view -- confirmed the cap that caused it.
//
// A guard, not switch-off evidence: it passes with SwitchBatchCapDecodeView
// either way, since neither side reads the learned cap.
func TestBatchCapReaders_ALearnedCapIsNotADecodeViewCap(t *testing.T) {
	params := deploymentParams()
	var an *SaturationAnalyzer
	confirmed := time.Date(2026, 10, 24, 11, 0, 0, 0, time.UTC)
	learned := learnedCapOf(decodeVariant, 128, params, confirmed)
	a, res := pdDecodeAtCapLearning(t, params, 128, true, func(x *SaturationAnalyzer) { learned(x); an = x })
	assert.False(t, recordedK2(a), "the deferred requests are in flight: not saturated")
	assert.InDelta(t, float64(halfCache), decodeDemand(t, res), 1, "only the capacity queue is charged")
	assert.Equal(t, confirmed, learnedCapEntry(t, an, decodeVariant).at, "the in-flight queue confirms nothing")
}

// The switch off reads the flags' cap alone, as before #420.
func TestBatchCapReaders_TheDecodeViewSwitchPicksTheSource(t *testing.T) {
	off := func(a *SaturationAnalyzer) { a.batchCapDecodeView = false }
	a, res := pdDecodeAtCap(t, declaredParams(128), 128, off)
	assert.False(t, recordedK2(a))
	assert.InDelta(t, float64(halfCache), decodeDemand(t, res), 1)

	stated := declaredParams(256)
	stated.MaxNumSeqs, stated.MaxNumSeqsSet = 128, true
	a, res = pdDecodeAtCap(t, stated, 128, off)
	assert.True(t, recordedK2(a), "the flags' cap is read either way")
	assert.InDelta(t, float64(halfCache)+queueCharge, decodeDemand(t, res), 1)
}

// derivedMuSeqs runs one cycle of a decode replica with a short shape whose
// cache holds far more sequences than declared, under params with an ITL
// line published, edit applied, and returns the derived-mu line.
func derivedMuSeqs(t *testing.T, params *capacity.EngineParams, edit func(*SaturationAnalyzer)) map[string]any {
	t.Helper()
	return derivedMuSeqsLearning(t, params, false, edit)
}

// derivedMuSeqsLearning is derivedMuSeqs under a policy whose
// enableBatchCapLearning is learning.
func derivedMuSeqsLearning(t *testing.T, params *capacity.EngineParams, learning bool,
	edit func(*SaturationAnalyzer)) map[string]any {
	t.Helper()
	ctx, logs := observedCtx(t)
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "variant-d", capacity.Record{
		AcceleratorName: "H200", GpuCount: 1, EngineParams: params,
	}, time.Now())
	a := NewSaturationAnalyzer(store)
	a.publishLine(a.itlPhysicsKey("test-ns", "test-model", "variant-d", "H200", 1, ""),
		"a fitted line", itl.Model{A: 0.08, B: 0.01}, time.Now())
	if edit != nil {
		edit(a)
	}
	rm := makeReplicaMetrics("pod-1", "variant-d", 5000, 100_000, 0, 100, 50)
	in := makeAnalyzerInput([]domain.ReplicaMetrics{rm}, []domain.VariantReplicaState{
		{VariantName: "variant-d", Role: domain.RoleDecode, AcceleratorName: "H200",
			CurrentReplicas: 1, GPUsPerReplica: 1},
	})
	in.Config.(*config.ScalingPolicy).EnableBatchCapLearning = learning
	_, err := a.Analyze(ctx, in)
	require.NoError(t, err)
	fields := requireLogged(t, logs, "derived-mu")
	require.Equal(t, true, fields["ok"], "the line prices mu: %v", fields)
	return fields
}

// The derived mu caps the resident sequences at the cap the scale target
// declares, as P3 does. Before #420 it capped them at the parser's default
// of 256, so a sequence-bound engine read a mu above what it serves and the
// throughput floor ordered too few replicas.
func TestBatchCapReaders_TheDerivedMuIsCappedAtTheDeclaredCap(t *testing.T) {
	fields := derivedMuSeqs(t, declaredParams(32), nil)
	assert.InDelta(t, 32, fields["seqs"], 1e-9)

	// With nothing declared the parser's default binds: the cache holds
	// 0.68 x 100000 / 125 = 544 sequences, capped at 256.
	fields = derivedMuSeqs(t, deploymentParams(), nil)
	assert.InDelta(t, 256, fields["seqs"], 1e-9)
}

// The switch off caps at the flags' cap or the default, as before #420.
func TestBatchCapReaders_TheDerivedMuSwitchPicksTheSource(t *testing.T) {
	on := derivedMuSeqs(t, declaredParams(32), nil)
	assert.Equal(t, batchCapDeclared, on["batchCapSource"])
	off := derivedMuSeqs(t, declaredParams(32), func(a *SaturationAnalyzer) { a.batchCapDerivedMu = false })
	assert.Equal(t, batchCapDefault, off["batchCapSource"])
	assert.EqualValues(t, 256, off["batchCap"])
	assert.InDelta(t, 256, off["seqs"], 1e-9)

	stated := declaredParams(64)
	stated.MaxNumSeqs, stated.MaxNumSeqsSet = 32, true
	off = derivedMuSeqs(t, stated, func(a *SaturationAnalyzer) { a.batchCapDerivedMu = false })
	assert.Equal(t, batchCapFromFlags, off["batchCapSource"], "the flags' cap is read either way")
	assert.InDelta(t, 32, off["seqs"], 1e-9)
}

// A declared cap above the parser's 256 -- vLLM V1's own default is 1024 on
// large accelerators -- raises what the readers price at: the documented
// side of the declared cap that orders FEWER replicas than with the cap
// unread. Right when the declaration is true.
func TestBatchCapReaders_ADeclaredCapAbove256(t *testing.T) {
	// mu: the cache's 0.68 x 100000 / 125 = 544 sequences bind, not 256.
	fields := derivedMuSeqs(t, declaredParams(1024), nil)
	assert.EqualValues(t, 1024, fields["batchCap"])
	assert.Equal(t, batchCapDeclared, fields["batchCapSource"])
	assert.InDelta(t, 544, fields["seqs"], 1e-9)

	// ITL admission: a replica at a declared 1024 is at its cap.
	fields = cappedITLWindow(t, declaredParams(1024), 1024, false, nil)
	assert.EqualValues(t, 1024, fields["batchCap"])
	assert.EqualValues(t, 1, fields["capped"])
	assert.EqualValues(t, 0, fields["aboveBand"])
}

// A learned cap lowers the derived mu, as it lowers P3, where the policy
// learns: mu is a price, and a cap below the true one only orders more
// replicas. The cap is stamped as used.
func TestBatchCapReaders_ALearnedCapLowersTheDerivedMu(t *testing.T) {
	params := deploymentParams()
	var a *SaturationAnalyzer
	learned := learnedCapOf("variant-d", 32, params, time.Date(2026, 10, 24, 11, 0, 0, 0, time.UTC))
	fields := derivedMuSeqsLearning(t, params, true, func(x *SaturationAnalyzer) { learned(x); a = x })
	assert.InDelta(t, 32, fields["seqs"], 1e-9)
	assert.Equal(t, batchCapLearned, fields["batchCapSource"])
	assert.False(t, learnedCapEntry(t, a, "variant-d").usedAt.IsZero(), "pricing mu is a use")

	// Learning off: the default binds.
	fields = derivedMuSeqsLearning(t, params, false, learnedCapOf("variant-d", 32, params, time.Time{}))
	assert.InDelta(t, 256, fields["seqs"], 1e-9)
}

// atCapBatchCap is the flags' cap, else -- with its reader's switch, and the
// declared cap's -- the declared one, and never the default or a learned
// cap. It takes no variant, so it reads no learned state and stamps none.
func TestBatchCapReaders_AtCapBatchCap(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	stated := declaredParams(96)
	stated.MaxNumSeqs, stated.MaxNumSeqsSet = 64, true
	assert.Equal(t, pricedBatchCap{64, batchCapFromFlags}, a.atCapBatchCap(stated, true))
	assert.Equal(t, pricedBatchCap{64, batchCapFromFlags}, a.atCapBatchCap(stated, false), "the flags either way")
	assert.Equal(t, pricedBatchCap{96, batchCapDeclared}, a.atCapBatchCap(declaredParams(96), true))
	assert.Equal(t, pricedBatchCap{0, batchCapNone}, a.atCapBatchCap(declaredParams(96), false), "the reader's switch off")
	assert.Equal(t, pricedBatchCap{0, batchCapNone}, a.atCapBatchCap(deploymentParams(), true), "the default is no cap")
	assert.Equal(t, pricedBatchCap{0, batchCapNone}, a.atCapBatchCap(nil, true))
	a.batchCapDeclared = false
	assert.Equal(t, pricedBatchCap{0, batchCapNone}, a.atCapBatchCap(declaredParams(96), true), "the declared cap's switch off")
}
