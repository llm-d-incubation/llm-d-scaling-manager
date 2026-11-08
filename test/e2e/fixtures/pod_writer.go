package fixtures

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/llm-d-incubation/llm-d-scaling-manager/test/utils"
)

// PodWriterRole is the Role that lets the controller patch pods in one
// namespace. The controller's ClusterRole only reads pods; deploy/warmpool.sh
// and deploy/utilization-share.sh create this Role where a warm pool or the
// utilization-share optimizer runs (deploy/lib/pod_writer.sh). The names here
// must match that file.
const PodWriterRole = "scaling-manager-pod-writer"

// GrantPodWrites gives the controller ServiceAccount (scaling-manager-controller-manager in
// controllerNamespace) patch on pods in namespace, the way the scripts an
// operator runs do. A spec that lends warm-pool Pods or moves deletion costs
// needs it in the namespace it uses; without it a cluster-scoped controller
// refuses the pool and fails every donor mark. Idempotent.
func GrantPodWrites(ctx context.Context, client kubernetes.Interface, namespace, controllerNamespace string) error {
	labels := map[string]string{"app.kubernetes.io/name": "scaling-manager"}
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: PodWriterRole, Namespace: namespace, Labels: labels},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"pods"},
			Verbs:     []string{"patch"},
		}},
	}
	if _, err := client.RbacV1().Roles(namespace).Create(ctx, role, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating role %s/%s: %w", namespace, PodWriterRole, err)
	}

	sum := sha256.Sum256([]byte(controllerNamespace))
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      PodWriterRole + "-" + hex.EncodeToString(sum[:])[:8],
			Namespace: namespace,
			Labels:    labels,
		},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: PodWriterRole},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      utils.ControllerServiceAccountName,
			Namespace: controllerNamespace,
		}},
	}
	if _, err := client.RbacV1().RoleBindings(namespace).Create(ctx, binding, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating rolebinding %s/%s: %w", namespace, binding.Name, err)
	}
	return nil
}
