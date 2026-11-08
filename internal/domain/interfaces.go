package domain

import (
	"context"

	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

type Actuator interface {
	// EmitMetrics publishes metrics for external autoscalers (e.g., HPA, KEDA).
	// This includes real-time current state and Inferno's optimization targets.
	EmitMetrics(
		ctx context.Context,
		Variants *llmdvariant.Variant,
	) error
}
