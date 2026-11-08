package kueue

import (
	"context"
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// countingWorkloads is a WorkloadSource that counts its lists and finds none.
type countingWorkloads struct{ lists atomic.Int32 }

func (s *countingWorkloads) Workloads(context.Context, string, types.UID) ([]unstructured.Unstructured, error) {
	s.lists.Add(1)
	return nil, nil
}
func (s *countingWorkloads) Indexed() bool { return false }

// Review M-m2: the reader listed every Workload cluster-wide whenever a
// cohort existed, also for an entry that counts no borrowing -- on an install
// without the cluster-wide grant, a Forbidden list on every refresh for
// nothing. Only an entry that counts borrowing reads them now.
var _ = Describe("Reader reads the Workloads only for borrowing", func() {
	const v = "v1beta2"
	DescribeTable("Workload lists per read",
		func(counts bool, want int32) {
			src := &countingWorkloads{}
			r := NewReader(versionClient(nil, docsExample(v, "cohortName")...), Options{CountsBorrowing: counts, Workloads: src})
			_, err := r.Quotas(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(src.lists.Load()).To(Equal(want))
		},
		Entry("counts no borrowing: the Workloads are not listed", false, int32(0)),
		Entry("control: counts borrowing: they are listed", true, int32(1)),
	)
})
