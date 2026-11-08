package variantmeta_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/variantmeta"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// testNamespace and testModelID are shared across the discovery specs.
const (
	testNamespace = "ns"
	testModelID   = "m1"
)

// buildVA constructs a synthesized Variant for tests.
func buildVA(name, scaleTargetName, variantCost string, minR *int32, maxR int32) llmdvariant.Variant {
	return llmdvariant.Variant{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: llmdvariant.Spec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: scaleTargetName},
			ModelID:        testModelID,
			MinReplicas:    minR,
			MaxReplicas:    maxR,
			ConfigSpec: llmdvariant.ConfigSpec{
				VariantCost: variantCost,
			},
		},
	}
}

// buildDeployment constructs a Deployment scale target with the given replica
// counts and optional role label.
func buildDeployment(specReplicas, statusReplicas, readyReplicas int32, role string) *appsv1.Deployment {
	d := &appsv1.Deployment{}
	d.Spec.Replicas = ptr.To(specReplicas)
	d.Status.Replicas = statusReplicas
	d.Status.ReadyReplicas = readyReplicas
	if role != "" {
		d.Spec.Template.Labels = map[string]string{"llm-d.ai/role": role}
	}
	return d
}

// targets wraps a single deployment accessor keyed as Discover expects.
func targets(scaleTargetName string, d *appsv1.Deployment) map[string]scaletarget.ScaleTargetAccessor {
	return map[string]scaletarget.ScaleTargetAccessor{
		utils.GetNamespacedKey(testNamespace, scaleTargetName): scaletarget.NewDeploymentAccessor(d),
	}
}

var _ = Describe("Discover", func() {
	ctx := context.Background()

	It("resolves identity, replica state, and cost from the VA + scale target", func() {
		va := buildVA("v1", "dep1", "2.5", ptr.To(int32(1)), 8)
		d := buildDeployment(3, 3, 2, "prefill")

		metas := variantmeta.Discover(ctx, []llmdvariant.Variant{va}, targets("dep1", d), nil, variantmeta.FromNodes)

		Expect(metas).To(HaveLen(1))
		m := metas[0]
		Expect(m.VariantName).To(Equal("v1"))
		Expect(m.ModelID).To(Equal(testModelID))
		Expect(m.Namespace).To(Equal("ns"))
		Expect(m.Role).To(Equal("prefill"))
		Expect(m.Cost).To(Equal(2.5))
		Expect(m.CurrentReplicas).To(Equal(3))
		Expect(m.ReadyReplicas).To(Equal(2))
		Expect(m.PendingReplicas).To(Equal(1))
		Expect(m.MinReplicas).NotTo(BeNil())
		Expect(*m.MinReplicas).To(Equal(1))
		Expect(m.MaxReplicas).NotTo(BeNil())
		Expect(*m.MaxReplicas).To(Equal(8))
	})

	It("defaults cost when unset or unparseable", func() {
		empty := buildVA("v1", "dep1", "", nil, 0)
		bad := buildVA("v2", "dep2", "not-a-number", nil, 0)
		d := buildDeployment(1, 1, 1, "")

		emptyMetas := variantmeta.Discover(ctx, []llmdvariant.Variant{empty}, targets("dep1", d), nil, variantmeta.FromNodes)
		badMetas := variantmeta.Discover(ctx, []llmdvariant.Variant{bad}, targets("dep2", d), nil, variantmeta.FromNodes)

		Expect(emptyMetas).To(HaveLen(1))
		Expect(emptyMetas[0].Cost).To(Equal(domain.DefaultVariantCost))
		Expect(badMetas).To(HaveLen(1))
		Expect(badMetas[0].Cost).To(Equal(domain.DefaultVariantCost))
	})

	It("falls back to spec replicas when status replicas is zero", func() {
		va := buildVA("v1", "dep1", "1", nil, 0)
		d := buildDeployment(2, 0, 0, "")

		metas := variantmeta.Discover(ctx, []llmdvariant.Variant{va}, targets("dep1", d), nil, variantmeta.FromNodes)

		Expect(metas).To(HaveLen(1))
		Expect(metas[0].CurrentReplicas).To(Equal(2))
		Expect(metas[0].PendingReplicas).To(Equal(2))
	})

	It("clamps pending replicas when ready exceeds current", func() {
		va := buildVA("v1", "dep1", "1", nil, 0)
		d := buildDeployment(1, 1, 5, "") // readyReplicas > statusReplicas

		metas := variantmeta.Discover(ctx, []llmdvariant.Variant{va}, targets("dep1", d), nil, variantmeta.FromNodes)

		Expect(metas).To(HaveLen(1))
		Expect(metas[0].PendingReplicas).To(Equal(0))
	})

	It("leaves MaxReplicas nil when unset (zero)", func() {
		va := buildVA("v1", "dep1", "1", nil, 0)
		d := buildDeployment(1, 1, 1, "")

		metas := variantmeta.Discover(ctx, []llmdvariant.Variant{va}, targets("dep1", d), nil, variantmeta.FromNodes)

		Expect(metas).To(HaveLen(1))
		Expect(metas[0].MinReplicas).To(BeNil())
		Expect(metas[0].MaxReplicas).To(BeNil())
	})

	It("skips a VA whose scale target cannot be resolved", func() {
		va := buildVA("v1", "dep1", "1", nil, 0)
		// No cached scale target and the live fetch returns NotFound → the VA is
		// skipped rather than producing a zero-valued record.
		scheme := runtime.NewScheme()
		Expect(appsv1.AddToScheme(scheme)).To(Succeed())
		c := fake.NewClientBuilder().WithScheme(scheme).Build()

		metas := variantmeta.Discover(ctx, []llmdvariant.Variant{va}, nil, c, variantmeta.FromNodes)

		Expect(metas).To(BeEmpty())
	})
})

