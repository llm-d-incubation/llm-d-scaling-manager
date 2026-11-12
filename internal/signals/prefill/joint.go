package prefill

import (
	"math"
	"slices"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// The joint prefill and decode TTFT model's inputs, J1 of
// docs/proposals/slo-capacity-planner-ttft.md ("Proposed: joint prefill and
// decode TTFT sizing"). Each is read from the per-pod vLLM histograms the
// collector already reads for the TTFT model, and none needs a pod to queue:
//
//   - the prefill pod's unshared work S, by inverting processor sharing,
//     S = T/(1 + λT), on a pod whose engine queue was empty;
//   - the prefill pod's overhead around its compute, stretched: a₀, the
//     highest (AvgTTFT − AvgQueueTime − AvgPrefillTime)·(1 − ρ_p);
//   - the KV pull's unshared work on the decode pods, by the same inversion,
//     by the prefill pods' utilization in buckets of PullBucketWidth;
//   - the decode batch's ITL line ITL(k) = α + β·k;
//   - the decode pod's own part, m_d·ITL + d: its TTFT less the pull.
//
// They are inputs only. Nothing here prices a fleet (whatif.JointTTFT does)
// and nothing sizes one.

// SourceInverted is V_P from the queue-free inversion of processor sharing
// (JointEstimate.VP): the prompt over the prefill pod's unshared work. It is
// no bound, and it needs no queue, so it stands only where every queueing
// source refused.
const SourceInverted Source = "inverted"

const (
	// JointRetention is how long a joint reading is kept. A staircase of
	// ten-minute steps spans about an hour, and the ITL line and the pull
	// table need the spread of loads it holds.
	JointRetention = time.Hour
	// PullBucketWidth is the width of a pull-table bucket, in prefill
	// utilization.
	PullBucketWidth = 0.1
	// MinJointReadings is the fewest readings an input is learned from.
	MinJointReadings = 4
	// OverheadQuantile is the share of overhead readings at or under a₀: the
	// highest past one reading in ten, so that one scrape does not set it.
	// The highest reading errs high, the side that asks for more replicas;
	// one scrape's spike read 17.4 ms on the staircase where its step read
	// 13.6 to 14.6 ms.
	OverheadQuantile = 0.9
	// pullWaitingSlack bounds a decode pod's waiting requests, in units of
	// the requests its own pulls hold (1 + λ·T): more than that is a pod
	// behind on its batch, whose pull and steps say nothing of the line. The
	// staircase's decode pods held 1 to 5 waiting below the knee and 61 to
	// 106 once past it.
	pullWaitingSlack = 4.0
	// QueueFreeSeconds is the most mean engine queue time a prefill pod may
	// read and still count as queue-free. vLLM's queue time is never quite
	// 0: the staircase's prefill pods read 5 to 10 microseconds below the
	// knee, and a pod that truly queues reads milliseconds.
	QueueFreeSeconds = 0.001
)

// The joint inputs' refusals.
const (
	// NoQueueFreeMinutes is no unshared prefill work: too few readings of a
	// settled prefill pod with an empty engine queue and a prefill time.
	NoQueueFreeMinutes Refusal = "NoQueueFreeMinutes"
	// NoOverheadReadings is no a₀: too few queue-free readings with a TTFT.
	NoOverheadReadings Refusal = "NoOverheadReadings"
	// NoPullReadings is no pull table: too few decode readings with a KV
	// transfer time, in a cycle whose prefill utilization was read.
	NoPullReadings Refusal = "NoPullReadings"
	// NoITLLine is no ITL(k) fit: too few decode readings, too little spread
	// in the batch, or a fit with no positive slope.
	NoITLLine Refusal = "NoITLLine"
	// NoDecodeOwnLine is no m_d and d: too few decode readings with a TTFT
	// and a pull, or a fit with no positive slope.
	NoDecodeOwnLine Refusal = "NoDecodeOwnLine"
)

// JointCycle is one cycle's rows of a model's prefill and decode variants.
type JointCycle struct {
	Now time.Time
	// Prefill and Decode are the rows of the model's prefill and decode
	// variants.
	Prefill, Decode []domain.ReplicaMetrics
	// SGLang says the prefill pods run SGLang, whose prompt reads another
	// way (MeanEffectivePrompt).
	SGLang bool
	// Settle is the settle tracker; nil admits every Ready replica.
	Settle *Settle
}

// jointRead is one reading: x and y are the input's own pair.
type jointRead struct {
	at   time.Time
	x, y float64
}

// JointWindow is a model's joint readings, kept for JointRetention.
type JointWindow struct {
	// work is (S, prompt) per queue-free prefill pod; overhead the stretched
	// overhead per queue-free prefill pod (x only).
	work, overhead []jointRead
	// pull is (the cycle's prefill utilization, S of the pull) per decode
	// pod, and pullPrompt the cycle's prompt beside each.
	pull       []jointRead
	pullPrompt []float64
	// itl is (k, ITL) and own (ITL, TTFT less the pull) per decode pod;
	// output is the decode pods' mean output length (x only).
	itl, own, output []jointRead
	// utilization is the latest cycle's prefill utilization, NaN with none.
	utilization float64
	tag         string
}

// NewJointWindow is an empty window.
func NewJointWindow() *JointWindow {
	return &JointWindow{utilization: math.NaN()}
}

// Under keys the window to tag (the variants' accelerators and engine
// arguments): a change of tag drops every reading, since none of them is a
// property of the new engine. An empty tag is unknown and keeps them.
func (w *JointWindow) Under(tag string) {
	if tag == "" {
		return
	}
	if w.tag != "" && tag != w.tag {
		*w = *NewJointWindow()
	}
	w.tag = tag
}

// Observe folds one cycle into the window.
func (w *JointWindow) Observe(c JointCycle) {
	w.age(c.Now)
	prompt, _ := MeanEffectivePrompt(c.Prefill, c.SGLang)
	var rhoSum float64
	rhoN := 0
	for _, rm := range c.Prefill {
		s, rho, ok := queueFreeWork(rm, c.Settle, c.Now)
		if !ok {
			continue
		}
		rhoSum += rho
		rhoN++
		if ieff, ok := effectivePrompt(rm, c.SGLang); ok {
			w.work = append(w.work, jointRead{c.Now, s, ieff})
		}
		if rm.AvgTTFTKnown && finitePositive(rm.AvgTTFT) {
			if o := (rm.AvgTTFT - rm.AvgQueueTime - rm.AvgPrefillTime) * (1 - rho); finitePositive(o) {
				w.overhead = append(w.overhead, jointRead{at: c.Now, x: o})
			}
		}
	}
	w.utilization = math.NaN()
	if rhoN > 0 {
		w.utilization = rhoSum / float64(rhoN)
	}
	var outSum float64
	outN := 0
	for _, rm := range c.Decode {
		lambda, ok := decodeAdmitted(rm, c.Settle, c.Now)
		if !ok {
			continue
		}
		pull := rm.AvgKVTransferTime
		pullOK := rm.AvgKVTransferTimeKnown && finitePositive(pull)
		if pullOK && rhoN > 0 && prompt > 0 {
			w.pull = append(w.pull, jointRead{c.Now, w.utilization, pull / (1 + lambda*pull)})
			w.pullPrompt = append(w.pullPrompt, prompt)
		}
		if !rm.AvgITLKnown || !finitePositive(rm.AvgITL) {
			continue
		}
		if out := rm.AvgOutputTokens; finitePositive(out) {
			// The batch's mean over the minute, by Little's law: requests
			// in decode are the completion rate times the time each spends
			// there, n_out steps of ITL. The running gauge is an instant,
			// and its noise against a minute's ITL flattens the fit (on the
			// staircase's B, β 0.167 ms from the gauge, 0.218 from this).
			w.itl = append(w.itl, jointRead{c.Now, lambda * out * rm.AvgITL, rm.AvgITL})
			outSum += out
			outN++
		}
		if pullOK && rm.AvgTTFTKnown && finitePositive(rm.AvgTTFT) {
			if own := rm.AvgTTFT - pull; finitePositive(own) {
				w.own = append(w.own, jointRead{c.Now, rm.AvgITL, own})
			}
		}
	}
	if outN > 0 {
		w.output = append(w.output, jointRead{at: c.Now, x: outSum / float64(outN)})
	}
}

// queueFreeWork is a prefill row's unshared work S = T/(1 + λT) and its
// utilization ρ = λS, read only on a settled serving pod whose engine queue
// was empty over the minute (QueueFreeSeconds): under processor sharing the
// pod admits every prompt, and a queue says it stopped.
func queueFreeWork(rm domain.ReplicaMetrics, s *Settle, now time.Time) (work, rho float64, ok bool) {
	if !Serving(rm) || !s.Settled(rm, now) || !rm.AvgQueueTimeKnown || rm.AvgQueueTime > QueueFreeSeconds ||
		!rm.AvgPrefillTimeKnown || !finitePositive(rm.AvgPrefillTime) {
		return 0, 0, false
	}
	lambda, ok := requestRate(rm)
	if !ok {
		return 0, 0, false
	}
	t := rm.AvgPrefillTime
	work = t / (1 + lambda*t)
	return work, lambda * work, true
}

// decodeAdmitted is a decode row's completion rate, when the row reads: a
// settled serving pod with a rate, not behind on its batch (pullWaitingSlack).
func decodeAdmitted(rm domain.ReplicaMetrics, s *Settle, now time.Time) (float64, bool) {
	if !Serving(rm) || !s.Settled(rm, now) {
		return 0, false
	}
	lambda, ok := requestRate(rm)
	if !ok {
		return 0, false
	}
	held := 1.0
	if rm.AvgKVTransferTimeKnown && finitePositive(rm.AvgKVTransferTime) {
		held += lambda * rm.AvgKVTransferTime
	}
	if rm.QueueLengthKnown && float64(rm.QueueLength) > pullWaitingSlack*held {
		return 0, false
	}
	return lambda, true
}

// age drops the readings older than JointRetention at now.
func (w *JointWindow) age(now time.Time) {
	keep := func(reads []jointRead) int {
		i := 0
		for i < len(reads) && now.Sub(reads[i].at) > JointRetention {
			i++
		}
		return i
	}
	w.work = w.work[keep(w.work):]
	w.overhead = w.overhead[keep(w.overhead):]
	n := keep(w.pull)
	w.pull, w.pullPrompt = w.pull[n:], w.pullPrompt[n:]
	w.itl = w.itl[keep(w.itl):]
	w.own = w.own[keep(w.own):]
	w.output = w.output[keep(w.output):]
}

// PullBucket is one bucket of the pull table: the KV pull's unshared work on
// a decode pod while the prefill pods ran at a utilization in [Utilization,
// Utilization + PullBucketWidth), read at PromptTokens.
type PullBucket struct {
	Utilization  float64
	Seconds      float64
	PromptTokens float64
	Samples      int
}

// JointEstimate is what a joint window supports. A refused input is zero
// with its refusal set.
type JointEstimate struct {
	// PrefillWork is S, a prefill pod's unshared work per request, at
	// PrefillWorkPrompt tokens: the median of the queue-free readings. VP is
	// the capacity it gives, PrefillWorkPrompt / PrefillWork tokens/s.
	PrefillWork, PrefillWorkPrompt, VP float64
	PrefillWorkSamples                 int
	PrefillWorkRefused                 Refusal
	// Overhead is a₀ (OverheadQuantile of the readings).
	Overhead        float64
	OverheadSamples int
	OverheadRefused Refusal
	// Pull is the pull table, by utilization; nil when refused.
	Pull        []PullBucket
	PullRefused Refusal
	// ITLAlpha and ITLBeta are ITL(k) = α + β·k, seconds, k in requests.
	ITLAlpha, ITLBeta float64
	ITLSamples        int
	ITLRefused        Refusal
	// DecodeSteps and DecodeFixed are m_d and d: the decode pod's TTFT less
	// its pull is m_d·ITL + d.
	DecodeSteps, DecodeFixed float64
	DecodeSamples            int
	DecodeRefused            Refusal
	// OutputTokens is n_out, the decode pods' mean output length; 0 unread.
	OutputTokens float64
	// PrefillUtilization is ρ_p in the latest cycle, the mean of its
	// queue-free prefill pods; PrefillUtilizationKnown is false, and it is 0,
	// when none read. No NaN: an estimate is compared and encoded whole.
	PrefillUtilization      float64
	PrefillUtilizationKnown bool
}

// Estimate is what the window supports now.
func (w *JointWindow) Estimate() JointEstimate {
	var est JointEstimate
	if !math.IsNaN(w.utilization) {
		est.PrefillUtilization, est.PrefillUtilizationKnown = w.utilization, true
	}
	if n := len(w.work); n >= MinJointReadings {
		est.PrefillWork = medianOf(w.work, func(r jointRead) float64 { return r.x })
		est.PrefillWorkPrompt = medianOf(w.work, func(r jointRead) float64 { return r.y })
		est.VP, est.PrefillWorkSamples = est.PrefillWorkPrompt/est.PrefillWork, n
	} else {
		est.PrefillWorkRefused = NoQueueFreeMinutes
	}
	if n := len(w.overhead); n >= MinJointReadings {
		est.Overhead, est.OverheadSamples = quantileOf(w.overhead, OverheadQuantile), n
	} else {
		est.OverheadRefused = NoOverheadReadings
	}
	est.Pull = w.pullTable()
	if est.Pull == nil {
		est.PullRefused = NoPullReadings
	}
	est.ITLAlpha, est.ITLBeta, est.ITLSamples, est.ITLRefused = fitJointLine(w.itl, NoITLLine)
	est.DecodeFixed, est.DecodeSteps, est.DecodeSamples, est.DecodeRefused = fitJointLine(w.own, NoDecodeOwnLine)
	if len(w.output) > 0 {
		est.OutputTokens = medianOf(w.output, func(r jointRead) float64 { return r.x })
	}
	return est
}

// pullTable is the pull readings by bucket: each bucket's median work and
// prompt, for a bucket with MinJointReadings; nil with none.
func (w *JointWindow) pullTable() []PullBucket {
	type acc struct {
		reads   []jointRead
		prompts []jointRead
	}
	byBucket := map[int]*acc{}
	for i, r := range w.pull {
		b := min(int(r.x/PullBucketWidth+1e-9), int(1/PullBucketWidth)-1)
		a := byBucket[b]
		if a == nil {
			a = &acc{}
			byBucket[b] = a
		}
		a.reads = append(a.reads, r)
		a.prompts = append(a.prompts, jointRead{x: w.pullPrompt[i]})
	}
	var out []PullBucket
	for b, a := range byBucket {
		if len(a.reads) < MinJointReadings {
			continue
		}
		out = append(out, PullBucket{
			Utilization:  float64(b) * PullBucketWidth,
			Seconds:      medianOf(a.reads, func(r jointRead) float64 { return r.y }),
			PromptTokens: medianOf(a.prompts, func(r jointRead) float64 { return r.x }),
			Samples:      len(a.reads),
		})
	}
	slices.SortFunc(out, func(x, y PullBucket) int {
		switch {
		case x.Utilization < y.Utilization:
			return -1
		case x.Utilization > y.Utilization:
			return 1
		}
		return 0
	})
	return out
}

// fitJointLine is y = intercept + slope·x by ordinary least squares
// (itl.Fit), refused with refusal on fewer than MinJointReadings readings or a
// fit itl.ValidModel rejects.
func fitJointLine(reads []jointRead, refusal Refusal) (intercept, slope float64, n int, refused Refusal) {
	if len(reads) < MinJointReadings {
		return 0, 0, len(reads), refusal
	}
	obs := make([]itl.Observation, len(reads))
	for i, r := range reads {
		obs[i] = itl.Observation{K: r.x, ITLSec: r.y}
	}
	m, ok := itl.Fit(obs)
	if !ok {
		return 0, 0, len(reads), refusal
	}
	return m.B, m.A, len(reads), ""
}

// medianOf is the median of f over reads (median); reads is not empty.
func medianOf(reads []jointRead, f func(jointRead) float64) float64 {
	return median(valuesOf(reads, f))
}

// valuesOf is f over reads.
func valuesOf(reads []jointRead, f func(jointRead) float64) []float64 {
	xs := make([]float64, len(reads))
	for i, r := range reads {
		xs[i] = f(r)
	}
	return xs
}

// quantileOf is the q quantile of the readings' x, linearly interpolated
// between the order statistics; reads is not empty.
func quantileOf(reads []jointRead, q float64) float64 {
	xs := valuesOf(reads, func(r jointRead) float64 { return r.x })
	slices.Sort(xs)
	pos := q * float64(len(xs)-1)
	lo := int(math.Floor(pos))
	hi := min(lo+1, len(xs)-1)
	return xs[lo] + (pos-float64(lo))*(xs[hi]-xs[lo])
}
