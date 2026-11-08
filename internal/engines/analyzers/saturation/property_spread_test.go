package saturation

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// Variable output lengths (ev-shindin/llm-scaling-manager#276): a long request stays resident longer, so
// the KV a resident request holds is IL + E[O^2]/(2 E[O]), not IL + E[O]/2.
// The k2 the batch cap admits and the sequences the derived mu fits at
// kPrice are priced from it.

func spreadCases() []propCase {
	return []propCase{
		{class: classKnownTruth, learner: "k2", name: "output-spread", family: familyDirect, tuning: tunedDefault,
			check: checkK2OutputSpreadOn(nil)},
		{class: classKnownTruth, learner: "mu", name: "output-spread", family: familyDirect, tuning: tunedDefault,
			check: checkMuOutputSpreadOn(nil)},
	}
}

// vllmLengthBounds are vLLM's request_generation_tokens bucket bounds
// (1-2-5), up to a 50k context.
var vllmLengthBounds = []float64{1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000, 10000, 20000, 50000}

// spreadWorkload is a bimodal workload: short requests and long ones, at
// lengths that sit on vLLM's bucket bounds, the long ones a fraction p of
// the requests, behind a prompt of il tokens.
type spreadWorkload struct {
	il, short, long, p float64
}

// newSpreadWorkload draws a workload for seed. The lengths sit on bucket
// bounds, where the histogram's bound on E[O^2] is exact, so the case
// judges the pricing and not the histogram's resolution; that the bound is
// never above the truth is domain's unit test (TestSquaredMeanLowerBound).
func newSpreadWorkload(r *rand.Rand) spreadWorkload {
	shorts := []float64{50, 100, 200}
	longs := []float64{1000, 2000, 5000}
	return spreadWorkload{
		il:    500 + 2500*r.Float64(),
		short: shorts[r.IntN(len(shorts))],
		long:  longs[r.IntN(len(longs))],
		p:     0.1 + 0.3*r.Float64(),
	}
}

// replicaLengths draws n completed requests' output lengths.
func (w spreadWorkload) replicaLengths(r *rand.Rand, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = w.short
		if r.Float64() < w.p {
			out[i] = w.long
		}
	}
	return out
}

// spreadRow is one decode replica of variant v that completed lengths, as
// the collector reads it: the mean, and the bound on E[O^2] from the
// histogram (domain.SquaredMeanLowerBound), one request a second.
func spreadRow(pod string, w spreadWorkload, lengths []float64, kvTokens int64) domain.ReplicaMetrics {
	var sum float64
	for _, x := range lengths {
		sum += x
	}
	mean := sum / float64(len(lengths))
	rm := makeReplicaMetrics(pod, "v", kvTokens/20, kvTokens, 0, w.il, mean)
	rm.KvUsageInstant, rm.KvUsageInstantKnown = rm.KvCacheUsage, true
	rm.RequestRate = 1
	buckets := make([]domain.LengthBucket, 0, len(vllmLengthBounds)+1)
	for _, b := range append(append([]float64{}, vllmLengthBounds...), math.Inf(1)) {
		var n float64
		for _, x := range lengths {
			if x <= b {
				n++
			}
		}
		buckets = append(buckets, domain.LengthBucket{UpperBound: b, Rate: n / 300})
	}
	if sq, ok := domain.SquaredMeanLowerBound(buckets, mean); ok {
		rm.AvgOutputTokensSquared, rm.AvgOutputTokensSquaredKnown = sq, true
	}
	return rm
}

// residentKV is the KV a resident request of these lengths holds, averaged
// over the time each is resident: IL + E[O^2]/(2 E[O]).
func residentKV(il float64, lengths []float64) float64 {
	var s, sq float64
	for _, x := range lengths {
		s += x
		sq += x * x
	}
	return il + sq/(2*s)
}

// spreadRig is a rig for variant v whose engine is params, logging at the
// per-replica level into the observer it returns.
func spreadRig(params *capacity.EngineParams, edit func(*SaturationAnalyzer)) (*propRig, *observer.ObservedLogs) {
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "v", capacity.Record{AcceleratorName: "H200", GpuCount: 1,
		EngineParams: params}, propStart)
	rig := newPropRig()
	rig.a = NewSaturationAnalyzer(store)
	rig.a.now = func() time.Time { return rig.clock }
	if edit != nil {
		edit(rig.a)
	}
	core, logs := observer.New(zapcore.Level(-perReplicaLogLevel))
	rig.ctx = logr.NewContext(context.Background(), zapr.NewLogger(zap.New(core)))
	return rig, logs
}

