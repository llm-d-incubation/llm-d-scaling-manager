// Package dsetwriter scales the Static roles of a DisaggregatedSet by writing
// spec.roles[].spec.replicas on the set itself.
//
// Every other scale target is actuated by KEDA through a /scale subresource.
// A DisaggregatedSet has none on the set, and a role has one only in
// scaling.mode External, through its DisaggregatedSetRoleScaler -- which LWS
// before v0.10.0 does not have at all. KEDA cannot target a Static role, so
// it never calls the scaler about one, and the role is never registered. This
// writer is the second path for such a role:
//
//   - it registers each Static role of a set that opts in (OptInAnnotation) in
//     the registry, as KEDA's calls register a ScaledObject, so the engines
//     size it like any other variant (target kind DisaggregatedSetRoleScaler,
//     name <set>-<role>, the name a role scaler would have);
//   - it reads the decisions the engines publish for those roles and applies
//     them to the set with one server-side apply, field manager FieldManager.
//
// What KEDA and the HPA do on the other path, the writer does here: it writes
// nothing for a passive model (the external scaler's resolver decides), and it
// holds a scale-down for the HPA's default stabilization window.
//
// A lower count is not done when it is written. The writer checks that the
// role's child LeaderWorkerSets follow it, and reports a scale-down the
// cluster refuses -- denied on the set, or on the children -- instead of
// lowering the role again (scaledown.go).
//
// External roles are never written: their count is the role scaler's, and the
// set controller ignores spec.replicas for them. KEDA plus the role scaler is
// the preferred path; this one exists for sets that cannot take it.
//
// Off unless DSET_ROLE_WRITER is true, and on the leader only. It needs patch
// on disaggregatedsets, which the install grants only when asked: make with
// DSET_ROLE_WRITER=true, smt with --dset-role-writer.
// See docs/reference/disaggregatedset.md.
package dsetwriter

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
)

// Annotations on a DisaggregatedSet that configure the writer for its Static
// roles. They take the place of the ScaledObject a KEDA-scaled role has.
const (
	// OptInAnnotation opts the set in: "true" writes when no other field
	// manager owns a role's replicas; "force" takes them over (see Writer.apply).
	OptInAnnotation = "scaling-manager.llmd.ai/role-writer"
	// ModelIDAnnotation is the model the set serves, as the engine reports it
	// (--served-model-name). Required.
	ModelIDAnnotation = "scaling-manager.llmd.ai/model-id"
	// MinReplicasAnnotation is the fewest groups of every Static role while
	// the model runs: an integer of 0 or more, default 1. A model parks at
	// zero whatever it says.
	MinReplicasAnnotation = "scaling-manager.llmd.ai/min-replicas"
	// MaxReplicasAnnotation is the most groups of every Static role: an
	// integer of 1 or more and at least the minimum, default 10.
	MaxReplicasAnnotation = "scaling-manager.llmd.ai/max-replicas"
	// ScalingPolicyAnnotation names the scaling policy tier, as the trigger's
	// scalingPolicy does. Optional.
	ScalingPolicyAnnotation = "scaling-manager.llmd.ai/scaling-policy"
)

// FieldManager is the server-side apply field manager the writer applies as.
const FieldManager = "scaling-manager-role-writer"

// EventScaled is the reason of the Normal event recorded on a set when the
// writer changes the replicas of its Static roles.
const EventScaled = "RoleReplicasWritten"

// EventConflict is the reason of the Warning event recorded when another
// field manager owns a role's replicas and the writer may not take them.
const EventConflict = "RoleReplicasConflict"

// EventRefused is the reason of the Warning event recorded when the writer
// will not scale an opted-in set: no model, more than one slice, or bad
// bounds.
const EventRefused = "RoleWriterRefused"

// EventFailed is the reason of the Warning event recorded when a write fails
// for any reason other than a conflict.
const EventFailed = "RoleReplicasWriteFailed"

// DefaultInterval is the time between two passes when Writer.Interval is
// unset.
const DefaultInterval = 15 * time.Second

