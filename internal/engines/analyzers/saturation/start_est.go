package saturation

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
)

// How long a replica of this variant takes to become Ready, which the demand
// floor projects the arriving queue forward over.
//
// It has to be a measurement. Run T measured 67 s for four pods and 82 s for
// five while the controller ordered one replica per cycle for a minute and the
// queue reached 191 -- a projection over a guessed dead time is wrong by whatever
// the guess was, multiplied by the arrival rate.
//
// Per VARIANT, not per model: it is an image, a set of engine flags and a node,
// and two variants of one model routinely differ. Keyed exactly like the ITL
// windows so the two agree about what a variant is.

const (
	// DefaultReplicaStartSeconds is the fallback for a variant with no
	// measurement and no seed on its ScaledObject.
	//
	// 70 s, the middle of run T's measured 67-82 s for a 0.6B model on an H200.
	// Deliberately not generous: too long over-states the backlog that will
	// accumulate and over-orders. A GLM-5.2 cold start with a cold JIT cache is
	// nearer 500 s, which is why registry.ReplicaStartSecondsKey exists rather
	// than this number being asked to serve both.
	DefaultReplicaStartSeconds = 70.0

	// startSecondsAlpha weights a new measurement against the running estimate.
	//
	// 0.3 -- slow enough that one unlucky start does not move it far, fast
	// enough that a real change of image or node class is absorbed within a
	// handful of starts. Run T's own spread, 67 then 82, gives 71.5.
	startSecondsAlpha = 0.3

	// maxStartOutlierFactor bounds how far one sample may exceed the running
	// estimate before it is treated as something other than a start.
	//
	// The Ready condition's LastTransitionTime is re-stamped on EVERY
	// False->True transition, not only the first, so a readiness blip 45 minutes
	// into a pod's life reports 2700 s as its "start". Folded in at alpha 0.3
	// that moves a 67 s estimate to about 857 s, and the projection then
	// over-orders by an order of magnitude for the rest of the run. Nothing in
	// the Pod object distinguishes a first transition from a later one, so the
	// guard is on the value instead.
	maxStartOutlierFactor = 3.0

	// maxConsecutiveStartOutliers is how many rejections in a row are taken as
	// the world having changed rather than the samples being wrong.
	//
	// Without it a variant redeployed onto genuinely slower hardware would have
	// every sample rejected forever against an estimate that no longer describes
	// it -- the guard above would become the bug it was added to prevent.
	//
	// Read only with SwitchStartConfirmsByDistinctPod off: counted per
	// refusal, one Pod reporting its age on four cycles opens it.
	// startConfirmPods replaces it.
	maxConsecutiveStartOutliers = 3

	// startConfirmPods is how many distinct Pods must report a start over
	// maxStartOutlierFactor times the estimate before the guard takes the
	// world to have changed (SwitchStartConfirmsByDistinctPod).
	//
	// Unmeasured: 3 matches maxConsecutiveStartOutliers, the count it
	// replaces, and run T's scale-ups of 4 and 5 Pods, so a real change of
	// image or node is admitted within one scale-up. What it is counted in is
	// the point: one Pod whose Ready condition blipped reports its age every
	// cycle, and counting cycles let that one Pod open the guard on its
	// fourth. The Pods count across the run, not at once: a one-replica
	// variant rolled to a slower image three times opens it.
	startConfirmPods = 3

	// startOutlierRelog is how often a Pod still refused in an open run is
	// logged again: the rejection line is logged on the Pod's first refusal,
	// and a run can stay open for as long as no third Pod starts.
	startOutlierRelog = 10 * time.Minute
)

// The bounds a ScaledObject's replicaStartSeconds seed S holds the published
// estimate in while fewer than startConfirmPods Pods are measured
// (SwitchStartBlendsFirstSamples): [S/2, 2S].
//
// Unmeasured: wide enough that a first start a few times off the seed moves
// the estimate most of the way, narrow enough that one Pod reporting its age
// cannot. They bound the published figure only: sizing never reads a start
// shorter than one measured (startFiguresLocked).
const (
	startSeedLowFactor  = 0.5
	startSeedHighFactor = 2.0
)

// SwitchStartConfirmsByDistinctPod is the replay switch for learner PR 5b
// (analyzer-learner.md, PR 5, change B): the start guard opens when
// startConfirmPods distinct Pods have reported a start over
// maxStartOutlierFactor times the estimate, and one Pod repeating never
// opens it. Off, it opens after maxConsecutiveStartOutliers refusals,
// counted per refusal, so one Pod reporting its age on four cycles folds
// it. On from the constructor; a replay turns it off (SetSwitchesForReplay).
const SwitchStartConfirmsByDistinctPod = "start-confirms-by-distinct-pod"

