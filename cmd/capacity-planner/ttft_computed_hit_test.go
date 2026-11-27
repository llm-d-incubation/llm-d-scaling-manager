package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/slo"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// computedHitWindow is a prefill prompt window whose rows computed the given
// number of tokens of each 8000-token prompt, one request a second, over the
// computed rate's minute.
func computedHitWindow(computed float64) *whatif.PromptWindow {
	const whole = 8000
	w := &whatif.PromptWindow{}
	for i := range whatif.PromptWindowMinCycles {
		w.Note([]domain.ReplicaMetrics{{Ready: true, PrefillComputedTokenRate: computed, PrefillComputedTokenRateKnown: true,
			RequestRate: 1, RequestRateKnown: true, AvgInputTokensRecent: whole}}, i, time.Unix(int64(15*i), 0))
	}
	return w
}

// computedHitState is hitState with the prefill pods computing a quarter of
// an 8000-token prompt (a 0.75 computed-token hit) where the prefix-cache
// counter reads prefillHit; on is slo.SwitchTTFTComputedHit as the replay
// read it.
func computedHitState(prefillHit float64, on bool) *learnreplay.State {
	st := hitState(prefillHit, false)
	st.Prefill[0].PromptWindow = computedHitWindow(2000)
	st.Prefill[0].ComputedHit = on
	return st
}

// A whole prompt priced for the prefill pods is netted by the decode
// variant's prefix-cache hit, as before; with slo.SwitchTTFTComputedHit on,
// by the prefill pods' computed-token hit, which counts the hits of every
// tier: 8000 tokens at a 0.75 computed-token hit are the 2000 the engines
// computed, not the 4000 the 0.5 prefix-cache hit leaves. The hit rate
// beside the prompt stays the prefill variant's prefix-cache one, which
// V_P's compares with, and the inputs name both.
func TestTTFT_ComputedHitNetsTheWholePrompt(t *testing.T) {
	off := hitFleetOf(t, computedHitState(0.5, false), hitDecode(0.5, 0), answerFlags{inputTokens: 8000}, "assume-input-tokens")
	require.Nil(t, off.refused)
	assert.Equal(t, 4000.0, off.fleet.Prefill.PromptTokens, "off: the decode variant's prefix-cache hit")
	assert.NotContains(t, strings.Join(off.in.Assumed, "; "), "computed-token hit")

	on := hitFleetOf(t, computedHitState(0.5, true), hitDecode(0.5, 0), answerFlags{inputTokens: 8000}, "assume-input-tokens")
	require.Nil(t, on.refused)
	assert.InDelta(t, 2000.0, on.fleet.Prefill.PromptTokens, 1e-9, "on: what the prefill pods computed")
	assert.Equal(t, 0.5, on.fleet.Prefill.PromptHitRate, "the prefix-cache hit beside it, as V_P's is read")
	assert.Equal(t, 8000.0, on.fleet.Prefill.WholePromptTokens)
	assert.Contains(t, on.in.Assumed, "prompt netted by the prefill pods' computed-token hit 0.750, not the prefix-cache hit 0.500")

	// With slo.SwitchTTFTPrefixAttention on too, the hit replaced is the
	// prefill variant's, and the prompt is the same.
	both := computedHitState(0.5, true)
	both.Prefill[0].PrefixAttention = true
	pf := hitFleetOf(t, both, hitDecode(0.9, 0), answerFlags{inputTokens: 8000}, "assume-input-tokens")
	assert.InDelta(t, 2000.0, pf.fleet.Prefill.PromptTokens, 1e-9)
}

// A hit the question states governs (--prefix-hit, an IL/OL/hit shape); one
// the load's cycles filled in (--shape load) was read, and the computed-token
// hit replaces it.
func TestTTFT_ComputedHitAStatedHitGoverns(t *testing.T) {
	stated := func(read bool) *pricedVariant {
		pv := hitDecode(0.5, 0)
		pv.assumedHit, pv.hitRead = true, read
		return pv
	}
	pf := hitFleetOf(t, computedHitState(0.5, true), stated(false), answerFlags{inputTokens: 8000}, "assume-input-tokens")
	assert.Equal(t, 4000.0, pf.fleet.Prefill.PromptTokens, "a stated hit governs")
	pf = hitFleetOf(t, computedHitState(0.5, true), stated(true), answerFlags{inputTokens: 8000}, "assume-input-tokens")
	assert.InDelta(t, 2000.0, pf.fleet.Prefill.PromptTokens, 1e-9, "a hit the load's cycles read does not")
}

