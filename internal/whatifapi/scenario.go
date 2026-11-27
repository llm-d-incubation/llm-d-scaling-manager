package whatifapi

import (
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The scenario's ranges.
const (
	// maxReplicas is the most replicas of a role a scenario prices.
	maxReplicas = 64
	// statisticMean is the mean, as the SLO metrics' statistic label says it.
	statisticMean = "mean"
	// statisticNone asks no TPOT.
	statisticNone = "none"
	// defaultTTFTStatistic is the TTFT statistic with none in the request or
	// the policy (config.DefaultTTFTPercentile).
	defaultTTFTStatistic = "p90"
	// maxBucketEdges is the most bucket edges a scenario sets. Each edge not
	// cached prices a fleet's curves at five corners of V_P and f, holding a
	// plan slot throughout: on the worked recording 12 fresh edges took 16 to
	// 19 s against 7.7 s for the default five, so the cap stays near them.
	maxBucketEdges = 8
	// maxBucketEdge is the largest bucket edge a scenario sets, in tokens:
	// 1024k, past any context window served today.
	maxBucketEdge = 1 << 20
	// tokensPerK is the tokens in a "k" of a bucket's name.
	tokensPerK = 1024
)

// defaultBuckets are the default prompt buckets, the fixed set of v1, on the
// staircase's pattern (docs/proposals/capacity-planner.md, "Buckets"), each
// named by its upper edge (bucketName).
var defaultBuckets = []Bucket{{"1k", 1024}, {"4k", 4096}, {"8k", 8192}, {"32k", 32768}, {"128k", 131072}}

// Buckets is the default prompt buckets, smallest first: a scenario's own
// edges (PlanRequest.BucketEdges) replace them on its plan alone.
func Buckets() []Bucket { return slices.Clone(defaultBuckets) }

// BucketOf is the default bucket a prompt falls in: the nearest at or above
// it, and the last for anything past it.
func BucketOf(tokens float64) string {
	for _, b := range defaultBuckets {
		if tokens <= float64(b.UpperTokens) {
			return b.Name
		}
	}
	return defaultBuckets[len(defaultBuckets)-1].Name
}

// bucketName is the name of the bucket whose upper edge is tokens: the edge
// in k (1024 tokens) where it is a whole number of them, else in tokens. So
// 8192 is "8k" and 8000 is "8000", and no two edges share a name.
func bucketName(tokens int) string {
	if tokens%tokensPerK == 0 {
		return strconv.Itoa(tokens/tokensPerK) + "k"
	}
	return strconv.Itoa(tokens)
}

// defaultEdges is the default buckets' upper edges.
func defaultEdges() []int {
	edges := make([]int, len(defaultBuckets))
	for i, b := range defaultBuckets {
		edges[i] = b.UpperTokens
	}
	return edges
}

// usable reports whether x is finite and above 0.
func usable(x float64) bool { return x > 0 && !math.IsInf(x, 1) }

// scenarioOf is req with in's defaults applied, checked: an InputError names
// a field asked wrongly.
func scenarioOf(in *InputsResponse, req PlanRequest) (Scenario, error) {
	sc := Scenario{InputsID: in.InputsID, Mix: Mix{Observed: true}, Assume: Assume{}}
	if err := sc.fleet(in, req.Fleet); err != nil {
		return sc, err
	}
	if err := sc.traffic(req.Traffic); err != nil {
		return sc, err
	}
	var slo SLORequest
	if req.SLO != nil {
		slo = *req.SLO
	}
	if err := sc.ttft(in, slo.TTFT); err != nil {
		return sc, err
	}
	if err := sc.tpot(in, slo.TPOT); err != nil {
		return sc, err
	}
	sc.Headroom = in.Policy.Headroom
	if h := req.Headroom; h != nil {
		if !(*h > 0 && *h <= 1) {
			return sc, InputErrorf("headroom %g, want a fraction of the knee in (0, 1]", *h)
		}
		sc.Headroom = *h
	}
	if req.Assume != nil {
		sc.Assume = *req.Assume
	}
	if err := sc.checkAssume(); err != nil {
		return sc, err
	}
	if req.Views != nil {
		sc.Views = *req.Views
	}
	if err := sc.bucketEdges(req.BucketEdges); err != nil {
		return sc, err
	}
	return sc, sc.bucketTargets(slo.TTFT)
}

// bucketEdges sets the scenario's bucket edges: the request's, checked, else
// the default. Edges equal to the default are left out of the scenario, so
// that they price, name and cache as no edges do. The learned inputs hold
// nothing per bucket (the observed mix is refused, NoPerRequestLengths): the
// edges move only where the "if all traffic were bucket b" lines are priced,
// each at its upper edge, so no learned figure is re-bucketed or matched to
// them.
func (sc *Scenario) bucketEdges(edges []int) error {
	if edges == nil {
		return nil
	}
	if len(edges) == 0 || len(edges) > maxBucketEdges {
		return InputErrorf("bucketEdges: %d edges, want 1 to %d upper edges in tokens, ascending", len(edges), maxBucketEdges)
	}
	for i, e := range edges {
		if e < 1 || e > maxBucketEdge {
			return InputErrorf("bucketEdges[%d] %d, want an upper edge from 1 to %d tokens", i, e, maxBucketEdge)
		}
		if i > 0 && e <= edges[i-1] {
			return InputErrorf("bucketEdges[%d] %d is not above bucketEdges[%d] %d: want the edges ascending, each once", i, e, i-1, edges[i-1])
		}
	}
	if !sc.Views.AllTrafficPerBucket {
		return InputErrorf("bucketEdges need views.allTrafficPerBucket: the edges shape only that view, and no limit is read per bucket")
	}
	if !slices.Equal(edges, defaultEdges()) {
		sc.BucketEdges = slices.Clone(edges)
	}
	return nil
}

// buckets is the scenario's prompt buckets, smallest first: its edges', else
// the default.
func (sc *Scenario) buckets() []Bucket {
	if sc.BucketEdges == nil {
		return defaultBuckets
	}
	out := make([]Bucket, len(sc.BucketEdges))
	for i, e := range sc.BucketEdges {
		out[i] = Bucket{Name: bucketName(e), UpperTokens: e}
	}
	return out
}

// fleet sets the replica counts: the request's, else the recorded ones.
func (sc *Scenario) fleet(in *InputsResponse, k *FleetKnob) error {
	sc.Fleet.Decode = in.Fleet.Decode.Replicas
	if in.Fleet.Prefill != nil {
		sc.Fleet.Prefill = in.Fleet.Prefill.Replicas
	}
	if k == nil {
		return nil
	}
	for _, c := range []struct {
		role string
		n    *int
		into *int
	}{{"prefill", k.Prefill, &sc.Fleet.Prefill}, {"decode", k.Decode, &sc.Fleet.Decode}} {
		if c.n == nil {
			continue
		}
		if *c.n < 1 || *c.n > maxReplicas {
			return InputErrorf("fleet.%s %d, want 1 to %d replicas", c.role, *c.n, maxReplicas)
		}
		*c.into = *c.n
	}
	return nil
}

// traffic sets the rate, the mix and the arrivals. Regular and batch are not
// built: the critical rate is the total.
func (sc *Scenario) traffic(t *Traffic) error {
	if t == nil {
		return nil
	}
	if c := t.Classes; c != nil {
		for _, cl := range []struct {
			name string
			cr   *ClassRate
		}{{"regular", c.Regular}, {"batch", c.Batch}} {
			if cr := cl.cr; cr != nil && cr.Rate != nil && *cr.Rate != 0 {
				return InputErrorf("traffic.classes.%s.rate %g: v1 prices one class, the total, as critical; regular and batch need traffic split by priority", cl.name, *cr.Rate)
			}
			if cr := cl.cr; cr != nil && cr.Concurrency != nil && *cr.Concurrency != 0 {
				return InputErrorf("traffic.classes.%s.concurrency %g: v1 prices one class, the total, as critical", cl.name, *cr.Concurrency)
			}
		}
		if cr := c.Critical; cr != nil && cr.Rate != nil && cr.Concurrency != nil {
			return InputErrorf("traffic.classes.critical: give a rate or a concurrency, not both")
		}
		if cr := c.Critical; cr != nil && cr.Rate != nil {
			if !usable(*cr.Rate) {
				return InputErrorf("traffic.classes.critical.rate %g, want a finite rate above 0", *cr.Rate)
			}
			sc.Rate = ptr(*cr.Rate)
		}
		if cr := c.Critical; cr != nil && cr.Concurrency != nil {
			if x := *cr.Concurrency; !usable(x) || x > MaxConcurrency {
				return InputErrorf("traffic.classes.critical.concurrency %g, want requests in the system above 0 and at most %d", x, MaxConcurrency)
			}
			sc.Concurrency = ptr(*cr.Concurrency)
		}
	}
	if m := t.Mix; m != nil && !m.Observed {
		for _, l := range []struct {
			name string
			x    float64
		}{{"promptTokens", m.PromptTokens}, {"outputTokens", m.OutputTokens}} {
			if l.x != 0 && !usable(l.x) {
				return InputErrorf("traffic.mix.%s %g, want a finite length above 0", l.name, l.x)
			}
		}
		sc.Mix = *m
		if m.PromptTokens == 0 && m.OutputTokens == 0 {
			sc.Mix = Mix{Observed: true}
		}
	}
	if a := t.ArrivalCV2; a != nil {
		if !usable(*a) {
			return InputErrorf("traffic.arrivalCV2 %g, want a finite value above 0: 1 is Poisson", *a)
		}
		sc.ArrivalCV2 = ptr(*a)
	}
	return nil
}

// ttft sets the TTFT objective: the request's, else the policy's. A TTFT
// target is required: the prefill limit is read at it.
func (sc *Scenario) ttft(in *InputsResponse, o *Objective) error {
	stat, target := defaultTTFTStatistic, 0.0
	if p := in.Policy.TTFT; p != nil {
		stat, target = p.Statistic, p.Seconds
	}
	if o != nil && o.Statistic != "" {
		stat = o.Statistic
	}
	if o != nil && o.Target != nil {
		target = *o.Target
	}
	q, err := parseStatistic("slo.ttft.statistic", stat)
	if err != nil {
		return err
	}
	if !usable(target) {
		return InputErrorf("slo.ttft.target %g: want a target above 0 in seconds (the policy sets none)", target)
	}
	sc.TTFT = ResolvedObjective{Statistic: statisticName(q), Target: target}
	return nil
}

// tpot sets the TPOT objective: the request's, else the policy's, which is a
// mean. With neither, or with the statistic "none", TPOT is not asked, and
// decode has no limit of its own (as slo-to-rate with --ttft alone).
func (sc *Scenario) tpot(in *InputsResponse, o *Objective) error {
	if o != nil && o.Statistic == statisticNone {
		if o.Target != nil {
			return InputErrorf("slo.tpot: statistic none asks no TPOT, and takes no target")
		}
		return nil
	}
	stat, target := statisticMean, 0.0
	if p := in.Policy.TPOT; p != nil {
		stat, target = p.Statistic, p.Seconds
	}
	if o != nil && o.Statistic != "" {
		stat = o.Statistic
	}
	if o != nil && o.Target != nil {
		target = *o.Target
	}
	if o == nil && in.Policy.TPOT == nil {
		return nil
	}
	q, err := parseStatistic("slo.tpot.statistic", stat)
	if err != nil {
		return err
	}
	if !usable(target) {
		return InputErrorf("slo.tpot.target %g: want a target above 0 in seconds (the policy sets none)", target)
	}
	sc.TPOT = &ResolvedObjective{Statistic: statisticName(q), Target: target}
	return nil
}

// namedTerm is an assumed term and its field's name, in the order checked.
type namedTerm struct {
	name string
	x    *float64
}

// checkAssume refuses an assumed term no CLI flag would take. The CLI's own
// checks (Pricer.Fleet) refuse the rest.
func (sc *Scenario) checkAssume() error {
	a := sc.Assume
	for _, t := range []namedTerm{{"vp", a.VP}, {"a", a.A}, {"tpotRatio", a.TPOTRatio}} {
		if t.x != nil && !usable(*t.x) {
			return InputErrorf("assume.%s %g, want a finite value above 0", t.name, *t.x)
		}
	}
	for _, t := range []namedTerm{{"promptCV", a.PromptCV}, {"routerHop", a.RouterHop}} {
		if t.x != nil && (math.IsNaN(*t.x) || *t.x < 0 || math.IsInf(*t.x, 1)) {
			return InputErrorf("assume.%s %g, want a finite value of 0 or more", t.name, *t.x)
		}
	}
	if a.PromptCV != nil && *a.PromptCV > whatif.PromptCVStop {
		return InputErrorf("assume.promptCV %g, want a cv from 0 to %g", *a.PromptCV, whatif.PromptCVStop)
	}
	if a.TPOTRatio != nil && (sc.TPOT == nil || sc.TPOT.Statistic == statisticMean) {
		return InputErrorf("assume.tpotRatio needs a TPOT percentile (slo.tpot.statistic pNN)")
	}
	if err := a.CheckFlowControl(); err != nil {
		return err
	}
	switch {
	case a.ITLB != nil && a.ITLBFromFloor:
		return InputErrorf("assume.itlB and assume.itlBFromFloor both state B; give one")
	case a.ITLB != nil && !(usable(*a.ITLB) && *a.ITLB >= MinAssumedITLB && *a.ITLB <= MaxAssumedITLB):
		return InputErrorf("assume.itlB %g, want seconds from %g to %g", *a.ITLB, MinAssumedITLB, MaxAssumedITLB)
	}
	return nil
}

// MaxAssumedITLB is the largest intercept B, in seconds, assume.itlB takes
// (the CLI's --assume-itl-b takes the same): a token a second with the cache
// empty is no engine's floor.
const MaxAssumedITLB = 1.0

// MinAssumedITLB is the smallest intercept B, in seconds, assume.itlB takes:
// a microsecond, under any engine's step and well above the CLI's nanosecond
// rounding.
const MinAssumedITLB = 1e-6

// bucketTargets sets a target per bucket on the "if all traffic were bucket
// b" view: the request's, else the mix target.
func (sc *Scenario) bucketTargets(o *Objective) error {
	var asked map[string]float64
	if o != nil {
		asked = o.BucketTargets
	}
	names := make([]string, 0, len(asked))
	for name := range asked {
		names = append(names, name)
	}
	slices.Sort(names)
	buckets := sc.buckets()
	for _, name := range names {
		t := asked[name]
		if !slices.ContainsFunc(buckets, func(b Bucket) bool { return b.Name == name }) {
			return InputErrorf("slo.ttft.bucketTargets.%s: no such bucket; the buckets are %s", name, bucketNames(buckets))
		}
		if !usable(t) {
			return InputErrorf("slo.ttft.bucketTargets.%s %g, want a target above 0 in seconds", name, t)
		}
	}
	if !sc.Views.AllTrafficPerBucket {
		if len(asked) > 0 {
			return InputErrorf("slo.ttft.bucketTargets need views.allTrafficPerBucket: no limit is read per bucket")
		}
		return nil
	}
	sc.TTFT.BucketTargets = map[string]float64{}
	for _, b := range buckets {
		sc.TTFT.BucketTargets[b.Name] = sc.TTFT.Target
		if t, ok := asked[b.Name]; ok {
			sc.TTFT.BucketTargets[b.Name] = t
		}
	}
	return nil
}

// bucketNames is every bucket's name, in order.
func bucketNames(buckets []Bucket) string {
	names := make([]string, len(buckets))
	for i, b := range buckets {
		names[i] = b.Name
	}
	return strings.Join(names, ", ")
}

// percentileForm is a percentile as a statistic writes it: "p", one or two
// digits, and at most one decimal.
var percentileForm = regexp.MustCompile(`^p[0-9]{1,2}(\.[0-9])?$`)

// parseStatistic reads "mean" as 0 and "pNN" or "pNN.N" as q in (0, 1). Any
// other spelling (an exponent, a hex float, more decimals) is refused, so that
// no percentile rounds to p0 or p100 when it is named again
// (statisticName).
func parseStatistic(field, s string) (float64, error) {
	if s == statisticMean {
		return 0, nil
	}
	bad := InputErrorf("%s %q, want mean or a percentile from p1 to p99.9 (p90)", field, s)
	if !percentileForm.MatchString(s) {
		return 0, bad
	}
	p, err := strconv.ParseFloat(s[1:], 64)
	if err != nil || !(p >= 1 && p < 100) {
		return 0, bad
	}
	return p / 100, nil
}

// statisticName is q as a statistic: "mean", or "p" and the percentile.
func statisticName(q float64) string {
	if q == 0 {
		return statisticMean
	}
	return "p" + strconv.FormatFloat(math.Round(1e8*q)/1e6, 'f', -1, 64)
}

// knobs is the fleet the scenario prices, at the mix's lengths. A statistic
// that does not read back is an error, never the mean.
func (sc *Scenario) knobs() (Knobs, error) {
	k := Knobs{Prefill: sc.Fleet.Prefill, Decode: sc.Fleet.Decode, Assume: sc.Assume, ArrivalCV2: sc.ArrivalCV2,
		Headroom: sc.Headroom}
	var err error
	if k.TTFTPercentile, err = parseStatistic("slo.ttft.statistic", sc.TTFT.Statistic); err != nil {
		return k, err
	}
	if sc.TPOT != nil {
		if k.TPOTPercentile, err = parseStatistic("slo.tpot.statistic", sc.TPOT.Statistic); err != nil {
			return k, err
		}
	}
	if !sc.Mix.Observed {
		k.PromptTokens, k.OutputTokens = sc.Mix.PromptTokens, sc.Mix.OutputTokens
	}
	return k, nil
}
