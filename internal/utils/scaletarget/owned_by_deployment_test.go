package scaletarget

import (
	"testing"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestOwnedByDeployment(t *testing.T) {
	yes, no := true, false
	pod := func(hash string, owners ...metav1.OwnerReference) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", OwnerReferences: owners}}
		if hash != "" {
			p.Labels = map[string]string{appsv1.DefaultDeploymentUniqueLabelKey: hash}
		}
		return p
	}
	rs := func(name string, controller *bool) metav1.OwnerReference {
		return metav1.OwnerReference{Kind: "ReplicaSet", Name: name, Controller: controller}
	}

	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{"its own ReplicaSet", pod("7d9f", rs("app-7d9f", &yes)), true},
		{"no hash label", pod("", rs("app-7d9f", &yes)), false},
		{"no owner", pod("7d9f"), false},
		{"owner is not the controller", pod("7d9f", rs("app-7d9f", &no)), false},
		{"controller flag unset", pod("7d9f", rs("app-7d9f", nil)), false},
		{"owner is not a ReplicaSet", pod("7d9f",
			metav1.OwnerReference{Kind: "StatefulSet", Name: "app-7d9f", Controller: &yes}), false},
		{"another Deployment's ReplicaSet", pod("7d9f", rs("other-7d9f", &yes)), false},
		{"a Deployment whose name starts with this one", pod("88aa", rs("app-canary-88aa", &yes)), false},
		{"hash does not match the owner", pod("88aa", rs("app-7d9f", &yes)), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, OwnedByDeployment(tt.pod, "app"))
		})
	}
}
