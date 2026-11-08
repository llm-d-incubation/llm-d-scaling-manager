package itl

import (
	"math"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle/settlerow"
)

// The admission's settle fields (settle.go), on the Tracker directly: the
// key's own trace (Settle nil) and the settle tracker's path. Each run is a
// controller start at t0 with three replicas at distinct fills, one cycle
// every 15 s, and reports the cycles on which a reading was kept out as its
// replica's settle.

const settleTestCycle = 15 * time.Second

var settleT0 = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// settleTestRow is a Ready replica of v at fill k, every gauge read.
func settleTestRow(pod string, k float64) domain.ReplicaMetrics {
	return domain.ReplicaMetrics{PodName: pod, VariantName: "v", Ready: true, ReadySince: settleT0.Add(-time.Hour),
		KvCacheUsage: k, KvCacheUsageKnown: true, KvUsageInstant: k, KvUsageInstantKnown: true,
		AvgITL: 0.008 + 0.04*k, AvgITLKnown: true, GenerationTokenRate: 1000, GenerationTokenRateKnown: true}
}

func settleTestFleet() []domain.ReplicaMetrics {
	return []domain.ReplicaMetrics{settleTestRow("p0", 0.30), settleTestRow("p1", 0.45), settleTestRow("p2", 0.60)}
}

// settleRun drives a Tracker for n cycles; with tracked, an engine settle
// tracker is observed before each and read (adm.Settle). at is each cycle's
// instant, rows its rows.
func settleRun(adm Admission, tracked bool, n int, at func(i int) time.Time,
	rows func(i int) []domain.ReplicaMetrics) []int {
	tr := NewTracker()
	st := settle.NewTracker()
	st.SetInterval(settleTestCycle)
	if tracked {
		adm.Settle = &Settle{View: st, Model: "ns/m"}
	}
	var out []int
	for i := range n {
		r := rows(i)
		st.Observe("ns/m", settlerow.Of(r, nil), at(i))
		if tr.Observe("k", "v", r, adm, at(i)).Settling > 0 {
			out = append(out, i)
		}
	}
	return out
}

func every(i int) time.Time { return settleT0.Add(time.Duration(i) * settleTestCycle) }

func fleetAlways(int) []domain.ReplicaMetrics { return settleTestFleet() }

// missing is the fleet with p2 missing on the cycles from..to-1.
func missing(from, to int) func(i int) []domain.ReplicaMetrics {
	return func(i int) []domain.ReplicaMetrics {
		f := settleTestFleet()
		if i >= from && i < to {
			return f[:2]
		}
		return f
	}
}

