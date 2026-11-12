package whatifapi

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi/apitest"
)

var update = flag.Bool("update", false, "rewrite testdata/schema-v1.txt")

// post posts body to path and returns the status and the response body.
func post(t *testing.T, h http.Handler, path string, body any) (int, []byte) {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// inputsOf resolves the stub's inputs on s and returns the id.
func inputsOf(t *testing.T, h http.Handler) string {
	t.Helper()
	code, body := post(t, h, "/v1/inputs", InputsRequest{Source: &SourceRequest{Kind: SourceRecording, Name: "run.jsonl.gz"}})
	require.Equal(t, http.StatusOK, code, string(body))
	var in InputsResponse
	require.NoError(t, json.Unmarshal(body, &in))
	require.True(t, strings.HasPrefix(in.InputsID, "sha256:"), in.InputsID)
	return in.InputsID
}

// plan posts a plan and decodes the response.
func plan(t *testing.T, h http.Handler, req PlanRequest) (PlanResponse, []byte) {
	t.Helper()
	code, body := post(t, h, "/v1/plan", req)
	require.Equal(t, http.StatusOK, code, string(body))
	var out PlanResponse
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&out))
	return out, body
}

func f64(x float64) *float64 { return &x }
func intp(n int) *int        { return &n }

// The wire's key list is pinned: every key path the request and response
// types can carry, with its JSON type, is in testdata/schema-v1.txt, checked
// as the CLI's schema-v*.txt is. -update rewrites it.
func TestSchemaIsPinned(t *testing.T) {
	var paths []string
	walked := map[string]reflect.Type{
		"inputsRequest": reflect.TypeFor[InputsRequest](), "inputs": reflect.TypeFor[InputsResponse](),
		"planRequest": reflect.TypeFor[PlanRequest](), "plan": reflect.TypeFor[PlanResponse](), "error": reflect.TypeFor[ErrorResponse](),
	}
	for typ, name := range namedTypes {
		walked[name] = typ
	}
	for name, typ := range walked {
		schemaPaths(typ, name, &paths)
	}
	slices.Sort(paths)
	got := strings.Join(paths, "\n") + "\n"
	path := filepath.Join("testdata", "schema-v1.txt")
	if *update {
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
		return
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "a schema version with no pinned key list")
	assert.Equal(t, strings.ReplaceAll(string(want), "\r\n", "\n"), got,
		"the API's schema moved from its pinned list (SchemaVersion %d): bump it unless every change only adds a key", SchemaVersion)
	require.Greater(t, len(paths), 120, "walked too few key paths")
}

// namedTypes are the types listed once under their own name, and by name
// wherever they occur: a figure is one shape everywhere.
var namedTypes = map[reflect.Type]string{
	reflect.TypeFor[Figure](): "Figure", reflect.TypeFor[Refusal](): "Refusal",
	reflect.TypeFor[Line](): "Line", reflect.TypeFor[Markers](): "Markers",
}

// schemaPaths walks a wire type and appends "path type" for every key. Mix
// is written as a string or an object (Mix.MarshalJSON).
func schemaPaths(t reflect.Type, prefix string, out *[]string) {
	if name, ok := namedTypes[t]; ok && prefix != name {
		*out = append(*out, prefix+" "+name)
		return
	}
	switch {
	case t == reflect.TypeFor[Mix]():
		*out = append(*out, prefix+" string|object")
		schemaPaths(reflect.TypeFor[struct {
			PromptTokens float64 `json:"promptTokens"`
			OutputTokens float64 `json:"outputTokens"`
		}](), prefix+"{}", out)
		return
	case t == reflect.TypeFor[time.Time]():
		*out = append(*out, prefix+" string")
		return
	}
	switch t.Kind() {
	case reflect.Pointer:
		schemaPaths(t.Elem(), prefix, out)
	case reflect.Slice:
		*out = append(*out, prefix+" array")
		schemaPaths(t.Elem(), prefix+"[]", out)
	case reflect.Map:
		*out = append(*out, prefix+" object")
		schemaPaths(t.Elem(), prefix+".*", out)
	case reflect.Struct:
		if !strings.HasSuffix(prefix, "{}") {
			*out = append(*out, prefix+" object")
		}
		for i := range t.NumField() {
			f := t.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			switch {
			case f.Anonymous && name == "":
				schemaPaths(f.Type, prefix+"{}", out)
			case name == "" || name == "-":
			default:
				schemaPaths(f.Type, strings.TrimSuffix(prefix, "{}")+"."+name, out)
			}
		}
	case reflect.String:
		*out = append(*out, prefix+" string")
	case reflect.Bool:
		*out = append(*out, prefix+" boolean")
	case reflect.Int, reflect.Float64:
		*out = append(*out, prefix+" number")
	default:
		*out = append(*out, prefix+" "+t.Kind().String())
	}
}

