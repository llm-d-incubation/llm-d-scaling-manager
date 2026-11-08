package throughput

import (
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
)

// Forgetting one variant's ITL line, on an operator's request, as the slo
// analyzer forgets its own (slo/forget.go). The forget-learned-state
// annotation is read by the engine, which acts on a token once, through the
// saturation analyzer's mark (saturation.ForgottenToken), and then forgets
// the variant in every analyzer that keeps a line. This analyzer keeps no
// token of its own: it forgets when the engine asks.
//
// Before, the line went on pricing from the readings taken before a forget,
// and held the slope fitted from them for up to the slope hold.
//
// What a forget takes, per field of ThroughputAnalyzer: what the line's
// tracker has learned of the key (lines: the window, the steady window, the
// readings above the band and their steady ones, the intercept and the held
// slope, and with the windows every settling stamp and every reading taken
// at the sequence cap), and what learner-write last logged of it (itlLogged).
// Of a variant's state (variantStates, forgetLine), the parent's window and
// B, and the figures last priced from the line. The sequence cap is not
// learned: the admission reads it from the store each cycle, and with B gone
// a line at the cap prices one-sided again.
//
// What it keeps: the replicas' traces in lines
// (itl.Tracker.ForgetKeysKeepingTraces), which say what each replica is doing
// now and fit nothing. The saturation and slo forgets drop theirs. Here a
// trace dropped would blind the steady gate for a cycle: a step in load on
// the first cycle after a forget would read as steady
// (TestForget_TheSteadyGateSeesTheFirstCycleAfter). And a replica's settle is
// read through its trace on every path, so one inside its settle stays
// inside it: with the engine's view, on the analyzer's own tracker, and on
// the key's own trace; with no trace, only the engine's view with its wait
// would keep it (TestForget_AReplicaSettlingAtAForgetStillSettlesOnEveryPath).
// Also kept: learnerReports and forgottenReports, which go to the engine and
// name the forget themselves; ownSettle and ownSettled, when each replica was last seen, which
// the engine's tracker keeps alike; the shape, the traffic now, which no line
// is learned from; the role, whether the variant is rolling out, and the log
// lines' memories of what they last said; paths, the replay's counters over every fit. The rest is
// configuration or plumbing. Two tests hold every field to one of those lists
// (TestForget_EveryFieldIsClassified,
// TestForget_EveryVariantStateFieldIsClassified).
//
// Nothing here brings a forgotten line back. This analyzer restores nothing:
// the learned-state snapshot holds the saturation analyzer's line, under
// keys of another shape, and no method here is named as a restore
// (TestForget_NothingHereRestoresALine). It borrows no line: a key is one
// variant's, so a sibling's readings are filed under the sibling. A lent
// replica's rows are refused by itl.Tracker (FromWarmPool), and ownReplicasOnly
// drops them before Analyze reaches it, as defence in depth
// (TestForget_NoSiblingOrLentReplicaRefillsTheLine).

// SwitchITLForgetReachesTheLine is the replay switch for the forget: on, the
// annotation forgets the variant's ITL line here too -- what its tracker
// learned of the key, the parent's window and B, and the figures priced from
// the line; off, as before, it forgets nothing here.
const SwitchITLForgetReachesTheLine = "throughput-itl-forget-reaches-the-line"

// forgetVariant forgets one variant's learned state in this analyzer: each
// learner on learn's list that it keeps, by name (forgetLearnerLocked). It
// returns how many entries it dropped and the keys it forgot, by the
// operator, sorted. With SwitchITLForgetReachesTheLine off it forgets
// nothing.
func (a *ThroughputAnalyzer) forgetVariant(namespace, modelID, variant string) (int, []learn.Forgotten) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.itlForgetReachesLine {
		return 0, nil
	}
	n := 0
	var forgotten []learn.Forgotten
	for _, learner := range learn.Learners() {
		dropped, keys, _ := a.forgetLearnerLocked(learner, namespace, modelID, variant)
		n += dropped
		forgotten = append(forgotten, keys...)
	}
	a.noteForgottenLocked(namespace, modelID, variant, forgotten)
	return n, forgotten
}

// forgetLearnerLocked forgets one learner's state for a variant, and returns
// how many entries it dropped, the keys it forgot, and whether it has code
// for the learner. The learners on learn.Learners are the saturation
// analyzer's names; this analyzer keeps one of them, the ITL line, as an
// instance of its own, and holds none of the others' state. A name it does
// not know is a learner put on the list without a decision here: logged as
// an error, and nothing is forgotten. A test walks the list and holds every
// name to handled. Callers hold a.mu.
func (a *ThroughputAnalyzer) forgetLearnerLocked(name, namespace, modelID, variant string) (dropped int, forgotten []learn.Forgotten, handled bool) {
	switch name {
	case learn.ITL:
		dropped, forgotten = a.forgetLineLocked(namespace, modelID, variant)
		return dropped, forgotten, true
	case learn.K2:
		// k2 is the saturation analyzer's alone: this analyzer learns none.
		return 0, nil, true
	case learn.Mu:
		// Mu is the saturation analyzer's alone: this analyzer keeps no
		// measured service rate.
		return 0, nil, true
	case learn.Start:
		// The start time is the saturation analyzer's alone: this analyzer
		// keeps no start estimate.
		return 0, nil, true
	}
	ctrl.Log.WithName(AnalyzerName).Error(nil, "learner on learn.Learners has no forget decision in the throughput analyzer; nothing forgotten",
		"learner", name, "namespace", namespace, "modelID", modelID, "variant", variant)
	return 0, nil, false
}

// forgetLineLocked forgets the variant's ITL line: what the tracker learned of
// its one key (windows and intercepts are counted, as itl.Tracker.Sweep
// counts them; the traces stay), what learner-write last logged of it, and
// what the variant's state holds (forgetLine, counted with them). It returns
// the count and the key, if anything of it was held, as forgotten by the
// operator. Callers hold a.mu; the tracker's lock is taken under it, as
// observe's sweep takes it.
func (a *ThroughputAnalyzer) forgetLineLocked(namespace, modelID, variant string) (int, []learn.Forgotten) {
	key := variantKey(namespace, modelID, variant)
	windows, baselines, forgotten := a.lines.ForgetKeysKeepingTraces(a.clock.Now(), learn.ByOperator, func(k string) bool { return k == key })
	delete(a.itlLogged, key)
	n := windows + baselines
	if state, ok := a.variantStates[key]; ok {
		parent := state.forgetLine()
		n += parent
		if parent > 0 && len(forgotten) == 0 {
			forgotten = append(forgotten, learn.Forgotten{Key: key, By: learn.ByOperator})
		}
	}
	return n, forgotten
}

// forgetLine drops what the state holds of the variant's line: the parent's
// window and B, which are its line with SwitchITLSteadyGate off, and each
// counts as one entry; and, not counted, this cycle's reading, the figures
// last priced from the line, which a snapshot would go on showing, and the
// arguments it was learned under. The next Analyze observes anew: the shape
// stays, so no shape change is read.
func (s *variantState) forgetLine() int {
	n := 0
	if s.observationWindow.Len() > 0 {
		n++
	}
	if s.hasFittedB {
		n++
	}
	s.observationWindow.Clear()
	s.lastFittedB, s.hasFittedB = 0, false
	s.lineArgs = ""
	s.consecutiveGPSMismatches = 0
	s.reading, s.hasReading = itl.Reading{}, false
	s.lastITLModel = itl.Model{}
	s.lastPerReplicaSupply, s.lastTotalSupply, s.lastDemand = 0, 0, 0
	return n
}
