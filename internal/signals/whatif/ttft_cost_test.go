package whatif

import (
	"fmt"
	"testing"
)

// noTermCase is a fleet priced with no decode term (pl.term nil): no decode
// line, or the legacy sum. Those are the prices whose old-sum floor is never
// read.
type noTermCase struct {
	name   string
	fleet  func() TTFTFleet
	target TTFTTarget
	rate   float64
}

func noTermCases() []noTermCase {
	measured := func() TTFTFleet {
		fl := prefillOnly(2)
		fl.Prefill.Replicas, fl.Prefill.Imbalance = 3, 1.2
		fl.DecodeTTFT = DecodeTTFT{Seconds: 0.03, PromptTokens: 1000}
		return fl
	}
	legacy := func() TTFTFleet {
		fl := workedFleet()
		fl.LegacyDecodeSum = true
		return fl
	}
	return []noTermCase{
		{"prefill only, cv 1, p90", func() TTFTFleet { return prefillOnly(1) }, TTFTTarget{Seconds: 0.1, Percentile: 0.9}, 12},
		{"prefill only, cv 0.5, p99", func() TTFTFleet { return prefillOnly(0.5) }, TTFTTarget{Seconds: 0.2, Percentile: 0.99}, 20},
		{"prefill only, cv 1, mean", func() TTFTFleet { return prefillOnly(1) }, TTFTTarget{Seconds: 0.08}, 12},
		{"decode TTFT measured, cv 2, p95", measured, TTFTTarget{Seconds: 0.3, Percentile: 0.95}, 30},
		{"legacy decode sum, p90", legacy, TTFTTarget{Seconds: 0.2, Percentile: 0.9}, 8},
	}
}

// describe is a result in full, every float at the shortest form that reads
// back to the same bits, pointers followed.
func describe(r Result) string {
	switch v := r.(type) {
	case Answer:
		basis := v.Basis
		basis.TTFT = nil
		return fmt.Sprintf("answer %v flags %v band %s basis %+v ttft %s sensitivity %s",
			v.Value, v.Flags, followed(v.Band), basis, followed(v.Basis.TTFT), followed(v.Sensitivity))
	case TTFT:
		s := v.Sensitivity
		v.Sensitivity = nil
		return fmt.Sprintf("ttft %+v sensitivity %s", v, followed(s))
	default:
		return fmt.Sprintf("%T %+v", r, r)
	}
}

// followed is *p, or nil.
func followed[T any](p *T) string {
	if p == nil {
		return "nil"
	}
	return fmt.Sprintf("%+v", *p)
}

