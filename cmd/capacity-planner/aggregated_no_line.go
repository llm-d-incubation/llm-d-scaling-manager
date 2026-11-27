package main

import (
	"fmt"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// aggregatedNoLineHint is what noLineHint says of an aggregated variant
// (pods serving both roles) that learned no line, where its pods' KV fill
// never reached the least fill a reading of the line may be: an aggregated
// pod with a large KV cache can serve a whole day at a few percent fill, and
// a busier window of the same fleet learns no more (issue
// llm-d-incubation/llm-d-scaling-manager#512). It names the highest steady
// fill read. Empty for a variant that is not aggregated, or one whose fill
// did reach it: the spread is then what the line lacks, as on P/D.
func (pv pricedVariant) aggregatedNoLineHint() string {
	if domain.CanonicalRole(pv.state.Role) != domain.RoleBoth {
		return ""
	}
	lo := itl.DefaultMinObservableK
	f := pv.state.MaxFill
	if !(f > 0) || f >= lo {
		return ""
	}
	return fmt.Sprintf("; this aggregated variant's KV fill never reached the band's bottom: the highest steady fill read was %.3f, "+
		"under %.2f, the least a reading of the line may be, so no line by fill can be fitted", f, lo)
}
