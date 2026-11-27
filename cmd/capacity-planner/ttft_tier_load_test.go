package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// The fixture is the Qwen3-32B staircase's warm-up with a cache tier
// injected (tierRecording): its prompts four times as long, as
// hitRecording writes them, a quarter of each computed as recorded, a
// quarter served from the GPU prefix cache and half from a host tier, whose
// load into the GPU ran at tierBytesPerToken bytes a token and
// tierSecondsPerToken seconds a token. No committed recording has a cache
// tier.
const (
	tierWholeScale      = 4.0
	tierBytesPerToken   = 65536.0
	tierSecondsPerToken = 4e-6
)

// tierRecording is hitRecording's warm-up with each row that read a
// computed-token rate and a one-minute prompt given the cached tokens by
// tier of the fixture, the GPU's a quarter of the whole prompt and the rest
// past what was computed from the host tier, and, with load, the offload
// load that moved the host tier's tokens.
func tierRecording(t *testing.T, load bool) []byte {
	t.Helper()
	in := hitRecording(t, tierWholeScale, 0.25)
	var out bytes.Buffer
	sc := bufio.NewScanner(bytes.NewReader(in))
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	rewritten := 0
	for sc.Scan() {
		dec := json.NewDecoder(bytes.NewReader(sc.Bytes()))
		dec.UseNumber()
		var ev map[string]any
		require.NoError(t, dec.Decode(&ev))
		rewritten += injectTiers(t, ev["Data"], load)
		b, err := json.Marshal(ev)
		require.NoError(t, err)
		out.Write(append(b, '\n'))
	}
	require.NoError(t, sc.Err())
	require.Positive(t, rewritten, "the warm-up has prefill rows to give a tier")
	return out.Bytes()
}

// injectTiers gives every row under v that read the rates a tier split
// (tierRecording) and returns how many it gave one.
func injectTiers(t *testing.T, v any, load bool) int {
	n := 0
	switch x := v.(type) {
	case map[string]any:
		if rows, ok := x["ReplicaMetrics"].([]any); ok {
			for _, r := range rows {
				m, ok := r.(map[string]any)
				if !ok {
					continue
				}
				rate, computed, whole := jsonFloat(t, m["RequestRate"]), jsonFloat(t, m["PrefillComputedTokenRate"]), jsonFloat(t, m["AvgInputTokensRecent"])
				if !(rate > 0) || !(computed > 0) || !(whole > 0) {
					continue
				}
				device := 0.25 * rate * whole
				host := max(rate*whole-computed-device, 0)
				m["CachedTokenRateBySource"] = []any{
					map[string]any{"Source": whatif.SourceDevice, "Rate": device},
					map[string]any{"Source": whatif.SourceHost, "Rate": host},
				}
				if load {
					m["KVTransferRates"] = []any{map[string]any{"Kind": domain.KVTransferOffloadLoad,
						"ByteRate": host * tierBytesPerToken, "SecondsRate": host * tierSecondsPerToken}}
				}
				n++
			}
		}
		for _, c := range x {
			n += injectTiers(t, c, load)
		}
	case []any:
		for _, c := range x {
			n += injectTiers(t, c, load)
		}
	}
	return n
}

// jsonFloat is a decoded JSON number, 0 where it is not one.
func jsonFloat(t *testing.T, v any) float64 {
	t.Helper()
	num, ok := v.(json.Number)
	if !ok {
		return 0
	}
	f, err := num.Float64()
	require.NoError(t, err)
	return f
}

// tierState is rec's state at its last cycle, with no switch on: the
// planner reads slo.SwitchTTFTTierLoad from the prefill state alone
// (TestStateAt_TheTierLoadSwitchReachesThePrefillState), so each test turns
// it on a copy (tierFleet) and replays the recording once.
func tierState(t *testing.T, rec []byte) *learnreplay.State {
	t.Helper()
	st, err := learnreplay.StateAt(context.Background(), rec, learnreplay.Options{}, learnreplay.StateQuery{})
	require.NoError(t, err)
	require.Len(t, st.Prefill, 1)
	return st
}

