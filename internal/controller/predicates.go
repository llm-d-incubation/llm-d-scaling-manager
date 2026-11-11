package controller

import (
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/datastore"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// ConfigMapPredicate returns a predicate that filters ConfigMap events to only the target ConfigMaps.
// It matches the enqueue function logic - allows either configmap name if namespace matches.
// This predicate is used to filter only the target configmaps.
//
// For namespace-local ConfigMap support:
// - Global ConfigMaps: well-known names in controller namespace
// - Namespace-local ConfigMaps: well-known names in watched or tracked namespaces
//
// Filtering behavior:
//   - Single-namespace mode (--watch-namespace set): Always allow ConfigMaps from the watched namespace
//   - Multi-namespace mode: Only allow ConfigMaps from tracked namespaces (namespaces with VAs)
//
// ds is the datastore used to check if a namespace is tracked (fast, in-memory check).
// cfg is the configuration used to check if single-namespace mode is enabled.
// Opt-in labels and exclusion are handled in the handler to avoid expensive API calls in the predicate.
func ConfigMapPredicate(ds datastore.Datastore, cfg *config.Config) predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		name := obj.GetName()
		namespace := obj.GetNamespace()
		systemNamespace := config.SystemNamespace()

		// Check if this is a well-known ConfigMap name
		if name != config.ConfigMapName() && name != config.ScalingPolicyConfigMapName() &&
			name != config.SettingsConfigMapName() {
			return false
		}

		// Global ConfigMaps: must be in controller namespace
		if namespace == systemNamespace {
			return true
		}

		// Cluster policy: the namespace holding limiters and quotas, which is
		// deliberately outside the controller's own when a tenant runs the
		// controller. Without this it would be filtered out here — the namespace has
		// no Variant in it and never becomes "tracked" — and limiter
		// changes would take effect only on restart.
		if cfg != nil && cfg.PolicyNamespaceIsSeparate() && namespace == cfg.PolicyNamespace() {
			return true
		}

		// Single-namespace mode: watch all ConfigMaps in the watched namespace
		// Explicit CLI flag overrides tracking-based filtering
		if cfg != nil && cfg.Watches(namespace) {
			return true
		}

		// Multi-namespace mode: only allow in tracked namespaces (namespaces with VAs)
		// This prevents cluster-wide watching and cache sync timeouts.
		// Opt-in labels and exclusion are still checked in the handler for accuracy.
		if ds != nil {
			return ds.IsNamespaceTracked(namespace)
		}

		// If no datastore provided, fall back to allowing all (backwards compatible)
		// This should not happen in production, but provides safety during setup.
		return true
	})
}
