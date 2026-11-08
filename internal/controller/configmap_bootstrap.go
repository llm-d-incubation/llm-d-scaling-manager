package controller

import (
	"context"
	"errors"
	"fmt"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// BootstrapInitialConfigMaps performs an initial sync of known ConfigMaps before manager start.
// It ensures dynamic ConfigMap-backed settings are loaded before other reconcilers and engines run.
func (r *ConfigMapReconciler) BootstrapInitialConfigMaps(ctx context.Context) error {
	logger := log.FromContext(ctx)

	if r.Config == nil {
		err := errors.New("config is nil")
		errorType := "Config is nil in ConfigMapReconciler bootstrap"
		logger.Error(err, errorType)
		metrics.RecordError(constants.ComponentController, errorType)
		return err
	}

	systemNamespace := config.SystemNamespace()
	watchNamespace := r.Config.WatchNamespace()

	// Always bootstrap global ConfigMaps from system namespace
	targets := []struct {
		name      string
		namespace string
		isGlobal  bool
	}{}
	targets = append(targets, struct {
		name      string
		namespace string
		isGlobal  bool
	}{name: config.ScalingPolicyConfigMapName(), namespace: systemNamespace, isGlobal: true})

	// Cluster policy, when it lives outside the controller's namespace, must be
	// loaded before anything reads a limiter. Loading it here rather than on the
	// first watch event means the very first optimization cycle is already bounded:
	// a cycle that ran unbounded because policy had not arrived yet would allocate
	// against no budget, and the actuation that follows is not undone by loading
	// the limiter a moment later.
	if r.Config.PolicyNamespaceIsSeparate() {
		if err := r.bootstrapClusterPolicy(ctx); err != nil {
			return err
		}
	}

	// Determine which namespaces to scan for namespace-local ConfigMaps
	var namespacesToScan []string

	if watchNamespace != "" {
		// Namespace-scoped: only the watched namespaces, one or a listed few
		for _, ns := range r.Config.WatchNamespaces() {
			if ns != systemNamespace {
				namespacesToScan = append(namespacesToScan, ns)
			}
		}
		if len(namespacesToScan) > 0 {
			logger.Info("Initial ConfigMap bootstrap", "watchNamespace", watchNamespace)
		}
	} else {
		// All-namespaces mode: list and scan all namespaces
		namespaceList := &corev1.NamespaceList{}
		if err := r.List(ctx, namespaceList, &client.ListOptions{}); err != nil {
			errorType := "Failed to list namespaces during bootstrap"
			logger.Error(err, errorType)
			metrics.RecordError(constants.ComponentController, errorType)
			r.Config.MarkConfigMapsBootstrapFailed(err)
			return fmt.Errorf("failed to list namespaces: %w", err)
		}

		for _, ns := range namespaceList.Items {
			// Skip system namespace to avoid duplicate global config loading
			if ns.Name != systemNamespace {
				if ns.Annotations != nil {
					if value, ok := ns.Annotations[constants.NamespaceExcludeAnnotationKey]; ok {
						if value == constants.AnnotationValueTrue {
							continue // Skip excluded namespaces. Only for all-namespaces mode.
						}
					}

				}
				if ns.Labels != nil {
					if value, ok := ns.Labels[constants.NamespaceConfigEnabledLabelKey]; ok {
						if value == constants.AnnotationValueTrue {
							namespacesToScan = append(namespacesToScan, ns.Name)
						}
					}
				}
			}
		}
		logger.Info("Initial ConfigMap bootstrap", "namespaceCount", len(namespacesToScan))
	}

	// Add namespace-local ConfigMap targets
	for _, ns := range namespacesToScan {
		targets = append(targets,
			struct {
				name      string
				namespace string
				isGlobal  bool
			}{name: config.ScalingPolicyConfigMapName(), namespace: ns, isGlobal: false},
		)
	}

	// Bootstrap all target ConfigMaps
	for _, target := range targets {
		if r.Config.Unreadable(target.namespace) {
			// Left unmanaged while it cannot be read; reported where it was found.
			logger.Info("Skipping the ConfigMap bootstrap of a watched namespace this controller may not read",
				"namespace", target.namespace)
			continue
		}
		if err := r.bootstrapConfigMap(ctx, target.name, target.namespace, target.isGlobal); err != nil {
			r.Config.MarkConfigMapsBootstrapFailed(err)
			return err
		}
	}

	r.Config.MarkConfigMapsBootstrapComplete()
	logger.Info("Initial ConfigMap bootstrap completed", "targets", len(targets))
	return nil
}

// bootstrapClusterPolicy loads limiters and quotas from the policy namespace.
//
// A read failure here is FATAL to the bootstrap, unlike a missing tenant
// ConfigMap. The two are not the same kind of absence: if the policy ConfigMap
// simply does not exist, no bound was declared and running unbounded is what the
// admin asked for. But if it exists and cannot be read — almost always a missing
// RoleBinding in that namespace — then a bound WAS declared and this controller
// cannot see it. Starting anyway would run the tenant unbounded while the cluster
// admin has every reason to believe a quota is being enforced.
func (r *ConfigMapReconciler) bootstrapClusterPolicy(ctx context.Context) error {
	logger := log.FromContext(ctx)
	policyNS := r.Config.PolicyNamespace()

	name := config.ScalingPolicyConfigMapName()
	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: policyNS}, cm)
	if err == nil {
		if err := r.handleClusterPolicyConfigMap(ctx, cm); err != nil {
			logger.Error(err, "Cannot start: the cluster policy's default entry is invalid",
				"policyNamespace", policyNS, "configMap", name)
			r.Config.MarkConfigMapsBootstrapFailed(err)
			return err
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		errorType := "Failed to read cluster policy"
		logger.Error(err, errorType,
			"policyNamespace", policyNS, "configMap", name,
			"hint", policyReadRemedy(policyNS))
		metrics.RecordError(constants.ComponentController, errorType)
		r.Config.MarkConfigMapsBootstrapFailed(err)
		return fmt.Errorf("cannot read cluster policy from %s/%s, refusing to start unbounded: %w", policyNS, name, err)
	}

	// An EMPTY policy, not nil. Nil means "policy is not separated", which sends
	// the limiter accessors back to the ConfigMap in the controller's own namespace
	// — for a self-managed install, the one its own tenant writes. So mistyping the
	// ConfigMap name in the policy namespace would have handed limit-setting back to
	// the party the policy namespace exists to take it from, while logging that no
	// limiters were in force. An admin who published no policy means no bound, and
	// that is what an empty policy says.
	logger.Info("No cluster policy ConfigMap in the policy namespace: no limiters or quotas are in force",
		"policyNamespace", policyNS, "expectedName", name)
	r.Config.UpdateClusterPolicy(&config.ScalingPolicy{})
	r.Status.RecordClusterPolicy(r.Config, policyNS, name, "", nil)
	return nil
}

func (r *ConfigMapReconciler) bootstrapConfigMap(ctx context.Context, name, namespace string, isGlobal bool) error {
	logger := log.FromContext(ctx)
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, cm); err != nil {
		if apierrors.IsNotFound(err) {
			logger.V(1).Info("Bootstrap ConfigMap not found, continuing with defaults", "name", name, "namespace", namespace)
			return nil
		}
		return fmt.Errorf("failed to bootstrap ConfigMap %s/%s: %w", namespace, name, err)
	}

	if name == config.ScalingPolicyConfigMapName() {
		if err := r.handleScalingPolicyConfigMap(ctx, cm, namespace, isGlobal); err != nil {
			logger.Error(err, "Cannot start: the scaling policy's default entry is invalid",
				"namespace", namespace, "configMap", name)
			return err
		}
	} else {
		logger.V(1).Info("Ignoring unrecognized bootstrap ConfigMap", "name", name, "namespace", namespace)
	}

	return nil
}
