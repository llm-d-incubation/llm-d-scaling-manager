package utils

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

const testControllerNS = "llm-d-sim"

// controllerLabels are the labels the controller Pod carries, and the ones
// fixtures.ControllerDriverLabels puts on a relay.
func controllerLabels(extra map[string]string) map[string]string {
	l := map[string]string{
		"app.kubernetes.io/name": "scaling-manager",
		"control-plane":          "controller-manager",
	}
	for k, v := range extra {
		l[k] = v
	}
	return l
}

func testPod(ns, name string, labels map[string]string, ownerRS string, age time.Duration) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         ns,
			Name:              name,
			Labels:            labels,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-age)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if ownerRS != "" {
		p.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "apps/v1", Kind: "ReplicaSet", Name: ownerRS, Controller: ptr.To(true),
		}}
	}
	return p
}

// labelTwins is the controller Pod and every e2e Pod that wears its labels.
// The impostors are all younger than the controller, so a pick by label and
// age lands on one of them.
func labelTwins() []runtime.Object {
	return []runtime.Object{
		testPod(testControllerNS, "scaling-manager-controller-manager-7d9f-abcde",
			controllerLabels(map[string]string{"pod-template-hash": "7d9f"}),
			"scaling-manager-controller-manager-7d9f", 10*time.Minute),
		// A relay (fixtures.CreateHTTPDriver): a bare Pod with the controller's labels.
		testPod(testControllerNS, "e2e-control-driver", controllerLabels(nil), "", time.Minute),
		// A warm-pool Pod: owned by its own Deployment's ReplicaSet. Real ones
		// carry only app.kubernetes.io/name; this one wears control-plane too, so
		// the label selector cannot be what keeps it out.
		testPod(testControllerNS, "e2e-pool-5c6b-qwert",
			controllerLabels(map[string]string{"pod-template-hash": "5c6b", "app.kubernetes.io/component": "warm-pool"}),
			"e2e-pool-5c6b", 2*time.Minute),
		// A Deployment whose name merely starts with the controller's.
		testPod(testControllerNS, "scaling-manager-controller-manager-canary-88aa-zxcvb",
			controllerLabels(map[string]string{"pod-template-hash": "88aa"}),
			"scaling-manager-controller-manager-canary-88aa", 3*time.Minute),
		// The real controller's twin in another namespace.
		testPod("other", "scaling-manager-controller-manager-7d9f-fghij",
			controllerLabels(map[string]string{"pod-template-hash": "7d9f"}),
			"scaling-manager-controller-manager-7d9f", time.Minute),
	}
}

func TestControllerPodsIgnoresLabelTwins(t *testing.T) {
	c := fake.NewClientset(labelTwins()...)
	pods, err := ControllerPods(context.Background(), c, testControllerNS, ControllerDeploymentName)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(pods))
	for _, p := range pods {
		names = append(names, p.Name)
	}
	if len(names) != 1 || names[0] != "scaling-manager-controller-manager-7d9f-abcde" {
		t.Fatalf("ControllerPods = %v, want only the controller Pod", names)
	}
}

func TestDumpControllerLogsDumpsOnlyTheController(t *testing.T) {
	c := fake.NewClientset(labelTwins()...)
	var out bytes.Buffer
	DumpControllerLogs(context.Background(), c, testControllerNS, &out)
	got := out.String()
	if !strings.Contains(got, "Logs from pod scaling-manager-controller-manager-7d9f-abcde") {
		t.Fatalf("controller Pod not dumped:\n%s", got)
	}
	if n := strings.Count(got, "Logs from pod "); n != 1 {
		t.Fatalf("dumped %d Pods, want 1:\n%s", n, got)
	}
}

func TestControllerLogsContainReadsOnlyTheController(t *testing.T) {
	c := fake.NewClientset(labelTwins()...)
	_, logs, err := ControllerLogsContain(context.Background(), c, testControllerNS, ControllerDeploymentName, "", 60)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(logs, "--- pod: "); n != 1 || !strings.Contains(logs, "--- pod: scaling-manager-controller-manager-7d9f-abcde ---") {
		t.Fatalf("read %d Pods, want only the controller:\n%s", n, logs)
	}
}
