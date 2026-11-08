// Package kueue reads GPU quotas from Kueue and presents them in the shape the
// quota limiter's static maps use, so a quota entry can be bounded by what Kueue
// would admit.
//
// It reads three kinds through the API server, unstructured, so the project
// carries no Kueue module dependency and follows whichever API version the
// cluster serves (the quota fields used are identical across v1beta1 and
// v1beta2; the cohort is spec.cohort in v1beta1 and spec.cohortName in v1beta2,
// and both are read):
//
//   - ResourceFlavor (cluster-scoped): spec.nodeLabels names the accelerator
//     product, e.g. nvidia.com/gpu.product: NVIDIA-H100-80GB-HBM3.
//   - ClusterQueue (cluster-scoped): spec.resourceGroups[].flavors[].resources[]
//     carries nominalQuota per flavor per extended resource.
//   - LocalQueue (namespaced): spec.clusterQueue links a namespace to a
//     ClusterQueue. The link counts only when the ClusterQueue's
//     spec.namespaceSelector admits that namespace, which may need a get of
//     the Namespace for its labels.
//
// The same read also takes what cohort borrowing depends on (borrow.go): each
// ClusterQueue's cohort, lending and borrowing limits, reclaim policy and
// reservation status, each ResourceFlavor's tolerations, and the Cohort
// objects, listed to tell a flat cohort from a nested one. From them it
// publishes each namespace's grant at the guaranteed and within-nominal figures
// and the cohorts' free pools, which a quota entry that counts borrowing applies
// where the engine-side conditions hold (eligibility.go). A failure to read the
// Cohort objects never fails the read.
//
// Nothing here is watched. Reads happen on demand, at most once per refresh
// interval, and the last snapshot is kept so a failed read leaves the previous
// figures in force rather than none.
package kueue

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/accelerator"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
)

// Options configures a Reader.
type Options struct {
	// Resources are the extended-resource names that count as GPUs. Empty means
	// every constants.VendorResources resource name.
	Resources []string

	// RefreshInterval is the minimum time between two reads of the API; a read
	// inside the interval returns the last snapshot. Non-positive means every
	// call reads.
	RefreshInterval time.Duration

	// Namespace, when set, restricts the LocalQueue listing to that namespace,
	// or to each of a comma-separated list of them (a controller that watches
	// several). A namespace-scoped controller can list LocalQueues nowhere else, and a
	// cluster-wide list from it would be Forbidden — an error, not an empty
	// result (Forbidden is not absent). ClusterQueues and ResourceFlavors are
	// cluster-scoped and are always listed cluster-wide. Cohort borrowing
	// reads Workloads and their LocalQueues cluster-wide whatever this says
	// (readPending), through the grant of components/kueue-reader.
	Namespace string

	// ControllerNamespace is where the Kueue controller's Deployment is read
	// for its version; empty means DefaultControllerNamespace.
	ControllerNamespace string

	// AssumeTestedVersion counts borrowing under a controller whose image
	// names no version, as if it were tested (kueue.assumeTestedVersion). It
	// applies only to a version that cannot be read from a Deployment that
	// can: a Deployment that cannot be read is kueue-controller-down.
	AssumeTestedVersion bool

	// CountsBorrowing is set when the quota entry the reader serves counts
	// cohort borrowing: only then does it publish
	// llmsm_kueue_controller_version_known (R2).
	CountsBorrowing bool

	// Entry names the quota entry the reader serves, for the metrics it
	// publishes. Not part of the options' identity.
	Entry string `json:"-"`

	// Workloads, when set, is where Workloads are read; nil reads through
	// WorkloadsVia at the time of the read. Not part of the options' identity.
	Workloads WorkloadSource `json:"-"`

	// now is the clock; nil means time.Now. Tests inject one.
	now func() time.Time
}

// Reader reads Kueue quotas and caches the last snapshot.
type Reader struct {
	client   client.Client
	opts     Options
	gpuNames map[string]bool

	mu      sync.Mutex
	last    config.ExternalQuotas // ObservedAt zero until the first successful read
	lastTry time.Time
	lastErr error
	// warnedLabels is set once an unreadable Namespace has been logged, so a
	// missing grant is reported once rather than every refresh.
	warnedLabels bool
	// warnedCohorts is set once a failed Cohort list has been logged.
	warnedCohorts bool
	// nsListDenied is set once a Namespace LIST was Forbidden (a
	// namespace-scoped install): labels are then read with one GET per
	// namespace, as that RBAC allows, for the life of the Reader.
	nsListDenied bool
	// warnedNoStatus holds the cohort members logged as having no status.
	warnedNoStatus map[string]bool
	// warnedPending is set once a failed Workload read for the lender signal
	// has been logged.
	warnedPending bool
	// version is the Kueue controller's version and liveness as last read, at
	// versionAt. deploymentDeniedAt and leaseDeniedAt are when a get of the
	// Deployment or of the Lease was last Forbidden, each kept on its own
	// (G2): a Forbidden Deployment does not stop the Lease being read.
	version            ControllerVersion
	versionAt          time.Time
	deploymentDeniedAt time.Time
	leaseDeniedAt      time.Time
}

