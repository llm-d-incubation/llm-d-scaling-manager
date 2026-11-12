package whatifapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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

// Config is the server's configuration.
type Config struct {
	// Resolver resolves sources (cmd/whatif's).
	Resolver Resolver
	// WhatifSchema names the CLI schema the answer, refusal and flag shapes
	// follow ("schema-v3").
	WhatifSchema string
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
}

// inputsEntry is a source resolved: its id, its response body and its
// Pricer.
type inputsEntry struct {
	id     string
	doc    InputsResponse
	body   []byte
	pricer Pricer
}

// New is a server with empty caches.
func New(c Config) *Server {
	return &Server{resolver: c.Resolver, whatifSchema: c.WhatifSchema, inputs: newLRU[*inputsEntry](defaultInputs),
		plans: newLRU[[]byte](defaultPlans), curves: newLRU[*curveSet](defaultCurves), limits: newLRU[*limitEntry](defaultCurves)}
}

// Handler is the API's routes. The caller wraps it in authentication where
// the bind needs it (Authenticate).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/inputs", s.handleInputs)
	mux.HandleFunc("POST /v1/plan", s.handlePlan)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, fmt.Errorf("%s %s: no such endpoint; phase 0 serves POST /v1/inputs and POST /v1/plan, and no page", r.Method, r.URL.Path))
	})
	return mux
}

func (s *Server) handleInputs(w http.ResponseWriter, r *http.Request) {
	var req InputsRequest
	if !readJSON(w, r, &req) {
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
	body, err := marshal(doc)
	if err != nil {
		writeErr(w, err)
		return
	}
	e := s.inputs.add(doc.InputsID, &inputsEntry{id: doc.InputsID, doc: doc, body: body, pricer: res.Pricer})
	writeBody(w, e.body)
}

func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	var req PlanRequest
	if !readJSON(w, r, &req) {
		return
	}
	body, err := s.Plan(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeBody(w, body)
}

// errUnknownInputs is an inputsId the server does not hold.
var errUnknownInputs = errors.New("unknown inputsId: resolve the source with POST /v1/inputs (ids are kept while in use, the least used dropped first)")

// Plan prices req and returns the response body: from the cache when the same
// scenario was priced before, so the same scenario has the same bytes.
func (s *Server) Plan(req PlanRequest) ([]byte, error) {
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
	p := &planner{s: s, e: e, sc: sc}
	out, err := p.plan()
	if err != nil {
		return nil, err
	}
	out.ScenarioID = id
	body, err := marshal(out)
	if err != nil {
		return nil, err
	}
	return s.plans.add(id, body), nil
}

// readJSON reads the request body into v strictly: an unknown field, a
// second value, or a body past maxBody is a 400.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, fmt.Errorf("content type %q, want application/json", ct))
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
// an unknown inputsId, 500 for anything else.
func writeErr(w http.ResponseWriter, err error) {
	switch {
	case IsInputError(err):
		writeError(w, http.StatusBadRequest, err)
	case errors.Is(err, errUnknownInputs):
		writeError(w, http.StatusNotFound, err)
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
