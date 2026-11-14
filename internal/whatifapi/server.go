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
)

// defaultMaxInputs is how many sources are resolved at once by default: each
// is a replay or a Prometheus capture of seconds to minutes.
const defaultMaxInputs = 2

// retryAfterSeconds is what a busy server asks a client to wait.
const retryAfterSeconds = "1"

// Config is the server's configuration.
type Config struct {
	// Resolver resolves sources (cmd/capacity-planner's).
	Resolver Resolver
	// WhatifSchema names the CLI schema the answer, refusal and flag shapes
	// follow ("schema-v3").
	WhatifSchema string
	// MaxPlans and MaxInputs are how many plans and how many inputs are
	// priced at once; past them a request is 503 with Retry-After. 0 is the
	// default: GOMAXPROCS plans, defaultMaxInputs inputs.
	MaxPlans, MaxInputs int
}

// Server serves the API. Its state is caches only: an inputs entry per
// inputsId, a response per scenarioId, a curve set per fleet, and the limits
// per fleet and targets.
type Server struct {
	resolver     Resolver
	whatifSchema string
	inputs       *lru[*inputsEntry]
	plans        *lru[[]byte]
	curves       *lru[*curveSet]
	limits       *lru[*limitEntry]
	// planSlots and inputSlots bound the work in flight: a token per request
	// priced.
	planSlots, inputSlots chan struct{}
	// planFlight, curveFlight and limitFlight price each key once at a time:
	// identical requests in flight share one pricing and one slot.
	planFlight  flight[[]byte]
	curveFlight flight[*curveSet]
	limitFlight flight[*limitEntry]
	// onCurves and onLimits, when set, are called each time a curve set or a
	// limit entry is priced: the caching tests count them.
	onCurves, onLimits func()
}

// inputsEntry is a source resolved: its id, the inputs a plan reads its
// defaults from, and its Pricer.
type inputsEntry struct {
	id     string
	doc    InputsResponse
	pricer Pricer
}

// New is a server with empty caches.
func New(c Config) *Server {
	plans, inputs := c.MaxPlans, c.MaxInputs
	if plans <= 0 {
		plans = runtime.GOMAXPROCS(0)
	}
	if inputs <= 0 {
		inputs = defaultMaxInputs
	}
	return &Server{resolver: c.Resolver, whatifSchema: c.WhatifSchema, inputs: newLRU[*inputsEntry](defaultInputs),
		plans: newLRU[[]byte](defaultPlans), curves: newLRU[*curveSet](defaultCurves), limits: newLRU[*limitEntry](defaultCurves),
		planSlots: make(chan struct{}, plans), inputSlots: make(chan struct{}, inputs)}
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
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, fmt.Errorf("%s %s: no such endpoint; phase 0 serves POST /v1/inputs and POST /v1/plan, and no page", r.Method, r.URL.Path))
	})
	return mux
}

// handleInputs is POST /v1/inputs: the source resolved, kept under its id,
// and answered with the caller's own source.
func (s *Server) handleInputs(w http.ResponseWriter, r *http.Request) {
	var req InputsRequest
	if !readJSON(w, r, &req) {
		return
	}
	if !acquire(s.inputSlots) {
		writeErr(w, errBusy)
		return
	}
	defer func() { <-s.inputSlots }()
	res, err := s.resolver.Resolve(r.Context(), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	doc := res.Inputs
	doc.SchemaVersion, doc.WhatifSchema = SchemaVersion, s.whatifSchema
	doc.InputsID = contentID("inputs", res.Content)
	// The entry kept is the first caller's, but each caller is answered with
	// its own source: two recordings of the same bytes share an id, not a
	// name.
	s.inputs.add(doc.InputsID, &inputsEntry{id: doc.InputsID, doc: doc, pricer: res.Pricer})
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
	if !readJSON(w, r, &req) {
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
		if !acquire(s.planSlots) {
			return nil, errBusy
		}
		defer func() { <-s.planSlots }()
		out, err := (&planner{s: s, e: e, sc: sc}).plan(ctx)
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

// readJSON reads the request body into v strictly: a media type other than
// application/json (none included, so that no form a page posts across sites
// is read) is 415; an unknown field, a second value, or a body past maxBody
// is a 400.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, fmt.Errorf("content type %q, want application/json", r.Header.Get("Content-Type")))
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
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
// an unknown inputsId, 503 for a busy server (with Retry-After) or a request
// that ended, 500 for anything else.
func writeErr(w http.ResponseWriter, err error) {
	switch {
	case IsInputError(err):
		writeError(w, http.StatusBadRequest, err)
	case errors.Is(err, errUnknownInputs):
		writeError(w, http.StatusNotFound, err)
	case errors.As(err, new(ForbiddenError)):
		writeError(w, http.StatusForbidden, err)
	case errors.Is(err, errBusy):
		w.Header().Set("Retry-After", retryAfterSeconds)
		writeError(w, http.StatusServiceUnavailable, err)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("the request ended before it was priced: %w", err))
	default:
		writeError(w, http.StatusInternalServerError, err)
	}
}

// writeError writes an ErrorResponse.
func writeError(w http.ResponseWriter, status int, err error) {
	body, merr := marshal(ErrorResponse{SchemaVersion: SchemaVersion, Error: err.Error()})
	if merr != nil {
		body = []byte(`{"error": "internal"}` + "\n")
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
