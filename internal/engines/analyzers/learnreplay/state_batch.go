package learnreplay

import (
	"math"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The ITL batch term's rows (saturation.SwitchITLBatchTerm, issue
// llm-d-incubation/llm-d-scaling-manager#309): each decode variant's rows
// inside the band, with the running sequences per engine rank the scrape
// read, kept while the switch is on and fitted at the cycle asked
// (whatif.FitBatchTerm) on the residuals of the line the variant was priced
// on. Read, not fed back: the analyzers never see the term.

// batchTermRows is the most rows kept per variant, the latest: about four
// hours of two pods at the default 15 s cycle.
const batchTermRows = 2000

// batchSamples is each variant's rows for the batch term; nil rows while the
// switch is off, so nothing is kept or fitted.
// ranks is the most engine ranks a variant's rows were split over
// (domain.ReplicaMetrics.Ranks): the term is priced per rank as its rows
// were read, not at the variant's configured data parallelism, which a row
// that did not read its ranks does not divide by.
type batchSamples struct {
	on    bool
	rows  map[string][]whatif.BatchRow
	ranks map[string]int
}

func newBatchSamples(on bool) *batchSamples {
	return &batchSamples{on: on, rows: map[string][]whatif.BatchRow{}, ranks: map[string]int{}}
}

// note reads one cycle: every serving row of a variant that is not prefill,
// at a fill inside the band, with a finite mean ITL and a running count read.
func (b *batchSamples) note(in domain.AnalyzerInput) {
	if !b.on {
		return
	}
	policy, _ := in.Config.(*config.ScalingPolicy)
	bottom, top := policy.ITLBand()
	roles := rolesOf(in)
	for _, rm := range in.ReplicaMetrics {
		if !prefill.Serving(rm) || domain.CanonicalRole(roles[rm.VariantName]) == domain.RolePrefill {
			continue
		}
		k := rm.KvUsageForITL()
		if !(k >= bottom && k <= top) || rm.ITLState() != domain.ITLFinite || !(rm.AvgITL > 0) || math.IsInf(rm.AvgITL, 0) ||
			!rm.RunningRequestsKnown || rm.RunningRequests <= 0 {
			continue
		}
		b.rows[rm.VariantName] = append(b.rows[rm.VariantName], whatif.BatchRow{K: k,
			SeqsPerRank: float64(rm.RunningRequests) / float64(rm.Ranks()), ITLSec: rm.AvgITL})
		b.ranks[rm.VariantName] = max(b.ranks[rm.VariantName], rm.Ranks())
		if rows := b.rows[rm.VariantName]; len(rows) > batchTermRows {
			b.rows[rm.VariantName] = rows[len(rows)-batchTermRows:]
		}
	}
}

// fit sets each decode variant's BatchFit in s from the rows noted so far:
// on the residuals of the line it was priced on, a sequence one batch token
// split over the engine ranks its rows read (ranks). A variant with no line, or a
// prefill one, keeps the zero fit, as does every variant while the switch is
// off; a speculating vLLM variant is refused (whatif.BatchTermSpeculative).
func (b *batchSamples) fit(s *State) {
	if !b.on {
		return
	}
	for i := range s.Variants {
		v := &s.Variants[i]
		if domain.CanonicalRole(v.Role) == domain.RolePrefill || v.Line.IsZero() {
			continue
		}
		// Only vLLM's speculative flags are read (SpeculatesPerStep): SGLang's
		// ITL is already per token.
		if v.Params.SpeculatesPerStep() {
			// The rows are the step's mean ITL (rm.AvgITL), as the replay's
			// line rows are, while the analyzer may fit the line per
			// accepted token (saturation.SwitchITLPerToken): residuals of
			// the one against the other are not the batch's cost.
			v.BatchFit = whatif.BatchFit{Rows: len(b.rows[v.Variant]), Why: whatif.BatchTermSpeculative}
			continue
		}
		v.BatchFit = whatif.FitBatchTerm(v.Line, b.rows[v.Variant], max(b.ranks[v.Variant], 1), 0)
	}
}