// noTermFigures pins every no-term figure as the price gave it while it still
// worked out the old-sum floor it then threw away (llm-scaler/main e4d247645):
// skipping that floor must move no figure, at any bit.
var noTermFigures = map[string][2]string{
	"prefill only, cv 1, p90": {
		`answer 3.654632803270488 flags [UnvalidatedTTFT AssumedVP AssumedPromptSpread TransferOmitted DecodeTTFTUnmeasured DecodeSideOmitted] band nil basis {Fill:0 KneeRate:0 Floor:0 Uncapped:0 BoundBy:[TTFT] Tier: TTFT:<nil>} ttft {Floor:0.0893528364331349 KneeRate:30 PrefillOnlyRate:3.654632803270488} sensitivity nil`,
		`ttft {Seconds:0.1405213940552308 Independent:0.1405213940552308 Pooled:0.1405213940552306 Floor:0.0893528364331349 DecodeShare:0 Parts:{EngineWait:0.022222222222222227 OwnPrefill:0.04593333333333333 DecodeWait:0 FirstStep:0 DecodeEngine:0 RouterHop:0} PrefillUtilization:0.4 VPMarginApplied:0 WaitScale:0.055555555555555754 WaitOffset:0 DecodeWaitProbability:0 PrefillKnee:30 DecodeKnee:0 Onsets:{Prefill:0 Decode:0} Tier: Flags:[UnvalidatedTTFT AssumedVP AssumedPromptSpread TransferOmitted DecodeTTFTUnmeasured DecodeSideOmitted] Sensitivity:<nil>} sensitivity nil`,
	},
	"prefill only, cv 0.5, p99": {
		`answer 14.327862394038675 flags [UnvalidatedTTFT AssumedVP AssumedPromptSpread TransferOmitted DecodeTTFTUnmeasured DecodeSideOmitted] band nil basis {Fill:0 KneeRate:0 Floor:0 Uncapped:0 BoundBy:[TTFT] Tier: TTFT:<nil>} ttft {Floor:0.09630931262359675 KneeRate:30 PrefillOnlyRate:14.327862394038675} sensitivity nil`,
		`ttft {Seconds:0.30511240461331385 Independent:0.30511240461331385 Pooled:0.30511240461331385 Floor:0.09630931262359675 DecodeShare:0 Parts:{EngineWait:0.04166666666666666 OwnPrefill:0.04593333333333333 DecodeWait:0 FirstStep:0 DecodeEngine:0 RouterHop:0} PrefillUtilization:0.6666666666666666 VPMarginApplied:0 WaitScale:0.057654812897917726 WaitOffset:0.014376288549903788 DecodeWaitProbability:0 PrefillKnee:30 DecodeKnee:0 Onsets:{Prefill:0 Decode:0} Tier: Flags:[UnvalidatedTTFT AssumedVP AssumedPromptSpread TransferOmitted DecodeTTFTUnmeasured DecodeSideOmitted] Sensitivity:<nil>} sensitivity nil`,
	},
	"prefill only, cv 1, mean": {
		`answer 15.163204747764212 flags [UnvalidatedTTFT AssumedVP AssumedPromptSpread TransferOmitted DecodeTTFTUnmeasured DecodeSideOmitted] band nil basis {Fill:0 KneeRate:0 Floor:0 Uncapped:0 BoundBy:[TTFT] Tier: TTFT:<nil>} ttft {Floor:0.04593333333333333 KneeRate:30 PrefillOnlyRate:15.163204747764212} sensitivity nil`,
		`ttft {Seconds:0.06815555555555555 Independent:0.06815555555555555 Pooled:0.06815555555555555 Floor:0.04593333333333333 DecodeShare:0 Parts:{EngineWait:0.022222222222222227 OwnPrefill:0.04593333333333333 DecodeWait:0 FirstStep:0 DecodeEngine:0 RouterHop:0} PrefillUtilization:0.4 VPMarginApplied:0 WaitScale:0.055555555555555754 WaitOffset:0 DecodeWaitProbability:0 PrefillKnee:30 DecodeKnee:0 Onsets:{Prefill:0 Decode:0} Tier: Flags:[UnvalidatedTTFT AssumedVP AssumedPromptSpread TransferOmitted DecodeTTFTUnmeasured DecodeSideOmitted] Sensitivity:<nil>} sensitivity nil`,
	},
	"decode TTFT measured, cv 2, p95": {
		`answer 11.90767915572053 flags [UnvalidatedTTFT AssumedVP AssumedPromptSpread DecodeEngineMeasured] band nil basis {Fill:0 KneeRate:0 Floor:0 Uncapped:0 BoundBy:[TTFT] Tier: TTFT:<nil>} ttft {Floor:0.20394881832596726 KneeRate:75 PrefillOnlyRate:11.90767915572053} sensitivity nil`,
		`ttft {Seconds:0.448735842528878 Independent:0.448735842528878 Pooled:0.21294591189852957 Floor:0.20394881832596726 DecodeShare:0 Parts:{EngineWait:0.055555555555555546 OwnPrefill:0.04593333333333333 DecodeWait:0 FirstStep:0 DecodeEngine:0.03 RouterHop:0} PrefillUtilization:0.39999999999999997 VPMarginApplied:0 WaitScale:0.16590472818875196 WaitOffset:0 DecodeWaitProbability:0 PrefillKnee:75 DecodeKnee:0 Onsets:{Prefill:0 Decode:0} Tier: Flags:[UnvalidatedTTFT AssumedVP AssumedPromptSpread DecodeEngineMeasured] Sensitivity:<nil>} sensitivity nil`,
	},
	"legacy decode sum, p90": {
		`answer 11.638534256572129 flags [UnvalidatedTTFT AssumedVP AssumedPromptSpread TransferOmitted FlowControlInferredOff HeldByDecodeAdmission] band nil basis {Fill:0.5071772220374592 KneeRate:0 Floor:0 Uncapped:0 BoundBy:[TTFT decode admission] Tier: TTFT:<nil>} ttft {Floor:0.09157553643313489 KneeRate:13.312564951975148 PrefillOnlyRate:13.312564951975148} sensitivity nil`,
		`ttft {Seconds:0.11988559270651773 Independent:0.11988557446112186 Pooled:0.11988559270651751 Floor:0.09157553643313489 DecodeShare:1.0681688774185583e-07 Parts:{EngineWait:0.01212121212121212 OwnPrefill:0.04593333333333333 DecodeWait:2.010646245409999e-08 FirstStep:0.00262258538089853 DecodeEngine:0 RouterHop:0} PrefillUtilization:0.26666666666666666 VPMarginApplied:0 WaitScale:0.04545454545454584 WaitOffset:0 DecodeWaitProbability:1.0681688774185583e-07 PrefillKnee:30 DecodeKnee:13.312564951975148 Onsets:{Prefill:0 Decode:0} Tier: Flags:[UnvalidatedTTFT AssumedVP AssumedPromptSpread TransferOmitted FlowControlInferredOff] Sensitivity:<nil>} sensitivity nil`,
	},
}

