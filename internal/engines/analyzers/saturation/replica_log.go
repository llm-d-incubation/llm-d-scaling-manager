package saturation

import (
	"sort"
	"strconv"

	"github.com/go-logr/logr"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// perReplicaLogLevel is the V() level of the lines written once per replica
// per cycle: replica-capacity-decision, k2-decision and derived-mu. At the
// shipped verbosity (logging.DEFAULT) they were most of the controller's log,
// about 1.9 KB per replica per cycle, and they grow with the fleet. One level
// above it they stay a --v=3 away for a benchmark or a diagnosis, and
// replica-capacity-summary says what each replica was priced at, at the
// default, in a few dozen bytes a replica.
const perReplicaLogLevel = logging.VERBOSE

// criMaxLogLineBytes is the longest log line the container runtime keeps
// whole. The CRI logging format splits a longer line into partial lines
// (containerd and CRI-O both buffer 16 KiB per line), so `kubectl logs` and
// any grep over it see a long replica-capacity-summary as two lines, and a
// match on its variant misses the pods in the second one.
const criMaxLogLineBytes = 16 * 1024

// summaryEntryBudget is how many bytes of replica entries one
// replica-capacity-summary line carries: half the CRI line, leaving the other
// half for the timestamp, level, caller, message and fixed fields, which take
// a few hundred bytes and are written by an encoder this package does not own.
const summaryEntryBudget = criMaxLogLineBytes / 2

// logReplicaCapacitySummary writes replica-capacity-summary at the shipped
// verbosity: which replicas of each variant were priced this cycle and what
// each was priced at (replicaSummary). muMissing maps a decode pod with no
// derived mu to why (whatif.DerivedMu.Why). A variant whose entries would pass
// summaryEntryBudget is written over several lines, each with "part" and
// "parts"; "priced" is always the variant's whole count.
func logReplicaCapacitySummary(logger logr.Logger, modelID, namespace string,
	caps []capacity.ReplicaCapacity, muMissing map[string]string) {
	if len(caps) == 0 {
		return
	}
	byVariant := make(map[string][]string)
	for _, rc := range caps {
		byVariant[rc.VariantName] = append(byVariant[rc.VariantName], replicaSummary(rc, muMissing[rc.PodName]))
	}
	variants := make([]string, 0, len(byVariant))
	for v := range byVariant {
		variants = append(variants, v)
	}
	sort.Strings(variants)
	for _, v := range variants {
		replicas := byVariant[v]
		sort.Strings(replicas)
		parts := splitEntries(replicas, summaryEntryBudget)
		for i, part := range parts {
			kv := []any{"modelID", modelID, "namespace", namespace, "variant", v, "priced", len(replicas)}
			if len(parts) > 1 {
				kv = append(kv, "part", i+1, "parts", len(parts))
			}
			logger.V(logging.DEFAULT).Info("replica-capacity-summary", append(kv, "replicas", part)...)
		}
	}
}

// splitEntries groups entries, in order, so each group's encoded size (each
// entry quoted, with a separating comma) stays within budget. An entry larger
// than budget on its own gets a group of its own.
func splitEntries(entries []string, budget int) [][]string {
	var out [][]string
	start, size := 0, 0
	for i, e := range entries {
		n := len(e) + len(`"",`)
		if i > start && size+n > budget {
			out = append(out, entries[start:i])
			start, size = i, 0
		}
		size += n
	}
	if start < len(entries) {
		out = append(out, entries[start:])
	}
	return out
}

// replicaSummary is one replica's entry in replica-capacity-summary:
// "<pod>=<capacity>/<bound>/<k2 source>", then, when they apply,
// "/<P1-obs-* label>" for a saturated reading not recorded this cycle,
// "/mu:<why>" for a decode replica with no derived mu, and "/bridge" for a
// warm-pool Pod lent to the variant. The bound is k2 when the compute bound is
// the smaller and k1 otherwise, the same rule as replica-capacity-decision's
// boundBy.
func replicaSummary(rc capacity.ReplicaCapacity, muWhy string) string {
	bound := "k1"
	if rc.ComputeBoundCapacity < rc.MemoryBoundCapacity {
		bound = "k2"
	}
	s := rc.PodName + "=" + strconv.FormatInt(rc.EffectiveCapacity, 10) + "/" + bound + "/" + rc.K2Priority.String()
	if rc.K2NotRecorded != "" {
		s += "/" + rc.K2NotRecorded
	}
	if muWhy != "" {
		s += "/mu:" + muWhy
	}
	if rc.FromWarmPool {
		s += "/bridge"
	}
	return s
}
