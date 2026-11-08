package saturation

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// ITL per accepted token (#274). Each test here holds SwitchITLPerToken: it
// fails with the switch off.

// Through Analyze, a vLLM variant that speculates fits its line from the time
// per token: the property case on seed 1, and its step-time figure for scale.
func TestITLPerToken_TheLineIsFittedPerAcceptedToken(t *testing.T) {
	for seed := uint64(1); seed <= 5; seed++ {
		require.NoError(t, checkITLSpeculativeOn(nil)(seed), "seed %d", seed)
	}
}

// The rows the window reads, by what the flags and the counter say.
func TestITLPerToken_AMissingCounterUnderSpeculativeFlagsWithholdsTheReading(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	row := func(known bool) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{PodName: "p", VariantName: "v", Ready: true, KvUsageInstant: 0.5,
			AvgITL: 0.03, AvgITLKnown: true, SpecAcceptedPerStep: 2, SpecAcceptedPerStepKnown: known}
	}
	for _, tc := range []struct {
		name    string
		params  *capacity.EngineParams
		counter bool
		itl     float64
		known   bool
	}{
		{"vLLM speculating, counter read: per token", specParams(), true, 0.01, true},
		{"vLLM speculating, counter missing: withheld", specParams(), false, 0, false},
		{"vLLM not speculating, no counter: one token a step", keyParams(256), false, 0.03, true},
		{"vLLM, counter read without flags: per token", keyParams(256), true, 0.01, true},
		{"no configuration read, counter read: per token", nil, true, 0.01, true},
		{"no configuration read, no counter: as read", nil, false, 0.03, true},
		{"SGLang speculating, no counter: already per token", specParamsSGLang(), false, 0.03, true},
		{"SGLang speculating, counter read: left as read", specParamsSGLang(), true, 0.03, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := []domain.ReplicaMetrics{row(tc.counter)}
			got, _ := a.itlRows(in, "v", tc.params)
			require.Len(t, got, 1)
			assert.InDelta(t, tc.itl, got[0].AvgITL, 1e-12)
			assert.Equal(t, tc.known, got[0].AvgITLKnown)
			assert.InDelta(t, 0.03, in[0].AvgITL, 0, "the cycle's input is not edited")
		})
	}

	// Another variant's rows are left as they are, and an idle replica stays
	// idle.
	other := row(true)
	other.VariantName = "w"
	idle := domain.ReplicaMetrics{PodName: "q", VariantName: "v", Ready: true, AvgITLEmpty: true}
	got, _ := a.itlRows([]domain.ReplicaMetrics{other, idle}, "v", specParams())
	assert.InDelta(t, 0.03, got[0].AvgITL, 0)
	assert.Equal(t, domain.ITLEmpty, got[1].ITLState())
}

// A line shared under a speculating configuration is lent only in the unit
// the borrower reads.
func TestITLPerToken_AStepTimeLineIsNotLentToASpeculatingVariant(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	line := itl.Model{A: 0.05, B: 0.01}
	a.itlLines["phys"] = learnedLine{model: line, fromVariant: "owner", learnedAt: time.Now()}
	_, _, ok := a.borrowLine("phys", "borrower", true)
	assert.False(t, ok, "a step-time line priced a speculating borrower's token rate low")
	got, _, ok := a.borrowLine("phys", "borrower", false)
	assert.True(t, ok, "elsewhere a step is a token")
	assert.Equal(t, line, got)

	a.publishLineFor("phys", "owner", line, time.Now(), true)
	_, _, ok = a.borrowLine("phys", "borrower", true)
	assert.True(t, ok, "a line fitted per token is lent per token")
}