// NewReader constructs a Reader over the given client. Reads of unstructured
// objects through a controller-runtime client go to the API server directly
// unless the manager was told to cache unstructured kinds, which this project
// does not do — so no informer is started for Kueue objects.
func NewReader(c client.Client, opts Options) *Reader {
	names := make(map[string]bool, len(opts.Resources))
	for _, r := range opts.Resources {
		names[r] = true
	}
	if len(names) == 0 {
		for _, v := range constants.VendorResources {
			names[v.ResourceName] = true
		}
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	return &Reader{client: c, opts: opts, gpuNames: names}
}

// Quotas returns the current snapshot, reading Kueue if the last read is older
// than the refresh interval. On a failed read it returns the last good snapshot
// together with the error; ObservedAt on the result tells the caller how stale
// that snapshot is, and is zero when no read has ever succeeded — in which case
// the snapshot is empty and must not be taken as "Kueue grants nothing".
func (r *Reader) Quotas(ctx context.Context) (config.ExternalQuotas, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.opts.now()
	if !r.lastTry.IsZero() && r.opts.RefreshInterval > 0 && now.Sub(r.lastTry) < r.opts.RefreshInterval {
		return r.last, r.lastErr
	}
	quotas, err := r.read(ctx)
	if err != nil {
		if ctx.Err() != nil {
			// The CALLER ran out of time, not the source. One reader serves
			// both engines; recording this would hand the other engine a
			// "deadline exceeded" it did not incur for the rest of the
			// interval. Nothing is recorded; the next caller reads afresh.
			return r.last, err
		}
		r.lastTry, r.lastErr = now, err
		return r.last, err
	}
	quotas.ObservedAt = now
	r.lastTry, r.last, r.lastErr = now, quotas, nil
	return quotas, nil
}

// read performs one full read: flavors → type names, ClusterQueues → per-type
// caps, LocalQueues → namespace attribution. Any failure aborts the whole read;
// a partial snapshot (say, queues without their flavors) would mis-name every
// type and deny everything, which is worse than keeping the previous one.
func (r *Reader) read(ctx context.Context) (config.ExternalQuotas, error) {
	flavors, err := r.list(ctx, KindResourceFlavor)
	if err != nil {
		return config.ExternalQuotas{}, err
	}
	flavorType := make(map[string]string, len(flavors))
	topo := Topology{
		Queues:  make(map[string]QueueState),
		Flavors: make(map[string]FlavorPlacement, len(flavors)),
		Now:     r.opts.now(),
	}
	for i := range flavors {
		flavorType[flavors[i].GetName()] = flavorAcceleratorType(&flavors[i])
		topo.Flavors[flavors[i].GetName()] = flavorPlacement(&flavors[i])
	}

	queues, err := r.list(ctx, KindClusterQueue)
	if err != nil {
		return config.ExternalQuotas{}, err
	}
	perQueue := make(map[string]config.ExternalCaps, len(queues))
	admits := make(map[string]namespaceAdmission, len(queues))
	var cluster config.ExternalCaps
	for i := range queues {
		// Every queue, GPU or not: a cohort member without status, or with Fair
		// Sharing on, withholds borrowing for the whole cohort.
		topo.Queues[queues[i].GetName()] = r.queueState(&queues[i], flavorType)
		caps, err := r.clusterQueueCaps(&queues[i], flavorType)
		if err != nil {
			return config.ExternalQuotas{}, err
		}
		if caps.IsZero() {
			continue // declares no GPU resource: this queue governs no accelerator
		}
		perQueue[queues[i].GetName()] = caps
		admits[queues[i].GetName()] = clusterQueueAdmission(&queues[i])
		cluster = addCaps(cluster, caps)
	}

	localQueues, err := r.listLocalQueues(ctx)
	if err != nil {
		return config.ExternalQuotas{}, err
	}
	// A namespace with two LocalQueues on one ClusterQueue has that queue's
	// budget once, not twice: the budget belongs to the ClusterQueue.
	seen := make(map[string]map[string]bool)
	byNamespace := make(map[string]config.ExternalCaps)
	// The GPU-declaring queues each namespace is granted, for the borrowing
	// figures.
	nsQueues := make(map[string][]string)
	// Namespaces whose labels could not be read, so a selector-bearing queue's
	// grant to them could be neither confirmed nor ruled out.
	withheld := make(map[string]bool)
	nsLabels := make(map[string]nsRead)
	for i := range localQueues {
		lq := &localQueues[i]
		ns := lq.GetNamespace()
		cqName, _, _ := unstructured.NestedString(lq.Object, "spec", "clusterQueue")
		if cqName == "" {
			continue
		}
		if seen[ns] == nil {
			seen[ns] = make(map[string]bool)
		}
		if seen[ns][cqName] {
			continue
		}
		seen[ns][cqName] = true
		if _, ok := perQueue[cqName]; !ok {
			// A dangling reference, or a queue that declares no GPU resource at
			// all (a CPU batch queue). Neither says anything about GPUs, so the
			// namespace is left ungoverned here and the static entry decides.
			// Listing it with nothing granted would instead zero every inference
			// namespace on a cluster that uses Kueue for CPU jobs. A queue that
			// DOES declare a GPU resource with nominalQuota 0 is a real grant of
			// nothing and is kept.
			continue
		}
		// A LocalQueue is something a tenant creates in their own namespace,
		// pointing at any ClusterQueue they like. It carries that queue's grant
		// only where Kueue would admit through it: where the ClusterQueue's
		// namespaceSelector selects the namespace. A queue that does not admit
		// the namespace says nothing about it, like a dangling reference.
		admitted, err := r.admitsNamespace(ctx, admits[cqName], ns, nsLabels)
		if err != nil {
			var w *withheldError
			if !errors.As(err, &w) {
				// Anything but Forbidden or NotFound — a timeout, a throttled or
				// unavailable API server, a cancelled context — says nothing
				// about the grant. Fail the read so the previous snapshot stays
				// in force, rather than publishing one that bounds the namespace
				// at nothing over a transient blip.
				return config.ExternalQuotas{}, err
			}
			// Neither confirmed nor ruled out, so not granted. Nor left to the
			// static entry: with the recommended `default: {H100: -1}` that is
			// no bound at all, more than the queue could ever have granted.
			withheld[ns] = true
			if !r.warnedLabels {
				r.warnedLabels = true
				kueueLogger(ctx).Error(err,
					"Cannot read a Namespace to evaluate a ClusterQueue namespaceSelector; withholding that queue's grant from the namespace, which is bounded at nothing until the read succeeds. Logged once until a later read succeeds.",
					"namespace", ns, "clusterQueue", cqName,
					"remedy", w.remedy())
			}
			continue
		}
		if !admitted {
			continue
		}
		nsQueues[ns] = append(nsQueues[ns], cqName)
	}
	for ns, names := range nsQueues {
		byNamespace[ns] = namespaceCaps(names, perQueue)
	}
	cohorts, unreadable, cohortsStale := r.cohorts(ctx)
	// A transient failure is the stale freeze, not an unknown hierarchy: the
	// withheld reason would otherwise win over it (Precedence) and drop every
	// counted namespace to nominal over a blip.
	topo.Cohorts, topo.CohortsUnreadable = cohorts, unreadable
	controllerDown := false
	var classPriority, highest map[string]int32
	if slices.ContainsFunc(slices.Collect(maps.Values(topo.Queues)), func(q QueueState) bool { return q.Cohort != "" }) {
		// Only borrowing depends on the controller (its version, whether it
		// runs) and on the Workloads, and only a queue in a cohort can borrow
		// or lend: a cluster without cohorts costs neither read.
		version := r.controllerVersion(ctx)
		topo.VersionUntested = version.Known && !version.Tested
		// A Deployment read whose image names no version is unknown, and
		// withheld unless the operator vouches for it. One that could not be
		// read is not unknown but down (version.Down), which the override
		// does not lift.
		topo.VersionUnknown = !version.Down && !version.Known && !r.opts.AssumeTestedVersion
		controllerDown = version.Down
		// Known as the borrowing reasons read it: an image that names no
		// version is known once the operator vouches for it (R2).
		metrics.PublishKueueVersionKnown(r.opts.Entry, version.Known || r.opts.AssumeTestedVersion, r.opts.CountsBorrowing)
		// The Workloads are read cluster-wide only for an entry that counts
		// borrowing (M-m2): only borrowing reads a lender's wait or a
		// priority, and an install without the cluster-wide grant would
		// otherwise make a Forbidden list on every refresh for nothing.
		var pending pendingRead
		if r.opts.CountsBorrowing {
			pending, topo.LendersUnreadable = r.readPending(ctx, localQueues)
			topo.WorkloadsRead = !topo.LendersUnreadable
		}
		classPriority, highest = pending.classPriority, pending.highestByNamespace
		for name, q := range topo.Queues {
			q.PendingWorkloads = pending.byQueue[name]
			if p, ok := pending.lenderPriority[name]; ok {
				q.LenderPriority = &p
			}
			if p, ok := pending.borrowerPriority[name]; ok {
				q.BorrowerPriority = &p
			}
			if p, ok := pending.lowestReserved[name]; ok {
				q.LowestReserved = &p
			}
			q.Evicting = pending.evicting[name]
			topo.Queues[name] = q
		}
	} else {
		// No cohort, so the controller was not checked: the answer of an
		// earlier read with cohorts no longer holds, and nothing borrows.
		metrics.PublishKueueVersionKnown(r.opts.Entry, false, false)
	}
	// A read cut short by the caller is not a snapshot, even when every call
	// it made happened to return: a client that ignores ctx (or one that turned
	// the cancellation into some other error) must not cache what it saw.
	if err := ctx.Err(); err != nil {
		return config.ExternalQuotas{}, fmt.Errorf("kueue: read interrupted: %w", err)
	}
	for ns := range withheld {
		if _, ok := byNamespace[ns]; !ok {
			byNamespace[ns] = config.ExternalCaps{HasUntyped: true} // governed, granted nothing
		}
	}
	if len(withheld) == 0 && labelsRead(nsLabels) {
		// Every Namespace this read needed was readable, so a later revoke of
		// the grant is news again and is logged again.
		r.warnedLabels = false
	}
	r.warnNoStatus(ctx, topo)
	metrics.PublishKueueBorrowDrift(BorrowDrift(topo))
	borrowing := ComputeBorrowing(topo, cohortsStale)
	guaranteed, within, err := r.borrowCaps(queues, flavorType, borrowing, nsQueues, byNamespace)
	if err != nil {
		return config.ExternalQuotas{}, err
	}
	return config.ExternalQuotas{
		Namespace:                byNamespace,
		Cluster:                  cluster,
		Borrow:                   borrowRows(borrowing, topo, flavorType, nsQueues),
		NamespaceGuaranteed:      guaranteed,
		NamespaceWithinNominal:   within,
		SharedPools:              sharedPools(borrowing, flavorType),
		BorrowStale:              cohortsStale,
		KueueControllerDown:      controllerDown,
		ClassPriority:            classPriority,
		NamespaceHighestPriority: highest,
	}, nil
}

// kueueLogger is the logger every message of this package goes through.
func kueueLogger(ctx context.Context) logr.Logger {
	return ctrl.LoggerFrom(ctx).WithName("kueue")
}

// warnNoStatus logs, once per queue, each cohort member Kueue has written no
// reservation status for: it withholds borrowing for its whole cohort
// (no-status), and the borrowing line names only the reason. A queue that
// gains status is forgotten, so losing it again is logged again. Callers must
// hold r.mu.
func (r *Reader) warnNoStatus(ctx context.Context, t Topology) {
	for name, q := range t.Queues {
		switch {
		case q.Cohort == "" || q.HasStatus:
			delete(r.warnedNoStatus, name)
		case !r.warnedNoStatus[name]:
			if r.warnedNoStatus == nil {
				r.warnedNoStatus = map[string]bool{}
			}
			r.warnedNoStatus[name] = true
			kueueLogger(ctx).Info("A ClusterQueue in a cohort has no status.flavorsReservation; borrowing is not counted in its cohort (reason no-status). Is the Kueue controller running? Logged once per queue.",
				"clusterQueue", name, "cohort", q.Cohort)
		}
	}
	for name := range r.warnedNoStatus {
		if _, ok := t.Queues[name]; !ok {
			delete(r.warnedNoStatus, name)
		}
	}
}

// borrowCaps builds, per namespace, the grant at each queue's guaranteed figure
// and at its within-nominal figure, by exactly the rules the nominal grant is
// built by (clusterQueueCaps): a namespace gets the sum over the queues it is
// granted, and one whose grant was withheld keeps its grant of nothing. A queue
// whose borrowing is withheld has both figures at nominal, so for it the three
// grants agree.
func (r *Reader) borrowCaps(queues []unstructured.Unstructured, flavorType map[string]string, b Borrowing,
	nsQueues map[string][]string, nominal map[string]config.ExternalCaps) (guaranteed, within map[string]config.ExternalCaps, err error) {
	byQueueG := make(map[string]config.ExternalCaps, len(queues))
	byQueueW := make(map[string]config.ExternalCaps, len(queues))
	for i := range queues {
		name := queues[i].GetName()
		figs := make(map[ResourceKey]QueueFigures, len(b.Queues[name]))
		for _, f := range b.Queues[name] {
			figs[f.ResourceKey] = f
		}
		at := func(pick func(QueueFigures) int) func(ResourceKey, int) int {
			return func(key ResourceKey, nominal int) int {
				if f, ok := figs[key]; ok {
					return pick(f)
				}
				return nominal
			}
		}
		if byQueueG[name], err = r.clusterQueueCapsAt(&queues[i], flavorType, at(func(f QueueFigures) int { return f.Guaranteed })); err != nil {
			return nil, nil, err
		}
		if byQueueW[name], err = r.clusterQueueCapsAt(&queues[i], flavorType, at(func(f QueueFigures) int { return f.WithinNominal })); err != nil {
			return nil, nil, err
		}
	}
	guaranteed = make(map[string]config.ExternalCaps, len(nominal))
	within = make(map[string]config.ExternalCaps, len(nominal))
	for ns := range nominal {
		names := nsQueues[ns]
		if len(names) == 0 {
			// Governed but granted nothing (a withheld grant): the same nothing.
			guaranteed[ns], within[ns] = nominal[ns], nominal[ns]
			continue
		}
		guaranteed[ns], within[ns] = namespaceCaps(names, byQueueG), namespaceCaps(names, byQueueW)
	}
	return guaranteed, within, nil
}

// namespaceCaps is a namespace's grant from the GPU-declaring queues it has
// LocalQueues on: one queue's grant as is; for several, the smallest single
// queue's figure per accelerator type (minCaps), never their sum. Which queue
// a variant's replica is admitted through is not read, and Kueue charges the
// replica to that one queue only (KU2, multi-queue-namespace).
func namespaceCaps(names []string, byQueue map[string]config.ExternalCaps) config.ExternalCaps {
	var out config.ExternalCaps
	for i, name := range names {
		if i == 0 {
			out = addCaps(config.ExternalCaps{}, byQueue[name])
			continue
		}
		out = minCaps(out, byQueue[name])
	}
	return out
}

// minCaps is, per accelerator type, the smaller of two grants: a type one of
// them does not name takes that one's untyped grant, and is left to the other
// alone when that one has none (it grants nothing of the type). The untyped
// grant is the smaller of the two where both have one.
func minCaps(a, b config.ExternalCaps) config.ExternalCaps {
	grant := func(c config.ExternalCaps, accType string) (int, bool) {
		if k, ok := accelerator.FindKey(c.ByType, accType); ok {
			return c.ByType[k], true
		}
		return c.Untyped, c.HasUntyped
	}
	out := config.ExternalCaps{}
	for _, from := range []config.ExternalCaps{a, b} {
		for accType := range from.ByType {
			if _, done := accelerator.FindKey(out.ByType, accType); done {
				continue
			}
			ga, okA := grant(a, accType)
			gb, okB := grant(b, accType)
			n := ga
			switch {
			case okA && okB:
				n = min(ga, gb)
			case okB:
				n = gb
			}
			out.ByType = addTyped(out.ByType, accType, n)
		}
	}
	switch {
	case a.HasUntyped && b.HasUntyped:
		out.Untyped, out.HasUntyped = min(a.Untyped, b.Untyped), true
	case a.HasUntyped:
		out.Untyped, out.HasUntyped = a.Untyped, true
	case b.HasUntyped:
		out.Untyped, out.HasUntyped = b.Untyped, true
	}
	return out
}

// externalLenders converts a pool's lenders for the config snapshot.
func externalLenders(ls []LenderState) []config.ExternalLender {
	if len(ls) == 0 {
		return nil
	}
	out := make([]config.ExternalLender, 0, len(ls))
	for _, l := range ls {
		e := config.ExternalLender{Queue: l.Queue, Waiting: l.Waiting, Reclaim: l.Reclaim, Unused: l.Unused,
			Priority: l.Priority, Kept: l.Kept, PendingGPUs: l.PendingGPUs}
		if l.PendingPriority != nil {
			p := *l.PendingPriority
			e.PendingPriority = &p
		}
		if !l.PendingSince.IsZero() {
			at := l.PendingSince
			e.PendingSince = &at
		}
		out = append(out, e)
	}
	return out
}

// sharedPools converts the cohorts' free pools for the config snapshot.
func sharedPools(b Borrowing, flavorType map[string]string) []config.ExternalSharedPool {
	out := make([]config.ExternalSharedPool, 0, len(b.Pools))
	for _, p := range b.Pools {
		e := config.ExternalSharedPool{
			Cohort:         p.Cohort,
			Flavor:         p.Flavor,
			Resource:       p.Resource,
			Accelerator:    flavorType[p.Flavor],
			NodeLabels:     maps.Clone(p.NodeLabels),
			Tolerations:    slices.Clone(p.Tolerations),
			Free:           p.Free,
			Reason:         string(p.Reason),
			WaitingLenders: slices.Clone(p.WaitingLenders),
			Lenders:        externalLenders(p.Lenders),
			PendingGPUs:    p.PendingGPUs,
			CoFree:         maps.Clone(p.CoFree),
			CoUnbound:      slices.Clone(p.CoUnbound),
			Borrowed:       p.Borrowed,
		}
		if p.BorrowerPriority != nil {
			bp := *p.BorrowerPriority
			e.BorrowerPriority = &bp
		}
		if p.BorrowerLowest != nil {
			lo := *p.BorrowerLowest
			e.BorrowerLowest = &lo
		}
		e.EvictingGPUs, e.StuckEvictingGPUs, e.EvictionsRead = p.EvictingGPUs, p.StuckEvictingGPUs, p.EvictionsRead
		out = append(out, e)
	}
	return out
}

// borrowRows attributes each queue's borrowing figures to the namespaces it is
// granted to, in a stable order so equal reads log equal.
func borrowRows(b Borrowing, t Topology, flavorType map[string]string, nsQueues map[string][]string) map[string][]config.ExternalBorrow {
	out := make(map[string][]config.ExternalBorrow, len(nsQueues))
	for ns, names := range nsQueues {
		names = slices.Clone(names)
		slices.Sort(names)
		n := 0
		for _, name := range names {
			n += len(b.Queues[name])
		}
		rows := make([]config.ExternalBorrow, 0, n)
		for _, name := range names {
			for _, f := range b.Queues[name] {
				reason := f.Reason
				if reason == "" {
					reason = f.GuaranteedReason
				}
				row := config.ExternalBorrow{
					Queue:          name,
					Cohort:         t.Queues[name].Cohort,
					Flavor:         f.Flavor,
					Resource:       f.Resource,
					Accelerator:    flavorType[f.Flavor],
					Nominal:        f.Nominal,
					Guaranteed:     f.Guaranteed,
					Borrowable:     f.Borrowable,
					BorrowCap:      f.BorrowCap,
					WithinNominal:  f.WithinNominal,
					LentOut:        f.LentOut,
					PreemptAfter:   f.PreemptAfter,
					PreemptKept:    f.PreemptKept,
					SourceBorrowed: f.KueueBorrowed,
					Reason:         string(reason),
					PinRequired:    f.PinRequired,
					CoHeadroom:     maps.Clone(f.CoHeadroom),
					CoKept:         maps.Clone(f.CoKept),
					Reserved:       t.Queues[name].Resources[f.ResourceKey].Reserved,
					CoUnused:       coUnused(t.Queues[name], f.Flavor),
				}
				if ambiguousType(row.Accelerator, names, b, flavorType) {
					// KU2: more than one of the namespace's queues grants
					// this type, and which one a replica is admitted
					// through is not read, so none of their borrowing of
					// it is counted. A type one queue alone grants
					// borrows as usual.
					row = row.Withheld(PrecedenceOf(row.Reason, string(ReasonMultiQueueNamespace)))
				}
				rows = append(rows, row)
			}
		}
		out[ns] = rows
	}
	return out
}

// ambiguousType reports whether more than one of a namespace's GPU queues
// (names) grants a replica of accelerator acc: a flavor naming that
// accelerator, or an untyped one, which admits any type. An untyped row (acc
// "") of a namespace on several GPU queues is ambiguous: what it grants is not
// known per type. A namespace on one queue is never ambiguous.
func ambiguousType(acc string, names []string, b Borrowing, flavorType map[string]string) bool {
	if len(names) < 2 {
		return false
	}
	if acc == "" {
		return true
	}
	granting := 0
	for _, name := range names {
		if slices.ContainsFunc(b.Queues[name], func(f QueueFigures) bool {
			t := flavorType[f.Flavor]
			return t == "" || accelerator.SameName(t, acc)
		}) {
			granting++
		}
	}
	return granting > 1
}

// cohorts lists the Cohort objects. A failed list never reports "no Cohort
// objects", which would call every cohort flat, and never fails the read; the
// nominal figures do not depend on it.
//
//   - A Kueue that does not serve the kind, or a Forbidden list, reports the
//     hierarchy as unknown (unreadable): it lasts until someone acts, and
//     borrowing is withheld with reason hierarchy-unknown.
//   - Any other failure may pass by the next read. It reports the borrowing
//     figures of this read as stale (transient), so an entry that counts
//     borrowing freezes rather than dropping to nominal over a blip.
//
// The failure is logged once, until a later list succeeds; an unserved kind
// only at V(1). Callers must hold r.mu (warnedCohorts).
func (r *Reader) cohorts(ctx context.Context) (out map[string]CohortState, unreadable, transient bool) {
	items, err := r.list(ctx, KindCohort)
	if err != nil {
		noMatch := meta.IsNoMatchError(err)
		persistent := noMatch || apierrors.IsForbidden(err)
		if !r.warnedCohorts && ctx.Err() == nil {
			r.warnedCohorts = true
			logger := kueueLogger(ctx)
			msg := "Cannot list Kueue Cohort objects; borrowing is not counted (reason hierarchy-unknown). Logged once until a later list succeeds."
			if !persistent {
				msg = "Cannot list Kueue Cohort objects; borrowing figures are stale and frozen until a list succeeds. Logged once until a later list succeeds."
			}
			if noMatch {
				// A Kueue that serves no Cohort kind: no grant can fix it, and
				// the reason on the borrowing line already says it.
				logger.V(1).Info(msg, "error", err.Error())
			} else {
				logger.Error(err, msg,
					"remedy", "grant get and list on cohorts.kueue.x-k8s.io (config/components/kueue-reader)")
			}
		}
		return nil, persistent, !persistent
	}
	r.warnedCohorts = false
	out = make(map[string]CohortState, len(items))
	for i := range items {
		out[items[i].GetName()] = cohortState(&items[i])
	}
	return out, false, false
}

// cohortState reads a Cohort's parent and whether it declares quota of its
// own. The parent is spec.parentName; spec.parent is read too, for the older
// v1alpha1 Cohort API, which named the field so.
func cohortState(u *unstructured.Unstructured) CohortState {
	parent, _, _ := unstructured.NestedString(u.Object, "spec", "parentName")
	if parent == "" {
		parent, _, _ = unstructured.NestedString(u.Object, "spec", "parent")
	}
	groups, _, _ := unstructured.NestedSlice(u.Object, "spec", "resourceGroups")
	return CohortState{Name: u.GetName(), Parent: parent, HasResourceGroups: len(groups) > 0}
}

// queueState reads what borrowing depends on from a ClusterQueue. It never
// fails: a borrowing field that does not parse marks the queue Malformed,
// which withholds borrowing in its cohort and leaves the nominal figures,
// read by clusterQueueCaps, as they were.
func (r *Reader) queueState(cq *unstructured.Unstructured, flavorType map[string]string) QueueState {
	q := QueueState{Name: cq.GetName(), Reclaim: ReclaimNever, Resources: map[ResourceKey]QueueResource{}}
	q.Cohort, _, _ = unstructured.NestedString(cq.Object, "spec", "cohortName")
	if q.Cohort == "" {
		q.Cohort, _, _ = unstructured.NestedString(cq.Object, "spec", "cohort") // v1beta1
	}
	if reclaim, _, _ := unstructured.NestedString(cq.Object, "spec", "preemption", "reclaimWithinCohort"); reclaim != "" {
		q.Reclaim = reclaim
	}
	q.WithinClusterQueue, _, _ = unstructured.NestedString(cq.Object, "spec", "preemption", "withinClusterQueue")
	q.QueueingStrategy, _, _ = unstructured.NestedString(cq.Object, "spec", "queueingStrategy")
	if policy, _, _ := unstructured.NestedString(cq.Object, "spec", "stopPolicy"); policy == "Hold" || policy == "HoldAndDrain" {
		q.Inactive = true
	}
	q.AdmissionChecks = hasAdmissionChecks(cq)
	if policy, _, _ := unstructured.NestedString(cq.Object, "spec", "preemption", CQFieldBorrowWithinCohort, "policy"); policy != "" && policy != BorrowWithinCohortNever {
		q.BorrowWithinCohort = true
	}
	// flavorFungibility.whenCanBorrow is not read (K3). Kueue v0.20's CRD
	// defaults it to MayStopSearch on every ClusterQueue (kubebuilder default
	// in apis/kueue/v1beta1 and v1beta2 clusterqueue_types.go), so it cannot
	// tell a queue that chose it from one that did not; and it decides only
	// between flavors, so with one flavor for the GPU there is nothing to
	// pin. Several flavors require a pin, below, whatever it says.
	gpuFlavors := map[string]map[string]bool{} // GPU resource -> flavors offering it
	groups, _, _ := unstructured.NestedSlice(cq.Object, "spec", "resourceGroups")
	for _, g := range groups {
		group, _ := g.(map[string]interface{})
		flavors, _, _ := unstructured.NestedSlice(group, "flavors")
		// What the group covers, and its flavors in order: a co-resource is
		// charged on the GPU's flavor when its group covers a GPU too, else
		// on its own group's flavor (K2a, coFlavor).
		var covered []string
		flavorNames := make([]string, 0, len(flavors))
		for _, f := range flavors {
			flavor, _ := f.(map[string]interface{})
			name, _, _ := unstructured.NestedString(flavor, "name")
			flavorNames = append(flavorNames, name)
			resources, _, _ := unstructured.NestedSlice(flavor, "resources")
			for _, res := range resources {
				rq, _ := res.(map[string]interface{})
				resName, _, _ := unstructured.NestedString(rq, "name")
				if !slices.Contains(covered, resName) {
					covered = append(covered, resName)
				}
			}
		}
		groupHasGPU := slices.ContainsFunc(covered, func(res string) bool { return r.gpuNames[res] })
		for _, res := range covered {
			if r.gpuNames[res] {
				continue
			}
			if q.CoFlavors == nil {
				q.CoFlavors, q.CoWithGPU = map[string][]string{}, map[string]bool{}
			}
			if _, twice := q.CoFlavors[res]; twice {
				q.Malformed = true // Kueue's webhook admits a resource in one group only
			}
			q.CoFlavors[res], q.CoWithGPU[res] = flavorNames, groupHasGPU
		}
		for _, f := range flavors {
			flavor, _ := f.(map[string]interface{})
			name, _, _ := unstructured.NestedString(flavor, "name")
			if _, known := flavorType[name]; !known {
				q.Inactive = true
			}
			resources, _, _ := unstructured.NestedSlice(flavor, "resources")
			for _, res := range resources {
				rq, _ := res.(map[string]interface{})
				resName, _, _ := unstructured.NestedString(rq, "name")
				if !r.gpuNames[resName] {
					// A co-resource: Kueue admits only when it fits as well
					// (B1), whichever group covers it (K2a).
					c, err := coQuota(rq)
					if err != nil {
						q.Malformed = true
					}
					if q.CoResources == nil {
						q.CoResources = map[ResourceKey]CoQuota{}
					}
					q.CoResources[ResourceKey{Flavor: name, Resource: resName}] = c
					continue
				}
				if gpuFlavors[resName] == nil {
					gpuFlavors[resName] = map[string]bool{}
				}
				gpuFlavors[resName][name] = true
				var qr QueueResource
				var errs [3]error
				qr.Nominal, errs[0] = quantityField(rq, "nominalQuota")
				qr.LendingLimit, errs[1] = optionalQuantity(rq, "lendingLimit")
				qr.BorrowingLimit, errs[2] = optionalQuantity(rq, "borrowingLimit")
				if errors.Join(errs[:]...) != nil {
					q.Malformed = true
				}
				q.Resources[ResourceKey{Flavor: name, Resource: resName}] = qr
			}
		}
	}
	for _, fs := range gpuFlavors {
		if len(fs) > 1 {
			// Several flavors of one GPU resource: which one Kueue assigns a
			// replica depends on what fits first, unless the variant pins one.
			q.PinRequired = true
		}
	}

	status, found, _ := unstructured.NestedMap(cq.Object, "status")
	if !found {
		return q
	}
	q.FairSharing = status["fairSharing"] != nil
	if eq, ok := status["effectiveQuotas"]; ok && eq != nil {
		q.EffectiveQuotas = true
	}
	if n, ok := intField(status, "pendingWorkloads"); ok {
		q.Pending = n
	}
	reservations, found, _ := unstructured.NestedSlice(status, "flavorsReservation")
	q.HasStatus = found
	for _, fr := range reservations {
		flavor, _ := fr.(map[string]interface{})
		name, _, _ := unstructured.NestedString(flavor, "name")
		resources, _, _ := unstructured.NestedSlice(flavor, "resources")
		for _, res := range resources {
			rq, _ := res.(map[string]interface{})
			resName, _, _ := unstructured.NestedString(rq, "name")
			key := ResourceKey{Flavor: name, Resource: resName}
			if c, co := q.CoResources[key]; co {
				total, err := optionalMilli(rq, "total")
				if err != nil {
					q.Malformed = true
				}
				c.Reserved = total
				q.CoResources[key] = c
				continue
			}
			qr, declared := q.Resources[key]
			if !declared {
				continue
			}
			total, errT := optionalQuantity(rq, "total")
			borrowed, errB := optionalQuantity(rq, "borrowed")
			if errT != nil || errB != nil {
				q.Malformed = true
			}
			if total != nil {
				qr.Reserved = *total
			}
			if borrowed != nil {
				qr.Borrowed = *borrowed
			}
			q.Resources[key] = qr
		}
	}
	return q
}

// hasAdmissionChecks reports whether a ClusterQueue names any admission check:
// spec.admissionChecks, or spec.admissionChecksStrategy.admissionChecks (the
// field MultiKueue is configured through as well). A field of an unexpected
// shape counts as set, which withholds borrowing (fail safe).
func hasAdmissionChecks(cq *unstructured.Unstructured) bool {
	set := func(fields ...string) bool {
		raw, found, _ := unstructured.NestedFieldNoCopy(cq.Object, fields...)
		if !found || raw == nil {
			return false
		}
		list, ok := raw.([]interface{})
		return !ok || len(list) > 0
	}
	return set("spec", CQFieldAdmissionChecks) || set("spec", CQFieldAdmissionChecksStrategy, CQFieldAdmissionChecks)
}

// optionalQuantity is quantityField for a field that may be absent: nil then.
func optionalQuantity(m map[string]interface{}, field string) (*int, error) {
	if raw, ok := m[field]; !ok || raw == nil {
		return nil, nil
	}
	n, err := quantityField(m, field)
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// coQuota reads a co-resource's nominalQuota, lendingLimit and borrowingLimit,
// in milli-units. A field that does not parse is an error; the figures read
// stand.
func coQuota(rq map[string]interface{}) (CoQuota, error) {
	var c CoQuota
	nominal, errN := optionalMilli(rq, "nominalQuota")
	c.Nominal = nominal
	limit := func(field string) (*int64, error) {
		if raw, ok := rq[field]; !ok || raw == nil {
			return nil, nil
		}
		n, err := optionalMilli(rq, field)
		return &n, err
	}
	var errL, errB error
	c.LendingLimit, errL = limit("lendingLimit")
	c.BorrowingLimit, errB = limit("borrowingLimit")
	return c, errors.Join(errN, errL, errB)
}

// optionalMilli parses a resource.Quantity field in milli-units; 0 when absent.
func optionalMilli(m map[string]interface{}, field string) (int64, error) {
	raw, ok := m[field]
	if !ok || raw == nil {
		return 0, nil
	}
	var q resource.Quantity
	switch v := raw.(type) {
	case string:
		parsed, err := resource.ParseQuantity(v)
		if err != nil {
			return 0, fmt.Errorf("%s %q: %w", field, v, err)
		}
		q = parsed
	case int64:
		q = *resource.NewQuantity(v, resource.DecimalSI)
	case float64:
		q = *resource.NewMilliQuantity(int64(v*1000), resource.DecimalSI)
	default:
		return 0, fmt.Errorf("%s has unexpected type %T", field, raw)
	}
	if q.Sign() < 0 {
		return 0, errors.New(field + " is negative")
	}
	return q.MilliValue(), nil
}

// intField reads an integer status field, which decodes from JSON as int64 and
// may be float64 in a hand-built object.
func intField(m map[string]interface{}, field string) (int, bool) {
	switch v := m[field].(type) {
	case int64:
		return int(v), true
	case int:
		return v, true
	case float64:
		return int(v), true
	}
	return 0, false
}

// flavorPlacement reads a ResourceFlavor's node labels and tolerations, which
// Kueue pins every pod of the flavor to.
func flavorPlacement(rf *unstructured.Unstructured) FlavorPlacement {
	p := FlavorPlacement{AcceleratorType: flavorAcceleratorType(rf)}
	p.NodeLabels, _, _ = unstructured.NestedStringMap(rf.Object, "spec", "nodeLabels")
	p.TopologyName, _, _ = unstructured.NestedString(rf.Object, "spec", RFFieldTopologyName)
	if raw, found, _ := unstructured.NestedSlice(rf.Object, "spec", "tolerations"); found {
		for _, t := range raw {
			m, ok := t.(map[string]interface{})
			if !ok {
				continue
			}
			var tol corev1.Toleration
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(m, &tol); err == nil {
				p.Tolerations = append(p.Tolerations, tol)
			}
		}
	}
	return p
}

// nsRead is one Namespace read within a single Kueue read: its labels, or the
// error that withheld them. A failed read is cached too, so N LocalQueues in an
// unreadable namespace cost one API call, not N.
type nsRead struct {
	labels labels.Set
	err    error
}

// labelsRead reports whether any Namespace's labels were read successfully.
func labelsRead(cache map[string]nsRead) bool {
	for _, r := range cache {
		if r.err == nil {
			return true
		}
	}
	return false
}

// withheldError is a Namespace read that settles the question "can this
// namespace be shown to match?" with "no": the controller may not read it
// (Forbidden), or it no longer exists (NotFound). Either withholds the grant.
// Every other failure is transient and fails the whole read instead.
type withheldError struct {
	ns  string
	err error
}

func (e *withheldError) Error() string {
	return fmt.Sprintf("kueue: reading labels of namespace %q: %v", e.ns, e.err)
}

func (e *withheldError) Unwrap() error { return e.err }

// remedy is the operator action that would let the grant through.
func (e *withheldError) remedy() string {
	if apierrors.IsNotFound(e.err) {
		// The LocalQueue was listed but its Namespace is gone: it is being
		// deleted, and the LocalQueue with it. Nothing to grant; nothing to do.
		return "none needed: the namespace was deleted after its LocalQueue was listed; the next read will not see the LocalQueue"
	}
	return "grant get on namespaces to the controller ServiceAccount (config/components/kueue-reader, or make enable-physical-limiter LLMSM_LIMITER_TYPE=quota LLMSM_QUOTA_KUEUE=true)"
}

// namespaceAdmission is which namespaces a ClusterQueue admits workloads from.
type namespaceAdmission struct {
	// all: an empty selector, which in Kueue matches every namespace.
	all bool
	// selector is non-nil only when the answer depends on namespace labels.
	selector labels.Selector
}

// clusterQueueAdmission reads spec.namespaceSelector with Kueue semantics: an
// absent (nil) selector admits NO namespace, an empty one ({}) admits every
// namespace, anything else admits the namespaces whose labels it matches. This
// is metav1.LabelSelectorAsSelector, the conversion Kueue itself applies (nil
// gives labels.Nothing, empty gives labels.Everything). A selector that does not
// parse admits nothing; Kueue's webhook rejects one.
func clusterQueueAdmission(cq *unstructured.Unstructured) namespaceAdmission {
	raw, found, err := unstructured.NestedMap(cq.Object, "spec", "namespaceSelector")
	if err != nil || !found || raw == nil {
		return namespaceAdmission{}
	}
	var ls metav1.LabelSelector
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &ls); err != nil {
		return namespaceAdmission{}
	}
	sel, err := metav1.LabelSelectorAsSelector(&ls)
	if err != nil {
		return namespaceAdmission{}
	}
	if sel.Empty() {
		return namespaceAdmission{all: true}
	}
	return namespaceAdmission{selector: sel}
}

// nsListedKey marks, in a read's Namespace cache, that every Namespace was
// listed: a namespace missing from the cache then does not exist. No
// namespace is named "".
const nsListedKey = ""

// admitsNamespace reports whether a ClusterQueue admits ns. Namespace labels
// are read only when the selector needs them, by ONE list of every Namespace
// per read, so a read costs one call however many namespaces have
// LocalQueues. Where that list is Forbidden (a namespace-scoped install, whose
// RBAC names its namespaces), each namespace is read with a get instead, once
// per read -- a failed read included. Both are unstructured, so they go to
// the API server rather than starting a cluster-wide Namespace informer.
//
// A Forbidden or NotFound read returns a *withheldError; any other failure is
// returned as is and must fail the caller's whole read.
func (r *Reader) admitsNamespace(ctx context.Context, a namespaceAdmission, ns string, cache map[string]nsRead) (bool, error) {
	switch {
	case a.all:
		return true, nil
	case a.selector == nil:
		return false, nil
	}
	if err := r.listNamespaces(ctx, cache); err != nil {
		return false, err
	}
	got, ok := cache[ns]
	if !ok && r.listedAll(cache) {
		got = nsRead{err: &withheldError{ns: ns, err: apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, ns)}}
		cache[ns], ok = got, true
	}
	if !ok {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"})
		err := r.client.Get(ctx, client.ObjectKey{Name: ns}, u)
		switch {
		case err == nil:
			got = nsRead{labels: labels.Set(u.GetLabels())}
		case apierrors.IsForbidden(err), apierrors.IsNotFound(err):
			got = nsRead{err: &withheldError{ns: ns, err: err}}
		default:
			// Not cached: the caller aborts the read on it.
			return false, fmt.Errorf("kueue: reading labels of namespace %q: %w", ns, err)
		}
		cache[ns] = got
	}
	if got.err != nil {
		return false, got.err
	}
	return a.selector.Matches(got.labels), nil
}

