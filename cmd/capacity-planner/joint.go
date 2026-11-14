package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// joint-ttft prices the joint prefill and decode TTFT model, J1 of
// docs/proposals/slo-capacity-planner-ttft.md ("Proposed: joint prefill and
// decode TTFT sizing"): the mean TTFT at a fleet of N_p prefill and N_d
// decode pods, from the inputs a recording's rows support at the cycle asked
// for (learnreplay.State.Joint). With no --prefill, --decode or --rate, the
// fleet and the rate that ran. The mean only, and in shadow: nothing sizes
// from it yet.
//
//	capacity-planner joint-ttft --recording FILE [--cycle N|last] [--model M]
//	                  [--prefill N] [--decode N] [--rate R] [--prompt I] [--hop D] [-o json]

// jointFlags is joint-ttft's flags.
type jointFlags struct {
	answer          answerFlags
	prefill, decode int
	rate, prompt    float64
	hop             float64
	output          string
}

// jointSchemaVersion is the version of joint-ttft's JSON.
const jointSchemaVersion = 1

// jointAnswer is joint-ttft's answer, as its JSON holds it.
type jointAnswer struct {
	SchemaVersion int    `json:"schemaVersion"`
	Model         string `json:"model"`
	Cycle         int    `json:"cycle"`
	// PrefillReplicas, DecodeReplicas and Rate are the fleet and load
	// priced; RanPrefill, RanDecode and RanRate those of the cycle.
	PrefillReplicas int     `json:"prefillReplicas"`
	DecodeReplicas  int     `json:"decodeReplicas"`
	Rate            float64 `json:"rate"`
	RanPrefill      int     `json:"ranPrefillReplicas"`
	RanDecode       int     `json:"ranDecodeReplicas"`
	RanRate         float64 `json:"ranRate"`
	// Inputs is the joint inputs, Refused why there is no mean ("" with
	// one), and Mean the mean and its parts, nil when refused. Both carry
	// their own JSON names.
	Inputs  prefill.JointEstimate `json:"inputs"`
	Refused string                `json:"refused,omitempty"`
	Mean    *whatif.JointTTFT     `json:"mean,omitempty"`
	Note    string                `json:"note"`
}

// jointNote is printed on every answer.
const jointNote = "UNVALIDATED beyond the staircase's 2+2 phases; the mean only, arrivals split evenly over each " +
	"role's pods; R is assumed unless --hop states it; shadow: nothing sizes from it"

func runJointTTFT(ctx context.Context, args []string, out, _ io.Writer) error {
	var f jointFlags
	fs := newFlags("joint-ttft", out)
	fs.StringVar(&f.answer.recording, "recording", "", "a learner recording (.jsonl or .jsonl.gz) to answer from (required)")
	fs.StringVar(&f.answer.cycle, "cycle", "last", "the model's cycle to answer at: a number from 1, or last")
	fs.StringVar(&f.answer.model, "model", "", "the model, when the recording holds several")
	fs.IntVar(&f.prefill, "prefill", 0, "prefill pods to price at; 0 is the cycle's")
	fs.IntVar(&f.decode, "decode", 0, "decode pods to price at; 0 is the cycle's")
	fs.Float64Var(&f.rate, "rate", 0, "the model's arrival rate to price at, requests/s; 0 is the cycle's")
	fs.Float64Var(&f.prompt, "prompt", 0, "the prompt to price at, tokens; 0 is the one the work was read at")
	fs.Float64Var(&f.hop, "hop", 0, "the router hop R, seconds; 0 assumes whatif.AssumedRouterHopSeconds")
	fs.StringVar(&f.output, "o", "text", "text or json")
	if ok, err := parse(fs, args); !ok {
		return err
	}
	switch {
	case f.answer.recording == "":
		return usagef("--recording is required")
	case f.prefill < 0 || f.decode < 0 || !finite(f.rate) || f.rate < 0 || !finite(f.prompt) || f.prompt < 0 ||
		!finite(f.hop) || f.hop < 0:
		return usagef("--prefill, --decode, --rate, --prompt and --hop take a count or a figure at or above 0")
	case f.output != "text" && f.output != "json":
		return usagef("-o %q, want text or json", f.output)
	}
	if _, err := f.answer.cycleNumber(); err != nil {
		return err
	}
	st, err := f.answer.readState(ctx)
	if err != nil {
		return err
	}
	a := priceJoint(st, f)
	if f.output == "json" {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		err = enc.Encode(a)
	} else {
		_, err = io.WriteString(out, a.text())
	}
	switch {
	case err != nil:
		return err
	case a.Refused != "":
		// A refusal is printed, and exits as every refused answer does.
		return exitStatus(3)
	}
	return nil
}

