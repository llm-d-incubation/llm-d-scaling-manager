package whatifsource

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/prometheus/common/model"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/locator"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/variantmeta"
)

// The fleet at one step: which variant each Pod is, its role, and what
// kube-state-metrics says of it (readiness, Ready time, start, revision,
// GPUs, node). A kubeconfig sees today's Pods, and a past window's are
// gone, so all of it is read from kube-state-metrics in the same
// Prometheus, or from --role-of.
//
// The collector reads a Pod through two seams, and the fleet is both: its
// PodLocator (the variant a Pod is, its labels) and the uncached reader it
// lists the namespace's Pods with (readiness, Ready time, start, revision).
// Without kube-state-metrics there is no reader, and the collector takes
// every Pod as Ready and present, as it does when it cannot list Pods.

// The kube-state-metrics series a capture reads, each in the namespace
// (%[1]s) but the node labels. kube_pod_labels carries label_* only for the
// keys --metric-labels-allowlist names.
const (
	ksmPodLabels      = `kube_pod_labels{namespace="%[1]s"}`
	ksmPodOwner       = `kube_pod_owner{namespace="%[1]s"}`
	ksmReplicaSetOwn  = `kube_replicaset_owner{namespace="%[1]s"}`
	ksmPodRunning     = `kube_pod_status_phase{namespace="%[1]s",phase="Running"}`
	ksmPodReady       = `kube_pod_status_ready{namespace="%[1]s",condition="true"}`
	ksmPodReadyTime   = `kube_pod_status_ready_time{namespace="%[1]s"}`
	ksmPodCreated     = `kube_pod_created{namespace="%[1]s"}`
	ksmPodStarted     = `kube_pod_start_time{namespace="%[1]s"}`
	ksmPodDeleted     = `kube_pod_deletion_timestamp{namespace="%[1]s"}`
	ksmPodGPUs        = `sum by (pod) (kube_pod_container_resource_requests{namespace="%[1]s",resource="nvidia_com_gpu"})`
	ksmPodNode        = `kube_pod_info{namespace="%[1]s"}`
	ksmNodeLabels     = `kube_node_labels`
	ksmDeployReplicas = `kube_deployment_spec_replicas{namespace="%[1]s"}`
)

// KSM is the kube-state-metrics queries a capture of one namespace asks.
type KSM struct {
	PodLabels, PodOwner, ReplicaSetOwner, PodRunning, PodReady, PodReadyTime string
	PodCreated, PodStarted, PodDeleted, PodGPUs, PodNode, NodeLabels         string
	DeploymentReplicas                                                       string
}

// KSMQueries is the kube-state-metrics queries a capture of namespace
// asks.
func KSMQueries(namespace string) KSM {
	ns := source.EscapePromQLValue(namespace)
	in := func(q string) string { return fmt.Sprintf(q, ns) }
	return KSM{
		PodLabels: in(ksmPodLabels), PodOwner: in(ksmPodOwner), ReplicaSetOwner: in(ksmReplicaSetOwn),
		PodRunning: in(ksmPodRunning), PodReady: in(ksmPodReady), PodReadyTime: in(ksmPodReadyTime),
		PodCreated: in(ksmPodCreated), PodStarted: in(ksmPodStarted), PodDeleted: in(ksmPodDeleted),
		PodGPUs: in(ksmPodGPUs), PodNode: in(ksmPodNode), NodeLabels: ksmNodeLabels,
		DeploymentReplicas: in(ksmDeployReplicas),
	}
}

// acceleratorNodeLabel is the node label the accelerator is read from, as
// the engine reads it from a scale target's nodeSelector.
const acceleratorNodeLabel = "nvidia.com/gpu.product"