// A snapshot written before the correction (no unit mark) holds a speculating
// variant's ITL baseline and slope in step time: neither is restored. The
// same variant's baseline from a document that marks it per token is.
func TestITLPerToken_AStepTimeSnapshotIsNotRestoredForASpeculatingVariant(t *testing.T) {
	now := time.Now()
	restore := func(params *capacity.EngineParams, perToken bool) (*SaturationAnalyzer, RestoredVariant, string) {
		a, _, _ := waProcess(params)
		key := a.itlWindowKey("test-ns", "test-model", "variant-a", "H100", 1)
		doc := LearnedState{
			Config:        map[string]string{"test-ns/variant-a": params.Fingerprint()},
			Args:          map[string]string{"test-ns/variant-a": params.ArgsFingerprint()},
			VariantSeenAt: map[string]time.Time{key: now},
			ITLBaseline:   map[string]float64{key: 0.012},
			ITLSlopes:     map[string]SlopeSnapshot{key: {A: 0.05, FittedAt: now.Add(-time.Hour), Args: params.ArgsFingerprint()}},
			// A baseline put aside under arguments the variant ran before.
			ITLBaselineStash: map[string]float64{stashKey(key, "older-args"): 0.02},
		}
		if perToken {
			doc.ITLPerToken = map[string]bool{"test-ns/variant-a": true}
		}
		a.RestoreLearnedState(doc)
		r, done := a.installRestored("test-ns", "test-model", "variant-a")
		require.True(t, done, "precondition: the variant's entries were pending")
		require.Equal(t, RestoredWhole, r.Outcome, "precondition: the configuration matched")
		return a, r, key
	}

	a, r, key := restore(specParams(), false)
	_, ok := a.itlTracker.Baseline(key)
	assert.False(t, ok, "a step-time baseline restored for a speculating variant")
	assert.NotContains(t, a.itlBaselineStash, stashKey(key, "older-args"), "a step-time put-aside baseline restored for a speculating variant")
	_, ok = a.itlTracker.Slopes()[key]
	assert.False(t, ok, "a step-time slope restored for a speculating variant")
	assert.True(t, r.ITLOtherUnit)

	a, r, key = restore(specParams(), true)
	b, ok := a.itlTracker.Baseline(key)
	assert.True(t, ok && b == 0.012, "a baseline marked per token is restored")
	assert.Contains(t, a.itlBaselineStash, stashKey(key, "older-args"), "a put-aside baseline marked per token is restored")
	assert.False(t, r.ITLOtherUnit)

	a, r, key = restore(keyParams(256), false)
	_, ok = a.itlTracker.Baseline(key)
	assert.True(t, ok, "a variant that does not speculate restores as before")
	assert.False(t, r.ITLOtherUnit)
}

// The export marks a speculating variant's ITL entries and lines per token,
// and nothing else: a document from a fleet that does not speculate is the
// bytes it was.
func TestITLPerToken_TheExportMarksOnlySpeculatingVariants(t *testing.T) {
	for _, tc := range []struct {
		name   string
		params *capacity.EngineParams
		want   bool
	}{
		{"speculating", specParams(), true},
		{"not speculating", keyParams(256), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, _ := waProcess(tc.params)
			key := a.itlWindowKey("test-ns", "test-model", "variant-a", "H100", 1)
			a.itlTracker.SetBaseline(key, 0.01)
			a.mu.Lock()
			a.variantSeenAt[key] = time.Now()
			a.mu.Unlock()
			a.publishLineFor("phys", key, itl.Model{A: 0.05, B: 0.01}, time.Now(), tc.params.SpeculatesPerStep())
			s := a.ExportLearnedState()
			assert.Equal(t, tc.want, s.ITLPerToken["test-ns/variant-a"])
			assert.Equal(t, tc.want, s.ITLLinesPerToken["phys"])

			raw, err := json.Marshal(s)
			require.NoError(t, err)
			var doc LearnedState
			require.NoError(t, json.Unmarshal(raw, &doc))
			b, _, _ := waProcess(tc.params)
			b.RestoreLearnedState(doc)
			_, done := b.installRestored("test-ns", "test-model", "variant-a")
			require.True(t, done)
			_, ok := b.itlTracker.Baseline(key)
			assert.True(t, ok, "the variant's own export restores")
			_, _, ok = b.borrowLine("phys", "other", tc.params.SpeculatesPerStep())
			assert.True(t, ok, "its line is lent after the restart")
		})
	}
}

