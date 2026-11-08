package throughput

// newParentLineAnalyzer is an analyzer with learner PR 4b-1's switches off:
// the ITL line as it was before the move onto itl.Tracker, which fits a line
// from a single cycle's rows. The specs written for that line, and those of
// the supply, demand and aggregation arithmetic that need a line on their
// first cycle, run on it: they hold the switch-off path, which stays until
// the seam is deleted. The line on the tracker has its own tests
// (itl_line_test.go), and the two switches theirs.
func newParentLineAnalyzer() *ThroughputAnalyzer {
	a := NewThroughputAnalyzer()
	a.itlSteadyGate = false
	a.itlKeepsOnGPSMismatch = false
	return a
}
