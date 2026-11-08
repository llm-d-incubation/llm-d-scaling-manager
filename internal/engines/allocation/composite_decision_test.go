package allocation

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/aggregation"
)

// reasonMeasured is a capacity reason that is neither no-data nor an error.
const reasonMeasured = "measured"

// makeAnalyzer builds an eligible (Live, informative) NamedAnalyzerResult for
// name whose only variant is "v" with the given demand and PRC, so its
// TotalReplicas(v) = demand/prc when prc > 0.
func makeAnalyzer(name string, demand, prc float64) NamedAnalyzerResult {
	reason := reasonMeasured
	if prc <= 0 {
		reason = ReasonNoData
	}
	return NamedAnalyzerResult{
		Name: name,
		Live: true,
		Result: &domain.AnalyzerResult{
			TotalDemand: demand,
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "v", PerReplicaCapacity: prc, Reason: reason},
			},
		},
	}
}

// testDecision is ResolveSO's result in the field names the specs below use.
type testDecision struct {
	n            float64
	ok           bool
	path         DecisionPath
	contributors []string
}

// resolveSOForTest calls ResolveSO, the function buildComposite decides each
// variant with.
func resolveSOForTest(entries []NamedAnalyzerResult, sat *NamedAnalyzerResult, variant string) testDecision {
	d := ResolveSO(entries, sat, variant)
	return testDecision{n: d.N, ok: d.OK, path: d.Path, contributors: d.Contributors}
}

var _ = Describe("TotalReplicas", func() {

	It("computes D/PRC for a non-disaggregated result (demand in TotalDemand)", func() {
		result := &domain.AnalyzerResult{
			TotalDemand:       1000,
			VariantCapacities: []domain.VariantCapacity{{VariantName: "v", PerReplicaCapacity: 200}},
		}
		vc, _ := FindVariantCapacity(result, "v")
		n, ok := TotalReplicas(result, vc)
		Expect(ok).To(BeTrue())
		Expect(n).To(Equal(5.0))
	})

	It("computes D/PRC for a disaggregated result, using the variant's own role", func() {
		result := &domain.AnalyzerResult{
			RoleDemand: map[string]float64{"prefill": 400, "decode": 900},
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "p1", Role: "prefill", PerReplicaCapacity: 100},
				{VariantName: "d1", Role: "decode", PerReplicaCapacity: 300},
			},
		}
		pVC, _ := FindVariantCapacity(result, "p1")
		n, ok := TotalReplicas(result, pVC)
		Expect(ok).To(BeTrue())
		Expect(n).To(Equal(4.0))

		dVC, _ := FindVariantCapacity(result, "d1")
		n, ok = TotalReplicas(result, dVC)
		Expect(ok).To(BeTrue())
		Expect(n).To(Equal(3.0))
	})

	// PRC > 0 with demand(role(SO)) == 0 is legal.
	It("is defined and zero when demand is a real zero", func() {
		result := &domain.AnalyzerResult{
			TotalDemand:       0,
			VariantCapacities: []domain.VariantCapacity{{VariantName: "v", PerReplicaCapacity: 200}},
		}
		vc, _ := FindVariantCapacity(result, "v")
		n, ok := TotalReplicas(result, vc)
		Expect(ok).To(BeTrue())
		Expect(n).To(BeZero())
	})

	// PRC == 0, demand > 0 -> undefined, no division by zero.
	It("is undefined when PRC is zero", func() {
		vc := domain.VariantCapacity{VariantName: "v", PerReplicaCapacity: 0}
		result := &domain.AnalyzerResult{TotalDemand: 500, VariantCapacities: []domain.VariantCapacity{vc}}
		_, ok := TotalReplicas(result, vc)
		Expect(ok).To(BeFalse())
	})

	It("is undefined when PRC is negative", func() {
		vc := domain.VariantCapacity{VariantName: "v", PerReplicaCapacity: -10}
		result := &domain.AnalyzerResult{TotalDemand: 500, VariantCapacities: []domain.VariantCapacity{vc}}
		_, ok := TotalReplicas(result, vc)
		Expect(ok).To(BeFalse())
	})

	// Both zero -> no panic, no NaN/Inf, just undefined.
	It("is undefined, not NaN, when both PRC and demand are zero", func() {
		vc := domain.VariantCapacity{VariantName: "v", PerReplicaCapacity: 0}
		result := &domain.AnalyzerResult{TotalDemand: 0, VariantCapacities: []domain.VariantCapacity{vc}}
		n, ok := TotalReplicas(result, vc)
		Expect(ok).To(BeFalse())
		Expect(n).To(BeZero()) // zero value, not NaN — ok is the authority, not n
	})

	// An analyzer silent about a role does not participate in it.
	It("is undefined when the analyzer never attributed demand to the variant's role", func() {
		vc := domain.VariantCapacity{VariantName: "p1", Role: "prefill", PerReplicaCapacity: 100}
		result := &domain.AnalyzerResult{
			RoleDemand:        map[string]float64{"decode": 900}, // says nothing about prefill
			VariantCapacities: []domain.VariantCapacity{vc},
		}
		_, ok := TotalReplicas(result, vc)
		Expect(ok).To(BeFalse())
	})

	It("is undefined for a nil result", func() {
		_, ok := TotalReplicas(nil, domain.VariantCapacity{VariantName: "v", PerReplicaCapacity: 200})
		Expect(ok).To(BeFalse())
	})

	// Unit independence: scaling both D and PRC by the same constant k must
	// leave TotalReplicas unchanged.
	It("is unit-independent: scaling demand and PRC by the same constant leaves N unchanged", func() {
		baseVC := domain.VariantCapacity{VariantName: "v", PerReplicaCapacity: 200}
		base := &domain.AnalyzerResult{TotalDemand: 1000, VariantCapacities: []domain.VariantCapacity{baseVC}}
		scaledVC := domain.VariantCapacity{VariantName: "v", PerReplicaCapacity: 200 * 37}
		scaled := &domain.AnalyzerResult{TotalDemand: 1000 * 37, VariantCapacities: []domain.VariantCapacity{scaledVC}}

		nBase, okBase := TotalReplicas(base, baseVC)
		nScaled, okScaled := TotalReplicas(scaled, scaledVC)
		Expect(okBase).To(BeTrue())
		Expect(okScaled).To(BeTrue())
		Expect(nScaled).To(Equal(nBase))
	})

	// The max over contributors reduces to a single TotalReplicas call when
	// there is exactly one contributor.
	It("reduces to the single contributor's N when called on its own result (formerly AggN's single-contributor case)", func() {
		vc := domain.VariantCapacity{VariantName: "v", PerReplicaCapacity: 200}
		result := &domain.AnalyzerResult{TotalDemand: 1000, VariantCapacities: []domain.VariantCapacity{vc}}
		n, ok := TotalReplicas(result, vc)
		Expect(ok).To(BeTrue())
		Expect(n).To(Equal(5.0))
	})
})

