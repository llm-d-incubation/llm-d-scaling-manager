package controller

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

// The property under test is one sentence: a controller cannot grant itself the
// right to scale without limits.
//
// It matters for a specific deployment shape — a namespace-scoped controller
// running IN the namespace it manages — because there the namespace's own
// administrator owns the controller's Deployment, and therefore its flags, its
// env and its ServiceAccount. Every input that decides the outcome below is a
// cluster-scoped object for exactly that reason: a namespace admin holds RBAC
// inside their namespace, and can neither create a Namespace nor annotate one.
var _ = Describe("ResolvePolicyNamespace", func() {
	const tenantNS = "team-a"

	newScheme := func() *runtime.Scheme {
		s := runtime.NewScheme()
		Expect(corev1.AddToScheme(s)).To(Succeed())
		return s
	}

	namespace := func(name string, annotations map[string]string) *corev1.Namespace {
		return &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: annotations},
		}
	}

	labelled := func(name string, labels map[string]string) *corev1.Namespace {
		return &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		}
	}

	// selfManaged builds the tenant-owned shape: POD_NAMESPACE and the watched
	// namespace are the same, which is what the namespace-scoped overlay produces
	// by default.
	selfManaged := func() *config.Config {
		c := &config.Config{}
		config.SetWatchNamespaceForTest(c, tenantNS)
		return c
	}

	BeforeEach(func() {
		GinkgoT().Setenv("POD_NAMESPACE", tenantNS)
	})

	Context("when the controller manages the namespace it runs in", func() {
		// The default namespace-scoped install — which is what OpenShift gets
		// without asking for it — must come up on a cluster that has never heard
		// of a policy namespace.
		It("starts normally when no policy has been published anywhere", func() {
			c := fake.NewClientBuilder().WithScheme(newScheme()).
				WithObjects(namespace(tenantNS, nil)).Build()

			cfg := selfManaged()
			Expect(ResolvePolicyNamespace(context.Background(), c, cfg)).To(Succeed())

			Expect(cfg.PolicySource()).To(Equal(config.PolicySourceLocal))
			Expect(cfg.PolicyNamespaceIsSeparate()).To(BeFalse())
		})

		// The selectable form, and the one an admin is most likely to reach for:
		// a label can be listed across the cluster to audit what a policy governs.
		It("takes the policy namespace an admin labelled onto the namespace", func() {
			c := fake.NewClientBuilder().WithScheme(newScheme()).
				WithObjects(labelled(tenantNS, map[string]string{
					constants.PolicyNamespaceLabelKey: "platform-policy",
				})).Build()

			cfg := selfManaged()
			Expect(ResolvePolicyNamespace(context.Background(), c, cfg)).To(Succeed())

			Expect(cfg.PolicyNamespace()).To(Equal("platform-policy"))
			Expect(cfg.PolicyNamespaceIsSeparate()).To(BeTrue())
		})

		It("uses the well-known namespace when it exists", func() {
			c := fake.NewClientBuilder().WithScheme(newScheme()).
				WithObjects(
					namespace(tenantNS, nil),
					namespace(config.WellKnownPolicyNamespace, nil),
				).Build()

			cfg := selfManaged()
			Expect(ResolvePolicyNamespace(context.Background(), c, cfg)).To(Succeed())

			Expect(cfg.PolicyNamespace()).To(Equal(config.WellKnownPolicyNamespace))
			Expect(cfg.PolicySource()).To(Equal(config.PolicySourceWellKnown))
		})

		// The annotation outranks the well-known namespace, so an admin can move one
		// namespace's policy without disturbing the cluster-wide default.
		It("prefers the namespace label over the well-known namespace", func() {
			c := fake.NewClientBuilder().WithScheme(newScheme()).
				WithObjects(
					labelled(tenantNS, map[string]string{
						constants.PolicyNamespaceLabelKey: "platform-policy",
					}),
					namespace(config.WellKnownPolicyNamespace, nil),
				).Build()

			cfg := selfManaged()
			Expect(ResolvePolicyNamespace(context.Background(), c, cfg)).To(Succeed())
			Expect(cfg.PolicyNamespace()).To(Equal("platform-policy"))
		})

		// A controller with no namespace read is an ordinary namespace-scoped
		// deployment, not a suspicious one. It must still start.
		It("starts when it cannot read the namespace it manages", func() {
			c := fake.NewClientBuilder().WithScheme(newScheme()).Build() // namespace absent

			cfg := selfManaged()
			Expect(ResolvePolicyNamespace(context.Background(), c, cfg)).To(Succeed())
			Expect(cfg.PolicySource()).To(Equal(config.PolicySourceLocal))
		})
	})

	// A namespace-scoped controller holds only a Role, and a Role cannot reach a
	// cluster-scoped Namespace. Reporting that denial as "the policy namespace does
	// not exist" made every such controller ignore the policy an admin published
	// with `make enable-physical-limiter` — silently.
	Context("when reading Namespaces is Forbidden", func() {
		It("falls back to its own ConfigMap but records the source as unverified", func() {
			c := denyingNamespaces(newScheme(), func(string) bool { return true },
				namespace(tenantNS, nil), namespace(config.WellKnownPolicyNamespace, nil))

			cfg := selfManaged()
			Expect(ResolvePolicyNamespace(context.Background(), c, cfg)).To(Succeed())

			Expect(cfg.PolicySource()).To(Equal(config.PolicySourceLocalUnverified))
			Expect(cfg.PolicyNamespace()).To(Equal(tenantNS))
			Expect(cfg.PolicyNamespaceIsSeparate()).To(BeFalse())
		})

		It("records unverified when only the well-known namespace is denied", func() {
			c := denyingNamespaces(newScheme(),
				func(name string) bool { return name == config.WellKnownPolicyNamespace },
				namespace(tenantNS, nil))

			cfg := selfManaged()
			Expect(ResolvePolicyNamespace(context.Background(), c, cfg)).To(Succeed())
			Expect(cfg.PolicySource()).To(Equal(config.PolicySourceLocalUnverified))
		})

		It("still uses the well-known namespace when only the managed one is denied", func() {
			c := denyingNamespaces(newScheme(),
				func(name string) bool { return name == tenantNS },
				namespace(config.WellKnownPolicyNamespace, nil))

			cfg := selfManaged()
			Expect(ResolvePolicyNamespace(context.Background(), c, cfg)).To(Succeed())
			Expect(cfg.PolicyNamespace()).To(Equal(config.WellKnownPolicyNamespace))
			Expect(cfg.PolicySource()).To(Equal(config.PolicySourceWellKnown))
		})

		// The denial changes nothing for an admin-owned controller, whose own
		// ConfigMap is authoritative either way, so it is not flagged.
		It("does not flag an admin-owned controller", func() {
			GinkgoT().Setenv("POD_NAMESPACE", "scaling-manager-system")
			c := denyingNamespaces(newScheme(), func(string) bool { return true })

			cfg := &config.Config{}
			config.SetWatchNamespaceForTest(cfg, tenantNS)
			Expect(ResolvePolicyNamespace(context.Background(), c, cfg)).To(Succeed())
			Expect(cfg.PolicySource()).To(Equal(config.PolicySourceLocal))
		})

		// Distinct from the Forbidden case: an error that says nothing about
		// permission is still a reason to refuse rather than guess.
		It("refuses to start on any other error reading the well-known namespace", func() {
			c := fake.NewClientBuilder().WithScheme(newScheme()).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
						return apierrors.NewInternalError(errors.New("etcd unavailable"))
					},
				}).Build()

			Expect(ResolvePolicyNamespace(context.Background(), c, selfManaged())).NotTo(Succeed())
		})
	})

	Context("when the controller does not manage its own namespace", func() {
		// An admin-run controller — cluster-scoped, or namespace-scoped from
		// outside — is already beyond the tenant's reach, so it is not made to
		// prove anything.
		It("starts without policy, because the tenant does not own it", func() {
			GinkgoT().Setenv("POD_NAMESPACE", "scaling-manager-system")
			c := fake.NewClientBuilder().WithScheme(newScheme()).
				WithObjects(namespace(tenantNS, nil)).Build()

			cfg := &config.Config{}
			config.SetWatchNamespaceForTest(cfg, tenantNS)

			Expect(ResolvePolicyNamespace(context.Background(), c, cfg)).To(Succeed())
			Expect(cfg.PolicySource()).To(Equal(config.PolicySourceLocal))
		})

		// Regression guard. Letting the well-known namespace win here took a
		// working, correctly-bounded cluster-scoped install and silently switched
		// its policy source the moment someone created scaling-manager-policy for an unrelated
		// tenant — and if that ConfigMap declared no limiters, the cluster went
		// from bounded to UNBOUNDED with no edit to the install that changed.
		It("does NOT let the well-known namespace hijack an admin-owned controller", func() {
			GinkgoT().Setenv("POD_NAMESPACE", "scaling-manager-system")
			c := fake.NewClientBuilder().WithScheme(newScheme()).
				WithObjects(namespace(config.WellKnownPolicyNamespace, nil)).Build()

			cfg := &config.Config{}
			Expect(ResolvePolicyNamespace(context.Background(), c, cfg)).To(Succeed())

			Expect(cfg.PolicySource()).To(Equal(config.PolicySourceLocal))
			Expect(cfg.PolicyNamespaceIsSeparate()).To(BeFalse())
		})
	})
})

