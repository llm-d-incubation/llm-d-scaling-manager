package slo

import "github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"

// SwitchITLRolloutGuard is the replay switch for the line's rollout guard
// (issue #481, itl.Tracker.RolloutRows). On, while the variant's scale target
// rolls out (capacity.Record.InRollout) or its replicas report more than one
// revision (domain.RevisionsMixed), the line is offered the newest revision's
// rows alone: an old pod's reading would be judged against the new engine's
// sequence cap, and a fit would span two engines. The first rows of a new
// revision clear the window. With no new pod serving it is offered none, and
// holds what it has, under the arguments it was learned under
// (pricingDetail.rolloutHeld): the old pods still serve, so a change of
// arguments waits for a new pod too (linesUnderArgs). Off, as before, every row
// is offered.
const SwitchITLRolloutGuard = "slo-itl-rollout-guard"

// lineRows is what the line under key is offered this cycle, and whether it is
// held on the old pods.
func (a *Analyzer) lineRows(input domain.AnalyzerInput, fleet fleetReading, key, variant string) ([]domain.ReplicaMetrics, bool) {
	if !a.itlRolloutGuard {
		return input.ReplicaMetrics, false
	}
	rolling := fleet.mixed[variant] ||
		(a.store != nil && a.store.Get(input.Namespace, input.ModelID, variant).InRollout())
	sight := a.lines.RolloutRows(key, variant, input.ReplicaMetrics, rolling)
	return sight.Rows, sight.Held
}

// heldArgs is the arguments a held line stands under: those it was learned
// under, or args when none are known.
func (a *Analyzer) heldArgs(key, args string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if prev := a.args[key]; prev != "" {
		return prev
	}
	return args
}
