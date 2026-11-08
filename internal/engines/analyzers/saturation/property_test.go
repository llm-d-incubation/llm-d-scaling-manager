package saturation

import (
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/internal/learnertest"
)

// Property tests for the analyzers' learners (docs/proposals/analyzer-learner.md,
// 2.6 and 2.7). Each case is a synthetic input run against one learner on
// many seeds. A check returns a *propViolation when the learner breaks the
// property, and any other error when the case itself could not run; the
// latter never counts as a known failure.
//
// A case that fails on today's code by design is a row of the known-failures
// ledger (testdata/properties/known-failures.yaml), with the violation kind
// it is expected to fail with. Each subtest judges its own case against the
// ledger, so a filtered -run judges only what it ran:
//   - a case not in the ledger fails its subtest on any violation;
//   - a ledgered case must fail with its expected kind, and fails its subtest
//     if it fails with another kind, or (on a full seed run) not at all.
//
// The judgement, the ledger and the generators the analyzers share are
// learnertest's (the throughput analyzer's cases run on them too); the names
// below are theirs. The cases are in property_<learner>_test.go, the
// saturation analyzer's generators in property_gen_test.go, the mutants and
// the 2.7 accounting in property_mutants_test.go, and the coverage table in
// property_coverage_test.go.

// propClass is one of the twelve property classes of 2.6.
type propClass = learnertest.Class

// The twelve classes, as 2.6 names them.
const (
	classTransient      = learnertest.ClassTransient
	classFirstSample    = learnertest.ClassFirstSample
	classStale          = learnertest.ClassStale
	classMissing        = learnertest.ClassMissing
	classHeterogeneity  = learnertest.ClassHeterogeneity
	classRedistribution = learnertest.ClassRedistribution
	classNDependence    = learnertest.ClassNDependence
	classKnownTruth     = learnertest.ClassKnownTruth
	classScale          = learnertest.ClassScale
	classLiveness       = learnertest.ClassLiveness
	classMonotonicity   = learnertest.ClassMonotonicity
	classEdges          = learnertest.ClassEdges
)

// allPropClasses is every class.
var allPropClasses = learnertest.AllClasses

// propFamily is an input family. Every class runs on the time-base family as
// well as on direct inputs (2.6), unless property_coverage_test.go exempts
// it with a reason.
type propFamily = learnertest.Family

const (
	familyDirect   = learnertest.FamilyDirect
	familyTimeBase = learnertest.FamilyTimeBase
)

// propTuning names the analyzerTuning a case runs under.
type propTuning string

const (
	tunedDefault    propTuning = "default"
	tunedNonDefault propTuning = "steadyFor 10m, carry 15m, idleRows 5"
	// tunedPolicy is a case that runs the policy keys it lists (propCase.keys)
	// at values other than their defaults.
	tunedPolicy propTuning = "non-default policy keys"
)

// Violation kinds. A ledger row names the kind its case is expected to fail
// with; a case failing with another kind is a new failure.
const (
	kindNotLearned  = learnertest.KindNotLearned
	kindTransient   = learnertest.KindTransient
	kindAdoptedRaw  = learnertest.KindAdoptedRaw
	kindStale       = learnertest.KindStale
	kindMoved       = learnertest.KindMoved
	kindBiased      = learnertest.KindBiased
	kindOverPriced  = learnertest.KindOverPriced
	kindNDependent  = learnertest.KindNDependent
	kindFlipped     = learnertest.KindFlipped
	kindFell        = learnertest.KindFell
	kindUnbounded   = learnertest.KindUnbounded
	kindNotDetected = learnertest.KindNotDetected
	kindWrongSide   = learnertest.KindWrongSide
)

// propViolation is a broken property: the kind, and what was measured.
type propViolation = learnertest.Violation

// violation returns a *propViolation of kind.
func violation(kind, format string, args ...any) error {
	return learnertest.Violate(kind, format, args...)
}

// holds is nil when cond holds, and a violation of kind otherwise.
func holds(cond bool, kind, format string, args ...any) error {
	return learnertest.Holds(cond, kind, format, args...)
}

// propCase is one property case: a class, the learner it runs on, and a check
// that returns the violation it found for a seed, or nil.
type propCase struct {
	class   propClass
	learner string
	name    string
	family  propFamily
	tuning  propTuning
	// policy is the policy the case runs, set where it sets any key away
	// from its default, nil otherwise. The check reads the same value, so
	// the keys it sets (nonDefaultKeys) are the keys the case runs at a
	// non-default value: 2.6 requires one such case per key a learner reads
	// (policyKeys).
	policy *config.ScalingPolicy
	// seeds is how many seeds the case runs on; zero is defaultSeeds.
	seeds int
	check func(seed uint64) error
}

// common is the case as learnertest runs and judges it.
func (c propCase) common() learnertest.Case {
	return learnertest.Case{Class: c.class, Learner: c.learner, Name: c.name, Family: c.family,
		Tuning: string(c.tuning), Seeds: c.seeds, Check: c.check}
}

// commonCases is cases as learnertest runs them.
func commonCases(cases []propCase) []learnertest.Case {
	out := make([]learnertest.Case, 0, len(cases))
	for _, c := range cases {
		out = append(out, c.common())
	}
	return out
}

// id is the ledger's key for the case: class/learner/name.
func (c propCase) id() string { return c.common().ID() }

// Seed counts (learnertest.DefaultSeeds, learnertest.CheapSeeds).
const (
	defaultSeeds = learnertest.DefaultSeeds
	cheapSeeds   = learnertest.CheapSeeds
)

// runCase runs check on every seed (learnertest.RunCase).
func runCase(check func(uint64) error, seeds []uint64) (learnertest.CaseRun, error) {
	return learnertest.RunCase(check, seeds)
}

// ledgerRow is one known failure (learnertest.LedgerRow).
type ledgerRow = learnertest.LedgerRow

const ledgerPath = "testdata/properties/known-failures.yaml"

func loadLedger(t *testing.T) map[string]*ledgerRow {
	t.Helper()
	return learnertest.LoadLedger(t, ledgerPath)
}

// TestLearnerProperties runs every property case, each in its own subtest
// judged against its ledger row.
func TestLearnerProperties(t *testing.T) {
	learnertest.RunProperties(t, commonCases(propertyCases()), ledgerPath)
}

// TestPropertyLedgerNamesRegisteredCases checks the ledger file against the
// registered cases, whatever a -run filter selected.
func TestPropertyLedgerNamesRegisteredCases(t *testing.T) {
	for _, e := range learnertest.CheckLedger(learnertest.ReadLedger(t, ledgerPath), commonCases(propertyCases())) {
		t.Error(e)
	}
}

// The registered case ids are pinned in testdata/properties/cases.txt, so a
// case dropped or renamed shows in the diff.
func TestPropertyCaseListIsPinned(t *testing.T) {
	learnertest.CheckCaseList(t, commonCases(propertyCases()), "testdata/properties/cases.txt")
}
