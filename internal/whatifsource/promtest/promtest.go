// Package promtest is a Prometheus HTTP API for tests: an httptest server
// that answers /api/v1/query and /api/v1/query_range with series the test
// sets per PromQL expression, in Prometheus' own JSON, so the client under
// test parses them as it parses a real server's. It records every request,
// so a test can hold a capture to range queries only.
package promtest

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Series is one series: its labels, and its value at an instant, ok false
// where the series is absent.
type Series struct {
	Labels map[string]string
	Value  func(at time.Time) (v float64, ok bool)
}

// Const is a series that holds v at every instant.
func Const(labels map[string]string, v float64) Series {
	return Series{Labels: labels, Value: func(time.Time) (float64, bool) { return v, true }}
}

// Request is one request the server answered.
type Request struct {
	Method, Path string
	Query        string
	// Start, End and Step are a range query's; Time an instant query's.
	Start, End, Time time.Time
	Step             time.Duration
}

// Server is the fake Prometheus.
type Server struct {
	*httptest.Server

	mu       sync.Mutex
	series   map[string][]Series
	failures map[string]string
	requests []Request
}

// New starts a server; Close stops it.
func New() *Server {
	s := &Server{series: map[string][]Series{}, failures: map[string]string{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Set makes query answer with series, replacing what it answered before. A
// query nothing was set for answers with no series.
func (s *Server) Set(query string, series ...Series) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.series[query] = series
}

// Add adds series to what query answers.
func (s *Server) Add(query string, series ...Series) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.series[query] = append(s.series[query], series...)
}

// Fail makes query answer with a Prometheus error.
func (s *Server) Fail(query, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[query] = message
}

// Requests is every request answered, in order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Queries is the distinct PromQL expressions asked, sorted.
func (s *Server) Queries() []string {
	seen := map[string]bool{}
	for _, r := range s.Requests() {
		seen[r.Query] = true
	}
	out := make([]string, 0, len(seen))
	for q := range seen {
		out = append(out, q)
	}
	sort.Strings(out)
	return out
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	req := Request{Method: r.Method, Path: r.URL.Path, Query: r.Form.Get("query")}
	var err error
	switch r.URL.Path {
	case "/api/v1/query_range":
		if req.Start, err = parseTime(r.Form.Get("start")); err == nil {
			if req.End, err = parseTime(r.Form.Get("end")); err == nil {
				req.Step, err = parseStep(r.Form.Get("step"))
			}
		}
	case "/api/v1/query":
		req.Time = time.Now()
		if t := r.Form.Get("time"); t != "" {
			req.Time, err = parseTime(t)
		}
	default:
		s.record(req)
		writeError(w, http.StatusNotFound, "not_found", "promtest answers query and query_range only")
		return
	}
	s.record(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	s.mu.Lock()
	series, failure := s.series[req.Query], s.failures[req.Query]
	s.mu.Unlock()
	if failure != "" {
		writeError(w, http.StatusUnprocessableEntity, "execution", failure)
		return
	}
	if req.Path == "/api/v1/query" {
		writeData(w, "vector", vector(series, req.Time))
		return
	}
	if req.Step <= 0 {
		writeError(w, http.StatusBadRequest, "bad_data", "zero or negative query resolution step width")
		return
	}
	writeData(w, "matrix", matrix(series, req.Start, req.End, req.Step))
}

func (s *Server) record(r Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r)
}

func vector(series []Series, at time.Time) []any {
	out := []any{}
	for _, sr := range series {
		if v, ok := sr.Value(at); ok {
			out = append(out, map[string]any{"metric": labelsOf(sr), "value": point(at, v)})
		}
	}
	return out
}

func matrix(series []Series, start, end time.Time, step time.Duration) []any {
	out := []any{}
	for _, sr := range series {
		var values []any
		for at := start; !at.After(end); at = at.Add(step) {
			if v, ok := sr.Value(at); ok {
				values = append(values, point(at, v))
			}
		}
		if len(values) > 0 {
			out = append(out, map[string]any{"metric": labelsOf(sr), "values": values})
		}
	}
	return out
}

func labelsOf(s Series) map[string]string {
	if s.Labels == nil {
		return map[string]string{}
	}
	return s.Labels
}

// point is one sample as Prometheus writes it: [seconds, "value"].
func point(at time.Time, v float64) []any {
	return []any{float64(at.UnixMilli()) / 1000, formatValue(v)}
}

func formatValue(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func parseTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("cannot parse %q to a valid timestamp", s)
	}
	return time.UnixMilli(int64(math.Round(f * 1000))).UTC(), nil
}

func parseStep(s string) (time.Duration, error) {
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Duration(f * float64(time.Second)), nil
	}
	return time.ParseDuration(s)
}

func writeData(w http.ResponseWriter, resultType string, result []any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "success",
		"data": map[string]any{"resultType": resultType, "result": result}})
}

func writeError(w http.ResponseWriter, code int, errorType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "errorType": errorType, "error": msg})
}
