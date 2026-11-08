package gpunodes

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

// Review WN1: a LeaderWorkerSet group's pod on a node marks the node, GPUs or
// not, so node fit lands no exclusive-topology group in its domain.
func TestDiscoverUsageWithNodesMarksLeaderWorkerSetGroups(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	node := func(name string) *corev1.Node {
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"amd.com/gpu.product-name": "AMD-MI300X-192G"}},
			Status:     corev1.NodeStatus{Allocatable: corev1.ResourceList{"amd.com/gpu": resource.MustParse("8")}},
		}
	}
	pod := func(name, nodeName, gpus string, labels map[string]string) *corev1.Pod {
		req := corev1.ResourceList{}
		if gpus != "" {
			req["amd.com/gpu"] = resource.MustParse(gpus)
		}
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "x", Labels: labels},
			Spec: corev1.PodSpec{NodeName: nodeName, Containers: []corev1.Container{{
				Name: "c", Resources: corev1.ResourceRequirements{Requests: req}}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
	}
	group := map[string]string{lwsv1.GroupUniqueHashLabelKey: "abc"}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(
		node("n1"), node("n2"), node("n3"),
		pod("leader", "n1", "8", group),
		pod("sidecar", "n2", "", group),
		pod("plain", "n3", "8", nil),
	).Build()

	_, _, nodes, err := NewK8sWithGpuOperator(c).DiscoverUsageWithNodes(context.Background())
	require.NoError(t, err)
	assert.True(t, nodes["n1"].LWSGroup, "a group's GPU pod marks its node")
	assert.True(t, nodes["n2"].LWSGroup, "a group's pod without GPUs marks its node too")
	assert.False(t, nodes["n3"].LWSGroup, "a pod of no group marks nothing")
	assert.Equal(t, []string{"x/abc"}, nodes["n1"].LWSGroups, "the node names its group, by namespace and hash")
	assert.Empty(t, nodes["n3"].LWSGroups)
}
