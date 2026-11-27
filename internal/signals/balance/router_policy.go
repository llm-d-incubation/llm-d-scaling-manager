package balance

import (
	"cmp"
	"math"
	"slices"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// RouterVerdict is one side of the router's policy as its series read it.
type RouterVerdict string

const (
	// RouterYes is a policy side the series show.
	RouterYes RouterVerdict = "yes"
	// RouterNo is a policy side the series show the router does not have.
	RouterNo RouterVerdict = "no"
	// RouterNotDetected is a side the series are missing for or do not
	// decide.
	RouterNotDetected RouterVerdict = "not detected"
)

// The policies DetectRouterPolicy names, from its two verdicts.
const (
	// PolicyCacheAndLoadAware is a router that scores the prefix cache and
	// the load.
	PolicyCacheAndLoadAware = "cache-aware and load-aware"
	// PolicyCacheAware is a router that scores the prefix cache and not the
	// load.
	PolicyCacheAware = "cache-aware"
	// PolicyLoadAware is a router that scores the load and not the prefix
	// cache.
	PolicyLoadAware = "load-aware"
	// PolicyNeither is a router that scores neither.
	PolicyNeither = "neither"
	// PolicyNotDetected is a window that decides neither side.
	PolicyNotDetected = "not detected"
	// PolicyCacheAwareLoadNotDetected is a cache-aware router whose load
	// side is not detected.
	PolicyCacheAwareLoadNotDetected = "cache-aware; load not detected"
	// PolicyCacheBlindLoadNotDetected is a router that does not score the
	// prefix cache, its load side not detected.
	PolicyCacheBlindLoadNotDetected = "not cache-aware; load not detected"
	// PolicyLoadAwareCacheNotDetected is a load-aware router whose cache
	// side is not detected.
	PolicyLoadAwareCacheNotDetected = "load-aware; cache not detected"
	// PolicyLoadBlindCacheNotDetected is a router that does not score the
	// load, its cache side not detected.
	PolicyLoadBlindCacheNotDetected = "not load-aware; cache not detected"
)

// Why a verdict is not detected.
const (
	routerPolicyReasonNoSeries         = "no router prefix or per-endpoint series"
	routerPolicyReasonOneCandidate     = "one candidate endpoint: every placement keeps the whole hit"
	routerPolicyReasonShortWindow      = "too few cycles or too short a window"
	routerPolicyReasonProducerChanged  = "the prefix producer read changed within the window"
	routerPolicyReasonBetween          = "kept share between the bounds"
	routerPolicyReasonNoLoadRange      = "the load did not range widely enough"
	routerPolicyReasonFlat             = "kept share did not fall with the load"
	routerPolicyReasonNotBothHalves    = "the fall with the load did not hold in both halves of the window"
	routerPolicyReasonNoSpread         = "placements and in-flight requests both even, or spread alike"
	routerPolicyReasonMixedSpread      = "the cycles' spreads disagree"
	routerPolicyReasonSpreadNoEndpoint = "no per-endpoint placements with in-flight requests"
)

const (
	// RouterPolicyWindowCycles is the most cycles a RouterPolicyWindow
	// keeps: twelve hours at the controller's default 15 s cycle.
	RouterPolicyWindowCycles = 2880
	// RouterPolicyMaxAge is the oldest a cycle a RouterPolicyWindow keeps
	// may be, behind the newest: after a router reconfiguration the policy
	// reads the old configuration's cycles beside the new ones for up to
	// this long.
	RouterPolicyMaxAge = 12 * time.Hour
	// RouterPolicyStaleAfter is how long a window reads no series before it
	// forgets its cycles, so a router that stopped publishing them reads
	// not detected rather than the policy it last showed.
	RouterPolicyStaleAfter = time.Hour
	// RouterPolicyDwell is how long a new policy must read on every cycle
	// before the controller logs it as changed (Tracker.ObserveRouter).
	RouterPolicyDwell = 10 * time.Minute
	// RouterPolicyMinSpan is the least time a verdict is read over: four of
	// the router's five-minute windows, so no one window decides it.
	RouterPolicyMinSpan = 20 * time.Minute
	// RouterPolicyMinCycles is the least cycles a verdict is read over.
	RouterPolicyMinCycles = 8
	// DefaultRouterWindowSeconds is the router's rate window a cycle is read
	// at where its RouterLatency says none: the collector's five minutes.
	DefaultRouterWindowSeconds = 300.0

	// CacheAwareKeptShare is the least kept share, at the window's
	// lightest load, read as a router that scores the prefix.
	CacheAwareKeptShare = 0.9
	// CacheBlindKeptShare is the most kept share, at the window's lightest
	// load, read as a router that does not score the prefix.
	CacheBlindKeptShare = 0.6
	// ProducerPinShare is the least share of the cycles that read the
	// prefix pair that must read it from the same producer: the hit is read
	// from that producer's cycles alone.
	ProducerPinShare = 2.0 / 3

	// LoadRangeMin is the least ratio of the heaviest third's mean load to
	// the lightest third's that the hit's fall is read over.
	LoadRangeMin = 1.5
	// KeptShareFallMin is the least fall in the kept share, lightest third
	// to heaviest, read as a router that gives up hit for load; the fall
	// must also clear two standard errors of the difference.
	KeptShareFallMin = 0.02

	// SpreadMinCV is the least coefficient of variation across endpoints a
	// spread is read at.
	SpreadMinCV = 0.1
	// SpreadEvenRatio is the most one spread's coefficient of variation may
	// be of the other's to read as even beside it.
	SpreadEvenRatio = 0.5
	// SpreadNoiseMultiple is how many times the placements' sampling noise
	// their spread must be to read as uneven: a router that places at
	// random places n requests on an endpoint with a spread of about
	// 1/sqrt(n), which is no load scoring.
	SpreadNoiseMultiple = 2.0
	// SpreadMinInFlight is the least mean in-flight requests per endpoint a
	// cycle's spread is read at: under it the count is too small to spread.
	SpreadMinInFlight = 1.0
	// SpreadVerdictShare is the least share of the judged cycles that must
	// agree for the spread to decide.
	SpreadVerdictShare = 2.0 / 3
)

// RouterCycle is one cycle's reading of the router's policy series
// (RouterCycleOf).
type RouterCycle struct {
	// At is the cycle's clock.
	At time.Time
	// WindowSeconds is the router's rate window (domain.RouterLatency),
	// DefaultRouterWindowSeconds where it said none.
	WindowSeconds float64
	// Producer names the prefix producer the pair was read from,
	// plugin/role, empty for the model-level pair.
	Producer string
	// Candidates is the serving pods of the role the prefix producer
	// routes, and Load the model's requests a second over them.
	Candidates int
	Load       float64
	// PredictedTokenRate and BestAvailableTokenRate are the prefix pair;
	// KeptKnown says it was read, best available above 0 and at least the
	// predicted.
	PredictedTokenRate     float64
	BestAvailableTokenRate float64
	KeptKnown              bool
	// PlacementCV and InFlightCV are the coefficients of variation, across
	// SpreadEndpoints endpoints, of the requests placed on each and the
	// requests in flight on each, and PlacementsPerEndpoint the mean
	// requests placed on one over the window; SpreadKnown says at least
	// two endpoints read both, at a mean in flight of at least
	// SpreadMinInFlight.
	PlacementCV           float64
	InFlightCV            float64
	PlacementsPerEndpoint float64
	SpreadEndpoints       int
	SpreadKnown           bool
}

// Kept is the cycle's kept share, predicted over best available; 0 where
// the pair was not read.
func (c RouterCycle) Kept() float64 {
	if !c.KeptKnown {
		return 0
	}
	return c.PredictedTokenRate / c.BestAvailableTokenRate
}

// placementNoise is the spread a router placing at random would give the
// cycle's placements, 1/sqrt(placements per endpoint); +Inf with none.
func (c RouterCycle) placementNoise() float64 {
	if !(c.PlacementsPerEndpoint > 0) {
		return math.Inf(1)
	}
	return 1 / math.Sqrt(c.PlacementsPerEndpoint)
}

// RouterCycleOf is one cycle of in as the router's policy reads it: the
// prefix pair of the producer that predicted the most cached tokens (the
// model-level pair where no producer was recorded), the serving pods of the
// role it routes, and the spread of the placements and in-flight requests
// across the endpoints the router placed on. It reads in.RouterLatency and
// the cycle's rows only. A producer row is one the collector read whole
// (domain.RouterPrefixProducer), and an endpoint no router has placed on
// has no placement reading and is not in the spread.
func RouterCycleOf(in domain.AnalyzerInput, at time.Time) RouterCycle {
	c := RouterCycle{At: at}
	r := in.RouterLatency
	if r == nil {
		return c
	}
	c.WindowSeconds = r.WindowSeconds
	if !finite(c.WindowSeconds) || !(c.WindowSeconds > 0) {
		c.WindowSeconds = DefaultRouterWindowSeconds
	}
	role, pr, ba := "", r.PrefixPredictedTokenRate, r.PrefixBestAvailableTokenRate
	if p, ok := topProducer(r.PrefixProducers); ok {
		role, pr, ba = p.Role, p.PredictedTokenRate, p.BestAvailableTokenRate
		c.Producer = p.Plugin + "/" + p.Role
	}
	c.Candidates = candidates(in, role)
	c.Load = loadOf(in)
	if finite(pr) && finite(ba) && pr >= 0 && ba > 0 && pr <= ba*(1+1e-9) {
		c.PredictedTokenRate, c.BestAvailableTokenRate, c.KeptKnown = pr, ba, true
	}
	var placed float64
	c.PlacementCV, c.InFlightCV, placed, c.SpreadEndpoints, c.SpreadKnown = spreadOf(r.Endpoints)
	c.PlacementsPerEndpoint = placed * c.WindowSeconds
	return c
}

// topProducer is the prefix producer that predicted the most cached tokens,
// the one the model-level pair reads (domain.RouterLatency.PrefixPredictedTokenRate).
func topProducer(ps []domain.RouterPrefixProducer) (domain.RouterPrefixProducer, bool) {
	var best domain.RouterPrefixProducer
	found := false
	for _, p := range ps {
		if !finite(p.PredictedTokenRate) || !(p.BestAvailableTokenRate > 0) {
			continue
		}
		if !found || p.PredictedTokenRate > best.PredictedTokenRate {
			best, found = p, true
		}
	}
	return best, found
}

// candidates is the serving pods of role, the endpoints a producer of that
// role chooses among: prefill and decode by name; any other (none
// recorded) the prefill pods where the model has any, else its decode and
// unsplit pods.
func candidates(in domain.AnalyzerInput, role string) int {
	roles := map[string]string{}
	for _, vs := range in.VariantStates {
		roles[vs.VariantName] = domain.CanonicalRole(vs.Role)
	}
	counts := map[string]int{}
	for _, rm := range in.ReplicaMetrics {
		if prefill.Serving(rm) {
			counts[roles[rm.VariantName]]++
		}
	}
	switch domain.CanonicalRole(role) {
	case domain.RolePrefill:
		return counts[domain.RolePrefill]
	case domain.RoleDecode:
		return counts[domain.RoleDecode]
	}
	if n := counts[domain.RolePrefill]; n > 0 {
		return n
	}
	return counts[domain.RoleDecode] + counts[domain.RoleBoth]
}

// loadOf is the model's requests a second: the router's arrival rate,
// else its placements summed, else 0.
func loadOf(in domain.AnalyzerInput) float64 {
	if finite(in.ArrivalRate) && in.ArrivalRate > 0 {
		return in.ArrivalRate
	}
	var placed float64
	for _, e := range in.RouterLatency.Endpoints {
		if e.PlacementKnown && finite(e.PlacementRate) && e.PlacementRate > 0 {
			placed += e.PlacementRate
		}
	}
	return placed
}

// spreadOf is the coefficients of variation of the placements and of the
// in-flight requests across the endpoints that read both, the mean
// placement rate, and how many endpoints read both; known with at least
// two at a mean in flight of SpreadMinInFlight.
func spreadOf(es []domain.RouterEndpoint) (placementCV, inFlightCV, meanPlaced float64, n int, known bool) {
	var pl, inf []float64
	for _, e := range es {
		if !e.PlacementKnown || !e.InFlightRequestsKnown || !finite(e.PlacementRate) || !finite(e.InFlightRequests) ||
			e.PlacementRate < 0 || e.InFlightRequests < 0 {
			continue
		}
		pl = append(pl, e.PlacementRate)
		inf = append(inf, e.InFlightRequests)
	}
	n = len(pl)
	if n < 2 {
		return 0, 0, 0, n, false
	}
	pm, pcv := meanCV(pl)
	im, icv := meanCV(inf)
	if !(pm > 0) || !(im >= SpreadMinInFlight) {
		return 0, 0, 0, n, false
	}
	return pcv, icv, pm, n, true
}

// meanCV is xs' mean and coefficient of variation (population), the CV 0
// at a mean of 0.
func meanCV(xs []float64) (mean, cv float64) {
	for _, x := range xs {
		mean += x
	}
	mean /= float64(len(xs))
	if !(mean > 0) {
		return mean, 0
	}
	var ss float64
	for _, x := range xs {
		ss += (x - mean) * (x - mean)
	}
	return mean, math.Sqrt(ss/float64(len(xs))) / mean
}

// RouterPolicyWindow is a model's latest cycles that read any of the
// router's policy series: at most RouterPolicyWindowCycles, none older
// than RouterPolicyMaxAge behind the newest. A cycle that reads none leaves
// it as it is, so a gap in the series does not forget the policy, until
// the window has read none for RouterPolicyStaleAfter: then it forgets
// every cycle. The zero value is an empty window.
type RouterPolicyWindow struct {
	cycles []RouterCycle
	// lastRead is the clock of the last cycle that read a series.
	lastRead time.Time
}

// Note folds c into the window.
func (w *RouterPolicyWindow) Note(c RouterCycle) {
	if !c.KeptKnown && !c.SpreadKnown {
		if !w.lastRead.IsZero() && c.At.Sub(w.lastRead) >= RouterPolicyStaleAfter {
			w.cycles, w.lastRead = nil, time.Time{}
		}
		return
	}
	w.cycles = append(w.cycles, c)
	w.lastRead = c.At
	drop := 0
	for drop < len(w.cycles) && c.At.Sub(w.cycles[drop].At) > RouterPolicyMaxAge {
		drop++
	}
	drop = max(drop, len(w.cycles)-RouterPolicyWindowCycles)
	if drop > 0 {
		w.cycles = slices.Delete(w.cycles, 0, drop)
	}
}

// Len is the cycles the window holds.
func (w *RouterPolicyWindow) Len() int {
	if w == nil {
		return 0
	}
	return len(w.cycles)
}

// Policy is the policy the window's cycles read (DetectRouterPolicy).
func (w *RouterPolicyWindow) Policy() RouterPolicy {
	if w == nil {
		return DetectRouterPolicy(nil)
	}
	return DetectRouterPolicy(w.cycles)
}

// RouterPolicy is the router's policy as its series read it, with the
// evidence each verdict was read from. A figure is 0 where it was not read.
// The zero value is no reading: every verdict empty, which LoadAwareRouter
// reads as not detected; DetectRouterPolicy always sets them.
type RouterPolicy struct {
	// Policy names the two verdicts together (PolicyCacheAndLoadAware and
	// the others), and Cycles is the cycles that read any of the series.
	Policy string `json:"policy"`
	Cycles int    `json:"cycles"`
	// CacheAware is whether the router scores the prefix cache, and
	// CacheReason why it was not detected.
	CacheAware  RouterVerdict `json:"cacheAware"`
	CacheReason string        `json:"cacheReason,omitempty"`
	// LoadAware is whether the router scores the endpoints' load: yes where
	// either the hit or the spread shows it, no where the spread shows it
	// does not and the hit does not show it does. LoadAwareByHit and
	// LoadAwareBySpread are each reading's own verdict, with its reason.
	LoadAware         RouterVerdict `json:"loadAware"`
	LoadAwareByHit    RouterVerdict `json:"loadAwareByHit"`
	HitReason         string        `json:"hitReason,omitempty"`
	LoadAwareBySpread RouterVerdict `json:"loadAwareBySpread"`
	SpreadReason      string        `json:"spreadReason,omitempty"`

	// Producer is the prefix producer the hit was read from (plugin/role,
	// empty for the model-level pair). HitCycles is the cycles the kept
	// share was read over, and HitMinutes the time they span. KeptLight,
	// KeptHeavy and Kept are the kept share over the lightest third of
	// them by load per candidate, the heaviest third, and all, cached
	// tokens over cached tokens; KeptFallSE is the standard error of the
	// fall. LoadLight and LoadHeavy are the two thirds' mean requests a
	// second per candidate, and Candidates the most candidates a cycle
	// read. HalvesFalling is how many of the window's two halves, by time,
	// show the fall on their own.
	Producer      string  `json:"producer,omitempty"`
	HitCycles     int     `json:"hitCycles,omitempty"`
	HitMinutes    float64 `json:"hitMinutes,omitempty"`
	KeptLight     float64 `json:"keptLight,omitempty"`
	KeptHeavy     float64 `json:"keptHeavy,omitempty"`
	Kept          float64 `json:"kept,omitempty"`
	KeptFallSE    float64 `json:"keptFallSE,omitempty"`
	LoadLight     float64 `json:"loadLight,omitempty"`
	LoadHeavy     float64 `json:"loadHeavy,omitempty"`
	Candidates    int     `json:"candidates,omitempty"`
	HalvesFalling int     `json:"halvesFalling,omitempty"`

	// SpreadCycles is the cycles the spread was read over, and
	// SpreadBalancing and SpreadEven how many read placements uneven past
	// their sampling noise beside an even in-flight load, and placements
	// even beside an uneven one. PlacementCV and InFlightCV are the median
	// coefficients of variation over the judged cycles, and Endpoints the
	// most endpoints a cycle read.
	SpreadCycles    int     `json:"spreadCycles,omitempty"`
	SpreadBalancing int     `json:"spreadBalancing,omitempty"`
	SpreadEven      int     `json:"spreadEven,omitempty"`
	PlacementCV     float64 `json:"placementCV,omitempty"`
	InFlightCV      float64 `json:"inFlightCV,omitempty"`
	Endpoints       int     `json:"endpoints,omitempty"`
}

// LoadAwareRouter is whether the router scores load, and whether the series
// decided it: the detected counterpart of the planner's --load-aware-router.
// Only a yes or a no is decided; the zero value is not.
func (p RouterPolicy) LoadAwareRouter() (aware, detected bool) {
	return p.LoadAware == RouterYes, p.LoadAware == RouterYes || p.LoadAware == RouterNo
}

// DetectRouterPolicy is the policy cycles read, oldest first: whether the
// router scores the endpoints by their prefix cache, by their load, both
// or neither (issue llm-d-incubation/llm-d-scaling-manager#500). It is the
// one reader the controller (Tracker.ObserveRouter) and the capacity
// planner (learnreplay.State.RouterPolicy) both read cycles with.
//
//   - cache-aware: the share of the cached tokens the router kept, predicted
//     on the endpoint chosen over the best available on any candidate, at
//     the window's lightest load, read from one prefix producer
//     (ProducerPinShare). A prefix-cache scorer keeps nearly all of it
//     there, where no load scorer has a reason to send a request away from
//     its prefix. A share well under that is a router that does not score
//     the prefix.
//   - load-aware, by hit: the kept share falling as the load rises, in the
//     window as a whole and in each of its halves by time. It is a
//     correlation, not a test of the scorer: a change of the traffic mix
//     with the time of day, a cold cache or eviction under load can lower
//     the share at high load too, and the two halves only make one such
//     episode less likely to decide it.
//   - load-aware, by spread: the placements and the in-flight requests
//     across the endpoints the router places on (domain.RouterEndpoint). A
//     router that scores load evens the in-flight requests out by placing
//     unevenly; one that does not places evenly whatever the endpoints
//     hold, and the in-flight requests then spread wider. Placements read
//     uneven only past SpreadNoiseMultiple times the spread a random router
//     would give them.
//
// Each verdict is "not detected" where its series are missing or do not
// decide it. A kept share near 1 also follows from a prefix every endpoint
// holds, which a cache-blind router keeps as well. In P/D the router counts
// placements on the primary profile's endpoint, decode, so the spread reads
// the decode profile, while the hit is the prefix producer's.
//
// It is a reading, not an input to any price yet. The series are collected
// only while a learner recording is on (domain.RouterLatency.Endpoints), so
// a price that read the policy would make a recorded run price apart from
// the same run unrecorded until the collection is widened.
func DetectRouterPolicy(cycles []RouterCycle) RouterPolicy {
	p := RouterPolicy{Cycles: len(cycles), CacheAware: RouterNotDetected, LoadAwareByHit: RouterNotDetected,
		LoadAwareBySpread: RouterNotDetected}
	var hit, spread []RouterCycle
	for _, c := range cycles {
		if c.KeptKnown {
			hit = append(hit, c)
		}
		if c.SpreadKnown {
			spread = append(spread, c)
		}
	}
	if len(hit) == 0 {
		p.CacheReason, p.HitReason = routerPolicyReasonNoSeries, routerPolicyReasonNoSeries
	} else {
		p.readHit(hit)
	}
	if len(spread) == 0 {
		p.SpreadReason = routerPolicyReasonSpreadNoEndpoint
		if len(cycles) == 0 {
			p.SpreadReason = routerPolicyReasonNoSeries
		}
	} else {
		p.readSpread(spread)
	}
	switch {
	case p.LoadAwareByHit == RouterYes || p.LoadAwareBySpread == RouterYes:
		p.LoadAware = RouterYes
	case p.LoadAwareBySpread == RouterNo:
		p.LoadAware = RouterNo
	default:
		p.LoadAware = RouterNotDetected
	}
	p.Policy = policyName(p.CacheAware, p.LoadAware)
	return p
}

// policyName names the pair of verdicts.
func policyName(cache, load RouterVerdict) string {
	switch {
	case cache == RouterYes && load == RouterYes:
		return PolicyCacheAndLoadAware
	case cache == RouterYes && load == RouterNo:
		return PolicyCacheAware
	case cache == RouterYes:
		return PolicyCacheAwareLoadNotDetected
	case cache == RouterNo && load == RouterYes:
		return PolicyLoadAware
	case cache == RouterNo && load == RouterNo:
		return PolicyNeither
	case cache == RouterNo:
		return PolicyCacheBlindLoadNotDetected
	case load == RouterYes:
		return PolicyLoadAwareCacheNotDetected
	case load == RouterNo:
		return PolicyLoadBlindCacheNotDetected
	}
	return PolicyNotDetected
}

// span is the time cycles cover, oldest to newest, with one router window
// for the newest's own.
func span(cycles []RouterCycle) time.Duration {
	if len(cycles) == 0 {
		return 0
	}
	first := slices.MinFunc(cycles, func(a, b RouterCycle) int { return a.At.Compare(b.At) })
	last := slices.MaxFunc(cycles, func(a, b RouterCycle) int { return a.At.Compare(b.At) })
	window := time.Duration(last.WindowSeconds * float64(time.Second))
	return last.At.Sub(first.At) + window
}

// enough reports whether cycles are enough to read a verdict over.
func enough(cycles []RouterCycle) bool {
	return len(cycles) >= RouterPolicyMinCycles && span(cycles) >= RouterPolicyMinSpan
}

// pinnedProducer is the producer most of cycles read the pair from, and
// whether it read at least ProducerPinShare of them.
func pinnedProducer(cycles []RouterCycle) (string, bool) {
	counts := map[string]int{}
	for _, c := range cycles {
		counts[c.Producer]++
	}
	best, n := "", -1
	for k, v := range counts {
		if v > n || (v == n && k < best) {
			best, n = k, v
		}
	}
	return best, float64(n) >= ProducerPinShare*float64(len(cycles))
}

// readHit reads the cache verdict and the hit's load verdict from the
// cycles that read the prefix pair.
func (p *RouterPolicy) readHit(cycles []RouterCycle) {
	producer, pinned := pinnedProducer(cycles)
	p.Producer = producer
	if !pinned {
		p.CacheReason, p.HitReason = routerPolicyReasonProducerChanged, routerPolicyReasonProducerChanged
		return
	}
	var multi []RouterCycle
	for _, c := range cycles {
		if c.Producer != producer {
			continue
		}
		p.Candidates = max(p.Candidates, c.Candidates)
		if c.Candidates >= 2 && c.Load > 0 {
			multi = append(multi, c)
		}
	}
	if len(multi) == 0 {
		p.CacheReason, p.HitReason = routerPolicyReasonOneCandidate, routerPolicyReasonOneCandidate
		return
	}
	p.HitCycles, p.HitMinutes = len(multi), span(multi).Minutes()
	if !enough(multi) {
		p.CacheReason, p.HitReason = routerPolicyReasonShortWindow, routerPolicyReasonShortWindow
		return
	}
	whole := readFall(multi)
	p.Kept, p.KeptLight, p.KeptHeavy, p.KeptFallSE = whole.kept, whole.light, whole.heavy, whole.se
	p.LoadLight, p.LoadHeavy = whole.loadLight, whole.loadHeavy

	switch {
	case p.KeptLight >= CacheAwareKeptShare:
		p.CacheAware = RouterYes
	case p.KeptLight <= CacheBlindKeptShare:
		p.CacheAware = RouterNo
	default:
		p.CacheReason = routerPolicyReasonBetween
	}
	if p.CacheAware == RouterNo {
		// A router that keeps little of the hit at light load has none to
		// give up for load: the hit cannot show the load side.
		p.HitReason = routerPolicyReasonBetween
		return
	}
	if whole.reason != "" {
		p.HitReason = whole.reason
		return
	}
	// The same fall in each half of the window by time, so one stretch
	// where the load and the share moved together for another reason does
	// not decide it alone.
	byTime := slices.Clone(multi)
	slices.SortStableFunc(byTime, func(a, b RouterCycle) int { return a.At.Compare(b.At) })
	half := len(byTime) / 2
	for _, h := range [][]RouterCycle{byTime[:half], byTime[half:]} {
		if enough(h) && readFall(h).reason == "" {
			p.HalvesFalling++
		}
	}
	if p.HalvesFalling < 2 {
		p.HitReason = routerPolicyReasonNotBothHalves
		return
	}
	p.LoadAwareByHit = RouterYes
}

// fall is the kept share's reading against the load over some cycles
// (readFall): the shares over all, the lightest third and the heaviest
// third by load per candidate, the fall's standard error, the thirds' mean
// loads per candidate, and why the fall does not read as one, "" where it
// does.
type fall struct {
	kept, light, heavy, se float64
	loadLight, loadHeavy   float64
	reason                 string
}

// readFall reads the kept share's fall with the load over cycles.
func readFall(cycles []RouterCycle) fall {
	perPod := func(c RouterCycle) float64 { return c.Load / float64(c.Candidates) }
	sorted := slices.Clone(cycles)
	slices.SortStableFunc(sorted, func(a, b RouterCycle) int { return cmp.Compare(perPod(a), perPod(b)) })
	third := max(len(sorted)/3, 1)
	light, heavy := sorted[:third], sorted[len(sorted)-third:]
	var f fall
	var lightVar, heavyVar float64
	f.kept, _ = keptOf(sorted)
	f.light, lightVar = keptOf(light)
	f.heavy, heavyVar = keptOf(heavy)
	f.loadLight, f.loadHeavy = meanOf(light, perPod), meanOf(heavy, perPod)
	// Each third's cycles counted as the independent router windows they
	// span: cycles closer than the window read overlapping requests.
	f.se = math.Sqrt(lightVar/independent(light) + heavyVar/independent(heavy))
	d := f.light - f.heavy
	switch {
	case !(f.loadLight > 0) || !(f.loadHeavy >= LoadRangeMin*f.loadLight):
		f.reason = routerPolicyReasonNoLoadRange
	case !(d >= KeptShareFallMin) || !(d >= 2*f.se):
		f.reason = routerPolicyReasonFlat
	}
	return f
}

// keptOf is cycles' kept share, cached tokens over cached tokens, and the
// variance of the cycles' own shares about it, weighted by best available.
func keptOf(cycles []RouterCycle) (kept, variance float64) {
	var pr, ba float64
	for _, c := range cycles {
		pr += c.PredictedTokenRate
		ba += c.BestAvailableTokenRate
	}
	if !(ba > 0) {
		return 0, 0
	}
	kept = pr / ba
	var ss float64
	for _, c := range cycles {
		d := c.Kept() - kept
		ss += c.BestAvailableTokenRate * d * d
	}
	return kept, ss / ba
}

// independent is how many independent router windows cycles hold: each
// cycle is worth the share of a window its spacing to the one before it
// covers, at most one, the first a whole one; at least 1 and at most the
// cycles' count. A cycle's window is DefaultRouterWindowSeconds where it
// says none, never no decorrelation at all.
func independent(cycles []RouterCycle) float64 {
	n := float64(len(cycles))
	var window float64
	for _, c := range cycles {
		window = max(window, c.WindowSeconds)
	}
	if !finite(window) || !(window > 0) {
		window = DefaultRouterWindowSeconds
	}
	ats := make([]time.Time, len(cycles))
	for i, c := range cycles {
		ats[i] = c.At
	}
	slices.SortFunc(ats, func(a, b time.Time) int { return a.Compare(b) })
	worth := 1.0
	for i := 1; i < len(ats); i++ {
		worth += min(ats[i].Sub(ats[i-1]).Seconds()/window, 1)
	}
	return max(min(n, worth), 1)
}

// meanOf is the mean of f over cycles.
func meanOf(cycles []RouterCycle, f func(RouterCycle) float64) float64 {
	if len(cycles) == 0 {
		return 0
	}
	var s float64
	for _, c := range cycles {
		s += f(c)
	}
	return s / float64(len(cycles))
}

// readSpread reads the spread's load verdict from the cycles that read the
// placements and in-flight requests across endpoints.
func (p *RouterPolicy) readSpread(cycles []RouterCycle) {
	var judged []RouterCycle
	for _, c := range cycles {
		p.Endpoints = max(p.Endpoints, c.SpreadEndpoints)
		uneven := max(SpreadMinCV, SpreadNoiseMultiple*c.placementNoise())
		switch {
		case c.PlacementCV >= uneven && c.InFlightCV <= SpreadEvenRatio*c.PlacementCV:
			p.SpreadBalancing++
			judged = append(judged, c)
		case c.InFlightCV >= SpreadMinCV && c.PlacementCV <= SpreadEvenRatio*c.InFlightCV:
			p.SpreadEven++
			judged = append(judged, c)
		}
	}
	p.SpreadCycles = len(cycles)
	if !enough(cycles) {
		p.SpreadReason = routerPolicyReasonShortWindow
		return
	}
	if len(judged) > 0 {
		p.PlacementCV = median(judged, func(c RouterCycle) float64 { return c.PlacementCV })
		p.InFlightCV = median(judged, func(c RouterCycle) float64 { return c.InFlightCV })
	}
	n := float64(len(judged))
	switch {
	case !enough(judged):
		p.SpreadReason = routerPolicyReasonNoSpread
	case float64(p.SpreadBalancing) >= SpreadVerdictShare*n:
		p.LoadAwareBySpread = RouterYes
	case float64(p.SpreadEven) >= SpreadVerdictShare*n:
		p.LoadAwareBySpread = RouterNo
	default:
		p.SpreadReason = routerPolicyReasonMixedSpread
	}
}

// median is the median of f over cycles.
func median(cycles []RouterCycle, f func(RouterCycle) float64) float64 {
	xs := make([]float64, len(cycles))
	for i, c := range cycles {
		xs[i] = f(c)
	}
	slices.Sort(xs)
	n := len(xs)
	if n%2 == 1 {
		return xs[n/2]
	}
	return (xs[n/2-1] + xs[n/2]) / 2
}

// finite reports whether x is a number, neither NaN nor infinite.
func finite(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }
