package controllerstatus

import (
	"testing"
)

// The status keys of the install from the one release manifest: the managed
// and refused namespaces, each namespace's readiness, the preflight checks
// with ready, and KEDA's connection.
func TestInstallStatusKeys(t *testing.T) {
	p := New(nil, nil, "scaling-manager-system")

	p.SetNamespaces([]string{"team-b", "team-a"}, false, map[string]string{"tenant-x": "it is not in the watchNamespaces setting", "team-c": "not admitted"})
	d := p.Data()
	if got := d[KeyNamespacesManaged]; got != "team-a,team-b" {
		t.Errorf("%s %q", KeyNamespacesManaged, got)
	}
	if got := d[KeyNamespacesRefused]; got != "team-c: not admitted\ntenant-x: it is not in the watchNamespaces setting" {
		t.Errorf("%s %q", KeyNamespacesRefused, got)
	}
	p.SetNamespaces(nil, true, nil)
	d = p.Data()
	if got := d[KeyNamespacesManaged]; got != "*" {
		t.Errorf("every namespace managed: %s %q, want *", KeyNamespacesManaged, got)
	}
	p.SetNamespaces(nil, false, nil)
	d = p.Data()
	if got, ok := d[KeyNamespacesManaged]; !ok || got != "" {
		t.Errorf("no managed namespace: %s %q, present %v; it must say none, not vanish", KeyNamespacesManaged, got, ok)
	}
	if _, ok := d[KeyNamespacesRefused]; ok {
		t.Errorf("%s present with none refused", KeyNamespacesRefused)
	}

	p.SetNamespaceReadiness(map[string]string{"team-a": "ok: 1 ScaledObjects registered", "team-b": "waiting: none"})
	p.SetNamespaceReadiness(map[string]string{"team-a": "not ready: grant pod-writer missing"})
	d = p.Data()
	if got := d[KeyNamespacePrefix+"team-a"]; got != "not ready: grant pod-writer missing" {
		t.Errorf("%steam-a %q", KeyNamespacePrefix, got)
	}
	if _, ok := d[KeyNamespacePrefix+"team-b"]; ok {
		t.Error("a namespace no longer reported kept its readiness key")
	}

	p.SetPreflight(map[string]string{"keda.crd": "ok: found", "kueue": "warn: x"}, "ok")
	p.SetPreflight(map[string]string{"keda.crd": "fail: gone"}, "not ready: keda.crd")
	d = p.Data()
	if d[KeyPreflightPrefix+"keda.crd"] != "fail: gone" || d[KeyReady] != "not ready: keda.crd" {
		t.Errorf("preflight keys %q ready %q", d[KeyPreflightPrefix+"keda.crd"], d[KeyReady])
	}
	if _, ok := d[KeyPreflightPrefix+"kueue"]; ok {
		t.Error("a check no longer reported kept its key")
	}

	p.SetKEDAConnection("current", false)
	d = p.Data()
	if d[KeyKEDAConnection] != "current" || d[KeyKEDACopyStale] != "false" {
		t.Errorf("kedaConnection %q kedaCopyStale %q", d[KeyKEDAConnection], d[KeyKEDACopyStale])
	}

	var nilP *Publisher
	nilP.SetNamespaces([]string{"a"}, false, nil)
	nilP.SetNamespaceReadiness(map[string]string{"a": "ok"})
	nilP.SetPreflight(nil, "ok")
	nilP.SetKEDAConnection("none", true)
}
