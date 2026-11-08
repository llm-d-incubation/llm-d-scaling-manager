package throughput

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// lineAt is the ITL a replica reads at k on the line these tests price:
// 6 ms plus 50 ms per unit of k.
func lineAt(k float64) float64 { return 0.006 + 0.05*k }

// lineRow is a Ready replica of variant v1 at k, reading itlSec, Ready since
// readySince. Its ITL is Finite by its presence bits, as the collector
// reports a read mean (SwitchITLReadsFinitePodMean).
func lineRow(pod string, k, itlSec float64, readySince time.Time) domain.ReplicaMetrics {
	return domain.ReplicaMetrics{
		PodName: pod, VariantName: "v1", Ready: true, ReadySince: readySince,
		KvCacheUsage: k, KvUsageInstant: k, TotalKvCapacityTokens: 65536,
		AvgInputTokens: 1024, AvgOutputTokens: 256, AvgITL: itlSec, AvgITLKnown: itlSec > 0, RequestRate: 2,
	}
}

// lineRun drives an analyzer cycle by cycle, 15 s apart, from lineStart.
type lineRun struct {
	t     *testing.T
	a     *ThroughputAnalyzer
	clock *clocktesting.FakePassiveClock
	cycle int
	// ctx is what each cycle is analyzed under; nil for the background.
	ctx context.Context
}

var lineStart = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

func newLineRun(t *testing.T, a *ThroughputAnalyzer) *lineRun {
	c := clocktesting.NewFakePassiveClock(lineStart)
	return &lineRun{t: t, a: a.WithClock(c), clock: c}
}

// now is the instant of the run's next cycle.
func (r *lineRun) now() time.Time { return lineStart.Add(time.Duration(r.cycle) * 15 * time.Second) }

// step analyzes one cycle of rows and returns v1's capacity, if it has one.
func (r *lineRun) step(rows []domain.ReplicaMetrics) (domain.VariantCapacity, bool) {
	r.t.Helper()
	r.clock.SetTime(r.now())
	ctx := r.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	res, err := r.a.Analyze(ctx, domain.AnalyzerInput{ModelID: "m", Namespace: "ns",
		ReplicaMetrics: rows, ArrivalRate: 4,
		VariantStates: []domain.VariantReplicaState{{VariantName: "v1", CurrentReplicas: len(rows), GPUsPerReplica: 1}}})
	require.NoError(r.t, err)
	r.cycle++
	for _, vc := range res.VariantCapacities {
		if vc.VariantName == "v1" {
			return vc, true
		}
	}
	return domain.VariantCapacity{}, false
}

// slope is v1's last line's A.
func (r *lineRun) slope() float64 {
	st, _ := r.a.VariantState("m", "ns", "v1")
	return st.ITLModel.A
}

// steadyFleet is three replicas Ready since an hour before the run, at k 0.40 on
// the line.
func steadyFleet() []domain.ReplicaMetrics {
	early := lineStart.Add(-time.Hour)
	return []domain.ReplicaMetrics{
		lineRow("pod-a", 0.40, lineAt(0.40), early),
		lineRow("pod-b", 0.40, lineAt(0.40), early),
		lineRow("pod-c", 0.40, lineAt(0.40), early),
	}
}

// A joiner's first readings carry its warm-up: on a two-namespace run, about
// 0.46 of k at 435 to 496 ms against a line near 0.09. On the tracker they
// are stamped settling and kept out of the pinned fit, so A stays at the
// fleet's; the parent's pinned fit took the cycle's rows whole, and the one
// warm-up reading set A.
func TestITLLine_AJoinersWarmUpDoesNotMoveA(t *testing.T) {
	run := func(a *ThroughputAnalyzer) (before, during float64) {
		r := newLineRun(t, a)
		for range 12 {
			r.step(steadyFleet())
		}
		before = r.slope()
		joined := r.now()
		for range 4 {
			r.step(append(steadyFleet(), lineRow("pod-join", 0.46, 0.45, joined)))
			during = math.Max(during, r.slope())
		}
		return before, during
	}
	before, during := run(NewThroughputAnalyzer())
	require.InDelta(t, 0.05, before, 0.005, "the fleet's line before the joiner")
	require.InDelta(t, before, during, 0.05*before, "a joiner's warm-up moved A on the tracker")

	pBefore, pDuring := run(newParentLineAnalyzer())
	require.Greater(t, pDuring, 2*pBefore, "negative control: the parent's pinned fit took the warm-up "+
		"(before %g, during %g)", pBefore, pDuring)
}

