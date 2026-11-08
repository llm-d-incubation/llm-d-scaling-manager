package learnreplay

import (
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle/settlerow"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
)

// The settle tracker in a replay (analyzer-learner.md, 4.2). The engine
// observes each model's rows at the top of its cycle and records the model
// and the instant (settle.KindObserve); the rows are the ones the model's
// next Analyze event records. The replay applies the observation as the
// engine did: at the recorded instant, with that Analyze's rows.
//
// A recording made before the engine recorded its observations has none.
// There the replay observes at each saturation Analyze's own clock: the
// cycle clock, which is the instant the engine observes at, at the default
// optimize interval. A cycle whose collection failed (settle.KindHold) is in
// no such recording, so the fallback cannot hold through one.
//
// The saturation analyzer's k2 reads the replay's tracker (its joined test),
// as it read the engine's. A recording is an engine's: one made by driving
// the analyzer alone gives it a tracker of the caller's, observed as the
// engine does, or its replay reads otherwise from the first cycle (the
// analyzer alone takes its first cycle's replicas as already watched,
// saturation.observeOwnSettle).
//
// A known gap: an observation applies only with the next saturation Analyze
// of its model. A cycle the engine observed and then did not analyze (an
// error between the observation and the Analyze) leaves its observation
// pending, and the next one replaces it, so the replay's tracker misses
// rows the engine's saw, and k2 reads what the replay's holds. None of the
// committed recordings has such a cycle.
//
// The engine also forgets, at the top of each cycle, the settle record of
// every model it does not reconcile that cycle (retainSettle, from
// optimizeV2's prune; retainSettle(nil) on a cycle with no active variant),
// and records no event for it. The replay derives it: every model the
// engine reconciles gets one observation or one hold a cycle, and every
// cycle starts with the slo analyzer's BeginCycle. So a model whose last
// observation or hold was before the previous cycle sat out a cycle, and
// its record is forgotten before its next observation or hold applies
// (retain). Only that model's own Analyze reads its record, after its
// observation, so forgetting it then is what forgetting it at the cycle
// that dropped it was. A cycle that follows failed ones begins twice
// (beginSLOCycle's catch-up), both on one clock reading; two BeginCycles
// under half an interval apart are one cycle (beginCycle). A recording
// with no observation forgets nothing: its engine had no retain. The
// engine always builds the slo analyzer (engine.go registers it as a
// built-in), so a recording with settle events and no slo analyzer was
// not made by it: with no BeginCycle to count cycles by, its replay
// forgets nothing, where Generate retains, and the replay says so (a
// mismatch, settleNoCycles).
//
// The replay counts the tracker's verdicts per reason (2.5), so a golden set
// that never reaches one says so.

// settlePathPrefix names the settle tracker's per-path counters: the prefix
// and the reason.
const settlePathPrefix = "settle-"

// settlePathRestored counts the restored sightings the tracker installed
// (settle.Tracker.Restore): not a verdict, since no settle began.
const settlePathRestored = settlePathPrefix + "restored"

// settlePathNames is the settle tracker's per-path counters, one per reason.
func settlePathNames() []string {
	out := make([]string, 0, len(settle.Reasons()))
	for _, r := range settle.Reasons() {
		out = append(out, settlePathPrefix+string(r))
	}
	return out
}

// requiredSettlePaths is the settle verdicts the goldens must keep
// reaching: a join, a warm-pool lend, and a restart. This sum over every
// golden is met by the in-process generators' restarts too; the real one,
// a decode engine restarted under its own Pod name on a cluster, is pinned
// per fixture by TestGoldens_PDRestartReplayRestartsForReal. The committed
// recordings reach a rejoin only through their synthetic generators (clocks
// that jump), so requiring it would pin the generators, not the tracker; a
// rollout under a Pod's own name is in no recording. The settle suite
// (internal/signals/settle) covers the rejoin and the rollout.
func requiredSettlePaths() []string {
	return []string{
		settlePathPrefix + string(settle.Joined),
		settlePathPrefix + string(settle.Lent),
		settlePathPrefix + string(settle.Restarted),
	}
}

