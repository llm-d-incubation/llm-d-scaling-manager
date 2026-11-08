package allocation

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/gpunodes"
)

// mockDiscovery implements gpunodes.CapacityDiscovery for testing.
type mockDiscovery struct {
	inventory map[string]map[string]gpunodes.AcceleratorModelInfo
	err       error
}

func (m *mockDiscovery) Discover(ctx context.Context) (map[string]map[string]gpunodes.AcceleratorModelInfo, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.inventory, nil
}

// mockFullDiscovery implements gpunodes.FullDiscovery for testing.
type mockFullDiscovery struct {
	inventory map[string]map[string]gpunodes.AcceleratorModelInfo
	usage     map[string]int
	discErr   error
	usageErr  error
}

func (m *mockFullDiscovery) Discover(ctx context.Context) (map[string]map[string]gpunodes.AcceleratorModelInfo, error) {
	if m.discErr != nil {
		return nil, m.discErr
	}
	return m.inventory, nil
}

func (m *mockFullDiscovery) DiscoverUsage(ctx context.Context) (map[string]int, error) {
	if m.usageErr != nil {
		return nil, m.usageErr
	}
	return m.usage, nil
}

// DiscoverNodes is required by FullDiscovery. TypeInventory doesn't use per-node
// info, so this mock returns an empty map.
func (m *mockFullDiscovery) DiscoverNodes(ctx context.Context) (map[string]gpunodes.NodeInfo, error) {
	if m.discErr != nil {
		return nil, m.discErr
	}
	return map[string]gpunodes.NodeInfo{}, nil
}

