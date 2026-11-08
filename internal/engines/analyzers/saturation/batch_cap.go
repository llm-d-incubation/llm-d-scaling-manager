package saturation

import (
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// The batch cap the derived k2 (P3) is priced at, and where it comes from
// (issue ev-shindin/llm-scaling-manager#399).
//
// P3 prices a decode or aggregated replica at min(B*O/(I+O), S) sequences of
// (I + O/2) tokens, where S is one engine rank's batch cap. Where the
// engine's flags state the cap, S is that. Where they do not -- a launcher
// script, or the cap passed as a variable nothing resolves -- S was the
// parser's default of 256, and a variant whose scale target was never read
// had no P3 at all. Either way a sequence-bound engine whose true cap is
// lower was priced at k1: under flow control its own queue stays under the
// k2 gate, so no saturated reading (P1) is ever taken, and k1 alone was
// measured at 1.905x the truth -- 48 % under-provisioned.
//
// Two sources stand in for the unread flag, in this order:
//
//   - declared: the llm-d.ai/max-num-seqs annotation on the scale target
//     (capacity.EngineParams.DeclaredBatchCap). An operator's statement, so
//     it replaces the default either way.
//   - learned: the highest running count a pod of the variant held flat, per
//     engine rank, with requests waiting on it and its KV under k1
//     (observeBatchCap). Off unless the policy's enableBatchCapLearning turns
//     it on. A running count can never exceed the true cap, so every sample
//     is at or below it and the max over samples only rises toward it. It
//     may only LOWER the default -- min(default, learned) -- and with no
//     scale target read it replaces k1 only through min(k1, k2).
//
// The cap the flags state wins over both: it is what the engine runs.

// SwitchBatchCapDeclared is the replay switch for the declared batch cap
// (issue ev-shindin/llm-scaling-manager#399). On, a variant whose flags state no batch cap prices its
// derived k2 at the cap the llm-d.ai/max-num-seqs annotation states. Off,
// as before, at the parser's default.
const SwitchBatchCapDeclared = "batch-cap-declared"

// SwitchBatchCapLearned is the replay switch for the learned batch cap
// (issue ev-shindin/llm-scaling-manager#399). On, a variant whose policy sets enableBatchCapLearning
// learns its batch cap from where its pods' running counts plateau with
// requests waiting, and prices its derived k2 at no more than that. Off, as
// before, nothing is learned or read whatever the policy says.
const SwitchBatchCapLearned = "batch-cap-learned"

// The readers of the batch cap besides P3 (issue ev-shindin/llm-scaling-manager#420). Each took the cap
// from the engine's flags alone, so where the flags are unread a declared or
// learned cap reached P3 and nothing else: the ITL fit admitted no reading
// at the cap, a P/D decode at its cap dropped the part of its queue vLLM
// calls deferred (the ev-shindin/llm-scaling-manager#287 symptom), and the derived mu priced the replica
// at 256 sequences per rank. Each now reads the declared cap, behind its own
// switch.
//
// The learned cap reaches the derived mu, as it reaches P3, and NOT the two
// readers that ask whether a replica is AT its cap (atCapBatchCap). A
// learned cap is the highest plateau seen, so it may sit below the true
// cap. In mu and P3 a low cap only errs toward more replicas. At the cap it
// is a claim about the engine: below the true cap vLLM still promotes a
// decode's landed pulls, so the decode view would charge in-flight pulls as
// backlog -- and observeBatchCap, which reads the view, would see that
// queue and confirm the cap that caused it. And the ITL fit would admit a
// memory-bound reading above the band as capped.

// SwitchBatchCapITLAdmission is the replay switch for the ITL fit's
// sequence-cap admission (issue ev-shindin/llm-scaling-manager#420). On, a replica is at its cap
// (itl.Admission.MaxNumSeqs) at the cap the flags or the
// llm-d.ai/max-num-seqs annotation state (atCapBatchCap). Off, as before,
// at the flags' cap alone, so where they state none no reading above the
// band is admitted.
const SwitchBatchCapITLAdmission = "batch-cap-itl-admission"

// SwitchBatchCapDecodeView is the replay switch for the P/D decode view's
// at-cap exception (issue ev-shindin/llm-scaling-manager#420). On, a decode at the cap the flags or the
// annotation state (atCapBatchCap) keeps its full queue
// (decodeCapacityView). Off, as before, only a decode at the cap its flags
// state does.
const SwitchBatchCapDecodeView = "batch-cap-decode-view"

// SwitchBatchCapDerivedMu is the replay switch for the derived mu's batch
// cap (issue ev-shindin/llm-scaling-manager#420). On, whatif.DeriveMuAtCap caps the resident sequences at the cap
// the derived k2 is priced at (pricingBatchCap). Off, as before, at the
// flags' cap or the parser's default of 256.
const SwitchBatchCapDerivedMu = "batch-cap-derived-mu"

// SwitchBatchCapZeroReplica is the replay switch for the zero-replica
// estimate's batch cap (issue ev-shindin/llm-scaling-manager#427). On, a variant at zero replicas is
// priced at the cap the flags or the llm-d.ai/max-num-seqs annotation state
// (zeroReplicaBatchCap). Off, as before, at the flags' cap or the parser's
// default of 256.
const SwitchBatchCapZeroReplica = "batch-cap-zero-replica"

// Where the batch cap a derived k2 was priced at came from, as the
// k2-decision line names it (batchCapSource).
const (
	batchCapFromFlags = "flags"
	batchCapDeclared  = "declared"
	batchCapLearned   = "learned"
	// batchCapNone is no cap to read: for a reader at the cap
	// (atCapBatchCap), the flags state none and nothing is declared; for
	// the derived mu, no cap at all (the scale target unread).
	batchCapNone    = "none"
	batchCapDefault = "default"
)

// BatchCapPlateauCycles is how many cycles in a row a pod's running count
// must read the same, with every admission rule passing on each, before it
// is a sample of the batch cap: 45 s at the 15 s cycle. A fill passes
// through a count; a cap holds it.
const BatchCapPlateauCycles = 3

// BatchCapRankSpread is how far the busiest engine rank's KV may sit above
// the replica's mean for a data-parallel row to be a sample. The collector
// reports the running count summed over ranks, not per rank, so the sample
// is the mean per rank -- the cap only where every rank holds it. One full
// rank beside seven at half would read 72 for a cap of 128, and price the
// replica at 0.56 of its capacity for as long as the cap is kept. KV per
// rank is the evidence the ranks hold alike.
const BatchCapRankSpread = 0.10

// pricedBatchCap is one engine rank's batch cap a derived k2 is priced at,
// and its source. A zero perRank is no cap: no derived k2 can be priced.
type pricedBatchCap struct {
	perRank int64
	source  string
}

// learnedBatchCap is a variant's learned batch cap: the most sequences per
// engine rank one of its pods was seen holding at its cap, under the engine
// arguments it was learned under (args), raised from pod, last confirmed by a
// plateau when at says and last used to price a replica when usedAt says.
type learnedBatchCap struct {
	perRank int64
	args    string
	pod     string
	at      time.Time
	usedAt  time.Time
}

// batchCapPlateau is one pod's run of cycles at the same running count.
type batchCapPlateau struct {
	running, ranks, cycles int
	at                     time.Time
}

// batchCapObs is what the admission rules judge for one row.
type batchCapObs struct {
	adm    admission
	joined bool
	queue  int
	// decodeQueueUnknown is a decode row whose waiting count does not say
	// why it waits (domain.ReplicaMetrics.CapacityQueueKnown).
	decodeQueueUnknown bool
	running            int
	runningKnown       bool
	tokens             int64
	k1                 int64
	preemptKnown       bool
	preempt            float64
	// ranks, meanKv and busiestKv are the replica's engine ranks, their mean
	// KV fill and the busiest one's (zero when not collected).
	ranks             int
	meanKv, busiestKv float64
}

// batchCapRule is one admission rule of the learned batch cap: a row it
// refuses is no sample, and ends the pod's plateau.
type batchCapRule struct {
	name   string
	refuse func(batchCapObs) bool
}

// batchCapRules are the learned batch cap's admission rules. Each guards
// against a sample BELOW the true cap -- one can never be above it -- which
// would lower the derived k2 and order replicas the fleet does not need.
func batchCapRules() []batchCapRule {
	return []batchCapRule{
		// A bridge runs the pool's engine, a Pod that is not Ready is not
		// routed to, an idle one holds nothing, and mid-rollout a pod may run
		// the old arguments (admission.serving).
		{"not-serving", func(o batchCapObs) bool { return !o.adm.serving() }},
		// A replica Ready under a minute is still filling.
		{"joined", func(o batchCapObs) bool { return o.joined }},
		// Requests waiting on the pod itself. Under flow control the router
		// holds the queue, and a pod held with none waiting may be far from
		// its cap: the router judges the pool, not the pod (ev-shindin/llm-scaling-manager#399's 1.68x
		// over-order with one loaded pod and one idle).
		{"no-waiting", func(o batchCapObs) bool { return o.queue < 1 }},
		// A decode request whose KV is still in flight from prefill counts as
		// waiting (pd_decode_queue.go). Where vLLM does not say which part
		// waits for capacity, a queue on a decode pod is no evidence of a
		// full batch.
		{"decode-queue-unknown", func(o batchCapObs) bool { return o.decodeQueueUnknown }},
		{"running-unknown", func(o batchCapObs) bool { return !o.runningKnown || o.running < 1 }},
		// A pod at its KV threshold is held by memory, not its batch: its
		// running count is whatever fits, and k1 prices it anyway.
		{"kv-bound", func(o batchCapObs) bool { return o.k1 <= 0 || o.tokens >= o.k1 }},
		// A preempting engine evicts sequences from its batch. Unknown is
		// refused too: SGLang publishes no preemption counter.
		{"preempting", func(o batchCapObs) bool { return !o.preemptKnown || o.preempt > 0 }},
		// Data-parallel ranks loaded alike (BatchCapRankSpread), or the mean
		// per rank is under the cap the full rank holds. An unknown busiest
		// rank is refused.
		{"uneven-ranks", func(o batchCapObs) bool {
			return o.ranks > 1 && !(o.busiestKv > 0 && o.busiestKv <= o.meanKv*(1+BatchCapRankSpread))
		}},
	}
}

// batchCapKey is a variant's key in batchCaps, and the prefix of its pods'
// keys in batchCapPlateaus: namespace|variant|model|, the k2 windows' prefix
// (scopeK2Key), so forgetting the variant drops both.
func batchCapKey(namespace, modelID, variant string) string {
	return namespace + "|" + variant + "|" + modelID + "|"
}

// observeBatchCap judges one row of a variant as a sample of its batch cap,
// and raises the variant's learned cap when it is one. A no-op unless the
// switch is on and the policy enables learning.
func (a *SaturationAnalyzer) observeBatchCap(namespace, modelID, role string, rm domain.ReplicaMetrics,
	adm admission, k1 int64, params *capacity.EngineParams, enabled bool, logger logr.Logger) {
	// P3 never prices prefill, so a prefill cap would price nothing.
	if !a.batchCapLearned || !enabled || domain.CanonicalRole(role) == domain.RolePrefill {
		return
	}
	now := a.now()
	since := a.readySince(rm)
	o := batchCapObs{
		adm:                adm,
		joined:             !since.IsZero() && now.Sub(since) < JoinedReplicaWindow,
		queue:              rm.BusiestRankQueue(),
		decodeQueueUnknown: domain.CanonicalRole(role) == domain.RoleDecode && !rm.CapacityQueueKnown,
		running:            rm.RunningRequests,
		runningKnown:       rm.RunningRequestsKnown,
		tokens:             rm.BusiestRankTokens(),
		k1:                 k1,
		preemptKnown:       rm.PreemptionRateKnown,
		preempt:            rm.PreemptionRate,
		ranks:              rm.Ranks(),
		meanKv:             rm.KvCacheUsage,
		busiestKv:          rm.MaxRankKvCacheUsage,
	}
	key := batchCapKey(namespace, modelID, rm.VariantName)
	podKey := key + rm.PodName
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range a.batchCapAdmission {
		if r.refuse(o) {
			delete(a.batchCapPlateaus, podKey)
			return
		}
	}
	ranks := rm.Ranks()
	p := a.batchCapPlateaus[podKey]
	if p != nil && p.running == o.running && p.ranks == ranks && now.Sub(p.at) <= K2SettleMaxGap {
		p.cycles++
	} else {
		p = &batchCapPlateau{running: o.running, ranks: ranks, cycles: 1}
		a.batchCapPlateaus[podKey] = p
	}
	p.at = now
	if p.cycles < a.batchCapPlateauCycles {
		return
	}
	// Each rank holds at most its cap, so the floor of the mean is at most
	// the cap too.
	sample := int64(o.running / ranks)
	args := windowArgs(params)
	lc := a.batchCaps[key]
	if lc == nil || lc.args != args {
		lc = &learnedBatchCap{args: args}
		a.batchCaps[key] = lc
	}
	lc.at = now
	if sample <= lc.perRank {
		return
	}
	logger.V(perReplicaLogLevel).Info("batch-cap-learned",
		"modelID", modelID, "namespace", namespace, "variant", rm.VariantName, "pod", rm.PodName,
		"batchCap", sample, "previous", lc.perRank, "running", o.running, "engineRanks", ranks,
		"queueLength", o.queue, "plateauCycles", p.cycles)
	lc.perRank, lc.pod = sample, rm.PodName
}

// pricingBatchCap is the batch cap one engine rank of the variant is priced
// at by the derived k2, and its source: the flags' cap, the declared cap,
// the learned cap where it is lower than the default, or the default.
// learning is the policy's enableBatchCapLearning.
//
// A learned cap that prices the replica is stamped as used: it ages on its
// last use, as the k2 and mu windows do. A fleet the cap provisions
// correctly stops queueing, so nothing confirms the cap; aged on its last
// confirmation, it would be swept from exactly the fleet it is working for,
// and the fleet priced at the default again.
func (a *SaturationAnalyzer) pricingBatchCap(namespace, modelID, variant string,
	params *capacity.EngineParams, learning bool) pricedBatchCap {
	return a.pricingBatchCapFor(namespace, modelID, variant, params, learning, true)
}

// pricingBatchCapFor is pricingBatchCap; stamp false reads a learned cap
// without stamping it as used, for a reader outside the analyzer's cycle
// (PricingBatchCap).
func (a *SaturationAnalyzer) pricingBatchCapFor(namespace, modelID, variant string,
	params *capacity.EngineParams, learning, stamp bool) pricedBatchCap {
	if params != nil {
		if stated := params.ExplicitMaxNumSeqs(); stated > 0 {
			return pricedBatchCap{perRank: stated, source: batchCapFromFlags}
		}
		if a.batchCapDeclared && params.DeclaredBatchCap > 0 {
			return pricedBatchCap{perRank: params.DeclaredBatchCap, source: batchCapDeclared}
		}
	}
	def := params.BatchCap()
	if a.batchCapLearned && learning {
		a.mu.Lock()
		lc := a.batchCaps[batchCapKey(namespace, modelID, variant)]
		if lc != nil && lc.args == windowArgs(params) && lc.perRank > 0 && (def <= 0 || lc.perRank < def) {
			if stamp {
				lc.usedAt = a.now()
			}
			learned := lc.perRank
			a.mu.Unlock()
			return pricedBatchCap{perRank: learned, source: batchCapLearned}
		}
		a.mu.Unlock()
	}
	if def > 0 {
		return pricedBatchCap{perRank: def, source: batchCapDefault}
	}
	return pricedBatchCap{}
}

// atCapBatchCap is the batch cap a reader that asks whether a replica is AT
// its cap reads, and its source: the flags' cap, and with readsDeclared (that
// reader's switch, issue ev-shindin/llm-scaling-manager#420) the cap the llm-d.ai/max-num-seqs annotation
// declares where the flags state none (capacity.EngineParams.StatedBatchCap,
// as the declared cap prices P3 only under SwitchBatchCapDeclared). Never
// the parser's default -- vLLM V1 defaults to 1024 on large accelerators
// where the parser assumes 256, so a batch of 256 read against the default
// may still be growing -- and never the learned cap (see above). A zero
// perRank, source batchCapNone, admits nothing as at its cap. It reads no
// learned state, so it stamps none as used.
func (a *SaturationAnalyzer) atCapBatchCap(params *capacity.EngineParams, readsDeclared bool) pricedBatchCap {
	if s := params.ExplicitMaxNumSeqs(); s > 0 {
		return pricedBatchCap{perRank: s, source: batchCapFromFlags}
	}
	if readsDeclared && a.batchCapDeclared {
		if s := params.StatedBatchCap(); s > 0 {
			return pricedBatchCap{perRank: s, source: batchCapDeclared}
		}
	}
	return pricedBatchCap{source: batchCapNone}
}

// flagsOrDefaultBatchCap is the cap the derived mu was capped at before
// issue ev-shindin/llm-scaling-manager#420 (SwitchBatchCapDerivedMu off): the flags' cap, else the
// parser's default (capacity.EngineParams.BatchCap), named as
// pricingBatchCap names the same two.
func flagsOrDefaultBatchCap(params *capacity.EngineParams) pricedBatchCap {
	if s := params.ExplicitMaxNumSeqs(); s > 0 {
		return pricedBatchCap{perRank: s, source: batchCapFromFlags}
	}
	if d := params.BatchCap(); d > 0 {
		return pricedBatchCap{perRank: d, source: batchCapDefault}
	}
	return pricedBatchCap{}
}

// zeroReplicaBatchCap is the batch cap one engine rank of a variant at zero
// replicas is priced at by its derived estimate (estimateStoredCapacity),
// and its source. With SwitchBatchCapZeroReplica on, for a decode or
// aggregated variant, it is pricingBatchCap without learning: the cap the flags state, else, with
// SwitchBatchCapDeclared on, the cap the llm-d.ai/max-num-seqs annotation
// declares, else the parser's default. Off, as before issue ev-shindin/llm-scaling-manager#427, the
// flags' cap or the default (flagsOrDefaultBatchCap): a variant that
// declares 64 was priced at 64 per replica once a pod ran, and at 256 on its
// way up from zero.
//
// The learned cap is never read, so none is stamped as used.
//
// A prefill variant keeps the flags' cap or the default, switch on or off.
// P3 never prices prefill, so no live prefill replica is priced at a
// declared cap, and the zero-replica figure for prefill is not the figure
// its first replica will get. Moving it would change the order from zero
// and the optimizer's cost ranking on a cap nothing else reads for prefill.
func (a *SaturationAnalyzer) zeroReplicaBatchCap(namespace, modelID, variant, role string,
	params *capacity.EngineParams) pricedBatchCap {
	if a.batchCapZeroReplica && domain.CanonicalRole(role) != domain.RolePrefill {
		return a.pricingBatchCap(namespace, modelID, variant, params, false)
	}
	return flagsOrDefaultBatchCap(params)
}

// sweepBatchCapsLocked drops a learned cap neither confirmed by a pod nor
// used to price one for bucketRetention, and a plateau no cycle has extended
// within K2SettleMaxGap. It returns how many caps it dropped. Callers hold
// a.mu.
func (a *SaturationAnalyzer) sweepBatchCapsLocked(now time.Time, bucketRetention time.Duration) int {
	n := 0
	for k, lc := range a.batchCaps {
		if now.Sub(lc.at) > bucketRetention && now.Sub(lc.usedAt) > bucketRetention {
			delete(a.batchCaps, k)
			n++
		}
	}
	for k, p := range a.batchCapPlateaus {
		if now.Sub(p.at) > K2SettleMaxGap {
			delete(a.batchCapPlateaus, k)
		}
	}
	return n
}
