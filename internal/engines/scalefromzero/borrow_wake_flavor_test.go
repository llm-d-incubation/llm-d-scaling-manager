package scalefromzero

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
)

// flavorQueueProvider is namespace "a" on a queue of nominal 16 whose one GPU
// flavor pins node label pool=a. counted false reads "a" without counting its
// borrowing (its figures come as NamespaceNominal); a peer is counted, so the
// cohort's chunks are read either way.
type flavorQueueProvider struct{ counted bool }

const flavorQueueName = "flavor-quota"

func (flavorQueueProvider) Name() string { return flavorQueueName }
func (p flavorQueueProvider) ComputeConstraints(context.Context, map[string]int, map[string]map[string]int) (*allocation.ResourceConstraints, error) {
	c := &allocation.ResourceConstraints{
		ProviderName:     flavorQueueName,
		NamespaceScoped:  true,
		NamespaceCohorts: map[string][]string{"a": {"c"}, "peer": {"c"}},
		NamespaceBorrow: map[string]map[string]allocation.NamespaceBorrow{
			"peer": {wakeHBM3: {Cap: config.QuotaUnlimited}}},
		NamespacePools: map[string]map[string]allocation.ResourcePool{
			"a": {wakeHBM3: {Limit: 16}}, "peer": {wakeHBM3: {Limit: 0}}},
		SharedPools: []allocation.SharedPoolChunk{{Cohort: "c", Flavor: "pool-a", Resource: "nvidia.com/gpu",
			FlavorPlacement: kueue.FlavorPlacement{AcceleratorType: wakeHBM3, NodeLabels: map[string]string{"pool": "a"}}}},
	}
	if p.counted {
		c.NamespaceBorrow["a"] = map[string]allocation.NamespaceBorrow{wakeHBM3: {Cap: 0, WithinNominal: 16}}
	} else {
		c.NamespaceNominal = map[string]map[string]allocation.NominalFigures{"a": {wakeHBM3: {WithinNominal: 16, Guaranteed: 16}}}
	}
	return c, nil
}

// Review W5, the wake: Kueue admits every pod of a managed namespace on its
// queue's flavor, within nominal too. The steady placer fits a guaranteed
// replica there; the wake fitted by the templates alone, so a free node of
// another pool counted as a hole the woken pod can never land on, and the
// wake created a pod Kueue's flavor could not place. One 8-GPU node per pool:
// pool b's free, pool a's (the flavor's) full.
func TestAWakeFitsItsQueueFlavor(t *testing.T) {
	publishUsage(t)
	for _, counted := range []bool{true, false} {
		for _, tc := range []struct {
			name      string
			poolAUsed int
			wake      bool
		}{
			{"only another pool's node is free: no capacity", 8, false},
			{"control: the flavor's node is free: the decode wakes there", 0, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				allocation.DefaultInFlight.Reset()
				decision.DefaultNodeGPUs.Publish(map[string]decision.NodeGPU{
					"na": {Accelerator: wakeHBM3, Capacity: 8, Used: tc.poolAUsed, Labels: map[string]string{"pool": "a"}},
					"nb": {Accelerator: wakeHBM3, Capacity: 8, Labels: map[string]string{"pool": "b"}},
				}, time.Now())
				t.Cleanup(func() {
					decision.DefaultNodeGPUs.Publish(nil, time.Time{})
					allocation.DefaultInFlight.Reset()
				})
				e := &Engine{gpuLimiter: allocation.NewCompositeLimiter("composite", []allocation.Limiter{
					okProvider{name: allocation.PhysicalLimiterName, pools: map[string]allocation.ResourcePool{wakeHBM3: {Limit: 16 - tc.poolAUsed}}},
					flavorQueueProvider{counted: counted},
				})}
				e.beginTick()
				got := wakeOnce(t, e, "a", "m", cand("d", "decode", wakeHBM3, 8, 1))
				if woke := got != OutcomeNoCapacity; woke != tc.wake {
					t.Fatalf("counted %t: wake outcome %s, want a wake %t: Kueue admits the pod on pool a only", counted, got, tc.wake)
				}
			})
		}
	}
}
