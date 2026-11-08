package throughput

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/internal/learnertest"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle/settlerow"
)

// The closed-loop probe (analyzer-learner.md, 2.5; PR 4b, "Tests") for the
// throughput analyzer's ITL line, on the fleet simulator the saturation
// analyzer's probe runs on (learnertest.Fleet). Each case pastes its result
// with the line on the tracker and on the parent's line (both switches off).
//
// The physics, per Ready replica of a decode variant: the true line is
// ITL(k) = probeA k + probeB, and a replica at KV fill k serves
// k C / KVreq / ITL(k) tokens a second. Offered r requests a second of
// probeOL output tokens each, it settles at the k that serves them, up to
// probeKMax; beyond that it serves its limit and queues. A replica's first
// probeWarmCycles cycles after it turns Ready read the warm-up ITL a real
// engine reports (0.45 s, as on the two-namespace run), at its k.
//
// The analyzer's order is its demand (the arrival rate times the mean
// output length) over its per-replica supply, rounded up; a cycle with no
// throughput capacity for the variant holds the fleet (learnertest.Hold), as
// a silent analyzer leaves the decision to the others. Need is that order
// priced on the true line: what a truthful line orders.
const (
	probeA, probeB  = 0.050, 0.010
	probeC          = 65536
	probeIL         = 1024
	probeOL         = 256
	probeKMax       = 0.95
	probeWarmITL    = 0.45
	probeWarmCycles = 4
	// probeMaxReplicas is the scale target's maxReplicas: where an order
	// that feeds itself stops.
	probeMaxReplicas = 64
)

func probeITL(k float64) float64 { return probeA*k + probeB }

// probeKVreq is the footprint the analyzer's shape gives the probe's rows,
// read off the analyzer itself so need is priced as the analyzer prices.
func probeKVreq(t *testing.T) float64 {
	t.Helper()
	a := NewThroughputAnalyzer().WithClock(clocktesting.NewFakePassiveClock(learnertest.Start))
	row := probeRow("r00", 0.40, probeITL(0.40), learnertest.Start.Add(-time.Hour), 1, 0)
	_, err := a.Analyze(context.Background(), domain.AnalyzerInput{ModelID: "m", Namespace: "ns",
		ReplicaMetrics: []domain.ReplicaMetrics{row},
		VariantStates:  []domain.VariantReplicaState{{VariantName: "v1", CurrentReplicas: 1, GPUsPerReplica: 1}}})
	require.NoError(t, err)
	st, ok := a.VariantState("m", "ns", "v1")
	require.True(t, ok)
	require.Positive(t, st.Shape.KVreq)
	return st.Shape.KVreq
}

// probeTokens is what a replica at fill k serves, in tokens a second.
func probeTokens(k, kvReq float64) float64 { return k * probeC / kvReq / probeITL(k) }

// probeRow is one Ready decode replica at fill k reading itlSec, serving
// served requests a second with queue waiting.
func probeRow(pod string, k, itlSec float64, readySince time.Time, served float64, queue int) domain.ReplicaMetrics {
	return domain.ReplicaMetrics{
		PodName: pod, VariantName: "v1", Ready: true, ReadySince: readySince,
		KvCacheUsage: k, KvUsageInstant: k, TotalKvCapacityTokens: probeC,
		AvgInputTokens: probeIL, AvgOutputTokens: probeOL, AvgITL: itlSec, AvgITLKnown: itlSec > 0,
		RequestRate: served, GenerationTokenRate: served * probeOL, QueueLength: queue,
	}
}

// probeCase is one closed-loop scenario of the throughput line.
type probeCase struct {
	// start is the fleet's size at the start.
	start   int
	lambda  func(elapsed time.Duration) float64
	horizon time.Duration
	// restartAt, when set, restarts the controller then: a new analyzer
	// with nothing learned.
	restartAt time.Duration
	// engine runs the analyzer as the engine does, with a settle tracker
	// observed before every cycle and new with each controller
	// (WithSettleView); unset, the analyzer observes its own.
	engine bool
	// noise is the ITL gauge's relative noise.
	noise float64
}

// probeResult is what the loop did, and the line's reasons over it.
type probeResult struct {
	learnertest.Result
	reasons map[string]int
	// silentAfterRestart is how many cycles from the restart on the line
	// was silent (itlReasonT2Failed).
	silentAfterRestart int
}

