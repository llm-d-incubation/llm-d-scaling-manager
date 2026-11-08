package saturation

import (
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// noteLineMismatch logs, and only logs, when this replica's observed
// generation-token rate disagrees with what the fitted line predicts at the
// replica's own utilization.
//
// It is a DIAGNOSTIC, not a gate, and that is a decision taken against
// measurement rather than taste.
//
// The proposal asks for a derived mu to be verified against the observed rate
// before it may order. Built as a gate, that verification withheld ordering
// 28 times in run T (2026-09-28) and every one of them fell inside the first
// two minutes of the run -- the phase-1 ramp -- with the predicted rate above
// the observed one nearly every time, and on four cycles every replica of the
// role rejected at once. The fleet still reached 9 so the run stands, but its
// phase-1 TTFT p95 was 64 s against main's 42 s.
//
// The cause is not a threshold that needs tuning. The signals this check
// reads are not collected over the same window -- KvUsageInstant has none,
// while GenerationTokenRate and AvgITL are both rate[1m] -- so on a ramp k reaches
// its new level in seconds while the 1m rate still reports a fraction of the
// steady state. The disagreement is therefore largest exactly when a scale-up
// is needed, and it moves every replica together, so neither N-consecutive
// cycles nor a majority-of-replicas rule suppresses it.
//
// What it is still worth: it is the line that let run T be diagnosed, and a
// persistent mismatch away from a ramp is real evidence that the shape, and so
// KVreq, does not describe the fleet. The collector now has a k averaged over
// the same window as the rate (KvUsageMean, which the ITL line is fitted
// from: issue ev-shindin/llm-scaling-manager#393), so this can become a gate again. Until it does, the check
// still pairs the instant k with the rate[1m] figures, and only logs.
//
// Warm-pool replicas and roles priced from no line (itlLineRole: prefill,
// and an aggregated variant with SwitchITLFitsAggregated off) are skipped
// rather than logged: a
// bridge runs on the pool's own engine settings (noteITL excludes it from the
// fit for that reason and floor.Estimate prices it not at all), and a prefill
// replica generates about one token per request, so neither disagreement means
// anything about this variant's line.
// borrowedFrom is the variant this line was measured on when it is not this
// variant's own, and empty otherwise. Logged because this is the ONLY line
// that fires when a line does not match reality, and the two cases have
// different remedies: a mismatch on a borrowed line says those two
// configurations are not really alike and names who to look at, while a
// mismatch on an own line says this variant's own fit has drifted.
func (a *SaturationAnalyzer) noteLineMismatch(model itl.Model, params *capacity.EngineParams,
	rm domain.ReplicaMetrics, role string, kvReq float64, borrowedFrom string,
	logger logr.Logger) {
	if rm.FromWarmPool || !a.itlLineRole(role) {
		return
	}
	capacityTokens := whatif.CapacityTokens(params, rm.Ranks(), rm.TotalKvCapacityTokens)
	errPct, ok := itl.GPSErrorPct(model, rm.KvUsageInstant, capacityTokens,
		kvReq, rm.GenerationTokenRate)
	if !ok || errPct <= itl.DefaultGPSMismatchThresholdPct {
		return
	}
	logger.V(logging.DEFAULT).Info("itl-gps-mismatch",
		"namespace", rm.Namespace, "variant", rm.VariantName, "pod", rm.PodName,
		"k", rm.KvUsageInstant, "observedGPS", rm.GenerationTokenRate,
		"predictedGPS", itl.TokenRate(model, rm.KvUsageInstant, capacityTokens, kvReq),
		"errPct", errPct, "thresholdPct", itl.DefaultGPSMismatchThresholdPct,
		"lineBorrowed", borrowedFrom != "", "from", borrowedFrom,
		"gates", false)
}

// pricingK is the fill mu is priced at under cfg: whatif.PricingK over the
// tier's two thresholds and the band its line is fitted over. A nil cfg has
// neither threshold and the default band.
func pricingK(cfg *config.ScalingPolicy) float64 {
	var kv, scaleUp float64
	if cfg != nil {
		kv, scaleUp = cfg.KvCacheThreshold, cfg.ScaleUpThreshold
	}
	lo, hi := cfg.ITLBand()
	return whatif.PricingK(kv, scaleUp, whatif.FillBand{Lo: lo, Hi: hi})
}

// itlUnderArgs drops key's ITL window and baseline when they were learned
// under engine arguments other than args, the ones its variant runs now, and
// records args for them. A key seen for the first time adopts args, as an
// untagged k2 window does (window_args.go). The baseline is put aside under
// the arguments it was learned under, and a rollback to them gets it back
// (itlBaselineStash); the window is not: the slope refits from new readings
// within it.
func (a *SaturationAnalyzer) itlUnderArgs(key, args string) {
	if args == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	tag, ok := a.itlArgs[key]
	a.itlArgs[key] = args
	if !ok || tag == args {
		return
	}
	// The tracker's lock is taken under a.mu here, as EvictStaleHistory takes
	// it: the tracker never calls back into the analyzer.
	if b, ok := a.itlTracker.Baseline(key); ok {
		a.itlBaselineStash[stashKey(key, tag)] = b
	}
	a.itlTracker.Sweep(a.now(), func(k string) bool { return k == key })
	// An aggregated variant's shape state was the dropped window's and
	// slope's (itl_aggregated_shape.go).
	delete(a.itlAggregated, key)
	if b, ok := a.itlBaselineStash[stashKey(key, args)]; ok {
		delete(a.itlBaselineStash, stashKey(key, args))
		a.itlTracker.SetBaseline(key, b)
	}
}

// noteITL adds this cycle's (k, ITL) readings for one variant to its rolling
// window and returns the reading: the model fitted over what the window holds,
// and whether it is a slope held for want of readings (SlopeHeld) rather than
// a fit made now.
//
// Ready replicas of the variant itself only, and only where both halves of the
// pair are present: a pod still failing its readiness probe, or one lent by the
// warm pool, is not a reading of what one of this variant's replicas does.
//
// Why the warm-pool exclusion, stated correctly -- an earlier version of this
// comment said a lent pod runs "the pool's own engine settings", and that is
// false by construction. warmpool.EngineOptionsFrom derives the warm copy's
// command line from the ordinary replicas' own PodSpec precisely so the two
// MATCH (a different --gpu-memory-utilization is a different torch.compile
// cache key), and warmableFlags covers every flag the fingerprint hashes. By
// default a lent pod therefore hashes to the SAME engine configuration, so the
// fingerprint cannot be what tells it apart.
//
// What tells it apart is what it is running: a pool Pod hosts one awake engine
// plus its sleepers, each keeping ~1.4 GiB of GPU residue
// (internal/warmpool/demand.go measured
// 4.4 GiB free at 0.95 on an 80 GiB card). Its ITL(k) is the latency of an
// engine sharing a card, which is not the latency of one of this variant's own
// replicas -- and k itself is read against a KV budget the sleepers have
// already eaten into. A pool that does set an explicit GPUMemoryUtilization
// does also change the digest, but the exclusion cannot rest on that: zero
// inherits the workload's value, and zero is the default.
//
// Above itl.DefaultMaxObservableK the
// engine preempts rather than slowing down and the line stops describing it, so
// those readings are left out too.
//
// The window is NOT cleared on a shape change, which is the difference from the
// throughput analyzer's use of it. ITL(k) is a property of the hardware and the
// engine, not of the shape -- that is the whole reason mu can be derived across
// a shape change -- so a fit made under one shape is still the right fit under
// the next, and clearing it would reintroduce exactly the wait this replaces.
func (a *SaturationAnalyzer) noteITL(key string, replicas []domain.ReplicaMetrics,
	variantName string, adm itl.Admission, now time.Time, logger logr.Logger) itl.Reading {
	// Stamped here as well as in noteReplicaStartFor, because this function
	// CREATES learned state (the window, and the baseline the tracker writes
	// on a full fit) and the sweep's rule is that an unstamped key counts as
	// stale. Stamping only in noteReplicaStartFor made that rule hold by the
	// order two calls happen in a third file -- fitLines calls
	// noteReplicaStartFor just before the decode guard that reaches this
	// function -- so any early return added to noteReplicaStartFor, or a
	// reordering of those two calls, would have deleted every learned ITL
	// baseline on the first sweep 24h later, silently, falling back to
	// itl.DefaultBaselineSec. A producer of learned state stamps its own key.
	//
	// The analyzer-wide lock covers the stamp and nothing else: the tracker
	// takes its own around the window, and the scan, the fit and the log
	// lines below are not serialised behind every other model's cycle.
	a.mu.Lock()
	a.noteVariantSeen(key, now)
	a.mu.Unlock()

	// Which rows are readings, the window's confidence gate (Ready: enough
	// samples, far enough apart in k), and the two tiers below are the
	// tracker's: the slo analyzer fits with the same rules.
	r := a.itlTracker.Observe(key, variantName, replicas, adm, now)
	switch r.Tier {
	case itl.TierOLS:
		a.paths.add(pathITLOLS, 1)
	case itl.TierPinnedB:
		a.paths.add(pathITLPinned, 1)
	}
	// Every reading the window took was read at its instant k.
	a.paths.add(pathInstantKV, int64(r.Offered))

	// Logged at DEFAULT, not DEBUG: the deployment passes no -v, so DEBUG (4)
	// never prints and a diagnostic nobody can read is the problem it was
	// written to solve. It is one line per variant per cycle, against the
	// per-REPLICA replica-capacity-decision already logged at this level.
	//
	// Counted per reason, because "the window is empty" has four different
	// causes with four different fixes, and a run that cannot tell them apart
	// costs a rebuild to find out which one it was. Every row is counted
	// under exactly one of them, so they add up to `replicas`. belowBand was
	// the one missing: a single replica at 0.5 to 3 requests/s sat below k
	// 0.15 on nearly every cycle for 24 minutes and logged `offered: 0` with
	// every other count 0, which reads as the window being broken rather than
	// the load being light.
	logger.V(logging.DEFAULT).Info("itl-window",
		"variant", variantName, "key", key,
		"replicas", r.Considered, "offered", r.Offered, "held", r.Held,
		"notReady", r.NotReady, "noITL", r.NoITL, "noK", r.NoK,
		"belowBand", r.BelowBand, "aboveBand", r.AboveBand,
		// Of the offered, the readings above the band taken because their
		// replica was at its sequence cap (itl.Admission); batchCap is that
		// cap per rank -- the flags' max_num_seqs, lowered by
		// --max-num-active-seqs, or where they state none the
		// llm-d.ai/max-num-seqs annotation -- and 0 when neither states it,
		// which admits none. batchCapSource says which (flags, declared,
		// none).
		"capped", r.Capped, "batchCap", adm.MaxNumSeqs, "batchCapSource", adm.MaxNumSeqsSource,
		// Kept, but not for the full fit: the replica's load had just moved,
		// so its k and its ITL describe different moments (itl.DefaultSteadyFor).
		"unsteady", r.Unsteady, "steadyHeld", r.SteadyHeld,
		// Of those, the readings of a replica still inside its settle (it
		// joined, or the load came back to it): left out of the pinned fit
		// as well.
		"settling", r.Settling,
		// Ready needs minSamples steady readings AND this much spread among
		// them; without it a window that never becomes Ready does not say
		// which of the two it lacked.
		"steadySpread", r.SteadySpread, "minKSpread", itl.DefaultMinKSpread,
		"ready", r.Ready, "minSamples", itl.DefaultMinSamples)
	switch {
	case r.Tier == itl.TierOLS:
		logger.V(logging.DEFAULT).Info("itl-fit", "variant", variantName, "tier", "ols",
			"a", r.Model.A, "b", r.Model.B, "held", r.Held, "baselineLearned", true)
	case r.Tier == itl.TierPinnedB:
		// The fleet's replicas are at nearly the same load, so there is no
		// spread in k to fit a slope and an intercept from. That is the
		// NORMAL case, not a degenerate one: the router balances, so it
		// balances k too, and a run on 2026-09-24 sat at queue 0 on every
		// replica but one with the window never once Ready. Requiring the
		// full fit would have left the derivation inert on every fleet that
		// is actually working. So B is pinned to what THIS key measured and
		// only A is fitted -- the weaker answer, and still one about the
		// shape arriving now. The slope is fitted against whatever B is
		// pinned, so B decides the line's steepness as much as its offset.
		//
		// Downstream a derived mu both orders replicas (floor.Estimate's
		// mayOrder) and settles the whole variant's shape-change hold
		// (fleetHasMeasuredItself) on sight, which is why the tracker keeps
		// the sample floor Ready would have applied: only the spread is
		// waived. It costs about a cycle -- six replicas clear ten in two --
		// and nothing in the case this exists for: once the shape lightens,
		// an over-provisioned fleet sits at a k far below
		// DefaultMinObservableK and is priced from the readings the window
		// already holds from when it was working -- and, once those have aged
		// out, from the key's last slope (slopeHeld), so the line outlives a
		// long light stretch (issue ev-shindin/llm-scaling-manager#284).
		//
		// pinnedAtBPrior says the line was priced one-sided at the cap
		// (SwitchITLOneSidedAtCap): no learned B, and the prior's line (its
		// intercept the prior on B, capped at the window's least ITL, through
		// the mean of the readings at k 0.85 or more at or past the pricing
		// k) priced higher there than the fit through the default B, which it
		// replaced. priorBeyondReadings says the pricing k was past every
		// such reading, where that line's bound no longer holds.
		logger.V(logging.DEFAULT).Info("itl-fit", "variant", variantName, "tier", "pinned-B",
			"a", r.Model.A, "b", r.Model.B, "held", r.Held, "baselineLearned", r.BaselineStored,
			"slopeHeld", r.SlopeHeld, "tierHeld", r.TierHeld, "slopeAge", r.SlopeAge(now), "pinnedAtBPrior", r.PinnedAtBPrior,
			"priorBeyondReadings", r.PriorBeyondReadings)
	case r.Declined:
		logger.V(logging.DEFAULT).Info("itl-fit-declined", "variant", variantName, "held", r.Held)
	}
	return r
}
