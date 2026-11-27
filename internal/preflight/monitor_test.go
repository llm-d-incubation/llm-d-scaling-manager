package preflight

import (
	"strings"
	"testing"

	"k8s.io/client-go/tools/record"
)

// ready is ok only when no check fails; WARN and UNKNOWN do not block it,
// and each check's key holds its verdict, detail and fix.
func TestSummarize(t *testing.T) {
	rep := Report{Results: []Result{
		{Check: "settings", Status: Pass, Detail: "valid"},
		{Check: "kueue", Status: Warn, Detail: "no Kueue"},
		{Check: "scaler.tls", Status: Unknown, Detail: "blocked"},
	}}
	checks, ready := Summarize(rep)
	if ready != ReadyOK {
		t.Fatalf("ready %q with no FAIL", ready)
	}
	if checks["settings"] != "ok: valid" || checks["kueue"] != "warn: no Kueue" || checks["scaler.tls"] != "unknown: blocked" {
		t.Fatalf("checks %v", checks)
	}

	rep.Results = append(rep.Results,
		Result{Check: "prometheus.reachable", Status: Fail, Detail: "refused", Fix: "line one\nline two"},
		Result{Check: "keda.crd", Status: Fail, Detail: "absent"})
	checks, ready = Summarize(rep)
	if !strings.HasPrefix(ready, "not ready: keda.crd, prometheus.reachable") {
		t.Fatalf("ready %q", ready)
	}
	if checks["prometheus.reachable"] != "fail: refused; fix: line one line two" {
		t.Fatalf("prometheus.reachable %q", checks["prometheus.reachable"])
	}
}

// An Event per check on the first run and per change after, none for a
// check that stayed the same.
func TestRecordChanges(t *testing.T) {
	rec := record.NewFakeRecorder(20)
	m := &Monitor{Recorder: rec, StatusObject: StatusObjectRef("scaling-manager-system")}
	m.recordChanges(Report{Results: []Result{{Check: "a", Status: Pass}, {Check: "b", Status: Fail}}})
	if n := len(rec.Events); n != 2 {
		t.Fatalf("first run: %d events, want 2", n)
	}
	drain(rec)
	m.recordChanges(Report{Results: []Result{{Check: "a", Status: Pass}, {Check: "b", Status: Pass}}})
	events := drain(rec)
	if len(events) != 1 || !strings.Contains(events[0], "preflight check b is ok") || !strings.HasPrefix(events[0], "Normal") {
		t.Fatalf("second run events %v, want one Normal for b", events)
	}
	m.recordChanges(Report{Results: []Result{{Check: "a", Status: Pass}, {Check: "b", Status: Pass}}})
	if events := drain(rec); len(events) != 0 {
		t.Fatalf("unchanged run recorded %v", events)
	}
}

func drain(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}
