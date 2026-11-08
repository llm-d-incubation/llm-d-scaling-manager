package fixtures

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
)

// A Pod that makes HTTP requests on a test's behalf.
//
// Needed because the warm pool's ports are guarded by a NetworkPolicy, and a
// policy is a statement about which POD may connect. The API server's pod-proxy
// arrives from the control plane, so it is not admitted by any `from:` selector
// — measured: proxying to the pool's control port timed out, and the timeout
// arrived as a 503 that a naive assertion could not tell apart from the proxy's
// own "no model is awake" 503. Both problems go away if the request is made by a
// Pod: the policy sees the peer it was written about, and a denial looks like a
// connection failure rather than an HTTP answer.
//
// Which peer it is, is the caller's choice. A driver carrying the controller's
// labels is admitted to the control ports; one carrying a tenant's labels is
// not, and that difference is itself worth asserting.
const (
	driverPort = 8080

	// DriverImage has python and no curl, which is all the relay needs.
	DriverImage = "registry.access.redhat.com/ubi9/python-311:latest"

	driverScriptSuffix = "-driver-script"

	// driverPolicySuffix names the NetworkPolicy that admits the relay port.
	driverPolicySuffix = "-driver-ingress"

	// driverLabel is carried by every relay Pod, valued with the driver's
	// name. It is the only label the driver's own NetworkPolicy selects on.
	driverLabel = "http-driver"
)

// driverScript relays one request and reports what happened, INCLUDING the
// failure to connect at all. That distinction is the whole point: a
// NetworkPolicy denial is not an HTTP status.
const driverScript = `import json, urllib.error, urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer


class Relay(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        try:
            call = json.loads(self.rfile.read(length) or b"{}")
        except Exception as err:
            return self._reply({"status": 0, "body": "bad relay request: %s" % err})

        data = call.get("body") or None
        req = urllib.request.Request(
            call["url"], data=data.encode() if data else None, method=call.get("method", "GET"))
        if data:
            req.add_header("Content-Type", "application/json")
        try:
            with urllib.request.urlopen(req, timeout=call.get("timeout", 10)) as resp:
                self._reply({"status": resp.status, "body": resp.read().decode("utf-8", "replace")})
        except urllib.error.HTTPError as err:
            # A 4xx/5xx is an ANSWER. Several assertions are that a request is
            # refused with a particular code.
            self._reply({"status": err.code, "body": err.read().decode("utf-8", "replace")})
        except Exception as err:
            # Could not connect. This is what a NetworkPolicy denial looks like,
            # and it must never be confused with a status the peer returned.
            self._reply({"status": 0, "body": "unreachable: %s" % err})

    def do_GET(self):
        self._reply({"status": 200, "body": "relay ready"})

    def _reply(self, obj):
        body = json.dumps(obj).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


HTTPServer(("0.0.0.0", 8080), Relay).serve_forever()
`

// DriverSpec describes a relay Pod.
type DriverSpec struct {
	Name      string
	Namespace string
	// Labels the Pod carries. This is what a NetworkPolicy matches on, so it
	// decides whether this driver is admitted to the guarded ports.
	Labels map[string]string
}

// ControllerDriverLabels make a driver look like the scaling manager controller, which is
// what the pool's NetworkPolicy admits to the supervisor and control ports.
func ControllerDriverLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name": "scaling-manager",
		"control-plane":          "controller-manager",
	}
}

// TenantDriverLabels make a driver look like an ordinary workload in the
// namespace: admitted to the serving port and nothing else.
func TenantDriverLabels() map[string]string {
	return map[string]string{"app.kubernetes.io/name": "some-tenant-workload"}
}

// CreateHTTPDriver stands up a relay Pod.
func CreateHTTPDriver(ctx context.Context, clientset *kubernetes.Clientset, spec DriverSpec) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: spec.Name + driverScriptSuffix, Namespace: spec.Namespace},
		Data:       map[string]string{"driver.py": driverScript},
	}
	if _, err := clientset.CoreV1().ConfigMaps(spec.Namespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil &&
		!errors.IsAlreadyExists(err) {
		return fmt.Errorf("create driver script: %w", err)
	}

	// Before the Pod, so the relay port is admitted by the time it is Ready.
	if err := createDriverNetworkPolicy(ctx, clientset, spec); err != nil {
		return err
	}

	labels := map[string]string{driverLabel: spec.Name}
	for k, v := range spec.Labels {
		labels[k] = v
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: spec.Name, Namespace: spec.Namespace, Labels: labels},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Volumes: []corev1.Volume{{
				Name: "script",
				VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: spec.Name + driverScriptSuffix},
				}},
			}},
			Containers: []corev1.Container{{
				Name:         "relay",
				Image:        DriverImage,
				Command:      []string{"python3", "/script/driver.py"},
				Ports:        []corev1.ContainerPort{{Name: "relay", ContainerPort: driverPort}},
				VolumeMounts: []corev1.VolumeMount{{Name: "script", MountPath: "/script"}},
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						HTTPGet: &corev1.HTTPGetAction{Path: "/", Port: intstrFromInt(driverPort)},
					},
					InitialDelaySeconds: 2,
					PeriodSeconds:       2,
				},
			}},
		},
	}
	if _, err := clientset.CoreV1().Pods(spec.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil &&
		!errors.IsAlreadyExists(err) {
		return fmt.Errorf("create driver pod %s: %w", spec.Name, err)
	}
	return nil
}

