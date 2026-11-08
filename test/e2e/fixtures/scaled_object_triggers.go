package fixtures

import (
	"encoding/json"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/utils/ptr"
)

// Carrier keys for the deferred trigger edits below. Like the pending
// metadata keys, they never reach the cluster: applyPendingTriggers removes
// them.
const (
	pendingForeignTriggersKey = "e2e.llm-d.ai/pending-foreign-triggers"
	pendingOwnTriggerNameKey  = "e2e.llm-d.ai/pending-own-trigger-name"
)

// ownTriggerName is the name every scaling manager trigger option gives the trigger.
const ownTriggerName = "scaling-manager-external-scaler"

// WithForeignTrigger adds a trigger that is not the scaling manager's beside the scaling manager's own: a
// kubernetes-workload or prometheus trigger KEDA evaluates on its own.
//
// Deferred, like WithScalingManagerTriggerMetadata: the scaling manager trigger options replace
// spec.triggers wholesale, and the trigger added here must not receive the scaling manager's
// trigger metadata, which would make it read as the scaling manager's.
func WithForeignTrigger(t kedav1alpha1.ScaleTriggers) ScaledObjectOption {
	return func(so *kedav1alpha1.ScaledObject) {
		if so.Annotations == nil {
			so.Annotations = make(map[string]string)
		}
		pending := make([]kedav1alpha1.ScaleTriggers, 0, 1)
		if raw, ok := so.Annotations[pendingForeignTriggersKey]; ok {
			_ = json.Unmarshal([]byte(raw), &pending)
		}
		pending = append(pending, t)
		raw, err := json.Marshal(pending)
		if err != nil {
			return
		}
		so.Annotations[pendingForeignTriggersKey] = string(raw)
	}
}

// WithOwnTriggerName renames the scaling manager's trigger. A scalingModifiers formula names
// triggers as identifiers, which the default name, holding '-', cannot be.
func WithOwnTriggerName(name string) ScaledObjectOption {
	return func(so *kedav1alpha1.ScaledObject) {
		if so.Annotations == nil {
			so.Annotations = make(map[string]string)
		}
		so.Annotations[pendingOwnTriggerNameKey] = name
	}
}

// WithScalingModifiers sets spec.advanced.scalingModifiers: KEDA composes the
// named triggers through formula and scales on the result against target,
// instead of taking the highest trigger.
func WithScalingModifiers(formula, target string) ScaledObjectOption {
	return func(so *kedav1alpha1.ScaledObject) {
		if so.Spec.Advanced == nil {
			so.Spec.Advanced = &kedav1alpha1.AdvancedConfig{}
		}
		so.Spec.Advanced.ScalingModifiers = kedav1alpha1.ScalingModifiers{
			Formula:    formula,
			Target:     target,
			MetricType: autoscalingv2.AverageValueMetricType,
		}
	}
}

// WithScaleUpPodsPerPeriod limits the HPA's scale-up to pods every
// periodSeconds, with no stabilization: a slow, stepwise scale-up a spec can
// interrupt between two steps.
func WithScaleUpPodsPerPeriod(pods, periodSeconds int32) ScaledObjectOption {
	return func(so *kedav1alpha1.ScaledObject) {
		if so.Spec.Advanced == nil {
			so.Spec.Advanced = &kedav1alpha1.AdvancedConfig{}
		}
		if so.Spec.Advanced.HorizontalPodAutoscalerConfig == nil {
			so.Spec.Advanced.HorizontalPodAutoscalerConfig = &kedav1alpha1.HorizontalPodAutoscalerConfig{}
		}
		hpa := so.Spec.Advanced.HorizontalPodAutoscalerConfig
		if hpa.Behavior == nil {
			hpa.Behavior = &autoscalingv2.HorizontalPodAutoscalerBehavior{}
		}
		hpa.Behavior.ScaleUp = &autoscalingv2.HPAScalingRules{
			StabilizationWindowSeconds: ptr.To(int32(0)),
			Policies: []autoscalingv2.HPAScalingPolicy{{
				Type:          autoscalingv2.PodsScalingPolicy,
				Value:         pods,
				PeriodSeconds: periodSeconds,
			}},
		}
	}
}

// applyPendingTriggers applies WithOwnTriggerName and WithForeignTrigger to
// the triggers the other options left, and removes their carrier keys.
func applyPendingTriggers(so *kedav1alpha1.ScaledObject) {
	if name, ok := so.Annotations[pendingOwnTriggerNameKey]; ok {
		delete(so.Annotations, pendingOwnTriggerNameKey)
		for i := range so.Spec.Triggers {
			if so.Spec.Triggers[i].Name == ownTriggerName {
				so.Spec.Triggers[i].Name = name
			}
		}
	}
	if raw, ok := so.Annotations[pendingForeignTriggersKey]; ok {
		delete(so.Annotations, pendingForeignTriggersKey)
		var pending []kedav1alpha1.ScaleTriggers
		if err := json.Unmarshal([]byte(raw), &pending); err == nil {
			so.Spec.Triggers = append(so.Spec.Triggers, pending...)
		}
	}
}