// SwitchStartReadsSeed is the replay switch for learner PR 5c's change A
// (analyzer-learner.md, PR 5): the ScaledObject's replicaStartSeconds seed
// is the prior. A seeded variant with no start measured is sized at the
// seed, so the floor projects its backlog over it and share pacing reads it;
// the blend's prior and bounds are the seed; and the estimate gauge's source
// says default, seed or measured. Off, the seed is read by nothing, as in
// 5a, and the gauge labels the 70 s default "seed". On from the constructor;
// a replay turns it off (SetSwitchesForReplay).
const SwitchStartReadsSeed = "start-reads-replica-start-seed"

// SwitchStartBlendsFirstSamples is the replay switch for learner PR 5c's
// change C: while fewer than startConfirmPods Pods of a key are measured,
// the published estimate (the gauge, the log, learner start) blends the
// prior with the measured estimate, and sizing reads the larger of the blend
// and the measured estimate, never a start shorter than one measured. The
// key's sample count and last admitted start are kept and stored in the
// snapshot. Off, the measured estimate is published and sized from its
// first sample, as in 5b. On from the constructor; a replay turns it off.
const SwitchStartBlendsFirstSamples = "start-blends-first-samples"

// SwitchStartCountedPodsRestored is the replay switch for learner PR 5d's
// change D (analyzer-learner.md, PR 5): the snapshot keeps each counted Pod's
// Ready time, and after a controller restart a Pod reporting the same Ready
// time is counted already, so it is not folded again. A Pod with no Ready
// time, or another one, is folded as before. Off, every running Pod is folded
// again after a restart, and nothing is stored. On from the constructor; a
// replay turns it off (SetSwitchesForReplay).
const SwitchStartCountedPodsRestored = "start-counted-pods-restored"

// The estimate gauge's sources (llmsm_replica_start_seconds_estimate), and the
// start learner's: the 70 s default, the ScaledObject's seed, or a start
// measured on a Pod of the variant.
const (
	startSourceDefault  = metrics.ReplicaStartSourceDefault
	startSourceSeed     = metrics.ReplicaStartSourceSeed
	startSourceMeasured = metrics.ReplicaStartSourceMeasured
	// startSourceBlended is the gauge's alone: a blend of the prior with
	// fewer than startConfirmPods measured Pods.
	startSourceBlended = metrics.ReplicaStartSourceBlended
)

// startNote names what a cycle's start samples are noted for: the start key,
// the variant, and what the start learner reports it under. modelID empty
// files no learner report (a direct call from a test of the fold).
type startNote struct {
	key, namespace, modelID, variant, role string
	// seed is the variant's replicaStartSeconds, 0 when unset.
	seed float64
}

// startFigures is a key's start estimate as each reader takes it
// (startFiguresLocked).
type startFigures struct {
	// published is what the gauge, the log and learner start show: the
	// blend while fewer than startConfirmPods Pods are measured, else the
	// measured estimate; the prior when nothing is measured.
	published float64
	// sized is what the floor's projection and share pacing read: the
	// largest of the blend, the measured mean and the measured estimate;
	// the seed for a seeded, unmeasured key; 0 for an unseeded, unmeasured
	// one, which is not projected.
	sized float64
	// measured is the measured estimate (startSeconds), 0 when none. The
	// 5b guard compares with it, never with the blend.
	measured float64
	// mean is the plain mean of the counted Pods' starts while fewer than
	// startConfirmPods are counted, else the measured estimate.
	mean float64
	// pods is how many distinct Pods the estimate is from, at most
	// startConfirmPods: a key at the cap, or measured before the count was
	// kept, has full authority.
	pods int
	// prior and priorSource are the blend's prior: the seed, or the default.
	prior       float64
	priorSource string
	// source is startSourceMeasured, startSourceSeed or startSourceDefault.
	// A blended estimate's source is measured: a measured variant still wins
	// the floor's start over a seed (startSecondsPerRole).
	source string
	// blended says published is a blend of the prior with fewer than
	// startConfirmPods measured Pods; the gauge then says so
	// (startGaugeSourceLocked).
	blended bool
}

