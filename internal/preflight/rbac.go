package preflight

import (
	"context"
	"fmt"
	"slices"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ControllerDeployment is the controller's Deployment.
const ControllerDeployment = "scaling-manager-controller-manager"

// The per-install ClusterRoles whose namespace reads are narrowed by
// resourceNames to the controller's namespace (decision 5). The rule index is
// the one that names namespaces in the shipped role.
var narrowedReaders = []struct {
	prefix    string
	ruleIndex int
	kueueOnly bool
	file      string
}{
	{"scaling-manager-policy-namespace-reader-role-", 0, false, "scaling-manager-namespaced.yaml"},
	{"scaling-manager-kueue-reader-role-", 1, true, "scaling-manager-kueue.yaml"},
}

// namespacedNeeds are what the controller does in each namespace it manages
// (the manager Role, config/components/tenant-installable).
var namespacedNeeds = []access{
	{verb: "list", resource: "pods"},
	{verb: "watch", resource: "pods"},
	{verb: "list", resource: "configmaps"},
	{verb: "list", resource: "services"},
	{verb: "list", group: "apps", resource: "deployments"},
	{verb: "watch", group: "apps", resource: "deployments"},
	{verb: "get", group: "keda.sh", resource: "scaledobjects"},
	{verb: "list", group: "inference.networking.k8s.io", resource: "inferencepools"},
	{verb: "create", resource: "events"},
}

// ownNeeds are what it does in its own namespace only: leader election and
// its status.
var ownNeeds = []access{
	{verb: "create", group: "coordination.k8s.io", resource: "leases"},
	{verb: "update", group: "coordination.k8s.io", resource: "leases"},
	{verb: "create", resource: "configmaps"},
	{verb: "update", resource: "configmaps", name: "scaling-manager-controller-status"},
}

// checkControllerRBAC asks, by SubjectAccessReview, whether the controller's
// ServiceAccount may do what it needs in each managed namespace. Before the
// install the account and its bindings do not exist yet, so the check says
// it was not made.
func checkControllerRBAC(ctx context.Context, s *state) ([]Result, string) {
	ns := s.env.Namespace
	switch exists, why := s.controllerSA(ctx); exists {
	case triNo:
		return []Result{{Status: Unknown, Detail: why + "; run preflight again after applying the install file to check its rights"}}, ""
	case triUnknown:
		return []Result{{Status: Unknown, Detail: why}}, ""
	}
	nss, named := s.managed()
	var asks []access
	for _, a := range ownNeeds {
		a.namespace = ns
		asks = append(asks, a)
	}
	if named {
		for _, m := range nss {
			for _, a := range namespacedNeeds {
				a.namespace = m
				asks = append(asks, a)
			}
		}
	} else {
		asks = append(asks, namespacedNeeds...) // namespace "": every namespace
	}
	st := s.loadSettings(ctx)
	limiters := st.parsed.LimiterTypes()
	nodes := access{verb: "list", resource: "nodes"}
	var denied []string
	for _, a := range asks {
		ok, err := s.can(ctx, a)
		if err != nil {
			return []Result{unknown("a SubjectAccessReview for "+s.ctrlUser(), err)}, ""
		}
		if !ok {
			denied = append(denied, a.String())
		}
	}
	nodeOK, err := s.can(ctx, nodes)
	if err != nil {
		return []Result{unknown("a SubjectAccessReview for "+s.ctrlUser(), err)}, ""
	}
	fixes := []string{fmt.Sprintf(`sed "s/__NAMESPACE__/%s/g" <the install file> | kubectl apply -f -`, ns)}
	if named {
		for _, m := range nss {
			if m != ns {
				fixes = append(fixes, fmt.Sprintf(`sed -e "s/__NAMESPACE__/%s/g" -e "s/__MODEL_NAMESPACE__/%s/g" scaling-manager-model-namespace.yaml | kubectl apply -f -`, ns, m))
			}
		}
	}
	reapply := strings.Join(fixes, "\n")
	if len(denied) > 0 {
		return []Result{{Status: Fail, Detail: fmt.Sprintf("%s may not: %s", ControllerServiceAccount, listText(denied, 8)), Fix: reapply}}, ""
	}
	if !nodeOK {
		if limiters.HasPhysical() {
			return []Result{{Status: Fail,
				Detail: "the limiter gpu-inventory needs the controller to list nodes, and it may not: every variant then gets no GPU budget and never scales up",
				Fix:    reapply + "   # the node-reader ClusterRole and its binding, part=admin"}}, ""
		}
		return []Result{{Status: Warn,
			Detail: ControllerServiceAccount + " may do what it needs, but not list nodes: accelerators stay unresolved (no gpu-inventory limiter is set, so nothing stops)"}}, ""
	}
	return []Result{{Status: Pass, Detail: fmt.Sprintf("%s may do the %d things it needs, and list nodes", ControllerServiceAccount, len(asks))}}, ""
}

// checkNamespaceReaders: the ClusterRoles that read Namespaces by name must
// list every managed namespace (decision 5). The shipped ones name only the
// controller's namespace, so each other one needs a patch, printed here.
func checkNamespaceReaders(ctx context.Context, s *state) ([]Result, string) {
	if s.env.Scope == ScopeCluster {
		return nil, "a cluster-wide install reads every namespace"
	}
	nss, _ := s.managed()
	st := s.loadSettings(ctx)
	kueue := st.value(keyKueue) == valueTrue
	var results []Result
	for _, r := range narrowedReaders {
		if r.kueueOnly && !kueue {
			continue
		}
		name := r.prefix + s.env.Namespace
		var have []string
		installed := true
		cr, err := s.env.Kube.RbacV1().ClusterRoles().Get(ctx, name, metav1.GetOptions{})
		switch err {
		case nil:
			var all bool
			if have, all = namespaceNames(cr); all {
				have = nss
			}
		default:
			absent, why := readVerdict(err, "ClusterRole "+name)
			if !absent {
				results = append(results, Result{Status: Unknown, Detail: why})
				continue
			}
			// Not applied yet: the file names the controller's namespace.
			installed = false
			have = []string{s.env.Namespace}
		}
		var missing []string
		for _, m := range nss {
			if !slices.Contains(have, m) {
				missing = append(missing, m)
			}
		}
		if len(missing) == 0 {
			results = append(results, Result{Status: Pass, Detail: fmt.Sprintf("ClusterRole %s reads %s", name, strings.Join(nss, ", "))})
			continue
		}
		var patches []string
		for _, m := range missing {
			patches = append(patches, fmt.Sprintf(`kubectl patch clusterrole %s --type json -p '[{"op":"add","path":"/rules/%d/resourceNames/-","value":"%s"}]'`, name, r.ruleIndex, m))
		}
		if !installed {
			results = append(results, Result{Status: Warn,
				Detail: fmt.Sprintf("ClusterRole %s (in %s, not applied yet) names only %s; watchNamespaces adds %s, which it will not read",
					name, r.file, s.env.Namespace, strings.Join(missing, ", ")),
				Fix: "after applying it:\n" + strings.Join(patches, "\n")})
			continue
		}
		results = append(results, Result{Status: Fail,
			Detail: fmt.Sprintf("ClusterRole %s does not name %s, which watchNamespaces manages: the controller cannot read its policy label there", name, strings.Join(missing, ", ")),
			Fix:    strings.Join(patches, "\n")})
	}
	return results, ""
}

// namespaceNames is the resourceNames of the role's rule on namespaces, and
// true when that rule names none: it then reads every namespace.
func namespaceNames(cr *rbacv1.ClusterRole) ([]string, bool) {
	for _, rule := range cr.Rules {
		if slices.Contains(rule.Resources, "namespaces") {
			return rule.ResourceNames, len(rule.ResourceNames) == 0
		}
	}
	return nil, false
}

// checkKueue: with kueue "true", the Kueue CRDs are installed, and the
// controller may read ClusterQueues (scaling-manager-kueue.yaml).
func checkKueue(ctx context.Context, s *state) ([]Result, string) {
	st := s.loadSettings(ctx)
	if st.unread != "" {
		return []Result{{Status: Unknown, Detail: "whether it applies is not known: " + st.unread}}, ""
	}
	if st.value(keyKueue) != valueTrue {
		return nil, `kueue is "false"`
	}
	var missing []string
	for _, crd := range []string{"clusterqueues.kueue.x-k8s.io", "localqueues.kueue.x-k8s.io", "resourceflavors.kueue.x-k8s.io", "workloads.kueue.x-k8s.io"} {
		if _, err := s.env.Dynamic.Resource(crdGVR).Get(ctx, crd, metav1.GetOptions{}); err != nil {
			absent, why := readVerdict(err, "CustomResourceDefinition "+crd)
			if !absent {
				return []Result{{Status: Unknown, Detail: why}}, ""
			}
			missing = append(missing, crd)
		}
	}
	if len(missing) > 0 {
		return []Result{{Status: Fail, Detail: "kueue is \"true\", and Kueue is not installed: no " + strings.Join(missing, ", "),
			Fix: fmt.Sprintf(`kubectl patch configmap %s -n %s --type merge -p '{"data":{"kueue":"false"}}'   # or install Kueue`, SettingsConfigMap, s.env.Namespace)}}, ""
	}
	switch exists, why := s.controllerSA(ctx); exists {
	case triNo:
		return []Result{{Status: Pass, Detail: "Kueue's CRDs are installed (the controller's read is checked after the install)"}}, ""
	case triUnknown:
		return []Result{{Status: Unknown, Detail: "Kueue's CRDs are installed; whether the controller may read ClusterQueues is not known: " + why}}, ""
	}
	ok, err := s.can(ctx, access{verb: "list", group: "kueue.x-k8s.io", resource: "clusterqueues"})
	if err != nil {
		return []Result{unknown("a SubjectAccessReview for "+s.ctrlUser(), err)}, ""
	}
	if !ok {
		return []Result{{Status: Fail, Detail: "the controller may not list ClusterQueues: the Kueue quota is not read",
			Fix: fmt.Sprintf(`sed -e "s/__NAMESPACE__/%s/g" -e "s/__KUEUE_NAMESPACE__/%s/g" scaling-manager-kueue.yaml | kubectl apply -f -`, s.env.Namespace, st.value(keyKueueNamespace))}}, ""
	}
	return []Result{{Status: Pass, Detail: "Kueue's CRDs are installed, and the controller may list ClusterQueues"}}, ""
}

// checkActivation: with utilizationShare "active", the controller may patch
// pods in every namespace that shares (scaling-manager-activate.yaml, the
// only grant of it; decision 3).
func checkActivation(ctx context.Context, s *state) ([]Result, string) {
	st := s.loadSettings(ctx)
	if st.unread != "" {
		return []Result{{Status: Unknown, Detail: "whether it applies is not known: " + st.unread}}, ""
	}
	if st.value(keyUtilizationShare) != shareActive {
		return nil, fmt.Sprintf("utilizationShare is %q", st.value(keyUtilizationShare))
	}
	nss, named := s.managed()
	if !named {
		pods, why := s.modelServers(ctx)
		if pods == nil {
			return []Result{{Status: Unknown, Detail: "which namespaces share is not known: " + why}}, ""
		}
		nss = sortedKeys(pods)
	}
	var missing []string
	for _, m := range nss {
		ok, err := s.can(ctx, access{verb: "patch", resource: "pods", namespace: m})
		if err != nil {
			return []Result{unknown("a SubjectAccessReview for "+s.ctrlUser(), err)}, ""
		}
		if !ok {
			missing = append(missing, m)
		}
	}
	if len(missing) == 0 {
		return []Result{{Status: Pass, Detail: "the controller may patch pods in " + listText(nss, 8)}}, ""
	}
	var fix []string
	for _, m := range missing {
		fix = append(fix, fmt.Sprintf(`sed -e "s/__NAMESPACE__/%s/g" -e "s/__MODEL_NAMESPACE__/%s/g" scaling-manager-activate.yaml | kubectl apply -f -`, s.env.Namespace, m))
	}
	return []Result{{Status: Fail,
		Detail: "utilizationShare is \"active\", and the controller may not patch pods in " + strings.Join(missing, ", ") + ": sharing cannot mark or move a replica there",
		Fix:    strings.Join(fix, "\n")}}, ""
}
