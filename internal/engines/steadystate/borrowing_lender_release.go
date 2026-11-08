package steadystate

// Handing borrowed capacity back to a waiting lender Kueue does not reclaim
// for, on the engine side (PR-C, kueue.releaseToNonReclaimingLenders): the
// governor decides (allocation/borrow_lender_release.go); this file tells it
// how long a release takes to land, per cohort, from the cluster's own
// configuration, and tells on-call about the lenders it could not serve.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

// releaseLatencies is, per cohort whose pool opts in to
// releaseToNonReclaimingLenders, how long a lowered target of a model a
// release there may lower takes to free its GPUs: the slowest over those
// variants alone (allocation.LenderReleaseScope) of their ScaledObject's
// scale-down window and polling interval and their pods' termination grace,
// at the cluster's defaults where unset (allocation.ReleaseBound), capped at
// allocation.MaxReleaseLatency. A tenant in another cohort, or one whose
// models no release there lowers, does not lengthen it. Nil when no pool
// opts in.
func releaseLatencies(e *Engine, constraints []*allocation.ResourceConstraints,
	requests []allocation.ModelScalingRequest, scaleTargets map[string]scaletarget.ScaleTargetAccessor) map[string]time.Duration {
	scope := allocation.LenderReleaseScope(constraints, requests)
	if scope == nil {
		return nil
	}
	out := make(map[string]time.Duration, len(scope))
	for cohort, variants := range scope {
		slowest := time.Duration(0)
		for _, k := range variants {
			slowest = max(slowest, variantReleaseBound(e, k, scaleTargets[k]))
		}
		if len(variants) == 0 {
			slowest = allocation.ReleaseBound(nil, nil, nil)
		}
		out[cohort] = min(slowest, allocation.MaxReleaseLatency)
	}
	return out
}

// variantReleaseBound is allocation.ReleaseBound for the variant
// "namespace/variant" whose scale target is acc (nil: the defaults).
func variantReleaseBound(e *Engine, key string, acc scaletarget.ScaleTargetAccessor) time.Duration {
	var window, polling, grace *time.Duration
	if acc != nil {
		for _, tpl := range []*corev1.PodTemplateSpec{acc.GetLeaderPodTemplateSpec(), acc.GetWorkerPodTemplateSpec()} {
			if tpl != nil && tpl.Spec.TerminationGracePeriodSeconds != nil {
				longer(&grace, time.Duration(*tpl.Spec.TerminationGracePeriodSeconds)*time.Second)
			}
		}
		ns, _, _ := strings.Cut(key, "/")
		if e.Variants != nil {
			entry, ok := e.Variants.FindByScaleTarget(ns, constants.DeploymentKind, acc.GetName())
			if !ok {
				entry, ok = e.Variants.FindByScaleTarget(ns, constants.LeaderWorkerSetKind, acc.GetName())
			}
			if ok {
				if w := entry.Target.ScaleDownWindowSeconds; w != nil {
					longer(&window, time.Duration(*w)*time.Second)
				}
				if p := entry.Target.PollingIntervalSeconds; p != nil {
					longer(&polling, time.Duration(*p)*time.Second)
				}
			}
		}
	}
	return allocation.ReleaseBound(window, polling, grace)
}

// longer sets *cur to d when unset or shorter.
func longer(cur **time.Duration, d time.Duration) {
	if *cur == nil || d > **cur {
		v := d
		*cur = &v
	}
}

