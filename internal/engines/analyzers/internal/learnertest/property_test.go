package learnertest

import (
	"errors"
	"testing"
)

// The judgement's negative controls: an unledgered failing case, a ledgered
// case passing on a full run, a ledgered case failing with another kind, a
// ledger row for no case or missing a field, and an infrastructure error each
// break it; a ledgered case passing on a filtered seed does not.
func TestPropertyJudgementNegativeControls(t *testing.T) {
	c := Case{Class: ClassEdges, Learner: "x", Name: "y"}
	row := &LedgerRow{ID: c.ID(), Class: "edges", Learner: "x", Expect: KindFlipped, Finding: "f", Measured: "m", Owner: "open"}
	failing := CaseRun{Seeds: 2, Failures: []SeedFailure{{Seed: 1, V: &Violation{Kind: KindFlipped, Msg: "m"}}}}
	other := CaseRun{Seeds: 2, Failures: []SeedFailure{{Seed: 1, V: &Violation{Kind: KindNotLearned, Msg: "m"}}}}
	passing := CaseRun{Seeds: 2}
	for name, tc := range map[string]struct {
		row   *LedgerRow
		run   CaseRun
		full  bool
		fails bool
	}{
		"ledgered and failing with its kind":         {row, failing, true, false},
		"unledgered and passing":                     {nil, passing, true, false},
		"ledgered and passing on one filtered seed":  {row, passing, false, false},
		"unledgered and failing":                     {nil, failing, true, true},
		"ledgered and passing on every seed":         {row, passing, true, true},
		"ledgered and failing with another kind":     {row, other, true, true},
		"ledgered, another kind, on a filtered seed": {row, other, false, true},
	} {
		if got := len(Judge(c, tc.row, tc.run, tc.full)) > 0; got != tc.fails {
			t.Errorf("%s: judged failing %v, want %v", name, got, tc.fails)
		}
	}
	cases := []Case{c}
	for name, rows := range map[string][]LedgerRow{
		"a row naming no case":     {*row, {ID: "edges/x/gone", Class: "edges", Learner: "x", Expect: "k", Finding: "f", Measured: "m", Owner: "open"}},
		"a row with no expect":     {{ID: c.ID(), Class: "edges", Learner: "x", Finding: "f", Measured: "m", Owner: "open"}},
		"a row with no owner":      {{ID: c.ID(), Class: "edges", Learner: "x", Expect: "k", Finding: "f", Measured: "m"}},
		"a row a plan's PR owns":   {{ID: c.ID(), Class: "edges", Learner: "x", Expect: "k", Finding: "f", Measured: "m", Owner: "PR 3"}},
		"an owner with a tail":     {{ID: c.ID(), Class: "edges", Learner: "x", Expect: "k", Finding: "f", Measured: "m", Owner: "#495 soon"}},
		"a row on another learner": {{ID: c.ID(), Class: "edges", Learner: "z", Expect: "k", Finding: "f", Measured: "m", Owner: "open"}},
		"a row listed twice":       {*row, *row},
		"rows out of order":        {{ID: "z", Class: "edges", Learner: "x", Expect: "k", Finding: "f", Measured: "m", Owner: "open"}, *row},
	} {
		if len(CheckLedger(rows, cases)) == 0 {
			t.Errorf("%s: the ledger check passed", name)
		}
	}
	for _, owner := range []string{"open", "#495"} {
		ok := *row
		ok.Owner = owner
		if errs := CheckLedger([]LedgerRow{ok}, cases); len(errs) != 0 {
			t.Errorf("a matching ledger owned by %s is clean: %v", owner, errs)
		}
	}
	if _, err := RunCase(func(uint64) error { return errors.New("no rig") }, []uint64{1}); err == nil {
		t.Error("an infrastructure error was counted as a violation")
	}
}

// SeedsFor runs every seed of a case, and the one SeedEnv names alone.
func TestSeedsFor(t *testing.T) {
	seeds, full, err := SeedsFor(0)
	if err != nil || !full || len(seeds) != DefaultSeeds || seeds[0] != 1 {
		t.Errorf("zero seeds: %v %v %v, want 1..%d, full", seeds, full, err, DefaultSeeds)
	}
	t.Setenv(SeedEnv, "7")
	seeds, full, err = SeedsFor(CheapSeeds)
	if err != nil || full || len(seeds) != 1 || seeds[0] != 7 {
		t.Errorf("%s=7: %v %v %v, want seed 7 alone, not full", SeedEnv, seeds, full, err)
	}
}
