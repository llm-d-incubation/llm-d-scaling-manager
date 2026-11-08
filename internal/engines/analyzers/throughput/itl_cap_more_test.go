package throughput

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// Learner PR 4b-3's round-1 review: the one-sided price at the cap, the
// summary line while the line stays live, and second tests, through
// Analyze, for rules the first round held with one test each.

// withCapPinnedAtDefaultB prices readings at the cap through the default B
// alone, as 4b-3 first built it, for a negative control.
func (a *ThroughputAnalyzer) withCapPinnedAtDefaultB() *ThroughputAnalyzer {
	a.capPinsDefaultB = true
	return a
}

// TestITLCap_TheBiasTable prices each truth of the one-sided grid
// (atCapTruths) over 100 seeds three ways: on; pinned at the default B
// only, as 4b-3 first built it (the negative control); and with the cap
// admission off, where the above-band fallback prices. It logs each one's
// error at itl.DefaultKSat against the truth on the last cycle: without
// noise, and over the seeds with the gauge's noise (mean, least and most),
// in percent. It holds that the line on never prices under the truth
// without noise, and not on average with it; and that the control prices
// under it without noise at every point of the grid (every true B is above
// the default 6 ms), and on average at a true B of 30 ms.
func TestITLCap_TheBiasTable(t *testing.T) {
	const cycles, seeds = 24, 100
	type stat struct{ clean, sum, lo, hi float64 }
	measure := func(newA propAnalyzer, l atCapTruth) stat {
		truth := l.itl(itl.DefaultKSat)
		errAt := func(seed uint64, noisy bool) float64 {
			lines, _, err := atCapRun(newA, l, seed, cycles, noisy)
			require.NoError(t, err)
			m, ok := lines[cycles-1]
			require.True(t, ok, "%s seed %d: no line on the last cycle", l.name(), seed)
			return 100 * (propPriced(m) - truth) / truth
		}
		s := stat{clean: errAt(1, false), lo: math.Inf(1), hi: math.Inf(-1)}
		for seed := uint64(1); seed <= seeds; seed++ {
			e := errAt(seed, true)
			s.sum += e
			s.lo, s.hi = min(s.lo, e), max(s.hi, e)
		}
		return s
	}
	show := func(s stat) string {
		return fmt.Sprintf("%+.2f%%; %+.2f%% [%+.2f, %+.2f]", s.clean, s.sum/seeds, s.lo, s.hi)
	}
	t.Log("each cell: no noise; with noise, mean [least, most] over 100 seeds")
	t.Log("| A | B | k at cap | on | default B only | cap admission off |")
	for _, l := range atCapTruths {
		on := measure(NewThroughputAnalyzer, l)
		def := measure(func() *ThroughputAnalyzer { return NewThroughputAnalyzer().withCapPinnedAtDefaultB() }, l)
		off := measure(withSwitchOff(SwitchITLAdmitsAtSequenceCap), l)
		t.Logf("| %.2f | %.0f ms | %.2f | %s | %s | %s |", l.a, 1000*l.b, l.kCap, show(on), show(def), show(off))
		require.GreaterOrEqual(t, on.clean, 0.0, "%s: the line priced under the truth without noise", l.name())
		require.GreaterOrEqual(t, on.sum, 0.0, "%s: the line priced under the truth on average", l.name())
		// Known truth's epsilon: priced at least truth / (1 + epsilon), on
		// the worst seed.
		require.GreaterOrEqual(t, on.lo, 100*(1/(1+propTruthTol)-1), "%s: the worst seed is past epsilon", l.name())
		// The control is under by the term the plan derives,
		// (B0 - B)(1 - kSat/kbar) over the truth, kbar the cap's mean fill:
		// to 0.05 points, the window's mix of the three fills.
		want := 100 * (itl.DefaultBaselineSec - l.b) * (1 - itl.DefaultKSat/l.kCap) / l.itl(itl.DefaultKSat)
		require.InDelta(t, want, def.clean, 0.05, "negative control, %s: the default B alone priced off the derived term", l.name())
		require.Negative(t, def.clean, "negative control, %s: the default B alone did not price under the truth", l.name())
		if l.b == 0.030 {
			require.Negative(t, def.sum, "negative control, %s: the default B alone did not price under the truth on average", l.name())
		}
		// On readings at the cap alone the one-sided line is the fallback's
		// line, so the two columns agree.
		require.InDelta(t, on.clean, off.clean, 1e-9, "%s: on and the fallback differ without noise", l.name())
		require.InDelta(t, on.sum, off.sum, 1e-9, "%s: on and the fallback differ with noise", l.name())
		require.InDelta(t, on.lo, off.lo, 1e-9, "%s: on and the fallback differ on the worst seed", l.name())
		require.InDelta(t, on.hi, off.hi, 1e-9, "%s: on and the fallback differ on the best seed", l.name())
	}
}

