package whatifapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scEntry is the synthetic window as one source, its measured latency
// scaled by latencyX and every rate times rateX, with its folds: the stub
// fitted at half and three quarters of its cycles.
func scEntry(t *testing.T, latencyX, rateX float64) (*inputsEntry, []FoldFit) {
	t.Helper()
	r := &stubResolver{}
	s, l := btSeries(480, 0, latencyX)
	for i := range s {
		s[i].Rate *= rateX
		if l[i].Router != nil {
			for j := range l[i].Router.TTFTBuckets {
				l[i].Router.TTFTBuckets[j].Rate *= rateX
			}
			for j := range l[i].Router.TPOTBuckets {
				l[i].Router.TPOTBuckets[j].Rate *= rateX
			}
			l[i].Router.TTFTSumRate *= rateX
			l[i].Router.TTFTCountRate *= rateX
			l[i].Router.TPOTSumRate *= rateX
			l[i].Router.TPOTCountRate *= rateX
		}
	}
	e := btEntry(t, r, "src", s, l, btConfig("H100"))
	e.doc.Source.Cycle = len(s) // answered at its last cycle
	folds := make([]FoldFit, 0, len(selfCheckFractions))
	for _, c := range SelfCheckCycles(len(s)) {
		folds = append(folds, FoldFit{Cycle: c, At: s[c-1].At, Pricer: r})
	}
	return e, folds
}

// The self-check is not run, and says why, on a source it cannot judge.
func TestSelfCheck_SaysWhyItDidNotRun(t *testing.T) {
	e, folds := scEntry(t, 1, 1)
	short := *e
	short.series, short.latency = e.series[:200], e.latency[:200]
	assert.Contains(t, selfCheckEntry(context.Background(), &short, folds, SelfCheckInfo{}).Why, "a self-check needs 1 h")

	blind := *e
	blind.latency = make([]LatencySample, len(e.latency))
	assert.Contains(t, selfCheckEntry(context.Background(), &blind, folds, SelfCheckInfo{}).Why, "no measured latency")

	none := selfCheckEntry(context.Background(), e, nil, SelfCheckInfo{})
	assert.Equal(t, SelfCheckNotRun, none.Status)
	assert.Contains(t, none.Line, "Self-check not run")

	aggregated := *e
	aggregated.doc.Fleet.Prefill = nil
	assert.Contains(t, selfCheckEntry(context.Background(), &aggregated, folds, SelfCheckInfo{}).Why, "no prefill variant")
}

// A planner that reads far from what the fleet measured is inconsistent:
// the rules that failed say by how much, the answer says what not to trust,
// and an issue is offered; one past its knee where the fleet served is
// inconsistent too.
func TestSelfCheck_FindsAnInconsistency(t *testing.T) {
	for name, c := range map[string]struct {
		latencyX, rateX float64
		rule            string
	}{
		"reads high": {0.2, 1, "ttft-bias"},
		"reads low":  {8, 1, "ttft-bias"},
		"overloaded": {1, 2.2, "ttft-overloaded"},
	} {
		t.Run(name, func(t *testing.T) {
			e, folds := scEntry(t, c.latencyX, c.rateX)
			sc := selfCheckEntry(context.Background(), e, folds, SelfCheckInfo{PlannerVersion: "vtest"})
			require.Equal(t, SelfCheckInconsistent, sc.Status, "%s: %s", sc.Line, sc.Why)
			failed := []string{}
			for _, r := range sc.Checks {
				if !r.Passed {
					failed = append(failed, r.Name)
				}
			}
			assert.Contains(t, failed, c.rule)
			assert.Contains(t, sc.Trust, "Do not trust prefill sizing")
			require.NotEmpty(t, sc.IssueURL)
			assert.True(t, strings.HasPrefix(sc.IssueURL, selfCheckIssueRepo+"?"), sc.IssueURL)
			assert.Len(t, sc.Folds, 2)
			assert.True(t, strings.HasPrefix(sc.Line, "Self-check: tested on the last "), sc.Line)
		})
	}
}

