// Package managedns decides which namespaces a cluster-scoped controller
// installed from the one release manifest manages, and reports, per
// namespace, whether it is ready to be scaled.
//
// The cluster admin decides with the existing watchNamespaces setting, which
// on this install takes effect without a restart (--watch-namespaces-live):
// the controller's cache stays cluster-wide, and the setting selects the
// namespaces it acts in. Empty manages every namespace, as it always has on a
// cluster-wide install.
//
// The scaler refuses every KEDA call from a namespace this does not admit, so
// a workload there is never registered; the Reporter forgets what was
// registered in a namespace that stopped being managed, drops its
// InferencePools, and tells the namespace why, in Events a namespace owner
// reads in their own namespace.
package managedns

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
)

// RefusedTTL is how long a namespace refused at the scaler stays listed as
// refused after its last refused call.
const RefusedTTL = 10 * time.Minute

// Admission admits or refuses namespaces. Safe for concurrent use.
type Admission struct {
	// Namespaces returns the managed namespaces, and all when every
	// namespace is managed (config.Config.ManagedNamespaces).
	Namespaces func() (namespaces []string, all bool)
	// Now is the clock; nil is time.Now.
	Now func() time.Time

	mu      sync.Mutex
	refused map[string]refusal
}

// refusal is the latest refused call from one namespace, and the
// ScaledObjects KEDA called the scaler for there.
type refusal struct {
	reason        string
	at            time.Time
	scaledObjects map[string]time.Time
}

// Refused is a namespace the scaler refused calls from.
type Refused struct {
	// Reason is why the namespace is not managed.
	Reason string
	// ScaledObjects are the ones KEDA called the scaler for, sorted.
	ScaledObjects []string
}

func (a *Admission) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Manages reports whether namespace is managed.
func (a *Admission) Manages(namespace string) bool {
	return a.Reason(namespace) == ""
}

// Reason says why namespace is not managed, "" when it is.
func (a *Admission) Reason(namespace string) string {
	if a.Namespaces == nil {
		return ""
	}
	names, all := a.Namespaces()
	if all || slices.Contains(names, namespace) {
		return ""
	}
	if len(names) == 0 {
		return fmt.Sprintf("the %s setting was rejected, so no namespace is managed until it is fixed", config.SettingWatchNamespaces)
	}
	return fmt.Sprintf("it is not in the %s setting (%s)", config.SettingWatchNamespaces, strings.Join(names, ","))
}

// Admit says whether a ScaledObject in namespace is served. refusal is why
// not, "" when it is. It never fails: the setting is in memory. The error is
// for the scaler's AdmitFunc, whose other implementations read the cluster.
func (a *Admission) Admit(_ context.Context, namespace, scaledObject string) (refusal string, err error) {
	refusal = a.Reason(namespace)
	if refusal != "" {
		a.noteRefused(namespace, scaledObject, refusal)
	}
	return refusal, nil
}

func (a *Admission) noteRefused(namespace, scaledObject, reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.refused == nil {
		a.refused = map[string]refusal{}
	}
	now := a.now()
	r := a.refused[namespace]
	r.reason, r.at = reason, now
	if r.scaledObjects == nil {
		r.scaledObjects = map[string]time.Time{}
	}
	if scaledObject != "" {
		r.scaledObjects[scaledObject] = now
	}
	a.refused[namespace] = r
}

// Refused returns each namespace the scaler refused a call from within
// RefusedTTL that is not managed now, with why and the ScaledObjects KEDA
// called for. Older refusals, and those of a namespace managed since, are
// forgotten.
func (a *Admission) Refused() map[string]Refused {
	a.mu.Lock()
	defer a.mu.Unlock()
	cutoff := a.now().Add(-RefusedTTL)
	out := map[string]Refused{}
	for ns, r := range a.refused {
		if r.at.Before(cutoff) {
			delete(a.refused, ns)
			continue
		}
		why := a.Reason(ns)
		if why == "" {
			delete(a.refused, ns)
			continue
		}
		var names []string
		for so, at := range r.scaledObjects {
			if at.Before(cutoff) {
				delete(r.scaledObjects, so)
				continue
			}
			names = append(names, so)
		}
		sort.Strings(names)
		out[ns] = Refused{Reason: why, ScaledObjects: names}
	}
	return out
}
