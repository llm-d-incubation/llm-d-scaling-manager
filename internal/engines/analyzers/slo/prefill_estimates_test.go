package slo

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// The handle to P1's estimates (prefill_estimates.go; S1 tests 1 to 3): the
// slo analyzer reads the saturation analyzer's stash through
// PrefillEstimates, only an estimate stamped with its own cycle, and the two
// analyzers' locks are taken slo, then saturation, then the observer.

// The handle is the saturation analyzer.
var _ PrefillEstimates = (*saturation.SaturationAnalyzer)(nil)

var stashAt = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

// stashPair is a saturation and an slo analyzer on one clock, wired as the
// engine wires them.
type stashPair struct {
	clk *clocktesting.FakePassiveClock
	sat *saturation.SaturationAnalyzer
	slo *Analyzer
}

func newStashPair() stashPair {
	clk := clocktesting.NewFakePassiveClock(stashAt)
	store := capacity.NewStore()
	store.Update("ns", "m", "prefill", capacity.Record{EngineParams: &capacity.EngineParams{Engine: inferenceengine.EngineVLLM}}, stashAt)
	sat := saturation.NewSaturationAnalyzer(store).WithClock(clk)
	return stashPair{clk: clk, sat: sat, slo: New(nil, store).WithClock(clk).WithPrefillEstimates(sat)}
}