// judge, figure by figure, at each rule's edges: ok, weak, inconsistent and
// not run, what each says to trust, and the issue only when inconsistent.
func TestSelfCheck_JudgeAtTheEdges(t *testing.T) {
	e, _ := scEntry(t, 1, 1)
	fig := func(steps, within int, bias float64) *SelfCheckFigure {
		return &SelfCheckFigure{Statistic: statP90, Steps: steps, Within: within, Bias: &bias, Judged: steps, Refused: map[string]int{},
			Levels: selfCheckLevelsOK, WithinBound: ptr(float64(within) / float64(steps))}
	}
	// with is fig changed by set.
	with := func(f *SelfCheckFigure, set func(*SelfCheckFigure)) *SelfCheckFigure {
		set(f)
		return f
	}
	over := func(overloaded, judged int) *SelfCheckFigure {
		return &SelfCheckFigure{Statistic: statP90, Judged: judged, Overloaded: overloaded, PastKnee: &Band{Lo: 1, Hi: 2}, Refused: map[string]int{}}
	}
	for name, c := range map[string]struct {
		ttft, tpot *SelfCheckFigure
		status     string
		failed     string
		trust      string
	}{
		"ok":                      {fig(12, 9, 1), fig(12, 12, 1), SelfCheckOK, "", ""},
		"bias at 0.95 holds":      {fig(12, 12, 0.95), nil, SelfCheckOK, "", ""},
		"bias at 1.25 holds":      {fig(12, 12, 1.25), nil, SelfCheckOK, "", ""},
		"bias under 0.95 weak":    {fig(12, 12, 0.94), nil, SelfCheckWeak, "", ""},
		"bias at 0.80 weak":       {fig(12, 12, 0.8), nil, SelfCheckWeak, "", ""},
		"bias under 0.80":         {fig(12, 12, 0.79), nil, SelfCheckInconsistent, "ttft-bias", "Do not trust prefill sizing and the TTFT figures from this recording."},
		"low in 15% holds":        {with(fig(20, 20, 1), func(f *SelfCheckFigure) { f.Low = 3 }), nil, SelfCheckOK, "", ""},
		"low in over 15%":         {with(fig(20, 20, 1), func(f *SelfCheckFigure) { f.Low = 4 }), nil, SelfCheckInconsistent, "ttft-low", "Do not trust prefill sizing"},
		"a step at 0.67 holds":    {with(fig(12, 12, 1), func(f *SelfCheckFigure) { f.MinRatio = ptr(0.67) }), nil, SelfCheckOK, "", ""},
		"a step under 0.67":       {with(fig(12, 12, 1), func(f *SelfCheckFigure) { f.MinRatio = ptr(0.66) }), nil, SelfCheckInconsistent, "ttft-floor", "Do not trust prefill sizing"},
		"near the knee at 2x":     {with(fig(12, 12, 1), func(f *SelfCheckFigure) { f.NearKnee = &SelfCheckNearKnee{Steps: 3, Worst: 2} }), nil, SelfCheckOK, "", ""},
		"near the knee past 2x":   {with(fig(12, 12, 1), func(f *SelfCheckFigure) { f.NearKnee = &SelfCheckNearKnee{Steps: 3, Worst: 2.1} }), nil, SelfCheckInconsistent, "ttft-near-knee", "Do not trust prefill sizing"},
		"near the knee, 2 steps":  {with(fig(12, 12, 1), func(f *SelfCheckFigure) { f.NearKnee = &SelfCheckNearKnee{Steps: 2, Worst: 50} }), nil, SelfCheckOK, "", ""},
		"knee low in 1 step":      {with(fig(12, 12, 1), func(f *SelfCheckFigure) { f.Overloaded, f.ServedPastKnee = 2, 1 }), nil, SelfCheckOK, "", ""},
		"knee low in 2 of 10":     {with(fig(12, 12, 1), func(f *SelfCheckFigure) { f.Overloaded, f.ServedPastKnee = 10, 2 }), nil, SelfCheckInconsistent, "ttft-knee", "Do not trust prefill sizing"},
		"knee low in 2 of 11":     {with(fig(12, 12, 1), func(f *SelfCheckFigure) { f.Overloaded, f.ServedPastKnee = 11, 2 }), nil, SelfCheckOK, "", ""},
		"3 levels weak":           {with(fig(12, 12, 1), func(f *SelfCheckFigure) { f.Levels = 3 }), nil, SelfCheckWeak, "", ""},
		"a low bound weak":        {with(fig(20, 14, 1), func(f *SelfCheckFigure) { f.WithinBound = ptr(0.49) }), nil, SelfCheckWeak, "", ""},
		"bias over 1.25":          {fig(12, 12, 1.26), nil, SelfCheckInconsistent, "ttft-bias", "Do not trust prefill sizing"},
		"within 50% holds":        {fig(12, 6, 1), nil, SelfCheckWeak, "", ""},
		"within under 50%":        {fig(12, 5, 1), nil, SelfCheckInconsistent, "ttft-within", "Do not trust prefill sizing"},
		"within under 70% weak":   {fig(20, 13, 1), nil, SelfCheckWeak, "", ""},
		"within at 70% ok":        {fig(20, 14, 1), nil, SelfCheckOK, "", ""},
		"11 steps weak":           {fig(11, 11, 1), nil, SelfCheckWeak, "", ""},
		"6 steps judged":          {fig(6, 6, 1), nil, SelfCheckWeak, "", ""},
		"5 steps not judged":      {fig(5, 5, 1), nil, SelfCheckNotRun, "ttft-scored", ""},
		"overloaded in half":      {over(5, 10), nil, SelfCheckInconsistent, "ttft-overloaded", "Do not trust prefill sizing"},
		"overloaded under half":   {over(4, 10), nil, SelfCheckNotRun, "ttft-scored", ""},
		"tpot only, ttft holds":   {fig(12, 12, 1), fig(12, 2, 2), SelfCheckInconsistent, "tpot-within", "Do not trust decode sizing and the TPOT figures from this recording; prefill sizing and the TTFT figures held."},
		"tpot loose, ttft broken": {fig(12, 0, 3), fig(12, 7, 1), SelfCheckInconsistent, "ttft-bias", "prefill sizing and the TTFT figures from this recording; decode sizing and the TPOT figures (loosely) held."},
	} {
		t.Run(name, func(t *testing.T) {
			sc := &SelfCheck{TTFT: c.ttft, TPOT: c.tpot, Folds: []SelfCheckFold{}, Checks: []SelfCheckRule{}}
			sc.judge(e, time.Hour, SelfCheckInfo{PlannerVersion: "vtest"})
			require.Equal(t, c.status, sc.Status, "%s: %s", sc.Line, sc.Why)
			if c.failed != "" {
				assert.True(t, slices.ContainsFunc(sc.Checks, func(r SelfCheckRule) bool { return r.Name == c.failed && !r.Passed }), "%+v", sc.Checks)
			}
			if c.trust == "" {
				assert.Empty(t, sc.Trust)
			} else {
				assert.Contains(t, sc.Trust, c.trust)
			}
			assert.Equal(t, c.status == SelfCheckInconsistent, sc.IssueURL != "", "an issue only when inconsistent")
			switch c.status {
			case SelfCheckOK:
				assert.Empty(t, sc.Why)
			case SelfCheckWeak:
				assert.Contains(t, sc.Why, "few or loose agreements")
			case SelfCheckNotRun:
				assert.True(t, strings.HasPrefix(sc.Line, "Self-check not run: "), sc.Line)
			}
		})
	}
}