// One reading of one replica on one cycle is no line on the tracker: the
// window needs itl.DefaultMinSamples readings. The parent priced from it.
func TestITLLine_OneReadingIsNoLine(t *testing.T) {
	one := []domain.ReplicaMetrics{lineRow("pod-a", 0.40, lineAt(0.40), lineStart.Add(-time.Hour))}
	_, has := newLineRun(t, NewThroughputAnalyzer()).step(one)
	require.False(t, has, "a line from one reading")
	vc, has := newLineRun(t, newParentLineAnalyzer()).step(one)
	require.True(t, has, "negative control: the parent priced from one reading")
	require.Equal(t, itlReasonT2Default, vc.Reason)
}

// An ITL of +Inf -- a rate over a window with no tokens in it -- is no
// reading on the tracker, and the line holds through it. The parent took it,
// and had no line on that cycle.
func TestITLLine_AnInfiniteITLIsNoReading(t *testing.T) {
	run := func(a *ThroughputAnalyzer) bool {
		r := newLineRun(t, a)
		for range 12 {
			r.step(steadyFleet())
		}
		rows := steadyFleet()
		rows[0].AvgITL = math.Inf(1)
		_, has := r.step(rows)
		return has
	}
	require.True(t, run(NewThroughputAnalyzer()), "the line was lost to an infinite ITL")
	require.False(t, run(newParentLineAnalyzer()), "negative control: the parent kept a line through an infinite ITL")
}

// A full fit whose intercept is negative is a line, but its B is not
// remembered on the tracker: a negative floor pinned into later fits is
// worse than the default. The parent remembered it.
func TestITLLine_ANegativeInterceptIsNotRemembered(t *testing.T) {
	early := lineStart.Add(-time.Hour)
	spread := func() []domain.ReplicaMetrics {
		rows := make([]domain.ReplicaMetrics, 0, 5)
		for i, k := range []float64{0.20, 0.35, 0.50, 0.65, 0.80} {
			rows = append(rows, lineRow("pod-"+string(rune('a'+i)), k, 0.1*k-0.01, early))
		}
		return rows
	}
	run := func(a *ThroughputAnalyzer) ThroughputVariantState {
		r := newLineRun(t, a)
		for range 16 {
			r.step(spread())
		}
		st, _ := a.VariantState("m", "ns", "v1")
		return st
	}
	on := run(NewThroughputAnalyzer())
	require.Equal(t, 0.1, math.Round(on.ITLModel.A*1000)/1000, "the run must reach the full fit")
	require.False(t, on.HasFittedB, "a negative B was remembered: %g", on.LastFittedB)
	parent := run(newParentLineAnalyzer())
	require.True(t, parent.HasFittedB && parent.LastFittedB < 0,
		"negative control: the parent remembered B %g (%v)", parent.LastFittedB, parent.HasFittedB)
}

