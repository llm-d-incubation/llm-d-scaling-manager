package kueue

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
)

// The lender signal, read end to end: team-a borrows past its nominal 9,
// team-b lends and reports pending Workloads in its status.
var _ = Describe("Reader reads a lender as waiting from its pending Workloads", func() {
	const v = "v1beta2"
	workload := func(ns, name string, priority int64, requests map[string]interface{}, admittedTo string) *unstructured.Unstructured {
		w := obj(v, KindWorkload, ns, name, map[string]interface{}{})
		spec := podSetsRequesting(requests)
		spec["queueName"] = "lq"
		spec["priority"] = priority
		w.Object["spec"] = spec
		if admittedTo != "" {
			w.Object["status"] = map[string]interface{}{
				"admission":  map[string]interface{}{"clusterQueue": admittedTo},
				"conditions": []interface{}{map[string]interface{}{"type": "QuotaReserved", "status": "True"}},
			}
		}
		return w
	}
	// snap is the last read's snapshot.
	var snap config.ExternalQuotas
	// lenderPatch, when set, edits team-b's ClusterQueue before a read.
	var lenderPatch func(*unstructured.Unstructured)
	read := func(lenderPolicy string, extra ...client.Object) config.ExternalLender {
		objs := docsExample(v, "cohortName")
		a, b := objs[1].(*unstructured.Unstructured), objs[2].(*unstructured.Unstructured)
		// team-a holds 10 of nominal 9: it borrows 1.
		res := a.Object["status"].(map[string]interface{})["flavorsReservation"].([]interface{})[0].(map[string]interface{})["resources"].([]interface{})[0].(map[string]interface{})
		res["total"], res["borrowed"] = "10", "1"
		// team-b uses 8 of its 12: the cohort's free pool is 21 - 18 = 3, so a
		// pending Workload of 4, within team-b's unused 4, is not covered by
		// it and makes team-b wait (A8).
		resB := b.Object["status"].(map[string]interface{})["flavorsReservation"].([]interface{})[0].(map[string]interface{})["resources"].([]interface{})[0].(map[string]interface{})
		resB["total"] = "8"
		// team-b reports pending Workloads, as Kueue counts any.
		b.Object["status"].(map[string]interface{})["pendingWorkloads"] = int64(1)
		if lenderPolicy != "" {
			Expect(unstructured.SetNestedField(b.Object, lenderPolicy, "spec", "preemption", "reclaimWithinCohort")).To(Succeed())
		}
		if lenderPatch != nil {
			lenderPatch(b)
		}
		m := cohortMapper(v).(*meta.DefaultRESTMapper)
		m.Add(schema.GroupVersionKind{Group: Group, Version: v, Kind: KindWorkload}, meta.RESTScopeNamespace)
		c := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithRESTMapper(m).
			WithObjects(append(objs, extra...)...).Build()
		var err error
		snap, err = NewReader(c, Options{CountsBorrowing: true}).Quotas(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.SharedPools).To(HaveLen(1))
		for _, l := range snap.SharedPools[0].Lenders {
			if l.Queue == "team-b-cq" {
				return l
			}
		}
		Fail("no team-b lender")
		return config.ExternalLender{}
	}
	gpus := func(n string) map[string]interface{} { return map[string]interface{}{"nvidia.com/gpu": n} }
	cpu := map[string]interface{}{"cpu": "4"}

	It("does not wait for a pending CPU-only Workload, whatever Kueue's pending count says", func() {
		Expect(read("", workload("team-b", "batch", 0, cpu, "")).Waiting).To(BeFalse())
	})

	It("does not wait for a pending GPU Workload larger than its unused nominal", func() {
		Expect(read("", workload("team-b", "huge", 0, gpus("5"), "")).Waiting).To(BeFalse())
	})

	// A8: Kueue admits a pending Workload the cohort's free pool covers without
	// anything given back, so it closes nothing.
	It("does not wait for a pending GPU Workload the cohort's free pool covers", func() {
		Expect(read("", workload("team-b", "small", 0, gpus("3"), "")).Waiting).To(BeFalse())
	})

	It("waits for a pending GPU Workload that fits, queued through its LocalQueue", func() {
		Expect(read("", workload("team-b", "train", 0, gpus("4"), "")).Waiting).To(BeTrue())
		Expect(read("", workload("elsewhere", "train", 0, gpus("4"), "")).Waiting).To(BeFalse(),
			"a namespace without a LocalQueue on the lender queues nothing there")
	})

	It("does not count a Workload Kueue already reserved quota for", func() {
		Expect(read("", workload("team-b", "running", 0, gpus("4"), "team-b-cq")).Waiting).To(BeFalse())
	})

	finished := func(w *unstructured.Unstructured) *unstructured.Unstructured {
		st := w.Object["status"].(map[string]interface{})
		st["conditions"] = append(st["conditions"].([]interface{}),
			map[string]interface{}{"type": "Finished", "status": "True"})
		return w
	}
	classed := func(w *unstructured.Unstructured, group, name string) *unstructured.Unstructured {
		Expect(unstructured.SetNestedMap(w.Object, map[string]interface{}{"group": group, "name": name},
			"spec", "priorityClassRef")).To(Succeed())
		return w
	}

	// K1/G1: a LowerPriority lender is judged by the lowest priority it runs or
	// queues, not its highest; a Finished Workload keeps status.admission and
	// is neither.
	It("carries a LowerPriority lender's policy and the lowest priority of its unfinished GPU Workloads", func() {
		borrower := workload("team-a", "serve", 100, gpus("10"), "team-a-cq")
		l := read("LowerPriority",
			workload("team-b", "train", 1000, gpus("4"), ""),
			workload("team-b", "eval", 600, gpus("2"), "team-b-cq"),
			finished(workload("team-b", "done", 0, gpus("2"), "team-b-cq")),
			borrower)
		Expect(l.Waiting).To(BeTrue())
		Expect(l.Reclaim).To(Equal("LowerPriority"))
		Expect(l.Priority).To(HaveValue(Equal(int32(600))), "the finished Workload at 0 is not the lender's")
	})

	// PR-C review (KC5): what a release to the lender is sized by leaves out
	// a Workload in requeue backoff and one the lender admits by preempting
	// its own; the waiting signal that stops new borrowing is unchanged.
	It("sizes a release by the Workloads Kueue would admit next, read from the ClusterQueue and the Workloads", func() {
		l := read("", workload("team-b", "train", 0, gpus("4"), ""))
		Expect(l.Waiting).To(BeTrue())
		Expect(l.PendingGPUs).To(Equal(4))
		Expect(l.PendingPriority).To(HaveValue(Equal(int32(0))))
		created := workload("team-b", "train", 0, gpus("4"), "")
		created.SetCreationTimestamp(metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second)))
		Expect(read("", created).PendingSince).To(HaveValue(BeTemporally("==", created.GetCreationTimestamp().Time)))

		backoff := workload("team-b", "train", 0, gpus("4"), "")
		Expect(unstructured.SetNestedField(backoff.Object, time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"status", "requeueState", "requeueAt")).To(Succeed())
		l = read("", backoff)
		Expect(l.Waiting).To(BeTrue(), "the raw signal still stops new borrowing")
		Expect(l.PendingGPUs).To(BeZero(), "Kueue does not try it before requeueAt")
		Expect(l.PendingPriority).To(BeNil())

		own := func() *unstructured.Unstructured { return workload("team-b", "low", 1, gpus("2"), "team-b-cq") }
		l = read("", workload("team-b", "train", 100, gpus("4"), ""), own())
		Expect(l.PendingGPUs).To(Equal(4), "withinClusterQueue unset: Kueue does not preempt the lender's own")
		lenderPatch = func(b *unstructured.Unstructured) {
			Expect(unstructured.SetNestedField(b.Object, WithinLowerPriority, "spec", "preemption", "withinClusterQueue")).To(Succeed())
		}
		defer func() { lenderPatch = nil }()
		l = read("", workload("team-b", "train", 100, gpus("4"), ""), own())
		Expect(l.PendingGPUs).To(BeZero(), "Kueue admits it by preempting team-b's own priority-1 Workload")

		lenderPatch = func(b *unstructured.Unstructured) {
			Expect(unstructured.SetNestedField(b.Object, QueueingStrictFIFO, "spec", "queueingStrategy")).To(Succeed())
		}
		old := workload("team-b", "huge", 0, gpus("6"), "")
		old.SetCreationTimestamp(metav1.NewTime(time.Now().Add(-time.Hour)))
		younger := workload("team-b", "train", 0, gpus("4"), "")
		younger.SetCreationTimestamp(metav1.NewTime(time.Now()))
		l = read("", old, younger)
		Expect(l.Waiting).To(BeTrue())
		Expect(l.PendingGPUs).To(BeZero(), "StrictFIFO: the older 6-GPU head does not fit and blocks the 4")
	})

	// PR #203 re-check follow-up: a Workload Kueue preempted keeps its quota
	// reserved while its pods stop; the pool says how much.
	It("counts what preempted Workloads still hold reserved", func() {
		victim := workload("team-a", "victim", 10, gpus("2"), "team-a-cq")
		st := victim.Object["status"].(map[string]interface{})
		st["conditions"] = append(st["conditions"].([]interface{}),
			map[string]interface{}{"type": WorkloadEvicted, "status": "True", "reason": WorkloadEvictedByPreemption,
				"lastTransitionTime": time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339)})
		read("", victim, workload("team-a", "serve", 100, gpus("8"), "team-a-cq"))
		Expect(snap.SharedPools[0].EvictionsRead).To(BeTrue())
		Expect(snap.SharedPools[0].EvictingGPUs).To(Equal(2), "the victim's 2, not the running Workload's 8")
	})

	// PR #203 final check: a Workload re-admitted after its eviction is no
	// victim, whatever its Evicted condition still says.
	It("does not count a Workload re-admitted after its eviction", func() {
		evictedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
		readmittedAt := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
		w := workload("team-a", "back", 10, gpus("2"), "team-a-cq")
		w.Object["status"].(map[string]interface{})["conditions"] = []interface{}{
			map[string]interface{}{"type": "QuotaReserved", "status": "True", "lastTransitionTime": readmittedAt},
			map[string]interface{}{"type": WorkloadEvicted, "status": "True", "reason": WorkloadEvictedByPreemption,
				"lastTransitionTime": evictedAt},
		}
		read("", w)
		Expect(snap.SharedPools[0].EvictingGPUs).To(BeZero(), "re-admitted 5 minutes after its eviction")
	})

	It("reads each class's priority and each namespace's highest, from unfinished GPU Workloads", func() {
		read("",
			classed(workload("team-a", "serve", 1000, gpus("8"), "team-a-cq"), WorkloadPriorityClassGroup, "serving"),
			workload("team-a", "canary", 40, gpus("2"), "team-a-cq"),
			workload("team-a", "cpu-job", 5000, cpu, ""),
			finished(workload("team-a", "old", 9000, gpus("2"), "team-a-cq")),
			workload("team-b", "train", 500, gpus("4"), ""))
		Expect(snap.ClassPriority).To(Equal(map[string]int32{
			WorkloadPriorityClassGroup + "/serving": 1000, PriorityClassNone: 500}))
		Expect(snap.NamespaceHighestPriority).To(Equal(map[string]int32{"team-a": 1000, "team-b": 500}),
			"a CPU-only Workload is no GPU borrower, and a finished one no borrower at all")
	})
})
