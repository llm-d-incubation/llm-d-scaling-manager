package steadystate

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// historyMap reads the need history ConfigMap; nil when absent.
func historyMap(t *testing.T, c client.Client) *corev1.ConfigMap {
	t.Helper()
	var cm corev1.ConfigMap
	err := c.Get(context.Background(), client.ObjectKey{Namespace: config.SystemNamespace(), Name: shareHistoryConfigMap}, &cm)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return &cm
}

// observeEngine captures the engine's logs from here on.
func observeEngine(t *testing.T, se *shareEngine) *observer.ObservedLogs {
	t.Helper()
	var logs *observer.ObservedLogs
	se.ctx, logs = observe(t)
	return logs
}

// capped is whether the engine's single group had its headroom capped when it
// last planned, and whether it has planned since its ledger was built.
func (se *shareEngine) capped() (capped, planned bool) {
	for _, c := range se.e.utilizationShare.headroomCapped {
		return c, true
	}
	return false, false
}

// untilUncapped cycles until the planner, its peaks known, finds that they fit
// and lifts the cap.
func (se *shareEngine) untilUncapped() {
	se.t.Helper()
	for range 1000 {
		se.cycle()
		if c, planned := se.capped(); planned && !c {
			return
		}
	}
	se.t.Fatal("setup: the cap never came off; the fleet's peaks do not fit, so a restart cannot show anything")
}

// firstPlan cycles a newly built engine through its quiet period to its first
// planned cycle, and returns whether that cycle capped the headroom.
func (se *shareEngine) firstPlan() bool {
	se.t.Helper()
	for range 40 {
		se.cycle()
		if c, planned := se.capped(); planned {
			return c
		}
	}
	se.t.Fatal("no planned cycle after the quiet period")
	return false
}

// After a restart the ledger restores the need history the old controller
// stored, so the peaks it knew are known at once: the cap is not forced on for
// a window where the peaks fit.
func TestUtilizationShareHistorySurvivesARestart(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	se.untilUncapped()
	for range 4 { // past the write interval
		se.cycle()
	}
	cm := historyMap(t, c)
	if cm == nil || len(cm.Data) != 1 {
		t.Fatalf("history ConfigMap after a window of acting: %+v, want one group's entry", cm)
	}

	// Without the store a restart caps: the control that this fleet needs
	// the history at all.
	blind := newShareEngine(t, f, sharePods(t, f), se.clock.Add(time.Minute))
	if !blind.firstPlan() {
		t.Fatal("setup: a restart with no stored history did not cap; nothing to restore")
	}

	restarted := newShareEngine(t, f, c, se.clock.Add(time.Minute))
	logs := observeEngine(t, restarted)
	if restarted.firstPlan() {
		t.Fatal("restarted with the stored history, the first plan capped the headroom: the peaks were not restored")
	}
	if logs.FilterMessageSnippet("need history restored").Len() != 1 {
		t.Fatal("want the restore logged once")
	}
}

// A stored entry is used only when it is this format's version and was written
// for this group; a key is a valid ConfigMap data key whatever the group.
func TestUtilizationShareHistoryDecodeValidates(t *testing.T) {
	const group = "|A100"
	h := allocation.ShareNeedHistory{"ns/A/both": {Since: time.Unix(0, 0),
		Samples: []allocation.ShareNeedSample{{At: time.Unix(60, 0), Need: 3}}}}
	entry, _ := newShareHistoryEntry(group, h, allocation.ShareTimings{SwingWindow: time.Hour})
	raw, _, err := encodeShareHistory(entry, time.Unix(60, 0), shareHistoryMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	got, _, why := decodeShareHistory(raw, group)
	if why != "" || len(got) != 1 || got["ns/A/both"].Samples[0].Need != 3 || !got["ns/A/both"].Samples[0].At.Equal(time.Unix(60, 0)) {
		t.Fatalf("round trip: %+v (%s)", got, why)
	}
	if _, _, why := decodeShareHistory(raw, "ns|A100"); why == "" {
		t.Fatal("another group's entry was used")
	}
	if _, _, why := decodeShareHistory(strings.Replace(raw, `"version":1`, `"version":2`, 1), group); why == "" {
		t.Fatal("an entry of an unknown version was used")
	}
	if _, _, why := decodeShareHistory("{", group); why == "" {
		t.Fatal("an unparseable entry was used")
	}
	for _, g := range []string{"|A100", "team/a|NVIDIA H200", strings.Repeat("x", 400)} {
		if errs := validation.IsConfigMapKey(shareHistoryKey(g)); len(errs) != 0 {
			t.Fatalf("key for %q is not a valid data key: %v", g, errs)
		}
	}
	if shareHistoryKey("a|b") == shareHistoryKey("a_b") {
		t.Fatal("two groups share a key")
	}
}

// forbidHistory makes every read of the history ConfigMap Forbidden.
func forbidHistory(c client.Client) client.Client {
	return interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.ConfigMap); ok && key.Name == shareHistoryConfigMap {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, key.Name, nil)
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	})
}

