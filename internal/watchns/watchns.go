// Package watchns decides which of the namespaces a namespace-scoped
// controller is told to watch it may read now, and asks again while it runs.
//
// A namespace-scoped manager builds one informer per object kind per
// namespace, and an informer that cannot list never syncs: one listed
// namespace whose RoleBinding was deleted, or which was deleted itself, would
// keep the controller from ever becoming Ready -- for every other namespace
// too. So the cache is built from the readable ones only (Split), and the rest
// are reported and asked again (Prober).
package watchns

import (
	"context"
	"errors"
	"slices"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Reasons a watched namespace is unreadable, as the
// llmsm_watch_namespace_unreadable reason label carries them.
const (
	// ReasonForbidden is an authorization denial: the controller's RoleBinding
	// there is gone.
	ReasonForbidden = "forbidden"
	// ReasonNamespaceNotFound is a namespace that no longer exists.
	ReasonNamespaceNotFound = "namespace-not-found"
)

// DefaultInterval is how often Prober asks again.
const DefaultInterval = time.Minute

// probeTimeout bounds one namespace's probe.
const probeTimeout = 10 * time.Second

// ErrSetChanged ends the manager so the cache is rebuilt from the namespaces
// that are readable now.
var ErrSetChanged = errors.New("the readable set of watched namespaces changed; restarting to rebuild the cache")

// Split divides watched into the namespaces c may list ConfigMaps, Pods and
// Deployments in, in the given order, and the ones it may not, with why.
//
// Only an authorization answer, or NotFound, marks one unreadable. Any other
// error (a timeout, a 5xx) says nothing about the namespace, and dropping it on
// an API-server blip would stop scaling there; it counts as readable, and a
// real denial is caught by the next probe.
func Split(ctx context.Context, c client.Reader, watched []string) (readable []string, blocked map[string]string) {
	blocked = map[string]string{}
	for _, ns := range watched {
		if why := probe(ctx, c, ns); why != "" {
			blocked[ns] = why
			continue
		}
		readable = append(readable, ns)
	}
	return readable, blocked
}

// probe returns "" when namespace ns can be listed, else the reason.
func probe(ctx context.Context, c client.Reader, ns string) string {
	for _, list := range []client.ObjectList{&corev1.ConfigMapList{}, &corev1.PodList{}, &appsv1.DeploymentList{}} {
		pctx, cancel := context.WithTimeout(ctx, probeTimeout)
		err := c.List(pctx, list, client.InNamespace(ns), client.Limit(1))
		cancel()
		switch {
		case err == nil:
		case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
			return ReasonForbidden
		case apierrors.IsNotFound(err):
			return ReasonNamespaceNotFound
		}
	}
	return ""
}

// Prober asks again, every Interval, which watched namespaces are readable.
// When that set differs from Active, the one the cache was built from, Start
// returns ErrSetChanged, which ends the manager: the cache's namespaces are
// fixed at construction, and a restart rebuilds it from the new set. A restart
// costs one start; the alternative was a namespace that stays unmanaged after
// its RoleBinding is restored, or an informer that retries a Forbidden list
// forever.
type Prober struct {
	// Reader lists outside the cache.
	Reader client.Reader
	// Watched is every namespace the controller is told to watch.
	Watched []string
	// Active is the readable subset the cache holds.
	Active []string
	// Interval between probes; DefaultInterval when zero.
	Interval time.Duration
	// Report, when set, is given the unreadable set after every probe.
	Report func(blocked map[string]string)
}

// NeedLeaderElection implements manager.LeaderElectionRunnable: every replica
// builds its own cache, so every replica probes.
func (*Prober) NeedLeaderElection() bool { return false }

// Start probes until ctx ends (nil) or the readable set changes (ErrSetChanged).
func (p *Prober) Start(ctx context.Context) error {
	interval := p.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	var changed bool
	loopCtx, stop := context.WithCancel(ctx)
	defer stop()
	wait.UntilWithContext(loopCtx, func(ctx context.Context) {
		readable, blocked := Split(ctx, p.Reader, p.Watched)
		if ctx.Err() != nil {
			return
		}
		if p.Report != nil {
			p.Report(blocked)
		}
		if !slices.Equal(readable, p.Active) {
			ctrl.Log.WithName("setup").Info("The readable set of watched namespaces changed; restarting so the cache covers it",
				"was", p.Active, "now", readable, "unreadable", blocked)
			changed = true
			stop()
		}
	}, interval)
	if changed {
		return ErrSetChanged
	}
	return nil
}
