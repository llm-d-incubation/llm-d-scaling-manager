package saturation

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// The prefill observation through Analyze (prefill_observe.go): the switch
// on logs one prefill-capacity-observed line per prefill variant per cycle,
// and off logs none.

// prefillObserveRun runs cycles of a P/D model, one decode and one prefill
// replica of engine, the prefill pod queued at a steady computed-token rate,
// and returns the analyzer's lines at perReplicaLogLevel.
func prefillObserveRun(t *testing.T, engine inferenceengine.Engine, observe bool, cycles int) []string {
	t.Helper()
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "prefill", capacity.Record{EngineParams: &capacity.EngineParams{Engine: engine}}, propStart)
	a := NewSaturationAnalyzer(store)
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchPrefillCapacityObserve: observe}))
	clock := propStart
	a.now = func() time.Time { return clock }
	var lines []string
	ctx := ctrl.LoggerInto(context.Background(),
		funcr.New(func(prefix, args string) { lines = append(lines, prefix+" "+args) }, funcr.Options{Verbosity: int(perReplicaLogLevel)}))
	states := []domain.VariantReplicaState{
		{VariantName: "decode", Role: domain.RoleDecode, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
		{VariantName: "prefill", Role: domain.RolePrefill, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
	}
	for range cycles {
		dec := makeReplicaMetrics("decode-0", "decode", 5000, 16000, 0, 4000, 500)
		pre := makeReplicaMetrics("prefill-0", "prefill", 0, 16000, 3, 4000, 1)
		pre.Ready, pre.ReadySince = true, propStart.Add(-time.Hour)
		pre.PrefillComputedTokenRate, pre.PrefillComputedTokenRateKnown = 20000, true
		pre.RequestRate, pre.RequestRateKnown = 5, true
		dec.Ready, dec.ReadySince = true, propStart.Add(-time.Hour)
		_, err := a.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{dec, pre}, states))
		require.NoError(t, err)
		clock = clock.Add(time.Minute)
	}
	return lines
}

// observedLines is the prefill-capacity-observed lines of lines.
func observedLines(lines []string) []string {
	var out []string
	for _, l := range lines {
		if strings.Contains(l, `"prefill-capacity-observed"`) {
			out = append(out, l)
		}
	}
	return out
}

// The switch on logs one line per prefill variant per cycle, naming the
// prefill variant only; off, none. On, three queued minutes are a peak under
// a queue.
func TestPrefillObserve_TheSwitchLogsTheObservation(t *testing.T) {
	on := observedLines(prefillObserveRun(t, inferenceengine.EngineVLLM, true, 5))
	require.Len(t, on, 5, "one line per cycle for the one prefill variant")
	for _, l := range on {
		assert.Contains(t, l, `"variant"="prefill"`)
	}
	last := on[len(on)-1]
	assert.Contains(t, last, `"vpSource"="peak-under-queue"`)
	assert.Contains(t, last, `"vp"=20000`)
	assert.Contains(t, last, `"fRefused"="OnePod"`)

	off := observedLines(prefillObserveRun(t, inferenceengine.EngineVLLM, false, 5))
	assert.Empty(t, off, "the switch off observes nothing")
}

// An SGLang prefill variant is refused V_P through Analyze: the engine is
// read from the variant's scale target.
func TestPrefillObserve_AnSGLangVariantIsRefusedVP(t *testing.T) {
	on := observedLines(prefillObserveRun(t, inferenceengine.EngineSGLang, true, 5))
	require.Len(t, on, 5)
	last := on[len(on)-1]
	assert.Contains(t, last, `"vpRefused"="SGLangNoComputedTokens"`)
	assert.Contains(t, last, `"engine"="sglang"`)
	assert.Contains(t, last, `"vp"=0`)
}

// The back-pressure bound scenario (prefill_bound_test.go's fixture): a P/D
// model whose prefill queues behind a saturated decode, so the bound is
// learned, and then serves a doubled load with an empty queue.
const (
	boundPrefillVariant = "prefill-v"
	boundPrefillKv      = int64(1_149_312)
	boundPrompt         = 6000.0
	// boundTokens is the bound the scenario learns, in computed tokens/s:
	// the first and highest of its two readings.
	boundTokens = 4 * boundPrompt
)

