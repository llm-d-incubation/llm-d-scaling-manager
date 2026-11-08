package steadystate

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// The size bound drops a spent lower bound holding no figure before a k2
// window: the bound only keeps a borrow from re-arming without new evidence,
// where the window dropped for it priced its bucket at derived or k1 when the
// traffic came back. Ordered by when its variant was last seen, which is
// every cycle, every spent bound outlived the windows of buckets served two
// days ago.
func TestTheSizeBoundDropsASpentBoundBeforeAWindow(t *testing.T) {
	now := time.Now()
	s := saturation.LearnedState{
		K2: map[string]capacity.RollingAverageSnapshot{
			"ns|v|m|H100|1|both|b0|q5": {Values: []float64{4000}, LastUsed: now.Add(-48 * time.Hour), LastWritten: now.Add(-72 * time.Hour)},
		},
		LowerBounds: map[string]saturation.LowerBoundSnapshot{
			// Spent, as the analyzer exports one: armed, its start a whole hold
			// and relaxation before its last tick.
			"ns|v|m|H100|1|both|b1|q5": {Armed: true, Since: now.Add(-(saturation.BorrowedK2Hold + saturation.BorrowedK2Relax)), Tick: now, Seen: now, Donor: "w", DonorK2: 3000, Offered: 3000, Args: "fp"},
			// Running: an hour into its hold and relaxation.
			"ns|v|m|H100|1|both|b3|q5": {Armed: true, Since: now.Add(-time.Hour), Tick: now, Seen: now, Donor: "w", DonorK2: 3000, Offered: 3000, Args: "fp"},
			"ns|v|m|H100|1|both|b2|q5": {Seen: now.Add(-24 * time.Hour), Prior: 3000, PriorSince: now.Add(-24 * time.Hour), PriorUsed: now.Add(-24 * time.Hour)},
		},
		PriorK2: map[string]saturation.PriorK2Snapshot{
			"ns|v|m|H100|1|both|b2|q5": {K2: 3000, InstalledAt: now.Add(-24 * time.Hour)},
		},
	}
	entries := learnedStateEntries(s)
	entries[0].drop()
	assert.NotContains(t, s.LowerBounds, "ns|v|m|H100|1|both|b1|q5", "the spent bound is the first to go")
	assert.Contains(t, s.K2, "ns|v|m|H100|1|both|b0|q5", "the window outlived a spent bound")
	assert.Contains(t, s.LowerBounds, "ns|v|m|H100|1|both|b3|q5", "a running bound is not a spent one")
	assert.Contains(t, s.LowerBounds, "ns|v|m|H100|1|both|b2|q5", "a bound holding a figure is ordered by when it was used")
	// A bound holding a figure goes with its rollback copy, one entry.
	for _, e := range entries[1:] {
		if _, held := s.LowerBounds["ns|v|m|H100|1|both|b2|q5"]; !held {
			break
		}
		e.drop()
	}
	assert.NotContains(t, s.PriorK2, "ns|v|m|H100|1|both|b2|q5", "the rollback copy outlived its bound")
}

// The clocks that move on every cycle a floor holds
// a fleet on a put-aside service rate, or a previous figure is used, are
// left out of the digest: kept in, each rewrote the document every minute.
func TestTheDigestIgnoresTheClocksThatMoveEveryCycle(t *testing.T) {
	now := time.Now()
	base := func() saturation.LearnedState {
		return saturation.LearnedState{
			LowerBounds: map[string]saturation.LowerBoundSnapshot{
				"k": {Seen: now, Prior: 3000, PriorSince: now, PriorUsed: now},
			},
			ThroughputStash: map[string]saturation.ThroughputSnapshot{
				"k#a": {RollingAverageSnapshot: capacity.RollingAverageSnapshot{Values: []float64{2.5}}, HeldSeconds: 60},
			},
			PriorK2: map[string]saturation.PriorK2Snapshot{"k": {K2: 3000, InstalledAt: now}},
		}
	}
	for name, move := range map[string]func(*saturation.LearnedState){
		"the floor held on a put-aside rate": func(s *saturation.LearnedState) {
			w := s.ThroughputStash["k#a"]
			w.HeldSeconds = 120
			s.ThroughputStash["k#a"] = w
		},
		"the previous figure was used": func(s *saturation.LearnedState) {
			s.PriorK2["k"] = saturation.PriorK2Snapshot{K2: 3000, InstalledAt: now.Add(time.Minute)}
		},
	} {
		moved := base()
		move(&moved)
		assert.Equal(t, learnedStateDigest(base()), learnedStateDigest(moved), name)
	}
}

// An ITL line a decode fleet refits every cycle is republished with a new
// learnedAt and its last digits moved: the same line, which must not rewrite
// the document every minute. A line that really moved still does.
func TestTheDigestIgnoresALineThatOnlyRefitted(t *testing.T) {
	now := time.Now()
	line := func(a, b float64, at time.Time) saturation.LearnedState {
		return saturation.LearnedState{ITLLines: map[string]saturation.LineSnapshot{
			"m|H200|1|fp": {A: a, B: b, FromVariant: "ns|m|v|H200|1", LearnedAt: at},
		}}
	}
	base := learnedStateDigest(line(0.016921, 0.0029311, now))
	assert.Equal(t, base, learnedStateDigest(line(0.016918, 0.0029307, now.Add(time.Minute))),
		"a refit that moved the line in its last digits rewrote the document")
	assert.NotEqual(t, base, learnedStateDigest(line(0.0189, 0.0029311, now)),
		"a line that moved by a tenth is a change and must be written")
}