var _ = Describe("TypeInventory", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	Describe("Refresh", func() {
		DescribeTable("should aggregate GPU capacity from nodes",
			func(nodeInventory map[string]map[string]gpunodes.AcceleratorModelInfo, expectedLimits map[string]int, expectedTotal int) {
				disc := &mockDiscovery{inventory: nodeInventory}
				inv := NewTypeInventory("test", disc)

				err := inv.Refresh(ctx)
				Expect(err).NotTo(HaveOccurred())

				Expect(inv.TotalLimit()).To(Equal(expectedTotal))

				for accType, expected := range expectedLimits {
					Expect(inv.LimitByType(accType)).To(Equal(expected))
				}

				// With no usage set, available should equal limits
				Expect(inv.TotalAvailable()).To(Equal(expectedTotal))
				for accType, expected := range expectedLimits {
					Expect(inv.AvailableByType(accType)).To(Equal(expected))
				}
			},
			Entry("single node single type",
				map[string]map[string]gpunodes.AcceleratorModelInfo{
					"node-1": {"H100": {Count: 8, Memory: "80GB"}},
				},
				map[string]int{"H100": 8},
				8,
			),
			Entry("single node multiple types",
				map[string]map[string]gpunodes.AcceleratorModelInfo{
					"node-1": {
						"H100": {Count: 4, Memory: "80GB"},
						"A100": {Count: 4, Memory: "40GB"},
					},
				},
				map[string]int{"H100": 4, "A100": 4},
				8,
			),
			Entry("multiple nodes same type",
				map[string]map[string]gpunodes.AcceleratorModelInfo{
					"node-1": {"H100": {Count: 8, Memory: "80GB"}},
					"node-2": {"H100": {Count: 8, Memory: "80GB"}},
				},
				map[string]int{"H100": 16},
				16,
			),
			Entry("heterogeneous cluster",
				map[string]map[string]gpunodes.AcceleratorModelInfo{
					"node-1": {"H100": {Count: 8, Memory: "80GB"}},
					"node-2": {"A100": {Count: 8, Memory: "40GB"}},
					"node-3": {"L40S": {Count: 4, Memory: "48GB"}},
				},
				map[string]int{"H100": 8, "A100": 8, "L40S": 4},
				20,
			),
			Entry("empty cluster",
				map[string]map[string]gpunodes.AcceleratorModelInfo{},
				map[string]int{},
				0,
			),
		)
	})

	Describe("SetUsed", func() {
		It("should track GPU usage and update available capacity", func() {
			disc := &mockDiscovery{
				inventory: map[string]map[string]gpunodes.AcceleratorModelInfo{
					"node-1": {"H100": {Count: 16, Memory: "80GB"}},
					"node-2": {"A100": {Count: 8, Memory: "40GB"}},
				},
			}

			inv := NewTypeInventory("test", disc)
			err := inv.Refresh(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Initially: limit=24, used=0, available=24
			Expect(inv.TotalLimit()).To(Equal(24))
			Expect(inv.TotalUsed()).To(Equal(0))
			Expect(inv.TotalAvailable()).To(Equal(24))

			// Set some usage
			inv.SetUsed(map[string]int{"H100": 4, "A100": 2})

			// Now: limit=24, used=6, available=18
			Expect(inv.TotalLimit()).To(Equal(24))
			Expect(inv.TotalUsed()).To(Equal(6))
			Expect(inv.TotalAvailable()).To(Equal(18))

			// Per-type checks
			Expect(inv.LimitByType("H100")).To(Equal(16))
			Expect(inv.UsedByType("H100")).To(Equal(4))
			Expect(inv.AvailableByType("H100")).To(Equal(12))

			Expect(inv.LimitByType("A100")).To(Equal(8))
			Expect(inv.UsedByType("A100")).To(Equal(2))
			Expect(inv.AvailableByType("A100")).To(Equal(6))
		})
	})

	Describe("OverAllocation", func() {
		It("should handle usage exceeding limits gracefully", func() {
			disc := &mockDiscovery{
				inventory: map[string]map[string]gpunodes.AcceleratorModelInfo{
					"node-1": {"H100": {Count: 8, Memory: "80GB"}},
				},
			}

			inv := NewTypeInventory("test", disc)
			err := inv.Refresh(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Set usage greater than limit (shouldn't happen but handle gracefully)
			inv.SetUsed(map[string]int{"H100": 12})

			// Available should be 0, not negative
			Expect(inv.TotalLimit()).To(Equal(8))
			Expect(inv.TotalUsed()).To(Equal(12))
			Expect(inv.TotalAvailable()).To(Equal(0))
			Expect(inv.AvailableByType("H100")).To(Equal(0))
		})
	})

	Describe("RefreshAll", func() {
		Context("with usage discovery configured", func() {
			It("should refresh both capacity and usage", func() {
				disc := &mockFullDiscovery{
					inventory: map[string]map[string]gpunodes.AcceleratorModelInfo{
						"node-1": {"H100": {Count: 16, Memory: "80GB"}},
						"node-2": {"A100": {Count: 8, Memory: "40GB"}},
					},
					usage: map[string]int{"H100": 4, "A100": 2},
				}

				inv := NewTypeInventoryWithUsage("test", disc)
				err := inv.RefreshAll(ctx)
				Expect(err).NotTo(HaveOccurred())

				// Check limits
				Expect(inv.TotalLimit()).To(Equal(24))
				Expect(inv.LimitByType("H100")).To(Equal(16))
				Expect(inv.LimitByType("A100")).To(Equal(8))

				// Check usage (auto-discovered)
				Expect(inv.TotalUsed()).To(Equal(6))
				Expect(inv.UsedByType("H100")).To(Equal(4))
				Expect(inv.UsedByType("A100")).To(Equal(2))

				// Check available
				Expect(inv.TotalAvailable()).To(Equal(18))
				Expect(inv.AvailableByType("H100")).To(Equal(12))
				Expect(inv.AvailableByType("A100")).To(Equal(6))
			})
		})

		Context("without usage discovery configured", func() {
			It("should fail with appropriate error", func() {
				disc := &mockDiscovery{
					inventory: map[string]map[string]gpunodes.AcceleratorModelInfo{
						"node-1": {"H100": {Count: 8, Memory: "80GB"}},
					},
				}

				inv := NewTypeInventory("test", disc)
				err := inv.RefreshAll(ctx)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("usage discovery not configured"))
			})
		})
	})
})

var _ = Describe("TypeInventory normalization", func() {
	Context("with accelerator name normalization", func() {
		It("should normalize discovered GPU types", func() {
			ctx := context.Background()
			disc := &mockDiscovery{
				inventory: map[string]map[string]gpunodes.AcceleratorModelInfo{
					"node-1": {"NVIDIA-A100-PCIE-80GB": {Count: 4, Memory: "80GB"}},
					"node-2": {"NVIDIA-H100-SXM5-80GB": {Count: 8, Memory: "80GB"}},
				},
			}

			inv := NewTypeInventory("test", disc)
			err := inv.Refresh(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Verify that types are normalized to short names
			Expect(inv.LimitByType("A100")).To(Equal(4))
			Expect(inv.LimitByType("H100")).To(Equal(8))
			Expect(inv.TotalLimit()).To(Equal(12))

			// Full names should not be accessible
			Expect(inv.LimitByType("NVIDIA-A100-PCIE-80GB")).To(Equal(0))
		})
	})
})

// Unattributed usage — a workload whose accelerator could not be resolved — is
// normally dropped, because charging it to a pool it may not belong to would
// starve that pool. A HOMOGENEOUS cluster is the exception: with exactly one
// discovered type there is nowhere else those GPUs can be, so attributing them
// is a deduction rather than a guess, and it closes the over-provisioning gap
// outright for the common single-type cluster.
var _ = Describe("TypeInventory unresolved-accelerator attribution", func() {
	var ctx context.Context

	BeforeEach(func() { ctx = context.Background() })

	oneType := map[string]map[string]gpunodes.AcceleratorModelInfo{
		"node-a": {"A100": {Count: 8}},
	}
	twoTypes := map[string]map[string]gpunodes.AcceleratorModelInfo{
		"node-a": {"A100": {Count: 8}},
		"node-b": {"H100": {Count: 4}},
	}

	It("charges unresolved usage to the sole type on a homogeneous cluster", func() {
		inv := NewTypeInventory("test", &mockDiscovery{inventory: oneType})
		Expect(inv.Refresh(ctx)).To(Succeed())

		inv.SetUsed(map[string]int{"unknown": 3})

		Expect(inv.UsedByType("A100")).To(Equal(3),
			"with one type in the cluster the GPUs can only be on it")
		Expect(inv.TotalAvailable()).To(Equal(5),
			"and the pool must report them as taken, not free")
	})

	It("still drops unresolved usage when the cluster is heterogeneous", func() {
		inv := NewTypeInventory("test", &mockDiscovery{inventory: twoTypes})
		Expect(inv.Refresh(ctx)).To(Succeed())

		inv.SetUsed(map[string]int{"unknown": 3})

		Expect(inv.UsedByType("A100")).To(BeZero())
		Expect(inv.UsedByType("H100")).To(BeZero(),
			"guessing a pool would silently starve whichever one was picked")
	})

	It("keeps resolved usage exact on a homogeneous cluster", func() {
		// The fallback must not disturb usage that already resolves, including
		// the raw product label the nodeSelector deployment declares.
		inv := NewTypeInventory("test", &mockDiscovery{inventory: oneType})
		Expect(inv.Refresh(ctx)).To(Succeed())

		inv.SetUsed(map[string]int{"NVIDIA-A100-PCIE-80GB": 2, "": 1})

		Expect(inv.UsedByType("A100")).To(Equal(3),
			"the declared label reconciles, and the blank name folds in beside it")
	})
})
