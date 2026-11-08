package config

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("QuotaLimiterConfig", func() {

	Context("IsExcluded", func() {
		DescribeTable("namespace exclusion",
			func(entry QuotaLimiterConfig, namespace string, want bool) {
				Expect(entry.IsExcluded(namespace)).To(Equal(want))
			},
			Entry("cluster scope never excludes",
				QuotaLimiterConfig{Scope: QuotaScopeCluster, Exclude: []string{"any"}},
				"any", false),
			Entry("namespace scope matches exact entry",
				QuotaLimiterConfig{Scope: QuotaScopeNamespace, Exclude: []string{"kube-system", "llm-d-system"}},
				"kube-system", true),
			Entry("namespace scope returns false for unlisted namespace",
				QuotaLimiterConfig{Scope: QuotaScopeNamespace, Exclude: []string{"kube-system"}},
				"team-a", false),
		)
	})

	Context("QuotaForNamespace", func() {
		var entry QuotaLimiterConfig

		BeforeEach(func() {
			entry = QuotaLimiterConfig{
				Name:  "ns-quota",
				Type:  "quota",
				Scope: QuotaScopeNamespace,
				Exclude: []string{
					"kube-system",
				},
				NamespaceQuotas: map[string]map[string]int{
					"team-a": {
						"H100": 8,
						"A100": 4,
					},
					"team-priority": {
						"H100": QuotaUnlimited,
					},
					QuotaLimiterReservedNamespaceKey: {
						"H100": 2,
					},
				},
			}
		})

		It("returns excluded=true and nil map for excluded namespace", func() {
			quotas, excluded := entry.QuotaForNamespace("kube-system")
			Expect(excluded).To(BeTrue())
			Expect(quotas).To(BeNil())
		})

		It("returns exact match in preference to the default key", func() {
			quotas, excluded := entry.QuotaForNamespace("team-a")
			Expect(excluded).To(BeFalse())
			Expect(quotas).To(HaveKeyWithValue("H100", 8))
		})

		It("preserves QuotaUnlimited verbatim", func() {
			quotas, _ := entry.QuotaForNamespace("team-priority")
			Expect(quotas).To(HaveKeyWithValue("H100", QuotaUnlimited))
		})

		It("falls through to the default key for unlisted namespaces", func() {
			quotas, _ := entry.QuotaForNamespace("unknown")
			Expect(quotas).To(HaveKeyWithValue("H100", 2))
		})

		It("returns an empty map for strict allowlist (no default key)", func() {
			strict := QuotaLimiterConfig{
				Scope: QuotaScopeNamespace,
				NamespaceQuotas: map[string]map[string]int{
					"team-a": {"H100": 4},
				},
			}
			quotas, excluded := strict.QuotaForNamespace("team-b")
			Expect(excluded).To(BeFalse())
			Expect(quotas).To(BeEmpty())
		})

		It("returns (nil, false) for cluster scope", func() {
			cluster := QuotaLimiterConfig{
				Scope:         QuotaScopeCluster,
				ClusterQuotas: map[string]int{"H100": 16},
			}
			quotas, excluded := cluster.QuotaForNamespace("any")
			Expect(excluded).To(BeFalse())
			Expect(quotas).To(BeNil())
		})
	})
})

