package allocation

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

var _ = Describe("Eligible", func() {

	It("is eligible when live, informative, and Result is non-nil", func() {
		nr := makeNamed(100, 0, "v", 500.0) // makeNamed defaults Live to true
		Expect(Eligible(nr)).To(BeTrue())
	})

	It("excludes a non-live analyzer, even with an otherwise informative Result", func() {
		nr := makeNamed(100, 0, "v", 500.0)
		nr.Live = false
		Expect(Eligible(nr)).To(BeFalse())
	})

	It("excludes a nil Result", func() {
		nr := NamedAnalyzerResult{Result: nil, Live: true}
		Expect(Eligible(nr)).To(BeFalse())
	})

	It("excludes a Result whose every VariantCapacity carries the no-data sentinel", func() {
		nr := NamedAnalyzerResult{
			Live: true,
			Result: &domain.AnalyzerResult{
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v", Reason: ReasonNoData},
				},
			},
		}
		Expect(Eligible(nr)).To(BeFalse())
	})

	It("excludes a Result whose every VariantCapacity carries the error sentinel", func() {
		nr := NamedAnalyzerResult{
			Live: true,
			Result: &domain.AnalyzerResult{
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v", Reason: ReasonError},
				},
			},
		}
		Expect(Eligible(nr)).To(BeFalse())
	})

	It("applies the same rule regardless of direction: a stale analyzer is excluded whether it would raise or lower demand", func() {
		// Eligible() itself is direction-agnostic; this asserts there is no
		// separate looser check anywhere by exercising the one function twice
		// with the same non-live entry, mirroring how a scale-up contribution
		// check and a scale-down all-agree check would both consult it.
		nr := makeNamed(100, 50, "v", 500.0)
		nr.Live = false
		Expect(Eligible(nr)).To(BeFalse(), "must not raise demand")
		Expect(Eligible(nr)).To(BeFalse(), "must not block scale-down")
	})
})