// listedAll reports whether cache holds every Namespace from one list.
func (r *Reader) listedAll(cache map[string]nsRead) bool {
	_, ok := cache[nsListedKey]
	return ok
}

// listNamespaces fills cache from one list of every Namespace, once per read.
// A Forbidden list is remembered (nsListDenied) and leaves the cache to the
// per-namespace gets; any other failure is returned and fails the read.
// Callers must hold r.mu.
func (r *Reader) listNamespaces(ctx context.Context, cache map[string]nsRead) error {
	if r.nsListDenied || r.listedAll(cache) {
		return nil
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "NamespaceList"})
	if err := r.client.List(ctx, list); err != nil {
		if apierrors.IsForbidden(err) {
			r.nsListDenied = true
			return nil
		}
		return fmt.Errorf("kueue: listing namespaces for their labels: %w", err)
	}
	for i := range list.Items {
		cache[list.Items[i].GetName()] = nsRead{labels: labels.Set(list.Items[i].GetLabels())}
	}
	cache[nsListedKey] = nsRead{}
	return nil
}

// addCaps sums two grants. Typed caps are merged by accelerator identity: two
// grants of one product add up under one key whatever their case, while two
// different products of one family (PCIe and SXM) stay two keys until a static
// key says they are one (config.BoundBy sums a family for a short static key).
func addCaps(into, add config.ExternalCaps) config.ExternalCaps {
	for accType, n := range add.ByType {
		into.ByType = addTyped(into.ByType, accType, n)
	}
	if add.HasUntyped {
		into.Untyped = addCapped(into.Untyped, add.Untyped)
		into.HasUntyped = true
	}
	return into
}