func boundDecode(pod string, tokensInUse int64, queue int) domain.ReplicaMetrics {
	rm := makeReplicaMetrics(pod, decodeVariant, tokensInUse, runKvCapacity, queue, boundPrompt, 1000)
	rm.RequestRate = 4.73
	rm.GenerationTokenRate = rm.RequestRate * rm.AvgOutputTokens
	return rm
}

func boundDecodeFull() []domain.ReplicaMetrics {
	return []domain.ReplicaMetrics{boundDecode("decode-0", 970_475, 36), boundDecode("decode-1", 1_039_474, 81)}
}

func boundDecodeCoping() []domain.ReplicaMetrics {
	return []domain.ReplicaMetrics{boundDecode("decode-0", 300_000, 0), boundDecode("decode-1", 280_000, 0)}
}

func boundPrefillRow(queue int, tokens int64, computed float64) domain.ReplicaMetrics {
	rm := makeReplicaMetrics("prefill-0", boundPrefillVariant, tokens, boundPrefillKv, queue, boundPrompt, 1)
	rm.RequestRate, rm.RequestRateKnown = computed/boundPrompt, true
	rm.PrefillComputedTokenRate, rm.PrefillComputedTokenRateKnown = computed, true
	return rm
}

var boundStates = []domain.VariantReplicaState{
	{VariantName: decodeVariant, Role: domain.RoleDecode, AcceleratorName: "H200", CurrentReplicas: 2, GPUsPerReplica: 1},
	{VariantName: boundPrefillVariant, Role: domain.RolePrefill, AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1},
}

// boundRun is one analyzer driven through the bound scenario, its clock and
// its observed lines.
type boundRun struct {
	t     *testing.T
	a     *SaturationAnalyzer
	clock time.Time
	lines []string
	ctx   context.Context
}

func newBoundRun(t *testing.T, observe bool) *boundRun {
	t.Helper()
	r := &boundRun{t: t, a: NewSaturationAnalyzer(capacity.NewStore()), clock: propStart}
	require.NoError(t, r.a.SetSwitchesForReplay(map[string]bool{SwitchPrefillCapacityObserve: observe}))
	r.a.now = func() time.Time { return r.clock }
	r.ctx = ctrl.LoggerInto(context.Background(),
		funcr.New(func(prefix, args string) { r.lines = append(r.lines, prefix+" "+args) },
			funcr.Options{Verbosity: int(perReplicaLogLevel)}))
	return r
}

// analyze runs one cycle under ctx and returns its result (AnalyzedAt
// cleared) and its observed lines.
func (r *boundRun) analyze(ctx context.Context, rows []domain.ReplicaMetrics, lambda float64, bound bool) (*domain.AnalyzerResult, []string, error) {
	in := makeAnalyzerInput(rows, boundStates)
	in.ArrivalRate = lambda
	in.Config.(*config.ScalingPolicy).EnablePrefillThroughputBound = bound
	r.lines = nil
	res, err := r.a.Analyze(ctx, in)
	if res != nil {
		res.AnalyzedAt = time.Time{}
	}
	return res, observedLines(r.lines), err
}

// learn runs the two cycles that learn the bound: decode saturated, prefill
// queued behind it, each reading a little lower than the last.
func (r *boundRun) learn(bound bool) []domain.AnalyzerResult {
	out := make([]domain.AnalyzerResult, 0, 2)
	for i := range 2 {
		r.clock = r.clock.Add(ThroughputSampleSpacing)
		res, _, err := r.analyze(r.ctx, append(boundDecodeFull(),
			boundPrefillRow(30, 357_800, boundTokens*(1-0.01*float64(i)))), runLambda, bound)
		require.NoError(r.t, err)
		out = append(out, *res)
	}
	return out
}

