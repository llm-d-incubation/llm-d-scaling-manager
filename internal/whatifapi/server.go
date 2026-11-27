package whatifapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// maxBody is the largest request body the API reads: a scenario is a few
// hundred bytes.
const maxBody = 1 << 20

// Cache sizes: inputs are a replayed state each, so few are kept; plans and
// curve sets are small.
const (
	defaultInputs = 16
	defaultPlans  = 512
	defaultCurves = 256
	defaultSizes  = 128
)

// defaultResponseBytes bounds the plan and the size responses kept, each
// cache on its own: a plan with the per-bucket view and 8 edges measured
// 548 KB on the worked recording, so 512 of them would hold some 280 MB
// with no bound in bytes; 64 MiB keeps about 120 of them.
const defaultResponseBytes = 64 << 20

// bodyBytes is a response's weight in a cache: its length.
func bodyBytes(b []byte) int { return len(b) }

// DefaultMaxInputs is how many sources are resolved at once by default: each
// is a replay or a Prometheus capture of seconds to minutes, and holds its
// memory while it runs.
const DefaultMaxInputs = 2

// DefaultInputsBytes bounds the inputs kept, by the weight each Resolved
// gives (Resolved.Bytes): a replayed state of a day-long recording of a
// 16-replica P/D fleet weighs some tens of MiB, so the cache's 16 entries
// fit in it whatever the recordings, and a larger one is evicted first.
const DefaultInputsBytes = 256 << 20

// retryAfterSeconds is what a busy server asks a client to wait.
const retryAfterSeconds = "1"

// Config is the server's configuration.
type Config struct {
	// Resolver resolves sources (cmd/capacity-planner's).
	Resolver Resolver
	// WhatifSchema names the CLI schema the answer, refusal and flag shapes
	// follow ("schema-v3").
	WhatifSchema string
	// PlannerVersion is the planner's release, stamped on a prediction.
	PlannerVersion string
	// SelfCheck is whether inputs self-check when the request does not say
	// (InputsRequest.SelfCheck): the CLI's serve sets it unless
	// --no-self-check.
	SelfCheck bool
	// MaxPlans and MaxInputs are how many plans and how many inputs are
	// priced at once; past them a request is 503 with Retry-After. 0 is the
	// default: GOMAXPROCS plans, DefaultMaxInputs inputs.
	MaxPlans, MaxInputs int
	// MaxInputsBytes bounds the inputs kept, in all, by the weight each
	// Resolved gives; the newest is kept whatever it weighs. 0 or less is
	// DefaultInputsBytes.
	MaxInputsBytes int
	// PlanBudget is how long one plan may be priced. Past it the plan stops
	// at its next figure and is refused with PlanBudgetExceeded. 0 or less is
	// DefaultPlanBudget. It must end before the HTTP server's write timeout,
	// or the refusal is written to a connection already closed.
	PlanBudget time.Duration
	// Uploader keeps the recordings uploaded with POST /v1/recordings; nil
	// takes none, and the endpoint is 403. UploadMaxBytes is the largest
	// upload; 0 or less is DefaultUploadBytes.
	Uploader       Uploader
	UploadMaxBytes int64
	// TTFTModel and TPOTModel are the experimental models a backtest that
	// names none is priced with (BacktestRequest.TTFTModel, TPOTModel), and
	// the main view of a source whose inputs request names none
	// (InputsRequest.TTFTModel, TPOTModel): "" is the planner's own. The
	// CLI's serve sets them with
	// --experimental-ttft=v2 and --experimental-tpot=v2; its Resolver then
	// gives the cycles they read (Resolved.Mixture, Decode).
	TTFTModel, TPOTModel string
	// NoRankMixtureStratified prices the rank mixture with each rank's
	// service moments read from its own draws where a request does not say
	// (BacktestRequest.RankMixtureStratified,
	// InputsRequest.RankMixtureStratified); false, the default, reads them
	// from the computed-token histogram (whatif.DefaultRankMixtureStratified).
	// The CLI's serve sets it with --rank-mixture-stratified=false.
	NoRankMixtureStratified bool
	// RankMixtureKVHold and RankMixtureRemainderSpread turn the variant's
	// modes on for every backtest and source priced with the variant
	// (whatif.QueueRankMixtureNLLine) where the request does not, and
	// RankMixtureMinFitWindow is the shortest fit window either model
	// prices from where the request names none: 0 is
	// whatif.MinSpreadFitWindow. The CLI's serve sets them with
	// --rank-mixture-kv-hold, --rank-mixture-remainder-spread and
	// --rank-mixture-min-fit-window.
	RankMixtureKVHold, RankMixtureRemainderSpread bool
	RankMixtureMinFitWindow                       time.Duration
	// RankMixtureRemainderServer turns the variant's remainder server on
	// where the request does not, for a request whose remainder spread is
	// on. The CLI's serve sets it with --rank-mixture-remainder-server.
	RankMixtureRemainderServer bool
	// TPOTDistMinFitSteps is the fewest fit steps the TPOT distribution
	// fits on where a backtest or a source's request names none: 0 is
	// MinTPOTDistFitSteps. The CLI's serve sets it with
	// --tpot-dist-min-fit-steps.
	TPOTDistMinFitSteps int
}