// addTyped adds n under the key of caps that names the same accelerator as
// accType, or under accType itself when none does.
func addTyped(caps map[string]int, accType string, n int) map[string]int {
	if caps == nil {
		caps = make(map[string]int)
	}
	key := accType
	if existing, ok := accelerator.FindKey(caps, accType); ok {
		key = existing
	}
	caps[key] = addCapped(caps[key], n)
	return caps
}

// listLocalQueues lists LocalQueues cluster-wide when Options.Namespace is
// empty, else in each namespace it names (one, or a comma-separated list), in
// that order. A namespace-scoped controller may list them nowhere else.
//
// With several namespaces, one that is Forbidden (its RoleBinding deleted, or
// the namespace gone) is skipped and logged, not failed: the others keep their
// Kueue bound, and the skipped one has no LocalQueue here, so its grant reads
// as none -- bounded at nothing, the safe direction. With one namespace a
// Forbidden list stays the error it is.
func (r *Reader) listLocalQueues(ctx context.Context) ([]unstructured.Unstructured, error) {
	namespaces := config.ParseWatchNamespaces(r.opts.Namespace)
	if len(namespaces) == 0 {
		return r.list(ctx, KindLocalQueue)
	}
	var out []unstructured.Unstructured
	for _, ns := range namespaces {
		items, err := r.list(ctx, KindLocalQueue, client.InNamespace(ns))
		if err != nil {
			if len(namespaces) > 1 && apierrors.IsForbidden(err) {
				kueueLogger(ctx).Info("Cannot list Kueue LocalQueues in one watched namespace; it is skipped and gets no Kueue grant until the read is allowed again",
					"namespace", ns, "error", err.Error())
				continue
			}
			return nil, err
		}
		out = append(out, items...)
	}
	return out, nil
}

