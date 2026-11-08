package whatif

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// isInvalid reports whether r is an InvalidInput refusal.
func isInvalid(r Result) bool {
	u, ok := r.(Unanswerable)
	return ok && u.Reason == InvalidInput
}

// Question 1's headroom is valid in (0, 1]: 0, a negative one and one above 1
// are usage errors, and 1 (no cap) is not.
func TestQuestion1Headroom(t *testing.T) {
	d := olsSweep()
	target := TPOTTarget{Seconds: 0.004}
	for _, h := range []float64{0, -0.1, 1.0000001, 1.5} {
		if r := rateFor(d, target, h); !isInvalid(r) {
			t.Errorf("headroom %g: %#v, want InvalidInput", h, r)
		}
	}
	for _, h := range []float64{1e-9, 0.85, 1} {
		if _, ok := rateFor(d, target, h).(Answer); !ok {
			t.Errorf("headroom %g refused: %#v", h, rateFor(d, target, h))
		}
	}
}

// A negative rate is a usage error wherever a rate is taken.
func TestNegativeRate(t *testing.T) {
	d := olsSweep()
	for name, r := range map[string]Result{
		"FillAtRate":      FillAtRate(d, -1),
		"DecodeAdmission": DecodeAdmission(d, -1),
		"question 2":      SLOAtReplicas(d, ReplicasQuery{Rate: -1}).Rate,
	} {
		if !isInvalid(r) {
			t.Errorf("%s at -1 r/s: %#v, want InvalidInput", name, r)
		}
	}
}

// Question 2 takes exactly one of Headroom (in (0, 1)) and Rate; a percentile
// target with no percentile is a usage error, and a nil one asks for none.
func TestQuestion2Query(t *testing.T) {
	d := olsSweep()
	invalid := map[string]ReplicasQuery{
		"neither":                 {},
		"headroom 0":              {Headroom: 0},
		"negative headroom":       {Headroom: -0.1},
		"headroom 1":              {Headroom: 1},
		"headroom above 1":        {Headroom: 1.2},
		"rate and headroom":       {Rate: 5, Headroom: 0.85},
		"rate and headroom 1":     {Rate: 5, Headroom: 1},
		"rate and headroom above": {Rate: 5, Headroom: 1.2},
		"negative rate":           {Rate: -5},
	}
	for name, q := range invalid {
		slo := SLOAtReplicas(d, q)
		if !isInvalid(slo.Rate) || !isInvalid(slo.MeanTPOT) {
			t.Errorf("%s: rate %#v, mean %#v, want InvalidInput", name, slo.Rate, slo.MeanTPOT)
		}
		if _, ok := slo.KneeRate.(Answer); !ok {
			t.Errorf("%s: the knee is lost: %#v", name, slo.KneeRate)
		}
		if slo.PercentileTPOT != nil {
			t.Errorf("%s: a percentile nobody asked for: %#v", name, slo.PercentileTPOT)
		}
	}
	for name, q := range map[string]ReplicasQuery{"rate": {Rate: 5}, "headroom": {Headroom: 0.85}} {
		slo := SLOAtReplicas(d, q)
		if _, ok := slo.MeanTPOT.(Answer); !ok {
			t.Errorf("%s: %#v", name, slo.MeanTPOT)
		}
		if slo.PercentileTPOT != nil {
			t.Errorf("%s: PercentileTPOT %#v with no percentile asked, want nil", name, slo.PercentileTPOT)
		}
	}
	zero := SLOAtReplicas(d, ReplicasQuery{Rate: 5, PercentileTarget: &TPOTTarget{Ratio: TPOTRatio{Value: 1.3}}})
	if !isInvalid(zero.PercentileTPOT) {
		t.Errorf("percentile 0 asked for: %#v, want InvalidInput", zero.PercentileTPOT)
	}
	if _, ok := zero.MeanTPOT.(Answer); !ok {
		t.Errorf("percentile 0 asked for lost the mean: %#v", zero.MeanTPOT)
	}
	gaps := SLOAtReplicas(d, ReplicasQuery{Rate: 5, PercentileTarget: &TPOTTarget{OverTokenGaps: true}})
	if u, ok := gaps.PercentileTPOT.(Unanswerable); !ok || u.Reason != TokenGapPercentile {
		t.Errorf("token gaps: %#v, want TokenGapPercentile", gaps.PercentileTPOT)
	}
	// A refused fleet refuses the percentile only when it was asked.
	none := olsSweep()
	none.Replicas = 0
	if slo := SLOAtReplicas(none, ReplicasQuery{Rate: 5}); slo.PercentileTPOT != nil {
		t.Errorf("a refused fleet filled a percentile nobody asked for: %#v", slo.PercentileTPOT)
	}
	if slo := SLOAtReplicas(none, ReplicasQuery{Rate: 5, PercentileTarget: &TPOTTarget{Percentile: 0.9}}); slo.PercentileTPOT == nil {
		t.Error("a refused fleet left an asked-for percentile nil")
	}
}