// A variant that does not speculate keeps its pending restored ITL entries in
// the export: one step is one token there, so their unit cannot differ, and
// dropping them lost its held slope and baseline across a second restart
// (review of #274, SHOULD-1).
func TestITLPerToken_APendingNonSpeculatingVariantIsExported(t *testing.T) {
	params := keyParams(256)
	a, _, _ := waProcess(params)
	key := a.itlWindowKey("test-ns", "test-model", "variant-a", "H100", 1)
	now := time.Now()
	a.mu.Lock()
	a.variantSeenAt[key] = now
	a.mu.Unlock()
	a.RestoreLearnedState(LearnedState{
		Config:        map[string]string{"test-ns/variant-a": params.Fingerprint()},
		Args:          map[string]string{"test-ns/variant-a": params.ArgsFingerprint()},
		VariantSeenAt: map[string]time.Time{key: now},
		ITLBaseline:   map[string]float64{key: 0.012},
		ITLSlopes:     map[string]SlopeSnapshot{key: {A: 0.05, FittedAt: now.Add(-time.Hour), Args: params.ArgsFingerprint()}},
	})
	a.mu.Lock()
	_, pending := a.pendingRestore["test-ns/variant-a"]
	a.mu.Unlock()
	require.True(t, pending, "precondition: the entries are pending")
	s := a.ExportLearnedState()
	assert.Contains(t, s.ITLBaseline, key, "pending ITL baseline of a non-speculating variant dropped from the export")
	assert.Contains(t, s.ITLSlopes, key, "pending ITL slope of a non-speculating variant dropped from the export")
}

// An accepted count above num_speculative_tokens is not one a step can
// produce: the reading is withheld rather than over-stating the token rate.
// At the stated length it is corrected. Both spellings of the flag.
func TestITLPerToken_AnAcceptedCountAboveTheDraftLengthIsWithheld(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	dotted := keyParams(256)
	dotted.SpeculativeConfig = "config.method=mtp;config.num_speculative_tokens=3"
	for name, params := range map[string]*capacity.EngineParams{"json": specParams(), "dotted": dotted} {
		require.Equal(t, 3, params.SpeculativeTokens(), name)
		for _, tc := range []struct {
			accepted float64
			itl      float64
			known    bool
			overMax  int
		}{
			{3, 0.01, true, 0},
			{3.5, 0, false, 1},
		} {
			row := domain.ReplicaMetrics{PodName: "p", VariantName: "v", Ready: true, KvUsageInstant: 0.5,
				AvgITL: 0.04, AvgITLKnown: true, SpecAcceptedPerStep: tc.accepted, SpecAcceptedPerStepKnown: true}
			got, withheld := a.itlRows([]domain.ReplicaMetrics{row}, "v", params)
			assert.InDelta(t, tc.itl, got[0].AvgITL, 1e-12, "%s, E %v", name, tc.accepted)
			assert.Equal(t, tc.known, got[0].AvgITLKnown, "%s, E %v", name, tc.accepted)
			assert.Equal(t, tc.overMax, withheld.overMax, "%s, E %v", name, tc.accepted)
		}
	}
}

// A withheld reading is logged once when it starts and once when it ends,
// not every cycle, and names why (review of #274, SHOULD-3).
func TestITLPerToken_AWithheldReadingIsLoggedOncePerChange(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	var lines []string
	logger := funcr.New(func(prefix, args string) { lines = append(lines, prefix+" "+args) }, funcr.Options{Verbosity: 2})
	row := domain.ReplicaMetrics{PodName: "p", VariantName: "v", Ready: true, KvUsageInstant: 0.5,
		AvgITL: 0.04, AvgITLKnown: true}
	cycle := func(rows []domain.ReplicaMetrics) {
		_, w := a.itlRows(rows, "v", specParams())
		a.noteITLWithheld("k", "v", w, logger)
	}
	idle := domain.ReplicaMetrics{PodName: "p", VariantName: "v", Ready: true, AvgITLEmpty: true}
	for range 3 {
		cycle([]domain.ReplicaMetrics{row})
		cycle([]domain.ReplicaMetrics{idle})
		cycle(nil)
	}
	require.Len(t, lines, 1, "logged once over three busy cycles, with idle cycles between them: %v", lines)
	assert.Contains(t, lines[0], `"itl-withheld"`)
	assert.Contains(t, lines[0], "no vllm:spec_decode_num_accepted_tokens_total")
	row.SpecAcceptedPerStep, row.SpecAcceptedPerStepKnown = 1, true
	cycle([]domain.ReplicaMetrics{row})
	cycle([]domain.ReplicaMetrics{row})
	require.Len(t, lines, 2, "logged once more when it ends: %v", lines)
	assert.Contains(t, lines[1], `"itl-withheld-ended"`)
}
