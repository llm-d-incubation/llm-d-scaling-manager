package allocation

import (
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
)

// The placement timeout in a Kueue-backed namespace that counts no
// borrowing (gatedRelease), in 8-GPU units: serving holds 16 against a Kueue
// figure of 8, one of its two replicas at Kueue's admission gate since t0.
// Only a replica Kueue has not admitted is the queue's to give back, and only
// past the figure; an entry that counts borrowing gives back only where its
// queue is in no cohort.
func TestGatedReleaseOutsideBorrowing(t *testing.T) {
	t0 := time.Unix(50_000, 0)
	timeout := DefaultBorrowSettings().PlacementTimeout
	two := 2
	for _, tc := range []struct {
		name        string
		figure      int
		kueueBacked bool
		counting    bool
		withheld    kueue.Reason
		gated       bool
		minReplicas *int
		want        int
	}{
		{"gated past the figure: released", 8, true, false, "", true, nil, 1},
		{"Unschedulable, admitted by Kueue: not the queue's to give back", 8, true, false, "", false, nil, 2},
		{"gated within the figure: Kueue admits it", 16, true, false, "", true, nil, 2},
		{"a static quota, no Kueue: nothing gates on it", 8, false, false, "", true, nil, 2},
		{"at minReplicas: kept", 8, true, false, "", true, &two, 2},
		{"an entry counting borrowing, queue in no cohort: released", 8, true, true, kueue.ReasonNoCohort, true, nil, 1},
		{"an entry counting borrowing, namespace withheld: kept", 8, true, true, kueue.ReasonStale, true, nil, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &ResourceConstraints{ProviderName: "kq", NamespaceScoped: true, KueueBacked: tc.kueueBacked,
				KueueCountsBorrowing: tc.counting,
				NamespacePools:       map[string]map[string]ResourcePool{"serving": {"H100": {Limit: tc.figure, Used: 16}}}}
			if tc.withheld != "" {
				c.BorrowWithheld = map[string]string{"serving": string(tc.withheld)}
			}
			st := domain.VariantReplicaState{VariantName: "v", CurrentReplicas: 2, GPUsPerReplica: 8, AcceleratorName: "H100",
				MinReplicas: tc.minReplicas, UnplacedReplicas: 1, UnplacedSince: t0, UnplacedPods: []string{"v-1"},
				UnplacedAt: map[string]time.Time{"v-1": t0}}
			if tc.gated {
				st.GatedAt = map[string]time.Time{"v-1": t0}
			}
			reqs := []ModelScalingRequest{{ModelID: "m", Namespace: "serving", VariantStates: []domain.VariantReplicaState{st}}}
			decisions := func() []domain.VariantDecision {
				return []domain.VariantDecision{{Namespace: "serving", ModelID: "m", VariantName: "v", AcceleratorName: "H100",
					CurrentReplicas: 2, TargetReplicas: 2, GPUsPerReplica: 8}}
			}
			// Both the borrowing cycle (Observe, Releases) and the one that
			// counts none (HoldReleases, GatedReleases) release alike.
			for _, borrowing := range []bool{true, false} {
				g := NewBorrowGovernor(DefaultBorrowSettings())
				cycle := func(at time.Time) []domain.VariantDecision {
					c := BorrowCycle{Now: at, Constraints: []*ResourceConstraints{c}, Requests: reqs}
					ds := decisions()
					var rel []BorrowRelease
					if borrowing {
						g.Observe(c)
						rel = g.Releases(c, ds)
					} else {
						g.HoldReleases(at, reqs, ds)
						rel = g.GatedReleases(c, ds)
					}
					for _, r := range rel {
						if !r.AtGate || r.Cause != ReleaseKueueGate {
							t.Errorf("release %+v, want AtGate with cause %s", r, ReleaseKueueGate)
						}
					}
					return ds
				}
				if ds := cycle(t0); ds[0].TargetReplicas != 2 {
					t.Fatalf("borrowing cycle %v: released at once to %d, before the placement timeout", borrowing, ds[0].TargetReplicas)
				}
				if ds := cycle(t0.Add(timeout)); ds[0].TargetReplicas != tc.want {
					t.Fatalf("borrowing cycle %v: target %d after the placement timeout, want %d", borrowing, ds[0].TargetReplicas, tc.want)
				}
				if g.CoolingDown("serving", "m", t0.Add(timeout)) {
					t.Fatalf("borrowing cycle %v: a release at Kueue's gate started a borrowing cooldown", borrowing)
				}
			}
		})
	}
}