// DefaultPlanBudget is the plan budget a server has when its Config names
// none: a cold plan of the worked recording takes under 20 s, and it ends
// well before LongRequestDeadline (capacity-planner serve refuses a
// --plan-budget past 9 minutes).
const DefaultPlanBudget = 5 * time.Minute

// LongRequestDeadline is how long a request that computes (POST /v1/inputs,
// /v1/plan, /v1/size, /v1/report, /v1/dayplan, /v1/backtest, /v1/predict and
// /v1/compare) may run once its body is read: the connection's write
// deadline moves to it, past the server's own write timeout. A replay of a
// 24 h recording with every model fitted takes over 5 minutes on an idle
// 22-CPU node and about twice that on a busy one, and the server's 10-minute
// write timeout cut the connection before the answer. The other routes keep
// the server's timeouts.
const LongRequestDeadline = 30 * time.Minute

// ReasonPlanBudgetExceeded is the reason an ErrorResponse gives for a plan
// stopped at the server's plan budget.
const ReasonPlanBudgetExceeded = "PlanBudgetExceeded"

// errPlanBudget is the cause a plan's context ends with at its budget.
var errPlanBudget = errors.New("plan budget reached")

// BudgetError is a plan stopped because it ran past the server's plan
// budget. Nothing of it is cached, so the same plan asked again runs again.
//
// It deliberately has no Unwrap to context.DeadlineExceeded: writeErr would
// then read it as a request that ended, and flight.doFor would have every
// caller sharing the plan price it again on its own context.
type BudgetError struct {
	// Budget is the server's plan budget.
	Budget time.Duration
}

// Error says what ran out and what to do.
func (e BudgetError) Error() string {
	return fmt.Sprintf("the plan was not priced within the server's plan budget of %s, and was stopped: "+
		`ask without the per-bucket view (on the page, untick "Show if all traffic were bucket b") or with fewer bucket edges, `+
		"or start the server with a larger --plan-budget", e.Budget)
}

