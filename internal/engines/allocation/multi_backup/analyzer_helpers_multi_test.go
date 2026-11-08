//go:build ignore

package allocation

// Multi-entry test cases for the paired helpers.
//
// These tests exercised the N>1 cross-analyzer aggregation paths that were
// removed from the optimizer-side helpers as part of the CT3b simplification.
// The helpers themselves (original form) are preserved in analyzer_helpers_multi.go.
//
// These tests MUST be re-introduced (adapted as needed) when the multi-entry
// logic is moved to the engine side (the planned engine-side reduce step).
// Do not delete this file.

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

var _ = Describe("paired helpers — multi-entry (engine-side, pending restore)", func() {

	Describe("roleBottleneckReplicas", func() {
		It("returns max cross-analyzer ceil(roleRemaining/PRC) across two entries", func() {
			// analyzer0: prefill remaining=10000, PRC=5000 → ceil(10000/5000)=2
			// analyzer1: prefill remaining=15000, PRC=5000 → ceil(15000/5000)=3 (max)
			s := []NamedAnalyzerResult{
				makeNamedPD("sat", 10000, 20000, 0, 0, 10000, 20000, 5000, 8000),
				makeNamedPD("ta", 15000, 15000, 0, 0, 15000, 15000, 5000, 8000),
			}
			_, ps := initRoleState(s)
			Expect(roleBottleneckReplicas(s, ps, "prefill", "pf")).To(Equal(3))
			// decode: max(ceil(20000/8000)=3, ceil(15000/8000)=2) = 3
			Expect(roleBottleneckReplicas(s, ps, "decode", "dc")).To(Equal(3))
		})
	})

	Describe("needsScaleDownForRole", func() {
		It("never-analyzed analyzer does not veto: a non-live analyzer with no spare is skipped", func() {
			live := makeNamedPD("sat", 0, 0, 20000, 30000, 10000, 30000, 10000, 10000)
			neverAnalyzed := makeNamedPD("throughput", 0, 0, 0, 0, 0, 0, 10000, 10000)
			neverAnalyzed.Live = false
			neverAnalyzed.RoleSpare = nil
			s := []NamedAnalyzerResult{live, neverAnalyzed}
			Expect(needsScaleDownForRole(s, "prefill")).To(BeTrue())
			Expect(needsScaleDownForRole(s, "decode")).To(BeTrue())
		})

		It("stale analyzer does not veto: a non-live analyzer with zero spare is skipped", func() {
			// Staleness itself is computed at the engine level (see engine_v2_liveness_test.go);
			// here Live=false stands in for "last good analysis is older than the threshold".
			live := makeNamedPD("sat", 0, 0, 20000, 30000, 10000, 30000, 10000, 10000)
			stale := makeNamedPD("throughput", 0, 0, 0, 0, 0, 0, 10000, 10000)
			stale.Live = false
			s := []NamedAnalyzerResult{live, stale}
			Expect(needsScaleDownForRole(s, "prefill")).To(BeTrue())
			Expect(needsScaleDownForRole(s, "decode")).To(BeTrue())
		})

		It("safety floor: returns false when no live analyzer remains", func() {
			a := makeNamedPD("sat", 0, 0, 20000, 30000, 10000, 30000, 10000, 10000)
			a.Live = false
			b := makeNamedPD("throughput", 0, 0, 20000, 30000, 10000, 30000, 10000, 10000)
			b.Live = false
			s := []NamedAnalyzerResult{a, b}
			Expect(needsScaleDownForRole(s, "prefill")).To(BeFalse())
			Expect(needsScaleDownForRole(s, "decode")).To(BeFalse())
		})

		It("applies uniformly to saturation: a non-live saturation result does not veto", func() {
			satNonLive := makeNamedPD(domain.SaturationAnalyzerName, 0, 0, 0, 0, 0, 0, 10000, 10000)
			satNonLive.Live = false
			live := makeNamedPD("throughput", 0, 0, 20000, 30000, 10000, 30000, 10000, 10000)
			s := []NamedAnalyzerResult{satNonLive, live}
			Expect(needsScaleDownForRole(s, "prefill")).To(BeTrue())
			Expect(needsScaleDownForRole(s, "decode")).To(BeTrue())
		})
	})

	Describe("safeRemovalReplicasForRole", func() {
		It("skips a non-live analyzer instead of letting its tiny spare drag the min to 0", func() {
			live := makeNamedPD("sat", 0, 0, 20000, 30000, 10000, 30000, 10000, 10000) // floor(20000/10000)=2
			nonLive := makeNamedPD("throughput", 0, 0, 5000, 5000, 10000, 30000, 10000, 10000)
			nonLive.Live = false // would compute floor(5000/10000)=0 if counted
			s := []NamedAnalyzerResult{live, nonLive}
			Expect(safeRemovalReplicasForRole(s, "pf", "prefill")).To(Equal(2))
		})
	})
})