// A namespace counted for borrowing is not also governed as an uncounted one:
// its figure is NamespaceBorrow's guaranteed one, and the counted rules
// (borrowedRelease) alone apply to it.
func TestUncountedHeldPastSkipsCountedNamespaces(t *testing.T) {
	counted := &ResourceConstraints{ProviderName: "kq", KueueBacked: true,
		NamespacePools:  map[string]map[string]ResourcePool{"b": {"H100": {Limit: 0, Used: 16}}, "u": {"H100": {Limit: 8, Used: 16}}},
		NamespaceBorrow: map[string]map[string]NamespaceBorrow{"b": {"H100": {Cap: -1}}}}
	got := uncountedHeldPast([]*ResourceConstraints{counted})
	if _, ok := got["b"]; ok || got["u"]["H100"] != 8 {
		t.Fatalf("uncountedHeldPast = %v, want only u's 8", got)
	}
}

// A pin of a release at Kueue's gate is no borrowing state: it outlasts
// HoldReleases' idle window while its replica has not gone, so the demand
// floor does not raise the target again.
func TestGatedPinOutlastsTheIdleWindow(t *testing.T) {
	t0 := time.Unix(50_000, 0)
	s := DefaultBorrowSettings()
	c := &ResourceConstraints{ProviderName: "kq", NamespaceScoped: true, KueueBacked: true,
		NamespacePools: map[string]map[string]ResourcePool{"serving": {"H100": {Limit: 8, Used: 16}}}}
	st := domain.VariantReplicaState{VariantName: "v", CurrentReplicas: 2, GPUsPerReplica: 8, AcceleratorName: "H100",
		UnplacedReplicas: 1, UnplacedSince: t0, UnplacedPods: []string{"v-1"},
		UnplacedAt: map[string]time.Time{"v-1": t0}, GatedAt: map[string]time.Time{"v-1": t0}}
	reqs := []ModelScalingRequest{{ModelID: "m", Namespace: "serving", VariantStates: []domain.VariantReplicaState{st}}}
	// Late, the release has not landed: two pods still hold GPUs, neither at
	// the gate any more (the scale-down is in flight).
	landing := []ModelScalingRequest{{ModelID: "m", Namespace: "serving", VariantStates: []domain.VariantReplicaState{{
		VariantName: "v", CurrentReplicas: 2, GPUsPerReplica: 8, AcceleratorName: "H100"}}}}
	g := NewBorrowGovernor(s)
	target := func(at time.Time, reqs []ModelScalingRequest) int {
		ds := []domain.VariantDecision{{Namespace: "serving", ModelID: "m", VariantName: "v", AcceleratorName: "H100",
			CurrentReplicas: 2, TargetReplicas: 3, GPUsPerReplica: 8}}
		g.HoldReleases(at, reqs, ds)
		g.GatedReleases(BorrowCycle{Now: at, Constraints: []*ResourceConstraints{c}, Requests: reqs}, ds)
		return ds[0].TargetReplicas
	}
	target(t0, reqs)
	if got := target(t0.Add(s.PlacementTimeout), reqs); got != 1 {
		t.Fatalf("target %d after the placement timeout, want 1", got)
	}
	late := t0.Add(s.PlacementTimeout + s.Cooldown + s.ReleaseActuation + time.Minute)
	if got := target(late, landing); got != 1 {
		t.Fatalf("target %d past the idle window with the replica not gone, want 1: the pin holds", got)
	}
}
