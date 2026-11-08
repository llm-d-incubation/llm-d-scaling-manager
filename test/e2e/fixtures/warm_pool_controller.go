package fixtures

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/test/utils"
)

// ControllerDeployment is the scaling manager controller, wherever the suite installed it.
type ControllerDeployment struct {
	Namespace string
	Name      string
}

// LLMSMController is the controller Deployment the e2e install creates, in
// namespace. Its name is utils.ControllerDeploymentName, so the specs and the
// log helpers in test/utils name the same Deployment.
func LLMSMController(namespace string) ControllerDeployment {
	return ControllerDeployment{Namespace: namespace, Name: utils.ControllerDeploymentName}
}

// EnableWarmPool used to restart the controller with --warm-pool-namespace, and
// no longer does anything. It is kept so the specs read the same and so this
// explanation has somewhere to live.
//
// The pool is always on. A namespace-scoped controller derives the pool's
// namespace from the one it watches; a cluster-scoped one discovers pools
// wherever a ScaledObject trigger declares them. The flag could only repeat
// that or contradict it, and nothing but this fixture ever set it.
//
// Restarting the controller was never the point, and it cost more than it gave:
// it forced every spec that used it to be Ordered and Serial, and because the
// patch was a no-op when the arguments were already right, the restart happened
// only SOMETIMES -- which is how a spec came to read a startup line that had
// aged out of its window on the runs where nothing restarted.
//
// A spec that wants the pool to act on its namespace declares a pool there, the
// way an operator would: a ScaledObject whose trigger carries warmPoolName.
// That is the opt-in, and it is the same one in production.
func EnableWarmPool(_ context.Context, _ *kubernetes.Clientset, _ ControllerDeployment, _ string) (func(context.Context) error, error) {
	return func(context.Context) error { return nil }, nil
}

// WaitForControllerReady waits until the controller has exactly one ready Pod
// on the current generation.
//
// The generation check is what makes this a wait for the NEW controller rather
// than the old one: immediately after an Update the previous Pod is still ready,
// and a naive readiness check returns at once and reads the old process's log.
// That is not hypothetical -- it produced a "the pool started fine" reading from
// a Pod that predated the change it was meant to prove.
func WaitForControllerReady(ctx context.Context, clientset *kubernetes.Clientset, c ControllerDeployment) error {
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		d, err := clientset.AppsV1().Deployments(c.Namespace).Get(ctx, c.Name, metav1.GetOptions{})
		if err == nil &&
			d.Status.ObservedGeneration >= d.Generation &&
			d.Status.UpdatedReplicas == *d.Spec.Replicas &&
			d.Status.ReadyReplicas == *d.Spec.Replicas &&
			d.Status.UnavailableReplicas == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("controller %s/%s did not become ready", c.Namespace, c.Name)
}

// NewestControllerPod is the controller Pod that started last.
//
// Reading "the" controller Pod by label picks an arbitrary one, and during a
// rollout that is as likely to be the outgoing process as the incoming one. Its
// log then shows the behaviour of the configuration the test just replaced.
//
// Only Pods of c's Deployment count (utils.ControllerPods): a relay wearing
// the controller's labels is not the controller, however recently it started.
func NewestControllerPod(ctx context.Context, clientset kubernetes.Interface, c ControllerDeployment) (*corev1.Pod, error) {
	pods, err := utils.ControllerPods(ctx, clientset, c.Namespace, c.Name)
	if err != nil {
		return nil, err
	}
	var newest *corev1.Pod
	for i := range pods {
		p := &pods[i]
		if p.Status.Phase != corev1.PodRunning || p.DeletionTimestamp != nil {
			continue
		}
		if newest == nil || p.CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = p
		}
	}
	if newest == nil {
		return nil, fmt.Errorf("no running controller pod in %s", c.Namespace)
	}
	return newest, nil
}

// LabelNodeAccelerator makes a node report one GPU product, and returns a
// restore.
//
// Sets EVERY vendor's product label rather than NVIDIA's alone. The controller
// resolves a node's accelerator by walking the vendor list in reverse and taking
// the first match, so on a node that already carries another vendor's label the
// NVIDIA one never wins -- a kind worker labelled amd.com/gpu.product-name
// reported AMD-MI300X-192G no matter what this wrote, and the spec asserting on
// its own value failed for a reason that had nothing to do with the pool.
//
// Writing them all means the expected value wins whatever the precedence is,
// which keeps the test about the property it names: the accelerator the pool
// reports is the one its Pods' NODE declares.
func LabelNodeAccelerator(ctx context.Context, clientset kubernetes.Interface, node, product string) (func(context.Context) error, error) {
	before, err := clientset.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("read node %s: %w", node, err)
	}
	original := map[string]*string{}
	for _, key := range acceleratorProductLabels() {
		if v, had := before.Labels[key]; had {
			v := v
			original[key] = &v
		} else {
			original[key] = nil
		}
		if err := patchNodeLabel(ctx, clientset, node, key, &product); err != nil {
			return nil, err
		}
	}
	return func(ctx context.Context) error {
		var firstErr error
		for key, value := range original {
			if err := patchNodeLabel(ctx, clientset, node, key, value); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	}, nil
}

// acceleratorProductLabels is every vendor key that names a GPU model, read from
// the same constants the controller resolves against so the two cannot drift.
func acceleratorProductLabels() []string {
	keys := make([]string, 0, len(constants.VendorResources))
	for _, v := range constants.VendorResources {
		keys = append(keys, v.ProductLabel)
	}
	return keys
}

// patchNodeLabel sets one label on a node, or removes it when value is nil.
//
// The read-modify-write is retried on a Conflict, re-reading the node each
// time. A Node is written constantly -- the kubelet heartbeats its status, the
// node controllers patch conditions -- so an Update built from a read a moment
// old loses to one of them often enough to fail a BeforeAll in CI.
func patchNodeLabel(ctx context.Context, clientset kubernetes.Interface, node, key string, value *string) error {
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		n, err := clientset.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("read node %s: %w", node, err)
		}
		if n.Labels == nil {
			n.Labels = map[string]string{}
		}
		if value == nil {
			delete(n.Labels, key)
		} else {
			n.Labels[key] = *value
		}
		_, err = clientset.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("label node %s: %w", node, err)
	}
	return nil
}

// SchedulableNodes returns worker nodes, newest name order, for a test that
// needs somewhere specific to put a Pod.
func SchedulableNodes(ctx context.Context, clientset *kubernetes.Clientset) ([]corev1.Node, error) {
	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	var out []corev1.Node
	for _, n := range nodes.Items {
		if _, isControlPlane := n.Labels["node-role.kubernetes.io/control-plane"]; isControlPlane {
			continue
		}
		if n.Spec.Unschedulable {
			continue
		}
		out = append(out, n)
	}
	return out, nil
}