// runProbe drives pc with the analyzer newA builds (again at a restart).
func runProbe(t *testing.T, pc probeCase, newA func() *ThroughputAnalyzer) probeResult {
	t.Helper()
	kvReq := probeKVreq(t)
	perReplica := probeTokens(itl.DefaultKSat, kvReq) // tokens/s at the pricing fill
	mu := probeTokens(probeKMax, kvReq) / probeOL     // requests/s at the limit
	clock := clocktesting.NewFakePassiveClock(learnertest.Start)
	var tracker *settle.Tracker
	build := func() *ThroughputAnalyzer {
		a := newA().WithClock(clock).WithSlopeHoldFor(time.Hour)
		if pc.engine {
			tracker = settle.NewTracker()
			tracker.SetInterval(learnertest.Cycle)
			a = a.WithSettleView(tracker).WithOptimizationInterval(learnertest.Cycle)
		}
		return a
	}
	a := build()
	pods := make([]float64, pc.start)
	for i := range pods {
		pods[i] = mu
	}
	noise := learnertest.ARNoise(learnertest.Rand(1), 1<<14, pc.noise, 0.5)
	n := 0
	res := probeResult{reasons: map[string]int{}}
	restarted := false
	fleet := learnertest.Fleet{
		Pods: pods, AddedMu: mu, Lambda: pc.lambda, Horizon: pc.horizon,
		Need:        func(lambda float64) int { return learnertest.Ceil(lambda * probeOL / perReplica) },
		MaxReplicas: probeMaxReplicas,
	}
	res.Result = fleet.Run(func(now time.Time, elapsed time.Duration, lambda float64, ready []learnertest.Replica) int {
		if pc.restartAt > 0 && !restarted && elapsed >= pc.restartAt {
			restarted, a = true, build()
		}
		clock.SetTime(now)
		rows := make([]domain.ReplicaMetrics, 0, len(ready))
		for _, r := range ready {
			k := probeKMax
			if !r.Saturated {
				tokens := r.Served * probeOL
				k = tokens * probeB / (probeC/kvReq - tokens*probeA)
			}
			lat := probeITL(k) * (1 + noise[n%len(noise)])
			n++
			if now.Sub(r.ReadyAt) < probeWarmCycles*learnertest.Cycle && r.ReadyAt.After(learnertest.Start) {
				lat = probeWarmITL
			}
			rows = append(rows, probeRow(r.Name, k, lat, r.ReadyAt, r.Served, int(math.Round(r.Backlog))))
		}
		if tracker != nil {
			tracker.Observe("ns/m", settlerow.Of(rows, nil), now)
		}
		out, err := a.Analyze(context.Background(), domain.AnalyzerInput{ModelID: "m", Namespace: "ns",
			ReplicaMetrics: rows, ArrivalRate: lambda,
			VariantStates: []domain.VariantReplicaState{{VariantName: "v1", CurrentReplicas: len(rows), GPUsPerReplica: 1}}})
		require.NoError(t, err)
		for _, vc := range out.VariantCapacities {
			if vc.VariantName != "v1" || vc.PerReplicaCapacity <= 0 {
				continue
			}
			res.reasons[vc.Reason]++
			return learnertest.Ceil(out.TotalDemand / vc.PerReplicaCapacity)
		}
		res.reasons[itlReasonT2Failed]++
		if restarted {
			res.silentAfterRestart++
		}
		return learnertest.Hold
	})
	return res
}

func (r probeResult) String() string {
	return fmt.Sprintf("need %d, final %d, peak %d, under need for %s; reasons %v; silent %d cycles after the restart; verdict %q",
		r.Need, r.Final, r.Peak, r.UnderFor, r.reasons, r.silentAfterRestart, r.Verdict())
}

// stepAt is an arrival rate of from until at, then to.
func stepAt(from, to float64, at time.Duration) func(time.Duration) float64 {
	return func(e time.Duration) float64 {
		if e < at {
			return from
		}
		return to
	}
}

// The slope hold: a line learned at load, 35 min of light load below the
// band (no reading enters the window, and its readings age out at 30 min),
// then a step back to need. On the tracker the line holds its A (T2-held)
// through the light load and prices the step on its first cycle. The
// parent's line has no hold: below the band it fits A from k near zero on
// every cycle, whose A is the noise over a small k.
func TestThroughputProbe_TheSlopeHoldCarriesALineThroughLightLoad(t *testing.T) {
	pc := probeCase{start: 3, horizon: 60 * time.Minute, noise: 0.03,
		lambda: func(e time.Duration) float64 {
			switch {
			case e < 10*time.Minute:
				return 10
			case e < 45*time.Minute:
				return 0.5
			default:
				return 10
			}
		}}
	on := runProbe(t, pc, NewThroughputAnalyzer)
	t.Logf("on the tracker: %s", on)
	parent := runProbe(t, pc, newParentLineAnalyzer)
	t.Logf("the parent's line: %s", parent)
	require.Empty(t, on.Verdict())
	require.Positive(t, on.reasons[itlReasonT2Held], "premise: the light load held the slope")
	require.Zero(t, parent.reasons[itlReasonT2Held], "the parent has no held slope")
}

