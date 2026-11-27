package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/slo"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// cachedCostState is computedHitState (a 0.75 computed-token hit on
// 8000-token prompts) with slo.SwitchTTFTCachedTokenCost as on.
func cachedCostState(on bool) *learnreplay.State {
	st := computedHitState(0.5, true)
	st.Prefill[0].CachedTokenCost = on
	return st
}

// fittedAt is answerFlags priced at a fitted phi.
func fittedAt(phi float64) answerFlags {
	c := whatifapi.CachedTokenCost{Phi: phi, Fitted: true, Steps: 40, CachedShare: 0.75}
	return answerFlags{inputTokens: 8000, cachedCost: &c, cachedPhi: phi}
}

// With slo.SwitchTTFTCachedTokenCost on and phi fitted, a request's own
// latency carries phi x cached / V_P: 8000 tokens at a 0.75 computed-token
// hit are 6000 cached, at phi 0.5 and V_P 100000 tok/s 30 ms. Off, or at phi
// 0, every figure is as before; the answer names phi either way the switch
// is on.
func TestTTFT_CachedTokenCostAddsTheLoad(t *testing.T) {
	base := hitFleetOf(t, cachedCostState(false), hitDecode(0.5, 0), fittedAt(0.5), "assume-input-tokens")
	require.Nil(t, base.refused)
	a := base.fleet.Prefill.FixedLatency
	assert.NotContains(t, strings.Join(base.in.Assumed, "; "), "cached-token cost", "off: not named")

	on := hitFleetOf(t, cachedCostState(true), hitDecode(0.5, 0), fittedAt(0.5), "assume-input-tokens")
	require.Nil(t, on.refused)
	assert.InDelta(t, a+0.5*6000/100000.0, on.fleet.Prefill.FixedLatency, 1e-12)
	assert.Equal(t, base.fleet.Prefill.PromptTokens, on.fleet.Prefill.PromptTokens, "the load is no prefill work")
	assert.Equal(t, base.fleet.Prefill.VP, on.fleet.Prefill.VP)
	words := strings.Join(on.in.Assumed, "; ")
	assert.Contains(t, words, "cached-token cost phi 0.500, fitted on 40 steps")
	assert.Contains(t, words, "6000 cached prompt tokens a request priced at phi 0.500 of a computed token: 30.0 ms")

	zero := hitFleetOf(t, cachedCostState(true), hitDecode(0.5, 0), fittedAt(0), "assume-input-tokens")
	assert.Equal(t, a, zero.fleet.Prefill.FixedLatency, "phi 0: as before")

	// At the measured prompt the cached tokens are read from the computed
	// side: 2000 computed at a 0.75 hit are 6000 cached, whatever whole
	// prompt the prefill variant's shape read over its own window (12000
	// here, which at the same hit would be 9000).
	st := cachedCostState(true)
	st.Prefill[0].PromptTokens = 2000
	st.Variants[0].Shape.InputTokens = 12000
	f := fittedAt(0.5)
	f.inputTokens = 0
	m := hitFleetOf(t, st, hitDecode(0.5, 0), f)
	require.Nil(t, m.refused)
	assert.Equal(t, shapeMeasured, m.in.PromptSource)
	require.Equal(t, 12000.0, m.fleet.Prefill.WholePromptTokens, "the shape's whole prompt is read beside it")
	assert.InDelta(t, a+0.5*6000/100000.0, m.fleet.Prefill.FixedLatency, 1e-12)

	// Not fitted: phi 0, and the answer says why.
	nf := whatifapi.NotFittedCachedTokenCost(0.75, whatifapi.CachedCostTooFewSteps+" (3, under 12)")
	pf := hitFleetOf(t, cachedCostState(true), hitDecode(0.5, 0), answerFlags{inputTokens: 8000, cachedCost: &nf}, "assume-input-tokens")
	assert.Equal(t, a, pf.fleet.Prefill.FixedLatency)
	assert.Contains(t, strings.Join(pf.in.Assumed, "; "), "cached-token cost phi 0, not fitted: too few steps (3, under 12)")
	// No fit at all (a one-shot answer): named, nothing moves.
	pf = hitFleetOf(t, cachedCostState(true), hitDecode(0.5, 0), answerFlags{inputTokens: 8000}, "assume-input-tokens")
	assert.Equal(t, a, pf.fleet.Prefill.FixedLatency)
	assert.Contains(t, pf.in.Assumed, cachedCostWords)
}