// tierFleet is the decode variant's fleet of st with slo.SwitchTTFTTierLoad
// as on says, slo.SwitchTTFTPrefixAttention as attention says, and the
// prefill variant's a read from engine timing where engineTiming says so,
// as on a fleet with no quiet minutes; at the knobs given. st is not
// changed.
func tierFleet(t *testing.T, st *learnreplay.State, on, attention, engineTiming bool, k whatifapi.Knobs) *servedFleet {
	t.Helper()
	cp := *st
	cp.Prefill = slices.Clone(st.Prefill)
	cp.Prefill[0].TierLoad, cp.Prefill[0].PrefixAttention = on, attention
	if engineTiming {
		cp.Prefill[0].Estimate.ASource = prefill.ASourceEngineTiming
	}
	var dec string
	for _, v := range cp.Variants {
		if domain.CanonicalRole(v.Role) == domain.RoleDecode {
			dec = v.Variant
		}
	}
	require.NotEmpty(t, dec)
	s := &servedState{st: &cp, decodeVariant: dec, prefillVariant: cp.Prefill[0].Variant}
	sf, err := s.fleetAt(k)
	require.NoError(t, err)
	require.Nil(t, sf.pf.refused)
	return sf
}

// meanTTFT is the fleet's mean TTFT at half its prefill knee.
func meanTTFT(t *testing.T, sf *servedFleet) whatif.TTFT {
	t.Helper()
	knee, ok := sf.PrefillKnee().(whatif.Answer)
	require.True(t, ok, "%#v", sf.PrefillKnee())
	v, ok := sf.TTFTAt(knee.Value / 2).(whatif.TTFT)
	require.True(t, ok, "%#v", sf.TTFTAt(knee.Value/2))
	return v
}

// injectedLoad is the load a request paid in the fixture: its host tier's
// tokens times tierSecondsPerToken, over the prefill window's rows.
func injectedLoad(t *testing.T, sf *servedFleet) float64 {
	t.Helper()
	tl, ok := sf.pf.promptWindow.TierLoad()
	require.True(t, ok)
	return tl.Shares[whatif.SourceHost] * tl.WholePromptTokens * tierSecondsPerToken
}

// End to end on the warm-up with a host tier injected, its a from engine
// timing as on a fleet that never goes quiet: with slo.SwitchTTFTTierLoad
// on, the mean TTFT carries the load the recording measured, each host
// token's seconds; off, it reads low by exactly that. The prefill knee does
// not move: the load is a request's own wait, not the pod's work.
func TestTTFT_TierLoadMatchesTheInjectedLoad(t *testing.T) {
	t.Parallel()
	rec := tierState(t, tierRecording(t, true))
	off := tierFleet(t, rec, false, false, true, whatifapi.Knobs{Headroom: 0.8})
	on := tierFleet(t, rec, true, false, true, whatifapi.Knobs{Headroom: 0.8})
	want := injectedLoad(t, on)
	require.Greater(t, want, 0.01, "the fixture's load is material")

	tOff, tOn := meanTTFT(t, off), meanTTFT(t, on)
	t.Logf("injected load %.1f ms; mean TTFT off %.1f ms, on %.1f ms", 1000*want, 1000*tOff.Seconds, 1000*tOn.Seconds)
	assert.InDelta(t, want, tOn.Parts.TierLoad, 1e-9, "on: the measured load, each token's bytes over the bandwidth read")
	assert.InDelta(t, want, tOn.Seconds-tOff.Seconds, 1e-6, "off reads low by the load")
	assert.Zero(t, tOff.Parts.TierLoad)
	assert.InDelta(t, off.PrefillKnee().(whatif.Answer).Value, on.PrefillKnee().(whatif.Answer).Value, 1e-12)

	require.NotNil(t, on.pf.in.TierLoad)
	assert.InDelta(t, want, on.pf.in.TierLoad.Seconds, 1e-9)
	require.Len(t, on.pf.in.TierLoad.Transfers, 1)
	assert.InDelta(t, tierBytesPerToken, on.pf.in.TierLoad.Transfers[0].BytesPerToken, 1e-6)
	assert.InDelta(t, tierBytesPerToken/tierSecondsPerToken, on.pf.in.TierLoad.Transfers[0].BytesPerSecond, 1e-3)
	assumed := strings.Join(on.pf.in.Assumed, "; ")
	assert.Contains(t, assumed, "tier load")
	assert.Contains(t, assumed, "no overlap with the prefill")
	assert.Nil(t, off.pf.in.TierLoad)
	assert.NotContains(t, strings.Join(off.pf.in.Assumed, "; "), "tier load")

}

