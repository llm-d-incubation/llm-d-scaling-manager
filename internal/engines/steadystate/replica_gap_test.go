package steadystate

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// Issue #439: per variant, the replicas the analyzers suggest (X) against those
// the controller provides (Y), and why they differ. The engine's stores are
// process-wide: no t.Parallel.

// gapMode is how a cycle runs: active, under the utilization share in shadow
// mode, or with the model passive.
type gapMode struct {
	name    string
	config  func(t *testing.T) *config.Config
	passive bool
}

var gapModes = []gapMode{
	{"active", func(*testing.T) *config.Config { return config.NewTestConfig() }, false},
	{"shadow", func(t *testing.T) *config.Config { return shadowConfig(t, selectedShadow) }, false},
	{"passive", func(*testing.T) *config.Config { return config.NewTestConfig() }, true},
}

// applyGaps applies decisions at their targets the way applySaturationDecisions
// does, publishes, and returns each variant's gap. It also checks the identity
// every gap must satisfy: X - Y = sum(short) - borrowed.
func applyGaps(t *testing.T, e *Engine, ds []domain.VariantDecision) map[string]metrics.ReplicaGap {
	t.Helper()
	rec := &replicaGapRecorder{plan: e.cycleGaps}
	out := map[string]metrics.ReplicaGap{}
	for i := range ds {
		g := rec.record(&ds[i], ds[i].TargetReplicas)
		short := 0
		for _, s := range g.Short {
			short += s.Replicas
		}
		if g.Suggested-g.Provided != short-g.Borrowed {
			t.Fatalf("%s: suggested %d - provided %d != short %d - borrowed %d", g.VariantName,
				g.Suggested, g.Provided, short, g.Borrowed)
		}
		out[ds[i].VariantName] = g
	}
	rec.publish()
	return out
}

// gapSeries reads a replica-gap family keyed variant, or variant/reason/limiter
// for llmsm_replicas_short, or limiter/scope/accelerator for the quota gauge.
func gapSeries(t *testing.T, r *prometheus.Registry, name string) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for _, m := range family(t, r, name) {
		var k string
		switch name {
		case constants.LLMSMReplicasShort:
			k = label(m, constants.LabelVariantName) + "/" + label(m, constants.LabelReason) + "/" +
				label(m, constants.LabelLimiterName)
		case constants.LLMSMQuotaRemainingAfterSuggestedGPUs:
			k = label(m, constants.LabelLimiterName) + "/" + label(m, constants.LabelScope) + "/" +
				label(m, constants.LabelAccelerator)
		default:
			k = label(m, constants.LabelVariantName)
		}
		out[k] = m.GetGauge().GetValue()
	}
	return out
}

// gapModesOf is the mode label of each variant's llmsm_replicas_provided.
func gapModesOf(t *testing.T, r *prometheus.Registry) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, m := range family(t, r, constants.LLMSMReplicasProvided) {
		out[label(m, constants.LabelVariantName)] = label(m, constants.LabelMode)
	}
	return out
}

// limiterCase is one limiter reason: a model asking for one more 8-GPU
// replica, the constraints that refuse it, and what the short series say.
type limiterCase struct {
	reason string
	cons   func() []*allocation.ResourceConstraints
	nodes  map[string]decision.NodeGPU
	// short is the llmsm_replicas_short series in an active cycle; quota the
	// quota gauge.
	short map[string]float64
	quota map[string]float64
}

var limiterCases = []limiterCase{
	{
		reason: constants.ReplicasShortReasonQuota,
		cons: func() []*allocation.ResourceConstraints {
			return []*allocation.ResourceConstraints{{ProviderName: "team-quota",
				Pools: map[string]allocation.ResourcePool{"A100": {Limit: 8, Used: 8}}}}
		},
		short: map[string]float64{"m-v/quota/team-quota": 1},
		// 8 held of 8, and X adds one more 8-GPU replica.
		quota: map[string]float64{"team-quota/cluster/A100": -8},
	},
	{
		reason: constants.ReplicasShortReasonGPU,
		cons: func() []*allocation.ResourceConstraints {
			return []*allocation.ResourceConstraints{{ProviderName: allocation.PhysicalLimiterName,
				Pools: map[string]allocation.ResourcePool{"A100": {Limit: 8, Used: 8}}}}
		},
		short: map[string]float64{"m-v/gpu/": 1},
		quota: map[string]float64{},
	},
	{
		// 16 free GPUs, but 4 on each node: no node holds an 8-GPU replica.
		reason: constants.ReplicasShortReasonNodeFit,
		cons: func() []*allocation.ResourceConstraints {
			return []*allocation.ResourceConstraints{{ProviderName: allocation.PhysicalLimiterName,
				Pools: map[string]allocation.ResourcePool{"A100": {Limit: 24, Used: 8}}}}
		},
		nodes: a100Nodes(4, 4, 4, 4),
		short: map[string]float64{"m-v/node-fit/": 1},
		quota: map[string]float64{},
	},
}

