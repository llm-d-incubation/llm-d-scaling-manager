package scalertls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/scaler"
)

// statusRecorder keeps the last state and action Bootstrap published.
type statusRecorder struct {
	mu            sync.Mutex
	state, action string
}

func (r *statusRecorder) SetScalerTLS(state, action string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state, r.action = state, action
}

func (r *statusRecorder) get() (string, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state, r.action
}

// clock is a settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) set(t time.Time) { c.mu.Lock(); defer c.mu.Unlock(); c.t = t }

func marker() *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: MarkerConfigMapName, Namespace: testNS,
		Labels: map[string]string{OwnerLabel: "keda-client"}}}
}

func newBootstrap(cl client.Client, clk *clock, st *statusRecorder) *Bootstrap {
	e := newEnsurer(cl, time.Time{})
	e.Now = clk.now
	return &Bootstrap{Ensurer: *e, Status: st}
}

// serve starts a TLS listener with the scaler's mutual-TLS configuration over
// b, as cmd/main.go builds it, on the test clock, answering each connection
// with one byte.
func serve(t *testing.T, b *Bootstrap, ca *x509.Certificate, clk *clock) string {
	t.Helper()
	cfg, err := scaler.NewMutualTLSConfig(b.GetCertificate, ca, []string{b.ClientSAN})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Time = clk.now
	verify := cfg.VerifyConnection
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		if err := verify(cs); err != nil {
			return err
		}
		b.ObserveHandshake(cs)
		return nil
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				if tc, ok := conn.(*tls.Conn); ok && tc.Handshake() == nil {
					_, _ = conn.Write([]byte{1})
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// dial completes a handshake as KEDA does, with the client certificate and
// the CA in Secret ClientSecretName, and returns the serving certificate.
func dial(t *testing.T, cl client.Reader, addr string, now time.Time) (*x509.Certificate, error) {
	t.Helper()
	s := getSecret(t, cl, ClientSecretName)
	pair, err := tls.X509KeyPair(s.Data[corev1.TLSCertKey], s.Data[corev1.TLSPrivateKeyKey])
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(s.Data[corev1.ServiceAccountRootCAKey])
	conn, err := tls.Dial("tcp", addr, &tls.Config{
		Certificates: []tls.Certificate{pair},
		RootCAs:      roots,
		ServerName:   ServiceName + "." + testNS + ".svc",
		Time:         func() time.Time { return now },
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	// The server verifies the client after the client's handshake returns
	// (TLS 1.3); the byte it sends proves it accepted us.
	if _, err := io.ReadFull(conn, make([]byte, 1)); err != nil {
		return nil, err
	}
	return conn.ConnectionState().PeerCertificates[0], nil
}

// Without KEDA's side the scaler stays on plaintext and says how to wire it.
func TestBootstrap_PlaintextUntilKEDAsSideIsInPlace(t *testing.T) {
	var c counter
	cl := newClient(t, &c, interceptor.Funcs{})
	st := &statusRecorder{}
	clk := &clock{t: fixtureNow}
	b := newBootstrap(cl, clk, st)
	if ca := b.Prepare(context.Background()); ca != nil {
		t.Fatal("serves TLS with no marker")
	}
	state, action := st.get()
	if !strings.HasPrefix(state, "plaintext: KEDA's side") {
		t.Fatalf("state %q", state)
	}
	for _, want := range []string{
		"kubectl get secret " + ClientSecretName + " -n " + testNS,
		`-n "$KEDA_NS" -f -`,
		"--server-side",
		`sed -e "s/__NAMESPACE__/` + testNS + `/g"`,
		"kubectl rollout restart deployment/scaling-manager-controller-manager -n " + testNS,
		"restart KEDA's operator too",
	} {
		if !strings.Contains(action, want) {
			t.Errorf("action does not hold %q:\n%s", want, action)
		}
	}

	// KEDA's side appears: the next check asks for a restart, nothing else.
	if err := cl.Create(context.Background(), marker()); err != nil {
		t.Fatal(err)
	}
	b.Check(context.Background())
	if _, action := st.get(); !strings.HasPrefix(action, "KEDA's side is in place now; restart the controller") {
		t.Fatalf("action after the marker: %q", action)
	}
}

// A marker without the label is not KEDA's side.
func TestBootstrap_AnUnlabelledMarkerIsNotKEDAsSide(t *testing.T) {
	var c counter
	m := marker()
	m.Labels = nil
	cl := newClient(t, &c, interceptor.Funcs{}, m)
	st := &statusRecorder{}
	if ca := newBootstrap(cl, &clock{t: fixtureNow}, st).Prepare(context.Background()); ca != nil {
		t.Fatal("serves TLS on an unlabelled marker")
	}
	if state, _ := st.get(); !strings.Contains(state, "has no "+OwnerLabel+" label") {
		t.Fatalf("state %q", state)
	}
}

// With KEDA's side in place and the installer's Secrets, the scaler serves
// mutual TLS with them, KEDA's certificate completes a handshake, and there is
// nothing to do.
func TestBootstrap_ServesTheAdoptedCertificatesOverMutualTLS(t *testing.T) {
	var c counter
	cl := newClient(t, &c, interceptor.Funcs{}, append(installerSecrets(t), marker())...)
	st := &statusRecorder{}
	clk := &clock{t: fixtureNow}
	b := newBootstrap(cl, clk, st)
	ca := b.Prepare(context.Background())
	if ca == nil || c.total() != 0 {
		t.Fatalf("ca %v, writes %+v", ca != nil, c)
	}
	addr := serve(t, b, ca, clk)
	got, err := dial(t, cl, addr, clk.now())
	if err != nil {
		t.Fatalf("KEDA's certificate was refused: %v", err)
	}
	if want := certOf(t, getSecret(t, cl, ServingSecretName)); Fingerprint(got) != Fingerprint(want) {
		t.Fatal("served another certificate than the adopted one")
	}
	state, action := st.get()
	if !strings.HasPrefix(state, "mutual TLS:") || action != "" {
		t.Fatalf("state %q action %q", state, action)
	}
}

// The serving certificate is re-issued near its expiry and served from the
// next handshake on, with no restart.
func TestBootstrap_HotReloadsARenewedServingCertificate(t *testing.T) {
	var c counter
	cl := newClient(t, &c, interceptor.Funcs{}, append(installerSecrets(t), marker())...)
	clk := &clock{t: fixtureNow}
	b := newBootstrap(cl, clk, &statusRecorder{})
	ca := b.Prepare(context.Background())
	addr := serve(t, b, ca, clk)
	before, err := dial(t, cl, addr, clk.now())
	if err != nil {
		t.Fatal(err)
	}

	clk.set(before.NotAfter.Add(-10 * 24 * time.Hour))
	b.Check(context.Background())
	if c.updates != 1 {
		t.Fatalf("writes %+v, want the serving Secret updated", c)
	}
	after, err := dial(t, cl, addr, clk.now())
	if err != nil {
		t.Fatalf("handshake after the renewal: %v", err)
	}
	if Fingerprint(after) == Fingerprint(before) {
		t.Fatal("still serves the old certificate")
	}
	if want := certOf(t, getSecret(t, cl, ServingSecretName)); Fingerprint(after) != Fingerprint(want) {
		t.Fatal("serves a certificate other than the renewed Secret's")
	}
}

// A first start in an empty namespace with KEDA's side in place: the CA is
// minted, and the status asks for KEDA to be wired and restarted until KEDA
// presents the new client certificate.
func TestBootstrap_AMintedCAAsksForKEDAUntilKEDAPresentsIt(t *testing.T) {
	var c counter
	cl := newClient(t, &c, interceptor.Funcs{}, marker())
	st := &statusRecorder{}
	clk := &clock{t: fixtureNow}
	b := newBootstrap(cl, clk, st)
	ca := b.Prepare(context.Background())
	if ca == nil || c.creates != 3 {
		t.Fatalf("ca %v writes %+v", ca != nil, c)
	}
	if _, action := st.get(); !strings.Contains(action, "a new scaler CA was minted") ||
		!strings.Contains(action, "kubectl rollout restart deployment/keda-operator") {
		t.Fatalf("action %q", action)
	}
	if _, err := dial(t, cl, serve(t, b, ca, clk), clk.now()); err != nil {
		t.Fatal(err)
	}
	if state, action := st.get(); action != "" || !strings.HasPrefix(state, "mutual TLS:") {
		t.Fatalf("after KEDA presented its certificate: state %q action %q", state, action)
	}
}

// The CA Secret deleted under a running controller: a new CA is minted, but
// the scaler keeps serving the certificate of the CA it verifies clients with,
// and says the controller must restart.
func TestBootstrap_DoesNotServeACertificateOfAChangedCA(t *testing.T) {
	var c counter
	cl := newClient(t, &c, interceptor.Funcs{}, append(installerSecrets(t), marker())...)
	st := &statusRecorder{}
	clk := &clock{t: fixtureNow}
	b := newBootstrap(cl, clk, st)
	ca := b.Prepare(context.Background())
	addr := serve(t, b, ca, clk)
	before, err := dial(t, cl, addr, clk.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.Delete(context.Background(), getSecret(t, cl, CASecretName)); err != nil {
		t.Fatal(err)
	}
	b.Check(context.Background())
	cur, _ := b.GetCertificate(nil)
	if Fingerprint(cur.Leaf) != Fingerprint(before) {
		t.Fatal("serves a certificate of the new CA, which the scaler does not verify clients with")
	}
	if _, action := st.get(); !strings.Contains(action, "changed since the controller started") {
		t.Fatalf("action %q", action)
	}
}

// cert-manager's Secrets: served as they are, nothing written, nothing to do.
func TestBootstrap_ServesCertManagerSecretsWithoutWriting(t *testing.T) {
	var c counter
	objs := installerSecrets(t)
	for _, o := range objs {
		o.SetAnnotations(map[string]string{"cert-manager.io/certificate-name": o.GetName()})
	}
	cl := newClient(t, &c, interceptor.Funcs{}, append(objs, marker())...)
	clk := &clock{t: fixtureNow}
	b := newBootstrap(cl, clk, &statusRecorder{})
	ca := b.Prepare(context.Background())
	if ca == nil {
		t.Fatal("cert-manager's certificates are not served")
	}
	if _, err := dial(t, cl, serve(t, b, ca, clk), clk.now()); err != nil {
		t.Fatal(err)
	}
	clk.set(certOf(t, objs[1].(*corev1.Secret)).NotAfter.Add(-10 * 24 * time.Hour))
	b.Check(context.Background())
	if c.total() != 0 {
		t.Fatalf("wrote %+v to cert-manager's Secrets", c)
	}
}
