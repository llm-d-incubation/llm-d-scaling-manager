package metrics

import (
	"fmt"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

var (
	kedaUnwiredHPAs      *prometheus.GaugeVec
	kedaHPARewires       *prometheus.CounterVec
	kedaHPAWiringUnread  *prometheus.GaugeVec
	kedaUnwiredPublished = map[string]bool{}
	kedaUnwiredMu        sync.Mutex
)

// registerKEDAHPARepairMetrics creates and registers the KEDA HPA repair's
// metrics. Called from InitMetrics, after controllerInstance is set.
func registerKEDAHPARepairMetrics(registry prometheus.Registerer) error {
	withInstance := func(labels ...string) []string {
		if controllerInstance != "" {
			labels = append(labels, constants.LabelControllerInstance)
		}
		return labels
	}
	kedaUnwiredHPAs = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: constants.LLMSMKEDAUnwiredHPAs,
		Help: "HPAs KEDA built for this scaler's ScaledObjects without the scaler's External metric, " +
			"as of the last read of the namespace (KEDA_HPA_REPAIR).",
	}, withInstance(constants.LabelNamespace))
	kedaHPARewires = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: constants.LLMSMKEDAHPARewiresTotal,
		Help: "Annotations the KEDA HPA repair put on an unwired HPA so KEDA derives it again, " +
			"by outcome: requested, forbidden or failed.",
	}, withInstance(constants.LabelNamespace, constants.LabelOutcome))
	kedaHPAWiringUnread = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: constants.LLMSMKEDAHPAWiringUnread,
		Help: "1 when the KEDA HPA repair's last pass could not read an HPA list or a ScaledObject it needed, 0 when it read them all.",
	}, withInstance())
	for _, c := range []prometheus.Collector{kedaUnwiredHPAs, kedaHPARewires, kedaHPAWiringUnread} {
		if err := registry.Register(c); err != nil {
			return fmt.Errorf("failed to register a KEDA HPA repair metric: %w", err)
		}
	}
	kedaUnwiredMu.Lock()
	kedaUnwiredPublished = map[string]bool{}
	kedaUnwiredMu.Unlock()
	return nil
}

func kedaLabels(l prometheus.Labels) prometheus.Labels {
	if controllerInstance != "" {
		l[constants.LabelControllerInstance] = controllerInstance
	}
	return l
}

// PublishKEDAUnwiredHPAs sets llmsm_keda_unwired_hpas for each namespace in
// counts. A namespace published before and absent from counts and from keep
// loses its series. keep names namespaces whose last read failed: they keep
// the value they had, because an unread namespace is unknown, not clean.
func PublishKEDAUnwiredHPAs(counts map[string]int, keep map[string]bool) {
	kedaUnwiredMu.Lock()
	defer kedaUnwiredMu.Unlock()
	if kedaUnwiredHPAs == nil {
		return
	}
	next := make(map[string]bool, len(counts))
	for ns, n := range counts {
		kedaUnwiredHPAs.With(kedaLabels(prometheus.Labels{constants.LabelNamespace: ns})).Set(float64(n))
		next[ns] = true
	}
	for ns := range kedaUnwiredPublished {
		if next[ns] {
			continue
		}
		if keep[ns] {
			next[ns] = true
			continue
		}
		kedaUnwiredHPAs.Delete(kedaLabels(prometheus.Labels{constants.LabelNamespace: ns}))
	}
	kedaUnwiredPublished = next
}

// CountKEDAHPARewire counts one annotation the repair attempted on an HPA in
// namespace, with its outcome (constants.HPARewire*).
func CountKEDAHPARewire(namespace, outcome string) {
	if kedaHPARewires == nil {
		return
	}
	kedaHPARewires.With(kedaLabels(prometheus.Labels{
		constants.LabelNamespace: namespace,
		constants.LabelOutcome:   outcome,
	})).Inc()
}

// SetKEDAHPAWiringUnread sets llmsm_keda_hpa_wiring_unread.
func SetKEDAHPAWiringUnread(unread bool) {
	if kedaHPAWiringUnread == nil {
		return
	}
	v := 0.0
	if unread {
		v = 1
	}
	kedaHPAWiringUnread.With(kedaLabels(prometheus.Labels{})).Set(v)
}
