package scalefromzero

import (
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

const selNS = "chat"

// pools builds a single cluster-scoped provider with the given free capacity.
func pools(free map[string]int) []*allocation.ResourceConstraints {
	p := make(map[string]allocation.ResourcePool, len(free))
	for accType, avail := range free {
		p[accType] = allocation.ResourcePool{Limit: avail, Used: 0}
	}
	return []*allocation.ResourceConstraints{{ProviderName: "gpu-limiter", Pools: p}}
}

func cand(name, role, accel string, gpus int, cost float64) Candidate {
	return Candidate{
		VariantName:     name,
		ScaleTargetName: name + "-deploy",
		Role:            role,
		Accelerator:     accel,
		GPUsPerReplica:  gpus,
		Cost:            cost,
	}
}

// names extracts the selected variant names, order-independent comparison being
// the caller's job.
func names(set []Candidate) []string {
	out := make([]string, 0, len(set))
	for _, c := range set {
		out = append(out, c.VariantName)
	}
	return out
}

func hasAll(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]bool, len(got))
	for _, g := range got {
		seen[g] = true
	}
	for _, w := range want {
		if !seen[w] {
			return false
		}
	}
	return true
}

// TestSelectExhaustivePairBeatsGreedy is the reason the search is exhaustive.
// The cheapest decode (d-cheap, 4xH100) leaves only 0 H100 free, so no prefill
// fits beside it; the feasible pair needs the COSTLIER decode (d-l4, 2xL4). A
// greedy "cheapest decode, then find a prefill" would report no pair and drop to
// decode-only, giving up prefill capacity that exists.
func TestSelectExhaustivePairBeatsGreedy(t *testing.T) {
	in := SelectionInput{
		Namespace: selNS,
		Candidates: []Candidate{
			cand("d-cheap", domain.RoleDecode, "H100", 4, 1),
			cand("d-l4", domain.RoleDecode, "L4", 2, 3),
			cand("p-h100", domain.RolePrefill, "H100", 2, 1),
		},
		Constraints: pools(map[string]int{"H100": 4, "L4": 2}),
	}

	set, outcome := selectServingSet(in)
	if outcome != OutcomePair {
		t.Fatalf("outcome = %q, want %q; a feasible pair exists and must be found", outcome, OutcomePair)
	}
	if got := names(set); !hasAll(got, "d-l4", "p-h100") {
		t.Fatalf("selected %v, want the only feasible pair [d-l4 p-h100]", got)
	}
}

