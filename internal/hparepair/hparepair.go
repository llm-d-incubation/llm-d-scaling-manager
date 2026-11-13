// Package hparepair re-wires HPAs that KEDA 2.19 and older built for this
// scaler's ScaledObjects without the scaler's External metric.
//
// KEDA asks an external scaler for a ScaledObject's metric spec when it
// reconciles the ScaledObject. KEDA 2.19 and older keep one gRPC connection
// per scaler address; after the scaler was unreachable (a controller restart,
// rollout or outage) that connection stays failed for up to two minutes, the
// call fails at once, and KEDA builds the HPA from the empty spec, which
// Kubernetes defaults to CPU. Nothing retries: the ScaledObject reads Ready and
// the scaler's decisions reach nothing. KEDA 2.20 retries and 2.21 drops
// unused connections.
//
// KEDA's ScaledObject controller also reconciles when an annotation of an HPA
// it owns changes, asks the scaler again, and rewrites the HPA's annotations
// from the ScaledObject's, which drops the one written here. So the repair
// annotates the HPA, never the ScaledObject: that needs patch on
// horizontalpodautoscalers only, and changing an annotation of the
// ScaledObject does not make KEDA ask again.
//
// It is off unless KEDA_HPA_REPAIR is true, and runs on the leader only.
//
// Run on kind with KEDA 2.18.1: an HPA KEDA built while the controller was
// down was wired about 60 seconds after the controller came back, on the
// second request (the first met KEDA's still-failing connection). KEDA 2.19
// was not run.
package hparepair

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/scaler"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/scalertls"
)

// Annotation is the HPA annotation the repair writes. Its value is the time of
// the request. KEDA removes it when it next writes the HPA.
const Annotation = "scaling-manager.llmd.ai/keda-rewire"

// Event reasons on the ScaledObject.
const (
	// EventRewireRequested is the reason of the Normal event that says the
	// HPA was annotated so KEDA derives it again.
	EventRewireRequested = "HPARewireRequested"
	// EventRewireFailed is the reason of the Warning event that says the
	// annotation could not be written.
	EventRewireFailed = "HPARewireFailed"
	// EventRewired is the reason of the Normal event that says KEDA put the
	// scaler's metric on the HPA after one or more requests.
	EventRewired = "HPARewired"
)

// Defaults.
const (
	// DefaultInterval is how often the repair reads the HPAs.
	DefaultInterval = 30 * time.Second
	// DefaultGrace is how old an HPA must be before it counts as unwired:
	// KEDA writes the External metric in the reconcile that creates the
	// HPA, and a read in the same moment can see the object before that.
	DefaultGrace = 30 * time.Second
	// DefaultMinBackoff is the wait after the first request before the next.
	// KEDA 2.19 and older keep a failed connection for up to two minutes, so
	// the first one or two requests can fail the same way.
	DefaultMinBackoff = 30 * time.Second
	// DefaultMaxBackoff caps the doubling wait between requests.
	DefaultMaxBackoff = 10 * time.Minute
	// DefaultVerdictTTL is how long what one ScaledObject read said about an
	// HPA is kept while the HPA's generation and owner do not change.
	DefaultVerdictTTL = 10 * time.Minute
)

// Repairer finds and annotates unwired HPAs. Build it with its exported
// fields; zero durations take the defaults.
type Repairer struct {
	// Reader MUST be uncached (manager.GetAPIReader): a cached read would
	// start a cluster-wide informer on HPAs and ScaledObjects, and the
	// controller may not watch ScaledObjects.
	Reader client.Reader
	// Writer patches the HPA's annotation.
	Writer client.Writer
	// Recorder, if set, receives events on the ScaledObject.
	Recorder record.EventRecorder
	// ScalerNamespace is the namespace this scaler's Service runs in. Only
	// triggers naming it count.
	ScalerNamespace string
	// Namespaces are the namespaces to read. Empty reads every namespace.
	Namespaces []string

	// Interval is how often a pass runs. Zero takes DefaultInterval.
	Interval time.Duration
	// Grace is how old an HPA must be before it counts as unwired. Zero
	// takes DefaultGrace.
	Grace time.Duration
	// MinBackoff is the wait after the first request on an HPA. Zero takes
	// DefaultMinBackoff.
	MinBackoff time.Duration
	// MaxBackoff caps the doubling wait between requests. Zero takes
	// DefaultMaxBackoff.
	MaxBackoff time.Duration
	// VerdictTTL is how long a ScaledObject read is reused for an HPA whose
	// generation and owner have not changed. Zero takes DefaultVerdictTTL.
	VerdictTTL time.Duration

	now func() time.Time
	// verdicts is what the last ScaledObject read said about each HPA, by
	// UID, so an HPA that is not this scaler's, or is wired, costs one read
	// per VerdictTTL rather than one per pass. Only touched from Pass.
	verdicts map[types.UID]*verdict
	// tracked is the state per HPA, by UID, of every HPA the repair has asked
	// about at least once. Only touched from Pass.
	tracked map[types.UID]*attempt
	// patchDenied remembers that the last patch was Forbidden, so the warning
	// is logged once until a patch succeeds again.
	patchDenied bool
	// unread remembers whether the last pass was unread, so the change is
	// logged once.
	unread bool
}

