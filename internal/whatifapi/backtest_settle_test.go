package whatifapi

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The relaxation time of the queueing review's table (issue #305): one
// Qwen3-32B 8k prefill pod, S = 0.47 s, at rho 0.8, 0.85, 0.9 and 0.95,
// with the measured spread (cv 0.06) and the assumed one (cv 2), random
// arrivals. Pooled over N pods it is N times shorter.
func TestRelaxationSeconds(t *testing.T) {
	const s = 0.47
	knee := 1 / s
	for _, c := range []struct {
		rho, cv, want float64
	}{
		{0.8, 0.06, 19}, {0.85, 0.06, 36}, {0.9, 0.06, 85}, {0.95, 0.06, 358},
		{0.8, 2, 94}, {0.85, 2, 178}, {0.9, 2, 423}, {0.95, 2, 1786},
	} {
		got := relaxationSeconds(c.rho*knee, knee, 1, 1, c.cv)
		assert.InDelta(t, c.want, got, 0.02*c.want+0.5, "rho %.2f cv %.2f", c.rho, c.cv)
	}
	// Two pods, pooled: the knee doubles, a pod's service time stays.
	assert.InDelta(t, relaxationSeconds(0.9*knee, knee, 1, 1, 0.06)/2, relaxationSeconds(0.9*2*knee, 2*knee, 2, 1, 0.06), 1e-9)
	// Bursty arrivals (Ca^2 4) relax slower.
	assert.InDelta(t, relaxationSeconds(0.9*knee, knee, 1, 1, 0.06)*(4+0.0036)/(1+0.0036), relaxationSeconds(0.9*knee, knee, 1, 4, 0.06), 1e-9)
	assert.True(t, math.IsInf(relaxationSeconds(knee, knee, 1, 1, 0.06), 1), "at the knee it never settles")
	assert.Zero(t, relaxationSeconds(1, 0, 2, 1, 0.06), "no knee priced")
	assert.Zero(t, relaxationSeconds(0, knee, 2, 1, 0.06), "no traffic")
	// A 60 s window takes a relaxation time of up to 12 s: one pod at rho
	// 0.75 with the measured spread settles, rho 0.85 does not; two pods at
	// 0.8 do, and with the assumed spread they do not.
	assert.True(t, settles(relaxationSeconds(0.75*knee, knee, 1, 1, 0.06), 60))
	assert.False(t, settles(relaxationSeconds(0.85*knee, knee, 1, 1, 0.06), 60))
	assert.True(t, settles(relaxationSeconds(0.8*2*knee, 2*knee, 2, 1, 0.06), 60))
	assert.False(t, settles(relaxationSeconds(0.8*2*knee, 2*knee, 2, 1, 2), 60))
}

// A fluid backlog: what runs past the knee in one step is queued at the
// next, drains at the knee less the rate, and adds its wait over the step's
// measurement window, the last part of the step.
func TestFluidBacklog(t *testing.T) {
	var f fluid
	m, most := f.step(3, 4, 300, 300)
	assert.Zero(t, m, "under the knee with nothing queued")
	assert.Zero(t, most)
	// 5 r/s against 4 for 300 s queues 300 requests; nothing was carried in.
	m, _ = f.step(5, 4, 300, 300)
	assert.Zero(t, m)
	assert.InDelta(t, 300, f.q, 1e-9)
	// At 2 r/s it drains at 2 r/s, empty at 150 s. Over the whole step the
	// mean backlog is 300 x 150 / 600 = 75 requests, 18.75 s at 4 r/s; the
	// most, at the step's start, 75 s.
	g := f
	m, most = g.step(2, 4, 300, 300)
	assert.InDelta(t, 300, g.carried, 1e-9)
	assert.InDelta(t, 75.0/4, m, 1e-9)
	assert.InDelta(t, 75, most, 1e-9)
	assert.Zero(t, g.q)
	// Measured over the step's last 60 s, the backlog is already gone.
	g = f
	m, most = g.step(2, 4, 300, 60)
	assert.Zero(t, m)
	assert.Zero(t, most)
	// Over its last 200 s (100 to 300 s) it holds 100 requests at 100 s and
	// empties at 150 s: a mean of 100 x 50 / 2 / 200 = 12.5, over 4 r/s.
	g = f
	m, most = g.step(2, 4, 300, 200)
	assert.InDelta(t, 12.5/4, m, 1e-9)
	assert.InDelta(t, 25, most, 1e-9)
	// Past the knee the backlog grows through the step: over the last 60 s
	// of 100 + 0.5 t, the mean is 100 + 0.5 x 270 and the most at the end.
	f = fluid{q: 100}
	m, most = f.step(4.5, 4, 300, 60)
	assert.InDelta(t, (100+0.5*270)/4, m, 1e-9)
	assert.InDelta(t, (100+0.5*300)/4, most, 1e-9)
	// A step with no requests drains at the knee last served.
	f = fluid{q: 300, mu: 4}
	f.idle(60)
	assert.InDelta(t, 60, f.q, 1e-9)
	// A rate that is no number breaks the chain.
	f = fluid{q: 100, mu: 4}
	m, _ = f.step(math.NaN(), 4, 300, 60)
	assert.Zero(t, m)
	assert.Zero(t, f.q)
	f.reset()
	assert.Zero(t, f.q)
	m, _ = backlogWait(10, 1, 0, 300, 60)
	assert.Zero(t, m, "no knee")
}

