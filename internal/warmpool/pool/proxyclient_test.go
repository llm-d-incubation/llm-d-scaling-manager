package pool

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/warmpool/internal/deadaddr"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/warmpool/proxy"
)

// realProxy runs the actual proxy control handler, so these tests exercise the
// two halves against each other rather than against a restatement of the
// protocol.
// liveEngine stands up something that answers /health, and returns the port it
// bound. The proxy vets an engine before it will point at one -- pointing at
// nothing would leave the Pod READY with no model behind it -- so these tests
// need a real listener rather than a chosen number.
//
// The kernel picks the port, which is always well above the engine range floor.
// The listener is on 127.0.0.1 because that is the only host the proxy dials:
// an engine is Pod-local. httptest.NewServer falls back to [::1] when it cannot
// bind 127.0.0.1 (seen with the loopback ports exhausted by many test binaries
// side by side), and the proxy then got "connection refused" from an engine
// that was alive. Failing to bind 127.0.0.1 now fails the test, saying so.
func liveEngine(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind 127.0.0.1 for a live engine: %v", err)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case healthPath:
			w.WriteHeader(http.StatusOK)
		case sleepStatePath:
			// Awake. The proxy asks BOTH: a sleeping vLLM answers /health
			// perfectly well, so /health alone does not establish that this
			// engine can serve.
			_, _ = w.Write([]byte(`{"is_sleeping":false}`))
		default:
			http.NotFound(w, r)
		}
	})
	server := &httptest.Server{Listener: l, Config: &http.Server{Handler: handler}}
	server.Start()
	t.Cleanup(server.Close)
	return l.Addr().(*net.TCPAddr).Port
}

// deadEnginePort returns a loopback port with nothing that answers behind it,
// held for the life of the test binary so no other listener can take it
// (deadaddr).
func deadEnginePort(t *testing.T) int {
	t.Helper()
	return deadaddr.Port()
}

func realProxy(t *testing.T) (*Proxy, *proxy.Server) {
	t.Helper()
	// A window wide enough for a kernel-chosen port: these tests need a real
	// listener, and production's 9001-9016 cannot be bound reliably on a shared
	// machine. The range itself is covered in the proxy package's own tests.
	server := proxy.New(proxy.Config{
		MinUpstreamPort:   1024,
		UpstreamPortCount: 65535 - 1024,
	})
	mux := http.NewServeMux()
	mux.HandleFunc(proxy.UpstreamPath, server.UpstreamHandler)
	mux.HandleFunc(proxy.ReadyPath, server.ReadyHandler)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return &Proxy{client: ts.Client(), baseURL: ts.URL}, server
}

func TestPointAndClearDriveTheRealProxy(t *testing.T) {
	client, server := realProxy(t)
	ctx := context.Background()

	port := liveEngine(t)
	if err := client.Point(ctx, Endpoint{PodIP: "10.0.0.5", Port: port}); err != nil {
		t.Fatalf("Point: %v", err)
	}
	// The address is Pod-LOCAL: the proxy shares a network namespace with the
	// engines, so it dials 127.0.0.1 whatever the Pod's cluster IP is.
	if got, want := server.Upstream(), net.JoinHostPort("127.0.0.1", strconv.Itoa(port)); got != want {
		t.Fatalf("upstream = %q, want the loopback address %q", got, want)
	}

	if err := client.Clear(ctx); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if got := server.Upstream(); got != "" {
		t.Fatalf("Clear left %q behind", got)
	}
}

func TestPointingMakesThePodReadyAndClearingUnmakesIt(t *testing.T) {
	// This is the whole readiness mechanism: no Pod status is written anywhere,
	// the kubelet simply reads /readyz.
	client, server := realProxy(t)
	ctx := context.Background()

	rec := httptest.NewRecorder()
	server.ReadyHandler(rec, httptest.NewRequest(http.MethodGet, proxy.ReadyPath, nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a Pod with nothing awake must not be ready: %d", rec.Code)
	}

	if err := client.Point(ctx, Endpoint{Port: liveEngine(t)}); err != nil {
		t.Fatalf("Point: %v", err)
	}
	rec = httptest.NewRecorder()
	server.ReadyHandler(rec, httptest.NewRequest(http.MethodGet, proxy.ReadyPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("pointing the proxy must make the Pod ready: %d", rec.Code)
	}

	if err := client.Clear(ctx); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	rec = httptest.NewRecorder()
	server.ReadyHandler(rec, httptest.NewRequest(http.MethodGet, proxy.ReadyPath, nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("clearing must take the Pod out of service: %d", rec.Code)
	}
}

func TestUpstreamIsReadBackForReconciliation(t *testing.T) {
	// After a restart the controller asks the Pod what it is doing rather than
	// remembering what it asked for.
	client, _ := realProxy(t)
	ctx := context.Background()
	port := liveEngine(t)
	if err := client.Point(ctx, Endpoint{Port: port}); err != nil {
		t.Fatalf("Point: %v", err)
	}
	got, err := client.Upstream(ctx)
	if err != nil {
		t.Fatalf("Upstream: %v", err)
	}
	if !strings.HasSuffix(got, ":"+strconv.Itoa(port)) {
		t.Fatalf("Upstream = %q", got)
	}
}

func TestPointingAtAnEngineThatIsNotThereIsRefused(t *testing.T) {
	// Measured on a cluster: pointing at an address with nothing behind it left
	// the Pod READY, because the degraded flag is only raised once a request has
	// actually failed. Readiness would then admit traffic to nothing until the
	// first request 502'd. Vetting the engine before storing the pointer makes
	// "an upstream is set" mean "an engine answered".
	client, server := realProxy(t)

	err := client.Point(context.Background(), Endpoint{Port: deadEnginePort(t)})
	if err == nil {
		t.Fatal("want an error when the engine does not answer")
	}
	if server.Upstream() != "" {
		t.Errorf("a refused point must leave no upstream, got %q", server.Upstream())
	}

	rec := httptest.NewRecorder()
	server.ReadyHandler(rec, httptest.NewRequest(http.MethodGet, proxy.ReadyPath, nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("readiness = %d after a refused point, want 503", rec.Code)
	}
}
