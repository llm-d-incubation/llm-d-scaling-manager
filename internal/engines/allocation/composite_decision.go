package allocation

import (
	"math"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/aggregation"
)

// DecisionPath names how one variant's composite replica need was reached,
// carried in the composite's VariantCapacity.Reason and logged with the
// analyzer results, so a decision is traceable end to end.
//
// There is no path for a default PRC: saturation's P0-store ladder is the
// only source of estimated PRCs for an idle or never-seen variant, and its
// estimate contributes like any other. There is no saturation fallback
// either: saturation sizes a variant only when the policy lists it, and then
// it contributes like any other analyzer.
type DecisionPath string

const (
	// DecisionAgree means multiple contributors produced a defined N(SO);
	// the max was taken over more than one of them.
	DecisionAgree DecisionPath = "C0-agree"
	// DecisionSingle means exactly one analyzer contributed a defined N(SO).
	DecisionSingle DecisionPath = "C1-single"
	// DecisionNoSignal means no listed analyzer produced a defined N(SO) for
	// this SO. There is no usable signal, and this must not be treated as a
	// scale-to-zero decision; it is "no evidence", handled by the gate in
	// composite_signal_gate.go.
	DecisionNoSignal DecisionPath = "C4-no-signal"
)

// TotalReplicas returns D_i[role]/PRC_i(SO) — analyzer i's own measured demand
// for this SO's role, divided by analyzer i's own measured PRC for this SO.
// Both values are analyzer i's own data; never sat's, never the composite's.
// ok is false when vc.PerReplicaCapacity <= 0 or the analyzer has no demand
// entry for the role at all.
func TotalReplicas(result *domain.AnalyzerResult, vc domain.VariantCapacity) (n float64, ok bool) {
	if result == nil {
		return 0, false
	}
	if vc.PerReplicaCapacity <= 0 {
		return 0, false
	}
	demand, present := aggregation.DemandForRole(result, domain.RoleOfVC(vc))
	if !present {
		return 0, false
	}
	return demand / vc.PerReplicaCapacity, true
}

// Decision is how the composite reached one variant's replica need.
type Decision struct {
	// N is the replica need, in saturation's demand units; defined only when
	// OK.
	N  float64
	OK bool
	// Path names the rule that produced N.
	Path DecisionPath
	// Contributors names every analyzer whose need was considered.
	Contributors []string
	// Saturation is whether saturation is among the contributors, and
	// SaturationN its need when it is. Where saturation's need is N, the
	// composite keeps saturation's own PRC exactly rather than D / N, which
	// is not always bit-identical to it.
	Saturation  bool
	SaturationN float64
	// Others is the largest need among the contributors other than
	// saturation; defined only when OthersOK.
	Others   float64
	OthersOK bool
	// Needs is each contributor's need other than saturation's, in the order
	// the entries list them.
	Needs []AnalyzerNeed
}

// AnalyzerNeed is one analyzer's replica need for a variant.
type AnalyzerNeed struct {
	Analyzer string
	N        float64
}

// ResolveSO decides one variant's replica need from entries, the analyzers
// the policy lists. Each eligible one with a finite, non-negative
// TotalReplicas for the variant contributes, and N is the largest. A
// non-finite need is no reading: one analyzer's +Inf demand would otherwise
// price every replica at zero. sat only names saturation's entry, for
// Saturation and Others; saturation contributes only from entries.
func ResolveSO(entries []NamedAnalyzerResult, sat *NamedAnalyzerResult, variant string) Decision {
	var d Decision
	for _, e := range entries {
		if !Eligible(e) {
			continue
		}
		evc, present := FindVariantCapacity(e.Result, variant)
		if !present || evc.Reason == ReasonNoData || evc.Reason == ReasonError {
			continue
		}
		n, ok := finiteNeed(e.Result, evc)
		if !ok {
			continue
		}
		d.Contributors = append(d.Contributors, e.Name)
		if !d.OK || n > d.N {
			d.N, d.OK = n, true
		}
		if sat != nil && e.Name == sat.Name {
			d.Saturation, d.SaturationN = true, n
		} else {
			d.Needs = append(d.Needs, AnalyzerNeed{Analyzer: e.Name, N: n})
			if !d.OthersOK || n > d.Others {
				d.Others, d.OthersOK = n, true
			}
		}
	}
	switch len(d.Contributors) {
	case 0:
		d.Path = DecisionNoSignal
	case 1:
		d.Path = DecisionSingle
	default:
		d.Path = DecisionAgree
	}
	return d
}

// finiteNeed is TotalReplicas when it is finite and not negative.
func finiteNeed(result *domain.AnalyzerResult, vc domain.VariantCapacity) (float64, bool) {
	n, ok := TotalReplicas(result, vc)
	if !ok || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
		return 0, false
	}
	return n, true
}

// FindVariantCapacity returns variant's VariantCapacity from result, and
// whether it is present.
func FindVariantCapacity(result *domain.AnalyzerResult, variant string) (domain.VariantCapacity, bool) {
	if result == nil {
		return domain.VariantCapacity{}, false
	}
	for _, vc := range result.VariantCapacities {
		if vc.VariantName == variant {
			return vc, true
		}
	}
	return domain.VariantCapacity{}, false
}