// startFiguresLocked is the start estimate of key, a variant seeded with
// seed (0 when unset), as published and as sized. Callers hold a.mu.
//
// With n < startConfirmPods (N) Pods counted, m their mean start and E the
// measured estimate (startSeconds):
//
//	published = ((N - n) * P + n * m) / N, held in [S/2, 2S] with a seed S
//	sized     = max(published, m, E)
//
// E is in the max because the fold keeps an EWMA, not a mean: two Pods
// starting in 460 s and then 15 s give a mean of 237.5 s and an estimate of
// 326.5 s, and sizing at the mean alone would read a shorter start than the
// parent sized at. Sizing errs toward a longer start only, which on a
// scale-up projects more backlog and orders more. From N Pods on, every
// reader takes E.
func (a *SaturationAnalyzer) startFiguresLocked(key string, seed float64) startFigures {
	if !a.startReadsSeed || !(seed > 0) {
		seed = 0
	}
	f := startFigures{prior: DefaultReplicaStartSeconds, priorSource: startSourceDefault}
	if seed > 0 {
		f.prior, f.priorSource = seed, startSourceSeed
	}
	e, measured := a.startSeconds[key]
	if !measured || !(e > 0) {
		f.published, f.source = f.prior, f.priorSource
		f.sized = seed
		return f
	}
	f.measured, f.mean, f.published, f.sized, f.source = e, e, e, e, startSourceMeasured
	f.pods = startConfirmPods
	if !a.startBlends {
		return f
	}
	s, counting := a.startSamples[key]
	if !counting || s.pods >= startConfirmPods || !(s.mean > 0) {
		return f
	}
	f.pods, f.mean = s.pods, s.mean
	blend := (float64(startConfirmPods-s.pods)*f.prior + float64(s.pods)*s.mean) / startConfirmPods
	if seed > 0 {
		blend = min(max(blend, startSeedLowFactor*seed), startSeedHighFactor*seed)
	}
	f.published, f.blended = blend, true
	f.sized = max(blend, s.mean, e)
	return f
}

// startSample is a key's count of the distinct Pods whose start it admitted,
// and their mean start, while fewer than startConfirmPods
// (SwitchStartBlendsFirstSamples). A key with none has full authority.
type startSample struct {
	pods int
	mean float64
}

// noteStartSampleLocked stamps when key last admitted a start, ends its
// restored mark, and counts the start, sample seconds, toward the blend
// (SwitchStartBlendsFirstSamples). A key measured before the count was kept
// (restored from a document without it) has full authority already, and is
// not counted again; a key that reaches startConfirmPods drops its count,
// which reads as full authority. Off, nothing is kept, so the snapshot holds
// what the parent's did. Callers hold a.mu.
func (a *SaturationAnalyzer) noteStartSampleLocked(key string, sample float64, wasMeasured bool, now time.Time) {
	if !a.startBlends {
		return
	}
	a.startAdmittedAt[key] = now
	delete(a.startRestored, key)
	s, counting := a.startSamples[key]
	if wasMeasured && !counting {
		return
	}
	if s.pods+1 >= startConfirmPods {
		delete(a.startSamples, key)
		return
	}
	s.mean = (float64(s.pods)*s.mean + sample) / float64(s.pods+1)
	s.pods++
	a.startSamples[key] = s
}

// startGaugeSourceLocked is the estimate gauge's source for f: default,
// seed, blended or measured with SwitchStartReadsSeed on; off, the
// parent's, which labels the 70 s default "seed" (and, the blend on, a
// blend "measured"). Callers hold a.mu.
func (a *SaturationAnalyzer) startGaugeSourceLocked(f startFigures) string {
	switch {
	case !a.startReadsSeed:
		return startSource(f.source == startSourceMeasured)
	case f.blended:
		return startSourceBlended
	}
	return f.source
}

// maxRestoredStartSeconds bounds a restored mean start
// (installStartLearnerLocked): the collector reports no start over an hour
// (collector.maxCredibleStartSeconds), so no live mean exceeds it.
const maxRestoredStartSeconds = 3600.0