// checkK2OutputSpreadOn: one idle decode replica of a bimodal workload under
// the parser's default engine (256 sequences, 8192 batched tokens), so the
// chain reaches P3 and the batch cap binds. The derived k2 is the KV those
// 256 sequences hold, 256 x (IL + E[O^2]/(2 E[O])), within propTruthTol.
// IL + E[O]/2 prices them low, and a replica that holds more before its
// batch binds than k2 says is ordered replicas it does not need. edit puts
// a defect back.
func checkK2OutputSpreadOn(edit func(*SaturationAnalyzer)) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		w := newSpreadWorkload(r)
		lengths := w.replicaLengths(r, 400)
		rig, logs := spreadRig(deploymentParams(), edit)
		if _, err := rig.analyze([]domain.ReplicaMetrics{spreadRow("r0", w, lengths, 4_000_000)}, decodeStates(1), 1, propCycle); err != nil {
			return err
		}
		got := math.NaN()
		for _, e := range logs.FilterMessage("k2-decision").All() {
			if f := e.ContextMap(); f["priority"] == capacity.K2SrcDerived.String() {
				if k, ok := f["k2"].(int64); ok {
					got = float64(k)
				}
			}
		}
		if math.IsNaN(got) {
			return violation(kindNotLearned, "no derived k2 priced")
		}
		want := 256 * residentKV(w.il, lengths)
		return holds(relErr(got, want) <= propTruthTol, kindBiased,
			"%s: derived k2 %.0f against 256 sequences holding %.0f (%+.1f%%)", w, got, want, 100*(got/want-1))
	}
}

// checkMuOutputSpreadOn: four decode replicas of a bimodal workload, each
// with its own draw of 400 lengths, and a fitted line. The engine's cap
// (1024 a rank, declared) does not bind, so the derived mu fits kPrice x C /
// KVreq sequences, and KVreq must be the KV a resident request of the
// fleet's lengths holds: the sequences are within propTruthTol of kPrice x C
// / (IL + E[O^2]/(2 E[O])). IL + E[O]/2 over-counts them, over-states mu and
// under-orders. edit puts a defect back.
func checkMuOutputSpreadOn(edit func(*SaturationAnalyzer)) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		w := newSpreadWorkload(r)
		const kv = int64(400_000)
		params := deploymentParams()
		params.DeclaredBatchCap = 1024
		rig, logs := spreadRig(params, edit)
		rig.a.publishLine(rig.a.itlPhysicsKey("test-ns", "test-model", "v", "H200", 1, ""),
			"a fitted line", itl.Model{A: 0.08, B: 0.01}, propStart)
		rows := make([]domain.ReplicaMetrics, 0, 4)
		all := make([]float64, 0, 4*400)
		for i := range 4 {
			lengths := w.replicaLengths(r, 400)
			all = append(all, lengths...)
			rows = append(rows, spreadRow(fmt.Sprintf("r%d", i), w, lengths, kv))
		}
		if _, err := rig.analyze(rows, decodeStates(4), 4, propCycle); err != nil {
			return err
		}
		lines := logs.FilterMessage("derived-mu").All()
		if len(lines) == 0 {
			return violation(kindNotLearned, "no derived mu priced")
		}
		f := lines[0].ContextMap()
		seqs, ok1 := f["seqs"].(float64)
		kPrice, ok2 := f["kPrice"].(float64)
		if !ok1 || !ok2 || f["ok"] != true {
			return violation(kindNotLearned, "the derived mu priced nothing: %v", f)
		}
		want := kPrice * float64(kv) / residentKV(w.il, all)
		if seqs > want*(1+propTruthTol) {
			return violation(kindOverPriced, "%s: %.1f sequences at k %.2f against %.1f resident (%+.1f%%): mu over-stated",
				w, seqs, kPrice, want, 100*(seqs/want-1))
		}
		return holds(seqs >= want*(1-propTruthTol), kindBiased,
			"%s: %.1f sequences at k %.2f against %.1f resident (%+.1f%%)", w, seqs, kPrice, want, 100*(seqs/want-1))
	}
}

// String names the workload in a violation.
func (w spreadWorkload) String() string {
	return fmt.Sprintf("IL %.0f, %.0f or %.0f out (%.0f%% long)", w.il, w.short, w.long, 100*w.p)
}