// Under whatifapi.MinCachedTokenCostShare of the prompt cached there is no
// load to price: a 0.05 computed-token hit moves nothing at any phi.
func TestTTFT_CachedTokenCostNeedsACachedShare(t *testing.T) {
	st := cachedCostState(true)
	st.Prefill[0].PromptWindow = computedHitWindow(7600)
	off := cachedCostState(false)
	off.Prefill[0].PromptWindow = computedHitWindow(7600)
	a := hitFleetOf(t, off, hitDecode(0, 0), fittedAt(1), "assume-input-tokens").fleet.Prefill.FixedLatency
	pf := hitFleetOf(t, st, hitDecode(0, 0), fittedAt(1), "assume-input-tokens")
	assert.Equal(t, a, pf.fleet.Prefill.FixedLatency)
	assert.NotContains(t, strings.Join(pf.in.Assumed, "; "), "cached prompt tokens a request")
}

// costRecording is hitRecording (prompts wholeScale times what was served,
// all but the computed share served from cache) with every request's TTFT
// raised by phi x cached / vp, cached the cycle's whole prompt less its
// computed share: a fleet whose cached tokens cost phi of a computed one.
// The load is where an engine that loads a request's KV from a host tier or
// a peer before scheduling it reports it, in the request's queue time: the
// router's TTFT and the prefill variant's rows' mean TTFT and queue time
// rise by it, its prefill time does not. So a, read as the prefill time's
// intercept plus the TTFT outside the prefill and the queue, and the
// router's hop over the engine's TTFT, are learned as before. It returns
// the recording and the windows raised.
func costRecording(t *testing.T, wholeScale, phi, vp float64, prefillVariant string) ([]byte, int) {
	t.Helper()
	rec := hitRecording(t, wholeScale, 0.5)
	var out bytes.Buffer
	sc := bufio.NewScanner(bytes.NewReader(rec))
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	raised := 0
	for sc.Scan() {
		dec := json.NewDecoder(bytes.NewReader(sc.Bytes()))
		dec.UseNumber()
		var ev map[string]any
		require.NoError(t, dec.Decode(&ev))
		if prompt := meanPrompt(t, ev["Data"]); prompt > 0 {
			d := phi * prompt * (1 - 1/wholeScale) / vp
			raised += raiseTTFT(t, ev["Data"], d, prefillVariant)
		}
		b, err := json.Marshal(ev)
		require.NoError(t, err)
		out.Write(append(b, '\n'))
	}
	require.NoError(t, sc.Err())
	return out.Bytes(), raised
}

// floatOf is a JSON number as a float, false where it is none.
func floatOf(t *testing.T, v any) (float64, bool) {
	t.Helper()
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		require.NoError(t, err)
		return f, true
	case float64:
		return x, true
	}
	return 0, false
}