// stashInput is one decode and one prefill replica of model m, the prefill
// pod queued at 20000 computed tokens/s.
func stashInput(model string) domain.AnalyzerInput {
	row := func(pod, variant string, queue int) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{PodName: pod, VariantName: variant, Namespace: "ns", ModelID: model, Ready: true,
			ReadySince: stashAt.Add(-time.Hour), KvCacheUsage: 0.3, NumGpuBlocks: 1000, BlockSize: 16,
			TotalKvCapacityTokens: 16000, TokensInUse: 5000, QueueLength: queue, AvgInputTokens: 4000, AvgOutputTokens: 500}
	}
	dec := row("decode-0", "decode", 0)
	pre := row("prefill-0", "prefill", 3)
	pre.PrefillComputedTokenRate, pre.PrefillComputedTokenRateKnown = 20000, true
	pre.RequestRate, pre.RequestRateKnown = 5, true
	return domain.AnalyzerInput{Namespace: "ns", ModelID: model,
		Config: &config.ScalingPolicy{KvCacheThreshold: 0.8, QueueLengthThreshold: 5, ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.7},
		VariantStates: []domain.VariantReplicaState{
			{VariantName: "decode", Role: domain.RoleDecode, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
			{VariantName: "prefill", Role: domain.RolePrefill, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
		},
		ReplicaMetrics: []domain.ReplicaMetrics{dec, pre}}
}

// After saturation's cycle, the slo analyzer reads the estimate it stashed;
// a cycle later, before saturation has run, it reads none: an estimate from
// another cycle is refused.
func TestPrefillEstimates_OnlyTheCyclesOwnEstimateIsRead(t *testing.T) {
	p := newStashPair()
	_, ok := p.slo.prefillEstimate("ns", "m", "prefill", p.clk.Now())
	require.False(t, ok, "precondition: nothing stashed")
	_, err := p.sat.Analyze(context.Background(), stashInput("m"))
	require.NoError(t, err)
	est, ok := p.slo.prefillEstimate("ns", "m", "prefill", p.clk.Now())
	require.True(t, ok, "saturation's estimate of this cycle")
	rows := est.Admitted
	for _, n := range est.Refused {
		rows += n
	}
	assert.Equal(t, 1, rows, "the observation's own estimate: the cycle's one prefill row, admitted or not")
	_, ok = p.slo.prefillEstimate("other-ns", "m", "prefill", p.clk.Now())
	assert.False(t, ok, "keyed by namespace: another namespace's variant of the same name reads none")
	_, ok = p.slo.prefillEstimate("ns", "m", "decode", p.clk.Now())
	assert.False(t, ok, "keyed by variant: the decode variant has none")

	p.clk.SetTime(stashAt.Add(30 * time.Second))
	_, ok = p.slo.prefillEstimate("ns", "m", "prefill", p.clk.Now())
	assert.False(t, ok, "the last cycle's estimate is not this cycle's")
	_, stamped, stashed := p.sat.PrefillEstimate("ns", "m", "prefill")
	require.True(t, stashed, "still stashed: refused for its stamp, not missing")
	assert.Equal(t, stashAt, stamped)
}

// Replay order (S1 test 2): run slo before saturation within a cycle and it
// reads no estimate; run saturation first, as the engine and the replay do,
// and it reads one. A replay that reordered them would read no-estimate.
func TestPrefillEstimates_SLOBeforeSaturationReadsNone(t *testing.T) {
	p := newStashPair()
	for i := range 3 {
		p.clk.SetTime(stashAt.Add(time.Duration(i) * time.Minute))
		_, before := p.slo.prefillEstimate("ns", "m", "prefill", p.clk.Now())
		assert.False(t, before, "cycle %d: slo first reads none", i)
		_, err := p.sat.Analyze(context.Background(), stashInput("m"))
		require.NoError(t, err)
		_, after := p.slo.prefillEstimate("ns", "m", "prefill", p.clk.Now())
		assert.True(t, after, "cycle %d: saturation first, then slo reads it", i)
	}
}

// With no handle, as whatifsource's capture builds the analyzer, nothing is
// read.
func TestPrefillEstimates_NoHandleReadsNothing(t *testing.T) {
	a := New(nil, nil)
	_, ok := a.prefillEstimate("ns", "m", "prefill", time.Now())
	assert.False(t, ok)
}

// The read takes no lock of the slo analyzer's: a caller already holding
// a.mu reads without deadlocking itself
// (sync.Mutex is not reentrant).
func TestPrefillEstimates_TheReadTakesNoSLOLock(t *testing.T) {
	p := newStashPair()
	_, err := p.sat.Analyze(context.Background(), stashInput("m"))
	require.NoError(t, err)
	got := make(chan bool, 1)
	p.slo.mu.Lock()
	go func() {
		_, ok := p.slo.prefillEstimate("ns", "m", "prefill", p.clk.Now())
		got <- ok
	}()
	select {
	case ok := <-got:
		assert.True(t, ok)
	case <-time.After(10 * time.Second):
		t.Fatal("the read blocked on slo's mu")
	}
	p.slo.mu.Unlock()
}

// The slow interval end to end: at a 120 s step, the engine's order each
// cycle -- saturation's sweep, saturation's Analyze, then slo's read --
// reads the cycle's estimate every cycle. Swept on a minute, the sweep
// dropped the last cycle's entry, which Analyze then rewrote; the read
// holds either way, and this pins it.
func TestPrefillEstimates_ASlowIntervalReadsEveryCycle(t *testing.T) {
	p := newStashPair()
	for i := range 6 {
		p.clk.SetTime(stashAt.Add(time.Duration(i) * 120 * time.Second))
		ev := p.sat.EvictStaleHistory()
		assert.Zero(t, ev.PrefillEstimates, "cycle %d: nothing evicted", i)
		_, err := p.sat.Analyze(context.Background(), stashInput("m"))
		require.NoError(t, err)
		_, ok := p.slo.prefillEstimate("ns", "m", "prefill", p.clk.Now())
		assert.True(t, ok, "cycle %d", i)
	}
}

// A flush refuses the read until the next saturation cycle writes again.
func TestPrefillEstimates_AFlushRefusesUntilTheNextCycle(t *testing.T) {
	p := newStashPair()
	_, err := p.sat.Analyze(context.Background(), stashInput("m"))
	require.NoError(t, err)
	p.sat.ForgetLearnedState()
	_, ok := p.slo.prefillEstimate("ns", "m", "prefill", p.clk.Now())
	assert.False(t, ok, "after the flush")
	p.clk.SetTime(stashAt.Add(30 * time.Second))
	_, err = p.sat.Analyze(context.Background(), stashInput("m"))
	require.NoError(t, err)
	_, ok = p.slo.prefillEstimate("ns", "m", "prefill", p.clk.Now())
	assert.True(t, ok, "the next cycle's write")
}

// Lock order (S1 test 3): saturation cycles, its forget, flush and sweep run
// while slo readers, from several goroutines, read the stash and then take
// slo's own mu, the order S1b keeps (the read before slo.mu, never inside
// it). Run under -race, a lock taken out of order is a deadlock this test
// would hang on, and an unguarded stash a reported race.
func TestPrefillEstimates_ConcurrentReadsAndSaturationCycles(t *testing.T) {
	p := newStashPair()
	// The clock is fixed for the run: the reads must find the estimates of
	// the cycle in progress, stamped at the same reading.
	var wg sync.WaitGroup
	var reads atomic.Int64
	done := make(chan struct{})
	for m := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			model := fmt.Sprintf("m%d", m)
			for range 25 {
				if _, err := p.sat.Analyze(context.Background(), stashInput(model)); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 25 {
			p.sat.ForgetVariant("ns", "m0", "prefill", fmt.Sprintf("t%d", i))
			p.sat.EvictStaleHistory()
			if i%10 == 0 {
				p.sat.ForgetLearnedState()
			}
		}
	}()
	var readers sync.WaitGroup
	for m := range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			model := fmt.Sprintf("m%d", m)
			for {
				select {
				case <-done:
					return
				default:
				}
				_, ok := p.slo.prefillEstimate("ns", model, "prefill", p.clk.Now())
				// Then slo's own lock, after the read, as S1b takes it.
				p.slo.mu.Lock()
				if ok {
					reads.Add(1)
				}
				p.slo.mu.Unlock()
				p.slo.EvictStale(p.clk.Now())
			}
		}()
	}
	finished := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
		readers.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(2 * time.Minute):
		t.Fatal("deadlock: the cycles and the reads did not finish")
	}
	assert.Positive(t, reads.Load(), "some read found a stashed estimate, so the reads overlapped the cycles")
}
