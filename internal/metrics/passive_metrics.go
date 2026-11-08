package metrics

import (
	"fmt"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

var (
	passiveMode *prometheus.GaugeVec

	// passivePublished is each variant's labels the last PublishPassiveModes
	// set, keyed namespace/variant, so a variant that left loses its series.
	passivePublished = map[string]PassiveVariant{}
	passiveMu        sync.Mutex
)

// registerPassiveMetrics creates and registers llmsm_passive_mode. Called from
// InitMetrics, after controllerInstance is set.
func registerPassiveMetrics(registry prometheus.Registerer) error {
	labels := []string{constants.LabelNamespace, constants.LabelModelName, constants.LabelVariantName, constants.LabelMode}
	if controllerInstance != "" {
		labels = append(labels, constants.LabelControllerInstance)
	}
	passiveMode = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: constants.LLMSMPassiveMode,
		Help: "Passive mode: 1 for the mode a variant's trigger is in (active, hold, observe, observe-unsafe), " +
			"0 for the others. In hold and observe the controller changes none of the variant's replica counts.",
	}, labels)
	if err := registry.Register(passiveMode); err != nil {
		return fmt.Errorf("failed to register passive-mode metric %s: %w", constants.LLMSMPassiveMode, err)
	}
	return nil
}

// PassiveVariant is one variant's published passive mode.
type PassiveVariant struct {
	Namespace, ModelName, Variant, Mode string
}

func (v PassiveVariant) labels(mode string) prometheus.Labels {
	l := prometheus.Labels{
		constants.LabelNamespace:   v.Namespace,
		constants.LabelModelName:   v.ModelName,
		constants.LabelVariantName: v.Variant,
		constants.LabelMode:        mode,
	}
	if controllerInstance != "" {
		l[constants.LabelControllerInstance] = controllerInstance
	}
	return l
}

// PublishPassiveModes publishes every variant's passive mode -- 1 for its mode,
// 0 for the others -- and deletes the series of a variant no longer listed, or
// one whose model name changed.
func PublishPassiveModes(variants []PassiveVariant) {
	passiveMu.Lock()
	defer passiveMu.Unlock()
	if passiveMode == nil {
		return
	}
	next := make(map[string]PassiveVariant, len(variants))
	for _, v := range variants {
		key := v.Namespace + "/" + v.Variant
		next[key] = v
		for _, m := range constants.PassiveModes {
			val := 0.0
			if m == v.Mode {
				val = 1
			}
			passiveMode.With(v.labels(m)).Set(val)
		}
	}
	for key, old := range passivePublished {
		if cur, ok := next[key]; ok && cur.ModelName == old.ModelName {
			continue
		}
		for _, m := range constants.PassiveModes {
			passiveMode.Delete(old.labels(m))
		}
	}
	passivePublished = next
}