// No number where a refusal is: every plan of the synthetic fleets, answered
// and refused, passes the wire check, and a refused limit is a refusal.
func TestPlanRefusalShape(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		h := New(Config{Resolver: &stubResolver{refusePrefill: refuse}, WhatifSchema: "schema-v3"}).Handler()
		id := inputsOf(t, h)
		for _, req := range []PlanRequest{
			{InputsID: id},
			{InputsID: id, Traffic: &Traffic{Classes: &Classes{Critical: &ClassRate{Rate: f64(30)}}}},
			{InputsID: id, SLO: &SLORequest{TPOT: &Objective{Statistic: "p90"}}},
			{InputsID: id, SLO: &SLORequest{TPOT: &Objective{Statistic: "p90"}}, Assume: &Assume{TPOTRatio: f64(1.3)},
				Views: &ViewRequest{AllTrafficPerBucket: true}},
		} {
			out, body := plan(t, h, req)
			assert.Empty(t, apitest.CheckFigures(body), "%s", body)
			if refuse {
				require.NotNil(t, out.Limits.Prefill.Refused)
				assert.Nil(t, out.Limits.Prefill.Value)
				assert.Equal(t, "NoPrefillCapacity", out.Limits.Fleet.Refused.Reason)
				assert.Nil(t, out.Limits.Fleet.Value)
				assert.NotNil(t, out.Limits.Chosen.Headroom.Refused, "a headroom under a refused limit is refused")
			}
		}
	}
}

// The checker itself: a refusal beside a number, and a refusal written as 0,
// are each found.
func TestCheckFiguresFindsBreaches(t *testing.T) {
	assert.Len(t, apitest.CheckFigures([]byte(`{"limits": {"prefill": {"value": 3, "refused": {"reason": "Saturated"}}}}`)), 1)
	assert.Len(t, apitest.CheckFigures([]byte(`{"limits": {"prefill": {"value": 0}}}`)), 1)
	assert.Empty(t, apitest.CheckFigures([]byte(`{"limits": {"chosen": {"rate": 2, "headroom": {"value": 0}}}}`)))
}

// The curves follow the knobs, not the rate or the targets: the same scenario
// is the same id and the same bytes, priced once; a rate or a target moves the
// id and prices no curve; a replica count prices a new curve set.
func TestCaching(t *testing.T) {
	curves, limits := 0, 0
	onCurves = func() { curves++ }
	onLimits = func() { limits++ }
	t.Cleanup(func() { onCurves, onLimits = nil, nil })
	h := New(Config{Resolver: &stubResolver{}, WhatifSchema: "schema-v3"}).Handler()
	id := inputsOf(t, h)
	base := PlanRequest{InputsID: id, Traffic: &Traffic{Classes: &Classes{Critical: &ClassRate{Rate: f64(4)}}}}
	a, aBody := plan(t, h, base)
	require.Equal(t, 1, curves)
	b, bBody := plan(t, h, base)
	assert.Equal(t, a.ScenarioID, b.ScenarioID)
	assert.Equal(t, aBody, bBody)
	assert.Equal(t, 1, curves, "the same scenario priced its curves again")

	require.Equal(t, 1, limits)

	// A rate move prices the points alone.
	moved := base
	moved.Traffic = &Traffic{Classes: &Classes{Critical: &ClassRate{Rate: f64(5)}}}
	c, _ := plan(t, h, moved)
	assert.NotEqual(t, a.ScenarioID, c.ScenarioID)
	assert.Equal(t, 5.0, c.Limits.Chosen.Rate)
	assert.Equal(t, 1, curves, "a rate priced the curves again")
	assert.Equal(t, 1, limits, "a rate priced the limits again")
	assert.Equal(t, a.Limits.Fleet, c.Limits.Fleet)

	// A target move prices the limits, not the curves.
	moved.SLO = &SLORequest{TTFT: &Objective{Target: f64(0.5)}}
	e, _ := plan(t, h, moved)
	assert.NotEqual(t, c.ScenarioID, e.ScenarioID)
	assert.Equal(t, 1, curves, "a target priced the curves again")
	assert.Equal(t, 2, limits, "a target must price its own limits")
	assert.Equal(t, []string{"Poisson arrivals", "a stub's note at the target", "a stub's note at the rate"}, e.Assumed)

	more := base
	more.Fleet = &FleetKnob{Prefill: intp(2)}
	d, _ := plan(t, h, more)
	assert.NotEqual(t, a.ScenarioID, d.ScenarioID)
	assert.Equal(t, 2, curves, "a replica count must price its own curves")
	assert.Equal(t, 3, limits, "a replica count must price its own limits")
}