// noteReplicaStartFor folds this cycle's observed start times into the
// per-variant estimate of note's key, publishes both the observation and the
// estimate the seed and the blend make of it, files the start learner's
// report (SwitchStartBlendsFirstSamples), and forgets the replicas that have
// gone. A note with no model files no report.
//
// Warm-pool bridges are skipped. A bridged Pod reaching Ready in a second is a
// wake, not a start, and says nothing about how long this variant's own replicas
// take -- the same exclusion noteITL makes for the fit and floor.Estimate makes
// for the price.
//
// An observation is counted ONCE per replica. Every cycle sees the same Ready Pod
// reporting the same StartSeconds, so folding it in repeatedly would drive the
// estimate to whatever the longest-lived replica measured and make the histogram
// a count of cycles rather than of starts.
//
// The bookkeeping for that is bounded HERE, by forgetting Pods this variant no
// longer reports. EvictStaleHistory sweeps it too, as a backstop for a variant
// that disappears entirely -- it DOES have a caller on the reconcile path now
// (steadystate.evictStaleLearnedState, once per cycle), which is why the
// already-counted branch below re-stamps rather than leaving an entry to
// expire under a live Pod. A set keyed per Pod is the fastest-growing state on this
// struct, so it cannot be left to a sweep that does not run.
func (a *SaturationAnalyzer) noteReplicaStartFor(
	note startNote,
	replicas []domain.ReplicaMetrics,
	logger logr.Logger,
) {
	key, namespace, variantName := note.key, note.namespace, note.variant
	type outlier struct {
		pod     string
		sample  float64
		against float64
		// byPod says SwitchStartConfirmsByDistinctPod counted it: the line
		// then names the run's distinct Pods.
		byPod bool
	}
	var rejected []outlier
	observed := 0
	present := make(map[string]struct{}, len(replicas))
	// endsRun is an in-range start from a Pod not in the open run, this
	// cycle: applied after every sample is judged, so it does not depend on
	// the order the Pods are listed in (SwitchStartConfirmsByDistinctPod).
	endsRun := false

	a.mu.Lock()
	// This runs for every role, so it is what bounds the start estimate of a
	// prefill or RoleBoth variant: noteITL stamps decode keys only, and the
	// sweep that used to reach these maps through the ITL window map could
	// never visit a key that has no window.
	now := a.now()
	a.noteVariantSeen(key, now)
	for _, rm := range replicas {
		if seen, ok := startPodKey(key, variantName, rm); ok {
			present[seen] = struct{}{}
		}
	}
	for _, rm := range replicas {
		seen, ok := startPodKey(key, variantName, rm)
		if !ok {
			continue
		}

		// Re-stamp, ABOVE the StartSeconds gate below. The entry is what says
		// "this Pod's start is already in the estimate", and it used to be
		// written once and never refreshed -- which was safe only while
		// evictStartSeenPods had no caller. With the per-cycle sweep wired up,
		// a Pod Ready for longer than the timeout lost this record and its
		// start was folded into the EWMA a second time, then once per timeout
		// for the rest of its life. That breaks the invariant above: the
		// estimate would drift toward whatever the longest-lived replica
		// measured, and llmsm_replica_start_seconds would count cycles rather
		// than starts.
		//
		// It must sit above the gate because StartSeconds reads 0 for a Pod
		// that is still being reported: collector.podStartSeconds returns 0
		// when the Pod is not Ready, when the Pod list could not be read at
		// all, and when the Ready transition is more than
		// maxCredibleStartSeconds after creation -- and a readiness
		// re-transition rewrites LastTransitionTime, so a Pod can re-enter
		// that last case. Gated below the StartSeconds check, such a Pod
		// stayed in `present` (so the prefix cleanup kept its record) and was
		// never re-stamped, so the sweep expired it under a live Pod and the
		// next reading folded the same Pod in twice. A counted Pod needs
		// nothing else from this loop body whatever its StartSeconds reads.
		if _, counted := a.startSeenPods[seen]; counted {
			a.startSeenPods[seen] = a.now()
			continue
		}
		if a.claimRestoredCountLocked(seen, rm) {
			a.startSeenPods[seen] = now
			continue
		}
		if !(rm.StartSeconds > 0) {
			continue
		}
		// The guard compares with the measured estimate, the only one
		// startSeconds holds: a sample is never refused against a figure no
		// Pod of this key measured.
		cur, measured := a.startSeconds[key]
		if measured && cur > 0 && rm.StartSeconds > maxStartOutlierFactor*cur {
			if a.startConfirmsByPod {
				// Refused, and joins the run (noteStartRefusalLocked); the
				// guard opens after the loop, on the run's distinct Pods.
				if a.noteStartRefusalLocked(key, seen, rm.PodName, rm.StartSeconds, now) {
					rejected = append(rejected, outlier{rm.PodName, rm.StartSeconds, cur, true})
				}
				// NOT recorded as seen, as below.
				continue
			}
			if a.startOutliers[key] < maxConsecutiveStartOutliers {
				a.startOutliers[key]++
				rejected = append(rejected, outlier{rm.PodName, rm.StartSeconds, cur, false})
				// NOT recorded as seen: if the next cycle still reports it, it is
				// counted toward the consecutive run above rather than forgotten.
				continue
			}
		}
		// An admitted sample ends the run of refusals: the guard needs
		// consecutive evidence. Counted per refusal, at once; counted by Pod,
		// after the loop, and only for a Pod the run does not hold -- a run
		// Pod whose start is now in range just leaves the run.
		a.startOutliers[key] = 0
		if run := a.startOutlierPods[key]; a.startConfirmsByPod && run != nil {
			if _, inRun := run[seen]; inRun {
				delete(run, seen)
			} else {
				endsRun = true
			}
		}
		a.startSeenPods[seen] = now
		observed++
		a.noteStartSampleLocked(key, rm.StartSeconds, measured && cur > 0, now)

		if measured && cur > 0 {
			a.startSeconds[key] = (1-startSecondsAlpha)*cur + startSecondsAlpha*rm.StartSeconds
		} else {
			// The first measurement REPLACES the seed rather than being averaged
			// with it. The seed is a guess and the measurement is not, so giving
			// the guess most of the weight of the first real figure would keep a
			// wrong number in force for several starts.
			a.startSeconds[key] = rm.StartSeconds
		}
		metrics.ObserveReplicaStartSeconds(namespace, variantName, rm.StartSeconds)
	}
	var confirmed *startConfirmation
	if a.startConfirmsByPod {
		confirmed = a.closeStartRunLocked(key, endsRun, present, now)
		if confirmed != nil {
			// startConfirmPods Pods agreed: the median has full authority.
			if a.startBlends {
				delete(a.startSamples, key)
				delete(a.startRestored, key)
				a.startAdmittedAt[key] = now
			}
			observed += len(confirmed.samples)
			for _, s := range confirmed.samples {
				metrics.ObserveReplicaStartSeconds(namespace, variantName, s)
			}
		}
	}
	// The run's distinct Pods, AFTER the cycle closed it: 0 when a start in
	// range ended it this cycle, so a rejection line never reports a run
	// that no longer exists. (A run that confirmed logs no rejection line.)
	runPods := len(a.startOutlierPods[key])

	a.noteCountedReadyLocked(key, variantName, replicas)

	// Forget this variant's Pods that no longer report. Scoped by the key
	// prefix so one variant's cycle cannot drop another's bookkeeping.
	prefix := key + "|"
	for k := range a.startSeenPods {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if _, still := present[k]; !still {
			delete(a.startSeenPods, k)
		}
	}
	for _, counted := range []map[string]time.Time{a.startCountedReady, a.startCountedRestored} {
		maps.DeleteFunc(counted, func(k string, _ time.Time) bool {
			_, still := present[k]
			return strings.HasPrefix(k, prefix) && !still
		})
	}
	f := a.startFiguresLocked(key, note.seed)
	contradicted := a.noteSeedContradictionLocked(key, f)
	gaugeSource := a.startGaugeSourceLocked(f)
	blends := a.startBlends
	write := a.noteStartLearnerLocked(note, f, observed, now)
	a.mu.Unlock()

	metrics.SetReplicaStartSecondsEstimate(namespace, variantName, f.published, gaugeSource)
	for _, o := range rejected {
		if o.byPod && confirmed != nil {
			// The run confirmed this cycle: its line names the Pods.
			continue
		}
		kv := []any{"namespace", namespace, "variant", variantName, "pod", o.pod,
			"sampleSeconds", o.sample, "estimateSeconds", o.against,
			"factor", maxStartOutlierFactor}
		// Off, the parent's line, byte for byte.
		reason := "the Ready condition re-transitions, so a probe blip reports the pod's age"
		if o.byPod {
			kv = append(kv, "distinctPods", runPods, "needed", startConfirmPods)
			reason = "start-above-bound"
		}
		logger.V(logging.DEFAULT).Info("replica-start-outlier-rejected", append(kv, "reason", reason)...)
	}
	if confirmed != nil {
		logger.V(logging.DEFAULT).Info("replica-start-change-confirmed",
			"namespace", namespace, "variant", variantName,
			"distinctPods", len(confirmed.samples), "medianSeconds", confirmed.median,
			"previousEstimateSeconds", confirmed.previous)
	}
	if observed > 0 {
		kv := []any{"namespace", namespace, "variant", variantName, "observations", observed,
			"estimateSeconds", f.published, "source", startSource(f.source == startSourceMeasured)}
		if blends {
			// Off, the parent's line, byte for byte.
			kv = append(kv, "pods", f.pods, "prior", f.prior, "priorSource", f.priorSource,
				"sizedSeconds", f.sized)
		}
		logger.V(logging.DEFAULT).Info("replica-start-measured", kv...)
	}
	write.log(logger)
	if contradicted {
		// The seed holds the published estimate in [S/2, 2S], and so sizing
		// at no less than half the seed, until startConfirmPods Pods are
		// measured, which for a one-replica variant can take days and
		// survives a restart (the count is in the snapshot).
		logger.V(logging.DEFAULT).Info(StartSeedContradictedMessage,
			"namespace", namespace, "variant", variantName,
			"replicaStartSeconds", f.prior, "measuredSeconds", f.mean, "pods", f.pods,
			"lowSeconds", startSeedLowFactor*f.prior, "highSeconds", startSeedHighFactor*f.prior,
			"publishedSeconds", f.published, "sizedSeconds", f.sized, "podsNeeded", startConfirmPods)
	}
}

