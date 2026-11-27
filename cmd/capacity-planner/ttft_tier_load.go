package main

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The load of a cached prompt token from a tier outside the GPU, as
// slo.SwitchTTFTTierLoad prices it (whatif.PromptWindow.TierLoad), and the
// attention over every tier's cached context, as
// slo.SwitchTTFTPrefixAttention prices it. Both off, nothing here runs.

// tierLoadUnmeasured is a fleet whose prefill pods served a material share
// of the prompt from a tier outside the GPU with no transfer reading that
// prices it: the load is left out, and TTFT reads low by it.
const tierLoadUnmeasured whatif.Flag = "TierLoadUnmeasured"

// tierSplitUnread is a fleet whose prefill pods computed a material share
// less than the whole prompt with no cached tokens by tier recorded: where
// the hits came from, and what they cost to load, is not known, and TTFT
// leaves any load out.
const tierSplitUnread whatif.Flag = "TierSplitUnread"

// wireTierLoad is the tier load on the wire (wireTTFT.TierLoad): the
// seconds a request waits for its cached KV, the share of the whole prompt
// served from outside the GPU, the load per request at the traffic read,
// how a was read and so how much of the load was added, and each transfer
// read.
type wireTierLoad struct {
	Seconds           float64            `json:"seconds"`
	OuterShare        float64            `json:"outerShare"`
	Shares            map[string]float64 `json:"shares"`
	MeasuredSeconds   float64            `json:"measuredSeconds"`
	FixedLatencyHolds bool               `json:"fixedLatencyHolds,omitempty"`
	Transfers         []wireTierTransfer `json:"transfers"`
}

// wireTierTransfer is whatif.TierTransfer on the wire.
type wireTierTransfer struct {
	Kind            string  `json:"kind"`
	BytesPerSecond  float64 `json:"bytesPerSecond"`
	BytesPerToken   float64 `json:"bytesPerToken"`
	SecondsPerToken float64 `json:"secondsPerToken"`
}

// tierLoad prices the load of the question's cached tokens from outside
// the GPU into the prefill side (whatif.Prefill.TierLoadSeconds). Nothing
// with the switch off.
//
// The load at the traffic read is each transfer's seconds over the tokens
// it moved, times a request's tokens of those tiers: the bytes a token
// moved over the bandwidth the transfer ran at. At the question it scales
// with the whole prompt asked, the tiers' shares held as read; a stated hit
// rate caps their share. It is priced with no overlap with the request's
// own prefill: vLLM's OffloadingConnector loads before the request is
// scheduled. A connector that loads layer by layer overlaps part of it, and
// the term is then an upper bound.
//
// Where a was read decides how much is added. a from engine prefill timing
// takes its overhead less the engine's queue time, and vLLM counts a
// request's wait for its KV load in that queue time: the load is in no
// input, and all of it is added. a from low-load minutes holds the overhead
// those minutes read, the load at their traffic among it: only the excess
// of the question's load over the traffic read is added. A stated a
// (--assume-a) is the caller's, and nothing is added.
func (pf *pricedFleet) tierLoad(ps *learnreplay.PrefillState) {
	if !pf.tierLoadOn {
		return
	}
	p := &pf.fleet.Prefill
	tl, ok := pf.promptWindow.TierLoad()
	if !ok {
		if hit, read := pf.promptWindow.ComputedHit(); read && hit > whatif.MaterialPrefixHitMove {
			pf.flags = append(pf.flags, tierSplitUnread)
			pf.assume("no cached tokens by tier recorded at a computed-token hit of %.3f: TTFT leaves out the load of any hit from outside the GPU", hit)
		}
		return
	}
	if !(tl.OuterShare >= whatif.MinTierShare) {
		return
	}
	if len(tl.Unmeasured) > 0 {
		pf.flags = append(pf.flags, tierLoadUnmeasured)
		pf.assume("tier load not priced: %s served %s of the prompt with no KV transfer read; TTFT leaves out its load",
			strings.Join(tl.Unmeasured, ", "), tierPercentOf(tl, tl.Unmeasured))
		return
	}
	share := tl.OuterShare
	if r := pf.hitAtCount; r != nil && r.Rule == prefill.HitReuseGap {
		// The share of the tiers past the GPU at the fleet's prefill count,
		// as the reuse-gap rule moved it (priceHitAtCount).
		share *= r.TierScale
	}
	if pf.pv != nil && pf.pv.assumedHit && !pf.pv.hitRead {
		share = min(share, pf.pv.shape.PrefixHitRate)
	}
	// The whole prompt of the rows and minutes the load was read over; a
	// stated one (--assume-input-tokens, a stated or load shape) in its
	// place.
	whole := tl.WholePromptTokens
	if pf.in.PromptSource == shapeAssumed && p.WholePromptTokens > 0 {
		whole = p.WholePromptTokens
	}
	// The seconds a request paid at the traffic read: the transfers', or,
	// with slo-ttft-tier-kv-wait on, the engine's measured wait for the KV
	// where that is longer: the request is scheduled after its load, at a
	// step boundary, so it waits past the transfer.
	measured, waitText := tl.SecondsPerRequest, ""
	if pf.tierKVWaitOn && tl.WaitPerRequest > tl.SecondsPerRequest && tl.SecondsPerRequest > 0 {
		measured = tl.WaitPerRequest
		waitText = fmt.Sprintf("; priced at the engine's measured wait for the KV, %s a request, %.1fx the transfer's %s",
			ms(tl.WaitPerRequest), tl.WaitPerRequest/tl.SecondsPerRequest, ms(tl.SecondsPerRequest))
	}
	perToken := measured / (tl.OuterShare * tl.WholePromptTokens)
	load := share * whole * perToken
	w := &wireTierLoad{OuterShare: tl.OuterShare, Shares: tl.Shares, MeasuredSeconds: measured,
		Transfers: make([]wireTierTransfer, 0, len(tl.Transfers))}
	for _, t := range tl.Transfers {
		w.Transfers = append(w.Transfers, wireTierTransfer(t))
	}
	pf.in.TierLoad = w
	source := fmt.Sprintf("%s of the prompt from outside the GPU (%s), %s%s", tierPercent(tl.OuterShare), tierShares(tl), transfersText(tl), waitText)
	switch {
	case pf.assumedA:
		pf.assume("tier load %s a request not added: a is stated (--assume-a); %s", ms(load), source)
		return
	case ps.Estimate.ASource != prefill.ASourceEngineTiming:
		w.FixedLatencyHolds = true
		w.Seconds = max(load-measured, 0)
		pf.assume("tier load %s a request, of which a holds the %s read at low load: %s added; %s",
			ms(load), ms(measured), ms(w.Seconds), source)
	default:
		w.Seconds = load
		pf.assume("tier load %s a request: %s; no overlap with the prefill (an upper bound where loads overlap)", ms(load), source)
	}
	p.TierLoadSeconds = w.Seconds
}