// The marginal steps read one more replica of each role, and the binding
// role is the one with the lower limit.
func TestMarginal(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{}, WhatifSchema: "schema-v3"}).Handler()
	out, _ := plan(t, h, PlanRequest{InputsID: inputsOf(t, h)})
	assert.Equal(t, FleetCounts{Prefill: 2, Decode: 1}, out.Marginal.PrefillPlusOne.Fleet)
	assert.Equal(t, FleetCounts{Prefill: 1, Decode: 2}, out.Marginal.DecodePlusOne.Fleet)
	// Prefill: knee 10, limit 10 x (1 - 0.1/0.2) = 5. Decode: TPOT 12 ms
	// allows 7, under its cap 0.85 x 12. Prefill binds at 5.
	assert.Equal(t, []string{"prefill"}, out.Limits.Fleet.Binding)
	assert.InDelta(t, 5, *out.Limits.Fleet.Value, 1e-12)
	// +1 prefill: knee 20, the lower knee decode's 12, limit 6: +1.
	require.NotNil(t, out.Marginal.PrefillPlusOne.Delta.Value)
	assert.InDelta(t, 1, *out.Marginal.PrefillPlusOne.Delta.Value, 1e-12)
	// +1 decode: the role that does not bind changes nothing.
	require.NotNil(t, out.Marginal.DecodePlusOne.Delta.Value)
	assert.InDelta(t, 0, *out.Marginal.DecodePlusOne.Delta.Value, 1e-12)
	assert.Equal(t, []string{"prefill"}, out.Marginal.DecodePlusOne.Binding)
}

// A request asked wrongly is a 400 naming the field; an unknown id a 404.
func TestRequestsRefused(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{}, WhatifSchema: "schema-v3"}).Handler()
	id := inputsOf(t, h)
	for name, c := range map[string]struct {
		body any
		code int
		want string
	}{
		"regular class":   {PlanRequest{InputsID: id, Traffic: &Traffic{Classes: &Classes{Regular: &ClassRate{Rate: f64(1)}}}}, 400, "regular"},
		"zero rate":       {PlanRequest{InputsID: id, Traffic: &Traffic{Classes: &Classes{Critical: &ClassRate{Rate: f64(0)}}}}, 400, "critical.rate"},
		"replicas":        {PlanRequest{InputsID: id, Fleet: &FleetKnob{Decode: intp(65)}}, 400, "fleet.decode"},
		"statistic":       {PlanRequest{InputsID: id, SLO: &SLORequest{TTFT: &Objective{Statistic: "p100"}}}, 400, "slo.ttft.statistic"},
		"headroom":        {PlanRequest{InputsID: id, Headroom: f64(1.5)}, 400, "headroom"},
		"ratio on mean":   {PlanRequest{InputsID: id, Assume: &Assume{TPOTRatio: f64(1.3)}}, 400, "tpotRatio"},
		"bucket no view":  {PlanRequest{InputsID: id, SLO: &SLORequest{TTFT: &Objective{BucketTargets: map[string]float64{"8k": 1}}}}, 400, "allTrafficPerBucket"},
		"no such bucket":  {PlanRequest{InputsID: id, Views: &ViewRequest{AllTrafficPerBucket: true}, SLO: &SLORequest{TTFT: &Objective{BucketTargets: map[string]float64{"2k": 1}}}}, 400, "no such bucket"},
		"fleet's own":     {PlanRequest{InputsID: id, Assume: &Assume{VP: f64(2e9)}}, 400, "assume-vp"},
		"unknown id":      {PlanRequest{InputsID: "sha256:none"}, 404, "unknown inputsId"},
		"unknown field":   {map[string]any{"inputsId": id, "rate": 3}, 400, "unknown field"},
		"resolver's":      {InputsRequest{Model: "bad"}, 400, "no model bad"},
		"mix not a shape": {map[string]any{"inputsId": id, "traffic": map[string]any{"mix": "recorded"}}, 400, "mix"},
	} {
		t.Run(name, func(t *testing.T) {
			path := "/v1/plan"
			if _, ok := c.body.(InputsRequest); ok {
				path = "/v1/inputs"
			}
			code, body := post(t, h, path, c.body)
			assert.Equal(t, c.code, code, "%s", body)
			assert.Contains(t, string(body), c.want)
		})
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code, "phase 0 serves no page")
}
