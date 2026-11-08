package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

// ResolvePolicyNamespace decides where this controller reads limiters and quotas
// from, and records the decision on cfg. It runs once, before the manager starts.
//
// Order:
//
//  1. the scaling-manager.llmd.ai/policy-namespace LABEL on the managed Namespace
//  2. the well-known namespace, config.WellKnownPolicyNamespace — self-managed only
//  3. the controller's own namespace
//
// Steps 1 and 2 need `get namespaces`, which a namespace-scoped install's Role
// cannot grant: Namespaces are cluster-scoped. The prereqs phase grants it with a
// per-install ClusterRole (components/policy-namespace-reader). An install
// without that grant still starts on its own ConfigMap, as before, but the source
// is recorded as config.PolicySourceLocalUnverified — logged with the remedy and
// published as llmsm_policy_namespace_unverified — because "could not look" is not
// "no policy was published".
//
// Rule 1 is an annotation on a NAMESPACE because that object is cluster-scoped: a
// namespace admin holds RBAC inside their namespace and cannot edit it. So an
// admin can direct a controller they did not deploy.
//
// # What this is and is not
//
// This is a GUARDRAIL, not an enforcement boundary, and the distinction is worth
// being blunt about because an earlier version of this function got it wrong.
//
// That version refused to start when a controller managed its own namespace and
// found no policy — reasoning that such a controller is owned by the tenant it is
// supposed to bound, so it must not set its own limits. Both halves of that were
// wrong in practice:
//
//   - it did not work. `selfManaged` is derived from --watch-namespace, an
//     argument on the Deployment the tenant owns. Deleting one flag made the
//     controller look admin-owned and skipped the check entirely. A gate whose
//     trigger is self-reported by its subject is not a gate.
//   - it broke everyone else. Namespace scope is the DEFAULT on OpenShift, so
//     every such install — with no limiter configured and nothing to enforce —
//     would have refused to start until someone created a namespace they had
//     never heard of.
//
// Maximum breakage, zero containment. A tenant who owns the controller's
// Deployment owns its image, so no in-process rule can bind them; a check that
// pretends otherwise mainly misleads the admin relying on it.
//
// Enforcement against a tenant you do not trust belongs at admission, where it
// cannot be argued with: a ResourceQuota on the GPU resource in their namespace.
// What the scaling manager can honestly offer is that the budget is authoritative for every
// controller actually running the scaling manager — which is misconfiguration, drift and copied
// manifests, the things that occur far more often than malice — and, for the
// arrangement where the tenant does NOT own the controller (it runs in an admin
// namespace with LLMSM_WATCH_NS pointed at theirs), a bound that genuinely holds.
func ResolvePolicyNamespace(ctx context.Context, c client.Reader, cfg *config.Config) error {
	logger := log.FromContext(ctx)
	systemNS := config.SystemNamespace()
	managedNS := cfg.WatchNamespace()

	// The tenant-owned shape: this controller manages exactly the namespace it
	// lives in, so whoever administers that namespace administers this controller.
	selfManaged := managedNS != "" && managedNS == systemNS

	wellKnownExists, err := namespaceExists(ctx, c, config.WellKnownPolicyNamespace)
	// Forbidden is NOT absent. A namespace-scoped controller holds only a Role,
	// which cannot reach a cluster-scoped Namespace, so folding the denial into
	// "does not exist" made every such controller ignore a policy namespace an
	// admin had published — silently, while the admin believed it was bounded.
	// It is carried forward as its own answer and decided on below.
	wellKnownDenied := apierrors.IsForbidden(err)
	if err != nil && !wellKnownDenied {
		// Never guess. Falling back on an API error would quietly downgrade a
		// cluster-imposed bound to a self-chosen one, and nothing afterwards would
		// show that this controller came up under the weaker rule.
		return fmt.Errorf("cannot determine whether the %s namespace exists, refusing to guess which policy applies: %w",
			config.WellKnownPolicyNamespace, err)
	}

	if !selfManaged {
		// An admin-owned controller keeps reading its OWN ConfigMap unless it was
		// explicitly told otherwise. The well-known namespace deliberately does NOT
		// win here.
		//
		// Letting it win looked tidier and silently broke working installs: a
		// cluster-scoped controller, correctly bounded by limiters in its own
		// admin-owned ConfigMap, would switch sources the moment anyone created
		// scaling-manager-policy for an unrelated tenant. If that namespace's ConfigMap carried
		// a default entry with no limiters — the normal way to say "I have not
		// configured this yet" — the switch would take the cluster from bounded to
		// UNBOUNDED, with no edit to the install that changed.
		//
		// The override only earns its keep where the reader might otherwise choose
		// its own limits, which is the branch below.
		cfg.SetPolicyNamespace(systemNS, config.PolicySourceLocal)
		logger.V(1).Info("Cluster policy comes from this controller's own namespace", "namespace", systemNS)
		if wellKnownExists {
			logger.Info("A "+config.WellKnownPolicyNamespace+" namespace exists but is NOT used here: this controller does not manage its own namespace, so its own ConfigMap is authoritative.",
				"policyNamespace", config.WellKnownPolicyNamespace)
		}
		if wellKnownDenied {
			// Harmless here: the answer would not change the source.
			logger.V(1).Info("Cannot read the "+config.WellKnownPolicyNamespace+" namespace; it would not be used by this controller either way",
				"reason", err.Error())
		}
		return nil
	}

	// Self-managed: the namespace this controller runs in is the one it manages, so
	// an admin may have annotated that Namespace to direct it. Reading it is
	// best-effort — a controller with no namespace read is an ordinary
	// namespace-scoped deployment, and refusing to start over it would break the
	// default shape on OpenShift for no gain.
	ns := &corev1.Namespace{}
	managedDenied := false
	if err := c.Get(ctx, client.ObjectKey{Name: managedNS}, ns); err != nil {
		if !apierrors.IsNotFound(err) && !apierrors.IsForbidden(err) {
			return fmt.Errorf("failed reading namespace %s to resolve cluster policy: %w", managedNS, err)
		}
		managedDenied = apierrors.IsForbidden(err)
		// Info, not V(1): this is the difference between "no policy applies" and
		// "policy may apply and I cannot see it", and an admin who labelled this
		// namespace has no other way to find out the label was never read.
		logger.Info("Could not read the managed Namespace, so any scaling-manager.llmd.ai/policy-namespace label on it "+
			"has NOT been applied. To fix: "+policyNamespaceReaderRemedy,
			"namespace", managedNS, "reason", err.Error())
	}

	// Label first, then annotation. The label is the selectable form — an admin can
	// ask `kubectl get ns -l scaling-manager.llmd.ai/policy-namespace=X` which namespaces a
	// policy governs — so it is the one to prefer when both are somehow present.
	if pointed := ns.Labels[constants.PolicyNamespaceLabelKey]; pointed != "" {
		cfg.SetPolicyNamespace(pointed, config.PolicySourceWellKnown)
		logger.Info("Cluster policy comes from the namespace this one is labelled with",
			"policyNamespace", pointed, "label", constants.PolicyNamespaceLabelKey)
		return nil
	}

	if wellKnownExists {
		cfg.SetPolicyNamespace(config.WellKnownPolicyNamespace, config.PolicySourceWellKnown)
		logger.Info("Cluster policy comes from the well-known namespace", "policyNamespace", config.WellKnownPolicyNamespace)
		return nil
	}

	if wellKnownDenied || managedDenied {
		// The fallback is today's behaviour — an install that predates the
		// namespace-reader grant must keep starting — but it is no longer
		// silent. The source is recorded as UNVERIFIED, which publishes
		// llmsm_policy_namespace_unverified=1, and the log says what to grant.
		cfg.SetPolicyNamespace(systemNS, config.PolicySourceLocalUnverified)
		logger.Info("Cannot tell whether a cluster admin published policy for this controller: reading Namespaces is Forbidden. "+
			"Falling back to this controller's own ConfigMap, which may be WEAKER than the policy in force. To fix: "+
			policyNamespaceReaderRemedy,
			"wellKnownPolicyNamespace", config.WellKnownPolicyNamespace,
			"wellKnownReadDenied", wellKnownDenied,
			"managedNamespaceReadDenied", managedDenied,
			"fallbackNamespace", systemNS,
			"metric", constants.LLMSMPolicyNamespaceUnverified)
		return nil
	}

	cfg.SetPolicyNamespace(systemNS, config.PolicySourceLocal)
	logger.V(1).Info("Cluster policy comes from this controller's own namespace", "namespace", systemNS)
	return nil
}