// With no decode term, the price reads no old-sum floor, so skipping it moves
// nothing: RateForTTFT and TTFTAt answer, to the bit, what they answered while
// the floor was still worked out and thrown away.
func TestTTFT_NoTermFiguresUnmovedBySkippedFloor(t *testing.T) {
	for _, c := range noTermCases() {
		got := [2]string{describe(rateForTTFT(t, c.fleet(), c.target)), describe(ttftAt(t, c.fleet(), c.rate, c.target.Percentile))}
		want, ok := noTermFigures[c.name]
		if !ok {
			t.Errorf("%s: no pinned figures; got\n%q", c.name, got)
			continue
		}
		for i, what := range []string{"RateForTTFT", "TTFTAt"} {
			if got[i] != want[i] {
				t.Errorf("%s, %s:\n got %s\nwant %s", c.name, what, got[i], want[i])
			}
		}
	}
}

// oldFloorReads counts the old-sum floors f works out, through onOldFloor.
func oldFloorReads(t *testing.T, f func()) int {
	t.Helper()
	n := 0
	onOldFloor = func() { n++ }
	defer func() { onOldFloor = nil }()
	f()
	return n
}

// A price with no decode term works out no old-sum floor, eagerly or
// otherwise: with none its own time is a zero ownTime, whose quantile costs
// about 100 ms a RateForTTFT for a NaN nothing reads. With a term the floor is
// worked out, on the mean and the percentile branch both.
func TestTTFT_OldFloorWorkedOutOnlyWithATerm(t *testing.T) {
	for _, c := range noTermCases() {
		fl := c.fleet()
		if n := oldFloorReads(t, func() { RateForTTFT(fl, c.target) }); n != 0 {
			t.Errorf("%s: RateForTTFT worked out the old floor %d times, want 0", c.name, n)
		}
		if n := oldFloorReads(t, func() { TTFTAt(fl, c.rate, c.target.Percentile) }); n != 0 {
			t.Errorf("%s: TTFTAt worked out the old floor %d times, want 0", c.name, n)
		}
	}
	for name, fl := range map[string]TTFTFleet{"default term": workedFleet(), "stated term": termFleet(stateTerm())} {
		for _, q := range []float64{0, 0.9} {
			if n := oldFloorReads(t, func() { RateForTTFT(fl, TTFTTarget{Seconds: 0.2, Percentile: q}) }); n == 0 {
				t.Errorf("%s q %g: RateForTTFT worked out no old floor", name, q)
			}
			if n := oldFloorReads(t, func() { TTFTAt(fl, 8, q) }); n != 1 {
				t.Errorf("%s q %g: TTFTAt worked out the old floor %d times, want 1", name, q, n)
			}
		}
	}
}

// termFloorFleet is the worked fleet with a decode term (nil is the default,
// engine omitted), four decode replicas so prefill's knee (30 r/s) is the
// lower, a 5% V_P margin, and a KV transfer of xfer seconds per token.
func termFloorFleet(term *DecodeTerm, xfer float64) TTFTFleet {
	fl := termFleet(term)
	d := *fl.Decode
	d.Replicas = 4
	fl.Decode = &d
	fl.Prefill.VPMargin = 0.05
	fl.Prefill.TransferSecondsPerToken = xfer
	return fl
}

