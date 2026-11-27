package dsetwriter

import (
	"context"
	"fmt"
	"slices"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// GrantLabel labels the per-namespace grants of scaling-manager-grants.yaml;
// GrantName is the role writer's value.
const (
	GrantLabel = "scaling-manager.llmd.ai/grant"
	GrantName  = "dset-role-writer"
)

// GrantedNamespaces returns a Writer.NamespacesFunc: the namespaces holding a
// RoleBinding labelled GrantLabel=GrantName that binds serviceAccount in
// controllerNamespace, which manages admits, sorted. reader lists
// RoleBindings across the cluster.
func GrantedNamespaces(reader client.Reader, controllerNamespace, serviceAccount string,
	manages func(namespace string) bool) func(ctx context.Context) ([]string, error) {
	return func(ctx context.Context) ([]string, error) {
		var list rbacv1.RoleBindingList
		if err := reader.List(ctx, &list, client.MatchingLabels{GrantLabel: GrantName}); err != nil {
			return nil, fmt.Errorf("listing RoleBindings labelled %s=%s: %w", GrantLabel, GrantName, err)
		}
		var out []string
		for _, b := range list.Items {
			if slices.Contains(out, b.Namespace) || (manages != nil && !manages(b.Namespace)) {
				continue
			}
			if slices.ContainsFunc(b.Subjects, func(s rbacv1.Subject) bool {
				return s.Kind == rbacv1.ServiceAccountKind && s.Name == serviceAccount && s.Namespace == controllerNamespace
			}) {
				out = append(out, b.Namespace)
			}
		}
		slices.Sort(out)
		return out, nil
	}
}
