package learnreplay

import (
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/balance"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// What a recording says of a TTFT question at a cycle: the what-if planner's
// TTFT half (docs/proposals/slo-capacity-planner.md, "Recording mode, as
// built"). P1's estimate is read from the saturation analyzer's stash, the
// same handle the slo analyzer reads it through, and the rest from the
// cycles' rows by the rules of docs/proposals/slo-tiers.md, "S1 decisions".
// All of it is read, none of it is fed back: the analyzers run as they do
// without it, so it needs no replay switch.

// PrefillState is one prefill variant at the cycle, as a TTFT question reads
// it.
type PrefillState struct {
	Variant string
	// Pods is N_p, the serving pods at the cycle (prefill.Serving), the rule
	// P1 counts its per-count f by.
	Pods int
	// Estimate is P1's estimate of the variant, written at this cycle;
	// EstimateRead is false when none was (the observation was off, or the
	// cycle gave it no rows).
	Estimate     prefill.Estimate
	EstimateRead bool
	// PromptTokens is E[I_eff], computed tokens over requests, over the
	// variant's latest cycles with traffic (whatif.PromptWindow), so that a recording
	// that ends idle is priced at the traffic it last served; 0 with none.
	// PromptFromCycle, PromptToCycle and PromptCycles say which cycles.
	PromptTokens                                 float64
	PromptFromCycle, PromptToCycle, PromptCycles int
	// PromptWindow is those cycles, as the slo analyzer keeps them: its
	// Spread is the spread of that prompt, assumed when the window is nil or
	// holds no computed-token histogram, as a recording made before the
	// collector read one does not.
	PromptWindow *whatif.PromptWindow
	// PromptSupport is slo.SwitchTTFTPromptSupport: the longest prompt the
	// window's histogram allows (whatif.PromptSpread.MaxRatio) is priced.
	PromptSupport bool
	// PrefixAttention is slo.SwitchTTFTPrefixAttention: the planner prices a
	// prefix hit's cost (whatif.Prefill.PrefixAttention, the prefill
	// variant's hit on the prefill work, the decode variant's on the KV
	// transfer). Off unless the replay's options turn it on.
	PrefixAttention bool
	// ComputedHit is slo.SwitchTTFTComputedHit: the planner nets a whole
	// prompt it prices by the hit PromptWindow.ComputedHit reads, not by the
	// prefix-cache hit rate. Off unless the replay's options turn it on.
	ComputedHit bool
	// TierLoad is slo.SwitchTTFTTierLoad: the planner prices the load of a
	// cached token from a tier outside the GPU (PromptWindow.TierLoad), and
	// reads the attention span from the whole prompt less the computed one.
	// Off unless the replay's options turn it on.
	TierLoad bool
	// TierKVWait is slo.SwitchTTFTTierKVWait: with TierLoad, the planner
	// prices the engine's measured wait for a cached KV
	// (PromptWindow.TierLoad's WaitPerRequest). Off unless the replay's
	// options turn it on.
	TierKVWait bool
	// PSByEngine is slo.SwitchTTFTPSByEngine: the planner prices the prefill
	// queue as processor sharing where the engine's arguments say it shares
	// its steps. Off unless the replay's options turn it on.
	PSByEngine bool
	// CachedTokenCost is slo.SwitchTTFTCachedTokenCost: the planner fits
	// phi, the cost of a cached prompt token over a computed one, on the
	// source's steps, and prices phi x cached / V_P in a request's own
	// latency. Off unless the replay's options turn it on.
	CachedTokenCost bool
	// RawShare is f_raw, the busiest pod's share of the computed-token rate
	// over the mean share, not de-biased: the largest over the latest
	// whatif.TTFTWindowCycles cycles that read one at the cycle's pod count. A pod with
	// no rate counts as a pod that computed nothing. 0 with none, or one pod.
	// RawShareLeast is the window's least, and RawShareFull says it holds
	// whatif.TTFTWindowCycles readings: what the routing-bound flag reads. The
	// slo-tiers rule reads settled rows; here a pod still warming up reads
	// low, which raises the share, the side that rule permits.
	RawShare, RawShareLeast float64
	RawShareFull            bool
	// ServedComputed is, at each pod count, the highest mean computed-token
	// rate the variant's serving pods sustained over ServedWorkWindow in the
	// last prefill.Retention (servedWork.peaks): work the pods were seen to
	// compute, a lower bound on what that many can. Read only with
	// saturation.SwitchPrefillVPServedUnqueued on; nil otherwise.
	ServedComputed map[int]float64
	// VPAtServedWork is saturation.SwitchPrefillVPAtServedWork: the planner
	// prices a TTFT from V_P raised with a knee floored at the served work.
	// Off unless the replay's options turn it on.
	VPAtServedWork bool
	// HitAtCount is saturation.SwitchPrefillHitAtCount: the planner prices
	// the prefix hit at a prefill count other than the fit's
	// (prefill.HitAtCountOf). Off unless the replay's options turn it on.
	HitAtCount bool
	// ServedComputedWindow is ServedComputed over the whole window replayed,
	// read with VPAtServedWork alone (servedWork.windowPeaks); nil otherwise.
	ServedComputedWindow map[int]float64
	// ServedUnsteady is, by pod count, whether the prefill pods served a
	// stretch of ServedWorkWindow there, since prefill.Retention, whose
	// backlog was read and grew, and ServedComputed holds none that did not
	// (servedWork.peaksUnsteady): served work not sustained, which floors no
	// knee. A backlog not read gives no entry. Read with ServedComputed.
	ServedUnsteady map[int]bool
	// WindowFrom and At are the replay's first cycle and the cycle read, for
	// the age of a V_P read before the samples kept
	// (prefill.Estimate.VPLearnedAt).
	WindowFrom, At time.Time
}

// DecodeTTFTState is what a decode variant's rows say of the decode side of
// a TTFT, read from its serving rows.
type DecodeTTFTState struct {
	// EngineTTFT is the decode engine's TTFT: of the latest whatif.TTFTWindowCycles
	// cycles, the largest, each a mean over the rows weighted by their
	// request rate, with the mean effective prompt of the same cycle (the
	// pair is never split). A cycle with no reading holds an empty slot, so a
	// figure ages out when the rows stop carrying one. Zero when unread. Its
	// Waiting is the largest decode backlog (decodeBacklog) any cycle of the
	// window carries, a backlog carried for prefill.DecodeTTFTLag after it drains.
	EngineTTFT whatif.DecodeTTFT
	// CalmCycle is the latest cycle, through this one, that read a decode
	// TTFT and closed a run of whatif.TTFTWindowCycles such cycles carrying no
	// decode backlog (idle cycles between them neither count nor break the
	// run): the cycle to answer at instead, read at CalmAt. 0 when no cycle
	// did.
	CalmCycle int
	CalmAt    time.Time
	// BacklogUnread says a decode row of the window did not say why its
	// requests wait (no capacity series): there a backlog cannot be told
	// from KV pulls in flight, and none is read.
	BacklogUnread bool
	// TransferSeconds is prefill.TransferReading's Figure, the one S1 prices
	// (slo.SwitchTTFTQuietTransfer): the fleet's quiet cycles inside
	// prefill.HeldDecodeAge of the cycle, else every row's over the same age; 0
	// with none. The CLI has no switch.
	TransferSeconds float64
	// AllRowsTransferSeconds is every row's figure, quiet or not, and TransferHeld
	// says the latest cycle was not quiet and TransferSeconds comes from earlier.
	AllRowsTransferSeconds float64
	TransferHeld           bool
	// Term is decode's part with a line, as S1 would have learned it over the
	// cycles through this one. It reads the rows with no settle tracker, so a
	// pod still warming up is admitted; the prompt-drift gate keeps most of it
	// out.
	Term whatif.DecodeTerm
}

// shareWindow is a prefill variant's raw shares at its current pod count. A
// new count restarts it; a cycle that reads no share leaves it as it is, so
// an idle tail forgets nothing.
type shareWindow struct {
	pods   int
	shares []float64
}

// note folds in a cycle's share at pods.
func (w *shareWindow) note(pods int, share float64, ok bool) {
	if w.pods != pods {
		w.pods, w.shares = pods, nil
	}
	if ok {
		w.shares = keepLast(w.shares, share, whatif.TTFTWindowCycles)
	}
}

// read is the window's largest and least, and whether it is full.
func (w *shareWindow) read() (largest, least float64, full bool) {
	for i, s := range w.shares {
		largest = max(largest, s)
		if i == 0 || s < least {
			least = s
		}
	}
	return largest, least, len(w.shares) == whatif.TTFTWindowCycles
}

// ttftSamples is the windows of the rows a TTFT question reads, by variant.
type ttftSamples struct {
	// promptSupport is slo.SwitchTTFTPromptSupport (PrefillState.PromptSupport).
	promptSupport bool
	prompts       map[string]*whatif.PromptWindow
	shares        map[string]*shareWindow
	engineTTFT    map[string][]whatif.DecodeTTFT
	// backlogs is each decode variant's latest backlog (backlogTrack.note).
	backlogs  map[string]*backlogTrack
	transfers map[string]*prefill.TransferWindow
	terms     map[string]*prefill.DecodeTermWindow
	// joint is the model's joint TTFT readings (noteJoint).
	joint *prefill.JointWindow
	// prefixAttention is slo.SwitchTTFTPrefixAttention
	// (PrefillState.PrefixAttention).
	prefixAttention bool
	// computedHit is slo.SwitchTTFTComputedHit (PrefillState.ComputedHit).
	computedHit bool
	// tierLoad is slo.SwitchTTFTTierLoad (PrefillState.TierLoad).
	tierLoad bool
	// tierKVWait is slo.SwitchTTFTTierKVWait (PrefillState.TierKVWait).
	tierKVWait bool
	// psByEngine is slo.SwitchTTFTPSByEngine (PrefillState.PSByEngine).
	psByEngine bool
	// pullWork is slo.SwitchTTFTPullWork, for the transfer windows
	// (prefill.TransferWindow.PullWork).
	pullWork bool
	// cachedTokenCost is slo.SwitchTTFTCachedTokenCost
	// (PrefillState.CachedTokenCost).
	cachedTokenCost bool
	// hopUnqueued is slo.SwitchTTFTHopUnqueued, for the decode term's
	// router windows (noteTerm).
	hopUnqueued bool
	// specHeld is slo.SwitchTTFTDecodeSpeculatingHeld, and itlPerToken
	// saturation.SwitchITLPerToken: the unit of the line a speculating row's
	// ITL is set against (prefill.DecodeTermCycle). StatesFrom sets both.
	specHeld, itlPerToken bool
	// hopWindowMeans is slo.SwitchTTFTHopWindowMeans, for the decode term's
	// router windows (noteTerm).
	hopWindowMeans bool
	// served is each prefill variant's served work, kept with servedOn
	// (saturation.SwitchPrefillVPServedUnqueued; PrefillState.ServedComputed).
	served   map[string]*servedWork
	servedOn bool
	// vpAtServed is saturation.SwitchPrefillVPAtServedWork
	// (PrefillState.VPAtServedWork).
	vpAtServed bool
	// hitAtCount is saturation.SwitchPrefillHitAtCount
	// (PrefillState.HitAtCount).
	hitAtCount bool
	// router is the model's router policy cycles (State.RouterPolicy).
	router balance.RouterPolicyWindow
}

// servedWorkOf is variant v's served work: its peaks over prefill.Retention
// (PrefillState.ServedComputed), and with vpAtServed its peaks over the
// whole window (PrefillState.ServedComputedWindow), and the counts whose
// served work was not sustained (PrefillState.ServedUnsteady). nil where
// none is kept.
func (t *ttftSamples) servedWorkOf(v string) (peaks, window map[int]float64, unsteady map[int]bool) {
	w := t.served[v]
	if w == nil {
		return nil, nil, nil
	}
	if t.vpAtServed {
		window = w.windowPeaks()
	}
	peaks, unsteady = w.peaksUnsteady()
	return peaks, window, unsteady
}

// newTTFTSamples returns empty windows; hopUnqueued is
// slo.SwitchTTFTHopUnqueued.
func newTTFTSamples(hopUnqueued bool) *ttftSamples {
	return &ttftSamples{hopUnqueued: hopUnqueued, prompts: map[string]*whatif.PromptWindow{}, shares: map[string]*shareWindow{},
		engineTTFT: map[string][]whatif.DecodeTTFT{}, backlogs: map[string]*backlogTrack{},
		transfers: map[string]*prefill.TransferWindow{},
		terms:     map[string]*prefill.DecodeTermWindow{}, joint: prefill.NewJointWindow(), served: map[string]*servedWork{}}
}

// noteJoint folds one cycle's prefill and decode rows into the model's joint
// window as S1 does (slo.noteJoint): only for a model with exactly one
// prefill and one decode variant, keyed to their accelerators and engine
// arguments (args, "" unknown), with the prompt read SGLang's way where the
// prefill variant runs SGLang (sglang). One difference stands: no settle
// tracker. S1 drops a replica's rows until it has been settled for
// prefill.SteadyFor; here every Ready row reads, so the CLI's inputs can
// include a replica's first two minutes after a start or an idle spell.
func (t *ttftSamples) noteJoint(in domain.AnalyzerInput, now time.Time, args func(variant string) string,
	sglang func(variant string) bool) {
	roles := rolesOf(in)
	var prefills, decodes []string
	for _, v := range slices.Sorted(maps.Keys(roles)) {
		switch domain.CanonicalRole(roles[v]) {
		case domain.RolePrefill:
			prefills = append(prefills, v)
		case domain.RoleDecode:
			decodes = append(decodes, v)
		}
	}
	if len(prefills) != 1 || len(decodes) != 1 {
		return
	}
	var tag strings.Builder
	for _, v := range []string{prefills[0], decodes[0]} {
		for _, vs := range in.VariantStates {
			if vs.VariantName == v {
				tag.WriteString(vs.AcceleratorName + "|" + args(v) + "|")
			}
		}
	}
	t.joint.Under(tag.String())
	t.joint.Observe(prefill.JointCycle{Now: now, Prefill: rowsOfVariant(in.ReplicaMetrics, prefills[0]),
		Decode: rowsOfVariant(in.ReplicaMetrics, decodes[0]), SGLang: sglang(prefills[0])})
}

// keepLast is s with v appended, holding the last n.
func keepLast[T any](s []T, v T, n int) []T {
	s = append(s, v)
	if len(s) > n {
		s = s[len(s)-n:]
	}
	return s
}

// rolesOf is each variant's role, as the cycle states it.
func rolesOf(in domain.AnalyzerInput) map[string]string {
	roles := map[string]string{}
	for _, vs := range in.VariantStates {
		roles[vs.VariantName] = vs.Role
	}
	return roles
}

// decodeRows is one decode variant's readings in one cycle.
type decodeRows struct {
	ttftWeighted, ttftWeight, ttftPlain float64
	ttftRows                            int
	// waiting is the largest decodeBacklog of the cycle's serving rows, and
	// unread says a row did not say why its requests wait.
	waiting int
	unread  bool
}

// backlogTrack is one decode variant's backlog over the cycles: the latest
// backlog, carried for prefill.DecodeTTFTLag (the rule the slo analyzer
// reads too), the cycles in a row since the last tainted one, the latest
// cycle that closed a calm window, and the latest cycle whose rows did not
// say why they wait.
type backlogTrack struct {
	carry        prefill.DecodeBacklogCarry
	calmRun      int
	calm         int
	calmAt       time.Time
	unreadCycle  int
	unreadInView bool
}

// note folds one cycle's backlog in and returns the backlog the cycle's TTFT
// carries: the cycle's own, or the latest one read within
// prefill.DecodeTTFTLag before it (prefill.DecodeBacklogCarry). read says the
// cycle has a decode TTFT: only such a cycle counts toward a calm window, so
// a calm cycle is one the planner can answer at with a calm decode figure,
// never an idle one; an idle cycle clears nothing. The window must be whole:
// the first cycles of a window that begins during a backlog's tail cannot
// see it.
func (b *backlogTrack) note(d *decodeRows, cycle int, at time.Time, read bool) int {
	if d.unread {
		b.unreadCycle = cycle
	}
	b.unreadInView = b.unreadCycle > 0 && cycle-b.unreadCycle < whatif.TTFTWindowCycles
	carried := b.carry.Carry(d.waiting, at)
	if carried > 0 {
		b.calmRun = 0
		return carried
	}
	if !read {
		return 0
	}
	b.calmRun++
	if b.calmRun >= whatif.TTFTWindowCycles {
		b.calm, b.calmAt = cycle, at
	}
	return 0
}

// prefillRows is one prefill variant's readings in one cycle.
type prefillRows struct {
	pods, rated           int
	queue, queueRead      int
	sum, busiest          float64
	computedRate, reqRate float64
}

// positive reports a reading above 0 that is finite: a zero is no reading of
// a latency, and pulls a mean low.
func positive(x float64) bool { return whatif.Finite(x) && x > 0 }

// note takes one cycle's rows, read at at.
func (t *ttftSamples) note(in domain.AnalyzerInput, cycle int, at time.Time) {
	t.router.Note(balance.RouterCycleOf(in, at))
	roles := rolesOf(in)
	pre := map[string]*prefillRows{}
	decode := map[string]*decodeRows{}
	for v, role := range roles {
		switch domain.CanonicalRole(role) {
		case domain.RolePrefill:
			pre[v] = &prefillRows{}
		case domain.RoleDecode:
			decode[v] = &decodeRows{}
		}
	}
	for _, rm := range in.ReplicaMetrics {
		if !prefill.Serving(rm) {
			continue
		}
		v := rm.VariantName
		if p := pre[v]; p != nil {
			p.pods++
			if rm.QueueLengthKnown {
				p.queueRead++
				p.queue += rm.QueueLength
			}
			if !rm.PrefillComputedTokenRateKnown || !positive(rm.PrefillComputedTokenRate) {
				continue
			}
			p.rated++
			p.sum += rm.PrefillComputedTokenRate
			p.busiest = max(p.busiest, rm.PrefillComputedTokenRate)
			if rm.RequestRateKnown && rm.RequestRate > 0 {
				p.computedRate += rm.PrefillComputedTokenRate
				p.reqRate += rm.RequestRate
			}
			continue
		}
		d := decode[v]
		if d == nil {
			continue
		}
		n, known := prefill.DecodeBacklog(rm)
		d.waiting, d.unread = max(d.waiting, n), d.unread || !known
		if rm.AvgTTFTKnown && positive(rm.AvgTTFT) {
			d.ttftRows++
			d.ttftPlain += rm.AvgTTFT
			if rm.RequestRateKnown && rm.RequestRate > 0 {
				d.ttftWeighted += rm.AvgTTFT * rm.RequestRate
				d.ttftWeight += rm.RequestRate
			}
		}
	}
	var allComputed, allRequests float64
	for v, p := range pre {
		w := t.shares[v]
		if w == nil {
			w = &shareWindow{}
			t.shares[v] = w
		}
		share, ok := rawShare(p.pods, p.sum, p.busiest)
		w.note(p.pods, share, ok)
		if t.servedOn {
			if t.served[v] == nil {
				t.served[v] = &servedWork{}
			}
			t.served[v].note(servedWorkSample{at: at, pods: p.pods, sum: p.sum, ok: p.pods > 0 && p.rated == p.pods,
				queue: p.queue, queueOK: p.pods > 0 && p.queueRead == p.pods})
		}
		if p.computedRate > 0 {
			allComputed += p.computedRate
			allRequests += p.reqRate
		}
		if t.prompts[v] == nil {
			t.prompts[v] = &whatif.PromptWindow{}
		}
		t.prompts[v].Note(rowsOfVariant(in.ReplicaMetrics, v), cycle, at)
		t.prompts[v].NoteRouterPrefix(cycle, in.RouterLatency)
	}
	for v, d := range decode {
		var slot whatif.DecodeTTFT
		if d.ttftRows > 0 && allComputed > 0 {
			slot = whatif.DecodeTTFT{Seconds: d.ttftPlain / float64(d.ttftRows), PromptTokens: allComputed / allRequests}
			if d.ttftWeight > 0 {
				slot.Seconds = d.ttftWeighted / d.ttftWeight
			}
		}
		b := t.backlogs[v]
		if b == nil {
			b = &backlogTrack{}
			t.backlogs[v] = b
		}
		slot.Waiting = b.note(d, cycle, at, slot.Seconds > 0)
		t.engineTTFT[v] = keepLast(t.engineTTFT[v], slot, whatif.TTFTWindowCycles)
	}
}

// windowBacklog is the largest backlog any cycle of the window carries, so
// a figure the queue raised is caught whichever slot is the largest.
func windowBacklog(samples []whatif.DecodeTTFT) int {
	var n int
	for _, d := range samples {
		n = max(n, d.Waiting)
	}
	return n
}

// noteTerm folds one cycle, read at now, into each decode variant's readings
// of the decode term, as S1 does (slo.noteDecodeTerm). args is a variant's
// engine arguments, "" unknown.
func (t *ttftSamples) noteTerm(in domain.AnalyzerInput, now time.Time, args func(variant string) string) {
	roles := rolesOf(in)
	var pre []domain.ReplicaMetrics
	for _, rm := range in.ReplicaMetrics {
		if domain.CanonicalRole(roles[rm.VariantName]) == domain.RolePrefill {
			pre = append(pre, rm)
		}
	}
	prompt, _ := prefill.MeanEffectivePrompt(pre, false)
	for v, role := range roles {
		if domain.CanonicalRole(role) != domain.RoleDecode {
			continue
		}
		w := t.terms[v]
		if w == nil {
			w = prefill.NewDecodeTermWindow()
			t.terms[v] = w
		}
		accelerator := ""
		for _, vs := range in.VariantStates {
			if vs.VariantName == v {
				accelerator = vs.AcceleratorName
			}
		}
		w.Under(accelerator, args(v))
		x := t.transfers[v]
		if x == nil {
			x = &prefill.TransferWindow{PullWork: t.pullWork}
			t.transfers[v] = x
		}
		x.Under(accelerator, args(v))
		x.Observe(now, rowsOfVariant(in.ReplicaMetrics, v))
		w.Observe(prefill.DecodeTermCycle{Now: now, Prefill: pre, Decode: rowsOfVariant(in.ReplicaMetrics, v),
			PromptTokens: prompt, Router: in.RouterLatency,
			RouterHeld:  in.SchedulerQueue != nil && in.SchedulerQueue.QueueSize > 0,
			HopUnqueued: t.hopUnqueued, SpeculatingHeld: t.specHeld, ITLPerToken: t.itlPerToken,
			HopWindowMeans: t.hopWindowMeans})
	}
}

// rawShare is one cycle's f_raw: the busiest of pods over the mean, a pod with
// no rate counting as one that computed nothing, which reads high, the
// permitted side. False with fewer than two pods or no rate on any.
func rawShare(pods int, sum, busiest float64) (float64, bool) {
	if pods < 2 || !(sum > 0) {
		return 0, false
	}
	return busiest * float64(pods) / sum, true
}

// rowsOfVariant is rows of the variant.
func rowsOfVariant(rows []domain.ReplicaMetrics, variant string) []domain.ReplicaMetrics {
	var out []domain.ReplicaMetrics
	for _, rm := range rows {
		if rm.VariantName == variant {
			out = append(out, rm)
		}
	}
	return out
}

// fillTTFT sets what the state says of a TTFT question, from the samples
// through the cycle in and from P1's estimates.
func (r *replayer) fillTTFT(st *State, in domain.AnalyzerInput, t *ttftSamples) {
	roles := rolesOf(in)
	pods := map[string]int{}
	for _, rm := range in.ReplicaMetrics {
		if prefill.Serving(rm) {
			pods[rm.VariantName]++
		}
	}
	for _, v := range slices.Sorted(maps.Keys(roles)) {
		if domain.CanonicalRole(roles[v]) != domain.RolePrefill {
			continue
		}
		ps := PrefillState{Variant: v, Pods: pods[v], WindowFrom: st.FirstAt, At: st.At}
		if w := t.shares[v]; w != nil {
			ps.RawShare, ps.RawShareLeast, ps.RawShareFull = w.read()
		}
		ps.PromptTokens, ps.PromptFromCycle, ps.PromptToCycle, ps.PromptCycles = t.prompts[v].Prompt()
		ps.PromptWindow = t.prompts[v].Clone()
		ps.PromptSupport = t.promptSupport
		ps.PrefixAttention = t.prefixAttention
		ps.ComputedHit = t.computedHit
		ps.TierLoad = t.tierLoad
		ps.TierKVWait = t.tierKVWait
		ps.PSByEngine = t.psByEngine
		ps.VPAtServedWork = t.vpAtServed
		ps.HitAtCount = t.hitAtCount
		ps.ServedComputed, ps.ServedComputedWindow, ps.ServedUnsteady = t.servedWorkOf(v)
		ps.CachedTokenCost = t.cachedTokenCost
		ps.Estimate, ps.EstimateRead = estimateAt(r.sat.PrefillEstimate, in.Namespace, in.ModelID, v, st.At)
		st.Prefill = append(st.Prefill, ps)
	}
	for i := range st.Variants {
		vs := &st.Variants[i]
		vs.DecodeTTFT.EngineTTFT = largestTTFT(t.engineTTFT[vs.Variant])
		if vs.DecodeTTFT.EngineTTFT.Seconds > 0 {
			vs.DecodeTTFT.EngineTTFT.Waiting = windowBacklog(t.engineTTFT[vs.Variant])
		}
		if b := t.backlogs[vs.Variant]; b != nil {
			vs.DecodeTTFT.CalmCycle, vs.DecodeTTFT.CalmAt = b.calm, b.calmAt
			vs.DecodeTTFT.BacklogUnread = b.unreadInView
		}
		var est prefill.DecodeTermEstimate
		if w := t.terms[vs.Variant]; w != nil {
			est = w.Estimate()
		}
		vs.DecodeTTFT.Term = whatif.TermFromEstimate(est)
		if x := t.transfers[vs.Variant]; x != nil {
			r := x.Reading()
			vs.DecodeTTFT.TransferSeconds, vs.DecodeTTFT.AllRowsTransferSeconds = r.Figure(), r.AllRows
			vs.DecodeTTFT.TransferHeld = r.Held
		}
	}
	st.FlowControlSeries = in.FlowControlSeries()
	st.RouterPolicy = t.router.Policy()
	st.Joint, st.JointRate = t.joint.Estimate(), in.ArrivalRate
	for _, rm := range in.ReplicaMetrics {
		if !prefill.Serving(rm) {
			continue
		}
		switch domain.CanonicalRole(roles[rm.VariantName]) {
		case domain.RolePrefill:
			st.JointPrefillPods++
		case domain.RoleDecode:
			st.JointDecodePods++
		}
	}
}

// estimateAt is a variant's estimate read through read, when it was stashed
// at now: one left from an earlier cycle, which did observe the variant, is
// not this cycle's.
func estimateAt(read func(namespace, modelID, variant string) (prefill.Estimate, time.Time, bool),
	namespace, modelID, variant string, now time.Time) (prefill.Estimate, bool) {
	est, at, ok := read(namespace, modelID, variant)
	if !ok || !at.Equal(now) {
		return prefill.Estimate{}, false
	}
	return est, true
}

// largestTTFT is the sample with the largest TTFT, as a pair: S1 takes the
// largest and never the largest of each.
func largestTTFT(samples []whatif.DecodeTTFT) whatif.DecodeTTFT {
	var top whatif.DecodeTTFT
	for _, d := range samples {
		if d.Seconds > top.Seconds {
			top = d
		}
	}
	return top
}
