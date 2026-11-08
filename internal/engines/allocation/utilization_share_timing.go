package allocation

import "time"

// Defaults the cluster applies when a ScaledObject or pod template leaves a
// value unset, and the fixed parts of a release and a fill (§8.4).
const (
	hpaDefaultScaleDownWindow  = 300 * time.Second
	kedaDefaultPolling         = 30 * time.Second
	k8sDefaultTerminationGrace = 30 * time.Second
	hpaSyncPeriod              = 15 * time.Second
	// shareModelLoadEstimate stands in for pod start and model load until the
	// analyzer has measured a replica start of the group's (ReplicaStart).
	shareModelLoadEstimate = 5 * time.Minute
	// shareMaxReplicaStart bounds a measured replica start: one pod stuck
	// pulling an image for an hour would otherwise stretch every pacing
	// horizon of its group with it.
	shareMaxReplicaStart = 30 * time.Minute
	// shareSchedulingAllowance is how long scheduling a receiver may take
	// before its fill times out; a gang-scheduled LWS group gets twice this.
	shareSchedulingAllowance = time.Minute
)

// ShareTimingInputs are the cluster facts a group's timings derive from: the
// slowest of its donors' configurations. A nil value is unset, and the
// default the cluster applies is used.
type ShareTimingInputs struct {
	Cycle            time.Duration
	ScaleDownWindow  *time.Duration
	PollingInterval  *time.Duration
	TerminationGrace *time.Duration
	// LWS is true when any role of the group is a LeaderWorkerSet, whose
	// groups are gang-scheduled.
	LWS bool
	// ReplicaStart is the measured time a new replica of the group takes to
	// become Ready (ShareReplicaStart), zero when none is measured.
	ReplicaStart time.Duration
	// ReplicaStartSeeded says ReplicaStart is a ScaledObject's
	// replicaStartSeconds seed, no role it came from measured
	// (ShareReplicaStart); the start's source is then "seed".
	ReplicaStartSeeded bool
}

// ShareReplicaStart is the slowest measured replica start among a group's
// roles (ShareGroup.StartSeconds), at most shareMaxReplicaStart, or zero when
// none is measured. The slowest, because the timings are the group's and a
// move is not done until the receiver's replica serves: paced on the fastest
// role, a whole-node model's moves would be judged to land in half the time
// they take, and moved for bursts that end before they serve. A reading that
// is not a positive number (NaN included) is ignored, and one beyond the bound
// is taken at the bound -- compared in seconds, before it becomes a Duration
// it may not fit.
//
// seeded says the slowest role's start is a seed (seededRoles names it); a
// tie with a measured role is measured.
func ShareReplicaStart(startSeconds map[string]float64, seededRoles map[string]bool) (start time.Duration, seeded bool) {
	slowest := 0.0
	for role, s := range startSeconds {
		switch {
		case s > slowest:
			slowest, seeded = s, seededRoles[role]
		case s == slowest && !seededRoles[role]:
			seeded = false
		}
	}
	switch {
	case !(slowest > 0):
		return 0, false
	case slowest >= shareMaxReplicaStart.Seconds():
		return shareMaxReplicaStart, seeded
	}
	return time.Duration(slowest * float64(time.Second)), seeded
}

// The values of a ShareTimingSource: the group's own measurement, the
// ScaledObject, the pod template, the cluster's default for an unset value,
// or a ScaledObject's replicaStartSeconds seed for the start (published as
// ShareTimingFromScaledObject).
const (
	ShareTimingMeasured         = "measured"
	ShareTimingFromScaledObject = "scaledobject"
	ShareTimingFromPod          = "pod"
	ShareTimingDefault          = "default"
	ShareTimingSeed             = "seed"
)

// ShareTimingSource says where each derived value came from, for the log line
// and the effective-timing gauge (§8.4): measured, scaledobject, pod, default.
type ShareTimingSource map[string]string

// DeriveShareTimings computes a group's timings from cluster facts and, once
// it has them, the group's own measured releases (§8.4). Nothing here is
// configuration: the values are facts the cluster states, or are measured, and
// the multipliers are those §6.7's simulation validated.
//
//   - the release bound is the scale-down window, the HPA sync, the KEDA poll
//     and the donor pods' termination grace -- an upper bound, so the release
//     timeout built on it does not abort a release that is merely slow;
//   - the release time is the measured p90 once three releases have
//     completed, else the bound; the hold and the swing window build on it,
//     so a long drain grace that is rarely used does not stretch them;
//   - a move lands after the release and the receiver's start: the measured
//     replica start once the analyzer has one (ReplicaStart), else
//     shareModelLoadEstimate; the move latency and the swing window build on
//     it.
func DeriveShareTimings(in ShareTimingInputs, l *ShareLedger) (ShareTimings, ShareTimingSource) {
	src := ShareTimingSource{}
	pick := func(name string, v *time.Duration, def time.Duration, from string) time.Duration {
		if v != nil && *v >= 0 {
			src[name] = from
			return *v
		}
		src[name] = ShareTimingDefault
		return def
	}
	window := pick("window", in.ScaleDownWindow, hpaDefaultScaleDownWindow, ShareTimingFromScaledObject)
	polling := pick("polling", in.PollingInterval, kedaDefaultPolling, ShareTimingFromScaledObject)
	grace := pick("grace", in.TerminationGrace, k8sDefaultTerminationGrace, ShareTimingFromPod)

	bound := window + hpaSyncPeriod + polling + grace
	release := bound
	src["release"] = "bound"
	if l != nil {
		if m, ok := l.MeasuredRelease(); ok {
			release = m
			src["release"] = ShareTimingMeasured
		}
	}
	scheduling := shareSchedulingAllowance
	if in.LWS {
		scheduling *= 2
	}
	start := shareModelLoadEstimate
	src["start"] = ShareTimingDefault
	if in.ReplicaStart > 0 {
		start = min(in.ReplicaStart, shareMaxReplicaStart)
		src["start"] = ShareTimingMeasured
		if in.ReplicaStartSeeded {
			// A ScaledObject's seed, which can be under the 5 min default
			// as well as over it (learner PR 5c).
			src["start"] = ShareTimingSeed
		}
	}
	latency := release + start + 3*in.Cycle
	return ShareTimings{
		Window:         window,
		ReleaseTimeout: bound*3/2 + 2*in.Cycle,
		FillTimeout:    polling + hpaSyncPeriod + 2*in.Cycle + scheduling,
		ReversalHold:   2 * release,
		SwingWindow:    8 * latency,
		MoveLatency:    latency,
	}, src
}
