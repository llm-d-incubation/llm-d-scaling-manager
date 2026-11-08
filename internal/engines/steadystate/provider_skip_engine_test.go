package steadystate

import (
	"context"
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// neverReadStub is a Kueue-backed quota entry that has never read Kueue: it
// still answers, and its finite caps bound.
type neverReadStub struct{}

func (neverReadStub) Name() string { return "kq" }
func (neverReadStub) ComputeConstraints(context.Context, map[string]int, map[string]map[string]int) (*allocation.ResourceConstraints, error) {
	return &allocation.ResourceConstraints{ProviderName: "kq",
		Pools: map[string]allocation.ResourcePool{"H100": {Limit: 16}}, KueueNeverRead: true}, nil
}

// SR-M3: a never-read Kueue entry is in the decision, so it is not skipped.
// llmsm_kueue_never_read is its signal; reporting it as a skipped provider too
// paged twice for one cause, and said the entry bounds nothing, which its
// finite caps contradict.
func TestANeverReadKueueEntryIsNotASkippedProvider(t *testing.T) {
	resetUsageAndHeadroom(t)
	decision.PublishGPUUsage(map[string]int{"H100": 0}, nil) // observed: nothing else would skip it

	var lines []string
	logger := funcr.New(func(_, args string) { lines = append(lines, args) }, funcr.Options{})
	ctx := ctrl.LoggerInto(context.Background(), logger)

	e := &Engine{GPULimiter: neverReadStub{}, optimizer: allocation.NewGreedyByScoreOptimizer()}
	e.selectV2Optimizer(ctx, nil)
	for _, l := range lines {
		if strings.Contains(l, "GPU constraint provider skipped") {
			t.Fatalf("a never-read Kueue entry was reported as a skipped provider: %s", l)
		}
	}
	if len(e.providerSkips.last) != 0 {
		t.Fatalf("skipped providers %v, want none", e.providerSkips.last)
	}
}

// Ten cycles with the physical provider skipped log the skip once, not ten
// times: the skip is a metric now, and the log speaks when the set changes.
func TestSelectV2OptimizerLogsASkippedProviderOnce(t *testing.T) {
	resetUsageAndHeadroom(t) // no physical observation: the physical stub is skipped

	var lines []string
	logger := funcr.New(func(_, args string) { lines = append(lines, args) }, funcr.Options{})
	ctx := ctrl.LoggerInto(context.Background(), logger)

	e := &Engine{
		GPULimiter: allocation.NewCompositeLimiter("gpu-and-quota-limiter", []allocation.Limiter{
			physicalStub{pools: map[string]allocation.ResourcePool{"H100": {Limit: 16}}},
			quotaLimiterFor("tenant", "H100", 2),
		}),
		optimizer: allocation.NewGreedyByScoreOptimizer(),
	}
	for range 10 {
		e.selectV2Optimizer(ctx, nil)
	}
	n := 0
	for _, l := range lines {
		if strings.Contains(l, `"provider"="gpu-limiter"`) {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("ten cycles skipping the same provider logged it %d times, want 1:\n%s", n, strings.Join(lines, "\n"))
	}
}