// denyingNamespaces builds a client whose Namespace Gets return Forbidden for
// every name denied() accepts, the way the API server answers a ServiceAccount
// that holds only a namespaced Role.
func denyingNamespaces(s *runtime.Scheme, denied func(string) bool, objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isNS := obj.(*corev1.Namespace); isNS && denied(key.Name) {
					return apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, key.Name,
						errors.New("namespaces is forbidden: cannot get resource \"namespaces\" at the cluster scope"))
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
}

var _ = Describe("namespaceExists", func() {
	newScheme := func() *runtime.Scheme {
		s := runtime.NewScheme()
		Expect(corev1.AddToScheme(s)).To(Succeed())
		return s
	}

	It("reports a present namespace", func() {
		c := fake.NewClientBuilder().WithScheme(newScheme()).
			WithObjects(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "scaling-manager-policy"}}).Build()
		exists, err := namespaceExists(context.Background(), c, "scaling-manager-policy")
		Expect(err).NotTo(HaveOccurred())
		Expect(exists).To(BeTrue())
	})

	It("reports NotFound as absent", func() {
		c := fake.NewClientBuilder().WithScheme(newScheme()).Build()
		exists, err := namespaceExists(context.Background(), c, "scaling-manager-policy")
		Expect(err).NotTo(HaveOccurred())
		Expect(exists).To(BeFalse())
	})

	// Forbidden is not absent: the caller must be able to tell "I looked and it
	// is not there" from "I was not allowed to look".
	It("reports Forbidden as an error, not as absent", func() {
		c := denyingNamespaces(newScheme(), func(string) bool { return true },
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "scaling-manager-policy"}})
		exists, err := namespaceExists(context.Background(), c, "scaling-manager-policy")
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "got err=%v", err)
		Expect(exists).To(BeFalse())
	})
})

var _ = Describe("policyReadRemedy", func() {
	It("names the RoleBinding and the command when policy lives in another namespace", func() {
		GinkgoT().Setenv("POD_NAMESPACE", "team-a")
		got := policyReadRemedy("scaling-manager-policy")
		Expect(got).To(ContainSubstring("RoleBinding scaling-manager-policy-reader-"))
		Expect(got).To(ContainSubstring("NAMESPACE=team-a make setup-prereqs"))
	})

	It("keeps the plain hint for the controller's own namespace", func() {
		GinkgoT().Setenv("POD_NAMESPACE", "team-a")
		Expect(policyReadRemedy("team-a")).NotTo(ContainSubstring("setup-prereqs"))
	})
})
