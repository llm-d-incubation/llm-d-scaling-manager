package prefill

import (
	"cmp"
	"math"
	"slices"
	"time"
)

// The imbalance f from pooled minutes (Cycle.FPooledMinutes).
//
// A minute's shares are read only at MinRequestsPerPodMinute requests a pod:
// under that, the chance excess the de-bias takes off is a large part of the
// share, and a minute's f is mostly noise. A fleet of long prompts can serve
// well under that at its knee: the Qwen3-32B lab fleet served about 78
// requests a pod a minute at prompts of about 16000 tokens, so it learned no
// f at any count, and the planner priced every count above one at
// max(whatif.ImbalanceFloor, raw share), 1.5, a prefill knee at four pods
// about 30% under the rate they served without a queue.
//
// With the switch on, consecutive minutes on the same pods are pooled until
// they carry MinRequestsPerPodMinute requests a pod between them, and the
// pool gives one f: the busiest pod's share of the pool's computed tokens
// over the mean share, less the chance excess of the pooled count
// (poissonMaxExcess at the pool's requests a pod). The threshold and the
// de-bias are the minute's own, so a pool carries the counts a minute must;
// a minute that carries them alone is read alone, as with the switch off.
// The pool is the sustained share: a pod busier in one minute and quieter in
// the next reads nearer even, which is what a queue that lasts sees.
//
// A pool spans at most MaxPooledMinutes minutes: past that, its oldest
// minute is dropped, so a fleet far under the threshold gives no f, as
// before, rather than one taken over a window whose load moved. A minute not
// every serving replica of which was admitted, or whose shares are not all
// read, a change of the pods, or a gap of two minutes with no cycle ends the
// pool unread.

// MaxPooledMinutes is the most minutes one pool of minutes spans
// (Cycle.FPooledMinutes): the planner's prompt window, five minutes
// (whatif.PromptWindowCycles at the default 15 s cycle).
const MaxPooledMinutes = 5

// poolMinute is one minute of a pool: each pod's computed-token rate, in the
// pool's pod order, and the minute's requests a pod.
type poolMinute struct {
	at        time.Time
	rates     []float64
	perPod    float64
	multiRank bool
}

// fPool is the minutes pooled toward the key's next f.
type fPool struct {
	// pods is the pool's pods, sorted.
	pods    []string
	minutes []poolMinute
	// emitted says the pool gave its f; a later cycle of its last minute
	// rewrites that f, and the next minute starts a new pool.
	emitted bool
}

// poolMinuteOf is the cycle's shares as one minute of a pool, with its pods
// sorted, and whether every share was read.
func poolMinuteOf(shares []share, now time.Time) (poolMinute, []string, bool) {
	if len(shares) < 2 {
		return poolMinute{}, nil, false
	}
	sorted := slices.Clone(shares)
	slices.SortFunc(sorted, func(a, b share) int { return cmp.Compare(a.pod, b.pod) })
	m := poolMinute{at: now, rates: make([]float64, len(sorted))}
	pods := make([]string, len(sorted))
	var requests float64
	for i, s := range sorted {
		if !s.rateOK || !s.requestsOK || !isFinite(s.rate) || !isFinite(s.requests) {
			return poolMinute{}, nil, false
		}
		if i > 0 && s.pod == sorted[i-1].pod {
			return poolMinute{}, nil, false
		}
		pods[i], m.rates[i] = s.pod, s.rate
		requests += s.requests
		m.multiRank = m.multiRank || s.multiRank
	}
	m.perPod = requests * 60 / float64(len(sorted))
	return m, pods, isFinite(m.perPod)
}

// notePooledImbalance folds the cycle into the key's pool, and keeps the
// pool's f as the key's latest minute once the pool carries
// MinRequestsPerPodMinute requests a pod.
func (ks *keyState) notePooledImbalance(shares []share, allAdmitted bool, now time.Time) {
	m, pods, ok := poolMinuteOf(shares, now)
	if !allAdmitted || !ok {
		ks.pool = fPool{}
		return
	}
	p := &ks.pool
	last := len(p.minutes) - 1
	switch {
	case last >= 0 && now.Sub(p.minutes[last].at) < SampleSpacing && slices.Equal(p.pods, pods):
		// Another cycle of the pool's last minute: its rows are one-minute
		// rates, so this one replaces that one's.
		m.at = p.minutes[last].at
		p.minutes[last] = m
	default:
		if p.emitted || !slices.Equal(p.pods, pods) || (last >= 0 && now.Sub(p.minutes[last].at) >= 2*SampleSpacing) {
			*p = fPool{pods: pods}
		}
		p.minutes = append(p.minutes, m)
		if len(p.minutes) > MaxPooledMinutes {
			p.minutes = slices.Delete(p.minutes, 0, len(p.minutes)-MaxPooledMinutes)
		}
	}
	if m.perPod >= MinRequestsPerPodMinute {
		// A minute that carries the threshold alone is read alone, as with
		// the switch off: the lighter minutes pooled before it are dropped,
		// not read into its f.
		p.minutes = p.minutes[len(p.minutes)-1:]
	}
	s, ok := p.sample()
	if !ok {
		return
	}
	p.emitted = true
	ks.putF(s, now)
}

// sample is the pool's f, at its last minute, and whether it carries
// MinRequestsPerPodMinute requests a pod.
func (p *fPool) sample() (fSample, bool) {
	n := len(p.pods)
	if n < 2 || len(p.minutes) == 0 {
		return fSample{}, false
	}
	sums := make([]float64, n)
	var perPod float64
	multi := false
	for _, m := range p.minutes {
		for i, r := range m.rates {
			sums[i] += r
		}
		perPod += m.perPod
		multi = multi || m.multiRank
	}
	var total, busiest float64
	for _, s := range sums {
		total += s
		busiest = math.Max(busiest, s)
	}
	if !(total > 0) || !(perPod >= MinRequestsPerPodMinute) || !isFinite(perPod) {
		return fSample{}, false
	}
	raw := float64(n) * busiest / total
	f := math.Max(1, raw-poissonMaxExcess(n, perPod))
	load := total / float64(n*len(p.minutes))
	return fSample{at: p.minutes[len(p.minutes)-1].at, f: f, pods: n, load: load, multiRank: multi}, isFinite(f)
}