// A Forbidden read is not an empty history: nothing is restored -- the ledger
// starts as it would with no store -- the failure is logged once, at Error,
// and the history the read could not see is not overwritten.
func TestUtilizationShareForbiddenHistoryIsNotEmpty(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	se.untilUncapped()
	for range 4 {
		se.cycle()
	}
	before := historyMap(t, c)
	if before == nil {
		t.Fatal("setup: no history stored")
	}

	restarted := newShareEngine(t, f, forbidHistory(c), se.clock.Add(time.Minute))
	logs := observeEngine(t, restarted)
	if !restarted.firstPlan() {
		t.Fatal("history restored through a Forbidden read")
	}
	for range 10 { // the read is retried every minute
		restarted.cycle()
	}
	failed := logs.FilterMessageSnippet("could not read the stored need history")
	if failed.Len() != 1 || failed.All()[0].Level.String() != "error" {
		t.Fatalf("Forbidden read logged %d times (%v), want once at error", failed.Len(), failed.All())
	}
	if after := historyMap(t, c); after.ResourceVersion != before.ResourceVersion {
		t.Fatal("the history was written over although it could not be read")
	}
}

// Deleting the ConfigMap is the flush: the running controller forgets its
// in-memory history too, rather than writing it straight back.
func TestUtilizationShareDeletingTheHistoryFlushesIt(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	se.untilUncapped()
	for range 4 {
		se.cycle()
	}
	cm := historyMap(t, c)
	if cm == nil {
		t.Fatal("setup: no history stored")
	}
	if err := c.Delete(context.Background(), cm); err != nil {
		t.Fatal(err)
	}
	logs := observeEngine(t, se)
	capped := false
	for range 6 {
		se.cycle()
		if c, _ := se.capped(); c {
			capped = true
		}
	}
	if logs.FilterMessageSnippet("need history ConfigMap was deleted").Len() != 1 {
		t.Fatal("want the flush logged once")
	}
	if !capped {
		t.Fatal("after the flush the peaks are still known: the cap stayed off")
	}
}

// A group dropped as vanished keeps its stored history: dropped after two
// minutes of failed collection, it comes back with its peaks. Deleted, the
// history would be lost to the outage it exists to survive.
func TestUtilizationShareVanishedGroupKeepsItsHistory(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	for range 10 {
		se.cycle()
	}
	if cm := historyMap(t, c); cm == nil || len(cm.Data) != 1 {
		t.Fatalf("setup: history %+v, want one entry", cm)
	}
	for range 10 { // the group's models are gone, past the absence grace
		se.clock = se.clock.Add(30 * time.Second)
		se.e.evaluateUtilizationShare(se.ctx, nil, fullQuota(), nil)
	}
	if cm := historyMap(t, c); cm == nil || len(cm.Data) != 1 {
		t.Fatalf("vanished group's history deleted: %+v", cm)
	}
	if len(se.e.utilizationShare.history.written) != 0 {
		t.Fatal("the controller still remembers what it wrote for a vanished group")
	}
}