// scenario runs the whole scenario and returns each cycle's result and
// observed lines: learn; the load doubles while decode is still saturated,
// prefill's queue empty, so the prefill hold stands on decodeSaturated;
// decode recovers, and the bound orders prefill before its queue forms.
func (r *boundRun) scenario(bound bool) ([]domain.AnalyzerResult, [][]string) {
	out := r.learn(bound)
	lines := [][]string{nil, nil}
	step := func(rows []domain.ReplicaMetrics) {
		res, obs, err := r.analyze(r.ctx, rows, 2*runLambda, bound)
		require.NoError(r.t, err)
		out = append(out, *res)
		lines = append(lines, obs)
	}
	r.clock = r.clock.Add(15 * time.Second)
	step(append(boundDecodeFull(), boundPrefillRow(0, 20_000, 2*runLambda*boundPrompt)))
	r.clock = r.clock.Add(DecodeSaturationMemory + time.Second)
	for range 3 {
		r.clock = r.clock.Add(15 * time.Second)
		step(append(boundDecodeCoping(), boundPrefillRow(0, 20_000, 2*runLambda*boundPrompt)))
	}
	return out, lines
}

// The observation writes no state any output reads: the same cycles give the
// same results with the switch on and off. The scenario makes prefill's
// demand depend on both inputs the observation shares with pricing, so an
// observation that touched either would move a result: the back-pressure
// bound (on, learned while decode saturates, ordering prefill once decode
// recovers) and decode's saturation (holding prefill on the cycle decode is
// still saturated). Two mutants of observePrefill fail here: one clearing
// c.decodeSaturated, one deleting the bound's windows.
func TestPrefillObserve_MovesNoResult(t *testing.T) {
	off, _ := newBoundRun(t, false).scenario(true)
	// The premises: the bound moves prefill's demand, and the held cycle's is
	// below the first recovered cycle's at the same load.
	noBound, _ := newBoundRun(t, false).scenario(false)
	require.NotEqual(t, noBound, off, "the bound must move a result")
	require.Less(t, off[2].RoleDemand[domain.RolePrefill], off[3].RoleDemand[domain.RolePrefill],
		"decode's saturation must hold prefill")
	on, _ := newBoundRun(t, true).scenario(true)
	assert.Equal(t, off, on)
}

// The bound reaches the observation through Analyze: every cycle after it is
// learned observes V_P from it, in computed tokens/s, with its two readings.
// The scenario's prefill never queues for three minutes in a row, so the
// bound is the only source.
func TestPrefillObserve_TheBoundIsObservedThroughAnalyze(t *testing.T) {
	_, lines := newBoundRun(t, true).scenario(true)
	for i := 2; i < len(lines); i++ {
		require.Len(t, lines[i], 1, "cycle %d", i)
		l := lines[i][0]
		assert.Contains(t, l, `"vpSource"="back-pressure-bound"`, "cycle %d", i)
		// The mean of its two readings, 24000 and 23760: two minutes hold
		// no lower confidence bound above it (SwitchPrefillBoundHeldConfidence).
		assert.Contains(t, l, `"vp"=23880`, "cycle %d: the bound in tokens/s", i)
		assert.Contains(t, l, `"vpSamples"=2`, "cycle %d: its two readings", i)
		assert.Contains(t, l, `"vpLowerBound"=true`, "cycle %d", i)
	}
}

// doneAfter is a context that is cancelled from its (n+1)th Done call on,
// so a cycle's pricing fails part way through its rows.
type doneAfter struct {
	context.Context
	n    int
	done chan struct{}
}

func newDoneAfter(ctx context.Context, n int) *doneAfter {
	d := &doneAfter{Context: ctx, n: n, done: make(chan struct{})}
	close(d.done)
	return d
}

func (d *doneAfter) Done() <-chan struct{} {
	d.n--
	if d.n < 0 {
		return d.done
	}
	return nil
}

func (d *doneAfter) Err() error {
	if d.n < 0 {
		return context.Canceled
	}
	return nil
}

// A cycle whose pricing fails after the prefill replica read the bound
// leaves nothing for the next cycle: with the bound off, the next cycle
// stashes none of its own, and its observation has no back-pressure bound.
func TestPrefillObserve_AFailedCycleLeavesNoBound(t *testing.T) {
	r := newBoundRun(t, true)
	r.learn(true)
	r.clock = r.clock.Add(15 * time.Second)
	// The prefill row first: the cycle is cancelled on the row after it.
	rows := append([]domain.ReplicaMetrics{boundPrefillRow(0, 20_000, 2*runLambda*boundPrompt)}, boundDecodeFull()...)
	_, _, err := r.analyze(newDoneAfter(r.ctx, 1), rows, 2*runLambda, true)
	require.ErrorIs(t, err, context.Canceled)

	r.clock = r.clock.Add(15 * time.Second)
	_, obs, err := r.analyze(r.ctx, rows, 2*runLambda, false)
	require.NoError(t, err)
	require.Len(t, obs, 1)
	assert.NotContains(t, obs[0], `"vpSource"="back-pressure-bound"`, "the failed cycle's bound was inherited")
	assert.Contains(t, obs[0], `"vpRefused"="NoPrefillCapacity"`)
}

