package allocation

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
)

func newClusterQuotaInv(quotas map[string]int) *QuotaInventory {
	return NewQuotaInventory(config.QuotaLimiterConfig{
		Name:          "cluster-quota",
		Type:          "quota",
		Scope:         config.QuotaScopeCluster,
		ClusterQuotas: quotas,
	})
}

func newNamespaceQuotaInv(quotas map[string]map[string]int, exclude []string) *QuotaInventory {
	return NewQuotaInventory(config.QuotaLimiterConfig{
		Name:            "namespace-quota",
		Type:            "quota",
		Scope:           config.QuotaScopeNamespace,
		NamespaceQuotas: quotas,
		Exclude:         exclude,
	})
}

var _ = Describe("QuotaInventory", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	Describe("Inventory interface contract", func() {
		It("satisfies the Inventory interface", func() {
			var _ Inventory = (*QuotaInventory)(nil)
		})

		It("satisfies the NamespaceAwareInventory interface", func() {
			var _ NamespaceAwareInventory = (*QuotaInventory)(nil)
		})

		It("returns the configured name", func() {
			inv := NewQuotaInventory(config.QuotaLimiterConfig{
				Name:          "my-quota",
				Type:          "quota",
				Scope:         config.QuotaScopeCluster,
				ClusterQuotas: map[string]int{"H100": 1},
			})
			Expect(inv.Name()).To(Equal("my-quota"))
		})

		It("treats Refresh as a no-op", func() {
			inv := newClusterQuotaInv(map[string]int{"H100": 1})
			Expect(inv.Refresh(ctx)).To(Succeed())
		})
	})

	Describe("GetResourcePools", func() {
		It("returns per-type pools for cluster scope, emitting unlimited as a sentinel and keeping zero caps", func() {
			inv := newClusterQuotaInv(map[string]int{
				"H100": 8,
				"A100": config.QuotaUnlimited,
				"L40S": 0,
			})
			inv.SetUsed(map[string]int{"H100": 3})

			pools := inv.GetResourcePools()
			Expect(pools).To(HaveKey("H100"))
			Expect(pools["H100"].Limit).To(Equal(8))
			Expect(pools["H100"].Used).To(Equal(3))
			Expect(pools).To(HaveKey("A100"), "unlimited is emitted as a sentinel so the V2 optimizer can distinguish it from an unconfigured (deny) type")
			Expect(pools["A100"].Limit).To(Equal(config.QuotaUnlimited), "unlimited is carried as the QuotaUnlimited sentinel")
			Expect(pools).To(HaveKey("L40S"))
			Expect(pools["L40S"].Limit).To(Equal(0), "a quota of 0 is a real deny cap, distinct from unlimited")
		})

		It("sums per-type across listed namespaces for namespace scope, skipping default and excluded", func() {
			inv := newNamespaceQuotaInv(map[string]map[string]int{
				"team-a":                                {"H100": 4},
				"team-b":                                {"H100": 2, "A100": 3},
				"kube-system":                           {"H100": 99},
				config.QuotaLimiterReservedNamespaceKey: {"H100": 1},
			}, []string{"kube-system"})

			pools := inv.GetResourcePools()
			Expect(pools["H100"].Limit).To(Equal(6), "team-a(4)+team-b(2); default and excluded kube-system skipped")
			Expect(pools["A100"].Limit).To(Equal(3))
			Expect(pools).NotTo(HaveKey("team-a/H100"), "namespace scope no longer emits composite keys")
		})
	})

	Describe("NamespaceResourcePools", func() {
		It("returns per-(namespace,type) caps, applying default fall-through and excludes, with unlimited as a sentinel", func() {
			inv := newNamespaceQuotaInv(map[string]map[string]int{
				"team-a":                                {"H100": 4, "A100": config.QuotaUnlimited},
				"kube-system":                           {"H100": 99},
				config.QuotaLimiterReservedNamespaceKey: {"H100": 2},
			}, []string{"kube-system"})
			inv.SetUsedByNamespace(map[string]map[string]int{"team-a": {"H100": 1}})

			// team-a explicit; team-z unlisted (falls through to default);
			// kube-system excluded.
			pools := inv.NamespaceResourcePools([]string{"team-a", "team-z", "kube-system"})

			Expect(pools["team-a"]).To(HaveKeyWithValue("H100", ResourcePool{Limit: 4, Used: 1}))
			Expect(pools["team-a"]).To(HaveKeyWithValue("A100", ResourcePool{Limit: config.QuotaUnlimited, Used: 0}),
				"unlimited cap emitted as a sentinel, not omitted, so it stays distinguishable from an unlisted (denied) type")
			Expect(pools["team-z"]).To(HaveKeyWithValue("H100", ResourcePool{Limit: 2, Used: 0}), "default fall-through cap")
			Expect(pools).NotTo(HaveKey("kube-system"), "excluded namespace omitted (open / pass-through)")
		})

		It("emits a namespace cap of 0 as a real deny, distinct from unlimited", func() {
			// Locks in that 0 is a hard-deny cap and is not folded into the
			// unlimited sentinel — a regression that did so would turn a deny
			// quota into an unbounded one and no other spec would catch it.
			inv := newNamespaceQuotaInv(map[string]map[string]int{
				"team-a": {"H100": 0},
			}, nil)

			pools := inv.NamespaceResourcePools([]string{"team-a"})
			Expect(pools["team-a"]).To(HaveKeyWithValue("H100", ResourcePool{Limit: 0}),
				"a cap of 0 denies all allocation")
		})

		It("materializes a namespace with neither an explicit quota nor a default as an empty deny-all allowlist", func() {
			inv := newNamespaceQuotaInv(map[string]map[string]int{
				"team-a": {"H100": 4},
			}, nil)

			// team-z is unlisted and there is no "default" fall-through.
			pools := inv.NamespaceResourcePools([]string{"team-a", "team-z"})

			Expect(pools).To(HaveKey("team-z"), "present (closed allowlist) so the optimizer denies it")
			Expect(pools["team-z"]).To(BeEmpty(), "no listed types — a real deny-all")
		})

		It("returns nil for cluster scope", func() {
			inv := newClusterQuotaInv(map[string]int{"H100": 8})
			Expect(inv.NamespaceResourcePools([]string{"team-a"})).To(BeNil())
		})
	})

	Describe("totals", func() {
		It("excludes unlimited entries from cluster scope totals", func() {
			inv := newClusterQuotaInv(map[string]int{
				"H100": 8,
				"A100": config.QuotaUnlimited,
				"L40S": 2,
			})
			inv.SetUsed(map[string]int{"H100": 3, "L40S": 1})

			Expect(inv.TotalLimit()).To(Equal(10))
			Expect(inv.TotalUsed()).To(Equal(4))
			Expect(inv.TotalAvailable()).To(Equal(6))
		})

		It("skips default and excluded namespaces in namespace scope", func() {
			inv := newNamespaceQuotaInv(map[string]map[string]int{
				"team-a":                                {"H100": 4},
				"team-b":                                {"H100": 2},
				"kube-system":                           {"H100": 99},
				config.QuotaLimiterReservedNamespaceKey: {"H100": 1},
			}, []string{"kube-system"})

			Expect(inv.TotalLimit()).To(Equal(6), "sum of team-a + team-b only")
		})

		It("clamps TotalAvailable to 0 when usage exceeds the configured quota", func() {
			inv := newClusterQuotaInv(map[string]int{"H100": 2})
			inv.SetUsed(map[string]int{"H100": 5}) // over-used
			Expect(inv.TotalAvailable()).To(Equal(0), "never reports negative availability")
		})
	})

	Describe("SetUsed / SetUsedByNamespace scope guards", func() {
		It("namespace-scoped inventory ignores cluster-wide SetUsed and uses per-namespace usage", func() {
			inv := newNamespaceQuotaInv(map[string]map[string]int{"team-a": {"H100": 4}}, nil)

			// Cluster-wide SetUsed must be a no-op for a namespace-scoped inventory.
			inv.SetUsed(map[string]int{"H100": 99})
			Expect(inv.TotalUsed()).To(Equal(0), "cluster SetUsed ignored at namespace scope")

			inv.SetUsedByNamespace(map[string]map[string]int{"team-a": {"H100": 3}})
			pools := inv.NamespaceResourcePools([]string{"team-a"})
			Expect(pools["team-a"]).To(HaveKeyWithValue("H100", ResourcePool{Limit: 4, Used: 3}),
				"only SetUsedByNamespace usage applies")
		})

		It("cluster-scoped inventory ignores per-namespace SetUsedByNamespace", func() {
			inv := newClusterQuotaInv(map[string]int{"H100": 4})

			// Per-namespace usage must be a no-op for a cluster-scoped inventory.
			inv.SetUsedByNamespace(map[string]map[string]int{"team-a": {"H100": 99}})
			Expect(inv.GetResourcePools()).To(HaveKeyWithValue("H100", ResourcePool{Limit: 4, Used: 0}),
				"cluster scope ignores SetUsedByNamespace")
		})
	})
})