// A line fitted at load holds its A once the fleet drops below the band and
// its readings age out (T2-held), for the slope hold and no longer. The hold
// counts from the last fit: the pinned fit keeps refitting from the window's
// old readings until they age out (30 min), and each refit is a fit.
func TestITLLine_TheSlopeHoldsBelowTheBandForTheHold(t *testing.T) {
	r := newLineRun(t, NewThroughputAnalyzer().WithSlopeHoldFor(time.Hour))
	for range 12 {
		r.step(steadyFleet())
	}
	fitted := r.slope()
	require.InDelta(t, 0.05, fitted, 0.005)
	idle := func() []domain.ReplicaMetrics {
		rows := steadyFleet()
		for i := range rows {
			rows[i].KvCacheUsage, rows[i].KvUsageInstant, rows[i].AvgITL = 0.05, 0.05, lineAt(0.05)
		}
		return rows
	}
	reasons := map[string]int{}
	var lastFit, lastHeld time.Time
	for r.now().Before(lineStart.Add(150 * time.Minute)) {
		at := r.now()
		vc, has := r.step(idle())
		if !has {
			reasons[itlReasonT2Failed]++
			continue
		}
		reasons[vc.Reason]++
		switch vc.Reason {
		case itlReasonT2Held:
			lastHeld = at
			require.InDelta(t, fitted, r.slope(), 1e-12, "a held slope moved")
		default:
			lastFit = at
		}
	}
	require.Positive(t, reasons[itlReasonT2Held], "the slope was never held: %v", reasons)
	require.Positive(t, reasons[itlReasonT2Failed], "the slope was held past its hold: %v", reasons)
	require.True(t, lastFit.Before(lastHeld), "held before the last fit: %v", reasons)
	held := lastHeld.Sub(lastFit)
	require.LessOrEqual(t, held, time.Hour, "held %v past the last fit, over the 1 h hold", held)
	require.Greater(t, held, time.Hour-time.Minute, "held only %v past the last fit", held)
}

// With the GPS-mismatch clear dropped (SwitchITLKeepsWindowOnGPSMismatch),
// a correct line whose generated-token rate disagrees keeps its window.
// Negative control: with the switch off, three such cycles empty it.
func TestITLLine_AGPSMismatchKeepsTheWindow(t *testing.T) {
	early := lineStart.Add(-time.Hour)
	rows := func() []domain.ReplicaMetrics {
		out := make([]domain.ReplicaMetrics, 0, 3)
		for i, k := range []float64{0.40, 0.50, 0.60} {
			row := lineRow("pod-"+string(rune('a'+i)), k, lineAt(k), early)
			row.GenerationTokenRate = 1e6 // far off any rate the line predicts
			out = append(out, row)
		}
		return out
	}
	minWindow := func(keeps bool) int {
		a := NewThroughputAnalyzer()
		a.itlKeepsOnGPSMismatch = keeps
		r := newLineRun(t, a)
		least, filled := math.MaxInt, false
		for range 20 {
			r.step(rows())
			n := a.lines.WindowLen(variantKey("ns", "m", "v1"))
			if n >= 10 {
				filled = true
			}
			if filled {
				least = min(least, n)
			}
		}
		require.True(t, filled, "the window never filled: the case tests nothing")
		return least
	}
	require.GreaterOrEqual(t, minWindow(true), 10, "a GPS mismatch emptied the window")
	require.Zero(t, minWindow(false), "negative control: with the clear, three mismatches empty the window")
}

// The switches are what SetSwitchesForReplay accepts, by name.
func TestITLLine_TheReplaySwitches(t *testing.T) {
	require.ElementsMatch(t, append([]string{SwitchITLKeepsWindowOnGPSMismatch, SwitchITLSteadyGate, SwitchITLAdmitsAtSequenceCap,
		SwitchITLForgetReachesTheLine, SwitchITLRolloutGuard, SwitchITLForgetsOnNewArguments, SwitchITLFilesReports,
		SwitchITLKeepsPresentVariants, SwitchITLTierHysteresis, SwitchKVPerRequestOutputDispersion,
		SwitchITLOwnSettleBackdates}, settleSwitches...),
		ReplaySwitchNames())
	a := NewThroughputAnalyzer()
	for name, on := range a.switchFields() {
		require.True(t, *on, "%s is off by the constructor", name)
	}
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchITLSteadyGate: false}))
	require.False(t, a.itlSteadyGate)
	require.True(t, a.itlKeepsOnGPSMismatch)
	require.Error(t, a.SetSwitchesForReplay(map[string]bool{"no-such-switch": true}))
}
