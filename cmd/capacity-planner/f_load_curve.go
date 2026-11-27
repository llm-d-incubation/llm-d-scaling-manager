package main

import (
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// --prefill-f-load-curve prices f, the prefill imbalance, against the load
// per pod (issue llm-d-incubation/llm-d-scaling-manager#482): the replay
// switch saturation.SwitchPrefillFLoadCurve, which is not in
// learnreplay.PlannerSwitches, so it is off unless asked. On, P1 fits
// f(u) = f_inf + delta/u at each prefill count (prefill.FLoadCurve), and a
// count whose fit is identified is priced at the higher of its f and the
// curve's at the rate's load (whatif.Prefill.ImbalanceCurveFloor); any other
// count keeps its f, and says so. It passed one pre-registered offline test
// on the lab's own recordings only, so the flag is in flagsOutside: set,
// every figure reads OUTSIDE VALIDATED REGION.

// fLoadCurveFlag is the flag's name, the switch's own.
const fLoadCurveFlag = "prefill-f-load-curve"

// fLoadCurveUsage is the flag's help, on every command that takes it.
const fLoadCurveUsage = "opt-in, validated on lab recordings only: fit f, the prefill imbalance, against the computed tokens a second a pod " +
	"at each prefill count, f(u) = f_inf + delta/u, and price f at a rate as the higher of its f and the curve's at that rate's load, " +
	"so a light load reads the higher f it showed and the knee is where f(u) times the utilization reaches 1 less the margin. " +
	"A count with fewer than 8 minutes, or whose f does not fall with the load, keeps its f. Every figure reads OUTSIDE VALIDATED REGION. Off by default"

// priceFLoadCurve sets f against the load on p, the prefill inputs at
// p.Replicas, from the curve P1 fitted at that count, where it is
// identified; elsewhere it leaves f as it is and says why. One pod has no f
// to price.
func (pf *pricedFleet) priceFLoadCurve(ps *learnreplay.PrefillState, p *whatif.Prefill) {
	if p.Replicas < 2 {
		return
	}
	c, ok := ps.Estimate.FCurveByPods[p.Replicas]
	if !ok || !c.Identified() {
		pf.assume("f against the load not identified on %d prefill pods (%d minutes, %d needed with f falling with the load); f %.4g at every load",
			p.Replicas, c.Minutes, prefill.MinFLoadCurveMinutes, max(p.Imbalance, 1))
		return
	}
	p.ImbalanceCurveFloor, p.ImbalanceCurveExcess = c.Floor, c.Excess
	pf.in.ImbalanceCurveFloor, pf.in.ImbalanceCurveExcess, pf.in.ImbalanceCurveMinutes = c.Floor, c.Excess, c.Minutes
	pf.assume("f at a rate the higher of %.4g and %.4g + %.4g/u, u the computed tokens a second a prefill pod, fitted over %d minutes on %d pods (--%s)",
		max(p.Imbalance, 1), c.Floor, c.Excess, c.Minutes, p.Replicas, fLoadCurveFlag)
}
