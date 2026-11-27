package managedns

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/client-go/tools/record"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
	poolutil "github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/pool"
)

// setting is a watchNamespaces setting that a test changes while the
// Admission runs, as the settings reconciler does: no restart in between.
type setting struct {
	mu    sync.Mutex
	names []string
	all   bool
}

func (s *setting) get() ([]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.names), s.all
}

func (s *setting) set(all bool, names ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.names, s.all = names, all
}

// The watchNamespaces setting decides, and a change takes effect on the next
// call: a namespace refused a moment ago is admitted once it is listed.
func TestAdmissionFollowsTheSettingLive(t *testing.T) {
	ws := &setting{}
	ws.set(false, "team-a")
	a := &Admission{Namespaces: ws.get}
	ctx := context.Background()

	if why, err := a.Admit(ctx, "team-a", "so-a"); err != nil || why != "" {
		t.Fatalf("team-a refused (%q, %v); it is listed", why, err)
	}
	why, _ := a.Admit(ctx, "team-b", "so-b")
	if !strings.Contains(why, "not in the watchNamespaces setting (team-a)") {
		t.Fatalf("team-b: %q", why)
	}
	if got := a.Refused(); len(got) != 1 || !slices.Equal(got["team-b"].ScaledObjects, []string{"so-b"}) {
		t.Fatalf("Refused %+v", got)
	}

	ws.set(false, "team-a", "team-b")
	if why, _ := a.Admit(ctx, "team-b", "so-b"); why != "" {
		t.Fatalf("team-b still refused after the setting listed it: %q", why)
	}
	if got := a.Refused(); len(got) != 0 {
		t.Fatalf("a namespace managed since is still listed as refused: %+v", got)
	}

	ws.set(true)
	if !a.Manages("anything") {
		t.Fatal("an empty watchNamespaces must manage every namespace")
	}
}

// A rejected watchNamespaces with no value before it manages nothing: a typo
// in the first list never widens the install to every namespace.
func TestAdmissionRejectedSettingManagesNothing(t *testing.T) {
	a := &Admission{Namespaces: func() ([]string, bool) { return nil, false }}
	if why := a.Reason("team-a"); !strings.Contains(why, "was rejected") {
		t.Fatalf("Reason %q", why)
	}
}

// A refusal is forgotten after RefusedTTL.
func TestRefusedExpires(t *testing.T) {
	now := time.Unix(1000, 0)
	a := &Admission{Namespaces: func() ([]string, bool) { return []string{"team-a"}, false },
		Now: func() time.Time { return now }}
	_, _ = a.Admit(context.Background(), "team-b", "so")
	now = now.Add(RefusedTTL + time.Second)
	if got := a.Refused(); len(got) != 0 {
		t.Fatalf("Refused after the TTL: %+v", got)
	}
}

type fakeRegistry struct{ entries []registry.Entry }

func (f *fakeRegistry) Snapshot() []registry.Entry { return slices.Clone(f.entries) }
func (f *fakeRegistry) Forget(ns, name string) {
	f.entries = slices.DeleteFunc(f.entries, func(e registry.Entry) bool { return e.Namespace == ns && e.Name == name })
}

type fakePools struct{ pools []*poolutil.EndpointPool }

func (f *fakePools) PoolList() []*poolutil.EndpointPool { return slices.Clone(f.pools) }
func (f *fakePools) PoolDelete(n string) {
	f.pools = slices.DeleteFunc(f.pools, func(p *poolutil.EndpointPool) bool { return p.Namespace+"/"+p.Name == n })
}

type fakeSink struct {
	managed   []string
	all       bool
	refused   map[string]string
	readiness map[string]string
}

func (s *fakeSink) SetNamespaces(managed []string, all bool, refused map[string]string) {
	s.managed, s.all, s.refused = managed, all, refused
}
func (s *fakeSink) SetNamespaceReadiness(r map[string]string) { s.readiness = r }