// With a read at low load, a holds the load of the traffic it was read at:
// at the measured shape nothing is added, and a prompt asked twice as long
// adds the second half of its load.
func TestTTFT_TierLoadLowLoadAHoldsTheMeasuredLoad(t *testing.T) {
	t.Parallel()
	rec := tierState(t, tierRecording(t, true))
	on := tierFleet(t, rec, true, false, false, whatifapi.Knobs{Headroom: 0.8})
	require.NotEqual(t, sourceEngineTiming, on.pf.in.FixedLatencySource, "the warm-up's a is read at low load")
	off := tierFleet(t, rec, false, false, false, whatifapi.Knobs{Headroom: 0.8})
	want := injectedLoad(t, on)
	assert.InDelta(t, meanTTFT(t, off).Seconds, meanTTFT(t, on).Seconds, 1e-9, "a holds the measured load")
	require.NotNil(t, on.pf.in.TierLoad)
	assert.True(t, on.pf.in.TierLoad.FixedLatencyHolds)
	assert.Contains(t, strings.Join(on.pf.in.Assumed, "; "), "of which a holds")

	tl, _ := on.pf.promptWindow.TierLoad()
	k := whatifapi.Knobs{Headroom: 0.8, PromptTokens: 2 * tl.WholePromptTokens}
	on2, off2 := tierFleet(t, rec, true, false, false, k), tierFleet(t, rec, false, false, false, k)
	assert.InDelta(t, want, on2.pf.fleet.Prefill.TierLoadSeconds, 1e-6*want, "twice the prompt: the second load added")
	assert.InDelta(t, want, meanTTFT(t, on2).Seconds-meanTTFT(t, off2).Seconds, 1e-6)
}

// No bandwidth is guessed: a tier serving the prompt with no transfer read
// leaves the load out, flagged TierLoadUnmeasured; a recording with no
// split by tier where the engines computed far less than the prompt is
// flagged TierSplitUnread. Either way the figure is the switch-off one.
func TestTTFT_TierLoadUnmeasuredIsFlagged(t *testing.T) {
	t.Parallel()
	noLoad := tierState(t, tierRecording(t, false))
	on := tierFleet(t, noLoad, true, false, true, whatifapi.Knobs{Headroom: 0.8})
	off := tierFleet(t, noLoad, false, false, true, whatifapi.Knobs{Headroom: 0.8})
	assert.Contains(t, on.pf.flags, tierLoadUnmeasured)
	assert.NotContains(t, off.pf.flags, tierLoadUnmeasured)
	assert.Zero(t, on.pf.fleet.Prefill.TierLoadSeconds)
	assert.InDelta(t, meanTTFT(t, off).Seconds, meanTTFT(t, on).Seconds, 1e-12)
	assert.Contains(t, strings.Join(on.pf.in.Assumed, "; "), "tier load not priced: host served")

	split := tierState(t, hitRecording(t, tierWholeScale, 0.25))
	on = tierFleet(t, split, true, false, true, whatifapi.Knobs{Headroom: 0.8})
	off = tierFleet(t, split, false, false, true, whatifapi.Knobs{Headroom: 0.8})
	assert.Contains(t, on.pf.flags, tierSplitUnread)
	assert.NotContains(t, off.pf.flags, tierSplitUnread)
	assert.InDelta(t, meanTTFT(t, off).Seconds, meanTTFT(t, on).Seconds, 1e-12)

	// The public warm-up as committed: no hit, no split, nothing moves and
	// nothing is flagged.
	plain := tierState(t, hitRecording(t, 1, 0))
	on = tierFleet(t, plain, true, false, false, whatifapi.Knobs{Headroom: 0.8})
	off = tierFleet(t, plain, false, false, false, whatifapi.Knobs{Headroom: 0.8})
	assert.NotContains(t, on.pf.flags, tierSplitUnread)
	assert.NotContains(t, on.pf.flags, tierLoadUnmeasured)
	assert.Equal(t, off.pf.in.Assumed, on.pf.in.Assumed)
	assert.InDelta(t, meanTTFT(t, off).Seconds, meanTTFT(t, on).Seconds, 1e-12)
}

// With slo.SwitchTTFTPrefixAttention on too, the attention span's cached
// context is the whole prompt less the computed one, from the computed-token
// hit of the same rows, not the GPU's prefix-cache hit rate, which the
// fixture reads at 0.25 against a computed-token hit of 0.75.
func TestTTFT_TierLoadAttentionSpanFromTheComputedHit(t *testing.T) {
	t.Parallel()
	st := tierState(t, tierRecording(t, true))
	sf := tierFleet(t, st, true, true, false, whatifapi.Knobs{Headroom: 0.8})
	p := sf.pf.fleet.Prefill
	hit, ok := st.Prefill[0].PromptWindow.ComputedHit()
	require.True(t, ok)
	require.InDelta(t, 1-1/tierWholeScale, hit, 0.01, "the fixture computes a quarter of the prompt")
	require.Positive(t, p.VPWholePromptTokens, "V_P's whole prompt is read on the warm-up")
	require.True(t, p.CachedContextRead)
	assert.InDelta(t, p.PromptTokens*hit/(1-hit), p.CachedContextTokens, 1e-9)
	assert.InDelta(t, max(p.VPWholePromptTokens-p.VPPromptTokens, 0), p.VPCachedContextTokens, 1e-9)

	// With slo.SwitchTTFTPrefixAttention off, the hit rates stand.
	assert.False(t, tierFleet(t, st, true, false, false, whatifapi.Knobs{Headroom: 0.8}).pf.fleet.Prefill.CachedContextRead)
}