// DefaultDecisionTTL is how long a positive decision speaks for its role when
// Writer.DecisionTTL is unset: the external scaler's rule.
const DefaultDecisionTTL = 3 * time.Minute

// DefaultScaleDownWindow is the scale-down stabilization window when
// Writer.ScaleDownWindow is unset: the HPA's default (behavior.scaleDown.
// stabilizationWindowSeconds 300), which a KEDA-scaled role gets.
const DefaultScaleDownWindow = 5 * time.Minute

const (
	defaultMin = 1
	defaultMax = 10
)

var setGVK = schema.FromAPIVersionAndKind(constants.DisaggregatedSetAPIVersion, constants.DisaggregatedSetKind)

var errNoPassiveResolver = errors.New("dsetwriter: Writer.Passive is nil")

// DecisionReader is the part of the decision store the writer reads.
type DecisionReader interface {
	Get(namespace, name string) (decision.Decision, bool)
}

// PassiveResolver reports whether a model is passive: passive.Resolver, the
// one the external scaler asks, so both paths agree.
type PassiveResolver interface {
	Model(namespace, modelID string) (bool, string)
}

// Writer registers and actuates the Static roles of opted-in sets.
type Writer struct {
	// Client reads (uncached) and applies the sets.
	Client client.Client
	// Registry is where the roles are registered, as KEDA calls register a
	// ScaledObject.
	Registry *registry.Registry
	// Decisions is the store the engines publish into.
	Decisions DecisionReader
	// Recorder records events on the set; may be nil.
	Recorder record.EventRecorder
	// Passive resolves passive mode. The roles of a passive model are
	// registered, so the engines publish their would-be counts, but never
	// written. Nil writes nothing at all: without it the writer cannot tell
	// a passive model, and writing one is the failure passive mode forbids.
	Passive PassiveResolver
	// Namespaces to scan. Empty scans none unless AllNamespaces is set.
	Namespaces []string
	// AllNamespaces scans every namespace, for a cluster-scoped install.
	AllNamespaces bool
	// NamespacesFunc, when set, returns the namespaces to scan on each pass,
	// in place of Namespaces and AllNamespaces: on the one-manifest install,
	// the managed namespaces that hold the per-namespace grant
	// (GrantedNamespaces), so the writer needs no patch on every set in the
	// cluster. An error skips the pass.
	NamespacesFunc func(ctx context.Context) ([]string, error)
	// Interval between passes, and DecisionTTL, past which a positive
	// decision no longer speaks for its role (the external scaler's rule).
	Interval, DecisionTTL time.Duration
	// ScaleDownWindow holds a scale-down to the highest count planned within
	// it, as the HPA's stabilization window does. Zero is
	// DefaultScaleDownWindow; negative is no window.
	ScaleDownWindow time.Duration
	// FollowTimeout is how long the child LeaderWorkerSets of a role may take
	// to follow a lower count before the scale-down is reported as refused.
	// Zero is DefaultFollowTimeout.
	FollowTimeout time.Duration
	// DeniedRetry is the first wait before a scale-down the API server denied
	// is asked for again; it doubles with each denial, up to MaxDeniedRetry.
	// Zero is DefaultDeniedRetry.
	DeniedRetry time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	mu sync.Mutex
	// history is the counts planned for each role within the scale-down
	// window, oldest first.
	history map[roleKey][]planned
	// warned is the last warning reason recorded on each set, so a set that
	// stays refused is told once, not every pass.
	warned map[types.NamespacedName]string
	// passive is each set's passive state as last logged.
	passive map[types.NamespacedName]bool
	// noResolverLogged is set once the missing Passive has been logged.
	noResolverLogged bool
	// listFailed is the last list failure logged for each namespace.
	listFailed map[string]string
	// scaleDowns is each role's lower count the writer wrote or tried, until
	// the cluster applies it or the count is raised (scaledown.go).
	scaleDowns map[roleKey]*scaleDown
	// childFailed is the last failed read of each set's children logged.
	childFailed map[types.NamespacedName]string
	// followed is every role whose children the writer has compared with
	// its count once, at first sight.
	followed map[roleKey]bool
}

