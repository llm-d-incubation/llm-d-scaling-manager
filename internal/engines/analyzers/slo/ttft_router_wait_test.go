package slo

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The router's flow-control wait (SwitchTTFTRouterWait): off by default at the
// CLI's default, and with it on or off the analyzer's verdict and flags are
// the shared ones (whatif.TTFTAt, whatif.TTFTMeets) on the same fleet, below
// the prefill onset, above it inside and past whatif.MaxRouterWaitKneeShare of
// the knee, and refused past the knee.
func TestTTFT_TheRouterWaitSwitch(t *testing.T) {
	a := newTTFTRig(ttftEstimate(), prefillRows("pre", 4000)).a
	require.Equal(t, whatif.DefaultPriceRouterWait, a.ttftRouterWait, "the analyzer's default is the CLI's")
	require.False(t, a.ttftRouterWait, "experimental: off by default")
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchTTFTRouterWait: true}))
	require.True(t, a.ttftRouterWait)

	in := ttftInputsAt(2, 1.5)
	in.flow = whatif.FlowControl{State: whatif.FlowControlOn, QueueThreshold: 0.2, KVThreshold: 0.8}
	in.decode = testDecode()
	in.processorSharing = true
	in.spread = whatif.PromptSpread{CV: 0.06} // inside MaxPSPromptCV, where the switch takes
	fl, _ := ttftFleet(in, 2)
	on, ok := whatif.FlowControlOnsets(fl).(whatif.Onsets)
	require.True(t, ok)
	knee, ok := whatif.PrefillKnee(fl.Prefill).(whatif.Answer)
	require.True(t, ok)
	within := on.Prefill * 1.01
	require.Less(t, within, whatif.MaxRouterWaitKneeShare*knee.Value, "precondition: a rate above the onset inside the share")
	require.Less(t, within, on.Decode, "precondition: under the decode onset")
	past := (whatif.MaxRouterWaitKneeShare + 1) / 2 * knee.Value
	cases := []struct {
		name string
		rate float64
	}{
		{"below the onset", 0.9 * on.Prefill},
		{"above the onset", within},
		{"past the share", past},
		{"past the knee", knee.Value * 1.01},
	}
	targets := []whatif.TTFTTarget{{Seconds: 100, Percentile: 0.9}, {Seconds: 1e-3, Percentile: 0.9}}
	seen := map[whatif.Flag]bool{}
	for _, routerWait := range []bool{false, true} {
		in.routerWait = routerWait
		fl, _ := ttftFleet(in, 2)
		require.Equal(t, routerWait, fl.PriceRouterWait)
		for _, c := range cases {
			for _, target := range targets {
				p := priceTTFT(in, c.rate, target)
				shared := whatif.TTFTAt(fl, c.rate, target.Percentile)
				met, known := whatif.TTFTMeets(shared, target)
				assert.Equal(t, known && !met, p.unmet, "switch %v, %s, target %g s: verdict", routerWait, c.name, target.Seconds)
				switch v := shared.(type) {
				case whatif.TTFT:
					require.NotNil(t, p.at, "switch %v, %s", routerWait, c.name)
					assert.Equal(t, v.Flags, p.at.Flags, "switch %v, %s: flags", routerWait, c.name)
					assert.Equal(t, v.Seconds, p.at.Seconds)
					for _, f := range v.Flags {
						seen[f] = true
					}
					wantFlag := map[bool]whatif.Flag{false: whatif.AboveFlowControlOnset, true: whatif.FlowControlWaitPriced}[routerWait && c.rate <= whatif.MaxRouterWaitKneeShare*knee.Value]
					above := c.rate >= on.Prefill
					assert.Equal(t, above, v.HasFlag(wantFlag), "switch %v, %s: %v", routerWait, c.name, v.Flags)
					if above && wantFlag == whatif.AboveFlowControlOnset {
						assert.True(t, p.unmet, "switch %v, %s: a miss however low the figure", routerWait, c.name)
					}
				case whatif.Unanswerable:
					require.NotNil(t, p.atRefused, "switch %v, %s", routerWait, c.name)
					assert.Equal(t, v.Reason, p.atRefused.Reason)
					assert.Contains(t, []whatif.Reason{whatif.Saturated, whatif.AboveDecodeOnset}, v.Reason, c.name)
					assert.True(t, p.unmet)
				}
			}
		}
	}
	assert.True(t, seen[whatif.AboveFlowControlOnset] && seen[whatif.FlowControlWaitPriced], "both flags reached: %v", seen)
	// With the switch on, inside the share the figure decides.
	in.routerWait = true
	assert.False(t, priceTTFT(in, within, targets[0]).unmet, "a figure under the target is met")

	// The rate the analyzer publishes meets the analyzer's own verdict at
	// that rate, with the switch off and on: question 1 is capped under the
	// prefill onset, where the verdict turns.
	for _, routerWait := range []bool{false, true} {
		in.routerWait = routerWait
		for _, secs := range []float64{0.5, 2, 100} {
			target := whatif.TTFTTarget{Seconds: secs, Percentile: 0.9}
			p := priceTTFT(in, 1, target)
			if p.rate == nil {
				continue
			}
			at := priceTTFT(in, p.rate.Value, target)
			assert.False(t, at.unmet, "switch %v, target %g s: rate %g r/s (bound %v) is unmet at itself: %v",
				routerWait, secs, p.rate.Value, p.rate.Basis.BoundBy, at.at)
		}
	}
}