// Each limiter reason, in each mode: the limiter holds the model at 1 of the 2
// it asks for, X is 2, Y the target the cycle sets, and the short series names
// the limiter. A passive model's would-be pass is not fitted to nodes
// (ShadowOptimize), so node fit does not hold it: its would-be target is the
// 2 it asks for, and nothing is short.
func TestReplicaGap_EachLimiterReasonInEachMode(t *testing.T) {
	for _, lc := range limiterCases {
		for _, mode := range gapModes {
			t.Run(lc.reason+"/"+mode.name, func(t *testing.T) {
				decision.DefaultHeadroom.Reset()
				t.Cleanup(decision.DefaultHeadroom.Reset)
				reg := freshMetrics(t)
				e := &Engine{Config: mode.config(t)}
				req := growingA100("ns", "m", 8)
				req.Passive = mode.passive
				o := allocation.NewGreedyByScoreOptimizer()
				if lc.nodes != nil {
					o.Nodes = allocation.NewNodeView(lc.nodes)
				}
				ds := e.decideV2(t.Context(), o, []allocation.ModelScalingRequest{req}, lc.cons(), nil)
				require.Len(t, ds, 1)
				gaps := applyGaps(t, e, ds)
				// No decision changed: a passive model's would-be target is
				// not fitted to nodes, so node fit leaves it at the 2 it asks
				// for; only its series are fitted.
				wantTarget := 1
				if mode.passive && lc.reason == constants.ReplicasShortReasonNodeFit {
					wantTarget = 2
				}
				require.Equal(t, wantTarget, ds[0].TargetReplicas, "the cycle's target")
				require.Equal(t, map[string]float64{"m-v": 2}, gapSeries(t, reg, constants.LLMSMReplicasSuggested))
				require.Equal(t, map[string]float64{"m-v": 1}, gapSeries(t, reg, constants.LLMSMReplicasProvided))
				require.Equal(t, lc.short, gapSeries(t, reg, constants.LLMSMReplicasShort))
				require.Equal(t, map[string]float64{"m-v": 0}, gapSeries(t, reg, constants.LLMSMReplicasBorrowed))
				require.Equal(t, lc.quota, gapSeries(t, reg, constants.LLMSMQuotaRemainingAfterSuggestedGPUs))
				require.Equal(t, "both", gaps["m-v"].Role)
				wantMode := constants.ReplicasModeActive
				if mode.passive {
					wantMode = constants.ReplicasModePassive
				}
				require.Equal(t, map[string]string{"m-v": wantMode}, gapModesOf(t, reg))
				// WasLimited stays off a passive model's would-be decision
				// (the sticky hold reads it).
				if mode.passive {
					require.False(t, ds[0].WasLimited, "a passive would-be decision must not be marked limited")
				}
			})
		}
	}
}

// maxCapped is a model at one replica asking for one more, on a variant whose
// maxReplicas is 1, optionally beside an uncapped sibling at zero.
func maxCapped(sibling bool) allocation.ModelScalingRequest {
	req := growingA100("ns", "m", 1)
	req.VariantStates[0].MaxReplicas = ptr.To(1)
	if sibling {
		req.CompositeSignal.Result.VariantCapacities = append(req.CompositeSignal.Result.VariantCapacities,
			domain.VariantCapacity{VariantName: "m-w", PerReplicaCapacity: 1000})
		req.Variants = append(req.Variants, domain.VariantMetadata{VariantName: "m-w", AcceleratorName: "A100", Cost: 10})
		req.VariantStates = append(req.VariantStates, domain.VariantReplicaState{VariantName: "m-w", GPUsPerReplica: 1})
	}
	return req
}