// The load's shape (--shape load, the report's default) nets its whole
// prompt by the prefill pods' computed-token hit with
// slo.SwitchTTFTComputedHit off too: on a P/D fleet the hit it carries is
// the decode variant's, none here, and netted by that the 8000-token prompt
// was priced as computed where the pods computed 2000. The prompt is then in
// the pods' computed tokens, so the served work stays the knee's floor. A
// stated shape is as before: its hit governs, and no floor is kept.
func TestTTFT_TheLoadShapeNetsByThePrefillPodsComputedHit(t *testing.T) {
	served := map[int]float64{1: 6000}
	state := func() *learnreplay.State {
		st := computedHitState(0, false)
		st.Prefill[0].ServedComputed = served
		return st
	}
	asked := func(read bool) *pricedVariant {
		pv := hitDecode(0, 0)
		pv.assumedHit, pv.hitRead = true, read
		return pv
	}

	load := hitFleetOf(t, state(), asked(true), answerFlags{inputTokens: 8000}, "assume-input-tokens")
	require.Nil(t, load.refused)
	assert.InDelta(t, 2000.0, load.fleet.Prefill.PromptTokens, 1e-9, "the load's shape: what the prefill pods computed")
	assert.Equal(t, 8000.0, load.fleet.Prefill.WholePromptTokens)
	assert.Contains(t, load.in.Assumed, "prompt netted by the prefill pods' computed-token hit 0.750, not the prefix-cache hit 0.000")
	assert.Equal(t, served, load.servedComputed, "the served work is the floor at the load's computed prompt")

	stated := hitFleetOf(t, state(), asked(false), answerFlags{inputTokens: 8000}, "assume-input-tokens")
	require.Nil(t, stated.refused)
	assert.Equal(t, 8000.0, stated.fleet.Prefill.PromptTokens, "a stated hit governs")
	assert.Nil(t, stated.servedComputed)

	plain := hitFleetOf(t, state(), hitDecode(0, 0), answerFlags{inputTokens: 8000}, "assume-input-tokens")
	assert.Equal(t, 8000.0, plain.fleet.Prefill.PromptTokens, "--assume-input-tokens with the switch off: as before")
	assert.Nil(t, plain.servedComputed)
}

// A whole prompt read from the source's cycles with no hit stated (a day
// plan's bucket, a backtest's step: whatifapi.Knobs.PromptTokensRead) is the
// source's shape too: netted by the prefill pods' computed-token hit with
// slo.SwitchTTFTComputedHit off, and the served work stays the knee's floor.
// Netted by the decode variant's hit, none on a P/D fleet, the 8000-token
// prompt was priced as computed where the pods computed 2000. A hit stated
// beside it governs, and a stated prompt is as before.
func TestTTFT_AReadPromptNetsByThePrefillPodsComputedHit(t *testing.T) {
	served := map[int]float64{1: 6000}
	state := func() *learnreplay.State {
		st := computedHitState(0, false)
		st.Prefill[0].ServedComputed = served
		return st
	}
	asked := func(read, statedHit bool) *pricedVariant {
		pv := hitDecode(0, 0)
		pv.assumedIL, pv.ilRead, pv.assumedHit = true, read, statedHit
		return pv
	}

	read := hitFleetOf(t, state(), asked(true, false), answerFlags{inputTokens: 8000}, "assume-input-tokens")
	require.Nil(t, read.refused)
	assert.InDelta(t, 2000.0, read.fleet.Prefill.PromptTokens, 1e-9, "a read prompt: what the prefill pods computed")
	assert.Equal(t, 8000.0, read.fleet.Prefill.WholePromptTokens)
	assert.Contains(t, read.in.Assumed, "prompt netted by the prefill pods' computed-token hit 0.750, not the prefix-cache hit 0.000")
	assert.Equal(t, served, read.servedComputed, "the served work is the floor at the read prompt's computed tokens")

	hit := hitFleetOf(t, state(), asked(true, true), answerFlags{inputTokens: 8000}, "assume-input-tokens")
	require.Nil(t, hit.refused)
	assert.Equal(t, 8000.0, hit.fleet.Prefill.PromptTokens, "a stated hit governs a read prompt")
	assert.Nil(t, hit.servedComputed)

	stated := hitFleetOf(t, state(), asked(false, false), answerFlags{inputTokens: 8000}, "assume-input-tokens")
	require.Nil(t, stated.refused)
	assert.Equal(t, 8000.0, stated.fleet.Prefill.PromptTokens, "a stated prompt with the switch off: as before")
	assert.Nil(t, stated.servedComputed)
}