// observeHarness drives the P/D model of prefillObserveRun one cycle at a
// time, the prefill pod queued, with the engine arguments in the store. The
// settle tracker admits the pod from its third cycle (prefill.SteadyFor).
type observeHarness struct {
	t     *testing.T
	a     *SaturationAnalyzer
	store *capacity.Store
	clock time.Time
	lines []string
	ctx   context.Context
}

// newObserveHarness returns a harness with the observation on and params
// recorded for the prefill variant.
func newObserveHarness(t *testing.T, params *capacity.EngineParams) *observeHarness {
	t.Helper()
	h := &observeHarness{t: t, store: capacity.NewStore(), clock: propStart}
	h.setParams(params)
	h.a = NewSaturationAnalyzer(h.store)
	require.NoError(t, h.a.SetSwitchesForReplay(map[string]bool{SwitchPrefillCapacityObserve: true}))
	h.a.now = func() time.Time { return h.clock }
	h.ctx = ctrl.LoggerInto(context.Background(),
		funcr.New(func(prefix, args string) { h.lines = append(h.lines, prefix+" "+args) },
			funcr.Options{Verbosity: int(perReplicaLogLevel)}))
	return h
}

// setParams records the prefill variant's engine arguments, as the engine
// does from its scale target each cycle.
func (h *observeHarness) setParams(params *capacity.EngineParams) {
	h.store.Update("test-ns", "test-model", "prefill", capacity.Record{EngineParams: params}, h.clock)
}

