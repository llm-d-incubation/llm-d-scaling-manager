package learnreplay

import (
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// ServedWorkWindow is the span the prefill fleet's served work is averaged
// over (servedWork.peaks), and the shortest stretch of steady backlog it is
// read from. A two-minute stretch read a burst the pods carried with a queue
// building as work they keep up (#477): on a lab 2-pod fleet of long prompts
// it floored the 2+2 knee at 2.92 r/s, where that arm ran away past about 2.6.
// Five minutes is the issue's own minimum, chosen before any figure was read.
const ServedWorkWindow = 5 * time.Minute

// servedWorkMaxGap is the widest gap between two cycles that still continues
// a stretch: a missed cycle at the 15 s default, not a missing minute.
const servedWorkMaxGap = time.Minute

// servedWorkSample is one cycle of a prefill variant: its serving pods, the
// computed-token rate summed over them, ok when every serving pod read one,
// and its prefill backlog: the requests waiting summed over the serving pods
// (domain.ReplicaMetrics.QueueLength), queueOK when every one of them read
// it.
type servedWorkSample struct {
	at      time.Time
	pods    int
	sum     float64
	ok      bool
	queue   int
	queueOK bool
}

// servedWork is a prefill variant's computed-token work over prefill.Retention,
// cycle by cycle: what the fleet's prefill pods were seen to compute, from
// the engines' own counter (domain.ReplicaMetrics.PrefillComputedTokenRate).
type servedWork struct {
	samples []servedWorkSample
	// best is, by pod count, the highest stretch read from samples since
	// forgotten (windowPeaks): a recording is read as a whole.
	best map[int]float64
}

// note keeps one cycle and forgets what is older than prefill.Retention.
func (w *servedWork) note(s servedWorkSample) {
	w.samples = append(w.samples, s)
	cut := s.at.Add(-prefill.Retention)
	i := 0
	for i < len(w.samples) && w.samples[i].at.Before(cut) {
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

// peaks is, at each pod count, the highest mean computed-token rate the
// variant's serving pods sustained over a steady stretch of ServedWorkWindow
// or more: consecutive cycles, each read on every pod, at that one count,
// none more than servedWorkMaxGap apart, whose prefill backlog did not grow
// (steady). A stretch shorter than the window, broken, or whose backlog grew
// is no reading: a burst the pods carry with a queue building is not a rate
// they hold. The prefill pods compute no faster than their capacity, so each
// reading is a lower bound on what that many pods can compute at the traffic
// they ran: the floor under a prefill knee priced from a lower bound (the
// planner's servedKneeFloor).
func (w *servedWork) peaks() map[int]float64 {
	read, _ := w.stretches(0, len(w.samples))
	return read
}

// windowPeaks is peaks over the whole window replayed: the stretches the
// samples kept read, and those of samples forgotten past prefill.Retention.
// The planner reads a recording as a whole, as prefill-vp-window-best keeps
// the peak under a queue; work the pods kept up hours earlier is work that
// many pods can do (saturation.SwitchPrefillVPAtServedWork).
func (w *servedWork) windowPeaks() map[int]float64 {
	out := w.peaks()
	for pods, v := range w.best {
		out[pods] = max(out[pods], v)
	}
	return out
}

// peaksUnsteady is peaks, and the pod counts at which a stretch of
// ServedWorkWindow kept since prefill.Retention had its backlog read on
// every cycle and growing, and peaks reads none: served work was there, and
// not sustained. A stretch whose backlog was not read is no evidence either
// way, and a count with only such stretches is not in it.
func (w *servedWork) peaksUnsteady() (map[int]float64, map[int]bool) {
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

// stretches is peaks over the stretches that start at samples lo to hi-1,
// and the pod counts with a stretch of the full window whose backlog was
// read on every cycle and grew.
func (w *servedWork) stretches(lo, hi int) (map[int]float64, map[int]bool) {
	out, grew := map[int]float64{}, map[int]bool{}
	n := len(w.samples)
	for i := lo; i < hi; i++ {
		first := w.samples[i]
		if !first.ok || first.pods < 1 {
			continue
		}
		steady, read := w.enteredSteady(i), first.queueOK
		sum := 0.0
		for j := i; j < n; j++ {
			s := w.samples[j]
			if !s.ok || s.pods != first.pods || (j > i && s.at.Sub(w.samples[j-1].at) > servedWorkMaxGap) {
				break
			}
			steady = steady && s.queueOK && s.queue <= first.queue+first.pods
			read = read && s.queueOK
			sum += s.sum
			if s.at.Sub(first.at) >= ServedWorkWindow {
				if !steady {
					if read {
						grew[first.pods] = true
					}
				} else if mean := sum / float64(j-i+1); mean > out[first.pods] {
					out[first.pods] = mean
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
func (w *servedWork) enteredSteady(i int) bool {
	s := w.samples[i]
	if !s.queueOK {
		return false
	}
	if i == 0 {
		return true
	}
	p := w.samples[i-1]
	if !p.ok || !p.queueOK || p.pods != s.pods || s.at.Sub(p.at) > servedWorkMaxGap {
		return true
	}
	return s.queue <= p.queue+s.pods
}