// claimRestoredCountLocked says whether the Pod seen (key|namespace|pod), not
// counted in this process, is one the snapshot counted: its row reports the
// Ready time stored for it (SwitchStartCountedPodsRestored). A row that
// reports neither a Ready time nor a start says nothing, a Pod list that
// could not be read among them, and the stored time waits for the next. Any
// other row settles it: a Pod with no Ready time, or another one, is folded
// as a new Pod is. Callers hold a.mu.
func (a *SaturationAnalyzer) claimRestoredCountLocked(seen string, rm domain.ReplicaMetrics) bool {
	stored, ok := a.startCountedRestored[seen]
	if !ok {
		return false
	}
	ready := a.readySince(rm)
	if ready.IsZero() && !(rm.StartSeconds > 0) {
		return false
	}
	delete(a.startCountedRestored, seen)
	return !ready.IsZero() && ready.Equal(stored)
}

// noteCountedReadyLocked keeps, for each Pod of key counted this cycle, the
// Ready time its row reports, which is what the snapshot stores
// (SwitchStartCountedPodsRestored). The latest, not the one it was counted
// at: a Pod whose readiness blipped stays counted, and reports its new time
// on the far side of a restart. A row with no Ready time keeps the last one.
// Callers hold a.mu.
func (a *SaturationAnalyzer) noteCountedReadyLocked(key, variantName string, replicas []domain.ReplicaMetrics) {
	if !a.startCountsRestored {
		return
	}
	for _, rm := range replicas {
		seen, ok := startPodKey(key, variantName, rm)
		if !ok {
			continue
		}
		if _, counted := a.startSeenPods[seen]; !counted {
			continue
		}
		if ready := a.readySince(rm); !ready.IsZero() {
			a.startCountedReady[seen] = ready
		}
	}
}