// cycle runs one cycle, the prefill pod computing rate tokens/s with a
// queue, a minute after the last, and returns its observed line.
func (h *observeHarness) cycle(rate float64) string {
	h.t.Helper()
	states := []domain.VariantReplicaState{
		{VariantName: "decode", Role: domain.RoleDecode, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
		{VariantName: "prefill", Role: domain.RolePrefill, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
	}
	dec := makeReplicaMetrics("decode-0", "decode", 5000, 16000, 0, 4000, 500)
	pre := makeReplicaMetrics("prefill-0", "prefill", 0, 16000, 3, 4000, 1)
	pre.PrefillComputedTokenRate, pre.PrefillComputedTokenRateKnown = rate, true
	pre.RequestRate, pre.RequestRateKnown = rate/4000, true
	h.lines = nil
	_, err := h.a.Analyze(h.ctx, makeAnalyzerInput([]domain.ReplicaMetrics{dec, pre}, states))
	require.NoError(h.t, err)
	h.clock = h.clock.Add(time.Minute)
	obs := observedLines(h.lines)
	require.Len(h.t, obs, 1)
	return obs[0]
}

// A change of engine arguments ends the old engine's peak: a redeploy with a
// lower --max-num-batched-tokens starts its own V_P instead of reading the
// old one's for prefill.Retention.
func TestPrefillObserve_AnArgsChangeEndsTheOldPeak(t *testing.T) {
	h := newObserveHarness(t, &capacity.EngineParams{Engine: inferenceengine.EngineVLLM, MaxNumBatchedTokens: 8192, EffectiveMaxBatchedTokens: 8192})
	var last string
	for range 5 {
		last = h.cycle(20000)
	}
	require.Contains(t, last, `"vp"=20000`)

	h.setParams(&capacity.EngineParams{Engine: inferenceengine.EngineVLLM, MaxNumBatchedTokens: 2048, EffectiveMaxBatchedTokens: 2048})
	first := h.cycle(10000)
	assert.Contains(t, first, `"vpRefused"="NoPrefillCapacity"`, "the new arguments have no peak yet")
	for range 2 {
		last = h.cycle(10000)
	}
	assert.Contains(t, last, `"vp"=10000`, "the new arguments' own peak, not the old one")
}

// llm-d.ai/forget-learned-state ends the observation's state, by variant
// (ForgetVariant) and in full (ForgetLearnedState).
func TestPrefillObserve_AForgetEndsTheState(t *testing.T) {
	params := &capacity.EngineParams{Engine: inferenceengine.EngineVLLM, MaxNumBatchedTokens: 8192, EffectiveMaxBatchedTokens: 8192}
	for _, tc := range []struct {
		name   string
		forget func(h *observeHarness)
	}{
		{"variant", func(h *observeHarness) {
			h.a.ForgetVariant("test-ns", "test-model", "prefill", "t1")
			// The variant's record went with it; the engine reloads it from
			// the scale target on the same cycle, under the same arguments,
			// so the key is the one forgotten.
			h.setParams(params)
		}},
		{"all", func(h *observeHarness) { h.a.ForgetLearnedState() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newObserveHarness(t, params)
			var last string
			for range 5 {
				last = h.cycle(20000)
			}
			require.Contains(t, last, `"vp"=20000`)
			tc.forget(h)
			assert.Contains(t, h.cycle(20000), `"vpRefused"="NoPrefillCapacity"`, "the peak is forgotten")
		})
	}
}

// Forgetting variant prefill leaves its sibling prefill-2 alone: the prefix
// ends at the variant's separator, so "prefill" does not match "prefill-2".
func TestPrefillObserve_ForgettingAVariantLeavesItsSibling(t *testing.T) {
	params := &capacity.EngineParams{Engine: inferenceengine.EngineVLLM, MaxNumBatchedTokens: 8192, EffectiveMaxBatchedTokens: 8192}
	variants := []string{"prefill", "prefill-2"}
	store := capacity.NewStore()
	for _, v := range variants {
		store.Update("test-ns", "test-model", v, capacity.Record{EngineParams: params}, propStart)
	}
	a := NewSaturationAnalyzer(store)
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchPrefillCapacityObserve: true}))
	clock := propStart
	a.now = func() time.Time { return clock }
	var lines []string
	ctx := ctrl.LoggerInto(context.Background(),
		funcr.New(func(prefix, args string) { lines = append(lines, prefix+" "+args) },
			funcr.Options{Verbosity: int(perReplicaLogLevel)}))
	states := []domain.VariantReplicaState{
		{VariantName: "decode", Role: domain.RoleDecode, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
		{VariantName: "prefill", Role: domain.RolePrefill, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
		{VariantName: "prefill-2", Role: domain.RolePrefill, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
	}
	// cycle runs one cycle, each prefill pod queued at 20000 tokens/s, and
	// returns each variant's observed line.
	cycle := func() map[string]string {
		rows := make([]domain.ReplicaMetrics, 0, 1+len(variants))
		rows = append(rows, makeReplicaMetrics("decode-0", "decode", 5000, 16000, 0, 4000, 500))
		for _, v := range variants {
			pre := makeReplicaMetrics(v+"-0", v, 0, 16000, 3, 4000, 1)
			pre.PrefillComputedTokenRate, pre.PrefillComputedTokenRateKnown = 20000, true
			pre.RequestRate, pre.RequestRateKnown = 5, true
			rows = append(rows, pre)
		}
		lines = nil
		_, err := a.Analyze(ctx, makeAnalyzerInput(rows, states))
		require.NoError(t, err)
		clock = clock.Add(time.Minute)
		out := map[string]string{}
		for _, l := range observedLines(lines) {
			for _, v := range variants {
				if strings.Contains(l, `"variant"="`+v+`"`) {
					out[v] = l
				}
			}
		}
		require.Len(t, out, 2)
		return out
	}
	var last map[string]string
	for range 5 {
		last = cycle()
	}
	for _, v := range variants {
		require.Contains(t, last[v], `"vp"=20000`, v)
	}
	// A bound stashed for each, as a cycle that never took it would leave.
	a.mu.Lock()
	for _, v := range variants {
		a.prefillBounds[prefillBoundStashKey("test-ns", "test-model", v)] =
			stashedBound{bound: prefill.Bound{Tokens: 1, Samples: 2}, at: clock}
	}
	a.mu.Unlock()
	a.ForgetVariant("test-ns", "test-model", "prefill", "t1")
	assert.NotContains(t, a.prefillBounds, prefillBoundStashKey("test-ns", "test-model", "prefill"),
		"the forgotten variant's stashed bound")
	assert.Contains(t, a.prefillBounds, prefillBoundStashKey("test-ns", "test-model", "prefill-2"))
	store.Update("test-ns", "test-model", "prefill", capacity.Record{EngineParams: params}, clock)
	got := cycle()
	assert.Contains(t, got["prefill"], `"vpRefused"="NoPrefillCapacity"`, "the forgotten variant")
	assert.Contains(t, got["prefill-2"], `"vp"=20000`, "the sibling keeps its peak")
}

// The eviction sweep forgets the observation's keys with no cycle for
// prefill.Retention, and bounds stashed for a cycle that never took them, and
// counts both: without it a key outlived its variant until the next Observe,
// which a model with no prefill variant left never makes.
func TestPrefillObserve_TheEvictionSweepForgetsIdleState(t *testing.T) {
	h := newObserveHarness(t, &capacity.EngineParams{Engine: inferenceengine.EngineVLLM})
	h.cycle(20000)
	require.Equal(t, 1, h.a.prefillObserver.Len())
	h.a.mu.Lock()
	h.a.prefillBounds[prefillBoundStashKey("test-ns", "other-model", "prefill")] =
		stashedBound{bound: prefill.Bound{Tokens: 1, Samples: 2}, at: h.clock}
	h.a.mu.Unlock()

	ev := h.a.EvictStaleHistory()
	assert.Zero(t, ev.PrefillObservations, "a key seen within Retention stays")
	assert.Zero(t, ev.PrefillBounds, "a bound stashed just now stays")

	h.clock = h.clock.Add(prefill.Retention + time.Minute)
	ev = h.a.EvictStaleHistory()
	assert.Equal(t, 1, ev.PrefillObservations)
	assert.Equal(t, 1, ev.PrefillBounds)
	assert.True(t, ev.Any())
	assert.Zero(t, h.a.prefillObserver.Len())
	assert.Empty(t, h.a.prefillBounds)
}

// A cycle takes its model's stashed bounds, those of variants it does not
// have too, and leaves another model's.
func TestPrefillObserve_ACycleClearsItsModelsUntakenBounds(t *testing.T) {
	h := newObserveHarness(t, &capacity.EngineParams{Engine: inferenceengine.EngineVLLM})
	gone := prefillBoundStashKey("test-ns", "test-model", "gone")
	other := prefillBoundStashKey("test-ns", "other-model", "prefill")
	h.a.mu.Lock()
	for _, k := range []string{gone, other} {
		h.a.prefillBounds[k] = stashedBound{bound: prefill.Bound{Tokens: 1, Samples: 2}, at: h.clock}
	}
	h.a.mu.Unlock()
	h.cycle(20000)
	assert.NotContains(t, h.a.prefillBounds, gone, "a variant the cycle does not have")
	assert.Contains(t, h.a.prefillBounds, other, "another model's")
}

// The stash keeps a variant's highest bound of the cycle: its replicas can
// read different windows (one per input bucket), each a floor on the true
// rate. A stash from an earlier time is replaced whatever its value.
func TestPrefillObserve_TheStashKeepsTheHighestBoundOfTheCycle(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	clock := propStart
	a.now = func() time.Time { return clock }
	rm := makeReplicaMetrics("prefill-0", "prefill", 0, 16000, 3, 4000, 1)
	key := prefillBoundStashKey(rm.Namespace, rm.ModelID, rm.VariantName)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stashPrefillBound(rm, 20000, 2)
	a.stashPrefillBound(rm, 10000, 5)
	assert.Equal(t, prefill.Bound{Tokens: 20000, Samples: 2}, a.prefillBounds[key].bound, "a lower reading does not replace it")
	a.stashPrefillBound(rm, 30000, 3)
	assert.Equal(t, prefill.Bound{Tokens: 30000, Samples: 3}, a.prefillBounds[key].bound, "a higher one does")
	clock = clock.Add(time.Minute)
	a.stashPrefillBound(rm, 1000, 2)
	assert.Equal(t, prefill.Bound{Tokens: 1000, Samples: 2}, a.prefillBounds[key].bound, "a new cycle's replaces it")
}
