package steadystate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/prometheus/client_golang/prometheus"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/passive"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// The limiter decision, through decideV2: under a limiter an active model is
// allocated exactly as if its passive neighbour were absent at its real usage
// (which the limiter's used figure already holds), and the passive model's
// would-be growth comes from what is left. One replica's GPUs are free, and the
// passive model has the higher priority: in one pass it would take them.
func TestPassiveGrowthTakesNothingFromAnActiveModel(t *testing.T) {
	t.Cleanup(decision.DefaultHeadroom.Reset)
	pool := func() []*allocation.ResourceConstraints {
		// Two models at one 8-GPU replica each: 16 used, 8 free.
		return []*allocation.ResourceConstraints{{ProviderName: "gpu-limiter",
			Pools: map[string]allocation.ResourcePool{"A100": {Limit: 24, Used: 16}}}}
	}
	act := growingA100("a", "act", 8)
	pas := growingA100("p", "pas", 8)
	pas.Priority = 5
	pas.Passive = true
	added := func(reqs ...allocation.ModelScalingRequest) map[string]int {
		e := &Engine{Config: config.NewTestConfig()}
		out := map[string]int{}
		for _, d := range e.decideV2(context.Background(), allocation.NewGreedyByScoreOptimizer(), reqs, pool(), nil) {
			out[d.ModelID] += (d.TargetReplicas - d.CurrentReplicas) * d.GPUsPerReplica
		}
		return out
	}
	alone, beside := added(act), added(act, pas)
	if alone["act"] != 8 || beside["act"] != alone["act"] {
		t.Fatalf("the active model grew %d GPUs beside a passive one and %d alone, want 8 both", beside["act"], alone["act"])
	}
	if beside["pas"] != 0 {
		t.Fatalf("the passive model's would-be growth is %d GPUs, want 0: nothing is left", beside["pas"])
	}
}

// recordBorrowDraws draws nothing for a passive model: its would-be growth is
// never ordered, and a draw would hold borrowed capacity from the models that
// can grow. The same model active is the control.
func TestPassiveDrawsNoBorrowedCapacity(t *testing.T) {
	for _, tc := range []struct {
		passive bool
		want    int
	}{{false, 1}, {true, 0}} {
		t.Cleanup(decision.DefaultHeadroom.Reset)
		e := &Engine{Config: config.NewTestConfig()}
		e.borrowing.inFlight = allocation.NewBorrowInFlight()
		now := time.Unix(30_000, 0)
		requests := []allocation.ModelScalingRequest{recordRequest(2, 2)}
		optimizer := allocation.NewGreedyByScoreOptimizer()
		c := e.prepareBorrowing(context.Background(), recordQuota(8, now), requests, nil, now)
		optimizer.BorrowBlocked = e.borrowBlocked()
		decisions := e.decideV2(context.Background(), optimizer, requests, c, nil)
		if tc.passive {
			e.cyclePassive = map[string]bool{"a/m": true}
		}
		if draws := e.recordBorrowDraws(optimizer, decisions, now); len(draws) != tc.want {
			t.Errorf("passive=%v: %d draws, want %d", tc.passive, len(draws), tc.want)
		}
		if tc.passive && len(e.borrowing.inFlight.Entries()) != 0 {
			t.Errorf("passive: the record holds %+v, want nothing", e.borrowing.inFlight.Entries())
		}
	}
}

