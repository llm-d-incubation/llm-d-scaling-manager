package steadystate

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
)

// The size trim once dropped a borrow clock with its state, but a previous
// figure was not an entry, so a trim could
// keep a prior whose clock it dropped; restored, that prior was unused again
// and armed at full hold. A prior and its clock are now one record
// (LowerBounds), so they stay or go together, and the trim must not reach the
// caller's maps.
func TestTheTrimKeepsAPriorWithItsClock(t *testing.T) {
	now := time.Now()
	key := "ns|v|m|H100|1|decode|b1|q5"
	s := saturation.LearnedState{
		LowerBounds: map[string]saturation.LowerBoundSnapshot{key: {
			Armed: true, Since: now.Add(-time.Hour), Tick: now, Seen: now,
			Donor: "v", Own: true, DonorK2: 4000, Args: "x",
			Prior: 4000, PriorSince: now.Add(-time.Hour),
		}},
		K2BorrowedSince: map[string]time.Time{key: now.Add(-time.Hour)},
		Config:          map[string]string{"ns/v": "fp"},
		Args:            map[string]string{"ns/v": "args"},
	}
	full, dropped, err := encodeLearnedState(s, nil, now, 1<<20)
	if err != nil || dropped != 0 {
		t.Fatalf("precondition: full encode, dropped=%d err=%v", dropped, err)
	}
	raw, dropped, err := encodeLearnedState(s, nil, now, len(full)-1)
	if err != nil {
		t.Fatalf("trimmed encode: %v", err)
	}
	if dropped == 0 {
		t.Fatal("precondition: the bound dropped an entry")
	}
	var doc learnedStateDoc
	if err := json.NewDecoder(strings.NewReader(raw)).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if lb, kept := doc.State.LowerBounds[key]; kept && (lb.Prior == 0 || !lb.Armed) {
		t.Errorf("trimmed apart: kept %+v", lb)
	}
	if _, start := doc.State.K2BorrowedSince[key]; start {
		if _, kept := doc.State.LowerBounds[key]; !kept {
			t.Error("the rollback start was kept without its bound")
		}
	}
	if len(s.LowerBounds) != 1 || len(s.K2BorrowedSince) != 1 {
		t.Errorf("the trim reached the caller's maps: bounds=%d starts=%d", len(s.LowerBounds), len(s.K2BorrowedSince))
	}
}

// Every map of LearnedState is either an entry the size bound can drop, or
// one of the per-variant configuration tags that are kept. A map added to the
// document and to neither list is caught here, rather than growing past the
// bound with nothing able to trim it -- which is how the prior got in.
func TestEveryLearnedStateMapIsTrimmable(t *testing.T) {
	// ITLPerToken is a per-variant tag like them: one bool, and losing it
	// only makes a speculating variant's ITL entries read as step time,
	// which the restore then leaves out.
	kept := map[string]bool{"Config": true, "Args": true, "ITLPerToken": true}
	var s saturation.LearnedState
	v := reflect.ValueOf(&s).Elem()
	for i := range v.NumField() {
		f := v.Field(i)
		if f.Kind() != reflect.Map {
			continue
		}
		m := reflect.MakeMap(f.Type())
		key := "ns|v|m|H100|1|decode|b1|q5"
		if v.Type().Field(i).Name == "Accelerators" || v.Type().Field(i).Name == "Forgotten" {
			key = "ns/v"
		}
		m.SetMapIndex(reflect.ValueOf(key), reflect.New(f.Type().Elem()).Elem())
		f.Set(m)
	}
	for _, e := range learnedStateEntries(s) {
		e.drop()
	}
	for i := range v.NumField() {
		f, name := v.Field(i), v.Type().Field(i).Name
		if f.Kind() != reflect.Map || kept[name] {
			continue
		}
		if f.Len() != 0 {
			t.Errorf("LearnedState.%s keeps an entry the size bound cannot drop", name)
		}
	}
}

// A line's unit mark left with no line (#274) is spare: the size bound drops
// it before any entry that prices something, and a line keeps its own mark.
func TestTheTrimDropsAnOrphanedLineUnitMarkFirst(t *testing.T) {
	now := time.Now()
	s := saturation.LearnedState{
		ITLLines: map[string]saturation.LineSnapshot{"line": {A: 0.05, B: 0.01, FromVariant: "ns|m|v|H100|1",
			LearnedAt: now.Add(-time.Hour)}},
		ITLLinesPerToken: map[string]bool{"line": true, "orphan": true},
	}
	full, dropped, err := encodeLearnedState(s, nil, now, 1<<20)
	if err != nil || dropped != 0 {
		t.Fatalf("precondition: full encode, dropped=%d err=%v", dropped, err)
	}
	raw, dropped, err := encodeLearnedState(s, nil, now, len(full)-1)
	if err != nil {
		t.Fatalf("trimmed encode: %v", err)
	}
	if dropped != 1 {
		t.Fatalf("one entry over the bound, %d dropped", dropped)
	}
	var doc learnedStateDoc
	if err := json.NewDecoder(strings.NewReader(raw)).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if doc.State.ITLLinesPerToken["orphan"] {
		t.Error("the orphaned mark was kept and something else dropped")
	}
	if _, kept := doc.State.ITLLines["line"]; !kept || !doc.State.ITLLinesPerToken["line"] {
		t.Errorf("the line or its mark was dropped before the orphan: %+v %+v", doc.State.ITLLines, doc.State.ITLLinesPerToken)
	}
}

// A line's shape left with no line (#275) is spare, as its unit mark is: the
// size bound drops it before any entry that prices something, and a line
// keeps its own shape.
func TestTheTrimDropsAnOrphanedLineShapeFirst(t *testing.T) {
	now := time.Now()
	s := saturation.LearnedState{
		ITLLines: map[string]saturation.LineSnapshot{"line": {A: 0.05, B: 0.01, FromVariant: "ns|m|v|H100|1",
			LearnedAt: now.Add(-time.Hour)}},
		ITLLineShapes: map[string]saturation.LineShapeSnapshot{"line": {G: 250}, "orphan": {G: 600}},
	}
	full, dropped, err := encodeLearnedState(s, nil, now, 1<<20)
	if err != nil || dropped != 0 {
		t.Fatalf("precondition: full encode, dropped=%d err=%v", dropped, err)
	}
	raw, dropped, err := encodeLearnedState(s, nil, now, len(full)-1)
	if err != nil {
		t.Fatalf("trimmed encode: %v", err)
	}
	if dropped != 1 {
		t.Fatalf("one entry over the bound, %d dropped", dropped)
	}
	var doc learnedStateDoc
	if err := json.NewDecoder(strings.NewReader(raw)).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if _, kept := doc.State.ITLLineShapes["orphan"]; kept {
		t.Error("the orphaned shape was kept and something else dropped")
	}
	if _, kept := doc.State.ITLLines["line"]; !kept || doc.State.ITLLineShapes["line"].G != 250 {
		t.Errorf("the line or its shape was dropped before the orphan: %+v %+v", doc.State.ITLLines, doc.State.ITLLineShapes)
	}
}
