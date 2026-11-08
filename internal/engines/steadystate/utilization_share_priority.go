package steadystate

// Whole-node receivers and pod priority (issue ev-shindin/llm-scaling-manager#234). A node the optimizer
// frees for a whole-node model opens one GPU at a time as the donors' pods go,
// and the receiver's pod fits only once all of them are free. The scheduler
// holds nothing for a pending pod of equal priority, so any pending pod that
// needs fewer GPUs takes the first one, and the transfer ends release-taken.
// A receiver at a higher PriorityClass preempts that pod and is nominated to
// the node, which then stays its own. The optimizer cannot set the class: it
// is the cluster owner's choice, since a pending pod of the model evicts
// lower-priority pods anywhere. It says when a whole-node model has none.

import (
	"slices"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/accelerator"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

// shareWholeNodeUnprioritized lists, as "namespace/variant", the variants of
// group g whose pod needs a whole node of the group's accelerator -- as many
// GPUs as its largest node has -- and whose pod template sets no
// priorityClassName. A LeaderWorkerSet is listed when its leader or worker
// template sets none. Nil without a node picture: the optimizer then frees no
// node. A node is the group's by accelerator.SameName, as node-aware planning
// matches it: the group takes the budget's spelling ("H200"), the node its
// label's ("NVIDIA-H200"). A cordoned node counts, so cordoning one for
// maintenance does not change the list.
func shareWholeNodeUnprioritized(g allocation.ShareGroup, nodes map[string]decision.NodeGPU,
	accessor func(role, variant string) scaletarget.ScaleTargetAccessor) []string {
	largest := 0
	for _, n := range nodes {
		if accelerator.SameName(n.Accelerator, g.AcceleratorType) {
			largest = max(largest, n.Capacity)
		}
	}
	if largest == 0 {
		return nil
	}
	var out []string
	for role, vs := range g.Variants {
		for _, v := range vs {
			acc := accessor(role, v.Name)
			if acc == nil || slices.Max(append(scaletarget.PodGPUs(acc), 0)) < largest {
				continue
			}
			named := true
			for _, t := range []*corev1.PodTemplateSpec{acc.GetLeaderPodTemplateSpec(), acc.GetWorkerPodTemplateSpec()} {
				if t != nil && t.Spec.PriorityClassName == "" {
					named = false
				}
			}
			if !named {
				out = append(out, utils.GetNamespacedKey(g.Origins[role].Namespace, v.Name))
			}
		}
	}
	slices.Sort(out)
	return out
}

// reportWholeNodeUnprioritized logs, once per change, the whole-node variants
// that run at no PriorityClass (shareWholeNodeUnprioritized).
func (st *utilizationShareState) reportWholeNodeUnprioritized(logger logr.Logger, variants []string) {
	slices.Sort(variants)
	joined := strings.Join(variants, ",")
	if joined == st.lastUnprioritized {
		return
	}
	st.lastUnprioritized = joined
	if joined != "" {
		logger.Info("WARNING: Utilization share: whole-node models set no priorityClassName; a node freed for one "+
			"can be taken by any pending pod before its replica lands (release-taken). Give them a PriorityClass "+
			"above the cluster's other GPU pods", "variants", variants)
	}
}
