package managedns

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
	poolutil "github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/pool"
)

// DefaultReportInterval is how often Reporter publishes the managed set.
const DefaultReportInterval = 15 * time.Second

// Readiness values start with one of these words.
const (
	// StateOK: the namespace is managed and nothing it needs is missing.
	StateOK = "ok"
	// StateWaiting: managed, but no ScaledObject there has called the
	// scaler yet.
	StateWaiting = "waiting"
	// StateNotReady: managed, but a grant it needs is missing.
	StateNotReady = "not ready"
	// StateNotManaged: KEDA calls the scaler for it, and it is not managed.
	StateNotManaged = "not managed"
)

// Event reasons, recorded on the ScaledObjects and InferencePools of a
// namespace when its readiness changes: a namespace owner reads them with
// kubectl get events -n <namespace>, without reading the controller's
// namespace.
const (
	EventNamespaceReady      = "ScalingManagerNamespaceReady"
	EventNamespaceWaiting    = "ScalingManagerNamespaceWaiting"
	EventNamespaceNotReady   = "ScalingManagerNamespaceNotReady"
	EventNamespaceNotManaged = "ScalingManagerNamespaceNotManaged"
)

// Sink receives the managed namespaces, the refused ones with why, and each
// namespace's readiness (controllerstatus.Publisher).
type Sink interface {
	SetNamespaces(managed []string, all bool, refused map[string]string)
	SetNamespaceReadiness(readiness map[string]string)
}

// Registry is the part of registry.Registry the Reporter reads and prunes.
type Registry interface {
	Snapshot() []registry.Entry
	Forget(namespace, name string)
}

// Pools is the part of the datastore the Reporter reads and prunes: the
// InferencePools whose EPP the controller scrapes.
type Pools interface {
	PoolList() []*poolutil.EndpointPool
	PoolDelete(namespacedName string)
}

// Grant is a per-namespace grant the controller needs while a switch is on,
// applied from scaling-manager-grants.yaml.
type Grant struct {
	// Name is the grant label value (scaling-manager.llmd.ai/grant).
	Name string
	// Group, Resource and Verb are what the grant allows.
	Group, Resource, Verb string
	// Why is the switch that needs it, e.g. "utilizationShare is active".
	Why string
	// Required: without it the namespace is not ready. Otherwise its
	// absence is a note: e.g. the role writer skips such a namespace.
	Required bool
}

// AccessChecker says whether the controller holds g in namespace.
type AccessChecker func(ctx context.Context, namespace string, g Grant) (bool, error)

// SelfAccess returns an AccessChecker that asks the API server with a
// SelfSubjectAccessReview, which every authenticated identity may create.
func SelfAccess(c client.Client) AccessChecker {
	return func(ctx context.Context, namespace string, g Grant) (bool, error) {
		r := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: namespace, Group: g.Group, Resource: g.Resource, Verb: g.Verb,
			},
		}}
		if err := c.Create(ctx, r); err != nil {
			return false, err
		}
		return r.Status.Allowed, nil
	}
}

// Reporter publishes, every Interval, which namespaces are managed, which
// are refused, and each one's readiness, records a change of readiness as an
// Event in that namespace, and forgets every registered workload and
// InferencePool in a namespace that is not managed.
type Reporter struct {
	Admission *Admission
	Sink      Sink
	Registry  Registry
	// Pools, when set, are pruned and counted.
	Pools Pools
	// PoolAPIVersion is the InferencePools' apiVersion, for their Events.
	PoolAPIVersion string
	// OnChange, when set, is called when the managed set changes, so the
	// InferencePools of a namespace managed since are read again.
	OnChange func(ctx context.Context)
	// Grants returns the grants needed now; nil needs none.
	Grants func() []Grant
	// Access checks a grant; nil skips the grant checks.
	Access AccessChecker
	// Recorder records the readiness Events; nil records none.
	Recorder record.EventRecorder
	// Interval between reports; DefaultReportInterval when zero.
	Interval time.Duration
	Log      logr.Logger

	mu        sync.Mutex
	lastSet   string
	haveLast  bool
	lastEvent map[string]string
}

