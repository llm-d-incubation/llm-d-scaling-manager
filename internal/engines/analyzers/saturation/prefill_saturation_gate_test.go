package saturation

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/require"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// Prefill saturated without an engine queue (prefill_saturation_gate.go).

// statedBudget is a prefill engine whose flags state a per-step token budget
// of 65536, the staircase's.
func statedBudget() *capacity.EngineParams {
	return &capacity.EngineParams{Engine: inferenceengine.EngineVLLM, MaxNumBatchedTokens: 65536, EffectiveMaxBatchedTokens: 65536}
}

// runningPrefill is a prefill row with running requests of prompt tokens and
// no queue.
func runningPrefill(running int, prompt float64) domain.ReplicaMetrics {
	rm := makeReplicaMetrics("prefill-0", "prefill", 64000, 1_000_000, 0, prompt, 1)
	rm.RunningRequests, rm.RunningRequestsKnown = running, true
	return rm
}

func TestPrefillAtBatchBudget(t *testing.T) {
	cases := []struct {
		name   string
		rm     func() domain.ReplicaMetrics
		params *capacity.EngineParams
		want   bool
	}{
		{"eight 8000-token prompts fill a 65536 budget", func() domain.ReplicaMetrics { return runningPrefill(8, 8000) }, statedBudget(), true},
		{"at the fraction exactly", func() domain.ReplicaMetrics { return runningPrefill(1, 0.9*65536) }, statedBudget(), true},
		{"just under the fraction", func() domain.ReplicaMetrics { return runningPrefill(1, 0.9*65536-1) }, statedBudget(), false},
		{"seven prompts are under it", func() domain.ReplicaMetrics { return runningPrefill(7, 8000) }, statedBudget(), false},
		{"nothing running", func() domain.ReplicaMetrics { return runningPrefill(0, 8000) }, statedBudget(), false},
		{"the running count not read", func() domain.ReplicaMetrics {
			rm := runningPrefill(8, 8000)
			rm.RunningRequestsKnown = false
			return rm
		}, statedBudget(), false},
		{"no engine arguments", func() domain.ReplicaMetrics { return runningPrefill(8, 8000) }, nil, false},
		{"a budget the flags do not state", func() domain.ReplicaMetrics { return runningPrefill(8, 8000) },
			&capacity.EngineParams{Engine: inferenceengine.EngineVLLM, EffectiveMaxBatchedTokens: 8192}, false},
		{"no prompt length", func() domain.ReplicaMetrics { return runningPrefill(8, 0) }, statedBudget(), false},
		{"an infinite prompt length", func() domain.ReplicaMetrics { return runningPrefill(8, math.Inf(1)) }, statedBudget(), false},
		{"the recent prompt length wins", func() domain.ReplicaMetrics {
			rm := runningPrefill(8, 1000)
			rm.AvgInputTokensRecent = 8000
			return rm
		}, statedBudget(), true},
		{"prefix-cache hits are not computed", func() domain.ReplicaMetrics {
			rm := runningPrefill(8, 8000)
			rm.PrefixCacheHitRate = 0.5
			return rm
		}, statedBudget(), false},
		{"a NaN hit rate discounts nothing", func() domain.ReplicaMetrics {
			rm := runningPrefill(8, 8000)
			rm.PrefixCacheHitRate = math.NaN()
			return rm
		}, statedBudget(), true},
		{"the budget is per rank", func() domain.ReplicaMetrics {
			rm := runningPrefill(8, 8000)
			rm.EngineRanks = 2
			return rm
		}, statedBudget(), false},
		{"two ranks full", func() domain.ReplicaMetrics {
			rm := runningPrefill(16, 8000)
			rm.EngineRanks = 2
			return rm
		}, statedBudget(), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, prefillAtBatchBudget(c.rm(), c.params))
		})
	}
}

