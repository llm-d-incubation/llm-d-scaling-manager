package saturation

import (
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// SwitchITLOneSidedAtCap is the replay switch for the one-sided price at
// the sequence cap on the ITL line (issue #467; analyzer-learner.md, 4b-3
// round 1, which built it for the throughput line). A replica at its stated
// cap teaches the line from above the band (SwitchBatchCapITLAdmission), at
// a k above itl.DefaultKSat. While the key has no learned B the pinned fit
// pins itl.DefaultBaselineSec, and through readings at a k_cap above kSat it
// prices, at a fill k, A'k + B' + (B0 - B')(1 - k/k_cap): under the truth
// whenever the true B' is above the 6 ms default, so too high a derived mu
// and too few replicas. On, while B is unlearned and the window holds
// readings at k >= kSat, the pinned fit is set against the line through the
// mean of those at or past the pricing k with its intercept an upper prior
// on B (itl.Admission.BPrior: itlBPrior, capped at the window's least ITL),
// compared at the k the line is priced at (pricingK;
// itl.Admission.OneSidedAt), and the higher there is the line. A held slope
// keeps the A that prices the chosen line there. A learned B ends it. Off,
// as before, the fit through the default B is the line.
const SwitchITLOneSidedAtCap = "saturation-itl-one-sided-at-cap"

// itlOneSided sets on adm the one-sided price at the cap, under
// SwitchITLOneSidedAtCap, at kPrice, the fill the line is priced at, pinned
// at the prior on B of the variant's physics (itlBPrior). A variant with no
// stated cap admits nothing at it, and the rule has nothing past kSat to
// read but what the window took at the cap: it is set all the same, and does
// nothing there.
func (a *SaturationAnalyzer) itlOneSided(adm *itl.Admission, kPrice float64, scope itl.PriorScope, physicsKey string) {
	a.mu.Lock()
	on := a.itlOneSidedAtCap
	a.mu.Unlock()
	if !on {
		return
	}
	adm.OneSidedAboveKSat, adm.OneSidedAt, adm.BPrior = true, kPrice, a.itlBPrior(scope, physicsKey)
	a.mu.Lock()
	a.itlOneSidedLast[scope.Key()] = itlOneSidedRecord{adm: itl.Admission{OneSidedAboveKSat: true,
		OneSidedAt: kPrice, BPrior: adm.BPrior}}
	a.mu.Unlock()
}

// itlOneSidedRecord is the one-sided admission a key was last priced under
// (itlOneSided), and whether that cycle's line was priced at the prior
// (itl.Reading.PinnedAtBPrior; noteITLOneSided), for the what-if refit.
type itlOneSidedRecord struct {
	adm    itl.Admission
	raised bool
}

// noteITLOneSided records whether the key's line was priced at the prior
// this cycle, beside the admission itlOneSided recorded for it.
func (a *SaturationAnalyzer) noteITLOneSided(key string, r itl.Reading) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if rec, ok := a.itlOneSidedLast[key]; ok {
		rec.raised = r.PinnedAtBPrior
		a.itlOneSidedLast[key] = rec
	}
}

// itlBPrior is the prior on B the one-sided rule pins (itl.BPrior): the
// floor itl.DefaultBPrior, raised by the largest B learned on the variant's
// own physics, never lowered by it. Two sources are read, and only within
// that physics. One is the B of the tracker's keys in scope (itl.PriorScope:
// the same model, accelerator and GPU count, under the same engine
// arguments, in the same role). The other is the line published for the
// variant's own physics key (itlPhysicsKey: the model, the accelerator and
// the engine fingerprint). One published through the default B (every key
// without a learned one publishes it) is under the floor and moves nothing.
// Called without a.mu held.
func (a *SaturationAnalyzer) itlBPrior(scope itl.PriorScope, physicsKey string) float64 {
	learned := a.itlTracker.ScopedBaselines(scope)
	a.mu.Lock()
	if l, ok := a.itlLines[physicsKey]; ok {
		learned = append(learned, l.model.B)
	}
	a.mu.Unlock()
	return itl.BPrior(learned...)
}

// itlPriorScope is the scope of the variant's prior on B (itl.NewPriorScope):
// its window key, the engine arguments it runs, the arguments each key was
// last fitted under (itlArgs, read under a.mu, only for a key that matches
// the variant's physics), and the role of each of the model's variants this
// cycle. The tracker's baselines are copied before the scope reads them
// (Tracker.ScopedBaselines), so a.mu is never taken under its lock.
func (a *SaturationAnalyzer) itlPriorScope(key, args string, roles map[string]string) itl.PriorScope {
	return itl.NewPriorScope(key, args,
		func(k string) string {
			a.mu.Lock()
			defer a.mu.Unlock()
			return a.itlArgs[k]
		},
		func(v string) string {
			r, ok := roles[v]
			if !ok {
				return ""
			}
			return domain.CanonicalRole(r)
		})
}
