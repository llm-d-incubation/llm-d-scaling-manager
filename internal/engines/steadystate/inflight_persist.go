package steadystate

import (
	"context"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/controllerstatus"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/variantmeta"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

// inFlightStore is the in-flight ledger's persistence in the controller
// status (controllerstatus.KeyInFlightLedger).
type inFlightStore struct {
	// loaded is set once the persisted ledger was read back, or found
	// absent or unusable; errLogged once a failed read was logged.
	loaded    bool
	errLogged bool
	// last is the ledger last handed to the status publisher.
	last string
}

// restoreInFlight reads the persisted in-flight ledger back into
// allocation.DefaultInFlight, once (allocation.InFlightLedger.RestoreInFlight):
// the holds still within their TTL, a replica whose pods are Unschedulable
// and a LeaderWorkerSet group's unplaced workers among them, outlive the
// restart. Each restored hold counts its scale target's pods from here on
// (restoredPlaced), so one whose pods land before a cycle admits its variant
// ends then. A read that fails is tried again next cycle; until one succeeds
// nothing is persisted, so the ledger in the ConfigMap is not overwritten by
// this process's empty one, and wakes fitted to nodes wait for it
// (allocation.InFlightLedger.MarkRestored, once it is done). A ledger that
// cannot be parsed, or of another version, is ignored and logged.
func (e *Engine) restoreInFlight(ctx context.Context, now time.Time) {
	st := &e.inFlightStore
	if st.loaded {
		return
	}
	var reader client.Reader
	if e.apiReader != nil {
		reader = e.apiReader
	} else if e.client != nil {
		reader = e.client
	}
	if reader == nil {
		st.loaded = true
		allocation.DefaultInFlight.MarkRestored()
		return
	}
	logger := ctrl.LoggerFrom(ctx)
	raw, found, err := controllerstatus.ReadKey(ctx, reader, config.SystemNamespace(), controllerstatus.KeyInFlightLedger)
	if err != nil {
		if !st.errLogged {
			st.errLogged = true
			logger.Info("Cannot read the persisted in-flight node ledger yet; the node holes it carries are not held until it is read, and the read is tried again each cycle",
				"error", err.Error())
		}
		return
	}
	st.loaded = true
	// Whatever the read found, the wakes waiting on it may go once the
	// entries are in (allocation.InFlightLedger.RestorePending).
	defer allocation.DefaultInFlight.MarkRestored()
	if !found {
		return
	}
	p, err := allocation.UnmarshalInFlight(raw)
	if err != nil {
		logger.Error(err, "Ignoring an unreadable in-flight node ledger")
		return
	}
	// No hold the engine starts outlasts three placement timeouts (a Kueue
	// flavor's while a cluster-autoscaler scale-up is pending).
	maxTTL := max(allocation.InFlightTTL, 3*e.borrowSettings().PlacementTimeout)
	restored, dropped := allocation.DefaultInFlight.RestoreInFlightWith(p, now, maxTTL, e.restoredPlaced)
	if dropped > 0 {
		logger.Info("Warning: dropped in-flight node ledger entries out of bounds; this controller did not write them",
			"dropped", dropped)
	}
	if restored > 0 {
		logger.Info("Restored in-flight node holds after a restart", "variants", restored)
	}
	// last stays unset: the next persist hands the restored ledger to the
	// status publisher.
}

// restoredCountTimeout bounds one count of a restored entry's pods
// (restoredPlaced): the scale target read and the pod list. A count that
// times out counts nothing, and the hold stays until the next.
const restoredCountTimeout = 5 * time.Second

// restoredPlaced counts a restored entry's placed replicas from its persisted
// scale target (allocation.InFlightLedger.RestoreInFlightWith), as
// inFlightAdmissions' count does for a live one: the hold ends once its pods
// hold their nodes, whether or not a cycle has admitted its variant since the
// restart. The scale target is read on first use and kept; a read that fails
// counts nothing (the hold stays) and is tried again on the next count.
func (e *Engine) restoredPlaced(key string, target allocation.ScaleTargetRef) func() (allocation.Placed, bool) {
	c := e.client
	ns, _, ok := strings.Cut(key, "/")
	if c == nil || !ok || target.Name == "" {
		return nil
	}
	var (
		mu  sync.Mutex
		acc scaletarget.ScaleTargetAccessor
	)
	return func() (allocation.Placed, bool) {
		// Bounded: the count runs while a node view is built, the
		// scale-from-zero engine's under its wake lock.
		ctx, cancel := context.WithTimeout(context.Background(), restoredCountTimeout)
		defer cancel()
		mu.Lock()
		a := acc
		mu.Unlock()
		if a == nil {
			got, err := scaleTargetOf(ctx, c, target, ns)
			if err != nil {
				return allocation.Placed{}, false
			}
			mu.Lock()
			acc, a = got, got
			mu.Unlock()
		}
		whole, partial, ok := variantmeta.PlacedGroups(ctx, c, ns, a)
		return allocation.Placed{Whole: whole, Partial: partial}, ok
	}
}

// scaleTargetOf reads the scale target ref names in namespace, once: the
// count runs while a node view is built, so it does not retry.
func scaleTargetOf(ctx context.Context, c client.Client, ref allocation.ScaleTargetRef, namespace string) (scaletarget.ScaleTargetAccessor, error) {
	key := client.ObjectKey{Namespace: namespace, Name: ref.Name}
	switch ref.Kind {
	case constants.LeaderWorkerSetKind:
		var lws lwsv1.LeaderWorkerSet
		if err := c.Get(ctx, key, &lws); err != nil {
			return nil, err
		}
		return scaletarget.NewLWSAccessor(&lws), nil
	default:
		var d appsv1.Deployment
		if err := c.Get(ctx, key, &d); err != nil {
			return nil, err
		}
		return scaletarget.NewDeploymentAccessor(&d), nil
	}
}

// persistInFlight hands the in-flight ledger to the status publisher when it
// changed, once the persisted one was read back.
func (e *Engine) persistInFlight(ctx context.Context, now time.Time) {
	st := &e.inFlightStore
	if !st.loaded {
		return
	}
	raw, err := allocation.DefaultInFlight.MarshalInFlight(now)
	if err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "Cannot persist the in-flight node ledger; the last persisted one stays")
		return
	}
	if raw == st.last {
		return
	}
	st.last = raw
	e.Status.SetInFlightLedger(raw)
}