func TestRouterQueueSaturatesPrefill(t *testing.T) {
	q := func(n int64) *domain.SchedulerQueueMetrics { return &domain.SchedulerQueueMetrics{QueueSize: n} }
	require.False(t, routerQueueSaturatesPrefill(nil, 2, 5), "flow control off")
	require.False(t, routerQueueSaturatesPrefill(q(9), 2, 5), "under the threshold per replica")
	require.True(t, routerQueueSaturatesPrefill(q(10), 2, 5), "at it")
	require.True(t, routerQueueSaturatesPrefill(q(1000), 2, 5), "over it")
	require.False(t, routerQueueSaturatesPrefill(q(1000), 0, 5), "no Ready prefill replica")
	require.False(t, routerQueueSaturatesPrefill(q(1000), 2, 0), "no threshold")
	require.False(t, routerQueueSaturatesPrefill(q(1000), 2, math.NaN()), "a NaN threshold")
}

func TestReadyPrefillReplicas(t *testing.T) {
	roles := map[string]string{"p": domain.RolePrefill, "d": domain.RoleDecode}
	rows := []domain.ReplicaMetrics{
		{VariantName: "p", Ready: true}, {VariantName: "p", Ready: false},
		{VariantName: "p", Ready: true, FromWarmPool: true}, {VariantName: "d", Ready: true},
	}
	require.Equal(t, 2, readyPrefillReplicas(rows, roles))
}

// The gate opens only for a prefill row, only without a queue of its own,
// and only under the switch whose signal fired.
func TestPrefillGateOpenedBy(t *testing.T) {
	full := runningPrefill(8, 8000)
	for _, c := range []struct {
		name           string
		budget, router bool
		rm             domain.ReplicaMetrics
		role           string
		routerOpen     bool
		want           string
	}{
		{"both off", false, false, full, domain.RolePrefill, true, ""},
		{"budget on, batch full", true, false, full, domain.RolePrefill, false, prefillGateBatchBudget},
		{"budget on, batch not full", true, false, runningPrefill(2, 8000), domain.RolePrefill, false, ""},
		{"router on, queue over", false, true, runningPrefill(0, 8000), domain.RolePrefill, true, prefillGateRouterQueue},
		{"router on, queue under", false, true, full, domain.RolePrefill, false, ""},
		{"both on, the batch named first", true, true, full, domain.RolePrefill, true, prefillGateBatchBudget},
		{"decode never", true, true, full, domain.RoleDecode, true, ""},
		{"aggregated never", true, true, full, domain.RoleBoth, true, ""},
		{"a queue of its own needs neither", true, true, func() domain.ReplicaMetrics {
			rm := full
			rm.QueueLength = 5
			return rm
		}(), domain.RolePrefill, true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := NewSaturationAnalyzer(capacity.NewStore())
			require.NoError(t, a.SetSwitchesForReplay(map[string]bool{
				SwitchPrefillSaturatedAtBatchBudget: c.budget, SwitchPrefillSaturatedOnRouterQueue: c.router}))
			require.Equal(t, c.want, a.prefillGateOpenedBy(c.rm, c.role, 5, statedBudget(), c.routerOpen))
		})
	}
}

func TestGateQueue(t *testing.T) {
	rm := runningPrefill(8, 8000)
	require.Equal(t, 0, gateQueue(rm, 5, replicaRouting{}), "closed: the row's own queue")
	require.Equal(t, 5, gateQueue(rm, 5, replicaRouting{prefillGate: prefillGateBatchBudget}), "open: the threshold")
	require.Equal(t, 5, gateQueue(rm, 4.5, replicaRouting{prefillGate: prefillGateBatchBudget}), "a fractional threshold rounds up")
	rm.QueueLength = 3
	require.Equal(t, 5, gateQueue(rm, 5, replicaRouting{prefillGate: prefillGateRouterQueue}))
}

// Both switches are off from the constructor: nothing this file adds runs
// in production until a replay shows it should.
func TestPrefillSaturationGate_OffFromTheConstructor(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	require.False(t, a.prefillBatchBudget)
	require.False(t, a.prefillRouterQueue)
}

