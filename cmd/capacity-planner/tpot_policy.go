package main

import (
	"fmt"
	"io"
	"strings"
)

// warnTPOTPercentile warns, on warn, that a TPOT target given as a
// percentile (--tpot 30ms@p99) cannot be carried into a scaling policy as it
// is: the policy's slo.tpot is the mean of the router's per-request TPOT and
// takes no percentile, and the controller's bound on decode packing holds the
// mean at it (docs/reference/latency-definitions.md). Nothing for a mean
// target, "none", or no target.
func warnTPOTPercentile(warn io.Writer, target string) {
	if warn == nil {
		return
	}
	_, pct, ok := strings.Cut(target, "@")
	if !ok || !strings.HasPrefix(pct, "p") {
		return
	}
	fmt.Fprintf(warn, "%s: warning: --tpot %s is a percentile. A scaling policy's slo.tpot is the mean of the "+
		"router's per-request TPOT and takes no percentile, so this target cannot be written into the policy: "+
		"there, write the mean TPOT this answer reads at the replicas you choose. The controller bounds decode "+
		"packing at that target where its ITL line allows (a mean ITL, not this per-request %s), and the %s runs "+
		"above the mean. See docs/reference/latency-definitions.md.\n", progName, target, pct, pct)
}