// The maxReplicas ceiling, in each mode, with no limiter declared: X is the 2
// the model asks for, Y the 1 the ceiling allows. Negative control: an
// uncapped sibling takes the replica the ceiling refused, so the role is not
// short and nothing is reported.
func TestReplicaGap_MaxReplicasInEachMode(t *testing.T) {
	for _, mode := range gapModes {
		for _, sibling := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/sibling=%t", mode.name, sibling), func(t *testing.T) {
				reg := freshMetrics(t)
				e := &Engine{Config: mode.config(t)}
				req := maxCapped(sibling)
				req.Passive = mode.passive
				ds := e.decideV2(t.Context(), allocation.NewCostAwareOptimizer(), []allocation.ModelScalingRequest{req}, nil, nil)
				applyGaps(t, e, ds)
				if !sibling {
					require.Equal(t, map[string]float64{"m-v": 2}, gapSeries(t, reg, constants.LLMSMReplicasSuggested))
					require.Equal(t, map[string]float64{"m-v": 1}, gapSeries(t, reg, constants.LLMSMReplicasProvided))
					require.Equal(t, map[string]float64{"m-v/max-replicas/": 1}, gapSeries(t, reg, constants.LLMSMReplicasShort))
					return
				}
				require.Equal(t, map[string]float64{"m-v": 1, "m-w": 1}, gapSeries(t, reg, constants.LLMSMReplicasProvided),
					"setup: the sibling takes the replica")
				require.Equal(t, map[string]float64{"m-v": 1, "m-w": 1}, gapSeries(t, reg, constants.LLMSMReplicasSuggested))
				require.Empty(t, gapSeries(t, reg, constants.LLMSMReplicasShort))
			})
		}
	}
}

// modelStub returns fixed decisions, for the models it is asked about only:
// a passive model's would-be pass asks about it alone.
type modelStub struct{ decisions []domain.VariantDecision }

func (s modelStub) Name() string { return "stub" }
func (s modelStub) Optimize(_ context.Context, reqs []allocation.ModelScalingRequest,
	_ []*allocation.ResourceConstraints) []domain.VariantDecision {
	asked := map[string]bool{}
	for _, r := range reqs {
		asked[r.ModelID] = true
	}
	var out []domain.VariantDecision
	for _, d := range s.decisions {
		if asked[d.ModelID] {
			out = append(out, d)
		}
	}
	return out
}

// shareStub is today's optimizer holding every model of the §5.7 fleet where
// it runs.
func shareStub(f *shareFleet) modelStub {
	var s modelStub
	for _, id := range []string{"A", "B", "C"} {
		s.decisions = append(s.decisions, domain.VariantDecision{Namespace: "ns", ModelID: id, VariantName: id + "-v",
			CurrentReplicas: f.current[id], TargetReplicas: f.current[id], GPUsPerReplica: 1})
	}
	return s
}

