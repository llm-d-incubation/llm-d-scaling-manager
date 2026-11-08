package fixtures

import (
	"context"
	"fmt"

	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

// Fast Model Actuation layout, built without FMA.
//
// FMA splits a model server across two pods: a REQUESTER Deployment carrying the
// llm-d identity, which a scaler moves, and LAUNCHER pods owned by a
// LauncherConfig, which hold the GPU and run the engine. The ownerReferences gap
// is deliberate — FMA patches the provider's labels so no ReplicaSet adopts it —
// and it is why the collector's owner walk ends at nothing and falls back to the
// pairing declared on the pods.
//
// Reproducing that here needs no FMA controller and no CRDs, because the labels
// ARE the contract:
//
//   - a launcher pod's controller reference names a kind the owner walk cannot
//     follow, so the walk stops without reaching a scale target and without
//     erroring — exactly the production path.
//   - both halves carry dual-pods.llm-d.ai/dual naming each other, which is what
//     FMA's dual-pods controller maintains on a bound pair.
//
// A launcher is a plain pod, not a Deployment, which also matches production: the
// populator creates them directly.
//
// # Why the owner is a ConfigMap and not a fake LauncherConfig
//
// The obvious fixture points the ownerReference at "LauncherConfig/fma-<name>"
// and never creates it, on the reasoning that walkOwnersUp breaks at an unknown
// kind before it fetches anything. That reasoning is right about the walk and
// wrong about the cluster: Kubernetes' garbage collector resolves owners, finds
// nothing, and deletes the pod. Measured on a real cluster, the launcher pods
// were gone within five seconds of creation, and the e2e spec then waited five
// minutes for the collector to attribute pods that no longer existed.
//
// It passed in unit tests because the fake client has no garbage collector — the
// one difference that mattered.
//
// A ConfigMap is the fix: it is a real object, so the GC leaves the pod alone,
// and scaleTargetKindSupported does not recognise it, so the walk stops exactly
// where a LauncherConfig would stop it. The fixture is faithful in the property
// under test and honest about not being FMA.
const (
	// The stand-in owner. Any existing object of a kind the walk cannot follow
	// would do; a ConfigMap needs no CRD and no permissions beyond the namespace.
	fmaOwnerAPIVersion = "v1"
	fmaOwnerKind       = "ConfigMap"
	// The port FMA records on a bound provider pod. Read by the PodMonitor's
	// relabeling, which is why an unbound launcher generates no target.
	fmaServerPortAnnotation = "dual-pods.llm-d.ai/server-port"
)

// FMALayout names the objects a CreateFMALayout call produced, so a spec can
// assert against them without rebuilding the naming rules.
type FMALayout struct {
	// RequesterDeployment is the scale target — the half a ScaledObject points at.
	RequesterDeployment string
	// RequesterPod is the pod the launcher pairs with.
	RequesterPod string
	// LauncherPod holds the engine and reports the metrics.
	LauncherPod string
	// UnboundLauncherPod is a warm spare: a launcher with no pairing label. It
	// must NOT be attributed, and is included so a spec can prove that.
	UnboundLauncherPod string
	// OwnerConfigMap stands in for the LauncherConfig — see the note above.
	OwnerConfigMap string
}

// CreateFMALayout builds a bound FMA pair plus one unbound spare in namespace.
//
// modelLabel is the sanitized DNS-safe model label (llm-d.ai/model), not the
// model ID — the two are different strings and the collector compares this one
// only against another pod's copy of it.
//
// The requester Deployment is created with replicas=0 and its pod is created
// directly, so the pair can be wired to each other by name: a Deployment-managed
// pod gets a generated name that is not known until after it exists, and the
// pairing label has to name it.
func CreateFMALayout(ctx context.Context, k8sClient *kubernetes.Clientset, namespace, name, modelLabel, modelID string, maxNumSeqs int) (*FMALayout, error) {
	l := &FMALayout{
		RequesterDeployment: name,
		RequesterPod:        name + "-pod",
		LauncherPod:         "launcher-" + name,
		UnboundLauncherPod:  "launcher-" + name + "-spare",
		OwnerConfigMap:      "fma-" + name,
	}

	// The stand-in LauncherConfig. Created first: the launcher pods reference it,
	// and a dependent whose owner does not exist is garbage-collected.
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: l.OwnerConfigMap, Namespace: namespace,
		Labels: map[string]string{"test-resource": defaultTestResourceLabelValue},
	}}
	if _, err := k8sClient.CoreV1().ConfigMaps(namespace).Create(ctx, owner, metav1.CreateOptions{}); err != nil && !errors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("create launcher owner ConfigMap: %w", err)
	}
	ownerUID, err := k8sClient.CoreV1().ConfigMaps(namespace).Get(ctx, l.OwnerConfigMap, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("read back launcher owner ConfigMap: %w", err)
	}

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      l.RequesterDeployment,
			Namespace: namespace,
			Labels:    map[string]string{"test-resource": defaultTestResourceLabelValue},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(0)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": l.RequesterDeployment}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					"app":                        l.RequesterDeployment,
					"llm-d.ai/role":              "requester",
					constants.ModelLabelKey:      modelLabel,
					"llm-d.ai/inference-serving": defaultLabelValueTrue,
				}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "inference-server", Image: defaultModelServiceSimulatorImage,
				}}},
			},
		},
	}
	if _, err := k8sClient.AppsV1().Deployments(namespace).Create(ctx, deploy, metav1.CreateOptions{}); err != nil && !errors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("create requester deployment: %w", err)
	}

	// The requester pod, owned by the Deployment via a ReplicaSet stand-in. A
	// direct Deployment ownerReference is enough for the walk: it looks for the
	// first ancestor of a scale-target kind, and a Deployment is one.
	created, derr := k8sClient.AppsV1().Deployments(namespace).Get(ctx, l.RequesterDeployment, metav1.GetOptions{})
	if derr != nil {
		return nil, fmt.Errorf("read back requester deployment: %w", derr)
	}
	requester := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      l.RequesterPod,
			Namespace: namespace,
			Labels: map[string]string{
				"llm-d.ai/role":                "requester",
				constants.ModelLabelKey:        modelLabel,
				constants.DualPodsPairLabelKey: l.LauncherPod,
				"test-resource":                defaultTestResourceLabelValue,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "Deployment",
				Name: created.Name, UID: created.UID, Controller: ptr.To(true),
			}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "inference-server", Image: defaultModelServiceSimulatorImage,
		}}},
	}
	if _, err := k8sClient.CoreV1().Pods(namespace).Create(ctx, requester, metav1.CreateOptions{}); err != nil && !errors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("create requester pod: %w", err)
	}

	bound := fmaLauncherPod(namespace, l.LauncherPod, modelLabel, modelID, maxNumSeqs, l.RequesterPod, l.OwnerConfigMap, ownerUID.UID)
	if _, err := k8sClient.CoreV1().Pods(namespace).Create(ctx, bound, metav1.CreateOptions{}); err != nil && !errors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("create bound launcher: %w", err)
	}

	spare := fmaLauncherPod(namespace, l.UnboundLauncherPod, modelLabel, modelID, maxNumSeqs, "", l.OwnerConfigMap, ownerUID.UID)
	if _, err := k8sClient.CoreV1().Pods(namespace).Create(ctx, spare, metav1.CreateOptions{}); err != nil && !errors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("create unbound launcher: %w", err)
	}

	return l, nil
}