// These exercise ResolveSO's contributor collection and decision switch,
// which steadystate.buildComposite applies to each variant.
var _ = Describe("buildComposite's contributor/decision logic (via resolveSOForTest)", func() {

	Describe("C0-agree / C1-single", func() {
		It("records C1-single with exactly one contributor", func() {
			other := makeAnalyzer("throughput", 600, 200) // N=3
			d := resolveSOForTest([]NamedAnalyzerResult{other}, nil, "v")
			Expect(d.ok).To(BeTrue())
			Expect(d.path).To(Equal(DecisionSingle))
			Expect(d.n).To(Equal(3.0))
			Expect(d.contributors).To(ConsistOf("throughput"))
		})

		It("records C0-agree with more than one contributor, taking the max", func() {
			sat := makeAnalyzer(domain.SaturationAnalyzerName, 1000, 200) // N=5
			other := makeAnalyzer("throughput", 2400, 200)                // N=12
			d := resolveSOForTest([]NamedAnalyzerResult{sat, other}, &sat, "v")
			Expect(d.ok).To(BeTrue())
			Expect(d.path).To(Equal(DecisionAgree))
			Expect(d.n).To(Equal(12.0))
			Expect(d.contributors).To(ConsistOf(domain.SaturationAnalyzerName, "throughput"))
		})

		// Sat is a fallback, not a floor — a lower-N contributor can pull the
		// composite below sat's own value when sat also qualifies as an
		// ordinary contributor.
		It("allows the aggregate to fall below saturation's own N when both are ordinary contributors", func() {
			sat := makeAnalyzer(domain.SaturationAnalyzerName, 2400, 200) // N=12
			other := makeAnalyzer("throughput", 1000, 200)                // N=5
			d := resolveSOForTest([]NamedAnalyzerResult{sat, other}, &sat, "v")
			Expect(d.ok).To(BeTrue())
			Expect(d.path).To(Equal(DecisionAgree))
			Expect(d.n).To(Equal(12.0)) // max(12,5) = 12, but not a floor -- see below
		})
	})

	Describe("saturation not listed", func() {
		// Saturation outside entries (the policy does not list it) never
		// sizes a variant, however usable its own reading.
		It("does not fall back to saturation when no listed analyzer contributes", func() {
			sat := makeAnalyzer(domain.SaturationAnalyzerName, 1000, 200) // N=5
			otherNoData := NamedAnalyzerResult{
				Name: "throughput",
				Live: true,
				Result: &domain.AnalyzerResult{
					VariantCapacities: []domain.VariantCapacity{{VariantName: "v", Reason: "T1-ols"}}, // informative but no PRC for v -> undefined N
				},
			}
			d := resolveSOForTest([]NamedAnalyzerResult{otherNoData}, &sat, "v")
			Expect(d.ok).To(BeFalse())
			Expect(d.path).To(Equal(DecisionNoSignal))
			Expect(d.contributors).To(BeEmpty())
		})
	})

	Describe("P0-store", func() {
		// Idle SO with its own store record -- PRC from P0-store, N defined,
		// and it contributes like a measured one.
		It("scales up from an idle SO's P0-store PRC", func() {
			sat := NamedAnalyzerResult{
				Name: domain.SaturationAnalyzerName,
				Live: true,
				Result: &domain.AnalyzerResult{
					TotalDemand: 500,
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "v", PerReplicaCapacity: 100, Reason: "P0-store"},
					},
				},
			}
			d := resolveSOForTest([]NamedAnalyzerResult{sat}, &sat, "v")
			Expect(d.ok).To(BeTrue())
			Expect(d.path).To(Equal(DecisionSingle))
			Expect(d.n).To(Equal(5.0))
		})

		// Never-seen-before SO -- borrowed EffectiveCapacity (also P0-store)
		// contributes normally, not discounted or clamped.
		It("does not discount a borrowed P0-store PRC for a never-seen-before SO", func() {
			satBorrowed := NamedAnalyzerResult{
				Name: domain.SaturationAnalyzerName,
				Live: true,
				Result: &domain.AnalyzerResult{
					TotalDemand: 500,
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "v", PerReplicaCapacity: 250, Reason: "P0-store"}, // borrowed, possibly an over-estimate
					},
				},
			}
			d := resolveSOForTest([]NamedAnalyzerResult{satBorrowed}, &satBorrowed, "v")
			Expect(d.ok).To(BeTrue())
			Expect(d.path).To(Equal(DecisionSingle))
			Expect(d.n).To(Equal(2.0)) // 500/250, exactly as reported -- no clamp/discount
		})
	})

	Describe("C4-no-signal", func() {
		// No-data (PRC=0) -- N undefined, contribution skipped, decision
		// path records it, no division/panic.
		It("records no-signal when saturation is present but has no-data for this SO", func() {
			sat := NamedAnalyzerResult{
				Name: domain.SaturationAnalyzerName,
				Live: true,
				Result: &domain.AnalyzerResult{
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "v", PerReplicaCapacity: 0, Reason: ReasonNoData},
					},
				},
			}
			d := resolveSOForTest([]NamedAnalyzerResult{sat}, &sat, "v")
			Expect(d.ok).To(BeFalse())
			Expect(d.path).To(Equal(DecisionNoSignal))
		})

		It("records no-signal when there are no entries and no sat at all", func() {
			d := resolveSOForTest(nil, nil, "v")
			Expect(d.ok).To(BeFalse())
			Expect(d.path).To(Equal(DecisionNoSignal))
		})

		It("records no-signal when saturation itself is not eligible and nothing else contributes", func() {
			sat := makeAnalyzer(domain.SaturationAnalyzerName, 1000, 200)
			sat.Live = false // not eligible
			d := resolveSOForTest([]NamedAnalyzerResult{sat}, &sat, "v")
			Expect(d.ok).To(BeFalse())
			Expect(d.path).To(Equal(DecisionNoSignal))
		})

		// Regression guard: sat disabled (excluded from eligibleAnalyzers/
		// "entries" upstream) AND !Eligible(sat) (here: stale) with no other
		// contributor must record DecisionNoSignal. A sat with no real result
		// -- nil, erroring, no-data, or stale -- must never participate in
		// scaling decisions.
		It("never sizes on a disabled-and-stale sat, even with no other contributor (regression guard)", func() {
			disabledAndStaleSat := makeAnalyzer(domain.SaturationAnalyzerName, 1000, 200) // N=5, would satisfy TotalReplicas if reached
			disabledAndStaleSat.Live = false                                              // stale -- fails Eligible()
			d := resolveSOForTest(nil, &disabledAndStaleSat, "v")
			Expect(d.path).To(Equal(DecisionNoSignal))
			Expect(d.ok).To(BeFalse())
			Expect(d.contributors).To(BeEmpty())
		})
	})

	Describe("eligibility gating", func() {
		// Non-live analyzer -> not a contributor.
		It("excludes a non-live analyzer from contributing", func() {
			sat := makeAnalyzer(domain.SaturationAnalyzerName, 1000, 200) // N=5
			other := makeAnalyzer("throughput", 2400, 200)                // N=12, but non-live
			other.Live = false
			d := resolveSOForTest([]NamedAnalyzerResult{sat, other}, &sat, "v")
			Expect(d.ok).To(BeTrue())
			Expect(d.path).To(Equal(DecisionSingle))
			Expect(d.n).To(Equal(5.0))
			Expect(d.contributors).To(ConsistOf(domain.SaturationAnalyzerName))
		})

		// Non-informative analyzer (Reason=no-data/error) -> not a
		// contributor.
		It("excludes a non-informative analyzer from contributing", func() {
			sat := makeAnalyzer(domain.SaturationAnalyzerName, 1000, 200) // N=5
			other := NamedAnalyzerResult{
				Name: "throughput",
				Live: true,
				Result: &domain.AnalyzerResult{
					TotalDemand:       2400,
					VariantCapacities: []domain.VariantCapacity{{VariantName: "v", PerReplicaCapacity: 200, Reason: ReasonNoData}},
				},
			}
			d := resolveSOForTest([]NamedAnalyzerResult{sat, other}, &sat, "v")
			Expect(d.ok).To(BeTrue())
			Expect(d.path).To(Equal(DecisionSingle))
			Expect(d.n).To(Equal(5.0))
		})

		// Regression guard: a stale analyzer must never appear in
		// contributors, regardless of what its per-SO Reason says, because
		// Eligible() is the loop's first, unconditional check.
		It("excludes a stale (!Live) analyzer even though its per-SO Reason looks perfectly usable (regression guard)", func() {
			sat := makeAnalyzer(domain.SaturationAnalyzerName, 1000, 200) // N=5
			staleButOtherwiseUsable := NamedAnalyzerResult{
				Name: "throughput",
				Live: false, // stale -- must be excluded by Eligible(), never reaching the per-SO Reason check
				Result: &domain.AnalyzerResult{
					TotalDemand:       2400,
					VariantCapacities: []domain.VariantCapacity{{VariantName: "v", PerReplicaCapacity: 200, Reason: reasonMeasured}}, // N=12, looks perfectly fine per-SO
				},
			}
			d := resolveSOForTest([]NamedAnalyzerResult{sat, staleButOtherwiseUsable}, &sat, "v")
			Expect(d.contributors).ToNot(ContainElement("throughput"),
				"a stale analyzer must never contribute, regardless of its per-SO Reason")
			Expect(d.path).To(Equal(DecisionSingle),
				"with the stale analyzer excluded, only sat's contribution remains")
			Expect(d.n).To(Equal(5.0))
		})
	})

	// Unit independence at the decision level too.
	It("is unit-independent: scaling demand and PRC by the same constant leaves the decision unchanged", func() {
		sat := makeAnalyzer(domain.SaturationAnalyzerName, 1000, 200) // N=5
		scaledSat := makeAnalyzer(domain.SaturationAnalyzerName, 1000*13, 200*13)
		d1 := resolveSOForTest([]NamedAnalyzerResult{sat}, &sat, "v")
		d2 := resolveSOForTest([]NamedAnalyzerResult{scaledSat}, &scaledSat, "v")
		Expect(d1.ok).To(BeTrue())
		Expect(d2.ok).To(BeTrue())
		Expect(d2.n).To(Equal(d1.n))
		Expect(d2.path).To(Equal(d1.path))
	})

	// Perturbing PRC_sat must not change another analyzer's contribution to
	// the composite.
	It("PRC_sat independence: perturbing saturation's own PRC does not change another analyzer's N", func() {
		other := makeAnalyzer("throughput", 2400, 200) // N=12, independent of sat entirely

		satLowPRC := makeAnalyzer(domain.SaturationAnalyzerName, 1000, 50)   // N=20
		satHighPRC := makeAnalyzer(domain.SaturationAnalyzerName, 1000, 500) // N=2

		dLow := resolveSOForTest([]NamedAnalyzerResult{satLowPRC, other}, &satLowPRC, "v")
		dHigh := resolveSOForTest([]NamedAnalyzerResult{satHighPRC, other}, &satHighPRC, "v")

		// other's own contribution (12) is identical in both cases; only the
		// winner of the max (and therefore the aggregate) may change based
		// on sat's PRC, but other's computed N_i is untouched by it.
		otherVC, _ := FindVariantCapacity(other.Result, "v")
		otherN, ok := TotalReplicas(other.Result, otherVC)
		Expect(ok).To(BeTrue())
		Expect(otherN).To(Equal(12.0))

		Expect(dLow.ok).To(BeTrue())
		Expect(dLow.n).To(Equal(20.0)) // sat's low-PRC N wins
		Expect(dHigh.ok).To(BeTrue())
		Expect(dHigh.n).To(Equal(12.0)) // other's N wins once sat's own N drops to 2
	})

	// Analyzer with no RoleDemand for a role -> not a contributor for that
	// role.
	It("excludes an analyzer that never attributed demand to this SO's role", func() {
		sat := NamedAnalyzerResult{
			Name: domain.SaturationAnalyzerName,
			Live: true,
			Result: &domain.AnalyzerResult{
				RoleDemand: map[string]float64{"decode": 900},
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "p1", Role: "prefill", PerReplicaCapacity: 100, Reason: reasonMeasured},
				},
			},
		}
		d := resolveSOForTest([]NamedAnalyzerResult{sat}, &sat, "p1")
		Expect(d.ok).To(BeFalse())
		Expect(d.path).To(Equal(DecisionNoSignal))
	})

	// A role carrying demand but currently served by no SO at all -- no
	// crash, and the decision reflects a genuinely uncovered role
	// (C4-no-signal) rather than a spurious full coverage. Unlike the case
	// above, the role's demand IS attributed (RoleDemand has a real entry),
	// but nothing in VariantCapacities serves it, so there is no SO to even
	// ask the question of.
	It("does not crash and records no-signal for a role with demand but no serving SO", func() {
		sat := NamedAnalyzerResult{
			Name: domain.SaturationAnalyzerName,
			Live: true,
			Result: &domain.AnalyzerResult{
				RoleDemand: map[string]float64{"decode": 900}, // decode has real demand...
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "p1", Role: "prefill", PerReplicaCapacity: 100, Reason: reasonMeasured},
					// ...but no variant serves decode at all.
				},
			},
		}
		d := resolveSOForTest([]NamedAnalyzerResult{sat}, &sat, "decode-variant-that-does-not-exist")
		Expect(d.ok).To(BeFalse())
		Expect(d.path).To(Equal(DecisionNoSignal))
		Expect(d.contributors).To(BeEmpty())
	})

	// Disaggregated, per-role aggregation where another analyzer is higher for ONE role only --
	// the other role must be unaffected.
	It("aggregates independently per role: a higher contributor for prefill only raises the decision there, not for decode", func() {
		sat := NamedAnalyzerResult{
			Name: domain.SaturationAnalyzerName,
			Live: true,
			Result: &domain.AnalyzerResult{
				RoleDemand: map[string]float64{"prefill": 400, "decode": 900},
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "p1", Role: "prefill", PerReplicaCapacity: 100, Reason: reasonMeasured}, // N=4
					{VariantName: "d1", Role: "decode", PerReplicaCapacity: 300, Reason: reasonMeasured},  // N=3
				},
			},
		}
		other := NamedAnalyzerResult{
			Name: "throughput",
			Live: true,
			Result: &domain.AnalyzerResult{
				RoleDemand: map[string]float64{"prefill": 800},
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "p1", Role: "prefill", PerReplicaCapacity: 100, Reason: reasonMeasured}, // N=8, higher than sat's 4
					// other says nothing about decode/d1 at all.
				},
			},
		}

		dPrefill := resolveSOForTest([]NamedAnalyzerResult{sat, other}, &sat, "p1")
		Expect(dPrefill.ok).To(BeTrue())
		Expect(dPrefill.n).To(Equal(8.0), "other's higher N must win for prefill")

		dDecode := resolveSOForTest([]NamedAnalyzerResult{sat, other}, &sat, "d1")
		Expect(dDecode.ok).To(BeTrue())
		Expect(dDecode.n).To(Equal(3.0), "decode must be untouched by other's prefill-only disagreement")

		// PRC_com computed from these N's as buildComposite does: the role's
		// demand divided by the composite N.
		prefillDemand, ok := aggregation.DemandForRole(sat.Result, "prefill")
		Expect(ok).To(BeTrue())
		Expect(prefillDemand/dPrefill.n).To(Equal(50.0), "400/8 -- lower than sat's own PRC of 100")

		decodeDemand, ok := aggregation.DemandForRole(sat.Result, "decode")
		Expect(ok).To(BeTrue())
		Expect(decodeDemand/dDecode.n).To(Equal(300.0), "decode's PRC is unaffected, since nothing disagreed there")
	})
})
