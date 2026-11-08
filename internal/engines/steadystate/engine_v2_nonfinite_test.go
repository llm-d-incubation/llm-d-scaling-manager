package steadystate

import (
	"math"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// RequiredCapacity is the figure that sizes a scale-up, so a non-finite one is
// worse than a wrong one: it reaches the optimizer as the amount to add.
//
// The clamp used to be `if rc < 0 { rc = 0 }`, and a NaN fails every comparison,
// so a NaN walked straight through it. The supply side of `rc` is now finite by
// construction (aggregation refuses to carry a non-finite capacity into a sum),
// but DEMAND is deliberately not sanitized -- silently dropping a role's demand
// would be worse than declining to act on it -- so the clamp is the place a
// non-finite figure has to fail closed.
var _ = Describe("applyUniversalThreshold with a non-finite demand", func() {
	newResult := func(demand float64) *allocation.NamedAnalyzerResult {
		return &allocation.NamedAnalyzerResult{
			Result:                 &domain.AnalyzerResult{TotalDemand: demand},
			TotalAnticipatedSupply: 1000,
			TotalSupply:            1000,
		}
	}

	for _, tc := range []struct {
		name   string
		demand float64
	}{
		{"a NaN demand", math.NaN()},
		{"an infinite demand", math.Inf(1)},
	} {
		It(tc.name+" never becomes a required capacity", func() {
			nr := newResult(tc.demand)
			applyUniversalThreshold(nr, 0.85, 0.5)

			// FINITE, not merely non-NaN. Asserting only IsNaN let a +Inf
			// through: +Inf is not NaN and is >= 0, so both of those checks
			// passed while RequiredCapacity was infinite. A mutation that
			// stopped excluding +Inf survived the suite until this asserted
			// finiteness.
			for label, v := range map[string]float64{
				"RequiredCapacity": nr.RequiredCapacity,
				"SpareCapacity":    nr.SpareCapacity,
			} {
				Expect(math.IsNaN(v)).To(BeFalse(), "%s must not be NaN", label)
				Expect(math.IsInf(v, 0)).To(BeFalse(), "%s must not be infinite", label)
			}
			Expect(nr.RequiredCapacity).To(BeNumerically(">=", 0),
				"and it is never negative")
		})
	}

	It("leaves a real demand alone", func() {
		// The clamp must still compute, not just refuse: demand 2000 at a 0.85
		// threshold against 1000 anticipated is 2000/0.85 - 1000 = 1352.94.
		nr := newResult(2000)
		applyUniversalThreshold(nr, 0.85, 0.5)
		Expect(nr.RequiredCapacity).To(BeNumerically("~", 2000/0.85-1000, 1e-6),
			"the ordinary path is unchanged")
	})

	It("clamps a demand the fleet already covers to zero, as before", func() {
		nr := newResult(500)
		applyUniversalThreshold(nr, 0.85, 0.5)
		Expect(nr.RequiredCapacity).To(BeZero(),
			"500/0.85 is under 1000 anticipated, so nothing is required")
	})
})
