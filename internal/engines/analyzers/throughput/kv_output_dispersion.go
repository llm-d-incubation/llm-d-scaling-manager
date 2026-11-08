package throughput

import (
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/fleet"
)

// The KV a resident request holds, with output lengths that vary (issue ev-shindin/llm-scaling-manager#433,
// split from ev-shindin/llm-scaling-manager#276; the same correction as saturation's, kv_output_dispersion.go
// there, and the slo analyzer's, output_dispersion.go there).
//
// A long request stays resident longer, so a KV snapshot finds it in
// proportion to how long it stays, and the average resident request holds
// ILeff + D*OL/2, D = E[O^2]/E[O]^2 (shape.WithOutputDispersion). With D taken
// as 1 this analyzer read too many sequences from a fill, by 7-44% at IL = 2
// OL: its supply (computeVariantSupply) read high, and it ordered too few
// replicas where saturation, on the same variant, now prices the spread.
//
// Here KVreq sets how many sequences a fill holds, in the supply at k_sat, the
// local demand at k* (computeLocalDemand) and the GPS check. The model-level
// demand (arrival rate x OL) does not read it, so a larger D lowers the supply
// against an unchanged demand, and orders more replicas.
//
// D is read as saturation and slo read it (fleet.OutputDispersion): from each
// replica's lower bound on E[O^2] (domain.SquaredMeanLowerBound),
// request-weighted over the replicas the variant's shape averages
// (averageShapeMetrics), and only when every one of them read the bound;
// otherwise 1, no spread.
//
// As saturation does, it is held at 1 while a shape change is recent
// (spreadHeld): the [5m] histogram then holds both shapes, and their
// difference reads as a spread of one workload's lengths. Priced, that D is
// high, the supply low, and every shape swap would over-provision until the
// window turns over.

// SwitchKVPerRequestOutputDispersion is the replay switch for the spread of
// output lengths in the KV per request on the throughput analyzer (issue
// ev-shindin/llm-scaling-manager#433). On, a resident request is priced at ILeff + D*OL/2, D the spread
// read from the output-length histogram. Off, as before, at ILeff + OL/2.
const SwitchKVPerRequestOutputDispersion = "throughput-kv-per-request-output-dispersion"

// DefaultShapeChangeSpreadHold is how long after a variant's shape change its
// spread is held at 1 when the policy sets no shapeChangeHoldSeconds: the
// histogram's [5m] window, which holds the old shape's requests until then,
// as saturation's ShapeChangeHoldMax.
const DefaultShapeChangeSpreadHold = 5 * time.Minute

// shapeChangeWindowReader is the policy's shape-change window, as
// config.ScalingPolicy.ShapeChangeWindow reads shapeChangeHoldSeconds.
type shapeChangeWindowReader interface {
	ShapeChangeWindow(defaultWindow time.Duration) time.Duration
}

// spreadHoldOf is the window a shape change holds the spread at 1 for, under
// cfg: the policy's shape-change window, DefaultShapeChangeSpreadHold
// without one.
func spreadHoldOf(cfg domain.AnalyzerConfig) time.Duration {
	if r, ok := cfg.(shapeChangeWindowReader); ok {
		return r.ShapeChangeWindow(DefaultShapeChangeSpreadHold)
	}
	return DefaultShapeChangeSpreadHold
}

// spreadHeld reports whether the variant's last shape change is within hold
// of now: its spread is then held at 1. False before any change.
func spreadHeld(state *variantState, hold time.Duration, now time.Time) bool {
	return !state.shapeChangedAt.IsZero() && now.Sub(state.shapeChangedAt) < hold
}

// variantOutputDispersion is the spread of a variant's output lengths, D =
// E[O^2]/E[O]^2, over the replicas averageShapeMetrics averages and with its
// weights, so the two are one reading of one variant: 1 unless every one of
// them read a positive, finite bound on E[O^2] (fleet.OutputDispersion).
func variantOutputDispersion(rows []domain.ReplicaMetrics) float64 {
	d, _ := fleet.OutputDispersion(rows, shapeEligible)
	return d
}