// list fetches every object of a Kueue kind at whatever version the cluster
// serves. A cluster without Kueue is reported as such rather than as an empty
// list: an operator who enabled the reader must learn it found nothing to read.
func (r *Reader) list(ctx context.Context, kind string, opts ...client.ListOption) ([]unstructured.Unstructured, error) {
	mapping, err := r.client.RESTMapper().RESTMapping(schema.GroupKind{Group: Group, Kind: kind})
	if err != nil {
		if meta.IsNoMatchError(err) {
			return nil, fmt.Errorf("kueue: %s.%s is not served by this cluster (is Kueue installed?): %w", kind, Group, err)
		}
		return nil, fmt.Errorf("kueue: resolving %s: %w", kind, err)
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(mapping.GroupVersionKind.GroupVersion().WithKind(kind + "List"))
	if err := r.client.List(ctx, list, opts...); err != nil {
		return nil, fmt.Errorf("kueue: listing %s: %w", kind, err)
	}
	return list.Items, nil
}

// clusterQueueCaps sums a ClusterQueue's nominalQuota over its GPU resources,
// per accelerator type. Two flavors of the same type (an SXM and a PCIe H100
// flavor, say) add up, as the static maps could not tell them apart either.
//
// A flavor whose ResourceFlavor carries no product label — Kueue's quickstart
// `default-flavor`, or any single-GPU-type cluster that never bothered — is an
// UNTYPED grant. It is not named after the flavor: a flavor called
// "default-flavor" or "h100-sxm" is not an accelerator type, and treating it as
// one would make it compete with every static key and zero them all.
//
// An untyped grant carries no vendor either, so within one queue the untyped
// figures of different GPU resources (nvidia.com/gpu and amd.com/gpu, say) are
// not summed — the LARGEST is taken. The grant bounds every static type alike,
// and a sum would bound an H100 entry by the AMD queue's GPUs too; the largest
// single resource is the loosest bound no one resource contradicts. A cluster
// that needs the distinction pins `kueue.resources` to one vendor.
func (r *Reader) clusterQueueCaps(cq *unstructured.Unstructured, flavorType map[string]string) (config.ExternalCaps, error) {
	return r.clusterQueueCapsAt(cq, flavorType, nil)
}

// clusterQueueCapsAt is clusterQueueCaps with each GPU line's figure taken from
// figure(key, nominal) instead of its nominalQuota; nil takes the nominal
// figure. The borrowing grants (guaranteed, within nominal) go through it so they
// follow every rule the nominal grant does.
func (r *Reader) clusterQueueCapsAt(cq *unstructured.Unstructured, flavorType map[string]string, figure func(ResourceKey, int) int) (config.ExternalCaps, error) {
	var caps config.ExternalCaps
	groups, _, err := unstructured.NestedSlice(cq.Object, "spec", "resourceGroups")
	if err != nil {
		return caps, fmt.Errorf("kueue: ClusterQueue %q: spec.resourceGroups: %w", cq.GetName(), err)
	}
	untypedByResource := map[string]int{}
	// A queue Kueue will not admit from is a GOVERNING queue that grants nothing
	// -- not an absent one. Absent would leave the namespace to the static
	// entry, which with the recommended `default: {H100: -1}` is unlimited: the
	// exact opposite of what Kueue is doing. Two things make a queue inactive
	// here: a stopPolicy that holds admission, and a flavor the cluster does not
	// have (FlavorNotFound). Either way the queue's whole grant becomes an
	// untyped grant of zero, which bounds every static type at nothing.
	inactive := false
	if policy, _, _ := unstructured.NestedString(cq.Object, "spec", "stopPolicy"); policy == "Hold" || policy == "HoldAndDrain" {
		inactive = true
	}
	for _, g := range groups {
		group, _ := g.(map[string]interface{})
		flavors, _, _ := unstructured.NestedSlice(group, "flavors")
		for _, f := range flavors {
			flavor, _ := f.(map[string]interface{})
			name, _, _ := unstructured.NestedString(flavor, "name")
			resources, _, _ := unstructured.NestedSlice(flavor, "resources")
			for _, res := range resources {
				rq, _ := res.(map[string]interface{})
				resName, _, _ := unstructured.NestedString(rq, "name")
				if !r.gpuNames[resName] {
					continue
				}
				n, err := quantityField(rq, "nominalQuota")
				if err != nil {
					return caps, fmt.Errorf("kueue: ClusterQueue %q flavor %q resource %q: %w", cq.GetName(), name, resName, err)
				}
				if figure != nil {
					n = figure(ResourceKey{Flavor: name, Resource: resName}, n)
				}
				accType, known := flavorType[name]
				switch {
				case !known:
					inactive = true
				case accType != "":
					caps.ByType = addTyped(caps.ByType, accType, n)
				default:
					untypedByResource[resName] = addCapped(untypedByResource[resName], n)
				}
			}
		}
	}
	if inactive {
		return config.ExternalCaps{HasUntyped: true}, nil
	}
	for _, n := range untypedByResource {
		caps.HasUntyped = true
		caps.Untyped = max(caps.Untyped, n)
	}
	return caps, nil
}

// flavorAcceleratorType derives the accelerator type from a ResourceFlavor's
// node labels — the product label of any known vendor (or its aliases), kept AS
// WRITTEN. Empty when the flavor carries no product label.
//
// Not reduced to the short name here. A ResourceFlavor exists to tell one
// product from another within a family — a 40GB PCIe A100 from an 80GB SXM one
// — and reducing both to "A100" before aggregation would sum their quotas and
// bind an SXM entry by a PCIe grant. Long names meet short static keys at bound
// time through accelerator.SameName, which is built for exactly that and never
// equates two different long names.
func flavorAcceleratorType(rf *unstructured.Unstructured) string {
	labels, _, _ := unstructured.NestedStringMap(rf.Object, "spec", "nodeLabels")
	return accelerator.ProductFromNodeSelector(labels)
}

// quantityField parses a resource.Quantity field of an unstructured map into a
// whole GPU count, clamped to config.MaxQuotaValue. The API server stores a
// Quantity as a string, but a hand-built object (tests, or a future typed
// conversion) may carry a number, so both are accepted.
func quantityField(m map[string]interface{}, field string) (int, error) {
	raw, ok := m[field]
	if !ok || raw == nil {
		return 0, fmt.Errorf("%s is missing", field)
	}
	var q resource.Quantity
	switch v := raw.(type) {
	case string:
		parsed, err := resource.ParseQuantity(v)
		if err != nil {
			return 0, fmt.Errorf("%s %q: %w", field, v, err)
		}
		q = parsed
	case int64:
		q = *resource.NewQuantity(v, resource.DecimalSI)
	case float64:
		q = *resource.NewMilliQuantity(int64(v*1000), resource.DecimalSI)
	default:
		return 0, fmt.Errorf("%s has unexpected type %T", field, raw)
	}
	if q.Sign() < 0 {
		return 0, errors.New(field + " is negative")
	}
	// Value rounds up, so a fractional quota (which Kueue allows for other
	// resources and rejects for none) counts the GPU it partially names.
	return clampQuota(q.Value()), nil
}

// addCapped adds two caps without exceeding config.MaxQuotaValue.
func addCapped(a, b int) int {
	return clampQuota(int64(a) + int64(b))
}

// clampQuota bounds a figure to config.MaxQuotaValue, the largest finite cap
// the limiter accepts. Anything Kueue declares beyond it is a cap that will
// never bind, and clamping keeps the aggregate sums clear of overflow.
func clampQuota(n int64) int {
	if n > config.MaxQuotaValue {
		return config.MaxQuotaValue
	}
	return int(n)
}
