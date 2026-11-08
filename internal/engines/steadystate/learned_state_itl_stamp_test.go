package steadystate

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
)

// The ITL baseline's last-admitted stamp (issue #495) is one entry with its
// baseline: the size bound drops it with the baseline's variant, a stamp
// left with no baseline goes first, and the digest leaves it out, as it does
// a slope's fit time.

const stampKey = "ns|m|v|H100|1"

// stampedState is one variant's baseline with its stamp, and its liveness.
func stampedState(now time.Time) saturation.LearnedState {
	return saturation.LearnedState{
		VariantSeenAt:         map[string]time.Time{stampKey: now.Add(-time.Hour)},
		ITLBaseline:           map[string]float64{stampKey: 0.01},
		ITLBaselineAdmittedAt: map[string]time.Time{stampKey: now.Add(-2 * time.Hour)},
	}
}

func decodeLearnedState(t *testing.T, raw string) saturation.LearnedState {
	t.Helper()
	var doc learnedStateDoc
	if err := json.NewDecoder(strings.NewReader(raw)).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	return doc.State
}

// Dropping the variant's liveness drops the baseline and its stamp together.
func TestLearnedState_TheTrimDropsTheBaselineStampWithItsBaseline(t *testing.T) {
	now := time.Now()
	s := stampedState(now)
	for _, e := range learnedStateEntries(s) {
		if e.key == "seen|"+stampKey {
			e.drop()
		}
	}
	if len(s.ITLBaseline) != 0 || len(s.ITLBaselineAdmittedAt) != 0 {
		t.Fatalf("the baseline or its stamp outlived the variant: %v %v", s.ITLBaseline, s.ITLBaselineAdmittedAt)
	}

	s = stampedState(now)
	full, dropped, err := encodeLearnedState(s, nil, now, 1<<20)
	if err != nil || dropped != 0 {
		t.Fatalf("precondition: full encode, dropped=%d err=%v", dropped, err)
	}
	if got := decodeLearnedState(t, full); !got.ITLBaselineAdmittedAt[stampKey].Equal(s.ITLBaselineAdmittedAt[stampKey]) {
		t.Fatalf("the stamp did not survive the encode: %v", got.ITLBaselineAdmittedAt)
	}
	raw, _, err := encodeLearnedState(s, nil, now, len(full)-1)
	if err != nil {
		t.Fatal(err)
	}
	// The variant's liveness is the only entry: over the bound, it goes, and
	// its baseline and stamp with it.
	got := decodeLearnedState(t, raw)
	if len(got.ITLBaseline) != 0 || len(got.ITLBaselineAdmittedAt) != 0 {
		t.Errorf("the trim kept a baseline or a stamp of a dropped variant: %v %v", got.ITLBaseline, got.ITLBaselineAdmittedAt)
	}
	if len(s.ITLBaselineAdmittedAt) != 1 {
		t.Errorf("the trim reached the caller's map: %v", s.ITLBaselineAdmittedAt)
	}
}

// A stamp left with no baseline prices nothing: the size bound drops it
// before anything that does, and a baseline keeps its own.
func TestLearnedState_TheTrimDropsAnOrphanedBaselineStampFirst(t *testing.T) {
	now := time.Now()
	s := stampedState(now)
	s.ITLBaselineAdmittedAt["orphan"] = now.Add(-time.Minute)
	full, dropped, err := encodeLearnedState(s, nil, now, 1<<20)
	if err != nil || dropped != 0 {
		t.Fatalf("precondition: full encode, dropped=%d err=%v", dropped, err)
	}
	raw, dropped, err := encodeLearnedState(s, nil, now, len(full)-1)
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 1 {
		t.Fatalf("one entry over the bound, %d dropped", dropped)
	}
	got := decodeLearnedState(t, raw)
	if _, kept := got.ITLBaselineAdmittedAt["orphan"]; kept {
		t.Error("the orphaned stamp was kept and something else dropped")
	}
	if _, b := got.ITLBaseline[stampKey]; !b || got.ITLBaselineAdmittedAt[stampKey].IsZero() {
		t.Errorf("the baseline or its stamp was dropped before the orphan: %v %v", got.ITLBaseline, got.ITLBaselineAdmittedAt)
	}
}

// A full fit restamps the baseline on every cycle it holds steady readings:
// the digest leaves the stamp out, so a refit that moves only it does not
// rewrite the document every minute.
func TestLearnedState_TheDigestIgnoresTheBaselineStamp(t *testing.T) {
	now := time.Now()
	base := stampedState(now)
	moved := stampedState(now)
	moved.ITLBaselineAdmittedAt[stampKey] = now
	if learnedStateDigest(base) != learnedStateDigest(moved) {
		t.Error("a moved stamp changed the digest")
	}
	moved.ITLBaseline[stampKey] = 0.02
	if learnedStateDigest(base) == learnedStateDigest(moved) {
		t.Error("a moved baseline did not change the digest")
	}
}
