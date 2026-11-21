package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// withGPUs is st with the prefill variant's replicas taking prefill GPUs
// and the decode variant's decode GPUs, st itself unchanged.
func withGPUs(st *learnreplay.State, prefill, decode int) *learnreplay.State {
	out := *st
	out.Variants = slices.Clone(st.Variants)
	for i := range out.Variants {
		switch out.Variants[i].Variant {
		case "variant-02":
			out.Variants[i].GPUsPerReplica = prefill
		case "variant-01":
			out.Variants[i].GPUsPerReplica = decode
		}
	}
	return &out
}

// With the roles' GPUs per replica unequal, the fleet needed is the fewest
// GPUs that hold, not the fewest replicas: it holds, and at each decode
// count up to the bound the most prefill replicas that would cost fewer
// GPUs do not. On the 16k-prompt shape at TTFT p95, where decode admission
// sets most of the TTFT and decode replicas lift it, the GPU answer trades
// one role for the other against the replica answer at some load.
func TestAtReplicas_NeededFewestGPUs(t *testing.T) {
	plain := stairReplay.state(t)
	traded := false
	for _, w := range []whatifapi.FleetCounts{{Prefill: 8, Decode: 1}, {Prefill: 1, Decode: 8}} {
		if neededFewestGPUs(t, plain, w) {
			traded = true
		}
	}
	assert.True(t, traded, "the GPU answer never differed from the replica answer: the test does not reach the weighing")
}

// neededFewestGPUs checks the needed fleet on plain with the GPUs w across
// loads, and reports whether it differed from the replica answer at any.
func neededFewestGPUs(t *testing.T, plain *learnreplay.State, w whatifapi.FleetCounts) bool {
	t.Helper()
	st := withGPUs(plain, w.Prefill, w.Decode)
	sh, err := whatifapi.ParseReportShape("16k/200/0.3")
	require.NoError(t, err)
	needed := func(st *learnreplay.State, load float64) (*reporter, whatifapi.ReportNeeded) {
		t.Helper()
		q := stairQuery(2, 2, 0)
		q.TTFT.Percentile = 0.95
		q.Shapes = []whatifapi.ReportShapeAsk{sh}
		r := &reporter{st: st, s: &servedState{st: st, decodeVariant: "variant-01"}, q: q, fleets: map[fleetKey]*servedFleet{}}
		r.resolveTargets()
		r.sh, r.load = sh, load
		base, err := r.evaluateAt(2, 2, load, false)
		require.NoError(t, err)
		var v whatifapi.ReportVerdict
		_, _, err = r.needed(2, 2, base, &v)
		require.NoError(t, err)
		require.NotNil(t, v.Needed)
		require.Empty(t, v.Needed.Unmet)
		return r, *v.Needed
	}
	traded := false
	for _, load := range []float64{30, 41.7, 54, 62.6} {
		r, n := needed(st, load)
		_, byReplicas := needed(plain, load)
		assert.Equal(t, whatifapi.MinimizesGPUs, n.Minimizes)
		require.NotNil(t, n.GPUs)
		cost := whatifapi.FleetCounts{Prefill: n.Prefill, Decode: n.Decode}.Cost(w)
		assert.Equal(t, cost, *n.GPUs)
		t.Logf("GPUs %v, at %g r/s: %d + %d = %d GPUs; by replicas %d + %d = %d GPUs", w, load, n.Prefill, n.Decode, cost,
			byReplicas.Prefill, byReplicas.Decode, whatifapi.FleetCounts{Prefill: byReplicas.Prefill, Decode: byReplicas.Decode}.Cost(w))
		assert.LessOrEqual(t, cost, whatifapi.FleetCounts{Prefill: byReplicas.Prefill, Decode: byReplicas.Decode}.Cost(w), "at %g r/s the GPU answer costs more than the replica answer", load)
		if n.Prefill != byReplicas.Prefill {
			traded = true
		}
		holds := func(np, nd int) bool {
			t.Helper()
			e, err := r.evaluateAt(np, nd, load, false)
			require.NoError(t, err)
			return e.holds
		}
		assert.True(t, holds(n.Prefill, n.Decode))
		for d := 1; d <= whatifapi.DefaultSizeBound && d*w.Decode < cost-w.Prefill; d++ {
			p := min((cost-d*w.Decode-1)/w.Prefill, whatifapi.DefaultSizeBound)
			if p < 1 {
				continue
			}
			f := whatifapi.FleetCounts{Prefill: p, Decode: d}
			assert.False(t, holds(p, d), "at %g r/s: %d + %d (%d GPUs) holds, fewer than %d + %d (%d GPUs)",
				load, p, d, f.Cost(w), n.Prefill, n.Decode, cost)
		}
	}
	return traded
}

