package scalertls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/scaler"
)

func init() { prepareRetry = time.Millisecond }

// statusRecorder keeps the last state, action and CA Bootstrap published.
type statusRecorder struct {
	mu            sync.Mutex
	state, action string
	caPEM, caSha  string
}

func (r *statusRecorder) SetScalerCA(certPEM, sha256Hex string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.caPEM, r.caSha = certPEM, sha256Hex
}

// publishedCA checks the published CA and returns its first certificate: it
// parses, holds nothing but certificates, and its sha256 is the one published.
func (r *statusRecorder) publishedCA(t *testing.T) *x509.Certificate {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if strings.Contains(r.caPEM, "PRIVATE KEY") || strings.Contains(r.caPEM, "KEY-----") {
		t.Fatalf("the published CA holds key material:\n%s", r.caPEM)
	}
	rest := []byte(r.caPEM)
	var first *x509.Certificate
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if block.Type != pemCertificate || err != nil {
			t.Fatalf("published a %s block (%v)", block.Type, err)
		}
		if first == nil {
			first = c
		}
	}
	if first == nil || strings.TrimSpace(string(rest)) != "" {
		t.Fatalf("the published CA does not parse as certificates:\n%s", r.caPEM)
	}
	if r.caSha != Fingerprint(first) {
		t.Fatalf("published sha256 %s, the certificate's is %s", r.caSha, Fingerprint(first))
	}
	return first
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