// startCountedKeyOf is the start key a counted Pod's key
// (key|namespace|pod) was built from, and false for a key too short to be
// one.
func startCountedKeyOf(counted string) (string, bool) {
	i := strings.LastIndexByte(counted, '|')
	if i < 0 {
		return "", false
	}
	i = strings.LastIndexByte(counted[:i], '|')
	if i <= 0 {
		return "", false
	}
	return counted[:i], true
}

// StartSeedContradictedMessage is logged once per variant key and seed, and
// again after a controller restart, while the mean of the variant's measured
// replica starts lies outside [S/2, 2S] of its replicaStartSeconds seed S and
// fewer than startConfirmPods Pods are measured: the seed is wrong, and
// holds the estimate until they are.
//
// Its bounds and Pod count are built from startSeedLowFactor,
// startSeedHighFactor and startConfirmPods, so the text cannot drift from the
// rule.
var StartSeedContradictedMessage = fmt.Sprintf("WARNING: A variant's replicaStartSeconds is contradicted by its measured starts: "+
	"the start estimate is held between %g and %g times the seed until %d of its pods have started. "+
	"Correct or remove the seed on the ScaledObject", startSeedLowFactor, startSeedHighFactor, startConfirmPods)

// noteSeedContradictionLocked says whether f is a blend whose measured mean
// lies outside [S/2, 2S] of its seed S, and records that it said so for that
// seed, so the warning is logged once per key and seed (and again with a new
// seed, or after a restart: startSeedWarned is working state). With either
// 5c switch off there is no blend and nothing to warn of. Callers hold a.mu.
func (a *SaturationAnalyzer) noteSeedContradictionLocked(key string, f startFigures) bool {
	if !f.blended || f.priorSource != startSourceSeed {
		return false
	}
	s := f.prior
	if f.mean >= startSeedLowFactor*s && f.mean <= startSeedHighFactor*s {
		return false
	}
	if warned, ok := a.startSeedWarned[key]; ok && warned == s {
		return false
	}
	a.startSeedWarned[key] = s
	return true
}

// startPodKey is the key a replica's start is counted under
// (key|namespace|pod), and false for a replica whose start is not one of this
// variant's: another variant's row, a warm-pool bridge, or a row with no Pod.
//
// Qualified by the variant's window key AND the namespace. A bare Pod name is
// not unique: a StatefulSet or LeaderWorkerSet reuses deterministic names
// across incarnations, and the same release in two namespaces gives two Pods
// the same name -- either of which would have one variant's measurement
// suppress another's silently. Including the window key also means a variant
// whose key changes (its accelerator resolving differently, its GPU count
// changing) re-measures from the Pods it can already see instead of falling
// back to the seed for the life of those Pods.
func startPodKey(key, variantName string, rm domain.ReplicaMetrics) (string, bool) {
	if rm.VariantName != variantName || rm.FromWarmPool || rm.PodName == "" {
		return "", false
	}
	return key + "|" + rm.Namespace + "|" + rm.PodName, true
}

// startRunPod is one Pod of a key's open run of refused starts
// (startOutlierPods).
type startRunPod struct {
	// pod is the Pod's name, for the log.
	pod string
	// sample is the start it last reported: what the run folds when it
	// confirms.
	sample float64
	// loggedAt is when its rejection was last logged (startOutlierRelog).
	loggedAt time.Time
}

// startConfirmation is a run that confirmed a change of start time
// (closeStartRunLocked).
type startConfirmation struct {
	// samples is each distinct Pod's start, sorted.
	samples []float64
	// median is the estimate the run installed, and previous the one it
	// replaced.
	median   float64
	previous float64
}