type roleKey struct {
	set  types.NamespacedName
	role string
}

type planned struct {
	at time.Time
	n  int32
}

// NeedLeaderElection makes the writer run on the leader only.
func (w *Writer) NeedLeaderElection() bool { return true }

// Start runs a pass every Interval until ctx is done.
func (w *Writer) Start(ctx context.Context) error {
	interval := w.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	logger := ctrl.LoggerFrom(ctx).WithName("dset-role-writer")
	logger.Info("DisaggregatedSet role writer is on: Static roles of sets annotated "+OptInAnnotation+" are scaled by writing spec.roles[].spec.replicas",
		"namespaces", w.Namespaces, "allNamespaces", w.AllNamespaces, "interval", interval)
	ctx = ctrl.LoggerInto(ctx, logger)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		w.Pass(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Pass scans the sets once: registers their Static roles and applies the
// decisions published for them.
func (w *Writer) Pass(ctx context.Context) {
	namespaces := w.Namespaces
	if w.AllNamespaces {
		namespaces = []string{metav1.NamespaceAll}
	}
	if w.NamespacesFunc != nil {
		var err error
		if namespaces, err = w.NamespacesFunc(ctx); err != nil {
			ctrl.LoggerFrom(ctx).Error(err, "Cannot tell which namespaces hold the role writer's grant; skipping this pass")
			return
		}
	}
	seen := map[roleKey]bool{}
	for _, ns := range namespaces {
		if ctx.Err() != nil {
			return
		}
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(setGVK.GroupVersion().WithKind(constants.DisaggregatedSetKind + "List"))
		if err := w.Client.List(ctx, list, client.InNamespace(ns)); err != nil {
			w.listFailure(ctx, ns, err)
			continue
		}
		w.listFailure(ctx, ns, nil)
		for i := range list.Items {
			w.reconcile(ctx, &list.Items[i], seen)
		}
	}
	w.forget(seen)
}

// listFailure logs a failed list once per namespace and kind of failure, not
// every pass: a missing CRD at Info (nothing to scale), anything else, a
// missing grant say, as an error. A nil err clears the namespace.
func (w *Writer) listFailure(ctx context.Context, ns string, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.listFailed == nil {
		w.listFailed = map[string]string{}
	}
	if err == nil {
		delete(w.listFailed, ns)
		return
	}
	kind := string(apierrors.ReasonForError(err))
	if meta.IsNoMatchError(err) {
		kind = noMatch
	}
	// A plain error (a timeout) has no reason: kind "" must not match an
	// absent entry.
	if prev, logged := w.listFailed[ns]; logged && prev == kind {
		return
	}
	w.listFailed[ns] = kind
	logger := ctrl.LoggerFrom(ctx)
	switch {
	case kind == noMatch:
		logger.Info("The DisaggregatedSet CRD is not installed: the role writer has nothing to scale until it is", "namespace", ns)
	case apierrors.IsForbidden(err):
		logger.Error(err, "The role writer may not list DisaggregatedSets: it needs get, list and patch on them, which the install grants only when asked. "+
			"Release install: the cluster admin runs smt admin-setup -n <namespace> --dset-role-writer (a cluster-wide install: smt install --scope cluster --dset-role-writer). "+
			"make install: DSET_ROLE_WRITER=true make deploy-scaling-manager. Or turn the writer off: DSET_ROLE_WRITER=false", "namespace", ns)
	default:
		logger.Error(err, "Could not list DisaggregatedSets", "namespace", ns)
	}
}

// Role is one Static role of a set as the writer sees it.
type Role struct {
	// Name is the role's name, its key in spec.roles.
	Name string
	// Current is the role's spec.replicas: 1 when unset, the API default.
	Current int32
}

// StaticRoles returns the roles of set the writer may write, in spec order,
// and why none may be written when that is so: an External role is the role
// scaler's, a partitioned role's count is its sub-roles', and with slices
// above 1 a role's count is per slice while the engines size its total.
func StaticRoles(set *unstructured.Unstructured) ([]Role, string) {
	if slices, found, _ := unstructured.NestedInt64(set.Object, "spec", "slices"); found && slices > 1 {
		return nil, fmt.Sprintf("spec.slices is %d; the role writer scales only sets of one slice", slices)
	}
	roles, _, _ := unstructured.NestedSlice(set.Object, "spec", "roles")
	var out []Role
	for _, r := range roles {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		mode, _, _ := unstructured.NestedString(m, "scaling", "mode")
		subs, _, _ := unstructured.NestedSlice(m, "subRoles")
		if name == "" || mode == "External" || len(subs) > 0 {
			continue
		}
		replicas, found, _ := unstructured.NestedInt64(m, "spec", "replicas")
		if !found {
			replicas = 1
		}
		out = append(out, Role{Name: name, Current: clampInt32(replicas)})
	}
	return out, ""
}

// clampInt32 is n bounded to [0, MaxInt32]: a count the API stored as an
// int64 never wraps into a negative one.
func clampInt32(n int64) int32 {
	return int32(min(max(n, 0), math.MaxInt32))
}

// Plan returns the replicas to write for roles, given the decision for each
// role that has one, clamped to [lo, hi]. A role with no decision keeps its
// count.
//
// It honours the DisaggregatedSet rule that every Static role is above zero
// or every one is zero (a CEL rule on the spec): a role is zeroed only when
// every role would be. Otherwise a role decided down to zero keeps
// max(lo, 1) groups, so one role is never parked alone -- the set would
// reject the write, and a prefill with no decode serves nothing.
func Plan(roles []Role, decided map[string]int32, lo, hi int32) map[string]int32 {
	out := make(map[string]int32, len(roles))
	allZero := len(roles) > 0
	for _, r := range roles {
		n := r.Current
		if d, ok := decided[r.Name]; ok {
			n = d
			if n > 0 {
				n = min(max(n, lo), hi)
			}
		}
		out[r.Name] = n
		if n > 0 {
			allZero = false
		}
	}
	if !allZero {
		for name, n := range out {
			if n <= 0 {
				out[name] = max(lo, 1)
			}
		}
	}
	return out
}

// ApplyConfiguration is the server-side apply document that sets the replicas
// of the named roles of set and nothing else. spec.roles is a list map keyed
// by name, so the apply owns only spec.roles[name=<role>].spec.replicas of
// each role it names.
//
// The roles are listed in set's own order. A server-side apply orders a list
// map's items as the applied document lists them, and LWS before v1.0.0
// hashes spec.roles in order into the set's revision: an apply that listed
// them in another order would roll every group of every role.
func ApplyConfiguration(set *unstructured.Unstructured, replicas map[string]int32) *unstructured.Unstructured {
	spec, _, _ := unstructured.NestedSlice(set.Object, "spec", "roles")
	roles := make([]any, 0, len(replicas))
	for _, r := range spec {
		m, _ := r.(map[string]any)
		name, _ := m["name"].(string)
		if n, ok := replicas[name]; ok {
			roles = append(roles, map[string]any{"name": name, "spec": map[string]any{"replicas": int64(n)}})
		}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": constants.DisaggregatedSetAPIVersion,
		"kind":       constants.DisaggregatedSetKind,
		"metadata":   map[string]any{"name": set.GetName(), "namespace": set.GetNamespace()},
		"spec":       map[string]any{"roles": roles},
	}}
}

// TargetName is the scale-target name of role in set: <set>-<role>.
func TargetName(set, role string) string { return set + "-" + role }

// EntryName is the registry entry of role in set, as a ScaledObject the plan
// would write for it is named: <set>-<role>-llmsm.
func EntryName(set, role string) string { return TargetName(set, role) + "-llmsm" }

// Bounds reads the min and max annotations of set: the defaults when absent,
// and an error naming the annotation when a value is not a count the writer
// can use -- not an integer, a negative minimum, a maximum below 1, or a
// minimum above the maximum.
func Bounds(set *unstructured.Unstructured) (lo, hi int32, err error) {
	lo, err = annotationInt(set, MinReplicasAnnotation, defaultMin, 0)
	if err != nil {
		return 0, 0, err
	}
	hi, err = annotationInt(set, MaxReplicasAnnotation, defaultMax, 1)
	if err != nil {
		return 0, 0, err
	}
	if lo > hi {
		return 0, 0, fmt.Errorf("%s %d is above %s %d", MinReplicasAnnotation, lo, MaxReplicasAnnotation, hi)
	}
	return lo, hi, nil
}

func (w *Writer) reconcile(ctx context.Context, set *unstructured.Unstructured, seen map[roleKey]bool) {
	mode := set.GetAnnotations()[OptInAnnotation]
	if mode != "true" && mode != "force" {
		return
	}
	// A set being deleted is the garbage collector's: writing it would only
	// race the delete, and registering it would size a model that is going.
	if set.GetDeletionTimestamp() != nil {
		return
	}
	key := types.NamespacedName{Namespace: set.GetNamespace(), Name: set.GetName()}
	logger := ctrl.LoggerFrom(ctx).WithValues("namespace", set.GetNamespace(), "disaggregatedSet", set.GetName())
	model := set.GetAnnotations()[ModelIDAnnotation]
	if model == "" {
		w.warn(set, EventRefused, "no "+ModelIDAnnotation+" annotation: the role writer cannot attribute this set to a model")
		return
	}
	roles, why := StaticRoles(set)
	if why != "" {
		w.warn(set, EventRefused, why)
		return
	}
	lo, hi, err := Bounds(set)
	if err != nil {
		w.warn(set, EventRefused, err.Error()+": the role writer neither registers nor writes this set until it is fixed")
		return
	}

	decided := map[string]int32{}
	for _, r := range roles {
		seen[roleKey{key, r.Name}] = true
		w.register(set, r.Name, model, lo, hi)
		if d, ok := w.Decisions.Get(set.GetNamespace(), TargetName(set.GetName(), r.Name)); ok && w.honours(d) {
			decided[r.Name] = d.DesiredReplicas
		}
	}
	if w.passiveModel(key, model, logger) {
		// The external scaler holds a passive model's count; here, holding
		// is writing nothing. The window starts afresh when it turns active.
		w.dropHistory(key)
		w.clearWarning(key)
		return
	}
	held := w.followScaleDowns(ctx, set, roles, logger)
	stable := w.stabilize(key, roles, Plan(roles, decided, lo, hi))
	w.settleDenied(key, roles, stable)
	plan := holdRefused(roles, stable, held, lo)
	for role, why := range held {
		logger.V(1).Info("Holding a role at its count: the cluster refused a lower one", "role", role, "reason", why)
	}
	changed := map[string]int32{}
	for _, r := range roles {
		if plan[r.Name] != r.Current {
			changed[r.Name] = plan[r.Name]
		}
	}
	if len(changed) == 0 {
		w.clearWarning(key)
		return
	}
	// Every Static role is written together, so the CEL rule is checked
	// against one consistent document, and a rejected write leaves no role
	// half-applied.
	opts := []client.ApplyOption{client.FieldOwner(FieldManager)}
	if mode == "force" {
		opts = append(opts, client.ForceOwnership)
	}
	if err := w.Client.Apply(ctx, client.ApplyConfigurationFromUnstructured(ApplyConfiguration(set, plan)), opts...); err != nil {
		if apierrors.IsConflict(err) {
			w.warn(set, EventConflict,
				"another field manager owns spec.roles[].spec.replicas. A create or client-side apply (kubectl, Helm, Argo CD without ServerSideApply) owns them even when its manifest leaves them out. "+
					"Apply the set server-side without replicas, or set "+OptInAnnotation+"=force: "+err.Error())
			return
		}
		if isDenial(err) {
			if lowered := w.recordDenied(key, roles, plan, err.Error()); len(lowered) > 0 {
				// A scale-down the cluster gates (a drain first, say): bounded
				// retries, and the roles it did not lower go on alone.
				logger.Info("The API server denied a role scale-down; the role writer holds those roles and asks again later",
					"roles", lowered, "replicas", changed, "error", err.Error())
				w.event(set, corev1.EventTypeWarning, EventScaleDownRefused,
					fmt.Sprintf("the API server denied writing role replicas %v, which lowers %v: %v. "+
						"The writer holds those roles at their count, writes any other change without them, and asks again later",
						changed, lowered, err))
				return
			}
		}
		logger.Error(err, "Could not write the role replicas", "replicas", plan)
		w.warn(set, EventFailed, fmt.Sprintf("could not write role replicas %v: %v", changed, err))
		return
	}
	w.recordWrite(key, roles, plan)
	logger.Info("Wrote DisaggregatedSet role replicas", "replicas", changed)
	w.clearWarning(key)
	w.event(set, corev1.EventTypeNormal, EventScaled, fmt.Sprintf("role replicas %v", changed))
}

// passiveModel reports whether model is passive, and logs a change of that
// for the set once. With no resolver every model counts as passive, and that
// is logged once, as an error: the writer cannot tell which models it may
// scale, so it scales none.
func (w *Writer) passiveModel(key types.NamespacedName, model string, logger logr.Logger) bool {
	if w.Passive == nil {
		w.mu.Lock()
		logged := w.noResolverLogged
		w.noResolverLogged = true
		w.mu.Unlock()
		if !logged {
			logger.Error(errNoPassiveResolver, "The role writer writes no replicas: it has no passive resolver, so it cannot tell which models are passive")
		}
		return true
	}
	p, source := w.Passive.Model(key.Namespace, model)
	w.mu.Lock()
	if w.passive == nil {
		w.passive = map[types.NamespacedName]bool{}
	}
	prev := w.passive[key]
	w.passive[key] = p
	w.mu.Unlock()
	if prev != p {
		if p {
			logger.Info("Passive: the role writer registers this set's Static roles but writes no replicas", "model", model, "source", source)
		} else {
			logger.Info("Passive mode left: the role writer writes this set's Static roles again", "model", model, "source", source)
		}
	}
	return p
}

// stabilize holds each role's scale-down to the highest count planned for it
// within the scale-down window, as the HPA does, and records plan. A role
// seen for the first time counts its current replicas as planned now, so a
// restart does not skip the window.
//
// The all-zero-or-all-non-zero rule survives: every plan recorded obeys it,
// all roles of a set are recorded at the same instants, so the highest of
// each role over one window is zero for all of them or for none.
func (w *Writer) stabilize(set types.NamespacedName, roles []Role, plan map[string]int32) map[string]int32 {
	window := w.ScaleDownWindow
	if window == 0 {
		window = DefaultScaleDownWindow
	}
	if window < 0 {
		return plan
	}
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.history == nil {
		w.history = map[roleKey][]planned{}
	}
	out := make(map[string]int32, len(plan))
	for _, r := range roles {
		k := roleKey{set, r.Name}
		h := w.history[k]
		if len(h) == 0 {
			h = append(h, planned{now, r.Current})
		}
		h = append(h, planned{now, plan[r.Name]})
		i := 0
		for i < len(h)-1 && now.Sub(h[i].at) > window {
			i++
		}
		h = h[i:]
		w.history[k] = h
		n := plan[r.Name]
		if n < r.Current {
			highest := n
			for _, p := range h {
				highest = max(highest, p.n)
			}
			n = min(highest, r.Current)
		}
		out[r.Name] = n
	}
	return out
}

// dropHistory forgets the scale-down history and records of every role of
// set.
func (w *Writer) dropHistory(set types.NamespacedName) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for k := range w.history {
		if k.set == set {
			delete(w.history, k)
		}
	}
	for k := range w.scaleDowns {
		if k.set == set {
			delete(w.scaleDowns, k)
		}
	}
	for k := range w.followed {
		if k.set == set {
			delete(w.followed, k)
		}
	}
}