var _ = Describe("QuotaLimiterEntries.Validate", func() {

	It("accepts a combined cluster + namespace config", func() {
		entries := &QuotaLimiterEntries{
			Limiters: []QuotaLimiterConfig{
				{
					Name:          "cluster-quota",
					Type:          "quota",
					Scope:         QuotaScopeCluster,
					ClusterQuotas: map[string]int{"H100": 16, "A100": 8},
				},
				{
					Name:    "namespace-quota",
					Type:    "quota",
					Scope:   QuotaScopeNamespace,
					Exclude: []string{"kube-system"},
					NamespaceQuotas: map[string]map[string]int{
						"team-a":                         {"H100": 8},
						"team-b":                         {"H100": QuotaUnlimited},
						QuotaLimiterReservedNamespaceKey: {"H100": 2},
					},
				},
			},
		}
		warnings, err := entries.Validate()
		Expect(err).NotTo(HaveOccurred())
		Expect(warnings).To(BeEmpty())
	})

	It("warns (non-fatal) when a namespace is in both exclude and namespaceQuotas", func() {
		entries := &QuotaLimiterEntries{
			Limiters: []QuotaLimiterConfig{
				{
					Name:    "ns",
					Type:    "quota",
					Scope:   QuotaScopeNamespace,
					Exclude: []string{"team-a"},
					NamespaceQuotas: map[string]map[string]int{
						"team-a": {"H100": 4},
					},
				},
			},
		}
		warnings, err := entries.Validate()
		Expect(err).NotTo(HaveOccurred())
		Expect(warnings).To(ContainElement(And(ContainSubstring("team-a"), ContainSubstring("both exclude and namespaceQuotas"))))
	})

	Describe("exclude warnings", func() {
		tenants := func(exclude ...string) QuotaLimiterConfig {
			return QuotaLimiterConfig{Name: "tenants", Type: "quota", Scope: QuotaScopeNamespace,
				Exclude: exclude, NamespaceQuotas: map[string]map[string]int{"team-a": {"A100": 8, "H100": 4}}}
		}
		cluster := func(quotas map[string]int) QuotaLimiterConfig {
			return QuotaLimiterConfig{Name: "cluster", Type: "quota", Scope: QuotaScopeCluster, ClusterQuotas: quotas}
		}
		// A second cluster entry needs a name of its own.
		const secondClusterName = "cluster-2"
		validate := func(entries ...QuotaLimiterConfig) []string {
			GinkgoHelper()
			e := &QuotaLimiterEntries{Limiters: entries}
			warnings, err := e.Validate()
			Expect(err).NotTo(HaveOccurred())
			return append(warnings, e.unboundedExcludeWarnings(false)...)
		}

		It("warns that an excluded namespace has no limit without a cluster quota", func() {
			Expect(validate(tenants("ns-x"))).To(ConsistOf(
				And(ContainSubstring("[ns-x]"), ContainSubstring("no scope: cluster quota"))))
			excludeOnly := QuotaLimiterConfig{Name: "tenants", Type: "quota", Scope: QuotaScopeNamespace, Exclude: []string{"ns-x"}}
			Expect(validate(excludeOnly)).To(ConsistOf(ContainSubstring("[ns-x] have no GPU limit")),
				"also when the namespace quota names no accelerator type")
		})

		It("names the accelerator types every cluster quota leaves unlimited", func() {
			Expect(validate(tenants("ns-x"), cluster(map[string]int{"A100": -1, "H100": -1}))).To(ConsistOf(
				And(ContainSubstring("[ns-x]"), ContainSubstring("on [A100, H100]"))),
				"an unlimited cluster quota bounds nothing")
			Expect(validate(tenants("ns-x"), cluster(map[string]int{"H100": 16, "A100": -1}))).To(ConsistOf(
				And(ContainSubstring("[ns-x]"), ContainSubstring("on [A100]"))))
			Expect(validate(tenants("ns-x"), cluster(map[string]int{"H100": 16, "A100": 20}))).To(BeEmpty())
			Expect(validate(tenants("ns-x"), cluster(map[string]int{"H100": 16, "L4": -1}))).To(ConsistOf(
				ContainSubstring("on [L4]")), "a type only the cluster quota names counts too")
		})

		// A cluster quota is a closed allowlist: a type it does not list is
		// denied to an excluded namespace, not left unbounded.
		It("does not warn about a type a cluster quota leaves out", func() {
			Expect(validate(tenants("ns-x"), cluster(map[string]int{"H100": 16}))).To(BeEmpty())
			second := cluster(map[string]int{"H100": 8})
			second.Name = secondClusterName
			Expect(validate(tenants("ns-x"), cluster(map[string]int{"A100": -1}), second)).To(BeEmpty(),
				"the second cluster quota denies A100")
		})

		It("does not warn without an exclude list", func() {
			Expect(validate(tenants())).To(BeEmpty())
		})

		It("warns about an exclude entry that can never match a namespace", func() {
			bounded := cluster(map[string]int{"H100": 16, "A100": 20})
			Expect(validate(tenants(" ns-x"), bounded)).To(ConsistOf(ContainSubstring(`" ns-x" is not a valid namespace name`)))
			Expect(validate(tenants("NS-X"), bounded)).To(ConsistOf(ContainSubstring(`"NS-X" is not a valid namespace name`)))
			Expect(validate(tenants("ns-x"), bounded)).To(BeEmpty())
		})

		// Its static -1 would read as unbounded; Kueue's figures bound it at
		// runtime, and they cannot be read here.
		It("does not warn when a cluster quota reads its numbers from Kueue", func() {
			kueue := QuotaLimiterConfig{Name: "cluster", Type: "quota", Scope: QuotaScopeCluster,
				ClusterQuotas: map[string]int{"A100": -1}, Kueue: &KueueQuotaSource{Enabled: true}}
			Expect(validate(tenants("ns-x"), kueue)).To(BeEmpty())
			kueue.Kueue = nil
			Expect(validate(tenants("ns-x"), kueue)).To(ConsistOf(ContainSubstring("on [A100]")),
				"the same entry without Kueue")
		})

		It("does not warn about a namespace a namespace quota that reads Kueue holds", func() {
			kueue := QuotaLimiterConfig{Name: "kueue-tenants", Type: "quota", Scope: QuotaScopeNamespace,
				NamespaceQuotas: map[string]map[string]int{"default": {"A100": -1}}, Kueue: &KueueQuotaSource{Enabled: true}}
			Expect(validate(tenants("ns-x"), kueue)).To(BeEmpty())
		})

		It("matches accelerator names as the limiter does at runtime", func() {
			Expect(validate(tenants("ns-x"), cluster(map[string]int{"a100": -1, "NVIDIA-H100-80GB-HBM3": 16}))).To(ConsistOf(
				ContainSubstring("on [A100]")), "a100 is A100; the H100 product key bounds the H100 family")
			Expect(validate(tenants("ns-x"), cluster(map[string]int{"NVIDIA-A100-SXM4-80GB": 20, "nvidia-h100-80gb": -1}))).To(ConsistOf(
				ContainSubstring("on [H100 (bare name), nvidia-h100-80gb]")),
				"the bare family is the sum of its listed products; an unlisted H100 product is denied")
		})

		// The planner reads a family key as the bound of every product a
		// quota does not list (accelerator.AllowlistBound), so the warning must
		// too: a finite product key does not bound its family.
		It("reads a family key as the planner does: it bounds the products the quota does not list", func() {
			Expect(validate(tenants("ns-x"), cluster(map[string]int{"H100": -1, "NVIDIA-H100-PCIE-80GB": 4}))).To(ConsistOf(
				ContainSubstring("on [H100 (bare name), other H100 products]")),
				"only PCIE has a number; HBM3, SXM5 and the bare family fall back to H100: -1")
			Expect(validate(tenants("ns-x"), cluster(map[string]int{"H100": -1, "L4": -1, "NVIDIA-H100-80GB-HBM3": 0}))).To(ConsistOf(
				ContainSubstring("on [H100 (bare name), other H100 products, L4]")))
			second := cluster(map[string]int{"H100": -1, "L4": 0, "NVIDIA-H100-PCIE-80GB": 20})
			second.Name = secondClusterName
			Expect(validate(tenants("ns-x"), cluster(map[string]int{"NVIDIA-H100-PCIE-80GB": -1}), second)).To(BeEmpty(),
				"each quota alone leaves a bare H100 unlimited, but under the first it can only be PCIE, which the second holds to 20")
			Expect(validate(tenants("ns-x"), cluster(map[string]int{"H100": 16, "NVIDIA-H100-PCIE-80GB": -1}))).To(BeEmpty(),
				"a finite family key bounds its products")
		})

		// Each cluster quota leaves "unknown" (a workload naming no GPU type)
		// unbounded unless it lists it, so one quota listing it as -1 is enough.
		It("warns about a workload that names no accelerator when a cluster quota lists unknown as -1", func() {
			second := cluster(map[string]int{"A100": 4})
			second.Name = secondClusterName
			Expect(validate(tenants("ns-x"), cluster(map[string]int{"unknown": -1, "A100": 4}), second)).To(ConsistOf(
				ContainSubstring("on [unknown]")))
			finite := cluster(map[string]int{"unknown": 2})
			finite.Name = secondClusterName
			Expect(validate(tenants("ns-x"), cluster(map[string]int{"unknown": -1, "A100": 4}), finite)).To(BeEmpty())
		})

		// A namespace another namespace quota holds is a closed allowlist there,
		// bounded by that quota's caps for it, which may be -1.
		It("warns about an excluded namespace another namespace quota holds with -1", func() {
			others := QuotaLimiterConfig{Name: "others", Type: "quota", Scope: QuotaScopeNamespace,
				NamespaceQuotas: map[string]map[string]int{"default": {"A100": -1}}}
			Expect(validate(tenants("ns-x"), others)).To(ConsistOf(And(
				ContainSubstring("exclude [ns-x] have no GPU limit on [A100]"),
				ContainSubstring(`the namespace quotas that hold them ("others") set those accelerator types to -1`))))
			Expect(validate(tenants("ns-x"), others, cluster(map[string]int{"A100": 8}))).To(BeEmpty(),
				"a finite cluster quota bounds it")
			bounded := others
			bounded.NamespaceQuotas = map[string]map[string]int{"default": {"A100": 2}}
			Expect(validate(tenants("ns-x"), bounded)).To(BeEmpty())
		})

		// The planner bounds a bare family in a namespace that lists only
		// products of it by those products under the cluster quota.
		It("reads a family a holding quota lists only through products as the planner does", func() {
			sxm5Only := QuotaLimiterConfig{Name: "others", Type: "quota", Scope: QuotaScopeNamespace,
				NamespaceQuotas: map[string]map[string]int{"ns-x": {"NVIDIA-H100-SXM5-80GB": -1}}}
			Expect(validate(tenants("ns-x"), sxm5Only, cluster(map[string]int{"NVIDIA-H100-80GB-HBM3": -1}))).To(BeEmpty(),
				"a bare H100 in ns-x can only be SXM5, which the cluster quota denies")
			Expect(validate(tenants("ns-x"), sxm5Only, cluster(map[string]int{"NVIDIA-H100-SXM5-80GB": -1}))).To(ConsistOf(
				ContainSubstring("on [H100 (bare name), NVIDIA-H100-SXM5-80GB]")), "the cluster quota leaves SXM5 unlimited")
		})

		// The planner bounds a bare family in a namespace that lists the family
		// and a product of it by the products the namespace may use, when the
		// cluster quota lists the family only through products.
		It("reads a family a holding quota lists beside a product as the planner does", func() {
			holder := QuotaLimiterConfig{Name: "others", Type: "quota", Scope: QuotaScopeNamespace,
				NamespaceQuotas: map[string]map[string]int{"ns-x": {"H100": -1, "NVIDIA-H100-SXM5-80GB": 0}}}
			Expect(validate(tenants("ns-x"), holder, cluster(map[string]int{"NVIDIA-H100-80GB-HBM3": 8, "NVIDIA-H100-SXM5-80GB": -1}))).To(BeEmpty(),
				"a bare H100 in ns-x can only be HBM3, which the cluster quota holds to 8")
			Expect(validate(tenants("ns-x"), holder, cluster(map[string]int{"NVIDIA-H100-80GB-HBM3": -1, "NVIDIA-H100-SXM5-80GB": 8}))).To(ConsistOf(
				ContainSubstring("on [H100 (bare name), NVIDIA-H100-80GB-HBM3]")), "the cluster quota leaves HBM3 unlimited")
		})

		It("does not warn about a namespace another namespace quota caps", func() {
			capsX := QuotaLimiterConfig{Name: "b", Type: "quota", Scope: QuotaScopeNamespace,
				Exclude: []string{"ns-y"}, NamespaceQuotas: map[string]map[string]int{"ns-x": {"A100": 4}}}
			Expect(validate(tenants("ns-x"), capsX)).To(BeEmpty())
			// Both entries exclude ns-y, so each says so; neither names ns-x.
			Expect(validate(tenants("ns-x", "ns-y"), capsX)).To(And(HaveLen(2),
				HaveEach(And(ContainSubstring("exclude [ns-y] have no GPU limit"), Not(ContainSubstring("ns-x"))))))
		})

		It("leaves the bound check out of Validate: a physical limiter is not one of these entries", func() {
			warnings, err := (&QuotaLimiterEntries{Limiters: []QuotaLimiterConfig{tenants("ns-x")}}).Validate()
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeEmpty())
		})
	})

	// SR-M5 and #216: a quota entry named like the physical limiter, node fit
	// or a borrowing rule would be reported under a name that means something
	// else. v0.10.0 rejects it; the release before only warned.
	DescribeTable("reserved names are rejected",
		func(name string) {
			entries := &QuotaLimiterEntries{Limiters: []QuotaLimiterConfig{
				{Name: name, Type: "quota", Scope: QuotaScopeCluster, ClusterQuotas: map[string]int{"H100": 8}},
			}}
			warnings, err := entries.Validate()
			Expect(err).To(MatchError(And(
				ContainSubstring(`entry[0]: quota entry name %q is reserved`, name),
				ContainSubstring("rename the entry"))))
			Expect(warnings).To(BeEmpty())
		},
		Entry("the physical limiter's reported name", "gpu-limiter"),
		Entry("node-fit", "node-fit"),
		Entry("pd-pair-borrows-whole", "pd-pair-borrows-whole"),
		Entry("lender-cannot-reclaim", "lender-cannot-reclaim"),
	)

	It("accepts a physical entry named gpu-limiter beside a quota entry named otherwise", func() {
		// Only a quota entry is reported under its own name; a physical entry
		// is always reported as gpu-limiter, whatever it is named.
		policy := ScalingPolicy{Limiters: []QuotaLimiterConfig{
			{Name: "gpu-limiter", Type: "gpu-inventory"},
			{Name: "install-quota", Type: "quota", Scope: QuotaScopeCluster, ClusterQuotas: map[string]int{"H100": 8}},
		}}
		Expect(policy.validateLimiters()).To(Succeed())
	})

	It("accumulates multiple errors instead of failing fast", func() {
		// Two broken entries: one with wrong type, one with bad scope.
		// errors.Join joins messages with newlines; both should appear.
		entries := &QuotaLimiterEntries{
			Limiters: []QuotaLimiterConfig{
				{Name: "a", Type: "reservation", Scope: QuotaScopeCluster, ClusterQuotas: map[string]int{"H100": 1}},
				{Name: "b", Type: "quota", Scope: "global"},
			},
		}
		_, err := entries.Validate()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(`type must be "quota"`))
		Expect(err.Error()).To(ContainSubstring(`scope must be`))
	})

	DescribeTable("error cases",
		func(entries *QuotaLimiterEntries, want string) {
			_, err := entries.Validate()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(want))
		},
		Entry("empty name rejected",
			&QuotaLimiterEntries{Limiters: []QuotaLimiterConfig{
				{Name: "", Type: "quota", Scope: QuotaScopeCluster, ClusterQuotas: map[string]int{"H100": 8}},
			}}, "name must not be empty"),
		Entry("duplicate name rejected",
			&QuotaLimiterEntries{Limiters: []QuotaLimiterConfig{
				{Name: "x", Type: "quota", Scope: QuotaScopeCluster, ClusterQuotas: map[string]int{"H100": 8}},
				{Name: "x", Type: "quota", Scope: QuotaScopeNamespace, NamespaceQuotas: map[string]map[string]int{"a": {"H100": 1}}},
			}}, "duplicate limiter name"),
		Entry("wrong type rejected",
			&QuotaLimiterEntries{Limiters: []QuotaLimiterConfig{
				{Name: "x", Type: "reservation", Scope: QuotaScopeCluster, ClusterQuotas: map[string]int{"H100": 8}},
			}}, `type must be "quota"`),
		Entry("unknown scope rejected",
			&QuotaLimiterEntries{Limiters: []QuotaLimiterConfig{
				{Name: "x", Type: "quota", Scope: "global"},
			}}, "scope must be"),
		Entry("cluster scope rejects namespaceQuotas",
			&QuotaLimiterEntries{Limiters: []QuotaLimiterConfig{
				{
					Name: "x", Type: "quota", Scope: QuotaScopeCluster,
					NamespaceQuotas: map[string]map[string]int{"a": {"H100": 1}},
				},
			}}, "namespaceQuotas is invalid for cluster scope"),
		Entry("cluster scope rejects exclude",
			&QuotaLimiterEntries{Limiters: []QuotaLimiterConfig{
				{
					Name: "x", Type: "quota", Scope: QuotaScopeCluster,
					ClusterQuotas: map[string]int{"H100": 1},
					Exclude:       []string{"kube-system"},
				},
			}}, "exclude is invalid for cluster scope"),
		Entry("namespace scope rejects cluster quotas",
			&QuotaLimiterEntries{Limiters: []QuotaLimiterConfig{
				{
					Name: "x", Type: "quota", Scope: QuotaScopeNamespace,
					ClusterQuotas: map[string]int{"H100": 8},
				},
			}}, "quotas is invalid for namespace scope"),
		Entry("value < -1 rejected",
			&QuotaLimiterEntries{Limiters: []QuotaLimiterConfig{
				{
					Name: "x", Type: "quota", Scope: QuotaScopeCluster,
					ClusterQuotas: map[string]int{"H100": -5},
				},
			}}, "only -1 is allowed as negative"),
		Entry("value < -1 in namespaceQuotas inner map rejected",
			&QuotaLimiterEntries{Limiters: []QuotaLimiterConfig{
				{
					Name: "x", Type: "quota", Scope: QuotaScopeNamespace,
					NamespaceQuotas: map[string]map[string]int{"team-a": {"H100": -5}},
				},
			}}, "only -1 is allowed as negative"),
		Entry("value above MaxQuotaValue rejected",
			&QuotaLimiterEntries{Limiters: []QuotaLimiterConfig{
				{
					Name: "x", Type: "quota", Scope: QuotaScopeCluster,
					ClusterQuotas: map[string]int{"H100": MaxQuotaValue + 1},
				},
			}}, "exceeds the maximum"),
		Entry("empty accelerator type rejected",
			&QuotaLimiterEntries{Limiters: []QuotaLimiterConfig{
				{
					Name: "x", Type: "quota", Scope: QuotaScopeCluster,
					ClusterQuotas: map[string]int{"": 8},
				},
			}}, "accelerator type name must not be empty"),
		Entry("empty namespace name rejected",
			&QuotaLimiterEntries{Limiters: []QuotaLimiterConfig{
				{
					Name: "x", Type: "quota", Scope: QuotaScopeNamespace,
					NamespaceQuotas: map[string]map[string]int{"": {"H100": 1}},
				},
			}}, "namespaceQuotas contains an empty namespace key"),
		Entry("empty exclude name rejected",
			&QuotaLimiterEntries{Limiters: []QuotaLimiterConfig{
				{
					Name: "x", Type: "quota", Scope: QuotaScopeNamespace,
					Exclude:         []string{""},
					NamespaceQuotas: map[string]map[string]int{"team-a": {"H100": 1}},
				},
			}}, "exclude contains an empty namespace name"),
	)
})