func prepare(t *testing.T, b *Bootstrap) *x509.Certificate {
	t.Helper()
	ca, err := b.Prepare(context.Background())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return ca
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

// Without KEDA's side the scaler stays on plaintext and says how to wire it,
// in numbered steps, with KEDA's namespace left to the operator.
func TestBootstrap_PlaintextUntilKEDAsSideIsInPlace(t *testing.T) {
	var c counter
	cl := newClient(t, &c, interceptor.Funcs{}, placeholders()...)
	st := &statusRecorder{}
	b := newBootstrap(cl, &clock{t: fixtureNow}, st)
	if ca := prepare(t, b); ca != nil {
		t.Fatal("serves TLS with no marker")
	}
	state, action := st.get()
	if !strings.HasPrefix(state, "plaintext: KEDA's side") {
		t.Fatalf("state %q", state)
	}
	if !strings.HasPrefix(action, "1. wire KEDA") {
		t.Fatalf("action is not numbered steps:\n%s", action)
	}
	for _, want := range []string{
		`KEDA_NS="${KEDA_NS:?set KEDA_NS to the namespace KEDA runs in`,
		"kubectl get secret " + ClientSecretName + " -n " + testNS,
		`{{index .data "ca.crt"}}`,
		`-n "$KEDA_NS" -f -`,
		"--server-side",
		`sed -e "s/__NAMESPACE__/` + testNS + `/g"`,
		"kubectl rollout restart deployment/scaling-manager-controller-manager -n " + testNS,
		"the CA is new",
	} {
		if !strings.Contains(action, want) {
			t.Errorf("action does not hold %q:\n%s", want, action)
		}
	}
	// The wire step ends in its command, so what follows "then restart the
	// controller: " runs as it stands; the reinstall note is a step of its
	// own, not prose after a ";" that a shell would try to run.
	lines := strings.Split(action, "\n")
	restart := "kubectl rollout restart deployment/scaling-manager-controller-manager -n " + testNS
	if !strings.HasSuffix(lines[0], " && "+restart) {
		t.Errorf("the wire step does not end in its command:\n%s", lines[0])
	}
	if len(lines) != 2 || !strings.HasPrefix(lines[1], "2. only on a reinstall") ||
		!strings.HasSuffix(lines[1], `kubectl rollout restart deployment/keda-operator -n "`+kedaNS+`"`) {
		t.Errorf("the KEDA restart for a new CA is not a step of its own ending in its command:\n%s", action)
	}

	// KEDA's side appears: the next check asks for a restart, nothing else.
	if err := cl.Create(context.Background(), marker()); err != nil {
		t.Fatal(err)
	}
	b.Check(context.Background())
	if _, action := st.get(); !strings.HasPrefix(action, "1. KEDA's side is in place now; restart the controller") {
		t.Fatalf("action after the marker: %q", action)
	}
}

// With a serving certificate the OpenShift service CA issued, KEDA must trust
// the service CA bundle, and the copy command takes ca.crt from there.
func TestBootstrap_TheServiceCAIsWhatKEDAIsToldToTrust(t *testing.T) {
	var c counter
	cl := newClient(t, &c, interceptor.Funcs{}, serviceCASecrets(t, true)...)
	st := &statusRecorder{}
	prepare(t, newBootstrap(cl, &clock{t: fixtureNow}, st))
	_, action := st.get()
	if !strings.Contains(action, "kubectl get configmap "+ServiceCAConfigMap+" -n "+testNS) ||
		!strings.Contains(action, `"ca.crt":"__SERVICE_CA__"`) || strings.Contains(action, `{{index .data "ca.crt"}}`) {
		t.Fatalf("the copy command does not take the service CA:\n%s", action)
	}
}

// A marker without the label is not KEDA's side.
func TestBootstrap_AnUnlabelledMarkerIsNotKEDAsSide(t *testing.T) {
	var c counter
	m := marker()
	m.Labels = nil
	cl := newClient(t, &c, interceptor.Funcs{}, append(placeholders(), m)...)
	st := &statusRecorder{}
	if ca := prepare(t, newBootstrap(cl, &clock{t: fixtureNow}, st)); ca != nil {
		t.Fatal("serves TLS on an unlabelled marker")
	}
	if state, _ := st.get(); !strings.Contains(state, "has no "+OwnerLabel+" label") {
		t.Fatalf("state %q", state)
	}
}

// failingGets fails the first n reads of the objects named.
func failingGets(n *int, names ...string) interceptor.Funcs {
	return interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			for _, name := range names {
				if key.Name == name && *n > 0 {
					*n--
					return apierrors.NewServiceUnavailable("apiserver restarting")
				}
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}
}

// Prepare fails closed: a marker it cannot read, or Secrets it cannot read
// while the marker says TLS, are retried, and still unknown stop the start
// rather than choose plaintext. Unknown Secrets with no marker are plaintext.
func TestBootstrap_PrepareFailsClosed(t *testing.T) {
	short := func() context.Context {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		t.Cleanup(cancel)
		return ctx
	}
	t.Run("a transient marker error is retried", func(t *testing.T) {
		var c counter
		n := 3
		cl := newClient(t, &c, failingGets(&n, MarkerConfigMapName), append(installerSecrets(t), marker())...)
		ca, err := newBootstrap(cl, &clock{t: fixtureNow}, &statusRecorder{}).Prepare(short())
		if err != nil || ca == nil {
			t.Fatalf("ca %v, err %v", ca != nil, err)
		}
	})
	t.Run("a marker that stays unknown stops the start", func(t *testing.T) {
		var c counter
		n := 1 << 30
		cl := newClient(t, &c, failingGets(&n, MarkerConfigMapName), installerSecrets(t)...)
		ca, err := newBootstrap(cl, &clock{t: fixtureNow}, &statusRecorder{}).Prepare(short())
		if err == nil || ca != nil || !strings.Contains(err.Error(), "cannot tell whether KEDA's side") {
			t.Fatalf("ca %v, err %v", ca != nil, err)
		}
	})
	t.Run("a transient Secret error with the marker is retried", func(t *testing.T) {
		var c counter
		n := 3
		cl := newClient(t, &c, failingGets(&n, CASecretName), append(installerSecrets(t), marker())...)
		ca, err := newBootstrap(cl, &clock{t: fixtureNow}, &statusRecorder{}).Prepare(short())
		if err != nil || ca == nil {
			t.Fatalf("ca %v, err %v", ca != nil, err)
		}
	})
	t.Run("Secrets that stay unknown with the marker stop the start", func(t *testing.T) {
		var c counter
		n := 1 << 30
		cl := newClient(t, &c, failingGets(&n, ServingSecretName), append(installerSecrets(t), marker())...)
		ca, err := newBootstrap(cl, &clock{t: fixtureNow}, &statusRecorder{}).Prepare(short())
		if err == nil || ca != nil || !strings.Contains(err.Error(), "KEDA's side of mutual TLS is in place") {
			t.Fatalf("ca %v, err %v", ca != nil, err)
		}
	})
	t.Run("Secrets that stay unknown without the marker are plaintext", func(t *testing.T) {
		var c counter
		n := 1 << 30
		cl := newClient(t, &c, failingGets(&n, ServingSecretName), installerSecrets(t)...)
		st := &statusRecorder{}
		ca, err := newBootstrap(cl, &clock{t: fixtureNow}, st).Prepare(short())
		if err != nil || ca != nil {
			t.Fatalf("ca %v, err %v", ca != nil, err)
		}
		if _, action := st.get(); !strings.Contains(action, "the latest check failed") {
			t.Fatalf("action %q", action)
		}
	})
}

// A failed check is reported, and the report goes once a check succeeds.
func TestBootstrap_AFailedCheckIsReportedUntilOneSucceeds(t *testing.T) {
	var c counter
	n := 0
	cl := newClient(t, &c, failingGets(&n, CASecretName), append(installerSecrets(t), marker())...)
	st := &statusRecorder{}
	b := newBootstrap(cl, &clock{t: fixtureNow}, st)
	prepare(t, b)
	if _, action := st.get(); action != "" {
		t.Fatalf("action %q", action)
	}
	n = 1 << 30
	b.Check(context.Background())
	if state, action := st.get(); !strings.Contains(action, "the latest check failed") || !strings.HasPrefix(state, "mutual TLS:") {
		t.Fatalf("after a failed check: state %q action %q", state, action)
	}
	n = 0
	b.Check(context.Background())
	if _, action := st.get(); action != "" {
		t.Fatalf("after a good check: action %q", action)
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
	ca := prepare(t, b)
	if ca == nil || c.total() != 0 {
		t.Fatalf("ca %v, writes %+v", ca != nil, c)
	}
	got, err := dial(t, cl, serve(t, b, ca, clk), clk.now())
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

// The serving certificate is issued again near its expiry and served from
// the next handshake on, with no restart.
func TestBootstrap_HotReloadsARenewedServingCertificate(t *testing.T) {
	var c counter
	cl := newClient(t, &c, interceptor.Funcs{}, append(installerSecrets(t), marker())...)
	clk := &clock{t: fixtureNow}
	b := newBootstrap(cl, clk, &statusRecorder{})
	ca := prepare(t, b)
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

// A first start into empty placeholders with KEDA's side in place: the CA is
// minted, and every replica, not only the one that minted it, asks for KEDA
// to be wired and restarted until KEDA presents the new client certificate.
// Then the mark leaves the Secret.
func TestBootstrap_AMintedCAAsksForKEDAOnEveryReplicaUntilKEDAPresentsIt(t *testing.T) {
	var c counter
	cl := newClient(t, &c, interceptor.Funcs{}, append(placeholders(), marker())...)
	clk := &clock{t: fixtureNow}
	st := &statusRecorder{}
	b := newBootstrap(cl, clk, st)
	ca := prepare(t, b)
	if ca == nil || c.updates != 3 {
		t.Fatalf("ca %v writes %+v", ca != nil, c)
	}
	other := &statusRecorder{}
	prepare(t, newBootstrap(cl, clk, other))
	for name, rec := range map[string]*statusRecorder{"minting replica": st, "other replica": other} {
		if _, action := rec.get(); !strings.Contains(action, "issued for a new CA") ||
			!strings.Contains(action, "kubectl rollout restart deployment/keda-operator") {
			t.Fatalf("%s: action %q", name, action)
		}
	}
	if _, err := dial(t, cl, serve(t, b, ca, clk), clk.now()); err != nil {
		t.Fatal(err)
	}
	if state, action := st.get(); action != "" || !strings.HasPrefix(state, "mutual TLS:") {
		t.Fatalf("after KEDA presented its certificate: state %q action %q", state, action)
	}
	b.Check(context.Background())
	if _, ok := getSecret(t, cl, ClientSecretName).Annotations[PendingAnnotation]; ok {
		t.Fatal("the pending mark stays on the Secret after KEDA presented the certificate")
	}
}

// The CA emptied under a running controller: a new CA is minted, but the
// scaler keeps serving the certificate of the CA it verifies clients with,
// and says the controller must restart.
func TestBootstrap_DoesNotServeACertificateOfAChangedCA(t *testing.T) {
	var c counter
	cl := newClient(t, &c, interceptor.Funcs{}, append(installerSecrets(t), marker())...)
	st := &statusRecorder{}
	clk := &clock{t: fixtureNow}
	b := newBootstrap(cl, clk, st)
	ca := prepare(t, b)
	before, err := dial(t, cl, serve(t, b, ca, clk), clk.now())
	if err != nil {
		t.Fatal(err)
	}
	s := getSecret(t, cl, CASecretName)
	s.Data = map[string][]byte{corev1.TLSCertKey: {}, corev1.TLSPrivateKeyKey: {}, corev1.ServiceAccountRootCAKey: {}}
	if err := cl.Update(context.Background(), s); err != nil {
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

// The status carries the public certificate the served chain verifies
// against, and no key, whichever path the certificates came from; and it
// follows what is served, not a CA changed underneath.
func TestBootstrap_PublishesThePublicCAOnly(t *testing.T) {
	fixture := certOf(t, installerSecret(t, CASecretName, "ca", "ca.crt", "ca.key"))

	t.Run("adopted, mutual TLS", func(t *testing.T) {
		var c counter
		objs := installerSecrets(t)
		// A tls.crt that also holds the key must not leak it.
		ca := objs[0].(*corev1.Secret)
		ca.Data[corev1.TLSCertKey] = append(append([]byte{}, ca.Data[corev1.TLSCertKey]...), ca.Data[corev1.TLSPrivateKeyKey]...)
		cl := newClient(t, &c, interceptor.Funcs{}, append(objs, marker())...)
		st := &statusRecorder{}
		b := newBootstrap(cl, &clock{t: fixtureNow}, st)
		if prepare(t, b) == nil {
			t.Fatal("no TLS")
		}
		if Fingerprint(st.publishedCA(t)) != Fingerprint(fixture) {
			t.Fatal("published another CA than the one served")
		}
		// The CA emptied under the running controller: the new one is not
		// served, so the published one stays.
		s := getSecret(t, cl, CASecretName)
		s.Data = map[string][]byte{corev1.TLSCertKey: {}, corev1.TLSPrivateKeyKey: {}, corev1.ServiceAccountRootCAKey: {}}
		if err := cl.Update(context.Background(), s); err != nil {
			t.Fatal(err)
		}
		b.Check(context.Background())
		if Fingerprint(st.publishedCA(t)) != Fingerprint(fixture) {
			t.Fatal("published a CA the served certificate does not verify against")
		}
	})
	t.Run("minted, plaintext", func(t *testing.T) {
		var c counter
		cl := newClient(t, &c, interceptor.Funcs{}, placeholders()...)
		st := &statusRecorder{}
		prepare(t, newBootstrap(cl, &clock{t: fixtureNow}, st))
		if Fingerprint(st.publishedCA(t)) != Fingerprint(certOf(t, getSecret(t, cl, CASecretName))) {
			t.Fatal("did not publish the minted CA")
		}
	})
	t.Run("cert-manager", func(t *testing.T) {
		var c counter
		objs := installerSecrets(t)
		for _, o := range objs {
			o.SetAnnotations(map[string]string{"cert-manager.io/certificate-name": o.GetName()})
		}
		// A ca.crt that also holds a key must not leak it.
		cs := objs[2].(*corev1.Secret)
		cs.Data[corev1.ServiceAccountRootCAKey] = append(append([]byte{}, cs.Data[corev1.ServiceAccountRootCAKey]...),
			cs.Data[corev1.TLSPrivateKeyKey]...)
		cl := newClient(t, &c, interceptor.Funcs{}, append(objs, marker())...)
		st := &statusRecorder{}
		prepare(t, newBootstrap(cl, &clock{t: fixtureNow}, st))
		if Fingerprint(st.publishedCA(t)) != Fingerprint(fixture) {
			t.Fatal("did not publish cert-manager's CA")
		}
	})
	t.Run("service CA", func(t *testing.T) {
		var c counter
		objs := serviceCASecrets(t, true)
		cl := newClient(t, &c, interceptor.Funcs{}, append(objs, marker())...)
		st := &statusRecorder{}
		prepare(t, newBootstrap(cl, &clock{t: fixtureNow}, st))
		bundle, err := firstCert([]byte(objs[len(objs)-1].(*corev1.ConfigMap).Data[ServiceCAKey]))
		if err != nil {
			t.Fatal(err)
		}
		if Fingerprint(st.publishedCA(t)) != Fingerprint(bundle) {
			t.Fatal("did not publish the service CA the serving certificate verifies against")
		}
	})
	t.Run("nothing usable", func(t *testing.T) {
		var c counter
		cl := newClient(t, &c, interceptor.Funcs{})
		st := &statusRecorder{}
		prepare(t, newBootstrap(cl, &clock{t: fixtureNow}, st))
		if st.caPEM != "" || st.caSha != "" {
			t.Fatalf("published %q %q with no usable certificate", st.caPEM, st.caSha)
		}
	})
}

// cert-manager's Secrets: served as they are, nothing written.
func TestBootstrap_ServesCertManagerSecretsWithoutWriting(t *testing.T) {
	var c counter
	objs := installerSecrets(t)
	for _, o := range objs {
		o.SetAnnotations(map[string]string{"cert-manager.io/certificate-name": o.GetName()})
	}
	cl := newClient(t, &c, interceptor.Funcs{}, append(objs, marker())...)
	clk := &clock{t: fixtureNow}
	b := newBootstrap(cl, clk, &statusRecorder{})
	ca := prepare(t, b)
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