// Server serves the API. Its state is caches only: an inputs entry per
// inputsId, a response per scenarioId and per sizeId, a curve set per fleet,
// and the limits per fleet and targets.
type Server struct {
	resolver       Resolver
	whatifSchema   string
	plannerVersion string
	selfCheck      bool
	inputs         *lru[*inputsEntry]
	plans          *lru[[]byte]
	sizes          *lru[[]byte]
	// searches is each size's search, by sizeId, kept before its answer's
	// plan is priced: a size whose plan failed is not searched again.
	searches *lru[*SizeResponse]
	curves   *lru[*curveSet]
	limits   *lru[*limitEntry]
	// planSlots and inputSlots bound the work in flight: a token per request
	// priced.
	planSlots, inputSlots chan struct{}
	// planBudget is how long one plan may be priced.
	planBudget time.Duration
	// planFlight, curveFlight and limitFlight price each key once at a time:
	// identical requests in flight share one pricing and one slot.
	planFlight  flight[[]byte]
	sizeFlight  flight[[]byte]
	curveFlight flight[*curveSet]
	limitFlight flight[*limitEntry]
	// onCurves and onLimits, when set, are called each time a curve set or a
	// limit entry is priced: the caching tests count them.
	onCurves, onLimits func()
	// onSearch, when set, is called each time a size searches.
	onSearch func()
	// uploader keeps uploads, nil for none; uploadMax is the largest, and
	// uploadSlots bounds them to one at a time.
	uploader    Uploader
	uploadMax   int64
	uploadSlots chan struct{}
	// uploadIdle is how long an upload may send nothing (uploadIdle), and
	// uploadDeadline how long it may take in all (uploadDeadline).
	uploadIdle     time.Duration
	uploadDeadline time.Duration
	// longDeadline is how long a request that computes may run once its body
	// is read (LongRequestDeadline).
	longDeadline time.Duration
	// ttftModel and tpotModel are Config's.
	ttftModel, tpotModel string
	// noStratified is Config.NoRankMixtureStratified.
	noStratified bool
	// kvHold, remainderSpread and minFitWindow are Config's
	// RankMixtureKVHold, RankMixtureRemainderSpread and
	// RankMixtureMinFitWindow.
	kvHold, remainderSpread bool
	minFitWindow            time.Duration
	// remainderServer is Config.RankMixtureRemainderServer.
	remainderServer bool
	// tpotMinFitSteps is Config.TPOTDistMinFitSteps.
	tpotMinFitSteps int
}

// inputsEntry is a source resolved: its id, the inputs a plan reads its
// defaults from, and its Pricer.
type inputsEntry struct {
	id     string
	doc    InputsResponse
	pricer Pricer
	// series is the source's cycles, for a day plan; nil where the
	// Resolver gave none.
	series []SeriesSample
	// latency and config are the source's for a backtest (Resolved.Latency,
	// Resolved.Config).
	latency []LatencySample
	config  RunConfig
	// mixture is the source's cycles for the rank-mixture model
	// (Resolved.Mixture); nil unless asked.
	mixture []whatif.MixtureCycle
	// decode is the source's cycles for the TPOT distribution
	// (Resolved.Decode); nil unless asked.
	decode []whatif.DecodeCycle
	// engine is the engines' flags those models read (Resolved.Engine).
	engine EngineSlots
	// ttftModel is the experimental TTFT model the main view prices with,
	// fitted on the source; nil unless asked (InputsRequest.TTFTModel).
	ttftModel *mainMixture
	// tpotModel is the experimental TPOT model the main view prices with,
	// fitted on the source; nil unless asked (InputsRequest.TPOTModel).
	tpotModel *mainTPOT
	// bytes is the entry's weight in the inputs cache (Resolved.Bytes).
	bytes int
}

// entryBytes is an inputs entry's weight in the cache.
func entryBytes(e *inputsEntry) int { return e.bytes }

