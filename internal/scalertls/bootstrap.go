package scalertls

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DefaultInterval is how often Bootstrap checks the Secrets after the first
// pass: a serving certificate is re-issued at most this long after it enters
// RenewBefore, and a deleted CA is minted again within it.
const DefaultInterval = 10 * time.Minute

// ControllerDeployment is the controller's Deployment, which every install
// path names the same.
const ControllerDeployment = "scaling-manager-controller-manager"

// StatusSink receives the state of the scaler's TLS for the controller-status
// ConfigMap: state is always set; action is what a person must run, or "".
type StatusSink interface {
	SetScalerTLS(state, action string)
}

// Bootstrap provisions the scaler's certificates (Ensurer) at start and every
// Interval, serves the current serving certificate, and reports what an
// operator must do. It decides once, in Prepare, whether the scaler serves
// mutual TLS: when the certificates are usable and KEDA's side is in place
// (ConfigMap MarkerConfigMapName exists), as the installer decides; plaintext
// otherwise.
//
// After Prepare, a renewed serving certificate is served from the next
// handshake on, with no restart, as long as it chains to the CA the scaler
// verifies clients with, which is fixed for the life of the process. A change
// of CA is reported, never served.
type Bootstrap struct {
	Ensurer
	// Status receives the state; nil reports nothing.
	Status StatusSink
	// Interval is the period of the checks after Prepare; 0 is DefaultInterval.
	Interval time.Duration
	// Log is the logger; the zero value logs under ctrl.Log.
	Log logr.Logger

	serving atomic.Pointer[tls.Certificate]

	mu sync.Mutex
	// tlsOn: Prepare chose mutual TLS.
	tlsOn bool
	// clientCA is the CA the scaler verifies clients with, when tlsOn.
	clientCA *x509.Certificate
	// rewire: this process minted the CA or issued KEDA's client certificate,
	// so KEDA's copy must be replaced; caChanged says the CA itself is new.
	rewire, caChanged bool
	// issuedClient is the fingerprint of the client certificate KEDA must
	// present for rewire to be done.
	issuedClient string
	// kedaPresented is set once a handshake presented issuedClient.
	kedaPresented bool
	// last is the outcome of the latest pass, lastErr its error.
	last    *Outcome
	lastErr error
	// markerAtStart and markerNow: whether KEDA's side was in place at
	// Prepare, and at the latest pass.
	markerAtStart, markerNow bool
	markerErr                error
}

func (b *Bootstrap) logger() logr.Logger {
	if b.Log.GetSink() == nil {
		return ctrl.Log.WithName("scaler-tls")
	}
	return b.Log
}

// Prepare runs the first pass and decides whether the scaler serves mutual
// TLS. It returns the CA client certificates must chain to when it does, nil
// when the scaler serves plaintext. It retries a failed pass four times, with
// a growing pause, while ctx lasts.
func (b *Bootstrap) Prepare(ctx context.Context) *x509.Certificate {
	var o *Outcome
	var err error
	for attempt := 0; ; attempt++ {
		if o, err = b.Ensure(ctx); err == nil || ctx.Err() != nil || attempt >= 4 {
			break
		}
		b.logger().Info("Scaler TLS: a pass failed, retrying", "error", err.Error())
		select {
		case <-ctx.Done():
		case <-time.After(time.Duration(attempt+1) * 2 * time.Second):
		}
	}
	marker, merr := b.readMarker(ctx)

	b.mu.Lock()
	b.markerAtStart, b.markerNow, b.markerErr = marker, marker, merr
	b.record(o, err)
	if err == nil && o.Usable() && marker {
		b.tlsOn = true
		b.clientCA = o.CA
		b.serving.Store(o.Serving.tlsCertificate())
	}
	ca := b.clientCA
	b.mu.Unlock()

	b.publish()
	return ca
}

// Start checks the Secrets every Interval until ctx ends. It implements
// manager.Runnable.
func (b *Bootstrap) Start(ctx context.Context) error {
	interval := b.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			b.Check(ctx)
		}
	}
}

// NeedLeaderElection is false: every replica serves the scaler and needs the
// certificate. Writes race safely: a Create that finds the Secret, or an
// Update that conflicts, re-reads and adopts.
func (b *Bootstrap) NeedLeaderElection() bool { return false }