// priceJoint is the answer at the state's cycle for f.
func priceJoint(st *learnreplay.State, f jointFlags) jointAnswer {
	a := jointAnswer{SchemaVersion: jointSchemaVersion, Model: st.Namespace + "/" + st.ModelID, Cycle: st.Cycle,
		PrefillReplicas: st.JointPrefillPods, DecodeReplicas: st.JointDecodePods, Rate: st.JointRate,
		RanPrefill: st.JointPrefillPods, RanDecode: st.JointDecodePods, RanRate: st.JointRate,
		Inputs: st.Joint, Note: jointNote}
	if f.prefill > 0 {
		a.PrefillReplicas = f.prefill
	}
	if f.decode > 0 {
		a.DecodeReplicas = f.decode
	}
	if f.rate > 0 {
		a.Rate = f.rate
	}
	q := whatif.JointQuestion{PrefillReplicas: a.PrefillReplicas, DecodeReplicas: a.DecodeReplicas, Rate: a.Rate,
		PromptTokens: f.prompt, HopSeconds: f.hop}
	if !(f.hop > 0) {
		q.HopSeconds, q.HopAssumed = whatif.AssumedRouterHopSeconds, true
	}
	m, refused := whatif.JointTTFTMean(st.Joint, q)
	if refused != "" {
		a.Refused = string(refused)
		return a
	}
	if q.HopAssumed {
		m.Flags = append(m.Flags, whatif.RouterHopAssumed)
	}
	a.Mean = &m
	return a
}

// inputRefusalText is an input's refusal as the text prints it: nothing with none.
func inputRefusalText(r prefill.Refusal) string {
	if r == "" {
		return ""
	}
	return ", refused: " + string(r)
}

// text is the answer for a reader.
func (a jointAnswer) text() string {
	var b strings.Builder
	e := a.Inputs
	fmt.Fprintf(&b, "joint TTFT, %s at cycle %d: %d prefill + %d decode at %.2f req/s (ran %d + %d at %.2f)\n",
		a.Model, a.Cycle, a.PrefillReplicas, a.DecodeReplicas, a.Rate, a.RanPrefill, a.RanDecode, a.RanRate)
	if a.Refused != "" {
		fmt.Fprintf(&b, "  refused: %s\n", a.Refused)
	} else {
		m := a.Mean
		fmt.Fprintf(&b, "  mean TTFT %s (engines %s, hop %s)\n", msText(m.Seconds), msText(m.Engine), msText(m.Hop))
		fmt.Fprintf(&b, "    overhead a(rho_p) %s, prefill T_P %s at rho_p %.3f\n", msText(m.Overhead), msText(m.Prefill),
			m.PrefillUtilization)
		fmt.Fprintf(&b, "    KV pull T_X %s, unshared %s at %.3f of the decode pod\n", msText(m.Pull), msText(m.PullWork),
			m.PullUtilization)
		fmt.Fprintf(&b, "    decode m_d*ITL(k) + d %s, batch k %.1f, ITL %s\n", msText(m.Decode), m.Batch, msText(m.ITL))
		fmt.Fprintf(&b, "    flags: %v\n", m.Flags)
	}
	fmt.Fprintf(&b, "  inputs: S %s at %.0f tokens (%d readings%s); a0 %s (%d%s); pull table %d buckets%s\n",
		msText(e.PrefillWork), e.PrefillWorkPrompt, e.PrefillWorkSamples, inputRefusalText(e.PrefillWorkRefused),
		msText(e.Overhead), e.OverheadSamples, inputRefusalText(e.OverheadRefused), len(e.Pull), inputRefusalText(e.PullRefused))
	fmt.Fprintf(&b, "          ITL(k) %.3f ms + %.4f ms*k (%d%s); decode own %.2f*ITL + %s (%d%s); n_out %.0f\n",
		1000*e.ITLAlpha, 1000*e.ITLBeta, e.ITLSamples, inputRefusalText(e.ITLRefused), e.DecodeSteps,
		msText(e.DecodeFixed), e.DecodeSamples, inputRefusalText(e.DecodeRefused), e.OutputTokens)
	fmt.Fprintf(&b, "  note: %s\n", a.Note)
	return b.String()
}
