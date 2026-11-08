package slo

import (
	"context"
	"fmt"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// The engine analyzes one model at a time today, but the analyzer says it is
// safe for concurrent use, and the per-cycle sweeps reach it from the engine
// while a cycle could be analyzing. Run with go test -race this is what holds
// the claim (dropping one lock made it report three races); make test runs
// this package under -race for that reason.
var _ = Describe("Analyzer under concurrent use", func() {
	It("analyzes models, sweeps and notes presence at once without a race", func() {
		rl := &domain.RouterLatency{WindowSeconds: 300, TTFTBuckets: buckets(1, 0.9, 3, 0.95, inf, 1.0),
			TPOTSumRate: 0.04, TPOTCountRate: 1}
		a := New(fakeRouter{latency: rl}, nil)
		var wg sync.WaitGroup
		for m := range 8 {
			wg.Add(1)
			go func() {
				defer GinkgoRecover()
				defer wg.Done()
				model := fmt.Sprintf("m%d", m)
				vs := domain.VariantReplicaState{VariantName: "agg", Role: domain.RoleBoth,
					AcceleratorName: "H100", GPUsPerReplica: 1, CurrentReplicas: 2}
				for i := range 20 {
					_, err := a.Analyze(context.Background(), domain.AnalyzerInput{
						ModelID: model, Namespace: "ns", Config: policy(44*time.Millisecond, 3*time.Second),
						ArrivalRate: 2, VariantStates: []domain.VariantReplicaState{vs},
						ReplicaMetrics: cycleRows("agg", i%5),
					})
					Expect(err).NotTo(HaveOccurred())
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer GinkgoRecover()
			defer wg.Done()
			for range 50 {
				a.EvictStale(time.Now())
				a.NoteVariantsPresent("ns", "m0", []string{"agg"})
			}
		}()
		wg.Wait()
		Expect(a.transients).To(HaveLen(8))
	})
})