// NeedLeaderElection is true: the status and the registry the engines read
// are the leader's.
func (*Reporter) NeedLeaderElection() bool { return true }

// Start reports until ctx ends. It implements manager.Runnable.
func (r *Reporter) Start(ctx context.Context) error {
	interval := r.Interval
	if interval <= 0 {
		interval = DefaultReportInterval
	}
	wait.UntilWithContext(ctx, r.Once, interval)
	return nil
}

// nsView is what one report knows about one namespace.
type nsView struct {
	managed       bool
	refusal       string
	scaledObjects []registry.Entry
	refusedSOs    []string
	pools         []*poolutil.EndpointPool
}

// Once runs one report.
func (r *Reporter) Once(ctx context.Context) {
	names, all := r.Admission.Namespaces()
	set := fmt.Sprintf("%v|%s", all, strings.Join(names, ","))
	r.mu.Lock()
	changed := r.haveLast && r.lastSet != set
	r.lastSet, r.haveLast = set, true
	r.mu.Unlock()
	if changed {
		r.Log.Info("The managed namespaces changed", "watchNamespaces", strings.Join(names, ","), "all", all)
	}

	views := map[string]*nsView{}
	view := func(ns string) *nsView {
		v, ok := views[ns]
		if !ok {
			v = &nsView{}
			v.refusal = r.Admission.Reason(ns)
			v.managed = v.refusal == ""
			views[ns] = v
		}
		return v
	}
	for _, ns := range names {
		view(ns)
	}
	if r.Registry != nil {
		for _, e := range r.Registry.Snapshot() {
			v := view(e.Namespace)
			if !v.managed {
				r.Registry.Forget(e.Namespace, e.Name)
				r.Log.Info("Forgot a workload in a namespace this controller does not manage",
					"namespace", e.Namespace, "scaledObject", e.Name, "reason", v.refusal)
				v.refusedSOs = append(v.refusedSOs, e.Name)
				continue
			}
			v.scaledObjects = append(v.scaledObjects, e)
		}
	}
	if r.Pools != nil {
		for _, p := range r.Pools.PoolList() {
			v := view(p.Namespace)
			if !v.managed {
				r.Pools.PoolDelete(p.Namespace + "/" + p.Name)
				r.Log.Info("Dropped an InferencePool in a namespace this controller does not manage",
					"namespace", p.Namespace, "inferencePool", p.Name, "reason", v.refusal)
				continue
			}
			v.pools = append(v.pools, p)
		}
	}
	refusedNS := map[string]string{}
	for ns, rf := range r.Admission.Refused() {
		v := view(ns)
		for _, so := range rf.ScaledObjects {
			if !slices.Contains(v.refusedSOs, so) {
				v.refusedSOs = append(v.refusedSOs, so)
			}
		}
		refusedNS[ns] = rf.Reason + " (KEDA called the scaler for it)"
	}
	var grants []Grant
	if r.Grants != nil {
		grants = r.Grants()
	}
	readiness, events := map[string]string{}, map[string]string{}
	for ns, v := range views {
		if !v.managed && len(v.refusedSOs) == 0 {
			continue // listed in watchNamespaces' complement, and nothing of ours there
		}
		readiness[ns], events[ns] = r.readiness(ctx, ns, v, grants)
	}
	if r.Sink != nil {
		managed := names
		if all {
			managed = nil
		}
		r.Sink.SetNamespaces(managed, all, refusedNS)
		r.Sink.SetNamespaceReadiness(readiness)
	}
	r.recordEvents(views, events)
	if changed && r.OnChange != nil {
		r.OnChange(ctx)
	}
}