func drain(r *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-r.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// One report: a namespace not listed loses its workloads and InferencePools,
// every namespace gets a readiness line, and each line reaches the objects
// in that namespace as an Event once, not on every report.
func TestReporterReadinessAndEvents(t *testing.T) {
	ws := &setting{}
	ws.set(false, "team-a", "team-c")
	reg := &fakeRegistry{entries: []registry.Entry{
		{Namespace: "team-a", Name: "so-a"},
		{Namespace: "team-b", Name: "so-b"},
	}}
	pools := &fakePools{pools: []*poolutil.EndpointPool{
		{Namespace: "team-a", Name: "pool-a"}, {Namespace: "team-b", Name: "pool-b"},
	}}
	sink := &fakeSink{}
	rec := record.NewFakeRecorder(50)
	changes := 0
	r := &Reporter{Admission: &Admission{Namespaces: ws.get}, Sink: sink, Registry: reg, Pools: pools,
		PoolAPIVersion: "inference.networking.k8s.io/v1", Recorder: rec, Log: logr.Discard(),
		OnChange: func(context.Context) { changes++ }}
	ctx := context.Background()
	r.Once(ctx)

	if len(reg.entries) != 1 || reg.entries[0].Namespace != "team-a" {
		t.Fatalf("registry %+v: team-b's workload must be forgotten", reg.entries)
	}
	if len(pools.pools) != 1 || pools.pools[0].Namespace != "team-a" {
		t.Fatalf("pools %+v: team-b's InferencePool must be dropped", pools.pools)
	}
	if !slices.Equal(sink.managed, []string{"team-a", "team-c"}) || sink.all {
		t.Fatalf("managed %v all %v", sink.managed, sink.all)
	}
	if got := sink.readiness["team-a"]; got != "ok: 1 ScaledObjects registered, 1 InferencePools" {
		t.Errorf("team-a %q", got)
	}
	if got := sink.readiness["team-b"]; !strings.HasPrefix(got, "not managed: it is not in the watchNamespaces setting") ||
		!strings.Contains(got, "so-b") {
		t.Errorf("team-b %q", got)
	}
	if got := sink.readiness["team-c"]; !strings.HasPrefix(got, "waiting: no ScaledObject here has called the scaler yet") {
		t.Errorf("team-c %q", got)
	}
	ev := drain(rec)
	want := []string{
		"Normal " + EventNamespaceReady + " ok: managed, and its ScaledObjects call the scaler",
		"Warning " + EventNamespaceNotManaged + " not managed: ",
	}
	for _, w := range want {
		if !slices.ContainsFunc(ev, func(e string) bool { return strings.HasPrefix(e, w) }) {
			t.Errorf("no Event %q in %q", w, ev)
		}
	}
	for _, e := range ev {
		if strings.Contains(e, EventNamespaceNotManaged) && strings.Contains(e, "team-a") {
			t.Errorf("team-b's Event names the managed namespaces: %q", e)
		}
	}
	if len(ev) != 3 { // so-a and pool-a in team-a, so-b in team-b
		t.Errorf("%d Events, want 3: %q", len(ev), ev)
	}

	r.Once(ctx)
	if ev := drain(rec); len(ev) != 0 {
		t.Errorf("an unchanged readiness was recorded again: %q", ev)
	}
	if changes != 0 {
		t.Errorf("OnChange called %d times with no change", changes)
	}

	// Another registration changes the counts in the status, not the Event:
	// only the new ScaledObject gets one, never so-a and pool-a again.
	reg.entries = append(reg.entries, registry.Entry{Namespace: "team-a", Name: "so-a2"})
	r.Once(ctx)
	if got := sink.readiness["team-a"]; got != "ok: 2 ScaledObjects registered, 1 InferencePools" {
		t.Errorf("team-a after a registration %q", got)
	}
	if ev := drain(rec); len(ev) != 1 {
		t.Errorf("a registration recorded %d Events, want 1 on the new ScaledObject: %q", len(ev), ev)
	}

	ws.set(true)
	r.Once(ctx)
	if changes != 1 || !sink.all {
		t.Errorf("after watchNamespaces emptied: OnChange %d, all %v", changes, sink.all)
	}
}

// A grant a switch needs that the controller lacks makes the namespace not
// ready, with the command that grants it; an optional one is a note.
func TestReporterGrants(t *testing.T) {
	reg := &fakeRegistry{entries: []registry.Entry{{Namespace: "team-a", Name: "so-a"}}}
	sink := &fakeSink{}
	rec := record.NewFakeRecorder(10)
	held := map[string]bool{}
	r := &Reporter{Admission: &Admission{Namespaces: func() ([]string, bool) { return []string{"team-a"}, false }},
		Sink: sink, Registry: reg, Recorder: rec, Log: logr.Discard(),
		Grants: func() []Grant {
			return []Grant{
				{Name: "pod-writer", Resource: "pods", Verb: "patch", Why: "utilizationShare is active", Required: true},
				{Name: "dset-role-writer", Group: "disaggregatedset.x-k8s.io", Resource: "disaggregatedsets", Verb: "patch", Why: "dsetRoleWriter is on"},
			}
		},
		Access: func(_ context.Context, ns string, g Grant) (bool, error) {
			if g.Name == "explode" {
				return false, errors.New("boom")
			}
			return held[ns+"/"+g.Name], nil
		}}
	r.Once(context.Background())
	got := sink.readiness["team-a"]
	if !strings.HasPrefix(got, "not ready: grant pod-writer missing (utilizationShare is active): scaling-manager-owner.sh grant pod-writer team-a") ||
		!strings.Contains(got, "no dset-role-writer grant") {
		t.Fatalf("team-a %q", got)
	}
	if ev := drain(rec); len(ev) != 1 || !strings.HasPrefix(ev[0], "Warning "+EventNamespaceNotReady) {
		t.Fatalf("Events %q", ev)
	}
	held["team-a/pod-writer"] = true
	r.Once(context.Background())
	if got := sink.readiness["team-a"]; !strings.HasPrefix(got, "ok: ") {
		t.Fatalf("with the grant, team-a %q", got)
	}
}
