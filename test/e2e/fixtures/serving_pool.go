package fixtures

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1 "k8s.io/api/core/v1"
)

// ServingPoolNameForEPP derives the InferencePool name from the EPP Service name,
// which the chart names "<pool>-epp".
func ServingPoolNameForEPP(eppServiceName string) string {
	return strings.TrimSuffix(eppServiceName, "-epp")
}

// PoolMember is a pod the InferencePool currently selects.
type PoolMember struct {
	Name  string
	Ready bool
	Age   time.Duration
	// OwningPool is the pod's llm-d.ai/model-pool label — the per-suite pool NAME
	// its fixture asked for. Every suite passes a distinct one, but they all land
	// in the same real InferencePool, so this is what identifies which suite left
	// a pod behind. Empty for workloads not built by these fixtures.
	OwningPool string
}

func (m PoolMember) String() string {
	owner := m.OwningPool
	if owner == "" {
		owner = "unknown owner"
	}
	return fmt.Sprintf("%s (age=%s, fixture pool %q)", m.Name, m.Age.Truncate(time.Second), owner)
}

// ReadyPoolMembers returns the pods that are READY endpoints of the named
// InferencePool — the pods the EPP may dispatch a request to.
//
// This is the condition scale-from-zero depends on and nothing asserts. The EPP
// queues a request (raising the flow-control gauge the scaling manager reads) only when it has no
// endpoint to send it to. With any ready endpoint present it dispatches instead,
// and a pod serving a DIFFERENT model answers "the model does not exist" in
// single-digit milliseconds — so the queue stays empty, the scaling manager correctly reports no
// demand, and the spec times out blaming the engine.
//
// That is not hypothetical: it is what a kind run reproduced. Two pods left in the
// pool by other suites — one from earlier in the same run, one three hours old from
// a previous run — turned every request from the trigger job into an instant 404,
// and the engine polled an empty queue for five minutes.
func ReadyPoolMembers(
	ctx context.Context,
	crClient client.Client,
	namespace, poolName string,
) ([]PoolMember, error) {
	selector, err := servingPoolSelector(ctx, crClient, namespace, poolName)
	if err != nil {
		return nil, err
	}
	if len(selector) == 0 {
		return nil, fmt.Errorf("InferencePool %s/%s selects nothing", namespace, poolName)
	}

	var pods corev1.PodList
	if err := crClient.List(ctx, &pods,
		client.InNamespace(namespace),
		client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(selector)},
	); err != nil {
		return nil, fmt.Errorf("list pods selected by InferencePool %s: %w", poolName, err)
	}

	var ready []PoolMember
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil {
			continue // already going away; it stops serving when it does
		}
		if !podReady(pod) {
			continue
		}
		ready = append(ready, PoolMember{
			Name:       pod.Name,
			Ready:      true,
			Age:        time.Since(pod.CreationTimestamp.Time),
			OwningPool: pod.Labels[owningPoolLabelKey],
		})
	}
	return ready, nil
}

// owningPoolLabelKey carries the per-suite pool name the fixture was asked for.
// Absent on workloads built elsewhere, which is why PoolMember tolerates "".
const owningPoolLabelKey = "llm-d.ai/model-pool"

func podReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// servingPoolSelector reads the pool's pod selector, tolerating both InferencePool
// API shapes: v1 nests it under spec.selector.matchLabels, v1alpha2 puts the map
// directly on spec.selector.
func servingPoolSelector(
	ctx context.Context,
	crClient client.Client,
	namespace, poolName string,
) (map[string]string, error) {
	apiVersions := []string{
		"inference.networking.k8s.io/v1",
		"inference.networking.x-k8s.io/v1alpha2",
	}

	var lastErr error
	for _, apiVersion := range apiVersions {
		pool := &unstructured.Unstructured{}
		pool.SetAPIVersion(apiVersion)
		pool.SetKind("InferencePool")
		err := crClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: poolName}, pool)
		if err != nil {
			if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
				lastErr = err
				continue
			}
			return nil, fmt.Errorf("get InferencePool %s/%s: %w", namespace, poolName, err)
		}

		if nested, found, nErr := unstructured.NestedStringMap(pool.Object, "spec", "selector", "matchLabels"); nErr == nil && found {
			return nested, nil
		}
		if flat, found, fErr := unstructured.NestedStringMap(pool.Object, "spec", "selector"); fErr == nil && found {
			return flat, nil
		}
		return nil, fmt.Errorf("InferencePool %s/%s has no readable pod selector", namespace, poolName)
	}
	return nil, fmt.Errorf("InferencePool %s/%s not found in any supported API group: %w", namespace, poolName, lastErr)
}
