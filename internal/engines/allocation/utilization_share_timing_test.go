package allocation

import (
	"math"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("DeriveShareTimings", func() {
	cycle := 30 * time.Second
	dur := func(d time.Duration) *time.Duration { return &d }

	It("uses the cluster's defaults when nothing is set", func() {
		tm, src := DeriveShareTimings(ShareTimingInputs{Cycle: cycle}, nil)
		bound := 300*time.Second + 15*time.Second + 30*time.Second + 30*time.Second
		Expect(tm.Window).To(Equal(300 * time.Second))
		Expect(tm.ReleaseTimeout).To(Equal(bound*3/2 + 2*cycle))
		Expect(tm.ReversalHold).To(Equal(2 * bound))
		Expect(tm.FillTimeout).To(Equal(30*time.Second + 15*time.Second + 2*cycle + time.Minute))
		Expect(tm.SwingWindow).To(Equal(8 * (bound + 5*time.Minute + 3*cycle)))
		Expect(src).To(HaveKeyWithValue("window", "default"))
		Expect(src).To(HaveKeyWithValue("release", "bound"))
	})

	It("follows a short scale-down window and polling interval from the ScaledObject", func() {
		tm, src := DeriveShareTimings(ShareTimingInputs{
			Cycle: cycle, ScaleDownWindow: dur(60 * time.Second), PollingInterval: dur(15 * time.Second),
		}, nil)
		Expect(tm.Window).To(Equal(60 * time.Second))
		Expect(tm.ReversalHold).To(Equal(2 * (60*time.Second + 15*time.Second + 15*time.Second + 30*time.Second)))
		Expect(src).To(HaveKeyWithValue("window", "scaledobject"))
	})

	It("lets measured releases set the hold, not a long grace that is rarely used", func() {
		l := NewShareLedger()
		tmp := ShareTimings{Window: time.Hour, ReleaseTimeout: time.Hour, FillTimeout: time.Hour}
		t0 := time.Unix(0, 0)
		for i := range 3 {
			held := map[string]int{"A": 4}
			start := t0.Add(time.Duration(i) * time.Hour)
			l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 1, Entitled: true}, held, start, tmp)
			held["A"] = 3
			l.Observe(held, start.Add(400*time.Second), tmp)
		}
		grace := 30 * time.Minute
		tm, src := DeriveShareTimings(ShareTimingInputs{Cycle: cycle, TerminationGrace: &grace}, l)
		Expect(src).To(HaveKeyWithValue("release", "measured"))
		Expect(tm.ReversalHold).To(Equal(800 * time.Second))
		// The timeout still allows for the configured grace: a release that
		// uses it must not be aborted.
		Expect(tm.ReleaseTimeout).To(BeNumerically(">", grace))
	})

	It("lands a move after the measured replica start, not the 5-minute estimate", func() {
		bound := 300*time.Second + 15*time.Second + 30*time.Second + 30*time.Second
		tm, src := DeriveShareTimings(ShareTimingInputs{Cycle: cycle, ReplicaStart: 10 * time.Minute}, nil)
		Expect(src).To(HaveKeyWithValue("start", "measured"))
		Expect(tm.MoveLatency).To(Equal(bound + 10*time.Minute + 3*cycle))
		Expect(tm.SwingWindow).To(Equal(8 * tm.MoveLatency))

		def, src := DeriveShareTimings(ShareTimingInputs{Cycle: cycle}, nil)
		Expect(src).To(HaveKeyWithValue("start", "default"))
		Expect(def.MoveLatency).To(Equal(bound + 5*time.Minute + 3*cycle))
	})

	It("bounds a measured replica start, so one stuck pod cannot freeze the pacing", func() {
		tm, _ := DeriveShareTimings(ShareTimingInputs{Cycle: cycle, ReplicaStart: 2 * time.Hour}, nil)
		bound := 300*time.Second + 15*time.Second + 30*time.Second + 30*time.Second
		Expect(tm.MoveLatency).To(Equal(bound + 30*time.Minute + 3*cycle))
	})

	DescribeTable("ShareReplicaStart: the slowest measured role, bounded, bad readings ignored",
		func(seconds map[string]float64, want time.Duration) {
			got, seeded := ShareReplicaStart(seconds, nil)
			Expect(got).To(Equal(want))
			Expect(seeded).To(BeFalse())
		},
		Entry("none measured", map[string]float64(nil), time.Duration(0)),
		Entry("the slowest of a group's roles", map[string]float64{"a": 120, "b": 600}, 10*time.Minute),
		Entry("NaN, negative and zero ignored", map[string]float64{"a": math.NaN(), "b": -5, "c": 0}, time.Duration(0)),
		Entry("NaN beside a real reading", map[string]float64{"a": math.NaN(), "b": 90}, 90*time.Second),
		Entry("beyond the bound", map[string]float64{"a": 7200}, 30*time.Minute),
		Entry("too large for a Duration", map[string]float64{"a": 1e300}, 30*time.Minute),
		Entry("infinite", map[string]float64{"a": math.Inf(1)}, 30*time.Minute),
	)

	DescribeTable("ShareReplicaStart: a seed is named as one only when it is the slowest role's",
		func(seconds map[string]float64, seededRoles map[string]bool, want time.Duration, wantSeeded bool) {
			got, seeded := ShareReplicaStart(seconds, seededRoles)
			Expect(got).To(Equal(want))
			Expect(seeded).To(Equal(wantSeeded))
		},
		Entry("the slowest role seeded", map[string]float64{"a": 120, "b": 600}, map[string]bool{"b": true}, 10*time.Minute, true),
		Entry("a faster role seeded", map[string]float64{"a": 120, "b": 600}, map[string]bool{"a": true}, 10*time.Minute, false),
		Entry("a tie with a measured role", map[string]float64{"a": 600, "b": 600}, map[string]bool{"a": true}, 10*time.Minute, false),
		Entry("a low seed alone", map[string]float64{"a": 60}, map[string]bool{"a": true}, time.Minute, true),
		Entry("both at or over the cap, unequal: the larger, seeded", map[string]float64{"a": 1800, "b": 7200},
			map[string]bool{"b": true}, 30*time.Minute, true),
		Entry("both at or over the cap, unequal: the larger, measured", map[string]float64{"a": 1800, "b": 7200},
			map[string]bool{"a": true}, 30*time.Minute, false),
	)

	It("names a seeded start as seed, under the 5 min default as well as over it", func() {
		for _, start := range []time.Duration{time.Minute, 20 * time.Minute} {
			tm, src := DeriveShareTimings(ShareTimingInputs{Cycle: cycle, ReplicaStart: start, ReplicaStartSeeded: true}, nil)
			Expect(src["start"]).To(Equal("seed"))
			unseeded, src2 := DeriveShareTimings(ShareTimingInputs{Cycle: cycle, ReplicaStart: start}, nil)
			Expect(src2["start"]).To(Equal("measured"))
			Expect(tm.MoveLatency).To(Equal(unseeded.MoveLatency), "the source names the start; it does not change it")
		}
	})

	It("gives a gang-scheduled LWS more time to schedule", func() {
		plain, _ := DeriveShareTimings(ShareTimingInputs{Cycle: cycle}, nil)
		lws, _ := DeriveShareTimings(ShareTimingInputs{Cycle: cycle, LWS: true}, nil)
		Expect(lws.FillTimeout - plain.FillTimeout).To(Equal(time.Minute))
	})
})