// policyNamespaceReaderRemedy names the grant that lets a namespace-scoped
// controller resolve its policy namespace, and the command that creates it.
const policyNamespaceReaderRemedy = "grant this ServiceAccount get on namespaces " +
	"(ClusterRole scaling-manager-policy-namespace-reader-role-<suffix> and its ClusterRoleBinding, " +
	"created by 'NAMESPACE=<this namespace> make setup-prereqs' or 'make enable-physical-limiter'), then restart the controller"

// policyReadRemedy says how to let this controller read the policy ConfigMap in
// policyNS. For a separate policy namespace that is the Role/RoleBinding pair
// `make enable-physical-limiter` and `make setup-prereqs` create there.
func policyReadRemedy(policyNS string) string {
	systemNS := config.SystemNamespace()
	if policyNS == systemNS {
		return "the controller's ServiceAccount needs get/list/watch on ConfigMaps in this namespace"
	}
	return fmt.Sprintf("the controller's ServiceAccount needs get/list/watch on ConfigMaps in %[1]s: "+
		"Role scaling-manager-policy-reader and RoleBinding scaling-manager-policy-reader-<suffix> in %[1]s, bound to this controller's ServiceAccount. "+
		"A cluster admin creates both with 'NAMESPACE=%[2]s make setup-prereqs' (or 'make enable-physical-limiter'); "+
		"the next container restart picks it up", policyNS, systemNS)
}

// namespaceExists reports whether a namespace is present.
//
// Only NotFound means absent. Forbidden is returned as an error, because a
// controller that cannot see a namespace does not know whether it exists, and
// the caller has to say so rather than act as though it had looked.
func namespaceExists(ctx context.Context, c client.Reader, name string) (bool, error) { //nolint:unparam // one caller today; the name stays a parameter so the lookup is testable on its own
	ns := &corev1.Namespace{}
	err := c.Get(ctx, client.ObjectKey{Name: name}, ns)
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err):
		return false, nil
	default:
		return false, err
	}
}
