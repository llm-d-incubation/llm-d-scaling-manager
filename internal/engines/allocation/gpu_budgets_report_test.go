package allocation

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// GPUBudgets exists so the budgets can be REPORTED with the same resolution
// FitsGPUBudget applies. A reported budget that merely looked plausible would be
// worse than no log line at all, because it would be used to rule the capacity
// check out during exactly the investigation it was added to serve.
//
// So the assertions are behavioural rather than structural: for every budget
// reported, demand at the budget fits and one more GPU does not.
var _ = Describe("GPUBudgets", func() {
	assertExplainsVerdict := func(constraints []*ResourceConstraints, wantScoped bool) {
		GinkgoHelper()
		budgets, scoped := GPUBudgets(constraints, budgetNS)
		Expect(scoped).To(Equal(wantScoped))
		Expect(budgets).ToNot(BeEmpty())

		for accType, budget := range budgets {
			if budget < 0 {
				continue // unlimited: nothing to pin
			}
			Expect(FitsGPUBudget(constraints, budgetNS, map[string]int{accType: budget})).To(BeTrue(),
				"%s: demand %d was refused, but that is the budget reported", accType, budget)
			Expect(FitsGPUBudget(constraints, budgetNS, map[string]int{accType: budget + 1})).To(BeFalse(),
				"%s: demand %d was allowed, above the reported budget of %d", accType, budget+1, budget)
		}
	}

	It("reports the budget the cluster pools actually enforce", func() {
		assertExplainsVerdict([]*ResourceConstraints{
			clusterPools(map[string]ResourcePool{"A100": {Limit: 4, Used: 4}, "H100": {Limit: 8, Used: 2}}),
		}, false)
	})

	It("reports the namespace allowlist tightened by the cluster pool", func() {
		assertExplainsVerdict([]*ResourceConstraints{nsPools(
			map[string]ResourcePool{"A100": {Limit: 8, Used: 6}},
			map[string]map[string]ResourcePool{budgetNS: {"A100": {Limit: 4, Used: 1}}},
		)}, true)
	})

	It("reports nothing rather than zero when constraints are absent", func() {
		// FitsGPUBudget treats no constraints as permissive; a line reading
		// "budgets: {}" would say "nothing is free", inverting it.
		budgets, scoped := GPUBudgets(nil, budgetNS)
		Expect(budgets).To(BeNil())
		Expect(scoped).To(BeFalse())
	})
})