// verdict is what one ScaledObject read said about an HPA. It holds while
// the HPA keeps generation and owner, until until.
type verdict struct {
	generation int64
	owner      types.UID
	until      time.Time
	// ours are this scaler's trigger indices; empty when the ScaledObject
	// is not this scaler's, is paused, or is not the HPA's owner any more.
	// A scalingModifiers HPA carries composite-metric, never the triggers'
	// own metrics, so hpaCarries is false for it and it is read every pass:
	// only the ScaledObject's status shows its wiring.
	ours []int
}

// attempt is the repair's state for one HPA.
type attempt struct {
	namespace, scaledObject, hpa string
	// tries counts every patch attempt and sets the backoff; requests
	// counts the ones the API server accepted.
	tries, requests int
	next            time.Time
	seen            bool
}

// Result is what one Pass found. Unwired and Requested are by namespace.
type Result struct {
	Unwired   map[string]int
	Requested map[string]int
	// Unread lists what could not be read: "<namespace>" for an HPA list
	// ("*" for every namespace), "<namespace>/<scaledObject>" for a
	// ScaledObject.
	Unread []string
}

// NeedLeaderElection makes the repair run on the leader only: two replicas
// would annotate the same HPA twice.
func (r *Repairer) NeedLeaderElection() bool { return true }

