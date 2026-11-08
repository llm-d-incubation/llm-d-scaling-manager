package steadystate

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
)

// recordQuota is namespace "a" at its guaranteed 16 GPUs (two 8-GPU
// replicas), in cohort "c" whose pool Kueue reads with free GPUs, at readAt.
func recordQuota(free int, readAt time.Time) []*allocation.ResourceConstraints {
	return []*allocation.ResourceConstraints{{
		ProviderName:     "namespace-quota",
		NamespaceScoped:  true,
		Pools:            map[string]allocation.ResourcePool{"H100": {Limit: config.QuotaUnlimited}},
		NamespacePools:   map[string]map[string]allocation.ResourcePool{"a": {"H100": {Limit: 16, Used: 16}}},
		NamespaceBorrow:  map[string]map[string]allocation.NamespaceBorrow{"a": {"H100": {Cap: config.QuotaUnlimited, WithinNominal: 16}}},
		NamespaceCohorts: map[string][]string{"a": {"c"}},
		SharedPools: []allocation.SharedPoolChunk{{
			Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu", Free: free, ReadAt: readAt,
			FlavorPlacement: kueue.FlavorPlacement{AcceleratorType: "H100"},
			Lenders:         []kueue.LenderState{{Queue: "lender", Reclaim: kueue.ReclaimAny, Unused: free}},
		}},
	}}
}

// recordRequest is model "m" of namespace "a": one variant of 8-GPU replicas
// at current, held at desired, whose demand asks for one replica more.
func recordRequest(current, desired int) allocation.ModelScalingRequest {
	return allocation.ModelScalingRequest{
		ModelID: "m", Namespace: "a", Priority: 1,
		CompositeSignal: allocation.NamedAnalyzerResult{
			Name: domain.SaturationAnalyzerName,
			Result: &domain.AnalyzerResult{ModelID: "m", Namespace: "a",
				VariantCapacities: []domain.VariantCapacity{{VariantName: "v", ReplicaCount: current, PerReplicaCapacity: 10000}},
			},
			RequiredCapacity: 10000, Remaining: 10000, Live: true,
		},
		Variants:      []domain.VariantMetadata{{VariantName: "v", AcceleratorName: "H100", Cost: 5}},
		VariantStates: []domain.VariantReplicaState{{VariantName: "v", CurrentReplicas: current, DesiredReplicas: desired, GPUsPerReplica: 8}},
	}
}

// recordCycle runs stage 2 of one cycle as optimizeV2 does, the cohort's pool
// read at now with free GPUs. planned, when set, is the models the
// utilization share overrides this cycle. It returns m's target and the
// borrowed figure the scaling-decision line carries for it.
func recordCycle(e *Engine, free int, current, desired int, now time.Time, planned map[string]bool) (int, int) {
	ctx := context.Background()
	requests := []allocation.ModelScalingRequest{recordRequest(current, desired)}
	optimizer := allocation.NewGreedyByScoreOptimizer()
	c := e.prepareBorrowing(ctx, recordQuota(free, now), requests, nil, now)
	optimizer.BorrowBlocked = e.borrowBlocked()
	optimizer.BorrowCredit = e.borrowing.credit
	decisions := e.decideV2(ctx, optimizer, requests, c, nil)
	if planned != nil {
		e.cyclePlanned = planned
	}
	draws := e.recordBorrowDraws(optimizer, decisions, now)
	borrowed := borrowedByModel(draws, c)["a/m"]
	for _, d := range decisions {
		if d.VariantName == "v" {
			return d.TargetReplicas, borrowed
		}
	}
	return -1, borrowed
}