// fmaLauncherPod builds a server-providing pod. partner empty means unbound —
// FMA removes the pairing label when a pair breaks, and that absence is how a
// warm spare declines to be counted as capacity.
//
// The container runs the inference simulator on :8000, the same port FMA's real
// launchers serve on, so the shipped PodMonitor scrapes this pod through exactly
// the relabeling it uses in production. It declares NO container port, which is
// the property that makes a launcher invisible to a port-name endpoint and is the
// whole reason that PodMonitor exists — a fixture that declared one would test a
// pod FMA never creates.
func fmaLauncherPod(namespace, name, modelLabel, modelID string, maxNumSeqs int, partner, ownerName string, ownerUID types.UID) *corev1.Pod {
	labels := map[string]string{
		constants.ComponentLabelKey: constants.LauncherComponent,
		"test-resource":             defaultTestResourceLabelValue,
	}
	annotations := map[string]string{}
	if partner != "" {
		// FMA stamps the serving labels onto a launcher only at bind time, which
		// is what puts it in the InferencePool. An unbound spare has neither
		// these, nor the pairing label, nor the port annotation — so it is not
		// scraped and not attributed, which is what the spec asserts.
		labels[constants.DualPodsPairLabelKey] = partner
		labels[constants.ModelLabelKey] = modelLabel
		labels["llm-d.ai/inferenceServing"] = defaultLabelValueTrue
		annotations[fmaServerPortAnnotation] = "8000"
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      labels,
			Annotations: annotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: fmaOwnerAPIVersion,
				Kind:       fmaOwnerKind,
				Name:       ownerName,
				UID:        ownerUID,
				Controller: ptr.To(true),
			}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  "inference-server",
			Image: defaultModelServiceSimulatorImage,
			Args:  buildModelServerBaseArgs(modelID, true, maxNumSeqs),
			// Required by the args above, not optional decoration: the simulator
			// refuses to start with --enable-kvcache unless POD_IP is set —
			// "IP should be defined in the environment (POD_IP) for KV cache to
			// work" — and then CrashLoopBackOffs. A launcher that never starts
			// serves no metrics, so nothing is scraped and nothing is attributed,
			// which is a confusing way to watch an attribution test time out.
			// buildModelServiceDeployment sets the same three; borrowing its args
			// without its environment was the whole bug.
			Env: []corev1.EnvVar{
				{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.name"}}},
				{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"}}},
				{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "status.podIP"}}},
			},
		}}},
	}
}

