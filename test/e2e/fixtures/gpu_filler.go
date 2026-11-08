package fixtures

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/resources"
)

// fillerImage holds GPUs and does nothing. Never docker.io.
const fillerImage = "registry.k8s.io/pause:3.9"

// FreeGPUsOnNode is a node's allocatable GPUs of resourceName less what the
// pods scheduled to it request, as the scheduler accounts them.
func FreeGPUsOnNode(ctx context.Context, k8sClient kubernetes.Interface, node *corev1.Node, resourceName string) (int, error) {
	alloc, ok := node.Status.Allocatable[corev1.ResourceName(resourceName)]
	if !ok {
		return 0, nil
	}
	pods, err := k8sClient.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + node.Name})
	if err != nil {
		return 0, fmt.Errorf("list pods on %s: %w", node.Name, err)
	}
	used := 0
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		used += resources.PodGPURequests(p)
	}
	return int(alloc.Value()) - used, nil
}

// FillGPUNodes takes every GPU still free on the nodes labelled key=product
// with a pause pod per node, pinned by node name, in namespace. It returns the
// pods' names and how many GPUs they took. The pods are not the scaling manager's: no limiter
// or optimizer counts them as a model's, but the node picture does, which is
// what a spec needing full nodes wants.
func FillGPUNodes(ctx context.Context, k8sClient *kubernetes.Clientset, namespace, key, product, resourceName string) ([]string, int, error) {
	nodes, err := k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: key + "=" + product})
	if err != nil {
		return nil, 0, fmt.Errorf("list %s nodes: %w", product, err)
	}
	var names []string
	total := 0
	for i := range nodes.Items {
		n := &nodes.Items[i]
		free, err := FreeGPUsOnNode(ctx, k8sClient, n, resourceName)
		if err != nil {
			return names, total, err
		}
		if free <= 0 {
			continue
		}
		name, err := FillNodeGPUs(ctx, k8sClient, namespace, n.Name, resourceName, free)
		if err != nil {
			return names, total, err
		}
		names = append(names, name)
		total += free
	}
	return names, total, nil
}

// FillNodeGPUs takes gpus GPUs of resourceName on node with one pause pod in
// namespace, bound to the node by name, and returns the pod's name. Bound by
// name, the pod skips the scheduler: it holds the GPUs from the moment the
// kubelet admits it, whatever else is waiting for them.
func FillNodeGPUs(ctx context.Context, k8sClient kubernetes.Interface, namespace, node, resourceName string, gpus int) (string, error) {
	pod := gpuPausePod(namespace, "gpu-filler-", "gpu-filler", resourceName, gpus)
	pod.Spec.NodeName = node
	created, err := k8sClient.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("create filler on %s: %w", node, err)
	}
	return created.Name, nil
}

// QueueGPUPod creates a pause pod in namespace asking for gpus GPUs of
// resourceName on node, through the scheduler: a node selector on the node's
// hostname, not a binding. While the node has no room the pod waits Pending,
// and the scheduler places it the moment enough GPUs come free there -- a
// foreign workload taking GPUs another model was released for. It returns the
// pod's name.
func QueueGPUPod(ctx context.Context, k8sClient kubernetes.Interface, namespace, node, resourceName string, gpus int) (string, error) {
	pod := gpuPausePod(namespace, "gpu-queued-", "gpu-queued", resourceName, gpus)
	pod.Spec.NodeSelector = map[string]string{corev1.LabelHostname: node}
	created, err := k8sClient.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("create queued GPU pod for %s: %w", node, err)
	}
	return created.Name, nil
}

// gpuPausePod is a pause pod labelled app=app asking for gpus GPUs of
// resourceName. It tolerates every taint: a product labelled only on the
// control-plane is still a node the picture counts.
func gpuPausePod(namespace, generateName, app, resourceName string, gpus int) *corev1.Pod {
	qty := *resource.NewQuantity(int64(gpus), resource.DecimalSI)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: generateName, Namespace: namespace,
			Labels: map[string]string{"app": app}},
		Spec: corev1.PodSpec{
			Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{{Name: "pause", Image: fillerImage,
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceName(resourceName): qty},
					Limits:   corev1.ResourceList{corev1.ResourceName(resourceName): qty},
				}}},
		},
	}
}

// TerminatingPodsOnNodes counts the pods still terminating on the nodes
// labelled key=product, by namespace; empty when there are none. The
// scheduler counts their GPUs as used until they are gone, so a fill taken
// while any remains leaves GPUs that come free later.
//
// A pod in a namespace being deleted counts too, before it has a deletion
// timestamp of its own: an earlier spec's cleanup deletes its namespace and
// returns at once, and the namespace controller reaches the pods only some
// seconds later. Counted by their own timestamp alone, they read as running
// in that gap, the fill leaves their GPUs to them, and they come free after.
// A finished pod there holds no GPU (FreeGPUsOnNode) and does not count.
func TerminatingPodsOnNodes(ctx context.Context, k8sClient kubernetes.Interface, key, product string) (map[string]int, error) {
	nodes, err := k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: key + "=" + product})
	if err != nil {
		return nil, fmt.Errorf("list %s nodes: %w", product, err)
	}
	namespaces, err := k8sClient.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	leaving := map[string]bool{}
	for i := range namespaces.Items {
		ns := &namespaces.Items[i]
		leaving[ns.Name] = ns.DeletionTimestamp != nil || ns.Status.Phase == corev1.NamespaceTerminating
	}
	out := map[string]int{}
	for i := range nodes.Items {
		pods, err := k8sClient.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + nodes.Items[i].Name})
		if err != nil {
			return nil, fmt.Errorf("list pods on %s: %w", nodes.Items[i].Name, err)
		}
		for j := range pods.Items {
			p := &pods.Items[j]
			finished := p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed
			if p.DeletionTimestamp != nil || (leaving[p.Namespace] && !finished) {
				out[p.Namespace]++
			}
		}
	}
	return out, nil
}
