package utils

import (
	"context"
	"fmt"
	"io"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

// ControllerLogsContain fetches logs from every Pod of the named controller
// Deployment in namespace (concatenated with pod separators) and reports
// whether substr appears in the combined text. sinceSeconds limits how far back
// log lines are retrieved per pod (Kubernetes PodLogOptions.SinceSeconds).
//
// Pods are chosen by ControllerPods, not by label: a relay or warm-pool Pod
// wearing the controller's labels would otherwise have its log read as the
// controller's.
func ControllerLogsContain(
	ctx context.Context,
	k8s kubernetes.Interface,
	namespace string,
	deployment string,
	substr string,
	sinceSeconds int64,
) (bool, string, error) {
	pods, err := ControllerPods(ctx, k8s, namespace, deployment)
	if err != nil {
		return false, "", err
	}
	if len(pods) == 0 {
		return false, "", fmt.Errorf("no pods of deployment %s in namespace %s", deployment, namespace)
	}
	var b strings.Builder
	since := sinceSeconds
	for _, pod := range pods {
		opts := &corev1.PodLogOptions{SinceSeconds: &since}
		stream, streamErr := k8s.CoreV1().Pods(namespace).GetLogs(pod.Name, opts).Stream(ctx)
		if streamErr != nil {
			return false, "", streamErr
		}
		raw, readErr := func() ([]byte, error) {
			defer func() { _ = stream.Close() }()
			return io.ReadAll(stream)
		}()
		if readErr != nil {
			return false, "", readErr
		}
		fmt.Fprintf(&b, "\n--- pod: %s ---\n", pod.Name)
		b.Write(raw)
	}
	combined := b.String()
	return strings.Contains(combined, substr), combined, nil
}
