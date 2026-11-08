package throughput

import (
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
)

// What the ITL line's admission reads (analyzer-learner.md, PR 4b): the
// policy's band, steady gate and cap preemption rate; each variant's engine
// parameters, for its stated sequence cap and the arguments a held slope was
// fitted under; the engine's settle tracker; and how long a slope may be
// held. The parent's window (SwitchITLSteadyGate off) reads only the band;
// the line on itl.Tracker reads the rest, as the saturation and slo
// analyzers' lines do.

// SwitchITLAdmitsAtSequenceCap is the replay switch for cap admission
// (analyzer-learner.md, PR 4b-3; inventory row 1's rule, #287, #293, #331).
// On: a reading above the band is one of the band's when its replica's
// batch is exactly at the cap the variant's engine states -- the flags'
// max_num_seqs, lowered by --max-num-active-seqs, else the
// llm-d.ai/max-num-seqs annotation (capacity.EngineParams.StatedBatchCap),
// never a default or a learned cap -- on every rank, with requests waiting
// and a preemption rate read and at most
// analyzerTuning.itlBand.capMaxPreemptionRate (itl.Admission.MaxNumSeqs).
// Such a reading enters the band's window, not the above-band fallback's,
// so a variant at its cap prices from the band's line; the fallback stays
// for a key with no fit and no slope it may hold. Until the key learns B,
// that line is priced one-sided at itl.DefaultKSat
// (itl.Admission.OneSidedAboveKSat): the readings at the cap lie above the
// k it is priced at, where a fit through the default B prices under the
// truth when the true B is higher. Off, as before: nothing
// above the band is the band's. Meaningful only with SwitchITLSteadyGate on:
// the parent's window reads the band alone.
const SwitchITLAdmitsAtSequenceCap = "throughput-itl-admits-at-sequence-cap"

// batchCapNone is where an admission's sequence cap came from when it has
// none: the value the slo analyzer logs too (itl.MaxNumSeqsSourceNone).
// Nothing above the band is admitted.
const batchCapNone = itl.MaxNumSeqsSourceNone

// itlPolicy is what the ITL line reads of a model's policy.
type itlPolicy struct {
	// lo and hi are the band (analyzerTuning.itlBand).
	lo, hi float64
	// steady is the steady-state gate (analyzerTuning.steadyGate).
	steady itl.Steadiness
	// capPreemption is analyzerTuning.itlBand.capMaxPreemptionRate.
	capPreemption float64
}

// itlPolicyOf reads cfg's ITL keys through itl.PolicyReader:
// internal/config's tests import this package, so importing config here
// would be a cycle. *config.ScalingPolicy implements it, nil included, and
// config asserts that at compile time, so a renamed method fails the build
// rather than reading as the defaults here. A config that does not
// implement it (none in production) reads as the defaults: the default
// band, a zero gate (itl.Steadiness's defaults) and no preemption at the cap.
func itlPolicyOf(cfg domain.AnalyzerConfig) itlPolicy {
	r, ok := cfg.(itl.PolicyReader)
	if !ok {
		return itlPolicy{lo: itl.DefaultMinObservableK, hi: itl.DefaultMaxObservableK}
	}
	p := itlPolicy{steady: r.ITLSteadiness(), capPreemption: r.ITLCapPreemptionRate()}
	p.lo, p.hi = r.ITLBand()
	return p
}

// admission is the model's ITL admission under policy p: the policy's keys,
// the engine's settle tracker with its absence tolerance at the optimize
// interval, and the slope hold. What only the capacity store gives is read
// per variant (engineOf) and set per variant (lineAdmission): the arguments
// a held slope was fitted under (ArgsTag, as the saturation analyzer tags
// its windows), since 4b-1, and the stated sequence cap, since 4b-3. It
// costs no lookup.
func (a *ThroughputAnalyzer) admission(p itlPolicy, namespace, modelID string) itl.Admission {
	adm := itl.Admission{
		MinK: p.lo, MaxK: p.hi,
		MaxNumSeqsSource:     batchCapNone,
		CapMaxPreemptionRate: p.capPreemption,
		Steady:               p.steady,
		SlopeHoldFor:         a.slopeHoldFor,
	}
	if a.settle != nil {
		adm.Settle = &itl.Settle{View: a.settle, Model: utils.GetNamespacedKey(namespace, modelID)}
	}
	if a.settleInterval > 0 {
		adm.AbsenceTolerance = settle.Tolerance(a.settleInterval)
	}
	return adm
}