var _ = Describe("RoleFromScaleTarget", func() {
	It("returns 'both' for a nil scale target", func() {
		Expect(variantmeta.RoleFromScaleTarget(nil)).To(Equal("both"))
	})

	It("maps the llm-d.ai/role label", func() {
		Expect(variantmeta.RoleFromScaleTarget(scaletarget.NewDeploymentAccessor(buildDeployment(1, 1, 1, "prefill")))).To(Equal("prefill"))
		Expect(variantmeta.RoleFromScaleTarget(scaletarget.NewDeploymentAccessor(buildDeployment(1, 1, 1, "decode")))).To(Equal("decode"))
		Expect(variantmeta.RoleFromScaleTarget(scaletarget.NewDeploymentAccessor(buildDeployment(1, 1, 1, "weird")))).To(Equal("both"))
		Expect(variantmeta.RoleFromScaleTarget(scaletarget.NewDeploymentAccessor(buildDeployment(1, 1, 1, "")))).To(Equal("both"))
	})
})

var _ = Describe("VariantMetadata.ToReplicaState", func() {
	It("projects the replica-state fields and drops identity/economics", func() {
		m := domain.VariantMetadata{
			VariantName:     "v1",
			ModelID:         "m1",
			Namespace:       "ns",
			Role:            "decode",
			Cost:            2.5,
			AcceleratorName: "H100",
			GPUsPerReplica:  2,
			CurrentReplicas: 4,
			DesiredReplicas: 5,
			ReadyReplicas:   3,
			PendingReplicas: 1,
			MinReplicas:     ptr.To(1),
			MaxReplicas:     ptr.To(8),
		}

		rs := m.ToReplicaState()
		Expect(rs.VariantName).To(Equal("v1"))
		Expect(rs.Role).To(Equal("decode"))
		Expect(rs.GPUsPerReplica).To(Equal(2))
		Expect(rs.CurrentReplicas).To(Equal(4))
		Expect(rs.DesiredReplicas).To(Equal(5))
		Expect(rs.PendingReplicas).To(Equal(1))
		Expect(rs.MinReplicas).To(Equal(m.MinReplicas))
		Expect(rs.MaxReplicas).To(Equal(m.MaxReplicas))
	})
})