// pod is one Pod at one step.
type pod struct {
	name string
	// ksm is whether kube-state-metrics listed the Pod at this step.
	ksm bool
	// labels is its kube_pod_labels label_* labels, nil when none are
	// exported.
	labels map[string]string
	// variant and role are what the role map makes it, variant "" for none,
	// and inferred whether that came from its name alone.
	variant, role string
	inferred      bool
	// overlap is every --role-of that picked it; more than one is refused
	// (ErrRoleOverlap).
	overlap []string
	// ownerKind is the kind kube-state-metrics says owns it.
	ownerKind string

	running, ready, deleting bool
	created, started, since  time.Time
	revision                 string
	gpus                     int
	node                     string
}

// fleet is the fleet at one step.
type fleet struct {
	namespace string
	ksm       bool
	roleOf    []RoleOf
	// pods is the Pods kube-state-metrics listed, by name; empty without it.
	pods map[string]*pod
	// named is the Pods only a series named, attributed on first use.
	named map[string]*pod
	// replicas is each Deployment's spec replicas, and nodeAccel each
	// node's accelerator.
	replicas  map[string]int
	nodeAccel map[string]string
}

// fleetAt reads the fleet at the step api is set to.
func fleetAt(ctx context.Context, api *windowAPI, namespace string, ksm bool, roleOf []RoleOf) (*fleet, error) {
	f := &fleet{namespace: namespace, ksm: ksm, roleOf: roleOf, pods: map[string]*pod{},
		named: map[string]*pod{}, replicas: map[string]int{}, nodeAccel: map[string]string{}}
	if !ksm {
		return f, nil
	}
	k := KSMQueries(namespace)
	var err error
	ask := func(q string) model.Vector {
		if err != nil {
			return nil
		}
		v, _, qerr := api.Query(ctx, q, time.Time{})
		if qerr != nil {
			err = fmt.Errorf("kube-state-metrics: %w", qerr)
			return nil
		}
		vec, _ := v.(model.Vector)
		return vec
	}
	labelsV, ownerV, rsV := ask(k.PodLabels), ask(k.PodOwner), ask(k.ReplicaSetOwner)
	runningV, readyV, readyTimeV := ask(k.PodRunning), ask(k.PodReady), ask(k.PodReadyTime)
	createdV, startedV, deletedV := ask(k.PodCreated), ask(k.PodStarted), ask(k.PodDeleted)
	gpusV, nodeV, nodeLabelsV, replicasV := ask(k.PodGPUs), ask(k.PodNode), ask(k.NodeLabels), ask(k.DeploymentReplicas)
	if err != nil {
		return nil, err
	}

	get := func(name string) *pod {
		p := f.pods[name]
		if p == nil {
			p = &pod{name: name, ksm: true}
			f.pods[name] = p
		}
		return p
	}
	// The Pods: kube_pod_info lists every Pod kube-state-metrics knows.
	for _, s := range nodeV {
		if name := string(s.Metric["pod"]); name != "" {
			get(name).node = string(s.Metric["node"])
		}
	}
	for _, s := range labelsV {
		name := string(s.Metric["pod"])
		if name == "" {
			continue
		}
		p := get(name)
		for k, v := range s.Metric {
			if strings.HasPrefix(string(k), "label_") {
				if p.labels == nil {
					p.labels = map[string]string{}
				}
				p.labels[string(k)] = string(v)
			}
		}
	}
	rsOwner := map[string]string{}
	for _, s := range rsV {
		if s.Metric["owner_kind"] == "Deployment" {
			rsOwner[string(s.Metric["replicaset"])] = string(s.Metric["owner_name"])
		}
	}
	owner := map[string]string{}
	for _, s := range ownerV {
		name := string(s.Metric["pod"])
		if name == "" || f.pods[name] == nil {
			continue
		}
		f.pods[name].ownerKind = string(s.Metric["owner_kind"])
		if s.Metric["owner_kind"] != "ReplicaSet" {
			continue
		}
		rs := string(s.Metric["owner_name"])
		if d := rsOwner[rs]; d != "" {
			owner[name] = d
			// A Deployment's ReplicaSet is named <deployment>-<pod-template-hash>.
			f.pods[name].revision = strings.TrimPrefix(rs, d+"-")
		}
	}
	each := func(v model.Vector, set func(p *pod, value float64)) {
		for _, s := range v {
			if p := f.pods[string(s.Metric["pod"])]; p != nil && finite(float64(s.Value)) {
				set(p, float64(s.Value))
			}
		}
	}
	each(runningV, func(p *pod, v float64) { p.running = v == 1 })
	each(readyV, func(p *pod, v float64) { p.ready = v == 1 })
	each(readyTimeV, func(p *pod, v float64) { p.since = unixTime(v) })
	each(createdV, func(p *pod, v float64) { p.created = unixTime(v) })
	each(startedV, func(p *pod, v float64) { p.started = unixTime(v) })
	each(deletedV, func(p *pod, v float64) { p.deleting = v > 0 })
	each(gpusV, func(p *pod, v float64) { p.gpus = int(math.Round(v)) })
	gpuLabel := model.LabelName(ksmLabelName(acceleratorNodeLabel))
	for _, s := range nodeLabelsV {
		if accel := string(s.Metric[gpuLabel]); accel != "" {
			f.nodeAccel[string(s.Metric["node"])] = accel
		}
	}
	for _, s := range replicasV {
		if d := string(s.Metric["deployment"]); d != "" && finite(float64(s.Value)) {
			f.replicas[d] = int(math.Round(float64(s.Value)))
		}
	}
	for name, p := range f.pods {
		f.attribute(p, owner[name])
	}
	for _, name := range slices.Sorted(maps.Keys(f.pods)) {
		if p := f.pods[name]; len(p.overlap) > 1 {
			return nil, overlapErr(p)
		}
	}
	return f, nil
}