// The one-sided rule lasts exactly while the key has no learned B, both
// ways, in one run. A variant taught only by its capped replica prices
// one-sided (itl.Reading.PinnedAtLeastITL); once a full fit in the band
// learns B, the pinned fit through it prices the same capped readings, and
// the rule is off; and once the key is forgotten -- the variant unseen past
// the analyzer's eviction (2 x itl.DefaultObservationMaxAge), which sweeps
// its line's key, the line's only forget today -- B is unlearned again and
// the one-sided price comes back.
func TestITLCap_ALearnedBEndsTheOneSidedPriceAndAForgetBringsItBack(t *testing.T) {
	a := NewThroughputAnalyzer().WithCapacityStore(paramsStore(capacity.EngineParams{Engine: "vllm", DeclaredBatchCap: 64}))
	r := newLineRun(t, a)
	early := lineStart.Add(-time.Hour)
	capOnly := func() []domain.ReplicaMetrics {
		return []domain.ReplicaMetrics{capLineRow("pod-cap", 0.92),
			lineRow("pod-a", 0.05, lineAt(0.05), early), lineRow("pod-b", 0.05, lineAt(0.05), early)}
	}
	// pinned runs n cycles of rows and returns the last pinned reading.
	pinned := func(n int, rows func(c int) []domain.ReplicaMetrics) itl.Reading {
		var last itl.Reading
		for c := range n {
			r.step(rows(c))
			if read, _, _ := lineRead(a); read.Tier == itl.TierPinnedB {
				last = read
			}
		}
		return last
	}

	// Unlearned: the capped replica alone prices one-sided.
	read := pinned(12, func(int) []domain.ReplicaMetrics { return capOnly() })
	require.False(t, read.BaselineLearned, "premise: no B learned yet")
	require.True(t, read.PinnedAtLeastITL, "unlearned: not priced one-sided")

	// Learned: two replicas stepping through the band teach B by a full fit.
	pinned(64, func(c int) []domain.ReplicaMetrics {
		k := 0.25
		if (c/16)%2 == 1 {
			k = 0.65
		}
		return append(capOnly(), lineRow("pod-c", k, lineAt(k), early), lineRow("pod-d", 0.45, lineAt(0.45), early))
	})
	b, learned := a.lines.Baseline(variantKey("ns", "m", "v1"))
	require.True(t, learned && b > 0, "premise: the band's full fit learned B")
	// The band's readings age out; the capped replica alone prices again.
	read = pinned(140, func(int) []domain.ReplicaMetrics { return capOnly() })
	require.True(t, read.BaselineLearned)
	require.Positive(t, read.Capped)
	require.False(t, read.PinnedAtLeastITL, "learned: still priced one-sided")

	// Forgotten: another variant's cycles alone, past the eviction.
	other := lineRow("pod-x", 0.40, lineAt(0.40), early)
	other.VariantName = "v2"
	until := r.now().Add(2*itl.DefaultObservationMaxAge + time.Minute)
	for r.now().Before(until) {
		r.step([]domain.ReplicaMetrics{other})
	}
	_, learned = a.lines.Baseline(variantKey("ns", "m", "v1"))
	require.False(t, learned, "premise: the eviction forgot the key's B")
	read = pinned(12, func(int) []domain.ReplicaMetrics { return capOnly() })
	require.False(t, read.BaselineLearned)
	require.True(t, read.PinnedAtLeastITL, "forgotten: the one-sided price did not come back")
}

