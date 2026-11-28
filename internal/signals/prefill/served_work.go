package prefill

import "time"

// The prefill fleet's served work: what its pods were seen to compute,
// sustained with a steady backlog, by pod count. It floors a prefill knee
// priced from a lower bound of V_P (whatif.ServedKneeFloor), and with
// saturation.SwitchPrefillVPAtServedWork raises V_P for a TTFT. The capacity
// planner's replay keeps it (learnreplay, behind
// saturation.SwitchPrefillVPServedUnqueued); no analyzer keeps one yet.

// ServedWorkWindow is the span the prefill fleet's served work is averaged
// over (ServedWork.Peaks), and the shortest stretch of steady backlog it is
// read from. A two-minute stretch read a burst the pods carried with a queue
// building as work they keep up (#477): on a lab 2-pod fleet of long prompts
// it floored the 2+2 knee at 2.92 r/s, where that arm ran away past about 2.6.
// Five minutes is the issue's own minimum, chosen before any figure was read.
const ServedWorkWindow = 5 * time.Minute

// servedWorkMaxGap is the widest gap between two cycles that still continues
// a stretch: a missed cycle at the 15 s default, not a missing minute.
const servedWorkMaxGap = time.Minute

// ServedWorkSample is one cycle of a prefill variant: its serving pods
// (Pods), the computed-token rate summed over them (Sum), OK when every
// serving pod read one, and its prefill backlog: the requests waiting summed
// over the serving pods (Queue; domain.ReplicaMetrics.QueueLength), QueueOK
// when every one of them read it.
type ServedWorkSample struct {
	At      time.Time
	Pods    int
	Sum     float64
	OK      bool
	Queue   int
	QueueOK bool
}

// ServedWork is a prefill variant's computed-token work over Retention,
// cycle by cycle: what the fleet's prefill pods were seen to compute, from
// the engines' own counter (domain.ReplicaMetrics.PrefillComputedTokenRate).
// The zero value is empty and ready to use.
type ServedWork struct {
	samples []ServedWorkSample
	// best is, by pod count, the highest stretch read from samples since
	// forgotten (WindowPeaks): a recording is read as a whole.
	best map[int]float64
}

// Note keeps one cycle and forgets what is older than Retention.
func (w *ServedWork) Note(s ServedWorkSample) {
	w.samples = append(w.samples, s)
	cut := s.At.Add(-Retention)
	i := 0
	for i < len(w.samples) && w.samples[i].At.Before(cut) {
		i++
	}
	if i > 0 {
		if w.best == nil {
			w.best = map[int]float64{}
		}
		read, _ := w.stretches(0, i)
		for pods, v := range read {
			w.best[pods] = max(w.best[pods], v)
		}
	}
	w.samples = w.samples[i:]
}

// Peaks is, at each pod count, the highest mean computed-token rate the
// variant's serving pods sustained over a steady stretch of ServedWorkWindow
// or more: consecutive cycles, each read on every pod, at that one count,
// none more than servedWorkMaxGap apart, whose prefill backlog did not grow
// (steady). A stretch shorter than the window, broken, or whose backlog grew
// is no reading: a burst the pods carry with a queue building is not a rate
// they hold. The prefill pods compute no faster than their capacity, so each
// reading is a lower bound on what that many pods can compute at the traffic
// they ran: the floor under a prefill knee priced from a lower bound
// (whatif.ServedKneeFloor).
func (w *ServedWork) Peaks() map[int]float64 {
	read, _ := w.stretches(0, len(w.samples))
	return read
}

// WindowPeaks is Peaks over the whole window replayed: the stretches the
// samples kept read, and those of samples forgotten past Retention. The
// planner reads a recording as a whole, as prefill-vp-window-best keeps the
// peak under a queue; work the pods kept up hours earlier is work that many
// pods can do (saturation.SwitchPrefillVPAtServedWork).
func (w *ServedWork) WindowPeaks() map[int]float64 {
	out := w.Peaks()
	for pods, v := range w.best {
		out[pods] = max(out[pods], v)
	}
	return out
}

// PeaksUnsteady is Peaks, and the pod counts at which a stretch of
// ServedWorkWindow kept since Retention had its backlog read on every cycle
// and growing, and Peaks reads none: served work was there, and not
// sustained. A stretch whose backlog was not read is no evidence either way,
// and a count with only such stretches is not in it.
func (w *ServedWork) PeaksUnsteady() (map[int]float64, map[int]bool) {
	read, grew := w.stretches(0, len(w.samples))
	var out map[int]bool
	for pods := range grew {
		if _, ok := read[pods]; !ok {
			if out == nil {
				out = map[int]bool{}
			}
			out[pods] = true
		}
	}
	return read, out
}

// stretches is Peaks over the stretches that start at samples lo to hi-1,
// and the pod counts with a stretch of the full window whose backlog was
// read on every cycle and grew.
func (w *ServedWork) stretches(lo, hi int) (map[int]float64, map[int]bool) {
	out, grew := map[int]float64{}, map[int]bool{}
	n := len(w.samples)
	for i := lo; i < hi; i++ {
		first := w.samples[i]
		if !first.OK || first.Pods < 1 {
			continue
		}
		steady, read := w.enteredSteady(i), first.QueueOK
		sum := 0.0
		for j := i; j < n; j++ {
			s := w.samples[j]
			if !s.OK || s.Pods != first.Pods || (j > i && s.At.Sub(w.samples[j-1].At) > servedWorkMaxGap) {
				break
			}
			steady = steady && s.QueueOK && s.Queue <= first.Queue+first.Pods
			read = read && s.QueueOK
			sum += s.Sum
			if s.At.Sub(first.At) >= ServedWorkWindow {
				if !steady {
					if read {
						grew[first.Pods] = true
					}
				} else if mean := sum / float64(j-i+1); mean > out[first.Pods] {
					out[first.Pods] = mean
				}
				break
			}
		}
	}
	return out, grew
}

// enteredSteady says a stretch starting at sample i is not entered while the
// backlog grows: the backlog there is read, and at most one request a pod
// over the cycle before it, where that cycle continues the stretch (read, at
// the same count, within servedWorkMaxGap). A first cycle with no such cycle
// before it passes.
func (w *ServedWork) enteredSteady(i int) bool {
	s := w.samples[i]
	if !s.QueueOK {
		return false
	}
	if i == 0 {
		return true
	}
	p := w.samples[i-1]
	if !p.OK || !p.QueueOK || p.Pods != s.Pods || s.At.Sub(p.At) > servedWorkMaxGap {
		return true
	}
	return s.Queue <= p.Queue+s.Pods
}
