package collector

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A Pod's template revision is read from the label its controller stamps it
// under: a LeaderWorkerSet's, a Deployment's ReplicaSet's, a StatefulSet's.
func TestPodRevisionReadsTheTemplateRevision(t *testing.T) {
	pod := func(labels map[string]string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: labels}}
	}
	for _, c := range []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{"leaderworkerset", map[string]string{
			"leaderworkerset.sigs.k8s.io/template-revision-hash": "lws-1",
			"controller-revision-hash":                           "sts-1",
		}, "lws-1"},
		{"deployment", map[string]string{"pod-template-hash": "rs-1"}, "rs-1"},
		{"statefulset", map[string]string{"controller-revision-hash": "sts-1"}, "sts-1"},
		{"none", map[string]string{"app": "x"}, ""},
	} {
		if got := podRevision(pod(c.labels)); got != c.want {
			t.Errorf("%s: podRevision = %q, want %q", c.name, got, c.want)
		}
	}
}