// What is not at the cap is no reading, through Analyze, apart from the
// property case's edges: a two-rank replica at one rank's cap, and a
// replica at its cap with nothing waiting. Both ranks at the cap is.
func TestITLCap_WhatIsNotAtTheCapIsNotAReading(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  func(*domain.ReplicaMetrics)
		want int
	}{
		{"two ranks, one rank's cap", func(m *domain.ReplicaMetrics) { m.EngineRanks = 2 }, 0},
		{"two ranks, both at the cap", func(m *domain.ReplicaMetrics) { m.EngineRanks, m.RunningRequests = 2, 128 }, 1},
		{"nothing waiting", func(m *domain.ReplicaMetrics) { m.QueueLength = 0 }, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := NewThroughputAnalyzer().WithCapacityStore(paramsStore(capacity.EngineParams{
				Engine: inferenceengine.EngineVLLM, MaxNumSeqs: 64, MaxNumSeqsSet: true}))
			row := capLineRow("pod-a", 0.88)
			tc.row(&row)
			newLineRun(t, a).step([]domain.ReplicaMetrics{row})
			read, _, _ := lineRead(a)
			require.Equal(t, tc.want, read.Capped)
			require.Equal(t, 1-tc.want, read.AboveBand)
		})
	}
}

// The line's summary logs while the line stays live when readings at the
// cap start or stop, and when the cap's source changes: one line each, with
// atCap and batchCapSource as they are then.
func TestITLCap_TheSummaryLogsTheCapWhileTheLineIsLive(t *testing.T) {
	store := paramsStore(capacity.EngineParams{Engine: "vllm", DeclaredBatchCap: 64})
	ctx, buf := logContext()
	r := newLineRun(t, NewThroughputAnalyzer().WithCapacityStore(store))
	r.ctx = ctx
	capped := func(running int) []domain.ReplicaMetrics {
		row := capLineRow("pod-cap", 0.88)
		row.RunningRequests = running
		return append(steadyFleet(), row)
	}
	for range 12 {
		r.step(steadyFleet())
	}
	logs := lineLogs(buf.String())
	require.Len(t, logs, 2, "premise: silent on the first cycles, then live: %s", buf)
	for range 3 {
		vc, has := r.step(capped(64))
		require.True(t, has, "premise: the line stays live (%s)", vc.Reason)
	}
	logs = lineLogs(buf.String())
	require.Len(t, logs, 3, "readings at the cap started: one line, not one per cycle")
	require.Contains(t, logs[2], `"silent": false`)
	require.Contains(t, logs[2], `"atCap": 1, "batchCap": 64, "batchCapSource": "declared"`)

	for range 2 {
		r.step(capped(63))
	}
	logs = lineLogs(buf.String())
	require.Len(t, logs, 4, "readings at the cap stopped: one line")
	require.Contains(t, logs[3], `"atCap": 0, "batchCap": 64, "batchCapSource": "declared"`)

	store.Update("ns", "m", "v1", capacity.Record{EngineParams: &capacity.EngineParams{
		Engine: inferenceengine.EngineVLLM, MaxNumSeqs: 64, MaxNumSeqsSet: true}}, r.now())
	for range 2 {
		r.step(capped(63))
	}
	logs = lineLogs(buf.String())
	require.Len(t, logs, 5, "the cap's source changed: one line")
	require.Contains(t, logs[4], `"atCap": 0, "batchCap": 64, "batchCapSource": "flags"`)
	require.Equal(t, 5, strings.Count(buf.String(), "throughput analyzer: ITL line"))
}