// roleTargetWithArgs builds a Deployment accessor whose container declares args,
// optionally alongside a role label.
func roleTargetWithArgs(args []string, label string) scaletarget.ScaleTargetAccessor {
	labels := map[string]string{}
	if label != "" {
		labels[variantmeta.RoleLabel] = label
	}
	return scaletarget.NewDeploymentAccessor(&appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "server", Args: args}}},
			},
		},
	})
}

// The llm-d role label is the sole signal; engine arguments are deliberately not
// consulted, since the flag differs per engine.
var _ = Describe("RoleFromScaleTarget", func() {
	DescribeTable("resolves the P/D role from the llm-d role label",
		func(label string, want string) {
			Expect(variantmeta.RoleFromScaleTarget(roleTargetWithArgs(nil, label))).To(Equal(want))
		},
		Entry("prefill", domain.RolePrefill, domain.RolePrefill),
		Entry("decode", domain.RoleDecode, domain.RoleDecode),
		Entry("no label means non-disaggregated", "", domain.RoleBoth),
		Entry("an unrecognised value means non-disaggregated", "sidecar", domain.RoleBoth),
	)

	It("ignores engine arguments, whichever engine they belong to", func() {
		// SGLang spells this --disaggregation-mode and vLLM uses
		// --kv-transfer-config; neither is consulted, so a workload that declares
		// a role only through args reads as non-disaggregated.
		target := roleTargetWithArgs([]string{"--disaggregation-mode", "prefill"}, "")
		Expect(variantmeta.RoleFromScaleTarget(target)).To(Equal(domain.RoleBoth))
	})

	It("treats a nil scale target as non-disaggregated", func() {
		Expect(variantmeta.RoleFromScaleTarget(nil)).To(Equal(domain.RoleBoth))
	})
})

// Nodes are cluster-scoped, and reading them is the only thing in the normal path
// that needs cluster-scoped RBAC. It is worth doing exactly when a physical
// limiter charges the variant to an accelerator pool; with no such limiter the
// answer changes nothing (FitsGPUBudget asks whether ANY pool has room), so the
// read must not happen. That is what lets a namespace-scoped install run with no
// cluster-scoped permissions at all.
var _ = Describe("accelerator observation is conditional", func() {
	ctx := context.Background()

	// A client that fails every Node read, standing in for an install with no
	// permission on nodes. DeclaredOnly must never reach it; FromNodes must
	// tolerate the denial rather than fail the variant.
	newNodelessClient := func() client.Client {
		scheme := runtime.NewScheme()
		Expect(appsv1.AddToScheme(scheme)).To(Succeed())
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		return fake.NewClientBuilder().WithScheme(scheme).Build()
	}

	It("leaves the accelerator unresolved instead of reading nodes", func() {
		va := buildVA("v1", "dep1", "1", nil, 0)
		d := buildDeployment(1, 1, 1, "") // declares no accelerator

		metas := variantmeta.Discover(ctx, []llmdvariant.Variant{va},
			targets("dep1", d), newNodelessClient(), variantmeta.DeclaredOnly)

		Expect(metas).To(HaveLen(1))
		Expect(constants.IsAcceleratorResolved(metas[0].AcceleratorName)).To(BeFalse(),
			"with no physical limiter the accelerator stays unresolved, which is permissive; "+
				"resolving it would cost a cluster-scoped node read nothing consumes")
	})

	It("still produces a usable record when the node read is denied", func() {
		va := buildVA("v1", "dep1", "1", nil, 0)
		d := buildDeployment(1, 1, 1, "")

		metas := variantmeta.Discover(ctx, []llmdvariant.Variant{va},
			targets("dep1", d), newNodelessClient(), variantmeta.FromNodes)

		Expect(metas).To(HaveLen(1),
			"a denied or empty node read must degrade to an unresolved accelerator, "+
				"not drop the variant out of the fleet")
	})
})