// forget drops the history of the roles a pass did not see: sets deleted or
// opted out.
func (w *Writer) forget(seen map[roleKey]bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for k := range w.history {
		if !seen[k] {
			delete(w.history, k)
		}
	}
	for k := range w.scaleDowns {
		if !seen[k] {
			delete(w.scaleDowns, k)
		}
	}
	for k := range w.followed {
		if !seen[k] {
			delete(w.followed, k)
		}
	}
	sets := map[types.NamespacedName]bool{}
	for k := range seen {
		sets[k.set] = true
	}
	for set := range w.childFailed {
		if !sets[set] {
			delete(w.childFailed, set)
		}
	}
}

// register records role of set in the registry, as a KEDA call records a
// ScaledObject, and sets its target. Repeated every pass, it keeps the entry
// live; a set that opts out simply stops refreshing it and it expires.
func (w *Writer) register(set *unstructured.Unstructured, role, model string, lo, hi int32) {
	if w.Registry == nil {
		return
	}
	md := map[string]string{
		registry.ModelIDKey: model,
		registry.SourceKey:  registry.SourceRoleWriter,
	}
	if p := set.GetAnnotations()[ScalingPolicyAnnotation]; p != "" {
		md[registry.ScalingPolicyKey] = p
	}
	ns, name := set.GetNamespace(), EntryName(set.GetName(), role)
	w.Registry.Observe(ns, name, md)
	w.Registry.SetTarget(ns, name, registry.Target{
		APIVersion:  constants.DisaggregatedSetAPIVersion,
		Kind:        constants.DisaggregatedSetRoleScalerKind,
		Name:        TargetName(set.GetName(), role),
		MinReplicas: &lo,
		MaxReplicas: &hi,
		Labels:      set.GetLabels(),
	})
}

