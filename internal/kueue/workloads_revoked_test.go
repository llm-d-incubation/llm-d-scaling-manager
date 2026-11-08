package kueue

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// Review K2-4: once the informer has synced, a revoked cluster-wide grant
// left its reflector failing in the background and its store as it was: the
// lender signal kept reading a frozen cache as fresh. The started informer's
// list is asked again every reprobeEvery; Forbidden sends the reads back to
// the API server, whose Forbidden withholds borrowing as lenders-unreadable,
// until a probe passes again.
var _ = Describe("InformerWorkloads after the cluster-wide grant is revoked", func() {
	It("stops reading the synced cache within reprobeEvery, and reads it again once granted", func() {
		p := gpuPod("p", 8, nil, labelled)
		w := workloadFor(p)
		forbid := false
		api := eligibilityClient(&interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if forbid && (&client.ListOptions{}).ApplyOptions(opts).Namespace == "" {
				return apierrors.NewForbidden(schema.GroupResource{Group: Group, Resource: "workloads"}, "", errors.New("revoked"))
			}
			return cl.List(ctx, list, opts...)
		}}, w)
		idxObj := &unstructured.Unstructured{}
		idxObj.SetGroupVersionKind(w.GroupVersionKind())
		reader := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithRESTMapper(eligibilityMapper()).
			WithObjects(w.DeepCopy()).WithIndex(idxObj, WorkloadOwnerUIDField, workloadOwnerUIDs).Build()
		fc := &fakeWorkloadCache{FakeInformers: unsyncedInformers(idxObj.GroupVersionKind()), reader: reader}
		src := NewInformerWorkloads(fc, api)
		clock := time.Unix(50000, 0)
		src.now = func() time.Time { return clock }
		ctx := context.Background()

		_, err := src.Workloads(ctx, "", "")
		Expect(err).NotTo(HaveOccurred())
		inf, err := fc.FakeInformerFor(ctx, idxObj)
		Expect(err).NotTo(HaveOccurred())
		inf.Synced()
		got, err := src.Workloads(ctx, "", "")
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(1))
		Expect(fc.lists).To(Equal(1), "synced: read from the cache")

		forbid = true
		clock = clock.Add(reprobeEvery + time.Second)
		_, err = src.Workloads(ctx, "", "")
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "a revoked grant is Forbidden, not a cache that stopped changing")
		Expect(fc.lists).To(Equal(1), "the frozen cache is not read")
		Expect(src.Indexed()).To(BeFalse())

		forbid = false
		clock = clock.Add(deniedRetryAfter + time.Second)
		got, err = src.Workloads(ctx, "", "")
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(1))
		Expect(fc.lists).To(Equal(2), "granted again: the cache is read again")
	})
})
