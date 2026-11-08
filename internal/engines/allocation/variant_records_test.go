package allocation

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

var _ = Describe("buildVariantRecords", func() {

	It("joins discovery identity with the analyzer's capacity signal", func() {
		// The two halves come from different owners and meet only here. The
		// analyzer's entry cannot even express cost or accelerator any more, so
		// there is nothing left for the optimizer to read but discovery's.
		req := ModelScalingRequest{
			Variants: []domain.VariantMetadata{
				{VariantName: "v1", Role: "decode", Cost: 12.5, AcceleratorName: "H100"},
			},
		}
		sat := &domain.AnalyzerResult{
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "v1", Role: "decode", PerReplicaCapacity: 4000, Utilization: 0.4},
			},
		}

		records := buildVariantRecords(req, sat)

		Expect(records).To(HaveLen(1))
		Expect(records[0].Cost).To(Equal(12.5))
		Expect(records[0].AcceleratorName).To(Equal("H100"))
		Expect(records[0].Role).To(Equal("decode"))
		// The analyzer's half is carried through untouched.
		Expect(records[0].PerReplicaCapacity).To(Equal(4000.0))
		Expect(records[0].Utilization).To(Equal(0.4))
	})

	It("keeps a discovered variant the analyzer did not size, with a zero P", func() {
		// Consumers skip PerReplicaCapacity <= 0, so the variant is inert — but it
		// stays visible rather than the optimizer's view of the fleet depending on
		// which variants an analyzer happened to emit.
		req := ModelScalingRequest{
			Variants: []domain.VariantMetadata{
				{VariantName: "sized", Cost: 5},
				{VariantName: "unsized", Cost: 7},
			},
		}
		sat := &domain.AnalyzerResult{
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "sized", PerReplicaCapacity: 1000},
			},
		}

		records := buildVariantRecords(req, sat)

		Expect(records).To(HaveLen(2))
		Expect(records[0].VariantName).To(Equal("sized"))
		Expect(records[0].PerReplicaCapacity).To(Equal(1000.0))
		Expect(records[1].VariantName).To(Equal("unsized"))
		Expect(records[1].PerReplicaCapacity).To(BeZero())
	})

	It("ignores an analyzer capacity for a variant discovery does not know about", func() {
		// Metadata drives the result set, so a stale analyzer entry cannot
		// reintroduce a variant that discovery no longer reports.
		req := ModelScalingRequest{
			Variants: []domain.VariantMetadata{{VariantName: "current"}},
		}
		sat := &domain.AnalyzerResult{
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "current", PerReplicaCapacity: 100},
				{VariantName: "removed", PerReplicaCapacity: 900},
			},
		}

		records := buildVariantRecords(req, sat)

		Expect(records).To(HaveLen(1))
		Expect(records[0].VariantName).To(Equal("current"))
	})

	It("preserves discovery order", func() {
		req := ModelScalingRequest{
			Variants: []domain.VariantMetadata{
				{VariantName: "c"}, {VariantName: "a"}, {VariantName: "b"},
			},
		}
		records := buildVariantRecords(req, &domain.AnalyzerResult{})
		Expect([]string{records[0].VariantName, records[1].VariantName, records[2].VariantName}).
			To(Equal([]string{"c", "a", "b"}))
	})

	It("returns nil when the request carries no discovery metadata", func() {
		// Without metadata there is no cost and no accelerator to choose between
		// variants by, and the analyzer's output no longer carries either. Skipping
		// the model is the honest outcome; running the cost-aware math on zero costs
		// would pick arbitrarily and look like a working decision.
		sat := &domain.AnalyzerResult{
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "v1", Role: "decode", PerReplicaCapacity: 500},
			},
		}
		Expect(buildVariantRecords(ModelScalingRequest{}, sat)).To(BeNil())
	})

	It("returns nil for a nil analyzer result", func() {
		Expect(buildVariantRecords(ModelScalingRequest{}, nil)).To(BeNil())
	})

	It("recordsForRequest returns nil when the model has no composite result", func() {
		req := ModelScalingRequest{
			CompositeSignal: NamedAnalyzerResult{Name: "throughput", Result: nil},
		}
		Expect(recordsForRequest(req)).To(BeNil())
	})
})