// New is a server with empty caches.
func New(c Config) *Server {
	plans, inputs := c.MaxPlans, c.MaxInputs
	if plans <= 0 {
		plans = runtime.GOMAXPROCS(0)
	}
	if inputs <= 0 {
		inputs = DefaultMaxInputs
	}
	inputsBytes := c.MaxInputsBytes
	if inputsBytes <= 0 {
		inputsBytes = DefaultInputsBytes
	}
	budget := c.PlanBudget
	if budget <= 0 {
		budget = DefaultPlanBudget
	}
	uploadMax := c.UploadMaxBytes
	if uploadMax <= 0 {
		uploadMax = DefaultUploadBytes
	}
	return &Server{resolver: c.Resolver, whatifSchema: c.WhatifSchema, plannerVersion: c.PlannerVersion, selfCheck: c.SelfCheck, inputs: newWeighedLRU(defaultInputs, inputsBytes, entryBytes),
		plans: newWeighedLRU(defaultPlans, defaultResponseBytes, bodyBytes), sizes: newWeighedLRU(defaultSizes, defaultResponseBytes, bodyBytes),
		searches: newLRU[*SizeResponse](defaultSizes), curves: newLRU[*curveSet](defaultCurves), limits: newLRU[*limitEntry](defaultCurves),
		planSlots: make(chan struct{}, plans), inputSlots: make(chan struct{}, inputs), planBudget: budget,
		uploader: c.Uploader, uploadMax: uploadMax, uploadSlots: make(chan struct{}, 1), uploadIdle: uploadIdle, uploadDeadline: uploadDeadline,
		longDeadline: LongRequestDeadline, ttftModel: c.TTFTModel, tpotModel: c.TPOTModel, noStratified: c.NoRankMixtureStratified,
		kvHold: c.RankMixtureKVHold, remainderSpread: c.RankMixtureRemainderSpread, minFitWindow: c.RankMixtureMinFitWindow,
		remainderServer: c.RankMixtureRemainderServer,
		tpotMinFitSteps: c.TPOTDistMinFitSteps}
}

// stratified is whether the rank mixture is priced with stratified service
// moments where a request does not say.
func (s *Server) stratified() bool {
	return whatif.DefaultRankMixtureStratified && !s.noStratified
}

// errBusy is a request past the server's bound on work in flight.
var errBusy = errors.New("the server is pricing as many requests as it takes at once; retry shortly")

// acquire takes a slot, or reports false at once when none is free.
func acquire(slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
		return true
	default:
		return false
	}
}

// Handler is the API's routes. The caller wraps it in authentication where
// the bind needs it (Authenticator.Wrap), or in LoopbackOnly.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/inputs", s.handleInputs)
	mux.HandleFunc("POST /v1/plan", s.handlePlan)
	mux.HandleFunc("POST /v1/size", s.handleSize)
	mux.HandleFunc("POST /v1/report", s.handleReport)
	mux.HandleFunc("POST /v1/dayplan", s.handleDayPlan)
	mux.HandleFunc("POST /v1/backtest", s.handleBacktest)
	mux.HandleFunc("POST /v1/predict", s.handlePredict)
	mux.HandleFunc("POST /v1/compare", s.handleCompare)
	mux.HandleFunc("GET /v1/sources", s.handleSources)
	mux.HandleFunc("POST "+UploadPath, s.handleUpload)
	mux.HandleFunc("GET /{$}", servePage)
	mux.HandleFunc("GET /page/", servePage)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, fmt.Errorf("%s %s: no such endpoint; the server serves POST /v1/inputs, POST /v1/plan, POST /v1/size, POST /v1/report, POST /v1/dayplan, GET /v1/sources, POST /v1/recordings and the page at /",
			r.Method, r.URL.Path))
	})
	return mux
}

// handleSources is GET /v1/sources: the sources the resolver reads, or none
// when it cannot list them.
func (s *Server) handleSources(w http.ResponseWriter, r *http.Request) {
	out := SourcesResponse{Namespaces: []string{}, Recordings: []string{}}
	if l, ok := s.resolver.(SourceLister); ok {
		var err error
		if out, err = l.Sources(r.Context()); err != nil {
			writeErr(w, err)
			return
		}
		if out.Namespaces == nil {
			out.Namespaces = []string{}
		}
		if out.Recordings == nil {
			out.Recordings = []string{}
		}
	}
	if s.uploader != nil {
		out.Upload = &UploadInfo{MaxBytes: int(s.uploadMax)}
	}
	out.SchemaVersion = SchemaVersion
	body, err := marshal(out)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeBody(w, body)
}