// Start runs a pass every Interval until ctx is done.
func (r *Repairer) Start(ctx context.Context) error {
	interval := r.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	logger := ctrl.LoggerFrom(ctx).WithName("keda-hpa-repair")
	logger.Info("KEDA HPA repair is on: HPAs KEDA builds for this scaler's ScaledObjects without its External metric are annotated so KEDA derives them again",
		"scalerNamespace", r.ScalerNamespace, "namespaces", r.scope(), "interval", interval)
	ctx = ctrl.LoggerInto(ctx, logger)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		r.Pass(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (r *Repairer) scope() []string {
	if len(r.Namespaces) == 0 {
		return []string{"*"}
	}
	return r.Namespaces
}

func (r *Repairer) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// Pass reads the HPAs once, annotates each unwired one that is due, and
// publishes the metrics.
func (r *Repairer) Pass(ctx context.Context) Result {
	logger := ctrl.LoggerFrom(ctx)
	if r.tracked == nil {
		r.tracked = map[types.UID]*attempt{}
	}
	if r.verdicts == nil {
		r.verdicts = map[types.UID]*verdict{}
	}
	for uid, v := range r.verdicts {
		if !r.clock().Before(v.until) {
			delete(r.verdicts, uid)
		}
	}
	for _, a := range r.tracked {
		a.seen = false
	}
	res := Result{Unwired: map[string]int{}, Requested: map[string]int{}}
	keep := map[string]bool{}
	listedAll := true

	namespaces := r.Namespaces
	if len(namespaces) == 0 {
		namespaces = []string{""}
	}
	for _, ns := range namespaces {
		var list autoscalingv2.HorizontalPodAutoscalerList
		if err := r.Reader.List(ctx, &list, client.InNamespace(ns)); err != nil {
			where := ns
			if ns == "" {
				where = "*"
				listedAll = false
			}
			keep[ns] = true
			res.Unread = append(res.Unread, where)
			logger.V(1).Info("Could not list HPAs; their wiring is unknown", "namespace", where, "forbidden", apierrors.IsForbidden(err), "error", err.Error())
			r.keepTracked(ns)
			continue
		}
		if ns != "" {
			res.Unwired[ns] = 0
		}
		for i := range list.Items {
			r.check(ctx, &list.Items[i], &res, keep)
		}
	}

	r.forgetGone(ctx)
	if listedAll {
		metrics.PublishKEDAUnwiredHPAs(res.Unwired, keep)
	}
	metrics.SetKEDAHPAWiringUnread(len(res.Unread) > 0)
	if unread := len(res.Unread) > 0; unread != r.unread {
		r.unread = unread
		if unread {
			logger.Info("Could not read every HPA or ScaledObject: the wiring of some HPAs is unknown until it can (a Forbidden read needs get and list on horizontalpodautoscalers and get on scaledobjects)",
				"unread", res.Unread)
		} else {
			logger.Info("Every HPA and ScaledObject could be read again")
		}
	}
	return res
}

// keepTracked marks every tracked HPA in namespace ns ("" for all) as seen:
// a failed list says nothing about them.
func (r *Repairer) keepTracked(ns string) {
	for _, a := range r.tracked {
		if ns == "" || a.namespace == ns {
			a.seen = true
		}
	}
}

// forgetGone drops the state of every HPA this pass did not see: deleted,
// rebuilt under a new UID, or no longer this scaler's.
func (r *Repairer) forgetGone(ctx context.Context) {
	for uid, a := range r.tracked {
		if !a.seen {
			ctrl.LoggerFrom(ctx).V(1).Info("Forgetting an HPA the repair asked about: it is gone or no longer unwired",
				"namespace", a.namespace, "hpa", a.hpa)
			delete(r.tracked, uid)
		}
	}
}

// check classifies one HPA, and annotates it when it is this scaler's,
// unwired, past the grace and due.
func (r *Repairer) check(ctx context.Context, hpa *autoscalingv2.HorizontalPodAutoscaler, res *Result, keep map[string]bool) {
	logger := ctrl.LoggerFrom(ctx)
	owner := scaledObjectOwner(hpa)
	if owner == nil {
		return
	}
	// Reuse the last read while the HPA keeps its generation and owner: an
	// HPA that is not this scaler's (or is paused) needs nothing, and one of
	// ours whose metrics are all on it is wired. Not for an HPA the repair
	// asked about, whose recovery is reported below with the ScaledObject.
	if v := r.verdicts[hpa.UID]; v != nil && v.generation == hpa.Generation && v.owner == owner.UID && r.tracked[hpa.UID] == nil {
		if len(v.ours) == 0 || hpaCarries(hpa, v.ours) {
			return
		}
	}
	var so kedav1alpha1.ScaledObject
	if err := r.Reader.Get(ctx, types.NamespacedName{Namespace: hpa.Namespace, Name: owner.Name}, &so); err != nil {
		if apierrors.IsNotFound(err) {
			delete(r.verdicts, hpa.UID)
			return // being deleted: KEDA's garbage, not ours to wire
		}
		// Unknown, not "not ours": keep what was known about it.
		res.Unread = append(res.Unread, hpa.Namespace+"/"+owner.Name)
		keep[hpa.Namespace] = true
		if a := r.tracked[hpa.UID]; a != nil {
			a.seen = true
		}
		logger.V(1).Info("Could not read the ScaledObject that owns an HPA; its wiring is unknown",
			"namespace", hpa.Namespace, "scaledObject", owner.Name, "hpa", hpa.Name, "forbidden", apierrors.IsForbidden(err), "error", err.Error())
		return
	}
	v := &verdict{generation: hpa.Generation, owner: owner.UID,
		until: r.clock().Add(orDefault(r.VerdictTTL, DefaultVerdictTTL))}
	r.verdicts[hpa.UID] = v
	if so.UID != owner.UID {
		return // the HPA belongs to an earlier ScaledObject of that name
	}
	idx := OwnTriggers(&so, r.ScalerNamespace)
	if len(idx) == 0 || paused(&so) {
		return
	}
	v.ours = idx
	a := r.tracked[hpa.UID]
	if Wired(&so, hpa, idx) {
		if a != nil && a.requests > 0 {
			logger.Info("KEDA wired the scaler's metric onto the HPA",
				"namespace", hpa.Namespace, "scaledObject", so.Name, "hpa", hpa.Name, "requests", a.requests)
			r.event(&so, corev1.EventTypeNormal, EventRewired,
				"KEDA put the scaler's External metric on HPA %s after %d request(s)", hpa.Name, a.requests)
		}
		delete(r.tracked, hpa.UID)
		return
	}
	now := r.clock()
	if now.Sub(hpa.CreationTimestamp.Time) < orDefault(r.Grace, DefaultGrace) {
		return
	}
	res.Unwired[hpa.Namespace]++
	if a == nil {
		a = &attempt{namespace: hpa.Namespace, scaledObject: so.Name, hpa: hpa.Name}
		r.tracked[hpa.UID] = a
	}
	a.seen = true
	if now.Before(a.next) {
		return
	}
	r.request(ctx, &so, hpa, a, now, res)
}

// request annotates hpa and schedules the next request.
func (r *Repairer) request(ctx context.Context, so *kedav1alpha1.ScaledObject, hpa *autoscalingv2.HorizontalPodAutoscaler, a *attempt, now time.Time, res *Result) {
	logger := ctrl.LoggerFrom(ctx)
	a.tries++
	a.next = now.Add(Backoff(a.tries, orDefault(r.MinBackoff, DefaultMinBackoff), orDefault(r.MaxBackoff, DefaultMaxBackoff)))
	wait := a.next.Sub(now)

	patch := fmt.Appendf(nil, `{"metadata":{"annotations":{%q:%q}}}`, Annotation, now.UTC().Format(time.RFC3339))
	err := r.Writer.Patch(ctx, hpa, client.RawPatch(types.MergePatchType, patch))
	switch {
	case err == nil:
		a.requests++
		res.Requested[hpa.Namespace]++
		metrics.CountKEDAHPARewire(hpa.Namespace, constants.HPARewireRequested)
		r.patchDenied = false
		logger.Info("KEDA built this HPA without the scaler's External metric; annotated it so KEDA derives it again",
			"namespace", hpa.Namespace, "scaledObject", so.Name, "hpa", hpa.Name,
			"metrics", metricSummary(hpa), "request", a.requests, "nextCheckAfter", wait)
		r.event(so, corev1.EventTypeNormal, EventRewireRequested,
			"KEDA built HPA %s without the scaler's External metric (it has %s); annotated it so KEDA derives it again (request %d)",
			hpa.Name, metricSummary(hpa), a.requests)
	case apierrors.IsForbidden(err):
		metrics.CountKEDAHPARewire(hpa.Namespace, constants.HPARewireForbidden)
		if !r.patchDenied {
			logger.Info("Not allowed to annotate an unwired HPA: the controller's role needs patch on horizontalpodautoscalers; re-run the prereqs phase, or turn KEDA_HPA_REPAIR off",
				"namespace", hpa.Namespace, "hpa", hpa.Name, "error", err.Error())
		}
		r.patchDenied = true
		r.event(so, corev1.EventTypeWarning, EventRewireFailed,
			"HPA %s has no External metric for the scaler and could not be annotated: %v", hpa.Name, err)
	default:
		metrics.CountKEDAHPARewire(hpa.Namespace, constants.HPARewireFailed)
		logger.Info("Could not annotate an unwired HPA; will try again",
			"namespace", hpa.Namespace, "hpa", hpa.Name, "nextCheckAfter", wait, "error", err.Error())
	}
}

func (r *Repairer) event(so *kedav1alpha1.ScaledObject, kind, reason, format string, args ...any) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(so, kind, reason, format, args...)
}

// Backoff is the wait after the n-th attempt (n >= 1): lo, doubling, at most hi.
func Backoff(n int, lo, hi time.Duration) time.Duration {
	wait := lo
	for i := 1; i < n && wait < hi; i++ {
		wait *= 2
	}
	return min(wait, hi)
}

// scaledObjectOwner is the HPA's controlling owner when that is a KEDA
// ScaledObject, else nil.
func scaledObjectOwner(hpa *autoscalingv2.HorizontalPodAutoscaler) *metav1.OwnerReference {
	ref := metav1.GetControllerOfNoCopy(hpa)
	if ref == nil || ref.Kind != "ScaledObject" {
		return nil
	}
	if group, _, _ := strings.Cut(ref.APIVersion, "/"); group != kedav1alpha1.GroupVersion.Group {
		return nil
	}
	return ref
}

// paused reports whether KEDA is told not to scale so.
func paused(so *kedav1alpha1.ScaledObject) bool {
	if so.NeedToBePausedByAnnotation() {
		return true
	}
	for _, c := range so.Status.Conditions {
		if c.Type == kedav1alpha1.ConditionPaused && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

// NamesScaler reports whether address names the scaler Service in namespace:
// scaling-manager-external-scaler.<namespace>, with .svc or
// .svc.cluster.local, with or without a :<port>, in any letter case. No
// other form counts: the rule the installer applies.
func NamesScaler(address, namespace string) bool {
	if namespace == "" {
		return false
	}
	a := strings.ToLower(strings.TrimSpace(address))
	base := scalertls.ServiceName + "." + strings.ToLower(namespace)
	for _, host := range []string{base, base + ".svc", base + ".svc.cluster.local"} {
		if a == host || strings.HasPrefix(a, host+":") {
			return true
		}
	}
	return false
}

// OwnTriggers returns the indices of so's triggers whose scalerAddress names
// the scaler in scalerNamespace.
func OwnTriggers(so *kedav1alpha1.ScaledObject, scalerNamespace string) []int {
	var idx []int
	for i, t := range so.Spec.Triggers {
		if NamesScaler(t.Metadata[registry.ScalerAddressKey], scalerNamespace) {
			idx = append(idx, i)
		}
	}
	return idx
}

// Wired reports whether KEDA wired every trigger in idx: the ScaledObject's
// status.externalMetricNames holds its s<index>- metric, and the HPA carries
// it as an External metric. With scalingModifiers the HPA carries one
// composite-metric in place of the triggers' metrics whether or not KEDA got
// their specs, so then the status alone says which triggers were wired.
func Wired(so *kedav1alpha1.ScaledObject, hpa *autoscalingv2.HorizontalPodAutoscaler, idx []int) bool {
	var external []string
	for _, m := range hpa.Spec.Metrics {
		if m.Type == autoscalingv2.ExternalMetricSourceType && m.External != nil {
			external = append(external, m.External.Metric.Name)
		}
	}
	modifiers := so.IsUsingModifiers()
	if modifiers && !hasName(external, kedav1alpha1.CompositeMetricName) {
		return false
	}
	for _, i := range idx {
		prefix := fmt.Sprintf("s%d-", i)
		if !hasPrefix(so.Status.ExternalMetricNames, prefix) {
			return false
		}
		if !modifiers && !hasPrefix(external, prefix) {
			return false
		}
	}
	return true
}

// hpaCarries reports whether hpa has the External metric
// s<index>-<scaler.MetricName> of every trigger index in idx. Which indices
// are this scaler's only the ScaledObject says, so idx comes from a read: an
// HPA with s0 wired cannot tell on its own whether an s1 of ours failed.
//
// KEDA labels its HPAs app.kubernetes.io/managed-by=keda-operator, but copies
// the ScaledObject's own labels over it, so a Helm- or GitOps-owned
// ScaledObject's HPA reads managed-by=Helm: the list is not narrowed by it.
func hpaCarries(hpa *autoscalingv2.HorizontalPodAutoscaler, idx []int) bool {
	for _, i := range idx {
		want := fmt.Sprintf("s%d-%s", i, scaler.MetricName)
		found := false
		for _, m := range hpa.Spec.Metrics {
			if m.Type == autoscalingv2.ExternalMetricSourceType && m.External != nil && m.External.Metric.Name == want {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func hasPrefix(names []string, prefix string) bool {
	for _, n := range names {
		if strings.HasPrefix(n, prefix) {
			return true
		}
	}
	return false
}

func hasName(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// metricSummary names the HPA's metrics for a log line: "cpu" for the
// Kubernetes default, "none" for an empty list.
func metricSummary(hpa *autoscalingv2.HorizontalPodAutoscaler) string {
	var out []string
	for _, m := range hpa.Spec.Metrics {
		switch {
		case m.Resource != nil:
			out = append(out, string(m.Resource.Name))
		case m.External != nil:
			out = append(out, "external:"+m.External.Metric.Name)
		default:
			out = append(out, strings.ToLower(string(m.Type)))
		}
	}
	if len(out) == 0 {
		return "none"
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}