// Check runs one pass: it renews what the rules allow, serves a renewed
// serving certificate, and reports.
func (b *Bootstrap) Check(ctx context.Context) {
	o, err := b.Ensure(ctx)
	marker, merr := b.readMarker(ctx)
	b.mu.Lock()
	b.markerNow, b.markerErr = marker, merr
	b.record(o, err)
	if err == nil && b.tlsOn && o.Serving != nil && o.CA != nil && bytes.Equal(o.CA.Raw, b.clientCA.Raw) {
		if cur := b.serving.Load(); cur == nil || !bytes.Equal(cur.Leaf.Raw, o.Serving.cert.Raw) {
			b.serving.Store(o.Serving.tlsCertificate())
			b.logger().Info("Scaler TLS: serving a new certificate", "notAfter", o.Serving.cert.NotAfter)
		}
	}
	b.mu.Unlock()
	b.publish()
}

// record keeps the outcome of a pass and what it wrote. A failed pass keeps
// the previous outcome. Call with mu held.
func (b *Bootstrap) record(o *Outcome, err error) {
	b.lastErr = err
	if err != nil {
		b.logger().Error(err, "Scaler TLS: cannot provision the certificates")
		return
	}
	b.last = o
	if o.CAMinted {
		b.rewire, b.caChanged = true, true
		b.logger().Info("Scaler TLS: minted a new CA and its certificates",
			"secret", CASecretName, "caSha256", Fingerprint(o.CA))
	}
	if o.ServingIssued {
		b.logger().Info("Scaler TLS: issued the serving certificate", "secret", ServingSecretName,
			"notAfter", o.Serving.cert.NotAfter)
	}
	if o.ClientIssued {
		b.rewire = true
		b.logger().Info("Scaler TLS: issued KEDA's client certificate", "secret", ClientSecretName)
	}
	if o.ClientIssued || o.CAMinted {
		b.issuedClient, b.kedaPresented = Fingerprint(o.Client), false
	}
	for _, p := range o.Problems {
		b.logger().Info("Scaler TLS: " + p)
	}
}

// GetCertificate serves the current serving certificate; pass it as
// tls.Config.GetCertificate.
func (b *Bootstrap) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	if c := b.serving.Load(); c != nil {
		return c, nil
	}
	return nil, errors.New("scaler TLS: no serving certificate")
}

// ObserveHandshake records a handshake the scaler accepted; call it from
// tls.Config.VerifyConnection once the client is verified. KEDA presenting
// the client certificate this process issued ends the notice that KEDA must
// be wired again.
func (b *Bootstrap) ObserveHandshake(cs tls.ConnectionState) {
	if len(cs.PeerCertificates) == 0 {
		return
	}
	presented := Fingerprint(cs.PeerCertificates[0])
	b.mu.Lock()
	done := b.rewire && !b.kedaPresented && b.issuedClient != "" && b.issuedClient == presented
	if done {
		b.kedaPresented = true
	}
	b.mu.Unlock()
	if done {
		b.logger().Info("Scaler TLS: KEDA presents the client certificate issued here")
		b.publish()
	}
}

// readMarker reports whether ConfigMap MarkerConfigMapName exists and carries
// OwnerLabel. A read that fails for any reason but NotFound is an error, and
// counts as absent for the decision: the scaler stays on plaintext.
func (b *Bootstrap) readMarker(ctx context.Context) (bool, error) {
	cm := &corev1.ConfigMap{}
	err := b.Reader.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: MarkerConfigMapName}, cm)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading ConfigMap %s in %s: %w", MarkerConfigMapName, b.Namespace, err)
	}
	if _, ok := cm.Labels[OwnerLabel]; !ok {
		return false, fmt.Errorf("ConfigMap %s in %s has no %s label, so it is not KEDA's side as the release file or the installer writes it",
			MarkerConfigMapName, b.Namespace, OwnerLabel)
	}
	return true, nil
}

// publish sends the state and the action to Status.
func (b *Bootstrap) publish() {
	if b.Status == nil {
		return
	}
	b.mu.Lock()
	state, action := b.describe()
	b.mu.Unlock()
	b.Status.SetScalerTLS(state, action)
}

