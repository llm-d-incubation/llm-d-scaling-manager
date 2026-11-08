package slo

import (
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// SwitchITLOneSidedAtCap is the replay switch for the one-sided price at the
// sequence cap on the slo line (issue #467; analyzer-learner.md, 4b-3 round
// 1, which built it for the throughput line). A replica at its stated cap
// teaches the line from above the band (SwitchBatchCapAdmission), at a k
// above itl.DefaultKSat. While the key has no learned B the pinned fit pins
// itl.DefaultBaselineSec, and through readings at a k_cap above kSat it
// prices, at a fill k, A'k + B' + (B0 - B')(1 - k/k_cap): under the truth
// whenever the true B' is above the 6 ms default, so the TPOT target is met
// at a higher fill than the replica reaches, and too few replicas are
// ordered. On, while B is unlearned and the window holds readings at k >=
// kSat, the pinned fit is set against the line through the mean of those at
// or past the fill priced, with its intercept an upper prior on B
// (itl.Admission.BPrior: bPrior, capped at the window's least ITL; the
// window's least ITL is a proven bound on B), each priced at the fill where
// it meets the TPOT target (itl.Admission.OneSidedTarget), and the one with
// the lower fill is the line. It still abstains (infeasible) where even that
// line meets the target nowhere in the band. A held slope keeps the A whose
// line meets the target at that fill. A learned B ends it. Off, as before,
// the fit through the default B is the line.
const SwitchITLOneSidedAtCap = "slo-itl-one-sided-at-cap"

// oneSided sets on adm the one-sided price at the cap, under
// SwitchITLOneSidedAtCap, at the TPOT target in seconds (0 with none, where
// the line is fitted and not priced, and the rule judges at
// itl.DefaultKSat), pinned at the prior on B of the variant's physics
// (bPrior).
func (a *Analyzer) oneSided(adm *itl.Admission, tpotTarget float64, scope itl.PriorScope) {
	a.mu.Lock()
	on := a.itlOneSidedAtCap
	a.mu.Unlock()
	if on {
		adm.OneSidedAboveKSat, adm.OneSidedTarget, adm.BPrior = true, tpotTarget, a.bPrior(scope)
	}
}

// bPrior is the prior on B the one-sided rule pins (itl.BPrior): the floor
// itl.DefaultBPrior, raised by the largest B learned on the line's tracker
// within the variant's physics (itl.PriorScope), never lowered by it.
func (a *Analyzer) bPrior(scope itl.PriorScope) float64 {
	return itl.BPrior(a.lines.ScopedBaselines(scope)...)
}

// priorScope is the scope of the variant's prior on B (itl.PriorScope): its
// window key, the engine arguments it runs, the arguments each key was last
// fitted under (args), and the role of each of the model's variants this
// cycle.
func (a *Analyzer) priorScope(key, args string, states []domain.VariantReplicaState) itl.PriorScope {
	roles := make(map[string]string, len(states))
	for _, vs := range states {
		roles[vs.VariantName] = domain.CanonicalRole(vs.Role)
	}
	return itl.NewPriorScope(key, args,
		func(k string) string {
			a.mu.Lock()
			defer a.mu.Unlock()
			return a.args[k]
		},
		func(v string) string { return roles[v] })
}