// honours applies the external scaler's staleness rule: a decision to park
// (0) never expires, a positive one only speaks for its role while fresh.
func (w *Writer) honours(d decision.Decision) bool {
	if d.DesiredReplicas <= 0 {
		return true
	}
	ttl := w.DecisionTTL
	if ttl <= 0 {
		ttl = DefaultDecisionTTL
	}
	return w.now().Sub(d.UpdatedAt) <= ttl
}

func (w *Writer) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// warn records a Warning event on set, unless the last warning recorded on
// it had the same reason and nothing has gone right since: a set that stays
// refused or in conflict is told once, not every pass.
func (w *Writer) warn(set *unstructured.Unstructured, reason, msg string) {
	key := types.NamespacedName{Namespace: set.GetNamespace(), Name: set.GetName()}
	w.mu.Lock()
	if w.warned == nil {
		w.warned = map[types.NamespacedName]string{}
	}
	same := w.warned[key] == reason
	w.warned[key] = reason
	w.mu.Unlock()
	if !same {
		w.event(set, corev1.EventTypeWarning, reason, msg)
	}
}

// clearWarning forgets the last warning on set: the pass went right, so the
// next warning is news.
func (w *Writer) clearWarning(key types.NamespacedName) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.warned, key)
}

func (w *Writer) event(set *unstructured.Unstructured, kind, reason, msg string) {
	if w.Recorder != nil {
		w.Recorder.Event(set, kind, reason, msg)
	}
}

// annotationInt reads annotation key of set as a count of at least floor:
// def when absent, an error when present and not such a count.
func annotationInt(set *unstructured.Unstructured, key string, def, floor int32) (int32, error) {
	v, ok := set.GetAnnotations()[key]
	if !ok {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil || n < int64(floor) {
		return 0, fmt.Errorf("%s is %q, not an integer of %d or more", key, v, floor)
	}
	return int32(n), nil
}
