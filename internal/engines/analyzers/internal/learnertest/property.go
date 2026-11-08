package learnertest

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// Property cases for the analyzers' learners (analyzer-learner.md, 2.6 and
// 2.7). Each case is a synthetic input run against one learner on many
// seeds. A check returns a *Violation when the learner breaks the property,
// and any other error when the case itself could not run; the latter never
// counts as a known failure.
//
// A case that fails on today's code by design is a row of its package's
// known-failures ledger, with the violation kind it is expected to fail
// with. Each subtest judges its own case against the ledger, so a filtered
// -run judges only what it ran:
//   - a case not in the ledger fails its subtest on any violation;
//   - a ledgered case must fail with its expected kind, and fails its subtest
//     if it fails with another kind, or (on a full seed run) not at all.

// Class is one of the twelve property classes of 2.6.
type Class string

// The twelve classes, as 2.6 names them.
const (
	ClassTransient      Class = "transient"
	ClassFirstSample    Class = "first-sample"
	ClassStale          Class = "stale-value"
	ClassMissing        Class = "missing-duplicated"
	ClassHeterogeneity  Class = "contamination-heterogeneity"
	ClassRedistribution Class = "redistribution"
	ClassNDependence    Class = "n-dependence"
	ClassKnownTruth     Class = "known-truth"
	ClassScale          Class = "scale"
	ClassLiveness       Class = "liveness"
	ClassMonotonicity   Class = "monotonicity"
	ClassEdges          Class = "edges"
)

// AllClasses is every class.
var AllClasses = []Class{
	ClassTransient, ClassFirstSample, ClassStale, ClassMissing, ClassHeterogeneity,
	ClassRedistribution, ClassNDependence, ClassKnownTruth, ClassScale, ClassLiveness,
	ClassMonotonicity, ClassEdges,
}

// Family is an input family. Every class runs on the time-base family as
// well as on direct inputs (2.6), unless a package's coverage table exempts
// it with a reason.
type Family string

// The two input families.
const (
	FamilyDirect   Family = "direct"
	FamilyTimeBase Family = "time-base"
)

// Violation kinds. A ledger row names the kind its case is expected to fail
// with; a case failing with another kind is a new failure.
const (
	KindNotLearned  = "not-learned"        // the learner learned nothing: the check would be vacuous
	KindTransient   = "transient-admitted" // a transient set or moved a figure
	KindAdoptedRaw  = "adopted-raw"        // one reading set a figure a second one moves
	KindStale       = "stale"              // a figure did not converge on identifying data
	KindMoved       = "moved"              // a figure moved where the property says it stays
	KindBiased      = "biased"             // a figure is off its declared estimand or truth
	KindOverPriced  = "over-priced"        // priced capacity over truth x (1 + epsilon)
	KindNDependent  = "n-dependent"        // an expectation moved with N
	KindFlipped     = "flipped"            // a verdict or key reversed more than the path allows
	KindFell        = "fell"               // demand fell along a rising trajectory
	KindUnbounded   = "unbounded"          // demand over what the fleet can drain
	KindNotDetected = "not-detected"       // a refusal the property needs did not happen
	KindWrongSide   = "wrong-side"         // a verdict on the wrong side of its threshold
)

// Violation is a broken property: the kind, and what was measured.
type Violation struct {
	Kind, Msg string
}

func (v *Violation) Error() string { return v.Kind + ": " + v.Msg }

// Violate returns a *Violation of kind.
func Violate(kind, format string, args ...any) error {
	return &Violation{Kind: kind, Msg: fmt.Sprintf(format, args...)}
}

// Holds is nil when cond holds, and a violation of kind otherwise.
func Holds(cond bool, kind, format string, args ...any) error {
	if cond {
		return nil
	}
	return Violate(kind, format, args...)
}

// Case is one property case: a class, the learner it runs on, and a check
// that returns the violation it found for a seed, or nil.
type Case struct {
	Class   Class
	Learner string
	Name    string
	Family  Family
	// Tuning names the policy tuning the case runs under, for its log.
	Tuning string
	// Seeds is how many seeds the case runs on; zero is DefaultSeeds.
	Seeds int
	Check func(seed uint64) error
}

// ID is the ledger's key for the case: class/learner/name.
func (c Case) ID() string { return string(c.Class) + "/" + c.Learner + "/" + c.Name }

