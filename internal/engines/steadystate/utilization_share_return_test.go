package steadystate

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/tools/record"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// A model that keeps more headroom than maxHeadroomReplicas gives it, with
// nobody short of its need, gives the extra back once its scale-down window
// has passed: its pods are marked as a return, its target is lowered, the
// Event says where the GPUs went, and nobody is raised into GPUs its pods
// still hold. A restart reads the returns back as returns.
//
// A (important, weight 2) needs 2 and holds 9: its target is 4, and it stays
// calm down to 6 (2 <= 0.6 x (6 - 2)). C (need 1.25, holding 2) is one replica
// short of its headroom, so A first gives it one by a transfer; then, after
// its window, A returns 2. B (need 3.75, holding 5) is at its target.
func TestUtilizationShareReturnsSpare(t *testing.T) {
	reg := freshMetrics(t)
	f := newShareFleet()
	f.demand = map[string]float64{"A": 1600, "B": 3000, "C": 1000}
	c := sharePods(t, f)
	se := newShareEngine(t, f, c, time.Unix(0, 0))
	rec := record.NewFakeRecorder(100)
	se.e.Recorder = rec

	// B and C have recently burst to a need of 13: at their peaks the models
	// want 28 of the quota's 16, so the spare is capped (and only then
	// returned). Had only B burst, its room beside A's and C's peaks would
	// take A's spare by a transfer, and nothing would be returned.
	burst := func() {
		for _, l := range se.e.utilizationShare.ledgers {
			l.RecordNeeds(map[string]float64{"ns/B/" + domain.RoleBoth: 13, "ns/C/" + domain.RoleBoth: 13}, se.clock,
				allocation.ShareTimings{SwingWindow: time.Hour})
		}
	}
	var o map[string]utilizationShareOverride
	for range 40 {
		burst()
		if o = se.cycle(); targetOf(t, o, "ns/A-v") == 6 {
			break
		}
	}
	if got := targetOf(t, o, "ns/A-v"); got != 6 {
		t.Fatalf("A's target %d, want 6: the lowest it stays calm at", got)
	}
	if got := cappedGauge(t, reg); got != 1 {
		t.Fatalf("headroom_capped %v while the peaks contend, want 1", got)
	}
	if targetOf(t, o, "ns/B-v") != 5 || targetOf(t, o, "ns/C-v") != 2 {
		t.Fatalf("a model was raised into GPUs that are still releasing: %+v", o)
	}
	returned, given := 0, 0
	for _, p := range markedPods(t, c) {
		var m transferMark
		if err := json.Unmarshal([]byte(p.Annotations[utilizationShareTransferAnnotation]), &m); err != nil {
			t.Fatal(err)
		}
		switch {
		case m.Returned && m.Receiver == "" && m.Donor == roleA:
			returned++
		case !m.Returned && m.Receiver == "ns/C/"+domain.RoleBoth:
			given++
		default:
			t.Fatalf("pod %s carries %+v", p.Name, m)
		}
	}
	if returned != 2 || given != 1 {
		t.Fatalf("A returned %d and gave C %d, want 2 and 1", returned, given)
	}
	giving := 0
	for done := false; !done; {
		select {
		case ev := <-rec.Events:
			if strings.Contains(ev, constants.K8SEventUtilizationShareGiving) && strings.Contains(ev, "unassigned GPUs") {
				giving++
			}
		default:
			done = true
		}
	}
	if giving != 2 {
		t.Fatalf("%d Giving Events name the unassigned GPUs, want 2", giving)
	}
	starts := map[string]float64{}
	for _, m := range family(t, reg, constants.LLMSMUtilizationShareStartsTotal) {
		starts[label(m, constants.LabelKind)+"/"+label(m, constants.LabelSkippedWaits)] = m.GetCounter().GetValue()
	}
	if starts["return/false"] != 2 || starts["transfer/false"] != 1 {
		t.Fatalf("starts_total %v, want return=2 and transfer=1", starts)
	}

	// A restart reads the marks back as returns.
	se2 := newShareEngine(t, f, c, se.clock)
	se2.cycle()
	restored := 0
	for _, l := range se2.e.utilizationShare.ledgers {
		for _, tr := range l.Transfers() {
			if tr.Returned && tr.Entitled && tr.Receiver == "" {
				restored++
			}
		}
	}
	if restored != 2 {
		t.Fatalf("restored %d returns, want 2", restored)
	}
}

// An active group publishes the targets the planner acts on. On a quota whose
// peaks fit, it spreads the spare uncapped: the 16 GPUs of a fleet that needs
// 7 are all some model's target, not the 12 a capped evaluation would show.
func TestUtilizationSharePublishesThePlannersTargets(t *testing.T) {
	reg := freshMetrics(t)
	f := newShareFleet()
	f.demand = map[string]float64{"A": 1600, "B": 3000, "C": 1000}
	se := newShareEngine(t, f, sharePods(t, f), time.Unix(0, 0))
	// Past the restart quiet period, when the planner first plans, and past
	// a swing window, before which every model's peak is taken at its
	// ceiling and the cap is on.
	for range 10 {
		se.cycle()
	}
	// Every 30 s for 5 h, so the history never ages out (a gap longer than
	// the window starts it again).
	for range 600 {
		se.cycle()
	}
	sum := 0.0
	for _, m := range family(t, reg, constants.LLMSMUtilizationShareTargetGPUs) {
		sum += m.GetGauge().GetValue()
	}
	if sum < 15.5 {
		t.Fatalf("published targets sum to %.2f GPUs of 16: the capped evaluation, not the planner's", sum)
	}
	if got := cappedGauge(t, reg); got != 0 {
		t.Fatalf("headroom_capped %v on a quota whose peaks fit, want 0", got)
	}
}

// cappedGauge is the one group's llmsm_utilization_share_headroom_capped.
func cappedGauge(t *testing.T, reg *prometheus.Registry) float64 {
	t.Helper()
	ms := family(t, reg, constants.LLMSMUtilizationShareHeadroomCapped)
	if len(ms) != 1 {
		t.Fatalf("%d headroom_capped series, want 1", len(ms))
	}
	return ms[0].GetGauge().GetValue()
}