// cachedContext reads the attention span's cached context from the whole
// prompt less the computed one, on both sides (whatif.Prefill
// .CachedContextRead), with slo.SwitchTTFTPrefixAttention on, whether
// slo.SwitchTTFTTierLoad is on or not (issue
// llm-d-incubation/llm-d-scaling-manager#383): the question's from the
// computed-token hit of the same rows and minutes, or from a stated whole
// prompt and hit; V_P's from the whole and computed prompts of its own
// minutes. A prompt netted from a whole one (--assume-input-tokens, a
// backtest's step, the decode variant's shape) is read as the whole less
// the netted one, so no token is both computed and cached, and only where
// slo.SwitchTTFTComputedHit netted it by every tier's hit. A token served from a host, disk or peer tier is a miss to the
// GPU's prefix-cache hit rate, yet the prompt attends over it. Either side
// unread leaves the hit rates to it.
func (pf *pricedFleet) cachedContext() {
	p := &pf.fleet.Prefill
	if !p.PrefixAttention || !(p.VPWholePromptTokens > 0) || !(p.PromptTokens > 0) {
		return
	}
	var h float64
	switch {
	case pf.stepComputed:
		h = max(p.WholePromptTokens-p.PromptTokens, 0)
	case pf.pv != nil && pf.pv.assumedHit && !pf.pv.hitRead && p.WholePromptTokens > 0:
		h = max(p.WholePromptTokens-p.PromptTokens, 0)
	case pf.in.PromptSource != shapeMeasured:
		// A prompt netted from a whole one: netted by the prefix-cache hit
		// rate, a tier's tokens are priced as computed, and the hit rates
		// stand.
		if _, _, ok := pf.netByComputedHit(); !ok || !(p.WholePromptTokens > 0) {
			return
		}
		h = max(p.WholePromptTokens-p.PromptTokens, 0)
	default:
		hit, ok := pf.pricedComputedHit()
		if !ok {
			return
		}
		h = p.PromptTokens * hit / (1 - hit)
	}
	p.CachedContextRead = true
	p.CachedContextTokens = h
	p.VPCachedContextTokens = max(p.VPWholePromptTokens-p.VPPromptTokens, 0)
}

// tierPercent is a share in percent, to one decimal.
func tierPercent(x float64) string { return fmt.Sprintf("%.1f%%", 100*x) }

// tierPercentOf is the share of the whole prompt the named tiers served.
func tierPercentOf(tl whatif.TierLoad, tiers []string) string {
	var s float64
	for _, t := range tiers {
		s += tl.Shares[t]
	}
	return tierPercent(s)
}

// tierShares is each tier's share past the GPU, by tier.
func tierShares(tl whatif.TierLoad) string {
	var parts []string
	for _, t := range slices.Sorted(maps.Keys(tl.Shares)) {
		if t != whatif.SourceDevice && tl.Shares[t] > 0 {
			parts = append(parts, t+" "+tierPercent(tl.Shares[t]))
		}
	}
	return strings.Join(parts, ", ")
}

// transfersText is each transfer read: its bandwidth and its bytes a token.
func transfersText(tl whatif.TierLoad) string {
	parts := make([]string, 0, len(tl.Transfers))
	for _, t := range tl.Transfers {
		parts = append(parts, fmt.Sprintf("%s %.2f GB/s at %.0f bytes a token", t.Kind, t.BytesPerSecond/1e9, t.BytesPerToken))
	}
	return strings.Join(parts, ", ")
}
