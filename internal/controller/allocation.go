package controller

import (
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// BuildAllocationFromMetrics assembles an Allocation struct from raw optimizer metrics
// and Kubernetes resources. This delegates to utils.BuildAllocationFromMetrics.
func BuildAllocationFromMetrics(
	metrics domain.OptimizerMetrics,
	va *variant.Variant,
	scaleTarget scaletarget.ScaleTargetAccessor,
	acceleratorCost float64,
) (domain.Allocation, error) {
	return utils.BuildAllocationFromMetrics(metrics, va, scaleTarget, acceleratorCost)
}