// Below whatif.MaterialPrefixHitMove on both sides, the two hits agree to
// within what a minute's counters resolve, and nothing moves: no committed
// recording has a prefix hit, and none of their figures moves with the
// switch on. Unread, the prefix-cache hit stands.
func TestTTFT_ComputedHitNeedsAMaterialHit(t *testing.T) {
	st := computedHitState(0, true)
	st.Prefill[0].PromptWindow = computedHitWindow(7600)
	pf := hitFleetOf(t, st, hitDecode(0, 0), answerFlags{inputTokens: 8000}, "assume-input-tokens")
	assert.Equal(t, 8000.0, pf.fleet.Prefill.PromptTokens, "a 0.05 computed-token hit against none: as before")

	unread := computedHitState(0.5, true)
	unread.Prefill[0].PromptWindow = measuredWindow()
	pf = hitFleetOf(t, unread, hitDecode(0.5, 0), answerFlags{inputTokens: 8000}, "assume-input-tokens")
	assert.Equal(t, 4000.0, pf.fleet.Prefill.PromptTokens, "no one-minute prompt read: as before")
}

// hitRecording writes the Qwen3-32B staircase's warm-up (q32Recording) with
// prefix hits the GPU prefix-cache counter only partly sees: every row's
// whole prompt is wholeScale times what it served, its computed tokens are
// as recorded, and its prefix-cache hit rate reads gpuHit. A fleet whose
// prompts are wholeScale times longer and served from cache past what the
// counter sees (an offload tier, a KV connector) reads so. It returns the
// recording, uncompressed, and the rows rewritten.
func hitRecording(t *testing.T, wholeScale, gpuHit float64) []byte {
	t.Helper()
	in, err := os.Open(q32Recording)
	require.NoError(t, err)
	defer func() { _ = in.Close() }()
	zr, err := gzip.NewReader(in)
	require.NoError(t, err)
	var out bytes.Buffer
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	rewritten := 0
	for sc.Scan() {
		// Numbers stay as written, as lowerBoundRecording keeps them.
		dec := json.NewDecoder(bytes.NewReader(sc.Bytes()))
		dec.UseNumber()
		var ev map[string]any
		require.NoError(t, dec.Decode(&ev))
		rewritten += rewriteHits(t, ev["Data"], wholeScale, gpuHit)
		b, err := json.Marshal(ev)
		require.NoError(t, err)
		out.Write(append(b, '\n'))
	}
	require.NoError(t, sc.Err())
	require.Positive(t, rewritten, "the warm-up has rows to rewrite")
	return out.Bytes()
}

// rewriteHits rewrites every analyzer input's rows under v (hitRecording)
// and returns how many it rewrote.
func rewriteHits(t *testing.T, v any, wholeScale, gpuHit float64) int {
	n := 0
	switch x := v.(type) {
	case map[string]any:
		if rows, ok := x["ReplicaMetrics"].([]any); ok {
			for _, r := range rows {
				m, ok := r.(map[string]any)
				if !ok {
					continue
				}
				scaled := false
				for _, k := range []string{"AvgInputTokens", "AvgInputTokensRecent"} {
					num, ok := m[k].(json.Number)
					if !ok {
						continue
					}
					f, err := num.Float64()
					require.NoError(t, err)
					if f > 0 {
						m[k] = f * wholeScale
						scaled = true
					}
				}
				if scaled {
					m["PrefixCacheHitRate"] = gpuHit
					n++
				}
			}
		}
		for _, c := range x {
			n += rewriteHits(t, c, wholeScale, gpuHit)
		}
	case []any:
		for _, c := range x {
			n += rewriteHits(t, c, wholeScale, gpuHit)
		}
	}
	return n
}