// A scale-up whose joiners carry warm-up ITL: two replicas at k near 0.4
// under 6 requests a second, then a step to 20. On the tracker the joiners'
// warm-up readings are stamped settling and kept out of the line, and the
// fleet settles at need with a peak no more than a replica over it. The
// parent's pinned fit takes them: A steepens, supply falls, every joiner it
// orders lowers its supply again, and it orders more than the line on the
// tracker does at the peak.
func TestThroughputProbe_AScaleUpsWarmUpDoesNotInflateTheOrder(t *testing.T) {
	pc := probeCase{start: 2, horizon: 40 * time.Minute, noise: 0.02, lambda: stepAt(6, 20, 5*time.Minute)}
	on := runProbe(t, pc, NewThroughputAnalyzer)
	t.Logf("on the tracker: %s", on)
	parent := runProbe(t, pc, newParentLineAnalyzer)
	t.Logf("the parent's line: %s", parent)
	require.Empty(t, on.Verdict())
	require.LessOrEqual(t, on.Peak, on.Need+1, "the joiners' warm-up inflated the order on the tracker: %v", on.Trace)
	require.Greater(t, parent.Peak, on.Peak, "negative control: the parent's line took the warm-up and ordered more")
}

// restartCase is four replicas at k near 0.4 under 8 requests a second; the
// controller restarts at 15 min, and the load steps to 30 at stepAfter past
// the restart.
func restartCase(stepAfter time.Duration) probeCase {
	return probeCase{start: 4, horizon: 40 * time.Minute, noise: 0.02, restartAt: 15 * time.Minute,
		lambda: stepAt(12, 30, 15*time.Minute+stepAfter)}
}

// A controller restart 2 min before a load step: the restarted analyzer has
// learned nothing, and its line needs itl.DefaultMinSamples readings, which
// four replicas in the band give it within the minute. The fleet is never
// under need for longer than a start and a settle take, and settles at need.
// The parent's line prices from the restart's first cycle, takes the
// joiners' warm-up, and runs to the replica cap.
func TestThroughputProbe_AControllerRestartBeforeALoadStepSettlesAtNeed(t *testing.T) {
	pc := restartCase(2 * time.Minute)
	on := runProbe(t, pc, NewThroughputAnalyzer)
	t.Logf("on the tracker: %s", on)
	parent := runProbe(t, pc, newParentLineAnalyzer)
	t.Logf("the parent's line: %s", parent)
	require.Empty(t, on.Verdict())
	require.Positive(t, on.reasons[itlReasonT2Failed], "premise: the restarted line was silent for a while")
}

// The same restart 15 s before the step, as the plan states the case: the
// step lands before the restarted line has its ten readings, and the step
// pushes every replica above the band, where the band admits no reading
// (the probe's fleet states no sequence cap, so none is read as at one,
// SwitchITLAdmitsAtSequenceCap). The restarted line has never fitted, so it fits from the
// readings above the band (T2-above-band) until the scale-up it orders
// brings the fleet back into the band, and the fleet is under need for no
// longer than a start and a settle take.
//
// Before the fallback, measured on this case: need 9, final 4, peak 4, under
// need for 24m45s, reasons T2-default 58 (before the restart) and T2-failed
// 102 -- the restarted line was silent for the rest of the run, and alone
// the analyzer never ordered the scale-up. With it: need 9, final 10, peak
// 10 (inside the over-order bound), under need for 30s, 7 cycles priced from
// above the band. Negative control: with the fallback off the case fails as
// it did.
func TestThroughputProbe_ARestartJustBeforeAStepAboveTheBandSettlesAtNeed(t *testing.T) {
	on := runProbe(t, restartCase(15*time.Second), NewThroughputAnalyzer)
	t.Logf("on the tracker: %s", on)
	require.Empty(t, on.Verdict())
	require.Positive(t, on.reasons[itlReasonT2AboveBand], "premise: the restarted line priced from above the band")
	require.LessOrEqual(t, on.UnderFor, learnertest.UnderFor)

	off := runProbe(t, restartCase(15*time.Second), func() *ThroughputAnalyzer {
		return NewThroughputAnalyzer().withoutAboveBandFallback()
	})
	t.Logf("negative control, no above-band fallback: %s", off)
	require.Contains(t, off.Verdict(), "under-ordered", "negative control: without the fallback the restarted line stays silent")
	require.Zero(t, off.reasons[itlReasonT2AboveBand])

	parent := runProbe(t, restartCase(15*time.Second), newParentLineAnalyzer)
	t.Logf("the parent's line: %s", parent)
}