// noteStartRefusalLocked adds the Pod seen (key|namespace|pod), whose start
// is over maxStartOutlierFactor times key's estimate, to key's run, or
// updates its sample when it is already there, and says whether to log the
// rejection: on the Pod's first refusal in the run, and again each
// startOutlierRelog while it keeps being refused. The same Pod reporting
// again adds no Pod to the run, so one Pod never opens the guard however
// many cycles it reports. Callers hold a.mu.
func (a *SaturationAnalyzer) noteStartRefusalLocked(key, seen, pod string, sample float64, now time.Time) bool {
	run := a.startOutlierPods[key]
	if run == nil {
		run = make(map[string]startRunPod, startConfirmPods)
		a.startOutlierPods[key] = run
	}
	e, again := run[seen]
	logIt := !again || now.Sub(e.loggedAt) >= startOutlierRelog
	e.pod, e.sample = pod, sample
	if logIt {
		e.loggedAt = now
	}
	run[seen] = e
	return logIt
}

// closeStartRunLocked applies what this cycle's judged samples decided of
// key's run, after every sample is judged, so the outcome does not depend on
// the order the Pods were listed in:
//
//   - endsRun (an in-range start from a Pod not in the run): the run ends,
//     and every Pod in it is forgotten. The guard needs consecutive evidence.
//   - startConfirmPods or more distinct Pods: the guard opens. The estimate
//     becomes the MEDIAN of the run's starts, one per Pod, and the run's
//     Pods still reporting are counted as seen. Returned, for the log and
//     the histogram.
//   - otherwise the run stays open. Pods that left stay in it: a variant
//     with one or two replicas never has three slow Pods up at once, and
//     dropping a Pod when it left kept such a variant on its old estimate
//     for good.
//
// Why the median and not the EWMA. All startConfirmPods Pods were refused as
// implausible against the old estimate; that is the evidence the variant
// changed. Folding them at alpha keeps most of the weight on the figure they
// refute (70 s to 3000 s stalled near 949 s, and the Pods are then counted,
// so nothing moves it further until the next scale-up). The median takes the
// new level from the Pods themselves and is robust to one of them being a
// blip in the run: two Pods at 300 s and one reporting its age at 2700 s give
// 300 s, where the mean gives 1100 s.
//
// The run is bounded: between cycles it holds at most startConfirmPods-1
// Pods, since a cycle that brings it to startConfirmPods opens the guard and
// empties it. Callers hold a.mu.
func (a *SaturationAnalyzer) closeStartRunLocked(
	key string, endsRun bool, present map[string]struct{}, now time.Time,
) *startConfirmation {
	run := a.startOutlierPods[key]
	switch {
	case endsRun || len(run) == 0:
		delete(a.startOutlierPods, key)
		return nil
	case len(run) < startConfirmPods:
		return nil
	}
	c := &startConfirmation{previous: a.startSeconds[key]}
	for seen, e := range run {
		c.samples = append(c.samples, e.sample)
		if _, here := present[seen]; here {
			a.startSeenPods[seen] = now
		}
	}
	slices.Sort(c.samples)
	n := len(c.samples)
	c.median = c.samples[n/2]
	if n%2 == 0 {
		c.median = (c.samples[n/2-1] + c.samples[n/2]) / 2
	}
	a.startSeconds[key] = c.median
	delete(a.startOutlierPods, key)
	return c
}

// startSecondsByRole is how long one replica of each role takes to become Ready,
// from the per-variant estimates as sized (startFiguresLocked), and where
// each role's figure came from: measured, or seed.
//
// The MINIMUM over a role's measured variants, not the maximum or the mean: it
// is the first relief to arrive, and the projection is what gets ordered on.
// Taking the slowest would project more arrivals and order more replicas on a
// role whose variants differ -- the wrong direction to err, given the failure
// this whole file is chasing is a fleet that over-orders and cannot come back
// down.
//
// A seed enters only for a role with no measured variant
// (SwitchStartReadsSeed): beside a measured sibling, a short seed would be
// the minimum and project less backlog than the measurement does. A role with
// neither is absent from the result, which leaves the floor on its observed
// backlog for that role.
func (a *SaturationAnalyzer) startSecondsByRole(
	input domain.AnalyzerInput, roleOf map[string]string,
) roleStarts {
	sized, source, parent := a.startSecondsPerRole(input, roleOf, false)
	return roleStarts{sized: sized, source: source, parent: parent}
}

// roleStarts is each role's start time for the floor's projection
// (startSecondsByRole).
type roleStarts struct {
	// sized is the role's start as 5c sizes it, and source where it came
	// from: measured, or seed.
	sized  map[string]float64
	source map[string]string
	// parent is the role's start as 5b sized it: the fastest measured
	// variant's measured estimate, no seed and no blend; a role with none is
	// absent. The floor prices both and keeps the larger order
	// (applyThroughputFloor), because a longer start does not always order
	// more: on a fleet that drains faster than work arrives it projects more
	// drain, and past the 60 s drain time it spreads the backlog over a
	// longer horizon. Equal to sized with both 5c switches off.
	parent map[string]float64
}

