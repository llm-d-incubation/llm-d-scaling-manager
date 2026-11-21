package whatifapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The blind backtest, in two steps: predict, then compare.
//
// POST /v1/predict (capacity-planner predict) prices a window from its
// load alone (each step's rate, lengths, prompt spread and replicas) on a
// fitted source, and answers a prediction file: the predicted figures of
// every step, a hash of the inputs they were priced from and a hash of the
// file itself. Nothing in it reads the window's latency: a window whose
// latency was taken out (capacity-planner strip-latency) predicts the same
// file as the whole recording.
//
// POST /v1/compare (capacity-planner compare) takes that file and the same
// window measured, checks both hashes, and scores it as a backtest scores
// one. A file edited after it was made, or a window that is not the one
// predicted, is refused.

// PredictionKind names a prediction file.
const PredictionKind = "capacity-planner-prediction"

// PredictionFormat is the prediction file's format version.
const PredictionFormat = 1

// DefaultMeasureWindow is the window a prediction prices each step over
// when none is asked: the router's rate window, five minutes.
const DefaultMeasureWindow = 300 * time.Second

// PredictRequest is POST /v1/predict's body: InputsID the fitted source,
// WindowInputsID the window to predict, and the backtest's step, settle,
// arrivals, assumed terms and configuration check. MeasureWindow is the
// window the measurement will be read over, the router's five minutes by
// default: each step is priced at the inputs of that window.
type PredictRequest struct {
	InputsID            string   `json:"inputsId"`
	WindowInputsID      string   `json:"windowInputsId"`
	Step                string   `json:"step,omitempty"`
	Settle              string   `json:"settle,omitempty"`
	MeasureWindow       string   `json:"measureWindow,omitempty"`
	ArrivalCV2          *float64 `json:"arrivalCV2,omitempty"`
	Assume              *Assume  `json:"assume,omitempty"`
	AllowConfigMismatch bool     `json:"allowConfigMismatch,omitempty"`
}

// Prediction is a prediction file. InputsHash names the inputs each step
// was priced from; PredictionHash the file itself, computed with it empty.
type Prediction struct {
	Kind           string         `json:"kind"`
	Format         int            `json:"format"`
	PlannerVersion string         `json:"plannerVersion"`
	WhatifSchema   string         `json:"whatifSchema"`
	MadeAt         time.Time      `json:"madeAt"`
	Model          string         `json:"model"`
	Fit            BacktestWindow `json:"fit"`
	Window         BacktestWindow `json:"window"`
	Step           float64        `json:"step"`
	Settle         float64        `json:"settle"`
	MeasureWindow  float64        `json:"measureWindow"`
	ArrivalCV2     *float64       `json:"arrivalCV2,omitempty"`
	Config         BacktestConfig `json:"config"`
	Assumed        []string       `json:"assumed"`
	Notes          []string       `json:"notes"`
	Steps          []BacktestStep `json:"steps"`
	InputsHash     string         `json:"inputsHash"`
	PredictionHash string         `json:"predictionHash"`
}

// CompareRequest is POST /v1/compare's body: the prediction file and the
// measured window's inputs.
type CompareRequest struct {
	Prediction json.RawMessage `json:"prediction"`
	InputsID   string          `json:"inputsId"`
	TTFT       *Objective      `json:"ttft,omitempty"`
	Measured   string          `json:"measured,omitempty"`
}

// PredictInfo is what a prediction is stamped with: the planner's release
// and schema, and the time it was made.
type PredictInfo struct {
	PlannerVersion, WhatifSchema string
	Now                          func() time.Time
}

// stepInputsRecord is what one step was priced from, hashed into
// InputsHash: never a latency.
type stepInputsRecord struct {
	Start         time.Time
	Cycles        int
	Rate          float64
	Prefill       int
	Decode        int
	PromptTokens  *float64
	OutputTokens  *float64
	PromptCV      *float64
	WindowSeconds float64
}

