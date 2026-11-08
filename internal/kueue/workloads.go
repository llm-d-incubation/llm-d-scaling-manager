package kueue

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// WorkloadOwnerUIDField is the field index the Workload informer keeps on each
// Workload's owner UIDs, so the managed check looks a pod's owners up instead
// of reading every Workload of the namespace.
const WorkloadOwnerUIDField = "kueue.metadata.ownerReferences.uid"

// WorkloadSource lists Kueue Workloads, for the managed check, the preemption
// check and the lender-waiting signal.
//
// A failure is returned as the API reports it: a Kueue that does not serve
// Workloads fails with a NoMatch error and a denied read with Forbidden, which
// the callers read as "known: none" (fail closed, never as "absent").
type WorkloadSource interface {
	// Workloads lists the Workloads in ns, "" for every namespace the source
	// can see; with owner set, only those owned by owner.
	Workloads(ctx context.Context, ns string, owner types.UID) ([]unstructured.Unstructured, error)
	// Indexed reports whether a lookup by owner is served from an index, so
	// one per owner costs no API call. When false, a caller that needs many
	// owners of one namespace lists the namespace once instead.
	Indexed() bool
}

// defaultWorkloads is the source every caller that is handed none uses: the
// Workload informer, once the controller has wired one (UseWorkloadSource).
var defaultWorkloads atomic.Pointer[WorkloadSource]

// UseWorkloadSource makes s the source of every Workload read that is not
// handed one explicitly: the managed check of every quota entry and the
// preemption check. The controller sets the informer-backed source once at
// startup; nil restores reading from the API server.
func UseWorkloadSource(s WorkloadSource) {
	if s == nil {
		defaultWorkloads.Store(nil)
		return
	}
	defaultWorkloads.Store(&s)
}

// WorkloadsVia returns the Workload source to read through: the one
// UseWorkloadSource set, or a direct API read through c.
func WorkloadsVia(c client.Client) WorkloadSource {
	if s := defaultWorkloads.Load(); s != nil {
		return *s
	}
	return APIWorkloads{Client: c}
}

// APIWorkloads reads Workloads straight from the API server: unstructured,
// which the project's clients never cache, so each call is one LIST.
type APIWorkloads struct {
	Client client.Client
}

// Workloads implements WorkloadSource.
func (a APIWorkloads) Workloads(ctx context.Context, ns string, owner types.UID) ([]unstructured.Unstructured, error) {
	list, err := newWorkloadList(a.Client)
	if err != nil {
		return nil, err
	}
	var opts []client.ListOption
	if ns != "" {
		opts = append(opts, client.InNamespace(ns))
	}
	if err := a.Client.List(ctx, list, opts...); err != nil {
		return nil, err
	}
	return ownedBy(list.Items, owner), nil
}

// Indexed implements WorkloadSource: every call is a LIST.
func (APIWorkloads) Indexed() bool { return false }

// newWorkloadList is an empty Workload list at the version the cluster serves.
func newWorkloadList(c client.Client) (*unstructured.UnstructuredList, error) {
	gvk, err := workloadGVK(c)
	if err != nil {
		return nil, err
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk.GroupVersion().WithKind(KindWorkload + "List"))
	return list, nil
}

// workloadGVK resolves the Workload kind at the version the cluster serves.
func workloadGVK(c client.Client) (schema.GroupVersionKind, error) {
	mapping, err := c.RESTMapper().RESTMapping(schema.GroupKind{Group: Group, Kind: KindWorkload})
	if err != nil {
		return schema.GroupVersionKind{}, err
	}
	return mapping.GroupVersionKind, nil
}

// ownedBy keeps the Workloads owned by owner; all of them when owner is empty.
func ownedBy(items []unstructured.Unstructured, owner types.UID) []unstructured.Unstructured {
	if owner == "" {
		return items
	}
	out := items[:0:0]
	for i := range items {
		for _, ref := range items[i].GetOwnerReferences() {
			if ref.UID == owner {
				out = append(out, items[i])
				break
			}
		}
	}
	return out
}

// workloadOwnerUIDs is the WorkloadOwnerUIDField index function.
func workloadOwnerUIDs(o client.Object) []string {
	refs := o.GetOwnerReferences()
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		out = append(out, string(ref.UID))
	}
	return out
}

// InformerWorkloads reads Workloads from an informer of a cache, started on
// first use, with an index on owner UID. The cache must cover every namespace:
// the controller hands a namespace-scoped install a cluster-wide cache of its
// own (cmd/main.go), since the lender signal reads every namespace (D1). The
// informer starts only after a cluster-wide list has not been Forbidden.
// Until it has synced -- and for good, if it never does (no watch permission,
// say) -- every read goes to the API server instead, so a Forbidden or a
// missing Kueue surfaces as the error it is rather than as an empty cache.
// Once synced, the cluster-wide list is asked again every reprobeEvery: a
// grant revoked after the sync leaves the informer's reflector failing in the
// background while its store stays as it was, which would read as fresh
// forever (K2-4). A Forbidden probe sends every read back to the API server,
// whose Forbidden withholds borrowing as lenders-unreadable, until a later
// probe passes.
type InformerWorkloads struct {
	cache cache.Cache
	api   client.Client
	now   func() time.Time

	mu        sync.Mutex
	gvk       schema.GroupVersionKind
	informer  cache.Informer
	startedAt time.Time
	indexed   bool
	warned    bool
	// deniedAt is when a cluster-wide list was last Forbidden; the informer
	// is not started, or not read, until a later probe passes.
	deniedAt time.Time
	// probedAt is when the cluster-wide list was last asked and passed.
	probedAt time.Time
}

