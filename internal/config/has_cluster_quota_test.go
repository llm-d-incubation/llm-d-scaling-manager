package config

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Config.HasClusterQuota", func() {
	clusterQuota := QuotaLimiterConfig{
		Type: "quota", Name: "c", Scope: QuotaScopeCluster,
		ClusterQuotas: map[string]int{"A100": 16},
	}
	namespaceQuota := QuotaLimiterConfig{
		Type: "quota", Name: "n", Scope: QuotaScopeNamespace,
		Exclude:         []string{"ns-x"},
		NamespaceQuotas: map[string]map[string]int{"team-a": {"A100": 8}},
	}
	with := func(limiters ...QuotaLimiterConfig) *Config {
		c := &Config{}
		c.UpdateScalingPolicyConfig(map[string]ScalingPolicy{"default": {Limiters: limiters}})
		return c
	}

	// The limiter set holds a quota for a namespace-scoped entry alone, which
	// is how the utilization-share optimizer used to read "the cluster group is
	// bounded by a quota" -- and planned a namespace that entry excludes over
	// the other tenants' free quota.
	It("is false for a namespace-scoped quota alone, though a quota limiter is declared", func() {
		c := with(namespaceQuota)
		Expect(c.EffectiveLimiterTypes().HasQuota()).To(BeTrue())
		Expect(c.HasClusterQuota()).To(BeFalse())
	})

	It("is true for a cluster-scoped quota, alone or beside a namespace-scoped one", func() {
		Expect(with(clusterQuota).HasClusterQuota()).To(BeTrue())
		Expect(with(namespaceQuota, clusterQuota).HasClusterQuota()).To(BeTrue())
	})

	It("is false with no limiter and with the physical inventory", func() {
		Expect(with().HasClusterQuota()).To(BeFalse())
		Expect(with(QuotaLimiterConfig{Type: "gpu-inventory"}).HasClusterQuota()).To(BeFalse())
	})

	// Validation rejects a scope on gpu-inventory; the check must not rely on
	// that. Only a quota entry is a quota, whatever scope it carries.
	It("is false for a cluster scope on an entry that is not a quota", func() {
		inventory := QuotaLimiterConfig{Type: "gpu-inventory", Name: "i", Scope: QuotaScopeCluster}
		Expect(with(inventory).HasClusterQuota()).To(BeFalse())
		Expect(with(inventory, namespaceQuota).HasClusterQuota()).To(BeFalse())
	})
})