// Deleting the ConfigMap flushes the history even when the first to find it
// gone is another group's new ledger reading it, not this group's write: the
// write that follows does not recreate it from memory.
func TestUtilizationShareFlushSeenByAnotherGroupsRestore(t *testing.T) {
	f := newShareFleet()
	c := sharePods(t, f)
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	se.untilUncapped()
	for range 4 {
		se.cycle()
	}
	cm := historyMap(t, c)
	if cm == nil {
		t.Fatal("setup: no history stored")
	}
	if err := c.Delete(context.Background(), cm); err != nil {
		t.Fatal(err)
	}
	logs := observeEngine(t, se)
	ctx := se.ctx
	se.e.restoreShareHistory(ctx, logr.FromContextOrDiscard(ctx), "otherns|H100", allocation.NewShareLedger(),
		se.clock, allocation.ShareTimings{SwingWindow: time.Hour})
	capped := false
	for range 6 {
		se.cycle()
		if c, _ := se.capped(); c {
			capped = true
		}
	}
	if logs.FilterMessageSnippet("need history ConfigMap was deleted").Len() != 1 {
		t.Fatal("want the flush logged once")
	}
	if !capped {
		t.Fatal("after the flush the peaks are still known: the history was written back from memory")
	}
}

func countHistory(c client.Client) (client.Client, *int, *int) {
	gets, writes := new(int), new(int)
	is := func(obj client.Object) bool {
		_, ok := obj.(*corev1.ConfigMap)
		return ok && obj.GetName() == shareHistoryConfigMap
	}
	return interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.ConfigMap); ok && key.Name == shareHistoryConfigMap {
				*gets++
			}
			return cl.Get(ctx, key, obj, opts...)
		},
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if is(obj) {
				*writes++
			}
			return cl.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if is(obj) {
				*writes++
			}
			return cl.Update(ctx, obj, opts...)
		},
	}), gets, writes
}

// historyLedger is a ledger with a window of need history for roles, role i
// at a need of i+1 (and so its peak), every 30s up to now.
func historyLedger(roles int, now time.Time, tm allocation.ShareTimings) *allocation.ShareLedger {
	l := allocation.NewShareLedger()
	for at := now.Add(-tm.SwingWindow); !at.After(now); at = at.Add(30 * time.Second) {
		needs := make(map[string]float64, roles)
		for i := range roles {
			// Digits past the fourth, as an analyzer's needs carry.
			needs[fmt.Sprintf("ns/model-%03d/decode", i)] = float64(i+1) + 0.123456789*float64(at.Unix()%7)
		}
		l.RecordNeeds(needs, at, tm)
	}
	return l
}

// historyEngine is an engine whose ledgers are given, for the store alone.
func historyEngine(t *testing.T, c client.Client, ledgers map[string]*allocation.ShareLedger) *Engine {
	t.Helper()
	e := &Engine{Config: shadowConfig(t, activeShare), client: c}
	e.utilizationShare.ledgers = ledgers
	return e
}

// Every group's history goes out in one write per cycle, and every ledger
// built in a cycle restores from one read: N groups do not cost N round trips
// to the API server, nor N chances to conflict with each other.
func TestUtilizationShareHistoryOneWritePerCycle(t *testing.T) {
	tm := allocation.ShareTimings{SwingWindow: time.Hour}
	now := time.Unix(1_760_000_000, 0)
	c, gets, writes := countHistory(sharePods(t, newShareFleet()))
	groups := []string{"|A100", "|H100", "team-a|A100"}
	ledgers := map[string]*allocation.ShareLedger{}
	for _, g := range groups {
		ledgers[g] = historyLedger(3, now, tm)
	}
	e := historyEngine(t, c, ledgers)
	ctx, logs := observe(t)
	logger := logr.FromContextOrDiscard(ctx)
	for _, g := range groups {
		e.restoreShareHistory(ctx, logger, g, allocation.NewShareLedger(), now, tm)
	}
	if *gets != 1 {
		t.Fatalf("%d groups' ledgers built in one cycle read the ConfigMap %d times, want once", len(groups), *gets)
	}
	for _, g := range groups {
		e.stageShareHistory(ctx, logger, g, tm, now)
	}
	e.writeShareHistory(ctx, logger, now)
	if *writes != 1 {
		t.Fatalf("%d groups' histories took %d writes, want one", len(groups), *writes)
	}
	if cm := historyMap(t, c); cm == nil || len(cm.Data) != len(groups) {
		t.Fatalf("history after one write: %+v, want an entry per group", cm)
	}
	if n := logs.FilterLevelExact(zapcore.ErrorLevel).Len(); n != 0 {
		t.Fatalf("%d errors logged: %v", n, logs.All())
	}
}