// inputsHash is the hash of the steps' inputs.
func inputsHash(steps []BacktestStep, windowSeconds float64) (string, error) {
	recs := make([]stepInputsRecord, len(steps))
	for i, s := range steps {
		recs[i] = stepInputsRecord{Start: s.Start, Cycles: s.Cycles, Rate: s.Rate, Prefill: s.Prefill, Decode: s.Decode,
			PromptTokens: s.PromptTokens, OutputTokens: s.OutputTokens, PromptCV: s.PromptCV, WindowSeconds: windowSeconds}
	}
	b, err := json.Marshal(recs)
	if err != nil {
		return "", fmt.Errorf("hashing the inputs: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// predictionHash is the hash of p with its PredictionHash empty.
func predictionHash(p Prediction) (string, error) {
	p.PredictionHash = ""
	b, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("hashing the prediction: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// measureWindowOfRequest reads a MeasureWindow, the default when empty.
func measureWindowOfRequest(s string) (time.Duration, error) {
	if s == "" {
		return DefaultMeasureWindow, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 15*time.Second || d > time.Hour {
		return 0, InputErrorf("measureWindow %q, want a duration from 15s to 1h", s)
	}
	return d, nil
}

// Predict prices the window on the fit and answers the prediction file:
// the CLI's predict, on the same path as POST /v1/predict.
func Predict(ctx context.Context, fit, window *Resolved, req PredictRequest, info PredictInfo) (*Prediction, error) {
	entry := func(res *Resolved) *inputsEntry {
		doc := res.Inputs
		doc.InputsID = contentID("inputs", res.Content)
		return &inputsEntry{id: doc.InputsID, doc: doc, pricer: res.Pricer, series: res.Series, latency: res.Latency, config: res.Config}
	}
	return predictEntries(ctx, entry(fit), entry(window), req, info)
}

// predictEntries is Predict on inputs entries. The window's latency, kept
// or taken out, is never read: the entry it prices from has none.
func predictEntries(ctx context.Context, fit, window *inputsEntry, req PredictRequest, info PredictInfo) (*Prediction, error) {
	if req.MeasureWindow == "" {
		// The router's rate window is no latency: a stripped window keeps it.
		for _, l := range window.latency {
			if l.Router != nil && l.Router.WindowSeconds > 0 {
				req.MeasureWindow = time.Duration(l.Router.WindowSeconds * float64(time.Second)).String()
				break
			}
		}
	}
	if req.MeasureWindow == "" {
		// No router window: the engines' histograms, over a minute.
		req.MeasureWindow = time.Duration(defaultMeasureWindow * float64(time.Second)).String()
	}
	mw, err := measureWindowOfRequest(req.MeasureWindow)
	if err != nil {
		return nil, err
	}
	blind := *window
	blind.latency = nil
	q, err := questionOf(fit, &blind, BacktestRequest{Step: req.Step, Settle: req.Settle, ArrivalCV2: req.ArrivalCV2, Assume: req.Assume,
		AllowConfigMismatch: req.AllowConfigMismatch}, time.Time{}, time.Time{}, "", true)
	if err != nil {
		return nil, err
	}
	q.measureWindow = mw.Seconds()
	_, steps, err := q.predictSteps(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now
	if info.Now != nil {
		now = info.Now
	}
	p := Prediction{Kind: PredictionKind, Format: PredictionFormat, PlannerVersion: info.PlannerVersion, WhatifSchema: info.WhatifSchema,
		MadeAt: now().UTC().Truncate(time.Second), Model: fit.doc.Model, Fit: windowOf(fit, nil, time.Time{}),
		Window: windowOf(&blind, q.testIndices, time.Time{}), Step: q.step.Seconds(), Settle: q.settle.Seconds(), MeasureWindow: q.measureWindow,
		ArrivalCV2: q.cv2, Config: q.config, Assumed: append([]string{}, q.assumed...), Steps: steps}
	// The window's own id names its bytes, latency included: left out, so
	// that a window with its latency taken out predicts the same file.
	p.Window.InputsID, p.Window.Source = "", ""
	p.Notes = []string{
		"Predicted from the window's load alone: each step's request rate, prompt and output lengths, prompt-length spread and " +
			"replicas, read over the measurement window ending at the step's last cycle. The window's latency, if the file carried any, was not read.",
		"Compare it with the same window measured (capacity-planner compare, POST /v1/compare). The hashes refuse a file edited and not " +
			"rehashed, and a window of another load; they show an edit, they do not sign the file.",
	}
	if q.config.Allowed {
		p.Notes = append(p.Notes, "CONFIGURATION DIFFERS, priced anyway: "+mismatchWords(q.config.Differences)+".")
	}
	if p.InputsHash, err = inputsHash(steps, q.measureWindow); err != nil {
		return nil, err
	}
	if p.PredictionHash, err = predictionHash(p); err != nil {
		return nil, err
	}
	return &p, nil
}

// ReadPrediction reads a prediction file strictly: an unknown field, a kind
// or format it does not know, or a hash that does not match the file is an
// InputError.
func ReadPrediction(b []byte) (*Prediction, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var p Prediction
	if err := dec.Decode(&p); err != nil {
		return nil, InputErrorf("not a prediction file: %v", err)
	}
	switch {
	case p.Kind != PredictionKind:
		return nil, InputErrorf("not a prediction file: kind %q, want %q", p.Kind, PredictionKind)
	case p.Format != PredictionFormat:
		return nil, InputErrorf("prediction format %d, this planner reads %d", p.Format, PredictionFormat)
	case dec.More():
		return nil, InputErrorf("not a prediction file: data after the prediction")
	case !(p.Step >= minBacktestStep.Seconds() && p.Step <= maxBacktestStep.Seconds()),
		!(p.Settle >= 0 && p.Settle <= maxBacktestSettle.Seconds()),
		!(p.MeasureWindow >= 15 && p.MeasureWindow <= 3600),
		len(p.Steps) == 0 || len(p.Steps) > maxBacktestSteps+1, len(p.PredictionHash) < 12 || len(p.InputsHash) < 12:
		return nil, InputErrorf("not a prediction file: its step, settle, measure window or steps are out of range")
	}
	for _, s := range p.Steps {
		for _, pts := range [][]BacktestPoint{s.TTFT, s.TPOT} {
			if len(pts) == 0 {
				continue
			}
			if len(pts) != len(backtestStats) {
				return nil, InputErrorf("not a prediction file: a step at %s has %d points, want %d", s.Start.UTC().Format(time.RFC3339), len(pts), len(backtestStats))
			}
			for i, x := range pts {
				if x.Statistic != statisticName(backtestStats[i]) {
					return nil, InputErrorf("not a prediction file: a step at %s has %q in place of %s", s.Start.UTC().Format(time.RFC3339), x.Statistic,
						statisticName(backtestStats[i]))
				}
			}
		}
	}
	h, err := predictionHash(p)
	if err != nil {
		return nil, InputError{Err: err}
	}
	if h != p.PredictionHash {
		return nil, InputErrorf("the prediction file does not match its own hash: it was changed after it was made; compare only an unedited prediction")
	}
	if h, err = inputsHash(p.Steps, p.MeasureWindow); err != nil || h != p.InputsHash {
		return nil, InputErrorf("the prediction's steps do not match its inputs hash: it was changed after it was made")
	}
	return &p, nil
}

// noPricer prices nothing: a comparison recomputes each step's inputs to
// check them against the prediction's, and never prices.
type noPricer struct{}

func (noPricer) Fleet(Knobs) (Fleet, error) {
	return nil, errors.New("not priced: a comparison reads inputs only")
}

// Compare scores a prediction file against the window measured: the CLI's
// compare, on the same path as POST /v1/compare.
func Compare(ctx context.Context, p *Prediction, measured *Resolved, req CompareRequest, whatifSchema string) (*BacktestResponse, error) {
	doc := measured.Inputs
	doc.InputsID = contentID("inputs", measured.Content)
	e := &inputsEntry{id: doc.InputsID, doc: doc, series: measured.Series, latency: measured.Latency, config: measured.Config}
	out, err := compareEntry(ctx, p, e, req)
	if err != nil {
		return nil, err
	}
	out.SchemaVersion, out.WhatifSchema = SchemaVersion, whatifSchema
	return out, nil
}

// compareEntry is Compare on an inputs entry.
func compareEntry(ctx context.Context, p *Prediction, e *inputsEntry, req CompareRequest) (*BacktestResponse, error) {
	if e.doc.Model != p.Model {
		return nil, InputErrorf("the prediction is of model %q and the measured window of model %q", p.Model, e.doc.Model)
	}
	if len(e.latency) != len(e.series) || len(e.series) == 0 || !HasLatency(e.latency) {
		return nil, InputErrorf("the measured window carries no latency to compare with: give the recording of that period, latency included")
	}
	// The window alone, as predict saw it: a measured recording that starts
	// before the window or ends after it holds cycles the prediction's
	// steps did not read.
	var idx []int
	for i, x := range e.series {
		if !x.At.Before(p.Window.From) && !x.At.After(p.Window.To) {
			idx = append(idx, i)
		}
	}
	e = &inputsEntry{id: e.id, doc: e.doc, series: seriesAt(e.series, idx), latency: latencyAt(e.latency, idx), config: e.config}
	q := &backtestQuestion{test: e, step: secondsDuration(p.Step), settle: secondsDuration(p.Settle),
		measureWindow: p.MeasureWindow, cv2: p.ArrivalCV2, config: p.Config, assumed: p.Assumed, predictionHash: p.PredictionHash,
		measured: MeasuredAuto}
	if req.Measured != "" {
		q.measured = req.Measured
	}
	switch req.Measured {
	case "", MeasuredAuto, MeasuredRouter, MeasuredEngine:
	default:
		return nil, InputErrorf("measured %q, want %s, %s or %s", req.Measured, MeasuredAuto, MeasuredRouter, MeasuredEngine)
	}
	var err error
	if q.target, q.targetAsked, err = targetOf(req.TTFT); err != nil {
		return nil, err
	}
	for i, x := range e.series {
		if !x.At.Before(p.Window.From) && !x.At.After(p.Window.To) {
			q.testIndices = append(q.testIndices, i)
		}
	}
	if len(q.testIndices) != p.Window.Cycles {
		return nil, InputErrorf("the measured source holds %d cycles from %s to %s, the prediction %d: it is not the window predicted",
			len(q.testIndices), p.Window.From.UTC().Format(time.RFC3339), p.Window.To.UTC().Format(time.RFC3339), p.Window.Cycles)
	}
	slices.SortStableFunc(q.testIndices, func(a, b int) int { return e.series[a].At.Compare(e.series[b].At) })
	if whole := stepInputsOf(seriesAt(e.series, q.testIndices)); whole.promptCV != nil {
		q.promptCV, q.promptMaxRatio = whole.promptCV, whole.promptMaxRatio
	}
	q.fit = &inputsEntry{pricer: noPricer{}}
	var inputs []BacktestStep
	_, inputs, err = q.predictSteps(ctx)
	if err != nil {
		return nil, err
	}
	if len(inputs) != len(p.Steps) {
		return nil, InputErrorf("the measured window has %d steps, the prediction %d: it is not the window predicted", len(inputs), len(p.Steps))
	}
	if h, err := inputsHash(inputs, p.MeasureWindow); err != nil || h != p.InputsHash {
		if err != nil {
			return nil, err
		}
		return nil, InputErrorf("the measured window's load is not the one predicted (its inputs hash %s, the prediction's %s): "+
			"compare the prediction with the recording of the window it was made for", h[:12], p.InputsHash[:12])
	}
	q.fit = nil
	q.source = q.measuredSource()
	steps, _, _ := q.layout()
	out := &BacktestResponse{Banner: backtestBanner, Config: p.Config, Step: p.Step, Settle: p.Settle, Measured: q.source,
		MeasuredNote: measuredNote(q.source, e.doc.Fleet.Prefill != nil), Assumed: append([]string{}, p.Assumed...), TTFT: q.targetAsked,
		Steps: []BacktestStep{}, Notes: []string{fmt.Sprintf("Compared with the prediction %s, made %s by planner %s.",
			p.PredictionHash[:12], p.MadeAt.UTC().Format(time.RFC3339), p.PlannerVersion)}}
	out.Fit = p.Fit
	out.Test = windowOf(e, q.testIndices, time.Time{})
	for k, s := range p.Steps {
		s = cloneStep(s)
		if st := steps[k]; st.anchor >= 0 {
			l := e.latency[st.anchor]
			if mw := measureWindowOf(l, q.source); mw != p.MeasureWindow {
				return nil, InputErrorf("the window was measured over %s, the prediction priced over %s: predict with measureWindow %s "+
					"(--measure-window), which is no latency, and compare again",
					secondsDuration(mw), secondsDuration(p.MeasureWindow), secondsDuration(mw))
			}
			scoreStep(&s, measuredOf(l, q.source, st.windowSeconds, e.doc.Fleet.Prefill != nil), q.target)
		}
		out.Steps = append(out.Steps, s)
	}
	q.finish(out)
	return out, nil
}

// secondsDuration is x seconds as a duration, to the nanosecond.
func secondsDuration(x float64) time.Duration {
	return time.Duration(math.Round(x * float64(time.Second)))
}

// latencyAt is the samples of lat at idx; nil with none.
func latencyAt(lat []LatencySample, idx []int) []LatencySample {
	if len(lat) == 0 {
		return nil
	}
	out := make([]LatencySample, len(idx))
	for i, j := range idx {
		out[i] = lat[j]
	}
	return out
}

// HasLatency says any cycle of lat carries a measured latency: a router
// histogram or mean, or an engine's. The router's rate window alone is none.
func HasLatency(lat []LatencySample) bool {
	for _, l := range lat {
		if r := l.Router; r != nil && (len(r.TTFTBuckets) > 0 || len(r.TPOTBuckets) > 0 || r.TTFTCountRate > 0 || r.TPOTCountRate > 0) {
			return true
		}
		if len(l.EngineTTFT) > 0 || len(l.EngineTPOT) > 0 || l.EngineTTFTMean > 0 || l.EngineITLMean > 0 {
			return true
		}
	}
	return false
}

// cloneStep is s with its points copied, so that scoring it leaves the
// prediction as it was.
func cloneStep(s BacktestStep) BacktestStep {
	s.TTFT, s.TPOT = slices.Clone(s.TTFT), slices.Clone(s.TPOT)
	return s
}

// maxCompareBody is the largest POST /v1/compare body: a prediction of
// maxBacktestSteps steps, about 2 KB each, with room.
const maxCompareBody = 8 << 20

// PredictionCSV is a prediction as CSV: a comment line with its hashes,
// then a row a step with each statistic predicted.
func PredictionCSV(p *Prediction) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s model=%s window=%s..%s predictionHash=%s inputsHash=%s planner=%s madeAt=%s\n", p.Kind, p.Model,
		p.Window.From.UTC().Format(time.RFC3339), p.Window.To.UTC().Format(time.RFC3339), p.PredictionHash, p.InputsHash, p.PlannerVersion,
		p.MadeAt.UTC().Format(time.RFC3339))
	head := make([]string, 0, 5+2*len(backtestStats))
	head = append(head, "start", "excluded", "rate", "prefill", "decode")
	for _, k := range []string{"ttft", "tpot"} {
		for _, q := range backtestStats {
			head = append(head, k+"_"+statisticName(q)+"_seconds")
		}
	}
	b.WriteString(strings.Join(head, ",") + "\n")
	num := func(v *float64) string {
		if v == nil {
			return ""
		}
		return strconv.FormatFloat(*v, 'g', 6, 64)
	}
	for _, s := range p.Steps {
		row := make([]string, 0, len(head))
		row = append(row, s.Start.UTC().Format(time.RFC3339), s.Excluded, strconv.FormatFloat(s.Rate, 'f', 3, 64), strconv.Itoa(s.Prefill), strconv.Itoa(s.Decode))
		for _, pts := range [][]BacktestPoint{s.TTFT, s.TPOT} {
			for i := range backtestStats {
				v := ""
				if i < len(pts) {
					v = num(pts[i].Predicted.Value)
				}
				row = append(row, v)
			}
		}
		b.WriteString(strings.Join(row, ",") + "\n")
	}
	return b.String()
}

// handlePredict is POST /v1/predict.
func (s *Server) handlePredict(w http.ResponseWriter, r *http.Request) {
	var req PredictRequest
	if !readJSON(w, r, &req) {
		return
	}
	fit, ok := s.inputs.get(req.InputsID)
	if !ok {
		writeErr(w, errUnknownInputs)
		return
	}
	window, ok := s.inputs.get(req.WindowInputsID)
	if !ok {
		writeErr(w, errUnknownInputs)
		return
	}
	if !acquire(s.planSlots) {
		writeErr(w, errBusy)
		return
	}
	defer func() { <-s.planSlots }()
	bctx, cancel := context.WithTimeoutCause(r.Context(), s.planBudget, errPlanBudget)
	defer cancel()
	p, err := predictEntries(bctx, fit, window, req, PredictInfo{PlannerVersion: s.plannerVersion, WhatifSchema: s.whatifSchema})
	if err != nil {
		if errors.Is(context.Cause(bctx), errPlanBudget) && r.Context().Err() == nil {
			err = BudgetError{Budget: s.planBudget}
		}
		writeErr(w, err)
		return
	}
	body, err := marshal(p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeBody(w, body)
}

// handleCompare is POST /v1/compare.
func (s *Server) handleCompare(w http.ResponseWriter, r *http.Request) {
	var req CompareRequest
	if !readJSONUpTo(w, r, &req, maxCompareBody) {
		return
	}
	p, err := ReadPrediction(req.Prediction)
	if err != nil {
		writeErr(w, err)
		return
	}
	e, ok := s.inputs.get(req.InputsID)
	if !ok {
		writeErr(w, errUnknownInputs)
		return
	}
	out, err := compareEntry(r.Context(), p, e, req)
	if err != nil {
		writeErr(w, err)
		return
	}
	out.SchemaVersion, out.WhatifSchema = SchemaVersion, s.whatifSchema
	body, err := marshal(out)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeBody(w, body)
}