// observePassive resolves each variant's mode from the registry the scaler
// fills -- our trigger beside a foreign one is observe -- and publishes
// llmsm_passive_mode; a variant that leaves loses its series.
func TestObservePassiveThroughTheRegistry(t *testing.T) {
	reg := prometheus.NewRegistry()
	if err := metrics.InitMetrics(reg); err != nil {
		t.Fatal(err)
	}
	cfg := config.NewTestConfig()
	yes := true
	cfg.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{
		"m": {ModelID: "acme/chat", Namespace: "ns", Passive: &yes}})
	vars := registry.New(0)
	vars.Observe("ns", "chat-so", map[string]string{registry.ModelIDKey: "acme/chat"})
	vars.SetTarget("ns", "chat-so", registry.Target{Name: "chat-deploy", ForeignTriggers: 1})
	e := &Engine{Config: cfg, Variants: vars, Passive: &passive.Resolver{Config: cfg, Registry: vars}}
	va := variant.Variant{
		ObjectMeta: metav1.ObjectMeta{Name: "chat-so", Namespace: "ns"},
		Spec: variant.Spec{ModelID: "acme/chat",
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "chat-deploy"}},
	}
	e.observePassive(logr.Discard(), []variant.Variant{va})
	if got := e.cyclePassiveVariants["ns/chat-deploy"]; got != passive.Observe {
		t.Fatalf("mode %q, want observe", got)
	}
	if !e.modelPassive("ns", "acme/chat") {
		t.Fatal("the model is not marked passive for the cycle")
	}
	modes := func() map[string]float64 {
		mfs, err := reg.Gather()
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]float64{}
		for _, mf := range mfs {
			if mf.GetName() != constants.LLMSMPassiveMode {
				continue
			}
			for _, m := range mf.GetMetric() {
				for _, l := range m.GetLabel() {
					if l.GetName() == constants.LabelMode {
						out[l.GetValue()] = m.GetGauge().GetValue()
					}
				}
			}
		}
		return out
	}
	if got := modes(); got[constants.PassiveModeObserve] != 1 || got[constants.PassiveModeActive] != 0 || len(got) != 4 {
		t.Fatalf("llmsm_passive_mode = %v, want observe=1 and the other three 0", got)
	}
	// At zero replicas the variant is in no active list, and it is still
	// observed: its mode stays published.
	e.observePassive(logr.Discard(), nil)
	if got := modes(); got[constants.PassiveModeObserve] != 1 || len(got) != 4 {
		t.Fatalf("llmsm_passive_mode at zero replicas = %v, want observe=1 and the other three 0", got)
	}
	vars.Forget("ns", "chat-so")
	e.observePassive(logr.Discard(), nil)
	if got := modes(); len(got) != 0 {
		t.Fatalf("llmsm_passive_mode after the variant left = %v, want no series", got)
	}
}

// passiveAtZero lists only passive variants outside the active list: an
// active model parked at zero (the control) and a warm pool's trigger, which
// names no model, are left out, and so is a variant already listed.
func TestPassiveAtZeroListsOnlyPassiveVariants(t *testing.T) {
	cfg := config.NewTestConfig()
	yes := true
	cfg.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{
		"m": {ModelID: "acme/held", Namespace: "ns", Passive: &yes}})
	vars := registry.New(0)
	vars.Observe("ns", "held-so", map[string]string{registry.ModelIDKey: "acme/held"})
	vars.Observe("ns", "parked-so", map[string]string{registry.ModelIDKey: "acme/parked"})
	vars.Observe("ns", "pool-so", map[string]string{registry.WarmPoolNameKey: "pool"})
	vars.Observe("ns", "running-so", map[string]string{registry.ModelIDKey: "acme/held"})
	e := &Engine{Config: cfg, Variants: vars, Passive: &passive.Resolver{Config: cfg, Registry: vars}}
	running := variant.Variant{ObjectMeta: metav1.ObjectMeta{Name: "running-so", Namespace: "ns"}}
	got := e.passiveAtZero([]variant.Variant{running})
	want := []metrics.PassiveVariant{{Namespace: "ns", ModelName: "acme/held", Variant: "held-so", Mode: constants.PassiveModeHold}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("passiveAtZero = %+v, want %+v", got, want)
	}
}

// For a passive variant the decision line says it was published, not
// applied; an active variant keeps the applied line (the control).
func TestLogDecisionNeverSaysAppliedForAPassiveVariant(t *testing.T) {
	for _, tc := range []struct {
		passive bool
		want    string
	}{
		{false, "Applied saturation decision via shared cache"},
		{true, "Passive: decision published, not applied"},
	} {
		var lines []string
		logger := funcr.New(func(_, args string) { lines = append(lines, args) }, funcr.Options{})
		logDecision(logger, tc.passive, "variant", "chat-so", "action", "scale-down", "target", 2)
		if len(lines) != 1 || !strings.Contains(lines[0], tc.want) {
			t.Fatalf("passive=%v: logged %q, want one line holding %q", tc.passive, lines, tc.want)
		}
		if tc.passive && strings.Contains(lines[0], "Applied") {
			t.Fatalf("passive: the line reads as applied: %q", lines[0])
		}
	}
}

