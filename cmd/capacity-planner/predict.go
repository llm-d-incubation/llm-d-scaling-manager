package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// The blind backtest in two steps (POST /v1/predict and /v1/compare):
//
//	capacity-planner predict --fit FIT --window LOAD -o prediction.json [--csv prediction.csv]
//	capacity-planner compare --prediction prediction.json --measured WINDOW
//
// LOAD may have its latency taken out (capacity-planner strip-latency): the
// prediction reads none.

// plannerVersion is the release and commit a prediction is stamped with.
func plannerVersion() string { return version + " (commit " + learnrec.Commit() + ")" }

// predictNow is the time a prediction is stamped with; tests fix it.
var predictNow = time.Now

// runPredict is `capacity-planner predict`.
func runPredict(ctx context.Context, args []string, out, warn io.Writer) error {
	fs := newFlags("predict", out)
	var f backtestFlags
	var window, dst, csv, measureWindow string
	fs.StringVar(&f.fit, "fit", "", "the recording to fit on (required)")
	fs.StringVar(&window, "window", "", "the window to predict: a recording, its latency kept or taken out (strip-latency) (required)")
	fs.StringVar(&dst, "o", "", "where to write the prediction file, JSON (required)")
	fs.StringVar(&csv, "csv", "", "also write the prediction as CSV here")
	fs.StringVar(&f.model, "model", "", "the model; empty takes the source's only model")
	fs.StringVar(&f.prefillVariant, "prefill-variant", "", "the prefill variant, when the model has several")
	fs.StringVar(&f.decodeVariant, "variant", "", "the decode variant, when the model has several")
	fs.StringVar(&f.step, "step", whatifapi.DefaultBacktestStep.String(), "the length of a step, 1m to 1h")
	fs.StringVar(&f.settle, "settle", whatifapi.DefaultBacktestSettle.String(), "how long after a change of replicas, and after the window's start, a step is not judged")
	fs.StringVar(&measureWindow, "measure-window", "", "the window the latency will be measured over; default the window's router rate window, else 1m")
	fs.Float64Var(&f.arrivalCV2, "arrival-cv2", 0, "the window's arrivals' Ca^2, when known; default random (1), flagged")
	f.registerAssume(fs)
	fs.BoolVar(&f.allowMismatch, "allow-config-mismatch", false, "predict a window of another configuration, with the difference as a caveat")
	if ok, err := parse(fs, args); !ok {
		return err
	}
	f.parsed(fs)
	if f.fit == "" || window == "" || dst == "" {
		return usagef("--fit, --window and -o are required")
	}
	if f.rankMixtureTTFT {
		return usagef("--%s=%s is backtest's only: the model is fitted on a backtest's fit window", experimentalTTFTFlag, experimentalTTFTV2)
	}
	ctrllog.SetLogger(logr.Discard())
	bt, err := f.request()
	if err != nil {
		return err
	}
	fit, err := f.resolve(ctx, f.fit, 0, warn)
	if err != nil {
		return err
	}
	win, err := f.resolve(ctx, window, 0, warn)
	if err != nil {
		return err
	}
	if whatifapi.HasLatency(win.Latency) {
		fmt.Fprintln(warn, progName+": predict: the window carries latency series; they are not read (strip-latency takes them out)")
	}
	req := whatifapi.PredictRequest{Step: bt.Step, Settle: bt.Settle, MeasureWindow: measureWindow, ArrivalCV2: bt.ArrivalCV2,
		Assume: bt.Assume, AllowConfigMismatch: bt.AllowConfigMismatch}
	p, err := whatifapi.Predict(ctx, fit, win, req, whatifapi.PredictInfo{PlannerVersion: plannerVersion(),
		WhatifSchema: "schema-v" + strconv.Itoa(schemaVersion), Now: predictNow})
	if err != nil {
		return backtestError(err)
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(dst, func(w io.Writer) error { _, err := w.Write(append(b, '\n')); return err }); err != nil {
		return err
	}
	if csv != "" {
		if err := writeFileAtomic(csv, func(w io.Writer) error { _, err := io.WriteString(w, whatifapi.PredictionCSV(p)); return err }); err != nil {
			return err
		}
	}
	priced := 0
	for _, s := range p.Steps {
		if s.Excluded == "" {
			priced++
		}
	}
	fmt.Fprintf(out, "predicted %d of %d steps of %s (%s to %s UTC) from its load alone\nwrote %s (prediction %s, inputs %s)\n",
		priced, len(p.Steps), filepath.Base(window), p.Window.From.UTC().Format("2006-01-02 15:04"), p.Window.To.UTC().Format("2006-01-02 15:04"),
		dst, p.PredictionHash[:12], p.InputsHash[:12])
	return nil
}

// runCompare is `capacity-planner compare`.
func runCompare(ctx context.Context, args []string, out, warn io.Writer) error {
	fs := newFlags("compare", out)
	var f backtestFlags
	var prediction, measured string
	fs.StringVar(&prediction, "prediction", "", "the prediction file capacity-planner predict wrote (required)")
	fs.StringVar(&measured, "measured", "", "the same window measured: its recording, latency included (required)")
	fs.StringVar(&f.model, "model", "", "the model; empty takes the source's only model")
	fs.StringVar(&f.prefillVariant, "prefill-variant", "", "the prefill variant, when the model has several")
	fs.StringVar(&f.decodeVariant, "variant", "", "the decode variant, when the model has several")
	fs.StringVar(&f.ttft, "ttft-target", "", "a TTFT target to judge each step by, predicted and measured: 500ms or 500ms@p90")
	fs.StringVar(&f.measured, "measured-by", whatifapi.MeasuredAuto, "the latency scored against: auto, router or engine")
	fs.StringVar(&f.output, "o", outputText, "output: text or json")
	if ok, err := parse(fs, args); !ok {
		return err
	}
	if prediction == "" || measured == "" {
		return usagef("--prediction and --measured are required")
	}
	if f.output != outputText && f.output != outputJSON {
		return usagef("-o %q, want text or json", f.output)
	}
	ctrllog.SetLogger(logr.Discard())
	b, err := os.ReadFile(prediction)
	if err != nil {
		return usageError{err}
	}
	p, err := whatifapi.ReadPrediction(b)
	if err != nil {
		return usageError{err}
	}
	o, err := objectiveOf("ttft-target", f.ttft)
	if err != nil {
		return err
	}
	m, err := f.resolve(ctx, measured, 0, warn)
	if err != nil {
		return err
	}
	bt, err := whatifapi.Compare(ctx, p, m, whatifapi.CompareRequest{TTFT: o, Measured: f.measured}, "schema-v"+strconv.Itoa(schemaVersion))
	if err != nil {
		return backtestError(err)
	}
	if f.output == outputJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		err = enc.Encode(bt)
	} else {
		err = writeBacktestText(out, bt)
	}
	if err != nil {
		return err
	}
	if bt.Summary.Judged == 0 {
		return exitStatus(3)
	}
	return nil
}
