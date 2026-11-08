package saturation

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// Speculative decoding (#274): vLLM reads its ITL once per engine step, and a
// step emits 1 + the accepted draft tokens. The ITL line is the time per
// token, so a fleet that speculates is priced from its tokens, not its steps.

func specCases() []propCase {
	return []propCase{
		{class: classKnownTruth, learner: "itl", name: "speculative-accepted-tokens", family: familyDirect, tuning: tunedDefault,
			check: checkITLSpeculativeOn(nil)},
	}
}

// specParams is a vLLM engine with MTP on: three draft tokens a step.
func specParams() *capacity.EngineParams {
	p := keyParams(256)
	p.SpeculativeConfig = `{"method":"mtp","num_speculative_tokens":3}`
	return p
}

// checkITLSpeculativeOn: ten replicas of a vLLM variant that speculates, each
// accepting its own 0.5-2 draft tokens a step and reading ITL per step, as
// vLLM does, over k in [0.20, 0.65]. After 3 min the variant's own line,
// priced at propKPrice, is the time per token within propTruthTol; the step
// time over-states it by the tokens a step emits, which prices the replica's
// token rate that much low. edit puts a defect back.
func checkITLSpeculativeOn(edit func(*SaturationAnalyzer)) func(uint64) error {
	return func(seed uint64) error {
		line, found, err := specFleetLine(seed, edit)
		if err != nil {
			return err
		}
		if !found {
			return violation(kindNotLearned, "no line of its own after 3 min of ten replicas")
		}
		got, want := line.model.ITLAt(propKPrice), propITL(propKPrice)
		return holds(relErr(got, want) <= propTruthTol, kindBiased,
			"ITL(%.2f) %.5f per token against %.5f: the step time prices %.2fx the replica's token rate",
			propKPrice, got, want, want/got)
	}
}

// specFleetLine runs checkITLSpeculativeOn's fleet through Analyze for 3 min
// and returns the line the variant published, and whether it published one.
func specFleetLine(seed uint64, edit func(*SaturationAnalyzer)) (learnedLine, bool, error) {
	r := propRand(seed)
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "spec", capacity.Record{AcceleratorName: "H100", GpuCount: 1,
		EngineParams: specParams()}, propStart)
	a := NewSaturationAnalyzer(store)
	clock := propStart
	a.now = func() time.Time { return clock }
	if edit != nil {
		edit(a)
	}
	const n = 10
	accepted := make([]float64, n)
	for i := range accepted {
		accepted[i] = 0.5 + 1.5*r.Float64()
	}
	states := []domain.VariantReplicaState{
		{VariantName: "spec", Role: domain.RoleDecode, AcceleratorName: "H100", CurrentReplicas: n, GPUsPerReplica: 1},
	}
	for range 13 {
		rows := make([]domain.ReplicaMetrics, 0, n)
		for i := range n {
			k := 0.20 + 0.05*float64(i)
			rm := makeReplicaMetrics(fmt.Sprintf("spec-%d", i), "spec", 5000, 16000, 0, 6000, 1000)
			rm.KvUsageInstant, rm.KvUsageInstantKnown = k, true
			rm.AvgITL = propITL(k) * (1 + 0.005*r.NormFloat64()) * (1 + accepted[i])
			rm.AvgITLKnown = true
			rm.SpecAcceptedPerStep, rm.SpecAcceptedPerStepKnown = accepted[i], true
			rows = append(rows, rm)
		}
		if _, err := a.Analyze(context.Background(), makeAnalyzerInput(rows, states)); err != nil {
			return learnedLine{}, false, err
		}
		clock = clock.Add(propCycle)
	}
	var line learnedLine
	found := false
	a.mu.Lock()
	for _, l := range a.itlLines {
		if strings.HasPrefix(l.fromVariant, "test-ns|test-model|spec|") {
			line, found = l, true
		}
	}
	a.mu.Unlock()
	return line, found, nil
}

// specParamsSGLang is an SGLang engine with speculative flags: its ITL is per
// token already.
func specParamsSGLang() *capacity.EngineParams {
	p := specParams()
	p.Engine = inferenceengine.EngineSGLang
	p.SpeculativeConfig = "algorithm=EAGLE;num_draft_tokens=4"
	return p
}