// notOnTheTTFTPath are the whatif flags the S1 TTFT series does not publish:
// TPOT and decode-line flags no TTFT answer carries, and the joint model's,
// which SLOTTFTJointFlags lists. Listed when this test was written; a new
// whatif flag must go on constants.SLOTTFTFlags or here, with its reason.
var notOnTheTTFTPath = map[string]string{
	"AssumedShape":        "decode question: the shape assumed",
	"AssumedTPOTRatio":    "TPOT",
	"BorrowedLine":        "decode line",
	"CoarseTPOTBucket":    "TPOT",
	"EngineTPOTHistogram": "TPOT",
	"ExtrapolatedFill":    "decode line",
	"HeldLine":            "decode line",
	"LineNearGate":        "decode line",
	"TierUnstable":        "decode line",
	"TransferHeld":        "joint model",
}

// Every flag the whatif package declares is on the TTFT series' list, the
// joint series' list, or named above as off the path: a new model flag the
// series would drop fails here.
func TestTTFT_EveryModelFlagIsListedOrOffThePath(t *testing.T) {
	const dir = "../../../signals/whatif"
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	fset := token.NewFileSet()
	var declared []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		require.NoError(t, err)
		for _, d := range file.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			// A flag is a constant typed Flag or a Flag("x") conversion; an
			// untyped string beside flags in one group is a flag that lost
			// its type, which no list would catch.
			isFlag := func(vs *ast.ValueSpec) bool {
				id, ok := vs.Type.(*ast.Ident)
				return ok && id.Name == "Flag"
			}
			conv := func(e ast.Expr) (*ast.BasicLit, bool) {
				c, ok := e.(*ast.CallExpr)
				if !ok || len(c.Args) != 1 {
					return nil, false
				}
				if id, ok := c.Fun.(*ast.Ident); !ok || id.Name != "Flag" {
					return nil, false
				}
				lit, ok := c.Args[0].(*ast.BasicLit)
				return lit, ok
			}
			group := false
			for _, s := range g.Specs {
				vs := s.(*ast.ValueSpec)
				if isFlag(vs) {
					group = true
				}
				for _, v := range vs.Values {
					if _, ok := conv(v); ok {
						group = true
					}
				}
			}
			for _, s := range g.Specs {
				vs := s.(*ast.ValueSpec)
				for i, v := range vs.Values {
					lit, ok := conv(v)
					switch {
					case ok:
					case isFlag(vs):
						lit, ok = v.(*ast.BasicLit)
						require.True(t, ok, "flag %s is not a string literal", vs.Names[i].Name)
					default:
						if b, str := v.(*ast.BasicLit); str && b.Kind == token.STRING && vs.Type == nil && group {
							t.Errorf("%s: %s is an untyped string in a group of flags", name, vs.Names[i].Name)
						}
						continue
					}
					str, err := strconv.Unquote(lit.Value)
					require.NoError(t, err)
					declared = append(declared, str)
				}
			}
		}
	}
	require.Greater(t, len(declared), 30, "the parse found the flags")
	for f := range notOnTheTTFTPath {
		assert.Contains(t, declared, f, "notOnTheTTFTPath names %s, which whatif no longer declares", f)
	}
	for _, f := range declared {
		listed := slices.Contains(constants.SLOTTFTFlags, f) || slices.Contains(constants.SLOTTFTJointFlags, f)
		_, off := notOnTheTTFTPath[f]
		assert.True(t, listed || off, "whatif flag %s is on no list: add it to constants.SLOTTFTFlags, or to notOnTheTTFTPath with its reason", f)
		assert.False(t, listed && off, "whatif flag %s is listed and named off the path", f)
	}
}