// --spare N adds N replicas of each role to the fleet needed, and the
// needed line under the verdict says so with the GPUs. A right-sized fleet
// takes its spare on itself. The verdict sentence is unchanged.
func TestAtReplicas_Spare(t *testing.T) {
	q := stairQuery(2, 2, 0)
	plain := stairReport(t, q)
	q.Spare = 1
	rep := stairReport(t, q)
	v := rep.Shapes[0].Verdict
	assert.Equal(t, plain.Shapes[0].Verdict.Summary, v.Summary, "a spare rewrote the verdict")
	require.Equal(t, whatifapi.VerdictUnder, v.Verdict)
	require.NotNil(t, v.Needed)
	require.NotNil(t, v.Needed.WithSpare)
	n := v.Needed
	assert.Equal(t, whatifapi.FleetCounts{Prefill: n.Prefill + 1, Decode: n.Decode + 1}, n.WithSpare.Fleet)
	assert.Nil(t, plain.Shapes[0].Verdict.Needed.WithSpare)
	text := reportText(rep)
	assert.Contains(t, text, "with 1 spare of each role", text)
	assert.NotContains(t, reportText(plain), "spare of each role")

	q = stairQuery(n.Prefill, n.Decode, 0)
	q.Spare = 2
	right := stairReport(t, q).Shapes[0].Verdict
	if right.Verdict == whatifapi.VerdictRight {
		require.NotNil(t, right.Needed)
		assert.Empty(t, right.Needed.Minimizes, "the fleet judged is not searched")
		assert.Equal(t, whatifapi.FleetCounts{Prefill: n.Prefill + 2, Decode: n.Decode + 2}, right.Needed.WithSpare.Fleet)
	} else {
		t.Logf("%d + %d is %s, not right-sized: the right-sized spare is not reached here", n.Prefill, n.Decode, right.Verdict)
	}
}

