// Package scaleplan writes the controller's suggested ScaledObjects: one per
// model server in each managed namespace, as YAML a person reviews and
// applies, into the pre-created ConfigMap scaling-manager-scaledobjects-plan
// (the kubectl install proposal, PR #404, "A suggested plan"). It never
// creates a ScaledObject itself: registering a workload stays a person's act.
//
// It is the core of deploy/lib/scaledobject.sh's discovery, ported: Deployments
// whose pod template carries a serving role (llm-d.ai/role decode or
// prefill), with the model id from the serving container's
// --served-model-name, else --model. A workload without one is listed as a
// comment, never guessed.
package scaleplan

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// ConfigMapName is the plan ConfigMap, pre-created empty by the manifest.
const ConfigMapName = "scaling-manager-scaledobjects-plan"

// DefaultInterval is how often the plan is written again.
const DefaultInterval = time.Minute

// Defaults of a suggested ScaledObject; the person edits them before applying.
const (
	DefaultMinReplicas = 1
	DefaultMaxReplicas = 10
)

// roleLabel marks a model server's pods, and servingRoles the roles that
// serve (an FMA requester holds no engine).
const roleLabel = "llm-d.ai/role"

var servingRoles = []string{"decode", "prefill"}

// Managed lists the namespaces the controller manages
// (managedns.Admission.Report's first result).
type Managed func(ctx context.Context) ([]string, error)

// Writer writes the plan every Interval. Leader only.
type Writer struct {
	// Client lists Deployments and updates the plan ConfigMap.
	Client client.Client
	// Namespace is the controller's, where the plan ConfigMap is.
	Namespace string
	// Managed lists the managed namespaces.
	Managed Managed
	// Instance is the app.kubernetes.io/instance label value a suggested
	// ScaledObject carries, so the uninstall's selector finds it.
	Instance string
	Interval time.Duration
	Log      logr.Logger
}

// NeedLeaderElection is true: one writer.
func (*Writer) NeedLeaderElection() bool { return true }

// Start writes the plan until ctx ends. It implements manager.Runnable.
func (w *Writer) Start(ctx context.Context) error {
	interval := w.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	wait.UntilWithContext(ctx, func(ctx context.Context) {
		if err := w.Once(ctx); err != nil {
			w.Log.Error(err, "Cannot write the suggested ScaledObjects", "configMap", ConfigMapName)
		}
	}, interval)
	return nil
}

// Once writes the plan for every managed namespace, one ConfigMap key per
// namespace, and drops the keys of namespaces no longer managed. The
// ConfigMap is updated only when a key changed.
func (w *Writer) Once(ctx context.Context) error {
	namespaces, err := w.Managed(ctx)
	if err != nil {
		return err
	}
	want := map[string]string{}
	for _, ns := range namespaces {
		plan, err := w.plan(ctx, ns)
		if err != nil {
			return err
		}
		want[ns] = plan
	}
	cm := &corev1.ConfigMap{}
	if err := w.Client.Get(ctx, client.ObjectKey{Namespace: w.Namespace, Name: ConfigMapName}, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("ConfigMap %s in %s does not exist: the release manifest creates it, and the controller "+
				"does not; apply the manifest again", ConfigMapName, w.Namespace)
		}
		return err
	}
	if maps.Equal(cm.Data, want) || (len(cm.Data) == 0 && len(want) == 0) {
		return nil
	}
	cm.Data = want
	return w.Client.Update(ctx, cm)
}

// plan is the YAML of the suggested ScaledObjects for one namespace.
func (w *Writer) plan(ctx context.Context, ns string) (string, error) {
	var list appsv1.DeploymentList
	if err := w.Client.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return "", fmt.Errorf("listing Deployments in %s: %w", ns, err)
	}
	items := slices.Clone(list.Items)
	slices.SortFunc(items, func(a, b appsv1.Deployment) int { return strings.Compare(a.Name, b.Name) })
	var b strings.Builder
	fmt.Fprintf(&b, "# Suggested ScaledObjects for namespace %s, written by the scaling manager. Nothing here is applied.\n", ns)
	fmt.Fprintf(&b, "# Review min and max, then: kubectl get configmap %s -n %s -o jsonpath='{.data.%s}' | kubectl apply -n %s -f -\n",
		ConfigMapName, w.Namespace, ns, ns)
	n := 0
	for i := range items {
		d := &items[i]
		if !slices.Contains(servingRoles, d.Spec.Template.Labels[roleLabel]) {
			continue
		}
		model := ModelID(d.Spec.Template.Spec.Containers)
		if model == "" {
			fmt.Fprintf(&b, "# Deployment %s: no --served-model-name or --model on its containers, so no model id; "+
				"write one by hand with metadata.modelID.\n", d.Name)
			continue
		}
		so, err := yaml.Marshal(w.scaledObject(ns, d.Name, model))
		if err != nil {
			return "", err
		}
		b.WriteString("---\n")
		b.Write(so)
		n++
	}
	if n == 0 {
		b.WriteString("# No model server found: no Deployment here has a pod label llm-d.ai/role of decode or prefill.\n")
	}
	return b.String(), nil
}

// scaledObject is one suggested ScaledObject, as a plain map so the plan
// needs no KEDA types and carries only what a person sets.
func (w *Writer) scaledObject(ns, deployment, model string) map[string]any {
	return map[string]any{
		"apiVersion": "keda.sh/v1alpha1",
		"kind":       "ScaledObject",
		"metadata": map[string]any{
			"name":      deployment,
			"namespace": ns,
			"labels": map[string]any{
				"app.kubernetes.io/managed-by": "llm-d-scaling-manager",
				"app.kubernetes.io/instance":   w.Instance,
			},
		},
		"spec": map[string]any{
			"scaleTargetRef":  map[string]any{"name": deployment},
			"minReplicaCount": DefaultMinReplicas,
			"maxReplicaCount": DefaultMaxReplicas,
			"triggers": []any{map[string]any{
				"type": "external",
				"authenticationRef": map[string]any{
					"kind": "ClusterTriggerAuthentication",
					"name": "scaling-manager-external-scaler-" + w.Namespace,
				},
				"metadata": map[string]any{
					"scalerAddress": "scaling-manager-external-scaler." + w.Namespace + ".svc:9090",
					"modelID":       model,
				},
			}},
		},
	}
}

// ModelID is the model a serving container serves: --served-model-name,
// else --model, read from args and command in either --flag value or
// --flag=value form; "" when none says.
func ModelID(containers []corev1.Container) string {
	for _, flag := range []string{"--served-model-name", "--model"} {
		for _, c := range containers {
			argv := append(slices.Clone(c.Command), c.Args...)
			for i, a := range argv {
				if v, ok := strings.CutPrefix(a, flag+"="); ok && v != "" {
					return v
				}
				if a == flag && i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
					return argv[i+1]
				}
			}
		}
	}
	return ""
}
