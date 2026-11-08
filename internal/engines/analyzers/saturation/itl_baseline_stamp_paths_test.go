package saturation

import (
	"encoding/json"
	"maps"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// The stamp on the paths it takes: export, a pending variant, dropVariant,
// the clock-skew rule and the switch (SwitchITLBaselineStamped).

// baselineAlone is doc with the held slopes taken out, so only the
// baseline dates the restored key.
func baselineAlone(t *testing.T, doc LearnedState) (LearnedState, string, time.Time) {
	t.Helper()
	require.Len(t, doc.ITLBaseline, 1, "premise: the line's full fit set a baseline")
	var key string
	for k := range doc.ITLBaseline {
		key = k
	}
	doc.ITLSlopes = nil
	return doc, key, doc.ITLBaselineAdmittedAt[key]
}

// A baseline restored alone, with no reading and no slope, reports the stamp
// it was written with: the time of its last full fit, not zero and not the
// restart.
func TestITLBaselineStamp_ARestoredBaselineReportsItsStamp(t *testing.T) {
	r, first := newBaselineStampRig(t)
	doc, key, stamp := baselineAlone(t, exported(t, first))
	require.False(t, stamp.IsZero(), "the export carries the baseline's stamp")
	assert.False(t, stamp.After(r.fitEnd), "stamped at its last full fit")
	assert.True(t, r.clock.Sub(stamp) > itl.DefaultObservationMaxAge, "and older than the window")

	second := r.process()
	second.RestoreLearnedState(doc)
	r.clock = r.clock.Add(time.Minute)
	r.cycle(t, second, r.replica(0.06, 0.01)) // under the band: nothing kept
	require.True(t, second.itlTracker.Restored(key), "premise: the baseline came from the snapshot")
	_, held := second.itlTracker.Slopes()[key]
	require.False(t, held, "premise: no slope dates the key")
	rep := r.itlReport(t, second)
	require.Zero(t, rep.Samples, "premise: the window kept nothing")
	assert.True(t, stamp.Equal(rep.LastAdmitted), "the report publishes the persisted stamp: %v, want %v",
		rep.LastAdmitted, stamp)
}

// A v1 document written before the stamp loads, and its baseline is as old
// as can be: the report publishes zero, as every build before did.
func TestITLBaselineStamp_ADocumentWithoutTheFieldReportsUnknown(t *testing.T) {
	r, first := newBaselineStampRig(t)
	doc, key, _ := baselineAlone(t, exported(t, first))
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	var old map[string]any
	require.NoError(t, json.Unmarshal(raw, &old))
	require.Contains(t, old, "itlBaselineAdmittedAt", "premise: the field's JSON name")
	delete(old, "itlBaselineAdmittedAt")
	raw, err = json.Marshal(old)
	require.NoError(t, err)
	var v1 LearnedState
	require.NoError(t, json.Unmarshal(raw, &v1))

	second := r.process()
	second.RestoreLearnedState(v1)
	r.clock = r.clock.Add(time.Minute)
	r.cycle(t, second, r.replica(0.06, 0.01))
	b, ok := second.itlTracker.Baseline(key)
	require.True(t, ok, "the baseline still loads")
	assert.Equal(t, doc.ITLBaseline[key], b)
	assert.True(t, r.itlReport(t, second).LastAdmitted.IsZero(), "an unknown age publishes zero")
	assert.Empty(t, exported(t, second).ITLBaselineAdmittedAt, "and no stamp is invented for it")
}

// Export, import and export again: the stamp goes out as it came in, beside
// its baseline, and a restore does not make it younger.
func TestITLBaselineStamp_RoundTrips(t *testing.T) {
	r, first := newBaselineStampRig(t)
	doc := exported(t, first)
	require.NotEmpty(t, doc.ITLBaselineAdmittedAt)

	second := r.process()
	second.RestoreLearnedState(doc)
	r.clock = r.clock.Add(time.Minute)
	r.cycle(t, second, r.replica(0.06, 0.01))
	again := exported(t, second)
	assert.Equal(t, doc.ITLBaseline, again.ITLBaseline)
	assert.Equal(t, doc.ITLBaselineAdmittedAt, again.ITLBaselineAdmittedAt)
}

// A pending variant's baseline goes out with its stamp before it is
// installed, and dropVariant takes the stamp with the baseline.
func TestITLBaselineStamp_PendingAndDroppedWithTheBaseline(t *testing.T) {
	r, first := newBaselineStampRig(t)
	doc := exported(t, first)
	require.NotEmpty(t, doc.ITLBaselineAdmittedAt)

	// Restored, not yet analysed: every entry is pending.
	second := r.process()
	second.RestoreLearnedState(doc)
	assert.Equal(t, doc.ITLBaselineAdmittedAt, exported(t, second).ITLBaselineAdmittedAt,
		"a pending baseline goes out with its stamp")

	dropped := doc
	dropped.ITLBaseline = maps.Clone(doc.ITLBaseline)
	dropped.ITLBaselineAdmittedAt = maps.Clone(doc.ITLBaselineAdmittedAt)
	dropVariant(&dropped, "test-ns/variant-a")
	assert.Empty(t, dropped.ITLBaseline, "premise: the baseline went")
	assert.Empty(t, dropped.ITLBaselineAdmittedAt, "the stamp went with it")
}

// Off, nothing is exported or installed, and a restored baseline publishes
// zero, as before the switch.
func TestITLBaselineStamp_TheSwitch(t *testing.T) {
	r, first := newBaselineStampRig(t)
	doc, key, stamp := baselineAlone(t, exported(t, first))
	require.False(t, stamp.IsZero())

	off := r.process()
	require.NoError(t, off.SetSwitchesForReplay(map[string]bool{SwitchITLBaselineStamped: false}))
	off.RestoreLearnedState(doc)
	r.clock = r.clock.Add(time.Minute)
	r.cycle(t, off, r.replica(0.06, 0.01))
	_, ok := off.itlTracker.Baseline(key)
	require.True(t, ok, "premise: the baseline is restored either way")
	assert.True(t, r.itlReport(t, off).LastAdmitted.IsZero(), "off: as old as can be")
	assert.Empty(t, exported(t, off).ITLBaselineAdmittedAt, "off: no stamp is exported")
	require.NoError(t, first.SetSwitchesForReplay(map[string]bool{SwitchITLBaselineStamped: false}))
	assert.Empty(t, exported(t, first).ITLBaselineAdmittedAt, "off: a live baseline's stamp is not exported either")
}

// A stamp ahead of now past the skew is not installed: the baseline is, of
// unknown age. One inside the skew is taken as now.
func TestITLBaselineStamp_AFutureStampIsNotInstalled(t *testing.T) {
	r, first := newBaselineStampRig(t)
	doc, key, _ := baselineAlone(t, exported(t, first))
	for name, c := range map[string]struct {
		ahead time.Duration
		want  func(now time.Time) time.Time
	}{
		"past the skew":   {maxClockSkew + time.Minute, func(time.Time) time.Time { return time.Time{} }},
		"inside the skew": {maxClockSkew / 2, func(now time.Time) time.Time { return now }},
	} {
		t.Run(name, func(t *testing.T) {
			d := doc
			d.ITLBaselineAdmittedAt = map[string]time.Time{key: r.clock.Add(c.ahead)}
			a := r.process()
			a.RestoreLearnedState(d)
			r.cycle(t, a, r.replica(0.06, 0.01)) // the same clock: installed now
			_, ok := a.itlTracker.Baseline(key)
			require.True(t, ok, "the baseline is installed")
			got := r.itlReport(t, a).LastAdmitted
			assert.True(t, c.want(a.now()).Equal(got), "got %v", got)
		})
	}
}