// The needed line is printed only where it adds to the verdict: a spare
// asked, or roles whose GPUs per replica differ, when it names the GPUs,
// what the search minimised and the GPUs of the other mixes.
func TestSpareText(t *testing.T) {
	g := whatifapi.FleetCounts{Prefill: 8, Decode: 1}
	n := &whatifapi.ReportNeeded{Prefill: 8, Decode: 5, Minimizes: whatifapi.MinimizesGPUs, GPUsPerReplica: &g,
		GPUs: whatifapi.FleetGPUs(whatifapi.FleetCounts{Prefill: 8, Decode: 5}, g), Alternatives: []whatifapi.FleetCounts{{Prefill: 9, Decode: 4}}}
	assert.Equal(t, "8 prefill + 5 decode = 69 GPUs (8 a prefill replica, 1 a decode replica), the fewest GPUs that hold; or 9 + 4 = 76 GPUs",
		spareText(n))
	n.WithSpare = whatifapi.WithSpare(whatifapi.FleetCounts{Prefill: 8, Decode: 5}, 1, g)
	assert.True(t, strings.HasSuffix(spareText(n), "; with 1 spare of each role 9 prefill + 6 decode = 78 GPUs, which still holds with 1 of each role lost"),
		spareText(n))

	same := whatifapi.FleetCounts{Prefill: 1, Decode: 1}
	assert.Empty(t, spareText(&whatifapi.ReportNeeded{Prefill: 3, Decode: 3, GPUsPerReplica: &same}), "equal GPUs and no spare add nothing")
	assert.Empty(t, spareText(&whatifapi.ReportNeeded{Unmet: "fleet"}))
	assert.Empty(t, spareText(nil))
	d := &whatifapi.ReportNeeded{Decode: 3, DecodeOnly: true, WithSpare: whatifapi.WithSpare(whatifapi.FleetCounts{Decode: 3}, 1, whatifapi.FleetCounts{})}
	assert.Equal(t, "at least 3 decode; with 1 spare decode at least 4 decode, which still holds with 1 decode lost", spareText(d))
	d.GPUs, d.WithSpare = whatifapi.FleetGPUs(whatifapi.FleetCounts{Decode: 3}, whatifapi.FleetCounts{Decode: 2}),
		whatifapi.WithSpare(whatifapi.FleetCounts{Decode: 3}, 1, whatifapi.FleetCounts{Decode: 2})
	assert.Equal(t, "at least 3 decode = 6 decode GPUs; with 1 spare decode at least 4 decode = 8 decode GPUs, which still holds with 1 decode lost", spareText(d))
}

// A right-sized fleet has no replica of either role to spare, yet with the
// roles' GPUs per replica unequal a mix of fewer GPUs may hold: at 54 r/s on
// the 16k-prompt shape at TTFT p95, 8 prefill + 7 decode holds and neither 7
// + 7 nor 8 + 6 does, while 7 + 8 holds on 64 GPUs against 71 at 8 and 1 a
// replica. The verdict stays RIGHT-SIZED, its sentence unchanged, and Needed
// names the cheaper mix. With equal GPUs and no spare there is none.
func TestAtReplicas_RightSizedNamesACheaperMix(t *testing.T) {
	plain := stairReplay.state(t)
	sh, err := whatifapi.ParseReportShape("16k/200/0.3")
	require.NoError(t, err)
	verdict := func(st *learnreplay.State) whatifapi.ReportVerdict {
		t.Helper()
		q := stairQuery(8, 7, 0)
		q.TTFT.Percentile = 0.95
		q.Shapes = []whatifapi.ReportShapeAsk{sh}
		r := &reporter{st: st, s: &servedState{st: st, decodeVariant: "variant-01"}, q: q, fleets: map[fleetKey]*servedFleet{}}
		r.resolveTargets()
		r.sh, r.load = sh, 54
		base, err := r.evaluateAt(8, 7, 54, false)
		require.NoError(t, err)
		v, err := r.verdict(8, 7, base)
		require.NoError(t, err)
		return v
	}
	v := verdict(withGPUs(plain, 8, 1))
	require.Equal(t, whatifapi.VerdictRight, v.Verdict, v.Reason)
	require.NotNil(t, v.Needed)
	assert.Equal(t, whatifapi.FleetCounts{Prefill: 7, Decode: 8}, whatifapi.FleetCounts{Prefill: v.Needed.Prefill, Decode: v.Needed.Decode})
	assert.Equal(t, whatifapi.MinimizesGPUs, v.Needed.Minimizes)
	assert.Empty(t, v.Grow, "a right-sized fleet grows no role")
	assert.NotContains(t, v.Summary, "needs", "the verdict sentence is unchanged")
	assert.Contains(t, spareText(v.Needed), "7 prefill + 8 decode = 64 GPUs")
	assert.Contains(t, v.Reason, "A mix of fewer GPUs also holds: 7 prefill + 8 decode (has 8 + 7)")

	v = verdict(plain)
	require.Equal(t, whatifapi.VerdictRight, v.Verdict, v.Reason)
	assert.Nil(t, v.Needed, "equal GPUs and no spare: nothing to name")
}