// The utilization share's two reasons, in each mode, on the §5.7 fleet. Active,
// it lowers A to give B its GPUs: A is short by share-lent; once A has
// released, B is raised past what today's optimizer gives it: borrowed. In
// shadow mode, or with a model passive (its group plans in shadow), the share
// sets nothing, and its series say what it would set, under mode shadow (and
// passive for the passive model).
func TestReplicaGap_ShareLentAndBorrowedInEachMode(t *testing.T) {
	for _, mode := range []struct {
		name, policy string
		passive      string
	}{
		{"active", activeShare, ""},
		{"shadow", selectedShadow, ""},
		{"passive", activeShare, "C"},
	} {
		t.Run(mode.name, func(t *testing.T) {
			reg := freshMetrics(t)
			f := newShareFleet()
			clock := time.Unix(0, 0)
			e := &Engine{Config: shadowConfig(t, mode.policy), client: sharePods(t, f)}
			e.utilizationShare.now = func() time.Time { return clock }
			if mode.passive != "" {
				// What observePassive records at the start of a cycle.
				e.cyclePassive = map[string]bool{utils.GetNamespacedKey("ns", mode.passive): true}
			}
			ctx, _ := observe(t)
			requests := func() []allocation.ModelScalingRequest {
				reqs := f.requests()
				for i := range reqs {
					reqs[i].Passive = reqs[i].ModelID == mode.passive
				}
				return reqs
			}
			var gaps map[string]metrics.ReplicaGap
			lowered := 0
			for range 20 {
				clock = clock.Add(30 * time.Second)
				ds := e.decideV2(ctx, shareStub(f), requests(), fullQuota(), f.scaleTargets())
				gaps = applyGaps(t, e, ds)
				if lowered = 9 - gaps["A-v"].Provided; lowered > 0 {
					break
				}
			}
			if mode.name != "active" {
				// The share sets nothing: every decision is today's
				// optimizer's. Its series say what it would set, in one
				// cycle: where its transfers would end, A lowered and B
				// raised by what A gives, as the active share gets there.
				ds := e.decideV2(ctx, shareStub(f), requests(), fullQuota(), f.scaleTargets())
				for _, d := range ds {
					require.Equal(t, f.current[d.ModelID], d.TargetReplicas, "the share changed %s's decision", d.VariantName)
				}
				gaps = applyGaps(t, e, ds)
				lowered = 9 - gaps["A-v"].Provided
				require.Positive(t, lowered, "the share would lower A")
				require.Equal(t, map[string]float64{"A-v": 9, "B-v": 5, "C-v": 2}, gapSeries(t, reg, constants.LLMSMReplicasSuggested))
				require.Equal(t, map[string]float64{"A-v/share-lent/": float64(lowered)}, gapSeries(t, reg, constants.LLMSMReplicasShort))
				require.Equal(t, map[string]float64{"A-v": 0, "B-v": float64(lowered), "C-v": 0},
					gapSeries(t, reg, constants.LLMSMReplicasBorrowed))
				want := map[string]string{"A-v": constants.ReplicasModeShadow, "B-v": constants.ReplicasModeShadow,
					"C-v": constants.ReplicasModeShadow}
				if mode.passive != "" {
					want[mode.passive+"-v"] = constants.ReplicasModePassive
				}
				require.Equal(t, want, gapModesOf(t, reg))
				return
			}
			require.Equal(t, map[string]string{"A-v": constants.ReplicasModeActive, "B-v": constants.ReplicasModeActive,
				"C-v": constants.ReplicasModeActive}, gapModesOf(t, reg))
			require.Positive(t, lowered, "setup: the share never lowered A")
			require.Equal(t, 9.0, gapSeries(t, reg, constants.LLMSMReplicasSuggested)["A-v"])
			require.Equal(t, map[string]float64{"A-v/share-lent/": float64(lowered)}, gapSeries(t, reg, constants.LLMSMReplicasShort))

			// A's pods are gone: the share raises B by what A gave.
			f.current["A"] -= lowered
			clock = clock.Add(30 * time.Second)
			gaps = applyGaps(t, e, e.decideV2(ctx, shareStub(f), requests(), fullQuota(), f.scaleTargets()))
			require.Equal(t, 5+lowered, gaps["B-v"].Provided, "setup: B raised by what A gave")
			require.Equal(t, 5.0, gapSeries(t, reg, constants.LLMSMReplicasSuggested)["B-v"])
			require.Equal(t, float64(lowered), gapSeries(t, reg, constants.LLMSMReplicasBorrowed)["B-v"])
			// B is not short; whatever A still lends is share-lent, measured
			// from what today's optimizer gives it now (what it runs).
			for k := range gapSeries(t, reg, constants.LLMSMReplicasShort) {
				require.Equal(t, "A-v/share-lent/", k)
			}
			require.Equal(t, float64(f.current["A"]-gaps["A-v"].Provided),
				gapSeries(t, reg, constants.LLMSMReplicasShort)["A-v/share-lent/"])
		})
	}
}

