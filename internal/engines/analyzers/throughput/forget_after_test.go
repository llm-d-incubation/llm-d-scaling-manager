package throughput

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// What a line does after a forget, on the figures the reference pages quote
// (docs/reference/troubleshooting.md).

// balancedFleet is n replicas of v1 at one k, reading the line at k, Ready
// since an hour before the run.
func balancedFleet(n int, k float64, line func(float64) float64) []domain.ReplicaMetrics {
	early := lineStart.Add(-time.Hour)
	rows := make([]domain.ReplicaMetrics, 0, n)
	for i := range n {
		rows = append(rows, lineRow(fmt.Sprintf("pod-%d", i), k, line(k), early))
	}
	return rows
}

// fittedFleet is settleFleet under balancedFleet's Pod names: the same
// replicas go on after the forget, as they do in a running fleet. A name the
// key's kept traces do not know is a joiner, and settles.
func fittedFleet() []domain.ReplicaMetrics {
	rows := settleFleet()
	for i := range rows {
		rows[i].PodName = fmt.Sprintf("pod-%d", i)
	}
	return rows
}

// Ten readings are necessary for a line, and not a recovery time. A fleet
// that offers none stays silent for as long as it does: at k 0.82, above the
// band and under kSat, with no stated cap, and at k 0.10, below the band.
// Off, the held slope prices every cycle: the forget took what priced them.
func TestForget_AFleetOfferingNoReadingStaysSilent(t *testing.T) {
	const cycles = 40
	for _, tc := range []struct {
		k     float64
		cause string
	}{
		{0.82, silenceAboveBandUnderKSat},
		{0.10, silenceTooFewReadings},
	} {
		for _, on := range []bool{true, false} {
			a := NewThroughputAnalyzer()
			r := forgetRun(t, a, on)
			fitLine(r, fittedFleet)
			forgetV1(a)
			priced := 0
			for range cycles {
				if _, ok := r.step(balancedFleet(3, tc.k, lineAt)); ok {
					priced++
				}
			}
			if !on {
				assert.Equal(t, cycles, priced, "k %.2f, switch off: the held slope prices", tc.k)
				continue
			}
			assert.Zero(t, priced, "k %.2f: no reading, no line", tc.k)
			a.mu.Lock()
			st := a.variantStates[variantKey("ns", "m", "v1")]
			cause := silenceCause(st, st.reading)
			a.mu.Unlock()
			assert.Equal(t, tc.cause, cause)
		}
	}
}

// When readings do flow, a line is back on the cycle the window holds
// itl.DefaultMinSamples: the fourth with three replicas, the tenth with one.
func TestForget_TheLineIsBackOnTheCycleItHoldsTenReadings(t *testing.T) {
	for replicas, want := range map[int]int{3: 4, 1: 10} {
		a := NewThroughputAnalyzer()
		r := forgetRun(t, a, true)
		fitLine(r, fittedFleet)
		forgetV1(a)
		back := 0
		for c := 1; c <= 12 && back == 0; c++ {
			if vc, ok := r.step(balancedFleet(replicas, 0.45, lineAt)); ok {
				back = c
				assert.Equal(t, itlReasonT2Default, vc.Reason, "B is gone: the default pin")
			}
		}
		assert.Equal(t, want, back, "%d replicas", replicas)
	}
}

// Past a forget a balanced fleet pins itl.DefaultBaselineSec, as a new
// variant does, and in the band that is not one-sided: capacity, the inverse
// of ITL(kSat), is over the truth when the true B is under 6 ms and under it
// when above, by more the further k is from kSat. Each case is capacity over
// the truth, on a true slope of 50 ms per unit of k.
func TestForget_ABalancedFleetPricesEitherSideOfTheTruth(t *testing.T) {
	for _, tc := range []struct{ b, k, want float64 }{
		{0.002, 0.30, 1.20}, {0.002, 0.50, 1.07}, {0.002, 0.70, 1.02},
		{0.020, 0.30, 0.71}, {0.020, 0.50, 0.86}, {0.020, 0.70, 0.95},
	} {
		line := func(k float64) float64 { return tc.b + 0.05*k }
		a := NewThroughputAnalyzer()
		r := forgetRun(t, a, true)
		fitLine(r, func() []domain.ReplicaMetrics {
			rows := fittedFleet()
			for i := range rows {
				rows[i].AvgITL = line(rows[i].KvCacheUsage)
			}
			return rows
		})
		forgetV1(a)
		var vc domain.VariantCapacity
		var ok bool
		for range 4 {
			vc, ok = r.step(balancedFleet(3, tc.k, line))
		}
		require.True(t, ok, "B %.3f k %.2f: no line", tc.b, tc.k)
		require.Equal(t, itlReasonT2Default, vc.Reason)
		st, _ := a.VariantState("m", "ns", "v1")
		got := line(itl.DefaultKSat) / st.ITLModel.ITLAt(itl.DefaultKSat)
		assert.InDelta(t, tc.want, got, 0.005, "B %.3f k %.2f", tc.b, tc.k)
	}
}

// A forget keeps the replicas' traces: they are what the steady gate judges
// a reading against, the replica's load now, and no line is fitted from them.
// So the first cycle after a forget still sees a step: k up 0.25 and ITL
// tripled is unsteady, on and off. Dropped, the gate read it as steady, and a
// full fit could have taken it.
func TestForget_TheSteadyGateSeesTheFirstCycleAfter(t *testing.T) {
	for _, on := range []bool{true, false} {
		a := NewThroughputAnalyzer()
		r := forgetRun(t, a, on)
		for range 12 {
			r.step(balancedFleet(3, 0.30, lineAt))
		}
		forgetV1(a)
		r.step(balancedFleet(3, 0.55, func(float64) float64 { return 3 * lineAt(0.30) }))
		read, _, _ := lineRead(a)
		assert.Equal(t, 3, read.Unsteady, "switch %v: the step is read", on)
		if on {
			assert.Equal(t, 3, read.Held, "and the window holds this cycle's readings alone")
		}
	}
}

// TestForget_AReplicaSettlingAtAForgetStillSettles with no engine view, on
// the analyzer's own settle tracker and on the key's own trace: the kept
// trace holds the settle on each. Made anew, the key would take the joiner
// as already settled.
func TestForget_AReplicaSettlingAtAForgetStillSettlesOnEveryPath(t *testing.T) {
	for name, switches := range map[string]map[string]bool{
		"own settle tracker": nil,
		"own trace":          {SwitchITLReadsSettle: false},
	} {
		t.Run(name, func(t *testing.T) {
			a := NewThroughputAnalyzer()
			require.NoError(t, a.SetSwitchesForReplay(switches))
			r := newLineRun(t, a)
			for range 12 {
				r.step(steadyFleet())
			}
			join := append(steadyFleet(), lineRow("pod-join", 0.46, 0.45, r.now()))
			r.step(join)
			read, _, _ := lineRead(a)
			require.Positive(t, read.Settling, "premise: the joiner's reading is stamped")

			forgetV1(a)
			r.step(join)
			read, _, _ = lineRead(a)
			assert.Equal(t, len(join), read.Held, "the stamped readings went with the window")
			assert.Positive(t, read.Settling, "the joiner still settles")
		})
	}
}