// A slope held after a one-sided price prices what that line did at
// itl.DefaultKSat: the hold pairs its A with the default B, so the A held is
// the one that reproduces the one-sided price through it, not the flat
// line's own A (which, through 6 ms, would price far under the truth).
func TestITLCap_AHeldSlopeKeepsTheOneSidedPrice(t *testing.T) {
	a := NewThroughputAnalyzer().WithSlopeHoldFor(time.Hour).
		WithCapacityStore(paramsStore(capacity.EngineParams{Engine: "vllm", DeclaredBatchCap: 64}))
	r := newLineRun(t, a)
	early := lineStart.Add(-time.Hour)
	below := func() []domain.ReplicaMetrics {
		return []domain.ReplicaMetrics{lineRow("pod-a", 0.05, probeITL(0.05), early), lineRow("pod-b", 0.05, probeITL(0.05), early)}
	}
	for range 20 {
		r.step(append(below(), capLineRow("pod-cap", 0.92)))
	}
	read, _, _ := lineRead(a)
	require.True(t, read.PinnedAtLeastITL, "premise: priced one-sided")
	st, _ := a.VariantState("m", "ns", "v1")
	oneSided := st.ITLModel.ITLAt(itl.DefaultKSat)

	var vc domain.VariantCapacity
	var has bool
	for r.now().Before(lineStart.Add(40 * time.Minute)) {
		vc, has = r.step(below())
	}
	require.True(t, has)
	require.Equal(t, itlReasonT2Held, vc.Reason, "premise: the window ran short and the slope is held")
	st, _ = a.VariantState("m", "ns", "v1")
	held := st.ITLModel.ITLAt(itl.DefaultKSat)
	t.Logf("one-sided %.5f s, held %.5f s, truth %.5f s", oneSided, held, probeITL(itl.DefaultKSat))
	require.InDelta(t, oneSided, held, 1e-9)
	require.GreaterOrEqual(t, held, probeITL(itl.DefaultKSat))
}

// The held one-sided slope ends with the hold (SlopeHoldFor, 1 h here),
// counted from the last fit: the window of capped readings last fitted
// about 32 min in, as its tenth-newest reading aged out of the 30 min
// window. After that the line is silent -- T2-failed, no throughput
// capacity -- and nothing prices the variant from above the band: the
// fallback has no readings there, and the held slope is not extended.
func TestITLCap_TheHeldOneSidedSlopeEndsWithTheHold(t *testing.T) {
	a := NewThroughputAnalyzer().WithSlopeHoldFor(time.Hour).
		WithCapacityStore(paramsStore(capacity.EngineParams{Engine: "vllm", DeclaredBatchCap: 64}))
	r := newLineRun(t, a)
	early := lineStart.Add(-time.Hour)
	below := func() []domain.ReplicaMetrics {
		return []domain.ReplicaMetrics{lineRow("pod-a", 0.05, probeITL(0.05), early), lineRow("pod-b", 0.05, probeITL(0.05), early)}
	}
	for range 20 {
		r.step(append(below(), capLineRow("pod-cap", 0.92)))
	}
	var lastFit, lastHeld, firstSilent time.Time
	for r.now().Before(lineStart.Add(2 * time.Hour)) {
		at := r.now()
		vc, has := r.step(below())
		switch {
		case has && vc.Reason == itlReasonT2Held:
			lastHeld = at
		case has:
			lastFit = at
		case firstSilent.IsZero():
			firstSilent = at
		}
		if !firstSilent.IsZero() {
			require.False(t, has, "%s: a line came back after the hold ended (%s)", at.Sub(lineStart), vc.Reason)
		}
	}
	t.Logf("last fit %s, last held %s, silent from %s", lastFit.Sub(lineStart), lastHeld.Sub(lineStart), firstSilent.Sub(lineStart))
	require.False(t, lastHeld.IsZero(), "premise: the slope was held")
	require.False(t, firstSilent.IsZero(), "the held slope never ended")
	require.True(t, lastHeld.Sub(lastFit) <= time.Hour && firstSilent.Sub(lastFit) > time.Hour,
		"the hold ended %s after the last fit, want just past 1h", firstSilent.Sub(lastFit))
	require.Equal(t, 15*time.Second, firstSilent.Sub(lastHeld), "silent on the cycle after the last held one")
}