// readiness is one namespace's readiness line: a state word, then what was
// found and what to do. event is the same line without what changes while
// nothing needs doing (the counts, the refused ScaledObjects, an error's
// wording), so an Event is recorded when the state or a problem changes,
// not on every registration.
func (r *Reporter) readiness(ctx context.Context, ns string, v *nsView, grants []Grant) (line, event string) {
	if !v.managed {
		so := ""
		if len(v.refusedSOs) > 0 {
			sort.Strings(v.refusedSOs)
			so = "; the scaler refuses ScaledObjects " + strings.Join(v.refusedSOs, ", ")
		}
		const todo = ". A cluster admin adds it to watchNamespaces (scaling-manager-admin.sh namespaces)"
		// The Event leaves out the reason, which names the managed
		// namespaces: another tenant's names, and a new Event on every
		// change of the list. The status line keeps it.
		return StateNotManaged + ": " + v.refusal + so + todo,
			StateNotManaged + ": the scaler refuses ScaledObjects in this namespace" + todo
	}
	var problems, notes, eventNotes []string
	for _, g := range grants {
		if r.Access == nil {
			break
		}
		ok, err := r.Access(ctx, ns, g)
		switch {
		case err != nil:
			notes = append(notes, fmt.Sprintf("grant %s not checked: %v", g.Name, err))
			eventNotes = append(eventNotes, fmt.Sprintf("grant %s not checked", g.Name))
		case ok:
		case g.Required:
			problems = append(problems, fmt.Sprintf("grant %s missing (%s): scaling-manager-owner.sh grant %s %s", g.Name, g.Why, g.Name, ns))
		default:
			notes = append(notes, fmt.Sprintf("no %s grant (%s): skipped here until scaling-manager-owner.sh grant %s %s", g.Name, g.Why, g.Name, ns))
			eventNotes = append(eventNotes, notes[len(notes)-1])
		}
	}
	found := fmt.Sprintf("%d ScaledObjects registered, %d InferencePools", len(v.scaledObjects), len(v.pools))
	parts := []string{}
	state := StateOK
	switch {
	case len(problems) > 0:
		state = StateNotReady
		parts = append(parts, problems...)
	case len(v.scaledObjects) == 0:
		state = StateWaiting
		parts = append(parts, "no ScaledObject here has called the scaler yet (scaling-manager-owner.sh register "+ns+")")
	}
	eventParts := append(append([]string{}, parts...), eventNotes...)
	parts = append(parts, found)
	parts = append(parts, notes...)
	line = state + ": " + strings.Join(parts, "; ")
	if len(eventParts) == 0 {
		return line, state + ": managed, and its ScaledObjects call the scaler"
	}
	return line, state + ": " + strings.Join(eventParts, "; ")
}

// recordEvents records each namespace's event line (readiness) on its
// ScaledObjects and InferencePools when it changed for that object.
func (r *Reporter) recordEvents(views map[string]*nsView, readiness map[string]string) {
	if r.Recorder == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastEvent == nil {
		r.lastEvent = map[string]string{}
	}
	seen := map[string]bool{}
	for ns, msg := range readiness {
		v := views[ns]
		eventType, reason := corev1.EventTypeNormal, EventNamespaceReady
		switch {
		case strings.HasPrefix(msg, StateNotManaged):
			eventType, reason = corev1.EventTypeWarning, EventNamespaceNotManaged
		case strings.HasPrefix(msg, StateNotReady):
			eventType, reason = corev1.EventTypeWarning, EventNamespaceNotReady
		case strings.HasPrefix(msg, StateWaiting):
			reason = EventNamespaceWaiting
		}
		var refs []*corev1.ObjectReference
		for _, e := range v.scaledObjects {
			refs = append(refs, &corev1.ObjectReference{APIVersion: "keda.sh/v1alpha1", Kind: "ScaledObject",
				Namespace: ns, Name: e.Name, UID: e.Target.UID})
		}
		for _, so := range v.refusedSOs {
			refs = append(refs, &corev1.ObjectReference{APIVersion: "keda.sh/v1alpha1", Kind: "ScaledObject",
				Namespace: ns, Name: so})
		}
		if r.PoolAPIVersion != "" {
			for _, p := range v.pools {
				refs = append(refs, &corev1.ObjectReference{APIVersion: r.PoolAPIVersion, Kind: "InferencePool",
					Namespace: ns, Name: p.Name})
			}
		}
		for _, ref := range refs {
			key := ref.Kind + "/" + ns + "/" + ref.Name
			seen[key] = true
			if r.lastEvent[key] == msg {
				continue
			}
			r.lastEvent[key] = msg
			r.Recorder.Event(ref, eventType, reason, msg)
		}
	}
	for key := range r.lastEvent {
		if !seen[key] {
			delete(r.lastEvent, key)
		}
	}
}