// describe returns the state and the action, the action's steps joined by
// " | ". Call with mu held.
func (b *Bootstrap) describe() (state, action string) {
	switch {
	case b.tlsOn:
		state = fmt.Sprintf("mutual TLS: Secrets %s, %s and %s; client identity %s; CA sha256 %s",
			CASecretName, ServingSecretName, ClientSecretName, b.ClientSAN, Fingerprint(b.clientCA))
		if c := b.serving.Load(); c != nil && c.Leaf != nil {
			state += "; serving certificate expires " + c.Leaf.NotAfter.UTC().Format(time.RFC3339)
		}
	case b.last == nil:
		state = "plaintext: the scaler TLS Secrets could not be provisioned"
	case !b.last.Usable():
		state = "plaintext: the scaler TLS certificates are not usable"
	case b.markerErr != nil:
		state = "plaintext: " + b.markerErr.Error()
	default:
		state = "plaintext: KEDA's side of mutual TLS was not in place at start (no ConfigMap " + MarkerConfigMapName + ")"
	}

	var steps []string
	if b.lastErr != nil {
		steps = append(steps, fmt.Sprintf("the latest check failed (%v): the controller needs get, create and update on "+
			"Secrets %s, %s and %s in %s (Role scaling-manager-scaler-tls-role in the release files)",
			b.lastErr, CASecretName, ServingSecretName, ClientSecretName, b.Namespace))
	}
	if b.last != nil {
		steps = append(steps, b.last.Problems...)
	}
	switch {
	case b.last == nil || !b.last.Usable():
	case !b.tlsOn && b.markerNow:
		steps = append(steps, "KEDA's side is in place now; restart the controller to serve mutual TLS: "+b.restartController())
	case !b.tlsOn:
		step := "wire KEDA to the scaler's certificates (a cluster admin), then restart the controller: " +
			b.wireCommand() + " && " + b.restartController()
		if b.caChanged {
			step += " ; this controller minted a new CA: if KEDA dialled this scaler before (a reinstall), restart KEDA's operator too: " + restartKEDA()
		}
		steps = append(steps, step)
	case !bytes.Equal(b.last.CA.Raw, b.clientCA.Raw):
		steps = append(steps, fmt.Sprintf("the scaler CA in Secret %s changed since the controller started (sha256 %s; "+
			"it verifies clients with %s): wire KEDA to it, then restart the controller and KEDA's operator: %s && %s && %s",
			CASecretName, Fingerprint(b.last.CA), Fingerprint(b.clientCA), b.wireCommand(), b.restartController(), restartKEDA()))
	case b.rewire && !b.kedaPresented && b.caChanged:
		steps = append(steps, fmt.Sprintf("a new scaler CA was minted; copy KEDA's client certificate (Secret %s) to KEDA again "+
			"and restart KEDA's operator, which keeps the CA of its first connection (a cluster admin): %s && %s",
			ClientSecretName, b.wireCommand(), restartKEDA()))
	case b.rewire && !b.kedaPresented:
		steps = append(steps, fmt.Sprintf("KEDA's client certificate (Secret %s) was issued again; copy it to KEDA "+
			"(a cluster admin); this notice clears when KEDA presents it, after its operator restarts: %s", ClientSecretName, b.wireCommand()))
	}
	return state, strings.Join(steps, " | ")
}

// wireCommand copies KEDA's client certificate into KEDA's namespace and
// applies the release file that points KEDA at it and writes the marker:
// the documented cluster-admin step. KEDA_NS is KEDA's namespace (keda by
// default; openshift-keda on OpenShift). Only the three data keys are copied,
// and with a server-side apply, so no last-applied annotation holds a second
// copy of the private key.
func (b *Bootstrap) wireCommand() string {
	ns := b.Namespace
	copyName := "scaling-manager-external-scaler-" + ns
	tmpl := `{"apiVersion":"v1","kind":"Secret","type":"Opaque","metadata":{"name":"` + copyName + `","labels":{"` +
		managedByLabel + `":"` + managedByValue + `","` + partOfLabel + `":"` + partOfValue + `","` + OwnerLabel + `":"keda-client"}},` +
		`"data":{"tls.crt":"{{index .data "tls.crt"}}","tls.key":"{{index .data "tls.key"}}","ca.crt":"{{index .data "ca.crt"}}"}}`
	return fmt.Sprintf(`KEDA_NS=${KEDA_NS:-keda}; kubectl get secret %s -n %s -o go-template='%s' | `+
		`kubectl apply --server-side --force-conflicts --field-manager=%s -n "$KEDA_NS" -f - && `+
		`sed -e "s/__NAMESPACE__/%s/g" -e "s/__KEDA_NAMESPACE__/$KEDA_NS/g" scaling-manager-keda-auth.yaml | kubectl apply -f -`,
		ClientSecretName, ns, tmpl, managedByValue, ns)
}

// restartController restarts the controller, which reads KEDA's side at start.
func (b *Bootstrap) restartController() string {
	return fmt.Sprintf("kubectl rollout restart deployment/%s -n %s", ControllerDeployment, b.Namespace)
}

// restartKEDA restarts KEDA's operator, which keeps the CA and client
// certificate of its first connection to a scaler address until it restarts.
func restartKEDA() string {
	return `kubectl rollout restart deployment/keda-operator -n "${KEDA_NS:-keda}"`
}