// The cached context is slo.SwitchTTFTPrefixAttention's, with
// slo.SwitchTTFTTierLoad off too (issue
// llm-d-incubation/llm-d-scaling-manager#383): the prefix-cache counter of
// the fixture sees a third of the cached tokens (a GPU hit rate of 0.25
// against a computed-token hit of 0.75), and the span reads them all, the
// computed-token hit's, not the hit rate's. It needs no split by tier: the
// recording with none reads the same.
func TestTTFT_PrefixAttentionReadsEveryTiersCachedContext(t *testing.T) {
	t.Parallel()
	for name, rec := range map[string][]byte{
		"tier split":    tierRecording(t, true),
		"no tier split": hitRecording(t, tierWholeScale, 0.25),
	} {
		st := tierState(t, rec)
		p := tierFleet(t, st, false, true, false, whatifapi.Knobs{Headroom: 0.8}).pf.fleet.Prefill
		hit, ok := st.Prefill[0].PromptWindow.ComputedHit()
		require.True(t, ok, name)
		require.InDelta(t, 1-1/tierWholeScale, hit, 0.01, name)
		require.Less(t, p.PromptHitRate, 0.5, "%s: the prefix-cache hit rate sees part of the cache", name)
		require.True(t, p.CachedContextRead, name)
		assert.InDelta(t, p.PromptTokens*hit/(1-hit), p.CachedContextTokens, 1e-9, name)
		assert.Greater(t, p.CachedContextTokens, 2*p.PromptTokens*p.PromptHitRate/(1-p.PromptHitRate),
			"%s: more than the hit rate's cached context", name)
		assert.InDelta(t, max(p.VPWholePromptTokens-p.VPPromptTokens, 0), p.VPCachedContextTokens, 1e-9, name)
	}
}

// A prompt the question states (--assume-input-tokens, a backtest's step)
// is netted from a whole one, and its cached context is the whole prompt
// less the netted one, so no token is both computed and cached. Netted by
// the prefix-cache hit rate, the host tier's tokens are priced as computed:
// the span reads no cached context, and the hit rates stand on both sides,
// like with like. Netted by every tier's hit (slo.SwitchTTFTComputedHit),
// the span reads that hit's cached context.
func TestTTFT_PrefixAttentionStatedPromptCountsEachTokenOnce(t *testing.T) {
	t.Parallel()
	st := tierState(t, tierRecording(t, true))
	whole := tierFleet(t, st, false, true, false, whatifapi.Knobs{Headroom: 0.8}).pf.fleet.Prefill.WholePromptTokens
	require.Positive(t, whole, "the fixture reads the whole prompt")
	knobs := whatifapi.Knobs{Headroom: 0.8, PromptTokens: whole}

	p := tierFleet(t, st, false, true, false, knobs).pf.fleet.Prefill
	require.InDelta(t, whole, p.WholePromptTokens, 1e-9)
	require.InDelta(t, whole*(1-p.PromptHitRate), p.PromptTokens, 1e-6, "netted by the prefix-cache hit rate")
	assert.False(t, p.CachedContextRead, "cached context %.0f over a computed %.0f of a %.0f prompt",
		p.CachedContextTokens, p.PromptTokens, whole)

	cp := *st
	cp.Prefill = slices.Clone(st.Prefill)
	cp.Prefill[0].ComputedHit = true
	p = tierFleet(t, &cp, false, true, false, knobs).pf.fleet.Prefill
	hit, ok := st.Prefill[0].PromptWindow.ComputedHit()
	require.True(t, ok)
	require.InDelta(t, whole*(1-hit), p.PromptTokens, 1e-6, "netted by the computed-token hit")
	require.True(t, p.CachedContextRead)
	assert.InDelta(t, whole*hit, p.CachedContextTokens, 1e-6)
	assert.InDelta(t, whole, p.PromptTokens+p.CachedContextTokens, 1e-6)
}
