package steadystate

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// selectInventory is a minimal allocation.Inventory whose Refresh result and
// resource pools are configurable, so selectV2Optimizer's handling of the
// inventory limiter can be exercised without a real cluster.
type selectInventory struct {
	refreshErr    error
	pools         map[string]allocation.ResourcePool
	refreshCalled bool
}

func (s *selectInventory) Name() string {
	return "select-inventory"
}

func (s *selectInventory) Refresh(context.Context) error {
	s.refreshCalled = true
	return s.refreshErr
}

func (s *selectInventory) SetUsed(map[string]int) {}

func (s *selectInventory) GetResourcePools() map[string]allocation.ResourcePool {
	return s.pools
}

func (s *selectInventory) TotalLimit() int {
	t := 0
	for _, p := range s.pools {
		t += p.Limit
	}
	return t
}
func (s *selectInventory) TotalUsed() int {
	t := 0
	for _, p := range s.pools {
		t += p.Used
	}
	return t
}
func (s *selectInventory) TotalAvailable() int {
	t := 0
	for _, p := range s.pools {
		t += p.Available()
	}
	return t
}

// selectLimiter is a allocation.Limiter that is NOT a ConstraintProvider, used to
// exercise the "limiter cannot be queried" branch.
type selectLimiter struct{}

func (selectLimiter) Name() string { return "select-limiter" }

var _ = Describe("selectV2Optimizer", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
		// GreedyByScore now requires an OBSERVED usage figure: with none, the
		// constraints would be built on an implicit zero and it would read the
		// whole cluster as free. These specs are about the limiter/inventory
		// branches, so they stand an observation up first; the "nothing observed"
		// branch is pinned separately in gpu_usage_publish_test.go.
		decision.PublishGPUUsage(map[string]int{}, map[string]map[string]int{})
	})
	AfterEach(func() { decision.DefaultGPUUsage.Reset() })

	newInventoryLimiter := func(refreshErr error, pools map[string]allocation.ResourcePool) *allocation.DefaultLimiter {
		return allocation.NewDefaultLimiter("gpu-limiter",
			&selectInventory{refreshErr: refreshErr, pools: pools})
	}

	It("falls back to the cost-aware (unlimited) optimizer when inventory refresh fails", func() {
		// Models the cluster-limitation case: node objects cannot be listed, so
		// ComputeConstraints errors. GreedyByScore with empty constraints would
		// silently block all scale-up, so we must run cost-aware instead.
		e := &Engine{
			optimizer:  allocation.NewGreedyByScoreOptimizer(),
			GPULimiter: newInventoryLimiter(errors.New("failed to list nodes"), nil),
		}
		opt, constraints := e.selectV2Optimizer(ctx, nil)
		Expect(opt.Name()).To(Equal("cost-aware"))
		Expect(constraints).To(BeEmpty())
	})

	It("uses greedy-by-score with constraints when the inventory reports GPUs (heterogeneous pools)", func() {
		e := &Engine{
			optimizer: allocation.NewGreedyByScoreOptimizer(),
			GPULimiter: newInventoryLimiter(nil, map[string]allocation.ResourcePool{
				"A100": {Limit: 8},
				"H100": {Limit: 4},
			}),
		}
		opt, constraints := e.selectV2Optimizer(ctx, nil)
		Expect(opt.Name()).To(Equal("greedy-by-score"))
		Expect(constraints).To(HaveLen(1))
		Expect(constraints[0].Pools).To(HaveKey("A100"))
		Expect(constraints[0].Pools).To(HaveKey("H100"))
	})

	It("keeps greedy-by-score when refresh succeeds but reports zero GPUs (genuine no-capacity)", func() {
		// A present-but-empty constraint is a real "no capacity" signal and must
		// still block scale-up — distinct from "capacity unknown".
		e := &Engine{
			optimizer:  allocation.NewGreedyByScoreOptimizer(),
			GPULimiter: newInventoryLimiter(nil, map[string]allocation.ResourcePool{}),
		}
		opt, constraints := e.selectV2Optimizer(ctx, nil)
		Expect(opt.Name()).To(Equal("greedy-by-score"))
		Expect(constraints).To(HaveLen(1))
		Expect(constraints[0].Pools).To(BeEmpty())
	})

	It("falls back to cost-aware when the GPU limiter is not the inventory limiter", func() {
		e := &Engine{
			optimizer:  allocation.NewGreedyByScoreOptimizer(),
			GPULimiter: selectLimiter{},
		}
		opt, constraints := e.selectV2Optimizer(ctx, nil)
		Expect(opt.Name()).To(Equal("cost-aware"))
		Expect(constraints).To(BeEmpty())
	})

	It("leaves a non-greedy optimizer untouched and never queries the limiter", func() {
		inv := &selectInventory{refreshErr: errors.New("must not be called")}
		e := &Engine{
			optimizer:  allocation.NewCostAwareOptimizer(),
			GPULimiter: allocation.NewDefaultLimiter("gpu-limiter", inv),
		}
		opt, constraints := e.selectV2Optimizer(ctx, nil)
		Expect(opt.Name()).To(Equal("cost-aware"))
		Expect(constraints).To(BeEmpty())
		Expect(inv.refreshCalled).To(BeFalse(), "the limiter must not be queried for a non-greedy optimizer")
	})
})
