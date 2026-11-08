package fixtures

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// CreatePriorityClass creates PriorityClass name at value, with the default
// preemption policy (PreemptLowerPriority), and returns a function that
// deletes it. One left over from an earlier run is replaced.
func CreatePriorityClass(ctx context.Context, k8sClient kubernetes.Interface, name string, value int32) (func(context.Context) error, error) {
	pcs := k8sClient.SchedulingV1().PriorityClasses()
	if err := pcs.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
		return nil, fmt.Errorf("delete leftover PriorityClass %s: %w", name, err)
	}
	pc := &schedulingv1.PriorityClass{
		ObjectMeta:  metav1.ObjectMeta{Name: name},
		Value:       value,
		Description: "e2e: a whole-node model that preempts what takes its freed node",
	}
	if _, err := pcs.Create(ctx, pc, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("create PriorityClass %s: %w", name, err)
	}
	return func(ctx context.Context) error {
		if err := pcs.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
			return err
		}
		return nil
	}, nil
}

// WithPriorityClass runs the model's pods at PriorityClass name.
func WithPriorityClass(name string) ModelServiceOption {
	return func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.PriorityClassName = name
	}
}
