package preflight

import (
	"context"
	"fmt"
	"strings"
	"sync"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Resources read through the dynamic client: their types are not in the
// scheme, or their CRD may not be installed.
var (
	crdGVR        = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	apiServiceGVR = schema.GroupVersionResource{Group: "apiregistration.k8s.io", Version: "v1", Resource: "apiservices"}
	ctaGVR        = schema.GroupVersionResource{Group: "keda.sh", Version: "v1alpha1", Resource: "clustertriggerauthentications"}
	podMonitorGVR = schema.GroupVersionResource{Group: "monitoring.coreos.com", Version: "v1", Resource: "podmonitors"}
	prometheusGVR = schema.GroupVersionResource{Group: "monitoring.coreos.com", Version: "v1", Resource: "prometheuses"}
)

// modelServerSelector selects the pods that serve a model: the llm-d roles
// the shipped PodMonitor scrapes (config/modelserver-metrics).
const modelServerSelector = "llm-d.ai/role in (decode,prefill)"

// externalMetricsAPIService is the APIService KEDA serves external metrics
// through; its Service names the namespace KEDA runs in.
const externalMetricsAPIService = "v1beta1.external.metrics.k8s.io"

// tri is a yes/no answer that may not be known.
type tri int

const (
	triUnknown tri = iota
	triYes
	triNo
)

// state is what checks share. Each fact is read once, on first use.
type state struct {
	env *Env

	settingsOnce sync.Once
	settings     settingsState

	osOnce    sync.Once
	openshift tri
	osWhy     string

	kedaOnce   sync.Once
	kedaNS     string
	kedaNSWhy  string
	kedaNSKind kedaNSKind

	podsOnce  sync.Once
	modelPods map[string][]corev1.Pod
	podsWhy   string

	saOnce   sync.Once
	saExists tri
	saWhy    string

	// Set by prometheus.reachable.
	prom       Prometheus
	promUsable bool
	promWhy    string

	// Set by podmonitor.port, read by prometheus.selects.
	podMonitors []podMonitorRef
}

// kedaNSKind says how kedaNamespace was found.
type kedaNSKind int

const (
	kedaNSUnread kedaNSKind = iota
	kedaNSFound
	kedaNSOther // the APIService is served by something other than KEDA
	kedaNSAbsent
)

// podMonitorRef is a PodMonitor that selects model-server pods.
type podMonitorRef struct {
	namespace string
	name      string
	labels    map[string]string
}

// ctrlUser is the controller ServiceAccount's user name.
func (s *state) ctrlUser() string {
	return "system:serviceaccount:" + s.env.Namespace + ":" + ControllerServiceAccount
}

// managed returns the namespaces the install manages, and false for a
// cluster-wide install that names none (it manages every namespace): the
// watchNamespaces setting where the controller accepts it, else what its
// Deployment names (loadSettings).
func (s *state) managed() ([]string, bool) {
	st := s.loadSettings(context.Background())
	if len(st.watched) > 0 {
		return st.watched, true
	}
	return nil, false
}

// isOpenShift reads the API groups: OpenShift serves route.openshift.io.
// Discovery needs no RBAC beyond what every authenticated account has.
func (s *state) isOpenShift() (tri, string) {
	s.osOnce.Do(func() {
		groups, err := s.env.Kube.Discovery().ServerGroups()
		if err != nil {
			s.openshift, s.osWhy = triUnknown, "cannot list the API groups: "+firstLine(err.Error())
			return
		}
		s.openshift = triNo
		for _, g := range groups.Groups {
			if g.Name == "route.openshift.io" || g.Name == "config.openshift.io" {
				s.openshift = triYes
				return
			}
		}
	})
	return s.openshift, s.osWhy
}

// kedaNamespace reads where KEDA runs from the Service behind the
// external-metrics APIService, as the installer does (llmsm_keda_apiservice_ns).
func (s *state) kedaNamespace(ctx context.Context) (string, kedaNSKind, string) {
	s.kedaOnce.Do(func() {
		obj, err := s.env.Dynamic.Resource(apiServiceGVR).Get(ctx, externalMetricsAPIService, metav1.GetOptions{})
		if err != nil {
			absent, why := readVerdict(err, "APIService "+externalMetricsAPIService)
			if absent {
				s.kedaNSKind, s.kedaNSWhy = kedaNSAbsent, "no APIService "+externalMetricsAPIService+": nothing serves external metrics, so KEDA is not installed or not running"
				return
			}
			s.kedaNSKind, s.kedaNSWhy = kedaNSUnread, why
			return
		}
		ns, _, _ := unstructured.NestedString(obj.Object, "spec", "service", "namespace")
		name, _, _ := unstructured.NestedString(obj.Object, "spec", "service", "name")
		switch {
		case ns == "":
			s.kedaNSKind, s.kedaNSWhy = kedaNSAbsent, "APIService "+externalMetricsAPIService+" names no Service"
		case strings.Contains(name, "keda"):
			s.kedaNS, s.kedaNSKind = ns, kedaNSFound
		default:
			s.kedaNS, s.kedaNSKind = ns, kedaNSOther
			s.kedaNSWhy = fmt.Sprintf("APIService %s is served by %s/%s, not KEDA", externalMetricsAPIService, ns, name)
		}
	})
	return s.kedaNS, s.kedaNSKind, s.kedaNSWhy
}

// modelServers lists the model-server pods per namespace: in the managed
// namespaces, or cluster-wide for a cluster install that names none.
func (s *state) modelServers(ctx context.Context) (map[string][]corev1.Pod, string) {
	s.podsOnce.Do(func() {
		s.modelPods = map[string][]corev1.Pod{}
		nss, named := s.managed()
		if !named {
			nss = []string{metav1.NamespaceAll}
		}
		for _, ns := range nss {
			list, err := s.env.Kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: modelServerSelector})
			if err != nil {
				where := "in " + ns
				if ns == metav1.NamespaceAll {
					where = "in every namespace"
				}
				_, why := readVerdict(err, "pods "+where)
				s.podsWhy = why
				s.modelPods = nil
				return
			}
			for _, p := range list.Items {
				s.modelPods[p.Namespace] = append(s.modelPods[p.Namespace], p)
			}
		}
	})
	return s.modelPods, s.podsWhy
}

