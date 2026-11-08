package fixtures

import (
	"context"
	"errors"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// conflictOnFirstNodeUpdate makes the first Update of a Node return 409, the
// way the API server does when the kubelet wrote the Node between our read and
// our write, and lets every later Update through. It returns a pointer to the
// count of Node updates seen.
func conflictOnFirstNodeUpdate(c *fake.Clientset) *int {
	updates := 0
	c.PrependReactor("update", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
		updates++
		if updates == 1 {
			node := action.(k8stesting.UpdateAction).GetObject().(*corev1.Node)
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "nodes"}, node.Name,
				errObjectModified)
		}
		return false, nil, nil
	})
	return &updates
}

var errObjectModified = errors.New("the object has been modified; please apply your changes to the latest version and try again")

// A Conflict on the first write is retried against a fresh read, so the label
// lands instead of failing the caller's BeforeAll.
func TestPatchNodeLabelRetriesOnConflict(t *testing.T) {
	const node = "kind-scaling-manager-gpu-cluster-worker"
	c := fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: node, Labels: map[string]string{"keep": "me"},
	}})
	updates := conflictOnFirstNodeUpdate(c)

	value := "H100"
	if err := patchNodeLabel(context.Background(), c, node, "nvidia.com/gpu.product", &value); err != nil {
		t.Fatalf("patchNodeLabel: %v", err)
	}
	if *updates != 2 {
		t.Fatalf("node updates = %d, want 2 (one conflict, one retry)", *updates)
	}
	got, err := c.CoreV1().Nodes().Get(context.Background(), node, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Labels["nvidia.com/gpu.product"] != value {
		t.Errorf("label = %q, want %q", got.Labels["nvidia.com/gpu.product"], value)
	}
	if got.Labels["keep"] != "me" {
		t.Errorf("unrelated label lost: %v", got.Labels)
	}
}

// LabelNodeAccelerator and its restore both survive a Conflict: the restore
// puts back a label the node had and removes one it did not.
func TestLabelNodeAcceleratorRetriesOnConflict(t *testing.T) {
	const node = "kind-scaling-manager-gpu-cluster-worker"
	// Two vendors can share a product label (Intel's GPU kinds do), so the
	// helper writes every entry but restores each distinct key once.
	keys := acceleratorProductLabels()
	slices.Sort(keys)
	keys = slices.Compact(keys)
	if len(keys) < 2 {
		t.Fatalf("want at least two vendor product labels, got %v", keys)
	}
	writes := len(acceleratorProductLabels())
	c := fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: node, Labels: map[string]string{keys[0]: "original"},
	}})
	updates := conflictOnFirstNodeUpdate(c)

	restore, err := LabelNodeAccelerator(context.Background(), c, node, "held")
	if err != nil {
		t.Fatalf("LabelNodeAccelerator: %v", err)
	}
	if *updates != writes+1 {
		t.Fatalf("node updates = %d, want %d (one conflict, then one per vendor)", *updates, writes+1)
	}
	got, err := c.CoreV1().Nodes().Get(context.Background(), node, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if got.Labels[k] != "held" {
			t.Errorf("label %s = %q, want held", k, got.Labels[k])
		}
	}

	*updates = 0
	if err := restore(context.Background()); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if *updates != len(keys)+1 {
		t.Fatalf("restore node updates = %d, want %d (one conflict, then one per key)", *updates, len(keys)+1)
	}
	got, err = c.CoreV1().Nodes().Get(context.Background(), node, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Labels[keys[0]] != "original" {
		t.Errorf("label %s = %q, want original restored", keys[0], got.Labels[keys[0]])
	}
	for _, k := range keys[1:] {
		if v, had := got.Labels[k]; had {
			t.Errorf("label %s = %q, want removed", k, v)
		}
	}
}