// handleInputs is POST /v1/inputs: the source resolved, kept under its id,
// and answered with the caller's own source.
func (s *Server) handleInputs(w http.ResponseWriter, r *http.Request) {
	var req InputsRequest
	if !s.readLongJSON(w, r, &req, maxBody) {
		return
	}
	if !acquire(s.inputSlots) {
		writeErr(w, errBusy)
		return
	}
	defer func() { <-s.inputSlots }()
	check := s.selfCheck
	if req.SelfCheck != nil {
		check = *req.SelfCheck
	}
	req.SelfCheck = &check
	model, err := ttftModelOf(req.TTFTModel, s.ttftModel)
	if err != nil {
		writeErr(w, err)
		return
	}
	tpotModel, err := tpotModelOf(req.TPOTModel, s.tpotModel)
	if err != nil {
		writeErr(w, err)
		return
	}
	req.TTFTModel, req.TPOTModel = model, tpotModel
	tpotSteps, err := s.tpotMinFitStepsOf(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	opts, err := s.mainTTFTOptions(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	res, err := s.resolver.Resolve(r.Context(), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	doc := res.Inputs
	doc.SchemaVersion, doc.WhatifSchema = SchemaVersion, s.whatifSchema
	doc.InputsID = contentID("inputs", res.Content)
	doc.Measured = BusiestHourMeasured(res.Series, res.Latency)
	var mm *mainMixture
	stratified := stratifiedOf(req.RankMixtureStratified, s.stratified())
	if model != "" {
		// The same source priced with the model is another entry: its id
		// names the model.
		doc.InputsID = contentID("inputs", struct {
			Content   any
			TTFTModel string
		}{res.Content, model})
		opts.Stratified = stratified
		mm = fitMainMixture(&inputsEntry{doc: doc, series: res.Series, latency: res.Latency, mixture: res.Mixture, engine: res.Engine},
			opts.switches())
		info := mm.info
		doc.TTFTModel = &info
	}
	var mt *mainTPOT
	if tpotModel != "" {
		// So is the source priced with the TPOT model: its id names both.
		doc.InputsID = contentID("inputs", struct {
			Content              any
			TTFTModel, TPOTModel string
		}{res.Content, model, tpotModel})
		mt = fitMainTPOT(&inputsEntry{doc: doc, series: res.Series, latency: res.Latency, decode: res.Decode, engine: res.Engine}, tpotModel, tpotSteps)
		info := mt.info
		doc.TPOTModel = &info
	}
	if tpotModel != "" && tpotSteps > 0 {
		// And so is the distribution fitted on another fewest steps.
		doc.InputsID = contentID("inputs", struct {
			ID                  string
			TPOTDistMinFitSteps int
		}{doc.InputsID, tpotSteps})
	}
	if model != "" && !stratified {
		// And so is the model read from each rank's own draws.
		doc.InputsID = contentID("inputs", struct {
			ID           string
			Unstratified bool
		}{doc.InputsID, true})
	}
	if model != "" && opts.moved() {
		// And so is the model fitted with the variant's modes or another
		// shortest fit window.
		doc.InputsID = contentID("inputs", struct {
			ID                      string
			KVHold, RemainderSpread bool
			MinFitWindow            time.Duration
		}{doc.InputsID, opts.KVHold, opts.RemainderSpread, opts.MinFitWindow})
	}
	if model != "" && opts.RemainderServer {
		// And so is the remainder drawn from the pods' API servers, hashed
		// apart so every inputsId without it stays as it was.
		doc.InputsID = contentID("inputs", struct {
			ID              string
			RemainderServer bool
		}{doc.InputsID, true})
	}
	if check && (doc.Source.Kind == SourceRecording || doc.Source.Kind == SourcePrometheus) {
		// The self-check is bounded as a plan is; out of time it is not run,
		// and the inputs are still answered.
		sctx, cancel := context.WithTimeoutCause(r.Context(), s.planBudget, errPlanBudget)
		doc.SelfCheck = SelfCheckOf(sctx, res, SelfCheckInfo{PlannerVersion: s.plannerVersion, TPOTModel: tpotModel})
		cancel()
	}
	// The entry kept is the first caller's, but each caller is answered with
	// its own source: two recordings of the same bytes share an id, not a
	// name.
	s.inputs.add(doc.InputsID, &inputsEntry{id: doc.InputsID, doc: doc, pricer: res.Pricer, series: res.Series, latency: res.Latency,
		config: res.Config, bytes: res.Bytes, mixture: res.Mixture, decode: res.Decode, engine: res.Engine, ttftModel: mm, tpotModel: mt})
	body, err := marshal(doc)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeBody(w, body)
}

// handlePlan is POST /v1/plan: the scenario priced, or its cached bytes.
func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	var req PlanRequest
	if !s.readLongJSON(w, r, &req, maxBody) {
		return
	}
	body, err := s.plan(r.Context(), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeBody(w, body)
}

// errUnknownInputs is an inputsId the server does not hold.
var errUnknownInputs = errors.New("unknown inputsId: resolve the source with POST /v1/inputs (ids are kept while in use, the least used dropped first)")

// plan prices req and returns the response body: from the cache when the same
// scenario was priced before, so the same scenario has the same bytes. A
// plan not cached is priced once however many ask for it at once, takes one
// of the server's plan slots (errBusy with none free), and stops between
// figures once ctx ends.
func (s *Server) plan(ctx context.Context, req PlanRequest) ([]byte, error) {
	return s.planHolding(ctx, req, false)
}

// planHolding is plan for a caller that may already hold a plan slot (held):
// a size prices its answer's plan on the slot its search took. A held caller
// that joined a plan already in flight shares that plan's result; when its
// leader, holding no slot, was turned away busy, the held caller asks once
// more and, with no plan in flight then, prices it on its own slot. So a
// finished search is lost to a busy server only when another request for the
// same plan is turned away busy twice in a row.
func (s *Server) planHolding(ctx context.Context, req PlanRequest, held bool) ([]byte, error) {
	body, err := s.planOnce(ctx, req, held)
	if held && errors.Is(err, errBusy) {
		body, err = s.planOnce(ctx, req, held)
	}
	return body, err
}

// planOnce is one ask of planHolding.
func (s *Server) planOnce(ctx context.Context, req PlanRequest, held bool) ([]byte, error) {
	e, ok := s.inputs.get(req.InputsID)
	if !ok {
		return nil, errUnknownInputs
	}
	sc, err := scenarioOf(&e.doc, req)
	if err != nil {
		return nil, err
	}
	id := contentID("scenario", sc)
	if b, ok := s.plans.get(id); ok {
		return b, nil
	}
	return s.planFlight.doFor(ctx, id, func() ([]byte, error) {
		if b, ok := s.plans.get(id); ok {
			return b, nil
		}
		if !held {
			if !acquire(s.planSlots) {
				return nil, errBusy
			}
			defer func() { <-s.planSlots }()
		}
		out, err := s.planWithin(ctx, &planner{s: s, e: e, sc: sc})
		if err != nil {
			return nil, err
		}
		out.ScenarioID = id
		body, err := marshal(out)
		if err != nil {
			return nil, err
		}
		return s.plans.add(id, body), nil
	})
}

// planWithin prices p within the server's plan budget: past it, the plan
// stops at its next figure and is a BudgetError. A request that ended first
// is its context's error, as before.
func (s *Server) planWithin(ctx context.Context, p *planner) (*PlanResponse, error) {
	bctx, cancel := context.WithTimeoutCause(ctx, s.planBudget, errPlanBudget)
	defer cancel()
	out, err := p.plan(bctx)
	// Only the budget's own end is a BudgetError: an error the plan returned
	// for another reason as the budget ran out (a knob refused, a fleet that
	// failed) is that error.
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil && errors.Is(context.Cause(bctx), errPlanBudget) {
		return nil, BudgetError{Budget: s.planBudget}
	}
	return out, err
}

// LoopbackOnly answers only requests addressed to a loopback name, by the
// rule CheckListen binds by (isLoopback: localhost, 127.0.0.0/8, ::1, with
// any port): a server with no authentication must not answer a page that
// rebinds its own name to 127.0.0.1. Others are 403.
func LoopbackOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if !isLoopback(strings.Trim(host, "[]")) {
			writeError(w, http.StatusForbidden, fmt.Errorf("host %q: this server answers loopback names only (localhost, 127.0.0.0/8, [::1])", r.Host))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// readLongJSON is readJSONUpTo for a request that computes: once the body is
// read, within the server's read timeout, the connection's write deadline
// moves to longDeadline from now (LongRequestDeadline). The read deadline is
// left alone: once the body is read the server clears it, and its read of
// the idle connection ends the request's context when the client leaves. A
// recorder in a test has no connection to set a deadline on.
func (s *Server) readLongJSON(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	if !readJSONUpTo(w, r, v, limit) {
		return false
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.longDeadline))
	return true
}

// readJSONUpTo reads the request body into v strictly: a media type other
// than application/json (none included, so that no form a page posts across
// sites is read) is 415; an unknown field, a second value, or a body past
// limit bytes (maxBody for most routes) is a 400.
func readJSONUpTo(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, fmt.Errorf("content type %q, want application/json", r.Header.Get("Content-Type")))
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("request body: %w", err))
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, errors.New("request body: more than one JSON value"))
		return false
	}
	return true
}