// One group with many roles cannot crowd the others out of the ConfigMap:
// each group written gets an equal share of the size bound, and a group over
// its share leaves out its smallest peaks -- logged -- not its whole entry.
func TestUtilizationShareHistoryGroupsShareTheBound(t *testing.T) {
	tm := allocation.ShareTimings{SwingWindow: 3 * time.Hour}
	now := time.Unix(1_760_000_000, 0)
	c := sharePods(t, newShareFleet())
	groups := []string{"|A100", "|H100", "|H200", "|L40S"}
	ledgers := map[string]*allocation.ShareLedger{}
	for _, g := range groups {
		ledgers[g] = historyLedger(shareHistoryMaxRoles, now, tm)
	}
	e := historyEngine(t, c, ledgers)
	ctx, logs := observe(t)
	logger := logr.FromContextOrDiscard(ctx)
	for _, g := range groups {
		e.stageShareHistory(ctx, logger, g, tm, now)
	}
	e.writeShareHistory(ctx, logger, now)
	if failed := logs.FilterMessageSnippet("could not store the need history"); failed.Len() != 0 {
		t.Fatalf("a group was refused: %v", failed.All()[0].ContextMap())
	}
	cm := historyMap(t, c)
	if cm == nil || len(cm.Data) != len(groups) {
		t.Fatalf("stored %d of %d groups", len(cm.Data), len(groups))
	}
	size := 0
	for k, v := range cm.Data {
		size += len(k) + len(v)
		if len(k)+len(v) > shareHistoryMaxBytes/len(groups) {
			t.Fatalf("entry %s is %d bytes, over its share %d", k, len(k)+len(v), shareHistoryMaxBytes/len(groups))
		}
	}
	if size > shareHistoryMaxBytes {
		t.Fatalf("ConfigMap data is %d bytes, over the bound", size)
	}
	for _, g := range groups {
		h, _, why := decodeShareHistory(cm.Data[shareHistoryKey(g)], g)
		if why != "" || len(h) == 0 || len(h) == shareHistoryMaxRoles {
			t.Fatalf("group %s stored %d roles (%s), want some but not all", g, len(h), why)
		}
		// The smallest peaks are left out: every stored role peaks higher
		// than every one left out.
		if _, ok := h[fmt.Sprintf("ns/model-%03d/decode", shareHistoryMaxRoles-1)]; !ok {
			t.Fatalf("group %s left out its highest peak", g)
		}
		if _, ok := h["ns/model-000/decode"]; ok {
			t.Fatalf("group %s kept its smallest peak over larger ones", g)
		}
	}
	if n := logs.FilterMessageSnippet("need history truncated").Len(); n != len(groups) {
		t.Fatalf("truncation logged %d times, want once per group", n)
	}
}

// Past shareHistoryMaxRoles, the roles left out are the smallest peaks, not
// the last alphabetically, and they are named.
func TestUtilizationShareHistoryDropsTheSmallestPeaks(t *testing.T) {
	tm := allocation.ShareTimings{SwingWindow: time.Hour}
	now := time.Unix(1_760_000_000, 0)
	h := allocation.ShareNeedHistory{}
	for i := range shareHistoryMaxRoles + 2 {
		// z-named roles peak highest, a-named ones lowest.
		name := fmt.Sprintf("%c-%03d", 'a'+i%26, i)
		h[name] = allocation.ShareRoleNeedHistory{Since: now.Add(-tm.SwingWindow),
			Samples: []allocation.ShareNeedSample{{At: now, Need: float64(i%26) + float64(i)/1000}}}
	}
	_, dropped := newShareHistoryEntry("|A100", h, tm)
	slices.Sort(dropped)
	if !slices.Equal(dropped, []string{"a-000", "a-026"}) {
		t.Fatalf("dropped %v, want the two smallest peaks a-000 and a-026", dropped)
	}
}

// A stored need keeps four significant digits, rounded up: never below what
// was recorded.
func TestUtilizationShareHistoryRoundsNeedsUp(t *testing.T) {
	for _, c := range []struct{ in, want float64 }{
		{0, 0}, {2, 2}, {1.23456789, 1.235}, {1.2341, 1.235}, {1.234, 1.234}, {1234567, 1.235e6}, {0.000123456, 0.0001235},
		{9.9999, 10},
	} {
		if got := shareHistoryRound(c.in); got != c.want {
			t.Fatalf("round(%v) = %v, want %v", c.in, got, c.want)
		}
	}
	if got := shareHistoryRound(0.1 + 0.2); got < 0.1+0.2 {
		t.Fatalf("round(0.1+0.2) = %v, below what was recorded", got)
	}
}

