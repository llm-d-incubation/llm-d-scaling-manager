package variantmeta

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

func TestPodPlacements(t *testing.T) {
	rdma := map[string]string{"network": "rdma"}
	rdmaZone := map[string]string{"network": "rdma", "zone": "z1"}

	if got := podPlacements(scaletarget.NewDeploymentAccessor(&appsv1.Deployment{})); got != nil {
		t.Errorf("an unconstrained Deployment has placements %+v, want nil", got)
	}

	d := &appsv1.Deployment{}
	d.Spec.Template.Spec.NodeSelector = rdma
	got := podPlacements(scaletarget.NewDeploymentAccessor(d))
	if len(got) != 1 || got[0].NodeSelector["network"] != "rdma" {
		t.Errorf("a pinned Deployment has placements %+v, want its nodeSelector", got)
	}

	size := int32(2)
	lws := &lwsv1.LeaderWorkerSet{Spec: lwsv1.LeaderWorkerSetSpec{LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{
		Size:           &size,
		LeaderTemplate: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{NodeSelector: rdma}},
		WorkerTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{NodeSelector: rdmaZone}},
	}}}
	got = podPlacements(scaletarget.NewLWSAccessor(lws))
	if len(got) != 2 || got[1].NodeSelector["zone"] != "z1" {
		t.Errorf("an LWS whose worker template differs has placements %+v, want leader then worker", got)
	}

	// An unconstrained leader keeps its place: the worker's entry is second.
	lws.Spec.LeaderWorkerTemplate.LeaderTemplate = &corev1.PodTemplateSpec{}
	got = podPlacements(scaletarget.NewLWSAccessor(lws))
	if len(got) != 2 || len(got[0].NodeSelector) != 0 || got[1].NodeSelector["zone"] != "z1" {
		t.Errorf("an LWS with an unconstrained leader has placements %+v, want a zero leader then the worker", got)
	}

	lws.Spec.LeaderWorkerTemplate.LeaderTemplate = nil
	if got = podPlacements(scaletarget.NewLWSAccessor(lws)); len(got) != 1 {
		t.Errorf("an LWS without a leader template has placements %+v, want the worker's once", got)
	}

	// The placement is a copy: editing the template after discovery changes nothing.
	got = podPlacements(scaletarget.NewDeploymentAccessor(d))
	d.Spec.Template.Spec.NodeSelector = map[string]string{"network": "tcp"}
	rdma["network"] = "tcp"
	if got[0].NodeSelector["network"] != "rdma" {
		t.Errorf("the placement shares the template's nodeSelector: %+v", got)
	}
}
