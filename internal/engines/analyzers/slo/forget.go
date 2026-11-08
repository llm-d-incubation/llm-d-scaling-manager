package slo

import (
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
)

// Forgetting one variant's learned state, on an operator's request (issue
// ev-shindin/llm-scaling-manager#431). The forget-learned-state annotation is read by the engine, which
// acts on a token once, through the saturation analyzer's mark
// (saturation.ForgottenToken), and then forgets the variant in every
// analyzer that keeps a line. This analyzer keeps no token of its own: it
// forgets when the engine asks.
//
// Before, the annotation reached the saturation analyzer alone. This
// analyzer fits its own ITL line with its own itl.Tracker, so after a forget
// its line still priced from the readings taken before it, and held the
// slope fitted from them for up to the slope hold.
//
// What a forget takes, per field of the Analyzer: the line (lines), the
// engine arguments it was learned under (args), its last-seen stamp (seen),
// what learner-write last logged of it (lineLogged),
// the accelerator memo (accelerators), as the saturation analyzer drops its
// lastAccelerator: a variant forgotten after a move to new hardware would
// otherwise key its next line under the old card for any cycle whose pods do
// not resolve one; and the TTFT windows (imbalance, decodeTTFT, decodeTerms,
// prompts).
//
// What it keeps: transients, the model's recent fleet history, which is no
// learned state of the variant; ownSettled (and ownSettle), when each replica
// was last seen, which the line reads its settle from and the engine's tracker
// keeps alike; paths, the replay's counters over every fit; learnerReports and
// forgottenReports, which go to the engine and name the forget themselves.
//
// The rest is configuration or plumbing (router, store, now, mu, rec, settle,
// prefillEstimates: the last is a handle to the saturation analyzer's stash,
// whose forget deletes the variant's entry there). A test holds every field
// that is not a plain value to one of the three lists
// (TestForget_EveryFieldIsClassified).

// SwitchITLForgetReachesTheLine is the replay switch for the forget: on, the
// annotation forgets the variant's ITL line here too, under every key it was
// filed under -- its window, steady window, traces, intercept and held
// slope, and the engine arguments it was learned under -- and its
// accelerator memo; off, as before, it forgets nothing here.
const SwitchITLForgetReachesTheLine = "slo-itl-forget-reaches-the-line"

// forgetVariant forgets one variant's learned state in this analyzer: each
// learner on learn's list that it keeps, by name (forgetLearnerLocked), and
// the variant's accelerator memo. It returns how many entries it dropped and
// the keys it forgot, by the operator, sorted. With
// SwitchITLForgetReachesTheLine off it forgets nothing.
func (a *Analyzer) forgetVariant(namespace, modelID, variant string) (int, []learn.Forgotten) {
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
	if _, ok := a.accelerators[namespace+"/"+variant]; ok {
		delete(a.accelerators, namespace+"/"+variant)
		n++
	}
	key := ttftWindowKey(namespace, modelID, variant)
	if _, ok := a.imbalance[key]; ok {
		delete(a.imbalance, key)
		n++
	}
	if _, ok := a.decodeTTFT[key]; ok {
		delete(a.decodeTTFT, key)
		n++
	}
	if _, ok := a.decodeTerms[key]; ok {
		delete(a.decodeTerms, key)
		n++
	}
	if _, ok := a.prompts[key]; ok {
		delete(a.prompts, key)
		n++
	}
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
func (a *Analyzer) forgetLearnerLocked(name, namespace, modelID, variant string) (dropped int, forgotten []learn.Forgotten, handled bool) {
	switch name {
	case learn.ITL:
		dropped, forgotten = a.forgetLinesLocked(namespace, modelID, variant)
		return dropped, forgotten, true
	case learn.K2:
		// k2 is the saturation analyzer's alone: this analyzer reads the
		// capacity store, which that analyzer's forget clears.
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
	ctrl.Log.WithName("slo").Error(nil, "learner on learn.Learners has no forget decision in the slo analyzer; nothing forgotten",
		"learner", name, "namespace", namespace, "modelID", modelID, "variant", variant)
	return 0, nil, false
}

// forgetLinesLocked forgets the variant's ITL line under every key: what the
// tracker holds of it (windows and intercepts are counted, as EvictStale
// counts them), and the engine arguments it was learned under
// (linesUnderArgs), so the next fit starts a new line rather than reading a
// change of arguments. The key's last-seen stamp goes too: the next Analyze
// sets it again. It returns the count and each key the tracker held, sorted,
// as forgotten by the operator. Callers hold a.mu; the tracker's lock is
// taken under it, as EvictStale takes it.
func (a *Analyzer) forgetLinesLocked(namespace, modelID, variant string) (int, []learn.Forgotten) {
	// The key shape of windowKey. The trailing separator keeps "v" from
	// matching "v2".
	prefix := namespace + "|" + modelID + "|" + variant + "|"
	match := func(k string) bool { return strings.HasPrefix(k, prefix) }
	windows, baselines, forgotten := a.lines.ForgetKeys(a.now(), learn.ByOperator, match)
	for key := range a.args {
		if match(key) {
			delete(a.args, key)
		}
	}
	for key := range a.seen {
		if match(key) {
			delete(a.seen, key)
		}
	}
	for key := range a.lineLogged {
		if match(key) {
			delete(a.lineLogged, key)
		}
	}
	return windows + baselines, forgotten
}
