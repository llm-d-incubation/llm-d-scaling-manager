package steadystate

import (
	"github.com/prometheus/client_golang/prometheus"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
)

// The optimizeV2 loop publishes ScalingBlockedNoCompositeSignal through the
// same llmsm_model_scaling_blocked metric the scale-to-zero policy/wake reasons
// already use, via metrics.SetModelScalingBlockedReasons with the
// ScalingBlockedReasonsSignal ownership set. These specs pin the metric
// plumbing directly — the same approach engine_scaling_blocked_wiring_test.go's
// "leaves the wake reason alone" case uses to test one producer without
// invoking the others; the full per-model collection loop is covered by the
// composite's end-to-end tests in composite_test.go.
var _ = Describe("no-composite-signal blocked-reason plumbing", func() {
	const (
		modelID   = "signal-wired-model"
		namespace = "signal-wired-ns"
	)

	registryFor := func() *prometheus.Registry {
		registry := prometheus.NewRegistry()
		Expect(metrics.InitMetrics(registry)).To(Succeed())
		return registry
	}

	publishedReasons := func(registry *prometheus.Registry) []string {
		mfs, err := registry.Gather()
		Expect(err).NotTo(HaveOccurred())
		var out []string
		for _, mf := range mfs {
			if mf.GetName() != constants.LLMSMModelScalingBlocked {
				continue
			}
			for _, m := range mf.GetMetric() {
				for _, l := range m.GetLabel() {
					if l.GetName() == constants.LabelReason {
						out = append(out, l.GetValue())
					}
				}
			}
		}
		return out
	}

	It("reports no-composite-signal when the composite carried no usable signal", func() {
		registry := registryFor()
		metrics.SetModelScalingBlockedReasons(namespace, modelID,
			constants.ScalingBlockedReasonsSignal,
			[]string{constants.ScalingBlockedNoCompositeSignal})

		Expect(publishedReasons(registry)).To(ConsistOf(constants.ScalingBlockedNoCompositeSignal))
	})

	It("clears no-composite-signal once a usable signal returns", func() {
		registry := registryFor()
		metrics.SetModelScalingBlockedReasons(namespace, modelID,
			constants.ScalingBlockedReasonsSignal,
			[]string{constants.ScalingBlockedNoCompositeSignal})
		Expect(publishedReasons(registry)).NotTo(BeEmpty())

		metrics.SetModelScalingBlockedReasons(namespace, modelID,
			constants.ScalingBlockedReasonsSignal,
			nil) // healthy: no active reasons this cycle

		Expect(publishedReasons(registry)).To(BeEmpty())
	})

	// The signal producer owns only its one reason. If it cleared everything,
	// it would erase the steady-state enforcer's and the wake loop's verdicts
	// every cycle it ran (mirrors "leaves the wake reason alone" above it).
	It("leaves the policy and wake reasons alone", func() {
		registry := registryFor()
		metrics.SetModelScalingBlockedReasons(namespace, modelID,
			constants.ScalingBlockedReasonsPolicy,
			[]string{constants.ScalingBlockedVariantFloor})
		metrics.SetModelScalingBlockedReasons(namespace, modelID,
			constants.ScalingBlockedReasonsWake,
			[]string{constants.ScalingBlockedNoWakeSignal})

		metrics.SetModelScalingBlockedReasons(namespace, modelID,
			constants.ScalingBlockedReasonsSignal,
			nil) // this cycle's signal is healthy

		Expect(publishedReasons(registry)).To(ConsistOf(
			constants.ScalingBlockedVariantFloor, constants.ScalingBlockedNoWakeSignal))
	})
})