// The issue a self-check offers holds no name from the recording: not the
// model, the namespace, a variant or the file; and no request rate.
func TestSelfCheck_IssueHoldsNoName(t *testing.T) {
	e, folds := scEntry(t, 8, 1)
	secrets := []string{"secret-model-xyz", "secret-ns", "secret-prefill-variant", "secret-decode-variant", "secret-recording.jsonl.gz"}
	e.doc.Model, e.doc.Namespace, e.doc.Source.Name = secrets[0], secrets[1], secrets[4]
	e.doc.Fleet.Prefill.Variant, e.doc.Fleet.Decode.Variant = secrets[2], secrets[3]
	// A release a build named, and a fold that is not backtested, whose
	// error text carries a name: neither reaches the issue.
	e.doc.Fleet.Decode.Engine = &EngineInfo{Engine: "secret-engine", VLLMVersion: "0.34.0+secret-build"}
	folds = append([]FoldFit{{Cycle: 1, At: e.series[len(e.series)-1].At.Add(time.Hour), Pricer: folds[0].Pricer}}, folds...)
	sc := selfCheckEntry(context.Background(), e, folds, SelfCheckInfo{PlannerVersion: "vtest"})
	require.NotEmpty(t, sc.IssueURL)
	u, err := url.Parse(sc.IssueURL)
	require.NoError(t, err)
	body := u.Query().Get("diagnostics")
	require.NotEmpty(t, body)
	assert.Equal(t, selfCheckIssueTemplate, u.Query().Get("template"))
	for _, s := range append(secrets, "secret") {
		assert.NotContains(t, strings.ToLower(sc.IssueURL), s)
		assert.NotContains(t, strings.ToLower(body), s)
	}
	assert.NotContains(t, body, "r/s", "no request rate")
	for _, want := range []string{"planner: vtest", "failed: ttft-bias", "fleet: prefill 1, decode 1", "shape: prompt"} {
		assert.Contains(t, body, want)
	}
}