// A write removes what nothing will read: an entry that is not one at all, at
// once; one of another format version once a week old; one of this version
// once older than its window. A recent entry of another version is kept, for
// a controller rolled back to read.
func TestUtilizationShareHistoryPrunesWhatNothingReads(t *testing.T) {
	tm := allocation.ShareTimings{SwingWindow: time.Hour}
	now := time.Unix(1_760_000_000, 0)
	at := func(d time.Duration) string { return now.Add(-d).UTC().Format(time.RFC3339) }
	seed := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: shareHistoryConfigMap, Namespace: config.SystemNamespace()},
		Data: map[string]string{
			"junk":       "{",
			"no-age":     `{"version":2}`,
			"old-v2":     `{"version":2,"writtenAt":"` + at(8*24*time.Hour) + `"}`,
			"recent-v2":  `{"version":2,"writtenAt":"` + at(6*24*time.Hour) + `","roles":[1,2]}`,
			"expired-v1": `{"version":1,"writtenAt":"` + at(3*time.Hour) + `","swingWindow":"1h0m0s","roles":{}}`,
			"live-v1":    `{"version":1,"writtenAt":"` + at(30*time.Minute) + `","swingWindow":"1h0m0s","roles":{}}`,
		}}
	c := sharePods(t, newShareFleet())
	if err := c.Create(context.Background(), seed); err != nil {
		t.Fatal(err)
	}
	e := historyEngine(t, c, map[string]*allocation.ShareLedger{"|A100": historyLedger(2, now, tm)})
	ctx, _ := observe(t)
	logger := logr.FromContextOrDiscard(ctx)
	e.restoreShareHistory(ctx, logger, "|A100", allocation.NewShareLedger(), now, tm)
	e.stageShareHistory(ctx, logger, "|A100", tm, now)
	e.writeShareHistory(ctx, logger, now)
	cm := historyMap(t, c)
	for _, k := range []string{"junk", "no-age", "old-v2", "expired-v1"} {
		if _, ok := cm.Data[k]; ok {
			t.Fatalf("%s survived the write", k)
		}
	}
	for _, k := range []string{"recent-v2", "live-v1", shareHistoryKey("|A100")} {
		if _, ok := cm.Data[k]; !ok {
			t.Fatalf("%s was removed", k)
		}
	}
}

// A failure is reported per group: one group's failed read and another's
// success do not log a recovery, and the recovery names the group that failed.
func TestUtilizationShareHistoryErrorsArePerGroup(t *testing.T) {
	tm := allocation.ShareTimings{SwingWindow: time.Hour}
	now := time.Unix(1_760_000_000, 0)
	base := sharePods(t, newShareFleet())
	forbid := true
	c := interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.ConfigMap); ok && key.Name == shareHistoryConfigMap && forbid {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, key.Name, nil)
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	})
	e := historyEngine(t, c, map[string]*allocation.ShareLedger{})
	ctx, logs := observe(t)
	logger := logr.FromContextOrDiscard(ctx)
	e.restoreShareHistory(ctx, logger, "|A100", allocation.NewShareLedger(), now, tm)
	failed := logs.FilterMessageSnippet("could not read the stored need history")
	if failed.Len() != 1 || failed.All()[0].ContextMap()["group"] != "|A100" {
		t.Fatalf("read failure logged as %v, want once with its group", failed.All())
	}
	forbid = false
	now = now.Add(time.Minute)
	e.restoreShareHistory(ctx, logger, "|H100", allocation.NewShareLedger(), now, tm)
	if logs.FilterMessageSnippet("works again").Len() != 0 {
		t.Fatal("another group's successful read was logged as the failed group's recovery")
	}
	now = now.Add(time.Minute)
	e.restoreShareHistory(ctx, logger, "|A100", allocation.NewShareLedger(), now, tm)
	again := logs.FilterMessageSnippet("works again")
	if again.Len() != 1 || again.All()[0].ContextMap()["group"] != "|A100" {
		t.Fatalf("recovery logged as %v, want once for the group that failed", again.All())
	}
}