// variantEngine is what the line's admission reads of one variant's engine
// parameters in the capacity store (engineOf): the arguments its rows were
// produced under, and the batch cap one engine rank states, with where it
// came from; and whether it is rolling out. The zero value is a variant the
// store does not know: no arguments and no cap, which admits nothing above
// the band.
type variantEngine struct {
	// args is capacity.EngineParams.ArgsTag.
	args string
	// batchCap is the stated cap per rank (capacity.EngineParams.StatedBatchCap)
	// and batchCapSource batchCapFlags or batchCapDeclared; zero and
	// batchCapNone when nothing states one.
	batchCap       int64
	batchCapSource string
	// rollout is capacity.Record.InRollout, or the variant's rows reporting
	// more than one revision (domain.RevisionsMixed): a reading may be an old
	// pod's.
	rollout bool
}

// Where a stated sequence cap came from, as the slo and saturation analyzers
// log it: the engine's flags, or the llm-d.ai/max-num-seqs annotation.
const (
	batchCapFlags    = "flags"
	batchCapDeclared = "declared"
)

// engineOf reads the capacity store once for each variant with a row: what
// the line's admission takes of its engine parameters (variantEngine). It
// runs before observe takes a.mu and takes only the store's lock, so the
// cycle never holds the two at once. A variant the store does not know, or
// every variant when the analyzer has no store, reads as no arguments and
// no cap, and rolling out only if its rows say so.
func (a *ThroughputAnalyzer) engineOf(namespace, modelID string, rows []domain.ReplicaMetrics) map[string]variantEngine {
	out := make(map[string]variantEngine)
	mixed := domain.RevisionsMixed(rows)
	for _, m := range rows {
		if _, done := out[m.VariantName]; done {
			continue
		}
		e := variantEngine{batchCapSource: batchCapNone, rollout: mixed[m.VariantName]}
		if a.store != nil {
			if rec := a.store.Get(namespace, modelID, m.VariantName); rec != nil {
				e.rollout = e.rollout || rec.InRollout()
				p := rec.EngineParams
				e.args = p.ArgsTag()
				if c := p.StatedBatchCap(); c > 0 {
					e.batchCap, e.batchCapSource = c, batchCapDeclared
					if p.ExplicitMaxNumSeqs() > 0 {
						e.batchCapSource = batchCapFlags
					}
				}
			}
		}
		out[m.VariantName] = e
	}
	return out
}

// WithSettleView sets the settle tracker the ITL line reads each replica's
// settle from: the engine's, which it observes at the top of every cycle.
// Call it before the analyzer is used. Unset, the admission carries no
// settle tracker, and the line reads a tracker the analyzer observes itself
// (settle.go), as one that has watched the fleet: nothing waits after a
// restart.
func (a *ThroughputAnalyzer) WithSettleView(v settle.View) *ThroughputAnalyzer {
	a.settle = v
	return a
}

// WithOptimizationInterval sets the controller's optimize interval, which
// sets how long a replica may go without a row and keep its settle
// (settle.Tolerance), on the key's own trace and on the tracker the analyzer
// observes itself when the engine gave it none. Call it before the analyzer
// is used. Unset, the tolerance is the least, settle.Period.
func (a *ThroughputAnalyzer) WithOptimizationInterval(d time.Duration) *ThroughputAnalyzer {
	a.settleInterval = d
	a.ownSettle.SetInterval(d)
	return a
}

// WithSlopeHoldFor sets how long after it was fitted the ITL line's last
// slope may be held when its window cannot fit one: the controller's bucket
// retention, as for the other lines. Call it before the analyzer is used.
// Unset, a held slope has no age limit (itl.Admission.SlopeHoldFor).
func (a *ThroughputAnalyzer) WithSlopeHoldFor(d time.Duration) *ThroughputAnalyzer {
	a.slopeHoldFor = d
	return a
}

// WithCapacityStore sets the capacity store the analyzer reads each
// variant's engine parameters from. It reads them only, and writes nothing.
// Call it before the analyzer is used. Unset, no variant has a stated
// sequence cap or known engine arguments: nothing above the band is
// admitted, and a held slope is not bounded by the arguments.
func (a *ThroughputAnalyzer) WithCapacityStore(s *capacity.Store) *ThroughputAnalyzer {
	a.store = s
	return a
}

// StartConfig is what the builders set, for a recording's first line: a
// replay builds the analyzer from it.
func (a *ThroughputAnalyzer) StartConfig() learnrec.ThroughputStart {
	return learnrec.ThroughputStart{
		SlopeHoldFor:         a.slopeHoldFor,
		OptimizationInterval: a.settleInterval,
		SettleView:           a.settle != nil,
		CapacityStore:        a.store != nil,
	}
}
