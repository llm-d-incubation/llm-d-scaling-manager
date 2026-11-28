package whatifapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// A figure carries its answer's kappa, its own copy, and none where the
// answer has none: the key is absent, not 0.
func TestFigureOf_Kappa(t *testing.T) {
	k := 6.25
	a := whatif.Answer{Value: 0.02, Basis: whatif.Basis{Kappa: &k}}
	f := figureOf(a, unitSeconds, true)
	if f.Kappa == nil || *f.Kappa != k {
		t.Fatalf("kappa %v, want %g", f.Kappa, k)
	}
	if f.Kappa == &k {
		t.Error("the figure shares the answer's kappa pointer")
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"kappa":6.25`) {
		t.Errorf("wire %s, want kappa 6.25", b)
	}
	b, err = json.Marshal(figureOf(whatif.Answer{Value: 3}, unitRate, false))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "kappa") {
		t.Errorf("wire %s: kappa on a figure with none", b)
	}
}