// overlapErr is a Pod more than one --role-of picked.
func overlapErr(p *pod) error {
	return fmt.Errorf("%w: Pod %s is picked by %s; narrow the selectors so each Pod has one",
		ErrRoleOverlap, p.name, strings.Join(p.overlap, " and "))
}

// inGroup reports whether kube-state-metrics shows p in a multi-Pod group:
// a LeaderWorkerSet's labels, where they are exported, or a StatefulSet
// owner, which is how a LeaderWorkerSet runs its groups.
func (p *pod) inGroup() bool {
	if p.ownerKind == "StatefulSet" || p.ownerKind == "LeaderWorkerSet" {
		return true
	}
	for _, k := range []string{lwsv1.WorkerIndexLabelKey, lwsv1.SetNameLabelKey} {
		if _, ok := p.labels[ksmLabelName(k)]; ok {
			return true
		}
	}
	return false
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// unixTime is a kube-state-metrics timestamp series' value, in seconds.
func unixTime(v float64) time.Time {
	if v <= 0 {
		return time.Time{}
	}
	sec, frac := math.Modf(v)
	return time.Unix(int64(sec), int64(math.Round(frac*1e9))).UTC()
}

// attribute sets p's variant and role: the --role-of that picks it, else
// the Deployment kube-state-metrics says owns it, with the role its
// llm-d.ai/role label names. Every --role-of that picks it is noted in
// p.overlap; more than one is refused by the caller.
func (f *fleet) attribute(p *pod, deployment string) {
	for _, r := range f.roleOf {
		if ok, inferred := r.matches(p.name, p.labels); ok {
			if len(p.overlap) == 0 {
				p.variant, p.role, p.inferred = r.Variant, r.Role, inferred
			}
			p.overlap = append(p.overlap, r.String())
		}
	}
	if len(f.roleOf) > 0 || deployment == "" {
		return
	}
	p.variant = deployment
	p.role = p.labels[ksmLabelName(variantmeta.RoleLabel)]
}

// lookup is the Pod named name: kube-state-metrics' when it listed it, else
// one attributed from the role map alone.
func (f *fleet) lookup(name string) *pod {
	if p := f.pods[name]; p != nil {
		return p
	}
	if f.ksm && len(f.pods) > 0 {
		// kube-state-metrics lists the namespace's Pods; one it does not
		// list is gone, and the collector drops its series (podIsGone).
		// At a step where it lists none, its series are missing, not the
		// Pods: they are attributed by --role-of alone, and read as Ready
		// (List).
		return nil
	}
	p := f.named[name]
	if p == nil {
		p = &pod{name: name}
		f.attribute(p, "")
		f.named[name] = p
	}
	return p
}

// Locate implements locator.PodLocator: the variant the role map makes the
// Pod, as the scaler the engine names a variant by.
func (f *fleet) Locate(_ context.Context, namespace, podName string) (*locator.ManagedScaler, error) {
	p := f.lookup(podName)
	if p == nil || p.variant == "" {
		return nil, nil
	}
	return &locator.ManagedScaler{Namespace: namespace, Name: p.variant}, nil
}

// ResolveScaleTarget implements locator.PodLocator; the collector does not
// call it, and a capture has no scale target to resolve.
func (f *fleet) ResolveScaleTarget(context.Context, string, string) (autoscalingv2.CrossVersionObjectReference, bool, error) {
	return autoscalingv2.CrossVersionObjectReference{}, false, nil
}

// readLabels are the Pod labels the collector reads (GetPodLabels): the LWS
// worker index and the FMA component and pairing.
var readLabels = []string{lwsv1.WorkerIndexLabelKey, constants.ComponentLabelKey, constants.DualPodsPairLabelKey,
	variantmeta.RoleLabel}

// GetPodLabels implements locator.PodLocator: the labels the collector reads,
// where kube-state-metrics exports them; nil otherwise.
func (f *fleet) GetPodLabels(_ context.Context, _, podName string) map[string]string {
	p := f.lookup(podName)
	if p == nil || len(p.labels) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, k := range readLabels {
		if v, ok := p.labels[ksmLabelName(k)]; ok {
			out[k] = v
		}
	}
	return out
}

// errNoPods is a step at which kube-state-metrics lists no Pod: its series
// are missing, not the Pods, and the collector fails open on a listing it
// cannot read.
var errNoPods = errors.New("whatifsource: kube-state-metrics lists no Pod in the namespace at this step")

// List implements client.Reader for a PodList of the namespace: the Pods
// kube-state-metrics listed at this step, as the collector reads them.
func (f *fleet) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	pods, ok := list.(*corev1.PodList)
	if !ok {
		return fmt.Errorf("whatifsource: a capture lists Pods only, not %T", list)
	}
	if len(f.pods) == 0 {
		return errNoPods
	}
	pods.Items = pods.Items[:0]
	for _, p := range f.pods {
		pods.Items = append(pods.Items, p.object(f.namespace))
	}
	return nil
}

// Get implements client.Reader: nothing is read one object at a time.
func (f *fleet) Get(_ context.Context, key client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	return apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, key.Name)
}

// object is the Pod as the collector reads it (pod_serving_state.go): its
// phase, Ready condition and its transition time, creation and start times,
// deletion, and revision label.
func (p *pod) object(namespace string) corev1.Pod {
	o := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: p.name, Namespace: namespace}}
	if p.revision != "" {
		o.Labels = map[string]string{"pod-template-hash": p.revision}
	}
	if !p.created.IsZero() {
		o.CreationTimestamp = metav1.NewTime(p.created)
	}
	if p.deleting {
		now := metav1.NewTime(p.created)
		o.DeletionTimestamp = &now
	}
	if p.running {
		o.Status.Phase = corev1.PodRunning
	}
	if !p.started.IsZero() {
		t := metav1.NewTime(p.started)
		o.Status.StartTime = &t
	}
	cond := corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionFalse}
	if p.ready {
		cond.Status = corev1.ConditionTrue
		cond.LastTransitionTime = metav1.NewTime(p.since)
	}
	o.Status.Conditions = []corev1.PodCondition{cond}
	return o
}
