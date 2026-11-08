package allocation

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
)

func init() {
	// Initialize metrics for all limiter tests
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		panic("failed to initialize metrics: " + err.Error())
	}
}

// mockInventory implements Inventory for testing
type mockInventory struct {
	name        string
	limitByType map[string]int
	usedByType  map[string]int
	refreshErr  error
}

func newMockInventory(name string, limitByType map[string]int) *mockInventory {
	return &mockInventory{
		name:        name,
		limitByType: limitByType,
		usedByType:  make(map[string]int),
	}
}

func (m *mockInventory) Name() string {
	return m.name
}

func (m *mockInventory) Refresh(ctx context.Context) error {
	return m.refreshErr
}

func (m *mockInventory) SetUsed(usedByType map[string]int) {
	m.usedByType = usedByType
}

func (m *mockInventory) TotalLimit() int {
	total := 0
	for _, v := range m.limitByType {
		total += v
	}
	return total
}

func (m *mockInventory) TotalUsed() int {
	total := 0
	for _, v := range m.usedByType {
		total += v
	}
	return total
}

func (m *mockInventory) TotalAvailable() int {
	return m.TotalLimit() - m.TotalUsed()
}

func (m *mockInventory) GetResourcePools() map[string]ResourcePool {
	pools := make(map[string]ResourcePool, len(m.limitByType))
	for accType, limit := range m.limitByType {
		used := m.usedByType[accType]
		pools[accType] = ResourcePool{Limit: limit, Used: used}
	}
	return pools
}

var _ = Describe("DefaultLimiter", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	Describe("Name", func() {
		It("should return the limiter name", func() {
			limiter := NewDefaultLimiter("test-limiter", newMockInventory("test-inventory", map[string]int{"A100": 10}))

			Expect(limiter.Name()).To(Equal("test-limiter"))
		})
	})

	Describe("ComputeConstraints", func() {
		It("exposes the inventory's per-type pools with the caller's usage applied", func() {
			inv := newMockInventory("type-inv", map[string]int{"A100": 8, "H100": 4})
			limiter := NewDefaultLimiter("gpu-limiter", inv)

			rc, err := limiter.ComputeConstraints(ctx, map[string]int{"A100": 4, "H100": 2}, nil)
			Expect(err).NotTo(HaveOccurred())

			Expect(rc.ProviderName).To(Equal("gpu-limiter"))
			Expect(rc.Pools).To(HaveKeyWithValue("A100", ResourcePool{Limit: 8, Used: 4}))
			Expect(rc.Pools).To(HaveKeyWithValue("H100", ResourcePool{Limit: 4, Used: 2}))
			Expect(rc.TotalLimit).To(Equal(12))
			Expect(rc.TotalUsed).To(Equal(6))
			Expect(rc.TotalAvail).To(Equal(6))
		})

		It("propagates a refresh error", func() {
			inv := newMockInventory("type-inv", map[string]int{"A100": 8})
			inv.refreshErr = context.DeadlineExceeded
			limiter := NewDefaultLimiter("gpu-limiter", inv)

			_, err := limiter.ComputeConstraints(ctx, nil, nil)
			Expect(err).To(MatchError(ContainSubstring("failed to refresh inventory")))
		})
	})
})
