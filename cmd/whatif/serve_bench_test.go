package main

import (
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// The plan's latency (docs/proposals/capacity-planner.md, "Caching and
// cost": a slider move answered in under 200 ms on the server), on the
// processor-sharing fleets of curveFleets and the goldens' parity cases:
//
//	go test ./cmd/whatif -run '^$' -bench Plan -benchtime 5x
//
// Cold is a plan whose curves are priced (a new fleet: a replica count, a
// statistic, an assumption); slider is a rate move, whose curves are cached.

// benchCase is one fleet's plan.
type benchCase struct {
	name, recording, model string
	req                    whatifapi.PlanRequest
}

// benchCases are the curve fleets, with and without the per-bucket view, and
// every golden's question.
func benchCases() []benchCase {
	var out []benchCase
	for name, c := range curveFleets() {
		out = append(out, benchCase{name, c.recording, c.model, c.req})
		if c.req.Views != nil {
			mix := c.req
			mix.Views = nil
			out = append(out, benchCase{name + "-mix-only", c.recording, c.model, mix})
		}
	}
	for _, c := range parityCases() {
		out = append(out, benchCase{c.golden, c.recording, c.model, c.plan(1)})
	}
	return out
}

func BenchmarkPlanCold(b *testing.B) {
	for _, c := range benchCases() {
		b.Run(c.name, func(b *testing.B) {
			for b.Loop() {
				b.StopTimer()
				h, _ := servedAPI(b)
				in, _ := apiInputs(b, h, c.recording, c.model)
				req := c.req
				req.InputsID = in.InputsID
				b.StartTimer()
				apiPlan(b, h, req)
			}
		})
	}
}

func BenchmarkPlanSliderMove(b *testing.B) {
	for _, c := range benchCases() {
		b.Run(c.name, func(b *testing.B) {
			h, _ := servedAPI(b)
			in, _ := apiInputs(b, h, c.recording, c.model)
			req := c.req
			req.InputsID = in.InputsID
			first, _ := apiPlan(b, h, req)
			base := first.Limits.Chosen.Rate
			i := 0
			for b.Loop() {
				i++
				moved := req
				moved.Traffic = &whatifapi.Traffic{Classes: &whatifapi.Classes{Critical: &whatifapi.ClassRate{Rate: fp(base * (0.5 + float64(i%97)/200))}}}
				if req.Traffic != nil {
					moved.Traffic.Mix, moved.Traffic.ArrivalCV2 = req.Traffic.Mix, req.Traffic.ArrivalCV2
				}
				apiPlan(b, h, moved)
			}
			b.ReportMetric(base, "rate")
		})
	}
}

// A TTFT target move: the curves are cached, the limits and the marginal
// steps priced again.
func BenchmarkPlanTargetMove(b *testing.B) {
	for _, c := range benchCases() {
		b.Run(c.name, func(b *testing.B) {
			h, _ := servedAPI(b)
			in, _ := apiInputs(b, h, c.recording, c.model)
			req := c.req
			req.InputsID = in.InputsID
			first, _ := apiPlan(b, h, req)
			i := 0
			for b.Loop() {
				i++
				moved := req
				slo := whatifapi.SLORequest{TTFT: &whatifapi.Objective{Statistic: first.Scenario.TTFT.Statistic,
					Target: fp(first.Scenario.TTFT.Target * (1 + float64(i%97)/100))}}
				if req.SLO != nil {
					slo.TPOT = req.SLO.TPOT
				}
				moved.SLO = &slo
				apiPlan(b, h, moved)
			}
		})
	}
}