// A step whose prefill queue does not settle in its window has its TTFT
// unscored (unsettled), its TPOT scored.
func TestScoreStepUnsettled(t *testing.T) {
	one := 1.0
	st := BacktestStep{Unsettled: true, TTFT: []BacktestPoint{predictedPoint(0.9, Figure{Value: &one}, nil)},
		TPOT: []BacktestPoint{predictedPoint(0.9, Figure{Value: &one}, nil)}}
	m := measured{ttft: []*float64{&one}, tpot: []*float64{&one}, ttftWhy: []string{""}, tpotWhy: []string{""}, requests: 1000, tpotRequests: 1000}
	scoreStep(&st, m, nil)
	assert.Equal(t, UnscoredUnsettled, st.TTFT[0].Unscored)
	assert.Nil(t, st.TTFT[0].Ratio)
	require.NotNil(t, st.TPOT[0].Ratio)
	assert.InDelta(t, 1, *st.TPOT[0].Ratio, 1e-12)
}

// btSteps is a test window of 2 prefill and 2 decode pods at one rate a
// five-minute step (rates), the prefill pods of step i set by prefill[i]
// where given, with the router's latency.
func btSteps(rates []float64, prefill map[int]int) ([]SeriesSample, []LatencySample) {
	n := 20 * len(rates)
	s := make([]SeriesSample, n)
	l := make([]LatencySample, n)
	for i := range n {
		at := btT0.Add(time.Duration(i) * btCycle)
		r, np := rates[i/20], 2
		if p, ok := prefill[i/20]; ok {
			np = p
		}
		s[i] = SeriesSample{At: at, Rate: r, RateKnown: r > 0, Prefill: np, Decode: 2, PromptTokens: 1000, OutputTokens: 100, HasShape: true}
		l[i] = LatencySample{At: at, Router: btRouter(r, 0.1)}
	}
	return s, l
}

// Through a backtest on the stub (a fleet knee of 19.6 r/s, the prefill
// pods', and the assumed prompt spread): a step at 0.95 of the knee is
// unsettled and its TTFT unscored, one at 0.77 is scored; a step past the
// knee leaves a backlog that an idle step drains at the knee and the next
// step carries; a step that does not price (no prefill pod) clears it.
func TestBacktestStepsSettleAndCarry(t *testing.T) {
	r := &stubResolver{}
	fitS, fitL := btEarlier(btSeries(240, 0, 1))
	fit := btEntry(t, r, "fit", fitS, fitL, btConfig("H100"))
	rates := []float64{5, 15, 18.6, 45, 0, 5, 45, 5, 5}
	s, l := btSteps(rates, map[int]int{7: 0})
	out := btRun(t, fit, btEntry(t, r, "t", s, l, btConfig("H100")), BacktestRequest{})
	require.Len(t, out.Steps, len(rates))
	st := out.Steps
	require.NotNil(t, st[1].KneeShare)
	mu := 5 / *st[0].KneeShare
	require.InDelta(t, 19.6, mu, 0.1, "the premise: the stub's fleet knee")

	require.NotNil(t, st[1].RelaxationSeconds)
	assert.False(t, st[1].Unsettled, "0.77 of the knee settles: tr %.0f s", *st[1].RelaxationSeconds)
	require.NotNil(t, st[1].TTFT[0].Ratio, "and its TTFT is scored")
	require.NotNil(t, st[2].RelaxationSeconds)
	assert.True(t, st[2].Unsettled, "0.95 of the knee does not: tr %.0f s", *st[2].RelaxationSeconds)
	for _, p := range st[2].TTFT {
		assert.Equal(t, UnscoredUnsettled, p.Unscored, p.Statistic)
	}
	assert.Nil(t, st[3].RelaxationSeconds, "past the knee")

	// 45 r/s for 300 s queues (45 - mu) x 300; the idle step drains mu x 300
	// of it, and 10:25 carries the rest.
	assert.Equal(t, ExcludedNoTraffic, st[4].Excluded)
	require.NotNil(t, st[5].Backlog)
	assert.InDelta(t, (45-mu)*300-mu*300, *st[5].Backlog, 1)
	require.NotNil(t, st[5].BacklogWait)
	assert.Positive(t, *st[5].BacklogWait)

	// The step with no prefill pod does not price: the backlog 10:30 left is
	// not carried past it.
	assert.Zero(t, st[7].Prefill)
	assert.Nil(t, st[7].KneeShare, "10:35 did not price")
	assert.Nil(t, st[8].Backlog, "the chain broke at 10:35")
}