// One replica at its sequence cap with its siblings below the band: the
// only reading in reach is above the band, from a replica at its stated
// cap. With the cap admission (learner PR 4b-3,
// SwitchITLAdmitsAtSequenceCap) its readings are the band's, and the band's
// line prices the variant from the tenth (T2-default), never the
// above-band fallback. It stays synthetic because no recording has this
// fleet: throughput-sweep-restart holds its one decode pod at the cap, so it
// has no sibling below the band, and that pod's line had full fits before
// the load reached the cap, so its first line never comes from capped
// readings. With no learned B the line is priced one-sided
// (itl.Admission.OneSidedAboveKSat): at or above the truth at k 0.85. On
// this case's one fill and no noise the line through the least ITL is flat
// at the capped replica's ITL, 0.0560 s, where the fit through the default
// B priced 0.0522 s, under the truth's 0.0525 s.
//
// Before 4b-3 (and with the switch off, the negative control): the band's
// window held 0 readings, the line was silent 9 cycles and the fallback
// priced the other 31 (T2-above-band). The parent's line priced every cycle
// from the below-band rows' tier-2 fit (T2-default 40), which reads the
// rows at k 0.08 that the band refuses.
func TestThroughputProbe_AReplicaAtItsCapWithSiblingsBelowTheBandPricesFromTheBand(t *testing.T) {
	type outcome struct {
		reasons map[string]int
		inBand  int
		priced  float64
		logs    []string
	}
	run := func(a *ThroughputAnalyzer) outcome {
		store := capacity.NewStore()
		store.Update("ns", "m", "v1", capacity.Record{EngineParams: &capacity.EngineParams{Engine: "vllm", DeclaredBatchCap: 64}},
			lineStart)
		r := newLineRun(t, a.WithCapacityStore(store))
		ctx, buf := logContext()
		r.ctx = ctx
		early := lineStart.Add(-time.Hour)
		o := outcome{reasons: map[string]int{}}
		for range 40 {
			capped := probeRow("r00", 0.92, probeITL(0.92), early, 3, 6)
			capped.RunningRequests, capped.PreemptionRate, capped.PreemptionRateKnown = 64, 0, true
			rows := []domain.ReplicaMetrics{capped,
				probeRow("r01", 0.08, probeITL(0.08), early, 0.3, 0), probeRow("r02", 0.08, probeITL(0.08), early, 0.3, 0)}
			vc, has := r.step(rows)
			if has {
				o.reasons[vc.Reason]++
			} else {
				o.reasons[itlReasonT2Failed]++
			}
			st, _ := a.VariantState("m", "ns", "v1")
			o.inBand = max(o.inBand, st.SampleCount)
			o.priced = st.ITLModel.ITLAt(itl.DefaultKSat)
		}
		o.logs = lineLogs(buf.String())
		t.Logf("summary lines: %v", o.logs)
		return o
	}
	truth := probeITL(itl.DefaultKSat)
	on := run(NewThroughputAnalyzer())
	t.Logf("on: %v, the band's window up to %d readings, priced %.4f s at kSat against the truth %.4f s",
		on.reasons, on.inBand, on.priced, truth)
	off := run(withSwitchOff(SwitchITLAdmitsAtSequenceCap)())
	t.Logf("negative control, the cap admission off: %v, the band's window up to %d readings, priced %.4f s",
		off.reasons, off.inBand, off.priced)
	parent := run(newParentLineAnalyzer())
	t.Logf("the parent's line: %v, priced %.4f s", parent.reasons, parent.priced)

	require.Equal(t, 20, on.inBand, "the capped replica's readings fill the band's window")
	require.Len(t, on.logs, 2, "silent, then live")
	require.Contains(t, on.logs[1], `"pinnedAtLeastITL": true`, "the summary does not say the line was priced one-sided")
	require.Equal(t, map[string]int{itlReasonT2Failed: 9, itlReasonT2Default: 31}, on.reasons,
		"silent until the capped replica's tenth reading, then the band's line")
	require.GreaterOrEqual(t, on.priced, truth, "the band's line priced under the truth with no learned B")
	pinned := run(NewThroughputAnalyzer().withCapPinnedAtDefaultB())
	t.Logf("negative control, pinned at the default B only: priced %.4f s", pinned.priced)
	require.Less(t, pinned.priced, truth, "negative control: the default B alone prices under the truth")
	require.InEpsilon(t, truth, on.priced, 0.10, "the band's line prices the capped replica's line")

	require.Zero(t, off.inBand, "negative control: off, the band's window holds nothing")
	require.Equal(t, map[string]int{itlReasonT2Failed: 9, itlReasonT2AboveBand: 31}, off.reasons,
		"negative control: off, the fallback prices the variant")
	require.Positive(t, parent.reasons[itlReasonT2Default], "the parent prices from the below-band rows' tier-2 fit")
}