// Review A4: across cycles, a variant whose borrowed replica's pods are not
// created yet (current 2, desired 3) keeps its target. Its own draw in flight
// is deducted from a pool Kueue still reads as 8 free; without the credit the
// next pass refused the replica it ordered, the target fell to 2, the draw
// was dropped, and the one after ordered it again: 3/2/3.
func TestABorrowedTargetDoesNotAlternate(t *testing.T) {
	t.Cleanup(decision.DefaultHeadroom.Reset)
	e := &Engine{Config: config.NewTestConfig()}
	e.borrowing.inFlight = allocation.NewBorrowInFlight()
	t0 := time.Unix(30_000, 0)
	got := make([]int, 0, 3)
	desired := 2
	for i := range 3 {
		target, _ := recordCycle(e, 8, 2, desired, t0.Add(time.Duration(i)*30*time.Second), nil)
		got = append(got, target)
		desired = target
	}
	for i, n := range got {
		if n != 3 {
			t.Fatalf("targets over three cycles %v, want 3 each: cycle %d refused its own draw", got, i+1)
		}
	}
	if e := e.borrowing.inFlight.Entries(); len(e) != 1 || e[0].GPUs != 8 {
		t.Fatalf("the record holds %+v, want the one 8-GPU draw", e)
	}
}

// Review NEW-1, through the engine: namespace a (a reclaiming queue, nominal
// 24, 16 used) lent 8 that a peer borrows, and the one node is full. Model m
// wants a third replica: Kueue preempts the peer for it, so it places without
// a hole. Across three cycles before its pods exist it keeps target 3, and the
// record holds the one 8-GPU reclaim: prepareBorrowing hands the variant its
// own entry back (reclaimCredit) after DeductReclaimed took it off LentOut.
func TestAReclaimedTargetDoesNotAlternate(t *testing.T) {
	t.Cleanup(decision.DefaultHeadroom.Reset)
	e := &Engine{Config: config.NewTestConfig()}
	e.borrowing.inFlight = allocation.NewBorrowInFlight()
	t0 := time.Unix(35_000, 0)
	quota := func(now time.Time) []*allocation.ResourceConstraints {
		c := recordQuota(0, now)
		c[0].NamespacePools["a"]["H100"] = allocation.ResourcePool{Limit: 24, Used: 16}
		c[0].NamespaceBorrow["a"]["H100"] = allocation.NamespaceBorrow{Cap: 0, WithinNominal: 24, LentOut: 8}
		// The physical limiter: every GPU of the three nodes in use.
		return append(c, &allocation.ResourceConstraints{ProviderName: allocation.PhysicalLimiterName,
			Pools: map[string]allocation.ResourcePool{"H100": {Limit: 24, Used: 24}}})
	}
	got := make([]int, 0, 3)
	desired := 2
	for i := range 3 {
		ctx, now := context.Background(), t0.Add(time.Duration(i)*30*time.Second)
		requests := []allocation.ModelScalingRequest{recordRequest(2, desired)}
		optimizer := allocation.NewGreedyByScoreOptimizer()
		c := e.prepareBorrowing(ctx, quota(now), requests, nil, now)
		optimizer.Nodes = allocation.NewNodeView(map[string]decision.NodeGPU{
			"n1": {Accelerator: "H100", Capacity: 8, Used: 8}, "n2": {Accelerator: "H100", Capacity: 8, Used: 8},
			"n3": {Accelerator: "H100", Capacity: 8, Used: 8}})
		optimizer.BorrowCredit = e.borrowing.credit
		optimizer.ReclaimCredit = e.borrowing.reclaimCredit
		decisions := e.decideV2(ctx, optimizer, requests, c, nil)
		e.recordBorrowDraws(optimizer, decisions, now)
		target := -1
		for _, d := range decisions {
			if d.VariantName == "v" {
				target = d.TargetReplicas
			}
		}
		got = append(got, target)
		desired = target
	}
	for i, n := range got {
		if n != 3 {
			t.Fatalf("targets over three cycles %v, want 3 each: cycle %d refused its own reclaimed replica", got, i+1)
		}
	}
	if rs := e.borrowing.inFlight.Reclaims(); len(rs) != 1 || rs[0].GPUs != 8 {
		t.Fatalf("the record holds %+v, want the one 8-GPU reclaim", rs)
	}
}