// reportLenders tells on-call about the waiting lenders the cycle's Releases
// handed nothing (allocation.LenderReport): each is published on
// llmsm_kueue_lender_release_blocked, and, once per change, logged and recorded
// as a Warning Event on the scale targets of the models that were candidates.
// A pool where Kueue's borrowed figure exceeds what this controller holds is
// logged as a warning once per change: another borrower holds the rest,
// possibly a second scaling manager handing the same lender the same GPUs.
func (e *Engine) reportLenders(ctx context.Context, requests []allocation.ModelScalingRequest,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor) {
	st := &e.borrowing
	report := st.governor.LenderReport()
	logger := ctrl.LoggerFrom(ctx)
	if st.lenderBlocked == nil {
		st.lenderBlocked = map[string]string{}
	}
	if st.sharedWarned == nil {
		st.sharedWarned = map[string]string{}
	}
	series := make([]metrics.BlockedLender, 0, len(report.Blocked))
	seen := map[string]bool{}
	for _, b := range report.Blocked {
		series = append(series, metrics.BlockedLender{Cohort: b.Lender.Cohort, ClusterQueue: b.Lender.Lender, Reason: b.Reason})
		k := b.Lender.Cohort + "/" + b.Lender.Flavor + "/" + b.Lender.Resource + "/" + b.Lender.Lender
		seen[k] = true
		sig := fmt.Sprintf("%s/%d/%d", b.Reason, b.Need, b.Givable)
		if st.lenderBlocked[k] == sig {
			continue
		}
		st.lenderBlocked[k] = sig
		msg := lenderBlockedMessage(b)
		logger.Info(msg, "lenderClusterQueue", b.Lender.Lender, "cohort", b.Lender.Cohort, "flavor", b.Lender.Flavor,
			"reason", b.Reason, "needGPUs", b.Need, "givableGPUs", b.Givable, "models", b.Models)
		e.lenderBlockedEvents(b, msg, requests, scaleTargets, constants.K8SEventLenderReleaseBlocked)
	}
	for k := range st.lenderBlocked {
		if !seen[k] {
			delete(st.lenderBlocked, k)
		}
	}
	metrics.PublishLenderReleaseBlocked(series)
	for _, m := range report.HalfLowered {
		msg := "A release to a waiting lender outlived twice its timeout with this P/D model half lowered: one role " +
			"scaled down, its partner did not, and the release's pins are dropped. Check the scale-down of the " +
			"role still above its target (its ScaledObject's scale-down policies and its pods' termination grace)"
		logger.Info("Warning: "+msg, "model", m)
		e.lenderBlockedEvents(allocation.LenderReleaseBlock{Models: []string{m}}, msg, requests, scaleTargets,
			constants.K8SEventLenderReleaseHalfLanded)
	}
	shared := map[string]bool{}
	for _, s := range report.Shared {
		k := s.Cohort + "/" + s.Flavor + "/" + s.Resource
		shared[k] = true
		sig := fmt.Sprintf("%d/%d", s.Kueue, s.Ours)
		if st.sharedWarned[k] == sig {
			continue
		}
		st.sharedWarned[k] = sig
		logger.Info("Warning: Kueue's borrowed figure in this cohort exceeds what this controller's namespaces hold past "+
			"their guaranteed figures; another borrower holds the rest. With releaseToNonReclaimingLenders, run one "+
			"scaling manager per cohort: a second one would hand a waiting lender the same GPUs again",
			"cohort", s.Cohort, "flavor", s.Flavor, "kueueBorrowedGPUs", s.Kueue, "thisControllerGPUs", s.Ours)
	}
	for k := range st.sharedWarned {
		if !shared[k] {
			delete(st.sharedWarned, k)
		}
	}
}

// lenderBlockedMessage says why a waiting lender was handed nothing.
func lenderBlockedMessage(b allocation.LenderReleaseBlock) string {
	if b.Reason == allocation.LenderPreempting {
		return fmt.Sprintf("ClusterQueue %s waits in cohort %s while %d GPUs of Workloads Kueue preempted still stop: "+
			"nothing is handed back until they are free, at most 30 minutes after each eviction "+
			"(releaseToNonReclaimingLenders)", b.Lender.Lender, b.Lender.Cohort, b.Givable)
	}
	if b.Reason == allocation.LenderTaken {
		return fmt.Sprintf("ClusterQueue %s still waits in cohort %s after capacity was handed back to it, and Kueue's "+
			"borrowed figure did not fall: another borrower took it. Nothing more is handed back to it while it waits "+
			"(releaseToNonReclaimingLenders)", b.Lender.Lender, b.Lender.Cohort)
	}
	return fmt.Sprintf("ClusterQueue %s waits in cohort %s for %d GPUs more than the free pool covers, and this "+
		"controller can hand back only %d on its flavor without going below minReplicas or a guaranteed figure: "+
		"nothing is handed back, it would sit idle (releaseToNonReclaimingLenders)", b.Lender.Lender, b.Lender.Cohort,
		b.Need, b.Givable)
}

// lenderBlockedEvents records msg as a Warning Event of reason on one scale
// target of each model of b.
func (e *Engine) lenderBlockedEvents(b allocation.LenderReleaseBlock, msg string, requests []allocation.ModelScalingRequest,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor, reason string) {
	models := map[string]bool{}
	for _, m := range b.Models {
		models[m] = true
	}
	for _, req := range requests {
		if !models[req.Namespace+"/"+req.ModelID] {
			continue
		}
		names := variantNames(req.VariantStates)
		slices.Sort(names)
		for _, v := range names {
			if acc := scaleTargets[req.Namespace+"/"+v]; acc != nil {
				e.shareEvent(acc, corev1.EventTypeWarning, reason, msg)
				break
			}
		}
	}
}
