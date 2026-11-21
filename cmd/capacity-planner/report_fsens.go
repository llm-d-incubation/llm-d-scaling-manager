package main

import (
	"fmt"
	"strings"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// sensitivityImbalances are the prefill imbalances f the needed search is
// repeated at (issue llm-d-incubation/llm-d-scaling-manager#300): an even
// spread, and whatif.ImbalanceFloor, the f assumed at an unlearned count
// with the curve off. A count the recording never ran at has no f of its
// own, and between the two the answer can move by a prefill pod: the third
// pod is worth nothing at f 1.5 when f was 1 at two.
var sensitivityImbalances = [...]float64{1, whatif.ImbalanceFloor}

// fSensitivity repeats the needed search with f fixed at each of
// sensitivityImbalances on every count above one prefill pod that P1 has not
// learned; a learned count keeps its own f (answerFlags.fixedImbalance).
// search is the needed search, which prices at r.fixedF.
func (r *reporter) fSensitivity(search func() (whatifapi.FleetCounts, string, error)) ([]whatifapi.ReportNeededAtF, error) {
	defer func() { r.fixedF = 0 }()
	out := make([]whatifapi.ReportNeededAtF, 0, len(sensitivityImbalances))
	for _, f := range sensitivityImbalances {
		r.fixedF = f
		at, unmet, err := search()
		if err != nil {
			return nil, err
		}
		row := whatifapi.ReportNeededAtF{F: f, Unmet: unmet}
		if unmet == "" {
			row.Prefill, row.Decode = at.Prefill, at.Decode
		}
		out = append(out, row)
	}
	return out, nil
}

// fSensitivityWords is the sensitivity in words, after the needed fleet n:
// "; the same at f 1 to 1.5" or "; at f 1 needs 3 + 3, at f 1.5 needs
// 4 + 3".
func fSensitivityWords(n whatifapi.FleetCounts, rows []whatifapi.ReportNeededAtF) string {
	if len(rows) == 0 {
		return ""
	}
	same := true
	for _, row := range rows {
		same = same && row.Unmet == "" && row.Prefill == n.Prefill && row.Decode == n.Decode
	}
	if same {
		return fmt.Sprintf("; the same at f %s to %s", num(rows[0].F), num(rows[len(rows)-1].F))
	}
	words := make([]string, len(rows))
	for i, row := range rows {
		if row.Unmet != "" {
			words[i] = fmt.Sprintf("at f %s no fleet holds", num(row.F))
			continue
		}
		words[i] = fmt.Sprintf("at f %s needs %d + %d", num(row.F), row.Prefill, row.Decode)
	}
	return "; " + strings.Join(words, ", ")
}