// The share filling a limiter's shortfall is not borrowing: it first covers
// what the limiters, then the ceiling, left short. A share that lowers a
// target is share-lent on top of the limiter's reason.
func TestReplicaGap_ShareOffsetsTheLimitersFirst(t *testing.T) {
	d := domain.VariantDecision{Namespace: "ns", ModelID: "m", VariantName: "v"}
	for _, tc := range []struct {
		name         string
		en           replicaGapEntry
		provided     int
		wantX        int
		wantShort    []metrics.ReplicaShort
		wantBorrowed int
	}{
		{"share fills part of a quota shortfall",
			replicaGapEntry{ask: 5, pass: 2, limitShort: 3, limitedBy: "q", shared: true, shareTarget: 4}, 4,
			5, []metrics.ReplicaShort{{Reason: "quota", LimiterName: "q", Replicas: 1}}, 0},
		{"share fills past it",
			replicaGapEntry{ask: 5, pass: 2, limitShort: 3, limitedBy: "q", shared: true, shareTarget: 7}, 7,
			5, nil, 2},
		{"share lowers a limited variant further",
			replicaGapEntry{ask: 5, pass: 2, limitShort: 3, limitedBy: "gpu-limiter", shared: true, shareTarget: 1}, 1,
			5, []metrics.ReplicaShort{{Reason: "gpu", Replicas: 3}, {Reason: "share-lent", Replicas: 1}}, 0},
		{"ceiling and quota", replicaGapEntry{ask: 3, overCap: 2, pass: 1, limitShort: 2, limitedBy: "q"}, 1,
			5, []metrics.ReplicaShort{{Reason: "quota", LimiterName: "q", Replicas: 2}, {Reason: "max-replicas", Replicas: 2}}, 0},
		{"a later stage moves X with Y", replicaGapEntry{ask: 3, pass: 1, limitShort: 2, limitedBy: "q"}, 2,
			4, []metrics.ReplicaShort{{Reason: "quota", LimiterName: "q", Replicas: 2}}, 0},
		{"held unrouted: nothing moves it", replicaGapEntry{ask: 3, pass: 1, limitShort: 2, held: true}, 1,
			1, nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &replicaGapPlan{entries: map[string]replicaGapEntry{"ns/v": tc.en}}
			g := p.gap(&d, tc.provided)
			require.Equal(t, tc.wantX, g.Suggested)
			require.Equal(t, tc.provided, g.Provided)
			require.Equal(t, tc.wantShort, g.Short)
			require.Equal(t, tc.wantBorrowed, g.Borrowed)
		})
	}
}

// The ResourceConstrained Event carries X and Y, through the apply pass.
func TestReplicaGap_ResourceConstrainedEventCarriesXAndY(t *testing.T) {
	e := wiringEngine(t, wiringDeployment())
	e.Config = stickyConfig(t, false)
	rec := record.NewFakeRecorder(10)
	e.Recorder = rec
	d := wiringDecision(2, 2, 0)
	d.WasLimited, d.LimitedBy = true, "team-quota"
	e.cycleGaps = &replicaGapPlan{entries: map[string]replicaGapEntry{
		utils.GetNamespacedKey(wiringNS, wiringTarget): {ask: 3, pass: 2, limitShort: 1, limitedBy: "team-quota"}}}
	va := wiringVA()
	e.applySaturationDecisions(context.Background(), []domain.VariantDecision{d},
		map[string]*variant.Variant{utils.GetNamespacedKey(wiringNS, wiringTarget): va}, map[string]*domain.Allocation{})
	var events []string
	for len(rec.Events) > 0 {
		events = append(events, <-rec.Events)
	}
	found := false
	for _, ev := range events {
		if strings.Contains(ev, constants.K8SEventResourceConstrained) {
			found = true
			require.Contains(t, ev, "(limited by team-quota): suggested 3 replicas, provided 2")
		}
	}
	require.True(t, found, "no ResourceConstrained event in %q", events)
}

// The apply pass publishes the cycle's gaps, and the next cycle replaces them:
// a variant no longer decided loses its series. A cycle with no active variant
// clears them all.
func TestReplicaGap_ApplyPassPublishesAndReplaces(t *testing.T) {
	e := wiringEngine(t, wiringDeployment())
	e.Config = stickyConfig(t, false)
	reg := prometheus.NewRegistry()
	require.NoError(t, metrics.InitMetrics(reg))
	vaMap := map[string]*variant.Variant{utils.GetNamespacedKey(wiringNS, wiringTarget): wiringVA()}
	e.cycleGaps = &replicaGapPlan{entries: map[string]replicaGapEntry{
		utils.GetNamespacedKey(wiringNS, wiringTarget): {ask: 3, pass: 2, limitShort: 1, limitedBy: "team-quota"}}}
	d := wiringDecision(2, 2, 0)
	e.applySaturationDecisions(context.Background(), []domain.VariantDecision{d}, vaMap, map[string]*domain.Allocation{})
	require.Equal(t, map[string]float64{wiringTarget: 3}, gapSeries(t, reg, constants.LLMSMReplicasSuggested))
	require.Equal(t, map[string]float64{wiringTarget + "/quota/team-quota": 1}, gapSeries(t, reg, constants.LLMSMReplicasShort))

	// No decision this cycle: the variant has no suggestion.
	e.cycleGaps = nil
	e.applySaturationDecisions(context.Background(), nil, vaMap, map[string]*domain.Allocation{})
	require.Empty(t, gapSeries(t, reg, constants.LLMSMReplicasSuggested))
	require.Empty(t, gapSeries(t, reg, constants.LLMSMReplicasShort))
}