// gateRun is one cycle of a P/D model, one decode and one prefill replica,
// the prefill engine's arguments params, its row pre and the router's queue
// q, with the two switches as given. It returns the prefill variant's k2
// source and the analyzer's lines at perReplicaLogLevel.
func gateRun(t *testing.T, budget, router bool, params *capacity.EngineParams, pre domain.ReplicaMetrics,
	dec domain.ReplicaMetrics, q *domain.SchedulerQueueMetrics, mut ...func(*config.ScalingPolicy)) (string, []string) {
	t.Helper()
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "prefill", capacity.Record{EngineParams: params}, propStart)
	a := NewSaturationAnalyzer(store)
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{
		SwitchPrefillSaturatedAtBatchBudget: budget, SwitchPrefillSaturatedOnRouterQueue: router}))
	a.now = func() time.Time { return propStart }
	var lines []string
	ctx := ctrl.LoggerInto(context.Background(),
		funcr.New(func(prefix, args string) { lines = append(lines, prefix+" "+args) }, funcr.Options{Verbosity: int(perReplicaLogLevel)}))
	states := []domain.VariantReplicaState{
		{VariantName: "decode", Role: domain.RoleDecode, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
		{VariantName: "prefill", Role: domain.RolePrefill, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
	}
	in := makeAnalyzerInput([]domain.ReplicaMetrics{dec, pre}, states)
	in.SchedulerQueue = q
	for _, m := range mut {
		m(in.Config.(*config.ScalingPolicy))
	}
	res, err := a.Analyze(ctx, in)
	require.NoError(t, err)
	for _, vc := range res.VariantCapacities {
		if vc.VariantName == "prefill" {
			return vc.K2Source, lines
		}
	}
	t.Fatal("no prefill variant in the result")
	return "", nil
}

// idleDecode is a decode row far from full, with no queue.
func idleDecode() domain.ReplicaMetrics {
	return makeReplicaMetrics("decode-0", "decode", 5000, 1_000_000, 0, 8000, 200)
}

// saturatedDecode is a decode row full and queued: prefill behind it reads
// decode's back-pressure.
func saturatedDecode() domain.ReplicaMetrics {
	return makeReplicaMetrics("decode-0", "decode", 950_000, 1_000_000, 20, 8000, 200)
}

// opened is a k2 source a saturated reading gave: recorded, or held as its
// window's first.
func opened(src string) bool {
	return src == capacity.K2SrcObserved.String() || src == capacity.K2SrcUnadmitted.String()
}

func hasGateLine(lines []string, reason string) bool {
	for _, l := range lines {
		if strings.Contains(l, `"prefill-saturated-without-queue"`) && strings.Contains(l, reason) {
			return true
		}
	}
	return false
}

// A prefill replica whose running prompts fill its stated budget, with no
// queue, reads saturated with the switch on and is priced at k1 with it off.
func TestPrefillSaturationGate_ABatchAtItsBudgetOpensTheGate(t *testing.T) {
	src, lines := gateRun(t, false, false, statedBudget(), runningPrefill(8, 8000), idleDecode(), nil)
	require.Equal(t, capacity.K2SrcFallback.String(), src, "off: priced at k1")
	require.False(t, hasGateLine(lines, prefillGateBatchBudget))

	src, lines = gateRun(t, true, false, statedBudget(), runningPrefill(8, 8000), idleDecode(), nil)
	require.True(t, opened(src), "on: a saturated reading, got %s", src)
	require.True(t, hasGateLine(lines, prefillGateBatchBudget))
}

// Under the budget, or with a budget the flags do not state, nothing changes.
func TestPrefillSaturationGate_BelowTheBudgetOrWithoutOneNothingChanges(t *testing.T) {
	src, _ := gateRun(t, true, false, statedBudget(), runningPrefill(4, 8000), idleDecode(), nil)
	require.Equal(t, capacity.K2SrcFallback.String(), src, "half the budget")
	unstated := &capacity.EngineParams{Engine: inferenceengine.EngineVLLM, EffectiveMaxBatchedTokens: 8192}
	src, lines := gateRun(t, true, false, unstated, runningPrefill(8, 8000), idleDecode(), nil)
	require.Equal(t, capacity.K2SrcFallback.String(), src, "a resolved default is not a stated budget")
	require.False(t, hasGateLine(lines, prefillGateBatchBudget))
}

// The refusals past the gate stand: behind a saturated decode a full prefill
// batch is decode's back-pressure, and is not learned as prefill's.
func TestPrefillSaturationGate_ASaturatedDecodeStillRefuses(t *testing.T) {
	src, lines := gateRun(t, true, true, statedBudget(), runningPrefill(8, 8000), saturatedDecode(),
		&domain.SchedulerQueueMetrics{QueueSize: 100})
	require.Equal(t, capacity.K2SrcFallback.String(), src, "decode saturated: not learned, priced at k1")
	require.True(t, hasGateLine(lines, prefillGateBatchBudget), "the gate opened; the refusal past it held")
}

// The router's backlog opens the gate at the threshold per Ready prefill
// replica with its switch on, and not under it, nor with flow control off.
func TestPrefillSaturationGate_TheRouterQueue(t *testing.T) {
	idle := runningPrefill(0, 8000)
	for _, c := range []struct {
		name   string
		on     bool
		q      *domain.SchedulerQueueMetrics
		opened bool
	}{
		{"off", false, &domain.SchedulerQueueMetrics{QueueSize: 100}, false},
		{"on, flow control off", true, nil, false},
		{"on, under the threshold", true, &domain.SchedulerQueueMetrics{QueueSize: 4}, false},
		{"on, at the threshold", true, &domain.SchedulerQueueMetrics{QueueSize: 5}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			src, lines := gateRun(t, false, c.on, statedBudget(), idle, idleDecode(), c.q)
			require.Equal(t, c.opened, opened(src), "got %s", src)
			require.Equal(t, c.opened, hasGateLine(lines, prefillGateRouterQueue))
		})
	}
}

