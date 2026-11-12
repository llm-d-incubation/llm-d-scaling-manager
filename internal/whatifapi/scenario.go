package whatifapi

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
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
)

// buckets are the fixed prompt buckets of v1, on the staircase's pattern
// (docs/proposals/capacity-planner.md, "Buckets"), each named by its upper
// edge.
var buckets = []Bucket{{"1k", 1024}, {"4k", 4096}, {"8k", 8192}, {"32k", 32768}, {"128k", 131072}}

// Buckets is the fixed prompt buckets, smallest first.
func Buckets() []Bucket { return slices.Clone(buckets) }

// BucketOf is the bucket a prompt falls in: the nearest at or above it, and
// the last for anything past it.
func BucketOf(tokens float64) string {
	for _, b := range buckets {
		if tokens <= float64(b.UpperTokens) {
			return b.Name
		}
	}
	return buckets[len(buckets)-1].Name
}

// inputErrorf is an InputError.
func inputErrorf(format string, args ...any) error {
	return InputError{Err: fmt.Errorf(format, args...)}
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
			return sc, inputErrorf("headroom %g, want a fraction of the knee in (0, 1]", *h)
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
	return sc, sc.bucketTargets(slo.TTFT)
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
			return inputErrorf("fleet.%s %d, want 1 to %d replicas", c.role, *c.n, maxReplicas)
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
				return inputErrorf("traffic.classes.%s.rate %g: v1 prices one class, the total, as critical; regular and batch need traffic split by priority", cl.name, *cr.Rate)
			}
		}
		if cr := c.Critical; cr != nil && cr.Rate != nil {
			if !usable(*cr.Rate) {
				return inputErrorf("traffic.classes.critical.rate %g, want a finite rate above 0", *cr.Rate)
			}
			sc.Rate = ptr(*cr.Rate)
		}
	}
	if m := t.Mix; m != nil && !m.Observed {
		for _, l := range []struct {
			name string
			x    float64
		}{{"promptTokens", m.PromptTokens}, {"outputTokens", m.OutputTokens}} {
			if l.x != 0 && !usable(l.x) {
				return inputErrorf("traffic.mix.%s %g, want a finite length above 0", l.name, l.x)
			}
		}
		sc.Mix = *m
		if m.PromptTokens == 0 && m.OutputTokens == 0 {
			sc.Mix = Mix{Observed: true}
		}
	}
	if a := t.ArrivalCV2; a != nil {
		if !usable(*a) {
			return inputErrorf("traffic.arrivalCV2 %g, want a finite value above 0: 1 is Poisson", *a)
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
		return inputErrorf("slo.ttft.target %g: want a target above 0 in seconds (the policy sets none)", target)
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
			return inputErrorf("slo.tpot: statistic none asks no TPOT, and takes no target")
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
		return inputErrorf("slo.tpot.target %g: want a target above 0 in seconds (the policy sets none)", target)
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
			return inputErrorf("assume.%s %g, want a finite value above 0", t.name, *t.x)
		}
	}
	for _, t := range []namedTerm{{"promptCV", a.PromptCV}, {"routerHop", a.RouterHop}} {
		if t.x != nil && (math.IsNaN(*t.x) || *t.x < 0 || math.IsInf(*t.x, 1)) {
			return inputErrorf("assume.%s %g, want a finite value of 0 or more", t.name, *t.x)
		}
	}
	if a.TPOTRatio != nil && (sc.TPOT == nil || sc.TPOT.Statistic == statisticMean) {
		return inputErrorf("assume.tpotRatio needs a TPOT percentile (slo.tpot.statistic pNN)")
	}
	return nil
}

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
	for _, name := range names {
		t := asked[name]
		if !slices.ContainsFunc(buckets, func(b Bucket) bool { return b.Name == name }) {
			return inputErrorf("slo.ttft.bucketTargets.%s: no such bucket; the buckets are %s", name, bucketNames())
		}
		if !usable(t) {
			return inputErrorf("slo.ttft.bucketTargets.%s %g, want a target above 0 in seconds", name, t)
		}
	}
	if !sc.Views.AllTrafficPerBucket {
		if len(asked) > 0 {
			return inputErrorf("slo.ttft.bucketTargets need views.allTrafficPerBucket: no limit is read per bucket")
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
func bucketNames() string {
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
	bad := inputErrorf("%s %q, want mean or a percentile from p1 to p99.9 (p90)", field, s)
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