// The server self-checks a source beside its inputs by default when it was
// started to, and not when the request says selfCheck false.
func TestSelfCheck_OverHTTP(t *testing.T) {
	r := &btResolver{folds: true, n: 480}
	h := New(Config{Resolver: r, WhatifSchema: "schema-v3", SelfCheck: true}).Handler()
	code, body := post(t, h, "/v1/inputs", InputsRequest{Source: &SourceRequest{Kind: SourceRecording, Name: "run.jsonl.gz"}})
	require.Equal(t, http.StatusOK, code, string(body))
	var in InputsResponse
	require.NoError(t, json.Unmarshal(body, &in))
	require.NotNil(t, in.SelfCheck)
	assert.NotEqual(t, SelfCheckNotRun, in.SelfCheck.Status, in.SelfCheck.Why)

	off := false
	code, body = post(t, h, "/v1/inputs", InputsRequest{Source: &SourceRequest{Kind: SourceRecording, Name: "run.jsonl.gz"}, SelfCheck: &off})
	require.Equal(t, http.StatusOK, code, string(body))
	var none InputsResponse
	require.NoError(t, json.Unmarshal(body, &none))
	assert.Nil(t, none.SelfCheck)
	assert.False(t, *r.lastSelfCheck, "the resolver was told not to read folds")
}

// The server's self-check is bounded as a plan is: out of its budget it is
// not run, and says so, and the inputs are still answered.
func TestSelfCheck_OverHTTPIsBounded(t *testing.T) {
	r := &btResolver{folds: true, n: 480}
	h := New(Config{Resolver: r, WhatifSchema: "schema-v3", SelfCheck: true, PlanBudget: time.Nanosecond}).Handler()
	code, body := post(t, h, "/v1/inputs", InputsRequest{Source: &SourceRequest{Kind: SourceRecording, Name: "run.jsonl.gz"}})
	require.Equal(t, http.StatusOK, code, string(body))
	var in InputsResponse
	require.NoError(t, json.Unmarshal(body, &in))
	require.NotNil(t, in.SelfCheck)
	assert.Equal(t, SelfCheckNotRun, in.SelfCheck.Status)
	assert.Contains(t, in.SelfCheck.Why, "time budget")
}
