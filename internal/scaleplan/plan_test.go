package scaleplan

import (
	"context"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const ctrlNS = "scaling-manager-system"

func modelServer(ns, name, role string, args ...string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{roleLabel: role}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "vllm", Args: args}}},
		}},
	}
}

func planConfigMap(data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: ctrlNS}, Data: data}
}

func newWriter(t *testing.T, managed []string, objs ...client.Object) (*Writer, client.Client) {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	return &Writer{Client: cl, Namespace: ctrlNS, Instance: ctrlNS, Log: logr.Discard(),
		Managed: func(context.Context) ([]string, error) { return managed, nil }}, cl
}

// parse returns the ScaledObjects of one plan key.
func parse(t *testing.T, plan string) []*unstructured.Unstructured {
	t.Helper()
	var out []*unstructured.Unstructured
	dec := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(plan), 4096)
	for {
		u := &unstructured.Unstructured{}
		if err := dec.Decode(&u.Object); err != nil {
			break
		}
		if u.Object != nil && u.GetKind() != "" {
			out = append(out, u)
		}
	}
	return out
}

// One ScaledObject per serving Deployment in each managed namespace, with
// the model id, the TLS scaler address, KEDA's ClusterTriggerAuthentication
// and the labels the uninstall selects; nothing for a namespace not managed,
// a Deployment without a serving role, and a comment for one with no model id.
func TestPlan(t *testing.T) {
	w, cl := newWriter(t, []string{"team-a"},
		planConfigMap(nil),
		modelServer("team-a", "qwen-decode", "decode", "--model", "Qwen/Qwen3-0.6B", "--served-model-name=qwen"),
		modelServer("team-a", "qwen-prefill", "prefill", "--model=Qwen/Qwen3-0.6B"),
		modelServer("team-a", "nameless-decode", "decode", "--port", "8000"),
		modelServer("team-a", "launcher", "requester", "--model", "x"),
		modelServer("team-b", "other-decode", "decode", "--model", "y"),
	)
	ctx := context.Background()
	if err := w.Once(ctx); err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: ctrlNS, Name: ConfigMapName}, cm); err != nil {
		t.Fatal(err)
	}
	if _, ok := cm.Data["team-b"]; ok || len(cm.Data) != 1 {
		t.Fatalf("plan keys %v, want team-a only", cm.Data)
	}
	sos := parse(t, cm.Data["team-a"])
	if len(sos) != 2 {
		t.Fatalf("%d ScaledObjects, want 2 (decode and prefill):\n%s", len(sos), cm.Data["team-a"])
	}
	models := map[string]string{}
	for _, so := range sos {
		trig := so.Object["spec"].(map[string]any)["triggers"].([]any)[0].(map[string]any)
		models[so.GetName()] = trig["metadata"].(map[string]any)["modelID"].(string)
		if trig["metadata"].(map[string]any)["scalerAddress"] != "scaling-manager-external-scaler."+ctrlNS+".svc:9090" {
			t.Errorf("%s: scalerAddress %v", so.GetName(), trig["metadata"])
		}
		if trig["authenticationRef"].(map[string]any)["name"] != "scaling-manager-external-scaler-"+ctrlNS {
			t.Errorf("%s: authenticationRef %v", so.GetName(), trig["authenticationRef"])
		}
		if so.GetLabels()["app.kubernetes.io/instance"] != ctrlNS || so.GetLabels()["app.kubernetes.io/managed-by"] != "llm-d-scaling-manager" {
			t.Errorf("%s: labels %v", so.GetName(), so.GetLabels())
		}
		if so.GetNamespace() != "team-a" {
			t.Errorf("%s: namespace %q", so.GetName(), so.GetNamespace())
		}
	}
	if models["qwen-decode"] != "qwen" || models["qwen-prefill"] != "Qwen/Qwen3-0.6B" {
		t.Fatalf("model ids %v: --served-model-name wins over --model", models)
	}
	if !strings.Contains(cm.Data["team-a"], "# Deployment nameless-decode: no --served-model-name or --model") {
		t.Fatalf("no note for the Deployment without a model id:\n%s", cm.Data["team-a"])
	}

	// A namespace that stops being managed loses its key.
	w.Managed = func(context.Context) ([]string, error) { return nil, nil }
	if err := w.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: ctrlNS, Name: ConfigMapName}, cm); err != nil {
		t.Fatal(err)
	}
	if len(cm.Data) != 0 {
		t.Fatalf("plan keys %v after team-a stopped being managed", cm.Data)
	}
}

// The plan ConfigMap is the manifest's: without it the writer says so and
// creates nothing.
func TestPlanNeedsThePreCreatedConfigMap(t *testing.T) {
	w, cl := newWriter(t, []string{"team-a"}, modelServer("team-a", "d", "decode", "--model", "m"))
	err := w.Once(context.Background())
	if err == nil || !strings.Contains(err.Error(), "apply the manifest again") {
		t.Fatalf("err %v", err)
	}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: ctrlNS, Name: ConfigMapName}, &corev1.ConfigMap{}); err == nil {
		t.Fatal("the writer created the ConfigMap")
	}
}

func TestModelID(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--model", "a", "--served-model-name", "b"}, "b"},
		{[]string{"--model=a"}, "a"},
		{[]string{"--model", "--port"}, ""},
		{[]string{"--port", "8000"}, ""},
	}
	for _, c := range cases {
		if got := ModelID([]corev1.Container{{Args: c.args}}); got != c.want {
			t.Errorf("ModelID(%v) = %q, want %q", c.args, got, c.want)
		}
	}
}
