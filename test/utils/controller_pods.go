package utils

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

// ControllerDeploymentName is the scaling manager controller Deployment every e2e install
// creates (config/base's controller-manager with the scaling-manager- prefix). Every
// overlay uses that prefix and no suffix, so the name does not vary.
const ControllerDeploymentName = "scaling-manager-controller-manager"

// ControllerServiceAccountName is the controller's ServiceAccount:
// config/base/manager's controller-manager with the same scaling-manager- prefix. It
// equals the Deployment's name because both come from the same base name,
// not because one is the other.
const ControllerServiceAccountName = "scaling-manager-controller-manager"

// ControllerContainerName is the controller Pod's container that runs the
// manager binary (config/base/manager/deployment.yaml).
const ControllerContainerName = "manager"

// controllerLabelSelector narrows the list before ownership is checked. It
// does not identify the controller on its own: see IsControllerPod.
const controllerLabelSelector = "control-plane=controller-manager"

// IsControllerPod reports whether pod was created by the named controller
// Deployment.
//
// Labels cannot answer this. The e2e relay Pods (fixtures.CreateHTTPDriver)
// wear the controller's labels on purpose, control-plane=controller-manager
// included, so the pool's NetworkPolicy admits them, and they can run in the
// controller's namespace. Warm-pool Pods do not carry control-plane; they share
// only app.kubernetes.io/name, which is the selector DumpControllerLogs and the
// pod-scraping helper used before. Ownership does answer it:
// scaletarget.OwnedByDeployment, the same check the engine uses.
func IsControllerPod(pod *corev1.Pod, deployment string) bool {
	return scaletarget.OwnedByDeployment(pod, deployment)
}

// ControllerPods lists the Pods of the named controller Deployment in
// namespace, and no other Pod that happens to share its labels.
//
// An empty result is not an error: during a rollout, or before the install,
// there may be no Pod. A caller that needs one must check the length.
func ControllerPods(ctx context.Context, k8s kubernetes.Interface, namespace, deployment string) ([]corev1.Pod, error) {
	list, err := k8s.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: controllerLabelSelector,
	})
	if err != nil {
		return nil, fmt.Errorf("list controller pods in %s: %w", namespace, err)
	}
	var pods []corev1.Pod
	for i := range list.Items {
		if IsControllerPod(&list.Items[i], deployment) {
			pods = append(pods, list.Items[i])
		}
	}
	return pods, nil
}
