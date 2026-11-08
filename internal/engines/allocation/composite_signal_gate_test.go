package allocation

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

var _ = Describe("CompositeHasSignal", func() {

	It("is false for a nil Result (absent — no analysis this cycle)", func() {
		Expect(CompositeHasSignal(NamedAnalyzerResult{Result: nil})).To(BeFalse())
	})

	It("is true for a composite with at least one SO reaching a real decision path, regardless of its Name", func() {
		composite := NamedAnalyzerResult{
			Name: "CompositeSignal", // not "saturation": the composite carries its own name
			Result: &domain.AnalyzerResult{
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v", PerReplicaCapacity: 100, Reason: string(DecisionSingle)},
				},
			},
		}
		Expect(CompositeHasSignal(composite)).To(BeTrue())
	})

	// A composite not named "saturation" still passes the quota guard. A
	// name check (Name == domain.SaturationAnalyzerName && Result != nil)
	// would be permanently false here -- CompositeHasSignal must not care.
	It("is true for a renamed composite carrying a usable signal (guard not silently disabled)", func() {
		composite := NamedAnalyzerResult{
			Name: "CompositeSignal",
			Result: &domain.AnalyzerResult{
				TotalDemand:       500,
				VariantCapacities: []domain.VariantCapacity{{VariantName: "v", PerReplicaCapacity: 100, Reason: string(DecisionSingle)}},
			},
		}
		Expect(CompositeHasSignal(composite)).To(BeTrue())
		Expect(composite.Name).ToNot(Equal(domain.SaturationAnalyzerName),
			"the test is only meaningful if the composite's name really is not saturation's")
	})

	// No signal at all -> no autoscaling. Every SO's decision path bottomed
	// out at C4-no-signal.
	It("is false when every VariantCapacity's decision path is C4-no-signal (no signal at all)", func() {
		composite := NamedAnalyzerResult{
			Name: "CompositeSignal",
			Result: &domain.AnalyzerResult{
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v1", Reason: string(DecisionNoSignal)},
					{VariantName: "v2", Reason: string(DecisionNoSignal)},
				},
			},
		}
		Expect(CompositeHasSignal(composite)).To(BeFalse())
	})

	It("is false for an empty VariantCapacities slice — no capacity signal at all", func() {
		composite := NamedAnalyzerResult{
			Name:   "CompositeSignal",
			Result: &domain.AnalyzerResult{},
		}
		Expect(CompositeHasSignal(composite)).To(BeFalse())
	})

	It("is true when at least one SO reached a real decision even if others are C4-no-signal", func() {
		composite := NamedAnalyzerResult{
			Name: "CompositeSignal",
			Result: &domain.AnalyzerResult{
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v1", Reason: string(DecisionNoSignal)},
					{VariantName: "v2", PerReplicaCapacity: 200, Reason: string(DecisionAgree)},
				},
			},
		}
		Expect(CompositeHasSignal(composite)).To(BeTrue())
	})

	// An analyzer-style no-data sentinel string is NOT the same thing as the composite's own
	// DecisionNoSignal marker, and the two must never be conflated -- a
	// composite Reason of anything other than literally "C4-no-signal" (even
	// an analyzer sentinel string used by mistake) must count as usable,
	// because on the real composite construction path Reason is always a
	// decision-path value, never an analyzer sentinel.
	It("treats a composite Reason equal to an analyzer sentinel string as usable, since composite Reason means decision path, not analyzer provenance", func() {
		composite := NamedAnalyzerResult{
			Name: "CompositeSignal",
			Result: &domain.AnalyzerResult{
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v", Reason: ReasonNoData}, // NOT DecisionNoSignal -- a different string
				},
			},
		}
		Expect(CompositeHasSignal(composite)).To(BeTrue())
	})
})
