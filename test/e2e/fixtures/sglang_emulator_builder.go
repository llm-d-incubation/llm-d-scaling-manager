package fixtures

import (
	"context"
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

const (
	// sglangEmulatorImage is a CPU-only image used to run the synthetic SGLang
	// metrics emitter. A real SGLang server needs a GPU; this emitter instead
	// serves the sglang:* Prometheus metrics the scaling manager consumes, so the engine-aware
	// metric-collection path can be exercised in the kind-emulator environment.
	// Not from docker.io, per the project's e2e image policy.
	sglangEmulatorImage = "registry.access.redhat.com/ubi9/python-311:latest"
	sglangScriptSuffix  = "-sglang-script"
)

// sglangEmitterScript is served at :8000/metrics. Counters grow with elapsed time
// so rate()/increase() are non-zero; gauges hold a fixed saturated operating
// point. The served model name is read from the MODEL env var so it matches the
// model ID annotation on the annotated scaler.
//
// IDLE_AFTER freezes the counters that many seconds in, which is what lets a
// suite emulate a model that SERVED AND THEN WENT QUIET. Without it the counters
// grow for as long as the pod lives, so increase() never reaches zero and such a
// model can never be seen as idle — it could never be parked, and a scale-to-zero
// test against it could never pass. Unset (the default) keeps the original
// grow-forever behaviour every other suite relies on.
const sglangEmitterScript = `import os, time
from http.server import BaseHTTPRequestHandler, HTTPServer

START = time.time()
MODEL = os.environ.get("MODEL", "sglang-emulator-model")
IDLE_AFTER = float(os.environ.get("IDLE_AFTER", "0"))

def render():
    el = max(time.time() - START, 0.001)
    # Freeze the counters once the serving window is over: a counter that stops
    # advancing is exactly what an idle model looks like to increase().
    if IDLE_AFTER > 0:
        el = min(el, IDLE_AFTER)
    L = 'model_name="%s"' % MODEL
    out = []
    def s(n, v):
        out.append('%s{%s} %r' % (n, L, float(v)))
    s("sglang:num_running_reqs", 5)
    s("sglang:num_queue_reqs", 3)
    s("sglang:token_usage", 0.85)
    s("sglang:max_total_num_tokens", 100000)
    s("sglang:num_requests_total", 2 * el)
    s("sglang:prompt_tokens_total", 1000 * el)
    # cached_tokens_total carries an extra cache_source label (as real SGLang does),
    # so its label set differs from prompt_tokens_total. The prefix-cache-hit-rate
    # query must aggregate this label away before dividing; emitting it here keeps
    # the e2e a genuine regression guard for that PromQL.
    out.append('sglang:cached_tokens_total{%s,cache_source="radix_cache"} %r' % (L, float(300 * el)))
    s("sglang:time_to_first_token_seconds_count", 2 * el)
    s("sglang:time_to_first_token_seconds_sum", 1.0 * el)
    s("sglang:inter_token_latency_seconds_count", 200 * el)
    s("sglang:inter_token_latency_seconds_sum", 4.0 * el)
    s("sglang:prompt_tokens_histogram_count", 2 * el)
    s("sglang:prompt_tokens_histogram_sum", 1000 * el)
    s("sglang:generation_tokens_histogram_count", 2 * el)
    s("sglang:generation_tokens_histogram_sum", 500 * el)
    return ("\n".join(out) + "\n").encode()

class H(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path.startswith("/metrics"):
            b = render()
            self.send_response(200)
            self.send_header("Content-Type", "text/plain; version=0.0.4")
            self.send_header("Content-Length", str(len(b)))
            self.end_headers()
            self.wfile.write(b)
        else:
            self.send_response(200); self.end_headers(); self.wfile.write(b"ok\n")
    def log_message(self, *a):
        pass

print("sglang-emulator serving :8000/metrics for %s" % MODEL, flush=True)
HTTPServer(("0.0.0.0", 8000), H).serve_forever()
`

// CreateSGLangEmulator deploys a synthetic SGLang model server: a CPU-only pod
// that serves sglang:* metrics and whose launch command is a faithful
// "python -m sglang.launch_server ..." line, so the scaling manager's engine detection classifies
// it as SGLang. It creates a ConfigMap ("<name>-sglang-script") and a Deployment
// ("<name>-decode").
//
// Pair it with CreateService/EnsureServiceMonitor (appLabel "<name>-decode").
// (pass "" to skip). The emitted metrics carry model_name == modelID.
// CreateSGLangEmulator deploys the emulator with counters that grow for the life
// of the pod.
func CreateSGLangEmulator(ctx context.Context, k8sClient *kubernetes.Clientset, namespace, name, modelID string) error {
	return CreateSGLangEmulatorIdleAfter(ctx, k8sClient, namespace, name, modelID, 0)
}

// CreateSGLangEmulatorIdleAfter is CreateSGLangEmulator with a serving window:
// after idleAfterSeconds the counters stop advancing, so the model reads as idle.
// Zero means never idle.
func CreateSGLangEmulatorIdleAfter(ctx context.Context, k8sClient *kubernetes.Clientset, namespace, name, modelID string, idleAfterSeconds int) error {
	appLabel := name + decodeNameSuffix
	cmName := name + sglangScriptSuffix

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: namespace,
			Labels:    map[string]string{"test-resource": defaultTestResourceLabelValue},
		},
		Data: map[string]string{"serve.py": sglangEmitterScript},
	}
	if _, err := k8sClient.CoreV1().ConfigMaps(namespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("create sglang configmap %s: %w", cmName, err)
	}

	labels := map[string]string{
		"app":                       appLabel,
		"llm-d.ai/inferenceServing": defaultLabelValueTrue,
		"test-resource":             defaultTestResourceLabelValue,
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: appLabel, Namespace: namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": appLabel}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "sglang",
							Image: sglangEmulatorImage,
							// Faithful SGLang launch line: The scaling manager's inferenceengine.Detect
							// sees "sglang.launch_server" and ParseSGLangArgs reads these
							// flags. serve.py ignores the extra argv.
							Command: []string{"python3", "-u", "/app/serve.py"},
							Args: []string{
								"sglang.launch_server",
								"--model-path=" + modelID,
								"--mem-fraction-static=0.85",
								"--max-running-requests=512",
								"--page-size=16",
								"--tp-size=1",
								"--max-total-tokens=100000",
								"--context-length=8192",
								"--disable-cuda-graph",
							},
							Env: []corev1.EnvVar{{Name: "MODEL", Value: modelID}, {Name: "IDLE_AFTER", Value: strconv.Itoa(idleAfterSeconds)}},
							Ports: []corev1.ContainerPort{
								{Name: defaultServicePortName, ContainerPort: defaultModelServiceContainerPort, Protocol: corev1.ProtocolTCP},
							},
							VolumeMounts: []corev1.VolumeMount{{Name: "script", MountPath: "/app"}},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("50m"),
									corev1.ResourceMemory: resource.MustParse("64Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("200m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "script",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{Name: cmName},
								},
							},
						},
					},
				},
			},
		},
	}
	if _, err := k8sClient.AppsV1().Deployments(namespace).Create(ctx, deployment, metav1.CreateOptions{}); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("create sglang deployment %s: %w", appLabel, err)
	}
	return nil
}

// DeleteSGLangEmulator removes the emulator Deployment and ConfigMap. Idempotent;
// ignores NotFound.
func DeleteSGLangEmulator(ctx context.Context, k8sClient *kubernetes.Clientset, namespace, name string) error {
	appLabel := name + decodeNameSuffix
	if err := k8sClient.AppsV1().Deployments(namespace).Delete(ctx, appLabel, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete sglang deployment %s: %w", appLabel, err)
	}
	if err := k8sClient.CoreV1().ConfigMaps(namespace).Delete(ctx, name+sglangScriptSuffix, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete sglang configmap %s: %w", name+sglangScriptSuffix, err)
	}
	return nil
}
