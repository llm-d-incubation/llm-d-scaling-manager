package steadystate

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zaptest/observer"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

const (
	capUnknownMessage = "Utilization share: the models' peaks are not known yet"
	capExceedMessage  = "Utilization share: the models' recent peaks do not fit the quota together"
	capFitMessage     = "Utilization share: the models' recent peaks fit the quota together again"
)

// capLogs counts the logs whose message starts with msg.
func capLogs(logs *observer.ObservedLogs, msg string) []observer.LoggedEntry {
	return logs.FilterMessageSnippet(msg).All()
}

// calmShareEngine is the section 5.7 fleet at a demand whose peaks fit its
// quota (A 2, B 3.75, C 1.25 of 16), with its logs observed.
func calmShareEngine(t *testing.T) (*shareEngine, *observer.ObservedLogs) {
	t.Helper()
	f := newShareFleet()
	f.demand = map[string]float64{"A": 1600, "B": 3000, "C": 1000}
	se := newShareEngine(t, f, sharePods(t, f), time.Unix(0, 0))
	var logs *observer.ObservedLogs
	se.ctx, logs = observe(t)
	return se, logs
}

// The cap's flip log names its cause. A new group has no need history, so
// every model counts at its maxReplicas and the cap is on: said once, as
// peaks-unknown, naming the models -- not as peaks that exceed the quota,
// which would send an operator to a quota that is not short. Once a window of
// history shows the peaks fit, the cap comes off: said once too.
func TestUtilizationShareCapLogNamesItsCause(t *testing.T) {
	freshMetrics(t)
	se, logs := calmShareEngine(t)
	for range 10 {
		se.cycle()
	}
	unknown := capLogs(logs, capUnknownMessage)
	if len(unknown) != 1 {
		t.Fatalf("%d peaks-unknown logs over the first planned cycles, want 1", len(unknown))
	}
	ctx := unknown[0].ContextMap()
	if ctx["cause"] != "peaks-unknown" {
		t.Fatalf("cause %v, want peaks-unknown", ctx["cause"])
	}
	for _, m := range []string{"A", "B", "C"} {
		if !strings.Contains(fmt.Sprint(ctx["unknownPeaks"]), "ns/"+m+"/"+domain.RoleBoth) {
			t.Fatalf("unknownPeaks %v does not name %s", ctx["unknownPeaks"], m)
		}
	}
	if ctx["scope"] != constants.UtilizationShareClusterScope {
		t.Fatalf("scope %q, want %q: the scope as the metrics label it", ctx["scope"], constants.UtilizationShareClusterScope)
	}
	// Every 30 s for 5 h: the history never ages out.
	for range 600 {
		se.cycle()
	}
	if n := len(capLogs(logs, capUnknownMessage)); n != 1 {
		t.Fatalf("%d peaks-unknown logs, want still 1: said when the cap flips, not every cycle", n)
	}
	if n := len(capLogs(logs, capFitMessage)); n != 1 {
		t.Fatalf("%d logs that the peaks fit again, want 1", n)
	}
	if n := len(capLogs(logs, capExceedMessage)); n != 0 {
		t.Fatalf("%d logs that the peaks exceed the quota, which they never did", n)
	}
}

// llmsm_utilization_share_headroom_capped is published only for a cycle the
// planner planned. In the restart quiet period the published targets are not
// the planner's, and a 0 there would read as "uncapped" for a group whose cap
// is on as soon as it plans -- its models' peaks are not known yet.
func TestUtilizationShareCappedGaugeWaitsForAPlan(t *testing.T) {
	reg := freshMetrics(t)
	se, _ := calmShareEngine(t)
	se.cycle()
	if n := len(family(t, reg, constants.LLMSMUtilizationShareHeadroomCapped)); n != 0 {
		t.Fatalf("%d headroom_capped series in the quiet period, want none", n)
	}
	planned := 0
	for i := range 20 {
		se.cycle()
		if len(family(t, reg, constants.LLMSMUtilizationShareHeadroomCapped)) > 0 {
			planned = i + 2
			break
		}
	}
	if planned == 0 {
		t.Fatal("no headroom_capped series once the planner plans")
	}
	if got := cappedGauge(t, reg); got != 1 {
		t.Fatalf("headroom_capped %v on the first plan, want 1: no peak is known yet", got)
	}
	// A restart: the new ledger's quiet period publishes none again.
	restarted := newShareEngine(t, se.f, se.c, se.clock)
	restarted.cycle()
	if n := len(family(t, reg, constants.LLMSMUtilizationShareHeadroomCapped)); n != 0 {
		t.Fatalf("%d headroom_capped series in a restart's quiet period, want none", n)
	}
}

// Switching the optimizer to shadow and back rebuilds the group's ledger, and
// forgets the cap state the old one logged: the new ledger's cap -- on, its
// peaks unknown again -- is logged afresh. Kept, the flip log stayed silent,
// and an operator reading the logs saw a cap with no cause.
func TestUtilizationShareCapLoggedAfreshAfterAToggle(t *testing.T) {
	freshMetrics(t)
	se, logs := calmShareEngine(t)
	for range 10 {
		se.cycle()
	}
	if n := len(capLogs(logs, capUnknownMessage)); n != 1 {
		t.Fatalf("setup: %d peaks-unknown logs, want 1", n)
	}
	setShadowPolicy(t, se.e.Config, selectedShadow)
	se.cycle()
	setShadowPolicy(t, se.e.Config, activeShare)
	for range 10 {
		se.cycle()
	}
	if n := len(capLogs(logs, capUnknownMessage)); n != 2 {
		t.Fatalf("%d peaks-unknown logs after a toggle to shadow and back, want 2", n)
	}
}

// Active, a model's blocked reasons are judged on the planner's evaluation of
// the group, which counts the transfers in flight -- not on the shadow
// evaluation of what each model holds. A gives B replicas; their release is
// past its window, so it can no longer be called off. Then A's demand rises to
// a need of 7.2: A still holds 9, but is committed only what it keeps, below
// that need, and whole replicas cannot cover every model (8 + 8 + 2 of 16).
// So A is whole-replica-short. Judged on what it holds, it is not short at all.
func TestUtilizationShareActiveReasonsUseThePlannersEvaluation(t *testing.T) {
	reg := freshMetrics(t)
	f := newShareFleet()
	c := sharePods(t, f)
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	se.untilStarted()
	start := se.clock
	for se.clock.Sub(start) < 330*time.Second {
		se.cycle()
	}
	if got := reasonModels(t, reg, constants.ScalingBlockedWholeReplicaShort); slices.Contains(got, "A") {
		t.Fatalf("setup: A whole-replica-short before its demand rose: %v", got)
	}
	f.demand["A"] = 5760
	se.cycle()
	// A gives at least 2 GPUs by releases past their window: committed 7 or
	// less, below its need of 7.2, while it holds 9.
	giving := 0
	for _, l := range se.e.utilizationShare.ledgers {
		for _, tr := range l.Transfers() {
			if tr.Donor == roleA && tr.State == allocation.ShareReleasing && se.clock.Sub(tr.Started) > 300*time.Second {
				giving += tr.DonorGPUs
			}
		}
	}
	if giving < 2 {
		t.Fatalf("setup: A gives %d GPUs by releases past their window, want at least 2", giving)
	}
	if got := reasonModels(t, reg, constants.ScalingBlockedWholeReplicaShort); !slices.Contains(got, "A") {
		t.Fatalf("A, committed below its need of 7.2, is not whole-replica-short: %v", got)
	}
}
