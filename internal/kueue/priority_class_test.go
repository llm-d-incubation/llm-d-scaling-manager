package kueue

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// K1: a model's priority is looked up by the class its pod template names,
// spelled as the class its Workloads carry, under either Workload version.
func TestPriorityClassSpelledAlikeOnTemplateAndWorkload(t *testing.T) {
	tmpl := func(label, pcn string) *corev1.PodTemplateSpec {
		t := &corev1.PodTemplateSpec{Spec: corev1.PodSpec{PriorityClassName: pcn}}
		if label != "" {
			t.ObjectMeta = metav1.ObjectMeta{Labels: map[string]string{PriorityClassLabel: label}}
		}
		return t
	}
	wl := func(spec map[string]interface{}) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]interface{}{"spec": spec}}
	}
	cases := []struct {
		name          string
		template, wls string
	}{
		{"WorkloadPriorityClass, v1beta2 ref", TemplatePriorityClass(tmpl("serving", "ignored")),
			workloadClass(wl(map[string]interface{}{"priorityClassRef": map[string]interface{}{"group": "kueue.x-k8s.io", "kind": "WorkloadPriorityClass", "name": "serving"}}))},
		{"WorkloadPriorityClass, v1beta1 source", TemplatePriorityClass(tmpl("serving", "")),
			workloadClass(wl(map[string]interface{}{"priorityClassName": "serving", "priorityClassSource": WorkloadPriorityClassSource}))},
		{"pod PriorityClass, v1beta1 source", TemplatePriorityClass(tmpl("", "high")),
			workloadClass(wl(map[string]interface{}{"priorityClassName": "high", "priorityClassSource": "scheduling.k8s.io/priorityclass"}))},
		{"pod PriorityClass, v1beta2 ref", TemplatePriorityClass(tmpl("", "high")),
			workloadClass(wl(map[string]interface{}{"priorityClassRef": map[string]interface{}{"group": "scheduling.k8s.io", "kind": "PriorityClass", "name": "high"}}))},
		{"none", TemplatePriorityClass(tmpl("", "")), workloadClass(wl(map[string]interface{}{}))},
	}
	for _, c := range cases {
		if c.template != c.wls || c.template == "" {
			t.Errorf("%s: template reads %q, Workload %q; want one non-empty spelling", c.name, c.template, c.wls)
		}
	}
	if got := TemplatePriorityClass(nil); got != "" {
		t.Errorf("no template reads %q, want empty: not known", got)
	}
	if TemplatePriorityClass(tmpl("x", "")) == TemplatePriorityClass(tmpl("", "x")) {
		t.Error("a WorkloadPriorityClass and a pod PriorityClass of one name are two classes")
	}
}