// slowestStartSecondsByRole is the MAXIMUM over each role's variants as
// sized, seeded ones included, for a consumer that must not act before a
// replica serves (domain.AnalyzerResult.RoleStartSeconds): the
// utilization-share planner paces its moves on how long a moved replica takes
// to land, and erring short there moves GPUs for a burst that ends before
// they serve. A seed longer than a measured sibling lengthens it; a shorter
// one cannot be the maximum.
//
// seeded names each role whose figure is a seed, none of its variants
// measured (domain.AnalyzerResult.RoleStartSeeded), so share pacing can say
// its start came from the ScaledObject, not a measurement. A role with a
// measured variant is never named, even when a slower seed is its maximum:
// the figure is then the seed only in part of the role.
func (a *SaturationAnalyzer) slowestStartSecondsByRole(
	input domain.AnalyzerInput, roleOf map[string]string,
) (out map[string]float64, seeded map[string]bool) {
	out, sources, parent := a.startSecondsPerRole(input, roleOf, true)
	for role, src := range sources {
		if _, measured := parent[role]; src == startSourceSeed && !measured {
			if seeded == nil {
				seeded = map[string]bool{}
			}
			seeded[role] = true
		}
	}
	return out, seeded
}

// startSecondsPerRole folds the per-variant estimates as sized into one per
// role, and names each role's source: the slowest variant when slowest is
// set, else the fastest measured one, or the fastest seed when the role has
// no measured variant. A role with neither is absent. parent is the same
// fold over the measured estimates alone, as 5b sized them.
func (a *SaturationAnalyzer) startSecondsPerRole(
	input domain.AnalyzerInput, roleOf map[string]string, slowest bool,
) (sized map[string]float64, sources map[string]string, parent map[string]float64) {
	accel := make(map[string]string, len(input.VariantStates))
	gpus := make(map[string]int, len(input.VariantStates))
	seeds := make(map[string]float64, len(input.VariantStates))
	for _, vs := range input.VariantStates {
		accel[vs.VariantName] = vs.AcceleratorName
		gpus[vs.VariantName] = vs.GPUsPerReplica
		if vs.StartSeedSet {
			seeds[vs.VariantName] = vs.StartSeedSeconds
		}
	}

	// Keys FIRST, outside the lock. itlWindowKey reaches stableAccelerator,
	// which takes a.mu -- and sync.Mutex is not reentrant, so resolving a key
	// while holding it deadlocks the whole analyzer. noteITL documents the same
	// hazard where it drops the lock before logging; this is that hazard.
	keys := make(map[string]string, len(roleOf))
	for variant := range roleOf {
		keys[variant] = a.itlWindowKey(input.Namespace, input.ModelID, variant,
			accel[variant], gpus[variant])
	}

	out := make(map[string]float64, 2)
	source := make(map[string]string, 2)
	parent = make(map[string]float64, 2)
	a.mu.Lock()
	defer a.mu.Unlock()
	// Sorted, so a tie between two variants of one role names the same
	// source every cycle.
	for _, variant := range slices.Sorted(maps.Keys(roleOf)) {
		role := roleOf[variant]
		f := a.startFiguresLocked(keys[variant], seeds[variant])
		if e := f.measured; e > 0 {
			if cur, seen := parent[role]; !seen || (slowest && e > cur) || (!slowest && e < cur) {
				parent[role] = e
			}
		}
		if !(f.sized > 0) {
			continue
		}
		est := f.sized
		cur, seen := out[role]
		var better bool
		switch {
		case !seen:
			better = true
		case slowest:
			better = est > cur
		case f.source != source[role]:
			// The fastest: a measured variant replaces a seed, and a seed
			// never replaces a measurement.
			better = f.source == startSourceMeasured
		default:
			better = est < cur
		}
		if better {
			out[role], source[role] = est, f.source
		}
	}
	return out, source, parent
}

// startSource labels the estimate for the log and the metric, so a run says
// whether it sized against a measurement or a guess.
func startSource(measured bool) string {
	if measured {
		return "measured"
	}
	return "seed"
}

// evictStartSeenPods drops bookkeeping older than the timeout.
//
// A backstop only: noteReplicaStartFor already forgets a Pod the cycle it stops
// reporting, which is what actually bounds the map. This exists for the one
// case that cannot reach -- a variant which disappears entirely, never
// reported again, so never pruned by its own cycle. EvictStaleHistory now has
// a caller (steadystate.evictStaleLearnedState), so this runs every cycle, and
// noteReplicaStartFor re-stamps a Pod it still sees precisely so that a live Pod
// is never swept out from under its own "already counted" record.
func (a *SaturationAnalyzer) evictStartSeenPods(now time.Time, timeout time.Duration) int {
	dropped := 0
	for pod := range a.startCountedRestored {
		if key, ok := startCountedKeyOf(pod); !ok || a.variantIsStale(key, now, timeout) {
			delete(a.startCountedRestored, pod)
		}
	}
	for pod, seen := range a.startSeenPods {
		if now.Sub(seen) > timeout {
			delete(a.startSeenPods, pod)
			delete(a.startCountedReady, pod)
			dropped++
		}
	}
	return dropped
}