func TestSelectServingSet(t *testing.T) {
	tests := []struct {
		name        string
		in          SelectionInput
		wantOutcome SelectionOutcome
		wantSet     []string
	}{
		{
			name: "non-disaggregated model wakes its cheapest variant",
			in: SelectionInput{
				Namespace: selNS,
				Candidates: []Candidate{
					cand("v-expensive", domain.RoleBoth, "H100", 1, 9),
					cand("v-cheap", domain.RoleBoth, "L4", 1, 2),
				},
				Constraints: pools(map[string]int{"H100": 8, "L4": 8}),
			},
			wantOutcome: OutcomeDecodeOnly,
			wantSet:     []string{"v-cheap"},
		},
		{
			name: "P/D model wakes decode and prefill together when both fit",
			in: SelectionInput{
				Namespace: selNS,
				Candidates: []Candidate{
					cand("d", domain.RoleDecode, "H100", 2, 3),
					cand("p", domain.RolePrefill, "H100", 2, 1),
				},
				Constraints: pools(map[string]int{"H100": 8}),
			},
			wantOutcome: OutcomePair,
			wantSet:     []string{"d", "p"},
		},
		{
			name: "prefill dropped when the pair will not fit but decode alone will",
			in: SelectionInput{
				Namespace: selNS,
				Candidates: []Candidate{
					cand("d", domain.RoleDecode, "H100", 2, 3),
					cand("p", domain.RolePrefill, "H100", 2, 1),
				},
				// Room for one of them, not both: the decode wins because the
				// router runs prefill on the decode worker anyway.
				Constraints: pools(map[string]int{"H100": 2}),
			},
			wantOutcome: OutcomeDecodeOnly,
			wantSet:     []string{"d"},
		},
		{
			name: "requirePrefill refuses a decode-only wake",
			in: SelectionInput{
				Namespace: selNS,
				Candidates: []Candidate{
					cand("d", domain.RoleDecode, "H100", 2, 3),
					cand("p", domain.RolePrefill, "H100", 2, 1),
				},
				Constraints:    pools(map[string]int{"H100": 2}),
				RequirePrefill: true,
			},
			wantOutcome: OutcomePrefillRequired,
			wantSet:     nil,
		},
		{
			name: "requirePrefill still wakes the pair when it fits",
			in: SelectionInput{
				Namespace: selNS,
				Candidates: []Candidate{
					cand("d", domain.RoleDecode, "H100", 2, 3),
					cand("p", domain.RolePrefill, "H100", 2, 1),
				},
				Constraints:    pools(map[string]int{"H100": 4}),
				RequirePrefill: true,
			},
			wantOutcome: OutcomePair,
			wantSet:     []string{"d", "p"},
		},
		{
			name: "nothing is woken when no decode fits",
			in: SelectionInput{
				Namespace:   selNS,
				Candidates:  []Candidate{cand("d", domain.RoleDecode, "H100", 4, 1)},
				Constraints: pools(map[string]int{"H100": 2}),
			},
			wantOutcome: OutcomeNoCapacity,
			wantSet:     nil,
		},
		{
			name: "a prefill-only population has nothing that can serve",
			in: SelectionInput{
				Namespace:   selNS,
				Candidates:  []Candidate{cand("p", domain.RolePrefill, "H100", 2, 1)},
				Constraints: pools(map[string]int{"H100": 8}),
			},
			wantOutcome: OutcomeNoDecodeCandidate,
			wantSet:     nil,
		},
		{
			name: "a model already serving with nothing left to wake is reported as such, not as a refusal",
			in: SelectionInput{
				Namespace:     selNS,
				Candidates:    []Candidate{cand("d2", domain.RoleDecode, "H100", 2, 1)},
				DecodeCovered: true,
				Constraints:   pools(map[string]int{"H100": 8}),
			},
			// Distinct from the refusals: the caller must be able to stay silent
			// for this one, since it is the steady state for every serving model
			// with a queue and the loop runs at 10Hz.
			wantOutcome: OutcomeAlreadyServing,
			wantSet:     nil,
		},
		{
			// The two halves of a P/D model are discovered when KEDA first calls
			// about each, so they routinely become visible on different ticks and
			// decode is woken alone. Without this, the parked prefill is stranded
			// FOREVER: this engine would report already-serving from then on, and
			// the saturation engine only scales what is already running. The model
			// keeps serving, silently degraded, which is why it went unnoticed.
			name: "a serving model's parked prefill is still woken",
			in: SelectionInput{
				Namespace:     selNS,
				Candidates:    []Candidate{cand("p", domain.RolePrefill, "H100", 2, 1)},
				DecodeCovered: true,
				Constraints:   pools(map[string]int{"H100": 8}),
			},
			wantOutcome: OutcomePrefillCatchUp,
			wantSet:     []string{"p"},
		},
		{
			// Charged for its own demand only: the running decode already holds
			// its GPUs and is counted as used, so billing for it again here would
			// refuse a prefill that fits.
			name: "the catch-up prefill is judged on its own demand",
			in: SelectionInput{
				Namespace:     selNS,
				Candidates:    []Candidate{cand("p", domain.RolePrefill, "H100", 2, 1)},
				DecodeCovered: true,
				Constraints:   pools(map[string]int{"H100": 2}),
			},
			wantOutcome: OutcomePrefillCatchUp,
			wantSet:     []string{"p"},
		},
		{
			// A model that cannot complete still serves. Catching up is best
			// effort, not a refusal to report.
			name: "a serving model whose prefill does not fit stays already-serving",
			in: SelectionInput{
				Namespace:     selNS,
				Candidates:    []Candidate{cand("p", domain.RolePrefill, "H100", 4, 1)},
				DecodeCovered: true,
				Constraints:   pools(map[string]int{"H100": 2}),
			},
			wantOutcome: OutcomeAlreadyServing,
			wantSet:     nil,
		},
		{
			name: "an already-running prefill is not duplicated",
			in: SelectionInput{
				Namespace: selNS,
				Candidates: []Candidate{
					cand("d", domain.RoleDecode, "H100", 2, 3),
					cand("p", domain.RolePrefill, "H100", 2, 1),
				},
				PrefillCovered: true,
				Constraints:    pools(map[string]int{"H100": 8}),
			},
			wantOutcome: OutcomeDecodeOnly,
			wantSet:     []string{"d"},
		},
		{
			name: "unknown constraints do not block the wake",
			in: SelectionInput{
				Namespace:  selNS,
				Candidates: []Candidate{cand("d", domain.RoleDecode, "H100", 128, 1)},
			},
			wantOutcome: OutcomeDecodeOnly,
			wantSet:     []string{"d"},
		},
		{
			name: "equal-cost candidates resolve deterministically by name",
			in: SelectionInput{
				Namespace: selNS,
				Candidates: []Candidate{
					cand("d-b", domain.RoleBoth, "H100", 1, 5),
					cand("d-a", domain.RoleBoth, "H100", 1, 5),
				},
				Constraints: pools(map[string]int{"H100": 8}),
			},
			wantOutcome: OutcomeDecodeOnly,
			wantSet:     []string{"d-a"},
		},
		{
			name: "a namespace quota that excludes the accelerator blocks the wake",
			in: SelectionInput{
				Namespace:  selNS,
				Candidates: []Candidate{cand("d", domain.RoleDecode, "H100", 1, 1)},
				Constraints: []*allocation.ResourceConstraints{{
					ProviderName: "quota-limiter",
					Pools:        map[string]allocation.ResourcePool{"H100": {Limit: 64}},
					NamespacePools: map[string]map[string]allocation.ResourcePool{
						selNS: {"L4": {Limit: 8}},
					},
				}},
			},
			wantOutcome: OutcomeNoCapacity,
			wantSet:     nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set, outcome := selectServingSet(tt.in)
			if outcome != tt.wantOutcome {
				t.Fatalf("outcome = %q, want %q", outcome, tt.wantOutcome)
			}
			if got := names(set); !hasAll(got, tt.wantSet...) {
				t.Fatalf("selected %v, want %v", got, tt.wantSet)
			}
		})
	}
}

