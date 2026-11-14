// Package whatifapi is the capacity planner's HTTP API
// (docs/proposals/capacity-planner.md, "The API"): POST /v1/inputs resolves
// a source (a Prometheus window, or a recording) for one model into an
// inputsId and every term it rests on, and POST /v1/plan prices a scenario
// against that id: curves, points, limits, marginal deltas and flags, as
// JSON.
//
// The package builds no model. Every figure comes from a Fleet, which
// cmd/capacity-planner implements on the CLI's own code paths (internal/signals/whatif
// through the CLI's flags-to-fleet wiring), so a plan's numbers are the ones
// `capacity-planner slo-to-rate` and `capacity-planner replicas-to-slo` print; a parity test in
// cmd/capacity-planner holds them to the CLI's JSON goldens. What the package adds is
// the shape: which figure is priced at which rate, the curve grid, the
// limits read side by side, caching, and authentication.
//
// The wire rule: a figure is an object holding a value or a refusal, never
// both; a refusal carries no number, and no refusal is written as 0.
package whatifapi