// settleReplay is one replay's settle tracker, and the observations the
// recording has made that wait for their rows.
type settleReplay struct {
	tracker *settle.Tracker
	// pending is, per model, the instant of a recorded observation whose
	// Analyze has not come yet.
	pending map[string]time.Time
	// recorded is set once the recording has shown an observation: from
	// then on the replay observes only where the recording did.
	recorded bool

	// cycles is how many engine cycles have begun (beginCycle), and seen,
	// per model, the cycle of its last recorded observation or hold.
	cycles int
	seen   map[string]int
	// begunAt is when the current cycle began, and interval the engine's
	// optimize interval, as its observations last recorded it.
	begunAt  time.Time
	interval time.Duration
	// uncounted is set once a settle event has been applied in a replay
	// with no slo analyzer, whose cycles it cannot count.
	uncounted bool
}

func newSettleReplay() *settleReplay {
	t := settle.NewTracker()
	t.SetInterval(config.DefaultOptimizationInterval)
	return &settleReplay{tracker: t, pending: map[string]time.Time{}, seen: map[string]int{},
		interval: config.DefaultOptimizationInterval}
}

// beginCycle notes the slo analyzer's BeginCycle at now, the start of an
// engine cycle. One under half an interval after the cycle's first is the
// same cycle's: the engine counts a run of failed cycles as one more cycle
// by beginning the next one twice, on one clock reading, and starts each
// cycle an interval after the last one ended. Half an interval, not a
// whole one, because real cycles sit at the interval: a wall clock
// stepped back a few milliseconds would merge two of them. A merged one
// leaves begunAt at the cycle's first, so the window never slides.
func (s *settleReplay) beginCycle(now time.Time) {
	if s.cycles > 0 && now.Sub(s.begunAt) < s.interval/2 {
		return
	}
	s.cycles++
	s.begunAt = now
}

// retain forgets model's record when the engine did: when a cycle passed,
// since the model's last observation or hold, that neither observed nor
// held it, so the engine's prune did not keep it (settle.go, above). It
// then marks the model reconciled this cycle.
func (s *settleReplay) retain(model string) {
	if last, ok := s.seen[model]; ok && last < s.cycles-1 {
		s.tracker.Forget(model)
	}
	s.seen[model] = s.cycles
}

// setInterval takes the interval a recorded observation or hold carries.
func (s *settleReplay) setInterval(d time.Duration) {
	s.tracker.SetInterval(d)
	if d > 0 {
		s.interval = d
	}
}

// observe notes a recorded observation of a model at now.
func (s *settleReplay) observe(d settle.ObserveEvent, now time.Time) {
	s.recorded = true
	s.setInterval(d.Interval)
	model := utils.GetNamespacedKey(d.Namespace, d.ModelID)
	s.retain(model)
	s.pending[model] = now
}

// hold applies a recorded hold of a model at now.
func (s *settleReplay) hold(d settle.ObserveEvent, now time.Time) {
	s.recorded = true
	s.setInterval(d.Interval)
	model := utils.GetNamespacedKey(d.Namespace, d.ModelID)
	s.retain(model)
	s.tracker.Hold(model, now)
}

// analyze applies the observation waiting for a saturation Analyze's rows,
// or, in a recording with none, observes them at the Analyze's clock.
func (s *settleReplay) analyze(in domain.AnalyzerInput, now time.Time) {
	model := utils.GetNamespacedKey(in.Namespace, in.ModelID)
	at, ok := s.pending[model]
	switch {
	case ok:
		delete(s.pending, model)
	case s.recorded:
		return
	default:
		at = now
	}
	roles := make(map[string]string, len(in.VariantStates))
	for _, vs := range in.VariantStates {
		roles[vs.VariantName] = vs.Role
	}
	s.tracker.Observe(model, settlerow.Of(in.ReplicaMetrics, roles), at)
}

// pathCounts is the settle verdicts per reason, by settlePathNames.
func (s *settleReplay) pathCounts() map[string]int64 {
	out := map[string]int64{}
	for r, n := range s.tracker.Verdicts() {
		out[settlePathPrefix+string(r)] = n
	}
	// Only when reached: most recordings hold no restored sighting.
	if n := s.tracker.RestoredCount(); n > 0 {
		out[settlePathRestored] = n
	}
	return out
}
