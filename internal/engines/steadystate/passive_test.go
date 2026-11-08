package steadystate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/passive"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
)

// markPassive makes the wiring fixture's variant passive (hold) for the
// engine's next apply passes, as observePassive would.
func markPassive(e *Engine) {
	e.cyclePassive = map[string]bool{utils.GetNamespacedKey(wiringNS, "Qwen/Qwen3-8B"): true}
	e.cyclePassiveVariants = map[string]passive.Mode{utils.GetNamespacedKey(wiringNS, wiringTarget): passive.Hold}
}

// TestPassiveSkipsTheStickyHold: the sticky scale-down hold judges a fresh
// target against the published one, as though KEDA were descending to it.
// Nothing acts on a passive variant's published value, so the would-be count
// it reports is the fresh target, not one pinned to an earlier figure.
func TestPassiveSkipsTheStickyHold(t *testing.T) {
	e := wiringEngine(t, wiringDeployment())
	markPassive(e)
	require.EqualValues(t, 1, cycle(t, e, "fleet-1", wiringDecision(2, 1, 18770)))
	// Active, the hold would keep 1 here (TestStickyWiring_HoldThenCarryThenExternalPark).
	require.EqualValues(t, 2, cycle(t, e, "fleet-1", wiringDecision(2, 2, 20676)))
}

// TestPassiveSkipsTheCarry: a no-decision cycle reports the running count for
// a passive variant, not a held would-be scale-down nothing is acting on.
func TestPassiveSkipsTheCarry(t *testing.T) {
	e := wiringEngine(t, wiringDeployment())
	markPassive(e)
	require.EqualValues(t, 1, cycle(t, e, "fleet-1", wiringDecision(2, 1, 18770)))
	require.EqualValues(t, 2, cycle(t, e, "fleet-1"))
}

// TestPassiveRecordsARecommendationNotAScale: a passive variant's event says
// what the controller would do. ScaledUp, ScaledDown and ScaledToZero claim
// a scale, and nothing scaled.
func TestPassiveRecordsARecommendationNotAScale(t *testing.T) {
	for _, tc := range []struct {
		name    string
		passive bool
		want    string
	}{
		{name: "passive", passive: true, want: constants.K8SEventPassiveRecommendation},
		{name: "active (control)", passive: false, want: constants.K8SEventScaledDown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := wiringEngine(t, wiringDeployment())
			rec := record.NewFakeRecorder(10)
			e.Recorder = rec
			if tc.passive {
				markPassive(e)
			}
			cycle(t, e, "fleet-1", wiringDecision(2, 1, 18770))
			var events []string
			for len(rec.Events) > 0 {
				events = append(events, <-rec.Events)
			}
			require.Len(t, events, 1, "events: %v", events)
			require.Contains(t, events[0], " "+tc.want+" ")
			for _, claim := range []string{constants.K8SEventScaledUp, constants.K8SEventScaledDown, constants.K8SEventScaledToZero} {
				if tc.passive && strings.Contains(events[0], " "+claim+" ") {
					t.Errorf("a passive variant got %s: %s", claim, events[0])
				}
			}
		})
	}
}

// TestPassiveHoldsNoNodeHole: a passive model's would-be growth is never
// ordered, so it holds no hole in the in-flight ledger that a model which can
// grow would then be kept out of. The active model beside it is the control.
func TestPassiveHoldsNoNodeHole(t *testing.T) {
	allocation.DefaultInFlight.Reset()
	t.Cleanup(allocation.DefaultInFlight.Reset)
	now := time.Now()
	reqs := []allocation.ModelScalingRequest{growingA100("observed", "m", 8), growingA100("scaled", "m", 8)}
	grow := []domain.VariantDecision{
		{VariantName: "m-v", Namespace: "observed", AcceleratorName: "A100", TargetReplicas: 5},
		{VariantName: "m-v", Namespace: "scaled", AcceleratorName: "A100", TargetReplicas: 5},
	}
	e := &Engine{cyclePassive: map[string]bool{"observed/m": true}}
	e.admitInFlight(reqs, grow, nil, nil, now)
	if allocation.DefaultInFlight.Holds("observed/m-v", now) {
		t.Fatal("a passive model's would-be growth holds a node hole")
	}
	if !allocation.DefaultInFlight.Holds("scaled/m-v", now) {
		t.Fatal("control: an active model's growth holds nothing")
	}
}

// TestPassiveShareGroupWritesNothing:a share group holding a passive model
// plans in shadow -- no target, no pod mark, no history write -- under an
// optimizer configured to act. The fleet and the cycles are the ones
// TestUtilizationShareActuatesScaleDownFirst actuates, with the donor A
// passive.
func TestPassiveShareGroupWritesNothing(t *testing.T) {
	f := newShareFleet()
	writes := 0
	c := interceptor.NewClient(sharePods(t, f).(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			writes++
			return cl.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			writes++
			return cl.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
			writes++
			return cl.Patch(ctx, obj, p, opts...)
		},
	})
	clock := time.Unix(0, 0)
	e := &Engine{Config: shadowConfig(t, activeShare), client: c}
	e.utilizationShare.now = func() time.Time { return clock }
	e.cyclePassive = map[string]bool{"ns/A": true}
	ctx, _ := observe(t)
	for range 20 {
		clock = clock.Add(30 * time.Second)
		if o := e.evaluateUtilizationShare(ctx, f.requests(), fullQuota(), f.scaleTargets()); len(o) != 0 {
			t.Fatalf("a group with a passive model returned targets: %+v", o)
		}
	}
	if n := len(markedPods(t, c)); n != 0 {
		t.Fatalf("a group with a passive model marked %d pods", n)
	}
	if writes != 0 {
		t.Fatalf("a group with a passive model wrote %d objects", writes)
	}
}
