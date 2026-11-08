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
)

// Review A3: across cycles, a Pending pod the scheduler has nominated a node
// for (it preempted pods there and waits for them to leave) holds that node's
// hole; counting it free let the next cycle order another replica into it.
// Whole 8-GPU units: two 8-GPU nodes, one running an 8-GPU pod, the other
// nominated for a Pending 8-GPU pod -- nothing is free.
func TestANominatedPendingPodOccupiesItsNode(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	node := func(name string) *corev1.Node {
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3"}},
			Status:     corev1.NodeStatus{Allocatable: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")}},
		}
	}
	pod := func(name, nodeName, nominated string, phase corev1.PodPhase) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "x"},
			Spec: corev1.PodSpec{NodeName: nodeName, Containers: []corev1.Container{{
				Name: "c", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")}},
			}}},
			Status: corev1.PodStatus{Phase: phase, NominatedNodeName: nominated},
		}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(
		node("n1"), node("n2"),
		pod("running", "n1", "", corev1.PodRunning),
		pod("nominated", "", "n2", corev1.PodPending),
		pod("unscheduled", "", "", corev1.PodPending),
	).Build()

	byType, byNamespace, nodes, err := NewK8sWithGpuOperator(c).DiscoverUsageWithNodes(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 8, nodes["n1"].Used)
	assert.Equal(t, 8, nodes["n2"].Used, "the nominated pod holds n2's hole")
	assert.Equal(t, 16, byType["NVIDIA-H100-80GB-HBM3"], "one walk, every view: the type count agrees")
	assert.Equal(t, 16, byNamespace["x"]["NVIDIA-H100-80GB-HBM3"])
}
