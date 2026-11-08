package metrics

import (
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
)

const lastAdmittedSeries = constants.LLMSMLearnedStateLastAdmittedTimestampSeconds

func report(learner, role, key string, at time.Time, forgotten ...string) learn.Report {
	r := learn.Report{Learner: learner, Namespace: "ns", ModelID: "m", Variant: "v", Role: role,
		PricingKey: key, LastAdmitted: at}
	for _, k := range forgotten {
		r.Forgotten = append(r.Forgotten, learn.Forgotten{Key: k, By: learn.ByOperator})
	}
	return r
}

func TestRecordLearnerReportsPublishesOneSeriesPerVariantAndLearner(t *testing.T) {
	emitter, registry := engineConfigEmitter(t)
	at := time.Unix(1_700_000_000, 500_000_000)
	emitter.RecordLearnerReports([]learn.Report{
		report(learn.K2, "decode", "k2-key", at),
		report(learn.Mu, "decode", "mu-key", at.Add(time.Minute)),
		report(learn.ITL, "decode", "itl-key", at.Add(2*time.Minute)),
		report(learn.Start, "decode", "start-key", at.Add(3*time.Minute)),
	}, nil)
	if got := countSeries(t, registry, lastAdmittedSeries); got != 4 {
		t.Fatalf("series = %d, want 4", got)
	}
	v, ok := seriesValue(t, registry, lastAdmittedSeries, map[string]string{
		constants.LabelVariantName: "v", constants.LabelRole: "decode", constants.LabelLearner: learn.Mu,
	})
	if !ok || v != 1_700_000_060.5 {
		t.Fatalf("mu = %v (present %v), want 1700000060.5", v, ok)
	}
}

// The slo and throughput lines have series of their own beside the saturation
// line's, and a forget of one leaves the others.
func TestRecordLearnerReportsKeepsTheThreeLinesApart(t *testing.T) {
	emitter, registry := engineConfigEmitter(t)
	at := time.Unix(1_700_000_000, 0)
	emitter.RecordLearnerReports([]learn.Report{
		report(learn.ITL, "decode", "sat-key", at),
		report(learn.SLO, "decode", "slo-key", at.Add(time.Minute)),
		report(learn.Throughput, "decode", "thr-key", at.Add(2*time.Minute)),
	}, nil)
	if got := countSeries(t, registry, lastAdmittedSeries); got != 3 {
		t.Fatalf("series = %d, want 3", got)
	}
	gone := report(learn.SLO, "decode", "", time.Time{}, "slo-key")
	emitter.RecordLearnerReports([]learn.Report{gone}, nil)
	if got := countSeries(t, registry, lastAdmittedSeries); got != 2 {
		t.Fatalf("after the forget: series = %d, want 2", got)
	}
	if _, ok := seriesValue(t, registry, lastAdmittedSeries, map[string]string{constants.LabelLearner: learn.SLO}); ok {
		t.Fatal("the forgotten line's series stands")
	}
}

// A figure with no stamp is as old as can be, not the year 1.
func TestRecordLearnerReportsPublishesZeroForNoStamp(t *testing.T) {
	emitter, registry := engineConfigEmitter(t)
	emitter.RecordLearnerReports([]learn.Report{report(learn.Start, "decode", "start-key", time.Time{})}, nil)
	if v, ok := seriesValue(t, registry, lastAdmittedSeries, map[string]string{constants.LabelLearner: learn.Start}); !ok || v != 0 {
		t.Fatalf("start = %v (present %v), want 0", v, ok)
	}
}

// A change of pricing key sets the series from the new key, and however many
// keys a variant moves through it holds one series: the key is no label.
func TestRecordLearnerReportsAChangeOfKeyStaysOneSeries(t *testing.T) {
	emitter, registry := engineConfigEmitter(t)
	at := time.Unix(1_700_000_000, 0)
	for i := range 1000 {
		emitter.RecordLearnerReports([]learn.Report{report(learn.K2, "decode", "key-"+string(rune('a'+i%26)), at.Add(time.Duration(i)*time.Second))}, nil)
	}
	if got := countSeries(t, registry, lastAdmittedSeries); got != 1 {
		t.Fatalf("series = %d, want 1", got)
	}
	if v, _ := seriesValue(t, registry, lastAdmittedSeries, map[string]string{constants.LabelLearner: learn.K2}); v != 1_700_000_999 {
		t.Fatalf("value = %v, want the last report's", v)
	}
}

// A new role moves the series, and leaves nothing behind under the old one.
func TestRecordLearnerReportsAChangeOfRoleMovesTheSeries(t *testing.T) {
	emitter, registry := engineConfigEmitter(t)
	at := time.Unix(1_700_000_000, 0)
	emitter.RecordLearnerReports([]learn.Report{report(learn.K2, "decode", "key", at)}, nil)
	emitter.RecordLearnerReports([]learn.Report{report(learn.K2, "both", "key", at)}, nil)
	if got := countSeries(t, registry, lastAdmittedSeries); got != 1 {
		t.Fatalf("series = %d, want 1", got)
	}
	if _, ok := seriesValue(t, registry, lastAdmittedSeries, map[string]string{constants.LabelRole: "decode"}); ok {
		t.Fatal("the old role's series is still up")
	}
}

// A report with no pricing key leaves the series: a learner that stopped
// pricing still shows how old its evidence is. A forget of another key does
// too. Only the key published going takes the series with it, and a key
// after that publishes it again.
func TestRecordLearnerReportsDeletesOnlyWhenThePublishedKeyIsForgotten(t *testing.T) {
	emitter, registry := engineConfigEmitter(t)
	at := time.Unix(1_700_000_000, 0)
	emitter.RecordLearnerReports([]learn.Report{report(learn.K2, "decode", "key", at)}, nil)

	emitter.RecordLearnerReports([]learn.Report{report(learn.K2, "decode", "", time.Time{})}, nil)
	if v, ok := seriesValue(t, registry, lastAdmittedSeries, map[string]string{constants.LabelLearner: learn.K2}); !ok || v != 1_700_000_000 {
		t.Fatalf("no key priced: %v (present %v), want the value kept", v, ok)
	}
	emitter.RecordLearnerReports([]learn.Report{report(learn.K2, "decode", "", time.Time{}, "another-key")}, nil)
	if got := countSeries(t, registry, lastAdmittedSeries); got != 1 {
		t.Fatalf("another key forgotten: series = %d, want 1", got)
	}
	emitter.RecordLearnerReports([]learn.Report{report(learn.K2, "decode", "", time.Time{}, "another-key", "key")}, nil)
	if got := countSeries(t, registry, lastAdmittedSeries); got != 0 {
		t.Fatalf("the published key forgotten: series = %d, want 0", got)
	}
	emitter.RecordLearnerReports([]learn.Report{report(learn.K2, "decode", "new-key", at)}, nil)
	if got := countSeries(t, registry, lastAdmittedSeries); got != 1 {
		t.Fatalf("a new key: series = %d, want 1", got)
	}
}

// A series remembers its variant's scale target, through a report that has
// none, once per variant however many learners publish.
func TestLearnerVariantsCarryTheScaleTarget(t *testing.T) {
	emitter, _ := engineConfigEmitter(t)
	at := time.Unix(1_700_000_000, 0)
	target := LearnerTarget{Kind: "Deployment", Name: "d"}
	emitter.RecordLearnerReports([]learn.Report{report(learn.K2, "decode", "key", at), report(learn.Mu, "decode", "key", at)},
		func(namespace, variant string) LearnerTarget {
			if namespace != "ns" || variant != "v" {
				t.Fatalf("targetOf(%q, %q)", namespace, variant)
			}
			return target
		})
	emitter.RecordLearnerReports([]learn.Report{report(learn.K2, "decode", "key", at)},
		func(string, string) LearnerTarget { return LearnerTarget{} })
	emitter.RecordLearnerReports([]learn.Report{report(learn.Mu, "decode", "key", at)}, nil)
	emitter.RecordLearnerReports([]learn.Report{report(learn.ITL, "decode", "key", at)}, nil) // published with none
	var got []LearnerVariant
	for _, v := range emitter.LearnerVariants() {
		if v.Namespace == "ns" && v.ModelID == "m" && v.Variant == "v" {
			got = append(got, v)
		}
	}
	if len(got) != 1 || got[0].Target != target {
		t.Fatalf("LearnerVariants() = %+v, want one with %+v", got, target)
	}
}

// A key forgotten with another pricing in the same report is a change of key.
func TestRecordLearnerReportsAForgottenKeyWithANewOneKeepsTheSeries(t *testing.T) {
	emitter, registry := engineConfigEmitter(t)
	at := time.Unix(1_700_000_000, 0)
	emitter.RecordLearnerReports([]learn.Report{report(learn.K2, "decode", "key", at)}, nil)
	emitter.RecordLearnerReports([]learn.Report{report(learn.K2, "decode", "new-key", at.Add(time.Second), "key")}, nil)
	if v, ok := seriesValue(t, registry, lastAdmittedSeries, map[string]string{constants.LabelLearner: learn.K2}); !ok || v != 1_700_000_001 {
		t.Fatalf("value = %v (present %v), want the new key's", v, ok)
	}
}

// One learner's forget leaves the other learners' series.
func TestRecordLearnerReportsAForgetIsPerLearner(t *testing.T) {
	emitter, registry := engineConfigEmitter(t)
	at := time.Unix(1_700_000_000, 0)
	emitter.RecordLearnerReports([]learn.Report{report(learn.K2, "decode", "key", at), report(learn.Mu, "decode", "key", at)}, nil)
	emitter.RecordLearnerReports([]learn.Report{report(learn.K2, "decode", "", time.Time{}, "key")}, nil)
	if _, ok := seriesValue(t, registry, lastAdmittedSeries, map[string]string{constants.LabelLearner: learn.Mu}); !ok {
		t.Fatal("mu's series went with k2's forget")
	}
}

func TestRetainLearnerSeriesKeepsOnlyWhatIsKnown(t *testing.T) {
	emitter, registry := engineConfigEmitter(t)
	at := time.Unix(1_700_000_000, 0)
	seed := func() {
		for _, r := range []learn.Report{
			{Learner: learn.K2, Namespace: "ns", ModelID: "m", Variant: "v", Role: "decode", PricingKey: "k", LastAdmitted: at},
			{Learner: learn.Mu, Namespace: "ns", ModelID: "m", Variant: "v", Role: "decode", PricingKey: "k", LastAdmitted: at},
			{Learner: learn.K2, Namespace: "ns", ModelID: "m", Variant: "w", Role: "decode", PricingKey: "k", LastAdmitted: at},
			{Learner: learn.K2, Namespace: "ns", ModelID: "other", Variant: "v", Role: "decode", PricingKey: "k", LastAdmitted: at},
		} {
			emitter.RecordLearnerReports([]learn.Report{r}, nil)
		}
	}
	seed()
	emitter.RetainLearnerSeries(func(namespace, modelID, variant string) bool {
		return modelID != "m" || variant != "v"
	})
	if got := countSeries(t, registry, lastAdmittedSeries); got != 2 {
		t.Fatalf("after the variant: series = %d, want 2", got)
	}
	learnerPublishedMu.Lock()
	_, other := learnerPublished[learnerSeriesID{"ns", "other", "v", learn.K2}]
	learnerPublishedMu.Unlock()
	if !other {
		t.Fatal("the bookkeeping lost a same-named variant of another model")
	}
	seed()
	emitter.RetainLearnerSeries(func(namespace, modelID, variant string) bool { return modelID != "m" })
	if got := countSeries(t, registry, lastAdmittedSeries); got != 1 {
		t.Fatalf("after the model: series = %d, want 1", got)
	}
	emitter.RetainLearnerSeries(func(string, string, string) bool { return false })
	if got := countSeries(t, registry, lastAdmittedSeries); got != 0 {
		t.Fatalf("after everything: series = %d, want 0", got)
	}
	seed()
	emitter.RecordLearnerReports([]learn.Report{{Learner: learn.K2, Namespace: "ns", ModelID: "m", Variant: "v",
		Role: "both", PricingKey: "k", LastAdmitted: at}}, nil)
	emitter.RetainLearnerSeries(func(string, string, string) bool { return false })
	if got := countSeries(t, registry, lastAdmittedSeries); got != 0 {
		t.Fatalf("a series that moved role: series = %d, want 0", got)
	}
	learnerPublishedMu.Lock()
	defer learnerPublishedMu.Unlock()
	if _, ok := learnerPublished[learnerSeriesID{"ns", "m", "v", learn.K2}]; ok {
		t.Fatal("the bookkeeping kept a deleted series")
	}
}

// No labels past the five documented, whatever the report holds: a pod, a
// bucket or a key as a label would grow with the fleet.
func TestLearnerSeriesLabelsAreBounded(t *testing.T) {
	emitter, registry := engineConfigEmitter(t)
	emitter.RecordLearnerReports([]learn.Report{report(learn.K2, "decode", "key", time.Unix(1, 0))}, nil)
	mfs, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{constants.LabelNamespace: true, constants.LabelModelName: true,
		constants.LabelVariantName: true, constants.LabelRole: true, constants.LabelLearner: true,
		constants.LabelControllerInstance: true}
	checked := 0
	for _, mf := range mfs {
		if mf.GetName() != lastAdmittedSeries {
			continue
		}
		for _, m := range mf.GetMetric() {
			checked++
			for _, l := range m.GetLabel() {
				if !want[l.GetName()] {
					t.Fatalf("unexpected label %q", l.GetName())
				}
			}
		}
	}
	if checked != 1 {
		t.Fatalf("checked %d series, want 1", checked)
	}
}
