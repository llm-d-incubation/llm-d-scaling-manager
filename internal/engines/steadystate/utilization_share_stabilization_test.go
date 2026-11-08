package steadystate

import (
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
)

// The policy's stabilization settings are the ones the planner and the blocked reasons
// apply.
func TestShareSkipWaitsComesFromThePolicy(t *testing.T) {
	off := false
	us, err := config.ResolveUtilizationShare(&config.UtilizationShareConfig{
		Stabilization: &config.UtilizationShareStabilization{
			SkipWaitsWhen: &config.UtilizationShareSkipWaits{Enabled: &off, ReceiverLoadAtLeast: 1.2, DonorLoadAtMost: 0.4},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := allocation.ShareSkipWaits{Off: true, ReceiverLoad: 1.2, DonorLoad: 0.4}
	if got := shareSkipWaits(us); got != want {
		t.Fatalf("shareSkipWaits = %+v, want %+v", got, want)
	}
}

// waitBeforeReverseMove and flappingWindow replace the derived timings, and
// the published source says the policy set them; "auto" leaves them derived.
// swingReversals reaches the ledger as set, and as its default when not.
func TestShareStabilizationTimingsComeFromThePolicy(t *testing.T) {
	derived := allocation.ShareTimings{ReversalHold: 12 * time.Minute, SwingWindow: 2 * time.Hour,
		SwingReversals: config.DefaultSwingReversals}
	source := func(series []metrics.UtilizationShareTiming, param string) string {
		for _, s := range series {
			if s.Param == param {
				return s.Source
			}
		}
		return ""
	}

	us, err := config.ResolveUtilizationShare(&config.UtilizationShareConfig{
		Stabilization: &config.UtilizationShareStabilization{WaitBeforeReverseMove: "5m", FlappingWindow: "0s", SwingReversals: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	tm, src := derived, allocation.ShareTimingSource{}
	shareStabilizationTimings(&tm, src, us.Stabilization)
	if tm.ReversalHold != 5*time.Minute || tm.SwingWindow != 0 || tm.SwingReversals != 2 {
		t.Fatalf("timings %+v, want the policy's 5m, 0s and 2 reversals", tm)
	}
	series := shareTimingSeries(tm, src)
	for _, p := range []string{constants.UtilizationShareParamReversalHold, constants.UtilizationShareParamSwingWindow} {
		if got := source(series, p); got != "policy" {
			t.Errorf("%s source %q, want policy", p, got)
		}
	}

	us, err = config.ResolveUtilizationShare(&config.UtilizationShareConfig{
		Stabilization: &config.UtilizationShareStabilization{WaitBeforeReverseMove: "auto"},
	})
	if err != nil {
		t.Fatal(err)
	}
	tm, src = derived, allocation.ShareTimingSource{}
	shareStabilizationTimings(&tm, src, us.Stabilization)
	if tm != derived {
		t.Fatalf("auto changed the derived timings: %+v", tm)
	}
	if got := source(shareTimingSeries(tm, src), constants.UtilizationShareParamReversalHold); got == "policy" {
		t.Fatal("auto reported as policy")
	}
}

// A group whose slowest start is a ScaledObject seed publishes its swing
// window as read from the ScaledObject, not measured (learner PR 5c): the
// seed is the only input, and a measured start would say measured.
func TestShareTimingSeriesNamesASeededStart(t *testing.T) {
	swing := func(seeded bool) string {
		_, src := allocation.DeriveShareTimings(allocation.ShareTimingInputs{
			Cycle: 30 * time.Second, ReplicaStart: time.Minute, ReplicaStartSeeded: seeded,
			ScaleDownWindow: nil}, nil)
		for _, s := range shareTimingSeries(allocation.ShareTimings{}, src) {
			if s.Param == constants.UtilizationShareParamSwingWindow {
				return s.Source
			}
		}
		return ""
	}
	if got := swing(true); got != "scaledobject" {
		t.Errorf("seeded start: swing window source %q, want scaledobject", got)
	}
	if got := swing(false); got != "measured" {
		t.Errorf("measured start: swing window source %q, want measured", got)
	}
}
