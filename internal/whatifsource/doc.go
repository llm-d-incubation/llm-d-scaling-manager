// Package whatifsource reads the what-if planner's inputs from Prometheus
// alone (docs/proposals/slo-capacity-planner.md, "Packaging"; R2 in
// docs/proposals/analyzer-learner.md, 2.9).
//
// R2 is the capture: Capture builds, for every step of a past window and
// every model in a namespace, the learnrec.Input a controller's analyzers
// would have read then, and writes them as a capture file that
// learnreplay's Generate mode plays through the analyzers. The rows are the
// collector's own (collector.ReplicaMetricsCollector over its registered
// query templates), so a capture reads every field a recording does, the
// TTFT model's inputs included, by the same rules; only the Prometheus
// client underneath answers each instant query from one range query over
// the window (windowAPI).
//
// What a controller reads from the cluster comes from kube-state-metrics in
// the same Prometheus, or from --role-of selectors (RoleOf), since a
// kubeconfig sees only today's Pods (fleet.go). Engine parameters are not
// in kube-state-metrics; they come from flags (Options.EngineArgs), and the
// capture says so.
//
// The package sits outside the pipeline's layers, like the controller: the
// only importer of the collector and a Prometheus client besides the
// engine. Every query it asks is a range query.
package whatifsource