// The restore line says what the planner will use: how many roles' peaks are
// known now and from when; an entry nothing survives in is "not used".
func TestUtilizationShareHistoryRestoreLogIsHonest(t *testing.T) {
	tm := allocation.ShareTimings{SwingWindow: time.Hour}
	now := time.Unix(1_760_000_000, 0)
	c := sharePods(t, newShareFleet())
	e := historyEngine(t, c, map[string]*allocation.ShareLedger{"|A100": historyLedger(2, now, tm)})
	ctx, logs := observe(t)
	logger := logr.FromContextOrDiscard(ctx)
	e.restoreShareHistory(ctx, logger, "|A100", allocation.NewShareLedger(), now, tm)
	e.stageShareHistory(ctx, logger, "|A100", tm, now)
	e.writeShareHistory(ctx, logger, now)

	restart := now.Add(2 * time.Minute)
	e2 := historyEngine(t, c, map[string]*allocation.ShareLedger{})
	e2.restoreShareHistory(ctx, logger, "|A100", allocation.NewShareLedger(), restart, tm)
	restored := logs.FilterMessageSnippet("need history restored")
	if restored.Len() != 1 {
		t.Fatalf("restore logged %d times, want once", restored.Len())
	}
	kv := restored.All()[0].ContextMap()
	if kv["rolesKnownNow"] != int64(2) || kv["peaksKnownAt"] == nil {
		t.Fatalf("restore line %v, want rolesKnownNow=2 and peaksKnownAt", kv)
	}
	if _, ok := kv["peaksKnownFrom"]; ok {
		t.Fatal("the restore line still carries peaksKnownFrom")
	}

	late := now.Add(2 * tm.SwingWindow)
	e3 := historyEngine(t, c, map[string]*allocation.ShareLedger{})
	e3.restoreShareHistory(ctx, logger, "|A100", allocation.NewShareLedger(), late, tm)
	notUsed := logs.FilterMessageSnippet("stored need history not used")
	if notUsed.Len() != 1 || notUsed.All()[0].ContextMap()["reason"] != "older than its window" {
		t.Fatalf("an entry older than its window logged %v, want not used, older than its window", notUsed.All())
	}
	if logs.FilterMessageSnippet("need history restored").Len() != 1 {
		t.Fatal("an entry nothing survived in was logged as restored")
	}
}

// An entry kept over a shorter window than today's makes no peak known: its
// samples say nothing of the rest of today's window, whatever its Since says.
func TestUtilizationShareHistoryShorterWindowIsNotKnown(t *testing.T) {
	now := time.Unix(1_760_000_000, 0)
	long := allocation.ShareTimings{SwingWindow: 96 * time.Minute}
	short := allocation.ShareTimings{SwingWindow: 30 * time.Minute}
	h := allocation.ShareNeedHistory{"ns/A/decode": {Since: now.Add(-3 * time.Hour)}}
	rh := h["ns/A/decode"]
	for at := now.Add(-95 * time.Minute); !at.After(now); at = at.Add(time.Minute) {
		rh.Samples = append(rh.Samples, allocation.ShareNeedSample{At: at, Need: 2})
	}
	h["ns/A/decode"] = rh
	entry, _ := newShareHistoryEntry("|A100", h, short)
	raw, _, err := encodeShareHistory(entry, now, shareHistoryMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	c := sharePods(t, newShareFleet())
	if err := c.Create(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: shareHistoryConfigMap, Namespace: config.SystemNamespace()},
		Data:       map[string]string{shareHistoryKey("|A100"): raw}}); err != nil {
		t.Fatal(err)
	}
	e := historyEngine(t, c, map[string]*allocation.ShareLedger{})
	ctx, _ := observe(t)
	l := allocation.NewShareLedger()
	e.restoreShareHistory(ctx, logr.FromContextOrDiscard(ctx), "|A100", l, now, long)
	if l.PeakKnown("ns/A/decode", now, long) {
		t.Fatal("an entry kept over a 30m window made a peak known over 96m")
	}
	if got := l.PeakNeed("ns/A/decode", 0); got != 2 {
		t.Fatalf("restored peak %v, want the stored 2 kept", got)
	}
}