// A namespace-scoped quota is read per namespace: a model in another namespace
// is not charged to it, and an unlimited pool publishes nothing.
func TestReplicaGap_QuotaRemainingPerNamespace(t *testing.T) {
	p := &replicaGapPlan{quotas: []*allocation.ResourceConstraints{{ProviderName: "ns-quota", NamespaceScoped: true,
		NamespacePools: map[string]map[string]allocation.ResourcePool{
			"a": {"A100": {Limit: 8, Used: 4}},
			"b": {"A100": {Limit: -1}},
		}}}}
	got := p.quotasAfterSuggested([]gapVariant{
		{namespace: "a", accelerator: "A100", current: 2, gpusPerReplica: 2, x: 5},
		{namespace: "b", accelerator: "A100", current: 1, gpusPerReplica: 2, x: 9},
	})
	require.Equal(t, []metrics.QuotaAfterSuggested{{LimiterName: "ns-quota", Scope: "a", Accelerator: "A100", GPUs: 8 - 4 - 3*2}}, got)
}

// A limiter holds the cheaper variant to one replica of the two it asks for,
// and the pass grows a sibling of the same role in its place: the role gets
// the three replicas it asks for, so nothing is short. Negative control for
// charging a limiter's shortfall per role (planReplicaGaps).
func TestReplicaGap_ASiblingCoversALimitedVariant(t *testing.T) {
	reg := freshMetrics(t)
	e := &Engine{Config: config.NewTestConfig()}
	req := growingA100("ns", "m", 1)
	req.CompositeSignal.Remaining = 2000
	req.CompositeSignal.Result.TotalDemand = 3000
	req.CompositeSignal.Result.VariantCapacities = append(req.CompositeSignal.Result.VariantCapacities,
		domain.VariantCapacity{VariantName: "m-w", PerReplicaCapacity: 1000})
	req.Variants = append(req.Variants, domain.VariantMetadata{VariantName: "m-w", AcceleratorName: "H100", Cost: 10})
	req.VariantStates = append(req.VariantStates, domain.VariantReplicaState{VariantName: "m-w", GPUsPerReplica: 1})
	cons := []*allocation.ResourceConstraints{{ProviderName: "team-quota",
		Pools: map[string]allocation.ResourcePool{"A100": {Limit: 2, Used: 1}, "H100": {Limit: 8}}}}
	ds := e.decideV2(t.Context(), allocation.NewGreedyByScoreOptimizer(), []allocation.ModelScalingRequest{req}, cons, nil)
	targets := map[string]int{}
	for _, d := range ds {
		targets[d.VariantName] = d.TargetReplicas
		if d.VariantName == "m-v" {
			require.True(t, d.WasLimited, "setup: the quota holds m-v")
		}
	}
	require.Equal(t, map[string]int{"m-v": 2, "m-w": 1}, targets, "setup: m-w grows in m-v's place")
	applyGaps(t, e, ds)
	require.Equal(t, map[string]float64{"m-v": 2, "m-w": 1}, gapSeries(t, reg, constants.LLMSMReplicasSuggested))
	require.Empty(t, gapSeries(t, reg, constants.LLMSMReplicasShort))
}

// The asks are computed on copies: the requests the cycle decides with are
// exactly as they were.
func TestReplicaGap_AsksLeaveTheRequestsAlone(t *testing.T) {
	reqs := []allocation.ModelScalingRequest{maxCapped(true), growingA100("ns", "n", 8)}
	before := []allocation.ModelScalingRequest{maxCapped(true), growingA100("ns", "n", 8)}
	replicaAsks(t.Context(), reqs)
	require.Equal(t, before, reqs)
}