// A model that goes passive mid-transfer puts its share group in shadow. For
// the absence grace the transfer in flight is held -- the donor stays at its
// lowered target, the passive model at its own count -- and the marks stay (a
// failed read must not drop them); then the ledger goes, no target is left,
// and the marks are removed. Removing them restores the pods' own deletion
// cost: the one write the share makes after a model went passive.
func TestPassiveMidTransferUnmarksAfterTheGrace(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	if started := se.untilStarted(); started < 1 {
		t.Fatal("setup: no transfer started")
	}
	if len(markedPods(t, c)) == 0 {
		t.Fatal("setup: no donor pod marked")
	}
	se.e.cyclePassive = map[string]bool{"ns/B": true}
	o := se.cycle()
	if b, ok := o["ns/B-v"]; ok && b.Target != f.current["B"] {
		t.Fatalf("the passive model's target moved to %d, want its count %d", b.Target, f.current["B"])
	}
	if len(markedPods(t, c)) == 0 {
		t.Fatal("the marks were dropped at once; they are kept through the absence grace")
	}
	for range 5 { // 30 s cycles past the 2 min grace
		o = se.cycle()
	}
	if len(o) != 0 {
		t.Fatalf("targets after the grace: %+v, want none", o)
	}
	if n := len(markedPods(t, c)); n != 0 {
		t.Fatalf("%d donor pods still marked after the grace", n)
	}
}

// A held variant whose ScaledObject sets spec.fallback is warned about once:
// an unreadable count makes the scaler answer an error, and past
// failureThreshold KEDA sets fallback.replicas.
func TestHeldVariantWithFallbackIsWarnedOnce(t *testing.T) {
	warned := 0
	logger := funcr.New(func(_, args string) {
		if strings.Contains(args, "spec.fallback") {
			warned++
		}
	}, funcr.Options{})
	e := &Engine{}
	va := &variant.Variant{ObjectMeta: metav1.ObjectMeta{Name: "so", Namespace: "ns"},
		Spec: variant.Spec{ModelID: "m"}}
	held := passiveVariantState{mode: passive.Hold, fallback: true}
	e.reportPassiveChange(logger, va, "d", passiveVariantState{}, held, registry.Target{HasFallback: true})
	e.reportPassiveChange(logger, va, "d", held, held, registry.Target{HasFallback: true})
	if warned != 1 {
		t.Fatalf("warned %d times over two cycles, want 1", warned)
	}
}

// A share group that goes to shadow because it holds a passive model says so
// on llmsm_utilization_share_passive_shadow.
func TestPassiveShadowGroupIsNamed(t *testing.T) {
	reg := prometheus.NewRegistry()
	if err := metrics.InitMetrics(reg); err != nil {
		t.Fatal(err)
	}
	f := newShareFleet()
	clock := time.Unix(0, 0)
	e := &Engine{Config: shadowConfig(t, activeShare), client: sharePods(t, f)}
	e.utilizationShare.now = func() time.Time { return clock }
	e.cyclePassive = map[string]bool{"ns/A": true}
	ctx, _ := observe(t)
	clock = clock.Add(30 * time.Second)
	e.evaluateUtilizationShare(ctx, f.requests(), fullQuota(), f.scaleTargets())
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == constants.LLMSMUtilizationSharePassiveShadow {
			for _, m := range mf.GetMetric() {
				if m.GetGauge().GetValue() == 1 {
					return
				}
			}
		}
	}
	t.Fatalf("no %s series at 1", constants.LLMSMUtilizationSharePassiveShadow)
}

// recordingOptimizer records the models each Optimize call was given.
type recordingOptimizer struct{ calls [][]string }

func (r *recordingOptimizer) Name() string { return "recording" }
func (r *recordingOptimizer) Optimize(_ context.Context, reqs []allocation.ModelScalingRequest,
	_ []*allocation.ResourceConstraints) []domain.VariantDecision {
	models := make([]string, 0, len(reqs))
	out := make([]domain.VariantDecision, 0, len(reqs))
	for _, req := range reqs {
		models = append(models, req.ModelID)
		out = append(out, domain.VariantDecision{Namespace: req.Namespace, ModelID: req.ModelID, VariantName: req.ModelID + "-v"})
	}
	r.calls = append(r.calls, models)
	return out
}

// The passive exclusion holds under any optimizer, not only the greedy one:
// the models that scale are decided without the passive ones, and the
// passive would-be figures come from a pass of their own.
func TestPassiveExclusionUnderAnyOptimizer(t *testing.T) {
	act := growingA100("a", "act", 8)
	pas := growingA100("p", "pas", 8)
	pas.Passive = true
	o := &recordingOptimizer{}
	e := &Engine{Config: config.NewTestConfig()}
	ds := e.decideV2(context.Background(), o, []allocation.ModelScalingRequest{act, pas}, nil, nil)
	if len(o.calls) != 2 || len(o.calls[0]) != 1 || o.calls[0][0] != "act" || len(o.calls[1]) != 1 || o.calls[1][0] != "pas" {
		t.Fatalf("Optimize calls %v, want [[act] [pas]]", o.calls)
	}
	if len(ds) != 2 {
		t.Fatalf("%d decisions, want both models'", len(ds))
	}
}