// Review W9: a model the utilization share overrides this cycle keeps no draw
// of the pass's in flight, and its scaling-decision line borrows nothing.
func TestAShareOverrideClearsThePassDraw(t *testing.T) {
	t.Cleanup(decision.DefaultHeadroom.Reset)
	t0 := time.Unix(40_000, 0)

	e := &Engine{Config: config.NewTestConfig()}
	e.borrowing.inFlight = allocation.NewBorrowInFlight()
	if _, borrowed := recordCycle(e, 8, 2, 2, t0, nil); borrowed != 8 {
		t.Fatalf("control: the pass borrows %d, want 8", borrowed)
	}
	if len(e.borrowing.inFlight.Entries()) != 1 {
		t.Fatal("control: the pass's draw is in flight")
	}

	e = &Engine{Config: config.NewTestConfig()}
	e.borrowing.inFlight = allocation.NewBorrowInFlight()
	_, borrowed := recordCycle(e, 8, 2, 2, t0, map[string]bool{"a/m": true})
	if borrowed != 0 {
		t.Errorf("the scaling-decision line of an overridden model borrows %d, want 0", borrowed)
	}
	if got := e.borrowing.inFlight.Entries(); len(got) != 0 {
		t.Errorf("an overridden model's pass draw is in flight: %+v", got)
	}
}

// gpuPod is an 8-GPU pod of namespace "a", at Kueue's admission gate when
// gated.
func gpuPod(name string, gated bool) *corev1.Pod {
	gpus := corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")}
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "a", UID: types.UID("uid-" + name)},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{Requests: gpus, Limits: gpus}}},
		},
	}
	if gated {
		p.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: kueue.AdmissionGate}}
	}
	return p
}

// poolFree is the free GPUs of every chunk of cs.
func poolFree(cs []*allocation.ResourceConstraints) int {
	n := 0
	for _, c := range cs {
		for _, p := range c.SharedPools {
			n += p.Free
		}
	}
	return n
}

// Review W3: a pod seen at Kueue's gate and admitted since is reserved, but a
// read taken before the admission was seen shows its 8 GPUs free. It stays
// deducted from such a read, and not from a later one.
func TestAnAdmittedPodStaysDeductedUntilARead(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gpuPod("p", true)).Build()
	e := &Engine{Config: config.NewTestConfig(), client: cl}
	e.borrowing.inFlight = allocation.NewBorrowInFlight()
	ctx := context.Background()
	t0 := time.Unix(45_000, 0)
	reqs := []allocation.ModelScalingRequest{recordRequest(2, 2)}

	if got := poolFree(e.prepareBorrowing(ctx, recordQuota(16, t0), reqs, nil, t0)); got != 8 {
		t.Fatalf("premise: the gated pod's 8 come off the pool, free %d, want 8", got)
	}
	if err := cl.Update(ctx, gpuPod("p", false)); err != nil {
		t.Fatal(err)
	}
	t1 := t0.Add(30 * time.Second)
	if got := poolFree(e.prepareBorrowing(ctx, recordQuota(16, t0), reqs, nil, t1)); got != 8 {
		t.Fatalf("the admitted pod reads free in a read from before its admission: free %d, want 8", got)
	}
	if got := poolFree(e.prepareBorrowing(ctx, recordQuota(8, t1.Add(time.Second)), reqs, nil, t1.Add(time.Second))); got != 8 {
		t.Fatalf("a read after the admission counts it already, free %d, want Kueue's 8", got)
	}
}

// Review G3: a namespace whose pods could not be listed may hide any number
// of gated pods. Its cohorts borrow nothing new this cycle, for the wake too.
func TestAFailedGatedListClosesTheCohorts(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	failing := true
	cl := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.PodList); ok && failing {
				return errors.New("pods are unreadable")
			}
			return c.List(ctx, list, opts...)
		},
	}).Build()
	record := allocation.NewBorrowInFlight()
	e := &Engine{Config: config.NewTestConfig(), client: cl}
	e.borrowing.inFlight = record
	t0 := time.Unix(47_000, 0)
	reqs := []allocation.ModelScalingRequest{recordRequest(2, 2)}

	if got := poolFree(e.prepareBorrowing(context.Background(), recordQuota(16, t0), reqs, nil, t0)); got != 0 {
		t.Fatalf("the cohort of a namespace whose gated pods are unknown reads %d free, want 0", got)
	}
	if !record.Blocked(t0)["c"] {
		t.Fatal("the wake does not see the cohort closed")
	}
	failing = false
	if got := poolFree(e.prepareBorrowing(context.Background(), recordQuota(16, t0), reqs, nil, t0.Add(time.Second))); got != 16 {
		t.Fatalf("control: a list that succeeds leaves %d free, want 16", got)
	}
}
