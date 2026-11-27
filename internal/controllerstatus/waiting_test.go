package controllerstatus

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
)

const waitNS = "scaling-manager-system"

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func probe(addr, path string) int {
	resp, err := http.Get("http://" + addr + path) //nolint:noctx // a test probe
	if err != nil {
		return 0
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// With no prometheusURL the controller waits instead of exiting: live but not
// ready on its probes, ready and preflight.settings saying why in the status
// ConfigMap, and it returns the settings once prometheusURL is set. The
// negative control is the status while it waits: ready is never ok.
func TestSettingsWait(t *testing.T) {
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	addr := freeAddr(t)
	w := &SettingsWait{Client: cl, Namespace: waitNS, SettingsName: "scaling-manager-settings", ProbeAddr: addr,
		Interval: 20 * time.Millisecond, Done: config.ReadyToStart, Log: logr.Discard()}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type result struct {
		data map[string]string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		d, err := w.Wait(ctx)
		done <- result{d, err}
	}()

	statusKey := func(k string) string {
		cm := &corev1.ConfigMap{}
		if err := cl.Get(ctx, client.ObjectKey{Namespace: waitNS, Name: ConfigMapName}, cm); err != nil {
			return ""
		}
		return cm.Data[k]
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}

	waitFor("ready says there are no settings", func() bool { return strings.Contains(statusKey(KeyReady), "no ConfigMap scaling-manager-settings") })
	if !strings.HasPrefix(statusKey(KeyPreflightPrefix+"settings"), "fail: ") {
		t.Fatalf("preflight.settings %q", statusKey(KeyPreflightPrefix+"settings"))
	}
	waitFor("the probes", func() bool { return probe(addr, "/healthz") == http.StatusOK })
	if code := probe(addr, "/readyz"); code != http.StatusServiceUnavailable {
		t.Fatalf("readyz %d while waiting", code)
	}

	settings := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "scaling-manager-settings", Namespace: waitNS},
		Data: map[string]string{"passive": "true"}}
	if err := cl.Create(ctx, settings); err != nil {
		t.Fatal(err)
	}
	waitFor("ready says prometheusURL is not set", func() bool { return strings.Contains(statusKey(KeyReady), "prometheusURL is not set") })
	select {
	case r := <-done:
		t.Fatalf("returned before prometheusURL was set: %v %v", r.data, r.err)
	default:
	}

	settings.Data["prometheusURL"] = "https://prometheus:9090"
	if err := cl.Update(ctx, settings); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err != nil || r.data["prometheusURL"] != "https://prometheus:9090" {
			t.Fatalf("Wait returned %v, %v", r.data, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after prometheusURL was set")
	}
	// The probe port is free again for the manager.
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the probe port is still held: %v", err)
	}
	_ = l.Close()
}