// Seed counts. A cheap case runs on CheapSeeds; one that drives an analyzer
// for minutes of simulated time on DefaultSeeds. Changing a generator's
// draws moves which seeds fail: a ledger row's measured rate is restated
// then.
const (
	DefaultSeeds = 20
	CheapSeeds   = 100
)

// SeedEnv runs every case on the one seed it names, to reproduce a failure:
// PROPSEED=7 go test -run 'TestLearnerProperties/<id>'. A ledgered case that
// passes on that one seed is not an error then.
const SeedEnv = "PROPSEED"

// SeedsFor is the seeds a case of n seeds runs on (zero is DefaultSeeds),
// and whether that is every one of them: SeedEnv narrows it to one.
func SeedsFor(n int) (seeds []uint64, full bool, err error) {
	if s := os.Getenv(SeedEnv); s != "" {
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return nil, false, fmt.Errorf("%s=%q: %w", SeedEnv, s, err)
		}
		return []uint64{n}, false, nil
	}
	if n == 0 {
		n = DefaultSeeds
	}
	seeds = make([]uint64, n)
	for i := range seeds {
		seeds[i] = uint64(i + 1)
	}
	return seeds, true, nil
}

// CaseRun is what a case did over its seeds.
type CaseRun struct {
	Seeds    int
	Failures []SeedFailure
}

// SeedFailure is one seed's violation.
type SeedFailure struct {
	Seed uint64
	V    *Violation
}

func (r CaseRun) String() string {
	if len(r.Failures) == 0 {
		return fmt.Sprintf("passes on %d/%d seeds", r.Seeds, r.Seeds)
	}
	f := r.Failures[0]
	return fmt.Sprintf("fails on %d/%d seeds; first, seed %d: %v", len(r.Failures), r.Seeds, f.Seed, f.V)
}

// RunCase runs check on every seed. A non-violation error is the case
// failing to run: it is returned as err and never counted as a violation.
func RunCase(check func(uint64) error, seeds []uint64) (CaseRun, error) {
	run := CaseRun{Seeds: len(seeds)}
	for _, s := range seeds {
		err := check(s)
		var v *Violation
		switch {
		case err == nil:
		case errors.As(err, &v):
			run.Failures = append(run.Failures, SeedFailure{Seed: s, V: v})
		default:
			return run, fmt.Errorf("seed %d: the case did not run: %w", s, err)
		}
	}
	return run, nil
}

// Judge holds one case's run to its ledger row (nil when it has none). full
// says every seed ran; a ledgered case that passes is an error only then.
func Judge(c Case, row *LedgerRow, run CaseRun, full bool) []error {
	var errs []error
	if row == nil {
		for i, f := range run.Failures {
			if i == 3 {
				errs = append(errs, fmt.Errorf("... and %d more seeds", len(run.Failures)-3))
				break
			}
			errs = append(errs, fmt.Errorf("%s [%s, %s] seed %d: %v", c.ID(), c.Family, c.Tuning, f.Seed, f.V))
		}
		if len(errs) > 0 {
			errs = append(errs, fmt.Errorf("%s: a new failure. Fix the learner; only a known defect with an owning PR becomes a ledger row (docs/developer-guide/testing.md)", c.ID()))
		}
		return errs
	}
	if len(run.Failures) == 0 && full {
		errs = append(errs, fmt.Errorf("%s passes on all %d seeds, and the ledger says it fails (%s, owner %s). If the learner was fixed, remove the row; if the case changed, check it still tests the finding",
			c.ID(), run.Seeds, row.Finding, row.Owner))
	}
	for _, f := range run.Failures {
		if f.V.Kind != row.Expect {
			errs = append(errs, fmt.Errorf("%s seed %d fails with %q, and its ledger row expects %q: a new failure: %v",
				c.ID(), f.Seed, f.V.Kind, row.Expect, f.V))
			break
		}
	}
	return errs
}

// LedgerRow is one known failure: (class, learner, finding, measured result,
// the PR that clears it or "open"), keyed by the case it names, with the
// violation kind the case fails with.
type LedgerRow struct {
	ID       string `json:"id"`
	Class    string `json:"class"`
	Learner  string `json:"learner"`
	Expect   string `json:"expect"`
	Finding  string `json:"finding"`
	Measured string `json:"measured"`
	Owner    string `json:"owner"`
}