// meanPrompt is the mean AvgInputTokensRecent of the rows under v, 0 with
// none.
func meanPrompt(t *testing.T, v any) float64 {
	var sum float64
	var n int
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if f, ok := floatOf(t, x["AvgInputTokensRecent"]); ok && f > 0 {
				sum += f
				n++
			}
			for _, c := range x {
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	walk(v)
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// raiseTTFT adds d to the mean of every router TTFT window under v (its sum
// rate by d x its count rate), and to the mean TTFT and queue time of every
// row of prefillVariant that reads them, and returns how many it raised.
func raiseTTFT(t *testing.T, v any, d float64, prefillVariant string) int {
	n := 0
	switch x := v.(type) {
	case map[string]any:
		s, okS := floatOf(t, x["TTFTSumRate"])
		c, okC := floatOf(t, x["TTFTCountRate"])
		if okS && okC && c > 0 {
			x["TTFTSumRate"] = s + d*c
			n++
		}
		if x["VariantName"] == prefillVariant {
			for _, k := range []string{"AvgTTFT", "AvgQueueTime"} {
				if f, ok := floatOf(t, x[k]); ok && f > 0 && x[k+"Known"] == true {
					x[k] = f + d
				}
			}
		}
		for k, c := range x {
			if k != "TTFTSumRate" {
				n += raiseTTFT(t, c, d, prefillVariant)
			}
		}
	case []any:
		for _, c := range x {
			n += raiseTTFT(t, c, d, prefillVariant)
		}
	}
	return n
}

// meanBias is the backtest's mean-TTFT bias over every judged step, the
// median of predicted over measured. A step priced from a lower bound of V_P
// is counted too (whatifapi.UnscoredVPLowerBound): the backtest scores it
// apart, but leaving it out would drop the steps it prices.
func meanBias(t *testing.T, out string) (float64, int) {
	t.Helper()
	var bt whatifapi.BacktestResponse
	require.NoError(t, json.Unmarshal([]byte(out), &bt), out)
	var ratios []float64
	for _, s := range bt.Steps {
		if s.Excluded != "" {
			continue
		}
		for _, p := range s.TTFT {
			if p.Statistic != statMean || p.Measured == nil || p.Predicted.Value == nil || !(*p.Measured > 0) ||
				(p.Unscored != "" && p.Unscored != whatifapi.UnscoredVPLowerBound) {
				continue
			}
			ratios = append(ratios, *p.Predicted.Value / *p.Measured)
		}
	}
	if len(ratios) == 0 {
		t.Fatalf("no mean TTFT priced beside a measurement: %s", bt.Verdict)
	}
	slices.Sort(ratios)
	n := len(ratios)
	if n%2 == 1 {
		return ratios[n/2], n
	}
	return (ratios[n/2-1] + ratios[n/2]) / 2, n
}

// End to end on a public recording made to hold cached tokens that cost a
// known phi (costRecording): the Qwen3-32B warm-up with its prompts sixteen
// times as long, all but the sixteenth it computed served from cache, and
// each request's TTFT raised by 0.8 x cached / V_P in its queue time. The fit
// is the whole recording, read at its last cycle; the test is the same
// traffic a day later. With slo.SwitchTTFTCachedTokenCost off the mean TTFT
// reads low; on, it reads as measured. phi is fitted above 0 and under the
// 0.8 injected: a is learned from the same TTFTs at low load, so it holds the
// load at the shape it was read at, and phi is what a does not hold (here
// about half, as the prompts grow past that shape). On the recording as
// injected, with no cost raised, phi fits near 0: the term is fitted from
// the cost, not from the model's own residual.
func TestCachedTokenCost_SyntheticRecording(t *testing.T) {
	const wholeScale, phi = 16.0, 0.8
	if testing.Short() {
		t.Skip("replays the Qwen3-32B warm-up several times")
	}
	ctx := context.Background()
	on := map[string]map[string]bool{learnreplay.PackageSLO: {slo.SwitchTTFTCachedTokenCost: true, slo.SwitchTTFTComputedHit: true}}
	// V_P as the fit reads it, at the last cycle, from the recording as
	// injected.
	plain := hitRecording(t, wholeScale, 0.5)
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, b, 0o600))
		return p
	}
	plainPath := write("plain.jsonl", plain)
	st, err := learnreplay.StateAt(ctx, plain, learnreplay.Options{Switches: on}, learnreplay.StateQuery{})
	require.NoError(t, err)
	require.Len(t, st.Prefill, 1)
	ps := &st.Prefill[0]
	ok, why := queueLearnedVP(ps)
	require.True(t, ok, "the warm-up's V_P is learned under a queue: %s", why)
	vp := ps.Estimate.VP

	rec, raised := costRecording(t, wholeScale, phi, vp, ps.Variant)
	require.Positive(t, raised)
	costPath := write("cost.jsonl", rec)
	// The same traffic a day later: every time in the recording is on one
	// day.
	require.Positive(t, bytes.Count(rec, []byte("2026-11-14T")))
	laterPath := write("later.jsonl", bytes.ReplaceAll(rec, []byte("2026-11-14T"), []byte("2026-11-15T")))

	fitPhi := func(path string) whatifapi.CachedTokenCost {
		t.Helper()
		res, err := resolveBacktest(ctx, &backtestFlags{}, path, 0, io.Discard)
		require.NoError(t, err)
		require.NotNil(t, res.Inputs.CachedTokenCost, "the switch is on: the inputs name phi")
		return *res.Inputs.CachedTokenCost
	}
	bias := func() (float64, int) {
		t.Helper()
		out, code := runCmd(t, "backtest", "--fit", costPath, "--test", laterPath, "--arrival-cv2", "1", "-o", "json")
		require.Equal(t, 0, code, out)
		return meanBias(t, out)
	}

	testReplaySwitches = map[string]map[string]bool{learnreplay.PackageSLO: {slo.SwitchTTFTComputedHit: true}}
	defer func() { testReplaySwitches = nil }()
	offBias, offSteps := bias()
	testReplaySwitches = on
	base := fitPhi(plainPath)
	c := fitPhi(costPath)
	onBias, onSteps := bias()
	costSt, err := learnreplay.StateAt(ctx, rec, learnreplay.Options{Switches: on}, learnreplay.StateQuery{})
	require.NoError(t, err)
	t.Logf("V_P %.0f tok/s; a %.3f s as injected, %.3f s with the cost (%s); phi as injected: %+v; with the cost: %+v; "+
		"mean TTFT bias off %.3f (%d steps), on %.3f (%d steps)", vp, ps.Estimate.A, costSt.Prefill[0].Estimate.A,
		costSt.Prefill[0].Estimate.ASource, base, c, offBias, offSteps, onBias, onSteps)

	require.True(t, c.Fitted, c.Why)
	assert.Greater(t, c.Phi, 0.2, "phi fitted from the cost")
	assert.LessOrEqual(t, c.Phi, phi, "phi is net of the load a holds")
	assert.Less(t, base.Phi, 0.05, "no cost raised: phi near 0")
	assert.Less(t, offBias, 0.9, "off: the mean TTFT reads low")
	assert.InDelta(t, 1, onBias, 0.1, "on: the mean TTFT reads as measured")
}