// A controller restart on the engine's settle tracker (learner PR 4b-2,
// SwitchITLWaitsAfterRestart): the restarted engine's tracker sees every
// replica first on its first cycle, and the line waits the settle from it,
// so the band's line waits steadyFor and its ten readings after the
// restart. The above-band fallback does not wait (itl's aboveBandSettling):
// it is one-sided, and a warm-up reading can only raise its price. Run 2 min
// and 15 s before a load step, with the wait and without it (the negative
// control: the line 4b-1 built).
//
// Measured: a step 2 min after the restart, need 9, final 10, peak 10,
// under need for 30s, silent 10 cycles (2.5 min) from the restart on,
// against final 9, 0s and 2 cycles taken as settled: the step lands after
// the wait, in the band, and the wait is the SLO side (4.4), inside
// learnertest.UnderFor. A step 15 s after it pushes every replica above the
// band inside the wait: final 10, peak 10, under need for 30s, silent 3
// cycles (45 s), the same as taken as settled. With the restart wait
// stamped on the above-band window too, as the PR first built it, that
// case was under need for 1m45s and silent 8 cycles (2 min).
func TestThroughputProbe_AControllerRestartOnTheEngineTrackerWaitsItsSettle(t *testing.T) {
	run := func(stepAfter time.Duration) (on, off probeResult) {
		pc := restartCase(stepAfter)
		pc.engine = true
		on = runProbe(t, pc, NewThroughputAnalyzer)
		t.Logf("step %s after the restart, waits: %s", stepAfter, on)
		off = runProbe(t, pc, withSwitchOff(SwitchITLWaitsAfterRestart))
		t.Logf("step %s after the restart, taken as settled: %s", stepAfter, off)
		require.Empty(t, on.Verdict(), "step %s after the restart", stepAfter)
		require.LessOrEqual(t, on.UnderFor, learnertest.UnderFor)
		return on, off
	}

	// The figures the reference pages quote (configuration.md, cycle-log.md,
	// troubleshooting.md), pinned: 4 replicas, 15 s cycles, steadyFor 2 min.
	on, off := run(2 * time.Minute)
	require.GreaterOrEqual(t, on.reasons[itlReasonT2Failed], off.reasons[itlReasonT2Failed]+int(itl.DefaultSteadyFor/learnertest.Cycle),
		"the band's line did not wait its settle")
	require.Greater(t, on.UnderFor, off.UnderFor, "the wait cost nothing")
	require.Equal(t, 30*time.Second, on.UnderFor, "in the band: under need")
	require.Equal(t, 10, on.silentAfterRestart, "in the band: silent cycles after the restart")

	on, off = run(15 * time.Second)
	require.Greater(t, on.reasons[itlReasonT2Failed], off.reasons[itlReasonT2Failed], "the band's line did not wait its settle")
	require.Positive(t, on.reasons[itlReasonT2AboveBand], "premise: the restarted line priced from above the band")
	require.LessOrEqual(t, on.UnderFor, off.UnderFor, "the above-band fallback waited the restart")
	require.LessOrEqual(t, on.silentAfterRestart, off.silentAfterRestart, "the above-band fallback waited the restart")
	require.Equal(t, 30*time.Second, on.UnderFor, "above the band: under need")
	require.Equal(t, 3, on.silentAfterRestart, "above the band: silent cycles after the restart")

	// Negative control: the restart's wait stamped on the above-band
	// readings too, as the PR first built it.
	pc := restartCase(15 * time.Second)
	pc.engine = true
	stamped := runProbe(t, pc, func() *ThroughputAnalyzer { return NewThroughputAnalyzer().withAboveBandRestartWait() })
	t.Logf("step 15s after the restart, the wait stamped above the band: %s", stamped)
	require.Equal(t, 105*time.Second, stamped.UnderFor, "negative control: under need")
	require.Equal(t, 8, stamped.silentAfterRestart, "negative control: silent cycles after the restart")
}