// The prefill bound reads the same gate: with the bound on, a batch at its
// budget is a bound reading with the switch on, and is not with it off.
func TestPrefillSaturationGate_ThePrefillBoundReadsTheGate(t *testing.T) {
	boundOn := func(c *config.ScalingPolicy) { c.EnablePrefillThroughputBound = true }
	pre := runningPrefill(8, 8000)
	pre.PrefillComputedTokenRate, pre.PrefillComputedTokenRateKnown = 120000, true
	pre.RequestRate, pre.RequestRateKnown = 15, true
	reading := func(lines []string) bool {
		for _, l := range lines {
			if strings.Contains(l, `"prefill-throughput-bound-reading"`) {
				return true
			}
		}
		return false
	}
	_, lines := gateRun(t, false, false, statedBudget(), pre, idleDecode(), nil, boundOn)
	require.False(t, reading(lines), "off: no queue, no bound reading")
	_, lines = gateRun(t, true, false, statedBudget(), pre, idleDecode(), nil, boundOn)
	require.True(t, reading(lines), "on: the full batch is a bound reading")
}

// The settle test reads the same gate: a full batch whose queue-equivalent
// (the threshold's prompts) outweighs what the replica holds resident is a
// load step it was handed, not a limit, and is not learned.
func TestPrefillSaturationGate_TheSettleTestReadsTheGate(t *testing.T) {
	step := makeReplicaMetrics("prefill-0", "prefill", 30000, 1_000_000, 0, 8000, 1)
	step.RunningRequests, step.RunningRequestsKnown = 8, true
	src, lines := gateRun(t, true, false, statedBudget(), step, idleDecode(), nil)
	require.True(t, hasGateLine(lines, prefillGateBatchBudget), "the gate opened")
	require.Equal(t, capacity.K2SrcFallback.String(), src, "the settle test refused it: priced at k1")
}