// EnsureFMALauncherPodMonitor creates the PodMonitor that scrapes launcher pods,
// mirroring config/fma-launcher-metrics so the spec exercises the shipped shape
// rather than a test-only one: keep the inference-server container, drop pods
// without a bound instance, and build the address from the port FMA records.
func EnsureFMALauncherPodMonitor(ctx context.Context, crClient client.Client, namespace string) error {
	pm := &promoperator.PodMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fma-launcher-metrics",
			Namespace: namespace,
			Labels:    map[string]string{"test-resource": defaultTestResourceLabelValue},
		},
		Spec: promoperator.PodMonitorSpec{
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{constants.ComponentLabelKey: constants.LauncherComponent},
			},
			PodMetricsEndpoints: []promoperator.PodMetricsEndpoint{{
				Path:     "/metrics",
				Interval: "15s",
				RelabelConfigs: []promoperator.RelabelConfig{
					{
						SourceLabels: []promoperator.LabelName{"__meta_kubernetes_pod_container_name"},
						Action:       "keep",
						Regex:        "inference-server",
					},
					{
						SourceLabels: []promoperator.LabelName{"__meta_kubernetes_pod_annotation_dual_pods_llm_d_ai_server_port"},
						Action:       "keep",
						Regex:        ".+",
					},
					{
						SourceLabels: []promoperator.LabelName{
							"__meta_kubernetes_pod_ip",
							"__meta_kubernetes_pod_annotation_dual_pods_llm_d_ai_server_port",
						},
						Regex:       "(.+);(.+)",
						Replacement: ptr.To("$1:$2"),
						TargetLabel: "__address__",
						Action:      "replace",
					},
				},
			}},
		},
	}
	if err := crClient.Create(ctx, pm); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("create FMA launcher PodMonitor: %w", err)
	}
	return nil
}

// DeleteFMALauncherPodMonitor removes it; a missing object is not an error.
func DeleteFMALauncherPodMonitor(ctx context.Context, crClient client.Client, namespace string) error {
	pm := &promoperator.PodMonitor{ObjectMeta: metav1.ObjectMeta{Name: "fma-launcher-metrics", Namespace: namespace}}
	if err := crClient.Delete(ctx, pm); err != nil && !errors.IsNotFound(err) {
		return err
	}
	return nil
}

// DeleteFMALayout removes everything CreateFMALayout made. Missing objects are
// not an error, so a spec can call it unconditionally in a cleanup step.
func DeleteFMALayout(ctx context.Context, k8sClient *kubernetes.Clientset, namespace string, l *FMALayout) error {
	if l == nil {
		return nil
	}
	for _, pod := range []string{l.RequesterPod, l.LauncherPod, l.UnboundLauncherPod} {
		if err := k8sClient.CoreV1().Pods(namespace).Delete(ctx, pod, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("delete pod %s: %w", pod, err)
		}
	}
	if err := k8sClient.AppsV1().Deployments(namespace).Delete(ctx, l.RequesterDeployment, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete deployment %s: %w", l.RequesterDeployment, err)
	}
	// Last: deleting it first would have the garbage collector remove the
	// launcher pods out from under the spec.
	if err := k8sClient.CoreV1().ConfigMaps(namespace).Delete(ctx, l.OwnerConfigMap, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete owner ConfigMap %s: %w", l.OwnerConfigMap, err)
	}
	return nil
}