// termFloorFigures pins TTFTAt's floor, figure and DecodeTermFloored with a
// decode term, at the mean and at p90, below the margin (6 r/s, rho 0.2) and
// under all of it (24 r/s, rho 0.8, where the price's own time is the
// margined one and the floor's must stay the zero-rate one).
var termFloorFigures = map[string]string{
	"default term at 6 r/s, q 0":                "floor 0.048156033333333334 seconds 0.056489366666666666 floored false margin 0",
	"default term at 6 r/s, q 0.9":              "floor 0.09157553643313489 seconds 0.11076374554142299 floored false margin 0",
	"default term at 24 r/s, q 0":               "floor 0.048156033333333334 seconds 0.23706165055538927 floored false margin 0.05",
	"default term at 24 r/s, q 0.9":             "floor 0.09157553643313489 seconds 0.5265250045543252 floored false margin 0.05",
	"default term, 2e-5 s/tok at 6 r/s, q 0":    "floor 0.06815603333333332 seconds 0.07648936666666666 floored false margin 0",
	"default term, 2e-5 s/tok at 6 r/s, q 0.9":  "floor 0.13762723829301582 seconds 0.15446115912937 floored false margin 0",
	"default term, 2e-5 s/tok at 24 r/s, q 0":   "floor 0.06815603333333332 seconds 0.25706165055538927 floored false margin 0.05",
	"default term, 2e-5 s/tok at 24 r/s, q 0.9": "floor 0.13762723829301582 seconds 0.5516269093104237 floored false margin 0.05",
	"stated term at 6 r/s, q 0":                 "floor 0.06371833333333334 seconds 0.07269366323523542 floored false margin 0",
	"stated term at 6 r/s, q 0.9":               "floor 0.11234817680511108 seconds 0.1317117356315117 floored false margin 0",
	"stated term at 24 r/s, q 0":                "floor 0.06371833333333334 seconds 0.25601936388799107 floored false margin 0.05",
	"stated term at 24 r/s, q 0.9":              "floor 0.11234817680511108 seconds 0.5462841301053257 floored false margin 0.05",
	"stated term, 2e-5 s/tok at 6 r/s, q 0":     "floor 0.06815603333333334 seconds 0.07648936666666667 floored true margin 0",
	"stated term, 2e-5 s/tok at 6 r/s, q 0.9":   "floor 0.13762723829301582 seconds 0.15446115912937006 floored true margin 0",
	"stated term, 2e-5 s/tok at 24 r/s, q 0":    "floor 0.06815603333333334 seconds 0.25706165055538927 floored true margin 0.05",
	"stated term, 2e-5 s/tok at 24 r/s, q 0.9":  "floor 0.13762723829301582 seconds 0.5516269093104237 floored true margin 0.05",
}

// With a term the old sum's floor is the zero-rate own time's, at the asked
// percentile, plus the old step: TTFTAt's floor and figure, and whether it is
// flagged DecodeTermFloored, are pinned on floored and unfloored fleets, at
// the mean and at p90, with and without the V_P margin.
func TestTTFT_TermOldFloorPinned(t *testing.T) {
	fleets := map[string]TTFTFleet{
		"default term":             termFloorFleet(nil, 0),
		"default term, 2e-5 s/tok": termFloorFleet(nil, 2e-5),
		"stated term":              termFloorFleet(stateTerm(), 0),
		"stated term, 2e-5 s/tok":  termFloorFleet(stateTerm(), 2e-5),
	}
	floored, unfloored := 0, 0
	for name, fl := range fleets {
		for _, rate := range []float64{6, 24} {
			for _, q := range []float64{0, 0.9} {
				key := fmt.Sprintf("%s at %g r/s, q %g", name, rate, q)
				v := mustTTFT(t, ttftAt(t, fl, rate, q))
				got := fmt.Sprintf("floor %v seconds %v floored %v margin %v", v.Floor, v.Seconds, v.HasFlag(DecodeTermFloored), v.VPMarginApplied)
				if v.HasFlag(DecodeTermFloored) {
					floored++
				} else {
					unfloored++
				}
				if want, ok := termFloorFigures[key]; !ok {
					t.Errorf("no pin: %q: %q,", key, got)
				} else if got != want {
					t.Errorf("%s:\n got %s\nwant %s", key, got, want)
				}
			}
		}
	}
	if floored == 0 || unfloored == 0 {
		t.Errorf("floored %d, unfloored %d: want both", floored, unfloored)
	}
}

// BenchmarkRateForTTFTNoTerm is a RateForTTFT with no decode term, the SLO
// analyzer's prefill-only (S1) shape.
func BenchmarkRateForTTFTNoTerm(b *testing.B) {
	fl := prefillOnly(1)
	tg := TTFTTarget{Seconds: 0.1, Percentile: 0.9}
	for b.Loop() {
		RateForTTFT(fl, tg)
	}
}