// run is the steadyFor cycles of a settle that starts on cycle from.
func run(from int) []int {
	out := make([]int, int(DefaultSteadyFor/settleTestCycle))
	for i := range out {
		out[i] = from + i
	}
	return out
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestAdmissionSettleFields(t *testing.T) {
	for _, tc := range []struct {
		name    string
		adm     Admission
		tracked bool
		at      func(i int) time.Time
		rows    func(i int) []domain.ReplicaMetrics
		n       int
		want    []int
	}{
		// The key's own trace.
		{name: "own trace: a replica on the key's first cycle is settled", rows: fleetAlways, n: 12, at: every},
		{name: "own trace: WaitsAfterRestart makes it wait steadyFor", adm: Admission{WaitsAfterRestart: true},
			rows: fleetAlways, n: 12, at: every, want: run(0)},
		{name: "own trace: one missed cycle restarts the settle without KeepsThroughMissingRow",
			rows: missing(10, 11), n: 25, at: every, want: run(11)},
		{name: "own trace: KeepsThroughMissingRow keeps it through one missed cycle",
			adm: Admission{KeepsThroughMissingRow: true}, rows: missing(12, 13), n: 25, at: every},
		{name: "own trace: KeepsThroughMissingRow restarts it past the absence tolerance (75 s > 1 min)",
			adm: Admission{KeepsThroughMissingRow: true}, rows: missing(10, 15), n: 30, at: every, want: run(15)},
		{name: "own trace: a longer AbsenceTolerance (2 min) keeps a 75 s absence",
			adm: Admission{KeepsThroughMissingRow: true, AbsenceTolerance: 2 * time.Minute}, rows: missing(10, 15), n: 30, at: every},
		{name: "own trace: a gap between the key's cycles is no absence",
			adm: Admission{KeepsThroughMissingRow: true}, rows: fleetAlways, n: 25,
			at: func(i int) time.Time {
				if i >= 10 {
					return every(i).Add(time.Hour)
				}
				return every(i)
			}},
		// The settle tracker.
		{name: "tracker: settled on the first cycle without the wait", tracked: true, rows: fleetAlways, n: 12, at: every},
		{name: "tracker: WaitsAfterRestart waits from the tracker's first sight", tracked: true,
			adm: Admission{WaitsAfterRestart: true}, rows: fleetAlways, n: 12, at: every, want: run(0)},
		{name: "tracker: a new Ready time under the name settles with RestartUnderName", tracked: true,
			adm: Admission{RestartUnderName: true}, rows: newReadyAt(10), n: 25, at: every, want: run(10)},
		{name: "tracker: and is missed without it", tracked: true, rows: newReadyAt(10), n: 25, at: every},
		{name: "tracker: a scrape that read nothing does not idle with IdleOnEvidence", tracked: true,
			adm: Admission{IdleOnEvidence: true}, rows: readNothing(10), n: 25, at: every},
		{name: "tracker: and does without it", tracked: true, rows: readNothing(10), n: 25, at: every, want: run(12)},
		{name: "tracker: a not-Ready row ageing out of the tracker's trace starts nothing", tracked: true,
			rows: notReadyAt(10), n: 50, at: every, want: run(11)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := settleRun(tc.adm, tc.tracked, tc.n, tc.at, tc.rows); !equalInts(got, tc.want) {
				t.Errorf("settling cycles %v, want %v", got, tc.want)
			}
		})
	}
}

// newReadyAt gives p2 a new Ready time from cycle at on.
func newReadyAt(at int) func(i int) []domain.ReplicaMetrics {
	return func(i int) []domain.ReplicaMetrics {
		f := settleTestFleet()
		if i >= at {
			f[2].ReadySince = every(at).Add(-5 * time.Second)
		}
		return f
	}
}

// readNothing makes p2's rows read nothing at all on cycles at and at+1.
func readNothing(at int) func(i int) []domain.ReplicaMetrics {
	return func(i int) []domain.ReplicaMetrics {
		f := settleTestFleet()
		if i == at || i == at+1 {
			f[2] = domain.ReplicaMetrics{PodName: "p2", VariantName: "v", Ready: true, ReadySince: f[2].ReadySince}
		}
		return f
	}
}

// notReadyAt makes p2 not Ready on cycle at.
func notReadyAt(at int) func(i int) []domain.ReplicaMetrics {
	return func(i int) []domain.ReplicaMetrics {
		f := settleTestFleet()
		if i == at {
			f[2].Ready = false
		}
		return f
	}
}

// A Pod with an Absent rank: FiniteOnly refuses it, and without it its
// Finite ranks' mean is a reading.
func TestAdmissionFiniteOnly(t *testing.T) {
	rm := settleTestRow("p0", 0.4)
	rm.AvgITLKnown = false
	for _, tc := range []struct {
		finite         bool
		offered, noITL int
	}{{true, 0, 1}, {false, 1, 0}} {
		r := NewTracker().Observe("k", "v", []domain.ReplicaMetrics{rm}, Admission{FiniteOnly: tc.finite}, settleT0)
		if r.Offered != tc.offered || r.NoITL != tc.noITL {
			t.Errorf("FiniteOnly %v: offered %d noITL %d, want %d and %d", tc.finite, r.Offered, r.NoITL, tc.offered, tc.noITL)
		}
	}
}

// FiniteOnly also reads the value: a row whose bits say Finite but whose
// AvgITL is 0, NaN or +Inf (a hand-built or replayed row; the collector
// never writes one) is no reading, as it was not before the bits.
func TestAdmissionFiniteOnlyReadsTheValueToo(t *testing.T) {
	for _, itl := range []float64{0, math.NaN(), math.Inf(1)} {
		rm := settleTestRow("p0", 0.4)
		rm.AvgITL = itl
		r := NewTracker().Observe("k", "v", []domain.ReplicaMetrics{rm}, Admission{FiniteOnly: true}, settleT0)
		if r.Offered != 0 || r.NoITL != 1 {
			t.Errorf("AvgITL %v with AvgITLKnown: offered %d noITL %d, want 0 and 1", itl, r.Offered, r.NoITL)
		}
	}
}