// End to end on a public recording with prefix hits injected (the warm-up's
// prompts four times as long, all but the quarter it computed served from
// cache, a prefix-cache counter that sees half): asked the load's shape, as
// --shape load asks it (the whole prompt, at the decode variant's hit as the
// load's cycles read it), the prefill knee matches the knee at the measured
// shape with slo.SwitchTTFTComputedHit on or off. The load's shape is the
// report's default, so off it must not price the prompt the pods' caches
// served as computed: that priced the knee of a P/D fleet with a host tier
// at a third of the rate its own window served.
func TestTTFT_ComputedHitTheLoadShapeMatchesTheMeasured(t *testing.T) {
	const wholeScale, gpuHit = 4.0, 0.5
	rec := hitRecording(t, wholeScale, gpuHit)
	var read float64
	knees := func(on bool) (measured, load float64, st *learnreplay.State) {
		t.Helper()
		sw := map[string]map[string]bool{learnreplay.PackageSLO: {slo.SwitchTTFTComputedHit: on}}
		st, err := learnreplay.StateAt(context.Background(), rec, learnreplay.Options{Switches: sw}, learnreplay.StateQuery{})
		require.NoError(t, err)
		// The served-work floor (prefill-vp-served-unqueued) raises a
		// lower-bound knee at the measured prompt only; the knees compared
		// here are the ones V_P gives.
		for i := range st.Prefill {
			st.Prefill[i].ServedComputed, st.Prefill[i].ServedComputedWindow = nil, nil
		}
		var dec learnreplay.VariantState
		for _, v := range st.Variants {
			if domain.CanonicalRole(v.Role) == domain.RoleDecode {
				dec = v
			}
		}
		require.NotEmpty(t, dec.Variant)
		require.Len(t, st.Prefill, 1)
		s := &servedState{st: st, decodeVariant: dec.Variant, prefillVariant: st.Prefill[0].Variant}
		knee := func(k whatifapi.Knobs) float64 {
			t.Helper()
			f, err := s.Fleet(k)
			require.NoError(t, err)
			a, ok := f.PrefillKnee().(whatif.Answer)
			require.True(t, ok, "%#v", f.PrefillKnee())
			return a.Value
		}
		hit := dec.Shape.PrefixHitRate
		// A day plan's bucket and a backtest's step state the whole
		// prompt they read and no hit (Knobs.PromptTokensRead).
		read = knee(whatifapi.Knobs{Headroom: 0.8, PromptTokens: dec.Shape.InputTokens, PromptTokensRead: true})
		return knee(whatifapi.Knobs{Headroom: 0.8}), knee(whatifapi.Knobs{Headroom: 0.8, PromptTokens: dec.Shape.InputTokens, PrefixHit: &hit, PrefixHitRead: true}), st
	}
	offMeasured, offLoad, st := knees(false)
	computed, ok := st.Prefill[0].PromptWindow.ComputedHit()
	require.True(t, ok)
	require.InDelta(t, 1-1/wholeScale, computed, 0.01, "the injected recording reads a 0.75 computed-token hit")
	assert.InDelta(t, offMeasured, offLoad, 0.02*offMeasured, "off: the load's shape is the measured one's work")
	assert.InDelta(t, offMeasured, read, 0.02*offMeasured, "off: a read prompt is the measured one's work")

	onMeasured, onLoad, _ := knees(true)
	t.Logf("computed-token hit %.4f; prefill knee measured %.3f, load shape off %.3f, on %.3f r/s", computed, onMeasured, offLoad, onLoad)
	assert.InDelta(t, offMeasured, onMeasured, 1e-9*offMeasured, "the measured shape does not move")
	assert.InDelta(t, onMeasured, onLoad, 0.02*onMeasured, "on: the load's shape is the measured one's work")
}