// reprobeEvery is how often a started informer's cluster-wide list is asked
// again, so a revoked grant is noticed within it rather than read as a cache
// that stopped changing.
const reprobeEvery = 5 * time.Minute

// deniedRetryAfter is how long a Forbidden probe holds the informer back
// before it is asked again: RBAC changes rarely.
const deniedRetryAfter = 5 * time.Minute

// NewInformerWorkloads returns a source over the manager's cache c; api
// resolves the Workload kind and serves the reads until the informer syncs.
func NewInformerWorkloads(c cache.Cache, api client.Client) *InformerWorkloads {
	return &InformerWorkloads{cache: c, api: api, now: time.Now}
}

// notSyncedWarnAfter is how long the informer may stay unsynced before that
// is logged: past it, every read is a LIST again.
const notSyncedWarnAfter = 2 * time.Minute

// synced starts the informer for the served Workload version if needed and
// reports whether it has synced, and whether the owner index is in place.
func (w *InformerWorkloads) synced(ctx context.Context) (schema.GroupVersionKind, bool, bool, error) {
	gvk, err := workloadGVK(w.api)
	if err != nil {
		return gvk, false, false, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	if w.informer == nil || w.gvk != gvk {
		if !w.deniedAt.IsZero() && w.now().Sub(w.deniedAt) < deniedRetryAfter {
			return gvk, false, false, nil // the API serves, and says Forbidden
		}
		// An informer that may not list never syncs, and its reflector
		// retries and logs for as long as the process runs: a
		// namespace-scoped install whose admin has not granted the
		// cluster-wide read (D1). One cheap list asks first.
		if w.denied(ctx, gvk) {
			return gvk, false, false, nil
		}
		inf, err := w.cache.GetInformer(ctx, obj, cache.BlockUntilSynced(false))
		if err != nil {
			return gvk, false, false, nil // the API serves meanwhile
		}
		w.informer, w.gvk, w.startedAt, w.indexed, w.warned = inf, gvk, w.now(), false, false
	} else if !w.deniedAt.IsZero() || w.now().Sub(w.probedAt) >= reprobeEvery {
		// Started: is the cluster-wide read still granted (K2-4)?
		if !w.deniedAt.IsZero() && w.now().Sub(w.deniedAt) < deniedRetryAfter {
			return gvk, false, false, nil
		}
		if w.denied(ctx, gvk) {
			kueueLogger(ctx).Info("The cluster-wide Kueue Workload read is Forbidden now; the informer is not read until it is granted again, and borrowing is withheld as lenders-unreadable",
				"recheckAfter", deniedRetryAfter.String())
			return gvk, false, false, nil
		}
	}
	if !w.informer.HasSynced() {
		if !w.warned && w.now().Sub(w.startedAt) > notSyncedWarnAfter {
			w.warned = true
			kueueLogger(ctx).Info("The Kueue Workload informer has not synced; Workloads are read from the API server on every check until it does. Does the controller have watch on workloads.kueue.x-k8s.io?",
				"waited", notSyncedWarnAfter.String())
		}
		return gvk, false, false, nil
	}
	if !w.indexed {
		// Synced, so IndexField returns at once; an index added after the
		// informer started is built from its store.
		if err := w.cache.IndexField(ctx, obj, WorkloadOwnerUIDField, workloadOwnerUIDs); err == nil {
			w.indexed = true
		}
	}
	return gvk, true, w.indexed, nil
}

// denied asks for one Workload cluster-wide and reports whether the API said
// Forbidden, recording when (deniedAt) or that it passed (probedAt). Any other
// answer is not a denial: the reads that follow report it. Callers hold w.mu.
func (w *InformerWorkloads) denied(ctx context.Context, gvk schema.GroupVersionKind) bool {
	probe := &unstructured.UnstructuredList{}
	probe.SetGroupVersionKind(gvk.GroupVersion().WithKind(KindWorkload + "List"))
	if err := w.api.List(ctx, probe, client.Limit(1)); apierrors.IsForbidden(err) {
		w.deniedAt = w.now()
		return true
	}
	w.deniedAt, w.probedAt = time.Time{}, w.now()
	return false
}

// Workloads implements WorkloadSource.
func (w *InformerWorkloads) Workloads(ctx context.Context, ns string, owner types.UID) ([]unstructured.Unstructured, error) {
	gvk, synced, indexed, err := w.synced(ctx)
	if err != nil {
		return nil, err
	}
	if !synced {
		return APIWorkloads{Client: w.api}.Workloads(ctx, ns, owner)
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk.GroupVersion().WithKind(KindWorkload + "List"))
	var opts []client.ListOption
	if ns != "" {
		opts = append(opts, client.InNamespace(ns))
	}
	if owner != "" && indexed {
		opts = append(opts, client.MatchingFields{WorkloadOwnerUIDField: string(owner)})
	}
	if err := w.cache.List(ctx, list, opts...); err != nil {
		return nil, err
	}
	return ownedBy(list.Items, owner), nil
}

// Indexed implements WorkloadSource: true once the informer has synced and
// carries the owner index.
func (w *InformerWorkloads) Indexed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.informer != nil && w.indexed && w.informer.HasSynced() && w.deniedAt.IsZero()
}
