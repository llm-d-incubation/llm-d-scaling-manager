package steadystate

import (
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// podBusyWithheld runs TestUtilizationShareReturnsSpare's fleet -- A holds 9
// against a target of 4 and returns its spare once its window has passed --
// with busy as A's variant's busiest queue in every analyzer result, and
// returns how often a return was withheld as return-pod-busy.
func podBusyWithheld(t *testing.T, busy float64) float64 {
	t.Helper()
	reg := freshMetrics(t)
	f := newShareFleet()
	f.demand = map[string]float64{"A": 1600, "B": 3000, "C": 1000}
	se := newShareEngine(t, f, sharePods(t, f), time.Unix(0, 0))
	for range 12 {
		for _, l := range se.e.utilizationShare.ledgers {
			l.RecordNeeds(map[string]float64{"ns/B/" + domain.RoleBoth: 13, "ns/C/" + domain.RoleBoth: 13}, se.clock,
				allocation.ShareTimings{SwingWindow: time.Hour})
		}
		reqs := f.requests()
		for i := range reqs {
			if reqs[i].ModelID == "A" {
				reqs[i].CompositeSignal.Result.VariantBusiestQueueLoad = map[string]float64{"A-v": busy}
			}
		}
		se.clock = se.clock.Add(30 * time.Second)
		se.e.evaluateUtilizationShare(se.ctx, reqs, fullQuota(), f.scaleTargets())
	}
	n := 0.0
	for _, m := range family(t, reg, constants.LLMSMUtilizationShareWithheldTotal) {
		if label(m, constants.LabelReason) == allocation.ShareWithheldReturnPodBusy {
			n += m.GetCounter().GetValue()
		}
	}
	return n
}

// The engine carries each role's busiest queue from the analyzer result into
// the plan: with one of A's pods queued at twice the saturation queue, A's
// return is held as return-pod-busy. Control: unqueued, nothing is.
func TestUtilizationShareCarriesTheBusiestQueueIntoThePlan(t *testing.T) {
	if n := podBusyWithheld(t, 2); n == 0 {
		t.Fatal("no return-pod-busy with one of A's pods queued: the busiest queue never reached the plan")
	}
	if n := podBusyWithheld(t, 0); n != 0 {
		t.Fatalf("control: %v return-pod-busy with no pod queued", n)
	}
}
