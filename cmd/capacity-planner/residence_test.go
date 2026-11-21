package main

import (
	"math"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// residenceState is a P/D state (or a decode-only one) whose decode engine
// read a TTFT of 40 ms at 8000 prompt tokens, with waiting requests held
// on decode capacity.
func residenceState(pd, on bool, waiting int) *learnreplay.State {
	d := learnreplay.VariantState{Variant: "d", Role: string(domain.RoleDecode), Replicas: 2,
		Line: itl.Model{A: 0.03125, B: 0.00172}, Tier: whatif.TierOLS, CapacityTokens: 1.16e6, BatchCap: 256, Ranks: 1,
		Shape: learnreplay.MeasuredShape{InputTokens: 8000, OutputTokens: 26}}
	d.DecodeTTFT.EngineTTFT = whatif.DecodeTTFT{Seconds: 0.040, PromptTokens: 8000, Waiting: waiting}
	st := &learnreplay.State{DecodeResidencePull: on, Variants: []learnreplay.VariantState{d}}
	if pd {
		st.Variants = append(st.Variants, learnreplay.VariantState{Variant: "p", Role: string(domain.RolePrefill)})
	}
	return st
}

// With saturation.SwitchDecodeResidencePull a P/D decode's residence holds
// the decode engine's TTFT at the prompt priced (#301); off, or with no
// prefill variant, it holds nothing. A TTFT read with requests waiting on
// decode capacity is a queue that holds no KV: it is not priced, and the
// figure is flagged ResidenceUnread, as it is when the TTFT is unread or
// not finite, or the window's backlog is unread (BacklogUnread). An
// aggregated variant holds no pull.
func TestDecodeResidence_TheDecodeEngineTTFTOnTheSwitch(t *testing.T) {
	for name, c := range map[string]struct {
		pd, on   bool
		waiting  int
		unread   bool
		backlog  bool
		both     bool
		inf      bool
		want     float64
		wantFlag bool
	}{
		"P/D, on":                 {pd: true, on: true, want: 0.040},
		"P/D, off":                {pd: true},
		"decode only, on":         {on: true},
		"decode only, off":        {},
		"P/D, on, backlog":        {pd: true, on: true, waiting: 3, wantFlag: true},
		"P/D, off, backlog":       {pd: true, waiting: 3},
		"P/D, on, TTFT unread":    {pd: true, on: true, unread: true, wantFlag: true},
		"decode only, on, unread": {on: true, unread: true},
		"P/D, on, backlog unread": {pd: true, on: true, backlog: true, wantFlag: true},
		"P/D, on, aggregated":     {pd: true, on: true, both: true},
		"P/D, on, TTFT +Inf":      {pd: true, on: true, inf: true, wantFlag: true},
	} {
		t.Run(name, func(t *testing.T) {
			st := residenceState(c.pd, c.on, c.waiting)
			if c.unread {
				st.Variants[0].DecodeTTFT.EngineTTFT = whatif.DecodeTTFT{}
			}
			if c.inf {
				st.Variants[0].DecodeTTFT.EngineTTFT.Seconds = math.Inf(1)
			}
			st.Variants[0].DecodeTTFT.BacklogUnread = c.backlog
			if c.both {
				st.Variants[0].Role = string(domain.RoleBoth)
			}
			f := answerFlags{set: map[string]bool{}}
			vs, err := f.decodeVariants(st)
			require.NoError(t, err)
			require.Len(t, vs, 1)
			assert.InDelta(t, c.want, vs[0].decode.HeldBefore, 1e-12)
			assert.Equal(t, c.wantFlag, slices.Contains(vs[0].flags, residenceUnread), "flags %v", vs[0].flags)
		})
	}
}