// createDriverNetworkPolicy admits the relay port on this one driver.
//
// A driver wearing the controller's labels (ControllerDriverLabels) is selected
// by the shipped external-scaler NetworkPolicy whenever the controller runs in
// the driver's namespace, and that policy admits :8443, :8081 and :9090 only.
// The test reaches the relay on :8080 through the API server's pod proxy, so
// without an allow of its own the relay is cut off from its only caller.
// kindnetd skips traffic that originates on the control-plane node itself,
// which is why a relay scheduled there answered and one on a worker timed out
// with `dial tcp <pod-ip>:8080: i/o timeout`.
//
// NetworkPolicy allows are additive, so a second policy that selects only this
// driver and admits :8080 opens the relay port without touching the product's
// policy. It has no `from:`, because the pod proxy arrives from the control
// plane, which no peer selector can name. It is created for every driver, not
// only controller-labelled ones: which shipped policies select a driver is the
// product's business, and the relay port has to answer either way.
func createDriverNetworkPolicy(ctx context.Context, clientset *kubernetes.Clientset, spec DriverSpec) error {
	tcp := corev1.ProtocolTCP
	port := intstrFromInt(driverPort)
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.Name + driverPolicySuffix,
			Namespace: spec.Namespace,
			Labels:    map[string]string{driverLabel: spec.Name},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{driverLabel: spec.Name}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
			}},
		},
	}
	if _, err := clientset.NetworkingV1().NetworkPolicies(spec.Namespace).Create(
		ctx, policy, metav1.CreateOptions{}); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("create driver network policy %s: %w", policy.Name, err)
	}
	return nil
}

// DeleteHTTPDriver removes a relay, its script and its NetworkPolicy.
func DeleteHTTPDriver(ctx context.Context, clientset *kubernetes.Clientset, spec DriverSpec) error {
	if err := clientset.CoreV1().Pods(spec.Namespace).Delete(ctx, spec.Name,
		metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete driver %s: %w", spec.Name, err)
	}
	if err := clientset.CoreV1().ConfigMaps(spec.Namespace).Delete(ctx, spec.Name+driverScriptSuffix,
		metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete driver script: %w", err)
	}
	if err := clientset.NetworkingV1().NetworkPolicies(spec.Namespace).Delete(ctx, spec.Name+driverPolicySuffix,
		metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete driver network policy: %w", err)
	}
	return nil
}

// DriverCall makes one request FROM the driver Pod TO url, and reports what the
// driver saw.
//
// A Status of 0 means the driver could not connect at all, which is what a
// NetworkPolicy denial looks like and is deliberately distinct from every HTTP
// answer.
func DriverCall(
	ctx context.Context,
	clientset *kubernetes.Clientset,
	namespace, driver string,
	method, url, body string,
) (PodProxyResult, error) {
	return DriverCallTimeout(ctx, clientset, namespace, driver, method, url, body, 0)
}

// DriverCallTimeout is DriverCall with a relay timeout of its own.
//
// The relay defaults to ten seconds, which is right for the control calls this
// suite is mostly made of and wrong for two things a warm pool does: a
// generation of several hundred tokens, and a sleep that DRAINS -- both block
// for as long as the work takes. Zero keeps the default.
//
// Worth having rather than raising the default: a control call that hangs should
// fail quickly and loudly, and only the caller knows which kind it is making.
func DriverCallTimeout(
	ctx context.Context,
	clientset *kubernetes.Clientset,
	namespace, driver string,
	method, url, body string,
	timeout time.Duration,
) (PodProxyResult, error) {
	request := map[string]any{"method": method, "url": url, "body": body}
	if timeout > 0 {
		request["timeout"] = timeout.Seconds()
	}
	call, err := json.Marshal(request)
	if err != nil {
		return PodProxyResult{}, fmt.Errorf("encode relay request: %w", err)
	}

	// The relay itself is reached through the API server. The pool's
	// NetworkPolicy does not select a driver, and the driver's own policy
	// (createDriverNetworkPolicy) admits this port even where the shipped
	// external-scaler policy selects the driver by the controller's labels.
	relayed, err := PodProxy(ctx, clientset, namespace, driver, driverPort, "POST", "/", string(call))
	if err != nil {
		return PodProxyResult{}, fmt.Errorf("reach relay %s: %w", driver, err)
	}

	var answer struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
	}
	if err := json.Unmarshal([]byte(relayed.Body), &answer); err != nil {
		return PodProxyResult{}, fmt.Errorf("decode relay answer %q: %w", relayed.Body, err)
	}
	return PodProxyResult{Status: answer.Status, Body: answer.Body}, nil
}

// intstrFromInt keeps the probe declaration readable.
func intstrFromInt(port int) intstr.IntOrString { return intstr.FromInt32(int32(port)) }