func TestExitCode(t *testing.T) {
	answered := Answer{Value: 1}
	refused := Unanswerable{Reason: NoLine}
	usage := Unanswerable{Reason: InvalidInput}
	admitted := Admission{}
	for _, c := range []struct {
		name    string
		results []Result
		want    int
	}{
		{"nothing", nil, ExitInternal},
		{"only not-asked", []Result{nil, nil}, ExitInternal},
		{"one answer", []Result{answered}, ExitAnswered},
		{"answers, admission and a not-asked percentile", []Result{answered, admitted, nil}, ExitAnswered},
		{"a refusal", []Result{answered, refused}, ExitRefused},
		{"every data reason refuses", reasonsExcept(InvalidInput), ExitRefused},
		{"a usage error", []Result{answered, usage}, ExitUsage},
		{"a usage error outranks a refusal", []Result{refused, usage, answered}, ExitUsage},
		{"a usage error last", []Result{refused, refused, usage}, ExitUsage},
	} {
		if got := ExitCode(c.results...); got != c.want {
			t.Errorf("%s: exit %d, want %d", c.name, got, c.want)
		}
	}
	// Question 2 as the CLI will pass it, with the percentile not asked.
	slo := SLOAtReplicas(olsSweep(), ReplicasQuery{Headroom: 0.85})
	if got := ExitCode(slo.KneeRate, slo.Rate, slo.MeanTPOT, slo.PercentileTPOT); got != ExitAnswered {
		t.Errorf("question 2 answered: exit %d", got)
	}
}

func reasonsExcept(skip Reason) []Result {
	var out []Result
	for _, r := range Reasons() {
		if r != skip {
			out = append(out, Unanswerable{Reason: r})
		}
	}
	return out
}

// pageOnlyReasons are on the page's reasons table for terms later PRs build
// (the standalone role map); this package does not raise it yet.
var pageOnlyReasons = []string{"NoRoleMap"}

// Every exported Reason constant is in Reasons(), every one of them is on the
// page's reasons table, and the page lists no reason this package neither
// raises nor names as a later PR's.
func TestReasonsAreListedAndDocumented(t *testing.T) {
	fset := token.NewFileSet()
	// Every non-test file: the TTFT model declares its reasons beside it.
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var decls []ast.Decl
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		decls = append(decls, f.Decls...)
	}
	var declared []string
	for _, decl := range decls {
		g, ok := decl.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, spec := range g.Specs {
			v := spec.(*ast.ValueSpec)
			if id, ok := v.Type.(*ast.Ident); ok && id.Name == "Reason" {
				for _, n := range v.Names {
					declared = append(declared, n.Name)
				}
			}
		}
	}
	listed := make([]string, 0, len(Reasons()))
	for _, r := range Reasons() {
		listed = append(listed, string(r))
	}
	if len(declared) < 19 {
		t.Fatalf("parsed only %d Reason constants from the package: %v", len(declared), declared)
	}
	slices.Sort(declared)
	sortedListed := slices.Sorted(slices.Values(listed))
	if !slices.Equal(declared, sortedListed) {
		t.Errorf("Reason constants %v, Reasons() %v", declared, sortedListed)
	}

	page, err := os.ReadFile("../../../docs/proposals/slo-capacity-planner.md")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(page), "\r\n", "\n")
	start := strings.Index(text, "| Reason | When |")
	if start < 0 {
		t.Fatal("no reasons table on the page")
	}
	end := strings.Index(text[start:], "\n\n")
	if end < 0 {
		end = len(text) - start
	}
	rows := regexp.MustCompile("(?m)^\\| `(\\w+)` \\|").FindAllStringSubmatch(text[start:start+end], -1)
	onPage := make([]string, 0, len(rows))
	for _, m := range rows {
		onPage = append(onPage, m[1])
	}
	if len(onPage) < len(listed) {
		t.Fatalf("parsed only %d rows from the page's reasons table: %v", len(onPage), onPage)
	}
	for _, r := range listed {
		if !slices.Contains(onPage, r) {
			t.Errorf("reason %s is not on the page's reasons table", r)
		}
	}
	for _, r := range onPage {
		if !slices.Contains(listed, r) && !slices.Contains(pageOnlyReasons, r) {
			t.Errorf("the page lists %s, which neither Reasons() nor the later PRs' list holds", r)
		}
	}
}