// TestDemandOfSumsSharedAccelerator pins the reason demand is a map summed over
// the set: two variants on the same accelerator contend for one pool, which a
// per-variant feasibility check would miss entirely.
func TestDemandOfSumsSharedAccelerator(t *testing.T) {
	set := []Candidate{
		cand("d", domain.RoleDecode, "H100", 2, 1),
		cand("p", domain.RolePrefill, "H100", 3, 1),
	}
	demand := demandOf(set)
	if demand["H100"] != 5 {
		t.Fatalf("demand[H100] = %d, want 5 (2+3 on the same pool)", demand["H100"])
	}
}

// TestDemandOfFloorsMissingGPURequest mirrors GetTotalGPUsPerReplica's floor: a
// workload with no explicit GPU request still occupies one.
func TestDemandOfFloorsMissingGPURequest(t *testing.T) {
	demand := demandOf([]Candidate{cand("d", domain.RoleDecode, "H100", 0, 1)})
	if demand["H100"] != 1 {
		t.Fatalf("demand[H100] = %d, want 1", demand["H100"])
	}
}

// TestUnresolvedAcceleratorDoesNotBlockTheWake: accelerator resolution needs a
// nodeSelector or label the workload need not carry. No provider publishes a
// pool for a placeholder, so charging demand to one would deny every such
// variant and turn an annotation gap into "this model can never wake".
//
// Every placeholder the project recognises is covered, not just "". That matters
// because accelerator.GetAcceleratorNameFromScaleTarget never returns "" — it
// falls back to DefaultAcceleratorName — so a bare empty-string check would pass
// this test's first case while failing in production on every unresolved
// variant.
func TestUnresolvedAcceleratorDoesNotBlockTheWake(t *testing.T) {
	placeholders := map[string]string{
		"empty":                     "",
		"DefaultAcceleratorName":    constants.DefaultAcceleratorName,
		"UnresolvedAcceleratorType": constants.UnresolvedAcceleratorType,
	}

	for name, accel := range placeholders {
		t.Run(name, func(t *testing.T) {
			if demand := demandOf([]Candidate{cand("d", domain.RoleDecode, accel, 4, 1)}); len(demand) != 0 {
				t.Fatalf("demand = %v, want empty for accelerator %q", demand, accel)
			}

			in := SelectionInput{
				Namespace:  selNS,
				Candidates: []Candidate{cand("d", domain.RoleDecode, accel, 4, 1)},
				// A fully-committed cluster: a resolvable variant would be denied here.
				Constraints: pools(map[string]int{"H100": 0}),
			}
			set, outcome := selectServingSet(in)
			if outcome != OutcomeDecodeOnly || !hasAll(names(set), "d") {
				t.Fatalf("selected %v (%s), want the variant woken despite accelerator %q",
					names(set), outcome, accel)
			}
		})
	}
}

// TestResolvedAcceleratorIsStillCharged is the counter-case: skipping
// placeholders must not degrade into skipping the budget check altogether.
func TestResolvedAcceleratorIsStillCharged(t *testing.T) {
	in := SelectionInput{
		Namespace:   selNS,
		Candidates:  []Candidate{cand("d", domain.RoleDecode, "H100", 4, 1)},
		Constraints: pools(map[string]int{"H100": 0}),
	}
	if set, outcome := selectServingSet(in); outcome != OutcomeNoCapacity || len(set) != 0 {
		t.Fatalf("selected %v (%s), want no capacity for a resolved accelerator with an empty pool",
			names(set), outcome)
	}
}
