package scalefromzero

import (
	"context"
	"strings"

	"github.com/go-logr/logr/funcr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// A wake published with NO capacity check used to produce a log identical to one
// that passed a check — the skips were V(4) lines and the shipped verbosity keeps
// V(1), and one skip logged nothing at any level. The outcome is identical too: a
// variant comes up, which is what success looks like. That is how a kind e2e run
// published 44 activations for the variant its capacity suite requires to be
// REFUSED with no reason recorded anywhere.
var _ = Describe("Reporting a wake with no capacity check", func() {
	// Verbosity 0 keeps Info and discards V(1) and above — a stricter bar than
	// production, so a future demotion back to a V-level fails here rather than in
	// a cluster.
	captureAtInfo := func(fn func(ctx context.Context)) []string {
		var logged []string
		sink := funcr.New(func(prefix, args string) { logged = append(logged, args) },
			funcr.Options{Verbosity: 0})
		fn(log.IntoContext(context.Background(), sink))
		return logged
	}

	expectReported := func(logged []string, reason string) {
		GinkgoHelper()
		for _, line := range logged {
			if strings.Contains(line, "WITHOUT a GPU capacity check") && strings.Contains(line, reason) {
				return
			}
		}
		Fail("the wake was published with no capacity check and nothing said so at Info; logged=" +
			strings.Join(logged, " | "))
	}

	BeforeEach(func() { decision.DefaultGPUUsage.Reset() })
	AfterEach(func() { decision.DefaultGPUUsage.Reset() })

	It("says so when no constraint provider is configured", func() {
		e := &Engine{}
		logged := captureAtInfo(func(ctx context.Context) {
			Expect(e.gpuConstraints(ctx, "chat")).To(BeNil())
		})
		expectReported(logged, "no constraint provider")
	})

	It("says so when nothing has been observed yet", func() {
		e := &Engine{gpuLimiter: okProvider{name: "gpu-limiter"}}
		logged := captureAtInfo(func(ctx context.Context) {
			Expect(e.gpuConstraints(ctx, "chat")).To(BeNil())
		})
		expectReported(logged, "no cluster GPU-usage snapshot has been published")
	})

	It("says so when the physical view is there but a quota's managed view is not", func() {
		// The two measures are published by different producers, so one can be
		// present while the other is not. A quota provider handed the physical
		// figure would bind on GPUs it does not govern; handed nothing at all it
		// would report its whole allowance free. Neither is acceptable, so the wake
		// is reported as unchecked instead.
		decision.PublishGPUUsage(map[string]int{"H100": 1}, nil)
		e := &Engine{gpuLimiter: okProvider{name: "quota-limiter", basis: allocation.ManagedUsage}}
		logged := captureAtInfo(func(ctx context.Context) {
			Expect(e.gpuConstraints(ctx, "chat")).To(BeNil())
		})
		expectReported(logged, "no scaling-manager-managed GPU-usage snapshot")
	})

	It("says so when a provider could not compute constraints", func() {
		decision.PublishGPUUsage(map[string]int{"H100": 1}, nil)
		e := &Engine{gpuLimiter: failingProvider{name: "quota-limiter"}}
		logged := captureAtInfo(func(ctx context.Context) {
			Expect(e.gpuConstraints(ctx, "chat")).To(BeNil())
		})
		expectReported(logged, "could not compute constraints")
	})
})

// The change-throttle must not swallow the transition that matters. Checked
// becoming unchecked is the whole signal — throttling on "this namespace was
// already reported" rather than on the basis itself would report the first state
// and then go quiet across the change.
var _ = Describe("Throttling the placement-basis report", func() {
	It("reports every change and nothing in between", func() {
		e := &Engine{}
		Expect(e.placementBasisChanged("chat", "ok|budgets")).To(BeTrue(),
			"the first report for a namespace must be logged")
		Expect(e.placementBasisChanged("chat", "ok|budgets")).To(BeFalse(),
			"an unchanged basis must not re-log; this runs at 10Hz")
		Expect(e.placementBasisChanged("chat", "none|snapshot went away")).To(BeTrue(),
			"checked -> unchecked must be reported")
		Expect(e.placementBasisChanged("chat", "ok|budgets")).To(BeTrue(),
			"unchecked -> checked must be reported too")
	})
})