// A Pod that left the rotation and came back with the same Ready time is a
// restart as far as anything can tell (settlerow.ReadyAgain): its settle
// starts again. Here its not-Ready row reaches the tracker but not the line
// (the analyzer withheld the variant's rows that cycle), so only the
// tracker's trace says it happened.
func TestAdmissionReadyAgainOnTheTrackerStartsTheSettle(t *testing.T) {
	tr := NewTracker()
	st := settle.NewTracker()
	st.SetInterval(settleTestCycle)
	adm := Admission{Settle: &Settle{View: st, Model: "ns/m"}, KeepsThroughMissingRow: true}
	var got []int
	for i := range 25 {
		rows := settleTestFleet()
		lineRows := rows
		if i == 10 {
			rows[2].Ready = false
			lineRows = nil
		}
		st.Observe("ns/m", settlerow.Of(rows, nil), every(i))
		if tr.Observe("k", "v", lineRows, adm, every(i)).Settling > 0 {
			got = append(got, i)
		}
	}
	if want := run(11); !equalInts(got, want) {
		t.Errorf("settling cycles %v, want %v", got, want)
	}
}

// ForgetTraces drops only the matched keys' replica traces: their windows,
// intercepts and held slopes stay, and another key keeps its traces. The
// next Observe reads every replica as on the key's first sight: a replica
// the settle tracker saw join that cycle, which the key would have read as
// a joiner, settles only with WaitsAfterRestart.
func TestTracker_ForgetTracesKeepsEverythingButTheTraces(t *testing.T) {
	const joinAt = 12
	run := func(forget, waits bool) Reading {
		tr := NewTracker()
		st := settle.NewTracker()
		st.SetInterval(settleTestCycle)
		adm := Admission{Settle: &Settle{View: st, Model: "ns/m"}}
		var r Reading
		for i := range joinAt + 1 {
			rows := settleTestFleet()
			if i < joinAt {
				rows = rows[:2]
			}
			st.Observe("ns/m", settlerow.Of(rows, nil), every(i))
			tr.Observe("other", "v", rows, Admission{}, every(i))
			if i == joinAt && forget {
				before := tr.WindowLen("k")
				b, bOK := tr.Baseline("k")
				slope, slopeOK := tr.Slopes()["k"]
				if !slopeOK || before == 0 {
					t.Fatalf("premise: the key fitted before the forget (window %d, slope held %v)", before, slopeOK)
				}
				if n := tr.ForgetTraces(func(key string) bool { return key == "k" }); n != 1 {
					t.Fatalf("ForgetTraces dropped %d keys' traces, want 1", n)
				}
				if _, ok := tr.traces["k"]; ok {
					t.Error("the key's traces survived ForgetTraces")
				}
				if _, ok := tr.traces["other"]; !ok {
					t.Error("ForgetTraces dropped a key it did not match")
				}
				if got := tr.WindowLen("k"); got != before {
					t.Errorf("window %d after ForgetTraces, was %d", got, before)
				}
				if got, ok := tr.Baseline("k"); got != b || ok != bOK {
					t.Errorf("intercept %v (%v) after ForgetTraces, was %v (%v)", got, ok, b, bOK)
				}
				if got := tr.Slopes()["k"]; got != slope {
					t.Errorf("held slope %+v after ForgetTraces, was %+v", got, slope)
				}
				adm.WaitsAfterRestart = waits
			}
			r = tr.Observe("k", "v", rows, adm, every(i))
		}
		return r
	}
	if r := run(false, false); r.Settling == 0 {
		t.Error("premise: without the forget the joiner settles")
	}
	if r := run(true, false); r.Settling != 0 {
		t.Errorf("after ForgetTraces %d readings settled, want none: the key's first sight is no settle", r.Settling)
	}
	if r := run(true, true); r.Settling == 0 {
		t.Error("after ForgetTraces with WaitsAfterRestart the joiner did not settle")
	}
}