// ReadLedger reads the ledger at path, a slash-separated path relative to the
// test's package.
func ReadLedger(t testing.TB, path string) []LedgerRow {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(path))
	if err != nil {
		t.Fatalf("read the known-failures ledger: %v", err)
	}
	var rows []LedgerRow
	if err := yaml.UnmarshalStrict(b, &rows); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return rows
}

// LoadLedger is the ledger at path, by case id.
func LoadLedger(t testing.TB, path string) map[string]*LedgerRow {
	t.Helper()
	rows := ReadLedger(t, path)
	out := make(map[string]*LedgerRow, len(rows))
	for i := range rows {
		out[rows[i].ID] = &rows[i]
	}
	return out
}

// ledgerOwner is the form a ledger row's owner takes: an issue, or "open". It
// holds the form only and reads no tracker, so a closed issue passes. It keeps
// out a plan's PR, which two rows still named after that PR closed.
var ledgerOwner = regexp.MustCompile(`^(#[0-9]+|open)$`)

// CheckLedger holds the ledger's rows to the registered cases: each names a
// case, agrees with its class and learner, carries every field, has an owner
// in ledgerOwner's form, is listed once, and the rows are sorted by id.
func CheckLedger(rows []LedgerRow, cases []Case) []error {
	byID := make(map[string]Case, len(cases))
	for _, c := range cases {
		byID[c.ID()] = c
	}
	var errs []error
	seen := map[string]bool{}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
		if seen[r.ID] {
			errs = append(errs, fmt.Errorf("ledger: %s is listed twice", r.ID))
		}
		seen[r.ID] = true
		if r.Expect == "" || r.Finding == "" || r.Measured == "" || r.Owner == "" {
			errs = append(errs, fmt.Errorf("ledger: %s needs an expected kind, a finding, a measured result and an owner (an issue, or \"open\")", r.ID))
		} else if !ledgerOwner.MatchString(r.Owner) {
			errs = append(errs, fmt.Errorf("ledger: %s is owned by %q: an owner is an issue (\"#394\") or \"open\", not a plan's PR", r.ID, r.Owner))
		}
		c, ok := byID[r.ID]
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("ledger: %s names no property case: a row with no failing case is an error", r.ID))
		case r.Class != string(c.Class) || r.Learner != c.Learner:
			errs = append(errs, fmt.Errorf("ledger: %s says class %q learner %q; the case is %q %q", r.ID, r.Class, r.Learner, c.Class, c.Learner))
		}
	}
	if !slices.IsSorted(ids) {
		errs = append(errs, errors.New("ledger: rows are not sorted by id"))
	}
	return errs
}

// RunProperties runs every case, each in its own subtest judged against its
// row of the ledger at ledgerPath.
func RunProperties(t *testing.T, cases []Case, ledgerPath string) {
	ledger := LoadLedger(t, ledgerPath)
	for _, c := range cases {
		t.Run(c.ID(), func(t *testing.T) {
			seeds, full, err := SeedsFor(c.Seeds)
			if err != nil {
				t.Fatal(err)
			}
			run, err := RunCase(c.Check, seeds)
			if err != nil {
				t.Fatalf("%s: %v", c.ID(), err)
			}
			row := ledger[c.ID()]
			if row != nil {
				t.Logf("[%s, %s] ledgered (expect %s, owner %s): %s; measured when listed: %s", c.Family, c.Tuning, row.Expect, row.Owner, run, row.Measured)
			} else {
				t.Logf("[%s, %s] %s", c.Family, c.Tuning, run)
			}
			for _, e := range Judge(c, row, run, full) {
				t.Error(e)
			}
		})
	}
}

// CheckCaseList holds the registered case ids to the list pinned at path, so
// a case dropped or renamed shows in the diff.
func CheckCaseList(t *testing.T, cases []Case, path string) {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Fields(string(b))
	got := map[string]bool{}
	for _, c := range cases {
		if got[c.ID()] {
			t.Errorf("property case %s is registered twice", c.ID())
		}
		got[c.ID()] = true
	}
	if ids := slices.Sorted(maps.Keys(got)); !slices.Equal(ids, want) {
		t.Errorf("registered cases differ from %s; registered:\n%s", path, strings.Join(ids, "\n"))
	}
}