// marshal is v as indented JSON with a final newline, HTML left unescaped.
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// writeBody writes a JSON body with 200.
func writeBody(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

// writeErr writes err with its status: 400 for the caller's input, 404 for
// an unknown inputsId, 413 for an upload past a bound, 503 for a busy server (with Retry-After), a plan past
// the plan budget (reason PlanBudgetExceeded) or a request that ended, 500
// for anything else.
func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.As(err, new(ConfigMismatchError)):
		writeErrorReason(w, http.StatusBadRequest, err, ReasonConfigMismatch)
	case IsInputError(err):
		writeError(w, http.StatusBadRequest, err)
	case errors.Is(err, errUnknownInputs):
		writeError(w, http.StatusNotFound, err)
	case errors.As(err, new(ForbiddenError)):
		writeError(w, http.StatusForbidden, err)
	case errors.As(err, new(TooLargeError)):
		writeError(w, http.StatusRequestEntityTooLarge, err)
	case errors.Is(err, errBusy):
		w.Header().Set("Retry-After", retryAfterSeconds)
		writeError(w, http.StatusServiceUnavailable, err)
	case errors.As(err, new(BudgetError)):
		writeErrorReason(w, http.StatusServiceUnavailable, err, ReasonPlanBudgetExceeded)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("the request ended before it was priced: %w", err))
	default:
		writeError(w, http.StatusInternalServerError, err)
	}
}

// writeError writes an ErrorResponse.
func writeError(w http.ResponseWriter, status int, err error) {
	writeErrorReason(w, status, err, "")
}

// writeErrorReason writes an ErrorResponse with its reason, "" for none.
func writeErrorReason(w http.ResponseWriter, status int, err error, reason string) {
	body, merr := marshal(ErrorResponse{SchemaVersion: SchemaVersion, Error: err.Error(), Reason: reason})
	if merr != nil {
		body = []byte(`{"error": "internal"}` + "\n")
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