// controllerSA says whether the controller's ServiceAccount exists yet.
// Before the install it does not, and the controller's RBAC is not there to
// check.
func (s *state) controllerSA(ctx context.Context) (tri, string) {
	s.saOnce.Do(func() {
		_, err := s.env.Kube.CoreV1().ServiceAccounts(s.env.Namespace).Get(ctx, ControllerServiceAccount, metav1.GetOptions{})
		switch {
		case err == nil:
			s.saExists = triYes
		case apierrors.IsNotFound(err):
			s.saExists, s.saWhy = triNo, "ServiceAccount "+ControllerServiceAccount+" does not exist in "+s.env.Namespace+" yet: the controller is not installed"
		default:
			_, s.saWhy = readVerdict(err, "ServiceAccount "+ControllerServiceAccount+" in "+s.env.Namespace)
			s.saExists = triUnknown
		}
	})
	return s.saExists, s.saWhy
}

// access is one question to ask about the controller's ServiceAccount.
type access struct {
	verb, group, resource, namespace, name string
}

func (a access) String() string {
	r := a.resource
	if a.group != "" {
		r += "." + a.group
	}
	if a.name != "" {
		r += "/" + a.name
	}
	if a.namespace == "" {
		return a.verb + " " + r + " (cluster)"
	}
	return a.verb + " " + r + " in " + a.namespace
}

// can asks the API server, by SubjectAccessReview, whether the controller's
// ServiceAccount may do a. A refused or failed review is an error, never a no.
func (s *state) can(ctx context.Context, a access) (bool, error) {
	sar := &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   s.ctrlUser(),
			Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + s.env.Namespace, "system:authenticated"},
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Verb: a.verb, Group: a.group, Resource: a.resource, Namespace: a.namespace, Name: a.name,
			},
		},
	}
	out, err := s.env.Kube.AuthorizationV1().SubjectAccessReviews().Create(ctx, sar, metav1.CreateOptions{})
	if err != nil {
		return false, err
	}
	if out.Status.EvaluationError != "" && !out.Status.Allowed {
		return false, fmt.Errorf("the review could not be evaluated: %s", out.Status.EvaluationError)
	}
	return out.Status.Allowed, nil
}

// splitList splits a comma-separated value, dropping blanks.
func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
